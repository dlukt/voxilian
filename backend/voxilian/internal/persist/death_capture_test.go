package persist

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/store"
	"github.com/dlukt/voxilian/internal/world"
)

// M5-T5c3c2 Store-domain mapping tests (spec §9.5.1g): the
// mechanical sim -> Store translation of a complete
// ImmediateDeathCapture into store.DeathEntryRequest. No PG, no
// Saver, no recovery: the mapper never touches the database.

// Minimal engine fakes: the mapper tests build real sim captures
// through the public owner-local sim API.
type captureTestTicker struct{ ch chan time.Time }

func (t *captureTestTicker) C() <-chan time.Time { return t.ch }
func (t *captureTestTicker) Stop()               {}

type captureTestClock struct{}

func (captureTestClock) NewTicker(time.Duration) sim.Ticker {
	return &captureTestTicker{ch: make(chan time.Time)}
}

type captureTestRNG struct{ v uint64 }

func (r *captureTestRNG) Uint64() uint64 { r.v++; return r.v }

type captureTestCollision struct{}

func (captureTestCollision) SolidAt(world.Vec3) bool { return false }
func (captureTestCollision) VolumeFlagsAt(world.Vec3) world.VolumeFlags {
	return world.VolumeNone
}

type captureTestRunGate struct{}

func (captureTestRunGate) CanRun(sim.EntityID) bool { return true }

func mustSimEngine(t *testing.T) *sim.Engine {
	t.Helper()
	e, err := sim.NewEngine(sim.EngineConfig{TickHz: 20}, sim.EngineDeps{
		Clock: captureTestClock{}, RNG: &captureTestRNG{},
		Collision: captureTestCollision{}, RunGate: captureTestRunGate{},
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return e
}

func testDeathVitals(t *testing.T) sim.PlayerVitals {
	t.Helper()
	v, err := sim.NewPlayerVitals(25)
	if err != nil {
		t.Fatalf("NewPlayerVitals: %v", err)
	}
	return v
}

func zeroHPDeathVitals(t *testing.T) sim.PlayerVitals {
	t.Helper()
	v := testDeathVitals(t)
	v.HP = 0
	if err := v.Validate(); err != nil {
		t.Fatalf("zeroHPVitals invalid: %v", err)
	}
	return v
}

func testDeathRuntimeInputs(t *testing.T) sim.PlayerVitalsRuntimeInputs {
	t.Helper()
	in := sim.PlayerVitalsRuntimeInputs{
		EffectiveStamina: 25, EffectiveMysticism: 25, RestRecoveryMultiplier: 1,
	}
	if err := in.Validate(); err != nil {
		t.Fatalf("runtime inputs invalid: %v", err)
	}
	return in
}

// fullDeathDurableState mirrors the sim-side fixture: deliberate
// ItemIDs (101+) never equal the opaque capture-order T5a keys.
func fullDeathDurableState() sim.PlayerDurableState {
	return sim.PlayerDurableState{
		Karma:       150,
		Advancement: []byte(`{"adv_points":7,"gain_chance":-40,"adv_timer_due":12345,"school_casts":{"1":3},"custom":"keep"}`),
		Flags:       0x1274,
		Spells: []sim.PlayerAbilityState{
			{ID: 11, Ability: 50, AtrophyFlag: false},
			{ID: 12, Ability: 60, AtrophyFlag: true},
		},
		Skills: []sim.PlayerAbilityState{
			{ID: 21, Ability: 40, AtrophyFlag: false},
		},
		Items: []sim.PlayerInventoryItemState{
			{ID: 101, ProtoID: 1001, Qty: 3, Hits: 250, Enchants: []byte(`{"glow":1}`), Slot: "hand"},
			{ID: 202, ProtoID: 1002, Qty: 500, Hits: 0, Enchants: []byte(`{}`), Slot: "pack"},
			{ID: 303, ProtoID: 1003, Qty: 1, Hits: 100, Enchants: []byte(`{"bane":true}`), Slot: "pack"},
		},
	}
}

// captureFixture builds a complete Normal-death sim capture over
// an in-memory engine: full durable state, mixed drops, PK kill
// metadata, and a post-death placement deliberately different
// from the death position. Deliberate ItemIDs (101+) never equal
// the opaque capture-order T5a keys.
func captureFixture(t *testing.T, killer sim.DeathKillerIdentity) sim.ImmediateDeathCapture {
	t.Helper()
	e := mustSimEngine(t)
	v := zeroHPDeathVitals(t)
	d := fullDeathDurableState()
	snap, err := e.AddPlayerEntityWithDurableState(7, world.Vec3{X: 5, Y: 0, Z: 6}, v, testDeathRuntimeInputs(t), d)
	if err != nil {
		t.Fatalf("AddPlayerEntityWithDurableState: %v", err)
	}
	_, base, err := e.PlayerBeginImmediateDeathCapture(snap.ID)
	if err != nil {
		t.Fatalf("PlayerBeginImmediateDeathCapture: %v", err)
	}
	plan, err := sim.PlanDeathDisposition(100, sim.DeathContext{}, killer.Kind == sim.DeathKillerCharacter)
	if err != nil {
		t.Fatalf("PlanDeathDisposition: %v", err)
	}
	inputs := []sim.DeathItemInput{
		{Key: 0, DropOnDeath: true, RoomAccepts: true},
		{Key: 1, DropOnDeath: false, RoomAccepts: true},
		{Key: 2, DropOnDeath: true, RoomAccepts: true},
	}
	drops, err := sim.PlanDeathDrops(plan, inputs)
	if err != nil {
		t.Fatalf("PlanDeathDrops: %v", err)
	}
	points, gain, err := sim.DecodeDeathAdvancementInputs(base.Durable.Advancement)
	if err != nil {
		t.Fatalf("DecodeDeathAdvancementInputs: %v", err)
	}
	adv, err := sim.PlanDeathAdvancement(sim.DeathNormal, points, gain)
	if err != nil {
		t.Fatalf("PlanDeathAdvancement: %v", err)
	}
	corpse, err := sim.PlanCorpse(777)
	if err != nil {
		t.Fatalf("PlanCorpse: %v", err)
	}
	pending, err := sim.PlanPendingDeath(plan, corpse)
	if err != nil {
		t.Fatalf("PlanPendingDeath: %v", err)
	}
	post, err := sim.PlanPostDeathVitals(sim.PostDeathVitalsInput{
		Vitals: testDeathVitals(t), Disposition: sim.DeathNormal,
	})
	if err != nil {
		t.Fatalf("PlanPostDeathVitals: %v", err)
	}
	got, err := sim.BuildImmediateDeathCapture(sim.ImmediateDeathBuildInput{
		Base: base, Disposition: plan, Corpse: corpse,
		Drops: drops, Advancement: adv, PostVitals: post,
		Pending: pending, Placement: world.Vec3{X: 100.5, Y: -2.25, Z: 100},
		Killer: killer,
	})
	if err != nil {
		t.Fatalf("BuildImmediateDeathCapture: %v", err)
	}
	return got
}

// TestMapImmediateDeathCaptureNormal proves the complete Normal
// capture maps to a complete DeathEntryRequest: zero revision
// placeholders, converted positions, JSON vitals round-trip,
// resolved advancement/flags/spells/skills, ground items at the
// death position, exact PK durations, pending metadata, and the
// character killer.
func TestMapImmediateDeathCaptureNormal(t *testing.T) {
	capture := captureFixture(t, sim.DeathKillerIdentity{Kind: sim.DeathKillerCharacter, CharacterID: 99})
	req, err := MapImmediateDeathCapture(capture)
	if err != nil {
		t.Fatalf("MapImmediateDeathCapture: %v", err)
	}

	if req.Character.ID != 7 {
		t.Fatalf("character ID = %d, want 7", req.Character.ID)
	}
	if req.Character.ExpectedRevision != 0 {
		t.Fatalf("character revision = %d, want 0 placeholder", req.Character.ExpectedRevision)
	}
	// Death position (5,0,6) vs post-death placement
	// (100.5,-2.25,100): the two Store positions differ correctly.
	if req.DeathPosX != 5000 || req.DeathPosY != 0 || req.DeathPosZ != 6000 {
		t.Fatalf("death pos = (%d,%d,%d), want (5000,0,6000)",
			req.DeathPosX, req.DeathPosY, req.DeathPosZ)
	}
	if req.Character.PosX != 100500 || req.Character.PosY != -2250 || req.Character.PosZ != 100000 {
		t.Fatalf("character pos = (%d,%d,%d), want (100500,-2250,100000)",
			req.Character.PosX, req.Character.PosY, req.Character.PosZ)
	}

	// PlayerVitals JSON round-trips exactly through sim.PlayerVitals.
	var vitals sim.PlayerVitals
	if err := json.Unmarshal(req.Character.Vitals, &vitals); err != nil {
		t.Fatalf("vitals unmarshal: %v", err)
	}
	if vitals != capture.Vitals {
		t.Fatalf("vitals = %+v, want %+v", vitals, capture.Vitals)
	}
	// Advancement is the already-resolved post-death object.
	if !bytes.Equal(req.Character.Advancement, capture.Durable.Advancement) {
		t.Fatalf("advancement = %s, want %s", req.Character.Advancement, capture.Durable.Advancement)
	}
	var adv map[string]json.RawMessage
	if err := json.Unmarshal(req.Character.Advancement, &adv); err != nil {
		t.Fatalf("advancement invalid: %v", err)
	}
	if string(adv["adv_points"]) != "0" || string(adv["gain_chance"]) != "-20" {
		t.Fatalf("advancement not resolved: %s", req.Character.Advancement)
	}
	if _, present := adv["adv_timer_due"]; present {
		t.Fatalf("adv_timer_due present: %s", req.Character.Advancement)
	}
	// Flags complete with the 0x70 gain bits cleared.
	if req.Character.Flags != capture.Durable.Flags || req.Character.Flags&0x70 != 0 {
		t.Fatalf("flags = %#x, want %#x", req.Character.Flags, capture.Durable.Flags)
	}
	// Complete spells and skills with atrophy reset.
	if len(req.Character.Spells) != 2 || len(req.Character.Skills) != 1 {
		t.Fatalf("spells/skills = %d/%d, want 2/1", len(req.Character.Spells), len(req.Character.Skills))
	}
	for _, sp := range req.Character.Spells {
		if !sp.AtrophyFlag {
			t.Fatalf("spell not reset: %+v", sp)
		}
	}
	if req.Character.Spells[0].SpellID != 11 || req.Character.Spells[0].Ability != 50 {
		t.Fatalf("spell content = %+v", req.Character.Spells[0])
	}
	if req.Character.Skills[0].SkillID != 21 || !req.Character.Skills[0].AtrophyFlag {
		t.Fatalf("skill content = %+v", req.Character.Skills[0])
	}
	if req.Character.Karma != 150 {
		t.Fatalf("karma = %d, want 150", req.Character.Karma)
	}

	// Affected items become kind=1 ground at exactly DeathPosXYZ
	// with all non-ground references nil and slot nil.
	if len(req.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(req.Items))
	}
	for _, it := range req.Items {
		if it.Snapshot.ExpectedRevision != 0 {
			t.Fatalf("item %d revision = %d, want 0", it.Snapshot.ID, it.Snapshot.ExpectedRevision)
		}
		loc := it.Snapshot.Location
		if loc.Kind != 1 {
			t.Fatalf("item %d kind = %d, want 1", it.Snapshot.ID, loc.Kind)
		}
		if loc.PosX == nil || loc.PosY == nil || loc.PosZ == nil ||
			*loc.PosX != req.DeathPosX || *loc.PosY != req.DeathPosY || *loc.PosZ != req.DeathPosZ {
			t.Fatalf("item %d ground pos != death pos: %+v", it.Snapshot.ID, loc)
		}
		if loc.CharacterID != nil || loc.CorpseID != nil || loc.ContainerItemID != nil || loc.VaultRegion != nil || loc.Slot != nil {
			t.Fatalf("item %d carries non-ground reference: %+v", it.Snapshot.ID, loc)
		}
		if it.PKProtectionDuration != 600000*time.Millisecond {
			t.Fatalf("item %d PK duration = %s", it.Snapshot.ID, it.PKProtectionDuration)
		}
	}
	if req.Items[0].Snapshot.ID != 101 || req.Items[0].Snapshot.Qty != 3 ||
		req.Items[0].Snapshot.Hits != 250 || string(req.Items[0].Snapshot.Enchants) != `{"glow":1}` {
		t.Fatalf("item content = %+v", req.Items[0].Snapshot)
	}
	if req.Items[1].Snapshot.ID != 303 {
		t.Fatalf("item order = [%d %d], want [101 303]", req.Items[0].Snapshot.ID, req.Items[1].Snapshot.ID)
	}

	// Underworld-bound pending entry metadata maps exactly.
	if req.EffectiveDeathCost != 100 || req.DeathTimeSeconds != 777 {
		t.Fatalf("cost/time = %d/%d, want 100/777", req.EffectiveDeathCost, req.DeathTimeSeconds)
	}
	if req.CorpseLifetime != time.Duration(sim.PlayerCorpseDecomposeMs)*time.Millisecond {
		t.Fatalf("corpse lifetime = %s", req.CorpseLifetime)
	}
	if req.NewbieHomeRespawn {
		t.Fatalf("newbie-home set for Underworld-bound death")
	}
	// Character killer maps exactly.
	if req.Killer == nil || req.Killer.Kind != store.DeathEntryKillerCharacter || req.Killer.CharacterID != 99 {
		t.Fatalf("killer = %+v", req.Killer)
	}
}

// TestMapImmediateDeathCaptureKillers proves mob-killer and
// nil/environmental killer mapping.
func TestMapImmediateDeathCaptureKillers(t *testing.T) {
	mob := captureFixture(t, sim.DeathKillerIdentity{Kind: sim.DeathKillerMob, MobID: 45})
	req, err := MapImmediateDeathCapture(mob)
	if err != nil {
		t.Fatalf("mob killer: %v", err)
	}
	if req.Killer == nil || req.Killer.Kind != store.DeathEntryKillerMob || req.Killer.MobID != 45 || req.Killer.CharacterID != 0 {
		t.Fatalf("mob killer = %+v", req.Killer)
	}

	none := captureFixture(t, sim.DeathKillerIdentity{Kind: sim.DeathKillerNone})
	req, err = MapImmediateDeathCapture(none)
	if err != nil {
		t.Fatalf("nil killer: %v", err)
	}
	if req.Killer != nil {
		t.Fatalf("nil killer = %+v, want nil", req.Killer)
	}
}

// TestMapImmediateDeathCaptureNewbieHome proves the
// direct-newbie-home flag and zero cost map exactly.
func TestMapImmediateDeathCaptureNewbieHome(t *testing.T) {
	e := mustSimEngine(t)
	v := zeroHPDeathVitals(t)
	snap, err := e.AddPlayerEntityWithDurableState(7, world.Vec3{X: 5, Y: 0, Z: 6}, v, testDeathRuntimeInputs(t), fullDeathDurableState())
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	_, base, err := e.PlayerBeginImmediateDeathCapture(snap.ID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	// Newbie-zone cheap death: direct newbie-home route, no
	// pending phase, zero cost.
	plan, err := sim.PlanDeathDisposition(100, sim.DeathContext{NewbieZoneDeath: true}, false)
	if err != nil {
		t.Fatalf("disposition: %v", err)
	}
	inputs := []sim.DeathItemInput{{Key: 0}, {Key: 1}, {Key: 2}}
	drops, err := sim.PlanDeathDrops(plan, inputs)
	if err != nil {
		t.Fatalf("drops: %v", err)
	}
	points, gain, err := sim.DecodeDeathAdvancementInputs(base.Durable.Advancement)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	adv, err := sim.PlanDeathAdvancement(sim.DeathCheap, points, gain)
	if err != nil {
		t.Fatalf("advancement: %v", err)
	}
	corpse, err := sim.PlanCorpse(12)
	if err != nil {
		t.Fatalf("corpse: %v", err)
	}
	pending, err := sim.PlanPendingDeath(plan, corpse)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	post, err := sim.PlanPostDeathVitals(sim.PostDeathVitalsInput{Vitals: testDeathVitals(t), Disposition: sim.DeathCheap})
	if err != nil {
		t.Fatalf("vitals: %v", err)
	}
	capture, err := sim.BuildImmediateDeathCapture(sim.ImmediateDeathBuildInput{
		Base: base, Disposition: plan, Corpse: corpse,
		Drops: drops, Advancement: adv, PostVitals: post,
		Pending: pending, Placement: world.Vec3{X: 1, Y: 0, Z: 1},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	req, err := MapImmediateDeathCapture(capture)
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	if !req.NewbieHomeRespawn || req.EffectiveDeathCost != 0 || len(req.Items) != 0 {
		t.Fatalf("newbie-home = %v/%d/%d items", req.NewbieHomeRespawn, req.EffectiveDeathCost, len(req.Items))
	}
}

// TestMetersToStoreMillimeters pins math.Round per-axis
// conversion including positive and negative half-rounding and
// the NaN/Inf/overflow rejections.
func TestMetersToStoreMillimeters(t *testing.T) {
	cases := []struct {
		meters float64
		want   int64
	}{
		{0, 0},
		{1, 1000},
		{5, 5000},
		{-2.25, -2250},
		{100.5, 100500},
		{0.0005, 1},   // positive half rounds away from zero
		{-0.0005, -1}, // negative half rounds away from zero
		{0.0025, 3},
		{-0.0025, -3},
		{1.2345, 1235},
		{-1.2345, -1235},
	}
	for _, c := range cases {
		if got, err := MetersToStoreMillimeters(c.meters); err != nil || got != c.want {
			t.Fatalf("(%v) = %d,%v; want %d,nil", c.meters, got, err, c.want)
		}
	}
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), 1e16, -1e16} {
		if _, err := MetersToStoreMillimeters(bad); !errors.Is(err, sim.ErrInvalidDeathInput) {
			t.Fatalf("(%v) err = %v, want ErrInvalidDeathInput", bad, err)
		}
	}
}

// TestMetersToStoreMillimetersInt64Boundary pins the immediate
// signed-int64 millimeter boundary with an exactly representable
// binary64 limit (2^63): float64(math.MaxInt64) == 2^63, so it
// must not serve as the positive validity boundary. +2^63 rejects,
// the representable value one ulp below in meters accepts, -2^63
// accepts as math.MinInt64, and anything below -2^63 rejects.
func TestMetersToStoreMillimetersInt64Boundary(t *testing.T) {
	limit := math.Ldexp(1, 63) // exactly 2^63

	// 1. Exact positive 2^63-millimeter result is rejected.
	posLimitMeters := limit / 1000
	if got := math.Round(posLimitMeters * 1000); got != limit {
		t.Fatalf("fixture: Round(%v*1000) = %v, want 2^63", posLimitMeters, got)
	}
	if _, err := MetersToStoreMillimeters(posLimitMeters); !errors.Is(err, sim.ErrInvalidDeathInput) {
		t.Fatalf("(+2^63 mm) err = %v, want ErrInvalidDeathInput", err)
	}

	// 2. A representable positive value immediately below 2^63
	// (one ulp below in meters) is accepted.
	justBelowMeters := math.Nextafter(posLimitMeters, 0)
	justBelowRounded := math.Round(justBelowMeters * 1000)
	if !(justBelowRounded < limit) {
		t.Fatalf("fixture: Round(%v*1000) = %v, want < 2^63", justBelowMeters, justBelowRounded)
	}
	got, err := MetersToStoreMillimeters(justBelowMeters)
	if err != nil {
		t.Fatalf("(just below +2^63 mm) unexpected err = %v", err)
	}
	if want := int64(justBelowRounded); got != want {
		t.Fatalf("(just below +2^63 mm) = %d, want %d", got, want)
	}

	// 3. Exact -2^63 millimeters is accepted as math.MinInt64.
	negLimitMeters := -limit / 1000
	if got := math.Round(negLimitMeters * 1000); got != -limit {
		t.Fatalf("fixture: Round(%v*1000) = %v, want -2^63", negLimitMeters, got)
	}
	got, err = MetersToStoreMillimeters(negLimitMeters)
	if err != nil {
		t.Fatalf("(-2^63 mm) unexpected err = %v", err)
	}
	if got != math.MinInt64 {
		t.Fatalf("(-2^63 mm) = %d, want math.MinInt64", got)
	}

	// 4. A representable value below -2^63 is rejected.
	belowNegMeters := math.Nextafter(negLimitMeters, math.Inf(-1))
	if got := math.Round(belowNegMeters * 1000); !(got < -limit) {
		t.Fatalf("fixture: Round(%v*1000) = %v, want < -2^63", belowNegMeters, got)
	}
	if _, err := MetersToStoreMillimeters(belowNegMeters); !errors.Is(err, sim.ErrInvalidDeathInput) {
		t.Fatalf("(below -2^63 mm) err = %v, want ErrInvalidDeathInput", err)
	}

	// 5. Half-rounding away from zero is unchanged.
	if got, err := MetersToStoreMillimeters(0.0005); err != nil || got != 1 {
		t.Fatalf("(0.0005) = %d,%v; want 1,nil", got, err)
	}
	if got, err := MetersToStoreMillimeters(-0.0005); err != nil || got != -1 {
		t.Fatalf("(-0.0005) = %d,%v; want -1,nil", got, err)
	}

	// 6. NaN / +Inf / -Inf remain rejected.
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := MetersToStoreMillimeters(bad); !errors.Is(err, sim.ErrInvalidDeathInput) {
			t.Fatalf("(%v) err = %v, want ErrInvalidDeathInput", bad, err)
		}
	}
}

// TestMapImmediateDeathCaptureBoundaryOverflow proves a
// boundary-overflow position (exact +2^63 millimeters) fails the
// mapper with the ZERO store.DeathEntryRequest.
func TestMapImmediateDeathCaptureBoundaryOverflow(t *testing.T) {
	limit := math.Ldexp(1, 63) // exactly 2^63
	overflowMeters := limit / 1000
	if got := math.Round(overflowMeters * 1000); got != limit {
		t.Fatalf("fixture: Round(%v*1000) = %v, want 2^63", overflowMeters, got)
	}
	for name, mutate := range map[string]func(*sim.ImmediateDeathCapture){
		"boundary overflow placement": func(c *sim.ImmediateDeathCapture) {
			c.Placement.X = overflowMeters
		},
		"boundary overflow death position": func(c *sim.ImmediateDeathCapture) {
			c.DeathPosition.Y = overflowMeters
		},
	} {
		t.Run(name, func(t *testing.T) {
			capture := captureFixture(t, sim.DeathKillerIdentity{Kind: sim.DeathKillerNone})
			mutate(&capture)
			req, err := MapImmediateDeathCapture(capture)
			if err == nil {
				t.Fatalf("expected error, got %+v", req)
			}
			if !errors.Is(err, sim.ErrInvalidDeathInput) {
				t.Fatalf("err = %v, want ErrInvalidDeathInput", err)
			}
			if !reflect.DeepEqual(req, store.DeathEntryRequest{}) {
				t.Fatalf("non-zero request on error: %+v", req)
			}
		})
	}
}

// TestMapImmediateDeathCaptureFailures proves conversion/range
// failures return the zero request.
func TestMapImmediateDeathCaptureFailures(t *testing.T) {
	good := captureFixture(t, sim.DeathKillerIdentity{Kind: sim.DeathKillerNone})
	bads := map[string]func(*sim.ImmediateDeathCapture){
		"bad character": func(c *sim.ImmediateDeathCapture) {
			c.Token.CharacterID = 0
		},
		"bad vitals": func(c *sim.ImmediateDeathCapture) {
			c.Vitals.HP = -1
		},
		"nan death position": func(c *sim.ImmediateDeathCapture) {
			c.DeathPosition.X = math.NaN()
		},
		"inf placement": func(c *sim.ImmediateDeathCapture) {
			c.Placement.Z = math.Inf(1)
		},
		"overflow placement": func(c *sim.ImmediateDeathCapture) {
			c.Placement.X = 1e16
		},
		"bad advancement": func(c *sim.ImmediateDeathCapture) {
			c.Durable.Advancement = []byte(`oops`)
		},
		"bad token id": func(c *sim.ImmediateDeathCapture) {
			c.AffectedItems[0].ID = 0
		},
		"bad enchants": func(c *sim.ImmediateDeathCapture) {
			c.AffectedItems[0].Enchants = []byte(`{bad`)
		},
		"negative PK duration": func(c *sim.ImmediateDeathCapture) {
			c.AffectedItems[0].PKProtectionDurationMs = -1
		},
		"bad killer": func(c *sim.ImmediateDeathCapture) {
			c.Killer = sim.DeathKillerIdentity{Kind: sim.DeathKillerCharacter, MobID: 1}
		},
	}
	for name, mutate := range bads {
		t.Run(name, func(t *testing.T) {
			c := good
			mutate(&c)
			req, err := MapImmediateDeathCapture(c)
			if err == nil {
				t.Fatalf("expected error, got %+v", req)
			}
			if !reflect.DeepEqual(req, store.DeathEntryRequest{}) {
				t.Fatalf("non-zero request on error: %+v", req)
			}
		})
	}
}

// TestMapImmediateDeathCaptureImmutableBoundary proves caller and
// capture mutation after mapping cannot mutate the Store
// request: the mapper owns its output bytes/slices.
func TestMapImmediateDeathCaptureImmutableBoundary(t *testing.T) {
	capture := captureFixture(t, sim.DeathKillerIdentity{Kind: sim.DeathKillerCharacter, CharacterID: 99})
	req, err := MapImmediateDeathCapture(capture)
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	capture.Vitals.HP = 424242
	capture.Durable.Advancement[1] = 'X'
	capture.Durable.Flags = 0
	capture.Durable.Spells[0].Ability = 1
	capture.AffectedItems[0].Qty = -7
	capture.AffectedItems[0].Enchants[1] = 'X'

	var vitals sim.PlayerVitals
	if err := json.Unmarshal(req.Character.Vitals, &vitals); err != nil {
		t.Fatalf("vitals unmarshal: %v", err)
	}
	if vitals.HP == 424242 {
		t.Fatalf("vitals aliased")
	}
	if req.Character.Flags == 0 {
		t.Fatalf("flags aliased")
	}
	if req.Character.Spells[0].Ability == 1 {
		t.Fatalf("spells aliased")
	}
	if req.Items[0].Snapshot.Qty == -7 || req.Items[0].Snapshot.Enchants[1] == 'X' {
		t.Fatalf("items aliased: %+v", req.Items[0].Snapshot)
	}
	if req.Character.Advancement[1] == 'X' {
		t.Fatalf("advancement aliased: %s", req.Character.Advancement)
	}
	if req.Killer != nil && req.Killer.CharacterID != 99 {
		t.Fatalf("killer aliased: %+v", req.Killer)
	}
}
