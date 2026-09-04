package proto

import "fmt"

// Container / inventory / chunk / shop messages: opcodes 211, 212, 218,
// 220 (spec v0.3.7 §6.3). This is a wire codec only: no inventory
// ownership, equipment rules, containment checks, item lookup, handle
// allocation/invalidation runtime, trade state machine, shop purchase,
// chunk pacing, or world generation.

// Inventory location discriminator meanings (producer-side semantics;
// the codec carries the value opaquely and never rejects unknown
// future values).
const (
	// InventoryLocationDirect means the item is directly owned by the
	// current character; Container is ignored (senders emit 0).
	InventoryLocationDirect uint8 = 0

	// InventoryLocationContainer means the item is contained in another
	// inventory item named by Container.
	InventoryLocationContainer uint8 = 1
)

// InventoryEntry is inventoryEntry v1 per spec v0.3.7:
//
//	item u32 NetEntityID, proto u16 stable, qty u16, hits i32,
//	location u8, container u32 NetEntityID, slot string
//
// Field order is binding. Fixed prefix before slot bytes is 19 bytes
// (4+2+2+4+1+4+2); the enclosing u16 entryLen is not included. Slot is
// the authoritative location label (opaque UTF-8, 1024-byte cap); no
// numeric slot IDs exist. Hits is signed to match persistence; revision,
// enchants JSONB, and database IDs never cross this message.
type InventoryEntry struct {
	Item      uint32
	Proto     uint16
	Qty       uint16
	Hits      int32
	Location  uint8
	Container uint32
	Slot      string
}

// InventoryEntryPrefixSize is the fixed byte size of InventoryEntry
// before the slot string bytes (excludes the entryLen prefix itself).
const InventoryEntryPrefixSize = 19

// encode writes the v1 InventoryEntry layout. No entryLen here.
func (v InventoryEntry) encode(e *Encoder) {
	e.U32(v.Item)
	e.U16(v.Proto)
	e.U16(v.Qty)
	e.I32(v.Hits)
	e.U8(v.Location)
	e.U32(v.Container)
	e.String(v.Slot, MaxStringBytes)
}

// decodeInventoryEntry reads one InventoryEntry from the current cursor.
func decodeInventoryEntry(d *Decoder, context string) (InventoryEntry, error) {
	var v InventoryEntry
	var err error
	if v.Item, err = d.U32(); err != nil {
		return InventoryEntry{}, fmt.Errorf("proto: %s item: %w", context, err)
	}
	if v.Proto, err = d.U16(); err != nil {
		return InventoryEntry{}, fmt.Errorf("proto: %s proto: %w", context, err)
	}
	if v.Qty, err = d.U16(); err != nil {
		return InventoryEntry{}, fmt.Errorf("proto: %s qty: %w", context, err)
	}
	if v.Hits, err = d.I32(); err != nil {
		return InventoryEntry{}, fmt.Errorf("proto: %s hits: %w", context, err)
	}
	if v.Location, err = d.U8(); err != nil {
		return InventoryEntry{}, fmt.Errorf("proto: %s location: %w", context, err)
	}
	if v.Container, err = d.U32(); err != nil {
		return InventoryEntry{}, fmt.Errorf("proto: %s container: %w", context, err)
	}
	if v.Slot, err = d.String(MaxStringBytes); err != nil {
		return InventoryEntry{}, fmt.Errorf("proto: %s slot: %w", context, err)
	}
	return v, nil
}

// InventoryDelta is opcode 211: {count u16, [entryLen]inventoryEntry...}.
// Entries are authoritative UPSERTS for client-visible inventory state —
// there is deliberately no delete op, tombstone, or Qty==0-removal
// interpretation here. Removal/invalidation of an item handle uses
// opcode 206 EntityRemove.
type InventoryDelta struct {
	Items []InventoryEntry
}

// Encode writes u16 count + each entry via Encoder.Entry.
func (m InventoryDelta) Encode(e *Encoder) {
	e.Count(len(m.Items), MaxArrayCount)
	for _, v := range m.Items {
		v := v
		e.Entry(func(sub *Encoder) error {
			v.encode(sub)
			return nil
		})
	}
}

// DecodeInventoryDelta reads an InventoryDelta payload. Each entry is
// decoded from its bounded child decoder; trailing entry bytes are
// ignored. Qty==0 and unknown Location values decode successfully.
func DecodeInventoryDelta(d *Decoder) (InventoryDelta, error) {
	n, err := d.Count(MaxArrayCount)
	if err != nil {
		return InventoryDelta{}, fmt.Errorf("proto: inventory_delta count: %w", err)
	}
	m := InventoryDelta{Items: make([]InventoryEntry, 0, n)}
	for i := 0; i < n; i++ {
		child, err := d.Entry()
		if err != nil {
			return InventoryDelta{}, fmt.Errorf("proto: inventory_delta[%d]: %w", i, err)
		}
		v, err := decodeInventoryEntry(child, "inventory_delta entry")
		if err != nil {
			return InventoryDelta{}, fmt.Errorf("proto: inventory_delta[%d]: %w", i, err)
		}
		child.SkipRemaining()
		m.Items = append(m.Items, v)
	}
	d.SkipRemaining()
	return m, nil
}

// OfferItem is one entry of opcode 212: {item u32, qty u16} = 6 bytes.
// No semantics.
type OfferItem struct {
	Item uint32
	Qty  uint16
}

// OfferItemWireSize is the current encoded byte size of OfferItem.
const OfferItemWireSize = 6

// encode writes the 6-byte OfferItem layout.
func (v OfferItem) encode(e *Encoder) {
	e.U32(v.Item)
	e.U16(v.Qty)
}

// decodeOfferItem reads one OfferItem from the current cursor.
func decodeOfferItem(d *Decoder, context string) (OfferItem, error) {
	var v OfferItem
	var err error
	if v.Item, err = d.U32(); err != nil {
		return OfferItem{}, fmt.Errorf("proto: %s item: %w", context, err)
	}
	if v.Qty, err = d.U16(); err != nil {
		return OfferItem{}, fmt.Errorf("proto: %s qty: %w", context, err)
	}
	return v, nil
}

// OfferUpdate is opcode 212: {with u32, state u8, count u16,
// [entryLen]OfferItem...}. State is raw u8 — no numeric trade-state
// mapping is frozen, so nothing is defined or coerced here.
type OfferUpdate struct {
	With  uint32
	State uint8
	Items []OfferItem
}

// Encode writes with u32 + state u8 + u16 count + framed items.
func (m OfferUpdate) Encode(e *Encoder) {
	e.U32(m.With)
	e.U8(m.State)
	e.Count(len(m.Items), MaxArrayCount)
	for _, v := range m.Items {
		v := v
		e.Entry(func(sub *Encoder) error {
			v.encode(sub)
			return nil
		})
	}
}

// DecodeOfferUpdate reads an OfferUpdate payload, ignoring trailing
// bytes inside each entry and after the message.
func DecodeOfferUpdate(d *Decoder) (OfferUpdate, error) {
	var m OfferUpdate
	var err error
	if m.With, err = d.U32(); err != nil {
		return OfferUpdate{}, fmt.Errorf("proto: offer_update with: %w", err)
	}
	if m.State, err = d.U8(); err != nil {
		return OfferUpdate{}, fmt.Errorf("proto: offer_update state: %w", err)
	}
	n, err := d.Count(MaxArrayCount)
	if err != nil {
		return OfferUpdate{}, fmt.Errorf("proto: offer_update count: %w", err)
	}
	m.Items = make([]OfferItem, 0, n)
	for i := 0; i < n; i++ {
		child, err := d.Entry()
		if err != nil {
			return OfferUpdate{}, fmt.Errorf("proto: offer_update[%d]: %w", i, err)
		}
		v, err := decodeOfferItem(child, "offer_update entry")
		if err != nil {
			return OfferUpdate{}, fmt.Errorf("proto: offer_update[%d]: %w", i, err)
		}
		child.SkipRemaining()
		m.Items = append(m.Items, v)
	}
	d.SkipRemaining()
	return m, nil
}

// ChunkFragment is opcode 218: {cell, chunkIdx u32, fragIdx u16,
// fragCount u16, byteLen u16, bytes u8[byteLen]}. The explicit byteLen
// (spec v0.3.7) bounds the blob so decoders read exactly those bytes
// and can still ignore future msg_version trailing fields. Only the
// byte-size bound is enforced here; fragIdx/fragCount/chunkIdx values
// (including empty Bytes) are streaming semantics, not codec checks.
type ChunkFragment struct {
	Cell      Cell
	ChunkIdx  uint32
	FragIdx   uint16
	FragCount uint16
	Bytes     []byte
}

// Encode writes the exact layout above. Blobs above
// MaxChunkFragmentBytes fail with ErrChunkFragmentTooLarge — never
// truncated, never wrapped.
func (m ChunkFragment) Encode(e *Encoder) {
	e.Cell(m.Cell)
	e.U32(m.ChunkIdx)
	e.U16(m.FragIdx)
	e.U16(m.FragCount)
	if len(m.Bytes) > MaxChunkFragmentBytes {
		e.fail(fmt.Errorf("proto: chunk_fragment bytes=%d max=%d: %w",
			len(m.Bytes), MaxChunkFragmentBytes, ErrChunkFragmentTooLarge))
		return
	}
	e.U16(uint16(len(m.Bytes)))
	e.WriteBytes(m.Bytes)
}

// DecodeChunkFragment reads a ChunkFragment payload. A declared length
// above MaxChunkFragmentBytes fails before any allocation; a declared
// length beyond the remaining bytes fails truncated. Trailing message
// bytes after the blob are ignored.
func DecodeChunkFragment(d *Decoder) (ChunkFragment, error) {
	var m ChunkFragment
	var err error
	if m.Cell, err = d.Cell(); err != nil {
		return ChunkFragment{}, fmt.Errorf("proto: chunk_fragment cell: %w", err)
	}
	if m.ChunkIdx, err = d.U32(); err != nil {
		return ChunkFragment{}, fmt.Errorf("proto: chunk_fragment chunkIdx: %w", err)
	}
	if m.FragIdx, err = d.U16(); err != nil {
		return ChunkFragment{}, fmt.Errorf("proto: chunk_fragment fragIdx: %w", err)
	}
	if m.FragCount, err = d.U16(); err != nil {
		return ChunkFragment{}, fmt.Errorf("proto: chunk_fragment fragCount: %w", err)
	}
	var n uint16
	if n, err = d.U16(); err != nil {
		return ChunkFragment{}, fmt.Errorf("proto: chunk_fragment byteLen: %w", err)
	}
	if int(n) > MaxChunkFragmentBytes {
		return ChunkFragment{}, fmt.Errorf("proto: chunk_fragment bytes=%d max=%d: %w",
			n, MaxChunkFragmentBytes, ErrChunkFragmentTooLarge)
	}
	if m.Bytes, err = d.ReadBytes(int(n)); err != nil {
		return ChunkFragment{}, fmt.Errorf("proto: chunk_fragment bytes: %w", err)
	}
	d.SkipRemaining()
	return m, nil
}

// ShopListingEntry is one entry of opcode 220: {listing u16 stable,
// price u32, qty u16} = 8 bytes. Price stays u32; no DB BIGINT leaks.
// No pricing/stock validation.
type ShopListingEntry struct {
	Listing uint16
	Price   uint32
	Qty     uint16
}

// ShopListingEntryWireSize is the current encoded byte size of ShopListingEntry.
const ShopListingEntryWireSize = 8

// encode writes the 8-byte ShopListingEntry layout.
func (v ShopListingEntry) encode(e *Encoder) {
	e.U16(v.Listing)
	e.U32(v.Price)
	e.U16(v.Qty)
}

// decodeShopListingEntry reads one ShopListingEntry from the cursor.
func decodeShopListingEntry(d *Decoder, context string) (ShopListingEntry, error) {
	var v ShopListingEntry
	var err error
	if v.Listing, err = d.U16(); err != nil {
		return ShopListingEntry{}, fmt.Errorf("proto: %s listing: %w", context, err)
	}
	if v.Price, err = d.U32(); err != nil {
		return ShopListingEntry{}, fmt.Errorf("proto: %s price: %w", context, err)
	}
	if v.Qty, err = d.U16(); err != nil {
		return ShopListingEntry{}, fmt.Errorf("proto: %s qty: %w", context, err)
	}
	return v, nil
}

// ShopList is opcode 220: {vendor u32, count u16,
// [entryLen]shopListing...}. Vendor is a NetEntityID handle; listing is
// a stable per-vendor-proto ID.
type ShopList struct {
	Vendor   uint32
	Listings []ShopListingEntry
}

// Encode writes vendor u32 + u16 count + framed listings.
func (m ShopList) Encode(e *Encoder) {
	e.U32(m.Vendor)
	e.Count(len(m.Listings), MaxArrayCount)
	for _, v := range m.Listings {
		v := v
		e.Entry(func(sub *Encoder) error {
			v.encode(sub)
			return nil
		})
	}
}

// DecodeShopList reads a ShopList payload, ignoring trailing bytes
// inside each entry and after the message.
func DecodeShopList(d *Decoder) (ShopList, error) {
	var m ShopList
	var err error
	if m.Vendor, err = d.U32(); err != nil {
		return ShopList{}, fmt.Errorf("proto: shop_list vendor: %w", err)
	}
	n, err := d.Count(MaxArrayCount)
	if err != nil {
		return ShopList{}, fmt.Errorf("proto: shop_list count: %w", err)
	}
	m.Listings = make([]ShopListingEntry, 0, n)
	for i := 0; i < n; i++ {
		child, err := d.Entry()
		if err != nil {
			return ShopList{}, fmt.Errorf("proto: shop_list[%d]: %w", i, err)
		}
		v, err := decodeShopListingEntry(child, "shop_list entry")
		if err != nil {
			return ShopList{}, fmt.Errorf("proto: shop_list[%d]: %w", i, err)
		}
		child.SkipRemaining()
		m.Listings = append(m.Listings, v)
	}
	d.SkipRemaining()
	return m, nil
}
