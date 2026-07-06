package sandbox

import "fmt"

// Progress narrates long-running provider work (image pulls, rootfs
// builds) to the user. Providers call it sequentially: Step begins a
// unit of work, Detail updates its live status line, StepDone replaces
// the step with a completion summary. Close stops any rendering;
// callers should defer it as soon as they obtain a tracker.
//
// The CLI installs a spinner-based implementation on interactive
// terminals (see internal/cli); everything else gets PlainProgress.
type Progress interface {
	Step(format string, args ...any)
	Detail(format string, args ...any)
	StepDone(format string, args ...any)
	// Skip abandons the current step silently — for steps that turn out
	// to be cache hits, where a checkmark line would just be noise.
	Skip()
	// SetFraction attaches a completion fraction (0..1) to the current
	// step — renderers draw it as a progress bar. Negative clears it
	// (back to indeterminate). Implementations may ignore it.
	SetFraction(frac float64)
	// SetBars replaces the set of per-item sub-progress bars drawn under
	// the current step — one bar per concurrent layer download. Nil or
	// empty clears them. Implementations may ignore it.
	SetBars(bars []Bar)
	Close()
}

// Bar is one labelled sub-progress bar (e.g. a single layer's download)
// rendered on its own line beneath the current step.
type Bar struct {
	// Label names the item, shown left of the bar (e.g. "layer 3/12").
	Label string
	// Frac is the completion fraction, 0..1.
	Frac float64
}

// PlainProgress is the no-frills fallback: one line per completed step,
// no spinner, no transient detail. Suitable for pipes and logs.
type PlainProgress struct{}

func (PlainProgress) Step(format string, args ...any) {
	fmt.Printf(format+"...\n", args...)
}

func (PlainProgress) Detail(string, ...any) {}

func (PlainProgress) StepDone(format string, args ...any) {
	fmt.Printf(format+"\n", args...)
}

func (PlainProgress) Skip() {}

func (PlainProgress) SetFraction(float64) {}

func (PlainProgress) SetBars([]Bar) {}

func (PlainProgress) Close() {}
