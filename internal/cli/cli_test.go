// CLI tests drive Run in-process: pure commands (version, verify, show)
// with plain buffers, and the socket commands (replay, play, record) as
// end-to-end loopback sessions on 127.0.0.1 ephemeral ports, with the
// listen address scraped from the CLI's own stderr — no fixed ports, no
// sleeps, no flakiness.
package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JaydenCJ/wstage/internal/cassette"
)

const tickerCassette = `{"wstage":1,"name":"ticker","url":"ws://127.0.0.1:9601/feed","recorded_at":"2026-07-10T09:15:00Z"}
{"t":0,"dir":"c2s","type":"text","data":"subscribe:AAPL"}
{"t":0.04,"dir":"s2c","type":"text","data":"{\"sym\":\"AAPL\",\"px\":210.05,\"seq\":1}"}
{"t":0.54,"dir":"s2c","type":"text","data":"{\"sym\":\"AAPL\",\"px\":210.11,\"seq\":2}"}
{"t":0.6,"dir":"c2s","type":"text","data":"{\"op\":\"ack\",\"seq\":2}","match":"subset","expect":"{\"op\":\"ack\"}"}
{"t":1.04,"dir":"s2c","type":"text","data":"{\"sym\":\"AAPL\",\"px\":209.98,\"seq\":3}"}
{"t":1.2,"dir":"c2s","type":"text","data":"unsubscribe:AAPL","match":"regex","expect":"^unsubscribe:[A-Z]+$"}
{"t":1.5,"dir":"close","by":"server","code":1000,"reason":"feed complete"}
`

func writeCassette(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.jsonl")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func runCLI(args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = Run(args, &out, &errb)
	return code, out.String(), errb.String()
}

// waitBuffer is a goroutine-safe writer whose waitFor blocks (without
// polling sleeps) until a regexp matches what has been written so far.
type waitBuffer struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	signal chan struct{}
}

func newWaitBuffer() *waitBuffer { return &waitBuffer{signal: make(chan struct{})} }

func (b *waitBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, err := b.buf.Write(p)
	close(b.signal)
	b.signal = make(chan struct{})
	return n, err
}

func (b *waitBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *waitBuffer) waitFor(t *testing.T, re *regexp.Regexp) string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		b.mu.Lock()
		m := re.FindStringSubmatch(b.buf.String())
		ch := b.signal
		b.mu.Unlock()
		if m != nil {
			if len(m) > 1 {
				return m[1]
			}
			return m[0]
		}
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("output never matched %v; got %q", re, b.String())
		}
	}
}

var listenRe = regexp.MustCompile(`ws://(127\.0\.0\.1:\d+)/`)

// server runs a CLI server command in the background and returns the
// address it bound plus a wait() that yields the exit code and stdout.
func server(t *testing.T, args ...string) (addr string, wait func() (int, string)) {
	t.Helper()
	stderr := newWaitBuffer()
	var stdout bytes.Buffer
	var mu sync.Mutex
	done := make(chan int, 1)
	go func() {
		var out bytes.Buffer
		code := Run(args, &out, stderr)
		mu.Lock()
		stdout = out
		mu.Unlock()
		done <- code
	}()
	addr = stderr.waitFor(t, listenRe)
	return addr, func() (int, string) {
		select {
		case code := <-done:
			mu.Lock()
			defer mu.Unlock()
			return code, stdout.String()
		case <-time.After(5 * time.Second):
			t.Fatalf("server command did not exit; stderr: %q", stderr.String())
			return -1, ""
		}
	}
}

func TestVersionPrintsTheManifestVersion(t *testing.T) {
	code, out, _ := runCLI("version")
	if code != 0 || out != "wstage 0.1.0\n" {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestUsageProblemsAllExit2(t *testing.T) {
	// No arguments at all → usage on stderr.
	code, _, errOut := runCLI()
	if code != 2 || !strings.Contains(errOut, "Usage:") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	// An unknown command is named in the error.
	code, _, errOut = runCLI("rewind")
	if code != 2 || !strings.Contains(errOut, `unknown command "rewind"`) {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	// An unknown flag is a usage error, not silently dropped.
	path := writeCassette(t, tickerCassette)
	if code, _, _ := runCLI("verify", "--frobnicate", path); code != 2 {
		t.Fatalf("code=%d", code)
	}
}

func TestFlagsMayFollowPositionalArguments(t *testing.T) {
	// `wstage verify file --flag` must not silently ignore the flag; here
	// a known-late flag still parses.
	path := writeCassette(t, tickerCassette)
	code, out, _ := runCLI("show", path)
	if code != 0 {
		t.Fatalf("show failed: %q", out)
	}
	code2, _, _ := runCLI("replay", path, "--listen", "not-an-address")
	if code2 != 3 {
		t.Fatalf("late --listen flag was ignored (code=%d)", code2)
	}
}

func TestVerifyReportsAHealthyCassette(t *testing.T) {
	path := writeCassette(t, tickerCassette)
	code, out, _ := runCLI("verify", path)
	if code != 0 {
		t.Fatalf("code=%d out=%q", code, out)
	}
	want := fmt.Sprintf("ok: %s — 7 events (3 c2s, 3 s2c, close by server), 1.50s\n", path)
	if out != want {
		t.Fatalf("out=%q want %q", out, want)
	}
}

func TestVerifyListsEveryProblemAndExits1(t *testing.T) {
	bad := `{"wstage":1}
{"t":0,"dir":"c2s","type":"text","data":"x","match":"fuzzy"}
{"t":-1,"dir":"upward","type":"text","data":"y"}
`
	path := writeCassette(t, bad)
	code, _, errOut := runCLI("verify", path)
	if code != 1 {
		t.Fatalf("code=%d", code)
	}
	for _, want := range []string{`unknown match mode "fuzzy"`, `unknown dir "upward"`, "negative timestamp"} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("stderr misses %q: %q", want, errOut)
		}
	}
}

func TestVerifyChecksMultipleCassettesAndMissingFilesInOneRun(t *testing.T) {
	good := writeCassette(t, tickerCassette)
	bad := writeCassette(t, `{"wstage":1}`+"\n"+`{"t":0,"dir":"nope"}`+"\n")
	missing := filepath.Join(t.TempDir(), "nope.jsonl")
	code, out, errOut := runCLI("verify", good, bad, missing)
	if code != 1 {
		t.Fatalf("any bad cassette must fail the run: code=%d", code)
	}
	if !strings.Contains(out, "ok: "+good) || !strings.Contains(errOut, "nope") {
		t.Fatalf("out=%q stderr=%q", out, errOut)
	}
	if !strings.Contains(errOut, "nope.jsonl") {
		t.Fatalf("missing file not reported: %q", errOut)
	}
}

func TestShowRendersTheFullTranscript(t *testing.T) {
	path := writeCassette(t, tickerCassette)
	code, out, _ := runCLI("show", path)
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	for _, want := range []string{
		"7 events (3 c2s, 3 s2c, close by server), 1.50s",
		"recorded 2026-07-10T09:15:00Z from ws://127.0.0.1:9601/feed",
		"→ c2s   text   subscribe:AAPL",
		`← s2c   text   {"sym":"AAPL","px":210.05,"seq":1}`,
		`[match: subset {"op":"ack"}]`,
		"[match: regex ^unsubscribe:[A-Z]+$]",
		`✕ close        1000 by server "feed complete"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("transcript misses %q:\n%s", want, out)
		}
	}

	// Long payloads are truncated with an ellipsis so transcripts stay
	// readable.
	long := strings.Repeat("x", 500)
	longPath := writeCassette(t, `{"wstage":1}`+"\n"+
		`{"t":0,"dir":"s2c","type":"text","data":"`+long+`"}`+"\n")
	code, out, _ = runCLI("show", longPath)
	if code != 0 || strings.Contains(out, long) || !strings.Contains(out, "…") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestReplayOncePassesAgainstAConformingClient(t *testing.T) {
	path := writeCassette(t, tickerCassette)
	addr, wait := server(t, "replay", "--once", "--listen", "127.0.0.1:0", path)

	pcode, pout, perr := runCLI("play", path, "ws://"+addr+"/feed")
	if pcode != 0 {
		t.Fatalf("play: code=%d out=%q err=%q", pcode, pout, perr)
	}
	if !strings.Contains(pout, "play: 3 sent, 3/3 expectations matched — PASS") {
		t.Fatalf("play out=%q", pout)
	}
	code, out := wait()
	if code != 0 || !strings.Contains(out, "session: 3 sent, 3/3 expectations matched — PASS") {
		t.Fatalf("replay: code=%d out=%q", code, out)
	}
}

func TestReplayOnceFailsAnOffScriptClientWithEvidence(t *testing.T) {
	srvPath := writeCassette(t, tickerCassette)
	bad := strings.Replace(tickerCassette, "subscribe:AAPL", "subscribe:DOGE", 1)
	cliPath := writeCassette(t, bad)

	addr, wait := server(t, "replay", "--once", "--listen", "127.0.0.1:0", srvPath)
	pcode, _, _ := runCLI("play", cliPath, "ws://"+addr+"/feed")
	if pcode != 1 {
		t.Fatalf("off-script play should fail too (it gets closed early), code=%d", pcode)
	}
	code, out := wait()
	if code != 1 {
		t.Fatalf("replay --once must exit 1 on mismatch, got %d (out=%q)", code, out)
	}
	if !strings.Contains(out, "FAIL") || !strings.Contains(out, `want "subscribe:AAPL", got "subscribe:DOGE"`) {
		t.Fatalf("verdict must quote both sides: %q", out)
	}
}

func TestReplayLenientReportsMismatchButFinishes(t *testing.T) {
	srvPath := writeCassette(t, tickerCassette)
	bad := strings.Replace(tickerCassette, "subscribe:AAPL", "subscribe:DOGE", 1)
	cliPath := writeCassette(t, bad)

	addr, wait := server(t, "replay", "--once", "--lenient", "--listen", "127.0.0.1:0", srvPath)
	pcode, pout, _ := runCLI("play", cliPath, "ws://"+addr+"/feed")
	if pcode != 0 {
		t.Fatalf("lenient server should let the client finish: %q", pout)
	}
	code, out := wait()
	if code != 1 || !strings.Contains(out, "2/3 expectations matched — FAIL") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestReplayRejectsAnInvalidCassetteBeforeListening(t *testing.T) {
	path := writeCassette(t, `{"wstage":1}`+"\n"+`{"t":0,"dir":"c2s","type":"text","data":"x","match":"fuzzy"}`+"\n")
	code, _, errOut := runCLI("replay", "--once", path)
	if code != 1 || !strings.Contains(errOut, "unknown match mode") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
}

func TestPlayReportsARefusedConnectionAsRuntimeError(t *testing.T) {
	path := writeCassette(t, tickerCassette)
	// Reserved port 1 on loopback is never listening.
	code, _, errOut := runCLI("play", path, "ws://127.0.0.1:1/feed", "--timeout", "1")
	if code != 3 || !strings.Contains(errOut, "play:") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
}

func TestRecordProxiesASessionAndWritesTheCassette(t *testing.T) {
	// Upstream = a replay of the ticker cassette; client = play of the
	// same cassette pointed at the recorder. The recorder sits in the
	// middle and must capture the identical conversation.
	srcPath := writeCassette(t, tickerCassette)
	outPath := filepath.Join(t.TempDir(), "rerecorded.jsonl")

	upAddr, upWait := server(t, "replay", "--once", "--listen", "127.0.0.1:0", srcPath)
	recAddr, recWait := server(t, "record", "ws://"+upAddr+"/feed",
		"--out", outPath, "--listen", "127.0.0.1:0", "--name", "rerecorded")

	pcode, pout, _ := runCLI("play", srcPath, "ws://"+recAddr+"/")
	if pcode != 0 {
		t.Fatalf("play through the recorder failed: %q", pout)
	}
	if code, out := upWait(); code != 0 {
		t.Fatalf("upstream replay: code=%d out=%q", code, out)
	}
	code, out := recWait()
	if code != 0 || !strings.Contains(out, "recorded 7 events (3 c2s, 3 s2c, close by server) → "+outPath) {
		t.Fatalf("record: code=%d out=%q", code, out)
	}

	cas, err := cassette.LoadFile(outPath)
	if err != nil {
		t.Fatalf("recorded cassette: %v", err)
	}
	if errs := cas.Validate(); len(errs) != 0 {
		t.Fatalf("recorded cassette invalid: %v", errs)
	}
	if cas.Header.Name != "rerecorded" || !strings.HasPrefix(cas.Header.URL, "ws://127.0.0.1:") {
		t.Fatalf("header: %+v", cas.Header)
	}
	// Same conversation, in order.
	var texts []string
	for _, ev := range cas.Events {
		if ev.Type == cassette.TypeText {
			texts = append(texts, ev.Dir+" "+ev.Data)
		}
	}
	want := []string{
		"c2s subscribe:AAPL",
		`s2c {"sym":"AAPL","px":210.05,"seq":1}`,
		`s2c {"sym":"AAPL","px":210.11,"seq":2}`,
		`c2s {"op":"ack","seq":2}`,
		`s2c {"sym":"AAPL","px":209.98,"seq":3}`,
		"c2s unsubscribe:AAPL",
	}
	if strings.Join(texts, "\n") != strings.Join(want, "\n") {
		t.Fatalf("recorded conversation:\n%s\nwant:\n%s", strings.Join(texts, "\n"), strings.Join(want, "\n"))
	}
	st := cas.Stats()
	if !st.HasClose || st.CloseBy != "server" || st.CloseCode != 1000 {
		t.Fatalf("close: %+v", st)
	}
}

func TestARecordedCassetteRoundTripsThroughReplayAndPlay(t *testing.T) {
	// Record a session, then verify the recording, replay it, and play the
	// original client script against the replay — the cassette-workflow
	// loop closed end to end through the real CLI.
	srcPath := writeCassette(t, tickerCassette)
	outPath := filepath.Join(t.TempDir(), "loop.jsonl")

	upAddr, upWait := server(t, "replay", "--once", "--listen", "127.0.0.1:0", srcPath)
	recAddr, recWait := server(t, "record", "ws://"+upAddr+"/feed", "--out", outPath, "--listen", "127.0.0.1:0")
	if code, _, _ := runCLI("play", srcPath, "ws://"+recAddr+"/"); code != 0 {
		t.Fatal("recording pass failed")
	}
	upWait()
	if code, _ := recWait(); code != 0 {
		t.Fatal("record failed")
	}

	if code, out, _ := runCLI("verify", outPath); code != 0 {
		t.Fatalf("verify of the recording: %q", out)
	}
	addr, wait := server(t, "replay", "--once", "--listen", "127.0.0.1:0", outPath)
	pcode, pout, _ := runCLI("play", srcPath, "ws://"+addr+"/")
	if pcode != 0 {
		t.Fatalf("original script vs recorded replay: %q", pout)
	}
	if code, out := wait(); code != 0 {
		t.Fatalf("recorded replay session: code=%d out=%q", code, out)
	}
}

func TestRecordValidatesItsTargetAndOutputFlags(t *testing.T) {
	// A non-ws:// target is refused before any socket is opened.
	code, _, errOut := runCLI("record", "https://example.test/feed", "--out", filepath.Join(t.TempDir(), "x.jsonl"))
	if code != 2 || !strings.Contains(errOut, "ws://") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	// --out is mandatory: recording without a destination is meaningless.
	code, _, errOut = runCLI("record", "ws://127.0.0.1:9601/feed")
	if code != 2 || !strings.Contains(errOut, "--out") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
}
