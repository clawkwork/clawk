package cli

// The pause / resume / snapshot verbs — VM lifecycle beyond up/down.
//
// Three levels of "stopped", cheapest to deepest:
//
//   pause     vCPUs freeze in place; memory stays resident. Instant both
//             ways. The guest's processes, sessions, and network stack are
//             exactly where they were.
//   snapshot  suspend-to-disk: memory + device state are saved next to the
//             VM and the VM stops WITHOUT running again past the save
//             point (so the rootfs stays consistent with the saved
//             memory). Frees all host memory. `clawk resume` — or any
//             boot — restores the guest mid-thought.
//   down      plain stop; the guest shuts down and the next up is a cold
//             boot.
//
// pause/resume against a live daemon go through the control socket
// (vzdctl); resume of a snapshotted sandbox is just a boot — the daemon
// finds the suspend state and restores instead of cold-booting.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/clawkwork/clawk/internal/vzdctl"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(pauseCmd)
	rootCmd.AddCommand(resumeCmd)
	rootCmd.AddCommand(snapshotCmd)
}

// lifecycleVerbTimeout bounds the control-socket round trip for pause and
// resume. Both are sub-second in the framework; the margin covers a daemon
// mid-GC or a host under load.
const lifecycleVerbTimeout = 30 * time.Second

// suspendTimeout bounds `clawk snapshot`'s save. The daemon writes the
// guest's entire memory image (up to the sandbox's memory ceiling), so this
// scales with the largest configured guests rather than the common case.
const suspendTimeout = 10 * time.Minute

var pauseCmd = &cobra.Command{
	ValidArgsFunction: completeSandboxNames,
	Use:               "pause [<name>]",
	Short:             "Pause a running sandbox VM in place (memory stays resident)",
	Long: `pause freezes the sandbox's vCPUs where they are. Everything inside —
processes, sessions, network state — stays exactly as it was, held in host
memory. Instant both ways: 'clawk resume' continues the guest mid-thought,
as do 'clawk up' and the attach verbs.

To also free the sandbox's host memory, use 'clawk snapshot' (suspend to
disk) instead.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name, err := resolveSandboxName(args)
		if err != nil {
			return err
		}
		provider, sb, err := providerForName(name)
		if err != nil {
			return err
		}
		if status, _ := provider.Status(sb); !isRunning(status) {
			return fmt.Errorf("sandbox %q is not running — pause freezes a live VM", sb.DisplayName())
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), lifecycleVerbTimeout)
		defer cancel()
		if err := sandboxCtl(sb).Pause(ctx); err != nil {
			return lifecycleVerbError("pause", sb, err)
		}
		fmt.Fprintf(cmd.OutOrStdout(),
			"Sandbox %q paused — 'clawk resume%s' continues it; 'clawk snapshot%s' would free its memory too\n",
			sb.DisplayName(), sandboxRef(sb), sandboxRef(sb))
		return nil
	},
}

var resumeCmd = &cobra.Command{
	ValidArgsFunction: completeSandboxNames,
	Use:               "resume [<name>]",
	Short:             "Resume a paused or snapshotted sandbox VM",
	Long: `resume continues a sandbox where it left off:

  - paused ('clawk pause'): the vCPUs restart in place, instantly;
  - snapshotted ('clawk snapshot'): the VM boots from its saved memory
    image, restoring every process and session as they were;
  - plain stopped: nothing to continue — resume boots it fresh, same as
    'clawk up'.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name, err := resolveSandboxName(args)
		if err != nil {
			return err
		}
		provider, sb, err := providerForName(name)
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		if status, _ := provider.Status(sb); isRunning(status) {
			// The explicit verb must not shrug when the probe fails: an
			// unanswerable daemon and "genuinely running" are different
			// answers, and conflating them buries a paused-but-unqueriable
			// VM behind a reassuring message. (The attach paths' lenient
			// resumeIfPaused deliberately makes the opposite call.)
			ls, err := probeLifecycle(sb, lifecycleVerbTimeout)
			switch {
			case err != nil:
				return lifecycleVerbError("resume", sb, err)
			case ls.State == vzdctl.LifecyclePaused:
				fmt.Fprintf(out, "Sandbox %q is paused — resuming\n", sb.DisplayName())
				ctx, cancel := context.WithTimeout(cmd.Context(), lifecycleVerbTimeout)
				defer cancel()
				if err := sandboxCtl(sb).Resume(ctx); err != nil {
					return lifecycleVerbError("resume", sb, err)
				}
			default:
				fmt.Fprintf(out, "Sandbox %q is already running\n", sb.DisplayName())
			}
			return nil
		}
		if len(sb.Phases) == 0 {
			return fmt.Errorf("sandbox %q has no worktrees — use 'clawk worktree add' first", sb.DisplayName())
		}
		if hasSuspendState(suspendStateDir(store.VMDir(sb.Name))) {
			fmt.Fprintf(out, "Restoring sandbox %q from its disk snapshot\n", sb.DisplayName())
		} else {
			fmt.Fprintf(out, "Sandbox %q has no saved state — booting fresh\n", sb.DisplayName())
		}
		return bootSandbox(provider, sb)
	},
}

var snapshotCmd = &cobra.Command{
	ValidArgsFunction: completeSandboxNames,
	Use:               "snapshot [<name>]",
	Aliases:           []string{"suspend"},
	Short:             "Snapshot a running sandbox to disk and stop it (resume continues it)",
	Long: `snapshot suspends the sandbox to disk: the VM's memory and device state
are saved next to its disk image and the VM stops — without the guest
running past the save point, so the disk stays exactly consistent with the
saved memory. All host memory is freed.

'clawk resume' (or 'clawk up', or any attach verb) boots the sandbox back
precisely where it left off: running builds, shell history, sessions — all
mid-thought. Attached sessions are disconnected by the stop, and the guest's
open TCP connections won't survive the gap, same as a laptop waking from
hibernation.

If the saved state can't be restored later (for example the sandbox's
shares or memory changed in between), the boot falls back to a clean cold
boot and discards the stale state.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name, err := resolveSandboxName(args)
		if err != nil {
			return err
		}
		provider, sb, err := providerForName(name)
		if err != nil {
			return err
		}
		if status, _ := provider.Status(sb); !isRunning(status) {
			return fmt.Errorf("sandbox %q is not running — snapshot suspends a live VM", sb.DisplayName())
		}

		var progress sandbox.Progress = sandbox.PlainProgress{}
		if t := newProgressTracker(); t != nil {
			progress = t
		}
		progress.Step("Snapshotting sandbox %q to disk", sb.DisplayName())
		start := time.Now()

		// The daemon owns the record transition (stopped + suspended,
		// written before the save, reverted by the daemon if the save
		// fails) — the layer that performs the stop records it, and a CLI
		// killed mid-call can't leave the record lying.
		ctx, cancel := context.WithTimeout(cmd.Context(), suspendTimeout)
		defer cancel()
		if err := sandboxCtl(sb).Suspend(ctx); err != nil {
			progress.Close()
			return lifecycleVerbError("snapshot", sb, err)
		}
		// The daemon exits on its own once the machine reports stopped;
		// wait for it so a follow-up boot never races the dying process
		// for the VM's sockets.
		if !waitDaemonExit(provider, sb, 30*time.Second) {
			progress.Close()
			return fmt.Errorf("sandbox %q saved its state but the daemon is still running after 30s — check 'clawk doctor %s'",
				sb.DisplayName(), sb.Name)
		}
		progress.StepDone("Sandbox %q snapshotted%s (%.1fs) — 'clawk resume%s' continues it where it left off",
			sb.DisplayName(), suspendSizeNote(sb), time.Since(start).Seconds(), sandboxRef(sb))
		progress.Close()
		return nil
	},
}

// sandboxCtl returns a control-socket client for the sandbox's daemon.
func sandboxCtl(sb *config.Sandbox) *vzdctl.Client {
	return vzdctl.NewClient(vzdctl.SocketPath(store.VMDir(sb.Name)))
}

// lifecycleVerbError translates the control socket's two capability
// failures into actionable messages; anything else passes through wrapped.
func lifecycleVerbError(verb string, sb *config.Sandbox, err error) error {
	switch {
	case errors.Is(err, vzdctl.ErrNotRunning):
		return fmt.Errorf("%s: the sandbox's daemon isn't answering (mid-boot or mid-shutdown?): %w", verb, err)
	case errors.Is(err, vzdctl.ErrLifecycleUnsupported):
		return fmt.Errorf("%s: this sandbox's daemon predates lifecycle control — restart it first ('clawk down%s && clawk up%s')",
			verb, sandboxRef(sb), sandboxRef(sb))
	default:
		return fmt.Errorf("%s: %w", verb, err)
	}
}

// lifecycleProbeTimeout bounds best-effort lifecycle probes on display and
// attach paths. A healthy daemon answers a unix-socket GET in microseconds;
// a wedged one must cost the verb at most this, never a hang.
const lifecycleProbeTimeout = 2 * time.Second

// probeLifecycle fetches the daemon's lifecycle snapshot with the given
// timeout. The single place every paused/restored check goes through.
func probeLifecycle(sb *config.Sandbox, timeout time.Duration) (vzdctl.LifecycleState, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return sandboxCtl(sb).Lifecycle(ctx)
}

// sandboxPaused is the best-effort "is it paused right now" predicate:
// probe failures (an old daemon, a mid-boot socket) read as not-paused and
// the caller's next step surfaces any real problem.
func sandboxPaused(sb *config.Sandbox) bool {
	ls, err := probeLifecycle(sb, lifecycleProbeTimeout)
	return err == nil && ls.State == vzdctl.LifecyclePaused
}

// resumeIfPaused resumes a live-but-paused VM and reports whether it did.
// The attach verbs call it right before dialing the agent: a paused guest
// accepts vsock connections but never answers them, which reads as a hang —
// resuming first is what the user meant anyway. Best-effort on the probe
// side (see sandboxPaused); a failed resume of a genuinely paused VM is an
// error the caller must surface.
func resumeIfPaused(w io.Writer, sb *config.Sandbox) (bool, error) {
	if !sandboxPaused(sb) {
		return false, nil
	}
	fmt.Fprintf(w, "Sandbox %q is paused — resuming\n", sb.DisplayName())
	ctx, cancel := context.WithTimeout(context.Background(), lifecycleVerbTimeout)
	defer cancel()
	if err := sandboxCtl(sb).Resume(ctx); err != nil {
		return false, lifecycleVerbError("resume", sb, err)
	}
	return true, nil
}

// sandboxRestored reports whether the sandbox's current daemon boot
// restored the guest from a suspend snapshot. Best-effort: probe failures
// read as a cold boot, which only means boot hooks run when they could
// have been skipped. Safe to trust right after a boot verb returns: the
// daemon attaches its lifecycle state before it creates the agent socket
// the boot's readiness gate waits on.
func sandboxRestored(sb *config.Sandbox) bool {
	ls, err := probeLifecycle(sb, lifecycleProbeTimeout)
	return err == nil && ls.Restored
}

// livePausedState overlays the control socket's pause knowledge onto the
// provider's process-level status: the daemon of a paused VM is alive, so
// the provider alone can only say "running". Display-only.
func livePausedState(sb *config.Sandbox, liveStatus string) string {
	if isRunning(liveStatus) && sandboxPaused(sb) {
		return vzdctl.LifecyclePaused
	}
	return liveStatus
}

// waitDaemonExit polls the provider until the daemon is gone, so a caller
// can boot the sandbox again without racing the dying process for the VM's
// sockets. Reports false when the daemon outlives the timeout.
func waitDaemonExit(provider sandbox.Provider, sb *config.Sandbox, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if status, _ := provider.Status(sb); !isRunning(status) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// suspendSizeNote renders " (N.NGiB state)" for the snapshot completion
// line, or "" when the size can't be read — the size is a nicety, never a
// failure.
func suspendSizeNote(sb *config.Sandbox) string {
	total := dirDiskUsage(suspendStateDir(store.VMDir(sb.Name)))
	if total == 0 {
		return ""
	}
	return fmt.Sprintf(" (%s state)", humanSize(total))
}
