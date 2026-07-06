package cli

// Phase 6 tests: derived PR state via gh. Tests use the pr.GHBin seam
// to stand in for `gh` (no GitHub round-trip, no auth needed). The
// shim is a tiny shell script that prints a canned JSON payload.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/pr"
	"github.com/stretchr/testify/require"
)

// setGHBin points pr.GHBin (the shared gh seam) at bin for one test.
func setGHBin(t *testing.T, bin string) {
	t.Helper()
	prev := pr.GHBin
	pr.GHBin = bin
	t.Cleanup(func() { pr.GHBin = prev })
}

// writeGHShim drops a tiny shell script that prints `payload` and
// exits 0, regardless of arguments. Mode 0755 so os/exec can run it.
func writeGHShim(t *testing.T, payload string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "gh-shim")
	body := "#!/bin/sh\ncat <<'EOF'\n" + payload + "\nEOF\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStatusRefreshesPRStateViaGH(t *testing.T) {
	s, _ := setupTest(t)

	dir := t.TempDir()
	repo := filepath.Join(dir, "r")
	gitInit(t, repo)
	wt := filepath.Join(dir, "wt")
	require.NoError(t, os.MkdirAll(wt, 0o755))

	// Phase starts as pending; gh shim says the PR is merged. After
	// `clawk status`, the cached Status should be Merged.
	sb := &config.Sandbox{
		Name:     "PR-1",
		Provider: config.ProviderVZ,
		VMState:  config.VMStateStopped,
		Phases: []config.Phase{
			{Repo: repo, Branch: "feat-x",
				Status:   config.PhaseStatusPending,
				Worktree: wt},
		},
	}
	s.Save(sb)

	shim := writeGHShim(t, `[{"number":7,"headRefName":"feat-x","state":"MERGED","url":"https://x/7"}]`)
	setGHBin(t, shim)

	_, err := executeCommand("status", "PR-1")
	require.NoError(t, err)
	got, _ := store.Load("PR-1")
	require.Equal(t, config.PhaseStatusMerged, got.Phases[0].Status)
	require.False(t, got.PRRefreshedAt.IsZero(), "PRRefreshedAt should be set after refresh")
}

func TestStatusUsesPRCacheWithinTTL(t *testing.T) {
	s, _ := setupTest(t)

	dir := t.TempDir()
	wt := filepath.Join(dir, "wt")
	sb := &config.Sandbox{
		Name:     "CACHED-1",
		Provider: config.ProviderVZ,
		VMState:  config.VMStateStopped,
		Phases: []config.Phase{
			{Repo: "/tmp/r", Branch: "feat-y",
				Status:   config.PhaseStatusActive,
				Worktree: wt},
		},
		PRRefreshedAt: time.Now().Add(-10 * time.Second),
	}
	s.Save(sb)

	// gh shim that would *flip* the value to merged. The cache is 60s
	// and this sandbox was refreshed 10s ago — so the shim must NOT
	// run, and the status must stay Active.
	shim := writeGHShim(t, `[{"number":1,"headRefName":"feat-y","state":"MERGED","url":""}]`)
	setGHBin(t, shim)

	_, err := executeCommand("status", "CACHED-1")
	require.NoError(t, err)
	got, _ := store.Load("CACHED-1")
	require.Equal(t, config.PhaseStatusActive, got.Phases[0].Status, "cache violated — status changed within TTL")
}

func TestStatusToleratesMissingGH(t *testing.T) {
	s, _ := setupTest(t)
	sb := &config.Sandbox{
		Name:     "no-gh",
		Provider: config.ProviderVZ,
		VMState:  config.VMStateStopped,
		Phases: []config.Phase{
			{Repo: "/tmp/r", Branch: "feat", Status: config.PhaseStatusPending,
				Worktree: "/tmp/wt"},
		},
	}
	s.Save(sb)

	// Point gh at a path that definitely doesn't exist. status must
	// still succeed — the dashboard renders cached values.
	setGHBin(t, "/does/not/exist/gh-binary")

	out, err := executeCommand("status", "no-gh")
	require.NoError(t, err)
	require.True(t, strings.Contains(out, "no-gh"), "expected dashboard to still render: %s", out)
}
