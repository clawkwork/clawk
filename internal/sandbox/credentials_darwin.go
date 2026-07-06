//go:build darwin

package sandbox

import (
	"os"
	"os/exec"
	"os/user"
	"strings"
)

// readHostClaudeCredentials extracts the Claude Code OAuth blob from the
// macOS Keychain. Confirmed service + account format (per decompiled
// Claude Code 2.1.x binary and anthropics/claude-code#1311):
//
//	service = "Claude Code-credentials"
//	account = $USER (Unix username, NOT email)
//
// If CLAUDE_CONFIG_DIR is set on the host, the service gets an 8-hex
// sha256 suffix — we don't handle that variant yet; those users
// authenticate inside the sandbox.
//
// `security` will prompt the user for Keychain access on first call;
// they can click "Always Allow" to make future sandbox creates silent.
func readHostClaudeCredentials() ([]byte, bool) {
	if os.Getenv("CLAUDE_CONFIG_DIR") != "" {
		return nil, false
	}
	acct := os.Getenv("USER")
	if acct == "" {
		u, err := user.Current()
		if err != nil {
			return nil, false
		}
		acct = u.Username
	}
	cmd := exec.Command("security", "find-generic-password",
		"-s", "Claude Code-credentials",
		"-a", acct,
		"-w")
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	out = []byte(strings.TrimRight(string(out), "\n"))
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}
