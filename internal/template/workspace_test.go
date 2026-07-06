package template

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// mustGit is a test helper for invoking `git` with isolated global config.
func mustGit(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s: %v", args, out, err)
	}
}

func initRepo(t *testing.T, dir string) {
	t.Helper()
	mustGit(t, "init", "-b", "main", dir)
	mustGit(t, "-C", dir, "config", "user.email", "test@test.com")
	mustGit(t, "-C", dir, "config", "user.name", "Test")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README"), []byte("x"), 0o644))
	mustGit(t, "-C", dir, "add", ".")
	mustGit(t, "-C", dir, "commit", "-m", "init")
}

func TestFindGitRepo(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "sub", "deep"), 0o755))
	initRepo(t, repo)

	got, err := FindGitRepo(filepath.Join(repo, "sub", "deep"))
	require.NoError(t, err)
	if got != repo {
		t.Errorf("FindGitRepo returned %s, want %s", got, repo)
	}

	if _, err := FindGitRepo(dir); err == nil {
		t.Error("expected error for non-repo directory")
	}
}

func TestLoadWorkspaceSingleRepo(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "k8s-deploy")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	initRepo(t, repo)

	// Repo clawk.mod exercises vm, network, forwards, on up.
	require.NoError(t, os.WriteFile(
		filepath.Join(repo, "clawk.mod"),
		[]byte(`sandbox (
    vm (
        provider vz
    )
    network (
        allow *.internal.myco.com
    )
    forwards (
        8080
    )
    on up (
        "make deps"
    )
)
`), 0o644))

	wsPath := filepath.Join(dir, RepoFileName)
	require.NoError(t, os.WriteFile(wsPath,
		[]byte("sandbox (\n    includes (\n        ./k8s-deploy\n    )\n)\n"), 0o644))

	ws, err := LoadWorkspace(wsPath)
	require.NoError(t, err)
	if len(ws.Repos) != 1 {
		t.Fatalf("want 1 repo, got %d", len(ws.Repos))
	}
	r := ws.Repos[0]
	if r.Name != "k8s-deploy" {
		t.Errorf("name = %q, want k8s-deploy", r.Name)
	}
	if r.Clawkfile == nil {
		t.Fatal("expected Clawkfile to be loaded")
	}
	if r.Clawkfile.Provider != "vz" {
		t.Errorf("provider = %q", r.Clawkfile.Provider)
	}
	if len(r.Clawkfile.OnUp) != 1 || r.Clawkfile.OnUp[0] != "make deps" {
		t.Errorf("on_up = %v", r.Clawkfile.OnUp)
	}
}

func TestLoadWorkspaceNameOverride(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "odd-dir-name")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	initRepo(t, repo)
	require.NoError(t, os.WriteFile(
		filepath.Join(repo, "clawk.mod"),
		[]byte("sandbox canonical-name (\n)\n"), 0o644))
	wsPath := filepath.Join(dir, RepoFileName)
	require.NoError(t, os.WriteFile(wsPath,
		[]byte("sandbox (\n    includes (\n        ./odd-dir-name\n    )\n)\n"), 0o644))
	ws, err := LoadWorkspace(wsPath)
	require.NoError(t, err)
	if ws.Repos[0].Name != "canonical-name" {
		t.Errorf("name override not applied: %+v", ws.Repos[0])
	}
}

func TestLoadWorkspaceNameCollision(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{"a/same", "b/same"} {
		repo := filepath.Join(dir, sub)
		require.NoError(t, os.MkdirAll(repo, 0o755))
		initRepo(t, repo)
	}
	wsPath := filepath.Join(dir, RepoFileName)
	require.NoError(t, os.WriteFile(wsPath,
		[]byte("sandbox (\n    includes (\n        ./a/same\n        ./b/same\n    )\n)\n"), 0o644))
	_, err := LoadWorkspace(wsPath)
	require.Error(t, err)
	if !strings.Contains(err.Error(), "collides") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLoadWorkspaceMissingGit(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "notgit"), 0o755))
	wsPath := filepath.Join(dir, RepoFileName)
	require.NoError(t, os.WriteFile(wsPath,
		[]byte("sandbox (\n    includes (\n        ./notgit\n    )\n)\n"), 0o644))
	if _, err := LoadWorkspace(wsPath); err == nil {
		t.Error("expected error for non-git include")
	}
}

func TestLoadWorkspaceRejectsRepos(t *testing.T) {
	dir := t.TempDir()
	wsPath := filepath.Join(dir, RepoFileName)
	require.NoError(t, os.WriteFile(wsPath,
		[]byte("sandbox (\n    repos (\n        ./x\n    )\n)\n"), 0o644))
	_, err := LoadWorkspace(wsPath)
	require.Error(t, err)
	if !strings.Contains(err.Error(), "replaced by 'includes'") {
		t.Errorf("expected repos->includes hint, got %v", err)
	}
}

func TestLoadWorkspaceRejectsAppsDirective(t *testing.T) {
	dir := t.TempDir()
	wsPath := filepath.Join(dir, RepoFileName)
	require.NoError(t, os.WriteFile(wsPath,
		[]byte("sandbox (\n    apps (\n        ./x\n    )\n)\n"), 0o644))
	_, err := LoadWorkspace(wsPath)
	require.Error(t, err)
	if !strings.Contains(err.Error(), "no longer supported") {
		t.Errorf("expected migration hint, got %v", err)
	}
}

// TestLoadWorkspaceFlatFileHint: a pre-cutover flat clawk.mod must fail with
// the wrap-in-sandbox migration hint, path-prefixed so the user knows which
// file to edit.
func TestLoadWorkspaceFlatFileHint(t *testing.T) {
	dir := t.TempDir()
	wsPath := filepath.Join(dir, RepoFileName)
	require.NoError(t, os.WriteFile(wsPath,
		[]byte("vm (\n    provider vz\n)\n"), 0o644))
	_, err := LoadWorkspace(wsPath)
	require.Error(t, err)
	require.Contains(t, err.Error(), "wrap the body in `sandbox ( ... )`")
	require.Contains(t, err.Error(), wsPath)
}

// TestLoadWorkspaceRetiredClawkWork: an explicit clawk.work path gets the
// rename hint without the file even being read.
func TestLoadWorkspaceRetiredClawkWork(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, RetiredWorkspaceFileName)
	require.NoError(t, os.WriteFile(legacy, []byte("includes (\n)\n"), 0o644))
	_, err := LoadWorkspace(legacy)
	require.Error(t, err)
	require.Contains(t, err.Error(), "clawk.work is retired")
	require.Contains(t, err.Error(), legacy)
}

func TestLoadStandaloneClawkfile(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "onboarding-repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	initRepo(t, repo)

	require.NoError(t, os.WriteFile(
		filepath.Join(repo, "clawk.mod"),
		[]byte(`sandbox (
    vm (
        provider vz
    )
    forwards (
        3000
    )
    on up (
        "npm install"
    )
)
`), 0o644))

	ws, err := LoadStandaloneClawkfile(repo)
	require.NoError(t, err)
	if len(ws.Repos) != 1 {
		t.Fatalf("got %d repos", len(ws.Repos))
	}
	if ws.Repos[0].Clawkfile == nil ||
		ws.Repos[0].Clawkfile.Forwards[0] != "3000" {
		t.Errorf("standalone clawkfile not loaded: %+v", ws.Repos[0].Clawkfile)
	}
}

// TestLoadStandaloneSandboxHeaderName: the block-header name feeds
// Template.Name so downstream naming is unchanged from the old `name`
// directive.
func TestLoadStandaloneSandboxHeaderName(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "some-dir")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	initRepo(t, repo)
	require.NoError(t, os.WriteFile(filepath.Join(repo, "clawk.mod"),
		[]byte("sandbox my-project (\n)\n"), 0o644))

	ws, err := LoadStandaloneClawkfile(repo)
	require.NoError(t, err)
	require.Equal(t, "my-project", ws.Repos[0].Name)
	require.Equal(t, "my-project", ws.Repos[0].Clawkfile.Name)
}

// TestLoadStandalonePolicies: `policy` blocks beside the sandbox block are
// carried on the workspace for the create path to register.
func TestLoadStandalonePolicies(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "svc")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	initRepo(t, repo)
	require.NoError(t, os.WriteFile(filepath.Join(repo, "clawk.mod"),
		[]byte(`sandbox (
    network (
        use default corp
    )
)

policy corp (
    allow ip 10.20.0.0/16
    deny  telemetry.corp.com
)
`), 0o644))

	ws, err := LoadStandaloneClawkfile(repo)
	require.NoError(t, err)
	require.Len(t, ws.Policies, 1)
	require.Equal(t, "corp", ws.Policies[0].Name)
	require.Equal(t, []string{"10.20.0.0/16"}, ws.Policies[0].AllowIPs)
	require.Equal(t, []string{"default", "corp"}, ws.Repos[0].Clawkfile.Use)
}

func TestWorkspaceFromGitRepo(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "myrepo")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "sub"), 0o755))
	initRepo(t, repo)

	resolvedRepo, _ := filepath.EvalSymlinks(repo)

	// Calling from the repo root yields a single-repo workspace with no
	// Clawkfile and the repo basename as the name.
	ws, err := WorkspaceFromGitRepo(repo)
	if err != nil {
		t.Fatalf("from repo root: %v", err)
	}
	if got, _ := filepath.EvalSymlinks(ws.Root); got != resolvedRepo {
		t.Errorf("ws.Root = %s, want %s", got, resolvedRepo)
	}
	if len(ws.Repos) != 1 {
		t.Fatalf("got %d repos", len(ws.Repos))
	}
	r := ws.Repos[0]
	if r.Name != "myrepo" {
		t.Errorf("repo name = %q, want myrepo", r.Name)
	}
	if r.Clawkfile != nil {
		t.Errorf("expected nil Clawkfile, got %+v", r.Clawkfile)
	}
	if got, _ := filepath.EvalSymlinks(r.RepoPath); got != resolvedRepo {
		t.Errorf("RepoPath = %s, want %s", got, resolvedRepo)
	}

	// From a subdirectory: still resolves to the repo root.
	ws, err = WorkspaceFromGitRepo(filepath.Join(repo, "sub"))
	if err != nil {
		t.Fatalf("from subdir: %v", err)
	}
	if got, _ := filepath.EvalSymlinks(ws.Repos[0].RepoPath); got != resolvedRepo {
		t.Errorf("subdir RepoPath = %s, want %s", got, resolvedRepo)
	}

	// Outside any git repo: error.
	if _, err := WorkspaceFromGitRepo(t.TempDir()); err == nil {
		t.Error("expected error for non-git directory")
	}
}

func TestFindWorkspaceWalksUp(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "svc")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	initRepo(t, repo)
	wsPath := filepath.Join(dir, RepoFileName)
	require.NoError(t, os.WriteFile(wsPath,
		[]byte("sandbox (\n    includes (\n        ./svc\n    )\n)\n"), 0o644))
	deep := filepath.Join(dir, "a", "b", "c")
	require.NoError(t, os.MkdirAll(deep, 0o755))
	ws, err := FindWorkspace(deep)
	require.NoError(t, err)
	got, _ := filepath.EvalSymlinks(ws.Root)
	want, _ := filepath.EvalSymlinks(dir)
	if got != want {
		t.Errorf("ws.Root = %s, want %s", got, want)
	}
}

// TestFindWorkspaceSkipsIncludeless: the walk continues past an includeless
// clawk.mod (a single-repo file), so a workspace root above still wins —
// mirroring the old clawk.work-anywhere-above-clawk.mod precedence.
func TestFindWorkspaceSkipsIncludeless(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "svc")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	initRepo(t, repo)
	require.NoError(t, os.WriteFile(filepath.Join(dir, RepoFileName),
		[]byte("sandbox (\n    includes (\n        ./svc\n    )\n)\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repo, RepoFileName),
		[]byte("sandbox (\n    forwards ( 3000 )\n)\n"), 0o644))

	ws, err := FindWorkspace(repo)
	require.NoError(t, err)
	got, _ := filepath.EvalSymlinks(ws.Root)
	want, _ := filepath.EvalSymlinks(dir)
	require.Equal(t, want, got, "workspace root above must win over includeless cwd clawk.mod")

	// With no workspace above, the walk reports ErrNoWorkspace and callers
	// fall back to the standalone loader.
	lone := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(lone, RepoFileName),
		[]byte("sandbox (\n)\n"), 0o644))
	_, err = FindWorkspace(lone)
	require.ErrorIs(t, err, ErrNoWorkspace)
}

// TestFindWorkspaceRetiredClawkWork: a clawk.work anywhere on the walk —
// where the old loader would have accepted it — produces the rename hint
// naming the file, never a silent skip.
func TestFindWorkspaceRetiredClawkWork(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, RetiredWorkspaceFileName)
	require.NoError(t, os.WriteFile(legacy, []byte("includes (\n)\n"), 0o644))
	deep := filepath.Join(dir, "a", "b")
	require.NoError(t, os.MkdirAll(deep, 0o755))

	_, err := FindWorkspace(deep)
	require.Error(t, err)
	require.Contains(t, err.Error(), "clawk.work is retired")
	require.Contains(t, err.Error(), legacy)
}

// TestFindWorkspaceSurfacesFlatFile: a flat-grammar clawk.mod on the walk
// fails loudly with its migration hint instead of degrading to defaults.
func TestFindWorkspaceSurfacesFlatFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, RepoFileName),
		[]byte("network (\n    allow example.com\n)\n"), 0o644))
	_, err := FindWorkspace(dir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "wrap the body in `sandbox ( ... )`")
}

func TestFilterRepos(t *testing.T) {
	ws := &Workspace{
		Repos: []Repo{
			{Name: "a"}, {Name: "b"}, {Name: "c"},
		},
	}
	filtered, err := ws.FilterRepos([]string{"a", "c"})
	require.NoError(t, err)
	if len(filtered.Repos) != 2 ||
		filtered.Repos[0].Name != "a" || filtered.Repos[1].Name != "c" {
		t.Errorf("filter returned %v", filtered.Repos)
	}
	if _, err := ws.FilterRepos([]string{"bogus"}); err == nil {
		t.Error("expected error for unknown repo")
	}
}
