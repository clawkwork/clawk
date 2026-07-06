// Package firecracker is a machine.Backend for AWS Firecracker
// (https://github.com/firecracker-microvm/firecracker) on Linux.
//
// The backend talks to firecracker over its REST-on-UDS API. Import this
// package for its side effect of registering itself with the machine
// registry:
//
//	import _ "github.com/clawkwork/clawk/machine/firecracker"
//
// On non-linux platforms the package compiles to nothing — no backend is
// registered, and machine.Get("firecracker") returns machine.ErrNoBackend.
package firecracker

// Name is the registered backend name.
const Name = "firecracker"
