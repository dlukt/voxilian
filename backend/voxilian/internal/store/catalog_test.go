package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func strp(s string) *string { return &s }

// jsonEqual compares JSON semantically (PostgreSQL JSONB normalizes
// whitespace/key order on read, so byte comparison is wrong).
func jsonEqual(t *testing.T, got, want string) bool {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("unmarshal got: %v", err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("unmarshal want: %v", err)
	}
	return reflect.DeepEqual(g, w)
}

func spellRec(id, ver, mana int32, reagents string) SpellProtoRecord {
	return SpellProtoRecord{ID: id, School: 1, Level: 1, Mana: mana,
		Exertion: 2, CastMs: 0, MinHP: 1, Reagents: json.RawMessage(reagents),
		Params: json.RawMessage(`{}`), Version: ver}
}

func skillRec(id, ver int32) SkillProtoRecord {
	return SkillProtoRecord{ID: id, Division: 1, Level: 1, Exertion: 2,
		Params: json.RawMessage(`{}`), Version: ver}
}

func itemRec(id int32, ver int32, slot *string) ItemProtoRecord {
	return ItemProtoRecord{ID: id, Kind: 0, Slot: slot,
		Base: json.RawMessage(`{}`), Version: ver}
}

func mobRec(id int32, ver int32, key string, listings ...ShopListingRecord) MobProtoRecord {
	return MobProtoRecord{ID: id, Key: key, Level: 45, Difficulty: 6,
		Karma: -40, Atk: json.RawMessage(`{}`), Resists: json.RawMessage(`{}`),
		Spells: json.RawMessage(`{}`), Version: ver, Listings: listings}
}

func seedBatch() CatalogBatch {
	return CatalogBatch{
		Spells: []SpellProtoRecord{spellRec(1, 1, 10, `{"a":1}`), spellRec(2, 1, 20, `{}`)},
		Skills: []SkillProtoRecord{skillRec(1, 1)},
		Items:  []ItemProtoRecord{itemRec(100, 1, strp("mainhand")), itemRec(101, 1, nil)},
		Mobs: []MobProtoRecord{
			{ID: 200, Key: "vendor-a", Level: 45, Difficulty: 6, Karma: -40,
				Atk: json.RawMessage(`{}`), Resists: json.RawMessage(`{}`),
				Spells: json.RawMessage(`{}`), LootTID: strp("TID_ORC"), Version: 1,
				Listings: []ShopListingRecord{{Listing: 20, ItemProto: 100, Price: 150, Qty: 10}, {Listing: 10, ItemProto: 101, Price: 50, Qty: 5}}},
			mobRec(201, 1, "plain-mob"),
		},
	}
}

func mustUpsert(t *testing.T, pool *pgxpool.Pool, b CatalogBatch, allow bool) {
	t.Helper()
	if err := UpsertCatalogBatch(context.Background(), pool, b, allow); err != nil {
		t.Fatalf("upsert: %v", err)
	}
}

func xminOf(t *testing.T, pool *pgxpool.Pool, table, where string, args ...any) string {
	t.Helper()
	var x string
	if err := pool.QueryRow(context.Background(),
		`SELECT xmin::text FROM `+table+` WHERE `+where, args...).Scan(&x); err != nil {
		t.Fatalf("xmin %s: %v", table, err)
	}
	return x
}

func TestCatalogRegistryLoad(t *testing.T) {
	pool, _ := openQueries(t)
	ctx := context.Background()
	mustUpsert(t, pool, seedBatch(), false)

	reg, err := LoadCatalogRegistry(ctx, pool)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	sp, ok := reg.Spell(1)
	if !ok || sp.Mana != 10 || sp.Version != 1 || !jsonEqual(t, sp.Reagents, `{"a":1}`) {
		t.Fatalf("spell = %+v, %v", sp, ok)
	}
	if _, ok := reg.Spell(9999); ok {
		t.Fatal("unknown spell found")
	}
	sk, ok := reg.Skill(1)
	if !ok || sk.Division != 1 {
		t.Fatalf("skill = %+v, %v", sk, ok)
	}
	it, ok := reg.Item(100)
	if !ok || it.Slot == nil || *it.Slot != "mainhand" {
		t.Fatalf("item = %+v, %v", it, ok)
	}
	it, ok = reg.Item(101)
	if !ok || it.Slot != nil {
		t.Fatalf("null slot = %+v, %v", it, ok)
	}
	mb, ok := reg.Mob(200)
	if !ok || mb.Key != "vendor-a" || mb.LootTID == nil || *mb.LootTID != "TID_ORC" {
		t.Fatalf("mob = %+v, %v", mb, ok)
	}
	if _, ok := reg.Mob(9999); ok {
		t.Fatal("unknown mob found")
	}
	l, ok := reg.ShopListing(200, 10)
	if !ok || l.Price != 50 || l.ItemProto != 101 {
		t.Fatalf("listing = %+v, %v", l, ok)
	}
	if _, ok := reg.ShopListing(200, 999); ok {
		t.Fatal("unknown listing found")
	}
	got := reg.ShopListings(200)
	if len(got) != 2 || got[0].Listing != 10 || got[1].Listing != 20 {
		t.Fatalf("listings order = %+v", got)
	}
}

func TestCatalogRegistryImmutable(t *testing.T) {
	pool, _ := openQueries(t)
	ctx := context.Background()
	mustUpsert(t, pool, seedBatch(), false)
	reg, err := LoadCatalogRegistry(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	// Mutate returned slice + pointed-to strings; registry must not change.
	ls := reg.ShopListings(200)
	ls[0].Price = 0
	ls[0].Listing = 999
	mb, _ := reg.Mob(200)
	*mb.LootTID = "MUTATED"
	it, _ := reg.Item(100)
	*it.Slot = "MUTATED"
	again := reg.ShopListings(200)
	if again[0].Price != 50 || again[0].Listing != 10 {
		t.Fatalf("listings mutated: %+v", again)
	}
	mb2, _ := reg.Mob(200)
	if *mb2.LootTID != "TID_ORC" {
		t.Fatalf("loot mutated: %q", *mb2.LootTID)
	}
	it2, _ := reg.Item(100)
	if *it2.Slot != "mainhand" {
		t.Fatalf("slot mutated: %q", *it2.Slot)
	}
}

func TestCatalogHolderSwap(t *testing.T) {
	pool, _ := openQueries(t)
	ctx := context.Background()
	mustUpsert(t, pool, seedBatch(), false)
	a, err := LoadCatalogRegistry(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	holder := NewCatalogRegistryRef(a)
	if holder.Load() != a {
		t.Fatal("holder initial")
	}
	mustUpsert(t, pool, CatalogBatch{Spells: []SpellProtoRecord{spellRec(1, 2, 11, `{"a":1}`)}}, false)
	b, err := LoadCatalogRegistry(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	holder.Swap(b)
	if holder.Load() != b {
		t.Fatal("holder swap")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = holder.Load()
				if j%2 == 0 {
					holder.Swap(a)
				} else {
					holder.Swap(b)
				}
			}
		}()
	}
	wg.Wait()
}

func TestCatalogStrictNoOp(t *testing.T) {
	pool, _ := openQueries(t)
	b := seedBatch()
	mustUpsert(t, pool, b, false)
	sx := xminOf(t, pool, "spell_protos", "id=1")
	mx := xminOf(t, pool, "mob_protos", "id=200")
	lx := xminOf(t, pool, "shop_listings", "vendor_id=200 AND listing=10")
	mustUpsert(t, pool, b, false) // identical rerun
	if got := xminOf(t, pool, "spell_protos", "id=1"); got != sx {
		t.Fatalf("spell xmin %s → %s (row touched)", sx, got)
	}
	if got := xminOf(t, pool, "mob_protos", "id=200"); got != mx {
		t.Fatalf("mob xmin %s → %s (row touched)", mx, got)
	}
	if got := xminOf(t, pool, "shop_listings", "vendor_id=200 AND listing=10"); got != lx {
		t.Fatalf("listing xmin %s → %s (row touched)", lx, got)
	}
}

func TestCatalogJSONSemanticNoOp(t *testing.T) {
	pool, _ := openQueries(t)
	mustUpsert(t, pool, CatalogBatch{Spells: []SpellProtoRecord{spellRec(5, 1, 10, `{"a":1,"b":2}`)}}, false)
	sx := xminOf(t, pool, "spell_protos", "id=5")
	mustUpsert(t, pool, CatalogBatch{Spells: []SpellProtoRecord{spellRec(5, 1, 10, `{ "b": 2, "a": 1 }`)}}, false)
	if got := xminOf(t, pool, "spell_protos", "id=5"); got != sx {
		t.Fatalf("json-textual rerun touched row: %s → %s", sx, got)
	}
}

func TestCatalogSameVersionConflict(t *testing.T) {
	pool, _ := openQueries(t)
	ctx := context.Background()
	mustUpsert(t, pool, seedBatch(), false)

	// Proto field change, same version.
	err := UpsertCatalogBatch(ctx, pool, CatalogBatch{Spells: []SpellProtoRecord{spellRec(1, 1, 11, `{"a":1}`)}}, false)
	var cv *CatalogVersionError
	if !errors.As(err, &cv) || !errors.Is(err, ErrCatalogVersionConflict) {
		t.Fatalf("err = %v, want conflict", err)
	}
	if cv.Table != "spell_protos" || cv.ID != 1 || cv.Current != 1 || cv.Incoming != 1 {
		t.Fatalf("details = %+v", cv)
	}
	// Row untouched.
	reg, _ := LoadCatalogRegistry(ctx, pool)
	sp, _ := reg.Spell(1)
	if sp.Mana != 10 {
		t.Fatalf("conflict mutated row: %+v", sp)
	}

	// Vendor listing price change, same version.
	err = UpsertCatalogBatch(ctx, pool, CatalogBatch{Mobs: []MobProtoRecord{{
		ID: 200, Key: "vendor-a", Level: 45, Difficulty: 6, Karma: -40,
		Atk: json.RawMessage(`{}`), Resists: json.RawMessage(`{}`), Spells: json.RawMessage(`{}`),
		LootTID: strp("TID_ORC"), Version: 1,
		Listings: []ShopListingRecord{{Listing: 20, ItemProto: 100, Price: 101, Qty: 10}, {Listing: 10, ItemProto: 101, Price: 50, Qty: 5}},
	}}}, false)
	if !errors.Is(err, ErrCatalogVersionConflict) {
		t.Fatalf("listing change err = %v, want conflict", err)
	}

	// Listing removal, same version.
	err = UpsertCatalogBatch(ctx, pool, CatalogBatch{Mobs: []MobProtoRecord{{
		ID: 200, Key: "vendor-a", Level: 45, Difficulty: 6, Karma: -40,
		Atk: json.RawMessage(`{}`), Resists: json.RawMessage(`{}`), Spells: json.RawMessage(`{}`),
		LootTID: strp("TID_ORC"), Version: 1,
		Listings: []ShopListingRecord{{Listing: 10, ItemProto: 101, Price: 50, Qty: 5}},
	}}}, false)
	if !errors.Is(err, ErrCatalogVersionConflict) {
		t.Fatalf("listing removal err = %v, want conflict", err)
	}
}

func TestCatalogNewerVersion(t *testing.T) {
	pool, _ := openQueries(t)
	ctx := context.Background()
	mustUpsert(t, pool, seedBatch(), false)

	mustUpsert(t, pool, CatalogBatch{
		Spells: []SpellProtoRecord{spellRec(1, 2, 42, `{"a":9}`)},
		Mobs: []MobProtoRecord{{
			ID: 200, Key: "vendor-a", Level: 46, Difficulty: 6, Karma: -40,
			Atk: json.RawMessage(`{}`), Resists: json.RawMessage(`{}`), Spells: json.RawMessage(`{}`),
			LootTID: strp("TID_ORC"), Version: 2,
			// price changed, listing 10 removed, listing 30 added
			Listings: []ShopListingRecord{{Listing: 20, ItemProto: 100, Price: 200, Qty: 10}, {Listing: 30, ItemProto: 101, Price: 5, Qty: 1}},
		}},
	}, false)

	reg, err := LoadCatalogRegistry(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	sp, _ := reg.Spell(1)
	if sp.Version != 2 || sp.Mana != 42 || !jsonEqual(t, sp.Reagents, `{"a":9}`) {
		t.Fatalf("spell = %+v", sp)
	}
	got := reg.ShopListings(200)
	if len(got) != 2 || got[0].Listing != 20 || got[0].Price != 200 || got[1].Listing != 30 {
		t.Fatalf("listings = %+v", got)
	}
}

func TestCatalogRollbackAndForcedDowngrade(t *testing.T) {
	pool, _ := openQueries(t)
	ctx := context.Background()
	mustUpsert(t, pool, CatalogBatch{Spells: []SpellProtoRecord{spellRec(9, 5, 10, `{}`)}}, false)

	err := UpsertCatalogBatch(ctx, pool, CatalogBatch{Spells: []SpellProtoRecord{spellRec(9, 4, 11, `{}`)}}, false)
	var cv *CatalogVersionError
	if !errors.As(err, &cv) || !errors.Is(err, ErrCatalogVersionRollback) {
		t.Fatalf("err = %v, want rollback", err)
	}
	if cv.Table != "spell_protos" || cv.ID != 9 || cv.Current != 5 || cv.Incoming != 4 {
		t.Fatalf("details = %+v", cv)
	}
	reg, _ := LoadCatalogRegistry(ctx, pool)
	if sp, _ := reg.Spell(9); sp.Version != 5 || sp.Mana != 10 {
		t.Fatalf("rollback mutated: %+v", sp)
	}

	mustUpsert(t, pool, CatalogBatch{Spells: []SpellProtoRecord{spellRec(9, 4, 11, `{}`)}}, true)
	reg, _ = LoadCatalogRegistry(ctx, pool)
	if sp, _ := reg.Spell(9); sp.Version != 4 || sp.Mana != 11 {
		t.Fatalf("forced = %+v", sp)
	}
}

func TestCatalogBatchAtomic(t *testing.T) {
	pool, _ := openQueries(t)
	ctx := context.Background()
	mustUpsert(t, pool, CatalogBatch{
		Spells: []SpellProtoRecord{spellRec(11, 3, 10, `{}`)},
		Items:  []ItemProtoRecord{itemRec(110, 1, nil)},
		Mobs: []MobProtoRecord{{
			ID: 210, Key: "atomic-v", Level: 1, Difficulty: 1, Karma: 0,
			Atk: json.RawMessage(`{}`), Resists: json.RawMessage(`{}`), Spells: json.RawMessage(`{}`),
			Version:  1,
			Listings: []ShopListingRecord{{Listing: 1, ItemProto: 110, Price: 1, Qty: 1}},
		}},
	}, false)

	// Earlier valid update + later conflict → whole batch rolls back.
	err := UpsertCatalogBatch(ctx, pool, CatalogBatch{
		Spells: []SpellProtoRecord{spellRec(11, 4, 99, `{}`), spellRec(11, 4, 100, `{}`)},
	}, false)
	if !errors.Is(err, ErrCatalogVersionConflict) {
		t.Fatalf("err = %v, want conflict", err)
	}
	reg, _ := LoadCatalogRegistry(ctx, pool)
	if sp, _ := reg.Spell(11); sp.Version != 3 || sp.Mana != 10 {
		t.Fatalf("partial commit: %+v", sp)
	}

	// PG error (listing references missing item) rolls back earlier writes.
	err = UpsertCatalogBatch(ctx, pool, CatalogBatch{
		Spells: []SpellProtoRecord{spellRec(11, 4, 99, `{}`)},
		Mobs: []MobProtoRecord{{
			ID: 211, Key: "atomic-w", Level: 1, Difficulty: 1, Karma: 0,
			Atk: json.RawMessage(`{}`), Resists: json.RawMessage(`{}`), Spells: json.RawMessage(`{}`),
			Version:  1,
			Listings: []ShopListingRecord{{Listing: 1, ItemProto: 9999, Price: 1, Qty: 1}},
		}},
	}, false)
	if err == nil {
		t.Fatal("bad listing FK accepted")
	}
	reg, _ = LoadCatalogRegistry(ctx, pool)
	if sp, _ := reg.Spell(11); sp.Version != 3 {
		t.Fatalf("FK failure committed partial batch: %+v", sp)
	}
	if _, ok := reg.Mob(211); ok {
		t.Fatal("failed vendor committed")
	}
}

func TestCatalogExplicitIDs(t *testing.T) {
	pool, _ := openQueries(t)
	ctx := context.Background()
	mustUpsert(t, pool, CatalogBatch{
		Spells: []SpellProtoRecord{spellRec(1, 1, 1, `{}`), spellRec(65535, 1, 1, `{}`)},
		Items:  []ItemProtoRecord{itemRec(65535, 1, nil)},
	}, false)
	reg, err := LoadCatalogRegistry(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Spell(1); !ok {
		t.Fatal("id 1 missing")
	}
	if _, ok := reg.Spell(65535); !ok {
		t.Fatal("id 65535 missing")
	}
}
