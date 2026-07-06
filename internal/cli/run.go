package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/clawkwork/clawk/internal/template"
	"github.com/clawkwork/clawk/internal/worktree"
	"github.com/spf13/cobra"
)

// registerWorkFlags binds the ticket-mode flag set to `clawk work`.
// Lives in its own function so work.go's init() reads as a single
// "register the verb + its flags" block.
func registerWorkFlags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&runName, "name", "",
		"override sandbox name (defaults to branch, non-alnum replaced with '-')")
	cmd.Flags().StringSliceVar(&runOnly, "only", nil,
		"comma-separated repo names to include (defaults to all workspace repos)")
	cmd.Flags().StringVar(&runProfile, "profile", "",
		"overlay profile to apply (loads clawk.mod.<profile> overlays)")
	// Profiles stay fully wired but are hidden from launch help — an overlay
	// feature we're not surfacing in the initial release.
	_ = cmd.Flags().MarkHidden("profile")
}

var (
	runName    string
	runBare    bool
	runOnly    []string
	runProfile string
	// runSafe is the shared backing var for the --safe flag on every runner-
	// attaching command (bare clawk, attach, run, work). One var keeps the
	// opt-out identical across entry points; each command registers the flag
	// locally so it only appears where a runner is actually launched.
	runSafe bool
)

// registerSafeFlag binds --safe to a command that attaches a runner. Kept in
// one place so the four attach entry points describe the opt-out — and the
// permission switches it suppresses — identically.
func registerSafeFlag(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&runSafe, "safe", false,
		"attach the runner without its permission-bypass default args "+
			"(claude's --dangerously-skip-permissions, codex's "+
			"--dangerously-bypass-approvals-and-sandbox), so the agent asks "+
			"for confirmation as it would on the host")
}

// applySafeMode drops a runner's permission-bypass DefaultArgs when --safe is
// set. clawk enables those switches by default because the agent already runs
// inside a disposable VM; --safe is the escape hatch for a task where that
// boundary isn't trusted enough and the on-host confirmation prompts are
// wanted. Clearing the field on the Agent value (rather than at each transport)
// is deliberate: runAgentSession reads DefaultArgs on both the vsock and the
// exec-fallback paths, so stripping it once here covers both.
func applySafeMode(a Agent) Agent {
	if runSafe {
		a.DefaultArgs = nil
	}
	return a
}

// runWorkCmd is the body of `clawk work`. Phase 9 of the v2 redesign
// retired the v1 `clawk new` alias; the shared-body indirection is
// kept anyway so future variants (e.g. `clawk work --resume`) can
// reuse the dispatch.
func runWorkCmd(cmd *cobra.Command, args []string) error {
	var source, branch string
	switch len(args) {
	case 1:
		branch = args[0]
	case 2:
		source, branch = args[0], args[1]
	}

	// The sandbox name is a pure function of the args/flags — it never needs
	// the template — so compute and validate it before resolveSource. That
	// ordering lets an already-existing sandbox short-circuit to attach WITHOUT
	// reading (a possibly different) template from the current directory.
	sbName := runName
	if sbName == "" {
		sbName = sanitiseName(branch)
	}
	if err := validateSandboxName(sbName); err != nil {
		return fmt.Errorf("%w (override with --name)", err)
	}

	// An existing sandbox is resumed, never rebuilt: the template was
	// snapshotted at create time and is deliberately never re-read (editing
	// clawk.mod afterwards is a no-op). Redirecting to attach here also closes
	// the trap where re-running `clawk work ticket-123` from the wrong
	// directory would resolveSource a *different* template and silently
	// reconfigure — resolveSource simply never runs for a name that exists.
	//
	// The one exception is a record with no phases: that's the residue of a
	// create that failed before its first worktree landed, so it falls
	// through to the create path below, which re-reads the template and
	// completes it (loadOrCreateSandboxFromWorkspace returns the existing
	// record; addPhases is idempotent).
	if store.Exists(sbName) {
		sb, err := store.Load(sbName)
		if err != nil {
			return err
		}
		if len(sb.Phases) > 0 {
			// Reconstruct the invocation for the rebuild hint — an explicit
			// template path must survive into the suggested command, or
			// following it from another directory resolves a different
			// template (the exact trap this branch exists to close).
			rebuild := branch
			if source != "" {
				rebuild = source + " " + branch
			}
			fmt.Printf("Sandbox %q already exists (template not re-read).\n"+
				"  add a repo:            clawk worktree add %s <repo>\n"+
				"  rebuild from template: clawk destroy %s, then clawk work %s\n",
				sb.DisplayName(), sb.Name, sb.Name, rebuild)
			if runBare {
				return nil
			}
			if err := ensureUp(cmd.ErrOrStderr(), sb); err != nil {
				return err
			}
			if err := attachDefaultAgent(sb); err != nil {
				return err
			}
			printDetachHint(cmd.ErrOrStderr(), sb)
			return nil
		}
	}

	ws, err := resolveSource(source, runProfile)
	if err != nil {
		return err
	}
	ws, err = ws.FilterRepos(runOnly)
	if err != nil {
		return err
	}
	if len(ws.Repos) == 0 {
		return errors.New("no repos to run (did --only match anything?)")
	}

	sb, err := loadOrCreateSandboxFromWorkspace(sbName, ws)
	if err != nil {
		return err
	}
	if err := addPhases(sb, ws, branch); err != nil {
		return err
	}
	if err := store.Save(sb); err != nil {
		return err
	}

	if runBare {
		fmt.Printf("Sandbox %q prepared (boot skipped)\n", sb.DisplayName())
		return nil
	}
	if err := runUpInline(sb); err != nil {
		return err
	}
	if err := attachDefaultAgent(sb); err != nil {
		return err
	}
	printDetachHint(os.Stderr, sb)
	return nil
}

// agentStartDir returns the guest path claude should start in. When
// there's a single phase we dive straight into its worktree — Claude
// Code auto-loads skills/CLAUDE.md from CWD and PARENTS only, so
// starting at the workspace root would miss repo-local
// <repo>/.claude/skills/. Multi-phase sandboxes keep the workspace
// root as CWD because there's no one "right" repo to pick; the
// top-level CLAUDE.md we drop there lists each phase so the agent can
// cd into whichever one it needs.
//
// Provider is consulted for the guest workspace prefix so the
// firecracker provider's /workspace differs from vz's
// /home/agent/workspace.
func agentStartDir(provider sandbox.Provider, sb *config.Sandbox) string {
	wsRoot := sandbox.GuestWorkspace(provider)
	if len(sb.Phases) == 1 && sb.Phases[0].Worktree != "" {
		return wsRoot + "/" + filepath.Base(sb.Phases[0].Worktree)
	}
	return wsRoot
}

// attachDefaultAgent attaches the default agent (claude) inside the
// running VM. Used by the bare `clawk` invocation and by `clawk
// new` once provisioning succeeds. Re-fetches the sandbox so the
// updated VMState from runUpInline is observed. Extra args are
// forwarded as the agent's argv (e.g. --resume, --continue).
func attachDefaultAgent(sb *config.Sandbox, extra ...string) error {
	fresh, err := store.Load(sb.Name)
	if err != nil {
		return err
	}
	provider, err := providerFor(fresh)
	if err != nil {
		return err
	}
	a, err := agentByName("claude")
	if err != nil {
		return err
	}
	return runAgentSession(fresh, provider, applySafeMode(a), extra)
}

// resolveSource picks the clawk.mod that shapes the sandbox: an auto-detected
// one when source is empty (workspace root first — the nearest ancestor whose
// sandbox block has `includes` — then cwd's own clawk.mod), otherwise an
// explicit path. The optional profile is threaded through to loaders that can
// apply overlays.
func resolveSource(source, profile string) (*template.Workspace, error) {
	if source == "" {
		cwd, _ := os.Getwd()
		ws, err := template.FindWorkspaceWithProfile(cwd, profile)
		if err == nil {
			return ws, nil
		}
		// Only "no workspace anywhere" falls through to the standalone /
		// git-repo ladder. Anything else — a legacy clawk.work, a flat file
		// needing its migration hint, a broken include — must surface, not
		// silently degrade to defaults.
		if !errors.Is(err, template.ErrNoWorkspace) {
			return nil, err
		}
		if ws, err := template.LoadStandaloneClawkfileWithProfile(cwd, profile); err == nil {
			return ws, nil
		}
		// Last resort: cwd is itself a git repo. Treat it as a single-repo
		// workspace inheriting only defaults — keeps `clawk work <branch>`
		// usable in any checkout without a config file. Profiles need
		// overlays to merge into, so they're rejected here.
		if profile != "" {
			return nil, fmt.Errorf(
				"--profile requires a clawk.mod (none found in %s)", cwd)
		}
		if ws, err := template.WorkspaceFromGitRepo(cwd); err == nil {
			fmt.Fprintf(os.Stderr,
				"note: no clawk.mod in %s; using defaults "+
					"(no forwards, no setup). Add clawk.mod to declare them.\n",
				ws.Root)
			return ws, nil
		}
		return nil, fmt.Errorf(
			"no clawk.mod found, and %s is not a git repo", cwd)
	}

	if source == template.RepoFileName {
		cwd, _ := os.Getwd()
		return template.LoadClawkfilePathWithProfile(
			filepath.Join(cwd, template.RepoFileName), profile)
	}

	// Any other explicit source must be a path to a clawk.mod. Named
	// templates and the legacy formats (single-file `.clawk`, clawk.work)
	// were retired pre-launch; clawk.work paths get the rename hint instead
	// of the generic usage error.
	if looksLikePath(source) {
		if filepath.Base(source) == template.RetiredWorkspaceFileName {
			return nil, template.RetiredWorkspaceFileError(source)
		}
		if filepath.Base(source) == template.RepoFileName {
			return template.LoadClawkfilePathWithProfile(source, profile)
		}
	}
	return nil, fmt.Errorf(
		"template %q must be a path to a clawk.mod file", source)
}

// looksLikePath matches the documented detection rules for explicit template
// paths: leading ./, /, ../, ~, a clawk.work suffix (retired name, kept so a
// bare "clawk.work" argument earns its rename hint), or any embedded path
// separator.
func looksLikePath(s string) bool {
	switch {
	case strings.HasPrefix(s, "./"),
		strings.HasPrefix(s, "/"),
		strings.HasPrefix(s, "../"),
		strings.HasPrefix(s, "~"):
		return true
	case strings.HasSuffix(s, template.RetiredWorkspaceFileName):
		return true
	case strings.Contains(s, string(filepath.Separator)):
		return true
	}
	return false
}

// loadOrCreateSandboxFromWorkspace returns an existing sandbox by name or
// builds a new one whose provider/allow/forwards compose workspace defaults
// and every selected repo's Clawkfile. Errors on provider and port
// conflicts — both are failure modes that would otherwise silently drop
// configuration.
func loadOrCreateSandboxFromWorkspace(name string, ws *template.Workspace) (*config.Sandbox, error) {
	if store.Exists(name) {
		return store.Load(name)
	}

	provider, err := resolveProvider(ws)
	if err != nil {
		return nil, err
	}

	// Register the files' `policy <name> ( ... )` blocks before the network
	// policy composes: `use` references resolve against the store at
	// up/reload, so the definitions must land first.
	if err := registerFilePolicies(ws.Policies); err != nil {
		return nil, err
	}

	// Merge network policy: the workspace file and every repo Clawkfile
	// contribute to one "mod" block plus the Use references; the built-in
	// defaults resolve live via the "default" policy instead of being
	// baked in at create.
	netTmpls := []*template.Template{ws.File}
	for _, r := range ws.Repos {
		netTmpls = append(netTmpls, r.Clawkfile)
	}
	network, err := composeNetworkPolicy(netTmpls...)
	if err != nil {
		return nil, err
	}

	// Merge and validate forwards. Port conflicts surface here with a
	// message naming both contributors — silent dedup would mask bugs.
	forwards, err := mergeForwards(ws)
	if err != nil {
		return nil, err
	}

	files, err := mergeFiles(ws)
	if err != nil {
		return nil, err
	}
	shares, err := mergeShares(ws)
	if err != nil {
		return nil, err
	}

	// Nested virt: workspace-level `nested` directive OR any repo's
	// Clawkfile requesting it turns it on for the whole sandbox. This
	// matches how allow/forwards merge — opt-in unioned across sources.
	nested := ws.File.Nested
	for _, r := range ws.Repos {
		if r.Clawkfile != nil && r.Clawkfile.Nested {
			nested = true
			break
		}
	}

	cpu, memoryMiB, memoryMaxMiB := resolveResources(ws)
	if err := validateResources(memoryMiB, memoryMaxMiB); err != nil {
		return nil, err
	}
	memoryMiB, memoryMaxMiB = normalizeMemory(memoryMiB, memoryMaxMiB)

	image, err := resolveImage(ws)
	if err != nil {
		return nil, err
	}
	kernel, err := resolveKernel(ws)
	if err != nil {
		return nil, err
	}

	sb := &config.Sandbox{
		Name:           name,
		Provider:       provider,
		GuestABI:       sandbox.CurrentGuestABI,
		Profile:        runProfile,
		Namespace:      createNamespace(),
		VMState:        config.VMStateStopped,
		Network:        network,
		Forwards:       forwards,
		Files:          files,
		Shares:         shares,
		NestedVirt:     nested,
		CPU:            cpu,
		MemoryMiB:      memoryMiB,
		MemoryMaxMiB:   memoryMaxMiB,
		IdleTimeoutSec: resolveIdleTimeout(ws),
		Image:          image,
		Kernel:         kernel,
		CreatedAt:      time.Now(),
	}
	if err := applyNamespaceDefaults(sb); err != nil {
		return nil, err
	}
	if err := store.Save(sb); err != nil {
		return nil, err
	}
	if sb.Image != "" {
		fmt.Printf("Created sandbox %q (provider: %s, image: %s)\n",
			sb.DisplayName(), sb.Provider, sb.Image)
	} else {
		fmt.Printf("Created sandbox %q (provider: %s)\n", sb.DisplayName(), sb.Provider)
	}
	return sb, nil
}

// resolveProvider picks the provider for a new sandbox, erroring on
// workspace-vs-repo or repo-vs-repo disagreement when the workspace itself
// is silent. This matches the design: config should reject ambiguity.
func resolveProvider(ws *template.Workspace) (config.Provider, error) {
	if ws.File.Provider != "" {
		return config.Provider(ws.File.Provider).Normalize(), nil
	}
	if providerFlag != "" {
		return config.Provider(providerFlag).Normalize(), nil
	}
	// No workspace-level preference. Collect repo-level values.
	var picked string
	var pickedRepo string
	for _, r := range ws.Repos {
		if r.Clawkfile == nil || r.Clawkfile.Provider == "" {
			continue
		}
		if picked == "" {
			picked = r.Clawkfile.Provider
			pickedRepo = r.Name
			continue
		}
		if r.Clawkfile.Provider != picked {
			return "", fmt.Errorf(
				"provider conflict: %s declares %q, %s declares %q — set provider in the workspace to choose one",
				pickedRepo, picked, r.Name, r.Clawkfile.Provider)
		}
	}
	if picked != "" {
		return config.Provider(picked).Normalize(), nil
	}
	return defaultProvider(), nil
}

// defaultImage is the rootfs new sandboxes boot when neither --image
// nor clawk.mod chooses one: clawk-dev — our own image bundling a
// development toolchain (Go/Node/Rust/etc. + claude/codex), built
// and published by .github/workflows/publish-clawk-dev.yml. Override
// per invocation with --image, or per repo/workspace with
// `vm ( image ... )`.
//
// Pinned to the :v0 tag (the workflow also publishes :latest) so the
// default boot image is reproducible rather than shifting under users.
// The GHCR package is public, so the pull needs no credentials; for a
// private registry the host would `docker login` first, since clawk
// pulls via go-containerregistry's authn.DefaultKeychain.
const defaultImage = "ghcr.io/clawkwork/clawk-dev:v0"

// finalizeImageRef applies the resolution chain and normalizations to
// an `image` value: the --image flag is the most explicit statement of
// intent and wins over clawk.mod; empty falls through to the built-in
// default; `~/...` tarball paths expand to the user's home. Local
// `docker save` tarballs are how images built without any registry
// reach clawk.
func finalizeImageRef(ref string) string {
	if imageFlag != "" {
		ref = imageFlag
	}
	if ref == "" {
		ref = defaultImage
	}
	if strings.HasPrefix(ref, "~/") {
		if expanded, err := template.ExpandPath(ref); err == nil {
			return expanded
		}
	}
	return ref
}

// resolveImage picks the OCI rootfs image for a new sandbox: the
// workspace-level `vm ( image ... )` wins; otherwise every repo that
// declares one must agree — the sandbox boots a single rootfs, so a
// disagreement is a real conflict, mirroring resolveProvider. When
// nothing declares an image, finalizeImageRef's default chain applies.
func resolveImage(ws *template.Workspace) (string, error) {
	if ws.File.Image != "" {
		return finalizeImageRef(ws.File.Image), nil
	}
	var picked, pickedRepo string
	for _, r := range ws.Repos {
		if r.Clawkfile == nil || r.Clawkfile.Image == "" {
			continue
		}
		if picked == "" {
			picked = r.Clawkfile.Image
			pickedRepo = r.Name
			continue
		}
		if r.Clawkfile.Image != picked {
			return "", fmt.Errorf(
				"image conflict: %s declares %q, %s declares %q — set image in the workspace to choose one",
				pickedRepo, picked, r.Name, r.Clawkfile.Image)
		}
	}
	return finalizeImageRef(picked), nil
}

// finalizeKernelRef applies the resolution chain to a `kernel` value: the
// --kernel flag wins over clawk.mod; empty stays empty (the default Kata
// kernel); `~/...` paths expand to the user's home. A bare path or an
// http(s) URL passes through untouched.
func finalizeKernelRef(ref string) string {
	if kernelFlag != "" {
		ref = kernelFlag
	}
	if strings.HasPrefix(ref, "~/") {
		if expanded, err := template.ExpandPath(ref); err == nil {
			return expanded
		}
	}
	return ref
}

// resolveKernel picks the guest-kernel override for a new sandbox, with
// the same precedence as resolveImage: the workspace-level
// `vm ( kernel ... )` wins; otherwise every repo that declares one must
// agree. Empty (nothing declared, no flag) selects the default Kata
// kernel.
func resolveKernel(ws *template.Workspace) (string, error) {
	if ws.File.Kernel != "" {
		return finalizeKernelRef(ws.File.Kernel), nil
	}
	var picked, pickedRepo string
	for _, r := range ws.Repos {
		if r.Clawkfile == nil || r.Clawkfile.Kernel == "" {
			continue
		}
		if picked == "" {
			picked = r.Clawkfile.Kernel
			pickedRepo = r.Name
			continue
		}
		if r.Clawkfile.Kernel != picked {
			return "", fmt.Errorf(
				"kernel conflict: %s declares %q, %s declares %q — set kernel in the workspace to choose one",
				pickedRepo, picked, r.Name, r.Clawkfile.Kernel)
		}
	}
	return finalizeKernelRef(picked), nil
}

// mergeForwards gathers forward specs from the workspace and each repo's
// Clawkfile, parses them, and errors on duplicate host ports with a message
// naming both sources.
func mergeForwards(ws *template.Workspace) ([]config.PortForward, error) {
	type src struct {
		Origin string // human-readable: "workspace" or a repo name
		Spec   string
	}
	var sources []src
	for _, s := range ws.File.Forwards {
		sources = append(sources, src{Origin: "workspace", Spec: s})
	}
	for _, r := range ws.Repos {
		if r.Clawkfile == nil {
			continue
		}
		for _, s := range r.Clawkfile.Forwards {
			sources = append(sources, src{Origin: r.Name, Spec: s})
		}
	}

	byHostPort := make(map[int]src)
	var out []config.PortForward
	for _, s := range sources {
		fwd, err := parsePortSpec(s.Spec)
		if err != nil {
			return nil, fmt.Errorf("%s forward %q: %w", s.Origin, s.Spec, err)
		}
		if prev, dup := byHostPort[fwd.HostPort]; dup {
			// Allow identical specs from multiple sources (harmless); reject
			// only genuine conflicts where the guest port differs.
			if fwd == mustParseSpec(prev.Spec) {
				continue
			}
			return nil, fmt.Errorf(
				"host port %d declared by both %s (%s) and %s (%s) — change one",
				fwd.HostPort, prev.Origin, prev.Spec, s.Origin, s.Spec)
		}
		byHostPort[fwd.HostPort] = s
		out = append(out, fwd)
	}
	return out, nil
}

// mustParseSpec parses a forward spec that already passed validation once.
// It only appears on the duplicate-spec fast path in mergeForwards; panics
// on re-parse failure because reaching that code with bad input means the
// earlier parse was inconsistent — a programmer error.
func mustParseSpec(spec string) config.PortForward {
	fwd, err := parsePortSpec(spec)
	if err != nil {
		panic(fmt.Sprintf("mergeForwards: spec %q failed re-parse: %v", spec, err))
	}
	return fwd
}

// addPhases creates one Phase per selected repo in the workspace, each
// carrying that repo's setup commands. Idempotent — existing phases for a
// (repo, branch) pair are left alone.
func addPhases(sb *config.Sandbox, ws *template.Workspace, branch string) error {
	wtDir := store.WorktreeDir(sb.Name)

	existing := make(map[string]bool)
	for _, p := range sb.Phases {
		existing[p.Repo] = true
	}

	for _, repo := range ws.Repos {
		if existing[repo.RepoPath] {
			continue
		}
		// If this repo's branch already has a merged PR, bump to the
		// next -N suffix (see worktree.ResolveBranchName). Done
		// per-repo because the branch may be merged in one but not
		// others — each phase can diverge.
		res := worktree.ResolveBranchName(repo.RepoPath, branch)
		switch {
		case res.Reused && res.Branch != res.Base:
			// Existing suffix still in flight — reuse rather than spawn
			// yet another sibling.
			fmt.Printf("  note: %s reusing existing branch %q (PR not merged)\n",
				filepath.Base(repo.RepoPath), res.Branch)
		case res.WasMerged:
			fmt.Printf("  note: %s branch %q was merged (PR #%d); starting %q forked from %s\n",
				filepath.Base(repo.RepoPath), res.Base, res.MergedPR, res.Branch, res.StartPoint)
		}
		wtPath, err := worktree.Add(repo.RepoPath, res.Branch, wtDir, res.StartPoint)
		if err != nil {
			return fmt.Errorf("creating worktree for %s: %w", repo.RepoPath, err)
		}
		var onUp, onCreate []string
		if repo.Clawkfile != nil {
			onUp = repo.Clawkfile.OnUp
			onCreate = repo.Clawkfile.OnCreate
			// Union `env ( NAME ... )` declarations from this repo's
			// Clawkfile into the sandbox-wide required set. Values are
			// never stored here — see config.Sandbox.RequiredEnv.
			sb.RequiredEnv = unionStrings(sb.RequiredEnv, repo.Clawkfile.Env)
			// Fold the repo's `agent ( ... )` seed in, reading any referenced
			// markdown files relative to the repo root. applyNamespaceDefaults
			// later layers the namespace's equivalents ahead of these, so the
			// final order is namespace-then-repo regardless of call order.
			instr, err := resolveAgentDocs(repo.RepoPath, repo.Clawkfile.Instructions)
			if err != nil {
				return fmt.Errorf("agent instructions for %s: %w", repo.Name, err)
			}
			sb.Instructions = append(sb.Instructions, instr...)
			mem, err := resolveAgentDocs(repo.RepoPath, repo.Clawkfile.Memory)
			if err != nil {
				return fmt.Errorf("agent memory for %s: %w", repo.Name, err)
			}
			sb.Memory = joinMemory(append([]string{sb.Memory}, mem...)...)
		}
		sb.Phases = append(sb.Phases, config.Phase{
			Repo:     repo.RepoPath,
			Branch:   res.Branch,
			Status:   config.PhaseStatusPending,
			Order:    len(sb.Phases),
			Worktree: wtPath,
			Setup:    onUp,
			OnCreate: onCreate,
		})
		// Persist after every successful phase: if a later worktree.Add
		// errors, destroy needs to know about the orphans we already
		// created, otherwise a follow-up `clawk work` collides on the
		// existing path. Save errors here are surfaced — losing track of
		// a worktree we just created is worse than failing loudly.
		if err := store.Save(sb); err != nil {
			return fmt.Errorf("saving sandbox after phase %d: %w", len(sb.Phases), err)
		}
		// Single-repo sandboxes skip the per-phase line — the Created
		// line already names everything; the list only earns its rows
		// when there are siblings to enumerate.
		if len(ws.Repos) > 1 {
			fmt.Printf("  added phase: %s @ %s\n", filepath.Base(repo.RepoPath), res.Branch)
		}
	}
	return nil
}

// unionStrings returns a + b with duplicates removed, preserving
// first-seen order.
func unionStrings(a, b []string) []string {
	seen := make(map[string]bool, len(a))
	out := append([]string(nil), a...)
	for _, s := range a {
		seen[s] = true
	}
	for _, s := range b {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// runUpInline invokes the same logic as `clawk up` without re-parsing argv.
func runUpInline(sb *config.Sandbox) error {
	provider, err := providerFor(sb)
	if err != nil {
		return err
	}
	// Already running is the happy path when we're about to attach —
	// narrating it is noise. The explicit `clawk up` command keeps its
	// "already running" line, where it IS the command's outcome. A paused
	// VM is unfrozen first: its agent accepts vsock dials but never
	// answers them, which would read as a hung attach.
	if status, _ := provider.Status(sb); isRunning(status) {
		_, err := resumeIfPaused(os.Stderr, sb)
		return err
	}
	return bootSandbox(provider, sb)
}

// ensureUp is runUpInline for the resume verbs (attach, run, the work
// redirect). It adds two things the create paths don't need: it refuses a
// record with no worktrees — same contract as `clawk up` — and it narrates
// the implicit boot on w, because there the user asked to attach, not to
// boot, and a multi-second silent bring-up reads as a hang.
func ensureUp(w io.Writer, sb *config.Sandbox) error {
	if len(sb.Phases) == 0 {
		return fmt.Errorf("sandbox %q has no worktrees — use 'clawk worktree add' first", sb.DisplayName())
	}
	provider, err := providerFor(sb)
	if err != nil {
		return err
	}
	if status, _ := provider.Status(sb); isRunning(status) {
		_, err := resumeIfPaused(w, sb)
		return err
	}
	fmt.Fprintf(w, "sandbox %q is not running — booting it first\n", sb.DisplayName())
	return bootSandbox(provider, sb)
}

// bootSandbox records the boot as the user's desired lifecycle state, then
// brings the VM up on its provider. Recording DesiredState on every boot
// path — not just in `clawk up` — keeps the declarative record honest, so a
// future reconciler never converges a just-attached sandbox back to stopped
// because its last explicit verb was `down`. Callers have already
// established that the VM isn't running.
func bootSandbox(provider sandbox.Provider, sb *config.Sandbox) error {
	if sb.DesiredState != config.VMStateRunning || sb.StopReason != "" {
		sb.DesiredState = config.VMStateRunning
		sb.StopReason = "" // booting retires any recorded idle-stop
		if err := store.Save(sb); err != nil {
			return err
		}
	}
	switch sb.Provider {
	case config.ProviderFirecracker:
		return bringUpFirecracker(provider, sb)
	default:
		return bringUpVZ(provider, sb)
	}
}

// sanitiseName coerces a branch name into a valid sandbox name. Branches
// can contain "/" (e.g., "feature/INFRA-1234") but sandbox names cannot.
//
// Long names are truncated. macOS caps Unix-domain socket paths
// (sun_path) at 104 chars; our per-sandbox state lives under
//
//	/Users/<u>/.clawk/vms/<name>/<sock>
//
// with sockets like usermode.sock (13 chars). With a typical home
// dir that leaves ~57 chars for <name> before bind() returns
// `invalid argument`. We truncate proactively at 40 chars (with an
// 8-char sha256 hash suffix for collision resistance) so even users
// with long usernames stay well under the limit.
var invalidNameRune = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

const maxSandboxNameLen = 40

func sanitiseName(branch string) string {
	s := invalidNameRune.ReplaceAllString(branch, "-")
	if len(s) <= maxSandboxNameLen {
		return s
	}
	// Truncate prefix + 8 hex chars of the original-name hash, separated
	// by "-". The hash is on the *sanitised* form so two distinct
	// branches that sanitise to the same prefix still get distinct
	// names. 40 - 1 (separator) - 8 (hash) = 31 chars of prefix.
	sum := sha256.Sum256([]byte(s))
	prefix := s[:maxSandboxNameLen-9]
	prefix = strings.TrimRight(prefix, "-._") // don't leave a dangling separator
	return prefix + "-" + hex.EncodeToString(sum[:4])
}

func dedupStrings(xs []string) []string {
	seen := make(map[string]bool, len(xs))
	out := xs[:0]
	for _, x := range xs {
		if seen[x] {
			continue
		}
		seen[x] = true
		out = append(out, x)
	}
	return out
}
