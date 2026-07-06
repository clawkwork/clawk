//go:build darwin

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/clawkwork/clawk/machine"
)

// SSHAgentVSockPort is the host-side vsock port the in-guest
// ssh-agent forwarder dials when a guest process connects to
// /run/clawk-ssh-agent.sock. Disjoint from clawk-pty-agent's port
// (1024) and clawk-time-sync's port (1025).
const SSHAgentVSockPort uint32 = 1026

// sshAgentProxy listens on a host-side vsock port for guest-initiated
// connections and bidir-pumps bytes to the host's local SSH_AUTH_SOCK.
// Lets git push, ssh-add, etc. inside the VM see the same keys as the
// host (1Password's agent, launchd ssh-agent, ...) without falling
// back to an SSH session that would carry its own -A forwarding.
type sshAgentProxy struct {
	hostSock string
	listener net.Listener
	logger   *log.Logger

	ctx    context.Context
	cancel context.CancelFunc

	wg sync.WaitGroup
}

// startSSHAgentProxy spins up the host-side ssh-agent forwarder. No-op
// (logs a one-line skip and returns nil) when the host has no
// SSH_AUTH_SOCK exported and no 1Password fallback socket on disk —
// there's nothing to proxy in that case, and we don't want guests to
// silently dial a vsock listener that 503s on every accept.
//
// Caller is responsible for Stop() during teardown.
func startSSHAgentProxy(ctx context.Context, m machine.Machine, logger *log.Logger) (*sshAgentProxy, error) {
	hostSock := resolveHostSSHAuthSock()
	if hostSock == "" {
		logger.Printf("ssh-agent-proxy: no SSH_AUTH_SOCK and no 1Password fallback; not starting")
		return nil, nil
	}
	listener, ok := m.(machine.VSockListener)
	if !ok {
		logger.Printf("ssh-agent-proxy: backend doesn't expose VSockListen; not starting")
		return nil, nil
	}

	pCtx, cancel := context.WithCancel(ctx)
	l, err := listener.VSockListen(pCtx, SSHAgentVSockPort)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("vsock listen port=%d: %w", SSHAgentVSockPort, err)
	}

	p := &sshAgentProxy{
		hostSock: hostSock,
		listener: l,
		logger:   logger,
		ctx:      pCtx,
		cancel:   cancel,
	}
	p.wg.Add(1)
	go p.acceptLoop()

	logger.Printf("ssh-agent-proxy: listening on guest vsock port %d -> host %s",
		SSHAgentVSockPort, hostSock)
	return p, nil
}

// Stop closes the listener, waits for in-flight forwards to finish.
// Idempotent.
func (p *sshAgentProxy) Stop() {
	if p == nil {
		return
	}
	p.cancel()
	_ = p.listener.Close()
	p.wg.Wait()
}

func (p *sshAgentProxy) acceptLoop() {
	defer p.wg.Done()
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || p.ctx.Err() != nil {
				return
			}
			p.logger.Printf("ssh-agent-proxy: accept: %v", err)
			return
		}
		p.wg.Add(1)
		go p.handle(conn)
	}
}

func (p *sshAgentProxy) handle(guestConn net.Conn) {
	defer p.wg.Done()
	defer guestConn.Close()

	hostConn, err := net.Dial("unix", p.hostSock)
	if err != nil {
		p.logger.Printf("ssh-agent-proxy: dial host %s: %v", p.hostSock, err)
		return
	}
	defer hostConn.Close()

	// Bidirectional pump. Each direction is a goroutine; the first
	// half-close ends the other via the deferred Close above.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(hostConn, guestConn) }()
	go func() { defer wg.Done(); _, _ = io.Copy(guestConn, hostConn) }()
	wg.Wait()
}

// resolveHostSSHAuthSock picks the host-side socket path the proxy
// will dial on each guest accept. Order:
//
//  1. $CLAWK_SSH_AUTH_SOCK — explicit override. Highest priority
//     so a user with a non-default agent setup can be unambiguous.
//  2. $SSH_AUTH_SOCK if set and NOT the launchd default. macOS's
//     launchd writes /private/tmp/com.apple.launchd.<id>/Listeners
//     into the env of every login session — it points at the system
//     ssh-agent, which is empty unless the user explicitly ran
//     `ssh-add`. We treat that path as "macOS noise, not a user
//     choice" and fall through.
//  3. 1Password's well-known group-container socket. The dominant
//     case on macOS clawk hosts.
//  4. $SSH_AUTH_SOCK if set, even if it looks like launchd's — last
//     resort so an empty launchd agent is at least reachable.
//
// Returns "" when nothing is available; the proxy treats that as
// "nothing to forward" and skips startup with one log line.
func resolveHostSSHAuthSock() string {
	if s := os.Getenv("CLAWK_SSH_AUTH_SOCK"); s != "" {
		if _, err := os.Stat(s); err == nil {
			return s
		}
	}
	envSock := os.Getenv("SSH_AUTH_SOCK")
	if envSock != "" && !looksLikeLaunchdSSHAgent(envSock) {
		if _, err := os.Stat(envSock); err == nil {
			return envSock
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		op := filepath.Join(home,
			"Library/Group Containers/2BUA8C4S2C.com.1password/t/agent.sock")
		if _, err := os.Stat(op); err == nil {
			return op
		}
	}
	if envSock != "" {
		if _, err := os.Stat(envSock); err == nil {
			return envSock
		}
	}
	return ""
}

// looksLikeLaunchdSSHAgent matches the path shape macOS uses for its
// default per-session ssh-agent socket. We deprioritise these paths
// because the launchd-managed agent is empty unless the user
// specifically ran `ssh-add` — in practice it's almost never the
// agent the user wants forwarded into a sandbox.
func looksLikeLaunchdSSHAgent(p string) bool {
	return strings.HasPrefix(p, "/private/tmp/com.apple.launchd.") ||
		strings.Contains(p, "/com.apple.launchd.") || // /var/run/com.apple.launchd.*/Listeners
		strings.HasPrefix(p, "/var/folders/")
}
