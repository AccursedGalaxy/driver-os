package agent

import (
	"context"
	"reflect"
	"testing"
)

func TestTerminationPolicyRecordRoundTrip(t *testing.T) {
	policy := DefaultTerminationPolicy()
	policy.Version = "tp-test"
	policy.NavSpiralWindow = 9
	counters := DetectorCounters{SpiralCycle: 1, GreenRepeatNudge: 2, TerminatedBy: "spiral_cycle", TerminatedAtIter: 6}
	res := &RunResult{
		ID:                "20260705-010203-abcdef12",
		Task:              "record policy",
		Outcome:           KilledSpiral,
		TerminationPolicy: policy,
		DetectorCounters:  counters,
	}

	dir := t.TempDir()
	if _, err := WriteTranscript(dir, RecordFrom(res, "test/model")); err != nil {
		t.Fatalf("WriteTranscript: %v", err)
	}
	got, err := LoadTranscript(dir, res.ID)
	if err != nil {
		t.Fatalf("LoadTranscript: %v", err)
	}
	if !reflect.DeepEqual(got.TerminationPolicy, policy) {
		t.Errorf("TerminationPolicy = %+v, want %+v", got.TerminationPolicy, policy)
	}
	if !reflect.DeepEqual(got.DetectorCounters, counters) {
		t.Errorf("DetectorCounters = %+v, want %+v", got.DetectorCounters, counters)
	}
}

func TestSpiralKillRecordsDetectorCounters(t *testing.T) {
	sp := &scripted{replies: []string{
		"list_dir a", "list_dir b", "list_dir a", "list_dir b", "list_dir a", "list_dir b",
	}}
	res, err := runT(context.Background(), Config{
		Model: sp, Sandbox: sbWith(t, map[string]string{"a/x": "1", "b/x": "1"}),
		Task: "t", MaxIterations: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != KilledSpiral {
		t.Fatalf("Outcome = %q (%s), want KilledSpiral", res.Outcome, res.Reason)
	}
	if res.DetectorCounters.TerminatedBy != "spiral_cycle" || res.DetectorCounters.SpiralCycle == 0 {
		t.Fatalf("spiral counters = %+v, want spiral_cycle with a nonzero cycle count", res.DetectorCounters)
	}
}

func TestDefaultTerminationPolicyHistoricalValues(t *testing.T) {
	got := DefaultTerminationPolicy()
	want := TerminationPolicy{
		Version: "tp-2026-07-10", MaxRepeats: 2, MaxReasoningRepeats: 6,
		MaxStagnant: 3, NavSpiralWindow: 4, WanderMultiple: 4,
		FrontierCap: 64, GreenRepeatThreshold: 3,
	}
	if got != want {
		t.Errorf("DefaultTerminationPolicy() = %+v, want %+v", got, want)
	}
}
