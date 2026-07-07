package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/eval/suite/gobench"
)

func TestWriteRejections(t *testing.T) {
	tmp := t.TempDir()
	rejections := []*gobench.Rejection{
		{InstanceID: "inst1", Reason: "broken-base"},
		{InstanceID: "inst2", Reason: "flaky"},
	}
	if err := writeRejections(tmp, rejections); err != nil {
		t.Fatalf("writeRejections failed: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(tmp, "validation-rejections.jsonl"))
	if err != nil {
		t.Fatalf("read file failed: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) != 2 {
		t.Errorf("expected 2 lines, got %d", len(lines))
	}

	var r gobench.Rejection
	if err := json.Unmarshal([]byte(lines[0]), &r); err != nil {
		t.Fatalf("unmarshal line 0 failed: %v", err)
	}
	if r.InstanceID != "inst1" {
		t.Errorf("expected inst1, got %s", r.InstanceID)
	}
}

func TestPrintSummary(t *testing.T) {
	rejections := []*gobench.Rejection{
		{Reason: "broken-base"},
		{Reason: "flaky"},
		{Reason: "flaky"},
	}

	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	printSummary(10, 7, 2, rejections)

	w.Close()
	var buf bytes.Buffer
	buf.ReadFrom(r)
	os.Stdout = oldStdout

	output := buf.String()
	expectedParts := []string{
		"Candidates", "10",
		"Accepted", "7",
		"Demoted", "2",
		"Rejected", "3",
		"- broken-base", "1",
		"- flaky", "2",
		"- gold-red", "0",
	}

	for _, part := range expectedParts {
		if !strings.Contains(output, part) {
			t.Errorf("output missing %q: %s", part, output)
		}
	}
}

func TestShouldFetchIssueBodyModes(t *testing.T) {
	if shouldFetchIssueBody(gobench.Instance{IssueURL: ""}, false) {
		t.Fatalf("empty issue_url should be authored mode, not fetch")
	}
	if shouldFetchIssueBody(gobench.Instance{IssueURL: "https://github.com/o/r/issues/1"}, true) {
		t.Fatalf("-no-scrub should skip fetch even with issue_url")
	}
	if !shouldFetchIssueBody(gobench.Instance{IssueURL: "https://github.com/o/r/issues/1"}, false) {
		t.Fatalf("non-empty issue_url without -no-scrub should fetch")
	}
}
