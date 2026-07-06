//go:build darwin

package cow

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// fastClone attempts clonefile(2). clonefile requires dst to not exist,
// so we remove any prior file first. Returns (true, nil) on success,
// (false, nil) when the filesystem doesn't support it, (false, err) on
// real errors.
func fastClone(src, dst string) (bool, error) {
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := unix.Clonefile(src, dst, 0); err != nil {
		if isUnsupported(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func isUnsupported(err error) bool {
	return errors.Is(err, syscall.ENOTSUP) ||
		errors.Is(err, syscall.EOPNOTSUPP) ||
		errors.Is(err, syscall.EXDEV) ||
		errors.Is(err, syscall.EINVAL) ||
		errors.Is(err, syscall.ENOSYS)
}
