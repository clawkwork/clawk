package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/clawkwork/clawk/internal/template"
	"github.com/stretchr/testify/require"
)

func TestParseBlocklistLine(t *testing.T) {
	tests := []struct {
		line string
		want []string
	}{
		{"0.0.0.0 ads.example.com", []string{"ads.example.com"}},
		{"127.0.0.1 localhost", nil},
		{"0.0.0.0 a.com b.com", []string{"a.com", "b.com"}},
		{"tracker.example.com", []string{"tracker.example.com"}},
		{"||evil.example.com^", []string{"evil.example.com"}},
		{"||ads.example.com^$third-party", []string{"ads.example.com"}},
		{"# a comment", nil},
		{"! adblock comment", nil},
		{"", nil},
		{"not a domain", nil},
	}
	for _, tt := range tests {
		got := parseBlocklistLine(tt.line)
		require.True(t, slices.Equal(got, tt.want), "parseBlocklistLine(%q) = %v, want %v", tt.line, got, tt.want)
	}
}

func TestFetchBlocklist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("# hosts list\n0.0.0.0 ads.example.com\n0.0.0.0 localhost\ntracker.example.com\n||evil.example.com^\n"))
	}))
	defer srv.Close()

	got, err := fetchBlocklist(srv.URL)
	require.NoError(t, err)
	want := []string{"ads.example.com", "tracker.example.com", "evil.example.com"}
	require.True(t, slices.Equal(got, want), "fetchBlocklist = %v, want %v", got, want)
}

// parseNamespaceDef is a test helper: parse a one-namespace manifest and
// return its NamespaceDef.
func parseNamespaceDef(t *testing.T, src string) template.NamespaceDef {
	t.Helper()
	f, err := template.ParseFileString(src)
	require.NoError(t, err)
	require.Len(t, f.Namespaces, 1)
	return f.Namespaces[0]
}

func TestNamespaceFromDef_NetworkAndBaseline(t *testing.T) {
	_, _ = setupTest(t)
	def := parseNamespaceDef(t, `namespace work (
    network (
        deny *
        allow github.com
        allow *.githubusercontent.com
        deny tracker.example.com
        deny ip 192.168.10.9
    )
    env ( CORP_TOKEN )
)
`)
	ns, err := namespaceFromDef(def, false)
	require.NoError(t, err)
	require.Equal(t, "work", ns.Name, "name")
	require.True(t, slices.Contains(ns.AllowedDomains, "github.com") && slices.Contains(ns.AllowedDomains, "*.githubusercontent.com"),
		"allow = %v", ns.AllowedDomains)
	// `deny *` is the baseline and must be dropped; the real deny stays.
	require.False(t, slices.Contains(ns.DeniedDomains, "*"), "deny * baseline should be dropped, got %v", ns.DeniedDomains)
	require.True(t, slices.Contains(ns.DeniedDomains, "tracker.example.com"), "deny = %v", ns.DeniedDomains)
	require.Equal(t, []string{"192.168.10.9"}, ns.DeniedIPs, "deny ip")
	require.True(t, slices.Contains(ns.Env, "CORP_TOKEN"), "env = %v", ns.Env)
	require.Nil(t, ns.Use, "no use line means unspecified (inherit default)")
}

// TestNamespaceFromDef_UseAndSources: a written `use` chain lands on the
// record, and `deny source` URLs register as source policies spliced into the
// chain (after "default" when it leads) instead of being fetched and baked.
func TestNamespaceFromDef_UseAndSources(t *testing.T) {
	s, _ := setupTest(t)
	def := parseNamespaceDef(t, `namespace work (
    network (
        use default corp
        deny source "https://example.com/list.txt"
    )
)
`)
	ns, err := namespaceFromDef(def, false)
	require.NoError(t, err)
	srcName := sourcePolicyName("https://example.com/list.txt")
	require.Equal(t, []string{"default", srcName, "corp"}, ns.Use)

	// The source policy must exist in the store, carrying the URL.
	p, err := s.LoadPolicy(srcName)
	require.NoError(t, err)
	require.Equal(t, "https://example.com/list.txt", p.Source)
}

func TestApplyCommand_UpsertsNamespace(t *testing.T) {
	s, _ := setupTest(t)
	manifest := filepath.Join(t.TempDir(), "work.clawk")
	require.NoError(t, os.WriteFile(manifest,
		[]byte("namespace work (\n  network ( deny * allow github.com deny tracker.example.com )\n)\n"), 0o644))
	_, err := executeCommand("apply", "-f", manifest)
	require.NoError(t, err, "apply")
	ns, err := s.LoadNamespace("work")
	require.NoError(t, err)
	require.True(t, slices.Contains(ns.AllowedDomains, "github.com"), "allow not applied: %v", ns.AllowedDomains)
	require.True(t, slices.Contains(ns.DeniedDomains, "tracker.example.com") && !slices.Contains(ns.DeniedDomains, "*"),
		"deny not applied correctly: %v", ns.DeniedDomains)
}

// TestApplyCommand_PolicyAndMultiDoc: one manifest can carry several
// resources; policy blocks land in the policy store with the PolicyDef →
// Policy mapping (refresh renders as a duration string).
func TestApplyCommand_PolicyAndMultiDoc(t *testing.T) {
	s, _ := setupTest(t)
	manifest := filepath.Join(t.TempDir(), "corp.clawk")
	require.NoError(t, os.WriteFile(manifest, []byte(`policy corp-egress (
    allow github.com
    allow ip 10.20.0.0/16
    deny  telemetry.corp.com
    source "https://example.com/oisd.txt"
    refresh 24h
)

namespace work (
    network ( use default corp-egress )
)
`), 0o644))
	out, err := executeCommand("apply", "-f", manifest)
	require.NoError(t, err, "apply: %s", out)

	p, err := s.LoadPolicy("corp-egress")
	require.NoError(t, err)
	require.Equal(t, []string{"github.com"}, p.AllowDomains)
	require.Equal(t, []string{"10.20.0.0/16"}, p.AllowIPs)
	require.Equal(t, []string{"telemetry.corp.com"}, p.DenyDomains)
	require.Equal(t, "https://example.com/oisd.txt", p.Source)
	require.Equal(t, "24h0m0s", p.Refresh)

	ns, err := s.LoadNamespace("work")
	require.NoError(t, err)
	require.Equal(t, []string{"default", "corp-egress"}, ns.Use)
}

// TestApplyCommand_RejectsSandbox: sandbox templates live in their repo's
// clawk.mod; apply names the limitation instead of half-registering one.
func TestApplyCommand_RejectsSandbox(t *testing.T) {
	_, _ = setupTest(t)
	manifest := filepath.Join(t.TempDir(), "sb.clawk")
	require.NoError(t, os.WriteFile(manifest, []byte("sandbox acme (\n)\n"), 0o644))
	_, err := executeCommand("apply", "-f", manifest)
	require.Error(t, err)
	require.Contains(t, err.Error(), "registering sandbox templates via apply is not supported yet")
}

// TestApplyCommand_PolicyOneSource: a policy block with two sources is
// rejected — a policy's cache is one fetch.
func TestApplyCommand_PolicyOneSource(t *testing.T) {
	_, _ = setupTest(t)
	manifest := filepath.Join(t.TempDir(), "p.clawk")
	require.NoError(t, os.WriteFile(manifest,
		[]byte("policy p (\n  source \"https://a.example/x\"\n  source \"https://b.example/y\"\n)\n"), 0o644))
	_, err := executeCommand("apply", "-f", manifest)
	require.Error(t, err)
	require.Contains(t, err.Error(), "one source per policy")
}

// TestApplyCommand_Directory: every regular non-hidden file in a directory
// applies independently — a broken file is reported by name without stopping
// the others, and the command still exits nonzero.
func TestApplyCommand_Directory(t *testing.T) {
	s, _ := setupTest(t)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a-good.clawk"),
		[]byte("policy good-a (\n  allow github.com\n)\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b-bad.clawk"),
		[]byte("vm (\n  provider vz\n)\n"), 0o644)) // flat grammar: parse error
	require.NoError(t, os.WriteFile(filepath.Join(dir, "c-good.clawk"),
		[]byte("namespace work (\n  network ( allow github.com )\n)\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".hidden.clawk"),
		[]byte("this would not even parse"), 0o644)) // hidden: skipped

	out, err := executeCommand("apply", "-f", dir)
	require.Error(t, err, "one broken manifest must make the exit status nonzero")
	require.Contains(t, err.Error(), "b-bad.clawk")

	// Both good files applied despite the failure in between.
	_, perr := s.LoadPolicy("good-a")
	require.NoError(t, perr, "good-a should have applied")
	ns, nerr := s.LoadNamespace("work")
	require.NoError(t, nerr)
	require.True(t, slices.Contains(ns.AllowedDomains, "github.com"))
	// The per-file error names the file on stderr (captured in out).
	require.Contains(t, out, "b-bad.clawk")
}
