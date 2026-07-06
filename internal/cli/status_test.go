package cli

// Phase 5 tests: dashboard, --brief, --json v2, JSON-only list verbs.
// The dashboard test asserts a few load-bearing labels — full layout
// matching would be too brittle for a renderer that is supposed to
// gain PR badges in Phase 6.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/stretchr/testify/require"
)

func TestStatusDashboardRendersWorktreesAndForwards(t *testing.T) {
	s, mock := setupTest(t)

	sb := &config.Sandbox{
		Name:      "INFRA-512",
		Provider:  config.ProviderVZ,
		Image:     "docker.io/docker/sandbox-templates:claude-code",
		VMState:   config.VMStateRunning,
		VMPid:     12345,
		GuestIP:   "192.168.64.10",
		CreatedAt: time.Now().Add(-1 * time.Hour),
		Phases: []config.Phase{
			{Repo: "/tmp/k8s", Branch: "INFRA-512", Status: config.PhaseStatusActive,
				Worktree: "/tmp/wt/k8s", Setup: []string{"make deps"}},
			{Repo: "/tmp/mono", Branch: "INFRA-512", Status: config.PhaseStatusPending,
				Worktree: "/tmp/wt/mono"},
		},
		Forwards: []config.PortForward{
			{HostPort: 3000, GuestPort: 3000},
			{HostPort: 8080, GuestPort: 80},
		},
		Network: config.NetworkPolicy{
			AllowedDomains: []string{"example.com", "api.example.com"},
			AllowedIPs:     []string{"10.0.0.5"},
		},
	}
	require.NoError(t, s.Save(sb))
	mock.Running["INFRA-512"] = true

	out, err := executeCommand("status", "INFRA-512")
	require.NoError(t, err, "status")
	for _, want := range []string{
		"INFRA-512", "ticket", "vz",
		"image  docker.io/docker/sandbox-templates:claude-code",
		"Worktrees", "k8s", "mono",
		"Forwards", "3000 → 3000", "8080 → 80",
		"Network", "use: default",
		"Setup", "k8s (1)",
	} {
		require.Contains(t, out, want, "dashboard missing %q", want)
	}
}

func TestStatusBriefOneLine(t *testing.T) {
	s, _ := setupTest(t)

	sb := &config.Sandbox{
		Name:     "BR-1",
		Provider: config.ProviderVZ,
		VMState:  config.VMStateStopped,
		Phases: []config.Phase{
			{Repo: "/tmp/r", Branch: "BR-1", Status: config.PhaseStatusPending},
		},
	}
	s.Save(sb)

	out, err := executeCommand("status", "BR-1", "--brief")
	require.NoError(t, err, "status --brief")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	require.Len(t, lines, 1, "brief should be one line")
	require.Contains(t, out, "BR-1", "brief missing sandbox name")
	require.Contains(t, out, "ticket", "brief missing mode label")
}

func TestStatusJSONv2HasForwardsAndNetwork(t *testing.T) {
	s, _ := setupTest(t)

	sb := &config.Sandbox{
		Name:      "JSON-1",
		Provider:  config.ProviderVZ,
		VMState:   config.VMStateStopped,
		CreatedAt: time.Now(),
		Forwards:  []config.PortForward{{HostPort: 3000, GuestPort: 3000}},
		Network: config.NetworkPolicy{
			AllowedDomains: []string{"example.com"},
		},
	}
	s.Save(sb)

	out, err := executeCommand("status", "JSON-1", "--json")
	require.NoError(t, err, "status --json")

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got), "invalid JSON\n%s", out)
	require.Equal(t, "2", got["schema"], "schema")
	require.Equal(t, "ticket", got["mode"], "mode")
	_, hasForwards := got["forwards"]
	require.True(t, hasForwards, "v2 schema must include forwards: %s", out)
	_, hasNetwork := got["network"]
	require.True(t, hasNetwork, "v2 schema must include network: %s", out)
}

func TestStatusBriefAndJSONExclusive(t *testing.T) {
	s, _ := setupTest(t)
	sb := &config.Sandbox{Name: "X", Provider: config.ProviderVZ, VMState: config.VMStateStopped}
	s.Save(sb)

	_, err := executeCommand("status", "X", "--brief", "--json")
	require.Error(t, err, "expected error for --brief + --json")
}

func TestStatusCwdSandboxModeLabel(t *testing.T) {
	s, _ := setupTest(t)
	sb := &config.Sandbox{
		Name:     "myproj",
		Provider: config.ProviderVZ,
		Anchor:   "/work/myproj",
		VMState:  config.VMStateStopped,
	}
	s.Save(sb)

	out, err := executeCommand("status", sb.Name, "--brief")
	require.NoError(t, err)
	// mode=cwd is derived from the Anchor binding, not the sandbox name.
	require.Contains(t, out, "cwd", "anchored sandbox should report mode=cwd")
	require.Contains(t, out, "myproj", "brief output missing sandbox name 'myproj'")
}

func TestForwardListRequiresJSON(t *testing.T) {
	s, _ := setupTest(t)
	sb := &config.Sandbox{Name: "fwd", Provider: config.ProviderVZ, VMState: config.VMStateStopped,
		Forwards: []config.PortForward{{HostPort: 3000, GuestPort: 3000}}}
	s.Save(sb)

	_, err := executeCommand("forward", "list", "fwd")
	require.Error(t, err, "expected error without --json")
	require.Contains(t, err.Error(), "JSON-only")

	out, err := executeCommand("forward", "list", "fwd", "--json")
	require.NoError(t, err, "with --json")
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got), "not JSON\n%s", out)
}

func TestNetworkListRequiresJSON(t *testing.T) {
	s, _ := setupTest(t)
	sb := &config.Sandbox{Name: "net", Provider: config.ProviderVZ, VMState: config.VMStateStopped,
		Network: config.NetworkPolicy{AllowedDomains: []string{"example.com"}}}
	s.Save(sb)

	_, err := executeCommand("network", "list", "net")
	require.Error(t, err, "expected error without --json")
	require.Contains(t, err.Error(), "JSON-only")

	_, err = executeCommand("network", "list", "net", "--json")
	require.NoError(t, err, "with --json")
}

func TestWorktreeListRequiresJSON(t *testing.T) {
	s, _ := setupTest(t)
	sb := &config.Sandbox{Name: "wt", Provider: config.ProviderVZ, VMState: config.VMStateStopped,
		Phases: []config.Phase{{Repo: "/tmp/r", Branch: "x"}}}
	s.Save(sb)

	_, err := executeCommand("worktree", "list", "wt")
	require.Error(t, err, "expected error without --json")
	require.Contains(t, err.Error(), "JSON-only")

	_, err = executeCommand("worktree", "list", "wt", "--json")
	require.NoError(t, err, "with --json")
}
