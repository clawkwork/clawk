package template

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// SkillResolver turns a SkillRef into a pinned version. The returned
// string must satisfy IsResolvedVersion or RewriteSkillVersions errors —
// half-resolved values would defeat the whole point of tidy.
//
// Returning ErrSkipResolution leaves the entry untouched (used for local
// skills, which never carry a version).
type SkillResolver interface {
	Resolve(SkillRef) (string, error)
}

// SkillResolverFunc adapts a plain function into the SkillResolver
// interface. Useful for tests and the stub resolver below.
type SkillResolverFunc func(SkillRef) (string, error)

// Resolve implements SkillResolver.
func (f SkillResolverFunc) Resolve(s SkillRef) (string, error) { return f(s) }

// ErrSkipResolution is the sentinel a SkillResolver returns to leave a
// skill untouched. Local skills always skip; distributed skills with an
// already-pinned version may skip.
var ErrSkipResolution = errors.New("skip resolution")

// StubRemoteResolver is the resolver used by the initial implementation:
// it pins nothing, just shapes the error so the user sees an actionable
// "implement me" rather than a silent miss. Replaced by a real git-fetch
// resolver in a follow-up.
var StubRemoteResolver SkillResolver = SkillResolverFunc(func(s SkillRef) (string, error) {
	if s.Kind != SkillKindDistributed {
		return "", ErrSkipResolution
	}
	if IsResolvedVersion(s.Version) {
		return "", ErrSkipResolution
	}
	return "", fmt.Errorf(
		"remote skill resolution is not yet implemented; pin %s manually with a vMAJOR.MINOR.PATCH tag",
		s.Path)
})

// TidyResult describes what RewriteSkillVersions changed. Useful for
// `clawk mod tidy` to print a one-line "rewrote N versions" summary
// without the caller diff'ing strings.
type TidyResult struct {
	Rewrites []SkillRewrite
	Source   string // post-rewrite source. Equal to input when Rewrites is empty.
}

// SkillRewrite is a single (path → new version) rewrite that happened.
type SkillRewrite struct {
	Path        string
	OldVersion  string // empty when a version is being inserted for the first time
	NewVersion  string
	Line        int
	InsertedNew bool // true when no version token existed before the rewrite
}

// RewriteSkillVersions applies a SkillResolver to every distributed skill
// in the parsed template and returns a new source with the resolved
// versions spliced in. Comments, alignment, blank lines, and entry order
// are preserved — only version tokens change.
//
// Local skills are never rewritten; the resolver should return
// ErrSkipResolution for them, but RewriteSkillVersions also short-circuits
// before calling the resolver for safety.
//
// The function does not parse — it works off `tmpl.Skills`, which already
// carries the source positions populated by the parser.
func RewriteSkillVersions(src string, tmpl *Template, resolver SkillResolver) (TidyResult, error) {
	res := TidyResult{Source: src}
	if tmpl == nil || len(tmpl.Skills) == 0 || resolver == nil {
		return res, nil
	}

	type splice struct {
		start, end  int
		replacement string
		rewrite     SkillRewrite
	}
	var splices []splice

	for _, s := range tmpl.Skills {
		if s.Kind != SkillKindDistributed {
			continue
		}
		newVer, err := resolver.Resolve(s)
		if err != nil {
			if errors.Is(err, ErrSkipResolution) {
				continue
			}
			return res, err
		}
		if !IsResolvedVersion(newVer) {
			return res, fmt.Errorf(
				"resolver for %s returned %q which is not a pinned version",
				s.Path, newVer)
		}
		if newVer == s.Version {
			continue
		}
		if s.VersionLine == 0 {
			// No version token — splice one in immediately after the path.
			pathStart, err := offsetOf(src, s.Line, s.Col)
			if err != nil {
				return res, err
			}
			insertAt := pathStart + len(s.Path)
			splices = append(splices, splice{
				start: insertAt, end: insertAt,
				replacement: " " + newVer,
				rewrite: SkillRewrite{
					Path: s.Path, NewVersion: newVer, Line: s.Line,
					InsertedNew: true,
				},
			})
			continue
		}
		verStart, err := offsetOf(src, s.VersionLine, s.VersionCol)
		if err != nil {
			return res, err
		}
		verEnd := verStart + len(s.Version)
		splices = append(splices, splice{
			start: verStart, end: verEnd,
			replacement: newVer,
			rewrite: SkillRewrite{
				Path:       s.Path,
				OldVersion: s.Version,
				NewVersion: newVer,
				Line:       s.VersionLine,
			},
		})
	}

	if len(splices) == 0 {
		return res, nil
	}

	// Apply splices from the highest start offset down so earlier splices
	// keep valid offsets. Equal starts get tiebroken by end so an insertion
	// at the same point as a replacement does not overlap.
	sort.Slice(splices, func(i, j int) bool {
		if splices[i].start != splices[j].start {
			return splices[i].start > splices[j].start
		}
		return splices[i].end > splices[j].end
	})

	var sb strings.Builder
	sb.Grow(len(src) + 32*len(splices))
	cursor := len(src)
	// We iterate from the end of the file backwards. Build the post-rewrite
	// string by interleaving the segments after / between / before splices.
	// To keep the code straightforward, gather pieces in reverse, then flip.
	var pieces []string
	for _, s := range splices {
		if s.end > cursor {
			return res, fmt.Errorf("internal: overlapping splices at %d", s.end)
		}
		pieces = append(pieces, src[s.end:cursor], s.replacement)
		cursor = s.start
	}
	pieces = append(pieces, src[:cursor])
	for i := len(pieces) - 1; i >= 0; i-- {
		sb.WriteString(pieces[i])
	}
	res.Source = sb.String()

	res.Rewrites = make([]SkillRewrite, 0, len(splices))
	// Splices were sorted by descending start; report Rewrites in source
	// order (ascending) so a `clawk mod tidy` summary reads top-to-bottom.
	for i := len(splices) - 1; i >= 0; i-- {
		res.Rewrites = append(res.Rewrites, splices[i].rewrite)
	}
	return res, nil
}

// offsetOf converts a (line, col) pair as recorded by the lexer into a
// byte offset into src. Both line and col are 1-based; col counts bytes
// within the line (the lexer increments per byte).
//
// The lexer guarantees positions point inside src, so an out-of-range
// position here is a programmer error — reported as an error rather than
// a panic so callers can wrap context.
func offsetOf(src string, line, col int) (int, error) {
	if line < 1 || col < 1 {
		return 0, fmt.Errorf("invalid position line %d col %d", line, col)
	}
	cur := 1
	i := 0
	for i < len(src) && cur < line {
		if src[i] == '\n' {
			cur++
		}
		i++
	}
	if cur != line {
		return 0, fmt.Errorf("line %d not found (source has %d lines)", line, cur)
	}
	off := i + col - 1
	if off > len(src) {
		return 0, fmt.Errorf("col %d past end of line %d", col, line)
	}
	return off, nil
}
