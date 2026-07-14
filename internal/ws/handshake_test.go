// Handshake tests: the RFC 6455 accept-key vector, server-side validation
// of malformed upgrade requests, and a full client↔server handshake over
// an in-memory pipe.
package ws

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

const sampleUpgradeRequest = "GET /feed HTTP/1.1\r\n" +
	"Host: example.test\r\n" +
	"Upgrade: websocket\r\n" +
	"Connection: keep-alive, Upgrade\r\n" +
	"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
	"Sec-WebSocket-Version: 13\r\n\r\n"

func TestAcceptKeyMatchesRFCExample(t *testing.T) {
	// The exact vector from RFC 6455 §1.3.
	got := AcceptKey("dGhlIHNhbXBsZSBub25jZQ==")
	if got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("AcceptKey = %q", got)
	}
}

// handshakeExchange writes rawRequest from the client end of a pipe, runs
// ServerAccept on the server end, and returns the outcome plus the raw
// HTTP response the client saw.
func handshakeExchange(t *testing.T, rawRequest string) (conn *Conn, info *ReqInfo, srvErr error, resp *http.Response) {
	t.Helper()
	c, s := net.Pipe()
	t.Cleanup(func() { c.Close(); s.Close() })

	respc := make(chan *http.Response, 1)
	go func() {
		io.WriteString(c, rawRequest)
		r, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodGet})
		if err != nil {
			respc <- nil
			return
		}
		respc <- r
	}()
	conn, info, srvErr = ServerAccept(s)
	return conn, info, srvErr, <-respc
}

func TestServerAcceptCompletesAValidUpgrade(t *testing.T) {
	conn, info, err, resp := handshakeExchange(t, sampleUpgradeRequest)
	if err != nil {
		t.Fatalf("ServerAccept: %v", err)
	}
	if conn == nil || info.Path != "/feed" || info.Host != "example.test" {
		t.Fatalf("bad ReqInfo: %+v", info)
	}
	if resp.StatusCode != 101 {
		t.Fatalf("client saw status %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("accept header = %q", got)
	}
}

func TestServerAcceptRejectsMalformedUpgradeRequests(t *testing.T) {
	cases := []struct {
		name, old, new string
		status         int
	}{
		{"POST instead of GET", "GET", "POST", 405},
		{"missing Upgrade header", "Upgrade: websocket\r\n", "", 400},
		{"missing Sec-WebSocket-Key", "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n", "", 400},
	}
	for _, tc := range cases {
		req := strings.Replace(sampleUpgradeRequest, tc.old, tc.new, 1)
		_, _, err, resp := handshakeExchange(t, req)
		assertHandshakeStatus(t, err, resp, tc.status)
	}
}

func TestServerAcceptRejectsWrongVersionWith426(t *testing.T) {
	req := strings.Replace(sampleUpgradeRequest, "Version: 13", "Version: 8", 1)
	_, _, err, resp := handshakeExchange(t, req)
	assertHandshakeStatus(t, err, resp, 426)
	if v := resp.Header.Get("Sec-WebSocket-Version"); v != "13" {
		t.Fatalf("426 must advertise version 13, got %q", v)
	}
}

func TestClientHandshakeAgainstServerAcceptRoundTrips(t *testing.T) {
	c, s := net.Pipe()
	t.Cleanup(func() { c.Close(); s.Close() })

	type srvOut struct {
		conn *Conn
		info *ReqInfo
		err  error
	}
	srv := make(chan srvOut, 1)
	go func() {
		conn, info, err := ServerAccept(s)
		srv <- srvOut{conn, info, err}
	}()

	u, _ := url.Parse("ws://example.test/rooms/1?token=abc")
	client, err := ClientHandshake(c, u)
	if err != nil {
		t.Fatalf("ClientHandshake: %v", err)
	}
	out := <-srv
	if out.err != nil {
		t.Fatalf("ServerAccept: %v", out.err)
	}
	if out.info.Path != "/rooms/1?token=abc" {
		t.Fatalf("server saw path %q", out.info.Path)
	}

	// The handshaken pair must carry a real message.
	send(t, func() error { return client.WriteMessage(OpText, []byte("post-handshake")) })
	msg, err := out.conn.ReadMessage()
	if err != nil || string(msg.Data) != "post-handshake" {
		t.Fatalf("message after handshake: %q, %v", msg.Data, err)
	}
}

// fakeServer answers one upgrade request with a canned raw response.
func fakeServer(t *testing.T, respond func(key string) string) *Conn {
	t.Helper()
	c, s := net.Pipe()
	t.Cleanup(func() { c.Close(); s.Close() })
	errc := make(chan error, 1)
	var client *Conn
	go func() {
		req, err := http.ReadRequest(bufio.NewReader(s))
		if err != nil {
			errc <- err
			return
		}
		_, err = io.WriteString(s, respond(req.Header.Get("Sec-WebSocket-Key")))
		errc <- err
	}()
	u, _ := url.Parse("ws://example.test/")
	var err error
	client, err = ClientHandshake(c, u)
	if serr := <-errc; serr != nil {
		t.Fatalf("fake server: %v", serr)
	}
	if err == nil {
		t.Fatal("want ClientHandshake to fail")
	}
	if client != nil {
		t.Fatal("failed handshake must not return a Conn")
	}
	return nil
}

func TestClientHandshakeRejectsBadServerResponses(t *testing.T) {
	// A 101 carrying the wrong Sec-WebSocket-Accept.
	fakeServer(t, func(string) string {
		return "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: bm90IHRoZSByaWdodCBrZXk=\r\n\r\n"
	})
	// A refusal that never upgrades.
	fakeServer(t, func(string) string {
		return "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n"
	})
	// A 101 with the right key but no Upgrade header.
	fakeServer(t, func(key string) string {
		return "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + AcceptKey(key) + "\r\n\r\n"
	})
}

func TestDialRejectsUnsupportedSchemes(t *testing.T) {
	if _, err := Dial("wss://example.test/", time.Second); err == nil || !strings.Contains(err.Error(), "wss") {
		t.Fatalf("wss:// should be rejected with a pointer to ws://, got %v", err)
	}
	if _, err := Dial("http://example.test/", time.Second); err == nil || !strings.Contains(err.Error(), "ws://") {
		t.Fatalf("http:// should be rejected, got %v", err)
	}
}

func assertHandshakeStatus(t *testing.T, err error, resp *http.Response, status int) {
	t.Helper()
	var he *HandshakeError
	if !errors.As(err, &he) || he.Status != status {
		t.Fatalf("want HandshakeError %d, got %v", status, err)
	}
	if resp == nil || resp.StatusCode != status {
		t.Fatalf("client should see HTTP %d, got %+v", status, resp)
	}
}
