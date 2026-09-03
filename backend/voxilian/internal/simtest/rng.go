package simtest

import (
	"fmt"
	"math/rand/v2"
)

// RNG is a deterministic seeded pseudo-random source for tests. Each
// instance owns its state; there is no package-global RNG. Report the
// seed alongside any failure so randomized cases reproduce exactly.
type RNG struct {
	seed uint64
	rng  *rand.Rand
}

// NewRNG returns an RNG seeded explicitly by the test.
func NewRNG(seed uint64) *RNG {
	return &RNG{seed: seed, rng: rand.New(rand.NewPCG(seed, seed^0x9E3779B97F4A7C15))}
}

// Seed returns the seed this RNG was created with (for failure logs).
func (r *RNG) Seed() uint64 { return r.seed }

// Intn mirrors math/rand Intn over the owned stream.
func (r *RNG) Intn(n int) int { return r.rng.IntN(n) }

// Uint64 draws from the owned stream.
func (r *RNG) Uint64() uint64 { return r.rng.Uint64() }

// String returns a short reproducible identifier for the stream.
func (r *RNG) String() string { return fmt.Sprintf("simtest.RNG(seed=%d)", r.seed) }
