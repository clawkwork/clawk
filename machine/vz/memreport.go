package vz

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"time"

	"github.com/clawkwork/clawk/machine"
)

// agentMemPort is the AF_VSOCK port the in-guest agent serves memory
// snapshots on: the host dials, the guest writes exactly one memReport and
// closes. Distinct from the pty agent (1024), time-sync (1025), and
// ssh-agent (1026) so the protocols stay cleanly separated.
//
// Keep in lock-step with the memReport server in
// internal/agentembed/main.go.in (the guest inlines its own encoder).
const agentMemPort uint32 = 1027

// memReport wire sizes. The report grew from three to five big-endian
// uint64s when the guest agent started sampling activity (load, net I/O)
// alongside memory. Decoding accepts the legacy 24-byte prefix alone so a
// new host reads an old guest's report (and vice versa: an old host's
// fixed 24-byte read simply ignores the tail a new guest appends).
const (
	memReportLegacySize = 24
	memReportSize       = 40
)

// memReport is a guest snapshot. The balloon controller polls the memory
// fields to decide whether to deflate the balloon (grant the guest more
// RAM, up to the ceiling) or inflate it (reclaim idle RAM back toward the
// baseline). The activity fields feed the sandbox daemon's idle watchdog
// via FetchGuestStats.
//
// Apple's virtio balloon exposes no guest statistics queue (VIRTIO_BALLOON_F_
// STATS_VQ is not negotiated), so this report — pushed over vsock by the
// guest agent — is the host's only window into guest state.
type memReport struct {
	// TotalKiB and AvailableKiB mirror /proc/meminfo's MemTotal and
	// MemAvailable. AvailableKiB is the kernel's own estimate of how much
	// memory is reclaimable without swapping, which is a better demand
	// signal than MemFree.
	TotalKiB     uint64
	AvailableKiB uint64

	// PSIMemSomeCenti is the memory PSI "some avg10" value (percent of the
	// last 10 s in which at least one task stalled on memory) scaled by 100
	// — e.g. 12.34% is 1234. Zero when PSI is unavailable (the kernel was
	// booted without psi=1); the controller then leans on the
	// available/total ratio alone.
	PSIMemSomeCenti uint64

	// Load1Centi is /proc/loadavg's 1-minute average × 100. Zero either
	// means a truly idle guest or a legacy agent that predates the field —
	// HasActivity disambiguates.
	Load1Centi uint64

	// NetIOBytes is the cumulative rx+tx byte count across the guest's
	// non-loopback interfaces. Meaningful only as a delta between two
	// samples: moving counters mean traffic is flowing.
	NetIOBytes uint64

	// HasActivity reports whether the guest agent is new enough to include
	// the activity fields (Load1Centi, NetIOBytes). When false those fields
	// are zero because they're absent, not because the guest is quiet.
	HasActivity bool
}

// encodeMemReport serializes r into exactly memReportSize bytes.
func encodeMemReport(r memReport) []byte {
	b := make([]byte, memReportSize)
	binary.BigEndian.PutUint64(b[0:8], r.TotalKiB)
	binary.BigEndian.PutUint64(b[8:16], r.AvailableKiB)
	binary.BigEndian.PutUint64(b[16:24], r.PSIMemSomeCenti)
	binary.BigEndian.PutUint64(b[24:32], r.Load1Centi)
	binary.BigEndian.PutUint64(b[32:40], r.NetIOBytes)
	return b
}

// decodeMemReport is the inverse of encodeMemReport. A buffer shorter than
// the legacy prefix is an error rather than a zero-padded report — a
// truncated read means the guest agent died mid-write, which the caller
// must treat as "no data", not "guest reports zero memory". A buffer that
// carries only the legacy prefix decodes with HasActivity false.
func decodeMemReport(b []byte) (memReport, error) {
	if len(b) < memReportLegacySize {
		return memReport{}, fmt.Errorf("vz: mem report short: %d bytes, want %d", len(b), memReportLegacySize)
	}
	r := memReport{
		TotalKiB:        binary.BigEndian.Uint64(b[0:8]),
		AvailableKiB:    binary.BigEndian.Uint64(b[8:16]),
		PSIMemSomeCenti: binary.BigEndian.Uint64(b[16:24]),
	}
	if len(b) >= memReportSize {
		r.Load1Centi = binary.BigEndian.Uint64(b[24:32])
		r.NetIOBytes = binary.BigEndian.Uint64(b[32:40])
		r.HasActivity = true
	}
	return r, nil
}

// GuestStats is the exported view of the guest agent's snapshot, for
// callers outside the balloon controller (the sandbox daemon's idle
// watchdog). Field semantics match memReport.
type GuestStats struct {
	TotalKiB        uint64
	AvailableKiB    uint64
	PSIMemSomeCenti uint64
	Load1Centi      uint64
	NetIOBytes      uint64

	// HasActivity is false when the guest runs a legacy agent without the
	// activity fields; Load1Centi and NetIOBytes are then meaningless.
	HasActivity bool
}

// guestStatsReadTimeout bounds the read after a successful dial. The guest
// writes one report and closes immediately, so anything slower than a
// couple of seconds means the agent is wedged.
const guestStatsReadTimeout = 3 * time.Second

// FetchGuestStats dials the guest agent's snapshot port on m and returns
// one decoded report. It works on any machine whose backend implements
// VSock and whose guest runs the clawk agent — the protocol is defined by
// internal/agentembed, but this package owns the host-side decoding, so
// the fetch lives here rather than growing a third copy of the wire
// format. Callers bound the dial via ctx.
func FetchGuestStats(ctx context.Context, m machine.Machine) (GuestStats, error) {
	conn, err := m.VSock(ctx, agentMemPort)
	if err != nil {
		return GuestStats{}, err
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(guestStatsReadTimeout))
	// Read to EOF (the guest closes after one report), capped well above
	// any future report size so a runaway writer can't balloon memory.
	buf, err := io.ReadAll(io.LimitReader(conn, 256))
	if err != nil {
		return GuestStats{}, fmt.Errorf("read guest stats: %w", err)
	}
	r, err := decodeMemReport(buf)
	if err != nil {
		return GuestStats{}, err
	}
	return GuestStats{
		TotalKiB:        r.TotalKiB,
		AvailableKiB:    r.AvailableKiB,
		PSIMemSomeCenti: r.PSIMemSomeCenti,
		Load1Centi:      r.Load1Centi,
		NetIOBytes:      r.NetIOBytes,
		HasActivity:     r.HasActivity,
	}, nil
}
