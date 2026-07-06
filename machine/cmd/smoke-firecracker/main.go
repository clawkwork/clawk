// smoke-firecracker boots Ubuntu via machine.Get("firecracker") end-to-end
// on Linux. Mirrors smoke-alpine's shape but exercises the firecracker
// backend we couldn't validate from a Mac.
//
// Intended to be run INSIDE a `clawk here --nested` VM on macOS, or on
// any Linux host with /dev/kvm and the firecracker binary available.
//
//	go run ./cmd/smoke-firecracker             # defaults + auto-download
//	go run ./cmd/smoke-firecracker --keep      # leave VM running after boot
//
// On non-linux the binary compiles to a stub that prints a clear error —
// the firecracker backend itself is linux-only.
package main
