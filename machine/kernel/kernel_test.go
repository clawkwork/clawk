package kernel

import (
	"archive/tar"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

// fakeArchive builds a kata-static-shaped tar.zst with the given members.
func fakeArchive(t *testing.T, members map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	require.NoError(t, err)
	tw := tar.NewWriter(zw)
	for name, body := range members {
		err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
		})
		require.NoError(t, err)
		_, err = tw.Write([]byte(body))
		require.NoError(t, err)
	}
	err = tw.Close()
	require.NoError(t, err)
	err = zw.Close()
	require.NoError(t, err)
	return buf.Bytes()
}

func serveArchive(t *testing.T, archive []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(archive)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetch(t *testing.T) {
	archive := fakeArchive(t, map[string]string{
		// kata-static archives prefix members with "./".
		"./opt/kata/share/kata-containers/vmlinux-6.18.15-186": "FAKE KERNEL",
		"./opt/kata/bin/kata-runtime":                          "not a kernel",
	})
	srv := serveArchive(t, archive)

	cache := t.TempDir()
	got, err := Fetch(context.Background(), Options{
		CacheDir: cache,
		Arch:     "arm64",
		URL:      srv.URL + "/kata-static.tar.zst",
	})
	require.NoError(t, err, "Fetch")
	body, err := os.ReadFile(got)
	require.NoError(t, err)
	require.Equal(t, "FAKE KERNEL", string(body), "extracted kernel mismatch")

	t.Run("cache hit needs no network", func(t *testing.T) {
		srv.Close()
		again, err := Fetch(context.Background(), Options{
			CacheDir: cache,
			Arch:     "arm64",
			URL:      srv.URL + "/kata-static.tar.zst",
		})
		require.NoError(t, err, "cached Fetch")
		require.Equal(t, got, again, "cached path mismatch")
	})
}

func TestFetchMissingMember(t *testing.T) {
	archive := fakeArchive(t, map[string]string{
		"./opt/kata/share/kata-containers/vmlinux-9.99.9-999": "NEWER KERNEL",
	})
	srv := serveArchive(t, archive)

	_, err := Fetch(context.Background(), Options{
		CacheDir: t.TempDir(),
		Arch:     "arm64",
		URL:      srv.URL + "/kata-static.tar.zst",
	})
	require.Error(t, err, "Fetch succeeded with missing member")
	// The error must name the candidates so a version bump is actionable.
	require.Contains(t, err.Error(), "vmlinux-9.99.9-999", "error doesn't list found vmlinux members")
}

func TestFetchHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	_, err := Fetch(context.Background(), Options{
		CacheDir: t.TempDir(),
		Arch:     "arm64",
		URL:      srv.URL + "/missing.tar.zst",
	})
	require.Error(t, err, "want HTTP 404 failure")
	require.Contains(t, err.Error(), "404", "want HTTP 404 in error")
}

func TestFetchValidation(t *testing.T) {
	_, err := Fetch(context.Background(), Options{Arch: "arm64"})
	require.Error(t, err, "Fetch without CacheDir succeeded")
	_, err = Fetch(context.Background(), Options{CacheDir: t.TempDir()})
	require.Error(t, err, "Fetch without Arch succeeded")
}

func TestCachedPath(t *testing.T) {
	cache := t.TempDir()
	opts := Options{CacheDir: cache, Arch: "arm64"}
	p, cached := CachedPath(opts)
	require.False(t, cached, "empty cache reported cached")
	err := os.MkdirAll(filepath.Dir(p), 0o755)
	require.NoError(t, err)
	err = os.WriteFile(p, []byte("k"), 0o644)
	require.NoError(t, err)
	p2, cached := CachedPath(opts)
	require.True(t, cached, "CachedPath not cached after writing")
	require.Equal(t, p, p2, "CachedPath returned different path")
}

// TestFetchOverrideLocalPath: a local vmlinux is returned as-is and
// reported cached; Arch is not required for an override.
func TestFetchOverrideLocalPath(t *testing.T) {
	vmlinux := filepath.Join(t.TempDir(), "vmlinux")
	err := os.WriteFile(vmlinux, []byte("kernel"), 0o644)
	require.NoError(t, err)
	got, err := Fetch(context.Background(), Options{CacheDir: t.TempDir(), Override: vmlinux})
	require.NoError(t, err, "Fetch")
	require.Equal(t, vmlinux, got, "Fetch override path mismatch")
	p, cached := CachedPath(Options{CacheDir: t.TempDir(), Override: vmlinux})
	require.True(t, cached, "CachedPath not cached for override")
	require.Equal(t, vmlinux, p, "CachedPath override path mismatch")
}

func TestFetchOverrideMissingLocalPath(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "absent")
	_, err := Fetch(context.Background(), Options{CacheDir: t.TempDir(), Override: absent})
	require.Error(t, err, "Fetch with missing override path succeeded")
}

// TestFetchOverrideURL downloads a raw vmlinux once; the second call
// revalidates against the origin (conditional GET) and, when the content is
// unchanged, serves the cache without re-downloading. See TestOverrideRevalidates
// for the republish/offline cases.
func TestFetchOverrideURL(t *testing.T) {
	downloads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		downloads++
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write([]byte("raw-vmlinux-bytes"))
	}))
	defer srv.Close()

	opts := Options{CacheDir: t.TempDir(), Override: srv.URL + "/vmlinux"}
	got, err := Fetch(context.Background(), opts)
	require.NoError(t, err, "Fetch")
	data, err := os.ReadFile(got)
	require.NoError(t, err)
	require.Equal(t, "raw-vmlinux-bytes", string(data), "downloaded content mismatch")
	p, cached := CachedPath(opts)
	require.True(t, cached, "CachedPath not cached after download")
	require.Equal(t, got, p, "CachedPath after download path mismatch")
	_, err = Fetch(context.Background(), opts)
	require.NoError(t, err, "second Fetch")
	require.Equal(t, 1, downloads, "downloaded %d times, want 1 (unchanged fetch should 304, not re-download)", downloads)
}
