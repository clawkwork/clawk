package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"unicode/utf8"
)

// maxVirtioFSTagBytes is Apple's Virtualization.framework limit on a
// virtio-fs device tag (VZVirtioFileSystemDeviceConfiguration): 36 bytes.
// A tag over the limit makes VZ reject the whole VM at preparation —
// before the kernel boots, so there's no vz.pid and the daemon dies —
// which is why an over-long tag surfaces to the user only as a boot
// timeout. Every generated repo/worktree tag must pass through
// boundedTag so this can't happen.
const maxVirtioFSTagBytes = 36

// tagHashHexLen is how many hex chars of the SHA-256 the overflow form
// keeps: 32 bits, collision-resistant across one sandbox's handful of
// shares while leaving most of the budget for a readable name prefix.
const tagHashHexLen = 8

// boundedTag returns prefix+name when it fits the virtio-fs tag limit.
// Otherwise it keeps the *start* of the name and appends a hash, so the
// tag stays identifiable ("src_my-long-service_1a2b3c4d") instead of an
// opaque digest you can't map back to a repo:
//
//	prefix + name[:budget] + "_" + <8 hex of SHA-256(key)>   (<= 36 bytes)
//
// The name is truncated on a byte budget (the limit is bytes, not runes)
// but never mid-rune. key must be identical wherever the same share's
// tag is computed (the guest manifest in oci_sandbox.go AND the host
// device list in internal/cli/shares.go), or the guest would mount a tag
// no device exposes — so key is the full path, which also gives the hash
// its uniqueness. Names that already fit are returned unchanged, so
// existing sandboxes keep byte-identical tags across an upgrade.
func boundedTag(prefix, name, key string) string {
	if tag := prefix + name; len(tag) <= maxVirtioFSTagBytes {
		return tag
	}
	sum := sha256.Sum256([]byte(key))
	h := hex.EncodeToString(sum[:])[:tagHashHexLen]
	budget := maxVirtioFSTagBytes - len(prefix) - 1 - len(h) // "_" + hash
	if budget < 0 {
		budget = 0
	}
	return prefix + truncateBytesOnRune(name, budget) + "_" + h
}

// truncateBytesOnRune returns at most n bytes of s, backing off to the
// previous rune boundary so it never emits an invalid UTF-8 fragment.
func truncateBytesOnRune(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// RepoShareTag is the virtio-fs tag for a managed repo's src_ alias (the
// device that re-exposes the repo at its original host path so worktree
// .git backpointers resolve in the guest). Keyed on the full repo path.
func RepoShareTag(repoPath string) string {
	return boundedTag("src_", filepath.Base(repoPath), repoPath)
}

// InPlaceWorktreeTag is the virtio-fs tag for an in-place worktree device
// (from `clawk here`, where the worktree is the user's own directory).
// Keyed on the full worktree path. Note the guest mount POINT keeps the
// readable basename; only the opaque tag is bounded.
func InPlaceWorktreeTag(worktreePath string) string {
	return boundedTag("", filepath.Base(worktreePath), worktreePath)
}
