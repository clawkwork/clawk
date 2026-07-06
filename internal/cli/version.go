package cli

// `clawk version` — the string a bug report starts with. There is no
// ldflags stamping to keep `go install` builds honest: everything is read
// from the Go toolchain's own build info, which carries the module version
// for tagged installs and the vcs stamp for checkout builds.

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"
)

func init() {
	// Setting Version gives cobra's --version flag; the explicit verb exists
	// because "clawk version" is what people actually type.
	rootCmd.Version = buildVersion()
	rootCmd.SetVersionTemplate("clawk {{.Version}}\n")
	rootCmd.AddCommand(versionCmd)
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the clawk version (include this in bug reports)",
	Run: func(cmd *cobra.Command, _ []string) {
		fmt.Fprintf(cmd.OutOrStdout(), "clawk %s\n", buildVersion())
	},
}

// buildVersion assembles a human-pasteable version line from debug.BuildInfo:
// the module version when installed from a tag (v0.1.0), else the vcs
// revision and commit date of the checkout, with a +dirty marker for
// uncommitted changes. Falls back to "devel" when built without vcs (e.g.
// from a tarball) rather than failing — a bug report with "devel linux/arm64"
// still beats no version at all.
func buildVersion() string {
	version, revision, when, modified := "devel", "", "", false
	if bi, ok := debug.ReadBuildInfo(); ok {
		if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			version = bi.Main.Version
		}
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				revision = s.Value
			case "vcs.time":
				when = s.Value
			case "vcs.modified":
				modified = s.Value == "true"
			}
		}
	}
	short := revision
	if len(short) > 12 {
		short = short[:12]
	}
	out := version
	// The toolchain already suffixes +dirty onto pseudo-versions of modified
	// checkouts; only add it where it's missing (tagged builds, plain devel).
	if modified && !strings.HasSuffix(out, "+dirty") {
		out += "+dirty"
	}
	// A pseudo-version (v0.0.0-<date>-<rev>) already embeds the revision;
	// repeating it in the paren block is noise. Only tagged builds get the
	// "(rev, date)" suffix that ties the tag back to a commit.
	if short != "" && !strings.Contains(version, short) {
		out += " (" + short
		if when != "" {
			out += ", " + when
		}
		out += ")"
	}
	return out + " " + runtime.GOOS + "/" + runtime.GOARCH
}
