package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/spf13/cobra"
)

// v2 turns `clawk status` into the canonical "what's going on with this
// sandbox" view: VM state, worktrees, forwards, network ACL, setup
// summary — all folded into one screen. The flat tabwriter output that
// shipped in v1 lives on as `--brief` for greppable scripting; the
// stable wire schema continues under `--json` (schema bumped to "2"
// when the new fields landed, additive only — old consumers keep
// working).
//
// The dashboard intentionally renders read-only state pulled from the
// sandbox record + provider. Phase 6 of the v2 redesign will refresh
// the PR-derived bits (status badges per worktree) by calling `gh`;
// the layout is shaped to leave room for them.

var (
	statusJSON  bool
	statusBrief bool
)

func init() {
	rootCmd.AddCommand(statusCmd)
	statusCmd.Flags().BoolVar(&statusJSON, "json", false,
		"emit JSON (stable schema for scripts)")
	statusCmd.Flags().BoolVar(&statusBrief, "brief", false,
		"one-line greppable summary (single sandbox per line)")
}

const statusJSONSchema = "2"

// statusJSONOutput is the wire schema for `clawk status --json`. The
// `forwards`, `network`, and `setup` blocks are v2 additions; older
// consumers that ignore unknown fields keep working.
type statusJSONOutput struct {
	Schema    string `json:"schema"`
	Name      string `json:"name"`         // addressable store key
	Display   string `json:"display_name"` // human-facing label
	Namespace string `json:"namespace"`
	Anchor    string `json:"anchor,omitempty"` // bound directory, if any
	Mode      string `json:"mode"`             // "cwd" or "ticket"
	Provider  string `json:"provider"`
	Image     string `json:"image,omitempty"`
	VMState   string `json:"vm_state"`
	// StopReason qualifies a stopped vm_state when the stop wasn't a user
	// verb (today: "idle" for the daemon's idle park). Additive field.
	StopReason string             `json:"stop_reason,omitempty"`
	Pid        int                `json:"pid,omitempty"`
	GuestIP    string             `json:"guest_ip,omitempty"`
	MAC        string             `json:"mac,omitempty"`
	Created    string             `json:"created"`
	Branches   []statusJSONBranch `json:"branches"`

	// v2 additive blocks.
	Forwards []statusJSONForward `json:"forwards,omitempty"`
	Network  *statusJSONNetwork  `json:"network,omitempty"`
	Setup    []statusJSONSetup   `json:"setup,omitempty"`
}

type statusJSONBranch struct {
	Index    int    `json:"index"`
	Repo     string `json:"repo"`
	Branch   string `json:"branch"`
	Status   string `json:"status"`
	Worktree string `json:"worktree,omitempty"`
}

type statusJSONForward struct {
	HostPort  int `json:"host_port"`
	GuestPort int `json:"guest_port"`
}

type statusJSONNetwork struct {
	// Use is the effective policy-reference chain, lowest precedence
	// first; Blocks are the sandbox's own layers above it.
	Use    []string              `json:"use"`
	Blocks []config.NetworkBlock `json:"blocks,omitempty"`
}

type statusJSONSetup struct {
	Repo  string `json:"repo"`
	Steps int    `json:"steps"`
}

var statusCmd = &cobra.Command{
	ValidArgsFunction: completeSandboxNames,
	Use:               "status [<name>]",
	Short:             "Show sandbox dashboard (cwd-sandbox by default)",
	Long: `status prints one screen of sandbox state: VM, worktrees, forwards,
network ACL summary, and the configured setup steps.

  --brief   one greppable line (good for shell pipelines)
  --json    stable schema, currently version 2 (additive over v1)`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if statusJSON && statusBrief {
			return errors.New("--json and --brief are mutually exclusive")
		}
		name, err := resolveSandboxName(args)
		if err != nil {
			return err
		}
		provider, sb, err := providerForName(name)
		if err != nil {
			return err
		}
		liveStatus, statusErr := provider.Status(sb)
		if statusErr != nil {
			liveStatus = string(sb.VMState)
		}
		// The provider only sees the daemon process, which is alive for a
		// paused VM too — overlay the daemon's own lifecycle knowledge so
		// the dashboard says "paused" rather than "running".
		liveStatus = livePausedState(sb, liveStatus)

		// v2: Phase.Status is derived from gh. We refresh on every
		// `status` call but the 60-second cache absorbs spam. Errors
		// from gh are non-fatal — the dashboard still renders with
		// whatever cached values exist.
		if err := refreshPRState(sb, false); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "clawk: %v\n", err)
		}

		switch {
		case statusJSON:
			return renderStatusJSON(cmd.OutOrStdout(), sb, liveStatus)
		case statusBrief:
			return renderStatusBrief(cmd.OutOrStdout(), sb, liveStatus)
		default:
			return renderStatusDashboard(cmd.OutOrStdout(), provider, sb, liveStatus)
		}
	},
}

// renderStatusJSON emits the wire schema. Order of fields here is
// stable; consumers parse by key, but stability also makes diffs
// readable when the schema is committed to a fixture.
func renderStatusJSON(w io.Writer, sb *config.Sandbox, liveStatus string) error {
	out := statusJSONOutput{
		Schema:     statusJSONSchema,
		Name:       sb.Name,
		Display:    sb.DisplayName(),
		Namespace:  sb.NamespaceName(),
		Anchor:     sb.Anchor,
		Mode:       sandboxMode(sb),
		Provider:   string(sb.Provider),
		Image:      sb.Image,
		VMState:    liveStatus,
		StopReason: stopReasonFor(liveStatus, sb),
		Pid:        sb.VMPid,
		GuestIP:    sb.GuestIP,
		MAC:        sb.MACAddress,
		Created:    sb.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	for i, p := range sb.Phases {
		out.Branches = append(out.Branches, statusJSONBranch{
			Index:    i,
			Repo:     p.Repo,
			Branch:   p.Branch,
			Status:   string(p.Status),
			Worktree: p.Worktree,
		})
	}
	for _, f := range sb.Forwards {
		out.Forwards = append(out.Forwards, statusJSONForward{
			HostPort: f.HostPort, GuestPort: f.GuestPort,
		})
	}
	out.Network = &statusJSONNetwork{
		Use:    effectiveUseForLog(sb),
		Blocks: sb.Network.Blocks,
	}
	for _, p := range sb.Phases {
		if len(p.Setup) == 0 {
			continue
		}
		out.Setup = append(out.Setup, statusJSONSetup{
			Repo:  filepath.Base(p.Repo),
			Steps: len(p.Setup),
		})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// renderStatusBrief is the single-line greppable form. Tabs separate
// columns so awk/cut work without quoting concerns. Missing fields
// render as "-" rather than empty so column counts stay stable.
func renderStatusBrief(w io.Writer, sb *config.Sandbox, liveStatus string) error {
	pid := "-"
	if sb.VMPid > 0 {
		pid = fmt.Sprintf("pid=%d", sb.VMPid)
	}
	_, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d worktrees\t%s\n",
		sb.DisplayName(), displayVMState(liveStatus, sb), sandboxMode(sb), sb.Provider,
		len(sb.Phases), pid)
	return err
}

// renderStatusDashboard is the default human view. Layout intentionally
// mirrors the design doc — each subsection labelled, two-space indent
// for child rows, blank lines only between top-level groups so the
// whole thing fits in one terminal screen for typical sandboxes.
func renderStatusDashboard(w io.Writer, provider sandbox.Provider, sb *config.Sandbox, liveStatus string) error {
	age := relativeAge(sb.CreatedAt)
	mode := sandboxMode(sb)

	// Header line: name / state / mode / age / provider. Compact so
	// `clawk status` followed by a few `--brief` greps reads naturally.
	fmt.Fprintf(w, "%s    %s    %s    created %s    provider %s\n",
		sb.DisplayName(), displayVMState(liveStatus, sb), mode, age, sb.Provider)
	if sb.Image != "" {
		fmt.Fprintf(w, "  image  %s\n", sb.Image)
	}

	if sb.GuestIP != "" || sb.VMPid > 0 {
		var bits []string
		if sb.VMPid > 0 {
			bits = append(bits, fmt.Sprintf("pid %d", sb.VMPid))
		}
		if sb.GuestIP != "" {
			bits = append(bits, "ip "+sb.GuestIP)
		}
		if sb.MACAddress != "" {
			bits = append(bits, "mac "+sb.MACAddress)
		}
		fmt.Fprintf(w, "  %s\n", strings.Join(bits, "  "))
	}

	fmt.Fprintln(w, "  Worktrees")
	if len(sb.Phases) == 0 {
		fmt.Fprintln(w, "    (none — use 'clawk worktree add' to attach a repo)")
	} else {
		for _, p := range sb.Phases {
			repoName := filepath.Base(p.Repo)
			wt := p.Worktree
			if wt == "" {
				wt = "-"
			}
			fmt.Fprintf(w, "    %-24s  %-24s  %-8s  %s\n",
				repoName, p.Branch, p.Status, wt)
		}
	}

	if len(sb.Forwards) > 0 {
		parts := make([]string, 0, len(sb.Forwards))
		for _, f := range sb.Forwards {
			parts = append(parts, fmt.Sprintf("%d → %d", f.HostPort, f.GuestPort))
		}
		fmt.Fprintf(w, "  Forwards    %s\n", strings.Join(parts, ", "))
	}

	// One line per policy layer, lowest precedence first: the use chain,
	// then the sandbox's own blocks with entry counts. Full contents live
	// in `network list --json` and `clawk policy show`.
	fmt.Fprintf(w, "  Network     use: %s\n", strings.Join(effectiveUseForLog(sb), " → "))
	for _, b := range sb.Network.Blocks {
		var parts []string
		if n := len(b.AllowDomains) + len(b.AllowIPs); n > 0 {
			parts = append(parts, fmt.Sprintf("%d allowed", n))
		}
		if n := len(b.DenyDomains) + len(b.DenyIPs); n > 0 {
			parts = append(parts, fmt.Sprintf("%d denied", n))
		}
		if len(parts) == 0 {
			continue
		}
		fmt.Fprintf(w, "              %s layer: %s\n", b.Origin, strings.Join(parts, ", "))
	}

	// Live denial ledger, best-effort from the daemon's control socket.
	// Top three hosts keep the dashboard one-screen; the full table is
	// `clawk network denials`.
	if isRunning(liveStatus) {
		if denials := recentDenials(sb.Name); len(denials) > 0 {
			parts := make([]string, 0, 3)
			for _, d := range denials[:min(3, len(denials))] {
				parts = append(parts, fmt.Sprintf("%s (×%d, %s)",
					d.Host, d.Count, relativeAge(d.LastSeen)))
			}
			suffix := ""
			if len(denials) > 3 {
				suffix = fmt.Sprintf(", … (+%d more — 'clawk network denials')",
					len(denials)-3)
			}
			fmt.Fprintf(w, "  Blocked     %s%s\n", strings.Join(parts, ", "), suffix)
		}
	}

	var setupBits []string
	for _, p := range sb.Phases {
		if len(p.Setup) == 0 {
			continue
		}
		setupBits = append(setupBits, fmt.Sprintf("%s (%d)",
			filepath.Base(p.Repo), len(p.Setup)))
	}
	if len(setupBits) > 0 {
		fmt.Fprintf(w, "  Setup       %s\n", strings.Join(setupBits, ", "))
	}

	return nil
}

// sandboxMode returns "cwd" for directory-bound (anchored) sandboxes and
// "ticket" otherwise. The distinction surfaces in --json (so chat-bots can
// group views) and in the dashboard header.
func sandboxMode(sb *config.Sandbox) string {
	if isAnchored(sb) {
		return "cwd"
	}
	return "ticket"
}

// relativeAge produces "1h12m ago" / "5m ago" / "just now" for the
// dashboard header. Kept loose because absolute timestamps still ship
// via --json; this is the human glance.
func relativeAge(t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	d := time.Since(t).Round(time.Minute)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) - 60*h
		if m == 0 {
			return fmt.Sprintf("%dh ago", h)
		}
		return fmt.Sprintf("%dh%dm ago", h, m)
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
