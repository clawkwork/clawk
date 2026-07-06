package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/google/renameio/v2"
)

// Policy is a named, reusable block of network rules, referenced from
// sandboxes via `use <name>`. Stored at policies/<name>/policy.json.
type Policy struct {
	Name         string   `json:"name"`
	AllowDomains []string `json:"allow_domains,omitempty"`
	AllowIPs     []string `json:"allow_ips,omitempty"`
	DenyDomains  []string `json:"deny_domains,omitempty"`
	DenyIPs      []string `json:"deny_ips,omitempty"`
	// Source is an external blocklist URL (hosts/Adblock/plain formats).
	// Fetched entries live in the sibling cache file, not here.
	Source string `json:"source,omitempty"`
	// Refresh bounds cache staleness, as a Go duration string ("24h").
	Refresh string `json:"refresh,omitempty"`
}

// defaultPolicyRefresh caps cache staleness when a policy doesn't set its own
// `refresh` interval.
const defaultPolicyRefresh = 24 * time.Hour

// RefreshInterval returns the policy's refresh interval, defaulting to 24h
// when unset.
func (p *Policy) RefreshInterval() (time.Duration, error) {
	if p.Refresh == "" {
		return defaultPolicyRefresh, nil
	}
	d, err := time.ParseDuration(p.Refresh)
	if err != nil {
		return 0, fmt.Errorf("policy %q: invalid refresh %q: %w", p.Name, p.Refresh, err)
	}
	return d, nil
}

// PolicyCache holds entries fetched from Policy.Source. Regenerable;
// stored at policies/<name>/cache.json.
type PolicyCache struct {
	FetchedAt    time.Time `json:"fetched_at"`
	ETag         string    `json:"etag,omitempty"`
	DenyDomains  []string  `json:"deny_domains"`
	AllowDomains []string  `json:"allow_domains,omitempty"` // @@ exceptions
}

// ErrPolicyNotFound is returned by LoadPolicy for a name with no record on
// disk (and no builtin).
var ErrPolicyNotFound = errors.New("policy not found")

// DefaultPolicyName is reserved for the built-in dev allowlist; it can be
// loaded but never saved or deleted, and it is the chain a sandbox gets
// when no `use` list was ever written.
const DefaultPolicyName = "default"

const builtinPolicyName = DefaultPolicyName

// policyNameRe constrains policy names to safe directory names: lowercase
// alphanumerics plus dot/underscore/dash, not leading with punctuation.
var policyNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

func (s *Store) policyDir(name string) string {
	return filepath.Join(s.rootDir, "policies", name)
}

func (s *Store) policyPath(name string) string {
	return filepath.Join(s.policyDir(name), "policy.json")
}

func (s *Store) policyCachePath(name string) string {
	return filepath.Join(s.policyDir(name), "cache.json")
}

func validatePolicyName(name string) error {
	if name == builtinPolicyName {
		return fmt.Errorf("policy name %q is reserved for the builtin default", name)
	}
	if !policyNameRe.MatchString(name) {
		return fmt.Errorf("invalid policy name %q: must match [a-z0-9][a-z0-9._-]*", name)
	}
	return nil
}

// BuiltinDefaultPolicy is the "default" chain block: the dev allowlist. It is
// never stored on disk — LoadPolicy synthesizes it so a `use default` entry
// always resolves.
func BuiltinDefaultPolicy() *Policy {
	return &Policy{
		Name:         builtinPolicyName,
		AllowDomains: append([]string(nil), DefaultAllowedDomains...),
	}
}

// SavePolicy writes a policy record, validating its name first (the builtin
// "default" is reserved and cannot be overwritten).
func (s *Store) SavePolicy(p *Policy) error {
	if err := validatePolicyName(p.Name); err != nil {
		return err
	}
	if err := os.MkdirAll(s.policyDir(p.Name), 0o755); err != nil {
		return fmt.Errorf("creating policy dir: %w", err)
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return renameio.WriteFile(s.policyPath(p.Name), data, 0o644)
}

// LoadPolicy returns a policy by name. The reserved name "default" resolves
// to BuiltinDefaultPolicy; an absent record returns ErrPolicyNotFound.
func (s *Store) LoadPolicy(name string) (*Policy, error) {
	if name == builtinPolicyName {
		return BuiltinDefaultPolicy(), nil
	}
	// Same gate as Save/Delete: the name is joined into a filesystem path,
	// so an unvalidated read ("../../…") would traverse outside the store.
	if !policyNameRe.MatchString(name) {
		return nil, fmt.Errorf("policy %q: %w", name, ErrPolicyNotFound)
	}
	data, err := os.ReadFile(s.policyPath(name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("policy %q: %w", name, ErrPolicyNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("reading policy %q: %w", name, err)
	}
	var p Policy
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parsing policy %q: %w", name, err)
	}
	if p.Name == "" {
		p.Name = name
	}
	return &p, nil
}

// ListPolicies returns every policy record on disk, sorted by name. The
// builtin "default" is synthesized, not stored, so it never appears here.
func (s *Store) ListPolicies() ([]*Policy, error) {
	entries, err := os.ReadDir(filepath.Join(s.rootDir, "policies"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading policies dir: %w", err)
	}
	var out []*Policy
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p, err := s.LoadPolicy(e.Name())
		if err != nil {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// DeletePolicy removes a policy's whole directory — record and cache both.
func (s *Store) DeletePolicy(name string) error {
	if err := validatePolicyName(name); err != nil {
		return err
	}
	return os.RemoveAll(s.policyDir(name))
}

// SavePolicyCache writes the fetched-entry cache for a policy. The cache is
// regenerable, so it lives beside the record rather than inside it — refresh
// is a one-file rewrite picked up by every referencing sandbox.
func (s *Store) SavePolicyCache(name string, c *PolicyCache) error {
	if err := validatePolicyName(name); err != nil {
		return err
	}
	if err := os.MkdirAll(s.policyDir(name), 0o755); err != nil {
		return fmt.Errorf("creating policy dir: %w", err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return renameio.WriteFile(s.policyCachePath(name), data, 0o644)
}

// LoadPolicyCache returns a policy's fetched-entry cache. An absent cache
// reads as an empty one (zero FetchedAt → always stale), so "never fetched"
// needs no special casing at call sites.
func (s *Store) LoadPolicyCache(name string) (*PolicyCache, error) {
	// Path-joined like LoadPolicy — same traversal gate.
	if !policyNameRe.MatchString(name) {
		return &PolicyCache{}, nil
	}
	data, err := os.ReadFile(s.policyCachePath(name))
	if errors.Is(err, fs.ErrNotExist) {
		return &PolicyCache{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading policy cache %q: %w", name, err)
	}
	var c PolicyCache
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parsing policy cache %q: %w", name, err)
	}
	return &c, nil
}
