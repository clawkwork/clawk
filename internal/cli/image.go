package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"text/tabwriter"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/guestbuild"
	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/clawkwork/clawk/machine/oci"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(imageCmd)
	imageCmd.AddCommand(imageListCmd)
	imageCmd.AddCommand(imageGCCmd)
	imageGCCmd.Flags().BoolVarP(&imageGCDryRun, "dry-run", "n", false,
		"print what would be removed without deleting anything")
	imageGCCmd.Flags().BoolVar(&imageGCLayers, "layers", false,
		"also clear the shared OCI layer cache (re-downloaded on demand)")
}

var (
	imageGCDryRun bool
	imageGCLayers bool
)

var imageCmd = &cobra.Command{
	Use:   "image",
	Short: "Inspect and prune the OCI rootfs build cache",
	Long: `Sandboxes boot an OCI image as their root filesystem (clawk.mod
'vm ( image <ref> )', the --image flag, or the built-in clawk-dev
default). The first sandbox from a given image builds a flattened ext4
rootfs and caches it under ~/.clawk/cache/oci/<digest>/; every later
sandbox from the same image is a near-zero-cost clonefile of that disk.

These commands inspect that cache and reclaim space from entries no
sandbox would rebuild today.`,
}

var imageListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "Show cached OCI rootfs disks",
	RunE: func(cmd *cobra.Command, args []string) error {
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "KIND\tHASH\tPHYSICAL\tLOGICAL\tPATH")

		// OCI rootfs disks (clawk.mod / --image sandboxes). Entries are
		// digest-keyed; `clawk image gc` removes the ones no sandbox would
		// rebuild.
		ociEntries, hadOCI := listOCICacheRows(cacheDir())
		for _, row := range ociEntries {
			fmt.Fprintln(w, row)
		}

		if err := w.Flush(); err != nil {
			return err
		}
		if hadOCI {
			fmt.Println("\nOCI entries are digest-keyed build caches; " +
				"`clawk image gc` removes the ones no sandbox would rebuild.")
		} else {
			fmt.Println("No cached OCI rootfs disks yet — they're built on the first " +
				"`clawk up` from a given image.")
		}
		return nil
	},
}

var imageGCCmd = &cobra.Command{
	Use:   "gc",
	Short: "Remove OCI rootfs disks no longer referenced by any sandbox",
	Long: `Delete OCI rootfs disks (built for clawk.mod's 'vm ( image ... )')
whose cache key no sandbox would rebuild today. Image references resolve
over the network for the keep-set, so a tag that moved since the sandbox
was created keeps the NEW digest's entry — the old disk rebuilds on
demand if ever needed.

APFS clonefile preserves shared blocks through source deletion, so all
of this is safe — sandboxes keep working even after their source disk is
removed; deleting only costs a rebuild on the next 'clawk up'.

--layers additionally clears the shared OCI layer cache (a pure
download cache; the next build re-pulls what it needs).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		home := clawkRoot()
		store := config.NewStoreAt(home)
		sandboxes, err := store.List()
		if err != nil {
			return fmt.Errorf("listing sandboxes: %w", err)
		}

		removed, reclaimed := gcOCI(cmd.OutOrStdout(), cmd.ErrOrStderr(),
			cacheDir(), sandboxes, imageGCDryRun, imageGCLayers)

		switch {
		case removed == 0:
			fmt.Println("Nothing to remove.")
		case imageGCDryRun:
			fmt.Printf("\nDry run: would remove %d item(s), reclaim %s. "+
				"Re-run without --dry-run to delete.\n",
				removed, humanSize(reclaimed))
		default:
			fmt.Printf("\nRemoved %d item(s), reclaimed %s.\n",
				removed, humanSize(reclaimed))
		}
		return nil
	},
}

// gcOCI removes OCI rootfs cache entries (and stale guest-binary builds)
// that no existing sandbox would rebuild. The keep-set is computed the
// same way a boot computes its cache key — digest resolution included —
// so it is exact for current state; deleting is always safe because
// per-VM disks are independent clones and any entry can be rebuilt.
func gcOCI(out, errOut io.Writer, cacheDir string, sandboxes []config.Sandbox, dryRun, clearLayers bool) (removed int, reclaimed int64) {
	ociDir := filepath.Join(cacheDir, "oci")
	entries, err := os.ReadDir(ociDir)
	if err != nil && !clearLayers {
		return 0, 0 // no OCI cache yet
	}

	verb := "Remove"
	if dryRun {
		verb = "Would remove"
	}

	// Keep-set: one cache key per distinct image reference in use. A
	// failed resolution (offline, deleted tarball) keeps everything for
	// that ref out of caution is impossible — we can't map ref→digest —
	// so we warn and continue; worst case the next `clawk up` rebuilds.
	ctx := context.Background()
	keep := map[string][]string{} // cache key → sandbox names
	currentBinKey := ""
	imageSandboxes := map[string][]string{}
	for _, sb := range sandboxes {
		if sb.Image != "" {
			imageSandboxes[sb.Image] = append(imageSandboxes[sb.Image], sb.Name)
		}
	}
	if len(imageSandboxes) > 0 {
		bins, err := guestbuild.Build(ctx, cacheDir, runtime.GOARCH)
		if err != nil {
			fmt.Fprintf(errOut, "warning: skipping OCI cache gc (guest binaries: %v)\n", err)
			return 0, 0
		}
		currentBinKey = filepath.Base(filepath.Dir(bins.Init))
		for ref, names := range imageSandboxes {
			sb := &config.Sandbox{Image: ref}
			key, err := oci.Key(ctx, oci.OptionsForImage(sandbox.OCIRootFS(sb, cacheDir, bins)))
			if err != nil {
				fmt.Fprintf(errOut, "warning: resolving %s (%v): %v — its cache entry may be removed and will rebuild on next up\n",
					ref, names, err)
				continue
			}
			keep[key] = append(keep[key], names...)
		}
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if e.Name() == "layers" {
			if !clearLayers {
				continue
			}
		} else if refs, ok := keep[e.Name()]; ok {
			fmt.Fprintf(out, "Keep         %s  (referenced by %v)\n", e.Name(), refs)
			continue
		}
		p := filepath.Join(ociDir, e.Name())
		size := dirDiskUsage(p)
		if !dryRun {
			if err := os.RemoveAll(p); err != nil {
				fmt.Fprintf(errOut, "warning: removing %s: %v\n", p, err)
				continue
			}
		}
		fmt.Fprintf(out, "%s  oci/%s  (%s)\n", verb, e.Name(), humanSize(size))
		removed++
		reclaimed += size
	}

	// Guest-binary builds: keep only the one current sources produce.
	binDir := filepath.Join(cacheDir, "guestbin")
	if binEntries, err := os.ReadDir(binDir); err == nil {
		for _, e := range binEntries {
			if !e.IsDir() || e.Name() == currentBinKey {
				continue
			}
			p := filepath.Join(binDir, e.Name())
			size := dirDiskUsage(p)
			if !dryRun {
				if err := os.RemoveAll(p); err != nil {
					fmt.Fprintf(errOut, "warning: removing %s: %v\n", p, err)
					continue
				}
			}
			fmt.Fprintf(out, "%s  guestbin/%s  (%s)\n", verb, e.Name(), humanSize(size))
			removed++
			reclaimed += size
		}
	}
	return removed, reclaimed
}

// listOCICacheRows renders one tabwriter row per OCI rootfs cache entry:
// kind, shortened digest key, physical/logical size of the disk, path.
// The layer cache gets a single summary row — it's blobs, not an image.
func listOCICacheRows(cacheDir string) (rows []string, any bool) {
	ociDir := filepath.Join(cacheDir, "oci")
	entries, err := os.ReadDir(ociDir)
	if err != nil {
		return nil, false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(ociDir, e.Name())
		if e.Name() == "layers" {
			rows = append(rows, fmt.Sprintf("oci-layers\t-\t%s\t-\t%s",
				humanSize(dirDiskUsage(p)), p))
			any = true
			continue
		}
		disk := filepath.Join(p, "disk.ext4")
		info, err := os.Stat(disk)
		if err != nil {
			continue
		}
		key := e.Name()
		if len(key) > 30 {
			key = key[:14] + "…" + key[len(key)-12:]
		}
		rows = append(rows, fmt.Sprintf("oci\t%s\t%s\t%s\t%s",
			key, humanSize(diskUsage(disk)), humanSize(info.Size()), disk))
		any = true
	}
	return rows, any
}

// dirDiskUsage sums the physical disk usage of every regular file under
// dir — sparse rootfs images report blocks, not their logical size.
func dirDiskUsage(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		total += diskUsage(p)
		return nil
	})
	return total
}

// ---------------- helpers ----------------

// clawkRoot returns ~/.clawk. The store knows about subdirectories
// like sandboxes/ and vms/ but the image cache lives at the root — we
// compute that here rather than exposing a new Store method.
func clawkRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".clawk"
	}
	return filepath.Join(home, ".clawk")
}

func cacheDir() string { return filepath.Join(clawkRoot(), "cache") }

// diskUsage returns the number of bytes a file actually occupies on disk —
// not the logical size. On APFS, clonefile'd raw images can be 2GB logical
// but near-zero physical. Falls back to logical size if the platform-
// specific stat fails.
func diskUsage(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return platformDiskUsage(info)
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}
