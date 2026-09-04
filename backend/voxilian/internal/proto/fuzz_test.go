package proto

import (
	"encoding/binary"
	"errors"
	"testing"
)

// Fuzz targets: one per concrete message decoder (48) plus the frame
// decoder, per the M2-T5 plan ("Go fuzz targets per decoder"). Thin
// wrappers share fuzzPayloadDecoder; seeds reuse the T4 goldenCases
// table. The primary fuzz property is structural safety (never panic,
// never unbounded allocation, always return); a successful decode must
// additionally consume the whole payload because every production
// decoder skips trailing unknown bytes.

// goldenPayloadByName encodes the canonical T4 payload for a fixture
// filename without touching the checked-in files.
func goldenPayloadByName(filename string) ([]byte, bool) {
	for _, c := range goldenCases {
		if c.Filename == filename {
			e := NewEncoder()
			c.Encode(e)
			p, err := e.Bytes()
			if err != nil {
				return nil, false
			}
			return p, true
		}
	}
	return nil, false
}

// fuzzDecode adapts a typed message decoder to the error-only driver.
func fuzzDecode[T any](fn func(*Decoder) (T, error)) func(*Decoder) error {
	return func(d *Decoder) error {
		_, err := fn(d)
		return err
	}
}

// fuzzPayloadDecoder seeds and drives one message decoder. Inputs above
// the largest payload that could ever arrive through DecodeFrame are
// ignored to save fuzz time; protocol limits themselves are unchanged.
func fuzzPayloadDecoder(f *testing.F, filename string, decode func(*Decoder) error) {
	payload, ok := goldenPayloadByName(filename)
	if !ok {
		f.Fatalf("no golden case for %q", filename)
	}
	seeds := [][]byte{append([]byte{}, payload...), {}}
	if len(payload) > 0 {
		seeds = append(seeds,
			append([]byte{}, payload[:len(payload)-1]...),
			append(append([]byte{}, payload...), 0xDE, 0xAD, 0xBE, 0xEF),
		)
	} else {
		seeds = append(seeds, []byte{0xDE, 0xAD, 0xBE, 0xEF})
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > MaxFrameSize-HeaderSize {
			return
		}
		d := NewDecoder(data)
		if err := decode(d); err != nil {
			return
		}
		if d.Remaining() != 0 {
			t.Fatalf("%s: successful decode left %d bytes", filename, d.Remaining())
		}
	})
}

func FuzzDecodeHello(f *testing.F) {
	fuzzPayloadDecoder(f, "100_hello.hex", fuzzDecode(DecodeHello))
}

func FuzzDecodeReauth(f *testing.F) {
	fuzzPayloadDecoder(f, "101_reauth.hex", fuzzDecode(DecodeReauth))
}

func FuzzDecodeMove(f *testing.F) {
	fuzzPayloadDecoder(f, "102_move.hex", fuzzDecode(DecodeMove))
}

func FuzzDecodeAttack(f *testing.F) {
	fuzzPayloadDecoder(f, "103_attack.hex", fuzzDecode(DecodeAttack))
}

func FuzzDecodeCast(f *testing.F) {
	fuzzPayloadDecoder(f, "104_cast.hex", fuzzDecode(DecodeCast))
}

func FuzzDecodeUse(f *testing.F) {
	fuzzPayloadDecoder(f, "105_use.hex", fuzzDecode(DecodeUse))
}

func FuzzDecodeGet(f *testing.F) {
	fuzzPayloadDecoder(f, "106_get.hex", fuzzDecode(DecodeGet))
}

func FuzzDecodeDrop(f *testing.F) {
	fuzzPayloadDecoder(f, "107_drop.hex", fuzzDecode(DecodeDrop))
}

func FuzzDecodePut(f *testing.F) {
	fuzzPayloadDecoder(f, "108_put.hex", fuzzDecode(DecodePut))
}

func FuzzDecodeGive(f *testing.F) {
	fuzzPayloadDecoder(f, "109_give.hex", fuzzDecode(DecodeGive))
}

func FuzzDecodeOffer(f *testing.F) {
	fuzzPayloadDecoder(f, "110_offer.hex", fuzzDecode(DecodeOffer))
}

func FuzzDecodeCounter(f *testing.F) {
	fuzzPayloadDecoder(f, "111_counter.hex", fuzzDecode(DecodeCounter))
}

func FuzzDecodeAccept(f *testing.F) {
	fuzzPayloadDecoder(f, "112_accept.hex", fuzzDecode(DecodeAccept))
}

func FuzzDecodeCancel(f *testing.F) {
	fuzzPayloadDecoder(f, "113_cancel.hex", fuzzDecode(DecodeCancel))
}

func FuzzDecodeBuy(f *testing.F) {
	fuzzPayloadDecoder(f, "114_buy.hex", fuzzDecode(DecodeBuy))
}

func FuzzDecodeRest(f *testing.F) {
	fuzzPayloadDecoder(f, "115_rest.hex", fuzzDecode(DecodeRest))
}

func FuzzDecodeEat(f *testing.F) {
	fuzzPayloadDecoder(f, "116_eat.hex", fuzzDecode(DecodeEat))
}

func FuzzDecodeSay(f *testing.F) {
	fuzzPayloadDecoder(f, "117_say.hex", fuzzDecode(DecodeSay))
}

func FuzzDecodeSayGroup(f *testing.F) {
	fuzzPayloadDecoder(f, "118_say_group.hex", fuzzDecode(DecodeSayGroup))
}

func FuzzDecodeSafetyToggle(f *testing.F) {
	fuzzPayloadDecoder(f, "119_safety_toggle.hex", fuzzDecode(DecodeSafetyToggle))
}

func FuzzDecodeRespawnAck(f *testing.F) {
	fuzzPayloadDecoder(f, "120_respawn_ack.hex", fuzzDecode(DecodeRespawnAck))
}

func FuzzDecodeCharacterListRequest(f *testing.F) {
	fuzzPayloadDecoder(f, "121_character_list_request.hex", fuzzDecode(DecodeCharacterListRequest))
}

func FuzzDecodeCharacterCreate(f *testing.F) {
	fuzzPayloadDecoder(f, "122_character_create.hex", fuzzDecode(DecodeCharacterCreate))
}

func FuzzDecodeCharacterDelete(f *testing.F) {
	fuzzPayloadDecoder(f, "123_character_delete.hex", fuzzDecode(DecodeCharacterDelete))
}

func FuzzDecodeEnterWorld(f *testing.F) {
	fuzzPayloadDecoder(f, "124_enter_world.hex", fuzzDecode(DecodeEnterWorld))
}

func FuzzDecodeAck(f *testing.F) {
	fuzzPayloadDecoder(f, "125_ack.hex", fuzzDecode(DecodeAck))
}

func FuzzDecodeLeaveWorld(f *testing.F) {
	fuzzPayloadDecoder(f, "126_leave_world.hex", fuzzDecode(DecodeLeaveWorld))
}

func FuzzDecodeWelcome(f *testing.F) {
	fuzzPayloadDecoder(f, "200_welcome.hex", fuzzDecode(DecodeWelcome))
}

func FuzzDecodeReauthOK(f *testing.F) {
	fuzzPayloadDecoder(f, "201_reauth_ok.hex", fuzzDecode(DecodeReauthOK))
}

func FuzzDecodeErrorMessage(f *testing.F) {
	fuzzPayloadDecoder(f, "202_error.hex", fuzzDecode(DecodeErrorMessage))
}

func FuzzDecodeCellSnapshot(f *testing.F) {
	fuzzPayloadDecoder(f, "203_cell_snapshot.hex", fuzzDecode(DecodeCellSnapshot))
}

func FuzzDecodeEntityCreate(f *testing.F) {
	fuzzPayloadDecoder(f, "204_entity_create.hex", fuzzDecode(DecodeEntityCreate))
}

func FuzzDecodeEntityMove(f *testing.F) {
	fuzzPayloadDecoder(f, "205_entity_move.hex", fuzzDecode(DecodeEntityMove))
}

func FuzzDecodeEntityRemove(f *testing.F) {
	fuzzPayloadDecoder(f, "206_entity_remove.hex", fuzzDecode(DecodeEntityRemove))
}

func FuzzDecodeStat(f *testing.F) {
	fuzzPayloadDecoder(f, "207_stat.hex", fuzzDecode(DecodeStat))
}

func FuzzDecodeStatGroup(f *testing.F) {
	fuzzPayloadDecoder(f, "208_stat_group.hex", fuzzDecode(DecodeStatGroup))
}

func FuzzDecodeSaid(f *testing.F) {
	fuzzPayloadDecoder(f, "209_said.hex", fuzzDecode(DecodeSaid))
}

func FuzzDecodeEffect(f *testing.F) {
	fuzzPayloadDecoder(f, "210_effect.hex", fuzzDecode(DecodeEffect))
}

func FuzzDecodeInventoryDelta(f *testing.F) {
	fuzzPayloadDecoder(f, "211_inventory_delta.hex", fuzzDecode(DecodeInventoryDelta))
}

func FuzzDecodeOfferUpdate(f *testing.F) {
	fuzzPayloadDecoder(f, "212_offer_update.hex", fuzzDecode(DecodeOfferUpdate))
}

func FuzzDecodeTradeResult(f *testing.F) {
	fuzzPayloadDecoder(f, "213_trade_result.hex", fuzzDecode(DecodeTradeResult))
}

func FuzzDecodeDeath(f *testing.F) {
	fuzzPayloadDecoder(f, "214_death.hex", fuzzDecode(DecodeDeath))
}

func FuzzDecodeRespawn(f *testing.F) {
	fuzzPayloadDecoder(f, "215_respawn.hex", fuzzDecode(DecodeRespawn))
}

func FuzzDecodeCharacterList(f *testing.F) {
	fuzzPayloadDecoder(f, "216_character_list_result.hex", fuzzDecode(DecodeCharacterList))
}

func FuzzDecodeCharacterOp(f *testing.F) {
	fuzzPayloadDecoder(f, "217_character_op.hex", fuzzDecode(DecodeCharacterOp))
}

func FuzzDecodeChunkFragment(f *testing.F) {
	fuzzPayloadDecoder(f, "218_chunk_fragment.hex", fuzzDecode(DecodeChunkFragment))
}

func FuzzDecodeWorldReady(f *testing.F) {
	fuzzPayloadDecoder(f, "219_world_ready.hex", fuzzDecode(DecodeWorldReady))
}

func FuzzDecodeShopList(f *testing.F) {
	fuzzPayloadDecoder(f, "220_shop_list.hex", fuzzDecode(DecodeShopList))
}

// FuzzDecodeFrame fuzzes complete arbitrary frames. Oversized input
// must fail with ErrFrameTooLarge before parsing, short input with
// ErrTruncated, and anything in between must parse structurally (the
// frame layer has no opcode registry) with the header matching an
// independent little-endian read.
func FuzzDecodeFrame(f *testing.F) {
	for _, c := range goldenCases {
		frame, err := EncodeFrame(c.Header, func(e *Encoder) error {
			c.Encode(e)
			return e.Err()
		})
		if err != nil {
			f.Fatalf("seed %s: %v", c.Filename, err)
		}
		f.Add(frame)
	}
	f.Add([]byte{})
	f.Add(make([]byte, 11))
	f.Add(make([]byte, 12))
	f.Add(make([]byte, MaxFrameSize))
	f.Add(make([]byte, MaxFrameSize+1))
	f.Fuzz(func(t *testing.T, data []byte) {
		header, dec, err := DecodeFrame(data)
		switch {
		case len(data) > MaxFrameSize:
			if !errors.Is(err, ErrFrameTooLarge) {
				t.Fatalf("len %d: got %v, want ErrFrameTooLarge", len(data), err)
			}
		case len(data) < HeaderSize:
			if !errors.Is(err, ErrTruncated) {
				t.Fatalf("len %d: got %v, want ErrTruncated", len(data), err)
			}
		default:
			if err != nil {
				t.Fatalf("len %d: %v", len(data), err)
			}
			if dec.Remaining() != len(data)-HeaderSize {
				t.Fatalf("remaining = %d, want %d", dec.Remaining(), len(data)-HeaderSize)
			}
			if header.Opcode != binary.LittleEndian.Uint16(data[0:2]) ||
				header.MsgVersion != binary.LittleEndian.Uint16(data[2:4]) ||
				header.Seq != binary.LittleEndian.Uint32(data[4:8]) ||
				header.Tick != binary.LittleEndian.Uint32(data[8:12]) {
				t.Fatalf("header %+v mismatches LE bytes % x", header, data[:12])
			}
		}
	})
}
