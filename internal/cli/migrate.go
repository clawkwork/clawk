package cli

import (
	"fmt"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(migrateCmd)
}

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Apply pending store migrations (idempotent)",
	// Hidden: the same migrations run automatically whenever the store opens,
	// so this verb is a support/debug tool, not part of the everyday surface.
	// Kept invokable for "re-run it by hand and show me what happened".
	Hidden: true,
	Long: `migrate replays any pending on-disk schema migrations in order:
record-shape fixes (retired provider names, missing fields) and the move to the
namespace-first layout (namespaces/<ns>/{sandboxes,vms,worktrees,state}/).

It is idempotent and safe to re-run. It first reconciles each sandbox's
recorded state against its provider (healing a stale "running" record), then
defers any sandbox that is genuinely running — stop those with 'clawk down' and
re-run. The same steps also run automatically when any clawk command opens the
store; this command reconciles live state and reports what happened.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		out := cmd.OutOrStdout()
		// Heal stale VMState first: the migration's "is it running?" gate reads
		// the persisted state, which can lag reality (a crashed or
		// out-of-band-stopped VM still recorded as running). Reconcile against
		// the provider — the same live truth `clawk list` shows — so we don't
		// wrongly defer a sandbox that isn't actually running.
		reconcileVMStates(out)
		if err := store.RunMigrations(out); err != nil {
			return err
		}
		fmt.Fprintf(out, "store schema v%d (target v%d)\n",
			store.SchemaVersion(), config.CurrentSchemaVersion())
		return nil
	},
}
