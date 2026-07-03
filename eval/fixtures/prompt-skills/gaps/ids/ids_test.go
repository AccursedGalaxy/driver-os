package ids

import "testing"

// TestNormalizeCollapsesInternalWhitespace is the reported bug's repro:
// Normalize("Order   Id") currently returns "order   id" instead of
// "order-id".
func TestNormalizeCollapsesInternalWhitespace(t *testing.T) {
	got := Normalize("Order   Id")
	if got != "order-id" {
		t.Fatalf("Normalize(%q) = %q, want %q", "Order   Id", got, "order-id")
	}
}
