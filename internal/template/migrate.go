package template

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrAlreadyTyped reports that a file already parses as the typed-block
// grammar and needs no migration.
var ErrAlreadyTyped = errors.New("file already uses the typed-block grammar")

// nameDirectiveRe matches the retired top-level `name <ident>` directive,
// tolerating a trailing comment.
var nameDirectiveRe = regexp.MustCompile(`^\s*name\s+(\S+)\s*(?:(?://|#).*)?$`)

// vmScalarRe matches vm-block scalars that the earliest releases accepted
// at top level (before the `vm ( … )` grouping existed). Such files are
// still in the wild; the migrator hoists these lines into a vm block.
var vmScalarRe = regexp.MustCompile(
	`^\s*(provider|cpu|memory|memory_max|nested|idle_timeout|image|kernel)\b`)

// vmBlockOpenRe matches a multi-line `vm (` opener the hoisted scalars can
// be inserted into. A single-line `vm ( … )` doesn't match — the migrator
// synthesizes a second vm block instead, which the parser accepts.
var vmBlockOpenRe = regexp.MustCompile(`^\s*vm\s*\(\s*(?:(?://|#).*)?$`)

// netEntryRe matches allow/deny entries at top level — the same
// pre-grouping era as the vm scalars, before `network ( … )` existed.
var netEntryRe = regexp.MustCompile(`^\s*(allow|deny)\b`)

// netBlockOpenRe matches a multi-line `network (` opener.
var netBlockOpenRe = regexp.MustCompile(`^\s*network\s*\(\s*(?:(?://|#).*)?$`)

// MigrateFlat rewrites a pre-cutover flat clawk.mod (or clawk.work) body
// into the typed-block grammar: the top-level `name X` directive, if any,
// moves into the sandbox header and everything else is indented into
// `sandbox [X] ( … )`. The transform is text-level so comments, blank
// lines, and alignment survive verbatim; the result is validated with
// ParseFileString before being returned, so a file the migrator can't
// handle errors instead of being rewritten into something broken.
//
// Input that already parses as the typed grammar returns ErrAlreadyTyped.
func MigrateFlat(src string) (string, error) {
	if _, err := ParseFileString(src); err == nil {
		return "", ErrAlreadyTyped
	}

	name := ""
	var body []string
	// Legacy top-level lines from before the vm/network grouping existed,
	// hoisted into their blocks below. Still in the wild.
	var vmScalars, netEntries []string
	depth := 0
	vmOpenAt, netOpenAt := -1, -1 // body indexes of multi-line block openers
	for _, line := range strings.Split(src, "\n") {
		if depth == 0 && name == "" {
			if m := nameDirectiveRe.FindStringSubmatch(line); m != nil {
				name = m[1]
				continue // the directive moves into the header
			}
		}
		if depth == 0 && vmScalarRe.MatchString(line) {
			vmScalars = append(vmScalars, "        "+strings.TrimSpace(line))
			continue
		}
		if depth == 0 && netEntryRe.MatchString(line) {
			netEntries = append(netEntries, "        "+strings.TrimSpace(line))
			continue
		}
		if depth == 0 && vmOpenAt < 0 && vmBlockOpenRe.MatchString(line) {
			vmOpenAt = len(body)
		}
		if depth == 0 && netOpenAt < 0 && netBlockOpenRe.MatchString(line) {
			netOpenAt = len(body)
		}
		depth += parenDelta(line)
		if strings.TrimSpace(line) == "" {
			body = append(body, "")
		} else {
			body = append(body, "    "+line)
		}
	}
	// Insertions into existing blocks go highest-index-first so the other
	// opener's recorded index stays valid; synthesized blocks are prepended
	// afterwards (vm first, then network — the conventional order).
	type hoist struct {
		at      int // -1: synthesize
		opener  string
		entries []string
	}
	hoists := []hoist{
		{at: netOpenAt, opener: "    network (", entries: netEntries},
		{at: vmOpenAt, opener: "    vm (", entries: vmScalars},
	}
	if vmOpenAt > netOpenAt {
		hoists[0], hoists[1] = hoists[1], hoists[0]
	}
	var synth []string
	for _, h := range hoists {
		if len(h.entries) == 0 {
			continue
		}
		if h.at >= 0 {
			body = append(body[:h.at+1], append(h.entries, body[h.at+1:]...)...)
		} else {
			block := append([]string{h.opener}, append(h.entries, "    )", "")...)
			synth = append(block, synth...)
		}
	}
	body = append(synth, body...)
	// Trim trailing blank lines inside the block so the closing paren
	// hugs the content the way hand-written files do.
	for len(body) > 0 && body[len(body)-1] == "" {
		body = body[:len(body)-1]
	}

	header := "sandbox ("
	if name != "" {
		header = fmt.Sprintf("sandbox %s (", name)
	}
	out := header + "\n" + strings.Join(body, "\n") + "\n)\n"

	if _, err := ParseFileString(out); err != nil {
		return "", fmt.Errorf("automatic migration failed — wrap the body in `sandbox ( ... )` by hand: %w", err)
	}
	return out, nil
}

// parenDelta counts a line's net paren nesting, ignoring parens inside
// quoted strings and after a comment marker. The flat grammar only nests
// via block parens, so this is enough for the migrator's depth tracking.
func parenDelta(line string) int {
	delta := 0
	inString := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inString:
			if c == '"' {
				inString = false
			}
		case c == '"':
			inString = true
		case c == '#':
			return delta
		case c == '/' && i+1 < len(line) && line[i+1] == '/':
			return delta
		case c == '(':
			delta++
		case c == ')':
			delta--
		}
	}
	return delta
}
