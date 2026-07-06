package cli

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/clawkwork/clawk/internal/netfilter"
	"github.com/stretchr/testify/require"
)

type decideCall struct{ id, action, scope string }

// fakeDecider feeds a canned event stream and records decisions, standing in
// for the control-socket client in watch-loop tests.
type fakeDecider struct {
	events  chan netfilter.Event
	pending []netfilter.Pending

	mu          sync.Mutex
	decisions   []decideCall
	allowAllFor time.Duration
}

func (f *fakeDecider) Events(context.Context) (<-chan netfilter.Event, error) { return f.events, nil }
func (f *fakeDecider) Pending(context.Context) ([]netfilter.Pending, error)   { return f.pending, nil }
func (f *fakeDecider) Decide(_ context.Context, id, action, scope string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decisions = append(f.decisions, decideCall{id, action, scope})
	return nil
}
func (f *fakeDecider) AllowAll(_ context.Context, d time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.allowAllFor = d
	return nil
}

func (f *fakeDecider) recorded() []decideCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]decideCall(nil), f.decisions...)
}

func TestRunWatchMapsInputToDecision(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		action string
		scope  string
	}{
		{name: "allow once", input: "a\n", action: "allow", scope: "once"},
		{name: "allow session", input: "s\n", action: "allow", scope: "session"},
		{name: "allow always short", input: "A\n", action: "allow", scope: "always"},
		{name: "allow always word", input: "always\n", action: "allow", scope: "always"},
		{name: "explicit deny", input: "d\n", action: "deny", scope: "once"},
		{name: "deny domain", input: "D\n", action: "deny", scope: "always"},
		{name: "unknown denies", input: "wat\n", action: "deny", scope: "once"},
		{name: "eof denies", input: "", action: "deny", scope: "once"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch := make(chan netfilter.Event, 1)
			ch <- netfilter.Event{
				Type:    netfilter.EventPending,
				ID:      "203.0.113.1#1",
				Pending: &netfilter.Pending{ID: "203.0.113.1#1", Host: "api.example", IP: "203.0.113.1", Port: "443"},
			}
			close(ch)
			f := &fakeDecider{events: ch}

			var out bytes.Buffer
			// Stream ends after the one event, so runWatch returns; we only
			// care that the decision was recorded.
			_ = runWatch(context.Background(), []watchTarget{{name: "test", client: f}}, &out, strings.NewReader(tt.input))

			got := f.recorded()
			require.Len(t, got, 1, "recorded decisions")
			require.Equal(t, tt.action, got[0].action, "decision action")
			require.Equal(t, tt.scope, got[0].scope, "decision scope")
			require.Contains(t, out.String(), "api.example", "prompt should name the host")
		})
	}
}

func TestRunWatchAllowAll(t *testing.T) {
	ch := make(chan netfilter.Event, 1)
	ch <- netfilter.Event{
		Type:    netfilter.EventPending,
		ID:      "203.0.113.5#1",
		Pending: &netfilter.Pending{ID: "203.0.113.5#1", Host: "x.example", IP: "203.0.113.5", Port: "443"},
	}
	close(ch)
	f := &fakeDecider{events: ch}

	var out bytes.Buffer
	_ = runWatch(context.Background(), []watchTarget{{name: "test", client: f}}, &out, strings.NewReader("1\n"))

	require.Equal(t, allowAllWindow, f.allowAllFor, "AllowAll duration")
	require.Empty(t, f.recorded(), "allow-all should not record a per-host decision")
}

func TestRunWatchRoutesAcrossVMs(t *testing.T) {
	mk := func(id, host, ip string) *fakeDecider {
		ch := make(chan netfilter.Event, 1)
		ch <- netfilter.Event{
			Type:    netfilter.EventPending,
			ID:      id,
			Pending: &netfilter.Pending{ID: id, Host: host, IP: ip, Port: "443"},
		}
		close(ch)
		return &fakeDecider{events: ch}
	}
	vmA := mk("a#1", "a.example", "203.0.113.1")
	vmB := mk("b#1", "b.example", "203.0.113.2")

	var out bytes.Buffer
	_ = runWatch(
		context.Background(),
		[]watchTarget{{name: "ALPHA", client: vmA}, {name: "BRAVO", client: vmB}},
		&out, strings.NewReader("a\na\n"),
	)

	gotA := vmA.recorded()
	require.Len(t, gotA, 1, "ALPHA decisions")
	require.Equal(t, "a#1", gotA[0].id, "ALPHA decision id")
	gotB := vmB.recorded()
	require.Len(t, gotB, 1, "BRAVO decisions")
	require.Equal(t, "b#1", gotB[0].id, "BRAVO decision id")
}

func TestRunWatchShowsAlreadyHeld(t *testing.T) {
	ch := make(chan netfilter.Event)
	close(ch) // no live events; loop ends immediately
	f := &fakeDecider{
		events:  ch,
		pending: []netfilter.Pending{{ID: "x#1", Host: "held.example", IP: "198.51.100.9", Port: "443", Waiters: 2}},
	}
	var out bytes.Buffer
	_ = runWatch(context.Background(), []watchTarget{{name: "test", client: f}}, &out, strings.NewReader(""))
	s := out.String()
	require.Contains(t, s, "held.example", "expected already-held line")
	require.Contains(t, s, "×2", "expected waiter count in output")
}
