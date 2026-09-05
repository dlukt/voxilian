package sim

import (
	"testing"

	"github.com/dlukt/voxilian/internal/world"
)

func sampleAt(tick uint32, x float64) PositionSample {
	return PositionSample{Tick: tick, Position: world.Vec3{X: x}}
}

func tickSequence(s []PositionSample) []uint32 {
	out := make([]uint32, len(s))
	for i, v := range s {
		out[i] = v.Tick
	}
	return out
}

func equalUint32(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPositionHistoryEmpty(t *testing.T) {
	h := newPositionHistory(40)
	if h.Len() != 0 {
		t.Fatalf("Len = %d, want 0", h.Len())
	}
	if h.Capacity() != 40 {
		t.Fatalf("Capacity = %d, want 40", h.Capacity())
	}
	if got := h.Samples(); len(got) != 0 {
		t.Fatalf("Samples len = %d, want 0", len(got))
	}
	if _, ok := h.Latest(); ok {
		t.Fatal("Latest on empty must report false")
	}
}

func TestPositionHistoryPartial(t *testing.T) {
	h := newPositionHistory(40)
	for i := uint32(1); i <= 10; i++ {
		h.Append(sampleAt(i, float64(i)))
	}
	if h.Len() != 10 {
		t.Fatalf("Len = %d, want 10", h.Len())
	}
	got := h.Samples()
	if len(got) != 10 {
		t.Fatalf("Samples len = %d, want 10", len(got))
	}
	for i, s := range got {
		if s.Tick != uint32(i+1) || s.Position.X != float64(i+1) {
			t.Fatalf("sample %d = %+v, want tick %d", i, s, i+1)
		}
	}
	latest, ok := h.Latest()
	if !ok || latest.Tick != 10 {
		t.Fatalf("Latest = %+v,%v, want tick 10", latest, ok)
	}
}

func TestPositionHistoryExactCapacity(t *testing.T) {
	h := newPositionHistory(40)
	for i := uint32(1); i <= 40; i++ {
		h.Append(sampleAt(i, float64(i)))
	}
	if h.Len() != 40 {
		t.Fatalf("Len = %d, want 40", h.Len())
	}
	got := tickSequence(h.Samples())
	want := make([]uint32, 40)
	for i := range want {
		want[i] = uint32(i + 1)
	}
	if !equalUint32(got, want) {
		t.Fatalf("ticks = %v, want 1..40", got)
	}
}

func TestPositionHistoryOverwriteOne(t *testing.T) {
	h := newPositionHistory(40)
	for i := uint32(1); i <= 41; i++ {
		h.Append(sampleAt(i, float64(i)))
	}
	if h.Len() != 40 {
		t.Fatalf("Len = %d, want 40", h.Len())
	}
	got := tickSequence(h.Samples())
	want := make([]uint32, 40)
	for i := range want {
		want[i] = uint32(i + 2)
	}
	if !equalUint32(got, want) {
		t.Fatalf("ticks = %v, want 2..41", got)
	}
	latest, _ := h.Latest()
	if latest.Tick != 41 {
		t.Fatalf("Latest tick = %d, want 41", latest.Tick)
	}
}

func TestPositionHistoryManyWraps(t *testing.T) {
	h := newPositionHistory(40)
	for i := uint32(1); i <= 400; i++ {
		h.Append(sampleAt(i, float64(i)))
	}
	if h.Len() != 40 {
		t.Fatalf("Len = %d, want 40", h.Len())
	}
	got := h.Samples()
	if uint32(got[0].Tick) != 361 || uint32(got[39].Tick) != 400 {
		t.Fatalf("oldest/newest = %d/%d, want 361/400", got[0].Tick, got[39].Tick)
	}
	for i, s := range got {
		if s.Position.X != float64(361+i) {
			t.Fatalf("sample %d X = %v, want %v", i, s.Position.X, float64(361+i))
		}
	}
}

func TestPositionHistoryTickWrapChronological(t *testing.T) {
	h := newPositionHistory(4)
	h.Append(sampleAt(^uint32(0)-1, 1))
	h.Append(sampleAt(^uint32(0), 2))
	h.Append(sampleAt(0, 3))
	h.Append(sampleAt(1, 4))
	got := tickSequence(h.Samples())
	want := []uint32{^uint32(0) - 1, ^uint32(0), 0, 1}
	if !equalUint32(got, want) {
		t.Fatalf("ring order = %v, want %v (chronological, NOT numeric sort)", got, want)
	}
	// Overflow one more: oldest (MaxUint32-1) drops.
	h.Append(sampleAt(2, 5))
	got = tickSequence(h.Samples())
	want = []uint32{^uint32(0), 0, 1, 2}
	if !equalUint32(got, want) {
		t.Fatalf("ring order = %v, want %v", got, want)
	}
}

func TestPositionHistorySamplesAreCopies(t *testing.T) {
	h := newPositionHistory(4)
	h.Append(sampleAt(1, 100))
	got := h.Samples()
	got[0].Tick = 999
	got[0].Position.X = -999
	again := h.Samples()
	if again[0].Tick != 1 || again[0].Position.X != 100 {
		t.Fatalf("mutating Samples() result altered ring: %+v", again[0])
	}
	latest, _ := h.Latest()
	if latest.Tick != 1 {
		t.Fatalf("Latest tick = %d, want 1", latest.Tick)
	}
}

func TestPositionHistorySmallCapacity(t *testing.T) {
	h := newPositionHistory(20)
	if h.Capacity() != 20 {
		t.Fatalf("Capacity = %d, want 20", h.Capacity())
	}
	for i := uint32(1); i <= 25; i++ {
		h.Append(sampleAt(i, float64(i)))
	}
	got := tickSequence(h.Samples())
	if len(got) != 20 || got[0] != 6 || got[19] != 25 {
		t.Fatalf("ticks = %v, want 6..25", got)
	}
}
