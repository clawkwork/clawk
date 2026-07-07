package cli

import (
	"os"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/clawkwork/clawk/machine"
)

// collectSandboxShares enumerates every virtio-fs directory the sandbox wants
// mounted, as machine.Share entries for the vz backend. It MUST stay in
// lock-step with the guest mount manifest (sandbox.OCIGuestManifest): a tag
// present in one but not the other means the guest tries to mount a device vz
// never exposed, or vz exposes a device the guest never mounts.
//
// Worktree consolidation: every managed (non-in-place) worktree lives under a
// single host parent (store.WorktreeDir), so we expose that parent as ONE
// virtio-fs device tagged WorkspaceShareTag rather than one device per phase.
// Apple's Virtualization.framework caps the number of virtio/PCIe devices on
// a VM (see machine/vz); a sandbox with many repos used to blow past the
// ceiling and fail to start with an opaque internal error. Each worktree
// still appears at WorkspaceRoot/<repo> through this one device.
//
// In-place phases (from `clawk here`) point their worktree at the user's own
// repo, which lives OUTSIDE the worktree parent — so each keeps its own
// device, sub-mounted on top of the consolidated parent. They emit no
// src_<repo> alias: Repo == Worktree there, and exposing one host path under
// two virtiofs tags has bitten us with mount conflicts.
func collectSandboxShares(sb *config.Sandbox) []machine.Share {
	var out []machine.Share

	// Consolidated worktree parent, exposed once at WorkspaceRoot. MkdirAll
	// is defensive: worktree.Add already created it at `clawk here`/`run`
	// time, but virtio-fs refuses a missing source path, so if it somehow
	// can't be created we skip the share rather than fail the whole spec —
	// same posture as ToolchainCacheShares.
	if sandbox.HasManagedWorktree(sb) {
		wtDir := store.WorktreeDir(sb.Name)
		if err := os.MkdirAll(wtDir, 0o755); err == nil {
			out = append(out, machine.Share{
				HostPath: wtDir,
				Tag:      sandbox.WorkspaceShareTag,
			})
		}
	}

	// In-place worktree devices, plus one src_<repo> alias per distinct
	// managed repo (git-worktree metadata in .git/worktrees/ uses absolute
	// host paths that must resolve identically inside the VM).
	seenRepos := make(map[string]bool)
	for _, p := range sb.Phases {
		if p.Worktree == "" {
			continue
		}
		if p.InPlace {
			out = append(out, machine.Share{
				HostPath: p.Worktree,
				Tag:      sandbox.InPlaceWorktreeTag(p.Worktree),
			})
			continue
		}
		if !seenRepos[p.Repo] {
			seenRepos[p.Repo] = true
			out = append(out, machine.Share{
				HostPath: p.Repo,
				Tag:      sandbox.RepoShareTag(p.Repo),
			})
		}
	}

	// Per-sandbox persistent state — whole ~/.claude/, pre-seeded with
	// settings.json/CLAUDE.md/.credentials.json by SeedClaudeStateDir
	// (called from the provider Create path). Host dir lives outside
	// VMDir so destroy cannot touch it; the same sandbox name always
	// sees the same state dir across recreate cycles. MUST come before
	// DefaultHostShares so the agents/commands sub-mounts land on top
	// of the parent rather than being shadowed under it.
	for _, sh := range sandbox.PersistentClaudeShares(store.StateDir(sb.Name)) {
		out = append(out, machine.Share{
			HostPath: sh.HostPath,
			Tag:      sh.Tag,
			ReadOnly: sh.ReadOnly,
		})
	}
	for _, sh := range sandbox.DefaultHostShares() {
		out = append(out, machine.Share{
			HostPath: sh.HostPath,
			Tag:      sh.Tag,
			ReadOnly: sh.ReadOnly,
		})
	}
	// Toolchain dependency caches (Go module, Cargo registry/git).
	// MUST mirror the guest manifest's mount list (OCIGuestManifest):
	// omitting a tag here means vz never exposes the device the guest
	// tries to mount, so every `go build`/`cargo` download lands in the
	// rootfs instead of the shared host cache — ballooning each per-VM
	// disk. Use the same CacheDir the manifest does.
	for _, sh := range sandbox.ToolchainCacheShares(store.CacheDir()) {
		out = append(out, machine.Share{
			HostPath: sh.HostPath,
			Tag:      sh.Tag,
			ReadOnly: sh.ReadOnly,
		})
	}
	// User-declared shares from `shares (...)` in clawk.mod. Same tag
	// derivation as the guest manifest so both layers agree on which
	// device exposes which mountpoint.
	for _, sh := range sandbox.UserHostShares(sb.Shares) {
		out = append(out, machine.Share{
			HostPath: sh.HostPath,
			Tag:      sh.Tag,
			ReadOnly: sh.ReadOnly,
		})
	}
	return out
}
