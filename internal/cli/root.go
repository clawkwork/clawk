package cli

import (
	"fmt"
	"os"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/spf13/cobra"
)

var (
	store *config.Store

	// providerFlag is set via --provider on commands that create/start VMs.
	// For commands operating on an existing sandbox, we use the sandbox's
	// persisted Provider field instead.
	providerFlag string
	imageFlag    string
	kernelFlag   string
)

var rootCmd = &cobra.Command{
	Use:   "clawk",
	Short: "Local development sandbox manager",
	Long: `clawk runs coding agents inside disposable Linux microVMs, one
per piece of work. Two modes — pick the one that fits the work, not
the tool.

  clawk                         cwd mode. Bind-mounts $CWD into a VM keyed
                                on the directory and attaches the default
                                runner (claude). No git, no template, no
                                ticket. The VM persists until you destroy
                                it; conversation memory persists across
                                destroys.

  clawk work <ticket>           ticket mode. Reads a clawk.mod template
                                (multi-repo when its sandbox block has
                                'includes') from $CWD or a parent,
                                snapshots its configuration into
                                a new sandbox, materialises one git worktree
                                per repo on a fresh branch, and attaches
                                the default runner. The template is read
                                once at create time and never re-read; the
                                sandbox is self-contained from there.

  clawk attach <name>           resume mode. The universal way back into an
                                existing sandbox from any directory: loads
                                the record, boots the VM if needed, and
                                attaches the default runner. Never creates,
                                never re-reads a template.

Use 'clawk run <runner> <name>' to attach a non-default runner to an
existing sandbox; 'clawk work <ticket> --bare' creates the sandbox
without booting or attaching, and pairs with 'clawk run' for non-default
runners.

Run 'clawk --help' for the full verb tree (status, lifecycle, network
policy, port forwards, diagnostics).`,
	// MaximumNArgs(0) forces "no positional args" — protects users who
	// type e.g. `clawk INFRA-123` expecting it to "do the right
	// thing" by erroring with a clear message instead of dispatching
	// to an unrelated subcommand by accident.
	Args: cobra.NoArgs,
	RunE: runZeroArgs,
	// Same rationale as the runner verbs in runners.go: when the
	// bare `clawk` invocation fails (sandbox not running, vsock
	// disconnect, etc.), don't follow the error with the full
	// command tree. The user already knows the verbs.
	SilenceUsage: true,
	// main() is the single error reporter (and the single os.Exit site,
	// so it can translate a sandbox.ExitError into the guest's own exit
	// code). Without this cobra would also print the error, double-printing
	// it and emitting a spurious line for a non-zero interactive session.
	SilenceErrors: true,
}

// runZeroArgs implements the bare `clawk` invocation: create-or-
// resume the sandbox keyed on $CWD, boot it, and launch the default
// agent. The cwd-vm naming/loading helpers live in here.go and are
// shared with the top-level commands' cwd-inference.
func runZeroArgs(cmd *cobra.Command, _ []string) error {
	cwd, name, err := hereCWDAndName()
	if err != nil {
		return err
	}
	sb, created, err := loadOrCreateHereSandbox(name, cwd)
	if err != nil {
		return err
	}
	if created {
		fmt.Printf("Created sandbox %q (cwd-vm; mounts %s)\n", sb.DisplayName(), cwd)
	} else {
		fmt.Printf("Using sandbox %q\n", sb.DisplayName())
	}
	emitCwdShadowHint(cmd.ErrOrStderr(), cwd)
	if err := runUpInline(sb); err != nil {
		if created {
			// First boot of a brand-new cwd-sandbox failed; roll the record
			// back so a retry re-reads clawk.mod instead of reusing the config
			// that just failed (a bad kernel/image ref, say). Without this the
			// snapshot-at-create rule pins the broken record and `clawk
			// destroy` is the only non-obvious recovery — see
			// loadOrCreateHereSandbox.
			rollbackFailedCreate(sb)
		}
		return err
	}
	if err := attachDefaultAgent(sb); err != nil {
		return err
	}
	printDetachHint(cmd.ErrOrStderr(), sb)
	return nil
}

func Execute() error {
	return rootCmd.Execute()
}

func init() {
	var err error
	store, err = config.NewStore()
	if err != nil {
		fmt.Fprintln(os.Stderr, "failed to init config store:", err)
		os.Exit(1)
	}

	// --safe is local to the bare `clawk` invocation (not persistent): it only
	// governs the runner attach that runZeroArgs performs. The other attach
	// entry points register their own copy.
	registerSafeFlag(rootCmd)
	rootCmd.PersistentFlags().StringVar(&providerFlag, "provider", "",
		"VM provider for new sandboxes: vz (default; macOS, Apple Virtualization.framework + userspace networking via gvproxy) or firecracker (Linux)")
	rootCmd.PersistentFlags().StringVar(&imageFlag, "image", "",
		"OCI image for new sandboxes (registry ref or docker-save tarball path; overrides clawk.mod)")
	rootCmd.PersistentFlags().StringVar(&kernelFlag, "kernel", "",
		"guest kernel override for new sandboxes (local vmlinux path or http(s) URL; overrides clawk.mod). Default: the Kata kernel")
}

// testProvider, if non-nil, is returned from providerFor — used by tests to
// inject a MockProvider without going through provider selection.
var testProvider sandbox.Provider

// providerFor returns the VM provider for the given sandbox. It honors
// the sandbox's persisted choice, falling back to the --provider flag
// (used on create) or the default (vz) for legacy sandboxes without
// the field set. Construction is delegated to per-platform
// resolveProvider so non-host providers fail at validation with a clear
// "this is a $OTHER_OS build" message rather than a runtime stub error.
func providerFor(sb *config.Sandbox) (sandbox.Provider, error) {
	if testProvider != nil {
		return testProvider, nil
	}
	p := sb.Provider
	if p == "" {
		p = config.Provider(providerFlag)
	}
	if p == "" {
		p = defaultProvider()
	}
	provider, err := newProvider(p.Normalize())
	if err != nil {
		return nil, err
	}
	// Interactive terminals get spinner-narrated progress for the
	// long-running Create work (image pulls, rootfs builds). The
	// tracker starts lazily; non-TTY runs keep plain line output.
	if ps, ok := provider.(interface{ SetProgress(sandbox.Progress) }); ok {
		if t := newProgressTracker(); t != nil {
			ps.SetProgress(t)
		}
	}
	return provider, nil
}

// providerForName is a convenience for commands that take a sandbox name.
func providerForName(name string) (sandbox.Provider, *config.Sandbox, error) {
	sb, err := store.Load(name)
	if err != nil {
		return nil, nil, err
	}
	p, err := providerFor(sb)
	return p, sb, err
}
