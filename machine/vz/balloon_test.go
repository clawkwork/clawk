package vz

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBalloonTarget(t *testing.T) {
	const gib = 1024 * 1024 * 1024
	tests := []struct {
		name  string
		level pressureLevel
		full  uint64
		want  uint64
	}{
		{
			name:  "normal restores full",
			level: pressureNormal,
			full:  4 * gib,
			want:  4 * gib,
		},
		{
			name:  "warn gives back a quarter",
			level: pressureWarn,
			full:  4 * gib,
			want:  3 * gib,
		},
		{
			name:  "critical gives back half",
			level: pressureCritical,
			full:  4 * gib,
			want:  2 * gib,
		},
		{
			name:  "critical clamps to floor",
			level: pressureCritical,
			full:  768 * 1024 * 1024, // half would be 384 MiB, below the floor
			want:  minBalloonFloorBytes,
		},
		{
			name:  "warn clamps to floor",
			level: pressureWarn,
			full:  640 * 1024 * 1024, // 3/4 = 480 MiB, below the floor
			want:  minBalloonFloorBytes,
		},
		{
			name:  "guest at or below floor is left untouched",
			level: pressureCritical,
			full:  minBalloonFloorBytes,
			want:  minBalloonFloorBytes,
		},
		{
			name:  "tiny guest is never inflated below its own size",
			level: pressureCritical,
			full:  256 * 1024 * 1024,
			want:  256 * 1024 * 1024,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := balloonTarget(tt.level, tt.full)
			require.Equal(t, tt.want, got, "balloonTarget(%s, %d)", tt.level, tt.full)
		})
	}
}

func TestBalloonTargetNeverExceedsFull(t *testing.T) {
	const full = 2 * 1024 * 1024 * 1024
	for _, level := range []pressureLevel{pressureNormal, pressureWarn, pressureCritical} {
		got := balloonTarget(level, full)
		require.LessOrEqual(t, got, uint64(full), "balloonTarget(%s, %d) exceeds full size", level, full)
	}
}

func TestGuestDesiredTarget(t *testing.T) {
	const (
		mib      = 1024 * 1024
		gib      = 1024 * mib
		baseline = 1 * gib
		ceiling  = 4 * gib
		totalKiB = ceiling / 1024 // guest boots at the ceiling
	)
	tests := []struct {
		name string
		cur  uint64
		r    memReport
		want uint64
	}{
		{
			name: "no report holds current",
			cur:  2 * gib,
			r:    memReport{}, // TotalKiB == 0
			want: 2 * gib,
		},
		{
			name: "high PSI grows straight to ceiling",
			cur:  baseline,
			r:    memReport{TotalKiB: totalKiB, AvailableKiB: totalKiB / 2, PSIMemSomeCenti: psiDemandCenti},
			want: ceiling,
		},
		{
			name: "low slack grows to ceiling",
			cur:  baseline,
			r:    memReport{TotalKiB: totalKiB, AvailableKiB: totalKiB / 16}, // ~6% available < 12.5%
			want: ceiling,
		},
		{
			name: "high slack reclaims one step",
			cur:  ceiling,
			r:    memReport{TotalKiB: totalKiB, AvailableKiB: totalKiB / 2}, // 50% available > 40%
			want: ceiling - reclaimStepBytes,
		},
		{
			name: "high slack near baseline snaps to baseline",
			cur:  baseline + reclaimStepBytes/2,
			r:    memReport{TotalKiB: totalKiB, AvailableKiB: totalKiB / 2},
			want: baseline,
		},
		{
			name: "hysteresis band holds current",
			cur:  2 * gib,
			r:    memReport{TotalKiB: totalKiB, AvailableKiB: totalKiB / 4}, // 25% available, between 12.5% and 40%
			want: 2 * gib,
		},
		{
			name: "no burst headroom collapses to baseline",
			cur:  baseline,
			r:    memReport{TotalKiB: baseline / 1024, AvailableKiB: baseline / 1024 / 2},
			want: baseline, // ceiling == baseline here
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := uint64(ceiling)
			if tt.name == "no burst headroom collapses to baseline" {
				c = baseline
			}
			got := guestDesiredTarget(tt.cur, baseline, c, tt.r)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestGuestDesiredTargetStaysInRange(t *testing.T) {
	const (
		gib      = 1024 * 1024 * 1024
		baseline = 1 * gib
		ceiling  = 4 * gib
	)
	reports := []memReport{
		{},
		{TotalKiB: ceiling / 1024, AvailableKiB: 0, PSIMemSomeCenti: 9999},
		{TotalKiB: ceiling / 1024, AvailableKiB: ceiling / 1024},
		{TotalKiB: ceiling / 1024, AvailableKiB: ceiling / 1024 / 4},
	}
	for _, cur := range []uint64{baseline, 2 * gib, ceiling} {
		for _, r := range reports {
			got := guestDesiredTarget(cur, baseline, ceiling, r)
			require.GreaterOrEqual(t, got, uint64(baseline))
			require.LessOrEqual(t, got, uint64(ceiling))
		}
	}
}

func TestMergedBalloonTargetHostPressureWins(t *testing.T) {
	const (
		gib      = 1024 * 1024 * 1024
		baseline = 1 * gib
		ceiling  = 4 * gib
	)
	// Guest is starved and wants the full ceiling.
	starved := memReport{TotalKiB: ceiling / 1024, AvailableKiB: 0, PSIMemSomeCenti: 9999}

	// Under normal pressure the guest gets what it asks for.
	require.Equal(t, uint64(ceiling),
		mergedBalloonTarget(pressureNormal, baseline, baseline, ceiling, starved))

	// Under WARN the host caps growth at balloonTarget(warn, ceiling) even
	// though the guest is starving.
	wantWarn := balloonTarget(pressureWarn, ceiling)
	require.Equal(t, wantWarn,
		mergedBalloonTarget(pressureWarn, baseline, baseline, ceiling, starved))
	require.Less(t, wantWarn, uint64(ceiling))

	// Under CRITICAL it caps even lower.
	wantCrit := balloonTarget(pressureCritical, ceiling)
	require.Equal(t, wantCrit,
		mergedBalloonTarget(pressureCritical, baseline, baseline, ceiling, starved))
	require.Less(t, wantCrit, wantWarn)
}
