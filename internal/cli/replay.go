package cli

import (
	"fmt"
	"io"
	"net"
	"time"

	"github.com/JaydenCJ/wstage/internal/cassette"
	"github.com/JaydenCJ/wstage/internal/replay"
	"github.com/JaydenCJ/wstage/internal/ws"
)

// cmdReplay serves a cassette as a scripted mock server. Every incoming
// connection gets a fresh run of the cassette from the top. With --once
// the server handles exactly one session and its exit code reports whether
// all expectations matched — the shape a test harness wants.
func cmdReplay(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("replay", stderr)
	listen := fs.String("listen", "127.0.0.1:9601", "address to listen on")
	once := fs.Bool("once", false, "serve one session, then exit with its result")
	lenient := fs.Bool("lenient", false, "report mismatches but keep the session going")
	speed := fs.Float64("speed", 0, "multiplier on recorded gaps (0 = no delays)")
	timeout := fs.Float64("timeout", 30, "per-read timeout in seconds (0 = none)")
	paths, err := parseFlags(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(paths) != 1 {
		return usageErr(stderr, "replay: exactly one cassette path is required")
	}
	cas, code := loadValidated(paths[0], stderr)
	if cas == nil {
		return code
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return runtimeErr(stderr, "replay: %v", err)
	}
	defer ln.Close()
	fmt.Fprintf(stderr, "wstage: replay listening on ws://%s/ — %s (%s)\n",
		ln.Addr(), paths[0], cas.Stats().Summary())

	opt := replay.Options{
		Role:    replay.RoleServer,
		Lenient: *lenient,
		Speed:   *speed,
		Timeout: time.Duration(*timeout * float64(time.Second)),
	}

	if *once {
		conn, err := ln.Accept()
		if err != nil {
			return runtimeErr(stderr, "replay: %v", err)
		}
		return serveSession(conn, cas, opt, "session", stdout, stderr)
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			return runtimeErr(stderr, "replay: %v", err)
		}
		go serveSession(conn, cas, opt, "session", stderr, stderr)
	}
}

// serveSession upgrades one TCP connection and runs the cassette against
// it, printing the session verdict to out.
func serveSession(conn net.Conn, cas *cassette.Cassette, opt replay.Options, label string, out, stderr io.Writer) int {
	defer conn.Close()
	wsc, _, err := ws.ServerAccept(conn)
	if err != nil {
		return runtimeErr(stderr, "replay: handshake: %v", err)
	}
	res, err := replay.Run(wsc, cas, opt)
	if err != nil {
		return runtimeErr(stderr, "replay: %v", err)
	}
	return printResult(out, label, res)
}
