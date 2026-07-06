package cli

// Tests for the v2 `clawk worktree add|rebase` verbs. Phase 1 keeps
// `clawk branch ...` registered alongside; these tests exercise the
// new positional-repo signature and the rebase flow.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/stretchr/testify/require"
)

// TestWorktreeAddPositional: `clawk worktree add <sb> <repo>` accepts
// the repo as a positional and produces the same Phase shape that
// `clawk branch add --repo` does.
func TestWorktreeAddPositional(t *testing.T) {
	s, _ := setupTest(t)

	dir := t.TempDir()
	repo := filepath.Join(dir, "r")
	gitInit(t, repo)

	sb := &config.Sandbox{
		Name:    "wt-add",
		VMState: config.VMStateStopped,
	}
	require.NoError(t, s.Save(sb))

	_, err := executeCommand("worktree", "add", "wt-add", repo)
	require.NoError(t, err)

	loaded, _ := s.Load("wt-add")
	require.Len(t, loaded.Phases, 1)
	p := loaded.Phases[0]
	require.Equal(t, "wt-add", p.Branch)
	require.Equal(t, "r", filepath.Base(p.Repo))
	require.NotEmpty(t, p.Worktree, "worktree path empty")

	// Verify the branch actually got created in the repo.
	out, err := exec.Command("git", "-C", repo, "branch", "--list", "wt-add").Output()
	require.NoError(t, err)
	if !strings.Contains(string(out), "wt-add") {
		t.Errorf("branch wt-add missing in %s", repo)
	}
}

// TestWorktreeAddCustomName exercises the --name flag.
func TestWorktreeAddCustomName(t *testing.T) {
	s, _ := setupTest(t)

	dir := t.TempDir()
	repo := filepath.Join(dir, "r")
	gitInit(t, repo)

	sb := &config.Sandbox{Name: "wt-named", VMState: config.VMStateStopped}
	s.Save(sb)

	_, err := executeCommand("worktree", "add", "wt-named", repo,
		"--name", "feat/foo")
	require.NoError(t, err)

	loaded, _ := s.Load("wt-named")
	require.Equal(t, "feat/foo", loaded.Phases[0].Branch)
}

// TestWorktreeRebaseNoOpenWorktrees: rebase when nothing's open in the
// repo errors with a clear pointer rather than silently succeeding.
func TestWorktreeRebaseNoOpenWorktrees(t *testing.T) {
	s, _ := setupTest(t)

	dir := t.TempDir()
	repo := filepath.Join(dir, "r")
	gitInit(t, repo)

	// Sandbox with a single worktree, marked merged.
	sb := &config.Sandbox{
		Name:    "wt-norebase",
		VMState: config.VMStateStopped,
		Phases: []config.Phase{{
			Repo:     repo,
			Branch:   "wt-norebase",
			Status:   config.PhaseStatusMerged,
			Worktree: "/tmp/some-worktree",
		}},
	}
	s.Save(sb)

	_, err := executeCommand("worktree", "rebase", "wt-norebase", "r")
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "no open worktrees"))
}

// TestWorktreeRebaseUnknownRepo: passing a repo basename that doesn't
// match any worktree errors clearly.
func TestWorktreeRebaseUnknownRepo(t *testing.T) {
	s, _ := setupTest(t)

	sb := &config.Sandbox{Name: "wt-unknown", VMState: config.VMStateStopped}
	s.Save(sb)

	_, err := executeCommand("worktree", "rebase", "wt-unknown", "ghost-repo")
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "no worktree for repo"))
}

// TestWorktreeRebaseFlow: full rebase flow with a real git remote. A
// "remote" repo gets a commit on main; the local worktree branched
// before that commit gets rebased onto origin/main.
func TestWorktreeRebaseFlow(t *testing.T) {
	s, _ := setupTest(t)

	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.git")
	local := filepath.Join(dir, "local")

	// Bare remote.
	mustGit(t, "init", "--bare", "-b", "main", remote)

	// Local clone with an initial commit.
	mustGit(t, "init", "-b", "main", local)
	mustGit(t, "-C", local, "config", "user.email", "t@t")
	mustGit(t, "-C", local, "config", "user.name", "T")
	require.NoError(t, os.WriteFile(filepath.Join(local, "f"), []byte("v1\n"), 0o644))
	mustGit(t, "-C", local, "add", ".")
	mustGit(t, "-C", local, "commit", "-m", "v1")
	mustGit(t, "-C", local, "remote", "add", "origin", remote)
	mustGit(t, "-C", local, "push", "origin", "main")

	// Add a feature branch that touches a *different* file than main
	// will, so the rebase has commits to replay but no conflicts.
	mustGit(t, "-C", local, "checkout", "-b", "feat/follow-up")
	require.NoError(t, os.WriteFile(filepath.Join(local, "feature"), []byte("feat\n"), 0o644))
	mustGit(t, "-C", local, "add", "feature")
	mustGit(t, "-C", local, "commit", "-m", "feature work")
	mustGit(t, "-C", local, "checkout", "main")

	// Make a follow-up worktree pointing at the feature branch.
	wtDir := filepath.Join(dir, "wt")
	mustGit(t, "-C", local, "worktree", "add", wtDir, "feat/follow-up")

	// Advance origin/main with a commit on a different file so rebase
	// has work to do but doesn't conflict.
	require.NoError(t, os.WriteFile(filepath.Join(local, "f"), []byte("v2\n"), 0o644))
	mustGit(t, "-C", local, "commit", "-am", "v2 on main")
	mustGit(t, "-C", local, "push", "origin", "main")

	sb := &config.Sandbox{
		Name:    "wt-rebase",
		VMState: config.VMStateStopped,
		Phases: []config.Phase{{
			Repo:     local,
			Branch:   "feat/follow-up",
			Status:   config.PhaseStatusActive,
			Worktree: wtDir,
		}},
	}
	s.Save(sb)

	_, err := executeCommand("worktree", "rebase", "wt-rebase", filepath.Base(local))
	require.NoError(t, err)

	// After rebase, the feature branch should have the v2 main commit
	// in its ancestry. `git merge-base --is-ancestor origin/main HEAD`
	// (run inside the worktree) succeeds → exit 0.
	cmd := exec.Command("git", "-C", wtDir, "merge-base", "--is-ancestor", "origin/main", "HEAD")
	if err := cmd.Run(); err != nil {
		t.Errorf("origin/main not in HEAD ancestry after rebase: %v", err)
	}
}
