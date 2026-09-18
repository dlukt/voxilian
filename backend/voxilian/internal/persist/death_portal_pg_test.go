package persist

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/store"
	"github.com/dlukt/voxilian/internal/store/gen"
	"github.com/dlukt/voxilian/internal/world"
)

// M5-T5c3d2b2 real-PostgreSQL 18 Portal proofs (spec §9.5.1k,
// frozen v0.3.55): normal Portal through the concrete d2a+d2b2
// flow, newer-MarkDirty survival with a later real flush, REAL
// lost acknowledgement (exactly-once transaction, in-callback
// read-only recovery, no replay, no ReconcileSaver/
// ResolveReconciled), REAL stale proof failure, and REAL corpse
// mismatch. Reuses the existing testcontainer harness (openPG,
// pgAccountChar, pgAbilityProtos); no second harness. The owner
// sink applies completions/aborts to a real sim engine
// owner-locally (its Run never starts, so worker delivery is the
// only accessor after orchestration returns).

// portalPGStore counts real Store calls and optionally discards a
// successful commit behind a synthetic error (lost-ack simulation:
// the transaction executed exactly once, the adapter observes only
// the error).
type portalPGStore struct {
	*store.PGStore
	commitCalls    int
	loadCalls      int
	discardSuccess bool
	synthErr       error
}

var _ PortalExecutionStore = (*portalPGStore)(nil)

func (s *portalPGStore) CommitPortalOfLife(
	ctx context.Context, req store.PortalOfLifeRequest,
) (store.PortalOfLifeResult, error) {
	s.commitCalls++
	if s.discardSuccess {
		if _, realErr := s.PGStore.CommitPortalOfLife(ctx, req); realErr != nil {
			return store.PortalOfLifeResult{}, realErr
		}
		return store.PortalOfLifeResult{}, s.synthErr
	}
	return s.PGStore.CommitPortalOfLife(ctx, req)
}

func (s *portalPGStore) LoadDeathCharacterRecovery(
	ctx context.Context, id int64,
) (store.DeathCharacterRecoverySnapshot, error) {
	s.loadCalls++
	return s.PGStore.LoadDeathCharacterRecovery(ctx, id)
}

// portalPGSink applies deliveries to a real sim engine owner-locally
// (no Run) and records every call.
type portalPGSink struct {
	e           *sim.Engine
	completions []sim.PortalOfLifeCompletion
	aborts      []sim.PortalAttemptToken
}

var _ PortalOwnerSink = (*portalPGSink)(nil)

func (f *portalPGSink) EnqueuePortalOfLifeCompletion(
	_ context.Context, c sim.PortalOfLifeCompletion,
) (sim.PortalCompletionDisposition, error) {
	f.completions = append(f.completions, c)
	return f.e.PlayerAcceptPortalOfLifeCompletion(c)
}

func (f *portalPGSink) EnqueuePortalOfLifeAbort(
	_ context.Context, tok sim.PortalAttemptToken,
) (sim.PortalAbortDisposition, error) {
	f.aborts = append(f.aborts, tok)
	return f.e.PlayerAbortPortalOfLifeAttempt(tok)
}

// portalPGSetup migrates a fresh disposable PG18 database and returns
// the pool, queries, wrapped counting store, a death-entry Saver,
// and the real character ID. No items: Portal persists the
// character root only.
func portalPGSetup(t *testing.T, sub, name string) (context.Context, *pgxpool.Pool, *gen.Queries, *portalPGStore, *sim.Saver, int64) {
	t.Helper()
	pool, q := openPG(t)
	ctx := context.Background()
	raw, err := store.New(pool, newPGRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	st := &portalPGStore{PGStore: raw}
	charID := pgAccountChar(t, q, sub, name)
	pgAbilityProtos(t, pool)
	s := mustSaverForPersist(t)
	if err := s.Track(sim.AggregateKey{Kind: sim.AggregateCharacter, ID: charID}, 0); err != nil {
		t.Fatal(err)
	}
	return ctx, pool, q, st, s, charID
}

// portalPGVitals is valid gameplay vitals shared by the death entry
// and the Portal capture.
func portalPGVitals(t *testing.T) sim.PlayerVitals {
	t.Helper()
	v, err := sim.NewPlayerVitals(25)
	if err != nil {
		t.Fatalf("NewPlayerVitals: %v", err)
	}
	return v
}

// portalPGDurable is catalog-valid durable state (spell IDs 1-2,
// skill ID 1 exist via pgAbilityProtos).
func portalPGDurable() sim.PlayerDurableState {
	return sim.PlayerDurableState{
		Karma:       150,
		Advancement: []byte(`{"adv_points":7,"gain_chance":-40,"custom":"keep"}`),
		Flags:       0x1274,
		Spells: []sim.PlayerAbilityState{
			{ID: 1, Ability: 50, AtrophyFlag: false},
			{ID: 2, Ability: 60, AtrophyFlag: true},
		},
		Skills: []sim.PlayerAbilityState{
			{ID: 1, Ability: 40, AtrophyFlag: false},
		},
	}
}

// portalPGDeathEntry commits a real environmental death entry (cost
// 40, death time 1000, no items, no kills row) through the given
// Saver, advancing the character 0 -> 1 with a real corpse + pending
// row. It returns the generated corpse ID.
func portalPGDeathEntry(
	t *testing.T, ctx context.Context, s *sim.Saver, st *portalPGStore, charID int64, vitals sim.PlayerVitals, durable sim.PlayerDurableState,
) int64 {
	t.Helper()
	vitalsJSON, err := json.Marshal(vitals)
	if err != nil {
		t.Fatalf("vitals marshal: %v", err)
	}
	spells := make([]store.CharacterSpellSnapshot, 0, len(durable.Spells))
	for _, sp := range durable.Spells {
		spells = append(spells, store.CharacterSpellSnapshot{SpellID: sp.ID, Ability: sp.Ability, AtrophyFlag: sp.AtrophyFlag})
	}
	skills := make([]store.CharacterSkillSnapshot, 0, len(durable.Skills))
	for _, sk := range durable.Skills {
		skills = append(skills, store.CharacterSkillSnapshot{SkillID: sk.ID, Ability: sk.Ability, AtrophyFlag: sk.AtrophyFlag})
	}
	res, err := CommitDeathEntry(ctx, s, st.PGStore, store.DeathEntryRequest{
		Character: store.CharacterSnapshot{
			ID: charID, Karma: durable.Karma,
			PosX: 1000, PosY: 0, PosZ: 1000,
			Vitals:      vitalsJSON,
			Advancement: append([]byte(nil), durable.Advancement...),
			Flags:       durable.Flags,
			Spells:      spells,
			Skills:      skills,
		},
		DeathPosX: 1000, DeathPosY: 0, DeathPosZ: 1000,
		EffectiveDeathCost: 40,
		DeathTimeSeconds:   1000,
		CorpseLifetime:     10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("CommitDeathEntry: %v", err)
	}
	if res.CharacterRevision != 1 || res.CorpseID <= 0 {
		t.Fatalf("death result = %+v, want rev1 + corpse", res)
	}
	return res.CorpseID
}

// portalPGEngine builds a real sim engine whose live player exactly
// matches the materialized recovery snapshot (position mm -> m,
// vitals JSON, durable content) and hydrates the authoritative
// pending. The Portal Saver is tracked at the recovered revision.
func portalPGEngine(
	t *testing.T, charID int64, rec store.DeathCharacterRecoverySnapshot,
) (*sim.Engine, sim.EntityID, *sim.Saver) {
	t.Helper()
	var vitals sim.PlayerVitals
	if err := json.Unmarshal(rec.Character.Vitals, &vitals); err != nil {
		t.Fatalf("vitals unmarshal: %v", err)
	}
	durable := sim.PlayerDurableState{
		Karma:       rec.Character.Karma,
		Advancement: append([]byte(nil), rec.Character.Advancement...),
		Flags:       rec.Character.Flags,
	}
	for _, sp := range rec.Character.Spells {
		durable.Spells = append(durable.Spells, sim.PlayerAbilityState{ID: sp.SpellID, Ability: sp.Ability, AtrophyFlag: sp.AtrophyFlag})
	}
	for _, sk := range rec.Character.Skills {
		durable.Skills = append(durable.Skills, sim.PlayerAbilityState{ID: sk.SkillID, Ability: sk.Ability, AtrophyFlag: sk.AtrophyFlag})
	}
	e := mustSimEngine(t)
	pos := world.Vec3{
		X: float64(rec.Character.PosX) / 1000,
		Y: float64(rec.Character.PosY) / 1000,
		Z: float64(rec.Character.PosZ) / 1000,
	}
	snap, err := e.AddPlayerEntityWithDurableState(sim.CharacterID(charID), pos, vitals, testDeathRuntimeInputs(t), durable)
	if err != nil {
		t.Fatalf("add player: %v", err)
	}
	pending, err := MapPendingDeathRecovery(sim.CharacterID(charID), rec.Pending)
	if err != nil {
		t.Fatalf("map pending: %v", err)
	}
	if err := e.PlayerInstallRecoveredPendingDeath(snap.ID, pending); err != nil {
		t.Fatalf("hydrate pending: %v", err)
	}
	s := mustSaverForPersist(t)
	if err := s.Track(sim.AggregateKey{Kind: sim.AggregateCharacter, ID: charID}, rec.Character.ExpectedRevision); err != nil {
		t.Fatal(err)
	}
	return e, snap.ID, s
}

// portalPGOrchestrate reserves, prepares (through the d2a owner),
// and activates one Portal attempt with the given age/power against
// the hydrated pending. NowSeconds = deathTime + age.
func portalPGOrchestrate(
	t *testing.T, ctx context.Context, ex *PortalExecutor, e *sim.Engine, id sim.EntityID,
	nowSeconds int64, power int, corpseID int64,
) (*PortalExecutionReservation, sim.PortalOfLifeOrchestrationResult) {
	t.Helper()
	_ = ctx
	r, err := ex.ReservePortalOfLife()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	res, err := e.PlayerOrchestratePortalOfLife(id, sim.PortalOfLifeResolvedInput{
		NowSeconds: nowSeconds, SpellPower: power, TargetCorpseID: corpseID,
	}, r)
	if err != nil {
		t.Fatalf("orchestrate: %v", err)
	}
	return r, res
}

func startPortalPGExecutor(
	t *testing.T, st *portalPGStore, s *sim.Saver, sink *portalPGSink,
) (*PortalExecutor, context.CancelFunc) {
	t.Helper()
	return startPortalExecutor(t, PortalExecutorConfig{
		Workers: 1, QueueCapacity: 4, Store: st, Saver: s, Sink: sink,
		RetryDelay: time.Millisecond, AbortTimeout: 5 * time.Second,
	})
}

// A. Normal Portal through the executor: real Store transaction
// exactly once, character revision +1, pending portal_used=true with
// the exact effective cost, owner completion Applied, Saver known
// advanced, no recovery path.
func TestPortalPGNormalThroughExecutor(t *testing.T) {
	ctx, _, _, st, deathSaver, charID := portalPGSetup(t, "sub-portal-a", "Portala")
	vitals := portalPGVitals(t)
	durable := portalPGDurable()
	corpseID := portalPGDeathEntry(t, ctx, deathSaver, st, charID, vitals, durable)
	rec, err := st.LoadDeathCharacterRecovery(ctx, charID)
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	st.loadCalls = 0
	e, id, saver := portalPGEngine(t, charID, rec)
	sink := &portalPGSink{e: e}
	ex, _ := startPortalPGExecutor(t, st, saver, sink)
	// Age 0, power 50: proposed = 40-(50+60) -> bound 5; expected = 5.
	r, res := portalPGOrchestrate(t, ctx, ex, e, id, 1000, 50, corpseID)
	if res.ExpectedEffectiveCost != 5 {
		t.Fatalf("expected = %d, want 5", res.ExpectedEffectiveCost)
	}
	ch, err := r.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	out := awaitPortalResult(t, ch)
	if out.Err != nil || out.Recovered {
		t.Fatalf("result = %+v, want success unrecovered", out)
	}
	if out.Delivery != sim.PortalCompletionApplied {
		t.Fatalf("delivery = %d, want Applied", uint8(out.Delivery))
	}
	if st.commitCalls != 1 {
		t.Fatalf("Store transactions = %d, want exactly 1", st.commitCalls)
	}
	if st.loadCalls != 0 {
		t.Fatalf("recovery loads = %d, want 0 (no recovery path)", st.loadCalls)
	}
	if got := inspectKnown(t, saver, sim.AggregateCharacter, charID); got.KnownRevision != 2 || got.Blocked {
		t.Fatalf("saver = %+v, want known2 clean", got)
	}
	after, err := st.LoadDeathCharacterRecovery(ctx, charID)
	if err != nil {
		t.Fatalf("post recovery: %v", err)
	}
	if after.Character.ExpectedRevision != 2 {
		t.Fatalf("DB revision = %d, want 2", after.Character.ExpectedRevision)
	}
	if after.Pending == nil || !after.Pending.PortalUsed || after.Pending.EffectiveCost != 5 {
		t.Fatalf("DB pending = %+v, want used/cost5", after.Pending)
	}
	if len(sink.completions) != 1 {
		t.Fatalf("completions = %d, want 1", len(sink.completions))
	}
	live, ok, err := e.PlayerPendingDeathOf(id)
	if err != nil || !ok || !live.PortalUsed || live.EffectiveCost != 5 {
		t.Fatalf("live pending = %+v,%v,%v; want used/cost5", live, ok, err)
	}
}

// markDirtyBetweenPrepare marks a newer real character snapshot
// between Prepare and Activate (owner turn, gate already held).
type markDirtyBetweenPrepare struct {
	*PortalExecutionReservation
	mark func()
}

func (m *markDirtyBetweenPrepare) PreparePortalOfLifeWork(c sim.PortalOfLifeCapture) error {
	return m.PortalExecutionReservation.PreparePortalOfLifeWork(c)
}

func (m *markDirtyBetweenPrepare) ActivatePortalOfLifeWork() error {
	m.mark()
	return m.PortalExecutionReservation.ActivatePortalOfLifeWork()
}

func (m *markDirtyBetweenPrepare) CancelPortalOfLifeWork() {
	m.PortalExecutionReservation.CancelPortalOfLifeWork()
}

// B. Newer MarkDirty survives a REAL Portal: Prepare reserves the
// slot, a newer snapshot is marked dirty, Activate commits. After
// success the Saver stays Dirty at the Portal revision; a later
// flush through the real snapshot adapter advances one further
// revision with the newer content winning while pending stays
// portal_used with the exact cost.
func TestPortalPGNewerMarkDirtySurvives(t *testing.T) {
	ctx, _, _, st, deathSaver, charID := portalPGSetup(t, "sub-portal-b", "Portalb")
	vitals := portalPGVitals(t)
	durable := portalPGDurable()
	corpseID := portalPGDeathEntry(t, ctx, deathSaver, st, charID, vitals, durable)
	rec, err := st.LoadDeathCharacterRecovery(ctx, charID)
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	st.loadCalls = 0
	st.commitCalls = 0
	e, id, saver := portalPGEngine(t, charID, rec)
	sink := &portalPGSink{e: e}
	ex, _ := startPortalPGExecutor(t, st, saver, sink)

	r, err := ex.ReservePortalOfLife()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	newerVitals, err := json.Marshal(vitals)
	if err != nil {
		t.Fatalf("vitals marshal: %v", err)
	}
	newerSpells := []store.CharacterSpellSnapshot{{SpellID: 1, Ability: 50}, {SpellID: 2, Ability: 60, AtrophyFlag: true}}
	newerSkills := []store.CharacterSkillSnapshot{{SkillID: 1, Ability: 40}}
	mark := func() {
		job, jerr := NewCharacterSnapshotJob(st.PGStore, store.CharacterSnapshot{
			ID: charID, Karma: 999,
			PosX: 1000, PosY: 0, PosZ: 1000,
			Vitals:      append([]byte(nil), newerVitals...),
			Advancement: []byte(`{"adv_points":7,"gain_chance":-40,"custom":"keep"}`),
			Flags:       0x1274,
			Spells:      newerSpells,
			Skills:      newerSkills,
		})
		if jerr != nil {
			t.Errorf("snapshot job: %v", jerr)
			return
		}
		if merr := saver.MarkDirty(sim.AggregateKey{Kind: sim.AggregateCharacter, ID: charID}, job.Write); merr != nil {
			t.Errorf("MarkDirty: %v", merr)
		}
	}
	wrapped := &markDirtyBetweenPrepare{PortalExecutionReservation: r, mark: mark}
	res, err := e.PlayerOrchestratePortalOfLife(id, sim.PortalOfLifeResolvedInput{
		NowSeconds: 1000, SpellPower: 50, TargetCorpseID: corpseID,
	}, wrapped)
	if err != nil {
		t.Fatalf("orchestrate: %v", err)
	}
	if res.ExpectedEffectiveCost != 5 {
		t.Fatalf("expected = %d, want 5", res.ExpectedEffectiveCost)
	}
	ch, _ := r.Result()
	out := awaitPortalResult(t, ch)
	if out.Err != nil {
		t.Fatalf("result err = %v", out.Err)
	}
	got := inspectKnown(t, saver, sim.AggregateCharacter, charID)
	if got.KnownRevision != 2 || got.Blocked {
		t.Fatalf("saver = %+v, want known2 clean", got)
	}
	if !got.Dirty {
		t.Fatal("newer MarkDirty superseded: want Dirty preserved after Portal success")
	}
	// Flush the newer snapshot through the real adapter: it must use
	// the Portal revision as expected, advance one further revision,
	// and let the newer content win while pending stays intact.
	if err := saver.FlushDirty(ctx); err != nil {
		t.Fatalf("FlushDirty: %v", err)
	}
	after, err := st.LoadDeathCharacterRecovery(ctx, charID)
	if err != nil {
		t.Fatalf("post recovery: %v", err)
	}
	if after.Character.ExpectedRevision != 3 {
		t.Fatalf("DB revision = %d, want 3 (Portal 2 + flush 3)", after.Character.ExpectedRevision)
	}
	if after.Character.Karma != 999 {
		t.Fatalf("DB karma = %d, want newer 999", after.Character.Karma)
	}
	if after.Pending == nil || !after.Pending.PortalUsed || after.Pending.EffectiveCost != 5 {
		t.Fatalf("DB pending = %+v, want used/cost5 intact", after.Pending)
	}
	if got := inspectKnown(t, saver, sim.AggregateCharacter, charID); got.KnownRevision != 3 || got.Dirty {
		t.Fatalf("saver = %+v, want known3 clean", got)
	}
}

// C. REAL lost acknowledgement: the wrapper calls the real
// CommitPortalOfLife successfully, discards success, and returns a
// synthetic error. The transaction executed exactly once, the real
// in-callback recovery proves revision E+1 with exact content and
// pending, Saver accepts E+1 normally (no block), the newer
// MarkDirty survives, the owner completion is delivered, and
// Recovered=true — with NO Store replay, NO ReconcileSaver, NO
// ResolveReconciled.
func TestPortalPGRealLostAck(t *testing.T) {
	ctx, _, _, st, deathSaver, charID := portalPGSetup(t, "sub-portal-c", "Portalc")
	vitals := portalPGVitals(t)
	durable := portalPGDurable()
	corpseID := portalPGDeathEntry(t, ctx, deathSaver, st, charID, vitals, durable)
	rec, err := st.LoadDeathCharacterRecovery(ctx, charID)
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	st.loadCalls = 0
	st.commitCalls = 0
	st.discardSuccess = true
	st.synthErr = errors.New("test: portal ack lost")
	e, id, saver := portalPGEngine(t, charID, rec)
	sink := &portalPGSink{e: e}
	ex, _ := startPortalPGExecutor(t, st, saver, sink)

	r, err := ex.ReservePortalOfLife()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	newerVitals, err := json.Marshal(vitals)
	if err != nil {
		t.Fatalf("vitals marshal: %v", err)
	}
	mark := func() {
		job, jerr := NewCharacterSnapshotJob(st.PGStore, store.CharacterSnapshot{
			ID: charID, Karma: 777,
			PosX: 1000, PosY: 0, PosZ: 1000,
			Vitals:      append([]byte(nil), newerVitals...),
			Advancement: []byte(`{"adv_points":7,"gain_chance":-40,"custom":"keep"}`),
			Flags:       0x1274,
			Spells:      []store.CharacterSpellSnapshot{{SpellID: 1, Ability: 50}, {SpellID: 2, Ability: 60, AtrophyFlag: true}},
			Skills:      []store.CharacterSkillSnapshot{{SkillID: 1, Ability: 40}},
		})
		if jerr != nil {
			t.Errorf("snapshot job: %v", jerr)
			return
		}
		if merr := saver.MarkDirty(sim.AggregateKey{Kind: sim.AggregateCharacter, ID: charID}, job.Write); merr != nil {
			t.Errorf("MarkDirty: %v", merr)
		}
	}
	wrapped := &markDirtyBetweenPrepare{PortalExecutionReservation: r, mark: mark}
	if _, err := e.PlayerOrchestratePortalOfLife(id, sim.PortalOfLifeResolvedInput{
		NowSeconds: 1000, SpellPower: 50, TargetCorpseID: corpseID,
	}, wrapped); err != nil {
		t.Fatalf("orchestrate: %v", err)
	}
	ch, _ := r.Result()
	out := awaitPortalResult(t, ch)
	if out.Err != nil {
		t.Fatalf("result err = %v, want proven success", out.Err)
	}
	if !out.Recovered {
		t.Fatal("Recovered = false, want true (lost ack)")
	}
	if st.commitCalls != 1 {
		t.Fatalf("Store transactions = %d, want exactly 1 (no replay)", st.commitCalls)
	}
	if st.loadCalls != 1 {
		t.Fatalf("recovery loads = %d, want 1 (in-callback read-only)", st.loadCalls)
	}
	got := inspectKnown(t, saver, sim.AggregateCharacter, charID)
	if got.KnownRevision != 2 || got.Blocked {
		t.Fatalf("saver = %+v, want known2 clean (normal E+1 acceptance, no block)", got)
	}
	if !got.Dirty {
		t.Fatal("newer MarkDirty lost: proven path must preserve gen > reservedGeneration (no ResolveReconciled)")
	}
	after, err := st.LoadDeathCharacterRecovery(ctx, charID)
	if err != nil {
		t.Fatalf("post recovery: %v", err)
	}
	if after.Character.ExpectedRevision != 2 || after.Pending == nil || !after.Pending.PortalUsed || after.Pending.EffectiveCost != 5 {
		t.Fatalf("DB = rev%d pending%+v, want rev2 used/cost5", after.Character.ExpectedRevision, after.Pending)
	}
	if out.Delivery != sim.PortalCompletionApplied || len(sink.completions) != 1 {
		t.Fatalf("delivery = %d completions=%d, want Applied/1", uint8(out.Delivery), len(sink.completions))
	}
}

// D. REAL stale/unproven: the DB character root advances outside the
// Saver before Portal execution. The Store call happens once, the
// transaction does not apply the Portal, read-only recovery runs,
// the proof fails, Saver blocks, and there is no completion, no
// abort, and no replay. The pending row stays unused with its cost.
func TestPortalPGRealStaleUnproven(t *testing.T) {
	ctx, _, _, st, deathSaver, charID := portalPGSetup(t, "sub-portal-d", "Portald")
	vitals := portalPGVitals(t)
	durable := portalPGDurable()
	corpseID := portalPGDeathEntry(t, ctx, deathSaver, st, charID, vitals, durable)
	rec, err := st.LoadDeathCharacterRecovery(ctx, charID)
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	// External advance outside the Saver: same shape, hostile karma,
	// so the materialized root sits at E+1 with mismatched content.
	bump := rec.Character
	bump.ExpectedRevision = 1
	bump.Karma = 424242
	if _, err := st.SaveCharacterSnapshot(ctx, bump); err != nil {
		t.Fatalf("external bump: %v", err)
	}
	st.loadCalls = 0
	st.commitCalls = 0
	e, id, saver := portalPGEngine(t, charID, rec)
	sink := &portalPGSink{e: e}
	ex, _ := startPortalPGExecutor(t, st, saver, sink)
	r, res := portalPGOrchestrate(t, ctx, ex, e, id, 1000, 50, corpseID)
	_ = res
	ch, _ := r.Result()
	out := awaitPortalResult(t, ch)
	if out.Err == nil || !errors.Is(out.Err, ErrPortalCommitUnproven) {
		t.Fatalf("err = %v, want ErrPortalCommitUnproven", out.Err)
	}
	if st.commitCalls != 1 {
		t.Fatalf("Store calls = %d, want 1 (no replay)", st.commitCalls)
	}
	if got := inspectKnown(t, saver, sim.AggregateCharacter, charID); !got.Blocked {
		t.Fatalf("saver = %+v, want blocked", got)
	}
	if len(sink.completions) != 0 || len(sink.aborts) != 0 {
		t.Fatalf("sink completions=%d aborts=%d, want 0/0 (fail-closed, never abort Store-crossed)",
			len(sink.completions), len(sink.aborts))
	}
	after, err := st.LoadDeathCharacterRecovery(ctx, charID)
	if err != nil {
		t.Fatalf("post recovery: %v", err)
	}
	if after.Pending == nil || after.Pending.PortalUsed || after.Pending.EffectiveCost != 40 {
		t.Fatalf("DB pending = %+v, want unused/cost40", after.Pending)
	}
}

// deleteCorpseBetweenPrepare expires the corpse between owner Prepare
// and Store execution (ON DELETE SET NULL clears the pending row's
// corpse association).
type deleteCorpseBetweenPrepare struct {
	*PortalExecutionReservation
	delete func()
}

func (d *deleteCorpseBetweenPrepare) PreparePortalOfLifeWork(c sim.PortalOfLifeCapture) error {
	return d.PortalExecutionReservation.PreparePortalOfLifeWork(c)
}

func (d *deleteCorpseBetweenPrepare) ActivatePortalOfLifeWork() error {
	d.delete()
	return d.PortalExecutionReservation.ActivatePortalOfLifeWork()
}

func (d *deleteCorpseBetweenPrepare) CancelPortalOfLifeWork() {
	d.PortalExecutionReservation.CancelPortalOfLifeWork()
}

// E. REAL corpse mismatch: the corpse is deleted between owner
// Prepare and Store execution. Fail closed: Store semantic error,
// recovery unproven, Saver blocked, no completion, no abort, no
// replay.
func TestPortalPGRealCorpseMismatch(t *testing.T) {
	ctx, pool, _, st, deathSaver, charID := portalPGSetup(t, "sub-portal-e", "Portale")
	vitals := portalPGVitals(t)
	durable := portalPGDurable()
	corpseID := portalPGDeathEntry(t, ctx, deathSaver, st, charID, vitals, durable)
	rec, err := st.LoadDeathCharacterRecovery(ctx, charID)
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	st.loadCalls = 0
	st.commitCalls = 0
	e, id, saver := portalPGEngine(t, charID, rec)
	sink := &portalPGSink{e: e}
	ex, _ := startPortalPGExecutor(t, st, saver, sink)
	r, err := ex.ReservePortalOfLife()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	wrapped := &deleteCorpseBetweenPrepare{PortalExecutionReservation: r, delete: func() {
		if _, derr := pool.Exec(ctx, `DELETE FROM corpses WHERE id = $1`, corpseID); derr != nil {
			t.Errorf("delete corpse: %v", derr)
		}
	}}
	if _, err := e.PlayerOrchestratePortalOfLife(id, sim.PortalOfLifeResolvedInput{
		NowSeconds: 1000, SpellPower: 50, TargetCorpseID: corpseID,
	}, wrapped); err != nil {
		t.Fatalf("orchestrate: %v", err)
	}
	ch, _ := r.Result()
	out := awaitPortalResult(t, ch)
	if out.Err == nil {
		t.Fatal("err = nil, want fail-closed corpse mismatch")
	}
	if !errors.Is(out.Err, ErrPortalCommitUnproven) {
		t.Fatalf("err = %v, want ErrPortalCommitUnproven", out.Err)
	}
	if !errors.Is(out.Err, store.ErrPortalCorpseMismatch) {
		t.Fatalf("err = %v, want Store corpse-mismatch cause preserved", out.Err)
	}
	if st.commitCalls != 1 {
		t.Fatalf("Store calls = %d, want 1 (no replay)", st.commitCalls)
	}
	if got := inspectKnown(t, saver, sim.AggregateCharacter, charID); !got.Blocked {
		t.Fatalf("saver = %+v, want blocked", got)
	}
	if len(sink.completions) != 0 || len(sink.aborts) != 0 {
		t.Fatalf("sink completions=%d aborts=%d, want 0/0", len(sink.completions), len(sink.aborts))
	}
	after, err := st.LoadDeathCharacterRecovery(ctx, charID)
	if err != nil {
		t.Fatalf("post recovery: %v", err)
	}
	if after.Pending == nil || after.Pending.PortalUsed || after.Pending.CorpseID != nil {
		t.Fatalf("DB pending = %+v, want unused with nil corpse (expired)", after.Pending)
	}
}
