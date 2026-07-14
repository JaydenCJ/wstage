package cassette

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Match modes for expectation events. The mode names appear verbatim in
// cassette files, so they are part of the format.
const (
	MatchExact  = "exact"  // byte-for-byte equality (default)
	MatchPrefix = "prefix" // message starts with the expectation
	MatchRegex  = "regex"  // expectation is an RE2 pattern
	MatchJSON   = "json"   // structural JSON equality (key order ignored)
	MatchSubset = "subset" // expectation is a structural subset of the message
	MatchAny    = "any"    // any single message matches
)

// ValidMatch reports whether m names a known match mode.
func ValidMatch(m string) bool {
	switch m {
	case MatchExact, MatchPrefix, MatchRegex, MatchJSON, MatchSubset, MatchAny:
		return true
	}
	return false
}

// MatchMessage checks a received data message against an expectation event.
// binary reports whether the received message was a binary frame. It returns
// ok, plus a human-readable reason when the message does not match — the
// reason is what test users see, so it names both sides concretely.
func MatchMessage(e *Event, binary bool, payload []byte) (bool, string) {
	mode := e.Match
	if mode == "" {
		mode = MatchExact
	}
	if mode == MatchAny {
		return true, ""
	}
	if binary != e.Binary() {
		return false, fmt.Sprintf("type mismatch: want %s, got %s", typeName(e.Binary()), typeName(binary))
	}
	if e.Binary() {
		want, err := e.Payload()
		if err != nil {
			return false, err.Error()
		}
		if bytes.Equal(want, payload) {
			return true, ""
		}
		return false, fmt.Sprintf("binary payload differs (want %d bytes, got %d bytes)", len(want), len(payload))
	}

	got := string(payload)
	want := e.Expectation()
	switch mode {
	case MatchExact:
		if got == want {
			return true, ""
		}
		return false, fmt.Sprintf("want %s, got %s", quote(want), quote(got))
	case MatchPrefix:
		if strings.HasPrefix(got, want) {
			return true, ""
		}
		return false, fmt.Sprintf("want prefix %s, got %s", quote(want), quote(got))
	case MatchRegex:
		re, err := regexp.Compile(want)
		if err != nil {
			return false, fmt.Sprintf("cassette regex does not compile: %v", err)
		}
		if re.MatchString(got) {
			return true, ""
		}
		return false, fmt.Sprintf("regex %s does not match %s", quote(want), quote(got))
	case MatchJSON:
		return jsonEqual(want, got)
	case MatchSubset:
		return jsonSubset(want, got)
	}
	return false, fmt.Sprintf("unknown match mode %q", e.Match)
}

func typeName(binary bool) string {
	if binary {
		return TypeBinary
	}
	return TypeText
}

// quote renders a payload for mismatch messages, truncated so a huge frame
// cannot flood test logs.
func quote(s string) string {
	return fmt.Sprintf("%q", Truncate(s, 120))
}

// Truncate shortens s to at most max bytes for display, appending an
// ellipsis. The cut backs up to a rune boundary so a multi-byte character
// is never split into mojibake mid-sequence.
func Truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func jsonEqual(want, got string) (bool, string) {
	var w, g any
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		return false, fmt.Sprintf("cassette expectation is not valid JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		return false, fmt.Sprintf("message is not valid JSON: %v", err)
	}
	if reflect.DeepEqual(w, g) {
		return true, ""
	}
	return false, fmt.Sprintf("JSON documents differ: want %s, got %s", quote(want), quote(got))
}

func jsonSubset(want, got string) (bool, string) {
	var w, g any
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		return false, fmt.Sprintf("cassette expectation is not valid JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		return false, fmt.Sprintf("message is not valid JSON: %v", err)
	}
	if ok, path := subsetValue(w, g, "$"); !ok {
		return false, fmt.Sprintf("subset mismatch at %s (want %s ⊆ got %s)", path, quote(want), quote(got))
	}
	return true, ""
}

// subsetValue reports whether want is a structural subset of got. Objects
// may carry extra keys in got; arrays must have equal length and match
// element-wise; scalars must be equal. path tracks the JSON-path of the
// first mismatch for the error message.
func subsetValue(want, got any, path string) (bool, string) {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return false, path + " (want an object)"
		}
		for k, wv := range w {
			gv, present := g[k]
			if !present {
				return false, path + "." + k + " (missing)"
			}
			if ok, p := subsetValue(wv, gv, path+"."+k); !ok {
				return false, p
			}
		}
		return true, ""
	case []any:
		g, ok := got.([]any)
		if !ok {
			return false, path + " (want an array)"
		}
		if len(w) != len(g) {
			return false, fmt.Sprintf("%s (array length: want %d, got %d)", path, len(w), len(g))
		}
		for i := range w {
			if ok, p := subsetValue(w[i], g[i], fmt.Sprintf("%s[%d]", path, i)); !ok {
				return false, p
			}
		}
		return true, ""
	default:
		if reflect.DeepEqual(want, got) {
			return true, ""
		}
		return false, path
	}
}
