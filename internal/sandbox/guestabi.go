package sandbox

import (
	"fmt"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/guestcfg"
	"github.com/clawkwork/clawk/internal/vsockproto"
)

// CurrentGuestABI is the version of the guest-side contract baked into a
// sandbox's disk at create time. Two things move together under this one
// number: the clawk-init boot manifest schema (guestcfg.Version) and the
// pty-agent vsock protocol (vsockproto.ProtoVersion). It is recorded on
// the sandbox record (config.Sandbox.GuestABI) so a later host binary can
// tell — without booting anything — whether it still speaks this
// sandbox's dialect.
const CurrentGuestABI = 1

// MinSupportedGuestABI is the oldest recorded guest ABI this host binary
// still boots and attaches to. Raising it turns every sandbox created
// before the new floor into a "recreate me" error, so raise it only
// alongside an actual manifest/protocol break — and say so in the
// changelog.
const MinSupportedGuestABI = 1

// Compile-time locks: the ABI is exactly these two versions moving
// together. If either bumps without CurrentGuestABI (or vice versa), the
// index goes out of range and the build fails — fix all three, then
// revisit MinSupportedGuestABI and the guest sources in
// internal/agentembed which mirror them.
var (
	_ = [1]struct{}{}[CurrentGuestABI-guestcfg.Version]
	_ = [1]struct{}{}[CurrentGuestABI-int(vsockproto.ProtoVersion)]
)

// RecordedGuestABI is sb's guest ABI, resolving the zero value (records
// written before the field existed) to ABI 1.
func RecordedGuestABI(sb *config.Sandbox) int {
	if sb.GuestABI == 0 {
		return 1
	}
	return sb.GuestABI
}

// CheckGuestABI refuses, with recreate guidance, when the guest binaries
// baked into sb's disk are outside what this host supports. Called before
// boot and before attach: it is the readable version of the failure the
// guest itself would otherwise produce mid-boot (clawk-init manifest
// check) or mid-attach (pty-agent handshake check).
func CheckGuestABI(sb *config.Sandbox) error {
	abi := RecordedGuestABI(sb)
	if abi < MinSupportedGuestABI || abi > CurrentGuestABI {
		return fmt.Errorf(
			"sandbox %q has guest ABI v%d baked into its disk; this clawk supports v%d–v%d — recreate it with 'clawk destroy && clawk' (your repo and agent conversations survive; the VM disk does not)",
			sb.DisplayName(), abi, MinSupportedGuestABI, CurrentGuestABI)
	}
	return nil
}
