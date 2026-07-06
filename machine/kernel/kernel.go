// Package kernel fetches and caches guest kernels for [machine.DirectKernel]
// boot.
//
// The default source is the Kata Containers static release — the same
// kernel Apple's `container machine` boots (see
// third_party/misc/container ContainerSystemConfig.swift). It is built to
// host containers inside a VM, which is exactly the clawk shape: virtio-fs
// (host shares), vsock (agent transport), overlayfs and cgroups are all in,
// and it boots fast with no initrd. Firecracker-CI kernels, by contrast,
// lack virtio-fs because firecracker doesn't support it.
//
// The release asset is a ~hundreds-of-MB tar.zst containing the whole Kata
// runtime; only the vmlinux member is extracted, streamed straight out of
// the download without touching disk. Results are cached as
//
//	<CacheDir>/kernels/kata-<version>-<arch>/vmlinux
package kernel

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// DefaultKataVersion is the pinned Kata Containers release. Bump together
// with DefaultBinaryPath — the vmlinux member name embeds the kernel
// version shipped by that release.
const DefaultKataVersion = "3.28.0"

// DefaultBinaryPath is the vmlinux member inside the kata-static archive
// for DefaultKataVersion.
const DefaultBinaryPath = "opt/kata/share/kata-containers/vmlinux-6.18.15-186"

// Options control a Fetch.
type Options struct {
	// CacheDir is the root cache directory. Required.
	CacheDir string

	// Arch is the guest architecture ("arm64", "amd64"). Required —
	// callers usually pass runtime.GOARCH.
	Arch string

	// Version is the Kata release to pin. Default DefaultKataVersion.
	Version string

	// URL overrides the archive URL derived from Version and Arch.
	URL string

	// BinaryPath is the archive member to extract. Default
	// DefaultBinaryPath. Must be overridden together with URL/Version
	// when they name a release shipping a different kernel version.
	BinaryPath string

	// Override, when set, replaces the default Kata kernel with a
	// user-supplied raw vmlinux — a local file path (returned as-is) or
	// an http(s) URL (downloaded once and cached). Used by the clawk.mod
	// `vm ( kernel <path|url> )` directive / `--kernel` flag, chiefly to
	// boot a KVM-enabled kernel for nested virtualization (the stock Kata
	// kernel ships with KVM disabled, so the guest has no /dev/kvm).
	// Unlike URL, this points at a bare vmlinux, not a kata-static
	// archive. When set, Version/URL/BinaryPath are ignored.
	Override string
}

// Fetch returns the path to a cached vmlinux, downloading and extracting
// it on first use. Safe to call concurrently for different cache dirs;
// concurrent calls for the same cache dir may download twice but converge
// via atomic rename.
func Fetch(ctx context.Context, opts Options) (string, error) {
	if opts.CacheDir == "" {
		return "", errors.New("kernel: Options.CacheDir is required")
	}
	if opts.Override != "" {
		return fetchOverride(ctx, opts.CacheDir, opts.Override)
	}
	if opts.Arch == "" {
		return "", errors.New("kernel: Options.Arch is required")
	}
	if opts.Version == "" {
		opts.Version = DefaultKataVersion
	}
	if opts.URL == "" {
		opts.URL = fmt.Sprintf(
			"https://github.com/kata-containers/kata-containers/releases/download/%[1]s/kata-static-%[1]s-%[2]s.tar.zst",
			opts.Version, opts.Arch)
	}
	if opts.BinaryPath == "" {
		opts.BinaryPath = DefaultBinaryPath
	}

	vmlinux, cached := CachedPath(opts)
	if cached {
		return vmlinux, nil
	}
	if err := os.MkdirAll(filepath.Dir(vmlinux), 0o755); err != nil {
		return "", fmt.Errorf("kernel: cache dir: %w", err)
	}

	if err := download(ctx, opts.URL, opts.BinaryPath, vmlinux); err != nil {
		return "", err
	}
	return vmlinux, nil
}

// CachedPath returns where Fetch(opts) stores its vmlinux and whether
// it is already present — callers use it to skip "fetching" narration
// for what is actually a stat.
func CachedPath(opts Options) (string, bool) {
	if opts.Override != "" {
		if !isURL(opts.Override) {
			_, err := os.Stat(opts.Override)
			return opts.Override, err == nil
		}
		dst := overridePath(opts.CacheDir, opts.Override)
		_, err := os.Stat(dst)
		return dst, err == nil
	}
	if opts.Version == "" {
		opts.Version = DefaultKataVersion
	}
	vmlinux := filepath.Join(opts.CacheDir, "kernels",
		fmt.Sprintf("kata-%s-%s", opts.Version, opts.Arch), "vmlinux")
	_, err := os.Stat(vmlinux)
	return vmlinux, err == nil
}

// fetchOverride resolves a user-supplied kernel. A local path is
// returned as-is (it must already exist); an http(s) URL is downloaded
// once (raw vmlinux, not an archive) and cached under
// kernels/override-<hash>/vmlinux keyed by the URL.
func fetchOverride(ctx context.Context, cacheDir, src string) (string, error) {
	if !isURL(src) {
		if _, err := os.Stat(src); err != nil {
			return "", fmt.Errorf("kernel: override %s: %w", src, err)
		}
		return src, nil
	}
	dst := overridePath(cacheDir, src)
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", fmt.Errorf("kernel: cache dir: %w", err)
	}
	if err := downloadRaw(ctx, src, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func isURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// overridePath is the cache location for a downloaded override kernel,
// keyed by a short hash of the source URL so distinct URLs don't collide.
func overridePath(cacheDir, url string) string {
	sum := sha256.Sum256([]byte(url))
	return filepath.Join(cacheDir, "kernels",
		"override-"+hex.EncodeToString(sum[:4]), "vmlinux")
}

// downloadRaw streams a bare file at url into dst through a temp file so a
// partial download never looks cached.
func downloadRaw(ctx context.Context, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("kernel: building request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("kernel: downloading %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("kernel: downloading %s: HTTP %s", url, resp.Status)
	}
	tmp := dst + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("kernel: creating %s: %w", tmp, err)
	}
	_, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		os.Remove(tmp)
		return fmt.Errorf("kernel: writing %s: %w", tmp, copyErr)
	}
	if closeErr != nil {
		os.Remove(tmp)
		return fmt.Errorf("kernel: closing %s: %w", tmp, closeErr)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("kernel: promoting %s: %w", dst, err)
	}
	return nil
}

// download streams the archive at url and extracts member into dst,
// writing through a temp file so a partial download never looks cached.
func download(ctx context.Context, url, member, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("kernel: building request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("kernel: downloading %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("kernel: downloading %s: HTTP %s", url, resp.Status)
	}

	zr, err := zstd.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("kernel: opening zstd stream: %w", err)
	}
	defer zr.Close()

	tmp := dst + ".tmp"
	if err := extractMember(tar.NewReader(zr), member, tmp); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("kernel: extracting from %s: %w", url, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("kernel: promoting %s: %w", dst, err)
	}
	return nil
}

// extractMember scans tr for member and writes it to tmp. On a miss it
// reports the vmlinux-like members it saw, so a version bump that changed
// the kernel file name produces an actionable error.
func extractMember(tr *tar.Reader, member, tmp string) error {
	want := normalizeMember(member)
	var candidates []string
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("member %q not found; vmlinux-like members: %v",
				member, candidates)
		}
		if err != nil {
			return fmt.Errorf("reading archive: %w", err)
		}
		name := normalizeMember(hdr.Name)
		if strings.Contains(path.Base(name), "vmlinux") {
			candidates = append(candidates, name)
		}
		if name != want || hdr.Typeflag != tar.TypeReg {
			continue
		}
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return fmt.Errorf("creating %s: %w", tmp, err)
		}
		_, copyErr := io.Copy(f, tr)
		closeErr := f.Close()
		if copyErr != nil {
			return fmt.Errorf("writing %s: %w", tmp, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("closing %s: %w", tmp, closeErr)
		}
		return nil
	}
}

// normalizeMember strips the "./" and "/" prefixes archive tools
// variously emit so member comparison is prefix-insensitive.
func normalizeMember(name string) string {
	return strings.TrimPrefix(path.Clean(strings.TrimPrefix(name, "/")), "./")
}
