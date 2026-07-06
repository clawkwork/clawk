package cli

// Chain resolution: turning a sandbox record plus its referenced policies
// into the ordered netfilter chain the daemon enforces. This is the only
// place the layering order is decided; the enforcement side receives a
// fully resolved, self-contained chain (no name lookups, no store access),
// so the same netfilter library can later run in a cloud runner unchanged.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/netfilter"
	"github.com/clawkwork/clawk/internal/template"
)

// resolveChain builds the sandbox's effective chain, lowest precedence
// first: Use-referenced policies, the namespace's live network defaults,
// then the sandbox's own stored blocks (mod, then custom). Problems —
// unknown policy names, unreadable records, invalid entries — degrade to
// warnings and an absent layer rather than an error: the ACL is
// fail-closed, so a missing block can only ever block more, never less.
func resolveChain(sb *config.Sandbox) (*netfilter.Chain, []string) {
	var blocks []netfilter.Block
	var warnings []string

	ns, err := store.LoadNamespace(sb.NamespaceName())
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("namespace %q unreadable: %v", sb.NamespaceName(), err))
		ns = &config.Namespace{Name: sb.NamespaceName()}
	}

	for _, name := range effectiveUse(ns, sb) {
		p, err := store.LoadPolicy(name)
		if err != nil {
			warnings = append(warnings,
				fmt.Sprintf("policy %q not found — that layer is absent (fail-closed)", name))
			continue
		}
		cache, err := store.LoadPolicyCache(name)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("policy %q cache unreadable: %v", name, err))
			cache = &config.PolicyCache{}
		}
		// A source-backed policy that has never fetched contributes an
		// EMPTY deny set — for a blocklist that is fail-open, not
		// fail-closed, and silently so. Booting anyway is the deliberate
		// call (a flaky blocklist host must not brick sandbox start), but
		// it has to be said out loud.
		if p.Source != "" && cache.FetchedAt.IsZero() {
			warnings = append(warnings, fmt.Sprintf(
				"policy %q: blocklist source has never been fetched — this layer currently blocks NOTHING (source: %s; retry with 'clawk policy refresh %s')",
				name, p.Source, name))
		}
		blocks = append(blocks, policyBlock(p, cache))
	}

	if len(ns.AllowedDomains)+len(ns.AllowedIPs)+len(ns.DeniedDomains)+len(ns.DeniedIPs) > 0 {
		blocks = append(blocks, netfilter.Block{
			Origin:       config.BlockOriginNamespace,
			Name:         ns.Name,
			AllowDomains: ns.AllowedDomains,
			AllowIPs:     ns.AllowedIPs,
			DenyDomains:  ns.DeniedDomains,
			DenyIPs:      ns.DeniedIPs,
		})
	}

	for _, b := range sb.Network.Blocks {
		blocks = append(blocks, netfilter.Block{
			Origin:       b.Origin,
			Name:         b.Name,
			AllowDomains: b.AllowDomains,
			AllowIPs:     b.AllowIPs,
			DenyDomains:  b.DenyDomains,
			DenyIPs:      b.DenyIPs,
		})
	}

	chain, err := netfilter.NewChain(blocks)
	if err != nil {
		// A malformed entry (bad CIDR in a hand-edited record) must not
		// take the sandbox's network down with a panic-shaped failure:
		// fall back to an empty chain — everything denied — and say why.
		warnings = append(warnings, fmt.Sprintf("network policy invalid, denying all: %v", err))
		chain, _ = netfilter.NewChain(nil)
	}
	return chain, warnings
}

// effectiveUse resolves the policy references for a sandbox. A nil list
// means "never made explicit" and inherits: the sandbox inherits the
// namespace's list, and an unset namespace means the built-in default.
// A written list is complete (include "default" where wanted); when both
// scopes write one, the namespace's comes first (broader scope, lower
// precedence) and duplicates collapse to their first mention.
func effectiveUse(ns *config.Namespace, sb *config.Sandbox) []string {
	nsUse, sbUse := ns.Use, sb.Network.Use
	if nsUse == nil && sbUse == nil {
		return []string{config.DefaultPolicyName}
	}
	seen := make(map[string]bool, len(nsUse)+len(sbUse))
	var out []string
	for _, name := range append(append([]string{}, nsUse...), sbUse...) {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// composeNetworkPolicy folds the network entries of the contributing
// templates (workspace first, then each repo — broader scope lower) into a
// sandbox's stored policy: one "mod" block carrying every inline allow AND
// deny (repo-file denies used to be silently dropped on this path), plus
// the merged Use references. `deny source` URLs become auto-registered
// source policies so their blocklists share the named-policy fetch/refresh
// machinery; they slot in at the lowest-precedence end of the Use list,
// right after "default" when present.
func composeNetworkPolicy(tmpls ...*template.Template) (config.NetworkPolicy, error) {
	var n config.NetworkPolicy
	var err error
	mod := config.NetworkBlock{Origin: config.BlockOriginMod, Name: "clawk.mod"}
	var use, sources []string
	explicitUse := false
	for _, t := range tmpls {
		if t == nil {
			continue
		}
		mod.AllowDomains = append(mod.AllowDomains, t.Domains...)
		mod.AllowIPs = append(mod.AllowIPs, t.IPs...)
		mod.DenyDomains = append(mod.DenyDomains, t.DenyDomains...)
		mod.DenyIPs = append(mod.DenyIPs, t.DenyIPs...)
		if t.Use != nil {
			explicitUse = true
			use = append(use, t.Use...)
		}
		sources = append(sources, t.DenySources...)
	}
	n.Use, err = spliceSourcePolicies(use, explicitUse, sources, false)
	if err != nil {
		return n, err
	}
	if len(mod.AllowDomains)+len(mod.AllowIPs)+len(mod.DenyDomains)+len(mod.DenyIPs) > 0 {
		mod.AllowDomains = dedupStrings(mod.AllowDomains)
		mod.AllowIPs = dedupStrings(mod.AllowIPs)
		mod.DenyDomains = dedupStrings(mod.DenyDomains)
		mod.DenyIPs = dedupStrings(mod.DenyIPs)
		n.Blocks = append(n.Blocks, mod)
	}
	return n, nil
}

// spliceSourcePolicies registers one anonymous source policy per blocklist
// URL (skipped on dryRun — names are still computed for reporting) and slots
// the names into the use chain at its lowest-precedence end: right after
// "default" when the written chain leads with it, otherwise ahead of the
// whole chain. When no chain was written, the result is "default" followed
// by the sources, matching the implicit `use default`. With no sources the
// chain passes through unchanged (nil stays nil — "never made explicit").
func spliceSourcePolicies(use []string, explicit bool, sources []string, dryRun bool) ([]string, error) {
	if len(sources) == 0 {
		if !explicit {
			return nil, nil
		}
		return dedupStrings(use), nil
	}
	var srcNames []string
	for _, url := range dedupStrings(sources) {
		if dryRun {
			srcNames = append(srcNames, sourcePolicyName(url))
			continue
		}
		name, err := ensureSourcePolicy(url)
		if err != nil {
			return nil, err
		}
		srcNames = append(srcNames, name)
	}
	switch {
	case !explicit:
		use = append([]string{config.DefaultPolicyName}, srcNames...)
	case len(use) > 0 && use[0] == config.DefaultPolicyName:
		use = append(use[:1], append(srcNames, use[1:]...)...)
	default:
		use = append(srcNames, use...)
	}
	return dedupStrings(use), nil
}

// sourcePolicyName derives the stable store name for a `deny source "<url>"`
// policy: a short digest of the URL, so the same list shared by several
// sandboxes is fetched and refreshed once.
func sourcePolicyName(url string) string {
	sum := sha256.Sum256([]byte(url))
	return "src-" + hex.EncodeToString(sum[:4])
}

// ensureSourcePolicy registers (once) the anonymous source policy for a
// `deny source "<url>"` entry.
func ensureSourcePolicy(url string) (string, error) {
	name := sourcePolicyName(url)
	if _, err := store.LoadPolicy(name); err == nil {
		return name, nil
	} else if !errors.Is(err, config.ErrPolicyNotFound) {
		return "", err
	}
	if err := store.SavePolicy(&config.Policy{Name: name, Source: url}); err != nil {
		return "", fmt.Errorf("registering blocklist %s: %w", url, err)
	}
	return name, nil
}

// registerFilePolicies stores every `policy <name> ( ... )` block declared
// in the loaded clawk files. Registration is an upsert — the file is the
// source of truth and the store a projection — so re-creating a sandbox
// after editing a policy block refreshes the stored record. Reserved-name
// and validation errors surface to the user via SavePolicy.
func registerFilePolicies(defs []template.PolicyDef) error {
	for _, def := range defs {
		p, err := policyFromDef(def)
		if err != nil {
			return err
		}
		if err := store.SavePolicy(p); err != nil {
			return err
		}
	}
	return nil
}

// policyFromDef maps a parsed `policy` block onto the stored record shape.
// A policy carries at most one source — its cache is one fetch — so extra
// `source` entries are rejected rather than silently dropped.
func policyFromDef(def template.PolicyDef) (*config.Policy, error) {
	if len(def.Sources) > 1 {
		return nil, fmt.Errorf("policy %q: one source per policy (got %d)",
			def.Name, len(def.Sources))
	}
	p := &config.Policy{
		Name:         def.Name,
		AllowDomains: def.AllowDomains,
		AllowIPs:     def.AllowIPs,
		DenyDomains:  def.DenyDomains,
		DenyIPs:      def.DenyIPs,
	}
	if len(def.Sources) == 1 {
		p.Source = def.Sources[0]
	}
	if def.Refresh != 0 {
		p.Refresh = def.Refresh.String()
	}
	return p, nil
}

// refreshPolicyCache refetches a source-backed policy's blocklist when the
// cache is older than the policy's refresh interval (or force is set). A
// policy without a source is a no-op. The previous cache is only replaced
// on a successful fetch.
func refreshPolicyCache(p *config.Policy, force bool) error {
	if p.Source == "" {
		return nil
	}
	interval, err := p.RefreshInterval()
	if err != nil {
		return fmt.Errorf("policy %q: %w", p.Name, err)
	}
	cache, err := store.LoadPolicyCache(p.Name)
	if err != nil {
		return fmt.Errorf("policy %q cache: %w", p.Name, err)
	}
	if !force && time.Since(cache.FetchedAt) < interval {
		return nil
	}
	denies, allows, err := fetchBlocklistFull(p.Source)
	if err != nil {
		return fmt.Errorf("policy %q source: %w", p.Name, err)
	}
	return store.SavePolicyCache(p.Name, &config.PolicyCache{
		FetchedAt:    time.Now(),
		DenyDomains:  denies,
		AllowDomains: allows,
	})
}

// policyBlock flattens a policy record and its fetch cache into one chain
// block. The cache carries entries fetched from Policy.Source (denies plus
// any @@ exception allows); inline entries and cached entries are one
// layer — the policy is the unit of precedence, not the fetch.
func policyBlock(p *config.Policy, cache *config.PolicyCache) netfilter.Block {
	origin := "policy"
	switch {
	case p.Name == config.DefaultPolicyName:
		origin = "default"
	case p.Source != "":
		origin = "source"
	}
	return netfilter.Block{
		Origin:       origin,
		Name:         p.Name,
		AllowDomains: append(append([]string{}, p.AllowDomains...), cache.AllowDomains...),
		AllowIPs:     p.AllowIPs,
		DenyDomains:  append(append([]string{}, p.DenyDomains...), cache.DenyDomains...),
		DenyIPs:      p.DenyIPs,
	}
}
