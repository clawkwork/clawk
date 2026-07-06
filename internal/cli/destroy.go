package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/clawkwork/clawk/internal/worktree"
	"github.com/spf13/cobra"
)

// indent prefixes every line in s with prefix so git's porcelain output
// visibly nests under its worktree heading when we print the dirty list.
func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

func init() {
	rootCmd.AddCommand(destroyCmd)
	destroyCmd.Flags().BoolVarP(&destroyForce, "force", "f", false,
		"destroy even if a phase worktree has uncommitted or untracked changes")
}

var destroyForce bool

var destroyCmd = &cobra.Command{
	ValidArgsFunction: completeSandboxNames,
	Use:               "destroy [<name>]",
	Short:             "Destroy a sandbox (VM + worktrees + network rules; cwd-sandbox by default)",
	Args:              cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name, err := resolveSandboxName(args)
		if err != nil {
			return err
		}
		provider, sb, err := providerForName(name)
		if err != nil {
			return err
		}

		// Guard against silent data loss: git worktree remove --force
		// (used below) strips uncommitted + untracked work. Check every
		// phase's worktree first and refuse unless --force is passed.
		// Nothing destructive has happened yet at this point, so it's
		// safe to bail.
		if !destroyForce {
			var dirty []string
			for _, p := range sb.Phases {
				if p.Worktree == "" || p.InPlace {
					// InPlace phases point at the user's real checkout
					// which we must never git-worktree-remove — let the
					// user manage its contents with their usual tools.
					continue
				}
				status, err := worktree.DirtyStatus(p.Worktree)
				if err != nil {
					return fmt.Errorf("checking %s: %w", p.Worktree, err)
				}
				if status != "" {
					dirty = append(dirty, fmt.Sprintf("  %s\n%s",
						p.Worktree, indent(status, "    ")))
				}
			}
			if len(dirty) > 0 {
				return fmt.Errorf(
					"refusing to destroy — uncommitted or untracked changes:\n\n%s\n\n"+
						"commit or discard the changes above, or re-run with --force to delete anyway",
					strings.Join(dirty, "\n"))
			}
		}

		warn := func(msg string, err error) {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s: %v\n", msg, err)
		}

		// Destroy stops the daemon first (a graceful stop can take ~10s)
		// — narrate it like clawk down does instead of freezing the
		// prompt. Worktree/config cleanup afterwards is instant.
		var progress sandbox.Progress = sandbox.PlainProgress{}
		if t := newProgressTracker(); t != nil {
			progress = t
		}
		progress.Step("Destroying sandbox %q", sb.DisplayName())
		start := time.Now()

		// Destroy the VM — report but continue so we still clean up worktrees/config
		if err := provider.Destroy(sb); err != nil {
			progress.Close()
			warn("destroying VM", err)
		}

		// Preserve this VM's conversation history on its own branch now that
		// the guest has stopped writing. Under the explicit-merge model this
		// does NOT publish to the project's canonical history — destroying a
		// throwaway VM keeps its sessions safe (publishable later via
		// `clawk sessions merge`) without polluting main. Best-effort; vz-only.
		preserveSessionHistory(sb)

		// Clean up git worktrees — InPlace phases are skipped because
		// their Worktree path IS the user's host directory; blowing it
		// away would delete their actual code.
		//
		// We always pass through to RemoveAll even when phases is empty,
		// so the on-disk base dir gets nuked regardless of how the saved
		// state ended up. This catches orphans left by a prior
		// `clawk work` that errored after creating a worktree but
		// before saving the sandbox config — destroy didn't know about
		// them, but their directories sit on disk and would otherwise
		// collide with the next `clawk work` for the same name.
		var phases []worktree.Phase
		for _, p := range sb.Phases {
			if p.InPlace {
				continue
			}
			phases = append(phases, worktree.Phase{
				Repo:     p.Repo,
				Branch:   p.Branch,
				Worktree: p.Worktree,
				Order:    p.Order,
			})
		}
		if err := worktree.RemoveAll(phases, store.WorktreeDir(name)); err != nil {
			warn("removing worktrees", err)
		}

		if err := store.Delete(name); err != nil {
			progress.Close()
			return err
		}
		progress.StepDone("Destroyed sandbox %q (%.1fs)", sb.DisplayName(), time.Since(start).Seconds())
		progress.Close()
		return nil
	},
}
