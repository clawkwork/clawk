//go:build darwin

package cli

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/debug"
	"github.com/clawkwork/clawk/internal/guestbuild"
	"github.com/clawkwork/clawk/internal/netfilter"
	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/clawkwork/clawk/internal/vzdctl"
	"github.com/clawkwork/clawk/machine"
	"github.com/clawkwork/clawk/machine/kernel"

	// Side-effect import: registers "vz" with the machine registry.
	_ "github.com/clawkwork/clawk/machine/vz"

	"github.com/spf13/cobra"
)

func init() { rootCmd.AddCommand(vzdCmd) }

// vzdCmd runs one sandbox's VM lifecycle out-of-process. Internal; end
// users never call this — `clawk up` spawns it detached so the VM
// outlives the CLI invocation.
//
// The daemon itself owns: logfile, its own pidfile (for `up` liveness),
// the guest manifest disk, the AllowList's periodic DNS refresh, and
// signal handling. Everything else — gvproxy, the vz VM, the unixgram
// network plumbing — is delegated to the machine library
// (github.com/clawkwork/clawk/machine/vz).
var vzdCmd = &cobra.Command{
	Use:    "__vzd <sandbox>",
	Short:  "internal: run the sandbox VM via the machine library",
	Hidden: true,
	Args:   cobra.ExactArgs(1),
	RunE:   runVzd,
}

func runVzd(_ *cobra.Command, args []string) (retErr error) {
	sb, err := store.Load(args[0])
	if err != nil {
		return fmt.Errorf("loading sandbox: %w", err)
	}
	vmDir := store.VMDir(sb.Name)
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		return fmt.Errorf("preparing vm dir: %w", err)
	}

	logger, closeLog, err := openDaemonLog(vmDir)
	if err != nil {
		return err
	}
	defer closeLog()
	defer func() {
		if retErr != nil {
			logger.Printf("FATAL: %v", retErr)
		}
		if r := recover(); r != nil {
			// Dump every goroutine's stack on panic — a freeze that
			// manifests as a panic is worthless without the full
			// goroutine state at death.
			logger.Printf("PANIC: %v", r)
			buf := make([]byte, 1<<20)
			n := runtime.Stack(buf, true)
			logger.Printf("GOROUTINE DUMP:\n%s", buf[:n])
			panic(r)
		}
	}()

	logger.Printf("vzd starting pid=%d sandbox=%q debug=%v",
		os.Getpid(), sb.Name, debug.Enabled())

	pidPath := filepath.Join(vmDir, "vz.pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		return fmt.Errorf("writing pidfile: %w", err)
	}
	defer os.Remove(pidPath)

	allow, err := startAllowList(sb, logger)
	if err != nil {
		return err
	}
	defer allow.Stop()

	// Lifecycle surface: hands the machine to the control socket's
	// pause/resume/suspend endpoints once it exists. Built before the
	// socket so the endpoints answer "booting" instead of racing.
	lc := newVMLifecycle(sb.Name, vmDir, logger)

	// Control socket: lets the CLI push network-policy edits into the live
	// allow list (`clawk network allow` without a down/up cycle), read
	// the denial ledger (`clawk network denials`), and drive the VM
	// lifecycle (`clawk pause/resume/snapshot`). Best-effort — without
	// it, policy edits apply on the next up, as before.
	ctl, err := vzdctl.Start(vzdctl.SocketPath(vmDir), controlHandlers(sb, allow, lc, logger))
	if err != nil {
		logger.Printf("control socket: disabled (%v) — network edits apply on next up", err)
	} else {
		defer ctl.Close()
	}

	// Direct-kernel boot of the image-built rootfs with injected init;
	// config rides the manifest disk.
	spec, err := buildOCISandboxSpec(sb, vmDir, allow)
	if err != nil {
		return fmt.Errorf("building oci spec: %w", err)
	}
	// A restore boot must reuse the suspended boot's disk instead of the
	// spec's fresh-from-image rootfs — see suspendBootRootFS. "disk.raw"
	// is the vz backend's materialize target in this vmDir.
	if rootfs, ok := suspendBootRootFS(vmDir, "disk.raw", logger); ok {
		logger.Printf("suspend state present — booting the existing disk instead of re-materializing the rootfs")
		spec.RootFS = rootfs
	}
	logger.Printf("spec: vcpu=%d mem=%dMiB max=%dMiB shares=%d forwards=%d nested=%v",
		spec.VCPU, spec.MemoryMiB, spec.MemoryMaxMiB, len(spec.Shares), countForwards(spec),
		spec.NestedVirt)
	if debug.Enabled() {
		for _, sh := range spec.Shares {
			logger.Printf("share: tag=%s ro=%v host=%s", sh.Tag, sh.ReadOnly, sh.HostPath)
		}
	}

	backend, err := machine.Get("vz")
	if err != nil {
		return fmt.Errorf("getting vz backend: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m, err := backend.New(ctx, spec, vmDir)
	if err != nil {
		return fmt.Errorf("constructing machine: %w", err)
	}
	if err := m.Create(ctx); err != nil {
		return fmt.Errorf("preparing machine: %w", err)
	}
	// Attach a state-transition logger BEFORE Start so we capture the
	// initial Starting → Running edge. Implemented as a duck-typed
	// interface assertion to avoid pulling the vz package into cli/ —
	// every backend that supports state observation declares an
	// AttachStateLogger method matching this shape.
	if attacher, ok := m.(interface {
		AttachStateLogger(func(string, ...any))
	}); ok {
		attacher.AttachStateLogger(logger.Printf)
		logger.Printf("state-logger: attached")
	}

	// Boot — restoring from a `clawk snapshot` suspend state when one
	// exists (see restoreOrStart for the one-shot-consume invariant).
	// lc.attach must land before startAgentProxy below: the CLI's boot
	// paths gate on agent readiness and then probe /v1/lifecycle for the
	// restored bit, so agent.sock existing implies attach already ran.
	suspendMeta := machine.SuspendMeta{
		Backend:         "vz",
		SpecFingerprint: machine.SuspendSpecFingerprint(spec),
		ClawkVersion:    buildVersion(),
	}
	lc.setSuspendMeta(suspendMeta)
	restored, err := restoreOrStart(ctx, m, vmDir, logger, suspendMeta)
	if err != nil {
		return err
	}
	lc.attach(m, restored)

	// Agent proxy: bridges <vmDir>/agent.sock ↔ guest AF_VSOCK port 1024.
	// This is the sole control path into the guest (the sandbox runs no
	// sshd), so a failure here means `clawk claude`/`clawk shell` can't
	// attach — we log it loudly and let the readiness probe time out.
	proxy, err := startAgentProxy(ctx, m, agentSockPath(vmDir), logger)
	if err != nil {
		logger.Printf("agent-proxy: FAILED (%v) — clawk claude/shell cannot attach", err)
	} else {
		defer proxy.Stop()
	}

	// SSH-agent proxy: lets in-VM `git push`, `ssh-add -l`, etc. reach
	// the host's $SSH_AUTH_SOCK (1Password / launchd ssh-agent) over a
	// dedicated vsock port instead of relying on per-session SSH -A
	// forwarding. Best-effort and silent when the host has no agent.
	sshAgent, err := startSSHAgentProxy(ctx, m, logger)
	if err != nil {
		logger.Printf("ssh-agent-proxy: disabled (%v)", err)
	} else if sshAgent != nil {
		defer sshAgent.Stop()
	}

	// Wallclock-tick watchdog: detects host sleep events that
	// mac-sleep-notifier missed (macOS standby is the common offender)
	// and bounces the VM to recover guest clocks and network state —
	// but only when no client session is currently attached, so a
	// post-sleep bounce doesn't kill the user's live claude session, and
	// never while the user has the VM deliberately paused, so the bounce's
	// resume half doesn't silently unfreeze it.
	// See wallclock_watchdog_darwin.go for the rationale.
	startWallclockWatchdog(ctx, m, proxy, lc.isUserPaused, logger)

	// Idle watchdog: parks the VM (graceful stop; DesiredState untouched)
	// once the sandbox has had no attached session and a quiescent guest
	// for its idle timeout, so a forgotten sandbox stops pinning its
	// memory baseline. Any attach/run verb boots it back.
	// See idle_watchdog_darwin.go and idle.go for the rationale.
	startIdleWatchdog(ctx, sb, m, proxy, logger)

	// Time-sync sender: pushes the host wallclock to the guest every
	// 10 s via vsock port 1025. Pairs with clawk-time-sync inside the
	// guest. Cures the post-Mac-sleep clock drift that breaks TLS,
	// JWT, and any other timestamp-validated service.
	// See timesync_sender_darwin.go for the rationale.
	startTimeSyncSender(ctx, m, logger)

	return waitAndShutdown(ctx, m, logger)
}

// openDaemonLog opens vzd.log in append mode and returns a logger plus a
// close function. Also redirects os.Stderr to the same file so that any
// internal/debug output (and anything any dependency writes straight to
// fd 2) gets captured alongside the daemon's own log lines — otherwise
// debug breadcrumbs disappear since the CLI spawns vzd with Stderr=nil
// (see VZProvider.Start).
func openDaemonLog(vmDir string) (*log.Logger, func() error, error) {
	f, err := os.OpenFile(
		filepath.Join(vmDir, "vzd.log"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("opening daemon log: %w", err)
	}
	// Dup the log fd onto stderr so debug.Log (stderr-bound) writes
	// land in the same file. Any prior stderr is closed by dup2.
	// Failures are non-fatal — worst case, debug lines vanish, but
	// the primary logger still works.
	if err := syscall.Dup2(int(f.Fd()), int(os.Stderr.Fd())); err != nil {
		fmt.Fprintf(f, "warning: could not redirect stderr: %v\n", err)
	}
	return log.New(f, "", log.LstdFlags), f.Close, nil
}

// buildSandboxSpec translates a sandbox config into the shared parts of
// a machine.Spec: resources, networking, host shares, and the serial
// log. The boot mode and rootfs are set by the caller
// (buildOCISandboxSpec).
func buildSandboxSpec(sb *config.Sandbox, vmDir string, allow *netfilter.AllowList) machine.Spec {
	forwards := make([]machine.PortForward, 0, len(sb.Forwards))
	for _, f := range sb.Forwards {
		forwards = append(forwards, machine.PortForward{
			HostPort: uint16(f.HostPort), GuestPort: uint16(f.GuestPort),
			Proto: machine.ProtoTCP,
		})
	}
	vcpu, memMiB, memMaxMiB := specResources(sb)
	return machine.Spec{
		ID:           sb.Name,
		VCPU:         vcpu,
		MemoryMiB:    memMiB,
		MemoryMaxMiB: memMaxMiB,
		NestedVirt:   sb.NestedVirt,
		Net: []machine.Net{
			machine.UserMode{Forwards: forwards, Filter: allow},
		},
		Shares: collectSandboxShares(sb),
		Serial: machine.Serial{LogPath: filepath.Join(vmDir, "console.log")},
	}
}

// buildOCISandboxSpec completes buildSandboxSpec for image-rooted
// sandboxes: the boot is the Kata kernel straight into the injected
// clawk-init, the rootfs comes from the OCI pipeline, and a second disk
// carries the guest manifest. The manifest is rewritten on every boot,
// so share/file changes propagate with a plain restart.
//
// Everything here is a cache hit when the provider's Create ran: guest
// binaries, kernel, and the digest-keyed rootfs were all built there.
func buildOCISandboxSpec(sb *config.Sandbox, vmDir string, allow *netfilter.AllowList) (machine.Spec, error) {
	ctx := context.Background()
	bins, err := guestbuild.Build(ctx, store.CacheDir(), runtime.GOARCH)
	if err != nil {
		return machine.Spec{}, fmt.Errorf("guest binaries: %w", err)
	}
	vmlinux, err := kernel.Fetch(ctx, kernel.Options{
		CacheDir: store.CacheDir(),
		Arch:     runtime.GOARCH,
		Override: sb.Kernel,
	})
	if err != nil {
		return machine.Spec{}, fmt.Errorf("kernel: %w", err)
	}
	if err := sandbox.WriteOCIGuestConfig(
		sb, vmDir, store.StateDir(sb.Name), store.CacheDir(), store.RootDir()); err != nil {
		return machine.Spec{}, fmt.Errorf("guest config disk: %w", err)
	}

	spec := buildSandboxSpec(sb, vmDir, allow)
	spec.Boot = machine.DirectKernel{
		Vmlinux: vmlinux,
		Cmdline: sandbox.OCICmdline,
	}
	spec.RootFS = sandbox.OCIRootFS(sb, store.CacheDir(), bins)
	spec.Disks = []machine.Disk{
		{Path: filepath.Join(vmDir, sandbox.OCIConfigDiskName), ReadOnly: true},
	}
	return spec, nil
}
