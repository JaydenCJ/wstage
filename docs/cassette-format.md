# The wstage cassette format

A cassette is a JSON Lines (`.jsonl`) file: the first non-blank line is a
header object, every following line is one session event in recorded
order. The format is line-oriented on purpose — the recorder flushes each
event as it happens, so an interrupted recording is still a parseable
cassette, and cassettes diff cleanly in code review.

Unknown JSON fields are ignored on read, so newer wstage versions can add
fields without breaking older readers. `wstage verify` performs all the
semantic checks described below.

## Header

```json
{"wstage":1,"name":"ticker","url":"ws://127.0.0.1:9601/feed","recorded_at":"2026-07-10T09:15:00Z"}
```

| Key | Required | Meaning |
|---|---|---|
| `wstage` | yes | format version; this build reads and writes `1` |
| `name` | no | free-form cassette name |
| `url` | no | the upstream URL the session was recorded against |
| `recorded_at` | no | RFC 3339 recording timestamp |

## Events

Every event carries `t` (seconds since session start, millisecond
precision, non-decreasing) and `dir`:

- `"c2s"` — a data message from the client to the server
- `"s2c"` — a data message from the server to the client
- `"close"` — the closing handshake; nothing may follow it

Data events (`c2s`/`s2c`) carry `type` (`"text"` or `"binary"`), the
payload (`data` for text, `data_b64` for binary), and optionally a match
rule. Close events carry `by` (`"client"` or `"server"`, default server),
`code` (1000–4999, or 0/absent for "no status code"), and `reason`.
A free-form `note` field is allowed anywhere and ignored by the engine.

## Match modes

When a session is replayed, incoming messages are asserted against the
recorded events for that direction (c2s expectations for `replay`, s2c for
`play`). `match` selects how, and `expect` optionally supplies the
assertion input when it differs from the recorded payload:

| Mode | Matches when | `expect` holds |
|---|---|---|
| `exact` (default) | payload is byte-identical | alternative literal |
| `prefix` | payload starts with the expectation | the prefix |
| `regex` | RE2 pattern matches the payload | the pattern |
| `json` | both parse to structurally equal JSON | expected document |
| `subset` | expectation is a structural subset (objects may have extra keys; arrays element-wise, equal length) | expected subset |
| `any` | always (consumes one message of either type) | — |

`regex`, `json`, and `subset` apply to text events only; binary events
support `exact` and `any`. Because `expect` is separate from `data`, a
cassette stays *playable*: `wstage play` always sends `data` verbatim, and
`subset`/`regex` expectations still hold for it — so a replay and a play
of the same cassette PASS against each other by construction.

## Semantics and deliberate limits

- **Events replay strictly in recorded order.** A live session may have
  raced; recording linearizes it at the message level. If your protocol is
  order-insensitive somewhere, loosen that expectation with `match`.
- **Message-level, not frame-level.** Fragmented messages are reassembled
  before recording; fragmentation boundaries are not preserved.
- **Ping/pong keepalives are answered locally, not recorded** — they are
  transport chatter, not conversation.
- **One session per cassette.** Each connection to `wstage replay` starts
  the script from the top.
- The close code `0` means the peer sent no status code; on replay it is
  sent as a normal `1000`.
