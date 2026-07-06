package cli

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/spf13/cobra"
)

// listJSON requested via --json. Schema is part of the CLI contract;
// add fields freely, never remove or rename — consumers (the messaging
// bot, scripts) parse this in production.
const listJSONSchema = "1"

// sandboxKind groups sandboxes for the list view. Presence of an
// InPlace phase implies `clawk here` created it; otherwise it came
// from `run` (workflow-style, branch-scoped).
func sandboxKind(sb *config.Sandbox) string {
	for _, p := range sb.Phases {
		if p.InPlace {
			return "here"
		}
	}
	return "run"
}

var listJSON bool

func init() {
	rootCmd.AddCommand(listCmd)
	listCmd.Flags().BoolVar(&listJSON, "json", false,
		"emit JSON (stable schema; safe for scripts and the messaging bot)")
}

var listCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List all sandboxes",
	RunE: func(cmd *cobra.Command, args []string) error {
		sandboxes, err := store.List()
		if err != nil {
			return err
		}
		if listJSON {
			return emitListJSON(cmd, sandboxes)
		}
		if len(sandboxes) == 0 {
			fmt.Println("No sandboxes yet — run `clawk` in a project directory, " +
				"or `clawk work <ticket>` for a fresh worktree.")
			return nil
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tKIND\tPROVIDER\tIMAGE\tBRANCHES\tVM STATE\tCREATED")
		for i := range sandboxes {
			sb := &sandboxes[i]
			// Each sandbox remembers its own provider; query live status there.
			provider, err := providerFor(sb)
			state := string(sb.VMState)
			if err == nil {
				if live, err := provider.Status(sb); err == nil {
					state = live
				}
			}
			state = livePausedState(sb, state)
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
				sb.DisplayName(),
				sandboxKind(sb),
				sb.Provider,
				shortImageRef(sb.Image),
				len(sb.Phases),
				displayVMState(state, sb),
				sb.CreatedAt.Format("2006-01-02 15:04"),
			)
		}
		if err := w.Flush(); err != nil {
			return err
		}
		// The table shows sandboxes outlive the session but not how to get
		// back into one; a single trailing line names the resume verb so the
		// list is self-documenting without cluttering every row.
		fmt.Fprintln(cmd.OutOrStdout(), "\nreattach to any sandbox with 'clawk attach <name>'.")
		return nil
	},
}

// listEntry is the wire format for `clawk list --json`. Keep it
// stable: scripts and the messaging bot integration consume this.
type listEntry struct {
	Name      string `json:"name"`         // addressable store key
	Display   string `json:"display_name"` // human-facing label
	Namespace string `json:"namespace"`
	Anchor    string `json:"anchor,omitempty"` // bound directory, if any
	Kind      string `json:"kind"`             // "here" or "run"
	Provider  string `json:"provider"`
	Image     string `json:"image,omitempty"`
	Branches  int    `json:"branches"` // count
	VMState   string `json:"vm_state"`
	// StopReason qualifies a stopped VMState when the stop wasn't a user
	// verb (today: "idle" for the daemon's idle park). Additive field.
	StopReason string `json:"stop_reason,omitempty"`
	Created    string `json:"created"` // RFC 3339
}

type listJSONOutput struct {
	Schema    string      `json:"schema"`
	Sandboxes []listEntry `json:"sandboxes"`
}

func emitListJSON(cmd *cobra.Command, sandboxes []config.Sandbox) error {
	out := listJSONOutput{
		Schema:    listJSONSchema,
		Sandboxes: make([]listEntry, 0, len(sandboxes)),
	}
	for i := range sandboxes {
		sb := &sandboxes[i]
		state := string(sb.VMState)
		if provider, err := providerFor(sb); err == nil {
			if live, err := provider.Status(sb); err == nil {
				state = live
			}
		}
		state = livePausedState(sb, state)
		out.Sandboxes = append(out.Sandboxes, listEntry{
			Name:       sb.Name,
			Display:    sb.DisplayName(),
			Namespace:  sb.NamespaceName(),
			Anchor:     sb.Anchor,
			Image:      sb.Image,
			Kind:       sandboxKind(sb),
			Provider:   string(sb.Provider),
			Branches:   len(sb.Phases),
			VMState:    state,
			StopReason: stopReasonFor(state, sb),
			Created:    sb.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		})
	}
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// shortImageRef compresses an image reference for column display:
// registry noise is stripped (docker.io/, library/), tarball paths show
// their basename, and an unset image renders as a placeholder.
func shortImageRef(ref string) string {
	if ref == "" {
		return "(none)"
	}
	if strings.HasPrefix(ref, "/") || strings.HasPrefix(ref, "./") || strings.HasPrefix(ref, "../") {
		return filepath.Base(ref)
	}
	ref = strings.TrimPrefix(ref, "docker.io/")
	return strings.TrimPrefix(ref, "library/")
}
