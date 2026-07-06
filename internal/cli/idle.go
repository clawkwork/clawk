package cli

// Idle-stop decision logic, kept free of any VM backend so it compiles and
// tests everywhere. The darwin daemon (idle_watchdog_darwin.go) feeds it
// one idleSample per tick and stops the VM when the tracker says the
// sandbox has been idle past its timeout.
//
// "Idle" is deliberately conjunctive — every signal has to be quiet:
//   - no client session bridged through the agent proxy (clawk claude /
//     shell / run all hold one for their lifetime);
//   - guest load below a busy threshold, so a build or test run left
//     going after the user detached keeps the VM alive;
//   - guest network counters not moving, so a forwarded dev server that's
//     being browsed (traffic bypasses the agent proxy) keeps the VM alive.
//
// When the guest can't be sampled at all the tracker counts the tick as
// active: a booting guest hasn't started its agent yet, and stopping a VM
// we can't see is exactly the wrong reflex.

import (
	"time"

	"github.com/clawkwork/clawk/internal/config"
)

const (
	// idleCheckInterval is how often the daemon samples the sandbox. A
	// minute is coarse enough to cost nothing and fine enough that the
	// effective park time overshoots the configured timeout by at most
	// one tick.
	idleCheckInterval = time.Minute

	// idleBusyLoadCenti is the guest load1 (× 100) at or above which the
	// guest counts as working. An idle guest sits well under 0.10; a
	// single busy process drives load1 toward 1.0 within a minute, and
	// load1's slow decay conveniently extends the active window past
	// short dips between build steps.
	idleBusyLoadCenti = 25

	// idleBusyNetBytes is how much the guest's rx+tx counters must move
	// between two samples to count as traffic. High enough to ignore
	// housekeeping drips (DHCP renewals, a stray NTP query), low enough
	// that a single browser page load against a forwarded dev server
	// clears it comfortably.
	idleBusyNetBytes = 32 * 1024
)

// idleSample is one per-tick observation of the sandbox.
type idleSample struct {
	now time.Time

	// sessionsActive is whether the agent proxy has a bridged client
	// session open right now.
	sessionsActive bool
	// lastSessionNano is the UnixNano of the most recent session start or
	// end the proxy observed (0 = none since boot). It catches sessions
	// that began and ended entirely between two ticks.
	lastSessionNano int64

	// statsOK is whether the guest agent answered the stats poll this
	// tick. False = can't see the guest = treat as active.
	statsOK bool
	// hasGuestActivity is whether the guest agent is new enough to report
	// load and net counters. False (legacy agent) degrades the tracker to
	// session-signals only.
	hasGuestActivity bool
	loadCenti        uint64
	netIOBytes       uint64
}

// idleTracker accumulates samples and decides when the timeout expired.
type idleTracker struct {
	timeout    time.Duration
	lastActive time.Time

	prevNet     uint64
	havePrevNet bool
}

func newIdleTracker(timeout time.Duration, now time.Time) *idleTracker {
	return &idleTracker{timeout: timeout, lastActive: now}
}

// observe folds one sample and reports how long the sandbox has been idle
// and whether that exceeds the timeout. The caller stops the VM on
// expired; if the stop is then aborted (a session raced in), it calls
// reset so the next window starts fresh.
func (t *idleTracker) observe(s idleSample) (idleFor time.Duration, expired bool) {
	active := s.sessionsActive || !s.statsOK

	if s.statsOK && s.hasGuestActivity {
		if s.loadCenti >= idleBusyLoadCenti {
			active = true
		}
		if t.havePrevNet && s.netIOBytes >= t.prevNet &&
			s.netIOBytes-t.prevNet >= idleBusyNetBytes {
			active = true
		}
		// A counter that went backwards is an interface bounce resetting
		// its stats, not traffic — just re-baseline.
		t.prevNet = s.netIOBytes
		t.havePrevNet = true
	}

	if s.lastSessionNano > 0 {
		if end := time.Unix(0, s.lastSessionNano); end.After(t.lastActive) {
			t.lastActive = end
		}
	}
	if active {
		t.lastActive = s.now
	}

	idleFor = s.now.Sub(t.lastActive)
	return idleFor, idleFor >= t.timeout
}

// reset restarts the idle window, used after an aborted stop.
func (t *idleTracker) reset(now time.Time) { t.lastActive = now }

// stopReasonFor returns the recorded stop reason when it applies to the
// live state — a reason only describes a stop, so a running VM (or a
// record that predates the last stop) yields "".
func stopReasonFor(live string, sb *config.Sandbox) string {
	if live == string(config.VMStateStopped) {
		return string(sb.StopReason)
	}
	return ""
}

// displayVMState annotates a live "stopped" with the recorded idle-stop so
// list/status distinguish "you stopped this" from "clawk parked this".
func displayVMState(live string, sb *config.Sandbox) string {
	if reason := stopReasonFor(live, sb); reason != "" {
		return live + " (" + reason + ")"
	}
	return live
}
