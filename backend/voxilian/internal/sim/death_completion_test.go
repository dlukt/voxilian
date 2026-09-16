package sim

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/dlukt/voxilian/internal/world"
)

// M5-T5c3c3a authoritative immediate-death owner completion +
// typed completion ingress tests (spec §9.5.1h): owner-local
// PlayerAcceptImmediateDeathCompletion (first apply incl.
// cross-cell, durable prevalidation failure + retry,
// install-failure + retry, duplicate exact no-op, duplicate
// ignoring replacement payload, token mismatch matrix, ABA)
// plus typed EnqueueImmediateDeathCompletion ingress (success,
// duplicate, pre-cancel, non-running, full mailbox, payload
// aliasing). Deterministic, no sleeps, no wall clock, no
// Store/persist/gateway/proto involvement.

// testPostDeathDurableState is a valid post-death durable shadow
// deliberately different from testFullDurableState in every
// field: different karma/advancement/flags, all atrophy reset,
// and one inventory item removed.
func testPostDeathDurableState() PlayerDurableState {
	return PlayerDurableState{
		Karma:       42,
		Advancement: []byte(`{"adv_points":0,"gain_chance":-20,"custom":"keep"}`),
		Flags:       0x1204,
		Spells: []PlayerAbilityState{
			{ID: 11, Ability: 50, AtrophyFlag: true},
			{ID: 12, Ability: 60, AtrophyFlag: true},
		},
		Skills: []PlayerAbilityState{
			{ID: 21, Ability: 40, AtrophyFlag: true},
		},
		Items: []PlayerInventoryItemState{
			{ID: 101, ProtoID: 1001, Qty: 3, Hits: 250, Enchants: []byte(`{"glow":1}`), Slot: "hand"},
			{ID: 303, ProtoID: 1003, Qty: 1, Hits: 100, Enchants: []byte(`{"bane":true}`), Slot: "pack"},
		},
	}
}

// testPostDeathVitals is a Validate-valid canonical post-death
// value distinct from zero-HP pre-death vitals.
func testPostDeathVitals(t *testing.T) PlayerVitals {
	t.Helper()
	post := testVitals()
	post.HP = 10
	post.Mana = 5
	if err := post.Validate(); err != nil {
		t.Fatalf("post-death vitals invalid: %v", err)
	}
	return post
}

// testDeathCompletion builds an authoritative completion value
// for the token with canonical runtime inputs.
func testDeathCompletion(tok DeathAttemptToken, dest world.Vec3, v PlayerVitals, d PlayerDurableState) ImmediateDeathCompletion {
	return ImmediateDeathCompletion{
		Token:         tok,
		Placement:     dest,
		Vitals:        v,
		RuntimeInputs: testRuntimeInputs(),
		Durable:       d,
	}
}

// prepareCompletionPlayer mirrors prepareDeathGatePlayer but with
// a full durable shadow: a resident zero-HP player with a
// processed movement anchor (seq 1), a newer accepted pending
// move (seq 2), armed mana/rest deadlines, actedSinceEntry true,
// nonempty history, and a seeded recent-OpID cache.
func prepareCompletionPlayer(t *testing.T, e *Engine, charID CharacterID, d PlayerDurableState) EntityID {
	t.Helper()
	v := zeroHPVitals(t)
	v.Mana = 5
	if err := v.Validate(); err != nil {
		t.Fatalf("completion-player vitals invalid: %v", err)
	}
	snap, err := e.AddPlayerEntityWithDurableState(charID, world.Vec3{X: 1, Y: 0, Z: 1}, v, testRuntimeInputs(), d)
	if err != nil {
		t.Fatalf("AddPlayerEntityWithDurableState: %v", err)
	}
	id := snap.ID
	if err := e.PlayerStartResting(id); err != nil {
		t.Fatalf("PlayerStartResting: %v", err)
	}
	submitMove(t, e, id, 1, MoveDirForward, 1, 100)
	e.Step()
	submitMove(t, e, id, 2, MoveDirLeft, 0, 200)
	seedRecentOps(t, e, id, OpID(7), OpID(9))
	return id
}

// durableOf is a fatal-on-error PlayerDurableStateOf wrapper
// requiring a present shadow.
func durableOf(t *testing.T, e *Engine, id EntityID) PlayerDurableState {
	t.Helper()
	d, ok, err := e.PlayerDurableStateOf(id)
	if err != nil || !ok {
		t.Fatalf("PlayerDurableStateOf(%d) = %+v,%v,%v; want present shadow", uint64(id), d, ok, err)
	}
	return d
}

// requireDurableEqual pins exact durable-shadow equality.
func requireDurableEqual(t *testing.T, got, want PlayerDurableState, what string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s durable mismatch:\ngot  %+v\nwant %+v", what, got, want)
	}
}

// TestImmediateDeathCompletionFirstApply proves the first valid
// completion installs placement, post-death vitals, runtime
// inputs, AND the post-death durable shadow atomically, with
// T5c3b history/runtime behavior and no observer event.
func TestImmediateDeathCompletionFirstApply(t *testing.T) {
	rec := &vitalsRecorder{}
	e := newPlayerEngine(t, rec)
	pre := testFullDurableState()
	id := prepareCompletionPlayer(t, e, testCharacterID(), pre)
	before, err := e.Entity(id)
	if err != nil {
		t.Fatalf("Entity: %v", err)
	}
	tok := beginDeath(t, e, id)
	tick := e.CurrentTick()
	dest := world.Vec3{X: 5, Y: 0, Z: 7}
	post := testPostDeathVitals(t)
	inputs := testRuntimeInputs()
	wantDurable := testPostDeathDurableState()
	completion := testDeathCompletion(tok, dest, post, wantDurable)

	got, disp, err := e.PlayerAcceptImmediateDeathCompletion(completion)
	if err != nil {
		t.Fatalf("PlayerAcceptImmediateDeathCompletion: %v", err)
	}
	if disp != DeathCompletionApplied {
		t.Fatalf("disp = %d, want Applied", disp)
	}
	if st, _ := lifeOf(t, e, id); st != PlayerLifeAwaitingRespawn {
		t.Fatalf("life = %d, want AwaitingRespawn", uint8(st))
	}
	if got.ID != id || got.CharacterID != testCharacterID() {
		t.Fatalf("identity = %+v", got)
	}
	if got.Position != dest {
		t.Fatalf("position = %+v, want %+v", got.Position, dest)
	}
	if got.OwnershipGeneration != before.OwnershipGeneration {
		t.Fatalf("same-cell generation changed: %d vs %d", got.OwnershipGeneration, before.OwnershipGeneration)
	}
	if live, ok, err := e.PlayerVitalsOf(id); err != nil || !ok || live != post {
		t.Fatalf("vitals = %+v,%v,%v; want %+v", live, ok, err, post)
	}
	rt, ok, err := e.PlayerVitalsRuntimeOf(id)
	if err != nil || !ok || rt.Inputs != inputs {
		t.Fatalf("runtime = %+v,%v,%v", rt, ok, err)
	}
	if rt.ActedSinceEntry {
		t.Fatalf("actedSinceEntry true")
	}
	if rt.RestArmed || rt.RestDue != 0 {
		t.Fatalf("rest armed: %+v", rt)
	}
	if rt.StomachAnchorTick != tick {
		t.Fatalf("stomach anchor = %d, want %d", rt.StomachAnchorTick, tick)
	}
	if !rt.HealthArmed || rt.HealthDue != wantHealthDue(t, e, tick, post, inputs) {
		t.Fatalf("health = %v/%d, want fresh deadline", rt.HealthArmed, rt.HealthDue)
	}
	if !rt.ManaArmed || rt.ManaDue != wantManaDue(t, e, tick, post, inputs) {
		t.Fatalf("mana = %v/%d, want fresh deadline", rt.ManaArmed, rt.ManaDue)
	}
	ent := entOf(t, e, id)
	if ent.activeHeldDirs != 0 || ent.activeRun || ent.speed != 0 || ent.hasPending {
		t.Fatalf("movement not stopped")
	}
	if !ent.hasAccepted || ent.lastAcceptedSeq != 2 || !ent.hasProcessed || ent.lastProcessedSeq != 1 {
		t.Fatalf("sequence anchors changed")
	}
	if ent.recentOps == nil || !ent.recentOps.contains(OpID(7)) || !ent.recentOps.contains(OpID(9)) {
		t.Fatalf("recent OpIDs not preserved")
	}
	if hist, err := e.History(id); err != nil || len(hist) != 0 {
		t.Fatalf("history len=%d err=%v, want empty", len(hist), err)
	}
	if rec.count() != 0 {
		t.Fatalf("observer fired %d events, want 0", rec.count())
	}
	if ent.deathEpoch != tok.Epoch {
		t.Fatalf("epoch changed by completion")
	}
	requireDurableEqual(t, durableOf(t, e, id), wantDurable, "first apply")
	if reflect.DeepEqual(durableOf(t, e, id), pre) {
		t.Fatalf("pre-death durable shadow still live")
	}

	// Caller mutation of the submitted completion after the call
	// cannot reach live state (compare against a fresh builder:
	// completion shares slice backing with wantDurable).
	completion.Durable.Karma = -999
	completion.Durable.Advancement[0] = 'X'
	completion.Durable.Spells[0].Ability = 1
	completion.Durable.Items[0].Qty = -999
	completion.Durable.Items[0].Enchants[0] = 'X'
	requireDurableEqual(t, durableOf(t, e, id), testPostDeathDurableState(), "post-call caller mutation")
}

// TestImmediateDeathCompletionCrossCell proves cross-cell
// application: same entity, generation exactly +1, destination
// resident, durable shadow installed, AwaitingRespawn.
func TestImmediateDeathCompletionCrossCell(t *testing.T) {
	e := newPlayerEngine(t, nil)
	id := prepareCompletionPlayer(t, e, testCharacterID(), testFullDurableState())
	before, err := e.Entity(id)
	if err != nil {
		t.Fatalf("Entity: %v", err)
	}
	tok := beginDeath(t, e, id)
	dest := world.Vec3{X: 40, Y: 0, Z: 3}
	wantDurable := testPostDeathDurableState()

	got, disp, err := e.PlayerAcceptImmediateDeathCompletion(testDeathCompletion(tok, dest, testPostDeathVitals(t), wantDurable))
	if err != nil {
		t.Fatalf("PlayerAcceptImmediateDeathCompletion: %v", err)
	}
	if disp != DeathCompletionApplied {
		t.Fatalf("disp = %d, want Applied", disp)
	}
	if got.ID != id || got.CharacterID != testCharacterID() {
		t.Fatalf("identity changed: %+v", got)
	}
	if got.Position != dest || got.Cell != (world.CellCoord{X: 1, Z: 0}) {
		t.Fatalf("destination = %+v, want %+v in cell {1 0}", got.Position, dest)
	}
	if got.OwnershipGeneration != before.OwnershipGeneration+1 {
		t.Fatalf("generation = %d, want exactly +1", got.OwnershipGeneration)
	}
	if st, _ := lifeOf(t, e, id); st != PlayerLifeAwaitingRespawn {
		t.Fatalf("life = %d, want AwaitingRespawn", uint8(st))
	}
	requireDurableEqual(t, durableOf(t, e, id), wantDurable, "cross-cell")
	if len(e.registry.migrations) != 0 {
		t.Fatalf("migration record left behind")
	}
}

// TestImmediateDeathCompletionInvalidDurableRetry proves durable
// prevalidation failure leaves everything bit-identical with the
// token retryable, then retries the SAME token successfully.
func TestImmediateDeathCompletionInvalidDurableRetry(t *testing.T) {
	e := newPlayerEngine(t, nil)
	pre := testFullDurableState()
	id := prepareCompletionPlayer(t, e, testCharacterID(), pre)
	tok := beginDeath(t, e, id)
	probe := captureLifeProbe(t, e, id)

	bad := testPostDeathDurableState()
	bad.Items = append(append([]PlayerInventoryItemState(nil), bad.Items...),
		PlayerInventoryItemState{ID: 101, ProtoID: 1002, Qty: 1, Hits: 1, Enchants: []byte(`{}`), Slot: "pack"})
	if _, _, err := e.PlayerAcceptImmediateDeathCompletion(testDeathCompletion(tok, world.Vec3{X: 5, Y: 0, Z: 7}, testPostDeathVitals(t), bad)); !errors.Is(err, ErrInvalidDeathInput) {
		t.Fatalf("invalid durable err = %v, want ErrInvalidDeathInput", err)
	}
	if st, _ := lifeOf(t, e, id); st != PlayerLifeDeathPersisting {
		t.Fatalf("life = %d, want DeathPersisting after durable rejection", uint8(st))
	}
	requireLifeProbeUnchanged(t, e, id, probe, "invalid durable")
	requireDurableEqual(t, durableOf(t, e, id), pre, "invalid durable pre-death shadow")
	if entOf(t, e, id).deathEpoch != tok.Epoch {
		t.Fatalf("epoch changed by rejected completion")
	}

	wantDurable := testPostDeathDurableState()
	got, disp, err := e.PlayerAcceptImmediateDeathCompletion(testDeathCompletion(tok, world.Vec3{X: 5, Y: 0, Z: 7}, testPostDeathVitals(t), wantDurable))
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if disp != DeathCompletionApplied {
		t.Fatalf("retry disp = %d, want Applied", disp)
	}
	if got.Position != (world.Vec3{X: 5, Y: 0, Z: 7}) {
		t.Fatalf("retry position = %+v", got.Position)
	}
	requireDurableEqual(t, durableOf(t, e, id), wantDurable, "retry")
	if entOf(t, e, id).deathEpoch != tok.Epoch {
		t.Fatalf("retry advanced the epoch")
	}
}

// TestImmediateDeathCompletionInstallFailureRetry forces a
// deterministic T5c3b install failure (cross-cell ownership
// generation exhaustion), proves the pre-death durable shadow
// and DeathPersisting attempt survive, then retries the SAME
// completion successfully.
func TestImmediateDeathCompletionInstallFailureRetry(t *testing.T) {
	e := newPlayerEngine(t, nil)
	pre := testFullDurableState()
	id := prepareCompletionPlayer(t, e, testCharacterID(), pre)
	tok := beginDeath(t, e, id)
	origGen := entOf(t, e, id).generation
	entOf(t, e, id).generation = math.MaxUint64
	probe := captureLifeProbe(t, e, id)

	dest := world.Vec3{X: 40, Y: 0, Z: 3}
	wantDurable := testPostDeathDurableState()
	if _, _, err := e.PlayerAcceptImmediateDeathCompletion(testDeathCompletion(tok, dest, testPostDeathVitals(t), wantDurable)); !errors.Is(err, ErrOwnershipGenerationExhausted) {
		t.Fatalf("install err = %v, want ErrOwnershipGenerationExhausted", err)
	}
	if st, _ := lifeOf(t, e, id); st != PlayerLifeDeathPersisting {
		t.Fatalf("life = %d, want DeathPersisting after failed install", uint8(st))
	}
	if after := entOf(t, e, id); after.generation != math.MaxUint64 {
		t.Fatalf("generation = %d, want forced MaxUint64", after.generation)
	}
	requireLifeProbeUnchanged(t, e, id, probe, "failed install")
	requireDurableEqual(t, durableOf(t, e, id), pre, "failed install pre-death shadow")
	if len(e.registry.migrations) != 0 {
		t.Fatalf("migration record left behind")
	}

	entOf(t, e, id).generation = origGen
	snap, disp, err := e.PlayerAcceptImmediateDeathCompletion(testDeathCompletion(tok, dest, testPostDeathVitals(t), wantDurable))
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if disp != DeathCompletionApplied {
		t.Fatalf("retry disp = %d, want Applied", disp)
	}
	if snap.Position != dest {
		t.Fatalf("retry position = %+v, want %+v", snap.Position, dest)
	}
	requireDurableEqual(t, durableOf(t, e, id), wantDurable, "retry")
	if st, _ := lifeOf(t, e, id); st != PlayerLifeAwaitingRespawn {
		t.Fatalf("retry life = %d, want AwaitingRespawn", uint8(st))
	}
	if entOf(t, e, id).deathEpoch != tok.Epoch {
		t.Fatalf("retry advanced the epoch")
	}
}

// TestImmediateDeathCompletionDuplicateNoOp proves redelivery of
// the SAME completion is an absolute zero-mutation no-op,
// including the durable shadow.
func TestImmediateDeathCompletionDuplicateNoOp(t *testing.T) {
	rec := &vitalsRecorder{}
	e := newPlayerEngine(t, rec)
	id := prepareCompletionPlayer(t, e, testCharacterID(), testFullDurableState())
	tok := beginDeath(t, e, id)
	wantDurable := testPostDeathDurableState()
	completion := testDeathCompletion(tok, world.Vec3{X: 5, Y: 0, Z: 7}, testPostDeathVitals(t), wantDurable)

	first, disp, err := e.PlayerAcceptImmediateDeathCompletion(completion)
	if err != nil || disp != DeathCompletionApplied {
		t.Fatalf("first = %+v,%d,%v; want Applied,nil", first, disp, err)
	}
	eventsAfterFirst := rec.count()

	ent := entOf(t, e, id)
	wantSnap := ent.snapshot()
	wantVitals := ent.vitals
	wantRuntime := ent.runtimeSnapshotValue()
	wantDurableCopy := durableOf(t, e, id)
	wantGen := ent.generation
	wantPos := ent.position
	wantYaw := ent.yaw
	wantAccepted, wantAcceptedSeq := ent.hasAccepted, ent.lastAcceptedSeq
	wantProcessed, wantProcessedSeq := ent.hasProcessed, ent.lastProcessedSeq
	wantAnchor := ent.stomachAnchorTick
	wantEpoch := ent.deathEpoch
	histBefore, err := e.History(id)
	if err != nil {
		t.Fatalf("History: %v", err)
	}

	dup, ddisp, err := e.PlayerAcceptImmediateDeathCompletion(completion)
	if err != nil {
		t.Fatalf("duplicate err = %v, want nil", err)
	}
	if ddisp != DeathCompletionDuplicate {
		t.Fatalf("duplicate disp = %d, want Duplicate", ddisp)
	}
	if dup != first {
		t.Fatalf("duplicate snapshot = %+v, want %+v", dup, first)
	}
	now := entOf(t, e, id)
	if now.snapshot() != wantSnap {
		t.Fatalf("duplicate mutated the snapshot")
	}
	if now.vitals != wantVitals || now.runtimeSnapshotValue() != wantRuntime {
		t.Fatalf("duplicate mutated vitals/runtime")
	}
	requireDurableEqual(t, durableOf(t, e, id), wantDurableCopy, "duplicate")
	if now.generation != wantGen || now.position != wantPos || now.yaw != wantYaw {
		t.Fatalf("duplicate relocated the entity")
	}
	if now.hasAccepted != wantAccepted || now.lastAcceptedSeq != wantAcceptedSeq ||
		now.hasProcessed != wantProcessed || now.lastProcessedSeq != wantProcessedSeq {
		t.Fatalf("duplicate touched sequence anchors")
	}
	if now.stomachAnchorTick != wantAnchor {
		t.Fatalf("duplicate re-anchored the stomach")
	}
	if now.deathEpoch != wantEpoch {
		t.Fatalf("duplicate changed the epoch")
	}
	if now.healthDue != wantRuntime.HealthDue || now.manaDue != wantRuntime.ManaDue || now.restDue != wantRuntime.RestDue {
		t.Fatalf("duplicate restarted deadlines")
	}
	histAfter, err := e.History(id)
	if err != nil || len(histAfter) != len(histBefore) {
		t.Fatalf("duplicate history len=%d err=%v, want %d", len(histAfter), err, len(histBefore))
	}
	for i := range histBefore {
		if histAfter[i] != histBefore[i] {
			t.Fatalf("duplicate mutated history[%d]", i)
		}
	}
	if rec.count() != eventsAfterFirst {
		t.Fatalf("duplicate fired observer events: %d vs %d", rec.count(), eventsAfterFirst)
	}
	if st, _ := lifeOf(t, e, id); st != PlayerLifeAwaitingRespawn {
		t.Fatalf("duplicate life = %d, want AwaitingRespawn", uint8(st))
	}
}

// TestImmediateDeathCompletionDuplicateIgnoresPayload pins
// duplicate-before-payload-validation: the same valid token with
// a malformed replacement payload still returns Duplicate with
// nil error and zero mutation.
func TestImmediateDeathCompletionDuplicateIgnoresPayload(t *testing.T) {
	e := newPlayerEngine(t, nil)
	id := prepareCompletionPlayer(t, e, testCharacterID(), testFullDurableState())
	tok := beginDeath(t, e, id)
	wantDurable := testPostDeathDurableState()
	if _, disp, err := e.PlayerAcceptImmediateDeathCompletion(testDeathCompletion(tok, world.Vec3{X: 5, Y: 0, Z: 7}, testPostDeathVitals(t), wantDurable)); err != nil || disp != DeathCompletionApplied {
		t.Fatalf("first = %d,%v; want Applied,nil", disp, err)
	}
	probe := captureLifeProbe(t, e, id)

	garbage := ImmediateDeathCompletion{
		Token:         tok,
		Placement:     world.Vec3{X: math.NaN(), Y: 0, Z: 0},
		Vitals:        PlayerVitals{},
		RuntimeInputs: PlayerVitalsRuntimeInputs{},
		Durable:       PlayerDurableState{Advancement: []byte(`nope`)},
	}
	dup, ddisp, err := e.PlayerAcceptImmediateDeathCompletion(garbage)
	if err != nil {
		t.Fatalf("malformed redelivery err = %v, want nil", err)
	}
	if ddisp != DeathCompletionDuplicate {
		t.Fatalf("malformed redelivery disp = %d, want Duplicate", ddisp)
	}
	if dup.Position != (world.Vec3{X: 5, Y: 0, Z: 7}) {
		t.Fatalf("malformed redelivery snapshot = %+v", dup)
	}
	requireLifeProbeUnchanged(t, e, id, probe, "malformed redelivery")
	requireDurableEqual(t, durableOf(t, e, id), wantDurable, "malformed redelivery")
}

// TestImmediateDeathCompletionMismatch proves the token mismatch
// matrix preserves existing semantics with zero mutation.
func TestImmediateDeathCompletionMismatch(t *testing.T) {
	e := newPlayerEngine(t, nil)
	pre := testFullDurableState()
	id := prepareCompletionPlayer(t, e, testCharacterID(), pre)
	tok := beginDeath(t, e, id)
	payload := func() ImmediateDeathCompletion {
		return testDeathCompletion(tok, world.Vec3{X: 5, Y: 0, Z: 7}, testPostDeathVitals(t), testPostDeathDurableState())
	}

	other, err := e.AddPlayerEntity(CharacterID(55), world.Vec3{X: 9, Y: 0, Z: 9}, testVitals(), testRuntimeInputs())
	if err != nil {
		t.Fatalf("AddPlayerEntity: %v", err)
	}
	gen, err := e.AddEntity(world.Vec3{X: 10, Y: 0, Z: 10})
	if err != nil {
		t.Fatalf("AddEntity: %v", err)
	}
	probe := captureLifeProbe(t, e, id)

	withToken := func(mut func(*ImmediateDeathCompletion)) ImmediateDeathCompletion {
		c := payload()
		mut(&c)
		return c
	}
	mismatch := []struct {
		name string
		c    ImmediateDeathCompletion
	}{
		{"generic entity", withToken(func(c *ImmediateDeathCompletion) { c.Token.EntityID = gen.ID })},
		{"other player", withToken(func(c *ImmediateDeathCompletion) { c.Token.EntityID = other.ID })},
		{"wrong character", withToken(func(c *ImmediateDeathCompletion) { c.Token.CharacterID = CharacterID(999) })},
		{"zero character", withToken(func(c *ImmediateDeathCompletion) { c.Token.CharacterID = 0 })},
		{"future epoch", withToken(func(c *ImmediateDeathCompletion) { c.Token.Epoch = tok.Epoch + 1 })},
		{"zero epoch", withToken(func(c *ImmediateDeathCompletion) { c.Token.Epoch = 0 })},
	}
	for _, tc := range mismatch {
		if _, _, err := e.PlayerAcceptImmediateDeathCompletion(tc.c); !errors.Is(err, ErrDeathAttemptMismatch) {
			t.Fatalf("%s err = %v, want ErrDeathAttemptMismatch", tc.name, err)
		}
		requireLifeProbeUnchanged(t, e, id, probe, tc.name)
		requireDurableEqual(t, durableOf(t, e, id), pre, tc.name)
	}

	// Unknown EntityID keeps ErrEntityNotFound.
	unknown := payload()
	unknown.Token.EntityID = EntityID(999)
	if _, _, err := e.PlayerAcceptImmediateDeathCompletion(unknown); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("unknown entity err = %v, want ErrEntityNotFound", err)
	}
	requireLifeProbeUnchanged(t, e, id, probe, "unknown entity")

	// A valid-shaped token on an Alive player (never began) is an
	// incompatible state: mismatch, not applied.
	alive := payload()
	alive.Token = DeathAttemptToken{EntityID: other.ID, CharacterID: CharacterID(55), Epoch: 0}
	if _, _, err := e.PlayerAcceptImmediateDeathCompletion(alive); !errors.Is(err, ErrDeathAttemptMismatch) {
		t.Fatalf("alive forged-token err = %v, want ErrDeathAttemptMismatch", err)
	}
}

// TestImmediateDeathCompletionABA proves a late completion for a
// removed entity returns ErrEntityNotFound and never touches the
// new entity for the same CharacterID.
func TestImmediateDeathCompletionABA(t *testing.T) {
	e := newPlayerEngine(t, nil)
	a := addFullStatePlayer(t, e, testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1}, testFullDurableState())
	oldTok := beginDeath(t, e, a)
	completion := testDeathCompletion(oldTok, world.Vec3{X: 5, Y: 0, Z: 7}, testPostDeathVitals(t), testPostDeathDurableState())
	if err := e.RemoveEntity(a); err != nil {
		t.Fatalf("RemoveEntity: %v", err)
	}
	b := addFullStatePlayer(t, e, testCharacterID(), world.Vec3{X: 2, Y: 0, Z: 2}, testFullDurableState())
	if b == a {
		t.Fatalf("EntityID reused: %d", uint64(b))
	}
	bProbe := captureLifeProbe(t, e, b)
	bDurable := durableOf(t, e, b)
	if _, _, err := e.PlayerAcceptImmediateDeathCompletion(completion); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("late completion err = %v, want ErrEntityNotFound", err)
	}
	requireLifeProbeUnchanged(t, e, b, bProbe, "late completion")
	requireDurableEqual(t, durableOf(t, e, b), bDurable, "late completion")
}

// completionRunEngine builds a Run-capable engine with a manual
// clock for death-completion ingress tests.
func completionRunEngine(t *testing.T, clk *manualClock, col CollisionWorld, rec *vitalsRecorder) *Engine {
	t.Helper()
	var obs PlayerVitalsObserver
	if rec != nil {
		obs = rec
	}
	return mustEngine(t, 20, EngineDeps{
		Clock: clk, RNG: newTestRNG(7), Collision: col, RunGate: staticGate{allow: true}, Vitals: obs,
	})
}

// prepareIngressDeath sets up a resident zero-HP full-state
// player with a begun death attempt owner-locally (before Run
// starts) and returns its token plus the authoritative
// completion to deliver.
func prepareIngressDeath(t *testing.T, e *Engine) (DeathAttemptToken, ImmediateDeathCompletion) {
	t.Helper()
	id := addFullStatePlayer(t, e, testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1}, testFullDurableState())
	tok, err := e.PlayerBeginDeathPersistence(id)
	if err != nil {
		t.Fatalf("PlayerBeginDeathPersistence: %v", err)
	}
	if got := tok.EntityID; got != id {
		t.Fatalf("token entity = %d, want %d", uint64(got), uint64(id))
	}
	return tok, testDeathCompletion(tok, world.Vec3{X: 5, Y: 0, Z: 7}, testPostDeathVitals(t), testPostDeathDurableState())
}

// TestEnqueueImmediateDeathCompletionSuccess drives a real
// Engine.Run and proves the typed command executes on the owner
// with exact state installation.
func TestEnqueueImmediateDeathCompletionSuccess(t *testing.T) {
	clk := newManualClock()
	rec := &vitalsRecorder{}
	e := completionRunEngine(t, clk, openCollision{}, rec)
	tok, completion := prepareIngressDeath(t, e)
	cancel, done := runOwner(t, e, clk)
	defer stopOwner(t, cancel, done)

	got, disp, err := e.EnqueueImmediateDeathCompletion(context.Background(), completion)
	if err != nil {
		t.Fatalf("EnqueueImmediateDeathCompletion: %v", err)
	}
	if disp != DeathCompletionApplied {
		t.Fatalf("disp = %d, want Applied", disp)
	}
	if got.ID != tok.EntityID || got.Position != completion.Placement {
		t.Fatalf("snapshot = %+v", got)
	}
	if live, ok, err := e.PlayerVitalsOf(tok.EntityID); err != nil || !ok || live != completion.Vitals {
		t.Fatalf("vitals = %+v,%v,%v", live, ok, err)
	}
	requireDurableEqual(t, durableOf(t, e, tok.EntityID), completion.Durable, "ingress success")
	if st, _ := lifeOf(t, e, tok.EntityID); st != PlayerLifeAwaitingRespawn {
		t.Fatalf("life = %d, want AwaitingRespawn", uint8(st))
	}
	if rec.count() != 0 {
		t.Fatalf("observer fired %d events, want 0", rec.count())
	}
}

// TestEnqueueImmediateDeathCompletionDuplicate proves a second
// delivery of the same completion through ingress resolves as
// Duplicate with zero mutation.
func TestEnqueueImmediateDeathCompletionDuplicate(t *testing.T) {
	clk := newManualClock()
	e := completionRunEngine(t, clk, openCollision{}, nil)
	tok, completion := prepareIngressDeath(t, e)
	cancel, done := runOwner(t, e, clk)
	defer stopOwner(t, cancel, done)

	ctx := context.Background()
	first, disp, err := e.EnqueueImmediateDeathCompletion(ctx, completion)
	if err != nil || disp != DeathCompletionApplied {
		t.Fatalf("first = %+v,%d,%v; want Applied,nil", first, disp, err)
	}
	probe := captureLifeProbe(t, e, tok.EntityID)
	wantDurable := durableOf(t, e, tok.EntityID)

	dup, ddisp, err := e.EnqueueImmediateDeathCompletion(ctx, completion)
	if err != nil {
		t.Fatalf("duplicate err = %v, want nil", err)
	}
	if ddisp != DeathCompletionDuplicate {
		t.Fatalf("duplicate disp = %d, want Duplicate", ddisp)
	}
	if dup != first {
		t.Fatalf("duplicate snapshot = %+v, want %+v", dup, first)
	}
	requireLifeProbeUnchanged(t, e, tok.EntityID, probe, "ingress duplicate")
	requireDurableEqual(t, durableOf(t, e, tok.EntityID), wantDurable, "ingress duplicate")
}

// TestEnqueueImmediateDeathCompletionPreCancelled proves a
// pre-cancelled context publishes nothing and mutates nothing.
func TestEnqueueImmediateDeathCompletionPreCancelled(t *testing.T) {
	clk := newManualClock()
	e := completionRunEngine(t, clk, openCollision{}, nil)
	tok, completion := prepareIngressDeath(t, e)
	cancel, done := runOwner(t, e, clk)
	defer stopOwner(t, cancel, done)
	probe := captureLifeProbe(t, e, tok.EntityID)

	ctx, stop := context.WithCancel(context.Background())
	stop()
	if _, _, err := e.EnqueueImmediateDeathCompletion(ctx, completion); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled enqueue = %v, want context.Canceled", err)
	}
	if n := len(e.ingress); n != 0 {
		t.Fatalf("mailbox len = %d after cancelled enqueue, want 0", n)
	}
	requireLifeProbeUnchanged(t, e, tok.EntityID, probe, "pre-cancelled")
	requireDurableEqual(t, durableOf(t, e, tok.EntityID), testFullDurableState(), "pre-cancelled")
}

// TestEnqueueImmediateDeathCompletionNotRunning proves admission
// without a Run owner fails with ErrEngineNotRunning and zero
// mutation.
func TestEnqueueImmediateDeathCompletionNotRunning(t *testing.T) {
	e := completionRunEngine(t, newManualClock(), openCollision{}, nil)
	tok, completion := prepareIngressDeath(t, e)
	if _, _, err := e.EnqueueImmediateDeathCompletion(context.Background(), completion); !errors.Is(err, ErrEngineNotRunning) {
		t.Fatalf("enqueue without Run = %v, want ErrEngineNotRunning", err)
	}
	if st, _ := lifeOf(t, e, tok.EntityID); st != PlayerLifeDeathPersisting {
		t.Fatalf("life = %d, want DeathPersisting", uint8(st))
	}
	requireDurableEqual(t, durableOf(t, e, tok.EntityID), testFullDurableState(), "not-running")
}

// TestEnqueueImmediateDeathCompletionFullMailbox proves a
// saturated mailbox rejects with ErrSimIngressFull while the
// player stays DeathPersisting, and the SAME completion retries
// successfully once capacity frees.
func TestEnqueueImmediateDeathCompletionFullMailbox(t *testing.T) {
	col := newBlockingCollision()
	clk := newManualClock()
	e := completionRunEngine(t, clk, col, nil)
	tok, completion := prepareIngressDeath(t, e)
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
	if _, _, err := e.EnqueueImmediateDeathCompletion(context.Background(), completion); !errors.Is(err, ErrSimIngressFull) {
		t.Fatalf("full-mailbox enqueue = %v, want ErrSimIngressFull", err)
	}
	if st, _ := lifeOf(t, e, tok.EntityID); st != PlayerLifeDeathPersisting {
		t.Fatalf("life = %d, want DeathPersisting after full rejection", uint8(st))
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
	got, disp, err := e.EnqueueImmediateDeathCompletion(context.Background(), completion)
	if err != nil || disp != DeathCompletionApplied {
		t.Fatalf("retry = %+v,%d,%v; want Applied,nil", got, disp, err)
	}
	requireDurableEqual(t, durableOf(t, e, tok.EntityID), completion.Durable, "full-mailbox retry")
	if st, _ := lifeOf(t, e, tok.EntityID); st != PlayerLifeAwaitingRespawn {
		t.Fatalf("retry life = %d, want AwaitingRespawn", uint8(st))
	}
	stopOwner(t, cancel, done)
}

// TestEnqueueImmediateDeathCompletionPayloadAliasing proves the
// typed ingress freezes the completion payload before owner
// execution: caller mutation after publication but before the
// owner applies installs the frozen originals.
func TestEnqueueImmediateDeathCompletionPayloadAliasing(t *testing.T) {
	col := newBlockingCollision()
	clk := newManualClock()
	e := completionRunEngine(t, clk, col, nil)
	tok, completion := prepareIngressDeath(t, e)
	wantDurable := completion.Durable
	// Deep-copy the want values independently: the caller-owned
	// completion is about to be poisoned.
	wantDurable.Advancement = append([]byte(nil), wantDurable.Advancement...)
	wantDurable.Spells = append([]PlayerAbilityState(nil), wantDurable.Spells...)
	wantDurable.Skills = append([]PlayerAbilityState(nil), wantDurable.Skills...)
	wantDurable.Items = append([]PlayerInventoryItemState(nil), wantDurable.Items...)
	for i := range wantDurable.Items {
		wantDurable.Items[i].Enchants = append([]byte(nil), wantDurable.Items[i].Enchants...)
	}

	// Stall the owner inside the completion's own VolumeFlagsAt
	// (same-cell install samples destination flags after
	// relocation, before vitals/durable installation).
	release := col.block(completion.Placement)
	cancel, done := runOwner(t, e, clk)
	type out struct {
		snap EntitySnapshot
		disp DeathCompletionDisposition
		err  error
	}
	resCh := make(chan out, 1)
	go func() {
		snap, disp, err := e.EnqueueImmediateDeathCompletion(context.Background(), completion)
		resCh <- out{snap: snap, disp: disp, err: err}
	}()
	waitEntered(t, col, completion.Placement)

	// Poison every caller-owned buffer after publication but
	// before owner execution completes.
	completion.Durable.Karma = -1
	for i := range completion.Durable.Advancement {
		completion.Durable.Advancement[i] = 'X'
	}
	for i := range completion.Durable.Spells {
		completion.Durable.Spells[i].Ability = 1
		completion.Durable.Spells[i].AtrophyFlag = false
	}
	for i := range completion.Durable.Skills {
		completion.Durable.Skills[i].Ability = 99
	}
	for i := range completion.Durable.Items {
		completion.Durable.Items[i].Qty = -777
		completion.Durable.Items[i].Hits = -778
		for j := range completion.Durable.Items[i].Enchants {
			completion.Durable.Items[i].Enchants[j] = 'X'
		}
	}
	close(release)

	var res out
	select {
	case res = <-resCh:
	case <-time.After(10 * time.Second):
		t.Fatalf("timeout waiting for completion result")
	}
	if res.err != nil || res.disp != DeathCompletionApplied {
		t.Fatalf("result = %+v,%d,%v; want Applied,nil", res.snap, res.disp, res.err)
	}
	requireDurableEqual(t, durableOf(t, e, tok.EntityID), wantDurable, "aliasing")
	if live, ok, err := e.PlayerVitalsOf(tok.EntityID); err != nil || !ok || live != completion.Vitals {
		t.Fatalf("vitals = %+v,%v,%v", live, ok, err)
	}
	stopOwner(t, cancel, done)
}
