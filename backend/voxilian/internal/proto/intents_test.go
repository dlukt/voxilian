package proto

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestIntentOpcodesExact(t *testing.T) {
	want := map[uint16]string{
		102: "Move", 103: "Attack", 104: "Cast", 105: "Use",
		106: "Get", 107: "Drop", 108: "Put", 109: "Give",
		110: "Offer", 111: "Counter", 112: "Accept", 113: "Cancel",
		114: "Buy", 115: "Rest", 116: "Eat", 117: "Say",
		118: "SayGroup", 119: "SafetyToggle", 120: "RespawnAck",
	}
	got := map[uint16]string{
		OpcodeMove: "Move", OpcodeAttack: "Attack", OpcodeCast: "Cast", OpcodeUse: "Use",
		OpcodeGet: "Get", OpcodeDrop: "Drop", OpcodePut: "Put", OpcodeGive: "Give",
		OpcodeOffer: "Offer", OpcodeCounter: "Counter", OpcodeAccept: "Accept", OpcodeCancel: "Cancel",
		OpcodeBuy: "Buy", OpcodeRest: "Rest", OpcodeEat: "Eat", OpcodeSay: "Say",
		OpcodeSayGroup: "SayGroup", OpcodeSafetyToggle: "SafetyToggle", OpcodeRespawnAck: "RespawnAck",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("intent opcodes = %v, want %v", got, want)
	}
	// 126 LeaveWorld (M2-T2) completes the intent catalog unchanged.
	if OpcodeLeaveWorld != 126 {
		t.Fatalf("OpcodeLeaveWorld = %d, want 126", OpcodeLeaveWorld)
	}
}

func TestIntentRoundTrips(t *testing.T) {
	t.Run("Move", func(t *testing.T) {
		want := Move{InputSeq: math.MaxUint32, HeldDirs: 0xA5, RunFlag: 1, Yaw: 2048}
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodeMove(NewDecoder(b))
		if err != nil || got != want {
			t.Fatalf("Move = %+v, %v", got, err)
		}
	})
	t.Run("Attack", func(t *testing.T) {
		want := Attack{Target: math.MaxUint32}
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodeAttack(NewDecoder(b))
		if err != nil || got != want {
			t.Fatalf("Attack = %+v, %v", got, err)
		}
	})
	t.Run("Cast", func(t *testing.T) {
		want := Cast{Spell: 0xFFFF, Target: 123456}
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodeCast(NewDecoder(b))
		if err != nil || got != want {
			t.Fatalf("Cast = %+v, %v", got, err)
		}
	})
	t.Run("Use", func(t *testing.T) {
		for _, want := range []Use{
			{Kind: 0, ID: 7},
			{Kind: 1, ID: math.MaxUint32},
			{Kind: 200, ID: math.MaxUint32},
		} {
			b := encodeTestPayload(t, want.Encode)
			got, err := DecodeUse(NewDecoder(b))
			if err != nil || got != want {
				t.Fatalf("Use(%+v) = %+v, %v", want, got, err)
			}
		}
	})
	t.Run("Get", func(t *testing.T) {
		want := Get{Entity: 11, Item: 22}
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodeGet(NewDecoder(b))
		if err != nil || got != want {
			t.Fatalf("Get = %+v, %v", got, err)
		}
	})
	t.Run("Drop", func(t *testing.T) {
		want := Drop{Item: math.MaxUint32}
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodeDrop(NewDecoder(b))
		if err != nil || got != want {
			t.Fatalf("Drop = %+v, %v", got, err)
		}
	})
	t.Run("Put", func(t *testing.T) {
		// Self-container is codec-valid; containment rules live elsewhere.
		want := Put{Item: 9, Container: 9}
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodePut(NewDecoder(b))
		if err != nil || got != want {
			t.Fatalf("Put = %+v, %v", got, err)
		}
	})
	t.Run("Give", func(t *testing.T) {
		// Qty 0 is codec-valid; gameplay policy decides.
		want := Give{Target: 1, Item: 2, Qty: 0}
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodeGive(NewDecoder(b))
		if err != nil || got != want {
			t.Fatalf("Give = %+v, %v", got, err)
		}
	})
	t.Run("Offer", func(t *testing.T) {
		for _, want := range []Offer{
			{Target: 5, Items: nil},
			{Target: math.MaxUint32, Items: []uint32{1, 2, math.MaxUint32}},
		} {
			b := encodeTestPayload(t, want.Encode)
			got, err := DecodeOffer(NewDecoder(b))
			if err != nil || got.Target != want.Target || len(got.Items) != len(want.Items) {
				t.Fatalf("Offer(%+v) = %+v, %v", want, got, err)
			}
			for i := range want.Items {
				if got.Items[i] != want.Items[i] {
					t.Fatalf("Offer(%+v) = %+v", want, got)
				}
			}
		}
	})
	t.Run("Counter", func(t *testing.T) {
		want := Counter{Items: []uint32{42}}
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodeCounter(NewDecoder(b))
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("Counter = %+v, %v", got, err)
		}
		empty := encodeTestPayload(t, Counter{}.Encode)
		gotEmpty, err := DecodeCounter(NewDecoder(empty))
		if err != nil || len(gotEmpty.Items) != 0 {
			t.Fatalf("Counter empty = %+v, %v", gotEmpty, err)
		}
	})
	t.Run("Accept", func(t *testing.T) {
		b := encodeTestPayload(t, Accept{}.Encode)
		if len(b) != 0 {
			t.Fatalf("payload len = %d", len(b))
		}
		if _, err := DecodeAccept(NewDecoder(b)); err != nil {
			t.Fatalf("decode: %v", err)
		}
	})
	t.Run("Cancel", func(t *testing.T) {
		b := encodeTestPayload(t, Cancel{}.Encode)
		if len(b) != 0 {
			t.Fatalf("payload len = %d", len(b))
		}
		if _, err := DecodeCancel(NewDecoder(b)); err != nil {
			t.Fatalf("decode: %v", err)
		}
	})
	t.Run("Buy", func(t *testing.T) {
		want := Buy{Vendor: math.MaxUint32, Listing: 0x0506, Qty: 0}
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodeBuy(NewDecoder(b))
		if err != nil || got != want {
			t.Fatalf("Buy = %+v, %v", got, err)
		}
	})
	t.Run("Rest", func(t *testing.T) {
		// Raw state preserved, not coerced to bool/0-1.
		want := Rest{State: 255}
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodeRest(NewDecoder(b))
		if err != nil || got != want {
			t.Fatalf("Rest = %+v, %v", got, err)
		}
	})
	t.Run("Eat", func(t *testing.T) {
		want := Eat{Item: 77}
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodeEat(NewDecoder(b))
		if err != nil || got != want {
			t.Fatalf("Eat = %+v, %v", got, err)
		}
	})
	t.Run("Say", func(t *testing.T) {
		want := Say{Channel: 255, Text: "hello ✓"}
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodeSay(NewDecoder(b))
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("Say = %+v, %v", got, err)
		}
	})
	t.Run("SayGroup", func(t *testing.T) {
		want := SayGroup{Text: "party: Grüße"}
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodeSayGroup(NewDecoder(b))
		if err != nil || got != want {
			t.Fatalf("SayGroup = %+v, %v", got, err)
		}
	})
	t.Run("SafetyToggle", func(t *testing.T) {
		b := encodeTestPayload(t, SafetyToggle{}.Encode)
		if len(b) != 0 {
			t.Fatalf("payload len = %d", len(b))
		}
		if _, err := DecodeSafetyToggle(NewDecoder(b)); err != nil {
			t.Fatalf("decode: %v", err)
		}
	})
	t.Run("RespawnAck", func(t *testing.T) {
		b := encodeTestPayload(t, RespawnAck{}.Encode)
		if len(b) != 0 {
			t.Fatalf("payload len = %d", len(b))
		}
		if _, err := DecodeRespawnAck(NewDecoder(b)); err != nil {
			t.Fatalf("decode: %v", err)
		}
	})
}

func TestIntentFrameIntegration(t *testing.T) {
	t.Run("Move", func(t *testing.T) {
		h := Header{Opcode: OpcodeMove, MsgVersion: 1, Seq: 100, Tick: 200}
		want := Move{InputSeq: 999, HeldDirs: 3, RunFlag: 0, Yaw: 4095}
		dec := frameRoundTrip(t, h, want.Encode)
		got, err := DecodeMove(dec)
		if err != nil || got != want {
			t.Fatalf("Move = %+v, %v", got, err)
		}
	})
	t.Run("Use", func(t *testing.T) {
		h := Header{Opcode: OpcodeUse, MsgVersion: 1, Seq: 101, Tick: 201}
		want := Use{Kind: 1, ID: 0x01020304}
		dec := frameRoundTrip(t, h, want.Encode)
		got, err := DecodeUse(dec)
		if err != nil || got != want {
			t.Fatalf("Use = %+v, %v", got, err)
		}
	})
	t.Run("Offer", func(t *testing.T) {
		h := Header{Opcode: OpcodeOffer, MsgVersion: 1, Seq: 102, Tick: 202}
		want := Offer{Target: 7, Items: []uint32{8, 9}}
		dec := frameRoundTrip(t, h, want.Encode)
		got, err := DecodeOffer(dec)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("Offer = %+v, %v", got, err)
		}
	})
	t.Run("Say", func(t *testing.T) {
		h := Header{Opcode: OpcodeSay, MsgVersion: 1, Seq: 103, Tick: 203}
		want := Say{Channel: 2, Text: "hi"}
		dec := frameRoundTrip(t, h, want.Encode)
		got, err := DecodeSay(dec)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("Say = %+v, %v", got, err)
		}
	})
}

func TestIntentExactWire(t *testing.T) {
	t.Run("Move", func(t *testing.T) {
		b := encodeTestPayload(t, Move{InputSeq: 0x01020304, HeldDirs: 0xA5, RunFlag: 1, Yaw: 0x0123}.Encode)
		want := []byte{
			0x04, 0x03, 0x02, 0x01, // inputSeq LE
			0xA5,       // heldDirs
			0x01,       // runFlag
			0x23, 0x01, // yaw LE
		}
		if string(b) != string(want) {
			t.Fatalf("move wire = % x, want % x", b, want)
		}
	})
	t.Run("UseKind1FixedU32", func(t *testing.T) {
		b := encodeTestPayload(t, Use{Kind: 1, ID: 0x01020304}.Encode)
		want := []byte{0x01, 0x04, 0x03, 0x02, 0x01}
		if string(b) != string(want) {
			t.Fatalf("use wire = % x, want % x", b, want)
		}
	})
	t.Run("UseKind0StillU32", func(t *testing.T) {
		// kind=0 uses the same 4-byte carrier: total 5 bytes.
		b := encodeTestPayload(t, Use{Kind: 0, ID: 0x0304}.Encode)
		want := []byte{0x00, 0x04, 0x03, 0x00, 0x00}
		if string(b) != string(want) {
			t.Fatalf("use kind0 wire = % x, want % x", b, want)
		}
	})
	t.Run("Buy", func(t *testing.T) {
		b := encodeTestPayload(t, Buy{Vendor: 0x01020304, Listing: 0x0506, Qty: 0x0708}.Encode)
		want := []byte{
			0x04, 0x03, 0x02, 0x01, // vendor u32 LE
			0x06, 0x05, // listing u16 LE
			0x08, 0x07, // qty u16 LE
		}
		if string(b) != string(want) {
			t.Fatalf("buy wire = % x, want % x", b, want)
		}
	})
	t.Run("Say", func(t *testing.T) {
		b := encodeTestPayload(t, Say{Channel: 3, Text: "A"}.Encode)
		want := []byte{0x03, 0x01, 0x00, 0x41}
		if string(b) != string(want) {
			t.Fatalf("say wire = % x, want % x", b, want)
		}
	})
}

func TestMoveYawRange(t *testing.T) {
	b := encodeTestPayload(t, Move{InputSeq: 1, Yaw: 4095}.Encode)
	got, err := DecodeMove(NewDecoder(b))
	if err != nil || got.Yaw != 4095 {
		t.Fatalf("yaw 4095 = %+v, %v", got, err)
	}
	e := NewEncoder()
	Move{InputSeq: 1, Yaw: 4096}.Encode(e)
	if _, err := e.Bytes(); !errors.Is(err, ErrAngleOutOfRange) {
		t.Fatalf("yaw 4096 encode: got %v", err)
	}
	raw := encodeTestPayload(t, func(e *Encoder) {
		e.U32(1)
		e.U8(0)
		e.U8(0)
		e.U16(4096)
	})
	if _, err := DecodeMove(NewDecoder(raw)); !errors.Is(err, ErrAngleOutOfRange) {
		t.Fatalf("yaw 4096 decode: got %v", err)
	}
}

func TestUseStableIDInvariant(t *testing.T) {
	t.Run("Kind0Boundary", func(t *testing.T) {
		b := encodeTestPayload(t, Use{Kind: 0, ID: 65535}.Encode)
		got, err := DecodeUse(NewDecoder(b))
		if err != nil || got != (Use{Kind: 0, ID: 65535}) {
			t.Fatalf("kind0/65535 = %+v, %v", got, err)
		}
		e := NewEncoder()
		Use{Kind: 0, ID: 65536}.Encode(e)
		if _, err := e.Bytes(); !errors.Is(err, ErrStableIDOutOfRange) {
			t.Fatalf("kind0/65536 encode: got %v", err)
		}
		raw := encodeTestPayload(t, func(e *Encoder) {
			e.U8(0)
			e.U32(65536)
		})
		if _, err := DecodeUse(NewDecoder(raw)); !errors.Is(err, ErrStableIDOutOfRange) {
			t.Fatalf("kind0/65536 decode: got %v", err)
		}
	})
	t.Run("Kind0Zero", func(t *testing.T) {
		b := encodeTestPayload(t, Use{Kind: 0, ID: 0}.Encode)
		if _, err := DecodeUse(NewDecoder(b)); err != nil {
			t.Fatalf("kind0/0: %v", err)
		}
	})
	t.Run("Kind1FullU32", func(t *testing.T) {
		b := encodeTestPayload(t, Use{Kind: 1, ID: math.MaxUint32}.Encode)
		got, err := DecodeUse(NewDecoder(b))
		if err != nil || got != (Use{Kind: 1, ID: math.MaxUint32}) {
			t.Fatalf("kind1/max: %+v, %v", got, err)
		}
	})
	t.Run("UnknownKindRoundTrip", func(t *testing.T) {
		// Future kinds are structural: never reinterpreted as skill/item.
		b := encodeTestPayload(t, Use{Kind: 200, ID: math.MaxUint32}.Encode)
		got, err := DecodeUse(NewDecoder(b))
		if err != nil || got != (Use{Kind: 200, ID: math.MaxUint32}) {
			t.Fatalf("kind200/max: %+v, %v", got, err)
		}
	})
}

func TestChatLimits(t *testing.T) {
	t.Run("Say", func(t *testing.T) {
		ok := strings.Repeat("s", MaxChatBytes)
		b := encodeTestPayload(t, Say{Channel: 1, Text: ok}.Encode)
		got, err := DecodeSay(NewDecoder(b))
		if err != nil || got.Text != ok {
			t.Fatalf("512-byte say: %v", err)
		}
		e := NewEncoder()
		Say{Channel: 1, Text: strings.Repeat("s", MaxChatBytes+1)}.Encode(e)
		if _, err := e.Bytes(); !errors.Is(err, ErrStringTooLong) {
			t.Fatalf("513-byte say encode: got %v", err)
		}
	})
	t.Run("SayGroup", func(t *testing.T) {
		// Multibyte content: byte length, not rune count, governs.
		ok := strings.Repeat("é", 256) // 512 UTF-8 bytes, 256 runes
		if len(ok) != 512 {
			t.Fatalf("fixture len = %d", len(ok))
		}
		b := encodeTestPayload(t, SayGroup{Text: ok}.Encode)
		got, err := DecodeSayGroup(NewDecoder(b))
		if err != nil || got.Text != ok {
			t.Fatalf("512-byte say_group: %v", err)
		}
		e := NewEncoder()
		SayGroup{Text: strings.Repeat("g", MaxChatBytes+1)}.Encode(e)
		if _, err := e.Bytes(); !errors.Is(err, ErrStringTooLong) {
			t.Fatalf("513-byte say_group encode: got %v", err)
		}
		raw := encodeTestPayload(t, func(e *Encoder) {
			e.U16(uint16(MaxChatBytes + 1))
			e.WriteBytes([]byte(strings.Repeat("g", MaxChatBytes+1)))
		})
		if _, err := DecodeSayGroup(NewDecoder(raw)); !errors.Is(err, ErrStringTooLong) {
			t.Fatalf("513-byte say_group decode: got %v", err)
		}
	})
}

func TestOfferCounterLimits(t *testing.T) {
	ids1024 := make([]uint32, 1024)
	for i := range ids1024 {
		ids1024[i] = uint32(i + 1)
	}
	b := encodeTestPayload(t, Offer{Target: 1, Items: ids1024}.Encode)
	got, err := DecodeOffer(NewDecoder(b))
	if err != nil || !reflect.DeepEqual(got.Items, ids1024) {
		t.Fatalf("1024 offer items: %v", err)
	}
	e := NewEncoder()
	Offer{Target: 1, Items: make([]uint32, 1025)}.Encode(e)
	if _, err := e.Bytes(); !errors.Is(err, ErrArrayTooLong) {
		t.Fatalf("1025 offer encode: got %v", err)
	}
	// Representative over-limit decode for Counter sharing the same helper.
	raw := encodeTestPayload(t, func(e *Encoder) {
		e.U16(1025)
	})
	if _, err := DecodeCounter(NewDecoder(raw)); !errors.Is(err, ErrArrayTooLong) {
		t.Fatalf("1025 counter decode: got %v", err)
	}
}

func TestIntentTrailingTolerance(t *testing.T) {
	b := encodeTestPayload(t, Move{InputSeq: 7, HeldDirs: 1, RunFlag: 0, Yaw: 100}.Encode)
	b = append(b, 0xDE, 0xAD, 0xBE, 0xEF)
	got, err := DecodeMove(NewDecoder(b))
	if err != nil {
		t.Fatalf("move with trailing bytes: %v", err)
	}
	if got.InputSeq != 7 || got.Yaw != 100 {
		t.Fatalf("move = %+v", got)
	}
	empty := encodeTestPayload(t, Accept{}.Encode)
	empty = append(empty, 0x01, 0x02)
	if _, err := DecodeAccept(NewDecoder(empty)); err != nil {
		t.Fatalf("accept with trailing bytes: %v", err)
	}
}

func TestIntentTruncation(t *testing.T) {
	t.Run("MoveMissingYaw", func(t *testing.T) {
		raw := encodeTestPayload(t, func(e *Encoder) {
			e.U32(1)
			e.U8(2)
			e.U8(0)
			e.U8(0x23) // only 1 of 2 yaw bytes
		})
		if _, err := DecodeMove(NewDecoder(raw)); !errors.Is(err, ErrTruncated) {
			t.Fatalf("got %v, want ErrTruncated", err)
		}
	})
	t.Run("CastMissingTarget", func(t *testing.T) {
		raw := encodeTestPayload(t, func(e *Encoder) {
			e.U16(9)
			e.U8(0x01) // partial u32 target
		})
		if _, err := DecodeCast(NewDecoder(raw)); !errors.Is(err, ErrTruncated) {
			t.Fatalf("got %v, want ErrTruncated", err)
		}
	})
	t.Run("UsePartialID", func(t *testing.T) {
		raw := encodeTestPayload(t, func(e *Encoder) {
			e.U8(1)
			e.WriteBytes([]byte{0x01, 0x02}) // 2 of 4 ID bytes
		})
		if _, err := DecodeUse(NewDecoder(raw)); !errors.Is(err, ErrTruncated) {
			t.Fatalf("got %v, want ErrTruncated", err)
		}
	})
	t.Run("GiveMissingQty", func(t *testing.T) {
		raw := encodeTestPayload(t, func(e *Encoder) {
			e.U32(1)
			e.U32(2)
			e.U8(0x09) // 1 of 2 qty bytes
		})
		if _, err := DecodeGive(NewDecoder(raw)); !errors.Is(err, ErrTruncated) {
			t.Fatalf("got %v, want ErrTruncated", err)
		}
	})
	t.Run("OfferEarlyEnd", func(t *testing.T) {
		raw := encodeTestPayload(t, func(e *Encoder) {
			e.U32(1)
			e.U16(4)  // declares 4 items...
			e.U32(10) // ...provides 1
		})
		if _, err := DecodeOffer(NewDecoder(raw)); !errors.Is(err, ErrTruncated) {
			t.Fatalf("got %v, want ErrTruncated", err)
		}
	})
	t.Run("SayOverlongText", func(t *testing.T) {
		raw := encodeTestPayload(t, func(e *Encoder) {
			e.U8(0)
			e.U16(100)                    // declares 100 text bytes...
			e.WriteBytes([]byte("short")) // ...provides 5
		})
		if _, err := DecodeSay(NewDecoder(raw)); !errors.Is(err, ErrTruncated) {
			t.Fatalf("got %v, want ErrTruncated", err)
		}
	})
}
