//go:build linux

// Loop-mount plumbing for rootfs injection. We do NOT call `sudo mount -o loop`
// because util-linux mount(8) and umount(8) crash with SIGILL/SIGSEGV on some
// aarch64 nested-virt kernels (the mount(2) syscall itself succeeds, but the
// binaries die in post-syscall housekeeping). Instead we re-exec the clawk
// binary under sudo and invoke syscall.Mount / syscall.Unmount directly.
//
// The hidden subcommands MountHelperCmd / UnmountHelperCmd are dispatched by
// InitRootHelpers, which must be called at the top of main before any CLI
// parsing so sudo re-exec paths don't run cobra.

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// Hidden subcommand names. Prefixed with "__" so they don't collide with
// user-facing cobra commands and are obviously private.
const (
	mountHelperCmd   = "__loop-mount"
	unmountHelperCmd = "__loop-unmount"
)

// InitRootHelpers handles privileged mount/umount re-exec before cobra runs.
// Returns only if os.Args does not match a helper command; otherwise it
// exits the process.
func InitRootHelpers() {
	if len(os.Args) < 2 {
		return
	}
	switch os.Args[1] {
	case mountHelperCmd:
		// args: __loop-mount <source> <mountpoint> <fstype>
		if len(os.Args) != 5 {
			fmt.Fprintf(os.Stderr, "usage: clawk %s <source> <mountpoint> <fstype>\n", mountHelperCmd)
			os.Exit(2)
		}
		if err := syscall.Mount(os.Args[2], os.Args[3], os.Args[4], 0, ""); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	case unmountHelperCmd:
		// args: __loop-unmount <mountpoint>
		if len(os.Args) != 3 {
			fmt.Fprintf(os.Stderr, "usage: clawk %s <mountpoint>\n", unmountHelperCmd)
			os.Exit(2)
		}
		// MNT_DETACH so a lingering reference (e.g. a half-torn-down util-linux
		// mount(8) from this kernel's SIGILL bug) can't pin the mountpoint.
		if err := syscall.Unmount(os.Args[2], 0x00000002 /* MNT_DETACH */); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
}

// loopMountExt4 attaches backing to a free loop device and mounts it as ext4
// at mnt. Returns the loop device path so the caller can detach it after
// unmounting.
func loopMountExt4(backing, mnt string) (string, error) {
	loop, err := losetupFind(backing)
	if err != nil {
		return "", err
	}
	if err := selfMount(loop, mnt, "ext4"); err != nil {
		_ = losetupDetach(loop)
		return "", err
	}
	return loop, nil
}

// loopUnmountExt4 unmounts mnt and detaches the loop device. Both steps run
// even if the first fails, so the caller isn't left with half-torn-down
// kernel state; the first error encountered is returned.
func loopUnmountExt4(mnt, loop string) error {
	umErr := selfUnmount(mnt)
	loErr := losetupDetach(loop)
	switch {
	case umErr != nil:
		return umErr
	case loErr != nil:
		return loErr
	}
	return nil
}

func selfMount(src, dst, fstype string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving self exe: %w", err)
	}
	out, err := exec.Command("sudo", "-n", self, mountHelperCmd, src, dst, fstype).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s %s: %w (%s)", mountHelperCmd, src, dst, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func selfUnmount(dst string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving self exe: %w", err)
	}
	out, err := exec.Command("sudo", "-n", self, unmountHelperCmd, dst).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w (%s)", unmountHelperCmd, dst, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func losetupFind(backing string) (string, error) {
	out, err := exec.Command("sudo", "-n", "losetup", "--find", "--show", backing).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("losetup --find %s: %w (%s)", backing, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func losetupDetach(loop string) error {
	out, err := exec.Command("sudo", "-n", "losetup", "-d", loop).CombinedOutput()
	if err != nil {
		return fmt.Errorf("losetup -d %s: %w (%s)", loop, err, strings.TrimSpace(string(out)))
	}
	return nil
}
