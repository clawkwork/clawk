package vzdctl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clawkwork/clawk/internal/netfilter"
	"github.com/stretchr/testify/require"
)

// testSocket returns a socket path in a short-lived temp dir. It avoids
// t.TempDir(), whose paths embed the test name and can exceed the ~104-char
// unix-domain socket limit on macOS (bind: invalid argument).
func testSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "vzdctl")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	return SocketPath(dir)
}

func TestRoundTrip(t *testing.T) {
	sock := testSocket(t)
	want := []netfilter.Denial{{
		Host: "api.blocked.dev", IP: "9.9.9.9", Port: "443", Count: 3,
		FirstSeen: time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC),
		LastSeen:  time.Date(2026, 6, 12, 10, 5, 0, 0, time.UTC),
	}}
	var reloaded int
	srv, err := Start(sock, Handlers{
		Denials: func() []netfilter.Denial { return want },
		Reload:  func() error { reloaded++; return nil },
	})
	require.NoError(t, err)
	t.Cleanup(func() { srv.Close() })

	c := NewClient(sock)
	ctx := context.Background()

	got, err := c.Denials(ctx)
	require.NoError(t, err, "Denials")
	require.True(t, len(got) == 1 && got[0] == want[0], "Denials = %+v, want %+v", got, want)

	require.NoError(t, c.Reload(ctx), "Reload")
	require.Equal(t, 1, reloaded, "reload handler ran %d times, want 1", reloaded)
}

func TestReloadErrorSurfaces(t *testing.T) {
	sock := testSocket(t)
	srv, err := Start(sock, Handlers{
		Denials: func() []netfilter.Denial { return nil },
		Reload:  func() error { return errors.New("bad policy entry") },
	})
	require.NoError(t, err)
	t.Cleanup(func() { srv.Close() })

	err = NewClient(sock).Reload(context.Background())
	require.Error(t, err, "expected reload error")
	require.True(t, strings.Contains(err.Error(), "bad policy entry"), "error should carry the server's reason, got: %v", err)
}

// TestInteractiveRoundTrip exercises the whole hold-and-prompt pipeline over
// the real control socket: a blocked Allow() is held, surfaces as a pending
// event on the SSE stream, and an allow decision posted back releases it.
func TestInteractiveRoundTrip(t *testing.T) {
	al, err := netfilter.NewAllowList(nil, nil, nil)
	require.NoError(t, err)
	gate := al.EnableInteractive()
	t.Cleanup(gate.Close)

	sock := testSocket(t)
	srv, err := Start(sock, Handlers{
		Denials: al.Denials,
		Reload:  func() error { return nil },
		Gate:    gate,
	})
	require.NoError(t, err)
	t.Cleanup(func() { srv.Close() })

	c := NewClient(sock)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events, err := c.Events(ctx)
	require.NoError(t, err, "Events")

	// The subscription is live now (the server registers it before
	// returning 200), so this connection is held rather than refused.
	allowErr := make(chan error, 1)
	go func() { allowErr <- al.Allow("203.0.113.77:443") }()

	var pending netfilter.Pending
	select {
	case ev := <-events:
		if ev.Type != netfilter.EventPending || ev.Pending == nil {
			t.Fatalf("first event = %+v, want pending", ev)
		}
		pending = *ev.Pending
	case <-time.After(2 * time.Second):
		t.Fatal("no pending event")
	}
	require.Equal(t, "203.0.113.77", pending.IP)

	// It should also show up in the pending snapshot.
	snap, err := c.Pending(ctx)
	require.NoError(t, err)
	require.Len(t, snap, 1, "Pending() want one hold")

	require.NoError(t, c.Decide(ctx, pending.ID, "allow", "session"), "Decide")

	select {
	case err := <-allowErr:
		require.NoError(t, err, "held Allow should have been permitted")
	case <-time.After(2 * time.Second):
		t.Fatal("Allow did not return after decision")
	}

	// Session grant: the IP is now allowed outright.
	require.NoError(t, al.Allow("203.0.113.77:443"), "granted IP should be allowed")
}

func TestAllowAllOverSocket(t *testing.T) {
	al, err := netfilter.NewAllowList(nil, nil, nil)
	require.NoError(t, err)
	gate := al.EnableInteractive()
	t.Cleanup(gate.Close)

	sock := testSocket(t)
	srv, err := Start(sock, Handlers{
		Denials: al.Denials,
		Reload:  func() error { return nil },
		Gate:    gate,
	})
	require.NoError(t, err)
	t.Cleanup(func() { srv.Close() })

	c := NewClient(sock)
	ctx := context.Background()
	require.NoError(t, c.AllowAll(ctx, time.Hour), "AllowAll")
	// A never-listed destination now passes outright.
	require.NoError(t, al.Allow("203.0.113.200:443"), "with allow-all active, expected pass")
}

func TestDecideUnknownIsNotFound(t *testing.T) {
	al, err := netfilter.NewAllowList(nil, nil, nil)
	require.NoError(t, err)
	gate := al.EnableInteractive()
	t.Cleanup(gate.Close)

	sock := testSocket(t)
	srv, err := Start(sock, Handlers{
		Denials: al.Denials,
		Reload:  func() error { return nil },
		Gate:    gate,
	})
	require.NoError(t, err)
	t.Cleanup(func() { srv.Close() })

	err = NewClient(sock).Decide(context.Background(), "ghost#1", "allow", "once")
	require.True(t, errors.Is(err, netfilter.ErrUnknownDecision), "Decide(unknown) = %v, want ErrUnknownDecision", err)
}

func TestInteractiveEndpointsDisabledWithoutGate(t *testing.T) {
	sock := testSocket(t)
	srv, err := Start(sock, Handlers{
		Denials: func() []netfilter.Denial { return nil },
		Reload:  func() error { return nil },
	})
	require.NoError(t, err)
	t.Cleanup(func() { srv.Close() })

	c := NewClient(sock)
	_, err = c.Pending(context.Background())
	require.Error(t, err, "Pending should fail when no gate is configured")
	_, err = c.Events(context.Background())
	require.Error(t, err, "Events should fail when no gate is configured")
}

func TestMissingSocketIsErrNotRunning(t *testing.T) {
	c := NewClient(filepath.Join(t.TempDir(), SocketName))
	require.True(t, errors.Is(c.Reload(context.Background()), ErrNotRunning), "Reload on missing socket want ErrNotRunning")
	_, err := c.Denials(context.Background())
	require.True(t, errors.Is(err, ErrNotRunning), "Denials on missing socket want ErrNotRunning")
}

func TestStartReplacesStaleSocket(t *testing.T) {
	sock := testSocket(t)
	first, err := Start(sock, Handlers{
		Denials: func() []netfilter.Denial { return nil },
		Reload:  func() error { return nil },
	})
	require.NoError(t, err)
	// Simulate a daemon that died without cleanup: the socket file stays.
	first.ln.Close()

	second, err := Start(sock, Handlers{
		Denials: func() []netfilter.Denial { return nil },
		Reload:  func() error { return nil },
	})
	require.NoError(t, err, "Start over stale socket")
	t.Cleanup(func() { second.Close() })

	require.NoError(t, NewClient(sock).Reload(context.Background()), "Reload via replacement server")
}

// TestLifecycleRoundTrip drives the lifecycle endpoints end to end: the
// state snapshot decodes, and each verb reaches its handler.
func TestLifecycleRoundTrip(t *testing.T) {
	sock := testSocket(t)
	var paused, resumed, suspended int
	srv, err := Start(sock, Handlers{
		Denials: func() []netfilter.Denial { return nil },
		Reload:  func() error { return nil },
		Lifecycle: &LifecycleHandlers{
			State: func() LifecycleState {
				return LifecycleState{State: LifecyclePaused, Restored: true}
			},
			Pause:   func() error { paused++; return nil },
			Resume:  func() error { resumed++; return nil },
			Suspend: func() error { suspended++; return nil },
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { srv.Close() })

	c := NewClient(sock)
	ctx := context.Background()

	ls, err := c.Lifecycle(ctx)
	require.NoError(t, err, "Lifecycle")
	require.Equal(t, LifecycleState{State: LifecyclePaused, Restored: true}, ls)

	require.NoError(t, c.Pause(ctx), "Pause")
	require.NoError(t, c.Resume(ctx), "Resume")
	require.NoError(t, c.Suspend(ctx), "Suspend")
	require.Equal(t, []int{1, 1, 1}, []int{paused, resumed, suspended},
		"each verb should reach its handler exactly once")
}

// TestLifecycleUnsupported: a daemon without lifecycle handlers answers 404,
// which the client maps to ErrLifecycleUnsupported so callers can suggest a
// restart instead of surfacing a raw HTTP status.
func TestLifecycleUnsupported(t *testing.T) {
	sock := testSocket(t)
	srv, err := Start(sock, Handlers{
		Denials: func() []netfilter.Denial { return nil },
		Reload:  func() error { return nil },
	})
	require.NoError(t, err)
	t.Cleanup(func() { srv.Close() })

	c := NewClient(sock)
	ctx := context.Background()
	_, err = c.Lifecycle(ctx)
	require.ErrorIs(t, err, ErrLifecycleUnsupported, "Lifecycle without handlers")
	require.ErrorIs(t, c.Pause(ctx), ErrLifecycleUnsupported, "Pause without handlers")
}

// TestLifecycleVerbErrorSurfaces: a failing handler's reason reaches the
// client verbatim.
func TestLifecycleVerbErrorSurfaces(t *testing.T) {
	sock := testSocket(t)
	srv, err := Start(sock, Handlers{
		Denials: func() []netfilter.Denial { return nil },
		Reload:  func() error { return nil },
		Lifecycle: &LifecycleHandlers{
			State:   func() LifecycleState { return LifecycleState{State: LifecycleRunning} },
			Suspend: func() error { return errors.New("backend cannot suspend") },
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { srv.Close() })

	c := NewClient(sock)
	err = c.Suspend(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "backend cannot suspend")
	// Pause has no handler wired — that specific verb is unsupported.
	require.ErrorIs(t, c.Pause(context.Background()), ErrLifecycleUnsupported)
}
