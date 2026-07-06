// Package agentembed exposes the Go source files for the in-guest
// binaries clawk ships: clawk-pty-agent (PTY-backed shell sessions over
// vsock), clawk-init (PID 1 for OCI-image sandboxes), and
// clawk-time-sync.
//
// internal/guestbuild cross-compiles these on the host (GOOS=linux,
// CGO_ENABLED=0) and machine/oci injects the static binaries into the
// rootfs at disk-build time — arbitrary OCI images have no Go toolchain,
// so nothing is built in the guest.
//
// Why the .go.in extension: the sources use Linux-only AF_VSOCK
// constants and their own go.mod files. A non-.go extension keeps the
// host's Go tooling from compiling them as part of the clawk module —
// `go build ./...` doesn't recurse into them and IDE indexers ignore
// them.
package agentembed

import _ "embed"

// AgentMainGo is the agent's main.go source, cross-compiled on the host
// by internal/guestbuild.
//
//go:embed main.go.in
var AgentMainGo []byte

// AgentGoMod is the go.mod used to build the agent. Lists only the
// agent's direct deps (creack/pty + golang.org/x/sys); `go mod tidy`
// resolves the rest.
//
//go:embed go.mod.in
var AgentGoMod []byte

// TimeSyncMainGo is the source for the standalone clawk-time-sync
// binary that listens on vsock port 1025 and applies pushes from the
// host-side time-sync goroutine. Injected into the rootfs alongside
// clawk-pty-agent so guest clocks survive macOS standby.
//
//go:embed time_sync_main.go.in
var TimeSyncMainGo []byte

// TimeSyncGoMod is the matching go.mod for clawk-time-sync.
//
//go:embed time_sync_go.mod.in
var TimeSyncGoMod []byte

// InitMainGo is the source of clawk-init — PID 1 for OCI-image sandboxes.
// internal/guestbuild cross-compiles it on the host and machine/oci
// injects the binary into the rootfs at disk-build time.
//
//go:embed init_main.go.in
var InitMainGo []byte

// InitGoMod is the matching go.mod for clawk-init.
//
//go:embed init_go.mod.in
var InitGoMod []byte
