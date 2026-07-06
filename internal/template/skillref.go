package template

import (
	"fmt"
	"regexp"
	"strings"
)

// SkillRef is a single entry in a `skills (...)` block.
//
// Three shapes are valid:
//
//   - SkillKindLocalHome: ~/foo or $HOME/foo — a path under the user's
//     home directory. Versions are forbidden.
//   - SkillKindLocalWorkspace: ./foo — a path relative to the workspace
//     root. Versions are forbidden.
//   - SkillKindDistributed: <host.tld>/path — a remote skill addressed
//     like a Go module. Versions are required after `clawk mod tidy`;
//     before tidy, branch names and the literal "latest" are accepted as
//     resolution inputs that will be rewritten.
type SkillRef struct {
	// Path is the raw entry as written in clawk.mod, before any
	// expansion of ~ or $HOME. Tidy preserves it verbatim and only ever
	// rewrites Version.
	Path string

	// Version is empty for local skills. For distributed skills it is
	// either a tag ("v1.2.3"), a Go-style pseudo-version
	// ("v0.0.0-yyyymmddhhmmss-12charSHA"), or — only as input to tidy —
	// a branch name / "latest" / short SHA that tidy will resolve and
	// rewrite.
	Version string

	// Kind classifies the path shape. Set by ClassifySkillPath; the
	// parser populates it after each entry is read.
	Kind SkillKind

	// Line / Col anchor diagnostics back to the source. The rewriter
	// also uses them to find the byte range to patch.
	Line int
	Col  int

	// VersionLine / VersionCol point at the version token, or zero
	// when no version was written. The rewriter uses these to splice
	// resolved versions in place.
	VersionLine int
	VersionCol  int
}

// SkillKind classifies the resolution path of a SkillRef.
type SkillKind int

const (
	// SkillKindUnknown is the zero value, returned for paths that
	// match no rule. Callers should treat it as a parse error.
	SkillKindUnknown SkillKind = iota
	SkillKindLocalHome
	SkillKindLocalWorkspace
	SkillKindDistributed
)

func (k SkillKind) String() string {
	switch k {
	case SkillKindLocalHome:
		return "local-home"
	case SkillKindLocalWorkspace:
		return "local-workspace"
	case SkillKindDistributed:
		return "distributed"
	}
	return "unknown"
}

// pseudoVersionRE matches Go-style pseudo-versions:
// v0.0.0-yyyymmddhhmmss-12charcommit. Real semver tags like v1.2.3 also
// match the broader IsValidVersion rule below; pseudo-versions get
// their own check because tidy generates them.
var pseudoVersionRE = regexp.MustCompile(
	`^v\d+\.\d+\.\d+-\d{14}-[0-9a-f]{12}$`)

// semverRE matches a relaxed semver "vMAJOR.MINOR.PATCH[-suffix]" form.
// Pre-release suffixes like -rc1 or -beta.2 are allowed; build
// metadata (+...) is also tolerated. We deliberately do not enforce
// strict SemVer 2.0.0 — clawk mod tidy normalises anything looser into
// pseudo-version form on rewrite.
var semverRE = regexp.MustCompile(
	`^v\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)

// shortSHARE matches a 7-12 char hex string. Used as a tidy input
// shorthand: the user can write `repo abc1234` and tidy expands it.
var shortSHARE = regexp.MustCompile(`^[0-9a-f]{7,12}$`)

// branchNameRE accepts the tidy-input shapes: arbitrary branch refs,
// "latest", "main", "master", etc. No anchoring to specific names —
// any non-version-looking, non-empty token is a candidate, and the
// resolver decides whether it actually resolves.
var branchNameRE = regexp.MustCompile(`^[A-Za-z0-9._/+-]+$`)

// ClassifySkillPath maps a raw entry path to its SkillKind. It does
// not touch the filesystem — the classification is purely lexical so
// the parser stays decoupled from disk state.
func ClassifySkillPath(path string) SkillKind {
	switch {
	case strings.HasPrefix(path, "~/"), path == "~",
		strings.HasPrefix(path, "$HOME/"), path == "$HOME":
		return SkillKindLocalHome
	case strings.HasPrefix(path, "./"), strings.HasPrefix(path, "../"):
		return SkillKindLocalWorkspace
	case isDistributedPath(path):
		return SkillKindDistributed
	}
	return SkillKindUnknown
}

// isDistributedPath returns true when path looks like
// <host.tld>/<sub-path> — the first segment must contain a dot
// (the hostname check) and there must be at least one slash. This is
// the same heuristic Go's module-path validator uses for the
// host-portion check.
func isDistributedPath(path string) bool {
	slash := strings.IndexByte(path, '/')
	if slash <= 0 {
		return false
	}
	host := path[:slash]
	rest := path[slash+1:]
	if rest == "" {
		return false
	}
	// Hostname must contain at least one dot to be unambiguous; this
	// is also what rules out "local/foo" without needing reserved
	// names.
	if !strings.Contains(host, ".") {
		return false
	}
	// First and last byte of host must be alphanumeric (rough check
	// against weird leading/trailing punctuation).
	if !isAlnum(host[0]) || !isAlnum(host[len(host)-1]) {
		return false
	}
	return true
}

func isAlnum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}

// IsResolvedVersion reports whether v is in a form that tidy would
// leave alone — a semver tag or a pseudo-version. Anything else (a
// branch name, "latest", a short SHA) is a tidy input that needs to
// be rewritten before the file can be considered pinned.
func IsResolvedVersion(v string) bool {
	if v == "" {
		return false
	}
	if pseudoVersionRE.MatchString(v) {
		return true
	}
	if semverRE.MatchString(v) {
		return true
	}
	return false
}

// IsTidyInputVersion reports whether v is a valid version-shaped token
// the user might write before tidy runs: branch names, "latest", short
// SHAs. A return of false means the parser should reject the token as
// not a version at all.
func IsTidyInputVersion(v string) bool {
	if v == "" {
		return false
	}
	if IsResolvedVersion(v) {
		return true
	}
	if v == "latest" {
		return true
	}
	if shortSHARE.MatchString(v) {
		return true
	}
	return branchNameRE.MatchString(v)
}

// validateSkillRef enforces the cross-cutting invariants on a SkillRef
// after the parser has filled in the fields. Returns a descriptive
// error suitable for prefixing with line/col.
func validateSkillRef(s SkillRef) error {
	switch s.Kind {
	case SkillKindUnknown:
		return fmt.Errorf(
			"skill %q: must be a path (~/..., ./..., or <host.tld>/...) — "+
				"bare names are not allowed", s.Path)
	case SkillKindLocalHome, SkillKindLocalWorkspace:
		if s.Version != "" {
			return fmt.Errorf(
				"skill %q: version %q is not allowed on a local path",
				s.Path, s.Version)
		}
	case SkillKindDistributed:
		if s.Version != "" && !IsTidyInputVersion(s.Version) {
			return fmt.Errorf(
				"skill %q: version %q is not a recognised version, branch, "+
					"or commit form", s.Path, s.Version)
		}
	}
	return nil
}
