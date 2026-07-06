package compactext4

// Upstream verifies images by loop-mounting them, which needs root. This
// fork verifies with e2fsck -fn instead — unprivileged, and it still
// catches structural corruption. The mount-based per-file verification
// helpers are stubbed out; file content is covered by the package's
// Stat-based assertions and by machine/oci's debugfs-driven tests.

import (
	"os/exec"
	"strings"
	"testing"
)

const mib = 1 << 20

func verifyTestFile(t *testing.T, mountPath string, tf testFile) {
	t.Helper()
}

func mountImage(t *testing.T, image string, mountPath string) bool {
	t.Helper()
	return false
}

func unmountImage(t *testing.T, mountPath string) {
	t.Helper()
}

// fsck runs e2fsck in read-only mode against the image. e2fsck exits 0
// only for a clean filesystem; anything else fails the test. Skipped
// quietly when e2fsprogs isn't installed.
func fsck(t *testing.T, image string) {
	t.Helper()
	e2fsck, err := exec.LookPath("e2fsck")
	if err != nil {
		t.Log("e2fsck not found; skipping filesystem check")
		return
	}
	out, err := exec.Command(e2fsck, "-f", "-n", image).CombinedOutput()
	if err != nil {
		t.Errorf("e2fsck %s: %v\n%s", image, err, strings.TrimSpace(string(out)))
	}
}
