package cli

// Phase 7: ticket-mode sandboxes don't participate in cwd inference,
// but when one shares a cwd with the cwd-mode sandbox we surface a
// stderr hint so the user knows the resolution wasn't unambiguous.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/stretchr/testify/require"
)

// resolvedTempDir is t.TempDir with symlinks resolved, so paths compare
// equal to what os.Getwd and EvalSymlinks-based production code see —
// on macOS the temp root sits behind the /var → /private/var symlink.
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return dir
}

func TestFindTicketSandboxesRootedAt(t *testing.T) {
	s, _ := setupTest(t)

	// Resolve symlinks up front and create the nested repo dir: the
	// production lookup EvalSymlinks-es both sides, and on macOS
	// t.TempDir() lives under the /var → /private/var symlink, so an
	// unresolved (or nonexistent, hence unresolvable) path silently
	// fails the comparison.
	dir := resolvedTempDir(t)
	repo := filepath.Join(dir, "api")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	other := filepath.Join(resolvedTempDir(t), "elsewhere")

	// here-mode sandbox at dir — must be ignored by the ticket lookup.
	hereName, err := deriveHereSandboxName(dir)
	require.NoError(t, err)
	s.Save(&config.Sandbox{
		Name: hereName,
		Phases: []config.Phase{{
			Repo: dir, Worktree: dir, InPlace: true,
			Status: config.PhaseStatusActive,
		}},
	})

	// Ticket sandbox rooted under dir (workspace-root case).
	s.Save(&config.Sandbox{
		Name: "TICKET-1",
		Phases: []config.Phase{{
			Repo: repo, Branch: "feat", Worktree: "/tmp/wt/api",
		}},
	})

	// Ticket sandbox with phase exactly at dir (standalone-Clawkfile).
	s.Save(&config.Sandbox{
		Name: "TICKET-EXACT",
		Phases: []config.Phase{{
			Repo: dir, Branch: "feat", Worktree: "/tmp/wt/x",
		}},
	})

	// Ticket sandbox unrelated to dir — must NOT match.
	s.Save(&config.Sandbox{
		Name: "TICKET-OTHER",
		Phases: []config.Phase{{
			Repo: other, Branch: "feat", Worktree: "/tmp/wt/y",
		}},
	})

	got := findTicketSandboxesRootedAt(dir)
	want := []string{"TICKET-1", "TICKET-EXACT"}
	require.Equal(t, strings.Join(want, ","), strings.Join(got, ","), "findTicketSandboxesRootedAt")
}

// TestRunVerbEmitsCwdShadowHint: when `clawk run` infers the sandbox
// from cwd and a ticket-mode sandbox is also rooted at cwd, the hint
// fires. Sandbox dispatch still resolves to the cwd-mode sandbox (the
// design says cwd-mode wins).
func TestRunVerbEmitsCwdShadowHint(t *testing.T) {
	s, mock := setupTest(t)

	// Resolved path required: cwd inference goes through os.Getwd,
	// which returns the physical (/private/var/...) path on macOS,
	// while t.TempDir reports the logical one. A sandbox saved under
	// the logical Worktree fails matchesHereSandbox and the derived
	// name picks up a collision suffix.
	dir := resolvedTempDir(t)
	t.Chdir(dir)

	hereName, err := deriveHereSandboxName(dir)
	require.NoError(t, err)
	s.Save(&config.Sandbox{
		Name:    hereName,
		VMState: config.VMStateRunning,
		Phases: []config.Phase{{
			Repo: dir, Worktree: dir, InPlace: true,
			Status: config.PhaseStatusActive,
		}},
	})
	mock.Running[hereName] = true

	s.Save(&config.Sandbox{
		Name:    "TICKET-1",
		VMState: config.VMStateStopped,
		Phases: []config.Phase{{
			Repo: dir, Branch: "feat", Worktree: "/tmp/wt",
		}},
	})

	out, err := executeCommand("run", "claude")
	require.NoError(t, err, "run claude failed")
	require.Contains(t, out, "hint:", "expected cwd-shadow hint")
	require.Contains(t, out, "TICKET-1", "expected hint to name TICKET-1")
}

// TestRunVerbExplicitNameSkipsHint: an explicit sandbox name on `clawk
// run` means the user already disambiguated — no shadow hint.
func TestRunVerbExplicitNameSkipsHint(t *testing.T) {
	s, mock := setupTest(t)

	dir := t.TempDir()
	t.Chdir(dir)

	hereName, err := deriveHereSandboxName(dir)
	require.NoError(t, err)
	s.Save(&config.Sandbox{
		Name:    hereName,
		VMState: config.VMStateRunning,
		Phases: []config.Phase{{
			Repo: dir, Worktree: dir, InPlace: true,
		}},
	})
	mock.Running[hereName] = true

	s.Save(&config.Sandbox{
		Name:    "TICKET-1",
		VMState: config.VMStateRunning,
		Phases: []config.Phase{{
			Repo: dir, Branch: "feat", Worktree: "/tmp/wt",
		}},
	})
	mock.Running["TICKET-1"] = true

	out, err := executeCommand("run", "claude", "TICKET-1")
	require.NoError(t, err, "run claude TICKET-1 failed")
	require.NotContains(t, out, "hint:", "explicit name should not emit shadow hint")
}
