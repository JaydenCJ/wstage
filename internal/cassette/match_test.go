// Matcher tests: every match mode's pass and fail paths, and the quality
// of the mismatch reasons — those strings are the tool's error UX.
package cassette

import (
	"strings"
	"testing"
)

func textEvent(data, match, expect string) *Event {
	return &Event{Dir: DirC2S, Type: TypeText, Data: data, Match: match, Expect: expect}
}

func mustMatch(t *testing.T, e *Event, binary bool, payload string) {
	t.Helper()
	ok, why := MatchMessage(e, binary, []byte(payload))
	if !ok {
		t.Fatalf("expected a match, got mismatch: %s", why)
	}
}

func mustMismatch(t *testing.T, e *Event, binary bool, payload, wantWhy string) string {
	t.Helper()
	ok, why := MatchMessage(e, binary, []byte(payload))
	if ok {
		t.Fatalf("expected a mismatch mentioning %q, but it matched", wantWhy)
	}
	if !strings.Contains(why, wantWhy) {
		t.Fatalf("reason %q does not mention %q", why, wantWhy)
	}
	return why
}

func TestExactMatchIsTheDefaultModeAndQuotesBothSidesOnMismatch(t *testing.T) {
	mustMatch(t, textEvent("ping", "", ""), false, "ping")
	why := mustMismatch(t, textEvent("ping", "", ""), false, "pong", `want "ping"`)
	if !strings.Contains(why, `got "pong"`) {
		t.Fatalf("reason %q should quote the received payload", why)
	}
}

func TestPrefixMatchUsesTheExpectField(t *testing.T) {
	e := textEvent("unsubscribe:AAPL", MatchPrefix, "unsubscribe:")
	mustMatch(t, e, false, "unsubscribe:TSLA")
	mustMismatch(t, e, false, "subscribe:TSLA", "want prefix")
}

func TestRegexMatchAppliesTheRE2PatternAndFailsSafelyOnBadPatterns(t *testing.T) {
	e := textEvent("auth:token-1234", MatchRegex, `^auth:token-\d+$`)
	mustMatch(t, e, false, "auth:token-99")
	mustMismatch(t, e, false, "auth:token-abc", "does not match")
	// Validate() catches broken patterns before a session starts, but the
	// matcher must still fail safely if handed a raw event.
	mustMismatch(t, textEvent("x", MatchRegex, "("), false, "x", "does not compile")
}

func TestJSONMatchIgnoresKeyOrderAndWhitespace(t *testing.T) {
	e := textEvent(`{"a":1,"b":[true,null]}`, MatchJSON, "")
	mustMatch(t, e, false, "{ \"b\": [true, null], \"a\": 1 }")
}

func TestJSONMatchFailsOnDifferentValuesAndNonJSONMessages(t *testing.T) {
	e := textEvent(`{"a":1}`, MatchJSON, "")
	mustMismatch(t, e, false, `{"a":2}`, "JSON documents differ")
	mustMismatch(t, e, false, "not json at all", "message is not valid JSON")
}

func TestSubsetMatchAllowsExtraKeysInTheMessage(t *testing.T) {
	e := textEvent(`{"op":"ack"}`, MatchSubset, "")
	mustMatch(t, e, false, `{"op":"ack","seq":42,"ts":"2026-07-10T09:15:00Z"}`)
}

func TestSubsetMatchRecursesIntoNestedObjectsAndReportsThePath(t *testing.T) {
	e := textEvent(`{"meta":{"user":"kai"}}`, MatchSubset, "")
	mustMatch(t, e, false, `{"meta":{"user":"kai","role":"admin"},"extra":1}`)
	mustMismatch(t, e, false, `{"meta":{"user":"aki"}}`, "$.meta.user")
	e2 := textEvent(`{"op":"ack","seq":1}`, MatchSubset, "")
	mustMismatch(t, e2, false, `{"op":"ack"}`, "$.seq (missing)")
}

func TestSubsetMatchComparesArraysElementWiseWithEqualLengths(t *testing.T) {
	e := textEvent(`{"ids":[1,2]}`, MatchSubset, "")
	mustMatch(t, e, false, `{"ids":[1,2],"more":true}`)
	mustMismatch(t, e, false, `{"ids":[1,2,3]}`, "array length: want 2, got 3")
	e2 := textEvent(`{"rows":[{"id":1}]}`, MatchSubset, "")
	mustMatch(t, e2, false, `{"rows":[{"id":1,"name":"x"}]}`)
	mustMismatch(t, e2, false, `{"rows":[{"id":2}]}`, "$.rows[0].id")
}

func TestAnyMatchAcceptsTextAndBinaryAlike(t *testing.T) {
	e := textEvent("whatever was recorded", MatchAny, "")
	mustMatch(t, e, false, "something else entirely")
	mustMatch(t, e, true, "\x00\x01binary")
}

func TestTypeMismatchBetweenTextAndBinaryIsReported(t *testing.T) {
	mustMismatch(t, textEvent("hi", "", ""), true, "hi", "type mismatch: want text, got binary")
	bin := &Event{Dir: DirC2S, Type: TypeBinary, DataB64: "aGk="}
	mustMismatch(t, bin, false, "hi", "type mismatch: want binary, got text")
}

func TestBinaryExactMatchComparesDecodedBytes(t *testing.T) {
	bin := &Event{Dir: DirC2S, Type: TypeBinary, DataB64: "AAECAw=="} // 00 01 02 03
	mustMatch(t, bin, true, "\x00\x01\x02\x03")
	mustMismatch(t, bin, true, "\x00\x01\x02", "want 4 bytes, got 3 bytes")
}

func TestTruncateCutsOnARuneBoundary(t *testing.T) {
	// Payloads are valid UTF-8, so a byte-offset cut must back up to the
	// start of the rune it lands in — otherwise transcripts and mismatch
	// reasons would show mojibake for non-ASCII payloads.
	s := strings.Repeat("a", 59) + "日本語" // the 3-byte 日 spans bytes 59–61
	got := Truncate(s, 60)
	if got != strings.Repeat("a", 59)+"…" {
		t.Fatalf("Truncate split a rune: %q", got)
	}
	if short := Truncate("短い", 60); short != "短い" {
		t.Fatalf("short strings must pass through unchanged, got %q", short)
	}
}

func TestMismatchReasonsTruncateHugePayloads(t *testing.T) {
	long := strings.Repeat("z", 5000)
	why := mustMismatch(t, textEvent("short", "", ""), false, long, "got")
	if len(why) > 400 {
		t.Fatalf("mismatch reason is %d bytes; it must stay log-friendly", len(why))
	}
	if !strings.Contains(why, "…") {
		t.Fatalf("truncated payload should end with an ellipsis: %q", why)
	}
}
