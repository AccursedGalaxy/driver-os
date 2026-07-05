package agent

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

func TestRunStampsIDAndTiming(t *testing.T) {
	// Every finished run is addressable by a stable, time-sortable ID and carries
	// wall-clock bounds — the P1 spine. (Uses the text loop via runScript.)
	res, _ := runScript(t, map[string]string{"go.mod": "module x\n"},
		[]string{"list_dir .", "answer done"})
	if res.ID == "" {
		t.Fatal("RunResult.ID is empty — the run was not stamped")
	}
	if !strings.Contains(res.ID, "-") || len(res.ID) < len("20060102-150405-aabbccdd") {
		t.Errorf("ID %q is not the expected <date-time>-<hex> shape", res.ID)
	}
	if res.StartedAt.IsZero() || res.EndedAt.IsZero() {
		t.Errorf("timing not stamped: started=%v ended=%v", res.StartedAt, res.EndedAt)
	}
	if res.EndedAt.Before(res.StartedAt) {
		t.Errorf("EndedAt %v is before StartedAt %v", res.EndedAt, res.StartedAt)
	}
}

func TestRunIDsAreUnique(t *testing.T) {
	a, _ := runScript(t, nil, []string{"answer x"})
	b, _ := runScript(t, nil, []string{"answer x"})
	if a.ID == b.ID {
		t.Fatalf("two runs got the same ID %q — the hex suffix is not doing its job", a.ID)
	}
}

func TestWriteTranscriptAndIndex(t *testing.T) {
	dir := t.TempDir()
	res, _ := runScript(t, map[string]string{"go.mod": "module x\n"},
		[]string{"list_dir .", "answer done"})

	rec := RecordFrom(res, "test/model-1")
	path, err := WriteTranscript(dir, rec)
	if err != nil {
		t.Fatalf("WriteTranscript: %v", err)
	}

	// The per-run file is named by ID and round-trips the full record + trace.
	if filepath.Base(path) != res.ID+".json" {
		t.Errorf("transcript path = %q, want <id>.json", path)
	}
	var got RunRecord
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("transcript is not valid JSON: %v", err)
	}
	if got.ID != res.ID || got.Model != "test/model-1" || got.SchemaVersion != TranscriptSchemaVersion {
		t.Errorf("header mismatch: %+v", got)
	}
	if len(got.Steps) != len(res.Steps) {
		t.Errorf("steps not preserved: got %d, want %d", len(got.Steps), len(res.Steps))
	}
	if len(got.Messages) != len(res.Messages) {
		t.Errorf("messages not preserved: got %d, want %d", len(got.Messages), len(res.Messages))
	}
	if len(got.Messages) > 0 && got.Messages[len(got.Messages)-1].Text() != res.Messages[len(res.Messages)-1].Text() {
		t.Errorf("last message = %q, want %q", got.Messages[len(got.Messages)-1].Text(), res.Messages[len(res.Messages)-1].Text())
	}

	// The index got exactly one compact (trace-less) line.
	idxRaw, err := os.ReadFile(filepath.Join(dir, "runs.jsonl"))
	if err != nil {
		t.Fatalf("index not written: %v", err)
	}
	var entry runIndexEntry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(idxRaw))), &entry); err != nil {
		t.Fatalf("index line not valid JSON: %v", err)
	}
	if entry.ID != res.ID || entry.Outcome != res.Outcome {
		t.Errorf("index entry mismatch: %+v", entry)
	}
	if strings.Contains(string(idxRaw), `"steps"`) {
		t.Error("index line should be trace-less but contains a steps field")
	}
}

func TestWriteTranscriptAppendsIndex(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		res, _ := runScript(t, nil, []string{"answer x"})
		if _, err := WriteTranscript(dir, RecordFrom(res, "m")); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.Open(filepath.Join(dir, "runs.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("index has %d lines, want 3 (append, not overwrite)", n)
	}
}

func TestWriteTranscriptRejectsNoID(t *testing.T) {
	if _, err := WriteTranscript(t.TempDir(), RunRecord{}); err == nil {
		t.Fatal("WriteTranscript should error on a record with no ID")
	}
}

func TestRecordFromNilAndTaskPreview(t *testing.T) {
	if got := RecordFrom(nil, "m"); got.SchemaVersion != TranscriptSchemaVersion || got.ID != "" {
		t.Errorf("RecordFrom(nil) = %+v, want versioned empty record", got)
	}
	long := "first line " + strings.Repeat("x", 300) + "\nsecond line"
	p := taskPreview(long)
	if strings.Contains(p, "\n") || strings.Contains(p, "second line") {
		t.Errorf("preview leaked past the first line: %q", p)
	}
	if len([]rune(p)) > 120 {
		t.Errorf("preview not clipped: %d runes", len([]rune(p)))
	}
}

func TestLoadTranscriptRoundTripsMessagesAndSteps(t *testing.T) {
	dir := t.TempDir()
	rec := RunRecord{
		SchemaVersion: TranscriptSchemaVersion,
		ID:            "20260705-010203-abcdef12",
		Task:          "resume me",
		Outcome:       Answered,
		Answer:        "ok",
		Steps:         []Step{{Iter: 1, Reply: "answer ok", Verb: "answer"}},
		Messages:      []llm.Message{llm.User("TASK: resume me"), llm.Assistant("ok")},
	}
	if _, err := WriteTranscript(dir, rec); err != nil {
		t.Fatalf("WriteTranscript: %v", err)
	}
	got, err := LoadTranscript(dir, rec.ID)
	if err != nil {
		t.Fatalf("LoadTranscript: %v", err)
	}
	if len(got.Messages) != 2 || got.Messages[0].Text() != "TASK: resume me" || got.Messages[1].Text() != "ok" {
		t.Fatalf("messages = %#v, want user+assistant text round-trip", got.Messages)
	}
	if len(got.Steps) != 1 || got.Steps[0].Reply != "answer ok" {
		t.Fatalf("steps = %#v, want one preserved step", got.Steps)
	}
}
