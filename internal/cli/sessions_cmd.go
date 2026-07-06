package cli

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/sessions"
	"github.com/clawkwork/clawk/internal/template"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(sessionsCmd)
	sessionsCmd.AddCommand(sessionsMergeCmd)
	sessionsCmd.AddCommand(sessionsListCmd)
}

var sessionsCmd = &cobra.Command{
	Use:   "sessions",
	Short: "Manage portable Claude Code session history",
	Long: "Claude Code conversations are versioned per project as a git history.\n" +
		"Each sandbox keeps its own branch (preserved automatically on destroy);\n" +
		"`clawk sessions merge` is the explicit step that publishes a sandbox's\n" +
		"sessions into the project's canonical history that fresh sandboxes seed from.",
}

var sessionsMergeCmd = &cobra.Command{
	ValidArgsFunction: completeSandboxNames,
	Use:               "merge [<name>]",
	Short:             "Publish a sandbox's sessions into the project's canonical history",
	Long: "Folds a sandbox's preserved session branch into the project's main\n" +
		"history. Works whether the sandbox is still running or already destroyed\n" +
		"(its branch was preserved on teardown). Defaults to the cwd-derived sandbox.",
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name, err := resolveSandboxName(args)
		if err != nil {
			return err
		}

		// If the sandbox still exists, push its latest sessions first so the
		// publish captures everything; otherwise its branch was already
		// preserved on destroy and we publish that.
		projectID, err := sessionProjectForMerge(name)
		if err != nil {
			return err
		}
		bare, err := sessions.EnsureBare(store.HistoryDir(), projectID)
		if err != nil {
			return err
		}
		branch := sessions.BranchFor(name)
		if err := sessions.Publish(bare, branch); err != nil {
			return err
		}
		fmt.Printf("Published %q into the project's session history.\n", config.DisplayName(name))
		return nil
	},
}

var sessionsListCmd = &cobra.Command{
	ValidArgsFunction: completeSandboxNames,
	Use:               "list [<name>]",
	Short:             "List preserved session branches for the project and whether each is published",
	Args:              cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		projectID, err := sessionProjectForList(args)
		if err != nil {
			return err
		}
		bare, err := sessions.EnsureBare(store.HistoryDir(), projectID)
		if err != nil {
			return err
		}
		infos, err := sessions.ListBranches(bare)
		if err != nil {
			return err
		}
		if len(infos) == 0 {
			fmt.Printf("No preserved sessions for project %s yet.\n", projectID)
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "SANDBOX\tLAST ACTIVITY\tPUBLISHED")
		for _, in := range infos {
			pub := "no"
			if in.Published {
				pub = "yes"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", in.Sandbox, in.LastSeen, pub)
		}
		return w.Flush()
	},
}

// sessionProjectForMerge resolves the project id for `sessions merge`. A
// still-existing sandbox is authoritative (its persisted id handles multi-repo
// workspaces and survives a folder rename); after destroy we derive it from
// the current repo. When the sandbox is live we also push its latest sessions
// so the publish is complete.
func sessionProjectForMerge(name string) (string, error) {
	if sb, err := store.Load(name); err == nil {
		preserveSessionHistory(sb)
		return ensureSessionProject(sb)
	}
	return projectIDFromCwd()
}

// sessionProjectForList resolves the project id for `sessions list`: the named
// sandbox's project if one is given and exists, else the cwd repo's project.
func sessionProjectForList(args []string) (string, error) {
	if len(args) > 0 && args[0] != "" {
		if sb, err := store.Load(args[0]); err == nil {
			return ensureSessionProject(sb)
		}
	}
	return projectIDFromCwd()
}

// projectIDFromCwd derives the session project id from the git repo containing
// the current directory — the path-keyed identity a single-repo sandbox would
// compute. Used when the sandbox record is gone (destroyed) or omitted.
func projectIDFromCwd() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	root, err := template.FindGitRepo(cwd)
	if err != nil {
		return "", fmt.Errorf("%s is not a git repository — run from the project directory", cwd)
	}
	return sessions.ProjectID([]string{root}), nil
}
