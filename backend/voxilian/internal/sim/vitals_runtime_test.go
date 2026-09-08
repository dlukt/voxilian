package sim

import (
	"errors"
	"math"
	"testing"

	"github.com/dlukt/voxilian/internal/serial32"
	"github.com/dlukt/voxilian/internal/world"
)

// M5-T4b2 deterministic vitals scheduling tests (spec §9.4b.21 + v0.3.31
// composition freeze). No sleeps, no wall clock: deadlines are driven by
// direct Step() calls; exact due timing uses small in-package deadline
// surgery helpers (force*Due) that place an armed deadline at a chosen
// tick, while arming MATH is always verified against the production T4a
// interval helpers + the ONE production CastTicks conversion.

// entOf resolves the live entity for in-package surgery/inspection.
func entOf(t *testing.T, e *Engine, id EntityID) *entity {
	t.Helper()
	ent, err := e.registry.lookup(id)
	if err != nil {
		t.Fatalf("lookup(%d): %v", uint64(id), err)
	}
	return ent
}

// forceHealthDue places an armed health deadline at due (surgery).
func forceHealthDue(t *testing.T, e *Engine, id EntityID, due uint32) {
	t.Helper()
	ent := entOf(t, e, id)
	ent.healthArmed = true
	ent.healthDue = due
}

func forceManaDue(t *testing.T, e *Engine, id EntityID, due uint32) {
	t.Helper()
	ent := entOf(t, e, id)
	ent.manaArmed = true
	ent.manaDue = due
}

func forceRestDue(t *testing.T, e *Engine, id EntityID, due uint32) {
	t.Helper()
	ent := entOf(t, e, id)
	ent.restArmed = true
	ent.restDue = due
}

// steps drives n direct Steps.
func steps(t *testing.T, e *Engine, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		e.Step()
	}
}

// addDamagedPlayer adds a player with HP 10/20 (health slot armed at
// attach) and full mana 20/20 (mana slot absent).
func addDamagedPlayer(t *testing.T, e *Engine, pos world.Vec3) EntityID {
	t.Helper()
	v := testVitals()
	v.HP = 10
	snap, err := e.AddPlayerEntity(pos, v, testRuntimeInputs())
	if err != nil {
		t.Fatalf("AddPlayerEntity: %v", err)
	}
	return snap.ID
}

// ---------------------------------------------------------------- runtime inputs

func TestRuntimeInputsValidation(t *testing.T) {
	base := testRuntimeInputs()
	if err := base.Validate(); err != nil {
		t.Fatalf("valid inputs rejected: %v", err)
	}
	for _, tc := range []struct {
		name  string
		mut   func(in PlayerVitalsRuntimeInputs) PlayerVitalsRuntimeInputs
		mutOp func(e *Engine, id EntityID) error
		want  error
	}{
		{
			name: "stamina zero",
			mut:  func(in PlayerVitalsRuntimeInputs) PlayerVitalsRuntimeInputs { in.EffectiveStamina = 0; return in },
			want: ErrInvalidCombatStat,
			mutOp: func(e *Engine, id EntityID) error {
				in := base
				in.EffectiveStamina = 0
				return e.PlayerSetVitalsRuntimeInputs(id, in)
			},
		},
		{
			name: "stamina 71",
			mut:  func(in PlayerVitalsRuntimeInputs) PlayerVitalsRuntimeInputs { in.EffectiveStamina = 71; return in },
			want: ErrInvalidCombatStat,
			mutOp: func(e *Engine, id EntityID) error {
				in := base
				in.EffectiveStamina = 71
				return e.PlayerSetVitalsRuntimeInputs(id, in)
			},
		},
		{
			name: "mysticism zero",
			mut:  func(in PlayerVitalsRuntimeInputs) PlayerVitalsRuntimeInputs { in.EffectiveMysticism = 0; return in },
			want: ErrInvalidCombatStat,
			mutOp: func(e *Engine, id EntityID) error {
				in := base
				in.EffectiveMysticism = 0
				return e.PlayerSetVitalsRuntimeInputs(id, in)
			},
		},
		{
			name: "mysticism 71",
			mut:  func(in PlayerVitalsRuntimeInputs) PlayerVitalsRuntimeInputs { in.EffectiveMysticism = 71; return in },
			want: ErrInvalidCombatStat,
			mutOp: func(e *Engine, id EntityID) error {
				in := base
				in.EffectiveMysticism = 71
				return e.PlayerSetVitalsRuntimeInputs(id, in)
			},
		},
		{
			name: "power 100",
			mut:  func(in PlayerVitalsRuntimeInputs) PlayerVitalsRuntimeInputs { in.RestoratePower = 100; return in },
			want: ErrInvalidSpellPower,
			mutOp: func(e *Engine, id EntityID) error {
				in := base
				in.InvigoratePower = 100
				return e.PlayerSetVitalsRuntimeInputs(id, in)
			},
		},
		{
			name: "power negative",
			mut:  func(in PlayerVitalsRuntimeInputs) PlayerVitalsRuntimeInputs { in.RejuvenatePower = -1; return in },
			want: ErrInvalidSpellPower,
			mutOp: func(e *Engine, id EntityID) error {
				in := base
				in.ManaFocusPower = -1
				return e.PlayerSetVitalsRuntimeInputs(id, in)
			},
		},
		{
			name: "rest multiplier 0",
			mut:  func(in PlayerVitalsRuntimeInputs) PlayerVitalsRuntimeInputs { in.RestRecoveryMultiplier = 0; return in },
			want: ErrInvalidVitals,
			mutOp: func(e *Engine, id EntityID) error {
				in := base
				in.RestRecoveryMultiplier = 0
				return e.PlayerSetVitalsRuntimeInputs(id, in)
			},
		},
		{
			name: "rest multiplier 4",
			mut:  func(in PlayerVitalsRuntimeInputs) PlayerVitalsRuntimeInputs { in.RestRecoveryMultiplier = 4; return in },
			want: ErrInvalidVitals,
			mutOp: func(e *Engine, id EntityID) error {
				in := base
				in.RestRecoveryMultiplier = 4
				return e.PlayerSetVitalsRuntimeInputs(id, in)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.mut(base).Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate err = %v, want %v", err, tc.want)
			}
			// Zero mutation through the owner-local update API.
			e := newPlayerEngine(t, nil)
			id := addDamagedPlayer(t, e, world.Vec3{X: 1, Y: 0, Z: 1})
			before, _, err := e.PlayerVitalsRuntimeOf(id)
			if err != nil {
				t.Fatalf("PlayerVitalsRuntimeOf: %v", err)
			}
			if err := tc.mutOp(e, id); !errors.Is(err, tc.want) {
				t.Fatalf("PlayerSetVitalsRuntimeInputs err = %v, want %v", err, tc.want)
			}
			after, _, _ := e.PlayerVitalsRuntimeOf(id)
			if after != before {
				t.Fatalf("rejected update mutated inputs: %+v", after.Inputs)
			}
		})
	}
	// Accepted power domain 0/1/99 and multiplier 1/2/3 through the API.
	for _, p := range []int{0, 1, 99} {
		for _, m := range []int{1, 2, 3} {
			e := newPlayerEngine(t, nil)
			id := addDamagedPlayer(t, e, world.Vec3{X: 1, Y: 0, Z: 1})
			in := testRuntimeInputs()
			in.RestoratePower = p
			in.RejuvenatePower = p
			in.ManaFocusPower = p
			in.InvigoratePower = p
			in.RestRecoveryMultiplier = m
			if err := e.PlayerSetVitalsRuntimeInputs(id, in); err != nil {
				t.Fatalf("power %d mult %d rejected: %v", p, m, err)
			}
			got, _, _ := e.PlayerVitalsRuntimeOf(id)
			if got.Inputs != in {
				t.Fatalf("inputs not stored: %+v", got.Inputs)
			}
		}
	}
}

// ---------------------------------------------------------------- initialization

func TestRuntimeAddInitialization(t *testing.T) {
	// Invalid runtime inputs consume no EntityID.
	e := newPlayerEngine(t, nil)
	bad := testRuntimeInputs()
	bad.RestRecoveryMultiplier = 9
	if _, err := e.AddPlayerEntity(world.Vec3{X: 1, Y: 0, Z: 1}, testVitals(), bad); !errors.Is(err, ErrInvalidVitals) {
		t.Fatalf("AddPlayerEntity(invalid inputs) err = %v", err)
	}
	if e.EntityCount() != 0 {
		t.Fatalf("invalid runtime add mutated registry")
	}
	if snap, err := e.AddEntity(world.Vec3{X: 1, Y: 0, Z: 1}); err != nil || snap.ID != 1 {
		t.Fatalf("first allocated ID = %d,%v; want 1 (no ID consumed)", snap.ID, err)
	}

	// Fresh full 20/20 HP + full Mana: no deadlines, fresh runtime state.
	e2 := newPlayerEngine(t, nil)
	full, err := e2.AddPlayerEntity(world.Vec3{X: 1, Y: 0, Z: 1}, testVitals(), testRuntimeInputs())
	if err != nil {
		t.Fatalf("AddPlayerEntity: %v", err)
	}
	rt, ok, err := e2.PlayerVitalsRuntimeOf(full.ID)
	if err != nil || !ok {
		t.Fatalf("PlayerVitalsRuntimeOf = %v,%v", ok, err)
	}
	if rt.HealthArmed || rt.ManaArmed || rt.RestArmed {
		t.Fatalf("full player armed a deadline: %+v", rt)
	}
	if rt.ActedSinceEntry {
		t.Fatalf("fresh player actedSinceEntry true")
	}
	if rt.StomachAnchorTick != e2.CurrentTick() {
		t.Fatalf("anchor = %d, want current tick %d", rt.StomachAnchorTick, e2.CurrentTick())
	}
	if rt.Inputs != testRuntimeInputs() {
		t.Fatalf("inputs not installed: %+v", rt.Inputs)
	}

	// Damaged HP + below-max mana: deadlines initialized from the
	// production interval helpers + the ONE CastTicks conversion.
	e3 := newPlayerEngine(t, nil)
	v := testVitals()
	v.HP = 10
	v.Mana = 5
	dmg, err := e3.AddPlayerEntity(world.Vec3{X: 1, Y: 0, Z: 1}, v, testRuntimeInputs())
	if err != nil {
		t.Fatalf("AddPlayerEntity: %v", err)
	}
	rt3, _, _ := e3.PlayerVitalsRuntimeOf(dmg.ID)
	hMs, err := HealthRegenIntervalMs(v.Vigor, 25, v.MaxHP, 0, 0)
	if err != nil {
		t.Fatalf("health interval: %v", err)
	}
	hDelay, err := CastTicks(hMs, 20)
	if err != nil {
		t.Fatalf("health cast ticks: %v", err)
	}
	if !rt3.HealthArmed || rt3.HealthDue != e3.CurrentTick()+uint32(hDelay) {
		t.Fatalf("health due = %v,%d; want armed at %d", rt3.HealthArmed, rt3.HealthDue, e3.CurrentTick()+uint32(hDelay))
	}
	mMs, err := ManaRegenIntervalMs(v.Mana, v.MaxMana, v.Vigor, 25, 0, 0, 0)
	if err != nil {
		t.Fatalf("mana interval: %v", err)
	}
	mDelay, err := CastTicks(mMs, 20)
	if err != nil {
		t.Fatalf("mana cast ticks: %v", err)
	}
	if !rt3.ManaArmed || rt3.ManaDue != e3.CurrentTick()+uint32(mDelay) {
		t.Fatalf("mana due = %v,%d; want armed at %d", rt3.ManaArmed, rt3.ManaDue, e3.CurrentTick()+uint32(mDelay))
	}

	// Attach path installs the same atomic initial state.
	e4 := newPlayerEngine(t, nil)
	generic, err := e4.AddEntity(world.Vec3{X: 1, Y: 0, Z: 1})
	if err != nil {
		t.Fatalf("AddEntity: %v", err)
	}
	if err := e4.AttachPlayerVitals(generic.ID, v, testRuntimeInputs()); err != nil {
		t.Fatalf("AttachPlayerVitals: %v", err)
	}
	rt4, ok4, _ := e4.PlayerVitalsRuntimeOf(generic.ID)
	if !ok4 || !rt4.HealthArmed || !rt4.ManaArmed || rt4.HealthDue != rt3.HealthDue || rt4.ManaDue != rt3.ManaDue {
		t.Fatalf("attach runtime init = %+v,%v", rt4, ok4)
	}
	if rt4.StomachAnchorTick != e4.CurrentTick() || rt4.ActedSinceEntry || rt4.RestArmed {
		t.Fatalf("attach runtime flags = %+v", rt4)
	}

	// Inspection classification: a fresh generic entity -> (zero,false,nil);
	// unknown -> ErrEntityNotFound.
	other, err := e4.AddEntity(world.Vec3{X: 2, Y: 0, Z: 2})
	if err != nil {
		t.Fatalf("AddEntity: %v", err)
	}
	if rtg, ok, err := e4.PlayerVitalsRuntimeOf(other.ID); err != nil || ok || rtg != (PlayerVitalsRuntimeSnapshot{}) {
		t.Fatalf("generic runtime inspection = %+v,%v,%v", rtg, ok, err)
	}
	if _, _, err := e4.PlayerVitalsRuntimeOf(9999); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("unknown runtime inspection err = %v", err)
	}
	// Runtime hooks reject generic IDs.
	if err := e4.PlayerStartResting(other.ID); !errors.Is(err, ErrEntityNotPlayer) {
		t.Fatalf("generic start err = %v", err)
	}
}

// ---------------------------------------------------------------- post-commit guard

func TestPostCommitMaxManaGuard(t *testing.T) {
	rec := &vitalsRecorder{}
	e := newPlayerEngine(t, rec)
	id := addDamagedPlayer(t, e, world.Vec3{X: 1, Y: 0, Z: 1})
	// 20 + (-19) -> MaxMana 1: allowed.
	out, delta, err := e.PlayerAdjustMaxMana(id, -19)
	if err != nil || delta != -19 || out.MaxMana != 1 {
		t.Fatalf("adjust -19 = %+v,%d,%v; want MaxMana 1", out, delta, err)
	}
	// 1 + 19 -> back to 20 for the second pin.
	if _, _, err := e.PlayerAdjustMaxMana(id, 19); err != nil {
		t.Fatalf("restore: %v", err)
	}
	events := rec.count()
	vBefore, _, _ := e.PlayerVitalsOf(id)
	rtBefore, _, _ := e.PlayerVitalsRuntimeOf(id)

	// 20 + (-20) -> MaxMana 0: rejected, live state bit-identical.
	if out, delta, err := e.PlayerAdjustMaxMana(id, -20); !errors.Is(err, ErrInvalidVitals) {
		t.Fatalf("adjust -20 = %+v,%d,%v; want ErrInvalidVitals", out, delta, err)
	} else if out.MaxMana != 20 {
		t.Fatalf("returned value not the unchanged live state: %+v", out)
	}
	vAfter, _, _ := e.PlayerVitalsOf(id)
	if vAfter != vBefore {
		t.Fatalf("rejected commit mutated vitals: %+v vs %+v", vAfter, vBefore)
	}
	rtAfter, _, _ := e.PlayerVitalsRuntimeOf(id)
	if rtAfter != rtBefore {
		t.Fatalf("rejected commit mutated runtime metadata")
	}
	if rec.count() != events {
		t.Fatalf("rejected commit fired %d events", rec.count()-events)
	}
	// The T4a pure itself stays unbounded (source-faithful): the same
	// request succeeds on a detached value.
	if pure, delta, err := AdjustMaxMana(vBefore, -20); err != nil || pure.MaxMana != 0 || delta != -20 {
		t.Fatalf("pure AdjustMaxMana(-20) = %+v,%d,%v; want unbounded 0", pure, delta, err)
	}
}

// ---------------------------------------------------------------- conversion

func TestRuntimeDeadlineConversion(t *testing.T) {
	e := newPlayerEngine(t, nil) // 20 Hz
	id := addDamagedPlayer(t, e, world.Vec3{X: 1, Y: 0, Z: 1})
	rt, _, _ := e.PlayerVitalsRuntimeOf(id)

	// Health: vigor 100 / stam 25 / max 20 -> 6665 ms (T4a golden);
	// 6665*20/1000 = 133.3 -> ceil 134 ticks, never floor 133.
	hMs, _ := HealthRegenIntervalMs(100, 25, 20, 0, 0)
	if hMs != 6665 {
		t.Fatalf("health interval = %d, want 6665", hMs)
	}
	hDelay, _ := CastTicks(hMs, 20)
	if hDelay != 134 {
		t.Fatalf("health delay = %d, want 134 (ceil, not 133)", hDelay)
	}
	if rt.HealthDue != 0+uint32(hDelay) {
		t.Fatalf("health due = %d, want %d", rt.HealthDue, hDelay)
	}

	// Rest: stam 25 -> 1000+30*26 = 1780 ms; 1780*20/1000 = 35.6 ->
	// ceil 36 ticks, never floor 35.
	if err := e.PlayerStartResting(id); err != nil {
		t.Fatalf("PlayerStartResting: %v", err)
	}
	rt, _, _ = e.PlayerVitalsRuntimeOf(id)
	rMs, _ := RestIntervalMs(25, 0)
	if rMs != 1780 {
		t.Fatalf("rest interval = %d, want 1780", rMs)
	}
	rDelay, _ := CastTicks(rMs, 20)
	if rDelay != 36 {
		t.Fatalf("rest delay = %d, want 36 (ceil, not 35)", rDelay)
	}
	if rt.RestDue != e.CurrentTick()+36 {
		t.Fatalf("rest due = %d, want %d", rt.RestDue, e.CurrentTick()+36)
	}

	// Mana over-max branch: fixed BOOST decay 30000 ms -> 600 ticks.
	over := testVitals()
	over.Mana = 23
	overSnap, err := e.AddPlayerEntity(world.Vec3{X: 5, Y: 0, Z: 5}, over, testRuntimeInputs())
	if err != nil {
		t.Fatalf("AddPlayerEntity: %v", err)
	}
	rtOver, _, _ := e.PlayerVitalsRuntimeOf(overSnap.ID)
	mDelay, _ := CastTicks(30000, 20)
	if !rtOver.ManaArmed || rtOver.ManaDue != e.CurrentTick()+uint32(mDelay) || mDelay != 600 {
		t.Fatalf("over-max mana due = %v,%d (delay %d)", rtOver.ManaArmed, rtOver.ManaDue, mDelay)
	}

	// Due is future iff serial32.After(due, current): pinned wrap case.
	if !serial32.After(rt.HealthDue, e.CurrentTick()) {
		t.Fatalf("fresh deadline not serially future")
	}
}

func TestRuntimeDueWrapU32(t *testing.T) {
	rec := &vitalsRecorder{}
	e := newPlayerEngine(t, rec)
	e.tick.Store(math.MaxUint32 - 2)
	id := addDamagedPlayer(t, e, world.Vec3{X: 1, Y: 0, Z: 1})
	rt, _, _ := e.PlayerVitalsRuntimeOf(id)
	// Attach-time arming wrapped past MaxUint32; still serially future.
	if !serial32.After(rt.HealthDue, e.CurrentTick()) {
		t.Fatalf("wrapped attach deadline not serially future: due %d current %d", rt.HealthDue, e.CurrentTick())
	}
	if rt.StomachAnchorTick != math.MaxUint32-2 {
		t.Fatalf("anchor = %d, want MaxUint32-2", rt.StomachAnchorTick)
	}
	// Surgery a due 6 ticks ahead (wraps to 3) and fire across the wrap.
	if err := e.PlayerMarkActedSinceEntry(id); err != nil {
		t.Fatalf("mark: %v", err)
	}
	forceHealthDue(t, e, id, e.CurrentTick()+6)
	for i := 0; i < 5; i++ {
		e.Step()
		if got, _, _ := e.PlayerVitalsOf(id); got.HP != 10 {
			t.Fatalf("HP = %d before wrap completion at step %d", got.HP, i)
		}
	}
	e.Step() // tick wraps MaxUint32 -> 0 -> ... -> 3: due fires exactly once
	v, _, _ := e.PlayerVitalsOf(id)
	if v.HP != 11 {
		t.Fatalf("HP = %d, want 11 (fired exactly once across wrap)", v.HP)
	}
	if rec.count() != 1 {
		t.Fatalf("events = %d, want 1", rec.count())
	}
	rt2, _, _ := e.PlayerVitalsRuntimeOf(id)
	if !rt2.HealthArmed || !serial32.After(rt2.HealthDue, e.CurrentTick()) {
		t.Fatalf("re-arm missing after wrap: %+v", rt2)
	}
}

// ---------------------------------------------------------------- NewHealth

func TestNewHealthLifecycle(t *testing.T) {
	t.Run("create cancel keep", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		id := addDamagedPlayer(t, e, world.Vec3{X: 1, Y: 0, Z: 1})
		rt, _, _ := e.PlayerVitalsRuntimeOf(id)
		if !rt.HealthArmed {
			t.Fatalf("damaged player has no health deadline")
		}
		due := rt.HealthDue

		// KEEP: Vigor change (via exertion conversion) restarts nothing.
		if _, err := e.PlayerApplyExertion(id, -30000, false); err != nil {
			t.Fatalf("exertion: %v", err)
		}
		// KEEP: runtime effective-stamina change restarts nothing.
		in := testRuntimeInputs()
		in.EffectiveStamina = 70
		if err := e.PlayerSetVitalsRuntimeInputs(id, in); err != nil {
			t.Fatalf("set inputs: %v", err)
		}
		rt, _, _ = e.PlayerVitalsRuntimeOf(id)
		if !rt.HealthArmed || rt.HealthDue != due {
			t.Fatalf("input change restarted deadline: due %d, kept %d", rt.HealthDue, due)
		}

		// CANCEL: heal to MaxHP.
		if _, _, err := e.PlayerGainHealthNormal(id, 10); err != nil {
			t.Fatalf("heal: %v", err)
		}
		rt, _, _ = e.PlayerVitalsRuntimeOf(id)
		if rt.HealthArmed {
			t.Fatalf("deadline survived heal-to-max")
		}

		// CREATE again: current tick + NEW inputs interval.
		if _, _, err := e.PlayerLoseHealth(id, 5, false); err != nil {
			t.Fatalf("damage: %v", err)
		}
		rt, _, _ = e.PlayerVitalsRuntimeOf(id)
		v, _, _ := e.PlayerVitalsOf(id)
		ms, _ := HealthRegenIntervalMs(v.Vigor, 70, v.MaxHP, 0, 0)
		delay, _ := CastTicks(ms, 20)
		if !rt.HealthArmed || rt.HealthDue != e.CurrentTick()+uint32(delay) {
			t.Fatalf("re-create due = %d, want %d (new inputs)", rt.HealthDue, e.CurrentTick()+uint32(delay))
		}
		if rt.HealthDue == due {
			t.Fatalf("re-create reused the old deadline/interval")
		}
	})

	t.Run("idle damaged player re-arms without healing", func(t *testing.T) {
		rec := &vitalsRecorder{}
		e := newPlayerEngine(t, rec)
		id := addDamagedPlayer(t, e, world.Vec3{X: 1, Y: 0, Z: 1})
		forceHealthDue(t, e, id, e.CurrentTick()+1)
		e.Step()
		v, _, _ := e.PlayerVitalsOf(id)
		if v.HP != 10 {
			t.Fatalf("idle health fired a mutation: HP = %d", v.HP)
		}
		rt, _, _ := e.PlayerVitalsRuntimeOf(id)
		if !rt.HealthArmed || !serial32.After(rt.HealthDue, e.CurrentTick()) {
			t.Fatalf("idle damaged player did not re-arm: %+v", rt)
		}
		if rec.count() != 0 {
			t.Fatalf("idle no-op fired %d events", rec.count())
		}
	})

	t.Run("first action same tick heals", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		id := addDamagedPlayer(t, e, world.Vec3{X: 1, Y: 0, Z: 1})
		// Accepted zero-translation/turn-like pending input.
		if _, err := e.SubmitMove(id, MoveIntent{InputSeq: 1, HeldDirs: 0, Yaw: 500}); err != nil {
			t.Fatalf("SubmitMove: %v", err)
		}
		forceHealthDue(t, e, id, e.CurrentTick()+1)
		e.Step()
		rt, _, _ := e.PlayerVitalsRuntimeOf(id)
		v, _, _ := e.PlayerVitalsOf(id)
		if !rt.ActedSinceEntry {
			t.Fatalf("turn-like input did not mark acted")
		}
		if v.HP != 11 {
			t.Fatalf("same-tick first action + deadline: HP = %d, want 11", v.HP)
		}
	})

	t.Run("acted gate: translating move heals on its tick", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		id := addDamagedPlayer(t, e, world.Vec3{X: 1, Y: 0, Z: 1})
		if _, err := e.SubmitMove(id, MoveIntent{InputSeq: 1, HeldDirs: MoveDirForward, Yaw: 0}); err != nil {
			t.Fatalf("SubmitMove: %v", err)
		}
		forceHealthDue(t, e, id, e.CurrentTick()+1)
		e.Step()
		v, _, _ := e.PlayerVitalsOf(id)
		if v.HP != 11 {
			t.Fatalf("HP = %d, want 11", v.HP)
		}
	})

	t.Run("hp zero never newly arms", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		id := addDamagedPlayer(t, e, world.Vec3{X: 1, Y: 0, Z: 1})
		// Cancel via heal to max, then drop straight to zero.
		if _, _, err := e.PlayerGainHealthNormal(id, 10); err != nil {
			t.Fatalf("heal: %v", err)
		}
		rt, _, _ := e.PlayerVitalsRuntimeOf(id)
		if rt.HealthArmed {
			t.Fatalf("slot not cancelled at max")
		}
		if _, _, err := e.PlayerLoseHealth(id, 20, false); err != nil {
			t.Fatalf("damage to zero: %v", err)
		}
		rt, _, _ = e.PlayerVitalsRuntimeOf(id)
		if rt.HealthArmed {
			t.Fatalf("absent slot newly armed at HP 0")
		}
		steps(t, e, 3)
		if got, _, _ := e.PlayerVitalsOf(id); got.HP != 0 {
			t.Fatalf("zero HP changed: %+v", got)
		}
	})

	t.Run("existing deadline survives transition to zero", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		id := addDamagedPlayer(t, e, world.Vec3{X: 1, Y: 0, Z: 1})
		due := func() uint32 { rt, _, _ := e.PlayerVitalsRuntimeOf(id); return rt.HealthDue }
		frozen := due()
		if _, _, err := e.PlayerLoseHealth(id, 10, false); err != nil {
			t.Fatalf("damage to zero: %v", err)
		}
		rt, _, _ := e.PlayerVitalsRuntimeOf(id)
		if !rt.HealthArmed || rt.HealthDue != frozen {
			t.Fatalf("keep semantics broke at zero: %+v", rt)
		}
	})

	t.Run("over-max decay path", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		v := testVitals()
		v.HP = 25 // > Max 20
		snap, err := e.AddPlayerEntity(world.Vec3{X: 1, Y: 0, Z: 1}, v, testRuntimeInputs())
		if err != nil {
			t.Fatalf("AddPlayerEntity: %v", err)
		}
		if err := e.PlayerMarkActedSinceEntry(snap.ID); err != nil {
			t.Fatalf("mark: %v", err)
		}
		forceHealthDue(t, e, snap.ID, e.CurrentTick()+1)
		e.Step()
		got, _, _ := e.PlayerVitalsOf(snap.ID)
		if got.HP != 24 {
			t.Fatalf("over-max decay HP = %d, want 24", got.HP)
		}
	})
}

// ---------------------------------------------------------------- NewMana

func TestNewManaLifecycle(t *testing.T) {
	t.Run("create cancel keep", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		v := testVitals()
		v.Mana = 19
		snap, err := e.AddPlayerEntity(world.Vec3{X: 1, Y: 0, Z: 1}, v, testRuntimeInputs())
		if err != nil {
			t.Fatalf("AddPlayerEntity: %v", err)
		}
		rt, _, _ := e.PlayerVitalsRuntimeOf(snap.ID)
		if !rt.ManaArmed {
			t.Fatalf("below-max mana has no deadline")
		}
		due := rt.ManaDue

		// KEEP: runtime input change restarts nothing.
		in := testRuntimeInputs()
		in.EffectiveMysticism = 70
		if err := e.PlayerSetVitalsRuntimeInputs(snap.ID, in); err != nil {
			t.Fatalf("set inputs: %v", err)
		}
		rt, _, _ = e.PlayerVitalsRuntimeOf(snap.ID)
		if !rt.ManaArmed || rt.ManaDue != due {
			t.Fatalf("input change restarted mana deadline")
		}

		// CANCEL: capped gain to MaxMana.
		if _, _, err := e.PlayerGainMana(snap.ID, 1, true); err != nil {
			t.Fatalf("gain: %v", err)
		}
		rt, _, _ = e.PlayerVitalsRuntimeOf(snap.ID)
		if rt.ManaArmed {
			t.Fatalf("deadline survived fill-to-max")
		}

		// CREATE: loss re-arms with current tick + current inputs.
		if _, _, err := e.PlayerLoseMana(snap.ID, 4); err != nil {
			t.Fatalf("lose: %v", err)
		}
		rt, _, _ = e.PlayerVitalsRuntimeOf(snap.ID)
		got, _, _ := e.PlayerVitalsOf(snap.ID)
		ms, _ := ManaRegenIntervalMs(got.Mana, got.MaxMana, got.Vigor, 70, 0, 0, 0)
		delay, _ := CastTicks(ms, 20)
		if !rt.ManaArmed || rt.ManaDue != e.CurrentTick()+uint32(delay) {
			t.Fatalf("mana re-create due wrong: %+v", rt)
		}
	})

	t.Run("below max +1 with no acted gate", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		v := testVitals()
		v.Mana = 5
		snap, err := e.AddPlayerEntity(world.Vec3{X: 1, Y: 0, Z: 1}, v, testRuntimeInputs())
		if err != nil {
			t.Fatalf("AddPlayerEntity: %v", err)
		}
		// actedSinceEntry stays false: mana regen is NOT action-gated.
		forceManaDue(t, e, snap.ID, e.CurrentTick()+1)
		e.Step()
		got, _, _ := e.PlayerVitalsOf(snap.ID)
		if got.Mana != 6 {
			t.Fatalf("idle mana = %d, want 6 (ungated)", got.Mana)
		}
		rt, _, _ := e.PlayerVitalsRuntimeOf(snap.ID)
		if !rt.ManaArmed {
			t.Fatalf("mana deadline missing after fire")
		}
	})

	t.Run("over max -1 re-arms via boost decay", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		v := testVitals()
		v.Mana = 23 // > Max 20
		snap, err := e.AddPlayerEntity(world.Vec3{X: 1, Y: 0, Z: 1}, v, testRuntimeInputs())
		if err != nil {
			t.Fatalf("AddPlayerEntity: %v", err)
		}
		forceManaDue(t, e, snap.ID, e.CurrentTick()+1)
		e.Step()
		got, _, _ := e.PlayerVitalsOf(snap.ID)
		if got.Mana != 22 {
			t.Fatalf("over-max decay mana = %d, want 22", got.Mana)
		}
		rt, _, _ := e.PlayerVitalsRuntimeOf(snap.ID)
		delay, _ := CastTicks(30000, 20)
		if !rt.ManaArmed || rt.ManaDue != e.CurrentTick()+uint32(delay) {
			t.Fatalf("boost re-arm wrong: %+v", rt)
		}
	})

	t.Run("at equality no mutation and no re-arm", func(t *testing.T) {
		rec := &vitalsRecorder{}
		e := newPlayerEngine(t, rec)
		id := addDamagedPlayer(t, e, world.Vec3{X: 1, Y: 0, Z: 1})
		// Mana is at MaxMana: arm by surgery (public arming requires
		// inequality), then fire: no mutation, slot stays absent.
		forceManaDue(t, e, id, e.CurrentTick()+1)
		e.Step()
		v, _, _ := e.PlayerVitalsOf(id)
		if v.Mana != v.MaxMana {
			t.Fatalf("equality fire mutated mana: %+v", v)
		}
		rt, _, _ := e.PlayerVitalsRuntimeOf(id)
		if rt.ManaArmed {
			t.Fatalf("equality fire re-armed mana")
		}
		if rec.count() != 0 {
			t.Fatalf("equality fire fired %d events", rec.count())
		}
	})
}

// ---------------------------------------------------------------- phase ordering

func TestPhaseOrderMovementBeforeVitals(t *testing.T) {
	e := newPlayerEngine(t, nil)
	v := testVitals()
	v.Vigor = 9
	snap, err := e.AddPlayerEntity(world.Vec3{X: 1, Y: 0, Z: 1}, v, testRuntimeInputs())
	if err != nil {
		t.Fatalf("AddPlayerEntity: %v", err)
	}
	if err := e.PlayerStartResting(snap.ID); err != nil {
		t.Fatalf("PlayerStartResting: %v", err)
	}
	// Rest event due THIS Step will recover Vigor 9 -> 12 (mult 3,
	// -30000 exertion crossing). A run request is processed the SAME
	// Step, but movement selection happens BEFORE rest recovery.
	in := testRuntimeInputs()
	in.RestRecoveryMultiplier = 3
	if err := e.PlayerSetVitalsRuntimeInputs(snap.ID, in); err != nil {
		t.Fatalf("set inputs: %v", err)
	}
	forceRestDue(t, e, snap.ID, e.CurrentTick()+1)
	if _, err := e.SubmitMove(snap.ID, MoveIntent{
		InputSeq: 1, HeldDirs: MoveDirForward, RunFlag: 1, Yaw: 1024,
	}); err != nil {
		t.Fatalf("SubmitMove: %v", err)
	}
	e.Step()
	s, err := e.Entity(snap.ID)
	if err != nil {
		t.Fatalf("Entity: %v", err)
	}
	if s.Speed != WalkSpeedWire {
		t.Fatalf("same-tick speed = %d, want WALK (movement sees Vigor 9)", s.Speed)
	}
	got, _, _ := e.PlayerVitalsOf(snap.ID)
	if got.Vigor < 10 {
		t.Fatalf("rest did not recover vigor after movement: %d", got.Vigor)
	}
	// A later run-capable Step may RUN (new accepted input).
	if _, err := e.SubmitMove(snap.ID, MoveIntent{
		InputSeq: 2, HeldDirs: MoveDirForward, RunFlag: 1, Yaw: 1024,
	}); err != nil {
		t.Fatalf("SubmitMove: %v", err)
	}
	e.Step()
	s, err = e.Entity(snap.ID)
	if err != nil {
		t.Fatalf("Entity: %v", err)
	}
	if s.Speed != RunSpeedWire {
		t.Fatalf("later run speed = %d, want RUN", s.Speed)
	}
}

// ---------------------------------------------------------------- rest lifecycle

func TestRestLifecycleBasics(t *testing.T) {
	e := newPlayerEngine(t, nil)
	id := addDamagedPlayer(t, e, world.Vec3{X: 1, Y: 0, Z: 1})
	resting, err := e.PlayerIsResting(id)
	if err != nil || resting {
		t.Fatalf("fresh resting = %v,%v", resting, err)
	}
	if err := e.PlayerStartResting(id); err != nil {
		t.Fatalf("start: %v", err)
	}
	rt, _, _ := e.PlayerVitalsRuntimeOf(id)
	if !rt.RestArmed {
		t.Fatalf("start did not arm rest")
	}
	rMs, _ := RestIntervalMs(25, 0)
	delay, _ := CastTicks(rMs, 20)
	if rt.RestDue != e.CurrentTick()+uint32(delay) {
		t.Fatalf("rest due = %d, want %d", rt.RestDue, e.CurrentTick()+uint32(delay))
	}
	// Second start: exact no-op, same due.
	if err := e.PlayerStartResting(id); err != nil {
		t.Fatalf("second start: %v", err)
	}
	rt2, _, _ := e.PlayerVitalsRuntimeOf(id)
	if !rt2.RestArmed || rt2.RestDue != rt.RestDue {
		t.Fatalf("second start was not a no-op: %+v", rt2)
	}
	if ok, _ := e.PlayerIsResting(id); !ok {
		t.Fatalf("IsResting false while armed")
	}
	// Stop cancels; second stop is a no-op.
	if err := e.PlayerStopResting(id); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if ok, _ := e.PlayerIsResting(id); ok {
		t.Fatalf("IsResting true after stop")
	}
	if err := e.PlayerStopResting(id); err != nil {
		t.Fatalf("second stop: %v", err)
	}
	rt3, _, _ := e.PlayerVitalsRuntimeOf(id)
	if rt3.RestArmed {
		t.Fatalf("second stop re-armed")
	}
	// Generic/unknown rejection.
	generic, _ := e.AddEntity(world.Vec3{X: 7, Y: 0, Z: 7})
	if err := e.PlayerStartResting(generic.ID); !errors.Is(err, ErrEntityNotPlayer) {
		t.Fatalf("generic start err = %v", err)
	}
	if _, err := e.PlayerIsResting(4242); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("unknown IsResting err = %v", err)
	}
	if err := e.PlayerStopResting(4242); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("unknown stop err = %v", err)
	}
}

func TestRestRecoveryGating(t *testing.T) {
	// The scheduler gate is strictly Vigor < RestThreshold (spec
	// §9.4b.13); at/above threshold no recovery applies, but resting
	// continues (always re-arm).
	for _, tc := range []struct {
		name      string
		vigor     int
		wantEvent bool
	}{
		{name: "below threshold recovers", vigor: 79, wantEvent: true},
		{name: "at threshold no recovery", vigor: 80},
		{name: "above threshold no recovery", vigor: 81},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &vitalsRecorder{}
			e := newPlayerEngine(t, rec)
			v := testVitals()
			v.Vigor = tc.vigor
			snap, err := e.AddPlayerEntity(world.Vec3{X: 1, Y: 0, Z: 1}, v, testRuntimeInputs())
			if err != nil {
				t.Fatalf("AddPlayerEntity: %v", err)
			}
			if err := e.PlayerStartResting(snap.ID); err != nil {
				t.Fatalf("start: %v", err)
			}
			forceRestDue(t, e, snap.ID, e.CurrentTick()+1)
			e.Step()
			want := v
			if tc.vigor < v.RestThreshold {
				want, err = ApplyRestExertion(v, -10000, 1)
				if err != nil {
					t.Fatalf("production ApplyRestExertion: %v", err)
				}
			}
			got, _, _ := e.PlayerVitalsOf(snap.ID)
			if got != want {
				t.Fatalf("vitals %+v != expected %+v", got, want)
			}
			wantEvents := 0
			if tc.wantEvent {
				wantEvents = 1
			}
			if rec.count() != wantEvents {
				t.Fatalf("events = %d, want %d", rec.count(), wantEvents)
			}
			rt, _, _ := e.PlayerVitalsRuntimeOf(snap.ID)
			if !rt.RestArmed {
				t.Fatalf("threshold stopped resting: %+v", rt)
			}
		})
	}
}

func TestRestInputTiming(t *testing.T) {
	t.Run("multiplier change affects next recovery not deadline", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		v := testVitals()
		v.Vigor = 10
		snap, err := e.AddPlayerEntity(world.Vec3{X: 1, Y: 0, Z: 1}, v, testRuntimeInputs())
		if err != nil {
			t.Fatalf("AddPlayerEntity: %v", err)
		}
		if err := e.PlayerStartResting(snap.ID); err != nil {
			t.Fatalf("start: %v", err)
		}
		rt, _, _ := e.PlayerVitalsRuntimeOf(snap.ID)
		due := rt.RestDue
		in := testRuntimeInputs()
		in.RestRecoveryMultiplier = 3
		if err := e.PlayerSetVitalsRuntimeInputs(snap.ID, in); err != nil {
			t.Fatalf("set inputs: %v", err)
		}
		rt, _, _ = e.PlayerVitalsRuntimeOf(snap.ID)
		if rt.RestDue != due {
			t.Fatalf("multiplier change restarted rest deadline")
		}
		// Run to the real fire: recovery uses the NEW multiplier
		// (-30000 crosses: Vigor 10 -> 13). The old multiplier would
		// have left Vigor at 10 with Exertion -10000.
		for i := 0; i < 40; i++ {
			e.Step()
			rt, _, _ = e.PlayerVitalsRuntimeOf(snap.ID)
			if rt.RestDue != due {
				break
			}
		}
		got, _, _ := e.PlayerVitalsOf(snap.ID)
		if got.Vigor != 13 || got.Exertion != 0 {
			t.Fatalf("recovery used stale multiplier: %+v", got)
		}
	})

	t.Run("stamina change affects next re-arm not deadline", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		id := addDamagedPlayer(t, e, world.Vec3{X: 1, Y: 0, Z: 1})
		if err := e.PlayerStartResting(id); err != nil {
			t.Fatalf("start: %v", err)
		}
		rt, _, _ := e.PlayerVitalsRuntimeOf(id)
		due := rt.RestDue
		in := testRuntimeInputs()
		in.EffectiveStamina = 70 // 430 ms -> 9 ticks at 20 Hz
		if err := e.PlayerSetVitalsRuntimeInputs(id, in); err != nil {
			t.Fatalf("set inputs: %v", err)
		}
		rt, _, _ = e.PlayerVitalsRuntimeOf(id)
		if rt.RestDue != due {
			t.Fatalf("stamina change restarted rest deadline")
		}
		// Fire at the ORIGINAL due (36 ticks with stamina 25): the
		// fire happens during the Step that reaches due, so afterwards
		// the slot is re-armed from the fire tick with the NEW inputs.
		for e.CurrentTick() < due {
			e.Step()
		}
		newMs, err := RestIntervalMs(70, 0)
		if err != nil {
			t.Fatalf("rest interval: %v", err)
		}
		newDelay, _ := CastTicks(newMs, 20)
		if newDelay != 9 {
			t.Fatalf("new rest delay = %d, want 9", newDelay)
		}
		rt, _, _ = e.PlayerVitalsRuntimeOf(id)
		if !rt.RestArmed || rt.RestDue != due+uint32(newDelay) {
			t.Fatalf("re-arm due = %d, want %d (new interval from fire tick)", rt.RestDue, due+uint32(newDelay))
		}
	})
}

// ---------------------------------------------------------------- adjust max HP scheduling

// TestAdjustMaxHPNewHealthReconciliation pins all three NewHealth
// transitions caused specifically by PlayerAdjustMaxHP (spec §9.4b.11,
// §9.4b.22): a MaxHP change can CREATE the slot (equality broken with
// HP > 0), CANCEL it (equality reached), or KEEP the exact existing
// due; the changed MaxHP affects only the NEXT create/re-arm.
func TestAdjustMaxHPNewHealthReconciliation(t *testing.T) {
	t.Run("absent creates after max increase and later heals", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		id := addDamagedPlayer(t, e, world.Vec3{X: 1, Y: 0, Z: 1})
		// Return to full 20/20 so the slot is absent at equality.
		if _, _, err := e.PlayerGainHealthNormal(id, 10); err != nil {
			t.Fatalf("heal to full: %v", err)
		}
		rt, _, _ := e.PlayerVitalsRuntimeOf(id)
		if rt.HealthArmed {
			t.Fatalf("slot armed at equality before adjust")
		}
		out, delta, err := e.PlayerAdjustMaxHP(id, 5)
		if err != nil || delta != 5 || out.HP != 20 || out.MaxHP != 25 {
			t.Fatalf("adjust +5 = %+v,%d,%v", out, delta, err)
		}
		rt, _, _ = e.PlayerVitalsRuntimeOf(id)
		if !rt.HealthArmed {
			t.Fatalf("equality broken did not CREATE the slot")
		}
		v, _, _ := e.PlayerVitalsOf(id)
		ms, err := HealthRegenIntervalMs(v.Vigor, 25, v.MaxHP, 0, 0)
		if err != nil {
			t.Fatalf("health interval: %v", err)
		}
		delay, err := CastTicks(ms, e.TickHz())
		if err != nil {
			t.Fatalf("cast ticks: %v", err)
		}
		if rt.HealthDue != e.CurrentTick()+uint32(delay) {
			t.Fatalf("created due = %d, want %d", rt.HealthDue, e.CurrentTick()+uint32(delay))
		}
		// The resulting timer really heals 20 -> 21 when acted.
		if err := e.PlayerMarkActedSinceEntry(id); err != nil {
			t.Fatalf("mark: %v", err)
		}
		forceHealthDue(t, e, id, e.CurrentTick()+1)
		e.Step()
		got, _, _ := e.PlayerVitalsOf(id)
		if got.HP != 21 {
			t.Fatalf("HP = %d, want 21 (created timer heals)", got.HP)
		}
	})

	t.Run("present cancels when max becomes hp", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		v := testVitals()
		v.HP = 25 // legal over-max: attach arms the health slot
		snap, err := e.AddPlayerEntity(world.Vec3{X: 1, Y: 0, Z: 1}, v, testRuntimeInputs())
		if err != nil {
			t.Fatalf("AddPlayerEntity: %v", err)
		}
		rt, _, _ := e.PlayerVitalsRuntimeOf(snap.ID)
		if !rt.HealthArmed {
			t.Fatalf("over-max player has no deadline at attach")
		}
		out, delta, err := e.PlayerAdjustMaxHP(snap.ID, 5)
		if err != nil || delta != 5 || out.HP != 25 || out.MaxHP != 25 {
			t.Fatalf("adjust +5 = %+v,%d,%v", out, delta, err)
		}
		rt, _, _ = e.PlayerVitalsRuntimeOf(snap.ID)
		if rt.HealthArmed {
			t.Fatalf("equality reached did not CANCEL the slot")
		}
		// No stale deadline may later mutate HP.
		steps(t, e, 5)
		got, _, _ := e.PlayerVitalsOf(snap.ID)
		if got.HP != 25 {
			t.Fatalf("stale deadline mutated HP: %d", got.HP)
		}
		rt, _, _ = e.PlayerVitalsRuntimeOf(snap.ID)
		if rt.HealthArmed {
			t.Fatalf("stale deadline re-armed at equality")
		}
	})

	t.Run("present keeps exact deadline", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		v := testVitals()
		v.HP = 10
		v.MaxHP = 30 // damaged under a raised max: attach arms the slot
		snap, err := e.AddPlayerEntity(world.Vec3{X: 1, Y: 0, Z: 1}, v, testRuntimeInputs())
		if err != nil {
			t.Fatalf("AddPlayerEntity: %v", err)
		}
		rt, _, _ := e.PlayerVitalsRuntimeOf(snap.ID)
		if !rt.HealthArmed {
			t.Fatalf("damaged player has no deadline at attach")
		}
		due := rt.HealthDue
		out, delta, err := e.PlayerAdjustMaxHP(snap.ID, -5) // MaxHP 25, HP 10 != 25
		if err != nil || delta != -5 || out.MaxHP != 25 || out.HP != 10 {
			t.Fatalf("adjust -5 = %+v,%d,%v", out, delta, err)
		}
		rt, _, _ = e.PlayerVitalsRuntimeOf(snap.ID)
		if !rt.HealthArmed || rt.HealthDue != due {
			t.Fatalf("nonzero adjust restarted deadline: armed %v due %d, want true/%d",
				rt.HealthArmed, rt.HealthDue, due)
		}
		// The changed MaxHP affects only the NEXT create/re-arm: fire
		// and verify the re-arm interval uses MaxHP 25.
		if err := e.PlayerMarkActedSinceEntry(snap.ID); err != nil {
			t.Fatalf("mark: %v", err)
		}
		forceHealthDue(t, e, snap.ID, e.CurrentTick()+1)
		e.Step()
		got, _, _ := e.PlayerVitalsOf(snap.ID)
		if got.HP != 11 {
			t.Fatalf("HP = %d, want 11", got.HP)
		}
		rt, _, _ = e.PlayerVitalsRuntimeOf(snap.ID)
		ms, _ := HealthRegenIntervalMs(got.Vigor, 25, 25, 0, 0)
		delay, _ := CastTicks(ms, 20)
		if !rt.HealthArmed || rt.HealthDue != e.CurrentTick()+uint32(delay) {
			t.Fatalf("re-arm did not use the new MaxHP: %+v", rt)
		}
	})

	t.Run("zero adjust never moves a deadline", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		id := addDamagedPlayer(t, e, world.Vec3{X: 1, Y: 0, Z: 1})
		rt, _, _ := e.PlayerVitalsRuntimeOf(id)
		due := rt.HealthDue
		if _, delta, err := e.PlayerAdjustMaxHP(id, 0); err != nil || delta != 0 {
			t.Fatalf("adjust 0 = %d,%v", delta, err)
		}
		rt, _, _ = e.PlayerVitalsRuntimeOf(id)
		if !rt.HealthArmed || rt.HealthDue != due {
			t.Fatalf("no-op adjust moved the deadline: %+v", rt)
		}
		// Absent at equality stays absent.
		full, err := e.AddPlayerEntity(world.Vec3{X: 2, Y: 0, Z: 2}, testVitals(), testRuntimeInputs())
		if err != nil {
			t.Fatalf("AddPlayerEntity: %v", err)
		}
		if _, _, err := e.PlayerAdjustMaxHP(full.ID, 0); err != nil {
			t.Fatalf("adjust 0 at equality: %v", err)
		}
		rtFull, _, _ := e.PlayerVitalsRuntimeOf(full.ID)
		if rtFull.HealthArmed {
			t.Fatalf("no-op adjust armed a slot at equality")
		}
	})

	t.Run("helper failure is atomic", func(t *testing.T) {
		rec := &vitalsRecorder{}
		e := newPlayerEngine(t, rec)
		id := addDamagedPlayer(t, e, world.Vec3{X: 1, Y: 0, Z: 1})
		vBefore, _, _ := e.PlayerVitalsOf(id)
		rtBefore, _, _ := e.PlayerVitalsRuntimeOf(id)
		// MaxHP + MaxInt overflows int64: the T4a helper rejects it
		// before any commit is attempted.
		if _, _, err := e.PlayerAdjustMaxHP(id, math.MaxInt); !errors.Is(err, ErrInvalidHealthAmount) {
			t.Fatalf("hostile adjust err = %v, want ErrInvalidHealthAmount", err)
		}
		vAfter, _, _ := e.PlayerVitalsOf(id)
		if vAfter != vBefore {
			t.Fatalf("failed adjust mutated vitals: %+v vs %+v", vAfter, vBefore)
		}
		rtAfter, _, _ := e.PlayerVitalsRuntimeOf(id)
		if rtAfter != rtBefore {
			t.Fatalf("failed adjust mutated runtime deadline state")
		}
		if rec.count() != 0 {
			t.Fatalf("failed adjust fired %d events", rec.count())
		}
	})
}

// ---------------------------------------------------------------- ordering / at-most-once

func TestSlotOrderHealthManaRest(t *testing.T) {
	rec := &vitalsRecorder{}
	e := newPlayerEngine(t, rec)
	v := testVitals()
	v.HP = 10   // health +1
	v.Mana = 5  // mana +1
	v.Vigor = 1 // rest: mult 3 -> -30000 crossing -> vigor +3
	snap, err := e.AddPlayerEntity(world.Vec3{X: 1, Y: 0, Z: 1}, v, testRuntimeInputs())
	if err != nil {
		t.Fatalf("AddPlayerEntity: %v", err)
	}
	in := testRuntimeInputs()
	in.RestRecoveryMultiplier = 3
	if err := e.PlayerSetVitalsRuntimeInputs(snap.ID, in); err != nil {
		t.Fatalf("set inputs: %v", err)
	}
	if err := e.PlayerMarkActedSinceEntry(snap.ID); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if err := e.PlayerStartResting(snap.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	due := e.CurrentTick() + 1
	forceHealthDue(t, e, snap.ID, due)
	forceManaDue(t, e, snap.ID, due)
	forceRestDue(t, e, snap.ID, due)
	e.Step()

	got, _, _ := e.PlayerVitalsOf(snap.ID)
	if got.HP != 11 || got.Mana != 6 || got.Vigor != 4 {
		t.Fatalf("combined fire result = %+v; want HP 11 Mana 6 Vigor 4", got)
	}
	if rec.count() != 3 {
		t.Fatalf("events = %d, want 3", rec.count())
	}
	// Fixed slot order: health first, mana second, rest third.
	if ev := rec.events[0]; ev.After.HP != 11 || ev.Before.HP != 10 {
		t.Fatalf("event 0 not health: %+v", ev)
	}
	if ev := rec.events[1]; ev.After.Mana != 6 || ev.Before.Mana != 5 {
		t.Fatalf("event 1 not mana: %+v", ev)
	}
	if ev := rec.events[2]; ev.After.Vigor != 4 || ev.Before.Vigor != 1 {
		t.Fatalf("event 2 not rest: %+v", ev)
	}
	// Every slot re-armed exactly once with a future deadline.
	rt, _, _ := e.PlayerVitalsRuntimeOf(snap.ID)
	if !rt.HealthArmed || !rt.ManaArmed || !rt.RestArmed ||
		!serial32.After(rt.HealthDue, e.CurrentTick()) ||
		!serial32.After(rt.ManaDue, e.CurrentTick()) ||
		!serial32.After(rt.RestDue, e.CurrentTick()) {
		t.Fatalf("re-arm state wrong: %+v", rt)
	}
}

func TestOverdueFiresOnce(t *testing.T) {
	rec := &vitalsRecorder{}
	e := newPlayerEngine(t, rec)
	id := addDamagedPlayer(t, e, world.Vec3{X: 1, Y: 0, Z: 1})
	if err := e.PlayerMarkActedSinceEntry(id); err != nil {
		t.Fatalf("mark: %v", err)
	}
	// 100-tick-overdue deadline (as if many Steps were never delivered):
	// a single event fires, not 100 catch-up events.
	forceHealthDue(t, e, id, e.CurrentTick()-100)
	e.Step()
	if rec.count() != 1 {
		t.Fatalf("events = %d, want exactly 1 (no catch-up)", rec.count())
	}
	v, _, _ := e.PlayerVitalsOf(id)
	if v.HP != 11 {
		t.Fatalf("HP = %d, want exactly +1", v.HP)
	}
	// A slot re-armed during processing is not revisited in this Step.
	rt, _, _ := e.PlayerVitalsRuntimeOf(id)
	if !rt.HealthArmed || !serial32.After(rt.HealthDue, e.CurrentTick()) {
		t.Fatalf("single re-arm missing: %+v", rt)
	}
	steps(t, e, 3)
	if v, _, _ := e.PlayerVitalsOf(id); v.HP != 11 {
		t.Fatalf("HP advanced again without a new deadline: %d", v.HP)
	}
}

// ---------------------------------------------------------------- actedSinceEntry

func TestActedSinceEntryLifecycle(t *testing.T) {
	t.Run("mark transitions", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		id := addDamagedPlayer(t, e, world.Vec3{X: 1, Y: 0, Z: 1})
		if rt, _, _ := e.PlayerVitalsRuntimeOf(id); rt.ActedSinceEntry {
			t.Fatalf("fresh player acted true")
		}
		// SubmitMove alone (not yet processed) does not mark.
		if _, err := e.SubmitMove(id, MoveIntent{InputSeq: 1, HeldDirs: MoveDirForward}); err != nil {
			t.Fatalf("SubmitMove: %v", err)
		}
		if rt, _, _ := e.PlayerVitalsRuntimeOf(id); rt.ActedSinceEntry {
			t.Fatalf("submitted-but-unprocessed input marked acted")
		}
		// Processed translating input marks.
		e.Step()
		if rt, _, _ := e.PlayerVitalsRuntimeOf(id); !rt.ActedSinceEntry {
			t.Fatalf("processed translating input did not mark")
		}
		// Entry reset to false: continuing the OLD held control on
		// later ticks does not re-mark (no new pending input).
		if err := e.PlayerApplyEntryActedPolicy(id, true); err != nil {
			t.Fatalf("reset policy: %v", err)
		}
		steps(t, e, 3)
		if rt, _, _ := e.PlayerVitalsRuntimeOf(id); rt.ActedSinceEntry {
			t.Fatalf("continuing old held direction re-marked acted")
		}
		// A new accepted zero-direction/turn-like input marks.
		if _, err := e.SubmitMove(id, MoveIntent{InputSeq: 2, HeldDirs: 0, Yaw: 900}); err != nil {
			t.Fatalf("SubmitMove: %v", err)
		}
		e.Step()
		if rt, _, _ := e.PlayerVitalsRuntimeOf(id); !rt.ActedSinceEntry {
			t.Fatalf("turn-like accepted input did not mark")
		}
	})

	t.Run("mark hook idempotent and policy preserves", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		id := addDamagedPlayer(t, e, world.Vec3{X: 1, Y: 0, Z: 1})
		if err := e.PlayerMarkActedSinceEntry(id); err != nil {
			t.Fatalf("mark: %v", err)
		}
		if err := e.PlayerMarkActedSinceEntry(id); err != nil {
			t.Fatalf("second mark: %v", err)
		}
		if rt, _, _ := e.PlayerVitalsRuntimeOf(id); !rt.ActedSinceEntry {
			t.Fatalf("mark lost")
		}
		// shouldReset=false preserves.
		if err := e.PlayerApplyEntryActedPolicy(id, false); err != nil {
			t.Fatalf("policy false: %v", err)
		}
		if rt, _, _ := e.PlayerVitalsRuntimeOf(id); !rt.ActedSinceEntry {
			t.Fatalf("policy false cleared acted")
		}
		// shouldReset=true clears.
		if err := e.PlayerApplyEntryActedPolicy(id, true); err != nil {
			t.Fatalf("policy true: %v", err)
		}
		if rt, _, _ := e.PlayerVitalsRuntimeOf(id); rt.ActedSinceEntry {
			t.Fatalf("policy true did not clear acted")
		}
		// Generic/unknown rejection.
		generic, _ := e.AddEntity(world.Vec3{X: 7, Y: 0, Z: 7})
		if err := e.PlayerMarkActedSinceEntry(generic.ID); !errors.Is(err, ErrEntityNotPlayer) {
			t.Fatalf("generic mark err = %v", err)
		}
		if err := e.PlayerApplyEntryActedPolicy(4242, true); !errors.Is(err, ErrEntityNotFound) {
			t.Fatalf("unknown policy err = %v", err)
		}
	})

	t.Run("generic entity movement never marks anything player-shaped", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		generic, _ := e.AddEntity(world.Vec3{X: 1, Y: 0, Z: 1})
		if _, err := e.SubmitMove(generic.ID, MoveIntent{InputSeq: 1, HeldDirs: MoveDirForward}); err != nil {
			t.Fatalf("SubmitMove: %v", err)
		}
		e.Step()
		if rt, ok, err := e.PlayerVitalsRuntimeOf(generic.ID); err != nil || ok {
			t.Fatalf("generic gained runtime metadata: %+v,%v", rt, ok)
		}
	})

	t.Run("technical handoff preserves acted=false", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		id := addDamagedPlayer(t, e, world.Vec3{X: 30.0, Y: 0, Z: 0.5})
		// One accepted run input drives the whole approach; the flag
		// is explicitly reset mid-approach, and the later ticks (incl.
		// the boundary crossing on CONTINUED held control) must not
		// re-mark: cell handoff is not room entry, and continuing an
		// old held direction is not a new action.
		if _, err := e.SubmitMove(id, MoveIntent{
			InputSeq: 1, HeldDirs: MoveDirForward, RunFlag: 1, Yaw: 1024,
		}); err != nil {
			t.Fatalf("SubmitMove: %v", err)
		}
		e.Step() // consumes input: acted true
		if rt, _, _ := e.PlayerVitalsRuntimeOf(id); !rt.ActedSinceEntry {
			t.Fatalf("consumed input did not mark acted")
		}
		if err := e.PlayerApplyEntryActedPolicy(id, true); err != nil {
			t.Fatalf("reset policy: %v", err)
		}
		for i := 0; i < 10; i++ {
			e.Step()
			if rt, _, _ := e.PlayerVitalsRuntimeOf(id); rt.ActedSinceEntry {
				t.Fatalf("continued held control re-marked acted at step %d", i)
			}
		}
		s, err := e.Entity(id)
		if err != nil {
			t.Fatalf("Entity: %v", err)
		}
		if s.Cell.X == 0 {
			t.Fatalf("did not cross cell boundary: %+v", s.Position)
		}
		if s.OwnershipGeneration != 2 {
			t.Fatalf("generation = %d, want 2 (one handoff)", s.OwnershipGeneration)
		}
	})
}

// ---------------------------------------------------------------- stomach anchor

func TestStomachAnchorLifecycle(t *testing.T) {
	t.Run("sub-second accumulation at 20 Hz", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		v := testVitals()
		v.Stomach = 50
		snap, err := e.AddPlayerEntity(world.Vec3{X: 1, Y: 0, Z: 1}, v, testRuntimeInputs())
		if err != nil {
			t.Fatalf("AddPlayerEntity: %v", err)
		}
		anchor := e.CurrentTick()
		// 7 ticks (< 1 second): wholeSeconds 0, anchor unchanged.
		steps(t, e, 7)
		if _, err := e.PlayerUpdateStomach(snap.ID); err != nil {
			t.Fatalf("update: %v", err)
		}
		rt, _, _ := e.PlayerVitalsRuntimeOf(snap.ID)
		if rt.StomachAnchorTick != anchor {
			t.Fatalf("anchor = %d, want unchanged %d", rt.StomachAnchorTick, anchor)
		}
		// 6 more ticks then 7 more: total 20 since anchor -> exactly
		// one second consumed, leftover preserved.
		steps(t, e, 6)
		if _, err := e.PlayerUpdateStomach(snap.ID); err != nil {
			t.Fatalf("update: %v", err)
		}
		rt, _, _ = e.PlayerVitalsRuntimeOf(snap.ID)
		if rt.StomachAnchorTick != anchor {
			t.Fatalf("anchor advanced early: %d", rt.StomachAnchorTick)
		}
		steps(t, e, 7)
		out, err := e.PlayerUpdateStomach(snap.ID)
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		rt, _, _ = e.PlayerVitalsRuntimeOf(snap.ID)
		if rt.StomachAnchorTick != anchor+20 {
			t.Fatalf("anchor = %d, want %d (consumed one second of ticks)", rt.StomachAnchorTick, anchor+20)
		}
		want, _ := DecayStomach(50, 1)
		if out.Stomach != want {
			t.Fatalf("stomach = %d, want production %d", out.Stomach, want)
		}
		// Repeated sub-second updates keep preserving the remainder.
		steps(t, e, 5)
		if _, err := e.PlayerUpdateStomach(snap.ID); err != nil {
			t.Fatalf("update: %v", err)
		}
		rt, _, _ = e.PlayerVitalsRuntimeOf(snap.ID)
		if rt.StomachAnchorTick != anchor+20 {
			t.Fatalf("sub-second update discarded remainder: %d", rt.StomachAnchorTick)
		}
	})

	t.Run("stomach zero becomes one on first explicit update", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		id := addDamagedPlayer(t, e, world.Vec3{X: 1, Y: 0, Z: 1}) // Stomach 0
		out, err := e.PlayerUpdateStomach(id)
		if err != nil {
			t.Fatalf("immediate update: %v", err)
		}
		if out.Stomach != 1 {
			t.Fatalf("Stomach 0 + immediate update = %d, want 1", out.Stomach)
		}
		rt, _, _ := e.PlayerVitalsRuntimeOf(id)
		if rt.StomachAnchorTick != e.CurrentTick() {
			t.Fatalf("zero-second update moved anchor")
		}
	})

	t.Run("wrap across MaxUint32", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		e.tick.Store(math.MaxUint32 - 10)
		v := testVitals()
		v.Stomach = 50
		snap, err := e.AddPlayerEntity(world.Vec3{X: 1, Y: 0, Z: 1}, v, testRuntimeInputs())
		if err != nil {
			t.Fatalf("AddPlayerEntity: %v", err)
		}
		anchor := uint32(math.MaxUint32 - 10)
		steps(t, e, 25) // wraps; elapsed 25 ticks -> 1 whole second
		out, err := e.PlayerUpdateStomach(snap.ID)
		if err != nil {
			t.Fatalf("update across wrap: %v", err)
		}
		rt, _, _ := e.PlayerVitalsRuntimeOf(snap.ID)
		if rt.StomachAnchorTick != anchor+20 {
			t.Fatalf("anchor = %d, want wrapped %d", rt.StomachAnchorTick, anchor+20)
		}
		want, _ := DecayStomach(50, 1)
		if out.Stomach != want {
			t.Fatalf("stomach = %d, want %d", out.Stomach, want)
		}
	})

	t.Run("ambiguous elapsed rejected", func(t *testing.T) {
		rec := &vitalsRecorder{}
		e := newPlayerEngine(t, rec)
		id := addDamagedPlayer(t, e, world.Vec3{X: 1, Y: 0, Z: 1})
		current := e.CurrentTick()
		entOf(t, e, id).stomachAnchorTick = current - (1 << 31)
		before, _, _ := e.PlayerVitalsOf(id)
		if _, err := e.PlayerUpdateStomach(id); !errors.Is(err, ErrInvalidRuntimeElapsed) {
			t.Fatalf("ambiguous elapsed err = %v, want ErrInvalidRuntimeElapsed", err)
		}
		after, _, _ := e.PlayerVitalsOf(id)
		if after != before || rec.count() != 0 {
			t.Fatalf("rejected stomach update mutated state")
		}
	})

	t.Run("handoff preserves anchor", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		id := addDamagedPlayer(t, e, world.Vec3{X: 31.9, Y: 0, Z: 0.5})
		anchor := func() uint32 { rt, _, _ := e.PlayerVitalsRuntimeOf(id); return rt.StomachAnchorTick }
		before := anchor()
		if got := runForwardSpeed(t, e, id); got != RunSpeedWire {
			t.Fatalf("player failed to run toward boundary: %d", got)
		}
		s, err := e.Entity(id)
		if err != nil {
			t.Fatalf("Entity: %v", err)
		}
		if s.Cell.X == 0 {
			t.Fatalf("did not cross cell boundary")
		}
		if anchor() != before {
			t.Fatalf("handoff changed stomach anchor")
		}
	})

	t.Run("re-add re-anchors with no offline digestion", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		v := testVitals()
		v.Stomach = 50
		snap, err := e.AddPlayerEntity(world.Vec3{X: 1, Y: 0, Z: 1}, v, testRuntimeInputs())
		if err != nil {
			t.Fatalf("AddPlayerEntity: %v", err)
		}
		steps(t, e, 500) // 25 simulated seconds pass
		if err := e.RemoveEntity(snap.ID); err != nil {
			t.Fatalf("RemoveEntity: %v", err)
		}
		reSnap, err := e.AddPlayerEntity(world.Vec3{X: 1, Y: 0, Z: 1}, v, testRuntimeInputs())
		if err != nil {
			t.Fatalf("re-add: %v", err)
		}
		rt, _, _ := e.PlayerVitalsRuntimeOf(reSnap.ID)
		if rt.StomachAnchorTick != e.CurrentTick() {
			t.Fatalf("re-add anchor = %d, want current %d", rt.StomachAnchorTick, e.CurrentTick())
		}
		out, err := e.PlayerUpdateStomach(reSnap.ID)
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if out.Stomach != 50 {
			t.Fatalf("offline digestion invented: stomach = %d, want 50", out.Stomach)
		}
	})
}

// ---------------------------------------------------------------- handoff / no double fire

func TestRuntimeHandoffNoDoubleFire(t *testing.T) {
	rec := &vitalsRecorder{}
	e := newPlayerEngine(t, rec)
	v := testVitals()
	v.HP = 10
	snap, err := e.AddPlayerEntity(world.Vec3{X: 31.9, Y: 0, Z: 0.5}, v, testRuntimeInputs())
	if err != nil {
		t.Fatalf("AddPlayerEntity: %v", err)
	}
	rt, _, _ := e.PlayerVitalsRuntimeOf(snap.ID)
	anchor := rt.StomachAnchorTick
	if err := e.PlayerMarkActedSinceEntry(snap.ID); err != nil {
		t.Fatalf("mark: %v", err)
	}
	forceHealthDue(t, e, snap.ID, e.CurrentTick()+1)
	if _, err := e.SubmitMove(snap.ID, MoveIntent{
		InputSeq: 1, HeldDirs: MoveDirForward, RunFlag: 1, Yaw: 1024,
	}); err != nil {
		t.Fatalf("SubmitMove: %v", err)
	}
	before, err := e.Entity(snap.ID)
	if err != nil {
		t.Fatalf("Entity: %v", err)
	}
	e.Step()

	after, err := e.Entity(snap.ID)
	if err != nil {
		t.Fatalf("Entity: %v", err)
	}
	if after.Cell.X == before.Cell.X {
		t.Fatalf("entity did not cross a cell boundary: %v", after.Cell)
	}
	if after.OwnershipGeneration != before.OwnershipGeneration+1 {
		t.Fatalf("generation = %d, want exactly one bump", after.OwnershipGeneration)
	}
	got, _, _ := e.PlayerVitalsOf(snap.ID)
	if got.HP != 11 {
		t.Fatalf("HP = %d, want exactly one fire (11)", got.HP)
	}
	if rec.count() != 1 {
		t.Fatalf("health events = %d, want 1 (no duplicate destination firing)", rec.count())
	}
	rt2, _, _ := e.PlayerVitalsRuntimeOf(snap.ID)
	if !rt2.HealthArmed || !serial32.After(rt2.HealthDue, e.CurrentTick()) {
		t.Fatalf("re-arm is not exactly one future deadline: %+v", rt2)
	}
	if rt2.StomachAnchorTick != anchor || !rt2.ActedSinceEntry || rt2.Inputs != testRuntimeInputs() {
		t.Fatalf("handoff lost runtime metadata: %+v", rt2)
	}
	if !after.IsPlayer {
		t.Fatalf("handoff lost player classification")
	}
}

// ---------------------------------------------------------------- dirty observer

func TestRuntimeDirtyObserver(t *testing.T) {
	t.Run("timer-driven changes fire through the same seam", func(t *testing.T) {
		rec := &vitalsRecorder{}
		e := newPlayerEngine(t, rec)
		v := testVitals()
		v.HP = 10
		v.Mana = 5
		v.Vigor = 1
		snap, err := e.AddPlayerEntity(world.Vec3{X: 1, Y: 0, Z: 1}, v, testRuntimeInputs())
		if err != nil {
			t.Fatalf("AddPlayerEntity: %v", err)
		}
		in := testRuntimeInputs()
		in.RestRecoveryMultiplier = 3
		if err := e.PlayerSetVitalsRuntimeInputs(snap.ID, in); err != nil {
			t.Fatalf("set inputs: %v", err)
		}
		if err := e.PlayerMarkActedSinceEntry(snap.ID); err != nil {
			t.Fatalf("mark: %v", err)
		}
		if err := e.PlayerStartResting(snap.ID); err != nil {
			t.Fatalf("start: %v", err)
		}
		// Runtime-only updates so far: no events.
		if rec.count() != 0 {
			t.Fatalf("runtime-only updates fired %d events", rec.count())
		}
		due := e.CurrentTick() + 1
		forceHealthDue(t, e, snap.ID, due)
		forceManaDue(t, e, snap.ID, due)
		forceRestDue(t, e, snap.ID, due)

		e.Step()
		if rec.count() != 3 {
			t.Fatalf("events = %d, want 3 (health, mana, rest)", rec.count())
		}
		for i, ev := range rec.events {
			if ev.EntityID != snap.ID {
				t.Fatalf("event %d wrong entity", i)
			}
			before := ev.Before
			after := ev.After
			before.HP = 999 // event payloads are immutable copies
			if ev.Before.HP == 999 {
				t.Fatalf("event %d Before aliases live/event storage", i)
			}
			_ = after
		}
	})

	t.Run("idle health no-op and stomach no-change fire nothing", func(t *testing.T) {
		rec := &vitalsRecorder{}
		e := newPlayerEngine(t, rec)
		v := testVitals()
		v.HP = 10
		v.Stomach = 50
		snap, err := e.AddPlayerEntity(world.Vec3{X: 1, Y: 0, Z: 1}, v, testRuntimeInputs())
		if err != nil {
			t.Fatalf("AddPlayerEntity: %v", err)
		}
		forceHealthDue(t, e, snap.ID, e.CurrentTick()+1)
		e.Step() // idle: acted=false
		if _, err := e.PlayerUpdateStomach(snap.ID); err != nil {
			t.Fatalf("stomach update: %v", err)
		}
		if rec.count() != 0 {
			t.Fatalf("no-op events fired: %d", rec.count())
		}
	})
}

// ---------------------------------------------------------------- totality

// TestRuntimeIntervalTotality proves the §9.4b.22/§9.4b.14a totality
// contract: validated vitals + validated runtime inputs + TickHz 1..120
// make every interval calculation, CastTicks conversion, timer-step
// mutation, and post-commit validation succeed — so the internal
// impossibleState assertion is unreachable through any public
// owner-local API.
func TestRuntimeIntervalTotality(t *testing.T) {
	vigors := []int{1, 100, 200}
	attrs := []int{1, 25, 70}
	powers := []int{0, 1, 99}
	maxHPs := []int{20, 100, 150}
	maxManas := []int{1, 20, 105}
	hzs := []int{1, 20, 120}
	for _, vigor := range vigors {
		for _, stam := range attrs {
			for _, maxHP := range maxHPs {
				for _, restorate := range powers {
					ms, err := HealthRegenIntervalMs(vigor, stam, maxHP, 0, restorate)
					if err != nil {
						t.Fatalf("health interval(%d,%d,%d,%d): %v", vigor, stam, maxHP, restorate, err)
					}
					for _, hz := range hzs {
						if _, err := CastTicks(ms, hz); err != nil {
							t.Fatalf("CastTicks(%d,%d): %v", ms, hz, err)
						}
					}
				}
			}
		}
	}
	for _, mana := range []int{0, 20, 21, 1000} {
		for _, maxMana := range maxManas {
			for _, vigor := range vigors {
				for _, myst := range attrs {
					for _, rej := range powers {
						for _, focus := range powers {
							ms, err := ManaRegenIntervalMs(mana, maxMana, vigor, myst, 0, rej, focus)
							if err != nil {
								t.Fatalf("mana interval: %v", err)
							}
							for _, hz := range hzs {
								if _, err := CastTicks(ms, hz); err != nil {
									t.Fatalf("CastTicks(%d,%d): %v", ms, hz, err)
								}
							}
						}
					}
				}
			}
		}
	}
	for _, stam := range attrs {
		for _, inv := range powers {
			ms, err := RestIntervalMs(stam, inv)
			if err != nil {
				t.Fatalf("rest interval: %v", err)
			}
			for _, hz := range hzs {
				if _, err := CastTicks(ms, hz); err != nil {
					t.Fatalf("CastTicks(%d,%d): %v", ms, hz, err)
				}
			}
		}
	}
	// Fire-path mutations are total over validated vitals and their
	// results pass the post-commit guard.
	for _, v := range []PlayerVitals{
		(func() PlayerVitals { x := testVitals(); x.HP = 10; return x })(),
		(func() PlayerVitals { x := testVitals(); x.HP = 45; return x })(),
		(func() PlayerVitals { x := testVitals(); x.Mana = 23; x.Vigor = 1; return x })(),
		(func() PlayerVitals { x := testVitals(); x.Vigor = 200; x.Exertion = -20000; return x })(),
	} {
		if err := v.Validate(); err != nil {
			t.Fatalf("fixture invalid: %v", err)
		}
		checks := []struct {
			name string
			run  func() (PlayerVitals, error)
		}{
			{"health gain", func() (PlayerVitals, error) {
				after, _, err := GainHealthNormal(v, 1)
				return after, err
			}},
			{"health decay", func() (PlayerVitals, error) {
				after, _, err := LoseHealth(v, 1, true)
				return after, err
			}},
			{"mana gain", func() (PlayerVitals, error) {
				after, _, err := GainMana(v, 1, false)
				return after, err
			}},
			{"mana decay", func() (PlayerVitals, error) {
				after, _, err := LoseMana(v, 1)
				return after, err
			}},
			{"rest mult 1", func() (PlayerVitals, error) { return ApplyRestExertion(v, -10000, 1) }},
			{"rest mult 3", func() (PlayerVitals, error) { return ApplyRestExertion(v, -10000, 3) }},
		}
		for _, c := range checks {
			after, err := c.run()
			if err != nil {
				t.Fatalf("%s(%+v): %v", c.name, v, err)
			}
			if err := after.Validate(); err != nil {
				t.Fatalf("%s produced invalid post-commit value: %v", c.name, err)
			}
		}
	}
}

// r2v rebuilds a full value with the decayed HP (LoseHealth triple form).
