// Package cassette defines the wstage cassette format: a JSON Lines file
// whose first line is a header object and whose remaining lines are session
// events in recorded order. The format is documented in
// docs/cassette-format.md; this package is the single implementation of
// reading, writing, and validating it.
package cassette

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// FormatVersion is the cassette format this build reads and writes.
const FormatVersion = 1

// Header is the first line of a cassette file.
type Header struct {
	Wstage     int    `json:"wstage"`
	Name       string `json:"name,omitempty"`
	URL        string `json:"url,omitempty"`
	RecordedAt string `json:"recorded_at,omitempty"`
}

// Event directions.
const (
	DirC2S   = "c2s"   // client → server data message
	DirS2C   = "s2c"   // server → client data message
	DirClose = "close" // closing handshake
)

// Payload types for data events.
const (
	TypeText   = "text"
	TypeBinary = "binary"
)

// Event is one line of a cassette after the header.
type Event struct {
	T       float64 `json:"t"`                  // seconds since session start
	Dir     string  `json:"dir"`                // c2s | s2c | close
	Type    string  `json:"type,omitempty"`     // text | binary (data events)
	Data    string  `json:"data,omitempty"`     // payload for text events
	DataB64 string  `json:"data_b64,omitempty"` // payload for binary events
	Match   string  `json:"match,omitempty"`    // expectation mode, default exact
	Expect  string  `json:"expect,omitempty"`   // expectation input when != Data
	Code    int     `json:"code,omitempty"`     // close status code
	By      string  `json:"by,omitempty"`       // close initiator: client | server
	Reason  string  `json:"reason,omitempty"`   // close reason
	Note    string  `json:"note,omitempty"`     // free-form annotation, ignored
}

// Binary reports whether the event carries a binary payload.
func (e *Event) Binary() bool { return e.Type == TypeBinary }

// Payload returns the event's payload bytes, decoding base64 for binary
// events.
func (e *Event) Payload() ([]byte, error) {
	if e.Binary() {
		b, err := base64.StdEncoding.DecodeString(e.DataB64)
		if err != nil {
			return nil, fmt.Errorf("cassette: bad base64 payload: %w", err)
		}
		return b, nil
	}
	return []byte(e.Data), nil
}

// Expectation returns the string the matcher compares against: Expect when
// set (e.g. a regex pattern), otherwise the recorded Data itself.
func (e *Event) Expectation() string {
	if e.Expect != "" {
		return e.Expect
	}
	return e.Data
}

// Cassette is a fully-loaded cassette file.
type Cassette struct {
	Header Header
	Events []Event
}

// Load parses a cassette from r, reporting syntax problems with 1-based
// line numbers. Blank lines are skipped, unknown JSON fields are tolerated
// for forward compatibility.
func Load(r io.Reader) (*Cassette, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)

	cas := &Cassette{}
	line := 0
	seenHeader := false
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		if !seenHeader {
			if err := json.Unmarshal([]byte(text), &cas.Header); err != nil {
				return nil, fmt.Errorf("line %d: bad header: %w", line, err)
			}
			if cas.Header.Wstage == 0 {
				return nil, fmt.Errorf(`line %d: not a wstage cassette header (missing "wstage": 1)`, line)
			}
			if cas.Header.Wstage != FormatVersion {
				return nil, fmt.Errorf("line %d: unsupported cassette format version %d (this build reads version %d)",
					line, cas.Header.Wstage, FormatVersion)
			}
			seenHeader = true
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(text), &ev); err != nil {
			return nil, fmt.Errorf("line %d: bad event: %w", line, err)
		}
		cas.Events = append(cas.Events, ev)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !seenHeader {
		return nil, fmt.Errorf("empty cassette: expected a header line with %q", `"wstage": 1`)
	}
	return cas, nil
}

// LoadFile loads and parses the cassette at path.
func LoadFile(path string) (*Cassette, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Load(f)
}

// Validate performs semantic checks beyond JSON syntax and returns a
// human-readable message per problem (empty slice = valid).
func (c *Cassette) Validate() []string {
	var errs []string
	add := func(i int, format string, a ...any) {
		errs = append(errs, fmt.Sprintf("event %d: %s", i+1, fmt.Sprintf(format, a...)))
	}

	prev := 0.0
	closeSeen := false
	for i := range c.Events {
		ev := &c.Events[i]
		if closeSeen {
			add(i, "events after the close event are unreachable")
			break
		}
		switch ev.Dir {
		case DirC2S, DirS2C:
			validateData(ev, i, add)
		case DirClose:
			validateClose(ev, i, add)
			closeSeen = true
		default:
			add(i, "unknown dir %q (want c2s, s2c, or close)", ev.Dir)
		}
		if ev.T < 0 {
			add(i, "negative timestamp t=%v", ev.T)
		} else if ev.T < prev {
			add(i, "timestamp goes backwards (t=%v after t=%v)", ev.T, prev)
		} else {
			prev = ev.T
		}
	}
	return errs
}

func validateData(ev *Event, i int, add func(int, string, ...any)) {
	switch ev.Type {
	case TypeText:
		if ev.DataB64 != "" {
			add(i, `text events carry "data", not "data_b64"`)
		}
	case TypeBinary:
		if ev.Data != "" {
			add(i, `binary events carry "data_b64", not "data"`)
		}
		if _, err := base64.StdEncoding.DecodeString(ev.DataB64); err != nil {
			add(i, `"data_b64" is not valid base64: %v`, err)
		}
	default:
		add(i, "unknown type %q (want text or binary)", ev.Type)
		return
	}

	mode := ev.Match
	if mode == "" {
		mode = MatchExact
	}
	if !ValidMatch(mode) {
		add(i, "unknown match mode %q", ev.Match)
		return
	}
	if ev.Binary() && mode != MatchExact && mode != MatchAny {
		add(i, "binary events support only exact or any matching, not %q", mode)
		return
	}
	switch mode {
	case MatchRegex:
		if _, err := regexp.Compile(ev.Expectation()); err != nil {
			add(i, "expectation is not a valid regex: %v", err)
		}
	case MatchJSON, MatchSubset:
		if !json.Valid([]byte(ev.Expectation())) {
			add(i, "%s expectation is not valid JSON: %q", mode, ev.Expectation())
		}
	}
}

func validateClose(ev *Event, i int, add func(int, string, ...any)) {
	if ev.Type != "" || ev.Data != "" || ev.DataB64 != "" {
		add(i, "close events carry no payload")
	}
	switch ev.By {
	case "", "client", "server":
	default:
		add(i, `unknown close initiator %q (want "client" or "server")`, ev.By)
	}
	if ev.Code != 0 && (ev.Code < 1000 || ev.Code > 4999) {
		add(i, "close code %d is outside the valid range 1000–4999", ev.Code)
	}
}

// Stats summarizes a cassette for one-line reporting.
type Stats struct {
	Events    int     // total events including close
	C2S       int     // client → server data messages
	S2C       int     // server → client data messages
	HasClose  bool    // a close event is recorded
	CloseBy   string  // close initiator ("server" when unspecified)
	CloseCode int     // recorded close code (0 = unspecified)
	Duration  float64 // timestamp of the last event, seconds
}

// Stats computes summary counts over the cassette's events.
func (c *Cassette) Stats() Stats {
	st := Stats{Events: len(c.Events)}
	for i := range c.Events {
		ev := &c.Events[i]
		switch ev.Dir {
		case DirC2S:
			st.C2S++
		case DirS2C:
			st.S2C++
		case DirClose:
			st.HasClose = true
			st.CloseBy = ev.By
			if st.CloseBy == "" {
				st.CloseBy = "server"
			}
			st.CloseCode = ev.Code
		}
		if ev.T > st.Duration {
			st.Duration = ev.T
		}
	}
	return st
}

// Summary renders the stats as the one-liner used across the CLI, e.g.
// "7 events (3 c2s, 3 s2c, close by server), 1.50s".
func (st Stats) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%d c2s, %d s2c", EventsWord(st.Events), st.C2S, st.S2C)
	if st.HasClose {
		fmt.Fprintf(&b, ", close by %s", st.CloseBy)
	}
	fmt.Fprintf(&b, "), %.2fs", st.Duration)
	return b.String()
}

// EventsWord renders an event count with the right plural, e.g. "1 event",
// "7 events" — shared by every CLI line that reports a count.
func EventsWord(n int) string {
	if n == 1 {
		return "1 event"
	}
	return fmt.Sprintf("%d events", n)
}

// Writer streams a cassette to w line by line, so a recording interrupted
// mid-session still leaves a parseable file.
type Writer struct {
	w io.Writer
}

// NewWriter returns a Writer emitting to w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// WriteHeader writes the header line; the format version is always stamped
// to the version this build writes.
func (wr *Writer) WriteHeader(h Header) error {
	h.Wstage = FormatVersion
	return wr.writeLine(h)
}

// WriteEvent appends one event line.
func (wr *Writer) WriteEvent(e Event) error { return wr.writeLine(e) }

func (wr *Writer) writeLine(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = wr.w.Write(append(b, '\n'))
	return err
}
