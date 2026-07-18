package template

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLexBasic(t *testing.T) {
	src := `provider vz

repos (
    ~/code/a
    /abs/path/b
)

allow (
    github.com
    *.github.com
    ip 10.0.0.5
    ip 192.168.10.0/24
)
`
	toks, err := Lex(src)
	require.NoError(t, err)

	// Collect IDENT values in order so the test doesn't have to enumerate
	// every newline / paren token — those are covered by TestLexNewlines.
	var idents []string
	for _, tok := range toks {
		if tok.Kind == TokIdent {
			idents = append(idents, tok.Val)
		}
	}
	want := []string{
		"provider", "vz",
		"repos",
		"~/code/a", "/abs/path/b",
		"allow",
		"github.com", "*.github.com",
		"ip", "10.0.0.5",
		"ip", "192.168.10.0/24",
	}
	if len(idents) != len(want) {
		t.Fatalf("got %d idents, want %d:\n  got %v\n  want %v", len(idents), len(want), idents, want)
	}
	for i, w := range want {
		if idents[i] != w {
			t.Errorf("ident[%d] = %q, want %q", i, idents[i], w)
		}
	}
}

// TestLexEnvValues covers the two lexer additions for the env compose
// syntax: a standalone '=' token, and ${…} scanning as one identifier
// even when it contains spaces (so a default like ${VAR:-a b} survives).
func TestLexEnvValues(t *testing.T) {
	toks, err := Lex("GH = ${ACME_GH_TOKEN:-a default}\n")
	require.NoError(t, err)

	var kinds []TokenKind
	var idents []string
	for _, tok := range toks {
		if tok.Kind == TokEOF || tok.Kind == TokNewline {
			continue
		}
		kinds = append(kinds, tok.Kind)
		if tok.Kind == TokIdent {
			idents = append(idents, tok.Val)
		}
	}
	require.Equal(t, []TokenKind{TokIdent, TokEquals, TokIdent}, kinds)
	require.Equal(t, []string{"GH", "${ACME_GH_TOKEN:-a default}"}, idents)

	// A '=' with no surrounding spaces still splits into three tokens.
	toks, err = Lex("GH=${X}\n")
	require.NoError(t, err)
	require.Equal(t, TokIdent, toks[0].Kind)
	require.Equal(t, "GH", toks[0].Val)
	require.Equal(t, TokEquals, toks[1].Kind)
	require.Equal(t, "${X}", toks[2].Val)

	// An unterminated ${ is a lex error, not a silently truncated token.
	_, err = Lex("GH = ${X\n")
	require.Error(t, err)
}

func TestLexComments(t *testing.T) {
	// Every comment form must strip to the same two identifiers. A lone '/'
	// in a path is NOT a comment — only "//" and "/*" open one.
	tests := []struct {
		name string
		src  string
	}{
		{"hash", "# top-level\nprovider vz  # inline\n"},
		{"slash-line", "// top-level\nprovider vz  // inline\n"},
		{"block-inline", "provider /* mid */ vz\n"},
		{"block-multiline", "provider /* spanning\nseveral\nlines */ vz\n"},
		{"block-then-line", "/* block */ provider vz // trailing\n"},
		{"mixed", "# hash\n// slash\nprovider /* block */ vz\n"},
		{"path-slash-not-comment", "provider vz\nrepos ( /abs/path  ~/code/a )\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			toks, err := Lex(tt.src)
			require.NoError(t, err)
			var idents []string
			for _, tok := range toks {
				if tok.Kind == TokIdent {
					idents = append(idents, tok.Val)
				}
			}
			// The path case carries extra idents; assert provider/vz survive
			// and that no comment delimiter leaked into a token.
			if len(idents) < 2 || idents[0] != "provider" || idents[1] != "vz" {
				t.Fatalf("comment stripping broken: %v", idents)
			}
			for _, id := range idents {
				if strings.Contains(id, "//") || strings.Contains(id, "/*") ||
					strings.Contains(id, "*/") || strings.Contains(id, "#") {
					t.Errorf("comment delimiter leaked into ident %q (all: %v)", id, idents)
				}
			}
		})
	}
}

func TestLexURLNotComment(t *testing.T) {
	// A bare URL value (kernel/image directives accept these) must lex as a
	// single identifier: the "//" after the scheme is mid-token, not a
	// comment. A trailing comment after whitespace is still stripped.
	src := "kernel https://example.com/vmlinux // override\n"
	toks, err := Lex(src)
	require.NoError(t, err)
	var idents []string
	for _, tok := range toks {
		if tok.Kind == TokIdent {
			idents = append(idents, tok.Val)
		}
	}
	want := []string{"kernel", "https://example.com/vmlinux"}
	if len(idents) != len(want) || idents[0] != want[0] || idents[1] != want[1] {
		t.Errorf("URL lexing broken: got %v, want %v", idents, want)
	}
}

func TestLexBlockCommentPreservesPosition(t *testing.T) {
	// A token after a multi-line block comment must report the correct line
	// and column — the rewriter relies on these offsets.
	src := "provider /* a\nb\nc */ vz\n"
	toks, err := Lex(src)
	require.NoError(t, err)
	var vz *Token
	for i := range toks {
		if toks[i].Val == "vz" {
			vz = &toks[i]
			break
		}
	}
	// The comment closes with "*/ " on line 3 ("c */ vz"); vz starts at col 6.
	if vz == nil || vz.Line != 3 || vz.Col != 6 {
		t.Errorf("vz position after block comment = %+v, want line 3 col 6", vz)
	}
}

func TestLexUnterminatedBlockComment(t *testing.T) {
	cases := []string{
		"provider /* never closed\n",
		"/* unclosed at eof",
		"/* trailing star *",
	}
	for _, src := range cases {
		if _, err := Lex(src); err == nil {
			t.Errorf("expected error for %q", src)
		}
	}
}

func TestLexUnbalancedParen(t *testing.T) {
	cases := []string{
		"repos (",
		"repos )",
		"repos ( a\nrepos ( b\n)",
	}
	for _, src := range cases {
		if _, err := Lex(src); err == nil {
			t.Errorf("expected error for %q", src)
		}
	}
}

func TestLexPositions(t *testing.T) {
	src := "provider\n  vz\n"
	toks, err := Lex(src)
	require.NoError(t, err)
	if toks[0].Line != 1 || toks[0].Col != 1 {
		t.Errorf("provider token at %d:%d, want 1:1", toks[0].Line, toks[0].Col)
	}
	// "vz" on line 2 after 2 spaces
	var vf *Token
	for i := range toks {
		if toks[i].Val == "vz" {
			vf = &toks[i]
			break
		}
	}
	if vf == nil || vf.Line != 2 || vf.Col != 3 {
		t.Errorf("vz position wrong: %+v", vf)
	}
}

func TestLexNewlineHandling(t *testing.T) {
	// Multiple consecutive newlines should produce multiple NEWLINE tokens;
	// the parser handles collapsing them.
	toks, err := Lex("a\n\n\nb\n")
	require.NoError(t, err)
	nlCount := 0
	for _, tk := range toks {
		if tk.Kind == TokNewline {
			nlCount++
		}
	}
	if nlCount != 4 {
		t.Errorf("got %d newlines, want 4", nlCount)
	}
}

func TestLexStrings(t *testing.T) {
	toks, err := Lex(`"hello world" "with \"quote\"" "newline\nhere"`)
	require.NoError(t, err)
	var strs []string
	for _, tok := range toks {
		if tok.Kind == TokString {
			strs = append(strs, tok.Val)
		}
	}
	want := []string{
		"hello world",
		`with "quote"`,
		"newline\nhere",
	}
	if len(strs) != len(want) {
		t.Fatalf("got %d strings, want %d: %v", len(strs), len(want), strs)
	}
	for i, w := range want {
		if strs[i] != w {
			t.Errorf("string[%d] = %q, want %q", i, strs[i], w)
		}
	}
}

func TestLexUnterminatedString(t *testing.T) {
	cases := []string{
		`"missing close quote`,
		"\"has\nnewline\"",
	}
	for _, src := range cases {
		if _, err := Lex(src); err == nil {
			t.Errorf("expected error for %q", src)
		}
	}
}

func TestLexIdentCharacterSet(t *testing.T) {
	// Tokens can contain ., *, /, -, _, digits — anything but whitespace
	// and ( ) # is part of an identifier.
	src := `*.foo-bar_1/x.y`
	toks, err := Lex(src)
	require.NoError(t, err)
	// Expect one IDENT + EOF
	if len(toks) != 2 || toks[0].Kind != TokIdent {
		t.Fatalf("unexpected tokens: %v", toks)
	}
	if !strings.Contains(toks[0].Val, "*.foo-bar_1/x.y") {
		t.Errorf("lost chars: %q", toks[0].Val)
	}
}
