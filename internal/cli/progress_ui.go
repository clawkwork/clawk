package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/clawkwork/clawk/internal/sandbox"

	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/spinner"
	lipgloss "charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
)

// newProgressTracker returns a spinner-based sandbox.Progress for
// interactive terminals, or nil when stdout isn't a TTY (providers fall
// back to PlainProgress).
//
// Deliberately NOT a Bubble Tea program: clawk's create/boot flow ends
// by handing the same terminal to another full TUI (claude). A TUI
// framework enables raw mode and emits capability queries whose
// responses land on stdin after it exits — the next TUI reads them as
// garbage and its rendering corrupts. This renderer is write-only:
// carriage returns and erase-line only, no input, no mode changes, no
// queries — safe to run right before any terminal handoff.
func newProgressTracker() sandbox.Progress {
	if !term.IsTerminal(os.Stdout.Fd()) {
		return nil
	}
	width, _, err := term.GetSize(os.Stdout.Fd())
	if err != nil || width <= 0 {
		width = 80
	}
	// Orange — the claw. Suppressed under the NO_COLOR convention.
	return newSpinProgress(os.Stdout, os.Getenv("NO_COLOR") == "", width)
}

// newSpinProgress wires a renderer for out. Split from
// newProgressTracker so tests can target a buffer.
func newSpinProgress(out io.Writer, color bool, width int) *spinProgress {
	return &spinProgress{out: out, color: color, width: width, bar: newBar()}
}

// newBar builds the bubbles progress bar this renderer drives by hand —
// an orange gradient in the style of the progress-animated example. The
// Model is used purely as a view (ViewAs); animation comes from our own
// ticker easing the shown value toward the target.
func newBar() progress.Model {
	return progress.New(
		progress.WithWidth(barWidth),
		progress.WithColors(lipgloss.Color("214"), lipgloss.Color("202")),
	)
}

// orangeStyle is the brand color. lipgloss v2 renders deterministically
// (no terminal queries unless explicitly requested), so this preserves
// the renderer's write-only property.
var orangeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("208"))

// dot is the charm spinner this renderer animates by hand — same frames
// and cadence as a Bubble Tea spinner.Model, none of the program.
var dot = spinner.Dot

// eraseLine returns the cursor to column 0 and clears the row.
// x/ansi is charm's own low-level library — the same package lipgloss
// and bubbletea build on; lipgloss exposes no erase/truncate API of its
// own, so this is as high-level as the charm stack goes for in-place
// single-line redraws short of running a full tea.Program (which the
// claude tty handoff rules out).
const eraseLine = "\r" + ansi.EraseEntireLine

const barWidth = 20

// spinProgress renders one in-flight step as `⠋ step  (detail)` redrawn
// in place; StepDone replaces it with a permanent ✓ line. All writes
// are serialized under mu, so output order matches call order exactly.
type spinProgress struct {
	out   io.Writer
	color bool
	width int // terminal columns; rendering clamps to this
	bar   progress.Model

	mu        sync.Mutex
	step      string
	detail    string
	frac      float64       // bar target; <0 = indeterminate (no bar)
	shown     float64       // bar position eased toward frac each tick
	bars      []sandbox.Bar // per-item sub-bars (concurrent layer downloads)
	frame     int
	liveLines int           // unterminated screen lines from the last paint
	stop      chan struct{} // non-nil while the ticker goroutine runs
	tick      sync.WaitGroup
}

// maxVisibleBars caps how many per-layer sub-bars are drawn at once, so a
// many-layered image can't paint a block taller than the terminal (which
// would break the in-place redraw). Overflow collapses to a summary line.
const maxVisibleBars = 12

func (s *spinProgress) Step(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.step = fmt.Sprintf(format, args...)
	s.detail = ""
	s.frac, s.shown = -1, -1
	s.bars = nil
	s.renderLocked()
	if s.stop == nil {
		s.stop = make(chan struct{})
		s.tick.Add(1)
		go s.spin(s.stop)
	}
}

func (s *spinProgress) Detail(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.step == "" {
		return
	}
	s.detail = fmt.Sprintf(format, args...)
	s.renderLocked()
}

// Skip clears the in-flight step without a completion line — cache
// hits would otherwise accumulate checkmarks that say nothing.
func (s *spinProgress) Skip() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.step == "" {
		return
	}
	s.step, s.detail, s.bars = "", "", nil
	s.clearLocked()
}

func (s *spinProgress) StepDone(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.step, s.detail, s.frac, s.shown, s.bars = "", "", -1, -1, nil
	s.clearLocked()
	fmt.Fprintf(s.out, s.paint("✓")+" "+format+"\n", args...)
}

// SetFraction sets the bar's target; the ticker animates toward it.
func (s *spinProgress) SetFraction(frac float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.step == "" {
		return
	}
	if frac > 1 {
		frac = 1
	}
	s.frac = frac
	if s.shown < 0 {
		// First report: snap rather than sweep up from zero.
		s.shown = frac
	}
	s.renderLocked()
}

// SetBars replaces the per-item sub-bars drawn beneath the step. Each
// reports its own fraction directly (no easing): they update from real
// download counters often enough that easing would only add lag.
func (s *spinProgress) SetBars(bars []sandbox.Bar) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.step == "" {
		return
	}
	s.bars = bars
	s.renderLocked()
}

func (s *spinProgress) Close() {
	s.mu.Lock()
	if s.stop != nil {
		close(s.stop)
		s.stop = nil
	}
	if s.step != "" {
		// A step was abandoned (error path) — clear the live lines so
		// the error prints on a clean row. The error text itself names
		// what failed.
		s.step, s.detail, s.bars = "", "", nil
		s.clearLocked()
	}
	s.mu.Unlock()
	s.tick.Wait()
}

// spin advances the frame until stopped. It only redraws; all state
// changes happen in the calling methods.
func (s *spinProgress) spin(stop chan struct{}) {
	defer s.tick.Done()
	t := time.NewTicker(dot.FPS)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s.mu.Lock()
			if s.step != "" {
				s.frame = (s.frame + 1) % len(dot.Frames)
				// Ease the bar toward its target — the animation the
				// progress-animated example gets from tea FrameMsgs.
				if s.frac >= 0 && s.shown >= 0 && s.shown != s.frac {
					s.shown += (s.frac - s.shown) * 0.3
					if diff := s.frac - s.shown; diff < 0.005 && diff > -0.005 {
						s.shown = s.frac
					}
				}
				s.renderLocked()
			}
			s.mu.Unlock()
		}
	}
}

// clearLocked erases the live (unterminated) lines from the last paint,
// leaving the cursor at column 0 of the top one. mu must be held.
func (s *spinProgress) clearLocked() {
	if s.liveLines == 0 {
		return
	}
	out := "\r" + ansi.EraseEntireLine
	for i := 1; i < s.liveLines; i++ {
		out += ansi.CursorUp(1) + ansi.EraseEntireLine
	}
	fmt.Fprint(s.out, out)
	s.liveLines = 0
}

// renderLocked repaints the live area: the spinner line, plus — like
// the progress-download example — the bar on its own line, where long
// step text can't truncate it away.
func (s *spinProgress) renderLocked() {
	line := s.paint(dot.Frames[s.frame]) + s.step
	if s.detail != "" {
		line += "  (" + s.detail + ")"
	}
	lines := []string{line}
	if s.shown >= 0 {
		lines = append(lines, "  "+s.bar.ViewAs(s.shown))
	}
	lines = append(lines, s.barLinesLocked()...)
	// Clamp each line to the terminal width (ANSI-aware): a wrapped
	// line breaks the redraw — erase-line only clears the final wrapped
	// row, so fragments stack up the screen.
	if s.width > 0 {
		for i := range lines {
			lines[i] = ansi.Truncate(lines[i], s.width-1, "…")
		}
	}
	s.clearLocked()
	fmt.Fprint(s.out, strings.Join(lines, "\n"))
	s.liveLines = len(lines)
}

// barLinesLocked renders the per-item sub-bars, one per line, with labels
// padded to a common width so the bars align. Beyond maxVisibleBars the
// tail collapses to a count, keeping the painted block within the
// terminal so the in-place redraw stays intact. mu must be held.
func (s *spinProgress) barLinesLocked() []string {
	if len(s.bars) == 0 {
		return nil
	}
	bars := s.bars
	var overflow int
	if len(bars) > maxVisibleBars {
		overflow = len(bars) - (maxVisibleBars - 1)
		bars = bars[:maxVisibleBars-1]
	}
	labelWidth := 0
	for _, b := range bars {
		if len(b.Label) > labelWidth {
			labelWidth = len(b.Label)
		}
	}
	lines := make([]string, 0, len(bars)+1)
	for _, b := range bars {
		frac := b.Frac
		if frac < 0 {
			frac = 0
		} else if frac > 1 {
			frac = 1
		}
		// ViewAs draws the gradient bar with its own percentage suffix.
		lines = append(lines, fmt.Sprintf("  %-*s  %s", labelWidth, b.Label, s.bar.ViewAs(frac)))
	}
	if overflow > 0 {
		lines = append(lines, fmt.Sprintf("  … %d more layers", overflow))
	}
	return lines
}

// paint wraps str in the brand orange when color is on. Zero-value
// trackers (tests) stay colorless, keeping assertions byte-exact.
func (s *spinProgress) paint(str string) string {
	if !s.color {
		return str
	}
	return orangeStyle.Render(str)
}
