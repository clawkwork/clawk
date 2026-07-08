//go:build darwin

package cli

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"sync"

	"github.com/clawkwork/clawk/internal/ninep"
	"github.com/clawkwork/clawk/machine"
)

// ninepServer serves a host directory to the guest over 9p2000.L on a vsock
// port. It's the host half of the cache transport that replaces virtio-fs for
// the toolchain caches: a 9p-capable clawk-init mounts it with `-t 9p -o
// trans=fd` over an AF_VSOCK socket, so the caches stay shared and write-back
// across sandboxes without Apple's virtio-fs pinning a host fd per inode (which
// exhausts kern.maxfiles and panics the Mac — see internal/ninep). One server
// runs per toolchain cache, each rooted at the cache's host dir on the vsock
// port from sandbox.ToolchainCacheShares (NinepBasePort+index), so host servers
// and guest mounts agree on port↔dir.
type ninepServer struct {
	srv      *ninep.Server
	listener net.Listener
	logger   *log.Logger

	ctx    context.Context
	cancel context.CancelFunc

	wg sync.WaitGroup
}

// startNinepServer serves dir over 9p on guest vsock port. Like the ssh-agent
// proxy it's best-effort: a backend without VSockListen, or a missing cache
// dir, logs one line and returns (nil, nil) rather than failing the VM boot.
// Caller Stop()s during teardown.
func startNinepServer(ctx context.Context, m machine.Machine, dir string, port uint32, logger *log.Logger) (*ninepServer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logger.Printf("ninep: cannot create cache dir %s (%v); not starting", dir, err)
		return nil, nil
	}
	srv, err := ninep.New(dir)
	if err != nil {
		return nil, fmt.Errorf("ninep server: %w", err)
	}
	listener, ok := m.(machine.VSockListener)
	if !ok {
		logger.Printf("ninep: backend doesn't expose VSockListen; not starting")
		return nil, nil
	}

	pCtx, cancel := context.WithCancel(ctx)
	l, err := listener.VSockListen(pCtx, port)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("vsock listen port=%d: %w", port, err)
	}

	n := &ninepServer{srv: srv, listener: l, logger: logger, ctx: pCtx, cancel: cancel}
	n.wg.Add(1)
	go n.acceptLoop()

	logger.Printf("ninep: serving %s on guest vsock port %d", dir, port)
	return n, nil
}

// Stop closes the listener and waits for in-flight sessions. Idempotent.
func (n *ninepServer) Stop() {
	if n == nil {
		return
	}
	n.cancel()
	_ = n.listener.Close()
	n.wg.Wait()
}

func (n *ninepServer) acceptLoop() {
	defer n.wg.Done()
	for {
		conn, err := n.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || n.ctx.Err() != nil {
				return
			}
			n.logger.Printf("ninep: accept: %v", err)
			return
		}
		n.wg.Add(1)
		go func() {
			defer n.wg.Done()
			defer conn.Close()
			// Isolate the session: a panic serving one guest must not take
			// down vzd (and with it every other sandbox's VM).
			defer func() {
				if r := recover(); r != nil {
					n.logger.Printf("ninep: session panic recovered: %v", r)
				}
			}()
			// Handle blocks for the life of the 9p session. A closed
			// connection or shutdown is expected, not worth logging.
			if err := n.srv.Handle(conn); err != nil && n.ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				n.logger.Printf("ninep: session: %v", err)
			}
		}()
	}
}
