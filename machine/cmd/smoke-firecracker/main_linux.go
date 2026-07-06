//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/clawkwork/clawk/machine"

	_ "github.com/clawkwork/clawk/machine/firecracker"
)

const (
	// firecracker-ci publishes kernels + Ubuntu squashfs rootfs at this
	// S3 bucket. Path-style HTTPS works; virtual-hosted TLS fails on
	// dot-containing bucket names.
	s3Host           = "s3.amazonaws.com"
	s3Bucket         = "spec.ccfc.min"
	defaultCIVersion = "v1.10"

	defaultVCPU       = 1
	defaultMemoryMiB  = 256
	defaultBootWindow = 30 * time.Second
)

func main() {
	kernelPath := flag.String("kernel", "", "path to vmlinux (defaults to firecracker-ci latest)")
	rootfsPath := flag.String("rootfs", "", "path to rootfs image (defaults to firecracker-ci ubuntu squashfs)")
	ciVersion := flag.String("ci-version", defaultCIVersion, "firecracker-ci version prefix")
	wait := flag.Duration("wait", defaultBootWindow, "how long to wait for boot evidence")
	keep := flag.Bool("keep", false, "don't stop the VM after boot; Ctrl-C to exit")
	flag.Parse()

	log.SetFlags(log.Ltime)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, *kernelPath, *rootfsPath, *ciVersion, *wait, *keep); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, kernelPathArg, rootfsPathArg, ciVersion string, wait time.Duration, keep bool) error {
	if _, err := exec.LookPath("firecracker"); err != nil {
		return fmt.Errorf("firecracker not on PATH; install from https://github.com/firecracker-microvm/firecracker/releases")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return fmt.Errorf("/dev/kvm not accessible: %w", err)
	}

	cache, err := cacheDir()
	if err != nil {
		return err
	}

	arch, err := firecrackerArch()
	if err != nil {
		return err
	}

	kernel, err := ensureKernel(ctx, cache, kernelPathArg, ciVersion, arch)
	if err != nil {
		return fmt.Errorf("obtaining kernel: %w", err)
	}
	log.Printf("kernel: %s", kernel)

	rootfs, err := ensureRootFS(ctx, cache, rootfsPathArg, ciVersion, arch)
	if err != nil {
		return fmt.Errorf("obtaining rootfs: %w", err)
	}
	log.Printf("rootfs: %s", rootfs)

	stateDir, err := os.MkdirTemp("", "smoke-firecracker-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stateDir)

	spec := machine.Spec{
		ID:        "smoke-firecracker",
		VCPU:      defaultVCPU,
		MemoryMiB: defaultMemoryMiB,
		Boot: machine.DirectKernel{
			Vmlinux: kernel,
			Cmdline: "console=ttyS0 reboot=k panic=1 pci=off ro",
		},
		RootFS: machine.RawDisk{Path: rootfs, ReadOnly: true},
	}

	backend, err := machine.Get("firecracker")
	if err != nil {
		return fmt.Errorf("machine.Get: %w", err)
	}
	m, err := backend.New(ctx, spec, stateDir)
	if err != nil {
		return fmt.Errorf("backend.New: %w", err)
	}

	log.Printf("creating vm (state: %s)", stateDir)
	if err := m.Create(ctx); err != nil {
		return fmt.Errorf("creating vm: %w", err)
	}
	log.Printf("starting vm")
	if err := m.Start(ctx); err != nil {
		return fmt.Errorf("starting vm: %w", err)
	}

	// firecracker's stdout (containing guest serial) is captured by the
	// backend into <stateDir>/firecracker.log.
	logPath := filepath.Join(stateDir, "firecracker.log")
	found, elapsed := waitForBoot(ctx, logPath, wait)
	if found != "" {
		log.Printf("PASS: boot evidence %q after %s", found, elapsed)
	} else {
		log.Printf("FAIL: no boot evidence after %s; log follows", wait)
		dumpTail(logPath, 60)
	}

	if keep && found != "" {
		log.Printf("--keep set; VM still running. Ctrl-C to stop.")
		<-ctx.Done()
	}

	log.Printf("stopping vm")
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.Stop(stopCtx, true); err != nil {
		log.Printf("Stop: %v", err)
	}
	if found != "" {
		dumpTail(logPath, 20)
	}
	if found == "" {
		return errors.New("boot did not produce expected output in time")
	}
	return nil
}

// waitForBoot polls firecracker's combined stdout/guest-serial log for
// strings that only appear after the guest has reached userspace.
func waitForBoot(ctx context.Context, logPath string, window time.Duration) (string, time.Duration) {
	needles := []string{"Reached target Multi-User", "Ubuntu 22.04", "login:", "Welcome to Ubuntu"}
	start := time.Now()
	deadline := start.Add(window)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return "", time.Since(start)
		}
		data, _ := os.ReadFile(logPath)
		for _, n := range needles {
			if strings.Contains(string(data), n) {
				return n, time.Since(start)
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return "", time.Since(start)
}

func dumpTail(path string, n int) {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("reading log %s: %v", path, err)
		return
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	fmt.Fprintln(os.Stderr, "--- last firecracker log ---")
	for _, l := range lines {
		fmt.Fprintln(os.Stderr, l)
	}
	fmt.Fprintln(os.Stderr, "----------------------------")
}

// --- kernel + rootfs provisioning ---

func cacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "clawk-smoke")
	return dir, os.MkdirAll(dir, 0o755)
}

func firecrackerArch() (string, error) {
	switch runtime.GOARCH {
	case "arm64":
		return "aarch64", nil
	case "amd64":
		return "x86_64", nil
	}
	return "", fmt.Errorf("no firecracker kernel for GOARCH=%s", runtime.GOARCH)
}

func ensureKernel(ctx context.Context, cache, override, ciVersion, arch string) (string, error) {
	if override != "" {
		if _, err := os.Stat(override); err != nil {
			return "", fmt.Errorf("--kernel %q: %w", override, err)
		}
		return override, nil
	}
	key, err := latestKey(ctx, ciVersion, arch, "vmlinux-", `vmlinux-[0-9]+\.[0-9]+\.[0-9]+`)
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("https://%s/%s/%s", s3Host, s3Bucket, key)
	dst := filepath.Join(cache, filepath.Base(url))
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	log.Printf("downloading %s → %s", url, dst)
	return dst, download(ctx, url, dst)
}

func ensureRootFS(ctx context.Context, cache, override, ciVersion, arch string) (string, error) {
	if override != "" {
		if _, err := os.Stat(override); err != nil {
			return "", fmt.Errorf("--rootfs %q: %w", override, err)
		}
		return override, nil
	}
	// firecracker-ci publishes ubuntu-XX.YY.squashfs alongside the kernel.
	key := fmt.Sprintf("firecracker-ci/%s/%s/ubuntu-22.04.squashfs", ciVersion, arch)
	url := fmt.Sprintf("https://%s/%s/%s", s3Host, s3Bucket, key)
	dst := filepath.Join(cache, "ubuntu-22.04-"+arch+".squashfs")
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	log.Printf("downloading %s → %s", url, dst)
	return dst, download(ctx, url, dst)
}

// latestKey queries the bucket for the newest key under
// firecracker-ci/<version>/<arch>/ whose basename matches the regex
// `prefix<pattern>`. Used to pick the most recent vmlinux version
// available without hard-coding a kernel release.
func latestKey(ctx context.Context, ciVersion, arch, prefix, pattern string) (string, error) {
	listURL := fmt.Sprintf("http://%s.%s/?prefix=firecracker-ci/%s/%s/%s&list-type=2",
		s3Bucket, s3Host, ciVersion, arch, prefix)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
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
	re := regexp.MustCompile(`<Key>(firecracker-ci/[^<]+` + pattern + `)</Key>`)
	m := re.FindAllStringSubmatch(string(body), -1)
	if len(m) == 0 {
		return "", fmt.Errorf("no keys under firecracker-ci/%s/%s/%s", ciVersion, arch, prefix)
	}
	keys := make([]string, 0, len(m))
	for _, mm := range m {
		keys = append(keys, mm[1])
	}
	sort.Slice(keys, func(i, j int) bool { return versionLess(keys[i], keys[j], prefix) })
	return keys[len(keys)-1], nil
}

// versionLess compares two keys by the numeric version that follows
// prefix in each. Lexical sort breaks here because 5.10.100 sorts before
// 5.10.99; we need component-wise numeric comparison.
func versionLess(a, b, prefix string) bool {
	va, vb := trailingVersion(a, prefix), trailingVersion(b, prefix)
	for i := 0; i < len(va) && i < len(vb); i++ {
		if va[i] != vb[i] {
			return va[i] < vb[i]
		}
	}
	return len(va) < len(vb)
}

func trailingVersion(s, prefix string) []int {
	idx := strings.LastIndex(s, prefix)
	if idx < 0 {
		return nil
	}
	parts := strings.Split(s[idx+len(prefix):], ".")
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

func download(ctx context.Context, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	tmp := dst + ".partial"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
