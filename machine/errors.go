package machine

import "errors"

var (
	// ErrNoBackend is returned by Get when the named backend is not
	// registered in this binary (typically because the backend's package was
	// not imported, or it is compiled out on this GOOS).
	ErrNoBackend = errors.New("machine: no such backend")

	// ErrVSockUnsupported is returned by Machine.VSock when the backend does
	// not expose vsock or the Spec did not allocate a CID.
	ErrVSockUnsupported = errors.New("machine: vsock not supported")

	// ErrUnsupportedSpec is returned by Backend.New when the Spec requests a
	// feature the backend does not support (e.g. EFI boot on firecracker,
	// TAP net on vz). Wrapped errors explain which field.
	ErrUnsupportedSpec = errors.New("machine: spec not supported by backend")

	// ErrInvalidState is returned when a Machine method is called in an
	// incompatible lifecycle state (e.g. Start after Destroy).
	ErrInvalidState = errors.New("machine: invalid state transition")
)
