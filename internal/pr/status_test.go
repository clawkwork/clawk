package pr

// Tests use the GHBin seam to swap a real `gh` for a tiny
// shell-script shim that echoes a canned JSON payload. We don't
// validate gh's API surface — only that this package's mapping
// from gh JSON to (state, number, url) is correct.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// setGHBin points the package's gh seam at bin for one test.
func setGHBin(t *testing.T, bin string) {
	t.Helper()
	prev := GHBin
	GHBin = bin
	t.Cleanup(func() { GHBin = prev })
}

// writeShim drops a tiny shell script that prints `payload` to stdout
// and exits 0, ignoring its arguments. Mode 0755 so the os/exec call
// can run it directly. Returns the absolute path.
func writeShim(t *testing.T, payload string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "gh-shim")
	body := "#!/bin/sh\ncat <<'EOF'\n" + payload + "\nEOF\n"
	require.NoError(t, os.WriteFile(p, []byte(body), 0o755))
	return p
}

func TestResolveMapsStates(t *testing.T) {
	shim := writeShim(t, `[
		{"number": 234, "headRefName": "PROJ-512",   "state": "OPEN",   "url": "https://x/234"},
		{"number": 233, "headRefName": "PROJ-510",   "state": "MERGED", "url": "https://x/233"},
		{"number": 232, "headRefName": "PROJ-509",   "state": "CLOSED", "url": "https://x/232"}
	]`)
	setGHBin(t, shim)

	results, err := Resolve(t.TempDir(), []string{"PROJ-512", "PROJ-510", "PROJ-509", "missing"})
	require.NoError(t, err)
	want := []State{StateOpen, StateMerged, StateClosed, StateUnknown}
	for i, w := range want {
		require.Equal(t, w, results[i].State, "branch %s: got %s, want %s", results[i].Branch, results[i].State, w)
	}
	require.Equal(t, 234, results[0].Number, "PR number not propagated")
	require.Equal(t, 0, results[3].Number, "unknown branch should have zero number")
}

func TestResolveEmptyBranchesNoExec(t *testing.T) {
	// If gh would have been called we'd see an error (no shim set);
	// passing zero branches must short-circuit before we reach exec.
	setGHBin(t, "/does/not/exist")
	results, err := Resolve(t.TempDir(), nil)
	require.NoError(t, err, "zero branches: unexpected error")
	require.Nil(t, results, "zero branches: expected nil result")
}

func TestResolveDedupesMultiplePRs(t *testing.T) {
	// gh returns rows newest-first. When a branch has been recreated
	// (delete + repush + new PR), there can be multiple rows for the
	// same headRefName. Keep the freshest.
	shim := writeShim(t, `[
		{"number": 999, "headRefName": "FOO", "state": "OPEN",   "url": "https://x/999"},
		{"number": 100, "headRefName": "FOO", "state": "MERGED", "url": "https://x/100"}
	]`)
	setGHBin(t, shim)

	results, err := Resolve(t.TempDir(), []string{"FOO"})
	require.NoError(t, err)
	require.Equal(t, 999, results[0].Number, "expected freshest PR (999)")
}
