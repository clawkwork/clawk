package machine

import (
	"os"
	"path/filepath"
)

// suspendStateFiles are the per-backend marker files a Suspendable backend
// writes into its suspend directory: vz saves everything into state.bin,
// firecracker splits device state (snapshot.state) from the memory image
// (snapshot.mem). Kept here — next to the backends — so callers above the
// machine layer never pattern-match backend file layouts themselves.
var suspendStateFiles = []string{
	"state.bin",      // vz
	"snapshot.state", // firecracker
}

// SuspendStateExists reports whether dir holds a suspend-to-disk state
// written by a Suspendable backend, i.e. whether a Restore from it could
// continue a guest. It deliberately does not say WHICH backend wrote it —
// a mismatched restore fails cleanly at Restore time.
func SuspendStateExists(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := filepath.Base(e.Name())
		for _, want := range suspendStateFiles {
			if name == want {
				return true
			}
		}
	}
	return false
}
