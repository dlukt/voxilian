package sim

import (
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/dlukt/voxilian/internal/world"
)

// M5-T5c3c3c2 zero-HP resolved T5a orchestration + double-death
// runtime gate tests (spec §9.5.1j). Deterministic, no sleeps,
// no wall clock, no Store/persist/gateway/proto involvement.
// Durable ItemIDs (101/202/303) deliberately differ from the
// opaque capture-order T5a keys (0/1/2).

// fakeDeathReservation is an instrumented
// ImmediateDeathWorkReservation: it records the call sequence
// plus the owner-observable life state at Prepare/Activate
// time, so tests pin the exact Prepare -> begin -> Activate
// order. prepareErr, when non-nil, fails Prepare like a
// validation/mapping failure.
type fakeDeathReservation struct {
	engine *Engine
	entity EntityID

	prepareErr error

	calls          []string
	lifeAtPrepare  []PlayerLifeState
	lifeAtActivate []PlayerLifeState
	prepareCalls   int
	activateCalls  int
	cancelCalls    int

	gotCapture ImmediateDeathCapture
	gotInputs  PlayerVitalsRuntimeInputs
}

func (f *fakeDeathReservation) PrepareImmediateDeathWork(capture ImmediateDeathCapture, in PlayerVitalsRuntimeInputs) error {
	f.prepareCalls++
	life, _, _ := f.engine.PlayerLifeStateOf(f.entity)
	f.lifeAtPrepare = append(f.lifeAtPrepare, life)
	f.calls = append(f.calls, "prepare")
	if f.prepareErr != nil {
		return f.prepareErr
	}
	f.gotCapture = capture
	f.gotInputs = in
	return nil
}

func (f *fakeDeathReservation) ActivateImmediateDeathWork() {
	f.activateCalls++
	life, _, _ := f.engine.PlayerLifeStateOf(f.entity)
	f.lifeAtActivate = append(f.lifeAtActivate, life)
	f.calls = append(f.calls, "activate")
}

func (f *fakeDeathReservation) CancelImmediateDeathWork() {
	f.cancelCalls++
	f.calls = append(f.calls, "cancel")
}

func newFakeReservation(e *Engine, id EntityID) *fakeDeathReservation {
	return &fakeDeathReservation{engine: e, entity: id}
}

func orchEngine(t *testing.T) *Engine {
	t.Helper()
	return mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
}

func orchPlacements() (newbieHome, underworld world.Vec3) {
	return world.Vec3{X: 8, Y: 0, Z: 8}, world.Vec3{X: 1024, Y: 0, Z: -512}
}

// orchPolicies builds resolved item policies 1:1 with the
// canonical durable fixture in exact durable order.
func orchPolicies(dropByIndex map[int]bool) []ImmediateDeathItemPolicy {
	d := testFullDurableState()
	out := make([]ImmediateDeathItemPolicy, len(d.Items))
	for i, it := range d.Items {
		out[i] = ImmediateDeathItemPolicy{ItemID: it.ID, DropOnDeath: dropByIndex[i], RoomAccepts: true}
	}
	return out
}

// baseOrchInput is a Normal-death resolved input over the
// canonical durable fixture; callers adjust per case.
func baseOrchInput(now int64) ImmediateDeathResolvedInput {
	nh, uw := orchPlacements()
	return ImmediateDeathResolvedInput{
		NowSeconds:          now,
		DefaultDeathCost:    100,
		Context:             DeathContext{},
		Killer:              DeathKillerIdentity{Kind: DeathKillerNone, CharacterID: InvalidCharacterID},
		Items:               orchPolicies(nil),
		NewbieHomePlacement: nh,
		UnderworldPlacement: uw,
		RuntimeInputs:       testRuntimeInputs(),
	}
}

func addOrchPlayer(t *testing.T, e *Engine, charID CharacterID) EntityID {
	t.Helper()
	return addFullStatePlayer(t, e, charID, world.Vec3{X: 4, Y: 0, Z: 4}, testFullDurableState())
}

func mustLastDeath(t *testing.T, e *Engine, id EntityID) int64 {
	t.Helper()
	v, ok, err := e.PlayerLastDeathSecondsOf(id)
	if err != nil {
		t.Fatalf("PlayerLastDeathSecondsOf: %v", err)
	}
	if !ok {
		t.Fatalf("PlayerLastDeathSecondsOf(%d): not a player", uint64(id))
	}
	return v
}

func mustLife(t *testing.T, e *Engine, id EntityID) PlayerLifeState {
	t.Helper()
	life, ok, err := e.PlayerLifeStateOf(id)
	if err != nil {
		t.Fatalf("PlayerLifeStateOf: %v", err)
	}
	if !ok {
		t.Fatalf("PlayerLifeStateOf(%d): not a player", uint64(id))
	}
	return life
}

func loseOneHP(t *testing.T, e *Engine, id EntityID) {
	t.Helper()
	if _, _, err := e.PlayerLoseHealth(id, 1, false); err != nil {
		t.Fatalf("PlayerLoseHealth: %v", err)
	}
}

// A. Double-death timing: last=100, now=100/101 blocked,
// now=102/103 proceeds. Blocked leaves everything unchanged.
func TestDeathOrchestrationDoubleDeathTiming(t *testing.T) {
	e := orchEngine(t)
	id := addOrchPlayer(t, e, testCharacterID())

	// Stamp last=100 via an Avoided orchestration.
	stamp := baseOrchInput(100)
	stamp.Context = DeathContext{ArenaNonRealDeath: true}
	res := newFakeReservation(e, id)
	got, err := e.PlayerOrchestrateImmediateDeath(id, res, stamp)
	if err != nil {
		t.Fatalf("stamp orchestration: %v", err)
	}
	if got.Disposition != ImmediateDeathAvoided {
		t.Fatalf("stamp disposition = %d, want Avoided", uint8(got.Disposition))
	}
	if v := mustLastDeath(t, e, id); v != 100 {
		t.Fatalf("lastDeath = %d, want 100", v)
	}
	loseOneHP(t, e, id)

	for _, now := range []int64{100, 101} {
		in := baseOrchInput(now)
		in.Context = DeathContext{ArenaNonRealDeath: true}
		fake := newFakeReservation(e, id)
		got, err := e.PlayerOrchestrateImmediateDeath(id, fake, in)
		if err != nil {
			t.Fatalf("now=%d: %v", now, err)
		}
		if got.Disposition != ImmediateDeathBlocked {
			t.Fatalf("now=%d disposition = %d, want Blocked", now, uint8(got.Disposition))
		}
		if v := mustLastDeath(t, e, id); v != 100 {
			t.Fatalf("now=%d lastDeath = %d, want unchanged 100", now, v)
		}
		if v, _, _ := e.PlayerVitalsOf(id); v.HP != 0 {
			t.Fatalf("now=%d HP = %d, want 0", now, v.HP)
		}
		if ent, _ := e.registry.lookup(id); ent.deathEpoch != 0 {
			t.Fatalf("now=%d epoch = %d, want 0", now, ent.deathEpoch)
		}
		if mustLife(t, e, id) != PlayerLifeAlive {
			t.Fatalf("now=%d life not Alive", now)
		}
		if fake.cancelCalls != 1 || fake.prepareCalls != 0 || fake.activateCalls != 0 {
			t.Fatalf("now=%d fake calls cancel=%d prepare=%d activate=%d, want 1/0/0",
				now, fake.cancelCalls, fake.prepareCalls, fake.activateCalls)
		}
	}

	for _, now := range []int64{102, 104} {
		in := baseOrchInput(now)
		in.Context = DeathContext{ArenaNonRealDeath: true}
		fake := newFakeReservation(e, id)
		got, err := e.PlayerOrchestrateImmediateDeath(id, fake, in)
		if err != nil {
			t.Fatalf("now=%d: %v", now, err)
		}
		if got.Disposition != ImmediateDeathAvoided {
			t.Fatalf("now=%d disposition = %d, want Avoided", now, uint8(got.Disposition))
		}
		if v := mustLastDeath(t, e, id); v != now {
			t.Fatalf("now=%d lastDeath = %d, want %d", now, v, now)
		}
		loseOneHP(t, e, id)
	}
}

// B. Avoided death updates the timestamp, heals to 1, and
// reconciles health runtime exactly like NewHealth.
func TestDeathOrchestrationAvoidedDeath(t *testing.T) {
	for _, ctx := range []DeathContext{
		{ArenaNonRealDeath: true},
		{PrisonRoom: true},
		{SafePlayerAttack: true},
	} {
		e := orchEngine(t)
		id := addOrchPlayer(t, e, testCharacterID())
		beforePos, err := e.Entity(id)
		if err != nil {
			t.Fatalf("Entity: %v", err)
		}
		beforeDurable, ok, err := e.PlayerDurableStateOf(id)
		if err != nil || !ok {
			t.Fatalf("PlayerDurableStateOf: %v %v", ok, err)
		}

		in := baseOrchInput(77)
		in.Context = ctx
		fake := newFakeReservation(e, id)
		got, err := e.PlayerOrchestrateImmediateDeath(id, fake, in)
		if err != nil {
			t.Fatalf("ctx %+v: %v", ctx, err)
		}
		if got.Disposition != ImmediateDeathAvoided {
			t.Fatalf("ctx %+v disposition = %d, want Avoided", ctx, uint8(got.Disposition))
		}
		if got.Plan.Disposition != DeathAvoided {
			t.Fatalf("ctx %+v plan = %v, want avoided", ctx, got.Plan.Disposition)
		}
		if v := mustLastDeath(t, e, id); v != 77 {
			t.Fatalf("ctx %+v lastDeath = %d, want 77", ctx, v)
		}
		v, _, err := e.PlayerVitalsOf(id)
		if err != nil {
			t.Fatalf("PlayerVitalsOf: %v", err)
		}
		if v.HP != 1 {
			t.Fatalf("ctx %+v HP = %d, want 1", ctx, v.HP)
		}
		if mustLife(t, e, id) != PlayerLifeAlive {
			t.Fatalf("ctx %+v life not Alive", ctx)
		}
		if ent, _ := e.registry.lookup(id); ent.deathEpoch != 0 {
			t.Fatalf("ctx %+v epoch = %d, want 0", ctx, ent.deathEpoch)
		}
		afterPos, _ := e.Entity(id)
		if afterPos.Position != beforePos.Position {
			t.Fatalf("ctx %+v position moved", ctx)
		}
		afterDurable, ok, _ := e.PlayerDurableStateOf(id)
		if !ok || !reflect.DeepEqual(beforeDurable, afterDurable) {
			t.Fatalf("ctx %+v durable changed", ctx)
		}
		rt, _, err := e.PlayerVitalsRuntimeOf(id)
		if err != nil {
			t.Fatalf("PlayerVitalsRuntimeOf: %v", err)
		}
		if !rt.HealthArmed {
			t.Fatalf("ctx %+v health not armed after HP=1 (NewHealth mismatch)", ctx)
		}
		if fake.cancelCalls != 1 || fake.prepareCalls != 0 || fake.activateCalls != 0 {
			t.Fatalf("ctx %+v fake calls cancel=%d prepare=%d activate=%d, want 1/0/0",
				ctx, fake.cancelCalls, fake.prepareCalls, fake.activateCalls)
		}
	}
}

// Avoided death fires exactly one vitals observer event.
func TestDeathOrchestrationAvoidedSingleObserverEvent(t *testing.T) {
	var events []PlayerVitalsEvent
	e := mustEngine(t, 20, EngineDeps{
		Clock:  newManualClock(),
		RNG:    newTestRNG(1),
		Vitals: PlayerVitalsObserverFunc(func(ev PlayerVitalsEvent) { events = append(events, ev) }),
	})
	id := addOrchPlayer(t, e, testCharacterID())
	in := baseOrchInput(60)
	in.Context = DeathContext{PrisonRoom: true}
	if _, err := e.PlayerOrchestrateImmediateDeath(id, newFakeReservation(e, id), in); err != nil {
		t.Fatalf("orchestrate: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("observer events = %d, want exactly 1", len(events))
	}
	if events[0].Before.HP != 0 || events[0].After.HP != 1 {
		t.Fatalf("observer event HP %d -> %d, want 0 -> 1", events[0].Before.HP, events[0].After.HP)
	}
}

// C. Cheap causes: frenzy, newbie-zone, newbie-honor, token,
// and combinations.
func TestDeathOrchestrationCheapCauses(t *testing.T) {
	nh, uw := orchPlacements()
	cases := []struct {
		name    string
		ctx     DeathContext
		tokenID int64
		thr     int
		// want placement/pending expectations
		newbieHome bool
		frenzyVit  bool
		token      bool
	}{
		{"frenzy", DeathContext{FrenzyActive: true}, 0, 0, false, true, false},
		{"newbie-zone", DeathContext{NewbieZoneDeath: true}, 0, 0, true, false, false},
		{"newbie-honor", DeathContext{NewbieHonor: true}, 0, 0, false, false, false},
		{"token", DeathContext{CarriesToken: true}, 202, 80, false, false, true},
		{"frenzy+token", DeathContext{FrenzyActive: true, CarriesToken: true}, 202, 80, false, true, true},
		{"zone+honor", DeathContext{NewbieZoneDeath: true, NewbieHonor: true}, 0, 0, true, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := orchEngine(t)
			id := addOrchPlayer(t, e, testCharacterID())
			before, _, _ := e.PlayerVitalsOf(id)
			beforeDurable, _, _ := e.PlayerDurableStateOf(id)

			in := baseOrchInput(200)
			in.Context = tc.ctx
			in.TokenItemID = tc.tokenID
			in.TokenRestoredThreshold = tc.thr
			// Generous drop policies prove cheap deaths drop nothing.
			for i := range in.Items {
				in.Items[i].DropOnDeath = true
			}
			fake := newFakeReservation(e, id)
			got, err := e.PlayerOrchestrateImmediateDeath(id, fake, in)
			if err != nil {
				t.Fatalf("orchestrate: %v", err)
			}
			if got.Disposition != ImmediateDeathAccepted {
				t.Fatalf("disposition = %d, want Accepted", uint8(got.Disposition))
			}
			if got.Plan.Disposition != DeathCheap {
				t.Fatalf("plan = %v, want cheap", got.Plan.Disposition)
			}
			if got.Plan.DeathCost != 0 {
				t.Fatalf("cost = %d, want 0", got.Plan.DeathCost)
			}
			if v := mustLastDeath(t, e, id); v != 200 {
				t.Fatalf("lastDeath = %d, want 200", v)
			}
			// Advancement preserved byte-for-byte on cheap death.
			if string(got.Capture.Durable.Advancement) != string(beforeDurable.Advancement) {
				t.Fatalf("cheap advancement changed: %s", got.Capture.Durable.Advancement)
			}
			if got.Capture.Durable.Flags != beforeDurable.Flags {
				t.Fatalf("cheap flags changed")
			}
			// Corpse exists in the capture.
			if got.Capture.Corpse.LifetimeMs != PlayerCorpseDecomposeMs ||
				got.Capture.Corpse.NoStealMs != PlayerCorpseNoStealMs ||
				got.Capture.Corpse.DeathTimeSeconds != 200 {
				t.Fatalf("cheap corpse = %+v", got.Capture.Corpse)
			}
			// Placement + pending selection.
			wantPlace := uw
			if tc.newbieHome {
				wantPlace = nh
			}
			if got.Capture.Placement != wantPlace {
				t.Fatalf("placement = %+v, want %+v", got.Capture.Placement, wantPlace)
			}
			if tc.newbieHome {
				if got.Capture.Pending.Phase != DeathPhaseNone {
					t.Fatalf("newbie-home pending = %d, want None", int8(got.Capture.Pending.Phase))
				}
			} else {
				if got.Capture.Pending.Phase != DeathPhasePending ||
					got.Capture.Pending.EffectiveDeathCost != 0 ||
					got.Capture.Pending.DeathTimeSeconds != 200 {
					t.Fatalf("pending = %+v", got.Capture.Pending)
				}
			}
			// Post vitals.
			pv := got.Capture.Vitals
			if tc.frenzyVit {
				if pv.HP != before.MaxHP/2 || pv.Mana != before.MaxMana/2 || pv.Vigor != 100 {
					t.Fatalf("frenzy vitals = %+v", pv)
				}
			} else {
				if pv.HP != 1 || pv.Mana != 1 {
					t.Fatalf("cheap vitals HP/Mana = %d/%d, want 1/1", pv.HP, pv.Mana)
				}
			}
			if tc.token {
				if pv.RestThreshold != tc.thr {
					t.Fatalf("token threshold = %d, want %d", pv.RestThreshold, tc.thr)
				}
				for _, it := range got.Capture.Durable.Items {
					if it.ID == tc.tokenID {
						t.Fatalf("token %d still in durable inventory", tc.tokenID)
					}
				}
				if len(got.Capture.AffectedItems) != 1 || got.Capture.AffectedItems[0].ID != tc.tokenID {
					t.Fatalf("affected = %+v, want exactly token %d", got.Capture.AffectedItems, tc.tokenID)
				}
				if got.Capture.AffectedItems[0].PKProtectionDurationMs != 0 {
					t.Fatalf("token got PK protection")
				}
			} else {
				if len(got.Capture.AffectedItems) != 0 {
					t.Fatalf("cheap affected = %+v, want none", got.Capture.AffectedItems)
				}
				if pv.RestThreshold != before.RestThreshold {
					t.Fatalf("non-token threshold changed")
				}
			}
			if mustLife(t, e, id) != PlayerLifeDeathPersisting {
				t.Fatalf("life not DeathPersisting")
			}
			if fake.prepareCalls != 1 || fake.activateCalls != 1 || fake.cancelCalls != 0 {
				t.Fatalf("fake calls prepare=%d activate=%d cancel=%d, want 1/1/0",
					fake.prepareCalls, fake.activateCalls, fake.cancelCalls)
			}
			if got.Token.Epoch != 1 || got.Token.EntityID != id || got.Token.CharacterID != testCharacterID() {
				t.Fatalf("token = %+v", got.Token)
			}
		})
	}
}

// D. Normal death: full pipeline exactness.
func TestDeathOrchestrationNormalDeath(t *testing.T) {
	e := orchEngine(t)
	id := addOrchPlayer(t, e, testCharacterID())
	before, _, _ := e.PlayerVitalsOf(id)
	_, uw := orchPlacements()

	in := baseOrchInput(300)
	in.DefaultDeathCost = 90
	in.Killer = DeathKillerIdentity{Kind: DeathKillerCharacter, CharacterID: CharacterID(9)}
	in.StillNewbie = true
	drops := map[int]bool{0: true, 2: true}
	in.Items = orchPolicies(drops)
	fake := newFakeReservation(e, id)
	got, err := e.PlayerOrchestrateImmediateDeath(id, fake, in)
	if err != nil {
		t.Fatalf("orchestrate: %v", err)
	}
	if got.Disposition != ImmediateDeathAccepted || got.Plan.Disposition != DeathNormal {
		t.Fatalf("disposition = %d/%v", uint8(got.Disposition), got.Plan.Disposition)
	}
	if got.Plan.DeathCost != 90 {
		t.Fatalf("cost = %d, want 90", got.Plan.DeathCost)
	}
	if got.Capture.Placement != uw {
		t.Fatalf("placement = %+v, want underworld", got.Capture.Placement)
	}
	if got.Capture.Pending.Phase != DeathPhasePending || got.Capture.Pending.EffectiveDeathCost != 90 ||
		got.Capture.Pending.DeathTimeSeconds != 300 {
		t.Fatalf("pending = %+v", got.Capture.Pending)
	}
	pv := got.Capture.Vitals
	if pv.HP != 1 {
		t.Fatalf("post HP = %d, want 1", pv.HP)
	}
	wantMana := before.MaxMana/2 + 2 // angel eligible: still-newbie non-murderer cost>0
	if pv.Mana != wantMana {
		t.Fatalf("post Mana = %d, want angel %d", pv.Mana, wantMana)
	}
	if !got.Hooks.GuardianAngelMail {
		t.Fatalf("angel mail not classified")
	}
	// Advancement five-effect plan applied.
	if got.Capture.Durable.Flags&0x70 != 0 {
		t.Fatalf("gain flags not cleared: %#x", got.Capture.Durable.Flags)
	}
	if got.Capture.Durable.Flags&^0x70 != 0x1274&^0x70 {
		t.Fatalf("unrelated flags changed: %#x", got.Capture.Durable.Flags)
	}
	for _, a := range append(append([]PlayerAbilityState{}, got.Capture.Durable.Spells...), got.Capture.Durable.Skills...) {
		if !a.AtrophyFlag {
			t.Fatalf("atrophy not reset: %+v", a)
		}
	}
	// Normal drops exact with PK metadata.
	if len(got.Capture.AffectedItems) != 2 ||
		got.Capture.AffectedItems[0].ID != 101 || got.Capture.AffectedItems[1].ID != 303 {
		t.Fatalf("affected = %+v, want [101 303]", got.Capture.AffectedItems)
	}
	for _, a := range got.Capture.AffectedItems {
		if a.PKProtectionDurationMs != PKProtectionDurationMs {
			t.Fatalf("affected %+v PK duration = %d", a, a.PKProtectionDurationMs)
		}
	}
	if len(got.Capture.Durable.Items) != 1 || got.Capture.Durable.Items[0].ID != 202 {
		t.Fatalf("remaining durable = %+v, want [202]", got.Capture.Durable.Items)
	}
	if got.Capture.Killer != in.Killer {
		t.Fatalf("killer = %+v", got.Capture.Killer)
	}
	if v := mustLastDeath(t, e, id); v != 300 {
		t.Fatalf("lastDeath = %d, want 300", v)
	}
	if mustLife(t, e, id) != PlayerLifeDeathPersisting {
		t.Fatalf("life not DeathPersisting")
	}
	if fake.prepareCalls != 1 || fake.activateCalls != 1 {
		t.Fatalf("fake prepare=%d activate=%d, want 1/1", fake.prepareCalls, fake.activateCalls)
	}
}

// E. Token bad contracts fail before the irreversible begin.
func TestDeathOrchestrationTokenContractFailures(t *testing.T) {
	tokenCtx := DeathContext{CarriesToken: true}
	cases := []struct {
		name  string
		ctx   DeathContext
		id    int64
		thr   int
		items func() []ImmediateDeathItemPolicy
	}{
		{"zero-id", tokenCtx, 0, 80, func() []ImmediateDeathItemPolicy { return orchPolicies(nil) }},
		{"absent-id", tokenCtx, 999, 80, func() []ImmediateDeathItemPolicy { return orchPolicies(nil) }},
		{"misaligned", tokenCtx, 202, 80, func() []ImmediateDeathItemPolicy {
			p := orchPolicies(nil)
			p[0], p[1] = p[1], p[0]
			return p
		}},
		{"non-token-nonzero-id", DeathContext{}, 101, 0, func() []ImmediateDeathItemPolicy { return orchPolicies(nil) }},
		{"non-token-threshold", DeathContext{}, 0, 80, func() []ImmediateDeathItemPolicy { return orchPolicies(nil) }},
		{"bad-threshold-low", tokenCtx, 202, 5, func() []ImmediateDeathItemPolicy { return orchPolicies(nil) }},
		{"bad-threshold-high", tokenCtx, 202, 200, func() []ImmediateDeathItemPolicy { return orchPolicies(nil) }},
		{"zero-threshold", tokenCtx, 202, 0, func() []ImmediateDeathItemPolicy { return orchPolicies(nil) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := orchEngine(t)
			id := addOrchPlayer(t, e, testCharacterID())
			in := baseOrchInput(400)
			in.Context = tc.ctx
			in.TokenItemID = tc.id
			in.TokenRestoredThreshold = tc.thr
			in.Items = tc.items()
			fake := newFakeReservation(e, id)
			if _, err := e.PlayerOrchestrateImmediateDeath(id, fake, in); err == nil {
				t.Fatalf("expected error")
			} else if !errors.Is(err, ErrInvalidDeathInput) {
				t.Fatalf("error = %v, want ErrInvalidDeathInput", err)
			}
			if v := mustLastDeath(t, e, id); v != 0 {
				t.Fatalf("lastDeath = %d, want unchanged 0", v)
			}
			if v, _, _ := e.PlayerVitalsOf(id); v.HP != 0 {
				t.Fatalf("HP changed")
			}
			if mustLife(t, e, id) != PlayerLifeAlive {
				t.Fatalf("life changed")
			}
			if fake.prepareCalls != 0 || fake.activateCalls != 0 || fake.cancelCalls != 1 {
				t.Fatalf("fake prepare=%d activate=%d cancel=%d, want 0/0/1",
					fake.prepareCalls, fake.activateCalls, fake.cancelCalls)
			}
		})
	}
}

// F. Item-policy alignment: reorder/wrong/missing/extra/duplicate.
func TestDeathOrchestrationItemAlignment(t *testing.T) {
	mk := func(ids ...int64) []ImmediateDeathItemPolicy {
		out := make([]ImmediateDeathItemPolicy, len(ids))
		for i, id := range ids {
			out[i] = ImmediateDeathItemPolicy{ItemID: id, DropOnDeath: true, RoomAccepts: true}
		}
		return out
	}
	for name, ids := range map[string][]int64{
		"reorder":   {202, 101, 303},
		"wrong-id":  {101, 202, 999},
		"missing":   {101, 202},
		"extra":     {101, 202, 303, 404},
		"duplicate": {101, 101, 303},
	} {
		t.Run(name, func(t *testing.T) {
			e := orchEngine(t)
			id := addOrchPlayer(t, e, testCharacterID())
			in := baseOrchInput(400)
			in.Items = mk(ids...)
			fake := newFakeReservation(e, id)
			if _, err := e.PlayerOrchestrateImmediateDeath(id, fake, in); err == nil {
				t.Fatalf("expected error")
			} else if !errors.Is(err, ErrInvalidDeathInput) {
				t.Fatalf("error = %v", err)
			}
			if v := mustLastDeath(t, e, id); v != 0 {
				t.Fatalf("lastDeath changed")
			}
			if fake.prepareCalls != 0 || fake.cancelCalls != 1 {
				t.Fatalf("fake prepare=%d cancel=%d", fake.prepareCalls, fake.cancelCalls)
			}
		})
	}
}

// G. Killer identity: none/character/mob captured exact;
// invalid contracts fail before begin.
func TestDeathOrchestrationKillerIdentity(t *testing.T) {
	valid := []DeathKillerIdentity{
		{Kind: DeathKillerNone, CharacterID: InvalidCharacterID},
		{Kind: DeathKillerCharacter, CharacterID: CharacterID(9)},
		{Kind: DeathKillerMob, CharacterID: InvalidCharacterID, MobID: 44},
	}
	for _, k := range valid {
		e := orchEngine(t)
		id := addOrchPlayer(t, e, testCharacterID())
		in := baseOrchInput(500)
		in.Killer = k
		got, err := e.PlayerOrchestrateImmediateDeath(id, newFakeReservation(e, id), in)
		if err != nil {
			t.Fatalf("killer %+v: %v", k, err)
		}
		if got.Capture.Killer != k {
			t.Fatalf("killer = %+v, want %+v", got.Capture.Killer, k)
		}
	}
	invalid := []DeathKillerIdentity{
		{Kind: DeathKillerCharacter, CharacterID: InvalidCharacterID},
		{Kind: DeathKillerMob, CharacterID: InvalidCharacterID, MobID: 0},
		{Kind: DeathKillerKind(99)},
		{Kind: DeathKillerNone, CharacterID: CharacterID(3)},
	}
	for _, k := range invalid {
		e := orchEngine(t)
		id := addOrchPlayer(t, e, testCharacterID())
		in := baseOrchInput(500)
		in.Killer = k
		fake := newFakeReservation(e, id)
		if _, err := e.PlayerOrchestrateImmediateDeath(id, fake, in); err == nil {
			t.Fatalf("killer %+v: expected error", k)
		}
		if v := mustLastDeath(t, e, id); v != 0 {
			t.Fatalf("killer %+v: lastDeath changed", k)
		}
		if fake.prepareCalls != 0 || fake.cancelCalls != 1 {
			t.Fatalf("killer %+v: fake prepare=%d cancel=%d", k, fake.prepareCalls, fake.cancelCalls)
		}
	}
}

// H. Guardian/Hook classification without any delivery or
// durable shield mutation.
func TestDeathOrchestrationHooksClassification(t *testing.T) {
	// Normal still-newbie non-murderer -> mail; murderer -> none.
	for _, murderer := range []bool{false, true} {
		e := orchEngine(t)
		id := addOrchPlayer(t, e, testCharacterID())
		beforeDurable, _, _ := e.PlayerDurableStateOf(id)
		in := baseOrchInput(600)
		in.StillNewbie = true
		in.Murderer = murderer
		got, err := e.PlayerOrchestrateImmediateDeath(id, newFakeReservation(e, id), in)
		if err != nil {
			t.Fatalf("murderer=%v: %v", murderer, err)
		}
		if got.Hooks.GuardianAngelMail == murderer {
			t.Fatalf("murderer=%v mail=%v", murderer, got.Hooks.GuardianAngelMail)
		}
		if len(got.Capture.AffectedItems) != 0 {
			t.Fatalf("murderer=%v affected = %+v, want none", murderer, got.Capture.AffectedItems)
		}
		if !reflect.DeepEqual(beforeDurable.Items, got.Capture.Durable.Items) {
			t.Fatalf("murderer=%v durable items changed", murderer)
		}
	}
	// Cheap -> no mail even for a still-newbie non-murderer.
	{
		e := orchEngine(t)
		id := addOrchPlayer(t, e, testCharacterID())
		in := baseOrchInput(600)
		in.Context = DeathContext{FrenzyActive: true}
		in.StillNewbie = true
		got, err := e.PlayerOrchestrateImmediateDeath(id, newFakeReservation(e, id), in)
		if err != nil {
			t.Fatalf("cheap: %v", err)
		}
		if got.Hooks.GuardianAngelMail {
			t.Fatalf("cheap death classified mail")
		}
	}
	// SoldierShield pure outcomes: rank 5 survives at 1, rank 2 dies.
	for _, tc := range []struct {
		rank      int
		wantAfter int
		wantDied  bool
	}{
		{5, 1, false},
		{2, 0, true},
	} {
		e := orchEngine(t)
		id := addOrchPlayer(t, e, testCharacterID())
		in := baseOrchInput(600)
		in.HasSoldierShield = true
		in.SoldierShieldRank = tc.rank
		in.KilledByShieldEnemy = true
		got, err := e.PlayerOrchestrateImmediateDeath(id, newFakeReservation(e, id), in)
		if err != nil {
			t.Fatalf("rank %d: %v", tc.rank, err)
		}
		sh := got.Hooks.SoldierShield
		if !sh.Triggered || sh.Died() != tc.wantDied || (!tc.wantDied && sh.RankAfter != tc.wantAfter) {
			t.Fatalf("rank %d effect = %+v", tc.rank, sh)
		}
	}
	// Invalid shield rank fails before begin.
	{
		e := orchEngine(t)
		id := addOrchPlayer(t, e, testCharacterID())
		in := baseOrchInput(600)
		in.HasSoldierShield = true
		in.SoldierShieldRank = 11
		in.KilledByShieldEnemy = true
		fake := newFakeReservation(e, id)
		if _, err := e.PlayerOrchestrateImmediateDeath(id, fake, in); err == nil {
			t.Fatalf("expected rank error")
		}
		if v := mustLastDeath(t, e, id); v != 0 {
			t.Fatalf("lastDeath changed")
		}
		if fake.prepareCalls != 0 {
			t.Fatalf("prepared despite invalid rank")
		}
	}
}

// I. A reservation whose Prepare fails: no begin, no quiesce,
// no activation, no leaked capacity.
func TestDeathOrchestrationPrepareFailure(t *testing.T) {
	e := orchEngine(t)
	id := addOrchPlayer(t, e, testCharacterID())
	if _, _, err := e.PlayerLoseMana(id, 5); err != nil {
		t.Fatalf("PlayerLoseMana: %v", err)
	}
	rtBefore, _, _ := e.PlayerVitalsRuntimeOf(id)
	if !rtBefore.ManaArmed {
		t.Fatalf("mana deadline not armed for quiesce proof")
	}
	in := baseOrchInput(700)
	fake := newFakeReservation(e, id)
	fake.prepareErr = errors.New("boom: frozen work invalid")
	if _, err := e.PlayerOrchestrateImmediateDeath(id, fake, in); err == nil {
		t.Fatalf("expected Prepare error")
	}
	if mustLife(t, e, id) != PlayerLifeAlive {
		t.Fatalf("life changed")
	}
	if v, _, _ := e.PlayerVitalsOf(id); v.HP != 0 {
		t.Fatalf("HP changed")
	}
	if ent, _ := e.registry.lookup(id); ent.deathEpoch != 0 {
		t.Fatalf("epoch changed")
	}
	rtAfter, _, _ := e.PlayerVitalsRuntimeOf(id)
	if !rtAfter.ManaArmed {
		t.Fatalf("mana deadline disarmed: quiesce ran despite Prepare failure")
	}
	if v := mustLastDeath(t, e, id); v != 700 {
		t.Fatalf("lastDeath = %d, want stamped 700", v)
	}
	if fake.activateCalls != 0 || fake.cancelCalls != 1 {
		t.Fatalf("fake activate=%d cancel=%d, want 0/1", fake.activateCalls, fake.cancelCalls)
	}
}

// J. Accepted reservation order: Prepare while Alive, then
// begin, then exactly one Activate.
func TestDeathOrchestrationReservationOrder(t *testing.T) {
	e := orchEngine(t)
	id := addOrchPlayer(t, e, testCharacterID())
	in := baseOrchInput(800)
	fake := newFakeReservation(e, id)
	got, err := e.PlayerOrchestrateImmediateDeath(id, fake, in)
	if err != nil {
		t.Fatalf("orchestrate: %v", err)
	}
	if len(fake.calls) != 2 || fake.calls[0] != "prepare" || fake.calls[1] != "activate" {
		t.Fatalf("calls = %v, want [prepare activate]", fake.calls)
	}
	if len(fake.lifeAtPrepare) != 1 || fake.lifeAtPrepare[0] != PlayerLifeAlive {
		t.Fatalf("life at Prepare = %v, want [Alive]", fake.lifeAtPrepare)
	}
	if len(fake.lifeAtActivate) != 1 || fake.lifeAtActivate[0] != PlayerLifeDeathPersisting {
		t.Fatalf("life at Activate = %v, want [DeathPersisting]", fake.lifeAtActivate)
	}
	if got.Token.Epoch != 1 {
		t.Fatalf("epoch = %d, want 1", got.Token.Epoch)
	}
}

// K. Begin failure after successful prevalidation: epoch
// exhaustion, migrating race, locked life. All cancel safely.
func TestDeathOrchestrationBeginFailures(t *testing.T) {
	t.Run("epoch-exhausted", func(t *testing.T) {
		e := orchEngine(t)
		id := addOrchPlayer(t, e, testCharacterID())
		ent, _ := e.registry.lookup(id)
		ent.deathEpoch = math.MaxUint64
		fake := newFakeReservation(e, id)
		if _, err := e.PlayerOrchestrateImmediateDeath(id, fake, baseOrchInput(900)); !errors.Is(err, ErrDeathAttemptExhausted) {
			t.Fatalf("error = %v, want ErrDeathAttemptExhausted", err)
		}
		if fake.prepareCalls != 0 || fake.activateCalls != 0 || fake.cancelCalls != 1 {
			t.Fatalf("fake prepare=%d activate=%d cancel=%d", fake.prepareCalls, fake.activateCalls, fake.cancelCalls)
		}
		if v := mustLastDeath(t, e, id); v != 0 {
			t.Fatalf("lastDeath changed")
		}
	})
	t.Run("migrating", func(t *testing.T) {
		e := orchEngine(t)
		id := addOrchPlayer(t, e, testCharacterID())
		snap, _ := e.Entity(id)
		dest := world.CellCoord{X: snap.Cell.X + 1, Z: snap.Cell.Z}
		final := world.Vec3{X: float64(int32(dest.X) * 32), Y: 0, Z: 4}
		tok, err := e.registry.beginHandoff(id, OwnerRef{Cell: snap.Cell, Generation: snap.OwnershipGeneration}, dest, final)
		if err != nil {
			t.Fatalf("beginHandoff: %v", err)
		}
		_ = tok
		fake := newFakeReservation(e, id)
		if _, err := e.PlayerOrchestrateImmediateDeath(id, fake, baseOrchInput(900)); !errors.Is(err, ErrCellHandoffRequired) {
			t.Fatalf("error = %v, want ErrCellHandoffRequired", err)
		}
		if fake.prepareCalls != 0 || fake.cancelCalls != 1 {
			t.Fatalf("fake prepare=%d cancel=%d", fake.prepareCalls, fake.cancelCalls)
		}
		e.registry.abortHandoff(id)
	})
	t.Run("locked-life", func(t *testing.T) {
		e := orchEngine(t)
		id := addOrchPlayer(t, e, testCharacterID())
		if _, err := e.PlayerBeginDeathPersistence(id); err != nil {
			t.Fatalf("begin: %v", err)
		}
		fake := newFakeReservation(e, id)
		if _, err := e.PlayerOrchestrateImmediateDeath(id, fake, baseOrchInput(900)); !errors.Is(err, ErrPlayerNotAlive) {
			t.Fatalf("error = %v, want ErrPlayerNotAlive", err)
		}
		if ent, _ := e.registry.lookup(id); ent.deathEpoch != 1 {
			t.Fatalf("epoch = %d, want still 1", ent.deathEpoch)
		}
		if fake.prepareCalls != 0 || fake.cancelCalls != 1 {
			t.Fatalf("fake prepare=%d cancel=%d", fake.prepareCalls, fake.cancelCalls)
		}
	})
}

// L. Structural validation atomicity: every malformed input
// fails with lastDeath unchanged and the entity bit-identical.
func TestDeathOrchestrationStructuralAtomicity(t *testing.T) {
	nh, uw := orchPlacements()
	badPlacement := world.Vec3{X: math.NaN(), Y: 0, Z: 0}
	mk := func(mut func(*ImmediateDeathResolvedInput)) ImmediateDeathResolvedInput {
		in := baseOrchInput(1000)
		mut(&in)
		return in
	}
	cases := []struct {
		name string
		in   ImmediateDeathResolvedInput
	}{
		{"now-negative", mk(func(in *ImmediateDeathResolvedInput) { in.NowSeconds = -1 })},
		{"cost-zero", mk(func(in *ImmediateDeathResolvedInput) { in.DefaultDeathCost = 0 })},
		{"cost-high", mk(func(in *ImmediateDeathResolvedInput) { in.DefaultDeathCost = 101 })},
		{"runtime", mk(func(in *ImmediateDeathResolvedInput) { in.RuntimeInputs.EffectiveStamina = 0 })},
		{"newbie-placement", mk(func(in *ImmediateDeathResolvedInput) { in.NewbieHomePlacement = badPlacement })},
		{"underworld-placement", mk(func(in *ImmediateDeathResolvedInput) { in.UnderworldPlacement = badPlacement })},
		{"killer", mk(func(in *ImmediateDeathResolvedInput) {
			in.Killer = DeathKillerIdentity{Kind: DeathKillerCharacter, CharacterID: InvalidCharacterID}
		})},
		{"policies", mk(func(in *ImmediateDeathResolvedInput) { in.Items = in.Items[:1] })},
		{"token-shape", mk(func(in *ImmediateDeathResolvedInput) { in.TokenItemID = -3 })},
		{"shield-rank", mk(func(in *ImmediateDeathResolvedInput) {
			in.HasSoldierShield = true
			in.SoldierShieldRank = 0
		})},
	}
	_ = nh
	_ = uw
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := orchEngine(t)
			id := addOrchPlayer(t, e, testCharacterID())
			beforeSnap, _ := e.Entity(id)
			beforeVitals, _, _ := e.PlayerVitalsOf(id)
			beforeDurable, _, _ := e.PlayerDurableStateOf(id)
			fake := newFakeReservation(e, id)
			if _, err := e.PlayerOrchestrateImmediateDeath(id, fake, tc.in); err == nil {
				t.Fatalf("expected error")
			}
			if v := mustLastDeath(t, e, id); v != 0 {
				t.Fatalf("lastDeath = %d, want 0", v)
			}
			afterSnap, _ := e.Entity(id)
			if afterSnap != beforeSnap {
				t.Fatalf("snapshot changed")
			}
			afterVitals, _, _ := e.PlayerVitalsOf(id)
			if afterVitals != beforeVitals {
				t.Fatalf("vitals changed")
			}
			afterDurable, _, _ := e.PlayerDurableStateOf(id)
			if !reflect.DeepEqual(beforeDurable, afterDurable) {
				t.Fatalf("durable changed")
			}
			if mustLife(t, e, id) != PlayerLifeAlive {
				t.Fatalf("life changed")
			}
			if fake.prepareCalls != 0 || fake.activateCalls != 0 || fake.cancelCalls != 1 {
				t.Fatalf("fake prepare=%d activate=%d cancel=%d", fake.prepareCalls, fake.activateCalls, fake.cancelCalls)
			}
		})
	}
	// Legacy minimal players without a durable shadow fail closed.
	t.Run("missing-durable", func(t *testing.T) {
		e := orchEngine(t)
		snap, err := e.AddPlayerEntity(testCharacterID(), world.Vec3{X: 4, Y: 0, Z: 4}, zeroHPVitals(t), testRuntimeInputs())
		if err != nil {
			t.Fatalf("AddPlayerEntity: %v", err)
		}
		fake := newFakeReservation(e, snap.ID)
		if _, err := e.PlayerOrchestrateImmediateDeath(snap.ID, fake, baseOrchInput(1000)); !errors.Is(err, ErrPlayerDurableStateMissing) {
			t.Fatalf("error = %v, want ErrPlayerDurableStateMissing", err)
		}
		if v := mustLastDeath(t, e, snap.ID); v != 0 {
			t.Fatalf("lastDeath changed")
		}
		if fake.cancelCalls != 1 {
			t.Fatalf("cancel=%d, want 1", fake.cancelCalls)
		}
	})
	// Nil reservation rejected before mutation.
	t.Run("nil-reservation", func(t *testing.T) {
		e := orchEngine(t)
		id := addOrchPlayer(t, e, testCharacterID())
		if _, err := e.PlayerOrchestrateImmediateDeath(id, nil, baseOrchInput(1000)); !errors.Is(err, ErrInvalidDeathInput) {
			t.Fatalf("error = %v, want ErrInvalidDeathInput", err)
		}
		if v := mustLastDeath(t, e, id); v != 0 {
			t.Fatalf("lastDeath changed")
		}
	})
	// Zero-HP entry contract: HP != 0, non-Alive handled.
	t.Run("hp-nonzero", func(t *testing.T) {
		e := orchEngine(t)
		id := addOrchPlayer(t, e, testCharacterID())
		in := baseOrchInput(1000)
		in.Context = DeathContext{ArenaNonRealDeath: true}
		if _, err := e.PlayerOrchestrateImmediateDeath(id, newFakeReservation(e, id), in); err != nil {
			t.Fatalf("avoided: %v", err)
		}
		fake := newFakeReservation(e, id)
		if _, err := e.PlayerOrchestrateImmediateDeath(id, fake, baseOrchInput(1001)); !errors.Is(err, ErrPlayerNotDead) {
			t.Fatalf("error = %v, want ErrPlayerNotDead", err)
		}
		if v := mustLastDeath(t, e, id); v != 1000 {
			t.Fatalf("lastDeath = %d, want 1000", v)
		}
		if fake.cancelCalls != 1 {
			t.Fatalf("cancel=%d", fake.cancelCalls)
		}
	})
}

// M. Handoff/removal semantics for lastDeathSeconds.
func TestDeathOrchestrationHandoffRemoval(t *testing.T) {
	e := orchEngine(t)
	id := addOrchPlayer(t, e, testCharacterID())
	in := baseOrchInput(50)
	in.Context = DeathContext{SafePlayerAttack: true}
	if _, err := e.PlayerOrchestrateImmediateDeath(id, newFakeReservation(e, id), in); err != nil {
		t.Fatalf("avoided: %v", err)
	}
	loseOneHP(t, e, id)
	// Drive east across the cell boundary with run.
	if _, err := e.SubmitMove(id, MoveIntent{InputSeq: 1, HeldDirs: 1, RunFlag: 1, Yaw: 1024}); err != nil {
		t.Fatalf("SubmitMove: %v", err)
	}
	moved := false
	for i := 0; i < 400; i++ {
		e.Step()
		snap, _ := e.Entity(id)
		if snap.Cell.X != 0 {
			moved = true
			break
		}
	}
	if !moved {
		t.Fatalf("player never crossed the cell boundary")
	}
	if v := mustLastDeath(t, e, id); v != 50 {
		t.Fatalf("lastDeath after handoff = %d, want 50", v)
	}
	// Removal discards it; a fresh entity for the same
	// character restarts at 0.
	if err := e.RemoveEntity(id); err != nil {
		t.Fatalf("RemoveEntity: %v", err)
	}
	if _, _, err := e.PlayerLastDeathSecondsOf(id); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("inspection after remove = %v, want ErrEntityNotFound", err)
	}
	snap, err := e.AddPlayerEntityWithDurableState(testCharacterID(), world.Vec3{X: 4, Y: 0, Z: 4}, zeroHPVitals(t), testRuntimeInputs(), testFullDurableState())
	if err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if v := mustLastDeath(t, e, snap.ID); v != 0 {
		t.Fatalf("fresh lastDeath = %d, want 0", v)
	}
}

// N. Caller mutation of resolved inputs after return cannot
// reach the frozen capture or the prepared work.
func TestDeathOrchestrationNoAliases(t *testing.T) {
	e := orchEngine(t)
	id := addOrchPlayer(t, e, testCharacterID())
	in := baseOrchInput(1100)
	in.Items = orchPolicies(map[int]bool{1: true})
	fake := newFakeReservation(e, id)
	got, err := e.PlayerOrchestrateImmediateDeath(id, fake, in)
	if err != nil {
		t.Fatalf("orchestrate: %v", err)
	}
	for i := range in.Items {
		in.Items[i].ItemID = 999999
		in.Items[i].DropOnDeath = !in.Items[i].DropOnDeath
		in.Items[i].RoomAccepts = !in.Items[i].RoomAccepts
	}
	in.TokenItemID = 424242
	in.NewbieHomePlacement = world.Vec3{X: -999, Y: -999, Z: -999}
	in.UnderworldPlacement = world.Vec3{X: -999, Y: -999, Z: -999}
	if len(got.Capture.Durable.Items) != 2 ||
		got.Capture.Durable.Items[0].ID != 101 || got.Capture.Durable.Items[1].ID != 303 {
		t.Fatalf("capture durable aliased: %+v", got.Capture.Durable.Items)
	}
	if len(fake.gotCapture.Durable.Items) != 2 || fake.gotCapture.Durable.Items[0].ID != 101 {
		t.Fatalf("prepared capture aliased: %+v", fake.gotCapture.Durable.Items)
	}
	if len(fake.gotCapture.AffectedItems) != 1 || fake.gotCapture.AffectedItems[0].ID != 202 {
		t.Fatalf("prepared affected aliased: %+v", fake.gotCapture.AffectedItems)
	}
}

// Inspection contract for the ephemeral timestamp.
func TestDeathOrchestrationLastDeathInspection(t *testing.T) {
	e := orchEngine(t)
	if _, ok, err := e.PlayerLastDeathSecondsOf(EntityID(4242)); !errors.Is(err, ErrEntityNotFound) || ok {
		t.Fatalf("unknown = %v/%v", ok, err)
	}
	gen, err := e.AddEntity(world.Vec3{X: 1, Y: 0, Z: 1})
	if err != nil {
		t.Fatalf("AddEntity: %v", err)
	}
	if v, ok, err := e.PlayerLastDeathSecondsOf(gen.ID); err != nil || ok || v != 0 {
		t.Fatalf("generic = %d/%v/%v", v, ok, err)
	}
	id := addOrchPlayer(t, e, testCharacterID())
	if v := mustLastDeath(t, e, id); v != 0 {
		t.Fatalf("fresh = %d, want 0", v)
	}
}
