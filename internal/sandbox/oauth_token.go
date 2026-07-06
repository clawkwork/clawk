package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/clawkwork/clawk/internal/config"
)

// oauthTokenEnvVar is Claude Code's documented long-lived-token env var.
// Names mirrored exactly so a host that already has it exported flows
// straight into every sandbox without translation.
const oauthTokenEnvVar = "CLAUDE_CODE_OAUTH_TOKEN"

// oauthTokenFilename is where `clawk auth set-token` persists the
// 1-year token under the user's clawk root. Lives at the root (not
// under sandboxes/) because it is host-scoped: one token, every
// sandbox.
const oauthTokenFilename = "claude-oauth-token"

// guestOAuthEnvFilePath is sourced by every login shell (the vsock pty
// agent, interactive bash). Prefix 98- so a user's clawk.mod
// RequiredEnv (which writes 99-clawk-env.sh) can still override on a
// per-sandbox basis.
const guestOAuthEnvFilePath = "/etc/profile.d/98-clawk-claude-oauth.sh"

// OAuthTokenPath returns the on-disk location of the persisted token.
// Public so the CLI can echo it back in `clawk auth status` and the
// docs can reference an unambiguous string.
func OAuthTokenPath(clawkRootDir string) string {
	return filepath.Join(clawkRootDir, oauthTokenFilename)
}

// OAuthTokenSource describes where a token came from. Used by
// `clawk auth status` so the user can see which copy is winning
// — the env var trumps the file on every Claude Code invocation,
// and a surprised user otherwise can't tell why edits to the file
// aren't taking effect.
type OAuthTokenSource string

const (
	OAuthTokenSourceNone OAuthTokenSource = ""
	OAuthTokenSourceEnv  OAuthTokenSource = "env"
	OAuthTokenSourceFile OAuthTokenSource = "file"
)

// LoadOAuthToken returns the long-lived Claude Code OAuth token to
// propagate into sandboxes, along with which source it came from. A
// token == "" / source == OAuthTokenSourceNone means none configured.
//
// Resolution order matches the precedence we want Claude Code itself
// to see at runtime:
//
//  1. CLAUDE_CODE_OAUTH_TOKEN in the host process env — convenient for
//     CI and one-off shells.
//  2. ~/.clawk/claude-oauth-token — what `clawk auth set-token`
//     persists. Mode 0600.
//
// Both forms produce the same effect inside the sandbox: an env var
// exported via /etc/profile.d/. Whitespace is trimmed so users who
// piped through `pbpaste` and got a trailing newline aren't surprised
// by a "token doesn't look valid" error inside the VM.
func LoadOAuthToken(clawkRootDir string) (string, OAuthTokenSource) {
	if v := strings.TrimSpace(os.Getenv(oauthTokenEnvVar)); v != "" {
		return v, OAuthTokenSourceEnv
	}
	data, err := os.ReadFile(OAuthTokenPath(clawkRootDir))
	if err != nil {
		return "", OAuthTokenSourceNone
	}
	v := strings.TrimSpace(string(data))
	if v == "" {
		return "", OAuthTokenSourceNone
	}
	return v, OAuthTokenSourceFile
}

// SaveOAuthToken persists the token at ~/.clawk/claude-oauth-token
// with mode 0600. Atomic via write-tmp-rename so a Ctrl-C mid-write
// can't leave an empty file.
func SaveOAuthToken(clawkRootDir, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("refusing to save empty token")
	}
	if err := os.MkdirAll(clawkRootDir, 0o755); err != nil {
		return fmt.Errorf("creating clawk root %s: %w", clawkRootDir, err)
	}
	dst := OAuthTokenPath(clawkRootDir)
	tmp, err := os.CreateTemp(clawkRootDir, ".claude-oauth-token-*")
	if err != nil {
		return fmt.Errorf("creating tempfile: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup of the temp on any error after this point.
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod tempfile: %w", err)
	}
	if _, err := tmp.WriteString(token + "\n"); err != nil {
		tmp.Close()
		return fmt.Errorf("writing token: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing tempfile: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("publishing %s: %w", dst, err)
	}
	return nil
}

// ClearOAuthToken removes the persisted token. Removing a missing file
// is not an error — `clawk auth clear` should be idempotent so users
// can run it after losing track of state.
func ClearOAuthToken(clawkRootDir string) error {
	path := OAuthTokenPath(clawkRootDir)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing %s: %w", path, err)
	}
	return nil
}

// guestClaudeJSONPath is the per-user state file Claude Code consults
// at startup. Two distinct flags live here, both worth pre-seeding so
// fresh sandboxes drop the user straight into the agent without an
// interactive setup dance:
//
//   - hasCompletedOnboarding: bypasses the theme + auth wizard. Only
//     useful when CLAUDE_CODE_OAUTH_TOKEN is in play; the keychain
//     path carries its own account metadata.
//     See anthropics/claude-code#8938, #4714.
//
//   - projects[<path>].hasTrustDialogAccepted: bypasses the
//     "Quick safety check: is this a project you trust?" prompt that
//     fires the first time claude opens an unfamiliar directory.
//     Each worktree path is its own key.
//     See anthropics/claude-code#9113, #29285, #36403.
//
// Claude rewrites the file on every startup (numStartups, projects,
// etc.); we only seed the flags we care about and let claude fill in
// the rest.
const guestClaudeJSONPath = GuestHome + "/.claude.json"

// ClaudeJSONMarkerFile returns the HostFile that
// pre-creates ~/.claude.json. Two concerns share the file:
//
//   - hasCompletedOnboarding (only when hasToken — the keychain
//     credentials path doesn't need it).
//   - hasTrustDialogAccepted=true for every phase worktree path the
//     sandbox mounts, the per-repo source mounts those worktrees
//     point at, and the workspace root. Without these, a fresh
//     sandbox prompts "is this a project you trust?" on first claude
//     launch in each directory — a 100% pointless gate in a VM whose
//     only contents the user just chose to mount.
//
// Always emitted: the trust block is sandbox-shape-dependent (we
// only know the paths after PrepareVM resolves phases), but it's
// always wanted.
func ClaudeJSONMarkerFile(phases []config.Phase, hasToken bool) HostFile {
	doc := map[string]any{}
	if hasToken {
		doc["hasCompletedOnboarding"] = true
	}

	projects := map[string]any{
		WorkspaceRoot: map[string]any{"hasTrustDialogAccepted": true},
	}
	for _, p := range phases {
		if p.Worktree != "" {
			guestMount := WorkspaceRoot + "/" + filepath.Base(p.Worktree)
			projects[guestMount] = map[string]any{"hasTrustDialogAccepted": true}
		}
		if p.Repo != "" {
			// Source repos are mounted at their host path inside the VM
			// (so worktree .git metadata resolves), so the trust key
			// must match.
			projects[p.Repo] = map[string]any{"hasTrustDialogAccepted": true}
		}
	}
	doc["projects"] = projects

	content, _ := json.MarshalIndent(doc, "", "  ")
	return HostFile{
		Content:   append(content, '\n'),
		GuestPath: guestClaudeJSONPath,
		Mode:      0o644,
		Owner:     GuestUser + ":" + GuestUser,
	}
}

// OAuthTokenEnvFile returns the HostFile that drops an
// /etc/profile.d/ script exporting CLAUDE_CODE_OAUTH_TOKEN into every
// login shell inside the sandbox.
//
// Mode is 0644 root-owned, deliberately readable by the agent user
// — /etc/profile.d/*.sh that aren't readable get silently skipped by
// /etc/profile, and the only non-root principal that ever runs inside
// the VM is `agent` anyway. The token is no more sensitive than the
// ~/.claude/.credentials.json blob already shipped.
//
// The value is shell-escaped (backslash-escaping backslash, dollar,
// backtick, double-quote) so tokens that happen to contain shell
// metacharacters survive the export. Claude Code tokens are
// base64-ish today, but pinning that assumption into the export
// layer would silently break the day Anthropic changes the format.
func OAuthTokenEnvFile(token string) HostFile {
	content := fmt.Sprintf(
		"# Generated by clawk — long-lived Claude Code OAuth token\n"+
			"# (avoids the OAuth refresh-token race across multiple sandboxes,\n"+
			"#  see anthropics/claude-code#24317 / #43392)\n"+
			"export %s=\"%s\"\n",
		oauthTokenEnvVar, shellEscapeDoubleQuoted(token))
	return HostFile{
		Content:   []byte(content),
		GuestPath: guestOAuthEnvFilePath,
		Mode:      0o644,
		Owner:     "root:root",
	}
}
