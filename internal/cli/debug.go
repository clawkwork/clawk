package cli

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(debugCmd)
	debugCmd.AddCommand(debugDumpCmd)
}

var debugCmd = &cobra.Command{
	Use:   "debug",
	Short: "Diagnostic helpers",
}

// debugDumpCmd collects everything we'd ask for when triaging a freeze
// into a single .tar.gz next to the CWD. The goal is: a user hits a
// freeze, runs one command, and ships us a self-contained bundle.
//
// Collected:
//
//   - Host-side VM dir: console.log (kernel output via hvc0),
//     vzd.log (daemon events + debug output), vz.pid.
//   - Host process state: `ps` snapshot of the vzd daemon process
//     tree so we can see CPU/RSS at freeze time.
//   - Guest-side (best effort — skipped silently if the agent is
//     unreachable): the per-boot journal, kernel journal,
//     /var/log/clawk-diag/*.log (the 30 s snapshotter),
//     uname/uptime/free/df. The whole point of the diag snapshotter is
//     that these survive even when the agent later wedges — we pull the
//     persisted files, not a live ps.
//
// Deliberately NOT collected: the worktree (potentially huge, user
// already has it), ssh keys, .credentials.json, any per-sandbox secrets.
var debugDumpCmd = &cobra.Command{
	ValidArgsFunction: completeSandboxNames,
	Use:               "dump <sandbox>",
	Short:             "Collect diagnostic bundle for a sandbox (freeze postmortem)",
	Args:              cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		sb, err := store.Load(name)
		if err != nil {
			return fmt.Errorf("loading sandbox: %w", err)
		}
		vmDir := store.VMDir(sb.Name)

		ts := time.Now().UTC().Format("20060102-150405")
		outPath := fmt.Sprintf("clawk-dump-%s-%s.tar.gz", name, ts)
		out, err := os.Create(outPath)
		if err != nil {
			return fmt.Errorf("creating bundle: %w", err)
		}
		defer out.Close()
		gzw := gzip.NewWriter(out)
		defer gzw.Close()
		tw := tar.NewWriter(gzw)
		defer tw.Close()

		// --- Host-side VM artifacts. Best-effort: missing files are
		// skipped silently because e.g. a sandbox that never started
		// won't have vz.pid, and that's fine.
		// Both providers' daemon logs/pidfiles are listed; only the ones for
		// the host's provider exist (vz.* on macOS, fc.* on Linux, plus
		// firecracker's own log), and missing files are skipped below.
		hostFiles := []string{
			"console.log",
			"vzd.log", "vz.pid",
			"fcd.log", "fc.pid", "fc/firecracker.log",
		}
		for _, f := range hostFiles {
			path := filepath.Join(vmDir, f)
			if err := addFile(tw, path, filepath.Join("host", f)); err != nil {
				// Non-fatal: a missing file when the VM is down
				// is expected, not an error worth aborting for.
				fmt.Fprintf(cmd.ErrOrStderr(), "skip %s: %v\n", f, err)
			}
		}

		// --- Host process snapshot. We shell out to ps because parsing
		// /proc is not portable to darwin and the tree view is what
		// humans actually want to read.
		if pid, err := readVMPID(vmDir); err == nil && pid > 0 {
			psOut, _ := exec.Command("ps", "-o",
				"pid,ppid,stat,%cpu,%mem,rss,time,command",
				"-p", strconv.Itoa(pid)).CombinedOutput()
			addBytes(tw, "host/ps-daemon.txt", psOut)
		}
		allPs, _ := exec.Command("ps", "auxww").CombinedOutput()
		addBytes(tw, "host/ps-aux.txt", allPs)

		// Guest-side state lives in the kernel serial log (console.log,
		// captured above); sandboxes are sshd-free, so there's no
		// in-guest log fetch here.

		fmt.Printf("Wrote %s\n", outPath)
		return nil
	},
}

// addFile streams a single host file into the tarball under archivePath.
// Zero-length files are preserved (a missing vz.pid distinct from a
// 0-byte one tells us the daemon never wrote it vs wrote-then-cleared).
func addFile(tw *tar.Writer, hostPath, archivePath string) error {
	fi, err := os.Stat(hostPath)
	if err != nil {
		return err
	}
	f, err := os.Open(hostPath)
	if err != nil {
		return err
	}
	defer f.Close()
	hdr := &tar.Header{
		Name:    archivePath,
		Mode:    0o644,
		Size:    fi.Size(),
		ModTime: fi.ModTime(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

// addBytes writes in-memory content to the tarball. Errors are swallowed
// so the dump command is never aborted by a single tarball write — we'd
// rather produce a partial bundle than none at all.
func addBytes(tw *tar.Writer, archivePath string, data []byte) {
	hdr := &tar.Header{
		Name:    archivePath,
		Mode:    0o644,
		Size:    int64(len(data)),
		ModTime: time.Now(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return
	}
	_, _ = tw.Write(data)
}
