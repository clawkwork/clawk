package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	mustGit(t, "init", dir)
	mustGit(t, "-C", dir, "config", "user.email", "test@test.com")
	mustGit(t, "-C", dir, "config", "user.name", "Test")

	// Initial commit on main
	os.WriteFile(filepath.Join(dir, "file.txt"), []byte("v1"), 0o644)
	mustGit(t, "-C", dir, "add", ".")
	mustGit(t, "-C", dir, "commit", "-m", "init")

	// Create a feature branch
	mustGit(t, "-C", dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "file.txt"), []byte("v2"), 0o644)
	mustGit(t, "-C", dir, "add", ".")
	mustGit(t, "-C", dir, "commit", "-m", "feature change")
	mustGit(t, "-C", dir, "checkout", "master")

	return dir
}

func mustGit(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %s failed: %s", strings.Join(args, " "), out)
}

func TestAddAndRemove(t *testing.T) {
	repo := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")

	// Add worktree
	wtPath, err := Add(repo, "feature", wtBase, "")
	require.NoError(t, err)

	// Verify worktree exists and is on correct branch
	_, err = os.Stat(wtPath)
	require.NoError(t, err, "worktree dir not created")

	out, _ := exec.Command("git", "-C", wtPath, "branch", "--show-current").Output()
	branch := strings.TrimSpace(string(out))
	require.Equal(t, "feature", branch)

	// Check file content matches feature branch
	data, err := os.ReadFile(filepath.Join(wtPath, "file.txt"))
	require.NoError(t, err)
	require.Equal(t, "v2", string(data))

	// Remove worktree
	require.NoError(t, Remove(repo, wtPath))

	// Verify cleaned up
	_, err = os.Stat(wtPath)
	require.True(t, os.IsNotExist(err), "worktree dir should be removed")
}

func TestAddCreatesMissingBranch(t *testing.T) {
	repo := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")

	// Branch doesn't exist yet — Add should create it off HEAD.
	wt, err := Add(repo, "INFRA-1234", wtBase, "")
	require.NoError(t, err, "unexpected error")

	// Verify the new branch is now registered on the worktree.
	out, err := exec.Command("git", "-C", wt, "rev-parse", "--abbrev-ref", "HEAD").Output()
	require.NoError(t, err)
	require.Equal(t, "INFRA-1234", strings.TrimSpace(string(out)))
}

func TestAddNotARepo(t *testing.T) {
	_, err := Add(t.TempDir(), "main", t.TempDir(), "")
	require.Error(t, err, "expected error for non-repo directory")
}

// TestAddRecoversOrphanDir simulates the failure mode that surfaced as
// "fatal: '<path>' already exists" after a destroy/retry cycle: the
// directory survives at the destination but git's worktree bookkeeping
// has no record of it. Add should detect the orphan and clean it up
// transparently.
func TestAddRecoversOrphanDir(t *testing.T) {
	repo := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")

	wtPath := filepath.Join(wtBase, filepath.Base(repo))
	require.NoError(t, os.MkdirAll(wtPath, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(wtPath, "stale"), []byte("orphan"), 0o644))

	wt, err := Add(repo, "feature", wtBase, "")
	require.NoError(t, err, "Add should recover from orphan dir")
	require.Equal(t, wtPath, wt)
	out, _ := exec.Command("git", "-C", wt, "branch", "--show-current").Output()
	require.Equal(t, "feature", strings.TrimSpace(string(out)))
}

// TestAddRefusesLiveWorktree guards the safety side of the orphan
// recovery: if a real, registered worktree already lives at the target
// path, Add must surface the original error rather than silently nuking
// in-flight work.
func TestAddRefusesLiveWorktree(t *testing.T) {
	repo := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")

	_, err := Add(repo, "feature", wtBase, "")
	require.NoError(t, err)
	_, err = Add(repo, "feature", wtBase, "")
	require.Error(t, err, "Add should refuse to overwrite a live worktree at the same path")
}

func TestRemoveAll(t *testing.T) {
	repo := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")

	// Create feature2 branch from feature
	mustGit(t, "-C", repo, "checkout", "-b", "feature2")
	mustGit(t, "-C", repo, "checkout", "master")

	wt1, err := Add(repo, "feature", wtBase, "")
	require.NoError(t, err)
	wt2, err := Add(repo, "feature2", filepath.Join(wtBase, "second"), "")
	require.NoError(t, err)

	phases := []Phase{
		{Repo: repo, Branch: "feature", Worktree: wt1, Order: 0},
		{Repo: repo, Branch: "feature2", Worktree: wt2, Order: 1},
	}

	require.NoError(t, RemoveAll(phases, wtBase))

	_, err = os.Stat(wtBase)
	require.True(t, os.IsNotExist(err), "worktree base dir should be removed")
}
