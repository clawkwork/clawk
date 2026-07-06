//go:build linux

package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// hostPhysicalMemoryBytes returns the host's total RAM from /proc/meminfo's
// MemTotal. Linux hosts run firecracker rather than vz, but admission control
// is host-agnostic, so the probe is provided on both platforms.
func hostPhysicalMemoryBytes() (uint64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, fmt.Errorf("read /proc/meminfo: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		rest, ok := strings.CutPrefix(line, "MemTotal:")
		if !ok {
			continue
		}
		rest = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest), "kB"))
		kib, err := strconv.ParseUint(rest, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse MemTotal %q: %w", rest, err)
		}
		return kib * 1024, nil
	}
	return 0, fmt.Errorf("MemTotal not found in /proc/meminfo")
}
