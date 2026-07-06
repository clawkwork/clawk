package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/netfilter"
	"github.com/stretchr/testify/require"
)

func TestApplyNamespaceDefaults_MergesFilesEnvNotNetwork(t *testing.T) {
	s, _ := setupTest(t)
	require.NoError(t, s.SaveNamespace(&config.Namespace{
		Name:           "work",
		AllowedDomains: []string{"corp.example.com"},
		Files:          []config.HostFile{{HostPath: "/h/ctx.md", GuestPath: "/g/ctx.md"}},
		Env:            []string{"CORP_TOKEN"},
	}))
	sb := &config.Sandbox{
		Name:      "x",
		Namespace: "work",
		Network:   config.NetworkPolicy{AllowedDomains: []string{"existing.com"}},
		Files:     []config.HostFile{{HostPath: "/h/own.md", GuestPath: "/g/own.md"}},
	}
	require.NoError(t, applyNamespaceDefaults(sb))
	// Network defaults resolve live at up/reload (resolveChain), never by
	// flattening at create — the namespace's domain must NOT be copied in.
	for _, b := range sb.Network.Blocks {
		require.NotContains(t, b.AllowDomains, "corp.example.com",
			"namespace network entries must not be flattened into the sandbox")
	}
	require.Len(t, sb.Files, 2, "namespace file not appended: %v", sb.Files)
	require.True(t, slices.Contains(sb.RequiredEnv, "CORP_TOKEN"), "env not merged: %v", sb.RequiredEnv)

	// The live chain, by contrast, carries both the namespace layer and the
	// sandbox's own entry (folded into its custom block by Normalize).
	sb.Network.Normalize()
	chain, warnings := resolveChain(sb)
	require.Empty(t, warnings)
	v, m := chain.DecideName("corp.example.com")
	require.Equal(t, netfilter.VerdictAllow, v, "namespace layer missing from chain")
	require.Equal(t, "namespace", m.Origin)
	v, _ = chain.DecideName("existing.com")
	require.Equal(t, netfilter.VerdictAllow, v, "sandbox custom layer missing from chain")
}

func TestApplyNamespaceDefaults_SandboxWinsOnGuestPathConflict(t *testing.T) {
	s, _ := setupTest(t)
	require.NoError(t, s.SaveNamespace(&config.Namespace{
		Name:  "work",
		Files: []config.HostFile{{HostPath: "/ns.md", GuestPath: "/g/ctx.md"}},
	}))
	sb := &config.Sandbox{
		Name:      "x",
		Namespace: "work",
		Files:     []config.HostFile{{HostPath: "/own.md", GuestPath: "/g/ctx.md"}},
	}
	require.NoError(t, applyNamespaceDefaults(sb))
	require.Len(t, sb.Files, 1, "sandbox-specific file should win on guest-path conflict")
	require.Equal(t, "/own.md", sb.Files[0].HostPath, "sandbox-specific file should win on guest-path conflict")
}

func TestApplyNamespaceDefaults_SeedsInstructionsAndMemory(t *testing.T) {
	s, _ := setupTest(t)
	require.NoError(t, s.SaveNamespace(&config.Namespace{
		Name:         "work",
		Instructions: []string{"namespace rule"},
		Memory:       "# Team baseline",
	}))
	sb := &config.Sandbox{
		Name:         "x",
		Namespace:    "work",
		Instructions: []string{"repo rule"},
		Memory:       "# Repo baseline",
	}
	require.NoError(t, applyNamespaceDefaults(sb))
	// Broader (namespace) scope reads first, then the sandbox's own clawk.mod.
	require.Equal(t, []string{"namespace rule", "repo rule"}, sb.Instructions)
	require.Equal(t, "# Team baseline\n\n# Repo baseline", sb.Memory)
}

func TestJoinMemorySkipsEmpty(t *testing.T) {
	require.Equal(t, "a\n\nb", joinMemory("a", "", "  ", "b"))
	require.Equal(t, "", joinMemory("", "   "))
}

// TestNamespaceCreateAndLs exercises the command surface end to end.
func TestNamespaceCreateAndLs(t *testing.T) {
	setupTest(t)
	_, err := executeCommand("namespace", "create", "work")
	require.NoError(t, err, "namespace create")
	out, err := executeCommand("namespace", "ls")
	require.NoError(t, err, "namespace ls")
	require.True(t, strings.Contains(out, "work") && strings.Contains(out, "default"),
		"namespace ls missing entries:\n%s", out)
}
