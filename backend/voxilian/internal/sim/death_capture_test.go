package sim

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/dlukt/voxilian/internal/world"
)

// M5-T5c3c2 complete immutable immediate-death capture tests
// (spec §9.5.1g): durable-state validation/freeze, additive
// full-state installation, atomic begin+capture, opaque keys,
// advancement JSON contract, Normal/Cheap mapping, drop/token
// composition, and builder rejection. Deterministic, no sleeps,
// no wall clock, no Store/persist/gateway/proto involvement.
// Deliberate ItemIDs (101+) never equal the opaque capture-order
// T5a keys (0-based indices).

// testFullDurableState is a valid complete durable shadow with
// mixed content: two spells (one pre-false atrophy), one skill
// (pre-false atrophy), and three inventory items whose durable
// IDs deliberately differ from any capture-order key.
func testFullDurableState() PlayerDurableState {
	return PlayerDurableState{
		Karma:       150,
		Advancement: []byte(`{"adv_points":7,"gain_chance":-40,"adv_timer_due":12345,"school_casts":{"1":3},"custom":"keep"}`),
		Flags:       0x1274, // 0x70 gain bits set plus unrelated bits
		Spells: []PlayerAbilityState{
			{ID: 11, Ability: 50, AtrophyFlag: false},
			{ID: 12, Ability: 60, AtrophyFlag: true},
		},
		Skills: []PlayerAbilityState{
			{ID: 21, Ability: 40, AtrophyFlag: false},
		},
		Items: []PlayerInventoryItemState{
			{ID: 101, ProtoID: 1001, Qty: 3, Hits: 250, Enchants: []byte(`{"glow":1}`), Slot: "hand"},
			{ID: 202, ProtoID: 1002, Qty: 500, Hits: 0, Enchants: []byte(`{}`), Slot: "pack"},
			{ID: 303, ProtoID: 1003, Qty: 1, Hits: 100, Enchants: []byte(`{"bane":true}`), Slot: "pack"},
		},
	}
}

// addFullStatePlayer installs a zero-HP full-state player for
// death-capture tests.
func addFullStatePlayer(t *testing.T, e *Engine, charID CharacterID, pos world.Vec3, d PlayerDurableState) EntityID {
	t.Helper()
	snap, err := e.AddPlayerEntityWithDurableState(charID, pos, zeroHPVitals(t), testRuntimeInputs(), d)
	if err != nil {
		t.Fatalf("AddPlayerEntityWithDurableState: %v", err)
	}
	return snap.ID
}

// beginFullCapture runs the atomic begin+capture helper,
// fatal on error.
func beginFullCapture(t *testing.T, e *Engine, id EntityID) (DeathAttemptToken, ImmediateDeathBaseCapture) {
	t.Helper()
	tok, base, err := e.PlayerBeginImmediateDeathCapture(id)
	if err != nil {
		t.Fatalf("PlayerBeginImmediateDeathCapture(%d): %v", uint64(id), err)
	}
	return tok, base
}

// normalDeathInputs resolves every T5a/build input for a Normal
// death over base: drop decisions per index, default cost, corpse
// time, and ordinary (non-frenzy, non-angel) post-death vitals.
func normalDeathInputs(t *testing.T, base ImmediateDeathBaseCapture, dropByIndex map[int]bool, killerIsPlayer bool) ImmediateDeathBuildInput {
	t.Helper()
	plan, err := PlanDeathDisposition(100, DeathContext{}, killerIsPlayer)
	if err != nil {
		t.Fatalf("PlanDeathDisposition: %v", err)
	}
	inputs := make([]DeathItemInput, len(base.ItemKeys))
	for i, k := range base.ItemKeys {
		inputs[i] = DeathItemInput{Key: k, DropOnDeath: dropByIndex[i], RoomAccepts: true}
	}
	drops, err := PlanDeathDrops(plan, inputs)
	if err != nil {
		t.Fatalf("PlanDeathDrops: %v", err)
	}
	points, gain, err := DecodeDeathAdvancementInputs(base.Durable.Advancement)
	if err != nil {
		t.Fatalf("DecodeDeathAdvancementInputs: %v", err)
	}
	adv, err := PlanDeathAdvancement(DeathNormal, points, gain)
	if err != nil {
		t.Fatalf("PlanDeathAdvancement: %v", err)
	}
	corpse, err := PlanCorpse(777)
	if err != nil {
		t.Fatalf("PlanCorpse: %v", err)
	}
	pending, err := PlanPendingDeath(plan, corpse)
	if err != nil {
		t.Fatalf("PlanPendingDeath: %v", err)
	}
	post, err := PlanPostDeathVitals(PostDeathVitalsInput{
		Vitals: testVitals(), Disposition: DeathNormal,
	})
	if err != nil {
		t.Fatalf("PlanPostDeathVitals: %v", err)
	}
	return ImmediateDeathBuildInput{
		Base: base, Disposition: plan, Corpse: corpse,
		Drops: drops, Advancement: adv, PostVitals: post,
		Pending: pending, Placement: world.Vec3{X: 100, Y: 0, Z: 100},
	}
}

// cheapTokenInputs resolves every T5a/build input for a Token
// cheap death over base with the token at the given item ID.
func cheapTokenInputs(t *testing.T, base ImmediateDeathBaseCapture, tokenItemID int64) ImmediateDeathBuildInput {
	t.Helper()
	plan, err := PlanDeathDisposition(100, DeathContext{CarriesToken: true}, true)
	if err != nil {
		t.Fatalf("PlanDeathDisposition: %v", err)
	}
	if plan.Disposition != DeathCheap || !plan.TokenDeath {
		t.Fatalf("token disposition = %+v, want cheap+token", plan)
	}
	inputs := make([]DeathItemInput, len(base.ItemKeys))
	for i, k := range base.ItemKeys {
		inputs[i] = DeathItemInput{Key: k, RoomAccepts: true}
	}
	drops, err := PlanDeathDrops(plan, inputs)
	if err != nil {
		t.Fatalf("PlanDeathDrops: %v", err)
	}
	points, gain, err := DecodeDeathAdvancementInputs(base.Durable.Advancement)
	if err != nil {
		t.Fatalf("DecodeDeathAdvancementInputs: %v", err)
	}
	adv, err := PlanDeathAdvancement(DeathCheap, points, gain)
	if err != nil {
		t.Fatalf("PlanDeathAdvancement: %v", err)
	}
	corpse, err := PlanCorpse(777)
	if err != nil {
		t.Fatalf("PlanCorpse: %v", err)
	}
	pending, err := PlanPendingDeath(plan, corpse)
	if err != nil {
		t.Fatalf("PlanPendingDeath: %v", err)
	}
	post, err := PlanPostDeathVitals(PostDeathVitalsInput{
		Vitals: testVitals(), Disposition: DeathCheap,
	})
	if err != nil {
		t.Fatalf("PlanPostDeathVitals: %v", err)
	}
	return ImmediateDeathBuildInput{
		Base: base, Disposition: plan, Corpse: corpse,
		Drops: drops, Advancement: adv, PostVitals: post,
		Pending: pending, Placement: world.Vec3{X: 100, Y: 0, Z: 100},
		TokenItemID: tokenItemID,
	}
}

// TestPlayerDurableStateValidation proves complete state
// validates before entity allocation and invalid durable state
// consumes no EntityID.
func TestPlayerDurableStateValidation(t *testing.T) {
	cases := map[string]func(PlayerDurableState) PlayerDurableState{
		"bad advancement non-object": func(d PlayerDurableState) PlayerDurableState {
			d.Advancement = []byte(`[1,2]`)
			return d
		},
		"bad advancement malformed": func(d PlayerDurableState) PlayerDurableState {
			d.Advancement = []byte(`{oops`)
			return d
		},
		"bad advancement empty": func(d PlayerDurableState) PlayerDurableState {
			d.Advancement = nil
			return d
		},
		"bad advancement null": func(d PlayerDurableState) PlayerDurableState {
			d.Advancement = []byte(`null`)
			return d
		},
		"spell id zero": func(d PlayerDurableState) PlayerDurableState {
			d.Spells[0].ID = 0
			return d
		},
		"spell id overflow": func(d PlayerDurableState) PlayerDurableState {
			d.Spells[0].ID = 65536
			return d
		},
		"duplicate spell id": func(d PlayerDurableState) PlayerDurableState {
			d.Spells[1].ID = d.Spells[0].ID
			return d
		},
		"spell ability zero": func(d PlayerDurableState) PlayerDurableState {
			d.Spells[0].Ability = 0
			return d
		},
		"spell ability hundred": func(d PlayerDurableState) PlayerDurableState {
			d.Spells[0].Ability = 100
			return d
		},
		"duplicate skill id": func(d PlayerDurableState) PlayerDurableState {
			d.Skills = append(d.Skills, PlayerAbilityState{ID: 21, Ability: 10})
			return d
		},
		"skill ability zero": func(d PlayerDurableState) PlayerDurableState {
			d.Skills[0].Ability = 0
			return d
		},
		"item id zero": func(d PlayerDurableState) PlayerDurableState {
			d.Items[0].ID = 0
			return d
		},
		"duplicate item id": func(d PlayerDurableState) PlayerDurableState {
			d.Items[1].ID = d.Items[0].ID
			return d
		},
		"proto id zero": func(d PlayerDurableState) PlayerDurableState {
			d.Items[0].ProtoID = 0
			return d
		},
		"proto id overflow": func(d PlayerDurableState) PlayerDurableState {
			d.Items[0].ProtoID = 70000
			return d
		},
		"enchants malformed": func(d PlayerDurableState) PlayerDurableState {
			d.Items[0].Enchants = []byte(`{bad`)
			return d
		},
		"enchants empty": func(d PlayerDurableState) PlayerDurableState {
			d.Items[0].Enchants = nil
			return d
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := newPlayerEngine(t, nil)
			bad := mutate(testFullDurableState())
			if _, err := e.AddPlayerEntityWithDurableState(testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1}, testVitals(), testRuntimeInputs(), bad); !errors.Is(err, ErrInvalidDeathInput) {
				t.Fatalf("err = %v, want ErrInvalidDeathInput", err)
			}
			if got := e.EntityCount(); got != 0 {
				t.Fatalf("entities = %d after invalid add, want 0", got)
			}
		})
	}

	// Repeated invalid adds consume no EntityID: the next valid
	// add issues ID 1.
	e := newPlayerEngine(t, nil)
	for i := 0; i < 3; i++ {
		bad := testFullDurableState()
		bad.Advancement = []byte(`nope`)
		if _, err := e.AddPlayerEntityWithDurableState(testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1}, testVitals(), testRuntimeInputs(), bad); err == nil {
			t.Fatalf("invalid add %d succeeded", i)
		}
	}
	snap, err := e.AddPlayerEntityWithDurableState(testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1}, testVitals(), testRuntimeInputs(), testFullDurableState())
	if err != nil {
		t.Fatalf("valid add: %v", err)
	}
	if snap.ID != EntityID(1) {
		t.Fatalf("valid add ID = %d, want 1 (no ID consumed by failures)", uint64(snap.ID))
	}

	// Cross-namespace ID reuse is legal: the same numeric ID may
	// exist once as a spell and once as a skill.
	e2 := newPlayerEngine(t, nil)
	d := testFullDurableState()
	d.Skills[0].ID = d.Spells[0].ID
	if _, err := e2.AddPlayerEntityWithDurableState(testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1}, testVitals(), testRuntimeInputs(), d); err != nil {
		t.Fatalf("cross-namespace ID reuse rejected: %v", err)
	}
}

// TestPlayerDurableStateDeepFrozen proves every durable state
// accepted into the entity is deep-frozen: later caller mutation
// cannot reach live sim state, and inspection returns deep
// copies.
func TestPlayerDurableStateDeepFrozen(t *testing.T) {
	e := newPlayerEngine(t, nil)
	d := testFullDurableState()
	id := addFullStatePlayer(t, e, testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1}, d)

	// Mutate every caller-owned buffer after installation.
	d.Advancement[1] = 'X'
	d.Spells[0].Ability = 1
	d.Spells[0].AtrophyFlag = true
	d.Skills[0].Ability = 99
	d.Items[0].Qty = 999
	d.Items[0].Enchants[1] = 'X'
	d.Items = append(d.Items, PlayerInventoryItemState{ID: 999})

	got, ok, err := e.PlayerDurableStateOf(id)
	if err != nil || !ok {
		t.Fatalf("inspect = %v,%v,%v; want live,true,nil", got, ok, err)
	}
	want := testFullDurableState()
	if !bytes.Equal(got.Advancement, want.Advancement) {
		t.Fatalf("advancement aliased: %s", got.Advancement)
	}
	if got.Spells[0].Ability != 50 || got.Spells[0].AtrophyFlag {
		t.Fatalf("spells aliased: %+v", got.Spells[0])
	}
	if got.Skills[0].Ability != 40 {
		t.Fatalf("skills aliased: %+v", got.Skills[0])
	}
	if got.Items[0].Qty != 3 || len(got.Items) != 3 {
		t.Fatalf("items aliased: %+v", got.Items)
	}
	if !bytes.Equal(got.Items[0].Enchants, []byte(`{"glow":1}`)) {
		t.Fatalf("enchants aliased: %s", got.Items[0].Enchants)
	}

	// Mutate the inspection result: live state must not move.
	got.Advancement[1] = 'Z'
	got.Spells[1].Ability = 2
	got.Items[1].Hits = 1111
	got.Items[1].Enchants[1] = 'Z'
	got2, ok, err := e.PlayerDurableStateOf(id)
	if err != nil || !ok {
		t.Fatalf("re-inspect = %v,%v,%v", got2, ok, err)
	}
	if !bytes.Equal(got2.Advancement, want.Advancement) || got2.Spells[1].Ability != 60 || got2.Items[1].Hits != 0 {
		t.Fatalf("inspection aliases live state: %+v", got2)
	}
	if !bytes.Equal(got2.Items[1].Enchants, []byte(`{}`)) {
		t.Fatalf("inspection enchants alias live state: %s", got2.Items[1].Enchants)
	}
}

// TestPlayerDurableStateHandoffAndRemoval proves the same entity
// object carries the shadow across cell handoff and entity
// removal discards it normally.
func TestPlayerDurableStateHandoffAndRemoval(t *testing.T) {
	e := newPlayerEngine(t, nil)
	id := addFullStatePlayer(t, e, testCharacterID(), world.Vec3{X: 31.9, Y: 0, Z: 16}, testFullDurableState())
	submitMove(t, e, id, 1, MoveDirForward, 0, 1024)
	e.Step()
	if got, err := e.Entity(id); err != nil || got.Cell != (world.CellCoord{X: 1, Z: 0}) {
		t.Fatalf("handoff = %+v,%v; want cell {1 0}", got, err)
	}
	got, ok, err := e.PlayerDurableStateOf(id)
	if err != nil || !ok {
		t.Fatalf("post-handoff inspect = %v,%v,%v", got, ok, err)
	}
	if len(got.Items) != 3 || got.Items[0].ID != 101 || got.Karma != 150 {
		t.Fatalf("handoff lost durable state: %+v", got)
	}

	if err := e.RemoveEntity(id); err != nil {
		t.Fatalf("RemoveEntity: %v", err)
	}
	if _, _, err := e.PlayerDurableStateOf(id); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("post-removal inspect err = %v, want ErrEntityNotFound", err)
	}
	if _, ok, err := e.PlayerLifeStateOf(id); err == nil || ok {
		t.Fatalf("post-removal life = %v,%v", ok, err)
	}
}

// TestPlayerBeginCaptureMissingShadow proves a legacy minimal
// player fails complete death capture closed with
// ErrPlayerDurableStateMissing and zero lifecycle mutation.
func TestPlayerBeginCaptureMissingShadow(t *testing.T) {
	e := newPlayerEngine(t, nil)
	snap, err := e.AddPlayerEntity(testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1}, zeroHPVitals(t), testRuntimeInputs())
	if err != nil {
		t.Fatalf("AddPlayerEntity: %v", err)
	}
	id := snap.ID
	if _, ok, err := e.PlayerDurableStateOf(id); err != nil || ok {
		t.Fatalf("legacy inspect = %v,%v; want zero,false,nil", ok, err)
	}
	// Arm pending movement so a quiesce would be observable.
	submitMove(t, e, id, 9, MoveDirForward, 0, 100)

	if _, _, err := e.PlayerBeginImmediateDeathCapture(id); !errors.Is(err, ErrPlayerDurableStateMissing) {
		t.Fatalf("err = %v, want ErrPlayerDurableStateMissing", err)
	}
	if st, ok := lifeOf(t, e, id); !ok || st != PlayerLifeAlive {
		t.Fatalf("life = %d,%v; want Alive,true", uint8(st), ok)
	}
	if epoch := entOf(t, e, id).deathEpoch; epoch != 0 {
		t.Fatalf("death epoch = %d, want 0", epoch)
	}
	if ent := entOf(t, e, id); !ent.hasPending {
		t.Fatalf("pending move cleared without quiesce")
	}
	// The legacy player still begins through the plain c3c1 path.
	if _, err := e.PlayerBeginDeathPersistence(id); err != nil {
		t.Fatalf("plain begin after failed capture: %v", err)
	}
}

// TestPlayerBeginImmediateDeathCaptureSuccess proves the atomic
// owner-local begin+capture: exact token, exact live values, the
// DeathPersisting transition, and an immutable base.
func TestPlayerBeginImmediateDeathCaptureSuccess(t *testing.T) {
	e := newPlayerEngine(t, nil)
	pos := world.Vec3{X: 5, Y: 0, Z: 6}
	id := addFullStatePlayer(t, e, testCharacterID(), pos, testFullDurableState())

	tok, base, err := e.PlayerBeginImmediateDeathCapture(id)
	if err != nil {
		t.Fatalf("PlayerBeginImmediateDeathCapture: %v", err)
	}
	if tok.EntityID != id || tok.CharacterID != testCharacterID() || tok.Epoch != 1 {
		t.Fatalf("token = %+v, want {%d %d 1}", tok, uint64(id), int64(testCharacterID()))
	}
	if base.Token != tok {
		t.Fatalf("base token = %+v, want exact %+v", base.Token, tok)
	}
	if base.DeathPosition != pos {
		t.Fatalf("death position = %+v, want %+v", base.DeathPosition, pos)
	}
	if want := zeroHPVitals(t); base.Vitals != want {
		t.Fatalf("vitals = %+v, want %+v", base.Vitals, want)
	}
	if st, ok := lifeOf(t, e, id); !ok || st != PlayerLifeDeathPersisting {
		t.Fatalf("life = %d,%v; want DeathPersisting,true", uint8(st), ok)
	}
	// Opaque keys are deterministic capture-order keys, unrelated
	// to the deliberate durable ItemIDs.
	if len(base.ItemKeys) != 3 || base.ItemKeys[0] != 0 || base.ItemKeys[1] != 1 || base.ItemKeys[2] != 2 {
		t.Fatalf("item keys = %v, want [0 1 2]", base.ItemKeys)
	}
	for i, it := range base.Durable.Items {
		if it.ID == int64(base.ItemKeys[i]) {
			t.Fatalf("item %d carries durable-ID semantics in key %d", it.ID, base.ItemKeys[i])
		}
	}
	if len(base.Durable.Items) != 3 || base.Durable.Items[0].ID != 101 || base.Durable.Karma != 150 {
		t.Fatalf("base durable incomplete: %+v", base.Durable)
	}
	// Mutating the returned base cannot reach live state.
	base.Durable.Items[0].Qty = -1
	base.Durable.Spells[0].Ability = -1
	base.Durable.Advancement[0] = 'X'
	live, ok, err := e.PlayerDurableStateOf(id)
	if err != nil || !ok {
		t.Fatalf("live inspect = %v,%v,%v", live, ok, err)
	}
	if live.Items[0].Qty != 3 || live.Spells[0].Ability != 50 || live.Advancement[0] != '{' {
		t.Fatalf("base aliases live state: %+v", live)
	}
	// A second begin fails closed: already DeathPersisting.
	if _, _, err := e.PlayerBeginImmediateDeathCapture(id); !errors.Is(err, ErrPlayerNotAlive) {
		t.Fatalf("second begin err = %v, want ErrPlayerNotAlive", err)
	}
}

// TestDecodeDeathAdvancementInputs pins the frozen advancement
// JSON contract, including the "{}" creation-compatibility rule.
func TestDecodeDeathAdvancementInputs(t *testing.T) {
	points, gain, err := DecodeDeathAdvancementInputs([]byte(`{}`))
	if err != nil || points != 0 || gain != 0 {
		t.Fatalf("({}) = %d,%d,%v; want 0,0,nil", points, gain, err)
	}
	points, gain, err = DecodeDeathAdvancementInputs([]byte(`{"adv_points":9,"gain_chance":-33,"adv_timer_due":5,"school_casts":{"2":4},"x":[1]}`))
	if err != nil || points != 9 || gain != -33 {
		t.Fatalf("full = %d,%d,%v; want 9,-33,nil", points, gain, err)
	}
	for _, bad := range []string{``, `null`, `[1]`, `7`, `"s"`, `{oops`, `{"adv_points":"9"}`, `{"adv_points":1.5}`, `{"gain_chance":true}`, `{"gain_chance":[1]}`} {
		if _, _, err := DecodeDeathAdvancementInputs([]byte(bad)); !errors.Is(err, ErrInvalidDeathInput) {
			t.Fatalf("(%q) err = %v, want ErrInvalidDeathInput", bad, err)
		}
	}
}

// TestBuildNormalDeathCapture proves the Normal-death mapping:
// planned advancement, deleted timer, preserved unknowns,
// cleared 0x70 flags with unrelated bits kept, full atrophy
// reset, ordered drops with complete content and T5a PK metadata.
func TestBuildNormalDeathCapture(t *testing.T) {
	e := newPlayerEngine(t, nil)
	id := addFullStatePlayer(t, e, testCharacterID(), world.Vec3{X: 5, Y: 0, Z: 6}, testFullDurableState())
	_, base := beginFullCapture(t, e, id)

	in := normalDeathInputs(t, base, map[int]bool{0: true, 1: false, 2: true}, true)
	got, err := BuildImmediateDeathCapture(in)
	if err != nil {
		t.Fatalf("BuildImmediateDeathCapture: %v", err)
	}
	if got.Token != base.Token || got.DeathPosition != base.DeathPosition || got.Placement != in.Placement {
		t.Fatalf("capture identity wrong: %+v", got)
	}

	var adv map[string]json.RawMessage
	if err := json.Unmarshal(got.Durable.Advancement, &adv); err != nil {
		t.Fatalf("result advancement invalid: %v", err)
	}
	if string(adv[advPointsKey]) != "0" {
		t.Fatalf("adv_points = %s, want 0", adv[advPointsKey])
	}
	if string(adv[advGainKey]) != "-20" {
		t.Fatalf("gain_chance = %s, want -20", adv[advGainKey])
	}
	if _, present := adv[advTimerDueKey]; present {
		t.Fatalf("adv_timer_due present after Normal death: %s", got.Durable.Advancement)
	}
	if string(adv["school_casts"]) != `{"1":3}` || string(adv["custom"]) != `"keep"` {
		t.Fatalf("unknown advancement fields lost: %s", got.Durable.Advancement)
	}
	if got.Durable.Flags&0x70 != 0 {
		t.Fatalf("gain flags not cleared: %#x", got.Durable.Flags)
	}
	if got.Durable.Flags&^0x70 != 0x1274&^0x70 {
		t.Fatalf("unrelated flags not preserved: %#x", got.Durable.Flags)
	}
	for _, sp := range got.Durable.Spells {
		if !sp.AtrophyFlag {
			t.Fatalf("spell %d atrophy not reset: %+v", sp.ID, sp)
		}
	}
	for _, sk := range got.Durable.Skills {
		if !sk.AtrophyFlag {
			t.Fatalf("skill %d atrophy not reset: %+v", sk.ID, sk)
		}
	}
	if got.Durable.Spells[0].Ability != 50 || got.Durable.Skills[0].Ability != 40 || got.Durable.Karma != 150 {
		t.Fatalf("abilities/karma disturbed: %+v", got.Durable)
	}

	if len(got.AffectedItems) != 2 {
		t.Fatalf("affected = %d items, want 2", len(got.AffectedItems))
	}
	first, second := got.AffectedItems[0], got.AffectedItems[1]
	if first.ID != 101 || second.ID != 303 {
		t.Fatalf("affected order = [%d %d], want [101 303]", first.ID, second.ID)
	}
	if first.Qty != 3 || first.Hits != 250 || !bytes.Equal(first.Enchants, []byte(`{"glow":1}`)) {
		t.Fatalf("affected content incomplete: %+v", first)
	}
	if second.Qty != 1 || second.Hits != 100 || !bytes.Equal(second.Enchants, []byte(`{"bane":true}`)) {
		t.Fatalf("affected content incomplete: %+v", second)
	}
	if first.PKProtectionDurationMs != PKProtectionDurationMs || second.PKProtectionDurationMs != PKProtectionDurationMs {
		t.Fatalf("PK metadata not exactly T5a: %d/%d", first.PKProtectionDurationMs, second.PKProtectionDurationMs)
	}
	if len(got.Durable.Items) != 1 || got.Durable.Items[0].ID != 202 {
		t.Fatalf("resulting inventory = %+v, want [202]", got.Durable.Items)
	}
}

// TestBuildNormalDeathKeptOrder proves kept inventory relative
// order is unchanged with interleaved drops.
func TestBuildNormalDeathKeptOrder(t *testing.T) {
	e := newPlayerEngine(t, nil)
	d := testFullDurableState()
	d.Items = append(d.Items, PlayerInventoryItemState{ID: 404, ProtoID: 1004, Qty: 9, Hits: 9, Enchants: []byte(`{}`), Slot: "pack"})
	id := addFullStatePlayer(t, e, testCharacterID(), world.Vec3{X: 5, Y: 0, Z: 6}, d)
	_, base := beginFullCapture(t, e, id)

	in := normalDeathInputs(t, base, map[int]bool{0: true, 1: false, 2: true, 3: false}, false)
	got, err := BuildImmediateDeathCapture(in)
	if err != nil {
		t.Fatalf("BuildImmediateDeathCapture: %v", err)
	}
	if len(got.AffectedItems) != 2 || got.AffectedItems[0].ID != 101 || got.AffectedItems[1].ID != 303 {
		t.Fatalf("affected = %+v, want [101 303]", got.AffectedItems)
	}
	// Killer was not a player: no PK protection anywhere.
	for _, a := range got.AffectedItems {
		if a.PKProtectionDurationMs != 0 {
			t.Fatalf("non-PK kill gained protection: %+v", a)
		}
	}
	if len(got.Durable.Items) != 2 || got.Durable.Items[0].ID != 202 || got.Durable.Items[1].ID != 404 {
		t.Fatalf("kept order = %+v, want [202 404]", got.Durable.Items)
	}
}

// TestBuildCheapDeathPreservation proves cheap real death
// preserves advancement bytes, flags, and atrophy values while
// still relocating the resolved Token.
func TestBuildCheapDeathPreservation(t *testing.T) {
	e := newPlayerEngine(t, nil)
	id := addFullStatePlayer(t, e, testCharacterID(), world.Vec3{X: 5, Y: 0, Z: 6}, testFullDurableState())
	_, base := beginFullCapture(t, e, id)

	in := cheapTokenInputs(t, base, 202)
	got, err := BuildImmediateDeathCapture(in)
	if err != nil {
		t.Fatalf("BuildImmediateDeathCapture: %v", err)
	}
	if !bytes.Equal(got.Durable.Advancement, base.Durable.Advancement) {
		t.Fatalf("cheap advancement re-marshalled:\n%s\n%s", got.Durable.Advancement, base.Durable.Advancement)
	}
	if got.Durable.Flags != base.Durable.Flags {
		t.Fatalf("cheap flags changed: %#x vs %#x", got.Durable.Flags, base.Durable.Flags)
	}
	if got.Durable.Spells[0].AtrophyFlag || !got.Durable.Spells[1].AtrophyFlag || got.Durable.Skills[0].AtrophyFlag {
		t.Fatalf("cheap atrophy changed: %+v %+v", got.Durable.Spells, got.Durable.Skills)
	}
	if len(got.AffectedItems) != 1 {
		t.Fatalf("affected = %d items, want exactly the token", len(got.AffectedItems))
	}
	tok := got.AffectedItems[0]
	if tok.ID != 202 || tok.Qty != 500 || tok.Hits != 0 || !bytes.Equal(tok.Enchants, []byte(`{}`)) {
		t.Fatalf("token content incomplete: %+v", tok)
	}
	if tok.PKProtectionDurationMs != 0 {
		t.Fatalf("token gained PK protection: %+v", tok)
	}
	if len(got.Durable.Items) != 2 || got.Durable.Items[0].ID != 101 || got.Durable.Items[1].ID != 303 {
		t.Fatalf("resulting inventory = %+v, want [101 303]", got.Durable.Items)
	}
}

// TestResetAtrophyFlagsCoversSpellsAndSkills is the explicit
// source-fidelity regression (spec §9.5.1g, player.kod
// ResetAtrophyFlags negates BOTH plSpells and plSkills): with a
// pre-false spell AND a pre-false skill, a Normal death must set
// BOTH to AtrophyFlag=true. The old spell-only interpretation
// cannot be reintroduced silently.
func TestResetAtrophyFlagsCoversSpellsAndSkills(t *testing.T) {
	e := newPlayerEngine(t, nil)
	d := testFullDurableState()
	if d.Spells[0].AtrophyFlag || d.Skills[0].AtrophyFlag {
		t.Fatalf("setup: want pre-false spell and skill")
	}
	id := addFullStatePlayer(t, e, testCharacterID(), world.Vec3{X: 5, Y: 0, Z: 6}, d)
	_, base := beginFullCapture(t, e, id)

	in := normalDeathInputs(t, base, map[int]bool{}, false)
	got, err := BuildImmediateDeathCapture(in)
	if err != nil {
		t.Fatalf("BuildImmediateDeathCapture: %v", err)
	}
	if !got.Durable.Spells[0].AtrophyFlag {
		t.Fatalf("pre-false spell not reset: %+v", got.Durable.Spells[0])
	}
	if !got.Durable.Skills[0].AtrophyFlag {
		t.Fatalf("pre-false skill not reset: %+v", got.Durable.Skills[0])
	}
}

// TestBuildDeathCaptureRejections proves bad composition is
// rejected before persistence mapping.
func TestBuildDeathCaptureRejections(t *testing.T) {
	setup := func(t *testing.T) (ImmediateDeathBuildInput, ImmediateDeathBaseCapture) {
		t.Helper()
		e := newPlayerEngine(t, nil)
		id := addFullStatePlayer(t, e, testCharacterID(), world.Vec3{X: 5, Y: 0, Z: 6}, testFullDurableState())
		_, base := beginFullCapture(t, e, id)
		return normalDeathInputs(t, base, map[int]bool{0: true}, true), base
	}

	t.Run("missing key", func(t *testing.T) {
		in, _ := setup(t)
		in.Drops.Items = in.Drops.Items[:2]
		if _, err := BuildImmediateDeathCapture(in); !errors.Is(err, ErrInvalidDeathInput) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("extra key", func(t *testing.T) {
		in, _ := setup(t)
		in.Drops.Items = append(in.Drops.Items, DeathDropItem{Key: 99})
		if _, err := BuildImmediateDeathCapture(in); !errors.Is(err, ErrInvalidDeathInput) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("reordered keys", func(t *testing.T) {
		in, _ := setup(t)
		in.Drops.Items[0], in.Drops.Items[2] = in.Drops.Items[2], in.Drops.Items[0]
		if _, err := BuildImmediateDeathCapture(in); !errors.Is(err, ErrInvalidDeathInput) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("duplicate keys", func(t *testing.T) {
		in, _ := setup(t)
		in.Drops.Items[2].Key = 0
		if _, err := BuildImmediateDeathCapture(in); !errors.Is(err, ErrInvalidDeathInput) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("token absent", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		id := addFullStatePlayer(t, e, testCharacterID(), world.Vec3{X: 5, Y: 0, Z: 6}, testFullDurableState())
		_, base := beginFullCapture(t, e, id)
		in := cheapTokenInputs(t, base, 999)
		if _, err := BuildImmediateDeathCapture(in); !errors.Is(err, ErrInvalidDeathInput) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("token zero with token death", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		id := addFullStatePlayer(t, e, testCharacterID(), world.Vec3{X: 5, Y: 0, Z: 6}, testFullDurableState())
		_, base := beginFullCapture(t, e, id)
		in := cheapTokenInputs(t, base, 0)
		if _, err := BuildImmediateDeathCapture(in); !errors.Is(err, ErrInvalidDeathInput) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("token id without token death", func(t *testing.T) {
		in, _ := setup(t)
		in.TokenItemID = 202
		if _, err := BuildImmediateDeathCapture(in); !errors.Is(err, ErrInvalidDeathInput) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("token duplicated in inventory", func(t *testing.T) {
		in, base := setup(t)
		plan, err := PlanDeathDisposition(100, DeathContext{CarriesToken: true}, false)
		if err != nil {
			t.Fatalf("PlanDeathDisposition: %v", err)
		}
		in.Disposition = plan
		dup := base
		dup.Durable.Items = append(append([]PlayerInventoryItemState(nil), base.Durable.Items...), base.Durable.Items[1])
		dup.ItemKeys = []int{0, 1, 2, 3}
		inputs := make([]DeathItemInput, 4)
		for i, k := range dup.ItemKeys {
			inputs[i] = DeathItemInput{Key: k}
		}
		drops, err := PlanDeathDrops(plan, inputs)
		if err != nil {
			t.Fatalf("PlanDeathDrops: %v", err)
		}
		in.Base = dup
		in.Drops = drops
		points, gain, err := DecodeDeathAdvancementInputs(dup.Durable.Advancement)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		in.Advancement, err = PlanDeathAdvancement(DeathCheap, points, gain)
		if err != nil {
			t.Fatalf("advancement: %v", err)
		}
		in.Pending, err = PlanPendingDeath(plan, in.Corpse)
		if err != nil {
			t.Fatalf("pending: %v", err)
		}
		post, err := PlanPostDeathVitals(PostDeathVitalsInput{Vitals: testVitals(), Disposition: DeathCheap})
		if err != nil {
			t.Fatalf("vitals: %v", err)
		}
		in.PostVitals = post
		in.TokenItemID = 202
		if _, err := BuildImmediateDeathCapture(in); !errors.Is(err, ErrInvalidDeathInput) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("avoided disposition", func(t *testing.T) {
		in, _ := setup(t)
		plan, err := PlanDeathDisposition(100, DeathContext{ArenaNonRealDeath: true}, false)
		if err != nil {
			t.Fatalf("PlanDeathDisposition: %v", err)
		}
		in.Disposition = plan
		if _, err := BuildImmediateDeathCapture(in); !errors.Is(err, ErrInvalidDeathInput) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("invalid placement", func(t *testing.T) {
		in, _ := setup(t)
		in.Placement = world.Vec3{X: 1, Y: 0, Z: 1e300}
		if _, err := BuildImmediateDeathCapture(in); err == nil {
			t.Fatalf("expected placement error")
		}
	})
	t.Run("invalid post vitals", func(t *testing.T) {
		in, _ := setup(t)
		in.PostVitals = PlayerVitals{}
		if _, err := BuildImmediateDeathCapture(in); err == nil {
			t.Fatalf("expected vitals error")
		}
	})
	t.Run("stale advancement plan", func(t *testing.T) {
		in, _ := setup(t)
		adv, err := PlanDeathAdvancement(DeathNormal, 999, 999)
		if err != nil {
			t.Fatalf("PlanDeathAdvancement: %v", err)
		}
		in.Advancement = adv
		if _, err := BuildImmediateDeathCapture(in); !errors.Is(err, ErrInvalidDeathInput) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("stale pending plan", func(t *testing.T) {
		in, _ := setup(t)
		in.Pending.EffectiveDeathCost = 42
		if _, err := BuildImmediateDeathCapture(in); !errors.Is(err, ErrInvalidDeathInput) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("non-frozen corpse policy", func(t *testing.T) {
		in, _ := setup(t)
		in.Corpse.LifetimeMs++
		if _, err := BuildImmediateDeathCapture(in); !errors.Is(err, ErrInvalidDeathInput) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("cheap drop entry dropping", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		id := addFullStatePlayer(t, e, testCharacterID(), world.Vec3{X: 5, Y: 0, Z: 6}, testFullDurableState())
		_, base := beginFullCapture(t, e, id)
		in := cheapTokenInputs(t, base, 202)
		in.Drops.Items[0].Drop = true
		if _, err := BuildImmediateDeathCapture(in); !errors.Is(err, ErrInvalidDeathInput) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("bad killer identity", func(t *testing.T) {
		in, _ := setup(t)
		in.Killer = DeathKillerIdentity{Kind: DeathKillerCharacter, MobID: 3}
		if _, err := BuildImmediateDeathCapture(in); !errors.Is(err, ErrInvalidDeathInput) {
			t.Fatalf("err = %v", err)
		}
	})
}
