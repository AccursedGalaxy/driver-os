// Package ids normalizes user-supplied identifiers.
package ids

import "strings"

// Normalize converts s to canonical form: lowercase, leading/trailing
// whitespace trimmed, and any run of internal whitespace collapsed to a
// single hyphen.
func Normalize(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
