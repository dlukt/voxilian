package proto

// Serial32After reports whether sequence number a is strictly after b
// under modulo-2^32 serial arithmetic (RFC 1982 style). Let
// d = a - b computed modulo 2^32: a is after b iff d != 0 and
// d < 2^31. This single helper serves every u32 serial domain on the
// wire (frame header seq, header tick, Move.inputSeq); M3/M4 consume it
// for ordering and MUST NOT implement rival arithmetic.
//
// The comparison is deliberately undefined at an exact distance of
// 2^31 (half the range): both After directions return false there, so
// no contradictory ordering can arise. Equality is never "after".
func Serial32After(a, b uint32) bool {
	d := a - b // wraps modulo 2^32 by Go unsigned semantics
	return d != 0 && d < 0x80000000
}

// Serial32Before reports whether a is strictly before b: the inverse
// direction of Serial32After. Equality and the exact 2^31 distance are
// false in both directions.
func Serial32Before(a, b uint32) bool {
	return Serial32After(b, a)
}
