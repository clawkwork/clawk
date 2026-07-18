package template

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// parseBody wraps a directive body in the typed `sandbox ( ... )` block and
// parses it. These tests exercise the directive grammar; the block wrapper
// itself is covered by resources_test.go.
func parseBody(src string) (*Template, error) {
	return ParseString("sandbox (\n" + src + "\n)\n")
}

func TestParseMinimal(t *testing.T) {
	tmpl, err := parseBody("vm (\n    provider vz\n)\n")
	require.NoError(t, err)
	if tmpl.Provider != "vz" {
		t.Errorf("provider = %q, want vz", tmpl.Provider)
	}
}

func TestParseFull(t *testing.T) {
	src := `# feature dev template
vm (
    provider vz
)

includes (
    ~/code/k8s-deploy
    ~/code/monorepo
)

network (
    allow github.com
    allow *.github.com
    allow ip 10.0.0.5
    allow ip 192.168.10.0/24
)
`
	tmpl, err := parseBody(src)
	require.NoError(t, err)
	if tmpl.Provider != "vz" {
		t.Errorf("provider %q", tmpl.Provider)
	}
	if len(tmpl.Includes) != 2 {
		t.Errorf("want 2 includes, got %v", tmpl.Includes)
	}
	if len(tmpl.Domains) != 2 {
		t.Errorf("want 2 domains, got %v", tmpl.Domains)
	}
	if len(tmpl.IPs) != 2 {
		t.Errorf("want 2 IPs, got %v", tmpl.IPs)
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		wantErr string
	}{
		{"unknown directive", "plover foo", "unknown directive"},
		{"v1 top-level provider rejected", "provider vz\n", "unknown directive"},
		{"v1 top-level cpu rejected", "cpu 4\n", "unknown directive"},
		{"v1 top-level allow rejected", "allow github.com\n", "unknown directive"},
		{"v1 top-level setup rejected", "setup \"make\"\n", "unknown directive"},
		{"includes missing paren", "includes\n~/a\n", "expected '('"},
		{"legacy repos directive rejected with hint", "repos (\n~/a\n)\n", "replaced by 'includes'"},
		{"duplicate provider", "vm (\nprovider vz\nprovider firecracker\n)\n", "duplicate"},
		{"provider keyword", "vm (\nprovider provider\n)\n", "reserved"},
		{"ip without arg", "network (\nallow ip\n)", "expected IP"},
		{"bare keyword in network entry", "network (\nallow ip 1.2.3.4\nallow vm\n)", "unexpected keyword"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseBody(c.src)
			if err == nil {
				t.Fatalf("%s: expected error", c.name)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error %q does not contain %q", err, c.wantErr)
			}
		})
	}
}

func TestParseEmpty(t *testing.T) {
	// An empty sandbox block is a valid (all-defaults) template.
	tmpl, err := parseBody("")
	require.NoError(t, err)
	if tmpl.Provider != "" || len(tmpl.Includes) != 0 {
		t.Errorf("empty template should produce empty Template, got %+v", tmpl)
	}
	// A file with no sandbox block at all is not: ParseString exists for
	// callers that need the template, so absence is an error, not a nil.
	_, err = ParseString("")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no sandbox block")
}

func TestParseForwards(t *testing.T) {
	src := `forwards (
    3000
    8080:80
)
`
	tmpl, err := parseBody(src)
	require.NoError(t, err)
	if len(tmpl.Forwards) != 2 || tmpl.Forwards[0] != "3000" || tmpl.Forwards[1] != "8080:80" {
		t.Errorf("forwards parsed wrong: %v", tmpl.Forwards)
	}
}

// TestParseInlineForms checks the go.mod-style syntax where single-entry
// directives can be written without parentheses — `forwards 3000` is
// equivalent to `forwards ( 3000 )`. Multi-entry directives still need the
// block form.
func TestParseInlineForms(t *testing.T) {
	src := `forwards 3000
env GITHUB_TOKEN
includes ~/code/app
`
	tmpl, err := parseBody(src)
	require.NoError(t, err)
	if len(tmpl.Forwards) != 1 || tmpl.Forwards[0] != "3000" {
		t.Errorf("forwards = %v", tmpl.Forwards)
	}
	if len(tmpl.Env) != 1 || tmpl.Env[0] != "GITHUB_TOKEN" {
		t.Errorf("env = %v", tmpl.Env)
	}
	if len(tmpl.Includes) != 1 || tmpl.Includes[0] != "~/code/app" {
		t.Errorf("includes = %v", tmpl.Includes)
	}
}

// TestParseEnvComposeModel covers the full env grammar — passthrough,
// alias, :- / - defaults, :? required, and bare/quoted literals — and
// checks each entry is stored in canonical envspec form. Whitespace
// around '=' is flexible (the lexer emits a standalone '=' token), and a
// ${…} value may contain spaces because it scans as one unit.
func TestParseEnvComposeModel(t *testing.T) {
	src := `env (
    NPM_TOKEN
    GH_TOKEN  = ${ACME_GH_TOKEN}
    LOG_LEVEL = ${LOG_LEVEL:-info}
    PORT      = ${PORT-8080}
    API_KEY   = ${API_KEY:?set it in your shell}
    EDITOR=vim
    BANNER    = "hi there"
)
`
	tmpl, err := parseBody(src)
	require.NoError(t, err)
	require.Equal(t, []string{
		"NPM_TOKEN",
		"GH_TOKEN=${ACME_GH_TOKEN}",
		"LOG_LEVEL=${LOG_LEVEL:-info}",
		"PORT=${PORT-8080}",
		"API_KEY=${API_KEY:?set it in your shell}",
		"EDITOR=vim",
		`BANNER="hi there"`,
	}, tmpl.Env)
}

// TestParseEnvErrors: malformed env entries fail at parse time with a
// traceable message rather than surfacing later in the guest.
func TestParseEnvErrors(t *testing.T) {
	for _, src := range []string{
		"env ( 1BAD )\n",           // bad name
		"env ( GH = ${1BAD} )\n",   // bad host name
		"env ( GH = ${HOST:x} )\n", // unknown operator
		"env ( GH = )\n",           // missing value
	} {
		if _, err := parseBody(src); err == nil {
			t.Errorf("parseBody(%q) = nil error, want failure", src)
		}
	}
}

// TestParseNetworkAccumulates: allow/deny entries within a network block
// accumulate into the domain/IP slices.
func TestParseNetworkAccumulates(t *testing.T) {
	src := `network (
    allow *.dev.myco.com
    allow *.stage.myco.com
    allow ip 10.0.0.5
    allow ip 192.168.0.0/24
)
`
	tmpl, err := parseBody(src)
	require.NoError(t, err)
	if len(tmpl.Domains) != 2 {
		t.Errorf("domains = %v, want 2", tmpl.Domains)
	}
	if len(tmpl.IPs) != 2 {
		t.Errorf("ips = %v, want 2", tmpl.IPs)
	}
}

func TestParseResources(t *testing.T) {
	src := `vm (
    provider   vz
    cpu        8
    memory     2GiB
    memory_max 16GiB
)
`
	tmpl, err := parseBody(src)
	require.NoError(t, err)
	if tmpl.CPU != 8 {
		t.Errorf("cpu = %d, want 8", tmpl.CPU)
	}
	if tmpl.MemoryMiB != 2048 {
		t.Errorf("memory = %d MiB, want 2048", tmpl.MemoryMiB)
	}
	if tmpl.MemoryMaxMiB != 16384 {
		t.Errorf("memory_max = %d MiB, want 16384", tmpl.MemoryMaxMiB)
	}
}

func TestParseResourceUnits(t *testing.T) {
	cases := []struct {
		src     string
		wantMiB uint64
	}{
		{"memory 512MiB", 512},
		{"memory 1GiB", 1024},
		{"memory 1G", 1024},
		{"memory 1T", 1024 * 1024},
		{"memory 1024M", 1024},
		{"memory 1GB", 953},    // 1e9 / 2^20 = 953.67 -> truncates to 953
		{"memory 1000MB", 953}, // same
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			// memory/cpu are vm-block directives; wrap the one-liner.
			tmpl, err := parseBody("vm (\n" + c.src + "\n)\n")
			if err != nil {
				t.Fatalf("parse %q: %v", c.src, err)
			}
			if tmpl.MemoryMiB != c.wantMiB {
				t.Errorf("%q: got %d MiB, want %d", c.src, tmpl.MemoryMiB, c.wantMiB)
			}
		})
	}
}

func TestParseResourceErrors(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		wantErr string
	}{
		{"bare memory number", "memory 4096", "unit suffix"},
		{"unknown memory unit", "memory 4XB", "unknown unit"},
		{"empty memory", "memory\n", "expected size"},
		{"zero cpu", "cpu 0", ">= 1"},
		{"non-numeric cpu", "cpu eight", "invalid cpu count"},
		{"duplicate cpu", "cpu 2\ncpu 4\n", "duplicate"},
		{"duplicate memory", "memory 1G\nmemory 2G\n", "duplicate"},
		{"duplicate memory_max", "memory_max 1G\nmemory_max 2G\n", "duplicate"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// cpu/memory are vm-block directives; wrap the one-liner.
			_, err := parseBody("vm (\n" + c.src + "\n)\n")
			if err == nil {
				t.Fatalf("%s: expected error", c.name)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error %q does not contain %q", err, c.wantErr)
			}
		})
	}
}

func TestParseIdleTimeout(t *testing.T) {
	cases := []struct {
		src     string
		wantSec int64
	}{
		{"idle_timeout 30m", 1800},
		{"idle_timeout 1h30m", 5400},
		{"idle_timeout 90s", 90},
		{"idle_timeout off", -1},
		{"idle_timeout 0", -1},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			tmpl, err := parseBody("vm (\n" + c.src + "\n)\n")
			require.NoError(t, err)
			require.Equal(t, c.wantSec, tmpl.IdleTimeoutSec)
		})
	}
}

func TestParseIdleTimeoutErrors(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		wantErr string
	}{
		{"not a duration", "idle_timeout soon", "invalid idle_timeout"},
		{"bare number", "idle_timeout 30", "invalid idle_timeout"},
		{"sub-minute", "idle_timeout 30s", "too short"},
		{"missing value", "idle_timeout\n", "expected duration"},
		{"duplicate", "idle_timeout 30m\nidle_timeout 1h\n", "duplicate"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseBody("vm (\n" + c.src + "\n)\n")
			if err == nil {
				t.Fatalf("%s: expected error", c.name)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error %q does not contain %q", err, c.wantErr)
			}
		})
	}
}

func TestMergeIdleTimeout(t *testing.T) {
	base := &Template{IdleTimeoutSec: 1800}
	base.Merge(&Template{}) // unset overlay leaves base alone
	require.Equal(t, int64(1800), base.IdleTimeoutSec)
	base.Merge(&Template{IdleTimeoutSec: 3600})
	require.Equal(t, int64(3600), base.IdleTimeoutSec)
	base.Merge(&Template{IdleTimeoutSec: -1}) // profile can switch it off
	require.Equal(t, int64(-1), base.IdleTimeoutSec)
}

func TestParseBlankLines(t *testing.T) {
	src := "\n\n\nvm (\n\n    provider vz\n\n)\n\n\nincludes (\n\n    ~/a\n\n)\n\n"
	tmpl, err := parseBody(src)
	require.NoError(t, err)
	if tmpl.Provider != "vz" || len(tmpl.Includes) != 1 {
		t.Errorf("blank-line handling broken: %+v", tmpl)
	}
}

// TestParseV2VMBlock exercises the grouped `vm (...)` directive — every
// scalar VM setting is parsed from inside the block.
func TestParseV2VMBlock(t *testing.T) {
	src := `vm (
    provider   vz
    cpu        4
    memory     2GiB
    memory_max 8GiB
    nested
    image      golang:1.25
)
`
	tmpl, err := parseBody(src)
	require.NoError(t, err)
	if tmpl.Provider != "vz" {
		t.Errorf("provider = %q", tmpl.Provider)
	}
	if tmpl.CPU != 4 {
		t.Errorf("cpu = %d", tmpl.CPU)
	}
	if tmpl.MemoryMiB != 2048 || tmpl.MemoryMaxMiB != 8192 {
		t.Errorf("memory = (%d, %d)", tmpl.MemoryMiB, tmpl.MemoryMaxMiB)
	}
	if !tmpl.Nested {
		t.Errorf("nested not set")
	}
	if tmpl.Image != "golang:1.25" {
		t.Errorf("image = %q, want golang:1.25", tmpl.Image)
	}
}

// TestParseImage covers the `image` directive's reference shapes and
// error cases.
func TestParseImage(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		want    string
		wantErr string
	}{
		{name: "short ref", src: "vm (\nimage golang:1.25\n)\n", want: "golang:1.25"},
		{name: "registry ref", src: "vm (\nimage ghcr.io/clawkwork/clawk-dev:v0\n)\n",
			want: "ghcr.io/clawkwork/clawk-dev:v0"},
		{name: "quoted ref", src: "vm (\nimage \"docker.io/library/alpine:3.20\"\n)\n",
			want: "docker.io/library/alpine:3.20"},
		{name: "duplicate", src: "vm (\nimage a:1\nimage b:2\n)\n", wantErr: "duplicate"},
		{name: "missing value", src: "vm (\nimage\n)\n", wantErr: "expected image reference"},
		{name: "top-level rejected", src: "image golang:1.25\n", wantErr: "unknown directive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpl, err := parseBody(tt.src)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			require.NoError(t, err)
			if tmpl.Image != tt.want {
				t.Errorf("image = %q, want %q", tmpl.Image, tt.want)
			}
		})
	}
}

// TestParseKernel covers the `kernel` override directive (local path or
// URL) and its error cases.
func TestParseKernel(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		want    string
		wantErr string
	}{
		{name: "url", src: "vm (\nkernel https://example.com/vmlinux\n)\n", want: "https://example.com/vmlinux"},
		{name: "abs path", src: "vm (\nkernel /opt/kernels/vmlinux\n)\n", want: "/opt/kernels/vmlinux"},
		{name: "home path", src: "vm (\nkernel ~/kernels/vmlinux\n)\n", want: "~/kernels/vmlinux"},
		{name: "quoted", src: "vm (\nkernel \"/path with space/vmlinux\"\n)\n", want: "/path with space/vmlinux"},
		{name: "duplicate", src: "vm (\nkernel a\nkernel b\n)\n", wantErr: "duplicate"},
		{name: "missing value", src: "vm (\nkernel\n)\n", wantErr: "expected kernel path or URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpl, err := parseBody(tt.src)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			require.NoError(t, err)
			if tmpl.Kernel != tt.want {
				t.Errorf("kernel = %q, want %q", tmpl.Kernel, tt.want)
			}
		})
	}
}

// TestParseV2NetworkBlock checks the `network (...)` block accepts both
// `allow` and the new `deny` entries, with the latter routed to the
// DenyDomains/DenyIPs slices.
func TestParseV2NetworkBlock(t *testing.T) {
	src := `network (
    allow     api.example.com
    allow     *.example.com
    allow ip  10.0.0.0/8
    deny      tracker.example.com
    deny ip   1.2.3.4
)
`
	tmpl, err := parseBody(src)
	require.NoError(t, err)
	if len(tmpl.Domains) != 2 || tmpl.Domains[0] != "api.example.com" {
		t.Errorf("domains = %v", tmpl.Domains)
	}
	if len(tmpl.IPs) != 1 || tmpl.IPs[0] != "10.0.0.0/8" {
		t.Errorf("ips = %v", tmpl.IPs)
	}
	if len(tmpl.DenyDomains) != 1 || tmpl.DenyDomains[0] != "tracker.example.com" {
		t.Errorf("deny domains = %v", tmpl.DenyDomains)
	}
	if len(tmpl.DenyIPs) != 1 || tmpl.DenyIPs[0] != "1.2.3.4" {
		t.Errorf("deny ips = %v", tmpl.DenyIPs)
	}
}

// TestParseV2OnLifecycle covers the four event names. Each `on EVENT (...)`
// block should populate the corresponding list and not bleed into siblings.
func TestParseV2OnLifecycle(t *testing.T) {
	src := `on create (
    "pnpm install"
    "go mod download"
)

on up "scripts/start.sh"

on down (
    "scripts/flush.sh"
)
`
	tmpl, err := parseBody(src)
	require.NoError(t, err)
	if got := tmpl.OnCreate; len(got) != 2 || got[0] != "pnpm install" {
		t.Errorf("on_create = %v", got)
	}
	if got := tmpl.OnUp; len(got) != 1 || got[0] != "scripts/start.sh" {
		t.Errorf("on_up = %v", got)
	}
	if got := tmpl.OnDown; len(got) != 1 || got[0] != "scripts/flush.sh" {
		t.Errorf("on_down = %v", got)
	}
	if len(tmpl.OnEnter) != 0 {
		t.Errorf("on_enter should be empty, got %v", tmpl.OnEnter)
	}
}

// TestParseV2OnUnknownEvent rejects `on EVENT` for any EVENT not in the
// fixed set. Pointing the user at the supported list beats silently
// dropping commands.
func TestParseV2OnUnknownEvent(t *testing.T) {
	_, err := parseBody(`on bogus ( "x" )`)
	require.Error(t, err)
	if !strings.Contains(err.Error(), "unknown 'on' event") {
		t.Errorf("error %q did not name the unknown-event case", err)
	}
}

// TestParseV2Skills covers the three valid path shapes plus an explicit
// version on the distributed entry. Bare names must error.
func TestParseV2Skills(t *testing.T) {
	src := `skills (
    ~/.claude/skills/idiomatic-go
    ./.claude/skills/audit-helper
    github.com/anthropics/skills/claude-api    v1.2.3
)
`
	tmpl, err := parseBody(src)
	require.NoError(t, err)
	if len(tmpl.Skills) != 3 {
		t.Fatalf("want 3 skills, got %d (%+v)", len(tmpl.Skills), tmpl.Skills)
	}
	if got := tmpl.Skills[0]; got.Kind != SkillKindLocalHome || got.Version != "" {
		t.Errorf("skill[0] = %+v", got)
	}
	if got := tmpl.Skills[1]; got.Kind != SkillKindLocalWorkspace {
		t.Errorf("skill[1] = %+v", got)
	}
	if got := tmpl.Skills[2]; got.Kind != SkillKindDistributed || got.Version != "v1.2.3" {
		t.Errorf("skill[2] = %+v", got)
	}
}

// TestParseV2SkillsBareNameRejected enforces the design rule that bare
// names like `idiomatic-go` are not valid — every entry must be path-shaped.
func TestParseV2SkillsBareNameRejected(t *testing.T) {
	_, err := parseBody("skills (\n    idiomatic-go\n)\n")
	require.Error(t, err)
	if !strings.Contains(err.Error(), "must be a path") {
		t.Errorf("error %q did not point at the path-shape requirement", err)
	}
}

// TestParseV2SkillsLocalRejectsVersion enforces that local paths cannot
// carry a version — there's nothing to resolve for them.
func TestParseV2SkillsLocalRejectsVersion(t *testing.T) {
	_, err := parseBody("skills (\n    ~/foo v1.0.0\n)\n")
	require.Error(t, err)
	if !strings.Contains(err.Error(), "not allowed on a local path") {
		t.Errorf("error %q did not point at the local-version rule", err)
	}
}

// TestParseV2Files exercises every accepted shape of a `files (...)` line:
// host alone, host+guest, host+mode, host+guest+mode, and mode-without-guest.
// Mode tokens are recognised by their `0NNN` shape regardless of position
// so users don't have to repeat the host path just to override the mode.
func TestParseV2Files(t *testing.T) {
	src := `files (
    ~/.kube/config_staging_only
    ~/.aws/config                /home/agent/.aws/config
    ~/.netrc                     0600
    ~/.docker/config.json        /home/agent/.docker/config.json   0600
)
`
	tmpl, err := parseBody(src)
	require.NoError(t, err)
	if len(tmpl.Files) != 4 {
		t.Fatalf("want 4 file entries, got %d (%+v)", len(tmpl.Files), tmpl.Files)
	}
	cases := []struct {
		host, guest string
		mode        uint32
	}{
		{"~/.kube/config_staging_only", "", 0},
		{"~/.aws/config", "/home/agent/.aws/config", 0},
		{"~/.netrc", "", 0o600},
		{"~/.docker/config.json", "/home/agent/.docker/config.json", 0o600},
	}
	for i, want := range cases {
		got := tmpl.Files[i]
		if got.HostPath != want.host || got.GuestPath != want.guest || uint32(got.Mode) != want.mode {
			t.Errorf("Files[%d] = {host:%q guest:%q mode:%#o}, want {host:%q guest:%q mode:%#o}",
				i, got.HostPath, got.GuestPath, got.Mode, want.host, want.guest, want.mode)
		}
	}
}

// TestParseV2FilesErrors covers the three rejection paths: a trailing
// fourth token, two mode tokens on one entry, and two non-mode tokens
// (which would otherwise quietly clobber the guest path).
func TestParseV2FilesErrors(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		wantErr string
	}{
		{
			"duplicate mode",
			"files (\n    ~/a 0600 0644\n)\n",
			"duplicate mode",
		},
		{
			"trailing path token",
			"files (\n    ~/a /home/agent/a /home/agent/b\n)\n",
			"unexpected token",
		},
		{
			"missing paren",
			"files\n    ~/a\n",
			"expected '('",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseBody(c.src)
			if err == nil {
				t.Fatalf("%s: expected error", c.name)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error %q does not contain %q", err, c.wantErr)
			}
		})
	}
}

// TestParseV2Shares covers the four shape variants of a `shares (...)` line.
// The default-readonly rule is asserted on the bare-path entry — that's
// the one that would silently invert behavior if someone changed the
// parse-time default.
func TestParseV2Shares(t *testing.T) {
	src := `shares (
    ~/.aws
    ~/.config/gcloud             /home/agent/.config/gcloud
    ~/.terraform.d               rw
    ~/.ssh/known_hosts.d         /home/agent/known_hosts.d         ro
)
`
	tmpl, err := parseBody(src)
	require.NoError(t, err)
	if len(tmpl.Shares) != 4 {
		t.Fatalf("want 4 share entries, got %d (%+v)", len(tmpl.Shares), tmpl.Shares)
	}
	cases := []struct {
		host, guest string
		ro          bool
	}{
		{"~/.aws", "", true},
		{"~/.config/gcloud", "/home/agent/.config/gcloud", true},
		{"~/.terraform.d", "", false},
		{"~/.ssh/known_hosts.d", "/home/agent/known_hosts.d", true},
	}
	for i, want := range cases {
		got := tmpl.Shares[i]
		if got.HostPath != want.host || got.GuestPath != want.guest || got.ReadOnly != want.ro {
			t.Errorf("Shares[%d] = {host:%q guest:%q ro:%v}, want {host:%q guest:%q ro:%v}",
				i, got.HostPath, got.GuestPath, got.ReadOnly, want.host, want.guest, want.ro)
		}
	}
}

// TestParseV2SharesErrors covers the duplicate-flag and trailing-token
// rejection paths. A two-flag entry would otherwise let `ro rw` quietly
// flip the last one wins; we surface it as a syntax error instead.
func TestParseV2SharesErrors(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		wantErr string
	}{
		{
			"duplicate flag",
			"shares (\n    ~/a ro rw\n)\n",
			"duplicate ro/rw flag",
		},
		{
			"trailing path token",
			"shares (\n    ~/a /home/agent/a /home/agent/b\n)\n",
			"unexpected token",
		},
		{
			"missing paren",
			"shares\n    ~/a\n",
			"expected '('",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseBody(c.src)
			if err == nil {
				t.Fatalf("%s: expected error", c.name)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error %q does not contain %q", err, c.wantErr)
			}
		})
	}
}

// TestParseV2FilesSharesMerge guards that an overlay can ADD entries to
// both lists without dropping the base list — Merge uses plain append for
// these slices, matching Forwards/Env semantics.
func TestParseV2FilesSharesMerge(t *testing.T) {
	base, err := parseBody("files ( ~/.kube/cfg )\nshares ( ~/.aws )\n")
	require.NoError(t, err)
	over, err := parseBody("files ( ~/.netrc 0600 )\nshares ( ~/.terraform.d rw )\n")
	require.NoError(t, err)
	base.Merge(over)
	if len(base.Files) != 2 {
		t.Errorf("Files after merge = %+v, want 2 entries", base.Files)
	}
	if len(base.Shares) != 2 {
		t.Errorf("Shares after merge = %+v, want 2 entries", base.Shares)
	}
}

func TestParseAgentBlock(t *testing.T) {
	// Mixed inline strings and a file path, order preserved; markdown lives in
	// the referenced file so it never has to survive the DSL string lexer.
	src := `agent (
    instructions "Ask before destructive commands."
    instructions (
        "Prefer pnpm over npm."
        ./CONVENTIONS.md
    )
    memory ./memory.seed.md
)
`
	tmpl, err := parseBody(src)
	require.NoError(t, err)
	require.Equal(t, []AgentDoc{
		{Text: "Ask before destructive commands."},
		{Text: "Prefer pnpm over npm."},
		{Path: "./CONVENTIONS.md"},
	}, tmpl.Instructions)
	require.Equal(t, []AgentDoc{{Path: "./memory.seed.md"}}, tmpl.Memory)
}

func TestParseAgentBlockInlineMemory(t *testing.T) {
	tmpl, err := parseBody("agent (\n  memory \"# Baseline\\n- needs redis\"\n)\n")
	require.NoError(t, err)
	require.Equal(t, []AgentDoc{{Text: "# Baseline\n- needs redis"}}, tmpl.Memory)
}

func TestParseAgentBlockRejectsUnknownDirective(t *testing.T) {
	_, err := parseBody("agent (\n  soul \"x\"\n)\n")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown 'agent' directive")
}
