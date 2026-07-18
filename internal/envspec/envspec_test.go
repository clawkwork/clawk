package envspec

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		entry string
		want  Spec
	}{
		{"GITHUB_TOKEN", Spec{Name: "GITHUB_TOKEN", Host: "GITHUB_TOKEN", Op: OpPassthrough}},
		{"http_proxy", Spec{Name: "http_proxy", Host: "http_proxy", Op: OpPassthrough}},
		{"GH=${ACME_GH_TOKEN}", Spec{Name: "GH", Host: "ACME_GH_TOKEN", Op: OpPassthrough}},
		{"LOG=${LOG:-info}", Spec{Name: "LOG", Host: "LOG", Arg: "info", Op: OpDefaultEmpty}},
		{"LOG=${LOG:-}", Spec{Name: "LOG", Host: "LOG", Arg: "", Op: OpDefaultEmpty}},
		{"PORT=${PORT-8080}", Spec{Name: "PORT", Host: "PORT", Arg: "8080", Op: OpDefaultUnset}},
		{"K=${K:?must be set}", Spec{Name: "K", Host: "K", Arg: "must be set", Op: OpRequiredEmpty}},
		{"K=${K?unset}", Spec{Name: "K", Host: "K", Arg: "unset", Op: OpRequiredUnset}},
		{"EDITOR=vim", Spec{Name: "EDITOR", Arg: "vim", Op: OpLiteral}},
		{"BANNER=\"hi there\"", Spec{Name: "BANNER", Arg: "hi there", Op: OpLiteral}},
		{"EMPTY=\"\"", Spec{Name: "EMPTY", Arg: "", Op: OpLiteral}},
		// A default that itself contains '=' — Cut must split on the first '='.
		{"KV=${KV:-a=b}", Spec{Name: "KV", Host: "KV", Arg: "a=b", Op: OpDefaultEmpty}},
		// A quoted literal that looks like an interpolation stays literal.
		{"LIT=\"${X}\"", Spec{Name: "LIT", Arg: "${X}", Op: OpLiteral}},
	}
	for _, tt := range tests {
		got, err := Parse(tt.entry)
		if err != nil {
			t.Errorf("Parse(%q) error: %v", tt.entry, err)
			continue
		}
		if got != tt.want {
			t.Errorf("Parse(%q) = %+v, want %+v", tt.entry, got, tt.want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, entry := range []string{
		"1BAD",           // leading digit
		"foo-bar",        // hyphen in bare name
		"foo.bar",        // dot
		"GH=${1BAD}",     // bad host name
		"GH=${}",         // empty host name
		"GH=${HOST:x}",   // unknown operator
		"GH=${HOST%bad}", // unknown operator
	} {
		if _, err := Parse(entry); err == nil {
			t.Errorf("Parse(%q) = nil error, want failure", entry)
		}
	}
}

func TestStringRoundTrips(t *testing.T) {
	// For every valid entry, Parse(spec.String()) must reproduce the spec.
	for _, entry := range []string{
		"GITHUB_TOKEN",
		"GH=${ACME_GH_TOKEN}",
		"LOG=${LOG:-info}",
		"LOG=${LOG:-}",
		"PORT=${PORT-8080}",
		"K=${K:?must be set}",
		"K=${K?unset}",
		"EDITOR=vim",
		"BANNER=\"hi there\"",
		"EMPTY=\"\"",
		"KV=${KV:-a=b}",
		"LIT=\"${X}\"",
	} {
		s1, err := Parse(entry)
		if err != nil {
			t.Fatalf("Parse(%q): %v", entry, err)
		}
		s2, err := Parse(s1.String())
		if err != nil {
			t.Fatalf("re-Parse(%q from %q): %v", s1.String(), entry, err)
		}
		if s1 != s2 {
			t.Errorf("round-trip drift for %q: %+v -> %q -> %+v", entry, s1, s1.String(), s2)
		}
	}
}

func TestResolve(t *testing.T) {
	env := map[string]string{
		"SET":   "value",
		"EMPTY": "",
	}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }

	tests := []struct {
		name     string
		entry    string
		wantVal  string
		wantWarn bool
		wantErr  bool
	}{
		{"passthrough set", "SET", "value", false, false},
		{"passthrough unset warns", "MISSING", "", true, false},
		{"passthrough empty no warn", "EMPTY", "", false, false},
		{"alias set", "G=${SET}", "value", false, false},
		{"alias unset warns", "G=${MISSING}", "", true, false},
		{":- uses default when unset", "L=${MISSING:-def}", "def", false, false},
		{":- uses default when empty", "L=${EMPTY:-def}", "def", false, false},
		{":- keeps set value", "L=${SET:-def}", "value", false, false},
		{"- uses default when unset", "L=${MISSING-def}", "def", false, false},
		{"- keeps empty value", "L=${EMPTY-def}", "", false, false},
		{":? errors when unset", "R=${MISSING:?nope}", "", false, true},
		{":? errors when empty", "R=${EMPTY:?nope}", "", false, true},
		{":? passes when set", "R=${SET:?nope}", "value", false, false},
		{"? errors when unset", "R=${MISSING?nope}", "", false, true},
		{"? passes when empty", "R=${EMPTY?nope}", "", false, false},
		{"literal", "E=vim", "vim", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, err := Parse(tt.entry)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.entry, err)
			}
			val, warn, err := spec.Resolve(lookup)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Resolve err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if val != tt.wantVal {
				t.Errorf("value = %q, want %q", val, tt.wantVal)
			}
			if warn != tt.wantWarn {
				t.Errorf("warnUnset = %v, want %v", warn, tt.wantWarn)
			}
		})
	}
}
