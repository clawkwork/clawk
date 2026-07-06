//go:build !linux

package firecracker

// No init on non-linux; machine.Get(Name) returns machine.ErrNoBackend.
