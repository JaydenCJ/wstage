// Command wstage records WebSocket sessions to cassette files and replays
// them as a scripted mock server (or scripted client) for tests.
package main

import (
	"os"

	"github.com/JaydenCJ/wstage/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
