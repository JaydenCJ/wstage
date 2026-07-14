#!/usr/bin/env bash
# Loopback demo: `wstage replay` serves the ticker cassette as a mock
# server while `wstage play` drives it as the scripted client. Both sides
# assert the same recording against each other, so both must PASS.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="${WSTAGE:-$ROOT/wstage}"
[ -x "$BIN" ] || { echo "build first: go build -o wstage ./cmd/wstage" >&2; exit 1; }

PORT="${1:-9601}"
CASSETTE="$ROOT/examples/ticker.jsonl"
LOG="$(mktemp)"
trap 'kill "$REPLAY_PID" 2>/dev/null || true; rm -f "$LOG"' EXIT

"$BIN" replay "$CASSETTE" --once --listen "127.0.0.1:$PORT" 2> >(tee "$LOG" >&2) &
REPLAY_PID=$!
for _ in $(seq 1 100); do
  grep -q 'listening on ws://' "$LOG" 2>/dev/null && break
  sleep 0.05
done

"$BIN" play "$CASSETTE" "ws://127.0.0.1:$PORT/feed"
wait "$REPLAY_PID"
echo "loopback: both sides PASS"
