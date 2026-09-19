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

// M5-T5c3d3b bounded penalty persistence executor unit tests
// (spec §9.5.1k, frozen v0.3.57): Store-domain penalty mapper
// with raw pending-cost semantics, bounded executor with
// Reserve-time queue-permit + Saver-critical-slot ownership,
// CPU-only Prepare, exactly-one reserved Execute, at-most-once
// Store, in-critical-callback read-only lost-ack proof
// (E+1 + exact content + Pending == nil), normal/proven-lost-ack
// completion, definitive-pre-Store retryable notification, and
// bounded same-mailbox redelivery. No PG here (real-PG proofs
// live in death_penalty_pg_test.go); the Saver is always real,
// Store and the owner sink are deterministic fakes.

// ---- fakes -----------------------------------------------------

// penaltyFakeStore scripts CommitDeathPenalties (via the
// embedded T5c2b fake) and serves deterministic read-only
// character recovery. Call counts prove at-most-once Store and
// exactly-once in-callback recovery. An optional hook gates the
// recovery load for deterministic MarkDirty interleaving.
type penaltyFakeStore struct {
	*fakeDeathStore
	recSnap  store.DeathCharacterRecoverySnapshot
	recErr   error
	recCalls int
	recHook  func()
}

var _ PenaltyExecutionStore = (*penaltyFakeStore)(nil)

func newPenaltyFakeStore() *penaltyFakeStore {
	return &penaltyFakeStore{fakeDeathStore: &fakeDeathStore{}}
}

func (f *penaltyFakeStore) LoadDeathCharacterRecovery(
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

// penaltyFakeSink scripts the typed d3a owner ingress and
// records every delivery for completion/retryable/redelivery
// proof.
type penaltyFakeSink struct {
	mu           sync.Mutex
	completions  []sim.DeathPenaltyCompletion
	retryables   []sim.DeathPenaltyAttemptToken
	onCompletion func(n int, c sim.DeathPenaltyCompletion) (sim.DeathPenaltyCompletionDisposition, error)
	onRetryable  func(n int, tok sim.DeathPenaltyAttemptToken) (sim.DeathPenaltyRetryDisposition, error)
}

var _ PenaltyOwnerSink = (*penaltyFakeSink)(nil)

func (f *penaltyFakeSink) EnqueueDeathPenaltyCompletion(
	_ context.Context, c sim.DeathPenaltyCompletion,
) (sim.DeathPenaltyCompletionDisposition, error) {
	f.mu.Lock()
	f.completions = append(f.completions, c)
	n := len(f.completions)
	on := f.onCompletion
	f.mu.Unlock()
	if on != nil {
		return on(n, c)
	}
	return sim.DeathPenaltyCompletionApplied, nil
}

func (f *penaltyFakeSink) EnqueueDeathPenaltyPersistenceRetryable(
	_ context.Context, tok sim.DeathPenaltyAttemptToken,
) (sim.DeathPenaltyRetryDisposition, error) {
	f.mu.Lock()
	f.retryables = append(f.retryables, tok)
	n := len(f.retryables)
	on := f.onRetryable
	f.mu.Unlock()
	if on != nil {
		return on(n, tok)
	}
	return sim.DeathPenaltyRetryApplied, nil
}

func (f *penaltyFakeSink) numCompletions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.completions)
}

func (f *penaltyFakeSink) numRetryables() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.retryables)
}

// penaltyRecordProvider composes the real executor as the d3a
// provider while recording every concrete reservation, so tests
// driving the real owner orchestration can await the bounded
// result channel afterwards.
type penaltyRecordProvider struct {
	ex           *PenaltyExecutor
	mu           sync.Mutex
	reservations []sim.DeathPenaltyWorkReservation
}

func (p *penaltyRecordProvider) ReserveDeathPenaltyWork(id sim.CharacterID) (sim.DeathPenaltyWorkReservation, error) {
	r, err := p.ex.ReserveDeathPenaltyWork(id)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.reservations = append(p.reservations, r)
	p.mu.Unlock()
	return r, nil
}

func (p *penaltyRecordProvider) last(t *testing.T) *PenaltyExecutionReservation {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.reservations) == 0 {
		t.Fatal("no reservations recorded")
	}
	conc, ok := p.reservations[len(p.reservations)-1].(*PenaltyExecutionReservation)
	if !ok {
		t.Fatalf("reservation type %T, want *PenaltyExecutionReservation", p.reservations[len(p.reservations)-1])
	}
	return conc
}

// ---- fixtures --------------------------------------------------

var errPenaltySynth = errors.New("test: synthetic post-commit error")

// penaltyTestVitals is valid gameplay vitals for penalty
// capture.
func penaltyTestVitals(t *testing.T) sim.PlayerVitals {
	t.Helper()
	v, err := sim.NewPlayerVitals(25)
	if err != nil {
		t.Fatalf("NewPlayerVitals: %v", err)
	}
	return v
}

// penaltyTestDurable is catalog-valid durable state: spell IDs
// 1-2, skill ID 1. Flags 0x1274 carry neither MURDERER nor
// TUTORIAL, so the T5a planner takes the still-newbie scaled
// branch (cost/3, no HP roll).
func penaltyTestDurable() sim.PlayerDurableState {
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

// penaltyTestPending is the canonical pending fixture: raw cost
// 90, death time 100, corpse 500, unused.
func penaltyTestPending() sim.PendingDeathRuntime {
	corpse := int64(500)
	return sim.PendingDeathRuntime{EffectiveCost: 90, DeathTimeSeconds: 100, CorpseID: &corpse}
}

// penaltyTestCapture builds a valid standalone capture with raw
// pending cost 90 and a deliberately different plan scaled cost
// 30, pinning the raw-vs-scaled mapper contract.
func penaltyTestCapture(t *testing.T) sim.DeathPenaltyCapture {
	t.Helper()
	pending := penaltyTestPending()
	return sim.DeathPenaltyCapture{
		Token:         sim.DeathPenaltyAttemptToken{EntityID: 1, CharacterID: 7, Epoch: 1},
		Position:      world.Vec3{X: 1, Y: 0, Z: 1},
		Vitals:        penaltyTestVitals(t),
		Durable:       penaltyTestDurable(),
		PendingBefore: pending,
		Plan:          sim.DeathPenaltyPlan{ScaledCost: 30, ClearPending: true, VitalsAfter: penaltyTestVitals(t)},
	}
}

// penaltyOrchestrationEngine builds a real sim engine with one
// Alive player carrying the canonical pending (cost 90) via
// authoritative hydration.
func penaltyOrchestrationEngine(t *testing.T, charID int64) (*sim.Engine, sim.EntityID) {
	t.Helper()
	e := mustSimEngine(t)
	snap, err := e.AddPlayerEntityWithDurableState(sim.CharacterID(charID),
		world.Vec3{X: 1, Y: 0, Z: 1}, penaltyTestVitals(t), testDeathRuntimeInputs(t), penaltyTestDurable())
	if err != nil {
		t.Fatalf("AddPlayerEntityWithDurableState: %v", err)
	}
	pending := penaltyTestPending()
	if err := e.PlayerInstallRecoveredPendingDeath(snap.ID, &pending); err != nil {
		t.Fatalf("PlayerInstallRecoveredPendingDeath: %v", err)
	}
	return e, snap.ID
}

func penaltyOrchestrationInput() sim.UnderworldExitResolvedInput {
	return sim.UnderworldExitResolvedInput{DefaultDeathCost: 100}
}

// penaltyTestTrack tracks the penalty character root at the
// given Saver revision.
func penaltyTestTrack(t *testing.T, s *sim.Saver, charID, rev int64) {
	t.Helper()
	if err := s.Track(sim.AggregateKey{Kind: sim.AggregateCharacter, ID: charID}, rev); err != nil {
		t.Fatal(err)
	}
}

// startPenaltyExecutor runs the executor and returns it with a
// cancel that also waits for Run to exit (no leak across
// -count=50). It spins until Run owns the executor (no pacing
// sleep on the hot path; the deadline only fails loudly on
// bugs).
func startPenaltyExecutor(
	t *testing.T, cfg PenaltyExecutorConfig,
) (*PenaltyExecutor, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ex, err := NewPenaltyExecutor(cfg)
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
			t.Fatal("timed out waiting for penalty executor Run")
		}
		time.Sleep(time.Millisecond)
	}
	t.Cleanup(func() {
		cancel()
		<-runErr
	})
	return ex, cancel
}

// awaitPenaltyResult waits for a result with a failure-mode
// timeout (never a pacing sleep: results are buffered, delivery
// is prompt; the timeout only fails loudly on bugs).
func awaitPenaltyResult(t *testing.T, ch <-chan PenaltyPersistenceResult) PenaltyPersistenceResult {
	t.Helper()
	select {
	case res := <-ch:
		return res
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for penalty result")
		return PenaltyPersistenceResult{}
	}
}

// mustReservePenalty reserves through the interface and asserts
// the concrete reservation type.
func mustReservePenalty(t *testing.T, ex *PenaltyExecutor, charID int64) *PenaltyExecutionReservation {
	t.Helper()
	r, err := ex.ReserveDeathPenaltyWork(sim.CharacterID(charID))
	if err != nil {
		t.Fatalf("Reserve(%d): %v", charID, err)
	}
	conc, ok := r.(*PenaltyExecutionReservation)
	if !ok {
		t.Fatalf("reservation type %T, want *PenaltyExecutionReservation", r)
	}
	return conc
}

// penaltyTestConfig builds a running-capable executor config
// over the given fakes with a real Saver.
func penaltyTestConfig(fs *penaltyFakeStore, s *sim.Saver, sink *penaltyFakeSink) PenaltyExecutorConfig {
	return PenaltyExecutorConfig{
		Workers: 1, QueueCapacity: 4, Store: fs, Saver: s, Sink: sink,
		RetryDelay: time.Millisecond, RetryTimeout: 5 * time.Second,
	}
}

// echoPenaltySuccess returns onPenalty behavior echoing E+1.
func echoPenaltySuccess() func(store.DeathPenaltiesRequest) (store.DeathPenaltiesResult, error) {
	return func(req store.DeathPenaltiesRequest) (store.DeathPenaltiesResult, error) {
		return store.DeathPenaltiesResult{CharacterRevision: req.Character.ExpectedRevision + 1}, nil
	}
}

// provenPenaltyRec builds the recovered character snapshot
// proving the given intended request at exactly rev with the
// pending row deleted (Pending == nil).
func provenPenaltyRec(req store.DeathPenaltiesRequest, rev int64) store.DeathCharacterRecoverySnapshot {
	c := freezeCharacterSnapshot(req.Character)
	c.ExpectedRevision = rev
	return store.DeathCharacterRecoverySnapshot{Character: c, Pending: nil}
}

// ---- mapper ----------------------------------------------------

// Exact complete Character mapping: ID, placeholder revision,
// karma/flags, spells/skills, mm position, vitals JSON,
// advancement bytes, raw pending cost, plus deep-copy
// ownership. The pending 90 vs scaled 30 split pins
// ExpectedPendingCost = raw durable cost.
func TestMapDeathPenaltyCaptureExact(t *testing.T) {
	capture := penaltyTestCapture(t)
	req, err := MapDeathPenaltyCapture(capture)
	if err != nil {
		t.Fatalf("MapDeathPenaltyCapture: %v", err)
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
	if req.Character.Spells[1].SpellID != 2 || req.Character.Spells[1].Ability != 60 || !req.Character.Spells[1].AtrophyFlag {
		t.Fatalf("spells[1] = %+v, want spell 2@60 atrophy", req.Character.Spells[1])
	}
	if len(req.Character.Skills) != 1 || req.Character.Skills[0].SkillID != 1 || req.Character.Skills[0].Ability != 40 {
		t.Fatalf("skills = %+v, want 1 with skill 1@40", req.Character.Skills)
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
	if req.ExpectedPendingCost != 90 {
		t.Fatalf("ExpectedPendingCost = %d, want raw pending 90 (not scaled 30)", req.ExpectedPendingCost)
	}
	// Deep-copy ownership: mutate the caller capture, request
	// keeps original frozen values.
	capture.Durable.Advancement[0] = 'X'
	capture.Durable.Spells[0].Ability = 99
	capture.Durable.Skills[0].Ability = 99
	*capture.PendingBefore.CorpseID = 999
	capture.PendingBefore.EffectiveCost = 1
	capture.Vitals.HP = 1
	if req.Character.Advancement[0] == 'X' {
		t.Fatal("request aliases Advancement")
	}
	if req.Character.Spells[0].Ability == 99 || req.Character.Skills[0].Ability == 99 {
		t.Fatal("request aliases Spells/Skills")
	}
	if req.ExpectedPendingCost != 90 {
		t.Fatal("request aliases pending cost")
	}
	vitalsJSON, err := json.Marshal(capture.Vitals)
	if err != nil {
		t.Fatalf("vitals marshal: %v", err)
	}
	if string(req.Character.Vitals) == string(vitalsJSON) {
		t.Fatal("request aliases Vitals")
	}
}

// Raw pending cost versus scaled plan cost: PendingBefore 90
// with plan ScaledCost 30 maps ExpectedPendingCost 90. A
// mutated scale (including 0, e.g. frenzy) never leaks in.
func TestMapDeathPenaltyRawCostVsScaled(t *testing.T) {
	for _, scaled := range []int{30, 0, 90, 5} {
		capture := penaltyTestCapture(t)
		capture.Plan.ScaledCost = scaled
		req, err := MapDeathPenaltyCapture(capture)
		if err != nil {
			t.Fatalf("scaled=%d: MapDeathPenaltyCapture: %v", scaled, err)
		}
		if req.ExpectedPendingCost != 90 {
			t.Fatalf("scaled=%d: ExpectedPendingCost = %d, want raw 90", scaled, req.ExpectedPendingCost)
		}
	}
}

// Nil CorpseID and both PortalUsed values are legal for
// Underworld exit: the mapper accepts all four combinations.
func TestMapDeathPenaltyPendingShapes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		corpseNil  bool
		portalUsed bool
	}{
		{"nil-unused", true, false},
		{"nil-used", true, true},
		{"set-unused", false, false},
		{"set-used", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := penaltyTestCapture(t)
			if tc.corpseNil {
				capture.PendingBefore.CorpseID = nil
			}
			capture.PendingBefore.PortalUsed = tc.portalUsed
			req, err := MapDeathPenaltyCapture(capture)
			if err != nil {
				t.Fatalf("MapDeathPenaltyCapture: %v", err)
			}
			if req.ExpectedPendingCost != 90 {
				t.Fatalf("ExpectedPendingCost = %d, want 90", req.ExpectedPendingCost)
			}
		})
	}
}

func TestMapDeathPenaltyCaptureRejects(t *testing.T) {
	base := penaltyTestCapture(t)
	cases := []struct {
		name string
		f    func(sim.DeathPenaltyCapture) sim.DeathPenaltyCapture
	}{
		{"zero entity", func(c sim.DeathPenaltyCapture) sim.DeathPenaltyCapture { c.Token.EntityID = 0; return c }},
		{"zero character", func(c sim.DeathPenaltyCapture) sim.DeathPenaltyCapture { c.Token.CharacterID = 0; return c }},
		{"negative character", func(c sim.DeathPenaltyCapture) sim.DeathPenaltyCapture { c.Token.CharacterID = -3; return c }},
		{"zero epoch", func(c sim.DeathPenaltyCapture) sim.DeathPenaltyCapture { c.Token.Epoch = 0; return c }},
		{"nan position", func(c sim.DeathPenaltyCapture) sim.DeathPenaltyCapture { c.Position.X = math.NaN(); return c }},
		{"inf position", func(c sim.DeathPenaltyCapture) sim.DeathPenaltyCapture { c.Position.Z = math.Inf(1); return c }},
		{"bad vitals", func(c sim.DeathPenaltyCapture) sim.DeathPenaltyCapture { c.Vitals.HP = -1; return c }},
		{"empty advancement", func(c sim.DeathPenaltyCapture) sim.DeathPenaltyCapture { c.Durable.Advancement = nil; return c }},
		{"invalid advancement", func(c sim.DeathPenaltyCapture) sim.DeathPenaltyCapture {
			c.Durable.Advancement = []byte(`{oops`)
			return c
		}},
		{"bad spell id", func(c sim.DeathPenaltyCapture) sim.DeathPenaltyCapture { c.Durable.Spells[0].ID = 0; return c }},
		{"spell id over catalog", func(c sim.DeathPenaltyCapture) sim.DeathPenaltyCapture { c.Durable.Spells[0].ID = 65536; return c }},
		{"bad spell ability", func(c sim.DeathPenaltyCapture) sim.DeathPenaltyCapture { c.Durable.Spells[0].Ability = 100; return c }},
		{"zero spell ability", func(c sim.DeathPenaltyCapture) sim.DeathPenaltyCapture { c.Durable.Spells[0].Ability = 0; return c }},
		{"dup spell", func(c sim.DeathPenaltyCapture) sim.DeathPenaltyCapture {
			c.Durable.Spells = append(c.Durable.Spells, c.Durable.Spells[0])
			return c
		}},
		{"bad skill id", func(c sim.DeathPenaltyCapture) sim.DeathPenaltyCapture { c.Durable.Skills[0].ID = 70000; return c }},
		{"bad skill ability", func(c sim.DeathPenaltyCapture) sim.DeathPenaltyCapture { c.Durable.Skills[0].Ability = 0; return c }},
		{"dup skill", func(c sim.DeathPenaltyCapture) sim.DeathPenaltyCapture {
			c.Durable.Skills = append(c.Durable.Skills, c.Durable.Skills[0])
			return c
		}},
		{"bad pending cost", func(c sim.DeathPenaltyCapture) sim.DeathPenaltyCapture { c.PendingBefore.EffectiveCost = 101; return c }},
		{"negative pending cost", func(c sim.DeathPenaltyCapture) sim.DeathPenaltyCapture { c.PendingBefore.EffectiveCost = -1; return c }},
		{"bad pending time", func(c sim.DeathPenaltyCapture) sim.DeathPenaltyCapture {
			c.PendingBefore.DeathTimeSeconds = -1
			return c
		}},
		{"zero corpse pointer", func(c sim.DeathPenaltyCapture) sim.DeathPenaltyCapture {
			v := int64(0)
			c.PendingBefore.CorpseID = &v
			return c
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := MapDeathPenaltyCapture(tc.f(base))
			if err == nil {
				t.Fatalf("MapDeathPenaltyCapture accepted %s", tc.name)
			}
			if !reflect.DeepEqual(req, store.DeathPenaltiesRequest{}) {
				t.Fatalf("error without zero request for %s: %+v", tc.name, req)
			}
		})
	}
}

// Cross-namespace IDs are independent: spell 5 + skill 5 is
// valid and both map completely.
func TestMapDeathPenaltyCrossNamespace(t *testing.T) {
	capture := penaltyTestCapture(t)
	capture.Durable.Spells = []sim.PlayerAbilityState{{ID: 5, Ability: 70}}
	capture.Durable.Skills = []sim.PlayerAbilityState{{ID: 5, Ability: 71}}
	req, err := MapDeathPenaltyCapture(capture)
	if err != nil {
		t.Fatalf("MapDeathPenaltyCapture: %v", err)
	}
	if len(req.Character.Spells) != 1 || req.Character.Spells[0].SpellID != 5 || req.Character.Spells[0].Ability != 70 {
		t.Fatalf("spells = %+v", req.Character.Spells)
	}
	if len(req.Character.Skills) != 1 || req.Character.Skills[0].SkillID != 5 || req.Character.Skills[0].Ability != 71 {
		t.Fatalf("skills = %+v", req.Character.Skills)
	}
}

// ---- reserve owns queue + Saver slot ---------------------------

// Capacity 1: one reserved penalty holds the queue permit AND
// the Saver gate before any owner RNG; Cancel returns both.
func TestPenaltyReserveOwnsQueueAndSaverSlot(t *testing.T) {
	s := mustSaverForPersist(t)
	penaltyTestTrack(t, s, 7, 5)
	fs := newPenaltyFakeStore()
	sink := &penaltyFakeSink{}
	ex, _ := startPenaltyExecutor(t, PenaltyExecutorConfig{
		Workers: 1, QueueCapacity: 1, Store: fs, Saver: s, Sink: sink,
		RetryDelay: time.Millisecond, RetryTimeout: 5 * time.Second,
	})

	r, err := ex.ReserveDeathPenaltyWork(7)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	conc := r.(*PenaltyExecutionReservation)
	if _, err := ex.ReserveDeathPenaltyWork(7); !errors.Is(err, ErrPenaltyExecutorQueueFull) {
		t.Fatalf("second Reserve = %v, want QueueFull (permit held)", err)
	}
	if _, err := s.ReserveCriticalSet([]sim.AggregateKey{{Kind: sim.AggregateCharacter, ID: 7}}); !errors.Is(err, sim.ErrCriticalSetReservationBusy) {
		t.Fatalf("Saver Reserve = %v, want Busy (slot held)", err)
	}
	// Ordinary WriteThrough cannot overtake the held gate: it
	// blocks until the reservation releases it.
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
		t.Fatal("WriteThrough overtook the held penalty Saver gate")
	case <-time.After(50 * time.Millisecond):
	}
	conc.CancelDeathPenaltyWork()
	select {
	case <-invoked:
	case <-time.After(10 * time.Second):
		t.Fatal("WriteThrough did not proceed after Cancel")
	}
	if err := <-done; err != nil {
		t.Fatalf("WriteThrough after Cancel: %v", err)
	}
	if fs.penaltyCalls != 0 {
		t.Fatalf("Store calls = %d, want 0", fs.penaltyCalls)
	}
	if sink.numCompletions() != 0 || sink.numRetryables() != 0 {
		t.Fatalf("sink traffic completions=%d retryables=%d, want 0/0",
			sink.numCompletions(), sink.numRetryables())
	}
	// Both queue capacity and the Saver gate returned.
	r2, err := ex.ReserveDeathPenaltyWork(7)
	if err != nil {
		t.Fatalf("Reserve after Cancel: %v", err)
	}
	r2.(*PenaltyExecutionReservation).CancelDeathPenaltyWork()
	probe, err := s.ReserveCriticalSet([]sim.AggregateKey{{Kind: sim.AggregateCharacter, ID: 7}})
	if err != nil {
		t.Fatalf("Saver Reserve after Cancel: %v", err)
	}
	probe.Cancel()
}

// A Saver-busy Reserve fails before RNG with the queue permit
// released: a replacement reservation succeeds immediately once
// the gate frees. Failed Reserve publishes nothing, calls Store
// zero times, and leaks no capacity across repeated failures.
func TestPenaltyReserveSaverBusyReleasesPermit(t *testing.T) {
	s := mustSaverForPersist(t)
	penaltyTestTrack(t, s, 7, 5)
	holder, err := s.ReserveCriticalSet([]sim.AggregateKey{{Kind: sim.AggregateCharacter, ID: 7}})
	if err != nil {
		t.Fatalf("hold gate: %v", err)
	}
	fs := newPenaltyFakeStore()
	sink := &penaltyFakeSink{}
	ex, _ := startPenaltyExecutor(t, PenaltyExecutorConfig{
		Workers: 1, QueueCapacity: 1, Store: fs, Saver: s, Sink: sink,
		RetryDelay: time.Millisecond, RetryTimeout: 5 * time.Second,
	})
	for i := 0; i < 20; i++ {
		if _, err := ex.ReserveDeathPenaltyWork(7); !errors.Is(err, sim.ErrCriticalSetReservationBusy) {
			t.Fatalf("failure %d: Reserve = %v, want Busy", i, err)
		}
	}
	if fs.penaltyCalls != 0 {
		t.Fatalf("Store calls = %d, want 0", fs.penaltyCalls)
	}
	if sink.numCompletions() != 0 || sink.numRetryables() != 0 {
		t.Fatalf("sink traffic completions=%d retryables=%d, want 0/0",
			sink.numCompletions(), sink.numRetryables())
	}
	holder.Cancel()
	r, err := ex.ReserveDeathPenaltyWork(7)
	if err != nil {
		t.Fatalf("Reserve after gate freed: %v (capacity leaked)", err)
	}
	r.(*PenaltyExecutionReservation).CancelDeathPenaltyWork()
	r3, err := ex.ReserveDeathPenaltyWork(7)
	if err != nil {
		t.Fatalf("Reserve after Cancel: %v", err)
	}
	r3.(*PenaltyExecutionReservation).CancelDeathPenaltyWork()
}

// A reconcile-blocked Saver Reserve fails with the queue permit
// released: reserving a different clean character succeeds on
// the same capacity-1 executor.
func TestPenaltyReserveSaverBlockedReleasesPermit(t *testing.T) {
	s := mustSaverForPersist(t)
	penaltyTestTrack(t, s, 7, 5)
	penaltyTestTrack(t, s, 8, 9)
	// Block character 7 via a failing WriteCriticalSet (no Store
	// involved): the entry stays reconcile-blocked.
	_, err := s.WriteCriticalSet(context.Background(),
		[]sim.AggregateKey{{Kind: sim.AggregateCharacter, ID: 7}},
		func(context.Context, []sim.AggregateRevision) ([]sim.AggregateRevision, error) {
			return nil, errors.New("test: boom")
		})
	if err == nil {
		t.Fatal("WriteCriticalSet accepted failing callback")
	}
	fs := newPenaltyFakeStore()
	sink := &penaltyFakeSink{}
	ex, _ := startPenaltyExecutor(t, PenaltyExecutorConfig{
		Workers: 1, QueueCapacity: 1, Store: fs, Saver: s, Sink: sink,
		RetryDelay: time.Millisecond, RetryTimeout: 5 * time.Second,
	})
	if _, err := ex.ReserveDeathPenaltyWork(7); !errors.Is(err, sim.ErrSaverReconcileRequired) {
		t.Fatalf("Reserve blocked = %v, want ReconcileRequired", err)
	}
	// The permit returned: a clean character reserves fine.
	r, err := ex.ReserveDeathPenaltyWork(8)
	if err != nil {
		t.Fatalf("Reserve clean character: %v (permit leaked)", err)
	}
	r.(*PenaltyExecutionReservation).CancelDeathPenaltyWork()
	if fs.penaltyCalls != 0 {
		t.Fatalf("Store calls = %d, want 0", fs.penaltyCalls)
	}
}

func TestPenaltyReserveValidation(t *testing.T) {
	s := mustSaverForPersist(t)
	penaltyTestTrack(t, s, 7, 5)
	fs := newPenaltyFakeStore()
	sink := &penaltyFakeSink{}
	ex, err := NewPenaltyExecutor(PenaltyExecutorConfig{
		Workers: 1, QueueCapacity: 1, Store: fs, Saver: s, Sink: sink,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := ex.ReserveDeathPenaltyWork(7); !errors.Is(err, ErrPenaltyExecutorNotRunning) {
		t.Fatalf("Reserve stopped = %v, want NotRunning", err)
	}
	if _, err := ex.ReserveDeathPenaltyWork(0); !errors.Is(err, ErrPenaltyExecutorInvalid) {
		t.Fatalf("Reserve char 0 = %v, want Invalid", err)
	}
	if _, err := ex.ReserveDeathPenaltyWork(-3); !errors.Is(err, ErrPenaltyExecutorInvalid) {
		t.Fatalf("Reserve char -3 = %v, want Invalid", err)
	}
}

// Reserve through the real d3a owner happens before RNG: the
// owner consumes zero RNG and mutates nothing on Reserve
// failure.
func TestPenaltyReserveBeforeOwnerRNG(t *testing.T) {
	s := mustSaverForPersist(t)
	penaltyTestTrack(t, s, 7, 5)
	holder, err := s.ReserveCriticalSet([]sim.AggregateKey{{Kind: sim.AggregateCharacter, ID: 7}})
	if err != nil {
		t.Fatalf("hold gate: %v", err)
	}
	defer holder.Cancel()
	fs := newPenaltyFakeStore()
	sink := &penaltyFakeSink{}
	ex, _ := startPenaltyExecutor(t, penaltyTestConfig(fs, s, sink))
	e, id := penaltyOrchestrationEngine(t, 7)
	rng := &captureTestRNG{}
	_, err = e.PlayerOrchestrateDeathPenalties(id, penaltyOrchestrationInput(), rng, ex)
	if !errors.Is(err, sim.ErrCriticalSetReservationBusy) {
		t.Fatalf("orchestrate err = %v, want Busy", err)
	}
	if rng.v != 0 {
		t.Fatalf("RNG draws = %d, want 0 (Reserve before RNG)", rng.v)
	}
	live, ok, lerr := e.PlayerPendingDeathOf(id)
	if lerr != nil || !ok || live.EffectiveCost != 90 {
		t.Fatalf("pending = %+v,%v,%v; want unchanged cost90", live, ok, lerr)
	}
	if fs.penaltyCalls != 0 {
		t.Fatalf("Store calls = %d, want 0", fs.penaltyCalls)
	}
}

// ---- prepare is CPU-only ---------------------------------------

// Prepare acquires neither Saver capacity nor queue capacity: a
// capacity-2 executor with one reserved+prepared reservation
// still admits exactly one more Reserve, and the held gate is
// the reservation's own Reserve-time slot (Prepare succeeds
// while the gate is held, which a second acquisition would
// refuse with Busy).
func TestPenaltyPrepareAcquiresNothing(t *testing.T) {
	s := mustSaverForPersist(t)
	penaltyTestTrack(t, s, 7, 5)
	fs := newPenaltyFakeStore()
	sink := &penaltyFakeSink{}
	ex, _ := startPenaltyExecutor(t, PenaltyExecutorConfig{
		Workers: 1, QueueCapacity: 2, Store: fs, Saver: s, Sink: sink,
		RetryDelay: time.Millisecond, RetryTimeout: 5 * time.Second,
	})
	r := mustReservePenalty(t, ex, 7)
	if err := r.PrepareDeathPenaltyWork(penaltyTestCapture(t)); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	r2, err := ex.ReserveDeathPenaltyWork(7)
	if err == nil {
		r2.(*PenaltyExecutionReservation).CancelDeathPenaltyWork()
		t.Fatal("second Reserve admitted while gate held (Prepare must not free the Reserve slot)")
	}
	if !errors.Is(err, sim.ErrCriticalSetReservationBusy) {
		t.Fatalf("second Reserve = %v, want Busy (own slot still held)", err)
	}
	// The queue permit of r is still outstanding, so capacity 2
	// admits one reservation for a clean character.
	penaltyTestTrack(t, s, 8, 1)
	r3, err := ex.ReserveDeathPenaltyWork(8)
	if err != nil {
		t.Fatalf("Reserve(8) = %v, want success (Prepare took no permit)", err)
	}
	r3.(*PenaltyExecutionReservation).CancelDeathPenaltyWork()
	r.CancelDeathPenaltyWork()
	if fs.penaltyCalls != 0 {
		t.Fatalf("Store calls = %d, want 0", fs.penaltyCalls)
	}
}

// Mapping failure releases/cancels owned resources exactly
// once: the Saver gate frees, the permit returns, Store stays
// zero, and the reservation is terminal.
func TestPenaltyPrepareMappingFailure(t *testing.T) {
	s := mustSaverForPersist(t)
	penaltyTestTrack(t, s, 7, 5)
	fs := newPenaltyFakeStore()
	sink := &penaltyFakeSink{}
	ex, _ := startPenaltyExecutor(t, PenaltyExecutorConfig{
		Workers: 1, QueueCapacity: 1, Store: fs, Saver: s, Sink: sink,
		RetryDelay: time.Millisecond, RetryTimeout: 5 * time.Second,
	})
	r := mustReservePenalty(t, ex, 7)
	bad := penaltyTestCapture(t)
	bad.Durable.Spells[0].Ability = 100
	if err := r.PrepareDeathPenaltyWork(bad); err == nil {
		t.Fatal("Prepare accepted bad capture")
	}
	if _, err := r.Result(); !errors.Is(err, ErrPenaltyReservationNotPrepared) {
		t.Fatalf("Result after failed Prepare = %v, want NotPrepared", err)
	}
	// No Saver slot held (gate free) and the permit returned.
	probe, err := s.ReserveCriticalSet([]sim.AggregateKey{{Kind: sim.AggregateCharacter, ID: 7}})
	if err != nil {
		t.Fatalf("Saver gate held after mapping failure: %v", err)
	}
	probe.Cancel()
	r2 := mustReservePenalty(t, ex, 7)
	r2.CancelDeathPenaltyWork()
	if fs.penaltyCalls != 0 {
		t.Fatalf("Store calls = %d, want 0", fs.penaltyCalls)
	}
	// Repeated Cancel after the failure terminal is safe.
	r.CancelDeathPenaltyWork()
	r.CancelDeathPenaltyWork()
}

// Character mismatch between Reserve and Prepare fails closed
// with resources released.
func TestPenaltyPrepareCharacterMismatch(t *testing.T) {
	s := mustSaverForPersist(t)
	penaltyTestTrack(t, s, 7, 5)
	fs := newPenaltyFakeStore()
	sink := &penaltyFakeSink{}
	ex, _ := startPenaltyExecutor(t, PenaltyExecutorConfig{
		Workers: 1, QueueCapacity: 1, Store: fs, Saver: s, Sink: sink,
		RetryDelay: time.Millisecond, RetryTimeout: 5 * time.Second,
	})
	r := mustReservePenalty(t, ex, 7)
	capture := penaltyTestCapture(t)
	capture.Token.CharacterID = 8
	if err := r.PrepareDeathPenaltyWork(capture); !errors.Is(err, ErrPenaltyExecutorInvalid) {
		t.Fatalf("Prepare mismatch = %v, want Invalid", err)
	}
	probe, err := s.ReserveCriticalSet([]sim.AggregateKey{{Kind: sim.AggregateCharacter, ID: 7}})
	if err != nil {
		t.Fatalf("Saver gate held after mismatch: %v", err)
	}
	probe.Cancel()
	mustReservePenalty(t, ex, 7).CancelDeathPenaltyWork()
	if fs.penaltyCalls != 0 {
		t.Fatalf("Store calls = %d, want 0", fs.penaltyCalls)
	}
}

// ---- activation ------------------------------------------------

// Prepare succeeds, executor stops before Activate: Shutdown
// error, Saver slot cancelled, permit returned, Store zero, no
// completion, no retryable. The d3a owner keeps its lock (its
// own contract); the executor half is proven here.
func TestPenaltyActivationShutdownBeforePublish(t *testing.T) {
	s := mustSaverForPersist(t)
	penaltyTestTrack(t, s, 7, 5)
	fs := newPenaltyFakeStore()
	sink := &penaltyFakeSink{}
	ctx, cancel := context.WithCancel(context.Background())
	ex, err := NewPenaltyExecutor(PenaltyExecutorConfig{
		Workers: 1, QueueCapacity: 2, Store: fs, Saver: s, Sink: sink,
		RetryDelay: time.Millisecond, RetryTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
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
			t.Fatal("Run did not start")
		}
		time.Sleep(time.Millisecond)
	}
	r := mustReservePenalty(t, ex, 7)
	if err := r.PrepareDeathPenaltyWork(penaltyTestCapture(t)); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	cancel()
	<-runErr
	if err := r.ActivateDeathPenaltyWork(); !errors.Is(err, ErrPenaltyExecutorShutdown) {
		t.Fatalf("Activate = %v, want Shutdown", err)
	}
	ch, err := r.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	out := awaitPenaltyResult(t, ch)
	if !errors.Is(out.Err, ErrPenaltyExecutorShutdown) {
		t.Fatalf("result err = %v, want Shutdown", out.Err)
	}
	if out.RetryNotified || sink.numRetryables() != 0 {
		t.Fatal("typed retryable sent for synchronous Activate failure (d3a owns this path)")
	}
	if sink.numCompletions() != 0 {
		t.Fatalf("completions = %d, want 0", sink.numCompletions())
	}
	if fs.penaltyCalls != 0 {
		t.Fatalf("Store calls = %d, want 0", fs.penaltyCalls)
	}
	probe, err := s.ReserveCriticalSet([]sim.AggregateKey{{Kind: sim.AggregateCharacter, ID: 7}})
	if err != nil {
		t.Fatalf("Saver gate still held: %v", err)
	}
	probe.Cancel()
}

// ---- normal success --------------------------------------------

// Full owner loop through the real d3a orchestration: raw cost
// 90 reaches Store (not scaled 30), one Store call, zero
// recovery loads, Saver known E+1, newer post-Reserve MarkDirty
// preserved, one Applied completion, Recovered=false.
func TestPenaltyNormalSuccess(t *testing.T) {
	s := mustSaverForPersist(t)
	penaltyTestTrack(t, s, 7, 5)
	fs := newPenaltyFakeStore()
	fs.onPenalty = echoPenaltySuccess()
	sink := &penaltyFakeSink{}
	ex, _ := startPenaltyExecutor(t, penaltyTestConfig(fs, s, sink))
	e, id := penaltyOrchestrationEngine(t, 7)
	prov := &penaltyRecordProvider{ex: ex}

	entered := make(chan struct{}, 1)
	proceed := make(chan struct{})
	fs.onPenalty = func(req store.DeathPenaltiesRequest) (store.DeathPenaltiesResult, error) {
		entered <- struct{}{}
		<-proceed
		return echoPenaltySuccess()(req)
	}
	res, err := e.PlayerOrchestrateDeathPenalties(id, penaltyOrchestrationInput(), &captureTestRNG{}, prov)
	if err != nil {
		t.Fatalf("orchestrate: %v", err)
	}
	if res.Token.Epoch != 1 {
		t.Fatalf("epoch = %d, want 1", res.Token.Epoch)
	}
	r := prov.last(t)
	ch, err := r.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	<-entered
	// Newer gameplay snapshot arrives while the penalty holds
	// the gate: it must survive success.
	mark := func(ctx context.Context, exp int64) (int64, error) { return exp + 1, nil }
	if err := s.MarkDirty(sim.AggregateKey{Kind: sim.AggregateCharacter, ID: 7}, mark); err != nil {
		t.Fatalf("MarkDirty: %v", err)
	}
	close(proceed)
	out := awaitPenaltyResult(t, ch)
	if out.Err != nil {
		t.Fatalf("result err = %v", out.Err)
	}
	if out.Recovered {
		t.Fatal("Recovered = true, want false (normal ack)")
	}
	if out.Delivery != sim.DeathPenaltyCompletionApplied {
		t.Fatalf("delivery = %d, want Applied", uint8(out.Delivery))
	}
	if fs.penaltyCalls != 1 {
		t.Fatalf("Store calls = %d, want 1", fs.penaltyCalls)
	}
	if fs.recCalls != 0 {
		t.Fatalf("recovery loads = %d, want 0", fs.recCalls)
	}
	gotReq := fs.gotPenalty[0]
	if gotReq.ExpectedPendingCost != 90 {
		t.Fatalf("Store ExpectedPendingCost = %d, want raw 90", gotReq.ExpectedPendingCost)
	}
	if gotReq.Character.ExpectedRevision != 5 {
		t.Fatalf("Store ExpectedRevision = %d, want execution-time 5", gotReq.Character.ExpectedRevision)
	}
	got := inspectKnown(t, s, sim.AggregateCharacter, 7)
	if got.KnownRevision != 6 || got.Blocked {
		t.Fatalf("saver = %+v, want known6 clean", got)
	}
	if !got.Dirty {
		t.Fatal("newer post-Reserve MarkDirty was superseded, want Dirty preserved")
	}
	if sink.numCompletions() != 1 {
		t.Fatalf("completions = %d, want 1", sink.numCompletions())
	}
	c := sink.completions[0]
	if c.Token != res.Token {
		t.Fatalf("completion token = %+v, want %+v", c.Token, res.Token)
	}
	// The delivered completion is genuinely consumable by the
	// d3a owner: Applied with pending cleared, back to Alive.
	if disp, err := e.PlayerAcceptDeathPenaltyCompletion(c); err != nil || disp != sim.DeathPenaltyCompletionApplied {
		t.Fatalf("owner apply = %d,%v; want Applied,nil", disp, err)
	}
	if sink.numRetryables() != 0 {
		t.Fatalf("retryables = %d, want 0", sink.numRetryables())
	}
}

// Duplicate owner completion is executor success with no Store
// replay.
func TestPenaltyDuplicateOwnerCompletion(t *testing.T) {
	s := mustSaverForPersist(t)
	penaltyTestTrack(t, s, 7, 5)
	fs := newPenaltyFakeStore()
	fs.onPenalty = echoPenaltySuccess()
	sink := &penaltyFakeSink{}
	sink.onCompletion = func(n int, c sim.DeathPenaltyCompletion) (sim.DeathPenaltyCompletionDisposition, error) {
		return sim.DeathPenaltyCompletionDuplicate, nil
	}
	ex, _ := startPenaltyExecutor(t, penaltyTestConfig(fs, s, sink))
	e, id := penaltyOrchestrationEngine(t, 7)
	prov := &penaltyRecordProvider{ex: ex}
	if _, err := e.PlayerOrchestrateDeathPenalties(id, penaltyOrchestrationInput(), &captureTestRNG{}, prov); err != nil {
		t.Fatalf("orchestrate: %v", err)
	}
	ch, _ := prov.last(t).Result()
	out := awaitPenaltyResult(t, ch)
	if out.Err != nil {
		t.Fatalf("result err = %v, want nil (Duplicate is success)", out.Err)
	}
	if out.Delivery != sim.DeathPenaltyCompletionDuplicate {
		t.Fatalf("delivery = %d, want Duplicate", uint8(out.Delivery))
	}
	if fs.penaltyCalls != 1 || fs.recCalls != 0 {
		t.Fatalf("Store=%d recovery=%d, want 1/0 (no replay)", fs.penaltyCalls, fs.recCalls)
	}
}

// ---- proven lost ack -------------------------------------------

// Store reports a synthetic error after the state it would have
// committed is represented by the recovery loader: exactly one
// Store call, one recovery load, Recovered=true, Saver known
// E+1 unblocked, newer pending snapshot survives, one
// completion, and no reconcile (behaviorally: the newer
// MarkDirty survives; statically: tripwire test below).
func TestPenaltyProvenLostAck(t *testing.T) {
	s := mustSaverForPersist(t)
	penaltyTestTrack(t, s, 7, 5)
	fs := newPenaltyFakeStore()
	fs.onPenalty = func(req store.DeathPenaltiesRequest) (store.DeathPenaltiesResult, error) {
		return store.DeathPenaltiesResult{}, errPenaltySynth
	}
	entered := make(chan struct{}, 1)
	proceed := make(chan struct{})
	fs.recHook = func() {
		entered <- struct{}{}
		<-proceed
	}
	sink := &penaltyFakeSink{}
	ex, _ := startPenaltyExecutor(t, penaltyTestConfig(fs, s, sink))
	e, id := penaltyOrchestrationEngine(t, 7)
	prov := &penaltyRecordProvider{ex: ex}

	res, err := e.PlayerOrchestrateDeathPenalties(id, penaltyOrchestrationInput(), &captureTestRNG{}, prov)
	if err != nil {
		t.Fatalf("orchestrate: %v", err)
	}
	r := prov.last(t)
	ch, _ := r.Result()
	<-entered
	// Build the proving recovery from the ACTUAL frozen
	// orchestrated capture (post-penalty losses applied), not a
	// hand-built probe: revision E+1 with exact content and the
	// pending row deleted.
	intended, err := MapDeathPenaltyCapture(r.capture)
	if err != nil {
		t.Fatalf("intended map: %v", err)
	}
	fs.recSnap = provenPenaltyRec(intended, 6)
	mark := func(ctx context.Context, exp int64) (int64, error) { return exp + 1, nil }
	if err := s.MarkDirty(sim.AggregateKey{Kind: sim.AggregateCharacter, ID: 7}, mark); err != nil {
		t.Fatalf("MarkDirty: %v", err)
	}
	close(proceed)
	out := awaitPenaltyResult(t, ch)
	if out.Err != nil {
		t.Fatalf("result err = %v, want proven success", out.Err)
	}
	if !out.Recovered {
		t.Fatal("Recovered = false, want true (lost ack)")
	}
	if fs.penaltyCalls != 1 {
		t.Fatalf("Store calls = %d, want 1 (no replay)", fs.penaltyCalls)
	}
	if fs.recCalls != 1 {
		t.Fatalf("recovery loads = %d, want 1", fs.recCalls)
	}
	got := inspectKnown(t, s, sim.AggregateCharacter, 7)
	if got.KnownRevision != 6 || got.Blocked {
		t.Fatalf("saver = %+v, want known6 clean (no block)", got)
	}
	if !got.Dirty {
		t.Fatal("newer post-Reserve pending snapshot lost: proven path must preserve gen > reservedGeneration")
	}
	if sink.numCompletions() != 1 {
		t.Fatalf("completions = %d, want 1", sink.numCompletions())
	}
	c := sink.completions[0]
	if c.Token != res.Token {
		t.Fatalf("completion token = %+v, want %+v", c.Token, res.Token)
	}
	if disp, err := e.PlayerAcceptDeathPenaltyCompletion(c); err != nil || disp != sim.DeathPenaltyCompletionApplied {
		t.Fatalf("owner apply = %d,%v; want Applied,nil", disp, err)
	}
	if sink.numRetryables() != 0 {
		t.Fatalf("retryables = %d, want 0 (callback invoked => never retryable)", sink.numRetryables())
	}
}

// Inconsistent nominal success (nil Store error, impossible
// revision) with proving recovery still completes recovered.
func TestPenaltyInconsistentSuccessProven(t *testing.T) {
	s := mustSaverForPersist(t)
	penaltyTestTrack(t, s, 7, 5)
	fs := newPenaltyFakeStore()
	entered := make(chan struct{}, 1)
	proceed := make(chan struct{})
	fs.onPenalty = func(req store.DeathPenaltiesRequest) (store.DeathPenaltiesResult, error) {
		entered <- struct{}{}
		<-proceed
		return store.DeathPenaltiesResult{CharacterRevision: req.Character.ExpectedRevision + 42}, nil
	}
	sink := &penaltyFakeSink{}
	ex, _ := startPenaltyExecutor(t, penaltyTestConfig(fs, s, sink))
	e, id := penaltyOrchestrationEngine(t, 7)
	prov := &penaltyRecordProvider{ex: ex}
	if _, err := e.PlayerOrchestrateDeathPenalties(id, penaltyOrchestrationInput(), &captureTestRNG{}, prov); err != nil {
		t.Fatalf("orchestrate: %v", err)
	}
	r := prov.last(t)
	ch, _ := r.Result()
	<-entered
	intended, err := MapDeathPenaltyCapture(r.capture)
	if err != nil {
		t.Fatalf("intended map: %v", err)
	}
	fs.recSnap = provenPenaltyRec(intended, 6)
	close(proceed)
	out := awaitPenaltyResult(t, ch)
	if out.Err != nil || !out.Recovered {
		t.Fatalf("result = %+v, want recovered success", out)
	}
	if fs.penaltyCalls != 1 || fs.recCalls != 1 {
		t.Fatalf("Store=%d recovery=%d, want 1/1 (no replay)", fs.penaltyCalls, fs.recCalls)
	}
	if sink.numCompletions() != 1 || sink.numRetryables() != 0 {
		t.Fatalf("completions=%d retryables=%d, want 1/0",
			sink.numCompletions(), sink.numRetryables())
	}
}

// ---- unproven paths --------------------------------------------

// runPenaltyUnproven drives one execution whose recovery
// snapshot is built by tweak, then asserts the fail-closed
// contract: Store exactly once, unproven sentinel with the
// Store cause preserved, Saver blocked, zero completion, zero
// retryable, zero replay, live attempt still in flight.
func runPenaltyUnproven(
	t *testing.T, name string,
	tweak func(req store.DeathPenaltiesRequest, capture sim.DeathPenaltyCapture, rec *store.DeathCharacterRecoverySnapshot),
	storeFn func(store.DeathPenaltiesRequest) (store.DeathPenaltiesResult, error),
	wantStoreCause error,
) {
	t.Helper()
	s := mustSaverForPersist(t)
	penaltyTestTrack(t, s, 7, 5)
	fs := newPenaltyFakeStore()
	entered := make(chan struct{}, 1)
	proceed := make(chan struct{})
	// Gate the Store call so the proving/unproving recovery is
	// derived from the ACTUAL frozen orchestrated capture before
	// the worker can load it.
	fs.onPenalty = func(req store.DeathPenaltiesRequest) (store.DeathPenaltiesResult, error) {
		entered <- struct{}{}
		<-proceed
		return storeFn(req)
	}
	sink := &penaltyFakeSink{}
	ex, _ := startPenaltyExecutor(t, penaltyTestConfig(fs, s, sink))
	e, id := penaltyOrchestrationEngine(t, 7)
	prov := &penaltyRecordProvider{ex: ex}
	if _, err := e.PlayerOrchestrateDeathPenalties(id, penaltyOrchestrationInput(), &captureTestRNG{}, prov); err != nil {
		t.Fatalf("orchestrate: %v", err)
	}
	r := prov.last(t)
	ch, _ := r.Result()
	<-entered
	intended, err := MapDeathPenaltyCapture(r.capture)
	if err != nil {
		t.Fatalf("intended map: %v", err)
	}
	rec := provenPenaltyRec(intended, 6)
	tweak(intended, r.capture, &rec)
	fs.recSnap = rec
	close(proceed)
	out := awaitPenaltyResult(t, ch)
	if out.Err == nil {
		t.Fatalf("%s: result err = nil, want unproven", name)
	}
	if !errors.Is(out.Err, ErrDeathPenaltyCommitUnproven) {
		t.Fatalf("%s: err = %v, want ErrDeathPenaltyCommitUnproven", name, out.Err)
	}
	if wantStoreCause != nil && !errors.Is(out.Err, wantStoreCause) {
		t.Fatalf("%s: err = %v, want Store cause preserved", name, out.Err)
	}
	if out.RetryNotified {
		t.Fatalf("%s: retryable notified after callback invocation", name)
	}
	if fs.penaltyCalls != 1 {
		t.Fatalf("%s: Store calls = %d, want 1 (no replay)", name, fs.penaltyCalls)
	}
	got := inspectKnown(t, s, sim.AggregateCharacter, 7)
	if !got.Blocked {
		t.Fatalf("%s: saver = %+v, want reconcile-blocked", name, got)
	}
	if sink.numCompletions() != 0 {
		t.Fatalf("%s: completions = %d, want 0", name, sink.numCompletions())
	}
	if sink.numRetryables() != 0 {
		t.Fatalf("%s: retryables = %d, want 0 (callback invoked => never retryable)", name, sink.numRetryables())
	}
	live, ok, lerr := e.PlayerPendingDeathOf(id)
	if lerr != nil || !ok || live.EffectiveCost != 90 {
		t.Fatalf("%s: live pending = %+v,%v,%v; want original attempt still in flight", name, live, ok, lerr)
	}
}

func TestPenaltyUnprovenMatrix(t *testing.T) {
	pending := &store.PendingDeathSnapshot{
		CharacterID: 7, EffectiveCost: 90, DeathTimeSeconds: 100, PortalUsed: false,
	}
	cases := []struct {
		name  string
		tweak func(req store.DeathPenaltiesRequest, capture sim.DeathPenaltyCapture, rec *store.DeathCharacterRecoverySnapshot)
	}{
		{"revision E", func(_ store.DeathPenaltiesRequest, _ sim.DeathPenaltyCapture, rec *store.DeathCharacterRecoverySnapshot) {
			rec.Character.ExpectedRevision = 5
		}},
		{"revision beyond E+1", func(_ store.DeathPenaltiesRequest, _ sim.DeathPenaltyCapture, rec *store.DeathCharacterRecoverySnapshot) {
			rec.Character.ExpectedRevision = 7
		}},
		{"position", func(req store.DeathPenaltiesRequest, _ sim.DeathPenaltyCapture, rec *store.DeathCharacterRecoverySnapshot) {
			rec.Character.PosX = req.Character.PosX + 1
		}},
		{"vitals", func(_ store.DeathPenaltiesRequest, _ sim.DeathPenaltyCapture, rec *store.DeathCharacterRecoverySnapshot) {
			rec.Character.Vitals = json.RawMessage(`{"hp":1}`)
		}},
		{"flags", func(req store.DeathPenaltiesRequest, _ sim.DeathPenaltyCapture, rec *store.DeathCharacterRecoverySnapshot) {
			rec.Character.Flags = req.Character.Flags ^ 1
		}},
		{"spell ability", func(_ store.DeathPenaltiesRequest, _ sim.DeathPenaltyCapture, rec *store.DeathCharacterRecoverySnapshot) {
			rec.Character.Spells[0].Ability++
		}},
		{"skill ability", func(_ store.DeathPenaltiesRequest, _ sim.DeathPenaltyCapture, rec *store.DeathCharacterRecoverySnapshot) {
			rec.Character.Skills[0].Ability++
		}},
		{"advancement", func(_ store.DeathPenaltiesRequest, _ sim.DeathPenaltyCapture, rec *store.DeathCharacterRecoverySnapshot) {
			rec.Character.Advancement = json.RawMessage(`{"adv_points":8}`)
		}},
		{"pending still exists", func(_ store.DeathPenaltiesRequest, _ sim.DeathPenaltyCapture, rec *store.DeathCharacterRecoverySnapshot) {
			rec.Pending = pending
		}},
	}
	synthFn := func(store.DeathPenaltiesRequest) (store.DeathPenaltiesResult, error) {
		return store.DeathPenaltiesResult{}, errPenaltySynth
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runPenaltyUnproven(t, tc.name, tc.tweak, synthFn, errPenaltySynth)
		})
	}
}

// Inconsistent nominal success with unproven recovery fails
// closed (no blind trust of the impossible result).
func TestPenaltyInconsistentSuccessUnproven(t *testing.T) {
	badFn := func(req store.DeathPenaltiesRequest) (store.DeathPenaltiesResult, error) {
		return store.DeathPenaltiesResult{CharacterRevision: req.Character.ExpectedRevision + 42}, nil
	}
	runPenaltyUnproven(t, "inconsistent-unproven",
		func(_ store.DeathPenaltiesRequest, _ sim.DeathPenaltyCapture, rec *store.DeathCharacterRecoverySnapshot) {
			rec.Character.ExpectedRevision = 47
		}, badFn, nil)
}

// Semantic Store error (cost mismatch) with unproven recovery:
// cause preserved, Saver blocked, NO retryable, NO replay. This
// pins the callback-invoked => no retryable rule.
func TestPenaltySemanticStoreErrorUnproven(t *testing.T) {
	semantic := fmt.Errorf("test: semantic penalty: %w", store.ErrPendingDeathCostMismatch)
	semanticFn := func(store.DeathPenaltiesRequest) (store.DeathPenaltiesResult, error) {
		return store.DeathPenaltiesResult{}, semantic
	}
	runPenaltyUnproven(t, "semantic", func(_ store.DeathPenaltiesRequest, _ sim.DeathPenaltyCapture, rec *store.DeathCharacterRecoverySnapshot) {
		rec.Character.ExpectedRevision = 5
		rec.Pending = &store.PendingDeathSnapshot{
			CharacterID: 7, EffectiveCost: 90, DeathTimeSeconds: 100,
		}
	}, semanticFn, semantic)
}

// Recovery loader error: Saver blocked, no completion, no
// retryable, no replay; both the Store cause and the recovery
// cause are preserved.
func TestPenaltyRecoveryLoaderError(t *testing.T) {
	s := mustSaverForPersist(t)
	penaltyTestTrack(t, s, 7, 5)
	fs := newPenaltyFakeStore()
	fs.onPenalty = func(store.DeathPenaltiesRequest) (store.DeathPenaltiesResult, error) {
		return store.DeathPenaltiesResult{}, errPenaltySynth
	}
	fs.recErr = errors.New("test: recovery unavailable")
	sink := &penaltyFakeSink{}
	ex, _ := startPenaltyExecutor(t, penaltyTestConfig(fs, s, sink))
	e, id := penaltyOrchestrationEngine(t, 7)
	prov := &penaltyRecordProvider{ex: ex}
	if _, err := e.PlayerOrchestrateDeathPenalties(id, penaltyOrchestrationInput(), &captureTestRNG{}, prov); err != nil {
		t.Fatalf("orchestrate: %v", err)
	}
	ch, _ := prov.last(t).Result()
	out := awaitPenaltyResult(t, ch)
	if out.Err == nil {
		t.Fatal("result err = nil, want recovery failure")
	}
	if !errors.Is(out.Err, errPenaltySynth) {
		t.Fatalf("err = %v, want Store cause preserved", out.Err)
	}
	if out.RetryNotified || sink.numRetryables() != 0 {
		t.Fatal("retryable sent after callback invocation")
	}
	if fs.penaltyCalls != 1 || fs.recCalls != 1 {
		t.Fatalf("Store=%d recovery=%d, want 1/1 (no replay)", fs.penaltyCalls, fs.recCalls)
	}
	if got := inspectKnown(t, s, sim.AggregateCharacter, 7); !got.Blocked {
		t.Fatalf("saver = %+v, want blocked", got)
	}
	if sink.numCompletions() != 0 {
		t.Fatalf("completions = %d, want 0", sink.numCompletions())
	}
}

// ---- shutdown / retryable paths --------------------------------

// A blocked worker on job A plus queued job B; executor cancel:
// B sees Store zero, Saver slot cancelled, exactly one
// retryable with the exact frozen token, a terminal result, and
// its permit returned.
func TestPenaltyQueuedShutdown(t *testing.T) {
	s := mustSaverForPersist(t)
	penaltyTestTrack(t, s, 7, 5)
	penaltyTestTrack(t, s, 8, 9)
	fs := newPenaltyFakeStore()
	releaseA := make(chan struct{})
	enteredA := make(chan struct{}, 1)
	fs.onPenalty = func(req store.DeathPenaltiesRequest) (store.DeathPenaltiesResult, error) {
		if req.Character.ID == 7 {
			enteredA <- struct{}{}
			<-releaseA
			return store.DeathPenaltiesResult{
				CharacterRevision: req.Character.ExpectedRevision + 1,
			}, nil
		}
		t.Errorf("Store called for queued job B (character 8)")
		return store.DeathPenaltiesResult{}, errPenaltySynth
	}
	sink := &penaltyFakeSink{}
	ex, cancel := startPenaltyExecutor(t, PenaltyExecutorConfig{
		Workers: 1, QueueCapacity: 2, Store: fs, Saver: s, Sink: sink,
		RetryDelay: time.Millisecond, RetryTimeout: 5 * time.Second,
	})
	// Job A: manual reservation for character 7.
	ra := mustReservePenalty(t, ex, 7)
	if err := ra.PrepareDeathPenaltyWork(penaltyTestCapture(t)); err != nil {
		t.Fatalf("Prepare A: %v", err)
	}
	if err := ra.ActivateDeathPenaltyWork(); err != nil {
		t.Fatalf("Activate A: %v", err)
	}
	<-enteredA
	// Job B: manual reservation for character 8, activated/queued.
	rb := mustReservePenalty(t, ex, 8)
	capB := penaltyTestCapture(t)
	capB.Token = sim.DeathPenaltyAttemptToken{EntityID: 2, CharacterID: 8, Epoch: 1}
	if err := rb.PrepareDeathPenaltyWork(capB); err != nil {
		t.Fatalf("Prepare B: %v", err)
	}
	if err := rb.ActivateDeathPenaltyWork(); err != nil {
		t.Fatalf("Activate B: %v", err)
	}
	chB, _ := rb.Result()
	cancel()
	close(releaseA)
	chA, _ := ra.Result()
	outA := awaitPenaltyResult(t, chA)
	if outA.Err != nil {
		t.Fatalf("job A err = %v (running callback observes cancellation but Store already returned)", outA.Err)
	}
	outB := awaitPenaltyResult(t, chB)
	if !errors.Is(outB.Err, ErrPenaltyExecutorShutdown) {
		t.Fatalf("job B err = %v, want terminal Shutdown", outB.Err)
	}
	if !outB.RetryNotified {
		t.Fatal("job B RetryNotified = false, want definitive retryable delivered")
	}
	if sink.numRetryables() != 1 {
		t.Fatalf("retryables = %d, want exactly 1", sink.numRetryables())
	}
	if sink.retryables[0] != capB.Token {
		t.Fatalf("retryable token = %+v, want B token %+v", sink.retryables[0], capB.Token)
	}
	if sink.numCompletions() != 1 {
		t.Fatalf("completions = %d, want 1 (only job A)", sink.numCompletions())
	}
	// B's Saver slot was cancelled: the gate is free.
	probe, err := s.ReserveCriticalSet([]sim.AggregateKey{{Kind: sim.AggregateCharacter, ID: 8}})
	if err != nil {
		t.Fatalf("B Saver gate still held: %v", err)
	}
	probe.Cancel()
}

// Deterministic pre-callback Execute failure (no-callback
// path): owner retryable, zero Store calls.
func TestPenaltyPreCallbackExecuteFailure(t *testing.T) {
	s := mustSaverForPersist(t)
	penaltyTestTrack(t, s, 7, 5)
	fs := newPenaltyFakeStore()
	sink := &penaltyFakeSink{}
	ex, _ := startPenaltyExecutor(t, penaltyTestConfig(fs, s, sink))
	r := mustReservePenalty(t, ex, 7)
	capture := penaltyTestCapture(t)
	if err := r.PrepareDeathPenaltyWork(capture); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	// White-box: steal the frozen job inputs, cancel the Saver
	// slot externally (simulating shutdown-before-execution),
	// then execute with an already-cancelled context so the
	// reserved Execute fails before callback invocation.
	r.mu.Lock()
	job := penaltyExecutionJob{req: r.req, capture: r.capture, critical: r.critical, res: r.res}
	r.mu.Unlock()
	job.critical.Cancel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := ex.execute(ctx, job)
	if fs.penaltyCalls != 0 {
		t.Fatalf("Store calls = %d, want 0", fs.penaltyCalls)
	}
	if fs.recCalls != 0 {
		t.Fatalf("recovery loads = %d, want 0", fs.recCalls)
	}
	if sink.numRetryables() != 1 {
		t.Fatalf("retryables = %d, want 1 (definitive pre-Store retryable)", sink.numRetryables())
	}
	if sink.retryables[0] != capture.Token {
		t.Fatalf("retryable token = %+v, want %+v", sink.retryables[0], capture.Token)
	}
	if !out.RetryNotified || sink.numCompletions() != 0 {
		t.Fatalf("result = %+v completions=%d, want retryable success with no completion",
			out, sink.numCompletions())
	}
}

// The retryable notification is consumable by the d3a owner:
// Applied flips persistenceActive true -> false with the
// gameplay lock, capture, and epoch preserved, so exact-plan
// retry without RNG is legal again. Deterministic: a blocked
// worker holds job A while the orchestrated job B stays queued;
// executor cancel drains B into the retryable path.
func TestPenaltyRetryableOwnerConsumable(t *testing.T) {
	s := mustSaverForPersist(t)
	penaltyTestTrack(t, s, 7, 5)
	penaltyTestTrack(t, s, 8, 9)
	fs := newPenaltyFakeStore()
	releaseA := make(chan struct{})
	enteredA := make(chan struct{}, 1)
	fs.onPenalty = func(req store.DeathPenaltiesRequest) (store.DeathPenaltiesResult, error) {
		if req.Character.ID == 7 {
			enteredA <- struct{}{}
			<-releaseA
		}
		return echoPenaltySuccess()(req)
	}
	e := mustSimEngine(t)
	addPenaltyPlayer := func(charID int64) sim.EntityID {
		snap, err := e.AddPlayerEntityWithDurableState(sim.CharacterID(charID),
			world.Vec3{X: 1, Y: 0, Z: 1}, penaltyTestVitals(t), testDeathRuntimeInputs(t), penaltyTestDurable())
		if err != nil {
			t.Fatalf("add player %d: %v", charID, err)
		}
		pending := penaltyTestPending()
		if err := e.PlayerInstallRecoveredPendingDeath(snap.ID, &pending); err != nil {
			t.Fatalf("hydrate %d: %v", charID, err)
		}
		return snap.ID
	}
	addPenaltyPlayer(7)
	idB := addPenaltyPlayer(8)
	sink := &penaltyFakeSink{}
	sink.onRetryable = func(n int, tok sim.DeathPenaltyAttemptToken) (sim.DeathPenaltyRetryDisposition, error) {
		return e.PlayerMarkDeathPenaltyPersistenceRetryable(tok)
	}
	ex, cancel := startPenaltyExecutor(t, PenaltyExecutorConfig{
		Workers: 1, QueueCapacity: 2, Store: fs, Saver: s, Sink: sink,
		RetryDelay: time.Millisecond, RetryTimeout: 5 * time.Second,
	})
	// Job A: manual reservation for character 7, blocking the
	// single worker.
	ra := mustReservePenalty(t, ex, 7)
	if err := ra.PrepareDeathPenaltyWork(penaltyTestCapture(t)); err != nil {
		t.Fatalf("Prepare A: %v", err)
	}
	if err := ra.ActivateDeathPenaltyWork(); err != nil {
		t.Fatalf("Activate A: %v", err)
	}
	<-enteredA
	// Job B: real d3a orchestration for character 8, queued
	// behind A.
	prov := &penaltyRecordProvider{ex: ex}
	resB, err := e.PlayerOrchestrateDeathPenalties(idB, penaltyOrchestrationInput(), &captureTestRNG{}, prov)
	if err != nil {
		t.Fatalf("orchestrate B: %v", err)
	}
	chB, _ := prov.last(t).Result()
	cancel()
	close(releaseA)
	chA, _ := ra.Result()
	if out := awaitPenaltyResult(t, chA); out.Err != nil {
		t.Fatalf("job A err = %v", out.Err)
	}
	outB := awaitPenaltyResult(t, chB)
	if !outB.RetryNotified {
		t.Fatalf("job B result = %+v, want pre-Store retryable", outB)
	}
	if !errors.Is(outB.Err, ErrPenaltyExecutorShutdown) {
		t.Fatalf("job B err = %v, want Shutdown cause", outB.Err)
	}
	if sink.retryables[0] != resB.Token {
		t.Fatalf("retryable token = %+v, want B token %+v", sink.retryables[0], resB.Token)
	}
	// The owner consumed the retryable: a repeated notification
	// is a zero-mutation Duplicate, and exact-plan retry with a
	// fresh executor reuses the same token without RNG.
	if disp, err := e.PlayerMarkDeathPenaltyPersistenceRetryable(resB.Token); err != nil || disp != sim.DeathPenaltyRetryDuplicate {
		t.Fatalf("repeat retryable = %d,%v; want Duplicate,nil", disp, err)
	}
	ex2, err := NewPenaltyExecutor(PenaltyExecutorConfig{
		Workers: 1, QueueCapacity: 2, Store: fs, Saver: s, Sink: sink,
	})
	if err != nil {
		t.Fatalf("New ex2: %v", err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- ex2.Run(ctx2) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		ex2.mu.Lock()
		running := ex2.running
		ex2.mu.Unlock()
		if running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ex2 Run did not start")
		}
		time.Sleep(time.Millisecond)
	}
	t.Cleanup(func() { cancel2(); <-runErr })
	tok, err := e.PlayerRetryDeathPenaltyPersistence(idB, ex2)
	if err != nil {
		t.Fatalf("owner exact-plan retry: %v", err)
	}
	if tok != resB.Token {
		t.Fatalf("retry token = %+v, want same %+v (epoch kept, no reroll)", tok, resB.Token)
	}
}

// ---- redelivery ------------------------------------------------

// Sink Full, Full, then Applied: exactly one Store call, at most
// one recovery, same completion/token redelivered, eventual
// success.
func TestPenaltyIngressFullCompletion(t *testing.T) {
	s := mustSaverForPersist(t)
	penaltyTestTrack(t, s, 7, 5)
	fs := newPenaltyFakeStore()
	fs.onPenalty = echoPenaltySuccess()
	sink := &penaltyFakeSink{}
	sink.onCompletion = func(n int, c sim.DeathPenaltyCompletion) (sim.DeathPenaltyCompletionDisposition, error) {
		if n <= 2 {
			return sim.DeathPenaltyCompletionApplied, sim.ErrSimIngressFull
		}
		return sim.DeathPenaltyCompletionApplied, nil
	}
	ex, _ := startPenaltyExecutor(t, penaltyTestConfig(fs, s, sink))
	e, id := penaltyOrchestrationEngine(t, 7)
	prov := &penaltyRecordProvider{ex: ex}
	res, err := e.PlayerOrchestrateDeathPenalties(id, penaltyOrchestrationInput(), &captureTestRNG{}, prov)
	if err != nil {
		t.Fatalf("orchestrate: %v", err)
	}
	ch, _ := prov.last(t).Result()
	out := awaitPenaltyResult(t, ch)
	if out.Err != nil {
		t.Fatalf("result err = %v, want eventual success", out.Err)
	}
	if fs.penaltyCalls != 1 {
		t.Fatalf("Store calls = %d, want exactly 1 (no replay)", fs.penaltyCalls)
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

// Definite pre-Store retryable under ingress pressure: Full
// then Applied for the same token, bounded retry, Store zero.
func TestPenaltyIngressFullRetryable(t *testing.T) {
	s := mustSaverForPersist(t)
	penaltyTestTrack(t, s, 7, 5)
	fs := newPenaltyFakeStore()
	sink := &penaltyFakeSink{}
	sink.onRetryable = func(n int, tok sim.DeathPenaltyAttemptToken) (sim.DeathPenaltyRetryDisposition, error) {
		if n == 1 {
			return sim.DeathPenaltyRetryApplied, sim.ErrSimIngressFull
		}
		return sim.DeathPenaltyRetryApplied, nil
	}
	ex, _ := startPenaltyExecutor(t, penaltyTestConfig(fs, s, sink))
	r := mustReservePenalty(t, ex, 7)
	capture := penaltyTestCapture(t)
	if err := r.PrepareDeathPenaltyWork(capture); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	r.mu.Lock()
	job := penaltyExecutionJob{req: r.req, capture: r.capture, critical: r.critical, res: r.res}
	r.mu.Unlock()
	job.critical.Cancel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := ex.execute(ctx, job)
	if !out.RetryNotified {
		t.Fatalf("result = %+v, want retryable delivered after Full retry", out)
	}
	if sink.numRetryables() != 2 {
		t.Fatalf("retryables = %d, want 2 (Full + Applied)", sink.numRetryables())
	}
	for i, tok := range sink.retryables {
		if tok != capture.Token {
			t.Fatalf("retryable %d token = %+v, want same %+v", i, tok, capture.Token)
		}
	}
	if fs.penaltyCalls != 0 || fs.recCalls != 0 {
		t.Fatalf("Store=%d recovery=%d, want 0/0", fs.penaltyCalls, fs.recCalls)
	}
}

// Terminal (non-full) completion delivery error: single
// delivery, no retry loop, no Store replay.
func TestPenaltyCompletionTerminalError(t *testing.T) {
	s := mustSaverForPersist(t)
	penaltyTestTrack(t, s, 7, 5)
	fs := newPenaltyFakeStore()
	fs.onPenalty = echoPenaltySuccess()
	terminal := errors.New("test: terminal owner error")
	sink := &penaltyFakeSink{}
	sink.onCompletion = func(n int, c sim.DeathPenaltyCompletion) (sim.DeathPenaltyCompletionDisposition, error) {
		return sim.DeathPenaltyCompletionApplied, terminal
	}
	ex, _ := startPenaltyExecutor(t, penaltyTestConfig(fs, s, sink))
	e, id := penaltyOrchestrationEngine(t, 7)
	prov := &penaltyRecordProvider{ex: ex}
	if _, err := e.PlayerOrchestrateDeathPenalties(id, penaltyOrchestrationInput(), &captureTestRNG{}, prov); err != nil {
		t.Fatalf("orchestrate: %v", err)
	}
	ch, _ := prov.last(t).Result()
	out := awaitPenaltyResult(t, ch)
	if !errors.Is(out.Err, terminal) {
		t.Fatalf("result err = %v, want terminal owner error", out.Err)
	}
	if sink.numCompletions() != 1 {
		t.Fatalf("completions = %d, want 1 (no retry loop)", sink.numCompletions())
	}
	if fs.penaltyCalls != 1 {
		t.Fatalf("Store calls = %d, want 1 (no replay)", fs.penaltyCalls)
	}
}

// ---- aliasing --------------------------------------------------

// Caller mutation after Prepare cannot reach the Store request,
// proof reference, or completion.
func TestPenaltySubmissionAliasing(t *testing.T) {
	s := mustSaverForPersist(t)
	penaltyTestTrack(t, s, 7, 5)
	fs := newPenaltyFakeStore()
	fs.onPenalty = echoPenaltySuccess()
	sink := &penaltyFakeSink{}
	ex, _ := startPenaltyExecutor(t, penaltyTestConfig(fs, s, sink))
	r := mustReservePenalty(t, ex, 7)
	capture := penaltyTestCapture(t)
	if err := r.PrepareDeathPenaltyWork(capture); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	// Poison every caller-owned mutable domain after Prepare.
	capture.Durable.Advancement[0] = 'X'
	capture.Durable.Spells[0].Ability = 1
	capture.Durable.Skills[0].Ability = 1
	*capture.PendingBefore.CorpseID = 424242
	capture.PendingBefore.EffectiveCost = 5
	capture.Vitals.HP = 1
	capture.Position.X = 999
	if err := r.ActivateDeathPenaltyWork(); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	ch, _ := r.Result()
	out := awaitPenaltyResult(t, ch)
	if out.Err != nil {
		t.Fatalf("result err = %v", out.Err)
	}
	got := fs.gotPenalty[0]
	if got.Character.Advancement[0] == 'X' || got.Character.Spells[0].Ability == 1 || got.Character.Skills[0].Ability == 1 {
		t.Fatal("Store request reflects post-Prepare caller mutation")
	}
	if got.ExpectedPendingCost != 90 {
		t.Fatalf("Store ExpectedPendingCost = %d, want frozen 90", got.ExpectedPendingCost)
	}
	if got.Character.PosX == 999000 {
		t.Fatal("Store request reflects post-Prepare position mutation")
	}
}

// ---- worker bounds ---------------------------------------------

// One worker executes many sequential penalties with exactly
// one Store call each: fixed pool, no goroutine per penalty.
func TestPenaltyFixedWorkerSequential(t *testing.T) {
	s := mustSaverForPersist(t)
	penaltyTestTrack(t, s, 7, 5)
	fs := newPenaltyFakeStore()
	fs.onPenalty = echoPenaltySuccess()
	sink := &penaltyFakeSink{}
	ex, _ := startPenaltyExecutor(t, PenaltyExecutorConfig{
		Workers: 1, QueueCapacity: 1, Store: fs, Saver: s, Sink: sink,
		RetryDelay: time.Millisecond, RetryTimeout: 5 * time.Second,
	})
	if ex.numWorkers != 1 {
		t.Fatalf("numWorkers = %d, want fixed 1", ex.numWorkers)
	}
	// Track revisions advance per job; re-track is unnecessary:
	// the Saver known revision advances through execution.
	for i := 0; i < 5; i++ {
		r := mustReservePenalty(t, ex, 7)
		capture := penaltyTestCapture(t)
		capture.Token.Epoch = uint64(i + 1)
		if err := r.PrepareDeathPenaltyWork(capture); err != nil {
			t.Fatalf("job %d Prepare: %v", i, err)
		}
		if err := r.ActivateDeathPenaltyWork(); err != nil {
			t.Fatalf("job %d Activate: %v", i, err)
		}
		ch, _ := r.Result()
		out := awaitPenaltyResult(t, ch)
		if out.Err != nil {
			t.Fatalf("job %d err = %v", i, out.Err)
		}
		// Cancel after activation is a no-op that never
		// retracts executed work.
		r.CancelDeathPenaltyWork()
	}
	if fs.penaltyCalls != 5 {
		t.Fatalf("Store calls = %d, want exactly 5 (one each, no dup)", fs.penaltyCalls)
	}
	if got := inspectKnown(t, s, sim.AggregateCharacter, 7); got.KnownRevision != 10 || got.Blocked {
		t.Fatalf("saver = %+v, want known10 clean", got)
	}
}

// The queue permit is released at dequeue while the Saver gate
// survives until Execute terminalizes: while job A runs, a
// replacement Reserve for a clean character succeeds (permit
// freed at dequeue) but the running job's Saver gate stays
// Busy.
func TestPenaltyPermitReleasedAtDequeue(t *testing.T) {
	s := mustSaverForPersist(t)
	penaltyTestTrack(t, s, 7, 5)
	penaltyTestTrack(t, s, 8, 1)
	fs := newPenaltyFakeStore()
	entered := make(chan struct{}, 1)
	proceed := make(chan struct{})
	fs.onPenalty = func(req store.DeathPenaltiesRequest) (store.DeathPenaltiesResult, error) {
		entered <- struct{}{}
		<-proceed
		return echoPenaltySuccess()(req)
	}
	sink := &penaltyFakeSink{}
	ex, _ := startPenaltyExecutor(t, PenaltyExecutorConfig{
		Workers: 1, QueueCapacity: 1, Store: fs, Saver: s, Sink: sink,
		RetryDelay: time.Millisecond, RetryTimeout: 5 * time.Second,
	})
	ra := mustReservePenalty(t, ex, 7)
	if err := ra.PrepareDeathPenaltyWork(penaltyTestCapture(t)); err != nil {
		t.Fatalf("Prepare A: %v", err)
	}
	if err := ra.ActivateDeathPenaltyWork(); err != nil {
		t.Fatalf("Activate A: %v", err)
	}
	<-entered
	// Dequeued: the permit is free even though execution is
	// still in flight.
	rb := mustReservePenalty(t, ex, 8)
	defer rb.CancelDeathPenaltyWork()
	// The Saver gate is still held by the running job.
	if _, err := s.ReserveCriticalSet([]sim.AggregateKey{{Kind: sim.AggregateCharacter, ID: 7}}); !errors.Is(err, sim.ErrCriticalSetReservationBusy) {
		t.Fatalf("Saver Reserve during execution = %v, want Busy", err)
	}
	close(proceed)
	chA, _ := ra.Result()
	if out := awaitPenaltyResult(t, chA); out.Err != nil {
		t.Fatalf("job A err = %v", out.Err)
	}
}

// Capacity-2 with a queued job: activating a second prepared
// reservation never reports queue-full.
func TestPenaltyActivateNeverQueueFull(t *testing.T) {
	s := mustSaverForPersist(t)
	penaltyTestTrack(t, s, 7, 5)
	penaltyTestTrack(t, s, 8, 9)
	fs := newPenaltyFakeStore()
	entered := make(chan struct{}, 1)
	proceed := make(chan struct{})
	fs.onPenalty = func(req store.DeathPenaltiesRequest) (store.DeathPenaltiesResult, error) {
		if req.Character.ID == 7 {
			entered <- struct{}{}
			<-proceed
		}
		return echoPenaltySuccess()(req)
	}
	sink := &penaltyFakeSink{}
	ex, _ := startPenaltyExecutor(t, PenaltyExecutorConfig{
		Workers: 1, QueueCapacity: 2, Store: fs, Saver: s, Sink: sink,
		RetryDelay: time.Millisecond, RetryTimeout: 5 * time.Second,
	})
	ra := mustReservePenalty(t, ex, 7)
	if err := ra.PrepareDeathPenaltyWork(penaltyTestCapture(t)); err != nil {
		t.Fatalf("Prepare A: %v", err)
	}
	if err := ra.ActivateDeathPenaltyWork(); err != nil {
		t.Fatalf("Activate A: %v", err)
	}
	<-entered
	rb := mustReservePenalty(t, ex, 8)
	capB := penaltyTestCapture(t)
	capB.Token = sim.DeathPenaltyAttemptToken{EntityID: 2, CharacterID: 8, Epoch: 1}
	if err := rb.PrepareDeathPenaltyWork(capB); err != nil {
		t.Fatalf("Prepare B: %v", err)
	}
	// The queue channel physically holds job A; B must still
	// activate without queue-full.
	if err := rb.ActivateDeathPenaltyWork(); err != nil {
		t.Fatalf("Activate B = %v, want nil (no queue-full after Prepare)", err)
	}
	if err := rb.ActivateDeathPenaltyWork(); err != nil {
		t.Fatalf("repeat Activate B = %v, want nil no-op", err)
	}
	close(proceed)
	chA, _ := ra.Result()
	if out := awaitPenaltyResult(t, chA); out.Err != nil {
		t.Fatalf("job A err = %v", out.Err)
	}
	chB, _ := rb.Result()
	if out := awaitPenaltyResult(t, chB); out.Err != nil {
		t.Fatalf("job B err = %v", out.Err)
	}
	if fs.penaltyCalls != 2 {
		t.Fatalf("Store calls = %d, want 2 (no duplicate publication)", fs.penaltyCalls)
	}
}

// ---- lifecycle / bounds / race ---------------------------------

func TestPenaltyExecutorConfigValidation(t *testing.T) {
	s := mustSaverForPersist(t)
	penaltyTestTrack(t, s, 7, 5)
	fs := newPenaltyFakeStore()
	sink := &penaltyFakeSink{}
	good := penaltyTestConfig(fs, s, sink)
	for _, tc := range []struct {
		name string
		f    func(*PenaltyExecutorConfig)
	}{
		{"workers", func(c *PenaltyExecutorConfig) { c.Workers = 0 }},
		{"capacity", func(c *PenaltyExecutorConfig) { c.QueueCapacity = 0 }},
		{"store", func(c *PenaltyExecutorConfig) { c.Store = nil }},
		{"saver", func(c *PenaltyExecutorConfig) { c.Saver = nil }},
		{"sink", func(c *PenaltyExecutorConfig) { c.Sink = nil }},
	} {
		cfg := good
		tc.f(&cfg)
		if _, err := NewPenaltyExecutor(cfg); !errors.Is(err, ErrPenaltyExecutorInvalid) {
			t.Fatalf("%s: err = %v, want Invalid", tc.name, err)
		}
	}
}

func TestPenaltyExecutorNotRunningAndQueueFull(t *testing.T) {
	s := mustSaverForPersist(t)
	penaltyTestTrack(t, s, 7, 5)
	fs := newPenaltyFakeStore()
	sink := &penaltyFakeSink{}
	ex, err := NewPenaltyExecutor(PenaltyExecutorConfig{
		Workers: 1, QueueCapacity: 1, Store: fs, Saver: s, Sink: sink,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := ex.ReserveDeathPenaltyWork(7); !errors.Is(err, ErrPenaltyExecutorNotRunning) {
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
	r1, err := ex.ReserveDeathPenaltyWork(7)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	defer r1.CancelDeathPenaltyWork()
	if _, err := ex.ReserveDeathPenaltyWork(7); !errors.Is(err, ErrPenaltyExecutorQueueFull) {
		t.Fatalf("second Reserve = %v, want QueueFull", err)
	}
	// Second concurrent Run is AlreadyRunning; after exit a
	// sequential Run is Shutdown (one-shot lifecycle).
	if err := ex.Run(context.Background()); !errors.Is(err, ErrPenaltyExecutorAlreadyRunning) {
		t.Fatalf("concurrent Run = %v, want AlreadyRunning", err)
	}
	cancel()
	<-runErr
	if err := ex.Run(context.Background()); !errors.Is(err, ErrPenaltyExecutorShutdown) {
		t.Fatalf("sequential Run = %v, want Shutdown", err)
	}
}

func TestPenaltyReservationAccessors(t *testing.T) {
	s := mustSaverForPersist(t)
	penaltyTestTrack(t, s, 7, 5)
	fs := newPenaltyFakeStore()
	sink := &penaltyFakeSink{}
	ex, _ := startPenaltyExecutor(t, penaltyTestConfig(fs, s, sink))
	r := mustReservePenalty(t, ex, 7)
	if _, err := r.Result(); !errors.Is(err, ErrPenaltyReservationNotPrepared) {
		t.Fatalf("Result before Prepare = %v, want NotPrepared", err)
	}
	if err := r.ActivateDeathPenaltyWork(); !errors.Is(err, ErrPenaltyReservationNotPrepared) {
		t.Fatalf("Activate before Prepare = %v, want NotPrepared", err)
	}
	if err := r.PrepareDeathPenaltyWork(penaltyTestCapture(t)); err != nil {
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
	// Same channel every time: the single buffered terminal
	// result is consumable through either handle exactly once
	// (proving both handles name the same channel), and Cancel
	// stays idempotent.
	r.CancelDeathPenaltyWork()
	select {
	case res := <-ch1:
		if !errors.Is(res.Err, ErrPenaltyReservationCanceled) {
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
	r.CancelDeathPenaltyWork()
	if err := r.ActivateDeathPenaltyWork(); !errors.Is(err, ErrPenaltyReservationCanceled) {
		t.Fatalf("Activate after Cancel = %v, want Canceled", err)
	}
	// Repeated Activate after success is a no-op without second
	// publication.
	r2 := mustReservePenalty(t, ex, 7)
	defer r2.CancelDeathPenaltyWork()
	if err := r2.PrepareDeathPenaltyWork(penaltyTestCapture(t)); err != nil {
		t.Fatalf("Prepare2: %v", err)
	}
	fs.onPenalty = echoPenaltySuccess()
	if err := r2.ActivateDeathPenaltyWork(); err != nil {
		t.Fatalf("Activate2: %v", err)
	}
	if err := r2.ActivateDeathPenaltyWork(); err != nil {
		t.Fatalf("repeat Activate = %v, want nil no-op", err)
	}
	ch, _ := r2.Result()
	out := awaitPenaltyResult(t, ch)
	if out.Err != nil {
		t.Fatalf("result = %+v", out)
	}
	if fs.penaltyCalls != 1 {
		t.Fatalf("Store calls = %d, want 1 (no duplicate publication)", fs.penaltyCalls)
	}
}

// Cancel/Activate/Result race stress: no double permit release,
// no double Saver cancel, no double publication, no panic,
// race-clean, permits balanced.
func TestPenaltyReservationRaceStress(t *testing.T) {
	s := mustSaverForPersist(t)
	penaltyTestTrack(t, s, 7, 5)
	fs := newPenaltyFakeStore()
	fs.onPenalty = echoPenaltySuccess()
	sink := &penaltyFakeSink{}
	ex, cancel := startPenaltyExecutor(t, PenaltyExecutorConfig{
		Workers: 2, QueueCapacity: 4, Store: fs, Saver: s, Sink: sink,
		RetryDelay: time.Millisecond, RetryTimeout: 5 * time.Second,
	})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				ri, err := ex.ReserveDeathPenaltyWork(7)
				if err != nil {
					continue
				}
				r := ri.(*PenaltyExecutionReservation)
				_ = r.PrepareDeathPenaltyWork(penaltyTestCapture(t))
				if (g+i)%2 == 0 {
					_ = r.ActivateDeathPenaltyWork()
				} else {
					r.CancelDeathPenaltyWork()
				}
				if ch, err := r.Result(); err == nil {
					select {
					case <-ch:
					case <-time.After(10 * time.Second):
					}
				}
				r.CancelDeathPenaltyWork()
			}
		}(g)
	}
	wg.Wait()
	cancel()
	// Drain: every activated job either executed (permit
	// released at dequeue) or was drained (permit released in
	// Run). Cancelled reservations released theirs. The count
	// must balance exactly.
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

// The d3b production path MUST NOT reconcile through
// ReconcileSaver/ResolveReconciled: proven lost-ack returns E+1
// inside the held-gate callback while preserving newer
// snapshots. Full-line comments may name the forbidden rule;
// production CODE (and string literals) must not contain it.
func TestPenaltyNoReconcileOnProvenPath(t *testing.T) {
	path := "death_penalty.go"
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
			t.Fatalf("production %s contains %q: proven penalty lost-ack must not reconcile", path, banned)
		}
	}
}

// The d3b reserved path MUST NOT use the ordinary Saver write
// or the reacquiring public adapter inside execution.
func TestPenaltyNoOrdinaryWritePath(t *testing.T) {
	path := "death_penalty.go"
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
	for _, banned := range []string{"WriteCriticalSet", "CommitDeathPenalties(ctx, s", "persist.CommitDeathPenalties"} {
		if strings.Contains(body, banned) {
			t.Fatalf("production %s contains %q: d3b must use only the reserved Execute path", path, banned)
		}
	}
	if !strings.Contains(body, ".Execute(ctx, func(") {
		t.Fatal("production death_penalty.go must execute through CriticalSetReservation.Execute")
	}
	if !strings.Contains(body, "x.store.CommitDeathPenalties(") {
		t.Fatal("production death_penalty.go must call the narrow Store seam at most once per job")
	}
}
