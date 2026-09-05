package serial32

import (
	"math"
	"testing"
)

func TestAfterBasics(t *testing.T) {
	cases := []struct {
		a, b  uint32
		after bool
	}{
		{1, 0, true},
		{0, math.MaxUint32, true},
		{1, math.MaxUint32, true},
		{math.MaxUint32, 0, false},
		{0x7FFFFFFF, 0, true},
		{0x80000000, 0, false}, // exact half-range: ambiguous, neither side newer
		{0x80000001, 0, false},
		{100, 99, true},
		{99, 100, false},
		{0, 1, false},
	}
	for _, tc := range cases {
		if got := After(tc.a, tc.b); got != tc.after {
			t.Fatalf("After(%d, %d) = %v, want %v", tc.a, tc.b, got, tc.after)
		}
		if got := Before(tc.b, tc.a); got != tc.after {
			t.Fatalf("Before(%d, %d) = %v, want %v", tc.b, tc.a, got, tc.after)
		}
	}
}

func TestEqualityAndHalfRange(t *testing.T) {
	for _, x := range []uint32{0, 1, 0x7FFFFFFF, 0x80000000, math.MaxUint32} {
		if After(x, x) {
			t.Fatalf("After(%d, %d) = true, want false", x, x)
		}
		if Before(x, x) {
			t.Fatalf("Before(%d, %d) = true, want false", x, x)
		}
	}
	// Half-range ambiguity is symmetric: both directions false.
	if After(0x80000000, 0) || After(0, 0x80000000) {
		t.Fatalf("half-range After must be false both ways")
	}
	if Before(0x80000000, 0) || Before(0, 0x80000000) {
		t.Fatalf("half-range Before must be false both ways")
	}
}

func TestWraparound(t *testing.T) {
	prev := uint32(math.MaxUint32)
	next := uint32(0)
	if !After(next, prev) {
		t.Fatalf("wraparound 0 after MaxUint32 = false")
	}
	if !Before(prev, next) {
		t.Fatalf("wraparound MaxUint32 before 0 = false")
	}
	// Forward progress across the wrap stays ordered.
	if !After(5, prev) || !Before(prev, 5) {
		t.Fatalf("post-wrap ordering broken")
	}
	// Stale values behind the wrap are not newer.
	if After(prev, next) || After(prev-1, next) {
		t.Fatalf("pre-wrap value must not be after post-wrap value")
	}
	// MaxUint32 -> 0 -> 1 chain.
	if !After(0, prev) || !After(1, 0) || !After(1, prev) {
		t.Fatalf("wrap chain MaxUint32 -> 0 -> 1 broken")
	}
}
