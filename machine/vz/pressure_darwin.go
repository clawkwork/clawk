//go:build darwin

package vz

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	codevz "github.com/Code-Hex/vz/v3"
	"github.com/clawkwork/clawk/machine/internal/debug"
	"golang.org/x/sys/unix"
)

// EVFILT_MEMORYSTATUS and its NOTE_MEMORYSTATUS_PRESSURE_* flags are part of
// the XNU kqueue ABI (<sys/event.h>) but golang.org/x/sys/unix does not export
// them, so we define them here. Registering this filter with ident 0 monitors
// *system-wide* memory pressure — the same kernel signal that backs
// DISPATCH_SOURCE_TYPE_MEMORYPRESSURE and drives jetsam.
const (
	evfiltMemorystatus = -14 // EVFILT_MEMORYSTATUS

	noteMemorystatusPressureNormal   = 0x00000001
	noteMemorystatusPressureWarn     = 0x00000002
	noteMemorystatusPressureCritical = 0x00000004

	pressureFflags = noteMemorystatusPressureNormal |
		noteMemorystatusPressureWarn |
		noteMemorystatusPressureCritical
)

// watchMemoryPressure blocks until ctx is cancelled, calling onLevel whenever
// the host's memory-pressure level changes. It owns a private kqueue for the
// duration and closes it on return. onLevel runs on this goroutine, so a slow
// callback delays observing the next transition; ours only posts a balloon
// target onto the VM's dispatch queue, which is cheap.
//
// Returns nil on a clean ctx cancellation; a non-nil error means the kqueue
// itself failed and pressure can no longer be observed.
func watchMemoryPressure(ctx context.Context, onLevel func(pressureLevel)) error {
	kq, err := unix.Kqueue()
	if err != nil {
		return fmt.Errorf("kqueue: %w", err)
	}
	defer unix.Close(kq)

	change := unix.Kevent_t{
		Ident:  0, // system-wide pressure
		Filter: evfiltMemorystatus,
		Flags:  unix.EV_ADD | unix.EV_CLEAR,
		Fflags: pressureFflags,
	}
	if _, err := unix.Kevent(kq, []unix.Kevent_t{change}, nil, nil); err != nil {
		return fmt.Errorf("register EVFILT_MEMORYSTATUS: %w", err)
	}

	// A one-second deadline lets us re-check ctx promptly without busy
	// spinning: the kernel wakes us on an actual pressure transition or this
	// timeout, whichever is first.
	timeout := unix.Timespec{Sec: 1}
	events := make([]unix.Kevent_t, 1)
	for {
		if ctx.Err() != nil {
			return nil
		}
		n, err := unix.Kevent(kq, nil, events, &timeout)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return fmt.Errorf("kevent wait: %w", err)
		}
		for _, ev := range events[:n] {
			if ev.Filter == evfiltMemorystatus {
				onLevel(levelFromFflags(ev.Fflags))
			}
		}
	}
}

// levelFromFflags maps the returned event flags to a level, picking the most
// severe bit set.
func levelFromFflags(f uint32) pressureLevel {
	switch {
	case f&noteMemorystatusPressureCritical != 0:
		return pressureCritical
	case f&noteMemorystatusPressureWarn != 0:
		return pressureWarn
	default:
		return pressureNormal
	}
}

// Balloon controller tuning.
const (
	// memPollInterval is how often the host dials the guest agent for a
	// memory snapshot. Short enough to react to a burst before the guest
	// thrashes its OOM killer (DEFLATE_ON_OOM is the stopgap in between),
	// long enough that the dial overhead is noise.
	memPollInterval = 5 * time.Second

	// memReevalInterval re-runs the target calculation even without new
	// input, so a gentle reclaim steps down over time and a guest agent that
	// went silent is noticed (see memReportStaleAfter).
	memReevalInterval = 5 * time.Second

	// memDialTimeout/memReadTimeout bound a single poll. The agent listens
	// in the same guest, so a healthy poll is sub-second; these only bite
	// during boot or a wedged guest.
	memDialTimeout = 3 * time.Second
	memReadTimeout = 2 * time.Second

	// memReportStaleAfter is how long a guest report stays trusted. Past
	// this — agent crashed, never started, or an image without the reporter
	// — the controller stops constraining the guest and hands it the full
	// ceiling (host pressure can still reclaim). This is what keeps old
	// images and a dead agent from being pinned at the baseline.
	memReportStaleAfter = 20 * time.Second
)

// balloonCeilingBytes is the guest's boot/maximum memory size in bytes: the
// ceiling it can burst to. Falls back to the baseline when no MemoryMaxMiB is
// configured (no burst headroom).
func (v *vm) balloonCeilingBytes() uint64 {
	mib := v.spec.MemoryMiB
	if v.spec.MemoryMaxMiB > mib {
		mib = v.spec.MemoryMaxMiB
	}
	return mib * 1024 * 1024
}

// startBalloonController wires three goroutines to this VM's memory balloon:
// a host memory-pressure watcher, a guest-memory poller, and a controller
// that merges both into a single balloon target. Together they keep an idle
// guest near its baseline (reclaiming RAM for the host), let it burst toward
// its ceiling on demand, and force reclaim under host pressure — the last of
// which is what stops the host from faulting an unkillable process like
// launchd and panicking the whole machine.
//
// Must be called with v.mu held and v.machine running. Opt out with
// CLAWK_MEMORY_PRESSURE=0.
func (v *vm) startBalloonController() {
	if os.Getenv("CLAWK_MEMORY_PRESSURE") == "0" {
		return
	}
	if v.balloon == nil {
		for _, d := range v.machine.MemoryBalloonDevices() {
			if b := codevz.AsVirtioTraditionalMemoryBalloonDevice(d); b != nil {
				v.balloon = b
				break
			}
		}
	}
	if v.balloon == nil {
		debug.Log("vz", "no memory balloon device; balloon controller disabled",
			"id", v.spec.ID)
		return
	}

	ceiling := v.balloonCeilingBytes()
	baseline := v.spec.MemoryMiB * 1024 * 1024
	if baseline > ceiling {
		baseline = ceiling
	}
	// The balloon boots fully deflated: the guest already sees the ceiling.
	v.balloonTargetBytes = ceiling

	ctx, cancel := context.WithCancel(context.Background())
	v.pressureCancel = cancel
	dev := v.balloon
	id := v.spec.ID

	levels := make(chan pressureLevel, 1)
	reports := make(chan memReport, 1)

	go func() {
		if err := watchMemoryPressure(ctx, func(l pressureLevel) { sendLatestLevel(levels, l) }); err != nil {
			debug.Log("vz", "memory pressure watch ended", "id", id, "err", err)
		}
	}()
	go v.pollGuestMemory(ctx, reports)
	go v.runBalloonController(ctx, dev, baseline, ceiling, levels, reports)
}

// runBalloonController is the single owner of the balloon target: it is the
// only caller of SetTargetVirtualMachineMemorySize, so the change occurs on
// one goroutine with no further locking. It folds the latest host-pressure
// level and the latest guest memory report into one target via
// mergedBalloonTarget, and a ticker re-evaluates so reclaim steps down over
// time and a stale guest report is noticed.
//
// dev is captured by the caller; the framework retains it until teardown, so
// a target set during shutdown is a harmless no-op on an already-stopped VM.
func (v *vm) runBalloonController(ctx context.Context, dev *codevz.VirtioTraditionalMemoryBalloonDevice, baseline, ceiling uint64, levels chan pressureLevel, reports chan memReport) {
	cur := ceiling // device boots fully deflated (guest sees the ceiling)
	level := pressureNormal
	var rep memReport
	var lastReport time.Time

	debug.Log("vz", "balloon controller started", "id", v.spec.ID,
		"baseline_mib", baseline/(1024*1024), "ceiling_mib", ceiling/(1024*1024),
		"poll_interval", memPollInterval.String())

	t := time.NewTicker(memReevalInterval)
	defer t.Stop()

	apply := func(trigger string) {
		fresh := !lastReport.IsZero() && time.Since(lastReport) <= memReportStaleAfter
		var target uint64
		if fresh {
			// Guest reported, so its balloon driver is up: manage between
			// baseline and ceiling on demand.
			target = mergedBalloonTarget(level, cur, baseline, ceiling, rep)
		} else {
			// No fresh report — guest still booting, image has no reporter, or
			// the agent died. Don't try to inflate: a target set before the
			// guest's virtio_balloon driver attaches is silently dropped, which
			// is the bug that left the balloon stuck. Give it the ceiling,
			// capped only by host pressure.
			target = balloonTarget(level, ceiling)
		}
		changed := target != cur
		cur = target
		// Re-issue on every tick, not only on change: a set lost before the
		// guest driver attached must be retried or the balloon stays stuck
		// forever (we believe we set it, but the guest never saw it). The call
		// is idempotent, so re-issuing the same target is harmless.
		dev.SetTargetVirtualMachineMemorySize(target)
		v.mu.Lock()
		v.balloonTargetBytes = target
		v.mu.Unlock()
		debug.Log("vz", "balloon apply", "id", v.spec.ID, "trigger", trigger,
			"level", level.String(), "target_mib", target/(1024*1024),
			"fresh_report", fresh, "changed", changed,
			"guest_avail_mib", rep.AvailableKiB/1024, "guest_psi_centi", rep.PSIMemSomeCenti)
	}

	apply("init")

	for {
		select {
		case <-ctx.Done():
			return
		case level = <-levels:
			apply("pressure")
		case rep = <-reports:
			lastReport = time.Now()
			apply("report")
		case <-t.C:
			apply("tick")
		}
	}
}

// pollGuestMemory dials the guest agent for a memory snapshot every
// memPollInterval and forwards the latest to the controller. A failed poll
// (agent not up yet, image without the reporter) is logged once then retried
// quietly; the controller treats the resulting silence as "no data".
func (v *vm) pollGuestMemory(ctx context.Context, out chan memReport) {
	t := time.NewTicker(memPollInterval)
	defer t.Stop()
	loggedFail := false
	loggedOK := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r, err := v.readGuestMemReport(ctx)
			if err != nil {
				if !loggedFail {
					debug.Log("vz", "guest mem poll failed (retrying quietly)",
						"id", v.spec.ID, "err", err)
					loggedFail = true
					loggedOK = false
				}
				continue
			}
			loggedFail = false
			if !loggedOK {
				debug.Log("vz", "guest mem poll healthy (reporter reachable)",
					"id", v.spec.ID, "total_mib", r.TotalKiB/1024,
					"avail_mib", r.AvailableKiB/1024, "psi_centi", r.PSIMemSomeCenti)
				loggedOK = true
			}
			sendLatestReport(out, r)
		}
	}
}

// readGuestMemReport performs one poll: dial agentMemPort, read the fixed-size
// snapshot, decode it.
func (v *vm) readGuestMemReport(ctx context.Context) (memReport, error) {
	dialCtx, cancel := context.WithTimeout(ctx, memDialTimeout)
	defer cancel()
	conn, err := v.VSock(dialCtx, agentMemPort)
	if err != nil {
		return memReport{}, err
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(memReadTimeout))
	// Read to EOF rather than a fixed size: the guest writes one report and
	// closes, and the report's length depends on the agent's vintage
	// (legacy 24-byte prefix vs. the full snapshot) — decodeMemReport
	// handles both.
	buf, err := io.ReadAll(io.LimitReader(conn, 256))
	if err != nil {
		return memReport{}, fmt.Errorf("read mem report: %w", err)
	}
	return decodeMemReport(buf)
}

// sendLatestLevel/sendLatestReport push the newest value into a depth-1
// channel, discarding any unread stale value first. Each channel has a single
// producer, so "drop the old, keep the new" never races to the wrong value
// and never blocks the producer behind a busy controller.
func sendLatestLevel(ch chan pressureLevel, l pressureLevel) {
	select {
	case ch <- l:
	default:
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- l:
		default:
		}
	}
}

func sendLatestReport(ch chan memReport, r memReport) {
	select {
	case ch <- r:
	default:
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- r:
		default:
		}
	}
}
