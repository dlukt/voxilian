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

// M5-T5b2b exactly-once Underworld-exit penalty consumption tests
// (spec §9.5.11a, §8.1/§8.3). All durable proofs run against real
// PostgreSQL 18. No migration beyond 6, no new SQL, no sim/gateway/
// proto change: composition uses the existing saveCharacterSnapshotTx
// seam plus GetPendingDeathByCharacter/DeletePendingDeathByCharacter.
// T5b2b writes ZERO ledger rows and ZERO kills rows. The verified
// cost is always the raw durable DeathPenaltyInput.PendingCost, never
// the scaled DeathPenaltyPlan.ScaledCost.

type penaltiesFixture struct {
	pool     *pgxpool.Pool
	q        *gen.Queries
	st       *PGStore
	victimID int64
}

func newPenaltiesFixture(t *testing.T) *penaltiesFixture {
	t.Helper()
	pool, q := openQueries(t)
	st := newTestStore(t, pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO spell_protos (id,school,level,mana,exertion,cast_ms,min_hp,outlaw,harmful,reagents,params,version) VALUES (71,1,1,1,1,0,1,false,false,'{}','{}',1), (72,1,1,1,1,0,1,false,false,'{}','{}',1) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO skill_protos (id,division,level,exertion,params,version) VALUES (71,1,1,1,'{}',1) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	acct, err := q.CreateAccount(ctx, gen.CreateAccountParams{KeycloakSub: "sub-death-penalties"})
	if err != nil {
		t.Fatal(err)
	}
	victim, err := createCharacter(ctx, q, validCharParams(acct.ID, 0, "PenaltyVictim"))
	if err != nil {
		t.Fatal(err)
	}
	return &penaltiesFixture{pool: pool, q: q, st: st, victimID: victim.ID}
}

// penaltySnapshot is a COMPLETE already-resolved post-penalty
// character snapshot with visibly changed vitals/flags/spells/skills,
// proving the Store persists exactly what the caller resolved.
func (f *penaltiesFixture) penaltySnapshot(rev int64) CharacterSnapshot {
	return CharacterSnapshot{
		ID: f.victimID, ExpectedRevision: rev, Karma: 777,
		PosX: 111, PosY: 222, PosZ: 333,
		Vitals:      json.RawMessage(`{"hp":12,"base_max":29,"max":29,"mana":7,"max_mana":30,"vigor":25,"threshold":55,"stomach":80,"exertion":0}`),
		Advancement: json.RawMessage(`{"pts":0}`),
		Flags:       21,
		Spells:      []CharacterSpellSnapshot{{SpellID: 71, Ability: 60}, {SpellID: 72, Ability: 6}},
		Skills:      []CharacterSkillSnapshot{{SkillID: 71, Ability: 50}},
	}
}

// seedPending inserts one corpse plus one pending row and returns the
// corpse ID.
func (f *penaltiesFixture) seedPending(t *testing.T, cost int16, deathTime int64, portalUsed bool) int64 {
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

func (f *penaltiesFixture) staleCount() float64 {
	return testutil.ToFloat64(f.st.stale.WithLabelValues("character"))
}

func isZeroPenaltiesResult(res DeathPenaltiesResult) bool {
	return res.CharacterRevision == 0
}

// spellSet collects the persisted spell child set keyed by spell ID.
func spellSet(t *testing.T, f *penaltiesFixture) map[int32]int16 {
	t.Helper()
	rows, err := f.q.ListCharacterSpells(context.Background(), f.victimID)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[int32]int16, len(rows))
	for _, r := range rows {
		out[r.SpellID] = r.Ability
	}
	return out
}

// skillSet collects the persisted skill child set keyed by skill ID.
func skillSet(t *testing.T, f *penaltiesFixture) map[int32]int16 {
	t.Helper()
	rows, err := f.q.ListCharacterSkills(context.Background(), f.victimID)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[int32]int16, len(rows))
	for _, r := range rows {
		out[r.SkillID] = r.Ability
	}
	return out
}

// assertPenaltiesSuccess checks the full durable effect of a
// successful consumption: character revision +1 with the complete
// caller-resolved snapshot persisted exactly, the pending row gone,
// and zero audit rows.
func assertPenaltiesSuccess(t *testing.T, f *penaltiesFixture, res DeathPenaltiesResult, wantRev int64, want CharacterSnapshot) {
	t.Helper()
	ctx := context.Background()
	if res.CharacterRevision != wantRev {
		t.Fatalf("result = %+v, want revision %d", res, wantRev)
	}
	got := readRoot(t, f.q, f.victimID)
	if got.Revision != wantRev || got.Karma != want.Karma || got.PosX != want.PosX ||
		got.PosY != want.PosY || got.PosZ != want.PosZ || got.Flags != want.Flags {
		t.Fatalf("character root = %+v, want post-penalty state rev %d", got, wantRev)
	}
	var gotVitals, wantVitals map[string]any
	if err := json.Unmarshal(got.Vitals, &gotVitals); err != nil {
		t.Fatalf("unmarshal persisted vitals %s: %v", got.Vitals, err)
	}
	if err := json.Unmarshal(want.Vitals, &wantVitals); err != nil {
		t.Fatalf("unmarshal want vitals %s: %v", want.Vitals, err)
	}
	if len(gotVitals) != len(wantVitals) {
		t.Fatalf("vitals = %s, want %s (PostgreSQL jsonb normalizes key order)", got.Vitals, want.Vitals)
	}
	for k, w := range wantVitals {
		if g, ok := gotVitals[k]; !ok || g != w {
			t.Fatalf("vitals = %s, want %s (PostgreSQL jsonb normalizes key order)", got.Vitals, want.Vitals)
		}
	}
	var gotAdv, wantAdv map[string]any
	if err := json.Unmarshal(got.Advancement, &gotAdv); err != nil {
		t.Fatalf("unmarshal persisted advancement %s: %v", got.Advancement, err)
	}
	if err := json.Unmarshal(want.Advancement, &wantAdv); err != nil {
		t.Fatalf("unmarshal want advancement %s: %v", want.Advancement, err)
	}
	if len(gotAdv) != len(wantAdv) {
		t.Fatalf("advancement = %s, want %s", got.Advancement, want.Advancement)
	}
	for k, w := range wantAdv {
		if g, ok := gotAdv[k]; !ok || g != w {
			t.Fatalf("advancement = %s, want %s", got.Advancement, want.Advancement)
		}
	}
	spells := spellSet(t, f)
	if len(spells) != len(want.Spells) {
		t.Fatalf("spells = %+v, want %d rows", spells, len(want.Spells))
	}
	for _, sp := range want.Spells {
		if spells[sp.SpellID] != sp.Ability {
			t.Fatalf("spells = %+v, want spell %d ability %d", spells, sp.SpellID, sp.Ability)
		}
	}
	skills := skillSet(t, f)
	if len(skills) != len(want.Skills) {
		t.Fatalf("skills = %+v, want %d rows", skills, len(want.Skills))
	}
	for _, sk := range want.Skills {
		if skills[sk.SkillID] != sk.Ability {
			t.Fatalf("skills = %+v, want skill %d ability %d", skills, sk.SkillID, sk.Ability)
		}
	}
	if _, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID); !isNoRows(err) {
		t.Fatalf("pending err = %v, want NoRows", err)
	}
	if n := countFor(t, f.pool, `SELECT COUNT(*) FROM pending_deaths WHERE character_id = $1`, f.victimID); n != 0 {
		t.Fatalf("pending rows = %d, want 0", n)
	}
	if n := countFor(t, f.pool, `SELECT COUNT(*) FROM kills`); n != 0 {
		t.Fatalf("kills = %d, want 0", n)
	}
	if n := countFor(t, f.pool, `SELECT COUNT(*) FROM ledger`); n != 0 {
		t.Fatalf("ledger rows = %d, want 0", n)
	}
}

// TestCommitDeathPenaltiesSuccess proves normal consumption: a nonzero
// pending cost plus a complete post-penalty snapshot commits rev 0->1
// with the exact root/children, deletes the pending row, and writes
// no ledger/kill rows.
func TestCommitDeathPenaltiesSuccess(t *testing.T) {
	f := newPenaltiesFixture(t)
	ctx := context.Background()
	f.seedPending(t, 60, 600, false)
	want := f.penaltySnapshot(0)

	res, err := f.st.CommitDeathPenalties(ctx, DeathPenaltiesRequest{
		Character:           want,
		ExpectedPendingCost: 60,
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	assertPenaltiesSuccess(t, f, res, 1, want)
}

// TestCommitDeathPenaltiesRawVsScaledSuccess is the mandatory newcomer
// regression: pending cost 90 with ExpectedPendingCost 90 succeeds
// even though the supplied snapshot already reflects T5a /3 scaling
// (ScaledCost 30 is a planning artifact, never the durable fence).
func TestCommitDeathPenaltiesRawVsScaledSuccess(t *testing.T) {
	f := newPenaltiesFixture(t)
	ctx := context.Background()
	f.seedPending(t, 90, 601, false)
	want := f.penaltySnapshot(0)

	res, err := f.st.CommitDeathPenalties(ctx, DeathPenaltiesRequest{
		Character:           want,
		ExpectedPendingCost: 90,
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	assertPenaltiesSuccess(t, f, res, 1, want)
}

// TestCommitDeathPenaltiesScaledCostRejected proves the companion
// half: ExpectedPendingCost 30 against a pending cost-90 row fails
// with ErrPendingDeathCostMismatch and rolls back every tentative
// root/child change.
func TestCommitDeathPenaltiesScaledCostRejected(t *testing.T) {
	f := newPenaltiesFixture(t)
	ctx := context.Background()
	f.seedPending(t, 90, 602, false)
	beforeChar := readRoot(t, f.q, f.victimID)
	beforePending, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID)
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.st.CommitDeathPenalties(ctx, DeathPenaltiesRequest{
		Character:           f.penaltySnapshot(0),
		ExpectedPendingCost: 30,
	})
	if !errors.Is(err, ErrPendingDeathCostMismatch) {
		t.Fatalf("err = %v, want ErrPendingDeathCostMismatch", err)
	}
	if errors.Is(err, ErrStaleRevision) {
		t.Fatalf("cost mismatch mapped to stale: %v", err)
	}
	if !isZeroPenaltiesResult(res) {
		t.Fatalf("result = %+v, want zero", res)
	}
	if after := readRoot(t, f.q, f.victimID); !sameCharacter(after, beforeChar) {
		t.Fatalf("character moved: %+v vs %+v", after, beforeChar)
	}
	if spells := spellSet(t, f); len(spells) != 0 {
		t.Fatalf("spells survived rollback: %+v", spells)
	}
	if skills := skillSet(t, f); len(skills) != 0 {
		t.Fatalf("skills survived rollback: %+v", skills)
	}
	afterPending, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID)
	if err != nil || afterPending != beforePending {
		t.Fatalf("pending = %+v, %v; want unchanged %+v", afterPending, err, beforePending)
	}
	if got := f.staleCount(); got != 0 {
		t.Fatalf("character stale = %v, want 0", got)
	}
}

// TestCommitDeathPenaltiesPortalUsedSuccess proves a portal-reduced
// row (cost 40, portal_used true) is completely valid for T5b2b: with
// the matching expected cost the transaction succeeds and consumes it.
func TestCommitDeathPenaltiesPortalUsedSuccess(t *testing.T) {
	f := newPenaltiesFixture(t)
	ctx := context.Background()
	f.seedPending(t, 40, 603, true)
	want := f.penaltySnapshot(0)

	res, err := f.st.CommitDeathPenalties(ctx, DeathPenaltiesRequest{
		Character:           want,
		ExpectedPendingCost: 40,
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	assertPenaltiesSuccess(t, f, res, 1, want)
}

// TestCommitDeathPenaltiesExpiredCorpseSuccess proves corpse expiry
// never prevents consumption: after DeleteCorpse NULLs the pending
// association (ON DELETE SET NULL), the correct expected cost still
// succeeds and consumes the row.
func TestCommitDeathPenaltiesExpiredCorpseSuccess(t *testing.T) {
	f := newPenaltiesFixture(t)
	ctx := context.Background()
	corpseID := f.seedPending(t, 40, 604, false)
	if err := f.q.DeleteCorpse(ctx, corpseID); err != nil {
		t.Fatalf("expire corpse: %v", err)
	}
	nulled, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID)
	if err != nil {
		t.Fatalf("get after expiry: %v", err)
	}
	if nulled.CorpseID.Valid || nulled.EffectiveCost != 40 ||
		nulled.DeathTimeSeconds != 604 || nulled.PortalUsed {
		t.Fatalf("after expiry = %+v, want NULL corpse with cost 40/time 604/unused", nulled)
	}
	want := f.penaltySnapshot(0)

	res, err := f.st.CommitDeathPenalties(ctx, DeathPenaltiesRequest{
		Character:           want,
		ExpectedPendingCost: 40,
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	assertPenaltiesSuccess(t, f, res, 1, want)
}

// TestCommitDeathPenaltiesCostZeroSuccess proves cheap-death cost 0 is
// still a pending lifecycle phase: the supplied snapshot persists and
// the pending row is deleted. There is no cost==0 skip.
func TestCommitDeathPenaltiesCostZeroSuccess(t *testing.T) {
	f := newPenaltiesFixture(t)
	ctx := context.Background()
	f.seedPending(t, 0, 605, false)
	want := f.penaltySnapshot(0)

	res, err := f.st.CommitDeathPenalties(ctx, DeathPenaltiesRequest{
		Character:           want,
		ExpectedPendingCost: 0,
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	assertPenaltiesSuccess(t, f, res, 1, want)
}

// TestCommitDeathPenaltiesNoPending proves consumption at a current
// character revision with no pending row maps to ErrNoPendingDeath
// (never ErrStaleRevision) with the full tentative character/children
// state rolled back and no stale-metric increment.
func TestCommitDeathPenaltiesNoPending(t *testing.T) {
	f := newPenaltiesFixture(t)
	ctx := context.Background()
	beforeChar := readRoot(t, f.q, f.victimID)

	res, err := f.st.CommitDeathPenalties(ctx, DeathPenaltiesRequest{
		Character:           f.penaltySnapshot(0),
		ExpectedPendingCost: 60,
	})
	if !errors.Is(err, ErrNoPendingDeath) {
		t.Fatalf("err = %v, want ErrNoPendingDeath", err)
	}
	if errors.Is(err, ErrStaleRevision) {
		t.Fatalf("absent pending mapped to stale: %v", err)
	}
	if !isZeroPenaltiesResult(res) {
		t.Fatalf("result = %+v, want zero", res)
	}
	if after := readRoot(t, f.q, f.victimID); !sameCharacter(after, beforeChar) {
		t.Fatalf("character moved: %+v vs %+v", after, beforeChar)
	}
	if spells := spellSet(t, f); len(spells) != 0 {
		t.Fatalf("spells survived rollback: %+v", spells)
	}
	if skills := skillSet(t, f); len(skills) != 0 {
		t.Fatalf("skills survived rollback: %+v", skills)
	}
	if _, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID); !isNoRows(err) {
		t.Fatalf("pending err = %v, want NoRows", err)
	}
	if got := f.staleCount(); got != 0 {
		t.Fatalf("character stale = %v, want 0", got)
	}
}

// TestCommitDeathPenaltiesCostMismatch proves a wrong expected cost
// maps to ErrPendingDeathCostMismatch (never ErrStaleRevision) with
// the pending row, the character root, and all children unchanged and
// no stale-metric increment.
func TestCommitDeathPenaltiesCostMismatch(t *testing.T) {
	f := newPenaltiesFixture(t)
	ctx := context.Background()
	f.seedPending(t, 55, 606, false)
	beforeChar := readRoot(t, f.q, f.victimID)
	beforePending, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID)
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.st.CommitDeathPenalties(ctx, DeathPenaltiesRequest{
		Character:           f.penaltySnapshot(0),
		ExpectedPendingCost: 54,
	})
	if !errors.Is(err, ErrPendingDeathCostMismatch) {
		t.Fatalf("err = %v, want ErrPendingDeathCostMismatch", err)
	}
	if errors.Is(err, ErrStaleRevision) {
		t.Fatalf("cost mismatch mapped to stale: %v", err)
	}
	if !isZeroPenaltiesResult(res) {
		t.Fatalf("result = %+v, want zero", res)
	}
	if after := readRoot(t, f.q, f.victimID); !sameCharacter(after, beforeChar) {
		t.Fatalf("character moved: %+v vs %+v", after, beforeChar)
	}
	if spells := spellSet(t, f); len(spells) != 0 {
		t.Fatalf("spells survived rollback: %+v", spells)
	}
	if skills := skillSet(t, f); len(skills) != 0 {
		t.Fatalf("skills survived rollback: %+v", skills)
	}
	afterPending, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID)
	if err != nil || afterPending != beforePending {
		t.Fatalf("pending = %+v, %v; want unchanged %+v", afterPending, err, beforePending)
	}
	if got := f.staleCount(); got != 0 {
		t.Fatalf("character stale = %v, want 0", got)
	}
	if n := countFor(t, f.pool, `SELECT COUNT(*) FROM kills`); n != 0 {
		t.Fatalf("kills = %d, want 0", n)
	}
	if n := countFor(t, f.pool, `SELECT COUNT(*) FROM ledger`); n != 0 {
		t.Fatalf("ledger rows = %d, want 0", n)
	}
}

// TestCommitDeathPenaltiesCharacterStale proves the character root
// comes FIRST: a wrong expected revision maps to ErrStaleRevision,
// counts the character metric exactly once, and mutates no pending
// state. This occurs before pending verification.
func TestCommitDeathPenaltiesCharacterStale(t *testing.T) {
	f := newPenaltiesFixture(t)
	ctx := context.Background()
	f.seedPending(t, 60, 607, false)
	beforePending, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID)
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.st.CommitDeathPenalties(ctx, DeathPenaltiesRequest{
		Character:           f.penaltySnapshot(99),
		ExpectedPendingCost: 60,
	})
	if !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("err = %v, want ErrStaleRevision", err)
	}
	if !isZeroPenaltiesResult(res) {
		t.Fatalf("result = %+v, want zero", res)
	}
	if got := f.staleCount(); got != 1 {
		t.Fatalf("character stale = %v, want 1", got)
	}
	afterPending, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID)
	if err != nil || afterPending != beforePending {
		t.Fatalf("pending = %+v, %v; want unchanged %+v", afterPending, err, beforePending)
	}
	if got := readRoot(t, f.q, f.victimID); got.Revision != 0 {
		t.Fatalf("character moved: %+v", got)
	}
	if spells := spellSet(t, f); len(spells) != 0 {
		t.Fatalf("spells survived stale rejection: %+v", spells)
	}
}

// TestCommitDeathPenaltiesTxAbort proves PostgreSQL rollback is the
// recovery: the real production tx-composition helper runs far enough
// that the character root CAS, spell/skill replacement, and pending
// deletion all happened, the transaction is then explicitly rolled
// back instead of committed, nothing durable survives, and the SAME
// request at the SAME expected revision then succeeds. (Explicit
// rollback after invoking the production seam stands in for backend
// termination; no compensation logic exists.)
func TestCommitDeathPenaltiesTxAbort(t *testing.T) {
	f := newPenaltiesFixture(t)
	ctx := context.Background()
	f.seedPending(t, 60, 608, false)
	req := DeathPenaltiesRequest{
		Character:           f.penaltySnapshot(0),
		ExpectedPendingCost: 60,
	}

	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	aborted, err := commitDeathPenaltiesTx(ctx, tx, req)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("tx composition: %v", err)
	}
	if aborted.CharacterRevision != 1 {
		_ = tx.Rollback(ctx)
		t.Fatalf("aborted result = %+v, want rev 1", aborted)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	if after := readRoot(t, f.q, f.victimID); after.Revision != 0 {
		t.Fatalf("character survived abort: %+v", after)
	}
	if spells := spellSet(t, f); len(spells) != 0 {
		t.Fatalf("spells survived abort: %+v", spells)
	}
	if skills := skillSet(t, f); len(skills) != 0 {
		t.Fatalf("skills survived abort: %+v", skills)
	}
	pending, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID)
	if err != nil || pending.EffectiveCost != 60 || pending.DeathTimeSeconds != 608 {
		t.Fatalf("pending = %+v, %v; want cost 60/time 608 after abort", pending, err)
	}
	if n := countFor(t, f.pool, `SELECT COUNT(*) FROM kills`); n != 0 {
		t.Fatalf("kills = %d, want 0", n)
	}
	if n := countFor(t, f.pool, `SELECT COUNT(*) FROM ledger`); n != 0 {
		t.Fatalf("ledger rows = %d, want 0", n)
	}

	res, err := f.st.CommitDeathPenalties(ctx, req)
	if err != nil {
		t.Fatalf("healthy retry: %v", err)
	}
	assertPenaltiesSuccess(t, f, res, 1, req.Character)
}

// TestCommitDeathPenaltiesCommitAmbiguity proves the lost-ACK contract
// without an outbox: commit through the real private seam, discard the
// acknowledgement, retry with the OLD revision (stale, no second
// mutation), then reconcile to the committed character revision and
// retry (no pending, tentative second snapshot rolled back). Final
// durable state is EXACTLY the first commit: revision advanced once,
// abilities changed once, pending absent, no ledger/kill duplication.
func TestCommitDeathPenaltiesCommitAmbiguity(t *testing.T) {
	f := newPenaltiesFixture(t)
	ctx := context.Background()
	f.seedPending(t, 60, 609, false)
	firstSnap := f.penaltySnapshot(0)
	req := DeathPenaltiesRequest{
		Character:           firstSnap,
		ExpectedPendingCost: 60,
	}

	// First commit through the production seam; the success
	// acknowledgement is then intentionally treated as lost.
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first, err := commitDeathPenaltiesTx(ctx, tx, req)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("first composition: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	if first.CharacterRevision != 1 {
		t.Fatalf("first = %+v, want rev 1", first)
	}

	// Retry with the OLD expected revision: stale, no duplicate effects.
	retryRes, err := f.st.CommitDeathPenalties(ctx, req)
	if !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("retry err = %v, want ErrStaleRevision", err)
	}
	if !isZeroPenaltiesResult(retryRes) {
		t.Fatalf("retry result = %+v, want zero", retryRes)
	}
	assertExactFirstCommit := func(stage string) {
		t.Helper()
		got := readRoot(t, f.q, f.victimID)
		if got.Revision != 1 || got.Karma != 777 || got.Flags != 21 {
			t.Fatalf("%s: character = %+v, want first commit rev 1", stage, got)
		}
		var gotV, wantV map[string]any
		if err := json.Unmarshal(got.Vitals, &gotV); err != nil {
			t.Fatalf("%s: unmarshal vitals %s: %v", stage, got.Vitals, err)
		}
		if err := json.Unmarshal(firstSnap.Vitals, &wantV); err != nil {
			t.Fatalf("%s: unmarshal want vitals: %v", stage, err)
		}
		if len(gotV) != len(wantV) {
			t.Fatalf("%s: vitals = %s, want first commit", stage, got.Vitals)
		}
		for k, w := range wantV {
			if g, ok := gotV[k]; !ok || g != w {
				t.Fatalf("%s: vitals = %s, want first commit", stage, got.Vitals)
			}
		}
		spells := spellSet(t, f)
		if len(spells) != 2 || spells[71] != 60 || spells[72] != 6 {
			t.Fatalf("%s: spells = %+v, want first commit set", stage, spells)
		}
		skills := skillSet(t, f)
		if len(skills) != 1 || skills[71] != 50 {
			t.Fatalf("%s: skills = %+v, want first commit set", stage, skills)
		}
		if _, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID); !isNoRows(err) {
			t.Fatalf("%s: pending err = %v, want NoRows", stage, err)
		}
		if n := countFor(t, f.pool, `SELECT COUNT(*) FROM pending_deaths WHERE character_id = $1`, f.victimID); n != 0 {
			t.Fatalf("%s: pending rows = %d, want 0", stage, n)
		}
		if n := countFor(t, f.pool, `SELECT COUNT(*) FROM kills`); n != 0 {
			t.Fatalf("%s: kills = %d, want 0", stage, n)
		}
		if n := countFor(t, f.pool, `SELECT COUNT(*) FROM ledger`); n != 0 {
			t.Fatalf("%s: ledger = %d, want 0", stage, n)
		}
	}
	assertExactFirstCommit("after stale retry")

	// Reconciled retry at the committed revision: the pending row is
	// gone, so ErrNoPendingDeath (never ErrStaleRevision), and the
	// tentative second snapshot rolls back.
	secondSnap := f.penaltySnapshot(1)
	secondSnap.Karma = 888
	secondSnap.Flags = 22
	reconciledRes, err := f.st.CommitDeathPenalties(ctx, DeathPenaltiesRequest{
		Character:           secondSnap,
		ExpectedPendingCost: 60,
	})
	if !errors.Is(err, ErrNoPendingDeath) {
		t.Fatalf("reconciled err = %v, want ErrNoPendingDeath", err)
	}
	if errors.Is(err, ErrStaleRevision) {
		t.Fatalf("reconciled rejection mapped to stale: %v", err)
	}
	if !isZeroPenaltiesResult(reconciledRes) {
		t.Fatalf("reconciled result = %+v, want zero", reconciledRes)
	}
	assertExactFirstCommit("after reconciled retry")
}

// TestCommitDeathPenaltiesValidation rejects every malformed shape
// before any PG mutation with the narrow stable error.
func TestCommitDeathPenaltiesValidation(t *testing.T) {
	f := newPenaltiesFixture(t)
	ctx := context.Background()
	f.seedPending(t, 50, 610, false)
	beforeChar := readRoot(t, f.q, f.victimID)
	beforePending, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID)
	if err != nil {
		t.Fatal(err)
	}
	base := func() DeathPenaltiesRequest {
		return DeathPenaltiesRequest{
			Character:           f.penaltySnapshot(0),
			ExpectedPendingCost: 50,
		}
	}
	cases := map[string]func(*DeathPenaltiesRequest){
		"character id zero":     func(r *DeathPenaltiesRequest) { r.Character.ID = 0 },
		"character id negative": func(r *DeathPenaltiesRequest) { r.Character.ID = -3 },
		"character revision":    func(r *DeathPenaltiesRequest) { r.Character.ExpectedRevision = -1 },
		"cost negative":         func(r *DeathPenaltiesRequest) { r.ExpectedPendingCost = -1 },
		"cost above 100":        func(r *DeathPenaltiesRequest) { r.ExpectedPendingCost = 101 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			req := base()
			mutate(&req)
			res, err := f.st.CommitDeathPenalties(ctx, req)
			if !errors.Is(err, ErrInvalidDeathPenalties) {
				t.Fatalf("err = %v, want ErrInvalidDeathPenalties", err)
			}
			if !isZeroPenaltiesResult(res) {
				t.Fatalf("result = %+v, want zero", res)
			}
		})
	}
	if after := readRoot(t, f.q, f.victimID); !sameCharacter(after, beforeChar) {
		t.Fatalf("character moved by rejections: %+v vs %+v", after, beforeChar)
	}
	afterPending, err := f.q.GetPendingDeathByCharacter(ctx, f.victimID)
	if err != nil || afterPending != beforePending {
		t.Fatalf("pending = %+v, %v; want unchanged %+v", afterPending, err, beforePending)
	}
	if got := f.staleCount(); got != 0 {
		t.Fatalf("character stale = %v, want 0", got)
	}
}
