// Cassette format tests: parsing with line-number errors, write→load round
// trips, semantic validation, and the stats summary the CLI prints.
package cassette

import (
	"bytes"
	"strings"
	"testing"
)

const sampleCassette = `{"wstage":1,"name":"ticker","url":"ws://127.0.0.1:9601/feed","recorded_at":"2026-07-10T09:15:00Z"}
{"t":0,"dir":"c2s","type":"text","data":"subscribe:AAPL"}
{"t":0.04,"dir":"s2c","type":"text","data":"{\"px\":210.05}"}
{"t":0.5,"dir":"c2s","type":"binary","data_b64":"AAECAw=="}
{"t":1.5,"dir":"close","by":"server","code":1000,"reason":"done"}
`

func load(t *testing.T, text string) *Cassette {
	t.Helper()
	cas, err := Load(strings.NewReader(text))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cas
}

func TestLoadParsesHeaderAndEvents(t *testing.T) {
	cas := load(t, sampleCassette)
	if cas.Header.Name != "ticker" || cas.Header.URL != "ws://127.0.0.1:9601/feed" {
		t.Fatalf("header: %+v", cas.Header)
	}
	if len(cas.Events) != 4 {
		t.Fatalf("want 4 events, got %d", len(cas.Events))
	}
	if cas.Events[0].Data != "subscribe:AAPL" || cas.Events[3].Reason != "done" {
		t.Fatalf("events parsed wrong: %+v", cas.Events)
	}
}

func TestLoadRejectsMissingOrForeignHeaders(t *testing.T) {
	// Empty input has no header at all.
	if _, err := Load(strings.NewReader("")); err == nil || !strings.Contains(err.Error(), "empty cassette") {
		t.Fatalf("got %v", err)
	}
	// An event on line 1 is not a header.
	_, err := Load(strings.NewReader(`{"t":0,"dir":"c2s","type":"text","data":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "wstage") {
		t.Fatalf("an event as line 1 should be rejected as a non-header, got %v", err)
	}
	// A future format version must be refused, not half-parsed.
	_, err = Load(strings.NewReader(`{"wstage":99}`))
	if err == nil || !strings.Contains(err.Error(), "version 99") {
		t.Fatalf("got %v", err)
	}
}

func TestLoadReportsLineNumberOnBadJSON(t *testing.T) {
	text := `{"wstage":1}
{"t":0,"dir":"c2s","type":"text","data":"ok"}
{not json}
`
	_, err := Load(strings.NewReader(text))
	if err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("want a line-3 error, got %v", err)
	}
}

func TestLoadSkipsBlankLinesAndToleratesUnknownFields(t *testing.T) {
	// Blank lines and future fields must both be non-fatal, so newer
	// wstage versions can extend the format without breaking older readers.
	text := "\n{\"wstage\":1,\"future_field\":true}\n\n{\"t\":0,\"dir\":\"s2c\",\"type\":\"text\",\"data\":\"a\",\"future_hint\":\"yes\"}\n\n"
	cas := load(t, text)
	if len(cas.Events) != 1 || cas.Events[0].Data != "a" {
		t.Fatalf("got %+v", cas.Events)
	}
}

func TestWriterThenLoadRoundTripsEverything(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.WriteHeader(Header{Name: "rt", URL: "ws://example.test/x"}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	events := []Event{
		{T: 0, Dir: DirC2S, Type: TypeText, Data: "hello", Match: MatchPrefix, Expect: "hel"},
		{T: 0.25, Dir: DirS2C, Type: TypeBinary, DataB64: "AP8Q"},
		{T: 1, Dir: DirClose, By: "client", Code: 1001, Reason: "bye"},
	}
	for _, ev := range events {
		if err := w.WriteEvent(ev); err != nil {
			t.Fatalf("WriteEvent: %v", err)
		}
	}

	cas := load(t, buf.String())
	if cas.Header.Wstage != FormatVersion || cas.Header.Name != "rt" {
		t.Fatalf("header: %+v", cas.Header)
	}
	if len(cas.Events) != 3 {
		t.Fatalf("want 3 events, got %d", len(cas.Events))
	}
	for i := range events {
		if cas.Events[i] != events[i] {
			t.Fatalf("event %d changed in round trip:\n want %+v\n  got %+v", i, events[i], cas.Events[i])
		}
	}
}

func TestPayloadDecodesBase64AndRejectsGarbage(t *testing.T) {
	ev := Event{Type: TypeBinary, DataB64: "AAECAw=="}
	got, err := ev.Payload()
	if err != nil {
		t.Fatalf("Payload: %v", err)
	}
	if !bytes.Equal(got, []byte{0, 1, 2, 3}) {
		t.Fatalf("got %v", got)
	}
	ev.DataB64 = "!!!not-base64!!!"
	if _, err := ev.Payload(); err == nil {
		t.Fatal("want a base64 error")
	}
}

func TestExpectationFallsBackToData(t *testing.T) {
	ev := Event{Type: TypeText, Data: "the payload"}
	if ev.Expectation() != "the payload" {
		t.Fatalf("got %q", ev.Expectation())
	}
	ev.Expect = "^the"
	if ev.Expectation() != "^the" {
		t.Fatalf("got %q", ev.Expectation())
	}
}

func TestValidateAcceptsAWellFormedCassette(t *testing.T) {
	cas := load(t, sampleCassette)
	if errs := cas.Validate(); len(errs) != 0 {
		t.Fatalf("unexpected problems: %v", errs)
	}
}

func validateOne(t *testing.T, ev Event, wantSubstr string) {
	t.Helper()
	cas := &Cassette{Header: Header{Wstage: 1}, Events: []Event{ev}}
	errs := cas.Validate()
	if len(errs) == 0 {
		t.Fatalf("want a validation error mentioning %q", wantSubstr)
	}
	if !strings.Contains(errs[0], wantSubstr) {
		t.Fatalf("error %q does not mention %q", errs[0], wantSubstr)
	}
}

func TestValidateRejectsUnknownDirsAndTypes(t *testing.T) {
	validateOne(t, Event{Dir: "sideways", Type: TypeText}, `unknown dir "sideways"`)
	validateOne(t, Event{Dir: DirC2S, Type: "emoji"}, `unknown type "emoji"`)
}

func TestValidateRejectsMismatchedPayloadEncodings(t *testing.T) {
	validateOne(t, Event{Dir: DirC2S, Type: TypeText, DataB64: "AAA="}, `not "data_b64"`)
	validateOne(t, Event{Dir: DirS2C, Type: TypeBinary, DataB64: "%%%"}, "not valid base64")
}

func TestValidateRejectsBadMatchRules(t *testing.T) {
	// Every broken expectation must be caught before a session starts —
	// discovering it mid-replay would waste the run.
	validateOne(t, Event{Dir: DirC2S, Type: TypeText, Data: "x", Match: "fuzzy"}, `unknown match mode "fuzzy"`)
	validateOne(t, Event{Dir: DirC2S, Type: TypeBinary, DataB64: "AAA=", Match: MatchRegex}, "binary events support only")
	validateOne(t, Event{Dir: DirC2S, Type: TypeText, Data: "x", Match: MatchRegex, Expect: "("}, "not a valid regex")
	validateOne(t, Event{Dir: DirC2S, Type: TypeText, Data: "{no}", Match: MatchSubset}, "not valid JSON")
}

func TestValidateRejectsMalformedCloseEvents(t *testing.T) {
	validateOne(t, Event{Dir: DirClose, Data: "extra"}, "carry no payload")
	validateOne(t, Event{Dir: DirClose, By: "moderator"}, `unknown close initiator "moderator"`)
	validateOne(t, Event{Dir: DirClose, Code: 999}, "outside the valid range")
}

func TestValidateRejectsBrokenTimelines(t *testing.T) {
	validateOne(t, Event{T: -0.5, Dir: DirC2S, Type: TypeText, Data: "x"}, "negative timestamp")
	cas := &Cassette{Header: Header{Wstage: 1}, Events: []Event{
		{T: 1, Dir: DirC2S, Type: TypeText, Data: "a"},
		{T: 0.5, Dir: DirS2C, Type: TypeText, Data: "b"},
	}}
	errs := cas.Validate()
	if len(errs) != 1 || !strings.Contains(errs[0], "goes backwards") {
		t.Fatalf("got %v", errs)
	}
}

func TestValidateRejectsEventsAfterClose(t *testing.T) {
	cas := &Cassette{Header: Header{Wstage: 1}, Events: []Event{
		{T: 0, Dir: DirClose},
		{T: 1, Dir: DirS2C, Type: TypeText, Data: "ghost"},
	}}
	errs := cas.Validate()
	if len(errs) != 1 || !strings.Contains(errs[0], "unreachable") {
		t.Fatalf("got %v", errs)
	}
}

func TestStatsSummarizesCountsAndClose(t *testing.T) {
	cas := load(t, sampleCassette)
	st := cas.Stats()
	if st.Events != 4 || st.C2S != 2 || st.S2C != 1 {
		t.Fatalf("counts: %+v", st)
	}
	if !st.HasClose || st.CloseBy != "server" || st.CloseCode != 1000 {
		t.Fatalf("close: %+v", st)
	}
	if st.Duration != 1.5 {
		t.Fatalf("duration: %v", st.Duration)
	}
	// An unattributed close defaults to the server as initiator.
	bare := &Cassette{Events: []Event{{T: 0, Dir: DirClose}}}
	if by := bare.Stats().CloseBy; by != "server" {
		t.Fatalf("default initiator: %q", by)
	}
}

func TestStatsSummaryStringIsStable(t *testing.T) {
	// The summary line appears in CLI output and the smoke test greps it.
	st := load(t, sampleCassette).Stats()
	want := "4 events (2 c2s, 1 s2c, close by server), 1.50s"
	if got := st.Summary(); got != want {
		t.Fatalf("Summary() = %q, want %q", got, want)
	}
}

func TestSummaryUsesTheSingularForASingleEvent(t *testing.T) {
	// "1 events" in user-facing output would be an embarrassment; the count
	// word must agree with the number.
	one := &Cassette{Events: []Event{{T: 0, Dir: DirClose, By: "server", Code: 1000}}}
	want := "1 event (0 c2s, 0 s2c, close by server), 0.00s"
	if got := one.Stats().Summary(); got != want {
		t.Fatalf("Summary() = %q, want %q", got, want)
	}
}
