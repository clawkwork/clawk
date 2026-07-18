// Package template implements clawk's resource file language.
//
// A clawk.mod is a list of typed blocks — `sandbox [<name>] ( ... )`,
// `policy <name> ( ... )`, `namespace <name> ( ... )` — one filename for
// every shape. The sandbox block is a template describing a reusable sandbox
// (provider, repositories, network policy). It is NOT tied to a particular
// branch — the branch is supplied at `clawk work` time, so the same template
// can spawn many per-ticket sandboxes. A sandbox block with `includes` is a
// multi-repo workspace root; without it, the file configures its own repo.
//
// Example workspace clawk.mod:
//
//	sandbox acme (
//	    vm (
//	        provider vz
//	    )
//
//	    includes (
//	        ~/code/k8s-deploy
//	        ~/code/monorepo
//	    )
//
//	    network (
//	        use default corp-egress
//	        allow github.com
//	        allow *.github.com
//	        allow ip 10.0.0.5
//	        allow ip 192.168.10.0/24
//	    )
//	)
//
//	policy corp-egress (
//	    allow ip 10.20.0.0/16
//	    deny  telemetry.corp.com
//	)
package template

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// TokenKind identifies a lexical token category.
type TokenKind int

const (
	TokEOF TokenKind = iota
	TokNewline
	TokLParen
	TokRParen
	TokEquals // '=', separates an env entry's name from its value
	TokIdent
	TokString // double-quoted, supports \" \\ \n \t escapes
)

func (k TokenKind) String() string {
	switch k {
	case TokEOF:
		return "EOF"
	case TokNewline:
		return "newline"
	case TokLParen:
		return "'('"
	case TokRParen:
		return "')'"
	case TokEquals:
		return "'='"
	case TokIdent:
		return "identifier"
	case TokString:
		return "string"
	}
	return "unknown"
}

// Token is a single lexical unit with source position (for error messages).
type Token struct {
	Kind TokenKind
	Val  string // only meaningful for TokIdent
	Line int    // 1-based
	Col  int    // 1-based
}

func (t Token) String() string {
	if t.Kind == TokIdent || t.Kind == TokString {
		return fmt.Sprintf("%s(%q)", t.Kind, t.Val)
	}
	return t.Kind.String()
}

// Lex tokenises src into a flat slice. Errors are returned with line/col
// from the first malformed byte. Comments are stripped: "#" or "//" run to
// end of line, and "/* ... */" spans lines until its close. "//" and "/*"
// open a comment only at a token boundary, so a bare URL value such as
// https://host/path stays a single identifier; an inline comment needs
// whitespace before it.
//
// Whitespace other than newlines is separator-only; newlines are significant
// because our grammar uses them as statement terminators (like go.mod).
func Lex(src string) ([]Token, error) {
	var toks []Token
	line, col := 1, 1

	emit := func(k TokenKind, val string, l, c int) {
		toks = append(toks, Token{Kind: k, Val: val, Line: l, Col: c})
	}

	i := 0
	n := len(src)
	for i < n {
		r := rune(src[i])
		switch {
		case r == '\n':
			emit(TokNewline, "", line, col)
			i++
			line++
			col = 1
		case r == '\r':
			// Swallow CR; the LF on the next iteration emits the newline.
			i++
		case unicode.IsSpace(r):
			i++
			col++
		case r == '#':
			// Comment to end of line.
			for i < n && src[i] != '\n' {
				i++
			}
		case r == '/' && i+1 < n && src[i+1] == '/':
			// Line comment ("//") to end of line. A lone '/' stays a normal
			// identifier byte (paths like /etc/foo), so only the two-slash
			// sequence opens a comment.
			for i < n && src[i] != '\n' {
				i++
			}
		case r == '/' && i+1 < n && src[i+1] == '*':
			// Block comment ("/* ... */"), possibly spanning lines. The line
			// counter and column are tracked through the body so a token after
			// the close keeps an accurate position.
			startLine, startCol := line, col
			i += 2
			col += 2
			closed := false
			for i < n {
				if src[i] == '*' && i+1 < n && src[i+1] == '/' {
					i += 2
					col += 2
					closed = true
					break
				}
				if src[i] == '\n' {
					i++
					line++
					col = 1
				} else {
					i++
					col++
				}
			}
			if !closed {
				return nil, fmt.Errorf("line %d col %d: unterminated block comment",
					startLine, startCol)
			}
		case r == '(':
			emit(TokLParen, "", line, col)
			i++
			col++
		case r == ')':
			emit(TokRParen, "", line, col)
			i++
			col++
		case r == '=':
			// Standalone '='. Used by env entries (`NAME = ${HOST}`); a '='
			// inside a quoted string never reaches here (the '"' case above
			// consumes the whole string first), so existing values that
			// embed '=' — shell commands in `on ( … )` etc. — are untouched.
			emit(TokEquals, "", line, col)
			i++
			col++
		case r == '"':
			// Double-quoted string with simple backslash escapes.
			startLine, startCol := line, col
			i++
			col++
			var sb strings.Builder
			for i < n {
				c := src[i]
				if c == '"' {
					emit(TokString, sb.String(), startLine, startCol)
					i++
					col++
					goto nextToken
				}
				if c == '\n' {
					return nil, fmt.Errorf("line %d col %d: unterminated string",
						startLine, startCol)
				}
				if c == '\\' && i+1 < n {
					esc := src[i+1]
					switch esc {
					case '"', '\\':
						sb.WriteByte(esc)
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					default:
						return nil, fmt.Errorf("line %d col %d: unknown escape \\%c",
							line, col, esc)
					}
					i += 2
					col += 2
					continue
				}
				sb.WriteByte(c)
				i++
				col++
			}
			return nil, fmt.Errorf("line %d col %d: unterminated string", startLine, startCol)
		default:
			// Identifier: run until whitespace, a paren, a quote, or '='.
			startLine, startCol := line, col
			start := i
			for i < n {
				c := src[i]
				// A ${…} parameter reference scans as one unit — braces and
				// all — so an env value like ${VAR:-a default} stays a single
				// identifier even though it contains spaces. Nesting is
				// tracked so ${FOO:-${BAR}} would balance, though shells
				// rarely nest here.
				if c == '$' && i+1 < n && src[i+1] == '{' {
					i += 2
					col += 2
					depth := 1
					for i < n && depth > 0 {
						switch src[i] {
						case '{':
							depth++
						case '}':
							depth--
						case '\n':
							return nil, fmt.Errorf(
								"line %d col %d: unterminated ${…}", startLine, startCol)
						}
						i++
						col++
					}
					if depth != 0 {
						return nil, fmt.Errorf(
							"line %d col %d: unterminated ${…}", startLine, startCol)
					}
					continue
				}
				if c == ' ' || c == '\t' || c == '\n' || c == '\r' ||
					c == '(' || c == ')' || c == '#' || c == '"' || c == '=' {
					break
				}
				// '/' is an ordinary identifier byte. "//" and "/*" open a
				// comment only at a token boundary (handled above), never
				// mid-identifier — so a bare URL like https://host/path stays
				// a single token. An inline comment therefore needs a space
				// before it: `provider vz // note`.
				i++
				col++
			}
			emit(TokIdent, src[start:i], startLine, startCol)
		}
	nextToken:
	}
	emit(TokEOF, "", line, col)

	if err := validateTokens(toks); err != nil {
		return nil, err
	}
	return toks, nil
}

func validateTokens(toks []Token) error {
	// Balance parens here — cheap, catches early and gives good positions.
	depth := 0
	for _, t := range toks {
		switch t.Kind {
		case TokLParen:
			depth++
		case TokRParen:
			if depth == 0 {
				return fmt.Errorf("line %d col %d: unmatched ')'", t.Line, t.Col)
			}
			depth--
		}
	}
	if depth != 0 {
		return errors.New("unmatched '(' in template")
	}
	return nil
}

// isKeyword returns whether s is a template keyword. Keywords are normal
// identifiers lexically — the parser is the one that recognises them.
//
// The list spans top-level directives (name, forwards, includes, env,
// vm, network, skills, on, agent) and the words used inside vm/network/agent
// blocks (provider, cpu, memory, memory_max, nested, allow, deny, ip,
// instructions). Event names
// that appear only after `on` (create, up, down, enter) are deliberately NOT
// keywords so they remain usable as values elsewhere.
func isKeyword(s string) bool {
	switch s {
	case "provider", "includes", "allow", "ip",
		"forwards", "name", "nested",
		"cpu", "memory", "memory_max",
		"vm", "network", "deny", "skills", "on",
		"agent", "instructions":
		return true
	}
	return false
}

// describeFirst is a helper for error messages listing expected tokens.
func describeFirst(vals []string) string {
	quoted := make([]string, len(vals))
	for i, v := range vals {
		quoted[i] = fmt.Sprintf("%q", v)
	}
	return strings.Join(quoted, " or ")
}
