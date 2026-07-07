package sandbox

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRepoShareTagWithinVirtioFSLimit(t *testing.T) {
	cases := []string{
		"/x/repo",                                    // short: unchanged
		"/x/" + strings.Repeat("a", 32),              // 32-char name + src_ = 36, exactly the limit
		"/x/" + strings.Repeat("a", 33),              // 33-char name + src_ = 37, just over
		"/some/deep/path/" + strings.Repeat("b", 36), // 36-char name
		"/x/" + strings.Repeat("é", 20),              // multibyte: bytes, not runes, matter
	}
	for _, repo := range cases {
		tag := RepoShareTag(repo)
		require.LessOrEqualf(t, len(tag), maxVirtioFSTagBytes,
			"tag %q for repo %q is %d bytes, over the %d-byte virtio-fs limit",
			tag, repo, len(tag), maxVirtioFSTagBytes)
	}
}

func TestShareTagsAreStableAndReadableWhenShort(t *testing.T) {
	// Short names keep the readable form (and stay byte-identical across
	// upgrades, so existing sandboxes don't churn their tags).
	require.Equal(t, "src_myrepo", RepoShareTag("/home/me/myrepo"))
	require.Equal(t, "myrepo", InPlaceWorktreeTag("/home/me/myrepo"))
}

func TestOverLongTagKeepsNamePrefix(t *testing.T) {
	// An over-long name must stay identifiable — keep the start of the
	// repo name, not just an opaque hash.
	repo := "/home/me/reporting-service-" + strings.Repeat("x", 30)
	tag := RepoShareTag(repo)
	require.LessOrEqual(t, len(tag), maxVirtioFSTagBytes)
	require.True(t, strings.HasPrefix(tag, "src_reporting-service-"),
		"overflow tag should keep the repo-name prefix, got %q", tag)
}

func TestShareTagDeterministicPerPath(t *testing.T) {
	// The manifest and the device list compute the tag independently from
	// the same path; they must land on the same string or the guest mounts
	// a tag no device exposes.
	long := "/repos/" + strings.Repeat("z", 40)
	require.Equal(t, RepoShareTag(long), RepoShareTag(long))
	require.NotEqual(t, RepoShareTag("/a/"+strings.Repeat("z", 40)),
		RepoShareTag("/b/"+strings.Repeat("z", 40)),
		"distinct repo paths must not collide")
}
