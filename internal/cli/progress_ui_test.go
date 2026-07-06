package cli

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/stretchr/testify/require"

	lipgloss "charm.land/lipgloss/v2"
)

// syncBuffer makes bytes.Buffer safe for the spinner's ticker goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestSpinProgressOrdering pins the two regressions the Bubble Tea
// implementation shipped: completed-step lines printing out of call
// order, and the final StepDone being swallowed by shutdown.
func TestSpinProgressOrdering(t *testing.T) {
	var buf syncBuffer
	p := newSpinProgress(&buf, false, 100)

	p.Step("first step")
	p.Detail("42 MiB")
	p.StepDone("first done")
	p.Step("second step")
	p.StepDone("second done")
	p.Close()

	out := buf.String()
	i, j := strings.Index(out, "✓ first done"), strings.Index(out, "✓ second done")
	require.True(t, i >= 0 && j >= 0 && i < j, "completed lines missing or out of order:\n%q", out)
	require.True(t, strings.HasSuffix(out, "✓ second done\n"), "final StepDone must be the last output:\n%q", out)
	require.Contains(t, out, "42 MiB", "detail never rendered")
}

// TestSpinProgressAbandonedStep: Close during an in-flight step (error
// path) clears the spinner line so the caller's error starts clean.
func TestSpinProgressAbandonedStep(t *testing.T) {
	var buf syncBuffer
	p := newSpinProgress(&buf, false, 100)
	p.Step("doomed step")
	p.Close()
	require.True(t, strings.HasSuffix(buf.String(), eraseLine), "abandoned step did not erase its line:\n%q", buf.String())

	// Close is idempotent and the tracker is restartable (Create and
	// Start share one tracker across two narration phases).
	p.Close()
	p.Step("rebooted step")
	p.StepDone("rebooted done")
	p.Close()
	require.Contains(t, buf.String(), "✓ rebooted done", "tracker not restartable after Close")
}

// TestSpinProgressWritesNothingToStdin documents the contract that the
// renderer is write-only: no raw mode, no terminal queries. The type
// holds only an io.Writer, so this is structural — the test exists to
// keep the property loud for future edits.
func TestSpinProgressWritesNothingToStdin(t *testing.T) {
	var buf syncBuffer
	p := newSpinProgress(&buf, false, 100)
	p.Step("s")
	p.StepDone("d")
	p.Close()
	for _, esc := range []string{"\x1b[?", "\x1b[>", "\x1b[c"} {
		require.NotContains(t, buf.String(), esc, "output contains terminal mode/query sequence %q", esc)
	}
}

// TestSpinProgressSkip: cache-hit steps vanish without a checkmark.
func TestSpinProgressSkip(t *testing.T) {
	var buf syncBuffer
	p := newSpinProgress(&buf, false, 100)
	p.Step("checking cache")
	p.Skip()
	p.Step("real work")
	p.StepDone("real work done")
	p.Close()
	out := buf.String()
	require.NotContains(t, out, "✓ checking cache", "skipped step produced a checkmark")
	require.Contains(t, out, "✓ real work done", "subsequent step lost")
}

// TestSpinProgressBar: a fraction renders as a bar; Step resets it.
func TestSpinProgressBar(t *testing.T) {
	var buf syncBuffer
	p := newSpinProgress(&buf, false, 100)
	p.Step("downloading")
	p.SetFraction(0.5)
	p.StepDone("done")
	p.Close()
	out := buf.String()
	require.True(t, strings.Contains(out, "50%") && strings.Contains(out, "░"),
		"fraction did not render as a bar:\n%q", out)
	require.Contains(t, out, "\n  ", "bar should render on its own line below the step")
	require.True(t, strings.HasSuffix(out, "✓ done\n"), "bar leaked past StepDone:\n%q", out)
}

// TestSpinProgressBars: SetBars draws one labelled bar per layer below
// the step, and StepDone clears them all.
func TestSpinProgressBars(t *testing.T) {
	var buf syncBuffer
	p := newSpinProgress(&buf, false, 100)

	p.Step("downloading layers")
	p.SetBars([]sandbox.Bar{
		{Label: "layer 1/3", Frac: 1.0},
		{Label: "layer 2/3", Frac: 0.5},
		{Label: "layer 3/3", Frac: 0.0},
	})
	out := buf.String()
	for _, want := range []string{"layer 1/3", "layer 2/3", "layer 3/3", "100%", " 50%", "  0%"} {
		require.Contains(t, out, want, "bars output missing %q", want)
	}
	// Three bars + the step line means at least three newlines in the
	// live block.
	require.GreaterOrEqual(t, strings.Count(out, "\n"), 3, "expected a multi-line bar block")

	p.StepDone("done")
	p.Close()
	require.True(t, strings.HasSuffix(buf.String(), "✓ done\n"), "bars leaked past StepDone:\n%q", buf.String())
}

// TestSpinProgressBarsOverflow: more layers than maxVisibleBars collapse
// into a summary line so the painted block can't outgrow the terminal.
func TestSpinProgressBarsOverflow(t *testing.T) {
	var buf syncBuffer
	p := newSpinProgress(&buf, false, 100)
	p.Step("downloading")
	bars := make([]sandbox.Bar, maxVisibleBars+5)
	for i := range bars {
		bars[i] = sandbox.Bar{Label: "layer", Frac: 0.5}
	}
	p.SetBars(bars)
	p.Close()
	require.Contains(t, buf.String(), "6 more layers", "overflow not summarized")
}

// TestSpinProgressOrange: with color on, the spinner/bar/checkmark wear
// the brand orange (256-color 208 via lipgloss) and every escape is
// plain SGR — no terminal queries (the write-only property, in color).
func TestSpinProgressOrange(t *testing.T) {
	var buf syncBuffer
	p := newSpinProgress(&buf, true, 120)
	p.Step("working")
	p.SetFraction(0.5)
	p.StepDone("done")
	p.Close()
	out := buf.String()
	require.Contains(t, out, "38;5;208", "color on but no orange SGR in output")
	for _, esc := range []string{"\x1b[?", "\x1b[>", "\x1b[c", "\x1b]"} {
		require.NotContains(t, out, esc, "styling emitted a terminal query/mode sequence %q", esc)
	}
}

// TestSpinProgressClampsWidth: the in-flight line never exceeds the
// terminal width — a wrapped line breaks the \r redraw and leaves
// fragments stacking up the screen (the bug colored output exposed).
func TestSpinProgressClampsWidth(t *testing.T) {
	var buf syncBuffer
	p := newSpinProgress(&buf, true, 40)
	p.Step("a very long step line that would certainly wrap on a narrow terminal %d", 12345)
	p.SetFraction(0.5)
	p.Detail("with a long detail suffix too")
	p.Close()
	for _, chunk := range strings.Split(buf.String(), eraseLine) {
		for _, line := range strings.Split(chunk, "\n") {
			// Strip repaint cursor motions before measuring.
			line = strings.ReplaceAll(line, "\x1b[1A", "")
			require.LessOrEqual(t, visibleWidth(line), 40, "rendered line is too wide:\n%q", line)
		}
	}
}

// visibleWidth measures terminal cells, skipping SGR sequences.
func visibleWidth(s string) int { return lipgloss.Width(s) }
