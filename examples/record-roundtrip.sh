#!/usr/bin/env bash
# Record round trip, fully offline: a replay of ticker.jsonl acts as the
# "upstream backend", `wstage record` proxies a session to it while writing
# a fresh cassette, and the recording is then verified, shown, and replayed
# against the original client script.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="${WSTAGE:-$ROOT/wstage}"
[ -x "$BIN" ] || { echo "build first: go build -o wstage ./cmd/wstage" >&2; exit 1; }

UP_PORT="${1:-9601}"
REC_PORT="${2:-9602}"
CASSETTE="$ROOT/examples/ticker.jsonl"
TMP="$(mktemp -d)"
trap 'kill $(cat "$TMP"/*.pid 2>/dev/null) 2>/dev/null || true; rm -rf "$TMP"' EXIT
OUT="$TMP/rerecorded.jsonl"

# wait_ready <logfile>: block until the server logs its listen address.
wait_ready() {
  for _ in $(seq 1 100); do
    grep -q 'listening on ws://' "$1" 2>/dev/null && return 0
    grep -q 'recording ws://' "$1" 2>/dev/null && return 0
    sleep 0.05
  done
  echo "server never came up: $(cat "$1")" >&2; exit 1
}

echo "1. start the fake upstream (a replay of the original cassette)"
"$BIN" replay "$CASSETTE" --once --listen "127.0.0.1:$UP_PORT" 2>"$TMP/up.log" &
echo $! > "$TMP/up.pid"
wait_ready "$TMP/up.log"

echo "2. record a session through the proxy"
"$BIN" record "ws://127.0.0.1:$UP_PORT/feed" --out "$OUT" --listen "127.0.0.1:$REC_PORT" 2>"$TMP/rec.log" &
echo $! > "$TMP/rec.pid"
wait_ready "$TMP/rec.log"
"$BIN" play "$CASSETTE" "ws://127.0.0.1:$REC_PORT/"
wait "$(cat "$TMP/up.pid")"
wait "$(cat "$TMP/rec.pid")"

echo "3. the recording verifies, shows, and replays cleanly"
"$BIN" verify "$OUT"
"$BIN" show "$OUT"
"$BIN" replay "$OUT" --once --listen "127.0.0.1:$UP_PORT" 2>"$TMP/up2.log" &
echo $! > "$TMP/up2.pid"
wait_ready "$TMP/up2.log"
"$BIN" play "$CASSETTE" "ws://127.0.0.1:$UP_PORT/"
wait "$(cat "$TMP/up2.pid")"
echo "record round trip: PASS"
