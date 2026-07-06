package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/clawkwork/clawk/internal/vsockclient"
	"github.com/spf13/cobra"
)

// debugVshellCmd is a thin escape hatch that lets the user run any
// command inside the sandbox over the vsock transport — not just
// claude. Two uses:
//
//   - Debugging input handling: `clawk debug vshell <name> -- cat -v`
//     prints raw bytes for every key, so you can see exactly what
//     iTerm2 is sending through the vsock path.
//   - Quick poke around the guest.
//
// It deliberately has NO fallback path. If the agent socket isn't
// there, this fails fast — the whole point is to exercise the vsock
// path in isolation.
var debugVshellCmd = &cobra.Command{
	ValidArgsFunction: completeSandboxNames,
	Use:               "vshell <name> [-- command args...]",
	Short:             "Run a command inside the sandbox via the vsock agent (debug/escape hatch)",
	Long: `vshell connects to the in-guest clawk-pty-agent over vsock, allocates a
PTY, and runs the requested command. With no command, it runs the
guest user's login shell.

Examples:

  clawk debug vshell foo                    # interactive shell over vsock
  clawk debug vshell foo -- cat -v          # see raw bytes (great for
                                               # debugging key encoding)
  clawk debug vshell foo -- htop            # any TUI

This talks to the agent directly. If the vsock agent isn't running it
returns an error rather than falling back.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		name := args[0]
		_, sb, err := providerForName(name)
		if err != nil {
			return err
		}

		extra := args[1:]
		var cmdPath string
		var cmdArgs []string
		if len(extra) > 0 {
			cmdPath = extra[0]
			cmdArgs = extra[1:]
		}
		// Empty Cmd → agent runs /bin/bash.

		sockPath := filepath.Join(store.VMDir(sb.Name), "agent.sock")
		if _, err := os.Stat(sockPath); err != nil {
			return fmt.Errorf("agent socket %s not present (vsock disabled or sandbox not started): %w",
				sockPath, err)
		}

		cfg := vsockclient.Config{
			SocketPath: sockPath,
			Cmd:        cmdPath,
			Args:       cmdArgs,
			User:       sandbox.GuestUser,
			Env:        buildVSockEnv(),
		}
		code, err := vsockclient.Run(context.Background(), cfg)
		if err != nil {
			return err
		}
		if code != 0 {
			return &sandbox.ExitError{Code: code}
		}
		return nil
	},
}

func init() {
	debugCmd.AddCommand(debugVshellCmd)
}
