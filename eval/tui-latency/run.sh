#!/usr/bin/env bash
# TUI latency harness: boots the driver TUI in a tmux pane against the mock
# provider (no API keys, no network), drives N turns with send-keys, and
# renders the per-hop latency table from the DRIVER_PERF_FILE trace.
#
# Usage: eval/tui-latency/run.sh [turns]   (default 3)
# Needs: tmux, python3, go. Runs entirely in a scratch fixture repo.
set -euo pipefail

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
TURNS="${1:-3}"
OUT="${TUI_LATENCY_OUT:-$(mktemp -d /tmp/tui-latency.XXXXXX)}"
SES="tuilat-$$"
PERF="$OUT/perf.jsonl"
BIN="$OUT/driver"

echo "out: $OUT" >&2

go build -o "$BIN" "$REPO/cmd/driver"

# Fixture: a minimal git repo with no Go markers, so -auto-verify derives no
# gate and the run is deterministic. README.md is what the mock's read_file hits.
FIX="$OUT/fixture"
mkdir -p "$FIX"
git -C "$FIX" init -q 2>/dev/null || true
echo "hello from the latency fixture" >"$FIX/README.md"
git -C "$FIX" add -A && git -C "$FIX" -c user.email=t@t -c user.name=t commit -qm fixture --allow-empty-message 2>/dev/null || true

cleanup() { tmux kill-session -t "$SES" 2>/dev/null || true; }
trap cleanup EXIT

# Boot the TUI: keyless (mock provider), state isolated to $OUT.
mkdir -p "$OUT/home"
tmux new-session -d -s "$SES" -x 140 -y 40 \
  "cd '$FIX' && env -u OPENROUTER_API_KEY -u ANTHROPIC_API_KEY -u CLAUDE_API_KEY \
     DRIVER_PERF_FILE='$PERF' HOME='$OUT/home' \
     '$BIN' -model 'mock:$REPO/eval/tui-latency/mockscript.json' \
       -ledger=false -memory=false -transcript-dir '$OUT/runs' \
       -trust trusted-local -approve off 2>'$OUT/stderr.log'; echo EXIT=\$? >>'$OUT/stderr.log'"

# Wait for boot: the input prompt appears.
for i in $(seq 1 100); do
  tmux has-session -t "$SES" 2>/dev/null || { echo "TUI exited during boot:" >&2; cat "$OUT/stderr.log" >&2; exit 1; }
  if tmux capture-pane -t "$SES" -p | grep -qE '>|›|❯'; then break; fi
  sleep 0.1
done
sleep 0.5

wait_mark() { # wait_mark <turn> <mark> <timeout_s>
  local deadline=$((SECONDS + $3))
  while ((SECONDS < deadline)); do
    if [[ -f "$PERF" ]] && grep -q "\"turn\":$1,\"mark\":\"$2\"" "$PERF" 2>/dev/null; then return 0; fi
    tmux has-session -t "$SES" 2>/dev/null || { echo "TUI died waiting for turn $1 $2" >&2; cat "$OUT/stderr.log" >&2; return 1; }
    sleep 0.05
  done
  echo "timeout waiting for turn $1 mark $2" >&2
  tmux capture-pane -t "$SES" -p | tail -20 >&2
  return 1
}

SENT_ARGS=()
for n in $(seq 1 "$TURNS"); do
  tmux send-keys -t "$SES" "please read the readme and summarize (turn $n)"
  SENT_NS=$(date +%s%N)
  tmux send-keys -t "$SES" Enter
  SENT_ARGS+=(-sent "$n=$SENT_NS")
  wait_mark "$n" ui_done_rendered 60
done

tmux capture-pane -t "$SES" -p >"$OUT/final-pane.txt"
tmux send-keys -t "$SES" C-c
sleep 0.3
cleanup

echo "--- final pane (tail) ---" >&2
tail -15 "$OUT/final-pane.txt" >&2
echo "--- latency report ($PERF) ---"
python3 "$REPO/eval/tui-latency/report.py" "${SENT_ARGS[@]}" "$PERF"
