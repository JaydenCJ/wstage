// Recorder tests: a fake client and a fake upstream drive the proxy over
// net.Pipe with an injected clock, so the produced cassette — ordering,
// payload encoding, close attribution, timestamps — is asserted exactly.
package record

import (
	"bytes"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/JaydenCJ/wstage/internal/cassette"
	"github.com/JaydenCJ/wstage/internal/replay"
	"github.com/JaydenCJ/wstage/internal/ws"
)

// fakeClock returns a deterministic now() that advances 10 ms per call.
func fakeClock() func() time.Time {
	base := time.Date(2026, 7, 10, 9, 15, 0, 0, time.UTC)
	n := 0
	var mu sync.Mutex
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		n++
		return base.Add(time.Duration(n) * 10 * time.Millisecond)
	}
}

// harness wires client ↔ recorder ↔ upstream over two in-memory pipes and
// runs Session in the background.
type harness struct {
	client   *ws.Conn // the test's end of the client side
	upstream *ws.Conn // the test's end of the upstream side
	buf      bytes.Buffer
	sum      *Summary
	err      error
	done     chan struct{}
}

func start(t *testing.T) *harness {
	t.Helper()
	cliOuter, cliInner := net.Pipe()
	upInner, upOuter := net.Pipe()
	t.Cleanup(func() { cliOuter.Close(); cliInner.Close(); upInner.Close(); upOuter.Close() })

	h := &harness{
		client:   ws.NewConn(cliOuter, true), // test acts as the real client
		upstream: ws.NewConn(upOuter, false), // test acts as the real upstream server
		done:     make(chan struct{}),
	}
	w := cassette.NewWriter(&h.buf)
	if err := w.WriteHeader(cassette.Header{Name: "rec"}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	go func() {
		defer close(h.done)
		// The recorder is a server toward the client and a client toward
		// the upstream, exactly as in the CLI.
		h.sum, h.err = Session(ws.NewConn(cliInner, false), ws.NewConn(upInner, true), w, fakeClock())
	}()
	return h
}

func (h *harness) wait(t *testing.T) *cassette.Cassette {
	t.Helper()
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		t.Fatal("recorder session did not finish")
	}
	if h.err != nil {
		t.Fatalf("Session: %v", h.err)
	}
	cas, err := cassette.Load(&h.buf)
	if err != nil {
		t.Fatalf("recorded cassette does not parse: %v", err)
	}
	if errs := cas.Validate(); len(errs) != 0 {
		t.Fatalf("recorded cassette does not validate: %v", errs)
	}
	return cas
}

func (h *harness) closeFromClient(t *testing.T, code int, reason string) {
	t.Helper()
	if err := h.client.WriteClose(code, reason); err != nil {
		t.Fatalf("client close: %v", err)
	}
	// Drain both ends so the proxied closing handshake completes.
	go h.client.ReadMessage()
	if _, err := h.upstream.ReadMessage(); !isClose(err) {
		t.Fatalf("upstream should see the client's close, got %v", err)
	}
}

func isClose(err error) bool {
	var ce *ws.CloseError
	return errors.As(err, &ce)
}

func TestRecordCapturesBothDirectionsInOrder(t *testing.T) {
	h := start(t)
	// Strictly sequenced request/response pairs make the event order
	// deterministic even though the two pumps are concurrent.
	if err := h.client.WriteMessage(ws.OpText, []byte("subscribe:AAPL")); err != nil {
		t.Fatalf("client send: %v", err)
	}
	msg, err := h.upstream.ReadMessage()
	if err != nil || string(msg.Data) != "subscribe:AAPL" {
		t.Fatalf("upstream got %q, %v", msg.Data, err)
	}
	if err := h.upstream.WriteMessage(ws.OpText, []byte(`{"px":210.05}`)); err != nil {
		t.Fatalf("upstream send: %v", err)
	}
	if msg, err = h.client.ReadMessage(); err != nil || string(msg.Data) != `{"px":210.05}` {
		t.Fatalf("client got %q, %v", msg.Data, err)
	}
	h.closeFromClient(t, 1000, "done")
	cas := h.wait(t)

	if len(cas.Events) != 3 {
		t.Fatalf("want 3 events, got %+v", cas.Events)
	}
	if cas.Events[0].Dir != cassette.DirC2S || cas.Events[0].Data != "subscribe:AAPL" {
		t.Fatalf("event 1: %+v", cas.Events[0])
	}
	if cas.Events[1].Dir != cassette.DirS2C || cas.Events[1].Data != `{"px":210.05}` {
		t.Fatalf("event 2: %+v", cas.Events[1])
	}
}

func TestRecordAttributesTheCloseToTheClient(t *testing.T) {
	h := start(t)
	h.closeFromClient(t, 1001, "going away")
	cas := h.wait(t)

	if len(cas.Events) != 1 {
		t.Fatalf("want just the close event, got %+v", cas.Events)
	}
	ev := cas.Events[0]
	if ev.Dir != cassette.DirClose || ev.By != "client" || ev.Code != 1001 || ev.Reason != "going away" {
		t.Fatalf("close event: %+v", ev)
	}
	if h.sum.CloseBy != "client" || h.sum.CloseCode != 1001 {
		t.Fatalf("summary: %+v", h.sum)
	}
}

func TestRecordAttributesTheCloseToTheServer(t *testing.T) {
	h := start(t)
	if err := h.upstream.WriteClose(4000, "kicked"); err != nil {
		t.Fatalf("upstream close: %v", err)
	}
	go h.upstream.ReadMessage() // drain the echo
	if _, err := h.client.ReadMessage(); !isClose(err) {
		t.Fatalf("client should see the server's close, got %v", err)
	}
	cas := h.wait(t)

	ev := cas.Events[0]
	if ev.By != "server" || ev.Code != 4000 || ev.Reason != "kicked" {
		t.Fatalf("close event: %+v", ev)
	}
}

func TestRecordEncodesBinaryMessagesAsBase64(t *testing.T) {
	h := start(t)
	payload := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	if err := h.client.WriteMessage(ws.OpBinary, payload); err != nil {
		t.Fatalf("client send: %v", err)
	}
	if msg, err := h.upstream.ReadMessage(); err != nil || !bytes.Equal(msg.Data, payload) {
		t.Fatalf("upstream got %v, %v", msg.Data, err)
	}
	h.closeFromClient(t, 1000, "")
	cas := h.wait(t)

	ev := cas.Events[0]
	if ev.Type != cassette.TypeBinary || ev.DataB64 != "3q2+7w==" || ev.Data != "" {
		t.Fatalf("binary event: %+v", ev)
	}
	got, err := ev.Payload()
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("payload round trip: %v, %v", got, err)
	}
}

func TestRecordedTimestampsComeFromTheInjectedClock(t *testing.T) {
	h := start(t)
	if err := h.client.WriteMessage(ws.OpText, []byte("tick")); err != nil {
		t.Fatalf("client send: %v", err)
	}
	if _, err := h.upstream.ReadMessage(); err != nil {
		t.Fatalf("upstream read: %v", err)
	}
	h.closeFromClient(t, 1000, "")
	cas := h.wait(t)

	// The clock advances 10 ms per call; call 1 is the session start, so
	// every event lands on a positive 10 ms multiple.
	for i, ev := range cas.Events {
		if ev.T <= 0 || ev.T >= 1 {
			t.Fatalf("event %d has a wall-clock-looking timestamp %v", i+1, ev.T)
		}
		ms := ev.T * 1000
		if ms != float64(int(ms)) || int(ms)%10 != 0 {
			t.Fatalf("event %d timestamp %v is not a 10 ms tick", i+1, ev.T)
		}
	}
}

func TestRecordCloseWithoutStatusIsWrittenAsUnspecified(t *testing.T) {
	h := start(t)
	// A raw close frame with an empty payload → code 1005 internally,
	// which must be recorded as "unspecified" (0), not invented.
	go h.client.ReadMessage()
	if err := h.client.WriteClose(1005, ""); err != nil {
		t.Fatalf("client close: %v", err)
	}
	if _, err := h.upstream.ReadMessage(); !isClose(err) {
		t.Fatalf("upstream should see a close, got %v", err)
	}
	cas := h.wait(t)
	if ev := cas.Events[0]; ev.Code != 0 {
		t.Fatalf("close code should be recorded as 0 (unspecified), got %+v", ev)
	}
}

func TestRecordedCassetteReplaysCleanly(t *testing.T) {
	// The tool's core promise: record a session, then a replay server and
	// a play client both PASS the recorded cassette.
	h := start(t)
	steps := []struct {
		fromClient bool
		data       string
	}{
		{true, "hello"},
		{false, "welcome"},
		{false, "update 1"},
		{true, "ack"},
	}
	for _, s := range steps {
		src, dst := h.client, h.upstream
		if !s.fromClient {
			src, dst = h.upstream, h.client
		}
		if err := src.WriteMessage(ws.OpText, []byte(s.data)); err != nil {
			t.Fatalf("send %q: %v", s.data, err)
		}
		if msg, err := dst.ReadMessage(); err != nil || string(msg.Data) != s.data {
			t.Fatalf("relay of %q: got %q, %v", s.data, msg.Data, err)
		}
	}
	h.closeFromClient(t, 1000, "bye")
	cas := h.wait(t)

	cp, sp := net.Pipe()
	t.Cleanup(func() { cp.Close(); sp.Close() })
	var wg sync.WaitGroup
	wg.Add(1)
	var cliRes *replay.Result
	var cliErr error
	go func() {
		defer wg.Done()
		cliRes, cliErr = replay.Run(ws.NewConn(cp, true), cas, replay.Options{Role: replay.RoleClient, Timeout: 5 * time.Second})
	}()
	srvRes, err := replay.Run(ws.NewConn(sp, false), cas, replay.Options{Role: replay.RoleServer, Timeout: 5 * time.Second})
	wg.Wait()
	if err != nil || cliErr != nil {
		t.Fatalf("replaying the recording errored: srv=%v cli=%v", err, cliErr)
	}
	if !srvRes.OK() || !cliRes.OK() {
		t.Fatalf("recorded cassette does not replay cleanly: srv=%+v cli=%+v", srvRes, cliRes)
	}
	if srvRes.Matched != 2 || cliRes.Matched != 2 {
		t.Fatalf("matched counts: srv=%d cli=%d", srvRes.Matched, cliRes.Matched)
	}
}
