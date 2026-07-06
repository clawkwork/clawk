package netfilter

import (
	"net"
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// mustChain compiles blocks or fails the test.
func mustChain(t *testing.T, blocks ...Block) *Chain {
	t.Helper()
	c, err := NewChain(blocks)
	require.NoErrorf(t, err, "NewChain")
	return c
}

func TestChainDecideNameWithinBlock(t *testing.T) {
	// One block: every case exercises within-block resolution only.
	c := mustChain(t, Block{
		Origin:       "policy",
		Name:         "corp",
		AllowDomains: []string{"*.example.com", "api.tracker.example.com", "exact.dev"},
		DenyDomains:  []string{"tracker.example.com", "*.internal.example.com", "exact.dev"},
	})
	cases := []struct {
		name  string
		want  Verdict
		entry string
	}{
		// Wildcard allows any depth of subdomain, never the apex.
		{"cdn.example.com", VerdictAllow, "*.example.com"},
		{"a.b.c.example.com", VerdictAllow, "*.example.com"},
		{"example.com", VerdictNone, ""},
		{"notexample.com", VerdictNone, ""},
		{"example.com.evil.org", VerdictNone, ""}, // suffix trick
		// Bare deny covers the apex and the whole subtree, and its extra
		// label beats the shallower allow wildcard.
		{"tracker.example.com", VerdictDeny, "tracker.example.com"},
		{"x.tracker.example.com", VerdictDeny, "tracker.example.com"},
		// ...but a deeper exact allow punches a hole in the denied subtree.
		{"api.tracker.example.com", VerdictAllow, "api.tracker.example.com"},
		// Deny wildcard vs allow wildcard: the deeper pattern wins.
		{"x.internal.example.com", VerdictDeny, "*.internal.example.com"},
		// Full specificity tie (same labels, both exact): deny wins.
		{"exact.dev", VerdictDeny, "exact.dev"},
		// Normalization: case and trailing dot on the queried name.
		{"CDN.Example.COM.", VerdictAllow, "*.example.com"},
	}
	for _, tt := range cases {
		v, m := c.DecideName(tt.name)
		if v != tt.want || m.Entry != tt.entry {
			t.Errorf("DecideName(%q) = %v/%q, want %v/%q", tt.name, v, m.Entry, tt.want, tt.entry)
		}
	}
}

func TestChainDecideNameCrossBlock(t *testing.T) {
	// The higher block's opinion decides even when the lower block's
	// matching rule is more specific — precedence is structural, between
	// blocks, never a specificity contest across them.
	c := mustChain(t,
		Block{Origin: "default", AllowDomains: []string{"very.specific.tracker.example.com"}},
		Block{Origin: "policy", Name: "oisd", DenyDomains: []string{"tracker.example.com"}, AllowDomains: []string{"good.example.com"}},
		Block{Origin: "custom", AllowDomains: []string{"*.tracker.example.com"}},
	)
	cases := []struct {
		name   string
		want   Verdict
		origin string
		block  string
	}{
		// Highest block has an opinion: its shallow wildcard beats the
		// policy's deny and the default's deeper exact allow.
		{"very.specific.tracker.example.com", VerdictAllow, "custom", ""},
		{"x.tracker.example.com", VerdictAllow, "custom", ""},
		// Highest block silent (wildcards never match the apex): the
		// policy's subtree deny decides, with attribution.
		{"tracker.example.com", VerdictDeny, "policy", "oisd"},
		{"good.example.com", VerdictAllow, "policy", "oisd"},
		// Nobody has an opinion.
		{"unrelated.org", VerdictNone, "", ""},
	}
	for _, tt := range cases {
		v, m := c.DecideName(tt.name)
		if v != tt.want || m.Origin != tt.origin || m.Block != tt.block {
			t.Errorf("DecideName(%q) = %v/%s/%s, want %v/%s/%s",
				tt.name, v, m.Origin, m.Block, tt.want, tt.origin, tt.block)
		}
	}
}

func TestChainDecideIP(t *testing.T) {
	c := mustChain(t,
		Block{Origin: "default", DenyIPs: []string{"10.9.0.0/16"}},
		Block{
			Origin:   "custom",
			AllowIPs: []string{"10.0.0.0/8", "10.1.2.3", "192.0.2.7"},
			DenyIPs:  []string{"10.1.0.0/16", "192.0.2.7", "10.2.0.0/16", "10.2.0.0/16"},
		},
	)
	cases := []struct {
		ip    string
		want  Verdict
		entry string
	}{
		{"10.200.0.1", VerdictAllow, "10.0.0.0/8"},    // only the /8 matches
		{"10.1.9.9", VerdictDeny, "10.1.0.0/16"},      // longer prefix beats /8
		{"10.1.2.3", VerdictAllow, "10.1.2.3"},        // exact IP beats /16 deny
		{"192.0.2.7", VerdictDeny, "192.0.2.7"},       // exact tie: deny wins
		{"10.2.5.5", VerdictDeny, "10.2.0.0/16"},      // deny wins prefix-length tie with itself
		{"10.9.5.5", VerdictAllow, "10.0.0.0/8"},      // higher block's /8 beats lower block's deny
		{"198.51.100.1", VerdictNone, ""},             // nobody has an opinion
		{"::ffff:10.1.2.3", VerdictAllow, "10.1.2.3"}, // 4-in-6 form unmapped
	}
	for _, tt := range cases {
		v, m := c.DecideIP(netip.MustParseAddr(tt.ip))
		if v != tt.want || m.Entry != tt.entry {
			t.Errorf("DecideIP(%s) = %v/%q, want %v/%q", tt.ip, v, m.Entry, tt.want, tt.entry)
		}
	}
}

func TestNewChainInvalidEntries(t *testing.T) {
	cases := []struct {
		name  string
		block Block
	}{
		{"bad IP", Block{Origin: "policy", Name: "corp", AllowIPs: []string{"not-an-ip"}}},
		{"bad CIDR", Block{Origin: "policy", Name: "corp", DenyIPs: []string{"10.0.0.0/99"}}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewChain([]Block{tt.block})
			require.Error(t, err)
			if !strings.Contains(err.Error(), "corp") {
				t.Errorf("error should name the block, got: %v", err)
			}
			bad := tt.block.AllowIPs
			if bad == nil {
				bad = tt.block.DenyIPs
			}
			if !strings.Contains(err.Error(), bad[0]) {
				t.Errorf("error should name the entry, got: %v", err)
			}
		})
	}
}

func TestChainAllowDomainPatterns(t *testing.T) {
	c := mustChain(t,
		Block{Origin: "default", AllowDomains: []string{"*.snapcraft.io", "github.com", "Dup.Example.ORG."}},
		Block{Origin: "custom", AllowDomains: []string{"dup.example.org", "*.corp.dev"}, DenyDomains: []string{"corp.dev"}},
		Block{Origin: "policy", Name: "oisd", DenyDomains: []string{"snapcraft.io"}},
	)
	got := c.AllowDomainPatterns()
	// *.snapcraft.io drops out: its apex is denied by a higher block.
	// *.corp.dev stays: only the same block denies its apex, and within a
	// block the resolver-time veto in Allow handles it.
	want := []string{"github.com", "dup.example.org", "*.corp.dev"}
	require.Equalf(t, want, got, "AllowDomainPatterns")
}

func TestChainVerdictNoneOnEmptyChain(t *testing.T) {
	c := mustChain(t)
	if v, _ := c.DecideName("anything.example.com"); v != VerdictNone {
		t.Errorf("empty chain DecideName = %v, want none", v)
	}
	if v, _ := c.DecideIP(netip.MustParseAddr("192.0.2.1")); v != VerdictNone {
		t.Errorf("empty chain DecideIP = %v, want none", v)
	}
	if got := c.AllowDomainPatterns(); len(got) != 0 {
		t.Errorf("empty chain AllowDomainPatterns = %v, want empty", got)
	}
}

func TestAllowListHigherBlockDenyBeatsLowerAllow(t *testing.T) {
	al := NewAllowListFromChain(mustChain(t,
		Block{Origin: "default", AllowDomains: []string{"*.example.com"}},
		Block{Origin: "policy", Name: "oisd", DenyDomains: []string{"tracker.example.com"}},
	))

	// A sibling the policy doesn't deny still flows through the allow.
	al.ObserveDNSAnswer("api.example.com", net.ParseIP("192.0.2.10"))
	if err := al.Allow("192.0.2.10:443"); err != nil {
		t.Fatalf("allowed subdomain should pass: %v", err)
	}

	// The denied subtree is refused despite the lower block's wildcard,
	// and the denial ledger attributes the deciding rule.
	al.ObserveDNSAnswer("tracker.example.com", net.ParseIP("192.0.2.11"))
	if err := al.Allow("192.0.2.11:443"); err == nil {
		t.Fatal("higher-block deny must beat lower-block allow")
	}
	denials := al.Denials()
	require.Len(t, denials, 1)
	if want := "policy oisd: deny tracker.example.com"; denials[0].Rule != want {
		t.Errorf("Denial.Rule = %q, want %q", denials[0].Rule, want)
	}
}

func TestAllowListGrantCannotOverrideChainDeny(t *testing.T) {
	al := NewAllowListFromChain(mustChain(t,
		Block{Origin: "policy", Name: "oisd", DenyDomains: []string{"evil.com"}},
	))
	al.ObserveDNSAnswer("cdn.evil.com", net.ParseIP("192.0.2.20"))
	al.grant(netip.MustParseAddr("192.0.2.20"))
	if err := al.Allow("192.0.2.20:443"); err == nil {
		t.Fatal("a runtime grant must not override a chain deny")
	}
	// An IP-level chain deny, by contrast, sits below the granted set.
	al2 := NewAllowListFromChain(mustChain(t,
		Block{Origin: "policy", Name: "corp", DenyIPs: []string{"192.0.2.0/24"}},
	))
	al2.grant(netip.MustParseAddr("192.0.2.21"))
	if err := al2.Allow("192.0.2.21:443"); err != nil {
		t.Fatalf("a grant should pass ahead of an IP rule: %v", err)
	}
}

func TestAllowListObservedOnlyWhenNameAllowed(t *testing.T) {
	al := NewAllowListFromChain(mustChain(t,
		Block{Origin: "custom", AllowDomains: []string{"*.ok.dev"}, DenyDomains: []string{"bad.ok.dev"}},
	))
	al.ObserveDNSAnswer("api.ok.dev", net.ParseIP("192.0.2.30"))   // allowed
	al.ObserveDNSAnswer("other.org", net.ParseIP("192.0.2.31"))    // no opinion
	al.ObserveDNSAnswer("x.bad.ok.dev", net.ParseIP("192.0.2.32")) // denied

	al.mu.RLock()
	defer al.mu.RUnlock()
	if _, ok := al.observed[netip.MustParseAddr("192.0.2.30")]; !ok {
		t.Error("allowed name should be in the observed set")
	}
	if _, ok := al.observed[netip.MustParseAddr("192.0.2.31")]; ok {
		t.Error("name without a verdict must not enter the observed set")
	}
	if _, ok := al.observed[netip.MustParseAddr("192.0.2.32")]; ok {
		t.Error("denied name must not enter the observed set")
	}
	// Attribution still records every answer.
	if got := al.lastName[netip.MustParseAddr("192.0.2.31")]; got != "other.org" {
		t.Errorf("lastName = %q, want other.org", got)
	}
}

func TestAllowListFailClosedDenialHasNoRule(t *testing.T) {
	al := NewAllowListFromChain(mustChain(t))
	if err := al.Allow("192.0.2.40:443"); err == nil {
		t.Fatal("expected fail-closed denial")
	}
	denials := al.Denials()
	require.Len(t, denials, 1)
	if denials[0].Rule != "" {
		t.Errorf("fail-closed Denial.Rule = %q, want empty", denials[0].Rule)
	}
}

func TestLegacyShimParity(t *testing.T) {
	// NewAllowList must behave exactly as the flat-list API always did:
	// one custom block carrying the allows, SetDeniedDomains layering its
	// denies into the same block, SetPolicy replacing only the allows.
	al, err := NewAllowList([]string{"192.0.2.50"}, []string{"10.0.0.0/24"}, []string{"*.good.dev"})
	require.NoError(t, err)

	if err := al.Allow("192.0.2.50:443"); err != nil {
		t.Errorf("static IP should be allowed: %v", err)
	}
	if err := al.Allow("10.0.0.9:443"); err != nil {
		t.Errorf("CIDR member should be allowed: %v", err)
	}
	al.ObserveDNSAnswer("api.good.dev", net.ParseIP("192.0.2.51"))
	if err := al.Allow("192.0.2.51:443"); err != nil {
		t.Errorf("observed wildcard subdomain should be allowed: %v", err)
	}

	// A deny on the same domain beats the wildcard allow (deeper entry) and
	// survives a SetPolicy that rewrites the allows.
	al.SetDeniedDomains([]string{"secret.good.dev"})
	al.ObserveDNSAnswer("secret.good.dev", net.ParseIP("192.0.2.52"))
	if err := al.Allow("192.0.2.52:443"); err == nil {
		t.Error("denied domain must be refused")
	}
	require.NoError(t, al.SetPolicy(nil, nil, []string{"*.good.dev"}))
	if got := al.DeniedDomains(); len(got) != 1 || got[0] != "secret.good.dev" {
		t.Errorf("DeniedDomains after SetPolicy = %v, want [secret.good.dev]", got)
	}
	if err := al.Allow("192.0.2.50:443"); err == nil {
		t.Error("SetPolicy should revoke the old static IP")
	}
}

// TestChainCrossSpacePrecedence: a higher block's IP deny must beat a lower
// block's domain allow. The two rule spaces are one precedence order, not
// two sequential walks — a low domain allow must not short-circuit past a
// high `deny ip` guardrail.
func TestChainCrossSpacePrecedence(t *testing.T) {
	al := NewAllowListFromChain(mustChain(t,
		Block{Origin: "custom", AllowDomains: []string{"evil.com"}},
		Block{Origin: "policy", Name: "guard", DenyIPs: []string{"203.0.113.0/24"}},
	))
	al.ObserveDNSAnswer("evil.com", net.ParseIP("203.0.113.5"))
	if err := al.Allow("203.0.113.5:443"); err == nil {
		t.Fatal("high-block deny ip must beat low-block domain allow")
	}
	// The same layout the other way up: the domain allow outranks the IP
	// deny when it sits in the higher block.
	al2 := NewAllowListFromChain(mustChain(t,
		Block{Origin: "policy", Name: "guard", DenyIPs: []string{"203.0.113.0/24"}},
		Block{Origin: "custom", AllowDomains: []string{"evil.com"}},
	))
	al2.ObserveDNSAnswer("evil.com", net.ParseIP("203.0.113.5"))
	if err := al2.Allow("203.0.113.5:443"); err != nil {
		t.Fatalf("high-block domain allow should beat low-block ip deny: %v", err)
	}
}
