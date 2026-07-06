package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/clawkwork/clawk/internal/vsockclient"
	"github.com/clawkwork/clawk/machine/oci"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// `clawk doctor` walks through the per-sandbox or host-level state
// that's most often broken when a user reports "claude isn't working,"
// emits a one-line ✓/⚠/✗ per check, and prints a short fix hint on
// every failure. It's the command someone runs when something's
// weird, before they go reading vzd.log.
//
// Two scopes:
//
//	clawk doctor             host-level checks (no VM needed)
//	clawk doctor <name>      per-sandbox checks
//	clawk doctor             (with cwd-sandbox if no <name> supplied)
//
// Output is human by default; --json emits a stable schema for the
// messaging-bot integration.

var doctorJSON bool

func init() {
	rootCmd.AddCommand(doctorCmd)
	doctorCmd.Flags().BoolVar(&doctorJSON, "json", false,
		"emit JSON (stable schema for scripts)")
}

var doctorCmd = &cobra.Command{
	ValidArgsFunction: completeSandboxNames,
	Use:               "doctor [<name>]",
	Short:             "Health check (cwd-sandbox by default; no args for host-level)",
	Long: `doctor walks through the per-sandbox state most often broken when
something's weird:

  - VM process alive
  - vsock dial succeeds (the path 'clawk claude' uses)
  - clawk-pty-agent socket present
  - console.log has no recent kernel panic markers
  - proxy not flagged as wedged

With no argument, defaults to the cwd-derived sandbox (same rule as
the bare 'clawk' invocation). To probe host prerequisites instead,
use 'clawk system info' — doctor focuses on what's broken, info
just reports.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// resolveSandboxName falls back to the cwd-derived sandbox
		// when no arg given. doctor on a host with no sandboxes
		// (fresh install) just runs the host-level checks.
		var name string
		if len(args) > 0 {
			name = args[0]
		} else {
			n, err := resolveSandboxName(nil)
			if err == nil {
				name = n
			}
			// no error path — we'll just run host-level checks.
		}

		results := runDoctorChecks(name)
		if doctorJSON {
			return emitDoctorJSON(cmd, name, results)
		}
		return printDoctorHuman(cmd, name, results)
	},
}

// doctorCheck is one line of output. Status ∈ {ok, warn, fail}; on
// non-ok results, FixHint suggests a concrete next step. Stable shape
// for both human and JSON renderers.
type doctorCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // statusOK | statusWarn | statusFail
	Detail  string `json:"detail"`
	FixHint string `json:"fix_hint,omitempty"`
}

const (
	statusOK   = "ok"
	statusWarn = "warn"
	statusFail = "fail"
)

func ok(name, detail string) doctorCheck {
	return doctorCheck{Name: name, Status: statusOK, Detail: detail}
}
func warn(name, detail, fix string) doctorCheck {
	return doctorCheck{Name: name, Status: statusWarn, Detail: detail, FixHint: fix}
}
func fail(name, detail, fix string) doctorCheck {
	return doctorCheck{Name: name, Status: statusFail, Detail: detail, FixHint: fix}
}

// runDoctorChecks dispatches the right set of checks based on whether
// a sandbox name resolved. Empty name → host-only.
func runDoctorChecks(name string) []doctorCheck {
	var results []doctorCheck

	// Host-level checks always run. They're cheap and useful even
	// when a per-sandbox check follows.
	results = append(results, hostChecks()...)

	if name == "" {
		return results
	}

	// Sandbox-specific checks. Look up the on-disk record first;
	// if the sandbox is gone we surface the obvious "no such
	// sandbox" without trying to probe its non-existent VM.
	sb, err := store.Load(name)
	if err != nil {
		results = append(results, fail("sandbox: exists",
			fmt.Sprintf("%q: %v", name, err),
			"check `clawk list`; create one with `clawk work <ticket>` or just `clawk` for cwd-vm"))
		return results
	}
	results = append(results, sandboxChecks(sb)...)
	return results
}

// hostChecks: state dir + per-OS host tools. Same probes the
// first-run setup prober uses, framed as diagnostic output.
func hostChecks() []doctorCheck {
	var results []doctorCheck

	results = append(results, doctorCheck{
		Name:   "host: os/arch",
		Status: statusOK,
		Detail: fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
	})

	if _, err := exec.LookPath("go"); err != nil {
		results = append(results, fail("host: go toolchain",
			"not found on PATH (builds the guest init/agent for image-based sandboxes)",
			"install from https://go.dev/dl or brew install go"))
	} else {
		results = append(results, ok("host: go toolchain", "on PATH"))
	}

	root := clawkRoot()
	if _, err := os.Stat(root); err != nil {
		results = append(results, warn("host: state dir",
			"~/.clawk/ missing",
			"will be created automatically on first VM start"))
	} else {
		results = append(results, ok("host: state dir", root))
	}

	// Per-OS hypervisor prerequisites (firecracker + /dev/kvm on Linux;
	// none on macOS, where Virtualization.framework is linked in).
	results = append(results, platformHostChecks()...)

	return results
}

// sandboxChecks: the per-VM probes. Each probe is short-timed (≤ 2 s)
// so doctor stays snappy even on a totally wedged sandbox.
func sandboxChecks(sb *config.Sandbox) []doctorCheck {
	var results []doctorCheck
	vmDir := store.VMDir(sb.Name)

	// 1. VM process alive
	pid, err := readVMPID(vmDir)
	if err != nil {
		results = append(results, fail("vm: daemon",
			"pidfile missing — sandbox not running",
			"clawk up "+sb.Name))
		return results // nothing else worth probing if the daemon is dead
	}
	if processAliveDoctor(pid) {
		results = append(results, ok("vm: daemon",
			fmt.Sprintf("daemon pid %d alive", pid)))
	} else {
		results = append(results, fail("vm: daemon",
			fmt.Sprintf("pid %d in pidfile is dead", pid),
			"clawk up "+sb.Name))
	}

	// 2. Image source still resolvable (image-based sandboxes). Works
	// even before the boot checks — a deleted tarball or a moved tag is
	// diagnosed without a running VM.
	if sb.Image != "" {
		if tarPath := oci.LocalTarballPath(sb.Image); tarPath != "" {
			if _, err := os.Stat(tarPath); err == nil {
				results = append(results, ok("image: source", tarPath))
			} else {
				results = append(results, fail("image: source",
					fmt.Sprintf("tarball missing: %s", tarPath),
					"docker save <image> -o "+tarPath+" (or point clawk.mod at a registry ref)"))
			}
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			digest, err := oci.ResolveDigest(ctx, sb.Image, "linux/"+runtime.GOARCH)
			cancel()
			if err == nil {
				results = append(results, ok("image: source",
					fmt.Sprintf("%s resolves to %.19s…", sb.Image, digest)))
			} else {
				results = append(results, warn("image: source",
					fmt.Sprintf("cannot resolve %s: %v", sb.Image, err),
					"offline or the tag moved; the cached rootfs keeps working, rebuilds need the registry"))
			}
		}
	}

	// 3. agent.sock present and dialable. This whole probe is vz-specific:
	// the __vzd daemon runs an in-process agent proxy that bridges a
	// host-side agent.sock to the guest's vsock, and doctor dials it. The
	// firecracker daemon has no such proxy — the CLI reaches the guest over
	// firecracker's hybrid-vsock UDS with a CONNECT handshake — so there is
	// no agent.sock to stat, and asserting one is "missing" is a false
	// negative. On firecracker the daemon-alive check above plus the console
	// scan below are the host-side signals; a live-guest probe would need
	// the provider's exec channel, which has no short doctor deadline.
	if sb.Provider == config.ProviderVZ {
		sockPath := filepath.Join(vmDir, "agent.sock")
		if _, err := os.Stat(sockPath); err == nil {
			results = append(results, ok("vm: agent.sock", sockPath))

			// 4. End-to-end vsock probe: dial agent.sock and write a
			//    handshake. Don't actually run a command — we just want
			//    to confirm the proxy + guest agent path is alive.
			if err := vsockProbe(sockPath); err == nil {
				results = append(results, ok("vm: vsock dial",
					"proxy ↔ guest agent answered"))

				// Image-based sandboxes: confirm the default runner exists
				// in the image — the single most common "it boots but
				// claude doesn't attach" cause.
				if sb.Image != "" {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					out, code, err := vsockclient.Output(ctx, sockPath, 0,
						sandbox.GuestUser, "/bin/sh", "-c", "command -v claude")
					cancel()
					switch {
					case err != nil:
						results = append(results, warn("guest: claude",
							fmt.Sprintf("probe failed: %v", err),
							"re-run in a moment; if persistent, clawk down && clawk up"))
					case code == 0:
						results = append(results, ok("guest: claude",
							strings.TrimSpace(out)))
					default:
						results = append(results, warn("guest: claude",
							"not found on the image's PATH",
							"bare `clawk` attaches claude; use an image that ships it (the default template does) or install it via `on create`"))
					}
				}
			} else if sandboxPaused(sb) {
				// A paused VM produces the identical probe failure (the proxy
				// is alive, the frozen guest never answers) — and the advice
				// below would destroy a deliberately-paused sandbox and all
				// its resident state. Check pause first, always.
				results = append(results, warn("vm: vsock dial",
					"guest is paused (vCPUs frozen by 'clawk pause')",
					fmt.Sprintf("continue it with: clawk resume%s", sandboxRef(sb))))
			} else {
				// This is the wedge symptom: timeouts / EOF here
				// mean the guest kernel is frozen.
				results = append(results, fail("vm: vsock dial",
					err.Error(),
					"VM is likely wedged (Apple Silicon vz kernel-mm bug); recover with: clawk destroy && clawk"))
			}
		} else if os.IsNotExist(err) {
			results = append(results, warn("vm: agent.sock",
				"missing (vsock proxy disabled or VM still starting)",
				"wait for boot to finish; if persistent, clawk down && clawk up"))
		}
	}

	// 5. Console-log scan for recent kernel panic markers
	consolePath := filepath.Join(vmDir, "console.log")
	if hits, err := scanConsolePanics(consolePath, 200); err == nil && len(hits) > 0 {
		results = append(results, warn("vm: console.log",
			fmt.Sprintf("recent kernel oops/panic markers: %d found", len(hits)),
			"a kernel-side mm bug fired recently; if the VM is still responsive it may be living on borrowed time. clawk destroy && clawk to start fresh"))
	} else {
		results = append(results, ok("vm: console.log", "no recent panic markers"))
	}

	// 6. Branches' worktrees are clean (no .git/index.lock files
	//    suggesting a crashed git operation).
	for _, p := range sb.Phases {
		if p.Worktree == "" || p.InPlace {
			continue
		}
		lockPath := filepath.Join(p.Worktree, ".git", "index.lock")
		if _, err := os.Stat(lockPath); err == nil {
			results = append(results, warn("vm: worktree",
				p.Repo+" has .git/index.lock (crashed git op?)",
				"rm "+lockPath))
		}
	}

	return results
}

// vsockProbe dials the agent.sock and sends a minimal handshake. If
// the proxy + guest agent are healthy, we get a frame back within
// ~500 ms. On wedge, the dial succeeds (proxy is alive) but the
// proxy's vsock dial to the guest times out — surfaces here as
// "agent disconnected before exit frame: EOF" or similar.
func vsockProbe(sockPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Exercise the same framed protocol `clawk claude` uses — dial,
	// handshake, exec, read the exit frame — but via Output, NOT the
	// interactive Run: Run puts the local terminal into raw mode and emits
	// terminal-reset escapes on exit, which is wrong for a background
	// health check (it would disturb the user's shell every time doctor
	// runs). Output is frame-only and never touches the tty. `/bin/true`
	// opens and closes the session in one round-trip. Output honors the
	// context deadline internally, so a wedged guest surfaces as a read
	// error rather than a hang.
	_, _, err := vsockclient.Output(ctx, sockPath, 0, "agent", "/bin/true")
	return err
}

// scanConsolePanics greps the last `tailLines` of console.log for
// kernel panic / oops markers. Returns the matched strings (one per
// hit). Empty slice + nil error = healthy.
func scanConsolePanics(path string, tailLines int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// Cheap last-N-lines: read all, split. console.log on a long-
	// running VM may be many MiB but it's bounded by virt-serial
	// buffer; reasonable to load in full.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	from := 0
	if len(lines) > tailLines {
		from = len(lines) - tailLines
	}
	var hits []string
	for _, l := range lines[from:] {
		ll := strings.ToLower(l)
		if strings.Contains(ll, "kernel panic") ||
			strings.Contains(ll, "internal error: oops") ||
			strings.Contains(ll, "bad rss-counter") ||
			strings.Contains(ll, "fpac:") ||
			strings.Contains(ll, "attempted to kill init") {
			hits = append(hits, strings.TrimSpace(l))
		}
	}
	return hits, nil
}

// vmPIDFileNames are the per-provider VM-daemon pidfiles. Only one exists
// on a given host — vz.pid on macOS (__vzd), fc.pid on Linux (__fcd) — so
// helpers that answer "is the VM daemon alive?" check both to stay
// provider-neutral. Checking the absent one is a harmless miss.
var vmPIDFileNames = []string{"vz.pid", "fc.pid"}

// readVMPID reads whichever VM-daemon pidfile is present in vmDir. It's a
// cross-platform sibling of the provider-specific readers (vz's
// readPIDFile in vzprovider_darwin.go, fc's in linux_shared.go); doctor
// and admission-control run on both OSes and can't assume the provider.
func readVMPID(vmDir string) (int, error) {
	var firstErr error
	for _, name := range vmPIDFileNames {
		data, err := os.ReadFile(filepath.Join(vmDir, name))
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		return strconv.Atoi(strings.TrimSpace(string(data)))
	}
	return 0, firstErr
}

// processAliveDoctor: signal-0 probe to confirm the daemon process is
// running. Named with "Doctor" suffix to avoid colliding with the
// processAlive helper that already exists in vzprovider_darwin.go.
func processAliveDoctor(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// printDoctorHuman renders one line per check with a colour-coded
// prefix. Falls back to plain text when stderr isn't a tty (CI runs).
func printDoctorHuman(cmd *cobra.Command, sandboxName string, results []doctorCheck) error {
	out := cmd.OutOrStdout()
	colour := term.IsTerminal(int(os.Stderr.Fd()))

	for _, r := range results {
		fmt.Fprintf(out, "%s %s — %s\n",
			statusGlyph(r.Status, colour), r.Name, r.Detail)
		if r.FixHint != "" && r.Status != statusOK {
			fmt.Fprintf(out, "    fix: %s\n", r.FixHint)
		}
	}

	// Final summary line so users skimming output see the verdict
	// without having to count glyphs.
	var oks, warns, fails int
	for _, r := range results {
		switch r.Status {
		case statusOK:
			oks++
		case statusWarn:
			warns++
		case statusFail:
			fails++
		}
	}
	target := "host"
	if sandboxName != "" {
		target = "sandbox " + sandboxName
	}
	fmt.Fprintf(out, "\n%s: %d ok, %d warn, %d fail\n", target, oks, warns, fails)
	if fails > 0 {
		// Surface a non-zero exit so scripts and the messaging bot
		// can detect "something's actually broken."
		os.Exit(2)
	}
	return nil
}

func statusGlyph(s string, colour bool) string {
	if !colour {
		switch s {
		case statusOK:
			return "[OK]  "
		case statusWarn:
			return "[WARN]"
		case statusFail:
			return "[FAIL]"
		}
		return "[?]   "
	}
	// ANSI colour for tty output. Green ✓, yellow ⚠, red ✗.
	switch s {
	case statusOK:
		return "\x1b[32m✓\x1b[0m"
	case statusWarn:
		return "\x1b[33m⚠\x1b[0m"
	case statusFail:
		return "\x1b[31m✗\x1b[0m"
	}
	return "?"
}

// emitDoctorJSON renders the doctor output as a versioned schema for
// the messaging-bot integration and any other scriptable consumer.
func emitDoctorJSON(cmd *cobra.Command, sandboxName string, results []doctorCheck) error {
	type out struct {
		Schema  string        `json:"schema"`
		Sandbox string        `json:"sandbox,omitempty"`
		Checks  []doctorCheck `json:"checks"`
		Summary struct {
			OK   int `json:"ok"`
			Warn int `json:"warn"`
			Fail int `json:"fail"`
		} `json:"summary"`
	}
	o := out{Schema: "1", Sandbox: sandboxName, Checks: results}
	for _, r := range results {
		switch r.Status {
		case statusOK:
			o.Summary.OK++
		case statusWarn:
			o.Summary.Warn++
		case statusFail:
			o.Summary.Fail++
		}
	}
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	if err := enc.Encode(o); err != nil {
		return err
	}
	if o.Summary.Fail > 0 {
		os.Exit(2)
	}
	return nil
}
