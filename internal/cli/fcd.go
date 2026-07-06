//go:build linux

package cli

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/clawkwork/clawk/internal/debug"
	"github.com/clawkwork/clawk/internal/sandbox"
	"github.com/clawkwork/clawk/internal/vzdctl"
	"github.com/clawkwork/clawk/machine"

	"github.com/spf13/cobra"
)

func init() { rootCmd.AddCommand(fcdCmd) }

// fcdCmd runs one firecracker sandbox's VM lifecycle out of process — the
// linux counterpart of __vzd. Internal; `clawk up` spawns it detached so the
// VM outlives the CLI invocation.
//
// The daemon owns: the logfile, its own pidfile (for `up` liveness and
// `stop`), the AllowList's periodic DNS refresh, and signal handling.
// Everything else — gvproxy, the frame pump, and the firecracker VM — is
// delegated to the machine library (via the firecracker provider's
// DaemonSpec + the machine/firecracker backend's UserMode support).
var fcdCmd = &cobra.Command{
	Use:    "__fcd <sandbox>",
	Short:  "internal: run a firecracker sandbox VM + gvproxy via the machine library",
	Hidden: true,
	Args:   cobra.ExactArgs(1),
	RunE:   runFcd,
}

func runFcd(_ *cobra.Command, args []string) (retErr error) {
	sb, err := store.Load(args[0])
	if err != nil {
		return fmt.Errorf("loading sandbox: %w", err)
	}
	vmDir := store.VMDir(sb.Name)
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		return fmt.Errorf("preparing vm dir: %w", err)
	}

	logger, closeLog, err := openDaemonLog(vmDir)
	if err != nil {
		return err
	}
	defer closeLog()
	defer func() {
		if retErr != nil {
			logger.Printf("FATAL: %v", retErr)
		}
		if r := recover(); r != nil {
			logger.Printf("PANIC: %v", r)
			dumpGoroutines(logger, "panic")
			panic(r)
		}
	}()

	logger.Printf("fcd starting pid=%d sandbox=%q debug=%v",
		os.Getpid(), sb.Name, debug.Enabled())

	pidPath := filepath.Join(vmDir, "fc.pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		return fmt.Errorf("writing pidfile: %w", err)
	}
	defer os.Remove(pidPath)

	allow, err := startAllowList(sb, logger)
	if err != nil {
		return err
	}
	defer allow.Stop()

	// Lifecycle surface: hands the machine to the control socket's
	// pause/resume/suspend endpoints once it exists.
	lc := newVMLifecycle(sb.Name, vmDir, logger)

	// Control socket: lets the CLI push policy edits into the live allow list
	// (`clawk network allow` with no down/up), read the denial ledger, and
	// drive the VM lifecycle (`clawk pause/resume/snapshot`).
	// Best-effort — without it, edits apply on the next up, as before.
	ctl, err := vzdctl.Start(vzdctl.SocketPath(vmDir), controlHandlers(sb, allow, lc, logger))
	if err != nil {
		logger.Printf("control socket: disabled (%v) — network edits apply on next up", err)
	} else {
		defer ctl.Close()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The provider sets up the host bridge/TAPs and returns the spec, whose
	// UserMode net carries the egress filter; the backend brings up gvproxy.
	prov := sandbox.NewFirecrackerProvider(store)
	spec, err := prov.DaemonSpec(sb, allow)
	if err != nil {
		return fmt.Errorf("building spec: %w", err)
	}
	logger.Printf("spec: vcpu=%d mem=%dMiB forwards=%d",
		spec.VCPU, spec.MemoryMiB, countForwards(spec))

	backend, err := machine.Get("firecracker")
	if err != nil {
		return fmt.Errorf("getting firecracker backend: %w", err)
	}
	m, err := backend.New(ctx, spec, prov.FCStateDir(sb))
	if err != nil {
		return fmt.Errorf("constructing machine: %w", err)
	}
	if err := m.Create(ctx); err != nil {
		return fmt.Errorf("preparing machine: %w", err)
	}
	// Boot — restoring from a `clawk snapshot` suspend state when one
	// exists; same contract as __vzd (see restoreOrStart).
	suspendMeta := machine.SuspendMeta{
		Backend:         "firecracker",
		SpecFingerprint: machine.SuspendSpecFingerprint(spec),
		ClawkVersion:    buildVersion(),
	}
	lc.setSuspendMeta(suspendMeta)
	restored, err := restoreOrStart(ctx, m, vmDir, logger, suspendMeta)
	if err != nil {
		return err
	}
	lc.attach(m, restored)

	return waitAndShutdown(ctx, m, logger)
}

// openDaemonLog opens fcd.log in append mode and returns a logger plus a close
// function. Stderr is dup'd onto the same file so internal/debug output (and
// anything a dependency writes to fd 2) is captured alongside the daemon's own
// lines — the CLI spawns fcd with Stderr=nil. Dup3 (not Dup2) because
// linux/arm64 has no dup2 syscall.
func openDaemonLog(vmDir string) (*log.Logger, func() error, error) {
	f, err := os.OpenFile(
		filepath.Join(vmDir, "fcd.log"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("opening daemon log: %w", err)
	}
	if err := syscall.Dup3(int(f.Fd()), int(os.Stderr.Fd()), 0); err != nil {
		fmt.Fprintf(f, "warning: could not redirect stderr: %v\n", err)
	}
	return log.New(f, "", log.LstdFlags), f.Close, nil
}
