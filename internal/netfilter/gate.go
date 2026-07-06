package netfilter

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/publicsuffix"
)

// Gate turns the allow list's fail-closed deny into an interactive
// allow/deny prompt. When the AllowList would refuse a connection, it asks
// the Gate to Decide: the Gate holds the calling goroutine (one of gvisor's
// per-connection forwarder goroutines, so blocking is local to that
// connection), publishes a "pending" event to every subscriber — a menubar
// app or `clawk network watch` — and unblocks when a decision arrives via
// Resolve, the hold times out, or the Gate is closed.
//
// The Gate only engages when at least one decider is subscribed. With no UI
// attached, Decide returns Deny immediately and the AllowList behaves exactly
// as it did before this feature existed — running the UI is what turns
// interactive prompting on.
//
// Holds are coalesced per destination IP: several connections to the same
// host wait on a single prompt and are released together. Concurrent holds
// are capped (maxHold); past the cap, Decide fails closed rather than queueing
// an unbounded backlog of prompts.
type Gate struct {
	al      *AllowList
	timeout time.Duration
	maxHold int

	mu      sync.Mutex
	byKey   map[netip.Addr]*pending // coalescing key: destination IP
	byID    map[string]*pending     // resolve lookup: unique per hold
	seq     uint64
	subs    map[int]chan Event
	nextSub int
	closed  bool

	// onAlways, if set, is invoked (off the lock, on the resolver's
	// goroutine) when a hold is allowed with ScopeAlways — the daemon
	// wires this to persist the grant to the sandbox's network policy.
	onAlways func(host string, ip netip.Addr)

	// onDenyDomain, if set, is invoked when a hold is denied with
	// ScopeAlways — the daemon wires this to persist the blocked registrable
	// domain to the sandbox's network policy.
	onDenyDomain func(domain string)

	now func() time.Time
}

// Action is the verdict on a held connection.
type Action int

const (
	// ActionDeny refuses the connection (RST to the guest), as the
	// non-interactive ACL would.
	ActionDeny Action = iota
	// ActionAllow lets the held connection through.
	ActionAllow
)

// String renders the wire form of an Action.
func (a Action) String() string {
	if a == ActionAllow {
		return "allow"
	}
	return "deny"
}

// Scope qualifies how long an ActionAllow persists.
type Scope int

const (
	// ScopeOnce allows only the connection(s) currently held for this
	// destination; the next connection to it prompts again.
	ScopeOnce Scope = iota
	// ScopeSession grants the destination IP for the life of the VM. Not
	// persisted: a restart forgets it.
	ScopeSession
	// ScopeAlways grants the destination and persists it to the sandbox's
	// network policy so it survives restarts.
	ScopeAlways
)

// String renders the wire form of a Scope.
func (s Scope) String() string {
	switch s {
	case ScopeSession:
		return "session"
	case ScopeAlways:
		return "always"
	default:
		return "once"
	}
}

// Decision is a resolved verdict for a held connection. The zero value denies.
type Decision struct {
	Action Action
	Scope  Scope
}

// ParseDecision maps wire-form action and scope strings to a Decision. An
// empty scope defaults to ScopeOnce.
func ParseDecision(action, scope string) (Decision, error) {
	var d Decision
	switch action {
	case "allow":
		d.Action = ActionAllow
	case "deny":
		d.Action = ActionDeny
	default:
		return Decision{}, fmt.Errorf("unknown action %q (want allow or deny)", action)
	}
	switch scope {
	case "", "once":
		d.Scope = ScopeOnce
	case "session":
		d.Scope = ScopeSession
	case "always":
		d.Scope = ScopeAlways
	default:
		return Decision{}, fmt.Errorf("unknown scope %q (want once, session, or always)", scope)
	}
	return d, nil
}

// Pending is a snapshot of one connection held awaiting a decision.
type Pending struct {
	ID   string `json:"id"`
	Host string `json:"host"` // DNS name the guest resolved, else the IP
	IP   string `json:"ip"`
	Port string `json:"port"`
	// Waiters is how many connections to this destination are coalesced
	// behind this single prompt — a UI can surface "3 connections waiting".
	Waiters  int       `json:"waiters"`
	Deadline time.Time `json:"deadline"`
}

// EventType distinguishes the lifecycle events on the gate's stream.
type EventType string

const (
	// EventPending announces a newly held connection.
	EventPending EventType = "pending"
	// EventResolved announces that a held connection was decided (by a
	// subscriber or by timeout).
	EventResolved EventType = "resolved"
)

// Event is one item on a subscriber's stream.
type Event struct {
	Type    EventType `json:"type"`
	ID      string    `json:"id"`
	Pending *Pending  `json:"pending,omitempty"` // set for EventPending
	Action  string    `json:"action,omitempty"`  // set for EventResolved
}

type pending struct {
	id       string
	key      netip.Addr
	host     string
	port     string
	waiters  int
	deadline time.Time
	timer    *time.Timer
	done     chan struct{} // closed when decision is set
	decision Decision
	resolved bool
}

func (p *pending) snapshot() *Pending {
	return &Pending{
		ID:       p.id,
		Host:     p.host,
		IP:       p.key.String(),
		Port:     p.port,
		Waiters:  p.waiters,
		Deadline: p.deadline,
	}
}

const (
	defaultHoldTimeout = 30 * time.Second

	// defaultMaxHold caps concurrent distinct holds. It sits below the
	// forwarder's maxInFlight so held connections can never starve every
	// slot the stack has for allowed traffic.
	defaultMaxHold = 16

	// eventBuffer is the per-subscriber backlog. A subscriber that falls
	// this far behind is dropped rather than allowed to stall the network
	// path (see broadcastLocked).
	eventBuffer = 64
)

func newGate(al *AllowList) *Gate {
	return &Gate{
		al:      al,
		timeout: defaultHoldTimeout,
		maxHold: defaultMaxHold,
		byKey:   make(map[netip.Addr]*pending),
		byID:    make(map[string]*pending),
		subs:    make(map[int]chan Event),
		now:     time.Now,
	}
}

// SetOnAlways registers the callback invoked when a hold is allowed with
// ScopeAlways. Set it once, before the gate serves traffic.
func (g *Gate) SetOnAlways(fn func(host string, ip netip.Addr)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.onAlways = fn
}

// SetOnDenyDomain registers the callback invoked when a hold is denied with
// ScopeAlways (the blocked registrable domain). Set it once, before serving.
func (g *Gate) SetOnDenyDomain(fn func(domain string)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.onDenyDomain = fn
}

// Decide holds the calling goroutine until the connection to key (the
// destination IP) is allowed or denied. host is the DNS name the guest
// resolved for key, if known, shown to the human; port is informational.
//
// It returns Deny immediately when no decider is subscribed, when the gate is
// closed, or when the concurrent-hold cap is reached — in every such case the
// AllowList records the denial and refuses the connection, exactly as the
// non-interactive path does.
func (g *Gate) Decide(key netip.Addr, host, port string) Decision {
	g.mu.Lock()
	if g.closed || len(g.subs) == 0 {
		g.mu.Unlock()
		return Decision{Action: ActionDeny}
	}
	p, ok := g.byKey[key]
	if !ok {
		if len(g.byKey) >= g.maxHold {
			g.mu.Unlock()
			return Decision{Action: ActionDeny}
		}
		g.seq++
		p = &pending{
			id:       fmt.Sprintf("%s#%d", key, g.seq),
			key:      key,
			host:     host,
			port:     port,
			waiters:  1,
			deadline: g.now().Add(g.timeout),
			done:     make(chan struct{}),
		}
		g.byKey[key] = p
		g.byID[p.id] = p
		p.timer = time.AfterFunc(g.timeout, func() { g.expire(p.id) })
		g.broadcastLocked(Event{Type: EventPending, ID: p.id, Pending: p.snapshot()})
	} else {
		p.waiters++
	}
	done := p.done
	g.mu.Unlock()

	<-done

	g.mu.Lock()
	d := p.decision
	g.mu.Unlock()
	return d
}

// Resolve decides the held connection identified by id. It returns
// ErrUnknownDecision if no such hold is outstanding (already resolved,
// expired, or never existed).
func (g *Gate) Resolve(id string, d Decision) error {
	g.mu.Lock()
	p, ok := g.byID[id]
	if !ok {
		g.mu.Unlock()
		return ErrUnknownDecision
	}
	g.finishLocked(p, d)
	host, key := p.host, p.key
	g.mu.Unlock()

	// Side effects run off the lock, on this caller's goroutine (the
	// control-socket handler), never on the held network path.
	if d.Action == ActionAllow && d.Scope != ScopeOnce {
		g.al.grant(key)
	}
	if d.Action == ActionAllow && d.Scope == ScopeAlways && g.onAlways != nil {
		g.onAlways(host, key)
	}
	// Deny + Always blocks the connection's whole registrable domain (apex +
	// every subdomain) from here on, persisted by the daemon.
	if d.Action == ActionDeny && d.Scope == ScopeAlways {
		if domain := rootDomain(host); domain != "" {
			g.al.denyDomain(domain)
			if g.onDenyDomain != nil {
				g.onDenyDomain(domain)
			}
		}
	}
	return nil
}

// rootDomain returns the registrable (root) domain of host — "evil.com" for
// "telemetry.evil.com", correctly handling multi-label suffixes like
// "co.uk". It returns "" when host is empty, a literal IP, or has no
// registrable domain, in which case a Deny+Always degrades to a one-shot deny.
func rootDomain(host string) string {
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return ""
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return "" // literal IP: nothing to block by domain
	}
	domain, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return ""
	}
	return domain
}

// AllowAll opens a time-boxed bypass on the allow list (every destination
// passes for d) and releases every currently held connection as allowed.
// While the bypass is active no new holds occur, since Allow passes before
// reaching the gate. Used by the "allow all for 1h" escape hatch.
func (g *Gate) AllowAll(d time.Duration) {
	g.al.AllowAllFor(d)

	g.mu.Lock()
	defer g.mu.Unlock()
	held := make([]*pending, 0, len(g.byKey))
	for _, p := range g.byKey {
		held = append(held, p)
	}
	for _, p := range held {
		g.finishLocked(p, Decision{Action: ActionAllow, Scope: ScopeOnce})
	}
}

// AllowAllUntil reports when the time-boxed bypass expires (zero if inactive).
func (g *Gate) AllowAllUntil() time.Time { return g.al.AllowAllUntil() }

// expire denies a hold that outlived its deadline. A no-op if it was already
// resolved.
func (g *Gate) expire(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if p, ok := g.byID[id]; ok {
		g.finishLocked(p, Decision{Action: ActionDeny})
	}
}

// finishLocked records the decision, removes the hold, notifies subscribers,
// and releases every waiter. The caller holds g.mu.
func (g *Gate) finishLocked(p *pending, d Decision) {
	if p.resolved {
		return
	}
	p.resolved = true
	p.decision = d
	if p.timer != nil {
		p.timer.Stop()
	}
	delete(g.byKey, p.key)
	delete(g.byID, p.id)
	g.broadcastLocked(Event{Type: EventResolved, ID: p.id, Action: d.Action.String()})
	close(p.done)
}

// Subscribe registers a stream of gate events and returns it with a cancel
// func that must be called to release it. Outstanding holds are replayed
// immediately so a UI attaching mid-flight sees what's already waiting.
func (g *Gate) Subscribe() (<-chan Event, func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	id := g.nextSub
	g.nextSub++
	ch := make(chan Event, eventBuffer)
	g.subs[id] = ch
	pendings := make([]*pending, 0, len(g.byKey))
	for _, p := range g.byKey {
		pendings = append(pendings, p)
	}
	slices.SortFunc(pendings, func(a, b *pending) int { return a.deadline.Compare(b.deadline) })
	for _, p := range pendings {
		ch <- Event{Type: EventPending, ID: p.id, Pending: p.snapshot()}
	}

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			if _, ok := g.subs[id]; ok {
				delete(g.subs, id)
				close(ch)
			}
		})
	}
	return ch, cancel
}

// broadcastLocked fans an event out to every subscriber. A subscriber whose
// buffer is full is dropped (its channel closed) rather than allowed to block
// the network path. The caller holds g.mu.
func (g *Gate) broadcastLocked(ev Event) {
	for id, ch := range g.subs {
		select {
		case ch <- ev:
		default:
			delete(g.subs, id)
			close(ch)
		}
	}
}

// Pending returns a snapshot of every outstanding hold, soonest deadline
// first.
func (g *Gate) Pending() []Pending {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Pending, 0, len(g.byKey))
	for _, p := range g.byKey {
		out = append(out, *p.snapshot())
	}
	slices.SortFunc(out, func(a, b Pending) int { return a.Deadline.Compare(b.Deadline) })
	return out
}

// Subscribers reports how many deciders are currently attached.
func (g *Gate) Subscribers() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.subs)
}

// Close denies every outstanding hold, releases all waiters, and drops all
// subscribers. Used on daemon shutdown so no forwarder goroutine is left
// blocked. Safe to call more than once.
func (g *Gate) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true
	for _, p := range g.byKey {
		if !p.resolved {
			p.resolved = true
			p.decision = Decision{Action: ActionDeny}
			if p.timer != nil {
				p.timer.Stop()
			}
			close(p.done)
		}
	}
	g.byKey = make(map[netip.Addr]*pending)
	g.byID = make(map[string]*pending)
	for id, ch := range g.subs {
		delete(g.subs, id)
		close(ch)
	}
}
