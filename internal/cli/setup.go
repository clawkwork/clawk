package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// First-run probe. Runs before any command that actually needs the VM
// (decided by skipFirstRun in the wired PersistentPreRunE), checks
// host prerequisites, and writes a sentinel so subsequent invocations
// short-circuit in microseconds.
//
// No `clawk init` verb — claude code itself doesn't have one, and
// claiming an `init` namespace clashes with the broader git/cobra
// convention where `init` is reserved for repo creation. This is the
// implicit alternative.

// firstRunSentinel is touched after a successful first-run probe.
// Future runs that find this file (and a matching version stamp) skip
// the probe entirely.
const firstRunSentinel = ".setup-ok"

// firstRunSentinelVersion is bumped when the probe gets a new check
// that should re-run on existing installs. Lives next to the sentinel
// file as the single line of content.
const firstRunSentinelVersion = "1"

// commandsThatSkipProbe never trigger the probe — they have to work
// even on a totally bare install (no ~/.clawk, no cached image).
var commandsThatSkipProbe = map[string]bool{
	"help":     true,
	"doctor":   true,
	"version":  true,
	"list":     true,
	"image":    true, // `image which`, `image build`, etc. — image build IS the prereq
	"system":   true, // `system info` is part of the surface that probes wouldn't help
	"debug":    true,
	"__daemon": true,
	"__vzd":    true,
}

// runFirstRunProbe is wired as rootCmd.PersistentPreRunE. It returns
// nil for commands that don't need the probe and for installs that
// already passed it. On a fresh install or a stale sentinel it runs
// the interactive checks; on CLAWK_NONINTERACTIVE it fails fast
// instead of prompting.
func runFirstRunProbe(cmdName string) error {
	if commandsThatSkipProbe[cmdName] {
		return nil
	}
	root := clawkRoot()
	sentinel := filepath.Join(root, firstRunSentinel)
	if data, err := os.ReadFile(sentinel); err == nil {
		if strings.TrimSpace(string(data)) == firstRunSentinelVersion {
			return nil
		}
		// Version mismatch — fall through and re-probe.
	}

	// Interactive vs CI.
	noninteractive := os.Getenv("CLAWK_NONINTERACTIVE") != ""

	checks := buildPrereqChecks(root)
	var failures []string
	for _, c := range checks {
		if err := c.run(); err != nil {
			failures = append(failures, fmt.Sprintf("  ✗ %s — fix: %s", c.label, c.fix))
		}
	}

	if len(failures) > 0 {
		msg := "clawk first-run check found unmet prerequisites:\n" +
			strings.Join(failures, "\n")
		if noninteractive {
			return fmt.Errorf("%s", msg)
		}
		// In interactive mode, surface the failures and continue
		// anyway — many of these are warn-level. The user can run
		// `clawk doctor` for the full version. We don't auto-run
		// brew install or other privileged steps without consent.
		fmt.Fprintln(os.Stderr, msg)
		fmt.Fprintln(os.Stderr,
			"Continuing; run `clawk doctor` for details, or set CLAWK_NONINTERACTIVE=1 to make these fatal.")
	}

	// Mark setup as done. Don't fail the command if this write
	// errors (e.g. read-only home dir) — the probe is best-effort.
	if err := os.MkdirAll(root, 0o755); err == nil {
		_ = os.WriteFile(sentinel, []byte(firstRunSentinelVersion+"\n"), 0o644)
	}
	return nil
}

// prereqCheck is one line in the probe report. Each is
// non-blocking on its own; the aggregate decides whether to fail.
type prereqCheck struct {
	label string
	fix   string
	run   func() error
}

// buildPrereqChecks assembles the host-level checks that run on first
// invocation. Kept as a function (not a package-var) so unit tests can
// swap commands in / out without touching global state.
func buildPrereqChecks(root string) []prereqCheck {
	checks := []prereqCheck{
		{
			label: "clawk state directory exists",
			fix:   "(automatic) creating " + root,
			run: func() error {
				if _, err := os.Stat(root); err == nil {
					return nil
				}
				return os.MkdirAll(root, 0o755)
			},
		},
	}
	// No host-platform-specific tooling to probe: the macOS hypervisor is
	// Apple Virtualization.framework (linked in via machine/vz) and the OCI
	// rootfs is cloned with APFS clonefile, so there is no qemu-img or other
	// external binary to require. Linux uses firecracker — its only host
	// prerequisite is a KVM-capable kernel, which we don't probe here.
	return checks
}

// init wires the probe into rootCmd. PersistentPreRunE runs once per
// invocation, before the resolved command's RunE — exactly the right
// place for "do something before the command starts but after flags
// have been parsed."
func init() {
	rootCmd.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		// Walk up the command tree to the top-level child of root
		// (e.g. `clawk image build` → "image"). The skip list is
		// keyed on the top-level verb because that's the granularity
		// users think in.
		top := cmd
		for top.Parent() != nil && top.Parent() != rootCmd {
			top = top.Parent()
		}
		return runFirstRunProbe(top.Name())
	}
}
