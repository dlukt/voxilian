package sim

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/dlukt/voxilian/internal/world"
)

// M5-T5c3d3a authoritative Underworld-exit penalty owner
// lifecycle tests (spec §9.5.1k): Reserve-before-RNG
// ordering, source flag derivation, clear-flag mapping,
// raw-vs-scaled cost, RNG exact composition, ability
// application by Kind+ID, Portal-in-flight gate,
// life/pending failure matrix, corpse/portal-status
// non-gating, initial attempt install, Prepare failure,
// Activate lock-on-failure, exact retry without RNG,
// retry provider failure, retryable notification,
// gameplay quiesce, success completion, completion
// duplicate, completion mismatch, ABA, handoff/removal
// fields, capture immutability, and typed same-mailbox
// retryable/completion ingress. Deterministic, no wall
// clock (except the Run-based ingress tests, which reuse
// the existing owner harness), no Store/persist/gateway/
// proto, no Saver, no PG, no d3b executor.

// fakePenaltyReservation is the instrumented d3a
// DeathPenaltyWorkReservation: it records Prepare
// captures, activation/cancellation counts, scripted
// Prepare/Activate failures, and optional Prepare/Activate
// hooks (e.g. asserting life is already locked inside
// Prepare).
type fakePenaltyReservation struct {
	prepares    []DeathPenaltyCapture
	activates   int
	cancels     int
	prepareErr  error
	activateErr error
	onPrepare   func(DeathPenaltyCapture)
	onActivate  func()
}

func (f *fakePenaltyReservation) PrepareDeathPenaltyWork(c DeathPenaltyCapture) error {
	if f.onPrepare != nil {
		f.onPrepare(c)
	}
	f.prepares = append(f.prepares, c)
	return f.prepareErr
}

func (f *fakePenaltyReservation) ActivateDeathPenaltyWork() error {
	f.activates++
	if f.onActivate != nil {
		f.onActivate()
	}
	return f.activateErr
}

func (f *fakePenaltyReservation) CancelDeathPenaltyWork() { f.cancels++ }

// fakePenaltyProvider is the instrumented d3a
// DeathPenaltyWorkProvider: it counts Reserve calls,
// records requested CharacterIDs, applies a sticky
// Reserve failure, and mints one fake reservation per
// successful Reserve (copying the scripted
// Prepare/Activate behavior and hooks into each).
type fakePenaltyProvider struct {
	reserves     int
	reserveIDs   []CharacterID
	reserveErr   error
	reservations []*fakePenaltyReservation
	prepareErr   error
	activateErr  error
	onPrepare    func(DeathPenaltyCapture)
	onActivate   func()
}

func (p *fakePenaltyProvider) ReserveDeathPenaltyWork(id CharacterID) (DeathPenaltyWorkReservation, error) {
	p.reserves++
	p.reserveIDs = append(p.reserveIDs, id)
	if p.reserveErr != nil {
		return nil, p.reserveErr
	}
	r := &fakePenaltyReservation{prepareErr: p.prepareErr, activateErr: p.activateErr, onPrepare: p.onPrepare, onActivate: p.onActivate}
	p.reservations = append(p.reservations, r)
	return r, nil
}

func (p *fakePenaltyProvider) last() *fakePenaltyReservation {
	return p.reservations[len(p.reservations)-1]
}

// orderRNG wraps a scriptRNG appending one "rng" event per
// Uint64 draw, so tests can prove Reserve happens before
// the first RNG roll.
type orderRNG struct {
	script *scriptRNG
	events *[]string
}

func (o *orderRNG) Uint64() uint64 {
	*o.events = append(*o.events, "rng")
	return o.script.Uint64()
}

// errPenaltyReserveBoom is the scripted infrastructure
// Reserve failure: it must consume zero RNG.
var errPenaltyReserveBoom = errors.New("test: penalty reserve boom")

// errPenaltyActivateBoom is the scripted definitive
// pre-publication Activate failure: the owner must stay
// locked with the exact frozen capture.
var errPenaltyActivateBoom = errors.New("test: penalty activate boom")

// errPenaltyPrepareBoom is the scripted Prepare failure:
// the owner must stay locked with the exact frozen
// capture for exact-plan retry (never unwind to Alive).
var errPenaltyPrepareBoom = errors.New("test: penalty prepare boom")

// penaltyAbilities is the canonical d3a ability fixture:
// one spell + one skill, both eligible (> 5).
func penaltyAbilities() (spells, skills []PlayerAbilityState) {
	return []PlayerAbilityState{{ID: 11, Ability: 50}},
		[]PlayerAbilityState{{ID: 21, Ability: 40}}
}

// penaltyDurable builds a minimal valid durable shadow
// with the given flags and abilities.
func penaltyDurable(flags int32, spells, skills []PlayerAbilityState) PlayerDurableState {
	return PlayerDurableState{
		Karma:       150,
		Advancement: []byte(`{}`),
		Flags:       flags,
		Spells:      spells,
		Skills:      skills,
	}
}

// penaltyAlivePlayer builds an Alive full-state player
// carrying the given pending via authoritative hydration.
func penaltyAlivePlayer(t *testing.T, e *Engine, charID CharacterID, pos world.Vec3, pending *PendingDeathRuntime, durable PlayerDurableState) EntityID {
	t.Helper()
	snap, err := e.AddPlayerEntityWithDurableState(charID, pos, testVitals(), testRuntimeInputs(), durable)
	if err != nil {
		t.Fatalf("AddPlayerEntityWithDurableState: %v", err)
	}
	if err := e.PlayerInstallRecoveredPendingDeath(snap.ID, pending); err != nil {
		t.Fatalf("PlayerInstallRecoveredPendingDeath: %v", err)
	}
	return snap.ID
}

// penaltyHighPlayer builds an Alive full-state player with
// HP/BaseMaxHP/MaxHP all equal to hp (hp > 20 so a lost HP roll
// produces an actual MaxHP change; the T4a base floor is 20)
// carrying the given pending via authoritative hydration.
func penaltyHighPlayer(t *testing.T, e *Engine, charID CharacterID, pos world.Vec3, hp int, pending *PendingDeathRuntime, durable PlayerDurableState) EntityID {
	t.Helper()
	v := testVitals()
	v.HP, v.BaseMaxHP, v.MaxHP = hp, hp, hp
	if err := v.Validate(); err != nil {
		t.Fatalf("high vitals: %v", err)
	}
	snap, err := e.AddPlayerEntityWithDurableState(charID, pos, v, testRuntimeInputs(), durable)
	if err != nil {
		t.Fatalf("AddPlayerEntityWithDurableState: %v", err)
	}
	if err := e.PlayerInstallRecoveredPendingDeath(snap.ID, pending); err != nil {
		t.Fatalf("PlayerInstallRecoveredPendingDeath: %v", err)
	}
	return snap.ID
}

// penaltyInput is the canonical resolved Underworld-exit
// input: default cost 100, no frenzy.
func penaltyInput() UnderworldExitResolvedInput {
	return UnderworldExitResolvedInput{DefaultDeathCost: 100}
}

// beginPenalty is a fatal-on-error orchestration wrapper.
func beginPenalty(t *testing.T, e *Engine, id EntityID, input UnderworldExitResolvedInput, rng RNG, p DeathPenaltyWorkProvider) DeathPenaltyOrchestrationResult {
	t.Helper()
	res, err := e.PlayerOrchestrateDeathPenalties(id, input, rng, p)
	if err != nil {
		t.Fatalf("PlayerOrchestrateDeathPenalties(%d): %v", uint64(id), err)
	}
	return res
}

// penaltyEnt white-box-resolves the live entity.
func penaltyEnt(t *testing.T, e *Engine, id EntityID) *entity {
	t.Helper()
	ent, err := e.registry.lookup(id)
	if err != nil {
		t.Fatalf("lookup(%d): %v", uint64(id), err)
	}
	return ent
}

// TestDeathPenaltyReserveBeforeRNG proves the canonical
// first-attempt order begins Reserve, RNG..., Prepare,
// Activate, and that a Reserve failure consumes zero RNG
// with zero mutation.
func TestDeathPenaltyReserveBeforeRNG(t *testing.T) {
	spells, skills := penaltyAbilities()
	e := newPlayerEngine(t, nil)
	id := penaltyAlivePlayer(t, e, testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1},
		pendingFixture(90, 100, int64ptr(500), false), penaltyDurable(0x800, spells, skills))
	events := []string{}
	p := &fakePenaltyProvider{
		onPrepare:  func(DeathPenaltyCapture) { events = append(events, "prepare") },
		onActivate: func() { events = append(events, "activate") },
	}
	// Instrument Reserve ordering through a decorator provider.
	dec := &orderPenaltyProvider{inner: p, events: &events}
	rng := &orderRNG{script: d100Rolls(90, 26, 89, 30, 50), events: &events}
	res := beginPenalty(t, e, id, penaltyInput(), rng, dec)
	if res.Token.Epoch != 1 {
		t.Fatalf("epoch = %d, want 1", res.Token.Epoch)
	}
	if len(events) != 8 {
		t.Fatalf("events = %v, want [reserve rng*5 prepare activate]", events)
	}
	if events[0] != "reserve" || events[6] != "prepare" || events[7] != "activate" {
		t.Fatalf("events = %v, want reserve-first prepare/activate-last", events)
	}
	for _, ev := range events[1:6] {
		if ev != "rng" {
			t.Fatalf("events = %v, want 5 rng draws between reserve and prepare", events)
		}
	}
	if p.reserves != 1 || len(p.reserveIDs) != 1 || p.reserveIDs[0] != testCharacterID() {
		t.Fatalf("reserves = %d ids = %v, want 1x character", p.reserves, p.reserveIDs)
	}
}

// orderPenaltyProvider decorates Reserve with an event marker.
type orderPenaltyProvider struct {
	inner  *fakePenaltyProvider
	events *[]string
}

func (o *orderPenaltyProvider) ReserveDeathPenaltyWork(id CharacterID) (DeathPenaltyWorkReservation, error) {
	*o.events = append(*o.events, "reserve")
	return o.inner.ReserveDeathPenaltyWork(id)
}

// TestDeathPenaltyReserveFailureZeroRNG proves a provider
// Reserve failure consumes zero RNG and mutates nothing.
func TestDeathPenaltyReserveFailureZeroRNG(t *testing.T) {
	spells, skills := penaltyAbilities()
	e := newPlayerEngine(t, nil)
	id := penaltyAlivePlayer(t, e, testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1},
		pendingFixture(90, 100, int64ptr(500), false), penaltyDurable(0x800, spells, skills))
	p := &fakePenaltyProvider{reserveErr: errPenaltyReserveBoom}
	rng := d100Rolls(1, 2, 3, 4, 5)
	if _, err := e.PlayerOrchestrateDeathPenalties(id, penaltyInput(), rng, p); !errors.Is(err, errPenaltyReserveBoom) {
		t.Fatalf("err = %v, want reserve boom", err)
	}
	if rng.at != 0 {
		t.Fatalf("rng draws = %d, want 0", rng.at)
	}
	if st, _ := lifeOf(t, e, id); st != PlayerLifeAlive {
		t.Fatalf("life = %d, want Alive", uint8(st))
	}
	live, ok := pendingOf(t, e, id)
	if !ok {
		t.Fatalf("pending lost after reserve failure")
	}
	requirePendingEqual(t, live, PendingDeathRuntime{EffectiveCost: 90, DeathTimeSeconds: 100, CorpseID: int64ptr(500)}, "reserve failure preserves pending")
	if ent := penaltyEnt(t, e, id); ent.penaltyEpoch != 0 || ent.penaltyAttempt != nil {
		t.Fatalf("epoch=%d attempt=%v, want 0/nil", ent.penaltyEpoch, ent.penaltyAttempt != nil)
	}
}

// TestDeathPenaltyFlagDerivation proves still-newbie and
// murderer derive from the authoritative durable Flags
// and cannot be caller-overridden (the resolved input has
// no such fields).
func TestDeathPenaltyFlagDerivation(t *testing.T) {
	spells, skills := penaltyAbilities()
	cases := []struct {
		name       string
		flags      int32
		wantScaled bool
		wantHPRoll bool
		wantLoss   int // expected spell loss magnitude, -1 when no loss expected marker
		expectLoss bool
	}{
		{"still-newbie non-murderer scales without HP roll", 0x0, true, false, 0, false},
		{"experienced non-murderer rolls HP", 0x800, false, true, 1, true},
		{"murderer newbie rolls HP (no scaling)", 0x2, false, true, 2, true},
		{"murderer experienced rolls HP", 0x802, false, true, 2, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newPlayerEngine(t, nil)
			id := penaltyAlivePlayer(t, e, testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1},
				pendingFixture(90, 100, int64ptr(500), false), penaltyDurable(tc.flags, spells, skills))
			p := &fakePenaltyProvider{}
			// All stamina saves fail, all cost rolls lose:
			// spell stamina, spell cost, skill stamina, skill cost,
			// plus the HP roll first when it happens.
			var res DeathPenaltyOrchestrationResult
			var err error
			if tc.wantHPRoll {
				res, err = e.PlayerOrchestrateDeathPenalties(id, penaltyInput(), d100Rolls(90, 26, 1, 26, 1), p)
			} else {
				res, err = e.PlayerOrchestrateDeathPenalties(id, penaltyInput(), d100Rolls(26, 1, 26, 1), p)
			}
			if err != nil {
				t.Fatalf("orchestrate: %v", err)
			}
			if res.Plan.CostScaled != tc.wantScaled {
				t.Fatalf("CostScaled = %v, want %v", res.Plan.CostScaled, tc.wantScaled)
			}
			if res.Plan.HPRolled != tc.wantHPRoll {
				t.Fatalf("HPRolled = %v, want %v", res.Plan.HPRolled, tc.wantHPRoll)
			}
			if !tc.expectLoss {
				return
			}
			if len(res.Plan.AbilityLosses) == 0 {
				t.Fatalf("no ability losses, want spell loss magnitude %d", tc.wantLoss)
			}
			if res.Plan.AbilityLosses[0].Loss != tc.wantLoss {
				t.Fatalf("spell loss = %d, want %d", res.Plan.AbilityLosses[0].Loss, tc.wantLoss)
			}
		})
	}
}

// TestDeathPenaltyClearFlagMapping pins the full-cost
// OUTLAW/HAUNTED clears (MURDERER/TUTORIAL/unrelated
// preserved) and the frenzy HAUNTED-only clear with no
// HP/ability penalty.
func TestDeathPenaltyClearFlagMapping(t *testing.T) {
	spells, skills := penaltyAbilities()
	t.Run("full cost clears outlaw+haunted only", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		const flags = int32(0x490B) // OUTLAW|HAUNTED|MURDERER|TUTORIAL|0x4000|0x1
		id := penaltyAlivePlayer(t, e, testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1},
			pendingFixture(100, 100, int64ptr(500), false), penaltyDurable(flags, spells, skills))
		p := &fakePenaltyProvider{}
		beginPenalty(t, e, id, penaltyInput(), d100Rolls(100, 26, 99, 26, 99), p)
		got := p.last().prepares[0].Durable.Flags
		if got != int32(0x4803) {
			t.Fatalf("post flags = %#x, want 0x4803", got)
		}
	})
	t.Run("frenzy clears haunted only with no penalties", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		const flags = int32(0x108) // OUTLAW|HAUNTED
		id := penaltyAlivePlayer(t, e, testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1},
			pendingFixture(90, 100, int64ptr(500), false), penaltyDurable(flags, spells, skills))
		p := &fakePenaltyProvider{}
		in := UnderworldExitResolvedInput{DefaultDeathCost: 100, FrenzyActive: true}
		res := beginPenalty(t, e, id, in, d100Rolls(1), p)
		if !res.Plan.ClearHaunted || res.Plan.ClearOutlaw {
			t.Fatalf("plan = %+v, want haunted-only clear", res.Plan)
		}
		if res.Plan.HPRolled || len(res.Plan.AbilityLosses) != 0 {
			t.Fatalf("frenzy plan has penalties: %+v", res.Plan)
		}
		if res.Plan.VitalsAfter != testVitals() {
			t.Fatalf("frenzy vitals changed: %+v", res.Plan.VitalsAfter)
		}
		if got := p.last().prepares[0].Durable.Flags; got != int32(0x8) {
			t.Fatalf("post flags = %#x, want 0x8", got)
		}
	})
}

// TestDeathPenaltyRawVsScaledCost proves the capture keeps
// both the scaled plan cost and the raw pending cost
// separately (future Store mapping must use the raw one).
func TestDeathPenaltyRawVsScaledCost(t *testing.T) {
	spells, skills := penaltyAbilities()
	e := newPlayerEngine(t, nil)
	id := penaltyAlivePlayer(t, e, testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1},
		pendingFixture(90, 100, int64ptr(500), false), penaltyDurable(0x0, spells, skills))
	p := &fakePenaltyProvider{}
	res := beginPenalty(t, e, id, penaltyInput(), d100Rolls(26, 29, 26, 29), p)
	if res.Plan.ScaledCost != 30 {
		t.Fatalf("ScaledCost = %d, want 30", res.Plan.ScaledCost)
	}
	cap := p.last().prepares[0]
	if cap.PendingBefore.EffectiveCost != 90 {
		t.Fatalf("PendingBefore.EffectiveCost = %d, want 90", cap.PendingBefore.EffectiveCost)
	}
	if cap.Plan.ScaledCost != 30 {
		t.Fatalf("capture plan ScaledCost = %d, want 30", cap.Plan.ScaledCost)
	}
}

// TestDeathPenaltyRNGExactComposition proves the owner
// calls PlanDeathPenalties exactly once with the golden
// HP/spell/skill ordering, matching a direct call with
// the same scripted sequence.
func TestDeathPenaltyRNGExactComposition(t *testing.T) {
	spells, skills := penaltyAbilities()
	newEngine := func(t *testing.T) (*Engine, EntityID) {
		e := newPlayerEngine(t, nil)
		id := penaltyAlivePlayer(t, e, testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1},
			pendingFixture(90, 100, int64ptr(500), false), penaltyDurable(0x800, spells, skills))
		return e, id
	}
	e, id := newEngine(t)
	p := &fakePenaltyProvider{}
	rng := d100Rolls(90, 26, 89, 30, 50)
	res := beginPenalty(t, e, id, penaltyInput(), rng, p)
	if rng.at != 5 {
		t.Fatalf("owner rng draws = %d, want exactly 5 (one planner call)", rng.at)
	}
	want, err := PlanDeathPenalties(d100Rolls(90, 26, 89, 30, 50), DeathPenaltyInput{
		PendingCost: 90, DefaultCost: 100,
		StillNewbie: false, Murderer: false, Stamina: 25,
		Vitals: testVitals(),
		Spells: []DeathAbilityInput{{Key: 11, Ability: 50}},
		Skills: []DeathAbilityInput{{Key: 21, Ability: 40}},
	})
	if err != nil {
		t.Fatalf("direct PlanDeathPenalties: %v", err)
	}
	if !reflect.DeepEqual(res.Plan, want) {
		t.Fatalf("owner plan = %+v, want direct %+v", res.Plan, want)
	}
	if len(res.Plan.AbilityLosses) != 2 {
		t.Fatalf("losses = %+v, want spell+skill loss", res.Plan.AbilityLosses)
	}
}

// TestDeathPenaltyAbilityApplication proves losses apply
// by Kind+ID with overlapping numeric IDs, preserving
// order/membership/AtrophyFlag, and that an impossible
// loss is rejected.
func TestDeathPenaltyAbilityApplication(t *testing.T) {
	newDurable := func() PlayerDurableState {
		return penaltyDurable(0x0,
			[]PlayerAbilityState{{ID: 7, Ability: 50}, {ID: 8, Ability: 60, AtrophyFlag: true}},
			[]PlayerAbilityState{{ID: 7, Ability: 40}})
	}
	t.Run("kind+id application preserves order and flags", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		id := penaltyAlivePlayer(t, e, testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1},
			pendingFixture(90, 100, int64ptr(500), false), newDurable())
		p := &fakePenaltyProvider{}
		// Newbie: no HP roll. Spell 7: stamina fail, cost lose.
		// Spell 8: stamina save (no cost roll). Skill 7:
		// stamina fail, cost save (roll == scaled cost).
		res := beginPenalty(t, e, id, penaltyInput(), d100Rolls(26, 29, 25, 30, 30), p)
		if len(res.Plan.AbilityLosses) != 1 {
			t.Fatalf("losses = %+v, want exactly spell 7", res.Plan.AbilityLosses)
		}
		loss := res.Plan.AbilityLosses[0]
		if loss.Kind != DeathAbilitySpell || loss.Key != 7 || loss.FromAbility != 50 || loss.ToAbility != 49 {
			t.Fatalf("loss = %+v, want spell 7 50->49", loss)
		}
		disp, err := e.PlayerAcceptDeathPenaltyCompletion(DeathPenaltyCompletion{Token: res.Token})
		if err != nil || disp != DeathPenaltyCompletionApplied {
			t.Fatalf("completion = %d,%v; want Applied,nil", disp, err)
		}
		ent := penaltyEnt(t, e, id)
		wantSpells := []PlayerAbilityState{{ID: 7, Ability: 49}, {ID: 8, Ability: 60, AtrophyFlag: true}}
		wantSkills := []PlayerAbilityState{{ID: 7, Ability: 40}}
		if !reflect.DeepEqual(ent.durable.Spells, wantSpells) {
			t.Fatalf("spells = %+v, want %+v", ent.durable.Spells, wantSpells)
		}
		if !reflect.DeepEqual(ent.durable.Skills, wantSkills) {
			t.Fatalf("skills = %+v, want %+v", ent.durable.Skills, wantSkills)
		}
	})
	t.Run("impossible loss rejected", func(t *testing.T) {
		d := newDurable()
		plan := DeathPenaltyPlan{
			AbilityLosses: []DeathAbilityLoss{{Kind: DeathAbilitySpell, Key: 7, FromAbility: 999, ToAbility: 49, Loss: 1}},
		}
		if _, err := applyDeathPenaltyPlanToDurable(d, plan); err == nil {
			t.Fatalf("mismatched FromAbility accepted")
		}
		absent := DeathPenaltyPlan{
			AbilityLosses: []DeathAbilityLoss{{Kind: DeathAbilitySkill, Key: 4242, FromAbility: 40, ToAbility: 39, Loss: 1}},
		}
		if _, err := applyDeathPenaltyPlanToDurable(d, absent); err == nil {
			t.Fatalf("absent key accepted")
		}
	})
}

// TestDeathPenaltyPortalInFlightGate proves a live Portal
// attempt blocks the Underworld exit before provider/RNG.
func TestDeathPenaltyPortalInFlightGate(t *testing.T) {
	e := newPlayerEngine(t, nil)
	id := penaltyAlivePlayer(t, e, testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1},
		portalPending(), testFullDurableState())
	r := &fakePortalReservation{}
	if _, err := e.PlayerOrchestratePortalOfLife(id, portalInput(100, 50, 500), r); err != nil {
		t.Fatalf("portal begin: %v", err)
	}
	p := &fakePenaltyProvider{}
	rng := d100Rolls(1, 2, 3)
	if _, err := e.PlayerOrchestrateDeathPenalties(id, penaltyInput(), rng, p); !errors.Is(err, ErrPortalAttemptInFlight) {
		t.Fatalf("err = %v, want ErrPortalAttemptInFlight", err)
	}
	if p.reserves != 0 || rng.at != 0 {
		t.Fatalf("reserves=%d rng=%d, want 0/0", p.reserves, rng.at)
	}
	if ent := penaltyEnt(t, e, id); ent.penaltyEpoch != 0 || ent.penaltyAttempt != nil {
		t.Fatalf("mutated under portal gate")
	}
}

// TestDeathPenaltyBeginFailures proves every life/pending
// failure rejects before provider/RNG with zero mutation.
func TestDeathPenaltyBeginFailures(t *testing.T) {
	spells, skills := penaltyAbilities()
	setupAlive := func(t *testing.T, e *Engine) EntityID {
		return penaltyAlivePlayer(t, e, testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1},
			pendingFixture(90, 100, int64ptr(500), false), penaltyDurable(0x800, spells, skills))
	}
	t.Run("unknown entity", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		p := &fakePenaltyProvider{}
		rng := d100Rolls(1)
		if _, err := e.PlayerOrchestrateDeathPenalties(9999, penaltyInput(), rng, p); !errors.Is(err, ErrEntityNotFound) {
			t.Fatalf("err = %v, want ErrEntityNotFound", err)
		}
		if p.reserves != 0 || rng.at != 0 {
			t.Fatalf("reserves=%d rng=%d, want 0/0", p.reserves, rng.at)
		}
	})
	t.Run("generic entity", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		snap, err := e.AddEntity(world.Vec3{X: 1, Y: 0, Z: 1})
		if err != nil {
			t.Fatal(err)
		}
		p := &fakePenaltyProvider{}
		rng := d100Rolls(1)
		if _, err := e.PlayerOrchestrateDeathPenalties(snap.ID, penaltyInput(), rng, p); !errors.Is(err, ErrEntityNotPlayer) {
			t.Fatalf("err = %v, want ErrEntityNotPlayer", err)
		}
		if p.reserves != 0 || rng.at != 0 {
			t.Fatalf("reserves=%d rng=%d, want 0/0", p.reserves, rng.at)
		}
	})
	t.Run("migrating", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		id := setupAlive(t, e)
		ent := penaltyEnt(t, e, id)
		destPos := world.Vec3{X: 33.5, Y: 0, Z: 1}
		dest, err := world.CellForPosition(destPos)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.registry.beginHandoff(id, OwnerRef{Cell: ent.cell, Generation: ent.generation}, dest, destPos); err != nil {
			t.Fatal(err)
		}
		p := &fakePenaltyProvider{}
		rng := d100Rolls(1)
		if _, err := e.PlayerOrchestrateDeathPenalties(id, penaltyInput(), rng, p); !errors.Is(err, ErrCellHandoffRequired) {
			t.Fatalf("err = %v, want ErrCellHandoffRequired", err)
		}
		if p.reserves != 0 || rng.at != 0 {
			t.Fatalf("reserves=%d rng=%d, want 0/0", p.reserves, rng.at)
		}
	})
	t.Run("awaiting respawn", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		id, _ := portalAwaitingPlayer(t, e, testCharacterID(), portalPending())
		p := &fakePenaltyProvider{}
		rng := d100Rolls(1)
		if _, err := e.PlayerOrchestrateDeathPenalties(id, penaltyInput(), rng, p); !errors.Is(err, ErrPlayerNotAlive) {
			t.Fatalf("err = %v, want ErrPlayerNotAlive", err)
		}
		if p.reserves != 0 || rng.at != 0 {
			t.Fatalf("reserves=%d rng=%d, want 0/0", p.reserves, rng.at)
		}
	})
	t.Run("death persisting", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		id, _ := pendingCompletionPlayer(t, e, testCharacterID())
		p := &fakePenaltyProvider{}
		rng := d100Rolls(1)
		if _, err := e.PlayerOrchestrateDeathPenalties(id, penaltyInput(), rng, p); !errors.Is(err, ErrPlayerNotAlive) {
			t.Fatalf("err = %v, want ErrPlayerNotAlive", err)
		}
		if p.reserves != 0 || rng.at != 0 {
			t.Fatalf("reserves=%d rng=%d, want 0/0", p.reserves, rng.at)
		}
	})
	t.Run("penalty already active", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		id := setupAlive(t, e)
		p := &fakePenaltyProvider{}
		beginPenalty(t, e, id, penaltyInput(), d100Rolls(90, 26, 89, 30, 50), p)
		rng := d100Rolls(1)
		if _, err := e.PlayerOrchestrateDeathPenalties(id, penaltyInput(), rng, p); !errors.Is(err, ErrDeathPenaltyPersistenceActive) {
			t.Fatalf("err = %v, want ErrDeathPenaltyPersistenceActive", err)
		}
		if rng.at != 0 {
			t.Fatalf("rng draws = %d, want 0", rng.at)
		}
		if ent := penaltyEnt(t, e, id); ent.penaltyEpoch != 1 {
			t.Fatalf("epoch = %d, want still 1", ent.penaltyEpoch)
		}
	})
	t.Run("no pending", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		snap, err := e.AddPlayerEntityWithDurableState(testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1}, testVitals(), testRuntimeInputs(), penaltyDurable(0x800, spells, skills))
		if err != nil {
			t.Fatal(err)
		}
		p := &fakePenaltyProvider{}
		rng := d100Rolls(1)
		if _, err := e.PlayerOrchestrateDeathPenalties(snap.ID, penaltyInput(), rng, p); !errors.Is(err, ErrDeathPenaltyUnavailable) {
			t.Fatalf("err = %v, want ErrDeathPenaltyUnavailable", err)
		}
		if p.reserves != 0 || rng.at != 0 {
			t.Fatalf("reserves=%d rng=%d, want 0/0", p.reserves, rng.at)
		}
	})
	t.Run("missing durable", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		snap, err := e.AddPlayerEntity(testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1}, testVitals(), testRuntimeInputs())
		if err != nil {
			t.Fatal(err)
		}
		if err := e.PlayerInstallRecoveredPendingDeath(snap.ID, pendingFixture(90, 100, int64ptr(500), false)); err != nil {
			t.Fatal(err)
		}
		p := &fakePenaltyProvider{}
		rng := d100Rolls(1)
		if _, err := e.PlayerOrchestrateDeathPenalties(snap.ID, penaltyInput(), rng, p); !errors.Is(err, ErrPlayerDurableStateMissing) {
			t.Fatalf("err = %v, want ErrPlayerDurableStateMissing", err)
		}
		if p.reserves != 0 || rng.at != 0 {
			t.Fatalf("reserves=%d rng=%d, want 0/0", p.reserves, rng.at)
		}
	})
	t.Run("invalid runtime inputs", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		id := setupAlive(t, e)
		penaltyEnt(t, e, id).runtimeInputs.EffectiveStamina = 0
		p := &fakePenaltyProvider{}
		rng := d100Rolls(1)
		if _, err := e.PlayerOrchestrateDeathPenalties(id, penaltyInput(), rng, p); !errors.Is(err, ErrInvalidCombatStat) {
			t.Fatalf("err = %v, want ErrInvalidCombatStat", err)
		}
		if p.reserves != 0 || rng.at != 0 {
			t.Fatalf("reserves=%d rng=%d, want 0/0", p.reserves, rng.at)
		}
	})
	t.Run("invalid default cost", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		id := setupAlive(t, e)
		p := &fakePenaltyProvider{}
		rng := d100Rolls(1)
		in := UnderworldExitResolvedInput{DefaultDeathCost: 0}
		if _, err := e.PlayerOrchestrateDeathPenalties(id, in, rng, p); !errors.Is(err, ErrInvalidDeathCost) {
			t.Fatalf("err = %v, want ErrInvalidDeathCost", err)
		}
		if p.reserves != 0 || rng.at != 0 {
			t.Fatalf("reserves=%d rng=%d, want 0/0", p.reserves, rng.at)
		}
	})
	t.Run("nil rng", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		id := setupAlive(t, e)
		p := &fakePenaltyProvider{}
		if _, err := e.PlayerOrchestrateDeathPenalties(id, penaltyInput(), nil, p); !errors.Is(err, ErrNilRNG) {
			t.Fatalf("err = %v, want ErrNilRNG", err)
		}
		if p.reserves != 0 {
			t.Fatalf("reserves = %d, want 0", p.reserves)
		}
		if st, _ := lifeOf(t, e, id); st != PlayerLifeAlive {
			t.Fatalf("life = %d, want Alive", uint8(st))
		}
	})
	t.Run("epoch exhausted", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		id := setupAlive(t, e)
		penaltyEnt(t, e, id).penaltyEpoch = math.MaxUint64
		p := &fakePenaltyProvider{}
		rng := d100Rolls(1)
		if _, err := e.PlayerOrchestrateDeathPenalties(id, penaltyInput(), rng, p); !errors.Is(err, ErrDeathPenaltyAttemptExhausted) {
			t.Fatalf("err = %v, want ErrDeathPenaltyAttemptExhausted", err)
		}
		if p.reserves != 0 || rng.at != 0 {
			t.Fatalf("reserves=%d rng=%d, want 0/0", p.reserves, rng.at)
		}
		if ent := penaltyEnt(t, e, id); ent.penaltyEpoch != math.MaxUint64 || ent.penaltyAttempt != nil {
			t.Fatalf("epoch mutated on exhaustion")
		}
	})
	t.Run("nil provider", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		id := setupAlive(t, e)
		rng := d100Rolls(1)
		if _, err := e.PlayerOrchestrateDeathPenalties(id, penaltyInput(), rng, nil); err == nil {
			t.Fatalf("nil provider accepted")
		}
		if rng.at != 0 {
			t.Fatalf("rng draws = %d, want 0", rng.at)
		}
		if st, _ := lifeOf(t, e, id); st != PlayerLifeAlive {
			t.Fatalf("life = %d, want Alive", uint8(st))
		}
	})
}

// TestDeathPenaltyCorpsePortalStatusNoGate proves nil and
// non-nil CorpseID plus used and unused Portal state are
// all accepted when pending exists.
func TestDeathPenaltyCorpsePortalStatusNoGate(t *testing.T) {
	spells, skills := penaltyAbilities()
	corpses := map[string]*int64{"nil corpse": nil, "live corpse": int64ptr(500)}
	portals := map[string]bool{"unused": false, "used": true}
	for cn, corpse := range corpses {
		for pn, used := range portals {
			t.Run(cn+"/"+pn, func(t *testing.T) {
				e := newPlayerEngine(t, nil)
				id := penaltyAlivePlayer(t, e, testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1},
					pendingFixture(90, 100, corpse, used), penaltyDurable(0x0, spells, skills))
				p := &fakePenaltyProvider{}
				res := beginPenalty(t, e, id, penaltyInput(), d100Rolls(26, 29, 26, 29), p)
				if res.Token.Epoch != 1 {
					t.Fatalf("epoch = %d, want 1", res.Token.Epoch)
				}
			})
		}
	}
}

// TestUnderworldExitPenaltyAttempt proves Prepare runs after
// the attempt install with life already locked, the post-state
// is not yet live, and the attempt install locks gameplay with
// persistence active.
func TestUnderworldExitPenaltyAttempt(t *testing.T) {
	spells, skills := penaltyAbilities()
	e := newPlayerEngine(t, nil)
	id := penaltyAlivePlayer(t, e, testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1},
		pendingFixture(90, 100, int64ptr(500), false), penaltyDurable(0x800, spells, skills))
	preVitals := penaltyEnt(t, e, id).vitals
	preDurable := freezePlayerDurableState(*penaltyEnt(t, e, id).durable)
	var prepareLife PlayerLifeState
	prepareSeen := false
	p := &fakePenaltyProvider{onPrepare: func(DeathPenaltyCapture) {
		st, _, _ := e.PlayerLifeStateOf(id)
		prepareLife, prepareSeen = st, true
	}}
	res := beginPenalty(t, e, id, penaltyInput(), d100Rolls(90, 26, 89, 30, 50), p)
	if !prepareSeen || prepareLife != PlayerLifeDeathPenaltyPersisting {
		t.Fatalf("prepare life seen=%v life=%d, want true/PenaltyPersisting", prepareSeen, uint8(prepareLife))
	}
	ent := penaltyEnt(t, e, id)
	if ent.penaltyEpoch != 1 {
		t.Fatalf("epoch = %d, want 1", ent.penaltyEpoch)
	}
	if ent.penaltyAttempt == nil || !ent.penaltyAttempt.persistenceActive {
		t.Fatalf("attempt missing or inactive after begin")
	}
	if res.Token != (DeathPenaltyAttemptToken{EntityID: id, CharacterID: testCharacterID(), Epoch: 1}) {
		t.Fatalf("token = %+v", res.Token)
	}
	if ent.penaltyAttempt.capture.Token != res.Token {
		t.Fatalf("capture token mismatch")
	}
	if st, _ := lifeOf(t, e, id); st != PlayerLifeDeathPenaltyPersisting {
		t.Fatalf("life = %d, want PenaltyPersisting", uint8(st))
	}
	r := p.last()
	if len(r.prepares) != 1 || r.activates != 1 || r.cancels != 0 {
		t.Fatalf("reservation = %d/%d/%d, want 1/1/0", len(r.prepares), r.activates, r.cancels)
	}
	// Live state is STILL pre-penalty: the planned
	// post-state lives only in the private capture.
	if ent.vitals != preVitals {
		t.Fatalf("live vitals mutated before completion")
	}
	if !reflect.DeepEqual(*ent.durable, preDurable) {
		t.Fatalf("live durable mutated before completion")
	}
	live, _ := pendingOf(t, e, id)
	requirePendingEqual(t, live, PendingDeathRuntime{EffectiveCost: 90, DeathTimeSeconds: 100, CorpseID: int64ptr(500)}, "begin preserves pending")
}

// TestDeathPenaltyPrepareFailureLocks proves an initial Prepare
// error after the private attempt was installed Cancels once with
// zero Activates AND retains the gameplay lock: life stays
// `PlayerLifeDeathPenaltyPersisting`, the penalty epoch is
// consumed exactly once, the private attempt is present with
// `persistenceActive == false`, pending is unchanged, the stored
// capture equals the exact capture passed to Prepare, an ordinary
// second LeaveHold does not reroll, and retry reuses the exact
// capture with zero additional RNG.
func TestDeathPenaltyPrepareFailureLocks(t *testing.T) {
	spells, skills := penaltyAbilities()
	e := newPlayerEngine(t, nil)
	id := penaltyAlivePlayer(t, e, testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1},
		pendingFixture(90, 100, int64ptr(500), false), penaltyDurable(0x800, spells, skills))
	events := []string{}
	p := &fakePenaltyProvider{
		prepareErr: errPenaltyPrepareBoom,
		onPrepare:  func(DeathPenaltyCapture) { events = append(events, "prepare") },
		onActivate: func() { events = append(events, "activate") },
	}
	dec := &orderPenaltyProvider{inner: p, events: &events}
	rng := &orderRNG{script: d100Rolls(90, 26, 89, 30, 50), events: &events}
	if _, err := e.PlayerOrchestrateDeathPenalties(id, penaltyInput(), rng, dec); !errors.Is(err, errPenaltyPrepareBoom) {
		t.Fatalf("err = %v, want prepare boom", err)
	}
	// Reserve succeeded before RNG: exactly one plan's RNG draws.
	if len(events) < 7 || events[0] != "reserve" {
		t.Fatalf("events = %v, want reserve-first", events)
	}
	for _, ev := range events[1:6] {
		if ev != "rng" {
			t.Fatalf("events = %v, want 5 rng draws between reserve and prepare", events)
		}
	}
	if events[6] != "prepare" {
		t.Fatalf("events = %v, want prepare after rng", events)
	}
	for _, ev := range events {
		if ev == "activate" {
			t.Fatalf("events = %v, want zero activates", events)
		}
	}
	r := p.last()
	if r.cancels != 1 || r.activates != 0 {
		t.Fatalf("reservation cancels=%d activates=%d, want 1/0", r.cancels, r.activates)
	}
	if len(r.prepares) != 1 {
		t.Fatalf("prepares = %d, want exactly 1", len(r.prepares))
	}
	ent := penaltyEnt(t, e, id)
	if st, _ := lifeOf(t, e, id); st != PlayerLifeDeathPenaltyPersisting {
		t.Fatalf("life = %d, want PenaltyPersisting", uint8(st))
	}
	if ent.penaltyEpoch != 1 {
		t.Fatalf("epoch = %d, want consumed exactly once", ent.penaltyEpoch)
	}
	if ent.penaltyAttempt == nil {
		t.Fatalf("private attempt missing after prepare failure")
	}
	if ent.penaltyAttempt.persistenceActive {
		t.Fatalf("persistence wrongly active after prepare failure")
	}
	if !reflect.DeepEqual(ent.penaltyAttempt.capture, r.prepares[0]) {
		t.Fatalf("stored capture != exact capture passed to Prepare")
	}
	if ent.penaltyAttempt.capture.Token.Epoch != 1 {
		t.Fatalf("capture epoch = %d, want 1", ent.penaltyAttempt.capture.Token.Epoch)
	}
	live, _ := pendingOf(t, e, id)
	requirePendingEqual(t, live, PendingDeathRuntime{EffectiveCost: 90, DeathTimeSeconds: 100, CorpseID: int64ptr(500)}, "prepare failure preserves pending")
	// An ordinary second LeaveHold MUST NOT reroll: it rejects
	// with the attempt already active and consumes zero RNG.
	rerollRNG := d100Rolls(1, 2, 3, 4, 5)
	if _, err := e.PlayerOrchestrateDeathPenalties(id, penaltyInput(), rerollRNG, &fakePenaltyProvider{}); !errors.Is(err, ErrDeathPenaltyPersistenceActive) {
		t.Fatalf("second LeaveHold = %v, want ErrDeathPenaltyPersistenceActive", err)
	}
	if rerollRNG.at != 0 {
		t.Fatalf("second LeaveHold rng draws = %d, want 0", rerollRNG.at)
	}
	if ent2 := penaltyEnt(t, e, id); ent2.penaltyEpoch != 1 || ent2.penaltyAttempt == nil {
		t.Fatalf("second LeaveHold disturbed the retained attempt")
	}
	// Retry reuses the exact stored capture with zero new RNG.
	storedBefore := ent.penaltyAttempt.capture
	fresh := &fakePenaltyProvider{}
	retryRNG := rng.script
	tok, err := e.PlayerRetryDeathPenaltyPersistence(id, fresh)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if tok != storedBefore.Token {
		t.Fatalf("retry token = %+v, want %+v", tok, storedBefore.Token)
	}
	if retryRNG.at != 5 {
		t.Fatalf("rng draws after retry = %d, want still 5 (zero new RNG)", retryRNG.at)
	}
	if fr := fresh.last(); !reflect.DeepEqual(fr.prepares[0], storedBefore) {
		t.Fatalf("retry prepared a different capture")
	}
	if ent3 := penaltyEnt(t, e, id); ent3.penaltyEpoch != 1 || !ent3.penaltyAttempt.persistenceActive {
		t.Fatalf("retry did not keep epoch 1 with active persistence")
	}
}

// TestDeathPenaltyActivateFailureLocks proves a definitive
// pre-publication Activate error keeps the player locked
// with the exact frozen capture: no unlock, no reroll.
func TestDeathPenaltyActivateFailureLocks(t *testing.T) {
	spells, skills := penaltyAbilities()
	e := newPlayerEngine(t, nil)
	id := penaltyAlivePlayer(t, e, testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1},
		pendingFixture(90, 100, int64ptr(500), false), penaltyDurable(0x800, spells, skills))
	preVitals := penaltyEnt(t, e, id).vitals
	p := &fakePenaltyProvider{activateErr: errPenaltyActivateBoom}
	rng := d100Rolls(90, 26, 89, 30, 50)
	if _, err := e.PlayerOrchestrateDeathPenalties(id, penaltyInput(), rng, p); !errors.Is(err, errPenaltyActivateBoom) {
		t.Fatalf("err = %v, want activate boom", err)
	}
	ent := penaltyEnt(t, e, id)
	if st, _ := lifeOf(t, e, id); st != PlayerLifeDeathPenaltyPersisting {
		t.Fatalf("life = %d, want PenaltyPersisting", uint8(st))
	}
	if ent.penaltyAttempt == nil || ent.penaltyAttempt.persistenceActive {
		t.Fatalf("attempt missing or wrongly active after activate failure")
	}
	if ent.penaltyEpoch != 1 {
		t.Fatalf("epoch = %d, want consumed 1", ent.penaltyEpoch)
	}
	stored := ent.penaltyAttempt.capture
	prepared := p.last().prepares[0]
	if !reflect.DeepEqual(stored, prepared) {
		t.Fatalf("stored capture != prepared capture")
	}
	if stored.Token.Epoch != 1 || stored.PendingBefore.EffectiveCost != 90 || stored.Plan.ScaledCost != 90 {
		t.Fatalf("capture = %+v, want epoch 1 cost 90/90", stored)
	}
	live, _ := pendingOf(t, e, id)
	requirePendingEqual(t, live, PendingDeathRuntime{EffectiveCost: 90, DeathTimeSeconds: 100, CorpseID: int64ptr(500)}, "activate failure preserves pending")
	if ent.vitals != preVitals {
		t.Fatalf("live vitals mutated on activate failure")
	}
}

// TestPenaltyPersistenceExactRetry proves retry reuses
// the exact frozen capture with the same token/plan,
// unchanged epoch, and zero additional RNG.
func TestPenaltyPersistenceExactRetry(t *testing.T) {
	spells, skills := penaltyAbilities()
	e := newPlayerEngine(t, nil)
	id := penaltyAlivePlayer(t, e, testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1},
		pendingFixture(90, 100, int64ptr(500), false), penaltyDurable(0x800, spells, skills))
	p := &fakePenaltyProvider{activateErr: errPenaltyActivateBoom}
	rng := d100Rolls(90, 26, 89, 30, 50)
	if _, err := e.PlayerOrchestrateDeathPenalties(id, penaltyInput(), rng, p); !errors.Is(err, errPenaltyActivateBoom) {
		t.Fatalf("begin: %v", err)
	}
	if rng.at != 5 {
		t.Fatalf("begin rng draws = %d, want 5", rng.at)
	}
	storedBefore := penaltyEnt(t, e, id).penaltyAttempt.capture
	fresh := &fakePenaltyProvider{}
	tok, err := e.PlayerRetryDeathPenaltyPersistence(id, fresh)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if tok != (DeathPenaltyAttemptToken{EntityID: id, CharacterID: testCharacterID(), Epoch: 1}) {
		t.Fatalf("retry token = %+v, want epoch 1", tok)
	}
	if rng.at != 5 {
		t.Fatalf("rng draws after retry = %d, want still 5", rng.at)
	}
	ent := penaltyEnt(t, e, id)
	if ent.penaltyEpoch != 1 {
		t.Fatalf("epoch = %d, want still 1", ent.penaltyEpoch)
	}
	if !reflect.DeepEqual(ent.penaltyAttempt.capture, storedBefore) {
		t.Fatalf("capture changed across retry")
	}
	if !reflect.DeepEqual(ent.penaltyAttempt.capture.Plan, storedBefore.Plan) {
		t.Fatalf("plan changed across retry")
	}
	r := fresh.last()
	if len(r.prepares) != 1 || r.activates != 1 || r.cancels != 0 {
		t.Fatalf("retry reservation = %d/%d/%d, want 1/1/0", len(r.prepares), r.activates, r.cancels)
	}
	if !reflect.DeepEqual(r.prepares[0], storedBefore) {
		t.Fatalf("retry prepared a different capture")
	}
	if !ent.penaltyAttempt.persistenceActive {
		t.Fatalf("retry did not activate persistence")
	}
	// Retry while active is a stable error with zero mutation.
	if _, err := e.PlayerRetryDeathPenaltyPersistence(id, &fakePenaltyProvider{}); !errors.Is(err, ErrDeathPenaltyPersistenceActive) {
		t.Fatalf("retry-while-active = %v, want ErrDeathPenaltyPersistenceActive", err)
	}
}

// TestDeathPenaltyRetryProviderFailure proves a retry
// Reserve failure keeps the player locked with the same
// capture and inactive persistence.
func TestDeathPenaltyRetryProviderFailure(t *testing.T) {
	spells, skills := penaltyAbilities()
	e := newPlayerEngine(t, nil)
	id := penaltyAlivePlayer(t, e, testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1},
		pendingFixture(90, 100, int64ptr(500), false), penaltyDurable(0x800, spells, skills))
	p := &fakePenaltyProvider{activateErr: errPenaltyActivateBoom}
	if _, err := e.PlayerOrchestrateDeathPenalties(id, penaltyInput(), d100Rolls(90, 26, 89, 30, 50), p); !errors.Is(err, errPenaltyActivateBoom) {
		t.Fatalf("begin: %v", err)
	}
	storedBefore := penaltyEnt(t, e, id).penaltyAttempt.capture
	bad := &fakePenaltyProvider{reserveErr: errPenaltyReserveBoom}
	if _, err := e.PlayerRetryDeathPenaltyPersistence(id, bad); !errors.Is(err, errPenaltyReserveBoom) {
		t.Fatalf("retry = %v, want reserve boom", err)
	}
	ent := penaltyEnt(t, e, id)
	if st, _ := lifeOf(t, e, id); st != PlayerLifeDeathPenaltyPersisting {
		t.Fatalf("life = %d, want PenaltyPersisting", uint8(st))
	}
	if !reflect.DeepEqual(ent.penaltyAttempt.capture, storedBefore) {
		t.Fatalf("capture changed on retry reserve failure")
	}
	if ent.penaltyAttempt.persistenceActive {
		t.Fatalf("persistence wrongly active after retry failure")
	}
	// Retry Prepare failure likewise keeps the lock with a Cancel.
	prepBad := &fakePenaltyProvider{prepareErr: errPenaltyPrepareBoom}
	if _, err := e.PlayerRetryDeathPenaltyPersistence(id, prepBad); !errors.Is(err, errPenaltyPrepareBoom) {
		t.Fatalf("retry prepare = %v, want prepare boom", err)
	}
	if prepBad.last().cancels != 1 {
		t.Fatalf("retry prepare failure cancels = %d, want 1", prepBad.last().cancels)
	}
	if ent := penaltyEnt(t, e, id); ent.penaltyEpoch != 1 || ent.penaltyAttempt == nil || ent.penaltyAttempt.persistenceActive {
		t.Fatalf("lock disturbed on retry prepare failure")
	}
}

// TestPenaltyPersistenceRetryable proves the first
// exact pre-Store notification Applies (flipping only the
// active flag) and the repeat Duplicates with zero
// mutation.
func TestPenaltyPersistenceRetryable(t *testing.T) {
	spells, skills := penaltyAbilities()
	e := newPlayerEngine(t, nil)
	id := penaltyAlivePlayer(t, e, testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1},
		pendingFixture(90, 100, int64ptr(500), false), penaltyDurable(0x800, spells, skills))
	p := &fakePenaltyProvider{}
	res := beginPenalty(t, e, id, penaltyInput(), d100Rolls(90, 26, 89, 30, 50), p)
	pre := penaltyEnt(t, e, id).penaltyAttempt.capture
	disp, err := e.PlayerMarkDeathPenaltyPersistenceRetryable(res.Token)
	if err != nil || disp != DeathPenaltyRetryApplied {
		t.Fatalf("mark = %d,%v; want Applied,nil", disp, err)
	}
	ent := penaltyEnt(t, e, id)
	if ent.penaltyAttempt.persistenceActive {
		t.Fatalf("still active after retryable mark")
	}
	if st, _ := lifeOf(t, e, id); st != PlayerLifeDeathPenaltyPersisting {
		t.Fatalf("life = %d, want PenaltyPersisting", uint8(st))
	}
	if !reflect.DeepEqual(ent.penaltyAttempt.capture, pre) {
		t.Fatalf("capture mutated by retryable mark")
	}
	if ent.penaltyEpoch != 1 {
		t.Fatalf("epoch mutated by retryable mark")
	}
	live, _ := pendingOf(t, e, id)
	requirePendingEqual(t, live, PendingDeathRuntime{EffectiveCost: 90, DeathTimeSeconds: 100, CorpseID: int64ptr(500)}, "mark preserves pending")
	disp, err = e.PlayerMarkDeathPenaltyPersistenceRetryable(res.Token)
	if err != nil || disp != DeathPenaltyRetryDuplicate {
		t.Fatalf("repeat mark = %d,%v; want Duplicate,nil", disp, err)
	}
	if ent2 := penaltyEnt(t, e, id); !reflect.DeepEqual(ent2.penaltyAttempt.capture, pre) || ent2.penaltyEpoch != 1 {
		t.Fatalf("duplicate mark mutated")
	}
	// Wrong epoch and post-success notifications mismatch.
	badEpoch := res.Token
	badEpoch.Epoch = 999
	if _, err := e.PlayerMarkDeathPenaltyPersistenceRetryable(badEpoch); !errors.Is(err, ErrDeathPenaltyAttemptMismatch) {
		t.Fatalf("bad epoch mark = %v, want mismatch", err)
	}
}

// TestDeathPenaltyRuntimeQuiesce proves the locked player
// rejects gameplay mutation and Steps perform no movement
// and no health/mana/rest runtime mutation while
// preserving slots, vitals, pending, and durable.
func TestDeathPenaltyRuntimeQuiesce(t *testing.T) {
	spells, skills := penaltyAbilities()
	e := newPlayerEngine(t, nil)
	id := penaltyAlivePlayer(t, e, testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1},
		pendingFixture(90, 100, int64ptr(500), false), penaltyDurable(0x800, spells, skills))
	if _, err := e.SubmitMove(id, MoveIntent{InputSeq: 1, HeldDirs: MoveDirForward, SampleTick: e.CurrentTick()}); err != nil {
		t.Fatalf("pre-lock SubmitMove: %v", err)
	}
	if err := e.PlayerStartResting(id); err != nil {
		t.Fatalf("PlayerStartResting: %v", err)
	}
	p := &fakePenaltyProvider{}
	beginPenalty(t, e, id, penaltyInput(), d100Rolls(90, 26, 89, 30, 50), p)
	if _, err := e.SubmitMove(id, MoveIntent{InputSeq: 2, HeldDirs: MoveDirForward, SampleTick: e.CurrentTick()}); !errors.Is(err, ErrPlayerNotAlive) {
		t.Fatalf("locked SubmitMove = %v, want ErrPlayerNotAlive", err)
	}
	if _, _, err := e.PlayerGainHealthNormal(id, 1); !errors.Is(err, ErrPlayerNotAlive) {
		t.Fatalf("locked vitals mutation = %v, want ErrPlayerNotAlive", err)
	}
	if err := e.SetPosition(id, world.Vec3{X: 9, Y: 0, Z: 9}); !errors.Is(err, ErrPlayerNotAlive) {
		t.Fatalf("locked SetPosition = %v, want ErrPlayerNotAlive", err)
	}
	if _, err := e.PlayerOrchestratePortalOfLife(id, portalInput(100, 50, 500), &fakePortalReservation{}); !errors.Is(err, ErrPlayerNotAlive) {
		t.Fatalf("locked portal begin = %v, want ErrPlayerNotAlive", err)
	}
	if err := e.PlayerInstallRecoveredPendingDeath(id, pendingFixture(5, 200, nil, false)); !errors.Is(err, ErrPlayerNotAlive) {
		t.Fatalf("locked hydration = %v, want ErrPlayerNotAlive", err)
	}
	ent := penaltyEnt(t, e, id)
	prePos := ent.position
	preSeq := ent.lastProcessedSeq
	preSlots := []any{ent.healthArmed, ent.healthDue, ent.manaArmed, ent.manaDue, ent.restArmed, ent.restDue, ent.actedSinceEntry, ent.stomachAnchorTick, ent.runtimeInputs}
	preVitals := ent.vitals
	preDurable := freezePlayerDurableState(*ent.durable)
	if !ent.restArmed {
		t.Fatalf("rest not armed before quiesce steps")
	}
	preHistory, err := e.History(id)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		e.Step()
	}
	ent = penaltyEnt(t, e, id)
	if ent.position != prePos {
		t.Fatalf("position moved under quiesce: %+v", ent.position)
	}
	if ent.lastProcessedSeq != preSeq {
		t.Fatalf("input consumed under quiesce")
	}
	if !ent.hasPending {
		t.Fatalf("pending movement input not preserved under quiesce")
	}
	if post := []any{ent.healthArmed, ent.healthDue, ent.manaArmed, ent.manaDue, ent.restArmed, ent.restDue, ent.actedSinceEntry, ent.stomachAnchorTick, ent.runtimeInputs}; !reflect.DeepEqual(post, preSlots) {
		t.Fatalf("runtime slots mutated under quiesce")
	}
	if ent.vitals != preVitals {
		t.Fatalf("vitals mutated under quiesce")
	}
	if !reflect.DeepEqual(*ent.durable, preDurable) {
		t.Fatalf("durable mutated under quiesce")
	}
	live, _ := pendingOf(t, e, id)
	requirePendingEqual(t, live, PendingDeathRuntime{EffectiveCost: 90, DeathTimeSeconds: 100, CorpseID: int64ptr(500)}, "quiesce preserves pending")
	postHistory, err := e.History(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(postHistory) != len(preHistory)+20 {
		t.Fatalf("history len = %d, want %d (stationary sampling continues)", len(postHistory), len(preHistory)+20)
	}
}

// TestPenaltyPersistenceCompletion proves the first valid
// token installs exactly the stored capture with full
// preservation and no observer replay. The stored plan
// lowers MaxHP by one while HP stays put, so the frozen
// v0.3.57 `reconcileHealth` arms the previously absent
// health slot; mana/rest slots stay bit-identical.
func TestPenaltyPersistenceCompletion(t *testing.T) {
	spells, skills := penaltyAbilities()
	rec := &vitalsRecorder{}
	e := newPlayerEngine(t, rec)
	id := penaltyHighPlayer(t, e, testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1}, 30,
		pendingFixture(90, 100, int64ptr(500), false), penaltyDurable(0x800, spells, skills))
	ent := penaltyEnt(t, e, id)
	ent.deathEpoch = 3
	ent.portalEpoch = 2
	ent.lastDeathSeconds = 42
	p := &fakePenaltyProvider{}
	res := beginPenalty(t, e, id, penaltyInput(), d100Rolls(90, 26, 89, 30, 50), p)
	wantVitals := p.last().prepares[0].Vitals
	wantDurable := p.last().prepares[0].Durable
	if wantVitals.HP != 30 || wantVitals.MaxHP != 29 {
		t.Fatalf("fixture plan vitals = %+v, want HP 30 MaxHP 29 (MaxHP lowered, HP put)", wantVitals)
	}
	prePos := ent.position
	preInputs := ent.runtimeInputs
	preManaRest := []any{ent.manaArmed, ent.manaDue, ent.restArmed, ent.restDue, ent.actedSinceEntry, ent.stomachAnchorTick}
	preHistory, err := e.History(id)
	if err != nil {
		t.Fatal(err)
	}
	disp, err := e.PlayerAcceptDeathPenaltyCompletion(DeathPenaltyCompletion{Token: res.Token})
	if err != nil || disp != DeathPenaltyCompletionApplied {
		t.Fatalf("completion = %d,%v; want Applied,nil", disp, err)
	}
	ent = penaltyEnt(t, e, id)
	if ent.vitals != wantVitals {
		t.Fatalf("vitals = %+v, want %+v", ent.vitals, wantVitals)
	}
	if !reflect.DeepEqual(*ent.durable, wantDurable) {
		t.Fatalf("durable mismatch after completion")
	}
	if ent.pendingDeath != nil {
		t.Fatalf("pending not cleared after completion")
	}
	if st, _ := lifeOf(t, e, id); st != PlayerLifeAlive {
		t.Fatalf("life = %d, want Alive", uint8(st))
	}
	if ent.penaltyAttempt != nil {
		t.Fatalf("private attempt not cleared after completion")
	}
	if ent.position != prePos || ent.runtimeInputs != preInputs {
		t.Fatalf("position/runtime inputs not preserved")
	}
	// Health reconciled per NewHealth: HP != MaxHP with HP > 0
	// arms the previously absent slot from the current tick.
	if !ent.healthArmed {
		t.Fatalf("healthArmed = false, want armed after MaxHP loss with HP > MaxHP")
	}
	if post := []any{ent.manaArmed, ent.manaDue, ent.restArmed, ent.restDue, ent.actedSinceEntry, ent.stomachAnchorTick}; !reflect.DeepEqual(post, preManaRest) {
		t.Fatalf("mana/rest slots not preserved")
	}
	if ent.deathEpoch != 3 || ent.penaltyEpoch != 1 || ent.portalEpoch != 2 || ent.lastDeathSeconds != 42 {
		t.Fatalf("epochs/lastDeath not preserved: death=%d penalty=%d portal=%d last=%d",
			ent.deathEpoch, ent.penaltyEpoch, ent.portalEpoch, ent.lastDeathSeconds)
	}
	postHistory, err := e.History(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(postHistory) != len(preHistory) {
		t.Fatalf("history changed across completion")
	}
	if rec.count() != 0 {
		t.Fatalf("observer events = %d, want 0", rec.count())
	}
}

// TestDeathPenaltyCompletionHealthAbsentArmed proves the frozen
// v0.3.57 completion health semantics for the arm edge: before the
// penalty HP == MaxHP with the health slot absent, the HP penalty
// lowers MaxHP by one while HP stays put, and the first successful
// completion arms the health slot per the existing `reconcileHealth`
// rule. No observer event fires; mana/rest slots stay bit-identical.
func TestDeathPenaltyCompletionHealthAbsentArmed(t *testing.T) {
	spells, skills := penaltyAbilities()
	rec := &vitalsRecorder{}
	e := newPlayerEngine(t, rec)
	id := penaltyHighPlayer(t, e, testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1}, 30,
		pendingFixture(90, 100, int64ptr(500), false), penaltyDurable(0x800, spells, skills))
	ent := penaltyEnt(t, e, id)
	if ent.healthArmed {
		t.Fatalf("healthArmed = true before penalty, want absent with HP == MaxHP")
	}
	if ent.vitals.HP != ent.vitals.MaxHP {
		t.Fatalf("HP = %d MaxHP = %d, want equal before penalty", ent.vitals.HP, ent.vitals.MaxHP)
	}
	p := &fakePenaltyProvider{}
	res := beginPenalty(t, e, id, penaltyInput(), d100Rolls(90, 26, 89, 30, 50), p)
	stored := p.last().prepares[0].Vitals
	if stored.HP != 30 || stored.MaxHP != 29 {
		t.Fatalf("stored vitals = %+v, want HP 30 MaxHP 29 (MaxHP lowered, HP put)", stored)
	}
	preManaRest := []any{ent.manaArmed, ent.manaDue, ent.restArmed, ent.restDue, ent.actedSinceEntry, ent.stomachAnchorTick}
	rec.events = nil
	disp, err := e.PlayerAcceptDeathPenaltyCompletion(DeathPenaltyCompletion{Token: res.Token})
	if err != nil || disp != DeathPenaltyCompletionApplied {
		t.Fatalf("completion = %d,%v; want Applied,nil", disp, err)
	}
	ent = penaltyEnt(t, e, id)
	if ent.vitals.HP != 30 || ent.vitals.MaxHP != 29 {
		t.Fatalf("live vitals = %+v, want exact stored HP 30 MaxHP 29", ent.vitals)
	}
	if !ent.healthArmed {
		t.Fatalf("healthArmed = false, want armed (HP > new MaxHP per reconcileHealth)")
	}
	if post := []any{ent.manaArmed, ent.manaDue, ent.restArmed, ent.restDue, ent.actedSinceEntry, ent.stomachAnchorTick}; !reflect.DeepEqual(post, preManaRest) {
		t.Fatalf("mana/rest slots changed across completion")
	}
	if rec.count() != 0 {
		t.Fatalf("observer events = %d, want 0", rec.count())
	}
}

// TestDeathPenaltyCompletionHealthArmedCancelled proves the cancel
// edge: before the penalty HP == MaxHP - 1 with the health slot
// armed, the MaxHP penalty makes HP == new MaxHP, and completion
// cancels the slot. Mana/rest slots stay bit-identical.
func TestDeathPenaltyCompletionHealthArmedCancelled(t *testing.T) {
	spells, skills := penaltyAbilities()
	rec := &vitalsRecorder{}
	e := newPlayerEngine(t, rec)
	id := penaltyHighPlayer(t, e, testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1}, 30,
		pendingFixture(90, 100, int64ptr(500), false), penaltyDurable(0x800, spells, skills))
	if _, _, err := e.PlayerLoseHealth(id, 1, false); err != nil {
		t.Fatalf("PlayerLoseHealth: %v", err)
	}
	ent := penaltyEnt(t, e, id)
	if ent.vitals.HP != 29 || ent.vitals.MaxHP != 30 {
		t.Fatalf("setup vitals = %+v, want HP 29 MaxHP 30", ent.vitals)
	}
	if !ent.healthArmed {
		t.Fatalf("healthArmed = false after damage, want armed")
	}
	p := &fakePenaltyProvider{}
	res := beginPenalty(t, e, id, penaltyInput(), d100Rolls(90, 26, 89, 30, 50), p)
	stored := p.last().prepares[0].Vitals
	if stored.HP != 29 || stored.MaxHP != 29 {
		t.Fatalf("stored vitals = %+v, want HP 29 MaxHP 29", stored)
	}
	preManaRest := []any{ent.manaArmed, ent.manaDue, ent.restArmed, ent.restDue, ent.actedSinceEntry, ent.stomachAnchorTick}
	rec.events = nil
	disp, err := e.PlayerAcceptDeathPenaltyCompletion(DeathPenaltyCompletion{Token: res.Token})
	if err != nil || disp != DeathPenaltyCompletionApplied {
		t.Fatalf("completion = %d,%v; want Applied,nil", disp, err)
	}
	ent = penaltyEnt(t, e, id)
	if ent.healthArmed {
		t.Fatalf("healthArmed = true, want cancelled (HP == new MaxHP)")
	}
	if post := []any{ent.manaArmed, ent.manaDue, ent.restArmed, ent.restDue, ent.actedSinceEntry, ent.stomachAnchorTick}; !reflect.DeepEqual(post, preManaRest) {
		t.Fatalf("mana/rest slots changed across completion")
	}
	if rec.count() != 0 {
		t.Fatalf("observer events = %d, want 0", rec.count())
	}
}

// TestDeathPenaltyCompletionHealthArmedKeptDue proves the keep edge:
// an armed health timer whose post-penalty HP still differs from the
// new MaxHP keeps its EXACT existing due across completion.
func TestDeathPenaltyCompletionHealthArmedKeptDue(t *testing.T) {
	spells, skills := penaltyAbilities()
	rec := &vitalsRecorder{}
	e := newPlayerEngine(t, rec)
	id := penaltyHighPlayer(t, e, testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1}, 30,
		pendingFixture(90, 100, int64ptr(500), false), penaltyDurable(0x800, spells, skills))
	if _, _, err := e.PlayerLoseHealth(id, 2, false); err != nil {
		t.Fatalf("PlayerLoseHealth: %v", err)
	}
	ent := penaltyEnt(t, e, id)
	if ent.vitals.HP != 28 || ent.vitals.MaxHP != 30 {
		t.Fatalf("setup vitals = %+v, want HP 28 MaxHP 30", ent.vitals)
	}
	if !ent.healthArmed {
		t.Fatalf("healthArmed = false after damage, want armed")
	}
	preDue := ent.healthDue
	p := &fakePenaltyProvider{}
	res := beginPenalty(t, e, id, penaltyInput(), d100Rolls(90, 26, 89, 30, 50), p)
	stored := p.last().prepares[0].Vitals
	if stored.HP != 28 || stored.MaxHP != 29 {
		t.Fatalf("stored vitals = %+v, want HP 28 MaxHP 29", stored)
	}
	rec.events = nil
	disp, err := e.PlayerAcceptDeathPenaltyCompletion(DeathPenaltyCompletion{Token: res.Token})
	if err != nil || disp != DeathPenaltyCompletionApplied {
		t.Fatalf("completion = %d,%v; want Applied,nil", disp, err)
	}
	ent = penaltyEnt(t, e, id)
	if !ent.healthArmed {
		t.Fatalf("healthArmed = false, want still armed (HP != new MaxHP)")
	}
	if ent.healthDue != preDue {
		t.Fatalf("healthDue = %d, want exact pre-penalty due %d", ent.healthDue, preDue)
	}
	if rec.count() != 0 {
		t.Fatalf("observer events = %d, want 0", rec.count())
	}
}

// TestDeathPenaltyCompletionHealthNoMaxHPChange proves a plan that
// does not change MaxHP never spuriously restarts the health
// deadline: an absent slot stays absent, an armed slot keeps its
// exact due. Frenzy carries no HP/ability penalty at all.
func TestDeathPenaltyCompletionHealthNoMaxHPChange(t *testing.T) {
	spells, skills := penaltyAbilities()
	frenzy := UnderworldExitResolvedInput{DefaultDeathCost: 100, FrenzyActive: true}
	t.Run("absent stays absent", func(t *testing.T) {
		rec := &vitalsRecorder{}
		e := newPlayerEngine(t, rec)
		id := penaltyHighPlayer(t, e, testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1}, 30,
			pendingFixture(90, 100, int64ptr(500), false), penaltyDurable(0x800, spells, skills))
		ent := penaltyEnt(t, e, id)
		if ent.healthArmed {
			t.Fatalf("healthArmed = true before penalty, want absent")
		}
		preVitals := ent.vitals
		p := &fakePenaltyProvider{}
		res := beginPenalty(t, e, id, frenzy, d100Rolls(1), p)
		if got := p.last().prepares[0].Vitals; got != preVitals {
			t.Fatalf("frenzy stored vitals = %+v, want unchanged %+v", got, preVitals)
		}
		rec.events = nil
		if _, err := e.PlayerAcceptDeathPenaltyCompletion(DeathPenaltyCompletion{Token: res.Token}); err != nil {
			t.Fatalf("completion: %v", err)
		}
		if ent2 := penaltyEnt(t, e, id); ent2.healthArmed {
			t.Fatalf("healthArmed = true after no-MaxHP-change completion, want absent")
		}
		if rec.count() != 0 {
			t.Fatalf("observer events = %d, want 0", rec.count())
		}
	})
	t.Run("armed keeps exact due", func(t *testing.T) {
		rec := &vitalsRecorder{}
		e := newPlayerEngine(t, rec)
		id := penaltyHighPlayer(t, e, testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1}, 30,
			pendingFixture(90, 100, int64ptr(500), false), penaltyDurable(0x800, spells, skills))
		if _, _, err := e.PlayerLoseHealth(id, 1, false); err != nil {
			t.Fatalf("PlayerLoseHealth: %v", err)
		}
		ent := penaltyEnt(t, e, id)
		if !ent.healthArmed {
			t.Fatalf("healthArmed = false after damage, want armed")
		}
		preDue := ent.healthDue
		p := &fakePenaltyProvider{}
		res := beginPenalty(t, e, id, frenzy, d100Rolls(1), p)
		rec.events = nil
		if _, err := e.PlayerAcceptDeathPenaltyCompletion(DeathPenaltyCompletion{Token: res.Token}); err != nil {
			t.Fatalf("completion: %v", err)
		}
		ent2 := penaltyEnt(t, e, id)
		if !ent2.healthArmed || ent2.healthDue != preDue {
			t.Fatalf("health slot = armed=%v due=%d, want armed=true due=%d", ent2.healthArmed, ent2.healthDue, preDue)
		}
		if rec.count() != 0 {
			t.Fatalf("observer events = %d, want 0", rec.count())
		}
	})
}

// TestDeathPenaltyCompletionDuplicate proves the same token
// after success Duplicates with zero mutation, while a
// NEW pending death makes the old token a mismatch.
func TestDeathPenaltyCompletionDuplicate(t *testing.T) {
	spells, skills := penaltyAbilities()
	e := newPlayerEngine(t, nil)
	id := penaltyAlivePlayer(t, e, testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1},
		pendingFixture(90, 100, int64ptr(500), false), penaltyDurable(0x800, spells, skills))
	p := &fakePenaltyProvider{}
	res := beginPenalty(t, e, id, penaltyInput(), d100Rolls(90, 26, 89, 30, 50), p)
	if _, err := e.PlayerAcceptDeathPenaltyCompletion(DeathPenaltyCompletion{Token: res.Token}); err != nil {
		t.Fatalf("first completion: %v", err)
	}
	preVitals := penaltyEnt(t, e, id).vitals
	preDurable := freezePlayerDurableState(*penaltyEnt(t, e, id).durable)
	disp, err := e.PlayerAcceptDeathPenaltyCompletion(DeathPenaltyCompletion{Token: res.Token})
	if err != nil || disp != DeathPenaltyCompletionDuplicate {
		t.Fatalf("duplicate = %d,%v; want Duplicate,nil", disp, err)
	}
	ent := penaltyEnt(t, e, id)
	if ent.vitals != preVitals || !reflect.DeepEqual(*ent.durable, preDurable) {
		t.Fatalf("duplicate mutated live state")
	}
	if st, _ := lifeOf(t, e, id); st != PlayerLifeAlive {
		t.Fatalf("life = %d after duplicate, want Alive", uint8(st))
	}
	if ent.penaltyEpoch != 1 || ent.penaltyAttempt != nil || ent.pendingDeath != nil {
		t.Fatalf("duplicate disturbed attempt bookkeeping")
	}
	// A NEW pending death supervenes: the old token is now
	// a mismatch, never a Duplicate.
	if err := e.PlayerInstallRecoveredPendingDeath(id, pendingFixture(40, 500, nil, false)); err != nil {
		t.Fatalf("hydration: %v", err)
	}
	if _, err := e.PlayerAcceptDeathPenaltyCompletion(DeathPenaltyCompletion{Token: res.Token}); !errors.Is(err, ErrDeathPenaltyAttemptMismatch) {
		t.Fatalf("old token with new pending = %v, want mismatch", err)
	}
}

// TestDeathPenaltyCompletionMismatch proves wrong entity /
// character / epoch and inactive-persistence completions
// fail closed with zero mutation.
func TestDeathPenaltyCompletionMismatch(t *testing.T) {
	spells, skills := penaltyAbilities()
	setup := func(t *testing.T) (*Engine, EntityID, DeathPenaltyOrchestrationResult) {
		e := newPlayerEngine(t, nil)
		id := penaltyAlivePlayer(t, e, testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1},
			pendingFixture(90, 100, int64ptr(500), false), penaltyDurable(0x800, spells, skills))
		p := &fakePenaltyProvider{}
		res := beginPenalty(t, e, id, penaltyInput(), d100Rolls(90, 26, 89, 30, 50), p)
		return e, id, res
	}
	t.Run("unknown entity", func(t *testing.T) {
		_, _, res := setup(t)
		e2 := newPlayerEngine(t, nil)
		if _, err := e2.PlayerAcceptDeathPenaltyCompletion(DeathPenaltyCompletion{Token: res.Token}); !errors.Is(err, ErrEntityNotFound) {
			t.Fatalf("err = %v, want ErrEntityNotFound", err)
		}
	})
	t.Run("live other entity", func(t *testing.T) {
		e, _, res := setup(t)
		other, err := e.AddPlayerEntity(CharacterID(77), world.Vec3{X: 5, Y: 0, Z: 5}, testVitals(), testRuntimeInputs())
		if err != nil {
			t.Fatal(err)
		}
		bad := res.Token
		bad.EntityID = other.ID
		if _, err := e.PlayerAcceptDeathPenaltyCompletion(DeathPenaltyCompletion{Token: bad}); !errors.Is(err, ErrDeathPenaltyAttemptMismatch) {
			t.Fatalf("err = %v, want mismatch", err)
		}
	})
	t.Run("wrong character", func(t *testing.T) {
		e, _, res := setup(t)
		bad := res.Token
		bad.CharacterID = CharacterID(78)
		if _, err := e.PlayerAcceptDeathPenaltyCompletion(DeathPenaltyCompletion{Token: bad}); !errors.Is(err, ErrDeathPenaltyAttemptMismatch) {
			t.Fatalf("err = %v, want mismatch", err)
		}
	})
	t.Run("wrong epoch", func(t *testing.T) {
		e, _, res := setup(t)
		bad := res.Token
		bad.Epoch = 999
		if _, err := e.PlayerAcceptDeathPenaltyCompletion(DeathPenaltyCompletion{Token: bad}); !errors.Is(err, ErrDeathPenaltyAttemptMismatch) {
			t.Fatalf("err = %v, want mismatch", err)
		}
	})
	t.Run("inactive persistence", func(t *testing.T) {
		e, id, res := setup(t)
		if _, err := e.PlayerMarkDeathPenaltyPersistenceRetryable(res.Token); err != nil {
			t.Fatal(err)
		}
		pre := penaltyEnt(t, e, id).penaltyAttempt.capture
		if _, err := e.PlayerAcceptDeathPenaltyCompletion(DeathPenaltyCompletion{Token: res.Token}); !errors.Is(err, ErrDeathPenaltyAttemptMismatch) {
			t.Fatalf("err = %v, want mismatch while retryable", err)
		}
		if ent := penaltyEnt(t, e, id); !reflect.DeepEqual(ent.penaltyAttempt.capture, pre) {
			t.Fatalf("mismatch mutated the frozen capture")
		}
	})
}

// TestDeathPenaltyABA proves removing a locked player and
// re-adding the same CharacterID orphans the old token
// (no CharacterID fallback) while the new entity starts
// clean.
func TestDeathPenaltyABA(t *testing.T) {
	spells, skills := penaltyAbilities()
	e := newPlayerEngine(t, nil)
	id := penaltyAlivePlayer(t, e, testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1},
		pendingFixture(90, 100, int64ptr(500), false), penaltyDurable(0x800, spells, skills))
	p := &fakePenaltyProvider{}
	res := beginPenalty(t, e, id, penaltyInput(), d100Rolls(90, 26, 89, 30, 50), p)
	if err := e.RemoveEntity(id); err != nil {
		t.Fatalf("RemoveEntity: %v", err)
	}
	snap, err := e.AddPlayerEntityWithDurableState(testCharacterID(), world.Vec3{X: 2, Y: 0, Z: 2}, testVitals(), testRuntimeInputs(), penaltyDurable(0x800, spells, skills))
	if err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if _, err := e.PlayerAcceptDeathPenaltyCompletion(DeathPenaltyCompletion{Token: res.Token}); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("old token = %v, want ErrEntityNotFound", err)
	}
	b := penaltyEnt(t, e, snap.ID)
	if b.penaltyEpoch != 0 || b.penaltyAttempt != nil {
		t.Fatalf("re-added entity carries penalty state")
	}
	if st, isPlayer, err := e.PlayerLifeStateOf(snap.ID); err != nil || !isPlayer || st != PlayerLifeAlive {
		t.Fatalf("re-added life = %d,%v,%v; want Alive,true,nil", uint8(st), isPlayer, err)
	}
}

// TestDeathPenaltyHandoffRemovalFields proves the penalty
// epoch rides a normal same-entity handoff, no handoff can
// start while locked, and removal discards the attempt.
func TestDeathPenaltyHandoffRemovalFields(t *testing.T) {
	spells, skills := penaltyAbilities()
	t.Run("epoch preserved across handoff after success", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		id := penaltyAlivePlayer(t, e, testCharacterID(), world.Vec3{X: 31.9, Y: 0, Z: 16},
			pendingFixture(90, 100, int64ptr(500), false), penaltyDurable(0x0, spells, skills))
		p := &fakePenaltyProvider{}
		res := beginPenalty(t, e, id, penaltyInput(), d100Rolls(26, 29, 26, 29), p)
		if _, err := e.PlayerAcceptDeathPenaltyCompletion(DeathPenaltyCompletion{Token: res.Token}); err != nil {
			t.Fatalf("completion: %v", err)
		}
		submitMove(t, e, id, 1, MoveDirForward, 0, 1024)
		e.Step()
		got, err := e.Entity(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Cell != (world.CellCoord{X: 1, Z: 0}) {
			t.Fatalf("cell = %v, want handoff to {1 0}", got.Cell)
		}
		if ent := penaltyEnt(t, e, id); ent.penaltyEpoch != 1 || ent.penaltyAttempt != nil {
			t.Fatalf("epoch/attempt not preserved across handoff")
		}
	})
	t.Run("no handoff while locked", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		id := penaltyAlivePlayer(t, e, testCharacterID(), world.Vec3{X: 31.9, Y: 0, Z: 16},
			pendingFixture(90, 100, int64ptr(500), false), penaltyDurable(0x0, spells, skills))
		submitMove(t, e, id, 1, MoveDirForward, 0, 1024)
		p := &fakePenaltyProvider{}
		beginPenalty(t, e, id, penaltyInput(), d100Rolls(26, 29, 26, 29), p)
		for i := 0; i < 3; i++ {
			e.Step()
		}
		got, err := e.Entity(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Cell != (world.CellCoord{X: 0, Z: 0}) {
			t.Fatalf("cell = %v, want no handoff while locked", got.Cell)
		}
		if _, migrating := e.registry.migrations[id]; migrating {
			t.Fatalf("migration started while locked")
		}
	})
	t.Run("removal discards attempt", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		id := penaltyAlivePlayer(t, e, testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1},
			pendingFixture(90, 100, int64ptr(500), false), penaltyDurable(0x0, spells, skills))
		p := &fakePenaltyProvider{}
		beginPenalty(t, e, id, penaltyInput(), d100Rolls(26, 29, 26, 29), p)
		if err := e.RemoveEntity(id); err != nil {
			t.Fatalf("RemoveEntity: %v", err)
		}
		snap, err := e.AddPlayerEntityWithDurableState(testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1}, testVitals(), testRuntimeInputs(), penaltyDurable(0x0, spells, skills))
		if err != nil {
			t.Fatalf("re-add: %v", err)
		}
		fresh := penaltyEnt(t, e, snap.ID)
		if fresh.penaltyEpoch != 0 || fresh.penaltyAttempt != nil {
			t.Fatalf("fresh entity carries penalty state")
		}
	})
}

// TestDeathPenaltyCaptureImmutability proves caller
// mutation after submission cannot reach the frozen
// capture (durable bytes, corpse pointer, loss list).
func TestDeathPenaltyCaptureImmutability(t *testing.T) {
	e := newPlayerEngine(t, nil)
	d := penaltyDurable(0x800,
		[]PlayerAbilityState{{ID: 11, Ability: 50}},
		[]PlayerAbilityState{{ID: 21, Ability: 40}})
	d.Advancement = []byte(`{"adv_points":7}`)
	id := penaltyAlivePlayer(t, e, testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1},
		pendingFixture(90, 100, int64ptr(500), false), d)
	p := &fakePenaltyProvider{}
	res := beginPenalty(t, e, id, penaltyInput(), d100Rolls(90, 26, 89, 30, 50), p)
	stored := p.last().prepares[0]
	// Mutate every live alias: durable bytes/abilities,
	// pending corpse pointer, and the returned plan.
	ent := penaltyEnt(t, e, id)
	ent.durable.Spells[0].Ability = 1
	ent.durable.Advancement[2] = 'X'
	*ent.pendingDeath.CorpseID = 9999
	res.Plan.AbilityLosses[0].ToAbility = 1
	if stored.Durable.Spells[0].Ability != 49 {
		t.Fatalf("capture spell = %d, want post-penalty 49 (not live-mutated 1)", stored.Durable.Spells[0].Ability)
	}
	if string(stored.Durable.Advancement) != `{"adv_points":7}` {
		t.Fatalf("capture advancement aliases live state")
	}
	if stored.PendingBefore.CorpseID == nil || *stored.PendingBefore.CorpseID != 500 {
		t.Fatalf("capture corpse aliases live state")
	}
	if stored.Plan.AbilityLosses[0].ToAbility == 1 {
		t.Fatalf("capture plan aliases returned plan")
	}
	if got := CloneDeathPenaltyCapture(stored); !reflect.DeepEqual(got, stored) {
		t.Fatalf("clone != stored")
	}
}

// penaltyRunEngine builds a Run-capable engine with a
// manual clock for the typed-ingress tests.
func penaltyRunEngine(t *testing.T, clk *manualClock) *Engine {
	t.Helper()
	return mustEngine(t, 20, EngineDeps{
		Clock: clk, RNG: newTestRNG(7), Collision: openCollision{}, RunGate: staticGate{allow: true},
	})
}

// penaltyLockedActive builds an owner-local locked+active
// penalty player (setup runs before Run owns the engine).
func penaltyLockedActive(t *testing.T, e *Engine, charID CharacterID) (EntityID, DeathPenaltyOrchestrationResult) {
	t.Helper()
	spells, skills := penaltyAbilities()
	id := penaltyAlivePlayer(t, e, charID, world.Vec3{X: 1, Y: 0, Z: 1},
		pendingFixture(90, 100, int64ptr(500), false), penaltyDurable(0x800, spells, skills))
	p := &fakePenaltyProvider{}
	res := beginPenalty(t, e, id, penaltyInput(), d100Rolls(90, 26, 89, 30, 50), p)
	return id, res
}

// TestDeathPenaltyTypedRetryableIngress proves the
// retryable notification over the real owner mailbox:
// success, Duplicate, pre-cancel, not-running, and full
// mailbox — with no second mailbox.
func TestDeathPenaltyTypedRetryableIngress(t *testing.T) {
	t.Run("applied then duplicate", func(t *testing.T) {
		clk := newManualClock()
		e := penaltyRunEngine(t, clk)
		_, res := penaltyLockedActive(t, e, testCharacterID())
		cancel, done := runOwner(t, e, clk)
		defer stopOwner(t, cancel, done)
		disp, err := e.EnqueueDeathPenaltyPersistenceRetryable(context.Background(), res.Token)
		if err != nil || disp != DeathPenaltyRetryApplied {
			t.Fatalf("enqueue = %d,%v; want Applied,nil", disp, err)
		}
		disp, err = e.EnqueueDeathPenaltyPersistenceRetryable(context.Background(), res.Token)
		if err != nil || disp != DeathPenaltyRetryDuplicate {
			t.Fatalf("reenqueue = %d,%v; want Duplicate,nil", disp, err)
		}
	})
	t.Run("pre-cancel", func(t *testing.T) {
		clk := newManualClock()
		e := penaltyRunEngine(t, clk)
		_, res := penaltyLockedActive(t, e, testCharacterID())
		cancel, done := runOwner(t, e, clk)
		defer stopOwner(t, cancel, done)
		ctx, stop := context.WithCancel(context.Background())
		stop()
		if _, err := e.EnqueueDeathPenaltyPersistenceRetryable(ctx, res.Token); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
	t.Run("not running", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		tok := DeathPenaltyAttemptToken{EntityID: 1, CharacterID: testCharacterID(), Epoch: 1}
		if _, err := e.EnqueueDeathPenaltyPersistenceRetryable(context.Background(), tok); !errors.Is(err, ErrEngineNotRunning) {
			t.Fatalf("err = %v, want ErrEngineNotRunning", err)
		}
	})
}

// TestDeathPenaltyTypedCompletionIngress proves success
// completion over the real owner mailbox: Applied,
// Duplicate, pre-cancel, not-running, and full mailbox.
func TestDeathPenaltyTypedCompletionIngress(t *testing.T) {
	t.Run("applied then duplicate", func(t *testing.T) {
		clk := newManualClock()
		e := penaltyRunEngine(t, clk)
		id, res := penaltyLockedActive(t, e, testCharacterID())
		cancel, done := runOwner(t, e, clk)
		defer stopOwner(t, cancel, done)
		disp, err := e.EnqueueDeathPenaltyCompletion(context.Background(), DeathPenaltyCompletion{Token: res.Token})
		if err != nil || disp != DeathPenaltyCompletionApplied {
			t.Fatalf("enqueue = %d,%v; want Applied,nil", disp, err)
		}
		if st, _, _ := e.PlayerLifeStateOf(id); st != PlayerLifeAlive {
			t.Fatalf("life = %d, want Alive after ingress completion", uint8(st))
		}
		disp, err = e.EnqueueDeathPenaltyCompletion(context.Background(), DeathPenaltyCompletion{Token: res.Token})
		if err != nil || disp != DeathPenaltyCompletionDuplicate {
			t.Fatalf("reenqueue = %d,%v; want Duplicate,nil", disp, err)
		}
	})
	t.Run("pre-cancel", func(t *testing.T) {
		clk := newManualClock()
		e := penaltyRunEngine(t, clk)
		_, res := penaltyLockedActive(t, e, testCharacterID())
		cancel, done := runOwner(t, e, clk)
		defer stopOwner(t, cancel, done)
		ctx, stop := context.WithCancel(context.Background())
		stop()
		if _, err := e.EnqueueDeathPenaltyCompletion(ctx, DeathPenaltyCompletion{Token: res.Token}); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
	t.Run("not running", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		c := DeathPenaltyCompletion{Token: DeathPenaltyAttemptToken{EntityID: 1, CharacterID: testCharacterID(), Epoch: 1}}
		if _, err := e.EnqueueDeathPenaltyCompletion(context.Background(), c); !errors.Is(err, ErrEngineNotRunning) {
			t.Fatalf("err = %v, want ErrEngineNotRunning", err)
		}
	})
}

// TestDeathPenaltyIngressFullMailbox proves both penalty
// commands report ErrSimIngressFull on the same bounded
// 256-command mailbox with zero mutation.
func TestDeathPenaltyIngressFullMailbox(t *testing.T) {
	col := newBlockingCollision()
	clk := newManualClock()
	e := mustEngine(t, 20, EngineDeps{
		Clock: clk, RNG: newTestRNG(7), Collision: col, RunGate: staticGate{allow: true},
	})
	pid, res := penaltyLockedActive(t, e, testCharacterID())
	cancel, done := runOwner(t, e, clk)

	blockPos := world.Vec3{X: 1}
	release := col.block(blockPos)
	first := make(chan ingressAddResult, 1)
	go func() {
		snap, err := e.EnqueueAddEntity(context.Background(), blockPos)
		first <- ingressAddResult{snap: snap, err: err}
	}()
	waitEntered(t, col, blockPos)

	results := make(chan error, SimIngressCapacity)
	for i := 0; i < SimIngressCapacity; i++ {
		go func(i int) {
			_, err := e.EnqueueAddEntity(context.Background(), world.Vec3{X: float64(100 + i)})
			results <- err
		}(i)
	}
	waitMailboxLen(t, e, SimIngressCapacity)
	if _, err := e.EnqueueDeathPenaltyCompletion(context.Background(), DeathPenaltyCompletion{Token: res.Token}); !errors.Is(err, ErrSimIngressFull) {
		t.Fatalf("completion on full mailbox = %v, want ErrSimIngressFull", err)
	}
	if _, err := e.EnqueueDeathPenaltyPersistenceRetryable(context.Background(), res.Token); !errors.Is(err, ErrSimIngressFull) {
		t.Fatalf("retryable on full mailbox = %v, want ErrSimIngressFull", err)
	}
	close(release)
	if res := <-first; res.err != nil {
		t.Fatalf("first add: %v", res.err)
	}
	for i := 0; i < SimIngressCapacity; i++ {
		if err := <-results; err != nil {
			t.Fatalf("queued add #%d: %v", i, err)
		}
	}
	stopOwner(t, cancel, done)
	// The rejected penalty commands mutated nothing: the
	// attempt is still locked+active.
	st, isPlayer, err := e.PlayerLifeStateOf(pid)
	if err != nil || !isPlayer || st != PlayerLifeDeathPenaltyPersisting {
		t.Fatalf("life = %d,%v,%v; want locked PenaltyPersisting", uint8(st), isPlayer, err)
	}
}
