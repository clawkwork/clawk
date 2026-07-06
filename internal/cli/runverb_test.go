package cli

// Tests for the v2 `clawk run <runner>` verb. Phase 1 keeps the legacy
// `clawk claude|codex|opencode|shell` verbs registered alongside, so
// these tests don't assert anything about the legacy verbs — they just
// exercise the new dispatch paths.

import (
	"strings"
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/stretchr/testify/require"
)

// TestRunVerbDispatchesAgent: `clawk run claude <sb>` against a
// running sandbox routes through runAgentSession. The mock provider
// has no agent socket, so dispatch falls through to the SSH/Exec
// branch — which is a no-op on the mock and returns nil.
func TestRunVerbDispatchesAgent(t *testing.T) {
	s, mock := setupTest(t)

	sb := &config.Sandbox{
		Name:    "rv-claude",
		VMState: config.VMStateRunning,
		Phases:  []config.Phase{{Repo: "/tmp/r", Branch: "main", Worktree: "/tmp/wt/r"}},
	}
	require.NoError(t, s.Save(sb))
	mock.Running["rv-claude"] = true

	_, err := executeCommand("run", "claude", "rv-claude")
	require.NoError(t, err)
}

// TestRunVerbDispatchesShell: `clawk run shell <sb>` opens an
// interactive shell. Mock implements ShellProvider so the fallback
// returns nil.
func TestRunVerbDispatchesShell(t *testing.T) {
	s, mock := setupTest(t)

	sb := &config.Sandbox{
		Name:    "rv-shell",
		VMState: config.VMStateRunning,
		Phases:  []config.Phase{{Repo: "/tmp/r", Branch: "main", Worktree: "/tmp/wt/r"}},
	}
	require.NoError(t, s.Save(sb))
	mock.Running["rv-shell"] = true

	_, err := executeCommand("run", "shell", "rv-shell")
	require.NoError(t, err)
}

func TestRunVerbMissingRunner(t *testing.T) {
	setupTest(t)
	_, err := executeCommand("run")
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "missing runner name"))
}

func TestRunVerbUnknownRunner(t *testing.T) {
	s, mock := setupTest(t)
	sb := &config.Sandbox{Name: "rv-unknown", VMState: config.VMStateRunning}
	s.Save(sb)
	mock.Running["rv-unknown"] = true

	_, err := executeCommand("run", "gibberish", "rv-unknown")
	require.Error(t, err)
	// agentByName produces "unknown agent <name> (have: ...)"
	require.True(t, strings.Contains(err.Error(), "unknown agent"))
}

func TestCodexRunnerBypassesApprovals(t *testing.T) {
	agent, err := agentByName("codex")
	require.NoError(t, err)
	require.True(t, len(agent.DefaultArgs) == 1 &&
		agent.DefaultArgs[0] == "--dangerously-bypass-approvals-and-sandbox",
		"codex DefaultArgs = %v", agent.DefaultArgs)
}

func TestRunVerbShellRejectsPassthroughArgs(t *testing.T) {
	s, mock := setupTest(t)
	sb := &config.Sandbox{Name: "rv-shellargs", VMState: config.VMStateRunning}
	s.Save(sb)
	mock.Running["rv-shellargs"] = true

	_, err := executeCommand("run", "shell", "rv-shellargs", "--", "echo", "hi")
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "passthrough"))
}

// TestRunVerbAutoBootsStopped: `clawk run claude <sb>` against a stopped
// sandbox boots it first (rather than refusing) and then attaches.
func TestRunVerbAutoBootsStopped(t *testing.T) {
	s, mock := setupTest(t)
	sb := &config.Sandbox{
		Name:    "rv-stopped",
		VMState: config.VMStateStopped,
		Phases:  []config.Phase{{Repo: "/tmp/r", Branch: "main", Worktree: "/tmp/wt/r"}},
	}
	require.NoError(t, s.Save(sb))

	out, err := executeCommand("run", "claude", "rv-stopped")
	require.NoError(t, err)
	require.Contains(t, mock.Started, "rv-stopped", "stopped sandbox should be booted, not refused")
	require.Contains(t, out, "booting it first")
}
