package cli

// Daemon helpers shared by the per-provider VM daemons (__vzd on darwin,
// __fcd on linux). Each daemon owns one sandbox's VM lifecycle out of
// process so the VM outlives the CLI invocation; the bits that are
// identical across providers — the allow-list wiring, the shutdown loop,
// and the goroutine dump — live here. The only per-platform piece is
// openDaemonLog (Dup2 vs Dup3), which each daemon file defines itself.

import (
	"context"
	"fmt"
	"log"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/clawkwork/clawk/internal/netfilter"
	"github.com/clawkwork/clawk/internal/vzdctl"
	"github.com/clawkwork/clawk/machine"
)

// suspendStateDir is where a sandbox's suspend-to-disk state lives, inside
// its VM dir. Written by the daemon's suspend handler, consumed (one-shot)
// by the next daemon boot, removed by both paths.
func suspendStateDir(vmDir string) string { return filepath.Join(vmDir, "suspend") }

// hasSuspendState reports whether dir holds a suspend-to-disk state file.
// The machine module owns the per-backend file layout.
func hasSuspendState(dir string) bool { return machine.SuspendStateExists(dir) }

// suspendBootRootFS decides the rootfs for a boot that may restore a
// suspend state. Normal boots return (nil, false): the caller keeps its
// own spec — on vz that re-materializes a fresh clone from the image,
// the per-boot-disposable rootfs the design wants. A RESTORE boot must
// not do that: the saved memory image pairs with the exact disk contents
// at the save point, and restoring it onto a fresh clone corrupts the
// guest filesystem (the invariant machine.Suspendable documents). So
// when a suspend state exists and the suspended boot's disk is still
// present, return that disk — the backend's Materialize then no-ops
// (src == dst) and the memory image meets the filesystem it was saved
// with. A state whose disk is gone can never restore: discard it loudly
// and let the caller cold-boot fresh.
func suspendBootRootFS(vmDir, diskName string, logger *log.Logger) (machine.RootFS, bool) {
	sDir := suspendStateDir(vmDir)
	if !machine.SuspendStateExists(sDir) {
		return nil, false
	}
	disk := filepath.Join(vmDir, diskName)
	if _, err := os.Stat(disk); err != nil {
		logger.Printf("suspend state at %s has no disk (%s) to restore onto — discarding it and cold-booting fresh", sDir, diskName)
		if err := os.RemoveAll(sDir); err != nil {
			logger.Printf("removing diskless suspend state: %v", err)
		}
		return nil, false
	}
	return machine.RawDisk{Path: disk}, true
}

// restoreOrStart boots the machine, restoring from the VM dir's suspend
// state when one exists instead of cold-booting. Shared by both daemons so
// the one-shot-consume invariant lives in exactly one place.
//
// Consumption is committed BEFORE the guest can run: the suspend dir is
// renamed aside first (atomic on the same filesystem), so even a daemon
// killed mid-restore can never restore the same memory image twice onto a
// rootfs the guest has since diverged — the corruption the suspend design
// exists to prevent. The renamed dir is deleted after the attempt; a crash
// in between leaves only dead bytes that the next boot sweeps.
func restoreOrStart(ctx context.Context, m machine.Machine, vmDir string, logger *log.Logger, want machine.SuspendMeta) (restored bool, err error) {
	sDir := suspendStateDir(vmDir)
	consumed := sDir + ".consumed"
	// A previous boot that crashed between rename and delete left the
	// consumed dir behind; it is stale by definition. Sweep it first.
	if err := os.RemoveAll(consumed); err != nil {
		logger.Printf("sweeping stale consumed suspend state: %v", err)
	}
	if hasSuspendState(sDir) {
		// Preflight: a state stamped by a different backend or VM shape
		// (a clawk upgrade that changed how the machine is built) can
		// never restore — say why and cold-boot instead of handing the
		// hypervisor bytes it will refuse with a cryptic error. States
		// without meta (written by earlier clawks) skip this and rely on
		// the hypervisor's own validation plus the cold-boot fallback.
		if meta, ok := machine.ReadSuspendMeta(sDir); ok {
			if reason := meta.IncompatibleWith(want); reason != "" {
				logger.Printf("suspend state at %s is not restorable by this boot: %s — discarding it and cold-booting (processes running before the snapshot are gone)", sDir, reason)
				if err := os.RemoveAll(sDir); err != nil {
					logger.Printf("removing unrestorable suspend state: %v", err)
				}
				if err := m.Start(ctx); err != nil {
					return false, fmt.Errorf("starting machine: %w", err)
				}
				logger.Printf("vm started")
				return false, nil
			}
		}
		if snap, ok := m.(machine.Snapshottable); ok {
			if err := os.Rename(sDir, consumed); err != nil {
				logger.Printf("consuming suspend state failed (%v) — cold boot", err)
			} else {
				logger.Printf("suspend state found at %s — restoring", sDir)
				if err := snap.Restore(ctx, consumed); err != nil {
					// A failed restore leaves the machine unstarted (the
					// hypervisor rejects the state before touching the
					// VM), so the cold Start below is safe on the same
					// machine.
					logger.Printf("restore failed (%v) — falling back to cold boot", err)
				} else {
					restored = true
					logger.Printf("vm restored from suspend state")
				}
				if err := os.RemoveAll(consumed); err != nil {
					logger.Printf("removing consumed suspend state: %v", err)
				}
			}
		} else {
			// No backend can ever restore this state, and a cold boot
			// diverges the disk from it immediately — it is dead weight
			// either way. Deleting is correct; being loud about it is owed.
			logger.Printf("suspend state found at %s but backend %T cannot restore — discarding it and cold-booting", sDir, m)
			if err := os.RemoveAll(sDir); err != nil {
				logger.Printf("removing unrestorable suspend state: %v", err)
			}
		}
	}
	if !restored {
		if err := m.Start(ctx); err != nil {
			return false, fmt.Errorf("starting machine: %w", err)
		}
		logger.Printf("vm started")
	}
	return restored, nil
}

// onVMResume is called after the daemon resumes a paused VM, so platform
// code can re-synchronise guest state that went stale during the pause.
// The darwin daemon points this at triggerTimeSync (the guest wallclock is
// the thing a long pause visibly breaks); elsewhere it is a no-op.
var onVMResume = func() {}

// vmLifecycle owns the daemon's pause/suspend surface: it holds the machine
// handle for the control socket's lifecycle endpoints and tracks the two
// bits the machine itself can't know — whether a pause was user-requested
// (vs the wallclock watchdog's transient sleep-recovery bounce) and whether
// this boot restored the guest from a suspend state file.
//
// Built before the control socket comes up (the machine doesn't exist yet);
// attach() hands it the machine once constructed. Until then the endpoints
// report "booting".
type vmLifecycle struct {
	logger     *log.Logger
	sbName     string
	suspendDir string

	mu         sync.Mutex
	m          machine.Machine
	restored   bool
	userPaused bool
	// suspendMeta describes this boot's backend + VM shape; written
	// beside the state files on suspend so the next boot can preflight
	// the restore (see restoreOrStart). Set by setSuspendMeta once the
	// daemon has built its spec.
	suspendMeta machine.SuspendMeta
}

// setSuspendMeta records the backend + VM shape this daemon boots with,
// for stamping onto any suspend state it later writes.
func (l *vmLifecycle) setSuspendMeta(meta machine.SuspendMeta) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.suspendMeta = meta
}

// daemonVerbTimeout bounds the machine calls behind the pause/resume
// endpoints. Both are sub-second in the hypervisor; the margin covers a
// host under load. Suspend deliberately has no such bound — see suspend().
const daemonVerbTimeout = 30 * time.Second

func newVMLifecycle(sbName, vmDir string, logger *log.Logger) *vmLifecycle {
	return &vmLifecycle{logger: logger, sbName: sbName, suspendDir: suspendStateDir(vmDir)}
}

// attach hands the constructed (and started) machine to the lifecycle
// surface. restored records whether this boot came from a suspend state.
func (l *vmLifecycle) attach(m machine.Machine, restored bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.m = m
	l.restored = restored
}

// machineHandle returns the attached machine, or nil while booting.
func (l *vmLifecycle) machineHandle() machine.Machine {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m
}

// isUserPaused reports whether the current pause was requested through the
// control socket. The wallclock watchdog consults it so its sleep-recovery
// bounce never silently resumes a VM the user deliberately froze.
func (l *vmLifecycle) isUserPaused() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.userPaused
}

// state snapshots the live lifecycle for GET /v1/lifecycle.
func (l *vmLifecycle) state() vzdctl.LifecycleState {
	l.mu.Lock()
	m, restored := l.m, l.restored
	l.mu.Unlock()
	out := vzdctl.LifecycleState{
		State:    vzdctl.LifecycleBooting,
		Restored: restored,
	}
	if m == nil {
		return out
	}
	s, err := m.State(context.Background())
	switch {
	case err != nil:
		// An unprobeable machine still exists — report running rather
		// than inventing a state; the error is transient by construction
		// (State only reads a field under the machine's lock).
		out.State = vzdctl.LifecycleRunning
	case s == machine.StatePaused:
		out.State = vzdctl.LifecyclePaused
	default:
		out.State = string(s)
	}
	return out
}

// pause suspends the vCPUs on behalf of `clawk pause`.
func (l *vmLifecycle) pause() error {
	m := l.machineHandle()
	if m == nil {
		return fmt.Errorf("vm is still booting")
	}
	pauser, ok := m.(machine.Pauseable)
	if !ok {
		return fmt.Errorf("backend does not support pause")
	}
	ctx, cancel := context.WithTimeout(context.Background(), daemonVerbTimeout)
	defer cancel()
	if err := pauser.Pause(ctx); err != nil {
		return err
	}
	l.mu.Lock()
	l.userPaused = true
	l.mu.Unlock()
	l.logger.Printf("lifecycle: paused by user (memory stays resident; resume with 'clawk resume')")
	return nil
}

// resume restarts the vCPUs on behalf of `clawk resume` (and the attach
// verbs' auto-resume).
func (l *vmLifecycle) resume() error {
	m := l.machineHandle()
	if m == nil {
		return fmt.Errorf("vm is still booting")
	}
	pauser, ok := m.(machine.Pauseable)
	if !ok {
		return fmt.Errorf("backend does not support resume")
	}
	ctx, cancel := context.WithTimeout(context.Background(), daemonVerbTimeout)
	defer cancel()
	if err := pauser.Resume(ctx); err != nil {
		return err
	}
	l.mu.Lock()
	l.userPaused = false
	l.mu.Unlock()
	l.logger.Printf("lifecycle: resumed by user")
	// A guest that sat frozen has a stale wallclock; push the host time
	// now instead of waiting for the next periodic sync.
	onVMResume()
	return nil
}

// suspend saves memory + device state under the suspend dir and stops the
// VM without resuming it. On success the machine is StateStopped, which the
// daemon's shutdown loop notices within one probe tick — the daemon exits
// on its own; no signal needed.
//
// The daemon owns the record write, mirroring the idle watchdog's park:
// the layer that performs a state transition is the one that records it.
// Writing before the save means a client that dials mid-suspend reads
// "stopped (suspended)" instead of racing a half-written record, and a
// CLI killed mid-call can't leave the record lying — the revert lives in
// the process that knows how the save actually ended.
func (l *vmLifecycle) suspend() error {
	m := l.machineHandle()
	if m == nil {
		return fmt.Errorf("vm is still booting")
	}
	sus, ok := m.(machine.Suspendable)
	if !ok {
		return fmt.Errorf("backend does not support suspend-to-disk")
	}
	revert, err := markSuspended(l.sbName)
	if err != nil {
		// The record is bookkeeping; the state transition is the point.
		// Proceed — status will reconcile from the live daemon's absence.
		l.logger.Printf("lifecycle: recording suspend in sandbox record: %v", err)
	}
	l.logger.Printf("lifecycle: suspend requested — saving memory + device state to %s", l.suspendDir)
	// No deadline: the save writes the guest's entire memory image; its
	// duration scales with guest RAM, and interrupting it halfway is the
	// one outcome worse than waiting.
	if err := sus.Suspend(context.Background(), l.suspendDir); err != nil {
		if revert != nil {
			revert()
		}
		l.logger.Printf("lifecycle: suspend failed: %v", err)
		return err
	}
	// Stamp the state with what wrote it, so the next boot can refuse a
	// restore that can't work (backend or VM shape changed) instead of
	// feeding it to the hypervisor. Best-effort: the state is already
	// safely written, and a meta-less state restores like a pre-meta one.
	l.mu.Lock()
	meta := l.suspendMeta
	l.mu.Unlock()
	if err := machine.WriteSuspendMeta(l.suspendDir, meta); err != nil {
		l.logger.Printf("lifecycle: writing suspend meta: %v", err)
	}
	l.logger.Printf("lifecycle: suspended — state saved, vm stopped; next boot restores it")
	// The machine is now StateStopped, so waitAndShutdown's probe winds the
	// daemon down within a tick — that is the normal exit. But a
	// self-initiated stop has no outside enforcer (unlike `clawk down`,
	// where the CLI escalates to SIGKILL), so arm the same force-exit
	// backstop the idle park uses: if graceful teardown hasn't exited the
	// process in time, remove the pidfile ourselves (os.Exit skips the
	// deferred cleanup) and die hard, so a teardown wedge can never leave a
	// zombie daemon whose live pidfile makes status report "running". The
	// state is already saved, so a hard exit is indistinguishable from a
	// graceful one to every reader; the timer dies with the process when
	// shutdown completes normally first.
	l.armExitBackstop()
	return nil
}

// suspendExitGrace bounds how long a post-suspend daemon may spend in
// graceful shutdown before force-exiting. Generous next to the stop path's
// internal budgets so it only fires on a genuine wedge, never a slow clean
// stop.
const suspendExitGrace = 30 * time.Second

func (l *vmLifecycle) armExitBackstop() {
	vmDir := filepath.Dir(l.suspendDir)
	time.AfterFunc(suspendExitGrace, func() {
		l.logger.Printf("lifecycle: shutdown still running %s after suspend; forcing exit", suspendExitGrace)
		// Only one of these exists (vz.pid on darwin, fc.pid on linux);
		// removing the absent one is a harmless no-op.
		for _, name := range []string{"vz.pid", "fc.pid"} {
			if err := os.Remove(filepath.Join(vmDir, name)); err != nil && !os.IsNotExist(err) {
				l.logger.Printf("lifecycle: removing pidfile %s: %v", name, err)
			}
		}
		os.Exit(0)
	})
}

// markSuspended records the suspend outcome in the sandbox record and
// returns a revert that restores the prior values (re-reading the record
// so it never clobbers unrelated interim writes).
func markSuspended(name string) (revert func(), err error) {
	sb, err := store.Load(name)
	if err != nil {
		return nil, err
	}
	prevDesired, prevVM, prevReason := sb.DesiredState, sb.VMState, sb.StopReason
	sb.DesiredState = config.VMStateStopped
	sb.VMState = config.VMStateStopped
	sb.StopReason = config.StopReasonSuspended
	if err := store.Save(sb); err != nil {
		return nil, err
	}
	return func() {
		cur, err := store.Load(name)
		if err != nil {
			return
		}
		cur.DesiredState, cur.VMState, cur.StopReason = prevDesired, prevVM, prevReason
		_ = store.Save(cur)
	}, nil
}

// lifecycleHandlers adapts the holder to the control socket's shape.
func (l *vmLifecycle) lifecycleHandlers() *vzdctl.LifecycleHandlers {
	return &vzdctl.LifecycleHandlers{
		State:   l.state,
		Pause:   l.pause,
		Resume:  l.resume,
		Suspend: l.suspend,
	}
}

// startAllowList builds the sandbox's AllowList, kicks off the periodic
// DNS-refresh goroutine, and returns it. The returned AllowList satisfies
// machine.Filter via its AllowTCP/ObserveDNS shim methods, so the same
// object filters gvproxy on both providers.
func startAllowList(sb *config.Sandbox, logger *log.Logger) (*netfilter.AllowList, error) {
	refreshSourcePolicies(sb, logger)
	chain, warnings := resolveChain(sb)
	for _, w := range warnings {
		logger.Printf("acl: %s", w)
	}
	allow := netfilter.NewAllowListFromChain(chain)
	// One log line per first-seen blocked host: enough to debug "why is
	// the agent stuck" from the daemon log alone, without per-SYN spam. The
	// full ledger is queryable via the control socket.
	allow.OnDenial = func(d netfilter.Denial) {
		logger.Printf("acl: denied %s (%s:%s)", d.Host, d.IP, d.Port)
	}

	// Interactive gate: harmless to enable always — it only holds and
	// prompts while a decider (the menubar app or `clawk network watch`)
	// is attached over the control socket; with none attached it denies
	// at once, exactly as before. An "always" decision is persisted to the
	// sandbox's policy so it survives restarts.
	gate := allow.EnableInteractive()
	gate.SetOnAlways(func(host string, ip netip.Addr) {
		if err := persistAlwaysAllow(allow, sb.Name, host, ip); err != nil {
			logger.Printf("acl: persisting always-allow for %s failed: %v", grantLabel(host, ip), err)
			return
		}
		logger.Printf("acl: persisted always-allow %s", grantLabel(host, ip))
	})
	gate.SetOnDenyDomain(func(domain string) {
		if err := persistDenyDomain(allow, sb.Name, domain); err != nil {
			logger.Printf("acl: persisting domain block %q failed: %v", domain, err)
			return
		}
		logger.Printf("acl: blocked domain %s (+ subdomains)", domain)
	})

	allow.Start(5 * time.Minute)
	ipCount, prefCount := allow.Size()
	logger.Printf("acl active: %d IPs + %d CIDRs across %d policy layers",
		ipCount, prefCount, len(sb.Network.Blocks)+len(effectiveUseForLog(sb)))
	return allow, nil
}

// effectiveUseForLog resolves the sandbox's policy references purely for the
// startup log line; resolution errors are already logged by resolveChain.
func effectiveUseForLog(sb *config.Sandbox) []string {
	ns, err := store.LoadNamespace(sb.NamespaceName())
	if err != nil {
		ns = &config.Namespace{Name: sb.NamespaceName()}
	}
	return effectiveUse(ns, sb)
}

// controlHandlers builds the control-socket callbacks shared by both VM
// daemons: the denial ledger, a live network-policy reload from the store,
// the VM lifecycle surface (pause/resume/suspend), and (when the allow
// list has one) the interactive gate.
func controlHandlers(sb *config.Sandbox, allow *netfilter.AllowList, lc *vmLifecycle, logger *log.Logger) vzdctl.Handlers {
	return vzdctl.Handlers{
		Denials:   allow.Denials,
		Lifecycle: lc.lifecycleHandlers(),
		Reload: func() error {
			cur, err := store.Load(sb.Name)
			if err != nil {
				return fmt.Errorf("reloading sandbox record: %w", err)
			}
			chain, warnings := resolveChain(cur)
			for _, w := range warnings {
				logger.Printf("acl: %s", w)
			}
			allow.SetChain(chain)
			logger.Printf("acl reloaded: %d stored blocks, use=%v",
				len(cur.Network.Blocks), effectiveUseForLog(cur))
			return nil
		},
		Gate: allow.Gate(),
	}
}

// persistAlwaysAllow records an interactive "allow always" decision in the
// sandbox's stored policy and applies it live. A known DNS name is persisted
// as an allowed domain (so it generalizes across the host's other IPs);
// otherwise the literal IP is stored.
func persistAlwaysAllow(allow *netfilter.AllowList, name, host string, ip netip.Addr) error {
	cur, err := store.Load(name)
	if err != nil {
		return fmt.Errorf("loading sandbox record: %w", err)
	}
	custom := cur.Network.Block(config.BlockOriginCustom)
	if host != "" && host != ip.String() {
		if !slices.Contains(custom.AllowDomains, host) {
			custom.AllowDomains = append(custom.AllowDomains, host)
		}
	} else {
		entry := ip.String()
		if !slices.Contains(custom.AllowIPs, entry) {
			custom.AllowIPs = append(custom.AllowIPs, entry)
		}
	}
	if err := store.Save(cur); err != nil {
		return fmt.Errorf("saving sandbox record: %w", err)
	}
	chain, _ := resolveChain(cur)
	allow.SetChain(chain)
	return nil
}

// persistDenyDomain records an interactive "deny domain" decision in the
// sandbox's custom block (the registrable domain, blocking it and every
// subdomain) and applies it live.
func persistDenyDomain(allow *netfilter.AllowList, name, domain string) error {
	cur, err := store.Load(name)
	if err != nil {
		return fmt.Errorf("loading sandbox record: %w", err)
	}
	custom := cur.Network.Block(config.BlockOriginCustom)
	if !slices.Contains(custom.DenyDomains, domain) {
		custom.DenyDomains = append(custom.DenyDomains, domain)
	}
	if err := store.Save(cur); err != nil {
		return fmt.Errorf("saving sandbox record: %w", err)
	}
	chain, _ := resolveChain(cur)
	allow.SetChain(chain)
	return nil
}

// refreshSourcePolicies best-effort refetches stale blocklist caches for
// every source-backed policy the sandbox references, so a daemon boot picks
// up list updates. Failures only log: the previous cache keeps serving. A
// policy that has NEVER fetched serves an empty deny set — fail-open for a
// blocklist — which resolveChain surfaces as a loud warning rather than
// blocking the boot (a flaky blocklist host must not brick sandbox start).
func refreshSourcePolicies(sb *config.Sandbox, logger *log.Logger) {
	ns, err := store.LoadNamespace(sb.NamespaceName())
	if err != nil {
		ns = &config.Namespace{Name: sb.NamespaceName()}
	}
	for _, name := range effectiveUse(ns, sb) {
		p, err := store.LoadPolicy(name)
		if err != nil || p.Source == "" {
			continue
		}
		if err := refreshPolicyCache(p, false); err != nil {
			logger.Printf("acl: refreshing policy %q: %v", name, err)
		}
	}
}

// grantLabel describes an interactive grant for a log line: the DNS name with
// the IP in parentheses, or just the IP when no name was observed.
func grantLabel(host string, ip netip.Addr) string {
	if host != "" && host != ip.String() {
		return fmt.Sprintf("%s (%s)", host, ip)
	}
	return ip.String()
}

// countForwards walks spec.Net and totals port forwards across the
// (usually single) UserMode entry. Split out so a daemon's startup log
// line stays a single fmt string.
func countForwards(spec machine.Spec) int {
	n := 0
	for _, nw := range spec.Net {
		if um, ok := nw.(machine.UserMode); ok {
			n += len(um.Forwards)
		}
	}
	return n
}

// waitAndShutdown blocks until SIGTERM/SIGINT or the guest powers off on
// its own, then asks the machine to stop gracefully.
//
// In debug mode, SIGUSR1 triggers a goroutine-stack dump without exiting —
// so an operator observing a "session hangs but daemon alive" freeze can
// `kill -USR1 $(cat <pid>)` and get a full stack in the daemon log to
// diagnose where we're stuck (hypervisor callback, gvproxy Serve, the
// frame pump, or the state probe).
func waitAndShutdown(ctx context.Context, m machine.Machine, logger *log.Logger) error {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGUSR1)

	stateTick := time.NewTicker(2 * time.Second)
	defer stateTick.Stop()

	// Track last-observed state so we log only on transitions. Without
	// this the periodic probe would either spam the log every 2 s or
	// log nothing and leave us guessing what the probe saw leading up to
	// a freeze. Transitions — and transitions only — are the signal.
	lastState := machine.StateRunning

	for {
		select {
		case sig := <-sigCh:
			if sig == syscall.SIGUSR1 {
				dumpGoroutines(logger, "SIGUSR1")
				continue
			}
			logger.Printf("caught %s, stopping vm", sig)
			return gracefulStop(m, logger)
		case <-stateTick.C:
			s, err := m.State(ctx)
			if err != nil {
				logger.Printf("state probe: %v", err)
				continue
			}
			if s != lastState {
				logger.Printf("state transition: %s -> %s", lastState, s)
				lastState = s
			}
			// Paused is alive: the guest's memory and devices are resident
			// and a resume continues it — only a genuine exit (stopped,
			// destroyed) ends the daemon. This is also how suspend works:
			// the suspend handler leaves the machine StateStopped and this
			// probe winds the daemon down within a tick.
			if s != machine.StateRunning && s != machine.StatePaused {
				logger.Printf("vm exited on its own (state=%s)", s)
				return nil
			}
		}
	}
}

func gracefulStop(m machine.Machine, logger *log.Logger) error {
	stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := m.Stop(stopCtx, true); err != nil {
		logger.Printf("graceful stop: %v", err)
	}
	return nil
}

// dumpGoroutines writes a snapshot of every goroutine's stack to the
// logger. Useful for post-mortem when the daemon is still alive but the
// VM has wedged — tells us whether we're stuck in a hypervisor callback,
// gvproxy's Serve loop, the frame pump, the state probe, or elsewhere.
func dumpGoroutines(logger *log.Logger, reason string) {
	var buf [1 << 20]byte
	n := runtime.Stack(buf[:], true)
	logger.Printf("goroutine dump (%s):\n%s", reason, buf[:n])
	_ = pprof.Lookup("goroutine")
}
