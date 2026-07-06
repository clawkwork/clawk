package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConsoleTail(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "console.log")

	t.Run("missing file is silent", func(t *testing.T) {
		if got := ConsoleTail(log, 5); got != "" {
			t.Errorf("ConsoleTail on missing file = %q, want empty", got)
		}
	})

	t.Run("tails last non-empty lines", func(t *testing.T) {
		content := "line1\n\nline2\nline3\n\nclawk-init: FATAL: boom\n\n"
		require.NoError(t, os.WriteFile(log, []byte(content), 0o644))
		got := ConsoleTail(log, 2)
		if !strings.Contains(got, "clawk-init: FATAL: boom") || !strings.Contains(got, "line3") {
			t.Errorf("missing expected tail lines:\n%s", got)
		}
		if strings.Contains(got, "line1") {
			t.Errorf("tail included lines beyond n:\n%s", got)
		}
		if !strings.Contains(got, log) {
			t.Errorf("frame should name the log path:\n%s", got)
		}
	})

	t.Run("blank-only log is silent", func(t *testing.T) {
		require.NoError(t, os.WriteFile(log, []byte("\n\n  \n"), 0o644))
		if got := ConsoleTail(log, 5); got != "" {
			t.Errorf("blank log produced output: %q", got)
		}
	})
}
