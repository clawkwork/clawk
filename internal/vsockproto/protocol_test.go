package vsockproto

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFrameRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		typ     FrameType
		payload []byte
	}{
		{"empty data", FrameData, nil},
		{"small data", FrameData, []byte("hello")},
		{"max-size data", FrameData, bytes.Repeat([]byte("x"), MaxPayloadBytes)},
		{"winsize", FrameWinsize, EncodeWinsize(50, 200)},
		{"exit", FrameExit, EncodeExit(0)},
		{"exit nonzero", FrameExit, EncodeExit(137)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			require.NoError(t, WriteFrame(&buf, tc.typ, tc.payload), "WriteFrame")
			gotType, gotPayload, err := ReadFrame(&buf)
			require.NoError(t, err, "ReadFrame")
			require.Equal(t, tc.typ, gotType)
			require.True(t, bytes.Equal(gotPayload, tc.payload), "payload: got %d bytes, want %d", len(gotPayload), len(tc.payload))
		})
	}
}

func TestFrameTooLarge(t *testing.T) {
	var buf bytes.Buffer
	huge := bytes.Repeat([]byte("x"), MaxPayloadBytes+1)
	require.True(t, errors.Is(WriteFrame(&buf, FrameData, huge), ErrFrameTooLarge), "WriteFrame: expected ErrFrameTooLarge")
	// And on the read side: a header announcing too much should fail.
	buf.Reset()
	// Type=FrameData, length = MaxPayloadBytes+1. Hand-construct.
	buf.WriteByte(byte(FrameData))
	n := uint32(MaxPayloadBytes + 1)
	buf.WriteByte(byte(n >> 24))
	buf.WriteByte(byte(n >> 16))
	buf.WriteByte(byte(n >> 8))
	buf.WriteByte(byte(n))
	_, _, err := ReadFrame(&buf)
	require.True(t, errors.Is(err, ErrFrameTooLarge), "ReadFrame: expected ErrFrameTooLarge, got %v", err)
}

func TestReadFrameCleanEOF(t *testing.T) {
	var buf bytes.Buffer
	_, _, err := ReadFrame(&buf)
	require.True(t, errors.Is(err, io.EOF), "expected io.EOF on empty stream, got %v", err)
}

func TestReadFrameTruncated(t *testing.T) {
	var buf bytes.Buffer
	// Write a valid header announcing 100 bytes then close after 50.
	buf.WriteByte(byte(FrameData))
	buf.Write([]byte{0, 0, 0, 100})
	buf.Write(bytes.Repeat([]byte("x"), 50))
	_, _, err := ReadFrame(&buf)
	require.True(t, errors.Is(err, io.ErrUnexpectedEOF), "expected io.ErrUnexpectedEOF, got %v", err)
}

func TestHandshakeRoundTrip(t *testing.T) {
	h := Handshake{
		Cmd:  "/usr/bin/claude",
		Args: []string{"--dangerously-skip-permissions"},
		Env:  []string{"TERM=xterm-256color"},
		Cwd:  "/home/agent/workspace/wt",
		User: "agent",
		Term: "xterm-256color",
		Rows: 50, Cols: 200,
	}
	b, err := MarshalHandshake(h)
	require.NoError(t, err, "MarshalHandshake")
	got, err := UnmarshalHandshake(b)
	require.NoError(t, err, "UnmarshalHandshake")
	require.Equal(t, ProtoVersion, got.ProtoVersion)
	require.True(t, got.Cmd == h.Cmd && got.Cwd == h.Cwd && got.Rows == h.Rows, "handshake mismatch:\n got %+v\nwant %+v", got, h)
}

func TestWinsizeRoundTrip(t *testing.T) {
	rows, cols, err := DecodeWinsize(EncodeWinsize(40, 132))
	require.NoError(t, err, "DecodeWinsize")
	require.Equal(t, uint16(40), rows)
	require.Equal(t, uint16(132), cols)
}

func TestExitRoundTrip(t *testing.T) {
	got, err := DecodeExit(EncodeExit(-1))
	require.NoError(t, err, "DecodeExit")
	require.Equal(t, int32(-1), got)
}
