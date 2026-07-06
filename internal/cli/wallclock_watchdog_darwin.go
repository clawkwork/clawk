//go:build darwin

package cli

import (
	"context"
	"log"
	"time"

	"github.com/clawkwork/clawk/machine"
)

// startWallclockWatchdog runs a 1-second-tick goroutine that detects
// host sleep events the macOS sleep notifier missed. If the wallclock
// jumps forward by more than `sleepDetectThreshold` between two ticks,
// the host almost certainly slept (the goroutine itself was suspended
// alongside everything else). On detection it logs the skip and, if
// no client session is currently attached, bounces the VM via
// machine.Pauseable so guest timers and network state recover faster.
//
// Why bounce only when idle: a Pause/Resume pair tears down any
// in-flight vsock connection, surfacing on the user's screen as
// "agent disconnected before exit frame: EOF". With the time-sync
// daemon (clawk-time-sync) handling clock drift on its own, the only
// remaining benefit of bouncing is reset-stale-network-state — and
// that's not worth nuking a live claude session.
//
// `mac-sleep-notifier` is reliable for short Sleep cycles but routinely
// drops Awake notifications across **macOS standby** (the deeper
// hibernate-to-disk state entered after ~3h on battery / ~24h on
// power). When that happens the daemon never paused the VM, the guest
// runs through the entire host-sleep window with stale clocks and dead
// NAT entries, and the user gets cryptic "vsock disconnected before
// exit / kex RST" errors on the first attempt to reconnect.
//
// The wallclock-tick approach is OS-event-free: we don't depend on any
// signal arriving, just on Go's own scheduler waking us up. As long as
// the daemon keeps running, we eventually notice the time jump and
// react.
//
// Returns when ctx is cancelled.
func startWallclockWatchdog(ctx context.Context, m machine.Machine, proxy *agentProxy, userPaused func() bool, logger *log.Logger) {
	pauser, ok := m.(machine.Pauseable)
	if !ok {
		// Backend can't pause/resume; we can still log time skips for
		// diagnostic purposes but can't take corrective action.
		logger.Printf("wallclock-watchdog: backend lacks Pauseable; will only log skips")
	}
	go runWallclockWatchdog(ctx, pauser, proxy, userPaused, logger)
}

// sleepDetectThreshold is how much the wallclock has to jump forward
// (between two 1-second ticks) before we declare "host slept." 5 s is
// generous enough to avoid false positives from a busy daemon yielding
// late and tight enough to catch any meaningful nap.
const sleepDetectThreshold = 5 * time.Second

// bounceCooldown prevents us from pausing/resuming the VM rapid-fire
// when the host enters a series of short sleeps (lid open / close /
// open). One bounce per 30 s is plenty.
const bounceCooldown = 30 * time.Second

func runWallclockWatchdog(ctx context.Context, pauser machine.Pauseable, proxy *agentProxy, userPaused func() bool, logger *log.Logger) {
	t := time.NewTicker(time.Second)
	defer t.Stop()

	prev := time.Now()
	var lastBounce time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			delta := now.Sub(prev)
			prev = now
			if delta < sleepDetectThreshold {
				continue
			}
			logger.Printf("wallclock-watchdog: detected %s skip (host probably slept; mac-sleep-notifier didn't fire)",
				delta.Round(time.Second))
			// Fire the time-sync push immediately so the guest's
			// wallclock catches up before any in-flight session
			// timestamp-validates anything. The 10s ticker would
			// otherwise leave the guest stale for up to 10s post-wake.
			triggerTimeSync()
			if pauser == nil {
				continue
			}
			if time.Since(lastBounce) < bounceCooldown {
				continue
			}
			// Skip bouncing while a vsock session is active. The
			// Pause/Resume pair kills in-flight connections — bad UX
			// for the user mid-claude. clawk-time-sync's next push
			// will resync the guest clock without disturbing the
			// session. We'll bounce on the next skip-detection only
			// if no one's attached.
			if proxy != nil && proxy.HasActiveSessions() {
				logger.Printf("wallclock-watchdog: skipping bounce (%d active session(s); time-sync will catch up)",
					atomicLoadInt64(&proxy.active))
				continue
			}
			// A user-paused VM stays paused: the bounce's resume half
			// would silently unfreeze what `clawk pause` deliberately
			// froze. The stale clock is repaired by the resume verb's
			// own time-sync push when the user comes back.
			if userPaused != nil && userPaused() {
				logger.Printf("wallclock-watchdog: skipping bounce (vm is user-paused)")
				continue
			}
			lastBounce = now

			// Bounce: pause then resume. The pause flushes any
			// stuck vCPU state; the resume restarts with fresh
			// scheduler/clock. Only fires when nobody's attached.
			bounceCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			if err := pauser.Pause(bounceCtx); err != nil {
				logger.Printf("wallclock-watchdog: pause failed: %v", err)
			} else if err := pauser.Resume(bounceCtx); err != nil {
				logger.Printf("wallclock-watchdog: resume failed: %v", err)
			} else {
				logger.Printf("wallclock-watchdog: bounced VM to recover guest state")
			}
			cancel()
		}
	}
}
