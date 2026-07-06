package cli

// `clawk work <ticket>` materialises a ticket sandbox from a template.
// The mental model: a sandbox is a ticket-instance materialised from a
// clawk.mod's sandbox block (multi-repo when it has `includes`).
// "work on TICKET-1234" reads naturally.

import "github.com/spf13/cobra"

func init() {
	rootCmd.AddCommand(workCmd)
	registerWorkFlags(workCmd)

	workCmd.Flags().BoolVar(&runBare, "bare", false,
		"prepare the sandbox but skip booting and attaching a runner "+
			"(use 'clawk run <runner> <ticket>' afterwards)")
	registerSafeFlag(workCmd)
}

var workCmd = &cobra.Command{
	Use:   "work [template] <ticket>",
	Short: "Materialise a ticket sandbox from a template and attach the default runner",
	Long: `work materialises a ticket sandbox from a template: the sandbox block
of a clawk.mod. A sandbox block with 'includes ( ... )' composes several
repos (a workspace); without it the file shapes its own repo. The template
is read once at create time and snapshotted into the sandbox record.
Editing it afterwards does not retro-modify existing sandboxes; each
sandbox is self-contained from creation.

If a sandbox with the target name already exists, work does not rebuild
it: it boots the VM if needed and attaches the default runner, without
re-reading the template. 'clawk attach <ticket>' is the direct resume
path and works from any directory.

Source resolution, in order:

  1. clawk.mod with 'includes' in CWD or any parent   — multi-repo workspace
  2. clawk.mod in CWD                                 — single-repo template
  3. CWD is inside a git repo (no clawk.mod)          — single-repo defaults
  4. <template> is a path to a clawk.mod              — explicit template

Examples:

  # auto-detected template; boots the VM and attaches claude
  clawk work INFRA-1234

  # limit to some repos
  clawk work INFRA-1234 --only k8s-deploy,monorepo

  # single-repo: run inside a cloned repo with a clawk.mod
  cd ~/code/api && clawk work feature-x

  # explicit template path
  clawk work ./path/to/clawk.mod INFRA-1234

  # use a non-default runner: prepare the sandbox without booting,
  # then attach the runner you want
  clawk work INFRA-1234 --bare
  clawk run codex INFRA-1234`,
	Args: cobra.RangeArgs(1, 2),
	RunE: runWorkCmd,
}
