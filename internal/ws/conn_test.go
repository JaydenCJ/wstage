// Conn tests over net.Pipe: message round trips, fragmentation, control
// frame handling, masking-direction enforcement, and the closing handshake
// — the behaviors the record and replay engines depend on.
package ws

import (
	"bytes"
	"errors"
	"net"
	"testing"
	"time"
)

// ignoreCloseError filters the expected *CloseError out of drain reads.
func ignoreCloseError(err error) error {
	var ce *CloseError
	if errors.As(err, &ce) {
		return nil
	}
	return err
}

// pipePair returns a connected client/server Conn pair over an in-memory
// synchronous transport.
func pipePair(t *testing.T) (client, server *Conn) {
	t.Helper()
	c, s := net.Pipe()
	t.Cleanup(func() { c.Close(); s.Close() })
	return NewConn(c, true), NewConn(s, false)
}

// send runs fn in a goroutine so the synchronous pipe cannot deadlock, and
// fails the test if fn errors.
func send(t *testing.T, fn func() error) {
	t.Helper()
	go func() {
		if err := fn(); err != nil {
			t.Errorf("send: %v", err)
		}
	}()
}

func TestTextMessageRoundTripsClientToServer(t *testing.T) {
	client, server := pipePair(t)
	send(t, func() error { return client.WriteMessage(OpText, []byte("hello over the wire")) })
	msg, err := server.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if msg.Type != OpText || string(msg.Data) != "hello over the wire" {
		t.Fatalf("got %+v", msg)
	}
}

func TestBinaryMessageRoundTripsServerToClient(t *testing.T) {
	client, server := pipePair(t)
	payload := []byte{0x00, 0xFF, 0x10, 0x20}
	send(t, func() error { return server.WriteMessage(OpBinary, payload) })
	msg, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if msg.Type != OpBinary || !bytes.Equal(msg.Data, payload) {
		t.Fatalf("got %+v", msg)
	}
}

func TestFragmentedMessageIsReassembled(t *testing.T) {
	c, s := net.Pipe()
	t.Cleanup(func() { c.Close(); s.Close() })
	server := NewConn(s, false)
	key := [4]byte{1, 2, 3, 4}
	send(t, func() error {
		if err := WriteFrame(c, Frame{Opcode: OpText, Masked: true, MaskKey: key, Payload: []byte("frag")}); err != nil {
			return err
		}
		if err := WriteFrame(c, Frame{Opcode: OpContinuation, Masked: true, MaskKey: key, Payload: []byte("ment")}); err != nil {
			return err
		}
		return WriteFrame(c, Frame{Fin: true, Opcode: OpContinuation, Masked: true, MaskKey: key, Payload: []byte("ed")})
	})
	msg, err := server.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if string(msg.Data) != "fragmented" {
		t.Fatalf("reassembly produced %q", msg.Data)
	}
}

func TestControlFrameInterleavedInsideFragmentedMessage(t *testing.T) {
	// RFC 6455 allows pings between the fragments of a data message.
	c, s := net.Pipe()
	t.Cleanup(func() { c.Close(); s.Close() })
	server := NewConn(s, false)
	key := [4]byte{7, 7, 7, 7}
	send(t, func() error {
		if err := WriteFrame(c, Frame{Opcode: OpText, Masked: true, MaskKey: key, Payload: []byte("ha")}); err != nil {
			return err
		}
		if err := WriteFrame(c, Frame{Fin: true, Opcode: OpPing, Masked: true, MaskKey: key, Payload: []byte("k")}); err != nil {
			return err
		}
		// Consume the server's automatic pong so the pipe does not block.
		if _, err := ReadFrame(c, 0); err != nil {
			return err
		}
		return WriteFrame(c, Frame{Fin: true, Opcode: OpContinuation, Masked: true, MaskKey: key, Payload: []byte("lf")})
	})
	msg, err := server.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if string(msg.Data) != "half" {
		t.Fatalf("got %q", msg.Data)
	}
}

func TestMaskingDirectionIsEnforcedBothWays(t *testing.T) {
	// A server must reject unmasked client frames…
	c, s := net.Pipe()
	t.Cleanup(func() { c.Close(); s.Close() })
	server := NewConn(s, false)
	send(t, func() error { return WriteFrame(c, Frame{Fin: true, Opcode: OpText, Payload: []byte("bare")}) })
	_, err := server.ReadMessage()
	assertProtocolError(t, err, "unmasked")

	// …and a client must reject masked server frames.
	c2, s2 := net.Pipe()
	t.Cleanup(func() { c2.Close(); s2.Close() })
	client := NewConn(c2, true)
	send(t, func() error {
		return WriteFrame(s2, Frame{Fin: true, Opcode: OpText, Masked: true, MaskKey: [4]byte{1, 1, 1, 1}, Payload: []byte("x")})
	})
	_, err = client.ReadMessage()
	assertProtocolError(t, err, "masked")
}

func TestPingIsAutomaticallyAnsweredWithMatchingPong(t *testing.T) {
	c, s := net.Pipe()
	t.Cleanup(func() { c.Close(); s.Close() })
	server := NewConn(s, false)
	key := [4]byte{5, 6, 7, 8}
	done := make(chan Frame, 1)
	send(t, func() error {
		if err := WriteFrame(c, Frame{Fin: true, Opcode: OpPing, Masked: true, MaskKey: key, Payload: []byte("beat")}); err != nil {
			return err
		}
		pong, err := ReadFrame(c, 0)
		if err != nil {
			return err
		}
		done <- pong
		// Follow with a data message so ReadMessage returns.
		return WriteFrame(c, Frame{Fin: true, Opcode: OpText, Masked: true, MaskKey: key, Payload: []byte("hi")})
	})
	if _, err := server.ReadMessage(); err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	pong := <-done
	if pong.Opcode != OpPong || string(pong.Payload) != "beat" {
		t.Fatalf("want pong echoing %q, got %+v", "beat", pong)
	}
}

func TestUnsolicitedPongsAreSkippedAndASecondWriteCloseIsANoOp(t *testing.T) {
	client, server := pipePair(t)
	send(t, func() error {
		// Unsolicited pongs must be absorbed silently…
		if err := client.WritePong([]byte("nobody asked")); err != nil {
			return err
		}
		if err := client.WriteMessage(OpText, []byte("real message")); err != nil {
			return err
		}
		// …and only the first close frame may hit the wire: the peer
		// reads exactly one close, carrying the first reason.
		if err := client.WriteClose(1000, "first"); err != nil {
			return err
		}
		if err := client.WriteClose(1000, "second"); err != nil {
			return err
		}
		_, err := client.ReadMessage() // drain the server's echo
		return ignoreCloseError(err)
	})
	msg, err := server.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if string(msg.Data) != "real message" {
		t.Fatalf("got %q", msg.Data)
	}
	_, err = server.ReadMessage()
	var ce *CloseError
	if !errors.As(err, &ce) || ce.Reason != "first" {
		t.Fatalf("got %v", err)
	}
}

func TestCloseHandshakeDeliversCodeAndReasonBothWays(t *testing.T) {
	client, server := pipePair(t)
	echoed := make(chan error, 1)
	send(t, func() error {
		if err := client.WriteClose(1001, "going away"); err != nil {
			return err
		}
		_, err := client.ReadMessage() // the server's echo
		echoed <- err
		return nil
	})
	_, err := server.ReadMessage()
	var ce *CloseError
	if !errors.As(err, &ce) {
		t.Fatalf("want CloseError, got %v", err)
	}
	if ce.Code != 1001 || ce.Reason != "going away" {
		t.Fatalf("got %+v", ce)
	}
	var echo *CloseError
	if err := <-echoed; !errors.As(err, &echo) || echo.Code != 1001 {
		t.Fatalf("initiator did not get a matching echo: %v", err)
	}
}

func TestCloseWithoutStatusCodeMapsTo1005(t *testing.T) {
	c, s := net.Pipe()
	t.Cleanup(func() { c.Close(); s.Close() })
	server := NewConn(s, false)
	send(t, func() error {
		if err := WriteFrame(c, Frame{Fin: true, Opcode: OpClose, Masked: true, MaskKey: [4]byte{2, 2, 2, 2}}); err != nil {
			return err
		}
		_, err := ReadFrame(c, 0) // drain the echo
		return err
	})
	_, err := server.ReadMessage()
	var ce *CloseError
	if !errors.As(err, &ce) || ce.Code != 1005 {
		t.Fatalf("want code 1005, got %v", err)
	}
}

func TestInvalidUTF8TextMessageIsRejected(t *testing.T) {
	c, s := net.Pipe()
	t.Cleanup(func() { c.Close(); s.Close() })
	server := NewConn(s, false)
	send(t, func() error {
		return WriteFrame(c, Frame{Fin: true, Opcode: OpText, Masked: true, MaskKey: [4]byte{3, 3, 3, 3}, Payload: []byte{0xFF, 0xFE}})
	})
	_, err := server.ReadMessage()
	assertProtocolError(t, err, "UTF-8")
}

func TestFragmentationStateMachineRejectsOutOfOrderFrames(t *testing.T) {
	// A continuation with no message started is a protocol error…
	c, s := net.Pipe()
	t.Cleanup(func() { c.Close(); s.Close() })
	server := NewConn(s, false)
	send(t, func() error {
		return WriteFrame(c, Frame{Fin: true, Opcode: OpContinuation, Masked: true, MaskKey: [4]byte{4, 4, 4, 4}, Payload: []byte("orphan")})
	})
	_, err := server.ReadMessage()
	assertProtocolError(t, err, "continuation")

	// …and so is a fresh data opcode while a fragmented message is open.
	c2, s2 := net.Pipe()
	t.Cleanup(func() { c2.Close(); s2.Close() })
	server2 := NewConn(s2, false)
	key := [4]byte{6, 6, 6, 6}
	send(t, func() error {
		if err := WriteFrame(c2, Frame{Opcode: OpText, Masked: true, MaskKey: key, Payload: []byte("open")}); err != nil {
			return err
		}
		return WriteFrame(c2, Frame{Fin: true, Opcode: OpText, Masked: true, MaskKey: key, Payload: []byte("barge-in")})
	})
	_, err = server2.ReadMessage()
	assertProtocolError(t, err, "middle of a fragmented message")
}

func TestMaxMessageIsEnforcedAcrossFragments(t *testing.T) {
	c, s := net.Pipe()
	t.Cleanup(func() { c.Close(); s.Close() })
	server := NewConn(s, false)
	server.MaxMessage = 10
	key := [4]byte{8, 8, 8, 8}
	send(t, func() error {
		if err := WriteFrame(c, Frame{Opcode: OpBinary, Masked: true, MaskKey: key, Payload: make([]byte, 8)}); err != nil {
			return err
		}
		return WriteFrame(c, Frame{Fin: true, Opcode: OpContinuation, Masked: true, MaskKey: key, Payload: make([]byte, 8)})
	})
	_, err := server.ReadMessage()
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
}

func TestSetReadDeadlineUnblocksAStuckRead(t *testing.T) {
	// Guards the engine's --timeout plumbing without any sleeps: the
	// deadline is in the past, so the read fails immediately.
	_, server := pipePair(t)
	if err := server.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	_, err := server.ReadMessage()
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("want a timeout error, got %v", err)
	}
}
