package vsockclient

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/clawkwork/clawk/internal/vsockproto"
)

// Output runs one command through the agent and returns its combined
// output and exit code. Non-interactive: nothing touches the local
// terminal — frames only — so it's safe from daemons, diagnostics, and
// anything that must not disturb tty state. The context bounds the
// whole session.
func Output(ctx context.Context, sockPath string, connectPort uint32, user, cmd string, args ...string) (string, int, error) {
	conn, err := dialAgent(ctx, sockPath, connectPort)
	if err != nil {
		return "", -1, err
	}
	defer conn.Close()

	hs, err := vsockproto.MarshalHandshake(vsockproto.Handshake{
		Cmd:  cmd,
		Args: args,
		User: user,
		Rows: 24,
		Cols: 80,
	})
	if err != nil {
		return "", -1, fmt.Errorf("vsockclient: handshake: %w", err)
	}
	if err := vsockproto.WriteFrame(conn, vsockproto.FrameHandshake, hs); err != nil {
		return "", -1, fmt.Errorf("vsockclient: handshake write: %w", err)
	}

	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Second)
	}
	if err := conn.SetReadDeadline(deadline); err != nil {
		return "", -1, fmt.Errorf("vsockclient: deadline: %w", err)
	}

	var out strings.Builder
	for {
		ft, payload, err := vsockproto.ReadFrame(conn)
		if err != nil {
			return out.String(), -1, fmt.Errorf("vsockclient: read: %w", err)
		}
		switch ft {
		case vsockproto.FrameData:
			out.Write(payload)
		case vsockproto.FrameExit:
			code, err := vsockproto.DecodeExit(payload)
			if err != nil {
				return out.String(), -1, err
			}
			return out.String(), int(code), nil
		}
	}
}
