package template

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// staticResolver pins distributed skills to a fixed version for testing.
// Local skills always skip via ErrSkipResolution.
type staticResolver struct {
	version string
}

func (s staticResolver) Resolve(ref SkillRef) (string, error) {
	if ref.Kind != SkillKindDistributed {
		return "", ErrSkipResolution
	}
	return s.version, nil
}

// TestRewriteReplacesVersion swaps an existing version token while
// preserving the surrounding alignment, comments, and blank lines.
func TestRewriteReplacesVersion(t *testing.T) {
	src := `# my project
sandbox (
    skills (
    ~/.claude/skills/idiomatic-go                            # local, untouched
    github.com/anthropics/skills/claude-api      main        # tidy this
    )
)
`
	tmpl, err := ParseString(src)
	require.NoError(t, err)
	res, err := RewriteSkillVersions(src, tmpl,
		staticResolver{version: "v0.0.0-20260508120000-abc1234567ab"})
	require.NoError(t, err)
	if !strings.Contains(res.Source, "v0.0.0-20260508120000-abc1234567ab") {
		t.Errorf("rewrite missing pseudo-version:\n%s", res.Source)
	}
	if strings.Contains(res.Source, "main") {
		t.Errorf("'main' should have been replaced:\n%s", res.Source)
	}
	// Comments and column alignment must survive verbatim.
	if !strings.Contains(res.Source, "# my project") {
		t.Errorf("preamble comment lost")
	}
	if !strings.Contains(res.Source, "# local, untouched") {
		t.Errorf("inline comment lost")
	}
	if len(res.Rewrites) != 1 || res.Rewrites[0].OldVersion != "main" {
		t.Errorf("unexpected rewrite log: %+v", res.Rewrites)
	}
}

// TestRewriteInsertsMissingVersion adds a version after a distributed
// path that didn't have one. Important for `clawk mod tidy` first-run.
func TestRewriteInsertsMissingVersion(t *testing.T) {
	src := `sandbox (
    skills (
    github.com/x/y
    )
)
`
	tmpl, err := ParseString(src)
	require.NoError(t, err)
	res, err := RewriteSkillVersions(src, tmpl, staticResolver{version: "v1.0.0"})
	require.NoError(t, err)
	if !strings.Contains(res.Source, "github.com/x/y v1.0.0") {
		t.Errorf("expected inserted version, got:\n%s", res.Source)
	}
	if len(res.Rewrites) != 1 || !res.Rewrites[0].InsertedNew {
		t.Errorf("expected one InsertedNew rewrite, got %+v", res.Rewrites)
	}
}

// TestRewriteSkipsLocal verifies local-home and local-workspace entries
// never change, even when the resolver returns a value (which it
// shouldn't for them — but the function must guard regardless).
func TestRewriteSkipsLocal(t *testing.T) {
	src := `sandbox (
    skills (
    ~/foo
    ./bar
    )
)
`
	tmpl, err := ParseString(src)
	require.NoError(t, err)
	resolverCalls := 0
	resolver := SkillResolverFunc(func(ref SkillRef) (string, error) {
		resolverCalls++
		return "v9.9.9", nil
	})
	res, err := RewriteSkillVersions(src, tmpl, resolver)
	require.NoError(t, err)
	if resolverCalls != 0 {
		t.Errorf("resolver should not be called for local skills, got %d calls", resolverCalls)
	}
	if res.Source != src {
		t.Errorf("source should be unchanged, got:\n%s", res.Source)
	}
}

// TestRewriteMultipleSplices applies several rewrites in one file. The
// reverse-iteration splice loop must keep earlier offsets valid.
func TestRewriteMultipleSplices(t *testing.T) {
	src := `sandbox (
    skills (
    github.com/a/one    main
    github.com/b/two    main
    github.com/c/three  v1.0.0
    )
)
`
	tmpl, err := ParseString(src)
	require.NoError(t, err)
	resolver := SkillResolverFunc(func(ref SkillRef) (string, error) {
		switch ref.Path {
		case "github.com/a/one":
			return "v2.0.0", nil
		case "github.com/b/two":
			return "v3.0.0", nil
		case "github.com/c/three":
			return "", ErrSkipResolution // already pinned
		}
		return "", ErrSkipResolution
	})
	res, err := RewriteSkillVersions(src, tmpl, resolver)
	require.NoError(t, err)
	if !strings.Contains(res.Source, "github.com/a/one    v2.0.0") {
		t.Errorf("first rewrite missing or alignment broken:\n%s", res.Source)
	}
	if !strings.Contains(res.Source, "github.com/b/two    v3.0.0") {
		t.Errorf("second rewrite missing or alignment broken:\n%s", res.Source)
	}
	if !strings.Contains(res.Source, "github.com/c/three  v1.0.0") {
		t.Errorf("pinned entry should not have changed")
	}
	if len(res.Rewrites) != 2 {
		t.Errorf("want 2 rewrites, got %d", len(res.Rewrites))
	}
	if res.Rewrites[0].Path != "github.com/a/one" {
		t.Errorf("rewrites should report top-to-bottom, got %+v", res.Rewrites)
	}
}

// TestRewriteRejectsUnpinnedResult: a resolver that returns a non-pinned
// value (a branch, latest, garbage) is a bug; the rewriter must reject it
// instead of writing it back to disk.
func TestRewriteRejectsUnpinnedResult(t *testing.T) {
	src := `sandbox (
    skills (
    github.com/a/one  main
    )
)
`
	tmpl, err := ParseString(src)
	require.NoError(t, err)
	_, err = RewriteSkillVersions(src, tmpl,
		SkillResolverFunc(func(SkillRef) (string, error) {
			return "main", nil // still a branch — not allowed as result
		}))
	require.Error(t, err)
	if !strings.Contains(err.Error(), "not a pinned version") {
		t.Errorf("error %q did not name the cause", err)
	}
}

// TestRewriteNoOpKeepsBytesIdentical: when no resolver applies, the
// returned Source must be byte-identical to the input.
func TestRewriteNoOpKeepsBytesIdentical(t *testing.T) {
	src := `sandbox (
    skills (
    ~/.claude/skills/idiomatic-go
    github.com/x/y v1.2.3
    )
)
`
	tmpl, err := ParseString(src)
	require.NoError(t, err)
	res, err := RewriteSkillVersions(src, tmpl, staticResolver{version: "v1.2.3"})
	require.NoError(t, err)
	if res.Source != src {
		t.Errorf("expected byte-identical source on no-op, got:\n%s", res.Source)
	}
	if len(res.Rewrites) != 0 {
		t.Errorf("expected no rewrites, got %v", res.Rewrites)
	}
}
