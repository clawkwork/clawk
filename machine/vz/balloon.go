package vz

import "fmt"

// pressureLevel is the host memory-pressure level as reported by the XNU
// memorystatus subsystem (the same signal that drives jetsam and
// DISPATCH_SOURCE_TYPE_MEMORYPRESSURE).
type pressureLevel int

const (
	pressureNormal pressureLevel = iota
	pressureWarn
	pressureCritical
)

func (p pressureLevel) String() string {
	switch p {
	case pressureNormal:
		return "normal"
	case pressureWarn:
		return "warn"
	case pressureCritical:
		return "critical"
	default:
		return fmt.Sprintf("pressureLevel(%d)", int(p))
	}
}

// minBalloonFloorBytes is the smallest target we will ever balloon a guest
// down to. Below this a Linux guest running the clawk agent thrashes its own
// OOM killer to no benefit, so we stop reclaiming there and let the host shed
// load elsewhere (or, ultimately, jetsam this VM — which is the acceptable
// outcome we are steering toward instead of a launchd SIGBUS panic).
const minBalloonFloorBytes = 512 * 1024 * 1024

// balloonTarget returns the guest's target memory size in bytes for a given
// host memory-pressure level, where fullBytes is the guest's configured
// (boot) memory. The target is how much memory the guest is allowed to use;
// setting it below fullBytes inflates the virtio balloon, handing the
// difference back to the host. pressureNormal restores the full size
// (balloon fully deflated).
//
// The fractions are deliberately gentle: WARN gives back ~25% and CRITICAL
// ~50%. With several guests live, even the WARN tier reclaims gigabytes —
// enough to relieve the host before the kernel has to fault an unkillable
// process like launchd. Reclaiming is never taken below minBalloonFloorBytes.
func balloonTarget(level pressureLevel, fullBytes uint64) uint64 {
	switch level {
	case pressureWarn:
		return clampFloor(fullBytes/4*3, fullBytes)
	case pressureCritical:
		return clampFloor(fullBytes/2, fullBytes)
	default: // pressureNormal
		return fullBytes
	}
}

// clampFloor keeps target at or above minBalloonFloorBytes, but never raises it
// above full (a guest configured with less than the floor is left untouched).
func clampFloor(target, full uint64) uint64 {
	if full <= minBalloonFloorBytes {
		return full
	}
	if target < minBalloonFloorBytes {
		return minBalloonFloorBytes
	}
	return target
}

// Guest-demand policy.
//
// On top of the host-pressure floor (balloonTarget), a guest configured with
// a baseline below its ceiling is held near the baseline while idle and
// allowed to grow toward the ceiling on demand. Demand is read from the guest
// itself (memReport over vsock) because Apple's balloon reports no guest stats
// to the host. Growth is aggressive (jump straight to the ceiling the moment
// the guest looks tight) so the guest never sits in OOM-thrash waiting for the
// next poll; reclaim is gentle (step down) so we never yank back memory the
// guest just started using.
const (
	// psiDemandCenti is the memory PSI "some avg10" (×100) at or above which
	// we treat the guest as memory-starved and grant the full ceiling. 5%
	// of wall-time stalled on memory in the last 10 s is already a workload
	// that wants more RAM.
	psiDemandCenti = 500

	// lowSlackNum/lowSlackDen express the MemAvailable/MemTotal ratio below
	// which the guest is "tight" and should grow: 1/8 = 12.5% available.
	lowSlackNum, lowSlackDen = 1, 8

	// highSlackNum/highSlackDen express the ratio above which the guest is
	// "roomy" and idle memory can be reclaimed: 2/5 = 40% available. The gap
	// between low and high is the hysteresis band in which we hold steady.
	highSlackNum, highSlackDen = 2, 5
)

// reclaimStepBytes is how much we inflate per reclaim step when the guest is
// idle. Gentle by design — a step at a time avoids reclaiming a large chunk
// the instant a guest goes briefly quiet, only to deflate it again moments
// later. 128 MiB converges an idle 4 GiB guest to its baseline in a handful
// of poll intervals.
const reclaimStepBytes = 128 * 1024 * 1024

// guestDesiredTarget returns the balloon target the guest's own memory state
// argues for, in the closed range [baseline, ceiling], given the current
// target cur (used for gentle stepping and hysteresis). It ignores host
// pressure; mergedBalloonTarget layers that on top.
//
// A zero report (TotalKiB == 0) means "no fresh guest data" — the caller is
// responsible for not constraining the guest in that case; here we simply hold
// cur clamped into range.
func guestDesiredTarget(cur, baseline, ceiling uint64, r memReport) uint64 {
	if ceiling <= baseline || r.TotalKiB == 0 {
		return clampRange(cur, baseline, ceiling)
	}
	// Grow to the ceiling when the guest is stalling on memory or has little
	// reclaimable headroom left.
	if r.PSIMemSomeCenti >= psiDemandCenti ||
		r.AvailableKiB*uint64(lowSlackDen) < r.TotalKiB*uint64(lowSlackNum) {
		return ceiling
	}
	// Reclaim a step toward the baseline when the guest is roomy: high slack
	// means the guest genuinely isn't using the memory, so handing it back to
	// the host is exactly right.
	if r.AvailableKiB*uint64(highSlackDen) > r.TotalKiB*uint64(highSlackNum) {
		if cur <= baseline+reclaimStepBytes {
			return baseline
		}
		return cur - reclaimStepBytes
	}
	// Within the hysteresis band: hold.
	return clampRange(cur, baseline, ceiling)
}

// mergedBalloonTarget combines the guest's demand-driven target with the
// host-pressure floor. The guest may ask to grow up to the ceiling, but host
// pressure caps how much memory the guest is allowed to hold: under WARN/
// CRITICAL the cap drops to balloonTarget's fractions, reclaiming RAM for the
// host even against guest demand (the guest's DEFLATE_ON_OOM is its safety
// net). The result is always min(guest-desired, host-allowed).
func mergedBalloonTarget(level pressureLevel, cur, baseline, ceiling uint64, r memReport) uint64 {
	desired := guestDesiredTarget(cur, baseline, ceiling, r)
	allowed := balloonTarget(level, ceiling)
	if desired > allowed {
		return allowed
	}
	return desired
}

// clampRange constrains v to [lo, hi]. When lo > hi (a guest with no burst
// headroom configured, baseline == ceiling) it collapses to lo.
func clampRange(v, lo, hi uint64) uint64 {
	if hi < lo {
		return lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
