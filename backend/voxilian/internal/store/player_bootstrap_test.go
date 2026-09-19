package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/dlukt/voxilian/internal/store/gen"
	"github.com/jackc/pgx/v5"
)

// M5-T5c4 reconnect bootstrap recovery tests (spec §9.5.1m
// C2/C3/C4). All proofs run against real PostgreSQL 18:
// the dedicated LoadPlayerBootstrapRecovery read observes
// one coherent snapshot with the exact carried inventory
// set, real base stats, and the optional pending child.
// Soft-deleted characters are rejected; every error
// returns zero staged state.

func TestLoadPlayerBootstrapRecovery(t *testing.T) {
	ctx := context.Background()
	pool, q := openQueries(t)
	st := newTestStore(t, pool)

	if _, err := pool.Exec(ctx, `INSERT INTO spell_protos (id,school,level,mana,exertion,cast_ms,min_hp,outlaw,harmful,reagents,params,version) VALUES (1,1,1,1,1,0,1,false,false,'{}','{}',1),(2,2,1,1,1,0,1,false,false,'{}','{}',1) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO skill_protos (id,division,level,exertion,params,version) VALUES (1,1,1,1,'{}',1),(2,2,1,1,'{}',1) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO item_protos (id,kind,slot,base,version) VALUES (1001,0,NULL,'{}',1),(1002,0,NULL,'{}',1),(1003,0,NULL,'{}',1),(1004,0,NULL,'{}',1) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	acct, err := q.CreateAccount(ctx, gen.CreateAccountParams{KeycloakSub: "sub-bootstrap"})
	if err != nil {
		t.Fatal(err)
	}
	params := validCharParams(acct.ID, 0, "BootstrapChar")
	// Deliberately NOT 10/10: runtime-input resolution
	// must observe the real durable base stats.
	params.Stamina = 7
	params.Mysticism = 42
	params.Karma = 11
	params.PosX, params.PosY, params.PosZ = 400500, -1000, -300250
	params.Vitals = []byte(`{"hp":20,"base_max":20,"max":20,"mana":20,"max_mana":20,"vigor":100,"threshold":80,"exertion":0,"stomach":0}`)
	params.Advancement = []byte(`{"pts":3}`)
	params.Flags = 5
	ch, err := createCharacter(ctx, q, params)
	if err != nil {
		t.Fatal(err)
	}
	charID := ch.ID
	if _, err := pool.Exec(ctx, `INSERT INTO character_spells (character_id,spell_id,ability,atrophy_flag) VALUES ($1,1,50,true)`, charID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO character_skills (character_id,skill_id,ability,atrophy_flag) VALUES ($1,2,20,false)`, charID); err != nil {
		t.Fatal(err)
	}
	bag, _, err := createItemWithLocation(ctx, pool,
		gen.InsertItemInstanceParams{Proto: 1001, Qty: 3, Hits: 250, Enchants: []byte(`{"glow":1}`)},
		NewItemLocation{Kind: 0, CharacterID: int8(charID), Slot: pgText("hand")})
	if err != nil {
		t.Fatal(err)
	}
	sword, _, err := createItemWithLocation(ctx, pool,
		gen.InsertItemInstanceParams{Proto: 1002, Qty: 500, Hits: 0, Enchants: []byte(`{}`)},
		NewItemLocation{Kind: 0, CharacterID: int8(charID), Slot: pgText("pack")})
	if err != nil {
		t.Fatal(err)
	}
	// Contained, vault, and ground rows are never carried
	// inventory and must not leak into the bootstrap.
	if _, _, err := createItemWithLocation(ctx, pool,
		gen.InsertItemInstanceParams{Proto: 1003, Qty: 1, Hits: 100, Enchants: []byte(`{"bane":true}`)},
		NewItemLocation{Kind: 4, ContainerItemID: int8(bag.ID), Slot: pgText("pocket")}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := createItemWithLocation(ctx, pool,
		gen.InsertItemInstanceParams{Proto: 1004, Qty: 7, Hits: 7, Enchants: []byte(`{}`)},
		NewItemLocation{Kind: 3, CharacterID: int8(charID), VaultRegion: pgText("tos"), Slot: pgText("vault")}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO pending_deaths (character_id,effective_cost,death_time_seconds,corpse_id,portal_used) VALUES ($1,35,2000,NULL,true)`, charID); err != nil {
		t.Fatal(err)
	}

	got, err := st.LoadPlayerBootstrapRecovery(ctx, charID)
	if err != nil {
		t.Fatalf("LoadPlayerBootstrapRecovery: %v", err)
	}
	if got.CharacterID != charID {
		t.Fatalf("char = %d; want %d", got.CharacterID, charID)
	}
	if got.Stamina != 7 || got.Mysticism != 42 {
		t.Fatalf("base stats = %d/%d; want 7/42", got.Stamina, got.Mysticism)
	}
	if got.Karma != 11 || got.Flags != 5 {
		t.Fatalf("karma/flags = %d/%d; want 11/5", got.Karma, got.Flags)
	}
	if got.PosX != 400500 || got.PosY != -1000 || got.PosZ != -300250 {
		t.Fatalf("pos = %d/%d/%d", got.PosX, got.PosY, got.PosZ)
	}
	if string(got.Advancement) != `{"pts":3}` && string(got.Advancement) != `{"pts": 3}` {
		t.Fatalf("advancement = %s", got.Advancement)
	}
	if len(got.Spells) != 1 || got.Spells[0].SpellID != 1 || got.Spells[0].Ability != 50 || !got.Spells[0].AtrophyFlag {
		t.Fatalf("spells = %+v", got.Spells)
	}
	if len(got.Skills) != 1 || got.Skills[0].SkillID != 2 || got.Skills[0].Ability != 20 {
		t.Fatalf("skills = %+v", got.Skills)
	}
	// C2: exactly the directly character-owned set in
	// ascending item-id order (bag created before sword).
	// Enchants compare JSON-semantically: JSONB storage
	// normalizes whitespace.
	wantItems := []PlayerBootstrapItemSnapshot{
		{ID: bag.ID, ProtoID: 1001, Qty: 3, Hits: 250, Enchants: json.RawMessage(`{"glow":1}`), Slot: "hand"},
		{ID: sword.ID, ProtoID: 1002, Qty: 500, Hits: 0, Enchants: json.RawMessage(`{}`), Slot: "pack"},
	}
	if len(got.Items) != len(wantItems) {
		t.Fatalf("items = %+v; want %+v", got.Items, wantItems)
	}
	for i, w := range wantItems {
		g := got.Items[i]
		if g.ID != w.ID || g.ProtoID != w.ProtoID || g.Qty != w.Qty || g.Hits != w.Hits || g.Slot != w.Slot {
			t.Fatalf("items[%d] = %+v; want %+v", i, g, w)
		}
		var gEnch, wEnch map[string]any
		if err := json.Unmarshal(g.Enchants, &gEnch); err != nil {
			t.Fatalf("items[%d] enchants invalid: %v", i, err)
		}
		if err := json.Unmarshal(w.Enchants, &wEnch); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(gEnch, wEnch) {
			t.Fatalf("items[%d] enchants = %s; want %s", i, g.Enchants, w.Enchants)
		}
	}
	if got.Pending == nil || got.Pending.CharacterID != charID ||
		got.Pending.EffectiveCost != 35 || got.Pending.DeathTimeSeconds != 2000 ||
		got.Pending.CorpseID != nil || !got.Pending.PortalUsed {
		t.Fatalf("pending = %+v", got.Pending)
	}

	// One coherent snapshot: a second read observes the
	// identical value.
	again, err := st.LoadPlayerBootstrapRecovery(ctx, charID)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if !reflect.DeepEqual(got, again) {
		t.Fatalf("snapshot drifted:\nfirst=%+v\nsecond=%+v", got, again)
	}

	// Missing character: missing-row error, zero value.
	if _, err := st.LoadPlayerBootstrapRecovery(ctx, charID+999999); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing char err = %v; want ErrNoRows", err)
	}

	// Soft-deleted character never resurrects into recovery.
	if _, err := SoftDeleteCharacter(ctx, pool, charID, got.ExpectedRevision); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if _, err := st.LoadPlayerBootstrapRecovery(ctx, charID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("deleted char err = %v; want ErrNoRows", err)
	}
}
