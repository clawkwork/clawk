//go:build darwin || linux

package usermode

import (
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// darwinSunPathMax is the macOS sun_path capacity (sizeof sa.Path). Go's
// socket layer rejects bind/dial when len(name) >= this, so a bound path
// must stay strictly below it on darwin. We assert the rendezvous paths
// respect this even on linux (where the real limit is higher) so the
// invariant is caught on either build.
const darwinSunPathMax = 104

// TestRendezvousDir confirms the derived bind directory stays short enough
// for the macOS sun_path limit even when the caller's SockPath is absurdly
// long, and that it is deterministic per key and distinct across keys.
func TestRendezvousDir(t *testing.T) {
	long := "/Users/some-very-long-username/.clawk/namespaces/default/vms/" +
		strings.Repeat("long-namespace-path-segment-", 8) + "/usermode.sock"

	dir := rendezvousDir(long)
	// Longest socket bound under dir is "host.sock" (9 bytes + separator).
	got := len(filepath.Join(dir, "host.sock"))
	require.Less(t, got, darwinSunPathMax, "bound path length = %d, want < %d (%q)", got, darwinSunPathMax, dir)
	require.Equal(t, dir, rendezvousDir(long), "rendezvousDir is not deterministic for the same key")
	require.NotEqual(t, dir, rendezvousDir(long+"x"), "distinct keys produced the same rendezvous dir")
}

// TestNewUnixgramPair confirms the paired sockets bind under bindDir and
// round-trip a datagram from the VM side to the host side.
func TestNewUnixgramPair(t *testing.T) {
	bindDir := filepath.Join(t.TempDir(), "rv")

	vmFile, host, err := newUnixgramPair(bindDir)
	require.NoError(t, err, "newUnixgramPair")
	t.Cleanup(func() { host.Close() })
	t.Cleanup(func() { vmFile.Close() })

	vm, err := net.FileConn(vmFile)
	require.NoError(t, err, "FileConn")
	t.Cleanup(func() { vm.Close() })

	_, err = vm.Write([]byte("hello"))
	require.NoError(t, err, "vm write")
	conn, err := acceptVfkit(host)
	require.NoError(t, err, "acceptVfkit")
	want := filepath.Join(bindDir, "vm.sock")
	require.Equal(t, want, conn.RemoteAddr().String(), "peer address mismatch")
}

// TestPeekPeerAddr confirms acceptVfkit learns the bound peer path of a
// sender via MSG_PEEK|MSG_TRUNC. Runs on darwin and linux — this is the
// portability claim the Linux build relies on.
func TestPeekPeerAddr(t *testing.T) {
	dir := t.TempDir()
	listenerPath := filepath.Join(dir, "host.sock")
	senderPath := filepath.Join(dir, "guest.sock")

	listener, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: listenerPath, Net: "unixgram"})
	require.NoError(t, err, "listen")
	t.Cleanup(func() { listener.Close() })

	sender, err := net.DialUnix("unixgram",
		&net.UnixAddr{Name: senderPath, Net: "unixgram"},
		&net.UnixAddr{Name: listenerPath, Net: "unixgram"},
	)
	require.NoError(t, err, "dial")
	t.Cleanup(func() { sender.Close() })

	// Sender emits one datagram so the listener has something to peek at.
	_, err = sender.Write([]byte("hello"))
	require.NoError(t, err, "write")

	conn, err := acceptVfkit(listener)
	require.NoError(t, err, "acceptVfkit")
	require.Equal(t, senderPath, conn.RemoteAddr().String(), "RemoteAddr mismatch")

	// Round-trip: write back to the sender and confirm it arrives.
	_, err = conn.Write([]byte("pong"))
	require.NoError(t, err, "conn write")
	buf := make([]byte, 16)
	err = sender.SetReadDeadline(deadlineShortlyFromNow())
	require.NoError(t, err, "deadline")
	n, err := sender.Read(buf)
	require.NoError(t, err, "sender read")
	require.Equal(t, "pong", string(buf[:n]), "sender got wrong data")
}
