//go:build darwin || linux

package cli

import (
	"os"
	"syscall"
)

// platformDiskUsage reports the physical bytes a file uses on disk via the
// standard Stat_t.Blocks field (512-byte blocks). On APFS this correctly
// reflects the savings from clonefile sharing.
func platformDiskUsage(info os.FileInfo) int64 {
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return info.Size()
	}
	// Blocks is in 512-byte units on both Linux and Darwin.
	return sys.Blocks * 512
}
