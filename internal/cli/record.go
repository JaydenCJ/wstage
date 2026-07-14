package cli

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/JaydenCJ/wstage/internal/cassette"
	"github.com/JaydenCJ/wstage/internal/record"
	"github.com/JaydenCJ/wstage/internal/ws"
)

// cmdRecord proxies exactly one client session to the target URL and
// writes everything that flows through to a cassette. The client connects
// to wstage; wstage dials the target (the target URL's path wins over the
// client's requested path). Events are flushed line by line, so even an
// interrupted recording is a parseable cassette.
func cmdRecord(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("record", stderr)
	out := fs.String("out", "", "cassette file to write (required)")
	listen := fs.String("listen", "127.0.0.1:9601", "address to listen on")
	name := fs.String("name", "", "cassette name (default: the output file's base name)")
	timeout := fs.Float64("timeout", 30, "upstream dial timeout in seconds")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(pos) != 1 {
		return usageErr(stderr, "record: exactly one target ws:// URL is required")
	}
	target := pos[0]
	if *out == "" {
		return usageErr(stderr, "record: --out <cassette> is required")
	}
	if u, err := url.Parse(target); err != nil || u.Scheme != "ws" {
		return usageErr(stderr, "record: target must be a ws:// URL, got %q", target)
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return runtimeErr(stderr, "record: %v", err)
	}
	defer ln.Close()
	fmt.Fprintf(stderr, "wstage: recording ws://%s/ → %s (out: %s)\n", ln.Addr(), target, *out)

	conn, err := ln.Accept()
	if err != nil {
		return runtimeErr(stderr, "record: %v", err)
	}
	defer conn.Close()
	client, _, err := ws.ServerAccept(conn)
	if err != nil {
		return runtimeErr(stderr, "record: client handshake: %v", err)
	}

	upstream, err := ws.Dial(target, time.Duration(*timeout*float64(time.Second)))
	if err != nil {
		client.WriteClose(1011, "wstage: upstream dial failed")
		return runtimeErr(stderr, "record: dialing upstream: %v", err)
	}
	defer upstream.Close()

	f, err := os.Create(*out)
	if err != nil {
		return runtimeErr(stderr, "record: %v", err)
	}
	defer f.Close()

	casName := *name
	if casName == "" {
		casName = strings.TrimSuffix(filepath.Base(*out), filepath.Ext(*out))
	}
	w := cassette.NewWriter(f)
	if err := w.WriteHeader(cassette.Header{
		Name:       casName,
		URL:        target,
		RecordedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		return runtimeErr(stderr, "record: %v", err)
	}

	sum, err := record.Session(client, upstream, w, nil)
	if err != nil {
		return runtimeErr(stderr, "%v", err)
	}
	closeBy := sum.CloseBy
	if closeBy == "" {
		closeBy = "nobody (session ended abruptly)"
	}
	fmt.Fprintf(stdout, "recorded %s (%d c2s, %d s2c, close by %s) → %s\n",
		cassette.EventsWord(sum.Events()), sum.C2S, sum.S2C, closeBy, *out)
	return exitOK
}
