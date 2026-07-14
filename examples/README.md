# wstage examples

One hand-written cassette and two runnable scripts, all offline and
self-contained — the "upstream server" in both scripts is wstage itself.

## ticker.jsonl

A scripted market-data session that shows every expectation mode in one
file: a plain `exact` c2s expectation, a `subset` JSON expectation with a
separate `expect` document, and a `regex` expectation. Point any command
at it:

```bash
wstage show   examples/ticker.jsonl
wstage verify examples/ticker.jsonl
wstage replay examples/ticker.jsonl --listen 127.0.0.1:9601
```

## loopback.sh

The smallest closed loop: `replay` serves the cassette as a mock server,
`play` drives it as the scripted client, and both sides assert the same
recording — so both print `PASS`.

```bash
go build -o wstage ./cmd/wstage
bash examples/loopback.sh
```

## record-roundtrip.sh

The full workflow without any real backend: a replay acts as the upstream,
`record` proxies a live session to it and writes a fresh cassette, and the
recording is then verified, printed as a transcript, and replayed against
the original client script.

```bash
bash examples/record-roundtrip.sh
```

Both scripts pick fixed loopback ports (9601/9602 by default; pass others
as arguments) and never touch the network beyond 127.0.0.1.
