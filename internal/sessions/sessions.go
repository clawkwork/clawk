// Package sessions models a coding agent's Claude Code conversation history
// as a git repository so it survives VM recreates and follows a project
// across sandboxes.
//
// The model:
//
//   - One bare "history" repo per project at <historyRoot>/<projectID>.git.
//     Its `main` branch is the canonical, accumulated history.
//   - Each sandbox checks out its own branch (vm/<sandbox>) into the
//     per-sandbox state dir that the provider already mounts at the guest's
//     ~/.claude. The mounted dir simply becomes a git working tree.
//   - On boot we fold the project's `main` into the sandbox branch, so a
//     fresh sandbox for the same project comes up with prior transcripts and
//     memory in its resume picker. On teardown we merge the sandbox branch
//     back into `main`.
//
// Only conversation transcripts (projects/**, which nests Claude Code's
// per-project memory/) are tracked — never secrets or host-seeded config.
// See the .gitignore written by Prepare.
//
// This package is host-side and platform-neutral: it shells out to `git`
// (matching internal/worktree and internal/config) and is wired into the
// lifecycle by internal/cli. It is currently used by the vz provider only;
// firecracker (no virtio-fs live mount) needs a copy-out step before this
// applies, which is deliberately out of scope here.
package sessions

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// branchPrefix namespaces per-sandbox branches so a sandbox can never
// collide with the canonical `main` ref (e.g. a sandbox literally named
// "main"), and so `git log` in the history repo reads as "vm/<name>".
const branchPrefix = "vm/"

// gitignore is written into every sandbox working tree. It versions ONLY
// conversation transcripts and auto-memory (which lives under
// projects/<encoded>/memory/), plus the two control files. Everything else
// under ~/.claude — credentials, the OAuth token, host-seeded settings.json
// and CLAUDE.md, the skills/agents/commands live mounts, caches and history
// — stays untracked. This is the invariant that keeps the repo safe to ever
// push to a remote (design §4.4).
const gitignore = `# Managed by clawk (internal/sessions). Version conversation transcripts
# and auto-memory only — never secrets or host-seeded config.
/*
!/.gitattributes
!/.gitignore
!/projects/
!/memory/
`

// gitattributes makes merges conflict-free for the only files that can be
// touched on two branches at once. Transcripts are append-only and
// single-owner per session id, so they never truly conflict, but a union
// merge is a safe fallback. MEMORY.md is a shared index that genuinely can
// gain lines on different VMs — union keeps every VM's additions. `union`
// is a built-in git driver, so no .git/config registration is needed; it
// only has to be a TRACKED file so every merge worktree sees it.
const gitattributes = `*.jsonl merge=union
**/memory/MEMORY.md merge=union
`

// ProjectID returns a stable, filesystem-safe identifier for the set of
// repos a sandbox works on. Sandboxes that share the same repo set share
// one history repo (and therefore one `main`); per design §6 this keys
// history to the workspace, not to any single repo.
//
// The id is "<label>-<hash>": a human-readable label (the sole repo's
// basename, or "ws" for a multi-repo workspace) for debuggability, plus a
// hash of the absolute, sorted, de-duplicated repo paths for uniqueness.
//
// FROZEN: ids name history repos on disk (~/.clawk/history/<id>.git) and
// are recorded on sandbox records. Changing the label rule, the hash
// algorithm, or the normalization orphans every existing transcript —
// any change needs a migration that re-keys the history dir.
func ProjectID(repoPaths []string) string {
	uniq := make(map[string]struct{}, len(repoPaths))
	var paths []string
	for _, p := range repoPaths {
		if p == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = p
		}
		if _, ok := uniq[abs]; ok {
			continue
		}
		uniq[abs] = struct{}{}
		paths = append(paths, abs)
	}
	sort.Strings(paths)

	sum := sha256.Sum256([]byte(strings.Join(paths, "\n")))
	hash := hex.EncodeToString(sum[:6]) // 12 hex chars — ample for a per-host namespace

	label := "ws"
	if len(paths) == 1 {
		label = sanitizeLabel(filepath.Base(paths[0]))
	}
	if label == "" {
		label = "project"
	}
	return label + "-" + hash
}

// sanitizeLabel keeps a repo basename readable but safe as a path component,
// collapsing anything outside [A-Za-z0-9._-] to '-'.
func sanitizeLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// BranchFor returns the per-sandbox branch name for a sandbox.
func BranchFor(sandboxName string) string { return branchPrefix + sandboxName }

// barePath is the on-disk location of a project's bare history repo.
func barePath(historyRoot, projectID string) string {
	return filepath.Join(historyRoot, projectID+".git")
}

// EnsureBare creates the bare history repo for a project if it does not yet
// exist and returns its path. `git init --bare` is idempotent, so this is
// safe to call on every boot. The repo starts with no refs; `main` is
// created lazily by the first Publish (a project's very first sandbox has
// nothing to seed from).
func EnsureBare(historyRoot, projectID string) (string, error) {
	if err := os.MkdirAll(historyRoot, 0o755); err != nil {
		return "", fmt.Errorf("creating history root: %w", err)
	}
	bare := barePath(historyRoot, projectID)
	if _, err := os.Stat(filepath.Join(bare, "HEAD")); err == nil {
		return bare, nil // already initialized
	}
	if _, err := git("", "init", "--bare", bare); err != nil {
		return "", fmt.Errorf("init bare history repo: %w", err)
	}
	// A local identity so merge commits in Publish's worktree succeed
	// regardless of host git config (worktrees inherit the bare repo's config).
	_, _ = git(bare, "config", "user.name", "clawk")
	_, _ = git(bare, "config", "user.email", "clawk@localhost")
	return bare, nil
}

// Prepare makes claudeDir a git working tree on the sandbox's branch and
// folds in the project's accumulated `main` so the sandbox boots with prior
// history. It is idempotent and runs on every `up`:
//
//  1. `git init` claudeDir if needed (safe on a dir that already holds a
//     previous sandbox's state — `git init` never deletes files).
//  2. Write the control files (.gitignore/.gitattributes) and point `origin`
//     at the bare repo.
//  3. Check out the sandbox branch, commit whatever transcripts already sit
//     in the tree (the previous session's tail), then merge origin/main in.
//
// The merge — never a checkout — is what brings prior history without ever
// clobbering files already on disk: new transcript files are added, and the
// union driver resolves the rare overlapping MEMORY.md.
func Prepare(bare, claudeDir, branch string) error {
	if err := ensureWorkingTree(bare, claudeDir, branch); err != nil {
		return err
	}

	// Capture the previous session's transcripts as this branch's tip so the
	// subsequent merge has a clean tree to work against. On an existing
	// pre-feature sandbox this is the migration step: its on-disk transcripts
	// become the branch's first commit.
	if err := commitAll(claudeDir, "clawk: checkpoint on boot"); err != nil {
		return err
	}

	// Fetch is best-effort: a brand-new project's bare repo has no refs yet.
	_, _ = git(claudeDir, "fetch", "-q", "origin")
	if refExists(claudeDir, "origin/main") {
		// --allow-unrelated-histories: the branch's root commit and main's
		// root commit are unrelated until the first merge-back links them.
		// Harmless once related. Conflicts in tracked transcript/MEMORY files
		// are auto-resolved by the union driver in .gitattributes.
		if _, err := git(claudeDir, "merge", "-q", "--no-edit",
			"--allow-unrelated-histories", "origin/main"); err != nil {
			return fmt.Errorf("seeding from main: %w", err)
		}
	}
	// git stamps every file it writes with the checkout time, which collapses
	// the seeded sessions onto one instant and scrambles Claude Code's resume
	// list. Restore real per-session times so the order is correct.
	restoreTranscriptMtimes(claudeDir)
	return nil
}

// Preserve commits the live tree and pushes the sandbox's branch to the bare
// repo. It is lossless and deliberately NEVER touches `main`: a detached or
// destroyed VM's sessions are kept safe on `vm/<name>`, ready to be published
// later with Publish. This is the teardown step under the explicit-merge
// model — destroying a throwaway VM no longer pollutes the canonical history.
//
// It also doubles as in-place migration for a pre-feature sandbox: the
// existing on-disk transcripts become the branch's first commit. A no-op when
// the sandbox produced no commits.
func Preserve(bare, claudeDir, branch string) error {
	if _, err := os.Stat(claudeDir); err != nil {
		return nil // never had any state — nothing to preserve
	}
	if err := ensureWorkingTree(bare, claudeDir, branch); err != nil {
		return err
	}
	if err := commitAll(claudeDir, "clawk: checkpoint"); err != nil {
		return err
	}
	return pushBranch(claudeDir, branch)
}

// Refresh is the per-attach sync for a running sandbox. It pulls the curated
// `main` into the working tree (so the resume list reflects what's been
// published) and preserves this VM's branch — but, per the explicit-merge
// model, it does NOT publish to `main`. This is what keeps a long-lived
// `clawk here` sandbox (which may stay up for weeks and so never re-runs
// Prepare) current with sessions published elsewhere.
//
// Safe on a live sandbox: a merge from main only ADDS other VMs' transcript
// files (sessions are single-owner per id) and never rewrites the session
// being appended; MEMORY.md union-merges.
func Refresh(bare, claudeDir, branch string) error {
	if _, err := os.Stat(claudeDir); err != nil {
		return nil
	}
	if err := ensureWorkingTree(bare, claudeDir, branch); err != nil {
		return err
	}
	if err := commitAll(claudeDir, "clawk: checkpoint on attach"); err != nil {
		return err
	}
	_, _ = git(claudeDir, "fetch", "-q", "origin")
	if refExists(claudeDir, "origin/main") {
		if _, err := git(claudeDir, "merge", "-q", "--no-edit",
			"--allow-unrelated-histories", "origin/main"); err != nil {
			return fmt.Errorf("refreshing from main: %w", err)
		}
	}
	restoreTranscriptMtimes(claudeDir)
	return pushBranch(claudeDir, branch)
}

// Publish folds a sandbox's preserved branch into the project's canonical
// `main` — the explicit "this VM's work belongs in the project history" step
// behind `clawk sessions merge`. It operates on the bare repo alone, so it
// works even after the VM is destroyed (the branch was pushed during its
// life). Idempotent: re-publishing an already-merged branch is a no-op.
func Publish(bare, branch string) error {
	if !branchInBare(bare, branch) {
		return fmt.Errorf("no preserved sessions for %q to publish", branch)
	}
	if !bareHasMain(bare) {
		// First publish for this project: main starts as a copy of the branch.
		if _, err := git(bare, "branch", "main", branch); err != nil {
			return fmt.Errorf("creating main: %w", err)
		}
		return nil
	}
	if branchMergedIntoMain(bare, branch) {
		return nil // already published — nothing new
	}
	return mergeInBareWorktree(bare, branch)
}

// pushBranch force-pushes the sandbox branch to the bare repo if it has any
// commits. Force is safe: a branch is single-owner (one sandbox), so the bare
// copy is only ever behind the local one.
func pushBranch(claudeDir, branch string) error {
	if !refExists(claudeDir, "HEAD") {
		return nil // unborn — no session data yet
	}
	if _, err := git(claudeDir, "push", "-q", "origin", "+"+branch+":"+branch); err != nil {
		return fmt.Errorf("pushing %s: %w", branch, err)
	}
	return nil
}

// mergeInBareWorktree merges branch into main inside a temporary worktree of
// the bare repo, then advances the bare repo's main. The worktree is removed
// regardless of outcome. On an unresolved conflict the branch is already
// pushed, so no history is lost — we surface the error and leave main as-is.
func mergeInBareWorktree(bare, branch string) (err error) {
	tmp, err := os.MkdirTemp("", "clawk-sessmerge-")
	if err != nil {
		return fmt.Errorf("merge worktree: %w", err)
	}
	// git refuses to `worktree add` onto an existing dir, so hand it a
	// fresh path inside tmp and clean the whole tmp tree afterwards.
	wt := filepath.Join(tmp, "wt")
	if _, err := git(bare, "worktree", "add", "-q", "-f", wt, "main"); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("adding merge worktree: %w", err)
	}
	defer func() {
		_, _ = git(bare, "worktree", "remove", "--force", wt)
		os.RemoveAll(tmp)
		_, _ = git(bare, "worktree", "prune")
	}()

	if _, mErr := git(wt, "merge", "-q", "--no-edit",
		"--allow-unrelated-histories", branch); mErr != nil {
		_, _ = git(wt, "merge", "--abort")
		return fmt.Errorf("merging %s into main: %w", branch, mErr)
	}
	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────

// ensureWorkingTree makes claudeDir a git repo whose origin is the bare
// history repo, with the control files in place and HEAD on branch. Safe on a
// dir that already holds a previous sandbox's state (`git init` never deletes
// files) and idempotent on an already-prepared tree — this is what lets a
// pre-feature sandbox migrate in place on its next boot or on destroy.
func ensureWorkingTree(bare, claudeDir, branch string) error {
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		return fmt.Errorf("creating state dir: %w", err)
	}
	if _, err := os.Stat(filepath.Join(claudeDir, ".git")); err != nil {
		if _, err := git(claudeDir, "init", "-q"); err != nil {
			return fmt.Errorf("git init: %w", err)
		}
		// A local identity so commits succeed regardless of host git config.
		_, _ = git(claudeDir, "config", "user.name", "clawk")
		_, _ = git(claudeDir, "config", "user.email", "clawk@localhost")
	}
	if err := writeControlFiles(claudeDir); err != nil {
		return err
	}
	if err := setRemote(claudeDir, "origin", bare); err != nil {
		return err
	}
	if _, err := git(claudeDir, "checkout", "-q", "-B", branch); err != nil {
		return fmt.Errorf("checkout %s: %w", branch, err)
	}
	return nil
}

// restoreTranscriptMtimes walks the tracked transcripts and sets each file's
// modification time to the timestamp of its last recorded event. Claude
// Code's resume picker orders sessions by mtime, but git stamps every
// checked-out/merged file with the checkout instant — bunching unrelated
// sessions together and scrambling the order. Each `<uuid>.jsonl` records
// ISO-8601 `timestamp` fields per line; the last one is the session's
// last-activity time, which is exactly the order we want. Best-effort: a file
// we can't parse keeps git's mtime.
func restoreTranscriptMtimes(claudeDir string) {
	root := filepath.Join(claudeDir, "projects")
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		if ts, ok := lastEventTime(path); ok {
			_ = os.Chtimes(path, ts, ts)
		}
		return nil
	})
}

// lastEventTime returns the timestamp of the last JSON line in a transcript
// that carries one. It reads only the file's tail — transcripts grow into the
// megabytes and we only need the final event.
func lastEventTime(path string) (time.Time, bool) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return time.Time{}, false
	}
	const tail = 64 << 10
	start := fi.Size() - tail
	if start < 0 {
		start = 0
	}
	buf := make([]byte, fi.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
		return time.Time{}, false
	}
	lines := bytes.Split(buf, []byte{'\n'})
	// Scan from the end; the leading line may be a fragment (we sliced
	// mid-file) but it fails to parse and is skipped harmlessly.
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var rec struct {
			Timestamp string `json:"timestamp"`
		}
		if json.Unmarshal(line, &rec) != nil || rec.Timestamp == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, rec.Timestamp); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func writeControlFiles(dir string) error {
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(gitignore), 0o644); err != nil {
		return fmt.Errorf("writing .gitignore: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte(gitattributes), 0o644); err != nil {
		return fmt.Errorf("writing .gitattributes: %w", err)
	}
	return nil
}

// setRemote points name at url, replacing any existing definition so a moved
// history root (e.g. the v1→v2 ~/.clawk migration) self-heals on next boot.
func setRemote(dir, name, url string) error {
	if _, err := git(dir, "remote", "set-url", name, url); err == nil {
		return nil
	}
	if _, err := git(dir, "remote", "add", name, url); err != nil {
		return fmt.Errorf("setting remote %s: %w", name, err)
	}
	return nil
}

// commitAll stages the tracked subset (.gitignore decides what that is) and
// commits if anything changed. A clean tree is not an error.
func commitAll(dir, msg string) error {
	if _, err := git(dir, "add", "-A"); err != nil {
		return fmt.Errorf("git add: %w", err)
	}
	if !dirty(dir) {
		return nil
	}
	if _, err := git(dir, "commit", "-q", "-m", msg); err != nil {
		return fmt.Errorf("git commit: %w", err)
	}
	return nil
}

// dirty reports whether the index has staged changes relative to HEAD (or
// any staged content at all when HEAD is unborn).
func dirty(dir string) bool {
	// Exit status 1 from `diff --cached --quiet` means "differences"; on an
	// unborn HEAD it errors, so fall back to checking the staged file list.
	if _, err := git(dir, "diff", "--cached", "--quiet"); err != nil {
		return true
	}
	return false
}

// refExists reports whether a revision resolves in the repo at dir.
func refExists(dir, rev string) bool {
	_, err := git(dir, "rev-parse", "--verify", "-q", rev+"^{commit}")
	return err == nil
}

// bareHasMain reports whether the bare repo already has a main branch.
func bareHasMain(bare string) bool {
	_, err := git(bare, "show-ref", "--verify", "-q", "refs/heads/main")
	return err == nil
}

// branchMergedIntoMain reports whether branch is already an ancestor of main
// in the bare repo — i.e. there's nothing new to merge back.
func branchMergedIntoMain(bare, branch string) bool {
	_, err := git(bare, "merge-base", "--is-ancestor", branch, "main")
	return err == nil
}

// branchInBare reports whether the bare repo holds the given branch.
func branchInBare(bare, branch string) bool {
	_, err := git(bare, "show-ref", "--verify", "-q", "refs/heads/"+branch)
	return err == nil
}

// ListBranches returns the preserved per-sandbox branches in the bare repo
// (the vm/<name> refs), each with its last-activity time and whether it has
// been published into main. Powers `clawk sessions list`.
func ListBranches(bare string) ([]BranchInfo, error) {
	out, err := git(bare, "for-each-ref", "--format=%(refname:short)%09%(committerdate:iso8601)", "refs/heads/"+branchPrefix)
	if err != nil {
		return nil, err
	}
	var infos []BranchInfo
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		ref, date, _ := strings.Cut(line, "\t")
		infos = append(infos, BranchInfo{
			Branch:    ref,
			Sandbox:   strings.TrimPrefix(ref, branchPrefix),
			LastSeen:  strings.TrimSpace(date),
			Published: bareHasMain(bare) && branchMergedIntoMain(bare, ref),
		})
	}
	return infos, nil
}

// BranchInfo describes one preserved session branch.
type BranchInfo struct {
	Branch    string // full ref short name, e.g. "vm/feature-x"
	Sandbox   string // the sandbox name (branch without the vm/ prefix)
	LastSeen  string // committer date of the branch tip
	Published bool   // already merged into main
}

// git runs a git command (in dir, or the process cwd when dir is "") and
// returns trimmed combined output. Errors include git's own message, which
// is what makes failures debuggable.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	// Keep behavior deterministic regardless of the host's git config.
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s: %w",
			strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)), nil
}
