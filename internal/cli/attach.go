package cli

// `clawk attach [<name>] [-- <runner args>]` — the one-word answer to
// "get me back into a sandbox". Unlike `clawk` (cwd mode) and `clawk work`
// (ticket mode), attach never creates and never reads a template: it is
// purely a resume verb, so it works from any directory when given a name.

import (
	"fmt"

	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(attachCmd)
	registerSafeFlag(attachCmd)
}

var attachCmd = &cobra.Command{
	ValidArgsFunction: completeSandboxNames,
	Use:               "attach [<name>] [-- <runner args>]",
	Short:             "Reattach the default runner to an existing sandbox, booting it first if needed",
	Long: `attach reattaches the default runner (claude) to a sandbox that
already exists. It is the universal resume verb: it never creates a
sandbox and never re-reads a template — it loads the record, boots the VM
if it isn't running, and attaches the runner. Given a name it works from
any directory; with no name it resolves the cwd-derived sandbox.

  clawk attach                  cwd-sandbox + claude
  clawk attach foo              sandbox foo + claude, from any directory
  clawk attach foo -- --resume  sandbox foo + claude --resume

To create a sandbox, use 'clawk' (cwd mode) or 'clawk work <ticket>'
(ticket mode); attach only ever resumes what those already made.`,
	Args: cobra.ArbitraryArgs,
	// Same rationale as the runner verbs: a boot failure or vsock disconnect
	// shouldn't bury the real error under the full command tree.
	SilenceUsage: true,
	RunE:         runAttachCmd,
}

func runAttachCmd(cmd *cobra.Command, args []string) error {
	positional, extra := splitDashArgs(cmd, args)
	if len(positional) > 1 {
		return fmt.Errorf(
			"attach takes at most one sandbox name (got %v); usage: clawk attach [<name>] [-- <runner args>]",
			positional)
	}
	var name string
	if len(positional) == 1 {
		name = positional[0]
	}

	resolved, err := resolveSandboxName(maybeName(name))
	if err != nil {
		return err
	}
	emitCwdShadowHintIfInferred(cmd.ErrOrStderr(), name)

	sb, err := store.Load(resolved)
	if err != nil {
		return fmt.Errorf(
			"%w — create one with 'clawk' (cwd mode) or 'clawk work <ticket>' (ticket mode)", err)
	}
	if err := ensureUp(cmd.ErrOrStderr(), sb); err != nil {
		return err
	}
	if err := attachDefaultAgent(sb, extra...); err != nil {
		return err
	}
	printDetachHint(cmd.ErrOrStderr(), sb)
	return nil
}
