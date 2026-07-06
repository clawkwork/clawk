package cli

// Tests for `clawk attach` (the resume verb), the `clawk work` redirect to
// attach when a sandbox already exists, and the shared --safe opt-out.

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/stretchr/testify/require"
)

// TestAttachUnknownSandbox: attaching to a name with no record errors with a
// message pointing at the two creation verbs.
func TestAttachUnknownSandbox(t *testing.T) {
	setupTest(t)

	_, err := executeCommand("attach", "ghost")
	require.Error(t, err)
	require.Contains(t, err.Error(), "clawk work", "error should point at how to create a sandbox")
}

// TestAttachBootsStopped: attaching to an existing stopped sandbox boots it
// (mock Start called) and then attaches the default runner.
func TestAttachBootsStopped(t *testing.T) {
	s, mock := setupTest(t)
	sb := &config.Sandbox{
		Name:    "at-stopped",
		VMState: config.VMStateStopped,
		Phases:  []config.Phase{{Repo: "/tmp/r", Branch: "main", Worktree: "/tmp/wt/r"}},
	}
	require.NoError(t, s.Save(sb))

	_, err := executeCommand("attach", "at-stopped")
	require.NoError(t, err)
	require.Contains(t, mock.Started, "at-stopped", "stopped sandbox should be booted on attach")
}

// TestAttachRunningNoBoot: attaching to an already-running sandbox attaches
// without booting (runUpInline is a no-op when the VM is up).
func TestAttachRunningNoBoot(t *testing.T) {
	s, mock := setupTest(t)
	sb := &config.Sandbox{
		Name:    "at-running",
		VMState: config.VMStateRunning,
		Phases:  []config.Phase{{Repo: "/tmp/r", Branch: "main", Worktree: "/tmp/wt/r"}},
	}
	require.NoError(t, s.Save(sb))
	mock.Running["at-running"] = true

	_, err := executeCommand("attach", "at-running")
	require.NoError(t, err)
	require.Empty(t, mock.Started, "running sandbox should not be re-booted on attach")
}

// TestAttachNoWorktrees: a record with no phases can't boot to anything
// useful, so attach refuses with the same message `clawk up` uses.
func TestAttachNoWorktrees(t *testing.T) {
	s, _ := setupTest(t)
	sb := &config.Sandbox{Name: "at-empty", VMState: config.VMStateStopped}
	require.NoError(t, s.Save(sb))

	_, err := executeCommand("attach", "at-empty")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no worktrees")
}

// TestWorkRedirectsToAttachWhenExists: `clawk work <name>` for an existing
// sandbox attaches (booting it) and does NOT re-read a template or add phases,
// even from a directory with no clawk.mod. The absence of a
// "no clawk.mod" error is the load-bearing assertion.
func TestWorkRedirectsToAttachWhenExists(t *testing.T) {
	s, mock := setupTest(t)
	sb := &config.Sandbox{
		Name:    "wr-ticket",
		VMState: config.VMStateStopped,
		Phases:  []config.Phase{{Repo: "/tmp/r", Branch: "main", Worktree: "/tmp/wt/r"}},
	}
	require.NoError(t, s.Save(sb))

	// The "already exists" notice is a fmt.Printf to os.Stdout (not captured);
	// the captured stderr carries the detach hint, which only prints on the
	// attach path — so its presence confirms the redirect fired.
	out, err := executeCommand("work", "wr-ticket")
	require.NoError(t, err, "work on an existing sandbox must not resolve a template")
	require.Contains(t, out, "clawk attach wr-ticket", "should reach the attach detach-hint")
	require.Contains(t, mock.Started, "wr-ticket", "work should boot the existing sandbox")

	got, err := s.Load("wr-ticket")
	require.NoError(t, err)
	require.Len(t, got.Phases, 1, "work must not add phases to an existing sandbox")
}

// TestWorkHealsPhaselessRecord: a record with no phases is the residue of a
// create that failed before its first worktree landed. `clawk work` must NOT
// redirect to attach for it — it falls through to the create path, re-reads
// the template, and completes the phases.
func TestWorkHealsPhaselessRecord(t *testing.T) {
	s, _ := setupTest(t)

	dir := t.TempDir()
	repo := filepath.Join(dir, "repo-a")
	gitInit(t, repo)
	tmplPath := filepath.Join(dir, "clawk.mod")
	body := fmt.Sprintf("sandbox (\n    vm (\n        provider vz\n    )\n\n    includes (\n        %s\n    )\n)\n", repo)
	require.NoError(t, os.WriteFile(tmplPath, []byte(body), 0o644))

	// The residue: the record exists (saved before addPhases) with no phases.
	sb := &config.Sandbox{
		Name:     "TICKET-HEAL",
		Provider: config.ProviderVZ,
		VMState:  config.VMStateStopped,
	}
	require.NoError(t, s.Save(sb))

	_, err := executeCommand("work", tmplPath, "TICKET-HEAL", "--bare")
	require.NoError(t, err)

	got, err := s.Load("TICKET-HEAL")
	require.NoError(t, err)
	require.Len(t, got.Phases, 1, "re-running work must complete the interrupted create")
}

// TestWorkRedirectBareSkipsBoot: `--bare` on an existing sandbox reports it
// exists and skips the boot entirely.
func TestWorkRedirectBareSkipsBoot(t *testing.T) {
	s, mock := setupTest(t)
	sb := &config.Sandbox{
		Name:    "wr-bare",
		VMState: config.VMStateStopped,
		Phases:  []config.Phase{{Repo: "/tmp/r", Branch: "main", Worktree: "/tmp/wt/r"}},
	}
	require.NoError(t, s.Save(sb))

	_, err := executeCommand("work", "wr-bare", "--bare")
	require.NoError(t, err)
	require.Empty(t, mock.Started, "--bare must not boot the existing sandbox")
}

// TestApplySafeModeStripsDefaultArgs: --safe drops the runner's
// permission-bypass DefaultArgs; without it they pass through unchanged.
func TestApplySafeModeStripsDefaultArgs(t *testing.T) {
	setupTest(t)
	claude, err := agentByName("claude")
	require.NoError(t, err)
	require.NotEmpty(t, claude.DefaultArgs, "precondition: claude carries bypass args")

	runSafe = false
	require.Equal(t, claude.DefaultArgs, applySafeMode(claude).DefaultArgs,
		"without --safe, DefaultArgs are preserved")

	runSafe = true
	require.Empty(t, applySafeMode(claude).DefaultArgs,
		"--safe should strip the permission-bypass DefaultArgs")
}
