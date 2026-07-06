package cli

import (
	"fmt"
	"io"

	"github.com/clawkwork/clawk/internal/config"
)

// observe refreshes a sandbox's VMState from its provider's live status and
// persists any change. The provider/OS is the source of truth for runtime
// state; the persisted VMState is a reconciled cache — never trust it for a
// decision without observing first. Returns the live status string, falling
// back to the cached state if the provider is unavailable on this host.
func observe(sb *config.Sandbox) string {
	prov, err := providerFor(sb)
	if err != nil {
		return string(sb.VMState)
	}
	status, err := prov.Status(sb)
	if err != nil {
		return string(sb.VMState)
	}
	want := config.VMStateStopped
	if isRunning(status) {
		want = config.VMStateRunning
	}
	if sb.VMState != want {
		sb.VMState = want
		_ = store.Save(sb)
	}
	return status
}

// reconcileVMStates observes every sandbox, healing stale VMState records (a
// crashed or out-of-band-stopped VM still recorded as running). The migration's
// running-gate reads VMState, so reconciling keeps it accurate.
func reconcileVMStates(w io.Writer) {
	list, err := store.List()
	if err != nil {
		return
	}
	for i := range list {
		sb := &list[i]
		before := sb.VMState
		observe(sb)
		if sb.VMState != before {
			fmt.Fprintf(w, "clawk: reconciled %q state -> %s\n", sb.DisplayName(), sb.VMState)
		}
	}
}
