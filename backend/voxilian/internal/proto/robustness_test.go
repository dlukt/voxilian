package proto

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

// Deterministic malformed/truncated/oversized corpus (M2-T5). Fuzzing
// explores randomly; these tests pin the review-sensitive boundaries.
// Malformed inputs live here as byte slices — never as .hex files, so
// the Godot golden directory stays valid-vectors-only.

// encodeCanonicalPayload encodes a golden case payload for corpus reuse.
func encodeCanonicalPayload(t *testing.T, c goldenCase) []byte {
	t.Helper()
	e := NewEncoder()
	c.Encode(e)
	p, err := e.Bytes()
	if err != nil {
		t.Fatalf("%s: encode: %v", c.Filename, err)
	}
	return p
}

func TestUnknownOpcodesPreserved(t *testing.T) {
	for _, op := range []uint16{0, 127, 199, 221, math.MaxUint16} {
		h := Header{Opcode: op, MsgVersion: 3, Seq: 0xA1B2C3D4, Tick: math.MaxUint32}
		payload := []byte{0x01, 0x02, 0x03}
		frame, err := EncodeFrame(h, func(e *Encoder) error {
			e.WriteBytes(payload)
			return e.Err()
		})
		if err != nil {
			t.Fatalf("opcode %d: EncodeFrame: %v", op, err)
		}
		got, dec, err := DecodeFrame(frame)
		if err != nil {
			t.Fatalf("opcode %d: DecodeFrame: %v", op, err)
		}
		if got != h {
			t.Fatalf("opcode %d: header = %+v, want %+v", op, got, h)
		}
		rest, err := dec.ReadBytes(3)
		if err != nil || string(rest) != string(payload) {
			t.Fatalf("opcode %d: payload = % x, %v", op, rest, err)
		}
	}
	// No unknown-opcode sentinel exists in this package.
	if ErrFrameTooLarge == nil || ErrTruncated == nil {
		t.Fatalf("sentinels must exist")
	}
}

func TestAllDecodersTrailingTolerance(t *testing.T) {
	for _, c := range goldenCases {
		t.Run(c.Filename, func(t *testing.T) {
			payload := encodeCanonicalPayload(t, c)
			payload = append(payload, 0xDE, 0xAD, 0xBE, 0xEF)
			got, err := c.Decode(NewDecoder(payload))
			if err != nil {
				t.Fatalf("trailing bytes: %v", err)
			}
			if !reflect.DeepEqual(got, c.Want) {
				t.Fatalf("got %#v, want %#v", got, c.Want)
			}
		})
	}
}

func TestAllDecodersFutureMsgVersion(t *testing.T) {
	for _, c := range goldenCases {
		t.Run(c.Filename, func(t *testing.T) {
			payload := encodeCanonicalPayload(t, c)
			payload = append(payload, 0xDE, 0xAD, 0xBE, 0xEF)
			h := c.Header
			h.MsgVersion = 0xFFFF // unknown future version; no production meaning
			frame, err := EncodeFrame(h, func(e *Encoder) error {
				e.WriteBytes(payload)
				return e.Err()
			})
			if err != nil {
				t.Fatalf("EncodeFrame: %v", err)
			}
			gotHeader, dec, err := DecodeFrame(frame)
			if err != nil {
				t.Fatalf("DecodeFrame: %v", err)
			}
			if gotHeader.MsgVersion != 0xFFFF {
				t.Fatalf("msg_version = %d", gotHeader.MsgVersion)
			}
			got, err := c.Decode(dec)
			if err != nil {
				t.Fatalf("payload decode: %v", err)
			}
			if !reflect.DeepEqual(got, c.Want) {
				t.Fatalf("got %#v, want %#v", got, c.Want)
			}
		})
	}
}

func TestFramedEntriesFutureSuffix(t *testing.T) {
	t.Run("CharacterList", func(t *testing.T) {
		raw := encodeTestPayload(t, func(e *Encoder) {
			e.Count(2, MaxArrayCount)
			e.Entry(func(sub *Encoder) error {
				sub.U8(0)
				sub.String("Ada", MaxStringBytes)
				sub.U16(20)
				sub.U32(0xDEADBEEF)
				return nil
			})
			e.Entry(func(sub *Encoder) error {
				sub.U8(1)
				sub.String("Bo", MaxStringBytes)
				sub.U16(30)
				return nil
			})
		})
		got, err := DecodeCharacterList(NewDecoder(raw))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(got.Characters) != 2 || got.Characters[1].CharName != "Bo" {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("CellSnapshot", func(t *testing.T) {
		raw := encodeTestPayload(t, func(e *Encoder) {
			e.Cell(Cell{X: 1, Z: 2})
			e.Count(2, MaxArrayCount)
			e.Entry(func(sub *Encoder) error {
				EntityEntry{Entity: 7, Angle: 100}.encode(sub)
				sub.U32(0xDEADBEEF)
				return nil
			})
			e.Entry(func(sub *Encoder) error {
				EntityEntry{Entity: 8, Angle: 200}.encode(sub)
				return nil
			})
		})
		got, err := DecodeCellSnapshot(NewDecoder(raw))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(got.Entities) != 2 || got.Entities[0].Entity != 7 || got.Entities[1].Entity != 8 {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("StatGroup", func(t *testing.T) {
		raw := encodeTestPayload(t, func(e *Encoder) {
			e.U32(5)
			e.Count(2, MaxArrayCount)
			e.Entry(func(sub *Encoder) error {
				StatEntry{StatID: 1, Value: 1}.encode(sub)
				sub.U16(0xBEEF)
				return nil
			})
			e.Entry(func(sub *Encoder) error {
				StatEntry{StatID: 2, Value: 2}.encode(sub)
				return nil
			})
		})
		got, err := DecodeStatGroup(NewDecoder(raw))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(got.Stats) != 2 || got.Stats[1].Value != 2 {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("InventoryDelta", func(t *testing.T) {
		raw := encodeTestPayload(t, func(e *Encoder) {
			e.Count(2, MaxArrayCount)
			e.Entry(func(sub *Encoder) error {
				InventoryEntry{Item: 1, Slot: "a"}.encode(sub)
				sub.U32(0xDEADBEEF)
				return nil
			})
			e.Entry(func(sub *Encoder) error {
				InventoryEntry{Item: 2, Slot: "b"}.encode(sub)
				return nil
			})
		})
		got, err := DecodeInventoryDelta(NewDecoder(raw))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(got.Items) != 2 || got.Items[1].Item != 2 {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("OfferUpdate", func(t *testing.T) {
		raw := encodeTestPayload(t, func(e *Encoder) {
			e.U32(9)
			e.U8(1)
			e.Count(2, MaxArrayCount)
			e.Entry(func(sub *Encoder) error {
				OfferItem{Item: 1, Qty: 1}.encode(sub)
				sub.U16(0xBEEF)
				return nil
			})
			e.Entry(func(sub *Encoder) error {
				OfferItem{Item: 2, Qty: 2}.encode(sub)
				return nil
			})
		})
		got, err := DecodeOfferUpdate(NewDecoder(raw))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(got.Items) != 2 || got.Items[1].Item != 2 {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("ShopList", func(t *testing.T) {
		raw := encodeTestPayload(t, func(e *Encoder) {
			e.U32(3)
			e.Count(2, MaxArrayCount)
			e.Entry(func(sub *Encoder) error {
				ShopListingEntry{Listing: 1, Price: 5}.encode(sub)
				sub.U32(0xDEADBEEF)
				return nil
			})
			e.Entry(func(sub *Encoder) error {
				ShopListingEntry{Listing: 2, Price: 6}.encode(sub)
				return nil
			})
		})
		got, err := DecodeShopList(NewDecoder(raw))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(got.Listings) != 2 || got.Listings[1].Price != 6 {
			t.Fatalf("got %+v", got)
		}
	})
}

func TestStrictPrefixTruncation(t *testing.T) {
	empty := map[string]bool{}
	for _, c := range goldenCases {
		t.Run(c.Filename, func(t *testing.T) {
			payload := encodeCanonicalPayload(t, c)
			if len(payload) == 0 {
				empty[c.Filename] = true
				return // structurally empty: zero bytes legitimately succeed
			}
			for i := 0; i < len(payload); i++ {
				_, err := c.Decode(NewDecoder(payload[:i]))
				if err == nil {
					t.Fatalf("prefix len %d of %d decoded successfully", i, len(payload))
				}
				if !errors.Is(err, ErrTruncated) {
					t.Fatalf("prefix len %d: got %v, want ErrTruncated", i, err)
				}
			}
		})
	}
	// The empty-message exception list is exact.
	wantEmpty := map[string]bool{
		"112_accept.hex": true, "113_cancel.hex": true,
		"119_safety_toggle.hex": true, "120_respawn_ack.hex": true,
		"121_character_list_request.hex": true, "126_leave_world.hex": true,
		"201_reauth_ok.hex": true, "219_world_ready.hex": true,
	}
	if !reflect.DeepEqual(empty, wantEmpty) {
		t.Fatalf("empty payloads = %v, want %v", empty, wantEmpty)
	}
	// Empty messages also succeed with arbitrary future bytes.
	for name := range wantEmpty {
		var dec func(*Decoder) (any, error)
		for _, c := range goldenCases {
			if c.Filename == name {
				dec = c.Decode
			}
		}
		if _, err := dec(NewDecoder([]byte{0x01, 0x02, 0x03})); err != nil {
			t.Fatalf("%s with future bytes: %v", name, err)
		}
	}
}

func TestOversizedCorpus(t *testing.T) {
	t.Run("Frame", func(t *testing.T) {
		if _, _, err := DecodeFrame(make([]byte, MaxFrameSize)); err != nil {
			t.Fatalf("65536: %v", err)
		}
		if _, _, err := DecodeFrame(make([]byte, MaxFrameSize+1)); !errors.Is(err, ErrFrameTooLarge) {
			t.Fatalf("65537: got %v", err)
		}
	})
	t.Run("AccessToken", func(t *testing.T) {
		big := strings.Repeat("t", MaxAccessTokenBytes+1)
		hello := encodeTestPayload(t, func(e *Encoder) {
			e.U32(1)
			e.U16(1)
			e.U16(uint16(MaxAccessTokenBytes + 1))
			e.WriteBytes([]byte(big))
		})
		if _, err := DecodeHello(NewDecoder(hello)); !errors.Is(err, ErrStringTooLong) {
			t.Fatalf("hello 8193: got %v", err)
		}
		reauth := encodeTestPayload(t, func(e *Encoder) {
			e.U16(uint16(MaxAccessTokenBytes + 1))
			e.WriteBytes([]byte(big))
		})
		if _, err := DecodeReauth(NewDecoder(reauth)); !errors.Is(err, ErrStringTooLong) {
			t.Fatalf("reauth 8193: got %v", err)
		}
	})
	t.Run("Chat", func(t *testing.T) {
		big := strings.Repeat("c", MaxChatBytes+1)
		say := encodeTestPayload(t, func(e *Encoder) {
			e.U8(0)
			e.U16(uint16(MaxChatBytes + 1))
			e.WriteBytes([]byte(big))
		})
		if _, err := DecodeSay(NewDecoder(say)); !errors.Is(err, ErrStringTooLong) {
			t.Fatalf("say 513: got %v", err)
		}
		said := encodeTestPayload(t, func(e *Encoder) {
			e.U32(1)
			e.U8(0)
			e.U16(uint16(MaxChatBytes + 1))
			e.WriteBytes([]byte(big))
		})
		if _, err := DecodeSaid(NewDecoder(said)); !errors.Is(err, ErrStringTooLong) {
			t.Fatalf("said 513: got %v", err)
		}
	})
	t.Run("GeneralString", func(t *testing.T) {
		big := strings.Repeat("g", MaxStringBytes+1)
		create := encodeTestPayload(t, func(e *Encoder) {
			e.U8(0)
			e.U16(uint16(MaxStringBytes + 1))
			e.WriteBytes([]byte(big))
		})
		if _, err := DecodeCharacterCreate(NewDecoder(create)); !errors.Is(err, ErrStringTooLong) {
			t.Fatalf("create name 1025: got %v", err)
		}
		errMsg := encodeTestPayload(t, func(e *Encoder) {
			e.U16(1)
			e.U16(uint16(MaxStringBytes + 1))
			e.WriteBytes([]byte(big))
		})
		if _, err := DecodeErrorMessage(NewDecoder(errMsg)); !errors.Is(err, ErrStringTooLong) {
			t.Fatalf("error 1025: got %v", err)
		}
		se := NewEncoder()
		InventoryDelta{Items: []InventoryEntry{{Item: 1, Slot: big}}}.Encode(se)
		if _, err := se.Bytes(); !errors.Is(err, ErrStringTooLong) {
			t.Fatalf("slot 1025 encode: got %v", err)
		}
		slotRaw := encodeTestPayload(t, func(e *Encoder) {
			e.Count(1, MaxArrayCount)
			e.U16(uint16(19 + MaxStringBytes + 1))
			e.U32(1)
			e.U16(2)
			e.U16(3)
			e.I32(4)
			e.U8(0)
			e.U32(0)
			e.U16(uint16(MaxStringBytes + 1))
			e.WriteBytes([]byte(big))
		})
		if _, err := DecodeInventoryDelta(NewDecoder(slotRaw)); !errors.Is(err, ErrStringTooLong) {
			t.Fatalf("slot 1025 decode: got %v", err)
		}
	})
	t.Run("Array", func(t *testing.T) {
		spells := encodeTestPayload(t, func(e *Encoder) {
			e.U8(0)
			e.String("A", MaxStringBytes)
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
			e.U16(1025)
		})
		if _, err := DecodeCharacterCreate(NewDecoder(spells)); !errors.Is(err, ErrArrayTooLong) {
			t.Fatalf("spells 1025: got %v", err)
		}
		snap := encodeTestPayload(t, func(e *Encoder) {
			e.Cell(Cell{})
			e.U16(1025)
		})
		if _, err := DecodeCellSnapshot(NewDecoder(snap)); !errors.Is(err, ErrArrayTooLong) {
			t.Fatalf("snapshot 1025: got %v", err)
		}
		inv := encodeTestPayload(t, func(e *Encoder) { e.U16(1025) })
		if _, err := DecodeInventoryDelta(NewDecoder(inv)); !errors.Is(err, ErrArrayTooLong) {
			t.Fatalf("inventory 1025: got %v", err)
		}
	})
	t.Run("Chunk", func(t *testing.T) {
		raw := encodeTestPayload(t, func(e *Encoder) {
			e.Cell(Cell{})
			e.U32(1)
			e.U16(0)
			e.U16(1)
			e.U16(61441)
		})
		if _, err := DecodeChunkFragment(NewDecoder(raw)); !errors.Is(err, ErrChunkFragmentTooLarge) {
			t.Fatalf("chunk 61441: got %v", err)
		}
	})
}

func TestMalformedEntryCorpus(t *testing.T) {
	oneBytePrefix := func(decode func(*Decoder) (any, error), build func(e *Encoder)) func(*testing.T) {
		return func(t *testing.T) {
			t.Helper()
			raw := encodeTestPayload(t, build)
			if _, err := decode(NewDecoder(raw)); !errors.Is(err, ErrTruncated) {
				t.Fatalf("got %v, want ErrTruncated", err)
			}
		}
	}
	t.Run("SnapshotShortPrefix", oneBytePrefix(
		func(d *Decoder) (any, error) { return DecodeCellSnapshot(d) },
		func(e *Encoder) {
			e.Cell(Cell{})
			e.Count(1, MaxArrayCount)
			e.U8(0x05) // one byte of the u16 entryLen
		}))
	t.Run("StatGroupOverlong", oneBytePrefix(
		func(d *Decoder) (any, error) { return DecodeStatGroup(d) },
		func(e *Encoder) {
			e.U32(1)
			e.Count(1, MaxArrayCount)
			e.U16(100)
			e.WriteBytes(make([]byte, 3))
		}))
	t.Run("InventoryOverlong", oneBytePrefix(
		func(d *Decoder) (any, error) { return DecodeInventoryDelta(d) },
		func(e *Encoder) {
			e.Count(1, MaxArrayCount)
			e.U16(60)
			e.WriteBytes(make([]byte, 4))
		}))
	t.Run("SnapshotEmptyEntry", func(t *testing.T) {
		// entryLen 0 is framing-valid but the known entry needs fields.
		raw := encodeTestPayload(t, func(e *Encoder) {
			e.Cell(Cell{})
			e.Count(1, MaxArrayCount)
			e.U16(0)
		})
		if _, err := DecodeCellSnapshot(NewDecoder(raw)); !errors.Is(err, ErrTruncated) {
			t.Fatalf("got %v, want ErrTruncated", err)
		}
	})
	t.Run("ChildCannotEscapeOuter", func(t *testing.T) {
		// A short first entry must not let its child read into entry 2.
		raw := encodeTestPayload(t, func(e *Encoder) {
			e.U32(1)
			e.Count(2, MaxArrayCount)
			e.U16(2) // only 2 bytes for a 17-byte stat
			e.WriteBytes([]byte{0x01, 0x02})
			e.Entry(func(sub *Encoder) error {
				StatEntry{StatID: 9, Value: 9}.encode(sub)
				return nil
			})
		})
		if _, err := DecodeStatGroup(NewDecoder(raw)); !errors.Is(err, ErrTruncated) {
			t.Fatalf("got %v, want ErrTruncated", err)
		}
	})
}

func TestMalformedStringCorpus(t *testing.T) {
	t.Run("DeclaredBeyondRemaining", func(t *testing.T) {
		raw := encodeTestPayload(t, func(e *Encoder) {
			e.U8(2)
			e.U16(40)
			e.WriteBytes([]byte("short"))
		})
		if _, err := DecodeSay(NewDecoder(raw)); !errors.Is(err, ErrTruncated) {
			t.Fatalf("got %v, want ErrTruncated", err)
		}
	})
	t.Run("InvalidUTF8", func(t *testing.T) {
		raw := encodeTestPayload(t, func(e *Encoder) {
			e.U8(2)
			e.U16(2)
			e.WriteBytes([]byte{0xFF, 0xFE})
		})
		if _, err := DecodeSay(NewDecoder(raw)); !errors.Is(err, ErrInvalidUTF8) {
			t.Fatalf("got %v, want ErrInvalidUTF8", err)
		}
	})
}

func TestMalformedAngleCorpus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		build  func(e *Encoder)
		decode func(*Decoder) error
	}{
		{"Move", func(e *Encoder) {
			e.U32(1)
			e.U8(0)
			e.U8(0)
			e.U16(4096)
		}, func(d *Decoder) error { _, err := DecodeMove(d); return err }},
		{"EntityMove", func(e *Encoder) {
			e.U32(1)
			e.Position(Position{})
			e.U16(4096)
		}, func(d *Decoder) error { _, err := DecodeEntityMove(d); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := encodeTestPayload(t, tc.build)
			if err := tc.decode(NewDecoder(raw)); !errors.Is(err, ErrAngleOutOfRange) {
				t.Fatalf("got %v, want ErrAngleOutOfRange", err)
			}
		})
	}
}

func TestUseStableIDCorpus(t *testing.T) {
	e := NewEncoder()
	Use{Kind: UseKindSkill, ID: 65536}.Encode(e)
	if _, err := e.Bytes(); !errors.Is(err, ErrStableIDOutOfRange) {
		t.Fatalf("encode: got %v", err)
	}
	raw := encodeTestPayload(t, func(e *Encoder) {
		e.U8(UseKindSkill)
		e.U32(65536)
	})
	if _, err := DecodeUse(NewDecoder(raw)); !errors.Is(err, ErrStableIDOutOfRange) {
		t.Fatalf("decode: got %v", err)
	}
	// Unknown kinds stay accepted (no enum whitelist).
	unknown := encodeTestPayload(t, Use{Kind: 200, ID: math.MaxUint32}.Encode)
	if _, err := DecodeUse(NewDecoder(unknown)); err != nil {
		t.Fatalf("unknown kind: %v", err)
	}
}
