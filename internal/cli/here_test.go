package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/stretchr/testify/require"
)

// withTempStore points the package-level store at a temp directory so
// deriveHereSandboxName has a clean state to probe. Restores the old
// store on cleanup.
func withTempStore(t *testing.T) {
	t.Helper()
	prev := store
	store = config.NewStoreAt(t.TempDir())
	t.Cleanup(func() { store = prev })
}

func TestDeriveHereSandboxName_FreshDir(t *testing.T) {
	withTempStore(t)

	cwd := t.TempDir()
	got, err := deriveHereSandboxName(cwd)
	require.NoError(t, err)
	base := filepath.Base(cwd)
	// A fresh dir gets a clean key — the basename, with no hash suffix
	// (the suffix only appears on a basename collision).
	require.Equal(t, base, got, "name should be clean basename (no prefix, no suffix)")
}

func TestDeriveHereSandboxName_Idempotent(t *testing.T) {
	withTempStore(t)

	cwd := t.TempDir()
	name, err := deriveHereSandboxName(cwd)
	require.NoError(t, err)
	// Save a sandbox at that name that matches the cwd.
	sb := &config.Sandbox{
		Name:      name,
		Provider:  config.ProviderVZ,
		Phases:    []config.Phase{{Worktree: resolved(t, cwd), InPlace: true}},
		CreatedAt: time.Now(),
	}
	require.NoError(t, store.Save(sb))
	// Second call for the same cwd should return the same name — no
	// new hash suffix just because the sandbox already exists.
	got, err := deriveHereSandboxName(cwd)
	require.NoError(t, err)
	require.Equal(t, name, got, "same dir should reuse existing name")
}

func TestDeriveHereSandboxName_BasenameCollision(t *testing.T) {
	withTempStore(t)

	// Two distinct directories sharing a basename.
	parent := t.TempDir()
	a := filepath.Join(parent, "a", "shared")
	b := filepath.Join(parent, "b", "shared")
	require.NoError(t, os.MkdirAll(a, 0o755))
	require.NoError(t, os.MkdirAll(b, 0o755))

	// Register sandbox for a at the plain name.
	nameA, err := deriveHereSandboxName(a)
	require.NoError(t, err)
	require.Equal(t, "shared", nameA, "first derive got unexpected key: %q (want clean %q)", nameA, "shared")
	require.NoError(t, store.Save(&config.Sandbox{
		Name:      nameA,
		Phases:    []config.Phase{{Worktree: resolved(t, a), InPlace: true}},
		CreatedAt: time.Now(),
	}))

	// b derives a distinct name — must not collide with a's.
	nameB, err := deriveHereSandboxName(b)
	require.NoError(t, err)
	require.NotEqual(t, nameA, nameB, "collision: both dirs resolved to %q", nameA)
	require.True(t, strings.HasPrefix(nameB, "shared_"), "nameB = %q, want a disambiguated %q key", nameB, "shared_")
}

// resolved mirrors the symlink-resolution step deriveHereSandboxName
// performs, so test assertions about stored Worktree paths match what
// matchesHereSandbox will compare against.
func resolved(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r
	}
	return abs
}

// TestRollbackFailedCreate verifies that rolling back a never-booted cwd-
// sandbox removes the record (so a retry re-reads clawk.mod) and tears down
// the VM. This is the recovery for a create that failed on a bad kernel/image
// ref — without it the snapshot-at-create record would pin the broken config.
func TestRollbackFailedCreate(t *testing.T) {
	s, mock := setupTest(t)
	sb := &config.Sandbox{
		Name:    "rb-test",
		VMState: config.VMStateStopped,
		Phases:  []config.Phase{{Repo: "/tmp/r", Worktree: "/tmp/r", InPlace: true}},
	}
	require.NoError(t, s.Save(sb))
	require.True(t, s.Exists("rb-test"))

	rollbackFailedCreate(sb)

	require.False(t, s.Exists("rb-test"), "record should be gone so a retry recomposes from clawk.mod")
	require.Contains(t, mock.Destroyed, "rb-test", "VM should be torn down on rollback")
}

// TestHereSandboxPicksUpIdleTimeout: the cwd-mode create path must copy every
// vm ( ... ) field from clawk.mod into the record. idle_timeout was the one
// field it missed when idle-stop landed, which silently pinned every cwd
// sandbox to the 30m default no matter what the clawk.mod said.
func TestHereSandboxPicksUpIdleTimeout(t *testing.T) {
	withTempStore(t)

	dir := t.TempDir()
	gitInit(t, dir) // the standalone clawk.mod loader requires a git repo
	mod := "sandbox (\n    vm (\n        idle_timeout 2m\n    )\n)\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "clawk.mod"), []byte(mod), 0o644))

	sb, created, err := loadOrCreateHereSandbox("idle-here", dir)
	require.NoError(t, err)
	require.True(t, created)
	require.EqualValues(t, 120, sb.IdleTimeoutSec,
		"idle_timeout from clawk.mod must be snapshotted at create")
}

// TestHereSandboxRegistersFilePolicies: the cwd-mode create path registers
// `policy` blocks from cwd's clawk.mod, mirroring the workspace create path.
func TestHereSandboxRegistersFilePolicies(t *testing.T) {
	withTempStore(t)

	dir := t.TempDir()
	gitInit(t, dir) // the standalone clawk.mod loader requires a git repo
	mod := `sandbox (
    network (
        use default here-pol
    )
)

policy here-pol (
    allow internal.example.com
)
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "clawk.mod"), []byte(mod), 0o644))

	sb, created, err := loadOrCreateHereSandbox("pol-here", dir)
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, []string{"default", "here-pol"}, sb.Network.Use)

	p, err := store.LoadPolicy("here-pol")
	require.NoError(t, err, "policy block must be registered at create")
	require.Equal(t, []string{"internal.example.com"}, p.AllowDomains)
}

// TestHereSandboxSymlinkedCwd: creating from a symlinked working directory
// (macOS's /tmp -> /private/tmp is the everyday case) and later inferring
// from the same raw path must resolve to ONE record. Before hereCWDAndName
// canonicalised, the create anchored the raw path while lookups matched the
// resolved one — so `clawk down` invented "name_<hash>" and found nothing.
func TestHereSandboxSymlinkedCwd(t *testing.T) {
	withTempStore(t)

	real := filepath.Join(t.TempDir(), "idle-test")
	require.NoError(t, os.Mkdir(real, 0o755))
	link := filepath.Join(t.TempDir(), "idle-test")
	require.NoError(t, os.Symlink(real, link))

	// Simulate bare `clawk` run with a symlinked $PWD: canonicalise as
	// hereCWDAndName now does, then create.
	cwd := canonicalPath(link)
	name1, err := deriveHereSandboxName(cwd)
	require.NoError(t, err)
	sb, created, err := loadOrCreateHereSandbox(name1, cwd)
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, canonicalPath(link), sb.Anchor, "anchor must be canonical")

	// A later command inferring from the RAW symlink path must land on the
	// same record, not a hash-suffixed orphan.
	name2, err := deriveHereSandboxName(link)
	require.NoError(t, err)
	require.Equal(t, name1, name2)
}

// TestHereSandboxHealsRawAnchor: a record written before canonicalisation
// (raw symlink path as Anchor/Worktree) must still match — the matcher
// resolves both sides, so existing sandboxes heal without migration.
func TestHereSandboxHealsRawAnchor(t *testing.T) {
	withTempStore(t)

	real := filepath.Join(t.TempDir(), "proj")
	require.NoError(t, os.Mkdir(real, 0o755))
	link := filepath.Join(t.TempDir(), "proj")
	require.NoError(t, os.Symlink(real, link))

	require.NoError(t, store.Save(&config.Sandbox{
		Name:    "proj",
		Anchor:  link, // raw, pre-fix form
		VMState: config.VMStateStopped,
		Phases: []config.Phase{{
			Repo: link, Worktree: link, InPlace: true,
			Status: config.PhaseStatusActive,
		}},
	}))

	got, err := deriveHereSandboxName(link)
	require.NoError(t, err)
	require.Equal(t, "proj", got, "pre-fix raw-anchored record must keep matching")
}
