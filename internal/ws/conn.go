package ws

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"time"
	"unicode/utf8"
)

// DefaultMaxMessage is the default cap on a reassembled message (16 MiB).
const DefaultMaxMessage = 16 << 20

// Message is one complete (reassembled) data message.
type Message struct {
	Type byte // OpText or OpBinary
	Data []byte
}

// CloseError is returned by ReadMessage when the peer sends a close frame.
// Code 1005 means the peer sent a close frame with no status code.
type CloseError struct {
	Code   int
	Reason string
}

func (e *CloseError) Error() string {
	return fmt.Sprintf("ws: peer closed the connection: code %d %q", e.Code, e.Reason)
}

// Conn is a message-oriented WebSocket connection on top of any
// io.ReadWriteCloser. A client Conn masks outgoing frames and requires
// unmasked incoming frames; a server Conn does the opposite, per RFC 6455.
type Conn struct {
	raw    io.ReadWriteCloser
	br     *bufio.Reader
	client bool

	// MaxMessage caps the size of a reassembled message (0 = unlimited).
	MaxMessage int64

	wmu       sync.Mutex
	closeSent bool
}

// NewConn wraps an already-handshaken transport. client selects which side
// of the masking rule this Conn plays.
func NewConn(rw io.ReadWriteCloser, client bool) *Conn {
	return newConn(rw, bufio.NewReader(rw), client)
}

func newConn(rw io.ReadWriteCloser, br *bufio.Reader, client bool) *Conn {
	return &Conn{raw: rw, br: br, client: client, MaxMessage: DefaultMaxMessage}
}

// SetReadDeadline forwards to the underlying transport when it supports
// deadlines (net.Conn and net.Pipe do); otherwise it is a no-op.
func (c *Conn) SetReadDeadline(t time.Time) error {
	if d, ok := c.raw.(interface{ SetReadDeadline(time.Time) error }); ok {
		return d.SetReadDeadline(t)
	}
	return nil
}

// Close closes the underlying transport without a closing handshake.
// Use WriteClose first for a clean shutdown.
func (c *Conn) Close() error { return c.raw.Close() }

// ReadMessage returns the next complete data message. It transparently
// answers pings with pongs, skips pongs, reassembles fragmented messages,
// validates UTF-8 on text, and enforces the masking rules. When the peer
// sends a close frame, ReadMessage echoes it (once) and returns *CloseError.
func (c *Conn) ReadMessage() (Message, error) {
	var msgType byte
	var buf []byte
	for {
		f, err := ReadFrame(c.br, c.MaxMessage)
		if err != nil {
			return Message{}, err
		}
		// RFC 6455 §5.1: clients mask, servers must not.
		if f.Masked == c.client {
			if c.client {
				return Message{}, ProtocolError("received a masked frame from the server")
			}
			return Message{}, ProtocolError("received an unmasked frame from the client")
		}

		if IsControl(f.Opcode) {
			switch f.Opcode {
			case OpPing:
				if err := c.WritePong(f.Payload); err != nil {
					return Message{}, err
				}
			case OpPong:
				// Unsolicited or answering pongs are simply absorbed.
			case OpClose:
				ce, perr := parseClose(f.Payload)
				if perr != nil {
					return Message{}, perr
				}
				if err := c.echoClose(ce.Code); err != nil && err != io.ErrClosedPipe {
					return Message{}, err
				}
				return Message{}, ce
			}
			continue
		}

		switch f.Opcode {
		case OpContinuation:
			if msgType == 0 {
				return Message{}, ProtocolError("continuation frame without a started message")
			}
			buf = append(buf, f.Payload...)
		case OpText, OpBinary:
			if msgType != 0 {
				return Message{}, ProtocolError("new data frame in the middle of a fragmented message")
			}
			msgType = f.Opcode
			buf = f.Payload
		}
		if c.MaxMessage > 0 && int64(len(buf)) > c.MaxMessage {
			return Message{}, ErrTooLarge
		}
		if f.Fin {
			if msgType == OpText && !utf8.Valid(buf) {
				return Message{}, ProtocolError("text message is not valid UTF-8")
			}
			return Message{Type: msgType, Data: buf}, nil
		}
	}
}

// WriteMessage sends one unfragmented data message.
func (c *Conn) WriteMessage(typ byte, data []byte) error {
	if typ != OpText && typ != OpBinary {
		return fmt.Errorf("ws: WriteMessage requires OpText or OpBinary, got 0x%X", typ)
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.writeFrameLocked(Frame{Fin: true, Opcode: typ, Payload: data})
}

// WritePing sends a ping control frame.
func (c *Conn) WritePing(data []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.writeFrameLocked(Frame{Fin: true, Opcode: OpPing, Payload: data})
}

// WritePong sends a pong control frame.
func (c *Conn) WritePong(data []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.writeFrameLocked(Frame{Fin: true, Opcode: OpPong, Payload: data})
}

// WriteClose sends a close frame with the given code and reason. Sending a
// second close frame on the same Conn is a silent no-op, which makes close
// echoing and shutdown races safe to express naively.
func (c *Conn) WriteClose(code int, reason string) error {
	return c.writeClosePayload(closePayload(code, reason))
}

func (c *Conn) echoClose(code int) error {
	if code == 1005 {
		// The peer omitted a status code; echo an empty close frame.
		return c.writeClosePayload(nil)
	}
	return c.writeClosePayload(closePayload(code, ""))
}

func (c *Conn) writeClosePayload(payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closeSent {
		return nil
	}
	c.closeSent = true
	return c.writeFrameLocked(Frame{Fin: true, Opcode: OpClose, Payload: payload})
}

func (c *Conn) writeFrameLocked(f Frame) error {
	if c.client {
		f.Masked = true
		if _, err := rand.Read(f.MaskKey[:]); err != nil {
			return err
		}
	}
	return WriteFrame(c.raw, f)
}

func closePayload(code int, reason string) []byte {
	p := binary.BigEndian.AppendUint16(nil, uint16(code))
	return append(p, reason...)
}

func parseClose(payload []byte) (*CloseError, error) {
	switch {
	case len(payload) == 0:
		return &CloseError{Code: 1005}, nil
	case len(payload) == 1:
		return nil, ProtocolError("close frame with a 1-byte payload")
	}
	reason := payload[2:]
	if !utf8.Valid(reason) {
		return nil, ProtocolError("close reason is not valid UTF-8")
	}
	return &CloseError{
		Code:   int(binary.BigEndian.Uint16(payload[:2])),
		Reason: string(reason),
	}, nil
}
