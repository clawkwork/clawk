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

// TestSeedClaudeMemorySeedsWhenAbsent is the first-boot case: a fresh state
// dir gets the baseline MEMORY.md at the path autoMemoryDirectory points at.
func TestSeedClaudeMemorySeedsWhenAbsent(t *testing.T) {
	state := t.TempDir()
	const seed = "# Memory\n\n- always run make lint before pushing"

	require.NoError(t, SeedClaudeMemory(state, seed))

	got, err := os.ReadFile(filepath.Join(state, "claude", "memory", "MEMORY.md"))
	require.NoError(t, err)
	if !strings.Contains(string(got), "make lint") {
		t.Errorf("seeded MEMORY.md missing seed content:\n%s", got)
	}
	if !strings.HasSuffix(string(got), "\n") {
		t.Errorf("seeded MEMORY.md should end in a newline, got %q", got)
	}
}

// TestSeedClaudeMemoryNoClobber is the core invariant: once memory exists
// (the agent wrote it, or a prior session merged it in) the seed never wins.
func TestSeedClaudeMemoryNoClobber(t *testing.T) {
	state := t.TempDir()
	memDir := filepath.Join(state, "claude", "memory")
	require.NoError(t, os.MkdirAll(memDir, 0o755))
	accumulated := []byte("# Memory\n\n- learned: the API needs a local redis\n")
	require.NoError(t, os.WriteFile(filepath.Join(memDir, "MEMORY.md"), accumulated, 0o644))

	require.NoError(t, SeedClaudeMemory(state, "# Memory\n\n- baseline seed"))

	got, err := os.ReadFile(filepath.Join(memDir, "MEMORY.md"))
	require.NoError(t, err)
	if string(got) != string(accumulated) {
		t.Errorf("seed clobbered accumulated memory:\ngot:  %q\nwant: %q", got, accumulated)
	}
}

// TestSeedClaudeMemoryEmptyIsNoOp: an empty (or whitespace-only) seed writes
// nothing, so a sandbox without a configured baseline keeps a bare memory dir.
func TestSeedClaudeMemoryEmptyIsNoOp(t *testing.T) {
	state := t.TempDir()
	for _, seed := range []string{"", "   \n\t"} {
		require.NoError(t, SeedClaudeMemory(state, seed))
	}
	if _, err := os.Stat(filepath.Join(state, "claude", "memory", "MEMORY.md")); err == nil {
		t.Error("empty seed should not create MEMORY.md")
	}
}

// TestSeedClaudeStateDirSetsAutoMemoryDirectory pins the setting that relocates
// auto-memory to the tracked, seedable /memory dir.
func TestSeedClaudeStateDirSetsAutoMemoryDirectory(t *testing.T) {
	clawkRoot := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	state := t.TempDir()

	require.NoError(t, SeedClaudeStateDir(state, clawkRoot))

	data, err := os.ReadFile(filepath.Join(state, "claude", "settings.json"))
	require.NoError(t, err)
	want := `"` + GuestHome + `/.claude/memory"`
	if !strings.Contains(string(data), want) {
		t.Errorf("settings.json missing autoMemoryDirectory %s in:\n%s", want, data)
	}
}

// TestSeedClaudeStateDirEnablesVoice pins voice dictation on by default in the
// seeded settings (tap mode — hold relies on terminal key-repeat the vsock pty
// doesn't deliver).
func TestSeedClaudeStateDirEnablesVoice(t *testing.T) {
	clawkRoot := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	state := t.TempDir()

	require.NoError(t, SeedClaudeStateDir(state, clawkRoot))

	data, err := os.ReadFile(filepath.Join(state, "claude", "settings.json"))
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(data, &got))
	voice, ok := got["voice"].(map[string]any)
	require.True(t, ok, "settings.json missing voice object:\n%s", data)
	require.Equal(t, true, voice["enabled"])
	require.Equal(t, "tap", voice["mode"])
}

// TestWorkspaceDocFileRendersInstructions checks that namespace/clawk.mod
// instructions reach the agent's auto-loaded workspace CLAUDE.md, and that the
// section is omitted entirely when there are none.
func TestWorkspaceDocFileRendersInstructions(t *testing.T) {
	with := WorkspaceDocFile(&config.Sandbox{
		Name:         "demo",
		Instructions: []string{"Ask before running destructive commands.", "Prefer pnpm over npm."},
	})
	body := string(with.Content)
	for _, want := range []string{"## Project instructions", "Ask before running destructive commands.", "Prefer pnpm over npm."} {
		if !strings.Contains(body, want) {
			t.Errorf("workspace doc missing %q in:\n%s", want, body)
		}
	}

	without := WorkspaceDocFile(&config.Sandbox{Name: "demo"})
	if strings.Contains(string(without.Content), "## Project instructions") {
		t.Errorf("workspace doc should omit instructions section when none set:\n%s", without.Content)
	}
}
