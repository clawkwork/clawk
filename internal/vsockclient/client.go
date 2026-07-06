// Package vsockclient is the Mac-side end of the vsock PTY transport.
//
// Architecture overview:
//
//	┌───────────┐ Unix sock ┌─────────────┐  vsock  ┌─────────────┐
//	│  iTerm2   │◀─────────▶│   vzd    │◀───────▶│ guest agent │
//	│ (claude)  │  framed   │ (host proxy │  framed │ (clawk-pty- │
//	└───────────┘  proto    └─────────────┘  proto  │   agent)    │
//	                                                └─────────────┘
//
// The client speaks vsockproto over a host-side Unix domain socket.
// vzd's proxy (internal/cli/agent_proxy_darwin.go) bridges that
// Unix socket to the guest's AF_VSOCK listener via the vz backend's
// Machine.VSock(). The wire protocol is identical on both sides — the
// proxy doesn't parse or modify frames, it just shovels bytes.
//
// Sleep/wake survival is the whole point: vsock has no TCP timeouts
// or keepalives, so an idle connection survives a laptop nap intact —
// the host proxy and guest agent simply resume where they left off when
// the VM's vCPU is rescheduled after wake.
package vsockclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/clawkwork/clawk/internal/vsockproto"
	"golang.org/x/term"
)

// Config describes one Run() invocation.
type Config struct {
	// SocketPath is the host-side Unix socket vzd exposes for this
	// VM. Conventionally <vmDir>/agent.sock. Must exist when Run is
	// called; the daemon creates it during VM start.
	SocketPath string

	// Cmd is the program to exec inside the guest PTY. Empty → the
	// guest agent uses /bin/bash.
	Cmd string

	// Args are the args to Cmd.
	Args []string

	// Env is extra environment forwarded to the child. Use this for
	// LANG, COLORTERM, anything user-specific. PATH/HOME/USER are set
	// by the agent based on User.
	Env []string

	// Cwd, if non-empty, is the child's working directory. Empty falls
	// back to the resolved user's home.
	Cwd string

	// User is the OS user inside the guest to drop to (typically
	// "agent"). Empty leaves the agent running as itself (root).
	User string

	// ClearScreen asks Run to clear the terminal before relaying, so a
	// full-screen TUI child (claude, codex, opencode) starts on a clean
	// canvas instead of overdrawing whatever the CLI printed first (boot
	// progress, hints): such TUIs position with absolute cursor moves and
	// don't erase the cells they skip, so stale text shows through.
	//
	// Leave it false for a plain interactive shell: a login shell is
	// line-oriented, draws at the cursor, and clearing would needlessly
	// wipe the user's scrollback — the shell should read as a continuation
	// of the terminal it was launched from, not a fresh screen.
	ClearScreen bool

	// ConnectPort selects firecracker's hybrid-vsock transport. 0 (vz):
	// SocketPath is a per-guest-port channel; speak the framed protocol
	// immediately. Non-zero (firecracker): SocketPath is firecracker's
	// shared vsock UDS and Run first does a "CONNECT <ConnectPort>"
	// handshake — typically the guest pty-agent's port, 1024.
	ConnectPort uint32
}

// DefaultDialTimeout is how long Run waits for the agent socket to
// become connectable. The proxy comes up early in vzd, so we
// shouldn't normally have to wait, but right after `clawk up`
// the socket can lag the daemon by a moment.
const DefaultDialTimeout = 10 * time.Second

// ErrAgentUnavailable is returned when the agent socket isn't there or
// refuses connections. Callers distinguish it from a live-connection
// error to decide whether to retry or fall back to another path.
var ErrAgentUnavailable = errors.New("vsockclient: agent socket unavailable")

// Run dials the agent, allocates a PTY-like raw mode on os.Stdin/
// Stdout, and pumps bytes until the child exits or stdin closes.
//
// On exit — successful or not — the local terminal state is fully
// restored. Even if the connection dies mid-session, the client
// re-emits the standard terminal-mode-disable escape sequences so
// iTerm2 doesn't end up wedged with focus reporting / bracketed paste
// / alternate-screen left enabled.
//
// Returns the child's exit code, or a non-nil error if the session
// failed before a clean exit. The exit code is meaningful only when
// err == nil.
func Run(ctx context.Context, cfg Config) (exitCode int, err error) {
	conn, err := dialAgent(ctx, cfg.SocketPath, cfg.ConnectPort)
	if err != nil {
		return -1, err
	}
	defer conn.Close()

	// Initial winsize from the local tty (ours, not the agent's).
	// Falls back to 24x80 if stdin isn't a tty (CI runs, pipes).
	rows, cols := termSize(os.Stdin)
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}

	// Local TTY → raw mode BEFORE the handshake, with deferred restore.
	// The handshake makes the agent exec the child immediately, so any
	// terminal query the child fires on startup (cursor-position report,
	// etc.) must find the local tty already raw — otherwise the reply
	// sits in the cooked line-discipline buffer and the child renders
	// against stale assumptions. This matches `ssh -t`, which goes raw
	// before the remote produces output. Restore always runs, even if
	// the connection dies mid-session, so iTerm2 never wedges with
	// focus reporting / bracketed paste / alternate-screen left on.
	restore, err := enterRawMode(os.Stdin)
	if err != nil {
		return -1, fmt.Errorf("vsockclient: raw mode: %w", err)
	}
	// restore reads the named return `err` at defer time. A clean exit
	// (err == nil) restores termios and the idempotent input-mode disables
	// only. An abnormal exit — connection dropped, read error — additionally
	// leaves the alternate screen, to rescue a TUI that died mid-draw before
	// it could restore the screen itself.
	defer func() { restore(err == nil) }()

	// TUI attach: clear the screen before the child draws so its TUI
	// starts clean instead of overdrawing the CLI's prior output (see
	// Config.ClearScreen). Shells and one-shot execs leave this off.
	if cfg.ClearScreen && term.IsTerminal(int(os.Stdout.Fd())) {
		_, _ = os.Stdout.WriteString("\x1b[2J\x1b[H")
	}

	hsBytes, err := vsockproto.MarshalHandshake(vsockproto.Handshake{
		Cmd:  cfg.Cmd,
		Args: cfg.Args,
		Env:  cfg.Env,
		Cwd:  cfg.Cwd,
		User: cfg.User,
		Term: os.Getenv("TERM"),
		Rows: rows,
		Cols: cols,
	})
	if err != nil {
		return -1, fmt.Errorf("vsockclient: marshal handshake: %w", err)
	}
	if err := vsockproto.WriteFrame(conn, vsockproto.FrameHandshake, hsBytes); err != nil {
		return -1, fmt.Errorf("vsockclient: send handshake: %w", err)
	}

	// Forward SIGWINCH so the guest PTY tracks our terminal size.
	winchCh := make(chan os.Signal, 1)
	signal.Notify(winchCh, syscall.SIGWINCH)
	defer signal.Stop(winchCh)

	winchCtx, cancelWinch := context.WithCancel(ctx)
	defer cancelWinch()
	go forwardWinch(winchCtx, conn, winchCh)

	// Restore the local terminal on a killing signal, too. The deferred
	// restore above only runs on a normal return — a SIGTERM (kill), SIGHUP
	// (terminal window/tab closed), or SIGQUIT would otherwise leave the tty
	// in raw mode. Catch those, run the same idempotent restore (shared with
	// the defer via sync.Once), then reset the signal to its default and
	// re-raise it: we clean up the terminal on the way out, we don't swallow
	// the signal — the process still dies with the signal's own semantics.
	//
	// The guest child needs no signal forwarding here: when this process
	// exits the OS closes our vsock socket, and the agent tears the child
	// down on the closed connection (see internal/agentembed main loop).
	killCh := make(chan os.Signal, 1)
	signal.Notify(killCh, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(killCh)
	killDone := make(chan struct{})
	defer close(killDone)
	go func() {
		select {
		case s := <-killCh:
			restore(false) // abnormal: also leave the alt screen (may be mid-TUI)
			signal.Reset(s)
			if sig, ok := s.(syscall.Signal); ok {
				_ = syscall.Kill(os.Getpid(), sig)
			}
		case <-killDone:
		}
	}()

	// stdin → conn pump. A closed stdin (^D) does NOT end the session
	// — only the child exiting does. The pump returns silently when
	// stdin closes and we wait for FrameExit from the agent.
	stdinDone := make(chan struct{})
	go func() {
		defer close(stdinDone)
		buf := make([]byte, 32*1024)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				if werr := vsockproto.WriteFrame(conn, vsockproto.FrameData, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// conn → stdout pump (the main goroutine).
	exitCode = -1
	for {
		t, payload, err := vsockproto.ReadFrame(conn)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				// Agent went away without a FrameExit. Surface that —
				// callers want to distinguish "child exited cleanly
				// with code X" from "transport died".
				return -1, fmt.Errorf("vsockclient: agent disconnected before exit frame: %w", err)
			}
			return -1, fmt.Errorf("vsockclient: read frame: %w", err)
		}
		switch t {
		case vsockproto.FrameData:
			if _, werr := os.Stdout.Write(payload); werr != nil {
				return -1, fmt.Errorf("vsockclient: stdout write: %w", werr)
			}
		case vsockproto.FrameExit:
			code, derr := vsockproto.DecodeExit(payload)
			if derr != nil {
				return -1, fmt.Errorf("vsockclient: decode exit: %w", derr)
			}
			exitCode = int(code)
			// Drain stdin pump — best-effort, bounded so a wedged stdin
			// can't pin the process.
			select {
			case <-stdinDone:
			case <-time.After(50 * time.Millisecond):
			}
			return exitCode, nil
		default:
			// Unknown frames are ignored. The agent may add types in
			// future versions; old clients should not crash.
		}
	}
}

// dialAgent connects to the host-side Unix socket that reaches the guest
// pty-agent, retrying briefly to absorb the small race where the socket
// isn't bound yet right after boot.
//
// connectPort selects the transport shape. 0 (vz): the socket is already
// a per-guest-port channel — vzd's agent proxy bridges <vmDir>/agent.sock
// straight to guest vsock 1024 — so we speak the framed protocol
// immediately. Non-zero (firecracker): the UDS is firecracker's shared
// hybrid-vsock endpoint, so we first send "CONNECT <port>\n" and wait for
// its "OK ...\n" before the stream is wired through to the guest.
func dialAgent(ctx context.Context, sockPath string, connectPort uint32) (net.Conn, error) {
	deadline := time.Now().Add(DefaultDialTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	var lastErr error
	for time.Now().Before(deadline) {
		c, err := net.Dial("unix", sockPath)
		if err == nil {
			if connectPort == 0 {
				return c, nil
			}
			// Firecracker hybrid-vsock handshake. The agent may not be
			// accepting on the port yet just after boot, so a rejected
			// CONNECT is retryable like a failed dial.
			if herr := fcVsockConnect(c, connectPort); herr != nil {
				c.Close()
				lastErr = herr
			} else {
				return c, nil
			}
		} else {
			lastErr = err
			if isFatalDialErr(err) {
				break
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("%w: %w", ErrAgentUnavailable, lastErr)
}

// fcVsockConnect performs firecracker's hybrid-vsock CONNECT handshake on
// an already-dialed UDS: send "CONNECT <port>\n", expect "OK <hostport>\n".
// See https://github.com/firecracker-microvm/firecracker/blob/main/docs/vsock.md
func fcVsockConnect(c net.Conn, port uint32) error {
	if _, err := fmt.Fprintf(c, "CONNECT %d\n", port); err != nil {
		return fmt.Errorf("vsockclient: vsock CONNECT: %w", err)
	}
	if err := c.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	buf := make([]byte, 32)
	n, err := c.Read(buf)
	if err != nil {
		return fmt.Errorf("vsockclient: vsock CONNECT response: %w", err)
	}
	if !strings.HasPrefix(string(buf[:n]), "OK ") {
		return fmt.Errorf("vsockclient: vsock CONNECT rejected: %q",
			strings.TrimSpace(string(buf[:n])))
	}
	// Clear the deadline so the framed protocol that follows isn't bound by it.
	return c.SetReadDeadline(time.Time{})
}

// isFatalDialErr distinguishes "agent isn't there" (recoverable, retry)
// from "agent rejected us" (don't retry, fall through to caller).
func isFatalDialErr(err error) bool {
	if err == nil {
		return false
	}
	// Non-existent socket file → ENOENT. This is retryable when the VM
	// is still booting; not retryable when the agent isn't installed.
	// We only have one signal to go on, so retry until the deadline.
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return false
	}
	return true
}

// forwardWinch sends a winsize frame on every SIGWINCH, plus one at
// startup so the agent picks up the local size before the user
// resizes the window.
func forwardWinch(ctx context.Context, conn io.Writer, ch <-chan os.Signal) {
	send := func() {
		rows, cols := termSize(os.Stdin)
		if rows == 0 {
			return
		}
		_ = vsockproto.WriteFrame(conn, vsockproto.FrameWinsize, vsockproto.EncodeWinsize(rows, cols))
	}
	send()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ch:
			if !ok {
				return
			}
			send()
		}
	}
}

// termSize reads the current local terminal size. Returns (0, 0) if
// the file isn't a TTY (e.g., piped input in CI).
func termSize(f *os.File) (rows, cols uint16) {
	if !term.IsTerminal(int(f.Fd())) {
		return 0, 0
	}
	w, h, err := term.GetSize(int(f.Fd()))
	if err != nil {
		return 0, 0
	}
	// term.GetSize returns (cols, rows). Map to our rows/cols order.
	return uint16(h), uint16(w)
}

// enterRawMode puts f's terminal into raw mode and returns a cleanup
// function that restores the previous state and re-emits the escapes that
// disable terminal modes Claude Code (or any other TUI) might have left
// enabled. The TUI inside the guest set those modes on the local terminal
// via its own writes — they live in the terminal's state, not in our
// termios — so restoring termios (canonical line discipline for typing) and
// re-emitting the escapes (the terminal's visual state) are both needed.
//
// The cleanup takes cleanExit: on a clean exit it emits only the idempotent
// input-mode disables (emitInputModeReset); on an abnormal exit it also
// leaves the alternate screen (emitLeaveAltScreen). See those functions for
// why the alt-screen leave must NOT run on a clean exit — it is the cause of
// the "screen clears / characters mixed" glitch when exiting a shell.
//
// The cleanup is idempotent and safe to call from a deferred handler in a
// panic path.
func enterRawMode(f *os.File) (func(cleanExit bool), error) {
	fd := int(f.Fd())
	if !term.IsTerminal(fd) {
		// Not a tty (piped stdin, CI): we never went raw and there is no
		// local terminal state to restore. Writing escapes here would only
		// risk corrupting captured output, so do nothing.
		return func(bool) {}, nil
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	var once sync.Once
	return func(cleanExit bool) {
		once.Do(func() {
			_ = term.Restore(fd, state)
			emitInputModeReset()
			if !cleanExit {
				emitLeaveAltScreen()
			}
		})
	}, nil
}

// inputModeResetSeq disables the input-related terminal modes a TUI inside
// the guest might have left on. Every code is a mode *disable* or "show
// cursor" — none move the cursor or switch screen buffers — so the whole
// string is fully idempotent and safe to emit on any exit:
//
//	?1004l   focus reporting off
//	?2004l   bracketed paste off
//	?25h     cursor visible
//	?1000l   mouse tracking off (basic)
//	?1002l   mouse tracking off (cell motion)
//	?1003l   mouse tracking off (any motion)
//	?1006l   mouse tracking off (SGR extended)
//	>4m      modifyOtherKeys off — claude turns this ON so it can distinguish
//	         Shift+Enter; without turning it OFF the user's bare keystrokes
//	         come back as `\e[27;2;XX~`-style CSI garbage at the shell prompt
//	<u       kitty keyboard protocol disable (no-op where unsupported)
//
// Deliberately absent: ?1049l (leave alt screen) — see leaveAltScreenSeq.
const inputModeResetSeq = "\x1b[?1004l" +
	"\x1b[?2004l" +
	"\x1b[?25h" +
	"\x1b[?1000l" +
	"\x1b[?1002l" +
	"\x1b[?1003l" +
	"\x1b[?1006l" +
	"\x1b[>4m" +
	"\x1b[<u"

// leaveAltScreenSeq is ?1049l: "use normal screen buffer and restore cursor".
// Unlike the input-mode disables it is NOT idempotent — the cursor-restore
// fires DECRC, so on a terminal that never entered the alt screen (a plain
// shell) or a child that already left it (a clean TUI exit) it drags the
// cursor to home and the next output overprints the visible screen. That is
// the "screen clears / characters mixed" glitch on exiting `clawk run shell`.
const leaveAltScreenSeq = "\x1b[?1049l"

// emitInputModeReset writes inputModeResetSeq to stderr (seen regardless of
// stdout redirection). Runs on every Run() exit: cheap, idempotent, and the
// only thing standing between the user and the "focus reporting left on" /
// "every keystroke is a CSI 27;mod;X~" input wedge when a connection dies.
func emitInputModeReset() {
	_, _ = os.Stderr.WriteString(inputModeResetSeq)
}

// emitLeaveAltScreen writes leaveAltScreenSeq to stderr. Emitted ONLY on an
// abnormal exit: a TUI (claude) that died mid-draw — connection dropped,
// read error — never left the alt screen itself, and without this the
// terminal stays stuck showing its frozen frame. On a clean exit the child
// has already restored its screen, so forcing the buffer switch would only
// reintroduce the cursor-jump glitch (see leaveAltScreenSeq).
func emitLeaveAltScreen() {
	_, _ = os.Stderr.WriteString(leaveAltScreenSeq)
}
