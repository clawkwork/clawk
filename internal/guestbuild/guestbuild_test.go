package guestbuild

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBuild compiles all three guest binaries for real (network needed
// the first time for the guest modules' deps) and exercises the cache.
// This is the host-side equivalent of agentembed's compile test, plus
// the init module that only this package builds.
func TestBuild(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("`go` not on PATH")
	}
	if os.Getenv("AGENT_TEST_NO_BUILD") != "" {
		t.Skip("AGENT_TEST_NO_BUILD set")
	}

	cache := t.TempDir()
	bins, err := Build(context.Background(), cache, runtime.GOARCH)
	require.NoError(t, err, "Build")
	require.False(t, bins.Cached, "fresh build reported Cached")
	for _, p := range []string{bins.Init, bins.Agent, bins.TimeSync} {
		fi, err := os.Stat(p)
		require.NoError(t, err, "missing binary %s", p)
		if fi.Size() < 100*1024 {
			t.Errorf("%s suspiciously small (%d bytes)", filepath.Base(p), fi.Size())
		}
	}

	t.Run("cache hit", func(t *testing.T) {
		// Mark the binaries, rebuild, and check the marks survive — a
		// cache hit must not rewrite the files.
		before, err := os.Stat(bins.Init)
		require.NoError(t, err)
		again, err := Build(context.Background(), cache, runtime.GOARCH)
		require.NoError(t, err, "cached Build")
		require.Equal(t, bins.Init, again.Init, "cache key changed: %s != %s", again.Init, bins.Init)
		require.True(t, again.Cached, "second build did not report Cached")
		after, err := os.Stat(bins.Init)
		require.NoError(t, err)
		require.True(t, after.ModTime().Equal(before.ModTime()), "cache hit rewrote the binary")
	})

	t.Run("validation", func(t *testing.T) {
		_, err := Build(context.Background(), "", runtime.GOARCH)
		require.Error(t, err, "Build without cacheDir succeeded")
		_, err = Build(context.Background(), cache, "")
		require.Error(t, err, "Build without arch succeeded")
	})
}
