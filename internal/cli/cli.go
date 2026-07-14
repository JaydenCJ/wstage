// Package cli implements the wstage command-line interface. All commands
// are dispatched through Run so tests can drive the full CLI in-process
// with plain buffers and pipes.
package cli

import (
	"flag"
	"fmt"
	"io"

	"github.com/JaydenCJ/wstage/internal/cassette"
	"github.com/JaydenCJ/wstage/internal/replay"
	"github.com/JaydenCJ/wstage/internal/version"
)

// Exit codes, kept stable so scripts can branch on them.
const (
	exitOK      = 0
	exitFail    = 1 // expectation or verification failure
	exitUsage   = 2
	exitRuntime = 3
)

const usage = `wstage — record WebSocket sessions and replay them as a scripted mock server

Usage:
  wstage record <ws-url> --out <cassette> [--listen 127.0.0.1:9601] [--name NAME] [--timeout SEC]
  wstage replay <cassette> [--listen 127.0.0.1:9601] [--once] [--lenient] [--speed X] [--timeout SEC]
  wstage play   <cassette> <ws-url> [--lenient] [--speed X] [--timeout SEC]
  wstage show   <cassette>
  wstage verify <cassette> [<cassette> ...]
  wstage version

Commands:
  record   proxy one client session to the target URL, writing a cassette
  replay   serve a cassette as a scripted mock server for a client under test
  play     drive a live server as the scripted client from a cassette
  show     print a cassette as a human-readable transcript
  verify   parse and validate cassettes without opening any sockets
  version  print the wstage version

Exit codes: 0 ok, 1 expectation/verify failure, 2 usage error, 3 runtime error.
`

// Run executes the wstage CLI and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "record":
		return cmdRecord(rest, stdout, stderr)
	case "replay":
		return cmdReplay(rest, stdout, stderr)
	case "play":
		return cmdPlay(rest, stdout, stderr)
	case "show":
		return cmdShow(rest, stdout, stderr)
	case "verify":
		return cmdVerify(rest, stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "wstage %s\n", version.Version)
		return exitOK
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return exitOK
	}
	fmt.Fprintf(stderr, "wstage: unknown command %q\n\n%s", cmd, usage)
	return exitUsage
}

func usageErr(stderr io.Writer, format string, a ...any) int {
	fmt.Fprintf(stderr, "wstage: "+format+"\n", a...)
	return exitUsage
}

func runtimeErr(stderr io.Writer, format string, a ...any) int {
	fmt.Fprintf(stderr, "wstage: "+format+"\n", a...)
	return exitRuntime
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// parseFlags parses fs against args while allowing flags to appear after
// positional arguments (Go's flag package stops at the first positional).
// It returns the positional arguments in order.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// loadValidated loads a cassette and runs semantic validation, printing
// every problem. It returns nil (and a non-zero code) on failure.
func loadValidated(path string, stderr io.Writer) (*cassette.Cassette, int) {
	cas, err := cassette.LoadFile(path)
	if err != nil {
		fmt.Fprintf(stderr, "wstage: %s: %v\n", path, err)
		return nil, exitFail
	}
	if errs := cas.Validate(); len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintf(stderr, "wstage: %s: %s\n", path, e)
		}
		return nil, exitFail
	}
	return cas, exitOK
}

// printResult renders a session result and returns the exit code for it.
func printResult(w io.Writer, label string, res *replay.Result) int {
	verdict := "PASS"
	if !res.OK() {
		verdict = "FAIL"
	}
	noun := "expectations"
	if res.Expected == 1 {
		noun = "expectation"
	}
	fmt.Fprintf(w, "%s: %d sent, %d/%d %s matched — %s\n",
		label, res.Sent, res.Matched, res.Expected, noun, verdict)
	for _, m := range res.Mismatches {
		fmt.Fprintf(w, "  expectation %d: %s\n", m.Index, m.Why)
	}
	if res.OK() {
		return exitOK
	}
	return exitFail
}
