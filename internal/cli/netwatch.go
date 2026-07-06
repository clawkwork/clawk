package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/netfilter"
	"github.com/clawkwork/clawk/internal/vzdctl"
	"github.com/spf13/cobra"
)

func init() { networkCmd.AddCommand(networkWatchCmd) }

// decider is the slice of the control-socket client that the watch loop
// needs: subscribe to gate events, list outstanding holds, and resolve one.
// Defined here, at the point of use, so the loop is testable with a fake.
type decider interface {
	Events(ctx context.Context) (<-chan netfilter.Event, error)
	Pending(ctx context.Context) ([]netfilter.Pending, error)
	Decide(ctx context.Context, id, action, scope string) error
	AllowAll(ctx context.Context, d time.Duration) error
}

// watchTarget pairs a sandbox name with its control-socket client.
type watchTarget struct {
	name   string
	client decider
}

// taggedEvent is a gate event annotated with the sandbox it came from, so the
// merged loop can route the decision back to the right socket.
type taggedEvent struct {
	vm string
	ev netfilter.Event
}

// allowAllWindow is how long the "allow all" escape hatch opens egress for.
const allowAllWindow = time.Hour

var networkWatchCmd = &cobra.Command{
	Use:   "watch [sandbox]",
	Short: "Interactively allow or deny blocked connections as they happen",
	Long: `watch attaches to running sandboxes and prompts you in the terminal the
moment the agent reaches for a host that isn't allowed. The connection is held
open while you decide, so an "allow" lets the agent's original attempt through
instead of leaving it to fail.

With a sandbox name it watches that one; with no argument it watches every
running sandbox at once.

  a  allow once     — let this attempt through; prompt again next time
  s  allow session  — allow this destination until the sandbox restarts
  A  allow always   — allow it and save it to the sandbox's network policy
  1  allow all 1h   — open egress to everything for an hour (escape hatch)
  d  deny           — refuse this attempt (also the default on timeout)
  D  deny domain    — block this host's whole domain + subdomains, for good

Holds time out to a deny after 30s. The macOS menubar app offers the same
control with native alerts; this command is the cross-platform equivalent.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		targets, err := watchTargets(args)
		if err != nil {
			return err
		}

		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer stop()

		err = runWatch(ctx, targets, cmd.OutOrStdout(), cmd.InOrStdin())
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	},
}

// watchTargets resolves the command's argument into watch targets: the named
// sandbox, or every running sandbox when no name is given.
func watchTargets(args []string) ([]watchTarget, error) {
	if len(args) == 1 {
		sb, err := store.Load(args[0])
		if err != nil {
			return nil, err
		}
		return []watchTarget{newWatchTarget(sb.Name)}, nil
	}

	all, err := store.List()
	if err != nil {
		return nil, fmt.Errorf("listing sandboxes: %w", err)
	}
	var targets []watchTarget
	for _, sb := range all {
		if sb.VMState == config.VMStateRunning {
			targets = append(targets, newWatchTarget(sb.Name))
		}
	}
	if len(targets) == 0 {
		return nil, errors.New("no running sandboxes — start one with 'clawk up', or name one explicitly")
	}
	return targets, nil
}

func newWatchTarget(name string) watchTarget {
	return watchTarget{name: name, client: vzdctl.NewClient(vzdctl.SocketPath(store.VMDir(name)))}
}

// runWatch subscribes to every target's gate stream, merges them, and prompts
// on stdin for each held connection, routing the decision back to the right
// sandbox. It returns when the context is cancelled or all streams end.
func runWatch(ctx context.Context, targets []watchTarget, out io.Writer, in io.Reader) error {
	clients := make(map[string]decider, len(targets))
	merged := make(chan taggedEvent)
	var wg sync.WaitGroup

	for _, t := range targets {
		events, err := t.client.Events(ctx)
		if err != nil {
			fmt.Fprintf(out, "⚠ %s: %v\n", t.name, err)
			continue
		}
		clients[t.name] = t.client
		if held, err := t.client.Pending(ctx); err == nil {
			for _, p := range held {
				fmt.Fprintf(out, "⏳ %s already held: %s\n", t.name, describePending(p))
			}
		}
		wg.Add(1)
		go func(name string, ch <-chan netfilter.Event) {
			defer wg.Done()
			for ev := range ch {
				select {
				case merged <- taggedEvent{vm: name, ev: ev}:
				case <-ctx.Done():
					return
				}
			}
		}(t.name, events)
	}
	if len(clients) == 0 {
		return errors.New("no sandboxes to watch (none reachable — is the daemon current?)")
	}
	go func() { wg.Wait(); close(merged) }()

	fmt.Fprintf(out, "Watching %s (a=once, s=session, A=always, 1=all-1h, d=deny, D=deny-domain). Ctrl-C to stop.\n",
		countLabel(len(clients), "sandbox", "sandboxes"))

	scanner := bufio.NewScanner(in)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case te, ok := <-merged:
			if !ok {
				return errors.New("all gate streams closed")
			}
			if te.ev.Type != netfilter.EventPending || te.ev.Pending == nil {
				continue // resolved events are informational; nothing to prompt
			}
			c := clients[te.vm]
			action, scope := promptDecision(out, scanner, te.vm, *te.ev.Pending)
			if action == "allow-all" {
				if err := c.AllowAll(ctx, allowAllWindow); err != nil {
					fmt.Fprintf(out, "  allow-all failed: %v\n", err)
				} else {
					fmt.Fprintf(out, "  → allowing ALL traffic on %s for %s\n", te.vm, allowAllWindow)
				}
				continue
			}
			switch err := c.Decide(ctx, te.ev.ID, action, scope); {
			case err == nil:
				fmt.Fprintf(out, "  → %s (%s)\n", action, scope)
			case errors.Is(err, netfilter.ErrUnknownDecision):
				fmt.Fprintln(out, "  (already resolved — it likely timed out)")
			default:
				fmt.Fprintf(out, "  decide failed: %v\n", err)
			}
		}
	}
}

// promptDecision shows one held connection (tagged with its sandbox) and reads
// a single keystroke-line of intent. EOF or an unrecognized line denies.
func promptDecision(out io.Writer, scanner *bufio.Scanner, vm string, p netfilter.Pending) (action, scope string) {
	fmt.Fprintf(out, "\n⛔ %s — %s\n   [a]llow once / [s]ession / [A]lways / [1] all 1h / [d]eny / [D]eny domain? ",
		vm, describePending(p))
	if !scanner.Scan() {
		return "deny", "once"
	}
	switch strings.TrimSpace(scanner.Text()) {
	case "a":
		return "allow", "once"
	case "s":
		return "allow", "session"
	case "A", "always":
		return "allow", "always"
	case "1":
		return "allow-all", ""
	case "D", "domain":
		return "deny", "always"
	default:
		return "deny", "once"
	}
}

// describePending renders a held connection for the prompt: the DNS name with
// its IP and port when known, otherwise just the address, plus a coalesced
// connection count when more than one is waiting.
func describePending(p netfilter.Pending) string {
	var b strings.Builder
	if p.Host != "" && p.Host != p.IP {
		fmt.Fprintf(&b, "%s (%s:%s)", p.Host, p.IP, p.Port)
	} else {
		fmt.Fprintf(&b, "%s:%s", p.IP, p.Port)
	}
	if p.Waiters > 1 {
		fmt.Fprintf(&b, " ×%d connections", p.Waiters)
	}
	return b.String()
}

// countLabel formats a count with a singular/plural noun, e.g. "1 sandbox" or
// "3 sandboxes".
func countLabel(n int, singular, plural string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
}
