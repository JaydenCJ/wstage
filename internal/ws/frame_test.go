// Frame codec tests: wire-level round trips, the RFC 6455 worked example,
// and the protocol-error edges (reserved bits, bad lengths, oversized
// control frames) that a fuzzing peer would hit first.
package ws

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func roundTrip(t *testing.T, f Frame) Frame {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteFrame(&buf, f); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	got, err := ReadFrame(&buf, 0)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	return got
}

func TestWriteThenReadTextFrameRoundTrips(t *testing.T) {
	got := roundTrip(t, Frame{Fin: true, Opcode: OpText, Payload: []byte("hello")})
	if !got.Fin || got.Opcode != OpText || string(got.Payload) != "hello" {
		t.Fatalf("round trip mangled the frame: %+v", got)
	}
}

func TestWriteThenReadMaskedFrameUnmasksWithoutMutatingCallerPayload(t *testing.T) {
	// Masking must happen on a copy: the replay engine reuses cassette
	// payloads across sessions.
	payload := []byte{1, 2, 3, 4, 5}
	f := Frame{Fin: true, Opcode: OpBinary, Masked: true, MaskKey: [4]byte{0x37, 0xfa, 0x21, 0x3d}, Payload: payload}
	got := roundTrip(t, f)
	if !got.Masked {
		t.Fatal("Masked flag lost")
	}
	if !bytes.Equal(got.Payload, []byte{1, 2, 3, 4, 5}) {
		t.Fatalf("payload not unmasked: %v", got.Payload)
	}
	if !bytes.Equal(payload, []byte{1, 2, 3, 4, 5}) {
		t.Fatalf("caller payload was mutated: %v", payload)
	}
}

func TestRFC6455MaskedHelloExampleDecodes(t *testing.T) {
	// The worked example from RFC 6455 §5.7: a masked "Hello".
	wire := []byte{0x81, 0x85, 0x37, 0xfa, 0x21, 0x3d, 0x7f, 0x9f, 0x4d, 0x51, 0x58}
	f, err := ReadFrame(bytes.NewReader(wire), 0)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if string(f.Payload) != "Hello" || f.Opcode != OpText || !f.Fin || !f.Masked {
		t.Fatalf("RFC example decoded wrong: %+v payload=%q", f, f.Payload)
	}
}

func TestExtendedLengthEncodingsRoundTrip(t *testing.T) {
	for _, n := range []int{300, 70000} { // the 126 and 127 length forms
		payload := bytes.Repeat([]byte{0xAB}, n)
		got := roundTrip(t, Frame{Fin: true, Opcode: OpBinary, Payload: payload})
		if !bytes.Equal(got.Payload, payload) {
			t.Fatalf("payload mismatch at %d bytes", n)
		}
	}
}

func TestReadFrameRejectsReservedBitsAndUnknownOpcodes(t *testing.T) {
	// RSV1 set without a negotiated extension is a protocol error.
	_, err := ReadFrame(bytes.NewReader([]byte{0x81 | 0x40, 0x01, 'x'}), 0)
	assertProtocolError(t, err, "RSV")
	// Opcode 0x3 is reserved.
	_, err = ReadFrame(bytes.NewReader([]byte{0x83, 0x00}), 0)
	assertProtocolError(t, err, "opcode")
}

func TestReadFrameRejectsMalformedControlFrames(t *testing.T) {
	// A ping without FIN (fragmented control frame).
	_, err := ReadFrame(bytes.NewReader([]byte{0x09, 0x00}), 0)
	assertProtocolError(t, err, "fragmented control")
	// A close with a 128-byte payload (control frames cap at 125).
	wire := append([]byte{0x88, 126, 0x00, 0x80}, make([]byte, 128)...)
	_, err = ReadFrame(bytes.NewReader(wire), 0)
	assertProtocolError(t, err, "control frame payload")
}

func TestReadFrameRejectsBadExtendedLengths(t *testing.T) {
	// 5 bytes encoded in the 16-bit form: not minimal.
	_, err := ReadFrame(bytes.NewReader([]byte{0x82, 126, 0x00, 0x05, 1, 2, 3, 4, 5}), 0)
	assertProtocolError(t, err, "minimally encoded")
	// 5 bytes encoded in the 64-bit form: not minimal either.
	_, err = ReadFrame(bytes.NewReader(append([]byte{0x82, 127, 0, 0, 0, 0, 0, 0, 0, 5}, 1, 2, 3, 4, 5)), 0)
	assertProtocolError(t, err, "minimally encoded")
	// A 64-bit length with the high bit set is forbidden outright.
	_, err = ReadFrame(bytes.NewReader([]byte{0x82, 127, 0x80, 0, 0, 0, 0, 0, 0, 0}), 0)
	assertProtocolError(t, err, "high bit")
}

func TestReadFrameEnforcesMaxPayload(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, Frame{Fin: true, Opcode: OpBinary, Payload: make([]byte, 200)}); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	_, err := ReadFrame(&buf, 100)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
}

func TestReadFrameReportsTruncatedInput(t *testing.T) {
	// A 1-byte stream dies in the header.
	if _, err := ReadFrame(bytes.NewReader([]byte{0x81}), 0); err == nil {
		t.Fatal("want an error for a 1-byte stream")
	}
	// A frame claiming 5 payload bytes but delivering 2 dies in the body.
	_, err := ReadFrame(bytes.NewReader([]byte{0x81, 0x05, 'h', 'i'}), 0)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("want io.ErrUnexpectedEOF, got %v", err)
	}
}

func TestMaskingIsAnInvolution(t *testing.T) {
	key := [4]byte{0x11, 0x22, 0x33, 0x44}
	p := []byte("mask me twice and I return")
	orig := append([]byte(nil), p...)
	maskBytes(p, key)
	if bytes.Equal(p, orig) {
		t.Fatal("masking with a non-zero key should change the bytes")
	}
	maskBytes(p, key)
	if !bytes.Equal(p, orig) {
		t.Fatalf("double masking is not identity: %q", p)
	}
}

func assertProtocolError(t *testing.T, err error, substr string) {
	t.Helper()
	var pe ProtocolError
	if !errors.As(err, &pe) {
		t.Fatalf("want ProtocolError containing %q, got %v", substr, err)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Fatalf("error %q does not mention %q", err, substr)
	}
}
