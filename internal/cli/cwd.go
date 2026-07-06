package cli

// CWD-VM helpers. Used by:
//   - the bare `clawk` invocation (zero-args = create-or-resume the
//     cwd-derived sandbox + launch the default agent)
//   - every lifecycle / read command (up, down, destroy, status,
//     claude, shell, …) when the user omitted the <name> argument
//
// The naming is "cwd" rather than "here" because the explicit
// `clawk here` verb has been retired in favor of zero-args. Anchored
// sandboxes are keyed by a clean basename; the binding itself is the
// Sandbox.Anchor field.

import (
	"fmt"

	"github.com/clawkwork/clawk/internal/config"
)

// resolveSandboxName returns the explicit sandbox name when args
// supplies one, else the name derived from the current working
// directory. Centralises the "<name> arg is optional, falls back to
// CWD" rule that read/lifecycle commands share.
func resolveSandboxName(args []string) (string, error) {
	if len(args) > 0 && args[0] != "" {
		return args[0], nil
	}
	_, name, err := hereCWDAndName()
	if err != nil {
		return "", fmt.Errorf("inferring sandbox from $CWD: %w", err)
	}
	return name, nil
}

// maybeName wraps an optional name into the slice form resolveSandboxName
// expects. Centralises the "" → nil fallthrough so runner verbs don't
// repeat it.
func maybeName(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

// sandboxRef renders how to address a sandbox as a trailing argument in a
// suggested command. Anchored sandboxes are reached by running clawk from
// their directory, so the verb takes no name — this returns "" and a hint
// like "clawk up%s" collapses to "clawk up". Named sandboxes return
// " <name>" so the same template yields "clawk up <name>". Note the leading
// space lives here so the format string has none.
func sandboxRef(sb *config.Sandbox) string {
	if isAnchored(sb) {
		return ""
	}
	return " " + sb.Name
}
