package headless

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/AccursedGalaxy/driver-os/agent"
)

// The ndjson observer exposes the optional review-gate events (slice 1e): it
// must satisfy agent.ReviewObserver so the loop's type-assertion discovers it,
// and the emitted events must carry the typed payloads under the documented
// event types.
func TestNDJSONReviewEvents(t *testing.T) {
	var out bytes.Buffer
	var obs agent.Observer = ndjsonObserver{&out}
	ro, ok := obs.(agent.ReviewObserver)
	if !ok {
		t.Fatal("ndjsonObserver must implement agent.ReviewObserver")
	}
	ro.ReviewStart(1, "openai/gpt-5.5")
	ro.ReviewFinding(agent.ReviewFinding{File: "client.go", Severity: "blocker", Confidence: 9, FailureScenario: "leak"})
	ro.ReviewVerdict(1, 1, "Looks good.")

	var evs []map[string]any
	dec := json.NewDecoder(&out)
	for dec.More() {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		evs = append(evs, m)
	}
	if len(evs) != 3 || evs[0]["type"] != "review_start" || evs[1]["type"] != "review_finding" || evs[2]["type"] != "review_verdict" {
		t.Fatalf("events = %v", evs)
	}
	f := evs[1]["finding"].(map[string]any)
	if f["file"] != "client.go" || f["severity"] != "blocker" {
		t.Fatalf("finding payload = %v", f)
	}
	if evs[2]["blocking"].(float64) != 1 || evs[2]["round"].(float64) != 1 || evs[2]["summary"] != "Looks good." {
		t.Fatalf("verdict payload = %v", evs[2])
	}
}

// The D3 result object carries the review report (null when the gate is off).
func TestResultCarriesReview(t *testing.T) {
	res := baseResult(agent.Answered)
	res.Review = &agent.ReviewReport{Rounds: 1, Findings: []agent.ReviewedFinding{{
		ReviewFinding: agent.ReviewFinding{File: "a.go", Severity: "note"},
		Round:         1, Fate: agent.FateNote,
	}}}
	data, err := json.Marshal(buildResult(res, "m", "", "", nil))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	rev, ok := m["review"].(map[string]any)
	if !ok || rev["rounds"].(float64) != 1 {
		t.Fatalf("review in result = %v", m["review"])
	}
	// Gate off ⇒ the key is present and null (D3: every key always present).
	data, _ = json.Marshal(buildResult(baseResult(agent.Answered), "m", "", "", nil))
	m = map[string]any{}
	_ = json.Unmarshal(data, &m)
	if v, present := m["review"]; !present || v != nil {
		t.Fatalf("review must be present-and-null when the gate is off, got %v (present=%v)", v, present)
	}
}

// The D3 result object carries the plan-stage report the same way (null when
// the stage is off) — per-role cost stays attributable in the machine output.
func TestResultCarriesPlan(t *testing.T) {
	res := baseResult(agent.Answered)
	res.Plan = &agent.PlanReport{Model: "planner", Plan: "1. do the thing"}
	data, err := json.Marshal(buildResult(res, "m", "", "", nil))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	pl, ok := m["plan"].(map[string]any)
	if !ok || pl["model"] != "planner" || pl["plan"] != "1. do the thing" {
		t.Fatalf("plan in result = %v", m["plan"])
	}
	data, _ = json.Marshal(buildResult(baseResult(agent.Answered), "m", "", "", nil))
	m = map[string]any{}
	_ = json.Unmarshal(data, &m)
	if v, present := m["plan"]; !present || v != nil {
		t.Fatalf("plan must be present-and-null when the stage is off, got %v (present=%v)", v, present)
	}
}
