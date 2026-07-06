package sandbox

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestToolchainCacheSharesEmptyCacheDir(t *testing.T) {
	if got := ToolchainCacheShares(""); got != nil {
		t.Fatalf("ToolchainCacheShares(\"\") = %v, want nil", got)
	}
}

func TestToolchainCacheShares(t *testing.T) {
	cacheDir := t.TempDir()

	want := []HostShare{
		{
			HostPath:  filepath.Join(cacheDir, "gomodcache"),
			Tag:       "go_modcache",
			GuestPath: GuestHome + "/go/pkg/mod",
		},
		// Go build cache (~/.cache/go-build) is deliberately NOT shared —
		// it stays per-VM (not concurrent-safe across builds, golang/go#43645,
		// and the heaviest virtio-fs fd churner). See ToolchainCacheShares.
		{
			HostPath:  filepath.Join(cacheDir, "cargo-registry-index"),
			Tag:       "cargo_registry_index",
			GuestPath: GuestHome + "/.cargo/registry/index",
		},
		{
			HostPath:  filepath.Join(cacheDir, "cargo-registry-cache"),
			Tag:       "cargo_registry_cache",
			GuestPath: GuestHome + "/.cargo/registry/cache",
		},
		{
			HostPath:  filepath.Join(cacheDir, "cargo-git-db"),
			Tag:       "cargo_git_db",
			GuestPath: GuestHome + "/.cargo/git/db",
		},
	}

	got := ToolchainCacheShares(cacheDir)
	require.Len(t, got, len(want))
	for i, w := range want {
		g := got[i]
		if g.HostPath != w.HostPath {
			t.Errorf("[%d] HostPath = %s, want %s", i, g.HostPath, w.HostPath)
		}
		if g.Tag != w.Tag {
			t.Errorf("[%d] Tag = %s, want %s", i, g.Tag, w.Tag)
		}
		if g.GuestPath != w.GuestPath {
			t.Errorf("[%d] GuestPath = %s, want %s", i, g.GuestPath, w.GuestPath)
		}
		if g.ReadOnly {
			t.Errorf("[%d] ReadOnly = true, want false (caches must be RW)", i)
		}
		// Host dir must exist — virtiofs refuses missing source paths.
		info, err := os.Stat(g.HostPath)
		if err != nil {
			t.Errorf("[%d] stat %s: %v", i, g.HostPath, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("[%d] %s exists but is not a directory", i, g.HostPath)
		}
	}
}

func TestToolchainCacheSharesIdempotent(t *testing.T) {
	cacheDir := t.TempDir()

	first := ToolchainCacheShares(cacheDir)
	second := ToolchainCacheShares(cacheDir)
	require.Len(t, first, len(second), "non-idempotent call lengths differ")
	for i := range first {
		if first[i] != second[i] {
			t.Errorf("[%d] mismatch on second call: %+v vs %+v",
				i, first[i], second[i])
		}
	}
}

func TestToolchainCacheSharesUniqueTags(t *testing.T) {
	// Tags must be unique across all shares mounted on a single VM —
	// virtiofs uses Tag as the mount identifier. Collisions with the
	// other share constructors here would cause the second mount to
	// fail at boot.
	cacheDir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateRoot := t.TempDir()

	all := append([]HostShare{}, DefaultHostShares()...)
	all = append(all, PersistentClaudeShares(stateRoot)...)
	all = append(all, ToolchainCacheShares(cacheDir)...)

	seen := make(map[string]string, len(all))
	for _, sh := range all {
		if prev, dup := seen[sh.Tag]; dup {
			t.Errorf("duplicate tag %q on %s and %s", sh.Tag, prev, sh.HostPath)
			continue
		}
		seen[sh.Tag] = sh.HostPath
	}
}
