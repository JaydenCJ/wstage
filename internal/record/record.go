// Package record implements the recording proxy: it relays a live
// WebSocket session between a client and an upstream server, writing every
// data message and the final closing handshake to a cassette as they
// happen. Recording is message-level: fragmented messages are reassembled
// before being written, and ping/pong keepalives are answered locally
// rather than recorded (see docs/cassette-format.md for the rationale).
package record

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/JaydenCJ/wstage/internal/cassette"
	"github.com/JaydenCJ/wstage/internal/ws"
)

// Summary reports what a recorded session contained.
type Summary struct {
	C2S       int
	S2C       int
	CloseBy   string // "client" or "server"; empty if the session ended abruptly
	CloseCode int
}

// Events returns the total number of event lines written.
func (s *Summary) Events() int {
	n := s.C2S + s.S2C
	if s.CloseBy != "" {
		n++
	}
	return n
}

// Session relays messages between client and upstream until either side
// closes, writing events to w. now supplies timestamps and is injectable
// for deterministic tests (nil = time.Now). The header must already have
// been written by the caller.
func Session(client, upstream *ws.Conn, w *cassette.Writer, now func() time.Time) (*Summary, error) {
	if now == nil {
		now = time.Now
	}
	s := &session{w: w, now: now, start: now(), sum: &Summary{}}

	errc := make(chan error, 2)
	go func() { errc <- s.pump(client, upstream, cassette.DirC2S, "client") }()
	go func() { errc <- s.pump(upstream, client, cassette.DirS2C, "server") }()
	err := <-errc
	if err2 := <-errc; err == nil {
		err = err2
	}
	return s.sum, err
}

type session struct {
	mu     sync.Mutex
	w      *cassette.Writer
	now    func() time.Time
	start  time.Time
	sum    *Summary
	closed bool
}

// pump reads messages from src and relays them to dst, recording each one.
// dir is the cassette direction of messages flowing out of src; from names
// the side that src speaks for ("client" or "server").
func (s *session) pump(src, dst *ws.Conn, dir, from string) error {
	for {
		msg, err := src.ReadMessage()
		var ce *ws.CloseError
		if errors.As(err, &ce) {
			if werr := s.recordClose(from, ce); werr != nil {
				return werr
			}
			// Forward the close; WriteClose is a no-op if the other pump
			// (or the Conn's automatic echo) already sent one.
			code := ce.Code
			if code == 1005 {
				code = 1000
			}
			dst.WriteClose(code, ce.Reason)
			return nil
		}
		if err != nil {
			if s.isClosed() {
				// The other direction already completed the closing
				// handshake; a read error here is just teardown noise.
				return nil
			}
			return fmt.Errorf("record: reading from %s: %w", from, err)
		}
		if werr := s.recordMessage(dir, msg); werr != nil {
			return werr
		}
		if err := dst.WriteMessage(msg.Type, msg.Data); err != nil {
			if s.isClosed() {
				return nil
			}
			return fmt.Errorf("record: relaying %s message: %w", dir, err)
		}
	}
}

func (s *session) recordMessage(dir string, msg ws.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	ev := cassette.Event{T: s.elapsed(), Dir: dir}
	if msg.Type == ws.OpBinary {
		ev.Type = cassette.TypeBinary
		ev.DataB64 = base64.StdEncoding.EncodeToString(msg.Data)
	} else {
		ev.Type = cassette.TypeText
		ev.Data = string(msg.Data)
	}
	if dir == cassette.DirC2S {
		s.sum.C2S++
	} else {
		s.sum.S2C++
	}
	return s.w.WriteEvent(ev)
}

func (s *session) recordClose(from string, ce *ws.CloseError) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		// The second close is the echo of the first; only the initiator
		// is recorded.
		return nil
	}
	s.closed = true
	code := ce.Code
	if code == 1005 {
		code = 0 // "no status code" is recorded as unspecified
	}
	s.sum.CloseBy = from
	s.sum.CloseCode = code
	return s.w.WriteEvent(cassette.Event{
		T:      s.elapsed(),
		Dir:    cassette.DirClose,
		By:     from,
		Code:   code,
		Reason: ce.Reason,
	})
}

func (s *session) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// elapsed returns seconds since the session started, rounded to the
// millisecond so cassettes stay tidy and diff-friendly.
func (s *session) elapsed() float64 {
	return math.Round(s.now().Sub(s.start).Seconds()*1000) / 1000
}
