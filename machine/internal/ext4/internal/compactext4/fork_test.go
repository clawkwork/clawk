package compactext4

// Tests for this fork's local additions (Writable, TotalDiskSize, sparse
// tail). See README.md one level up for the list of changes.

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/clawkwork/clawk/machine/internal/ext4/internal/format"
	"github.com/stretchr/testify/require"
)

func buildImage(t *testing.T, path string, files []testFile, opts ...Option) {
	t.Helper()
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()
	w := NewWriter(f, opts...)
	for _, tf := range files {
		createTestFile(t, w, tf)
	}
	err = w.Close()
	require.NoError(t, err)
}

func readSuperBlock(t *testing.T, path string) *format.SuperBlock {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	_, err = f.Seek(1024, 0)
	require.NoError(t, err)
	var sb format.SuperBlock
	err = binary.Read(f, binary.LittleEndian, &sb)
	require.NoError(t, err)
	require.Equal(t, format.SuperBlockMagic, sb.Magic, "superblock magic = %#x, want %#x", sb.Magic, format.SuperBlockMagic)
	return &sb
}

func TestWritable(t *testing.T) {
	files := []testFile{
		{Path: "etc", File: &File{Mode: format.S_IFDIR | 0o755}},
		{Path: "etc/hostname", File: &File{Mode: 0o644}, Data: []byte("box\n")},
	}

	tests := []struct {
		name         string
		opts         []Option
		wantReadonly bool
	}{
		{name: "default is readonly", wantReadonly: true},
		{name: "writable drops flag", opts: []Option{Writable}, wantReadonly: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			image := filepath.Join(t.TempDir(), "fs.img")
			buildImage(t, image, files, tt.opts...)
			fsck(t, image)
			sb := readSuperBlock(t, image)
			got := sb.FeatureRoCompat&format.RoCompatReadonly != 0
			require.Equal(t, tt.wantReadonly, got, "RO_COMPAT_READONLY mismatch")
		})
	}
}

func TestTotalDiskSize(t *testing.T) {
	const total = 64 * mib
	image := filepath.Join(t.TempDir(), "fs.img")
	buildImage(t, image, []testFile{
		{Path: "data", File: &File{Mode: 0o644}, Data: data},
	}, Writable, TotalDiskSize(total))
	fsck(t, image)

	sb := readSuperBlock(t, image)
	got := int64(sb.BlocksCountLow) * BlockSize
	require.GreaterOrEqual(t, got, int64(total), "filesystem spans %d bytes, want >= %d", got, total)
	// The padding must be free space the guest can allocate, not dead
	// blocks: expect the overwhelming majority of the disk free.
	free, blocks := int64(sb.FreeBlocksCountLow), int64(sb.BlocksCountLow)
	require.GreaterOrEqual(t, free, blocks*9/10, "free blocks = %d of %d, want >= 90%%", free, blocks)
	require.NotZero(t, sb.FreeInodesCount, "no free inodes in padded filesystem")

	info, err := os.Stat(image)
	require.NoError(t, err)
	require.Equal(t, int64(sb.BlocksCountLow)*BlockSize, info.Size(), "file size != filesystem size")
	// The free tail must be a hole, not written zeros.
	var st syscall.Stat_t
	err = syscall.Stat(image, &st)
	require.NoError(t, err)
	physical := st.Blocks * 512
	require.LessOrEqual(t, physical, int64(total/4), "physical footprint %d bytes; want sparse (< %d)", physical, total/4)
}

// TestTotalDiskSizeContentWins checks that content larger than the requested
// padding grows the filesystem instead of failing — and that a writable
// filesystem keeps headroom even then: zero free blocks means the guest
// can't append a line to /etc/passwd (hit for real when a 1.7 GiB image
// exceeded a 1 GiB floor). Expect at least the 512 MiB slack floor.
func TestTotalDiskSizeContentWins(t *testing.T) {
	image := filepath.Join(t.TempDir(), "fs.img")
	buildImage(t, image, []testFile{
		{Path: "big", File: &File{Mode: 0o644}, DataSize: 8 * mib},
	}, Writable, TotalDiskSize(1*mib))
	fsck(t, image)
	sb := readSuperBlock(t, image)
	got := int64(sb.BlocksCountLow) * BlockSize
	require.GreaterOrEqual(t, got, int64(8*mib), "filesystem spans %d bytes, want >= content size", got)
	free := int64(sb.FreeBlocksCountLow) * BlockSize
	require.GreaterOrEqual(t, free, int64(400*mib), "free space = %d bytes, want >= ~512 MiB writable slack", free)
}

// TestTotalDiskSizeInodes pins that the inode table scales with the
// padded filesystem size, not just the file count. A pure compact writer
// would allocate ~one inode per file and leave a large rootfs starved of
// inodes — file-heavy work (kernel builds, big npm trees) then hits
// ENOSPC with blocks still free. We size to mkfs.ext4's default ratio
// (one inode per 16 KiB) instead.
func TestTotalDiskSizeInodes(t *testing.T) {
	const total = 256 * mib
	image := filepath.Join(t.TempDir(), "fs.img")
	buildImage(t, image, []testFile{
		{Path: "etc", File: &File{Mode: format.S_IFDIR | 0o755}},
		{Path: "etc/hostname", File: &File{Mode: 0o644}, Data: []byte("box\n")},
	}, Writable, TotalDiskSize(total))
	fsck(t, image)

	sb := readSuperBlock(t, image)
	// One inode per 16 KiB of disk, with generous slack for group
	// rounding. The point is the count tracks the disk, not the handful
	// of files we wrote (which would yield only a few hundred inodes).
	want := uint32(total / bytesPerInode)
	require.GreaterOrEqual(t, sb.InodesCount, want*3/4,
		"InodesCount = %d, want ~%d (one per %d bytes of a %d-byte disk)",
		sb.InodesCount, want, bytesPerInode, total)
}

// TestReadOnlyContentExact pins the inverse: without Writable, content
// exceeding the request keeps the historical content-exact sizing — no
// slack is added to immutable images.
func TestReadOnlyContentExact(t *testing.T) {
	image := filepath.Join(t.TempDir(), "fs.img")
	buildImage(t, image, []testFile{
		{Path: "big", File: &File{Mode: 0o644}, DataSize: 8 * mib},
	}, TotalDiskSize(1*mib))
	fsck(t, image)
	sb := readSuperBlock(t, image)
	free := int64(sb.FreeBlocksCountLow) * BlockSize
	require.LessOrEqual(t, free, int64(64*mib), "read-only image gained %d bytes of slack; want content-exact", free)
}
