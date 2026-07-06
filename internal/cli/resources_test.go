package cli

import (
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/stretchr/testify/require"
)

func TestSpecResources(t *testing.T) {
	tests := []struct {
		name       string
		sb         config.Sandbox
		wantCPU    uint
		wantMiB    uint64
		wantMaxMiB uint64
	}{
		{name: "defaults", sb: config.Sandbox{}, wantCPU: 4, wantMiB: 1024, wantMaxMiB: 4096},
		{name: "cpu only keeps memory defaults", sb: config.Sandbox{CPU: 8}, wantCPU: 8, wantMiB: 1024, wantMaxMiB: 4096},
		{name: "baseline only is a fixed size", sb: config.Sandbox{MemoryMiB: 2048}, wantCPU: 4, wantMiB: 2048, wantMaxMiB: 2048},
		{name: "baseline and ceiling both honored", sb: config.Sandbox{MemoryMiB: 2048, MemoryMaxMiB: 8192}, wantCPU: 4, wantMiB: 2048, wantMaxMiB: 8192},
		{name: "ceiling only takes default baseline", sb: config.Sandbox{MemoryMaxMiB: 8192}, wantCPU: 4, wantMiB: 1024, wantMaxMiB: 8192},
		{name: "ceiling below default baseline clamps baseline", sb: config.Sandbox{CPU: 2, MemoryMaxMiB: 512}, wantCPU: 2, wantMiB: 512, wantMaxMiB: 512},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cpu, mib, maxMiB := specResources(&tt.sb)
			require.Equal(t, tt.wantCPU, cpu)
			require.Equal(t, tt.wantMiB, mib)
			require.Equal(t, tt.wantMaxMiB, maxMiB)
		})
	}
}
