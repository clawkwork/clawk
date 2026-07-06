package sandbox

// Networking constants for the gvproxy virtual network.
//
// Each sandbox gets its own gvproxy instance with these addresses and
// reaches the outside world only through the gateway, which is our
// userspace TCP/IP stack. Sandboxes have no DHCP client, so clawk-init
// configures these values statically from the guest manifest (see
// OCIGuestManifest).
//
// Platform-neutral on purpose: the manifest builder runs (and is tested)
// on any OS even though the vz provider is darwin-only.
const (
	gvproxyMTU     = 1500
	gvproxyGateway = "192.168.127.1"
	guestIP        = "192.168.127.2"
)
