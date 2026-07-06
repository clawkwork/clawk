package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/clawkwork/clawk/internal/vsockclient"
	"github.com/clawkwork/clawk/internal/vzdctl"
)

// runAgentSession dispatches to the in-guest agent over the vz vsock
// agent, with the provider's exec channel as a fallback. Vsock is
// preferred because the proxied UDS survives host sleep/wake without
// leaving iTerm2 wedged in focus-reporting state.
//
// The retry loop absorbs a specific failure shape: right after Mac
// sleep, the VM transiently has a vsock listener but the agent itself
// disconnects mid-handshake. Three attempts with 1s+2s backoff catches
// it before the user feels the need to scroll up and try again.
//
// When the host-side agent proxy isn't present (only the vz daemon runs
// one; firecracker is reached over its hybrid vsock directly), the first
// probe reports no socket and we drop straight to the provider's exec
// channel — which is vsock on both providers.
func runAgentSession(sb *config.Sandbox, provider sandbox.Provider, agent Agent, extra []string) error {
	// The pty-agent inside this sandbox is the one baked in at create;
	// refuse with recreate guidance instead of letting its handshake
	// check fail mid-attach.
	if err := sandbox.CheckGuestABI(sb); err != nil {
		return err
	}
	err := runAgentSessionOnce(sb, provider, agent, extra)
	if err != nil && recoverIdleParkedSandbox(sb, provider) {
		// The daemon parked the VM (idle timeout) in the window between
		// our caller's ensureUp and the attach — a race, not a failure.
		// The VM is booted back; run the attach once more.
		err = runAgentSessionOnce(sb, provider, agent, extra)
	}
	if err != nil && recoverPausedSandbox(sb) {
		// A paused guest accepts the vsock dial but never answers, so the
		// attach fails even though nothing is wrong. The pre-attach
		// auto-resume in the callers is best-effort (its probe swallows
		// transient errors); this is the transport-level safety net, so
		// any future attach-style verb self-heals without knowing the
		// resumeIfPaused ritual.
		err = runAgentSessionOnce(sb, provider, agent, extra)
	}
	if err != nil {
		printAgentSessionFailureHint(sb, agent, err)
	}
	return err
}

// recoverPausedSandbox resumes a paused VM after a failed attach and
// reports whether a retry makes sense.
func recoverPausedSandbox(sb *config.Sandbox) bool {
	if !sandboxPaused(sb) {
		return false
	}
	fmt.Fprintf(os.Stderr, "clawk: sandbox %q is paused; resuming and retrying\n", sb.DisplayName())
	ctx, cancel := context.WithTimeout(context.Background(), lifecycleVerbTimeout)
	defer cancel()
	if err := sandboxCtl(sb).Resume(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "clawk: resume failed: %v\n", err)
		return false
	}
	return true
}

// recoverIdleParkedSandbox detects the attach-vs-idle-stop race: the
// daemon writes StopReasonIdle (DesiredState still running) before it
// begins shutting down, so a failed attach against that record means we
// dialed a VM mid-park. Waits for the old daemon to finish exiting, boots
// the sandbox back, and reports whether a retry makes sense.
func recoverIdleParkedSandbox(sb *config.Sandbox, provider sandbox.Provider) bool {
	fresh, err := store.Load(sb.Name)
	if err != nil || fresh.StopReason != config.StopReasonIdle ||
		fresh.DesiredState == config.VMStateStopped {
		return false
	}
	// Let the parking daemon finish: its graceful stop takes a few
	// seconds (up to ~10s before the provider force-kills). Booting on
	// top of a daemon that still owns the VM's sockets would fail.
	if !waitDaemonExit(provider, fresh, 15*time.Second) {
		return false
	}
	fmt.Fprintf(os.Stderr, "clawk: sandbox %q was parked (idle timeout) just as you attached; booting it back\n",
		fresh.DisplayName())
	if err := bootSandbox(provider, fresh); err != nil {
		fmt.Fprintf(os.Stderr, "clawk: reboot after idle park failed: %v\n", err)
		return false
	}
	*sb = *fresh
	return true
}

func runAgentSessionOnce(sb *config.Sandbox, provider sandbox.Provider, agent Agent, extra []string) error {
	if sb.CreatePending {
		// A previous `on create` step failed and the user hasn't reset.
		// Attaching the runner now would put it in a half-provisioned VM
		// where its dependencies (pnpm install, go mod download) never
		// completed — almost always a worse experience than seeing an
		// explicit message pointing at the recovery path.
		reason := sb.CreatePendingReason
		if reason == "" {
			reason = "an earlier 'on create' step failed"
		}
		return fmt.Errorf(
			"sandbox %q is create-pending: %s\n  retry:    clawk up%s\n  inspect:  clawk debug vshell%s\n  reset:    clawk destroy%s",
			sb.DisplayName(), reason, sandboxRef(sb), sandboxRef(sb), sandboxRef(sb))
	}
	// Publish this VM's sessions and fold in any merged elsewhere since boot,
	// so a long-lived sandbox's resume list is current each time you attach.
	// Best-effort; no-op until the sandbox has booted under session history.
	refreshSessionHistory(sb)

	var lastErr error
	for attempt := range vsockMaxAttempts {
		err := tryVSockAgent(sb, provider, agent, extra)
		if err == nil {
			return nil
		}
		if isGuestExit(err) {
			// Clean session; the agent itself exited nonzero (e.g. 130 when
			// the user hits ^C). Propagate the code — this is not a transport
			// failure, so don't retry or fall back to the exec channel.
			return err
		}
		if errors.Is(err, errAgentSocketMissing) {
			break // no vsock agent — use the provider's exec channel
		}
		lastErr = err
		if attempt+1 == vsockMaxAttempts {
			continue
		}
		// Stay silent during retries: the agent may have already
		// written partial output (TUI setup bytes from a previous
		// claude attach) to stdout before disconnecting. Adding a
		// stderr line on top corrupts whatever claude re-renders
		// on the next attempt. The retry succeeds in the common
		// case; the user shouldn't see anything at all.
		delay := time.Duration(attempt+1) * time.Second
		time.Sleep(delay)
	}
	if lastErr != nil {
		// Only surface a message when we've actually given up on the
		// vsock agent — a real, user-visible state change, not a
		// transient retry.
		fmt.Fprintf(os.Stderr,
			"clawk: vsock agent failed after %d attempts (%v); retrying over the provider's exec channel\n",
			vsockMaxAttempts, lastErr)
	}
	return attachAgentViaExec(sb, provider, agent, extra)
}

// printAgentSessionFailureHint emits a recovery message after the agent
// attach has failed: doctor can tell a wedged guest from an image
// problem from boot-in-progress — point there first and keep the
// nuclear option as the known last resort.
//
// A paused VM produces exactly this failure shape (the proxy accepts the
// dial, the frozen guest never answers, the client sees EOF), so check for
// it first and name the one-command fix instead of the generic advice.
func printAgentSessionFailureHint(sb *config.Sandbox, agent Agent, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if ls, lsErr := sandboxCtl(sb).Lifecycle(ctx); lsErr == nil && ls.State == vzdctl.LifecyclePaused {
		fmt.Fprintf(os.Stderr,
			"clawk: agent attach failed — sandbox %q is paused (its vCPUs are frozen).\n"+
				"          continue it with:  clawk resume%s\n",
			sb.DisplayName(), sandboxRef(sb))
		return
	}
	fmt.Fprintf(os.Stderr,
		"clawk: agent attach failed.\n"+
			"          diagnose with:  clawk doctor %s\n"+
			"          if doctor shows a wedged guest (known Apple Silicon vz issue):  clawk destroy %s && clawk\n",
		sb.Name, sb.Name)
}

// vsockMaxAttempts caps how many times we retry the vsock dial before
// giving up. Empirically a 3-attempt window with 1s+2s backoff catches
// the post-sleep recovery (which usually clears by the second retry).
const vsockMaxAttempts = 3

// errAgentSocketMissing is the sentinel for "agent.sock isn't there
// yet" — distinct from real connection failures because the caller
// uses it to silently fall through to the provider's exec channel.
var errAgentSocketMissing = errors.New("agent socket missing")

// isGuestExit reports whether err is a clean guest session that ended with a
// nonzero exit code (a sandbox.ExitError), as opposed to a transport or agent
// failure. Attach paths use it to tell the two apart: a nonzero exit must
// propagate to clawk's own exit status, never trigger a retry or a fallback
// to another attach channel. A guest shell exits 130 on ^C-then-exit, so
// treating that as a transport failure spuriously opens a second shell.
func isGuestExit(err error) bool {
	var ee *sandbox.ExitError
	return errors.As(err, &ee)
}

// agentSocketPath returns the host-side Unix socket path the vz daemon's
// agent proxy bridges to the in-guest clawk-pty-agent.
func agentSocketPath(sb *config.Sandbox) string {
	return filepath.Join(store.VMDir(sb.Name), "agent.sock")
}

func tryVSockAgent(sb *config.Sandbox, provider sandbox.Provider, agent Agent, extra []string) error {
	sockPath := agentSocketPath(sb)
	if _, err := os.Stat(sockPath); err != nil {
		if os.IsNotExist(err) {
			return errAgentSocketMissing
		}
		return fmt.Errorf("stat agent socket: %w", err)
	}

	// Use the bare tool name: there is no fixed install location across
	// images, and the guest agent's PATH carries the image config's Env,
	// so LookPath lands wherever the image put the tool.
	cmd := agent.Name
	cfg := vsockclient.Config{
		SocketPath: sockPath,
		Cmd:        cmd,
		Args:       append(append([]string{}, agent.DefaultArgs...), extra...),
		Cwd:        agentStartDir(provider, sb),
		User:       sandbox.GuestUser,
		Env:        buildVSockEnv(),
		// Agents are full-screen TUIs: clear so they don't overdraw the
		// CLI's boot narration.
		ClearScreen: true,
	}

	code, err := vsockclient.Run(context.Background(), cfg)
	if err != nil {
		return err
	}
	if code != 0 {
		// Surface the child's exact exit code to our parent shell. main
		// translates this ExitError into clawk's own exit code; returning
		// it (rather than os.Exit here) lets deferred cleanup run first.
		return &sandbox.ExitError{Code: code}
	}
	return nil
}

// buildVSockEnv assembles the env entries the vsock client forwards to
// the guest agent. Keep it minimal — most of what claude needs is
// either set by the agent (PATH, HOME, USER, TERM) or comes out of the
// host shell already (LANG).
//
// TERM_PROGRAM / LC_TERMINAL are load-bearing for claude code: it
// uses them to detect that the outer terminal is iTerm2 (or WezTerm,
// Ghostty, etc.) and enable native Shift+Enter handling. Without
// these, claude's `/terminal-setup` reports "cannot be run from
// xterm-256color" and shift+enter falls back to a plain CR. The
// terminal we're indirecting through doesn't change just because
// the byte stream goes via vsock — claude needs to know the
// outermost program is iTerm2.
//
// CLAUDE_CODE_OAUTH_TOKEN is the long-lived token persisted by
// `clawk auth set-token`. The guest manifest also drops a
// /etc/profile.d/ env file, but the pty agent spawns its child
// directly (no login shell, no /etc/profile sourcing) — so for the
// vsock path the env must be threaded through the handshake here.
// Loading on every dispatch picks up edits to the token file without
// requiring a sandbox rebuild.
func buildVSockEnv() []string {
	env := []string{}
	if tok, _ := sandbox.LoadOAuthToken(clawkRoot()); tok != "" {
		env = append(env, "CLAUDE_CODE_OAUTH_TOKEN="+tok)
	}
	if v := os.Getenv("COLORTERM"); v != "" {
		env = append(env, "COLORTERM="+v)
	} else {
		env = append(env, "COLORTERM=truecolor")
	}
	// Forward terminal-identification env vars verbatim. Empty values
	// are dropped — we don't want to claim "I'm iTerm2" when the user
	// is actually running clawk from a different terminal.
	for _, k := range []string{
		"TERM_PROGRAM",         // iTerm.app, WezTerm, ghostty, kitty, ...
		"TERM_PROGRAM_VERSION", // version string companion
		"LC_TERMINAL",          // iTerm2-style "iTerm2" identifier
		"LC_TERMINAL_VERSION",  // version companion
		"ITERM_SESSION_ID",     // claude doesn't strictly need it but tools that drive iTerm2 from the guest do
		"ITERM_PROFILE",        // ditto
	} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	if v := os.Getenv("LANG"); v != "" {
		env = append(env, "LANG="+v)
	}
	if v := os.Getenv("LC_ALL"); v != "" {
		env = append(env, "LC_ALL="+v)
	}
	return env
}

// attachAgentViaExec runs the agent through the provider's exec channel
// (ShellProvider.Exec — vsock on both providers), used when the direct
// vsock agent path is unavailable. Builds a `bash -lc` line that sources
// the login profile (so the toolchain is on PATH), cd's into the agent's
// start dir, and execs the agent with its default args plus the user's
// extra args.
//
// The bash quote dance is necessary because the exec transport joins all
// trailing args with spaces on the wire — we can't rely on per-arg
// quoting reaching the remote shell. Using `"$@"` with a synthetic $0
// preserves intended word boundaries on the guest side.
func attachAgentViaExec(sb *config.Sandbox, provider sandbox.Provider, agent Agent, extra []string) error {
	sp, ok := provider.(sandbox.ShellProvider)
	if !ok {
		return fmt.Errorf("provider for %q cannot exec the agent", sb.Name)
	}
	defaults := strings.Join(quoteAll(agent.DefaultArgs), " ")
	cmdline := `: "${TERM:=xterm-256color}"; : "${COLORTERM:=truecolor}"; ` +
		"export TERM COLORTERM; " +
		"cd " + agentStartDir(provider, sb) + " 2>/dev/null || true; " +
		`exec ` + agent.Name + " " + defaults + ` "$@"`
	execArgs := append([]string{"bash", "-lc", cmdline, agent.Name}, extra...)
	return sp.Exec(sb, execArgs...)
}

// quoteAll wraps each entry in single quotes, escaping any embedded
// single quotes. Used to inline an Agent.DefaultArgs slice into the
// bash -lc command without losing word boundaries.
func quoteAll(xs []string) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = "'" + strings.ReplaceAll(x, "'", `'\''`) + "'"
	}
	return out
}
