// Package oci pulls OCI image references and materializes their merged
// layers as a bootable ext4 disk suitable for use as a [machine.RawDisk].
//
// The conversion is a single stream — registry layers are flattened with
// full OCI whiteout semantics (explicit and opaque; see flatten.go) and
// fed straight into a user-space ext4 writer — so no privileged mounts,
// no e2fsprogs, and no intermediate unpacked tree on disk. Ownership,
// setuid/setgid bits, xattrs and device nodes survive, which an
// unprivileged untar-to-directory pipeline silently loses.
//
// The pipeline is digest-keyed: calling Build twice with the same image
// digest reuses the cached disk without re-pulling. The cache layout is
// stable:
//
//	<CacheDir>/
//	  layers/                      # uncompressed layer blobs, shared across images
//	  <digest>_<platform>_<size>/  # one per (image, platform, size floor)
//	    disk.ext4                  # the built rootfs
//	    done                       # marker; absent until disk.ext4 is complete
//
// Layer blobs are content-addressed, so images that share base layers
// (e.g. several toolchains on the same debian base) download and store
// each layer once.
package oci

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/cache"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/clawkwork/clawk/machine"
	"github.com/clawkwork/clawk/machine/internal/cow"
	"github.com/clawkwork/clawk/machine/internal/ext4"
)

// Options control a Build.
type Options struct {
	// Ref is the OCI image reference (e.g. "docker.io/library/alpine:3.20").
	Ref string

	// CacheDir holds digest-keyed built disks and shared layer blobs.
	// Required.
	CacheDir string

	// MinSizeMiB is the minimum filesystem size in MiB; the gap between
	// content and this floor becomes free space the guest can write into
	// without resize2fs or growpart. Content larger than the floor grows
	// the filesystem past it. The padding is a sparse hole, so a large
	// floor costs no physical disk. Default 1024.
	MinSizeMiB int

	// Platform forces a specific OCI platform ("linux/amd64", "linux/arm64").
	// Empty picks the registry's default for the current arch.
	Platform string

	// Inject is a list of host files written into the filesystem on top of
	// the image content (last-wins, like an extra layer). The injected
	// content is hashed into the disk cache key.
	Inject []machine.InjectFile

	// Progress, if non-nil, reports build progress: once at PhaseStart,
	// then per-layer during the parallel PhaseDownload, then roughly
	// every 8 MiB of converted content during PhaseUnpack. Never called
	// on a cache hit.
	Progress func(ProgressUpdate)
}

// Phase identifies which stage of a Build a ProgressUpdate describes.
type Phase int

const (
	// PhaseStart is reported once when a real build begins (never on a
	// cache hit), carrying the compressed-size scale and layer count.
	PhaseStart Phase = iota
	// PhaseDownload covers the concurrent pull of layer blobs into the
	// cache; its updates carry per-layer Downloads.
	PhaseDownload
	// PhaseUnpack covers flattening the cached layers into the ext4 disk,
	// which proceeds one layer at a time.
	PhaseUnpack
)

// ProgressUpdate is one build progress report.
type ProgressUpdate struct {
	// Phase is the build stage this update describes.
	Phase Phase

	// UnpackedBytes is the cumulative flattened content converted into
	// the disk image so far. Reported during PhaseUnpack.
	UnpackedBytes int64

	// CompressedTotal is the sum of the image's compressed layer sizes
	// from the manifest.
	CompressedTotal int64

	// Downloads holds one entry per image layer during PhaseDownload —
	// the concurrent download's per-layer progress. Nil in other phases.
	Downloads []LayerStatus

	// Layer and Layers locate the layer currently being flattened
	// (1-based) during PhaseUnpack. Zero before the first layer starts.
	Layer, Layers int
}

// LayerStatus is one layer's download progress during PhaseDownload.
type LayerStatus struct {
	// Index is the layer's 1-based position in the image manifest.
	Index int
	// CompressedSize is the layer's blob size from the manifest.
	CompressedSize int64
	// Downloaded is the compressed bytes pulled from the registry so far.
	Downloaded int64
	// Cached is true when the layer was already present locally and
	// needed no download (Downloaded reads full from the first frame).
	Cached bool
	// Done is true once the layer is fully in the cache.
	Done bool
}

// Result describes a built rootfs.
type Result struct {
	// DiskPath is the built ext4 image. Use it as machine.RawDisk.Path.
	DiskPath string

	// Digest is the resolved image digest (the cache key).
	Digest string

	// UnpackedBytes is the size of the flattened filesystem tar that was
	// converted. Zero on a cache hit.
	UnpackedBytes int64
}

// Build pulls Opts.Ref, flattens its layers, and produces an ext4 disk. It
// is idempotent: a successful prior Build for the same digest is reused
// without re-pulling or re-creating the disk.
//
// The disk in the cache is shared; callers handing it to a VM that may
// write must copy-on-write it first (see Materialize).
func Build(ctx context.Context, opts Options) (Result, error) {
	if opts.Ref == "" {
		return Result{}, errors.New("oci: Options.Ref is required")
	}
	if opts.CacheDir == "" {
		return Result{}, errors.New("oci: Options.CacheDir is required")
	}
	if opts.MinSizeMiB <= 0 {
		opts.MinSizeMiB = 1024
	}

	digest, openImg, fetch, err := resolveSource(ctx, opts)
	if err != nil {
		return Result{}, err
	}

	// Injected content participates in the cache key, so it must be
	// hashed before the cache lookup — and hashing also fails fast on an
	// unreadable host file, before any network or disk work.
	injectHash, err := hashInject(opts.Inject)
	if err != nil {
		return Result{}, err
	}

	outDir := filepath.Join(opts.CacheDir, diskCacheKey(digest, opts, injectHash))
	diskPath := filepath.Join(outDir, "disk.ext4")
	doneMarker := filepath.Join(outDir, "done")

	if _, err := os.Stat(doneMarker); err == nil {
		if _, err := os.Stat(diskPath); err == nil {
			return Result{DiskPath: diskPath, Digest: digest}, nil
		}
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("oci: cache dir: %w", err)
	}

	img, err := openImg()
	if err != nil {
		return Result{}, err
	}
	// Content-addressed layer cache: layers shared between images are
	// fetched and stored once, and a re-Build after a cache wipe of the
	// built disks doesn't re-download. The download phase below warms this
	// same cache so the flatten reads every layer locally. Entries are
	// published atomically and hash-verified (see verifyingCache), so an
	// interrupted pull can't leave a truncated blob that later breaks the
	// flatten with "unexpected EOF".
	layerCache := newVerifyingCache(filepath.Join(opts.CacheDir, "layers"))
	img = cache.Image(img, layerCache)

	// The image config's Env (PATH and friends) is normally injected by
	// the container runtime at exec time — a VM boot has no runtime, so
	// bake it into the filesystem instead (a profile.d script for login
	// shells plus a raw KEY=VAL file for agents). Without this, golang's
	// /usr/local/go/bin never reaches PATH and `go` is "not found".
	cfgFile, err := img.ConfigFile()
	if err != nil {
		return Result{}, fmt.Errorf("oci: reading image config: %w", err)
	}

	total := compressedTotal(img)
	nLayers := layerCount(img)

	// Announce the build with the compressed-size scale before the long
	// part begins — callers use it to set expectations on a first build.
	if opts.Progress != nil {
		opts.Progress(ProgressUpdate{Phase: PhaseStart, CompressedTotal: total, Layers: nLayers})
	}

	// Download phase: pull every layer blob concurrently into the layer
	// cache, so the flatten below reads each layer warm instead of
	// serializing the network. Only registry sources have a fetcher;
	// tarball layers are already local and flatten reads them directly.
	if fetch != nil {
		layers, err := img.Layers()
		if err != nil {
			return Result{}, fmt.Errorf("oci: reading layers: %w", err)
		}
		report := func([]LayerStatus) {}
		if opts.Progress != nil {
			report = func(st []LayerStatus) {
				opts.Progress(ProgressUpdate{Phase: PhaseDownload, CompressedTotal: total, Downloads: st})
			}
		}
		if err := prefetchLayers(ctx, layerCache, layers, fetch, report); err != nil {
			return Result{}, fmt.Errorf("oci: downloading layers: %w", err)
		}
	}

	// Unpack phase: flatten the now-cached layers into the disk, one layer
	// at a time (whiteout ordering requires it).
	var progress func(int64)
	var layerPos atomic.Int64      // packed: pos<<32 | count, set by the flatten goroutine
	layerPos.Store(int64(nLayers)) // pos 0 of N
	if opts.Progress != nil {
		progress = func(done int64) {
			packed := layerPos.Load()
			opts.Progress(ProgressUpdate{
				Phase:           PhaseUnpack,
				UnpackedBytes:   done,
				CompressedTotal: total,
				Layer:           int(packed >> 32),
				Layers:          int(packed & 0xffffffff),
			})
		}
	}

	n, err := writeDisk(img, diskPath, int64(opts.MinSizeMiB)<<20, opts.Inject,
		imageEnvFiles(cfgFile.Config.Env), progress, &layerPos)
	if err != nil {
		return Result{}, err
	}

	if err := os.WriteFile(doneMarker, []byte(digest), 0o644); err != nil {
		return Result{}, fmt.Errorf("oci: writing done marker: %w", err)
	}
	return Result{DiskPath: diskPath, Digest: digest, UnpackedBytes: n}, nil
}

// countingTransport counts response-body bytes across every request it
// carries — blob downloads dominate, so the sum tracks pull progress.
type countingTransport struct {
	rt http.RoundTripper
	n  *atomic.Int64
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.rt.RoundTrip(req)
	if err != nil || resp.Body == nil {
		return resp, err
	}
	resp.Body = &countingBody{rc: resp.Body, n: t.n}
	return resp, nil
}

type countingBody struct {
	rc io.ReadCloser
	n  *atomic.Int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	b.n.Add(int64(n))
	return n, err
}

func (b *countingBody) Close() error { return b.rc.Close() }

// layerCount returns the number of layers in the image's manifest.
func layerCount(img v1.Image) int {
	manifest, err := img.Manifest()
	if err != nil {
		return 0
	}
	return len(manifest.Layers)
}

// compressedTotal sums the image's compressed layer sizes from the
// manifest — already fetched at this point, so this is local. Zero when
// the manifest is unreadable; callers treat that as "no scale".
func compressedTotal(img v1.Image) int64 {
	manifest, err := img.Manifest()
	if err != nil {
		return 0
	}
	var total int64
	for _, l := range manifest.Layers {
		total += l.Size
	}
	return total
}

// resolveSource turns opts.Ref into a content digest, a lazy image opener,
// and (for registry refs) a per-layer fetcher used by the parallel
// download phase. The digest comes first so a stale tag can't hit the
// cache for the wrong content. Local tarball refs (`docker save` output —
// any ref that looks like a path) read everything from the file, never
// touch the network, and have no fetcher.
func resolveSource(ctx context.Context, opts Options) (string, func() (v1.Image, error), layerFetcher, error) {
	if tarPath := LocalTarballPath(opts.Ref); tarPath != "" {
		img, err := tarball.ImageFromPath(tarPath, nil)
		if err != nil {
			return "", nil, nil, fmt.Errorf("oci: opening tarball %q: %w", tarPath, err)
		}
		h, err := img.Digest()
		if err != nil {
			return "", nil, nil, fmt.Errorf("oci: digest of %q: %w", tarPath, err)
		}
		return h.String(), func() (v1.Image, error) { return img, nil }, nil, nil
	}

	ref, err := name.ParseReference(opts.Ref)
	if err != nil {
		return "", nil, nil, fmt.Errorf("oci: parse reference %q: %w", opts.Ref, err)
	}
	craneOpts := []crane.Option{crane.WithContext(ctx)}
	if opts.Platform != "" {
		craneOpts = append(craneOpts, crane.WithPlatform(parsePlatform(opts.Platform)))
	}
	digest, err := crane.Digest(ref.Name(), craneOpts...)
	if err != nil {
		return "", nil, nil, fmt.Errorf("oci: resolving digest for %q: %w", ref.Name(), err)
	}
	open := func() (v1.Image, error) {
		img, err := crane.Pull(ref.Name(), craneOpts...)
		if err != nil {
			return nil, fmt.Errorf("oci: pulling %q: %w", ref.Name(), err)
		}
		return img, nil
	}
	// fetch opens one layer blob with its own counting transport. A
	// per-layer transport is what makes the byte count honest: blob GETs
	// often 307-redirect to a CDN, and only an isolated transport can
	// attribute the redirected body to the right layer.
	fetch := func(ctx context.Context, dg v1.Hash, counter *atomic.Int64) (v1.Layer, error) {
		var rt http.RoundTripper = remote.DefaultTransport
		if counter != nil {
			rt = &countingTransport{rt: rt, n: counter}
		}
		layer, err := remote.Layer(ref.Context().Digest(dg.String()),
			remote.WithContext(ctx),
			remote.WithAuthFromKeychain(authn.DefaultKeychain),
			remote.WithTransport(rt),
		)
		if err != nil {
			return nil, fmt.Errorf("oci: layer %s: %w", dg, err)
		}
		return layer, nil
	}
	return digest, open, fetch, nil
}

// ResolveDigest resolves ref to its content digest: a registry HEAD for
// registry refs, a local manifest hash for tarballs. Diagnostics
// (clawk doctor) use it to answer "is this sandbox's image still
// reachable" without building anything.
func ResolveDigest(ctx context.Context, ref, platform string) (string, error) {
	digest, _, _, err := resolveSource(ctx, Options{Ref: ref, Platform: platform})
	return digest, err
}

// Key returns the cache-directory name a Build with opts would use —
// without building anything. Registry refs resolve their digest over
// the network; tarball refs read it locally. `clawk image gc` computes
// the keep-set with this.
func Key(ctx context.Context, opts Options) (string, error) {
	if opts.MinSizeMiB <= 0 {
		opts.MinSizeMiB = 1024
	}
	digest, _, _, err := resolveSource(ctx, opts)
	if err != nil {
		return "", err
	}
	injectHash, err := hashInject(opts.Inject)
	if err != nil {
		return "", err
	}
	return diskCacheKey(digest, opts, injectHash), nil
}

// OptionsForImage maps a machine.OCIImage rootfs spec to build Options.
// Every consumer of the spec must use this one mapping, or cache keys
// drift between the provider's pre-build and the backend's Materialize.
func OptionsForImage(img machine.OCIImage) Options {
	return Options{
		Ref:        img.Ref,
		CacheDir:   img.CacheDir,
		MinSizeMiB: img.SizeMiB,
		Platform:   img.Platform,
		Inject:     img.Inject,
	}
}

// extraFile is a synthetic in-memory file written on top of the image
// content, alongside the caller's inject files.
type extraFile struct {
	path string // absolute guest path
	mode int64
	data []byte
}

// writeDisk streams img's flattened filesystem into a writable, padded
// ext4 image at diskPath, then appends inject and extra files on top
// (the converter applies later tar entries over earlier ones, so they
// behave like an extra topmost layer). The image is built under a
// temporary name and renamed into place so a crash never leaves a
// plausible-looking partial disk at the cached path. Returns the
// flattened tar's size in bytes.
func writeDisk(img v1.Image, diskPath string, sizeBytes int64, inject []machine.InjectFile, extra []extraFile, progress func(int64), layerPos *atomic.Int64) (int64, error) {
	shadow := make([]string, 0, len(inject)+len(extra))
	for _, f := range inject {
		shadow = append(shadow, f.GuestPath)
	}
	for _, f := range extra {
		shadow = append(shadow, f.path)
	}
	var onLayer func(pos, count int)
	if layerPos != nil {
		onLayer = func(pos, count int) {
			layerPos.Store(int64(pos)<<32 | int64(count))
		}
	}
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		err := flatten(img, tw, shadow, onLayer)
		if err == nil {
			err = appendInject(tw, inject)
		}
		if err == nil {
			err = appendExtra(tw, extra)
		}
		if err == nil {
			err = tw.Close()
		}
		pw.CloseWithError(err)
	}()
	// Closing the read end unblocks the flatten goroutine if the
	// conversion bails before draining the stream.
	defer pr.Close()
	cr := &countingReader{r: pr, progress: progress}

	tmp := diskPath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, fmt.Errorf("oci: creating disk image: %w", err)
	}
	defer func() {
		f.Close()
		os.Remove(tmp)
	}()

	if err := ext4.Convert(cr, f, ext4.Writable(), ext4.TotalSize(sizeBytes)); err != nil {
		return 0, fmt.Errorf("oci: converting to ext4: %w", err)
	}
	if err := f.Close(); err != nil {
		return 0, fmt.Errorf("oci: closing disk image: %w", err)
	}
	if err := os.Rename(tmp, diskPath); err != nil {
		return 0, fmt.Errorf("oci: promoting disk image: %w", err)
	}
	return cr.n, nil
}

// Resolve turns any machine.RootFS into the canonical on-disk path for
// its content. RawDisk values pass through unchanged; OCIImage values are
// materialized by calling Build.
//
// The returned path may be shared across multiple Machines (an OCI digest
// cache, or a user-owned master image). Backends wanting a per-VM
// writable disk should use Materialize instead, which reflinks on top.
func Resolve(ctx context.Context, r machine.RootFS) (machine.RawDisk, error) {
	switch rr := r.(type) {
	case machine.RawDisk:
		return rr, nil
	case machine.OCIImage:
		res, err := Build(ctx, OptionsForImage(rr))
		if err != nil {
			return machine.RawDisk{}, err
		}
		return machine.RawDisk{Path: res.DiskPath}, nil
	default:
		return machine.RawDisk{}, fmt.Errorf("oci: cannot resolve RootFS of type %T", r)
	}
}

// Materialize produces a per-caller writable RawDisk at dstPath by
// resolving r and copy-on-writing the source into dstPath. Use this
// instead of Resolve when the VM may write to the disk and the source
// may be shared across multiple Machines.
//
// If src and dstPath are the same file, no copy is performed — safe for
// callers that conditionally pre-materialize their rootfs.
func Materialize(ctx context.Context, r machine.RootFS, dstPath string) (machine.RawDisk, error) {
	src, err := Resolve(ctx, r)
	if err != nil {
		return machine.RawDisk{}, err
	}
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return machine.RawDisk{}, fmt.Errorf("oci: preparing disk dir: %w", err)
	}
	if err := cow.Clone(src.Path, dstPath); err != nil {
		return machine.RawDisk{}, fmt.Errorf("oci: cloning %s → %s: %w",
			src.Path, dstPath, err)
	}
	return machine.RawDisk{Path: dstPath}, nil
}

// countingReader counts bytes read through it; io.TeeReader has no
// equivalent that doesn't need a writer.
type countingReader struct {
	r        io.Reader
	n        int64
	progress func(int64) // optional; invoked at most every progressStride bytes
	reported int64
}

// progressStride spaces out progress callbacks: frequent enough to feel
// live, rare enough to never matter for throughput.
const progressStride = 8 << 20

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	cr.n += int64(n)
	if cr.progress != nil && cr.n-cr.reported >= progressStride {
		cr.reported = cr.n
		cr.progress(cr.n)
	}
	return n, err
}

// diskCacheKey names the cache directory for a built disk. The digest
// alone isn't enough: the disk's content also depends on the size floor,
// the platform (a multi-arch tag resolves to one index digest for every
// platform), and any injected files.
// diskFormatVersion is bumped whenever the ext4 layout the builder emits
// changes in a way that should invalidate already-cached disks. It is
// part of the cache key, so a bump makes the next `clawk up` rebuild the
// rootfs instead of reusing a stale one.
//
//	v2: inode table sized to the disk (mkfs.ext4's ratio) rather than to
//	    the file count, so a padded rootfs isn't starved of inodes (see
//	    compactext4.inodesForBlocks).
const diskFormatVersion = 2

func diskCacheKey(digest string, opts Options, injectHash string) string {
	key := strings.ReplaceAll(digest, ":", "_")
	if opts.Platform != "" {
		key += "_" + strings.ReplaceAll(opts.Platform, "/", "-")
	}
	key = fmt.Sprintf("%s_%dm", key, opts.MinSizeMiB)
	if injectHash != "" {
		key += "_i" + injectHash
	}
	key += fmt.Sprintf("_v%d", diskFormatVersion)
	return key
}

// hashInject digests the inject list — destination, mode, and file
// content — into a short stable hex string. Returns "" for an empty list
// so inject-free cache keys keep their historical shape.
func hashInject(inject []machine.InjectFile) (string, error) {
	if len(inject) == 0 {
		return "", nil
	}
	h := sha256.New()
	for _, f := range inject {
		fmt.Fprintf(h, "%s\x00%o\x00", f.GuestPath, f.Mode)
		src, err := os.Open(f.HostPath)
		if err != nil {
			return "", fmt.Errorf("oci: inject %s: %w", f.GuestPath, err)
		}
		_, err = io.Copy(h, src)
		src.Close()
		if err != nil {
			return "", fmt.Errorf("oci: hashing inject %s: %w", f.GuestPath, err)
		}
		h.Write([]byte{0})
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:16], nil
}

// ImageEnvPath is where the image config's Env lands inside the built
// filesystem, one KEY=VALUE per line. Guest agents read it to give
// spawned processes the environment the image was built to run with.
const ImageEnvPath = "/etc/clawk/image-env"

// imageEnvFiles renders the image config Env as in-filesystem files: the
// raw KEY=VALUE list at ImageEnvPath, plus a profile.d script so login
// shells get the image's PATH even after /etc/profile resets it.
func imageEnvFiles(env []string) []extraFile {
	if len(env) == 0 {
		return nil
	}
	var raw, script strings.Builder
	script.WriteString("# Generated by clawk from the OCI image config's Env.\n")
	for _, e := range env {
		k, v, ok := strings.Cut(e, "=")
		if !ok || k == "" {
			continue
		}
		fmt.Fprintf(&raw, "%s=%s\n", k, v)
		fmt.Fprintf(&script, "export %s='%s'\n", k, strings.ReplaceAll(v, "'", `'\''`))
	}
	return []extraFile{
		{path: ImageEnvPath, mode: 0o644, data: []byte(raw.String())},
		{path: "/etc/profile.d/00-clawk-image-env.sh", mode: 0o644, data: []byte(script.String())},
	}
}

// appendExtra writes synthetic in-memory files as regular tar entries.
func appendExtra(tw *tar.Writer, extra []extraFile) error {
	for _, f := range extra {
		hdr := &tar.Header{
			Name:     strings.TrimPrefix(f.path, "/"),
			Mode:     f.mode,
			Size:     int64(len(f.data)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("oci: writing %s: %w", f.path, err)
		}
		if _, err := tw.Write(f.data); err != nil {
			return fmt.Errorf("oci: writing %s: %w", f.path, err)
		}
	}
	return nil
}

// appendInject writes each inject file as a regular tar entry, owned by
// root. Parent directories are created on demand by the ext4 converter.
func appendInject(tw *tar.Writer, inject []machine.InjectFile) error {
	for _, f := range inject {
		if !strings.HasPrefix(f.GuestPath, "/") {
			return fmt.Errorf("oci: inject path %q must be absolute", f.GuestPath)
		}
		src, err := os.Open(f.HostPath)
		if err != nil {
			return fmt.Errorf("oci: inject %s: %w", f.GuestPath, err)
		}
		info, err := src.Stat()
		if err != nil {
			src.Close()
			return fmt.Errorf("oci: inject %s: %w", f.GuestPath, err)
		}
		hdr := &tar.Header{
			Name:     strings.TrimPrefix(f.GuestPath, "/"),
			Mode:     int64(f.Mode),
			Size:     info.Size(),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			src.Close()
			return fmt.Errorf("oci: inject %s: %w", f.GuestPath, err)
		}
		_, err = io.Copy(tw, src)
		src.Close()
		if err != nil {
			return fmt.Errorf("oci: inject %s: %w", f.GuestPath, err)
		}
	}
	return nil
}

// LocalTarballPath reports whether ref names a local `docker save`
// tarball rather than a registry reference, returning the path (or ""
// for registry refs). Path shapes (absolute, ./relative) can never be
// valid registry refs, and a .tar suffix on a bare name is unambiguous
// enough in practice.
func LocalTarballPath(ref string) string {
	if strings.HasPrefix(ref, "/") || strings.HasPrefix(ref, "./") ||
		strings.HasPrefix(ref, "../") || strings.HasSuffix(ref, ".tar") {
		return ref
	}
	return ""
}

func parsePlatform(s string) *v1.Platform {
	parts := strings.SplitN(s, "/", 2)
	p := &v1.Platform{OS: parts[0]}
	if len(parts) == 2 {
		p.Architecture = parts[1]
	}
	return p
}
