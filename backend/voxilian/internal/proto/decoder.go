package proto

import (
	"encoding/binary"
	"fmt"
	"unicode/utf8"
)

// Decoder reads explicitly typed little-endian wire values from a
// bounded byte region. Every read bounds-checks before slicing;
// malformed or truncated input returns an error, never panics.
// Reads use explicit returned errors (no sticky-error mode).
type Decoder struct {
	buf []byte
	off int
}

// NewDecoder returns a Decoder over b. The caller must not mutate b
// while the Decoder (or any child entry decoder) is in use.
func NewDecoder(b []byte) *Decoder {
	return &Decoder{buf: b}
}

// Remaining reports unread bytes in this decoder's region.
func (d *Decoder) Remaining() int {
	return len(d.buf) - d.off
}

// Skip advances past n bytes or returns ErrTruncated.
func (d *Decoder) Skip(n int) error {
	if n < 0 || d.Remaining() < n {
		return fmt.Errorf("proto: skip %d with %d remaining: %w", n, d.Remaining(), ErrTruncated)
	}
	d.off += n
	return nil
}

// SkipRemaining discards all unread bytes (msg_version forward
// compatibility: ignore trailing unknown fields).
func (d *Decoder) SkipRemaining() {
	d.off = len(d.buf)
}

// ExpectEOF is an opt-in exact-consumption check for tests and
// internal uses. It is never required for protocol success: unknown
// trailing bytes are valid on the wire.
func (d *Decoder) ExpectEOF() error {
	if d.Remaining() != 0 {
		return fmt.Errorf("proto: %d trailing bytes: %w", d.Remaining(), ErrTruncated)
	}
	return nil
}

// take returns n bytes or ErrTruncated without panicking.
func (d *Decoder) take(n int) ([]byte, error) {
	if n < 0 || d.Remaining() < n {
		return nil, fmt.Errorf("proto: need %d bytes with %d remaining: %w", n, d.Remaining(), ErrTruncated)
	}
	b := d.buf[d.off : d.off+n]
	d.off += n
	return b, nil
}

// U8 reads one byte.
func (d *Decoder) U8() (uint8, error) {
	b, err := d.take(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

// U16 reads a little-endian uint16.
func (d *Decoder) U16() (uint16, error) {
	b, err := d.take(2)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(b), nil
}

// U32 reads a little-endian uint32.
func (d *Decoder) U32() (uint32, error) {
	b, err := d.take(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}

// U64 reads a little-endian uint64.
func (d *Decoder) U64() (uint64, error) {
	b, err := d.take(8)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b), nil
}

// I8 reads one signed byte.
func (d *Decoder) I8() (int8, error) {
	v, err := d.U8()
	return int8(v), err
}

// I16 reads a little-endian int16.
func (d *Decoder) I16() (int16, error) {
	v, err := d.U16()
	return int16(v), err
}

// I32 reads a little-endian int32.
func (d *Decoder) I32() (int32, error) {
	v, err := d.U32()
	return int32(v), err
}

// I64 reads a little-endian int64.
func (d *Decoder) I64() (int64, error) {
	v, err := d.U64()
	return int64(v), err
}

// String reads u16 byte-length + UTF-8 bytes, enforcing the caller
// supplied semantic cap max. The declared length is checked against
// max before remaining bytes are consulted, and no allocation is made
// from an unchecked attacker-controlled length.
func (d *Decoder) String(max int) (string, error) {
	n, err := d.U16()
	if err != nil {
		return "", err
	}
	if int(n) > max {
		return "", fmt.Errorf("proto: string length=%d max=%d: %w", n, max, ErrStringTooLong)
	}
	b, err := d.take(int(n))
	if err != nil {
		return "", err
	}
	if !utf8.Valid(b) {
		return "", fmt.Errorf("proto: string length=%d invalid utf8: %w", n, ErrInvalidUTF8)
	}
	return string(b), nil
}

// Count reads a u16 array count prefix, enforcing both the caller
// supplied max and the absolute MaxArrayCount ceiling.
func (d *Decoder) Count(max int) (int, error) {
	n, err := d.U16()
	if err != nil {
		return 0, err
	}
	if int(n) > max || int(n) > MaxArrayCount {
		return 0, fmt.Errorf("proto: array count=%d max=%d: %w", n, max, ErrArrayTooLong)
	}
	return int(n), nil
}

// Cell reads i32 cx + i32 cz.
func (d *Decoder) Cell() (Cell, error) {
	x, err := d.I32()
	if err != nil {
		return Cell{}, err
	}
	z, err := d.I32()
	if err != nil {
		return Cell{}, err
	}
	return Cell{X: x, Z: z}, nil
}

// Position reads 3×i32 millimeters.
func (d *Decoder) Position() (Position, error) {
	x, err := d.I32()
	if err != nil {
		return Position{}, err
	}
	y, err := d.I32()
	if err != nil {
		return Position{}, err
	}
	z, err := d.I32()
	if err != nil {
		return Position{}, err
	}
	return Position{X: x, Y: y, Z: z}, nil
}

// Angle reads a u16 wire angle, rejecting values above MaxAngle
// instead of masking them.
func (d *Decoder) Angle() (uint16, error) {
	v, err := d.U16()
	if err != nil {
		return 0, err
	}
	if v > MaxAngle {
		return 0, fmt.Errorf("proto: angle=%d max=%d: %w", v, MaxAngle, ErrAngleOutOfRange)
	}
	return v, nil
}

// ReadBytes returns n raw bytes with bounds checking.
func (d *Decoder) ReadBytes(n int) ([]byte, error) {
	b, err := d.take(n)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out, nil
}

// Entry reads [u16 entryLen][entry bytes] and returns a child decoder
// bounded to exactly those entry bytes. The outer decoder advances
// past the entire entry, so unknown trailing bytes inside the child
// never disturb the next entry's boundary. entryLen == 0 is
// structurally valid here; opcode layers decide whether required
// fields are missing.
func (d *Decoder) Entry() (*Decoder, error) {
	n, err := d.U16()
	if err != nil {
		return nil, err
	}
	if d.Remaining() < int(n) {
		return nil, fmt.Errorf("proto: entry length=%d with %d remaining: %w", n, d.Remaining(), ErrTruncated)
	}
	child := &Decoder{buf: d.buf[d.off : d.off+int(n)]}
	d.off += int(n)
	return child, nil
}
