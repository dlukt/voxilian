package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync/atomic"

	"github.com/dlukt/voxilian/internal/store/gen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Catalog version sentinels. Match with errors.Is; the accompanying
// *CatalogVersionError carries table/ID/versions.
var (
	ErrCatalogVersionConflict = errors.New("catalog_version_conflict")
	ErrCatalogVersionRollback = errors.New("catalog_version_rollback")
)

// CatalogVersionError contextualizes a version rejection.
type CatalogVersionError struct {
	Table    string
	ID       int32
	Current  int32
	Incoming int32
	Kind     error
}

func (e *CatalogVersionError) Error() string {
	return fmt.Sprintf("store: catalog %s id=%d: current=%d incoming=%d: %v",
		e.Table, e.ID, e.Current, e.Incoming, e.Kind)
}

func (e *CatalogVersionError) Unwrap() error { return e.Kind }

// Store-domain seed record types. These are the future M9 seed API:
// gen types never cross this boundary outward. JSON payloads use
// json.RawMessage; nullable columns use pointers (nil = SQL NULL).
type SpellProtoRecord struct {
	ID       int32
	School   int16
	Level    int16
	Mana     int32
	Exertion int32
	CastMs   int32
	MinHP    int32
	Outlaw   bool
	Harmful  bool
	Reagents json.RawMessage
	Params   json.RawMessage
	Version  int32
}

type SkillProtoRecord struct {
	ID       int32
	Division int16
	Level    int16
	Exertion int32
	Params   json.RawMessage
	Version  int32
}

type ItemProtoRecord struct {
	ID      int32
	Kind    int16
	Slot    *string
	Base    json.RawMessage
	Version int32
}

type ShopListingRecord struct {
	Listing   int32
	ItemProto int32
	Price     int64
	Qty       int32
}

type MobProtoRecord struct {
	ID         int32
	Key        string
	Level      int16
	Difficulty int16
	Karma      int32
	Atk        json.RawMessage
	Resists    json.RawMessage
	Spells     json.RawMessage
	LootTID    *string
	Version    int32
	Listings   []ShopListingRecord
}

// CatalogBatch is atomic: commit only if every supplied record succeeds.
// Tables not represented in the batch are untouched; mobs carry their
// complete listing sets (spec §8.2).
type CatalogBatch struct {
	Spells []SpellProtoRecord
	Skills []SkillProtoRecord
	Items  []ItemProtoRecord
	Mobs   []MobProtoRecord
}

func rawOrEmpty(r json.RawMessage) []byte {
	if len(r) == 0 {
		return []byte("{}")
	}
	return []byte(r)
}

func textPtr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	s := t.String
	return &s
}

func textOrNull(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *s, Valid: true}
}

// upsertSpell applies the version rules inside an open catalog txn.
func upsertSpell(ctx context.Context, q *gen.Queries, r SpellProtoRecord, allowDowngrade bool) error {
	if _, err := q.InsertSpellProtoIfAbsent(ctx, gen.InsertSpellProtoIfAbsentParams{
		ID: r.ID, School: r.School, Level: r.Level, Mana: r.Mana,
		Exertion: r.Exertion, CastMs: r.CastMs, MinHp: r.MinHP,
		Outlaw: r.Outlaw, Harmful: r.Harmful,
		Reagents: rawOrEmpty(r.Reagents), Params: rawOrEmpty(r.Params), Version: r.Version,
	}); err == nil {
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	cur, err := q.GetSpellProtoForUpdate(ctx, r.ID)
	if err != nil {
		return err
	}
	switch {
	case r.Version == cur.Version:
		match, err := q.SpellProtoExactMatch(ctx, gen.SpellProtoExactMatchParams{
			ID: r.ID, School: r.School, Level: r.Level, Mana: r.Mana,
			Exertion: r.Exertion, CastMs: r.CastMs, MinHp: r.MinHP,
			Outlaw: r.Outlaw, Harmful: r.Harmful,
			Column10: rawOrEmpty(r.Reagents), Column11: rawOrEmpty(r.Params), Version: r.Version,
		})
		if err != nil {
			return err
		}
		if match.Valid && match.Bool {
			return nil // strict no-op
		}
		return &CatalogVersionError{Table: "spell_protos", ID: r.ID, Current: cur.Version, Incoming: r.Version, Kind: ErrCatalogVersionConflict}
	case r.Version > cur.Version || allowDowngrade:
		_, err := q.UpdateSpellProto(ctx, gen.UpdateSpellProtoParams{
			ID: r.ID, School: r.School, Level: r.Level, Mana: r.Mana,
			Exertion: r.Exertion, CastMs: r.CastMs, MinHp: r.MinHP,
			Outlaw: r.Outlaw, Harmful: r.Harmful,
			Reagents: rawOrEmpty(r.Reagents), Params: rawOrEmpty(r.Params), Version: r.Version,
		})
		return err
	default:
		return &CatalogVersionError{Table: "spell_protos", ID: r.ID, Current: cur.Version, Incoming: r.Version, Kind: ErrCatalogVersionRollback}
	}
}

func upsertSkill(ctx context.Context, q *gen.Queries, r SkillProtoRecord, allowDowngrade bool) error {
	if _, err := q.InsertSkillProtoIfAbsent(ctx, gen.InsertSkillProtoIfAbsentParams{
		ID: r.ID, Division: r.Division, Level: r.Level,
		Exertion: r.Exertion, Params: rawOrEmpty(r.Params), Version: r.Version,
	}); err == nil {
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	cur, err := q.GetSkillProtoForUpdate(ctx, r.ID)
	if err != nil {
		return err
	}
	switch {
	case r.Version == cur.Version:
		match, err := q.SkillProtoExactMatch(ctx, gen.SkillProtoExactMatchParams{
			ID: r.ID, Division: r.Division, Level: r.Level,
			Exertion: r.Exertion, Column5: rawOrEmpty(r.Params), Version: r.Version,
		})
		if err != nil {
			return err
		}
		if match.Valid && match.Bool {
			return nil
		}
		return &CatalogVersionError{Table: "skill_protos", ID: r.ID, Current: cur.Version, Incoming: r.Version, Kind: ErrCatalogVersionConflict}
	case r.Version > cur.Version || allowDowngrade:
		_, err := q.UpdateSkillProto(ctx, gen.UpdateSkillProtoParams{
			ID: r.ID, Division: r.Division, Level: r.Level,
			Exertion: r.Exertion, Params: rawOrEmpty(r.Params), Version: r.Version,
		})
		return err
	default:
		return &CatalogVersionError{Table: "skill_protos", ID: r.ID, Current: cur.Version, Incoming: r.Version, Kind: ErrCatalogVersionRollback}
	}
}

func upsertItem(ctx context.Context, q *gen.Queries, r ItemProtoRecord, allowDowngrade bool) error {
	if _, err := q.InsertItemProtoIfAbsent(ctx, gen.InsertItemProtoIfAbsentParams{
		ID: r.ID, Kind: r.Kind, Slot: textOrNull(r.Slot),
		Base: rawOrEmpty(r.Base), Version: r.Version,
	}); err == nil {
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	cur, err := q.GetItemProtoForUpdate(ctx, r.ID)
	if err != nil {
		return err
	}
	switch {
	case r.Version == cur.Version:
		match, err := q.ItemProtoExactMatch(ctx, gen.ItemProtoExactMatchParams{
			ID: r.ID, Kind: r.Kind, Slot: textOrNull(r.Slot),
			Column4: rawOrEmpty(r.Base), Version: r.Version,
		})
		if err != nil {
			return err
		}
		if match.Valid && match.Bool {
			return nil
		}
		return &CatalogVersionError{Table: "item_protos", ID: r.ID, Current: cur.Version, Incoming: r.Version, Kind: ErrCatalogVersionConflict}
	case r.Version > cur.Version || allowDowngrade:
		_, err := q.UpdateItemProto(ctx, gen.UpdateItemProtoParams{
			ID: r.ID, Kind: r.Kind, Slot: textOrNull(r.Slot),
			Base: rawOrEmpty(r.Base), Version: r.Version,
		})
		return err
	default:
		return &CatalogVersionError{Table: "item_protos", ID: r.ID, Current: cur.Version, Incoming: r.Version, Kind: ErrCatalogVersionRollback}
	}
}

// listingsEqual compares keyed by listing ID; source order is irrelevant.
func listingsEqual(a []gen.ShopListing, b []ShopListingRecord) bool {
	if len(a) != len(b) {
		return false
	}
	byID := make(map[int32]ShopListingRecord, len(b))
	for _, l := range b {
		byID[l.Listing] = l
	}
	for _, row := range a {
		want, ok := byID[row.Listing]
		if !ok || want.ItemProto != row.ItemProto || want.Price != row.Price || want.Qty != row.Qty {
			return false
		}
	}
	return true
}

func upsertMob(ctx context.Context, q *gen.Queries, r MobProtoRecord, allowDowngrade bool) error {
	if _, err := q.InsertMobProtoIfAbsent(ctx, gen.InsertMobProtoIfAbsentParams{
		ID: r.ID, Key: r.Key, Level: r.Level, Difficulty: r.Difficulty,
		Karma: r.Karma, Atk: rawOrEmpty(r.Atk), Resists: rawOrEmpty(r.Resists),
		Spells: rawOrEmpty(r.Spells), LootTid: textOrNull(r.LootTID), Version: r.Version,
	}); err == nil {
		return replaceListings(ctx, q, r)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	cur, err := q.GetMobProtoForUpdate(ctx, r.ID)
	if err != nil {
		return err
	}
	accept := r.Version > cur.Version || (allowDowngrade && r.Version != cur.Version)
	if r.Version == cur.Version {
		match, err := q.MobProtoExactMatch(ctx, gen.MobProtoExactMatchParams{
			ID: r.ID, Key: r.Key, Level: r.Level, Difficulty: r.Difficulty,
			Karma: r.Karma, Column6: rawOrEmpty(r.Atk), Column7: rawOrEmpty(r.Resists), Column8: rawOrEmpty(r.Spells), LootTid: textOrNull(r.LootTID), Version: r.Version,
		})
		if err != nil {
			return err
		}
		stored, err := q.ListShopListingsByVendor(ctx, r.ID)
		if err != nil {
			return err
		}
		if match.Valid && match.Bool && listingsEqual(stored, r.Listings) {
			return nil // strict no-op: proto and set identical
		}
		return &CatalogVersionError{Table: "mob_protos", ID: r.ID, Current: cur.Version, Incoming: r.Version, Kind: ErrCatalogVersionConflict}
	}
	if !accept {
		return &CatalogVersionError{Table: "mob_protos", ID: r.ID, Current: cur.Version, Incoming: r.Version, Kind: ErrCatalogVersionRollback}
	}
	if _, err := q.UpdateMobProto(ctx, gen.UpdateMobProtoParams{
		ID: r.ID, Key: r.Key, Level: r.Level, Difficulty: r.Difficulty,
		Karma: r.Karma, Atk: rawOrEmpty(r.Atk), Resists: rawOrEmpty(r.Resists),
		Spells: rawOrEmpty(r.Spells), LootTid: textOrNull(r.LootTID), Version: r.Version,
	}); err != nil {
		return err
	}
	return replaceListings(ctx, q, r)
}

// replaceListings swaps a vendor's whole listing set (empty set clears).
func replaceListings(ctx context.Context, q *gen.Queries, r MobProtoRecord) error {
	if err := q.DeleteShopListingsByVendor(ctx, r.ID); err != nil {
		return err
	}
	for _, l := range r.Listings {
		if err := q.InsertShopListing(ctx, gen.InsertShopListingParams{
			VendorID: r.ID, Listing: l.Listing, ItemProto: l.ItemProto,
			Price: l.Price, Qty: l.Qty,
		}); err != nil {
			return err
		}
	}
	return nil
}

// UpsertCatalogBatch applies a version-ruled batch atomically: spells,
// skills, items, then mobs with listing sets. Items precede listings so
// listing FKs resolve. One transaction; any failure rolls everything back.
func UpsertCatalogBatch(ctx context.Context, pool *pgxpool.Pool, batch CatalogBatch, allowDowngrade bool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: catalog batch: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := gen.New(tx)
	for _, r := range batch.Spells {
		if err := upsertSpell(ctx, q, r, allowDowngrade); err != nil {
			return err
		}
	}
	for _, r := range batch.Skills {
		if err := upsertSkill(ctx, q, r, allowDowngrade); err != nil {
			return err
		}
	}
	for _, r := range batch.Items {
		if err := upsertItem(ctx, q, r, allowDowngrade); err != nil {
			return err
		}
	}
	for _, r := range batch.Mobs {
		if err := upsertMob(ctx, q, r, allowDowngrade); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: catalog batch: commit: %w", err)
	}
	return nil
}

// Immutable registry value types. JSON payloads are strings (immutable);
// nullable columns are pointers; listing slices are copied on access.
type SpellProto struct {
	ID               int32
	School, Level    int16
	Mana, Exertion   int32
	CastMs           int32
	MinHP            int32
	Outlaw, Harmful  bool
	Reagents, Params string
	Version          int32
}

type SkillProto struct {
	ID              int32
	Division, Level int16
	Exertion        int32
	Params          string
	Version         int32
}

type ItemProto struct {
	ID      int32
	Kind    int16
	Slot    *string
	Base    string
	Version int32
}

type MobProto struct {
	ID         int32
	Key        string
	Level      int16
	Difficulty int16
	Karma      int32
	Atk        string
	Resists    string
	Spells     string
	LootTID    *string
	Version    int32
}

type ShopListing struct {
	VendorID  int32
	Listing   int32
	ItemProto int32
	Price     int64
	Qty       int32
}

// shopListingKey addresses one listing directly: O(1) pair lookup
// alongside the per-vendor slices used for enumeration.
type shopListingKey struct {
	VendorID  int32
	ListingID int32
}

// CatalogRegistry is immutable after construction: no maps escape, JSON
// is string-typed, and ShopListings returns a copy.
type CatalogRegistry struct {
	spells       map[int32]SpellProto
	skills       map[int32]SkillProto
	items        map[int32]ItemProto
	mobs         map[int32]MobProto
	listings     map[int32][]ShopListing
	listingByKey map[shopListingKey]ShopListing
}

func (r *CatalogRegistry) Spell(id int32) (SpellProto, bool) {
	v, ok := r.spells[id]
	return v, ok
}

func (r *CatalogRegistry) Skill(id int32) (SkillProto, bool) {
	v, ok := r.skills[id]
	return v, ok
}

func (r *CatalogRegistry) Item(id int32) (ItemProto, bool) {
	v, ok := r.items[id]
	if ok && v.Slot != nil {
		c := *v.Slot
		v.Slot = &c
	}
	return v, ok
}

func (r *CatalogRegistry) Mob(id int32) (MobProto, bool) {
	v, ok := r.mobs[id]
	if ok && v.LootTID != nil {
		c := *v.LootTID
		v.LootTID = &c
	}
	return v, ok
}

func (r *CatalogRegistry) ShopListing(vendorID, listingID int32) (ShopListing, bool) {
	// Direct map lookup: O(1). ShopListing holds only value fields, so
	// the returned copy cannot alias mutable registry backing state.
	v, ok := r.listingByKey[shopListingKey{VendorID: vendorID, ListingID: listingID}]
	return v, ok
}

// ShopListings returns a copy in listing-ID order; mutating it cannot
// affect the registry.
func (r *CatalogRegistry) ShopListings(vendorID int32) []ShopListing {
	src := r.listings[vendorID]
	out := make([]ShopListing, len(src))
	copy(out, src)
	sort.Slice(out, func(i, j int) bool { return out[i].Listing < out[j].Listing })
	return out
}

// LoadCatalogRegistry reads all five catalogs under ONE repeatable-read
// read-only transaction: a concurrent reseed can never produce a mixed
// registry.
func LoadCatalogRegistry(ctx context.Context, pool *pgxpool.Pool) (*CatalogRegistry, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("store: load registry: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := gen.New(tx)

	spells, err := q.ListAllSpellProtos(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: load registry: spells: %w", err)
	}
	skills, err := q.ListAllSkillProtos(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: load registry: skills: %w", err)
	}
	items, err := q.ListAllItemProtos(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: load registry: items: %w", err)
	}
	mobs, err := q.ListAllMobProtos(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: load registry: mobs: %w", err)
	}
	listings, err := q.ListAllShopListings(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: load registry: listings: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: load registry: commit: %w", err)
	}

	reg := &CatalogRegistry{
		spells:       make(map[int32]SpellProto, len(spells)),
		skills:       make(map[int32]SkillProto, len(skills)),
		items:        make(map[int32]ItemProto, len(items)),
		mobs:         make(map[int32]MobProto, len(mobs)),
		listings:     make(map[int32][]ShopListing),
		listingByKey: make(map[shopListingKey]ShopListing, len(listings)),
	}
	for _, s := range spells {
		reg.spells[s.ID] = SpellProto{ID: s.ID, School: s.School, Level: s.Level,
			Mana: s.Mana, Exertion: s.Exertion, CastMs: s.CastMs, MinHP: s.MinHp,
			Outlaw: s.Outlaw, Harmful: s.Harmful,
			Reagents: string(s.Reagents), Params: string(s.Params), Version: s.Version}
	}
	for _, s := range skills {
		reg.skills[s.ID] = SkillProto{ID: s.ID, Division: s.Division, Level: s.Level,
			Exertion: s.Exertion, Params: string(s.Params), Version: s.Version}
	}
	for _, s := range items {
		reg.items[s.ID] = ItemProto{ID: s.ID, Kind: s.Kind, Slot: textPtr(s.Slot),
			Base: string(s.Base), Version: s.Version}
	}
	for _, s := range mobs {
		reg.mobs[s.ID] = MobProto{ID: s.ID, Key: s.Key, Level: s.Level,
			Difficulty: s.Difficulty, Karma: s.Karma, Atk: string(s.Atk),
			Resists: string(s.Resists), Spells: string(s.Spells),
			LootTID: textPtr(s.LootTid), Version: s.Version}
	}
	for _, l := range listings {
		row := ShopListing{
			VendorID: l.VendorID, Listing: l.Listing, ItemProto: l.ItemProto,
			Price: l.Price, Qty: l.Qty,
		}
		reg.listings[l.VendorID] = append(reg.listings[l.VendorID], row)
		reg.listingByKey[shopListingKey{VendorID: l.VendorID, ListingID: l.Listing}] = row
	}
	for v := range reg.listings {
		sort.Slice(reg.listings[v], func(i, j int) bool {
			return reg.listings[v][i].Listing < reg.listings[v][j].Listing
		})
	}
	return reg, nil
}

// CatalogRegistryRef holds the live registry pointer; reseed swaps it
// atomically. Readers may retain old snapshots; the registry is immutable.
type CatalogRegistryRef struct {
	ptr atomic.Pointer[CatalogRegistry]
}

// NewCatalogRegistryRef creates a holder around an initial registry.
func NewCatalogRegistryRef(initial *CatalogRegistry) *CatalogRegistryRef {
	r := &CatalogRegistryRef{}
	r.ptr.Store(initial)
	return r
}

// Load returns the current registry snapshot.
func (r *CatalogRegistryRef) Load() *CatalogRegistry { return r.ptr.Load() }

// Swap installs a validated replacement registry.
func (r *CatalogRegistryRef) Swap(next *CatalogRegistry) { r.ptr.Store(next) }
