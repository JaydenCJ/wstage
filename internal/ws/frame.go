// Package ws is a minimal, dependency-free implementation of the WebSocket
// wire protocol (RFC 6455): frame codec, masking, fragmentation, control
// frames, and the HTTP/1.1 upgrade handshake. It implements exactly what
// wstage needs to record and replay sessions; protocol extensions
// (permessage-deflate) and wss:// (TLS) are out of scope for v0.1.0.
package ws

import (
	"encoding/binary"
	"errors"
	"io"
)

// Opcodes defined by RFC 6455 §5.2.
const (
	OpContinuation byte = 0x0
	OpText         byte = 0x1
	OpBinary       byte = 0x2
	OpClose        byte = 0x8
	OpPing         byte = 0x9
	OpPong         byte = 0xA
)

// ProtocolError reports a violation of RFC 6455 by the peer (or by a
// malformed hand-crafted frame in tests).
type ProtocolError string

func (e ProtocolError) Error() string { return "ws: protocol error: " + string(e) }

// ErrTooLarge is returned when a frame or reassembled message exceeds the
// configured size limit. It is a local policy error, not a peer violation.
var ErrTooLarge = errors.New("ws: message exceeds the configured size limit")

// Frame is one wire frame, with the payload already unmasked on read.
type Frame struct {
	Fin     bool
	Opcode  byte
	Masked  bool
	MaskKey [4]byte
	Payload []byte
}

// IsControl reports whether op is a control opcode (close, ping, pong).
func IsControl(op byte) bool { return op >= OpClose }

func validOpcode(op byte) bool {
	switch op {
	case OpContinuation, OpText, OpBinary, OpClose, OpPing, OpPong:
		return true
	}
	return false
}

// maskBytes XORs p in place with the 4-byte mask key. Masking is an
// involution, so the same function both masks and unmasks.
func maskBytes(p []byte, key [4]byte) {
	for i := range p {
		p[i] ^= key[i%4]
	}
}

// ReadFrame parses a single frame from r. maxPayload caps the payload
// length of one frame (0 means no cap). The returned payload is unmasked;
// Masked records whether the peer masked it, so callers can enforce the
// client-masks / server-does-not rule.
func ReadFrame(r io.Reader, maxPayload int64) (Frame, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Frame{}, err
	}
	f := Frame{
		Fin:    hdr[0]&0x80 != 0,
		Opcode: hdr[0] & 0x0F,
		Masked: hdr[1]&0x80 != 0,
	}
	if hdr[0]&0x70 != 0 {
		return Frame{}, ProtocolError("non-zero RSV bits (extensions are not negotiated)")
	}
	if !validOpcode(f.Opcode) {
		return Frame{}, ProtocolError("unknown opcode")
	}

	n := int64(hdr[1] & 0x7F)
	switch n {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return Frame{}, err
		}
		n = int64(binary.BigEndian.Uint16(ext[:]))
		if n < 126 {
			return Frame{}, ProtocolError("length not minimally encoded (16-bit form)")
		}
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return Frame{}, err
		}
		u := binary.BigEndian.Uint64(ext[:])
		if u&(1<<63) != 0 {
			return Frame{}, ProtocolError("64-bit length has the high bit set")
		}
		if u < 65536 {
			return Frame{}, ProtocolError("length not minimally encoded (64-bit form)")
		}
		n = int64(u)
	}

	if IsControl(f.Opcode) {
		if !f.Fin {
			return Frame{}, ProtocolError("fragmented control frame")
		}
		if n > 125 {
			return Frame{}, ProtocolError("control frame payload exceeds 125 bytes")
		}
	}
	if maxPayload > 0 && n > maxPayload {
		return Frame{}, ErrTooLarge
	}

	if f.Masked {
		if _, err := io.ReadFull(r, f.MaskKey[:]); err != nil {
			return Frame{}, err
		}
	}
	f.Payload = make([]byte, n)
	if _, err := io.ReadFull(r, f.Payload); err != nil {
		return Frame{}, err
	}
	if f.Masked {
		maskBytes(f.Payload, f.MaskKey)
	}
	return f, nil
}

// WriteFrame encodes f to w as a single Write call. If f.Masked is set the
// payload is masked with f.MaskKey on the wire; f.Payload itself is not
// modified.
func WriteFrame(w io.Writer, f Frame) error {
	if !validOpcode(f.Opcode) {
		return ProtocolError("attempt to write an unknown opcode")
	}
	n := len(f.Payload)
	buf := make([]byte, 0, 14+n)

	b0 := f.Opcode
	if f.Fin {
		b0 |= 0x80
	}
	buf = append(buf, b0)

	var b1 byte
	if f.Masked {
		b1 = 0x80
	}
	switch {
	case n < 126:
		buf = append(buf, b1|byte(n))
	case n < 65536:
		buf = append(buf, b1|126)
		buf = binary.BigEndian.AppendUint16(buf, uint16(n))
	default:
		buf = append(buf, b1|127)
		buf = binary.BigEndian.AppendUint64(buf, uint64(n))
	}

	if f.Masked {
		buf = append(buf, f.MaskKey[:]...)
		start := len(buf)
		buf = append(buf, f.Payload...)
		maskBytes(buf[start:], f.MaskKey)
	} else {
		buf = append(buf, f.Payload...)
	}

	_, err := w.Write(buf)
	return err
}
