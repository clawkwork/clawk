package cli

import (
	"fmt"
	"sort"
)

// Agent describes how a coding-agent runner is launched inside a
// sandbox. Treating each agent (claude, codex, opencode, ...) as a
// data-driven entry rather than a per-agent code path keeps the CLI
// surface symmetric: every agent's verb works the same way, and adding
// a fourth runner is a one-line registry change.
type Agent struct {
	// Name is the user-facing runner argument (`clawk run <name>`).
	// Must be unique. Lowercase ASCII, no whitespace. Also the command
	// resolved on the guest agent's PATH (which carries the image
	// config's Env), so it lands wherever the image installed the tool.
	Name string

	// DefaultArgs are prepended to whatever the user passes through
	// `-- <args>`. Claude and Codex both have explicit "externally
	// sandboxed" modes, so clawk enables those by default.
	DefaultArgs []string
}

// agents is the builtin runner registry. `clawk run <name>` consults
// this list; anything not here is rejected with the available names.
var agents = []Agent{
	{
		Name:        "claude",
		DefaultArgs: []string{"--dangerously-skip-permissions"},
	},
	{
		Name:        "codex",
		DefaultArgs: []string{"--dangerously-bypass-approvals-and-sandbox"},
	},
	{
		Name: "opencode",
	},
}

// agentByName returns the registry entry for a given runner name.
// Used by `clawk run <runner>` dispatch.
func agentByName(name string) (Agent, error) {
	for _, a := range agents {
		if a.Name == name {
			return a, nil
		}
	}
	names := make([]string, 0, len(agents))
	for _, a := range agents {
		names = append(names, a.Name)
	}
	sort.Strings(names)
	return Agent{}, fmt.Errorf("unknown agent %q (have: %v)", name, names)
}

// reservedAgentNames returns the set of names that cannot be used as
// sandbox names — they collide with runner names. Used by sandbox-name
// validation when a user runs `clawk work <branch>`.
func reservedAgentNames() []string {
	out := make([]string, 0, len(agents))
	for _, a := range agents {
		out = append(out, a.Name)
	}
	return out
}
