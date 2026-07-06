// Package pr resolves PR state for a list of (repo, branch) pairs by
// shelling out to `gh`. It is the source-of-truth in v2 for whether a
// worktree's branch is open / merged / closed; the per-Phase Status
// field on the on-disk sandbox record is just a 60-second cache.
//
// The package is deliberately small: one entry point, one shape of
// answer, and one shim seam (the GHBin variable) for tests. Anything fancier
// (auth probing, multi-account orchestration) belongs in `gh` itself.
package pr

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// State is the PR lifecycle as gh reports it. "unknown" covers the
// cases where gh failed (network, auth) or returned no rows for a
// branch — caller decides whether to fall back to a cached value or
// treat it as "no PR yet".
type State string

const (
	StateOpen    State = "open"
	StateMerged  State = "merged"
	StateClosed  State = "closed"
	StateUnknown State = "unknown"
)

// Result describes the canonical PR for one (repo, branch). When State
// is StateUnknown, Number is zero and the rest of the fields are
// undefined.
type Result struct {
	Branch string `json:"branch"`
	State  State  `json:"state"`
	Number int    `json:"number,omitempty"`
	URL    string `json:"url,omitempty"`
}

// Resolve queries `gh pr list` once for the given repo and returns one
// Result per branch the caller asked about. Branches with no PR are
// returned with State=StateUnknown rather than omitted, so callers
// can iterate the slice they passed in lock-step with the response.
//
// gh requires a working directory inside a git repo (it reads the
// remote URL from there). repo must therefore be the path to a git
// checkout, not a remote URL.
//
// One process per repo: PR listing is cheap on the GitHub side
// (single REST call) and avoiding O(branches) processes keeps a
// 10-branch sandbox snappy.
func Resolve(repo string, branches []string) ([]Result, error) {
	if len(branches) == 0 {
		return nil, nil
	}
	out, err := runGH(repo,
		"pr", "list",
		"--state", "all",
		"--limit", "200",
		"--json", "number,headRefName,state,url")
	if err != nil {
		return nil, err
	}

	var rows []struct {
		Number      int    `json:"number"`
		HeadRefName string `json:"headRefName"`
		State       string `json:"state"`
		URL         string `json:"url"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("decoding gh output: %w", err)
	}

	// Index by branch. gh returns rows newest-first; if a branch has
	// multiple historical PRs (not common, but happens after a force-
	// recreate) we keep the freshest one.
	byBranch := make(map[string]Result, len(rows))
	for _, r := range rows {
		if _, seen := byBranch[r.HeadRefName]; seen {
			continue
		}
		byBranch[r.HeadRefName] = Result{
			Branch: r.HeadRefName,
			State:  ghStateToState(r.State),
			Number: r.Number,
			URL:    r.URL,
		}
	}

	results := make([]Result, len(branches))
	for i, b := range branches {
		if r, ok := byBranch[b]; ok {
			results[i] = r
		} else {
			results[i] = Result{Branch: b, State: StateUnknown}
		}
	}
	return results, nil
}

// ghStateToState normalises gh's enum (uppercase OPEN/MERGED/CLOSED)
// into the lowercase form this package exposes.
func ghStateToState(s string) State {
	switch strings.ToUpper(s) {
	case "OPEN":
		return StateOpen
	case "MERGED":
		return StateMerged
	case "CLOSED":
		return StateClosed
	default:
		return StateUnknown
	}
}

// GHBin is the binary runGH execs. A variable, deliberately not an env
// knob: tests (here and in internal/cli) swap in a fixture shim that
// prints canned JSON, and no public surface exists to point clawk at a
// different gh — that's what PATH is for.
var GHBin = "gh"

// runGH execs gh in the given directory and returns stdout.
func runGH(dir string, args ...string) ([]byte, error) {
	bin := GHBin
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("%s: %s", bin, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("%s: %w", bin, err)
	}
	return out, nil
}
