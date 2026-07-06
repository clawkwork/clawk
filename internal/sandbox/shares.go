package sandbox

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/google/renameio/v2"
)

//go:embed workspace_claudemd.tmpl
var workspaceClaudeMDTmpl string

// Parsed at package init so syntax errors are caught at build/test time
// instead of the first sandbox-create.
var workspaceClaudeMD = template.Must(template.New("workspace_claudemd").
	Parse(workspaceClaudeMDTmpl))

// HostShare is a host directory virtiofs-mounted into every sandbox. Tag
// must be unique across all shares on one VM (vz uses it as the mount
// identifier). GuestPath is where the guest mounts the share inside.
type HostShare struct {
	HostPath  string
	Tag       string
	GuestPath string
	ReadOnly  bool
}

// HostFile is a single host file snapshotted into the VM at boot from
// the guest manifest. Static — edits on the host require a fresh sandbox
// (or a future `clawk sync`) to propagate. Used for items that don't fit
// virtiofs's dir-only mount model.
//
// Exactly one of HostPath or Content must be set. Content is used when the
// source isn't a file — e.g. a secret extracted from the macOS Keychain.
type HostFile struct {
	HostPath  string
	Content   []byte
	GuestPath string
	Mode      uint32
	Owner     string // "user:group" or "" for root
}

// PersistentClaudeShares returns the per-sandbox host share that
// carries Claude Code's entire ~/.claude/ across destroy/recreate
// cycles. Call sites already resolved a Store so they pass the state
// root directly rather than re-deriving the path.
//
// The host directory is created idempotently so virtiofs never
// encounters a missing source. The guest-side mount point is
// ~/.claude/ — per-sandbox storage that does NOT suffer the shared-
// .claude.json races documented below; two sandboxes can't touch
// the same path because each sandbox name maps to a distinct host
// dir.
//
// We mount the whole dir rather than a curated subdir list because:
//
//   - The "ephemeral" subdirs (cache/, paste-cache/, shell-snapshots/,
//     telemetry/) measure in hundreds of KB total — exclusion isn't
//     worth the bookkeeping.
//   - settings.json and CLAUDE.md, formerly snapshot HostFiles, are
//     now seeded via SeedClaudeStateDir straight into the synced
//     dir before the mount happens. That removes the snapshot-file
//     vs share-mount layering issue.
//   - .credentials.json (non-token path) lives at its canonical place
//     inside the synced dir, so claude's atomic write-rename refresh
//     persists naturally — no more copy-in/copy-out trick via
//     auth/credentials.json.
//
// Mount ordering: this share must come BEFORE DefaultHostShares in
// the assembled share list. The agents/commands sub-mounts land on
// top of ~/.claude/ at boot, and Linux would shadow them under a
// later parent mount.
//
// Cross-sandbox races (the ones DefaultHostShares is avoiding) don't
// apply here — each sandbox name maps to its own state dir, so two
// sandboxes never write to the same path.
//
// Opt out by passing an empty stateRoot.
func PersistentClaudeShares(stateRoot string) []HostShare {
	if stateRoot == "" {
		return nil
	}
	hostPath := filepath.Join(stateRoot, "claude")
	if err := os.MkdirAll(hostPath, 0o755); err != nil {
		return nil
	}
	return []HostShare{{
		HostPath:  hostPath,
		Tag:       "claude_home",
		GuestPath: GuestHome + "/.claude",
		ReadOnly:  false,
	}}
}

// SeedClaudeStateDir writes the host-snapshot files (settings.json,
// CLAUDE.md, .credentials.json) directly into the per-sandbox state dir that
// PersistentClaudeShares mounts at ~/.claude/. Run by the provider
// during sandbox preparation, BEFORE the VM boots — so by the time
// virtio-fs mounts the dir, the files are already in place.
//
// Semantics:
//
//   - settings.json: overwritten on every call. Lets clawk refresh
//     its forced overrides (bypass permissions, etc.) and propagate
//     host settings.json edits into already-persisted sandboxes on
//     re-create.
//
//   - CLAUDE.md: overwritten on every call from host ~/.claude/CLAUDE.md
//     (if present). Matches the old snapshot-at-create semantics
//     exactly — host edits flow into newly-created sandboxes.
//
//   - .credentials.json: written ONLY when (a) no long-lived OAuth
//     token is configured (env var or clawk root file) and (b) the
//     state dir doesn't already have a credentials file. The second
//     condition preserves a refreshed token across destroy/recreate
//     cycles, replacing the previous copy-in/copy-out dance through
//     auth/credentials.json.
//
// Returns the first error encountered. Best-effort: a missing host
// CLAUDE.md isn't an error (there's no source to copy), but failure
// to write into the state dir is.
func SeedClaudeStateDir(stateRoot, clawkRootDir string) error {
	if stateRoot == "" {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving host home: %w", err)
	}
	dst := filepath.Join(stateRoot, "claude")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dst, err)
	}

	if content, ok := buildGuestClaudeSettings(home); ok {
		if err := renameio.WriteFile(filepath.Join(dst, "settings.json"), content, 0o644); err != nil {
			return fmt.Errorf("seeding settings.json: %w", err)
		}
	}

	srcCLAUDE := filepath.Join(home, ".claude", "CLAUDE.md")
	if data, err := os.ReadFile(srcCLAUDE); err == nil {
		if err := renameio.WriteFile(filepath.Join(dst, "CLAUDE.md"), data, 0o644); err != nil {
			return fmt.Errorf("seeding CLAUDE.md: %w", err)
		}
	}

	// Credentials only when the long-lived OAuth token isn't configured.
	// With a token, claude reads CLAUDE_CODE_OAUTH_TOKEN and never
	// touches .credentials.json — shipping it would just leave a stale
	// refresh-token snapshot in the dir.
	if token, _ := LoadOAuthToken(clawkRootDir); token != "" {
		return nil
	}
	credPath := filepath.Join(dst, ".credentials.json")
	if _, err := os.Stat(credPath); err == nil {
		// Already persisted — claude has been refreshing in-place; don't
		// clobber a fresher copy with a stale keychain snapshot.
		return nil
	}
	if blob, ok := readHostClaudeCredentials(); ok {
		if err := renameio.WriteFile(credPath, blob, 0o600); err != nil {
			return fmt.Errorf("seeding .credentials.json: %w", err)
		}
	}
	return nil
}

// SeedClaudeMemory writes seed into the agent's auto-memory entrypoint
// (<stateRoot>/claude/memory/MEMORY.md — the path the autoMemoryDirectory
// setting points at) the FIRST time a sandbox boots, when no memory file
// exists yet. It never overwrites: once the agent — or a prior session folded
// in via internal/sessions — has written memory, that wins. An empty seed or
// an already-present file is a no-op. Best-effort like SeedClaudeStateDir:
// failing to seed baseline knowledge must not block boot.
func SeedClaudeMemory(stateRoot, seed string) error {
	if stateRoot == "" || strings.TrimSpace(seed) == "" {
		return nil
	}
	dir := filepath.Join(stateRoot, "claude", "memory")
	path := filepath.Join(dir, "MEMORY.md")
	if _, err := os.Stat(path); err == nil {
		return nil // already has memory — never clobber accumulated knowledge
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating memory dir: %w", err)
	}
	if !strings.HasSuffix(seed, "\n") {
		seed += "\n"
	}
	if err := renameio.WriteFile(path, []byte(seed), 0o644); err != nil {
		return fmt.Errorf("seeding MEMORY.md: %w", err)
	}
	return nil
}

// ToolchainCacheShares returns host shares that back the dependency
// caches of common language toolchains. Mounting one host directory per
// cache means a module/crate is downloaded once and reused across every
// sandbox — preventing the multi-GB-per-sandbox blowup we observed
// where each new clone re-downloaded the same Go module set.
//
// Each guest path is the toolchain's *default* cache location, so no
// env vars and no provision.sh changes are required: the mount is
// transparent. Host dirs are created on demand (virtiofs refuses
// missing source paths); MkdirAll failures silently drop the offending
// share rather than failing sandbox creation, matching the behavior
// of PersistentClaudeShares.
//
// Caches included (well-documented concurrent safety, real payoff):
//
//	Go modules         ~/go/pkg/mod         — file-locked, append-only;
//	                                          standard CI sharing pattern.
//	Cargo registry+git ~/.cargo/registry/{index,cache}, ~/.cargo/git/db
//	                                       — Cargo holds cross-process
//	                                         locks; the same set every
//	                                         Rust GHA workflow caches.
//
// Caches deliberately excluded:
//
//	Go build cache (~/.cache/go-build) — NOT shared. It stays per-VM on
//	  the rootfs (Go's default location). Two reasons: (1) the build
//	  cache is not safe for concurrent builds against one directory
//	  (golang/go#43645) — racing `go build`s in different sandboxes can
//	  delete/move entries mid-read and corrupt each other's lookups,
//	  unlike the append-only, file-locked module cache; (2) it's the
//	  churniest tree we'd share — rewritten on every build — and over
//	  Apple's virtio-fs each guest-cached inode pins a host fd in the VM
//	  XPC process, so it dominated the descriptor blowup we saw. A build
//	  cache is cheap to repopulate per-VM, so the sharing payoff never
//	  justified the hazard.
//	pnpm store / uv cache / Bun cache — all three hardlink from cache
//	  into the working tree (node_modules, .venv). Hardlinks don't
//	  cross filesystems, so on a virtiofs mount these tools silently
//	  fall back to copying — strictly worse than no sharing, since you
//	  pay the copy cost AND lose dedup. Revisit only if worktrees move
//	  onto the same shared mount layout.
//	Cargo target/ — locked per-project; cross-project sharing is an
//	  unimplemented Rust project goal.
//	~/.cargo/bin and ~/.cargo/registry/src — bin/ holds executables,
//	  not cache; src/ is cheap to re-extract from cache/ and sharing
//	  it would just balloon disk usage.
//	Zig and pip — small footprint here; pip is superseded by uv.
//
// Pass cacheDir = "" to opt out.
func ToolchainCacheShares(cacheDir string) []HostShare {
	if cacheDir == "" {
		return nil
	}
	specs := []struct{ sub, tag, guest string }{
		{"gomodcache", "go_modcache", GuestHome + "/go/pkg/mod"},
		{"cargo-registry-index", "cargo_registry_index", GuestHome + "/.cargo/registry/index"},
		{"cargo-registry-cache", "cargo_registry_cache", GuestHome + "/.cargo/registry/cache"},
		{"cargo-git-db", "cargo_git_db", GuestHome + "/.cargo/git/db"},
	}
	out := make([]HostShare, 0, len(specs))
	for _, s := range specs {
		hostPath := filepath.Join(cacheDir, s.sub)
		if err := os.MkdirAll(hostPath, 0o755); err != nil {
			continue
		}
		out = append(out, HostShare{
			HostPath:  hostPath,
			Tag:       s.tag,
			GuestPath: s.guest,
			ReadOnly:  false,
		})
	}
	return out
}

// DefaultHostShares returns host agent capability dirs that are safe to live
// read-write-share across host and VM.
//
// We do NOT share all of ~/.claude, even though that would match local
// multi-terminal behavior. The Claude Code issue tracker documents real
// corruption from concurrent access:
//
//   - anthropics/claude-code#28847 (.claude.json race corrupts state)
//   - anthropics/claude-code#25609 (OAuth refresh race revokes tokens)
//   - anthropics/claude-code#10039 (macOS Keychain deletes .credentials.json,
//     breaking Linux co-mounted sessions)
//
// Sharing Claude agents/commands and Codex skills is safe: user-authored
// capability dirs with low write contention. Sharing .claude.json,
// credentials, projects/, file-history/, or the whole ~/.codex state dir
// would risk concurrent writers. Each sandbox authenticates independently —
// one-time cost per sandbox in exchange for no cross-session corruption.
//
// ~/.claude/skills is deliberately NOT shared. A skill like gstack carries
// a large node_modules tree, and virtio-fs caches an inode (and host fd)
// per file it touches; across several running sandboxes that inflates the
// Virtualization.framework XPC processes' fd count enough to exhaust the
// host's system-wide open-file table (kern.maxfiles → ENFILE). Skills that
// a sandbox needs can be brought in per-sandbox via an explicit
// `shares (...)` entry instead.
func DefaultHostShares() []HostShare {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var shares []HostShare
	for _, sub := range []string{"agents", "commands"} {
		hostPath := filepath.Join(home, ".claude", sub)
		if info, err := os.Stat(hostPath); err != nil || !info.IsDir() {
			continue
		}
		shares = append(shares, HostShare{
			HostPath:  hostPath,
			Tag:       "claude_" + sub,
			GuestPath: GuestHome + "/.claude/" + sub,
			ReadOnly:  false, // user edits in VM propagate back to host
		})
	}
	codexSkills := filepath.Join(home, ".codex", "skills")
	if info, err := os.Stat(codexSkills); err == nil && info.IsDir() {
		shares = append(shares, HostShare{
			HostPath:  codexSkills,
			Tag:       "codex_skills",
			GuestPath: GuestHome + "/.codex/skills",
			ReadOnly:  false, // user edits in VM propagate back to host
		})
	}
	return shares
}

// UserHostShares converts a sandbox's user-declared shares
// (config.HostShare from `shares (...)` in clawk.mod) into the
// internal HostShare form with stable virtio-fs tags.
//
// Tags are derived from the guest path so the same share survives
// `clawk down && clawk up` without re-randomising — the guest manifest,
// the provider's --device list, and the runtime mount loop all key off
// the tag, and a churned tag means a stale mount entry that fails on
// next boot. SHA-256 hex prefix gives 8 hex chars (32 bits),
// collision-resistant inside a single sandbox's share list while staying
// short enough for virtio-fs tag length constraints.
func UserHostShares(shares []config.HostShare) []HostShare {
	out := make([]HostShare, 0, len(shares))
	for _, s := range shares {
		out = append(out, HostShare{
			HostPath:  s.HostPath,
			Tag:       userShareTag(s.GuestPath),
			GuestPath: s.GuestPath,
			ReadOnly:  s.ReadOnly,
		})
	}
	return out
}

// userShareTag derives a stable virtio-fs tag from the guest mount point.
// Exported only via UserHostShares; the tag itself is an implementation
// detail callers shouldn't construct directly.
func userShareTag(guestPath string) string {
	sum := sha256.Sum256([]byte(guestPath))
	return "usr_" + hex.EncodeToString(sum[:4])
}

// DefaultHostFiles returns host files snapshot-copied into each sandbox
// via the guest manifest. Today, the only items here are those
// that live OUTSIDE the per-sandbox ~/.claude/ mount:
//
//   - /etc/profile.d/98-clawk-claude-oauth.sh (env var for the long-
//     lived OAuth token, when configured)
//   - ~/.claude.json (onboarding marker, at home root, not inside
//     ~/.claude/)
//   - ~/.gitconfig
//   - ~/.ssh/known_hosts
//
// Files that DO live inside ~/.claude/ — settings.json, CLAUDE.md,
// .credentials.json — are pre-seeded into the per-sandbox state dir
// by SeedClaudeStateDir before the VM boots; they appear at the
// canonical paths through the PersistentClaudeShares mount, no
// snapshot-file involvement needed.
//
// clawkRootDir resolves the optional long-lived OAuth token
// (~/.clawk/claude-oauth-token, see oauth_token.go). When configured,
// the profile.d export and onboarding marker are emitted; the
// rotating keychain blob lives only in the seeded state dir.
func DefaultHostFiles(clawkRootDir string) []HostFile {
	var out []HostFile

	// Long-lived OAuth token: profile.d export. The onboarding-marker
	// and trust-dialog pre-acceptance both live in ~/.claude.json and
	// are emitted by ClaudeJSONMarkerFile (which needs the sandbox's
	// phase list, not just the clawk root). Callers add that file
	// separately at sandbox prepare time.
	//
	// The keychain-blob fallback (~/.claude/.credentials.json) is not
	// shipped here — SeedClaudeStateDir writes it into the mounted
	// state dir instead, so claude's atomic-rename refresh persists
	// naturally without a copy-in/copy-out dance.
	if token, _ := LoadOAuthToken(clawkRootDir); token != "" {
		out = append(out, OAuthTokenEnvFile(token))
	}

	// ~/.gitconfig — copies the user's git identity and rewrites HTTPS
	// github URLs to SSH so commits/pushes inside the VM use the
	// forwarded SSH agent instead of prompting for HTTPS credentials.
	if blob, ok := buildGuestGitConfig(); ok {
		out = append(out, HostFile{
			Content:   blob,
			GuestPath: GuestHome + "/.gitconfig",
			Mode:      0o644,
			Owner:     GuestUser + ":" + GuestUser,
		})
	}

	// ~/.ssh/known_hosts — avoid the "authenticity of host ... can't be
	// established" prompt on first `git push` inside a fresh sandbox.
	// Blends the user's existing host known_hosts (so previously trusted
	// servers carry over) with GitHub's published host keys as a
	// backstop.
	if blob, ok := buildGuestKnownHosts(); ok {
		out = append(out, HostFile{
			Content:   blob,
			GuestPath: GuestHome + "/.ssh/known_hosts",
			Mode:      0o644,
			Owner:     GuestUser + ":" + GuestUser,
		})
	}
	return out
}

// buildGuestGitConfig synthesizes a minimal ~/.gitconfig for the sandbox:
//
//   - [user] section copied from host `git config --global user.{name,email}`
//     so commits are attributed to you, not "agent@<hostname>".
//   - [url] rewrite so any `https://github.com/...` remote is transparently
//     pushed over ssh — required because the sandbox forwards your SSH
//     agent (working) but has no browser/Keychain for HTTPS auth.
//
// Deliberately doesn't copy the whole host ~/.gitconfig: it often contains
// host-absolute `[include]` paths, `[credential]` helpers that won't work
// in the VM (security on macOS, keychain backends), and aliases that can
// shell out to host tools. A minimal synthesized file is safer.
func buildGuestGitConfig() ([]byte, bool) {
	name := strings.TrimSpace(gitConfigValue("user.name"))
	email := strings.TrimSpace(gitConfigValue("user.email"))

	// [user] is only emitted if we got at least one of name/email from
	// the host — a [user] block with both fields missing is nonsensical.
	var userSection string
	switch {
	case name != "" && email != "":
		userSection = fmt.Sprintf("[user]\n\tname = %s\n\temail = %s\n\n", name, email)
	case name != "":
		userSection = fmt.Sprintf("[user]\n\tname = %s\n\n", name)
	case email != "":
		userSection = fmt.Sprintf("[user]\n\temail = %s\n\n", email)
	}

	out := userSection + `[url "git@github.com:"]
	insteadOf = https://github.com/
`
	return []byte(out), true
}

// sandboxForcedClaudeSettings are the keys clawk enforces on every
// sandbox regardless of what the host has configured. Your host
// settings might (rightly) disable bypass permissions — on your
// laptop, guardrails matter. Inside a disposable VM with only
// worktree and sandbox disk at risk, those guardrails just slow the
// agent down. The whole point of clawk is that permission prompts
// aren't load-bearing here. So we force them off.
var sandboxForcedClaudeSettings = map[string]any{
	"bypassPermissionsModeAccepted":     true,
	"defaultMode":                       "bypassPermissions",
	"skipDangerousModePermissionPrompt": true,
}

// sandboxDefaultClaudeSettings are polite suggestions for missing
// keys — host wins if they've set these. Mirrors Docker AI Sandbox's
// claude-code template for the preference-like bits: extended
// thinking on, dark theme.
var sandboxDefaultClaudeSettings = map[string]any{
	"alwaysThinkingEnabled": true,
	"themeId":               1,
	"model":                 "opus",
	// Consolidate auto-memory into the tracked top-level memory dir. Claude's
	// default is projects/<repo-encoded>/memory, but the sessions git working
	// tree versions /memory/ (see internal/sessions gitignore), so pointing
	// here makes baseline memory seedable (SeedClaudeMemory) and shareable
	// across a project's sandboxes through the same history merge. A default,
	// not forced: a host that deliberately sets autoMemoryDirectory still wins.
	"autoMemoryDirectory": GuestHome + "/.claude/memory",
	"env": map[string]any{
		"ENABLE_LSP_TOOL": "1",
	},
	"enabledPlugins": map[string]any{
		"gopls-lsp@claude-plugins-official":         true,
		"typescript-lsp@claude-plugins-official":    true,
		"pyright-lsp@claude-plugins-official":       true,
		"rust-analyzer-lsp@claude-plugins-official": true,
	},
	// Voice dictation on by default. Tap mode (not the default hold mode):
	// hold detection relies on terminal key-repeat events, which don't arrive
	// reliably over the vsock pty the agent is attached through, whereas tap
	// toggles on a single keypress. A default, not forced — a host that
	// configures `voice` differently still wins. Note: dictation needs a
	// microphone and a Claude.ai-account login; it stays inert until the host
	// forwards audio into the guest (the VM has no mic of its own).
	"voice": map[string]any{
		"enabled": true,
		"mode":    "tap",
	},
}

// buildGuestClaudeSettings layers host settings with sandbox overrides:
//
//	defaults → host (host wins for these) → forced (clawk wins)
//
// That way custom host hooks, permissions, theme choices flow through,
// but the sandbox's YOLO stance can't be accidentally undone by
// host-level guardrails. Host-malformed JSON is non-fatal; it just
// falls through to (defaults ∪ forced).
func buildGuestClaudeSettings(home string) ([]byte, bool) {
	merged := make(map[string]any,
		len(sandboxDefaultClaudeSettings)+len(sandboxForcedClaudeSettings)+8)
	// 1. Defaults first — host can override any of these.
	for k, v := range sandboxDefaultClaudeSettings {
		merged[k] = v
	}
	// 2. Host settings — inherit theme, hooks, env, permissions, etc.
	if data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json")); err == nil {
		var host map[string]any
		if err := json.Unmarshal(data, &host); err == nil {
			for k, v := range host {
				merged[k] = v
			}
		}
	}
	// 3. Forced sandbox overrides — applied last, cannot be
	//    undone by host settings.
	for k, v := range sandboxForcedClaudeSettings {
		merged[k] = v
	}
	content, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return nil, false
	}
	return append(content, '\n'), true
}

// shellEscapeDoubleQuotedReplacer escapes the four metacharacters that
// remain active inside POSIX double-quoted strings: backslash, double
// quote, dollar sign, and backtick. Built once; concurrent .Replace
// calls are safe per strings.Replacer's documentation.
var shellEscapeDoubleQuotedReplacer = strings.NewReplacer(
	`\`, `\\`,
	`"`, `\"`,
	`$`, `\$`,
	"`", "\\`",
)

// shellEscapeDoubleQuoted returns v with the four POSIX double-quote
// active metacharacters backslash-escaped, so the result is safe to
// embed inside an `export NAME="…"` line. Used by every site that
// emits a shell-export file (env-vars from clawk.mod, the OAuth token).
func shellEscapeDoubleQuoted(v string) string {
	return shellEscapeDoubleQuotedReplacer.Replace(v)
}

// EnvFile synthesizes an /etc/profile.d script that exports every
// sandbox-required env var. Names come from sb.RequiredEnv (declared in
// clawk.mod); values come from the host's process env at this call.
// Missing host vars log a warning and are exported empty — callers
// decide whether that's fatal (e.g., an MCP server failing to auth is
// a clear signal).
//
// Written into /etc/profile.d/99-clawk-env.sh so every login shell
// (ssh, `claude ...`, interactive bash, phase setup scripts) picks up
// the values without any per-tool configuration.
//
// Returns ok=false if the sandbox has no required env — saves the
// caller from having to filter empty HostFiles.
func EnvFile(sb *config.Sandbox) (HostFile, bool) {
	if len(sb.RequiredEnv) == 0 {
		return HostFile{}, false
	}
	var b bytes.Buffer
	b.WriteString("# Generated by clawk — host env vars requested in clawk.mod\n")
	for _, name := range sb.RequiredEnv {
		val, ok := os.LookupEnv(name)
		if !ok {
			fmt.Fprintf(os.Stderr,
				"warning: %s required by clawk.mod is unset on host; "+
					"exporting empty in sandbox %q\n", name, sb.Name)
		}
		// Double-quoted with $ and " escaped — covers secrets that
		// contain dollar signs or quotes. Single quotes aren't safe
		// because bash single-quote strings can't contain single
		// quotes, and API keys sometimes do.
		fmt.Fprintf(&b, "export %s=\"%s\"\n", name, shellEscapeDoubleQuoted(val))
	}
	return HostFile{
		Content:   b.Bytes(),
		GuestPath: "/etc/profile.d/99-clawk-env.sh",
		Mode:      0o600, // root-owned; secrets
		Owner:     "root:root",
	}, true
}

// WorkspaceDocFile returns a CLAUDE.md that gets dropped at
// /home/agent/workspace/CLAUDE.md on first boot. Claude Code auto-loads
// a CLAUDE.md in its startup CWD, so whatever's in here becomes part of
// the agent's instructions the moment it starts.
//
// The content describes WHERE the agent is (a clawk VM, not the
// user's laptop), WHAT the layout looks like (per-phase worktrees), and
// WHICH constraints apply (egress allowlist, mounts, git/ssh setup).
// Knowing these upfront stops the agent from wasting turns investigating
// the environment or being surprised by a blocked connection mid-task.
func WorkspaceDocFile(sb *config.Sandbox) HostFile {
	type phase struct{ Name, Branch, Repo string }
	data := struct {
		Name          string
		WorkspaceRoot string
		Phases        []phase
		Instructions  []string
	}{
		Name:          sb.DisplayName(),
		WorkspaceRoot: WorkspaceRoot,
		Instructions:  sb.Instructions,
	}
	for _, p := range sb.Phases {
		if p.Worktree == "" {
			continue
		}
		data.Phases = append(data.Phases, phase{
			Name:   filepath.Base(p.Worktree),
			Branch: p.Branch,
			Repo:   p.Repo,
		})
	}

	var buf bytes.Buffer
	// template.Must + Execute on a pre-parsed template leaves only the
	// execution failure path here — which is purely logic bugs (missing
	// field refs, nil maps, etc.) the tests would catch. Ignoring the
	// error after Must is idiomatic for embedded templates.
	_ = workspaceClaudeMD.Execute(&buf, data)

	return HostFile{
		Content:   buf.Bytes(),
		GuestPath: WorkspaceRoot + "/CLAUDE.md",
		Mode:      0o644,
		Owner:     GuestUser + ":" + GuestUser,
	}
}

// gitConfigValue runs `git config --global --get <key>` and returns the
// value, empty string if unset. git is required on the host since
// clawk uses worktrees already — a missing git binary would have
// failed long before sandbox create.
func gitConfigValue(key string) string {
	out, err := exec.Command("git", "config", "--global", "--get", key).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// GitHub's published SSH host keys (docs: https://api.github.com/meta —
// `ssh_keys` field). Hard-coded rather than fetched at runtime so the
// happy path works offline and doesn't depend on our firewall letting
// api.github.com through at sandbox-create time.
const githubKnownHosts = `github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl
github.com ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBEmKSENjQEezOmxkZMy7opKgwFB9nkt5YRrYMjNuG5N87uRgg6CLrbo5wAdT/y6v0mKV0U2w0WZ2YB/++Tpockg=
github.com ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQCj7ndNxQowgcQnjshcLrqPEiiphnt+VTTvDP6mHBL9j1aNUkY4Ue1gvwnGLVlOhGeYrnZaMgRK6+PKCUXaDbC7qtbW8gIkhL7aGCsOr/C56SJMy/BCZfxd1nWzAOxSDPgVsmerOBYfNqltV9/hWCqBywINIR+5dIg6JTJ72pcEpEjcYgXkE2YEFXV1JHnsKgbLWNlhScqb2UmyRkQyytRLtL+38TGxkxCflmO+5Z8CSSNY7GidjMIZ7Q4zMjA2n1nGrlTDkzwDCsw+wqFPGQA179cnfGWOWRVruj16z6XyvxvjJwbz0wQZ75XK5tKSb7FNyeIEs4TT4jk+S4dhPeAUC5y+bDYirYgM4GC7uEnztnZyaVWQ7B381AK4Qdrwt51ZqExKbQpTUNn+EjqoTwvqNj4kqx5QUCI0ThS/YkOxJCXmPUWZbhjpCg56i+2aB6CmK2JGhn57K5mj0MNdBXA4/WnwH6XoPWJzK5Nyu2zB3nAZp+S5hpQs+p1vN1/wsjk=
`

// buildGuestKnownHosts returns the merged content of the user's host
// ~/.ssh/known_hosts (if any) and the pinned GitHub keys. Ensures `git
// push` inside a fresh sandbox doesn't stall on "authenticity of host
// ... can't be established" just because the VM has never spoken to
// github.com before.
//
// We append GitHub keys only if they're not already present in the
// host file — avoids duplicate entries that would look noisy and
// occasionally cause ssh to log warnings.
func buildGuestKnownHosts() ([]byte, bool) {
	var b bytes.Buffer
	if home, err := os.UserHomeDir(); err == nil {
		if data, err := os.ReadFile(filepath.Join(home, ".ssh", "known_hosts")); err == nil {
			b.Write(data)
			if len(data) > 0 && data[len(data)-1] != '\n' {
				b.WriteByte('\n')
			}
		}
	}
	if !strings.Contains(b.String(), "github.com ssh-ed25519") {
		b.WriteString(githubKnownHosts)
	}
	return b.Bytes(), b.Len() > 0
}
