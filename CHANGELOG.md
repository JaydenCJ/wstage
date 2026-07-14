# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2026-07-13

### Added

- Dependency-free RFC 6455 implementation: frame codec with extended
  lengths and minimal-encoding checks, masking-direction enforcement,
  fragmentation reassembly, UTF-8 validation, ping/pong handling, closing
  handshake with code/reason, and the HTTP/1.1 upgrade handshake for both
  the client and server sides.
- JSON Lines cassette format (`"wstage": 1` header + one event per line)
  with text and base64 binary payloads, millisecond timestamps, close
  attribution, and forward-compatible parsing; documented in
  docs/cassette-format.md.
- Scripted expectations per recorded message: `exact`, `prefix`, `regex`,
  `json` (structural equality), `subset` (extra keys allowed, JSON-path
  mismatch reports), and `any`, with an optional `expect` field separating
  the assertion from the recorded payload.
- `record` — a recording proxy that relays one live client session to an
  upstream ws:// URL while streaming every message and the close to a
  cassette, flushed line by line.
- `replay` — a scripted mock server for any cassette: strict mode closes
  off-script clients with 1008 and the failed expectation number, lenient
  mode records mismatches and keeps going; `--once` exits 0/1 with the
  session verdict, `--speed` optionally reproduces recorded pacing.
- `play` — the same engine as a scripted client, driving a live server
  from the cassette's client side; a replay and a play of one cassette
  always PASS against each other.
- `show` and `verify` — human-readable transcripts and full semantic
  validation (match modes, regex/JSON expectations, close codes, timeline
  monotonicity) with per-event error messages.
- Stable exit codes (0 ok, 1 expectation/verify failure, 2 usage,
  3 runtime), loopback-only defaults, and no telemetry.
- Runnable examples (`examples/ticker.jsonl`, `examples/loopback.sh`,
  `examples/record-roundtrip.sh`), 92 deterministic offline tests, and
  `scripts/smoke.sh`.

[0.1.0]: https://github.com/JaydenCJ/wstage/releases/tag/v0.1.0
