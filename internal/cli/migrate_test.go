package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/stretchr/testify/require"
)

// TestMigrateCmd_ReconcilesStaleRunning verifies `clawk migrate` heals a record
// that claims to be running but whose provider reports it stopped, then nests
// it — i.e. a stale "running" record doesn't wrongly block migration.
func TestMigrateCmd_ReconcilesStaleRunning(t *testing.T) {
	s, _ := setupTest(t) // mock provider reports "stopped" for sandboxes never started

	rec := &config.Sandbox{
		Name:     "stale",
		Provider: config.ProviderVZ,
		VMState:  config.VMStateRunning, // stale: not actually running
		Phases:   []config.Phase{{Worktree: "/w/stale", InPlace: true}},
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	require.NoError(t, err)
	flat := filepath.Join(s.RootDir(), "sandboxes", "stale.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(flat), 0o755))
	require.NoError(t, os.WriteFile(flat, data, 0o644))

	_, err = executeCommand("migrate")
	require.NoError(t, err, "migrate")

	_, statErr := os.Stat(flat)
	require.True(t, os.IsNotExist(statErr), "flat record should be gone after reconcile+nest, err=%v", statErr)
	nested := filepath.Join(s.RootDir(), "namespaces", config.DefaultNamespace, "sandboxes", "stale.json")
	_, err = os.Stat(nested)
	require.NoError(t, err, "record not nested after reconcile")
}
