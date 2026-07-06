package cli

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAdmitGuestMemory(t *testing.T) {
	const (
		host16 = 16 * 1024 // 16 GiB host
		host24 = 24 * 1024
		res    = 6144 // default reserve
	)
	tests := []struct {
		name      string
		want      uint64
		others    []uint64
		hostMiB   uint64
		reserve   uint64
		wantAdmit bool
	}{
		{
			name:      "first box fits comfortably",
			want:      4096,
			others:    nil,
			hostMiB:   host16,
			reserve:   res,
			wantAdmit: true,
		},
		{
			name:      "two 4GiB boxes fit on 16GiB",
			want:      4096,
			others:    []uint64{4096},
			hostMiB:   host16,
			reserve:   res,
			wantAdmit: true, // 4096+4096+6144 = 14336 <= 16384
		},
		{
			name:      "third 4GiB box does not fit on 16GiB",
			want:      4096,
			others:    []uint64{4096, 4096},
			hostMiB:   host16,
			reserve:   res,
			wantAdmit: false, // 4096*3+6144 = 18432 > 16384
		},
		{
			name:      "exactly filling to the byte is admitted",
			want:      2048,
			others:    []uint64{8192},
			hostMiB:   host16,
			reserve:   res,
			wantAdmit: true, // 2048+8192+6144 = 16384 == 16384
		},
		{
			name:      "one MiB over is refused",
			want:      2049,
			others:    []uint64{8192},
			hostMiB:   host16,
			reserve:   res,
			wantAdmit: false,
		},
		{
			name:      "more headroom on a bigger host",
			want:      4096,
			others:    []uint64{4096, 4096},
			hostMiB:   host24,
			reserve:   res,
			wantAdmit: true, // 4096*3+6144 = 18432 <= 24576
		},
		{
			name:      "over-reserved host refuses without underflow",
			want:      1024,
			others:    nil,
			hostMiB:   4096,
			reserve:   res, // reserve alone exceeds host
			wantAdmit: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := admitGuestMemory(tt.want, tt.others, tt.hostMiB, tt.reserve)
			if tt.wantAdmit {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestHostReserveMiB(t *testing.T) {
	t.Run("scales at a quarter above the floor", func(t *testing.T) {
		require.Equal(t, uint64(6144), hostReserveMiB(24*1024)) // 24 GiB → 6 GiB
		require.Equal(t, uint64(4096), hostReserveMiB(16*1024)) // 16 GiB → 4 GiB
	})
	t.Run("clamps to the floor on small hosts", func(t *testing.T) {
		require.Equal(t, uint64(hostReserveFloorMiB), hostReserveMiB(8*1024)) // 8 GiB → 2 GiB, floored to 3 GiB
	})
	t.Run("env overrides outright", func(t *testing.T) {
		t.Setenv("CLAWK_HOST_RESERVE_MIB", "8192")
		require.Equal(t, uint64(8192), hostReserveMiB(24*1024))
	})
	t.Run("bad env falls back to scaling", func(t *testing.T) {
		t.Setenv("CLAWK_HOST_RESERVE_MIB", "not-a-number")
		require.Equal(t, uint64(6144), hostReserveMiB(24*1024))
	})
}
