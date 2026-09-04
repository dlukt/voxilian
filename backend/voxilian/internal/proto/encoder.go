package proto

import (
	"encoding/binary"
	"fmt"
	"unicode/utf8"
)

// Encoder accumulates explicitly written little-endian wire bytes.
// The zero value is not usable; construct with NewEncoder.
// Methods record the first error and make later writes no-ops so
// hand-written message codecs stay linear; callers check Err
// (or the error from Bytes) once at the end.
type Encoder struct {
	buf []byte
	err error
}

// NewEncoder returns an empty Encoder.
func NewEncoder() *Encoder {
	return &Encoder{}
}

// Err reports the first encoding error, if any.
func (e *Encoder) Err() error {
	return e.err
}

// Bytes returns the accumulated payload bytes and the first error.
// The returned slice is owned by the Encoder; callers needing to
// retain it must copy.
func (e *Encoder) Bytes() ([]byte, error) {
	if e.err != nil {
		return nil, e.err
	}
	return e.buf, nil
}

// mustFit records err if the encoder already failed, for early exit.
func (e *Encoder) failed() bool {
	return e.err != nil
}

func (e *Encoder) fail(err error) {
	if e.err == nil {
		e.err = err
	}
}

// U8 writes a single byte.
func (e *Encoder) U8(v uint8) {
	if e.failed() {
		return
	}
	e.buf = append(e.buf, v)
}

// U16 writes a little-endian uint16.
func (e *Encoder) U16(v uint16) {
	if e.failed() {
		return
	}
	var b [2]byte
	binary.LittleEndian.PutUint16(b[:], v)
	e.buf = append(e.buf, b[:]...)
}

// U32 writes a little-endian uint32.
func (e *Encoder) U32(v uint32) {
	if e.failed() {
		return
	}
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	e.buf = append(e.buf, b[:]...)
}

// U64 writes a little-endian uint64.
func (e *Encoder) U64(v uint64) {
	if e.failed() {
		return
	}
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	e.buf = append(e.buf, b[:]...)
}

// I8 writes a single signed byte.
func (e *Encoder) I8(v int8) {
	e.U8(uint8(v))
}

// I16 writes a little-endian int16 (two's complement).
func (e *Encoder) I16(v int16) {
	e.U16(uint16(v))
}

// I32 writes a little-endian int32 (two's complement).
func (e *Encoder) I32(v int32) {
	e.U32(uint32(v))
}

// I64 writes a little-endian int64 (two's complement).
func (e *Encoder) I64(v int64) {
	e.U64(uint64(v))
}

// String writes u16 byte-length + UTF-8 bytes, enforcing the caller
// supplied semantic cap max (use MaxStringBytes, MaxChatBytes, or
// MaxAccessTokenBytes). Length counts UTF-8 bytes, not runes.
// Empty strings are valid. Invalid UTF-8, over-cap lengths, and
// lengths unrepresentable in u16 are rejected; nothing is truncated.
func (e *Encoder) String(s string, max int) {
	if e.failed() {
		return
	}
	if !utf8.ValidString(s) {
		e.fail(fmt.Errorf("proto: string invalid utf8: %w", ErrInvalidUTF8))
		return
	}
	n := len(s)
	if n > max {
		e.fail(fmt.Errorf("proto: string length=%d max=%d: %w", n, max, ErrStringTooLong))
		return
	}
	if n > maxU16 {
		e.fail(fmt.Errorf("proto: string length=%d exceeds u16: %w", n, ErrStringTooLong))
		return
	}
	e.U16(uint16(n))
	if e.failed() {
		return
	}
	e.buf = append(e.buf, s...)
}

// Count writes a u16 array count prefix, enforcing both the caller
// supplied max and the absolute MaxArrayCount ceiling.
func (e *Encoder) Count(n, max int) {
	if e.failed() {
		return
	}
	if n < 0 || n > max || n > MaxArrayCount || n > maxU16 {
		e.fail(fmt.Errorf("proto: array count=%d max=%d: %w", n, max, ErrArrayTooLong))
		return
	}
	e.U16(uint16(n))
}

// Cell writes i32 cx + i32 cz.
func (e *Encoder) Cell(c Cell) {
	if e.failed() {
		return
	}
	e.I32(c.X)
	e.I32(c.Z)
}

// Position writes 3×i32 millimeters.
func (e *Encoder) Position(p Position) {
	if e.failed() {
		return
	}
	e.I32(p.X)
	e.I32(p.Y)
	e.I32(p.Z)
}

// Angle writes a 12-bit heading transported in a u16.
// Values above MaxAngle are rejected, never masked.
func (e *Encoder) Angle(v uint16) {
	if e.failed() {
		return
	}
	if v > MaxAngle {
		e.fail(fmt.Errorf("proto: angle=%d max=%d: %w", v, MaxAngle, ErrAngleOutOfRange))
		return
	}
	e.U16(v)
}

// WriteBytes appends raw bytes with no length prefix. The caller owns
// framing (entry handling uses this internally; a later chunk codec
// may use it for fragment payloads). No compression is applied.
func (e *Encoder) WriteBytes(b []byte) {
	if e.failed() {
		return
	}
	e.buf = append(e.buf, b...)
}

// Entry encodes one repeated-structure entry as
// [u16 entryLen][entry bytes], where entryLen counts only the bytes
// after its own prefix. The entry is first encoded into a temporary
// buffer so its length is known; entries above the u16 range fail
// with ErrEntryTooLarge. No nested tags are introduced.
func (e *Encoder) Entry(fn func(*Encoder) error) {
	if e.failed() {
		return
	}
	sub := NewEncoder()
	if err := fn(sub); err != nil {
		e.fail(err)
		return
	}
	body, err := sub.Bytes()
	if err != nil {
		e.fail(err)
		return
	}
	if len(body) > maxU16 {
		e.fail(fmt.Errorf("proto: entry length=%d exceeds u16: %w", len(body), ErrEntryTooLarge))
		return
	}
	e.U16(uint16(len(body)))
	if e.failed() {
		return
	}
	e.buf = append(e.buf, body...)
}
