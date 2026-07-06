package vsockclient

import (
	"strings"
	"testing"
)

// The alt-screen leave (?1049l) carries a cursor-restore side effect, so it
// is not idempotent: emitting it on a clean exit is what caused the "screen
// clears / characters mixed" glitch when leaving `clawk run shell` or claude.
// It must live only in leaveAltScreenSeq (abnormal-exit recovery), never in
// the always-emitted input-mode reset.
func TestInputModeResetHasNoAltScreenLeave(t *testing.T) {
	if strings.Contains(inputModeResetSeq, "1049") {
		t.Fatalf("inputModeResetSeq must not touch the alternate screen; got %q", inputModeResetSeq)
	}
	// Guard the other cursor/buffer movers too — the reset must only *disable*
	// modes, never reposition the cursor or switch buffers.
	for _, bad := range []string{"1047", "1048", "\x1b[2J", "\x1b[H"} {
		if strings.Contains(inputModeResetSeq, bad) {
			t.Fatalf("inputModeResetSeq must not contain %q; got %q", bad, inputModeResetSeq)
		}
	}
	// It must still carry the disables that prevent the input-garble wedge.
	for _, want := range []string{"\x1b[?2004l", "\x1b[?25h", "\x1b[>4m"} {
		if !strings.Contains(inputModeResetSeq, want) {
			t.Errorf("inputModeResetSeq missing %q; got %q", want, inputModeResetSeq)
		}
	}
}

func TestLeaveAltScreenSeqIsExact(t *testing.T) {
	if leaveAltScreenSeq != "\x1b[?1049l" {
		t.Fatalf("leaveAltScreenSeq = %q, want the ?1049l alt-screen leave", leaveAltScreenSeq)
	}
}
