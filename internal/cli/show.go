package cli

import (
	"fmt"
	"io"

	"github.com/JaydenCJ/wstage/internal/cassette"
)

// cmdShow prints a cassette as a human-readable transcript: one line per
// event with its timestamp, direction arrow, type, payload preview, and —
// for expectations that are not plain exact matches — the match rule.
func cmdShow(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("show", stderr)
	paths, err := parseFlags(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(paths) != 1 {
		return usageErr(stderr, "show: exactly one cassette path is required")
	}
	path := paths[0]
	cas, code := loadValidated(path, stderr)
	if cas == nil {
		return code
	}

	fmt.Fprintf(stdout, "%s — %s\n", path, cas.Stats().Summary())
	if cas.Header.URL != "" || cas.Header.RecordedAt != "" {
		fmt.Fprintf(stdout, "recorded %s from %s\n", orDash(cas.Header.RecordedAt), orDash(cas.Header.URL))
	}
	fmt.Fprintln(stdout)
	for i := range cas.Events {
		ev := &cas.Events[i]
		fmt.Fprintf(stdout, " %3d  %7.3f  %s\n", i+1, ev.T, renderEvent(ev))
	}
	return exitOK
}

func renderEvent(ev *cassette.Event) string {
	switch ev.Dir {
	case cassette.DirC2S:
		return fmt.Sprintf("→ c2s   %-6s %s%s", ev.Type, preview(ev), matchNote(ev))
	case cassette.DirS2C:
		return fmt.Sprintf("← s2c   %-6s %s%s", ev.Type, preview(ev), matchNote(ev))
	default:
		by := ev.By
		if by == "" {
			by = "server"
		}
		code := ev.Code
		if code == 0 {
			code = 1000
		}
		out := fmt.Sprintf("✕ close        %d by %s", code, by)
		if ev.Reason != "" {
			out += fmt.Sprintf(" %q", ev.Reason)
		}
		return out
	}
}

func preview(ev *cassette.Event) string {
	if ev.Binary() {
		p, err := ev.Payload()
		if err != nil {
			return "(bad base64)"
		}
		return fmt.Sprintf("(%d bytes binary)", len(p))
	}
	return cassette.Truncate(ev.Data, 60)
}

func matchNote(ev *cassette.Event) string {
	if ev.Match == "" || ev.Match == cassette.MatchExact {
		return ""
	}
	if ev.Match == cassette.MatchAny {
		return "   [match: any]"
	}
	return fmt.Sprintf("   [match: %s %s]", ev.Match, ev.Expectation())
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
