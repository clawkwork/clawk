package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/guestbuild"
	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/clawkwork/clawk/machine/oci"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/stretchr/testify/require"
)

// TestGCOCI exercises the OCI cache collector against a real (tiny)
// build: the entry a live sandbox would rebuild survives, stale digest
// dirs and stale guest-binary builds go, the layer cache goes only
// under --layers. Uses a docker-save tarball ref so the keep-set
// resolves without network.
func TestGCOCI(t *testing.T) {
	if testing.Short() {
		t.Skip("builds guest binaries")
	}
	ctx := context.Background()
	cache := t.TempDir()

	bins, err := guestbuild.Build(ctx, cache, runtime.GOARCH)
	require.NoError(t, err, "guestbuild")
	tarPath := writeTinyImageTarball(t)

	sb := config.Sandbox{Name: "box", Image: tarPath}
	res, err := oci.Build(ctx, oci.OptionsForImage(sandbox.OCIRootFS(&sb, cache, bins)))
	require.NoError(t, err, "oci.Build")
	liveEntry := filepath.Dir(res.DiskPath)

	staleEntry := filepath.Join(cache, "oci", "sha256_deadbeef_linux-arm64_8192m")
	mustMkdirWithFile(t, staleEntry, "disk.ext4")
	staleBins := filepath.Join(cache, "guestbin", "0123456789abcdef")
	mustMkdirWithFile(t, staleBins, "clawk-init")
	layers := filepath.Join(cache, "oci", "layers")
	_, err = os.Stat(layers)
	require.NoError(t, err, "expected layer cache from the build")

	t.Run("dry run removes nothing", func(t *testing.T) {
		removed, _ := gcOCI(io.Discard, io.Discard, cache, []config.Sandbox{sb}, true, false)
		require.Equal(t, 2, removed, "removed = %d, want 2 (stale entry + stale bins)", removed)
		for _, p := range []string{staleEntry, staleBins} {
			if _, err := os.Stat(p); err != nil {
				t.Errorf("dry run deleted %s", p)
			}
		}
	})

	t.Run("collects stale, keeps live", func(t *testing.T) {
		removed, reclaimed := gcOCI(io.Discard, io.Discard, cache, []config.Sandbox{sb}, false, false)
		if removed != 2 || reclaimed == 0 {
			t.Errorf("removed = %d reclaimed = %d", removed, reclaimed)
		}
		for _, p := range []string{staleEntry, staleBins} {
			if _, err := os.Stat(p); err == nil {
				t.Errorf("%s survived gc", p)
			}
		}
		for _, p := range []string{liveEntry, layers, filepath.Dir(bins.Init)} {
			if _, err := os.Stat(p); err != nil {
				t.Errorf("gc deleted live path %s: %v", p, err)
			}
		}
	})

	t.Run("no sandboxes removes everything", func(t *testing.T) {
		removed, _ := gcOCI(io.Discard, io.Discard, cache, nil, false, false)
		require.NotEqual(t, 0, removed, "expected the live entry to be collected once unreferenced")
		if _, err := os.Stat(liveEntry); err == nil {
			t.Errorf("unreferenced entry survived")
		}
	})

	t.Run("layers flag clears layer cache", func(t *testing.T) {
		gcOCI(io.Discard, io.Discard, cache, nil, false, true)
		if _, err := os.Stat(layers); err == nil {
			t.Error("layer cache survived --layers")
		}
	})
}

// writeTinyImageTarball produces a one-layer docker-save tarball.
func writeTinyImageTarball(t *testing.T) string {
	t.Helper()
	layer, err := random.Layer(64, "application/vnd.docker.image.rootfs.diff.tar.gzip")
	require.NoError(t, err)
	img, err := mutate.AppendLayers(empty.Image, layer)
	require.NoError(t, err)
	tag, err := name.NewTag("gc-test:latest")
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "gc-test.tar")
	require.NoError(t, tarball.WriteToFile(path, tag, img))
	return path
}

func mustMkdirWithFile(t *testing.T, dir, file string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, file), []byte("stale"), 0o644))
}
