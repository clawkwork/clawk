package cli

import (
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// matchRank scores how well a candidate name matches the user's partial
// input: 0 for a prefix match, 1 for a substring match, 2 for a subsequence
// ("fuzzy") match, -1 for no match. Case-insensitive throughout. The rank
// exists because shells display candidates in the order we return them, so
// exact-ish matches must sort ahead of fuzzy ones.
func matchRank(name, toComplete string) int {
	if toComplete == "" {
		return 0
	}
	n, t := strings.ToLower(name), strings.ToLower(toComplete)
	switch {
	case strings.HasPrefix(n, t):
		return 0
	case strings.Contains(n, t):
		return 1
	case isSubsequence(n, t):
		return 2
	}
	return -1
}

// isSubsequence reports whether every byte of t appears in s in order —
// "prj" matches "my-project". Byte-wise is enough: sandbox names are
// restricted to ASCII by sanitiseName. Returning subsequence matches is what
// makes fuzzy-capable shells (fish, zsh with matcher-list, fzf-tab) able to
// offer them at all — cobra hands the shell exactly what we return. Shells
// that re-filter by prefix (bash's completion script, stock zsh) silently
// drop the extras, so over-returning is harmless there.
func isSubsequence(s, t string) bool {
	i := 0
	for j := 0; j < len(s) && i < len(t); j++ {
		if s[j] == t[i] {
			i++
		}
	}
	return i == len(t)
}

// rankCompletions filters and orders "name\tdescription" candidates for a
// partial input: drop non-matches, then stable-sort by match quality so
// prefix matches lead and fuzzy matches trail. Matching runs against the
// name only — descriptions would otherwise make "agent" match every runner.
func rankCompletions(entries []string, toComplete string) []string {
	type ranked struct {
		entry string
		rank  int
	}
	var out []ranked
	for _, e := range entries {
		name, _, _ := strings.Cut(e, "\t")
		if r := matchRank(name, toComplete); r >= 0 {
			out = append(out, ranked{e, r})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].rank < out[j].rank })
	names := make([]string, len(out))
	for i, r := range out {
		names[i] = r.entry
	}
	return names
}

// completeSandboxNames is the ValidArgsFunction for every command whose first
// positional argument is a sandbox name. It reads the cached store record —
// intentionally NOT calling provider.Status — because tab-complete fires on
// every keystroke and a live VM probe (which can take hundreds of
// milliseconds) would make the shell feel stuck.
func completeSandboxNames(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		// First positional already filled; no further sandbox completions here.
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	sandboxes, err := store.List()
	if err != nil {
		// Failure-silent: a locked or absent store returns no completions
		// rather than a noisy error in the middle of a shell prompt.
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	entries := make([]string, 0, len(sandboxes))
	for _, sb := range sandboxes {
		// Cobra's "name\tdescription" form adds the description after a tab
		// character; shells that support it (fish, zsh with description mode)
		// display it as a hint. The cached VMState is cheap and useful
		// ("running" vs "stopped" steers the user toward attach vs up).
		entries = append(entries, sb.Name+"\t"+string(sb.VMState))
	}
	return rankCompletions(entries, toComplete), cobra.ShellCompDirectiveNoFileComp
}

// runnerNames lists the built-in runner registry plus the special "shell"
// dispatch. Used by completeRunArgs at position 0.
func runnerNames() []string {
	names := make([]string, 0, len(agents)+1)
	names = append(names, "shell\tinteractive bash inside the sandbox")
	for _, a := range agents {
		names = append(names, a.Name+"\tcoding agent runner")
	}
	return names
}

// completeRunArgs is the position-aware ValidArgsFunction for `clawk run`:
//
//   - position 0: runner name (claude / codex / opencode / shell)
//   - position 1: sandbox name
//   - position 2+: nothing (runner passthrough args follow `--`)
func completeRunArgs(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	switch len(args) {
	case 0:
		// First positional: runner name.
		return rankCompletions(runnerNames(), toComplete), cobra.ShellCompDirectiveNoFileComp
	case 1:
		// Second positional: sandbox name.
		return completeSandboxNames(cmd, nil, toComplete)
	default:
		// Further args are runner passthrough (after --); skip completions.
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
}
