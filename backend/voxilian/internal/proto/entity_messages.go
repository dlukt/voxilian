package proto

import "fmt"

// S→C entity/stat messages: opcodes 203–210 and 213–215.
// (Opcodes 211/212/218/220 belong to M2-T3c and are NOT defined here.)
//
// This is a wire codec only: no session/world behavior, no AOI logic,
// no reconciliation algorithms, no entity ownership, no stat semantics,
// no gameplay validation. Entity IDs stay raw u32, Kind stays raw u8,
// Proto stays raw u16, stat IDs stay raw u8 — catalog/consistency rules
// belong to later runtime layers.

// EntityEntry is the shared wire shape embedded in CellSnapshot (repeated,
// entry-framed) and EntityCreate (single, direct):
//
//	entity u32, kind u8, proto u16, pos, angle u16, speed u8
//
// Current encoded size is exactly 22 bytes (4+1+2+12+2+1). The helpers
// below add NO entry-length prefix themselves; whether [u16 entryLen]
// wraps the entry depends on the containing message.
type EntityEntry struct {
	Entity uint32
	Kind   uint8
	Proto  uint16
	Pos    Position
	Angle  uint16
	Speed  uint8
}

// EntityEntryWireSize is the current encoded byte size of EntityEntry.
const EntityEntryWireSize = 22

// encode writes the 22-byte EntityEntry layout.
func (v EntityEntry) encode(e *Encoder) {
	e.U32(v.Entity)
	e.U8(v.Kind)
	e.U16(v.Proto)
	e.Position(v.Pos)
	e.Angle(v.Angle)
	e.U8(v.Speed)
}

// decodeEntityEntry reads one EntityEntry from the current cursor.
func decodeEntityEntry(d *Decoder, context string) (EntityEntry, error) {
	var v EntityEntry
	var err error
	if v.Entity, err = d.U32(); err != nil {
		return EntityEntry{}, fmt.Errorf("proto: %s entity: %w", context, err)
	}
	if v.Kind, err = d.U8(); err != nil {
		return EntityEntry{}, fmt.Errorf("proto: %s kind: %w", context, err)
	}
	if v.Proto, err = d.U16(); err != nil {
		return EntityEntry{}, fmt.Errorf("proto: %s proto: %w", context, err)
	}
	if v.Pos, err = d.Position(); err != nil {
		return EntityEntry{}, fmt.Errorf("proto: %s pos: %w", context, err)
	}
	if v.Angle, err = d.Angle(); err != nil {
		return EntityEntry{}, fmt.Errorf("proto: %s angle: %w", context, err)
	}
	if v.Speed, err = d.U8(); err != nil {
		return EntityEntry{}, fmt.Errorf("proto: %s speed: %w", context, err)
	}
	return v, nil
}

// CellSnapshot is opcode 203: {cell, count u16, [entryLen]entityEntry...}.
// Every entity uses T1 entry framing; the count uses MaxArrayCount.
type CellSnapshot struct {
	Cell     Cell
	Entities []EntityEntry
}

// Encode writes cell + u16 count + each entity via Encoder.Entry.
func (m CellSnapshot) Encode(e *Encoder) {
	e.Cell(m.Cell)
	e.Count(len(m.Entities), MaxArrayCount)
	for _, v := range m.Entities {
		v := v
		e.Entry(func(sub *Encoder) error {
			v.encode(sub)
			return nil
		})
	}
}

// DecodeCellSnapshot reads a CellSnapshot payload. Each entity is decoded
// from its bounded child decoder; trailing bytes inside an entry are
// ignored so additive entry versions never corrupt the next boundary.
func DecodeCellSnapshot(d *Decoder) (CellSnapshot, error) {
	var m CellSnapshot
	var err error
	if m.Cell, err = d.Cell(); err != nil {
		return CellSnapshot{}, fmt.Errorf("proto: cell_snapshot cell: %w", err)
	}
	n, err := d.Count(MaxArrayCount)
	if err != nil {
		return CellSnapshot{}, fmt.Errorf("proto: cell_snapshot count: %w", err)
	}
	m.Entities = make([]EntityEntry, 0, n)
	for i := 0; i < n; i++ {
		child, err := d.Entry()
		if err != nil {
			return CellSnapshot{}, fmt.Errorf("proto: cell_snapshot[%d]: %w", i, err)
		}
		v, err := decodeEntityEntry(child, "cell_snapshot entry")
		if err != nil {
			return CellSnapshot{}, fmt.Errorf("proto: cell_snapshot[%d]: %w", i, err)
		}
		child.SkipRemaining()
		m.Entities = append(m.Entities, v)
	}
	d.SkipRemaining()
	return m, nil
}

// EntityCreate is opcode 204: a SINGLE EntityEntry with NO entryLen
// prefix (22 bytes current). Message-level trailing bytes already
// provide forward compatibility for single-entry messages.
type EntityCreate struct {
	Entity EntityEntry
}

// Encode writes the entry directly, without framing.
func (m EntityCreate) Encode(e *Encoder) {
	m.Entity.encode(e)
}

// DecodeEntityCreate reads an EntityCreate payload, tolerating trailing
// unknown bytes.
func DecodeEntityCreate(d *Decoder) (EntityCreate, error) {
	v, err := decodeEntityEntry(d, "entity_create")
	if err != nil {
		return EntityCreate{}, err
	}
	d.SkipRemaining()
	return EntityCreate{Entity: v}, nil
}

// EntityMove is opcode 205: {entity u32, pos, angle u16, speed u8,
// lastProcessedInputSeq u32}. LastProcessedInputSeq is the
// reconciliation anchor for the session's own character, but the codec
// preserves the full u32 range for every entity and decides no ownership.
type EntityMove struct {
	Entity                uint32
	Pos                   Position
	Angle                 uint16
	Speed                 uint8
	LastProcessedInputSeq uint32
}

// Encode writes the exact field order above.
func (m EntityMove) Encode(e *Encoder) {
	e.U32(m.Entity)
	e.Position(m.Pos)
	e.Angle(m.Angle)
	e.U8(m.Speed)
	e.U32(m.LastProcessedInputSeq)
}

// DecodeEntityMove reads an EntityMove payload, tolerating trailing
// unknown bytes. No RFC1982 arithmetic here; M2-T5 owns that.
func DecodeEntityMove(d *Decoder) (EntityMove, error) {
	var m EntityMove
	var err error
	if m.Entity, err = d.U32(); err != nil {
		return EntityMove{}, fmt.Errorf("proto: entity_move entity: %w", err)
	}
	if m.Pos, err = d.Position(); err != nil {
		return EntityMove{}, fmt.Errorf("proto: entity_move pos: %w", err)
	}
	if m.Angle, err = d.Angle(); err != nil {
		return EntityMove{}, fmt.Errorf("proto: entity_move angle: %w", err)
	}
	if m.Speed, err = d.U8(); err != nil {
		return EntityMove{}, fmt.Errorf("proto: entity_move speed: %w", err)
	}
	if m.LastProcessedInputSeq, err = d.U32(); err != nil {
		return EntityMove{}, fmt.Errorf("proto: entity_move lastProcessedInputSeq: %w", err)
	}
	d.SkipRemaining()
	return m, nil
}

// EntityRemove is opcode 206: {entity u32}. No validity/state checks.
type EntityRemove struct {
	Entity uint32
}

// Encode writes entity u32.
func (m EntityRemove) Encode(e *Encoder) {
	e.U32(m.Entity)
}

// DecodeEntityRemove reads an EntityRemove payload, tolerating trailing bytes.
func DecodeEntityRemove(d *Decoder) (EntityRemove, error) {
	entity, err := d.U32()
	if err != nil {
		return EntityRemove{}, fmt.Errorf("proto: entity_remove entity: %w", err)
	}
	d.SkipRemaining()
	return EntityRemove{Entity: entity}, nil
}

// StatEntry is the shared stat payload shape:
//
//	statId u8, value i32, min i32, max i32, curmax i32
//
// Current encoded size is exactly 17 bytes (1+4+4+4+4). All values are
// signed; no Min/Value/Max relationship is validated here.
type StatEntry struct {
	StatID uint8
	Value  int32
	Min    int32
	Max    int32
	CurMax int32
}

// StatEntryWireSize is the current encoded byte size of StatEntry.
const StatEntryWireSize = 17

// encode writes the 17-byte StatEntry layout.
func (v StatEntry) encode(e *Encoder) {
	e.U8(v.StatID)
	e.I32(v.Value)
	e.I32(v.Min)
	e.I32(v.Max)
	e.I32(v.CurMax)
}

// decodeStatEntry reads one StatEntry from the current cursor.
func decodeStatEntry(d *Decoder, context string) (StatEntry, error) {
	var v StatEntry
	var err error
	if v.StatID, err = d.U8(); err != nil {
		return StatEntry{}, fmt.Errorf("proto: %s statId: %w", context, err)
	}
	if v.Value, err = d.I32(); err != nil {
		return StatEntry{}, fmt.Errorf("proto: %s value: %w", context, err)
	}
	if v.Min, err = d.I32(); err != nil {
		return StatEntry{}, fmt.Errorf("proto: %s min: %w", context, err)
	}
	if v.Max, err = d.I32(); err != nil {
		return StatEntry{}, fmt.Errorf("proto: %s max: %w", context, err)
	}
	if v.CurMax, err = d.I32(); err != nil {
		return StatEntry{}, fmt.Errorf("proto: %s curmax: %w", context, err)
	}
	return v, nil
}

// Stat is opcode 207: {entity u32, statId u8, value/min/max/curmax i32}.
// The single stat payload is NOT entry-framed; StatEntry is reused so
// 207 and 208 cannot silently diverge.
type Stat struct {
	Entity uint32
	Stat   StatEntry
}

// Encode writes entity u32 + the 17-byte stat layout.
func (m Stat) Encode(e *Encoder) {
	e.U32(m.Entity)
	m.Stat.encode(e)
}

// DecodeStat reads a Stat payload, tolerating trailing unknown bytes.
func DecodeStat(d *Decoder) (Stat, error) {
	var m Stat
	var err error
	if m.Entity, err = d.U32(); err != nil {
		return Stat{}, fmt.Errorf("proto: stat entity: %w", err)
	}
	if m.Stat, err = decodeStatEntry(d, "stat"); err != nil {
		return Stat{}, err
	}
	d.SkipRemaining()
	return m, nil
}

// StatGroup is opcode 208: {entity u32, count u16,
// [entryLen]statEntry...}. Each stat entry is framed; the count uses
// MaxArrayCount.
type StatGroup struct {
	Entity uint32
	Stats  []StatEntry
}

// Encode writes entity u32 + u16 count + each stat via Encoder.Entry.
func (m StatGroup) Encode(e *Encoder) {
	e.U32(m.Entity)
	e.Count(len(m.Stats), MaxArrayCount)
	for _, v := range m.Stats {
		v := v
		e.Entry(func(sub *Encoder) error {
			v.encode(sub)
			return nil
		})
	}
}

// DecodeStatGroup reads a StatGroup payload. Each stat is decoded from
// its bounded child decoder; trailing entry bytes are ignored.
func DecodeStatGroup(d *Decoder) (StatGroup, error) {
	var m StatGroup
	var err error
	if m.Entity, err = d.U32(); err != nil {
		return StatGroup{}, fmt.Errorf("proto: stat_group entity: %w", err)
	}
	n, err := d.Count(MaxArrayCount)
	if err != nil {
		return StatGroup{}, fmt.Errorf("proto: stat_group count: %w", err)
	}
	m.Stats = make([]StatEntry, 0, n)
	for i := 0; i < n; i++ {
		child, err := d.Entry()
		if err != nil {
			return StatGroup{}, fmt.Errorf("proto: stat_group[%d]: %w", i, err)
		}
		v, err := decodeStatEntry(child, "stat_group entry")
		if err != nil {
			return StatGroup{}, fmt.Errorf("proto: stat_group[%d]: %w", i, err)
		}
		child.SkipRemaining()
		m.Stats = append(m.Stats, v)
	}
	d.SkipRemaining()
	return m, nil
}

// Said is opcode 209 (S→C): {from u32, channel u8, text string}. This is
// chat text, so MaxChatBytes (512) applies. No channel validation and
// no moderation here.
type Said struct {
	From    uint32
	Channel uint8
	Text    string
}

// Encode writes from u32 + channel u8 + string(text, 512).
func (m Said) Encode(e *Encoder) {
	e.U32(m.From)
	e.U8(m.Channel)
	e.String(m.Text, MaxChatBytes)
}

// DecodeSaid reads a Said payload, tolerating trailing unknown bytes.
func DecodeSaid(d *Decoder) (Said, error) {
	var m Said
	var err error
	if m.From, err = d.U32(); err != nil {
		return Said{}, fmt.Errorf("proto: said from: %w", err)
	}
	if m.Channel, err = d.U8(); err != nil {
		return Said{}, fmt.Errorf("proto: said channel: %w", err)
	}
	if m.Text, err = d.String(MaxChatBytes); err != nil {
		return Said{}, fmt.Errorf("proto: said text: %w", err)
	}
	d.SkipRemaining()
	return m, nil
}

// Effect is opcode 210: {id u16, target u32, pos}. ID stays raw u16;
// the spec gives it no named stable namespace here. No
// target/position gameplay validation.
type Effect struct {
	ID     uint16
	Target uint32
	Pos    Position
}

// Encode writes id u16 + target u32 + pos.
func (m Effect) Encode(e *Encoder) {
	e.U16(m.ID)
	e.U32(m.Target)
	e.Position(m.Pos)
}

// DecodeEffect reads an Effect payload, tolerating trailing unknown bytes.
func DecodeEffect(d *Decoder) (Effect, error) {
	var m Effect
	var err error
	if m.ID, err = d.U16(); err != nil {
		return Effect{}, fmt.Errorf("proto: effect id: %w", err)
	}
	if m.Target, err = d.U32(); err != nil {
		return Effect{}, fmt.Errorf("proto: effect target: %w", err)
	}
	if m.Pos, err = d.Position(); err != nil {
		return Effect{}, fmt.Errorf("proto: effect pos: %w", err)
	}
	d.SkipRemaining()
	return m, nil
}

// TradeResult is opcode 213: {ok u8}. Raw uint8 — never bool-coerced,
// never normalized (255 stays 255). No trade state logic.
type TradeResult struct {
	OK uint8
}

// Encode writes ok u8.
func (m TradeResult) Encode(e *Encoder) {
	e.U8(m.OK)
}

// DecodeTradeResult reads a TradeResult payload, tolerating trailing bytes.
func DecodeTradeResult(d *Decoder) (TradeResult, error) {
	ok, err := d.U8()
	if err != nil {
		return TradeResult{}, fmt.Errorf("proto: trade_result ok: %w", err)
	}
	d.SkipRemaining()
	return TradeResult{OK: ok}, nil
}

// Death is opcode 214: {victim u32}. No death-state logic.
type Death struct {
	Victim uint32
}

// Encode writes victim u32.
func (m Death) Encode(e *Encoder) {
	e.U32(m.Victim)
}

// DecodeDeath reads a Death payload, tolerating trailing unknown bytes.
func DecodeDeath(d *Decoder) (Death, error) {
	victim, err := d.U32()
	if err != nil {
		return Death{}, fmt.Errorf("proto: death victim: %w", err)
	}
	d.SkipRemaining()
	return Death{Victim: victim}, nil
}

// Respawn is opcode 215: {pos}. Fixed-point Position; no
// spawn-location validation.
type Respawn struct {
	Pos Position
}

// Encode writes pos.
func (m Respawn) Encode(e *Encoder) {
	e.Position(m.Pos)
}

// DecodeRespawn reads a Respawn payload, tolerating trailing unknown bytes.
func DecodeRespawn(d *Decoder) (Respawn, error) {
	pos, err := d.Position()
	if err != nil {
		return Respawn{}, fmt.Errorf("proto: respawn pos: %w", err)
	}
	d.SkipRemaining()
	return Respawn{Pos: pos}, nil
}
