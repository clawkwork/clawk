package worktree

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Add creates a git worktree for the given repo at the specified branch.
// The worktree is placed at baseDir/<repo-basename>/ .
// Returns the absolute path to the created worktree.
//
// If the branch does not already exist in the repo, it is created from
// startPoint. Pass "" to let git default to HEAD (typical per-ticket
// workflow: user starts from main, spawns new branch off it). Callers
// handling a merged-base rename should pass an explicit remote ref like
// "origin/main" so the new branch doesn't inherit whatever HEAD happens
// to point at.
//
// If the branch already exists, we attach the worktree to it and ignore
// startPoint.
func Add(repo, branch, baseDir, startPoint string) (string, error) {
	absRepo, err := filepath.Abs(repo)
	if err != nil {
		return "", fmt.Errorf("resolving repo path: %w", err)
	}

	if _, err := os.Stat(filepath.Join(absRepo, ".git")); err != nil {
		return "", fmt.Errorf("%s is not a git repository", absRepo)
	}

	repoName := filepath.Base(absRepo)
	wtPath := filepath.Join(baseDir, repoName)

	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return "", fmt.Errorf("creating worktree dir: %w", err)
	}

	args := []string{"-C", absRepo, "worktree", "add", wtPath, branch}
	if !branchExists(absRepo, branch) {
		// -b asks git to create the branch off of HEAD (or startPoint
		// if given). We only pass it when we're certain the branch
		// doesn't exist, so we never accidentally reset an existing
		// branch.
		args = []string{"-C", absRepo, "worktree", "add", "-b", branch, wtPath}
		if startPoint != "" {
			args = append(args, startPoint)
		}
	}

	cmd := exec.Command("git", args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return wtPath, nil
	}

	// Recovery path. The two failure modes we can safely auto-clean:
	//   1. wtPath is registered as a worktree in git's bookkeeping but
	//      the directory was removed externally (`os.RemoveAll`-without-
	//      `git worktree remove`). Reported as "already exists" or
	//      "missing but already registered".
	//   2. wtPath exists on disk but is *not* registered as an active
	//      worktree (orphan from a prior `clawk new` that errored
	//      after Add but before the sandbox config got saved).
	// Both look like "already exists" to the user. Refuse to touch the
	// path if it's currently a *registered, live* worktree — that's the
	// user actively using two sandboxes pointing at the same path, and
	// silently nuking it would lose work.
	if cleaned, cleanErr := cleanOrphanWorktree(absRepo, wtPath); cleanErr != nil {
		return "", fmt.Errorf("git worktree add: %s: %w (cleanup also failed: %w)",
			strings.TrimSpace(string(out)), err, cleanErr)
	} else if !cleaned {
		return "", fmt.Errorf("git worktree add: %s: %w", strings.TrimSpace(string(out)), err)
	}

	cmd = exec.Command("git", args...)
	out, err = cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git worktree add (after orphan cleanup): %s: %w",
			strings.TrimSpace(string(out)), err)
	}
	return wtPath, nil
}

// cleanOrphanWorktree clears the two cleanable forms of stale state at
// wtPath, and returns whether it actually cleaned anything (so callers
// know whether a retry is worthwhile).
//
// We prune git's bookkeeping first — that turns "registered but missing"
// into "not registered". Then if wtPath exists on disk and is not in the
// active worktree list, it's an orphan; remove it. If wtPath is in the
// active list, leave it alone — the user has a live worktree there and
// we won't silently destroy work.
func cleanOrphanWorktree(repo, wtPath string) (bool, error) {
	cleaned := false
	if err := exec.Command("git", "-C", repo, "worktree", "prune").Run(); err == nil {
		cleaned = true
	}
	if _, statErr := os.Stat(wtPath); statErr != nil {
		// Path doesn't exist — pruning may have been the whole fix.
		return cleaned, nil
	}
	registered, err := isRegisteredWorktree(repo, wtPath)
	if err != nil {
		return cleaned, fmt.Errorf("listing worktrees: %w", err)
	}
	if registered {
		// Live worktree at this path — refuse to touch.
		return false, nil
	}
	if err := os.RemoveAll(wtPath); err != nil {
		return cleaned, fmt.Errorf("removing orphan worktree dir %s: %w", wtPath, err)
	}
	return true, nil
}

// isRegisteredWorktree reports whether wtPath appears in `git worktree
// list` for repo. Uses --porcelain so the output is greppable across git
// versions (the human-readable form is whitespace-delimited and varies).
func isRegisteredWorktree(repo, wtPath string) (bool, error) {
	out, err := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain").Output()
	if err != nil {
		return false, err
	}
	want := "worktree " + wtPath
	for _, line := range strings.Split(string(out), "\n") {
		if line == want {
			return true, nil
		}
	}
	return false, nil
}

// BranchResolution is the outcome of ResolveBranchName: the branch name
// to actually use plus any context about why it may differ from the
// base the user asked for.
type BranchResolution struct {
	Branch     string // name to use (may equal Base, or Base-N)
	Base       string // the name the caller started with
	StartPoint string // git rev to fork from; "" means HEAD (branch already exists)
	Reused     bool   // an existing branch was reused (no commit/history lost)
	WasMerged  bool   // an earlier name in the chain had a merged PR — we bumped past it
	MergedPR   int    // PR number of the most-recent merged ancestor (0 if unknown)
}

// ResolveBranchName decides the branch name to use for new work on
// repo, given a desired base name. The walk:
//
//  1. Try `base`. If it doesn't exist: create it fresh.
//  2. If `base` exists AND its PR is merged: try `base-2`. Continue.
//  3. If any candidate exists and is NOT merged (open PR, no PR,
//     closed-unmerged): reuse it. We don't want to spawn `-N+1`
//     every `destroy`/`run` cycle when `-N` is still in-flight.
//  4. If a candidate doesn't exist yet but a previous one was
//     merged: create that fresh from origin/<default>.
//
// gh must be on PATH and authenticated for the merged-detection to
// fire; without it we treat every branch as "not merged" and reuse
// the base. Fails safe: a user without gh gets predictable reuse, not
// surprise suffixes.
func ResolveBranchName(repo, base string) BranchResolution {
	r := BranchResolution{Branch: base, Base: base}
	const maxSuffix = 1000 // pathological ceiling; never expected to hit
	for i := 1; i <= maxSuffix; i++ {
		candidate := base
		if i > 1 {
			candidate = fmt.Sprintf("%s-%d", base, i)
		}
		if !branchExists(repo, candidate) {
			// No branch by this name anywhere (local or origin). If
			// we walked past any merged predecessors, fork the new
			// one cleanly from origin/<default> so we don't inherit
			// a stale local HEAD.
			r.Branch = candidate
			if r.WasMerged {
				r.StartPoint = defaultOriginBranch(repo)
			}
			return r
		}
		merged, pr := isBranchPRMerged(repo, candidate)
		if !merged {
			// Branch exists and its PR (if any) isn't merged. Reuse
			// it — the user is mid-flight; new commits should land
			// on this branch, not a sibling.
			r.Branch = candidate
			r.Reused = true
			return r
		}
		// Branch is merged: record the most recent merged PR for
		// the caller's "note" line and keep walking.
		r.WasMerged = true
		r.MergedPR = pr
	}
	// Unreachable in practice — means 1000 consecutive merged branches.
	r.Branch = base + "-N"
	return r
}

// isBranchPRMerged asks gh whether the given branch has a merged PR on
// GitHub. Returns (false, 0) on any error — gh not installed, not
// authenticated, no PR for this branch, network failure. All of those
// should leave behavior unchanged (no silent rename).
func isBranchPRMerged(repo, branch string) (bool, int) {
	cmd := exec.Command("gh", "pr", "view", branch,
		"--json", "state,number",
		"-q", `.state + " " + (.number | tostring)`)
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		return false, 0
	}
	parts := strings.Fields(strings.TrimSpace(string(out)))
	if len(parts) != 2 || parts[0] != "MERGED" {
		return false, 0
	}
	n, _ := strconv.Atoi(parts[1])
	return true, n
}

// defaultOriginBranch returns the name of the default branch on origin
// (usually "origin/main" or "origin/master"), in the form
// "origin/<branch>" ready to pass to `git worktree add -b <name>
// <path> <start>`. Falls back to "origin/main" if we can't tell —
// that's the most common default and an incorrect guess just surfaces
// as a git error from the caller, not a silent branch-from-wrong-point.
func defaultOriginBranch(repo string) string {
	out, err := exec.Command("git", "-C", repo,
		"symbolic-ref", "--short", "refs/remotes/origin/HEAD").Output()
	if err != nil {
		return "origin/main"
	}
	name := strings.TrimSpace(string(out))
	if name == "" {
		return "origin/main"
	}
	return name
}

// branchExists reports whether a local branch by that name is registered in
// the repo. We check both heads and remote-tracking branches so
// `clawk run foo-template origin/feature` and similar work naturally.
func branchExists(repo, branch string) bool {
	// git show-ref --verify --quiet refs/heads/<branch> exits 0 if exists.
	for _, ref := range []string{"refs/heads/" + branch, "refs/remotes/origin/" + branch} {
		if exec.Command("git", "-C", repo, "show-ref", "--verify", "--quiet", ref).Run() == nil {
			return true
		}
	}
	// Fallback: rev-parse with --verify handles short names and abbreviations.
	return exec.Command("git", "-C", repo, "rev-parse", "--verify", "--quiet", branch).Run() == nil
}

// Remove removes a git worktree that was previously added.
// It runs `git worktree remove` from the original repo.
func Remove(repo, wtPath string) error {
	absRepo, err := filepath.Abs(repo)
	if err != nil {
		return fmt.Errorf("resolving repo path: %w", err)
	}

	cmd := exec.Command("git", "-C", absRepo, "worktree", "remove", wtPath, "--force")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	// "is not a working tree" means the worktree was already deleted on disk
	// but git still has a stale registration. Prune and remove the dir, then succeed.
	if strings.Contains(string(out), "is not a working tree") {
		// Best effort: these are the error-recovery path. Caller already knows
		// the primary operation had problems; surface only the truly-unrecoverable
		// issues rather than masking them with secondary cleanup failures.
		if pruneErr := exec.Command("git", "-C", absRepo, "worktree", "prune").Run(); pruneErr != nil {
			return fmt.Errorf("git worktree prune after stale worktree: %w", pruneErr)
		}
		if rmErr := os.RemoveAll(wtPath); rmErr != nil {
			return fmt.Errorf("removing stale worktree dir %s: %w", wtPath, rmErr)
		}
		return nil
	}
	return fmt.Errorf("git worktree remove: %s: %w", strings.TrimSpace(string(out)), err)
}

// RemoveAll removes all worktrees for a sandbox by cleaning up the sandbox's worktree directory.
// It runs `git worktree remove` for each phase, then removes the directory.
// Accumulates errors from every phase so a single failure doesn't stop cleanup.
func RemoveAll(phases []Phase, baseDir string) error {
	var errs []string
	for _, p := range phases {
		if p.Worktree == "" {
			continue
		}
		if err := Remove(p.Repo, p.Worktree); err != nil {
			errs = append(errs, fmt.Sprintf("phase %d (%s): %v", p.Order, p.Branch, err))
		}
	}
	if err := os.RemoveAll(baseDir); err != nil {
		errs = append(errs, fmt.Sprintf("removing base dir %s: %v", baseDir, err))
	}
	if len(errs) > 0 {
		return fmt.Errorf("errors removing worktrees:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

// Phase mirrors config.Phase to avoid circular imports.
type Phase struct {
	Repo     string
	Branch   string
	Worktree string
	Order    int
}

// DirtyStatus returns the output of `git status --porcelain` for the
// worktree. An empty string means clean (no staged, unstaged, or
// untracked changes). Non-empty means there's work that would be lost
// on a `git worktree remove --force`. If the worktree path doesn't
// exist or isn't a git working tree, returns "" and no error — nothing
// to preserve.
func DirtyStatus(wtPath string) (string, error) {
	if _, err := os.Stat(wtPath); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	cmd := exec.Command("git", "-C", wtPath, "status", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		// Stale worktree dir with no .git link file — treat as clean.
		// A real git error would come from a broken repo, not a missing
		// worktree, so this is a safe default.
		return "", nil
	}
	return strings.TrimSpace(string(out)), nil
}
