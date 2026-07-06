package template

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseFileSandbox(t *testing.T) {
	src := `# workspace root
sandbox acme (
    includes (
        ./api
        ./web
    )
    vm (
        provider vz
        cpu 4
    )
    network (
        use default corp-egress
        allow *.clawk.work
        allow ip 10.0.0.5
    )
)
`
	f, err := ParseFileString(src)
	require.NoError(t, err)
	require.NotNil(t, f.Sandbox)
	require.Equal(t, "acme", f.Sandbox.SandboxName)
	require.Empty(t, f.Sandbox.Name) // header name never lands in Name
	require.Equal(t, "vz", f.Sandbox.Provider)
	require.Equal(t, uint(4), f.Sandbox.CPU)
	require.Equal(t, []string{"./api", "./web"}, f.Sandbox.Includes)
	require.Equal(t, []string{"default", "corp-egress"}, f.Sandbox.Use)
	require.Equal(t, []string{"*.clawk.work"}, f.Sandbox.Domains)
	require.Equal(t, []string{"10.0.0.5"}, f.Sandbox.IPs)
	require.Empty(t, f.Policies)
	require.Empty(t, f.Namespaces)
}

func TestParseFileSandboxBare(t *testing.T) {
	f, err := ParseFileString("sandbox (\n)\n")
	require.NoError(t, err)
	require.NotNil(t, f.Sandbox)
	require.Empty(t, f.Sandbox.SandboxName)
}

func TestParseFilePolicy(t *testing.T) {
	src := `policy corp-egress (
    allow github.com
    allow ip 10.20.0.0/16
    deny tracker.example.com
    deny ip 192.168.10.9
    source "https://big.oisd.nl/domainswild"
    refresh 24h
)
`
	f, err := ParseFileString(src)
	require.NoError(t, err)
	require.Nil(t, f.Sandbox)
	require.Len(t, f.Policies, 1)
	def := f.Policies[0]
	require.Equal(t, "corp-egress", def.Name)
	require.Equal(t, []string{"github.com"}, def.AllowDomains)
	require.Equal(t, []string{"10.20.0.0/16"}, def.AllowIPs)
	require.Equal(t, []string{"tracker.example.com"}, def.DenyDomains)
	require.Equal(t, []string{"192.168.10.9"}, def.DenyIPs)
	require.Equal(t, []string{"https://big.oisd.nl/domainswild"}, def.Sources)
	require.Equal(t, 24*time.Hour, def.Refresh)
	require.Equal(t, 1, def.Line)
	require.Equal(t, 1, def.Col)
}

func TestParseFileNamespace(t *testing.T) {
	src := `namespace staging (
    network (
        use staging-egress
        allow *.staging.internal
    )
    env ( STAGING_TOKEN )
    files (
        ~/.kube/config_staging_only
    )
)
`
	f, err := ParseFileString(src)
	require.NoError(t, err)
	require.Len(t, f.Namespaces, 1)
	ns := f.Namespaces[0]
	require.Equal(t, "staging", ns.Name)
	require.Equal(t, []string{"staging-egress"}, ns.Template.Use)
	require.Equal(t, []string{"*.staging.internal"}, ns.Template.Domains)
	require.Equal(t, []string{"STAGING_TOKEN"}, ns.Template.Env)
	require.Len(t, ns.Template.Files, 1)
}

// TestParseFileMixed checks that a sandbox can share a file with the policies
// and namespaces it references, in any order.
func TestParseFileMixed(t *testing.T) {
	src := `policy internal-tools (
    allow ip 10.20.0.0/16
)

sandbox (
    network ( use internal-tools )
)

namespace prod (
    network ( allow prod.internal )
)
`
	f, err := ParseFileString(src)
	require.NoError(t, err)
	require.NotNil(t, f.Sandbox)
	require.Equal(t, []string{"internal-tools"}, f.Sandbox.Use)
	require.Len(t, f.Policies, 1)
	require.Len(t, f.Namespaces, 1)
}

func TestParseFileUseDuplicate(t *testing.T) {
	// Same line.
	_, err := ParseFileString("sandbox (\n    network (\n        use default default\n    )\n)\n")
	require.Error(t, err)
	require.Contains(t, err.Error(), `duplicate policy "default"`)

	// Across two use lines in the same file.
	_, err = ParseFileString("sandbox (\n    network (\n        use default\n        use corp default\n    )\n)\n")
	require.Error(t, err)
	require.Contains(t, err.Error(), `duplicate policy "default"`)
}

// TestParseStringWrapsSandbox checks the ParseString convenience wrapper: it
// returns the file's sandbox template, with the same `use` handling as the
// full-file entry point, and errors on a file without a sandbox block.
func TestParseStringWrapsSandbox(t *testing.T) {
	tmpl, err := ParseString("sandbox (\nnetwork (\n    use default oisd\n    allow github.com\n)\n)\n")
	require.NoError(t, err)
	require.Equal(t, []string{"default", "oisd"}, tmpl.Use)
	require.Equal(t, []string{"github.com"}, tmpl.Domains)

	_, err = ParseString("sandbox (\nnetwork (\n    use oisd oisd\n)\n)\n")
	require.Error(t, err)
	require.Contains(t, err.Error(), `duplicate policy "oisd"`)

	// A file whose only blocks are policies has no sandbox to return —
	// callers that can consume the other kinds use ParseFileString.
	_, err = ParseString("policy p (\n    allow github.com\n)\n")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no sandbox block")
}

func TestParseFileErrors(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		wantErr string
	}{
		{
			"second sandbox rejected at its line",
			"sandbox a (\n)\n\nsandbox b (\n)\n",
			"line 4 col 1: second 'sandbox' block; exactly one sandbox block is allowed per file",
		},
		{
			"name inside sandbox body",
			"sandbox (\n    name acme\n)\n",
			"line 2 col 5: name moved to the block header: sandbox <name> ( ... )",
		},
		{
			"flat file gets wrap hint naming the directive",
			"vm (\n    provider vz\n)\n",
			"top-level \"vm\" is retired: wrap the body in `sandbox ( ... )` and move `name <x>` into the header",
		},
		{
			"flat name gets wrap hint",
			"name acme\n",
			"top-level \"name\" is retired",
		},
		{
			"clawk.work content gets rename hint",
			"includes (\n    ~/code/api\n)\n",
			"clawk.work is retired — rename to clawk.mod and wrap the body in `sandbox <name> ( includes ( ... ) ... )`",
		},
		{
			"unknown block kind",
			"plover (\n)\n",
			`unknown block kind "plover" (want "sandbox" or "policy" or "namespace")`,
		},
		{
			"policy requires a name",
			"policy (\n)\n",
			"policy requires a name",
		},
		{
			"namespace requires a name",
			"namespace (\n)\n",
			"namespace requires a name",
		},
		{
			"namespace rejects vm",
			"namespace staging (\n    vm (\n        cpu 2\n    )\n)\n",
			`"vm" is a sandbox-level directive, not allowed in a namespace`,
		},
		{
			"namespace rejects includes",
			"namespace staging (\n    includes ( ./api )\n)\n",
			`"includes" is a sandbox-level directive`,
		},
		{
			"namespace rejects on",
			"namespace staging (\n    on up \"make\"\n)\n",
			`"on" is a sandbox-level directive`,
		},
		{
			"policy source needs a quoted URL",
			"policy p (\n    source https://x.example\n)\n",
			"expected a quoted URL after 'source'",
		},
		{
			"policy refresh needs a duration",
			"policy p (\n    refresh soon\n)\n",
			`invalid refresh "soon"`,
		},
		{
			"policy duplicate refresh",
			"policy p (\n    refresh 24h\n    refresh 12h\n)\n",
			"duplicate 'refresh' directive",
		},
		{
			"use needs at least one name",
			"sandbox (\n    network (\n        use\n    )\n)\n",
			"expected policy name after 'use'",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseFileString(c.src)
			require.Error(t, err)
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error %q does not contain %q", err, c.wantErr)
			}
		})
	}
}

// TestParseFileEmpty: a file with no blocks parses to an empty File — the
// caller decides whether a missing sandbox is an error for its context.
func TestParseFileEmpty(t *testing.T) {
	f, err := ParseFileString("# nothing but comments\n")
	require.NoError(t, err)
	require.Nil(t, f.Sandbox)
	require.Empty(t, f.Policies)
	require.Empty(t, f.Namespaces)
}

func TestParseFileClawkVersionDirective(t *testing.T) {
	f, err := ParseFileString("clawk 1\n\nsandbox demo (\n)\n")
	require.NoError(t, err)
	require.Equal(t, 1, f.FormatVersion)
	require.NotNil(t, f.Sandbox)

	// Absent directive: version 1 implied, recorded as zero.
	f, err = ParseFileString("sandbox demo (\n)\n")
	require.NoError(t, err)
	require.Zero(t, f.FormatVersion)
}

func TestParseFileClawkVersionErrors(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{"newer than supported", "clawk 2\n", "upgrade clawk"},
		{"duplicate", "clawk 1\nclawk 1\n", "duplicate 'clawk' version directive"},
		{"not a number", "clawk one\n", "positive integer"},
		{"zero", "clawk 0\n", "positive integer"},
		{"missing value", "clawk\nsandbox demo (\n)\n", "expected format version"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseFileString(tc.src)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestParseFileEnvNameValidation(t *testing.T) {
	// Lowercase is legitimate (http_proxy); shape is POSIX-variable.
	f, err := ParseFileString("sandbox demo (\n    env ( DATABASE_URL http_proxy _private )\n)\n")
	require.NoError(t, err)
	require.Equal(t, []string{"DATABASE_URL", "http_proxy", "_private"}, f.Sandbox.Env)

	for _, bad := range []string{"1BAD", "foo-bar", "foo.bar"} {
		_, err := ParseFileString("sandbox demo (\n    env ( " + bad + " )\n)\n")
		require.Error(t, err, bad)
		require.Contains(t, err.Error(), "invalid env name")
	}
}
