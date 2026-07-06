package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/stretchr/testify/require"
)

// shortTempDir returns a temp dir under a short base. The default t.TempDir()
// embeds the test name, and combined with the deep namespaces/<ns>/vms/<name>
// store layout that can push a control-socket path past the ~104-char
// unix-domain socket limit on macOS (bind: invalid argument).
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "clawk")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// setupTest replaces the package-level store and injects a mock provider
// via the testProvider hook.
func setupTest(t *testing.T) (*config.Store, *sandbox.MockProvider) {
	t.Helper()
	dir := shortTempDir(t)
	sandboxDir := filepath.Join(dir, "sandboxes")
	if err := os.MkdirAll(sandboxDir, 0o755); err != nil {
		t.Fatalf("creating test sandbox dir: %v", err)
	}

	s := config.NewStoreAt(dir)
	mock := sandbox.NewMockProvider()

	store = s
	testProvider = mock

	// Stub the host-RAM probe to a roomy fixed size so memory admission is
	// deterministic and never refuses a mock bring-up on a small CI/test host.
	hostMemoryProbe = func() (uint64, error) { return 64 << 30, nil }

	// Reset package-level flag state. Cobra persists flag values on the
	// command tree, so a previous test's `--only foo` would leak into the
	// next test's invocation unless we clear it here.
	t.Cleanup(func() {
		testProvider = nil
		hostMemoryProbe = hostPhysicalMemoryBytes
		runOnly = nil
		runName = ""
		runBare = false
		runProfile = ""
		runSafe = false
		providerFlag = ""
		worktreeAddName = ""
		statusJSON = false
		statusBrief = false
		forwardListJSON = false
		networkListJSON = false
		worktreeListJSON = false
		imageFlag = ""
		kernelFlag = ""
	})
	return s, mock
}

func executeCommand(args ...string) (string, error) {
	buf := new(bytes.Buffer)
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs(args)
	err := rootCmd.Execute()
	return buf.String(), err
}

func TestCreateAndList(t *testing.T) {
	setupTest(t)

	// Create
	_, err := executeCommand("create", "test-sb")
	require.NoError(t, err)

	// List
	out, err := executeCommand("list")
	require.NoError(t, err)
	if !strings.Contains(out, "test-sb") {
		// Output goes to stdout too, check store directly
		list, _ := store.List()
		if len(list) != 1 || list[0].Name != "test-sb" {
			t.Errorf("expected sandbox in list, got %v", list)
		}
	}
}

func TestCreateDuplicate(t *testing.T) {
	setupTest(t)

	executeCommand("create", "dup")
	_, err := executeCommand("create", "dup")
	require.Error(t, err)
}

func TestCreateInvalidName(t *testing.T) {
	setupTest(t)

	_, err := executeCommand("create", "bad name!")
	require.Error(t, err)
}

func TestUpDown(t *testing.T) {
	s, mock := setupTest(t)

	// Create sandbox with a phase (up requires phases)
	sb := &config.Sandbox{
		Name:    "vm-test",
		VMState: config.VMStateStopped,
		Phases:  []config.Phase{{Repo: "/tmp/r", Branch: "main", Status: config.PhaseStatusPending}},
	}
	s.Save(sb)

	// Up
	_, err := executeCommand("up", "vm-test")
	require.NoError(t, err)
	if len(mock.Created) != 1 || mock.Created[0] != "vm-test" {
		t.Errorf("expected Create called, got %v", mock.Created)
	}
	require.Len(t, mock.Started, 1, "expected Start called, got %v", mock.Started)

	// Verify persisted state
	loaded, _ := s.Load("vm-test")
	require.Equal(t, config.VMStateRunning, loaded.VMState)

	// Down
	_, err = executeCommand("down", "vm-test")
	require.NoError(t, err)
	require.Len(t, mock.Stopped, 1, "expected Stop called, got %v", mock.Stopped)

	loaded, _ = s.Load("vm-test")
	require.Equal(t, config.VMStateStopped, loaded.VMState)
}

func TestDestroyNonexistent(t *testing.T) {
	setupTest(t)
	_, err := executeCommand("destroy", "ghost")
	require.Error(t, err)
}

func TestStatusCommand(t *testing.T) {
	s, _ := setupTest(t)

	sb := &config.Sandbox{
		Name:    "status-test",
		VMState: config.VMStateStopped,
		Phases: []config.Phase{
			{Repo: "/tmp/r", Branch: "feat", Status: config.PhaseStatusPending, Worktree: "/tmp/wt/r"},
		},
	}
	s.Save(sb)

	_, err := executeCommand("status", "status-test")
	require.NoError(t, err)
}
