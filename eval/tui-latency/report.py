#!/usr/bin/env python3
"""Parse a DRIVER_PERF_FILE JSONL trace into a per-turn latency table.

Each record: {"turn": N, "mark": str, "at_ms": float, "wall_ns": int, "attrs": {...}}
The harness (run.sh) also passes -sent <turn>=<wall_ns> pairs recording when
tmux send-keys fired, so the terminal-input → ui_submit hop is measurable too.

Intervals reported (ms), per turn and aggregated (median / max over turns):

  input_to_submit      tmux send-keys wall clock -> ui_submit (terminal + tea dispatch)
  submit_to_goroutine  ui_submit -> host_turn_goroutine (StartTurn dispatch)
  setup                host_turn_goroutine -> setup_done (worktree + gates + env/recall + seed)
    gates              gates_start -> gates_done (verify-baseline measurement)
  model_wait_first     model_start(iter=1) -> first_delta_received (provider TTFT; mock floor = pre_delay_ms)
  delta_to_paint       first_delta_received -> ui_first_delta_rendered (bridge send + Update + render)
  first_feedback       ui_submit -> ui_first_delta_rendered (what the human feels as responsiveness)
  first_tool           ui_submit -> ui_first_tool (when a tool row first appears)
  turn_tail            host_run_done -> ui_done_rendered (loop return -> final paint)
  done_send_to_paint   host_done_sent -> ui_done_rendered (tea delivery + finishTurn + refresh)
  persist_after_done   host_done_sent -> host_persisted (post-paint disk I/O, off the hot path)
  total                ui_submit -> ui_done_rendered
"""

import json
import statistics
import sys


def load(path):
    turns = {}
    with open(path) as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            r = json.loads(line)
            t = turns.setdefault(r["turn"], {})
            key = r["mark"]
            it = (r.get("attrs") or {}).get("iter")
            if it is not None:
                key = f"{key}#{it}"
            # keep the FIRST occurrence of each mark per turn
            t.setdefault(key, r)
    return turns


def wall_ms(t, a, b):
    if a not in t or b not in t:
        return None
    return (t[b]["wall_ns"] - t[a]["wall_ns"]) / 1e6


INTERVALS = [
    ("input_to_submit", "__sent__", "ui_submit"),
    ("submit_to_goroutine", "ui_submit", "host_turn_goroutine"),
    ("setup", "host_turn_goroutine", "setup_done"),
    ("gates", "gates_start", "gates_done"),
    ("model_wait_first", "model_start#1", "first_delta_received"),
    ("delta_to_paint", "first_delta_received", "ui_first_delta_rendered"),
    ("first_feedback", "ui_submit", "ui_first_delta_rendered"),
    ("first_tool", "ui_submit", "ui_first_tool"),
    ("turn_tail", "host_run_done", "ui_done_rendered"),
    ("done_send_to_paint", "host_done_sent", "ui_done_rendered"),
    ("persist_after_done", "host_done_sent", "host_persisted"),
    ("total", "ui_submit", "ui_done_rendered"),
]


def main():
    args = sys.argv[1:]
    sent = {}
    while "-sent" in args:
        i = args.index("-sent")
        k, v = args[i + 1].split("=")
        sent[int(k)] = int(v)
        del args[i : i + 2]
    if len(args) != 1:
        print("usage: report.py [-sent turn=wall_ns ...] perf.jsonl", file=sys.stderr)
        sys.exit(2)
    turns = load(args[0])
    if not turns:
        print("no perf records found", file=sys.stderr)
        sys.exit(1)

    rows = {}
    for tid in sorted(turns):
        t = dict(turns[tid])
        if tid in sent:
            t["__sent__"] = {"wall_ns": sent[tid]}
        for name, a, b in INTERVALS:
            rows.setdefault(name, {})[tid] = wall_ms(t, a, b)

    tids = sorted(turns)
    hdr = ["interval"] + [f"turn{t}" for t in tids] + ["median", "max"]
    widths = [max(len(h), 19) for h in hdr]
    print("  ".join(h.ljust(w) for h, w in zip(hdr, widths)))
    exit_missing = False
    for name, _, _ in INTERVALS:
        vals = [rows[name][t] for t in tids]
        present = [v for v in vals if v is not None]
        cells = [f"{v:.1f}" if v is not None else "-" for v in vals]
        med = f"{statistics.median(present):.1f}" if present else "-"
        mx = f"{max(present):.1f}" if present else "-"
        line = [name] + cells + [med, mx]
        print("  ".join(c.ljust(w) for c, w in zip(line, widths)))
        if not present and name not in ("input_to_submit", "gates", "first_tool"):
            exit_missing = True
    if exit_missing:
        print(
            "\nERROR: core intervals missing marks — instrumentation incomplete",
            file=sys.stderr,
        )
        sys.exit(1)


if __name__ == "__main__":
    main()
