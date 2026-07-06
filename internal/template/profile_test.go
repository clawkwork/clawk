package template

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMergeAdditive: Merge unions lists and prefers non-empty scalars in the
// overlay. Profiles must not be able to shrink configuration.
func TestMergeAdditive(t *testing.T) {
	base := &Template{
		Name:     "base",
		Provider: "firecracker",
		Domains:  []string{"a.com"},
		IPs:      []string{"1.1.1.1"},
		Forwards: []string{"3000"},
		OnUp:     []string{"base-cmd"},
	}
	over := &Template{
		Provider: "vz", // scalar override
		Domains:  []string{"b.com"},
		IPs:      []string{"2.2.2.2"},
		Forwards: []string{"4000"},
		OnUp:     []string{"over-cmd"},
	}
	base.Merge(over)

	if base.Name != "base" {
		t.Errorf("Name wrongly overwritten: %q", base.Name)
	}
	if base.Provider != "vz" {
		t.Errorf("Provider should override to vz, got %q", base.Provider)
	}
	wantDomains := []string{"a.com", "b.com"}
	if !equalStrings(base.Domains, wantDomains) {
		t.Errorf("Domains = %v, want %v", base.Domains, wantDomains)
	}
	if !equalStrings(base.IPs, []string{"1.1.1.1", "2.2.2.2"}) {
		t.Errorf("IPs = %v", base.IPs)
	}
	if !equalStrings(base.Forwards, []string{"3000", "4000"}) {
		t.Errorf("Forwards = %v", base.Forwards)
	}
	if !equalStrings(base.OnUp, []string{"base-cmd", "over-cmd"}) {
		t.Errorf("OnUp = %v", base.OnUp)
	}
}

// TestProfileOverlayAppliesToRepo loads a repo with a base clawk.mod plus a
// clawk.mod.investigation overlay and verifies both are reflected in the
// workspace result.
func TestProfileOverlayAppliesToRepo(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "svc")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	initRepo(t, repo)

	require.NoError(t, os.WriteFile(filepath.Join(repo, RepoFileName),
		[]byte("sandbox (\n  network (\n    allow *.dev.myco.com\n  )\n)\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repo, RepoFileName+".investigation"),
		[]byte("sandbox (\n  network (\n    allow *.kube.internal.myco.com\n    allow *.postgres.internal.myco.com\n  )\n)\n"),
		0o644))

	wsPath := filepath.Join(dir, RepoFileName)
	require.NoError(t, os.WriteFile(wsPath,
		[]byte("sandbox (\n  includes (\n    ./svc\n  )\n)\n"), 0o644))

	ws, err := LoadWorkspaceWithProfile(wsPath, "investigation")
	require.NoError(t, err)
	got := ws.Repos[0].Clawkfile.Domains
	want := map[string]bool{
		"*.dev.myco.com":               true,
		"*.kube.internal.myco.com":     true,
		"*.postgres.internal.myco.com": true,
	}
	if len(got) != len(want) {
		t.Fatalf("domains = %v, want %v", got, want)
	}
	for _, d := range got {
		if !want[d] {
			t.Errorf("unexpected domain %q", d)
		}
	}
}

// TestProfileOverlayAppliesToWorkspace: overlay only at workspace level.
// (Repo has no clawk.mod at all.)
func TestProfileOverlayAppliesToWorkspace(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "svc")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	initRepo(t, repo)

	base := filepath.Join(dir, RepoFileName)
	require.NoError(t, os.WriteFile(base,
		[]byte("sandbox (\n  includes (\n    ./svc\n  )\n  network (\n    allow *.corp.com\n  )\n)\n"), 0o644))
	require.NoError(t, os.WriteFile(base+".audit",
		[]byte("sandbox (\n  network (\n    allow *.audit.corp.com\n  )\n)\n"), 0o644))

	ws, err := LoadWorkspaceWithProfile(base, "audit")
	require.NoError(t, err)
	if len(ws.File.Domains) != 2 {
		t.Errorf("want 2 domains after merge, got %v", ws.File.Domains)
	}
}

// TestProfileMissingReportsError: asking for a profile that nothing on disk
// matches must fail loudly so typos surface.
func TestProfileMissingReportsError(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "svc")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	initRepo(t, repo)

	wsPath := filepath.Join(dir, RepoFileName)
	require.NoError(t, os.WriteFile(wsPath,
		[]byte("sandbox (\n  includes (\n    ./svc\n  )\n)\n"), 0o644))

	_, err := LoadWorkspaceWithProfile(wsPath, "ghost")
	require.Error(t, err)
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error should mention profile name: %v", err)
	}
}

// TestProfileOverlayRejectsIncludes: workspace/repo overlays must not add
// includes/repos. Those are structural declarations, not policy.
func TestProfileOverlayRejectsIncludes(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "svc")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	initRepo(t, repo)
	require.NoError(t, os.WriteFile(filepath.Join(repo, RepoFileName), []byte(""), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repo, RepoFileName+".bad"),
		[]byte("sandbox (\n  includes (\n    ./other\n  )\n)\n"), 0o644))
	wsPath := filepath.Join(dir, RepoFileName)
	require.NoError(t, os.WriteFile(wsPath,
		[]byte("sandbox (\n  includes (\n    ./svc\n  )\n)\n"), 0o644))
	if _, err := LoadWorkspaceWithProfile(wsPath, "bad"); err == nil {
		t.Error("expected repo profile to reject 'includes'")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
