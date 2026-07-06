package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/clawkwork/clawk/internal/template"
)

// rollbackFailedCreate tears down a cwd-sandbox whose very first boot failed,
// so the freshly-written record doesn't linger pinning the config that just
// failed. The cwd-sandbox is snapshot-at-create (loadOrCreateHereSandbox does
// not re-read clawk.mod once a record exists), so without this a retry would
// reload the broken record and silently ignore an edited clawk.mod — leaving
// `clawk destroy` as the only, non-obvious, recovery. The cwd phase is InPlace
// (its worktree IS the user's directory), so there's nothing to git-worktree-
// remove. Call only for a sandbox created in this same invocation; best-effort,
// so cleanup errors are warned rather than masking the boot error.
func rollbackFailedCreate(sb *config.Sandbox) {
	if provider, err := providerFor(sb); err == nil {
		if err := provider.Destroy(sb); err != nil {
			fmt.Fprintf(os.Stderr, "warning: rolling back VM for %q: %v\n", sb.DisplayName(), err)
		}
	}
	if err := store.Delete(sb.Name); err != nil {
		fmt.Fprintf(os.Stderr, "warning: rolling back sandbox record %q: %v\n", sb.DisplayName(), err)
	}
}

// hereCWDAndName resolves the cwd-vm sandbox name derived from the
// current working directory. The returned cwd is canonical (absolute,
// symlinks resolved) because it becomes the record's Anchor and InPlace
// worktree: deriveHereSandboxName has always matched against the resolved
// path, so anchoring the raw Getwd (which honors $PWD and keeps macOS's
// /tmp -> /private/tmp symlink unresolved) split the two sources of truth
// — the create named the sandbox "foo" while every later cwd-inferred
// lookup decided the anchor didn't match and derived "foo_<hash>".
func hereCWDAndName() (cwd, name string, _ error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", "", fmt.Errorf("getwd: %w", err)
	}
	cwd = canonicalPath(cwd)
	name, err = deriveHereSandboxName(cwd)
	if err != nil {
		return "", "", err
	}
	return cwd, name, nil
}

// canonicalPath is the single canonical form for directory identity:
// absolute, symlinks resolved. Best-effort — a path that can't be resolved
// (racing deletion, permission) is returned as-is rather than failing the
// command that asked.
func canonicalPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return p
}

// loadOrCreateHereSandbox returns an existing sandbox or creates one
// wrapped around the current directory. The second return reports
// whether a new sandbox was written.
//
// On create, a clawk.mod in cwd (if present) is picked up: its allow
// list, forwards, setup commands, and env requirements merge into the
// sandbox. Absence of clawk.mod is fine — defaults apply. Changes to
// clawk.mod after creation do NOT propagate; destroy and recreate to
// pick up new config. (That's intentional — matches new's
// snapshot-at-create semantics and avoids surprise reconfig on a
// sandbox the user already trusts.)
func loadOrCreateHereSandbox(name, cwd string) (*config.Sandbox, bool, error) {
	if store.Exists(name) {
		sb, err := store.Load(name)
		if err != nil {
			return nil, false, err
		}
		// Heal a here-sandbox whose persisted provider can't run on this host
		// — e.g. a vz record written by an older default on a Linux box. The
		// here-VM is cwd-derived and disposable, so re-point it at a runnable
		// provider (--provider if valid, else the host default) and persist,
		// rather than wedging every command (including destroy) on the error.
		if _, perr := newProvider(sb.Provider.Normalize()); perr != nil {
			want := config.Provider(providerFlag).Normalize()
			if _, werr := newProvider(want); werr != nil {
				want = defaultProvider()
			}
			sb.Provider = want
			if err := store.Save(sb); err != nil {
				return nil, false, err
			}
		}
		return sb, false, nil
	}

	clawkfile, policies := loadHereClawkfile(cwd)

	// Register the file's `policy <name> ( ... )` blocks before composing
	// the network policy — `use` references resolve against the store at
	// up/reload, so the definitions must land first.
	if err := registerFilePolicies(policies); err != nil {
		return nil, false, err
	}

	provider := config.Provider(providerFlag).Normalize()
	if provider == "" && clawkfile != nil && clawkfile.Provider != "" {
		provider = config.Provider(clawkfile.Provider).Normalize()
	}
	if provider == "" {
		provider = defaultProvider()
	}

	// The built-in defaults resolve live via the "default" policy at
	// up/reload; only the clawk.mod's own entries are stored on the record.
	network, err := composeNetworkPolicy(clawkfile)
	if err != nil {
		return nil, false, err
	}
	var forwardSpecs []string
	var onUp, onCreate []string
	var requiredEnv []string
	var instructions []string
	var memory string
	var fileSources []fileSource
	var shareSources []shareSource
	if clawkfile != nil {
		forwardSpecs = append(forwardSpecs, clawkfile.Forwards...)
		onUp = append(onUp, clawkfile.OnUp...)
		onCreate = append(onCreate, clawkfile.OnCreate...)
		requiredEnv = append(requiredEnv, clawkfile.Env...)
		instr, err := resolveAgentDocs(cwd, clawkfile.Instructions)
		if err != nil {
			return nil, false, fmt.Errorf("agent instructions: %w", err)
		}
		instructions = append(instructions, instr...)
		mem, err := resolveAgentDocs(cwd, clawkfile.Memory)
		if err != nil {
			return nil, false, fmt.Errorf("agent memory: %w", err)
		}
		memory = joinMemory(mem...)
		for _, f := range clawkfile.Files {
			fileSources = append(fileSources, fileSource{Origin: "clawk.mod", Spec: f})
		}
		for _, s := range clawkfile.Shares {
			shareSources = append(shareSources, shareSource{Origin: "clawk.mod", Spec: s})
		}
	}

	forwards, err := parseForwardSpecs(forwardSpecs, cwd)
	if err != nil {
		return nil, false, err
	}
	files, err := composeFiles(fileSources)
	if err != nil {
		return nil, false, err
	}
	shares, err := composeShares(shareSources)
	if err != nil {
		return nil, false, err
	}

	var nested bool
	var cpu uint
	var memoryMiB, memoryMaxMiB uint64
	var image, kernel string
	var idleTimeoutSec int64
	if clawkfile != nil {
		nested = clawkfile.Nested
		cpu = clawkfile.CPU
		memoryMiB = clawkfile.MemoryMiB
		memoryMaxMiB = clawkfile.MemoryMaxMiB
		image = clawkfile.Image
		kernel = clawkfile.Kernel
		idleTimeoutSec = clawkfile.IdleTimeoutSec
	}
	image = finalizeImageRef(image)
	kernel = finalizeKernelRef(kernel)
	if err := validateResources(memoryMiB, memoryMaxMiB); err != nil {
		return nil, false, err
	}
	memoryMiB, memoryMaxMiB = normalizeMemory(memoryMiB, memoryMaxMiB)

	sb := &config.Sandbox{
		Name:         name,
		Provider:     provider,
		GuestABI:     sandbox.CurrentGuestABI,
		Namespace:    config.DefaultNamespace, // Phase 2: from clawk.mod / -n
		Anchor:       cwd,
		VMState:      config.VMStateStopped,
		Network:      network,
		Forwards:     forwards,
		Files:        files,
		Shares:       shares,
		RequiredEnv:  requiredEnv,
		Instructions: instructions,
		Memory:       memory,
		NestedVirt:   nested,
		CPU:          cpu,
		MemoryMiB:    memoryMiB,
		MemoryMaxMiB: memoryMaxMiB,
		// IdleTimeoutSec rides the same snapshot-at-create rule as every
		// other clawk.mod value; it was the one vm(...) field this path
		// forgot to copy when idle-stop landed, which silently pinned every
		// cwd sandbox to the 30m default.
		IdleTimeoutSec: idleTimeoutSec,
		Image:          image,
		Kernel:         kernel,
		Phases: []config.Phase{{
			Repo:     cwd,
			Status:   config.PhaseStatusActive,
			Order:    0,
			Worktree: cwd,
			InPlace:  true,
			Setup:    onUp,
			OnCreate: onCreate,
		}},
		CreatedAt: time.Now(),
	}
	if err := store.Save(sb); err != nil {
		return nil, false, fmt.Errorf("saving sandbox: %w", err)
	}
	return sb, true, nil
}

// loadHereClawkfile reads cwd/clawk.mod if present and returns its sandbox
// template plus any policy blocks declared beside it. Missing or unreadable
// clawk.mod is not an error — callers fall back to defaults.
func loadHereClawkfile(cwd string) (*template.Template, []template.PolicyDef) {
	ws, err := template.LoadStandaloneClawkfileWithProfile(cwd, "")
	if err != nil {
		// Only surface non-ENOENT errors; absence is expected.
		if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "warning: reading clawk.mod: %v\n", err)
		}
		return nil, nil
	}
	if ws == nil || len(ws.Repos) == 0 {
		return nil, nil
	}
	return ws.Repos[0].Clawkfile, ws.Policies
}

// parseForwardSpecs turns a list of "PORT" or "HOST:GUEST" strings into
// config.PortForwards. context is the directory the specs came from —
// used only for error messages.
func parseForwardSpecs(specs []string, context string) ([]config.PortForward, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	out := make([]config.PortForward, 0, len(specs))
	seen := make(map[int]bool, len(specs))
	for _, s := range specs {
		fwd, err := parsePortSpec(s)
		if err != nil {
			return nil, fmt.Errorf("%s forward %q: %w", context, s, err)
		}
		if seen[fwd.HostPort] {
			continue
		}
		seen[fwd.HostPort] = true
		out = append(out, fwd)
	}
	return out, nil
}

// deriveHereSandboxName produces a stable name for cwd. If two
// directories share a basename, the second one is disambiguated with a
// short hash of the absolute path. Symlinks are resolved so
// /tmp/foo and /private/tmp/foo (a macOS idiosyncrasy) don't create two
// sandboxes.
func deriveHereSandboxName(cwd string) (string, error) {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", cwd, err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	base := sanitiseName(filepath.Base(abs))
	if base == "" {
		base = "root"
	}

	candidate := base
	if existing, err := store.Load(candidate); err == nil {
		if matchesHereSandbox(existing, abs) {
			return candidate, nil
		}
		// Collision: a different sandbox (an explicitly-named one, or another
		// directory with the same basename) already holds this key. Disambiguate.
		return candidate + "_" + pathHashSuffix(abs), nil
	}
	return candidate, nil
}

// pathHashSuffix is the short, stable disambiguator appended to a sandbox key
// when its basename collides — a 6-hex-char digest of the resolved directory.
func pathHashSuffix(abs string) string {
	h := sha256.Sum256([]byte(abs))
	return hex.EncodeToString(h[:3])
}

// matchesHereSandbox reports whether sb is the cwd-sandbox for cwd.
// isAnchored reports whether a sandbox is directory-bound (created by bare
// `clawk`). The persisted Anchor field is authoritative once set, but we also
// accept the original signal — an InPlace phase — so discrimination is correct
// for records the backfill migration hasn't touched yet.
func isAnchored(sb *config.Sandbox) bool {
	if sb.Anchor != "" {
		return true
	}
	for i := range sb.Phases {
		if sb.Phases[i].InPlace {
			return true
		}
	}
	return false
}

func matchesHereSandbox(sb *config.Sandbox, cwd string) bool {
	// Compare canonical forms on both sides: records created before
	// hereCWDAndName canonicalised carry raw anchors (/tmp/x instead of
	// /private/tmp/x on macOS), and they must keep matching rather than
	// being treated as basename collisions.
	cwd = canonicalPath(cwd)
	// Prefer the first-class Anchor: it's the binding's source of truth and
	// survives even if the phase metadata is unusual. Fall back to the InPlace
	// phase for records loaded before Anchor was populated.
	if sb.Anchor != "" && canonicalPath(sb.Anchor) == cwd {
		return true
	}
	for _, p := range sb.Phases {
		if p.InPlace && canonicalPath(p.Worktree) == cwd {
			return true
		}
	}
	return false
}

// findTicketSandboxesRootedAt returns the names of ticket-mode sandboxes
// (i.e. anything *not* anchored to a directory) that share `cwd` as
// a logical root — either a phase's source repo lives at `cwd`, or it
// lives somewhere under `cwd` (the workspace-root case where one
// clawk.mod `includes` sibling repos).
//
// Used at cwd-inference points so users with both flavours of sandbox
// in the same directory get a hint pointing at the ticket-mode names —
// the cwd-mode wins by design (zero-arg `clawk` is unambiguous), but
// we don't want the ticket-mode sandbox to feel orphaned.
func findTicketSandboxesRootedAt(cwd string) []string {
	root, err := filepath.Abs(cwd)
	if err != nil {
		return nil
	}
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	list, err := store.List()
	if err != nil {
		return nil
	}
	var out []string
	for i := range list {
		sb := &list[i]
		if isAnchored(sb) {
			continue
		}
		for _, p := range sb.Phases {
			if pathContainsOrEquals(root, p.Repo) {
				out = append(out, sb.Name)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// pathContainsOrEquals reports whether `child` is `parent` itself or a
// path nested inside it. Both paths are absolutised + symlink-resolved
// before comparison so /tmp vs /private/tmp on macOS doesn't trip us.
func pathContainsOrEquals(parent, child string) bool {
	abs, err := filepath.Abs(child)
	if err != nil {
		return false
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		abs = r
	}
	if abs == parent {
		return true
	}
	rel, err := filepath.Rel(parent, abs)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// emitCwdShadowHint is called from cwd-inference points (bare `clawk`,
// `clawk run` with no name) after we've resolved to the cwd-mode
// sandbox. If any ticket-mode sandboxes share `cwd` as a root, write a
// one-line stderr hint listing them so the user knows the cwd-mode
// resolution wasn't the full picture.
func emitCwdShadowHint(w io.Writer, cwd string) {
	names := findTicketSandboxesRootedAt(cwd)
	if len(names) == 0 {
		return
	}
	fmt.Fprintf(w, "hint: ticket-mode sandbox(es) also rooted here: %s — "+
		"use 'clawk attach <name>' to attach to one of them.\n",
		strings.Join(names, ", "))
}

// emitCwdShadowHintIfInferred fires the shadow hint only when the sandbox
// was inferred from cwd — an explicit name means the user already
// disambiguated, so the hint would be noise.
func emitCwdShadowHintIfInferred(w io.Writer, explicitName string) {
	if explicitName != "" {
		return
	}
	if cwd, err := os.Getwd(); err == nil {
		emitCwdShadowHint(w, cwd)
	}
}

// printDetachHint reminds the user the sandbox outlives the session
// that just ended — without it the lifecycle is invisible and people
// assume exiting the agent stopped the VM (or worse, that re-running
// will rebuild from scratch). Mirrors the tmux detach message shape.
func printDetachHint(w io.Writer, sb *config.Sandbox) {
	if isAnchored(sb) {
		fmt.Fprintf(w, "\nsandbox is still running — reattach: clawk · shell: clawk run shell · stop: clawk down\n")
		return
	}
	name := sb.DisplayName()
	fmt.Fprintf(w, "\nsandbox %q is still running — reattach: clawk attach %s · shell: clawk run shell %s · stop: clawk down %s\n",
		name, name, name, name)
}
