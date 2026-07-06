package machine

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSuspendMetaRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := SuspendMeta{
		Backend:         "vz",
		SpecFingerprint: SuspendSpecFingerprint(Spec{VCPU: 4, MemoryMiB: 1024, MemoryMaxMiB: 4096}),
		ClawkVersion:    "v1.0.0",
	}
	require.NoError(t, WriteSuspendMeta(dir, want))
	got, ok := ReadSuspendMeta(dir)
	require.True(t, ok)
	require.Equal(t, want, got)
}

func TestReadSuspendMetaAbsentOrCorrupt(t *testing.T) {
	// Absent: pre-meta states restore via the hypervisor's own checks.
	_, ok := ReadSuspendMeta(t.TempDir())
	require.False(t, ok)
}

func TestSuspendMetaIncompatibleWith(t *testing.T) {
	base := SuspendMeta{Backend: "vz", SpecFingerprint: "vcpu=4 mem=1024", ClawkVersion: "v1.0.0"}

	require.Empty(t, base.IncompatibleWith(base), "identical meta must be compatible")

	// Cross-backend can never restore.
	reason := base.IncompatibleWith(SuspendMeta{Backend: "firecracker", SpecFingerprint: base.SpecFingerprint})
	require.Contains(t, reason, "vz")
	require.Contains(t, reason, "firecracker")

	// A changed VM shape names both sides, so the log line is a diff.
	reason = base.IncompatibleWith(SuspendMeta{Backend: "vz", SpecFingerprint: "vcpu=8 mem=1024"})
	require.Contains(t, reason, "vcpu=4")
	require.Contains(t, reason, "vcpu=8")
	require.Contains(t, reason, "v1.0.0")

	// Empty want fields skip their check (partial callers).
	require.Empty(t, base.IncompatibleWith(SuspendMeta{}))
}
