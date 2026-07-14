package ws

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// acceptGUID is the fixed GUID from RFC 6455 §1.3 used to derive
// Sec-WebSocket-Accept from Sec-WebSocket-Key.
const acceptGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// AcceptKey computes the Sec-WebSocket-Accept value for a client key.
func AcceptKey(key string) string {
	sum := sha1.Sum([]byte(key + acceptGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// ReqInfo describes the client's upgrade request, for logging and recording.
type ReqInfo struct {
	Path   string
	Host   string
	Header http.Header
}

// HandshakeError is a rejected upgrade request. Status is the HTTP status
// that was written back to the client before failing.
type HandshakeError struct {
	Status int
	Msg    string
}

func (e *HandshakeError) Error() string {
	return fmt.Sprintf("ws: handshake rejected (%d): %s", e.Status, e.Msg)
}

// headerHasToken reports whether a comma-separated header contains token,
// case-insensitively — required because clients send values like
// "Connection: keep-alive, Upgrade".
func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// ServerAccept performs the server side of the upgrade handshake on rw:
// it reads the HTTP request, validates it per RFC 6455 §4.2, and writes
// either the 101 response or an HTTP error. On success it returns a
// server-role Conn that shares rw.
func ServerAccept(rw io.ReadWriteCloser) (*Conn, *ReqInfo, error) {
	br := bufio.NewReader(rw)
	req, err := http.ReadRequest(br)
	if err != nil {
		return nil, nil, fmt.Errorf("ws: reading upgrade request: %w", err)
	}
	if herr := checkUpgrade(req); herr != nil {
		writeHTTPError(rw, herr)
		return nil, nil, herr
	}
	key := req.Header.Get("Sec-WebSocket-Key")
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + AcceptKey(key) + "\r\n\r\n"
	if _, err := io.WriteString(rw, resp); err != nil {
		return nil, nil, fmt.Errorf("ws: writing 101 response: %w", err)
	}
	info := &ReqInfo{Path: req.URL.RequestURI(), Host: req.Host, Header: req.Header}
	return newConn(rw, br, false), info, nil
}

func checkUpgrade(req *http.Request) *HandshakeError {
	if req.Method != http.MethodGet {
		return &HandshakeError{Status: 405, Msg: "upgrade requests must use GET"}
	}
	if !headerHasToken(req.Header, "Connection", "upgrade") {
		return &HandshakeError{Status: 400, Msg: `missing "Connection: Upgrade"`}
	}
	if !headerHasToken(req.Header, "Upgrade", "websocket") {
		return &HandshakeError{Status: 400, Msg: `missing "Upgrade: websocket"`}
	}
	if req.Header.Get("Sec-WebSocket-Key") == "" {
		return &HandshakeError{Status: 400, Msg: "missing Sec-WebSocket-Key"}
	}
	if v := req.Header.Get("Sec-WebSocket-Version"); v != "13" {
		return &HandshakeError{Status: 426, Msg: "unsupported Sec-WebSocket-Version " + v}
	}
	return nil
}

func writeHTTPError(w io.Writer, herr *HandshakeError) {
	extra := ""
	if herr.Status == 426 {
		extra = "Sec-WebSocket-Version: 13\r\n"
	}
	body := herr.Msg + "\n"
	fmt.Fprintf(w, "HTTP/1.1 %d %s\r\n%sContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		herr.Status, http.StatusText(herr.Status), extra, len(body), body)
}

// ClientHandshake performs the client side of the upgrade over rw for the
// given ws:// URL, verifying the Sec-WebSocket-Accept echo. On success it
// returns a client-role Conn that shares rw.
func ClientHandshake(rw io.ReadWriteCloser, u *url.URL) (*Conn, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(nonce)

	req := "GET " + u.RequestURI() + " HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(rw, req); err != nil {
		return nil, fmt.Errorf("ws: writing upgrade request: %w", err)
	}

	br := bufio.NewReader(rw)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		return nil, fmt.Errorf("ws: reading upgrade response: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return nil, fmt.Errorf("ws: server refused the upgrade: %s", resp.Status)
	}
	if !headerHasToken(resp.Header, "Upgrade", "websocket") {
		return nil, errors.New(`ws: 101 response is missing "Upgrade: websocket"`)
	}
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != AcceptKey(key) {
		return nil, fmt.Errorf("ws: bad Sec-WebSocket-Accept %q", got)
	}
	return newConn(rw, br, true), nil
}

// Dial connects to a ws:// URL over TCP and completes the client handshake.
func Dial(rawurl string, timeout time.Duration) (*Conn, error) {
	u, err := url.Parse(rawurl)
	if err != nil {
		return nil, fmt.Errorf("ws: parsing URL: %w", err)
	}
	switch u.Scheme {
	case "ws":
	case "wss":
		return nil, errors.New("ws: wss:// (TLS) is not supported in v0.1.0; use ws://")
	default:
		return nil, fmt.Errorf("ws: URL scheme must be ws://, got %q", u.Scheme)
	}
	hostport := u.Host
	if u.Port() == "" {
		hostport = net.JoinHostPort(u.Hostname(), "80")
	}
	nc, err := net.DialTimeout("tcp", hostport, timeout)
	if err != nil {
		return nil, err
	}
	conn, err := ClientHandshake(nc, u)
	if err != nil {
		nc.Close()
		return nil, err
	}
	return conn, nil
}
