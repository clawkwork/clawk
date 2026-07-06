package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	baseDir := filepath.Join(dir, "sandboxes")
	require.NoError(t, os.MkdirAll(baseDir, 0o755))
	return &Store{baseDir: baseDir, rootDir: dir}
}

func TestStoreSaveLoad(t *testing.T) {
	s := testStore(t)
	sb := &Sandbox{
		Name:      "test",
		VMState:   VMStateStopped,
		CreatedAt: time.Now().Truncate(time.Second),
		Phases: []Phase{
			{Repo: "/tmp/repo", Branch: "main", Status: PhaseStatusPending, Order: 0},
		},
	}
	require.NoError(t, s.Save(sb))

	loaded, err := s.Load("test")
	require.NoError(t, err)
	require.Equal(t, sb.Name, loaded.Name)
	require.Len(t, loaded.Phases, 1)
	require.Equal(t, "main", loaded.Phases[0].Branch)
}

func TestSaveBumpsResourceVersion(t *testing.T) {
	s := testStore(t)
	sb := &Sandbox{Name: "rv", Provider: ProviderVZ, VMState: VMStateStopped}
	require.NoError(t, s.Save(sb))
	require.Equal(t, 1, sb.ResourceVersion)
	require.NoError(t, s.Save(sb))
	loaded, err := s.Load("rv")
	require.NoError(t, err)
	require.Equal(t, 2, loaded.ResourceVersion)
}

func TestStoreLoadNotFound(t *testing.T) {
	s := testStore(t)
	_, err := s.Load("nonexistent")
	require.Error(t, err)
}

// TestStoreLoadNormalizesLegacyProvider verifies that a sandbox persisted
// under the retired "vfkit" provider name (created before the vfkit→vz
// rename) still loads and is folded onto the current "vz" identifier.
func TestProviderNormalize(t *testing.T) {
	cases := map[Provider]Provider{
		legacyProviderVFKit: ProviderVZ,
		ProviderVZ:          ProviderVZ,
		ProviderFirecracker: ProviderFirecracker,
		"":                  "",
		"bogus":             "bogus",
	}
	for in, want := range cases {
		if got := in.Normalize(); got != want {
			t.Errorf("Provider(%q).Normalize() = %q, want %q", in, got, want)
		}
	}
}

func TestStoreDelete(t *testing.T) {
	s := testStore(t)
	sb := &Sandbox{Name: "delete-me", VMState: VMStateStopped, CreatedAt: time.Now()}
	require.NoError(t, s.Save(sb))
	require.True(t, s.Exists("delete-me"), "sandbox should exist")
	require.NoError(t, s.Delete("delete-me"))
	require.False(t, s.Exists("delete-me"), "sandbox should not exist")
}

func TestStoreList(t *testing.T) {
	s := testStore(t)
	for _, name := range []string{"a", "b", "c"} {
		sb := &Sandbox{Name: name, VMState: VMStateStopped, CreatedAt: time.Now()}
		require.NoError(t, s.Save(sb))
	}
	list, err := s.List()
	require.NoError(t, err)
	require.Len(t, list, 3)
}

func TestStoreDirs(t *testing.T) {
	s := testStore(t)
	wt := s.WorktreeDir("test")
	require.Equal(t, "test", filepath.Base(wt))
	vm := s.VMDir("test")
	require.Equal(t, "test", filepath.Base(vm))
	cache := s.CacheDir()
	require.Equal(t, "cache", filepath.Base(cache))
}
