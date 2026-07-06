package config

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeRecord(t *testing.T, p string, sb *Sandbox) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	data, err := json.MarshalIndent(sb, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(p, data, 0o644))
}

// writeFlat writes a legacy flat record at sandboxes/<name>.json.
func writeFlat(t *testing.T, s *Store, sb *Sandbox) {
	writeRecord(t, filepath.Join(s.baseDir, sb.Name+".json"), sb)
}

// writeInterim writes an interim record at sandboxes/<ns>/<name>.json.
func writeInterim(t *testing.T, s *Store, ns string, sb *Sandbox) {
	writeRecord(t, filepath.Join(s.baseDir, ns, sb.Name+".json"), sb)
}

func currentRecord(s *Store, ns, name string) string {
	return filepath.Join(s.rootDir, "namespaces", ns, "sandboxes", name+".json")
}

func TestMigrate_RecordsVersionOnEmptyStore(t *testing.T) {
	s := testStore(t)
	require.NoError(t, s.RunMigrations(io.Discard))
	require.Equal(t, currentSchemaVersion, s.SchemaVersion())
}

func TestMigrate_NormalizesProvider(t *testing.T) {
	s := testStore(t)
	writeFlat(t, s, &Sandbox{Name: "legacy", Provider: legacyProviderVFKit, VMState: VMStateStopped})

	require.NoError(t, s.RunMigrations(io.Discard))
	got, err := s.Load("legacy")
	require.NoError(t, err)
	require.Equal(t, ProviderVZ, got.Provider)
}

func TestMigrate_BackfillsAnchorAndNamespace(t *testing.T) {
	s := testStore(t)
	writeFlat(t, s, &Sandbox{
		Name: "proj", Provider: ProviderVZ, VMState: VMStateStopped,
		Phases: []Phase{{Worktree: "/work/proj", InPlace: true}},
	})

	require.NoError(t, s.RunMigrations(io.Discard))
	got, err := s.Load("proj")
	require.NoError(t, err, "Load")
	require.Equal(t, "/work/proj", got.Anchor)
	require.Equal(t, DefaultNamespace, got.Namespace)
}

func TestMigrate_NestsFlatRecord(t *testing.T) {
	s := testStore(t)
	const name = "clawkwork"
	writeFlat(t, s, &Sandbox{
		Name: name, Provider: ProviderVZ, VMState: VMStateStopped,
		Anchor: "/w/clawkwork", Namespace: DefaultNamespace,
		Phases: []Phase{{Worktree: "/w/clawkwork", InPlace: true}},
	})
	// Seed a flat VM-state dir with a marker so we can prove it moves.
	vmMarker := filepath.Join(s.rootDir, "vms", name, "disk.raw")
	writeRecord(t, vmMarker, &Sandbox{}) // any file; reuse helper to mkdir+write

	require.NoError(t, s.RunMigrations(io.Discard))
	require.True(t, statExists(currentRecord(s, "default", name)), "record not nested under namespaces/default")
	require.False(t, statExists(filepath.Join(s.baseDir, name+".json")), "flat record still present")
	require.True(t, statExists(filepath.Join(s.rootDir, "namespaces", "default", "vms", name, "disk.raw")), "VM-state dir not moved into the namespace")
	require.False(t, statExists(filepath.Join(s.rootDir, "vms", name)), "flat VM-state dir should be gone")
}

func TestMigrate_FromInterimLayout(t *testing.T) {
	s := testStore(t)
	writeInterim(t, s, "default", &Sandbox{
		Name: "web", Provider: ProviderVZ, Namespace: DefaultNamespace, VMState: VMStateStopped,
	})

	require.NoError(t, s.RunMigrations(io.Discard))
	require.True(t, statExists(currentRecord(s, "default", "web")), "interim record not moved to namespace-first layout")
	require.False(t, statExists(filepath.Join(s.baseDir, "default", "web.json")), "interim record still present")
}

func TestMigrate_DefersRunningSandbox(t *testing.T) {
	s := testStore(t)
	writeFlat(t, s, &Sandbox{
		Name: "live", Provider: ProviderVZ, VMState: VMStateRunning,
		Anchor: "/w/live", Namespace: DefaultNamespace,
		Phases: []Phase{{Worktree: "/w/live", InPlace: true}},
	})

	var out bytes.Buffer
	require.NoError(t, s.RunMigrations(&out))
	require.False(t, statExists(currentRecord(s, "default", "live")), "a running sandbox must not be moved")
	require.Less(t, s.SchemaVersion(), currentSchemaVersion, "schema version should be < current (nesting deferred)")
	// The deferral must be reported (and name the sandbox) — not silent.
	require.True(t, strings.Contains(out.String(), "live"), "deferral notice should name the sandbox, got: %q", out.String())
}

func TestMigrate_Idempotent(t *testing.T) {
	s := testStore(t)
	writeFlat(t, s, &Sandbox{Name: "a", Provider: ProviderVZ, VMState: VMStateStopped, Namespace: DefaultNamespace})

	require.NoError(t, s.RunMigrations(io.Discard))
	v := s.SchemaVersion()
	before, err := os.ReadFile(s.recordPath("a"))
	require.NoError(t, err)

	require.NoError(t, s.RunMigrations(io.Discard))
	require.Equal(t, v, s.SchemaVersion())
	after, err := os.ReadFile(s.recordPath("a"))
	require.NoError(t, err)
	require.True(t, bytes.Equal(before, after), "second run rewrote the record")
}
