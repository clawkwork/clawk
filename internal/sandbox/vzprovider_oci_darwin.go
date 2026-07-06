//go:build darwin

package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/guestbuild"
	"github.com/clawkwork/clawk/internal/vsockclient"
	"github.com/clawkwork/clawk/machine/kernel"
	"github.com/clawkwork/clawk/machine/oci"
)

// OCI-image sandboxes on the vz provider. This path builds an ext4
// rootfs straight from the configured OCI image (with clawk-init and the
// agents injected), boots the Kata kernel directly into it, and talks to
// the guest exclusively over the vsock agent. No qemu-img, no seed ISO,
// no sshd.

// createOCI prepares an OCI-image sandbox: guest binaries, kernel, and
// the rootfs disk are all built (or cache-hit) here so failures surface
// interactively at create time; the daemon's later Materialize is then a
// pure cache hit + clonefile.
func (v *VZProvider) createOCI(sb *config.Sandbox) error {
	ctx := context.Background()
	vmDir := v.store.VMDir(sb.Name)
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		return fmt.Errorf("creating VM dir: %w", err)
	}

	progress := v.progress
	if progress == nil {
		progress = PlainProgress{}
	}
	defer progress.Close()

	// Cache hits are silent (Skip): a checkmark for a stat is noise.
	progress.Step("Preparing guest binaries")
	bins, err := guestbuild.Build(ctx, v.store.CacheDir(), runtime.GOARCH)
	if err != nil {
		return fmt.Errorf("building guest binaries: %w", err)
	}
	if bins.Cached {
		progress.Skip()
	} else {
		progress.StepDone("Built guest binaries (clawk-init, pty-agent)")
	}

	kernelOpts := kernel.Options{CacheDir: v.store.CacheDir(), Arch: runtime.GOARCH, Override: sb.Kernel}
	kernelLabel := "kata " + kernel.DefaultKataVersion
	if sb.Kernel != "" {
		kernelLabel = "override " + sb.Kernel
	}
	if _, cached := kernel.CachedPath(kernelOpts); !cached {
		progress.Step("Fetching kernel")
		if _, err := kernel.Fetch(ctx, kernelOpts); err != nil {
			return fmt.Errorf("fetching kernel: %w", err)
		}
		progress.StepDone("Kernel ready (%s)", kernelLabel)
	} else if _, err := kernel.Fetch(ctx, kernelOpts); err != nil {
		return fmt.Errorf("fetching kernel: %w", err)
	}

	progress.Step("Preparing rootfs from %s", sb.Image)
	rootfs := OCIRootFS(sb, v.store.CacheDir(), bins)
	opts := oci.OptionsForImage(rootfs)
	opts.Progress = func(u oci.ProgressUpdate) {
		switch u.Phase {
		case oci.PhaseStart:
			// A real build is starting — set expectations: this is the
			// one-time cost, every later sandbox from this image is a
			// cache hit + clone.
			progress.Step("Building rootfs from %s (first build)", sb.Image)
		case oci.PhaseDownload:
			// docker-pull style: one bar per layer, downloading in
			// parallel. The flatten that follows is sequential.
			progress.SetBars(layerBars(u.Downloads))
			progress.Detail("downloading %d layers", len(u.Downloads))
		case oci.PhaseUnpack:
			// Downloads are done; drop the bars and narrate the
			// sequential flatten by layer position.
			progress.SetBars(nil)
			if u.Layers > 0 {
				progress.SetFraction(float64(u.Layer) / float64(u.Layers))
				progress.Detail("unpacking layer %d/%d · %d MiB",
					u.Layer, u.Layers, u.UnpackedBytes>>20)
			} else {
				progress.Detail("%d MiB unpacked", u.UnpackedBytes>>20)
			}
		}
	}
	res, err := oci.Build(ctx, opts)
	if err != nil {
		return fmt.Errorf("building rootfs from %s: %w", sb.Image, err)
	}
	if res.UnpackedBytes > 0 {
		progress.StepDone("Built rootfs from %s (%d MiB unpacked; reused by future sandboxes)",
			sb.Image, res.UnpackedBytes>>20)
	} else {
		progress.StepDone("Rootfs %s (cached)", sb.Image)
	}

	// Pre-seed the per-sandbox Claude state — the ~/.claude mount is
	// provider-agnostic.
	if err := SeedClaudeStateDir(v.store.StateDir(sb.Name), v.store.RootDir()); err != nil {
		return fmt.Errorf("seeding claude state dir: %w", err)
	}
	// Seed baseline auto-memory once (no-op if the sandbox already has memory,
	// e.g. a re-create whose state dir survived). Best-effort: never block boot.
	if err := SeedClaudeMemory(v.store.StateDir(sb.Name), sb.Memory); err != nil {
		fmt.Fprintf(os.Stderr, "warning: seeding memory: %v\n", err)
	}

	sb.GuestIP = guestIP
	// No summary line: the Created line already named the provider and
	// image, and the rootfs step said what was built. Start's "Ready in"
	// is the next thing the user sees.
	return nil
}

// layerBars maps an OCI download snapshot to one progress bar per layer.
func layerBars(downloads []oci.LayerStatus) []Bar {
	if len(downloads) == 0 {
		return nil
	}
	bars := make([]Bar, len(downloads))
	for i, d := range downloads {
		var frac float64
		switch {
		case d.CompressedSize > 0:
			frac = float64(d.Downloaded) / float64(d.CompressedSize)
		case d.Done:
			frac = 1
		}
		label := fmt.Sprintf("layer %d/%d", d.Index, len(downloads))
		if d.Cached {
			label += " (cached)"
		}
		bars[i] = Bar{Label: label, Frac: frac}
	}
	return bars
}

// waitAgentReady polls the vsock agent until a full round trip succeeds:
// agent reachability proves kernel boot, clawk-init, user creation and
// the proxy wiring in one probe.
func (v *VZProvider) waitAgentReady(sb *config.Sandbox, pidPath string) error {
	progress := v.progress
	if progress == nil {
		progress = PlainProgress{}
	}
	defer progress.Close()

	vmDir := v.store.VMDir(sb.Name)
	sock := filepath.Join(vmDir, "agent.sock")
	progress.Step("Booting VM")
	start := time.Now()
	deadline := start.Add(120 * time.Second)
	for time.Now().Before(deadline) {
		pid, err := readPIDFile(pidPath)
		if err != nil || !processAlive(pid) {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		sb.VMPid = pid
		if _, err := os.Stat(sock); err != nil {
			progress.Detail("waiting for the VM daemon")
			time.Sleep(500 * time.Millisecond)
			continue
		}
		progress.Detail("waiting for the guest agent")
		if err := vsockclient.Ping(context.Background(), sock, 0, 3*time.Second); err == nil {
			progress.StepDone("Ready in %.1fs", time.Since(start).Seconds())
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("agent did not become ready within timeout (daemon log: %s/vzd.log)%s",
		vmDir, ConsoleTail(filepath.Join(vmDir, "console.log"), 15))
}

// agentShell opens an interactive shell via the vsock agent. The agent
// resolves the user's login shell from /etc/passwd, so this works on
// images without bash.
func (v *VZProvider) agentShell(sb *config.Sandbox, workdir string) error {
	code, err := vsockclient.Run(context.Background(), vsockclient.Config{
		SocketPath: filepath.Join(v.store.VMDir(sb.Name), "agent.sock"),
		Cwd:        workdir,
		User:       GuestUser,
		// A login shell is line-oriented: no screen clear, keep scrollback.
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return &ExitError{Code: code}
	}
	return nil
}

// agentExec runs a one-shot command via the vsock agent as GuestUser.
// Interactive stdio (the caller sees the command's output); a non-zero
// exit surfaces as an error, the way a remote exec contract does.
func (v *VZProvider) agentExec(sb *config.Sandbox, command ...string) error {
	return v.agentExecAs(sb, GuestUser, command...)
}

// agentExecAs is agentExec with an explicit guest user; the empty user
// runs as the agent itself (root).
func (v *VZProvider) agentExecAs(sb *config.Sandbox, user string, command ...string) error {
	if len(command) == 0 {
		return fmt.Errorf("agentExec: empty command")
	}
	code, err := vsockclient.Run(context.Background(), vsockclient.Config{
		SocketPath: filepath.Join(v.store.VMDir(sb.Name), "agent.sock"),
		Cmd:        command[0],
		Args:       command[1:],
		User:       user,
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("command exited with status %d", code)
	}
	return nil
}

// ExecCapture runs a command in the guest and returns its combined
// output over the agent's non-interactive frame protocol. Hooks can
// legitimately run for minutes (pnpm install), hence the generous
// deadline.
func (v *VZProvider) ExecCapture(sb *config.Sandbox, command ...string) (string, error) {
	if len(command) == 0 {
		return "", fmt.Errorf("ExecCapture: empty command")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	sock := filepath.Join(v.store.VMDir(sb.Name), "agent.sock")
	out, code, err := vsockclient.Output(ctx, sock, 0, GuestUser, command[0], command[1:]...)
	if err != nil {
		return out, err
	}
	if code != 0 {
		return out, fmt.Errorf("command exited with status %d", code)
	}
	return out, nil
}

// ExecRoot runs a command as root in the guest over the agent, which runs
// as root — no sudo needed, and arbitrary images often ship none.
func (v *VZProvider) ExecRoot(sb *config.Sandbox, command ...string) error {
	return v.agentExecAs(sb, "", command...)
}
