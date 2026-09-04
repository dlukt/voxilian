package proto

import "fmt"

// C→S gameplay intent messages: opcodes 102–120. (Opcode 126 LeaveWorld,
// already defined by M2-T2, completes the intent catalog but lives in
// character_messages.go and is not duplicated here.)
//
// This is a wire codec only: no session-state permission checks, no
// rate limits, no gameplay validation. Structurally valid messages such
// as Attack{Target:0}, Give{Qty:0}, or Put{Item:X, Container:X} decode
// successfully; ownership, range, and policy rules belong to sim/M3.
// Fields encoded as u8 stay raw uint8 — nothing is coerced to bool or 0/1.

// Move is opcode 102: client movement intent. Header tick carries the
// sampling tick; InputSeq ordering and heldDirs/runFlag semantics are
// application concerns, not codec checks.
type Move struct {
	InputSeq uint32
	HeldDirs uint8
	RunFlag  uint8
	Yaw      uint16
}

// Encode writes u32 inputSeq + u8 heldDirs + u8 runFlag + angle yaw.
func (m Move) Encode(e *Encoder) {
	e.U32(m.InputSeq)
	e.U8(m.HeldDirs)
	e.U8(m.RunFlag)
	e.Angle(m.Yaw)
}

// DecodeMove reads a Move payload, tolerating trailing unknown bytes.
func DecodeMove(d *Decoder) (Move, error) {
	var m Move
	var err error
	if m.InputSeq, err = d.U32(); err != nil {
		return Move{}, fmt.Errorf("proto: move inputSeq: %w", err)
	}
	if m.HeldDirs, err = d.U8(); err != nil {
		return Move{}, fmt.Errorf("proto: move heldDirs: %w", err)
	}
	if m.RunFlag, err = d.U8(); err != nil {
		return Move{}, fmt.Errorf("proto: move runFlag: %w", err)
	}
	if m.Yaw, err = d.Angle(); err != nil {
		return Move{}, fmt.Errorf("proto: move yaw: %w", err)
	}
	d.SkipRemaining()
	return m, nil
}

// Attack is opcode 103: {target u32}. Target is a session-local
// NetEntityID handle; existence is never validated here.
type Attack struct {
	Target uint32
}

// Encode writes target u32.
func (m Attack) Encode(e *Encoder) {
	e.U32(m.Target)
}

// DecodeAttack reads an Attack payload, tolerating trailing unknown bytes.
func DecodeAttack(d *Decoder) (Attack, error) {
	target, err := d.U32()
	if err != nil {
		return Attack{}, fmt.Errorf("proto: attack target: %w", err)
	}
	d.SkipRemaining()
	return Attack{Target: target}, nil
}

// Cast is opcode 104: {spell u16 stable, target u32}.
// Spell is the stable spell ID namespace carried directly as u16;
// catalog existence is never validated here.
type Cast struct {
	Spell  uint16
	Target uint32
}

// Encode writes spell u16 + target u32.
func (m Cast) Encode(e *Encoder) {
	e.U16(m.Spell)
	e.U32(m.Target)
}

// DecodeCast reads a Cast payload, tolerating trailing unknown bytes.
func DecodeCast(d *Decoder) (Cast, error) {
	var m Cast
	var err error
	if m.Spell, err = d.U16(); err != nil {
		return Cast{}, fmt.Errorf("proto: cast spell: %w", err)
	}
	if m.Target, err = d.U32(); err != nil {
		return Cast{}, fmt.Errorf("proto: cast target: %w", err)
	}
	d.SkipRemaining()
	return m, nil
}

// Use is opcode 105: {kind u8, id u32}. The ID width is ALWAYS u32
// regardless of kind — never u16 for kind=0. Meanings: kind=0 skill
// (ID is a stable skill ID and MUST fit u16), kind=1 item (ID is a
// full u32 NetEntityID). Unknown future kinds round-trip untouched.
type Use struct {
	Kind uint8
	ID   uint32
}

// UseKindSkill selects the stable-skill-ID namespace of Use.ID.
const UseKindSkill uint8 = 0

// UseKindItem selects the NetEntityID namespace of Use.ID.
const UseKindItem uint8 = 1

// Encode writes kind u8 + id u32, enforcing the kind=0 stable-ID
// namespace invariant (ID must fit u16).
func (m Use) Encode(e *Encoder) {
	e.U8(m.Kind)
	if m.Kind == UseKindSkill && m.ID > maxU16 {
		e.fail(fmt.Errorf("proto: use id=%d exceeds stable u16 range: %w", m.ID, ErrStableIDOutOfRange))
		return
	}
	e.U32(m.ID)
}

// DecodeUse reads a Use payload, enforcing the kind=0 stable-ID
// invariant and tolerating trailing unknown bytes.
func DecodeUse(d *Decoder) (Use, error) {
	var m Use
	var err error
	if m.Kind, err = d.U8(); err != nil {
		return Use{}, fmt.Errorf("proto: use kind: %w", err)
	}
	if m.ID, err = d.U32(); err != nil {
		return Use{}, fmt.Errorf("proto: use id: %w", err)
	}
	if m.Kind == UseKindSkill && m.ID > maxU16 {
		return Use{}, fmt.Errorf("proto: use id=%d exceeds stable u16 range: %w", m.ID, ErrStableIDOutOfRange)
	}
	d.SkipRemaining()
	return m, nil
}

// Get is opcode 106: {entity u32, item u32}. Both are NetEntityID-shaped
// handles with no ownership semantics in the codec.
type Get struct {
	Entity uint32
	Item   uint32
}

// Encode writes entity u32 + item u32.
func (m Get) Encode(e *Encoder) {
	e.U32(m.Entity)
	e.U32(m.Item)
}

// DecodeGet reads a Get payload, tolerating trailing unknown bytes.
func DecodeGet(d *Decoder) (Get, error) {
	var m Get
	var err error
	if m.Entity, err = d.U32(); err != nil {
		return Get{}, fmt.Errorf("proto: get entity: %w", err)
	}
	if m.Item, err = d.U32(); err != nil {
		return Get{}, fmt.Errorf("proto: get item: %w", err)
	}
	d.SkipRemaining()
	return m, nil
}

// Drop is opcode 107: {item u32}.
type Drop struct {
	Item uint32
}

// Encode writes item u32.
func (m Drop) Encode(e *Encoder) {
	e.U32(m.Item)
}

// DecodeDrop reads a Drop payload, tolerating trailing unknown bytes.
func DecodeDrop(d *Decoder) (Drop, error) {
	item, err := d.U32()
	if err != nil {
		return Drop{}, fmt.Errorf("proto: drop item: %w", err)
	}
	d.SkipRemaining()
	return Drop{Item: item}, nil
}

// Put is opcode 108: {item u32, container u32}. No self-containment,
// cycle, or ownership validation here; store/sim own those rules.
type Put struct {
	Item      uint32
	Container uint32
}

// Encode writes item u32 + container u32.
func (m Put) Encode(e *Encoder) {
	e.U32(m.Item)
	e.U32(m.Container)
}

// DecodePut reads a Put payload, tolerating trailing unknown bytes.
func DecodePut(d *Decoder) (Put, error) {
	var m Put
	var err error
	if m.Item, err = d.U32(); err != nil {
		return Put{}, fmt.Errorf("proto: put item: %w", err)
	}
	if m.Container, err = d.U32(); err != nil {
		return Put{}, fmt.Errorf("proto: put container: %w", err)
	}
	d.SkipRemaining()
	return m, nil
}

// Give is opcode 109: {target u32, item u32, qty u16}.
// Qty 0 is codec-valid; gameplay policy decides.
type Give struct {
	Target uint32
	Item   uint32
	Qty    uint16
}

// Encode writes target u32 + item u32 + qty u16.
func (m Give) Encode(e *Encoder) {
	e.U32(m.Target)
	e.U32(m.Item)
	e.U16(m.Qty)
}

// DecodeGive reads a Give payload, tolerating trailing unknown bytes.
func DecodeGive(d *Decoder) (Give, error) {
	var m Give
	var err error
	if m.Target, err = d.U32(); err != nil {
		return Give{}, fmt.Errorf("proto: give target: %w", err)
	}
	if m.Item, err = d.U32(); err != nil {
		return Give{}, fmt.Errorf("proto: give item: %w", err)
	}
	if m.Qty, err = d.U16(); err != nil {
		return Give{}, fmt.Errorf("proto: give qty: %w", err)
	}
	d.SkipRemaining()
	return m, nil
}

// encodeEntityIDs writes one u16-count + u32-element array of
// NetEntityID-shaped handles (no entry framing).
func encodeEntityIDs(e *Encoder, ids []uint32) {
	e.Count(len(ids), MaxArrayCount)
	for _, id := range ids {
		e.U32(id)
	}
}

// decodeEntityIDs reads one u16-count + u32-element array, validating
// the count before allocating.
func decodeEntityIDs(d *Decoder, context string) ([]uint32, error) {
	n, err := d.Count(MaxArrayCount)
	if err != nil {
		return nil, fmt.Errorf("proto: %s: %w", context, err)
	}
	ids := make([]uint32, 0, n)
	for i := 0; i < n; i++ {
		var id uint32
		if id, err = d.U32(); err != nil {
			return nil, fmt.Errorf("proto: %s[%d]: %w", context, i, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// Offer is opcode 110: {target u32, items u16-count + u32 IDs}.
// Direct scalar array, no entry framing.
type Offer struct {
	Target uint32
	Items  []uint32
}

// Encode writes target u32 + item ID array.
func (m Offer) Encode(e *Encoder) {
	e.U32(m.Target)
	encodeEntityIDs(e, m.Items)
}

// DecodeOffer reads an Offer payload, tolerating trailing unknown bytes.
func DecodeOffer(d *Decoder) (Offer, error) {
	var m Offer
	var err error
	if m.Target, err = d.U32(); err != nil {
		return Offer{}, fmt.Errorf("proto: offer target: %w", err)
	}
	if m.Items, err = decodeEntityIDs(d, "offer items"); err != nil {
		return Offer{}, err
	}
	d.SkipRemaining()
	return m, nil
}

// Counter is opcode 111: {items u16-count + u32 IDs}. A distinct type
// from Offer because Offer carries Target; the element helper is shared.
type Counter struct {
	Items []uint32
}

// Encode writes the item ID array.
func (m Counter) Encode(e *Encoder) {
	encodeEntityIDs(e, m.Items)
}

// DecodeCounter reads a Counter payload, tolerating trailing unknown bytes.
func DecodeCounter(d *Decoder) (Counter, error) {
	items, err := decodeEntityIDs(d, "counter items")
	if err != nil {
		return Counter{}, err
	}
	d.SkipRemaining()
	return Counter{Items: items}, nil
}

// Accept is opcode 112: empty.
type Accept struct{}

// Encode writes no current payload bytes.
func (m Accept) Encode(_ *Encoder) {}

// DecodeAccept ignores trailing bytes for additive compatibility.
func DecodeAccept(d *Decoder) (Accept, error) {
	d.SkipRemaining()
	return Accept{}, nil
}

// Cancel is opcode 113: empty. A distinct type from Accept.
type Cancel struct{}

// Encode writes no current payload bytes.
func (m Cancel) Encode(_ *Encoder) {}

// DecodeCancel ignores trailing bytes for additive compatibility.
func DecodeCancel(d *Decoder) (Cancel, error) {
	d.SkipRemaining()
	return Cancel{}, nil
}

// Buy is opcode 114: {vendor u32, listing u16, qty u16}. Vendor is a
// NetEntityID handle while listing is a stable per-vendor-proto ID —
// the widths must not be swapped. Listing existence and
// vendor/listing relationships belong to catalog lookup, not the codec.
type Buy struct {
	Vendor  uint32
	Listing uint16
	Qty     uint16
}

// Encode writes vendor u32 + listing u16 + qty u16.
func (m Buy) Encode(e *Encoder) {
	e.U32(m.Vendor)
	e.U16(m.Listing)
	e.U16(m.Qty)
}

// DecodeBuy reads a Buy payload, tolerating trailing unknown bytes.
func DecodeBuy(d *Decoder) (Buy, error) {
	var m Buy
	var err error
	if m.Vendor, err = d.U32(); err != nil {
		return Buy{}, fmt.Errorf("proto: buy vendor: %w", err)
	}
	if m.Listing, err = d.U16(); err != nil {
		return Buy{}, fmt.Errorf("proto: buy listing: %w", err)
	}
	if m.Qty, err = d.U16(); err != nil {
		return Buy{}, fmt.Errorf("proto: buy qty: %w", err)
	}
	d.SkipRemaining()
	return m, nil
}

// Rest is opcode 115: {state u8}. State stays a raw value; no bool
// coercion and no state constants are defined here.
type Rest struct {
	State uint8
}

// Encode writes state u8.
func (m Rest) Encode(e *Encoder) {
	e.U8(m.State)
}

// DecodeRest reads a Rest payload, tolerating trailing unknown bytes.
func DecodeRest(d *Decoder) (Rest, error) {
	state, err := d.U8()
	if err != nil {
		return Rest{}, fmt.Errorf("proto: rest state: %w", err)
	}
	d.SkipRemaining()
	return Rest{State: state}, nil
}

// Eat is opcode 116: {item u32}. No item validity checks here.
type Eat struct {
	Item uint32
}

// Encode writes item u32.
func (m Eat) Encode(e *Encoder) {
	e.U32(m.Item)
}

// DecodeEat reads an Eat payload, tolerating trailing unknown bytes.
func DecodeEat(d *Decoder) (Eat, error) {
	item, err := d.U32()
	if err != nil {
		return Eat{}, fmt.Errorf("proto: eat item: %w", err)
	}
	d.SkipRemaining()
	return Eat{Item: item}, nil
}

// Say is opcode 117: {channel u8, text string}. This is chat, so the
// text uses MaxChatBytes (512), not the general string cap. Channel
// values and moderation are application concerns.
type Say struct {
	Channel uint8
	Text    string
}

// Encode writes channel u8 + string(text, 512).
func (m Say) Encode(e *Encoder) {
	e.U8(m.Channel)
	e.String(m.Text, MaxChatBytes)
}

// DecodeSay reads a Say payload, tolerating trailing unknown bytes.
func DecodeSay(d *Decoder) (Say, error) {
	var m Say
	var err error
	if m.Channel, err = d.U8(); err != nil {
		return Say{}, fmt.Errorf("proto: say channel: %w", err)
	}
	if m.Text, err = d.String(MaxChatBytes); err != nil {
		return Say{}, fmt.Errorf("proto: say text: %w", err)
	}
	d.SkipRemaining()
	return m, nil
}

// SayGroup is opcode 118: {text string}, also capped at MaxChatBytes.
type SayGroup struct {
	Text string
}

// Encode writes string(text, 512).
func (m SayGroup) Encode(e *Encoder) {
	e.String(m.Text, MaxChatBytes)
}

// DecodeSayGroup reads a SayGroup payload, tolerating trailing unknown bytes.
func DecodeSayGroup(d *Decoder) (SayGroup, error) {
	text, err := d.String(MaxChatBytes)
	if err != nil {
		return SayGroup{}, fmt.Errorf("proto: say_group text: %w", err)
	}
	d.SkipRemaining()
	return SayGroup{Text: text}, nil
}

// SafetyToggle is opcode 119: empty. No PvP/safety logic here.
type SafetyToggle struct{}

// Encode writes no current payload bytes.
func (m SafetyToggle) Encode(_ *Encoder) {}

// DecodeSafetyToggle ignores trailing bytes for additive compatibility.
func DecodeSafetyToggle(d *Decoder) (SafetyToggle, error) {
	d.SkipRemaining()
	return SafetyToggle{}, nil
}

// RespawnAck is opcode 120: empty. No death/respawn transitions here.
type RespawnAck struct{}

// Encode writes no current payload bytes.
func (m RespawnAck) Encode(_ *Encoder) {}

// DecodeRespawnAck ignores trailing bytes for additive compatibility.
func DecodeRespawnAck(d *Decoder) (RespawnAck, error) {
	d.SkipRemaining()
	return RespawnAck{}, nil
}
