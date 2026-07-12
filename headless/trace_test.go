package headless

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func fixedNow() time.Time { return time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC) }

func TestTraceWriterCompact(t *testing.T) {
	var out bytes.Buffer
	tw := &traceWriter{dst: &out, compact: true, now: fixedNow}

	lines := []string{
		"protocol: tools",
		"",
		"========== iteration 3/40 ==========",
		"model: let me look at the file",
		"observation: (file contents)",
		"tokens: 1500000 cumulative after turn 3 (prompt 1400000, completion 100000)",
		"verify: FAIL — go test ./... exited 1",
		"review gate: ON — model=openai/gpt-5.5 rounds=2",
		"worktree: /tmp/driver-agent-wt-x (baseline abc = HEAD; main checkout was clean)",
		"WARN something odd",
	}
	tw.Write([]byte(strings.Join(lines, "\n") + "\n"))

	got := out.String()
	for _, want := range []string{
		"[12:00:00] iter 3/40 · 1.50M tok",
		// humanTokens scales: 1.5e6 → M; smaller counts get k / raw (pinned below)
		"[12:00:00] verify: FAIL",
		"[12:00:00] review gate: ON",
		"[12:00:00] worktree: /tmp/driver-agent-wt-x",
		"[12:00:00] WARN something odd",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("compact output missing %q\ngot:\n%s", want, got)
		}
	}
	for _, drop := range []string{"model: let me look", "observation:", "protocol:"} {
		if strings.Contains(got, drop) {
			t.Errorf("compact output should drop %q\ngot:\n%s", drop, got)
		}
	}
}

func TestTraceWriterFullPassesThroughAndTees(t *testing.T) {
	var out bytes.Buffer
	tw := &traceWriter{dst: &out, compact: false, now: fixedNow}
	tw.Write([]byte("model: hello\npartial"))
	if got := out.String(); got != "model: hello\n" {
		t.Fatalf("full mode should pass complete lines through verbatim, got %q", got)
	}
	tw.Close() // flushes the partial line
	if got := out.String(); !strings.HasSuffix(got, "partial\n") {
		t.Fatalf("Close should flush the partial trailing line, got %q", got)
	}
}

func TestTraceWriterSplitWrites(t *testing.T) {
	var out bytes.Buffer
	tw := &traceWriter{dst: &out, compact: true, now: fixedNow}
	// A milestone line delivered across two Write calls must still match.
	tw.Write([]byte("verify: PA"))
	tw.Write([]byte("SS\n"))
	if !strings.Contains(out.String(), "verify: PASS") {
		t.Fatalf("split-write milestone lost: %q", out.String())
	}
}

func TestWorktreeValueResolve(t *testing.T) {
	cases := []struct {
		set, val       bool
		inRepo, review bool
		want           bool
	}{
		{false, false, true, false, true},   // auto + repo → on
		{false, false, true, true, false},   // auto + -review → off (live tree needed)
		{false, false, false, false, false}, // auto + no repo → off
		{true, true, false, false, true},    // forced on wins even outside a repo (setup will fail loudly)
		{true, false, true, false, false},   // forced off wins in a repo
	}
	for i, c := range cases {
		w := worktreeValue{set: c.set, val: c.val}
		if got := w.resolve(c.inRepo, c.review); got != c.want {
			t.Errorf("case %d: resolve(%v,%v) with set=%v val=%v = %v, want %v", i, c.inRepo, c.review, c.set, c.val, got, c.want)
		}
	}
	var w worktreeValue
	if err := w.Set("auto"); err != nil || w.auto() != true {
		t.Errorf("Set(auto) should stay auto, err=%v auto=%v", err, w.auto())
	}
	if err := w.Set("nonsense"); err == nil {
		t.Error("Set(nonsense) should error")
	}
}

func TestHumanTokens(t *testing.T) {
	for n, want := range map[int]string{500: "500", 4200: "4.2k", 1_500_000: "1.50M"} {
		if got := humanTokens(n); got != want {
			t.Errorf("humanTokens(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestResolveEffort(t *testing.T) {
	for in, want := range map[string]string{
		"low": "low", "minimal": "minimal", "xhigh": "xhigh",
		"default": "", "provider": "", "": "",
	} {
		got, err := resolveEffort(in)
		if err != nil || got != want {
			t.Errorf("resolveEffort(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := resolveEffort("max"); err == nil {
		t.Error("resolveEffort(max) should error")
	}
}
