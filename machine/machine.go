// Package machine is a cross-platform microVM lifecycle API.
//
// The package defines a Spec that describes a VM and a Machine interface that
// backends implement. Backends register themselves via Register and callers
// look them up with Get. A backend wraps one hypervisor — firecracker on
// Linux, vz on macOS — and the Spec is intentionally the
// intersection of what they can all express.
//
// Capabilities that not every backend can provide (snapshot, EFI boot, etc.)
// are reported via Backend.Capabilities and surfaced as optional interfaces
// (Snapshottable, ...). Callers feature-check before using them.
package machine

import (
	"context"
	"net"
)

// State is a Machine's lifecycle phase.
type State string

const (
	StateCreated State = "created"
	StateRunning State = "running"
	// StatePaused is a running VM whose vCPUs are suspended (Pauseable).
	// Memory and device state stay resident; Resume continues execution.
	StatePaused    State = "paused"
	StateStopped   State = "stopped"
	StateDestroyed State = "destroyed"
)

// Machine is a single VM instance bound to a backend and a state directory.
//
// Methods are safe to call in any state; they return an error if the
// transition is invalid (e.g. Start on a destroyed machine). Implementations
// must be safe for concurrent use.
type Machine interface {
	// Create provisions on-disk artifacts (disks, sockets, configs) but does
	// not boot. Idempotent: calling Create twice returns nil the second time.
	Create(ctx context.Context) error

	// Start boots the VM. Returns once the hypervisor process is running and
	// the guest has begun boot; it does not wait for guest userspace.
	Start(ctx context.Context) error

	// Stop halts the VM. If graceful, the backend asks the guest to shut down
	// and waits up to an implementation-defined deadline; otherwise it kills
	// the hypervisor process.
	Stop(ctx context.Context, graceful bool) error

	// Destroy releases all on-disk and in-kernel resources owned by the
	// Machine. After Destroy returns nil, the state directory may be removed.
	Destroy(ctx context.Context) error

	// State reports the current lifecycle phase.
	State(ctx context.Context) (State, error)

	// VSock dials the guest on the given AF_VSOCK port. Returns
	// ErrVSockUnsupported if the backend does not expose vsock or if the Spec
	// did not allocate a CID.
	VSock(ctx context.Context, port uint32) (net.Conn, error)
}

// VSockListener is an optional capability for backends whose vsock
// transport allows the host to accept guest-initiated connections.
// Apple Virtualization.framework supports this; firecracker's
// implementation is one-way (guest listens, host dials) so it does
// not implement this interface.
//
// Used by the host-side SSH-agent proxy: a guest process dials a
// per-VM vsock port, the host accepts and bridges bytes to the
// host's local $SSH_AUTH_SOCK so 1Password / launchd ssh-agent
// keys are reachable from inside the VM without an SSH session.
type VSockListener interface {
	// VSockListen returns a net.Listener that accepts AF_VSOCK
	// connections initiated by the guest on the given port. Caller
	// owns the listener and must Close it.
	VSockListen(ctx context.Context, port uint32) (net.Listener, error)
}

// Snapshottable is an optional capability implemented by backends whose
// Caps.Snapshot is true. Callers must type-assert.
//
// A snapshot is written to a directory the backend owns for the duration of
// the call. The directory layout is backend-defined but stable across
// Snapshot/Restore pairs of the same backend.
type Snapshottable interface {
	// Snapshot pauses the VM, writes memory + device state into dir, and
	// resumes it. The VM must be running.
	Snapshot(ctx context.Context, dir string) error

	// Restore boots a new VM from a snapshot directory previously produced by
	// Snapshot. The Machine must be in StateCreated (not yet Started).
	Restore(ctx context.Context, dir string) error
}

// Suspendable is an optional capability for backends that can hibernate a
// VM: save its memory + device state into a directory and stop it WITHOUT
// letting the guest execute again. That ordering is the whole contract —
// because the guest never runs past the save point, the rootfs on disk is
// frozen at exactly the saved moment, which is what makes a later
// Snapshottable.Restore from the same directory safe. (A live Snapshot that
// resumes the guest afterwards lets the disk drift ahead of the saved
// memory image; restoring such a pair corrupts the guest filesystem.)
//
// The directory layout matches the backend's Snapshottable layout, so
// Suspend/Restore pair up the same way Snapshot/Restore do.
type Suspendable interface {
	// Suspend pauses the VM (if it isn't already paused), writes memory +
	// device state into dir, and stops the VM without resuming it. On
	// success the Machine is in StateStopped. On save failure the backend
	// makes a best effort to resume the guest so the VM isn't left wedged.
	Suspend(ctx context.Context, dir string) error
}

// Pauseable is an optional capability for backends that can suspend and
// resume a running guest without saving state to disk. Callers
// type-assert; backends without this capability run only on hosts whose
// power management is benign enough not to require it.
//
// The motivating use case is host sleep/wake: when the Mac hibernates,
// the daemon can Pause the VM before sleep and Resume on wake to keep
// guest timers and in-flight network state synchronised with the host's
// view of time. The wallclock watchdog also calls these on detected
// time jumps when the OS sleep notifications are missed (macOS standby
// is unreliable about delivering them).
type Pauseable interface {
	// Pause stops the vCPUs without altering memory or device state.
	// Idempotent: pausing a paused VM returns nil.
	Pause(ctx context.Context) error

	// Resume restarts the vCPUs after a Pause.
	// Idempotent: resuming a running VM returns nil.
	Resume(ctx context.Context) error
}
