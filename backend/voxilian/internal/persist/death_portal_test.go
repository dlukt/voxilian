package persist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/store"
	"github.com/dlukt/voxilian/internal/world"
)

// M5-T5c3d2b2 Portal persistence executor unit tests (spec
// §9.5.1k, frozen v0.3.55): Store-domain Portal mapper, bounded
// executor + queue reservation, concrete
// PortalOfLifeWorkReservation with Prepare-time Saver critical
// slot, exactly-one reserved Execute, in-critical-callback
// read-only lost-ack proof, normal/proven-lost-ack completion,
// definite-pre-execution abort, and bounded same-mailbox
// redelivery. No PG here (real-PG proofs live in
// death_portal_pg_test.go); the Saver is always real, Store and the
// owner sink are deterministic fakes.

// ---- fakes -----------------------------------------------------

// portalFakeStore scripts CommitPortalOfLife (via the embedded T5c2b
// fake) and serves deterministic read-only character recovery. Call
// counts prove at-most-once Store and exactly-once in-callback
// recovery. An optional hook gates the commit or the recovery load
// for deterministic MarkDirty interleaving.
type portalFakeStore struct {
	*fakeDeathStore
	recSnap  store.DeathCharacterRecoverySnapshot
	recErr   error
	recCalls int
	recHook  func()
}

var _ PortalExecutionStore = (*portalFakeStore)(nil)

func newPortalFakeStore() *portalFakeStore {
	return &portalFakeStore{fakeDeathStore: &fakeDeathStore{}}
}

func (f *portalFakeStore) LoadDeathCharacterRecovery(
	_ context.Context, _ int64,
) (store.DeathCharacterRecoverySnapshot, error) {
	f.recCalls++
	if f.recHook != nil {
		f.recHook()
	}
	if f.recErr != nil {
		return store.DeathCharacterRecoverySnapshot{}, f.recErr
	}
	return f.recSnap, nil
}

// portalFakeSink scripts the typed d2a owner ingress and records
// every delivery for redelivery/abort proof.
type portalFakeSink struct {
	mu           sync.Mutex
	completions  []sim.PortalOfLifeCompletion
	aborts       []sim.PortalAttemptToken
	onCompletion func(n int, c sim.PortalOfLifeCompletion) (sim.PortalCompletionDisposition, error)
	onAbort      func(n int, tok sim.PortalAttemptToken) (sim.PortalAbortDisposition, error)
}

var _ PortalOwnerSink = (*portalFakeSink)(nil)

func (f *portalFakeSink) EnqueuePortalOfLifeCompletion(
	_ context.Context, c sim.PortalOfLifeCompletion,
) (sim.PortalCompletionDisposition, error) {
	f.mu.Lock()
	f.completions = append(f.completions, c)
	n := len(f.completions)
	on := f.onCompletion
	f.mu.Unlock()
	if on != nil {
		return on(n, c)
	}
	return sim.PortalCompletionApplied, nil
}

func (f *portalFakeSink) EnqueuePortalOfLifeAbort(
	_ context.Context, tok sim.PortalAttemptToken,
) (sim.PortalAbortDisposition, error) {
	f.mu.Lock()
	f.aborts = append(f.aborts, tok)
	n := len(f.aborts)
	on := f.onAbort
	f.mu.Unlock()
	if on != nil {
		return on(n, tok)
	}
	return sim.PortalAbortAborted, nil
}

func (f *portalFakeSink) numCompletions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.completions)
}

func (f *portalFakeSink) numAborts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.aborts)
}

// ---- fixtures --------------------------------------------------

var errPortalSynth = errors.New("test: synthetic post-commit error")

// portalTestVitals is valid gameplay vitals for Portal capture.
func portalTestVitals(t *testing.T) sim.PlayerVitals {
	t.Helper()
	v, err := sim.NewPlayerVitals(25)
	if err != nil {
		t.Fatalf("NewPlayerVitals: %v", err)
	}
	return v
}

// portalTestDurable is catalog-valid durable state: spell IDs 1-2,
// skill ID 1.
func portalTestDurable() sim.PlayerDurableState {
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
		Items: []sim.PlayerInventoryItemState{
			{ID: 101, ProtoID: 900, Qty: 3, Hits: 250, Enchants: []byte(`{"glow":1}`), Slot: "hand"},
		},
	}
}

// portalTestPending is the canonical pending fixture: cost 80, death
// time 100, corpse 500, unused.
func portalTestPending() sim.PendingDeathRuntime {
	corpse := int64(500)
	return sim.PendingDeathRuntime{EffectiveCost: 80, DeathTimeSeconds: 100, CorpseID: &corpse}
}

// portalTestCapture builds a valid standalone capture (pending cost
// 80, age 0, power 50 -> proposed 5, expected 5) without an engine.
func portalTestCapture(t *testing.T) sim.PortalOfLifeCapture {
	t.Helper()
	pending := portalTestPending()
	proposed, err := sim.PlanPortalOfLife(sim.PortalOfLifeInput{
		PendingCost: 80, CorpseAgeSeconds: 0, SpellPower: 50,
	})
	if err != nil {
		t.Fatalf("PlanPortalOfLife: %v", err)
	}
	expected, err := sim.ReducePendingDeathCost(80, proposed)
	if err != nil {
		t.Fatalf("ReducePendingDeathCost: %v", err)
	}
	return sim.PortalOfLifeCapture{
		Token:                 sim.PortalAttemptToken{EntityID: 1, CharacterID: 7, Epoch: 1},
		Position:              world.Vec3{X: 1, Y: 0, Z: 1},
		Vitals:                portalTestVitals(t),
		Durable:               portalTestDurable(),
		PendingBefore:         pending,
		TargetCorpseID:        500,
		ProposedCost:          proposed,
		ExpectedEffectiveCost: expected,
	}
}

// portalOrchestrationEngine builds a real sim engine with one Alive
// player carrying the canonical pending via authoritative hydration.
func portalOrchestrationEngine(t *testing.T, charID int64) (*sim.Engine, sim.EntityID) {
	t.Helper()
	e := mustSimEngine(t)
	snap, err := e.AddPlayerEntityWithDurableState(sim.CharacterID(charID),
		world.Vec3{X: 1, Y: 0, Z: 1}, portalTestVitals(t), testDeathRuntimeInputs(t), portalTestDurable())
	if err != nil {
		t.Fatalf("AddPlayerEntityWithDurableState: %v", err)
	}
	pending := portalTestPending()
	if err := e.PlayerInstallRecoveredPendingDeath(snap.ID, &pending); err != nil {
		t.Fatalf("PlayerInstallRecoveredPendingDeath: %v", err)
	}
	return e, snap.ID
}

func portalOrchestrationInput() sim.PortalOfLifeResolvedInput {
	return sim.PortalOfLifeResolvedInput{NowSeconds: 100, SpellPower: 50, TargetCorpseID: 500}
}

// portalTestTrack tracks the Portal character root at the given Saver
// revision.
func portalTestTrack(t *testing.T, s *sim.Saver, charID, rev int64) {
	t.Helper()
	if err := s.Track(sim.AggregateKey{Kind: sim.AggregateCharacter, ID: charID}, rev); err != nil {
		t.Fatal(err)
	}
}

// startPortalExecutor runs the executor and returns it with a cancel
// that also waits for Run to exit (no leak across -count=50). It
// spins until Run owns the executor (no pacing sleep on the hot
// path; the deadline only fails loudly on bugs).
func startPortalExecutor(
	t *testing.T, cfg PortalExecutorConfig,
) (*PortalExecutor, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ex, err := NewPortalExecutor(cfg)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	runErr := make(chan error, 1)
	go func() { runErr <- ex.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		ex.mu.Lock()
		running := ex.running
		ex.mu.Unlock()
		if running {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("timed out waiting for portal executor Run")
		}
		time.Sleep(time.Millisecond)
	}
	t.Cleanup(func() {
		cancel()
		<-runErr
	})
	return ex, cancel
}

// awaitPortalResult waits for a result with a failure-mode timeout
// (never a pacing sleep: results are buffered, delivery is prompt;
// the timeout only fails loudly on bugs).
func awaitPortalResult(t *testing.T, ch <-chan PortalPersistenceResult) PortalPersistenceResult {
	t.Helper()
	select {
	case res := <-ch:
		return res
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for portal result")
		return PortalPersistenceResult{}
	}
}

// portalTestConfig builds a running-capable executor config over the
// given fakes with a real Saver tracked at rev for character 7.
func portalTestConfig(fs *portalFakeStore, s *sim.Saver, sink *portalFakeSink) PortalExecutorConfig {
	return PortalExecutorConfig{
		Workers: 1, QueueCapacity: 4, Store: fs, Saver: s, Sink: sink,
		RetryDelay: time.Millisecond, AbortTimeout: 5 * time.Second,
	}
}

// echoPortalSuccess returns onPortal behavior echoing E+1 with the
// given effective cost.
func echoPortalSuccess(cost int16) func(store.PortalOfLifeRequest) (store.PortalOfLifeResult, error) {
	return func(req store.PortalOfLifeRequest) (store.PortalOfLifeResult, error) {
		return store.PortalOfLifeResult{
			CharacterRevision: req.Character.ExpectedRevision + 1,
			EffectiveCost:     cost,
		}, nil
	}
}

// provenPortalRec builds the recovered character snapshot proving the
// given intended request at exactly E+1 with the exact pending shape
// (cost = capture expected, used, optional corpse override; nil corpse
// allowed).
func provenPortalRec(
	req store.PortalOfLifeRequest, capture sim.PortalOfLifeCapture, rev int64, corpseOverride *int64, corpseNil bool,
) store.DeathCharacterRecoverySnapshot {
	c := freezeCharacterSnapshot(req.Character)
	c.ExpectedRevision = rev
	var corpse *int64
	switch {
	case corpseNil:
		corpse = nil
	case corpseOverride != nil:
		v := *corpseOverride
		corpse = &v
	default:
		v := capture.TargetCorpseID
		corpse = &v
	}
	return store.DeathCharacterRecoverySnapshot{
		Character: c,
		Pending: &store.PendingDeathSnapshot{
			CharacterID:      req.Character.ID,
			EffectiveCost:    int16(capture.ExpectedEffectiveCost),
			DeathTimeSeconds: capture.PendingBefore.DeathTimeSeconds,
			CorpseID:         corpse,
			PortalUsed:       true,
		},
	}
}

// ---- mapper ----------------------------------------------------

// Exact Character mapping: ID, placeholder revision, karma/flags,
// spells/skills, mm position, vitals JSON, advancement bytes,
// CorpseID, ProposedCost, plus deep-copy ownership.
func TestMapPortalOfLifeCaptureExact(t *testing.T) {
	capture := portalTestCapture(t)
	req, err := MapPortalOfLifeCapture(capture)
	if err != nil {
		t.Fatalf("MapPortalOfLifeCapture: %v", err)
	}
	if req.Character.ID != 7 {
		t.Fatalf("character ID = %d, want 7", req.Character.ID)
	}
	if req.Character.ExpectedRevision != 0 {
		t.Fatalf("ExpectedRevision = %d, want 0 placeholder", req.Character.ExpectedRevision)
	}
	if req.Character.Karma != 150 || req.Character.Flags != 0x1274 {
		t.Fatalf("karma/flags = %d/%#x, want 150/0x1274", req.Character.Karma, req.Character.Flags)
	}
	if req.Character.PosX != 1000 || req.Character.PosY != 0 || req.Character.PosZ != 1000 {
		t.Fatalf("pos = %d/%d/%d, want 1000/0/1000", req.Character.PosX, req.Character.PosY, req.Character.PosZ)
	}
	if len(req.Character.Spells) != 2 || req.Character.Spells[0].SpellID != 1 || req.Character.Spells[0].Ability != 50 {
		t.Fatalf("spells = %+v, want 2 with spell 1@50", req.Character.Spells)
	}
	if len(req.Character.Skills) != 1 || req.Character.Skills[0].SkillID != 1 {
		t.Fatalf("skills = %+v, want 1", req.Character.Skills)
	}
	var vitals sim.PlayerVitals
	if err := json.Unmarshal(req.Character.Vitals, &vitals); err != nil {
		t.Fatalf("vitals decode: %v", err)
	}
	if vitals != capture.Vitals {
		t.Fatalf("vitals = %+v, want %+v", vitals, capture.Vitals)
	}
	if string(req.Character.Advancement) != string(capture.Durable.Advancement) {
		t.Fatalf("advancement = %s, want %s", req.Character.Advancement, capture.Durable.Advancement)
	}
	if req.CorpseID != 500 {
		t.Fatalf("CorpseID = %d, want 500", req.CorpseID)
	}
	if req.ProposedCost != int16(capture.ProposedCost) || req.ProposedCost != 5 {
		t.Fatalf("ProposedCost = %d, want 5", req.ProposedCost)
	}
	// Deep-copy ownership: mutate the caller capture, request keeps
	// original frozen values.
	capture.Durable.Advancement[0] = 'X'
	capture.Durable.Spells[0].Ability = 99
	capture.Durable.Skills[0].Ability = 99
	*capture.PendingBefore.CorpseID = 999
	if req.Character.Advancement[0] == 'X' {
		t.Fatal("request aliases Advancement")
	}
	if req.Character.Spells[0].Ability == 99 || req.Character.Skills[0].Ability == 99 {
		t.Fatal("request aliases Spells/Skills")
	}
	if req.CorpseID != 500 {
		t.Fatal("request aliases pending CorpseID")
	}
}

func TestMapPortalOfLifeCaptureRejects(t *testing.T) {
	base := portalTestCapture(t)
	cases := []struct {
		name string
		f    func(sim.PortalOfLifeCapture) sim.PortalOfLifeCapture
	}{
		{"zero entity", func(c sim.PortalOfLifeCapture) sim.PortalOfLifeCapture { c.Token.EntityID = 0; return c }},
		{"zero character", func(c sim.PortalOfLifeCapture) sim.PortalOfLifeCapture { c.Token.CharacterID = 0; return c }},
		{"zero epoch", func(c sim.PortalOfLifeCapture) sim.PortalOfLifeCapture { c.Token.Epoch = 0; return c }},
		{"nan position", func(c sim.PortalOfLifeCapture) sim.PortalOfLifeCapture { c.Position.X = math.NaN(); return c }},
		{"bad vitals", func(c sim.PortalOfLifeCapture) sim.PortalOfLifeCapture { c.Vitals.HP = -1; return c }},
		{"empty advancement", func(c sim.PortalOfLifeCapture) sim.PortalOfLifeCapture { c.Durable.Advancement = nil; return c }},
		{"invalid advancement", func(c sim.PortalOfLifeCapture) sim.PortalOfLifeCapture {
			c.Durable.Advancement = []byte(`{oops`)
			return c
		}},
		{"bad spell id", func(c sim.PortalOfLifeCapture) sim.PortalOfLifeCapture { c.Durable.Spells[0].ID = 0; return c }},
		{"bad spell ability", func(c sim.PortalOfLifeCapture) sim.PortalOfLifeCapture { c.Durable.Spells[0].Ability = 100; return c }},
		{"dup spell", func(c sim.PortalOfLifeCapture) sim.PortalOfLifeCapture {
			c.Durable.Spells = append(c.Durable.Spells, c.Durable.Spells[0])
			return c
		}},
		{"bad skill ability", func(c sim.PortalOfLifeCapture) sim.PortalOfLifeCapture { c.Durable.Skills[0].Ability = 0; return c }},
		{"bad pending cost", func(c sim.PortalOfLifeCapture) sim.PortalOfLifeCapture { c.PendingBefore.EffectiveCost = 101; return c }},
		{"bad pending time", func(c sim.PortalOfLifeCapture) sim.PortalOfLifeCapture {
			c.PendingBefore.DeathTimeSeconds = -1
			return c
		}},
		{"portal used", func(c sim.PortalOfLifeCapture) sim.PortalOfLifeCapture { c.PendingBefore.PortalUsed = true; return c }},
		{"nil corpse", func(c sim.PortalOfLifeCapture) sim.PortalOfLifeCapture { c.PendingBefore.CorpseID = nil; return c }},
		{"wrong corpse", func(c sim.PortalOfLifeCapture) sim.PortalOfLifeCapture {
			v := int64(501)
			c.PendingBefore.CorpseID = &v
			return c
		}},
		{"zero target", func(c sim.PortalOfLifeCapture) sim.PortalOfLifeCapture { c.TargetCorpseID = 0; return c }},
		{"low proposed", func(c sim.PortalOfLifeCapture) sim.PortalOfLifeCapture { c.ProposedCost = 4; return c }},
		{"high proposed", func(c sim.PortalOfLifeCapture) sim.PortalOfLifeCapture { c.ProposedCost = 81; return c }},
		{"wrong expected", func(c sim.PortalOfLifeCapture) sim.PortalOfLifeCapture { c.ExpectedEffectiveCost = 6; return c }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := MapPortalOfLifeCapture(tc.f(base))
			if err == nil {
				t.Fatalf("MapPortalOfLifeCapture accepted %s", tc.name)
			}
			if !reflect.DeepEqual(req, store.PortalOfLifeRequest{}) {
				t.Fatalf("error without zero request for %s: %+v", tc.name, req)
			}
		})
	}
}

// ---- reservation owns queue + Saver slot -----------------------

// Capacity 1: one prepared reservation consumes the queue permit AND
// the Saver gate; Cancel returns both.
func TestPortalReservationOwnsQueueAndSaverSlot(t *testing.T) {
	s := mustSaverForPersist(t)
	portalTestTrack(t, s, 7, 5)
	fs := newPortalFakeStore()
	sink := &portalFakeSink{}
	ex, _ := startPortalExecutor(t, PortalExecutorConfig{
		Workers: 1, QueueCapacity: 1, Store: fs, Saver: s, Sink: sink,
		RetryDelay: time.Millisecond, AbortTimeout: 5 * time.Second,
	})

	r, err := ex.ReservePortalOfLife()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := r.PreparePortalOfLifeWork(portalTestCapture(t)); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := ex.ReservePortalOfLife(); !errors.Is(err, ErrPortalExecutorQueueFull) {
		t.Fatalf("second Reserve = %v, want QueueFull", err)
	}
	if _, err := s.ReserveCriticalSet([]sim.AggregateKey{{Kind: sim.AggregateCharacter, ID: 7}}); !errors.Is(err, sim.ErrCriticalSetReservationBusy) {
		t.Fatalf("Saver Reserve = %v, want Busy", err)
	}
	// Ordinary WriteThrough cannot overtake the held gate: it blocks
	// until the reservation releases it.
	invoked := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		_, err := s.WriteThrough(context.Background(),
			sim.AggregateKey{Kind: sim.AggregateCharacter, ID: 7},
			func(_ context.Context, exp int64) (int64, error) {
				invoked <- struct{}{}
				return exp + 1, nil
			})
		done <- err
	}()
	select {
	case <-invoked:
		t.Fatal("WriteThrough overtook the held Portal Saver gate")
	case <-time.After(50 * time.Millisecond):
	}
	r.CancelPortalOfLifeWork()
	select {
	case <-invoked:
	case <-time.After(10 * time.Second):
		t.Fatal("WriteThrough did not proceed after Cancel")
	}
	if err := <-done; err != nil {
		t.Fatalf("WriteThrough after Cancel: %v", err)
	}
	// Both queue capacity and the Saver gate returned.
	r2, err := ex.ReservePortalOfLife()
	if err != nil {
		t.Fatalf("Reserve after Cancel: %v", err)
	}
	r2.CancelPortalOfLifeWork()
	probe, err := s.ReserveCriticalSet([]sim.AggregateKey{{Kind: sim.AggregateCharacter, ID: 7}})
	if err != nil {
		t.Fatalf("Saver Reserve after Cancel: %v", err)
	}
	probe.Cancel()
}

// Prepare with the Character Saver gate held: busy error, permit
// returned, terminal reservation, zero Store calls, and a
// replacement reservation succeeds immediately.
func TestPortalPrepareSaverBusy(t *testing.T) {
	s := mustSaverForPersist(t)
	portalTestTrack(t, s, 7, 5)
	holder, err := s.ReserveCriticalSet([]sim.AggregateKey{{Kind: sim.AggregateCharacter, ID: 7}})
	if err != nil {
		t.Fatalf("hold gate: %v", err)
	}
	defer holder.Cancel()
	fs := newPortalFakeStore()
	sink := &portalFakeSink{}
	ex, _ := startPortalExecutor(t, PortalExecutorConfig{
		Workers: 1, QueueCapacity: 1, Store: fs, Saver: s, Sink: sink,
		RetryDelay: time.Millisecond, AbortTimeout: 5 * time.Second,
	})
	r, err := ex.ReservePortalOfLife()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := r.PreparePortalOfLifeWork(portalTestCapture(t)); !errors.Is(err, sim.ErrCriticalSetReservationBusy) {
		t.Fatalf("Prepare = %v, want Busy", err)
	}
	if fs.portalCalls != 0 {
		t.Fatalf("portal Store calls = %d, want 0", fs.portalCalls)
	}
	if err := r.PreparePortalOfLifeWork(portalTestCapture(t)); !errors.Is(err, ErrPortalReservationCanceled) {
		t.Fatalf("second Prepare = %v, want terminal Canceled", err)
	}
	// The queue permit returned: a replacement reservation succeeds
	// immediately (and its own Prepare still sees Busy while held).
	r2, err := ex.ReservePortalOfLife()
	if err != nil {
		t.Fatalf("replacement Reserve: %v", err)
	}
	defer r2.CancelPortalOfLifeWork()
	if err := r2.PreparePortalOfLifeWork(portalTestCapture(t)); !errors.Is(err, sim.ErrCriticalSetReservationBusy) {
		t.Fatalf("replacement Prepare = %v, want Busy", err)
	}
}

// Mapping failure terminalizes without touching the Saver gate and
// returns the permit.
func TestPortalPrepareMappingFailure(t *testing.T) {
	s := mustSaverForPersist(t)
	portalTestTrack(t, s, 7, 5)
	fs := newPortalFakeStore()
	sink := &portalFakeSink{}
	ex, _ := startPortalExecutor(t, PortalExecutorConfig{
		Workers: 1, QueueCapacity: 1, Store: fs, Saver: s, Sink: sink,
		RetryDelay: time.Millisecond, AbortTimeout: 5 * time.Second,
	})
	r, err := ex.ReservePortalOfLife()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	bad := portalTestCapture(t)
	bad.ProposedCost = 81
	if err := r.PreparePortalOfLifeWork(bad); err == nil {
		t.Fatal("Prepare accepted bad capture")
	}
	// No Saver slot was taken (gate still free) and the permit
	// returned.
	probe, err := s.ReserveCriticalSet([]sim.AggregateKey{{Kind: sim.AggregateCharacter, ID: 7}})
	if err != nil {
		t.Fatalf("Saver gate held after mapping failure: %v", err)
	}
	probe.Cancel()
	if _, err := ex.ReservePortalOfLife(); err != nil {
		t.Fatalf("Reserve after mapping failure: %v", err)
	}
}

// ---- activation shutdown refinement ----------------------------

// Prepare succeeds, executor stops before Activate, then the owner
// orchestration path rolls back synchronously: Shutdown error,
// Saver slot cancelled, permit returned, portalInFlight cleared,
// pending unchanged, epoch kept consumed, Store zero.
func TestPortalActivationShutdownRefinement(t *testing.T) {
	s := mustSaverForPersist(t)
	portalTestTrack(t, s, 7, 5)
	fs := newPortalFakeStore()
	sink := &portalFakeSink{}
	ctx, cancel := context.WithCancel(context.Background())
	ex, err := NewPortalExecutor(PortalExecutorConfig{
		Workers: 1, QueueCapacity: 2, Store: fs, Saver: s, Sink: sink,
		RetryDelay: time.Millisecond, AbortTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runErr := make(chan error, 1)
	go func() { runErr <- ex.Run(ctx) }()
	var stopOnce sync.Once
	deadline := time.Now().Add(10 * time.Second)
	for {
		ex.mu.Lock()
		running := ex.running
		ex.mu.Unlock()
		if running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run did not start")
		}
		time.Sleep(time.Millisecond)
	}
	t.Cleanup(func() {
		stopOnce.Do(func() {
			cancel()
			<-runErr
		})
	})
	e, id := portalOrchestrationEngine(t, 7)
	r, err := ex.ReservePortalOfLife()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	// Stop the executor AND wait for Run to fully exit between
	// Prepare and Activate: shutdown deterministically wins the
	// lifecycle race before publication.
	stop := &stopBeforeActivate{PortalExecutionReservation: r, stop: func() {
		stopOnce.Do(func() {
			cancel()
			<-runErr
		})
	}}
	_, err = e.PlayerOrchestratePortalOfLife(id, portalOrchestrationInput(), stop)
	if !errors.Is(err, ErrPortalExecutorShutdown) {
		t.Fatalf("orchestrate err = %v, want Shutdown", err)
	}
	if fs.portalCalls != 0 {
		t.Fatalf("Store calls = %d, want 0", fs.portalCalls)
	}
	if sink.numCompletions() != 0 || sink.numAborts() != 0 {
		t.Fatalf("sink traffic completions=%d aborts=%d, want 0/0 (no typed abort on this path)",
			sink.numCompletions(), sink.numAborts())
	}
	// Saver critical slot cancelled: the gate is free again.
	probe, err := s.ReserveCriticalSet([]sim.AggregateKey{{Kind: sim.AggregateCharacter, ID: 7}})
	if err != nil {
		t.Fatalf("Saver gate still held: %v", err)
	}
	probe.Cancel()
	// Owner rolled back synchronously: no live attempt, pending
	// unchanged, epoch kept consumed.
	assertPortalRolledBack(t, e, id, 1)
}

// stopBeforeActivate stops the executor between Prepare and Activate,
// forcing the definitive pre-publication shutdown path through the
// real d2a orchestration.
type stopBeforeActivate struct {
	*PortalExecutionReservation
	stop func()
}

func (s *stopBeforeActivate) PreparePortalOfLifeWork(c sim.PortalOfLifeCapture) error {
	return s.PortalExecutionReservation.PreparePortalOfLifeWork(c)
}

func (s *stopBeforeActivate) ActivatePortalOfLifeWork() error {
	s.stop()
	return s.PortalExecutionReservation.ActivatePortalOfLifeWork()
}

func (s *stopBeforeActivate) CancelPortalOfLifeWork() {
	s.PortalExecutionReservation.CancelPortalOfLifeWork()
}

func assertPortalRolledBack(t *testing.T, e *sim.Engine, id sim.EntityID, wantEpoch uint64) {
	t.Helper()
	// White-box epoch/in-flight read via a second orchestration token
	// probe is impossible without mutating; inspect pending + attempt
	// a fresh begin to observe the consumed epoch.
	live, ok, err := e.PlayerPendingDeathOf(id)
	if err != nil || !ok {
		t.Fatalf("pending = %+v,%v,%v; want live pending unchanged", live, ok, err)
	}
	if live.EffectiveCost != 80 || live.DeathTimeSeconds != 100 || live.PortalUsed || live.CorpseID == nil || *live.CorpseID != 500 {
		t.Fatalf("pending mutated: %+v", live)
	}
	r := &rollbackProbeReservation{}
	res, err := e.PlayerOrchestratePortalOfLife(id, portalOrchestrationInput(), r)
	if err != nil {
		t.Fatalf("retry orchestrate: %v", err)
	}
	if res.Token.Epoch != wantEpoch+1 {
		t.Fatalf("retry epoch = %d, want %d (prior epoch kept consumed)", res.Token.Epoch, wantEpoch+1)
	}
}

type rollbackProbeReservation struct{}

func (rollbackProbeReservation) PreparePortalOfLifeWork(sim.PortalOfLifeCapture) error { return nil }
func (rollbackProbeReservation) ActivatePortalOfLifeWork() error                       { return nil }
func (rollbackProbeReservation) CancelPortalOfLifeWork()                               {}

// ---- normal success --------------------------------------------

// Full owner loop through a real reservation: one Store call, zero
// recovery loads, Saver known E+1, newer post-Prepare MarkDirty
// preserved, one Applied completion, Recovered=false.
func TestPortalNormalSuccess(t *testing.T) {
	s := mustSaverForPersist(t)
	portalTestTrack(t, s, 7, 5)
	fs := newPortalFakeStore()
	fs.onPortal = echoPortalSuccess(5)
	sink := &portalFakeSink{}
	ex, _ := startPortalExecutor(t, portalTestConfig(fs, s, sink))
	e, id := portalOrchestrationEngine(t, 7)

	entered := make(chan struct{}, 1)
	proceed := make(chan struct{})
	fs.onPortal = func(req store.PortalOfLifeRequest) (store.PortalOfLifeResult, error) {
		entered <- struct{}{}
		<-proceed
		return echoPortalSuccess(5)(req)
	}
	r, err := ex.ReservePortalOfLife()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	res, err := e.PlayerOrchestratePortalOfLife(id, portalOrchestrationInput(), r)
	if err != nil {
		t.Fatalf("orchestrate: %v", err)
	}
	if res.Token.Epoch != 1 {
		t.Fatalf("epoch = %d, want 1", res.Token.Epoch)
	}
	ch, err := r.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	<-entered
	// Newer gameplay snapshot arrives after Prepare while the Portal
	// holds the gate: it must survive success.
	mark := func(ctx context.Context, exp int64) (int64, error) { return exp + 1, nil }
	if err := s.MarkDirty(sim.AggregateKey{Kind: sim.AggregateCharacter, ID: 7}, mark); err != nil {
		t.Fatalf("MarkDirty: %v", err)
	}
	close(proceed)
	out := awaitPortalResult(t, ch)
	if out.Err != nil {
		t.Fatalf("result err = %v", out.Err)
	}
	if out.Recovered {
		t.Fatal("Recovered = true, want false (normal ack)")
	}
	if out.Delivery != sim.PortalCompletionApplied {
		t.Fatalf("delivery = %d, want Applied", uint8(out.Delivery))
	}
	if fs.portalCalls != 1 {
		t.Fatalf("Store calls = %d, want 1", fs.portalCalls)
	}
	if fs.recCalls != 0 {
		t.Fatalf("recovery loads = %d, want 0", fs.recCalls)
	}
	got := inspectKnown(t, s, sim.AggregateCharacter, 7)
	if got.KnownRevision != 6 || got.Blocked {
		t.Fatalf("saver = %+v, want known6 clean", got)
	}
	if !got.Dirty {
		t.Fatal("newer post-Prepare MarkDirty was superseded, want Dirty preserved")
	}
	if sink.numCompletions() != 1 {
		t.Fatalf("completions = %d, want 1", sink.numCompletions())
	}
	c := sink.completions[0]
	if c.Token != res.Token {
		t.Fatalf("completion token = %+v, want %+v", c.Token, res.Token)
	}
	if c.Pending.EffectiveCost != 5 || !c.Pending.PortalUsed || c.Pending.DeathTimeSeconds != 100 ||
		c.Pending.CorpseID == nil || *c.Pending.CorpseID != 500 {
		t.Fatalf("completion pending = %+v, want cost5/used/time100/corpse500", c.Pending)
	}
	// The delivered completion is genuinely consumable by the d2a
	// owner: Applied with pending installed, attempt cleared.
	if disp, err := e.PlayerAcceptPortalOfLifeCompletion(c); err != nil || disp != sim.PortalCompletionApplied {
		t.Fatalf("owner apply = %d,%v; want Applied,nil", disp, err)
	}
}

// Lowers-only cost 0: pending cost 0 with proposed >= 5 still starts,
// succeeds, and completes with PortalUsed + EffectiveCost 0.
func TestPortalLowersOnlyCostZero(t *testing.T) {
	s := mustSaverForPersist(t)
	portalTestTrack(t, s, 7, 5)
	fs := newPortalFakeStore()
	fs.onPortal = echoPortalSuccess(0)
	sink := &portalFakeSink{}
	ex, _ := startPortalExecutor(t, portalTestConfig(fs, s, sink))
	e := mustSimEngine(t)
	snap, err := e.AddPlayerEntityWithDurableState(7,
		world.Vec3{X: 1, Y: 0, Z: 1}, portalTestVitals(t), testDeathRuntimeInputs(t), portalTestDurable())
	if err != nil {
		t.Fatalf("add player: %v", err)
	}
	corpse := int64(500)
	zero := sim.PendingDeathRuntime{EffectiveCost: 0, DeathTimeSeconds: 100, CorpseID: &corpse}
	if err := e.PlayerInstallRecoveredPendingDeath(snap.ID, &zero); err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	r, err := ex.ReservePortalOfLife()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	res, err := e.PlayerOrchestratePortalOfLife(snap.ID, portalOrchestrationInput(), r)
	if err != nil {
		t.Fatalf("orchestrate: %v", err)
	}
	if res.ExpectedEffectiveCost != 0 {
		t.Fatalf("expected = %d, want 0", res.ExpectedEffectiveCost)
	}
	ch, _ := r.Result()
	out := awaitPortalResult(t, ch)
	if out.Err != nil || out.Recovered {
		t.Fatalf("result = %+v, want success unrecovered", out)
	}
	if sink.numCompletions() != 1 {
		t.Fatalf("completions = %d, want 1", sink.numCompletions())
	}
	if got := sink.completions[0].Pending; got.EffectiveCost != 0 || !got.PortalUsed {
		t.Fatalf("completion pending = %+v, want cost0/used", got)
	}
}

// Duplicate owner completion is executor success with no Store replay.
func TestPortalDuplicateOwnerCompletion(t *testing.T) {
	s := mustSaverForPersist(t)
	portalTestTrack(t, s, 7, 5)
	fs := newPortalFakeStore()
	fs.onPortal = echoPortalSuccess(5)
	sink := &portalFakeSink{}
	sink.onCompletion = func(n int, c sim.PortalOfLifeCompletion) (sim.PortalCompletionDisposition, error) {
		return sim.PortalCompletionDuplicate, nil
	}
	ex, _ := startPortalExecutor(t, portalTestConfig(fs, s, sink))
	e, id := portalOrchestrationEngine(t, 7)
	r, err := ex.ReservePortalOfLife()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if _, err := e.PlayerOrchestratePortalOfLife(id, portalOrchestrationInput(), r); err != nil {
		t.Fatalf("orchestrate: %v", err)
	}
	ch, _ := r.Result()
	out := awaitPortalResult(t, ch)
	if out.Err != nil {
		t.Fatalf("result err = %v, want nil (Duplicate is success)", out.Err)
	}
	if out.Delivery != sim.PortalCompletionDuplicate {
		t.Fatalf("delivery = %d, want Duplicate", uint8(out.Delivery))
	}
	if fs.portalCalls != 1 || fs.recCalls != 0 {
		t.Fatalf("Store=%d recovery=%d, want 1/0 (no replay)", fs.portalCalls, fs.recCalls)
	}
}

// ---- proven lost ack -------------------------------------------

// Store reports a synthetic error after the state it would have
// committed is represented by the recovery loader: exactly one Store
// call, one recovery load, Recovered=true, Saver known E+1 unblocked,
// newer pending snapshot survives, one completion, and no
// ReconcileSaver/ResolveReconciled (behaviorally: the newer MarkDirty
// survives; statically: tripwire test below).
func TestPortalProvenLostAck(t *testing.T) {
	s := mustSaverForPersist(t)
	portalTestTrack(t, s, 7, 5)
	fs := newPortalFakeStore()
	fs.onPortal = func(req store.PortalOfLifeRequest) (store.PortalOfLifeResult, error) {
		return store.PortalOfLifeResult{}, errPortalSynth
	}
	entered := make(chan struct{}, 1)
	proceed := make(chan struct{})
	fs.recHook = func() {
		entered <- struct{}{}
		<-proceed
	}
	sink := &portalFakeSink{}
	ex, _ := startPortalExecutor(t, portalTestConfig(fs, s, sink))
	e, id := portalOrchestrationEngine(t, 7)

	captureProbe := portalTestCapture(t)
	intended, err := MapPortalOfLifeCapture(captureProbe)
	if err != nil {
		t.Fatalf("intended map: %v", err)
	}
	fs.recSnap = provenPortalRec(intended, captureProbe, 6, nil, false)

	r, err := ex.ReservePortalOfLife()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	res, err := e.PlayerOrchestratePortalOfLife(id, portalOrchestrationInput(), r)
	if err != nil {
		t.Fatalf("orchestrate: %v", err)
	}
	ch, _ := r.Result()
	<-entered
	mark := func(ctx context.Context, exp int64) (int64, error) { return exp + 1, nil }
	if err := s.MarkDirty(sim.AggregateKey{Kind: sim.AggregateCharacter, ID: 7}, mark); err != nil {
		t.Fatalf("MarkDirty: %v", err)
	}
	close(proceed)
	out := awaitPortalResult(t, ch)
	if out.Err != nil {
		t.Fatalf("result err = %v, want proven success", out.Err)
	}
	if !out.Recovered {
		t.Fatal("Recovered = false, want true (lost ack)")
	}
	if fs.portalCalls != 1 {
		t.Fatalf("Store calls = %d, want 1 (no replay)", fs.portalCalls)
	}
	if fs.recCalls != 1 {
		t.Fatalf("recovery loads = %d, want 1", fs.recCalls)
	}
	got := inspectKnown(t, s, sim.AggregateCharacter, 7)
	if got.KnownRevision != 6 || got.Blocked {
		t.Fatalf("saver = %+v, want known6 clean (no block)", got)
	}
	if !got.Dirty {
		t.Fatal("newer post-Prepare pending snapshot lost: proven path must preserve gen > reservedGeneration")
	}
	if sink.numCompletions() != 1 {
		t.Fatalf("completions = %d, want 1", sink.numCompletions())
	}
	c := sink.completions[0]
	if c.Token != res.Token {
		t.Fatalf("completion token = %+v, want %+v", c.Token, res.Token)
	}
	if c.Pending.EffectiveCost != 5 || !c.Pending.PortalUsed || c.Pending.CorpseID == nil || *c.Pending.CorpseID != 500 {
		t.Fatalf("recovered completion pending = %+v, want cost5/used/corpse500", c.Pending)
	}
	if disp, err := e.PlayerAcceptPortalOfLifeCompletion(c); err != nil || disp != sim.PortalCompletionApplied {
		t.Fatalf("owner apply = %d,%v; want Applied,nil", disp, err)
	}
}

// Recovered pending with nil CorpseID (corpse expired after commit):
// proof succeeds and the completion carries nil.
func TestPortalLostAckNilCorpseID(t *testing.T) {
	s := mustSaverForPersist(t)
	portalTestTrack(t, s, 7, 5)
	fs := newPortalFakeStore()
	fs.onPortal = func(store.PortalOfLifeRequest) (store.PortalOfLifeResult, error) {
		return store.PortalOfLifeResult{}, errPortalSynth
	}
	capture := portalTestCapture(t)
	intended, err := MapPortalOfLifeCapture(capture)
	if err != nil {
		t.Fatalf("intended map: %v", err)
	}
	fs.recSnap = provenPortalRec(intended, capture, 6, nil, true)
	sink := &portalFakeSink{}
	ex, _ := startPortalExecutor(t, portalTestConfig(fs, s, sink))
	e, id := portalOrchestrationEngine(t, 7)
	r, err := ex.ReservePortalOfLife()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if _, err := e.PlayerOrchestratePortalOfLife(id, portalOrchestrationInput(), r); err != nil {
		t.Fatalf("orchestrate: %v", err)
	}
	ch, _ := r.Result()
	out := awaitPortalResult(t, ch)
	if out.Err != nil || !out.Recovered {
		t.Fatalf("result = %+v, want recovered success", out)
	}
	if sink.numCompletions() != 1 {
		t.Fatalf("completions = %d, want 1", sink.numCompletions())
	}
	if got := sink.completions[0].Pending; got.CorpseID != nil {
		t.Fatalf("completion corpse = %d, want nil (recovery authoritative)", *got.CorpseID)
	}
}

// ---- unproven paths --------------------------------------------

// runUnproven drives one execution whose recovery snapshot is built
// by tweak, then asserts the fail-closed contract: Store exactly
// once, unproven sentinel, Saver blocked, zero completion, zero
// abort, zero replay.
func runUnproven(
	t *testing.T, name string,
	tweak func(req store.PortalOfLifeRequest, capture sim.PortalOfLifeCapture, rec *store.DeathCharacterRecoverySnapshot),
	storeErr error,
) {
	t.Helper()
	s := mustSaverForPersist(t)
	portalTestTrack(t, s, 7, 5)
	fs := newPortalFakeStore()
	if storeErr != nil {
		fs.onPortal = func(store.PortalOfLifeRequest) (store.PortalOfLifeResult, error) {
			return store.PortalOfLifeResult{}, storeErr
		}
	} else {
		fs.onPortal = echoPortalSuccess(5)
	}
	capture := portalTestCapture(t)
	intended, err := MapPortalOfLifeCapture(capture)
	if err != nil {
		t.Fatalf("intended map: %v", err)
	}
	rec := provenPortalRec(intended, capture, 6, nil, false)
	tweak(intended, capture, &rec)
	fs.recSnap = rec
	sink := &portalFakeSink{}
	ex, _ := startPortalExecutor(t, portalTestConfig(fs, s, sink))
	e, id := portalOrchestrationEngine(t, 7)
	r, err := ex.ReservePortalOfLife()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if _, err := e.PlayerOrchestratePortalOfLife(id, portalOrchestrationInput(), r); err != nil {
		t.Fatalf("orchestrate: %v", err)
	}
	ch, _ := r.Result()
	out := awaitPortalResult(t, ch)
	if out.Err == nil {
		t.Fatalf("%s: result err = nil, want unproven", name)
	}
	if !errors.Is(out.Err, ErrPortalCommitUnproven) {
		t.Fatalf("%s: err = %v, want ErrPortalCommitUnproven", name, out.Err)
	}
	if storeErr != nil && !errors.Is(out.Err, storeErr) {
		t.Fatalf("%s: err = %v, want Store cause preserved", name, out.Err)
	}
	if fs.portalCalls != 1 {
		t.Fatalf("%s: Store calls = %d, want 1 (no replay)", name, fs.portalCalls)
	}
	got := inspectKnown(t, s, sim.AggregateCharacter, 7)
	if !got.Blocked {
		t.Fatalf("%s: saver = %+v, want reconcile-blocked", name, got)
	}
	if sink.numCompletions() != 0 {
		t.Fatalf("%s: completions = %d, want 0", name, sink.numCompletions())
	}
	if sink.numAborts() != 0 {
		t.Fatalf("%s: aborts = %d, want 0 (Store crossed => never abort)", name, sink.numAborts())
	}
	live, ok, lerr := e.PlayerPendingDeathOf(id)
	if lerr != nil || !ok || live.PortalUsed || live.EffectiveCost != 80 {
		t.Fatalf("%s: live pending = %+v,%v,%v; want original attempt still in flight", name, live, ok, lerr)
	}
}

func TestPortalUnprovenMatrix(t *testing.T) {
	corpse501 := int64(501)
	cases := []struct {
		name  string
		tweak func(req store.PortalOfLifeRequest, capture sim.PortalOfLifeCapture, rec *store.DeathCharacterRecoverySnapshot)
	}{
		{"revision E", func(_ store.PortalOfLifeRequest, _ sim.PortalOfLifeCapture, rec *store.DeathCharacterRecoverySnapshot) {
			rec.Character.ExpectedRevision = 5
		}},
		{"revision beyond E+1", func(_ store.PortalOfLifeRequest, _ sim.PortalOfLifeCapture, rec *store.DeathCharacterRecoverySnapshot) {
			rec.Character.ExpectedRevision = 7
		}},
		{"position", func(req store.PortalOfLifeRequest, _ sim.PortalOfLifeCapture, rec *store.DeathCharacterRecoverySnapshot) {
			rec.Character.PosX = req.Character.PosX + 1
		}},
		{"vitals", func(_ store.PortalOfLifeRequest, _ sim.PortalOfLifeCapture, rec *store.DeathCharacterRecoverySnapshot) {
			rec.Character.Vitals = json.RawMessage(`{"hp":1}`)
		}},
		{"flags", func(req store.PortalOfLifeRequest, _ sim.PortalOfLifeCapture, rec *store.DeathCharacterRecoverySnapshot) {
			rec.Character.Flags = req.Character.Flags ^ 1
		}},
		{"ability", func(_ store.PortalOfLifeRequest, _ sim.PortalOfLifeCapture, rec *store.DeathCharacterRecoverySnapshot) {
			rec.Character.Spells[0].Ability++
		}},
		{"advancement", func(_ store.PortalOfLifeRequest, _ sim.PortalOfLifeCapture, rec *store.DeathCharacterRecoverySnapshot) {
			rec.Character.Advancement = json.RawMessage(`{"adv_points":8}`)
		}},
		{"pending unused", func(_ store.PortalOfLifeRequest, _ sim.PortalOfLifeCapture, rec *store.DeathCharacterRecoverySnapshot) {
			rec.Pending.PortalUsed = false
		}},
		{"pending cost", func(_ store.PortalOfLifeRequest, _ sim.PortalOfLifeCapture, rec *store.DeathCharacterRecoverySnapshot) {
			rec.Pending.EffectiveCost = 80
		}},
		{"pending death time", func(_ store.PortalOfLifeRequest, _ sim.PortalOfLifeCapture, rec *store.DeathCharacterRecoverySnapshot) {
			rec.Pending.DeathTimeSeconds = 101
		}},
		{"pending wrong corpse", func(_ store.PortalOfLifeRequest, _ sim.PortalOfLifeCapture, rec *store.DeathCharacterRecoverySnapshot) {
			rec.Pending.CorpseID = &corpse501
		}},
		{"pending missing", func(_ store.PortalOfLifeRequest, _ sim.PortalOfLifeCapture, rec *store.DeathCharacterRecoverySnapshot) {
			rec.Pending = nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runUnproven(t, tc.name, tc.tweak, errPortalSynth)
		})
	}
}

// Semantic Store error (corpse mismatch) with unproven recovery:
// cause preserved, Saver blocked, NO abort, NO replay. This pins the
// Store-crossed => no definitive abort rule.
func TestPortalSemanticStoreErrorUnproven(t *testing.T) {
	semantic := fmt.Errorf("test: semantic portal: %w", store.ErrPortalCorpseMismatch)
	runUnproven(t, "semantic", func(_ store.PortalOfLifeRequest, _ sim.PortalOfLifeCapture, rec *store.DeathCharacterRecoverySnapshot) {
		rec.Character.ExpectedRevision = 5
		rec.Pending.PortalUsed = false
	}, semantic)
}

// Recovery loader error: Saver blocked, no completion, no abort, no
// replay.
func TestPortalRecoveryLoaderError(t *testing.T) {
	s := mustSaverForPersist(t)
	portalTestTrack(t, s, 7, 5)
	fs := newPortalFakeStore()
	fs.onPortal = func(store.PortalOfLifeRequest) (store.PortalOfLifeResult, error) {
		return store.PortalOfLifeResult{}, errPortalSynth
	}
	fs.recErr = errors.New("test: recovery unavailable")
	sink := &portalFakeSink{}
	ex, _ := startPortalExecutor(t, portalTestConfig(fs, s, sink))
	e, id := portalOrchestrationEngine(t, 7)
	r, err := ex.ReservePortalOfLife()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if _, err := e.PlayerOrchestratePortalOfLife(id, portalOrchestrationInput(), r); err != nil {
		t.Fatalf("orchestrate: %v", err)
	}
	ch, _ := r.Result()
	out := awaitPortalResult(t, ch)
	if out.Err == nil {
		t.Fatal("result err = nil, want recovery failure")
	}
	if !errors.Is(out.Err, errPortalSynth) {
		t.Fatalf("err = %v, want Store cause preserved", out.Err)
	}
	if fs.portalCalls != 1 || fs.recCalls != 1 {
		t.Fatalf("Store=%d recovery=%d, want 1/1 (no replay)", fs.portalCalls, fs.recCalls)
	}
	if got := inspectKnown(t, s, sim.AggregateCharacter, 7); !got.Blocked {
		t.Fatalf("saver = %+v, want blocked", got)
	}
	if sink.numCompletions() != 0 || sink.numAborts() != 0 {
		t.Fatalf("sink completions=%d aborts=%d, want 0/0", sink.numCompletions(), sink.numAborts())
	}
}

// ---- shutdown / abort paths ------------------------------------

// A blocked worker on job A plus queued job B; executor cancel:
// B sees Store zero, Saver slot cancelled, exactly one abort, a
// terminal result, and its permit returned.
func TestPortalQueuedShutdown(t *testing.T) {
	s := mustSaverForPersist(t)
	portalTestTrack(t, s, 7, 5)
	portalTestTrack(t, s, 8, 9)
	fs := newPortalFakeStore()
	releaseA := make(chan struct{})
	enteredA := make(chan struct{}, 1)
	fs.onPortal = func(req store.PortalOfLifeRequest) (store.PortalOfLifeResult, error) {
		if req.Character.ID == 7 {
			enteredA <- struct{}{}
			<-releaseA
			return store.PortalOfLifeResult{
				CharacterRevision: req.Character.ExpectedRevision + 1,
				EffectiveCost:     5,
			}, nil
		}
		t.Errorf("Store called for queued job B (character 8)")
		return store.PortalOfLifeResult{}, errPortalSynth
	}
	sink := &portalFakeSink{}
	ex, cancel := startPortalExecutor(t, PortalExecutorConfig{
		Workers: 1, QueueCapacity: 2, Store: fs, Saver: s, Sink: sink,
		RetryDelay: time.Millisecond, AbortTimeout: 5 * time.Second,
	})
	// Job A: manual reservation for character 7.
	ra, err := ex.ReservePortalOfLife()
	if err != nil {
		t.Fatalf("Reserve A: %v", err)
	}
	capA := portalTestCapture(t)
	if err := ra.PreparePortalOfLifeWork(capA); err != nil {
		t.Fatalf("Prepare A: %v", err)
	}
	if err := ra.ActivatePortalOfLifeWork(); err != nil {
		t.Fatalf("Activate A: %v", err)
	}
	<-enteredA
	// Job B: manual reservation for character 8, activated/queued.
	rb, err := ex.ReservePortalOfLife()
	if err != nil {
		t.Fatalf("Reserve B: %v", err)
	}
	capB := portalTestCapture(t)
	capB.Token = sim.PortalAttemptToken{EntityID: 2, CharacterID: 8, Epoch: 1}
	capB.PendingBefore.EffectiveCost = 80
	proposed, err := sim.PlanPortalOfLife(sim.PortalOfLifeInput{PendingCost: 80, CorpseAgeSeconds: 0, SpellPower: 50})
	if err != nil {
		t.Fatalf("plan B: %v", err)
	}
	expected, err := sim.ReducePendingDeathCost(80, proposed)
	if err != nil {
		t.Fatalf("reduce B: %v", err)
	}
	capB.ProposedCost = proposed
	capB.ExpectedEffectiveCost = expected
	if err := rb.PreparePortalOfLifeWork(capB); err != nil {
		t.Fatalf("Prepare B: %v", err)
	}
	if err := rb.ActivatePortalOfLifeWork(); err != nil {
		t.Fatalf("Activate B: %v", err)
	}
	chB, _ := rb.Result()
	cancel()
	close(releaseA)
	chA, _ := ra.Result()
	outA := awaitPortalResult(t, chA)
	if outA.Err != nil {
		t.Fatalf("job A err = %v (running callback observes cancellation but Store already returned)", outA.Err)
	}
	outB := awaitPortalResult(t, chB)
	if !errors.Is(outB.Err, ErrPortalExecutorShutdown) {
		t.Fatalf("job B err = %v, want terminal Shutdown", outB.Err)
	}
	if !outB.Aborted {
		t.Fatal("job B Aborted = false, want definitive abort delivered")
	}
	if sink.numAborts() != 1 {
		t.Fatalf("aborts = %d, want exactly 1", sink.numAborts())
	}
	if sink.aborts[0] != capB.Token {
		t.Fatalf("abort token = %+v, want B token %+v", sink.aborts[0], capB.Token)
	}
	// B's Saver slot was cancelled: the gate is free.
	probe, err := s.ReserveCriticalSet([]sim.AggregateKey{{Kind: sim.AggregateCharacter, ID: 8}})
	if err != nil {
		t.Fatalf("B Saver gate still held: %v", err)
	}
	probe.Cancel()
}

// Deterministic pre-callback Execute failure (no-callback path):
// owner abort, zero Store calls.
func TestPortalPreCallbackExecuteFailure(t *testing.T) {
	s := mustSaverForPersist(t)
	portalTestTrack(t, s, 7, 5)
	fs := newPortalFakeStore()
	sink := &portalFakeSink{}
	ex, _ := startPortalExecutor(t, portalTestConfig(fs, s, sink))
	r, err := ex.ReservePortalOfLife()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	capture := portalTestCapture(t)
	if err := r.PreparePortalOfLifeWork(capture); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	// White-box: steal the frozen job inputs, cancel the Saver slot
	// externally (simulating shutdown-before-execution), then execute
	// with an already-cancelled context so the reserved Execute fails
	// before callback invocation.
	r.mu.Lock()
	job := portalExecutionJob{req: r.req, capture: r.capture, critical: r.critical, res: r.res}
	r.mu.Unlock()
	job.critical.Cancel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := ex.execute(ctx, job)
	if fs.portalCalls != 0 {
		t.Fatalf("Store calls = %d, want 0", fs.portalCalls)
	}
	if sink.numAborts() != 1 {
		t.Fatalf("aborts = %d, want 1 (definitive pre-Store abort)", sink.numAborts())
	}
	if sink.aborts[0] != capture.Token {
		t.Fatalf("abort token = %+v, want %+v", sink.aborts[0], capture.Token)
	}
	if !out.Aborted || sink.numCompletions() != 0 {
		t.Fatalf("result = %+v completions=%d, want aborted success with no completion",
			out, sink.numCompletions())
	}
}

// ---- redelivery ------------------------------------------------

// Sink Full, Full, then Applied: exactly one Store call, at most one
// recovery, same completion/token redelivered, eventual success.
func TestPortalIngressFullCompletion(t *testing.T) {
	s := mustSaverForPersist(t)
	portalTestTrack(t, s, 7, 5)
	fs := newPortalFakeStore()
	fs.onPortal = echoPortalSuccess(5)
	sink := &portalFakeSink{}
	sink.onCompletion = func(n int, c sim.PortalOfLifeCompletion) (sim.PortalCompletionDisposition, error) {
		if n <= 2 {
			return sim.PortalCompletionApplied, sim.ErrSimIngressFull
		}
		return sim.PortalCompletionApplied, nil
	}
	ex, _ := startPortalExecutor(t, portalTestConfig(fs, s, sink))
	e, id := portalOrchestrationEngine(t, 7)
	r, err := ex.ReservePortalOfLife()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	res, err := e.PlayerOrchestratePortalOfLife(id, portalOrchestrationInput(), r)
	if err != nil {
		t.Fatalf("orchestrate: %v", err)
	}
	ch, _ := r.Result()
	out := awaitPortalResult(t, ch)
	if out.Err != nil {
		t.Fatalf("result err = %v, want eventual success", out.Err)
	}
	if fs.portalCalls != 1 {
		t.Fatalf("Store calls = %d, want exactly 1 (no replay)", fs.portalCalls)
	}
	if fs.recCalls > 1 {
		t.Fatalf("recovery loads = %d, want at most 1", fs.recCalls)
	}
	if sink.numCompletions() != 3 {
		t.Fatalf("completions = %d, want 3 redeliveries", sink.numCompletions())
	}
	for i, c := range sink.completions {
		if c.Token != res.Token {
			t.Fatalf("delivery %d token = %+v, want same %+v", i, c.Token, res.Token)
		}
	}
}

// Definite pre-Store abort under ingress pressure: Full then
// Aborted for the same token, bounded retry, Store zero.
func TestPortalIngressFullAbort(t *testing.T) {
	s := mustSaverForPersist(t)
	portalTestTrack(t, s, 7, 5)
	fs := newPortalFakeStore()
	sink := &portalFakeSink{}
	sink.onAbort = func(n int, tok sim.PortalAttemptToken) (sim.PortalAbortDisposition, error) {
		if n == 1 {
			return sim.PortalAbortAborted, sim.ErrSimIngressFull
		}
		return sim.PortalAbortAborted, nil
	}
	ex, cancel := startPortalExecutor(t, PortalExecutorConfig{
		Workers: 1, QueueCapacity: 1, Store: fs, Saver: s, Sink: sink,
		RetryDelay: time.Millisecond, AbortTimeout: 5 * time.Second,
	})
	r, err := ex.ReservePortalOfLife()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	capture := portalTestCapture(t)
	if err := r.PreparePortalOfLifeWork(capture); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := r.ActivatePortalOfLifeWork(); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	ch, _ := r.Result()
	cancel()
	out := awaitPortalResult(t, ch)
	if !errors.Is(out.Err, ErrPortalExecutorShutdown) {
		t.Fatalf("err = %v, want Shutdown", out.Err)
	}
	if !out.Aborted {
		t.Fatal("Aborted = false, want abort delivered after Full retry")
	}
	if sink.numAborts() != 2 {
		t.Fatalf("aborts = %d, want 2 (Full + Aborted)", sink.numAborts())
	}
	for i, tok := range sink.aborts {
		if tok != capture.Token {
			t.Fatalf("abort %d token = %+v, want same %+v", i, tok, capture.Token)
		}
	}
	if fs.portalCalls != 0 || fs.recCalls != 0 {
		t.Fatalf("Store=%d recovery=%d, want 0/0", fs.portalCalls, fs.recCalls)
	}
}

// ---- aliasing --------------------------------------------------

// Caller mutation after Prepare cannot reach the Store request,
// proof reference, or completion.
func TestPortalSubmissionAliasing(t *testing.T) {
	s := mustSaverForPersist(t)
	portalTestTrack(t, s, 7, 5)
	fs := newPortalFakeStore()
	fs.onPortal = echoPortalSuccess(5)
	sink := &portalFakeSink{}
	ex, _ := startPortalExecutor(t, portalTestConfig(fs, s, sink))
	r, err := ex.ReservePortalOfLife()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	capture := portalTestCapture(t)
	if err := r.PreparePortalOfLifeWork(capture); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	// Poison every caller-owned mutable domain after Prepare.
	capture.Durable.Advancement[0] = 'X'
	capture.Durable.Spells[0].Ability = 1
	capture.Durable.Skills[0].Ability = 1
	capture.Durable.Items[0].Enchants[0] = 'X'
	*capture.PendingBefore.CorpseID = 424242
	capture.Vitals.HP = 1
	if err := r.ActivatePortalOfLifeWork(); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	ch, _ := r.Result()
	out := awaitPortalResult(t, ch)
	if out.Err != nil {
		t.Fatalf("result err = %v", out.Err)
	}
	got := fs.gotPortal[0]
	if got.Character.Advancement[0] == 'X' || got.Character.Spells[0].Ability == 1 || got.Character.Skills[0].Ability == 1 {
		t.Fatal("Store request reflects post-Prepare caller mutation")
	}
	if got.CorpseID != 500 {
		t.Fatalf("Store CorpseID = %d, want frozen 500", got.CorpseID)
	}
	c := sink.completions[0]
	if c.Pending.CorpseID == nil || *c.Pending.CorpseID != 500 || c.Pending.EffectiveCost != 5 {
		t.Fatalf("completion = %+v, want frozen corpse500/cost5", c.Pending)
	}
}

// ---- lifecycle / bounds / race ---------------------------------

func TestPortalExecutorConfigValidation(t *testing.T) {
	s := mustSaverForPersist(t)
	portalTestTrack(t, s, 7, 5)
	fs := newPortalFakeStore()
	sink := &portalFakeSink{}
	good := portalTestConfig(fs, s, sink)
	for _, tc := range []struct {
		name string
		f    func(*PortalExecutorConfig)
	}{
		{"workers", func(c *PortalExecutorConfig) { c.Workers = 0 }},
		{"capacity", func(c *PortalExecutorConfig) { c.QueueCapacity = 0 }},
		{"store", func(c *PortalExecutorConfig) { c.Store = nil }},
		{"saver", func(c *PortalExecutorConfig) { c.Saver = nil }},
		{"sink", func(c *PortalExecutorConfig) { c.Sink = nil }},
	} {
		cfg := good
		tc.f(&cfg)
		if _, err := NewPortalExecutor(cfg); !errors.Is(err, ErrPortalExecutorInvalid) {
			t.Fatalf("%s: err = %v, want Invalid", tc.name, err)
		}
	}
}

func TestPortalExecutorNotRunningAndQueueFull(t *testing.T) {
	s := mustSaverForPersist(t)
	portalTestTrack(t, s, 7, 5)
	fs := newPortalFakeStore()
	sink := &portalFakeSink{}
	ex, err := NewPortalExecutor(PortalExecutorConfig{
		Workers: 1, QueueCapacity: 1, Store: fs, Saver: s, Sink: sink,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := ex.ReservePortalOfLife(); !errors.Is(err, ErrPortalExecutorNotRunning) {
		t.Fatalf("Reserve stopped = %v, want NotRunning", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- ex.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		ex.mu.Lock()
		running := ex.running
		ex.mu.Unlock()
		if running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run did not start")
		}
		time.Sleep(time.Millisecond)
	}
	r1, err := ex.ReservePortalOfLife()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	defer r1.CancelPortalOfLifeWork()
	if _, err := ex.ReservePortalOfLife(); !errors.Is(err, ErrPortalExecutorQueueFull) {
		t.Fatalf("second Reserve = %v, want QueueFull", err)
	}
	// Second concurrent Run is AlreadyRunning; after exit a sequential
	// Run is Shutdown (one-shot lifecycle).
	if err := ex.Run(context.Background()); !errors.Is(err, ErrPortalExecutorAlreadyRunning) {
		t.Fatalf("concurrent Run = %v, want AlreadyRunning", err)
	}
	cancel()
	<-runErr
	if err := ex.Run(context.Background()); !errors.Is(err, ErrPortalExecutorShutdown) {
		t.Fatalf("sequential Run = %v, want Shutdown", err)
	}
}

func TestPortalReservationAccessors(t *testing.T) {
	s := mustSaverForPersist(t)
	portalTestTrack(t, s, 7, 5)
	fs := newPortalFakeStore()
	sink := &portalFakeSink{}
	ex, _ := startPortalExecutor(t, portalTestConfig(fs, s, sink))
	r, err := ex.ReservePortalOfLife()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if _, err := r.Result(); !errors.Is(err, ErrPortalReservationNotPrepared) {
		t.Fatalf("Result before Prepare = %v, want NotPrepared", err)
	}
	if err := r.ActivatePortalOfLifeWork(); !errors.Is(err, ErrPortalReservationNotPrepared) {
		t.Fatalf("Activate before Prepare = %v, want NotPrepared", err)
	}
	if err := r.PreparePortalOfLifeWork(portalTestCapture(t)); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	ch1, err := r.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	ch2, err := r.Result()
	if err != nil {
		t.Fatalf("Result again: %v", err)
	}
	// Same channel every time: the single buffered terminal result
	// is consumable through either handle exactly once (proving both
	// handles name the same channel), and Cancel stays idempotent.
	r.CancelPortalOfLifeWork()
	select {
	case res := <-ch1:
		if !errors.Is(res.Err, ErrPortalReservationCanceled) {
			t.Fatalf("cancel result = %+v, want Canceled", res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no cancel result")
	}
	select {
	case res := <-ch2:
		t.Fatalf("second handle got %+v: channels differ (two buffered results)", res)
	default:
	}
	r.CancelPortalOfLifeWork()
	if err := r.ActivatePortalOfLifeWork(); !errors.Is(err, ErrPortalReservationCanceled) {
		t.Fatalf("Activate after Cancel = %v, want Canceled", err)
	}
	// Repeated Activate after success is a no-op without second
	// publication.
	r2, err := ex.ReservePortalOfLife()
	if err != nil {
		t.Fatalf("Reserve2: %v", err)
	}
	defer r2.CancelPortalOfLifeWork()
	if err := r2.PreparePortalOfLifeWork(portalTestCapture(t)); err != nil {
		t.Fatalf("Prepare2: %v", err)
	}
	fs.onPortal = echoPortalSuccess(5)
	if err := r2.ActivatePortalOfLifeWork(); err != nil {
		t.Fatalf("Activate2: %v", err)
	}
	if err := r2.ActivatePortalOfLifeWork(); err != nil {
		t.Fatalf("repeat Activate = %v, want nil no-op", err)
	}
	ch, _ := r2.Result()
	out := awaitPortalResult(t, ch)
	if out.Err != nil {
		t.Fatalf("result = %+v", out)
	}
	if fs.portalCalls != 1 {
		t.Fatalf("Store calls = %d, want 1 (no duplicate publication)", fs.portalCalls)
	}
}

// Cancel/Activate/Result race stress: no double permit release, no
// double publication, no panic, race-clean, permits balanced.
func TestPortalReservationRaceStress(t *testing.T) {
	s := mustSaverForPersist(t)
	portalTestTrack(t, s, 7, 5)
	fs := newPortalFakeStore()
	fs.onPortal = echoPortalSuccess(5)
	sink := &portalFakeSink{}
	ex, cancel := startPortalExecutor(t, PortalExecutorConfig{
		Workers: 2, QueueCapacity: 4, Store: fs, Saver: s, Sink: sink,
		RetryDelay: time.Millisecond, AbortTimeout: 5 * time.Second,
	})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				r, err := ex.ReservePortalOfLife()
				if err != nil {
					continue
				}
				_ = r.PreparePortalOfLifeWork(portalTestCapture(t))
				if (g+i)%2 == 0 {
					_ = r.ActivatePortalOfLifeWork()
				} else {
					r.CancelPortalOfLifeWork()
				}
				if ch, err := r.Result(); err == nil {
					select {
					case <-ch:
					case <-time.After(10 * time.Second):
					}
				}
				r.CancelPortalOfLifeWork()
			}
		}(g)
	}
	wg.Wait()
	cancel()
	// Drain: every activated job either executed (permit released at
	// dequeue) or was drained (permit released in Run). Cancelled
	// reservations released theirs. The count must balance exactly.
	deadline := time.Now().Add(10 * time.Second)
	for {
		ex.mu.Lock()
		permits := ex.permits
		ex.mu.Unlock()
		if permits == 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("permits = %d, want 4 (balanced)", permits)
		}
		time.Sleep(time.Millisecond)
	}
}

// ---- production-path tripwire ----------------------------------

// The d2b2 production path MUST NOT reconcile through
// ReconcileSaver/ResolveReconciled: proven lost-ack returns E+1
// inside the held-gate callback while preserving newer snapshots.
// Full-line comments may name the forbidden rule; production CODE
// (and string literals) must not contain it.
func TestPortalNoReconcileOnProvenPath(t *testing.T) {
	path := "death_portal.go"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var code []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		code = append(code, line)
	}
	body := strings.Join(code, "\n")
	for _, banned := range []string{"ReconcileSaver", "ResolveReconciled", "NewReconcileState"} {
		if strings.Contains(body, banned) {
			t.Fatalf("production %s contains %q: proven Portal lost-ack must not reconcile", path, banned)
		}
	}
}
