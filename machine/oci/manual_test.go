package oci

// Manual harness for building (and fsck'ing) a rootfs from any image:
//
//	TEST_OCI_REF=docker.io/library/golang:1.25 go test ./oci -run TestBuildManual -v
//
// TEST_OCI_CACHE overrides the cache dir (defaults to a temp dir).
import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/clawkwork/clawk/machine"
	"github.com/stretchr/testify/require"
)

func cacheDirOrTemp(t *testing.T) string {
	if dir := os.Getenv("TEST_OCI_CACHE"); dir != "" {
		return dir
	}
	return t.TempDir()
}

func TestBuildManual(t *testing.T) {
	ref := os.Getenv("TEST_OCI_REF")
	if ref == "" {
		t.Skip("set TEST_OCI_REF to run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	start := time.Now()

	// Inject a payload the way the clawk provider injects clawk-init —
	// /sbin is a symlink into /usr on Debian-based images, so this also
	// exercises the converter's symlink-following on real layouts.
	payload := filepath.Join(t.TempDir(), "payload")
	err := os.WriteFile(payload, []byte("#!/bin/sh\necho injected\n"), 0o644)
	require.NoError(t, err)

	sizeMiB := 4096
	if v := os.Getenv("TEST_OCI_SIZE_MIB"); v != "" {
		n, err := strconv.Atoi(v)
		require.NoError(t, err, "TEST_OCI_SIZE_MIB")
		sizeMiB = n
	}

	res, err := Build(ctx, Options{
		Ref:        ref,
		CacheDir:   cacheDirOrTemp(t),
		Platform:   "linux/" + runtime.GOARCH,
		MinSizeMiB: sizeMiB,
		Inject: []machine.InjectFile{
			{GuestPath: "/sbin/clawk-init", HostPath: payload, Mode: 0o755},
			{GuestPath: "/opt/clawk/bin/clawk-pty-agent", HostPath: payload, Mode: 0o755},
		},
	})
	require.NoError(t, err, "Build")
	t.Logf("built %s in %s (unpacked %d MiB)", res.DiskPath, time.Since(start).Round(time.Millisecond), res.UnpackedBytes>>20)
	out, err := exec.Command("e2fsck", "-f", "-n", res.DiskPath).CombinedOutput()
	require.NoError(t, err, "e2fsck:\n%s", out)
	t.Logf("e2fsck: %s", strings.TrimSpace(string(out)))
	fi, _ := os.Stat(res.DiskPath)
	t.Logf("logical size: %d MiB", fi.Size()>>20)
}
