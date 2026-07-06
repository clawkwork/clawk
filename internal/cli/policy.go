package cli

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/spf13/cobra"
)

// The policy surface manages named, reusable network-policy blocks — the
// layers a sandbox references with `use <name>` (see resolveChain). Policies
// are created by applying manifests (`clawk apply`) or registered
// automatically for `deny source` URLs; this command group covers the
// day-two verbs: inspect, refresh, remove.

var policyShowJSON bool

func init() {
	rootCmd.AddCommand(policyCmd)
	policyCmd.AddCommand(policyListCmd)
	policyCmd.AddCommand(policyShowCmd)
	policyCmd.AddCommand(policyRefreshCmd)
	policyCmd.AddCommand(policyDeleteCmd)
	policyShowCmd.Flags().BoolVar(&policyShowJSON, "json", false,
		"emit JSON (stable schema for scripts)")
}

var policyCmd = &cobra.Command{
	Use:   "policy",
	Short: "Manage named network policies referenced by 'use'",
}

var policyListCmd = &cobra.Command{
	Use:   "list",
	Short: "List stored policies (the built-in 'default' is always available)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		policies, err := store.ListPolicies()
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tRULES\tSOURCE\tFETCHED")
		builtin := config.BuiltinDefaultPolicy()
		fmt.Fprintf(tw, "%s\t%d\t(built-in)\t-\n", builtin.Name, len(builtin.AllowDomains))
		for _, p := range policies {
			cache, err := store.LoadPolicyCache(p.Name)
			if err != nil {
				return err
			}
			rules := len(p.AllowDomains) + len(p.AllowIPs) + len(p.DenyDomains) + len(p.DenyIPs) +
				len(cache.AllowDomains) + len(cache.DenyDomains)
			source, fetched := "-", "-"
			if p.Source != "" {
				source = p.Source
				if !cache.FetchedAt.IsZero() {
					fetched = relativeAge(cache.FetchedAt)
				} else {
					fetched = "never"
				}
			}
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", p.Name, rules, source, fetched)
		}
		return tw.Flush()
	},
}

var policyShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Show a policy's rules, including fetched blocklist entries",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		p, err := store.LoadPolicy(args[0])
		if err != nil {
			return err
		}
		cache, err := store.LoadPolicyCache(p.Name)
		if err != nil {
			return err
		}
		if policyShowJSON {
			out := struct {
				Schema string              `json:"schema"`
				Policy *config.Policy      `json:"policy"`
				Cache  *config.PolicyCache `json:"cache,omitempty"`
			}{Schema: "1", Policy: p}
			if p.Source != "" {
				out.Cache = cache
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(out)
		}
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "Policy %s\n", p.Name)
		printEntries := func(label string, entries []string) {
			if len(entries) == 0 {
				return
			}
			// Blocklist caches run to hundreds of thousands of entries;
			// preview a handful and point at --json for the full set.
			const previewMax = 10
			preview := entries
			suffix := ""
			if len(entries) > previewMax {
				preview = entries[:previewMax]
				suffix = fmt.Sprintf("  … (+%d more — use --json)\n", len(entries)-previewMax)
			}
			fmt.Fprintf(out, "  %s (%d):\n", label, len(entries))
			for _, e := range preview {
				fmt.Fprintf(out, "    %s\n", e)
			}
			fmt.Fprint(out, suffix)
		}
		printEntries("allow", append(append([]string{}, p.AllowDomains...), p.AllowIPs...))
		printEntries("deny", append(append([]string{}, p.DenyDomains...), p.DenyIPs...))
		if p.Source != "" {
			fmt.Fprintf(out, "  source: %s\n", p.Source)
			if cache.FetchedAt.IsZero() {
				fmt.Fprintln(out, "  fetched: never — run 'clawk policy refresh'")
			} else {
				fmt.Fprintf(out, "  fetched: %s\n", relativeAge(cache.FetchedAt))
			}
			printEntries("fetched deny", cache.DenyDomains)
			printEntries("fetched allow (list exceptions)", cache.AllowDomains)
		}
		return nil
	},
}

var policyRefreshCmd = &cobra.Command{
	Use:   "refresh <name>",
	Short: "Refetch a source-backed policy's blocklist now",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		p, err := store.LoadPolicy(args[0])
		if err != nil {
			return err
		}
		if p.Source == "" {
			return fmt.Errorf("policy %q has no source URL — nothing to refresh", p.Name)
		}
		if err := refreshPolicyCache(p, true); err != nil {
			return err
		}
		cache, err := store.LoadPolicyCache(p.Name)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Fetched %s: %d denies, %d exception allows.\n",
			p.Source, len(cache.DenyDomains), len(cache.AllowDomains))
		fmt.Fprintln(cmd.OutOrStdout(),
			"Running sandboxes pick it up on their next reload ('clawk network allow' or 'clawk up').")
		return nil
	},
}

var policyDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Remove a stored policy (sandboxes still referencing it lose that layer)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := store.DeletePolicy(args[0]); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(),
			"Deleted policy %q. Sandboxes referencing it now resolve it as an absent layer (fail-closed).\n",
			args[0])
		return nil
	},
}
