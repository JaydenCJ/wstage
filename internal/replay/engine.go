// Package replay drives one side of a live WebSocket session from a
// cassette. As RoleServer it is the scripted mock server: it sends the
// recorded s2c messages and asserts the peer's messages against the c2s
// expectations. As RoleClient (the `wstage play` command) the directions
// swap, so the same engine can also drive a scripted client against any
// server — including a wstage replay of the same cassette.
package replay

import (
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/JaydenCJ/wstage/internal/cassette"
	"github.com/JaydenCJ/wstage/internal/ws"
)

// Role selects which side of the cassette the engine performs.
type Role int

const (
	RoleServer Role = iota
	RoleClient
)

func (r Role) String() string {
	if r == RoleClient {
		return "client"
	}
	return "server"
}

// Options configures a session run.
type Options struct {
	Role    Role
	Lenient bool                // record mismatches but keep the session going
	Speed   float64             // multiplier on recorded gaps; 0 = no delays
	Timeout time.Duration       // per-read deadline; 0 = wait forever
	Sleep   func(time.Duration) // injectable for tests; nil = time.Sleep
}

// Mismatch is one failed expectation.
type Mismatch struct {
	Index int    // 1-based event number in the cassette
	Why   string // human-readable reason from the matcher
}

// Result summarizes a completed session.
type Result struct {
	Sent       int // messages this side sent
	Matched    int // expectations that matched
	Expected   int // expectations evaluated
	Mismatches []Mismatch
	PeerClose  int // close code received from the peer (0 = none seen)
}

// OK reports whether every expectation matched.
func (r *Result) OK() bool { return len(r.Mismatches) == 0 }

// Run replays the cassette over an already-handshaken connection and
// returns when the session ends. A non-nil error is a transport or cassette
// problem (exit code 3 territory); failed expectations are reported in
// Result.Mismatches with a nil error.
func Run(conn *ws.Conn, cas *cassette.Cassette, opt Options) (*Result, error) {
	e := engine{conn: conn, opt: opt, res: &Result{}}
	if e.opt.Sleep == nil {
		e.opt.Sleep = time.Sleep
	}
	e.outDir, e.expDir = cassette.DirS2C, cassette.DirC2S
	if opt.Role == RoleClient {
		e.outDir, e.expDir = cassette.DirC2S, cassette.DirS2C
	}
	return e.run(cas)
}

type engine struct {
	conn   *ws.Conn
	opt    Options
	res    *Result
	outDir string
	expDir string
}

func (e *engine) run(cas *cassette.Cassette) (*Result, error) {
	prev := 0.0
	for i := range cas.Events {
		ev := &cas.Events[i]
		n := i + 1
		switch ev.Dir {
		case cassette.DirClose:
			return e.res, e.finishClose(ev, n, ev.T-prev)
		case e.outDir:
			e.pause(ev.T - prev)
			payload, err := ev.Payload()
			if err != nil {
				return e.res, fmt.Errorf("event %d: %w", n, err)
			}
			typ := ws.OpText
			if ev.Binary() {
				typ = ws.OpBinary
			}
			if err := e.conn.WriteMessage(typ, payload); err != nil {
				return e.res, fmt.Errorf("event %d: sending message: %w", n, err)
			}
			e.res.Sent++
		case e.expDir:
			done, err := e.expect(ev, n)
			if err != nil || done {
				return e.res, err
			}
		}
		prev = ev.T
	}

	// No close event was recorded: the server initiates a normal close and
	// the client waits for it, so both roles agree on who goes first.
	var code int
	var err error
	if e.opt.Role == RoleServer {
		code, err = e.closeAndDrain(1000, "")
	} else {
		code, err = e.awaitClose()
	}
	if err != nil {
		return e.res, err
	}
	e.res.PeerClose = code
	return e.res, nil
}

// closeAndDrain sends a close frame while concurrently draining the peer's
// frames until its close arrives. Doing both at once means two endpoints
// closing simultaneously cannot deadlock, even on an unbuffered transport
// such as net.Pipe. A write error is only surfaced if the drain also
// failed — a broken write after the peer already closed is teardown noise.
func (e *engine) closeAndDrain(code int, reason string) (int, error) {
	werr := make(chan error, 1)
	go func() { werr <- e.conn.WriteClose(code, reason) }()
	got, rerr := e.awaitClose()
	if err := <-werr; err != nil && rerr == nil && got == 0 {
		return 0, err
	}
	return got, rerr
}

// expect reads one message from the peer and matches it against ev.
// done=true means the session is over (mismatch in strict mode, or the
// peer closed early).
func (e *engine) expect(ev *cassette.Event, n int) (done bool, err error) {
	e.res.Expected++
	msg, err := e.readMessage()
	var ce *ws.CloseError
	if errors.As(err, &ce) {
		e.res.PeerClose = ce.Code
		e.fail(n, fmt.Sprintf("peer closed early (code %d %q) while a message was expected", ce.Code, ce.Reason))
		return true, nil
	}
	if err != nil {
		return true, fmt.Errorf("event %d: reading message: %w", n, err)
	}

	ok, why := cassette.MatchMessage(ev, msg.Type == ws.OpBinary, msg.Data)
	if ok {
		e.res.Matched++
		return false, nil
	}
	e.fail(n, why)
	if e.opt.Lenient {
		return false, nil
	}
	// Strict mode: tell the peer exactly which expectation broke, then
	// drain its close echo so the TCP teardown is clean.
	if _, err := e.closeAndDrain(1008, fmt.Sprintf("wstage: expectation %d failed", n)); err != nil {
		return true, err
	}
	return true, nil
}

// finishClose performs the recorded closing handshake: the recorded
// initiator sends the close frame, the other side waits for it and checks
// the code.
func (e *engine) finishClose(ev *cassette.Event, n int, gap float64) error {
	initiator := ev.By
	if initiator == "" {
		initiator = "server"
	}
	code := ev.Code
	if code == 0 {
		code = 1000
	}

	if initiator == e.opt.Role.String() {
		e.pause(gap)
		got, err := e.closeAndDrain(code, ev.Reason)
		if err != nil {
			return err
		}
		e.res.PeerClose = got
		return nil
	}

	// Wait for the peer's close; a data message here means the peer went
	// off script.
	for {
		msg, err := e.readMessage()
		var ce *ws.CloseError
		if errors.As(err, &ce) {
			e.res.PeerClose = ce.Code
			if ev.Code != 0 && ce.Code != ev.Code {
				e.fail(n, fmt.Sprintf("close code: want %d, got %d", ev.Code, ce.Code))
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("event %d: waiting for close: %w", n, err)
		}
		e.fail(n, fmt.Sprintf("expected the peer to close, got a %s message %s",
			typeWord(msg.Type), previewData(msg)))
		if !e.opt.Lenient {
			_, err := e.closeAndDrain(1008, fmt.Sprintf("wstage: expectation %d failed", n))
			return err
		}
	}
}

// awaitClose reads until the peer's close frame arrives, ignoring any
// trailing data messages. A bare EOF after we sent our close is treated as
// an acceptable (if impolite) shutdown.
func (e *engine) awaitClose() (int, error) {
	for {
		_, err := e.readMessage()
		if err == nil {
			continue
		}
		var ce *ws.CloseError
		if errors.As(err, &ce) {
			return ce.Code, nil
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, nil
		}
		return 0, err
	}
}

func (e *engine) readMessage() (ws.Message, error) {
	if e.opt.Timeout > 0 {
		e.conn.SetReadDeadline(time.Now().Add(e.opt.Timeout))
	}
	return e.conn.ReadMessage()
}

func (e *engine) fail(n int, why string) {
	e.res.Mismatches = append(e.res.Mismatches, Mismatch{Index: n, Why: why})
}

func (e *engine) pause(gap float64) {
	if e.opt.Speed <= 0 || gap <= 0 {
		return
	}
	// Cassette timestamps carry millisecond precision, so the scaled gap
	// is rounded to the millisecond to keep delays exact and predictable.
	ms := math.Round(gap * e.opt.Speed * 1000)
	e.opt.Sleep(time.Duration(ms) * time.Millisecond)
}

func typeWord(op byte) string {
	if op == ws.OpBinary {
		return "binary"
	}
	return "text"
}

func previewData(msg ws.Message) string {
	if msg.Type == ws.OpBinary {
		return fmt.Sprintf("(%d bytes)", len(msg.Data))
	}
	return fmt.Sprintf("%q", cassette.Truncate(string(msg.Data), 60))
}
