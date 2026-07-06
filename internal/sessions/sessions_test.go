package sessions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// encoded is a stand-in for Claude Code's cwd-encoded project folder. The
// exact value doesn't matter to these tests — only that transcripts live
// under projects/<encoded>/ and memory under projects/<encoded>/memory/.
const encoded = "-home-agent-workspace-demo"

func TestProjectIDStableAndOrderIndependent(t *testing.T) {
	a := ProjectID([]string{"/repos/foo", "/repos/bar"})
	b := ProjectID([]string{"/repos/bar", "/repos/foo", "/repos/bar"})
	require.Equal(t, a, b, "ProjectID not order/dup invariant")
	got := ProjectID([]string{"/repos/foo"})
	require.True(t, strings.HasPrefix(got, "foo-"), "single-repo id = %q, want foo-<hash> prefix", got)
	got = ProjectID([]string{"/repos/foo", "/repos/bar"})
	require.True(t, strings.HasPrefix(got, "ws-"), "multi-repo id = %q, want ws-<hash> prefix", got)
}

// writeTranscript drops a fake transcript file for the project.
func writeTranscript(t *testing.T, claudeDir, name, body string) {
	t.Helper()
	dir := filepath.Join(claudeDir, "projects", encoded)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644))
}

func writeMemory(t *testing.T, claudeDir, body string) {
	t.Helper()
	dir := filepath.Join(claudeDir, "projects", encoded, "memory")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte(body), 0o644))
}

func exists(claudeDir, rel string) bool {
	_, err := os.Stat(filepath.Join(claudeDir, "projects", encoded, rel))
	return err == nil
}

// runSandbox simulates one VM lifecycle for a project under the explicit-merge
// model: prepare the working tree, let the caller mutate it (write
// transcripts), then preserve AND explicitly publish into main — i.e. the user
// ran `clawk sessions merge`.
func runSandbox(t *testing.T, historyRoot, projectID, name string, mutate func(claudeDir string)) string {
	t.Helper()
	bare, err := EnsureBare(historyRoot, projectID)
	require.NoError(t, err, "EnsureBare")
	claudeDir := filepath.Join(t.TempDir(), name, "claude")
	branch := BranchFor(name)
	require.NoError(t, Prepare(bare, claudeDir, branch), "Prepare(%s)", name)
	if mutate != nil {
		mutate(claudeDir)
	}
	publish(t, bare, claudeDir, name)
	return claudeDir
}

// publish preserves the sandbox branch and folds it into main — the explicit
// `clawk sessions merge` step that runSandbox and several tests rely on.
func publish(t *testing.T, bare, claudeDir, name string) {
	t.Helper()
	branch := BranchFor(name)
	require.NoError(t, Preserve(bare, claudeDir, branch), "Preserve(%s)", name)
	require.NoError(t, Publish(bare, branch), "Publish(%s)", name)
}

// TestCrossSandboxAccumulation is the core guarantee: distinct sandboxes for
// the same project each contribute transcripts, and a later sandbox boots
// with everything that came before.
func TestCrossSandboxAccumulation(t *testing.T) {
	hr := t.TempDir()
	pid := ProjectID([]string{"/repos/demo"})

	// Sandbox A: first ever for this project. Nothing to seed.
	runSandbox(t, hr, pid, "alpha", func(cd string) {
		writeTranscript(t, cd, "uuid-a.jsonl", "session A\n")
		writeMemory(t, cd, "- fact a\n")
	})

	// Sandbox B: fresh dir; must seed A's history, then add its own.
	bare := barePath(hr, pid)
	cdB := filepath.Join(t.TempDir(), "beta", "claude")
	require.NoError(t, Prepare(bare, cdB, BranchFor("beta")), "Prepare(beta)")
	require.True(t, exists(cdB, "uuid-a.jsonl"), "sandbox B did not seed A's transcript from main")
	require.True(t, exists(cdB, "memory/MEMORY.md"), "sandbox B did not seed A's memory from main")
	writeTranscript(t, cdB, "uuid-b.jsonl", "session B\n")
	publish(t, bare, cdB, "beta")

	// Sandbox C: should see both A and B.
	cdC := filepath.Join(t.TempDir(), "gamma", "claude")
	require.NoError(t, Prepare(bare, cdC, BranchFor("gamma")), "Prepare(gamma)")
	require.True(t, exists(cdC, "uuid-a.jsonl") && exists(cdC, "uuid-b.jsonl"),
		"sandbox C missing prior transcripts: a=%v b=%v",
		exists(cdC, "uuid-a.jsonl"), exists(cdC, "uuid-b.jsonl"))
}

// TestMemoryUnionMerge proves diverging MEMORY.md edits on two branches both
// survive the merge into main (the union driver in .gitattributes).
func TestMemoryUnionMerge(t *testing.T) {
	hr := t.TempDir()
	pid := ProjectID([]string{"/repos/demo"})

	// Seed main with a base memory line.
	runSandbox(t, hr, pid, "base", func(cd string) {
		writeMemory(t, cd, "- fact base\n")
	})
	bare := barePath(hr, pid)

	// Two sandboxes branch off the same main and edit memory differently,
	// without merging until both have diverged.
	cd1 := filepath.Join(t.TempDir(), "one", "claude")
	require.NoError(t, Prepare(bare, cd1, BranchFor("one")))
	writeMemory(t, cd1, "- fact base\n- fact one\n")

	cd2 := filepath.Join(t.TempDir(), "two", "claude")
	require.NoError(t, Prepare(bare, cd2, BranchFor("two")))
	writeMemory(t, cd2, "- fact base\n- fact two\n")

	publish(t, bare, cd1, "one")
	// Publishing the second branch must auto-resolve the MEMORY.md conflict
	// against main via the union driver.
	publish(t, bare, cd2, "two")

	// A fresh sandbox should see all three lines.
	cd3 := filepath.Join(t.TempDir(), "three", "claude")
	require.NoError(t, Prepare(bare, cd3, BranchFor("three")))
	mem, err := os.ReadFile(filepath.Join(cd3, "projects", encoded, "memory", "MEMORY.md"))
	require.NoError(t, err)
	for _, want := range []string{"fact base", "fact one", "fact two"} {
		require.True(t, strings.Contains(string(mem), want), "merged MEMORY.md missing %q:\n%s", want, mem)
	}
}

// TestMigrateExistingStateDir covers a pre-feature sandbox: its state dir is
// already populated with transcripts and secrets but is NOT a git repo.
// Preserve must initialize the working tree and capture the existing
// transcripts on the branch (the migration), and an explicit Publish folds
// them into main — all while leaving secrets untracked.
func TestMigrateExistingStateDir(t *testing.T) {
	hr := t.TempDir()
	pid := ProjectID([]string{"/repos/demo"})
	bare, err := EnsureBare(hr, pid)
	require.NoError(t, err)

	// Simulate a legacy sandbox: a populated, non-git claude dir.
	legacy := filepath.Join(t.TempDir(), "legacy", "claude")
	writeTranscript(t, legacy, "old.jsonl", "pre-existing session\n")
	writeMemory(t, legacy, "- legacy fact\n")
	require.NoError(t, os.WriteFile(filepath.Join(legacy, ".credentials.json"), []byte("secret"), 0o600))
	_, err = os.Stat(filepath.Join(legacy, ".git"))
	require.True(t, os.IsNotExist(err), "precondition: legacy dir should not be a git repo")

	// Migrate-on-destroy preserves the branch; an explicit merge publishes it.
	publish(t, bare, legacy, "legacy")

	// A fresh sandbox for the same project must now inherit the migrated
	// transcript + memory, and the secret must not have been published.
	fresh := filepath.Join(t.TempDir(), "fresh", "claude")
	require.NoError(t, Prepare(bare, fresh, BranchFor("fresh")))
	require.True(t, exists(fresh, "old.jsonl") && exists(fresh, "memory/MEMORY.md"),
		"migration lost data: transcript=%v memory=%v",
		exists(fresh, "old.jsonl"), exists(fresh, "memory/MEMORY.md"))
	tracked, err := git(bare, "ls-tree", "-r", "--name-only", "main")
	require.NoError(t, err)
	require.False(t, strings.Contains(tracked, ".credentials.json"), "migration leaked a secret into main:\n%s", tracked)
}

// TestSeedRestoresMtimeOrder proves that after a seed-from-main, transcript
// mtimes reflect each session's real last-activity time (not the checkout
// instant), so Claude Code's mtime-ordered resume list stays correct.
func TestSeedRestoresMtimeOrder(t *testing.T) {
	hr := t.TempDir()
	pid := ProjectID([]string{"/repos/demo"})

	// Seed main with two sessions whose last events are days apart.
	runSandbox(t, hr, pid, "seedy", func(cd string) {
		writeJSONL(t, cd, "older.jsonl", "2026-06-10T09:00:00Z")
		writeJSONL(t, cd, "newer.jsonl", "2026-06-14T18:30:00Z")
	})

	// Fresh sandbox seeds both from main via git (which would normally stamp
	// both with one checkout time).
	bare := barePath(hr, pid)
	cd := filepath.Join(t.TempDir(), "fresh", "claude")
	require.NoError(t, Prepare(bare, cd, BranchFor("fresh")))

	older := mtime(t, cd, "older.jsonl")
	newer := mtime(t, cd, "newer.jsonl")
	require.True(t, newer.After(older), "resume order wrong: newer (%s) not after older (%s) — git mtime not restored", newer, older)
	want, _ := time.Parse(time.RFC3339, "2026-06-14T18:30:00Z")
	require.True(t, newer.Equal(want), "mtime = %s, want session last-event %s", newer, want)
}

func writeJSONL(t *testing.T, claudeDir, name, lastTS string) {
	t.Helper()
	// A header line (no timestamp) + an event line carrying the timestamp,
	// mirroring the real transcript shape.
	body := `{"type":"summary","sessionId":"x"}` + "\n" +
		`{"type":"user","timestamp":"` + lastTS + `"}` + "\n"
	writeTranscript(t, claudeDir, name, body)
}

func mtime(t *testing.T, claudeDir, name string) time.Time {
	t.Helper()
	fi, err := os.Stat(filepath.Join(claudeDir, "projects", encoded, name))
	require.NoError(t, err)
	return fi.ModTime().UTC()
}

// TestRefreshLongLivedSandbox models a `clawk here` sandbox kept up for a long
// time under the explicit-merge model. Refresh on attach must (a) pull
// sessions PUBLISHED elsewhere into the live working tree, but (b) NOT
// auto-publish this VM's own sessions to main — that stays an explicit step.
func TestRefreshLongLivedSandbox(t *testing.T) {
	hr := t.TempDir()
	pid := ProjectID([]string{"/repos/demo"})
	bare, _ := EnsureBare(hr, pid)

	// The long-lived sandbox boots once and writes a session, then attaches
	// (Refresh): its branch is preserved but nothing is published to main.
	here := filepath.Join(t.TempDir(), "here", "claude")
	require.NoError(t, Prepare(bare, here, BranchFor("here")))
	writeJSONL(t, here, "here.jsonl", "2026-06-10T09:00:00Z")
	require.NoError(t, Refresh(bare, here, BranchFor("here")), "here initial refresh")
	require.False(t, bareHasMain(bare), "Refresh must not publish to main on its own")

	// A separate, short-lived sandbox runs and EXPLICITLY publishes a session.
	runSandbox(t, hr, pid, "ticket", func(cd string) {
		writeJSONL(t, cd, "ticket.jsonl", "2026-06-14T18:30:00Z")
	})

	// The long-lived sandbox never rebooted. A later attach (Refresh) must
	// pull the published ticket session into its live working tree, ordered
	// correctly — and still must not have auto-published its own.
	require.False(t, exists(here, "ticket.jsonl"), "precondition: long-lived sandbox should not yet have the new session")
	require.NoError(t, Refresh(bare, here, BranchFor("here")), "here refresh")
	require.True(t, exists(here, "ticket.jsonl"), "long-lived sandbox did not learn about the published session on attach")
	tracked, _ := git(bare, "ls-tree", "-r", "--name-only", "main")
	require.False(t, strings.Contains(tracked, "here.jsonl"), "Refresh auto-published the VM's own session (should be explicit):\n%s", tracked)
	// mtimes reflect real session times, not the pull instant.
	require.True(t, mtime(t, here, "ticket.jsonl").After(mtime(t, here, "here.jsonl")), "refresh scrambled mtime order")

	// Explicit publish lands the long-lived sandbox's own session in main.
	require.NoError(t, Publish(bare, BranchFor("here")), "publish here")
	tracked, _ = git(bare, "ls-tree", "-r", "--name-only", "main")
	require.True(t, strings.Contains(tracked, "here.jsonl"), "explicit publish did not land the session in main:\n%s", tracked)
}

// TestSecretsNeverTracked guards the §4.4 invariant: secrets and host-seeded
// config dropped into the state dir never enter the history repo.
func TestSecretsNeverTracked(t *testing.T) {
	hr := t.TempDir()
	pid := ProjectID([]string{"/repos/demo"})
	bare, err := EnsureBare(hr, pid)
	require.NoError(t, err)
	cd := filepath.Join(t.TempDir(), "s", "claude")
	require.NoError(t, Prepare(bare, cd, BranchFor("s")))
	// Things the provider seeds alongside the tracked subtree.
	for _, f := range []string{".credentials.json", ".claude.json", "settings.json", "settings.local.json"} {
		require.NoError(t, os.WriteFile(filepath.Join(cd, f), []byte("secret"), 0o600))
	}
	require.NoError(t, os.MkdirAll(filepath.Join(cd, "skills"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cd, "skills", "x.md"), []byte("mounted"), 0o644))
	writeTranscript(t, cd, "uuid.jsonl", "real session\n")

	out, err := git(cd, "add", "-A")
	require.NoError(t, err, out)
	tracked, err := git(cd, "ls-files")
	require.NoError(t, err)
	for _, leak := range []string{".credentials.json", ".claude.json", "settings.json", "settings.local.json", "skills/x.md"} {
		require.False(t, strings.Contains(tracked, leak), "secret/host file leaked into history: %q\ntracked:\n%s", leak, tracked)
	}
	require.True(t, strings.Contains(tracked, "uuid.jsonl"), "transcript not tracked; tracked:\n%s", tracked)
}

// TestRecreateContinuity: reusing the same state dir (a destroy+recreate of
// the same sandbox name, where state survives) keeps working and does not
// lose the prior tip.
func TestRecreateContinuity(t *testing.T) {
	hr := t.TempDir()
	pid := ProjectID([]string{"/repos/demo"})
	bare, err := EnsureBare(hr, pid)
	require.NoError(t, err)
	cd := filepath.Join(t.TempDir(), "keep", "claude")
	br := BranchFor("keep")

	require.NoError(t, Prepare(bare, cd, br))
	writeTranscript(t, cd, "uuid-1.jsonl", "first\n")
	require.NoError(t, Preserve(bare, cd, br))

	// Recreate: same dir, same branch. Prior transcript must still be there.
	require.NoError(t, Prepare(bare, cd, br), "re-prepare")
	require.True(t, exists(cd, "uuid-1.jsonl"), "lost prior transcript on recreate")
	writeTranscript(t, cd, "uuid-2.jsonl", "second\n")
	require.NoError(t, Preserve(bare, cd, br))
	require.True(t, exists(cd, "uuid-1.jsonl") && exists(cd, "uuid-2.jsonl"), "recreate dropped a transcript")
}
