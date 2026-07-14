// Engine tests: a server engine and a client engine drive the same
// cassette against each other over net.Pipe, so every path — matching,
// strict/lenient mismatches, close handshakes, timing — is exercised with
// zero sockets and zero real sleeps.
package replay

import (
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JaydenCJ/wstage/internal/cassette"
	"github.com/JaydenCJ/wstage/internal/ws"
)

func header() cassette.Header { return cassette.Header{Wstage: 1, Name: "test"} }

func fullCassette() *cassette.Cassette {
	return &cassette.Cassette{Header: header(), Events: []cassette.Event{
		{T: 0, Dir: cassette.DirC2S, Type: cassette.TypeText, Data: "subscribe:AAPL"},
		{T: 0.04, Dir: cassette.DirS2C, Type: cassette.TypeText, Data: `{"px":210.05,"seq":1}`},
		{T: 0.6, Dir: cassette.DirC2S, Type: cassette.TypeText, Data: `{"op":"ack","seq":1}`, Match: cassette.MatchSubset, Expect: `{"op":"ack"}`},
		{T: 1.0, Dir: cassette.DirS2C, Type: cassette.TypeBinary, DataB64: "AAECAw=="},
		{T: 1.5, Dir: cassette.DirClose, By: "server", Code: 1000, Reason: "done"},
	}}
}

// runPair runs a server engine with srvCas and a client engine with cliCas
// against each other over an in-memory pipe and returns both results.
func runPair(t *testing.T, srvCas, cliCas *cassette.Cassette, srvOpt, cliOpt Options) (srv, cli *Result) {
	t.Helper()
	cp, sp := pipe(t)
	srvOpt.Role = RoleServer
	cliOpt.Role = RoleClient
	if srvOpt.Timeout == 0 {
		srvOpt.Timeout = 5 * time.Second // hang guard, never hit when green
	}
	if cliOpt.Timeout == 0 {
		cliOpt.Timeout = 5 * time.Second
	}

	var wg sync.WaitGroup
	wg.Add(1)
	var cliErr error
	go func() {
		defer wg.Done()
		cli, cliErr = Run(ws.NewConn(cp, true), cliCas, cliOpt)
	}()
	srv, err := Run(ws.NewConn(sp, false), srvCas, srvOpt)
	if err != nil {
		t.Fatalf("server engine: %v", err)
	}
	wg.Wait()
	if cliErr != nil {
		t.Fatalf("client engine: %v", cliErr)
	}
	return srv, cli
}

func pipe(t *testing.T) (c, s net.Conn) {
	t.Helper()
	cp, sp := net.Pipe()
	t.Cleanup(func() { cp.Close(); sp.Close() })
	return cp, sp
}

func TestServerAndClientReplayAFullCassetteAgainstEachOther(t *testing.T) {
	srv, cli := runPair(t, fullCassette(), fullCassette(), Options{}, Options{})
	if !srv.OK() || srv.Sent != 2 || srv.Matched != 2 || srv.Expected != 2 {
		t.Fatalf("server result: %+v", srv)
	}
	if !cli.OK() || cli.Sent != 2 || cli.Matched != 2 {
		t.Fatalf("client result: %+v", cli)
	}
	if cli.PeerClose != 1000 {
		t.Fatalf("client should see the recorded close code, got %d", cli.PeerClose)
	}
}

func TestStrictMismatchClosesWith1008AndNamesTheExpectation(t *testing.T) {
	// The client's cassette sends the wrong first message.
	bad := fullCassette()
	bad.Events[0].Data = "subscribe:WRONG"
	srv, cli := runPair(t, fullCassette(), bad, Options{}, Options{})

	if srv.OK() || len(srv.Mismatches) != 1 {
		t.Fatalf("server result: %+v", srv)
	}
	if srv.Mismatches[0].Index != 1 || !strings.Contains(srv.Mismatches[0].Why, "subscribe:WRONG") {
		t.Fatalf("mismatch: %+v", srv.Mismatches[0])
	}
	// The client sees a policy-violation close naming the expectation.
	if cli.PeerClose != 1008 {
		t.Fatalf("client should be closed with 1008, got %d (result %+v)", cli.PeerClose, cli)
	}
}

func TestLenientModeRecordsMismatchesButFinishesTheSession(t *testing.T) {
	bad := fullCassette()
	bad.Events[0].Data = "subscribe:WRONG"
	srv, cli := runPair(t, fullCassette(), bad, Options{Lenient: true}, Options{})

	if len(srv.Mismatches) != 1 {
		t.Fatalf("want exactly 1 mismatch, got %+v", srv.Mismatches)
	}
	if srv.Matched != 1 || srv.Expected != 2 {
		t.Fatalf("the second expectation should still be evaluated: %+v", srv)
	}
	// The session ran to its recorded close on both sides.
	if cli.PeerClose != 1000 || !cli.OK() {
		t.Fatalf("client result: %+v", cli)
	}
}

func TestCloseInitiatedByClientIsHonoredByBothRoles(t *testing.T) {
	cas := &cassette.Cassette{Header: header(), Events: []cassette.Event{
		{T: 0, Dir: cassette.DirS2C, Type: cassette.TypeText, Data: "welcome"},
		{T: 0.2, Dir: cassette.DirClose, By: "client", Code: 1001, Reason: "leaving"},
	}}
	srv, cli := runPair(t, cas, cas, Options{}, Options{})
	if !srv.OK() || srv.PeerClose != 1001 {
		t.Fatalf("server should receive the client's 1001: %+v", srv)
	}
	if !cli.OK() {
		t.Fatalf("client result: %+v", cli)
	}
}

func TestMissingCloseEventEndsWithAServerInitiated1000(t *testing.T) {
	cas := &cassette.Cassette{Header: header(), Events: []cassette.Event{
		{T: 0, Dir: cassette.DirC2S, Type: cassette.TypeText, Data: "one-shot"},
	}}
	srv, cli := runPair(t, cas, cas, Options{}, Options{})
	if !srv.OK() || !cli.OK() {
		t.Fatalf("results: srv=%+v cli=%+v", srv, cli)
	}
	if cli.PeerClose != 1000 {
		t.Fatalf("client should see a normal close, got %d", cli.PeerClose)
	}
}

func TestDeviationsFromTheRecordedCloseAreMismatches(t *testing.T) {
	// The wrong close code is called out with both values…
	srvCas := &cassette.Cassette{Header: header(), Events: []cassette.Event{
		{T: 0, Dir: cassette.DirClose, By: "client", Code: 1000},
	}}
	cliCas := &cassette.Cassette{Header: header(), Events: []cassette.Event{
		{T: 0, Dir: cassette.DirClose, By: "client", Code: 4001},
	}}
	srv, _ := runPair(t, srvCas, cliCas, Options{}, Options{})
	if srv.OK() || !strings.Contains(srv.Mismatches[0].Why, "want 1000, got 4001") {
		t.Fatalf("server result: %+v", srv)
	}

	// …and so is a data message where the recording says "close".
	cliCas2 := &cassette.Cassette{Header: header(), Events: []cassette.Event{
		{T: 0, Dir: cassette.DirC2S, Type: cassette.TypeText, Data: "off script"},
		{T: 0.1, Dir: cassette.DirClose, By: "client", Code: 1000},
	}}
	srv2, _ := runPair(t, srvCas, cliCas2, Options{}, Options{})
	if srv2.OK() || !strings.Contains(srv2.Mismatches[0].Why, `"off script"`) {
		t.Fatalf("server result: %+v", srv2)
	}
}

func TestPeerClosingEarlyIsRecordedAsAMismatch(t *testing.T) {
	srvCas := fullCassette()
	cliCas := &cassette.Cassette{Header: header(), Events: []cassette.Event{
		{T: 0, Dir: cassette.DirClose, By: "client", Code: 1001, Reason: "bailing"},
	}}
	srv, _ := runPair(t, srvCas, cliCas, Options{}, Options{})
	if srv.OK() {
		t.Fatalf("server result: %+v", srv)
	}
	why := srv.Mismatches[0].Why
	if !strings.Contains(why, "closed early") || !strings.Contains(why, "1001") {
		t.Fatalf("mismatch reason: %q", why)
	}
}

func TestSpeedZeroNeverSleepsAndSpeedScalesTheRecordedGaps(t *testing.T) {
	// Speed 0 (the default) must not sleep at all, even with
	// multi-hundred-ms recorded gaps.
	var slept []time.Duration
	rec := func(d time.Duration) { slept = append(slept, d) }
	runPair(t, fullCassette(), fullCassette(), Options{Speed: 0, Sleep: rec}, Options{Speed: 0, Sleep: rec})
	if len(slept) != 0 {
		t.Fatalf("speed 0 must not sleep, got %v", slept)
	}

	// Speed 2 doubles every recorded gap, exactly. Only the server sleeps
	// here (all sends are s2c plus the final close).
	var mu sync.Mutex
	slept = nil
	rec = func(d time.Duration) { mu.Lock(); slept = append(slept, d); mu.Unlock() }
	cas := &cassette.Cassette{Header: header(), Events: []cassette.Event{
		{T: 0.1, Dir: cassette.DirS2C, Type: cassette.TypeText, Data: "a"},
		{T: 0.3, Dir: cassette.DirS2C, Type: cassette.TypeText, Data: "b"},
		{T: 0.4, Dir: cassette.DirClose, By: "server", Code: 1000},
	}}
	runPair(t, cas, cas, Options{Speed: 2, Sleep: rec}, Options{})
	want := []time.Duration{200 * time.Millisecond, 400 * time.Millisecond, 200 * time.Millisecond}
	if len(slept) != len(want) {
		t.Fatalf("got sleeps %v, want %v", slept, want)
	}
	for i := range want {
		if slept[i] != want[i] {
			t.Fatalf("sleep %d = %v, want %v", i, slept[i], want[i])
		}
	}
}

func TestBinaryEventsReplayAndMatchByBytes(t *testing.T) {
	cas := &cassette.Cassette{Header: header(), Events: []cassette.Event{
		{T: 0, Dir: cassette.DirC2S, Type: cassette.TypeBinary, DataB64: "3q2+7w=="}, // de ad be ef
		{T: 0.1, Dir: cassette.DirS2C, Type: cassette.TypeBinary, DataB64: "AAECAw=="},
	}}
	srv, cli := runPair(t, cas, cas, Options{}, Options{})
	if !srv.OK() || !cli.OK() {
		t.Fatalf("srv=%+v cli=%+v", srv, cli)
	}
}

func TestRunReturnsACassetteErrorForUndecodablePayloads(t *testing.T) {
	// A corrupt binary payload is a cassette problem (error), not a
	// mismatch — the caller must exit 3, not 1.
	cas := &cassette.Cassette{Header: header(), Events: []cassette.Event{
		{T: 0, Dir: cassette.DirS2C, Type: cassette.TypeBinary, DataB64: "%%%"},
	}}
	cp, sp := pipe(t)
	go ws.NewConn(cp, true).ReadMessage() // keep the pipe drained
	_, err := Run(ws.NewConn(sp, false), cas, Options{Role: RoleServer, Timeout: 5 * time.Second})
	if err == nil || !strings.Contains(err.Error(), "base64") {
		t.Fatalf("want a base64 cassette error, got %v", err)
	}
}
