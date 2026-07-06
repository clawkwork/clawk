//go:build !linux && !darwin

package cow

// fastClone reports "not supported" on platforms without a reflink
// syscall Clone knows about; callers always fall back to the copy path.
func fastClone(_, _ string) (bool, error) { return false, nil }
