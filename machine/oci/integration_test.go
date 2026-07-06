package oci

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/clawkwork/clawk/machine"
	"github.com/stretchr/testify/require"
)

// TestBuildAlpine_Integration runs the full OCI pipeline end-to-end
// against a public registry. It pulls ~3 MiB, builds an ext4 disk, and
// validates the filesystem with dumpe2fs + debugfs (no loopback mount
// required, so it works unprivileged).
//
// Skipped under -short, when debugfs/dumpe2fs aren't installed, or if
// the registry can't be reached.
func TestBuildAlpine_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test (-short)")
	}
	requireTool(t, "debugfs")
	requireTool(t, "dumpe2fs")

	const ref = "docker.io/library/alpine:3.20"
	cache := t.TempDir()
	platform := "linux/" + runtime.GOARCH

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	t.Logf("pulling %s platform=%s into %s", ref, platform, cache)
	start := time.Now()
	res, err := Build(ctx, Options{
		Ref:      ref,
		CacheDir: cache,
		Platform: platform,
	})
	require.NoError(t, err, "Build")
	t.Logf("built %s in %s (digest=%s unpacked=%d bytes)",
		res.DiskPath, time.Since(start), res.Digest, res.UnpackedBytes)

	assertValidRootFS(t, res.DiskPath)

	t.Run("cache hit", func(t *testing.T) {
		start := time.Now()
		res2, err := Build(ctx, Options{
			Ref:      ref,
			CacheDir: cache,
			Platform: platform,
		})
		require.NoError(t, err, "cached Build")
		require.Equal(t, res.DiskPath, res2.DiskPath, "cache miss")
		d := time.Since(start)
		require.LessOrEqual(t, d, time.Second, "cache hit too slow: %s (expected <1s)", d)
	})

	t.Run("materialize produces independent disks", func(t *testing.T) {
		dst1 := filepath.Join(t.TempDir(), "disk1.raw")
		dst2 := filepath.Join(t.TempDir(), "disk2.raw")
		d1, err := Materialize(ctx, machine.OCIImage{Ref: ref, CacheDir: cache}, dst1)
		require.NoError(t, err, "materialize 1")
		d2, err := Materialize(ctx, machine.OCIImage{Ref: ref, CacheDir: cache}, dst2)
		require.NoError(t, err, "materialize 2")
		require.NotEqual(t, d1.Path, d2.Path, "Materialize returned the same path twice: %s", d1.Path)
		// Both clones must still be valid ext4.
		assertValidRootFS(t, d1.Path)
		assertValidRootFS(t, d2.Path)
	})
}

// assertValidRootFS runs dumpe2fs and a handful of debugfs queries against
// disk to prove it's an Alpine rootfs. Does not mount — everything works
// unprivileged.
func assertValidRootFS(t *testing.T, disk string) {
	t.Helper()

	info, err := os.Stat(disk)
	require.NoError(t, err, "stat %s", disk)
	require.GreaterOrEqual(t, info.Size(), int64(5*1024*1024), "%s suspiciously small: %d bytes", disk, info.Size())

	out, err := exec.Command("dumpe2fs", "-h", disk).CombinedOutput()
	require.NoError(t, err, "dumpe2fs %s:\n%s", disk, out)
	require.Contains(t, string(out), "Filesystem magic number:", "dumpe2fs output missing magic number; not ext4?\n%s", out)

	// Directory listing at root: expect standard POSIX layout.
	listing := debugfsRun(t, disk, "ls /")
	for _, want := range []string{"bin", "etc", "usr", "var", "sbin"} {
		if !strings.Contains(listing, want) {
			t.Errorf("%s: root listing missing %q:\n%s", disk, want, listing)
		}
	}

	// os-release identifies the distro — the highest-signal content check.
	osRelease := debugfsRun(t, disk, "cat /etc/os-release")
	if !strings.Contains(strings.ToLower(osRelease), "alpine") {
		t.Errorf("%s: /etc/os-release doesn't mention Alpine:\n%s", disk, osRelease)
	}
}

// debugfsRun executes a single debugfs command against disk and returns
// its stdout. debugfs is invoked with -R so it exits after one command
// instead of dropping to an interactive prompt.
func debugfsRun(t *testing.T, disk, cmd string) string {
	t.Helper()
	out, err := exec.Command("debugfs", "-R", cmd, disk).CombinedOutput()
	require.NoError(t, err, "debugfs -R %q %s:\n%s", cmd, disk, out)
	return string(out)
}

func requireTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("required tool %q not on PATH; skipping", name)
	}
}
