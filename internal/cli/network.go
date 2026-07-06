package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/netfilter"
	"github.com/clawkwork/clawk/internal/vzdctl"
	"github.com/spf13/cobra"
)

var (
	networkListJSON    bool
	networkDenialsJSON bool
)

func init() {
	rootCmd.AddCommand(networkCmd)
	networkCmd.AddCommand(networkAllowCmd)
	networkCmd.AddCommand(networkRemoveCmd)
	networkCmd.AddCommand(networkListCmd)
	networkCmd.AddCommand(networkAllowIPCmd)
	networkCmd.AddCommand(networkRemoveIPCmd)
	networkCmd.AddCommand(networkDenialsCmd)
	networkCmd.AddCommand(networkBlockCmd)
	networkListCmd.Flags().BoolVar(&networkListJSON, "json", false,
		"emit JSON (the only supported mode; human path is 'clawk status')")
	networkDenialsCmd.Flags().BoolVar(&networkDenialsJSON, "json", false,
		"emit JSON (stable schema for scripts)")
}

var networkCmd = &cobra.Command{
	Use:     "network",
	Aliases: []string{"net"},
	Short:   "Manage sandbox network policy (vz gvproxy ACL)",
}

// note: allow/deny edits are saved to the store and then pushed into a
// running vzd via its control socket, so they apply live under --provider
// vz; when the sandbox is down they apply on the next 'up'.

// applyNetworkPolicy pushes the just-saved ACL into the running daemon via
// its control socket. It reports what happened but never fails the command:
// the store is already updated, so the worst case is the old behavior —
// the edit applies on the next up.
func applyNetworkPolicy(cmd *cobra.Command, name string) {
	ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
	defer cancel()
	err := vzdctl.NewClient(vzdctl.SocketPath(store.VMDir(name))).Reload(ctx)
	switch {
	case err == nil:
		fmt.Fprintln(cmd.OutOrStdout(), "Applied to running sandbox.")
	case errors.Is(err, vzdctl.ErrNotRunning):
		fmt.Fprintln(cmd.OutOrStdout(), "Sandbox not running — applies on next 'clawk up'.")
	default:
		fmt.Fprintf(cmd.ErrOrStderr(),
			"clawk: live apply failed (%v) — applies on next 'clawk up'\n", err)
	}
}

// allowEntry is one classified argument to 'network allow': a domain pattern
// destined for AllowedDomains, or an IP/CIDR destined for AllowedIPs.
type allowEntry struct {
	value string
	isIP  bool
}

// classifyAllowEntry decides which allow-list bucket one CLI argument belongs
// to. Anything that looks like an address but doesn't parse — a malformed
// CIDR, an IP with an impossible octet, a host:port, a URL — is an error
// rather than a silent "domain" entry that would never match anything.
func classifyAllowEntry(arg string) (allowEntry, error) {
	s := strings.TrimSpace(arg)
	if s == "" {
		return allowEntry{}, fmt.Errorf("empty entry")
	}
	if strings.Contains(s, "://") {
		return allowEntry{}, fmt.Errorf(
			"%q: schemes aren't part of the policy — a grant covers every protocol and port; allow the bare hostname", s)
	}
	if strings.Contains(s, "/") {
		if _, err := netip.ParsePrefix(s); err != nil {
			return allowEntry{}, fmt.Errorf("%q is not a valid CIDR range", s)
		}
		return allowEntry{value: s, isIP: true}, nil
	}
	if _, err := netip.ParseAddr(s); err == nil {
		return allowEntry{value: s, isIP: true}, nil
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		return allowEntry{}, fmt.Errorf(
			"%q: ports aren't part of the policy — a grant covers every port and protocol; allow %q instead", s, host)
	}
	if strings.Contains(s, ":") {
		return allowEntry{}, fmt.Errorf("%q is not a valid IPv6 address", s)
	}
	if strings.Trim(s, "0123456789.") == "" {
		return allowEntry{}, fmt.Errorf("%q is not a valid IP address", s)
	}
	return allowEntry{value: strings.TrimSuffix(s, ".")}, nil
}

var networkAllowCmd = &cobra.Command{
	ValidArgsFunction: completeSandboxNames,
	Use:               "allow <sandbox> <host> [host...]",
	Short:             "Allow outbound access to domains, IPs, or CIDR ranges",
	Long: `allow adds destinations to the sandbox's outbound allow list. Each entry is
a domain (wildcards like *.example.com cover every subdomain), a literal IP,
or a CIDR range — mix them freely:

  clawk network allow demo '*.example.com' api.stripe.com 10.0.0.5 192.168.10.0/24

Quote wildcard patterns so the shell doesn't try to expand them.

A grant is destination-only: it covers every protocol (TCP, UDP — including
QUIC/HTTP-3 — and ICMP) on every port. Ports and URL schemes are not part
of the policy, so allow example.com, not https://example.com:443.`,
	Args: cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		sb, err := store.Load(args[0])
		if err != nil {
			return err
		}
		// Classify every entry before touching the store, so a typo in the
		// third argument doesn't half-apply the first two.
		entries := make([]allowEntry, 0, len(args)-1)
		for _, a := range args[1:] {
			e, err := classifyAllowEntry(a)
			if err != nil {
				return err
			}
			entries = append(entries, e)
		}

		custom := sb.Network.Block(config.BlockOriginCustom)
		domains := make(map[string]bool, len(custom.AllowDomains))
		for _, d := range custom.AllowDomains {
			domains[d] = true
		}
		ips := make(map[string]bool, len(custom.AllowIPs))
		for _, e := range custom.AllowIPs {
			ips[e] = true
		}
		out := cmd.OutOrStdout()
		for _, e := range entries {
			switch {
			case e.isIP && !ips[e.value]:
				custom.AllowIPs = append(custom.AllowIPs, e.value)
				ips[e.value] = true
				fmt.Fprintf(out, "Allowed: %s\n", e.value)
				// Older releases filed IPs handed to 'allow' under the
				// domain list; it lives in the IP list now.
				if domains[e.value] {
					custom.AllowDomains = slices.DeleteFunc(custom.AllowDomains,
						func(d string) bool { return d == e.value })
					delete(domains, e.value)
				}
			case !e.isIP && !domains[e.value]:
				custom.AllowDomains = append(custom.AllowDomains, e.value)
				domains[e.value] = true
				fmt.Fprintf(out, "Allowed: %s\n", e.value)
			}
		}
		if err := store.Save(sb); err != nil {
			return err
		}
		applyNetworkPolicy(cmd, sb.Name)
		return nil
	},
}

var networkRemoveCmd = &cobra.Command{
	ValidArgsFunction: completeSandboxNames,
	Use:               "remove <sandbox> <host> [host...]",
	// This command was briefly named "deny" pre-release — a misleading
	// name (it undoes an allow; adding a deny rule is 'network block'),
	// retired without an alias before anything shipped.
	Aliases: []string{"rm"},
	Short:   "Remove rules — allows or blocks — for domains, IPs, or CIDR ranges",
	Long: `remove deletes the sandbox's custom rules for the given entries, whichever
kind they are: an allowed destination goes back to the default
deny-with-prompt behavior, a blocked one stops being auto-denied. One
removal verb for every rule kind — 'clawk network list' shows what exists.`,
	Args: cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		sb, err := store.Load(args[0])
		if err != nil {
			return err
		}
		remove := make(map[string]bool, len(args)-1)
		for _, a := range args[1:] {
			a = strings.TrimSpace(a)
			remove[a] = true
			remove[strings.TrimSuffix(a, ".")] = true
		}
		out := cmd.OutOrStdout()
		matched := make(map[string]bool, len(remove))
		custom := sb.Network.Block(config.BlockOriginCustom)
		drop := func(list []string, kind string) []string {
			var kept []string
			for _, e := range list {
				if remove[e] {
					matched[e] = true
					fmt.Fprintf(out, "Removed: %s (was %s)\n", e, kind)
				} else {
					kept = append(kept, e)
				}
			}
			return kept
		}
		custom.AllowDomains = drop(custom.AllowDomains, "allowed")
		custom.AllowIPs = drop(custom.AllowIPs, "allowed")
		custom.DenyDomains = drop(custom.DenyDomains, "blocked")
		custom.DenyIPs = drop(custom.DenyIPs, "blocked")
		// remove only edits the CLI's own layer. An entry that still matches
		// from a lower layer (clawk.mod, a policy, the namespace) needs its
		// source edited — say so instead of silently doing nothing.
		for _, b := range sb.Network.Blocks {
			if b.Origin == config.BlockOriginCustom {
				continue
			}
			for _, e := range append(append([]string{}, b.AllowDomains...), b.AllowIPs...) {
				if remove[e] {
					matched[e] = true
					fmt.Fprintf(out, "Note: %s is also allowed by the %s layer — edit its source to remove it there.\n", e, b.Origin)
				}
			}
			for _, e := range append(append([]string{}, b.DenyDomains...), b.DenyIPs...) {
				if remove[e] {
					matched[e] = true
					fmt.Fprintf(out, "Note: %s is also blocked by the %s layer — edit its source to remove it there.\n", e, b.Origin)
				}
			}
		}
		// Args that matched nothing were the command silently "not
		// working" before — say so, and point at the rule list.
		for _, a := range args[1:] {
			a = strings.TrimSuffix(strings.TrimSpace(a), ".")
			if !matched[a] {
				fmt.Fprintf(out, "No rule for: %s ('clawk network list %s' shows what exists)\n", a, args[0])
			}
		}
		if err := store.Save(sb); err != nil {
			return err
		}
		applyNetworkPolicy(cmd, sb.Name)
		return nil
	},
}

var networkAllowIPCmd = &cobra.Command{
	ValidArgsFunction: completeSandboxNames,
	Use:               "allow-ip <sandbox> <ip-or-cidr> [ip-or-cidr...]",
	Short:             "Allow outbound access to specific IPs or CIDR ranges",
	// Hidden: 'network allow' accepts IPs and CIDRs directly; this survives
	// as a strict variant for scripts that want an error on anything else.
	Hidden: true,
	Args:   cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		sb, err := store.Load(args[0])
		if err != nil {
			return err
		}
		// Validate up front: a malformed entry saved here would fail the
		// daemon's policy parse on the next 'clawk up'.
		for _, e := range args[1:] {
			entry, err := classifyAllowEntry(e)
			if err != nil {
				return err
			}
			if !entry.isIP {
				return fmt.Errorf("%q is not an IP or CIDR range (use 'clawk network allow' for domains)", e)
			}
		}
		custom := sb.Network.Block(config.BlockOriginCustom)
		existing := make(map[string]bool, len(custom.AllowIPs))
		for _, e := range custom.AllowIPs {
			existing[e] = true
		}
		out := cmd.OutOrStdout()
		for _, e := range args[1:] {
			if !existing[e] {
				custom.AllowIPs = append(custom.AllowIPs, e)
				existing[e] = true
				fmt.Fprintf(out, "Allowed: %s\n", e)
			}
		}
		if err := store.Save(sb); err != nil {
			return err
		}
		applyNetworkPolicy(cmd, sb.Name)
		return nil
	},
}

var networkRemoveIPCmd = &cobra.Command{
	ValidArgsFunction: completeSandboxNames,
	Use:               "remove-ip <sandbox> <ip-or-cidr> [ip-or-cidr...]",
	Short:             "Remove IPs or CIDR ranges from the allow list",
	// Hidden alongside allow-ip; 'network remove' removes IPs and CIDRs too.
	Hidden: true,
	Args:   cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		sb, err := store.Load(args[0])
		if err != nil {
			return err
		}
		remove := make(map[string]bool, len(args)-1)
		for _, e := range args[1:] {
			remove[e] = true
		}
		custom := sb.Network.Block(config.BlockOriginCustom)
		var kept []string
		for _, e := range custom.AllowIPs {
			if remove[e] {
				fmt.Fprintf(cmd.OutOrStdout(), "Removed: %s\n", e)
			} else {
				kept = append(kept, e)
			}
		}
		custom.AllowIPs = kept
		if err := store.Save(sb); err != nil {
			return err
		}
		applyNetworkPolicy(cmd, sb.Name)
		return nil
	},
}

var networkBlockCmd = &cobra.Command{
	ValidArgsFunction: completeSandboxNames,
	Use:               "block <sandbox> <domain> [domain...]",
	Short:             "Block a domain and all its subdomains (auto-deny, never prompts)",
	Long: `block adds domains to the sandbox's deny list. A blocked domain — and every
subdomain of it — is refused outright: it overrides the allow list and, under
interactive watching, is denied without a prompt. Use it to silence noisy or
unwanted destinations (telemetry, trackers):

  clawk network block demo telemetry.example.com   # blocks example.com + *.example.com

Undo with 'clawk network remove'.`,
	Args: cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		sb, err := store.Load(args[0])
		if err != nil {
			return err
		}
		custom := sb.Network.Block(config.BlockOriginCustom)
		existing := make(map[string]bool, len(custom.DenyDomains))
		for _, d := range custom.DenyDomains {
			existing[d] = true
		}
		for _, d := range args[1:] {
			d = strings.TrimSuffix(strings.TrimSpace(d), ".")
			if d != "" && !existing[d] {
				custom.DenyDomains = append(custom.DenyDomains, d)
				existing[d] = true
				fmt.Fprintf(cmd.OutOrStdout(), "Blocked: %s (+ subdomains)\n", d)
			}
		}
		if err := store.Save(sb); err != nil {
			return err
		}
		applyNetworkPolicy(cmd, sb.Name)
		return nil
	},
}

var networkDenialsCmd = &cobra.Command{
	ValidArgsFunction: completeSandboxNames,
	Use:               "denials [<sandbox>]",
	Short:             "Show outbound connections the ACL has blocked, most recent first",
	Long: `denials reads the running daemon's denial ledger: every refused outbound
TCP connection, aggregated per destination host. The host column carries the
DNS name the guest resolved right before dialing when one was observed, so
the fix is usually a copy-paste:

  clawk network allow <sandbox> <host>

The ledger lives in the daemon, so the sandbox must be running; it resets on
'clawk down'.`,
	// Name optional like the other read verbs (status, doctor): "why is my
	// agent's request failing" is asked from inside the project directory,
	// where retyping the sandbox name is pure friction.
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name, err := resolveSandboxName(args)
		if err != nil {
			return err
		}
		sb, err := store.Load(name)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
		defer cancel()
		denials, err := vzdctl.NewClient(vzdctl.SocketPath(store.VMDir(sb.Name))).Denials(ctx)
		if err != nil {
			return fmt.Errorf("reading denial ledger: %w", err)
		}

		if networkDenialsJSON {
			out := struct {
				Schema  string             `json:"schema"`
				Sandbox string             `json:"sandbox"`
				Denials []netfilter.Denial `json:"denials"`
			}{Schema: "1", Sandbox: sb.Name, Denials: denials}
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(out)
		}

		if len(denials) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No blocked connections recorded.")
			return nil
		}
		tw := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "HOST\tLAST IP\tPORT\tCOUNT\tLAST SEEN\tRULE")
		for _, d := range denials {
			rule := d.Rule
			if rule == "" {
				rule = "-" // fail-closed default, no rule matched
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n",
				d.Host, d.IP, d.Port, d.Count, relativeAge(d.LastSeen), rule)
		}
		if err := tw.Flush(); err != nil {
			return fmt.Errorf("rendering denials table: %w", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(),
			"\nAllow one with: clawk network allow %s <host>\n", sb.Name)
		return nil
	},
}

// recentDenials best-effort fetches the live denial ledger for the status
// dashboard; nil when the daemon (or its control socket) is unavailable —
// the dashboard simply omits the Blocked line rather than erroring.
func recentDenials(name string) []netfilter.Denial {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	denials, err := vzdctl.NewClient(vzdctl.SocketPath(store.VMDir(name))).Denials(ctx)
	if err != nil {
		return nil
	}
	return denials
}

var networkListCmd = &cobra.Command{
	ValidArgsFunction: completeSandboxNames,
	Use:               "list <sandbox>",
	Short:             "List allowed domains and IPs (JSON-only — use 'clawk status' for human view)",
	Args:              cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// v2: human output for the network ACL lives inside
		// `clawk status`. `network list` survives only as a JSON
		// shim for scripts; without --json we exit with a pointer
		// rather than print human text scripts would have to re-parse.
		if !networkListJSON {
			return fmt.Errorf(
				"network list is JSON-only — pass --json (or use 'clawk status %s' for human output)",
				args[0])
		}
		sb, err := store.Load(args[0])
		if err != nil {
			return err
		}
		ns, err := store.LoadNamespace(sb.NamespaceName())
		if err != nil {
			ns = &config.Namespace{Name: sb.NamespaceName()}
		}
		// Schema 2: the layered policy. "use" is the effective reference
		// chain (lowest precedence first); "blocks" are the sandbox's own
		// stored layers above it. Policy contents resolve via
		// 'clawk policy show'.
		out := struct {
			Schema  string                `json:"schema"`
			Sandbox string                `json:"sandbox"`
			Use     []string              `json:"use"`
			Blocks  []config.NetworkBlock `json:"blocks"`
		}{
			Schema:  "2",
			Sandbox: sb.Name,
			Use:     effectiveUse(ns, sb),
			Blocks:  sb.Network.Blocks,
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	},
}
