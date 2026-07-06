// smoke-alpine boots Alpine in a vz VM via the machine library. It's a
// hand-crank smoke test for the DirectKernel boot path (everything that
// went through clawk today uses EFIBoot).
//
// Usage:
//
//	cd machine && make smoke-alpine            # or ARGS="-ref ubuntu:24.04 ..."
//
// `go run` won't work: Virtualization.framework requires the binary to be
// codesigned with com.apple.security.virtualization, which the make target
// does (ad-hoc) before running.
//
// Defaults pull docker.io/library/alpine:3.20 for linux/arm64 and boot it
// with the Kata Containers release kernel (the one Apple's `container
// machine` uses; virtio-fs and vsock included). Flags override each step.
// The tool prints the last ~40 lines of the guest serial console at exit
// so you can see what the kernel thought.
//
// Docker images are not bootable distros: alpine's image ships an
// /etc/inittab pointing at openrc, but not openrc itself. So instead of
// the image's init, the default cmdline boots /bin/sh echoing a marker —
// that proves kernel + rootfs + exec for any image with a shell, which is
// everything this smoke test is for. Booting a real init is the injected
// sbin.clawk/init work.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/clawkwork/clawk/machine"
	"github.com/clawkwork/clawk/machine/kernel"
	"github.com/clawkwork/clawk/machine/oci"

	// vz registers itself in init; we need the side effect.
	_ "github.com/clawkwork/clawk/machine/vz"
)

const (
	defaultRef = "docker.io/library/alpine:3.20"

	defaultVCPU       = 1
	defaultMemoryMiB  = 512
	defaultBootWindow = 30 * time.Second

	// bootMarker is what the guest shell echoes once userspace is up. The
	// cmdline spells it CLAWK-SMOKE$x-OK ($x is unset, expanding to
	// nothing) so the kernel's "Kernel command line:" console echo can
	// never match the needle — only output from the running shell can.
	bootMarker = "CLAWK-SMOKE-OK"

	// defaultCmdline boots the image's /bin/sh rather than its init:
	// docker images generally don't contain a working init system.
	// Everything after "--" is passed to init as arguments; the kernel
	// honors double quotes when splitting.
	defaultCmdline = `console=hvc0 root=/dev/vda rw init=/bin/sh -- -c "echo CLAWK-SMOKE$x-OK; exec sleep 2147483647"`
)

func main() {
	ref := flag.String("ref", defaultRef, "OCI image reference to boot")
	kernelPath := flag.String("kernel", "", "path to a vmlinux (skips download)")
	kataVersion := flag.String("kata-version", kernel.DefaultKataVersion,
		"Kata Containers release to fetch the kernel from")
	cmdline := flag.String("cmdline", defaultCmdline, "kernel command line")
	wait := flag.Duration("wait", defaultBootWindow, "how long to wait for boot evidence")
	keep := flag.Bool("keep", false, "don't stop the VM on success — press Ctrl-C to stop")
	flag.Parse()

	log.SetFlags(log.Ltime)
	if runtime.GOOS != "darwin" {
		log.Fatal("smoke-alpine: darwin-only (uses the vz backend)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, *ref, *kernelPath, *kataVersion, *cmdline, *wait, *keep); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, ref, kernelPathArg, kataVersion, cmdline string, wait time.Duration, keep bool) error {
	cache, err := cacheDir()
	if err != nil {
		return err
	}

	vmlinux, err := ensureKernel(ctx, cache, kernelPathArg, kataVersion)
	if err != nil {
		return fmt.Errorf("obtaining kernel: %w", err)
	}
	log.Printf("kernel: %s", vmlinux)

	log.Printf("building rootfs from %s", ref)
	built, err := oci.Build(ctx, oci.Options{
		Ref:      ref,
		CacheDir: filepath.Join(cache, "oci"),
		Platform: "linux/" + runtime.GOARCH,
	})
	if err != nil {
		return fmt.Errorf("oci build: %w", err)
	}
	if built.UnpackedBytes > 0 {
		log.Printf("rootfs: %s (%d bytes unpacked)", built.DiskPath, built.UnpackedBytes)
	} else {
		log.Printf("rootfs: %s (cached)", built.DiskPath)
	}

	stateDir, err := os.MkdirTemp("", "smoke-alpine-*")
	if err != nil {
		return err
	}
	// defer os.RemoveAll(stateDir)
	fmt.Printf("stateDir: %+v\n", stateDir)
	consoleLog := filepath.Join(stateDir, "console.log")

	spec := machine.Spec{
		ID:        "smoke-alpine",
		VCPU:      defaultVCPU,
		MemoryMiB: defaultMemoryMiB,
		Boot: machine.DirectKernel{
			Vmlinux: vmlinux,
			Cmdline: cmdline,
		},
		RootFS: machine.OCIImage{
			Ref:      ref,
			CacheDir: filepath.Join(cache, "oci"),
			// Match the explicit Build above so Materialize reuses its
			// cache entry instead of building a second disk.
			Platform: "linux/" + runtime.GOARCH,
		},
		Serial: machine.Serial{LogPath: consoleLog},
	}

	backend, err := machine.Get("vz")
	if err != nil {
		return fmt.Errorf("machine.Get: %w", err)
	}
	m, err := backend.New(ctx, spec, stateDir)
	if err != nil {
		return fmt.Errorf("backend.New: %w", err)
	}

	log.Printf("creating vm")
	if err := m.Create(ctx); err != nil {
		// vz surfaces a missing-entitlement NSError as opaque text; the
		// usual cause is running an unsigned binary via `go run`.
		if strings.Contains(err.Error(), "com.apple.security.virtualization") {
			log.Printf("hint: the binary isn't codesigned — run `make smoke-alpine` instead of `go run`")
		}
		return fmt.Errorf("creating vm: %w", err)
	}
	log.Printf("starting vm")
	if err := m.Start(ctx); err != nil {
		return fmt.Errorf("starting vm: %w", err)
	}

	found, elapsed := waitForBoot(ctx, consoleLog, wait)
	if found != "" {
		log.Printf("PASS: boot evidence %q found after %s", found, elapsed)
	} else {
		// Dump the console now — Stop can be slow/hang and we want the
		// diagnostic info regardless.
		log.Printf("FAIL: no boot evidence after %s; console follows", wait)
		dumpTail(consoleLog, 60)
	}

	if keep && found != "" {
		log.Printf("--keep set; VM is still running (state=%s). Ctrl-C to stop.", stateDir)
		<-ctx.Done()
	}

	log.Printf("stopping vm")
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.Stop(stopCtx, true); err != nil {
		log.Printf("Stop: %v", err)
	}
	if found != "" {
		// Show a little post-shutdown context on success too.
		dumpTail(consoleLog, 20)
	}

	if found == "" {
		return errors.New("boot did not produce expected output in time")
	}
	return nil
}

// cacheDir returns a per-user cache directory for kernels and OCI blobs.
// Lives outside tmp so repeated runs don't re-download.
func cacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "clawk-smoke")
	return dir, os.MkdirAll(dir, 0o755)
}

// ensureKernel returns a local path to a vmlinux: the explicit --kernel
// path if given, otherwise the Kata Containers release kernel — the same
// one Apple's `container machine` boots. Unlike the firecracker-CI kernels
// this tool used to fetch, the Kata kernel ships virtio-fs and vsock,
// which clawk's host shares and agent transport need.
func ensureKernel(ctx context.Context, cache, pathArg, kataVersion string) (string, error) {
	if pathArg != "" {
		if _, err := os.Stat(pathArg); err != nil {
			return "", fmt.Errorf("--kernel %q: %w", pathArg, err)
		}
		return pathArg, nil
	}
	log.Printf("fetching kata %s kernel (cached after first run)", kataVersion)
	return kernel.Fetch(ctx, kernel.Options{
		CacheDir: cache,
		Arch:     runtime.GOARCH,
		Version:  kataVersion,
	})
}

// waitForBoot polls the serial log for strings that only appear after the
// kernel has handed off to userspace: the marker from the default cmdline,
// or distro-init banners when -cmdline overrides it to a real init.
func waitForBoot(ctx context.Context, consoleLog string, window time.Duration) (string, time.Duration) {
	needles := []string{bootMarker, "Welcome to Alpine", "OpenRC", "login:"}
	start := time.Now()
	deadline := start.Add(window)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return "", time.Since(start)
		}
		data, _ := os.ReadFile(consoleLog)
		for _, n := range needles {
			if strings.Contains(string(data), n) {
				return n, time.Since(start)
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return "", time.Since(start)
}

// dumpTail prints the last n non-empty lines of the console log. Helpful
// post-mortem when boot didn't finish, and reassurance when it did.
func dumpTail(path string, n int) {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("reading console: %v", err)
		return
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	fmt.Fprintln(os.Stderr, "--- last console output ---")
	for _, l := range lines {
		fmt.Fprintln(os.Stderr, l)
	}
	fmt.Fprintln(os.Stderr, "---------------------------")
}
