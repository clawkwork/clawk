//go:build linux

package cow

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// fastClone attempts an FICLONE ioctl. Returns (true, nil) on success,
// (false, nil) when the filesystem doesn't support reflink (so the caller
// can fall back), or (false, err) on genuine IO errors.
func fastClone(src, dst string) (bool, error) {
	srcF, err := os.Open(src)
	if err != nil {
		return false, err
	}
	defer srcF.Close()

	// FICLONE requires dst to exist and be empty; O_CREAT|O_TRUNC gets
	// us that. On failure we unlink so we don't leave a zero-byte file
	// that masks the fallback path.
	dstF, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return false, err
	}
	cloneErr := unix.IoctlFileClone(int(dstF.Fd()), int(srcF.Fd()))
	closeErr := dstF.Close()
	if cloneErr != nil {
		_ = os.Remove(dst)
		if isUnsupported(cloneErr) {
			return false, nil
		}
		return false, cloneErr
	}
	return true, closeErr
}

// isUnsupported classifies FICLONE errors that should trigger the caller's
// fallback path rather than surface as a hard error. See ioctl_ficlone(2).
func isUnsupported(err error) bool {
	return errors.Is(err, syscall.ENOTSUP) ||
		errors.Is(err, syscall.EOPNOTSUPP) ||
		errors.Is(err, syscall.EINVAL) ||
		errors.Is(err, syscall.EXDEV) ||
		errors.Is(err, syscall.ENOSYS)
}
