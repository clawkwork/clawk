package cli

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/netfilter"
	"github.com/clawkwork/clawk/internal/vzdctl"
	"github.com/stretchr/testify/require"
)

// startFakeControlServer brings up a vzdctl server on the sandbox's real
// control-socket path, standing in for a running vzd.
func startFakeControlServer(t *testing.T, s *config.Store, name string, h vzdctl.Handlers) {
	t.Helper()
	vmDir := s.VMDir(name)
	require.NoError(t, os.MkdirAll(vmDir, 0o755), "creating vm dir")
	srv, err := vzdctl.Start(vzdctl.SocketPath(vmDir), h)
	require.NoError(t, err, "starting fake control server")
	t.Cleanup(func() { srv.Close() })
}

func TestNetworkAllowAppliesLive(t *testing.T) {
	s, _ := setupTest(t)
	require.NoError(t, s.Save(&config.Sandbox{Name: "demo"}))

	var reloads int
	startFakeControlServer(t, s, "demo", vzdctl.Handlers{
		Denials: func() []netfilter.Denial { return nil },
		Reload:  func() error { reloads++; return nil },
	})

	out, err := executeCommand("network", "allow", "demo", "api.example.com")
	require.NoError(t, err, "network allow")
	require.Contains(t, out, "Applied to running sandbox.", "output should confirm live apply")
	require.Equal(t, 1, reloads, "daemon reloaded times")
	sb, err := s.Load("demo")
	require.NoError(t, err)
	require.Len(t, sb.Network.Block(config.BlockOriginCustom).AllowDomains, 1, "store should hold the new domain")
	require.Equal(t, "api.example.com", sb.Network.Block(config.BlockOriginCustom).AllowDomains[0])
}

func TestNetworkAllowRoutesMixedEntries(t *testing.T) {
	s, _ := setupTest(t)
	require.NoError(t, s.Save(&config.Sandbox{Name: "demo"}))

	out, err := executeCommand("network", "allow", "demo",
		"*.example.com", "10.0.0.5", "192.168.10.0/24", "api.test.dev.")
	require.NoError(t, err, "network allow with mixed entries")
	for _, want := range []string{"Allowed: *.example.com", "Allowed: 10.0.0.5",
		"Allowed: 192.168.10.0/24", "Allowed: api.test.dev"} {
		require.Contains(t, out, want)
	}

	sb, err := s.Load("demo")
	require.NoError(t, err)
	custom := sb.Network.Block(config.BlockOriginCustom)
	require.Equal(t, []string{"*.example.com", "api.test.dev"}, custom.AllowDomains,
		"domains should land in the custom block's allows, trailing dot trimmed")
	require.Equal(t, []string{"10.0.0.5", "192.168.10.0/24"}, custom.AllowIPs,
		"IPs and CIDRs should land in the custom block's IP allows")
}

func TestNetworkAllowRejectsMalformedEntries(t *testing.T) {
	tests := []struct {
		name    string
		entry   string
		wantErr string
	}{
		{"malformed CIDR", "10.0.0.0/33", "not a valid CIDR"},
		{"impossible octet", "10.0.0.999", "not a valid IP"},
		{"host with port", "example.com:443", "ports aren't part of the policy"},
		{"URL scheme", "https://example.com", "schemes aren't part of the policy"},
		{"malformed IPv6", "fe80::zzzz", "not a valid IPv6"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := setupTest(t)
			require.NoError(t, s.Save(&config.Sandbox{Name: "demo"}))

			// A valid entry ahead of the bad one must not be half-applied.
			_, err := executeCommand("network", "allow", "demo", "good.example.com", tt.entry)
			require.ErrorContains(t, err, tt.wantErr)

			sb, err := s.Load("demo")
			require.NoError(t, err)
			require.Empty(t, sb.Network.Blocks, "store must be untouched on a rejected entry")
		})
	}
}

func TestNetworkAllowHealsMisfiledIP(t *testing.T) {
	s, _ := setupTest(t)
	sb := &config.Sandbox{Name: "demo"}
	sb.Network.AllowedDomains = []string{"example.com", "10.0.0.5"}
	require.NoError(t, s.Save(sb))

	_, err := executeCommand("network", "allow", "demo", "10.0.0.5")
	require.NoError(t, err, "network allow")

	got, err := s.Load("demo")
	require.NoError(t, err)
	custom := got.Network.Block(config.BlockOriginCustom)
	require.Equal(t, []string{"example.com"}, custom.AllowDomains,
		"the misfiled IP should leave the domain allows")
	require.Equal(t, []string{"10.0.0.5"}, custom.AllowIPs,
		"the IP should move to the IP allows")
}

func TestNetworkRemoveDropsFromBothLists(t *testing.T) {
	s, _ := setupTest(t)
	sb := &config.Sandbox{Name: "demo"}
	sb.Network.AllowedDomains = []string{"example.com", "keep.example.com"}
	sb.Network.AllowedIPs = []string{"10.0.0.5", "192.168.10.0/24"}
	require.NoError(t, s.Save(sb))

	out, err := executeCommand("network", "remove", "demo", "example.com", "10.0.0.5")
	require.NoError(t, err, "network remove")
	require.Contains(t, out, "Removed: example.com (was allowed)")
	require.Contains(t, out, "Removed: 10.0.0.5 (was allowed)")

	got, err := s.Load("demo")
	require.NoError(t, err)
	custom := got.Network.Block(config.BlockOriginCustom)
	require.Equal(t, []string{"keep.example.com"}, custom.AllowDomains)
	require.Equal(t, []string{"192.168.10.0/24"}, custom.AllowIPs)
}

// remove is the one removal verb: it deletes block rules too, so
// block/remove round-trip without a separate unblock command.
func TestNetworkRemoveDropsBlockedEntries(t *testing.T) {
	s, _ := setupTest(t)
	require.NoError(t, s.Save(&config.Sandbox{Name: "demo"}))

	out, err := executeCommand("network", "block", "demo", "tracker.example.com")
	require.NoError(t, err, "network block")
	require.Contains(t, out, "Blocked: tracker.example.com")

	out, err = executeCommand("network", "remove", "demo", "tracker.example.com", "nosuch.example.com")
	require.NoError(t, err, "network remove")
	require.Contains(t, out, "Removed: tracker.example.com (was blocked)")
	// A miss is named, not silent — that silence used to read as the
	// command not working.
	require.Contains(t, out, "No rule for: nosuch.example.com")

	got, err := s.Load("demo")
	require.NoError(t, err)
	require.Empty(t, got.Network.Block(config.BlockOriginCustom).DenyDomains)
}

// "deny" was this command's pre-release name, retired without an alias
// before release — the verb stays free in case a real deny-rule command
// ever wants it. Asserted behaviorally (the entry survives) because an
// unknown subcommand prints the parent's help rather than erroring.
func TestNetworkDenyIsGone(t *testing.T) {
	s, _ := setupTest(t)
	sb := &config.Sandbox{Name: "demo"}
	sb.Network.AllowedDomains = []string{"example.com"}
	require.NoError(t, s.Save(sb))

	out, _ := executeCommand("network", "deny", "demo", "example.com")
	require.NotContains(t, out, "Removed:", "'network deny' must not resolve to remove")

	got, err := s.Load("demo")
	require.NoError(t, err)
	require.Contains(t, got.Network.Block(config.BlockOriginCustom).AllowDomains, "example.com",
		"'network deny' must not touch the allow list")
}

func TestNetworkAllowIPRejectsNonAddresses(t *testing.T) {
	s, _ := setupTest(t)
	require.NoError(t, s.Save(&config.Sandbox{Name: "demo"}))

	_, err := executeCommand("network", "allow-ip", "demo", "example.com")
	require.ErrorContains(t, err, "not an IP or CIDR range")

	sb, err := s.Load("demo")
	require.NoError(t, err)
	require.Empty(t, sb.Network.Blocks, "store must be untouched on a rejected entry")
}

func TestNetworkAllowWhenSandboxDown(t *testing.T) {
	s, _ := setupTest(t)
	require.NoError(t, s.Save(&config.Sandbox{Name: "demo"}))

	out, err := executeCommand("network", "allow", "demo", "api.example.com")
	require.NoError(t, err, "network allow")
	require.Contains(t, out, "applies on next 'clawk up'", "output should explain deferred apply")
}

func TestNetworkAllowReportsFailedLiveApply(t *testing.T) {
	s, _ := setupTest(t)
	require.NoError(t, s.Save(&config.Sandbox{Name: "demo"}))
	startFakeControlServer(t, s, "demo", vzdctl.Handlers{
		Denials: func() []netfilter.Denial { return nil },
		Reload:  func() error { return errors.New("boom") },
	})

	out, err := executeCommand("network", "allow", "demo", "api.example.com")
	require.NoError(t, err, "network allow should not fail the command")
	require.Contains(t, out, "live apply failed", "output should surface the live-apply error")
	require.Contains(t, out, "boom", "output should include error detail")
}

func TestNetworkDenials(t *testing.T) {
	s, _ := setupTest(t)
	t.Cleanup(func() { networkDenialsJSON = false })
	require.NoError(t, s.Save(&config.Sandbox{Name: "demo"}))

	ledger := []netfilter.Denial{{
		Host: "api.blocked.dev", IP: "9.9.9.9", Port: "443", Count: 14,
		FirstSeen: time.Now().Add(-10 * time.Minute),
		LastSeen:  time.Now().Add(-2 * time.Minute),
	}}
	startFakeControlServer(t, s, "demo", vzdctl.Handlers{
		Denials: func() []netfilter.Denial { return ledger },
		Reload:  func() error { return nil },
	})

	t.Run("human table", func(t *testing.T) {
		out, err := executeCommand("network", "denials", "demo")
		require.NoError(t, err, "network denials")
		for _, want := range []string{"HOST", "api.blocked.dev", "9.9.9.9", "443", "14", "clawk network allow demo"} {
			require.Contains(t, out, want, "output missing %q", want)
		}
	})

	t.Run("json", func(t *testing.T) {
		out, err := executeCommand("network", "denials", "demo", "--json")
		require.NoError(t, err, "network denials --json")
		for _, want := range []string{`"schema": "1"`, `"host": "api.blocked.dev"`, `"count": 14`} {
			require.Contains(t, out, want, "json output missing %q", want)
		}
	})
}

func TestNetworkDenialsEmpty(t *testing.T) {
	s, _ := setupTest(t)
	require.NoError(t, s.Save(&config.Sandbox{Name: "demo"}))
	startFakeControlServer(t, s, "demo", vzdctl.Handlers{
		Denials: func() []netfilter.Denial { return nil },
		Reload:  func() error { return nil },
	})

	out, err := executeCommand("network", "denials", "demo")
	require.NoError(t, err, "network denials")
	require.Contains(t, out, "No blocked connections recorded.", "output should report an empty ledger")
}

func TestNetworkDenialsSandboxDown(t *testing.T) {
	s, _ := setupTest(t)
	require.NoError(t, s.Save(&config.Sandbox{Name: "demo"}))

	_, err := executeCommand("network", "denials", "demo")
	require.ErrorIs(t, err, vzdctl.ErrNotRunning, "expected ErrNotRunning when no daemon is up")
}
