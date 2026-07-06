//go:build !darwin

package sandbox

import (
	"os"
	"path/filepath"
)

// readHostClaudeCredentials on Linux/Windows reads the plaintext
// ~/.claude/.credentials.json Claude Code writes on non-macOS hosts.
// Returns (nil, false) if the file doesn't exist (user not logged in).
func readHostClaudeCredentials() ([]byte, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, false
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
	if err != nil {
		return nil, false
	}
	return data, true
}
