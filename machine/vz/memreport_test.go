package vz

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMemReportRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		r    memReport
	}{
		{name: "zero", r: memReport{}},
		{
			name: "typical idle 4GiB guest",
			r:    memReport{TotalKiB: 4 * 1024 * 1024, AvailableKiB: 3_500_000, PSIMemSomeCenti: 0},
		},
		{
			name: "guest under memory pressure",
			r:    memReport{TotalKiB: 4 * 1024 * 1024, AvailableKiB: 120_000, PSIMemSomeCenti: 4231},
		},
		{
			name: "busy guest with traffic",
			r:    memReport{TotalKiB: 4 * 1024 * 1024, AvailableKiB: 1_000_000, Load1Centi: 312, NetIOBytes: 987_654_321},
		},
		{
			name: "max values",
			r:    memReport{TotalKiB: ^uint64(0), AvailableKiB: ^uint64(0), PSIMemSomeCenti: ^uint64(0), Load1Centi: ^uint64(0), NetIOBytes: ^uint64(0)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := encodeMemReport(tt.r)
			require.Len(t, b, memReportSize, "encoded length")
			got, err := decodeMemReport(b)
			require.NoError(t, err)
			want := tt.r
			want.HasActivity = true // full-size buffers always carry the activity fields
			require.Equal(t, want, got)
		})
	}
}

// TestDecodeMemReportLegacy: a 24-byte report from a guest agent that
// predates the activity fields must decode with the memory fields intact
// and HasActivity false — zeroed activity fields mean "absent", and the
// idle watchdog must be able to tell that apart from "guest is quiet".
func TestDecodeMemReportLegacy(t *testing.T) {
	full := encodeMemReport(memReport{
		TotalKiB: 4 * 1024 * 1024, AvailableKiB: 2_000_000, PSIMemSomeCenti: 17,
		Load1Centi: 999, NetIOBytes: 999, // must NOT survive a legacy-length decode
	})
	got, err := decodeMemReport(full[:memReportLegacySize])
	require.NoError(t, err)
	require.Equal(t, memReport{
		TotalKiB: 4 * 1024 * 1024, AvailableKiB: 2_000_000, PSIMemSomeCenti: 17,
	}, got)
	require.False(t, got.HasActivity)
}

func TestDecodeMemReportShort(t *testing.T) {
	_, err := decodeMemReport(make([]byte, memReportLegacySize-1))
	require.Error(t, err, "short buffer must error, not zero-pad")
}
