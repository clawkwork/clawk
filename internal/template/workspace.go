package template

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Workspace describes a dev environment rooted at a clawk.mod. A multi-repo
// workspace is a clawk.mod whose sandbox block has `includes ( ... )`; a
// single-repo environment is the degenerate case with one synthesised repo.
type Workspace struct {
	Root  string    // absolute path to the directory containing the clawk.mod
	File  *Template // parsed workspace-level sandbox block (provider, allow, forwards, ...)
	Repos []Repo    // every repo included by the workspace, in declaration order

	// Policies collects every `policy <name> ( ... )` block declared across
	// the loaded files — workspace file and its overlay first (broader
	// scope), then each repo's clawk.mod and overlay. The create paths
	// register them into the host policy store, so later (nearer) blocks
	// win a name collision.
	Policies []PolicyDef
}

// Repo is one git repository brought into a sandbox by a workspace. The
// Clawkfile (if present at the repo root) carries that repo's allow /
// forwards / setup.
type Repo struct {
	Name      string    // human-facing name used by --only and display
	Path      string    // absolute path on host (the `includes` entry, resolved)
	RepoPath  string    // absolute path to the containing git repo
	Clawkfile *Template // nil if the repo has no Clawkfile at its root
}

// RepoFileName is the single clawk config filename. Whether it describes a
// multi-repo workspace or one repo is structural: a sandbox block with
// `includes ( ... )` is a workspace root. Profiles extend a clawk.mod with a
// "clawk.mod.<profile>" overlay file.
const RepoFileName = "clawk.mod"

// RetiredWorkspaceFileName is the pre-cutover multi-repo filename. It is
// never loaded — the loaders recognise it only to emit a rename hint.
const RetiredWorkspaceFileName = "clawk.work"

// RetiredWorkspaceFileError is the loader-level rename hint produced when a
// clawk.work is encountered anywhere the old loader accepted one.
func RetiredWorkspaceFileError(path string) error {
	return fmt.Errorf("%s: clawk.work is retired — rename to clawk.mod and wrap "+
		"the body in `sandbox <name> ( includes ( ... ) ... )` — or run 'clawk mod migrate' there", path)
}

// ErrNoWorkspace is returned when FindWorkspace can't find a workspace
// clawk.mod (a sandbox block with `includes`) in dir or any ancestor.
var ErrNoWorkspace = errors.New(
	"no workspace clawk.mod (sandbox block with 'includes') found in this directory or any parent")

// FindWorkspace is FindWorkspaceWithProfile with no profile.
func FindWorkspace(dir string) (*Workspace, error) {
	return FindWorkspaceWithProfile(dir, "")
}

// FindWorkspaceWithProfile walks up from dir looking for a workspace root:
// the nearest clawk.mod whose sandbox block has `includes`. An includeless
// clawk.mod along the way is a single-repo file — the walk continues past it,
// so a workspace root above still wins (exactly the old clawk.work walk).
// Parse failures along the walk surface as errors rather than being skipped:
// a leftover flat-grammar file must produce its migration hint, not a silent
// fall-back to defaults. A clawk.work at any level gets the rename hint.
func FindWorkspaceWithProfile(dir, profile string) (*Workspace, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	for {
		if legacy := filepath.Join(abs, RetiredWorkspaceFileName); fileExists(legacy) {
			return nil, RetiredWorkspaceFileError(legacy)
		}
		p := filepath.Join(abs, RepoFileName)
		if fileExists(p) {
			f, err := loadFile(p)
			if err != nil {
				return nil, err
			}
			if f.Sandbox != nil && len(f.Sandbox.Includes) > 0 {
				return loadWorkspaceParsed(p, f, profile)
			}
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return nil, ErrNoWorkspace
		}
		abs = parent
	}
}

// fileExists reports whether path stats successfully. Plain os.Stat check —
// the loaders only need presence, not metadata.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// LoadWorkspace is LoadWorkspaceWithProfile with no profile.
func LoadWorkspace(path string) (*Workspace, error) {
	return LoadWorkspaceWithProfile(path, "")
}

// LoadWorkspaceWithProfile parses a workspace clawk.mod, resolves every
// listed include, and applies an optional profile overlay to both the
// workspace file itself and each repo's Clawkfile.
//
// If profile is non-empty:
//   - "<workspace-path>.<profile>" (e.g. clawk.mod.investigation) is loaded
//     and merged onto the base workspace, if it exists. Missing overlay is
//     tolerated — the profile may only affect per-repo policy.
//   - Each repo's "clawk.mod.<profile>" is loaded and merged onto its base
//     clawk.mod, if either exists.
//
// A profile name that matches NOTHING across the workspace is treated as an
// error, so typos surface loudly.
func LoadWorkspaceWithProfile(path, profile string) (*Workspace, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if filepath.Base(abs) == RetiredWorkspaceFileName {
		return nil, RetiredWorkspaceFileError(abs)
	}
	f, err := loadFile(abs)
	if err != nil {
		return nil, err
	}
	return loadWorkspaceParsed(abs, f, profile)
}

// loadWorkspaceParsed is the body of LoadWorkspaceWithProfile once the
// workspace file is parsed — shared with the FindWorkspace walk, which has
// already read the file to check for includes.
func loadWorkspaceParsed(abs string, f *File, profile string) (*Workspace, error) {
	tmpl := f.Sandbox
	if tmpl == nil {
		tmpl = &Template{}
	}
	// Lifecycle hooks belong elsewhere; flag them to guide migrations.
	// `on create / on up / on down / on enter` are repo-local because each
	// phase has its own worktree and tooling — running them at workspace scope
	// would be ambiguous about CWD.
	if err := rejectLifecycleAtWorkspace(abs, tmpl); err != nil {
		return nil, err
	}
	policies := append([]PolicyDef(nil), f.Policies...)

	profileMatched := false

	// Overlay workspace-level profile (clawk.mod.<profile>) if present.
	if profile != "" {
		overlayPath := abs + "." + profile
		overFile, err := maybeParseOverlay(overlayPath)
		if err != nil {
			return nil, err
		}
		if overFile != nil {
			profileMatched = true
			if over := overFile.Sandbox; over != nil {
				if err := rejectLifecycleAtWorkspace(overlayPath, over); err != nil {
					return nil, err
				}
				tmpl.Merge(over)
			}
			policies = append(policies, overFile.Policies...)
		}
	}

	root := filepath.Dir(abs)
	ws := &Workspace{Root: root, File: tmpl, Policies: policies}

	for _, rawInclude := range tmpl.Includes {
		repo, repoPolicies, matched, err := resolveRepoWithProfile(root, rawInclude, profile)
		if err != nil {
			return nil, err
		}
		if matched {
			profileMatched = true
		}
		ws.Repos = append(ws.Repos, repo)
		ws.Policies = append(ws.Policies, repoPolicies...)
	}

	if err := checkNameCollisions(ws.Repos); err != nil {
		return nil, err
	}
	if profile != "" && !profileMatched {
		return nil, fmt.Errorf(
			"profile %q matched no overlay file (looked for %s.%s beside the workspace file and in each repo)",
			profile, RepoFileName, profile)
	}
	return ws, nil
}

// loadFile reads and parses a single typed-block clawk file with path-
// prefixed errors, folding a sandbox block's header name into Template.Name
// so downstream naming works off one field.
func loadFile(absPath string) (*File, error) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, err
	}
	f, err := ParseFileString(string(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", absPath, err)
	}
	if f.Sandbox != nil && f.Sandbox.SandboxName != "" {
		f.Sandbox.Name = f.Sandbox.SandboxName
	}
	return f, nil
}

// maybeParseOverlay returns nil, nil if the path does not exist; the parsed
// file if it does; error for parse errors or other file-system issues.
func maybeParseOverlay(path string) (*File, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return loadFile(path)
}

// LoadStandaloneClawkfile is LoadStandaloneClawkfileWithProfile with no
// profile.
func LoadStandaloneClawkfile(dir string) (*Workspace, error) {
	return LoadStandaloneClawkfileWithProfile(dir, "")
}

// LoadStandaloneClawkfileWithProfile reads a clawk.mod at the given
// directory and synthesises a single-repo workspace rooted at that repo,
// optionally overlaying a clawk.mod.<profile> file.
func LoadStandaloneClawkfileWithProfile(dir, profile string) (*Workspace, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	clawkfilePath := filepath.Join(abs, RepoFileName)
	if _, err := os.Stat(clawkfilePath); err != nil {
		// Wrap the stat error: callers distinguish "no clawk.mod here"
		// (expected, silent — errors.Is ErrNotExist) from a clawk.mod they
		// couldn't read (worth a warning). Formatting it away made the
		// absent-file case warn on every bare `clawk` in a plain directory.
		return nil, fmt.Errorf("no %s in %s: %w", RepoFileName, abs, err)
	}
	repo, policies, matched, err := resolveRepoWithProfile(abs, abs, profile)
	if err != nil {
		return nil, err
	}
	if profile != "" && !matched {
		return nil, fmt.Errorf("profile %q has no matching overlay in %s",
			profile, abs)
	}
	return &Workspace{
		Root:     repo.RepoPath,
		File:     &Template{},
		Repos:    []Repo{repo},
		Policies: policies,
	}, nil
}

// LoadClawkfilePathWithProfile loads an explicitly-given clawk.mod path,
// picking workspace or single-repo semantics from the file's own shape: a
// sandbox block with `includes` is a workspace root, anything else is the
// standalone single-repo case rooted at the file's directory.
func LoadClawkfilePathWithProfile(path, profile string) (*Workspace, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	f, err := loadFile(abs)
	if err != nil {
		return nil, err
	}
	if f.Sandbox != nil && len(f.Sandbox.Includes) > 0 {
		return loadWorkspaceParsed(abs, f, profile)
	}
	return LoadStandaloneClawkfileWithProfile(filepath.Dir(abs), profile)
}

// WorkspaceFromGitRepo synthesises a single-repo workspace using dir's
// containing git repo, with no Clawkfile. Used by `clawk work` as a
// last-resort fallback when no clawk.mod is present: the repo gets
// defaults only (no forwards, no setup, default allow list) and the user
// keeps a one-line invocation. Errors when dir is not inside a git repo.
func WorkspaceFromGitRepo(dir string) (*Workspace, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	repoRoot, err := FindGitRepo(abs)
	if err != nil {
		return nil, err
	}
	return &Workspace{
		Root: repoRoot,
		File: &Template{},
		Repos: []Repo{{
			Name:     filepath.Base(repoRoot),
			Path:     repoRoot,
			RepoPath: repoRoot,
		}},
	}, nil
}

// resolveRepoWithProfile resolves a workspace repo entry, optionally
// overlaying the repo's clawk.mod.<profile> onto its base clawk.mod
// when profile is non-empty. The returned PolicyDefs are the policy blocks
// declared beside the repo's sandbox block (base file first, then overlay).
// The returned bool reports whether the overlay file existed and was
// applied — the workspace loader uses it to verify that at least SOME
// overlay file matched the requested profile.
func resolveRepoWithProfile(workspaceRoot, raw, profile string) (Repo, []PolicyDef, bool, error) {
	repo, policies, err := resolveRepoBase(workspaceRoot, raw)
	if err != nil {
		return repo, policies, false, err
	}
	if profile == "" {
		return repo, policies, false, nil
	}
	overlayPath := filepath.Join(repo.RepoPath, RepoFileName+"."+profile)
	overFile, err := maybeParseOverlay(overlayPath)
	if err != nil {
		return repo, policies, false, err
	}
	if overFile == nil {
		return repo, policies, false, nil
	}
	policies = append(policies, overFile.Policies...)
	over := overFile.Sandbox
	if over == nil {
		return repo, policies, true, nil
	}
	if len(over.Includes) > 0 {
		return repo, policies, false, fmt.Errorf(
			"%s: profile overlays cannot declare 'includes'",
			overlayPath)
	}
	if repo.Clawkfile == nil {
		repo.Clawkfile = over
	} else {
		repo.Clawkfile.Merge(over)
	}
	// Name overrides from an overlay take effect here. Collision detection
	// runs against the post-overlay name.
	if over.Name != "" {
		repo.Name = over.Name
	}
	return repo, policies, true, nil
}

// resolveRepoBase expands a raw include string to an absolute path, walks
// up to find the enclosing .git directory, and loads a clawk.mod at that
// root if one exists.
func resolveRepoBase(workspaceRoot, raw string) (Repo, []PolicyDef, error) {
	expanded, err := ExpandPath(raw)
	if err != nil {
		return Repo{}, nil, fmt.Errorf("expanding %q: %w", raw, err)
	}
	if !filepath.IsAbs(expanded) {
		expanded = filepath.Join(workspaceRoot, expanded)
	}
	absInclude, err := filepath.Abs(expanded)
	if err != nil {
		return Repo{}, nil, err
	}
	if info, err := os.Stat(absInclude); err != nil {
		return Repo{}, nil, fmt.Errorf("include path %s: %w", absInclude, err)
	} else if !info.IsDir() {
		return Repo{}, nil, fmt.Errorf("include path %s is not a directory", absInclude)
	}

	repoRoot, err := FindGitRepo(absInclude)
	if err != nil {
		return Repo{}, nil, fmt.Errorf("include %s: %w", absInclude, err)
	}

	repo := Repo{
		Name:     filepath.Base(repoRoot),
		Path:     absInclude,
		RepoPath: repoRoot,
	}

	// Load the repo's Clawkfile if present. A repo without a Clawkfile is
	// valid — it picks up workspace-level defaults only.
	clawkfilePath := filepath.Join(repoRoot, RepoFileName)
	f, err := loadFile(clawkfilePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return repo, nil, nil
		}
		return repo, nil, err
	}
	if tmpl := f.Sandbox; tmpl != nil {
		if len(tmpl.Includes) > 0 {
			// Reached for a workspace member whose own clawk.mod declares
			// includes (nested workspaces) and for a workspace root loaded
			// through a single-repo path (here-mode) — both unsupported.
			return repo, nil, fmt.Errorf(
				"%s: 'includes' declares a workspace root; it cannot be nested or loaded as a single repo",
				clawkfilePath)
		}
		if tmpl.Name != "" {
			repo.Name = tmpl.Name
		}
		repo.Clawkfile = tmpl
	}
	return repo, f.Policies, nil
}

// checkNameCollisions verifies every repo has a unique Name and returns an
// actionable error if two collide. Picked up by workspace-load rather than
// at run time so the user hears about it as soon as they edit the file.
func checkNameCollisions(repos []Repo) error {
	byName := make(map[string][]string)
	for _, r := range repos {
		byName[r.Name] = append(byName[r.Name], r.Path)
	}
	for name, paths := range byName {
		if len(paths) > 1 {
			return fmt.Errorf(
				"repo name %q collides across %s — name one of the sandbox blocks (sandbox <name> ( ... ))",
				name, strings.Join(paths, ", "))
		}
	}
	return nil
}

// FilterRepos returns a new workspace containing only repos whose Name is in
// the `only` list. Unknown names produce an error listing what IS known —
// typos should surface loudly.
func (w *Workspace) FilterRepos(only []string) (*Workspace, error) {
	if len(only) == 0 {
		return w, nil
	}
	wanted := make(map[string]bool, len(only))
	for _, o := range only {
		wanted[strings.TrimSpace(o)] = true
	}
	var kept []Repo
	for _, r := range w.Repos {
		if wanted[r.Name] {
			kept = append(kept, r)
			delete(wanted, r.Name)
		}
	}
	if len(wanted) > 0 {
		var missing []string
		for k := range wanted {
			missing = append(missing, k)
		}
		known := make([]string, 0, len(w.Repos))
		for _, r := range w.Repos {
			known = append(known, r.Name)
		}
		return nil, fmt.Errorf("unknown repo(s): %s (known: %s)",
			strings.Join(missing, ", "), strings.Join(known, ", "))
	}
	return &Workspace{Root: w.Root, File: w.File, Repos: kept, Policies: w.Policies}, nil
}

// rejectLifecycleAtWorkspace flags any of the four `on <event>` lists set
// on a workspace-position template. Lifecycle hooks are repo-local because
// each phase has its own worktree and tool ecosystem — running them at
// workspace scope would be ambiguous about which CWD applies.
func rejectLifecycleAtWorkspace(path string, tmpl *Template) error {
	switch {
	case len(tmpl.OnCreate) > 0:
		return fmt.Errorf(
			"%s: 'on create' belongs in a per-repo clawk.mod, not the workspace", path)
	case len(tmpl.OnUp) > 0:
		return fmt.Errorf(
			"%s: 'on up' (or legacy 'setup') belongs in a per-repo clawk.mod, not the workspace", path)
	case len(tmpl.OnDown) > 0:
		return fmt.Errorf(
			"%s: 'on down' belongs in a per-repo clawk.mod, not the workspace", path)
	case len(tmpl.OnEnter) > 0:
		return fmt.Errorf(
			"%s: 'on enter' belongs in a per-repo clawk.mod, not the workspace", path)
	}
	return nil
}

// FindGitRepo returns the nearest ancestor of dir containing a .git entry,
// inclusive of dir itself. Returns an error if no .git is found.
func FindGitRepo(dir string) (string, error) {
	cur := dir
	for {
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			return cur, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", fmt.Errorf("no .git ancestor found (checked up to %s)", cur)
		}
		cur = parent
	}
}
