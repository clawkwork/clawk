package oci

import (
	"crypto/sha256"
	"crypto/sha512"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"runtime"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/cache"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/renameio/v2"
)

// verifiedSuffix names the companion marker written next to a blob once its
// content has been hashed and matched against its content-address. A blob
// without this marker is treated as unverified — it predates this cache, or
// a publish died between the rename and the marker — and is re-hashed (and
// dropped if corrupt) before it is trusted.
const verifiedSuffix = ".verified"

// verifyingCache is a content-addressed layer-blob cache that fixes two
// failure modes of go-containerregistry's default filesystem cache, both of
// which an interrupted pull triggered in practice: a partially written layer
// blob was left at its final path, passed the lazy Get check on the next
// build, and then failed mid-flatten with "unexpected EOF".
//
//   - Atomic publish. A blob is streamed to a temp file and renamed into its
//     content-addressed path only after the whole stream has been read and
//     verified. An interrupted write leaves at most a stray temp file, never
//     a truncated blob masquerading as a complete one.
//
//   - Content verification. The streamed bytes are hashed and compared to
//     the digest the blob is keyed by — the manifest's diffID for the
//     uncompressed blob, the layer digest for the compressed one — before the
//     rename. A mismatch fails the build loudly instead of poisoning the
//     cache. Get re-verifies any blob lacking a verified marker (entries from
//     an older clawk, or a half-finished publish) and drops the ones that no
//     longer hash correctly so the next build re-pulls them.
//
// It implements cache.Cache, so cache.Image drives it exactly like the
// upstream cache it replaces; the on-disk blob names match too, so an
// existing layer cache stays readable (its entries are verified on first use
// rather than re-downloaded wholesale).
type verifyingCache struct {
	path string
}

func newVerifyingCache(path string) *verifyingCache {
	return &verifyingCache{path: path}
}

func (c *verifyingCache) Put(l v1.Layer) (v1.Layer, error) {
	digest, err := l.Digest()
	if err != nil {
		return nil, err
	}
	diffID, err := l.DiffID()
	if err != nil {
		return nil, err
	}
	return &verifyingLayer{Layer: l, dir: c.path, digest: digest, diffID: diffID}, nil
}

func (c *verifyingCache) Get(h v1.Hash) (v1.Layer, error) {
	blob := cachepath(c.path, h)
	if _, err := os.Stat(blob); err != nil {
		if os.IsNotExist(err) {
			return nil, cache.ErrNotFound
		}
		return nil, err
	}
	// A blob is trustworthy only once its content has been matched against
	// its content-address. The marker records that. Without it — an older
	// cache, or a publish that died between the rename and the marker —
	// re-hash the blob: adopt it if it still verifies, drop it if it does
	// not so the caller re-pulls a clean copy.
	if _, err := os.Stat(blob + verifiedSuffix); err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		if verr := verifyFile(blob, h); verr != nil {
			if derr := c.Delete(h); derr != nil && !errors.Is(derr, cache.ErrNotFound) {
				return nil, derr
			}
			return nil, cache.ErrNotFound
		}
		markVerified(blob)
	}
	return tarball.LayerFromFile(blob)
}

func (c *verifyingCache) Delete(h v1.Hash) error {
	blob := cachepath(c.path, h)
	_ = os.Remove(blob + verifiedSuffix)
	err := os.Remove(blob)
	if os.IsNotExist(err) {
		return cache.ErrNotFound
	}
	return err
}

// verifyingLayer wraps a layer so that consuming its blob (Compressed or
// Uncompressed) writes a verified, atomically published copy into the cache —
// mirroring the lazy write-through of the upstream cache's layer.
type verifyingLayer struct {
	v1.Layer
	dir            string
	digest, diffID v1.Hash
}

func (l *verifyingLayer) Compressed() (io.ReadCloser, error) {
	rc, err := l.Layer.Compressed()
	if err != nil {
		return nil, err
	}
	return l.cacheThrough(rc, l.digest)
}

func (l *verifyingLayer) Uncompressed() (io.ReadCloser, error) {
	rc, err := l.Layer.Uncompressed()
	if err != nil {
		return nil, err
	}
	return l.cacheThrough(rc, l.diffID)
}

// cacheThrough returns a ReadCloser that yields src's bytes unchanged while
// teeing them into a temp file and a hasher. On Close it publishes the temp
// file to want's content-addressed path, but only if the whole stream was
// read and hashed to want; otherwise the temp file is discarded and the cache
// left untouched. Verification failures surface as the Close error.
func (l *verifyingLayer) cacheThrough(src io.ReadCloser, want v1.Hash) (io.ReadCloser, error) {
	h, err := hasherFor(want.Algorithm)
	if err != nil {
		src.Close()
		return nil, err
	}
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		src.Close()
		return nil, err
	}
	dest := cachepath(l.dir, want)
	// The temp file lives in the cache dir (WithTempDir) so the publishing
	// rename stays within one filesystem and renameio skips probing for an
	// alternate temp dir. Its leading-dot name keeps it clear of the
	// "sha256:..." blob names a Get looks up.
	pf, err := renameio.NewPendingFile(dest, renameio.WithTempDir(l.dir))
	if err != nil {
		src.Close()
		return nil, err
	}
	return &cacheWriter{
		src:  src,
		tee:  io.TeeReader(src, io.MultiWriter(pf, h)),
		pf:   pf,
		hash: h,
		want: want,
		dest: dest,
	}, nil
}

// cacheWriter tees a layer blob into a temp file as it is read, then on Close
// verifies and atomically publishes it.
type cacheWriter struct {
	src  io.ReadCloser
	tee  io.Reader
	pf   *renameio.PendingFile
	hash hash.Hash
	want v1.Hash
	dest string
	full bool // src reached EOF, so the temp file holds the whole blob
}

func (w *cacheWriter) Read(p []byte) (int, error) {
	n, err := w.tee.Read(p)
	if errors.Is(err, io.EOF) {
		w.full = true
	}
	return n, err
}

func (w *cacheWriter) Close() error {
	srcErr := w.src.Close()

	// Any path that does not publish must drop the temp file so a partial or
	// unverified blob never lands at the cache's content-addressed path.
	// Cleanup is a no-op once the file has been atomically published.
	switch {
	case srcErr != nil:
		// The download itself failed; never publish what we got.
		_ = w.pf.Cleanup()
		return srcErr
	case !w.full:
		// The consumer stopped before EOF — an interrupted flatten, a
		// cancelled context, a Ctrl-C. The temp file is incomplete: drop it.
		_ = w.pf.Cleanup()
		return nil
	}

	if got := fmt.Sprintf("%x", w.hash.Sum(nil)); got != w.want.Hex {
		_ = w.pf.Cleanup()
		return fmt.Errorf("oci cache: %s content hashed to %s:%s — discarding corrupt download",
			w.want, w.want.Algorithm, got)
	}
	// CloseAtomicallyReplace fsyncs the temp file before the rename, so a
	// crash right after publish can't leave a correctly named blob whose
	// bytes never reached stable storage.
	if err := w.pf.CloseAtomicallyReplace(); err != nil {
		_ = w.pf.Cleanup()
		return fmt.Errorf("oci cache: publishing %s: %w", w.want, err)
	}
	markVerified(w.dest)
	return nil
}

// verifyFile reports nil iff the file at p hashes to h.
func verifyFile(p string, h v1.Hash) error {
	hasher, err := hasherFor(h.Algorithm)
	if err != nil {
		return err
	}
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(hasher, f); err != nil {
		return err
	}
	if got := fmt.Sprintf("%x", hasher.Sum(nil)); got != h.Hex {
		return fmt.Errorf("oci cache: %s content hashed to %s:%s", h, h.Algorithm, got)
	}
	return nil
}

// markVerified records that the blob at blobPath has been hash-verified, so
// later Gets trust it without re-hashing. Best-effort: a failed marker write
// only costs a re-verification on the next build.
func markVerified(blobPath string) {
	if f, err := os.Create(blobPath + verifiedSuffix); err == nil {
		f.Close()
	}
}

// hasherFor returns a hasher for the named content-address algorithm. OCI
// content-addresses are sha256 in practice; sha512 is the only other
// algorithm the image spec registers.
func hasherFor(algorithm string) (hash.Hash, error) {
	switch algorithm {
	case "sha256":
		return sha256.New(), nil
	case "sha512":
		return sha512.New(), nil
	default:
		return nil, fmt.Errorf("oci cache: unsupported digest algorithm %q", algorithm)
	}
}

// cachepath names the on-disk blob for hash h, matching the layout
// go-containerregistry's filesystem cache used so an existing layer cache
// stays readable.
func cachepath(dir string, h v1.Hash) string {
	name := h.String()
	if runtime.GOOS == "windows" {
		name = fmt.Sprintf("%s-%s", h.Algorithm, h.Hex)
	}
	return filepath.Join(dir, name)
}
