// Package vz is a machine.Backend that drives Apple's Virtualization.framework
// (vz) directly via CGO bindings (Code-Hex/vz). Driving the framework in-process
// (rather than an external VMM) exposes VM snapshot/restore via
// SaveMachineStateToPath / RestoreMachineStateFromURL.
//
//	import _ "github.com/clawkwork/clawk/machine/vz"
//
// On non-darwin platforms the package compiles to nothing — no backend is
// registered, and machine.Get("vz") returns machine.ErrNoBackend.
package vz

// Name is the registered backend name.
const Name = "vz"
