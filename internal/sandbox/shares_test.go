package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/stretchr/testify/require"
)

func TestToolchainCacheSharesEmptyCacheDir(t *testing.T) {
	if got := ToolchainCacheShares(""); got != nil {
		t.Fatalf("ToolchainCacheShares(\"\") = %v, want nil", got)
	}
}

func TestToolchainCacheShares(t *testing.T) {
	cacheDir := t.TempDir()

	want := []HostShare{
		{
			HostPath:  filepath.Join(cacheDir, "gomodcache"),
			Tag:       "go_modcache",
			GuestPath: GuestHome + "/go/pkg/mod",
		},
		// Go build cache (~/.cache/go-build) is deliberately NOT shared —
		// it stays per-VM (not concurrent-safe across builds, golang/go#43645,
		// and the heaviest virtio-fs fd churner). See ToolchainCacheShares.
		{
			HostPath:  filepath.Join(cacheDir, "cargo-registry-index"),
			Tag:       "cargo_registry_index",
			GuestPath: GuestHome + "/.cargo/registry/index",
		},
		{
			HostPath:  filepath.Join(cacheDir, "cargo-registry-cache"),
			Tag:       "cargo_registry_cache",
			GuestPath: GuestHome + "/.cargo/registry/cache",
		},
		{
			HostPath:  filepath.Join(cacheDir, "cargo-git-db"),
			Tag:       "cargo_git_db",
			GuestPath: GuestHome + "/.cargo/git/db",
		},
	}

	got := ToolchainCacheShares(cacheDir)
	require.Len(t, got, len(want))
	for i, w := range want {
		g := got[i]
		if g.HostPath != w.HostPath {
			t.Errorf("[%d] HostPath = %s, want %s", i, g.HostPath, w.HostPath)
		}
		if g.Tag != w.Tag {
			t.Errorf("[%d] Tag = %s, want %s", i, g.Tag, w.Tag)
		}
		if g.GuestPath != w.GuestPath {
			t.Errorf("[%d] GuestPath = %s, want %s", i, g.GuestPath, w.GuestPath)
		}
		if g.ReadOnly {
			t.Errorf("[%d] ReadOnly = true, want false (caches must be RW)", i)
		}
		// Host dir must exist — virtiofs refuses missing source paths.
		info, err := os.Stat(g.HostPath)
		if err != nil {
			t.Errorf("[%d] stat %s: %v", i, g.HostPath, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("[%d] %s exists but is not a directory", i, g.HostPath)
		}
	}
}

func TestToolchainCacheSharesIdempotent(t *testing.T) {
	cacheDir := t.TempDir()

	first := ToolchainCacheShares(cacheDir)
	second := ToolchainCacheShares(cacheDir)
	require.Len(t, first, len(second), "non-idempotent call lengths differ")
	for i := range first {
		if first[i] != second[i] {
			t.Errorf("[%d] mismatch on second call: %+v vs %+v",
				i, first[i], second[i])
		}
	}
}

func TestToolchainCacheSharesUniqueTags(t *testing.T) {
	// Tags must be unique across all shares mounted on a single VM —
	// virtiofs uses Tag as the mount identifier. Collisions with the
	// other share constructors here would cause the second mount to
	// fail at boot.
	cacheDir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateRoot := t.TempDir()

	all := append([]HostShare{}, DefaultHostShares()...)
	all = append(all, PersistentClaudeShares(stateRoot)...)
	all = append(all, ToolchainCacheShares(cacheDir)...)

	seen := make(map[string]string, len(all))
	for _, sh := range all {
		if prev, dup := seen[sh.Tag]; dup {
			t.Errorf("duplicate tag %q on %s and %s", sh.Tag, prev, sh.HostPath)
			continue
		}
		seen[sh.Tag] = sh.HostPath
	}
}

func TestEnvFileNoRequiredEnv(t *testing.T) {
	if _, ok, err := EnvFile(&config.Sandbox{Name: "sb"}); ok || err != nil {
		t.Fatalf("EnvFile with no RequiredEnv: ok=%v err=%v, want false/nil", ok, err)
	}
}

// TestEnvFileAgentReadable is the regression guard for
// clawkwork/clawk#4: 99-clawk-env.sh was written 0600 root:root, so the
// agent's login shells (which run /etc/profile as the non-root `agent`
// user) silently skipped it and the forwarded vars never arrived. The
// file must be readable by the agent — i.e. carry the world-read bit,
// since agent is neither root nor in root's group — exactly like the
// sibling OAuth export 98-clawk-claude-oauth.sh.
func TestEnvFileAgentReadable(t *testing.T) {
	t.Setenv("CLAWK_TEST_TOKEN", "s3cr3t")
	t.Setenv("CLAWK_TEST_MISSING", "") // present-but-empty is fine
	os.Unsetenv("CLAWK_TEST_MISSING")

	hf, ok, err := EnvFile(&config.Sandbox{
		Name:        "sb",
		RequiredEnv: []string{"CLAWK_TEST_TOKEN", "CLAWK_TEST_MISSING"},
	})
	if err != nil || !ok {
		t.Fatalf("EnvFile: ok=%v err=%v", ok, err)
	}
	if hf.GuestPath != "/etc/profile.d/99-clawk-env.sh" {
		t.Errorf("GuestPath = %q", hf.GuestPath)
	}
	if hf.Mode != 0o644 {
		t.Errorf("Mode = %o, want 0644 (agent-readable, matching the OAuth export)", hf.Mode)
	}
	if hf.Mode&0o004 == 0 {
		t.Errorf("Mode = %o lacks the other-read bit; the non-root agent user can't source it", hf.Mode)
	}
	content := string(hf.Content)
	if !strings.Contains(content, `export CLAWK_TEST_TOKEN="s3cr3t"`) {
		t.Errorf("missing export for a set var:\n%s", content)
	}
	if !strings.Contains(content, `export CLAWK_TEST_MISSING=""`) {
		t.Errorf("missing empty export for an unset var:\n%s", content)
	}
}

func TestEnvFileEscapesShellMetachars(t *testing.T) {
	t.Setenv("CLAWK_TEST_WEIRD", `val"with$weird\meta`+"`stuff`")
	hf, ok, err := EnvFile(&config.Sandbox{
		Name:        "sb",
		RequiredEnv: []string{"CLAWK_TEST_WEIRD"},
	})
	if err != nil || !ok {
		t.Fatalf("EnvFile: ok=%v err=%v", ok, err)
	}
	content := string(hf.Content)
	for _, want := range []string{`\"`, `\$`, `\\`, "\\`"} {
		if !strings.Contains(content, want) {
			t.Errorf("missing escape %q in:\n%s", want, content)
		}
	}
}

// TestEnvFileComposeModel exercises the envspec grammar end to end
// through EnvFile: alias, :- default (applied and skipped), and a bare
// literal — checking the generated export lines use the guest-side name
// and the resolved value.
func TestEnvFileComposeModel(t *testing.T) {
	t.Setenv("HOST_GH", "ghp_xyz")
	os.Unsetenv("HOST_MISSING")
	t.Setenv("HOST_SET", "present")

	hf, ok, err := EnvFile(&config.Sandbox{
		Name: "sb",
		RequiredEnv: []string{
			"GH_TOKEN=${HOST_GH}",             // alias
			"LOG_LEVEL=${HOST_MISSING:-info}", // default applied (unset)
			"MODE=${HOST_SET:-fallback}",      // default skipped (set)
			"EDITOR=vim",                      // literal
		},
	})
	if err != nil || !ok {
		t.Fatalf("EnvFile: ok=%v err=%v", ok, err)
	}
	content := string(hf.Content)
	for _, want := range []string{
		`export GH_TOKEN="ghp_xyz"`,
		`export LOG_LEVEL="info"`,
		`export MODE="present"`,
		`export EDITOR="vim"`,
	} {
		if !strings.Contains(content, want) {
			t.Errorf("missing line %q in:\n%s", want, content)
		}
	}
}

// TestEnvFileRequiredMissingErrors verifies that a ${HOST:?msg} whose
// host variable is unset fails EnvFile (and therefore sandbox creation)
// with a message that includes the author's note.
func TestEnvFileRequiredMissingErrors(t *testing.T) {
	os.Unsetenv("HOST_REQUIRED")
	_, _, err := EnvFile(&config.Sandbox{
		Name:        "sb",
		RequiredEnv: []string{"API_KEY=${HOST_REQUIRED:?set it in your shell}"},
	})
	if err == nil {
		t.Fatal("expected error for required-but-missing env var")
	}
	if !strings.Contains(err.Error(), "set it in your shell") {
		t.Errorf("error missing the author's note: %v", err)
	}
}
