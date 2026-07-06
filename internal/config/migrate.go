package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/google/renameio/v2"
)

// Migrations are a single ordered log of idempotent steps that bring a store
// from any older on-disk version up to currentSchemaVersion — the `go fix`
// model. The store records its version in meta.json; RunMigrations replays
// every step whose target version is newer, in order, so a v0 install and a
// v2 install both converge deterministically with no manual "first upgrade to
// X" dance.
//
// To add a breaking change to stored data: append a step, bump
// currentSchemaVersion. Keep every step idempotent — they re-run until they
// fully apply (a disruptive step defers running sandboxes and retries).

const currentSchemaVersion = 4

type migration struct {
	to   int    // schema version after this step
	name string // human-readable, shown by `clawk migrate`
	// disruptive steps move a sandbox's live on-disk directories; they skip
	// sandboxes whose VM is running (renaming a live state dir is unsafe) and
	// only complete once nothing is skipped.
	disruptive bool
	run        func(*Store) (migResult, error)
}

type migResult struct {
	changed  int      // records altered/moved
	deferred []string // sandboxes left for later (running, or a name conflict)
}

// The migration log. Steps 1–2 are the per-record field fixes that used to run
// lazily on Load; step 3 is the structural move to the namespace-first layout.
var migrations = []migration{
	{to: 1, name: "normalize retired provider names", run: (*Store).migNormalizeProviders},
	{to: 2, name: "backfill anchor and namespace fields", run: (*Store).migBackfillFields},
	{to: 3, name: "nest sandboxes under namespaces/<ns>/", disruptive: true, run: (*Store).migNestNamespaces},
	{to: 4, name: "fold flat network policy into blocks", run: (*Store).migNetworkBlocks},
}

type storeMeta struct {
	SchemaVersion int `json:"schema_version"`
}

func (s *Store) metaPath() string { return filepath.Join(s.rootDir, "meta.json") }

// SchemaVersion is the store's recorded on-disk schema version (0 if unset).
func (s *Store) SchemaVersion() int {
	v, _ := s.schemaVersion()
	return v
}

// CurrentSchemaVersion is the schema version this build targets.
func CurrentSchemaVersion() int { return currentSchemaVersion }

// schemaVersion reads the store's recorded schema version; a store with no
// meta.json (a pre-versioning install) is version 0.
func (s *Store) schemaVersion() (int, error) {
	data, err := os.ReadFile(s.metaPath())
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading store meta: %w", err)
	}
	var m storeMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return 0, fmt.Errorf("parsing store meta: %w", err)
	}
	return m.SchemaVersion, nil
}

func (s *Store) setSchemaVersion(v int) error {
	data, err := json.MarshalIndent(storeMeta{SchemaVersion: v}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.rootDir, 0o755); err != nil {
		return err
	}
	return renameio.WriteFile(s.metaPath(), data, 0o644)
}

// RunMigrations replays every pending migration step in order, advancing the
// recorded version after each. Idempotent: steps already applied are skipped,
// and a step is safe to re-run. A disruptive step that defers running
// sandboxes does not advance the version (so it retries) and, by strict
// ordering, blocks later steps until it fully applies.
func (s *Store) RunMigrations(w io.Writer) error {
	v, err := s.schemaVersion()
	if err != nil {
		return err
	}
	for _, m := range migrations {
		if m.to <= v {
			continue
		}
		res, err := m.run(s)
		if err != nil {
			return fmt.Errorf("schema migration %d (%s): %w", m.to, m.name, err)
		}
		if m.disruptive && len(res.deferred) > 0 {
			fmt.Fprintf(w, "clawk: deferred %d sandbox(es) not yet migrated (running or conflicting): %s\n"+
				"      stop them with `clawk down` and run `clawk migrate` to finish.\n",
				len(res.deferred), strings.Join(res.deferred, ", "))
			return nil // strict order: don't advance or run later steps
		}
		if res.changed > 0 {
			fmt.Fprintf(w, "clawk: applied migration %d (%s): %d record(s)\n", m.to, m.name, res.changed)
		}
		v = m.to
		if err := s.setSchemaVersion(v); err != nil {
			return fmt.Errorf("recording schema version %d: %w", v, err)
		}
	}
	return nil
}

// migNormalizeProviders folds retired provider identifiers (e.g. the pre-rename
// "vfkit") onto their current name across all records.
func (s *Store) migNormalizeProviders() (migResult, error) {
	list, err := s.List()
	if err != nil {
		return migResult{}, err
	}
	var changed int
	for i := range list {
		sb := &list[i]
		if canon := sb.Provider.Normalize(); canon != sb.Provider {
			sb.Provider = canon
			if err := s.Save(sb); err != nil {
				return migResult{changed: changed}, err
			}
			changed++
		}
	}
	return migResult{changed: changed}, nil
}

// migNetworkBlocks folds each record's flat allow/deny lists into the
// block-shaped policy (NetworkPolicy.Normalize) so every stored record is
// chain-ready. Legacy flat fields — which had the defaults baked in at
// create — land in the custom block; the record's Use stays nil, which
// resolves to the "default" policy, so a migrated sandbox keeps its exact
// effective policy (defaults may appear twice: harmless for allows).
func (s *Store) migNetworkBlocks() (migResult, error) {
	list, err := s.List()
	if err != nil {
		return migResult{}, err
	}
	var changed int
	for i := range list {
		sb := &list[i]
		if len(sb.Network.AllowedDomains)+len(sb.Network.AllowedIPs)+len(sb.Network.DeniedDomains) == 0 {
			continue
		}
		sb.Network.Normalize()
		if err := s.Save(sb); err != nil {
			return migResult{changed: changed}, err
		}
		changed++
	}
	return migResult{changed: changed}, nil
}

// migBackfillFields reconstructs the Anchor binding (from a directory-bound
// sandbox's InPlace phase) and sets an explicit namespace, so records written
// before those fields existed become self-describing.
func (s *Store) migBackfillFields() (migResult, error) {
	list, err := s.List()
	if err != nil {
		return migResult{}, err
	}
	var changed int
	for i := range list {
		sb := &list[i]
		dirty := false
		if sb.Anchor == "" {
			if a := legacyAnchor(sb); a != "" {
				sb.Anchor = a
				dirty = true
			}
		}
		if sb.Namespace == "" {
			sb.Namespace = DefaultNamespace
			dirty = true
		}
		if dirty {
			if err := s.Save(sb); err != nil {
				return migResult{changed: changed}, err
			}
			changed++
		}
	}
	return migResult{changed: changed}, nil
}

// migNestNamespaces moves each sandbox from a legacy layout (flat at the root,
// or the interim sandboxes/<ns>/ form) into the namespace-first layout,
// stripping the legacy anchored-key prefix from the name. Running sandboxes
// are deferred (moving a live VM's directories is unsafe).
func (s *Store) migNestNamespaces() (migResult, error) {
	list, err := s.List()
	if err != nil {
		return migResult{}, err
	}
	var changed int
	var deferred []string
	for i := range list {
		sb := &list[i]
		ns := sb.NamespaceName()
		cleanName := DisplayName(sb.Name)
		destRecord := filepath.Join(s.rootDir, "namespaces", ns, "sandboxes", cleanName+".json")
		if statExists(destRecord) {
			if s.recordPath(sb.Key()) == destRecord {
				continue // already nested
			}
			deferred = append(deferred, cleanName) // target taken — resolve by hand
			continue
		}
		if sb.VMState == VMStateRunning {
			deferred = append(deferred, cleanName) // can't move a live VM's directories
			continue
		}
		if err := s.nest(sb, cleanName); err != nil {
			return migResult{changed: changed, deferred: deferred}, err
		}
		changed++
	}
	return migResult{changed: changed, deferred: deferred}, nil
}

// nest relocates a single sandbox into namespaces/<ns>/ under cleanName: move
// the directory families, write the record at its new path, then remove the
// old one. Best-effort rollback isn't attempted here because the step re-runs
// idempotently — a partially-moved sandbox is finished on the next pass.
func (s *Store) nest(sb *Sandbox, cleanName string) error {
	ns := sb.NamespaceName()

	// Move the directory families that physically exist at a legacy location.
	for _, kind := range []string{"vms", "worktrees", "state"} {
		from := s.dirPath(kind, sb.Name)
		to := filepath.Join(s.rootDir, "namespaces", ns, kind, cleanName)
		if from == to || !statExists(from) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			return fmt.Errorf("preparing %s dir: %w", kind, err)
		}
		if err := os.Rename(from, to); err != nil {
			return fmt.Errorf("moving %s dir: %w", kind, err)
		}
	}

	// Managed-worktree phases store absolute paths under the worktrees dir;
	// rewrite any that point at a legacy location to the namespaced one and
	// repair git's backpointer. InPlace phases point at the user's own
	// directory and are left untouched.
	newWorktrees := filepath.Join(s.rootDir, "namespaces", ns, "worktrees", cleanName)
	oldWorktrees := []string{
		filepath.Join(s.rootDir, "worktrees", sb.Name),
		filepath.Join(s.rootDir, "worktrees", ns, sb.Name),
	}
	var repaired []string
	for i := range sb.Phases {
		p := &sb.Phases[i]
		if p.InPlace || p.Worktree == "" {
			continue
		}
		for _, old := range oldWorktrees {
			if strings.HasPrefix(p.Worktree, old) {
				p.Worktree = newWorktrees + p.Worktree[len(old):]
				repaired = append(repaired, p.Worktree)
				break
			}
		}
	}

	old := s.recordPath(sb.Key())
	sb.Name = cleanName
	data, err := json.MarshalIndent(sb, "", "  ")
	if err != nil {
		return err
	}
	dest := filepath.Join(s.rootDir, "namespaces", ns, "sandboxes", cleanName+".json")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("preparing record dir: %w", err)
	}
	if err := renameio.WriteFile(dest, data, 0o644); err != nil {
		return fmt.Errorf("writing record: %w", err)
	}
	if old != dest {
		if err := os.Remove(old); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("removing old record: %w", err)
		}
	}
	// Best-effort: a repair failure (e.g. no git, or the worktree dir is
	// absent in tests) is fine.
	for _, wt := range repaired {
		_, _ = exec.Command("git", "-C", wt, "worktree", "repair").CombinedOutput()
	}
	return nil
}

// legacyAnchor reconstructs a sandbox's Anchor from its phases for records
// written before the Anchor field existed: a directory-bound sandbox has an
// InPlace phase whose Worktree is the bound directory.
func legacyAnchor(sb *Sandbox) string {
	for i := range sb.Phases {
		if sb.Phases[i].InPlace && sb.Phases[i].Worktree != "" {
			return sb.Phases[i].Worktree
		}
	}
	return ""
}
