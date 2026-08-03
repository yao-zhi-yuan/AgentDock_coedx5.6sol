package mathutil

import "testing"

func TestDivideNonPositiveCountReturnsZero(t *testing.T) {
	for _, count := range []int{0, -1} {
		if got := Divide(10, count); got != 0 {
			t.Fatalf("Divide(10, %d) = %d, want 0", count, got)
		}
	}
}
