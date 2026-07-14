package cli

import (
	"io"
	"time"

	"github.com/JaydenCJ/wstage/internal/replay"
	"github.com/JaydenCJ/wstage/internal/ws"
)

// cmdPlay drives a live server as the scripted client: it connects to the
// URL, sends the cassette's c2s messages, and asserts the server's replies
// against the s2c events. Playing a cassette against `wstage replay` of the
// same cassette must PASS on both sides — that property is what the smoke
// test and the examples lean on.
func cmdPlay(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("play", stderr)
	lenient := fs.Bool("lenient", false, "report mismatches but keep the session going")
	speed := fs.Float64("speed", 0, "multiplier on recorded gaps (0 = no delays)")
	timeout := fs.Float64("timeout", 30, "dial and per-read timeout in seconds (0 = none)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(pos) != 2 {
		return usageErr(stderr, "play: a cassette path and a ws:// URL are required")
	}
	cas, code := loadValidated(pos[0], stderr)
	if cas == nil {
		return code
	}

	// A zero timeout means "no deadline" for the dial too, matching the
	// flag's contract (net.DialTimeout treats 0 as no timeout).
	conn, err := ws.Dial(pos[1], time.Duration(*timeout*float64(time.Second)))
	if err != nil {
		return runtimeErr(stderr, "play: %v", err)
	}
	defer conn.Close()

	res, err := replay.Run(conn, cas, replay.Options{
		Role:    replay.RoleClient,
		Lenient: *lenient,
		Speed:   *speed,
		Timeout: time.Duration(*timeout * float64(time.Second)),
	})
	if err != nil {
		return runtimeErr(stderr, "play: %v", err)
	}
	return printResult(stdout, "play", res)
}
