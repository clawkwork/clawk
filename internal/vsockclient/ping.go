package vsockclient

import (
	"context"
	"fmt"
	"time"

	"github.com/clawkwork/clawk/internal/vsockproto"
)

// Ping runs a trivial command through the whole agent path — host Unix
// socket → vzd proxy → guest vsock → clawk-pty-agent → exec — and waits
// for the exit frame. It is the boot-readiness probe for sshd-free (OCI)
// sandboxes: success proves the guest kernel booted, clawk-init ran, and
// the agent is accepting sessions.
//
// /bin/sh is the probe command because it is the one path POSIX
// guarantees in any bootable image; the exit code is irrelevant, only
// that the round trip completes.
func Ping(ctx context.Context, sockPath string, connectPort uint32, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := dialAgent(ctx, sockPath, connectPort)
	if err != nil {
		return err
	}
	defer conn.Close()

	hs, err := vsockproto.MarshalHandshake(vsockproto.Handshake{
		Cmd:  "/bin/sh",
		Args: []string{"-c", "exit 0"},
		Rows: 24,
		Cols: 80,
	})
	if err != nil {
		return fmt.Errorf("vsockclient: ping handshake: %w", err)
	}
	if err := vsockproto.WriteFrame(conn, vsockproto.FrameHandshake, hs); err != nil {
		return fmt.Errorf("vsockclient: ping write: %w", err)
	}

	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(timeout)
	}
	if err := conn.SetReadDeadline(deadline); err != nil {
		return fmt.Errorf("vsockclient: ping deadline: %w", err)
	}
	for {
		ft, _, err := vsockproto.ReadFrame(conn)
		if err != nil {
			return fmt.Errorf("vsockclient: ping read: %w", err)
		}
		if ft == vsockproto.FrameExit {
			return nil
		}
	}
}
