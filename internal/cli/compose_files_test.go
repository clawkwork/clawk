package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/clawkwork/clawk/internal/template"
	"github.com/stretchr/testify/require"
)

// TestComposeFilesExpandsTilde guards the host-path normalisation pipeline
// for `files (...)` entries: `~/foo` must resolve to $HOME/foo, the host
// file must exist, and a missing guest path mirrors the host path under
// the guest's home so `~/.kube/cfg` lands at /home/agent/.kube/cfg.
func TestComposeFilesExpandsTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	hostFile := filepath.Join(home, ".kube", "config_staging_only")
	require.NoError(t, os.MkdirAll(filepath.Dir(hostFile), 0o755))
	require.NoError(t, os.WriteFile(hostFile, []byte("apiVersion: v1\n"), 0o600))

	out, err := composeFiles([]fileSource{{
		Origin: "clawk.mod",
		Spec:   template.FileSpec{HostPath: "~/.kube/config_staging_only"},
	}})
	require.NoError(t, err)
	require.Len(t, out, 1, "want 1 entry")
	require.Equal(t, hostFile, out[0].HostPath, "HostPath")
	wantGuest := sandbox.GuestHome + "/.kube/config_staging_only"
	require.Equal(t, wantGuest, out[0].GuestPath, "GuestPath")
}

// TestComposeFilesRejectsDirectory enforces the "files = regular file,
// shares = directory" split. Without this guard a user could accidentally
// pass a directory to `files (...)` and get a confusing read-fails error
// later at push time instead of a compose-time message pointing at the
// right directive.
func TestComposeFilesRejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	_, err := composeFiles([]fileSource{{
		Origin: "clawk.mod",
		Spec:   template.FileSpec{HostPath: dir},
	}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "shares (...)", "expected directory rejection")
}

// TestComposeSharesRejectsFile is the symmetric guard for `shares (...)`.
func TestComposeSharesRejectsFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "f")
	require.NoError(t, err)
	f.Close()
	_, err = composeShares([]shareSource{{
		Origin: "clawk.mod",
		Spec:   template.ShareSpec{HostPath: f.Name(), ReadOnly: true},
	}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "files (...)", "expected regular-file rejection")
}

// TestComposeFilesDuplicateGuestPath catches two repos disagreeing on
// what should live at one guest path. Identical (host, guest, mode)
// from multiple sources is fine — silent dedupe is what we want there —
// but diverging hosts must surface a conflict message naming both.
func TestComposeFilesDuplicateGuestPath(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	for _, p := range []string{a, b} {
		require.NoError(t, os.WriteFile(p, []byte("x"), 0o600))
	}
	_, err := composeFiles([]fileSource{
		{Origin: "repoA", Spec: template.FileSpec{HostPath: a, GuestPath: "/etc/foo"}},
		{Origin: "repoB", Spec: template.FileSpec{HostPath: b, GuestPath: "/etc/foo"}},
	})
	require.Error(t, err, "expected conflict on duplicate guest path")
	for _, want := range []string{"repoA", "repoB", "/etc/foo"} {
		require.Contains(t, err.Error(), want, "error should name both contributors")
	}
}

// TestComposeFilesIdenticalDuplicateSilent checks that a workspace and a
// repo declaring the SAME (host, guest) pair don't error — both are
// pointing at the same artifact, so silent dedupe is the right answer.
func TestComposeFilesIdenticalDuplicateSilent(t *testing.T) {
	dir := t.TempDir()
	h := filepath.Join(dir, "creds")
	require.NoError(t, os.WriteFile(h, []byte("x"), 0o600))
	out, err := composeFiles([]fileSource{
		{Origin: "workspace", Spec: template.FileSpec{HostPath: h, GuestPath: "/etc/foo"}},
		{Origin: "repoA", Spec: template.FileSpec{HostPath: h, GuestPath: "/etc/foo"}},
	})
	require.NoError(t, err, "identical specs should dedupe silently")
	require.Len(t, out, 1, "want 1 entry after dedupe")
}

// TestComposeSharesDefaultsReadOnly threads the parser default through
// the compose layer. Even if a Clawkfile says nothing about ro/rw, the
// resulting config.HostShare carries ReadOnly=true — because the parser
// set it on the ShareSpec.
func TestComposeSharesDefaultsReadOnly(t *testing.T) {
	dir := t.TempDir()
	out, err := composeShares([]shareSource{{
		Origin: "clawk.mod",
		Spec:   template.ShareSpec{HostPath: dir, ReadOnly: true},
	}})
	require.NoError(t, err)
	require.True(t, out[0].ReadOnly, "ReadOnly = false, want true (default for shares)")
}

// TestResolveGuestPathAbsoluteRequired keeps a relative guest path from
// silently turning into something rooted at the current working directory
// inside the VM — there's no good default there, so we make the user
// declare an explicit absolute path or `~`-prefix.
func TestResolveGuestPathAbsoluteRequired(t *testing.T) {
	_, err := resolveGuestPath("~/x", "relative/path")
	require.Error(t, err, "expected error for relative guest path")
}

// TestResolveGuestPathMirrorsHomePrefix is the convenience case: an
// absolute host path that starts with $HOME (no `~`) should still mirror
// onto /home/agent inside the guest. Otherwise a Mac user who wrote
// `/Users/me/.aws/credentials` would land at the same path inside Linux,
// which doesn't exist.
func TestResolveGuestPathMirrorsHomePrefix(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := resolveGuestPath(filepath.Join(home, ".aws/creds"), "")
	require.NoError(t, err)
	require.Equal(t, sandbox.GuestHome+"/.aws/creds", got)
}
