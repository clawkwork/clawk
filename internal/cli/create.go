package cli

import (
	"fmt"
	"regexp"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/spf13/cobra"
)

var validName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// reservedSandboxNames are CLI verbs and runner names that cannot be
// used as sandbox names. Without this guard, `clawk work claude`
// would create a sandbox named "claude" — and then `clawk run claude
// claude` would resolve `claude` to the runner, not the sandbox,
// making the sandbox un-operable. Reject at create time rather than
// letting the user collide silently.
//
// Kept as a function (not a var) so changes to the agent registry are
// picked up without a restart of any test process.
func reservedSandboxNames() map[string]bool {
	out := map[string]bool{}
	for _, n := range []string{
		// Lifecycle / read verbs
		"new", "up", "down", "destroy", "status", "list",
		// v2 ticket / runner / worktree verbs
		"work", "run", "worktree", "attach",
		// Mutation namespaces
		"branch", "network", "forward", "image", "system", "pr",
		// Diagnostics
		"doctor", "debug",
		// Legacy shims (kept for back-compat; can't recycle as names)
		"here", "phase", "vshell",
		// Internal subprocess re-execs
		"__daemon", "__vzd", "__loop-mount", "__loop-unmount",
	} {
		out[n] = true
	}
	for _, a := range reservedAgentNames() {
		out[a] = true
	}
	return out
}

// validateSandboxName returns an error if name fails any rule. Used by
// every create-path command (clawk create, clawk work) so the
// rules are applied uniformly.
//
// The length cap matches sanitiseName's auto-truncation in run.go;
// names that come in via --name or `clawk create <name>` directly
// don't go through sanitiseName, so we enforce the limit here too.
// macOS sun_path is 104 chars; with the longest socket file in
// vmDir (currently usermode.sock at 13) and a typical home prefix,
// 40 chars of name is the safe upper bound. See the long comment
// on sanitiseName for the full math.
func validateSandboxName(name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf(
			"invalid sandbox name %q: must start with alphanumeric and contain only [a-zA-Z0-9._-]",
			name)
	}
	if len(name) > maxSandboxNameLen {
		return fmt.Errorf(
			"sandbox name %q is %d chars; max %d (macOS Unix-socket path limit) — pick a shorter name or use --name to override",
			name, len(name), maxSandboxNameLen)
	}
	if reservedSandboxNames()[name] {
		return fmt.Errorf(
			"sandbox name %q is reserved (collides with a clawk verb or runner; pick a different name)",
			name)
	}
	return nil
}

func init() {
	rootCmd.AddCommand(createCmd)
}

var createCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a new sandbox",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		if err := validateSandboxName(name); err != nil {
			return err
		}
		if store.Exists(name) {
			return fmt.Errorf("sandbox %q already exists", name)
		}
		// Resolve provider: --provider flag wins, else the host default
		// (vz on macOS, firecracker on Linux).
		prov := config.Provider(providerFlag).Normalize()
		if prov == "" {
			prov = defaultProvider()
		}
		switch prov {
		case config.ProviderVZ, config.ProviderFirecracker:
		default:
			return fmt.Errorf("unknown provider %q (valid: vz, firecracker)", prov)
		}

		// Network left zero: Use nil resolves to the built-in "default"
		// policy at up/reload, so the dev allowlist follows the policy
		// store instead of being frozen into the record at create.
		//
		// Image/Kernel: apply the same --image/--kernel resolution the
		// cwd/ticket paths use. Without this, `clawk create` records no
		// image and the sandbox is unbootable ("no OCI image configured"
		// at up) — finalizeImageRef's default chain gives it the built-in
		// image so a bare `create` + `worktree add` + `up` works.
		sb := &config.Sandbox{
			Name:      name,
			Provider:  prov,
			GuestABI:  sandbox.CurrentGuestABI,
			Namespace: createNamespace(),
			Image:     finalizeImageRef(""),
			Kernel:    finalizeKernelRef(""),
			VMState:   config.VMStateStopped,
			CreatedAt: time.Now(),
		}
		if err := applyNamespaceDefaults(sb); err != nil {
			return err
		}
		if err := store.Save(sb); err != nil {
			return err
		}
		fmt.Printf("Created sandbox %q (namespace: %s, provider: %s)\n", name, sb.NamespaceName(), prov)
		return nil
	},
}
