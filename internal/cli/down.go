package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(downCmd)
}

var downCmd = &cobra.Command{
	ValidArgsFunction: completeSandboxNames,
	Use:               "down [<name>]",
	Short:             "Stop a sandbox VM (defaults to the cwd-derived sandbox)",
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

		if status, _ := provider.Status(sb); !isRunning(status) {
			fmt.Printf("Sandbox %q is not running\n", sb.DisplayName())
		} else {
			// Graceful stop waits up to ~10s for the daemon to wind
			// down — narrate it instead of freezing the prompt.
			var progress sandbox.Progress = sandbox.PlainProgress{}
			if t := newProgressTracker(); t != nil {
				progress = t
			}
			progress.Step("Stopping sandbox %q", sb.DisplayName())
			start := time.Now()
			if err := provider.Stop(sb); err != nil {
				progress.Close()
				return fmt.Errorf("stopping VM: %w", err)
			}
			progress.StepDone("Sandbox %q is down (%.1fs)", sb.DisplayName(), time.Since(start).Seconds())
			progress.Close()
		}

		// `down` means the next boot is a cold one: discard any suspend
		// snapshot so a later up doesn't surprise-restore a guest the user
		// explicitly shut down. Also the documented way to throw away a
		// snapshot without booting it.
		if sDir := suspendStateDir(store.VMDir(sb.Name)); hasSuspendState(sDir) {
			if err := os.RemoveAll(sDir); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: discarding suspend snapshot: %v\n", err)
			} else {
				fmt.Println("Discarded the sandbox's suspend snapshot — the next boot is a cold one")
			}
		}

		// `down` is "set DesiredState=stopped, then converge" — record the
		// intent alongside the observed stop. An explicit down also clears
		// any idle-stop annotation: "stopped" with no reason means the user
		// asked for it.
		sb.DesiredState = config.VMStateStopped
		sb.VMState = config.VMStateStopped
		sb.StopReason = ""
		return store.Save(sb)
	},
}
