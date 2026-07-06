//go:build darwin || linux

// Package usermode runs an in-process gvisor-tap-vsock stack wired to a
// unixgram NIC. Backends that want a filtered userspace TCP/IP stack —
// without requiring root or a TAP device — pair one of these with their
// hypervisor's unixgram NIC option (Code-Hex/vz
// NewFileHandleNetworkDeviceAttachment, qemu -netdev stream/...).
//
// The package works on macOS and Linux; the only platform-specific piece
// is the MSG_PEEK-based peer-address discovery, which we implement here
// rather than depending on gvproxy's transport package (which gates the
// equivalent behind //go:build darwin upstream).
package usermode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"

	"github.com/clawkwork/clawk/machine"
	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"
)

// Default addresses gvproxy assigns. The MACs are fixed so the DHCP static
// lease on the gvproxy side always matches the NIC MAC we configure on the
// hypervisor side — a MAC drift produces a dynamic lease on a different IP,
// which silently breaks every port forward.
const (
	defaultMTU        = 1500
	defaultSubnet     = "192.168.127.0/24"
	defaultGatewayIP  = "192.168.127.1"
	defaultGatewayMAC = "5a:94:ef:e4:0c:ee"
	defaultGuestIP    = "192.168.127.2"
	defaultGuestMAC   = "5a:94:ef:e4:0c:dd"

	// macOS ships unix-socket buffers in the single-digit KB by default.
	// Fine for DHCP-sized frames, silently drops fragmented TCP once the
	// guest does real work. We bump them here — larger TCP segments (TLS
	// handshakes, package downloads) stall otherwise.
	socketBufBytes = 4 * 1024 * 1024
)

// Config configures a UserMode stack.
type Config struct {
	// SockPath identifies this stack's rendezvous; it must be unique per
	// VM (the caller's per-VM state path is the natural choice). The host
	// and VM unixgram sockets are NOT bound at SockPath itself — they live
	// under a short /tmp directory derived from it (see rendezvousDir),
	// because SockPath sits deep under ~/.clawk/namespaces/<ns>/vms/<name>/
	// and a long sandbox name there overflows the macOS sun_path limit. The
	// *os.File wrapping the VM socket's fd is exposed on Stack.VMSocket for
	// the hypervisor to attach as its NIC.
	SockPath string

	// Forwards are 127.0.0.1:<HostPort> → guest:<GuestPort> TCP forwards
	// exposed by gvproxy. PortForward.GuestIP is ignored; gvproxy routes
	// to the configured guest IP.
	Forwards []machine.PortForward

	// Filter, if non-nil, gates the stack's outbound TCP/UDP/ICMP flows and
	// observes DNS answers for the lifetime of the Stack. It is threaded
	// into the gvproxy Configuration (not a process global), so multiple
	// Stacks can run concurrently with independent filters.
	Filter machine.Filter
}

// Stack is a running UserMode network. A backend obtains one via Start,
// passes VMSocket to its hypervisor, and invokes Serve from a goroutine
// until its VM stops.
type Stack struct {
	// VMSocket is the VM-side unixgram fd. The hypervisor takes ownership;
	// Close does not close it.
	VMSocket *os.File

	// GuestIP is the IP gvproxy's DHCP server assigns to the guest. Ports
	// in Config.Forwards are forwarded to this IP.
	GuestIP string

	// GuestMAC is the MAC the backend must configure on its NIC to match
	// gvproxy's static DHCP lease.
	GuestMAC string

	host    *net.UnixConn
	vn      *virtualnetwork.VirtualNetwork
	cfg     Config
	bindDir string
}

// Start creates the unixgram pair, builds the gvproxy configuration, and
// installs filter globals. It does not block on guest traffic; call Serve
// after the hypervisor is running.
//
// Start binds the rendezvous sockets under a short directory derived from
// cfg.SockPath, removing any stale sockets there first.
func Start(cfg Config) (_ *Stack, err error) {
	if cfg.SockPath == "" {
		return nil, errors.New("usermode: Config.SockPath is required")
	}

	bindDir := rendezvousDir(cfg.SockPath)
	vmSocket, host, err := newUnixgramPair(bindDir)
	if err != nil {
		return nil, fmt.Errorf("usermode: wiring unixgram pair: %w", err)
	}
	defer func() {
		if err != nil {
			vmSocket.Close()
			host.Close()
		}
	}()

	vn, err := virtualnetwork.New(buildGvproxyConfig(cfg), filterOptions(cfg.Filter)...)
	if err != nil {
		return nil, fmt.Errorf("usermode: creating virtual network: %w", err)
	}

	return &Stack{
		VMSocket: vmSocket,
		GuestIP:  defaultGuestIP,
		GuestMAC: defaultGuestMAC,
		host:     host,
		vn:       vn,
		cfg:      cfg,
		bindDir:  bindDir,
	}, nil
}

// Serve peers with the guest and forwards packets until ctx is done. It
// blocks. Callers typically run Serve in a goroutine bound to the VM's
// lifecycle context.
//
// If the guest never boots, Serve blocks in AcceptVfkit until ctx is
// cancelled; cancellation closes the listening socket to unblock the read.
func (s *Stack) Serve(ctx context.Context) error {
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			// Close unblocks an in-flight ReadFromUnix in AcceptVfkit.
			_ = s.host.Close()
		case <-done:
		}
	}()

	peered, err := acceptVfkit(s.host)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("usermode: accepting guest: %w", err)
	}
	if err := s.vn.AcceptVfkit(ctx, peered); err != nil && ctx.Err() == nil {
		return fmt.Errorf("usermode: forwarding packets: %w", err)
	}
	return nil
}

// acceptVfkit blocks until the guest sends its first datagram, uses
// MSG_PEEK to learn the guest's bound peer path, and returns a net.Conn
// that writes datagrams back to that peer. Portable across darwin and
// linux — both expose SockaddrUnix and MSG_PEEK via package syscall.
//
// Ported from gvisor-tap-vsock's darwin-only transport.AcceptVfkit so we
// can use it on linux too.
func acceptVfkit(listener *net.UnixConn) (net.Conn, error) {
	peerAddr, err := peekPeerAddr(listener)
	if err != nil {
		return nil, err
	}
	return &connectedUnixgram{UnixConn: listener, peer: peerAddr}, nil
}

// peekPeerAddr uses a one-shot Recvfrom with MSG_PEEK|MSG_TRUNC to learn
// the sender's bound socket path without consuming the datagram. The guest's
// first packet may carry the four-byte "VFKT" handshake; if so we drain it
// since gvproxy's own parser doesn't expect it.
func peekPeerAddr(listener *net.UnixConn) (*net.UnixAddr, error) {
	raw, err := listener.SyscallConn()
	if err != nil {
		return nil, err
	}
	var (
		peer    syscall.Sockaddr
		peekErr error
	)
	magic := make([]byte, 4)
	peek := func(fd uintptr) bool {
		_, peer, peekErr = syscall.Recvfrom(int(fd), magic, syscall.MSG_PEEK|syscall.MSG_TRUNC)
		return !errors.Is(peekErr, syscall.EAGAIN)
	}
	if err := raw.Read(peek); err != nil {
		return nil, err
	}
	if peekErr != nil {
		return nil, peekErr
	}
	// Consume the legacy handshake if present so gvproxy sees a clean frame next.
	if bytes.Equal(magic, []byte("VFKT")) {
		if _, _, err := listener.ReadFrom(magic); err != nil {
			return nil, err
		}
	}
	unixPeer, ok := peer.(*syscall.SockaddrUnix)
	if !ok {
		return nil, fmt.Errorf("usermode: unexpected peer address %T", peer)
	}
	if unixPeer.Name == "" {
		return nil, fmt.Errorf("usermode: peer address is empty (guest NIC unbound?)")
	}
	return &net.UnixAddr{Name: unixPeer.Name, Net: "unixgram"}, nil
}

// connectedUnixgram wraps a bound unixgram listener with a fixed peer so
// Write goes to the learned guest address. gvproxy's VirtualNetwork.AcceptVfkit
// calls Write via the returned net.Conn on every outbound frame.
type connectedUnixgram struct {
	*net.UnixConn
	peer *net.UnixAddr
}

func (c *connectedUnixgram) RemoteAddr() net.Addr { return c.peer }

func (c *connectedUnixgram) Write(p []byte) (int, error) {
	return c.UnixConn.WriteTo(p, c.peer)
}

// Close tears down the virtual network. Safe to call more than once.
//
// VMSocket is not closed here; the hypervisor owns it after Start returns.
func (s *Stack) Close() error {
	// vn has no explicit Close; cancelling Serve's ctx is the shutdown
	// signal. Drop the bound listener, then remove the rendezvous dir —
	// it lives under /tmp, outside the caller's state dir, so it would
	// otherwise leak past the VM's lifetime. Unlinking vm.sock here is
	// safe: the hypervisor holds an open fd, and the guest has stopped by
	// the time Close runs.
	err := s.host.Close()
	if s.bindDir != "" {
		if rmErr := os.RemoveAll(s.bindDir); rmErr != nil && err == nil {
			err = fmt.Errorf("removing rendezvous dir: %w", rmErr)
		}
	}
	return err
}

// rendezvousDir returns a short, per-VM directory for the unixgram
// rendezvous sockets. key (the caller's SockPath) sits under
// ~/.clawk/namespaces/<ns>/vms/<name>/, which a long sandbox name can push
// past the macOS sun_path limit (104 bytes) when bound as a socket address.
// Binding under /tmp instead keeps the path short and bounded regardless of
// the caller's layout; the hash keeps it stable across restarts of the same
// VM and distinct between VMs (and between users, since key embeds $HOME).
func rendezvousDir(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join("/tmp", "clawk-net-"+hex.EncodeToString(sum[:5]))
}

// newUnixgramPair creates a paired SOCK_DGRAM AF_UNIX rendezvous under
// bindDir: host.sock (host) and vm.sock (VM). Both sockets must be bound to
// named paths rather than using socketpair: gvproxy's vfkit transport learns
// the VM's peer address from the source of the first datagram (via MSG_PEEK)
// so it can route replies back, and it rejects anonymous peers ("vfkit socket
// address is empty"). A socketpair endpoint has no named peer, so replies
// would have nowhere to go.
//
// bindDir is kept short (see rendezvousDir) so the bound paths stay within
// the macOS sun_path limit; the caller owns removing it once the VM stops.
func newUnixgramPair(bindDir string) (*os.File, *net.UnixConn, error) {
	if err := os.MkdirAll(bindDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("creating rendezvous dir %s: %w", bindDir, err)
	}
	hostPath := filepath.Join(bindDir, "host.sock")
	vmPath := filepath.Join(bindDir, "vm.sock")

	if err := removeStale(hostPath); err != nil {
		return nil, nil, err
	}
	hostAddr := &net.UnixAddr{Name: hostPath, Net: "unixgram"}
	host, err := net.ListenUnixgram("unixgram", hostAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("listening on %s: %w", hostPath, err)
	}
	if err := bumpBuffers(host, socketBufBytes); err != nil {
		host.Close()
		return nil, nil, fmt.Errorf("sizing host socket: %w", err)
	}

	if err := removeStale(vmPath); err != nil {
		host.Close()
		return nil, nil, err
	}
	vzLocal := &net.UnixAddr{Name: vmPath, Net: "unixgram"}
	vzRemote := &net.UnixAddr{Name: hostPath, Net: "unixgram"}
	vz, err := net.DialUnix("unixgram", vzLocal, vzRemote)
	if err != nil {
		host.Close()
		return nil, nil, fmt.Errorf("dialing %s from %s: %w", hostPath, vmPath, err)
	}
	if err := bumpBuffers(vz, socketBufBytes); err != nil {
		vz.Close()
		host.Close()
		return nil, nil, fmt.Errorf("sizing vm socket: %w", err)
	}

	// File() dup's the fd; release our *net.UnixConn wrapper after so the
	// hypervisor's dup becomes the sole owner from our side.
	f, err := vz.File()
	vz.Close()
	if err != nil {
		host.Close()
		return nil, nil, fmt.Errorf("dup'ing vm fd: %w", err)
	}
	return f, host, nil
}

func removeStale(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing stale socket %s: %w", path, err)
	}
	return nil
}

// bumpBuffers raises SO_SNDBUF/SO_RCVBUF. The kernel silently caps below
// what we ask (kern.ipc.maxsockbuf); that's fine — we want as much as
// allowed, not a precise size.
func bumpBuffers(c *net.UnixConn, bytes int) error {
	raw, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var setErr error
	ctrlErr := raw.Control(func(fd uintptr) {
		if e := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF, bytes); e != nil {
			setErr = e
			return
		}
		if e := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, bytes); e != nil {
			setErr = e
		}
	})
	if ctrlErr != nil {
		return ctrlErr
	}
	return setErr
}

func buildGvproxyConfig(cfg Config) *types.Configuration {
	forwards := make(map[string]string, len(cfg.Forwards))
	for _, f := range cfg.Forwards {
		forwards[fmt.Sprintf("127.0.0.1:%d", f.HostPort)] =
			fmt.Sprintf("%s:%d", defaultGuestIP, f.GuestPort)
	}
	return &types.Configuration{
		MTU:               defaultMTU,
		Subnet:            defaultSubnet,
		GatewayIP:         defaultGatewayIP,
		GatewayMacAddress: defaultGatewayMAC,
		DHCPStaticLeases: map[string]string{
			defaultGuestIP: defaultGuestMAC,
		},
		Forwards: forwards,
		NAT: map[string]string{
			// host.containers.internal → real host loopback, mirroring
			// Docker/podman convention so guest code can reach host
			// services at a stable address.
			defaultGatewayIP: "127.0.0.1",
		},
	}
}

// filterOptions translates a machine.Filter into the gvproxy egress hooks.
// A nil filter yields no options, leaving the stack unfiltered.
func filterOptions(f machine.Filter) []virtualnetwork.Option {
	if f == nil {
		return nil
	}
	return []virtualnetwork.Option{
		virtualnetwork.WithTCPFilter(f.AllowTCP),
		virtualnetwork.WithUDPFilter(f.AllowUDP),
		virtualnetwork.WithICMPFilter(f.AllowICMP),
		virtualnetwork.WithDNSObserver(f.ObserveDNS),
	}
}
