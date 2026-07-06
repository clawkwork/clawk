package cli

// `clawk worktree` is the v2 noun for the (repo, branch, on-disk path,
// status) tuples that hang off a ticket sandbox. v1 called this
// `branch`; v2 renames it because:
//
//  1. "branch" overlaps git's primitive; readers expected `clawk
//     branch` to wrap `git branch`, not coordinate worktrees.
//  2. The clawk noun is bigger than git's: worktree + branch + status,
//     all in service of a sandbox. "worktree" is the honest name.
//
// v2 also tightens the verb set. Only `add` and `rebase` survive.
// `set` and `list` collapse into derived state + `clawk status`;
// `merge` and its `close`/`reopen` cousins were the same overload
// under different names. `rebase` is the one explicit local action
// that has to exist (clawk has to *do* the rebase, not just observe).

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/worktree"
	"github.com/spf13/cobra"
)

var (
	worktreeAddName  string
	worktreeListJSON bool
)

func init() {
	rootCmd.AddCommand(worktreeCmd)
	worktreeCmd.AddCommand(worktreeAddCmd)
	worktreeCmd.AddCommand(worktreeRebaseCmd)
	worktreeCmd.AddCommand(worktreeListCmd)

	worktreeAddCmd.Flags().StringVar(&worktreeAddName, "name", "",
		"branch name (defaults to the sandbox name)")
	worktreeListCmd.Flags().BoolVar(&worktreeListJSON, "json", false,
		"emit JSON (the only supported mode; human path is 'clawk status')")
}

var worktreeCmd = &cobra.Command{
	Use:   "worktree",
	Short: "Manage a sandbox's worktrees (one git worktree per repo)",
	Long: `A clawk sandbox holds N worktrees — each a (repo, branch,
on-disk path, status) tuple in service of the sandbox's ticket.
Worktrees share the sandbox's ticket prefix (e.g., a sandbox named
INFRA-123 grows worktrees called INFRA-123 in each tracked repo).
When a worktree's PR is merged and the ticket needs follow-up work
in the same repo, ` + "`clawk worktree add <sandbox> <repo>`" + ` auto-bumps
the new branch to ` + "`<ticket>-2`" + ` to dodge the closed PR.`,
}

var worktreeAddCmd = &cobra.Command{
	ValidArgsFunction: completeSandboxNames,
	Use:               "add <sandbox> <repo> [--name <branch>]",
	Short:             "Add a worktree (repo + branch) to a sandbox",
	Args:              cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		name, repoArg := args[0], args[1]
		sb, err := store.Load(name)
		if err != nil {
			return err
		}

		// Resolve repo path to absolute, following symlinks
		// (macOS: /tmp → /private/tmp; git stores resolved paths in
		// worktree .git files).
		repo, err := filepath.Abs(repoArg)
		if err != nil {
			return fmt.Errorf("resolving repo path: %w", err)
		}
		repo, err = filepath.EvalSymlinks(repo)
		if err != nil {
			return fmt.Errorf("resolving symlinks: %w", err)
		}

		desired := worktreeAddName
		if desired == "" {
			desired = sb.DisplayName()
		}

		res := worktree.ResolveBranchName(repo, desired)
		switch {
		case res.Reused && res.Branch != res.Base:
			fmt.Printf("Note: reusing existing branch %q in %s (PR not merged).\n",
				res.Branch, filepath.Base(repo))
		case res.WasMerged:
			fmt.Printf("Note: branch %q in %s was merged (PR #%d). "+
				"Starting %q from %s for new work; override with --name if undesired.\n",
				res.Base, filepath.Base(repo), res.MergedPR, res.Branch, res.StartPoint)
		}
		wtDir := store.WorktreeDir(name)
		wtPath, err := worktree.Add(repo, res.Branch, wtDir, res.StartPoint)
		if err != nil {
			return fmt.Errorf("creating worktree: %w", err)
		}

		sb.Phases = append(sb.Phases, config.Phase{
			Repo:     repo,
			Branch:   res.Branch,
			Status:   config.PhaseStatusPending,
			Order:    len(sb.Phases),
			Worktree: wtPath,
		})

		if err := store.Save(sb); err != nil {
			return err
		}
		fmt.Printf("Added worktree %d: %s @ %s\n", len(sb.Phases)-1, filepath.Base(repo), res.Branch)
		fmt.Printf("  worktree: %s\n", wtPath)
		return nil
	},
}

var worktreeRebaseCmd = &cobra.Command{
	ValidArgsFunction: completeSandboxNames,
	Use:               "rebase <sandbox> <repo>",
	Short:             "Rebase open follow-up worktrees in <repo> onto origin/<default>",
	Long: `rebase brings open follow-up worktrees up to date after a prior
worktree in the same repo merged. The follow-up flow:

  1. <ticket>'s <repo> worktree merges; CI publishes a new artifact.
  2. ` + "`clawk worktree add <ticket> <repo>`" + ` opens
     ` + "`<ticket>-2`" + ` for the follow-up.
  3. ` + "`clawk worktree rebase <ticket> <repo>`" + ` rebases
     the open follow-up onto origin/<default> so it sits on top of the
     just-merged code.

Conflicts surface and the rebase stops; resolve inside the worktree
and run ` + "`git rebase --continue`" + `.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		name, repoArg := args[0], args[1]
		sb, err := store.Load(name)
		if err != nil {
			return err
		}

		repo, err := resolveRepoArg(sb, repoArg)
		if err != nil {
			return err
		}

		// v2 derives Phase.Status from PR state — refresh before we
		// pick rebase targets so a freshly merged sibling doesn't get
		// rebased against itself.
		if err := refreshPRState(sb, false); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "clawk: %v\n", err)
		}

		var targets []*config.Phase
		for i := range sb.Phases {
			p := &sb.Phases[i]
			if p.Repo != repo {
				continue
			}
			if p.Status == config.PhaseStatusMerged {
				continue
			}
			if p.Worktree == "" {
				continue
			}
			targets = append(targets, p)
		}
		if len(targets) == 0 {
			return fmt.Errorf("no open worktrees in %s for sandbox %q",
				filepath.Base(repo), config.DisplayName(name))
		}

		// Refresh origin so the rebase target is current. We rebase
		// onto the repo's default branch on origin — the merged
		// sibling's commits live there once its PR landed, which is
		// exactly the state a follow-up wants to sit on top of.
		def := defaultBranchOnOrigin(repo)
		if out, err := exec.Command("git", "-C", repo, "fetch", "origin", def).CombinedOutput(); err != nil {
			return fmt.Errorf("git fetch origin %s: %s", def, strings.TrimSpace(string(out)))
		}

		var rebaseErrs []string
		for _, p := range targets {
			fmt.Printf("rebasing %s onto origin/%s...\n", p.Branch, def)
			out, err := exec.Command(
				"git", "-C", p.Worktree, "rebase", "origin/"+def,
			).CombinedOutput()
			if err != nil {
				rebaseErrs = append(rebaseErrs,
					fmt.Sprintf("%s: %s", p.Branch, strings.TrimSpace(string(out))))
			}
		}
		if len(rebaseErrs) > 0 {
			return fmt.Errorf("rebase conflicts:\n  %s\nresolve inside the worktree(s) and run `git rebase --continue`",
				strings.Join(rebaseErrs, "\n  "))
		}
		return nil
	},
}

var worktreeListCmd = &cobra.Command{
	ValidArgsFunction: completeSandboxNames,
	Use:               "list <sandbox>",
	Short:             "List worktrees (JSON-only — use 'clawk status' for human view)",
	Args:              cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// v2: human output for worktrees lives inside `clawk status`.
		// `worktree list` is preserved as a JSON-only shim so scripts
		// that want a stable schema for `(repo, branch, status,
		// worktree_path)` tuples don't have to parse the dashboard.
		if !worktreeListJSON {
			return fmt.Errorf(
				"worktree list is JSON-only — pass --json (or use 'clawk status %s' for human output)",
				args[0])
		}
		sb, err := store.Load(args[0])
		if err != nil {
			return err
		}
		out := struct {
			Schema    string             `json:"schema"`
			Sandbox   string             `json:"sandbox"`
			Worktrees []statusJSONBranch `json:"worktrees"`
		}{Schema: "1", Sandbox: sb.Name}
		for i, p := range sb.Phases {
			out.Worktrees = append(out.Worktrees, statusJSONBranch{
				Index:    i,
				Repo:     p.Repo,
				Branch:   p.Branch,
				Status:   string(p.Status),
				Worktree: p.Worktree,
			})
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	},
}

// resolveRepoArg accepts either an absolute path or the repo basename
// and returns the canonical absolute path stored on the sandbox. The
// basename form is the common case (`clawk worktree rebase <ticket>
// <repo>`) — paths only matter for cross-checking.
func resolveRepoArg(sb *config.Sandbox, repoArg string) (string, error) {
	// Absolute path: normalise and demand it match exactly.
	if filepath.IsAbs(repoArg) {
		abs, err := filepath.EvalSymlinks(repoArg)
		if err != nil {
			abs = repoArg
		}
		for _, p := range sb.Phases {
			if p.Repo == abs {
				return abs, nil
			}
		}
		return "", fmt.Errorf("repo %s not part of sandbox %q", repoArg, sb.DisplayName())
	}
	// Basename match. Disallow ambiguous matches (two repos sharing
	// the same basename would be a real conflict — fail loudly so
	// the user picks).
	var matches []string
	seen := map[string]bool{}
	for _, p := range sb.Phases {
		if filepath.Base(p.Repo) != repoArg {
			continue
		}
		if !seen[p.Repo] {
			seen[p.Repo] = true
			matches = append(matches, p.Repo)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no worktree for repo %q in sandbox %q", repoArg, sb.DisplayName())
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("repo basename %q matches multiple repos in sandbox %q (%v); pass an absolute path",
			repoArg, sb.DisplayName(), matches)
	}
}

// defaultBranchOnOrigin returns the default branch name (typically
// "main") for the given repo by inspecting `git symbolic-ref
// refs/remotes/origin/HEAD`. Falls back to "main" when origin/HEAD
// isn't set — most modern repos use main, and the user can always
// rebase manually when our guess is wrong.
func defaultBranchOnOrigin(repo string) string {
	out, err := exec.Command("git", "-C", repo,
		"symbolic-ref", "--short", "refs/remotes/origin/HEAD").Output()
	if err != nil {
		// origin/HEAD missing — best-effort default. Same heuristic
		// `gh` uses when probing the default branch.
		return "main"
	}
	ref := strings.TrimSpace(string(out))
	// `origin/main` → `main`
	if i := strings.Index(ref, "/"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}
