package sandbox

import "github.com/clawkwork/clawk/internal/config"

// Provider abstracts VM lifecycle management.
// Implementations: vz (macOS), firecracker (Linux).
type Provider interface {
	// Create prepares the VM (image, config, etc.).
	Create(sb *config.Sandbox) error
	// Start launches the VM.
	Start(sb *config.Sandbox) error
	// Stop gracefully stops the VM.
	Stop(sb *config.Sandbox) error
	// Destroy removes all VM artifacts.
	Destroy(sb *config.Sandbox) error
	// Status returns the current VM state as a string.
	Status(sb *config.Sandbox) (string, error)
}

// ShellProvider is optionally implemented by providers that can open
// interactive shells directly (vz over the vsock agent).
type ShellProvider interface {
	Shell(sb *config.Sandbox, workdir string) error
	Exec(sb *config.Sandbox, command ...string) error
}

// CaptureExecProvider is optionally implemented by providers that can
// run a guest command non-interactively and return its combined output.
// Narrated hooks (`on create` / `on up`) use it to keep successful
// command output off the screen and show it only on failure.
type CaptureExecProvider interface {
	ExecCapture(sb *config.Sandbox, command ...string) (output string, err error)
}

// RootExecProvider is optionally implemented by providers that can run a
// one-shot command as root inside the guest without relying on sudo.
// OCI-image sandboxes need this: arbitrary images frequently ship no
// sudo, but their agent runs as root and can spawn root children
// directly.
type RootExecProvider interface {
	ExecRoot(sb *config.Sandbox, command ...string) error
}

// WorkspaceProvider is optionally implemented by providers whose guest
// mounts phase worktrees somewhere other than sandbox.WorkspaceRoot.
// Firecracker boots a bare-root rootfs without an `agent` user, so its
// shares live under /workspace; vz creates the agent user via clawk-init
// and mounts under /home/agent/workspace.
//
// Callers that need to address phase paths inside the guest (run.go's
// agentStartDir, up.go's runPhaseSetup) should query this interface
// when present and fall back to sandbox.WorkspaceRoot otherwise.
type WorkspaceProvider interface {
	GuestWorkspaceRoot() string
}

// GuestWorkspace returns the per-provider guest workspace path, falling
// back to the package default when the provider does not implement
// WorkspaceProvider.
func GuestWorkspace(p Provider) string {
	if wp, ok := p.(WorkspaceProvider); ok {
		return wp.GuestWorkspaceRoot()
	}
	return WorkspaceRoot
}
