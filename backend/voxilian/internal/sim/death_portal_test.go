package sim

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/dlukt/voxilian/internal/world"
)

// M5-T5c3d2a Portal-of-Life owner attempt lifecycle tests
// (spec §9.5.1k): resolved-input orchestration over the
// existing T5a PlanPortalOfLife + ReducePendingDeathCost
// composition (AwaitingRespawn + Alive begins, formula
// boundaries, lowers-only no-op, target/pending failure
// matrix, in-flight serialization, epoch exhaustion,
// Prepare failure, capture immutability), typed success
// completion (apply, nil-corpse, mismatch matrix,
// duplicate-before-payload-validation), definitive abort,
// ABA, respawn-release race, hydration guard, and typed
// same-mailbox completion/abort ingress. Deterministic,
// no wall clock, no Store/persist/gateway/proto, no
// Saver, no PG, no d2b executor.

// fakePortalReservation is the instrumented d2a
// PortalOfLifeWorkReservation: it records Prepare
// captures, activation/cancellation counts, and an
// optional Prepare failure.
type fakePortalReservation struct {
	prepares   []PortalOfLifeCapture
	activates  int
	cancels    int
	prepareErr error
}

func (f *fakePortalReservation) PreparePortalOfLifeWork(c PortalOfLifeCapture) error {
	f.prepares = append(f.prepares, c)
	return f.prepareErr
}

func (f *fakePortalReservation) ActivatePortalOfLifeWork() { f.activates++ }

func (f *fakePortalReservation) CancelPortalOfLifeWork() { f.cancels++ }

// portalPending is the canonical d2a pending fixture:
// cost 80, death time 100, corpse 500, unused.
func portalPending() *PendingDeathRuntime {
	return pendingFixture(80, 100, int64ptr(500), false)
}

// portalInput builds a resolved Portal input.
func portalInput(now int64, power int, corpse int64) PortalOfLifeResolvedInput {
	return PortalOfLifeResolvedInput{NowSeconds: now, SpellPower: power, TargetCorpseID: corpse}
}

// portalAwaitingPlayer runs the full immediate-death flow
// and installs the given pending, returning an
// AwaitingRespawn player plus its (now historical) death
// token.
func portalAwaitingPlayer(t *testing.T, e *Engine, charID CharacterID, pending *PendingDeathRuntime) (EntityID, DeathAttemptToken) {
	t.Helper()
	id, tok := pendingCompletionPlayer(t, e, charID)
	completion := testDeathCompletion(tok, world.Vec3{X: 5, Y: 0, Z: 7}, testPostDeathVitals(t), testPostDeathDurableState())
	completion.Pending = pending
	if _, disp, err := e.PlayerAcceptImmediateDeathCompletion(completion); err != nil || disp != DeathCompletionApplied {
		t.Fatalf("PlayerAcceptImmediateDeathCompletion = %d,%v; want Applied,nil", disp, err)
	}
	return id, tok
}

// portalAlivePlayer builds an Alive full-state player
// carrying the given pending via authoritative
// hydration.
func portalAlivePlayer(t *testing.T, e *Engine, charID CharacterID, pending *PendingDeathRuntime) EntityID {
	t.Helper()
	snap, err := e.AddPlayerEntityWithDurableState(charID, world.Vec3{X: 1, Y: 0, Z: 1}, testVitals(), testRuntimeInputs(), testFullDurableState())
	if err != nil {
		t.Fatalf("AddPlayerEntityWithDurableState: %v", err)
	}
	if err := e.PlayerInstallRecoveredPendingDeath(snap.ID, pending); err != nil {
		t.Fatalf("PlayerInstallRecoveredPendingDeath: %v", err)
	}
	return snap.ID
}

// beginPortal is a fatal-on-error orchestration wrapper.
func beginPortal(t *testing.T, e *Engine, id EntityID, input PortalOfLifeResolvedInput, r *fakePortalReservation) PortalOfLifeOrchestrationResult {
	t.Helper()
	res, err := e.PlayerOrchestratePortalOfLife(id, input, r)
	if err != nil {
		t.Fatalf("PlayerOrchestratePortalOfLife(%d): %v", uint64(id), err)
	}
	return res
}

// portalEnt white-box-resolves the live entity.
func portalEnt(t *testing.T, e *Engine, id EntityID) *entity {
	t.Helper()
	ent, err := e.registry.lookup(id)
	if err != nil {
		t.Fatalf("lookup(%d): %v", uint64(id), err)
	}
	return ent
}

// TestPortalBeginAwaitingRespawn proves the canonical
// valid begin: exact Plan/Reduce composition, Prepare
// while no attempt is live, one epoch increment,
// in-flight install, exactly-once Activate, and pending
// unchanged until completion.
func TestPortalBeginAwaitingRespawn(t *testing.T) {
	e := newPlayerEngine(t, nil)
	id, _ := portalAwaitingPlayer(t, e, testCharacterID(), portalPending())
	r := &fakePortalReservation{}
	res := beginPortal(t, e, id, portalInput(100, 50, 500), r)

	proposed, err := PlanPortalOfLife(PortalOfLifeInput{PendingCost: 80, CorpseAgeSeconds: 0, SpellPower: 50})
	if err != nil {
		t.Fatalf("PlanPortalOfLife: %v", err)
	}
	effective, err := ReducePendingDeathCost(80, proposed)
	if err != nil {
		t.Fatalf("ReducePendingDeathCost: %v", err)
	}
	if res.ProposedCost != proposed || res.ExpectedEffectiveCost != effective {
		t.Fatalf("result = %+v; want proposed %d expected %d", res, proposed, effective)
	}
	if res.ProposedCost != 5 || res.ExpectedEffectiveCost != 5 {
		t.Fatalf("result = %+v; want proposed 5 expected 5", res)
	}
	if res.Token.EntityID != id || res.Token.CharacterID != testCharacterID() || res.Token.Epoch != 1 {
		t.Fatalf("token = %+v; want entity %d epoch 1", res.Token, uint64(id))
	}
	ent := portalEnt(t, e, id)
	if ent.portalEpoch != 1 || !ent.portalInFlight {
		t.Fatalf("epoch=%d inFlight=%v; want 1,true", ent.portalEpoch, ent.portalInFlight)
	}
	if len(r.prepares) != 1 || r.activates != 1 || r.cancels != 0 {
		t.Fatalf("reservation prepares=%d activates=%d cancels=%d; want 1,1,0", len(r.prepares), r.activates, r.cancels)
	}
	if r.prepares[0].Token != res.Token {
		t.Fatalf("capture token = %+v, want %+v", r.prepares[0].Token, res.Token)
	}
	if st, _ := lifeOf(t, e, id); st != PlayerLifeAwaitingRespawn {
		t.Fatalf("life = %d, want AwaitingRespawn", uint8(st))
	}
	live, ok := pendingOf(t, e, id)
	if !ok {
		t.Fatalf("pending absent after begin")
	}
	requirePendingEqual(t, live, PendingDeathRuntime{EffectiveCost: 80, DeathTimeSeconds: 100, CorpseID: int64ptr(500)}, "begin preserves pending")
}

// TestPortalBeginAlive proves the same begin works for an
// Alive player (post respawn-release shape) and that
// ordinary gameplay input stays accepted while Portal
// persistence is in flight.
func TestPortalBeginAlive(t *testing.T) {
	e := newPlayerEngine(t, nil)
	id := portalAlivePlayer(t, e, testCharacterID(), portalPending())
	r := &fakePortalReservation{}
	res := beginPortal(t, e, id, portalInput(100, 50, 500), r)
	if res.Token.Epoch != 1 {
		t.Fatalf("epoch = %d, want 1", res.Token.Epoch)
	}
	if st, _ := lifeOf(t, e, id); st != PlayerLifeAlive {
		t.Fatalf("life = %d, want Alive", uint8(st))
	}
	disp, err := e.SubmitMove(id, MoveIntent{InputSeq: 1, HeldDirs: MoveDirForward, SampleTick: e.CurrentTick()})
	if err != nil || disp != MoveAccepted {
		t.Fatalf("SubmitMove during Portal flight = %d,%v; want Accepted,nil", disp, err)
	}
	live, _ := pendingOf(t, e, id)
	requirePendingEqual(t, live, PendingDeathRuntime{EffectiveCost: 80, DeathTimeSeconds: 100, CorpseID: int64ptr(500)}, "alive begin preserves pending")
}

// TestPortalFormulaCompositionBoundaries proves owner
// composition matches direct PlanPortalOfLife at age 0,
// 59, 60, and a later age.
func TestPortalFormulaCompositionBoundaries(t *testing.T) {
	for _, age := range []int64{0, 59, 60, 245} {
		e := newPlayerEngine(t, nil)
		id, _ := portalAwaitingPlayer(t, e, testCharacterID(), portalPending())
		r := &fakePortalReservation{}
		now := int64(100) + age
		res := beginPortal(t, e, id, portalInput(now, 50, 500), r)
		proposed, err := PlanPortalOfLife(PortalOfLifeInput{PendingCost: 80, CorpseAgeSeconds: age, SpellPower: 50})
		if err != nil {
			t.Fatalf("age %d PlanPortalOfLife: %v", age, err)
		}
		effective, err := ReducePendingDeathCost(80, proposed)
		if err != nil {
			t.Fatalf("age %d ReducePendingDeathCost: %v", age, err)
		}
		if res.ProposedCost != proposed || res.ExpectedEffectiveCost != effective {
			t.Fatalf("age %d result = %+v; want %d/%d", age, res, proposed, effective)
		}
	}
}

// TestPortalLowersOnlyNoop proves a pending cost of 0
// still starts the attempt even though the expected
// effective cost does not decrease.
func TestPortalLowersOnlyNoop(t *testing.T) {
	e := newPlayerEngine(t, nil)
	id, _ := portalAwaitingPlayer(t, e, testCharacterID(), pendingFixture(0, 100, int64ptr(500), false))
	r := &fakePortalReservation{}
	res := beginPortal(t, e, id, portalInput(100, 50, 500), r)
	if res.ProposedCost < 5 {
		t.Fatalf("proposed = %d, want >= 5", res.ProposedCost)
	}
	if res.ExpectedEffectiveCost != 0 {
		t.Fatalf("expected = %d, want 0", res.ExpectedEffectiveCost)
	}
	if ent := portalEnt(t, e, id); !ent.portalInFlight {
		t.Fatalf("attempt not live after lowers-only begin")
	}
}

// TestPortalBeginFailures proves every target/pending/
// input/lifecycle failure rejects before Prepare with
// zero mutation and exactly-once reservation Cancel.
func TestPortalBeginFailures(t *testing.T) {
	t.Run("unknown entity", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		r := &fakePortalReservation{}
		if _, err := e.PlayerOrchestratePortalOfLife(9999, portalInput(100, 50, 500), r); !errors.Is(err, ErrEntityNotFound) {
			t.Fatalf("err = %v, want ErrEntityNotFound", err)
		}
		if len(r.prepares) != 0 || r.activates != 0 || r.cancels != 1 {
			t.Fatalf("reservation = %d/%d/%d, want 0/0/1", len(r.prepares), r.activates, r.cancels)
		}
	})
	t.Run("generic entity", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		snap, err := e.AddEntity(world.Vec3{X: 1, Y: 0, Z: 1})
		if err != nil {
			t.Fatal(err)
		}
		r := &fakePortalReservation{}
		if _, err := e.PlayerOrchestratePortalOfLife(snap.ID, portalInput(100, 50, 500), r); !errors.Is(err, ErrEntityNotPlayer) {
			t.Fatalf("err = %v, want ErrEntityNotPlayer", err)
		}
		if len(r.prepares) != 0 || r.cancels != 1 {
			t.Fatalf("reservation = %d/%d, want 0/1", len(r.prepares), r.cancels)
		}
	})
	t.Run("migrating", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		id := portalAlivePlayer(t, e, testCharacterID(), portalPending())
		ent := portalEnt(t, e, id)
		destPos := world.Vec3{X: 33.5, Y: 0, Z: 1}
		dest, err := world.CellForPosition(destPos)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.registry.beginHandoff(id, OwnerRef{Cell: ent.cell, Generation: ent.generation}, dest, destPos); err != nil {
			t.Fatal(err)
		}
		r := &fakePortalReservation{}
		if _, err := e.PlayerOrchestratePortalOfLife(id, portalInput(100, 50, 500), r); !errors.Is(err, ErrCellHandoffRequired) {
			t.Fatalf("err = %v, want ErrCellHandoffRequired", err)
		}
		if len(r.prepares) != 0 || r.cancels != 1 {
			t.Fatalf("reservation = %d/%d, want 0/1", len(r.prepares), r.cancels)
		}
	})
	t.Run("death persisting", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		id, _ := pendingCompletionPlayer(t, e, testCharacterID())
		r := &fakePortalReservation{}
		if _, err := e.PlayerOrchestratePortalOfLife(id, portalInput(100, 50, 500), r); !errors.Is(err, ErrPlayerNotAlive) {
			t.Fatalf("err = %v, want ErrPlayerNotAlive", err)
		}
		if ent := portalEnt(t, e, id); ent.portalEpoch != 0 || ent.portalInFlight {
			t.Fatalf("epoch=%d inFlight=%v after rejection", ent.portalEpoch, ent.portalInFlight)
		}
		if len(r.prepares) != 0 || r.cancels != 1 {
			t.Fatalf("reservation = %d/%d, want 0/1", len(r.prepares), r.cancels)
		}
	})
	t.Run("nil reservation", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		id, _ := portalAwaitingPlayer(t, e, testCharacterID(), portalPending())
		if _, err := e.PlayerOrchestratePortalOfLife(id, portalInput(100, 50, 500), nil); !errors.Is(err, ErrInvalidDeathInput) {
			t.Fatalf("err = %v, want ErrInvalidDeathInput", err)
		}
		if ent := portalEnt(t, e, id); ent.portalEpoch != 0 || ent.portalInFlight {
			t.Fatalf("mutation on nil-reservation rejection")
		}
		live, _ := pendingOf(t, e, id)
		requirePendingEqual(t, live, PendingDeathRuntime{EffectiveCost: 80, DeathTimeSeconds: 100, CorpseID: int64ptr(500)}, "nil reservation")
	})
	aliveCases := map[string]struct {
		pending *PendingDeathRuntime
		input   PortalOfLifeResolvedInput
		want    error
	}{
		"no pending":       {nil, portalInput(100, 50, 500), ErrPortalUnavailable},
		"portal used":      {pendingFixture(80, 100, int64ptr(500), true), portalInput(100, 50, 500), ErrPortalUnavailable},
		"corpse expired":   {pendingFixture(80, 100, nil, false), portalInput(100, 50, 500), ErrPortalUnavailable},
		"target zero":      {portalPending(), portalInput(100, 50, 0), ErrPortalUnavailable},
		"target negative":  {portalPending(), portalInput(100, 50, -3), ErrPortalUnavailable},
		"target mismatch":  {portalPending(), portalInput(100, 50, 501), ErrPortalUnavailable},
		"now before death": {portalPending(), portalInput(99, 50, 500), ErrInvalidDeathTime},
		"now negative":     {portalPending(), PortalOfLifeResolvedInput{NowSeconds: -1, SpellPower: 50, TargetCorpseID: 500}, ErrInvalidDeathTime},
		"power zero":       {portalPending(), portalInput(100, 0, 500), ErrInvalidSpellPower},
		"power overflow":   {portalPending(), portalInput(100, 100, 500), ErrInvalidSpellPower},
	}
	for name, tc := range aliveCases {
		t.Run(name, func(t *testing.T) {
			e := newPlayerEngine(t, nil)
			var id EntityID
			if tc.pending == nil {
				snap, err := e.AddPlayerEntityWithDurableState(testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1}, testVitals(), testRuntimeInputs(), testFullDurableState())
				if err != nil {
					t.Fatal(err)
				}
				id = snap.ID
			} else {
				id = portalAlivePlayer(t, e, testCharacterID(), tc.pending)
			}
			r := &fakePortalReservation{}
			if _, err := e.PlayerOrchestratePortalOfLife(id, tc.input, r); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			ent := portalEnt(t, e, id)
			if ent.portalEpoch != 0 || ent.portalInFlight {
				t.Fatalf("epoch=%d inFlight=%v after rejection", ent.portalEpoch, ent.portalInFlight)
			}
			if len(r.prepares) != 0 || r.activates != 0 || r.cancels != 1 {
				t.Fatalf("reservation = %d/%d/%d, want 0/0/1", len(r.prepares), r.activates, r.cancels)
			}
			live, ok := pendingOf(t, e, id)
			if tc.pending == nil {
				if ok {
					t.Fatalf("pending appeared after rejection")
				}
			} else {
				requirePendingEqual(t, live, *tc.pending, "rejection preserves pending")
			}
		})
	}
	t.Run("missing durable", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		snap, err := e.AddPlayerEntity(testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1}, testVitals(), testRuntimeInputs())
		if err != nil {
			t.Fatal(err)
		}
		if err := e.PlayerInstallRecoveredPendingDeath(snap.ID, portalPending()); err != nil {
			t.Fatal(err)
		}
		r := &fakePortalReservation{}
		if _, err := e.PlayerOrchestratePortalOfLife(snap.ID, portalInput(100, 50, 500), r); !errors.Is(err, ErrPlayerDurableStateMissing) {
			t.Fatalf("err = %v, want ErrPlayerDurableStateMissing", err)
		}
		if len(r.prepares) != 0 || r.cancels != 1 {
			t.Fatalf("reservation = %d/%d, want 0/1", len(r.prepares), r.cancels)
		}
	})
}

// TestPortalInFlightSerialization proves a second begin
// while one attempt is live fails without Prepare, epoch
// change, or mutation.
func TestPortalInFlightSerialization(t *testing.T) {
	e := newPlayerEngine(t, nil)
	id, _ := portalAwaitingPlayer(t, e, testCharacterID(), portalPending())
	r := &fakePortalReservation{}
	beginPortal(t, e, id, portalInput(100, 50, 500), r)
	r2 := &fakePortalReservation{}
	if _, err := e.PlayerOrchestratePortalOfLife(id, portalInput(200, 50, 500), r2); !errors.Is(err, ErrPortalAttemptInFlight) {
		t.Fatalf("err = %v, want ErrPortalAttemptInFlight", err)
	}
	if ent := portalEnt(t, e, id); ent.portalEpoch != 1 || !ent.portalInFlight {
		t.Fatalf("epoch=%d inFlight=%v after duplicate begin", ent.portalEpoch, ent.portalInFlight)
	}
	if len(r2.prepares) != 0 || r2.activates != 0 || r2.cancels != 1 {
		t.Fatalf("second reservation = %d/%d/%d, want 0/0/1", len(r2.prepares), r2.activates, r2.cancels)
	}
	live, _ := pendingOf(t, e, id)
	requirePendingEqual(t, live, PendingDeathRuntime{EffectiveCost: 80, DeathTimeSeconds: 100, CorpseID: int64ptr(500)}, "serialization")
}

// TestPortalEpochExhaustion proves portalEpoch ==
// MaxUint64 rejects with zero mutation and no Prepare.
func TestPortalEpochExhaustion(t *testing.T) {
	e := newPlayerEngine(t, nil)
	id, _ := portalAwaitingPlayer(t, e, testCharacterID(), portalPending())
	portalEnt(t, e, id).portalEpoch = math.MaxUint64
	r := &fakePortalReservation{}
	if _, err := e.PlayerOrchestratePortalOfLife(id, portalInput(100, 50, 500), r); !errors.Is(err, ErrPortalAttemptExhausted) {
		t.Fatalf("err = %v, want ErrPortalAttemptExhausted", err)
	}
	if ent := portalEnt(t, e, id); ent.portalEpoch != math.MaxUint64 || ent.portalInFlight {
		t.Fatalf("mutation on exhaustion")
	}
	if len(r.prepares) != 0 || r.activates != 0 || r.cancels != 1 {
		t.Fatalf("reservation = %d/%d/%d, want 0/0/1", len(r.prepares), r.activates, r.cancels)
	}
}

// TestPortalPrepareFailure proves a Prepare error leaves
// no stranded attempt: epoch unchanged, not in flight,
// pending unchanged, Activate 0, Cancel exactly once.
func TestPortalPrepareFailure(t *testing.T) {
	e := newPlayerEngine(t, nil)
	id, _ := portalAwaitingPlayer(t, e, testCharacterID(), portalPending())
	beforeSnap, err := e.Entity(id)
	if err != nil {
		t.Fatal(err)
	}
	beforeDurable := durableOf(t, e, id)
	r := &fakePortalReservation{prepareErr: errors.New("portal queue saturated")}
	if _, err := e.PlayerOrchestratePortalOfLife(id, portalInput(100, 50, 500), r); err == nil || !errors.Is(err, r.prepareErr) {
		t.Fatalf("err = %v, want the Prepare error", err)
	}
	ent := portalEnt(t, e, id)
	if ent.portalEpoch != 0 || ent.portalInFlight {
		t.Fatalf("epoch=%d inFlight=%v after Prepare failure", ent.portalEpoch, ent.portalInFlight)
	}
	if len(r.prepares) != 1 || r.activates != 0 || r.cancels != 1 {
		t.Fatalf("reservation = %d/%d/%d, want 1/0/1", len(r.prepares), r.activates, r.cancels)
	}
	live, _ := pendingOf(t, e, id)
	requirePendingEqual(t, live, PendingDeathRuntime{EffectiveCost: 80, DeathTimeSeconds: 100, CorpseID: int64ptr(500)}, "prepare failure")
	requireDurableEqual(t, durableOf(t, e, id), beforeDurable, "prepare failure")
	if snap, _ := e.Entity(id); snap != beforeSnap {
		t.Fatalf("snapshot changed on Prepare failure")
	}
}

// TestPortalCaptureImmutability proves the Prepare-
// observed capture is deep-frozen: later live mutation
// of durable/pending buffers cannot reach it.
func TestPortalCaptureImmutability(t *testing.T) {
	e := newPlayerEngine(t, nil)
	id, _ := portalAwaitingPlayer(t, e, testCharacterID(), portalPending())
	r := &fakePortalReservation{}
	beginPortal(t, e, id, portalInput(100, 50, 500), r)
	got := r.prepares[0]

	ent := portalEnt(t, e, id)
	ent.durable.Advancement[0] = 'X'
	ent.durable.Spells[0].Ability = 1
	ent.durable.Skills[0].Ability = 99
	ent.durable.Items[0].Qty = -777
	ent.durable.Items[0].Enchants[0] = 'X'
	*ent.pendingDeath.CorpseID = 424242

	requireDurableEqual(t, got.Durable, testPostDeathDurableState(), "capture frozen")
	if got.PendingBefore.CorpseID == nil || *got.PendingBefore.CorpseID != 500 {
		t.Fatalf("capture corpse = %+v, want 500", got.PendingBefore.CorpseID)
	}
	if got.Position != (world.Vec3{X: 5, Y: 0, Z: 7}) {
		t.Fatalf("capture position = %+v", got.Position)
	}
}

// portalSuccessCompletion builds the authoritative
// success completion for the begun attempt.
func portalSuccessCompletion(res PortalOfLifeOrchestrationResult, corpse *int64) PortalOfLifeCompletion {
	return PortalOfLifeCompletion{
		Token: res.Token,
		Pending: PendingDeathRuntime{
			EffectiveCost:    res.ExpectedEffectiveCost,
			DeathTimeSeconds: 100,
			CorpseID:         corpse,
			PortalUsed:       true,
		},
	}
}

// TestPortalSuccessApply proves the exact matching
// completion replaces pending only, with ALL non-
// pending player state bit-identical.
func TestPortalSuccessApply(t *testing.T) {
	e := newPlayerEngine(t, nil)
	id, _ := portalAwaitingPlayer(t, e, testCharacterID(), portalPending())
	r := &fakePortalReservation{}
	res := beginPortal(t, e, id, portalInput(100, 50, 500), r)
	beforeSnap, err := e.Entity(id)
	if err != nil {
		t.Fatal(err)
	}
	beforeVitals, _, err := e.PlayerVitalsOf(id)
	if err != nil {
		t.Fatal(err)
	}
	beforeDurable := durableOf(t, e, id)

	disp, err := e.PlayerAcceptPortalOfLifeCompletion(portalSuccessCompletion(res, int64ptr(500)))
	if err != nil || disp != PortalCompletionApplied {
		t.Fatalf("completion = %d,%v; want Applied,nil", disp, err)
	}
	ent := portalEnt(t, e, id)
	if ent.portalInFlight || ent.portalEpoch != 1 {
		t.Fatalf("epoch=%d inFlight=%v after apply", ent.portalEpoch, ent.portalInFlight)
	}
	live, ok := pendingOf(t, e, id)
	if !ok {
		t.Fatalf("pending absent after apply")
	}
	requirePendingEqual(t, live, PendingDeathRuntime{EffectiveCost: 5, DeathTimeSeconds: 100, CorpseID: int64ptr(500), PortalUsed: true}, "apply")
	if snap, _ := e.Entity(id); snap != beforeSnap {
		t.Fatalf("snapshot changed by Portal apply")
	}
	if vitals, _, _ := e.PlayerVitalsOf(id); vitals != beforeVitals {
		t.Fatalf("vitals changed by Portal apply")
	}
	requireDurableEqual(t, durableOf(t, e, id), beforeDurable, "apply")
	if st, _ := lifeOf(t, e, id); st != PlayerLifeAwaitingRespawn {
		t.Fatalf("life = %d, want AwaitingRespawn", uint8(st))
	}
}

// TestPortalCompletionNilCorpse proves a valid
// authoritative success with CorpseID nil (corpse
// expired after commit before recovery).
func TestPortalCompletionNilCorpse(t *testing.T) {
	e := newPlayerEngine(t, nil)
	id, _ := portalAwaitingPlayer(t, e, testCharacterID(), portalPending())
	r := &fakePortalReservation{}
	res := beginPortal(t, e, id, portalInput(100, 50, 500), r)
	if disp, err := e.PlayerAcceptPortalOfLifeCompletion(portalSuccessCompletion(res, nil)); err != nil || disp != PortalCompletionApplied {
		t.Fatalf("nil-corpse completion = %d,%v; want Applied,nil", disp, err)
	}
	live, _ := pendingOf(t, e, id)
	requirePendingEqual(t, live, PendingDeathRuntime{EffectiveCost: 5, DeathTimeSeconds: 100, CorpseID: nil, PortalUsed: true}, "nil corpse")
}

// TestPortalCompletionMismatch proves every token or
// payload mismatch rejects with zero mutation.
func TestPortalCompletionMismatch(t *testing.T) {
	setup := func(t *testing.T) (*Engine, EntityID, PortalOfLifeOrchestrationResult, *fakePortalReservation) {
		t.Helper()
		e := newPlayerEngine(t, nil)
		id, _ := portalAwaitingPlayer(t, e, testCharacterID(), portalPending())
		r := &fakePortalReservation{}
		res := beginPortal(t, e, id, portalInput(100, 50, 500), r)
		return e, id, res, r
	}
	newCompletion := func(res PortalOfLifeOrchestrationResult, mutate func(*PortalOfLifeCompletion)) PortalOfLifeCompletion {
		c := portalSuccessCompletion(res, int64ptr(500))
		mutate(&c)
		return c
	}
	t.Run("unknown entity", func(t *testing.T) {
		e, _, res, _ := setup(t)
		bad := portalSuccessCompletion(res, int64ptr(500))
		bad.Token.EntityID = 9999
		if _, err := e.PlayerAcceptPortalOfLifeCompletion(bad); !errors.Is(err, ErrEntityNotFound) {
			t.Fatalf("err = %v, want ErrEntityNotFound", err)
		}
	})
	t.Run("wrong character", func(t *testing.T) {
		e, _, res, _ := setup(t)
		bad := newCompletion(res, func(c *PortalOfLifeCompletion) { c.Token.CharacterID = CharacterID(999) })
		if _, err := e.PlayerAcceptPortalOfLifeCompletion(bad); !errors.Is(err, ErrPortalAttemptMismatch) {
			t.Fatalf("err = %v, want ErrPortalAttemptMismatch", err)
		}
	})
	t.Run("wrong epoch", func(t *testing.T) {
		e, _, res, _ := setup(t)
		bad := newCompletion(res, func(c *PortalOfLifeCompletion) { c.Token.Epoch = 2 })
		if _, err := e.PlayerAcceptPortalOfLifeCompletion(bad); !errors.Is(err, ErrPortalAttemptMismatch) {
			t.Fatalf("err = %v, want ErrPortalAttemptMismatch", err)
		}
	})
	t.Run("zero epoch", func(t *testing.T) {
		e, _, res, _ := setup(t)
		bad := newCompletion(res, func(c *PortalOfLifeCompletion) { c.Token.Epoch = 0 })
		if _, err := e.PlayerAcceptPortalOfLifeCompletion(bad); !errors.Is(err, ErrPortalAttemptMismatch) {
			t.Fatalf("err = %v, want ErrPortalAttemptMismatch", err)
		}
	})
	mismatchPayloads := map[string]func(*PortalOfLifeCompletion){
		"wrong death time":  func(c *PortalOfLifeCompletion) { c.Pending.DeathTimeSeconds = 101 },
		"wrong cost":        func(c *PortalOfLifeCompletion) { c.Pending.EffectiveCost = 6 },
		"wrong corpse":      func(c *PortalOfLifeCompletion) { c.Pending.CorpseID = int64ptr(501) },
		"portal unused":     func(c *PortalOfLifeCompletion) { c.Pending.PortalUsed = false },
		"cost out of range": func(c *PortalOfLifeCompletion) { c.Pending.EffectiveCost = -1 },
	}
	for name, mutate := range mismatchPayloads {
		t.Run(name, func(t *testing.T) {
			e, id, res, _ := setup(t)
			bad := newCompletion(res, mutate)
			if _, err := e.PlayerAcceptPortalOfLifeCompletion(bad); err == nil {
				t.Fatalf("accepted invalid completion")
			}
			ent := portalEnt(t, e, id)
			if !ent.portalInFlight || ent.portalEpoch != 1 {
				t.Fatalf("attempt disturbed by mismatch")
			}
			live, _ := pendingOf(t, e, id)
			requirePendingEqual(t, live, PendingDeathRuntime{EffectiveCost: 80, DeathTimeSeconds: 100, CorpseID: int64ptr(500)}, "mismatch")
		})
	}
	t.Run("mismatched entity is another player", func(t *testing.T) {
		e, id, res, _ := setup(t)
		other, err := e.AddPlayerEntityWithDurableState(CharacterID(11), world.Vec3{X: 9, Y: 0, Z: 9}, testVitals(), testRuntimeInputs(), testFullDurableState())
		if err != nil {
			t.Fatal(err)
		}
		bad := portalSuccessCompletion(res, int64ptr(500))
		bad.Token.EntityID = other.ID
		if _, err := e.PlayerAcceptPortalOfLifeCompletion(bad); !errors.Is(err, ErrPortalAttemptMismatch) {
			t.Fatalf("err = %v, want ErrPortalAttemptMismatch", err)
		}
		live, _ := pendingOf(t, e, id)
		requirePendingEqual(t, live, PendingDeathRuntime{EffectiveCost: 80, DeathTimeSeconds: 100, CorpseID: int64ptr(500)}, "cross-entity")
	})
}

// TestPortalDuplicateBeforePayloadValidation proves a
// redelivered same-token completion with a malformed or
// different payload stays an exact zero-mutation
// Duplicate.
func TestPortalDuplicateBeforePayloadValidation(t *testing.T) {
	e := newPlayerEngine(t, nil)
	id, _ := portalAwaitingPlayer(t, e, testCharacterID(), portalPending())
	r := &fakePortalReservation{}
	res := beginPortal(t, e, id, portalInput(100, 50, 500), r)
	if _, err := e.PlayerAcceptPortalOfLifeCompletion(portalSuccessCompletion(res, int64ptr(500))); err != nil {
		t.Fatal(err)
	}
	beforeSnap, _ := e.Entity(id)
	beforePending, _ := pendingOf(t, e, id)
	mutants := []PortalOfLifeCompletion{
		{Token: res.Token, Pending: PendingDeathRuntime{EffectiveCost: -1, DeathTimeSeconds: 10, CorpseID: int64ptr(5)}},
		{Token: res.Token, Pending: PendingDeathRuntime{EffectiveCost: 5, DeathTimeSeconds: 100, CorpseID: int64ptr(500)}},
		{Token: res.Token, Pending: PendingDeathRuntime{EffectiveCost: 80, DeathTimeSeconds: 100, CorpseID: nil}},
	}
	for i, m := range mutants {
		disp, err := e.PlayerAcceptPortalOfLifeCompletion(m)
		if err != nil || disp != PortalCompletionDuplicate {
			t.Fatalf("mutant %d = %d,%v; want Duplicate,nil", i, disp, err)
		}
		if snap, _ := e.Entity(id); snap != beforeSnap {
			t.Fatalf("mutant %d snapshot changed", i)
		}
		live, _ := pendingOf(t, e, id)
		requirePendingEqual(t, live, beforePending, "duplicate")
	}
}

// TestPortalAbort proves first exact abort clears the
// attempt with pending unchanged, repeat abort is an
// idempotent no-op, a late success is a mismatch, and
// abort after success never undoes it.
func TestPortalAbort(t *testing.T) {
	e := newPlayerEngine(t, nil)
	id, _ := portalAwaitingPlayer(t, e, testCharacterID(), portalPending())
	r := &fakePortalReservation{}
	res := beginPortal(t, e, id, portalInput(100, 50, 500), r)

	disp, err := e.PlayerAbortPortalOfLifeAttempt(res.Token)
	if err != nil || disp != PortalAbortAborted {
		t.Fatalf("abort = %d,%v; want Aborted,nil", disp, err)
	}
	ent := portalEnt(t, e, id)
	if ent.portalInFlight || ent.portalEpoch != 1 {
		t.Fatalf("epoch=%d inFlight=%v after abort", ent.portalEpoch, ent.portalInFlight)
	}
	live, _ := pendingOf(t, e, id)
	requirePendingEqual(t, live, PendingDeathRuntime{EffectiveCost: 80, DeathTimeSeconds: 100, CorpseID: int64ptr(500)}, "abort preserves pending")

	again, err := e.PlayerAbortPortalOfLifeAttempt(res.Token)
	if err != nil || again != PortalAbortDuplicate {
		t.Fatalf("repeat abort = %d,%v; want Duplicate,nil", again, err)
	}
	if _, err := e.PlayerAcceptPortalOfLifeCompletion(portalSuccessCompletion(res, int64ptr(500))); !errors.Is(err, ErrPortalAttemptMismatch) {
		t.Fatalf("late success = %v, want ErrPortalAttemptMismatch", err)
	}
	live, _ = pendingOf(t, e, id)
	requirePendingEqual(t, live, PendingDeathRuntime{EffectiveCost: 80, DeathTimeSeconds: 100, CorpseID: int64ptr(500), PortalUsed: false}, "aborted attempt never succeeds")

	// A fresh attempt on the same pending still works
	// after abort (epoch advances, old token retires).
	r2 := &fakePortalReservation{}
	res2 := beginPortal(t, e, id, portalInput(100, 50, 500), r2)
	if res2.Token.Epoch != 2 {
		t.Fatalf("epoch = %d, want 2", res2.Token.Epoch)
	}
	if _, err := e.PlayerAcceptPortalOfLifeCompletion(portalSuccessCompletion(res2, int64ptr(500))); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlayerAbortPortalOfLifeAttempt(res2.Token); !errors.Is(err, ErrPortalAttemptMismatch) {
		t.Fatalf("abort-after-success = %v, want ErrPortalAttemptMismatch", err)
	}
	live, _ = pendingOf(t, e, id)
	requirePendingEqual(t, live, PendingDeathRuntime{EffectiveCost: 5, DeathTimeSeconds: 100, CorpseID: int64ptr(500), PortalUsed: true}, "success stands")
}

// TestPortalABA proves removing A and re-adding the
// same CharacterID as B leaves A's Portal completion
// orphaned: ErrEntityNotFound with B unchanged.
func TestPortalABA(t *testing.T) {
	e := newPlayerEngine(t, nil)
	a, _ := portalAwaitingPlayer(t, e, testCharacterID(), portalPending())
	r := &fakePortalReservation{}
	res := beginPortal(t, e, a, portalInput(100, 50, 500), r)
	if err := e.RemoveEntity(a); err != nil {
		t.Fatal(err)
	}
	b, err := e.AddPlayerEntityWithDurableState(testCharacterID(), world.Vec3{X: 9, Y: 0, Z: 9}, testVitals(), testRuntimeInputs(), testFullDurableState())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlayerAcceptPortalOfLifeCompletion(portalSuccessCompletion(res, int64ptr(500))); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("err = %v, want ErrEntityNotFound", err)
	}
	if _, ok, err := e.PlayerPendingDeathOf(b.ID); err != nil || ok {
		t.Fatalf("B pending = %v,%v; want absent,nil", ok, err)
	}
	if st, _ := lifeOf(t, e, b.ID); st != PlayerLifeAlive {
		t.Fatalf("B life = %d, want Alive", uint8(st))
	}
}

// TestPortalRespawnReleaseRace proves a Portal begun
// while AwaitingRespawn survives the respawn release to
// Alive and completes with life staying Alive.
func TestPortalRespawnReleaseRace(t *testing.T) {
	e := newPlayerEngine(t, nil)
	id, deathTok := portalAwaitingPlayer(t, e, testCharacterID(), portalPending())
	r := &fakePortalReservation{}
	res := beginPortal(t, e, id, portalInput(100, 50, 500), r)
	if _, disp, err := e.PlayerReleaseRespawn(deathTok); err != nil || disp != RespawnReleaseApplied {
		t.Fatalf("release = %d,%v; want Applied,nil", disp, err)
	}
	if disp, err := e.PlayerAcceptPortalOfLifeCompletion(portalSuccessCompletion(res, int64ptr(500))); err != nil || disp != PortalCompletionApplied {
		t.Fatalf("completion = %d,%v; want Applied,nil", disp, err)
	}
	if st, _ := lifeOf(t, e, id); st != PlayerLifeAlive {
		t.Fatalf("life = %d, want Alive", uint8(st))
	}
	live, _ := pendingOf(t, e, id)
	requirePendingEqual(t, live, PendingDeathRuntime{EffectiveCost: 5, DeathTimeSeconds: 100, CorpseID: int64ptr(500), PortalUsed: true}, "race")
}

// TestPortalHydrationGuard proves pending-death
// hydration is rejected while a Portal attempt is in
// flight, even with a malformed payload after the
// entity/life checks, with pending unchanged.
func TestPortalHydrationGuard(t *testing.T) {
	e := newPlayerEngine(t, nil)
	id := portalAlivePlayer(t, e, testCharacterID(), portalPending())
	r := &fakePortalReservation{}
	beginPortal(t, e, id, portalInput(100, 50, 500), r)
	for _, p := range []*PendingDeathRuntime{
		pendingFixture(10, 200, int64ptr(9), false),
		pendingFixture(-5, 200, int64ptr(9), false),
		nil,
	} {
		if err := e.PlayerInstallRecoveredPendingDeath(id, p); !errors.Is(err, ErrPortalAttemptInFlight) {
			t.Fatalf("hydration = %v, want ErrPortalAttemptInFlight", err)
		}
	}
	live, _ := pendingOf(t, e, id)
	requirePendingEqual(t, live, PendingDeathRuntime{EffectiveCost: 80, DeathTimeSeconds: 100, CorpseID: int64ptr(500)}, "hydration guard")
}

// prepareIngressPortal sets up an AwaitingRespawn player
// with pending and a live Portal attempt owner-locally
// (before Run starts), returning the orchestration
// result plus the authoritative completion.
func prepareIngressPortal(t *testing.T, e *Engine) PortalOfLifeOrchestrationResult {
	t.Helper()
	id, _ := portalAwaitingPlayer(t, e, testCharacterID(), portalPending())
	r := &fakePortalReservation{}
	res, err := e.PlayerOrchestratePortalOfLife(id, portalInput(100, 50, 500), r)
	if err != nil {
		t.Fatalf("PlayerOrchestratePortalOfLife: %v", err)
	}
	return res
}

func portalCompletionFor(res PortalOfLifeOrchestrationResult) PortalOfLifeCompletion {
	return portalSuccessCompletion(res, int64ptr(500))
}

// TestEnqueuePortalOfLifeCompletionSuccess drives a real
// Engine.Run and proves the typed completion executes on
// the owner with pending-only installation.
func TestEnqueuePortalOfLifeCompletionSuccess(t *testing.T) {
	clk := newManualClock()
	e := completionRunEngine(t, clk, openCollision{}, nil)
	res := prepareIngressPortal(t, e)
	completion := portalCompletionFor(res)
	cancel, done := runOwner(t, e, clk)
	defer stopOwner(t, cancel, done)

	disp, err := e.EnqueuePortalOfLifeCompletion(context.Background(), completion)
	if err != nil || disp != PortalCompletionApplied {
		t.Fatalf("completion = %d,%v; want Applied,nil", disp, err)
	}
	live, ok := pendingOf(t, e, res.Token.EntityID)
	if !ok {
		t.Fatalf("pending absent after ingress apply")
	}
	requirePendingEqual(t, live, PendingDeathRuntime{EffectiveCost: 5, DeathTimeSeconds: 100, CorpseID: int64ptr(500), PortalUsed: true}, "ingress success")
	if st, _ := lifeOf(t, e, res.Token.EntityID); st != PlayerLifeAwaitingRespawn {
		t.Fatalf("life = %d, want AwaitingRespawn", uint8(st))
	}
}

// TestEnqueuePortalOfLifeCompletionDuplicate proves a
// second delivery through ingress resolves as Duplicate.
func TestEnqueuePortalOfLifeCompletionDuplicate(t *testing.T) {
	clk := newManualClock()
	e := completionRunEngine(t, clk, openCollision{}, nil)
	res := prepareIngressPortal(t, e)
	completion := portalCompletionFor(res)
	cancel, done := runOwner(t, e, clk)
	defer stopOwner(t, cancel, done)

	ctx := context.Background()
	if disp, err := e.EnqueuePortalOfLifeCompletion(ctx, completion); err != nil || disp != PortalCompletionApplied {
		t.Fatalf("first = %d,%v", disp, err)
	}
	probe := captureLifeProbe(t, e, res.Token.EntityID)
	if disp, err := e.EnqueuePortalOfLifeCompletion(ctx, completion); err != nil || disp != PortalCompletionDuplicate {
		t.Fatalf("duplicate = %d,%v; want Duplicate,nil", disp, err)
	}
	requireLifeProbeUnchanged(t, e, res.Token.EntityID, probe, "ingress duplicate")
}

// TestEnqueuePortalOfLifeAbortSuccess proves the typed
// abort executes on the owner with pending unchanged.
func TestEnqueuePortalOfLifeAbortSuccess(t *testing.T) {
	clk := newManualClock()
	e := completionRunEngine(t, clk, openCollision{}, nil)
	res := prepareIngressPortal(t, e)
	cancel, done := runOwner(t, e, clk)
	defer stopOwner(t, cancel, done)

	disp, err := e.EnqueuePortalOfLifeAbort(context.Background(), res.Token)
	if err != nil || disp != PortalAbortAborted {
		t.Fatalf("abort = %d,%v; want Aborted,nil", disp, err)
	}
	live, _ := pendingOf(t, e, res.Token.EntityID)
	requirePendingEqual(t, live, PendingDeathRuntime{EffectiveCost: 80, DeathTimeSeconds: 100, CorpseID: int64ptr(500)}, "ingress abort")
}

// TestEnqueuePortalIngressAdmission proves pre-cancel,
// not-running, and full-mailbox admission semantics for
// both Portal commands on the same 256-command mailbox.
func TestEnqueuePortalIngressAdmission(t *testing.T) {
	t.Run("pre-cancel", func(t *testing.T) {
		clk := newManualClock()
		e := completionRunEngine(t, clk, openCollision{}, nil)
		res := prepareIngressPortal(t, e)
		cancel, done := runOwner(t, e, clk)
		defer stopOwner(t, cancel, done)
		probe := captureLifeProbe(t, e, res.Token.EntityID)

		ctx, stop := context.WithCancel(context.Background())
		stop()
		if _, err := e.EnqueuePortalOfLifeCompletion(ctx, portalCompletionFor(res)); !errors.Is(err, context.Canceled) {
			t.Fatalf("completion cancel = %v, want context.Canceled", err)
		}
		if _, err := e.EnqueuePortalOfLifeAbort(ctx, res.Token); !errors.Is(err, context.Canceled) {
			t.Fatalf("abort cancel = %v, want context.Canceled", err)
		}
		if n := len(e.ingress); n != 0 {
			t.Fatalf("mailbox len = %d, want 0", n)
		}
		requireLifeProbeUnchanged(t, e, res.Token.EntityID, probe, "pre-cancel")
	})
	t.Run("not-running", func(t *testing.T) {
		e := completionRunEngine(t, newManualClock(), openCollision{}, nil)
		res := prepareIngressPortal(t, e)
		if _, err := e.EnqueuePortalOfLifeCompletion(context.Background(), portalCompletionFor(res)); !errors.Is(err, ErrEngineNotRunning) {
			t.Fatalf("completion = %v, want ErrEngineNotRunning", err)
		}
		if _, err := e.EnqueuePortalOfLifeAbort(context.Background(), res.Token); !errors.Is(err, ErrEngineNotRunning) {
			t.Fatalf("abort = %v, want ErrEngineNotRunning", err)
		}
	})
	t.Run("full mailbox", func(t *testing.T) {
		col := newBlockingCollision()
		clk := newManualClock()
		e := completionRunEngine(t, clk, col, nil)
		res := prepareIngressPortal(t, e)
		cancel, done := runOwner(t, e, clk)

		blockPos := world.Vec3{X: 99}
		release := col.block(blockPos)
		first := make(chan ingressAddResult, 1)
		go func() {
			snap, err := e.EnqueueAddEntity(context.Background(), blockPos)
			first <- ingressAddResult{snap: snap, err: err}
		}()
		waitEntered(t, col, blockPos)

		results := make(chan ingressAddResult, SimIngressCapacity)
		for i := 0; i < SimIngressCapacity; i++ {
			go func(i int) {
				snap, err := e.EnqueueAddEntity(context.Background(), world.Vec3{X: float64(100 + i)})
				results <- ingressAddResult{snap: snap, err: err}
			}(i)
		}
		waitMailboxLen(t, e, SimIngressCapacity)
		if _, err := e.EnqueuePortalOfLifeCompletion(context.Background(), portalCompletionFor(res)); !errors.Is(err, ErrSimIngressFull) {
			t.Fatalf("completion full = %v, want ErrSimIngressFull", err)
		}
		if _, err := e.EnqueuePortalOfLifeAbort(context.Background(), res.Token); !errors.Is(err, ErrSimIngressFull) {
			t.Fatalf("abort full = %v, want ErrSimIngressFull", err)
		}
		close(release)
		if res := <-first; res.err != nil {
			t.Fatalf("first add: %v", res.err)
		}
		for i := 0; i < SimIngressCapacity; i++ {
			select {
			case r := <-results:
				if r.err != nil {
					t.Fatalf("queued add #%d: %v", i, r.err)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("timeout waiting for queued add #%d", i)
			}
		}
		if disp, err := e.EnqueuePortalOfLifeCompletion(context.Background(), portalCompletionFor(res)); err != nil || disp != PortalCompletionApplied {
			t.Fatalf("retry = %d,%v; want Applied,nil", disp, err)
		}
		live, _ := pendingOf(t, e, res.Token.EntityID)
		requirePendingEqual(t, live, PendingDeathRuntime{EffectiveCost: 5, DeathTimeSeconds: 100, CorpseID: int64ptr(500), PortalUsed: true}, "full-mailbox retry")
		stopOwner(t, cancel, done)
	})
}

// TestPortalCompletionCallerAliasing proves owner-local
// application deep-freezes the completion payload:
// caller mutation of the pointed CorpseID after the
// apply cannot reach live sim state.
func TestPortalCompletionCallerAliasing(t *testing.T) {
	e := newPlayerEngine(t, nil)
	id, _ := portalAwaitingPlayer(t, e, testCharacterID(), portalPending())
	r := &fakePortalReservation{}
	res := beginPortal(t, e, id, portalInput(100, 50, 500), r)
	corpse := int64(500)
	completion := PortalOfLifeCompletion{
		Token:   res.Token,
		Pending: PendingDeathRuntime{EffectiveCost: res.ExpectedEffectiveCost, DeathTimeSeconds: 100, CorpseID: &corpse, PortalUsed: true},
	}
	if _, err := e.PlayerAcceptPortalOfLifeCompletion(completion); err != nil {
		t.Fatal(err)
	}
	// Mutate the caller-owned pointed value after submission.
	corpse = 424242
	live, _ := pendingOf(t, e, id)
	requirePendingEqual(t, live, PendingDeathRuntime{EffectiveCost: 5, DeathTimeSeconds: 100, CorpseID: int64ptr(500), PortalUsed: true}, "caller aliasing")
}

// TestEnqueuePortalOfLifeCompletionAliasing proves the
// typed ingress delivers an independent payload copy:
// caller mutation after the synchronous round trip
// cannot reach live sim state.
func TestEnqueuePortalOfLifeCompletionAliasing(t *testing.T) {
	clk := newManualClock()
	e := completionRunEngine(t, clk, openCollision{}, nil)
	res := prepareIngressPortal(t, e)
	corpse := int64(500)
	completion := PortalOfLifeCompletion{
		Token:   res.Token,
		Pending: PendingDeathRuntime{EffectiveCost: res.ExpectedEffectiveCost, DeathTimeSeconds: 100, CorpseID: &corpse, PortalUsed: true},
	}
	cancel, done := runOwner(t, e, clk)
	defer stopOwner(t, cancel, done)

	if disp, err := e.EnqueuePortalOfLifeCompletion(context.Background(), completion); err != nil || disp != PortalCompletionApplied {
		t.Fatalf("completion = %d,%v; want Applied,nil", disp, err)
	}
	// Poison caller-owned buffers after the round trip.
	corpse = 666666
	live, _ := pendingOf(t, e, res.Token.EntityID)
	requirePendingEqual(t, live, PendingDeathRuntime{EffectiveCost: 5, DeathTimeSeconds: 100, CorpseID: int64ptr(500), PortalUsed: true}, "aliasing")
}
