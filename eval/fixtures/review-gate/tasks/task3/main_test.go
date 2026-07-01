package main

import (
	"strings"
	"testing"
)

func runOn(t *testing.T, input string, col int) string {
	t.Helper()
	var out strings.Builder
	if err := run(strings.NewReader(input), &out, col); err != nil {
		t.Fatalf("run: %v", err)
	}
	return out.String()
}

func TestFirstColumn(t *testing.T) {
	got := runOn(t, "a,b,c\nd,e,f\n", 1)
	if got != "a\nd\n" {
		t.Fatalf("got %q", got)
	}
}

func TestMiddleColumn(t *testing.T) {
	got := runOn(t, "a,b,c\nd,e,f\n", 2)
	if got != "b\ne\n" {
		t.Fatalf("got %q", got)
	}
}

func TestMissingColumnEmitsEmptyLine(t *testing.T) {
	got := runOn(t, "a,b\nc\n", 2)
	if got != "b\n\n" {
		t.Fatalf("got %q", got)
	}
}
