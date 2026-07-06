package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/stretchr/testify/require"
)

func TestLoadOAuthTokenEnvWinsOverFile(t *testing.T) {
	root := t.TempDir()
	// Pre-populate the file so we'd see "file" if env precedence
	// regressed.
	require.NoError(t, os.WriteFile(OAuthTokenPath(root), []byte("from-file\n"), 0o600))
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "from-env")

	got, src := LoadOAuthToken(root)
	if got != "from-env" || src != OAuthTokenSourceEnv {
		t.Fatalf("env-var precedence broken: got (%q,%q)", got, src)
	}
}

func TestLoadOAuthTokenFallsBackToFile(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(OAuthTokenPath(root), []byte("  from-file  \n"), 0o600))
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")

	got, src := LoadOAuthToken(root)
	if got != "from-file" || src != OAuthTokenSourceFile {
		t.Fatalf("file fallback / trim broken: got (%q,%q)", got, src)
	}
}

func TestLoadOAuthTokenAbsent(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")

	got, src := LoadOAuthToken(root)
	if got != "" || src != OAuthTokenSourceNone {
		t.Fatalf("expected empty/none when unconfigured, got (%q,%q)", got, src)
	}
}

func TestSaveOAuthTokenWritesMode0600AndTrims(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, SaveOAuthToken(root, "   sk-test-123\n"))
	path := OAuthTokenPath(root)
	fi, err := os.Stat(path)
	require.NoError(t, err)
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 0600", perm)
	}
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	got := strings.TrimRight(string(data), "\n")
	if got != "sk-test-123" {
		t.Errorf("file contents = %q, want %q", got, "sk-test-123")
	}
}

func TestSaveOAuthTokenRejectsEmpty(t *testing.T) {
	root := t.TempDir()
	require.Error(t, SaveOAuthToken(root, "   "), "expected error saving empty/whitespace-only token")
	if _, err := os.Stat(OAuthTokenPath(root)); err == nil {
		t.Error("empty-save should not have created the file")
	}
}

func TestSaveOAuthTokenAtomicReplace(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	require.NoError(t, SaveOAuthToken(root, "first"))
	require.NoError(t, SaveOAuthToken(root, "second"))
	tok, src := LoadOAuthToken(root)
	if tok != "second" || src != OAuthTokenSourceFile {
		t.Errorf("after overwrite got (%q,%q), want (second, file)", tok, src)
	}
	// And no temp leftover.
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".claude-oauth-token-") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

func TestClearOAuthTokenIsIdempotent(t *testing.T) {
	root := t.TempDir()
	// Idempotent when absent.
	require.NoError(t, ClearOAuthToken(root))
	// And after a real save.
	require.NoError(t, SaveOAuthToken(root, "abc"))
	require.NoError(t, ClearOAuthToken(root))
	if _, err := os.Stat(OAuthTokenPath(root)); !os.IsNotExist(err) {
		t.Errorf("file should be gone, stat err = %v", err)
	}
}

func TestOAuthTokenEnvFileShape(t *testing.T) {
	hf := OAuthTokenEnvFile("hello-world")
	if hf.GuestPath != "/etc/profile.d/98-clawk-claude-oauth.sh" {
		t.Errorf("GuestPath = %q", hf.GuestPath)
	}
	if hf.Mode != 0o644 {
		t.Errorf("Mode = %o, want 0644", hf.Mode)
	}
	if hf.Owner != "root:root" {
		t.Errorf("Owner = %q, want root:root", hf.Owner)
	}
	content := string(hf.Content)
	if !strings.Contains(content, `export CLAUDE_CODE_OAUTH_TOKEN="hello-world"`) {
		t.Errorf("env-file content lacks expected export line:\n%s", content)
	}
}

func TestOAuthTokenEnvFileEscapesShellMetachars(t *testing.T) {
	hf := OAuthTokenEnvFile(`tok"with$weird\meta` + "`stuff`")
	content := string(hf.Content)
	// The line as written should round-trip back to the original via
	// `sh -c '. /tmp/file; printf %s "$CLAUDE_CODE_OAUTH_TOKEN"'` —
	// verify each metachar is backslash-escaped.
	for _, want := range []string{`\"`, `\$`, `\\`, "\\`"} {
		if !strings.Contains(content, want) {
			t.Errorf("missing escape %q in:\n%s", want, content)
		}
	}
}

// TestDefaultHostFilesShape guards what DefaultHostFiles is now
// responsible for after the whole-~/.claude mount: only items that
// live OUTSIDE the per-sandbox mount. settings.json, CLAUDE.md, and
// .credentials.json all moved into SeedClaudeStateDir.
func TestDefaultHostFilesShape(t *testing.T) {
	clawkRoot := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")

	for _, f := range DefaultHostFiles(clawkRoot) {
		switch f.GuestPath {
		case GuestHome + "/.claude/settings.json",
			GuestHome + "/.claude/CLAUDE.md",
			GuestHome + "/.claude/.credentials.json":
			t.Errorf("DefaultHostFiles still emits %s — must move to "+
				"SeedClaudeStateDir so it lands inside the mounted "+
				"~/.claude/ instead of being shadowed by the mount",
				f.GuestPath)
		}
	}
}

// TestDefaultHostFilesEmitsProfileDWhenTokenConfigured is the
// remaining piece of the long-lived-token contract that still lives
// in DefaultHostFiles. The ~/.claude.json marker (onboarding +
// trust pre-acceptance) is emitted separately by ClaudeJSONMarkerFile
// at sandbox prepare time, since it needs the sandbox's phase list.
func TestDefaultHostFilesEmitsProfileDWhenTokenConfigured(t *testing.T) {
	clawkRoot := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	require.NoError(t, SaveOAuthToken(clawkRoot, "sk-test"))

	var sawProfileD bool
	for _, f := range DefaultHostFiles(clawkRoot) {
		if f.GuestPath == "/etc/profile.d/98-clawk-claude-oauth.sh" {
			sawProfileD = true
		}
	}
	if !sawProfileD {
		t.Error("expected profile.d env file when token configured")
	}
}

// TestClaudeJSONMarkerFilePreTrustsPhases is the trust-dialog
// contract: every phase worktree path (as mounted inside the guest)
// and every source repo path land in ~/.claude.json with
// hasTrustDialogAccepted=true, so claude doesn't prompt
// "is this a project you trust?" on first launch.
func TestClaudeJSONMarkerFilePreTrustsPhases(t *testing.T) {
	phases := []config.Phase{
		{Repo: "/Users/me/code/mono", Worktree: "/Users/me/.clawk/worktrees/foo/mono"},
		{Repo: "/Users/me/code/infra", Worktree: "/Users/me/.clawk/worktrees/foo/infra"},
	}
	hf := ClaudeJSONMarkerFile(phases, true)

	if hf.GuestPath != GuestHome+"/.claude.json" {
		t.Errorf("GuestPath = %q, want %s/.claude.json", hf.GuestPath, GuestHome)
	}

	var doc map[string]any
	require.NoErrorf(t, json.Unmarshal(hf.Content, &doc), "marker content not valid JSON: %s", hf.Content)
	if doc["hasCompletedOnboarding"] != true {
		t.Errorf("hasCompletedOnboarding = %v, want true (hasToken=true)", doc["hasCompletedOnboarding"])
	}

	projects, _ := doc["projects"].(map[string]any)
	for _, want := range []string{
		WorkspaceRoot + "/mono",
		WorkspaceRoot + "/infra",
		"/Users/me/code/mono",
		"/Users/me/code/infra",
		WorkspaceRoot,
	} {
		entry, ok := projects[want].(map[string]any)
		if !ok {
			t.Errorf("missing trust entry for %q in projects: %#v", want, projects)
			continue
		}
		if entry["hasTrustDialogAccepted"] != true {
			t.Errorf("projects[%q].hasTrustDialogAccepted = %v, want true",
				want, entry["hasTrustDialogAccepted"])
		}
	}
}

// TestClaudeJSONMarkerFileOmitsOnboardingWithoutToken keeps the
// "onboarding marker is scoped to the token path" property — the
// keychain-credentials flow ships its own account metadata.
func TestClaudeJSONMarkerFileOmitsOnboardingWithoutToken(t *testing.T) {
	hf := ClaudeJSONMarkerFile(nil, false)
	var doc map[string]any
	require.NoErrorf(t, json.Unmarshal(hf.Content, &doc), "marker content not valid JSON")
	if _, set := doc["hasCompletedOnboarding"]; set {
		t.Error("hasCompletedOnboarding should be absent without a token " +
			"(keychain creds carry their own account metadata)")
	}
}

// TestSeedClaudeStateDirWritesSettings checks that the synthesized
// settings.json (host overlay + clawk forced overrides) lands in the
// state dir where the mount will surface it inside the guest.
func TestSeedClaudeStateDirWritesSettings(t *testing.T) {
	clawkRoot := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	state := t.TempDir()

	require.NoError(t, SeedClaudeStateDir(state, clawkRoot))
	data, err := os.ReadFile(filepath.Join(state, "claude", "settings.json"))
	require.NoError(t, err)
	// Forced overrides must survive even though the host has no
	// settings.json — falls through to defaults ∪ forced.
	for _, want := range []string{
		`"bypassPermissionsModeAccepted": true`,
		`"defaultMode": "bypassPermissions"`,
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("seeded settings missing %q in:\n%s", want, data)
		}
	}
}

// TestSeedClaudeStateDirCopiesCLAUDEMD makes sure a host CLAUDE.md
// flows into the state dir. Mirrors the old snapshot semantics, now
// happening before mount.
func TestSeedClaudeStateDirCopiesCLAUDEMD(t *testing.T) {
	clawkRoot := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	state := t.TempDir()

	require.NoError(t, os.MkdirAll(filepath.Join(home, ".claude"), 0o755))
	want := []byte("hello from host CLAUDE.md\n")
	require.NoError(t, os.WriteFile(filepath.Join(home, ".claude", "CLAUDE.md"), want, 0o644))

	require.NoError(t, SeedClaudeStateDir(state, clawkRoot))
	got, err := os.ReadFile(filepath.Join(state, "claude", "CLAUDE.md"))
	require.NoError(t, err)
	if string(got) != string(want) {
		t.Errorf("seeded CLAUDE.md = %q, want %q", got, want)
	}
}

// TestSeedClaudeStateDirSkipsCredentialsWhenTokenSet guards the
// either/or rule between the long-lived token and the keychain blob,
// now enforced inside the state-dir seeder.
func TestSeedClaudeStateDirSkipsCredentialsWhenTokenSet(t *testing.T) {
	clawkRoot := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	require.NoError(t, SaveOAuthToken(clawkRoot, "sk-test"))
	// A host credentials file the !darwin reader would otherwise pick up.
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".claude"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"),
		[]byte(`{"refresh":"x"}`), 0o600))

	state := t.TempDir()
	require.NoError(t, SeedClaudeStateDir(state, clawkRoot))
	if _, err := os.Stat(filepath.Join(state, "claude", ".credentials.json")); err == nil {
		t.Error("expected NO .credentials.json in state when token configured " +
			"(would re-introduce the refresh-token race for bare-mode)")
	}
}

// TestSeedClaudeStateDirPreservesExistingCredentials is the
// destroy/recreate invariant: claude has been refreshing the token
// in-place against the synced dir; a stale keychain snapshot must
// not overwrite the fresher copy on re-create.
func TestSeedClaudeStateDirPreservesExistingCredentials(t *testing.T) {
	clawkRoot := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")

	// Host keychain has a stale value.
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".claude"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"),
		[]byte(`{"refresh":"stale"}`), 0o600))

	// Persisted state already has the fresh value claude refreshed to.
	state := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(state, "claude"), 0o755))
	fresh := []byte(`{"refresh":"fresh"}`)
	require.NoError(t, os.WriteFile(filepath.Join(state, "claude", ".credentials.json"),
		fresh, 0o600))

	require.NoError(t, SeedClaudeStateDir(state, clawkRoot))
	got, err := os.ReadFile(filepath.Join(state, "claude", ".credentials.json"))
	require.NoError(t, err)
	if string(got) != string(fresh) {
		t.Errorf("seeder overwrote refreshed credentials: got %q, want %q", got, fresh)
	}
}
