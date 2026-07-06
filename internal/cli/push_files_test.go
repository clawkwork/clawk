package cli

import (
	"encoding/base64"
	"os/exec"
	"strings"
	"testing"

	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/stretchr/testify/require"
)

// TestBuildFilePushScriptRoundTrips runs the rendered push script through
// `bash -c` on the host (a Linux test VM — see CLAUDE.md). The script's
// `sudo install` step is replaced via a shim PATH so we exercise the
// shape of the command without needing real sudo. If base64 encoding or
// shell quoting break, this test catches the corruption.
func TestBuildFilePushScriptRoundTrips(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not on PATH")
	}
	if _, err := exec.LookPath("base64"); err != nil {
		t.Skip("base64 not on PATH")
	}

	// Content that exercises every quoting hazard: single quote, dollar
	// sign, double quote, backslash, embedded newline, leading/trailing
	// whitespace. Empty bytes too — credentials sometimes contain them.
	payload := "line1 has 'single' and $dollar\n" +
		"line2 has \"double\" and \\back\n" +
		" leading and trailing \n\x00 zero byte"

	guest := "/tmp/clawk-push-test-target"
	script := buildFilePushScript(guest, []byte(payload), 0o600)

	// Build a harness: shadow `sudo` and `install` with shell functions
	// so we don't need privileges, and capture what `install` would have
	// written. Prepend our own line that decodes the inlined base64
	// back to a host file we then compare against the input.
	harness := `
sudo() { "$@"; }
install() {
    # parse out the trailing dest arg
    dest="${!#}"
    # read /dev/stdin into the dest file (host-side, not really /dev/stdin)
    cat > "$dest"
}
export -f sudo install
` + script

	cmd := exec.Command("bash", "-c", harness)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "script failed: %v\noutput:\n%s\nscript:\n%s", err, out, harness)
	got, err := exec.Command("cat", guest).Output()
	require.NoError(t, err, "reading target: %v", err)
	require.Equal(t, payload, string(got), "round-trip mismatch")
}

// TestBuildFilePushScriptUsesAgentOwnership locks the chown user in
// place — credentials owned by root:root would still work for an agent
// that's a sudoer but the principle-of-least-surprise expectation is
// that files placed at /home/agent are owned by agent.
func TestBuildFilePushScriptUsesAgentOwnership(t *testing.T) {
	script := buildFilePushScript("/home/agent/.kube/cfg", []byte("x"), 0o600)
	want := "-o " + sandbox.GuestUser + " -g " + sandbox.GuestUser
	if !strings.Contains(script, want) {
		t.Errorf("script missing %q:\n%s", want, script)
	}
}

// TestBuildFilePushScriptEncodesBinaryPayload confirms the encoder
// catches every byte (not just printable ASCII) — a credentials file
// containing a NUL byte or a UTF-8 multi-byte sequence must round-trip
// unchanged.
func TestBuildFilePushScriptEncodesBinaryPayload(t *testing.T) {
	payload := []byte{0x00, 0x7f, 0x80, 0xff, '\n', 'é'}
	script := buildFilePushScript("/tmp/x", payload, 0o600)
	encoded := base64.StdEncoding.EncodeToString(payload)
	if !strings.Contains(script, encoded) {
		t.Errorf("script does not contain expected base64 encoding\nscript:\n%s\nwant substring:\n%s",
			script, encoded)
	}
}

// TestParentDirRespectsLinuxSeparator: the in-guest script must use `/`
// even though the host might be macOS. parentDir is the helper that
// enforces this; testing it directly keeps the regression surface tight.
func TestParentDirRespectsLinuxSeparator(t *testing.T) {
	cases := map[string]string{
		"/home/agent/.kube/config": "/home/agent/.kube",
		"/etc/foo":                 "/etc",
		"/foo":                     "/",
		"":                         "/",
	}
	for in, want := range cases {
		if got := parentDir(in); got != want {
			t.Errorf("parentDir(%q) = %q, want %q", in, got, want)
		}
	}
}
