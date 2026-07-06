package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/stretchr/testify/require"
)

// isGuestExit is the gate that keeps a clean guest session that exited
// nonzero (e.g. 130 from ^C-then-exit) from being mistaken for a transport
// failure — the bug that spuriously opened a second shell on `clawk run
// shell` and retried/fell back the claude attach. Only a sandbox.ExitError
// (wrapped or not) counts; every transport/agent error must not.
func TestIsGuestExit(t *testing.T) {
	require.True(t, isGuestExit(&sandbox.ExitError{Code: 130}), "bare ExitError")
	require.True(t, isGuestExit(fmt.Errorf("attach: %w", &sandbox.ExitError{Code: 1})), "wrapped ExitError")

	require.False(t, isGuestExit(nil), "nil")
	require.False(t, isGuestExit(errAgentSocketMissing), "socket-missing sentinel")
	require.False(t, isGuestExit(errors.New("agent disconnected before exit frame")), "transport failure")
}

// TestBuildVSockEnvForwardsOAuthToken guards the wiring that gets the
// long-lived Claude Code OAuth token into the in-VM `claude` process.
// The pty agent spawns its child non-login with a custom env (see
// agentembed/main.go.in: buildChildEnv) — so /etc/profile.d/ never
// gets sourced and the token must ride the handshake env or it won't
// reach claude at all.
func TestBuildVSockEnvForwardsOAuthToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	require.NoError(t, sandbox.SaveOAuthToken(filepath.Join(home, ".clawk"), "sk-test-vsock"))

	env := buildVSockEnv()
	want := "CLAUDE_CODE_OAUTH_TOKEN=sk-test-vsock"
	for _, e := range env {
		if e == want {
			return
		}
	}
	t.Errorf("missing %q in buildVSockEnv() output: %v", want, env)
}

func TestBuildVSockEnvOmitsTokenWhenAbsent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	// Sanity: no token file under HOME/.clawk.
	_, err := os.Stat(filepath.Join(home, ".clawk", "claude-oauth-token"))
	require.True(t, os.IsNotExist(err), "expected no token file, got err=%v", err)

	for _, e := range buildVSockEnv() {
		if strings.HasPrefix(e, "CLAUDE_CODE_OAUTH_TOKEN=") {
			t.Errorf("unexpected token entry when unconfigured: %q", e)
		}
	}
}
