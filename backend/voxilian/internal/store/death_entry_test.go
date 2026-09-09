package store

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/dlukt/voxilian/internal/store/gen"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// M5-T5b1b death-entry transaction tests (spec §9.5.8a, §8.1/§8.3).
// All multi-row proofs run against real PostgreSQL 18. No new
// migration, query, or generated code is required: composition uses
// the existing CASUpdateCharacterSnapshot, CASUpdateItemSnapshot,
// UpsertItemLocation, InsertCorpse, InsertPendingDeath,
// UpsertItemPKProtection, and InsertKill primitives.

const (
	deathPosX int64 = 7000
	deathPosY int64 = 8000
	deathPosZ int64 = 9000

	deathTimeSeconds int64 = 1700000000

	deathCorpseLifetime = 10 * time.Minute
	deathPKDuration     = 10 * time.Minute
)

type deathFixture struct {
	pool         *pgxpool.Pool
	q            *gen.Queries
	st           *PGStore
	victimID     int64
	killerCharID int64
	killerMobID  int32
	itemA        int64 // itemA < itemB by construction
	itemB        int64
}

func newDeathFixture(t *testing.T) *deathFixture {
	t.Helper()
	pool, q := openQueries(t)
	st := newTestStore(t, pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO spell_protos (id,school,level,mana,exertion,cast_ms,min_hp,outlaw,harmful,reagents,params,version) VALUES (1,1,1,1,1,0,1,false,false,'{}','{}',1),(2,2,1,1,1,0,1,false,false,'{}','{}',1),(3,3,1,1,1,0,1,false,false,'{}','{}',1) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO skill_protos (id,division,level,exertion,params,version) VALUES (1,1,1,1,'{}',1),(2,2,1,1,'{}',1) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO item_protos (id,kind,slot,base,version) VALUES (910,0,NULL,'{}',1) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO mob_protos (id,key,level,difficulty,karma,atk,resists,spells,loot_tid,version) VALUES (7100,'death-mob',45,6,-40,'{}','{}','{}',NULL,1) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	acct, err := q.CreateAccount(ctx, gen.CreateAccountParams{KeycloakSub: "sub-death-entry"})
	if err != nil {
		t.Fatal(err)
	}
	victim, err := createCharacter(ctx, q, validCharParams(acct.ID, 0, "DeathVictim"))
	if err != nil {
		t.Fatal(err)
	}
	killer, err := createCharacter(ctx, q, validCharParams(acct.ID, 1, "DeathKiller"))
	if err != nil {
		t.Fatal(err)
	}
	mkItem := func(slot string) int64 {
		t.Helper()
		root, _, err := createItemWithLocation(ctx, pool,
			gen.InsertItemInstanceParams{Proto: 910, Qty: 1, Hits: 100, Enchants: []byte("{}")},
			NewItemLocation{Kind: 0, CharacterID: int8(victim.ID), Slot: pgText(slot)},
		)
		if err != nil {
			t.Fatal(err)
		}
		return root.ID
	}
	a, b := mkItem("hand"), mkItem("pack")
	if a > b {
		a, b = b, a
	}
	return &deathFixture{pool: pool, q: q, st: st, victimID: victim.ID,
		killerCharID: killer.ID, killerMobID: 7100, itemA: a, itemB: b}
}

// deathCharSnapshot is a complete already-resolved post-death
// character aggregate. Its durable position deliberately differs from
// the death position: T5c later resolves newbie-home vs Underworld
// placement, and Store must not force them equal.
func (f *deathFixture) deathCharSnapshot(rev int64) CharacterSnapshot {
	return CharacterSnapshot{
		ID: f.victimID, ExpectedRevision: rev, Karma: 11,
		PosX: 111, PosY: 222, PosZ: 333,
		Vitals:      json.RawMessage(`{"hp":1,"threshold":80}`),
		Advancement: json.RawMessage(`{"pts":0}`),
		Flags:       3,
		Spells:      []CharacterSpellSnapshot{{SpellID: 1, Ability: 10}, {SpellID: 2, Ability: 20}},
		Skills:      []CharacterSkillSnapshot{{SkillID: 1, Ability: 30}},
	}
}

// deathGroundItem is a complete resulting item snapshot relocated to
// the ground at the death position.
func (f *deathFixture) deathGroundItem(id, rev int64) ItemSnapshot {
	x, y, z := deathPosX, deathPosY, deathPosZ
	return ItemSnapshot{ID: id, ExpectedRevision: rev,
		Qty: 5, Hits: 77, Enchants: json.RawMessage(`{"e":1}`),
		Location: ItemLocationSnapshot{Kind: 1, PosX: &x, PosY: &y, PosZ: &z}}
}

func (f *deathFixture) staleCount(agg string) float64 {
	return testutil.ToFloat64(f.st.stale.WithLabelValues(agg))
}

func deathCount(t *testing.T, f *deathFixture, query string, args ...any) int {
	t.Helper()
	return countFor(t, f.pool, query, args...)
}

// TestCommitDeathEntryHappyPath proves one normal death with TWO item
// mutations supplied in reverse ItemID order: deterministic ascending
// revisions, ground locations, corpse/pending/kill/protection state,
// post-death vs death position split, and zero ledger rows.
func TestCommitDeathEntryHappyPath(t *testing.T) {
	f := newDeathFixture(t)
	ctx := context.Background()

	req := DeathEntryRequest{
		Character:          f.deathCharSnapshot(0),
		DeathPosX:          deathPosX,
		DeathPosY:          deathPosY,
		DeathPosZ:          deathPosZ,
		EffectiveDeathCost: 100,
		DeathTimeSeconds:   deathTimeSeconds,
		CorpseLifetime:     deathCorpseLifetime,
		Items: []DeathEntryItem{
			{Snapshot: f.deathGroundItem(f.itemB, 0), PKProtectionDuration: 0},
			{Snapshot: f.deathGroundItem(f.itemA, 0), PKProtectionDuration: deathPKDuration},
		},
		Killer: &DeathEntryKiller{Kind: DeathEntryKillerCharacter, CharacterID: f.killerCharID},
	}
	res, err := f.st.CommitDeathEntry(ctx, req)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if res.CharacterRevision != 1 || res.CorpseID <= 0 {
		t.Fatalf("result = %+v, want char rev 1 + corpse", res)
	}
	if len(res.ItemRevisions) != 2 ||
		res.ItemRevisions[0] != (DeathEntryItemRevision{ItemID: f.itemA, Revision: 1}) ||
		res.ItemRevisions[1] != (DeathEntryItemRevision{ItemID: f.itemB, Revision: 1}) {
		t.Fatalf("item revisions = %+v, want ascending A/B at rev 1", res.ItemRevisions)
	}

	got := readRoot(t, f.q, f.victimID)
	if got.Revision != 1 || got.Karma != 11 || got.PosX != 111 || got.PosY != 222 || got.PosZ != 333 || got.Flags != 3 {
		t.Fatalf("character root = %+v, want post-death state rev 1", got)
	}
	if string(got.Vitals) != `{"hp": 1, "threshold": 80}` && string(got.Vitals) != `{"hp":1,"threshold":80}` {
		t.Fatalf("vitals = %s", got.Vitals)
	}
	spells, err := f.q.ListCharacterSpells(ctx, f.victimID)
	if err != nil || len(spells) != 2 || spells[0].SpellID != 1 || spells[1].SpellID != 2 {
		t.Fatalf("spells = %+v, %v", spells, err)
	}
	skills, err := f.q.ListCharacterSkills(ctx, f.victimID)
	if err != nil || len(skills) != 1 || skills[0].SkillID != 1 || skills[0].Ability != 30 {
		t.Fatalf("skills = %+v, %v", skills, err)
	}

	for _, id := range []int64{f.itemA, f.itemB} {
		it := readItem(t, f.q, id)
		if it.Revision != 1 || it.Qty != 5 || it.Hits != 77 {
			t.Fatalf("item %d = %+v, want rev 1 qty 5 hits 77", id, it)
		}
		loc := readLoc(t, f.q, id)
		if loc.Kind != 1 || !loc.PosX.Valid || loc.PosX.Int64 != deathPosX ||
			!loc.PosY.Valid || loc.PosY.Int64 != deathPosY ||
			!loc.PosZ.Valid || loc.PosZ.Int64 != deathPosZ {
			t.Fatalf("item %d location = %+v, want ground at death pos", id, loc)
		}
		if loc.CorpseID.Valid || loc.CharacterID.Valid || loc.ContainerItemID.Valid ||
			loc.VaultRegion.Valid || loc.Slot.Valid {
			t.Fatalf("item %d location carries non-ground refs: %+v", id, loc)
		}
	}

	if n := deathCount(t, f, `SELECT COUNT(*) FROM corpses WHERE character_id = $1`, f.victimID); n != 1 {
		t.Fatalf("corpses = %d, want exactly 1", n)
	}
	corpse, err := f.q.GetCorpseByID(ctx, res.CorpseID)
	if err != nil {
		t.Fatalf("get corpse: %v", err)
	}
	deathInstant := time.Unix(deathTimeSeconds, 0).UTC()
	if corpse.CharacterID != f.victimID || corpse.PosX != deathPosX ||
		corpse.PosY != deathPosY || corpse.PosZ != deathPosZ {
		t.Fatalf("corpse = %+v, want victim owner at death pos", corpse)
	}
	if !corpse.ExpiresAt.Time.Equal(deathInstant.Add(deathCorpseLifetime)) {
		t.Fatalf("corpse expiry = %v, want deathTime+lifetime", corpse.ExpiresAt.Time)
	}

	pending, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID)
	if err != nil {
		t.Fatalf("get pending: %v", err)
	}
	if pending.EffectiveCost != 100 || pending.DeathTimeSeconds != deathTimeSeconds ||
		!pending.CorpseID.Valid || pending.CorpseID.Int64 != res.CorpseID || pending.PortalUsed {
		t.Fatalf("pending = %+v", pending)
	}

	prot, err := f.q.GetItemPKProtection(ctx, f.itemA)
	if err != nil {
		t.Fatalf("get protection: %v", err)
	}
	if prot.VictimCharacterID != f.victimID ||
		!prot.ExpiresAt.Time.Equal(deathInstant.Add(deathPKDuration)) {
		t.Fatalf("protection = %+v", prot)
	}
	if _, err := f.q.GetItemPKProtection(ctx, f.itemB); !isNoRows(err) {
		t.Fatalf("unprotected item protection err = %v, want NoRows", err)
	}

	var killerKind, killerChar, victimChar, kx, ky, kz int64
	var killCount int
	row := f.pool.QueryRow(ctx, `SELECT COUNT(*), MIN(killer_kind), MIN(killer_character_id), MIN(victim_character_id), MIN(pos_x), MIN(pos_y), MIN(pos_z) FROM kills WHERE victim_character_id = $1`, f.victimID)
	if err := row.Scan(&killCount, &killerKind, &killerChar, &victimChar, &kx, &ky, &kz); err != nil {
		t.Fatal(err)
	}
	if killCount != 1 || killerKind != 0 || killerChar != f.killerCharID ||
		victimChar != f.victimID || kx != deathPosX || ky != deathPosY || kz != deathPosZ {
		t.Fatalf("kills = count %d kind %d killer %d victim %d pos %d/%d/%d",
			killCount, killerKind, killerChar, victimChar, kx, ky, kz)
	}
	if n := deathCount(t, f, `SELECT COUNT(*) FROM ledger`); n != 0 {
		t.Fatalf("ledger rows = %d, want 0", n)
	}
}

// TestCommitDeathEntryTokenCheap proves the Token-death special
// relocation contract: a Cheap death (cost 0) with one
// caller-resolved item mutation and zero PK protection persists the
// ground relocation, still creates a corpse, writes the auditable
// kill row, and persists a caller-supplied restored rest threshold
// unchanged. Store knows no Token class or proto.
func TestCommitDeathEntryTokenCheap(t *testing.T) {
	f := newDeathFixture(t)
	ctx := context.Background()

	char := f.deathCharSnapshot(0)
	char.Vitals = json.RawMessage(`{"hp":1,"threshold":90}`)
	req := DeathEntryRequest{
		Character:          char,
		DeathPosX:          deathPosX,
		DeathPosY:          deathPosY,
		DeathPosZ:          deathPosZ,
		EffectiveDeathCost: 0,
		DeathTimeSeconds:   deathTimeSeconds,
		CorpseLifetime:     deathCorpseLifetime,
		Items:              []DeathEntryItem{{Snapshot: f.deathGroundItem(f.itemA, 0)}},
		Killer:             &DeathEntryKiller{Kind: DeathEntryKillerMob, MobID: f.killerMobID},
	}
	res, err := f.st.CommitDeathEntry(ctx, req)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if res.CharacterRevision != 1 || res.CorpseID <= 0 ||
		len(res.ItemRevisions) != 1 || res.ItemRevisions[0].ItemID != f.itemA {
		t.Fatalf("result = %+v", res)
	}
	var vitals map[string]int64
	got := readRoot(t, f.q, f.victimID)
	if err := json.Unmarshal(got.Vitals, &vitals); err != nil || vitals["threshold"] != 90 {
		t.Fatalf("vitals = %s, %v; want caller threshold 90 unchanged", got.Vitals, err)
	}
	loc := readLoc(t, f.q, f.itemA)
	if loc.Kind != 1 || !loc.PosX.Valid || loc.PosX.Int64 != deathPosX || loc.CorpseID.Valid {
		t.Fatalf("token item location = %+v, want ground at death pos", loc)
	}
	if _, err := f.q.GetItemPKProtection(ctx, f.itemA); !isNoRows(err) {
		t.Fatalf("token protection err = %v, want NoRows", err)
	}
	pending, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID)
	if err != nil || pending.EffectiveCost != 0 || !pending.CorpseID.Valid {
		t.Fatalf("pending = %+v, %v; want cost 0 with corpse", pending, err)
	}
	if n := deathCount(t, f, `SELECT COUNT(*) FROM corpses WHERE character_id = $1`, f.victimID); n != 1 {
		t.Fatalf("corpses = %d, want 1", n)
	}
	var n, kind int
	var mob int32
	if err := f.pool.QueryRow(ctx, `SELECT COUNT(*), MIN(killer_kind), MIN(killer_mob_id) FROM kills WHERE victim_character_id = $1`, f.victimID).Scan(&n, &kind, &mob); err != nil || n != 1 || kind != 1 || mob != f.killerMobID {
		t.Fatalf("kills = %d/%d/%d, %v; want one mob kill", n, kind, mob, err)
	}
	if n := deathCount(t, f, `SELECT COUNT(*) FROM ledger`); n != 0 {
		t.Fatalf("ledger rows = %d, want 0", n)
	}
}

// TestCommitDeathEntryEnvironmentalKill proves a killer-less death
// still commits character/items/corpse/pending with no kills row and
// no ledger row.
func TestCommitDeathEntryEnvironmentalKill(t *testing.T) {
	f := newDeathFixture(t)
	ctx := context.Background()
	req := DeathEntryRequest{
		Character:          f.deathCharSnapshot(0),
		DeathPosX:          deathPosX,
		DeathPosY:          deathPosY,
		DeathPosZ:          deathPosZ,
		EffectiveDeathCost: 50,
		DeathTimeSeconds:   deathTimeSeconds,
		CorpseLifetime:     deathCorpseLifetime,
		Items:              []DeathEntryItem{{Snapshot: f.deathGroundItem(f.itemA, 0)}},
		Killer:             nil,
	}
	res, err := f.st.CommitDeathEntry(ctx, req)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if res.CharacterRevision != 1 || res.CorpseID <= 0 {
		t.Fatalf("result = %+v", res)
	}
	if _, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID); err != nil {
		t.Fatalf("pending: %v", err)
	}
	if n := deathCount(t, f, `SELECT COUNT(*) FROM kills WHERE victim_character_id = $1`, f.victimID); n != 0 {
		t.Fatalf("kills = %d, want 0", n)
	}
	if n := deathCount(t, f, `SELECT COUNT(*) FROM ledger`); n != 0 {
		t.Fatalf("ledger rows = %d, want 0", n)
	}
}

// TestCommitDeathEntryCharacterStale proves a stale character root
// rolls back everything, maps to ErrStaleRevision, and counts the
// character metric exactly once.
func TestCommitDeathEntryCharacterStale(t *testing.T) {
	f := newDeathFixture(t)
	ctx := context.Background()
	beforeChar := readRoot(t, f.q, f.victimID)
	beforeItem := readItem(t, f.q, f.itemA)
	beforeLoc := readLoc(t, f.q, f.itemA)

	req := DeathEntryRequest{
		Character:          f.deathCharSnapshot(99),
		DeathPosX:          deathPosX,
		DeathPosY:          deathPosY,
		DeathPosZ:          deathPosZ,
		EffectiveDeathCost: 100,
		DeathTimeSeconds:   deathTimeSeconds,
		CorpseLifetime:     deathCorpseLifetime,
		Items:              []DeathEntryItem{{Snapshot: f.deathGroundItem(f.itemA, 0), PKProtectionDuration: deathPKDuration}},
		Killer:             &DeathEntryKiller{Kind: DeathEntryKillerCharacter, CharacterID: f.killerCharID},
	}
	res, err := f.st.CommitDeathEntry(ctx, req)
	if !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("err = %v, want ErrStaleRevision", err)
	}
	if !isZeroDeathResult(res) {
		t.Fatalf("result = %+v, want zero", res)
	}
	if got := f.staleCount("character"); got != 1 {
		t.Fatalf("character stale = %v, want 1", got)
	}
	if got := f.staleCount("item"); got != 0 {
		t.Fatalf("item stale = %v, want 0", got)
	}
	afterChar := readRoot(t, f.q, f.victimID)
	if !sameCharacter(afterChar, beforeChar) {
		t.Fatalf("character moved: %+v vs %+v", afterChar, beforeChar)
	}
	if afterItem := readItem(t, f.q, f.itemA); !sameItemInstance(afterItem, beforeItem) {
		t.Fatalf("item moved: %+v vs %+v", afterItem, beforeItem)
	}
	if afterLoc := readLoc(t, f.q, f.itemA); afterLoc != beforeLoc {
		t.Fatalf("location moved: %+v vs %+v", afterLoc, beforeLoc)
	}
	if n := deathCount(t, f, `SELECT COUNT(*) FROM corpses WHERE character_id = $1`, f.victimID); n != 0 {
		t.Fatalf("corpses = %d, want 0", n)
	}
	if _, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID); !isNoRows(err) {
		t.Fatalf("pending err = %v, want NoRows", err)
	}
	if _, err := f.q.GetItemPKProtection(ctx, f.itemA); !isNoRows(err) {
		t.Fatalf("protection err = %v, want NoRows", err)
	}
	if n := deathCount(t, f, `SELECT COUNT(*) FROM kills WHERE victim_character_id = $1`, f.victimID); n != 0 {
		t.Fatalf("kills = %d, want 0", n)
	}
	if n := deathCount(t, f, `SELECT COUNT(*) FROM ledger`); n != 0 {
		t.Fatalf("ledger rows = %d, want 0", n)
	}
}

// TestCommitDeathEntryItemStale proves multi-root atomicity: the
// character CAS and the lower-ItemID CAS tentatively succeed inside
// the transaction before the higher ItemID misses, and the rollback
// restores everything with exactly one item-stale count.
func TestCommitDeathEntryItemStale(t *testing.T) {
	f := newDeathFixture(t)
	ctx := context.Background()
	// Advance the higher item to rev 1 so ExpectedRevision 0 is stale.
	staleSnap := f.deathGroundItem(f.itemB, 0)
	staleSnap.Location.PosX = int64p(1)
	staleSnap.Location.PosY = int64p(2)
	staleSnap.Location.PosZ = int64p(3)
	if _, err := f.st.SaveItemSnapshot(ctx, staleSnap); err != nil {
		t.Fatalf("advance itemB: %v", err)
	}
	beforeChar := readRoot(t, f.q, f.victimID)
	beforeA := readItem(t, f.q, f.itemA)
	beforeALoc := readLoc(t, f.q, f.itemA)
	beforeB := readItem(t, f.q, f.itemB)

	req := DeathEntryRequest{
		Character:          f.deathCharSnapshot(0),
		DeathPosX:          deathPosX,
		DeathPosY:          deathPosY,
		DeathPosZ:          deathPosZ,
		EffectiveDeathCost: 100,
		DeathTimeSeconds:   deathTimeSeconds,
		CorpseLifetime:     deathCorpseLifetime,
		Items: []DeathEntryItem{
			{Snapshot: f.deathGroundItem(f.itemA, 0), PKProtectionDuration: deathPKDuration},
			{Snapshot: f.deathGroundItem(f.itemB, 0)},
		},
		Killer: &DeathEntryKiller{Kind: DeathEntryKillerCharacter, CharacterID: f.killerCharID},
	}
	res, err := f.st.CommitDeathEntry(ctx, req)
	if !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("err = %v, want ErrStaleRevision", err)
	}
	if !isZeroDeathResult(res) {
		t.Fatalf("result = %+v, want zero", res)
	}
	if got := f.staleCount("item"); got != 1 {
		t.Fatalf("item stale = %v, want 1", got)
	}
	if got := f.staleCount("character"); got != 0 {
		t.Fatalf("character stale = %v, want 0", got)
	}
	if after := readRoot(t, f.q, f.victimID); !sameCharacter(after, beforeChar) {
		t.Fatalf("character moved: %+v vs %+v", after, beforeChar)
	}
	if after := readItem(t, f.q, f.itemA); !sameItemInstance(after, beforeA) {
		t.Fatalf("first item moved: %+v vs %+v", after, beforeA)
	}
	if after := readLoc(t, f.q, f.itemA); after != beforeALoc {
		t.Fatalf("first location moved: %+v vs %+v", after, beforeALoc)
	}
	if after := readItem(t, f.q, f.itemB); !sameItemInstance(after, beforeB) {
		t.Fatalf("stale item moved: %+v vs %+v", after, beforeB)
	}
	if n := deathCount(t, f, `SELECT COUNT(*) FROM corpses WHERE character_id = $1`, f.victimID); n != 0 {
		t.Fatalf("corpses = %d, want 0", n)
	}
	if _, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID); !isNoRows(err) {
		t.Fatalf("pending err = %v, want NoRows", err)
	}
	if _, err := f.q.GetItemPKProtection(ctx, f.itemA); !isNoRows(err) {
		t.Fatalf("protection err = %v, want NoRows", err)
	}
	if n := deathCount(t, f, `SELECT COUNT(*) FROM kills WHERE victim_character_id = $1`, f.victimID); n != 0 {
		t.Fatalf("kills = %d, want 0", n)
	}
	if n := deathCount(t, f, `SELECT COUNT(*) FROM ledger`); n != 0 {
		t.Fatalf("ledger rows = %d, want 0", n)
	}
}

// TestCommitDeathEntryAlreadyPending proves the replay guard: with a
// live pending row and a CURRENT character revision, the transaction
// tentatively works then rolls back on pending_deaths_pkey, mapping
// to ErrDeathAlreadyPending with no stale count and no second corpse.
func TestCommitDeathEntryAlreadyPending(t *testing.T) {
	f := newDeathFixture(t)
	ctx := context.Background()
	seedCorpse, err := f.q.InsertCorpse(ctx, gen.InsertCorpseParams{
		CharacterID: f.victimID,
		ExpiresAt:   pgtype.Timestamptz{Time: time.Now().Add(time.Hour).UTC(), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	seed, err := f.q.InsertPendingDeath(ctx, gen.InsertPendingDeathParams{
		CharacterID:      f.victimID,
		EffectiveCost:    37,
		DeathTimeSeconds: 7,
		CorpseID:         int8(seedCorpse.ID),
		PortalUsed:       false,
	})
	if err != nil {
		t.Fatal(err)
	}
	beforeChar := readRoot(t, f.q, f.victimID)
	beforeA := readItem(t, f.q, f.itemA)

	req := DeathEntryRequest{
		Character:          f.deathCharSnapshot(0),
		DeathPosX:          deathPosX,
		DeathPosY:          deathPosY,
		DeathPosZ:          deathPosZ,
		EffectiveDeathCost: 100,
		DeathTimeSeconds:   deathTimeSeconds,
		CorpseLifetime:     deathCorpseLifetime,
		Items:              []DeathEntryItem{{Snapshot: f.deathGroundItem(f.itemA, 0), PKProtectionDuration: deathPKDuration}},
		Killer:             &DeathEntryKiller{Kind: DeathEntryKillerCharacter, CharacterID: f.killerCharID},
	}
	res, err := f.st.CommitDeathEntry(ctx, req)
	if !errors.Is(err, ErrDeathAlreadyPending) {
		t.Fatalf("err = %v, want ErrDeathAlreadyPending", err)
	}
	if errors.Is(err, ErrStaleRevision) {
		t.Fatalf("replay mapped to stale: %v", err)
	}
	if !isZeroDeathResult(res) {
		t.Fatalf("result = %+v, want zero", res)
	}
	after, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID)
	if err != nil || after != seed {
		t.Fatalf("pending = %+v, %v; want unchanged %+v", after, err, seed)
	}
	if afterChar := readRoot(t, f.q, f.victimID); !sameCharacter(afterChar, beforeChar) {
		t.Fatalf("character moved: %+v vs %+v", afterChar, beforeChar)
	}
	if afterItem := readItem(t, f.q, f.itemA); !sameItemInstance(afterItem, beforeA) {
		t.Fatalf("item moved: %+v vs %+v", afterItem, beforeA)
	}
	if n := deathCount(t, f, `SELECT COUNT(*) FROM corpses WHERE character_id = $1`, f.victimID); n != 1 {
		t.Fatalf("corpses = %d, want exactly the seeded 1", n)
	}
	if _, err := f.q.GetItemPKProtection(ctx, f.itemA); !isNoRows(err) {
		t.Fatalf("protection err = %v, want NoRows", err)
	}
	if n := deathCount(t, f, `SELECT COUNT(*) FROM kills WHERE victim_character_id = $1`, f.victimID); n != 0 {
		t.Fatalf("kills = %d, want 0", n)
	}
	if n := deathCount(t, f, `SELECT COUNT(*) FROM ledger`); n != 0 {
		t.Fatalf("ledger rows = %d, want 0", n)
	}
	if got := f.staleCount("character"); got != 0 {
		t.Fatalf("character stale = %v, want 0", got)
	}
	if got := f.staleCount("item"); got != 0 {
		t.Fatalf("item stale = %v, want 0", got)
	}
}

// TestCommitDeathEntryTxAbort proves PostgreSQL rollback is the
// recovery: the real production tx-composition helper runs to
// completion inside a transaction (character/item/corpse/pending/kill
// writes all occur), the transaction is then explicitly rolled back
// instead of committed, nothing durable survives, and a subsequent
// healthy call with the SAME expected revisions succeeds. (An
// explicit rollback after invoking the production seam is used
// because backend termination is impractical in this harness; no
// bespoke compensation logic exists — rollback does all recovery.)
func TestCommitDeathEntryTxAbort(t *testing.T) {
	f := newDeathFixture(t)
	ctx := context.Background()
	req := DeathEntryRequest{
		Character:          f.deathCharSnapshot(0),
		DeathPosX:          deathPosX,
		DeathPosY:          deathPosY,
		DeathPosZ:          deathPosZ,
		EffectiveDeathCost: 100,
		DeathTimeSeconds:   deathTimeSeconds,
		CorpseLifetime:     deathCorpseLifetime,
		Items:              []DeathEntryItem{{Snapshot: f.deathGroundItem(f.itemA, 0), PKProtectionDuration: deathPKDuration}},
		Killer:             &DeathEntryKiller{Kind: DeathEntryKillerCharacter, CharacterID: f.killerCharID},
	}
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	aborted, err := commitDeathEntryTx(ctx, tx, req)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("tx composition: %v", err)
	}
	if aborted.CharacterRevision != 1 || aborted.CorpseID <= 0 {
		_ = tx.Rollback(ctx)
		t.Fatalf("aborted result = %+v", aborted)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	if after := readRoot(t, f.q, f.victimID); after.Revision != 0 {
		t.Fatalf("character survived abort: %+v", after)
	}
	if after := readItem(t, f.q, f.itemA); after.Revision != 0 {
		t.Fatalf("item survived abort: %+v", after)
	}
	if n := deathCount(t, f, `SELECT COUNT(*) FROM corpses WHERE character_id = $1`, f.victimID); n != 0 {
		t.Fatalf("corpses = %d, want 0", n)
	}
	if _, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID); !isNoRows(err) {
		t.Fatalf("pending err = %v, want NoRows", err)
	}
	if _, err := f.q.GetItemPKProtection(ctx, f.itemA); !isNoRows(err) {
		t.Fatalf("protection err = %v, want NoRows", err)
	}
	if n := deathCount(t, f, `SELECT COUNT(*) FROM kills WHERE victim_character_id = $1`, f.victimID); n != 0 {
		t.Fatalf("kills = %d, want 0", n)
	}
	if n := deathCount(t, f, `SELECT COUNT(*) FROM ledger`); n != 0 {
		t.Fatalf("ledger rows = %d, want 0", n)
	}

	res, err := f.st.CommitDeathEntry(ctx, req)
	if err != nil {
		t.Fatalf("healthy retry: %v", err)
	}
	if res.CharacterRevision != 1 || res.CorpseID <= 0 {
		t.Fatalf("retry result = %+v", res)
	}
}

// TestCommitDeathEntryCommitAmbiguity proves the lost-ACK contract
// without an outbox: commit through the real private seam, discard
// the acknowledgement, retry with OLD revisions (stale, no dupes),
// then reconcile to authoritative revisions and retry while the
// pending row lives (already-pending, tentative increments rolled
// back). Final state is exactly the first commit.
func TestCommitDeathEntryCommitAmbiguity(t *testing.T) {
	f := newDeathFixture(t)
	ctx := context.Background()
	req := DeathEntryRequest{
		Character:          f.deathCharSnapshot(0),
		DeathPosX:          deathPosX,
		DeathPosY:          deathPosY,
		DeathPosZ:          deathPosZ,
		EffectiveDeathCost: 100,
		DeathTimeSeconds:   deathTimeSeconds,
		CorpseLifetime:     deathCorpseLifetime,
		Items:              []DeathEntryItem{{Snapshot: f.deathGroundItem(f.itemA, 0), PKProtectionDuration: deathPKDuration}},
		Killer:             &DeathEntryKiller{Kind: DeathEntryKillerCharacter, CharacterID: f.killerCharID},
	}
	// First commit through the production seam; the success
	// acknowledgement is then intentionally treated as lost.
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first, err := commitDeathEntryTx(ctx, tx, req)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("first composition: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	if first.CharacterRevision != 1 || first.CorpseID <= 0 {
		t.Fatalf("first = %+v", first)
	}

	// Retry with the OLD expected revisions: stale, no duplicates.
	retryRes, err := f.st.CommitDeathEntry(ctx, req)
	if !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("retry err = %v, want ErrStaleRevision", err)
	}
	if !isZeroDeathResult(retryRes) {
		t.Fatalf("retry result = %+v, want zero", retryRes)
	}
	assertSingleDeathState := func(stage string) {
		t.Helper()
		if n := deathCount(t, f, `SELECT COUNT(*) FROM corpses WHERE character_id = $1`, f.victimID); n != 1 {
			t.Fatalf("%s: corpses = %d, want 1", stage, n)
		}
		pending, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID)
		if err != nil || pending.EffectiveCost != 100 || !pending.CorpseID.Valid || pending.CorpseID.Int64 != first.CorpseID {
			t.Fatalf("%s: pending = %+v, %v", stage, pending, err)
		}
		if n := deathCount(t, f, `SELECT COUNT(*) FROM kills WHERE victim_character_id = $1`, f.victimID); n != 1 {
			t.Fatalf("%s: kills = %d, want 1", stage, n)
		}
		if got := readRoot(t, f.q, f.victimID); got.Revision != 1 {
			t.Fatalf("%s: character = %+v, want rev 1", stage, got)
		}
		if got := readItem(t, f.q, f.itemA); got.Revision != 1 {
			t.Fatalf("%s: item = %+v, want rev 1", stage, got)
		}
		loc := readLoc(t, f.q, f.itemA)
		if loc.Kind != 1 || !loc.PosX.Valid || loc.PosX.Int64 != deathPosX {
			t.Fatalf("%s: location = %+v", stage, loc)
		}
		if _, err := f.q.GetItemPKProtection(ctx, f.itemA); err != nil {
			t.Fatalf("%s: protection: %v", stage, err)
		}
		if n := deathCount(t, f, `SELECT COUNT(*) FROM ledger`); n != 0 {
			t.Fatalf("%s: ledger = %d, want 0", stage, n)
		}
	}
	assertSingleDeathState("after stale retry")

	// Reconciled retry at the authoritative revisions while the
	// pending row still exists: already-pending, rolled back.
	reconciled := req
	reconciled.Character = f.deathCharSnapshot(1)
	reconciled.Items = []DeathEntryItem{{Snapshot: f.deathGroundItem(f.itemA, 1), PKProtectionDuration: deathPKDuration}}
	reconciledRes, err := f.st.CommitDeathEntry(ctx, reconciled)
	if !errors.Is(err, ErrDeathAlreadyPending) {
		t.Fatalf("reconciled err = %v, want ErrDeathAlreadyPending", err)
	}
	if !isZeroDeathResult(reconciledRes) {
		t.Fatalf("reconciled result = %+v, want zero", reconciledRes)
	}
	assertSingleDeathState("after reconciled retry")
}

// TestCommitDeathEntryRejectsCorpseLocation pins the §9.5.5
// regression guard: a death-entry item aimed at corpse storage
// (Kind 2 with a CorpseID) is rejected before PG mutation so a
// future developer cannot regress drops back into the corpse.
func TestCommitDeathEntryRejectsCorpseLocation(t *testing.T) {
	f := newDeathFixture(t)
	ctx := context.Background()
	corpseID := int64(4242)
	snap := f.deathGroundItem(f.itemA, 0)
	snap.Location = ItemLocationSnapshot{Kind: 2, CorpseID: &corpseID}
	req := DeathEntryRequest{
		Character:          f.deathCharSnapshot(0),
		DeathPosX:          deathPosX,
		DeathPosY:          deathPosY,
		DeathPosZ:          deathPosZ,
		EffectiveDeathCost: 100,
		DeathTimeSeconds:   deathTimeSeconds,
		CorpseLifetime:     deathCorpseLifetime,
		Items:              []DeathEntryItem{{Snapshot: snap}},
	}
	if _, err := f.st.CommitDeathEntry(ctx, req); !errors.Is(err, ErrInvalidDeathEntry) {
		t.Fatalf("err = %v, want ErrInvalidDeathEntry", err)
	}
	if n := deathCount(t, f, `SELECT COUNT(*) FROM corpses WHERE character_id = $1`, f.victimID); n != 0 {
		t.Fatalf("corpses = %d, want 0", n)
	}
}

// TestCommitDeathEntryValidation rejects every malformed shape before
// meaningful PG mutation with the narrow stable error.
func TestCommitDeathEntryValidation(t *testing.T) {
	f := newDeathFixture(t)
	ctx := context.Background()
	base := func() DeathEntryRequest {
		return DeathEntryRequest{
			Character:          f.deathCharSnapshot(0),
			DeathPosX:          deathPosX,
			DeathPosY:          deathPosY,
			DeathPosZ:          deathPosZ,
			EffectiveDeathCost: 100,
			DeathTimeSeconds:   deathTimeSeconds,
			CorpseLifetime:     deathCorpseLifetime,
			Items:              []DeathEntryItem{{Snapshot: f.deathGroundItem(f.itemA, 0)}},
			Killer:             &DeathEntryKiller{Kind: DeathEntryKillerCharacter, CharacterID: f.killerCharID},
		}
	}
	groundAt := func(x, y, z int64) ItemLocationSnapshot {
		return ItemLocationSnapshot{Kind: 1, PosX: &x, PosY: &y, PosZ: &z}
	}
	cases := map[string]func(*DeathEntryRequest){
		"character id":       func(r *DeathEntryRequest) { r.Character.ID = 0 },
		"character revision": func(r *DeathEntryRequest) { r.Character.ExpectedRevision = -1 },
		"cost negative":      func(r *DeathEntryRequest) { r.EffectiveDeathCost = -1 },
		"cost over 100":      func(r *DeathEntryRequest) { r.EffectiveDeathCost = 101 },
		"death time":         func(r *DeathEntryRequest) { r.DeathTimeSeconds = -1 },
		"lifetime zero":      func(r *DeathEntryRequest) { r.CorpseLifetime = 0 },
		"lifetime negative":  func(r *DeathEntryRequest) { r.CorpseLifetime = -time.Second },
		"item id":            func(r *DeathEntryRequest) { r.Items[0].Snapshot.ID = 0 },
		"item revision":      func(r *DeathEntryRequest) { r.Items[0].Snapshot.ExpectedRevision = -1 },
		"duplicate items":    func(r *DeathEntryRequest) { r.Items = append(r.Items, r.Items[0]) },
		"negative PK":        func(r *DeathEntryRequest) { r.Items[0].PKProtectionDuration = -time.Second },
		"non-ground kind": func(r *DeathEntryRequest) {
			r.Items[0].Snapshot.Location = groundAt(deathPosX, deathPosY, deathPosZ)
			r.Items[0].Snapshot.Location.Kind = 0
		},
		"wrong pos":      func(r *DeathEntryRequest) { r.Items[0].Snapshot.Location = groundAt(1, 2, 3) },
		"nil pos":        func(r *DeathEntryRequest) { r.Items[0].Snapshot.Location.PosZ = nil },
		"character ref":  func(r *DeathEntryRequest) { id := f.victimID; r.Items[0].Snapshot.Location.CharacterID = &id },
		"corpse ref":     func(r *DeathEntryRequest) { id := int64(7); r.Items[0].Snapshot.Location.CorpseID = &id },
		"container ref":  func(r *DeathEntryRequest) { id := f.itemB; r.Items[0].Snapshot.Location.ContainerItemID = &id },
		"vault ref":      func(r *DeathEntryRequest) { s := "barloque"; r.Items[0].Snapshot.Location.VaultRegion = &s },
		"slot ref":       func(r *DeathEntryRequest) { s := "hand"; r.Items[0].Snapshot.Location.Slot = &s },
		"unknown killer": func(r *DeathEntryRequest) { r.Killer = &DeathEntryKiller{Kind: 7, CharacterID: 1} },
		"char killer missing": func(r *DeathEntryRequest) {
			r.Killer = &DeathEntryKiller{Kind: DeathEntryKillerCharacter}
		},
		"char killer mob set": func(r *DeathEntryRequest) {
			r.Killer = &DeathEntryKiller{Kind: DeathEntryKillerCharacter, CharacterID: f.killerCharID, MobID: 1}
		},
		"mob killer missing": func(r *DeathEntryRequest) { r.Killer = &DeathEntryKiller{Kind: DeathEntryKillerMob} },
		"mob killer char set": func(r *DeathEntryRequest) {
			r.Killer = &DeathEntryKiller{Kind: DeathEntryKillerMob, CharacterID: 1, MobID: f.killerMobID}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			req := base()
			mutate(&req)
			if _, err := f.st.CommitDeathEntry(ctx, req); !errors.Is(err, ErrInvalidDeathEntry) {
				t.Fatalf("err = %v, want ErrInvalidDeathEntry", err)
			}
		})
	}
	if n := deathCount(t, f, `SELECT COUNT(*) FROM corpses WHERE character_id = $1`, f.victimID); n != 0 {
		t.Fatalf("corpses = %d, want 0 after all rejections", n)
	}
	if got := readRoot(t, f.q, f.victimID); got.Revision != 0 {
		t.Fatalf("character moved by rejections: %+v", got)
	}
}

// TestCommitDeathEntryCallerImmutability proves the caller's item
// slice order and contents survive the call: the ascending-ID CAS
// order applies to a sorted copy, never in place.
func TestCommitDeathEntryCallerImmutability(t *testing.T) {
	f := newDeathFixture(t)
	ctx := context.Background()
	req := DeathEntryRequest{
		Character:          f.deathCharSnapshot(0),
		DeathPosX:          deathPosX,
		DeathPosY:          deathPosY,
		DeathPosZ:          deathPosZ,
		EffectiveDeathCost: 100,
		DeathTimeSeconds:   deathTimeSeconds,
		CorpseLifetime:     deathCorpseLifetime,
		Items: []DeathEntryItem{
			{Snapshot: f.deathGroundItem(f.itemB, 0), PKProtectionDuration: 0},
			{Snapshot: f.deathGroundItem(f.itemA, 0), PKProtectionDuration: deathPKDuration},
		},
		Killer: &DeathEntryKiller{Kind: DeathEntryKillerCharacter, CharacterID: f.killerCharID},
	}
	wantIDs := []int64{req.Items[0].Snapshot.ID, req.Items[1].Snapshot.ID}
	wantPK := []time.Duration{req.Items[0].PKProtectionDuration, req.Items[1].PKProtectionDuration}
	if _, err := f.st.CommitDeathEntry(ctx, req); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if len(req.Items) != 2 || req.Items[0].Snapshot.ID != wantIDs[0] || req.Items[1].Snapshot.ID != wantIDs[1] {
		t.Fatalf("caller slice reordered: %+v", req.Items)
	}
	if req.Items[0].PKProtectionDuration != wantPK[0] || req.Items[1].PKProtectionDuration != wantPK[1] {
		t.Fatalf("caller contents mutated: %+v", req.Items)
	}
}

func int64p(v int64) *int64 { return &v }

func isZeroDeathResult(res DeathEntryResult) bool {
	return res.CharacterRevision == 0 && res.CorpseID == 0 && len(res.ItemRevisions) == 0
}

// sameCharacter reports whether two character rows carry identical
// gameplay durability state. Identity/profile/timestamp columns are
// excluded: the death transaction never touches them, and timestamps
// re-read with driverlocale noise.
func sameCharacter(a, b gen.Character) bool {
	return a.ID == b.ID && a.Revision == b.Revision && a.Karma == b.Karma &&
		a.PosX == b.PosX && a.PosY == b.PosY && a.PosZ == b.PosZ &&
		string(a.Vitals) == string(b.Vitals) &&
		string(a.Advancement) == string(b.Advancement) && a.Flags == b.Flags
}

func sameItemInstance(a, b gen.ItemInstance) bool {
	return a.ID == b.ID && a.Revision == b.Revision && a.Qty == b.Qty &&
		a.Hits == b.Hits && string(a.Enchants) == string(b.Enchants)
}

// TestCommitDeathEntryHostileDeathTime proves a hostile death-time
// scalar can never silently become a different durable timestamp: the
// maximum int64 death time plus an ordinary corpse lifetime overflows
// the Unix-microsecond domain, so the PUBLIC operation rejects it
// with ErrInvalidDeathEntry before any PG mutation, with no panic
// and no stale-metric increment.
func TestCommitDeathEntryHostileDeathTime(t *testing.T) {
	f := newDeathFixture(t)
	ctx := context.Background()
	beforeLoc := readLoc(t, f.q, f.itemA)

	req := DeathEntryRequest{
		Character:          f.deathCharSnapshot(0),
		DeathPosX:          deathPosX,
		DeathPosY:          deathPosY,
		DeathPosZ:          deathPosZ,
		EffectiveDeathCost: 100,
		DeathTimeSeconds:   math.MaxInt64,
		CorpseLifetime:     deathCorpseLifetime,
		Items:              []DeathEntryItem{{Snapshot: f.deathGroundItem(f.itemA, 0), PKProtectionDuration: deathPKDuration}},
		Killer:             &DeathEntryKiller{Kind: DeathEntryKillerCharacter, CharacterID: f.killerCharID},
	}
	res, err := f.st.CommitDeathEntry(ctx, req)
	if !errors.Is(err, ErrInvalidDeathEntry) {
		t.Fatalf("err = %v, want ErrInvalidDeathEntry", err)
	}
	if !isZeroDeathResult(res) {
		t.Fatalf("result = %+v, want zero", res)
	}
	if got := readRoot(t, f.q, f.victimID); got.Revision != 0 {
		t.Fatalf("character moved: %+v", got)
	}
	if got := readItem(t, f.q, f.itemA); got.Revision != 0 {
		t.Fatalf("item moved: %+v", got)
	}
	if afterLoc := readLoc(t, f.q, f.itemA); afterLoc != beforeLoc {
		t.Fatalf("location moved: %+v vs %+v", afterLoc, beforeLoc)
	}
	if n := deathCount(t, f, `SELECT COUNT(*) FROM corpses WHERE character_id = $1`, f.victimID); n != 0 {
		t.Fatalf("corpses = %d, want 0", n)
	}
	if _, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID); !isNoRows(err) {
		t.Fatalf("pending err = %v, want NoRows", err)
	}
	if _, err := f.q.GetItemPKProtection(ctx, f.itemA); !isNoRows(err) {
		t.Fatalf("protection err = %v, want NoRows", err)
	}
	if n := deathCount(t, f, `SELECT COUNT(*) FROM kills WHERE victim_character_id = $1`, f.victimID); n != 0 {
		t.Fatalf("kills = %d, want 0", n)
	}
	if n := deathCount(t, f, `SELECT COUNT(*) FROM ledger`); n != 0 {
		t.Fatalf("ledger rows = %d, want 0", n)
	}
	if got := f.staleCount("character"); got != 0 {
		t.Fatalf("character stale = %v, want 0", got)
	}
	if got := f.staleCount("item"); got != 0 {
		t.Fatalf("item stale = %v, want 0", got)
	}
}

// TestCommitDeathEntryPKExpiryOverflow proves validation checks EVERY
// persisted derived timestamp, not only the corpse expiry: the death
// time sits at the safe upper boundary for the corpse lifetime, but
// the larger PK-protection duration overflows the Unix-microsecond
// domain, so the request is rejected before PG mutation.
func TestCommitDeathEntryPKExpiryOverflow(t *testing.T) {
	f := newDeathFixture(t)
	ctx := context.Background()
	corpseMicros := int64(deathCorpseLifetime) / 1000
	baseSec := (math.MaxInt64 - corpseMicros) / unixMicrosPerSecond
	// Sanity: the corpse expiry itself is still representable.
	if _, err := deathExpiryTime(baseSec, deathCorpseLifetime); err != nil {
		t.Fatalf("corpse boundary should fit: %v", err)
	}
	// Two extra seconds push the protection expiry past MaxInt64.
	overflowPK := deathCorpseLifetime + 2*time.Second
	if _, err := deathExpiryTime(baseSec, overflowPK); !errors.Is(err, ErrInvalidDeathEntry) {
		t.Fatalf("helper err = %v, want ErrInvalidDeathEntry", err)
	}

	req := DeathEntryRequest{
		Character:          f.deathCharSnapshot(0),
		DeathPosX:          deathPosX,
		DeathPosY:          deathPosY,
		DeathPosZ:          deathPosZ,
		EffectiveDeathCost: 100,
		DeathTimeSeconds:   baseSec,
		CorpseLifetime:     deathCorpseLifetime,
		Items:              []DeathEntryItem{{Snapshot: f.deathGroundItem(f.itemA, 0), PKProtectionDuration: overflowPK}},
		Killer:             &DeathEntryKiller{Kind: DeathEntryKillerCharacter, CharacterID: f.killerCharID},
	}
	res, err := f.st.CommitDeathEntry(ctx, req)
	if !errors.Is(err, ErrInvalidDeathEntry) {
		t.Fatalf("err = %v, want ErrInvalidDeathEntry", err)
	}
	if !isZeroDeathResult(res) {
		t.Fatalf("result = %+v, want zero", res)
	}
	if got := readRoot(t, f.q, f.victimID); got.Revision != 0 {
		t.Fatalf("character moved: %+v", got)
	}
	if got := readItem(t, f.q, f.itemA); got.Revision != 0 {
		t.Fatalf("item moved: %+v", got)
	}
	if n := deathCount(t, f, `SELECT COUNT(*) FROM corpses WHERE character_id = $1`, f.victimID); n != 0 {
		t.Fatalf("corpses = %d, want 0", n)
	}
	if _, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID); !isNoRows(err) {
		t.Fatalf("pending err = %v, want NoRows", err)
	}
	if _, err := f.q.GetItemPKProtection(ctx, f.itemA); !isNoRows(err) {
		t.Fatalf("protection err = %v, want NoRows", err)
	}
	if n := deathCount(t, f, `SELECT COUNT(*) FROM kills WHERE victim_character_id = $1`, f.victimID); n != 0 {
		t.Fatalf("kills = %d, want 0", n)
	}
	if n := deathCount(t, f, `SELECT COUNT(*) FROM ledger`); n != 0 {
		t.Fatalf("ledger rows = %d, want 0", n)
	}
	if got := f.staleCount("character"); got != 0 {
		t.Fatalf("character stale = %v, want 0", got)
	}
	if got := f.staleCount("item"); got != 0 {
		t.Fatalf("item stale = %v, want 0", got)
	}
}

// TestDeathExpiryTimeBoundary unit-tests the checked helper's exact
// arithmetic edge: a final Unix-microsecond scalar of exactly
// MaxInt64 is accepted with an exact round-trip, one microsecond
// more is rejected, ordinary production values match time.Add
// exactly, sub-microsecond remainders truncate as the codec encodes,
// and negative inputs are rejected.
func TestDeathExpiryTimeBoundary(t *testing.T) {
	// Exact top of the int64 microsecond domain.
	base := int64(math.MaxInt64) / unixMicrosPerSecond
	dur := time.Duration(int64(math.MaxInt64)%unixMicrosPerSecond) * time.Microsecond
	got, err := deathExpiryTime(base, dur)
	if err != nil {
		t.Fatalf("max scalar: %v", err)
	}
	if micros := got.UnixMicro(); micros != math.MaxInt64 {
		t.Fatalf("round-trip = %d, want MaxInt64", micros)
	}
	if _, err := deathExpiryTime(base, dur+time.Microsecond); !errors.Is(err, ErrInvalidDeathEntry) {
		t.Fatalf("max+1 err = %v, want ErrInvalidDeathEntry", err)
	}

	// Ordinary production values are unchanged vs time.Add.
	ordinary, err := deathExpiryTime(deathTimeSeconds, deathCorpseLifetime)
	if err != nil {
		t.Fatalf("ordinary: %v", err)
	}
	want := time.Unix(deathTimeSeconds, 0).UTC().Add(deathCorpseLifetime)
	if !ordinary.Equal(want) {
		t.Fatalf("ordinary = %v, want %v", ordinary, want)
	}

	// Sub-microsecond remainder truncates like the codec.
	trunc, err := deathExpiryTime(100, 1500*time.Nanosecond)
	if err != nil {
		t.Fatalf("trunc: %v", err)
	}
	if micros := trunc.UnixMicro(); micros != 100*unixMicrosPerSecond+1 {
		t.Fatalf("trunc = %d, want 100000001", micros)
	}

	if _, err := deathExpiryTime(-1, time.Second); !errors.Is(err, ErrInvalidDeathEntry) {
		t.Fatalf("negative base err = %v, want ErrInvalidDeathEntry", err)
	}
	if _, err := deathExpiryTime(1, -time.Second); !errors.Is(err, ErrInvalidDeathEntry) {
		t.Fatalf("negative duration err = %v, want ErrInvalidDeathEntry", err)
	}
}
