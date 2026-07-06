package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/google/renameio/v2"
)

// Namespace holds per-namespace defaults merged into every sandbox created in
// it: a shared network allowlist plus files/shares/env injected into each
// sandbox (e.g. context markdown). Stored at namespaces/<name>/namespace.json,
// co-located with that namespace's sandboxes — so a namespace's whole footprint
// (config + sandboxes) is one directory. An empty namespace is fine: it's a
// pure grouping until you give it defaults.
type Namespace struct {
	Name           string   `json:"name"`
	AllowedDomains []string `json:"allowed_domains,omitempty"`
	AllowedIPs     []string `json:"allowed_ips,omitempty"`
	// DeniedDomains are blocked outright (a block overrides any allow). They
	// come from inline `deny` entries; `deny source "<url>"` blocklists are
	// registered as source policies referenced via Use instead.
	DeniedDomains []string `json:"denied_domains,omitempty"`
	// DeniedIPs are the IP/CIDR counterparts of DeniedDomains, from
	// `deny ip <addr>` entries in a namespace manifest.
	DeniedIPs []string `json:"denied_ips,omitempty"`
	// Use names the policies forming the base of every member sandbox's
	// chain, in increasing precedence. nil means unspecified (the built-in
	// "default" applies unless a sandbox writes its own list); a non-nil
	// list is complete. Together with the inline allow/deny entries above,
	// these resolve live at up/reload — they are never copied into
	// sandbox records, so a namespace edit propagates to existing members.
	Use    []string    `json:"use,omitempty"`
	Files  []HostFile  `json:"files,omitempty"`
	Shares []HostShare `json:"shares,omitempty"`
	Env    []string    `json:"env,omitempty"`
	// Instructions and Memory seed every sandbox in the namespace: extra
	// CLAUDE.md guidance and baseline auto-memory respectively. They merge
	// with a repo's clawk.mod equivalents — namespace first, as the broader
	// scope — in applyNamespaceDefaults.
	Instructions []string `json:"instructions,omitempty"`
	Memory       string   `json:"memory,omitempty"`
}

func (s *Store) namespacePath(name string) string {
	return filepath.Join(s.rootDir, "namespaces", name, "namespace.json")
}

// LoadNamespace returns a namespace's config. A namespace directory with no
// namespace.json yet (e.g. one that only holds sandboxes) returns an empty
// config, so "no record" reads as "no defaults" rather than an error.
func (s *Store) LoadNamespace(name string) (*Namespace, error) {
	data, err := os.ReadFile(s.namespacePath(name))
	if errors.Is(err, fs.ErrNotExist) {
		return &Namespace{Name: name}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading namespace %q: %w", name, err)
	}
	var ns Namespace
	if err := json.Unmarshal(data, &ns); err != nil {
		return nil, fmt.Errorf("parsing namespace %q: %w", name, err)
	}
	if ns.Name == "" {
		ns.Name = name
	}
	return &ns, nil
}

func (s *Store) SaveNamespace(ns *Namespace) error {
	p := s.namespacePath(ns.Name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("creating namespace dir: %w", err)
	}
	data, err := json.MarshalIndent(ns, "", "  ")
	if err != nil {
		return err
	}
	return renameio.WriteFile(p, data, 0o644)
}

// NamespaceConfigPath is the on-disk path of a namespace's config record (for
// `clawk namespace edit`).
func (s *Store) NamespaceConfigPath(name string) string { return s.namespacePath(name) }

// NamespaceConfigExists reports whether a namespace has a config record (vs.
// being a bare grouping that only holds sandboxes).
func (s *Store) NamespaceConfigExists(name string) bool {
	return statExists(s.namespacePath(name))
}

// ListNamespaces returns every namespace present on disk — those with a
// namespace.json and those that merely hold sandboxes.
func (s *Store) ListNamespaces() ([]Namespace, error) {
	entries, err := os.ReadDir(filepath.Join(s.rootDir, "namespaces"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading namespaces dir: %w", err)
	}
	var out []Namespace
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		ns, err := s.LoadNamespace(e.Name())
		if err != nil {
			continue
		}
		out = append(out, *ns)
	}
	return out, nil
}
