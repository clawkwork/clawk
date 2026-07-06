//go:build darwin

package cli

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// hostPhysicalMemoryBytes returns the Mac's installed physical RAM via the
// hw.memsize sysctl — the same total Activity Monitor reports.
func hostPhysicalMemoryBytes() (uint64, error) {
	n, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0, fmt.Errorf("sysctl hw.memsize: %w", err)
	}
	return n, nil
}
