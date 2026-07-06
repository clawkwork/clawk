//go:build darwin

package cli

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/machine"
	"github.com/clawkwork/clawk/machine/vz"
)

// startIdleWatchdog arms the daemon's idle-stop: once the sandbox has had
// no attached session and a quiescent guest for its idle timeout (see
// internal/cli/idle.go for what counts as quiet), the daemon stops the VM
// to hand its memory back to the host. The stop is a park, not a `clawk
// down`: DesiredState stays running, the record is annotated with
// StopReasonIdle, and any attach/run verb boots the VM back.
//
// Darwin-only by construction, not oversight: on firecracker the client
// dials the guest's hybrid vsock directly, so a daemon-side watchdog
// can't see sessions and would park a VM mid-conversation.
func startIdleWatchdog(ctx context.Context, sb *config.Sandbox, m machine.Machine, proxy *agentProxy, logger *log.Logger) {
	timeout := effectiveIdleTimeout(sb)
	if timeout <= 0 {
		logger.Printf("idle-watchdog: disabled (idle_timeout off)")
		return
	}
	if proxy == nil {
		// Without the agent proxy we can't see client sessions, and
		// stopping a VM someone may be using is worse than the memory.
		logger.Printf("idle-watchdog: disabled (no agent proxy; sessions would be invisible)")
		return
	}
	logger.Printf("idle-watchdog: armed (timeout %s)", timeout)
	go runIdleWatchdog(ctx, sb.Name, m, proxy, timeout, logger)
}

func runIdleWatchdog(ctx context.Context, name string, m machine.Machine, proxy *agentProxy, timeout time.Duration, logger *log.Logger) {
	tracker := newIdleTracker(timeout, time.Now())
	t := time.NewTicker(idleCheckInterval)
	defer t.Stop()

	loggedStatsFail := false
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s := idleSample{
				now:             now,
				sessionsActive:  proxy.HasActiveSessions(),
				lastSessionNano: proxy.LastSessionNano(),
			}
			statsCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			stats, err := vz.FetchGuestStats(statsCtx, m)
			cancel()
			switch {
			case err != nil:
				// Log the first failure, then go quiet: a guest without
				// the reporter would otherwise fill the log at one line
				// per tick. The tracker treats the miss as activity.
				if !loggedStatsFail {
					logger.Printf("idle-watchdog: guest stats poll failed (treating as active, retrying quietly): %v", err)
					loggedStatsFail = true
				}
			default:
				loggedStatsFail = false
				s.statsOK = true
				s.hasGuestActivity = stats.HasActivity
				s.loadCenti = stats.Load1Centi
				s.netIOBytes = stats.NetIOBytes
			}

			idleFor, expired := tracker.observe(s)
			if !expired {
				continue
			}
			if !attemptIdleStop(name, proxy, logger, idleFor, timeout) {
				tracker.reset(time.Now())
				continue
			}
			return
		}
	}
}

// attemptIdleStop fences out new sessions, re-verifies the sandbox is
// really unused, records the park in the sandbox record, and triggers the
// daemon's own graceful-shutdown path. Returns false (with the proxy
// accepting again) when the stop must be aborted — a session raced in, or
// the sandbox is in a state that shouldn't be parked.
func attemptIdleStop(name string, proxy *agentProxy, logger *log.Logger, idleFor, timeout time.Duration) bool {
	fresh, err := store.Load(name)
	if err != nil {
		logger.Printf("idle-watchdog: cannot reload sandbox record, not stopping: %v", err)
		return false
	}
	// A create-pending VM was deliberately left running for the user to
	// investigate a failed `on create` — parking it would quietly destroy
	// the evidence mid-debugging.
	if fresh.CreatePending {
		return false
	}
	// DesiredState=stopped means a `clawk down` is already converging;
	// let the explicit verb own the stop (and the record).
	if fresh.DesiredState == config.VMStateStopped {
		return false
	}

	// Fence: after SuspendAccept returns, no new session can arrive and
	// the active count is authoritative. A session that snuck in between
	// our last sample and the fence aborts the stop.
	proxy.SuspendAccept()
	if proxy.HasActiveSessions() {
		logger.Printf("idle-watchdog: session attached during stop attempt; aborting")
		if err := proxy.ResumeAccept(); err != nil {
			logger.Printf("idle-watchdog: FAILED to resume agent socket (%v) — attach is degraded until next boot", err)
		}
		return false
	}

	// Record the park before the stop so a client that dialed mid-shutdown
	// can read the reason and boot the VM back instead of surfacing an
	// error. DesiredState is deliberately untouched — the user still wants
	// this sandbox available; it's just not resident right now.
	fresh.VMState = config.VMStateStopped
	fresh.StopReason = config.StopReasonIdle
	if err := store.Save(fresh); err != nil {
		logger.Printf("idle-watchdog: recording idle stop: %v", err)
	}

	logger.Printf("idle-watchdog: idle for %s (timeout %s) — stopping VM to reclaim memory; any attach boots it back",
		idleFor.Round(time.Second), timeout)
	// Reuse the daemon's one shutdown path (waitAndShutdown's SIGTERM
	// handler) instead of growing a second VM-stop sequence.
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		logger.Printf("idle-watchdog: self-SIGTERM failed: %v", err)
		fresh.VMState = config.VMStateRunning
		fresh.StopReason = ""
		if err := store.Save(fresh); err != nil {
			logger.Printf("idle-watchdog: reverting record: %v", err)
		}
		if err := proxy.ResumeAccept(); err != nil {
			logger.Printf("idle-watchdog: FAILED to resume agent socket (%v) — attach is degraded until next boot", err)
		}
		return false
	}
	// The CLI's down path guarantees the daemon dies — it escalates to
	// SIGKILL from outside after a grace period. This self-initiated park
	// has no outside enforcer, and a teardown that wedges (a hypervisor
	// call that never returns, a proxy close that blocks) leaves a zombie
	// daemon whose live pidfile makes status report "running" and attach
	// dial a dead socket forever. Arm the missing enforcer: if the
	// graceful path hasn't exited the process in time, remove the pidfile
	// ourselves (os.Exit skips runVzd's deferred cleanup) and die hard.
	// The park is already recorded, so a hard exit is indistinguishable
	// from a graceful one to every reader of the record; when shutdown
	// completes normally the process exits and this timer dies with it.
	time.AfterFunc(idleStopExitGrace, func() {
		logger.Printf("idle-watchdog: shutdown still running after %s; forcing exit", idleStopExitGrace)
		if err := os.Remove(filepath.Join(store.VMDir(name), "vz.pid")); err != nil && !os.IsNotExist(err) {
			logger.Printf("idle-watchdog: removing pidfile: %v", err)
		}
		os.Exit(0)
	})
	return true
}

// idleStopExitGrace bounds how long a self-initiated park may spend in
// graceful shutdown before the daemon force-exits. Generous next to the
// stop path's own internal budgets (10s graceful + 5s forced VM stop) so
// it only ever fires on a genuine wedge, never a slow clean stop.
const idleStopExitGrace = 30 * time.Second
