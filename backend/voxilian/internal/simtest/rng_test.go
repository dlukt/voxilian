package simtest

import (
	"testing"
)

func draw(r *RNG, n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = r.Intn(1_000_000)
	}
	return out
}

func equal(a, b []int) bool {
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

func TestRNGDeterministic(t *testing.T) {
	const seed = 12345
	a, b := draw(NewRNG(seed), 100), draw(NewRNG(seed), 100)
	if !equal(a, b) {
		t.Fatalf("same seed %d diverged (streams not reproducible)", seed)
	}
}

func TestRNGSeedsDiffer(t *testing.T) {
	a, b := draw(NewRNG(1), 100), draw(NewRNG(2), 100)
	if equal(a, b) {
		t.Fatal("different seeds produced identical streams")
	}
}

func TestRNGSeedReported(t *testing.T) {
	r := NewRNG(777)
	if r.Seed() != 777 {
		t.Fatalf("Seed() = %d, want 777", r.Seed())
	}
	t.Logf("reproduce with %s", r)
	if s := r.String(); s == "" {
		t.Fatal("empty String()")
	}
}
