package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/spf13/cobra"
)

var forwardListJSON bool

func init() {
	rootCmd.AddCommand(forwardCmd)
	forwardCmd.AddCommand(forwardAddCmd)
	forwardCmd.AddCommand(forwardRemoveCmd)
	forwardCmd.AddCommand(forwardListCmd)
	forwardListCmd.Flags().BoolVar(&forwardListJSON, "json", false,
		"emit JSON (the only supported mode; human path is 'clawk status')")
}

var forwardCmd = &cobra.Command{
	Use:     "forward",
	Aliases: []string{"fwd"},
	Short:   "Manage host-to-guest port forwards (changes apply on next up)",
}

var forwardAddCmd = &cobra.Command{
	ValidArgsFunction: completeSandboxNames,
	Use:               "add <sandbox> <port-spec> [port-spec...]",
	Short:             "Expose a guest port on the host (on 127.0.0.1)",
	Long: `Port specs:
  3000       — host port 3000 forwards to guest port 3000
  8080:80    — host port 8080 forwards to guest port 80

Changes take effect on the next 'clawk up' (down + up is enough for vz).`,
	Args: cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		sb, err := store.Load(args[0])
		if err != nil {
			return err
		}
		for _, spec := range args[1:] {
			fwd, err := parsePortSpec(spec)
			if err != nil {
				return err
			}
			if forwardExists(sb.Forwards, fwd) {
				fmt.Printf("  (already forwarded: %s)\n", fwd)
				continue
			}
			sb.Forwards = append(sb.Forwards, fwd)
			fmt.Printf("Forward added: %s\n", fwd)
		}
		if err := store.Save(sb); err != nil {
			return err
		}
		if status, _ := mustProviderStatus(sb); isRunning(status) {
			fmt.Println("(restart sandbox for new forwards to take effect)")
		}
		return nil
	},
}

var forwardRemoveCmd = &cobra.Command{
	ValidArgsFunction: completeSandboxNames,
	Use:               "remove <sandbox> <port-spec> [port-spec...]",
	Aliases:           []string{"rm"},
	Short:             "Remove a host-to-guest forward",
	Args:              cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		sb, err := store.Load(args[0])
		if err != nil {
			return err
		}
		drop := make(map[config.PortForward]bool)
		for _, spec := range args[1:] {
			fwd, err := parsePortSpec(spec)
			if err != nil {
				return err
			}
			drop[fwd] = true
		}
		var kept []config.PortForward
		for _, f := range sb.Forwards {
			if drop[f] {
				fmt.Printf("Forward removed: %s\n", f)
			} else {
				kept = append(kept, f)
			}
		}
		sb.Forwards = kept
		return store.Save(sb)
	},
}

var forwardListCmd = &cobra.Command{
	ValidArgsFunction: completeSandboxNames,
	Use:               "list <sandbox>",
	Short:             "List configured port forwards (JSON-only — use 'clawk status' for human view)",
	Args:              cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// v2: human output for forwards lives inside `clawk status`.
		// `forward list` survives only as a JSON shim for scripts.
		// Without --json we exit 2 with a pointer rather than dump the
		// list to stdout — that way scripts that expect parseable output
		// fail loud instead of silently misparsing a flat string.
		if !forwardListJSON {
			return fmt.Errorf(
				"forward list is JSON-only — pass --json (or use 'clawk status %s' for human output)",
				args[0])
		}
		sb, err := store.Load(args[0])
		if err != nil {
			return err
		}
		out := struct {
			Schema   string              `json:"schema"`
			Sandbox  string              `json:"sandbox"`
			Forwards []statusJSONForward `json:"forwards"`
		}{Schema: "1", Sandbox: sb.Name}
		for _, f := range sb.Forwards {
			out.Forwards = append(out.Forwards, statusJSONForward{
				HostPort: f.HostPort, GuestPort: f.GuestPort,
			})
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	},
}

// parsePortSpec accepts "PORT" or "HOST:GUEST" and returns a PortForward.
// Both ports must be 1..65535. Using string parsing (not Sscanf) so bad input
// like "80:80:80" or "foo:80" rejects with a clear error rather than silently
// ignoring trailing garbage.
func parsePortSpec(spec string) (config.PortForward, error) {
	parts := strings.Split(spec, ":")
	switch len(parts) {
	case 1:
		p, err := parsePort(parts[0])
		if err != nil {
			return config.PortForward{}, fmt.Errorf("port %q: %w", spec, err)
		}
		return config.PortForward{HostPort: p, GuestPort: p}, nil
	case 2:
		host, err := parsePort(parts[0])
		if err != nil {
			return config.PortForward{}, fmt.Errorf("host port %q: %w", parts[0], err)
		}
		guest, err := parsePort(parts[1])
		if err != nil {
			return config.PortForward{}, fmt.Errorf("guest port %q: %w", parts[1], err)
		}
		return config.PortForward{HostPort: host, GuestPort: guest}, nil
	default:
		return config.PortForward{}, fmt.Errorf("invalid port spec %q (want PORT or HOST:GUEST)", spec)
	}
}

func parsePort(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("not a number: %w", err)
	}
	if n < 1 || n > 65535 {
		return 0, errors.New("out of range (1..65535)")
	}
	return n, nil
}

func forwardExists(fs []config.PortForward, f config.PortForward) bool {
	for _, e := range fs {
		if e == f {
			return true
		}
	}
	return false
}

// mustProviderStatus returns the live VM status, or the persisted state if
// no provider could be resolved. Used for user-facing hints only.
func mustProviderStatus(sb *config.Sandbox) (string, error) {
	p, err := providerFor(sb)
	if err != nil {
		return string(sb.VMState), err
	}
	return p.Status(sb)
}
