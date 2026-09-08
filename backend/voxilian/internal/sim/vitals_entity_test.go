package sim

import (
	"errors"
	"fmt"
	"testing"

	"github.com/dlukt/voxilian/internal/world"
)

// testVitals is a valid canonical creation value (Myst 25: Mana/Max 20).
func testVitals() PlayerVitals {
	v, err := NewPlayerVitals(25)
	if err != nil {
		panic(err)
	}
	return v
}

// testRuntimeInputs is a valid resolved runtime-input snapshot
// (Stam/Myst 25, no powers, ordinary room multiplier).
func testRuntimeInputs() PlayerVitalsRuntimeInputs {
	in := PlayerVitalsRuntimeInputs{
		EffectiveStamina:       25,
		EffectiveMysticism:     25,
		RestRecoveryMultiplier: 1,
	}
	if err := in.Validate(); err != nil {
		panic(err)
	}
	return in
}

// vitalsRecorder captures dirty/event seam deliveries (spec §9.4b.6).
type vitalsRecorder struct {
	events []PlayerVitalsEvent
}

func (r *vitalsRecorder) OnPlayerVitalsChange(ev PlayerVitalsEvent) {
	r.events = append(r.events, ev)
}

func (r *vitalsRecorder) count() int { return len(r.events) }

func newPlayerEngine(t *testing.T, obs PlayerVitalsObserver) *Engine {
	t.Helper()
	return mustEngine(t, 20, EngineDeps{
		Clock:  newManualClock(),
		RNG:    newTestRNG(1),
		Vitals: obs,
	})
}

func TestPlayerVitalsGenericEntityRemainsGeneric(t *testing.T) {
	e := newPlayerEngine(t, nil)
	snap, err := e.AddEntity(world.Vec3{X: 1, Y: 0, Z: 1})
	if err != nil {
		t.Fatalf("AddEntity: %v", err)
	}
	if snap.IsPlayer {
		t.Fatalf("generic add classified as player")
	}
	got, ok, err := e.PlayerVitalsOf(snap.ID)
	if err != nil || ok || got != (PlayerVitals{}) {
		t.Fatalf("PlayerVitalsOf(generic) = %+v,%v,%v; want zero,false,nil", got, ok, err)
	}
	// Movement still works exactly as before for generic entities.
	if _, err := e.SubmitMove(snap.ID, MoveIntent{InputSeq: 1, HeldDirs: MoveDirForward}); err != nil {
		t.Fatalf("SubmitMove(generic): %v", err)
	}
	e.Step()
	s, err := e.Entity(snap.ID)
	if err != nil {
		t.Fatalf("Entity: %v", err)
	}
	if s.Position.Z >= 1 { // yaw 0 forward is -Z; walk moved 0.175 m
		t.Fatalf("generic entity did not move: %+v", s.Position)
	}
	if s.IsPlayer {
		t.Fatalf("movement classified generic entity as player")
	}
}

func TestPlayerVitalsAttachAcceptsValid(t *testing.T) {
	e := newPlayerEngine(t, nil)
	v := testVitals()
	v.HP = 7 // distinctive damaged state
	snap, err := e.AddPlayerEntity(world.Vec3{X: 3, Y: 0, Z: 4}, v, testRuntimeInputs())
	if err != nil {
		t.Fatalf("AddPlayerEntity: %v", err)
	}
	if !snap.IsPlayer {
		t.Fatalf("player add not classified")
	}
	if got, ok, err := e.PlayerVitalsOf(snap.ID); err != nil || !ok || got != v {
		t.Fatalf("PlayerVitalsOf = %+v,%v,%v; want %+v,true,nil", got, ok, err, v)
	}
	if e.EntityCount() != 1 {
		t.Fatalf("EntityCount = %d, want 1", e.EntityCount())
	}
	if snap.Cell != (world.CellCoord{X: 0, Z: 0}) {
		t.Fatalf("cell = %v", snap.Cell)
	}
}

func TestPlayerVitalsAttachRejectsInvalid(t *testing.T) {
	e := newPlayerEngine(t, nil)
	bad := testVitals()
	bad.Vigor = 0 // outside 1..200
	if _, err := e.AddPlayerEntity(world.Vec3{X: 1, Y: 0, Z: 1}, bad, testRuntimeInputs()); !errors.Is(err, ErrInvalidVitals) {
		t.Fatalf("AddPlayerEntity(invalid) err = %v, want ErrInvalidVitals", err)
	}
	if e.EntityCount() != 0 {
		t.Fatalf("invalid attach mutated registry")
	}
	// The failed attach must not consume an EntityID.
	snap, err := e.AddEntity(world.Vec3{X: 1, Y: 0, Z: 1})
	if err != nil {
		t.Fatalf("AddEntity: %v", err)
	}
	if snap.ID != 1 {
		t.Fatalf("first allocated ID = %d, want 1 (no ID consumed by rejection)", snap.ID)
	}

	good := testVitals()
	if err := e.AttachPlayerVitals(snap.ID, good, testRuntimeInputs()); err != nil {
		t.Fatalf("AttachPlayerVitals: %v", err)
	}
	if err := e.AttachPlayerVitals(snap.ID, good, testRuntimeInputs()); !errors.Is(err, ErrEntityAlreadyPlayer) {
		t.Fatalf("double attach err = %v, want ErrEntityAlreadyPlayer", err)
	}
	bad2 := testVitals()
	bad2.Stomach = 101
	if err := e.AttachPlayerVitals(snap.ID, bad2, testRuntimeInputs()); !errors.Is(err, ErrInvalidVitals) {
		t.Fatalf("AttachPlayerVitals(invalid) err = %v, want ErrInvalidVitals", err)
	}
	if err := e.AttachPlayerVitals(999, good, testRuntimeInputs()); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("AttachPlayerVitals(unknown) err = %v, want ErrEntityNotFound", err)
	}
	// The failed re-attach attempts left the original value untouched.
	if got, ok, _ := e.PlayerVitalsOf(snap.ID); !ok || got != good {
		t.Fatalf("failed attach mutated vitals: %+v", got)
	}
}

func TestPlayerVitalsNoCallerAliasing(t *testing.T) {
	e := newPlayerEngine(t, nil)
	v := testVitals()
	snap, err := e.AddPlayerEntity(world.Vec3{X: 1, Y: 0, Z: 1}, v, testRuntimeInputs())
	if err != nil {
		t.Fatalf("AddPlayerEntity: %v", err)
	}
	v.HP = 999 // caller mutates its copy afterwards
	if got, _, _ := e.PlayerVitalsOf(snap.ID); got.HP != 20 {
		t.Fatalf("caller mutation aliased live state: %+v", got)
	}
	// Inspection copies are independent too.
	got, _, _ := e.PlayerVitalsOf(snap.ID)
	got.Mana = 999
	got2, _, _ := e.PlayerVitalsOf(snap.ID)
	if got2.Mana != 20 {
		t.Fatalf("snapshot mutation aliased live state: %+v", got2)
	}
}

func TestPlayerVitalsZeroFieldsArePlayerVitals(t *testing.T) {
	e := newPlayerEngine(t, nil)
	v := testVitals() // Stomach 0, Exertion 0: legal creation values
	snap, err := e.AddPlayerEntity(world.Vec3{X: 1, Y: 0, Z: 1}, v, testRuntimeInputs())
	if err != nil {
		t.Fatalf("AddPlayerEntity: %v", err)
	}
	got, ok, err := e.PlayerVitalsOf(snap.ID)
	if err != nil || !ok {
		t.Fatalf("PlayerVitalsOf = %v,%v,%v; want player", got, ok, err)
	}
	if got.Stomach != 0 || got.Exertion != 0 {
		t.Fatalf("zero-valued fields lost: %+v", got)
	}
	if !snap.IsPlayer {
		t.Fatalf("IsPlayer false for zero-valued vitals")
	}
}

// TestPlayerVitalsMutationsComposeT4a runs every owner-local mutation
// family against the PRODUCTION T4a helper on the same input value and
// requires identical outputs plus identical committed live state (spec
// §9.4b.5: formulas are never reimplemented).
func TestPlayerVitalsMutationsComposeT4a(t *testing.T) {
	overHP := testVitals()
	overHP.HP = 45 // above 2*Max (40): the source-faithful weird corner
	overMana := testVitals()
	overMana.Mana = 23 // above Max 20
	type c struct {
		name string
		v    PlayerVitals
		// entity-side application
		mutate func(e *Engine, id EntityID) (PlayerVitals, string)
		// production T4a expectation
		direct func(v PlayerVitals) (PlayerVitals, string)
	}
	cases := []c{
		{
			name: "hp loss",
			v:    func() PlayerVitals { x := testVitals(); x.HP = 10; return x }(),
			mutate: func(e *Engine, id EntityID) (PlayerVitals, string) {
				out, res, err := e.PlayerLoseHealth(id, 3, false)
				return out, fmt.Sprint(res, err)
			},
			direct: func(v PlayerVitals) (PlayerVitals, string) {
				out, res, err := LoseHealth(v, 3, false)
				return out, fmt.Sprint(res, err)
			},
		},
		{
			name: "hp loss clamp at zero",
			v:    testVitals(),
			mutate: func(e *Engine, id EntityID) (PlayerVitals, string) {
				out, res, err := e.PlayerLoseHealth(id, 25, false)
				return out, fmt.Sprint(res, err)
			},
			direct: func(v PlayerVitals) (PlayerVitals, string) {
				out, res, err := LoseHealth(v, 25, false)
				return out, fmt.Sprint(res, err)
			},
		},
		{
			name: "normal heal partial",
			v:    func() PlayerVitals { x := testVitals(); x.HP = 18; return x }(),
			mutate: func(e *Engine, id EntityID) (PlayerVitals, string) {
				out, gained, err := e.PlayerGainHealthNormal(id, 5)
				return out, fmt.Sprint(gained, err)
			},
			direct: func(v PlayerVitals) (PlayerVitals, string) {
				out, gained, err := GainHealthNormal(v, 5)
				return out, fmt.Sprint(gained, err)
			},
		},
		{
			name: "overcap heal above 2max corner",
			v:    overHP,
			mutate: func(e *Engine, id EntityID) (PlayerVitals, string) {
				out, delta, err := e.PlayerGainHealthOvercap(id, 10)
				return out, fmt.Sprint(delta, err)
			},
			direct: func(v PlayerVitals) (PlayerVitals, string) {
				out, delta, err := GainHealthOvercap(v, 10)
				return out, fmt.Sprint(delta, err)
			},
		},
		{
			name: "base max adjust",
			v:    testVitals(),
			mutate: func(e *Engine, id EntityID) (PlayerVitals, string) {
				out, delta, err := e.PlayerAdjustBaseMaxHP(id, 1, 25)
				return out, fmt.Sprint(delta, err)
			},
			direct: func(v PlayerVitals) (PlayerVitals, string) {
				out, delta, err := AdjustBaseMaxHP(v, 1, 25)
				return out, fmt.Sprint(delta, err)
			},
		},
		{
			name: "max hp adjust no hp clamp",
			v:    func() PlayerVitals { x := testVitals(); x.HP = 25; return x }(),
			mutate: func(e *Engine, id EntityID) (PlayerVitals, string) {
				out, delta, err := e.PlayerAdjustMaxHP(id, -15)
				return out, fmt.Sprint(delta, err)
			},
			direct: func(v PlayerVitals) (PlayerVitals, string) {
				out, delta, err := AdjustMaxHP(v, -15)
				return out, fmt.Sprint(delta, err)
			},
		},
		{
			name: "mana loss clamp",
			v:    testVitals(),
			mutate: func(e *Engine, id EntityID) (PlayerVitals, string) {
				out, lost, err := e.PlayerLoseMana(id, 25)
				return out, fmt.Sprint(lost, err)
			},
			direct: func(v PlayerVitals) (PlayerVitals, string) {
				out, lost, err := LoseMana(v, 25)
				return out, fmt.Sprint(lost, err)
			},
		},
		{
			name: "capped mana gain above max corner",
			v:    overMana,
			mutate: func(e *Engine, id EntityID) (PlayerVitals, string) {
				out, gained, err := e.PlayerGainMana(id, 5, true)
				return out, fmt.Sprint(gained, err)
			},
			direct: func(v PlayerVitals) (PlayerVitals, string) {
				out, gained, err := GainMana(v, 5, true)
				return out, fmt.Sprint(gained, err)
			},
		},
		{
			name: "uncapped mana gain above max",
			v:    overMana,
			mutate: func(e *Engine, id EntityID) (PlayerVitals, string) {
				out, gained, err := e.PlayerGainMana(id, 5, false)
				return out, fmt.Sprint(gained, err)
			},
			direct: func(v PlayerVitals) (PlayerVitals, string) {
				out, gained, err := GainMana(v, 5, false)
				return out, fmt.Sprint(gained, err)
			},
		},
		{
			name: "max mana adjust",
			v:    testVitals(),
			mutate: func(e *Engine, id EntityID) (PlayerVitals, string) {
				out, delta, err := e.PlayerAdjustMaxMana(id, 6)
				return out, fmt.Sprint(delta, err)
			},
			direct: func(v PlayerVitals) (PlayerVitals, string) {
				out, delta, err := AdjustMaxMana(v, 6)
				return out, fmt.Sprint(delta, err)
			},
		},
		{
			name: "rest exertion sanctuary multiplier",
			v:    func() PlayerVitals { x := testVitals(); x.Vigor = 10; return x }(),
			mutate: func(e *Engine, id EntityID) (PlayerVitals, string) {
				out, err := e.PlayerApplyRestExertion(id, -10000, 2)
				return out, fmt.Sprint(err)
			},
			direct: func(v PlayerVitals) (PlayerVitals, string) {
				out, err := ApplyRestExertion(v, -10000, 2)
				return out, fmt.Sprint(err)
			},
		},
		{
			name: "rest threshold set",
			v:    testVitals(),
			mutate: func(e *Engine, id EntityID) (PlayerVitals, string) {
				out, err := e.PlayerSetRestThreshold(id, 10)
				return out, fmt.Sprint(err)
			},
			direct: func(v PlayerVitals) (PlayerVitals, string) {
				out, err := SetRestThreshold(v, 10)
				return out, fmt.Sprint(err)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newPlayerEngine(t, nil)
			snap, err := e.AddPlayerEntity(world.Vec3{X: 1, Y: 0, Z: 1}, tc.v, testRuntimeInputs())
			if err != nil {
				t.Fatalf("AddPlayerEntity: %v", err)
			}
			gotV, gotRes := tc.mutate(e, snap.ID)
			wantV, wantRes := tc.direct(tc.v)
			if gotRes != wantRes {
				t.Fatalf("result %q != production %q", gotRes, wantRes)
			}
			if gotV != wantV {
				t.Fatalf("value %+v != production %+v", gotV, wantV)
			}
			live, ok, _ := e.PlayerVitalsOf(snap.ID)
			if !ok || live != wantV {
				t.Fatalf("live state %+v != production %+v", live, wantV)
			}
		})
	}
}

// TestPlayerVitalsExertionStrictBoundary pins the strict >20000
// conversion boundary through the entity wrapper (spec §9.4b.20):
// ±20000 converts nothing, ±20001 does, residual arithmetic matches
// the production helper.
func TestPlayerVitalsExertionStrictBoundary(t *testing.T) {
	for _, tc := range []struct {
		amount        int64
		wantVigor     int
		wantExertion  int64
		wantConverted bool
	}{
		{amount: 19999, wantVigor: 100, wantExertion: 19999},
		{amount: 20000, wantVigor: 100, wantExertion: 20000},
		{amount: 20001, wantVigor: 98, wantExertion: 1, wantConverted: true},
	} {
		e := newPlayerEngine(t, nil)
		v := testVitals()
		snap, err := e.AddPlayerEntity(world.Vec3{X: 1, Y: 0, Z: 1}, v, testRuntimeInputs())
		if err != nil {
			t.Fatalf("AddPlayerEntity: %v", err)
		}
		out, err := e.PlayerApplyExertion(snap.ID, tc.amount, false)
		if err != nil {
			t.Fatalf("PlayerApplyExertion(%d): %v", tc.amount, err)
		}
		if out.Vigor != tc.wantVigor || out.Exertion != tc.wantExertion {
			t.Fatalf("apply %d = vigor %d exertion %d; want %d/%d",
				tc.amount, out.Vigor, out.Exertion, tc.wantVigor, tc.wantExertion)
		}
		wantV, err := ApplyExertion(v, tc.amount, false)
		if err != nil || wantV != out {
			t.Fatalf("wrapper divergence from production: %+v vs %+v (%v)", out, wantV, err)
		}
	}
}

func TestPlayerVitalsFailedMutationLeavesEntityUnchanged(t *testing.T) {
	e := newPlayerEngine(t, nil)
	rec := &vitalsRecorder{}
	e.vitalsObs = rec
	v := testVitals()
	snap, err := e.AddPlayerEntity(world.Vec3{X: 1, Y: 0, Z: 1}, v, testRuntimeInputs())
	if err != nil {
		t.Fatalf("AddPlayerEntity: %v", err)
	}
	cases := []struct {
		name string
		call func() error
		want error
	}{
		{
			name: "negative health loss",
			call: func() error { _, _, err := e.PlayerLoseHealth(snap.ID, -1, false); return err },
			want: ErrInvalidHealthAmount,
		},
		{
			name: "negative normal heal",
			call: func() error { _, _, err := e.PlayerGainHealthNormal(snap.ID, -1); return err },
			want: ErrInvalidHealthAmount,
		},
		{
			name: "negative mana gain",
			call: func() error { _, _, err := e.PlayerGainMana(snap.ID, -1, false); return err },
			want: ErrInvalidManaAmount,
		},
		{
			name: "threshold out of domain",
			call: func() error { _, err := e.PlayerSetRestThreshold(snap.ID, 9); return err },
			want: ErrInvalidRestThreshold,
		},
		{
			name: "rest multiplier out of domain",
			call: func() error { _, err := e.PlayerApplyRestExertion(snap.ID, -10000, 4); return err },
			want: ErrInvalidVitals,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if got, _, _ := e.PlayerVitalsOf(snap.ID); got != v {
				t.Fatalf("failed mutation changed entity: %+v", got)
			}
		})
	}
	if rec.count() != 0 {
		t.Fatalf("failed mutations fired %d events", rec.count())
	}
}

func TestPlayerVitalsMutationRejectsNonPlayer(t *testing.T) {
	e := newPlayerEngine(t, nil)
	generic, err := e.AddEntity(world.Vec3{X: 1, Y: 0, Z: 1})
	if err != nil {
		t.Fatalf("AddEntity: %v", err)
	}
	if _, _, err := e.PlayerLoseHealth(generic.ID, 1, false); !errors.Is(err, ErrEntityNotPlayer) {
		t.Fatalf("PlayerLoseHealth(generic) err = %v, want ErrEntityNotPlayer", err)
	}
	if _, err := e.PlayerApplyExertion(generic.ID, 1, false); !errors.Is(err, ErrEntityNotPlayer) {
		t.Fatalf("PlayerApplyExertion(generic) err = %v, want ErrEntityNotPlayer", err)
	}
	if _, err := e.PlayerSetRestThreshold(generic.ID, 80); !errors.Is(err, ErrEntityNotPlayer) {
		t.Fatalf("PlayerSetRestThreshold(generic) err = %v, want ErrEntityNotPlayer", err)
	}
	if _, _, err := e.PlayerLoseHealth(4242, 1, false); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("PlayerLoseHealth(unknown) err = %v, want ErrEntityNotFound", err)
	}
	if _, _, err := e.PlayerVitalsOf(4242); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("PlayerVitalsOf(unknown) err = %v, want ErrEntityNotFound", err)
	}
	if got, ok, _ := e.PlayerVitalsOf(generic.ID); ok || got != (PlayerVitals{}) {
		t.Fatalf("generic entity gained vitals")
	}
}

func TestPlayerVitalsDirtySeam(t *testing.T) {
	t.Run("real change fires exactly one immutable event", func(t *testing.T) {
		rec := &vitalsRecorder{}
		e := newPlayerEngine(t, rec)
		v := testVitals()
		v.HP = 10
		snap, err := e.AddPlayerEntity(world.Vec3{X: 1, Y: 0, Z: 1}, v, testRuntimeInputs())
		if err != nil {
			t.Fatalf("AddPlayerEntity: %v", err)
		}
		out, res, err := e.PlayerLoseHealth(snap.ID, 3, false)
		if err != nil || res.Applied != 3 {
			t.Fatalf("PlayerLoseHealth = %+v,%+v,%v", out, res, err)
		}
		if rec.count() != 1 {
			t.Fatalf("events = %d, want 1", rec.count())
		}
		ev := rec.events[0]
		if ev.EntityID != snap.ID || ev.Before != v || ev.After != out || ev.After.HP != 7 {
			t.Fatalf("event = %+v", ev)
		}
		// The event payload is an immutable copy: mutating it cannot
		// touch live state.
		ev.After.HP = 999
		if live, _, _ := e.PlayerVitalsOf(snap.ID); live.HP != 7 {
			t.Fatalf("event payload aliased live state: %+v", live)
		}
	})
	t.Run("noop fires nothing", func(t *testing.T) {
		rec := &vitalsRecorder{}
		e := newPlayerEngine(t, rec)
		v := testVitals() // HP == Max == 20
		snap, err := e.AddPlayerEntity(world.Vec3{X: 1, Y: 0, Z: 1}, v, testRuntimeInputs())
		if err != nil {
			t.Fatalf("AddPlayerEntity: %v", err)
		}
		if _, gained, err := e.PlayerGainHealthNormal(snap.ID, 5); err != nil || gained != 0 {
			t.Fatalf("normal heal at max = %d,%v", gained, err)
		}
		if _, delta, err := e.PlayerAdjustMaxHP(snap.ID, 0); err != nil || delta != 0 {
			t.Fatalf("max adjust zero = %d,%v", delta, err)
		}
		if got, _, _ := e.PlayerVitalsOf(snap.ID); got != v {
			t.Fatalf("no-op changed value: %+v", got)
		}
		if rec.count() != 0 {
			t.Fatalf("no-op fired %d events", rec.count())
		}
	})
	t.Run("failure fires nothing", func(t *testing.T) {
		rec := &vitalsRecorder{}
		e := newPlayerEngine(t, rec)
		snap, err := e.AddPlayerEntity(world.Vec3{X: 1, Y: 0, Z: 1}, testVitals(), testRuntimeInputs())
		if err != nil {
			t.Fatalf("AddPlayerEntity: %v", err)
		}
		if _, _, err := e.PlayerGainHealthOvercap(snap.ID, -5); !errors.Is(err, ErrInvalidHealthAmount) {
			t.Fatalf("err = %v", err)
		}
		if rec.count() != 0 {
			t.Fatalf("failure fired %d events", rec.count())
		}
	})
	t.Run("nil observer is a no-op", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		snap, err := e.AddPlayerEntity(world.Vec3{X: 1, Y: 0, Z: 1}, testVitals(), testRuntimeInputs())
		if err != nil {
			t.Fatalf("AddPlayerEntity: %v", err)
		}
		if _, _, err := e.PlayerLoseHealth(snap.ID, 1, false); err != nil {
			t.Fatalf("PlayerLoseHealth: %v", err)
		}
	})
}

// submitRunForward submits a run request heading +X and returns the
// post-Step snapshot speed.
func runForwardSpeed(t *testing.T, e *Engine, id EntityID) uint8 {
	t.Helper()
	if _, err := e.SubmitMove(id, MoveIntent{
		InputSeq: 1, HeldDirs: MoveDirForward, RunFlag: 1, Yaw: 1024,
	}); err != nil {
		t.Fatalf("SubmitMove: %v", err)
	}
	e.Step()
	snap, err := e.Entity(id)
	if err != nil {
		t.Fatalf("Entity: %v", err)
	}
	return snap.Speed
}

func TestPlayerVitalsRunGate(t *testing.T) {
	for _, tc := range []struct {
		vigor     int
		wantSpeed uint8
	}{
		{vigor: 9, wantSpeed: WalkSpeedWire},
		{vigor: 10, wantSpeed: RunSpeedWire},
		{vigor: 11, wantSpeed: RunSpeedWire},
		{vigor: 200, wantSpeed: RunSpeedWire},
	} {
		v := testVitals()
		v.Vigor = tc.vigor
		e := newPlayerEngine(t, nil)
		snap, err := e.AddPlayerEntity(world.Vec3{X: 1, Y: 0, Z: 1}, v, testRuntimeInputs())
		if err != nil {
			t.Fatalf("AddPlayerEntity: %v", err)
		}
		if got := runForwardSpeed(t, e, snap.ID); got != tc.wantSpeed {
			t.Fatalf("vigor %d run speed = %d, want %d", tc.vigor, got, tc.wantSpeed)
		}
	}
}

func TestPlayerVitalsRunGateDoesNotConsultInjectedGate(t *testing.T) {
	gate := &countGate{allow: false} // injected gate denies everything
	e := mustEngine(t, 20, EngineDeps{
		Clock: newManualClock(), RNG: newTestRNG(2), RunGate: gate,
	})
	v := testVitals() // vigor 100
	snap, err := e.AddPlayerEntity(world.Vec3{X: 1, Y: 0, Z: 1}, v, testRuntimeInputs())
	if err != nil {
		t.Fatalf("AddPlayerEntity: %v", err)
	}
	if got := runForwardSpeed(t, e, snap.ID); got != RunSpeedWire {
		t.Fatalf("player run denied by injected generic gate: speed %d", got)
	}
	if gate.calls != 0 {
		t.Fatalf("player run consulted injected gate %d times", gate.calls)
	}
}

func TestGenericRunGateStillDelegated(t *testing.T) {
	for _, allow := range []bool{true, false} {
		gate := &countGate{allow: allow}
		e := mustEngine(t, 20, EngineDeps{
			Clock: newManualClock(), RNG: newTestRNG(3), RunGate: gate,
		})
		snap, err := e.AddEntity(world.Vec3{X: 1, Y: 0, Z: 1})
		if err != nil {
			t.Fatalf("AddEntity: %v", err)
		}
		want := uint8(WalkSpeedWire)
		if allow {
			want = RunSpeedWire
		}
		if got := runForwardSpeed(t, e, snap.ID); got != want {
			t.Fatalf("generic run speed = %d, want %d", got, want)
		}
		if gate.calls == 0 {
			t.Fatalf("generic entity did not consult injected gate")
		}
	}
}

func TestPlayerVitalsHandoffPreservation(t *testing.T) {
	e := newPlayerEngine(t, nil)
	v := testVitals()
	v.HP = 7
	v.Mana = 11
	v.Exertion = -4242
	snap, err := e.AddPlayerEntity(world.Vec3{X: 31.9, Y: 0, Z: 0.5}, v, testRuntimeInputs())
	if err != nil {
		t.Fatalf("AddPlayerEntity: %v", err)
	}
	before, ok, err := e.PlayerVitalsOf(snap.ID)
	if err != nil || !ok || before != v {
		t.Fatalf("before = %+v,%v,%v", before, ok, err)
	}
	if got := runForwardSpeed(t, e, snap.ID); got != RunSpeedWire {
		t.Fatalf("player failed to run toward boundary: %d", got)
	}
	afterSnap, err := e.Entity(snap.ID)
	if err != nil {
		t.Fatalf("Entity: %v", err)
	}
	if afterSnap.Cell.X == snap.Cell.X {
		t.Fatalf("entity did not cross a cell boundary: %v", afterSnap.Cell)
	}
	if afterSnap.OwnershipGeneration != snap.OwnershipGeneration+1 {
		t.Fatalf("generation = %d, want exactly one bump", afterSnap.OwnershipGeneration)
	}
	if !afterSnap.IsPlayer {
		t.Fatalf("handoff lost player classification")
	}
	after, ok, err := e.PlayerVitalsOf(snap.ID)
	if err != nil || !ok || after != before {
		t.Fatalf("handoff changed vitals: %+v vs %+v (%v,%v)", after, before, ok, err)
	}
	// Mutations still work on the destination owner.
	if _, _, err := e.PlayerLoseHealth(snap.ID, 1, false); err != nil {
		t.Fatalf("post-handoff mutation: %v", err)
	}
	got, _, _ := e.PlayerVitalsOf(snap.ID)
	if got.HP != 6 {
		t.Fatalf("post-handoff HP = %d, want 6", got.HP)
	}
}

func TestGenericHandoffKeepsClassification(t *testing.T) {
	e := newPlayerEngine(t, nil)
	snap, err := e.AddEntity(world.Vec3{X: 31.9, Y: 0, Z: 0.5})
	if err != nil {
		t.Fatalf("AddEntity: %v", err)
	}
	if got := runForwardSpeed(t, e, snap.ID); got != RunSpeedWire {
		t.Fatalf("generic entity failed to run: %d", got)
	}
	after, err := e.Entity(snap.ID)
	if err != nil {
		t.Fatalf("Entity: %v", err)
	}
	if after.IsPlayer {
		t.Fatalf("generic handoff produced a player")
	}
	if _, ok, err := e.PlayerVitalsOf(snap.ID); err != nil || ok {
		t.Fatalf("generic handoff produced vitals: %v,%v", ok, err)
	}
}
