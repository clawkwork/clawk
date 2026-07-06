package cli

// `clawk mod` mirrors `go mod` — it manages the per-repo clawk.mod file.
// Today only `tidy` is wired; future subcommands (`why`, `verify`,
// `download`) slot in here as the skill ecosystem grows.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/clawkwork/clawk/internal/template"
	"github.com/google/renameio/v2"
	"github.com/spf13/cobra"
)

func init() {
	modCmd.AddCommand(modTidyCmd)
	modCmd.AddCommand(modMigrateCmd)
	rootCmd.AddCommand(modCmd)
}

var modCmd = &cobra.Command{
	Use:   "mod",
	Short: "Manage clawk.mod files",
	Long: `mod groups commands that operate on the per-repo clawk.mod file.

Subcommands:

  tidy     Pin every skill version in clawk.mod (format-preserving rewrite)
  migrate  Rewrite a pre-cutover flat clawk.mod (or clawk.work) to typed blocks
`,
}

var modMigrateCmd = &cobra.Command{
	Use:   "migrate [dir]",
	Short: "Rewrite a flat clawk.mod or clawk.work to the typed-block grammar",
	Long: `migrate converts a pre-cutover config file to the typed-block grammar:
the body is wrapped in 'sandbox ( ... )', a top-level 'name X' directive
moves into the block header, and a clawk.work is renamed to clawk.mod.
Comments, blank lines, and alignment survive verbatim; the result is
validated before anything is written.

With no argument it migrates the current directory's file. A directory
containing a clawk.work gets it migrated INTO clawk.mod (refused if a
clawk.mod already exists there — merge those by hand).`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := "."
		if len(args) == 1 {
			dir = args[0]
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			return err
		}
		return migrateModFile(cmd, abs)
	},
}

// migrateModFile migrates dir's config file in place: a clawk.work moves to
// clawk.mod (removing the retired file), a flat clawk.mod is rewritten.
func migrateModFile(cmd *cobra.Command, dir string) error {
	out := cmd.OutOrStdout()
	modPath := filepath.Join(dir, template.RepoFileName)
	workPath := filepath.Join(dir, template.RetiredWorkspaceFileName)

	if src, err := os.ReadFile(workPath); err == nil {
		if _, err := os.Stat(modPath); err == nil {
			return fmt.Errorf("%s: both clawk.work and clawk.mod exist — merge them into clawk.mod by hand", dir)
		}
		migrated, err := template.MigrateFlat(string(src))
		if errors.Is(err, template.ErrAlreadyTyped) {
			// A typed-grammar body in a clawk.work is just misnamed.
			migrated = string(src)
		} else if err != nil {
			return fmt.Errorf("%s: %w", workPath, err)
		}
		if err := renameio.WriteFile(modPath, []byte(migrated), 0o644); err != nil {
			return err
		}
		if err := os.Remove(workPath); err != nil {
			return fmt.Errorf("removing retired %s: %w", workPath, err)
		}
		fmt.Fprintf(out, "Migrated %s → %s (typed-block grammar).\n", workPath, modPath)
		return nil
	}

	src, err := os.ReadFile(modPath)
	if err != nil {
		return fmt.Errorf("no clawk.mod or clawk.work in %s", dir)
	}
	migrated, err := template.MigrateFlat(string(src))
	if errors.Is(err, template.ErrAlreadyTyped) {
		fmt.Fprintf(out, "%s already uses the typed-block grammar — nothing to do.\n", modPath)
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: %w", modPath, err)
	}
	if err := renameio.WriteFile(modPath, []byte(migrated), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "Migrated %s to the typed-block grammar.\n", modPath)
	return nil
}

var modTidyCmd = &cobra.Command{
	Use:   "tidy",
	Short: "Resolve unpinned skill versions and rewrite clawk.mod",
	Long: `tidy walks every entry in the clawk.mod 'skills' block and ensures
each distributed skill carries a pinned version (a tag like v1.2.3 or a
Go-style pseudo-version v0.0.0-yyyymmddhhmmss-12charSHA).

The rewrite is format-preserving: comments, blank lines, alignment, and
entry order survive verbatim. Only the version tokens change.

Local skills (~/... or ./...) are never modified — they have nothing to
resolve.

The remote resolver is not yet implemented; until it lands, distributed
skills with branch refs or 'latest' produce an actionable error pointing
at how to pin manually.`,
	Args: cobra.NoArgs,
	RunE: runModTidy,
}

func runModTidy(cmd *cobra.Command, args []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}
	path := filepath.Join(cwd, template.RepoFileName)
	src, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no %s in %s", template.RepoFileName, cwd)
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	f, err := template.ParseFileString(string(src))
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if f.Sandbox == nil {
		// A clawk.mod carrying only policy/namespace blocks has no skills
		// to pin — that's tidy, not an error.
		fmt.Printf("%s is tidy\n", template.RepoFileName)
		return nil
	}

	res, err := template.RewriteSkillVersions(string(src), f.Sandbox, template.StubRemoteResolver)
	if err != nil {
		return fmt.Errorf("rewrite skill versions: %w", err)
	}
	if len(res.Rewrites) == 0 {
		fmt.Printf("%s is tidy\n", template.RepoFileName)
		return nil
	}
	// renameio writes to a temp file in the same directory, fsyncs it, and
	// atomically renames it into place, so a tidy interrupted mid-write never
	// leaves a half-rewritten (or zero-length) clawk.mod on disk.
	if err := renameio.WriteFile(path, []byte(res.Source), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	for _, r := range res.Rewrites {
		if r.InsertedNew {
			fmt.Printf("  pinned %s @ %s\n", r.Path, r.NewVersion)
		} else {
			fmt.Printf("  pinned %s: %s -> %s\n", r.Path, r.OldVersion, r.NewVersion)
		}
	}
	fmt.Printf("rewrote %d skill version(s) in %s\n", len(res.Rewrites), path)
	return nil
}
