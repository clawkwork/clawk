package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/renameio/v2"
)

type Store struct {
	baseDir string
	rootDir string // ~/.clawk
}

const currentConfigDir = ".clawk"

func NewStore() (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("getting home dir: %w", err)
	}
	rootDir := filepath.Join(home, currentConfigDir)

	baseDir := filepath.Join(rootDir, "sandboxes")
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating config dir: %w", err)
	}

	s := &Store{baseDir: baseDir, rootDir: rootDir}
	// Replay schema migrations on open so every command self-heals. Notices go
	// to stderr so a deferral (e.g. a running sandbox the nesting step can't
	// move) is visible rather than a silent surprise. Best-effort: the version
	// doesn't advance on failure or deferral, so the next open retries and
	// nothing is lost.
	_ = s.RunMigrations(os.Stderr)
	return s, nil
}

// NewStoreAt creates a Store rooted at an explicit directory, skipping the
// home-dir resolution and legacy migration NewStore does. Used by `clawk
// image gc` (which targets a caller-supplied clawk root) and by tests.
func NewStoreAt(rootDir string) *Store {
	baseDir := filepath.Join(rootDir, "sandboxes")
	// Best-effort: the dir is also created lazily on first use, and a real
	// failure resurfaces as a wrapped error from Store.Save.
	_ = os.MkdirAll(baseDir, 0o755)
	return &Store{baseDir: baseDir, rootDir: rootDir}
}

// On-disk layout. Everything a namespace owns lives under one directory, so a
// namespace's whole footprint is a single deletable/backup-able folder:
//
//	namespaces/<ns>/sandboxes/<name>.json
//	namespaces/<ns>/vms/<name>/   worktrees/<name>/   state/<name>/
//
// cache/ and history/ stay at the root — shared infrastructure owned by no
// namespace. A bare key (no "/") resolves to the default namespace.
//
// TRANSITIONAL: two older layouts may linger until the nesting migration
// finishes (see migrate.go and runMigrations) — flat at the root
// (sandboxes/<name>.json, vms/<name>, …) and an interim namespace-under-family
// form (sandboxes/<ns>/<name>.json). Resolution prefers the current layout and
// falls back through both so running sandboxes keep resolving mid-migration;
// remove the fallbacks once the migration is retired.

func statExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// splitKey separates a store key into namespace and name. A bare name (no
// "/") belongs to the default namespace.
func splitKey(key string) (ns, name string) {
	if i := strings.IndexByte(key, '/'); i >= 0 {
		return key[:i], key[i+1:]
	}
	return DefaultNamespace, key
}

// recordPath returns the JSON path for a sandbox key, preferring the current
// namespaced layout and falling back to the interim and flat legacy layouts.
func (s *Store) recordPath(key string) string {
	ns, name := splitKey(key)
	current := filepath.Join(s.rootDir, "namespaces", ns, "sandboxes", name+".json")
	if statExists(current) {
		return current
	}
	if interim := filepath.Join(s.baseDir, ns, name+".json"); statExists(interim) {
		return interim
	}
	if flat := filepath.Join(s.baseDir, name+".json"); statExists(flat) {
		return flat
	}
	return current
}

// savePath returns where to write sb. An existing record is overwritten in
// place — a field-level save never relocates it (that's the nesting
// migration's job) — and a new record lands in the current namespaced layout.
func (s *Store) savePath(sb *Sandbox) string {
	if p := s.recordPath(sb.Key()); statExists(p) {
		return p
	}
	ns, name := splitKey(sb.Key())
	return filepath.Join(s.rootDir, "namespaces", ns, "sandboxes", name+".json")
}

// dirPath resolves a per-sandbox directory family (kind = "vms"/"worktrees"/
// "state") with the same current-preferred, legacy-fallback resolution.
func (s *Store) dirPath(kind, key string) string {
	ns, name := splitKey(key)
	current := filepath.Join(s.rootDir, "namespaces", ns, kind, name)
	if statExists(current) {
		return current
	}
	if interim := filepath.Join(s.rootDir, kind, ns, name); statExists(interim) {
		return interim
	}
	if flat := filepath.Join(s.rootDir, kind, name); statExists(flat) {
		return flat
	}
	return current
}

// WorktreeDir returns the worktree directory for a sandbox.
func (s *Store) WorktreeDir(sandboxName string) string {
	return s.dirPath("worktrees", sandboxName)
}

// VMDir returns the VM state directory for a sandbox.
func (s *Store) VMDir(sandboxName string) string {
	return s.dirPath("vms", sandboxName)
}

// CacheDir returns the shared cache directory.
func (s *Store) CacheDir() string {
	return filepath.Join(s.rootDir, "cache")
}

// HistoryDir returns the root holding per-project session-history repos —
// the bare git repos that version Claude Code conversations across sandboxes
// (see internal/sessions). Lives outside per-sandbox dirs so destroying one
// sandbox never touches the shared history.
//
//	<rootDir>/history/<projectID>.git
func (s *Store) HistoryDir() string {
	return filepath.Join(s.rootDir, "history")
}

// RootDir returns the top-level clawk directory (~/.clawk on a real
// host, or a temp dir for tests via NewStoreAt). Callers that need
// host-scoped artifacts living outside the per-sandbox subdirectories
// — provision.sh overrides, the long-lived OAuth token, image cache
// roots — anchor on this path.
func (s *Store) RootDir() string {
	return s.rootDir
}

// StateDir returns the per-sandbox persistent state directory — host
// storage that lives OUTSIDE the sandbox's VMDir and therefore survives
// destroy + recreate cycles. Used to mount Claude Code's conversation
// and memory directories from a stable location per sandbox name.
//
// Layout:
//
//	<rootDir>/state/<sandboxName>/
//	  claude/
//	    projects/   # mounted as /home/agent/.claude/projects/
//	    memory/     # mounted as /home/agent/.claude/memory/
//
// The directory is created lazily by callers that need it.
func (s *Store) StateDir(sandboxName string) string {
	return s.dirPath("state", sandboxName)
}

// RecordSchemaVersion is the current shape of a sandbox record's JSON,
// stamped onto Sandbox.RecordSchema at every Save. Bump only when a
// record written by this clawk would be misread by the previous one —
// additive omitempty fields don't count.
const RecordSchemaVersion = 1

func (s *Store) Save(sb *Sandbox) error {
	sb.ResourceVersion++ // monotonic "record changed" counter (optimistic-concurrency groundwork)
	sb.RecordSchema = RecordSchemaVersion
	data, err := json.MarshalIndent(sb, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling sandbox: %w", err)
	}
	p := s.savePath(sb)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("creating sandbox dir: %w", err)
	}
	if err := renameio.WriteFile(p, data, 0o644); err != nil {
		return fmt.Errorf("writing sandbox %q: %w", sb.Name, err)
	}
	return nil
}

func (s *Store) Load(name string) (*Sandbox, error) {
	data, err := os.ReadFile(s.recordPath(name))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("sandbox %q not found", name)
		}
		return nil, fmt.Errorf("reading sandbox: %w", err)
	}
	var sb Sandbox
	if err := json.Unmarshal(data, &sb); err != nil {
		return nil, fmt.Errorf("parsing sandbox: %w", err)
	}
	// Record-shape fixes (retired provider names, missing anchor/namespace) are
	// applied once by runMigrations on store open, not lazily here — see
	// migrate.go. Network.Normalize is the one exception: it is an in-memory
	// invariant (flat fields fold into the custom block, blocks sorted), not
	// a disk rewrite, and every consumer of Blocks relies on it.
	sb.Network.Normalize()
	return &sb, nil
}

func (s *Store) Delete(name string) error {
	if err := os.Remove(s.recordPath(name)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("sandbox %q not found", name)
		}
		return err
	}
	return nil
}

func (s *Store) List() ([]Sandbox, error) {
	var sandboxes []Sandbox
	seen := map[string]bool{}
	add := func(key string) {
		ns, name := splitKey(key)
		id := ns + "/" + name
		if seen[id] {
			return
		}
		if sb, err := s.Load(key); err == nil {
			seen[id] = true
			sandboxes = append(sandboxes, *sb)
		}
	}

	// Current layout: namespaces/<ns>/sandboxes/<name>.json. Scanned first so
	// it wins over any not-yet-removed legacy copy of the same sandbox.
	if nsDirs, err := os.ReadDir(filepath.Join(s.rootDir, "namespaces")); err == nil {
		for _, nd := range nsDirs {
			if !nd.IsDir() {
				continue
			}
			recs, err := os.ReadDir(filepath.Join(s.rootDir, "namespaces", nd.Name(), "sandboxes"))
			if err != nil {
				continue
			}
			for _, r := range recs {
				if r.IsDir() || !strings.HasSuffix(r.Name(), ".json") {
					continue
				}
				add(nd.Name() + "/" + strings.TrimSuffix(r.Name(), ".json"))
			}
		}
	}

	// TRANSITIONAL legacy layouts under sandboxes/: interim <ns>/<name>.json
	// and flat <name>.json. Remove with the nesting migration.
	if entries, err := os.ReadDir(s.baseDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				ns := e.Name()
				sub, err := os.ReadDir(filepath.Join(s.baseDir, ns))
				if err != nil {
					continue
				}
				for _, se := range sub {
					if se.IsDir() || !strings.HasSuffix(se.Name(), ".json") {
						continue
					}
					add(ns + "/" + strings.TrimSuffix(se.Name(), ".json"))
				}
				continue
			}
			if !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			add(strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	return sandboxes, nil
}

func (s *Store) Exists(name string) bool {
	return statExists(s.recordPath(name))
}
