//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/clawkwork/clawk/machine"
	"github.com/clawkwork/clawk/machine/internal/usermode"
	"github.com/clawkwork/clawk/machine/oci"
)

func init() { machine.Register(backend{}) }

type backend struct{}

func (backend) Name() string { return Name }

func (backend) Capabilities() machine.Caps {
	return machine.Caps{
		Snapshot:     true,
		OCIRootFS:    true,
		DirectKernel: true,
		VSock:        true,
		TAPNet:       true,
		UserModeNet:  true,
	}
}

func (backend) New(_ context.Context, spec machine.Spec, stateDir string) (machine.Machine, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if _, err := exec.LookPath(Name); err != nil {
		return nil, fmt.Errorf("firecracker: binary not on PATH")
	}
	if err := checkSpec(spec); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("firecracker: creating state dir: %w", err)
	}
	return &vm{spec: spec, stateDir: stateDir}, nil
}

func checkSpec(spec machine.Spec) error {
	if spec.NestedVirt {
		return fmt.Errorf("%w: firecracker does not support nested virtualization", machine.ErrUnsupportedSpec)
	}
	if _, ok := spec.Boot.(machine.DirectKernel); !ok {
		return fmt.Errorf("%w: firecracker only supports DirectKernel boot", machine.ErrUnsupportedSpec)
	}
	switch spec.RootFS.(type) {
	case machine.RawDisk, machine.OCIImage:
		// ok
	default:
		return fmt.Errorf("%w: unsupported RootFS %T", machine.ErrUnsupportedSpec, spec.RootFS)
	}
	for _, n := range spec.Net {
		switch nn := n.(type) {
		case machine.TAP:
			// ok
		case machine.UserMode:
			// gvproxy bridges to firecracker only in TAP-bridge mode: the VM
			// boots on GuestTAP and gvproxy is pumped to HostTAP. The fd-NIC
			// mode (both empty) has no firecracker analogue.
			if nn.GuestTAP == "" || nn.HostTAP == "" {
				return fmt.Errorf("%w: firecracker UserMode requires GuestTAP and HostTAP (TAP-bridge mode)", machine.ErrUnsupportedSpec)
			}
		case machine.Unixgram:
			return fmt.Errorf("%w: firecracker does not support %T net", machine.ErrUnsupportedSpec, n)
		default:
			return fmt.Errorf("%w: unsupported Net %T", machine.ErrUnsupportedSpec, n)
		}
	}
	if len(spec.Shares) > 0 {
		// Firecracker does not ship a virtiofsd integration out of the box.
		// Surface a clear error instead of silently dropping shares.
		return fmt.Errorf("%w: virtio-fs shares require an external virtiofsd not yet wired", machine.ErrUnsupportedSpec)
	}
	return nil
}

type vm struct {
	spec     machine.Spec
	stateDir string

	mu    sync.Mutex
	state machine.State
	proc  *os.Process

	// gvproxy lifecycle, set when Spec.Net has a UserMode entry (TAP-bridge
	// mode). umMAC is gvproxy's lease MAC, assigned to the VM's NIC.
	umStack  *usermode.Stack
	umCancel context.CancelFunc
	umMAC    string
}

func (v *vm) apiSockPath() string { return filepath.Join(v.stateDir, "firecracker.sock") }
func (v *vm) pidPath() string     { return filepath.Join(v.stateDir, "firecracker.pid") }
func (v *vm) vsockPath() string   { return filepath.Join(v.stateDir, "vsock.sock") }
func (v *vm) logPath() string     { return filepath.Join(v.stateDir, "firecracker.log") }

// consolePath is where the guest serial console (ttyS0) is captured.
// Firecracker wires the guest serial to its process stdout; callers set
// Spec.Serial.LogPath to collect it somewhere their diagnostics look (clawk
// scans it for kernel panics and tails it on a boot failure). Empty falls
// back to firecracker.log, preserving the old commingled behavior.
func (v *vm) consolePath() string {
	if v.spec.Serial.LogPath != "" {
		return v.spec.Serial.LogPath
	}
	return v.logPath()
}

func (v *vm) Create(ctx context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.state == machine.StateDestroyed {
		return fmt.Errorf("%w: create after destroy", machine.ErrInvalidState)
	}
	var disk machine.RawDisk
	if rd, ok := v.spec.RootFS.(machine.RawDisk); ok {
		// The rootfs is already a per-VM raw disk — the clawk firecracker
		// provider builds one in the VM dir with the worktree baked in.
		// Boot from it in place: cloning it into stateDir would just double
		// the on-disk footprint of a large image (and a 1+ GB image can
		// exhaust a small host). Only OCIImage sources need materializing.
		if _, err := os.Stat(rd.Path); err != nil {
			return fmt.Errorf("firecracker: rootfs %q: %w", rd.Path, err)
		}
		disk = rd
	} else {
		d, err := oci.Materialize(ctx, v.spec.RootFS, filepath.Join(v.stateDir, "disk.raw"))
		if err != nil {
			return fmt.Errorf("firecracker: materializing rootfs: %w", err)
		}
		disk = d
	}
	if _, err := os.Stat(disk.Path); err != nil {
		return fmt.Errorf("firecracker: rootfs %q: %w", disk.Path, err)
	}
	dk := v.spec.Boot.(machine.DirectKernel)
	if _, err := os.Stat(dk.Vmlinux); err != nil {
		return fmt.Errorf("firecracker: kernel %q: %w", dk.Vmlinux, err)
	}
	v.spec.RootFS = disk
	v.state = machine.StateCreated
	return nil
}

func (v *vm) Start(ctx context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.state == machine.StateRunning {
		return nil
	}
	if v.state == machine.StateDestroyed {
		return fmt.Errorf("%w: start after destroy", machine.ErrInvalidState)
	}
	if err := v.spawn(ctx); err != nil {
		return err
	}
	// gvproxy must come up before configure: it provides the NIC MAC.
	if err := v.startUserMode(); err != nil {
		_ = v.killChild()
		return err
	}
	if err := v.configure(ctx); err != nil {
		v.stopUserMode()
		_ = v.killChild()
		return err
	}
	if err := v.api().action(ctx, "InstanceStart"); err != nil {
		v.stopUserMode()
		_ = v.killChild()
		return fmt.Errorf("firecracker: InstanceStart: %w", err)
	}
	v.state = machine.StateRunning
	return nil
}

// userModeNet returns the spec's UserMode net entry, if any.
func (v *vm) userModeNet() (machine.UserMode, bool) {
	for _, n := range v.spec.Net {
		if um, ok := n.(machine.UserMode); ok {
			return um, true
		}
	}
	return machine.UserMode{}, false
}

// startUserMode brings up the in-process gvproxy stack and the frame pump
// joining it to the host TAP, when the spec selects TAP-bridge UserMode. A
// no-op (and nil) otherwise. The gvproxy lease MAC is recorded for configure
// to assign to the VM's NIC.
func (v *vm) startUserMode() error {
	um, ok := v.userModeNet()
	if !ok {
		return nil
	}
	stack, err := usermode.Start(usermode.Config{
		SockPath: filepath.Join(v.stateDir, "gvproxy.sock"),
		Forwards: um.Forwards,
		Filter:   um.Filter,
	})
	if err != nil {
		return fmt.Errorf("firecracker: gvproxy: %w", err)
	}
	tap, err := openTAP(um.HostTAP)
	if err != nil {
		stack.Close()
		return fmt.Errorf("firecracker: open host tap %q: %w", um.HostTAP, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := startPump(ctx, tap, stack.VMSocket); err != nil {
		cancel()
		tap.Close()
		stack.Close()
		return fmt.Errorf("firecracker: %w", err)
	}
	go func() { _ = stack.Serve(ctx) }()
	v.umStack = stack
	v.umCancel = cancel
	v.umMAC = stack.GuestMAC
	return nil
}

// stopUserMode tears down the gvproxy stack and frame pump. Idempotent.
func (v *vm) stopUserMode() {
	if v.umCancel != nil {
		v.umCancel() // unblocks the pump + gvproxy Serve, closing both TAP fds
		v.umCancel = nil
	}
	if v.umStack != nil {
		v.umStack.Close() // resets the process-global filter + host socket
		v.umStack = nil
	}
}

func (v *vm) Stop(ctx context.Context, graceful bool) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	// Tear down gvproxy on every return path — including the early one below,
	// which fires when a State() probe already flipped us out of Running.
	defer v.stopUserMode()
	if v.state != machine.StateRunning && v.state != machine.StatePaused {
		return nil
	}
	// A paused guest can't service CtrlAltDel; resume first so the
	// graceful path has a live guest. Failure degrades to the kill path.
	if v.state == machine.StatePaused {
		if err := v.api().patchVM(ctx, "Resumed"); err != nil {
			graceful = false
		} else {
			v.state = machine.StateRunning
		}
	}

	if graceful {
		// Firecracker only supports CtrlAltDel on x86_64. Best-effort: try
		// it; fall through to SIGTERM if the guest doesn't honour it in time.
		//
		// A guest that honours CtrlAltDel halts in well under a second (the
		// microVM has no disks to spin down); a guest whose kernel lacks the
		// i8042 keyboard path — as the firecracker-ci kernel does — never
		// receives it at all and would burn the whole wait for nothing. Keep
		// the window short so a `down` on such a guest doesn't stall: we fall
		// through to SIGTERM the VMM, which for a disposable sandbox is a fine
		// stop.
		_ = v.api().action(ctx, "SendCtrlAltDel")
		if v.waitExit(3 * time.Second) {
			v.cleanupFiles()
			v.state = machine.StateStopped
			return nil
		}
	}

	if v.proc != nil {
		sig := syscall.SIGTERM
		if !graceful {
			sig = syscall.SIGKILL
		}
		_ = v.proc.Signal(sig)
	}
	if !v.waitExit(3 * time.Second) {
		_ = v.killChild()
	}
	v.cleanupFiles()
	v.state = machine.StateStopped
	return nil
}

func (v *vm) Destroy(ctx context.Context) error {
	if err := v.Stop(ctx, true); err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.state = machine.StateDestroyed
	return os.RemoveAll(v.stateDir)
}

func (v *vm) State(_ context.Context) (machine.State, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.state != machine.StateRunning && v.state != machine.StatePaused {
		return v.state, nil
	}
	if v.proc == nil || !alive(v.proc.Pid) {
		v.state = machine.StateStopped
	}
	return v.state, nil
}

// Pause suspends the vCPUs via the firecracker API and moves the Machine
// to StatePaused. Idempotent.
func (v *vm) Pause(ctx context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.state != machine.StateRunning {
		return nil // idempotent: already paused, or nothing to pause
	}
	if err := v.api().patchVM(ctx, "Paused"); err != nil {
		return fmt.Errorf("firecracker: pause: %w", err)
	}
	v.state = machine.StatePaused
	return nil
}

// Resume restarts the vCPUs after a Pause. Idempotent.
func (v *vm) Resume(ctx context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.state != machine.StatePaused {
		return nil // idempotent: already running, or nothing to resume
	}
	if err := v.api().patchVM(ctx, "Resumed"); err != nil {
		return fmt.Errorf("firecracker: resume: %w", err)
	}
	v.state = machine.StateRunning
	return nil
}

func (v *vm) VSock(ctx context.Context, port uint32) (net.Conn, error) {
	if v.spec.VSockCID == 0 {
		return nil, machine.ErrVSockUnsupported
	}
	// Match the vz backend's contract: a paused guest accepts the UDS
	// connect but never completes the CONNECT handshake, so without this
	// guard dialers block in readVsockOK instead of failing fast.
	if s, _ := v.State(ctx); s != machine.StateRunning {
		return nil, fmt.Errorf("%w: VSock requires running VM (state %s)", machine.ErrInvalidState, s)
	}
	// Firecracker's vsock stub accepts on a unix socket and expects the
	// dialer to send "CONNECT <port>\n" as the first line. See:
	// https://github.com/firecracker-microvm/firecracker/blob/main/docs/vsock.md
	c, err := net.Dial("unix", v.vsockPath())
	if err != nil {
		return nil, fmt.Errorf("firecracker: dial vsock uds: %w", err)
	}
	if _, err := fmt.Fprintf(c, "CONNECT %d\n", port); err != nil {
		c.Close()
		return nil, fmt.Errorf("firecracker: vsock CONNECT: %w", err)
	}
	if err := readVsockOK(c); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// Snapshot pauses the VM, writes memory + state under dir, and resumes.
func (v *vm) Snapshot(ctx context.Context, dir string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.state != machine.StateRunning {
		return fmt.Errorf("%w: snapshot requires running", machine.ErrInvalidState)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("firecracker: snapshot dir: %w", err)
	}
	api := v.api()
	if err := api.patchVM(ctx, "Paused"); err != nil {
		return fmt.Errorf("firecracker: pausing: %w", err)
	}
	err := v.slowAPI().createSnapshot(ctx, snapshotCreate{
		SnapshotType: "Full",
		SnapshotPath: filepath.Join(dir, "snapshot.state"),
		MemFilePath:  filepath.Join(dir, "snapshot.mem"),
	})
	// Try to resume even if the snapshot failed so we don't leave the VM
	// paused.
	if resumeErr := api.patchVM(ctx, "Resumed"); resumeErr != nil && err == nil {
		err = fmt.Errorf("firecracker: resuming after snapshot: %w", resumeErr)
	}
	return err
}

// Suspend hibernates the VM: pause (if running), write memory + device
// state under dir, and kill the firecracker process WITHOUT resuming —
// the guest never executes past the save point, so the rootfs on disk
// stays frozen at exactly the saved moment and a later Restore of the
// pair is safe. On save failure the guest is resumed (when it was
// running) so the VM isn't left wedged.
func (v *vm) Suspend(ctx context.Context, dir string) error {
	v.mu.Lock()
	if v.state != machine.StateRunning && v.state != machine.StatePaused {
		v.mu.Unlock()
		return fmt.Errorf("%w: suspend requires a running or paused VM", machine.ErrInvalidState)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		v.mu.Unlock()
		return fmt.Errorf("firecracker: suspend dir: %w", err)
	}
	api := v.api()
	wasRunning := v.state == machine.StateRunning
	if wasRunning {
		if err := api.patchVM(ctx, "Paused"); err != nil {
			v.mu.Unlock()
			return fmt.Errorf("firecracker: pausing for suspend: %w", err)
		}
		v.state = machine.StatePaused
	}
	v.mu.Unlock()

	// The snapshot writes the guest's entire memory image — run it
	// without v.mu held so State probes and the lifecycle endpoint keep
	// answering (accurately, as StatePaused) for the duration.
	stateFile := filepath.Join(dir, "snapshot.state")
	saveErr := v.slowAPI().createSnapshot(ctx, snapshotCreate{
		SnapshotType: "Full",
		SnapshotPath: stateFile,
		MemFilePath:  filepath.Join(dir, "snapshot.mem"),
	})

	v.mu.Lock()
	defer v.mu.Unlock()
	if saveErr != nil {
		if wasRunning && v.state == machine.StatePaused {
			if resumeErr := api.patchVM(ctx, "Resumed"); resumeErr == nil {
				v.state = machine.StateRunning
			}
		}
		return fmt.Errorf("firecracker: save snapshot: %w", saveErr)
	}
	// A concurrent Stop can win the race while the lock was released: it
	// resumes the paused guest to shut it down, so the guest ran past the
	// save point and the snapshot is unsafe to ever restore.
	if v.state != machine.StatePaused {
		_ = os.Remove(stateFile)
		return fmt.Errorf("%w: vm was stopped during suspend", machine.ErrInvalidState)
	}
	// Stop without resuming: kill the paused VMM outright — a graceful
	// path would wake the guest and let the disk drift past the snapshot.
	defer v.stopUserMode()
	_ = v.killChild()
	_ = v.waitExit(5 * time.Second)
	v.cleanupFiles()
	v.state = machine.StateStopped
	return nil
}

// Restore boots a paused VM from a snapshot dir produced by Snapshot or
// Suspend.
func (v *vm) Restore(ctx context.Context, dir string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.state == machine.StateRunning {
		return fmt.Errorf("%w: restore into running VM", machine.ErrInvalidState)
	}
	if v.state == machine.StateDestroyed {
		return fmt.Errorf("%w: restore after destroy", machine.ErrInvalidState)
	}
	if err := v.spawn(ctx); err != nil {
		return err
	}
	// The snapshot carries the guest's device config, but the host half of
	// the network path (gvproxy + frame pump on the TAP) lives in this
	// process and has to come up fresh, same as Start.
	if err := v.startUserMode(); err != nil {
		_ = v.killChild()
		return err
	}
	err := v.slowAPI().loadSnapshot(ctx, snapshotLoad{
		SnapshotPath: filepath.Join(dir, "snapshot.state"),
		MemBackend: memBackend{
			BackendType: "File",
			BackendPath: filepath.Join(dir, "snapshot.mem"),
		},
		ResumeVM: true,
	})
	if err != nil {
		v.stopUserMode()
		_ = v.killChild()
		return fmt.Errorf("firecracker: load snapshot: %w", err)
	}
	v.state = machine.StateRunning
	return nil
}

// --- internals ---

func (v *vm) spawn(_ context.Context) error {
	apiSock := v.apiSockPath()
	_ = os.Remove(apiSock) // stale from previous run
	// Same for the vsock UDS: firecracker bind()s it and refuses to start
	// ("Address in use") if a socket from a prior boot is still on disk,
	// so a down→up cycle (or a crash) would wedge without this.
	_ = os.Remove(v.vsockPath())
	logFile, err := os.Create(v.logPath())
	if err != nil {
		return fmt.Errorf("firecracker: opening log: %w", err)
	}
	defer logFile.Close()

	// Guest serial (ttyS0) is firecracker's stdout; its own operational logs
	// go to stderr. Split them so the kernel console lands in consolePath()
	// (where clawk's panic scan and boot-failure tail read) instead of being
	// interleaved with firecracker's log lines. When Serial.LogPath is unset
	// consolePath()==logPath(), so both share one file as before.
	consoleFile := logFile
	if cp := v.consolePath(); cp != v.logPath() {
		consoleFile, err = os.Create(cp)
		if err != nil {
			return fmt.Errorf("firecracker: opening console log: %w", err)
		}
		defer consoleFile.Close()
	}

	cmd := exec.Command(Name, "--api-sock", apiSock)
	cmd.Stdout = consoleFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("firecracker: starting: %w", err)
	}
	v.proc = cmd.Process
	if err := os.WriteFile(v.pidPath(), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("firecracker: writing pidfile: %w", err)
	}
	// Wait for the API socket to appear; firecracker creates it after init.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(apiSock); err == nil {
			return nil
		}
		if !alive(cmd.Process.Pid) {
			return fmt.Errorf("firecracker: exited before API socket appeared (see %s)", v.logPath())
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	return fmt.Errorf("firecracker: API socket did not appear within 5s")
}

func (v *vm) configure(ctx context.Context) error {
	dk := v.spec.Boot.(machine.DirectKernel)
	disk := v.spec.RootFS.(machine.RawDisk)
	api := v.api()

	// Guest-visible memory is the max when set; otherwise baseline. The
	// balloon device (below) holds the delta back from the guest at boot.
	memMiB := v.spec.MemoryMiB
	if v.spec.MemoryMaxMiB > memMiB {
		memMiB = v.spec.MemoryMaxMiB
	}
	if err := api.putMachineConfig(ctx, machineConfig{
		VCPUCount:  int(v.spec.VCPU),
		MemSizeMiB: int(memMiB),
	}); err != nil {
		return fmt.Errorf("firecracker: /machine-config: %w", err)
	}
	// Balloon: only configured when max > baseline. amount_mib is the
	// initial balloon size — memory firecracker will reclaim from the
	// guest at boot and return to the host. deflate_on_oom lets the
	// kernel pop the balloon before invoking oom-killer, so a burst above
	// the baseline just grows host RSS instead of killing processes.
	if v.spec.MemoryMaxMiB > v.spec.MemoryMiB {
		reclaim := v.spec.MemoryMaxMiB - v.spec.MemoryMiB
		if err := api.putBalloon(ctx, balloon{
			AmountMiB:             int(reclaim),
			DeflateOnOOM:          true,
			StatsPollingIntervalS: 5,
		}); err != nil {
			return fmt.Errorf("firecracker: /balloon: %w", err)
		}
	}
	if err := api.putBootSource(ctx, bootSource{
		KernelImagePath: dk.Vmlinux,
		InitrdPath:      dk.Initrd,
		BootArgs:        dk.Cmdline,
	}); err != nil {
		return fmt.Errorf("firecracker: /boot-source: %w", err)
	}
	if err := api.putDrive(ctx, "rootfs", drive{
		DriveID:      "rootfs",
		PathOnHost:   disk.Path,
		IsRootDevice: true,
		IsReadOnly:   disk.ReadOnly,
	}); err != nil {
		return fmt.Errorf("firecracker: /drives/rootfs: %w", err)
	}
	for i, d := range v.spec.Disks {
		id := fmt.Sprintf("disk%d", i)
		if err := api.putDrive(ctx, id, drive{
			DriveID:      id,
			PathOnHost:   d.Path,
			IsRootDevice: false,
			IsReadOnly:   d.ReadOnly,
		}); err != nil {
			return fmt.Errorf("firecracker: /drives/%s: %w", id, err)
		}
	}
	for i, n := range v.spec.Net {
		id := fmt.Sprintf("eth%d", i)
		var iface networkIface
		switch nn := n.(type) {
		case machine.TAP:
			iface = networkIface{IfaceID: id, HostDevName: nn.Device, GuestMAC: nn.MAC}
		case machine.UserMode:
			// gvproxy (startUserMode) is bridged to nn.HostTAP; the VM's NIC
			// is nn.GuestTAP, with the MAC gvproxy's DHCP lease expects.
			iface = networkIface{IfaceID: id, HostDevName: nn.GuestTAP, GuestMAC: v.umMAC}
		default:
			continue
		}
		if err := api.putNetwork(ctx, id, iface); err != nil {
			return fmt.Errorf("firecracker: /network-interfaces/%s: %w", id, err)
		}
	}
	if v.spec.VSockCID != 0 {
		if err := api.putVsock(ctx, vsockConfig{
			GuestCID: int(v.spec.VSockCID),
			UDSPath:  v.vsockPath(),
		}); err != nil {
			return fmt.Errorf("firecracker: /vsock: %w", err)
		}
	}
	return nil
}

func (v *vm) api() *apiClient { return newAPIClient(v.apiSockPath()) }

// slowAPI is the client for snapshot save/load: firecracker answers those
// only after the guest's entire memory image has been written or read, so
// the regular client's hard 10s timeout would abort any non-trivial guest.
// Callers bound these with their ctx instead.
func (v *vm) slowAPI() *apiClient { return newAPIClientTimeout(v.apiSockPath(), 0) }

func (v *vm) killChild() error {
	if v.proc == nil {
		return nil
	}
	err := v.proc.Kill()
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

func (v *vm) waitExit(timeout time.Duration) bool {
	if v.proc == nil {
		return true
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !alive(v.proc.Pid) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func (v *vm) cleanupFiles() {
	_ = os.Remove(v.pidPath())
	_ = os.Remove(v.apiSockPath())
	// Leave logs in place for post-mortem.
}

// --- REST client ---

type apiClient struct{ hc *http.Client }

func newAPIClient(sockPath string) *apiClient {
	return newAPIClientTimeout(sockPath, 10*time.Second)
}

// newAPIClientTimeout builds an API client with the given hard timeout;
// zero means no client-side cap (the caller's ctx is the only bound).
func newAPIClientTimeout(sockPath string, timeout time.Duration) *apiClient {
	return &apiClient{hc: &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
	}}
}

func (c *apiClient) do(ctx context.Context, method, path string, body any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://fc"+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		return nil
	}
	msg, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("firecracker %s %s returned %d: %s",
		method, path, resp.StatusCode, strings.TrimSpace(string(msg)))
}

func (c *apiClient) putMachineConfig(ctx context.Context, m machineConfig) error {
	return c.do(ctx, http.MethodPut, "/machine-config", m)
}
func (c *apiClient) putBootSource(ctx context.Context, b bootSource) error {
	return c.do(ctx, http.MethodPut, "/boot-source", b)
}
func (c *apiClient) putDrive(ctx context.Context, id string, d drive) error {
	return c.do(ctx, http.MethodPut, "/drives/"+id, d)
}
func (c *apiClient) putNetwork(ctx context.Context, id string, n networkIface) error {
	return c.do(ctx, http.MethodPut, "/network-interfaces/"+id, n)
}
func (c *apiClient) putVsock(ctx context.Context, v vsockConfig) error {
	return c.do(ctx, http.MethodPut, "/vsock", v)
}
func (c *apiClient) putBalloon(ctx context.Context, b balloon) error {
	return c.do(ctx, http.MethodPut, "/balloon", b)
}
func (c *apiClient) action(ctx context.Context, action string) error {
	return c.do(ctx, http.MethodPut, "/actions", map[string]string{"action_type": action})
}
func (c *apiClient) patchVM(ctx context.Context, state string) error {
	return c.do(ctx, http.MethodPatch, "/vm", map[string]string{"state": state})
}
func (c *apiClient) createSnapshot(ctx context.Context, s snapshotCreate) error {
	return c.do(ctx, http.MethodPut, "/snapshot/create", s)
}
func (c *apiClient) loadSnapshot(ctx context.Context, s snapshotLoad) error {
	return c.do(ctx, http.MethodPut, "/snapshot/load", s)
}

// --- wire types ---

type machineConfig struct {
	VCPUCount  int `json:"vcpu_count"`
	MemSizeMiB int `json:"mem_size_mib"`
}

type bootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	InitrdPath      string `json:"initrd_path,omitempty"`
	BootArgs        string `json:"boot_args,omitempty"`
}

type drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

type networkIface struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
	GuestMAC    string `json:"guest_mac,omitempty"`
}

type vsockConfig struct {
	GuestCID int    `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

// balloon is the wire form of firecracker's /balloon PUT. amount_mib is
// the initial balloon size (pages reclaimed from guest). deflate_on_oom
// lets the guest kernel pop the balloon before invoking oom-killer.
// stats_polling_interval_s > 0 enables periodic balloon stats in
// /balloon/statistics; set to 0 to disable.
type balloon struct {
	AmountMiB             int  `json:"amount_mib"`
	DeflateOnOOM          bool `json:"deflate_on_oom"`
	StatsPollingIntervalS int  `json:"stats_polling_interval_s,omitempty"`
}

type snapshotCreate struct {
	SnapshotType string `json:"snapshot_type"`
	SnapshotPath string `json:"snapshot_path"`
	MemFilePath  string `json:"mem_file_path"`
}

type memBackend struct {
	BackendType string `json:"backend_type"`
	BackendPath string `json:"backend_path"`
}

type snapshotLoad struct {
	SnapshotPath string     `json:"snapshot_path"`
	MemBackend   memBackend `json:"mem_backend"`
	ResumeVM     bool       `json:"resume_vm"`
}

// --- helpers ---

func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// readVsockOK consumes "OK <port>\n" from a firecracker vsock hybrid
// connection. See the CONNECT protocol in firecracker's vsock docs.
func readVsockOK(c net.Conn) error {
	if err := c.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return err
	}
	defer c.SetReadDeadline(time.Time{})
	buf := make([]byte, 32)
	n, err := c.Read(buf)
	if err != nil {
		return fmt.Errorf("firecracker: reading vsock CONNECT response: %w", err)
	}
	line := string(buf[:n])
	if !strings.HasPrefix(line, "OK ") {
		return fmt.Errorf("firecracker: vsock CONNECT rejected: %q", strings.TrimSpace(line))
	}
	return nil
}

// Compile-time guard so the sealed unions don't drift.
var _ = []machine.Net{machine.UserMode{}, machine.TAP{}, machine.Unixgram{}}
