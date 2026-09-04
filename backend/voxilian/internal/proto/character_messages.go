package proto

import "fmt"

// Character lifecycle messages: opcodes 121–126 (C→S) and 216, 217, 219
// (S→C). This is a wire codec only: no session-state enforcement, no
// display-name rules, no slot/stat/budget validation, no mapping of
// CharacterOp raw values to named operations. Structural widths,
// string wire limits/UTF-8, and array count limits are the only checks.

// CharacterListRequest is opcode 121 (C→S): empty request; the server
// answers with opcode 216.
type CharacterListRequest struct{}

// Encode writes no current payload bytes.
func (m CharacterListRequest) Encode(_ *Encoder) {}

// DecodeCharacterListRequest ignores trailing bytes for additive compatibility.
func DecodeCharacterListRequest(d *Decoder) (CharacterListRequest, error) {
	d.SkipRemaining()
	return CharacterListRequest{}, nil
}

// CharacterFace is the nested face block of CharacterCreate:
// hairStyle + hairColor + skinTone + 5 fixed part bytes.
type CharacterFace struct {
	HairStyle uint8
	HairColor uint8
	SkinTone  uint8
	Parts     [5]uint8
}

// CharacterCreate is opcode 122 (C→S). Wire validation only: slot,
// gender, stats, and spell/skill IDs are carried as raw values so that
// application validation stays outside the codec.
type CharacterCreate struct {
	Slot   uint8
	Name   string
	Gender uint8

	Face  CharacterFace
	Stats [6]uint8

	Spells []uint16
	Skills []uint16
}

// Encode writes the exact §6.1 layout: slot u8, name string(1024),
// gender u8, face{3×u8, u8[5]}, stats u8[6], then spells and skills as
// plain u16-count + u16-ID arrays (no entry framing).
func (m CharacterCreate) Encode(e *Encoder) {
	e.U8(m.Slot)
	e.String(m.Name, MaxStringBytes)
	e.U8(m.Gender)
	e.U8(m.Face.HairStyle)
	e.U8(m.Face.HairColor)
	e.U8(m.Face.SkinTone)
	for _, p := range m.Face.Parts {
		e.U8(p)
	}
	for _, s := range m.Stats {
		e.U8(s)
	}
	encodeU16IDs(e, m.Spells)
	encodeU16IDs(e, m.Skills)
}

// encodeU16IDs writes one u16-count + u16-element ID array.
func encodeU16IDs(e *Encoder, ids []uint16) {
	e.Count(len(ids), MaxArrayCount)
	for _, id := range ids {
		e.U16(id)
	}
}

// DecodeCharacterCreate reads a CharacterCreate payload, tolerating
// trailing unknown bytes. Counts are cap-checked before allocation.
func DecodeCharacterCreate(d *Decoder) (CharacterCreate, error) {
	var m CharacterCreate
	var err error
	if m.Slot, err = d.U8(); err != nil {
		return CharacterCreate{}, fmt.Errorf("proto: character_create slot: %w", err)
	}
	if m.Name, err = d.String(MaxStringBytes); err != nil {
		return CharacterCreate{}, fmt.Errorf("proto: character_create name: %w", err)
	}
	if m.Gender, err = d.U8(); err != nil {
		return CharacterCreate{}, fmt.Errorf("proto: character_create gender: %w", err)
	}
	if m.Face.HairStyle, err = d.U8(); err != nil {
		return CharacterCreate{}, fmt.Errorf("proto: character_create face hairStyle: %w", err)
	}
	if m.Face.HairColor, err = d.U8(); err != nil {
		return CharacterCreate{}, fmt.Errorf("proto: character_create face hairColor: %w", err)
	}
	if m.Face.SkinTone, err = d.U8(); err != nil {
		return CharacterCreate{}, fmt.Errorf("proto: character_create face skinTone: %w", err)
	}
	for i := range m.Face.Parts {
		if m.Face.Parts[i], err = d.U8(); err != nil {
			return CharacterCreate{}, fmt.Errorf("proto: character_create face parts[%d]: %w", i, err)
		}
	}
	for i := range m.Stats {
		if m.Stats[i], err = d.U8(); err != nil {
			return CharacterCreate{}, fmt.Errorf("proto: character_create stats[%d]: %w", i, err)
		}
	}
	if m.Spells, err = decodeU16IDs(d, "spells"); err != nil {
		return CharacterCreate{}, err
	}
	if m.Skills, err = decodeU16IDs(d, "skills"); err != nil {
		return CharacterCreate{}, err
	}
	d.SkipRemaining()
	return m, nil
}

// decodeU16IDs reads one u16-count + u16-element ID array.
func decodeU16IDs(d *Decoder, what string) ([]uint16, error) {
	n, err := d.Count(MaxArrayCount)
	if err != nil {
		return nil, fmt.Errorf("proto: character_create %s: %w", what, err)
	}
	ids := make([]uint16, 0, n)
	for i := 0; i < n; i++ {
		var id uint16
		if id, err = d.U16(); err != nil {
			return nil, fmt.Errorf("proto: character_create %s[%d]: %w", what, i, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// CharacterDelete is opcode 123 (C→S): {slot u8}. No 0/1 validation here.
type CharacterDelete struct {
	Slot uint8
}

// Encode writes slot u8.
func (m CharacterDelete) Encode(e *Encoder) {
	e.U8(m.Slot)
}

// DecodeCharacterDelete reads a CharacterDelete payload, tolerating trailing bytes.
func DecodeCharacterDelete(d *Decoder) (CharacterDelete, error) {
	slot, err := d.U8()
	if err != nil {
		return CharacterDelete{}, fmt.Errorf("proto: character_delete slot: %w", err)
	}
	d.SkipRemaining()
	return CharacterDelete{Slot: slot}, nil
}

// EnterWorld is opcode 124 (C→S): {slot u8}. A distinct type from
// CharacterDelete even though the wire shape matches, preserving
// protocol meaning for future callers.
type EnterWorld struct {
	Slot uint8
}

// Encode writes slot u8.
func (m EnterWorld) Encode(e *Encoder) {
	e.U8(m.Slot)
}

// DecodeEnterWorld reads an EnterWorld payload, tolerating trailing bytes.
func DecodeEnterWorld(d *Decoder) (EnterWorld, error) {
	slot, err := d.U8()
	if err != nil {
		return EnterWorld{}, fmt.Errorf("proto: enter_world slot: %w", err)
	}
	d.SkipRemaining()
	return EnterWorld{Slot: slot}, nil
}

// Ack is opcode 125 (C→S): {ackSeq u32}. No sequence-ordering logic here.
type Ack struct {
	AckSeq uint32
}

// Encode writes ackSeq u32.
func (m Ack) Encode(e *Encoder) {
	e.U32(m.AckSeq)
}

// DecodeAck reads an Ack payload, tolerating trailing bytes.
func DecodeAck(d *Decoder) (Ack, error) {
	seq, err := d.U32()
	if err != nil {
		return Ack{}, fmt.Errorf("proto: ack ackSeq: %w", err)
	}
	d.SkipRemaining()
	return Ack{AckSeq: seq}, nil
}

// LeaveWorld is opcode 126 (C→S): empty.
type LeaveWorld struct{}

// Encode writes no current payload bytes.
func (m LeaveWorld) Encode(_ *Encoder) {}

// DecodeLeaveWorld ignores trailing bytes for additive compatibility.
func DecodeLeaveWorld(d *Decoder) (LeaveWorld, error) {
	d.SkipRemaining()
	return LeaveWorld{}, nil
}

// CharacterSummary is one entry of the opcode 216 character list.
// CharName uses MaxStringBytes at the wire layer; the semantic 3–16
// code-point rule is not enforced here.
type CharacterSummary struct {
	Slot     uint8
	CharName string
	Level    uint16
}

// CharacterList is opcode 216 (S→C): u16 count + entry-framed summaries.
type CharacterList struct {
	Characters []CharacterSummary
}

// Encode writes u16 count (MaxArrayCount) then each summary through
// Encoder.Entry so every entry carries its u16 entryLen prefix.
func (m CharacterList) Encode(e *Encoder) {
	e.Count(len(m.Characters), MaxArrayCount)
	for _, c := range m.Characters {
		c := c
		e.Entry(func(sub *Encoder) error {
			sub.U8(c.Slot)
			sub.String(c.CharName, MaxStringBytes)
			sub.U16(c.Level)
			return nil
		})
	}
}

// DecodeCharacterList reads a CharacterList payload. Each entry is
// decoded from its bounded child decoder; trailing bytes inside an
// entry are ignored so additive entry versions stay compatible and
// never corrupt the next entry's boundary.
func DecodeCharacterList(d *Decoder) (CharacterList, error) {
	n, err := d.Count(MaxArrayCount)
	if err != nil {
		return CharacterList{}, fmt.Errorf("proto: character_list count: %w", err)
	}
	m := CharacterList{Characters: make([]CharacterSummary, 0, n)}
	for i := 0; i < n; i++ {
		child, err := d.Entry()
		if err != nil {
			return CharacterList{}, fmt.Errorf("proto: character_list[%d]: %w", i, err)
		}
		var c CharacterSummary
		if c.Slot, err = child.U8(); err != nil {
			return CharacterList{}, fmt.Errorf("proto: character_list[%d] slot: %w", i, err)
		}
		if c.CharName, err = child.String(MaxStringBytes); err != nil {
			return CharacterList{}, fmt.Errorf("proto: character_list[%d] charName: %w", i, err)
		}
		if c.Level, err = child.U16(); err != nil {
			return CharacterList{}, fmt.Errorf("proto: character_list[%d] level: %w", i, err)
		}
		child.SkipRemaining()
		m.Characters = append(m.Characters, c)
	}
	d.SkipRemaining()
	return m, nil
}

// CharacterOp is opcode 217 (S→C): {op u8, ok u8}. Both fields are raw
// wire values; no numeric operation/result mapping is frozen here, and
// OK is intentionally not a Go bool.
type CharacterOp struct {
	Op uint8
	OK uint8
}

// Encode writes op u8 + ok u8.
func (m CharacterOp) Encode(e *Encoder) {
	e.U8(m.Op)
	e.U8(m.OK)
}

// DecodeCharacterOp reads a CharacterOp payload, tolerating trailing bytes.
func DecodeCharacterOp(d *Decoder) (CharacterOp, error) {
	var m CharacterOp
	var err error
	if m.Op, err = d.U8(); err != nil {
		return CharacterOp{}, fmt.Errorf("proto: character_op op: %w", err)
	}
	if m.OK, err = d.U8(); err != nil {
		return CharacterOp{}, fmt.Errorf("proto: character_op ok: %w", err)
	}
	d.SkipRemaining()
	return m, nil
}

// WorldReady is opcode 219 (S→C): empty baseline barrier marker.
// It carries no session-transition logic; M3 owns that.
type WorldReady struct{}

// Encode writes no current payload bytes.
func (m WorldReady) Encode(_ *Encoder) {}

// DecodeWorldReady ignores trailing bytes for additive compatibility.
func DecodeWorldReady(d *Decoder) (WorldReady, error) {
	d.SkipRemaining()
	return WorldReady{}, nil
}
