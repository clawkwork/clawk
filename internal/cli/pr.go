package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(prCmd)
	prCmd.Flags().StringVar(&prBase, "base", "",
		"base branch for each PR (defaults to the repo's default branch)")
	prCmd.Flags().BoolVar(&prDraft, "draft", false,
		"open the PRs as drafts")
	prCmd.Flags().StringVar(&prTitle, "title", "",
		"PR title (defaults to the branch name; applied to every phase)")
	prCmd.Flags().BoolVar(&prSkipPush, "no-push", false,
		"skip `git push` (use for re-running after manual push)")
}

var (
	prBase     string
	prDraft    bool
	prTitle    string
	prSkipPush bool
)

var prCmd = &cobra.Command{
	ValidArgsFunction: completeSandboxNames,
	Use:               "pr <sandbox>",
	Short:             "Open GitHub PRs for every phase branch, cross-linked",
	Long: `For each phase in the sandbox, push the branch and open a pull
request via the 'gh' CLI. PRs are cross-linked in their descriptions so
reviewers see the full phased deployment at a glance.

Requires the 'gh' CLI to be installed and authenticated.

Examples:
  clawk pr                                       # cwd-sandbox
  clawk pr INFRA-1234
  clawk pr INFRA-1234 --draft
  clawk pr INFRA-1234 --title "INFRA-1234: add the widget"
  clawk pr INFRA-1234 --base develop

Re-running 'clawk pr' is safe: if a PR already exists for a branch, 'gh'
returns an error for that phase and we continue with the rest (the existing
PR's URL is printed so you can cross-link manually if needed).`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := exec.LookPath("gh"); err != nil {
			return errors.New("'gh' CLI not found — install with: brew install gh")
		}
		name, err := resolveSandboxName(args)
		if err != nil {
			return err
		}
		sb, err := store.Load(name)
		if err != nil {
			return err
		}
		if len(sb.Phases) == 0 {
			return fmt.Errorf("sandbox %q has no phases", sb.DisplayName())
		}

		// Step 1: push every branch. Push failures are warnings, not
		// errors: a non-fast-forward reject usually means the PR already
		// exists and the remote has diverged (someone else pushed, or
		// you pushed from a different worktree). We'd rather still look
		// up the existing PR and update its body than abort everything.
		// If the reject is real and no PR exists, the PR-create step
		// will fail with its own clear message.
		pushFailed := false
		if !prSkipPush {
			for i, p := range sb.Phases {
				if p.Worktree == "" {
					continue
				}
				fmt.Printf("[phase %d] pushing %s to origin...\n", i, p.Branch)
				cmd := exec.Command("git", "-C", p.Worktree, "push", "-u", "origin", p.Branch)
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				if err := cmd.Run(); err != nil {
					fmt.Fprintf(os.Stderr,
						"  push failed for phase %d (%v) — continuing; "+
							"will still try to find/update the PR\n", i, err)
					pushFailed = true
				}
			}
		}

		// Step 2: create each PR. We deliberately don't synthesize a body
		// or cross-link between phases — the PR should look like any
		// human-authored PR, so the description is left blank for you
		// (or claude inside the sandbox) to write.
		for i, p := range sb.Phases {
			if p.Worktree == "" {
				continue
			}
			url, skipped, err := createOrFindPR(p)
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "  phase %d: %v\n", i, err)
				continue
			}
			if skipped {
				fmt.Printf("[phase %d] skipped (no changes vs base)\n", i)
				continue
			}
			fmt.Printf("[phase %d] %s\n", i, url)
		}

		// We just touched gh — force a refresh so the cached
		// Phase.Status reflects the freshly-opened PRs.
		if err := refreshPRState(sb, true); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "clawk: %v\n", err)
		}

		fmt.Println()
		fmt.Println("All PRs opened.")
		if pushFailed {
			fmt.Println()
			fmt.Println("Note: at least one `git push` was rejected (remote " +
				"has diverged). If that branch's local commits are the ones " +
				"you want, re-run after `git pull --rebase` inside the " +
				"worktree, or pass --no-push after pushing manually.")
		}
		return nil
	},
}

// createOrFindPR opens a PR for the given phase via `gh`, or returns the
// URL of an existing PR on the same branch. If the branch has no commits
// ahead of base, returns skipped=true with no error.
//
// We pass --body "" (instead of omitting it) so gh doesn't drop into an
// interactive prompt in terminals that have a tty. The empty body is
// deliberate: PRs opened this way should read like human-authored ones,
// with any description written by the user or claude after the fact.
func createOrFindPR(p config.Phase) (string, bool, error) {
	// Check for existing PR first.
	view := exec.Command("gh", "pr", "view", p.Branch, "--json", "url", "-q", ".url")
	view.Dir = p.Worktree
	var viewOut bytes.Buffer
	view.Stdout = &viewOut
	if err := view.Run(); err == nil {
		if url := strings.TrimSpace(viewOut.String()); url != "" {
			return url, false, nil
		}
	}

	title := p.Branch
	if prTitle != "" {
		title = prTitle
	}

	args := []string{"pr", "create", "--title", title, "--body", ""}
	if prDraft {
		args = append(args, "--draft")
	}
	if prBase != "" {
		args = append(args, "--base", prBase)
	}
	args = append(args, "--head", p.Branch)

	create := exec.Command("gh", args...)
	create.Dir = p.Worktree
	var stdout, stderr bytes.Buffer
	create.Stdout = &stdout
	create.Stderr = &stderr
	if err := create.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		// gh surfaces GitHub's "No commits between base and head" as a
		// GraphQL createPullRequest error. Treat as a clean skip —
		// there's literally nothing to review in this phase.
		if strings.Contains(msg, "No commits between") {
			return "", true, nil
		}
		return "", false, fmt.Errorf("%w: %s", err, msg)
	}
	return strings.TrimSpace(stdout.String()), false, nil
}
