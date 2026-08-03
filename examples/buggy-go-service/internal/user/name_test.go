package user

import "testing"

func TestNormalizeNameTrimsWhitespace(t *testing.T) {
	if got := NormalizeName("  Alice  "); got != "alice" {
		t.Fatalf("NormalizeName() = %q, want %q", got, "alice")
	}
}
