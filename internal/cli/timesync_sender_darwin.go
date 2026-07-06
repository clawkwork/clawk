//go:build darwin

package cli

import (
	"context"
	"encoding/binary"
	"errors"
	"log"
	"net"
	"time"

	"github.com/clawkwork/clawk/machine"
	sleepnotifier "github.com/prashantgupta24/mac-sleep-notifier/notifier"
)

// startTimeSyncSender pushes the host's wallclock to the guest every
// `timeSyncInterval` via vsock port 1025. Pairs with the
// clawk-time-sync daemon inside the guest (see
// internal/agentembed/time_sync_main.go.in).
//
// Why a host→guest push instead of guest→host pull: the existing
// vzd plumbing already supports host-initiated dials via
// machine.Machine.VSock; the guest-listens-on-vsock-and-host-dials
// pattern is what `agent-proxy` already uses for the PTY agent. Adding
// a second port keeps protocols separate.
//
// Cures the post-Mac-sleep "JWT/TLS expired" / "git auth refused" /
// "claude API rejects timestamp" failure class without depending on
// chronyd/NTP (which clawk's network ACL typically blocks anyway).
const (
	// timeSyncInterval is how often we push. 10 s catches sleep wakes
	// almost immediately while keeping the dial overhead negligible.
	timeSyncInterval = 10 * time.Second

	// timeSyncDialTimeout caps how long we wait for the guest to
	// accept the dial. The agent's vsock listener is in the same
	// guest, so this should always be sub-second; long timeouts only
	// happen during boot or when the guest is wedged.
	timeSyncDialTimeout = 5 * time.Second

	// timeSyncPort matches clawk-time-sync.service's --port argument.
	// Keep in sync with internal/agentembed/time_sync_main.go.in's
	// vsockPort constant.
	timeSyncPort = 1025
)

// timeSyncTrigger is closed-then-reused via channel-of-channels — but
// for our purposes a buffered chan struct{} is enough. Producers
// (mac-sleep-notifier callback, wallclock-watchdog) send a value;
// the sender goroutine drains it as a "push now" signal.
//
// Buffer of 1 means rapid back-to-back triggers coalesce into a
// single push instead of stacking up. We don't need queue semantics —
// "push the latest time" is idempotent.
var timeSyncPushNow = make(chan struct{}, 1)

// The daemon's lifecycle resume handler (shared code in daemon.go) can't
// name triggerTimeSync directly — it's darwin-only — so it calls through
// the onVMResume hook, pointed here at init.
func init() { onVMResume = triggerTimeSync }

// triggerTimeSync signals the sender goroutine to push immediately.
// Non-blocking; if a push is already pending, this is a no-op.
// Exported for the wallclock-watchdog and any future caller.
func triggerTimeSync() {
	select {
	case timeSyncPushNow <- struct{}{}:
	default:
	}
}

// startTimeSyncSender spawns a goroutine that pushes the host clock
// to the guest. Returns immediately. The goroutine ends when ctx is
// canceled.
//
// Three triggers fire a push:
//   - 10s ticker (catches macOS standby that drops Awake events)
//   - mac-sleep-notifier Awake callback (catches normal wake events
//     immediately, sub-second after the OS unfreezes)
//   - external trigger via triggerTimeSync() — the wallclock-watchdog
//     calls this when it detects a skip, so the push happens before
//     the watchdog's next tick
func startTimeSyncSender(ctx context.Context, m machine.Machine, logger *log.Logger) {
	go runTimeSyncSender(ctx, m, logger)
	go runSleepNotifierBridge(ctx, logger)
}

// runSleepNotifierBridge subscribes to mac-sleep-notifier and fires
// triggerTimeSync on Awake events. Separate goroutine so a hung
// notifier (the standby case) doesn't block the interval ticker.
// We keep the interval ticker as a fallback for cases where the
// notifier silently drops events (long-standby).
func runSleepNotifierBridge(ctx context.Context, logger *log.Logger) {
	notifier := sleepnotifier.GetInstance()
	ch := notifier.Start()
	defer notifier.Quit()
	for {
		select {
		case <-ctx.Done():
			return
		case activity, ok := <-ch:
			if !ok {
				return
			}
			if activity.Type == sleepnotifier.Awake {
				logger.Printf("time-sync: Awake notification received; triggering immediate push")
				triggerTimeSync()
			}
		}
	}
}

func runTimeSyncSender(ctx context.Context, m machine.Machine, logger *log.Logger) {
	t := time.NewTicker(timeSyncInterval)
	defer t.Stop()

	// We log the FIRST connect failure (so the operator knows whether
	// the guest daemon is up) and then go quiet — without rate
	// limiting, a wedged guest would spam the daemon log every 10 s.
	// On the first SUCCESSFUL push we log "ok" so the operator can
	// confirm the path is healthy.
	logged := struct {
		firstFail bool
		firstOK   bool
	}{}

	push := func() {
		dialCtx, cancel := context.WithTimeout(ctx, timeSyncDialTimeout)
		defer cancel()
		conn, err := m.VSock(dialCtx, timeSyncPort)
		if err != nil {
			if !logged.firstFail {
				logger.Printf("time-sync: first push failed: %v "+
					"(probably guest daemon not yet listening; will retry silently)", err)
				logged.firstFail = true
			}
			return
		}
		defer conn.Close()

		// Reset the deadline; we don't need ctx-canceled writes
		// anymore — the dial succeeded, write 8 bytes and close.
		_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(time.Now().UnixNano()))
		if _, err := conn.Write(buf[:]); err != nil {
			if !errors.Is(err, net.ErrClosed) && !logged.firstFail {
				logger.Printf("time-sync: push write failed: %v", err)
				logged.firstFail = true
			}
			return
		}
		if !logged.firstOK {
			logger.Printf("time-sync: push pipeline healthy (port %d)", timeSyncPort)
			logged.firstOK = true
		}
	}

	// First push runs immediately so the guest gets the right time
	// before any user-side timestamp validation runs against it.
	push()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			push()
		case <-timeSyncPushNow:
			// External trigger (Awake event, wallclock-watchdog, etc.)
			// asked for an out-of-band push. Fire immediately and
			// reset the ticker so we don't double-push within a few
			// hundred ms. ticker.Reset is cheaper than re-creating.
			push()
			t.Reset(timeSyncInterval)
		}
	}
}
