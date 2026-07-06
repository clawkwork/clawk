package cli

// Phase 6 of the v2 redesign moves Phase.Status from "thing the user
// edits with `branch set`" to "thing derived from gh pr list". The
// cached value still lives on the sandbox record so reads stay fast;
// refreshPRState is the one place we cross the gh boundary.
//
// Callers that want fresh state pass force=true (e.g. `clawk pr` after
// it just opened a PR). The dashboard and rebase paths pass false so
// the cache absorbs back-to-back invocations.

import (
	"fmt"
	"os/exec"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/pr"
)

// prCacheTTL is the freshness window for cached Phase.Status. Sixty
// seconds is short enough that interactive use ("I just merged the
// PR — what does status show?") feels live, and long enough that
// a tab full of `watch -n 5 clawk status` doesn't hammer gh.
const prCacheTTL = 60 * time.Second

// refreshPRState updates each Phase.Status from gh's view of the
// world, then persists the sandbox. When force is false and the cache
// is fresh, it's a no-op. When gh isn't on PATH, the function returns
// nil after leaving Status untouched — clawk continues to function
// without a GitHub CLI; users just see the cached values until they
// install one.
func refreshPRState(sb *config.Sandbox, force bool) error {
	if !force && time.Since(sb.PRRefreshedAt) < prCacheTTL {
		return nil
	}
	if _, err := exec.LookPath(prGHBin()); err != nil {
		// Best-effort: leave cached state alone. The dashboard and
		// rebase paths still render; they just won't have today's PR
		// merges reflected. Surfacing a hard error here would block
		// the basic `clawk status` flow on every machine without gh.
		return nil
	}

	// Group phases by Repo so gh fires once per repo, not once per
	// branch. We use one of the worktrees inside that repo as gh's
	// CWD — all worktrees of the same repo share the same `origin`,
	// so any of them works.
	byRepo := make(map[string][]*config.Phase)
	for i := range sb.Phases {
		p := &sb.Phases[i]
		if p.Worktree == "" {
			continue
		}
		byRepo[p.Repo] = append(byRepo[p.Repo], p)
	}

	var firstErr error
	for _, phases := range byRepo {
		// Pick the first worktree as gh's CWD. Branches in the
		// returned slice line up with `phases` by index, since
		// pr.Resolve preserves the input order.
		wtPath := phases[0].Worktree
		branches := make([]string, 0, len(phases))
		for _, p := range phases {
			branches = append(branches, p.Branch)
		}
		results, err := pr.Resolve(wtPath, branches)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for i, p := range phases {
			r := results[i]
			switch r.State {
			case pr.StateOpen:
				// "Open PR" maps to Active in our schema — there's
				// work in flight in this worktree.
				p.Status = config.PhaseStatusActive
			case pr.StateMerged:
				p.Status = config.PhaseStatusMerged
			case pr.StateClosed:
				// Closed-but-not-merged is unusual but real (PR
				// abandoned). Treat as merged for sibling-rebase
				// purposes — the worktree is no longer "open work".
				p.Status = config.PhaseStatusMerged
			case pr.StateUnknown:
				// No PR yet — leave cached value. Don't downgrade
				// Active→Pending just because gh didn't see a row;
				// the user may have manually marked it during the
				// transition.
			}
		}
	}

	sb.PRRefreshedAt = time.Now()
	if err := store.Save(sb); err != nil {
		return fmt.Errorf("saving refreshed PR state: %w", err)
	}
	if firstErr != nil {
		return fmt.Errorf("refreshing PR state (partial): %w", firstErr)
	}
	return nil
}

// prGHBin returns the gh binary path — pr.GHBin, so the tests' fixture
// shim reaches both this file's LookPath probe and pr.Resolve's exec.
func prGHBin() string {
	return pr.GHBin
}
