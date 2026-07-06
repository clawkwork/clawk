//go:build linux

package cli

import (
	"fmt"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/sandbox"
)

// newProvider maps a Provider identifier to a constructed provider
// for this host's OS. Identifiers that are valid on the type but not on
// this OS are rejected with a clear "platform" error rather than a
// runtime-only "unsupported" surface from a stub method.
func newProvider(p config.Provider) (sandbox.Provider, error) {
	switch p {
	case config.ProviderFirecracker:
		return sandbox.NewFirecrackerProvider(store), nil
	case config.ProviderVZ:
		return nil, fmt.Errorf("provider %q runs on macOS; this is a Linux build", p)
	default:
		return nil, fmt.Errorf("unknown provider %q (valid on Linux: firecracker)", p)
	}
}

// defaultProvider is the provider used for new sandboxes when neither
// --provider nor a clawk.mod selects one. vz cannot run on Linux, so the
// only sensible default here is firecracker.
func defaultProvider() config.Provider { return config.ProviderFirecracker }
