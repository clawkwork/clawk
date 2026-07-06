package netfilter

import (
	"errors"
	"net"
	"net/netip"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// newTestGate returns an AllowList with an interactive gate enabled and a
// short hold timeout, plus the gate, for white-box testing.
func newTestGate(t *testing.T, timeout time.Duration) (*AllowList, *Gate) {
	t.Helper()
	al, err := NewAllowList(nil, nil, nil)
	require.NoErrorf(t, err, "NewAllowList")
	g := al.EnableInteractive()
	g.timeout = timeout
	t.Cleanup(g.Close)
	return al, g
}

// recvEvent reads one event with a test deadline so a wiring bug fails the
// test instead of hanging it.
func recvEvent(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("event channel closed unexpectedly")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for gate event")
		return Event{}
	}
}

func recvDecision(t *testing.T, ch <-chan Decision) Decision {
	t.Helper()
	select {
	case d := <-ch:
		return d
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Decide to return")
		return Decision{}
	}
}

// waitFor polls cond until it holds or a test deadline elapses. Used to
// observe a goroutine reaching a registration point without racing on it.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("condition not met before deadline")
}

// waiters reports how many connections are coalesced behind the hold for key.
func (g *Gate) waitersFor(key netip.Addr) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if p, ok := g.byKey[key]; ok {
		return p.waiters
	}
	return 0
}

func TestGateNoSubscriberDeniesImmediately(t *testing.T) {
	_, g := newTestGate(t, time.Minute)
	ip := netip.MustParseAddr("203.0.113.5")
	if d := g.Decide(ip, "blocked.example", "443"); d.Action != ActionDeny {
		t.Fatalf("Decide with no subscriber = %v, want deny", d.Action)
	}
}

func TestGateAllowReleasesAndGrants(t *testing.T) {
	al, g := newTestGate(t, time.Minute)
	events, cancel := g.Subscribe()
	defer cancel()

	ip := netip.MustParseAddr("203.0.113.7")
	got := make(chan Decision, 1)
	go func() { got <- g.Decide(ip, "api.example", "443") }()

	ev := recvEvent(t, events)
	if ev.Type != EventPending || ev.Pending == nil || ev.Pending.IP != ip.String() {
		t.Fatalf("first event = %+v, want pending for %s", ev, ip)
	}
	require.NoErrorf(t, g.Resolve(ev.ID, Decision{Action: ActionAllow, Scope: ScopeSession}), "Resolve")
	if d := recvDecision(t, got); d.Action != ActionAllow {
		t.Fatalf("Decide returned %v, want allow", d.Action)
	}
	if resolved := recvEvent(t, events); resolved.Type != EventResolved || resolved.Action != "allow" {
		t.Fatalf("resolved event = %+v, want resolved/allow", resolved)
	}

	// Session scope grants the IP: a subsequent Allow passes without the
	// gate prompting again.
	if err := al.Allow(netip.AddrPortFrom(ip, 443).String()); err != nil {
		t.Fatalf("granted IP should now be allowed, got %v", err)
	}
}

func TestGateDenyDoesNotGrant(t *testing.T) {
	al, g := newTestGate(t, time.Minute)
	events, cancel := g.Subscribe()
	defer cancel()

	ip := netip.MustParseAddr("203.0.113.8")
	got := make(chan Decision, 1)
	go func() { got <- g.Decide(ip, "", "443") }()

	ev := recvEvent(t, events)
	require.NoErrorf(t, g.Resolve(ev.ID, Decision{Action: ActionDeny}), "Resolve")
	if d := recvDecision(t, got); d.Action != ActionDeny {
		t.Fatalf("Decide returned %v, want deny", d.Action)
	}
	al.mu.RLock()
	_, granted := al.granted[ip]
	al.mu.RUnlock()
	if granted {
		t.Fatal("deny must not grant the IP")
	}
}

func TestGateOnceDoesNotGrant(t *testing.T) {
	al, g := newTestGate(t, time.Minute)
	events, cancel := g.Subscribe()
	defer cancel()

	ip := netip.MustParseAddr("203.0.113.9")
	got := make(chan Decision, 1)
	go func() { got <- g.Decide(ip, "", "443") }()
	ev := recvEvent(t, events)
	require.NoErrorf(t, g.Resolve(ev.ID, Decision{Action: ActionAllow, Scope: ScopeOnce}), "Resolve")
	recvDecision(t, got)

	al.mu.RLock()
	_, granted := al.granted[ip]
	al.mu.RUnlock()
	if granted {
		t.Fatal("once scope must not grant the IP for future connections")
	}
}

func TestGateAlwaysInvokesPersistHook(t *testing.T) {
	_, g := newTestGate(t, time.Minute)
	persisted := make(chan string, 1)
	g.SetOnAlways(func(host string, ip netip.Addr) { persisted <- host })

	events, cancel := g.Subscribe()
	defer cancel()
	ip := netip.MustParseAddr("203.0.113.10")
	go g.Decide(ip, "always.example", "443")
	ev := recvEvent(t, events)
	require.NoErrorf(t, g.Resolve(ev.ID, Decision{Action: ActionAllow, Scope: ScopeAlways}), "Resolve")
	select {
	case host := <-persisted:
		if host != "always.example" {
			t.Fatalf("persist hook host = %q, want always.example", host)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("always scope did not invoke persist hook")
	}
}

func TestGateTimeoutDenies(t *testing.T) {
	_, g := newTestGate(t, 20*time.Millisecond)
	events, cancel := g.Subscribe()
	defer cancel()

	ip := netip.MustParseAddr("203.0.113.11")
	got := make(chan Decision, 1)
	go func() { got <- g.Decide(ip, "", "443") }()

	if ev := recvEvent(t, events); ev.Type != EventPending {
		t.Fatalf("want pending, got %+v", ev)
	}
	// Do not resolve: the deadline fires and denies.
	if d := recvDecision(t, got); d.Action != ActionDeny {
		t.Fatalf("timed-out hold returned %v, want deny", d.Action)
	}
	if ev := recvEvent(t, events); ev.Type != EventResolved || ev.Action != "deny" {
		t.Fatalf("want resolved/deny, got %+v", ev)
	}
}

func TestGateCoalescesByDestination(t *testing.T) {
	_, g := newTestGate(t, time.Minute)
	events, cancel := g.Subscribe()
	defer cancel()

	ip := netip.MustParseAddr("203.0.113.12")
	got := make(chan Decision, 2)
	go func() { got <- g.Decide(ip, "dup.example", "443") }()

	ev := recvEvent(t, events)
	if ev.Type != EventPending {
		t.Fatalf("want first pending, got %+v", ev)
	}
	// A second connection to the same IP must join the same hold — one
	// prompt, released together. Wait for it to register before resolving
	// so the join is observed, not raced.
	go func() { got <- g.Decide(ip, "dup.example", "8443") }()
	waitFor(t, func() bool { return g.waitersFor(ip) == 2 })

	require.NoErrorf(t, g.Resolve(ev.ID, Decision{Action: ActionAllow, Scope: ScopeOnce}), "Resolve")
	for i := 0; i < 2; i++ {
		if d := recvDecision(t, got); d.Action != ActionAllow {
			t.Fatalf("waiter %d returned %v, want allow", i, d.Action)
		}
	}
	// Exactly one resolved event for the coalesced hold.
	if ev := recvEvent(t, events); ev.Type != EventResolved {
		t.Fatalf("want resolved, got %+v", ev)
	}
}

func TestGateHoldCapFailsClosed(t *testing.T) {
	_, g := newTestGate(t, time.Minute)
	g.maxHold = 2
	events, cancel := g.Subscribe()
	defer cancel()

	// Fill the cap with two distinct holds.
	for i := 1; i <= 2; i++ {
		ip := netip.AddrFrom4([4]byte{203, 0, 113, byte(i)})
		go g.Decide(ip, "", "443")
		recvEvent(t, events) // pending
	}
	// The third distinct destination exceeds the cap and is denied at once.
	over := netip.MustParseAddr("203.0.113.250")
	if d := g.Decide(over, "", "443"); d.Action != ActionDeny {
		t.Fatalf("over-cap Decide = %v, want deny", d.Action)
	}
}

func TestGateCloseReleasesWaiters(t *testing.T) {
	_, g := newTestGate(t, time.Minute)
	events, cancel := g.Subscribe()
	defer cancel()

	ip := netip.MustParseAddr("203.0.113.30")
	got := make(chan Decision, 1)
	go func() { got <- g.Decide(ip, "", "443") }()
	recvEvent(t, events)

	g.Close()
	if d := recvDecision(t, got); d.Action != ActionDeny {
		t.Fatalf("Close released waiter with %v, want deny", d.Action)
	}
	// After close, Decide denies immediately even with the (now closed) sub.
	if d := g.Decide(ip, "", "443"); d.Action != ActionDeny {
		t.Fatalf("Decide after Close = %v, want deny", d.Action)
	}
}

func TestGateSubscribeReplaysPending(t *testing.T) {
	_, g := newTestGate(t, time.Minute)
	first, cancelFirst := g.Subscribe()
	defer cancelFirst()

	ip := netip.MustParseAddr("203.0.113.40")
	go g.Decide(ip, "replay.example", "443")
	recvEvent(t, first) // first subscriber sees it live

	// A second subscriber attaching afterward must be replayed the
	// outstanding hold.
	second, cancelSecond := g.Subscribe()
	defer cancelSecond()
	ev := recvEvent(t, second)
	if ev.Type != EventPending || ev.Pending == nil || ev.Pending.Host != "replay.example" {
		t.Fatalf("replayed event = %+v, want pending replay.example", ev)
	}
}

func TestGateAllowAllReleasesHoldsAndBypasses(t *testing.T) {
	al, g := newTestGate(t, time.Minute)
	events, cancel := g.Subscribe()
	defer cancel()

	// Two distinct destinations held.
	ipA := netip.MustParseAddr("203.0.113.60")
	ipB := netip.MustParseAddr("203.0.113.61")
	gotA := make(chan Decision, 1)
	gotB := make(chan Decision, 1)
	go func() { gotA <- g.Decide(ipA, "", "443") }()
	recvEvent(t, events)
	go func() { gotB <- g.Decide(ipB, "", "443") }()
	recvEvent(t, events)

	g.AllowAll(time.Hour)

	// Both holds release as allowed.
	if d := recvDecision(t, gotA); d.Action != ActionAllow {
		t.Errorf("hold A = %v, want allow", d.Action)
	}
	if d := recvDecision(t, gotB); d.Action != ActionAllow {
		t.Errorf("hold B = %v, want allow", d.Action)
	}

	// New, never-seen destination now passes outright (no prompt).
	fresh := netip.MustParseAddr("203.0.113.62")
	if err := al.Allow(netip.AddrPortFrom(fresh, 443).String()); err != nil {
		t.Errorf("with bypass active, fresh dest should pass, got %v", err)
	}
	if g.AllowAllUntil().IsZero() {
		t.Error("AllowAllUntil should report an active bypass")
	}
}

func TestAllowListAllowAllForExpiry(t *testing.T) {
	al, err := NewAllowList(nil, nil, nil)
	require.NoError(t, err)
	// Drive time deterministically.
	now := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	al.now = func() time.Time { return now }

	al.AllowAllFor(time.Hour)
	if err := al.Allow("198.51.100.1:443"); err != nil {
		t.Errorf("during bypass, expected allow, got %v", err)
	}

	// Advance past expiry: the bypass lapses and the destination is denied.
	now = now.Add(2 * time.Hour)
	if err := al.Allow("198.51.100.1:443"); err == nil {
		t.Error("after bypass expiry, expected denial")
	}
	if !al.AllowAllUntil().IsZero() {
		t.Error("AllowAllUntil should be zero once elapsed")
	}

	// Clearing with a non-positive duration disables it immediately.
	al.AllowAllFor(time.Hour)
	al.AllowAllFor(0)
	if err := al.Allow("198.51.100.2:443"); err == nil {
		t.Error("after clear, expected denial")
	}
}

func TestGateDenyDomainBlocksWholeTree(t *testing.T) {
	al, g := newTestGate(t, time.Minute)
	persisted := make(chan string, 1)
	g.SetOnDenyDomain(func(d string) { persisted <- d })
	events, cancel := g.Subscribe()
	defer cancel()

	// Guest resolved telemetry.evil.com → an IP; the connection is held.
	al.ObserveDNSAnswer("telemetry.evil.com", net.ParseIP("203.0.113.70"))
	errc := make(chan error, 1)
	go func() { errc <- al.Allow("203.0.113.70:443") }()

	ev := recvEvent(t, events)
	if ev.Pending == nil || ev.Pending.Host != "telemetry.evil.com" {
		t.Fatalf("pending host = %+v, want telemetry.evil.com", ev.Pending)
	}
	require.NoErrorf(t, g.Resolve(ev.ID, Decision{Action: ActionDeny, Scope: ScopeAlways}), "Resolve")
	if err := <-errc; err == nil {
		t.Fatal("held connection should have been denied")
	}

	// The registrable domain is persisted (evil.com, not the full host).
	select {
	case d := <-persisted:
		if d != "evil.com" {
			t.Errorf("blocked domain = %q, want evil.com", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("onDenyDomain was not invoked")
	}

	// A *sibling* subdomain on a different IP is now auto-denied without a
	// prompt — and the block beats an explicit grant.
	al.ObserveDNSAnswer("ads.evil.com", net.ParseIP("203.0.113.71"))
	al.grant(netip.MustParseAddr("203.0.113.71"))
	if err := al.Allow("203.0.113.71:443"); err == nil {
		t.Fatal("a granted IP under a blocked domain must still be refused")
	}
}

func TestNameUnderAnyDomain(t *testing.T) {
	denied := []string{"prout.com"}
	tests := []struct {
		name string
		want bool
	}{
		{"prout.com", true},           // the apex itself
		{"www.prout.com", true},       // a subdomain
		{"a.b.c.prout.com", true},     // a deep subdomain
		{"xprout.com", false},         // lookalike — must NOT match
		{"prout.com.evil.com", false}, // suffix trick — must NOT match
		{"prout.org", false},          // different TLD
		{"notprout.com", false},       // unrelated
	}
	for _, tt := range tests {
		if got := nameUnderAnyDomain(denied, tt.name); got != tt.want {
			t.Errorf("nameUnderAnyDomain([prout.com], %q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestRootDomain(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"telemetry.evil.com", "evil.com"},
		{"evil.com", "evil.com"},
		{"a.b.example.co.uk", "example.co.uk"},
		{"example.co.uk", "example.co.uk"},
		{"203.0.113.5", ""}, // literal IP
		{"", ""},
		{"localhost", ""}, // no registrable domain
	}
	for _, tt := range tests {
		if got := rootDomain(tt.in); got != tt.want {
			t.Errorf("rootDomain(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSetDeniedDomainsOverridesAllow(t *testing.T) {
	al, err := NewAllowList(nil, nil, nil)
	require.NoError(t, err)
	al.SetDeniedDomains([]string{"evil.com."}) // trailing dot tolerated
	al.ObserveDNSAnswer("cdn.evil.com", net.ParseIP("198.51.100.5"))
	al.grant(netip.MustParseAddr("198.51.100.5")) // even granted → still blocked

	if err := al.Allow("198.51.100.5:443"); err == nil {
		t.Fatal("blocked domain should override the grant")
	}
	if got := al.DeniedDomains(); len(got) != 1 || got[0] != "evil.com" {
		t.Errorf("DeniedDomains = %v, want [evil.com]", got)
	}
}

func TestGateResolveUnknown(t *testing.T) {
	_, g := newTestGate(t, time.Minute)
	if err := g.Resolve("nope#1", Decision{Action: ActionAllow}); !errors.Is(err, ErrUnknownDecision) {
		t.Fatalf("Resolve(unknown) = %v, want ErrUnknownDecision", err)
	}
}

func TestParseDecision(t *testing.T) {
	tests := []struct {
		name    string
		action  string
		scope   string
		want    Decision
		wantErr bool
	}{
		{name: "allow once default", action: "allow", scope: "", want: Decision{ActionAllow, ScopeOnce}},
		{name: "allow session", action: "allow", scope: "session", want: Decision{ActionAllow, ScopeSession}},
		{name: "allow always", action: "allow", scope: "always", want: Decision{ActionAllow, ScopeAlways}},
		{name: "deny", action: "deny", scope: "once", want: Decision{ActionDeny, ScopeOnce}},
		{name: "bad action", action: "maybe", scope: "once", wantErr: true},
		{name: "bad scope", action: "allow", scope: "forever", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseDecision(tt.action, tt.scope)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseDecision(%q,%q) err = %v, wantErr %v", tt.action, tt.scope, err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Fatalf("ParseDecision(%q,%q) = %+v, want %+v", tt.action, tt.scope, got, tt.want)
			}
		})
	}
}
