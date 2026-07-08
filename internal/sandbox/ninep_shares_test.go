package sandbox

import "testing"

// TestToolchainCacheSharesPorts locks the port contract the host ninep servers
// (vzd) and the guest manifest both rely on: every cache share gets a distinct,
// non-zero vsock port starting at NinepBasePort, clear of the fixed control
// ports. If these drift, host servers and guest mounts stop agreeing.
func TestToolchainCacheSharesPorts(t *testing.T) {
	shares := ToolchainCacheShares(t.TempDir())
	if len(shares) == 0 {
		t.Fatal("ToolchainCacheShares returned nothing")
	}

	control := map[uint32]bool{1024: true, 1025: true, 1026: true} // pty, time-sync, ssh
	seen := map[uint32]bool{}
	for i, sh := range shares {
		if sh.NinePVSockPort == 0 {
			t.Errorf("%s: NinePVSockPort=0, want non-zero", sh.Tag)
		}
		if want := NinepBasePort + uint32(i); sh.NinePVSockPort != want {
			t.Errorf("%s: port=%d, want %d", sh.Tag, sh.NinePVSockPort, want)
		}
		if control[sh.NinePVSockPort] {
			t.Errorf("%s: port %d collides with a control port", sh.Tag, sh.NinePVSockPort)
		}
		if seen[sh.NinePVSockPort] {
			t.Errorf("%s: duplicate port %d", sh.Tag, sh.NinePVSockPort)
		}
		seen[sh.NinePVSockPort] = true

		// Every cache also keeps a virtio-fs Tag as the older-guest fallback.
		if sh.Tag == "" {
			t.Errorf("cache share at %s has no fallback Tag", sh.GuestPath)
		}
	}
}

// TestToolchainCacheSharesEmptyDir keeps the opt-out contract: no cacheDir,
// no shares (and so no ninep servers), matching the pre-9p behavior.
func TestToolchainCacheSharesEmptyDir(t *testing.T) {
	if got := ToolchainCacheShares(""); got != nil {
		t.Errorf("ToolchainCacheShares(\"\") = %v, want nil", got)
	}
}
