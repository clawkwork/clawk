package machine

// Caps reports what a Backend supports. Returned by Backend.Capabilities.
//
// Callers use Caps to decide whether a Spec is satisfiable before creating a
// Machine, and to branch on optional interfaces (e.g. only type-asserting
// Snapshottable when Caps.Snapshot is true).
type Caps struct {
	// Snapshot: Machine implements Snapshottable.
	Snapshot bool

	// OCIRootFS: Spec.RootFS of type OCIImage is accepted.
	OCIRootFS bool

	// DirectKernel: Spec.Boot of type DirectKernel is accepted.
	DirectKernel bool

	// EFIBoot: Spec.Boot of type EFIBoot is accepted.
	EFIBoot bool

	// VSock: Machine.VSock returns real connections (not ErrVSockUnsupported).
	VSock bool

	// VirtioFS: Spec.Shares is honored.
	VirtioFS bool

	// UserModeNet: Spec.Net may contain a UserMode entry.
	UserModeNet bool

	// TAPNet: Spec.Net may contain a TAP entry.
	TAPNet bool

	// UnixgramNet: Spec.Net may contain a Unixgram entry.
	UnixgramNet bool

	// NestedVirt: Spec.NestedVirt is honored. For vz this depends on the
	// host (macOS 15+ / M3+), so the capability is queried dynamically
	// rather than hard-coded in Backend.Capabilities().
	NestedVirt bool
}
