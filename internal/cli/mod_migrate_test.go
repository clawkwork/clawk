package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/clawkwork/clawk/internal/template"
	"github.com/stretchr/testify/require"
)

func TestModMigrateRewritesFlatClawkMod(t *testing.T) {
	setupTest(t)
	dir := t.TempDir()
	path := filepath.Join(dir, template.RepoFileName)
	require.NoError(t, os.WriteFile(path,
		[]byte("name demo\n\nnetwork (\n    allow api.example.com\n)\n"), 0o644))

	out, err := executeCommand("mod", "migrate", dir)
	require.NoError(t, err, "mod migrate")
	require.Contains(t, out, "Migrated")

	src, err := os.ReadFile(path)
	require.NoError(t, err)
	f, err := template.ParseFileString(string(src))
	require.NoError(t, err, "migrated file must parse")
	require.Equal(t, "demo", f.Sandbox.SandboxName)
	require.Equal(t, []string{"api.example.com"}, f.Sandbox.Domains)

	// Idempotent: a second run is a no-op.
	out, err = executeCommand("mod", "migrate", dir)
	require.NoError(t, err)
	require.Contains(t, out, "nothing to do")
}

func TestModMigrateRenamesClawkWork(t *testing.T) {
	setupTest(t)
	dir := t.TempDir()
	workPath := filepath.Join(dir, template.RetiredWorkspaceFileName)
	require.NoError(t, os.WriteFile(workPath,
		[]byte("name acme\n\nincludes (\n    ./api\n)\n"), 0o644))

	_, err := executeCommand("mod", "migrate", dir)
	require.NoError(t, err, "mod migrate")

	_, err = os.Stat(workPath)
	require.True(t, os.IsNotExist(err), "clawk.work should be removed")
	src, err := os.ReadFile(filepath.Join(dir, template.RepoFileName))
	require.NoError(t, err, "clawk.mod should exist")
	f, err := template.ParseFileString(string(src))
	require.NoError(t, err)
	require.Equal(t, "acme", f.Sandbox.SandboxName)
	require.Equal(t, []string{"./api"}, f.Sandbox.Includes)
}

func TestModMigrateRefusesConflictingPair(t *testing.T) {
	setupTest(t)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, template.RetiredWorkspaceFileName),
		[]byte("includes ( ./api )\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, template.RepoFileName),
		[]byte("sandbox ( )\n"), 0o644))

	_, err := executeCommand("mod", "migrate", dir)
	require.ErrorContains(t, err, "merge them into clawk.mod by hand")
}
