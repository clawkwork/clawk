//go:build !linux

package cli

// platformHostChecks is a no-op on non-Linux hosts. macOS links
// Virtualization.framework directly into the binary (see machine/vz), so
// there is no external hypervisor binary or device node to probe — the
// entitlements check happens at codesign time, not runtime.
func platformHostChecks() []doctorCheck { return nil }
