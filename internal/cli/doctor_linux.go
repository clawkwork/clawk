//go:build linux

package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// platformHostChecks probes the Linux/firecracker host prerequisites that
// aren't visible until a VM actually tries to boot: the firecracker binary
// and read/write access to /dev/kvm. The kvm one is the single most common
// first-run failure — firecracker fails InstanceStart with an opaque
// "Permission denied (os error 13) ... /dev/kvm file's ACL" when the user
// isn't in the kvm group — so doctor names the exact fix.
func platformHostChecks() []doctorCheck {
	var results []doctorCheck

	if _, err := exec.LookPath("firecracker"); err != nil {
		results = append(results, fail("host: firecracker",
			"not found on PATH (the Linux hypervisor)",
			"install firecracker and put it on PATH: https://github.com/firecracker-microvm/firecracker/releases"))
	} else {
		results = append(results, ok("host: firecracker", "on PATH"))
	}

	switch f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); {
	case err == nil:
		f.Close()
		results = append(results, ok("host: /dev/kvm", "readable/writable"))
	case errors.Is(err, os.ErrNotExist):
		results = append(results, fail("host: /dev/kvm",
			"missing — no KVM support (nested virt off, or a VM/container without /dev/kvm)",
			"enable virtualization in BIOS/hypervisor; on a cloud VM, use a metal/nested-virt instance"))
	case errors.Is(err, os.ErrPermission):
		results = append(results, fail("host: /dev/kvm",
			"present but not accessible (permission denied) — firecracker cannot open it",
			"add yourself to the kvm group: sudo usermod -aG kvm $USER, then start a new login session"))
	default:
		results = append(results, warn("host: /dev/kvm",
			fmt.Sprintf("unexpected error opening it: %v", err),
			"ensure the invoking user can read/write /dev/kvm"))
	}

	return results
}
