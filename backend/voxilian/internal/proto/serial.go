package proto

import (
	"github.com/dlukt/voxilian/internal/serial32"
)

// Serial32After reports whether sequence number a is strictly after b
// under modulo-2^32 serial arithmetic (RFC 1982 style). This is a thin
// source-compatible delegate to the ONE canonical implementation in
// internal/serial32 (spec §5.3.1); names and behavior are unchanged.
// This single helper serves every u32 serial domain on the wire (frame
// header seq, header tick, Move.inputSeq); M3/M4 consume it for
// ordering and MUST NOT implement rival arithmetic.
//
// The comparison is deliberately undefined at an exact distance of
// 2^31 (half the range): both After directions return false there, so
// no contradictory ordering can arise. Equality is never "after".
func Serial32After(a, b uint32) bool {
	return serial32.After(a, b)
}

// Serial32Before reports whether a is strictly before b: the inverse
// direction of Serial32After. Equality and the exact 2^31 distance are
// false in both directions.
func Serial32Before(a, b uint32) bool {
	return serial32.Before(a, b)
}
