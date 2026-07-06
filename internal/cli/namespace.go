package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/spf13/cobra"
)

// namespaceFlag (--namespace) selects the namespace a newly-created sandbox
// belongs to. (-n is already taken by `image gc --dry-run`, so it's long-only.)
var namespaceFlag string

func init() {
	rootCmd.PersistentFlags().StringVar(&namespaceFlag, "namespace", "",
		"namespace for a new sandbox — groups it and applies that namespace's "+
			"allowlist/files (default \"default\")")
	// Hidden together with the namespace command subtree for the initial
	// release: the flag keeps working for anyone already using namespaces,
	// but the launch-facing help stays two-mode simple.
	_ = rootCmd.PersistentFlags().MarkHidden("namespace")
	rootCmd.AddCommand(namespaceCmd)
	namespaceCmd.AddCommand(namespaceLsCmd, namespaceCreateCmd, namespaceEditCmd)
}

// createNamespace is the namespace a newly-created sandbox is placed in: the
// --namespace flag if given, else the default.
func createNamespace() string {
	if namespaceFlag != "" {
		return namespaceFlag
	}
	return config.DefaultNamespace
}

func validateNamespaceName(name string) error {
	if name == "" {
		return errors.New("namespace name cannot be empty")
	}
	if strings.ContainsAny(name, "/\\ \t") {
		return fmt.Errorf("namespace %q must not contain slashes or whitespace", name)
	}
	return nil
}

// applyNamespaceDefaults merges the sandbox's namespace config (allowlist,
// files, shares, env) into sb. Additive and order-independent; a
// sandbox-specific entry wins over a namespace one on a guest-path conflict.
// Call after sb.Namespace is set and before Save.
func applyNamespaceDefaults(sb *config.Sandbox) error {
	ns, err := store.LoadNamespace(sb.NamespaceName())
	if err != nil {
		return err
	}
	// Network defaults are deliberately NOT copied here: the namespace's
	// allow/deny entries and Use references resolve live at up/reload
	// (resolveChain), so editing a namespace propagates to existing member
	// sandboxes instead of leaving stale flattened copies behind.
	sb.RequiredEnv = dedupStrings(append(sb.RequiredEnv, ns.Env...))
	sb.Files = appendMissingFiles(sb.Files, ns.Files)
	sb.Shares = appendMissingShares(sb.Shares, ns.Shares)
	// Namespace scope is broader than the sandbox's own (clawk.mod) entries,
	// so it reads first: namespace instructions, then sandbox-specific.
	sb.Instructions = append(append([]string{}, ns.Instructions...), sb.Instructions...)
	sb.Memory = joinMemory(ns.Memory, sb.Memory)
	return nil
}

// joinMemory concatenates non-empty memory seeds in scope order, separated by
// a blank line, so a namespace baseline precedes a repo's clawk.mod additions.
func joinMemory(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n\n")
}

func appendMissingFiles(have, add []config.HostFile) []config.HostFile {
	seen := make(map[string]bool, len(have))
	for _, f := range have {
		seen[f.GuestPath] = true
	}
	for _, f := range add {
		if !seen[f.GuestPath] {
			have = append(have, f)
			seen[f.GuestPath] = true
		}
	}
	return have
}

func appendMissingShares(have, add []config.HostShare) []config.HostShare {
	seen := make(map[string]bool, len(have))
	for _, sh := range have {
		seen[sh.GuestPath] = true
	}
	for _, sh := range add {
		if !seen[sh.GuestPath] {
			have = append(have, sh)
			seen[sh.GuestPath] = true
		}
	}
	return have
}

// namespaceCmd is hidden for the initial release: namespaces are a
// Kubernetes-flavored power feature kept working in code (sandboxes still
// resolve their namespace internally) but held back from the launch-facing
// help surface. Hiding the parent hides the whole subtree from help.
var namespaceCmd = &cobra.Command{
	Use:    "namespace",
	Short:  "Manage sandbox namespaces (per-namespace allowlist, files, …)",
	Args:   cobra.NoArgs,
	Hidden: true,
}

var namespaceLsCmd = &cobra.Command{
	Use:   "ls",
	Short: "List namespaces and their defaults",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		list, err := store.ListNamespaces()
		if err != nil {
			return err
		}
		cfg := map[string]config.Namespace{config.DefaultNamespace: {Name: config.DefaultNamespace}}
		for _, n := range list {
			cfg[n.Name] = n
		}
		names := make([]string, 0, len(cfg))
		for n := range cfg {
			names = append(names, n)
		}
		sort.Strings(names)

		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAMESPACE\tALLOW\tFILES\tSHARES\tENV")
		for _, n := range names {
			c := cfg[n]
			fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\n",
				n, len(c.AllowedDomains)+len(c.AllowedIPs), len(c.Files), len(c.Shares), len(c.Env))
		}
		return w.Flush()
	},
}

var namespaceCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create an empty namespace config",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		if err := validateNamespaceName(name); err != nil {
			return err
		}
		if store.NamespaceConfigExists(name) {
			return fmt.Errorf("namespace %q already has a config", name)
		}
		if err := store.SaveNamespace(&config.Namespace{Name: name}); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(),
			"created namespace %q — add an allowlist/files with: clawk namespace edit %s\n", name, name)
		return nil
	},
}

var namespaceEditCmd = &cobra.Command{
	Use:   "edit <name>",
	Short: "Edit a namespace's config (allowlist, files, …) in $EDITOR",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		if err := validateNamespaceName(name); err != nil {
			return err
		}
		// Materialize the record so the editor opens a real (possibly empty) file.
		if !store.NamespaceConfigExists(name) {
			if err := store.SaveNamespace(&config.Namespace{Name: name}); err != nil {
				return err
			}
		}
		editor := os.Getenv("EDITOR")
		if editor == "" {
			editor = "vi"
		}
		c := exec.Command(editor, store.NamespaceConfigPath(name))
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		return c.Run()
	},
}
