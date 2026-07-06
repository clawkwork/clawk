//go:build darwin

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/clawkwork/clawk/internal/config"
)

// VZProvider boots a VM via Apple's Virtualization.framework — driven
// in-process through machine/vz, with no external VMM binary — its single
// NIC connected to an in-process gvproxy (gvisor-tap-vsock) instance. The
// gvproxy process holds the entire TCP/IP stack in userspace, giving us a
// tamper-proof filter point for every outbound connection without any
// host-side root configuration.
type VZProvider struct {
	store *config.Store

	// progress narrates long-running Create work. Nil means
	// PlainProgress; the CLI installs a spinner on interactive
	// terminals via SetProgress.
	progress Progress
}

func NewVZProvider(store *config.Store) *VZProvider {
	return &VZProvider{store: store}
}

// SetProgress installs a progress narrator for subsequent Create calls.
func (v *VZProvider) SetProgress(p Progress) { v.progress = p }

// Create prepares the VM: it builds an ext4 rootfs from the configured
// OCI image with the guest init injected — see vzprovider_oci_darwin.go.
//
// The hypervisor is Apple's Virtualization.framework, accessed in-process
// via machine/vz — no external VMM binary is required on PATH.
func (v *VZProvider) Create(sb *config.Sandbox) error {
	if sb.Image == "" {
		return fmt.Errorf("sandbox %q has no OCI image configured", sb.Name)
	}
	return v.createOCI(sb)
}

// Start launches the VM. It spawns a child process running the daemon (which
// owns gvproxy and the vz VM) and returns once the in-guest agent answers
// over vsock.
func (v *VZProvider) Start(sb *config.Sandbox) error {
	vmDir := v.store.VMDir(sb.Name)
	pidPath := filepath.Join(vmDir, "vz.pid")

	if pid, err := readPIDFile(pidPath); err == nil && processAlive(pid) {
		sb.VMPid = pid
		return nil // already running
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating clawk binary: %w", err)
	}

	// Spawn __vzd <sandbox> — runs gvproxy + vz in the child, detached.
	// Setsid creates a new session (and new process group) so the daemon has
	// no controlling terminal. Without this, Ctrl+C on the parent `clawk
	// run` sends SIGINT to the whole foreground process group and kills the
	// daemon; SIGTTOU can also suspend it if it writes to the tty while
	// backgrounded. Setpgid alone isn't enough — it changes the pgid but
	// leaves the controlling tty attached.
	cmd := exec.Command(self, "__vzd", sb.Name)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawning vzd: %w", err)
	}
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("releasing vzd process: %w", err)
	}

	// Readiness is one agent round trip: the sandbox runs no sshd, so
	// reaching the vsock agent proves kernel boot, clawk-init, user
	// creation and the proxy wiring all in one probe.
	return v.waitAgentReady(sb, pidPath)
}

// Stop signals the daemon to stop. The daemon tears down gvproxy and the vz VM.
func (v *VZProvider) Stop(sb *config.Sandbox) error {
	vmDir := v.store.VMDir(sb.Name)
	pid, err := readPIDFile(filepath.Join(vmDir, "vz.pid"))
	if err != nil {
		return nil // nothing to stop
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return fmt.Errorf("sending SIGTERM to vzd: %w", err)
	}
	// Wait up to 10s for a clean shutdown.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	// Force kill if still alive.
	if err := proc.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("force killing vzd: %w", err)
	}
	return nil
}

// Destroy tears down a running VM and removes its state directory.
func (v *VZProvider) Destroy(sb *config.Sandbox) error {
	if err := v.Stop(sb); err != nil {
		return err
	}
	return os.RemoveAll(v.store.VMDir(sb.Name))
}

// Status returns "running" if the vzd daemon process is alive.
func (v *VZProvider) Status(sb *config.Sandbox) (string, error) {
	pidPath := filepath.Join(v.store.VMDir(sb.Name), "vz.pid")
	pid, err := readPIDFile(pidPath)
	if err != nil {
		return "stopped", nil
	}
	if processAlive(pid) {
		return "running", nil
	}
	return "stopped", nil
}

// Shell opens an interactive shell in the guest over the vsock agent.
func (v *VZProvider) Shell(sb *config.Sandbox, workdir string) error {
	return v.agentShell(sb, workdir)
}

// Exec runs a one-shot command inside the VM over the vsock agent.
func (v *VZProvider) Exec(sb *config.Sandbox, command ...string) error {
	return v.agentExec(sb, command...)
}

// ------------ helpers ------------

// processAlive reports whether pid names a live process. Uses
// signal-0 — POSIX-defined null signal that performs the kernel's
// permission and existence checks without delivering anything.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

func readPIDFile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}
