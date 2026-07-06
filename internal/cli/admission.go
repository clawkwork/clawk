package cli

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"syscall"

	"github.com/clawkwork/clawk/internal/config"
)

// Memory admission control.
//
// The host kernel panics (launchd faulted by SIGBUS) when the memory committed
// to running VMs plus the host's own needs exceeds physical RAM — VM guest
// memory is effectively unpageable, so the kernel can't relieve the pressure
// and kills an unkillable process instead. The balloon controller reclaims
// reactively, but reactive is too late under a fast burst and depends on a
// host pressure signal we can't fully rely on. Admission control is the
// preventive guarantee: refuse to start a box whose worst-case memory, summed
// with every other running box's worst case, wouldn't leave the host its
// reserve. It fails safe — the only thing it can do is decline to launch.

// hostReserveFloorMiB is the least RAM we ever hold back for the host (macOS
// itself, the user's apps, per-VM host-side overhead) regardless of how small
// the machine is. Above it the reserve scales with host size so big Macs keep
// proportionally more slack.
const hostReserveFloorMiB = 3072

// hostReserveMiB returns the RAM never offered to guests on a host of hostMiB:
// the larger of a quarter of host RAM and the floor. CLAWK_HOST_RESERVE_MIB
// overrides it outright for hosts with unusually heavy or light app load.
//
// Scaling rather than a fixed value keeps clawk usable on small Macs (a fixed
// 6 GiB reserve would refuse every box on an 8 GiB machine) while staying
// conservative on large ones.
func hostReserveMiB(hostMiB uint64) uint64 {
	if v := os.Getenv("CLAWK_HOST_RESERVE_MIB"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return n
		}
	}
	if quarter := hostMiB / 4; quarter > hostReserveFloorMiB {
		return quarter
	}
	return hostReserveFloorMiB
}

// admitGuestMemory reports whether a box wanting wantMiB of guest memory may
// start, given the memory already claimed by other running boxes (otherMiB)
// and the host's physical RAM. It is deliberately worst-case: wantMiB and each
// otherMiB entry are ceilings, so a box is admitted only if every box could
// simultaneously burst to its ceiling and still leave reserveMiB for the host.
// That trades peak concurrency for the guarantee that clawk never
// oversubscribes host RAM — the condition that panics the machine.
func admitGuestMemory(wantMiB uint64, otherMiB []uint64, hostMiB, reserveMiB uint64) error {
	var committed uint64
	for _, m := range otherMiB {
		committed += m
	}
	// Signed math so an over-reserved host (reserve+committed > host) reports a
	// sensible 0 free rather than wrapping around.
	avail := int64(hostMiB) - int64(reserveMiB) - int64(committed)
	if avail < 0 {
		avail = 0
	}
	if int64(wantMiB) > avail {
		return fmt.Errorf(
			"memory admission: this box can use up to %d MiB but only %d MiB is free for guests "+
				"(host %d MiB, %d MiB reserved for macOS, %d MiB committed by %d other running box(es)); "+
				"lower its memory_max, stop another box, or set CLAWK_HOST_RESERVE_MIB",
			wantMiB, avail, hostMiB, reserveMiB, committed, len(otherMiB))
	}
	return nil
}

// hostMemoryProbe reads the host's physical RAM. It's a var so tests can stub
// a deterministic host size instead of the real machine's (the platform
// implementations live in hostmem_{darwin,linux}.go).
var hostMemoryProbe = hostPhysicalMemoryBytes

// admitMemoryForStart is the host-side gate run before bringing a vz box up.
// A host-RAM probe failure does not block the start — clawk stays usable if
// the probe ever breaks — it just means the guarantee is skipped for that run.
func admitMemoryForStart(sb *config.Sandbox) error {
	hostBytes, err := hostMemoryProbe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "clawk: skipping memory admission (cannot read host RAM: %v)\n", err)
		return nil
	}
	_, _, wantMax := specResources(sb)
	hostMiB := hostBytes / (1024 * 1024)
	return admitGuestMemory(wantMax, otherRunningCeilingsMiB(sb.Name), hostMiB, hostReserveMiB(hostMiB))
}

// otherRunningCeilingsMiB returns the memory ceiling of every running box
// except self. "Running" is decided by a live vz.pid, not the stored VMState:
// a box that crashed without updating its state holds no host memory, so
// counting it would wrongly block new starts.
func otherRunningCeilingsMiB(self string) []uint64 {
	boxes, err := store.List()
	if err != nil {
		return nil
	}
	var out []uint64
	for i := range boxes {
		b := &boxes[i]
		if b.Name == self || !vmProcessAlive(b.Name) {
			continue
		}
		_, _, ceiling := specResources(b)
		out = append(out, ceiling)
	}
	return out
}

// vmProcessAlive reports whether the box's VM daemon (which owns the guest's
// host memory) is currently running, via its pidfile (vz.pid on macOS,
// fc.pid on Linux — readVMPID checks both).
func vmProcessAlive(name string) bool {
	pid, err := readVMPID(store.VMDir(name))
	if err != nil || pid <= 0 {
		return false
	}
	// signal 0 probes existence without delivering anything: nil means alive,
	// EPERM means alive-but-not-ours (still counts), ESRCH means gone.
	err = syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
