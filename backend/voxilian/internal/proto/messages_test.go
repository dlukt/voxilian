package proto

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

// encodeTestPayload runs fn against a fresh Encoder and returns the payload bytes.
func encodeTestPayload(t *testing.T, fn func(e *Encoder)) []byte {
	t.Helper()
	e := NewEncoder()
	fn(e)
	b, err := e.Bytes()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return b
}

// frameRoundTrip encodes msg through the full envelope and decodes it
// back, asserting header survival and returning the payload decoder.
func frameRoundTrip(t *testing.T, h Header, encode func(e *Encoder)) *Decoder {
	t.Helper()
	frame, err := EncodeFrame(h, func(e *Encoder) error {
		encode(e)
		return e.Err()
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
	return dec
}

func TestOpcodesExact(t *testing.T) {
	want := map[string]uint16{
		"Hello": 100, "Reauth": 101,
		"CharacterList": 121, "CharacterCreate": 122,
		"CharacterDelete": 123, "EnterWorld": 124,
		"Ack": 125, "LeaveWorld": 126,
		"Welcome": 200, "ReauthOK": 201, "Error": 202,
		"CharacterListResult": 216, "CharacterOp": 217, "WorldReady": 219,
	}
	got := map[string]uint16{
		"Hello": OpcodeHello, "Reauth": OpcodeReauth,
		"CharacterList": OpcodeCharacterList, "CharacterCreate": OpcodeCharacterCreate,
		"CharacterDelete": OpcodeCharacterDelete, "EnterWorld": OpcodeEnterWorld,
		"Ack": OpcodeAck, "LeaveWorld": OpcodeLeaveWorld,
		"Welcome": OpcodeWelcome, "ReauthOK": OpcodeReauthOK, "Error": OpcodeError,
		"CharacterListResult": OpcodeCharacterListResult, "CharacterOp": OpcodeCharacterOp,
		"WorldReady": OpcodeWorldReady,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("opcodes = %v, want %v", got, want)
	}
}

func sampleCharacterCreate() CharacterCreate {
	return CharacterCreate{
		Slot:   1,
		Name:   "Mira",
		Gender: 2,
		Face:   CharacterFace{HairStyle: 3, HairColor: 4, SkinTone: 5, Parts: [5]uint8{1, 2, 3, 4, 5}},
		Stats:  [6]uint8{10, 11, 12, 13, 14, 15},
		Spells: []uint16{100, 200, 300},
		Skills: []uint16{7, 8},
	}
}

func TestMessageRoundTrips(t *testing.T) {
	t.Run("Hello", func(t *testing.T) {
		want := Hello{ClientVersion: 0x01020304, ProtoVersion: 0x0506, AccessToken: "token-abc"}
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodeHello(NewDecoder(b))
		if err != nil || got != want {
			t.Fatalf("Hello = %+v, %v", got, err)
		}
	})
	t.Run("Reauth", func(t *testing.T) {
		want := Reauth{AccessToken: "refreshed-token"}
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodeReauth(NewDecoder(b))
		if err != nil || got != want {
			t.Fatalf("Reauth = %+v, %v", got, err)
		}
	})
	t.Run("CharacterListRequest", func(t *testing.T) {
		b := encodeTestPayload(t, CharacterListRequest{}.Encode)
		if len(b) != 0 {
			t.Fatalf("empty message payload len = %d", len(b))
		}
		if _, err := DecodeCharacterListRequest(NewDecoder(b)); err != nil {
			t.Fatalf("decode: %v", err)
		}
	})
	t.Run("CharacterCreate", func(t *testing.T) {
		want := sampleCharacterCreate()
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodeCharacterCreate(NewDecoder(b))
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("CharacterCreate = %+v, %v", got, err)
		}
	})
	t.Run("CharacterCreateSlot255", func(t *testing.T) {
		// Wire validation only: Slot=255 round-trips; semantic
		// rejection belongs to application validation, not the codec.
		want := sampleCharacterCreate()
		want.Slot = 255
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodeCharacterCreate(NewDecoder(b))
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("CharacterCreate slot 255 = %+v, %v", got, err)
		}
	})
	t.Run("CharacterDelete", func(t *testing.T) {
		want := CharacterDelete{Slot: 1}
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodeCharacterDelete(NewDecoder(b))
		if err != nil || got != want {
			t.Fatalf("CharacterDelete = %+v, %v", got, err)
		}
	})
	t.Run("EnterWorld", func(t *testing.T) {
		want := EnterWorld{Slot: 0}
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodeEnterWorld(NewDecoder(b))
		if err != nil || got != want {
			t.Fatalf("EnterWorld = %+v, %v", got, err)
		}
	})
	t.Run("Ack", func(t *testing.T) {
		for _, seq := range []uint32{0, 12345, math.MaxUint32} {
			want := Ack{AckSeq: seq}
			b := encodeTestPayload(t, want.Encode)
			got, err := DecodeAck(NewDecoder(b))
			if err != nil || got != want {
				t.Fatalf("Ack(%d) = %+v, %v", seq, got, err)
			}
		}
	})
	t.Run("LeaveWorld", func(t *testing.T) {
		b := encodeTestPayload(t, LeaveWorld{}.Encode)
		if len(b) != 0 {
			t.Fatalf("empty message payload len = %d", len(b))
		}
		if _, err := DecodeLeaveWorld(NewDecoder(b)); err != nil {
			t.Fatalf("decode: %v", err)
		}
	})
	t.Run("Welcome", func(t *testing.T) {
		want := Welcome{
			ServerTimeMs: 0x0102030405060708,
			Chunk:        16,
			AOIRadius:    96,
			TickRates:    []uint16{20, 10},
			World:        WorldInfo{Mode: 1, Seed: 0x1122334455667788, Version: 0xAABBCCDD},
		}
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodeWelcome(NewDecoder(b))
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("Welcome = %+v, %v", got, err)
		}
	})
	t.Run("ReauthOK", func(t *testing.T) {
		b := encodeTestPayload(t, ReauthOK{}.Encode)
		if len(b) != 0 {
			t.Fatalf("empty message payload len = %d", len(b))
		}
		if _, err := DecodeReauthOK(NewDecoder(b)); err != nil {
			t.Fatalf("decode: %v", err)
		}
	})
	t.Run("Error", func(t *testing.T) {
		want := ErrorMessage{Code: 0x1234, Message: "kicked by admin: Grüße ✓"}
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodeErrorMessage(NewDecoder(b))
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("Error = %+v, %v", got, err)
		}
	})
	t.Run("CharacterList", func(t *testing.T) {
		want := CharacterList{Characters: []CharacterSummary{
			{Slot: 0, CharName: "Mira", Level: 20},
			{Slot: 1, CharName: "Jala-9", Level: 0},
		}}
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodeCharacterList(NewDecoder(b))
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("CharacterList = %+v, %v", got, err)
		}
	})
	t.Run("CharacterOp", func(t *testing.T) {
		// Raw wire values; no semantic meaning asserted.
		want := CharacterOp{Op: 7, OK: 1}
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodeCharacterOp(NewDecoder(b))
		if err != nil || got != want {
			t.Fatalf("CharacterOp = %+v, %v", got, err)
		}
	})
	t.Run("WorldReady", func(t *testing.T) {
		b := encodeTestPayload(t, WorldReady{}.Encode)
		if len(b) != 0 {
			t.Fatalf("empty message payload len = %d", len(b))
		}
		if _, err := DecodeWorldReady(NewDecoder(b)); err != nil {
			t.Fatalf("decode: %v", err)
		}
	})
}

func TestFrameIntegration(t *testing.T) {
	t.Run("Hello", func(t *testing.T) {
		h := Header{Opcode: OpcodeHello, MsgVersion: 1, Seq: 7, Tick: 99}
		want := Hello{ClientVersion: 42, ProtoVersion: 3, AccessToken: "jwt.header.payload"}
		dec := frameRoundTrip(t, h, want.Encode)
		got, err := DecodeHello(dec)
		if err != nil || got != want {
			t.Fatalf("Hello = %+v, %v", got, err)
		}
	})
	t.Run("Welcome", func(t *testing.T) {
		h := Header{Opcode: OpcodeWelcome, MsgVersion: 2, Seq: math.MaxUint32, Tick: 1}
		want := Welcome{
			ServerTimeMs: 987654321,
			Chunk:        2,
			AOIRadius:    128,
			TickRates:    []uint16{20},
			World:        WorldInfo{Mode: 0, Seed: 12345, Version: 7},
		}
		dec := frameRoundTrip(t, h, want.Encode)
		got, err := DecodeWelcome(dec)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("Welcome = %+v, %v", got, err)
		}
	})
	t.Run("CharacterCreate", func(t *testing.T) {
		h := Header{Opcode: OpcodeCharacterCreate, MsgVersion: 1, Seq: 5, Tick: 6}
		want := sampleCharacterCreate()
		dec := frameRoundTrip(t, h, want.Encode)
		got, err := DecodeCharacterCreate(dec)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("CharacterCreate = %+v, %v", got, err)
		}
	})
	t.Run("CharacterList", func(t *testing.T) {
		h := Header{Opcode: OpcodeCharacterListResult, MsgVersion: 1, Seq: 8, Tick: 9}
		want := CharacterList{Characters: []CharacterSummary{{Slot: 0, CharName: "Al", Level: 1}}}
		dec := frameRoundTrip(t, h, want.Encode)
		got, err := DecodeCharacterList(dec)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("CharacterList = %+v, %v", got, err)
		}
	})
}

func TestExactWireVectors(t *testing.T) {
	t.Run("HelloPrefix", func(t *testing.T) {
		m := Hello{ClientVersion: 0x01020304, ProtoVersion: 0x0506, AccessToken: "A"}
		b := encodeTestPayload(t, m.Encode)
		want := []byte{
			0x04, 0x03, 0x02, 0x01, // clientVersion LE
			0x06, 0x05, // protoVersion LE
			0x01, 0x00, 0x41, // string "A"
		}
		if string(b) != string(want) {
			t.Fatalf("hello wire = % x, want % x", b, want)
		}
	})
	t.Run("Ack", func(t *testing.T) {
		b := encodeTestPayload(t, Ack{AckSeq: 0x01020304}.Encode)
		want := []byte{0x04, 0x03, 0x02, 0x01}
		if string(b) != string(want) {
			t.Fatalf("ack wire = % x, want % x", b, want)
		}
	})
	t.Run("CharacterDelete", func(t *testing.T) {
		b := encodeTestPayload(t, CharacterDelete{Slot: 1}.Encode)
		if string(b) != string([]byte{0x01}) {
			t.Fatalf("delete wire = % x", b)
		}
	})
	t.Run("CharacterListEntryLen", func(t *testing.T) {
		m := CharacterList{Characters: []CharacterSummary{{Slot: 0, CharName: "A", Level: 0x0201}}}
		b := encodeTestPayload(t, m.Encode)
		// count(2) + entryLen + {slot u8, name(1+2), level u16}.
		// entryLen = 1 + (2+1) + 2 = 6.
		want := []byte{
			0x01, 0x00, // count
			0x06, 0x00, // entryLen
			0x00,             // slot
			0x01, 0x00, 0x41, // charName "A"
			0x01, 0x02, // level LE
		}
		if string(b) != string(want) {
			t.Fatalf("character_list wire = % x, want % x", b, want)
		}
	})
	t.Run("WelcomeOrdering", func(t *testing.T) {
		m := Welcome{
			ServerTimeMs: 0x0807060504030201,
			Chunk:        16,
			AOIRadius:    96,
			TickRates:    []uint16{20, 10},
			World:        WorldInfo{Mode: 1, Seed: 0x0807060504030201, Version: 0x04030201},
		}
		b := encodeTestPayload(t, m.Encode)
		want := []byte{
			0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, // serverTimeMs LE
			0x10,       // chunk
			0x60, 0x00, // aoiRadius 96 LE
			0x02,                   // tickRates count u8
			0x14, 0x00, 0x0A, 0x00, // [20, 10] LE
			0x01,                                           // world mode
			0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, // world seed LE
			0x01, 0x02, 0x03, 0x04, // world version LE
		}
		if string(b) != string(want) {
			t.Fatalf("welcome wire = % x, want % x", b, want)
		}
		// Decode the manual vector back to prove the layout reads as written.
		got, err := DecodeWelcome(NewDecoder(want))
		if err != nil || !reflect.DeepEqual(got, m) {
			t.Fatalf("manual welcome decode = %+v, %v", got, err)
		}
	})
}

func TestTokenLimits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		encode func(e *Encoder, s string)
		decode func(d *Decoder) error
	}{
		{"Hello", func(e *Encoder, s string) {
			Hello{AccessToken: s}.Encode(e)
		}, func(d *Decoder) error {
			_, err := DecodeHello(d)
			return err
		}},
		{"Reauth", func(e *Encoder, s string) {
			Reauth{AccessToken: s}.Encode(e)
		}, func(d *Decoder) error {
			_, err := DecodeReauth(d)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok := strings.Repeat("t", MaxAccessTokenBytes)
			b := encodeTestPayload(t, func(e *Encoder) { tc.encode(e, ok) })
			if err := tc.decode(NewDecoder(b)); err != nil {
				t.Fatalf("8192-byte token should decode: %v", err)
			}
			e := NewEncoder()
			tc.encode(e, strings.Repeat("t", MaxAccessTokenBytes+1))
			if _, err := e.Bytes(); !errors.Is(err, ErrStringTooLong) {
				t.Fatalf("8193-byte token encode: got %v", err)
			}
			// Wire-level rejection of an over-cap declared length.
			raw := encodeTestPayload(t, func(e *Encoder) {
				if tc.name == "Hello" {
					e.U32(1)
					e.U16(1)
				}
				e.U16(uint16(MaxAccessTokenBytes + 1))
				e.WriteBytes([]byte(strings.Repeat("t", MaxAccessTokenBytes+1)))
			})
			if err := tc.decode(NewDecoder(raw)); !errors.Is(err, ErrStringTooLong) {
				t.Fatalf("over-cap wire token decode: got %v", err)
			}
		})
	}
}

func TestErrorMessageLimits(t *testing.T) {
	ok := strings.Repeat("e", MaxStringBytes)
	b := encodeTestPayload(t, ErrorMessage{Code: 9, Message: ok}.Encode)
	got, err := DecodeErrorMessage(NewDecoder(b))
	if err != nil || got.Message != ok {
		t.Fatalf("1024-byte message: %v", err)
	}
	e := NewEncoder()
	ErrorMessage{Code: 9, Message: strings.Repeat("e", MaxStringBytes+1)}.Encode(e)
	if _, err := e.Bytes(); !errors.Is(err, ErrStringTooLong) {
		t.Fatalf("1025-byte message encode: got %v", err)
	}
}

func TestSpellSkillArrayLimits(t *testing.T) {
	ids1024 := make([]uint16, 1024)
	for i := range ids1024 {
		ids1024[i] = uint16(i + 1)
	}
	m := sampleCharacterCreate()
	m.Spells = ids1024
	b := encodeTestPayload(t, m.Encode)
	got, err := DecodeCharacterCreate(NewDecoder(b))
	if err != nil || !reflect.DeepEqual(got.Spells, ids1024) {
		t.Fatalf("1024 spells: %v", err)
	}

	m.Spells = make([]uint16, 1025)
	e := NewEncoder()
	m.Encode(e)
	if _, err := e.Bytes(); !errors.Is(err, ErrArrayTooLong) {
		t.Fatalf("1025 spells encode: got %v", err)
	}

	// Representative wire-level rejection for the skills array.
	raw := encodeTestPayload(t, func(e *Encoder) {
		e.U8(0)
		e.String("Al", MaxStringBytes)
		e.U8(0)
		e.U8(0)
		e.U8(0)
		e.U8(0)
		for i := 0; i < 5; i++ {
			e.U8(0)
		}
		for i := 0; i < 6; i++ {
			e.U8(0)
		}
		e.U16(0)    // spells: empty
		e.U16(1025) // skills: over cap
	})
	if _, err := DecodeCharacterCreate(NewDecoder(raw)); !errors.Is(err, ErrArrayTooLong) {
		t.Fatalf("1025 skills decode: got %v", err)
	}
}

func TestTickRateBoundaries(t *testing.T) {
	rates255 := make([]uint16, 255)
	for i := range rates255 {
		rates255[i] = uint16(i)
	}
	m := Welcome{ServerTimeMs: 1, Chunk: 1, AOIRadius: 1, TickRates: rates255}
	b := encodeTestPayload(t, m.Encode)
	got, err := DecodeWelcome(NewDecoder(b))
	if err != nil || !reflect.DeepEqual(got.TickRates, rates255) {
		t.Fatalf("255 tick rates: %v", err)
	}

	e := NewEncoder()
	Welcome{TickRates: make([]uint16, 256)}.Encode(e)
	if _, err := e.Bytes(); !errors.Is(err, ErrArrayTooLong) {
		t.Fatalf("256 tick rates encode: got %v", err)
	}
}

func TestCharacterListEntryForwardCompatibility(t *testing.T) {
	// Entry 1 carries a future u32 after the known fields; entry 2 is
	// old-shaped. The T2 decoder must read both correctly.
	raw := encodeTestPayload(t, func(e *Encoder) {
		e.Count(2, MaxArrayCount)
		e.Entry(func(sub *Encoder) error {
			sub.U8(0)
			sub.String("Mira", MaxStringBytes)
			sub.U16(20)
			sub.U32(0xDEADBEEF) // unknown future field
			return nil
		})
		e.Entry(func(sub *Encoder) error {
			sub.U8(1)
			sub.String("Jala", MaxStringBytes)
			sub.U16(5)
			return nil
		})
	})
	got, err := DecodeCharacterList(NewDecoder(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := CharacterList{Characters: []CharacterSummary{
		{Slot: 0, CharName: "Mira", Level: 20},
		{Slot: 1, CharName: "Jala", Level: 5},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestMessageTrailingTolerance(t *testing.T) {
	t.Run("Hello", func(t *testing.T) {
		b := encodeTestPayload(t, Hello{ClientVersion: 1, ProtoVersion: 2, AccessToken: "tok"}.Encode)
		b = append(b, 0xDE, 0xAD, 0xBE, 0xEF)
		got, err := DecodeHello(NewDecoder(b))
		if err != nil {
			t.Fatalf("hello with trailing bytes: %v", err)
		}
		if got.ClientVersion != 1 || got.ProtoVersion != 2 || got.AccessToken != "tok" {
			t.Fatalf("hello = %+v", got)
		}
	})
	t.Run("WorldReady", func(t *testing.T) {
		b := encodeTestPayload(t, WorldReady{}.Encode)
		b = append(b, 0x01, 0x02, 0x03)
		if _, err := DecodeWorldReady(NewDecoder(b)); err != nil {
			t.Fatalf("world_ready with trailing bytes: %v", err)
		}
	})
	t.Run("Error", func(t *testing.T) {
		b := encodeTestPayload(t, ErrorMessage{Code: 3, Message: "x"}.Encode)
		b = append(b, 0xFF)
		got, err := DecodeErrorMessage(NewDecoder(b))
		if err != nil || got.Code != 3 || got.Message != "x" {
			t.Fatalf("error = %+v, %v", got, err)
		}
	})
	t.Run("EmptyMessages", func(t *testing.T) {
		futures := []byte{0x09, 0x08}
		if _, err := DecodeCharacterListRequest(NewDecoder(append([]byte{}, futures...))); err != nil {
			t.Fatalf("character_list request: %v", err)
		}
		if _, err := DecodeLeaveWorld(NewDecoder(append([]byte{}, futures...))); err != nil {
			t.Fatalf("leave_world: %v", err)
		}
		if _, err := DecodeReauthOK(NewDecoder(append([]byte{}, futures...))); err != nil {
			t.Fatalf("reauth_ok: %v", err)
		}
	})
}

func TestMessageTruncation(t *testing.T) {
	t.Run("HelloToken", func(t *testing.T) {
		b := encodeTestPayload(t, Hello{ClientVersion: 1, ProtoVersion: 1, AccessToken: "long-token"}.Encode)
		truncated := b[:len(b)-3]
		if _, err := DecodeHello(NewDecoder(truncated)); !errors.Is(err, ErrTruncated) {
			t.Fatalf("got %v, want ErrTruncated", err)
		}
	})
	t.Run("WelcomeTickRates", func(t *testing.T) {
		b := encodeTestPayload(t, func(e *Encoder) {
			Welcome{
				ServerTimeMs: 1, Chunk: 1, AOIRadius: 1,
				TickRates: []uint16{10, 20, 30},
				World:     WorldInfo{Mode: 1, Seed: 2, Version: 3},
			}.Encode(e)
		})
		// Cut inside the tick-rate values.
		truncated := b[:8+1+2+1+2*2]
		if _, err := DecodeWelcome(NewDecoder(truncated)); !errors.Is(err, ErrTruncated) {
			t.Fatalf("got %v, want ErrTruncated", err)
		}
	})
	t.Run("CreateFaceParts", func(t *testing.T) {
		b := encodeTestPayload(t, sampleCharacterCreate().Encode)
		// slot(1) + name(2+4) + gender(1) + face fixed 3 + 2 of 5 parts.
		truncated := b[:1+6+1+3+2]
		if _, err := DecodeCharacterCreate(NewDecoder(truncated)); !errors.Is(err, ErrTruncated) {
			t.Fatalf("got %v, want ErrTruncated", err)
		}
	})
	t.Run("CreateSpellsMissingIDs", func(t *testing.T) {
		raw := encodeTestPayload(t, func(e *Encoder) {
			e.U8(0)
			e.String("Al", MaxStringBytes)
			e.U8(0)
			e.U8(0)
			e.U8(0)
			e.U8(0)
			for i := 0; i < 5; i++ {
				e.U8(0)
			}
			for i := 0; i < 6; i++ {
				e.U8(0)
			}
			e.U16(3)  // declares 3 spell IDs...
			e.U16(11) // ...but provides only 1
		})
		if _, err := DecodeCharacterCreate(NewDecoder(raw)); !errors.Is(err, ErrTruncated) {
			t.Fatalf("got %v, want ErrTruncated", err)
		}
	})
	t.Run("CharacterListEntryOverlong", func(t *testing.T) {
		raw := encodeTestPayload(t, func(e *Encoder) {
			e.Count(1, MaxArrayCount)
			e.U16(20)                                          // entry claims 20 bytes...
			e.WriteBytes([]byte{0x00, 0x01, 0x02, 0x03, 0x04}) // ...only 5 follow
		})
		if _, err := DecodeCharacterList(NewDecoder(raw)); !errors.Is(err, ErrTruncated) {
			t.Fatalf("got %v, want ErrTruncated", err)
		}
	})
	t.Run("ErrorMessageOverlong", func(t *testing.T) {
		raw := encodeTestPayload(t, func(e *Encoder) {
			e.U16(1)
			e.U16(50)                     // declares 50 message bytes...
			e.WriteBytes([]byte("short")) // ...only 5 follow
		})
		if _, err := DecodeErrorMessage(NewDecoder(raw)); !errors.Is(err, ErrTruncated) {
			t.Fatalf("got %v, want ErrTruncated", err)
		}
	})
}

func TestCharacterListEmpty(t *testing.T) {
	m := CharacterList{}
	b := encodeTestPayload(t, m.Encode)
	if string(b) != string([]byte{0x00, 0x00}) {
		t.Fatalf("empty list wire = % x", b)
	}
	got, err := DecodeCharacterList(NewDecoder(b))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Characters) != 0 {
		t.Fatalf("got %+v", got)
	}
}
