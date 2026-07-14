# Contributing to wstage

Issues, discussions and pull requests are all welcome.

## Getting started

You need Go ≥1.22; nothing else — the WebSocket protocol implementation is
part of this repository, so there are no dependencies to fetch.

```bash
git clone https://github.com/JaydenCJ/wstage && cd wstage
go build ./...
go test ./...
bash scripts/smoke.sh
```

`scripts/smoke.sh` builds the binary and closes the whole cassette loop on
loopback — verify → show → replay↔play → record through the proxy → replay
of the recording — asserting on real CLI output and exit codes; it must
finish by printing `SMOKE OK`.

## Before you open a pull request

1. `gofmt -l .` reports nothing (formatting is enforced).
2. `go vet ./...` passes with no findings.
3. `go test ./...` passes (92 deterministic tests, loopback only).
4. `bash scripts/smoke.sh` prints `SMOKE OK`.
5. Add tests for behavior changes; keep logic in pure, unit-testable
   modules (the frame codec, matcher, and cassette parser never open
   sockets — only the CLI layer does).

## Ground rules

- Keep dependencies at zero — including the protocol layer. Adding one
  needs strong justification in the PR.
- Servers bind 127.0.0.1 by default; wstage never dials anything the user
  did not name on the command line. No telemetry.
- The cassette format is a public contract: any field or match-mode change
  needs a docs/cassette-format.md update, a Validate() rule, and tests for
  both reading and writing.
- Determinism first: engines take injectable clocks and sleep functions,
  and tests must not depend on wall-clock timing.
- Code comments and doc comments are written in English.

## Reporting bugs

Include the output of `wstage version`, the full command line, the session
verdict lines from stdout/stderr, and — for replay mismatches — the
cassette event (or `wstage show` excerpt) plus the actual client message,
since that pair is exactly what the matcher saw.

## Security

Please do not open public issues for security problems; use GitHub's
private vulnerability reporting on this repository instead.
