// Package proto implements the Voxilian binary protocol codec core.
//
// Every WebSocket message is exactly one binary frame:
//
//	[u16 opcode][u16 msg_version][u32 seq][u32 tick][payload...]
//
// Protocol invariants (see docs/backend-spec.md §6):
//
//   - All multibyte integers are little-endian.
//   - The header is exactly HeaderSize (12) bytes.
//   - The frame maximum (MaxFrameSize = 64 KiB) includes the header:
//     len == 65536 is structurally allowed, len == 65537 is rejected
//     with ErrFrameTooLarge before any parsing.
//   - Strings are u16 byte-length-prefixed UTF-8; the length field counts
//     UTF-8 bytes, not runes. Caps: general 1024, chat 512, accessToken 8 KiB.
//   - Arrays are u16 count-prefixed, max 1024 elements.
//   - Positions are millimeter fixed-point i32 triples; no floats on the wire.
//   - Angles are 12-bit values (0..4095) transported in a u16; out-of-range
//     values are rejected, never masked.
//   - Every repeated entry is [u16 entryLen][entry bytes] where entryLen
//     counts the bytes after its own u16 prefix. Old decoders read the
//     known prefix of each entry and ignore the remainder.
//   - Parsers MUST ignore trailing unknown payload bytes (msg_version
//     forward compatibility). Exact consumption is opt-in via ExpectEOF.
//
// This package owns wire representation only. It has no opcode knowledge,
// no WebSocket dependency, and no store/sim imports.
package proto
