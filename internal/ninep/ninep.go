// Package ninep serves a host directory to guest VMs over the 9p2000.L
// protocol, as a replacement for Apple's VZVirtioFileSystemDevice for the
// toolchain caches (internal/sandbox.ToolchainCacheShares).
//
// Why this exists: virtio-fs on Apple's Virtualization.framework caches an
// open host file descriptor per inode the guest touches and does not release
// them reliably (Apple FB13640480, still unfixed as of 2026). A sandbox that
// walks a large tree — `find ./vendor`, `go build ./...` over a shared module
// cache — drives the com.apple.Virtualization.VirtualMachine helper's fd count
// past the host's system-wide open-file table (kern.maxfiles). The resulting
// ENFILE turns the next mmap page-in into a SIGBUS ("FS pagein error: 23 Too
// many open files in system"); when the victim is launchd (pid 1) the whole
// Mac panics with "initproc exited" and reboots.
//
// A 9p server is an ordinary host process: it holds descriptors only for fids
// the guest currently has open (not yet clunked), so its descriptor use tracks
// live activity rather than the total file count. Every sandbox connects to a
// Server rooted at the same shared host cache directory, so a module or crate
// downloaded in one sandbox is immediately visible to the others and to the
// host — the cross-sandbox write-back dedup the virtio-fs cache shares gave us,
// without the descriptor blowup.
//
// The transport is deliberately the caller's choice: New returns a Server that
// can serve any net.Listener (Serve) or any single already-accepted connection
// (Handle). The vz provider drives it over a guest→host vsock port (mirroring
// internal/cli/sshagent_proxy_darwin.go); tests drive it over a unix socket.
package ninep

import (
	"fmt"
	"io"
	"net"
	"os"

	"github.com/hugelgupf/p9/fsimpl/localfs"
	"github.com/hugelgupf/p9/p9"
)

// Server exports a single host directory over 9p2000.L. A Server is safe for
// concurrent connections: the underlying p9 server serves each connection on
// its own goroutine, and every connection operates against the same on-disk
// root, which is exactly what makes the cache shared across sandboxes.
type Server struct {
	root string
	srv  *p9.Server
}

// New builds a 9p server rooted at dir. dir must already exist and be a
// directory — the guest mounts it and reads and writes through it live, so a
// missing root is a programming error (the caller creates the cache dir), not
// something to paper over.
func New(dir string) (*Server, error) {
	fi, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("ninep root %s: %w", dir, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("ninep root %s: not a directory", dir)
	}

	return &Server{
		root: dir,
		srv:  p9.NewServer(localfs.Attacher(dir)),
	}, nil
}

// Serve accepts connections on l until l is closed, serving each as an
// independent 9p session against the shared root. It returns whatever error
// closed the accept loop (nil is never returned while l is open).
func (s *Server) Serve(l net.Listener) error {
	return s.srv.Serve(l)
}

// Handle serves a single already-accepted connection — e.g. a vsock
// connection the VZ socket listener handed over — until it closes. It blocks
// for the life of the connection, so callers run it on its own goroutine.
//
// p9's Handle closes both halves it is given; we pass the connection as the
// read half and a non-closing wrapper as the write half so the connection is
// closed exactly once.
func (s *Server) Handle(conn io.ReadWriteCloser) error {
	return s.srv.Handle(conn, nopWriteCloser{conn})
}

// Root reports the host directory this server exports.
func (s *Server) Root() string { return s.root }

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
