package sandbox

import (
	"strings"
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/guestbuild"
	"github.com/clawkwork/clawk/internal/guestcfg"
	"github.com/stretchr/testify/require"
)

func TestOCIGuestManifest(t *testing.T) {
	sb := &config.Sandbox{
		Name:       "box",
		Image:      "golang:1.25",
		NestedVirt: true,
		Phases: []config.Phase{
			{Worktree: "/Users/u/wt/feature-x", Repo: "/Users/u/code/proj"},
			{Worktree: "/Users/u/code/here", Repo: "/Users/u/code/here", InPlace: true},
		},
		Shares: []config.HostShare{
			{HostPath: "/Users/u/.aws", GuestPath: GuestHome + "/.aws", ReadOnly: true},
		},
	}

	m, err := OCIGuestManifest(sb, t.TempDir(), "", t.TempDir())
	require.NoErrorf(t, err, "OCIGuestManifest")

	if m.Hostname != "box" {
		t.Errorf("hostname = %q", m.Hostname)
	}
	if m.Network == nil || m.Network.Address != "192.168.127.2/24" || m.Network.Gateway != "192.168.127.1" {
		t.Errorf("network = %+v", m.Network)
	}
	if m.User == nil || m.User.Name != GuestUser || m.User.UID == 0 {
		t.Errorf("user = %+v (uid must mirror the host, not root)", m.User)
	}
	if len(m.User.Groups) != 1 || m.User.Groups[0] != "kvm" {
		t.Errorf("nested sandbox should request kvm group, got %v", m.User.Groups)
	}

	mounts := map[string]guestcfg.Mount{}
	for _, mt := range m.Mounts {
		mounts[mt.Tag] = mt
	}
	// Managed (non-in-place) worktrees are folded into ONE parent share
	// mounted at WorkspaceRoot — no per-worktree device — so the guest sees
	// each as a WorkspaceRoot/<repo> subdir of that single virtio-fs mount.
	if mt, ok := mounts[WorkspaceShareTag]; !ok || mt.Path != WorkspaceRoot {
		t.Errorf("consolidated workspace mount missing/wrong: %+v", mounts)
	}
	if _, ok := mounts["feature-x"]; ok {
		t.Error("managed worktree must not get its own device; it rides the workspace parent")
	}
	// The in-place phase's repo lives outside the worktree parent, so it
	// keeps its own device sub-mounted at WorkspaceRoot/<name>.
	if mt, ok := mounts["here"]; !ok || mt.Path != WorkspaceRoot+"/here" {
		t.Errorf("in-place worktree sub-mount missing/wrong: %+v", mounts)
	}
	if mt, ok := mounts["src_proj"]; !ok || mt.Path != "/Users/u/code/proj" {
		t.Errorf("source repo mount missing/wrong: %+v", mounts)
	}
	if _, ok := mounts["src_here"]; ok {
		t.Error("InPlace phase must not get a source alias mount")
	}

	// clawk-init mounts in declaration order, so the workspace parent MUST
	// precede the in-place sub-mount that lands on top of it — otherwise the
	// parent mount shadows the already-mounted child.
	wsIdx, hereIdx := -1, -1
	for i, mt := range m.Mounts {
		switch mt.Tag {
		case WorkspaceShareTag:
			wsIdx = i
		case "here":
			hereIdx = i
		}
	}
	if wsIdx < 0 || hereIdx < 0 || wsIdx > hereIdx {
		t.Errorf("workspace parent (idx %d) must be mounted before in-place sub-mount (idx %d)", wsIdx, hereIdx)
	}
	if mt, ok := mounts["claude_home"]; !ok || mt.Path != GuestHome+"/.claude" {
		// PersistentClaudeShares' tag; if its tag ever changes this test
		// must change together with collectSandboxShares.
		t.Errorf("claude state mount missing/wrong: %+v", mounts)
	}

	var hasUserShare bool
	for _, mt := range m.Mounts {
		if mt.Path == GuestHome+"/.aws" && mt.ReadOnly {
			hasUserShare = true
		}
	}
	if !hasUserShare {
		t.Errorf("user share missing: %+v", m.Mounts)
	}

	wantServices := map[string]string{
		"pty-agent": guestcfg.AgentPath,
		"time-sync": guestcfg.TimeSyncPath,
	}
	for _, svc := range m.Services {
		if wantServices[svc.Name] != svc.Path {
			t.Errorf("service %s path = %q", svc.Name, svc.Path)
		}
		delete(wantServices, svc.Name)
	}
	if len(wantServices) != 0 {
		t.Errorf("services missing: %v", wantServices)
	}

	// The workspace doc must land as a file owned by the agent user.
	var foundDoc bool
	for _, f := range m.Files {
		if strings.HasSuffix(f.Path, "/CLAUDE.md") && f.Owner == "user" {
			foundDoc = true
		}
	}
	if !foundDoc {
		t.Errorf("workspace CLAUDE.md file missing or not user-owned")
	}
}

func TestHasManagedWorktree(t *testing.T) {
	cases := []struct {
		name   string
		phases []config.Phase
		want   bool
	}{
		{"no phases", nil, false},
		{"in-place only", []config.Phase{{Worktree: "/r", Repo: "/r", InPlace: true}}, false},
		{"managed", []config.Phase{{Worktree: "/wt/r", Repo: "/r"}}, true},
		{"mixed", []config.Phase{
			{Worktree: "/r", Repo: "/r", InPlace: true},
			{Worktree: "/wt/x", Repo: "/x"},
		}, true},
		{"empty worktree ignored", []config.Phase{{Repo: "/r"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasManagedWorktree(&config.Sandbox{Phases: tc.phases}); got != tc.want {
				t.Errorf("HasManagedWorktree = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOCIRootFS(t *testing.T) {
	sb := &config.Sandbox{Name: "box", Image: "alpine:3.20"}
	bins := guestbuild.Binaries{Init: "/cache/i", Agent: "/cache/a", TimeSync: "/cache/t"}
	r := OCIRootFS(sb, "/cache", bins)
	if r.Ref != "alpine:3.20" || r.CacheDir != "/cache/oci" {
		t.Errorf("rootfs = %+v", r)
	}
	if r.SizeMiB != DefaultDiskSizeGiB<<10 {
		t.Errorf("SizeMiB = %d, want %d", r.SizeMiB, DefaultDiskSizeGiB<<10)
	}
	if len(r.Inject) != 3 || r.Inject[0].GuestPath != guestcfg.InitPath {
		t.Errorf("inject = %+v", r.Inject)
	}
}
