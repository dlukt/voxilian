package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/dlukt/voxilian/internal/store/gen"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// M5-T5b2a Portal-of-Life durable transition tests (spec §9.5.10a,
// §8.1/§8.3). All durable proofs run against real PostgreSQL 18. No
// migration beyond 6, no sim/gateway/proto change: composition uses
// the existing saveCharacterSnapshotTx seam plus the single
// ApplyPendingDeathPortal primitive. T5b2a writes ZERO ledger rows
// and ZERO kills rows.

type portalFixture struct {
	pool     *pgxpool.Pool
	q        *gen.Queries
	st       *PGStore
	victimID int64
}

func newPortalFixture(t *testing.T) *portalFixture {
	t.Helper()
	pool, q := openQueries(t)
	st := newTestStore(t, pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO spell_protos (id,school,level,mana,exertion,cast_ms,min_hp,outlaw,harmful,reagents,params,version) VALUES (61,1,1,1,1,0,1,false,false,'{}','{}',1) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO skill_protos (id,division,level,exertion,params,version) VALUES (61,1,1,1,'{}',1) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	acct, err := q.CreateAccount(ctx, gen.CreateAccountParams{KeycloakSub: "sub-portal-life"})
	if err != nil {
		t.Fatal(err)
	}
	victim, err := createCharacter(ctx, q, validCharParams(acct.ID, 0, "PortalVictim"))
	if err != nil {
		t.Fatal(err)
	}
	return &portalFixture{pool: pool, q: q, st: st, victimID: victim.ID}
}

// portalCharSnapshot is a complete already-resolved authoritative
// character snapshot. Portal normally changes no root gameplay field
// itself; the snapshot still composes fully through the
// character-root CAS with distinct values proving persistence.
func (f *portalFixture) portalCharSnapshot(rev int64) CharacterSnapshot {
	return CharacterSnapshot{
		ID: f.victimID, ExpectedRevision: rev, Karma: 42,
		PosX: 1001, PosY: 2002, PosZ: 3003,
		Vitals:      json.RawMessage(`{"hp":1,"threshold":77}`),
		Advancement: json.RawMessage(`{"pts":0}`),
		Flags:       9,
		Spells:      []CharacterSpellSnapshot{{SpellID: 61, Ability: 33}},
		Skills:      []CharacterSkillSnapshot{{SkillID: 61, Ability: 44}},
	}
}

// seedPortalPending inserts one corpse plus one pending row and
// returns the corpse ID.
func (f *portalFixture) seedPortalPending(t *testing.T, cost int16, deathTime int64, portalUsed bool) int64 {
	t.Helper()
	ctx := context.Background()
	corpse, err := f.q.InsertCorpse(ctx, gen.InsertCorpseParams{
		CharacterID: f.victimID,
		ExpiresAt:   pgtype.Timestamptz{Time: time.Now().Add(time.Hour).UTC(), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.InsertPendingDeath(ctx, gen.InsertPendingDeathParams{
		CharacterID:      f.victimID,
		EffectiveCost:    cost,
		DeathTimeSeconds: deathTime,
		CorpseID:         int8(corpse.ID),
		PortalUsed:       portalUsed,
	}); err != nil {
		t.Fatal(err)
	}
	return corpse.ID
}

func (f *portalFixture) staleCount(agg string) float64 {
	return testutil.ToFloat64(f.st.stale.WithLabelValues(agg))
}

func portalCount(t *testing.T, f *portalFixture, query string, args ...any) int {
	t.Helper()
	return countFor(t, f.pool, query, args...)
}

func isZeroPortalResult(res PortalOfLifeResult) bool {
	return res.CharacterRevision == 0 && res.EffectiveCost == 0
}

// assertPortalSuccess checks the full durable effect of a successful
// Portal commit: character revision +1 with the complete snapshot
// persisted, exact pending cost/flag, unchanged death time and
// corpse association, exactly one pending row, and zero audit rows.
func assertPortalSuccess(t *testing.T, f *portalFixture, res PortalOfLifeResult, corpseID int64, wantCost int16, wantDeathTime int64) {
	t.Helper()
	ctx := context.Background()
	if res.CharacterRevision != 1 || res.EffectiveCost != wantCost {
		t.Fatalf("result = %+v, want rev 1 cost %d", res, wantCost)
	}
	got := readRoot(t, f.q, f.victimID)
	if got.Revision != 1 || got.Karma != 42 || got.PosX != 1001 ||
		got.PosY != 2002 || got.PosZ != 3003 || got.Flags != 9 {
		t.Fatalf("character root = %+v, want post-portal state rev 1", got)
	}
	var vitals map[string]int64
	if err := json.Unmarshal(got.Vitals, &vitals); err != nil || vitals["threshold"] != 77 {
		t.Fatalf("vitals = %s, %v; want caller snapshot unchanged", got.Vitals, err)
	}
	spells, err := f.q.ListCharacterSpells(ctx, f.victimID)
	if err != nil || len(spells) != 1 || spells[0].SpellID != 61 || spells[0].Ability != 33 {
		t.Fatalf("spells = %+v, %v", spells, err)
	}
	skills, err := f.q.ListCharacterSkills(ctx, f.victimID)
	if err != nil || len(skills) != 1 || skills[0].SkillID != 61 || skills[0].Ability != 44 {
		t.Fatalf("skills = %+v, %v", skills, err)
	}
	pending, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID)
	if err != nil {
		t.Fatalf("get pending: %v", err)
	}
	if pending.EffectiveCost != wantCost || !pending.PortalUsed ||
		pending.DeathTimeSeconds != wantDeathTime ||
		!pending.CorpseID.Valid || pending.CorpseID.Int64 != corpseID {
		t.Fatalf("pending = %+v, want cost %d portal_used time %d corpse %d",
			pending, wantCost, wantDeathTime, corpseID)
	}
	if n := portalCount(t, f, `SELECT COUNT(*) FROM pending_deaths WHERE character_id = $1`, f.victimID); n != 1 {
		t.Fatalf("pending rows = %d, want exactly 1", n)
	}
	if n := portalCount(t, f, `SELECT COUNT(*) FROM kills`); n != 0 {
		t.Fatalf("kills = %d, want 0", n)
	}
	if n := portalCount(t, f, `SELECT COUNT(*) FROM ledger`); n != 0 {
		t.Fatalf("ledger rows = %d, want 0", n)
	}
}

// TestCommitPortalOfLifeLowers proves the lowering path: cost
// 100 + proposal 40 commits cost 40 with the full durable effect.
func TestCommitPortalOfLifeLowers(t *testing.T) {
	f := newPortalFixture(t)
	ctx := context.Background()
	corpseID := f.seedPortalPending(t, 100, 500, false)

	res, err := f.st.CommitPortalOfLife(ctx, PortalOfLifeRequest{
		Character:    f.portalCharSnapshot(0),
		CorpseID:     corpseID,
		ProposedCost: 40,
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	assertPortalSuccess(t, f, res, corpseID, 40, 500)
}

// TestCommitPortalOfLifeWorseStillConsumes proves once-per-corpse:
// cost 30 + worse proposal 50 keeps cost 30 but still sets
// portal_used.
func TestCommitPortalOfLifeWorseStillConsumes(t *testing.T) {
	f := newPortalFixture(t)
	ctx := context.Background()
	corpseID := f.seedPortalPending(t, 30, 501, false)

	res, err := f.st.CommitPortalOfLife(ctx, PortalOfLifeRequest{
		Character:    f.portalCharSnapshot(0),
		CorpseID:     corpseID,
		ProposedCost: 50,
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	assertPortalSuccess(t, f, res, corpseID, 30, 501)
}

// TestCommitPortalOfLifeCheapStaysZero proves the mandatory cheap
// corner: cost 0 + proposal 5 (the pure formula floor) keeps cost 0
// and still consumes Portal. A cheap death is never raised to 5.
func TestCommitPortalOfLifeCheapStaysZero(t *testing.T) {
	f := newPortalFixture(t)
	ctx := context.Background()
	corpseID := f.seedPortalPending(t, 0, 502, false)

	res, err := f.st.CommitPortalOfLife(ctx, PortalOfLifeRequest{
		Character:    f.portalCharSnapshot(0),
		CorpseID:     corpseID,
		ProposedCost: 5,
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	assertPortalSuccess(t, f, res, corpseID, 0, 502)
}

// TestCommitPortalOfLifeAlreadyUsed proves a second Portal against
// the same corpse maps to ErrPortalAlreadyUsed with the tentative
// character CAS fully rolled back and no stale count.
func TestCommitPortalOfLifeAlreadyUsed(t *testing.T) {
	f := newPortalFixture(t)
	ctx := context.Background()
	corpseID := f.seedPortalPending(t, 40, 503, true)
	beforeChar := readRoot(t, f.q, f.victimID)
	beforePending, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID)
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.st.CommitPortalOfLife(ctx, PortalOfLifeRequest{
		Character:    f.portalCharSnapshot(0),
		CorpseID:     corpseID,
		ProposedCost: 5,
	})
	if !errors.Is(err, ErrPortalAlreadyUsed) {
		t.Fatalf("err = %v, want ErrPortalAlreadyUsed", err)
	}
	if errors.Is(err, ErrStaleRevision) {
		t.Fatalf("semantic rejection mapped to stale: %v", err)
	}
	if !isZeroPortalResult(res) {
		t.Fatalf("result = %+v, want zero", res)
	}
	if after := readRoot(t, f.q, f.victimID); !sameCharacter(after, beforeChar) {
		t.Fatalf("character moved: %+v vs %+v", after, beforeChar)
	}
	afterPending, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID)
	if err != nil || afterPending != beforePending {
		t.Fatalf("pending = %+v, %v; want unchanged %+v", afterPending, err, beforePending)
	}
	if got := f.staleCount("character"); got != 0 {
		t.Fatalf("character stale = %v, want 0", got)
	}
	if n := portalCount(t, f, `SELECT COUNT(*) FROM kills`); n != 0 {
		t.Fatalf("kills = %d, want 0", n)
	}
	if n := portalCount(t, f, `SELECT COUNT(*) FROM ledger`); n != 0 {
		t.Fatalf("ledger rows = %d, want 0", n)
	}
}

// TestCommitPortalOfLifeWrongCorpse proves a request naming a
// different corpse than the pending row maps to
// ErrPortalCorpseMismatch with no mutation.
func TestCommitPortalOfLifeWrongCorpse(t *testing.T) {
	f := newPortalFixture(t)
	ctx := context.Background()
	corpseID := f.seedPortalPending(t, 100, 504, false)
	beforeChar := readRoot(t, f.q, f.victimID)
	beforePending, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID)
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.st.CommitPortalOfLife(ctx, PortalOfLifeRequest{
		Character:    f.portalCharSnapshot(0),
		CorpseID:     corpseID + 999999,
		ProposedCost: 40,
	})
	if !errors.Is(err, ErrPortalCorpseMismatch) {
		t.Fatalf("err = %v, want ErrPortalCorpseMismatch", err)
	}
	if !isZeroPortalResult(res) {
		t.Fatalf("result = %+v, want zero", res)
	}
	if after := readRoot(t, f.q, f.victimID); !sameCharacter(after, beforeChar) {
		t.Fatalf("character moved: %+v vs %+v", after, beforeChar)
	}
	afterPending, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID)
	if err != nil || afterPending != beforePending {
		t.Fatalf("pending = %+v, %v; want unchanged %+v", afterPending, err, beforePending)
	}
	if got := f.staleCount("character"); got != 0 {
		t.Fatalf("character stale = %v, want 0", got)
	}
}

// TestCommitPortalOfLifeExpiredCorpse proves the corpse-expiry race:
// deleting the corpse NULLs the pending association (ON DELETE SET
// NULL) while cost/portal state survives, and a Portal naming the
// old corpse ID maps to ErrPortalCorpseMismatch with no mutation.
func TestCommitPortalOfLifeExpiredCorpse(t *testing.T) {
	f := newPortalFixture(t)
	ctx := context.Background()
	corpseID := f.seedPortalPending(t, 100, 505, false)
	if err := f.q.DeleteCorpse(ctx, corpseID); err != nil {
		t.Fatalf("expire corpse: %v", err)
	}
	nulled, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID)
	if err != nil {
		t.Fatalf("get after expiry: %v", err)
	}
	if nulled.CorpseID.Valid || nulled.EffectiveCost != 100 ||
		nulled.DeathTimeSeconds != 505 || nulled.PortalUsed {
		t.Fatalf("after expiry = %+v, want NULL corpse with cost 100/time 505/unused", nulled)
	}
	beforeChar := readRoot(t, f.q, f.victimID)

	res, err := f.st.CommitPortalOfLife(ctx, PortalOfLifeRequest{
		Character:    f.portalCharSnapshot(0),
		CorpseID:     corpseID,
		ProposedCost: 40,
	})
	if !errors.Is(err, ErrPortalCorpseMismatch) {
		t.Fatalf("err = %v, want ErrPortalCorpseMismatch", err)
	}
	if !isZeroPortalResult(res) {
		t.Fatalf("result = %+v, want zero", res)
	}
	if after := readRoot(t, f.q, f.victimID); !sameCharacter(after, beforeChar) {
		t.Fatalf("character moved: %+v vs %+v", after, beforeChar)
	}
	afterPending, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID)
	if err != nil || afterPending != nulled {
		t.Fatalf("pending = %+v, %v; want unchanged %+v", afterPending, err, nulled)
	}
	if got := f.staleCount("character"); got != 0 {
		t.Fatalf("character stale = %v, want 0", got)
	}
}

// TestCommitPortalOfLifeNoPending proves a Portal with no live
// pending row maps to ErrNoPendingDeath with the tentative
// character CAS rolled back.
func TestCommitPortalOfLifeNoPending(t *testing.T) {
	f := newPortalFixture(t)
	ctx := context.Background()
	beforeChar := readRoot(t, f.q, f.victimID)

	res, err := f.st.CommitPortalOfLife(ctx, PortalOfLifeRequest{
		Character:    f.portalCharSnapshot(0),
		CorpseID:     12345,
		ProposedCost: 40,
	})
	if !errors.Is(err, ErrNoPendingDeath) {
		t.Fatalf("err = %v, want ErrNoPendingDeath", err)
	}
	if errors.Is(err, ErrStaleRevision) {
		t.Fatalf("semantic rejection mapped to stale: %v", err)
	}
	if !isZeroPortalResult(res) {
		t.Fatalf("result = %+v, want zero", res)
	}
	if after := readRoot(t, f.q, f.victimID); !sameCharacter(after, beforeChar) {
		t.Fatalf("character moved: %+v vs %+v", after, beforeChar)
	}
	if _, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID); !isNoRows(err) {
		t.Fatalf("pending err = %v, want NoRows", err)
	}
	if got := f.staleCount("character"); got != 0 {
		t.Fatalf("character stale = %v, want 0", got)
	}
}

// TestCommitPortalOfLifeCharacterStale proves the character root
// comes FIRST: a stale revision maps to ErrStaleRevision, counts
// the character metric exactly once, and mutates no pending state.
func TestCommitPortalOfLifeCharacterStale(t *testing.T) {
	f := newPortalFixture(t)
	ctx := context.Background()
	corpseID := f.seedPortalPending(t, 100, 506, false)
	beforePending, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID)
	if err != nil {
		t.Fatal(err)
	}
	_ = corpseID

	res, err := f.st.CommitPortalOfLife(ctx, PortalOfLifeRequest{
		Character:    f.portalCharSnapshot(99),
		CorpseID:     corpseID,
		ProposedCost: 40,
	})
	if !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("err = %v, want ErrStaleRevision", err)
	}
	if !isZeroPortalResult(res) {
		t.Fatalf("result = %+v, want zero", res)
	}
	if got := f.staleCount("character"); got != 1 {
		t.Fatalf("character stale = %v, want 1", got)
	}
	afterPending, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID)
	if err != nil || afterPending != beforePending {
		t.Fatalf("pending = %+v, %v; want unchanged %+v", afterPending, err, beforePending)
	}
	if got := readRoot(t, f.q, f.victimID); got.Revision != 0 {
		t.Fatalf("character moved: %+v", got)
	}
}

// TestCommitPortalOfLifeTxAbort proves PostgreSQL rollback is the
// recovery: the real production tx-composition helper runs to
// completion, the transaction is then explicitly rolled back
// instead of committed, nothing durable survives, and the same
// request at the same expected revision then succeeds. (Explicit
// rollback after invoking the production seam stands in for backend
// termination; no compensation logic exists.)
func TestCommitPortalOfLifeTxAbort(t *testing.T) {
	f := newPortalFixture(t)
	ctx := context.Background()
	corpseID := f.seedPortalPending(t, 100, 507, false)
	req := PortalOfLifeRequest{
		Character:    f.portalCharSnapshot(0),
		CorpseID:     corpseID,
		ProposedCost: 40,
	}

	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	aborted, err := commitPortalOfLifeTx(ctx, tx, req)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("tx composition: %v", err)
	}
	if aborted.CharacterRevision != 1 || aborted.EffectiveCost != 40 {
		_ = tx.Rollback(ctx)
		t.Fatalf("aborted result = %+v, want rev 1 cost 40", aborted)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	if after := readRoot(t, f.q, f.victimID); after.Revision != 0 {
		t.Fatalf("character survived abort: %+v", after)
	}
	pending, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID)
	if err != nil || pending.EffectiveCost != 100 || pending.PortalUsed {
		t.Fatalf("pending = %+v, %v; want cost 100 unused after abort", pending, err)
	}
	if n := portalCount(t, f, `SELECT COUNT(*) FROM kills`); n != 0 {
		t.Fatalf("kills = %d, want 0", n)
	}
	if n := portalCount(t, f, `SELECT COUNT(*) FROM ledger`); n != 0 {
		t.Fatalf("ledger rows = %d, want 0", n)
	}

	res, err := f.st.CommitPortalOfLife(ctx, req)
	if err != nil {
		t.Fatalf("healthy retry: %v", err)
	}
	assertPortalSuccess(t, f, res, corpseID, 40, 507)
}

// TestCommitPortalOfLifeCommitAmbiguity proves the lost-ACK contract
// without an outbox: commit through the real private seam, discard
// the acknowledgement, retry with the OLD revision (stale, no second
// mutation), then reconcile to the committed revision and retry
// against the same corpse (already-used). Final durable state is
// exactly the first commit.
func TestCommitPortalOfLifeCommitAmbiguity(t *testing.T) {
	f := newPortalFixture(t)
	ctx := context.Background()
	corpseID := f.seedPortalPending(t, 100, 508, false)
	req := PortalOfLifeRequest{
		Character:    f.portalCharSnapshot(0),
		CorpseID:     corpseID,
		ProposedCost: 40,
	}

	// First commit through the production seam; the success
	// acknowledgement is then intentionally treated as lost.
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first, err := commitPortalOfLifeTx(ctx, tx, req)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("first composition: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	if first.CharacterRevision != 1 || first.EffectiveCost != 40 {
		t.Fatalf("first = %+v, want rev 1 cost 40", first)
	}

	// Retry with the OLD expected revision: stale, no second mutation.
	retryRes, err := f.st.CommitPortalOfLife(ctx, req)
	if !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("retry err = %v, want ErrStaleRevision", err)
	}
	if !isZeroPortalResult(retryRes) {
		t.Fatalf("retry result = %+v, want zero", retryRes)
	}
	assertPortalState := func(stage string) {
		t.Helper()
		if got := readRoot(t, f.q, f.victimID); got.Revision != 1 {
			t.Fatalf("%s: character = %+v, want rev 1", stage, got)
		}
		pending, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID)
		if err != nil || pending.EffectiveCost != 40 || !pending.PortalUsed ||
			pending.DeathTimeSeconds != 508 ||
			!pending.CorpseID.Valid || pending.CorpseID.Int64 != corpseID {
			t.Fatalf("%s: pending = %+v, %v", stage, pending, err)
		}
		if n := portalCount(t, f, `SELECT COUNT(*) FROM pending_deaths WHERE character_id = $1`, f.victimID); n != 1 {
			t.Fatalf("%s: pending rows = %d, want 1", stage, n)
		}
		if n := portalCount(t, f, `SELECT COUNT(*) FROM kills`); n != 0 {
			t.Fatalf("%s: kills = %d, want 0", stage, n)
		}
		if n := portalCount(t, f, `SELECT COUNT(*) FROM ledger`); n != 0 {
			t.Fatalf("%s: ledger = %d, want 0", stage, n)
		}
	}
	assertPortalState("after stale retry")

	// Reconciled retry at the committed revision against the same
	// corpse: already-used, rolled back.
	reconciled := PortalOfLifeRequest{
		Character:    f.portalCharSnapshot(1),
		CorpseID:     corpseID,
		ProposedCost: 40,
	}
	reconciledRes, err := f.st.CommitPortalOfLife(ctx, reconciled)
	if !errors.Is(err, ErrPortalAlreadyUsed) {
		t.Fatalf("reconciled err = %v, want ErrPortalAlreadyUsed", err)
	}
	if !isZeroPortalResult(reconciledRes) {
		t.Fatalf("reconciled result = %+v, want zero", reconciledRes)
	}
	assertPortalState("after reconciled retry")
}

// TestCommitPortalOfLifeValidation rejects every malformed shape
// before any PG mutation with the narrow stable error.
func TestCommitPortalOfLifeValidation(t *testing.T) {
	f := newPortalFixture(t)
	ctx := context.Background()
	corpseID := f.seedPortalPending(t, 100, 509, false)
	base := func() PortalOfLifeRequest {
		return PortalOfLifeRequest{
			Character:    f.portalCharSnapshot(0),
			CorpseID:     corpseID,
			ProposedCost: 40,
		}
	}
	cases := map[string]func(*PortalOfLifeRequest){
		"character id":       func(r *PortalOfLifeRequest) { r.Character.ID = 0 },
		"character revision": func(r *PortalOfLifeRequest) { r.Character.ExpectedRevision = -1 },
		"corpse zero":        func(r *PortalOfLifeRequest) { r.CorpseID = 0 },
		"corpse negative":    func(r *PortalOfLifeRequest) { r.CorpseID = -7 },
		"proposed below 5":   func(r *PortalOfLifeRequest) { r.ProposedCost = 4 },
		"proposed zero":      func(r *PortalOfLifeRequest) { r.ProposedCost = 0 },
		"proposed negative":  func(r *PortalOfLifeRequest) { r.ProposedCost = -1 },
		"proposed above 80":  func(r *PortalOfLifeRequest) { r.ProposedCost = 81 },
		"proposed 100":       func(r *PortalOfLifeRequest) { r.ProposedCost = 100 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			req := base()
			mutate(&req)
			res, err := f.st.CommitPortalOfLife(ctx, req)
			if !errors.Is(err, ErrInvalidPortalOfLife) {
				t.Fatalf("err = %v, want ErrInvalidPortalOfLife", err)
			}
			if !isZeroPortalResult(res) {
				t.Fatalf("result = %+v, want zero", res)
			}
		})
	}
	if got := readRoot(t, f.q, f.victimID); got.Revision != 0 {
		t.Fatalf("character moved by rejections: %+v", got)
	}
	pending, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID)
	if err != nil || pending.EffectiveCost != 100 || pending.PortalUsed {
		t.Fatalf("pending = %+v, %v; want cost 100 unused after rejections", pending, err)
	}
	if got := f.staleCount("character"); got != 0 {
		t.Fatalf("character stale = %v, want 0", got)
	}
}
