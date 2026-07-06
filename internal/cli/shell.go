package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/clawkwork/clawk/internal/vsockclient"
)

// runShellSession opens an interactive bash inside a running sandbox.
// Used by the `clawk run shell` dispatch in runverb.go; no cobra
// command of its own — `shell` is a special-cased runner name in the
// run verb, not a registry entry.
func runShellSession(sb *config.Sandbox, provider sandbox.Provider) error {
	status, statusErr := provider.Status(sb)
	if !isRunning(status) {
		detail := fmt.Sprintf("(provider reports %q", status)
		if statusErr != nil {
			detail += fmt.Sprintf(", err: %v", statusErr)
		}
		detail += ")"
		return fmt.Errorf("sandbox %q is not running %s — use 'clawk up%s' first",
			sb.DisplayName(), detail, sandboxRef(sb))
	}
	// A paused guest accepts the vsock dial but never answers — resume
	// before attaching so the shell doesn't read as hung.
	if _, err := resumeIfPaused(os.Stderr, sb); err != nil {
		return err
	}

	// Prefer the direct vsock agent path: it survives sleep/wake better
	// and uses the same transport claude already uses. Fall through to
	// provider.Shell (the provider's own vsock exec channel) on
	// no-socket / missing-agent.
	switch err := tryVSockShell(sb, provider); {
	case err == nil:
		return nil
	case isGuestExit(err):
		// The vsock session ran fine; the guest shell just exited nonzero
		// (e.g. 130 after ^C then `exit`). That is not a transport failure —
		// propagate the code and do NOT fall back to a second shell.
		return err
	case errors.Is(err, errAgentSocketMissing):
		// No agent socket — fall through to provider.Shell below.
	default:
		// Real transport failure. The pre-attach auto-resume above is
		// best-effort (its probe swallows transient errors), so a paused
		// guest can still reach this — recover once before falling back.
		if recoverPausedSandbox(sb) {
			switch err2 := tryVSockShell(sb, provider); {
			case err2 == nil:
				return nil
			case isGuestExit(err2):
				return err2
			}
		}
		// Still broken — surface it but try provider.Shell so a
		// partially-broken vsock path doesn't strand the user.
		fmt.Fprintf(os.Stderr, "clawk: vsock shell failed (%v); falling back\n", err)
	}

	if sp, ok := provider.(sandbox.ShellProvider); ok {
		return sp.Shell(sb, agentStartDir(provider, sb))
	}
	return fmt.Errorf("provider for %q cannot open a shell", sb.Name)
}

// tryVSockShell runs an interactive bash login shell inside the guest
// over the vsock agent. Same wire shape as tryVSockAgent in
// agent_session.go (which resolves the agent tool name); the shell case
// is a fixed `/bin/bash -l` so it lives here rather than threading
// another knob through that helper.
func tryVSockShell(sb *config.Sandbox, provider sandbox.Provider) error {
	sockPath := agentSocketPath(sb)
	if _, err := os.Stat(sockPath); err != nil {
		if os.IsNotExist(err) {
			return errAgentSocketMissing
		}
		return fmt.Errorf("stat agent socket: %w", err)
	}

	cfg := vsockclient.Config{
		SocketPath: sockPath,
		Cmd:        "/bin/bash",
		Args:       []string{"-l"},
		Cwd:        agentStartDir(provider, sb),
		User:       sandbox.GuestUser,
		Env:        buildVSockEnv(),
		// No ClearScreen: a login shell is line-oriented, so keep the
		// user's scrollback — the shell should read as a continuation of
		// the terminal it launched from, not a wiped screen.
	}
	code, err := vsockclient.Run(context.Background(), cfg)
	if err != nil {
		return err
	}
	if code != 0 {
		return &sandbox.ExitError{Code: code}
	}
	return nil
}
