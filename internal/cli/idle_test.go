package cli

import (
	"testing"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/template"
	"github.com/stretchr/testify/require"
)

// quietSample is a fully-idle observation at t: no sessions, guest
// reporting, no load, unchanged net counters.
func quietSample(now time.Time, netBytes uint64) idleSample {
	return idleSample{
		now:              now,
		statsOK:          true,
		hasGuestActivity: true,
		netIOBytes:       netBytes,
	}
}

func TestIdleTrackerExpiresAfterQuietTimeout(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	tr := newIdleTracker(10*time.Minute, start)

	var expired bool
	for i := 1; i <= 10; i++ {
		_, expired = tr.observe(quietSample(start.Add(time.Duration(i)*time.Minute), 500))
	}
	require.True(t, expired, "10 quiet minutes must expire a 10m timeout")
}

func TestIdleTrackerSessionsKeepAlive(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	tr := newIdleTracker(5*time.Minute, start)

	for i := 1; i <= 30; i++ {
		s := quietSample(start.Add(time.Duration(i)*time.Minute), 0)
		s.sessionsActive = true
		_, expired := tr.observe(s)
		require.False(t, expired, "an attached session must hold the VM open indefinitely")
	}
}

func TestIdleTrackerStatsFailureCountsAsActive(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	tr := newIdleTracker(5*time.Minute, start)

	for i := 1; i <= 30; i++ {
		s := idleSample{now: start.Add(time.Duration(i) * time.Minute)} // statsOK=false
		_, expired := tr.observe(s)
		require.False(t, expired, "an unreachable guest must never be parked")
	}
}

func TestIdleTrackerLegacyAgentFallsBackToSessions(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	tr := newIdleTracker(5*time.Minute, start)

	var expired bool
	for i := 1; i <= 5; i++ {
		s := idleSample{
			now:     start.Add(time.Duration(i) * time.Minute),
			statsOK: true, // reachable but predates the activity fields
		}
		_, expired = tr.observe(s)
	}
	require.True(t, expired, "legacy guest agent: session silence alone must expire the window")
}

func TestIdleTrackerGuestLoadKeepsAlive(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	tr := newIdleTracker(5*time.Minute, start)

	for i := 1; i <= 30; i++ {
		s := quietSample(start.Add(time.Duration(i)*time.Minute), 0)
		s.loadCenti = idleBusyLoadCenti // a detached build keeps load up
		_, expired := tr.observe(s)
		require.False(t, expired, "busy guest load must hold the VM open")
	}
}

func TestIdleTrackerNetTraffic(t *testing.T) {
	start := time.Unix(1_000_000, 0)

	t.Run("real traffic keeps alive", func(t *testing.T) {
		tr := newIdleTracker(5*time.Minute, start)
		net := uint64(0)
		for i := 1; i <= 30; i++ {
			net += idleBusyNetBytes // e.g. someone browsing a forwarded dev server
			_, expired := tr.observe(quietSample(start.Add(time.Duration(i)*time.Minute), net))
			require.False(t, expired, "moving net counters must hold the VM open")
		}
	})

	t.Run("housekeeping drip still expires", func(t *testing.T) {
		tr := newIdleTracker(5*time.Minute, start)
		net := uint64(0)
		var expired bool
		for i := 1; i <= 6; i++ {
			net += 512 // DHCP renewals etc. — below the busy threshold
			_, expired = tr.observe(quietSample(start.Add(time.Duration(i)*time.Minute), net))
		}
		require.True(t, expired, "sub-threshold drips must not count as activity")
	})

	t.Run("counter reset is not traffic", func(t *testing.T) {
		tr := newIdleTracker(5*time.Minute, start)
		_, _ = tr.observe(quietSample(start.Add(1*time.Minute), 10_000_000))
		var expired bool
		for i := 2; i <= 6; i++ {
			// Interface bounced: counters restarted near zero and stay flat.
			_, expired = tr.observe(quietSample(start.Add(time.Duration(i)*time.Minute), 100))
		}
		require.True(t, expired, "a counter that went backwards is a reset, not activity")
	})
}

func TestIdleTrackerShortSessionBetweenTicks(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	tr := newIdleTracker(5*time.Minute, start)

	// Four quiet minutes...
	for i := 1; i <= 4; i++ {
		_, expired := tr.observe(quietSample(start.Add(time.Duration(i)*time.Minute), 0))
		require.False(t, expired)
	}
	// ...then a `clawk run` that started and finished entirely between
	// ticks. The proxy's lastSession stamp is the only trace of it.
	s := quietSample(start.Add(5*time.Minute), 0)
	s.lastSessionNano = start.Add(4*time.Minute + 30*time.Second).UnixNano()
	_, expired := tr.observe(s)
	require.False(t, expired, "a just-finished session must restart the idle window")

	// The window restarts from the session end, not from the tick.
	idleFor, _ := tr.observe(quietSample(start.Add(6*time.Minute), 0))
	require.Equal(t, 90*time.Second, idleFor)
}

func TestIdleTrackerReset(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	tr := newIdleTracker(2*time.Minute, start)
	_, expired := tr.observe(quietSample(start.Add(2*time.Minute), 0))
	require.True(t, expired)

	tr.reset(start.Add(2 * time.Minute))
	_, expired = tr.observe(quietSample(start.Add(3*time.Minute), 0))
	require.False(t, expired, "reset must restart the idle window")
}

func TestEffectiveIdleTimeout(t *testing.T) {
	require.Equal(t, defaultIdleTimeout, effectiveIdleTimeout(&config.Sandbox{}))
	require.Equal(t, time.Duration(0), effectiveIdleTimeout(&config.Sandbox{IdleTimeoutSec: -1}))
	require.Equal(t, 45*time.Minute, effectiveIdleTimeout(&config.Sandbox{IdleTimeoutSec: 2700}))
}

func TestResolveIdleTimeout(t *testing.T) {
	ws := func(fileSec int64, repoSecs ...int64) *template.Workspace {
		w := &template.Workspace{File: &template.Template{IdleTimeoutSec: fileSec}}
		for _, s := range repoSecs {
			w.Repos = append(w.Repos, template.Repo{Clawkfile: &template.Template{IdleTimeoutSec: s}})
		}
		return w
	}
	require.Equal(t, int64(0), resolveIdleTimeout(ws(0)), "unset everywhere stays unset")
	require.Equal(t, int64(1800), resolveIdleTimeout(ws(1800)))
	require.Equal(t, int64(3600), resolveIdleTimeout(ws(1800, 3600)), "longest duration wins")
	require.Equal(t, int64(-1), resolveIdleTimeout(ws(1800, -1, 3600)), "off wins over durations")
	require.Equal(t, int64(-1), resolveIdleTimeout(ws(-1, 600)), "workspace off wins")
	// A repo without a Clawkfile contributes nothing.
	w := ws(900)
	w.Repos = append(w.Repos, template.Repo{})
	require.Equal(t, int64(900), resolveIdleTimeout(w))
}

func TestStopReasonDisplay(t *testing.T) {
	idle := &config.Sandbox{StopReason: config.StopReasonIdle}
	require.Equal(t, "stopped (idle)", displayVMState("stopped", idle))
	require.Equal(t, "running", displayVMState("running", idle),
		"a stale reason must not annotate a running VM")
	require.Equal(t, "stopped", displayVMState("stopped", &config.Sandbox{}))
	require.Equal(t, "idle", stopReasonFor("stopped", idle))
	require.Equal(t, "", stopReasonFor("running", idle))
}
