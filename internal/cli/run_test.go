package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/stretchr/testify/require"
)

// gitInit initialises a repo with an initial commit on main.
func gitInit(t *testing.T, dir string) {
	t.Helper()
	mustGit(t, "init", "-b", "main", dir)
	mustGit(t, "-C", dir, "config", "user.email", "test@test.com")
	mustGit(t, "-C", dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, "-C", dir, "add", ".")
	mustGit(t, "-C", dir, "commit", "-m", "init")
}

func mustGit(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %s: %v", strings.Join(args, " "), out, err)
	}
}

func TestRunCreatesSandboxFromTemplate(t *testing.T) {
	_, mock := setupTest(t)

	// Two test repos.
	dir := t.TempDir()
	repoA := filepath.Join(dir, "repo-a")
	repoB := filepath.Join(dir, "repo-b")
	gitInit(t, repoA)
	gitInit(t, repoB)

	// Workspace clawk.mod on disk: a sandbox block with includes.
	tmplPath := filepath.Join(dir, "clawk.mod")
	body := fmt.Sprintf(`
sandbox (
    vm (
        provider vz
    )

    includes (
        %s
        %s
    )

    network (
        allow example.com
        allow ip 10.0.0.5
    )
)
`, repoA, repoB)
	require.NoError(t, os.WriteFile(tmplPath, []byte(body), 0o644))

	_, err := executeCommand("work", tmplPath, "TICKET-1234", "--bare")
	require.NoError(t, err)

	// Sandbox named after the branch (sanitised).
	sb, err := store.Load("TICKET-1234")
	require.NoError(t, err, "sandbox not created")
	require.Len(t, sb.Phases, 2)
	for _, p := range sb.Phases {
		if p.Branch != "TICKET-1234" {
			t.Errorf("phase branch = %q, want TICKET-1234", p.Branch)
		}
	}
	if !containsStr(sb.Network.Block(config.BlockOriginMod).AllowIPs, "10.0.0.5") {
		t.Errorf("expected allow IP 10.0.0.5, got %v", sb.Network.Block(config.BlockOriginMod).AllowIPs)
	}
	if !containsStr(sb.Network.Block(config.BlockOriginMod).AllowDomains, "example.com") {
		t.Errorf("expected allow domain example.com, got %v", sb.Network.Block(config.BlockOriginMod).AllowDomains)
	}

	// Verify the worktree branch was created in both repos.
	for _, repo := range []string{repoA, repoB} {
		out, err := exec.Command("git", "-C", repo, "branch", "--list", "TICKET-1234").Output()
		require.NoError(t, err)
		if !strings.Contains(string(out), "TICKET-1234") {
			t.Errorf("branch TICKET-1234 missing in %s", repo)
		}
	}

	// --bare means the mock provider should NOT have seen Start.
	require.Len(t, mock.Started, 0, "--bare should not start VM, but got %v", mock.Started)
}

func TestRunIsIdempotent(t *testing.T) {
	_, _ = setupTest(t)
	dir := t.TempDir()
	repo := filepath.Join(dir, "r")
	gitInit(t, repo)

	tmplPath := filepath.Join(dir, "clawk.mod")
	require.NoError(t, os.WriteFile(tmplPath,
		[]byte(fmt.Sprintf("sandbox (\nvm (\nprovider vz\n)\nincludes (\n %s\n)\n)\n", repo)),
		0o644))

	_, err := executeCommand("work", tmplPath, "BRANCH-1", "--bare")
	require.NoError(t, err)
	// Running again with the same args should be a no-op on phases.
	_, err = executeCommand("work", tmplPath, "BRANCH-1", "--bare")
	require.NoError(t, err, "second run failed")

	sb, _ := store.Load("BRANCH-1")
	require.Len(t, sb.Phases, 1, "phases duplicated on re-run")
}

// TestRunWorkspaceRepos exercises the repo-centric model: one workspace
// including two repos, each with its own Clawkfile declaring forwards and
// setup. Expects one Phase per repo, each carrying its Setup inline.
func TestRunWorkspaceRepos(t *testing.T) {
	_, _ = setupTest(t)

	dir := t.TempDir()
	k8s := filepath.Join(dir, "k8s-deploy")
	mono := filepath.Join(dir, "monorepo")
	for _, r := range []string{k8s, mono} {
		require.NoError(t, os.MkdirAll(r, 0o755))
		gitInit(t, r)
	}

	require.NoError(t, os.WriteFile(filepath.Join(k8s, "clawk.mod"),
		[]byte(`sandbox (
    forwards (
        8080
    )
    on up (
        "make deps"
    )
)
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(mono, "clawk.mod"),
		[]byte(`sandbox (
    forwards (
        3000
    )
    on up (
        "pnpm install"
    )
)
`), 0o644))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "clawk.mod"),
		[]byte(`sandbox (
    vm (
        provider vz
    )
    includes (
        ./k8s-deploy
        ./monorepo
    )
)
`), 0o644))

	_, err := executeCommand(
		"work", filepath.Join(dir, "clawk.mod"), "INFRA-42", "--bare",
	)
	require.NoError(t, err)

	sb, err := store.Load("INFRA-42")
	require.NoError(t, err, "sandbox not created")
	require.Len(t, sb.Phases, 2)

	byName := make(map[string]*config.Phase)
	for i := range sb.Phases {
		byName[filepath.Base(sb.Phases[i].Repo)] = &sb.Phases[i]
	}
	if p := byName["k8s-deploy"]; p == nil ||
		len(p.Setup) != 1 || p.Setup[0] != "make deps" {
		t.Errorf("k8s-deploy phase setup wrong: %+v", p)
	}
	if p := byName["monorepo"]; p == nil ||
		len(p.Setup) != 1 || p.Setup[0] != "pnpm install" {
		t.Errorf("monorepo phase setup wrong: %+v", p)
	}

	fwd := make(map[int]bool)
	for _, f := range sb.Forwards {
		fwd[f.HostPort] = true
	}
	if !fwd[8080] || !fwd[3000] {
		t.Errorf("forwards = %v, want 8080 and 3000", sb.Forwards)
	}
}

func TestRunOnlyFiltersRepos(t *testing.T) {
	_, _ = setupTest(t)

	dir := t.TempDir()
	for _, sub := range []string{"k8s-deploy", "monorepo"} {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, sub), 0o755))
		gitInit(t, filepath.Join(dir, sub))
	}
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "clawk.mod"),
		[]byte("sandbox (\n  includes (\n    ./k8s-deploy\n    ./monorepo\n  )\n)\n"), 0o644))

	_, err := executeCommand(
		"work", filepath.Join(dir, "clawk.mod"), "BR",
		"--only", "monorepo", "--bare",
	)
	require.NoError(t, err)
	sb, _ := store.Load("BR")
	require.Len(t, sb.Phases, 1)
	if filepath.Base(sb.Phases[0].Repo) != "monorepo" {
		t.Errorf("wrong phase picked: %+v", sb.Phases[0])
	}
}

// TestRunRegistersFilePolicies: `policy` blocks in the loaded clawk.mod land
// in the policy store before the sandbox composes, so its `use` references
// resolve at up/reload; the mapping renders refresh as a duration string.
func TestRunRegistersFilePolicies(t *testing.T) {
	s, _ := setupTest(t)

	dir := t.TempDir()
	repo := filepath.Join(dir, "svc")
	gitInit(t, repo)
	require.NoError(t, os.WriteFile(filepath.Join(repo, "clawk.mod"), []byte(`sandbox (
    network (
        use default corp
    )
)

policy corp (
    allow ip 10.20.0.0/16
    deny  telemetry.corp.com
    source "https://example.com/list.txt"
    refresh 12h
)
`), 0o644))

	_, err := executeCommand("work", filepath.Join(repo, "clawk.mod"), "POL-1", "--bare")
	require.NoError(t, err)

	p, err := s.LoadPolicy("corp")
	require.NoError(t, err, "policy block must be registered at create")
	require.Equal(t, []string{"10.20.0.0/16"}, p.AllowIPs)
	require.Equal(t, []string{"telemetry.corp.com"}, p.DenyDomains)
	require.Equal(t, "https://example.com/list.txt", p.Source)
	require.Equal(t, "12h0m0s", p.Refresh)

	sb, err := s.Load("POL-1")
	require.NoError(t, err)
	require.Equal(t, []string{"default", "corp"}, sb.Network.Use)
}

// TestRunRejectsReservedPolicyName: a file policy named "default" collides
// with the builtin and must fail the create loudly, not shadow it.
func TestRunRejectsReservedPolicyName(t *testing.T) {
	_, _ = setupTest(t)

	dir := t.TempDir()
	repo := filepath.Join(dir, "svc")
	gitInit(t, repo)
	require.NoError(t, os.WriteFile(filepath.Join(repo, "clawk.mod"),
		[]byte("sandbox (\n)\n\npolicy default (\n    allow example.com\n)\n"), 0o644))

	_, err := executeCommand("work", filepath.Join(repo, "clawk.mod"), "POL-2", "--bare")
	require.Error(t, err)
	require.Contains(t, err.Error(), "reserved")
}

// TestRunPortConflict verifies that two repos both exposing the same host
// port surface as an error at sandbox creation time. We don't want silent
// dedup here — divergent forwards mean one service will fail to bind.
func TestRunPortConflict(t *testing.T) {
	_, _ = setupTest(t)

	dir := t.TempDir()
	for _, sub := range []string{"a", "b"} {
		r := filepath.Join(dir, sub)
		require.NoError(t, os.MkdirAll(r, 0o755))
		gitInit(t, r)
	}
	// Same host port, different guest ports — a genuine conflict.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a", "clawk.mod"),
		[]byte("sandbox (\n  forwards (\n    3000:80\n  )\n)\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b", "clawk.mod"),
		[]byte("sandbox (\n  forwards (\n    3000:8080\n  )\n)\n"), 0o644))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "clawk.mod"),
		[]byte("sandbox (\n  includes (\n    ./a\n    ./b\n  )\n)\n"), 0o644))
	_, err := executeCommand(
		"work", filepath.Join(dir, "clawk.mod"), "BR", "--bare",
	)
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "host port 3000"))
}

// TestRunProviderConflict: two repos disagree on provider and the workspace
// doesn't pick one.
func TestRunProviderConflict(t *testing.T) {
	_, _ = setupTest(t)

	dir := t.TempDir()
	for _, sub := range []string{"x", "y"} {
		r := filepath.Join(dir, sub)
		require.NoError(t, os.MkdirAll(r, 0o755))
		gitInit(t, r)
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "x", "clawk.mod"),
		[]byte("sandbox (\n  vm (\n    provider vz\n  )\n)\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "y", "clawk.mod"),
		[]byte("sandbox (\n  vm (\n    provider firecracker\n  )\n)\n"), 0o644))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "clawk.mod"),
		[]byte("sandbox (\n  includes (\n    ./x\n    ./y\n  )\n)\n"), 0o644))
	_, err := executeCommand(
		"work", filepath.Join(dir, "clawk.mod"), "BR", "--bare",
	)
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "provider conflict"))
}

// TestRunWithProfile: overlay adds allow entries and they land in the
// sandbox's AllowedDomains. The persisted Sandbox.Profile tracks which
// overlay is active.
func TestRunWithProfile(t *testing.T) {
	_, _ = setupTest(t)

	dir := t.TempDir()
	svc := filepath.Join(dir, "svc")
	require.NoError(t, os.MkdirAll(svc, 0o755))
	gitInit(t, svc)

	require.NoError(t, os.WriteFile(filepath.Join(svc, "clawk.mod"),
		[]byte("sandbox (\n  network (\n    allow *.dev.myco.com\n  )\n)\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(svc, "clawk.mod.investigation"),
		[]byte("sandbox (\n  network (\n    allow *.kube.internal.myco.com\n  )\n)\n"), 0o644))

	require.NoError(t, os.WriteFile(filepath.Join(dir, "clawk.mod"),
		[]byte("sandbox (\n  vm (\n    provider vz\n  )\n  includes (\n    ./svc\n  )\n)\n"), 0o644))

	_, err := executeCommand(
		"work", filepath.Join(dir, "clawk.mod"), "INV-1",
		"--profile", "investigation", "--bare",
	)
	require.NoError(t, err)
	sb, _ := store.Load("INV-1")
	require.Equal(t, "investigation", sb.Profile)
	if !containsStr(sb.Network.Block(config.BlockOriginMod).AllowDomains, "*.dev.myco.com") {
		t.Errorf("base domain missing: %v", sb.Network.Block(config.BlockOriginMod).AllowDomains)
	}
	if !containsStr(sb.Network.Block(config.BlockOriginMod).AllowDomains, "*.kube.internal.myco.com") {
		t.Errorf("overlay domain missing: %v", sb.Network.Block(config.BlockOriginMod).AllowDomains)
	}
}

func TestRunMissingProfileErrors(t *testing.T) {
	_, _ = setupTest(t)
	dir := t.TempDir()
	svc := filepath.Join(dir, "svc")
	require.NoError(t, os.MkdirAll(svc, 0o755))
	gitInit(t, svc)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "clawk.mod"),
		[]byte("sandbox (\n  includes (\n    ./svc\n  )\n)\n"), 0o644))
	_, err := executeCommand(
		"work", filepath.Join(dir, "clawk.mod"), "X",
		"--profile", "ghost", "--bare",
	)
	require.Error(t, err)
}

// TestWorkBareSkipsBoot: --bare prepares the sandbox (phases written,
// worktree created) but neither boots the VM nor attaches a runner.
// The mock provider records every Start call, so an empty mock.Started
// is the load-bearing assertion.
func TestWorkBareSkipsBoot(t *testing.T) {
	_, mock := setupTest(t)

	dir := t.TempDir()
	repo := filepath.Join(dir, "r")
	gitInit(t, repo)
	tmplPath := filepath.Join(dir, "clawk.mod")
	require.NoError(t, os.WriteFile(tmplPath,
		[]byte(fmt.Sprintf("sandbox (\nvm (\nprovider vz\n)\nincludes (\n  %s\n)\n)\n", repo)),
		0o644))

	out, err := executeCommand("work", tmplPath, "BARE-1", "--bare")
	require.NoError(t, err, "work --bare failed: %v\n%s", err, out)

	sb, err := store.Load("BARE-1")
	require.NoError(t, err, "sandbox not created")
	require.Len(t, sb.Phases, 1)
	require.Len(t, mock.Started, 0, "--bare should not start VM, got %v", mock.Started)
}

// TestWorkCwdGitRepoFallback: in a git repo with no clawk.mod,
// `clawk work <branch>` synthesises a single-repo workspace
// from the cwd's repo. One phase, branch matches, sandbox uses default
// allow list (no extras), and a stderr note tells the user defaults
// were applied.
func TestWorkCwdGitRepoFallback(t *testing.T) {
	_, _ = setupTest(t)

	dir := t.TempDir()
	repo := filepath.Join(dir, "myrepo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	gitInit(t, repo)
	t.Chdir(repo)

	// Capture stderr (cobra writes errs there, our note uses os.Stderr
	// directly so we redirect the FD).
	r, w, err := os.Pipe()
	require.NoError(t, err)
	origStderr := os.Stderr
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = origStderr })

	_, err = executeCommand("work", "FEAT-1", "--bare")
	require.NoError(t, err)
	w.Close()
	stderrBuf := make([]byte, 4096)
	n, _ := r.Read(stderrBuf)
	noteOut := string(stderrBuf[:n])

	sb, err := store.Load("FEAT-1")
	require.NoError(t, err, "sandbox not created")
	require.Len(t, sb.Phases, 1)
	require.Equal(t, "FEAT-1", sb.Phases[0].Branch)
	resolvedRepo, _ := filepath.EvalSymlinks(repo)
	if got, _ := filepath.EvalSymlinks(sb.Phases[0].Repo); got != resolvedRepo {
		t.Errorf("phase repo = %s, want %s", got, resolvedRepo)
	}
	require.Len(t, sb.Forwards, 0, "expected no forwards, got %v", sb.Forwards)

	if !strings.Contains(noteOut, "no clawk.mod") {
		t.Errorf("expected fallback note on stderr, got: %q", noteOut)
	}
}

// TestWorkCwdFallbackProfileRejected: --profile only makes sense with
// overlay files to merge into. The cwd-fallback has no base config, so
// asking for a profile must error rather than silently ignore the flag.
func TestWorkCwdFallbackProfileRejected(t *testing.T) {
	_, _ = setupTest(t)

	repo := filepath.Join(t.TempDir(), "r")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	gitInit(t, repo)
	t.Chdir(repo)

	_, err := executeCommand("work", "FEAT-1", "--profile", "ghost", "--bare")
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "--profile requires"))
}

// TestWorkCwdFallbackNonRepoErrors: cwd is neither a git repo nor a
// clawk template — error names the missing pieces so the user knows
// what to add.
func TestWorkCwdFallbackNonRepoErrors(t *testing.T) {
	_, _ = setupTest(t)
	dir := t.TempDir()
	t.Chdir(dir)

	_, err := executeCommand("work", "FEAT-1", "--bare")
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "not a git repo"))
}

func TestSanitiseName(t *testing.T) {
	cases := map[string]string{
		"INFRA-1234":    "INFRA-1234",
		"feature/foo":   "feature-foo",
		"release/1.2.3": "release-1.2.3",
		"x@y":           "x-y",
	}
	for in, want := range cases {
		if got := sanitiseName(in); got != want {
			t.Errorf("sanitiseName(%q) = %q, want %q", in, got, want)
		}
	}
}

func containsStr(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}
