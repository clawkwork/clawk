package cli

import (
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/clawkwork/clawk/machine"
	"github.com/stretchr/testify/require"
)

// TestCollectSandboxShares_ConsolidatesWorktrees pins the worktree device
// consolidation: every managed (non-in-place) worktree collapses into a
// single WorkspaceShareTag device backed by store.WorktreeDir, distinct
// source repos still get one src_<repo> alias each (deduped across worktrees
// of the same repo), and in-place phases keep their own device.
func TestCollectSandboxShares_ConsolidatesWorktrees(t *testing.T) {
	withTempStore(t)

	sb := &config.Sandbox{
		Name: "box",
		Phases: []config.Phase{
			{Worktree: "/wt/box/proj", Repo: "/code/proj"},
			{Worktree: "/wt/box/proj2", Repo: "/code/proj2"},
			// Second worktree of the same repo — its src alias must dedupe.
			{Worktree: "/wt/box/proj-2", Repo: "/code/proj"},
			{Worktree: "/code/here", Repo: "/code/here", InPlace: true},
		},
	}

	byTag := map[string]machine.Share{}
	for _, s := range collectSandboxShares(sb) {
		if _, dup := byTag[s.Tag]; dup {
			t.Fatalf("duplicate virtio-fs tag %q — vz requires unique tags per VM", s.Tag)
		}
		byTag[s.Tag] = s
	}

	// Exactly one consolidated worktree device, backed by the sandbox's
	// worktree parent dir — the whole point of the change.
	ws, ok := byTag[sandbox.WorkspaceShareTag]
	require.True(t, ok, "consolidated workspace share missing")
	require.Equal(t, store.WorktreeDir("box"), ws.HostPath)

	// Managed worktrees no longer get their own devices; they ride the parent.
	require.NotContains(t, byTag, "proj")
	require.NotContains(t, byTag, "proj2")
	require.NotContains(t, byTag, "proj-2")

	// One src alias per DISTINCT managed repo; the in-place repo gets none.
	require.Contains(t, byTag, "src_proj")
	require.Contains(t, byTag, "src_proj2")
	require.NotContains(t, byTag, "src_here")

	// In-place phase keeps its own device at its own host path.
	here, ok := byTag["here"]
	require.True(t, ok, "in-place worktree device missing")
	require.Equal(t, "/code/here", here.HostPath)
}

// TestCollectSandboxShares_InPlaceOnly verifies an in-place-only sandbox gets
// no consolidated parent device — WorkspaceRoot stays a plain guest dir and
// each in-place phase keeps its own device.
func TestCollectSandboxShares_InPlaceOnly(t *testing.T) {
	withTempStore(t)

	sb := &config.Sandbox{
		Name:   "box",
		Phases: []config.Phase{{Worktree: "/code/here", Repo: "/code/here", InPlace: true}},
	}

	byTag := map[string]bool{}
	for _, s := range collectSandboxShares(sb) {
		byTag[s.Tag] = true
	}
	require.NotContains(t, byTag, sandbox.WorkspaceShareTag,
		"in-place-only sandbox needs no consolidated parent")
	require.Contains(t, byTag, "here")
}
