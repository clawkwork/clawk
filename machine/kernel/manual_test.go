package kernel

// Manual harness for the real Kata release download (~hundreds of MB):
//
//	TEST_KATA_FETCH=1 go test ./kernel -run TestFetchKata -v
import (
	"context"
	"encoding/binary"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFetchKata_Manual(t *testing.T) {
	if os.Getenv("TEST_KATA_FETCH") == "" {
		t.Skip("set TEST_KATA_FETCH=1 to run (downloads the kata-static release)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	start := time.Now()
	vmlinux, err := Fetch(ctx, Options{
		CacheDir: t.TempDir(),
		Arch:     runtime.GOARCH,
	})
	require.NoError(t, err, "Fetch")
	info, err := os.Stat(vmlinux)
	require.NoError(t, err)
	t.Logf("fetched %s (%d MiB) in %s", vmlinux, info.Size()>>20, time.Since(start).Round(time.Second))
	require.GreaterOrEqual(t, info.Size(), int64(4<<20), "vmlinux suspiciously small: %d bytes", info.Size())
	assertLooksLikeKernel(t, vmlinux)
}

// assertLooksLikeKernel sanity-checks the file is an uncompressed kernel
// image for the current arch: ELF on amd64, the arm64 Image header magic
// ("ARMd" at offset 56) otherwise.
func assertLooksLikeKernel(t *testing.T, path string) {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	var head [64]byte
	_, err = f.ReadAt(head[:], 0)
	require.NoError(t, err)
	switch runtime.GOARCH {
	case "arm64":
		magic := binary.LittleEndian.Uint32(head[56:60])
		require.Equal(t, uint32(0x644d5241), magic, "missing arm64 Image magic, got %#x", magic)
	default:
		require.Equal(t, "\x7fELF", string(head[:4]), "not an ELF: %q", head[:4])
	}
}
