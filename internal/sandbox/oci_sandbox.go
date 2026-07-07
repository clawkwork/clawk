package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/guestbuild"
	"github.com/clawkwork/clawk/internal/guestcfg"
	"github.com/clawkwork/clawk/machine"
)

// This file assembles the pieces an OCI-image sandbox boots from. The
// sandbox boots the Kata kernel directly into an injected clawk-init,
// which reads a config disk and starts the vsock pty-agent — no
// cloud-init, no sshd, no distro assumptions. Platform-neutral so the
// manifest and spec logic is testable everywhere; only the vz provider
// wiring is darwin-gated.

// OCICmdline boots the image's flattened rootfs with the injected init.
// hvc0 is the vz virtio-console; root is the OCI-built ext4 on the first
// virtio-blk; clawk.cfg names the manifest config disk. psi=1 enables the
// kernel's pressure-stall information (CONFIG_PSI_DEFAULT_DISABLED is set in
// the Kata kernel) so the guest agent can report /proc/pressure/memory to the
// host balloon controller.
const OCICmdline = "console=hvc0 root=/dev/vda rw psi=1 init=" + guestcfg.InitPath +
	" clawk.cfg=/dev/vdb"

// OCIConfigDiskName is the manifest disk's filename inside a sandbox's
// VM directory.
const OCIConfigDiskName = "guestcfg.img"

// OCIRootFS builds the machine.OCIImage rootfs spec for sb. Every caller
// (provider Create pre-build, vzd boot) must produce the identical value
// or the digest-keyed disk cache misses and a second disk gets built.
func OCIRootFS(sb *config.Sandbox, cacheDir string, bins guestbuild.Binaries) machine.OCIImage {
	return machine.OCIImage{
		Ref:      sb.Image,
		CacheDir: filepath.Join(cacheDir, "oci"),
		SizeMiB:  DefaultDiskSizeGiB << 10,
		Platform: "linux/" + runtime.GOARCH,
		Inject: []machine.InjectFile{
			{GuestPath: guestcfg.InitPath, HostPath: bins.Init, Mode: 0o755},
			{GuestPath: guestcfg.AgentPath, HostPath: bins.Agent, Mode: 0o755},
			{GuestPath: guestcfg.TimeSyncPath, HostPath: bins.TimeSync, Mode: 0o755},
		},
	}
}

// OCIGuestManifest assembles the clawk-init boot manifest for sb: the
// guest-side description of the user, network, mounts, and snapshot
// files clawk-init applies at boot. stateDir is the per-sandbox
// persistent state dir (Claude state), cacheDir the clawk cache
// (toolchain shares), rootDir the clawk root (~/.clawk).
//
// Mount tags MUST stay in lock-step with the share enumeration the vz
// daemon exposes (collectSandboxShares in internal/cli/vzd.go) — a tag
// listed here but not exported by vz fails to mount in the guest, and
// vice versa silently hides a share.
// HasManagedWorktree reports whether sb has at least one non-in-place phase
// worktree — i.e. a worktree clawk created under store.WorktreeDir rather
// than pointing at the user's own repo. Only then is the consolidated
// WorkspaceShareTag mount emitted (an in-place-only sandbox keeps
// WorkspaceRoot a plain guest dir with per-phase sub-mounts). Shared by the
// spec side (collectSandboxShares) and the manifest side (OCIGuestManifest)
// so both agree on whether the parent device exists.
func HasManagedWorktree(sb *config.Sandbox) bool {
	for _, p := range sb.Phases {
		if p.Worktree != "" && !p.InPlace {
			return true
		}
	}
	return false
}

func OCIGuestManifest(sb *config.Sandbox, stateDir, cacheDir, rootDir string) (guestcfg.Manifest, error) {
	user := &guestcfg.User{
		// Mirror the HOST uid/gid: vz virtio-fs presents host files with
		// their host ownership, so a matching guest uid makes the
		// worktree writable without any squashing.
		Name: GuestUser,
		UID:  os.Getuid(),
		GID:  os.Getgid(),
	}
	if sb.NestedVirt {
		user.Groups = append(user.Groups, "kvm")
	}

	m := guestcfg.Manifest{
		Hostname: sb.Name,
		Network: &guestcfg.Network{
			Interface: "eth0",
			Address:   guestIP + "/24",
			Gateway:   gvproxyGateway,
			// gvproxy serves DNS at the gateway address.
			DNS: []string{gvproxyGateway},
			MTU: gvproxyMTU,
		},
		User: user,
		Services: []guestcfg.Service{
			{Name: "pty-agent", Path: guestcfg.AgentPath},
			{Name: "time-sync", Path: guestcfg.TimeSyncPath},
		},
	}

	// Consolidated worktree mount: every managed (non-in-place) worktree for
	// this sandbox lives under one host parent (store.WorktreeDir), which
	// collectSandboxShares exposes as a single virtio-fs device. Mount that
	// parent once at WorkspaceRoot and each worktree reappears as
	// WorkspaceRoot/<repo> — identical guest paths, one PCIe device instead
	// of one per phase. Emitted FIRST so clawk-init mounts the parent before
	// any in-place sub-mount lands on top of it. Mirror of the worktree
	// branch in collectSandboxShares (internal/cli); the two must agree on
	// tags or the guest tries to mount a device vz never exposed.
	if HasManagedWorktree(sb) {
		m.Mounts = append(m.Mounts, guestcfg.Mount{
			Tag: WorkspaceShareTag, Path: WorkspaceRoot,
		})
	}

	// In-place phases point their worktree at the user's real repo, which
	// lives OUTSIDE the worktree parent — so each keeps its own device,
	// sub-mounted at WorkspaceRoot/<name> on top of the consolidated parent.
	// Non-in-place phases are already covered by that parent; they only add a
	// src_<repo> alias at the repo's original host path so the worktree's
	// .git backpointer (an absolute path) resolves identically in the guest.
	seenRepos := map[string]bool{}
	for _, p := range sb.Phases {
		if p.Worktree == "" {
			continue
		}
		if p.InPlace {
			// Tag is bounded to the virtio-fs limit; the guest mount
			// point keeps the readable basename.
			wtBase := filepath.Base(p.Worktree)
			m.Mounts = append(m.Mounts, guestcfg.Mount{
				Tag: InPlaceWorktreeTag(p.Worktree), Path: WorkspaceRoot + "/" + wtBase,
			})
			continue
		}
		if seenRepos[p.Repo] {
			continue
		}
		seenRepos[p.Repo] = true
		m.Mounts = append(m.Mounts, guestcfg.Mount{
			Tag: RepoShareTag(p.Repo), Path: p.Repo,
		})
	}

	// Host shares, in parent-before-child order: the whole ~/.claude
	// state mount must precede the skills/agents/commands sub-mounts or
	// the latter end up shadowed.
	shares := append([]HostShare{}, PersistentClaudeShares(stateDir)...)
	shares = append(shares, DefaultHostShares()...)
	shares = append(shares, ToolchainCacheShares(cacheDir)...)
	shares = append(shares, UserHostShares(sb.Shares)...)
	for _, sh := range shares {
		m.Mounts = append(m.Mounts, guestcfg.Mount{
			Tag: sh.Tag, Path: sh.GuestPath, ReadOnly: sh.ReadOnly,
		})
	}

	// Snapshot files — the write_files: equivalent.
	token, _ := LoadOAuthToken(rootDir)
	files := append(DefaultHostFiles(rootDir), WorkspaceDocFile(sb))
	files = append(files, ClaudeJSONMarkerFile(sb.Phases, token != ""))
	if envFile, ok := EnvFile(sb); ok {
		files = append(files, envFile)
	}
	for _, f := range files {
		data := f.Content
		if data == nil {
			var err error
			data, err = os.ReadFile(f.HostPath)
			if err != nil {
				return guestcfg.Manifest{}, fmt.Errorf("reading %s: %w", f.HostPath, err)
			}
		}
		owner := ""
		if strings.HasPrefix(f.Owner, GuestUser+":") {
			owner = "user"
		}
		m.Files = append(m.Files, guestcfg.File{
			Path: f.GuestPath, Mode: f.Mode, Owner: owner, Content: data,
		})
	}
	return m, nil
}

// WriteOCIGuestConfig (re)writes the manifest config disk for sb into
// vmDir. Called before every boot so share/file changes propagate
// without a destroy.
func WriteOCIGuestConfig(sb *config.Sandbox, vmDir, stateDir, cacheDir, rootDir string) error {
	m, err := OCIGuestManifest(sb, stateDir, cacheDir, rootDir)
	if err != nil {
		return err
	}
	return guestcfg.WriteDisk(m, filepath.Join(vmDir, OCIConfigDiskName))
}
