// Package cow materializes a file as a copy-on-write copy of another.
//
// On filesystems that support block-level reflink (APFS on macOS,
// btrfs/xfs-with-reflink/bcachefs on Linux) the clone is near-instant
// and uses zero extra storage until one side diverges. On filesystems
// that don't support it — ext4, tmpfs, NFS — Clone falls back to a
// sparse-preserving byte copy.
//
// The primary consumer is machine/oci, which needs every Machine to
// get its own writable disk without mutating the shared pull cache.
package cow

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// Clone materializes dst as a copy of src using the fastest path the
// filesystem supports. If src and dst resolve to the same path, Clone
// is a no-op. If dst exists, it is replaced.
//
// The parent directory of dst must already exist.
func Clone(src, dst string) error {
	if src == dst {
		return nil
	}
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("cow: source %q: %w", src, err)
	}
	ok, err := fastClone(src, dst)
	if err != nil {
		return fmt.Errorf("cow: fast-clone: %w", err)
	}
	if ok {
		return nil
	}
	return fallbackCopy(src, dst)
}

// fallbackCopy is the no-reflink path — a byte copy that skips all-zero
// chunks so sparse disk images stay sparse. A padded ext4 rootfs is
// mostly hole; a naive io.Copy materializes every zero and can multiply
// the physical footprint by orders of magnitude. Preserves the source's
// mode but not ownership, xattrs, or timestamps.
func fallbackCopy(src, dst string) error {
	srcF, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening source: %w", err)
	}
	defer srcF.Close()

	info, err := srcF.Stat()
	if err != nil {
		return fmt.Errorf("stat source: %w", err)
	}

	dstF, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("opening dest: %w", err)
	}
	if err := sparseCopy(dstF, srcF, info.Size()); err != nil {
		dstF.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("copying: %w", err)
	}
	if err := dstF.Close(); err != nil {
		return fmt.Errorf("closing dest: %w", err)
	}
	return nil
}

// sparseCopy copies size bytes from src to dst, writing only non-zero
// chunks at their offsets and finishing with a Truncate so trailing holes
// (and the file's logical length) materialize without data writes.
func sparseCopy(dst *os.File, src io.Reader, size int64) error {
	buf := make([]byte, 1<<20)
	var pos int64
	for {
		n, err := io.ReadFull(src, buf)
		if n > 0 && !allZero(buf[:n]) {
			if _, werr := dst.WriteAt(buf[:n], pos); werr != nil {
				return werr
			}
		}
		pos += int64(n)
		switch {
		case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
			if pos != size {
				return fmt.Errorf("source changed size mid-copy: read %d bytes, expected %d", pos, size)
			}
			return dst.Truncate(size)
		case err != nil:
			return err
		}
	}
}

// allZero reports whether b contains only zero bytes.
func allZero(b []byte) bool {
	for len(b) >= 8 {
		if b[0]|b[1]|b[2]|b[3]|b[4]|b[5]|b[6]|b[7] != 0 {
			return false
		}
		b = b[8:]
	}
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
