package cow

import (
	"bytes"
	"math/rand"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestClone verifies byte-for-byte equality across the fast-path /
// fallback-path split. tmpfs (typical t.TempDir) doesn't support
// reflink so the test exercises the copy fallback; on a reflink-
// capable fs it takes the fast path. Either way the result should
// be a valid copy.
func TestClone(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	dst := filepath.Join(dir, "dst.bin")

	// 1 MiB of deterministic random data — large enough that a zero-byte
	// no-op would silently pass.
	want := make([]byte, 1<<20)
	rand.New(rand.NewSource(42)).Read(want)
	err := os.WriteFile(src, want, 0o644)
	require.NoError(t, err, "writing src")

	err = Clone(src, dst)
	require.NoError(t, err, "Clone")

	got, err := os.ReadFile(dst)
	require.NoError(t, err, "reading dst")
	require.True(t, bytes.Equal(got, want), "dst contents differ from src (%d vs %d bytes)", len(got), len(want))
}

// TestCloneReplacesExistingDst verifies Clone overwrites a pre-existing
// destination rather than appending or erroring.
func TestCloneReplacesExistingDst(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	dst := filepath.Join(dir, "dst.bin")

	err := os.WriteFile(src, []byte("hello"), 0o644)
	require.NoError(t, err)
	err = os.WriteFile(dst, []byte("this should be replaced, and is longer"), 0o644)
	require.NoError(t, err)
	err = Clone(src, dst)
	require.NoError(t, err, "Clone")
	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	require.Equal(t, "hello", string(got), "dst content mismatch")
}

// TestCloneSamePathNoOp ensures Clone does nothing when src==dst —
// important for backends that may pass pre-materialized disks through
// the Clone call for uniformity.
func TestCloneSamePathNoOp(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "file.bin")
	data := []byte("precious")
	err := os.WriteFile(p, data, 0o644)
	require.NoError(t, err)
	err = Clone(p, p)
	require.NoError(t, err, "Clone same path")
	got, err := os.ReadFile(p)
	require.NoError(t, err)
	require.True(t, bytes.Equal(got, data), "same-path clone corrupted file: got %q want %q", got, data)
}

// TestCloneSparse verifies the fallback copy doesn't materialize holes:
// a mostly-hole source must produce a destination whose physical
// footprint is close to its data, not its logical size.
func TestCloneSparse(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.img")
	dst := filepath.Join(dir, "dst.img")

	const logical = 256 << 20 // 256 MiB, almost all hole
	f, err := os.Create(src)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte("data at the front"), 0)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte("data in the middle"), logical/2)
	require.NoError(t, err)
	err = f.Truncate(logical)
	require.NoError(t, err)
	err = f.Close()
	require.NoError(t, err)

	err = Clone(src, dst)
	require.NoError(t, err, "Clone")

	info, err := os.Stat(dst)
	require.NoError(t, err)
	require.Equal(t, int64(logical), info.Size(), "dst logical size mismatch")
	var got [18]byte
	df, err := os.Open(dst)
	require.NoError(t, err)
	defer df.Close()
	_, err = df.ReadAt(got[:], logical/2)
	require.NoError(t, err)
	require.Equal(t, "data in the middle", string(got[:]), "dst content at hole boundary mismatch")

	var st syscall.Stat_t
	err = syscall.Stat(dst, &st)
	require.NoError(t, err)
	physical := st.Blocks * 512
	require.LessOrEqual(t, physical, int64(logical/8), "dst physical footprint = %d bytes; want sparse (< %d)", physical, logical/8)
}
