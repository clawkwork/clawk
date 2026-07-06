package oci

import (
	"archive/tar"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/cache"
	"github.com/stretchr/testify/require"
)

// sampleLayer is a small uncompressed layer used by the cache unit tests.
func sampleLayer(t *testing.T) (v1.Layer, v1.Hash) {
	t.Helper()
	layer := layerFromEntries(t, []layerEntry{
		{name: "etc/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "etc/hello", typeflag: tar.TypeReg, mode: 0o644, body: "hello world\n"},
	})
	diffID, err := layer.DiffID()
	require.NoError(t, err)
	return layer, diffID
}

// drain reads rc to completion and reports the Close error — the point at
// which the cache verifies and publishes.
func drain(t *testing.T, rc io.ReadCloser) error {
	t.Helper()
	_, err := io.Copy(io.Discard, rc)
	require.NoError(t, err, "draining cached blob")
	return rc.Close()
}

// TestVerifyingCacheRoundTrip: a fully consumed blob is published with its
// verified marker and read back by Get.
func TestVerifyingCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := newVerifyingCache(dir)
	layer, diffID := sampleLayer(t)

	_, err := c.Get(diffID)
	require.ErrorIs(t, err, cache.ErrNotFound, "Get on empty cache")

	cl, err := c.Put(layer)
	require.NoError(t, err)
	rc, err := cl.Uncompressed()
	require.NoError(t, err)
	err = drain(t, rc)
	require.NoError(t, err, "publishing blob")

	blob := cachepath(dir, diffID)
	_, err = os.Stat(blob)
	require.NoError(t, err, "blob not published")
	_, err = os.Stat(blob + verifiedSuffix)
	require.NoError(t, err, "verified marker not written")
	_, err = c.Get(diffID)
	require.NoError(t, err, "Get after publish")
	// Nothing left behind but the blob and its verified marker — no stray
	// temp file from the atomic publish.
	want := map[string]bool{
		filepath.Base(blob):                  true,
		filepath.Base(blob) + verifiedSuffix: true,
	}
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		require.True(t, want[e.Name()], "unexpected leftover file: %s", e.Name())
	}
}

// TestVerifyingCacheInterruptedWrite: a consumer that stops before EOF (a
// Ctrl-C, a cancelled context) must not publish a partial blob.
func TestVerifyingCacheInterruptedWrite(t *testing.T) {
	dir := t.TempDir()
	c := newVerifyingCache(dir)
	layer, diffID := sampleLayer(t)

	cl, err := c.Put(layer)
	require.NoError(t, err)
	rc, err := cl.Uncompressed()
	require.NoError(t, err)
	// Read a single byte — far short of the multi-block tar — then bail.
	_, err = rc.Read(make([]byte, 1))
	require.NoError(t, err, "partial read")
	err = rc.Close()
	require.NoError(t, err, "Close after partial read should be nil (clean abandon)")

	_, statErr := os.Stat(cachepath(dir, diffID))
	require.True(t, os.IsNotExist(statErr), "partial blob published (err=%v); an interrupted pull must leave no entry", statErr)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries, "interrupted write left files behind: %v", names(entries))
}

// TestVerifyingCacheRejectsTruncatedEntry reproduces the failure an
// interrupted pull on an older clawk left behind: a truncated blob sitting at
// its content-addressed path with no verified marker. Get must detect the
// mismatch, drop the entry, and report it absent so the caller re-pulls.
func TestVerifyingCacheRejectsTruncatedEntry(t *testing.T) {
	dir := t.TempDir()
	c := newVerifyingCache(dir)
	_, diffID := sampleLayer(t)

	err := os.MkdirAll(dir, 0o700)
	require.NoError(t, err)
	blob := cachepath(dir, diffID)
	err = os.WriteFile(blob, []byte("truncated, wrong content"), 0o644)
	require.NoError(t, err)

	_, getErr := c.Get(diffID)
	require.ErrorIs(t, getErr, cache.ErrNotFound, "Get on corrupt unverified blob")
	_, statErr := os.Stat(blob)
	require.True(t, os.IsNotExist(statErr), "corrupt blob not dropped (err=%v)", statErr)
}

// TestVerifyingCacheAdoptsValidLegacyEntry: a correct blob written by an
// older cache (no marker) is verified once, marked, and kept — not
// re-downloaded.
func TestVerifyingCacheAdoptsValidLegacyEntry(t *testing.T) {
	dir := t.TempDir()
	c := newVerifyingCache(dir)
	layer, diffID := sampleLayer(t)

	rc, err := layer.Uncompressed()
	require.NoError(t, err)
	legacyData, err := io.ReadAll(rc)
	rc.Close()
	require.NoError(t, err)
	err = os.MkdirAll(dir, 0o700)
	require.NoError(t, err)
	blob := cachepath(dir, diffID)
	err = os.WriteFile(blob, legacyData, 0o644)
	require.NoError(t, err)

	_, err = c.Get(diffID)
	require.NoError(t, err, "Get on valid legacy blob")
	_, err = os.Stat(blob + verifiedSuffix)
	require.NoError(t, err, "valid legacy blob not adopted (marker missing)")
}

// TestVerifyingCacheRejectsHashMismatch: a complete blob whose content does
// not match the digest it is keyed by is rejected at Close, not published.
func TestVerifyingCacheRejectsHashMismatch(t *testing.T) {
	dir := t.TempDir()
	c := newVerifyingCache(dir)
	layer, realDiffID := sampleLayer(t)
	digest, err := layer.Digest()
	require.NoError(t, err)
	size, err := layer.Size()
	require.NoError(t, err)
	// Lie about the diffID: the streamed content will hash to realDiffID,
	// which won't match this bogus key.
	wrong := v1.Hash{Algorithm: "sha256", Hex: strings.Repeat("0", 64)}
	cl, err := c.Put(knownLayer{Layer: layer, digest: digest, diffID: wrong, size: size})
	require.NoError(t, err)
	rc, err := cl.Uncompressed()
	require.NoError(t, err)
	err = drain(t, rc)
	require.Error(t, err, "Close on hash-mismatched blob = nil, want verification error")
	_, statErr := os.Stat(cachepath(dir, wrong))
	require.True(t, os.IsNotExist(statErr), "mismatched blob published (err=%v)", statErr)
	_, statErr = os.Stat(cachepath(dir, realDiffID))
	require.True(t, os.IsNotExist(statErr), "blob published under its real digest despite the wrong key (err=%v)", statErr)
}

// TestBuildRecoversFromTruncatedLayer is the end-to-end regression for the
// reported failure: an interrupted pull leaves a truncated blob in the layer
// cache, and the next build used to die with "converting to ext4: ...
// unexpected EOF". The verifying cache must detect the corruption and
// transparently re-pull.
func TestBuildRecoversFromTruncatedLayer(t *testing.T) {
	ref := pushTestImage(t)
	cacheDir := t.TempDir()

	res, err := Build(context.Background(), Options{Ref: ref, CacheDir: cacheDir, MinSizeMiB: 64})
	require.NoError(t, err, "initial Build")

	// Simulate the interrupted pull: truncate every cached layer blob and
	// drop its verified marker, mimicking entries an older clawk left.
	layersDir := filepath.Join(cacheDir, "layers")
	entries, err := os.ReadDir(layersDir)
	require.NoError(t, err)
	corrupted := 0
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), verifiedSuffix) {
			continue
		}
		p := filepath.Join(layersDir, e.Name())
		info, err := e.Info()
		require.NoError(t, err)
		err = os.Truncate(p, info.Size()/2)
		require.NoError(t, err)
		_ = os.Remove(p + verifiedSuffix)
		corrupted++
	}
	require.NotZero(t, corrupted, "no layer blobs found to corrupt")

	// Force a disk rebuild so the next Build must re-flatten from the cache.
	err = os.RemoveAll(filepath.Dir(res.DiskPath))
	require.NoError(t, err)

	res2, err := Build(context.Background(), Options{Ref: ref, CacheDir: cacheDir, MinSizeMiB: 64})
	require.NoError(t, err, "rebuild after a truncated layer cache should recover")
	require.NotZero(t, res2.UnpackedBytes, "expected a real rebuild (re-pull + re-flatten), not a cache hit")
}

func names(entries []os.DirEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name()
	}
	return out
}
