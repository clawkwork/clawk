package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/sessions"
)

// Session history (git-modeled Claude Code conversations) is a vz-only
// feature for now: it relies on the per-sandbox ~/.claude state dir being a
// virtio-fs LIVE mount, so the guest writes transcripts straight into the
// host git working tree. Firecracker bakes state into the rootfs with no
// live mount and would need a copy-out step first — out of scope here.
//
// These helpers are the seam between the lifecycle commands and the
// platform-neutral internal/sessions package. They are best-effort: a git
// hiccup must never block bringing a sandbox up or tearing it down, so
// callers warn-and-continue rather than fail.

// sessionHistoryEnabled reports whether session history applies to sb.
func sessionHistoryEnabled(sb *config.Sandbox) bool {
	switch sb.Provider {
	case config.ProviderVZ, "":
		return len(sb.Phases) > 0
	default:
		return false
	}
}

// sessionClaudeDir is the host working tree mounted into the guest as
// ~/.claude — the same path PersistentClaudeShares serves.
func sessionClaudeDir(sb *config.Sandbox) string {
	return filepath.Join(store.StateDir(sb.Name), "claude")
}

// ensureSessionProject returns the sandbox's history-repo id, computing it
// from the phase repo set and persisting it on first use so it stays stable
// even if phases are later added or removed.
func ensureSessionProject(sb *config.Sandbox) (string, error) {
	if sb.SessionProject != "" {
		return sb.SessionProject, nil
	}
	repos := make([]string, 0, len(sb.Phases))
	for _, p := range sb.Phases {
		repos = append(repos, p.Repo)
	}
	sb.SessionProject = sessions.ProjectID(repos)
	if err := store.Save(sb); err != nil {
		return "", fmt.Errorf("persisting session project id: %w", err)
	}
	return sb.SessionProject, nil
}

// prepareSessionHistory makes the sandbox's ~/.claude state dir a git working
// tree on the sandbox branch and folds in the project's accumulated history,
// so the guest boots with prior transcripts and memory. Called before the
// provider mounts the dir. Best-effort.
func prepareSessionHistory(sb *config.Sandbox) {
	if !sessionHistoryEnabled(sb) {
		return
	}
	projectID, err := ensureSessionProject(sb)
	if err != nil {
		warnSession("preparing session history", err)
		return
	}
	bare, err := sessions.EnsureBare(store.HistoryDir(), projectID)
	if err != nil {
		warnSession("preparing session history", err)
		return
	}
	if err := sessions.Prepare(bare, sessionClaudeDir(sb), sessions.BranchFor(sb.Name)); err != nil {
		warnSession("seeding session history", err)
	}
}

// preserveSessionHistory commits and pushes the sandbox's branch on teardown.
// Under the explicit-merge model it does NOT touch the project's canonical
// `main` — destroying a throwaway VM keeps its sessions on vm/<name> (safe,
// publishable later) without polluting the shared history. Best-effort.
//
// It computes the id if missing rather than bailing, so a pre-feature sandbox
// destroyed without ever being booted still has its on-disk transcripts
// preserved (Preserve initializes the working tree as needed).
func preserveSessionHistory(sb *config.Sandbox) {
	if !sessionHistoryEnabled(sb) {
		return
	}
	projectID, err := ensureSessionProject(sb)
	if err != nil {
		warnSession("preserving session history", err)
		return
	}
	bare, err := sessions.EnsureBare(store.HistoryDir(), projectID)
	if err != nil {
		warnSession("preserving session history", err)
		return
	}
	if err := sessions.Preserve(bare, sessionClaudeDir(sb), sessions.BranchFor(sb.Name)); err != nil {
		warnSession("preserving session history", err)
	}
}

// refreshSessionHistory syncs a running sandbox's history on agent attach: it
// pulls in sessions published to the project since boot and preserves this
// VM's branch — but does NOT publish to main (that's the explicit
// `clawk sessions merge`). This is what lets a long-lived `clawk here`
// sandbox — which may stay up for weeks and so never re-runs
// prepareSessionHistory — still learn about newly-published sessions.
//
// Guarded on the state dir already being a git repo: a sandbox that has never
// booted under this feature (or a unit-test sandbox) has no working tree to
// sync, so this is a clean no-op rather than initializing one mid-attach.
func refreshSessionHistory(sb *config.Sandbox) {
	if !sessionHistoryEnabled(sb) {
		return
	}
	claudeDir := sessionClaudeDir(sb)
	if _, err := os.Stat(filepath.Join(claudeDir, ".git")); err != nil {
		return
	}
	projectID, err := ensureSessionProject(sb)
	if err != nil {
		warnSession("syncing session history", err)
		return
	}
	bare, err := sessions.EnsureBare(store.HistoryDir(), projectID)
	if err != nil {
		warnSession("syncing session history", err)
		return
	}
	if err := sessions.Refresh(bare, claudeDir, sessions.BranchFor(sb.Name)); err != nil {
		warnSession("syncing session history", err)
	}
}

func warnSession(what string, err error) {
	fmt.Fprintf(os.Stderr, "warning: %s: %v\n", what, err)
}
