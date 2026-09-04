package proto

import (
	"errors"
	"math"
	"testing"
)

func TestHeaderEndiannessExactWire(t *testing.T) {
	h := Header{Opcode: 0x1234, MsgVersion: 0x5678, Seq: 0x01020304, Tick: 0xA1B2C3D4}
	frame, err := EncodeFrame(h, nil)
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	if len(frame) != HeaderSize {
		t.Fatalf("empty-payload frame len = %d, want %d", len(frame), HeaderSize)
	}
	want := []byte{
		0x34, 0x12, // opcode LE
		0x78, 0x56, // msg_version LE
		0x04, 0x03, 0x02, 0x01, // seq LE
		0xD4, 0xC3, 0xB2, 0xA1, // tick LE
	}
	if string(frame) != string(want) {
		t.Fatalf("header wire = % x, want % x", frame, want)
	}
}

func TestFrameRoundTrip(t *testing.T) {
	h := Header{Opcode: 0x00C8, MsgVersion: 3, Seq: 42, Tick: 9001}
	frame, err := EncodeFrame(h, func(e *Encoder) error {
		e.U32(0xDEADBEEF)
		e.String("hi", MaxStringBytes)
		return nil
	})
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	got, dec, err := DecodeFrame(frame)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if got != h {
		t.Fatalf("header = %+v, want %+v", got, h)
	}
	if v, err := dec.U32(); err != nil || v != 0xDEADBEEF {
		t.Fatalf("payload u32 = %v, %v", v, err)
	}
	if s, err := dec.String(MaxStringBytes); err != nil || s != "hi" {
		t.Fatalf("payload string = %q, %v", s, err)
	}
	if err := dec.ExpectEOF(); err != nil {
		t.Fatalf("payload should be exact: %v", err)
	}
}

func TestFrameUnknownOpcodeParses(t *testing.T) {
	h := Header{Opcode: 0xFFFF, MsgVersion: 0, Seq: 1, Tick: 2}
	frame, err := EncodeFrame(h, nil)
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	got, dec, err := DecodeFrame(frame)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if got.Opcode != 0xFFFF {
		t.Fatalf("opcode = %v", got.Opcode)
	}
	if got := dec.Remaining(); got != 0 {
		t.Fatalf("remaining = %d", got)
	}
}

func TestFrameSizeBoundary(t *testing.T) {
	// HeaderSize + payload == 65536 succeeds.
	payloadSize := MaxFrameSize - HeaderSize
	frame, err := EncodeFrame(Header{Opcode: 1}, func(e *Encoder) error {
		e.WriteBytes(make([]byte, payloadSize))
		return nil
	})
	if err != nil {
		t.Fatalf("65536-byte frame should encode: %v", err)
	}
	if len(frame) != MaxFrameSize {
		t.Fatalf("frame len = %d, want %d", len(frame), MaxFrameSize)
	}
	if _, _, err := DecodeFrame(frame); err != nil {
		t.Fatalf("65536-byte frame should decode: %v", err)
	}

	// One extra byte fails on encode.
	if _, err := EncodeFrame(Header{Opcode: 1}, func(e *Encoder) error {
		e.WriteBytes(make([]byte, payloadSize+1))
		return nil
	}); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("65537-byte encode: got %v", err)
	}

	// 65537 raw bytes fail on decode before header interpretation.
	oversized := make([]byte, MaxFrameSize+1)
	if _, _, err := DecodeFrame(oversized); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("65537-byte decode: got %v", err)
	}
}

func TestFrameTruncatedHeaders(t *testing.T) {
	for _, n := range []int{0, 1, 5, 11} {
		if _, _, err := DecodeFrame(make([]byte, n)); !errors.Is(err, ErrTruncated) {
			t.Fatalf("len %d: got %v, want ErrTruncated", n, err)
		}
	}
}

func TestFrameTrailingBytesTolerated(t *testing.T) {
	// Synthetic msg_version=2 frame: known u8 field + future bytes.
	frame, err := EncodeFrame(Header{Opcode: 7, MsgVersion: 2}, func(e *Encoder) error {
		e.U8(0x2A)
		e.U32(0x11223344) // future field
		e.WriteBytes([]byte{0xFF})
		return nil
	})
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	_, dec, err := DecodeFrame(frame)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	v, err := dec.U8()
	if err != nil || v != 0x2A {
		t.Fatalf("known field = %v, %v", v, err)
	}
	if got := dec.Remaining(); got != 5 {
		t.Fatalf("remaining = %d, want 5", got)
	}
	dec.SkipRemaining()
	if err := dec.ExpectEOF(); err != nil {
		t.Fatalf("skip remaining: %v", err)
	}
}

func TestHeaderSeqTickFullRange(t *testing.T) {
	for _, v := range []uint32{0, 1, 0x7FFFFFFF, 0x80000000, math.MaxUint32} {
		h := Header{Opcode: 9, MsgVersion: 1, Seq: v, Tick: math.MaxUint32 - v}
		frame, err := EncodeFrame(h, nil)
		if err != nil {
			t.Fatalf("EncodeFrame: %v", err)
		}
		got, _, err := DecodeFrame(frame)
		if err != nil {
			t.Fatalf("DecodeFrame: %v", err)
		}
		if got != h {
			t.Fatalf("header = %+v, want %+v", got, h)
		}
	}
}

func TestFrameEncodePayloadError(t *testing.T) {
	sentinel := errors.New("boom")
	if _, err := EncodeFrame(Header{}, func(*Encoder) error {
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("got %v, want sentinel", err)
	}
	// Sticky encoder errors also surface.
	if _, err := EncodeFrame(Header{}, func(e *Encoder) error {
		e.Angle(4096)
		return nil
	}); !errors.Is(err, ErrAngleOutOfRange) {
		t.Fatalf("got %v, want ErrAngleOutOfRange", err)
	}
}
