package persist

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/store"
	"github.com/dlukt/voxilian/internal/store/gen"
	"github.com/dlukt/voxilian/internal/world"
)

// M5-T5c3d3b real-PostgreSQL 18 penalty proofs (spec §9.5.1k,
// frozen v0.3.57): normal penalty through the concrete d3a+d3b
// flow (raw pending-cost verification, revision +1, pending
// deletion, owner completion), raw-vs-scaled cost mismatch
// rejection, REAL lost acknowledgement (exactly-once
// transaction, in-callback read-only recovery, no replay, no
// ReconcileSaver/ResolveReconciled), REAL unproven pending-row
// proof failure, and REAL stale proof failure. Reuses the
// existing testcontainer harness (openPG, pgAccountChar,
// pgAbilityProtos); no second harness. The owner sink applies
// completions/retryables to a real sim engine owner-locally
// (its Run never starts, so worker delivery is the only
// accessor after orchestration returns).

// penaltyPGStore counts real Store calls, records the last
// penalty request, and optionally discards a successful commit
// behind a synthetic error (lost-ack simulation: the
// transaction executed exactly once, the adapter observes only
// the error) or fails without touching PG (failErr).
type penaltyPGStore struct {
	*store.PGStore
	commitCalls    int
	loadCalls      int
	lastReq        store.DeathPenaltiesRequest
	discardSuccess bool
	synthErr       error
	failErr        error
}

var _ PenaltyExecutionStore = (*penaltyPGStore)(nil)

func (s *penaltyPGStore) CommitDeathPenalties(
	ctx context.Context, req store.DeathPenaltiesRequest,
) (store.DeathPenaltiesResult, error) {
	s.commitCalls++
	s.lastReq = req
	if s.failErr != nil {
		return store.DeathPenaltiesResult{}, s.failErr
	}
	if s.discardSuccess {
		if _, realErr := s.PGStore.CommitDeathPenalties(ctx, req); realErr != nil {
			return store.DeathPenaltiesResult{}, realErr
		}
		return store.DeathPenaltiesResult{}, s.synthErr
	}
	return s.PGStore.CommitDeathPenalties(ctx, req)
}

func (s *penaltyPGStore) LoadDeathCharacterRecovery(
	ctx context.Context, id int64,
) (store.DeathCharacterRecoverySnapshot, error) {
	s.loadCalls++
	return s.PGStore.LoadDeathCharacterRecovery(ctx, id)
}

// penaltyPGSink records every delivery without touching the
// sim engine from worker goroutines: the frozen d3a owner
// writes persistenceActive AFTER Activate, so a worker-side
// owner-local apply would race the orchestrating test
// goroutine by design (production delivery runs through the
// owner mailbox instead). Tests apply recorded completions
// owner-locally after awaiting the bounded result, which the
// result channel orders after all worker delivery.
type penaltyPGSink struct {
	mu          sync.Mutex
	completions []sim.DeathPenaltyCompletion
	retryables  []sim.DeathPenaltyAttemptToken
}

var _ PenaltyOwnerSink = (*penaltyPGSink)(nil)

func (f *penaltyPGSink) EnqueueDeathPenaltyCompletion(
	_ context.Context, c sim.DeathPenaltyCompletion,
) (sim.DeathPenaltyCompletionDisposition, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completions = append(f.completions, c)
	return sim.DeathPenaltyCompletionApplied, nil
}

func (f *penaltyPGSink) EnqueueDeathPenaltyPersistenceRetryable(
	_ context.Context, tok sim.DeathPenaltyAttemptToken,
) (sim.DeathPenaltyRetryDisposition, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retryables = append(f.retryables, tok)
	return sim.DeathPenaltyRetryApplied, nil
}

func (f *penaltyPGSink) snapshot() (completions []sim.DeathPenaltyCompletion, retryables []sim.DeathPenaltyAttemptToken) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sim.DeathPenaltyCompletion(nil), f.completions...),
		append([]sim.DeathPenaltyAttemptToken(nil), f.retryables...)
}

// penaltyPGSetup migrates a fresh disposable PG18 database and
// returns the pool, queries, wrapped counting store, a
// death-entry Saver, and the real character ID. No items:
// penalty persists the character root only.
func penaltyPGSetup(t *testing.T, sub, name string) (context.Context, *pgxpool.Pool, *gen.Queries, *penaltyPGStore, *sim.Saver, int64) {
	t.Helper()
	pool, q := openPG(t)
	ctx := context.Background()
	raw, err := store.New(pool, newPGRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	st := &penaltyPGStore{PGStore: raw}
	charID := pgAccountChar(t, q, sub, name)
	pgAbilityProtos(t, pool)
	s := mustSaverForPersist(t)
	if err := s.Track(sim.AggregateKey{Kind: sim.AggregateCharacter, ID: charID}, 0); err != nil {
		t.Fatal(err)
	}
	return ctx, pool, q, st, s, charID
}

// penaltyPGVitals is valid gameplay vitals shared by the death
// entry and the penalty capture.
func penaltyPGVitals(t *testing.T) sim.PlayerVitals {
	t.Helper()
	v, err := sim.NewPlayerVitals(25)
	if err != nil {
		t.Fatalf("NewPlayerVitals: %v", err)
	}
	return v
}

// penaltyPGDurable is catalog-valid durable state (spell IDs
// 1-2, skill ID 1 exist via pgAbilityProtos). Flags carry
// neither MURDERER nor TUTORIAL, so the planner scales cost/3.
func penaltyPGDurable() sim.PlayerDurableState {
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

// penaltyPGDeathEntry commits a real environmental death entry
// (raw cost 90, death time 1000, no items, no kills row)
// through the given Saver, advancing the character 0 -> 1 with
// a real corpse + pending row.
func penaltyPGDeathEntry(
	t *testing.T, ctx context.Context, s *sim.Saver, st *penaltyPGStore, charID int64, vitals sim.PlayerVitals, durable sim.PlayerDurableState,
) {
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
		EffectiveDeathCost: 90,
		DeathTimeSeconds:   1000,
		CorpseLifetime:     10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("CommitDeathEntry: %v", err)
	}
	if res.CharacterRevision != 1 {
		t.Fatalf("death result = %+v, want rev1", res)
	}
}

// penaltyPGEngine builds a real sim engine whose live player
// exactly matches the materialized recovery snapshot (position
// mm -> m, vitals JSON, durable content) and hydrates the
// authoritative pending. The penalty Saver is tracked at the
// recovered revision.
func penaltyPGEngine(
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

func startPenaltyPGExecutor(
	t *testing.T, st *penaltyPGStore, s *sim.Saver, sink *penaltyPGSink,
) (*PenaltyExecutor, context.CancelFunc) {
	t.Helper()
	return startPenaltyExecutor(t, PenaltyExecutorConfig{
		Workers: 1, QueueCapacity: 4, Store: st, Saver: s, Sink: sink,
		RetryDelay: time.Millisecond, RetryTimeout: 5 * time.Second,
	})
}

// 1. Normal penalty through the executor: real Store
// transaction exactly once, raw pending cost 90 verified (not
// the scaled 30), character revision +1 with the complete
// post-penalty content, pending_deaths row deleted, owner
// completion Applied, Saver known advanced, no recovery path.
func TestPenaltyPGNormalThroughExecutor(t *testing.T) {
	ctx, _, _, st, deathSaver, charID := penaltyPGSetup(t, "sub-penalty-a", "Penalya")
	vitals := penaltyPGVitals(t)
	durable := penaltyPGDurable()
	penaltyPGDeathEntry(t, ctx, deathSaver, st, charID, vitals, durable)
	rec, err := st.LoadDeathCharacterRecovery(ctx, charID)
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if rec.Pending == nil || rec.Pending.EffectiveCost != 90 {
		t.Fatalf("pre pending = %+v, want cost90", rec.Pending)
	}
	st.loadCalls = 0
	st.commitCalls = 0
	e, id, saver := penaltyPGEngine(t, charID, rec)
	sink := &penaltyPGSink{}
	ex, _ := startPenaltyPGExecutor(t, st, saver, sink)
	prov := &penaltyRecordProvider{ex: ex}
	res, err := e.PlayerOrchestrateDeathPenalties(id, sim.UnderworldExitResolvedInput{DefaultDeathCost: 100}, &captureTestRNG{}, prov)
	if err != nil {
		t.Fatalf("orchestrate: %v", err)
	}
	ch, err := prov.last(t).Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	out := awaitPenaltyResult(t, ch)
	if out.Err != nil || out.Recovered {
		t.Fatalf("result = %+v, want success unrecovered", out)
	}
	if out.Delivery != sim.DeathPenaltyCompletionApplied {
		t.Fatalf("delivery = %d, want Applied", uint8(out.Delivery))
	}
	if st.commitCalls != 1 {
		t.Fatalf("Store transactions = %d, want exactly 1", st.commitCalls)
	}
	if st.loadCalls != 0 {
		t.Fatalf("recovery loads = %d, want 0 (no recovery path)", st.loadCalls)
	}
	// The transaction verified the RAW durable cost: the
	// newbie-scaled plan cost (90/3 = 30) would have failed
	// with ErrPendingDeathCostMismatch.
	if st.lastReq.ExpectedPendingCost != 90 {
		t.Fatalf("Store ExpectedPendingCost = %d, want raw 90", st.lastReq.ExpectedPendingCost)
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
	if after.Pending != nil {
		t.Fatalf("DB pending = %+v, want nil (exactly-once deletion)", after.Pending)
	}
	// The complete post-penalty Character persisted exactly:
	// the DB content proves the frozen intended request.
	intended, err := MapDeathPenaltyCapture(prov.last(t).capture)
	if err != nil {
		t.Fatalf("intended map: %v", err)
	}
	if err := equalDeathCharacterContent(intended.Character, after.Character); err != nil {
		t.Fatalf("DB character content: %v", err)
	}
	completions, retryables := sink.snapshot()
	if len(completions) != 1 {
		t.Fatalf("completions = %d, want 1", len(completions))
	}
	if completions[0].Token != res.Token {
		t.Fatalf("completion token = %+v, want %+v", completions[0].Token, res.Token)
	}
	if len(retryables) != 0 {
		t.Fatalf("retryables = %d, want 0", len(retryables))
	}
	// The delivered completion is genuinely consumable by the
	// d3a owner: Applied with pending cleared, back to Alive.
	if disp, err := e.PlayerAcceptDeathPenaltyCompletion(completions[0]); err != nil || disp != sim.DeathPenaltyCompletionApplied {
		t.Fatalf("owner apply = %d,%v; want Applied,nil", disp, err)
	}
	if _, ok, err := e.PlayerPendingDeathOf(id); err != nil || ok {
		t.Fatalf("live pending = %v,%v; want cleared", ok, err)
	}
}

// 2. Raw pending cost: a scaled plan cost (30) presented to the
// transaction is rejected with ErrPendingDeathCostMismatch and
// rolls back the tentative character snapshot (revision and
// pending row unchanged).
func TestPenaltyPGRawCostMismatch(t *testing.T) {
	ctx, _, _, st, deathSaver, charID := penaltyPGSetup(t, "sub-penalty-b", "Penaltyb")
	vitals := penaltyPGVitals(t)
	durable := penaltyPGDurable()
	penaltyPGDeathEntry(t, ctx, deathSaver, st, charID, vitals, durable)
	rec, err := st.LoadDeathCharacterRecovery(ctx, charID)
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	s := mustSaverForPersist(t)
	if err := s.Track(sim.AggregateKey{Kind: sim.AggregateCharacter, ID: charID}, rec.Character.ExpectedRevision); err != nil {
		t.Fatal(err)
	}
	vitalsJSON, err := json.Marshal(vitals)
	if err != nil {
		t.Fatalf("vitals marshal: %v", err)
	}
	_, err = CommitDeathPenalties(ctx, s, st.PGStore, store.DeathPenaltiesRequest{
		Character: store.CharacterSnapshot{
			ID: charID, Karma: durable.Karma,
			PosX: 1000, PosY: 0, PosZ: 1000,
			Vitals:      vitalsJSON,
			Advancement: append([]byte(nil), durable.Advancement...),
			Flags:       durable.Flags,
			Spells: []store.CharacterSpellSnapshot{
				{SpellID: 1, Ability: 50}, {SpellID: 2, Ability: 60, AtrophyFlag: true},
			},
			Skills: []store.CharacterSkillSnapshot{{SkillID: 1, Ability: 40}},
		},
		// The newbie-scaled plan cost, NOT the raw durable 90.
		ExpectedPendingCost: 30,
	})
	if !errors.Is(err, store.ErrPendingDeathCostMismatch) {
		t.Fatalf("err = %v, want ErrPendingDeathCostMismatch", err)
	}
	after, err := st.LoadDeathCharacterRecovery(ctx, charID)
	if err != nil {
		t.Fatalf("post recovery: %v", err)
	}
	if after.Character.ExpectedRevision != 1 {
		t.Fatalf("DB revision = %d, want still 1 (rolled back)", after.Character.ExpectedRevision)
	}
	if after.Pending == nil || after.Pending.EffectiveCost != 90 {
		t.Fatalf("DB pending = %+v, want still cost90", after.Pending)
	}
}

// 3. REAL lost acknowledgement through the executor: the
// transaction commits exactly once, the wrapper reports a
// synthetic error, the in-callback recovery proves E+1 + exact
// Character + Pending nil with no replay, and the owner
// completion succeeds.
func TestPenaltyPGLostAckThroughExecutor(t *testing.T) {
	ctx, _, _, st, deathSaver, charID := penaltyPGSetup(t, "sub-penalty-c", "Penaltyc")
	vitals := penaltyPGVitals(t)
	durable := penaltyPGDurable()
	penaltyPGDeathEntry(t, ctx, deathSaver, st, charID, vitals, durable)
	rec, err := st.LoadDeathCharacterRecovery(ctx, charID)
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	st.loadCalls = 0
	st.commitCalls = 0
	st.discardSuccess = true
	st.synthErr = errors.New("test: synthetic lost ack")
	e, id, saver := penaltyPGEngine(t, charID, rec)
	sink := &penaltyPGSink{}
	ex, _ := startPenaltyPGExecutor(t, st, saver, sink)
	prov := &penaltyRecordProvider{ex: ex}
	res, err := e.PlayerOrchestrateDeathPenalties(id, sim.UnderworldExitResolvedInput{DefaultDeathCost: 100}, &captureTestRNG{}, prov)
	if err != nil {
		t.Fatalf("orchestrate: %v", err)
	}
	ch, _ := prov.last(t).Result()
	out := awaitPenaltyResult(t, ch)
	if out.Err != nil || !out.Recovered {
		t.Fatalf("result = %+v, want recovered success", out)
	}
	if st.commitCalls != 1 {
		t.Fatalf("Store transactions = %d, want exactly 1 (no replay)", st.commitCalls)
	}
	if st.loadCalls != 1 {
		t.Fatalf("recovery loads = %d, want 1", st.loadCalls)
	}
	if got := inspectKnown(t, saver, sim.AggregateCharacter, charID); got.KnownRevision != 2 || got.Blocked {
		t.Fatalf("saver = %+v, want known2 clean (no block, no reconcile)", got)
	}
	after, err := st.LoadDeathCharacterRecovery(ctx, charID)
	if err != nil {
		t.Fatalf("post recovery: %v", err)
	}
	if after.Character.ExpectedRevision != 2 {
		t.Fatalf("DB revision = %d, want 2", after.Character.ExpectedRevision)
	}
	if after.Pending != nil {
		t.Fatalf("DB pending = %+v, want nil", after.Pending)
	}
	completions, retryables := sink.snapshot()
	if len(completions) != 1 {
		t.Fatalf("completions = %d, want 1", len(completions))
	}
	if completions[0].Token != res.Token {
		t.Fatalf("completion token = %+v, want %+v", completions[0].Token, res.Token)
	}
	if len(retryables) != 0 {
		t.Fatalf("retryables = %d, want 0", len(retryables))
	}
	if disp, err := e.PlayerAcceptDeathPenaltyCompletion(completions[0]); err != nil || disp != sim.DeathPenaltyCompletionApplied {
		t.Fatalf("owner apply = %d,%v; want Applied,nil", disp, err)
	}
	if _, ok, err := e.PlayerPendingDeathOf(id); err != nil || ok {
		t.Fatalf("live pending = %v,%v; want cleared", ok, err)
	}
}

// 4. REAL unproven pending row: the Store call fails without
// committing, so the recovery shape still carries the pending
// row. The proof must fail closed: no completion, no
// retryable, no replay.
func TestPenaltyPGUnprovenPendingRow(t *testing.T) {
	ctx, _, _, st, deathSaver, charID := penaltyPGSetup(t, "sub-penalty-d", "Penaltyd")
	vitals := penaltyPGVitals(t)
	durable := penaltyPGDurable()
	penaltyPGDeathEntry(t, ctx, deathSaver, st, charID, vitals, durable)
	rec, err := st.LoadDeathCharacterRecovery(ctx, charID)
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if rec.Pending == nil {
		t.Fatal("pre pending = nil, want live row")
	}
	st.loadCalls = 0
	st.commitCalls = 0
	st.failErr = errors.New("test: synthetic pre-commit failure")
	e, id, saver := penaltyPGEngine(t, charID, rec)
	sink := &penaltyPGSink{}
	ex, _ := startPenaltyPGExecutor(t, st, saver, sink)
	prov := &penaltyRecordProvider{ex: ex}
	if _, err := e.PlayerOrchestrateDeathPenalties(id, sim.UnderworldExitResolvedInput{DefaultDeathCost: 100}, &captureTestRNG{}, prov); err != nil {
		t.Fatalf("orchestrate: %v", err)
	}
	ch, _ := prov.last(t).Result()
	out := awaitPenaltyResult(t, ch)
	if !errors.Is(out.Err, ErrDeathPenaltyCommitUnproven) {
		t.Fatalf("result err = %v, want ErrDeathPenaltyCommitUnproven", out.Err)
	}
	if out.RetryNotified {
		t.Fatal("retryable notified after callback invocation")
	}
	if st.commitCalls != 1 {
		t.Fatalf("Store calls = %d, want 1 (no replay)", st.commitCalls)
	}
	if st.loadCalls != 1 {
		t.Fatalf("recovery loads = %d, want 1", st.loadCalls)
	}
	if got := inspectKnown(t, saver, sim.AggregateCharacter, charID); !got.Blocked {
		t.Fatalf("saver = %+v, want reconcile-blocked", got)
	}
	completions, retryables := sink.snapshot()
	if len(completions) != 0 {
		t.Fatalf("completions = %d, want 0", len(completions))
	}
	if len(retryables) != 0 {
		t.Fatalf("retryables = %d, want 0", len(retryables))
	}
	after, err := st.LoadDeathCharacterRecovery(ctx, charID)
	if err != nil {
		t.Fatalf("post recovery: %v", err)
	}
	if after.Character.ExpectedRevision != 1 || after.Pending == nil {
		t.Fatalf("DB = rev%d pending=%+v, want rev1 with live row (nothing committed)", after.Character.ExpectedRevision, after.Pending)
	}
	live, ok, err := e.PlayerPendingDeathOf(id)
	if err != nil || !ok || live.EffectiveCost != 90 {
		t.Fatalf("live pending = %+v,%v,%v; want original attempt still in flight", live, ok, err)
	}
}

// 5. REAL stale/conflict: the penalty Saver runs behind the
// materialized revision (tracked 0, DB at 1), so the
// character CAS goes stale. The recovery (rev 1 == E+1 with
// pre-penalty content) cannot prove the post-penalty commit:
// fail closed with no completion, no retryable, no replay.
func TestPenaltyPGStaleFailsClosed(t *testing.T) {
	ctx, _, _, st, deathSaver, charID := penaltyPGSetup(t, "sub-penalty-e", "Penaltye")
	vitals := penaltyPGVitals(t)
	durable := penaltyPGDurable()
	penaltyPGDeathEntry(t, ctx, deathSaver, st, charID, vitals, durable)
	rec, err := st.LoadDeathCharacterRecovery(ctx, charID)
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	st.loadCalls = 0
	st.commitCalls = 0
	e, id, _ := penaltyPGEngine(t, charID, rec)
	// Deliberately stale Saver: tracked 0 while PG is at 1.
	staleSaver := mustSaverForPersist(t)
	if err := staleSaver.Track(sim.AggregateKey{Kind: sim.AggregateCharacter, ID: charID}, 0); err != nil {
		t.Fatal(err)
	}
	sink := &penaltyPGSink{}
	ex, _ := startPenaltyPGExecutor(t, st, staleSaver, sink)
	prov := &penaltyRecordProvider{ex: ex}
	if _, err := e.PlayerOrchestrateDeathPenalties(id, sim.UnderworldExitResolvedInput{DefaultDeathCost: 100}, &captureTestRNG{}, prov); err != nil {
		t.Fatalf("orchestrate: %v", err)
	}
	ch, _ := prov.last(t).Result()
	out := awaitPenaltyResult(t, ch)
	if !errors.Is(out.Err, ErrDeathPenaltyCommitUnproven) {
		t.Fatalf("result err = %v, want ErrDeathPenaltyCommitUnproven", out.Err)
	}
	if !errors.Is(out.Err, store.ErrStaleRevision) {
		t.Fatalf("result err = %v, want stale Store cause preserved", out.Err)
	}
	if out.RetryNotified {
		t.Fatal("retryable notified after callback invocation")
	}
	if st.commitCalls != 1 {
		t.Fatalf("Store calls = %d, want 1 (no replay)", st.commitCalls)
	}
	if got := inspectKnown(t, staleSaver, sim.AggregateCharacter, charID); !got.Blocked {
		t.Fatalf("saver = %+v, want reconcile-blocked", got)
	}
	if completions, retryables := sink.snapshot(); len(completions) != 0 || len(retryables) != 0 {
		t.Fatalf("completions=%d retryables=%d, want 0/0",
			len(completions), len(retryables))
	}
	after, err := st.LoadDeathCharacterRecovery(ctx, charID)
	if err != nil {
		t.Fatalf("post recovery: %v", err)
	}
	if after.Character.ExpectedRevision != 1 || after.Pending == nil {
		t.Fatalf("DB = rev%d pending=%+v, want untouched rev1 with live row",
			after.Character.ExpectedRevision, after.Pending)
	}
}
