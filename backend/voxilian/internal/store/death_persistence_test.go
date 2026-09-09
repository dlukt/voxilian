package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dlukt/voxilian/internal/store/gen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// TestDeathPersistenceQueries proves the M5-T5b1a low-level primitives
// (spec §9.5.8a) against real PostgreSQL 18. These are building blocks
// for the T5b1b/T5b2 transactions ONLY: no public Store mutation exists
// here, and none is added — composition happens in the owning task.
func TestDeathPersistenceQueries(t *testing.T) {
	_, q := openQueries(t)
	ctx := context.Background()

	acct, err := q.CreateAccount(ctx, gen.CreateAccountParams{KeycloakSub: "sub-death-persist"})
	if err != nil {
		t.Fatal(err)
	}
	victim, err := createCharacter(ctx, q, validCharParams(acct.ID, 0, "DeathVictim"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := createCharacter(ctx, q, validCharParams(acct.ID, 1, "DeathOther"))
	if err != nil {
		t.Fatal(err)
	}
	acct2, err := q.CreateAccount(ctx, gen.CreateAccountParams{KeycloakSub: "sub-death-third"})
	if err != nil {
		t.Fatal(err)
	}
	third, err := createCharacter(ctx, q, validCharParams(acct2.ID, 0, "DeathThird"))
	if err != nil {
		t.Fatal(err)
	}
	newCorpse := func(t *testing.T, chID int64) gen.Corpse {
		t.Helper()
		c, err := q.InsertCorpse(ctx, gen.InsertCorpseParams{
			CharacterID: chID,
			ExpiresAt:   pgtype.Timestamptz{Time: time.Now().Add(10 * time.Minute).UTC(), Valid: true},
		})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	corpse := newCorpse(t, victim.ID)

	// Insert + get round-trip (cost-100 boundary, portal unused).
	row, err := q.InsertPendingDeath(ctx, gen.InsertPendingDeathParams{
		CharacterID:      victim.ID,
		EffectiveCost:    100,
		DeathTimeSeconds: 42,
		CorpseID:         int8(corpse.ID),
		PortalUsed:       false,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if row.CharacterID != victim.ID || row.EffectiveCost != 100 ||
		row.DeathTimeSeconds != 42 || !row.CorpseID.Valid || row.CorpseID.Int64 != corpse.ID ||
		row.PortalUsed || !row.CreatedAt.Valid {
		t.Fatalf("pending = %+v", row)
	}
	got, err := q.GetPendingDeathByCharacter(ctx, victim.ID)
	if err != nil || got != row {
		t.Fatalf("get = %+v, %v; want %+v", got, err, row)
	}

	// Duplicate pending death for one character is impossible; T5b1b
	// maps this exact violation to ErrDeathAlreadyPending.
	if _, err := q.InsertPendingDeath(ctx, gen.InsertPendingDeathParams{
		CharacterID: victim.ID,
	}); err == nil || !contains(err.Error(), "pending_deaths_pkey") {
		t.Fatalf("duplicate err = %v, want pending_deaths_pkey", err)
	}

	// Missing row is NoRows (recovery path distinguishes absent state).
	if _, err := q.GetPendingDeathByCharacter(ctx, other.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing err = %v, want NoRows", err)
	}

	// Cost domain enforced by the DB CHECK (0..100, no NULL magic).
	for _, cost := range []int16{-1, 101} {
		if _, err := q.InsertPendingDeath(ctx, gen.InsertPendingDeathParams{
			CharacterID: other.ID, EffectiveCost: cost,
		}); err == nil || !contains(err.Error(), "pending_deaths_effective_cost_check") {
			t.Fatalf("cost %d err = %v, want effective_cost_check", cost, err)
		}
	}
	// Time domain enforced (>= 0).
	if _, err := q.InsertPendingDeath(ctx, gen.InsertPendingDeathParams{
		CharacterID: other.ID, DeathTimeSeconds: -1,
	}); err == nil || !contains(err.Error(), "pending_deaths_death_time_check") {
		t.Fatalf("negative time err = %v, want death_time_check", err)
	}
	// FKs enforced.
	if _, err := q.InsertPendingDeath(ctx, gen.InsertPendingDeathParams{CharacterID: 999999}); err == nil {
		t.Fatal("bad character accepted")
	}
	if _, err := q.InsertPendingDeath(ctx, gen.InsertPendingDeathParams{
		CharacterID: other.ID, CorpseID: int8(999999),
	}); err == nil {
		t.Fatal("bad corpse accepted")
	}

	// Cost-0 boundary with NULL corpse and portal-used round-trip.
	cheap, err := q.InsertPendingDeath(ctx, gen.InsertPendingDeathParams{
		CharacterID: other.ID, EffectiveCost: 0, DeathTimeSeconds: 0,
		PortalUsed: true,
	})
	if err != nil {
		t.Fatalf("cheap insert: %v", err)
	}
	if cheap.EffectiveCost != 0 || cheap.DeathTimeSeconds != 0 || !cheap.PortalUsed || cheap.CorpseID.Valid {
		t.Fatalf("cheap = %+v", cheap)
	}

	// One live corpse backs at most one pending row: a third character
	// referencing the same live corpse is rejected by the partial
	// unique index (NULL-corpse rows are unaffected — see above).
	if _, err := q.InsertPendingDeath(ctx, gen.InsertPendingDeathParams{
		CharacterID: third.ID, CorpseID: int8(corpse.ID),
	}); err == nil || !contains(err.Error(), "pending_deaths_corpse_uidx") {
		t.Fatalf("shared corpse err = %v, want pending_deaths_corpse_uidx", err)
	}

	// Corpse expiry MUST NOT take the pending death with it: deleting
	// the corpse row leaves the pending row with a NULL association
	// and cost/time/portal state preserved (ON DELETE SET NULL).
	corpse2 := newCorpse(t, other.ID)
	if err := q.DeletePendingDeathByCharacter(ctx, other.ID); err != nil {
		t.Fatalf("delete cheap: %v", err)
	}
	if _, err := q.InsertPendingDeath(ctx, gen.InsertPendingDeathParams{
		CharacterID:      other.ID,
		EffectiveCost:    37,
		DeathTimeSeconds: 7,
		CorpseID:         int8(corpse2.ID),
		PortalUsed:       true,
	}); err != nil {
		t.Fatalf("survivor insert: %v", err)
	}
	if err := q.DeleteCorpse(ctx, corpse2.ID); err != nil {
		t.Fatalf("expire corpse: %v", err)
	}
	after, err := q.GetPendingDeathByCharacter(ctx, other.ID)
	if err != nil {
		t.Fatalf("get after expiry: %v", err)
	}
	if after.CorpseID.Valid || after.EffectiveCost != 37 ||
		after.DeathTimeSeconds != 7 || !after.PortalUsed {
		t.Fatalf("after expiry = %+v, want NULL corpse with cost 37/time 7/portal", after)
	}

	// Delete clears the pending phase; the row is gone afterwards, and
	// deleting an absent row is a no-op (T5b2 clear is idempotent).
	if err := q.DeletePendingDeathByCharacter(ctx, other.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := q.GetPendingDeathByCharacter(ctx, other.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("after delete err = %v, want NoRows", err)
	}
	if err := q.DeletePendingDeathByCharacter(ctx, other.ID); err != nil {
		t.Fatalf("delete absent: %v", err)
	}
}

// TestItemPKProtectionQueries proves the M5-T5b1a PK-protection
// primitives (spec §9.5.8a): one row per item, victim FK, expiry
// round-trip, deterministic UPSERT replacement. Like pending deaths,
// these compose inside item-root CAS transactions in T5b1b — no public
// Store mutation is added here.
func TestItemPKProtectionQueries(t *testing.T) {
	pool, q := openQueries(t)
	ctx := context.Background()

	acct, err := q.CreateAccount(ctx, gen.CreateAccountParams{KeycloakSub: "sub-pk-protect"})
	if err != nil {
		t.Fatal(err)
	}
	victim, err := createCharacter(ctx, q, validCharParams(acct.ID, 0, "PKVictim"))
	if err != nil {
		t.Fatal(err)
	}
	victim2, err := createCharacter(ctx, q, validCharParams(acct.ID, 1, "PKVictim2"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO item_protos (id,kind,slot,base,version) VALUES (711,0,NULL,'{}',1)
		ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	root, _, err := createItemWithLocation(ctx, pool,
		gen.InsertItemInstanceParams{Proto: 711, Qty: 1, Hits: 100, Enchants: []byte("{}")},
		NewItemLocation{Kind: 0, CharacterID: int8(victim.ID), Slot: pgText("hand")},
	)
	if err != nil {
		t.Fatal(err)
	}

	exp1 := pgtype.Timestamptz{Time: time.Now().Add(10 * time.Minute).UTC().Truncate(time.Millisecond), Valid: true}
	prot, err := q.UpsertItemPKProtection(ctx, gen.UpsertItemPKProtectionParams{
		ItemID: root.ID, VictimCharacterID: victim.ID, ExpiresAt: exp1,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if prot.ItemID != root.ID || prot.VictimCharacterID != victim.ID ||
		!prot.ExpiresAt.Time.Equal(exp1.Time) {
		t.Fatalf("protection = %+v", prot)
	}
	got, err := q.GetItemPKProtection(ctx, root.ID)
	if err != nil || got != prot {
		t.Fatalf("get = %+v, %v; want %+v", got, err, prot)
	}

	// Re-protection deterministically REPLACES victim + expiry: still
	// exactly one row, carrying exactly the new values.
	exp2 := pgtype.Timestamptz{Time: time.Now().Add(20 * time.Minute).UTC().Truncate(time.Millisecond), Valid: true}
	repl, err := q.UpsertItemPKProtection(ctx, gen.UpsertItemPKProtectionParams{
		ItemID: root.ID, VictimCharacterID: victim2.ID, ExpiresAt: exp2,
	})
	if err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if repl.ItemID != root.ID || repl.VictimCharacterID != victim2.ID ||
		!repl.ExpiresAt.Time.Equal(exp2.Time) {
		t.Fatalf("replaced = %+v", repl)
	}
	var n int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM item_pk_protections WHERE item_id = $1`, root.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("count = %d, %v; want exactly 1", n, err)
	}

	// Missing row is NoRows; bad item/victim are FK violations.
	if _, err := q.GetItemPKProtection(ctx, root.ID+999999); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing err = %v, want NoRows", err)
	}
	if _, err := q.UpsertItemPKProtection(ctx, gen.UpsertItemPKProtectionParams{
		ItemID: 999999, VictimCharacterID: victim.ID, ExpiresAt: exp1,
	}); err == nil {
		t.Fatal("bad item accepted")
	}
	if _, err := q.UpsertItemPKProtection(ctx, gen.UpsertItemPKProtectionParams{
		ItemID: root.ID, VictimCharacterID: 999999, ExpiresAt: exp1,
	}); err == nil {
		t.Fatal("bad victim accepted")
	}

	// Delete removes protection; deleting an absent row is a no-op.
	if err := q.DeleteItemPKProtection(ctx, root.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := q.GetItemPKProtection(ctx, root.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("after delete err = %v, want NoRows", err)
	}
	if err := q.DeleteItemPKProtection(ctx, root.ID); err != nil {
		t.Fatalf("delete absent: %v", err)
	}
}
