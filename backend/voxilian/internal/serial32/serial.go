// Package serial32 is the ONE canonical implementation of modulo-2^32
// serial ordering (RFC 1982 style) shared by the wire protocol and the
// simulation (spec §5.3.1). Every u32 serial domain — frame header seq,
// header tick, movement inputSeq — orders through these two functions;
// no package implements a rival comparator.
package serial32

// halfRange is 2^31: the exact ambiguity distance at which neither
// direction is newer.
const halfRange = 0x80000000

// After reports whether sequence number a is strictly after b under
// modulo-2^32 serial arithmetic. Let d = a - b computed modulo 2^32:
// a is after b iff d != 0 and d < 2^31.
//
// Equality is never "after". The comparison is deliberately undefined
// at an exact distance of 2^31 (half the range): both After directions
// return false there, so no contradictory ordering can arise.
func After(a, b uint32) bool {
	d := a - b // wraps modulo 2^32 by Go unsigned semantics
	return d != 0 && d < halfRange
}

// Before reports whether a is strictly before b: the inverse direction
// of After. Equality and the exact 2^31 distance are false in both
// directions.
func Before(a, b uint32) bool {
	return After(b, a)
}
