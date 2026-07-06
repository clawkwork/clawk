// Package vsockproto is the framed wire protocol shared by the Mac-side
// client (internal/vsockclient) and the in-guest agent (cmd/clawk-pty-agent).
//
// The transport is a single byte stream — on the guest it's an AF_VSOCK
// socket; on the Mac it's the daemon's Unix-socket proxy that pipes bytes
// to that vsock. Either way, the protocol is the same.
//
// Frames are length-prefixed:
//
//	[1 byte type][4 bytes payload length, big-endian][payload]
//
// The framing is deliberately tiny: no protobuf, no JSON for control plane,
// just enough structure to multiplex pty-data with out-of-band signals
// (window-size changes, exit code) on one connection. Handshake is the only
// JSON-shaped frame and it only goes one way.
package vsockproto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// AgentVSockPort is the AF_VSOCK port the in-guest agent listens on.
// Fixed: every sandbox runs the same agent, so a static port keeps the
// host side dial-only with no discovery dance.
const AgentVSockPort uint32 = 1024

// FrameType tags each frame on the wire. Stable: never reuse a number.
type FrameType byte

const (
	// FrameHandshake is the first frame the client sends. Payload is JSON
	// (Handshake struct). Agent never sends this back; agent acknowledges
	// implicitly by streaming data.
	FrameHandshake FrameType = 0x01

	// FrameData carries raw PTY bytes. Bidirectional.
	FrameData FrameType = 0x02

	// FrameWinsize updates the PTY window size. Client → agent only.
	// Payload is exactly 4 bytes: rows uint16 BE, cols uint16 BE.
	FrameWinsize FrameType = 0x03

	// FrameExit is the agent's last frame before closing the connection.
	// Payload is exactly 4 bytes: exit code int32 BE. Client → agent
	// direction is not used.
	FrameExit FrameType = 0x04

	// FrameSignal carries a Linux signal number from client to agent.
	// Payload is 4 bytes: signal number int32 BE. Used to forward Ctrl+C
	// etc. that the local TTY can't deliver because we're in raw mode.
	// Reserved for v2 — current clients don't emit this; the in-PTY
	// keystrokes deliver INTR via the line discipline already.
	FrameSignal FrameType = 0x05
)

// Handshake is the JSON payload of the first frame.
type Handshake struct {
	// ProtoVersion is the wire protocol version. Bumped on
	// breaking changes. Servers refuse mismatched versions.
	ProtoVersion uint32 `json:"v"`

	// Cmd is the program to exec inside the PTY. If empty, the agent
	// runs the GuestUser's login shell.
	Cmd string `json:"cmd,omitempty"`

	// Args are the args to Cmd. Cmd[0] convention does NOT apply —
	// the agent calls exec.Cmd{Path: Cmd, Args: append([]string{Cmd}, Args...)}.
	Args []string `json:"args,omitempty"`

	// Env is extra environment for the child. Merged on top of the
	// agent's environment (the agent itself runs from systemd with a
	// minimal env; the merged result is what the child sees).
	Env []string `json:"env,omitempty"`

	// Cwd, if non-empty, is the child's working directory.
	Cwd string `json:"cwd,omitempty"`

	// User, if non-empty, is the OS user to drop to (by name). The
	// agent runs as root (systemd unit), so dropping privileges is its
	// job, not the client's.
	User string `json:"user,omitempty"`

	// Term is TERM. Defaults to xterm-256color if empty.
	Term string `json:"term,omitempty"`

	// Rows is the initial PTY row count. If zero, the agent uses 24
	// (the kernel's default). Cols is its companion.
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`

	// Distinguisher disambiguates concurrent sessions in the same
	// (user, cwd, cmd). The agent folds it into the abduco session key
	// so two terminals running the same command land on independent
	// sessions, while the same terminal across a sleep/wake cycle
	// keeps reattaching.
	//
	// The host typically derives this from os.Getppid (the parent
	// shell PID) — stable per terminal tab — and falls back to a
	// random nonce when stdin isn't a tty (CI / pipe), so
	// non-interactive invocations don't alias into the same session.
	Distinguisher string `json:"distinguisher,omitempty"`
}

// ProtoVersion is the current protocol version.
//
// Compatibility policy — guest agents are baked into sandbox disks at
// create time and are never updated in place, so version skew is normal:
// a user upgrades the host binary and their existing sandboxes keep the
// old agent. Three rules keep that survivable:
//
//  1. Additive changes (new optional Handshake fields, new frame types
//     the peer may ignore) do NOT bump ProtoVersion. JSON decoding is
//     lenient by design.
//  2. A breaking change bumps ProtoVersion, and the in-guest agent keeps
//     accepting [MinSupportedProtoVersion, ProtoVersion] — never a
//     single value — so the NEXT skew has a window.
//  3. The host refuses to attach to a sandbox whose recorded guest ABI
//     (config.Sandbox.GuestABI) is older than it supports, with an error
//     telling the user to recreate the sandbox — see
//     internal/sandbox.CheckGuestABI. The in-guest check is the
//     backstop, not the primary UX.
const ProtoVersion uint32 = 1

// MinSupportedProtoVersion is the oldest peer version the current
// in-guest agent still accepts. Kept in lock-step with the agent source
// in internal/agentembed/main.go.in.
const MinSupportedProtoVersion uint32 = 1

// MaxPayloadBytes caps the size of a single frame's payload. PTY chunks
// are tiny (the kernel buffer is one page), so 64 KiB is generous enough
// to never split a read but small enough that a malicious peer can't
// allocate 4 GiB.
const MaxPayloadBytes = 64 * 1024

// frameHeaderSize is 1 byte type + 4 bytes length.
const frameHeaderSize = 5

// ErrFrameTooLarge is returned when a peer announces a payload longer
// than MaxPayloadBytes. Connections in this state must be closed.
var ErrFrameTooLarge = errors.New("vsockproto: frame payload exceeds MaxPayloadBytes")

// WriteFrame writes one framed message to w.
//
// The full header+payload is written in two calls — the kernel will
// usually coalesce these into one packet. We don't bother with a
// userspace bufio.Writer; PTY data is bursty and a Flush would land in
// the same place.
func WriteFrame(w io.Writer, t FrameType, payload []byte) error {
	if len(payload) > MaxPayloadBytes {
		return ErrFrameTooLarge
	}
	var hdr [frameHeaderSize]byte
	hdr[0] = byte(t)
	binary.BigEndian.PutUint32(hdr[1:5], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
}

// ReadFrame reads one framed message from r. The returned payload is
// freshly allocated on each call; callers may retain it.
//
// Returns io.EOF cleanly when r is closed at a frame boundary (header
// not yet started). A truncated header or payload returns io.ErrUnexpectedEOF.
func ReadFrame(r io.Reader) (FrameType, []byte, error) {
	var hdr [frameHeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	t := FrameType(hdr[0])
	n := binary.BigEndian.Uint32(hdr[1:5])
	if n > MaxPayloadBytes {
		return 0, nil, fmt.Errorf("%w: announced %d bytes", ErrFrameTooLarge, n)
	}
	if n == 0 {
		return t, nil, nil
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return t, payload, nil
}

// MarshalHandshake encodes a Handshake into the bytes that go into a
// FrameHandshake payload. Lives here (not as a method) so the agent and
// client both see the exact same JSON shape — drift between encoders is
// the most common protocol-mismatch bug.
func MarshalHandshake(h Handshake) ([]byte, error) {
	if h.ProtoVersion == 0 {
		h.ProtoVersion = ProtoVersion
	}
	return json.Marshal(h)
}

// UnmarshalHandshake decodes the Handshake payload received over the wire.
func UnmarshalHandshake(b []byte) (Handshake, error) {
	var h Handshake
	if err := json.Unmarshal(b, &h); err != nil {
		return Handshake{}, fmt.Errorf("vsockproto: handshake: %w", err)
	}
	return h, nil
}

// EncodeWinsize packs rows and cols into the 4-byte payload of a
// FrameWinsize frame.
func EncodeWinsize(rows, cols uint16) []byte {
	var b [4]byte
	binary.BigEndian.PutUint16(b[0:2], rows)
	binary.BigEndian.PutUint16(b[2:4], cols)
	return b[:]
}

// DecodeWinsize is the inverse of EncodeWinsize. Returns an error on a
// short payload — the alternative would be silently zero-padding, which
// confuses debugging far more than an explicit error.
func DecodeWinsize(b []byte) (rows, cols uint16, err error) {
	if len(b) < 4 {
		return 0, 0, fmt.Errorf("vsockproto: winsize payload short: %d", len(b))
	}
	rows = binary.BigEndian.Uint16(b[0:2])
	cols = binary.BigEndian.Uint16(b[2:4])
	return rows, cols, nil
}

// EncodeExit packs an exit code into the 4-byte payload of a FrameExit frame.
func EncodeExit(code int32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(code))
	return b[:]
}

// DecodeExit is the inverse of EncodeExit.
func DecodeExit(b []byte) (int32, error) {
	if len(b) < 4 {
		return 0, fmt.Errorf("vsockproto: exit payload short: %d", len(b))
	}
	return int32(binary.BigEndian.Uint32(b[:4])), nil
}
