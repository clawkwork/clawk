package sandbox

// GuestUser is the non-root Linux user the agent runs as inside every
// sandbox. "agent" mirrors Docker AI Sandbox's convention
// (docker/sandbox-templates:claude-code, UID 1000, /home/agent), which
// keeps our docs and the wider ecosystem's docs interchangeable.
//
// All user-facing paths inside the VM are derived from this constant
// (GuestHome, WorkspaceRoot, ~/.claude/... mounts, etc.), so if we ever
// switch the base image's user, nothing else has to change.
const GuestUser = "agent"

// GuestHome is the agent user's home dir. Kept as a constant (not
// `/home/` + GuestUser expression) so it can be concatenated at compile
// time in other constants.
const GuestHome = "/home/" + GuestUser

// WorkspaceRoot is where worktrees get mounted inside the guest. Lives
// under the agent user's home (same convention as Docker AI Sandbox),
// not at the filesystem root — many agent tools (e.g., the Claude Code
// skills installer) create dotfiles next to the current working
// directory, and $HOME is writable by default. A root-owned
// /workspace causes "EACCES mkdir /workspace/.agents" errors.
const WorkspaceRoot = GuestHome + "/workspace"

// WorkspaceShareTag is the virtio-fs tag for the single consolidated
// worktree share: the host worktree parent (store.WorktreeDir) mounted at
// WorkspaceRoot. Every managed (non-in-place) worktree lives under that one
// host dir, so folding them into a single virtio-fs device — instead of one
// device per phase — keeps sandboxes with many repos under the device
// ceiling Apple's Virtualization.framework enforces (see machine/vz). Both
// the vz share list (collectSandboxShares) and the guest mount manifest
// (OCIGuestManifest) key off this exact tag, so they must never diverge.
const WorkspaceShareTag = "workspace"
