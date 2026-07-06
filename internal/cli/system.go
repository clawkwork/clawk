package cli

import (
	"fmt"
	"os"
	"runtime"

	"github.com/spf13/cobra"
)

// `clawk system` is the docker-shaped namespace for cross-cutting
// host operations: pruning orphans, reporting host state, showing disk
// usage. `info` and `df` surface what we already know in human-readable
// form; `prune` reaps unreferenced OCI rootfs disks.

func init() {
	rootCmd.AddCommand(systemCmd)
	systemCmd.AddCommand(systemPruneCmd, systemInfoCmd, systemDfCmd)

	// One flag per subsystem GC. Don't add a flag before its GC exists —
	// a shipped no-op flag can only ever be removed as a breaking change
	// (a --vm placeholder was cut for exactly that reason pre-release).
	systemPruneCmd.Flags().BoolVar(&systemPruneImage, "image", false,
		"only reap unreferenced OCI rootfs disks")
}

var systemPruneImage bool

var systemCmd = &cobra.Command{
	Use:   "system",
	Short: "Host-level operations: prune orphans, info, disk usage",
	Long: `system commands operate on host state shared across all sandboxes.

  clawk system prune     reap orphans across subsystems
  clawk system info      host prereqs + version + active components
  clawk system df        disk usage by sandbox / cache`,
}

var systemPruneCmd = &cobra.Command{
	Use:   "prune [--image]",
	Short: "Reap orphaned artefacts (think `docker system prune`)",
	Long: `Prune dispatches to per-subsystem GCs:

  --image     Delete OCI rootfs disks not referenced by any sandbox.

With no flag, all prunes run in sequence. Each is idempotent and safe —
nothing destructive ever happens to a live sandbox's state. (VM
artefacts — sockets, pidfiles — need no prune: sandboxes clean their
own at destroy.)`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := imageGCCmd.RunE(cmd, nil); err != nil {
			fmt.Fprintf(os.Stderr, "warning: image prune: %v\n", err)
		}
		return nil
	},
}

var systemInfoCmd = &cobra.Command{
	Use:   "info",
	Short: "Host prerequisites + version + active components",
	Long: `Reports what clawk sees about the host: OS/arch,
active provider default, configured directories.

For diagnosing a sandbox specifically, use 'clawk doctor'. info is
report-only; doctor evaluates and suggests fixes.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "Version:          %s\n", buildVersion())
		fmt.Fprintf(out, "OS / Arch:        %s/%s\n", runtime.GOOS, runtime.GOARCH)
		fmt.Fprintf(out, "clawk root:       %s\n", clawkRoot())
		fmt.Fprintf(out, "Default provider: %s\n", defaultProviderName())

		// Sandbox tally — useful one-glance signal of "is this a
		// fresh install or a heavily-used machine?"
		if names, err := store.List(); err == nil {
			running, total := 0, len(names)
			for _, sb := range names {
				if sb.VMState == "running" {
					running++
				}
			}
			fmt.Fprintf(out, "Sandboxes:        %d total, %d running\n", total, running)
		}
		return nil
	},
}

var systemDfCmd = &cobra.Command{
	Use:   "df",
	Short: "Disk usage by sandbox / cache",
	Long: `Reports the disk footprint of clawk-managed state. Sums per-sandbox
state directories and shared caches.

Today's implementation is a placeholder — surfaces a stat tally rather
than a full du-style breakdown. Useful as a first-pass "where did my
disk go?" probe.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		// Minimum viable: report the existence and size of the major
		// directories. A real du-walk is nice to have but expensive
		// on machines with many sandboxes; deferred to follow-up.
		root := clawkRoot()
		fmt.Fprintf(cmd.OutOrStdout(), "%-32s %s\n", "DIRECTORY", "SIZE")
		for _, sub := range []string{"vms", "cache", "state"} {
			path := root + "/" + sub
			size, err := dirSize(path)
			if err != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "%-32s (missing or unreadable)\n", path)
				continue
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%-32s %d MiB\n", path, size/(1024*1024))
		}
		return nil
	},
}

// dirSize returns the cumulative size (in bytes) of every regular file
// under path. Symlinks are not followed (we don't want to chase a
// symlinked image cache twice).
func dirSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return info.Size(), nil
	}
	var total int64
	walkErr := walkSizes(path, &total)
	return total, walkErr
}

func walkSizes(path string, total *int64) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, e := range entries {
		full := path + "/" + e.Name()
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if fi.IsDir() {
			_ = walkSizes(full, total)
			continue
		}
		*total += fi.Size()
	}
	return nil
}

// defaultProviderName surfaces the active default for new sandboxes.
// Honours --provider if set; else the host default (vz on macOS,
// firecracker on Linux).
func defaultProviderName() string {
	if providerFlag != "" {
		return providerFlag
	}
	return string(defaultProvider()) // matches providerFor's fallback
}
