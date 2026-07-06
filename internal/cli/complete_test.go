package cli

import (
	"strings"
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/spf13/cobra"
)

func TestCompleteSandboxNames(t *testing.T) {
	s, _ := setupTest(t)

	// Populate two sandboxes.
	s.Save(&config.Sandbox{Name: "alpha", VMState: config.VMStateRunning})
	s.Save(&config.Sandbox{Name: "beta", VMState: config.VMStateStopped})

	t.Run("all names returned when toComplete is empty", func(t *testing.T) {
		got, dir := completeSandboxNames(nil, nil, "")
		if dir != cobra.ShellCompDirectiveNoFileComp {
			t.Errorf("directive = %v, want ShellCompDirectiveNoFileComp", dir)
		}
		if !containsPrefix(got, "alpha") {
			t.Errorf("expected alpha in completions, got %v", got)
		}
		if !containsPrefix(got, "beta") {
			t.Errorf("expected beta in completions, got %v", got)
		}
	})

	t.Run("prefix filter narrows results", func(t *testing.T) {
		got, dir := completeSandboxNames(nil, nil, "al")
		if dir != cobra.ShellCompDirectiveNoFileComp {
			t.Errorf("directive = %v, want ShellCompDirectiveNoFileComp", dir)
		}
		if !containsPrefix(got, "alpha") {
			t.Errorf("expected alpha in completions, got %v", got)
		}
		if containsPrefix(got, "beta") {
			t.Errorf("beta should not appear with prefix 'al', got %v", got)
		}
	})

	t.Run("no match returns empty list", func(t *testing.T) {
		got, dir := completeSandboxNames(nil, nil, "z")
		if dir != cobra.ShellCompDirectiveNoFileComp {
			t.Errorf("directive = %v, want ShellCompDirectiveNoFileComp", dir)
		}
		if len(got) != 0 {
			t.Errorf("expected no completions, got %v", got)
		}
	})

	t.Run("subsequence fuzzy match finds non-prefix names", func(t *testing.T) {
		// "lh" is not a prefix or substring of alpha, but it is a
		// subsequence (aLpHa) — fuzzy-capable shells get it offered.
		got, _ := completeSandboxNames(nil, nil, "lh")
		if !containsPrefix(got, "alpha") {
			t.Errorf("expected alpha for subsequence 'lh', got %v", got)
		}
		if containsPrefix(got, "beta") {
			t.Errorf("beta should not match 'lh', got %v", got)
		}
	})

	t.Run("prefix matches rank ahead of fuzzy matches", func(t *testing.T) {
		// "a" is a prefix of alpha but only a substring/subsequence of
		// beta; alpha must come first because shells display candidates
		// in returned order.
		got, _ := completeSandboxNames(nil, nil, "a")
		if len(got) != 2 {
			t.Fatalf("expected both sandboxes for 'a', got %v", got)
		}
		if !strings.HasPrefix(got[0], "alpha\t") || !strings.HasPrefix(got[1], "beta\t") {
			t.Errorf("expected alpha ranked before beta, got %v", got)
		}
	})

	t.Run("matching is case-insensitive", func(t *testing.T) {
		got, _ := completeSandboxNames(nil, nil, "ALP")
		if !containsPrefix(got, "alpha") {
			t.Errorf("expected alpha for 'ALP', got %v", got)
		}
	})

	t.Run("returns no completions after first arg is already filled", func(t *testing.T) {
		got, dir := completeSandboxNames(nil, []string{"alpha"}, "")
		if dir != cobra.ShellCompDirectiveNoFileComp {
			t.Errorf("directive = %v, want ShellCompDirectiveNoFileComp", dir)
		}
		if len(got) != 0 {
			t.Errorf("expected no completions when first arg filled, got %v", got)
		}
	})

	t.Run("completion entries include VMState description", func(t *testing.T) {
		got, _ := completeSandboxNames(nil, nil, "alpha")
		if len(got) != 1 {
			t.Fatalf("expected 1 completion, got %v", got)
		}
		// cobra description form is "name\tdesc"
		if got[0] != "alpha\trunning" {
			t.Errorf("expected 'alpha\\trunning', got %q", got[0])
		}
	})
}

func TestCompleteRunArgs(t *testing.T) {
	s, _ := setupTest(t)
	s.Save(&config.Sandbox{Name: "my-sandbox", VMState: config.VMStateStopped})

	t.Run("position 0 returns runner names", func(t *testing.T) {
		got, dir := completeRunArgs(nil, nil, "")
		if dir != cobra.ShellCompDirectiveNoFileComp {
			t.Errorf("directive = %v, want ShellCompDirectiveNoFileComp", dir)
		}
		if !containsPrefix(got, "claude") {
			t.Errorf("expected claude in completions, got %v", got)
		}
		if !containsPrefix(got, "codex") {
			t.Errorf("expected codex in completions, got %v", got)
		}
		if !containsPrefix(got, "shell") {
			t.Errorf("expected shell in completions, got %v", got)
		}
	})

	t.Run("position 0 prefix filter", func(t *testing.T) {
		got, dir := completeRunArgs(nil, nil, "cl")
		if dir != cobra.ShellCompDirectiveNoFileComp {
			t.Errorf("directive = %v, want ShellCompDirectiveNoFileComp", dir)
		}
		if !containsPrefix(got, "claude") {
			t.Errorf("expected claude with prefix 'cl', got %v", got)
		}
		// "cl" is not a substring of codex, but c-l IS not a subsequence
		// either (c,o,d,e,x has no l) — codex must stay out.
		if containsPrefix(got, "codex") {
			t.Errorf("codex should not appear for 'cl', got %v", got)
		}
	})

	t.Run("descriptions do not participate in matching", func(t *testing.T) {
		// Every runner's description contains "agent"; matching against
		// descriptions would return them all. It must return nothing —
		// no runner NAME matches "agent".
		got, _ := completeRunArgs(nil, nil, "agent")
		if len(got) != 0 {
			t.Errorf("description text must not match, got %v", got)
		}
	})

	t.Run("position 1 returns sandbox names", func(t *testing.T) {
		got, dir := completeRunArgs(nil, []string{"claude"}, "")
		if dir != cobra.ShellCompDirectiveNoFileComp {
			t.Errorf("directive = %v, want ShellCompDirectiveNoFileComp", dir)
		}
		if !containsPrefix(got, "my-sandbox") {
			t.Errorf("expected my-sandbox in completions at position 1, got %v", got)
		}
	})

	t.Run("position 2+ returns nothing", func(t *testing.T) {
		got, dir := completeRunArgs(nil, []string{"claude", "my-sandbox"}, "")
		if dir != cobra.ShellCompDirectiveNoFileComp {
			t.Errorf("directive = %v, want ShellCompDirectiveNoFileComp", dir)
		}
		if len(got) != 0 {
			t.Errorf("expected no completions at position 2, got %v", got)
		}
	})
}

// containsPrefix reports whether any element of ss has the given string as a
// prefix. Completion entries may carry a tab-separated description suffix, so
// a plain strings.Contains would miss the tab; we test the name prefix only.
func containsPrefix(ss []string, name string) bool {
	for _, s := range ss {
		// entries are either "name" or "name\tdesc"
		if s == name || len(s) > len(name) && s[:len(name)] == name && s[len(name)] == '\t' {
			return true
		}
	}
	return false
}
