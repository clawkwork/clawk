//go:build !linux

package sandbox

// InitRootHelpers is a no-op on non-linux — the hidden mount/umount helpers
// only exist for the Linux-only VM providers.
func InitRootHelpers() {}
