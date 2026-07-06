//go:build darwin

package vz

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	codevz "github.com/Code-Hex/vz/v3"
	"github.com/clawkwork/clawk/machine"
	"github.com/clawkwork/clawk/machine/internal/debug"
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
		EFIBoot:      true,
		UserModeNet:  true,
		UnixgramNet:  true,
		VirtioFS:     true,
		VSock:        true,
		// Hardware-dependent — macOS 15+ on M3+ gates this. The check
		// is cheap (single framework call), so do it per query rather
		// than cache at init.
		NestedVirt: codevz.IsNestedVirtualizationSupported(),
	}
}

func (backend) New(_ context.Context, spec machine.Spec, stateDir string) (machine.Machine, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if err := checkSpec(spec); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("vz: creating state dir: %w", err)
	}
	return &vm{spec: spec, stateDir: stateDir}, nil
}

func checkSpec(spec machine.Spec) error {
	switch spec.Boot.(type) {
	case machine.DirectKernel, machine.EFIBoot:
		// ok
	default:
		return fmt.Errorf("%w: unsupported Boot %T", machine.ErrUnsupportedSpec, spec.Boot)
	}
	switch spec.RootFS.(type) {
	case machine.RawDisk, machine.OCIImage:
		// ok
	default:
		return fmt.Errorf("%w: unsupported RootFS %T", machine.ErrUnsupportedSpec, spec.RootFS)
	}
	for _, n := range spec.Net {
		switch n.(type) {
		case machine.UserMode, machine.Unixgram:
			// ok
		default:
			return fmt.Errorf("%w: unsupported Net %T", machine.ErrUnsupportedSpec, n)
		}
	}
	return nil
}

type vm struct {
	spec     machine.Spec
	stateDir string

	mu          sync.Mutex
	state       machine.State
	config      *codevz.VirtualMachineConfiguration
	machine     *codevz.VirtualMachine
	vsockDev    *codevz.VirtioSocketDevice
	stack       *usermode.Stack
	vmSocket    *os.File
	stateLogger stateLogger // optional; receives every state transition

	// Host memory-pressure response: the balloon hands guest RAM back to the
	// host under pressure (see startPressureResponse). balloon and
	// balloonTargetBytes are only touched under mu; pressureCancel stops the
	// watcher goroutine during cleanup.
	balloon            *codevz.VirtioTraditionalMemoryBalloonDevice
	balloonTargetBytes uint64
	pressureCancel     context.CancelFunc

	// keepAlive retains *os.File handles whose underlying fds are passed
	// by number to Code-Hex/vz and NOT dup'd on the C side (serial
	// console is the known case). Without these refs, Go GC closes the
	// fd from the file's runtime finalizer and the kernel's output
	// writes vanish. Released on cleanup.
	keepAlive []*os.File

	serveCtx    context.Context
	serveCancel context.CancelFunc
	serveDone   chan error

	runCtx    context.Context
	runCancel context.CancelFunc
	runDone   chan error
}

func (v *vm) Create(ctx context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.state == machine.StateDestroyed {
		return fmt.Errorf("%w: create after destroy", machine.ErrInvalidState)
	}
	debug.Log("vz", "create", "id", v.spec.ID, "state_dir", v.stateDir)
	disk, err := oci.Materialize(ctx, v.spec.RootFS, filepath.Join(v.stateDir, "disk.raw"))
	if err != nil {
		return fmt.Errorf("vz: materializing rootfs: %w", err)
	}
	if _, err := os.Stat(disk.Path); err != nil {
		return fmt.Errorf("vz: rootfs %q: %w", disk.Path, err)
	}
	v.spec.RootFS = disk
	if dk, ok := v.spec.Boot.(machine.DirectKernel); ok {
		if _, err := os.Stat(dk.Vmlinux); err != nil {
			return fmt.Errorf("vz: kernel %q: %w", dk.Vmlinux, err)
		}
	}
	cfg, m, err := v.build()
	if err != nil {
		return err
	}
	v.config = cfg
	v.machine = m
	v.state = machine.StateCreated
	return nil
}

func (v *vm) Start(_ context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.state == machine.StateRunning {
		return nil
	}
	if v.state == machine.StateDestroyed {
		return fmt.Errorf("%w: start after destroy", machine.ErrInvalidState)
	}
	if v.machine == nil {
		return fmt.Errorf("%w: Start before Create", machine.ErrInvalidState)
	}
	debug.Log("vz", "starting vm",
		"id", v.spec.ID,
		"cpu", v.spec.VCPU,
		"mem_mib", v.spec.MemoryMiB,
		"shares", len(v.spec.Shares),
		"disks", 1+len(v.spec.Disks))
	if err := v.machine.Start(); err != nil {
		debug.Log("vz", "start failed", "id", v.spec.ID, "err", err)
		return fmt.Errorf("vz: starting vm: %w", err)
	}
	// SocketDevices is only populated after Start; we configured a
	// virtio-socket device in build() so devices[0] is ours.
	if devs := v.machine.SocketDevices(); len(devs) > 0 {
		v.vsockDev = devs[0]
	}
	v.runGoroutine()
	v.serveGoroutine()
	v.state = machine.StateRunning
	v.startBalloonController()
	return nil
}

func (v *vm) Stop(_ context.Context, graceful bool) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if (v.state != machine.StateRunning && v.state != machine.StatePaused) || v.machine == nil {
		debug.Log("vz", "stop skipped (not running)",
			"id", v.spec.ID, "state", v.state)
		return nil
	}
	// A paused guest can't process a RequestStop — its vCPUs are frozen.
	// Resume first so the graceful path below has a live guest to talk
	// to; a resume failure just degrades to the force path.
	if v.state == machine.StatePaused {
		if err := v.machine.Resume(); err != nil {
			debug.Log("vz", "resume before stop failed, forcing",
				"id", v.spec.ID, "err", err)
			graceful = false
		} else {
			v.state = machine.StateRunning
		}
	}

	debug.Log("vz", "stop requested", "id", v.spec.ID, "graceful", graceful)
	if graceful {
		if _, err := v.machine.RequestStop(); err == nil {
			if v.waitStopped(10) {
				debug.Log("vz", "graceful stop completed", "id", v.spec.ID)
				v.cleanupLocked()
				return nil
			}
			debug.Log("vz", "graceful stop timeout, forcing", "id", v.spec.ID)
		} else {
			debug.Log("vz", "RequestStop errored, forcing",
				"id", v.spec.ID, "err", err)
		}
	}
	// Force: cancel the run context; Code-Hex/vz does not expose Kill
	// directly, but closing the state watcher stops the goroutine and
	// releases resources. The VM state transitions to Stopped when the
	// guest halts; the goroutine exits regardless on ctx cancellation.
	if v.runCancel != nil {
		v.runCancel()
	}
	v.waitStopped(5)
	v.cleanupLocked()
	debug.Log("vz", "force stop completed", "id", v.spec.ID)
	return nil
}

func (v *vm) Destroy(ctx context.Context) error {
	debug.Log("vz", "destroy", "id", v.spec.ID)
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
	return v.state, nil
}

// VSock dials the guest on the given AF_VSOCK port. The guest must be
// listening; unreachable ports yield a timeout from the Virtualization
// framework.
//
// Connect is synchronous on the Go side (blocks on a C completion
// handler). We run it in a goroutine so ctx cancellation is honored; if
// ctx fires first and the connect later succeeds, we close the dangling
// connection to release the fd.
func (v *vm) VSock(ctx context.Context, port uint32) (net.Conn, error) {
	v.mu.Lock()
	dev := v.vsockDev
	state := v.state
	v.mu.Unlock()
	if state != machine.StateRunning {
		return nil, fmt.Errorf("%w: VSock requires running VM", machine.ErrInvalidState)
	}
	if dev == nil {
		return nil, machine.ErrVSockUnsupported
	}
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := dev.Connect(port)
		ch <- result{c, err}
	}()
	select {
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.conn != nil {
				_ = r.conn.Close()
			}
		}()
		return nil, ctx.Err()
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("vz: vsock connect port=%d: %w", port, r.err)
		}
		return r.conn, nil
	}
}

// VSockListen accepts vsock connections initiated by the guest on the
// given port. Returns ErrVSockUnsupported if the device wasn't
// configured. The returned listener is owned by the caller; close it
// to free the host-side endpoint.
//
// The Code-Hex/vz wrapper's *VirtioSocketListener already satisfies
// net.Listener (Accept, Addr, Close), so we return it directly.
func (v *vm) VSockListen(_ context.Context, port uint32) (net.Listener, error) {
	v.mu.Lock()
	dev := v.vsockDev
	state := v.state
	v.mu.Unlock()
	if state != machine.StateRunning {
		return nil, fmt.Errorf("%w: VSockListen requires running VM", machine.ErrInvalidState)
	}
	if dev == nil {
		return nil, machine.ErrVSockUnsupported
	}
	l, err := dev.Listen(port)
	if err != nil {
		return nil, fmt.Errorf("vz: vsock listen port=%d: %w", port, err)
	}
	return l, nil
}

// Pause suspends the vCPUs and moves the Machine to StatePaused.
// Idempotent. Used by the wallclock-tick watchdog in clawk's daemon to
// nudge the guest's clock and network state after detecting that the
// host slept (which the macOS sleep notifier doesn't always report on
// standby), and by the daemon's control socket for `clawk pause`.
func (v *vm) Pause(_ context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.state != machine.StateRunning || v.machine == nil {
		return nil // idempotent: already paused, or nothing to pause
	}
	if err := v.machine.Pause(); err != nil {
		return fmt.Errorf("vz: pause: %w", err)
	}
	v.state = machine.StatePaused
	debug.Log("vz", "paused", "id", v.spec.ID)
	return nil
}

// Resume restarts the vCPUs after a Pause. Idempotent.
func (v *vm) Resume(_ context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.state != machine.StatePaused || v.machine == nil {
		return nil // idempotent: already running, or nothing to resume
	}
	if err := v.machine.Resume(); err != nil {
		return fmt.Errorf("vz: resume: %w", err)
	}
	v.state = machine.StateRunning
	debug.Log("vz", "resumed", "id", v.spec.ID)
	return nil
}

// Snapshot pauses the VM, writes state to dir/state.bin, and resumes.
// Requires macOS 14+ and a configuration that passes ValidateSaveRestoreSupport.
func (v *vm) Snapshot(_ context.Context, dir string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.state != machine.StateRunning || v.machine == nil {
		return fmt.Errorf("%w: snapshot requires running", machine.ErrInvalidState)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("vz: snapshot dir: %w", err)
	}
	if err := v.machine.Pause(); err != nil {
		return fmt.Errorf("vz: pausing: %w", err)
	}
	v.state = machine.StatePaused
	savePath := filepath.Join(dir, "state.bin")
	saveErr := v.machine.SaveMachineStateToPath(savePath)
	if resumeErr := v.machine.Resume(); resumeErr != nil {
		// Leave v.state = StatePaused: the wrapper's state must agree with
		// the framework's, or the state-gated Resume could never recover
		// the frozen guest afterwards.
		if saveErr == nil {
			saveErr = fmt.Errorf("vz: resume after snapshot: %w", resumeErr)
		}
	} else {
		v.state = machine.StateRunning
	}
	if saveErr != nil {
		return fmt.Errorf("vz: save state: %w", saveErr)
	}
	return nil
}

// Suspend hibernates the VM: pause (if running), save memory + device
// state to dir/state.bin, and stop WITHOUT resuming — the guest never
// executes past the save point, so the rootfs on disk stays frozen at
// exactly the saved moment and a later Restore of the pair is safe.
// On save failure the guest is resumed (when it was running) so the VM
// isn't left wedged.
func (v *vm) Suspend(_ context.Context, dir string) error {
	v.mu.Lock()
	if (v.state != machine.StateRunning && v.state != machine.StatePaused) || v.machine == nil {
		v.mu.Unlock()
		return fmt.Errorf("%w: suspend requires a running or paused VM", machine.ErrInvalidState)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		v.mu.Unlock()
		return fmt.Errorf("vz: suspend dir: %w", err)
	}
	wasRunning := v.state == machine.StateRunning
	if wasRunning {
		if err := v.machine.Pause(); err != nil {
			v.mu.Unlock()
			return fmt.Errorf("vz: pausing for suspend: %w", err)
		}
		v.state = machine.StatePaused
	}
	m := v.machine
	v.mu.Unlock()

	// The save writes the guest's entire memory image — minutes for a big
	// guest — so it runs without v.mu held: State probes, the lifecycle
	// endpoint, and the vsock dial guards keep answering (accurately, as
	// StatePaused) instead of blocking for the whole save.
	savePath := filepath.Join(dir, "state.bin")
	saveErr := m.SaveMachineStateToPath(savePath)

	v.mu.Lock()
	defer v.mu.Unlock()
	if saveErr != nil {
		if wasRunning && v.state == machine.StatePaused {
			if resumeErr := v.machine.Resume(); resumeErr == nil {
				v.state = machine.StateRunning
			}
		}
		return fmt.Errorf("vz: save state: %w", saveErr)
	}
	// A concurrent Stop can win the race while the lock was released: it
	// resumes the paused guest to shut it down, so the guest ran past the
	// save point and the state file is unsafe to ever restore.
	if v.state != machine.StatePaused {
		_ = os.Remove(savePath)
		return fmt.Errorf("%w: vm was stopped during suspend", machine.ErrInvalidState)
	}
	debug.Log("vz", "suspended", "id", v.spec.ID, "state_file", savePath)
	// Do NOT call the framework Stop() here: Apple's stop only completes on
	// a RUNNING VM, so invoking it on our paused guest blocks forever on
	// the C completion handler — with v.mu held, that deadlocks every
	// subsequent State() probe and wedges the daemon's shutdown loop (the
	// VM saved but never exits). Instead just tear down our side:
	// cleanupLocked cancels the watcher/serve goroutines and marks us
	// StateStopped, which is what the daemon's shutdown loop keys on. The
	// paused framework VM is abandoned and reaped when the daemon process
	// exits moments later — we never want it to run again anyway (that is
	// the whole point of suspend).
	v.cleanupLocked()
	return nil
}

func (v *vm) Restore(_ context.Context, dir string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.state == machine.StateRunning {
		return fmt.Errorf("%w: restore into running VM", machine.ErrInvalidState)
	}
	if v.machine == nil {
		return fmt.Errorf("%w: Restore before Create", machine.ErrInvalidState)
	}
	savePath := filepath.Join(dir, "state.bin")
	if err := v.machine.RestoreMachineStateFromURL(savePath); err != nil {
		return fmt.Errorf("vz: restore state: %w", err)
	}
	if err := v.machine.Resume(); err != nil {
		return fmt.Errorf("vz: resume after restore: %w", err)
	}
	// Same post-start wiring as Start: the socket device only becomes
	// addressable once the VM is live, and without it every VSock dial
	// (agent proxy, time sync, guest stats) would report unsupported.
	if devs := v.machine.SocketDevices(); len(devs) > 0 {
		v.vsockDev = devs[0]
	}
	v.runGoroutine()
	v.serveGoroutine()
	v.state = machine.StateRunning
	v.startBalloonController()
	return nil
}

// runGoroutine watches the VM's state notifications and converts them into a
// done channel. It exits when the VM reaches Stopped/Error or runCtx is done.
//
// Debug logging in this loop is load-bearing for freeze investigations:
// when the VM wedges, vzd.log's last "vz-state" line tells us whether
// the Virtualization framework still thinks the guest is running (i.e.
// the freeze is guest-internal — systemd or kernel) or whether vz itself
// saw a state change the userspace handlers missed.
func (v *vm) runGoroutine() {
	v.runCtx, v.runCancel = context.WithCancel(context.Background())
	v.runDone = make(chan error, 1)
	m := v.machine
	ctx := v.runCtx
	done := v.runDone
	id := v.spec.ID
	go func() {
		states := m.StateChangedNotify()
		for {
			select {
			case <-ctx.Done():
				debug.Log("vz", "run goroutine cancelled", "id", id, "err", ctx.Err())
				done <- ctx.Err()
				return
			case s := <-states:
				debug.Log("vz-state", "transition", "id", id, "state", s)
				// Mirror to the daemon's logger if one was attached
				// via AttachStateLogger. Done outside the lock — the
				// stateLogger is set once at attach time, so a stale
				// read just means we miss one transition during
				// startup, not a corruption hazard.
				v.mu.Lock()
				sl := v.stateLogger
				v.mu.Unlock()
				if sl != nil {
					sl.Logf("vm-state: %s", stateName(s))
				}
				switch s {
				case codevz.VirtualMachineStateStopped:
					done <- nil
					return
				case codevz.VirtualMachineStateError:
					done <- fmt.Errorf("vz: machine entered error state")
					return
				}
			}
		}
	}()
}

// stateLogger is the smallest interface that a *log.Logger satisfies
// for our purposes. Defined here so the vz package doesn't have to
// import log — keeps the dependency direction one-way (cli → vz, not
// the other).
type stateLogger interface {
	Logf(format string, args ...any)
}

// loggerWrap adapts the stdlib *log.Logger (which has Printf) to our
// Logf shape. Used by the AttachStateLogger callers in cli/.
type loggerWrap struct {
	printf func(format string, args ...any)
}

func (w loggerWrap) Logf(format string, args ...any) { w.printf(format, args...) }

// AttachStateLogger registers a logger that receives one line per
// observed VM state transition. Pass a function that takes (format,
// args) — typically (*log.Logger).Printf wrapped via this adapter.
//
// Idempotent: replacing a previously-attached logger is fine; the new
// one starts receiving transitions on the next state change.
func (v *vm) AttachStateLogger(printf func(format string, args ...any)) {
	v.mu.Lock()
	v.stateLogger = loggerWrap{printf: printf}
	v.mu.Unlock()
}

// stateName returns a short human-readable name for a vz state. The
// integer value is opaque, but the underlying String() (generated by
// stringer) embeds the constant prefix; we strip it for readability.
func stateName(s codevz.VirtualMachineState) string {
	const prefix = "VirtualMachineState"
	n := s.String()
	if len(n) > len(prefix) && n[:len(prefix)] == prefix {
		return n[len(prefix):]
	}
	return n
}

// serveGoroutine runs the UserMode stack's accept loop if one was started.
func (v *vm) serveGoroutine() {
	if v.stack == nil {
		return
	}
	v.serveCtx, v.serveCancel = context.WithCancel(context.Background())
	v.serveDone = make(chan error, 1)
	stack := v.stack
	ctx := v.serveCtx
	done := v.serveDone
	go func() { done <- stack.Serve(ctx) }()
}

// cleanupLocked must be called with v.mu held.
func (v *vm) cleanupLocked() {
	if v.pressureCancel != nil {
		v.pressureCancel()
		v.pressureCancel = nil
	}
	v.balloon = nil
	if v.serveCancel != nil {
		v.serveCancel()
		v.serveCancel = nil
	}
	if v.stack != nil {
		_ = v.stack.Close()
		v.stack = nil
	}
	if v.runCancel != nil {
		v.runCancel()
		v.runCancel = nil
	}
	for _, f := range v.keepAlive {
		_ = f.Close()
	}
	v.keepAlive = nil
	v.state = machine.StateStopped
}

func (v *vm) waitStopped(seconds int) bool {
	if v.runDone == nil {
		return true
	}
	t := timerSeconds(seconds)
	defer t.Stop()
	select {
	case <-v.runDone:
		return true
	case <-t.C:
		return false
	}
}

// defaultMaxVZDevices caps the number of virtio devices we let one VM
// configure. Apple's Virtualization.framework puts every device — each block
// disk, the NIC, the socket device, entropy, the memory balloon, the serial
// console, the audio device, and every virtio-fs share — on a single virtual
// PCIe root, and refuses to start the VM past an undocumented ceiling. The
// failure surfaces only as an opaque VZErrorInternal at start, with no hint
// that device count is the cause. 8 fixed devices + 26 shares = 34 is a
// confirmed failure in the field; 32 is the conservative cap. The true limit
// is undocumented and may vary by macOS version, so maxVZDevices() honours a
// CLAWK_MAX_VZ_DEVICES override for dialing it in without a rebuild.
const defaultMaxVZDevices = 32

// maxVZDevices returns the device ceiling, allowing CLAWK_MAX_VZ_DEVICES to
// override the default. A non-positive or unparseable value falls back to the
// default rather than disabling the guard.
func maxVZDevices() int {
	if v := os.Getenv("CLAWK_MAX_VZ_DEVICES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxVZDevices
}

// audioInputEnabled reports whether the microphone virtio-snd device is
// attached. On by default (voice dictation); opt out with
// CLAWK_AUDIO_INPUT=0. Consulted both at the attach site and by the
// device-count guard so the two never disagree about whether audio costs a
// PCIe slot.
func audioInputEnabled() bool { return os.Getenv("CLAWK_AUDIO_INPUT") != "0" }

// build assembles the vz.VirtualMachineConfiguration from v.spec. It also
// starts any UserMode network stack the Spec requires (the VM-side fd is
// attached directly to the vz net device).
func (v *vm) build() (_ *codevz.VirtualMachineConfiguration, _ *codevz.VirtualMachine, err error) {
	disk := v.spec.RootFS.(machine.RawDisk)

	bootloader, err := buildBootloader(v.spec.Boot)
	if err != nil {
		return nil, nil, err
	}

	// Boot the guest at its ceiling, not its baseline. The memory balloon
	// (configured below) starts deflated, so the guest sees the full ceiling
	// at boot; the balloon controller then inflates it down toward the
	// baseline once the guest reports it's idle. Booting at the ceiling is
	// what lets the guest burst up to MemoryMaxMiB at all — a virtio balloon
	// can only reclaim memory the guest was booted with, never add more.
	cfg, err := codevz.NewVirtualMachineConfiguration(bootloader, v.spec.VCPU,
		v.balloonCeilingBytes())
	if err != nil {
		return nil, nil, fmt.Errorf("vz: vm config: %w", err)
	}

	// Nested virtualization: only touched if requested. vz uses the
	// default GenericPlatformConfiguration otherwise, which is fine.
	if v.spec.NestedVirt {
		if !codevz.IsNestedVirtualizationSupported() {
			return nil, nil, fmt.Errorf(
				"vz: NestedVirt requested but this host doesn't support it " +
					"(requires macOS 15+ and M3-or-newer Apple Silicon)")
		}
		platform, err := codevz.NewGenericPlatformConfiguration()
		if err != nil {
			return nil, nil, fmt.Errorf("vz: platform config: %w", err)
		}
		if err := platform.SetNestedVirtualizationEnabled(true); err != nil {
			return nil, nil, fmt.Errorf("vz: enabling nested virt: %w", err)
		}
		cfg.SetPlatformVirtualMachineConfiguration(platform)
	}

	// Rootfs + additional disks.
	//
	// Caching=Cached + Sync=Full mirrors the documented fix from
	// crc-org/vfkit PR #76 for Apple-Silicon disk corruption
	// (lima-vm/lima #1957, QEMU #1997). The default
	// DiskImageCachingModeAutomatic mode lets macOS choose, and on
	// Apple Silicon that produced reproducible ext4 inode-checksum
	// failures and other corruption symptoms. Cached + Full keeps
	// every guest write in the host's page cache, with full fsync
	// on flush.
	var storage []codevz.StorageDeviceConfiguration
	rootAtt, err := codevz.NewDiskImageStorageDeviceAttachmentWithCacheAndSync(
		disk.Path, disk.ReadOnly,
		codevz.DiskImageCachingModeCached,
		codevz.DiskImageSynchronizationModeFull,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("vz: root disk attachment: %w", err)
	}
	rootBlk, err := codevz.NewVirtioBlockDeviceConfiguration(rootAtt)
	if err != nil {
		return nil, nil, fmt.Errorf("vz: root block device: %w", err)
	}
	storage = append(storage, rootBlk)
	for _, d := range v.spec.Disks {
		att, err := codevz.NewDiskImageStorageDeviceAttachmentWithCacheAndSync(
			d.Path, d.ReadOnly,
			codevz.DiskImageCachingModeCached,
			codevz.DiskImageSynchronizationModeFull,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("vz: disk attachment %s: %w", d.Path, err)
		}
		blk, err := codevz.NewVirtioBlockDeviceConfiguration(att)
		if err != nil {
			return nil, nil, fmt.Errorf("vz: block device %s: %w", d.Path, err)
		}
		storage = append(storage, blk)
	}
	cfg.SetStorageDevicesVirtualMachineConfiguration(storage)

	// Serial console to file if requested.
	//
	// Open APPEND mode (not truncate) so we keep prior-boot output
	// across guest-side reboots. With `panic=10 oops=panic` in our
	// kernel cmdline, a kernel oops auto-reboots the guest. If we
	// truncated on each VM start, the panic message would be wiped
	// the moment the post-reboot guest opens the console — we'd see
	// only the new boot's output, never the panic that caused it.
	// APPEND keeps the panic backtrace + the recovery boot in one
	// file; `clawk debug dump` collects it intact.
	if v.spec.Serial.LogPath != "" {
		f, ferr := os.OpenFile(v.spec.Serial.LogPath,
			os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if ferr != nil {
			return nil, nil, fmt.Errorf("vz: console file: %w", ferr)
		}
		rd, rerr := os.Open(os.DevNull)
		if rerr != nil {
			f.Close()
			return nil, nil, fmt.Errorf("vz: /dev/null: %w", rerr)
		}
		att, aerr := codevz.NewFileHandleSerialPortAttachment(rd, f)
		if aerr != nil {
			f.Close()
			rd.Close()
			return nil, nil, fmt.Errorf("vz: serial attachment: %w", aerr)
		}
		con, cerr := codevz.NewVirtioConsoleDeviceSerialPortConfiguration(att)
		if cerr != nil {
			f.Close()
			rd.Close()
			return nil, nil, fmt.Errorf("vz: console device: %w", cerr)
		}
		cfg.SetSerialPortsVirtualMachineConfiguration(
			[]*codevz.VirtioConsoleDeviceSerialPortConfiguration{con})
		// Code-Hex/vz passes f.Fd()/rd.Fd() by number without dup'ing —
		// retain the *os.Files so GC's runtime finalizer doesn't close
		// the fds out from under the VM.
		v.keepAlive = append(v.keepAlive, f, rd)
	}

	// Entropy: without this, /dev/urandom inside the guest blocks for 5-10s
	// at boot waiting for kernel entropy.
	rng, err := codevz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return nil, nil, fmt.Errorf("vz: entropy device: %w", err)
	}
	cfg.SetEntropyDevicesVirtualMachineConfiguration(
		[]*codevz.VirtioEntropyDeviceConfiguration{rng})

	// Memory balloon: lets the balloon controller (startBalloonController)
	// resize the guest at runtime — inflating to reclaim idle RAM toward the
	// baseline or under host memory pressure, deflating to grant burst up to
	// the ceiling on guest demand. Always present so the runtime device
	// exists; it boots fully deflated (guest sees the ceiling).
	balloon, err := codevz.NewVirtioTraditionalMemoryBalloonDeviceConfiguration()
	if err != nil {
		return nil, nil, fmt.Errorf("vz: memory balloon device: %w", err)
	}
	cfg.SetMemoryBalloonDevicesVirtualMachineConfiguration(
		[]codevz.MemoryBalloonDeviceConfiguration{balloon})

	// Audio input: on by default so voice dictation works out of the box;
	// opt out with CLAWK_AUDIO_INPUT=0. Adds a virtio-snd device with one
	// PCM stream backed by the host's default microphone (Apple's
	// VZVirtioSoundDeviceHostInputStreamConfiguration). The host process
	// carries the com.apple.security.device.audio-input entitlement (see
	// clawk.entitlements); macOS still prompts for mic access on first run,
	// and a denied mic just records silence rather than failing the VM.
	if audioInputEnabled() {
		snd, err := codevz.NewVirtioSoundDeviceConfiguration()
		if err != nil {
			return nil, nil, fmt.Errorf("vz: sound device: %w", err)
		}
		in, err := codevz.NewVirtioSoundDeviceHostInputStreamConfiguration()
		if err != nil {
			return nil, nil, fmt.Errorf("vz: sound input stream: %w", err)
		}
		snd.SetStreams(in)
		cfg.SetAudioDevicesVirtualMachineConfiguration(
			[]codevz.AudioDeviceConfiguration{snd})
	}

	// virtio-socket is always exposed; useful for future guest-agent.
	vsock, err := codevz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return nil, nil, fmt.Errorf("vz: vsock device: %w", err)
	}
	cfg.SetSocketDevicesVirtualMachineConfiguration(
		[]codevz.SocketDeviceConfiguration{vsock})

	// virtio-fs shares.
	if len(v.spec.Shares) > 0 {
		devs := make([]codevz.DirectorySharingDeviceConfiguration, 0, len(v.spec.Shares))
		for _, sh := range v.spec.Shares {
			shared, err := codevz.NewSharedDirectory(sh.HostPath, sh.ReadOnly)
			if err != nil {
				return nil, nil, fmt.Errorf("vz: shared dir %q: %w", sh.HostPath, err)
			}
			single, err := codevz.NewSingleDirectoryShare(shared)
			if err != nil {
				return nil, nil, fmt.Errorf("vz: single-dir share %q: %w", sh.HostPath, err)
			}
			fs, err := codevz.NewVirtioFileSystemDeviceConfiguration(sh.Tag)
			if err != nil {
				return nil, nil, fmt.Errorf("vz: virtio-fs %q: %w", sh.Tag, err)
			}
			fs.SetDirectoryShare(single)
			devs = append(devs, fs)
		}
		cfg.SetDirectorySharingDevicesVirtualMachineConfiguration(devs)
	}

	// Network: at most one NIC for now. First UserMode wins; if there's a
	// Unixgram and no UserMode, use that instead.
	var netAttach *os.File
	var netMAC string
	for _, n := range v.spec.Net {
		switch nn := n.(type) {
		case machine.UserMode:
			sockPath := filepath.Join(v.stateDir, "usermode.sock")
			stack, err := usermode.Start(usermode.Config{
				SockPath: sockPath,
				Forwards: nn.Forwards,
				Filter:   nn.Filter,
			})
			if err != nil {
				return nil, nil, fmt.Errorf("vz: usermode stack: %w", err)
			}
			v.stack = stack
			netAttach = stack.VMSocket
			netMAC = stack.GuestMAC
		case machine.Unixgram:
			// Caller already wired the VM-side fd somewhere; we can't
			// reach through a path easily here. For now: unsupported
			// unless the caller uses UserMode.
			return nil, nil, fmt.Errorf("%w: vz backend requires UserMode for now", machine.ErrUnsupportedSpec)
		}
		if netAttach != nil {
			break
		}
	}
	if netAttach != nil {
		att, err := codevz.NewFileHandleNetworkDeviceAttachment(netAttach)
		if err != nil {
			return nil, nil, fmt.Errorf("vz: net attachment: %w", err)
		}
		nic, err := codevz.NewVirtioNetworkDeviceConfiguration(att)
		if err != nil {
			return nil, nil, fmt.Errorf("vz: net device: %w", err)
		}
		hw, err := net.ParseMAC(netMAC)
		if err != nil {
			return nil, nil, fmt.Errorf("vz: parse mac %q: %w", netMAC, err)
		}
		mac, err := codevz.NewMACAddress(hw)
		if err != nil {
			return nil, nil, fmt.Errorf("vz: mac: %w", err)
		}
		nic.SetMACAddress(mac)
		cfg.SetNetworkDevicesVirtualMachineConfiguration(
			[]*codevz.VirtioNetworkDeviceConfiguration{nic})
		v.vmSocket = netAttach
	}

	// Preflight device-count guard. Every device configured above sits on the
	// VM's single virtual PCIe root; past an undocumented ceiling Apple's
	// Virtualization.framework fails VZVirtualMachine.start with an opaque
	// internal error that never mentions device count. cfg.Validate() below
	// checks per-device constraints but NOT this aggregate, so we tally what
	// we just attached and fail early with an actionable message.
	//
	// The tally mirrors the attach sites above: len(storage) block disks
	// (rootfs + v.spec.Disks), the always-on entropy + balloon + vsock (3),
	// the serial console when a log path is set, the audio device unless
	// disabled, the NIC when a network stack attached, and one virtio-fs
	// device per share.
	deviceCount := len(storage) + 3 + len(v.spec.Shares)
	if v.spec.Serial.LogPath != "" {
		deviceCount++
	}
	if audioInputEnabled() {
		deviceCount++
	}
	if netAttach != nil {
		deviceCount++
	}
	if limit := maxVZDevices(); deviceCount > limit {
		return nil, nil, fmt.Errorf(
			"vz: %d virtio devices (%d base + %d shares) exceeds the %d-device "+
				"ceiling Apple's Virtualization.framework enforces — the VM would "+
				"fail to start with an opaque internal error. Reduce shares: attach "+
				"fewer repos or split the sandbox, disable audio input "+
				"(CLAWK_AUDIO_INPUT=0), or raise CLAWK_MAX_VZ_DEVICES if your "+
				"host tolerates more.",
			deviceCount, deviceCount-len(v.spec.Shares), len(v.spec.Shares), limit)
	}

	ok, err := cfg.Validate()
	if err != nil || !ok {
		return nil, nil, fmt.Errorf("vz: config validation: %w", err)
	}

	m, err := codevz.NewVirtualMachine(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("vz: new virtual machine: %w", err)
	}
	return cfg, m, nil
}

// buildBootloader maps a machine.Boot to a Code-Hex/vz bootloader.
func buildBootloader(b machine.Boot) (codevz.BootLoader, error) {
	switch bb := b.(type) {
	case machine.DirectKernel:
		opts := []codevz.LinuxBootLoaderOption{codevz.WithCommandLine(bb.Cmdline)}
		if bb.Initrd != "" {
			// vz's WithInitrd stats the path; passing "" yields
			// "no such file or directory", so only attach when the
			// caller actually has an initrd.
			opts = append(opts, codevz.WithInitrd(bb.Initrd))
		}
		bl, err := codevz.NewLinuxBootLoader(bb.Vmlinux, opts...)
		if err != nil {
			return nil, fmt.Errorf("vz: linux bootloader: %w", err)
		}
		return bl, nil
	case machine.EFIBoot:
		if bb.StorePath == "" {
			return nil, fmt.Errorf("vz: EFIBoot.StorePath is required")
		}
		store, err := codevz.NewEFIVariableStore(bb.StorePath,
			codevz.WithCreatingEFIVariableStore())
		if err != nil {
			return nil, fmt.Errorf("vz: EFI variable store: %w", err)
		}
		bl, err := codevz.NewEFIBootLoader(codevz.WithEFIVariableStore(store))
		if err != nil {
			return nil, fmt.Errorf("vz: EFI bootloader: %w", err)
		}
		return bl, nil
	default:
		return nil, fmt.Errorf("vz: unsupported Boot %T", b)
	}
}

// Compile-time guard so sealed unions don't drift silently.
var _ = []machine.Net{machine.UserMode{}, machine.TAP{}, machine.Unixgram{}}
var _ = []machine.Boot{machine.DirectKernel{}, machine.EFIBoot{}}
var _ = errors.Is
