//go:build linux

// Firecracker provider (Linux). Boots an OCI image as an ext4 rootfs with
// clawk-init as PID 1 and the clawk-pty-agent on vsock — the same guest
// stack as the vz provider — and talks to it over firecracker's hybrid
// vsock. No sshd: every guest session goes through the agent, like vz.
//
// Networking mirrors vz too: the VM runs out of process under the __fcd
// daemon, which drives an in-process gvproxy (gvisor-tap-vsock) userspace
// TCP/IP stack as the guest's gateway and enforces the same per-connection
// egress allow-list + DNS-aware filtering (internal/netfilter.AllowList).
// gvproxy can't drive firecracker's TAP directly, so the daemon bridges
// the two with a frame pump (fcnet_linux.go).
//
// Known limitations:
//   - One shared bridge / one fixed guest IP, so two firecracker sandboxes
//     at once collide. A per-sandbox /30 is the next step.
//   - No virtio-fs, so the phase worktree is baked into the rootfs at
//     Create time rather than live-mounted; host edits don't propagate.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/guestbuild"
	"github.com/clawkwork/clawk/internal/guestcfg"
	"github.com/clawkwork/clawk/internal/netfilter"
	"github.com/clawkwork/clawk/internal/vsockclient"
	"github.com/clawkwork/clawk/machine"
	"github.com/clawkwork/clawk/machine/oci"

	// Register the firecracker backend.
	_ "github.com/clawkwork/clawk/machine/firecracker"
)

const (
	// fcGuestUser is the guest user sessions run as. Firecracker boots a
	// bare-root rootfs with no `agent` user, so it's root.
	fcGuestUser = "root"
	// fcVSockCID is the guest's AF_VSOCK context ID (any value >= 3).
	fcVSockCID = 3
	// fcAgentPort is the vsock port clawk-pty-agent listens on in the guest.
	fcAgentPort = 1024
)

// FirecrackerProvider implements the Provider + agent interfaces using the
// machine/firecracker backend.
type FirecrackerProvider struct {
	store *config.Store
}

func NewFirecrackerProvider(store *config.Store) *FirecrackerProvider {
	return &FirecrackerProvider{store: store}
}

func (f *FirecrackerProvider) vmDir(sb *config.Sandbox) string { return f.store.VMDir(sb.Name) }

// fcStateDir is the subdirectory the machine library owns (firecracker's
// api/vsock sockets, pidfile, console log). Nested under vmDir so
// `clawk destroy`'s RemoveAll sweep covers it.
func (f *FirecrackerProvider) fcStateDir(sb *config.Sandbox) string {
	return filepath.Join(f.vmDir(sb), "fc")
}

// vsockPath is firecracker's hybrid-vsock UDS; the agent client reaches
// the guest pty-agent through it with a "CONNECT <port>" handshake.
func (f *FirecrackerProvider) vsockPath(sb *config.Sandbox) string {
	return filepath.Join(f.fcStateDir(sb), "vsock.sock")
}

// GuestWorkspaceRoot: firecracker has no `agent` user and no virtio-fs, so
// the worktree is baked into the rootfs under /workspace.
func (f *FirecrackerProvider) GuestWorkspaceRoot() string { return "/workspace" }

// Create stages everything the VM boots from: the firecracker-CI kernel,
// the cross-compiled guest binaries, the OCI rootfs (with the worktree
// baked in), and the clawk-init manifest config disk.
func (f *FirecrackerProvider) Create(sb *config.Sandbox) error {
	if _, err := exec.LookPath("firecracker"); err != nil {
		return errors.New("firecracker binary not on PATH")
	}
	if sb.Image == "" {
		return fmt.Errorf("sandbox %q has no OCI image configured", sb.Name)
	}
	if len(sb.Phases) == 0 || sb.Phases[0].Worktree == "" {
		return fmt.Errorf("sandbox %q has no phase with a Worktree", sb.Name)
	}
	vmDir := f.vmDir(sb)
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		return fmt.Errorf("creating vm dir: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	if _, err := ciEnsureKernel(ctx); err != nil {
		return fmt.Errorf("kernel: %w", err)
	}
	bins, err := guestbuild.Build(ctx, f.store.CacheDir(), runtime.GOARCH)
	if err != nil {
		return fmt.Errorf("guest binaries: %w", err)
	}
	// Materialize the OCI rootfs (build cache + clone) into a per-VM disk
	// we can bake the worktree into — firecracker has no virtio-fs.
	rootfs := filepath.Join(vmDir, "rootfs.raw")
	if _, err := oci.Materialize(ctx, OCIRootFS(sb, f.store.CacheDir(), bins), rootfs); err != nil {
		return fmt.Errorf("materializing rootfs: %w", err)
	}
	if err := f.bakeWorktree(rootfs, sb.Phases[0].Worktree); err != nil {
		return fmt.Errorf("baking worktree: %w", err)
	}
	if err := guestcfg.WriteDisk(f.manifest(sb), filepath.Join(vmDir, "guestcfg.img")); err != nil {
		return fmt.Errorf("guest config disk: %w", err)
	}
	sb.GuestIP = guestIP
	return nil
}

// manifest is the clawk-init boot manifest for firecracker. The guest is
// configured statically with gvproxy's addresses (no DHCP client in arbitrary
// images): gateway + DNS are gvproxy, so DNS answers flow through gvproxy's
// resolver and feed the allow-list's domain matcher. Same values as the vz
// manifest (OCIGuestManifest). No virtio-fs mounts — the worktree is baked
// into the rootfs.
func (f *FirecrackerProvider) manifest(sb *config.Sandbox) guestcfg.Manifest {
	return guestcfg.Manifest{
		Hostname: sb.Name,
		Network: &guestcfg.Network{
			Interface: "eth0",
			Address:   guestIP + "/24",
			Gateway:   gvproxyGateway,
			DNS:       []string{gvproxyGateway},
			MTU:       gvproxyMTU,
		},
		Services: []guestcfg.Service{{Name: "pty-agent", Path: guestcfg.AgentPath}},
	}
}

// bakeWorktree copies the host worktree into the rootfs at the guest
// workspace path. Done at Create time because firecracker can't live-mount.
func (f *FirecrackerProvider) bakeWorktree(rootfs, worktreeSrc string) error {
	rel := strings.TrimPrefix(f.GuestWorkspaceRoot(), "/")
	return mountedRootfs(rootfs, func(mnt string) error {
		target := filepath.Join(mnt, rel, filepath.Base(worktreeSrc))
		if err := runSudo("mkdir", "-p", target); err != nil {
			return err
		}
		// Trailing /. copies contents into target, not the dir itself.
		if err := runSudo("cp", "-a", strings.TrimRight(worktreeSrc, "/")+"/.", target); err != nil {
			return fmt.Errorf("copying worktree: %w", err)
		}
		return nil
	})
}

// Start spawns the detached __fcd daemon — which owns gvproxy, the frame
// pump, and the firecracker VM for the VM's lifetime — and returns once the
// pty-agent answers over vsock. The VM must outlive this CLI invocation, so
// (like the vz provider) the work runs in a child process, not in-process.
func (f *FirecrackerProvider) Start(sb *config.Sandbox) error {
	if _, err := exec.LookPath("firecracker"); err != nil {
		return errors.New("firecracker binary not on PATH")
	}
	// Pre-flight /dev/kvm access. Without this, a user not in the kvm group
	// gets a detached daemon that dies on firecracker's opaque InstanceStart
	// "Permission denied (os error 13) ... /dev/kvm file's ACL" — which the
	// CLI never sees; it only observes the vsock ping timing out ("agent did
	// not become ready"). Checking here surfaces the real cause and the fix
	// in the foreground before the daemon is even spawned.
	if err := checkKVMAccess(); err != nil {
		return err
	}
	vmDir := f.vmDir(sb)
	pidPath := filepath.Join(vmDir, "fc.pid")
	if pid := readPIDFile(pidPath); pid > 0 && processAlive(pid) {
		sb.GuestIP = guestIP
		return nil // already running
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating clawk binary: %w", err)
	}
	// Setsid detaches the daemon from the CLI's controlling terminal so a
	// Ctrl+C on the foreground command doesn't take the VM down with it.
	cmd := exec.Command(self, "__fcd", sb.Name)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawning fcd: %w", err)
	}
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("releasing fcd: %w", err)
	}
	sb.GuestIP = guestIP

	// One agent round trip proves kernel boot, clawk-init, and the pty-agent
	// are all up — the firecracker counterpart of waiting for sshd, minus sshd.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	if err := vsockclient.Ping(ctx, f.vsockPath(sb), fcAgentPort, 120*time.Second); err != nil {
		return fmt.Errorf("agent did not become ready: %w%s",
			err, ConsoleTail(filepath.Join(vmDir, "console.log"), 20))
	}
	return nil
}

// FCStateDir exposes the machine-library state dir to the __fcd daemon.
func (f *FirecrackerProvider) FCStateDir(sb *config.Sandbox) string { return f.fcStateDir(sb) }

// DaemonSpec sets up the host network plumbing (IP-less bridge + the guest's
// TAP + the daemon-owned gvproxy TAP) and returns the machine.Spec the __fcd
// daemon boots. It runs in the daemon process; the returned spec carries a
// UserMode net in TAP-bridge mode so the firecracker backend brings up
// gvproxy (with allow as the egress filter) bridged to the guest's NIC.
func (f *FirecrackerProvider) DaemonSpec(sb *config.Sandbox, allow *netfilter.AllowList) (machine.Spec, error) {
	if err := ensureLinuxBridge(); err != nil {
		return machine.Spec{}, fmt.Errorf("bridge: %w", err)
	}
	fcTap := tapDevice(sb.Name)
	if err := ensureTAP(fcTap); err != nil {
		return machine.Spec{}, fmt.Errorf("guest tap: %w", err)
	}
	gvTap := gvTapDevice(sb.Name)
	if err := ensureTAP(gvTap); err != nil {
		return machine.Spec{}, fmt.Errorf("gvproxy tap: %w", err)
	}
	forwards := make([]machine.PortForward, 0, len(sb.Forwards))
	for _, fwd := range sb.Forwards {
		forwards = append(forwards, machine.PortForward{
			HostPort: uint16(fwd.HostPort), GuestPort: uint16(fwd.GuestPort),
			Proto: machine.ProtoTCP,
		})
	}
	spec := f.buildSpec(sb)
	spec.Net = []machine.Net{machine.UserMode{
		Filter:   allow,
		Forwards: forwards,
		GuestTAP: fcTap,
		HostTAP:  gvTap,
	}}
	return spec, nil
}

// buildSpec assembles the resource/boot/rootfs parts of the machine.Spec.
// The Net entry is filled in by DaemonSpec once the TAPs exist.
func (f *FirecrackerProvider) buildSpec(sb *config.Sandbox) machine.Spec {
	kernel, _ := ciEnsureKernel(context.Background())
	vcpu := uint(1)
	if sb.CPU > 0 {
		vcpu = sb.CPU
	}
	memMiB := uint64(512)
	if sb.MemoryMiB > 0 {
		memMiB = sb.MemoryMiB
	}
	memMaxMiB := memMiB
	if sb.MemoryMaxMiB > memMaxMiB {
		memMaxMiB = sb.MemoryMaxMiB
	}
	return machine.Spec{
		ID:           sb.Name,
		VCPU:         vcpu,
		MemoryMiB:    memMiB,
		MemoryMaxMiB: memMaxMiB,
		Boot: machine.DirectKernel{
			Vmlinux: kernel,
			// Firecracker's serial is ttyS0 (no virtio-console/hvc0).
			// clawk-init reads the manifest on /dev/vdb and configures the
			// network from it, so there's no ip= cmdline.
			Cmdline: "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw " +
				"init=" + guestcfg.InitPath + " clawk.cfg=/dev/vdb",
		},
		RootFS:   machine.RawDisk{Path: filepath.Join(f.vmDir(sb), "rootfs.raw")},
		Disks:    []machine.Disk{{Path: filepath.Join(f.vmDir(sb), "guestcfg.img"), ReadOnly: true}},
		VSockCID: fcVSockCID,
		Serial:   machine.Serial{LogPath: filepath.Join(f.vmDir(sb), "console.log")},
	}
}

// Stop signals the __fcd daemon, which tears down the VM, the frame pump, and
// gvproxy. A missing/stale pidfile is not an error.
//
// The timeout MUST exceed the daemon's own graceful-stop budget (gracefulStop
// gives m.Stop 15s: CtrlAltDel wait 10s + SIGTERM-firecracker wait 5s). The
// minimal OCI guest doesn't power off on CtrlAltDel, so the daemon always
// spends ~10s there before falling through to SIGTERM the firecracker child.
// If we SIGKILL the daemon before that completes, firecracker is orphaned —
// it keeps the guest TAP open, and the next boot fails with "Open tap device
// failed: Resource busy". 25s leaves margin over the 15s budget plus the
// daemon's post-stop cleanup.
func (f *FirecrackerProvider) Stop(sb *config.Sandbox) error {
	return stopByPIDFile(filepath.Join(f.vmDir(sb), "fc.pid"), 25*time.Second)
}

func (f *FirecrackerProvider) Destroy(sb *config.Sandbox) error {
	_ = f.Stop(sb)
	_ = runSudo("ip", "link", "del", tapDevice(sb.Name))
	_ = runSudo("ip", "link", "del", gvTapDevice(sb.Name))
	return os.RemoveAll(f.vmDir(sb))
}

func (f *FirecrackerProvider) Status(sb *config.Sandbox) (string, error) {
	pid := readPIDFile(filepath.Join(f.vmDir(sb), "fc.pid"))
	if pid > 0 && processAlive(pid) {
		return "running", nil
	}
	return "stopped", nil
}

// --- agent (vsock) sessions ---

// Shell opens an interactive login shell in the guest over the vsock agent.
func (f *FirecrackerProvider) Shell(sb *config.Sandbox, workdir string) error {
	code, err := vsockclient.Run(context.Background(), vsockclient.Config{
		SocketPath:  f.vsockPath(sb),
		ConnectPort: fcAgentPort,
		Cwd:         workdir,
		User:        fcGuestUser,
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

// Exec runs a command in the guest over the vsock agent. Used for the
// coding-agent attach (claude), so it's interactive.
func (f *FirecrackerProvider) Exec(sb *config.Sandbox, command ...string) error {
	if len(command) == 0 {
		return fmt.Errorf("firecracker exec: empty command")
	}
	code, err := vsockclient.Run(context.Background(), vsockclient.Config{
		SocketPath:  f.vsockPath(sb),
		ConnectPort: fcAgentPort,
		Cmd:         command[0],
		Args:        command[1:],
		User:        fcGuestUser,
		// Exec carries the coding-agent (claude) attach — a full-screen
		// TUI, so clear to a clean canvas.
		ClearScreen: true,
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("command exited with status %d", code)
	}
	return nil
}

// ExecCapture runs a command non-interactively and returns its combined
// output over the agent's frame protocol.
func (f *FirecrackerProvider) ExecCapture(sb *config.Sandbox, command ...string) (string, error) {
	if len(command) == 0 {
		return "", fmt.Errorf("firecracker execcapture: empty command")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	out, code, err := vsockclient.Output(ctx, f.vsockPath(sb), fcAgentPort,
		fcGuestUser, command[0], command[1:]...)
	if err != nil {
		return out, err
	}
	if code != 0 {
		return out, fmt.Errorf("command exited with status %d", code)
	}
	return out, nil
}
