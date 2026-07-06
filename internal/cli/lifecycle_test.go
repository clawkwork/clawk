package cli

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/netfilter"
	"github.com/clawkwork/clawk/internal/vzdctl"
	"github.com/clawkwork/clawk/machine"
	"github.com/stretchr/testify/require"
)

func TestUp_RecordsDesiredRunning(t *testing.T) {
	s, _ := setupTest(t)
	sb := &config.Sandbox{
		Name: "u", Provider: config.ProviderFirecracker, VMState: config.VMStateStopped,
		Phases: []config.Phase{{Repo: "/r", Worktree: "/r", InPlace: true}},
	}
	require.NoError(t, s.Save(sb))
	_, err := executeCommand("up", "u")
	require.NoError(t, err)
	got, err := s.Load("u")
	require.NoError(t, err)
	require.Equal(t, config.VMStateRunning, got.DesiredState)
}

func TestDown_RecordsDesiredStopped(t *testing.T) {
	s, mock := setupTest(t)
	sb := &config.Sandbox{
		Name: "d", Provider: config.ProviderFirecracker, VMState: config.VMStateRunning,
		Phases: []config.Phase{{Repo: "/r", Worktree: "/r", InPlace: true}},
	}
	require.NoError(t, s.Save(sb))
	mock.Running["d"] = true
	_, err := executeCommand("down", "d")
	require.NoError(t, err)
	got, err := s.Load("d")
	require.NoError(t, err)
	require.Equal(t, config.VMStateStopped, got.DesiredState)
}

// TestUp_ClearsIdleStopReason: booting retires a recorded idle park even
// when DesiredState was already running (the idle stop leaves it running
// on purpose, so the reason field is the only thing `up` must touch).
func TestUp_ClearsIdleStopReason(t *testing.T) {
	s, _ := setupTest(t)
	sb := &config.Sandbox{
		Name: "ui", Provider: config.ProviderFirecracker,
		VMState: config.VMStateStopped, DesiredState: config.VMStateRunning,
		StopReason: config.StopReasonIdle,
		Phases:     []config.Phase{{Repo: "/r", Worktree: "/r", InPlace: true}},
	}
	require.NoError(t, s.Save(sb))
	_, err := executeCommand("up", "ui")
	require.NoError(t, err)
	got, err := s.Load("ui")
	require.NoError(t, err)
	require.Empty(t, got.StopReason, "up must retire the idle-stop annotation")
}

// TestDown_ClearsIdleStopReason: an explicit down owns the stop — a bare
// "stopped" with no reason means the user asked for it, even if the VM
// had previously been idle-parked.
func TestDown_ClearsIdleStopReason(t *testing.T) {
	s, mock := setupTest(t)
	sb := &config.Sandbox{
		Name: "di", Provider: config.ProviderFirecracker,
		VMState: config.VMStateRunning, StopReason: config.StopReasonIdle,
		Phases: []config.Phase{{Repo: "/r", Worktree: "/r", InPlace: true}},
	}
	require.NoError(t, s.Save(sb))
	mock.Running["di"] = true
	_, err := executeCommand("down", "di")
	require.NoError(t, err)
	got, err := s.Load("di")
	require.NoError(t, err)
	require.Empty(t, got.StopReason)
	require.Equal(t, config.VMStateStopped, got.DesiredState)
}

func TestObserve_ReconcilesStaleRunning(t *testing.T) {
	s, _ := setupTest(t) // mock reports "stopped" for sandboxes never started
	sb := &config.Sandbox{Name: "o", Provider: config.ProviderFirecracker, VMState: config.VMStateRunning}
	require.NoError(t, s.Save(sb))
	if observe(sb); sb.VMState != config.VMStateStopped {
		t.Errorf("observe should reconcile to stopped, got %q", sb.VMState)
	}
	got, err := s.Load("o")
	require.NoError(t, err)
	require.Equal(t, config.VMStateStopped, got.VMState, "reconcile not persisted")
}

func TestPause_NotRunningErrors(t *testing.T) {
	s, _ := setupTest(t)
	sb := &config.Sandbox{
		Name: "p", Provider: config.ProviderFirecracker, VMState: config.VMStateStopped,
		Phases: []config.Phase{{Repo: "/r", Worktree: "/r", InPlace: true}},
	}
	require.NoError(t, s.Save(sb))
	_, err := executeCommand("pause", "p")
	require.ErrorContains(t, err, "not running")
}

func TestSnapshot_NotRunningErrors(t *testing.T) {
	s, _ := setupTest(t)
	sb := &config.Sandbox{
		Name: "sn", Provider: config.ProviderFirecracker, VMState: config.VMStateStopped,
		Phases: []config.Phase{{Repo: "/r", Worktree: "/r", InPlace: true}},
	}
	require.NoError(t, s.Save(sb))
	_, err := executeCommand("snapshot", "sn")
	require.ErrorContains(t, err, "not running")
}

// TestResume_StoppedBootsFresh: resume on a plain stopped sandbox (no saved
// state) is just a boot, recorded as the user's desired state.
func TestResume_StoppedBootsFresh(t *testing.T) {
	s, mock := setupTest(t)
	sb := &config.Sandbox{
		Name: "rf", Provider: config.ProviderFirecracker, VMState: config.VMStateStopped,
		DesiredState: config.VMStateStopped,
		Phases:       []config.Phase{{Repo: "/r", Worktree: "/r", InPlace: true}},
	}
	require.NoError(t, s.Save(sb))
	out, err := executeCommand("resume", "rf")
	require.NoError(t, err)
	require.Contains(t, out, "no saved state")
	require.Contains(t, mock.Started, "rf")
	got, err := s.Load("rf")
	require.NoError(t, err)
	require.Equal(t, config.VMStateRunning, got.DesiredState)
}

// TestResume_SuspendedNarratesRestore: with a suspend state file on disk the
// resume verb narrates a restore (the daemon consumes the file at boot).
func TestResume_SuspendedNarratesRestore(t *testing.T) {
	s, mock := setupTest(t)
	sb := &config.Sandbox{
		Name: "rs", Provider: config.ProviderFirecracker, VMState: config.VMStateStopped,
		StopReason: config.StopReasonSuspended,
		Phases:     []config.Phase{{Repo: "/r", Worktree: "/r", InPlace: true}},
	}
	require.NoError(t, s.Save(sb))
	sDir := suspendStateDir(s.VMDir("rs"))
	require.NoError(t, os.MkdirAll(sDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sDir, "state.bin"), []byte("x"), 0o644))

	out, err := executeCommand("resume", "rs")
	require.NoError(t, err)
	require.Contains(t, out, "Restoring")
	require.Contains(t, mock.Started, "rs")
	got, err := s.Load("rs")
	require.NoError(t, err)
	require.Empty(t, got.StopReason, "boot must retire the suspended annotation")
}

// TestSnapshot_RevertsRecordWhenDaemonUnreachable: the record is written
// before the daemon is asked to suspend (so concurrent readers see the
// outcome, mirroring the idle park); a failed suspend must put it back.
func TestSnapshot_RevertsRecordWhenDaemonUnreachable(t *testing.T) {
	s, mock := setupTest(t)
	sb := &config.Sandbox{
		Name: "sr", Provider: config.ProviderFirecracker, VMState: config.VMStateRunning,
		DesiredState: config.VMStateRunning,
		Phases:       []config.Phase{{Repo: "/r", Worktree: "/r", InPlace: true}},
	}
	require.NoError(t, s.Save(sb))
	mock.Running["sr"] = true // provider says running, but no control socket exists

	_, err := executeCommand("snapshot", "sr")
	require.Error(t, err)
	got, err := s.Load("sr")
	require.NoError(t, err)
	require.Equal(t, config.VMStateRunning, got.VMState, "failed snapshot must not leave a stopped record")
	require.Equal(t, config.VMStateRunning, got.DesiredState)
	require.Empty(t, got.StopReason)
}

// startTestLifecycleDaemon serves a real control socket in the sandbox's VM
// dir, standing in for a live daemon's lifecycle surface.
func startTestLifecycleDaemon(t *testing.T, s *config.Store, name string, lh *vzdctl.LifecycleHandlers) {
	t.Helper()
	vmDir := s.VMDir(name)
	require.NoError(t, os.MkdirAll(vmDir, 0o755))
	srv, err := vzdctl.Start(vzdctl.SocketPath(vmDir), vzdctl.Handlers{
		Denials:   func() []netfilter.Denial { return nil },
		Reload:    func() error { return nil },
		Lifecycle: lh,
	})
	require.NoError(t, err)
	t.Cleanup(func() { srv.Close() })
}

// TestPauseAndResume_OverControlSocket drives the verbs against a real
// control socket: pause reaches the daemon handler, and resume on a
// paused-reporting daemon reaches the resume handler.
func TestPauseAndResume_OverControlSocket(t *testing.T) {
	s, mock := setupTest(t)
	sb := &config.Sandbox{
		Name: "pc", Provider: config.ProviderFirecracker, VMState: config.VMStateRunning,
		Phases: []config.Phase{{Repo: "/r", Worktree: "/r", InPlace: true}},
	}
	require.NoError(t, s.Save(sb))
	mock.Running["pc"] = true

	state := vzdctl.LifecycleRunning
	var paused, resumed int
	startTestLifecycleDaemon(t, s, "pc", &vzdctl.LifecycleHandlers{
		State:  func() vzdctl.LifecycleState { return vzdctl.LifecycleState{State: state} },
		Pause:  func() error { paused++; state = vzdctl.LifecyclePaused; return nil },
		Resume: func() error { resumed++; state = vzdctl.LifecycleRunning; return nil },
	})

	out, err := executeCommand("pause", "pc")
	require.NoError(t, err)
	require.Contains(t, out, "paused")
	require.Equal(t, 1, paused)

	out, err = executeCommand("resume", "pc")
	require.NoError(t, err)
	require.Contains(t, out, "resuming")
	require.Equal(t, 1, resumed)

	// A second resume finds the VM running and says so.
	out, err = executeCommand("resume", "pc")
	require.NoError(t, err)
	require.Contains(t, out, "already running")
	require.Equal(t, 1, resumed)
}

// TestUp_ResumesPausedVM: `up` means "make it usable" — on a paused VM that
// is a resume, not an "already running" shrug.
func TestUp_ResumesPausedVM(t *testing.T) {
	s, mock := setupTest(t)
	sb := &config.Sandbox{
		Name: "upp", Provider: config.ProviderFirecracker, VMState: config.VMStateRunning,
		DesiredState: config.VMStateRunning,
		Phases:       []config.Phase{{Repo: "/r", Worktree: "/r", InPlace: true}},
	}
	require.NoError(t, s.Save(sb))
	mock.Running["upp"] = true

	var resumed int
	startTestLifecycleDaemon(t, s, "upp", &vzdctl.LifecycleHandlers{
		State:  func() vzdctl.LifecycleState { return vzdctl.LifecycleState{State: vzdctl.LifecyclePaused} },
		Resume: func() error { resumed++; return nil },
	})

	out, err := executeCommand("up", "upp")
	require.NoError(t, err)
	require.Contains(t, out, "resuming")
	require.Equal(t, 1, resumed)
}

// TestSnapshot_RecordsSuspendedStop: the DAEMON owns the record transition
// (mirroring the idle park), so this drives the real vmLifecycle with a
// suspendable fake machine and asserts the record it writes.
func TestSnapshot_RecordsSuspendedStop(t *testing.T) {
	s, mock := setupTest(t)
	sb := &config.Sandbox{
		Name: "ss", Provider: config.ProviderFirecracker, VMState: config.VMStateRunning,
		DesiredState: config.VMStateRunning,
		Phases:       []config.Phase{{Repo: "/r", Worktree: "/r", InPlace: true}},
	}
	require.NoError(t, s.Save(sb))
	mock.Running["ss"] = true

	vmDir := s.VMDir("ss")
	require.NoError(t, os.MkdirAll(vmDir, 0o755))
	fm := &fakePauseableMachine{
		state: machine.StateRunning,
		// The daemon stops the VM as part of suspending; mirror that into
		// the mock provider so the verb's wait-for-exit succeeds.
		onSuspend: func() { delete(mock.Running, "ss") },
	}
	lc := newVMLifecycle("ss", vmDir, log.New(io.Discard, "", 0))
	lc.attach(fm, false)
	srv, err := vzdctl.Start(vzdctl.SocketPath(vmDir), vzdctl.Handlers{
		Denials:   func() []netfilter.Denial { return nil },
		Reload:    func() error { return nil },
		Lifecycle: lc.lifecycleHandlers(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { srv.Close() })

	// Completion narration goes through the progress tracker (stdout, like
	// `down`), so only the record is asserted here.
	_, err = executeCommand("snapshot", "ss")
	require.NoError(t, err)
	st, _ := fm.State(context.Background())
	require.Equal(t, machine.StateStopped, st, "suspend must stop the machine")
	got, err := s.Load("ss")
	require.NoError(t, err)
	require.Equal(t, config.VMStateStopped, got.VMState)
	require.Equal(t, config.VMStateStopped, got.DesiredState)
	require.Equal(t, config.StopReasonSuspended, got.StopReason)
}

// TestSnapshot_DaemonRevertsRecordOnFailedSave: a suspend whose save fails
// must leave the record exactly as it was — the daemon wrote the outcome
// optimistically and owns putting it back.
func TestSnapshot_DaemonRevertsRecordOnFailedSave(t *testing.T) {
	s, mock := setupTest(t)
	sb := &config.Sandbox{
		Name: "sf", Provider: config.ProviderFirecracker, VMState: config.VMStateRunning,
		DesiredState: config.VMStateRunning,
		Phases:       []config.Phase{{Repo: "/r", Worktree: "/r", InPlace: true}},
	}
	require.NoError(t, s.Save(sb))
	mock.Running["sf"] = true

	vmDir := s.VMDir("sf")
	require.NoError(t, os.MkdirAll(vmDir, 0o755))
	fm := &fakePauseableMachine{state: machine.StateRunning, suspendErr: errSaveFailed}
	lc := newVMLifecycle("sf", vmDir, log.New(io.Discard, "", 0))
	lc.attach(fm, false)
	srv, err := vzdctl.Start(vzdctl.SocketPath(vmDir), vzdctl.Handlers{
		Denials:   func() []netfilter.Denial { return nil },
		Reload:    func() error { return nil },
		Lifecycle: lc.lifecycleHandlers(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { srv.Close() })

	_, err = executeCommand("snapshot", "sf")
	require.Error(t, err)
	require.Contains(t, err.Error(), "save failed")
	got, err := s.Load("sf")
	require.NoError(t, err)
	require.Equal(t, config.VMStateRunning, got.VMState, "failed save must not leave a stopped record")
	require.Equal(t, config.VMStateRunning, got.DesiredState)
	require.Empty(t, got.StopReason)
}

// TestDown_DiscardsSuspendSnapshot: `down` promises a cold next boot, so it
// must throw away any suspend state that would otherwise surprise-restore.
func TestDown_DiscardsSuspendSnapshot(t *testing.T) {
	s, _ := setupTest(t)
	sb := &config.Sandbox{
		Name: "dd", Provider: config.ProviderFirecracker, VMState: config.VMStateStopped,
		StopReason: config.StopReasonSuspended,
		Phases:     []config.Phase{{Repo: "/r", Worktree: "/r", InPlace: true}},
	}
	require.NoError(t, s.Save(sb))
	sDir := suspendStateDir(s.VMDir("dd"))
	require.NoError(t, os.MkdirAll(sDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sDir, "state.bin"), []byte("x"), 0o644))

	_, err := executeCommand("down", "dd")
	require.NoError(t, err)
	require.False(t, hasSuspendState(sDir), "down must discard the suspend snapshot")
	got, err := s.Load("dd")
	require.NoError(t, err)
	require.Empty(t, got.StopReason)
}

// fakePauseableMachine is a minimal machine.Machine + Pauseable whose state
// transitions mirror the vz wrapper's, for driving the real vmLifecycle.
type fakePauseableMachine struct {
	mu    sync.Mutex
	state machine.State

	suspendErr error  // returned by Suspend when set
	onSuspend  func() // runs after a successful Suspend
}

func (f *fakePauseableMachine) Create(context.Context) error  { return nil }
func (f *fakePauseableMachine) Start(context.Context) error   { return nil }
func (f *fakePauseableMachine) Destroy(context.Context) error { return nil }
func (f *fakePauseableMachine) Stop(context.Context, bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = machine.StateStopped
	return nil
}
func (f *fakePauseableMachine) State(context.Context) (machine.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, nil
}
func (f *fakePauseableMachine) VSock(context.Context, uint32) (net.Conn, error) {
	return nil, machine.ErrVSockUnsupported
}
func (f *fakePauseableMachine) Pause(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == machine.StateRunning {
		f.state = machine.StatePaused
	}
	return nil
}
func (f *fakePauseableMachine) Resume(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == machine.StatePaused {
		f.state = machine.StateRunning
	}
	return nil
}

// errSaveFailed stands in for a backend save error in the suspend tests.
var errSaveFailed = errors.New("save failed")

// Suspend implements machine.Suspendable: stop-without-resume on success,
// stay put on failure — same contract as the real backends.
func (f *fakePauseableMachine) Suspend(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.suspendErr != nil {
		return f.suspendErr
	}
	f.state = machine.StateStopped
	if f.onSuspend != nil {
		f.onSuspend()
	}
	return nil
}

// TestPauseResume_EndToEndThroughVMLifecycle drives the verbs through the
// REAL daemon-side vmLifecycle (not faked handlers): pause must move the
// machine to StatePaused, the lifecycle endpoint must report it, and resume
// must move it back.
func TestPauseResume_EndToEndThroughVMLifecycle(t *testing.T) {
	s, mock := setupTest(t)
	sb := &config.Sandbox{
		Name: "e2e", Provider: config.ProviderFirecracker, VMState: config.VMStateRunning,
		Phases: []config.Phase{{Repo: "/r", Worktree: "/r", InPlace: true}},
	}
	require.NoError(t, s.Save(sb))
	mock.Running["e2e"] = true

	vmDir := s.VMDir("e2e")
	require.NoError(t, os.MkdirAll(vmDir, 0o755))
	fm := &fakePauseableMachine{state: machine.StateRunning}
	lc := newVMLifecycle("e2e", vmDir, log.New(io.Discard, "", 0))
	lc.attach(fm, false)
	srv, err := vzdctl.Start(vzdctl.SocketPath(vmDir), vzdctl.Handlers{
		Denials:   func() []netfilter.Denial { return nil },
		Reload:    func() error { return nil },
		Lifecycle: lc.lifecycleHandlers(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { srv.Close() })

	_, err = executeCommand("pause", "e2e")
	require.NoError(t, err)
	st, _ := fm.State(context.Background())
	require.Equal(t, machine.StatePaused, st, "pause must reach the machine")
	require.True(t, lc.isUserPaused(), "pause must be recorded as user-requested")

	ls, err := sandboxCtl(sb).Lifecycle(context.Background())
	require.NoError(t, err)
	require.Equal(t, vzdctl.LifecyclePaused, ls.State, "lifecycle endpoint must report paused")

	out, err := executeCommand("resume", "e2e")
	require.NoError(t, err)
	require.Contains(t, out, "resuming")
	st, _ = fm.State(context.Background())
	require.Equal(t, machine.StateRunning, st, "resume must reach the machine")
	require.False(t, lc.isUserPaused())
}

// TestResume_UnanswerableDaemonErrors: when the provider says running but
// the control socket can't be reached, resume must surface that instead of
// claiming "already running" — a paused-but-unqueriable VM hidden behind a
// reassuring message is undebuggable.
func TestResume_UnanswerableDaemonErrors(t *testing.T) {
	s, mock := setupTest(t)
	sb := &config.Sandbox{
		Name: "ru", Provider: config.ProviderFirecracker, VMState: config.VMStateRunning,
		Phases: []config.Phase{{Repo: "/r", Worktree: "/r", InPlace: true}},
	}
	require.NoError(t, s.Save(sb))
	mock.Running["ru"] = true // no control socket exists for this "daemon"

	_, err := executeCommand("resume", "ru")
	require.Error(t, err)
	require.ErrorIs(t, err, vzdctl.ErrNotRunning)
}

// A restore boot must reuse the suspended boot's disk; pairing the saved
// memory image with a freshly materialized rootfs corrupts the guest
// filesystem (the machine.Suspendable invariant).
func TestSuspendBootRootFS(t *testing.T) {
	quiet := log.New(io.Discard, "", 0)

	t.Run("no suspend state: keep the fresh spec", func(t *testing.T) {
		_, ok := suspendBootRootFS(t.TempDir(), "disk.raw", quiet)
		require.False(t, ok)
	})

	t.Run("state and disk present: boot the existing disk", func(t *testing.T) {
		vmDir := t.TempDir()
		require.NoError(t, os.MkdirAll(suspendStateDir(vmDir), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(suspendStateDir(vmDir), "state.bin"), []byte("x"), 0o644))
		disk := filepath.Join(vmDir, "disk.raw")
		require.NoError(t, os.WriteFile(disk, []byte("x"), 0o644))

		rootfs, ok := suspendBootRootFS(vmDir, "disk.raw", quiet)
		require.True(t, ok)
		require.Equal(t, machine.RawDisk{Path: disk}, rootfs)
	})

	t.Run("state without its disk: unrestorable, discarded", func(t *testing.T) {
		vmDir := t.TempDir()
		require.NoError(t, os.MkdirAll(suspendStateDir(vmDir), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(suspendStateDir(vmDir), "state.bin"), []byte("x"), 0o644))

		_, ok := suspendBootRootFS(vmDir, "disk.raw", quiet)
		require.False(t, ok)
		require.False(t, machine.SuspendStateExists(suspendStateDir(vmDir)),
			"a diskless suspend state must be discarded, not left to poison the next boot")
	})
}
