package proto

import (
	"math"
	"testing"
)

func TestSerial32Basics(t *testing.T) {
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
		if got := Serial32After(tc.a, tc.b); got != tc.after {
			t.Fatalf("Serial32After(%d, %d) = %v, want %v", tc.a, tc.b, got, tc.after)
		}
		if got := Serial32Before(tc.b, tc.a); got != tc.after {
			t.Fatalf("Serial32Before(%d, %d) = %v, want %v", tc.b, tc.a, got, tc.after)
		}
	}
}

func TestSerial32EqualityAndHalfRange(t *testing.T) {
	for _, x := range []uint32{0, 1, 0x7FFFFFFF, 0x80000000, math.MaxUint32} {
		if Serial32After(x, x) {
			t.Fatalf("Serial32After(%d, %d) = true, want false", x, x)
		}
		if Serial32Before(x, x) {
			t.Fatalf("Serial32Before(%d, %d) = true, want false", x, x)
		}
	}
	// Half-range ambiguity is symmetric: both directions false.
	if Serial32After(0x80000000, 0) || Serial32After(0, 0x80000000) {
		t.Fatalf("half-range After must be false both ways")
	}
	if Serial32Before(0x80000000, 0) || Serial32Before(0, 0x80000000) {
		t.Fatalf("half-range Before must be false both ways")
	}
}

func TestSerial32Wraparound(t *testing.T) {
	prev := uint32(math.MaxUint32)
	next := uint32(0)
	if !Serial32After(next, prev) {
		t.Fatalf("wraparound 0 after MaxUint32 = false")
	}
	if !Serial32Before(prev, next) {
		t.Fatalf("wraparound MaxUint32 before 0 = false")
	}
	// Forward progress across the wrap stays ordered.
	if !Serial32After(5, prev) || !Serial32Before(prev, 5) {
		t.Fatalf("post-wrap ordering broken")
	}
	// Stale values behind the wrap are not newer.
	if Serial32After(prev, next) || Serial32After(prev-1, next) {
		t.Fatalf("pre-wrap value must not be after post-wrap value")
	}
}

func TestSerial32Domains(t *testing.T) {
	// The same helper serves header seq, header tick, and Move.inputSeq.
	prev := uint32(math.MaxUint32)
	next := uint32(0)
	headerBefore := Header{Opcode: OpcodeMove, MsgVersion: 1, Seq: prev, Tick: prev}
	headerAfter := Header{Opcode: OpcodeMove, MsgVersion: 1, Seq: next, Tick: next}
	moveBefore := Move{InputSeq: prev}
	moveAfter := Move{InputSeq: next}

	if !Serial32After(headerAfter.Seq, headerBefore.Seq) {
		t.Fatalf("header Seq wraparound not after")
	}
	if !Serial32After(headerAfter.Tick, headerBefore.Tick) {
		t.Fatalf("header Tick wraparound not after")
	}
	if !Serial32After(moveAfter.InputSeq, moveBefore.InputSeq) {
		t.Fatalf("Move.InputSeq wraparound not after")
	}
	if !Serial32Before(headerBefore.Seq, headerAfter.Seq) ||
		!Serial32Before(headerBefore.Tick, headerAfter.Tick) ||
		!Serial32Before(moveBefore.InputSeq, moveAfter.InputSeq) {
		t.Fatalf("Before direction broken across domains")
	}
}
