//go:build !darwin

package vz

// No init on non-darwin; machine.Get(Name) returns machine.ErrNoBackend.
