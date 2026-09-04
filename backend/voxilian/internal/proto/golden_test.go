package proto

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/dlukt/voxilian/internal/simtest"
)

// Cross-language golden vectors: one complete frame per opcode,
// checked in under repo-root testdata/protocol/. The checked-in bytes
// are the stable binary contract for the Godot client plan.
//
// Canonical fixture header (test data only — MsgVersion 0 here does NOT
// declare a deployment message version, and the Seq/Tick values are
// arbitrary fixed test values, not runtime requirements):
//
//	Header{Opcode: <opcode>, MsgVersion: 0, Seq: 0x01020304, Tick: 0x05060708}

// goldenHeader returns the canonical header for a fixture opcode.
func goldenHeader(opcode uint16) Header {
	return Header{Opcode: opcode, MsgVersion: 0, Seq: 0x01020304, Tick: 0x05060708}
}

// goldenCase is one fixture: its filename, header, payload encoder,
// payload decoder, and independently hard-coded expected value.
type goldenCase struct {
	Filename string
	Header   Header
	Encode   func(*Encoder)
	Decode   func(*Decoder) (any, error)
	Want     any
}

// dec adapts a typed message decoder to the golden table.
func dec[T any](fn func(*Decoder) (T, error)) func(*Decoder) (any, error) {
	return func(d *Decoder) (any, error) {
		m, err := fn(d)
		return m, err
	}
}

// goldenCases is the single hard-coded canonical table. A temporary
// generator (removed before commit) encoded these same values to emit
// the .hex files; the tests below validate the checked-in bytes in
// both directions against this table.
var goldenCases = []goldenCase{
	{
		Filename: "100_hello.hex", Header: goldenHeader(OpcodeHello),
		Encode: Hello{ClientVersion: 0x11223344, ProtoVersion: 0x5566, AccessToken: "tok-é"}.Encode,
		Decode: dec(DecodeHello),
		Want:   Hello{ClientVersion: 0x11223344, ProtoVersion: 0x5566, AccessToken: "tok-é"},
	},
	{
		Filename: "101_reauth.hex", Header: goldenHeader(OpcodeReauth),
		Encode: Reauth{AccessToken: "reauth-token"}.Encode,
		Decode: dec(DecodeReauth),
		Want:   Reauth{AccessToken: "reauth-token"},
	},
	{
		Filename: "102_move.hex", Header: goldenHeader(OpcodeMove),
		Encode: Move{InputSeq: 0xA1B2C3D4, HeldDirs: 0x05, RunFlag: 1, Yaw: 0x0123}.Encode,
		Decode: dec(DecodeMove),
		Want:   Move{InputSeq: 0xA1B2C3D4, HeldDirs: 0x05, RunFlag: 1, Yaw: 0x0123},
	},
	{
		Filename: "103_attack.hex", Header: goldenHeader(OpcodeAttack),
		Encode: Attack{Target: 0x01020304}.Encode,
		Decode: dec(DecodeAttack),
		Want:   Attack{Target: 0x01020304},
	},
	{
		Filename: "104_cast.hex", Header: goldenHeader(OpcodeCast),
		Encode: Cast{Spell: 0x0506, Target: 0x0708090A}.Encode,
		Decode: dec(DecodeCast),
		Want:   Cast{Spell: 0x0506, Target: 0x0708090A},
	},
	{
		// Skill namespace, proving the ID carrier stays fixed u32.
		Filename: "105_use.hex", Header: goldenHeader(OpcodeUse),
		Encode: Use{Kind: UseKindSkill, ID: 0x00001234}.Encode,
		Decode: dec(DecodeUse),
		Want:   Use{Kind: UseKindSkill, ID: 0x00001234},
	},
	{
		Filename: "106_get.hex", Header: goldenHeader(OpcodeGet),
		Encode: Get{Entity: 0x01020304, Item: 0x05060708}.Encode,
		Decode: dec(DecodeGet),
		Want:   Get{Entity: 0x01020304, Item: 0x05060708},
	},
	{
		Filename: "107_drop.hex", Header: goldenHeader(OpcodeDrop),
		Encode: Drop{Item: 0x11223344}.Encode,
		Decode: dec(DecodeDrop),
		Want:   Drop{Item: 0x11223344},
	},
	{
		Filename: "108_put.hex", Header: goldenHeader(OpcodePut),
		Encode: Put{Item: 0x01020304, Container: 0xA1B2C3D4}.Encode,
		Decode: dec(DecodePut),
		Want:   Put{Item: 0x01020304, Container: 0xA1B2C3D4},
	},
	{
		Filename: "109_give.hex", Header: goldenHeader(OpcodeGive),
		Encode: Give{Target: 0x01020304, Item: 0x05060708, Qty: 0x090A}.Encode,
		Decode: dec(DecodeGive),
		Want:   Give{Target: 0x01020304, Item: 0x05060708, Qty: 0x090A},
	},
	{
		Filename: "110_offer.hex", Header: goldenHeader(OpcodeOffer),
		Encode: Offer{Target: 0x01020304, Items: []uint32{0x05060708, 0xA1B2C3D4}}.Encode,
		Decode: dec(DecodeOffer),
		Want:   Offer{Target: 0x01020304, Items: []uint32{0x05060708, 0xA1B2C3D4}},
	},
	{
		Filename: "111_counter.hex", Header: goldenHeader(OpcodeCounter),
		Encode: Counter{Items: []uint32{0x01020304, 0x05060708}}.Encode,
		Decode: dec(DecodeCounter),
		Want:   Counter{Items: []uint32{0x01020304, 0x05060708}},
	},
	{
		Filename: "112_accept.hex", Header: goldenHeader(OpcodeAccept),
		Encode: Accept{}.Encode, Decode: dec(DecodeAccept), Want: Accept{},
	},
	{
		Filename: "113_cancel.hex", Header: goldenHeader(OpcodeCancel),
		Encode: Cancel{}.Encode, Decode: dec(DecodeCancel), Want: Cancel{},
	},
	{
		Filename: "114_buy.hex", Header: goldenHeader(OpcodeBuy),
		Encode: Buy{Vendor: 0x01020304, Listing: 0x0506, Qty: 0x0708}.Encode,
		Decode: dec(DecodeBuy),
		Want:   Buy{Vendor: 0x01020304, Listing: 0x0506, Qty: 0x0708},
	},
	{
		// Deliberately raw non-boolean state value.
		Filename: "115_rest.hex", Header: goldenHeader(OpcodeRest),
		Encode: Rest{State: 0xFF}.Encode,
		Decode: dec(DecodeRest),
		Want:   Rest{State: 0xFF},
	},
	{
		Filename: "116_eat.hex", Header: goldenHeader(OpcodeEat),
		Encode: Eat{Item: 0x01020304}.Encode,
		Decode: dec(DecodeEat),
		Want:   Eat{Item: 0x01020304},
	},
	{
		Filename: "117_say.hex", Header: goldenHeader(OpcodeSay),
		Encode: Say{Channel: 3, Text: "hé"}.Encode,
		Decode: dec(DecodeSay),
		Want:   Say{Channel: 3, Text: "hé"},
	},
	{
		Filename: "118_say_group.hex", Header: goldenHeader(OpcodeSayGroup),
		Encode: SayGroup{Text: "grüp"}.Encode,
		Decode: dec(DecodeSayGroup),
		Want:   SayGroup{Text: "grüp"},
	},
	{
		Filename: "119_safety_toggle.hex", Header: goldenHeader(OpcodeSafetyToggle),
		Encode: SafetyToggle{}.Encode, Decode: dec(DecodeSafetyToggle), Want: SafetyToggle{},
	},
	{
		Filename: "120_respawn_ack.hex", Header: goldenHeader(OpcodeRespawnAck),
		Encode: RespawnAck{}.Encode, Decode: dec(DecodeRespawnAck), Want: RespawnAck{},
	},
	{
		Filename: "121_character_list_request.hex", Header: goldenHeader(OpcodeCharacterList),
		Encode: CharacterListRequest{}.Encode, Decode: dec(DecodeCharacterListRequest), Want: CharacterListRequest{},
	},
	{
		Filename: "122_character_create.hex", Header: goldenHeader(OpcodeCharacterCreate),
		Encode: CharacterCreate{
			Slot: 1, Name: "Zoë", Gender: 2,
			Face:   CharacterFace{HairStyle: 3, HairColor: 4, SkinTone: 5, Parts: [5]uint8{6, 7, 8, 9, 10}},
			Stats:  [6]uint8{11, 12, 13, 14, 15, 16},
			Spells: []uint16{0x1111, 0x2222},
			Skills: []uint16{0x3333},
		}.Encode,
		Decode: dec(DecodeCharacterCreate),
		Want: CharacterCreate{
			Slot: 1, Name: "Zoë", Gender: 2,
			Face:   CharacterFace{HairStyle: 3, HairColor: 4, SkinTone: 5, Parts: [5]uint8{6, 7, 8, 9, 10}},
			Stats:  [6]uint8{11, 12, 13, 14, 15, 16},
			Spells: []uint16{0x1111, 0x2222},
			Skills: []uint16{0x3333},
		},
	},
	{
		Filename: "123_character_delete.hex", Header: goldenHeader(OpcodeCharacterDelete),
		Encode: CharacterDelete{Slot: 1}.Encode,
		Decode: dec(DecodeCharacterDelete),
		Want:   CharacterDelete{Slot: 1},
	},
	{
		Filename: "124_enter_world.hex", Header: goldenHeader(OpcodeEnterWorld),
		Encode: EnterWorld{Slot: 0}.Encode,
		Decode: dec(DecodeEnterWorld),
		Want:   EnterWorld{Slot: 0},
	},
	{
		Filename: "125_ack.hex", Header: goldenHeader(OpcodeAck),
		Encode: Ack{AckSeq: 0xA1B2C3D4}.Encode,
		Decode: dec(DecodeAck),
		Want:   Ack{AckSeq: 0xA1B2C3D4},
	},
	{
		Filename: "126_leave_world.hex", Header: goldenHeader(OpcodeLeaveWorld),
		Encode: LeaveWorld{}.Encode, Decode: dec(DecodeLeaveWorld), Want: LeaveWorld{},
	},
	{
		Filename: "200_welcome.hex", Header: goldenHeader(OpcodeWelcome),
		Encode: Welcome{
			ServerTimeMs: 0x0102030405060708, Chunk: 16, AOIRadius: 96,
			TickRates: []uint16{20, 10},
			World:     WorldInfo{Mode: 1, Seed: 0x1112131415161718, Version: 0x21222324},
		}.Encode,
		Decode: dec(DecodeWelcome),
		Want: Welcome{
			ServerTimeMs: 0x0102030405060708, Chunk: 16, AOIRadius: 96,
			TickRates: []uint16{20, 10},
			World:     WorldInfo{Mode: 1, Seed: 0x1112131415161718, Version: 0x21222324},
		},
	},
	{
		Filename: "201_reauth_ok.hex", Header: goldenHeader(OpcodeReauthOK),
		Encode: ReauthOK{}.Encode, Decode: dec(DecodeReauthOK), Want: ReauthOK{},
	},
	{
		Filename: "202_error.hex", Header: goldenHeader(OpcodeError),
		Encode: ErrorMessage{Code: 0x1234, Message: "oops-é"}.Encode,
		Decode: dec(DecodeErrorMessage),
		Want:   ErrorMessage{Code: 0x1234, Message: "oops-é"},
	},
	{
		Filename: "203_cell_snapshot.hex", Header: goldenHeader(OpcodeCellSnapshot),
		Encode: CellSnapshot{
			Cell: Cell{X: -2, Z: 3},
			Entities: []EntityEntry{{
				Entity: 0x01020304, Kind: 2, Proto: 0x0506,
				Pos:   Position{X: -1000, Y: 2000, Z: -3000},
				Angle: 0x0123, Speed: 7,
			}},
		}.Encode,
		Decode: dec(DecodeCellSnapshot),
		Want: CellSnapshot{
			Cell: Cell{X: -2, Z: 3},
			Entities: []EntityEntry{{
				Entity: 0x01020304, Kind: 2, Proto: 0x0506,
				Pos:   Position{X: -1000, Y: 2000, Z: -3000},
				Angle: 0x0123, Speed: 7,
			}},
		},
	},
	{
		// Single entry: no entryLen wrapper.
		Filename: "204_entity_create.hex", Header: goldenHeader(OpcodeEntityCreate),
		Encode: EntityCreate{Entity: EntityEntry{
			Entity: 0x01020304, Kind: 3, Proto: 0x0506,
			Pos:   Position{X: -1, Y: 2, Z: -3},
			Angle: 0x0123, Speed: 4,
		}}.Encode,
		Decode: dec(DecodeEntityCreate),
		Want: EntityCreate{Entity: EntityEntry{
			Entity: 0x01020304, Kind: 3, Proto: 0x0506,
			Pos:   Position{X: -1, Y: 2, Z: -3},
			Angle: 0x0123, Speed: 4,
		}},
	},
	{
		Filename: "205_entity_move.hex", Header: goldenHeader(OpcodeEntityMove),
		Encode: EntityMove{
			Entity: 0x01020304,
			Pos:    Position{X: 1234, Y: -5678, Z: 9012},
			Angle:  0x0123, Speed: 7, LastProcessedInputSeq: 0xA1B2C3D4,
		}.Encode,
		Decode: dec(DecodeEntityMove),
		Want: EntityMove{
			Entity: 0x01020304,
			Pos:    Position{X: 1234, Y: -5678, Z: 9012},
			Angle:  0x0123, Speed: 7, LastProcessedInputSeq: 0xA1B2C3D4,
		},
	},
	{
		Filename: "206_entity_remove.hex", Header: goldenHeader(OpcodeEntityRemove),
		Encode: EntityRemove{Entity: 0xA1B2C3D4}.Encode,
		Decode: dec(DecodeEntityRemove),
		Want:   EntityRemove{Entity: 0xA1B2C3D4},
	},
	{
		Filename: "207_stat.hex", Header: goldenHeader(OpcodeStat),
		Encode: Stat{
			Entity: 0x01020304,
			Stat:   StatEntry{StatID: 3, Value: -123, Min: -200, Max: 1000, CurMax: 999},
		}.Encode,
		Decode: dec(DecodeStat),
		Want: Stat{
			Entity: 0x01020304,
			Stat:   StatEntry{StatID: 3, Value: -123, Min: -200, Max: 1000, CurMax: 999},
		},
	},
	{
		Filename: "208_stat_group.hex", Header: goldenHeader(OpcodeStatGroup),
		Encode: StatGroup{
			Entity: 0x01020304,
			Stats: []StatEntry{
				{StatID: 1, Value: 10, Min: 0, Max: 20, CurMax: 15},
				{StatID: 2, Value: -5, Min: -10, Max: 30, CurMax: 25},
			},
		}.Encode,
		Decode: dec(DecodeStatGroup),
		Want: StatGroup{
			Entity: 0x01020304,
			Stats: []StatEntry{
				{StatID: 1, Value: 10, Min: 0, Max: 20, CurMax: 15},
				{StatID: 2, Value: -5, Min: -10, Max: 30, CurMax: 25},
			},
		},
	},
	{
		Filename: "209_said.hex", Header: goldenHeader(OpcodeSaid),
		Encode: Said{From: 0x01020304, Channel: 3, Text: "hé"}.Encode,
		Decode: dec(DecodeSaid),
		Want:   Said{From: 0x01020304, Channel: 3, Text: "hé"},
	},
	{
		Filename: "210_effect.hex", Header: goldenHeader(OpcodeEffect),
		Encode: Effect{
			ID: 0x0506, Target: 0x01020304,
			Pos: Position{X: -1, Y: 2, Z: -3},
		}.Encode,
		Decode: dec(DecodeEffect),
		Want: Effect{
			ID: 0x0506, Target: 0x01020304,
			Pos: Position{X: -1, Y: 2, Z: -3},
		},
	},
	{
		Filename: "211_inventory_delta.hex", Header: goldenHeader(OpcodeInventoryDelta),
		Encode: InventoryDelta{Items: []InventoryEntry{
			{Item: 0x01020304, Proto: 0x0506, Qty: 7, Hits: -1,
				Location: InventoryLocationDirect, Container: 0, Slot: "main"},
			{Item: 0x11121314, Proto: 0x1516, Qty: 0x17, Hits: 1234,
				Location: InventoryLocationContainer, Container: 0x01020304, Slot: "bag"},
		}}.Encode,
		Decode: dec(DecodeInventoryDelta),
		Want: InventoryDelta{Items: []InventoryEntry{
			{Item: 0x01020304, Proto: 0x0506, Qty: 7, Hits: -1,
				Location: InventoryLocationDirect, Container: 0, Slot: "main"},
			{Item: 0x11121314, Proto: 0x1516, Qty: 0x17, Hits: 1234,
				Location: InventoryLocationContainer, Container: 0x01020304, Slot: "bag"},
		}},
	},
	{
		Filename: "212_offer_update.hex", Header: goldenHeader(OpcodeOfferUpdate),
		Encode: OfferUpdate{
			With: 0x01020304, State: 0xFF,
			Items: []OfferItem{
				{Item: 0x05060708, Qty: 9},
				{Item: 0x0A0B0C0D, Qty: 0x0E0F},
			},
		}.Encode,
		Decode: dec(DecodeOfferUpdate),
		Want: OfferUpdate{
			With: 0x01020304, State: 0xFF,
			Items: []OfferItem{
				{Item: 0x05060708, Qty: 9},
				{Item: 0x0A0B0C0D, Qty: 0x0E0F},
			},
		},
	},
	{
		// Deliberately raw non-boolean OK value.
		Filename: "213_trade_result.hex", Header: goldenHeader(OpcodeTradeResult),
		Encode: TradeResult{OK: 0xFF}.Encode,
		Decode: dec(DecodeTradeResult),
		Want:   TradeResult{OK: 0xFF},
	},
	{
		Filename: "214_death.hex", Header: goldenHeader(OpcodeDeath),
		Encode: Death{Victim: 0xA1B2C3D4}.Encode,
		Decode: dec(DecodeDeath),
		Want:   Death{Victim: 0xA1B2C3D4},
	},
	{
		Filename: "215_respawn.hex", Header: goldenHeader(OpcodeRespawn),
		Encode: Respawn{Pos: Position{X: -1, Y: 2, Z: -3}}.Encode,
		Decode: dec(DecodeRespawn),
		Want:   Respawn{Pos: Position{X: -1, Y: 2, Z: -3}},
	},
	{
		Filename: "216_character_list_result.hex", Header: goldenHeader(OpcodeCharacterListResult),
		Encode: CharacterList{Characters: []CharacterSummary{
			{Slot: 0, CharName: "Ada", Level: 20},
			{Slot: 1, CharName: "Zoë", Level: 42},
		}}.Encode,
		Decode: dec(DecodeCharacterList),
		Want: CharacterList{Characters: []CharacterSummary{
			{Slot: 0, CharName: "Ada", Level: 20},
			{Slot: 1, CharName: "Zoë", Level: 42},
		}},
	},
	{
		// Deliberately raw values; no numeric op/result mapping exists.
		Filename: "217_character_op.hex", Header: goldenHeader(OpcodeCharacterOp),
		Encode: CharacterOp{Op: 0xA5, OK: 0xFF}.Encode,
		Decode: dec(DecodeCharacterOp),
		Want:   CharacterOp{Op: 0xA5, OK: 0xFF},
	},
	{
		Filename: "218_chunk_fragment.hex", Header: goldenHeader(OpcodeChunkFragment),
		Encode: ChunkFragment{
			Cell:     Cell{X: -2, Z: 3},
			ChunkIdx: 0x01020304, FragIdx: 0x0506, FragCount: 0x0708,
			Bytes: []byte{0xAA, 0xBB, 0xCC},
		}.Encode,
		Decode: dec(DecodeChunkFragment),
		Want: ChunkFragment{
			Cell:     Cell{X: -2, Z: 3},
			ChunkIdx: 0x01020304, FragIdx: 0x0506, FragCount: 0x0708,
			Bytes: []byte{0xAA, 0xBB, 0xCC},
		},
	},
	{
		Filename: "219_world_ready.hex", Header: goldenHeader(OpcodeWorldReady),
		Encode: WorldReady{}.Encode, Decode: dec(DecodeWorldReady), Want: WorldReady{},
	},
	{
		Filename: "220_shop_list.hex", Header: goldenHeader(OpcodeShopList),
		Encode: ShopList{
			Vendor: 0x01020304,
			Listings: []ShopListingEntry{
				{Listing: 0x0506, Price: 0x0708090A, Qty: 0x0B0C},
				{Listing: 0x0D0E, Price: 0x0F101112, Qty: 0x1314},
			},
		}.Encode,
		Decode: dec(DecodeShopList),
		Want: ShopList{
			Vendor: 0x01020304,
			Listings: []ShopListingEntry{
				{Listing: 0x0506, Price: 0x0708090A, Qty: 0x0B0C},
				{Listing: 0x0D0E, Price: 0x0F101112, Qty: 0x1314},
			},
		},
	},
}

// TestGoldenVectors validates every checked-in fixture in both
// directions: fixture bytes decode to the hard-coded expected value,
// and the hard-coded expected value re-encodes byte-identically.
// Tests fail on mismatch; fixtures are never rewritten by tests.
func TestGoldenVectors(t *testing.T) {
	if len(goldenCases) != 48 {
		t.Fatalf("golden cases = %d, want 48", len(goldenCases))
	}
	for _, c := range goldenCases {
		t.Run(c.Filename, func(t *testing.T) {
			raw := simtest.ProtocolGolden(t, c.Filename)

			// A: checked-in fixture -> decoder.
			header, dec, err := DecodeFrame(raw)
			if err != nil {
				t.Fatalf("DecodeFrame: %v", err)
			}
			if header != c.Header {
				t.Fatalf("header = %+v, want %+v", header, c.Header)
			}
			got, err := c.Decode(dec)
			if err != nil {
				t.Fatalf("payload decode: %v", err)
			}
			if !reflect.DeepEqual(got, c.Want) {
				t.Fatalf("decoded = %#v, want %#v", got, c.Want)
			}

			// B: canonical Go encoder -> checked-in fixture bytes.
			frame, err := EncodeFrame(c.Header, func(e *Encoder) error {
				c.Encode(e)
				return e.Err()
			})
			if err != nil {
				t.Fatalf("EncodeFrame: %v", err)
			}
			if string(frame) != string(raw) {
				t.Fatalf("re-encoded %d bytes differ from fixture %d bytes", len(frame), len(raw))
			}
		})
	}
}

// TestGoldenFixtureCoverage asserts the fixture directory holds exactly
// the 48 real vectors, that filename opcodes match frame opcodes, that
// every vector uses the canonical header, and that the underscore
// harness sentinel is excluded from protocol discovery.
func TestGoldenFixtureCoverage(t *testing.T) {
	dir := filepath.Join(simtest.RepoRoot(t), "testdata", "protocol")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read fixture dir: %v", err)
	}
	var real []string
	sawSentinel := false
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".hex") {
			continue
		}
		if strings.HasPrefix(name, "_") {
			if name == "_harness_example.hex" {
				sawSentinel = true
			}
			continue
		}
		real = append(real, name)
	}
	if !sawSentinel {
		t.Fatalf("harness sentinel _harness_example.hex missing")
	}
	if len(real) != 48 {
		t.Fatalf("real fixtures = %d (%v), want 48", len(real), real)
	}
	wantNames := make(map[string]uint16, len(goldenCases))
	for _, c := range goldenCases {
		wantNames[c.Filename] = c.Header.Opcode
	}
	if len(wantNames) != 48 {
		t.Fatalf("golden table names = %d, want 48 unique", len(wantNames))
	}
	for _, name := range real {
		wantOp, ok := wantNames[name]
		if !ok {
			t.Fatalf("unexpected fixture file %q", name)
		}
		// Filename opcode must equal the decoded frame opcode.
		num := strings.SplitN(name, "_", 2)[0]
		n, err := strconv.Atoi(num)
		if err != nil {
			t.Fatalf("fixture %q has non-numeric prefix: %v", name, err)
		}
		if uint16(n) != wantOp {
			t.Fatalf("fixture %q prefix opcode %d != table opcode %d", name, n, wantOp)
		}
		raw := simtest.ProtocolGolden(t, name)
		header, _, err := DecodeFrame(raw)
		if err != nil {
			t.Fatalf("fixture %q: DecodeFrame: %v", name, err)
		}
		if header.Opcode != wantOp {
			t.Fatalf("fixture %q frame opcode %d != %d", name, header.Opcode, wantOp)
		}
		if header.MsgVersion != 0 || header.Seq != 0x01020304 || header.Tick != 0x05060708 {
			t.Fatalf("fixture %q header = %+v, want canonical test header", name, header)
		}
	}
	// Opcode coverage must be exactly 100–126 and 200–220 with no gaps.
	covered := make(map[uint16]bool)
	for _, c := range goldenCases {
		covered[c.Header.Opcode] = true
	}
	for op := uint16(100); op <= 126; op++ {
		if !covered[op] {
			t.Fatalf("opcode %d missing fixture", op)
		}
	}
	for op := uint16(200); op <= 220; op++ {
		if !covered[op] {
			t.Fatalf("opcode %d missing fixture", op)
		}
	}
	if len(covered) != 48 {
		t.Fatalf("covered opcodes = %d, want 48", len(covered))
	}
}

// TestGoldenSentinelIgnored proves the harness sentinel is not a
// protocol vector: it must never decode as a Voxilian frame.
func TestGoldenSentinelIgnored(t *testing.T) {
	raw := simtest.ProtocolGolden(t, "_harness_example.hex")
	if len(raw) != 4 {
		t.Fatalf("sentinel len = %d, want 4", len(raw))
	}
	if _, _, err := DecodeFrame(raw); !errors.Is(err, ErrTruncated) {
		t.Fatalf("sentinel DecodeFrame: got %v, want ErrTruncated", err)
	}
}

// TestGoldenCriticalFraming pins the framing distinctions most likely
// to regress symmetrically, read back from the checked-in bytes.
func TestGoldenCriticalFraming(t *testing.T) {
	fixture := func(t *testing.T, name string) []byte {
		t.Helper()
		return simtest.ProtocolGolden(t, name)
	}
	t.Run("UseFixedU32", func(t *testing.T) {
		raw := fixture(t, "105_use.hex")
		// Header(12) + kind(1) + id(4): ID still occupies four bytes.
		if len(raw) != 12+1+4 {
			t.Fatalf("105 len = %d, want 17", len(raw))
		}
		if raw[12] != 0x00 || string(raw[13:17]) != string([]byte{0x34, 0x12, 0x00, 0x00}) {
			t.Fatalf("105 payload = % x", raw[12:])
		}
	})
	t.Run("SnapshotEntryFramed", func(t *testing.T) {
		raw := fixture(t, "203_cell_snapshot.hex")
		// Header(12) + cell(8) + count(2) -> entryLen 22 = 0x0016.
		if string(raw[22:24]) != string([]byte{0x16, 0x00}) {
			t.Fatalf("203 entryLen = % x", raw[22:24])
		}
	})
	t.Run("CreateDirectEntry", func(t *testing.T) {
		raw := fixture(t, "204_entity_create.hex")
		// No entryLen wrapper: exactly header + 22-byte entry.
		if len(raw) != 12+EntityEntryWireSize {
			t.Fatalf("204 len = %d, want %d", len(raw), 12+EntityEntryWireSize)
		}
	})
	t.Run("StatGroupFramed", func(t *testing.T) {
		raw := fixture(t, "208_stat_group.hex")
		body := string(raw[12:])
		if strings.Count(body, string([]byte{0x11, 0x00})) != 2 {
			t.Fatalf("208 should contain two 11 00 stat prefixes: % x", raw)
		}
	})
	t.Run("InventoryFramed", func(t *testing.T) {
		raw := fixture(t, "211_inventory_delta.hex")
		body := string(raw[12:])
		// Entry 1: 19 + len("main")=4 -> 23 = 0x17; slot bytes present.
		if !strings.Contains(body, string([]byte{0x17, 0x00})) {
			t.Fatalf("211 missing 17 00 entry prefix: % x", raw)
		}
		if !strings.Contains(body, "main") || !strings.Contains(body, "bag") {
			t.Fatalf("211 missing slot strings: % x", raw)
		}
	})
	t.Run("OfferUpdateFramed", func(t *testing.T) {
		raw := fixture(t, "212_offer_update.hex")
		if strings.Count(string(raw[12:]), string([]byte{0x06, 0x00})) != 2 {
			t.Fatalf("212 should contain two 06 00 item prefixes: % x", raw)
		}
	})
	t.Run("ChunkByteLen", func(t *testing.T) {
		raw := fixture(t, "218_chunk_fragment.hex")
		tail := raw[len(raw)-5:]
		if string(tail) != string([]byte{0x03, 0x00, 0xAA, 0xBB, 0xCC}) {
			t.Fatalf("218 tail = % x", tail)
		}
	})
	t.Run("ShopFramed", func(t *testing.T) {
		raw := fixture(t, "220_shop_list.hex")
		if strings.Count(string(raw[12:]), string([]byte{0x08, 0x00})) != 2 {
			t.Fatalf("220 should contain two 08 00 listing prefixes: % x", raw)
		}
	})
	t.Run("EmptyFramesAreHeaderOnly", func(t *testing.T) {
		for _, name := range []string{
			"112_accept.hex", "113_cancel.hex",
			"119_safety_toggle.hex", "120_respawn_ack.hex",
			"121_character_list_request.hex", "126_leave_world.hex",
			"201_reauth_ok.hex", "219_world_ready.hex",
		} {
			if got := len(fixture(t, name)); got != HeaderSize {
				t.Fatalf("%s len = %d, want %d", name, got, HeaderSize)
			}
		}
	})
}
