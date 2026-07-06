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
	"sync"
	"sync/atomic"
	"time"

	"github.com/clawkwork/clawk/internal/vsockproto"
	"github.com/clawkwork/clawk/machine"
)

// Tiny atomic helpers so the active-session counter doesn't pull in a
// dependency just for this. atomic.Int64 (Go 1.19+) would also work
// but the wrapper functions read more clearly at call sites.
//
// atomicAddInt64 returns the post-increment value (Go's Add returns
// the new value); we use it to detect the exact moment we cross
// wedgeThreshold so a single log line fires per wedge episode.
func atomicLoadInt64(p *int64) int64         { return atomic.LoadInt64(p) }
func atomicAddInt64(p *int64, n int64) int64 { return atomic.AddInt64(p, n) }
func atomicStoreInt64(p *int64, v int64)     { atomic.StoreInt64(p, v) }

// agentProxy bridges a host-side Unix domain socket to the in-guest
// AF_VSOCK listener exposed by clawk-pty-agent. On the Mac side it is
// a normal `net.Dial("unix", path)` target; vzd accepts each
// connection and dials guest vsock port AgentVSockPort via the vz
// backend's Machine.VSock(), then shovels bytes between the two.
//
// Why proxy instead of letting the client dial vsock directly:
// Machine.VSock() lives inside the daemon process — it's a method on
// the live *codevz.VirtualMachine. The client (clawk claude) is a
// separate short-lived process that has no handle on the VM. The
// daemon already runs forever and owns the VM, so it's the natural
// place to expose a connection broker.
//
// The proxy speaks no protocol of its own. It pipes bytes in both
// directions and lets the framed vsockproto messages flow end-to-end
// between the Mac client and the guest agent.
type agentProxy struct {
	sockPath string
	machine  machine.Machine
	logger   *log.Logger

	ctx    context.Context
	cancel context.CancelFunc

	wg sync.WaitGroup

	// mu guards listener/suspended/acceptDone across the accept loop,
	// Stop, and the idle watchdog's SuspendAccept/ResumeAccept pair.
	mu         sync.Mutex
	listener   net.Listener
	suspended  bool
	acceptDone chan struct{} // closed when the current accept loop exits

	// active counts in-flight client sessions. Other goroutines (e.g.
	// the wallclock-watchdog) check this to avoid bouncing the VM
	// while a user is mid-claude — a bounce kills the live vsock
	// connection and surfaces as "agent disconnected before exit
	// frame: EOF" on the user's screen.
	//
	// The increment happens in the accept loop, not in handle(): after
	// SuspendAccept has both closed the listener and seen the loop
	// exit, every accepted connection is guaranteed to be counted, so
	// "suspended && active == 0" really means no client anywhere.
	active int64

	// lastSession is the UnixNano of the most recent session start or
	// end. The idle watchdog uses it to catch sessions that began and
	// ended entirely between two of its ticks.
	lastSession int64

	// dialFailCount is the count of consecutive vsock dial failures.
	// Reset to 0 on any successful dial. When it crosses
	// wedgeThreshold the proxy logs a single "guest appears wedged"
	// breadcrumb and sets the wedged sentinel — used by clawk
	// doctor and the user-facing claude-session error.
	dialFailCount int64
	wedged        int64 // 0 = healthy, 1 = wedged (atomic)
}

// wedgeThreshold is the number of consecutive vsock dial failures
// (each ~10s timeout) before we declare the guest wedged. Three is
// enough to absorb transient post-wake recovery while still detecting
// the kernel-frozen case quickly: 3 × 10s ≈ 30s after the kernel
// died, the sentinel flips and the user gets clear guidance.
const wedgeThreshold = 3

// IsWedged reports whether recent vsock dials suggest the guest is
// frozen. Cleared automatically when the next dial succeeds. Used by
// clawk doctor and the failure-fallback messaging.
func (p *agentProxy) IsWedged() bool {
	return atomicLoadInt64(&p.wedged) != 0
}

// HasActiveSessions reports whether the proxy currently has any
// bridged connections open. Used by the wallclock-watchdog to skip
// the VM-bounce step when a user is actively connected — clock skew
// on a paused vCPU is fixed by clawk-time-sync's next push anyway,
// and we don't need to nuke the session in the meantime.
func (p *agentProxy) HasActiveSessions() bool {
	return atomicLoadInt64(&p.active) > 0
}

// LastSessionNano returns the UnixNano timestamp of the most recent
// session start or end observed by the proxy, 0 if none since boot.
func (p *agentProxy) LastSessionNano() int64 {
	return atomicLoadInt64(&p.lastSession)
}

// startAgentProxy creates the host-side Unix socket and starts an
// accept loop. Errors here are fatal for the daemon — without the proxy
// the host has no path to the in-guest agent, so failing loudly here
// beats losing it silently.
func startAgentProxy(parentCtx context.Context, m machine.Machine, sockPath string, logger *log.Logger) (*agentProxy, error) {
	// Clean up any stale socket from a previous (crashed) daemon run.
	// net.Listen("unix") with a pre-existing path fails with
	// EADDRINUSE — much less helpful than a generic "I removed the
	// stale file" log line.
	if err := os.Remove(sockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("removing stale agent sock: %w", err)
	}
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", sockPath, err)
	}
	// 0666 so the user account that ran clawk can connect even if
	// the daemon's umask masked group/other access. This socket only
	// exposes a path INTO the user's own VM — same security boundary
	// as their own processes.
	if err := os.Chmod(sockPath, 0o666); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("chmod agent sock: %w", err)
	}

	ctx, cancel := context.WithCancel(parentCtx)
	p := &agentProxy{
		sockPath: sockPath,
		listener: l,
		machine:  m,
		logger:   logger,
		ctx:      ctx,
		cancel:   cancel,
	}
	p.acceptDone = make(chan struct{})
	p.wg.Add(1)
	go p.acceptLoop(l, p.acceptDone)
	logger.Printf("agent-proxy: listening on %s → guest vsock port %d", sockPath, vsockproto.AgentVSockPort)
	return p, nil
}

// Stop closes the listener, cancels in-flight handlers, and waits for
// goroutines to drain. Idempotent.
func (p *agentProxy) Stop() {
	p.cancel()
	p.mu.Lock()
	_ = p.listener.Close()
	p.mu.Unlock()
	p.wg.Wait()
	_ = os.Remove(p.sockPath)
}

// SuspendAccept stops accepting new sessions: it closes the listener,
// removes the socket (so a client's next probe reports "not there" and
// falls through its recovery path rather than hanging), and waits for the
// accept loop to exit. In-flight sessions are untouched. After it returns,
// HasActiveSessions is authoritative — no uncounted connection can exist —
// which is exactly the fence the idle watchdog needs before stopping the
// VM. Undone by ResumeAccept; no-op when already suspended.
func (p *agentProxy) SuspendAccept() {
	p.mu.Lock()
	if p.suspended {
		p.mu.Unlock()
		return
	}
	p.suspended = true
	_ = p.listener.Close()
	_ = os.Remove(p.sockPath)
	done := p.acceptDone
	p.mu.Unlock()
	<-done
}

// ResumeAccept re-opens the socket and restarts the accept loop after a
// SuspendAccept whose stop was aborted (a session raced in). No-op when
// not suspended.
func (p *agentProxy) ResumeAccept() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.suspended {
		return nil
	}
	_ = os.Remove(p.sockPath)
	l, err := net.Listen("unix", p.sockPath)
	if err != nil {
		return fmt.Errorf("relistening on %s: %w", p.sockPath, err)
	}
	// Same rationale as startAgentProxy: the socket only exposes a path
	// into the user's own VM.
	if err := os.Chmod(p.sockPath, 0o666); err != nil {
		_ = l.Close()
		return fmt.Errorf("chmod agent sock: %w", err)
	}
	p.listener = l
	p.suspended = false
	p.acceptDone = make(chan struct{})
	p.wg.Add(1)
	go p.acceptLoop(l, p.acceptDone)
	p.logger.Printf("agent-proxy: accept resumed on %s", p.sockPath)
	return nil
}

func (p *agentProxy) acceptLoop(l net.Listener, done chan struct{}) {
	defer p.wg.Done()
	defer close(done)
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// Transient accept errors (e.g., EMFILE) shouldn't bring
			// down the proxy. Log and try again after a short backoff
			// so we don't spin if the kernel keeps refusing.
			p.logger.Printf("agent-proxy accept: %v", err)
			select {
			case <-p.ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		// Count the session here, not in handle(): see the field docs on
		// `active` for why the accept loop must own the increment.
		atomicAddInt64(&p.active, 1)
		atomicStoreInt64(&p.lastSession, time.Now().UnixNano())
		p.wg.Add(1)
		go p.handle(c)
	}
}

// handle dials the guest vsock and bridges bytes to/from the Unix
// socket the client connected on. Either side closing tears the
// other down — bidirectional close is mandatory or the client hangs
// waiting for an EOF that will never come.
func (p *agentProxy) handle(client net.Conn) {
	defer p.wg.Done()
	defer client.Close()

	// The accept loop already counted this session; release it (and stamp
	// the end time for the idle watchdog) when the pumps return.
	defer func() {
		atomicStoreInt64(&p.lastSession, time.Now().UnixNano())
		atomicAddInt64(&p.active, -1)
	}()

	// Log every accept up-front so operators can confirm the vsock path
	// is being exercised. Only logging on error makes "is vsock working?"
	// hard to answer — silence is ambiguous.
	startedAt := time.Now()
	p.logger.Printf("agent-proxy: accept (client connected)")

	// Bound the dial. Machine.VSock blocks on a Code-Hex/vz C callback
	// internally; if the guest agent isn't listening, it sits there.
	dialCtx, cancel := context.WithTimeout(p.ctx, 10*time.Second)
	defer cancel()

	guest, err := p.machine.VSock(dialCtx, vsockproto.AgentVSockPort)
	if err != nil {
		p.logger.Printf("agent-proxy: vsock dial: %v", err)
		// A paused VM refuses dials by design (machine.ErrInvalidState) —
		// that's a lifecycle state, not wedge evidence, and counting it
		// would eventually mislabel a healthy paused guest as wedged and
		// advise destroying it.
		if errors.Is(err, machine.ErrInvalidState) {
			return
		}
		// Bump the consecutive-failure counter and surface a single
		// "guest appears wedged" breadcrumb when we cross the
		// threshold. The line fires once per wedge episode (the
		// counter only resets on a successful dial), keeping the log
		// readable when a wedge persists across many client retries.
		fails := atomicAddInt64(&p.dialFailCount, 1)
		if fails == wedgeThreshold {
			atomicStoreInt64(&p.wedged, 1)
			p.logger.Printf("agent-proxy: guest appears wedged after %d consecutive dial failures (recover: clawk destroy && clawk)", fails)
		}
		return
	}
	defer guest.Close()
	// Successful dial — clear the wedge state if it was set, so a
	// recovered VM doesn't keep advertising itself as wedged.
	if atomicLoadInt64(&p.dialFailCount) > 0 {
		atomicStoreInt64(&p.dialFailCount, 0)
		if atomicLoadInt64(&p.wedged) != 0 {
			atomicStoreInt64(&p.wedged, 0)
			p.logger.Printf("agent-proxy: guest recovered (vsock dial succeeded)")
		}
	}
	p.logger.Printf("agent-proxy: vsock connected in %s; bridging bytes", time.Since(startedAt).Round(time.Millisecond))

	// Bidirectional pipe with mutual cancellation: when either copy
	// returns (peer EOF, error, or our parent ctx canceled), we close
	// BOTH sides so the other goroutine wakes up.
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(guest, client)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, guest)
		done <- struct{}{}
	}()
	select {
	case <-done:
	case <-p.ctx.Done():
	}
	// Slam both ends shut. The remaining io.Copy returns immediately
	// once its source closes.
	_ = client.Close()
	_ = guest.Close()
	// Drain the second done; the deferred Closes above may race with
	// the second goroutine still being inside io.Copy.
	select {
	case <-done:
	case <-time.After(time.Second):
	}
	p.logger.Printf("agent-proxy: session ended after %s", time.Since(startedAt).Round(time.Millisecond))
}

// agentSockPath is the conventional location of the host-side Unix
// socket inside a VM's state directory. Every layer that needs to
// dial or listen on this path consults this helper so the convention
// can change in exactly one place.
func agentSockPath(vmDir string) string {
	return vmDir + "/agent.sock"
}
