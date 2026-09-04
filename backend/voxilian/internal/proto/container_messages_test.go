package proto

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestContainerOpcodesExact(t *testing.T) {
	cases := []struct {
		got  uint16
		want uint16
		name string
	}{
		{OpcodeInventoryDelta, 211, "InventoryDelta"},
		{OpcodeOfferUpdate, 212, "OfferUpdate"},
		{OpcodeChunkFragment, 218, "ChunkFragment"},
		{OpcodeShopList, 220, "ShopList"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Fatalf("Opcode%s = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
}

// TestOpcodeCompleteness proves every §6 application opcode constant is
// present at its exact number. Real gaps (127–199, 221+) stay absent:
// no placeholder constants exist to make the ranges continuous.
func TestOpcodeCompleteness(t *testing.T) {
	cases := []struct {
		got  uint16
		want uint16
		name string
	}{
		{OpcodeHello, 100, "Hello"}, {OpcodeReauth, 101, "Reauth"},
		{OpcodeMove, 102, "Move"}, {OpcodeAttack, 103, "Attack"},
		{OpcodeCast, 104, "Cast"}, {OpcodeUse, 105, "Use"},
		{OpcodeGet, 106, "Get"}, {OpcodeDrop, 107, "Drop"},
		{OpcodePut, 108, "Put"}, {OpcodeGive, 109, "Give"},
		{OpcodeOffer, 110, "Offer"}, {OpcodeCounter, 111, "Counter"},
		{OpcodeAccept, 112, "Accept"}, {OpcodeCancel, 113, "Cancel"},
		{OpcodeBuy, 114, "Buy"}, {OpcodeRest, 115, "Rest"},
		{OpcodeEat, 116, "Eat"}, {OpcodeSay, 117, "Say"},
		{OpcodeSayGroup, 118, "SayGroup"},
		{OpcodeSafetyToggle, 119, "SafetyToggle"},
		{OpcodeRespawnAck, 120, "RespawnAck"},
		{OpcodeCharacterList, 121, "CharacterList"},
		{OpcodeCharacterCreate, 122, "CharacterCreate"},
		{OpcodeCharacterDelete, 123, "CharacterDelete"},
		{OpcodeEnterWorld, 124, "EnterWorld"},
		{OpcodeAck, 125, "Ack"}, {OpcodeLeaveWorld, 126, "LeaveWorld"},
		{OpcodeWelcome, 200, "Welcome"}, {OpcodeReauthOK, 201, "ReauthOK"},
		{OpcodeError, 202, "Error"},
		{OpcodeCellSnapshot, 203, "CellSnapshot"},
		{OpcodeEntityCreate, 204, "EntityCreate"},
		{OpcodeEntityMove, 205, "EntityMove"},
		{OpcodeEntityRemove, 206, "EntityRemove"},
		{OpcodeStat, 207, "Stat"}, {OpcodeStatGroup, 208, "StatGroup"},
		{OpcodeSaid, 209, "Said"}, {OpcodeEffect, 210, "Effect"},
		{OpcodeInventoryDelta, 211, "InventoryDelta"},
		{OpcodeOfferUpdate, 212, "OfferUpdate"},
		{OpcodeTradeResult, 213, "TradeResult"},
		{OpcodeDeath, 214, "Death"}, {OpcodeRespawn, 215, "Respawn"},
		{OpcodeCharacterListResult, 216, "CharacterListResult"},
		{OpcodeCharacterOp, 217, "CharacterOp"},
		{OpcodeChunkFragment, 218, "ChunkFragment"},
		{OpcodeWorldReady, 219, "WorldReady"},
		{OpcodeShopList, 220, "ShopList"},
	}
	if len(cases) != 48 {
		t.Fatalf("opcode cases = %d, want 48", len(cases))
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Fatalf("Opcode%s = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
}

func sampleInventoryEntry() InventoryEntry {
	return InventoryEntry{
		Item: 0x01020304, Proto: 0x0506, Qty: 0x0708, Hits: -1,
		Location: 1, Container: 0x0A0B0C0D, Slot: "A",
	}
}

func TestInventoryExactWire(t *testing.T) {
	if InventoryEntryPrefixSize != 19 {
		t.Fatalf("InventoryEntryPrefixSize = %d, want 19", InventoryEntryPrefixSize)
	}
	m := InventoryDelta{Items: []InventoryEntry{sampleInventoryEntry()}}
	b := encodeTestPayload(t, m.Encode)
	// count(2) + entryLen(2) + 20-byte body.
	want := []byte{
		0x01, 0x00, // count
		0x14, 0x00, // entryLen = 20 = 0x0014
		0x04, 0x03, 0x02, 0x01, // item LE
		0x06, 0x05, // proto LE
		0x08, 0x07, // qty LE
		0xFF, 0xFF, 0xFF, 0xFF, // hits -1 LE
		0x01,                   // location
		0x0D, 0x0C, 0x0B, 0x0A, // container LE
		0x01, 0x00, 0x41, // slot "A"
	}
	if string(b) != string(want) {
		t.Fatalf("inventory wire = % x, want % x", b, want)
	}
	got, err := DecodeInventoryDelta(NewDecoder(b))
	if err != nil || !reflect.DeepEqual(got, m) {
		t.Fatalf("inventory decode = %+v, %v", got, err)
	}
}

func TestInventoryHitsSignedRange(t *testing.T) {
	for _, hits := range []int32{math.MinInt32, -1, 0, math.MaxInt32} {
		v := InventoryEntry{Item: 1, Hits: hits, Slot: "backpack"}
		b := encodeTestPayload(t, InventoryDelta{Items: []InventoryEntry{v}}.Encode)
		got, err := DecodeInventoryDelta(NewDecoder(b))
		if err != nil || len(got.Items) != 1 || got.Items[0].Hits != hits {
			t.Fatalf("hits %d round-trip = %+v, %v", hits, got, err)
		}
	}
}

func TestInventoryLocationBehavior(t *testing.T) {
	for _, v := range []InventoryEntry{
		{Item: 1, Location: InventoryLocationDirect, Container: 0, Slot: "hand"},
		{Item: 2, Location: InventoryLocationContainer, Container: math.MaxUint32, Slot: "pouch"},
		{Item: 3, Location: 200, Container: math.MaxUint32, Slot: "future"},
	} {
		b := encodeTestPayload(t, InventoryDelta{Items: []InventoryEntry{v}}.Encode)
		got, err := DecodeInventoryDelta(NewDecoder(b))
		if err != nil || !reflect.DeepEqual(got.Items[0], v) {
			t.Fatalf("location %d = %+v, %v", v.Location, got, err)
		}
	}
	// Qty 0 is NOT removal at the wire layer; 206 owns invalidation.
	b := encodeTestPayload(t, InventoryDelta{Items: []InventoryEntry{{Item: 9, Qty: 0}}}.Encode)
	got, err := DecodeInventoryDelta(NewDecoder(b))
	if err != nil || len(got.Items) != 1 || got.Items[0].Qty != 0 {
		t.Fatalf("qty 0 = %+v, %v", got, err)
	}
}

func TestInventorySlotLimits(t *testing.T) {
	ok := strings.Repeat("s", MaxStringBytes)
	b := encodeTestPayload(t, InventoryDelta{Items: []InventoryEntry{{Item: 1, Slot: ok}}}.Encode)
	got, err := DecodeInventoryDelta(NewDecoder(b))
	if err != nil || got.Items[0].Slot != ok {
		t.Fatalf("1024-byte slot: %v", err)
	}
	e := NewEncoder()
	InventoryDelta{Items: []InventoryEntry{{Item: 1, Slot: strings.Repeat("s", MaxStringBytes+1)}}}.Encode(e)
	if _, err := e.Bytes(); !errors.Is(err, ErrStringTooLong) {
		t.Fatalf("1025-byte slot encode: got %v", err)
	}
}

func TestInventoryCountLimits(t *testing.T) {
	items := make([]InventoryEntry, 1024)
	for i := range items {
		items[i] = InventoryEntry{Item: uint32(i + 1), Slot: "bag"}
	}
	b := encodeTestPayload(t, InventoryDelta{Items: items}.Encode)
	got, err := DecodeInventoryDelta(NewDecoder(b))
	if err != nil || len(got.Items) != 1024 {
		t.Fatalf("1024 entries: %v", err)
	}
	e := NewEncoder()
	InventoryDelta{Items: make([]InventoryEntry, 1025)}.Encode(e)
	if _, err := e.Bytes(); !errors.Is(err, ErrArrayTooLong) {
		t.Fatalf("1025 encode: got %v", err)
	}
	raw := encodeTestPayload(t, func(e *Encoder) { e.U16(1025) })
	if _, err := DecodeInventoryDelta(NewDecoder(raw)); !errors.Is(err, ErrArrayTooLong) {
		t.Fatalf("1025 decode: got %v", err)
	}
}

func TestInventoryEntryForwardCompatibility(t *testing.T) {
	raw := encodeTestPayload(t, func(e *Encoder) {
		e.Count(2, MaxArrayCount)
		e.Entry(func(sub *Encoder) error {
			sampleInventoryEntry().encode(sub)
			sub.U32(0xDEADBEEF) // unknown future field
			return nil
		})
		e.Entry(func(sub *Encoder) error {
			sampleInventoryEntry().encode(sub)
			return nil
		})
	})
	got, err := DecodeInventoryDelta(NewDecoder(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Items) != 2 || !reflect.DeepEqual(got.Items[0], sampleInventoryEntry()) ||
		!reflect.DeepEqual(got.Items[1], sampleInventoryEntry()) {
		t.Fatalf("got %+v", got)
	}
}

func TestOfferUpdateExactWire(t *testing.T) {
	if OfferItemWireSize != 6 {
		t.Fatalf("OfferItemWireSize = %d, want 6", OfferItemWireSize)
	}
	m := OfferUpdate{With: 0x01020304, State: 7, Items: []OfferItem{{Item: 0x0A0B0C0D, Qty: 0x0E0F}}}
	b := encodeTestPayload(t, m.Encode)
	want := []byte{
		0x04, 0x03, 0x02, 0x01, // with LE
		0x07,       // state raw
		0x01, 0x00, // count
		0x06, 0x00, // entryLen = 6
		0x0D, 0x0C, 0x0B, 0x0A, // item LE
		0x0F, 0x0E, // qty LE
	}
	if string(b) != string(want) {
		t.Fatalf("offer_update wire = % x, want % x", b, want)
	}
	got, err := DecodeOfferUpdate(NewDecoder(b))
	if err != nil || !reflect.DeepEqual(got, m) {
		t.Fatalf("offer_update decode = %+v, %v", got, err)
	}
}

func TestOfferUpdateLimitsAndCompat(t *testing.T) {
	items := make([]OfferItem, 1024)
	for i := range items {
		items[i] = OfferItem{Item: uint32(i + 1), Qty: 1}
	}
	b := encodeTestPayload(t, OfferUpdate{With: 1, State: 255, Items: items}.Encode)
	got, err := DecodeOfferUpdate(NewDecoder(b))
	if err != nil || len(got.Items) != 1024 || got.State != 255 {
		t.Fatalf("1024 items: %v", err)
	}
	e := NewEncoder()
	OfferUpdate{Items: make([]OfferItem, 1025)}.Encode(e)
	if _, err := e.Bytes(); !errors.Is(err, ErrArrayTooLong) {
		t.Fatalf("1025 encode: got %v", err)
	}
	raw := encodeTestPayload(t, func(e *Encoder) {
		e.U32(1)
		e.U8(0)
		e.U16(1025)
	})
	if _, err := DecodeOfferUpdate(NewDecoder(raw)); !errors.Is(err, ErrArrayTooLong) {
		t.Fatalf("1025 decode: got %v", err)
	}
	// Future trailing bytes inside the first entry must not corrupt the second.
	future := encodeTestPayload(t, func(e *Encoder) {
		e.U32(9)
		e.U8(2)
		e.Count(2, MaxArrayCount)
		e.Entry(func(sub *Encoder) error {
			OfferItem{Item: 11, Qty: 1}.encode(sub)
			sub.U16(0xBEEF)
			return nil
		})
		e.Entry(func(sub *Encoder) error {
			OfferItem{Item: 12, Qty: 2}.encode(sub)
			return nil
		})
	})
	fgot, err := DecodeOfferUpdate(NewDecoder(future))
	if err != nil {
		t.Fatalf("future entry: %v", err)
	}
	if !reflect.DeepEqual(fgot.Items, []OfferItem{{Item: 11, Qty: 1}, {Item: 12, Qty: 2}}) {
		t.Fatalf("items = %+v", fgot.Items)
	}
}

func TestChunkFragmentExactWire(t *testing.T) {
	m := ChunkFragment{
		Cell: Cell{X: 1, Z: -1}, ChunkIdx: 0x01020304,
		FragIdx: 0x0506, FragCount: 0x0708,
		Bytes: []byte{0xAA, 0xBB, 0xCC},
	}
	b := encodeTestPayload(t, m.Encode)
	want := []byte{
		0x01, 0x00, 0x00, 0x00, // cell.x LE
		0xFF, 0xFF, 0xFF, 0xFF, // cell.z LE
		0x04, 0x03, 0x02, 0x01, // chunkIdx LE
		0x06, 0x05, // fragIdx LE
		0x08, 0x07, // fragCount LE
		0x03, 0x00, // byteLen LE
		0xAA, 0xBB, 0xCC, // bytes
	}
	if string(b) != string(want) {
		t.Fatalf("chunk wire = % x, want % x", b, want)
	}
	got, err := DecodeChunkFragment(NewDecoder(b))
	if err != nil || !reflect.DeepEqual(got, m) {
		t.Fatalf("chunk decode = %+v, %v", got, err)
	}
}

func TestChunkFragmentSizeBounds(t *testing.T) {
	if MaxChunkFragmentBytes != 61440 {
		t.Fatalf("MaxChunkFragmentBytes = %d, want 61440", MaxChunkFragmentBytes)
	}
	full := make([]byte, MaxChunkFragmentBytes)
	for i := range full {
		full[i] = byte(i)
	}
	b := encodeTestPayload(t, ChunkFragment{Bytes: full}.Encode)
	got, err := DecodeChunkFragment(NewDecoder(b))
	if err != nil || !reflect.DeepEqual(got.Bytes, full) {
		t.Fatalf("61440-byte fragment: %v", err)
	}
	e := NewEncoder()
	ChunkFragment{Bytes: make([]byte, MaxChunkFragmentBytes+1)}.Encode(e)
	if _, err := e.Bytes(); !errors.Is(err, ErrChunkFragmentTooLarge) {
		t.Fatalf("61441 encode: got %v", err)
	}
	// Declared length beyond remaining bytes is truncated...
	raw := encodeTestPayload(t, func(e *Encoder) {
		e.Cell(Cell{})
		e.U32(1)
		e.U16(0)
		e.U16(1)
		e.U16(100)
		e.WriteBytes([]byte{0x01, 0x02})
	})
	if _, err := DecodeChunkFragment(NewDecoder(raw)); !errors.Is(err, ErrTruncated) {
		t.Fatalf("short blob: got %v", err)
	}
	// ...but a declared length above the 60 KiB bound is the
	// message-specific error even though u16 can express it.
	big := encodeTestPayload(t, func(e *Encoder) {
		e.Cell(Cell{})
		e.U32(1)
		e.U16(0)
		e.U16(1)
		e.U16(61500)
	})
	if _, err := DecodeChunkFragment(NewDecoder(big)); !errors.Is(err, ErrChunkFragmentTooLarge) {
		t.Fatalf("61500 declared: got %v", err)
	}
}

func TestChunkFragmentTrailingAndFrameBound(t *testing.T) {
	b := encodeTestPayload(t, ChunkFragment{
		Cell: ChunkFragment{}.Cell, ChunkIdx: 9, FragIdx: 0, FragCount: 1,
		Bytes: []byte{0x01, 0x02},
	}.Encode)
	b = append(b, 0xDE, 0xAD, 0xBE, 0xEF)
	got, err := DecodeChunkFragment(NewDecoder(b))
	if err != nil {
		t.Fatalf("trailing bytes: %v", err)
	}
	if !reflect.DeepEqual(got.Bytes, []byte{0x01, 0x02}) {
		t.Fatalf("bytes = % x", got.Bytes)
	}
	// Max-size fragment still fits the 64 KiB envelope: total 61470.
	full := make([]byte, MaxChunkFragmentBytes)
	frame, err := EncodeFrame(Header{Opcode: OpcodeChunkFragment, MsgVersion: 1, Seq: 1, Tick: 2},
		func(e *Encoder) error {
			ChunkFragment{Cell: Cell{X: 1, Z: 2}, Bytes: full}.Encode(e)
			return e.Err()
		})
	if err != nil {
		t.Fatalf("max frame: %v", err)
	}
	if len(frame) != 61470 {
		t.Fatalf("max frame len = %d, want 61470", len(frame))
	}
	hdr, dec, err := DecodeFrame(frame)
	if err != nil {
		t.Fatalf("decode max frame: %v", err)
	}
	if hdr.Opcode != OpcodeChunkFragment {
		t.Fatalf("opcode = %d", hdr.Opcode)
	}
	frag, err := DecodeChunkFragment(dec)
	if err != nil || len(frag.Bytes) != MaxChunkFragmentBytes {
		t.Fatalf("max fragment decode: %v", len(frag.Bytes))
	}
}

func TestChunkFragmentNoSemantics(t *testing.T) {
	// FragCount 0, FragIdx >= FragCount, MaxUint32 idx, empty blob:
	// all codec-valid.
	m := ChunkFragment{ChunkIdx: math.MaxUint32, FragIdx: 5, FragCount: 0, Bytes: nil}
	b := encodeTestPayload(t, m.Encode)
	got, err := DecodeChunkFragment(NewDecoder(b))
	if err != nil || got.ChunkIdx != math.MaxUint32 || len(got.Bytes) != 0 {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestShopListExactWire(t *testing.T) {
	if ShopListingEntryWireSize != 8 {
		t.Fatalf("ShopListingEntryWireSize = %d, want 8", ShopListingEntryWireSize)
	}
	m := ShopList{
		Vendor: 0x01020304,
		Listings: []ShopListingEntry{
			{Listing: 0x0506, Price: 0x0708090A, Qty: 0x0B0C},
		},
	}
	b := encodeTestPayload(t, m.Encode)
	want := []byte{
		0x04, 0x03, 0x02, 0x01, // vendor u32 LE
		0x01, 0x00, // count
		0x08, 0x00, // entryLen = 8
		0x06, 0x05, // listing LE
		0x0A, 0x09, 0x08, 0x07, // price LE
		0x0C, 0x0B, // qty LE
	}
	if string(b) != string(want) {
		t.Fatalf("shop wire = % x, want % x", b, want)
	}
	got, err := DecodeShopList(NewDecoder(b))
	if err != nil || !reflect.DeepEqual(got, m) {
		t.Fatalf("shop decode = %+v, %v", got, err)
	}
}

func TestShopListLimitsAndCompat(t *testing.T) {
	listings := make([]ShopListingEntry, 1024)
	for i := range listings {
		listings[i] = ShopListingEntry{Listing: uint16(i + 1), Price: math.MaxUint32, Qty: 99}
	}
	m := ShopList{Vendor: math.MaxUint32, Listings: listings}
	b := encodeTestPayload(t, m.Encode)
	got, err := DecodeShopList(NewDecoder(b))
	if err != nil || !reflect.DeepEqual(got, m) {
		t.Fatalf("1024 listings: %v", err)
	}
	e := NewEncoder()
	ShopList{Listings: make([]ShopListingEntry, 1025)}.Encode(e)
	if _, err := e.Bytes(); !errors.Is(err, ErrArrayTooLong) {
		t.Fatalf("1025 encode: got %v", err)
	}
	raw := encodeTestPayload(t, func(e *Encoder) {
		e.U32(1)
		e.U16(1025)
	})
	if _, err := DecodeShopList(NewDecoder(raw)); !errors.Is(err, ErrArrayTooLong) {
		t.Fatalf("1025 decode: got %v", err)
	}
	future := encodeTestPayload(t, func(e *Encoder) {
		e.U32(7)
		e.Count(2, MaxArrayCount)
		e.Entry(func(sub *Encoder) error {
			ShopListingEntry{Listing: 1, Price: 10, Qty: 1}.encode(sub)
			sub.U32(0xDEADBEEF)
			return nil
		})
		e.Entry(func(sub *Encoder) error {
			ShopListingEntry{Listing: 2, Price: 20, Qty: 2}.encode(sub)
			return nil
		})
	})
	fgot, err := DecodeShopList(NewDecoder(future))
	if err != nil {
		t.Fatalf("future entry: %v", err)
	}
	if !reflect.DeepEqual(fgot.Listings, []ShopListingEntry{
		{Listing: 1, Price: 10, Qty: 1},
		{Listing: 2, Price: 20, Qty: 2},
	}) {
		t.Fatalf("listings = %+v", fgot.Listings)
	}
}

func TestContainerRoundTrips(t *testing.T) {
	t.Run("InventoryDelta", func(t *testing.T) {
		want := InventoryDelta{Items: []InventoryEntry{
			{Item: math.MaxUint32, Proto: 0xFFFF, Qty: 0xFFFF, Hits: math.MinInt32,
				Location: InventoryLocationDirect, Container: 0, Slot: ""},
			sampleInventoryEntry(),
		}}
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodeInventoryDelta(NewDecoder(b))
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("InventoryDelta = %+v, %v", got, err)
		}
	})
	t.Run("OfferUpdate", func(t *testing.T) {
		want := OfferUpdate{With: math.MaxUint32, State: 200, Items: []OfferItem{{Item: 1, Qty: 0}}}
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodeOfferUpdate(NewDecoder(b))
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("OfferUpdate = %+v, %v", got, err)
		}
	})
	t.Run("ChunkFragment", func(t *testing.T) {
		want := ChunkFragment{
			Cell:     Cell{X: math.MinInt32, Z: math.MaxInt32},
			ChunkIdx: math.MaxUint32, FragIdx: 3, FragCount: 9,
			Bytes: []byte{0x00, 0xFF, 0x42},
		}
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodeChunkFragment(NewDecoder(b))
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("ChunkFragment = %+v, %v", got, err)
		}
	})
	t.Run("ShopList", func(t *testing.T) {
		want := ShopList{
			Vendor: math.MaxUint32,
			Listings: []ShopListingEntry{
				{Listing: 0xFFFF, Price: math.MaxUint32, Qty: 0xFFFF},
			},
		}
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodeShopList(NewDecoder(b))
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("ShopList = %+v, %v", got, err)
		}
	})
}

func TestContainerFrameIntegration(t *testing.T) {
	t.Run("InventoryDelta", func(t *testing.T) {
		h := Header{Opcode: OpcodeInventoryDelta, MsgVersion: 1, Seq: 500, Tick: 600}
		want := InventoryDelta{Items: []InventoryEntry{sampleInventoryEntry()}}
		dec := frameRoundTrip(t, h, want.Encode)
		got, err := DecodeInventoryDelta(dec)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("InventoryDelta = %+v, %v", got, err)
		}
	})
	t.Run("ChunkFragment", func(t *testing.T) {
		h := Header{Opcode: OpcodeChunkFragment, MsgVersion: 1, Seq: 501, Tick: 601}
		want := ChunkFragment{Cell: Cell{X: 4, Z: 5}, ChunkIdx: 6, FragIdx: 0, FragCount: 2, Bytes: []byte{0xDE, 0xAD}}
		dec := frameRoundTrip(t, h, want.Encode)
		got, err := DecodeChunkFragment(dec)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("ChunkFragment = %+v, %v", got, err)
		}
	})
	t.Run("ShopList", func(t *testing.T) {
		h := Header{Opcode: OpcodeShopList, MsgVersion: 1, Seq: 502, Tick: 602}
		want := ShopList{Vendor: 11, Listings: []ShopListingEntry{{Listing: 1, Price: 100, Qty: 3}}}
		dec := frameRoundTrip(t, h, want.Encode)
		got, err := DecodeShopList(dec)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("ShopList = %+v, %v", got, err)
		}
	})
}

func TestContainerTrailingMessages(t *testing.T) {
	b := encodeTestPayload(t, InventoryDelta{Items: []InventoryEntry{sampleInventoryEntry()}}.Encode)
	b = append(b, 0xAA, 0xBB)
	got, err := DecodeInventoryDelta(NewDecoder(b))
	if err != nil || len(got.Items) != 1 {
		t.Fatalf("inventory trailing = %+v, %v", got, err)
	}
}

func TestContainerTruncation(t *testing.T) {
	t.Run("InventoryOverlongEntry", func(t *testing.T) {
		raw := encodeTestPayload(t, func(e *Encoder) {
			e.Count(1, MaxArrayCount)
			e.U16(40)
			e.WriteBytes(make([]byte, 5))
		})
		if _, err := DecodeInventoryDelta(NewDecoder(raw)); !errors.Is(err, ErrTruncated) {
			t.Fatalf("got %v, want ErrTruncated", err)
		}
	})
	t.Run("InventorySlotBytes", func(t *testing.T) {
		b := encodeTestPayload(t, InventoryDelta{Items: []InventoryEntry{sampleInventoryEntry()}}.Encode)
		if _, err := DecodeInventoryDelta(NewDecoder(b[:len(b)-1])); !errors.Is(err, ErrTruncated) {
			t.Fatalf("got %v, want ErrTruncated", err)
		}
	})
	t.Run("OfferUpdateQty", func(t *testing.T) {
		raw := encodeTestPayload(t, func(e *Encoder) {
			e.U32(1)
			e.U8(0)
			e.Count(1, MaxArrayCount)
			e.U16(6)
			e.U32(99) // item but no qty
		})
		if _, err := DecodeOfferUpdate(NewDecoder(raw)); !errors.Is(err, ErrTruncated) {
			t.Fatalf("got %v, want ErrTruncated", err)
		}
	})
	t.Run("ChunkMissingByteLen", func(t *testing.T) {
		raw := encodeTestPayload(t, func(e *Encoder) {
			e.Cell(Cell{})
			e.U32(1)
			e.U16(0)
			e.U16(1)
			e.U8(0x03) // 1 of 2 byteLen bytes
		})
		if _, err := DecodeChunkFragment(NewDecoder(raw)); !errors.Is(err, ErrTruncated) {
			t.Fatalf("got %v, want ErrTruncated", err)
		}
	})
	t.Run("ChunkShortBlob", func(t *testing.T) {
		raw := encodeTestPayload(t, func(e *Encoder) {
			e.Cell(Cell{})
			e.U32(1)
			e.U16(0)
			e.U16(1)
			e.U16(10)
			e.WriteBytes([]byte{0x01})
		})
		if _, err := DecodeChunkFragment(NewDecoder(raw)); !errors.Is(err, ErrTruncated) {
			t.Fatalf("got %v, want ErrTruncated", err)
		}
	})
	t.Run("ShopPrice", func(t *testing.T) {
		raw := encodeTestPayload(t, func(e *Encoder) {
			e.U32(1)
			e.Count(1, MaxArrayCount)
			e.U16(8)
			e.U16(5)
			e.U8(0x01) // partial price u32
		})
		if _, err := DecodeShopList(NewDecoder(raw)); !errors.Is(err, ErrTruncated) {
			t.Fatalf("got %v, want ErrTruncated", err)
		}
	})
}
