package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// M5-T5c2a materialized death recovery tests (spec §9.5.1b). All
// proofs run against real PostgreSQL 18. No new migration, query,
// or generated code: composition uses the existing
// GetCharacterByID, ListCharacterSpells, ListCharacterSkills,
// GetPendingDeathByCharacter, GetItemInstanceByID,
// GetItemLocationByItemID, and GetItemPKProtection primitives,
// each recovery inside ONE repeatable-read, read-only transaction.

const (
	recoveryDeathCost    int16 = 40
	recoveryProposedCost int16 = 10
)

// recoveryCharSnapshot is a complete already-resolved post-death
// character aggregate with >=2 spells and >=2 skills.
func recoveryCharSnapshot(f *deathFixture, rev int64) CharacterSnapshot {
	return CharacterSnapshot{
		ID: f.victimID, ExpectedRevision: rev, Karma: 11,
		PosX: 111, PosY: 222, PosZ: 333,
		Vitals:      json.RawMessage(`{"hp":1,"threshold":80}`),
		Advancement: json.RawMessage(`{"pts":0}`),
		Flags:       3,
		Spells: []CharacterSpellSnapshot{
			{SpellID: 1, Ability: 10},
			{SpellID: 2, Ability: 20},
		},
		Skills: []CharacterSkillSnapshot{
			{SkillID: 1, Ability: 30},
			{SkillID: 2, Ability: 40},
		},
	}
}

// commitRecoveryDeath runs one death entry with protection on itemA
// only, returning the generated corpse ID.
func commitRecoveryDeath(t *testing.T, f *deathFixture, snap CharacterSnapshot) int64 {
	t.Helper()
	res, err := f.st.CommitDeathEntry(context.Background(), DeathEntryRequest{
		Character:          snap,
		DeathPosX:          deathPosX,
		DeathPosY:          deathPosY,
		DeathPosZ:          deathPosZ,
		EffectiveDeathCost: recoveryDeathCost,
		DeathTimeSeconds:   deathTimeSeconds,
		CorpseLifetime:     deathCorpseLifetime,
		Items: []DeathEntryItem{
			{Snapshot: f.deathGroundItem(f.itemB, 0), PKProtectionDuration: 0},
			{Snapshot: f.deathGroundItem(f.itemA, 0), PKProtectionDuration: deathPKDuration},
		},
		Killer: &DeathEntryKiller{Kind: DeathEntryKillerCharacter, CharacterID: f.killerCharID},
	})
	if err != nil {
		t.Fatalf("commit death entry: %v", err)
	}
	return res.CorpseID
}

func sameJSON(t *testing.T, got []byte, compact, spaced string) {
	t.Helper()
	if string(got) != compact && string(got) != spaced {
		t.Fatalf("json = %s, want %s", got, compact)
	}
}

func checkRecoveryCharacter(t *testing.T, f *deathFixture, got DeathCharacterRecoverySnapshot, wantRev int64) {
	t.Helper()
	c := got.Character
	if c.ID != f.victimID || c.ExpectedRevision != wantRev || c.Karma != 11 ||
		c.PosX != 111 || c.PosY != 222 || c.PosZ != 333 || c.Flags != 3 {
		t.Fatalf("character = %+v, want victim root at rev %d", got.Character, wantRev)
	}
	sameJSON(t, c.Vitals, `{"hp":1,"threshold":80}`, `{"hp": 1, "threshold": 80}`)
	sameJSON(t, c.Advancement, `{"pts":0}`, `{"pts": 0}`)
	if len(c.Spells) != 2 || c.Spells[0].SpellID != 1 || c.Spells[1].SpellID != 2 ||
		c.Spells[0].Ability != 10 || c.Spells[1].Ability != 20 {
		t.Fatalf("spells = %+v, want ordered 1/2", c.Spells)
	}
	if len(c.Skills) != 2 || c.Skills[0].SkillID != 1 || c.Skills[1].SkillID != 2 ||
		c.Skills[0].Ability != 30 || c.Skills[1].Ability != 40 {
		t.Fatalf("skills = %+v, want ordered 1/2", c.Skills)
	}
	if root := readRoot(t, f.q, f.victimID); root.Revision != c.ExpectedRevision {
		t.Fatalf("ExpectedRevision = %d, persisted = %d", c.ExpectedRevision, root.Revision)
	}
}

// TestLoadDeathCharacterRecoveryComplete proves the full composite
// shape after death entry + Portal: exact root/children, exact
// pending cost/time/corpse/portal flag, ExpectedRevision ==
// persisted revision.
func TestLoadDeathCharacterRecoveryComplete(t *testing.T) {
	f := newDeathFixture(t)
	ctx := context.Background()
	corpseID := commitRecoveryDeath(t, f, recoveryCharSnapshot(f, 0))
	portalSnap := recoveryCharSnapshot(f, 1)
	pres, err := f.st.CommitPortalOfLife(ctx, PortalOfLifeRequest{
		Character: portalSnap, CorpseID: corpseID, ProposedCost: recoveryProposedCost,
	})
	if err != nil {
		t.Fatalf("portal: %v", err)
	}
	if pres.CharacterRevision != 2 || pres.EffectiveCost != recoveryProposedCost {
		t.Fatalf("portal result = %+v, want rev 2 cost %d", pres, recoveryProposedCost)
	}
	got, err := f.st.LoadDeathCharacterRecovery(ctx, f.victimID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	checkRecoveryCharacter(t, f, got, 2)
	if got.Pending == nil {
		t.Fatal("Pending = nil, want pending row")
	}
	p := got.Pending
	if p.CharacterID != f.victimID || p.EffectiveCost != recoveryProposedCost ||
		p.DeathTimeSeconds != deathTimeSeconds || !p.PortalUsed {
		t.Fatalf("pending = %+v", p)
	}
	if p.CorpseID == nil || *p.CorpseID != corpseID {
		t.Fatalf("pending corpse = %v, want %d", p.CorpseID, corpseID)
	}
}

// TestLoadDeathCharacterRecoveryNoPending proves a complete
// character without a pending row loads with Pending == nil.
func TestLoadDeathCharacterRecoveryNoPending(t *testing.T) {
	f := newDeathFixture(t)
	ctx := context.Background()
	snap := CharacterSnapshot{
		ID: f.killerCharID, ExpectedRevision: 0, Karma: -5,
		PosX: 1, PosY: 2, PosZ: 3,
		Vitals:      json.RawMessage(`{"hp":20}`),
		Advancement: json.RawMessage(`{"pts":7}`),
		Flags:       9,
		Spells: []CharacterSpellSnapshot{
			{SpellID: 3, Ability: 50},
			{SpellID: 1, Ability: 60},
		},
		Skills: []CharacterSkillSnapshot{
			{SkillID: 2, Ability: 70},
			{SkillID: 1, Ability: 80},
		},
	}
	if _, err := f.st.SaveCharacterSnapshot(ctx, snap); err != nil {
		t.Fatalf("setup save: %v", err)
	}
	got, err := f.st.LoadDeathCharacterRecovery(ctx, f.killerCharID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	c := got.Character
	if c.ID != f.killerCharID || c.ExpectedRevision != 1 || c.Karma != -5 || c.Flags != 9 {
		t.Fatalf("character = %+v, want killer root rev 1", c)
	}
	if len(c.Spells) != 2 || c.Spells[0].SpellID != 1 || c.Spells[1].SpellID != 3 {
		t.Fatalf("spells = %+v, want spell_id ascending", c.Spells)
	}
	if len(c.Skills) != 2 || c.Skills[0].SkillID != 1 || c.Skills[1].SkillID != 2 {
		t.Fatalf("skills = %+v, want skill_id ascending", c.Skills)
	}
	if got.Pending != nil {
		t.Fatalf("Pending = %+v, want nil", got.Pending)
	}
}

// TestLoadDeathCharacterRecoveryExpiredCorpse proves the pending
// row survives corpse deletion with CorpseID == nil while
// cost/time/portal state is preserved.
func TestLoadDeathCharacterRecoveryExpiredCorpse(t *testing.T) {
	f := newDeathFixture(t)
	ctx := context.Background()
	corpseID := commitRecoveryDeath(t, f, recoveryCharSnapshot(f, 0))
	if err := f.q.DeleteCorpse(ctx, corpseID); err != nil {
		t.Fatalf("delete corpse: %v", err)
	}
	got, err := f.st.LoadDeathCharacterRecovery(ctx, f.victimID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	checkRecoveryCharacter(t, f, got, 1)
	if got.Pending == nil {
		t.Fatal("pending row lost with corpse expiry")
	}
	p := got.Pending
	if p.CorpseID != nil {
		t.Fatalf("pending corpse = %d, want nil after expiry", *p.CorpseID)
	}
	if p.EffectiveCost != recoveryDeathCost || p.DeathTimeSeconds != deathTimeSeconds || p.PortalUsed {
		t.Fatalf("pending = %+v, want cost/time/portal preserved", p)
	}
}

// TestLoadDeathCharacterRecoveryDeletedAndMissing proves
// soft-deleted and absent characters fail with the missing-row
// convention and zero result — never a resurrection.
func TestLoadDeathCharacterRecoveryDeletedAndMissing(t *testing.T) {
	f := newDeathFixture(t)
	ctx := context.Background()
	snap := CharacterSnapshot{
		ID: f.killerCharID, ExpectedRevision: 0, Karma: 1,
		Vitals: json.RawMessage(`{}`), Advancement: json.RawMessage(`{}`),
	}
	if _, err := f.st.SaveCharacterSnapshot(ctx, snap); err != nil {
		t.Fatalf("setup save: %v", err)
	}
	if _, err := f.st.SoftDeleteCharacter(ctx, f.killerCharID, 1); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	got, err := f.st.LoadDeathCharacterRecovery(ctx, f.killerCharID)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("deleted err = %v, want pgx.ErrNoRows", err)
	}
	if !reflect.DeepEqual(got, DeathCharacterRecoverySnapshot{}) {
		t.Fatalf("deleted result = %+v, want zero", got)
	}
	got, err = f.st.LoadDeathCharacterRecovery(ctx, 1<<40)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing err = %v, want pgx.ErrNoRows", err)
	}
	if !reflect.DeepEqual(got, DeathCharacterRecoverySnapshot{}) {
		t.Fatalf("missing result = %+v, want zero", got)
	}
}

func checkRecoveryItem(t *testing.T, f *deathFixture, got DeathItemRecoverySnapshot, id, wantRev int64) {
	t.Helper()
	it := got.Item
	if it.ID != id || it.ExpectedRevision != wantRev || it.Qty != 5 || it.Hits != 77 {
		t.Fatalf("item = %+v, want id %d rev %d qty 5 hits 77", it, id, wantRev)
	}
	sameJSON(t, it.Enchants, `{"e":1}`, `{"e": 1}`)
	loc := it.Location
	if loc.Kind != 1 || loc.CharacterID != nil || loc.CorpseID != nil ||
		loc.ContainerItemID != nil || loc.VaultRegion != nil || loc.Slot != nil {
		t.Fatalf("location = %+v, want pure ground", loc)
	}
	for _, p := range []*int64{loc.PosX, loc.PosY, loc.PosZ} {
		if p == nil {
			t.Fatalf("location = %+v, want death position", loc)
		}
	}
	if *loc.PosX != deathPosX || *loc.PosY != deathPosY || *loc.PosZ != deathPosZ {
		t.Fatalf("location = %+v, want death pos", loc)
	}
	if row := readItem(t, f.q, id); row.Revision != it.ExpectedRevision {
		t.Fatalf("ExpectedRevision = %d, persisted = %d", it.ExpectedRevision, row.Revision)
	}
}

// TestLoadDeathItemRecoveryProtected proves the complete
// death-mutated item shape with exact protection state.
func TestLoadDeathItemRecoveryProtected(t *testing.T) {
	f := newDeathFixture(t)
	ctx := context.Background()
	commitRecoveryDeath(t, f, recoveryCharSnapshot(f, 0))
	got, err := f.st.LoadDeathItemRecovery(ctx, f.itemA)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	checkRecoveryItem(t, f, got, f.itemA, 1)
	if got.PKProtection == nil {
		t.Fatal("PKProtection = nil, want protection row")
	}
	prot := got.PKProtection
	deathInstant := time.Unix(deathTimeSeconds, 0).UTC()
	if prot.ItemID != f.itemA || prot.VictimCharacterID != f.victimID ||
		!prot.ExpiresAt.Equal(deathInstant.Add(deathPKDuration)) {
		t.Fatalf("protection = %+v", prot)
	}
}

// TestLoadDeathItemRecoveryUnprotected proves PKProtection == nil
// without error for an item with no protection row.
func TestLoadDeathItemRecoveryUnprotected(t *testing.T) {
	f := newDeathFixture(t)
	ctx := context.Background()
	commitRecoveryDeath(t, f, recoveryCharSnapshot(f, 0))
	got, err := f.st.LoadDeathItemRecovery(ctx, f.itemB)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	checkRecoveryItem(t, f, got, f.itemB, 1)
	if got.PKProtection != nil {
		t.Fatalf("PKProtection = %+v, want nil", got.PKProtection)
	}
}

// TestLoadDeathItemRecoveryMissing proves missing roots and
// deliberately removed locations fail with zero result and no
// fabricated location.
func TestLoadDeathItemRecoveryMissing(t *testing.T) {
	f := newDeathFixture(t)
	ctx := context.Background()
	commitRecoveryDeath(t, f, recoveryCharSnapshot(f, 0))
	got, err := f.st.LoadDeathItemRecovery(ctx, 1<<40)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing err = %v, want pgx.ErrNoRows", err)
	}
	if !reflect.DeepEqual(got, DeathItemRecoverySnapshot{}) {
		t.Fatalf("missing result = %+v, want zero", got)
	}
	if _, err := f.pool.Exec(ctx, `DELETE FROM item_locations WHERE item_id = $1`, f.itemB); err != nil {
		t.Fatalf("remove location: %v", err)
	}
	got, err = f.st.LoadDeathItemRecovery(ctx, f.itemB)
	if err == nil {
		t.Fatal("locationless load succeeded, want error")
	}
	if !reflect.DeepEqual(got, DeathItemRecoverySnapshot{}) {
		t.Fatalf("locationless result = %+v, want zero (no fabricated location)", got)
	}
}

// TestDeathRecoveryRepeatableRead proves the recovery transaction
// is REPEATABLE READ and READ ONLY without timing: SHOW checks, a
// rejected write, and snapshot stability across a concurrently
// committed update on the same aggregate.
func TestDeathRecoveryRepeatableRead(t *testing.T) {
	f := newDeathFixture(t)
	ctx := context.Background()
	tx, err := beginDeathRecoveryTx(ctx, f.pool)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var iso, ro string
	if err := tx.QueryRow(ctx, `SHOW transaction_isolation`).Scan(&iso); err != nil {
		t.Fatalf("show isolation: %v", err)
	}
	if iso != "repeatable read" {
		t.Fatalf("isolation = %q, want repeatable read", iso)
	}
	if err := tx.QueryRow(ctx, `SHOW transaction_read_only`).Scan(&ro); err != nil {
		t.Fatalf("show read_only: %v", err)
	}
	if ro != "on" {
		t.Fatalf("read_only = %q, want on", ro)
	}
	if _, err := tx.Exec(ctx, `UPDATE characters SET karma = karma WHERE id = $1`, f.victimID); err == nil {
		t.Fatal("write inside read-only recovery transaction succeeded")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// Snapshot stability: establish the tx snapshot, commit an
	// update outside it, and prove the tx still reads the old
	// coherent state while fresh reads see the new one.
	snapTx, err := beginDeathRecoveryTx(ctx, f.pool)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = snapTx.Rollback(ctx) }()
	first, err := loadDeathCharacterRecoveryTx(ctx, snapTx, f.victimID)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	moved := CharacterSnapshot{
		ID: f.victimID, ExpectedRevision: first.Character.ExpectedRevision, Karma: 42,
		Vitals: json.RawMessage(`{}`), Advancement: json.RawMessage(`{}`),
	}
	if _, err := f.st.SaveCharacterSnapshot(ctx, moved); err != nil {
		t.Fatalf("concurrent save: %v", err)
	}
	second, err := loadDeathCharacterRecoveryTx(ctx, snapTx, f.victimID)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("repeatable-read snapshot drifted:\nfirst=%+v\nsecond=%+v", first, second)
	}
	if err := snapTx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	fresh, err := f.st.LoadDeathCharacterRecovery(ctx, f.victimID)
	if err != nil {
		t.Fatalf("fresh load: %v", err)
	}
	if fresh.Character.Karma != 42 ||
		fresh.Character.ExpectedRevision != first.Character.ExpectedRevision+1 {
		t.Fatalf("fresh = %+v, want committed update", fresh.Character)
	}
}

// TestDeathRecoveryNoStaleMetric proves recovery reads — success,
// missing roots, optional-child absence, corrupt location,
// soft-deleted character — never increment the stale-revision
// counter. No new metric exists.
func TestDeathRecoveryNoStaleMetric(t *testing.T) {
	f := newDeathFixture(t)
	ctx := context.Background()
	commitRecoveryDeath(t, f, recoveryCharSnapshot(f, 0))
	beforeChar := testutil.ToFloat64(f.st.stale.WithLabelValues("character"))
	beforeItem := testutil.ToFloat64(f.st.stale.WithLabelValues("item"))
	if _, err := f.st.LoadDeathCharacterRecovery(ctx, f.victimID); err != nil {
		t.Fatalf("complete char: %v", err)
	}
	if _, err := f.st.LoadDeathCharacterRecovery(ctx, f.killerCharID); err != nil {
		t.Fatalf("no-pending char: %v", err)
	}
	if _, err := f.st.LoadDeathCharacterRecovery(ctx, 1<<40); err == nil {
		t.Fatal("missing char succeeded")
	}
	if _, err := f.st.LoadDeathItemRecovery(ctx, f.itemA); err != nil {
		t.Fatalf("protected item: %v", err)
	}
	if _, err := f.st.LoadDeathItemRecovery(ctx, f.itemB); err != nil {
		t.Fatalf("unprotected item: %v", err)
	}
	if _, err := f.st.LoadDeathItemRecovery(ctx, 1<<40); err == nil {
		t.Fatal("missing item succeeded")
	}
	if _, err := f.pool.Exec(ctx, `DELETE FROM item_locations WHERE item_id = $1`, f.itemB); err != nil {
		t.Fatalf("remove location: %v", err)
	}
	if _, err := f.st.LoadDeathItemRecovery(ctx, f.itemB); err == nil {
		t.Fatal("locationless item succeeded")
	}
	if _, err := f.st.SoftDeleteCharacter(ctx, f.killerCharID, 0); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if _, err := f.st.LoadDeathCharacterRecovery(ctx, f.killerCharID); err == nil {
		t.Fatal("deleted char succeeded")
	}
	if got := testutil.ToFloat64(f.st.stale.WithLabelValues("character")); got != beforeChar {
		t.Fatalf("character stale %v -> %v across reads", beforeChar, got)
	}
	if got := testutil.ToFloat64(f.st.stale.WithLabelValues("item")); got != beforeItem {
		t.Fatalf("item stale %v -> %v across reads", beforeItem, got)
	}
}
