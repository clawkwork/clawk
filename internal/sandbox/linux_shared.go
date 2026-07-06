//go:build linux

// Linux VM provider plumbing — bridge + TAP, firecracker-ci kernel
// fetch, loop-mounted rootfs. Consumed by the firecracker provider; kept
// separate because the asset/network plumbing is sizeable and unrelated
// to the hypervisor-specific spec building in firecracker_linux.go.
package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// linuxBridge is the shared L2 bridge firecracker TAPs attach to. It
// deliberately carries NO host IP: gvproxy owns L3 (gateway, DHCP, NAT,
// DNS) entirely in userspace — same as the vz provider — so the bridge is
// just a dumb switch joining each VM's TAP to the daemon-owned TAP that
// feeds gvproxy. Because no real interface ever claims gvproxy's
// 192.168.127.1/24, this is safe even when clawk runs nested inside a vz
// VM that already uses that subnet on its own eth0. See fcnet_linux.go.
const linuxBridge = "clawkbr0"

// firecracker-ci kernel catalog. The bucket exposes versioned
// `vmlinux-X.Y.Z` objects; we pick the newest by component-wise version
// ordering. The rootfs comes from the configured OCI image, not here.
const (
	ciCacheSubdir = ".cache/clawk/linux-assets"
	ciS3Host      = "s3.amazonaws.com"
	ciS3Bucket    = "spec.ccfc.min"
	ciVersion     = "v1.10"
	ciKernelPfx   = "vmlinux-"
)

// ciArch is the firecracker-ci asset arch path for the host. The bucket uses
// kernel-style names (aarch64 / x86_64), not Go's GOARCH ("arm64" / "amd64").
func ciArch() string {
	switch runtime.GOARCH {
	case "arm64":
		return "aarch64"
	case "amd64":
		return "x86_64"
	default:
		return runtime.GOARCH
	}
}

// tapDevice returns a deterministic TAP name for a sandbox. IFNAMSIZ is 16
// (15 usable); "clawk" + 8 hex chars of sha256(name) is 13.
func tapDevice(sbName string) string {
	sum := sha256.Sum256([]byte(sbName))
	return "clawk" + hex.EncodeToString(sum[:4])
}

// linkExists reports whether a network link with the given name is present.
// `ip link show` exits 0 when found, 1 otherwise — we rely on the exit code
// rather than parsing output.
func linkExists(name string) bool {
	return exec.Command("ip", "link", "show", name).Run() == nil
}

// ensureLinuxBridge is idempotent: creates the shared (IP-less) bridge and
// brings it up. No address is assigned — see linuxBridge.
func ensureLinuxBridge() error {
	if !linkExists(linuxBridge) {
		if err := runSudo("ip", "link", "add", "name", linuxBridge, "type", "bridge"); err != nil {
			return err
		}
	}
	return runSudo("ip", "link", "set", linuxBridge, "up")
}

// gvTapDevice returns the deterministic name of the daemon-owned TAP that
// feeds gvproxy for a sandbox — the firecracker guest's TAP (tapDevice)
// plus a "g" suffix. IFNAMSIZ is 16 (15 usable); "clawk"+8 hex+"g" is 14.
func gvTapDevice(sbName string) string { return tapDevice(sbName) + "g" }

// ensureTAP creates the TAP device if missing, enslaves it to the bridge,
// and brings it up. The device is owned by the current uid so the (non-
// root) daemon and firecracker can open its fd without CAP_NET_ADMIN.
// Safe to call on an already-configured device.
func ensureTAP(tap string) error {
	if !linkExists(tap) {
		uid := strconv.Itoa(os.Getuid())
		if err := runSudo("ip", "tuntap", "add", "dev", tap, "mode", "tap", "user", uid); err != nil {
			return err
		}
	}
	if err := runSudo("ip", "link", "set", tap, "master", linuxBridge); err != nil {
		return err
	}
	return runSudo("ip", "link", "set", tap, "up")
}

// runSudo runs `sudo -n <cmd> <args...>` and wraps the error with the command
// output for diagnosis. The -n flag means "never prompt" — fail fast instead
// of hanging waiting for a password.
func runSudo(cmd string, args ...string) error {
	full := append([]string{"-n", cmd}, args...)
	out, err := exec.Command("sudo", full...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("sudo %s %s: %w (%s)", cmd, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ciCacheDir is the user-local cache for the downloaded kernel.
func ciCacheDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ciCacheSubdir)
}

// ciEnsureKernel returns the path to the newest firecracker-ci vmlinux,
// downloading it on first use. Cached across providers.
func ciEnsureKernel(ctx context.Context) (string, error) {
	cache := ciCacheDir()
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return "", err
	}
	key, err := ciLatestKernelKey(ctx)
	if err != nil {
		return "", err
	}
	dst := filepath.Join(cache, filepath.Base(key))
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	url := fmt.Sprintf("https://%s/%s/%s", ciS3Host, ciS3Bucket, key)
	return dst, downloadAsset(ctx, url, dst)
}

// ciLatestKernelKey lists the S3 bucket and returns the newest vmlinux key,
// compared component-wise so 5.10.223 sorts higher than 5.10.99.
func ciLatestKernelKey(ctx context.Context) (string, error) {
	// Virtual-hosted style fails TLS (bucket name has dots); path-style
	// HTTPS works, or plain-HTTP on the same host.
	listURL := fmt.Sprintf("http://%s.%s/?prefix=firecracker-ci/%s/%s/%s&list-type=2",
		ciS3Bucket, ciS3Host, ciVersion, ciArch(), ciKernelPfx)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, http.NoBody)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	re := regexp.MustCompile(`<Key>(firecracker-ci/[^<]+vmlinux-[0-9]+\.[0-9]+\.[0-9]+)</Key>`)
	matches := re.FindAllStringSubmatch(string(body), -1)
	if len(matches) == 0 {
		return "", errors.New("no kernels listed")
	}
	keys := make([]string, 0, len(matches))
	for _, m := range matches {
		keys = append(keys, m[1])
	}
	sort.Slice(keys, func(i, j int) bool { return versionLess(keys[i], keys[j]) })
	return keys[len(keys)-1], nil
}

func versionLess(a, b string) bool {
	va, vb := trailingVersion(a), trailingVersion(b)
	for i := 0; i < len(va) && i < len(vb); i++ {
		if va[i] != vb[i] {
			return va[i] < vb[i]
		}
	}
	return len(va) < len(vb)
}

func trailingVersion(s string) []int {
	idx := strings.LastIndex(s, ciKernelPfx)
	if idx < 0 {
		return nil
	}
	parts := strings.Split(s[idx+len(ciKernelPfx):], ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				break
			}
			n = n*10 + int(c-'0')
		}
		out = append(out, n)
	}
	return out
}

func downloadAsset(ctx context.Context, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return fmt.Errorf("building request for %s: %w", url, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	tmp := dst + ".partial"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("creating %s: %w", tmp, err)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("finalising %s: %w", dst, err)
	}
	return nil
}

// mountedRootfs loop-mounts an ext4 rootfs, invokes fn with the mountpoint
// inside it, and unmounts on return. Uses loop_mount_linux.go's self-exec
// helpers instead of `sudo mount`/`sudo umount` because util-linux mount(8)
// and umount(8) crash with SIGILL on some aarch64 nested-virt kernels.
func mountedRootfs(rootfs string, fn func(mnt string) error) (retErr error) {
	mnt, err := os.MkdirTemp("", "clawk-rootfs-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(mnt)
	loop, err := loopMountExt4(rootfs, mnt)
	if err != nil {
		return err
	}
	defer func() {
		if err := loopUnmountExt4(mnt, loop); err != nil && retErr == nil {
			retErr = err
		}
	}()
	return fn(mnt)
}

// checkKVMAccess verifies the invoking user can open /dev/kvm read/write —
// the one host prerequisite firecracker can't degrade around. It returns an
// actionable error (naming the kvm-group fix) instead of letting the failure
// surface later as firecracker's opaque InstanceStart error in a detached
// daemon log. See FirecrackerProvider.Start for why the check lives there.
func checkKVMAccess() error {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err == nil {
		f.Close()
		return nil
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
		return errors.New("/dev/kvm not present — this host has no KVM support " +
			"(enable virtualization in BIOS, or use a metal/nested-virt instance)")
	case errors.Is(err, os.ErrPermission):
		return errors.New("/dev/kvm is not accessible (permission denied) — " +
			"add yourself to the kvm group: sudo usermod -aG kvm $USER, then start a new login session")
	default:
		return fmt.Errorf("cannot open /dev/kvm: %w", err)
	}
}

// readPIDFile reads a pidfile and returns the pid, or 0 on any error.
func readPIDFile(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid
}

// processAlive reports whether pid is still running. It probes with the
// null signal (signal 0): the kernel runs its existence/permission checks
// without delivering anything. NOTE: must be syscall.Signal(0), not a nil
// os.Signal — os.Process.Signal rejects a nil signal as "unsupported
// signal type", which would make this always report dead (so Start never
// sees an already-running daemon and Status always says "stopped").
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// stopByPIDFile sends SIGTERM to the process named by pidPath, waits up to
// timeout, then escalates to SIGKILL. A missing or stale pidfile is not an
// error — the caller's intent was "make sure it's not running."
func stopByPIDFile(pidPath string, timeout time.Duration) error {
	pid := readPIDFile(pidPath)
	if pid <= 0 {
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	if err := proc.Signal(os.Interrupt); err != nil {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return proc.Kill()
}
