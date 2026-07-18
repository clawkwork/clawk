// Package envspec parses and evaluates the entries of a clawk.mod
// `env ( … )` block — the small expression language that maps a
// guest-side environment variable to a host variable, a literal, or a
// host variable with a fallback.
//
// The grammar deliberately mirrors POSIX-shell / docker-compose
// parameter expansion, so there is nothing new to learn:
//
//	NAME                       passthrough — export NAME = host $NAME
//	                           (empty if unset; a warning is logged)
//	NAME = ${HOST}             alias      — export NAME = host $HOST
//	NAME = ${HOST:-default}    default if HOST is unset OR empty
//	NAME = ${HOST-default}     default if HOST is unset (empty kept)
//	NAME = ${HOST:?message}    hard error if HOST is unset OR empty
//	NAME = ${HOST?message}     hard error if HOST is unset
//	NAME = literal             literal constant, no host lookup
//	NAME = "literal"           quoted literal (spaces / special chars)
//
// A bare or quoted right-hand side is always a literal; host variables
// are referenced only through ${…}. This removes the "is that a value
// or a variable name?" ambiguity that a bare-name alias syntax hits the
// moment defaults enter the picture.
//
// Entries round-trip through their canonical String() form, which is
// what the template parser stores and what Parse reads back — so
// clawk.mod's env list stays a plain []string end to end (dedup, union,
// and JSON persistence never need to know about the structure).
package envspec

import (
	"fmt"
	"strings"
)

// Op is how a Spec combines its host variable and its literal argument.
type Op uint8

const (
	// OpPassthrough exports the host variable's value, or empty (with a
	// warning) when it is unset. Covers both `NAME` and `NAME = ${HOST}`.
	OpPassthrough Op = iota
	// OpDefaultUnset uses Arg when the host variable is unset; a set but
	// empty value is kept. `${HOST-arg}`.
	OpDefaultUnset
	// OpDefaultEmpty uses Arg when the host variable is unset OR empty.
	// `${HOST:-arg}`.
	OpDefaultEmpty
	// OpRequiredUnset fails resolution when the host variable is unset.
	// `${HOST?arg}` (arg is the error message).
	OpRequiredUnset
	// OpRequiredEmpty fails resolution when the host variable is unset OR
	// empty. `${HOST:?arg}`.
	OpRequiredEmpty
	// OpLiteral exports Arg verbatim with no host lookup.
	OpLiteral
)

// Spec is one parsed env entry.
type Spec struct {
	Name string // guest variable to export (always a valid env name)
	Host string // host variable to read; "" exactly when Op == OpLiteral
	Arg  string // default value, error message, or literal — per Op
	Op   Op
}

// Parse reads one canonical entry (see the package doc) into a Spec. It
// is the single source of truth for the grammar: the template parser
// calls it to validate at parse time, and the sandbox layer calls it
// again to evaluate at manifest-build time.
func Parse(entry string) (Spec, error) {
	name, rhs, hasValue := strings.Cut(entry, "=")
	name = strings.TrimSpace(name)
	if err := ValidateName(name); err != nil {
		return Spec{}, err
	}
	if !hasValue {
		// Bare name: passthrough of the identically-named host variable.
		return Spec{Name: name, Host: name, Op: OpPassthrough}, nil
	}
	rhs = strings.TrimSpace(rhs)

	// Quoted right-hand side is always a literal.
	if strings.HasPrefix(rhs, `"`) {
		val, err := Unquote(rhs)
		if err != nil {
			return Spec{}, err
		}
		return Spec{Name: name, Op: OpLiteral, Arg: val}, nil
	}

	// ${…} is a host-variable reference; anything else is a bare literal.
	if strings.HasPrefix(rhs, "${") && strings.HasSuffix(rhs, "}") {
		return parseRef(name, rhs[2:len(rhs)-1])
	}
	return Spec{Name: name, Op: OpLiteral, Arg: rhs}, nil
}

// parseRef parses the inside of a ${…} reference: a host variable name
// followed by an optional operator and argument.
func parseRef(name, inner string) (Spec, error) {
	host, rest := scanName(inner)
	if host == "" {
		return Spec{}, fmt.Errorf("empty variable name in ${%s}", inner)
	}
	if err := ValidateName(host); err != nil {
		return Spec{}, err
	}
	s := Spec{Name: name, Host: host}
	switch {
	case rest == "":
		s.Op = OpPassthrough
	case strings.HasPrefix(rest, ":-"):
		s.Op, s.Arg = OpDefaultEmpty, rest[2:]
	case strings.HasPrefix(rest, ":?"):
		s.Op, s.Arg = OpRequiredEmpty, rest[2:]
	case strings.HasPrefix(rest, "-"):
		s.Op, s.Arg = OpDefaultUnset, rest[1:]
	case strings.HasPrefix(rest, "?"):
		s.Op, s.Arg = OpRequiredUnset, rest[1:]
	default:
		return Spec{}, fmt.Errorf(
			"unsupported operator in ${%s}: %q (want :- , - , :? or ?)", inner, rest)
	}
	return s, nil
}

// Resolve evaluates the spec against a host-env lookup (os.LookupEnv in
// production; a fake in tests). warnUnset reports a bare passthrough
// whose host variable is unset — the caller logs it and exports empty,
// preserving the historical behavior. A required-but-missing variable
// returns an error instead.
func (s Spec) Resolve(lookup func(string) (string, bool)) (value string, warnUnset bool, err error) {
	if s.Op == OpLiteral {
		return s.Arg, false, nil
	}
	val, ok := lookup(s.Host)
	switch s.Op {
	case OpPassthrough:
		return val, !ok, nil
	case OpDefaultUnset:
		if !ok {
			return s.Arg, false, nil
		}
		return val, false, nil
	case OpDefaultEmpty:
		if !ok || val == "" {
			return s.Arg, false, nil
		}
		return val, false, nil
	case OpRequiredUnset:
		if !ok {
			return "", false, s.missingErr()
		}
		return val, false, nil
	case OpRequiredEmpty:
		if !ok || val == "" {
			return "", false, s.missingErr()
		}
		return val, false, nil
	default:
		return "", false, fmt.Errorf("envspec: unknown op %d", s.Op)
	}
}

func (s Spec) missingErr() error {
	if msg := strings.TrimSpace(s.Arg); msg != "" {
		return fmt.Errorf("required env %s (reads host variable %s) is unset: %s",
			s.Name, s.Host, msg)
	}
	return fmt.Errorf("required env %s (reads host variable %s) is unset on host",
		s.Name, s.Host)
}

// String returns the canonical, re-parseable text for the spec.
// Parse(s.String()) == s for every valid Spec.
func (s Spec) String() string {
	switch s.Op {
	case OpPassthrough:
		if s.Host == s.Name {
			return s.Name // bare-name shorthand
		}
		return s.Name + "=${" + s.Host + "}"
	case OpDefaultUnset:
		return s.Name + "=${" + s.Host + "-" + s.Arg + "}"
	case OpDefaultEmpty:
		return s.Name + "=${" + s.Host + ":-" + s.Arg + "}"
	case OpRequiredUnset:
		return s.Name + "=${" + s.Host + "?" + s.Arg + "}"
	case OpRequiredEmpty:
		return s.Name + "=${" + s.Host + ":?" + s.Arg + "}"
	case OpLiteral:
		return s.Name + "=" + quoteLiteral(s.Arg)
	default:
		return s.Name
	}
}

// ValidateName enforces the POSIX shell variable-name shape. Lowercase
// is allowed on purpose — names like http_proxy are legitimate.
func ValidateName(s string) error {
	if s == "" {
		return fmt.Errorf("invalid env name: empty")
	}
	for i, r := range s {
		switch {
		case r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
		case r >= '0' && r <= '9':
			if i == 0 {
				return fmt.Errorf("invalid env name %q: must not start with a digit", s)
			}
		default:
			return fmt.Errorf(
				"invalid env name %q: names only (letters, digits, '_')", s)
		}
	}
	return nil
}

// scanName splits off the leading POSIX-name-shaped run from s. Used to
// separate a host variable name from a trailing ${…} operator.
func scanName(s string) (name, rest string) {
	i := 0
	for i < len(s) {
		c := s[i]
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') {
			i++
			continue
		}
		break
	}
	return s[:i], s[i:]
}

// quoteLiteral returns v as a bare word when it is safe to write
// unquoted, otherwise as a double-quoted string. "Safe" means Parse
// would read the bare form back as this exact literal.
func quoteLiteral(v string) string {
	if isSafeBareLiteral(v) {
		return v
	}
	return Quote(v)
}

func isSafeBareLiteral(v string) bool {
	if v == "" || v[0] == '"' || strings.HasPrefix(v, "${") {
		return false
	}
	for _, r := range v {
		switch r {
		case ' ', '\t', '\n', '\r', '(', ')', '#', '"', '=':
			return false
		}
	}
	return true
}

// Quote renders v as a double-quoted string using the same escape set
// the clawk.mod lexer understands (\" \\ \n \t).
func Quote(v string) string {
	var b strings.Builder
	b.Grow(len(v) + 2)
	b.WriteByte('"')
	for _, r := range v {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// Unquote is the inverse of Quote. It requires a leading and trailing
// double quote.
func Unquote(s string) (string, error) {
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return "", fmt.Errorf("malformed quoted value %q", s)
	}
	inner := s[1 : len(s)-1]
	var b strings.Builder
	b.Grow(len(inner))
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		if c == '\\' && i+1 < len(inner) {
			switch inner[i+1] {
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			default:
				return "", fmt.Errorf("unknown escape \\%c in %q", inner[i+1], s)
			}
			i++
			continue
		}
		b.WriteByte(c)
	}
	return b.String(), nil
}
