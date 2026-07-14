#!/usr/bin/env bash
# End-to-end smoke test for wstage: builds the binary, then closes the full
# cassette loop on loopback — verify → show → replay↔play → record through
# the proxy → replay the recording — asserting on real CLI output and exit
# codes. No network beyond 127.0.0.1, idempotent, finishes in seconds.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKDIR="$(mktemp -d)"
cleanup() {
  # shellcheck disable=SC2046
  kill $(cat "$WORKDIR"/*.pid 2>/dev/null) 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

fail() {
  echo "SMOKE FAIL: $*" >&2
  exit 1
}

BIN="$WORKDIR/wstage"
CASSETTE="$ROOT/examples/ticker.jsonl"

# start_server <logfile> <args...>: run a server command in the background
# and wait (up to ~5 s) for the CLI's own "listening on ws://HOST:PORT/"
# line. Sets ADDR to the bound address and SRV_PID to the process id —
# globals rather than echoes, so the server stays a waitable child of this
# shell.
start_server() {
  local log="$1"; shift
  "$BIN" "$@" >"${log}.out" 2>"$log" &
  SRV_PID=$!
  echo "$SRV_PID" > "${log}.pid"
  for _ in $(seq 1 100); do
    ADDR="$(grep -oE 'ws://127\.0\.0\.1:[0-9]+/' "$log" 2>/dev/null | head -1 || true)"
    [ -n "$ADDR" ] && return 0
    sleep 0.05
  done
  fail "server never reported a listen address ($(cat "$log" 2>/dev/null))"
}

echo "1. build"
(cd "$ROOT" && go build -o "$BIN" ./cmd/wstage) || fail "go build failed"

echo "2. version matches the manifest"
"$BIN" version | grep -qx "wstage 0.1.0" || fail "version mismatch"

echo "3. verify the example cassette"
"$BIN" verify "$CASSETTE" | grep -q "7 events (3 c2s, 3 s2c, close by server), 1.50s" \
  || fail "verify summary wrong"

echo "4. show renders the transcript with match rules"
OUT="$("$BIN" show "$CASSETTE")"
echo "$OUT" | grep -q "subscribe:AAPL" || fail "transcript missing c2s payload"
echo "$OUT" | grep -qF '[match: regex ^unsubscribe:[A-Z]+$]' || fail "transcript missing match rule"
echo "$OUT" | grep -q "1000 by server" || fail "transcript missing close"

echo "5. replay serves the cassette; play passes it as the scripted client"
start_server "$WORKDIR/replay.log" replay "$CASSETTE" --once --listen 127.0.0.1:0
"$BIN" play "$CASSETTE" "${ADDR}feed" | grep -q "3 sent, 3/3 expectations matched — PASS" \
  || fail "play did not PASS"
wait "$SRV_PID" || fail "replay --once should exit 0 on PASS"

echo "6. an off-script client is refused with exit 1 and quoted evidence"
sed 's/subscribe:AAPL/subscribe:DOGE/' "$CASSETTE" > "$WORKDIR/bad.jsonl"
start_server "$WORKDIR/replay2.log" replay "$CASSETTE" --once --listen 127.0.0.1:0
"$BIN" play "$WORKDIR/bad.jsonl" "${ADDR}feed" >/dev/null && fail "off-script play should fail"
if wait "$SRV_PID"; then fail "replay --once should exit 1 on mismatch"; fi
grep -q 'want "subscribe:AAPL", got "subscribe:DOGE"' "$WORKDIR/replay2.log.out" \
  || fail "mismatch evidence missing"

echo "7. record proxies a live session into a fresh cassette"
start_server "$WORKDIR/up.log" replay "$CASSETTE" --once --listen 127.0.0.1:0
UP_ADDR="$ADDR"
start_server "$WORKDIR/rec.log" record "${UP_ADDR}feed" --out "$WORKDIR/rerecorded.jsonl" --listen 127.0.0.1:0
"$BIN" play "$CASSETTE" "$ADDR" >/dev/null || fail "play through the recorder failed"
wait "$SRV_PID" || fail "record should exit 0"
grep -q "recorded 7 events (3 c2s, 3 s2c, close by server)" "$WORKDIR/rec.log.out" \
  || fail "record summary wrong"

echo "8. the recording verifies and replays cleanly"
"$BIN" verify "$WORKDIR/rerecorded.jsonl" >/dev/null || fail "recording does not verify"
start_server "$WORKDIR/replay3.log" replay "$WORKDIR/rerecorded.jsonl" --once --listen 127.0.0.1:0
"$BIN" play "$CASSETTE" "$ADDR" | grep -q "PASS" || fail "recording does not replay"
wait "$SRV_PID" || fail "replay of the recording should PASS"

echo "9. usage errors exit 2; broken cassettes exit 1"
set +e
"$BIN" replay --format yaml 2>/dev/null; [ $? -eq 2 ] || fail "unknown flag should exit 2"
echo '{"wstage":1}
{"t":0,"dir":"nope"}' > "$WORKDIR/broken.jsonl"
"$BIN" verify "$WORKDIR/broken.jsonl" 2>/dev/null; [ $? -eq 1 ] || fail "broken cassette should exit 1"
set -e

echo "SMOKE OK"
