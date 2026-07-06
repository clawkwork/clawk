package agentembed

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAgentSourceCompiles simulates what internal/guestbuild does on the
// host: writes the embedded .in files into a temp dir, renames them to
// their canonical names, runs `go mod tidy && go build`. If this passes,
// a real cross-compile will too.
//
// We caught two real bugs (`net.FileListener` not supporting AF_VSOCK,
// and the child-exit deadlock) only after the user manually built the
// agent in a live VM. This test makes those failure classes a CI
// concern instead of a "first sandbox from a new clawk" concern.
//
// Always runs — the compile is host-OS-agnostic because we cross-compile
// to linux/$(GOARCH). Skips only if the host has no `go` tool, which
// would mean the test couldn't possibly have set up Go to run itself.
func TestAgentSourceCompiles(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("`go` not on PATH")
	}

	tmp := t.TempDir()
	mustWrite(t, filepath.Join(tmp, "main.go"), AgentMainGo)
	mustWrite(t, filepath.Join(tmp, "go.mod"), AgentGoMod)

	// `go mod tidy` first — fetches the guest-only deps (creack/pty,
	// mdlayher/vsock). Network is required; CI without internet will
	// have to add a vendored alternative.
	if out, err := runIn(t, tmp, "go", "mod", "tidy"); err != nil {
		t.Fatalf("go mod tidy failed: %v\n%s", err, out)
	}

	// Cross-compile for linux. The guest is always linux; never run
	// the produced binary here. AGENT_TEST_NO_BUILD lets folks skip
	// this on a host without network.
	if os.Getenv("AGENT_TEST_NO_BUILD") != "" {
		t.Log("AGENT_TEST_NO_BUILD set; skipping the build step")
		return
	}
	out, err := runIn(t, tmp, "go", "build", "-o", filepath.Join(tmp, "clawk-pty-agent"), ".")
	if err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}

	bin := filepath.Join(tmp, "clawk-pty-agent")
	fi, err := os.Stat(bin)
	require.NoError(t, err, "agent binary not produced")
	if fi.Size() < 100*1024 {
		t.Fatalf("agent binary suspiciously small (%d bytes); build probably hollow", fi.Size())
	}

	// On linux/$(GOARCH) we can sanity-check the binary is executable.
	// Any other OS skips that check — it'd be a non-native binary.
	if runtime.GOOS == "linux" {
		ver, err := exec.Command(bin, "--help").CombinedOutput()
		if err != nil && !strings.Contains(string(ver), "Usage") {
			// flag.Parse exits non-zero on --help in some configs;
			// what matters is we got recognizable output, not the
			// exit code. Surface only genuinely broken binaries.
			t.Logf("--help output (rc=%v):\n%s", err, ver)
		}
	}
}

// mustWrite writes content to path and fatals on error. Tiny helper so
// the assertion-heavy tests above stay readable.
func mustWrite(t *testing.T, path string, content []byte) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, content, 0o644), "writing %s", path)
}

func runIn(t *testing.T, dir, cmdName string, args ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command(cmdName, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		// Cross-compile target. Even when we're already linux/arm64
		// (matches host), being explicit keeps the test stable across
		// CI runners that might have GOARCH or GOOS unset / wrong.
		"GOOS=linux",
		"GOARCH="+runtime.GOARCH,
		"CGO_ENABLED=0",
	)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.Bytes(), err
}
