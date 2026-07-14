package cli

import (
	"fmt"
	"io"

	"github.com/JaydenCJ/wstage/internal/cassette"
)

// cmdVerify parses and validates one or more cassettes without opening any
// sockets, so it is safe to run in CI-like gates.
func cmdVerify(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("verify", stderr)
	paths, err := parseFlags(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(paths) == 0 {
		return usageErr(stderr, "verify: at least one cassette path is required")
	}

	bad := 0
	for _, p := range paths {
		cas, err := cassette.LoadFile(p)
		if err != nil {
			fmt.Fprintf(stderr, "wstage: %s: %v\n", p, err)
			bad++
			continue
		}
		if errs := cas.Validate(); len(errs) > 0 {
			for _, e := range errs {
				fmt.Fprintf(stderr, "wstage: %s: %s\n", p, e)
			}
			bad++
			continue
		}
		fmt.Fprintf(stdout, "ok: %s — %s\n", p, cas.Stats().Summary())
	}
	if bad > 0 {
		return exitFail
	}
	return exitOK
}
