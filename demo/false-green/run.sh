#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
TMP=$(mktemp -d "${TMPDIR:-/tmp}/driver-os-wow.XXXXXX")
trap 'rm -rf "$TMP"' EXIT HUP INT TERM
BIN="$TMP/runner"
SCRIPT="$ROOT/demo/false-green/lying-agent.json"

printf '\nBuilding the runner...\n'
(cd "$ROOT" && go build -o "$BIN" ./cmd/runner)

new_workspace() {
  WORK="$TMP/$1"
  mkdir "$WORK"
  printf '42\n' >"$WORK/answer.txt"
  git -C "$WORK" init -q
  git -C "$WORK" config user.email demo@driver-os.local
  git -C "$WORK" config user.name driver-os-demo
  git -C "$WORK" add answer.txt
  git -C "$WORK" commit -qm baseline
}

run_agent() {
  (cd "$WORK" && "$BIN" \
    -trust trusted-local \
    -provider mock -model "$SCRIPT" \
    -task "Fix answer.txt. The required answer is 42." \
    -worktree=false -max-iters=3 -finish-nudge=0 \
    -transcript-dir "$TMP/transcripts" -ledger=false \
    "$@")
}

printf '\n============================================================\n'
printf '  1/2  SELF-REPORTED SUCCESS (no external oracle)\n'
printf '============================================================\n'
new_workspace no-oracle
run_agent -trace=compact || true
printf '\nThe agent wrote: '; tr -d '\n' <"$WORK/answer.txt"
printf '  (wrong, but the harness had no oracle)\n'

printf '\n============================================================\n'
printf '  2/2  THE SAME AGENT, WITH ONE EXTERNAL GATE\n'
printf '============================================================\n'
new_workspace gated
set +e
run_agent -trace=compact \
  -verify-cmd 'test "$(cat answer.txt)" = 42' \
  -verify-continue=false
STATUS=$?
set -e
printf '\nThe agent wrote: '; tr -d '\n' <"$WORK/answer.txt"
printf '  (still wrong)\n'
printf 'driver-os exit code: %s (2 means unverified)\n' "$STATUS"

if [ "$STATUS" -ne 2 ]; then
  printf 'demo failed: expected the external gate to produce exit 2\n' >&2
  exit 1
fi

printf '\nVerifying the proof bundles without executing their contents...\n'
for BUNDLE in "$TMP"/transcripts/*.bundle; do
  "$BIN" bundle verify "$BUNDLE"
done
printf '\nThe model did not become more honest. The harness stopped believing it.\n'
printf 'Both proof bundles verified offline. Temporary demo files are now removed.\n\n'
