package proto

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestEntityOpcodesExact(t *testing.T) {
	want := map[uint16]string{
		203: "CellSnapshot", 204: "EntityCreate", 205: "EntityMove",
		206: "EntityRemove", 207: "Stat", 208: "StatGroup",
		209: "Said", 210: "Effect",
		213: "TradeResult", 214: "Death", 215: "Respawn",
	}
	got := map[uint16]string{
		OpcodeCellSnapshot: "CellSnapshot", OpcodeEntityCreate: "EntityCreate",
		OpcodeEntityMove: "EntityMove", OpcodeEntityRemove: "EntityRemove",
		OpcodeStat: "Stat", OpcodeStatGroup: "StatGroup",
		OpcodeSaid: "Said", OpcodeEffect: "Effect",
		OpcodeTradeResult: "TradeResult", OpcodeDeath: "Death",
		OpcodeRespawn: "Respawn",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entity opcodes = %v, want %v", got, want)
	}
}

func sampleEntityEntry() EntityEntry {
	return EntityEntry{
		Entity: 0x01020304,
		Kind:   2,
		Proto:  0x0506,
		Pos:    Position{X: 1, Y: -1, Z: 0x01020304},
		Angle:  0x0123,
		Speed:  0x45,
	}
}

func TestEntityEntryExactWire(t *testing.T) {
	if EntityEntryWireSize != 22 {
		t.Fatalf("EntityEntryWireSize = %d, want 22", EntityEntryWireSize)
	}
	b := encodeTestPayload(t, func(e *Encoder) { sampleEntityEntry().encode(e) })
	want := []byte{
		0x04, 0x03, 0x02, 0x01, // entity LE
		0x02,       // kind
		0x06, 0x05, // proto LE
		0x01, 0x00, 0x00, 0x00, // pos.x = 1 LE
		0xFF, 0xFF, 0xFF, 0xFF, // pos.y = -1 LE
		0x04, 0x03, 0x02, 0x01, // pos.z LE
		0x23, 0x01, // angle LE
		0x45, // speed
	}
	if len(b) != 22 {
		t.Fatalf("entry len = %d, want 22", len(b))
	}
	if string(b) != string(want) {
		t.Fatalf("entry wire = % x, want % x", b, want)
	}
	v, err := decodeEntityEntry(NewDecoder(b), "test")
	if err != nil || !reflect.DeepEqual(v, sampleEntityEntry()) {
		t.Fatalf("entry decode = %+v, %v", v, err)
	}
}

func TestEntityMessageRoundTrips(t *testing.T) {
	t.Run("CellSnapshot", func(t *testing.T) {
		want := CellSnapshot{
			Cell: Cell{X: math.MinInt32, Z: math.MaxInt32},
			Entities: []EntityEntry{
				sampleEntityEntry(),
				{Entity: math.MaxUint32, Kind: 0xFF, Proto: 0xFFFF,
					Pos:   Position{X: math.MaxInt32, Y: math.MinInt32, Z: 0},
					Angle: 4095, Speed: 0xFF},
			},
		}
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodeCellSnapshot(NewDecoder(b))
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("CellSnapshot = %+v, %v", got, err)
		}
	})
	t.Run("EntityCreate", func(t *testing.T) {
		want := EntityCreate{Entity: sampleEntityEntry()}
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodeEntityCreate(NewDecoder(b))
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("EntityCreate = %+v, %v", got, err)
		}
	})
	t.Run("EntityMove", func(t *testing.T) {
		for _, seq := range []uint32{0, 42, math.MaxUint32} {
			want := EntityMove{
				Entity: math.MaxUint32,
				Pos:    Position{X: math.MinInt32, Y: math.MaxInt32, Z: -1},
				Angle:  4095, Speed: 7, LastProcessedInputSeq: seq,
			}
			b := encodeTestPayload(t, want.Encode)
			got, err := DecodeEntityMove(NewDecoder(b))
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("EntityMove(seq=%d) = %+v, %v", seq, got, err)
			}
		}
	})
	t.Run("EntityRemove", func(t *testing.T) {
		want := EntityRemove{Entity: math.MaxUint32}
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodeEntityRemove(NewDecoder(b))
		if err != nil || got != want {
			t.Fatalf("EntityRemove = %+v, %v", got, err)
		}
	})
	t.Run("Stat", func(t *testing.T) {
		want := Stat{
			Entity: math.MaxUint32,
			Stat: StatEntry{
				StatID: 0xFF, Value: -123,
				Min: math.MinInt32, Max: math.MaxInt32, CurMax: -1,
			},
		}
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodeStat(NewDecoder(b))
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("Stat = %+v, %v", got, err)
		}
	})
	t.Run("StatGroup", func(t *testing.T) {
		want := StatGroup{
			Entity: math.MaxUint32,
			Stats: []StatEntry{
				{StatID: 1, Value: -123, Min: math.MinInt32, Max: math.MaxInt32, CurMax: -1},
				{StatID: 2, Value: 0, Min: 0, Max: 0, CurMax: 0},
			},
		}
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodeStatGroup(NewDecoder(b))
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("StatGroup = %+v, %v", got, err)
		}
	})
	t.Run("Said", func(t *testing.T) {
		want := Said{From: math.MaxUint32, Channel: 4, Text: "héllo ✓"}
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodeSaid(NewDecoder(b))
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("Said = %+v, %v", got, err)
		}
	})
	t.Run("Effect", func(t *testing.T) {
		want := Effect{
			ID: 0xBEEF, Target: math.MaxUint32,
			Pos: Position{X: math.MinInt32, Y: math.MaxInt32, Z: 123},
		}
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodeEffect(NewDecoder(b))
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("Effect = %+v, %v", got, err)
		}
	})
	t.Run("TradeResult", func(t *testing.T) {
		// Raw u8 preserved, never normalized.
		for _, ok := range []uint8{0, 1, 255} {
			want := TradeResult{OK: ok}
			b := encodeTestPayload(t, want.Encode)
			got, err := DecodeTradeResult(NewDecoder(b))
			if err != nil || got != want {
				t.Fatalf("TradeResult(%d) = %+v, %v", ok, got, err)
			}
		}
	})
	t.Run("Death", func(t *testing.T) {
		want := Death{Victim: math.MaxUint32}
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodeDeath(NewDecoder(b))
		if err != nil || got != want {
			t.Fatalf("Death = %+v, %v", got, err)
		}
	})
	t.Run("Respawn", func(t *testing.T) {
		want := Respawn{Pos: Position{X: math.MinInt32, Y: math.MaxInt32, Z: 0}}
		b := encodeTestPayload(t, want.Encode)
		got, err := DecodeRespawn(NewDecoder(b))
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("Respawn = %+v, %v", got, err)
		}
	})
}

func TestCellSnapshotEntryLenPrefix(t *testing.T) {
	m := CellSnapshot{
		Cell:     Cell{X: 1, Z: -1},
		Entities: []EntityEntry{sampleEntityEntry()},
	}
	b := encodeTestPayload(t, m.Encode)
	want := []byte{
		0x01, 0x00, 0x00, 0x00, // cell.x = 1 LE
		0xFF, 0xFF, 0xFF, 0xFF, // cell.z = -1 LE
		0x01, 0x00, // count
		0x16, 0x00, // entryLen = 22 = 0x0016 LE
	}
	if len(b) != 8+2+2+22 {
		t.Fatalf("snapshot len = %d, want 34", len(b))
	}
	if string(b[:12]) != string(want) {
		t.Fatalf("snapshot prefix = % x, want % x", b[:12], want)
	}
}

func TestCellSnapshotLimits(t *testing.T) {
	entries := make([]EntityEntry, 1024)
	for i := range entries {
		entries[i] = EntityEntry{Entity: uint32(i + 1), Angle: 100}
	}
	m := CellSnapshot{Cell: Cell{X: 3, Z: 4}, Entities: entries}
	b := encodeTestPayload(t, m.Encode)
	// 8 cell + 2 count + 1024*(2+22) = 24586, well under 64 KiB.
	if len(b) != 8+2+1024*24 {
		t.Fatalf("1024-entry snapshot len = %d", len(b))
	}
	got, err := DecodeCellSnapshot(NewDecoder(b))
	if err != nil {
		t.Fatalf("1024 entries: %v", err)
	}
	if len(got.Entities) != 1024 || got.Entities[1023].Entity != 1024 {
		t.Fatalf("1024 entries decoded = %d", len(got.Entities))
	}
	e := NewEncoder()
	CellSnapshot{Entities: make([]EntityEntry, 1025)}.Encode(e)
	if _, err := e.Bytes(); !errors.Is(err, ErrArrayTooLong) {
		t.Fatalf("1025 encode: got %v", err)
	}
	raw := encodeTestPayload(t, func(e *Encoder) {
		e.Cell(Cell{})
		e.U16(1025)
	})
	if _, err := DecodeCellSnapshot(NewDecoder(raw)); !errors.Is(err, ErrArrayTooLong) {
		t.Fatalf("1025 decode: got %v", err)
	}
}

func TestCellSnapshotEntryForwardCompatibility(t *testing.T) {
	raw := encodeTestPayload(t, func(e *Encoder) {
		e.Cell(Cell{X: 9, Z: 9})
		e.Count(2, MaxArrayCount)
		e.Entry(func(sub *Encoder) error {
			sampleEntityEntry().encode(sub)
			sub.U32(0xDEADBEEF) // unknown future field
			return nil
		})
		e.Entry(func(sub *Encoder) error {
			sampleEntityEntry().encode(sub)
			return nil
		})
	})
	got, err := DecodeCellSnapshot(NewDecoder(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Entities) != 2 {
		t.Fatalf("entities = %d", len(got.Entities))
	}
	if !reflect.DeepEqual(got.Entities[0], sampleEntityEntry()) {
		t.Fatalf("entry1 = %+v", got.Entities[0])
	}
	if !reflect.DeepEqual(got.Entities[1], sampleEntityEntry()) {
		t.Fatalf("entry2 = %+v", got.Entities[1])
	}
}

func TestEntityCreateHasNoEntryLen(t *testing.T) {
	b := encodeTestPayload(t, EntityCreate{Entity: sampleEntityEntry()}.Encode)
	if len(b) != EntityEntryWireSize {
		t.Fatalf("entity_create len = %d, want %d (no entryLen)", len(b), EntityEntryWireSize)
	}
	// First bytes are the entity u32, not an entry length.
	if string(b[:4]) != string([]byte{0x04, 0x03, 0x02, 0x01}) {
		t.Fatalf("entity_create starts = % x, want entity u32", b[:4])
	}
	withFuture := append(append([]byte{}, b...), 0xDE, 0xAD, 0xBE, 0xEF)
	got, err := DecodeEntityCreate(NewDecoder(withFuture))
	if err != nil {
		t.Fatalf("trailing bytes: %v", err)
	}
	if !reflect.DeepEqual(got.Entity, sampleEntityEntry()) {
		t.Fatalf("entity = %+v", got.Entity)
	}
}

func TestEntityMoveExactWire(t *testing.T) {
	m := EntityMove{
		Entity: 0x01020304,
		Pos:    Position{X: 1, Y: -1, Z: 0x01020304},
		Angle:  0x0123, Speed: 0x45,
		LastProcessedInputSeq: 0xA1B2C3D4,
	}
	b := encodeTestPayload(t, m.Encode)
	want := []byte{
		0x04, 0x03, 0x02, 0x01, // entity LE
		0x01, 0x00, 0x00, 0x00, // pos.x LE
		0xFF, 0xFF, 0xFF, 0xFF, // pos.y LE
		0x04, 0x03, 0x02, 0x01, // pos.z LE
		0x23, 0x01, // angle LE
		0x45,                   // speed
		0xD4, 0xC3, 0xB2, 0xA1, // lastProcessedInputSeq LE, last
	}
	if string(b) != string(want) {
		t.Fatalf("entity_move wire = % x, want % x", b, want)
	}
	got, err := DecodeEntityMove(NewDecoder(b))
	if err != nil || !reflect.DeepEqual(got, m) {
		t.Fatalf("entity_move decode = %+v, %v", got, err)
	}
}

func TestEntityAngleRange(t *testing.T) {
	b := encodeTestPayload(t, EntityCreate{Entity: EntityEntry{Angle: 4095}}.Encode)
	if _, err := DecodeEntityCreate(NewDecoder(b)); err != nil {
		t.Fatalf("angle 4095: %v", err)
	}
	e := NewEncoder()
	EntityCreate{Entity: EntityEntry{Angle: 4096}}.Encode(e)
	if _, err := e.Bytes(); !errors.Is(err, ErrAngleOutOfRange) {
		t.Fatalf("angle 4096 encode: got %v", err)
	}
	raw := encodeTestPayload(t, func(e *Encoder) {
		EntityEntry{Angle: 4095}.encode(e)
		// Patch the angle field (offset 4+1+2+12=19) to 4096.
	})
	raw[19] = 0x00
	raw[20] = 0x10
	if _, err := DecodeEntityCreate(NewDecoder(raw)); !errors.Is(err, ErrAngleOutOfRange) {
		t.Fatalf("angle 4096 decode: got %v", err)
	}
}

func TestStatEntryExactSize(t *testing.T) {
	if StatEntryWireSize != 17 {
		t.Fatalf("StatEntryWireSize = %d, want 17", StatEntryWireSize)
	}
	v := StatEntry{StatID: 7, Value: -123, Min: math.MinInt32, Max: math.MaxInt32, CurMax: -1}
	b := encodeTestPayload(t, func(e *Encoder) { v.encode(e) })
	want := []byte{
		0x07,                   // statId
		0x85, 0xFF, 0xFF, 0xFF, // value -123 LE
		0x00, 0x00, 0x00, 0x80, // min MinInt32 LE
		0xFF, 0xFF, 0xFF, 0x7F, // max MaxInt32 LE
		0xFF, 0xFF, 0xFF, 0xFF, // curmax -1 LE
	}
	if string(b) != string(want) {
		t.Fatalf("stat wire = % x, want % x", b, want)
	}
}

func TestStatGroupEntryPrefixAndLimits(t *testing.T) {
	m := StatGroup{
		Entity: 0x01020304,
		Stats:  []StatEntry{{StatID: 1, Value: 10, Min: 0, Max: 100, CurMax: 90}},
	}
	b := encodeTestPayload(t, m.Encode)
	wantPrefix := []byte{
		0x04, 0x03, 0x02, 0x01, // entity LE
		0x01, 0x00, // count
		0x11, 0x00, // entryLen = 17 = 0x0011 LE
	}
	if string(b[:8]) != string(wantPrefix) {
		t.Fatalf("stat_group prefix = % x, want % x", b[:8], wantPrefix)
	}

	stats := make([]StatEntry, 1024)
	for i := range stats {
		stats[i] = StatEntry{StatID: uint8(i)}
	}
	big := encodeTestPayload(t, StatGroup{Entity: 1, Stats: stats}.Encode)
	got, err := DecodeStatGroup(NewDecoder(big))
	if err != nil || len(got.Stats) != 1024 {
		t.Fatalf("1024 stats: %v", err)
	}
	e := NewEncoder()
	StatGroup{Entity: 1, Stats: make([]StatEntry, 1025)}.Encode(e)
	if _, err := e.Bytes(); !errors.Is(err, ErrArrayTooLong) {
		t.Fatalf("1025 encode: got %v", err)
	}
	raw := encodeTestPayload(t, func(e *Encoder) {
		e.U32(1)
		e.U16(1025)
	})
	if _, err := DecodeStatGroup(NewDecoder(raw)); !errors.Is(err, ErrArrayTooLong) {
		t.Fatalf("1025 decode: got %v", err)
	}
}

func TestStatGroupEntryForwardCompatibility(t *testing.T) {
	raw := encodeTestPayload(t, func(e *Encoder) {
		e.U32(99)
		e.Count(2, MaxArrayCount)
		e.Entry(func(sub *Encoder) error {
			StatEntry{StatID: 3, Value: 30, Min: 0, Max: 50, CurMax: 30}.encode(sub)
			sub.U16(0xBEEF) // unknown future field
			return nil
		})
		e.Entry(func(sub *Encoder) error {
			StatEntry{StatID: 4, Value: 40, Min: 0, Max: 60, CurMax: 40}.encode(sub)
			return nil
		})
	})
	got, err := DecodeStatGroup(NewDecoder(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := StatGroup{
		Entity: 99,
		Stats: []StatEntry{
			{StatID: 3, Value: 30, Min: 0, Max: 50, CurMax: 30},
			{StatID: 4, Value: 40, Min: 0, Max: 60, CurMax: 40},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestSaidLimitsAndWire(t *testing.T) {
	b := encodeTestPayload(t, Said{From: 0x01020304, Channel: 3, Text: "A"}.Encode)
	want := []byte{
		0x04, 0x03, 0x02, 0x01, // from LE
		0x03,             // channel
		0x01, 0x00, 0x41, // text "A"
	}
	if string(b) != string(want) {
		t.Fatalf("said wire = % x, want % x", b, want)
	}

	ok := strings.Repeat("é", 256) // 512 UTF-8 bytes
	if len(ok) != 512 {
		t.Fatalf("fixture len = %d", len(ok))
	}
	b = encodeTestPayload(t, Said{From: 1, Channel: 1, Text: ok}.Encode)
	got, err := DecodeSaid(NewDecoder(b))
	if err != nil || got.Text != ok {
		t.Fatalf("512-byte said: %v", err)
	}
	e := NewEncoder()
	Said{From: 1, Text: strings.Repeat("x", MaxChatBytes+1)}.Encode(e)
	if _, err := e.Bytes(); !errors.Is(err, ErrStringTooLong) {
		t.Fatalf("513-byte said encode: got %v", err)
	}
	raw := encodeTestPayload(t, func(e *Encoder) {
		e.U32(1)
		e.U8(1)
		e.U16(uint16(MaxChatBytes + 1))
		e.WriteBytes([]byte(strings.Repeat("x", MaxChatBytes+1)))
	})
	if _, err := DecodeSaid(NewDecoder(raw)); !errors.Is(err, ErrStringTooLong) {
		t.Fatalf("513-byte said decode: got %v", err)
	}
}

func TestEffectExactOrdering(t *testing.T) {
	m := Effect{ID: 0x0102, Target: 0x03040506, Pos: Position{X: 7, Y: 8, Z: 9}}
	b := encodeTestPayload(t, m.Encode)
	want := []byte{
		0x02, 0x01, // id LE (before target)
		0x06, 0x05, 0x04, 0x03, // target LE
		0x07, 0x00, 0x00, 0x00, // pos.x
		0x08, 0x00, 0x00, 0x00, // pos.y
		0x09, 0x00, 0x00, 0x00, // pos.z
	}
	if string(b) != string(want) {
		t.Fatalf("effect wire = % x, want % x", b, want)
	}
}

func TestEntityFrameIntegration(t *testing.T) {
	t.Run("CellSnapshot", func(t *testing.T) {
		h := Header{Opcode: OpcodeCellSnapshot, MsgVersion: 1, Seq: 300, Tick: 400}
		want := CellSnapshot{Cell: Cell{X: 1, Z: 2}, Entities: []EntityEntry{sampleEntityEntry()}}
		dec := frameRoundTrip(t, h, want.Encode)
		got, err := DecodeCellSnapshot(dec)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("CellSnapshot = %+v, %v", got, err)
		}
	})
	t.Run("EntityMove", func(t *testing.T) {
		h := Header{Opcode: OpcodeEntityMove, MsgVersion: 1, Seq: 301, Tick: 401}
		want := EntityMove{Entity: 55, Pos: Position{X: 1, Y: 2, Z: 3}, Angle: 100, Speed: 2, LastProcessedInputSeq: 66}
		dec := frameRoundTrip(t, h, want.Encode)
		got, err := DecodeEntityMove(dec)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("EntityMove = %+v, %v", got, err)
		}
	})
	t.Run("StatGroup", func(t *testing.T) {
		h := Header{Opcode: OpcodeStatGroup, MsgVersion: 1, Seq: 302, Tick: 402}
		want := StatGroup{Entity: 7, Stats: []StatEntry{{StatID: 1, Value: 5, Min: 0, Max: 10, CurMax: 9}}}
		dec := frameRoundTrip(t, h, want.Encode)
		got, err := DecodeStatGroup(dec)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("StatGroup = %+v, %v", got, err)
		}
	})
	t.Run("Said", func(t *testing.T) {
		h := Header{Opcode: OpcodeSaid, MsgVersion: 1, Seq: 303, Tick: 403}
		want := Said{From: 8, Channel: 1, Text: "frame chat"}
		dec := frameRoundTrip(t, h, want.Encode)
		got, err := DecodeSaid(dec)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("Said = %+v, %v", got, err)
		}
	})
}

func TestEntityTrailingTolerance(t *testing.T) {
	b := encodeTestPayload(t, EntityMove{Entity: 1, LastProcessedInputSeq: 2}.Encode)
	b = append(b, 0xDE, 0xAD, 0xBE, 0xEF)
	got, err := DecodeEntityMove(NewDecoder(b))
	if err != nil {
		t.Fatalf("entity_move trailing: %v", err)
	}
	if got.Entity != 1 || got.LastProcessedInputSeq != 2 {
		t.Fatalf("entity_move = %+v", got)
	}
	tr := encodeTestPayload(t, TradeResult{OK: 1}.Encode)
	tr = append(tr, 0x09, 0x09)
	trGot, err := DecodeTradeResult(NewDecoder(tr))
	if err != nil || trGot.OK != 1 {
		t.Fatalf("trade_result trailing = %+v, %v", trGot, err)
	}
}

func TestEntityTruncation(t *testing.T) {
	t.Run("SnapshotEntryBody", func(t *testing.T) {
		raw := encodeTestPayload(t, func(e *Encoder) {
			e.Cell(Cell{X: 1, Z: 2})
			e.Count(1, MaxArrayCount)
			e.U16(22)                      // claims a full entry...
			e.WriteBytes(make([]byte, 10)) // ...provides 10 bytes
		})
		if _, err := DecodeCellSnapshot(NewDecoder(raw)); !errors.Is(err, ErrTruncated) {
			t.Fatalf("got %v, want ErrTruncated", err)
		}
	})
	t.Run("EntityCreateSpeed", func(t *testing.T) {
		b := encodeTestPayload(t, EntityCreate{Entity: sampleEntityEntry()}.Encode)
		if _, err := DecodeEntityCreate(NewDecoder(b[:len(b)-1])); !errors.Is(err, ErrTruncated) {
			t.Fatalf("got %v, want ErrTruncated", err)
		}
	})
	t.Run("EntityMoveSeq", func(t *testing.T) {
		b := encodeTestPayload(t, EntityMove{Entity: 1, LastProcessedInputSeq: 2}.Encode)
		if _, err := DecodeEntityMove(NewDecoder(b[:len(b)-2])); !errors.Is(err, ErrTruncated) {
			t.Fatalf("got %v, want ErrTruncated", err)
		}
	})
	t.Run("StatCurMax", func(t *testing.T) {
		b := encodeTestPayload(t, Stat{Entity: 1, Stat: StatEntry{StatID: 1}}.Encode)
		if _, err := DecodeStat(NewDecoder(b[:len(b)-1])); !errors.Is(err, ErrTruncated) {
			t.Fatalf("got %v, want ErrTruncated", err)
		}
	})
	t.Run("StatGroupOverlong", func(t *testing.T) {
		raw := encodeTestPayload(t, func(e *Encoder) {
			e.U32(1)
			e.Count(1, MaxArrayCount)
			e.U16(40)                     // claims 40 entry bytes...
			e.WriteBytes(make([]byte, 5)) // ...provides 5
		})
		if _, err := DecodeStatGroup(NewDecoder(raw)); !errors.Is(err, ErrTruncated) {
			t.Fatalf("got %v, want ErrTruncated", err)
		}
	})
	t.Run("SaidText", func(t *testing.T) {
		raw := encodeTestPayload(t, func(e *Encoder) {
			e.U32(1)
			e.U8(0)
			e.U16(60)
			e.WriteBytes([]byte("short"))
		})
		if _, err := DecodeSaid(NewDecoder(raw)); !errors.Is(err, ErrTruncated) {
			t.Fatalf("got %v, want ErrTruncated", err)
		}
	})
	t.Run("EffectPos", func(t *testing.T) {
		b := encodeTestPayload(t, Effect{ID: 1, Target: 2, Pos: Position{X: 3}}.Encode)
		if _, err := DecodeEffect(NewDecoder(b[:len(b)-4])); !errors.Is(err, ErrTruncated) {
			t.Fatalf("got %v, want ErrTruncated", err)
		}
	})
	t.Run("RespawnPos", func(t *testing.T) {
		raw := encodeTestPayload(t, func(e *Encoder) {
			e.WriteBytes([]byte{0x01, 0x02, 0x03}) // partial i32
		})
		if _, err := DecodeRespawn(NewDecoder(raw)); !errors.Is(err, ErrTruncated) {
			t.Fatalf("got %v, want ErrTruncated", err)
		}
	})
}
