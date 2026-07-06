package sandbox

import (
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/guestcfg"
	"github.com/clawkwork/clawk/internal/vsockproto"
	"github.com/stretchr/testify/require"
)

// The guest ABI is defined as the manifest schema and the vsock protocol
// moving together; the compile-time locks in guestabi.go enforce it, this
// test documents it with a readable failure.
func TestGuestABIMatchesComponentVersions(t *testing.T) {
	require.Equal(t, CurrentGuestABI, guestcfg.Version,
		"guestcfg.Version moved without CurrentGuestABI (or vice versa)")
	require.Equal(t, uint32(CurrentGuestABI), vsockproto.ProtoVersion,
		"vsockproto.ProtoVersion moved without CurrentGuestABI (or vice versa)")
	require.LessOrEqual(t, MinSupportedGuestABI, CurrentGuestABI)
}

func TestCheckGuestABI(t *testing.T) {
	// Zero (pre-field records) resolves to ABI 1 and passes today.
	require.NoError(t, CheckGuestABI(&config.Sandbox{Name: "old"}))
	require.Equal(t, 1, RecordedGuestABI(&config.Sandbox{}))

	// The current ABI passes.
	require.NoError(t, CheckGuestABI(&config.Sandbox{Name: "now", GuestABI: CurrentGuestABI}))

	// A sandbox from a NEWER clawk (downgraded host) is refused with
	// recreate guidance rather than failing inside the guest.
	err := CheckGuestABI(&config.Sandbox{Name: "future", GuestABI: CurrentGuestABI + 1})
	require.Error(t, err)
	require.Contains(t, err.Error(), "clawk destroy && clawk")
}
