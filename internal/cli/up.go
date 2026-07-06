package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(upCmd)
}

var upCmd = &cobra.Command{
	ValidArgsFunction: completeSandboxNames,
	Use:               "up [<name>]",
	Short:             "Start a sandbox VM (defaults to the cwd-derived sandbox)",
	Args:              cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name, err := resolveSandboxName(args)
		if err != nil {
			return err
		}
		provider, sb, err := providerForName(name)
		if err != nil {
			return err
		}

		// Refuse before touching anything when the guest binaries baked
		// into this sandbox's disk are ones this clawk no longer speaks —
		// the alternative is a version error from inside the guest,
		// mid-boot, with no recreate hint.
		if err := sandbox.CheckGuestABI(sb); err != nil {
			return err
		}

		// Record the desired lifecycle state up front — `up` is, in declarative
		// terms, "set DesiredState=running, then converge." Today we converge
		// inline below; a future server-side reconciler would converge from this.
		// Any boot also retires a recorded idle-stop: the reason describes the
		// last stop, and after this verb that stop is history.
		if sb.DesiredState != config.VMStateRunning || sb.StopReason != "" {
			sb.DesiredState = config.VMStateRunning
			sb.StopReason = ""
			if err := store.Save(sb); err != nil {
				return err
			}
		}

		if len(sb.Phases) == 0 {
			return fmt.Errorf("sandbox %q has no worktrees — use 'clawk worktree add' first", sb.DisplayName())
		}

		if status, _ := provider.Status(sb); isRunning(status) {
			// "Running" covers paused too (the daemon is alive either way);
			// `up` means "make it usable", so unfreeze rather than shrug.
			if resumed, err := resumeIfPaused(cmd.OutOrStdout(), sb); err != nil {
				return err
			} else if !resumed {
				fmt.Printf("Sandbox %q is already running\n", sb.DisplayName())
			}
			return nil
		}

		switch sb.Provider {
		case config.ProviderVZ, "":
			return bringUpVZ(provider, sb)
		case config.ProviderFirecracker:
			return bringUpFirecracker(provider, sb)
		default:
			return fmt.Errorf("unknown provider %q", sb.Provider)
		}
	},
}

// bringUpFirecracker is the minimal lifecycle: Create prep, Start,
// save. The Linux provider deliberately skips host-side filtering and
// per-phase setup — those are macOS-first features today. If it ever
// grows `on up` hooks, mirror bringUpVZ's sandboxRestored guard: a boot
// that restored a suspend snapshot must not re-run them (the guest's
// processes are still alive inside).
func bringUpFirecracker(provider sandbox.Provider, sb *config.Sandbox) error {
	if err := provider.Create(sb); err != nil {
		return fmt.Errorf("preparing VM: %w", err)
	}
	if err := provider.Start(sb); err != nil {
		return fmt.Errorf("starting VM: %w", err)
	}
	sb.VMState = config.VMStateRunning
	if err := store.Save(sb); err != nil {
		return err
	}
	fmt.Printf("Sandbox %q is up (firecracker, guest %s)\n", sb.DisplayName(), sb.GuestIP)
	return nil
}

func bringUpVZ(provider sandbox.Provider, sb *config.Sandbox) error {
	// Make ~/.claude a git working tree on this sandbox's branch and fold in
	// the project's accumulated history BEFORE the provider mounts the dir,
	// so the guest comes up with prior transcripts and memory in its resume
	// picker. Best-effort — never blocks bring-up.
	prepareSessionHistory(sb)
	// Refuse to start if this box's worst-case memory would oversubscribe the
	// host — the condition that panics macOS. Fails safe: a probe error skips
	// the check rather than blocking.
	if err := admitMemoryForStart(sb); err != nil {
		return err
	}
	if err := provider.Create(sb); err != nil {
		return fmt.Errorf("preparing VM: %w", err)
	}
	if err := provider.Start(sb); err != nil {
		return fmt.Errorf("starting VM: %w", err)
	}
	sb.VMState = config.VMStateRunning
	if err := store.Save(sb); err != nil {
		return err
	}
	// No "is up" line: Start already narrated "Ready in N s"; a second
	// completion line is noise.
	// Phases or host capability shares added after first boot aren't in
	// the manifest the guest mounted at boot, so mount anything the guest
	// doesn't have up yet. vz already exposes every current tag via
	// its --device args — we just need the guest to mount them.
	if err := ensureRuntimeMounts(provider, sb); err != nil {
		fmt.Fprintf(os.Stderr, "warning: mounting runtime shares: %v\n", err)
	}
	// Push `files (...)` snapshots before `on create` so any setup
	// command that needs the kube config / aws creds finds them in
	// place. ensureRuntimeMounts ran first, so a share that gates the
	// guest mount point already exists.
	if err := pushHostFiles(provider, sb); err != nil {
		fmt.Fprintf(os.Stderr, "warning: pushing host files: %v\n", err)
	}
	if err := runPhaseOnCreate(provider, sb); err != nil {
		// sb.CreatePending has already been persisted by runPhaseOnCreate.
		// Surface the failure but leave the VM running so the user can
		// shell in and investigate.
		return err
	}
	// A boot that restored a suspend snapshot continues the guest
	// mid-thought: its `on up` processes (dev servers, watchers) are
	// still running inside — re-running the hooks would double them up.
	if sandboxRestored(sb) {
		fmt.Printf("Sandbox %q restored from its snapshot — skipping 'on up' hooks (guest state was preserved)\n",
			sb.DisplayName())
		return nil
	}
	if err := runPhaseOnUp(provider, sb); err != nil {
		fmt.Fprintf(os.Stderr, "warning: 'on up' commands failed: %v\n", err)
	}
	return nil
}

// ensureRuntimeMounts idempotently mounts every current phase's worktree,
// source repo, and host capability share inside the running VM. Safe to call
// on every `up`: uses `mountpoint -q` to skip anything already mounted.
//
// This is load-bearing for `phase add` or `run` on an already-running
// sandbox: clawk-init mounts the manifest's share list once at boot.
// Anything introduced later has its virtio-fs tag exposed by vz (we
// rebuild the device list per boot) but isn't yet mounted in the guest,
// so it wouldn't appear otherwise.
func ensureRuntimeMounts(provider sandbox.Provider, sb *config.Sandbox) error {
	sp, ok := provider.(sandbox.ShellProvider)
	if !ok {
		return nil
	}
	wsRoot := sandbox.GuestWorkspace(provider)
	// Consolidated worktree parent: one virtio-fs device mounted at wsRoot
	// (see collectSandboxShares). Managed worktrees added after first boot
	// appear as subdirs of this already-mounted parent — no per-worktree
	// mount needed — so we only ensure the parent itself is present. Done
	// before the in-place sub-mounts below so it never shadows them.
	if sandbox.HasManagedWorktree(sb) {
		if err := mountIfMissing(sp, sb, sandbox.WorkspaceShareTag, wsRoot); err != nil {
			return fmt.Errorf("%s: %w", wsRoot, err)
		}
	}
	seenRepos := make(map[string]bool)
	for _, p := range sb.Phases {
		if p.Worktree == "" {
			continue
		}
		// InPlace phases point at the user's own repo, which lives outside
		// the worktree parent, so each keeps its own device sub-mounted at
		// wsRoot/<name>. Repo == Worktree there, so no src_* alias is
		// exposed (see collectSandboxShares) — skip it so we don't mount a
		// non-existent virtiofs tag.
		if p.InPlace {
			tag := filepath.Base(p.Worktree)
			mp := wsRoot + "/" + tag
			if err := mountIfMissing(sp, sb, tag, mp); err != nil {
				return fmt.Errorf("%s: %w", mp, err)
			}
			continue
		}
		if !seenRepos[p.Repo] {
			seenRepos[p.Repo] = true
			srcTag := "src_" + filepath.Base(p.Repo)
			if err := mountIfMissing(sp, sb, srcTag, p.Repo); err != nil {
				return fmt.Errorf("%s: %w", p.Repo, err)
			}
		}
	}
	for _, sh := range sandbox.DefaultHostShares() {
		if err := mountIfMissing(sp, sb, sh.Tag, sh.GuestPath); err != nil {
			return fmt.Errorf("%s: %w", sh.GuestPath, err)
		}
	}
	// User-declared shares (clawk.mod `shares (...)`). The provider's
	// device list already exposes the tag; this loop is what makes the
	// guest mount the share when it wasn't in the original fstab — e.g.
	// after `clawk up` on a sandbox whose clawk.mod gained a new entry.
	for _, sh := range sandbox.UserHostShares(sb.Shares) {
		if err := mountIfMissing(sp, sb, sh.Tag, sh.GuestPath); err != nil {
			return fmt.Errorf("%s: %w", sh.GuestPath, err)
		}
	}
	return nil
}

// mountIfMissing runs `mkdir -p + mount -t virtiofs` inside the guest,
// skipping the mount if one is already in place.
func mountIfMissing(sp sandbox.ShellProvider, sb *config.Sandbox, tag, mountpoint string) error {
	// Providers with a root exec channel (OCI sandboxes — their agent
	// runs as root) take a sudo-less POSIX-sh script: arbitrary images
	// often ship neither sudo nor bash. The fallback uses a sudo + bash
	// shape for providers without a root exec channel.
	if re, ok := sp.(sandbox.RootExecProvider); ok {
		script := fmt.Sprintf(
			"mkdir -p %s && ( mountpoint -q %s || mount -t virtiofs %s %s )",
			shellQuote(mountpoint), shellQuote(mountpoint),
			shellQuote(tag), shellQuote(mountpoint))
		return re.ExecRoot(sb, "/bin/sh", "-c", script)
	}
	script := fmt.Sprintf(
		"sudo mkdir -p %s && ( mountpoint -q %s || sudo mount -t virtiofs %s %s )",
		shellQuote(mountpoint), shellQuote(mountpoint),
		shellQuote(tag), shellQuote(mountpoint))
	return sp.Exec(sb, "bash", "-c", script)
}

// shellQuote wraps s in single quotes and escapes embedded single
// quotes for safe interpolation into a bash -c script.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// runPhaseOnCreate runs each phase's `on create` block inside the VM, once
// per phase. Already-completed phases (Phase.OnCreateAt non-zero) are
// skipped so a second `clawk up` after success doesn't re-run them.
//
// Failure is hard: the first failing command marks the sandbox
// create-pending and returns. The VM is left running so the user can shell
// in and investigate. The next `clawk up` re-runs `on create` from
// scratch for the still-incomplete phases — the failed phase's
// OnCreateAt was never written, so the retry is automatic.
func runPhaseOnCreate(provider sandbox.Provider, sb *config.Sandbox) error {
	var anyPending bool
	for i := range sb.Phases {
		if len(sb.Phases[i].OnCreate) > 0 && sb.Phases[i].OnCreateAt.IsZero() {
			anyPending = true
			break
		}
	}
	if !anyPending {
		// Nothing to run. If the sandbox was previously create-pending and
		// the user hand-fixed things, clear the flag so a fresh attach
		// works. (Preserves CreatePending in genuine zero-on-create
		// sandboxes only when it was set; impossible if no on-create
		// commands exist, so no risk of stale state.)
		if sb.CreatePending {
			sb.CreatePending = false
			sb.CreatePendingReason = ""
			if err := store.Save(sb); err != nil {
				return err
			}
		}
		return nil
	}
	progress := hookProgress()
	defer progress.Close()
	wsRoot := sandbox.GuestWorkspace(provider)
	for i := range sb.Phases {
		p := &sb.Phases[i]
		if len(p.OnCreate) == 0 || !p.OnCreateAt.IsZero() {
			continue
		}
		wtBase := filepath.Base(p.Worktree)
		phaseDir := wsRoot + "/" + wtBase
		phaseName := filepath.Base(p.Repo)
		for n, cmd := range p.OnCreate {
			label := fmt.Sprintf("on create %s %d/%d", phaseName, n+1, len(p.OnCreate))
			if err := runHookCommand(provider, sb, progress, label, phaseDir, cmd); err != nil {
				sb.CreatePending = true
				sb.CreatePendingReason = fmt.Sprintf(
					"phase %s 'on create' command %q failed: %v",
					phaseName, cmd, err)
				if saveErr := store.Save(sb); saveErr != nil {
					return fmt.Errorf("%s (also failed to persist create-pending: %w)",
						sb.CreatePendingReason, saveErr)
				}
				fmt.Fprintf(os.Stderr,
					"sandbox %q is now create-pending; "+
						"investigate with 'clawk debug vshell%s', "+
						"retry with 'clawk up%s', or reset with 'clawk destroy%s'\n",
					sb.DisplayName(), sandboxRef(sb), sandboxRef(sb), sandboxRef(sb))
				return fmt.Errorf("%s", sb.CreatePendingReason)
			}
		}
		p.OnCreateAt = time.Now()
		if err := store.Save(sb); err != nil {
			return fmt.Errorf("persisting on-create completion for %s: %w", phaseName, err)
		}
	}
	if sb.CreatePending {
		sb.CreatePending = false
		sb.CreatePendingReason = ""
		if err := store.Save(sb); err != nil {
			return err
		}
	}
	return nil
}

// runPhaseOnUp runs each phase's `on up` (Phase.Setup) commands inside
// the VM at the worktree root. Failures are warned-and-skipped so a
// flaky service script doesn't block the rest of the bring-up — the user
// can shell in and re-run manually.
func runPhaseOnUp(provider sandbox.Provider, sb *config.Sandbox) error {
	var hasOnUp bool
	for _, p := range sb.Phases {
		if len(p.Setup) > 0 {
			hasOnUp = true
			break
		}
	}
	if !hasOnUp {
		return nil
	}
	progress := hookProgress()
	defer progress.Close()
	wsRoot := sandbox.GuestWorkspace(provider)
	for _, p := range sb.Phases {
		if len(p.Setup) == 0 {
			continue
		}
		wtBase := filepath.Base(p.Worktree)
		phaseDir := wsRoot + "/" + wtBase
		phaseName := filepath.Base(p.Repo)
		for n, cmd := range p.Setup {
			label := fmt.Sprintf("on up %s %d/%d", phaseName, n+1, len(p.Setup))
			if err := runHookCommand(provider, sb, progress, label, phaseDir, cmd); err != nil {
				return fmt.Errorf("phase %s command %q: %w", phaseName, cmd, err)
			}
		}
	}
	return nil
}

// runHookCommand executes one `on create` / `on up` command, narrated
// as a progress step. Capture-capable providers keep output off the
// screen and surface it only on failure; others stream with a framing
// line (mock provider, future providers).
func runHookCommand(provider sandbox.Provider, sb *config.Sandbox, progress sandbox.Progress, label, phaseDir, cmd string) error {
	full := fmt.Sprintf("cd %s && %s", shellQuote(phaseDir), cmd)
	ce, canCapture := provider.(sandbox.CaptureExecProvider)
	if !canCapture {
		sp, ok := provider.(sandbox.ShellProvider)
		if !ok {
			return errors.New("provider does not support running hook commands")
		}
		fmt.Printf("→ %s: %s\n", label, cmd)
		return sp.Exec(sb, "bash", "-lc", full)
	}
	progress.Step("%s: %s", label, cmd)
	start := time.Now()
	out, err := ce.ExecCapture(sb, "bash", "-lc", full)
	if err != nil {
		progress.Close()
		if trimmed := strings.TrimSpace(out); trimmed != "" {
			fmt.Fprintln(os.Stderr, trimmed)
		}
		return err
	}
	progress.StepDone("%s: %s (%.1fs)", label, cmd, time.Since(start).Seconds())
	return nil
}

// hookProgress builds the narration sink for hook runs: spinner on a
// TTY, plain lines otherwise.
func hookProgress() sandbox.Progress {
	if t := newProgressTracker(); t != nil {
		return t
	}
	return sandbox.PlainProgress{}
}

func isRunning(status string) bool {
	return strings.EqualFold(status, "running")
}
