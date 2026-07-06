package netfilter

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// Verdict is a policy chain's opinion on one destination: allow it, deny it,
// or no opinion at all (VerdictNone), in which case the caller falls through
// to the next mechanism — runtime grants, the interactive gate, or the
// default fail-closed refusal.
type Verdict int

const (
	VerdictNone Verdict = iota
	VerdictAllow
	VerdictDeny
)

// String renders a Verdict for attribution strings and test failures.
func (v Verdict) String() string {
	switch v {
	case VerdictAllow:
		return "allow"
	case VerdictDeny:
		return "deny"
	default:
		return "none"
	}
}

// Block is one origin-labeled layer of network policy: allow/deny sets for
// domain names and for literal IPs/CIDRs. A sandbox's effective policy is an
// ordered chain of blocks (defaults, external blocklists, named policies,
// inline file entries, runtime edits). Ordering lives *between* blocks —
// structural, few, stable — never between individual rules, so appending a
// rule to a block (a CLI edit, a gate-grant persistence) is never
// position-sensitive.
type Block struct {
	// Origin labels where the block came from: "default", "policy",
	// "source", "namespace", "mod", "custom", "runtime".
	Origin string
	// Name attributes the block in denial messages — a policy name, a
	// source URL. May be empty (inline and legacy blocks).
	Name string

	// AllowDomains are exact names (api.example.com, matching only
	// themselves) or left-anchored wildcards (*.example.com, matching any
	// depth of subdomains but never the apex). Trailing dots and
	// surrounding space are tolerated; matching is case-insensitive.
	AllowDomains []string
	// DenyDomains take the same forms, except a bare domain is a subtree
	// deny: it matches the apex AND every subdomain. Denying a name is a
	// guardrail — at resolution time it skips the interactive gate and
	// cannot be overridden by a runtime grant, only by a higher block.
	DenyDomains []string

	// AllowIPs and DenyIPs are literal IPs or CIDR prefixes.
	AllowIPs []string
	DenyIPs  []string
}

// Match attributes a Verdict to the rule that produced it, so a refused
// connection can be explained ("policy oisd: deny tracker.example.com")
// instead of just refused.
type Match struct {
	Origin  string
	Block   string // Block.Name
	Entry   string // the (normalized) rule text that matched
	Verdict Verdict
}

// String renders the human-readable attribution recorded with a denial,
// e.g. "policy oisd: deny tracker.example.com".
func (m Match) String() string {
	label := m.Origin
	if m.Block != "" {
		label += " " + m.Block
	}
	return fmt.Sprintf("%s: %s %s", label, m.Verdict, m.Entry)
}

// Chain is the compiled, immutable form of an ordered block list. It is safe
// for concurrent readers; a policy change builds a new Chain and swaps it in
// atomically (see AllowList.SetChain) rather than mutating one in place.
type Chain struct {
	blocks   []Block // source form, lowest precedence first
	compiled []compiledBlock
}

// nameRule is one compiled domain entry. Exactly one shape applies:
// exact (neither flag), wildcard ("*." prefix), or subtree (a bare DENY
// domain, covering the apex and every subdomain).
type nameRule struct {
	entry    string // normalized rule text, for attribution
	pattern  string // lowercased, trailing dot stripped, "*." stripped
	labels   int    // label count of pattern — the specificity depth
	wildcard bool
	subtree  bool
	deny     bool
}

// match reports whether name (already normalized) matches, and whether the
// match is exact — a whole-name equality, as opposed to a suffix match. The
// distinction breaks specificity ties: exact beats wildcard of equal depth.
func (r *nameRule) match(name string) (ok, exact bool) {
	switch {
	case r.wildcard:
		return strings.HasSuffix(name, "."+r.pattern), false
	case r.subtree:
		if name == r.pattern {
			return true, true
		}
		return strings.HasSuffix(name, "."+r.pattern), false
	default:
		eq := name == r.pattern
		return eq, eq
	}
}

// ipRule is one compiled IP entry: an exact address or a CIDR prefix.
type ipRule struct {
	entry  string
	addr   netip.Addr   // set when exact
	prefix netip.Prefix // set otherwise
	exact  bool
	deny   bool
}

func (r *ipRule) contains(ip netip.Addr) bool {
	if r.exact {
		return r.addr == ip
	}
	return r.prefix.Contains(ip)
}

type compiledBlock struct {
	origin string
	name   string
	names  []nameRule
	ips    []ipRule
}

// NewChain compiles blocks, ordered lowest→highest precedence, into a Chain.
// Domain entries are normalized (lowercase, trailing dots and space
// stripped); empty ones are dropped. An invalid IP/CIDR entry is an error
// naming the offending block and entry — policy compilation is the one place
// a typo can still be attributed to its source.
func NewChain(blocks []Block) (*Chain, error) {
	c := &Chain{
		blocks:   slices.Clone(blocks),
		compiled: make([]compiledBlock, 0, len(blocks)),
	}
	for _, b := range blocks {
		cb := compiledBlock{origin: b.Origin, name: b.Name}
		for _, e := range b.AllowDomains {
			cb.addNameRule(e, false)
		}
		for _, e := range b.DenyDomains {
			cb.addNameRule(e, true)
		}
		for _, e := range b.AllowIPs {
			if err := cb.addIPRule(e, false); err != nil {
				return nil, fmt.Errorf("block %s: %w", blockLabel(b), err)
			}
		}
		for _, e := range b.DenyIPs {
			if err := cb.addIPRule(e, true); err != nil {
				return nil, fmt.Errorf("block %s: %w", blockLabel(b), err)
			}
		}
		c.compiled = append(c.compiled, cb)
	}
	return c, nil
}

// blockLabel names a block in error messages: origin plus name when present.
func blockLabel(b Block) string {
	if b.Name != "" {
		return fmt.Sprintf("%s %q", b.Origin, b.Name)
	}
	return fmt.Sprintf("%q", b.Origin)
}

// normalizeDomain lowercases and strips the trailing dot and surrounding
// space from a pattern or a queried name, so both sides of every comparison
// share one canonical form.
func normalizeDomain(s string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s), "."))
}

func (cb *compiledBlock) addNameRule(entry string, deny bool) {
	p := normalizeDomain(entry)
	if p == "" || p == "*." {
		return
	}
	r := nameRule{entry: p, pattern: p, deny: deny}
	if rest, ok := strings.CutPrefix(p, "*."); ok {
		r.pattern = rest
		r.wildcard = true
	} else if deny {
		// A bare deny covers the whole subtree; a bare allow is exact.
		r.subtree = true
	}
	r.labels = strings.Count(r.pattern, ".") + 1
	cb.names = append(cb.names, r)
}

func (cb *compiledBlock) addIPRule(entry string, deny bool) error {
	e := strings.TrimSpace(entry)
	if e == "" {
		return nil
	}
	r := ipRule{entry: e, deny: deny}
	if strings.Contains(e, "/") {
		p, err := netip.ParsePrefix(e)
		if err != nil {
			return fmt.Errorf("parsing CIDR entry %q: %w", entry, err)
		}
		r.prefix = p
	} else {
		a, err := netip.ParseAddr(e)
		if err != nil {
			return fmt.Errorf("parsing IP entry %q: %w", entry, err)
		}
		r.addr = a.Unmap()
		r.exact = true
	}
	cb.ips = append(cb.ips, r)
	return nil
}

// DecideName walks blocks highest-precedence-first; the first block with an
// opinion on name decides. Within a block the most-specific matching entry
// wins — more labels is more specific, and an exact match beats a wildcard
// of equal depth — with deny winning a full specificity tie (the Cedar
// forbid-overrides-permit rule, scoped to one block). VerdictNone when no
// block matches: the caller's fail-closed default is unchanged.
func (c *Chain) DecideName(name string) (Verdict, Match) {
	name = normalizeDomain(name)
	if name == "" {
		return VerdictNone, Match{}
	}
	for i := len(c.compiled) - 1; i >= 0; i-- {
		if v, m := c.compiled[i].decideName(name); v != VerdictNone {
			return v, m
		}
	}
	return VerdictNone, Match{}
}

func (cb *compiledBlock) decideName(name string) (Verdict, Match) {
	var best *nameRule
	var bestExact bool
	for i := range cb.names {
		r := &cb.names[i]
		ok, exact := r.match(name)
		if !ok {
			continue
		}
		if best == nil || moreSpecificName(r, exact, best, bestExact) {
			best, bestExact = r, exact
		}
	}
	if best == nil {
		return VerdictNone, Match{}
	}
	return cb.verdictFor(best.entry, best.deny)
}

// moreSpecificName reports whether candidate (r, exact) beats the incumbent:
// deeper pattern first, exactness second, deny breaking the remaining tie.
func moreSpecificName(r *nameRule, exact bool, best *nameRule, bestExact bool) bool {
	if r.labels != best.labels {
		return r.labels > best.labels
	}
	if exact != bestExact {
		return exact
	}
	return r.deny && !best.deny
}

// DecideAddr walks blocks highest-precedence-first consulting BOTH rule
// spaces — the connection's name (when one is attributed) and its literal
// destination IP — in each block; the first block with an opinion on either
// decides. Running the two spaces in one walk is what makes precedence hold
// across them: a high block's `deny ip` must beat a low block's domain
// allow, which two independent walks (names fully first, then IPs) cannot
// express. Within one block, a deny in either space overrides an allow in
// the other — the Cedar forbid-overrides-permit rule, scoped to one block,
// same as the per-space tie rule.
//
// byName reports which space the verdict came from (meaningless when the
// verdict is VerdictNone). Callers need it because runtime grants sit
// BETWEEN the spaces in guardrail strength: a name verdict is the operator
// naming the destination — no grant overrides it — while IP rules rank
// below an explicit interactive grant (see AllowList.Allow).
func (c *Chain) DecideAddr(name string, ip netip.Addr) (v Verdict, m Match, byName bool) {
	name = normalizeDomain(name)
	ip = ip.Unmap()
	for i := len(c.compiled) - 1; i >= 0; i-- {
		nv, nm := VerdictNone, Match{}
		if name != "" {
			nv, nm = c.compiled[i].decideName(name)
		}
		iv, im := c.compiled[i].decideIP(ip)
		switch {
		case nv == VerdictNone && iv == VerdictNone:
			continue
		case iv == VerdictNone:
			return nv, nm, true
		case nv == VerdictNone:
			return iv, im, false
		case nv == VerdictDeny:
			return nv, nm, true
		case iv == VerdictDeny:
			return iv, im, false
		default: // both spaces allow; the name is the truer attribution
			return nv, nm, true
		}
	}
	return VerdictNone, Match{}, false
}

// DecideIP is the same highest-block-first walk for literal-IP rules.
// Within a block specificity is: exact IP > longer prefix > shorter prefix;
// deny wins ties.
func (c *Chain) DecideIP(ip netip.Addr) (Verdict, Match) {
	ip = ip.Unmap()
	for i := len(c.compiled) - 1; i >= 0; i-- {
		if v, m := c.compiled[i].decideIP(ip); v != VerdictNone {
			return v, m
		}
	}
	return VerdictNone, Match{}
}

func (cb *compiledBlock) decideIP(ip netip.Addr) (Verdict, Match) {
	var best *ipRule
	for i := range cb.ips {
		r := &cb.ips[i]
		if !r.contains(ip) {
			continue
		}
		if best == nil || moreSpecificIP(r, best) {
			best = r
		}
	}
	if best == nil {
		return VerdictNone, Match{}
	}
	return cb.verdictFor(best.entry, best.deny)
}

// verdictFor folds a winning rule into its verdict and attribution.
func (cb *compiledBlock) verdictFor(entry string, deny bool) (Verdict, Match) {
	v := VerdictAllow
	if deny {
		v = VerdictDeny
	}
	return v, Match{Origin: cb.origin, Block: cb.name, Entry: entry, Verdict: v}
}

func moreSpecificIP(r, best *ipRule) bool {
	if r.exact != best.exact {
		return r.exact
	}
	if !r.exact && r.prefix.Bits() != best.prefix.Bits() {
		return r.prefix.Bits() > best.prefix.Bits()
	}
	return r.deny && !best.deny
}

// AllowDomainPatterns returns the union of every block's AllowDomains
// (normalized, deduplicated, lowest block first) whose apex is not denied by
// a higher block. The periodic DNS resolver pre-resolves these apexes so
// SYNs to an allowed domain pass before the guest ever queries DNS; patterns
// a higher block has already overruled would only resolve IPs the chain then
// refuses, so they are skipped at the source.
func (c *Chain) AllowDomainPatterns() []string {
	var out []string
	seen := make(map[string]struct{})
	for i, b := range c.blocks {
		for _, e := range b.AllowDomains {
			p := normalizeDomain(e)
			if p == "" || p == "*." {
				continue
			}
			if _, dup := seen[p]; dup {
				continue
			}
			if c.apexDeniedAbove(i, strings.TrimPrefix(p, "*.")) {
				continue
			}
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	return out
}

// apexDeniedAbove reports whether the first higher block with an opinion on
// apex denies it — the same first-opinion walk as DecideName, restricted to
// blocks above i.
func (c *Chain) apexDeniedAbove(i int, apex string) bool {
	for j := len(c.compiled) - 1; j > i; j-- {
		if v, _ := c.compiled[j].decideName(apex); v != VerdictNone {
			return v == VerdictDeny
		}
	}
	return false
}

// allowIPCounts tallies exact-IP and prefix ALLOW rules across the chain,
// for AllowList.Size's observability report.
func (c *Chain) allowIPCounts() (exact, prefixes int) {
	for _, cb := range c.compiled {
		for _, r := range cb.ips {
			if r.deny {
				continue
			}
			if r.exact {
				exact++
			} else {
				prefixes++
			}
		}
	}
	return exact, prefixes
}
