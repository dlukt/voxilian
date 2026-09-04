package proto

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func mustBytes(t *testing.T, e *Encoder) []byte {
	t.Helper()
	b, err := e.Bytes()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return b
}

func TestIntegerRoundTrip(t *testing.T) {
	e := NewEncoder()
	e.U8(0xAB)
	e.U16(0xCDEF)
	e.U32(0x01020304)
	e.U64(0x0102030405060708)
	e.I8(-5)
	e.I16(-300)
	e.I32(-70000)
	e.I64(math.MinInt64)
	b := mustBytes(t, e)

	d := NewDecoder(b)
	if v, err := d.U8(); err != nil || v != 0xAB {
		t.Fatalf("U8 = %v, %v", v, err)
	}
	if v, err := d.U16(); err != nil || v != 0xCDEF {
		t.Fatalf("U16 = %v, %v", v, err)
	}
	if v, err := d.U32(); err != nil || v != 0x01020304 {
		t.Fatalf("U32 = %v, %v", v, err)
	}
	if v, err := d.U64(); err != nil || v != 0x0102030405060708 {
		t.Fatalf("U64 = %v, %v", v, err)
	}
	if v, err := d.I8(); err != nil || v != -5 {
		t.Fatalf("I8 = %v, %v", v, err)
	}
	if v, err := d.I16(); err != nil || v != -300 {
		t.Fatalf("I16 = %v, %v", v, err)
	}
	if v, err := d.I32(); err != nil || v != -70000 {
		t.Fatalf("I32 = %v, %v", v, err)
	}
	if v, err := d.I64(); err != nil || v != math.MinInt64 {
		t.Fatalf("I64 = %v, %v", v, err)
	}
	if err := d.ExpectEOF(); err != nil {
		t.Fatalf("trailing bytes: %v", err)
	}
}

func TestI32NegativeOneExactWire(t *testing.T) {
	e := NewEncoder()
	e.I32(-1)
	b := mustBytes(t, e)
	want := []byte{0xFF, 0xFF, 0xFF, 0xFF}
	if len(b) != 4 || string(b) != string(want) {
		t.Fatalf("i32(-1) wire = % x, want % x", b, want)
	}
	d := NewDecoder(b)
	v, err := d.I32()
	if err != nil || v != -1 {
		t.Fatalf("I32 decode = %v, %v", v, err)
	}
}

func TestU16U32U64ExactWire(t *testing.T) {
	e := NewEncoder()
	e.U16(0x1234)
	e.U32(0x01020304)
	b := mustBytes(t, e)
	want := []byte{0x34, 0x12, 0x04, 0x03, 0x02, 0x01}
	if string(b) != string(want) {
		t.Fatalf("wire = % x, want % x", b, want)
	}
}

func TestStringExactWire(t *testing.T) {
	e := NewEncoder()
	e.String("A", MaxStringBytes)
	b := mustBytes(t, e)
	want := []byte{0x01, 0x00, 0x41}
	if string(b) != string(want) {
		t.Fatalf(`"A" wire = % x, want % x`, b, want)
	}

	e = NewEncoder()
	e.String("é", MaxStringBytes)
	b = mustBytes(t, e)
	// "é" is 2 UTF-8 bytes: c3 a9 — length prefix must be 2, not 1.
	want = []byte{0x02, 0x00, 0xC3, 0xA9}
	if string(b) != string(want) {
		t.Fatalf(`"é" wire = % x, want % x`, b, want)
	}

	// Empty string is valid: zero length prefix.
	e = NewEncoder()
	e.String("", MaxStringBytes)
	b = mustBytes(t, e)
	if string(b) != string([]byte{0x00, 0x00}) {
		t.Fatalf(`"" wire = % x`, b)
	}
}

func TestStringLimits(t *testing.T) {
	cases := []struct {
		name string
		max  int
		ok   int
		bad  int
	}{
		{"general", MaxStringBytes, 1024, 1025},
		{"chat", MaxChatBytes, 512, 513},
		{"token", MaxAccessTokenBytes, 8192, 8193},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok := strings.Repeat("x", tc.ok)
			e := NewEncoder()
			e.String(ok, tc.max)
			b, err := e.Bytes()
			if err != nil {
				t.Fatalf("len %d should encode: %v", tc.ok, err)
			}
			d := NewDecoder(b)
			got, err := d.String(tc.max)
			if err != nil || got != ok {
				t.Fatalf("round-trip len %d: %v", tc.ok, err)
			}

			bad := strings.Repeat("y", tc.bad)
			e = NewEncoder()
			e.String(bad, tc.max)
			if _, err := e.Bytes(); !errors.Is(err, ErrStringTooLong) {
				t.Fatalf("len %d should fail with ErrStringTooLong, got %v", tc.bad, err)
			}

			// Wire-level decode rejection: craft an over-cap length
			// that the encoder would never emit.
			if tc.bad <= maxU16 {
				enc := NewEncoder()
				enc.U16(uint16(tc.bad))
				enc.WriteBytes([]byte(bad))
				raw := mustBytes(t, enc)
				d := NewDecoder(raw)
				if _, err := d.String(tc.max); !errors.Is(err, ErrStringTooLong) {
					t.Fatalf("decode len %d should fail with ErrStringTooLong, got %v", tc.bad, err)
				}
			}
		})
	}
}

func TestStringInvalidUTF8(t *testing.T) {
	// Encoding Go strings that are not valid UTF-8 must fail.
	e := NewEncoder()
	e.String(string([]byte{0xFF, 0xFE}), MaxStringBytes)
	if _, err := e.Bytes(); !errors.Is(err, ErrInvalidUTF8) {
		t.Fatalf("encode invalid utf8: got %v", err)
	}

	// Decoding invalid UTF-8 bytes must fail with ErrInvalidUTF8.
	enc := NewEncoder()
	enc.U16(2)
	enc.WriteBytes([]byte{0xFF, 0xFE})
	raw := mustBytes(t, enc)
	d := NewDecoder(raw)
	if _, err := d.String(MaxStringBytes); !errors.Is(err, ErrInvalidUTF8) {
		t.Fatalf("decode invalid utf8: got %v", err)
	}
}

func TestStringDeclaredLongerThanRemaining(t *testing.T) {
	enc := NewEncoder()
	enc.U16(20)
	enc.WriteBytes([]byte("short"))
	raw := mustBytes(t, enc)
	d := NewDecoder(raw)
	if _, err := d.String(MaxStringBytes); !errors.Is(err, ErrTruncated) {
		t.Fatalf("got %v, want ErrTruncated", err)
	}
}

func TestArrayCount(t *testing.T) {
	e := NewEncoder()
	e.Count(1024, MaxArrayCount)
	b := mustBytes(t, e)
	if string(b) != string([]byte{0x00, 0x04}) {
		t.Fatalf("count 1024 wire = % x", b)
	}
	d := NewDecoder(b)
	n, err := d.Count(MaxArrayCount)
	if err != nil || n != 1024 {
		t.Fatalf("Count = %v, %v", n, err)
	}

	e = NewEncoder()
	e.Count(1025, MaxArrayCount)
	if _, err := e.Bytes(); !errors.Is(err, ErrArrayTooLong) {
		t.Fatalf("count 1025 encode: got %v", err)
	}

	enc := NewEncoder()
	enc.U16(1025)
	raw := mustBytes(t, enc)
	d = NewDecoder(raw)
	if _, err := d.Count(MaxArrayCount); !errors.Is(err, ErrArrayTooLong) {
		t.Fatalf("count 1025 decode: got %v", err)
	}
}

func TestCellRoundTrip(t *testing.T) {
	cases := []Cell{
		{X: 0, Z: 0},
		{X: 12, Z: -34},
		{X: math.MaxInt32, Z: math.MinInt32},
		{X: math.MinInt32, Z: math.MaxInt32},
	}
	for _, c := range cases {
		e := NewEncoder()
		e.Cell(c)
		b := mustBytes(t, e)
		if len(b) != 8 {
			t.Fatalf("cell wire len = %d", len(b))
		}
		d := NewDecoder(b)
		got, err := d.Cell()
		if err != nil || got != c {
			t.Fatalf("Cell(%+v) round-trip = %+v, %v", c, got, err)
		}
	}
}

func TestPositionRoundTrip(t *testing.T) {
	cases := []Position{
		{X: 0, Y: 0, Z: 0},
		{X: 1500, Y: -250, Z: 98765},
		{X: math.MaxInt32, Y: math.MinInt32, Z: -1},
		{X: math.MinInt32, Y: math.MaxInt32, Z: 0},
	}
	for _, p := range cases {
		e := NewEncoder()
		e.Position(p)
		b := mustBytes(t, e)
		if len(b) != 12 {
			t.Fatalf("pos wire len = %d", len(b))
		}
		d := NewDecoder(b)
		got, err := d.Position()
		if err != nil || got != p {
			t.Fatalf("Position(%+v) round-trip = %+v, %v", p, got, err)
		}
	}
}

func TestAngleBoundaries(t *testing.T) {
	for _, v := range []uint16{0, 1, 2048, 4095} {
		e := NewEncoder()
		e.Angle(v)
		b := mustBytes(t, e)
		d := NewDecoder(b)
		got, err := d.Angle()
		if err != nil || got != v {
			t.Fatalf("Angle(%d) round-trip = %d, %v", v, got, err)
		}
	}
	for _, v := range []uint16{4096, 8192, 65535} {
		e := NewEncoder()
		e.Angle(v)
		if _, err := e.Bytes(); !errors.Is(err, ErrAngleOutOfRange) {
			t.Fatalf("Angle(%d) encode: got %v", v, err)
		}
		enc := NewEncoder()
		enc.U16(v)
		raw := mustBytes(t, enc)
		d := NewDecoder(raw)
		if _, err := d.Angle(); !errors.Is(err, ErrAngleOutOfRange) {
			t.Fatalf("Angle(%d) decode: got %v", v, err)
		}
	}
}

func TestEntryFramingRoundTrip(t *testing.T) {
	e := NewEncoder()
	e.Entry(func(sub *Encoder) error {
		sub.U8(7)
		sub.U16(0x0102)
		return nil
	})
	e.Entry(func(sub *Encoder) error {
		sub.U8(9)
		return nil
	})
	b := mustBytes(t, e)
	// First entry: len 3 (1+2 bytes), second: len 1.
	want := []byte{0x03, 0x00, 0x07, 0x02, 0x01, 0x01, 0x00, 0x09}
	if string(b) != string(want) {
		t.Fatalf("entries wire = % x, want % x", b, want)
	}

	d := NewDecoder(b)
	c1, err := d.Entry()
	if err != nil {
		t.Fatalf("entry 1: %v", err)
	}
	if v, err := c1.U8(); err != nil || v != 7 {
		t.Fatalf("entry1 u8 = %v, %v", v, err)
	}
	if v, err := c1.U16(); err != nil || v != 0x0102 {
		t.Fatalf("entry1 u16 = %v, %v", v, err)
	}
	c2, err := d.Entry()
	if err != nil {
		t.Fatalf("entry 2: %v", err)
	}
	if v, err := c2.U8(); err != nil || v != 9 {
		t.Fatalf("entry2 u8 = %v, %v", v, err)
	}
	if err := d.ExpectEOF(); err != nil {
		t.Fatalf("outer trailing: %v", err)
	}
}

func TestEntryForwardCompatibility(t *testing.T) {
	// Entry 1 carries a future u32 after the known u8 field;
	// entry 2 is old-shaped. An old decoder reads only the known
	// prefix of each child and must land on the next boundary.
	e := NewEncoder()
	e.Entry(func(sub *Encoder) error {
		sub.U8(42)          // known field
		sub.U32(0xDEADBEEF) // unknown future field
		return nil
	})
	e.Entry(func(sub *Encoder) error {
		sub.U8(43)
		return nil
	})
	b := mustBytes(t, e)

	d := NewDecoder(b)
	c1, err := d.Entry()
	if err != nil {
		t.Fatalf("entry 1: %v", err)
	}
	known, err := c1.U8()
	if err != nil || known != 42 {
		t.Fatalf("entry1 known = %v, %v", known, err)
	}
	// Old decoder ignores the 4 trailing future bytes inside entry 1.
	if got := c1.Remaining(); got != 4 {
		t.Fatalf("entry1 remaining = %d, want 4", got)
	}
	c1.SkipRemaining()

	c2, err := d.Entry()
	if err != nil {
		t.Fatalf("entry 2: %v", err)
	}
	known, err = c2.U8()
	if err != nil || known != 43 {
		t.Fatalf("entry2 known = %v, %v", known, err)
	}
	if err := c2.ExpectEOF(); err != nil {
		t.Fatalf("entry2 should be exact: %v", err)
	}
	if err := d.ExpectEOF(); err != nil {
		t.Fatalf("outer should be exact: %v", err)
	}
}

func TestEntryChildCannotEscape(t *testing.T) {
	e := NewEncoder()
	e.Entry(func(sub *Encoder) error {
		sub.U8(0xAA)
		return nil
	})
	e.Entry(func(sub *Encoder) error {
		sub.U8(0xBB)
		return nil
	})
	b := mustBytes(t, e)

	d := NewDecoder(b)
	c1, err := d.Entry()
	if err != nil {
		t.Fatalf("entry 1: %v", err)
	}
	if _, err := c1.U8(); err != nil {
		t.Fatalf("entry1 first byte: %v", err)
	}
	// Child is bounded to its 1 byte; a second read must fail and
	// must not leak into the adjacent entry.
	if _, err := c1.U8(); !errors.Is(err, ErrTruncated) {
		t.Fatalf("child escape read: got %v", err)
	}
	c2, err := d.Entry()
	if err != nil {
		t.Fatalf("entry 2: %v", err)
	}
	v, err := c2.U8()
	if err != nil || v != 0xBB {
		t.Fatalf("entry2 = %v, %v", v, err)
	}
}

func TestEntryMalformed(t *testing.T) {
	// Truncated prefix: only one byte where u16 entryLen is required.
	d := NewDecoder([]byte{0x05})
	if _, err := d.Entry(); !errors.Is(err, ErrTruncated) {
		t.Fatalf("truncated prefix: got %v", err)
	}

	// Declared entry exceeds remaining frame.
	d = NewDecoder([]byte{0x14, 0x00, 0x01, 0x02, 0x03, 0x04, 0x05})
	if _, err := d.Entry(); !errors.Is(err, ErrTruncated) {
		t.Fatalf("overlong entry: got %v", err)
	}

	// Empty entry is structurally valid at the framing layer.
	d = NewDecoder([]byte{0x00, 0x00})
	child, err := d.Entry()
	if err != nil {
		t.Fatalf("empty entry: %v", err)
	}
	if got := child.Remaining(); got != 0 {
		t.Fatalf("empty entry remaining = %d", got)
	}
}

func TestDecoderTruncationNeverPanics(t *testing.T) {
	cases := []struct {
		name string
		buf  []byte
		run  func(d *Decoder) error
	}{
		{"u16", []byte{0x01}, func(d *Decoder) error { _, err := d.U16(); return err }},
		{"u32-short", []byte{0x01, 0x02, 0x03}, func(d *Decoder) error { _, err := d.U32(); return err }},
		{"u64-short", []byte{0x01, 0x02, 0x03}, func(d *Decoder) error { _, err := d.U64(); return err }},
		{"cell", []byte{0x01, 0x02, 0x03}, func(d *Decoder) error { _, err := d.Cell(); return err }},
		{"pos", []byte{0x01, 0x02, 0x03, 0x04, 0x05}, func(d *Decoder) error { _, err := d.Position(); return err }},
		{"angle", []byte{0x01}, func(d *Decoder) error { _, err := d.Angle(); return err }},
		{"string-prefix", []byte{0x01}, func(d *Decoder) error { _, err := d.String(MaxStringBytes); return err }},
		{"count-prefix", []byte{0x01}, func(d *Decoder) error { _, err := d.Count(MaxArrayCount); return err }},
		{"bytes", []byte{0x01}, func(d *Decoder) error { _, err := d.ReadBytes(4); return err }},
		{"skip", []byte{0x01}, func(d *Decoder) error { return d.Skip(4) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panicked: %v", r)
				}
			}()
			d := NewDecoder(tc.buf)
			if err := tc.run(d); !errors.Is(err, ErrTruncated) {
				t.Fatalf("got %v, want ErrTruncated", err)
			}
		})
	}
}

func TestRawBytesPrimitive(t *testing.T) {
	e := NewEncoder()
	e.WriteBytes([]byte{0x01, 0x02, 0x03})
	b := mustBytes(t, e)
	d := NewDecoder(b)
	got, err := d.ReadBytes(3)
	if err != nil || string(got) != string([]byte{0x01, 0x02, 0x03}) {
		t.Fatalf("ReadBytes = % x, %v", got, err)
	}
	if _, err := d.ReadBytes(1); !errors.Is(err, ErrTruncated) {
		t.Fatalf("overread: got %v", err)
	}
}
