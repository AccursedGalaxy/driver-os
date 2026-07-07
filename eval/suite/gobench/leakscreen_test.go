package gobench

import "testing"

func TestNgramOverlapScoreAndBoundary(t *testing.T) {
	stmt := "one two three four five six seven eight nine"
	diff := "+one two three four five six seven eight changed\n"
	got := NgramOverlapScore(stmt, diff, 8)
	if got != 0.5 {
		t.Fatalf("score=%v want 0.5", got)
	}
	ls := LeakScreen{Score: got, Threshold: 0.5, Passed: got <= 0.5}
	if !ls.Passed {
		t.Fatalf("score at threshold should pass")
	}
	ls = LeakScreen{Score: got, Threshold: 0.49, Passed: got <= 0.49}
	if ls.Passed {
		t.Fatalf("score over threshold should fail")
	}
}

func TestScreenedDiffExcludesTestFiles(t *testing.T) {
	diff := "diff --git a/foo.go b/foo.go\n+keep prod line\ndiff --git a/foo_test.go b/foo_test.go\n+drop oracle line\n"
	text, ex := screenedDiffText(diff)
	if text != "keep prod line\n" {
		t.Fatalf("text=%q", text)
	}
	if len(ex) != 1 || ex[0] != "foo_test.go" {
		t.Fatalf("excluded=%v", ex)
	}
}

func TestHashStability(t *testing.T) {
	if sha256Hex("abc") != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatal("sha mismatch")
	}
}
