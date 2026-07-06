package cli

// `clawk run <runner> [<sandbox>] [-- <args>]` — the v2 surface that
// replaces flat per-runner verbs (`clawk claude`, `clawk codex`,
// ...). One verb scales as the agent registry grows; sandbox names no
// longer share a keyspace with runner names.

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(runVerbCmd)
	registerSafeFlag(runVerbCmd)
}

var runVerbCmd = &cobra.Command{
	ValidArgsFunction: completeRunArgs,
	Use:               "run <runner> [<sandbox>] [-- <runner args>]",
	Short:             "Attach a coding-agent runner to a sandbox",
	Long: `run dispatches to a coding agent inside a sandbox over the
provider's vsock agent transport (both vz and firecracker; no sshd).

  clawk run claude                      cwd-sandbox + claude
  clawk run claude foo                  sandbox foo + claude
  clawk run claude -- --resume          cwd-sandbox + claude --resume
  clawk run claude foo -- --resume      sandbox foo + claude --resume
  clawk run shell                       cwd-sandbox + interactive bash`,
	Args: cobra.ArbitraryArgs,
	// Same rationale as the runner verbs: a transient vsock disconnect
	// or VM-not-running shouldn't bury the actual error under help.
	SilenceUsage: true,
	RunE:         runVerbRunE,
}

// splitDashArgs splits a runner-style arg list at the `--` marker: positional
// args before it, verbatim runner passthrough after it. Centralised because
// `clawk run` and `clawk attach` both speak this shape and the split's edge
// cases (no dash, dash first) must stay identical between them.
func splitDashArgs(cmd *cobra.Command, args []string) (positional, extra []string) {
	dashAt := cmd.ArgsLenAtDash()
	if dashAt == -1 {
		return args, nil
	}
	return args[:dashAt], args[dashAt:]
}

func runVerbRunE(cmd *cobra.Command, args []string) error {
	positional, extra := splitDashArgs(cmd, args)
	if len(positional) == 0 {
		return errors.New("missing runner name; usage: clawk run <runner> [<sandbox>] [-- <args>]")
	}

	runnerName := positional[0]
	var sandboxName string
	if len(positional) >= 2 {
		sandboxName = positional[1]
	}

	resolved, err := resolveSandboxName(maybeName(sandboxName))
	if err != nil {
		return err
	}
	emitCwdShadowHintIfInferred(cmd.ErrOrStderr(), sandboxName)
	provider, sb, err := providerForName(resolved)
	if err != nil {
		return err
	}

	// Validate the runner before any boot work: a typo'd runner name on a
	// stopped sandbox should fail on the typo, not after paying for a VM
	// bring-up it never needed. shell isn't in the agent registry — it's a
	// fixed `/bin/bash -l` dispatch in shell.go, special-cased so `run shell`
	// is indistinguishable from the legacy `clawk shell`.
	isShell := runnerName == "shell"
	var a Agent
	if isShell {
		if len(extra) > 0 {
			return fmt.Errorf("clawk run shell does not accept passthrough args (got %v)", extra)
		}
	} else if a, err = agentByName(runnerName); err != nil {
		return err
	}

	// Boot a stopped sandbox before attaching rather than refusing. "Not
	// running" is a recoverable state, not a user error, and both the shell
	// and the agent attaches below need a live VM. ensureUp is a no-op when
	// the VM is already up (and admission-gated when it isn't), so the happy
	// path pays nothing.
	if err := ensureUp(cmd.ErrOrStderr(), sb); err != nil {
		return err
	}

	if isShell {
		if err := runShellSession(sb, provider); err != nil {
			return err
		}
		printDetachHint(cmd.ErrOrStderr(), sb)
		return nil
	}

	if err := runAgentSession(sb, provider, applySafeMode(a), extra); err != nil {
		return err
	}
	printDetachHint(cmd.ErrOrStderr(), sb)
	return nil
}
