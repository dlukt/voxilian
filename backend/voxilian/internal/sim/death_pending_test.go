package sim

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dlukt/voxilian/internal/world"
)

// M5-T5c3d1 authoritative pending-death owner state +
// respawn-release tests (spec §9.5.1k): completion pending
// install (active + nil + invalid + duplicate-before-
// validation), CorpseID aliasing (creation / ingress /
// inspection / hydration), handoff/removal ownership,
// hydration, respawn release (applied / duplicate /
// mismatch / ABA), and the gameplay gate proving release
// != Underworld-exit penalty consumption. Deterministic,
// no wall clock, no Store/persist/gateway/proto.

func int64ptr(v int64) *int64 { return &v }

func pendingFixture(cost int, tm int64, corpse *int64, portal bool) *PendingDeathRuntime {
	return &PendingDeathRuntime{
		EffectiveCost:    cost,
		DeathTimeSeconds: tm,
		CorpseID:         corpse,
		PortalUsed:       portal,
	}
}

// pendingCompletionPlayer builds a DeathPersisting player
// with a full durable shadow and returns its attempt
// token plus the pre-completion live baselines.
func pendingCompletionPlayer(t *testing.T, e *Engine, charID CharacterID) (EntityID, DeathAttemptToken) {
	t.Helper()
	id := prepareCompletionPlayer(t, e, charID, testFullDurableState())
	tok := beginDeath(t, e, id)
	return id, tok
}

func pendingOf(t *testing.T, e *Engine, id EntityID) (PendingDeathRuntime, bool) {
	t.Helper()
	p, ok, err := e.PlayerPendingDeathOf(id)
	if err != nil {
		t.Fatalf("PlayerPendingDeathOf(%d): %v", uint64(id), err)
	}
	return p, ok
}

func requirePendingEqual(t *testing.T, got, want PendingDeathRuntime, what string) {
	t.Helper()
	if got.EffectiveCost != want.EffectiveCost ||
		got.DeathTimeSeconds != want.DeathTimeSeconds ||
		got.PortalUsed != want.PortalUsed {
		t.Fatalf("%s pending scalar mismatch: got %+v want %+v", what, got, want)
	}
	if (got.CorpseID == nil) != (want.CorpseID == nil) {
		t.Fatalf("%s pending corpse presence mismatch: got %+v want %+v", what, got, want)
	}
	if got.CorpseID != nil && *got.CorpseID != *want.CorpseID {
		t.Fatalf("%s pending corpse mismatch: got %d want %d", what, *got.CorpseID, *want.CorpseID)
	}
	if got.CorpseID != nil && want.CorpseID != nil && got.CorpseID == want.CorpseID {
		t.Fatalf("%s pending corpse pointer aliases", what)
	}
}

// TestPendingDeathCompletionInstallsActive proves a
// DeathPersisting player + valid completion installs the
// exact pending, durable, placement, vitals, and runtime
// atomically with no observer replay.
func TestPendingDeathCompletionInstallsActive(t *testing.T) {
	rec := &vitalsRecorder{}
	e := newPlayerEngine(t, rec)
	id, tok := pendingCompletionPlayer(t, e, testCharacterID())
	dest := world.Vec3{X: 5, Y: 0, Z: 7}
	post := testPostDeathVitals(t)
	wantDurable := testPostDeathDurableState()
	completion := testDeathCompletion(tok, dest, post, wantDurable)
	completion.Pending = pendingFixture(37, 1234, int64ptr(99), false)

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
	if got.Position != dest {
		t.Fatalf("position = %+v, want %+v", got.Position, dest)
	}
	if live, ok, err := e.PlayerVitalsOf(id); err != nil || !ok || live != post {
		t.Fatalf("vitals = %+v,%v,%v; want %+v", live, ok, err, post)
	}
	requireDurableEqual(t, durableOf(t, e, id), wantDurable, "pending install")
	live, ok := pendingOf(t, e, id)
	if !ok {
		t.Fatalf("pending absent after install")
	}
	requirePendingEqual(t, live, PendingDeathRuntime{
		EffectiveCost: 37, DeathTimeSeconds: 1234,
		CorpseID: int64ptr(99), PortalUsed: false,
	}, "install")
	if len(rec.events) != 0 {
		t.Fatalf("observer replayed %d events", len(rec.events))
	}
}

// TestPendingDeathCompletionNilPending proves a
// newbie-home completion installs exact nil pending.
func TestPendingDeathCompletionNilPending(t *testing.T) {
	e := newPlayerEngine(t, nil)
	id, tok := pendingCompletionPlayer(t, e, testCharacterID())
	completion := testDeathCompletion(tok, world.Vec3{X: 5, Y: 0, Z: 7}, testPostDeathVitals(t), testPostDeathDurableState())
	completion.Pending = nil
	if _, disp, err := e.PlayerAcceptImmediateDeathCompletion(completion); err != nil || disp != DeathCompletionApplied {
		t.Fatalf("apply = %d,%v; want Applied,nil", disp, err)
	}
	if _, ok := pendingOf(t, e, id); ok {
		t.Fatalf("pending present after nil completion")
	}
}

// TestPendingDeathCompletionInvalidZeroMutation proves
// invalid pending values fail with zero mutation and the
// same token stays retryable.
func TestPendingDeathCompletionInvalidZeroMutation(t *testing.T) {
	cases := []struct {
		name    string
		pending *PendingDeathRuntime
	}{
		{"cost -1", pendingFixture(-1, 10, int64ptr(5), false)},
		{"cost 101", pendingFixture(101, 10, int64ptr(5), false)},
		{"negative death time", pendingFixture(10, -1, int64ptr(5), false)},
		{"corpse 0", pendingFixture(10, 10, int64ptr(0), false)},
		{"corpse -1", pendingFixture(10, 10, int64ptr(-1), false)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newPlayerEngine(t, nil)
			id, tok := pendingCompletionPlayer(t, e, testCharacterID())
			beforeSnap, err := e.Entity(id)
			if err != nil {
				t.Fatal(err)
			}
			beforeVitals, _, _ := e.PlayerVitalsOf(id)
			beforeRT, _, _ := e.PlayerVitalsRuntimeOf(id)
			beforeDurable := durableOf(t, e, id)
			beforeHist, err := e.History(id)
			if err != nil {
				t.Fatal(err)
			}
			completion := testDeathCompletion(tok, world.Vec3{X: 5, Y: 0, Z: 7}, testPostDeathVitals(t), testPostDeathDurableState())
			completion.Pending = tc.pending
			if _, _, err := e.PlayerAcceptImmediateDeathCompletion(completion); err == nil {
				t.Fatalf("invalid pending accepted")
			}
			if st, _ := lifeOf(t, e, id); st != PlayerLifeDeathPersisting {
				t.Fatalf("life = %d, want DeathPersisting", uint8(st))
			}
			afterSnap, _ := e.Entity(id)
			if afterSnap != beforeSnap {
				t.Fatalf("snapshot mutated:\nbefore %+v\nafter  %+v", beforeSnap, afterSnap)
			}
			if live, _, _ := e.PlayerVitalsOf(id); live != beforeVitals {
				t.Fatalf("vitals mutated")
			}
			if rt, _, _ := e.PlayerVitalsRuntimeOf(id); rt != beforeRT {
				t.Fatalf("runtime mutated")
			}
			requireDurableEqual(t, durableOf(t, e, id), beforeDurable, "invalid pending")
			if _, ok := pendingOf(t, e, id); ok {
				t.Fatalf("pending installed by invalid completion")
			}
			afterHist, _ := e.History(id)
			if len(afterHist) != len(beforeHist) {
				t.Fatalf("history mutated")
			}
			// Same token retries successfully.
			retry := testDeathCompletion(tok, world.Vec3{X: 5, Y: 0, Z: 7}, testPostDeathVitals(t), testPostDeathDurableState())
			retry.Pending = pendingFixture(37, 1234, int64ptr(99), false)
			if _, disp, err := e.PlayerAcceptImmediateDeathCompletion(retry); err != nil || disp != DeathCompletionApplied {
				t.Fatalf("retry = %d,%v; want Applied,nil", disp, err)
			}
		})
	}
}

// TestPendingDeathDuplicateBeforeValidation proves a
// duplicate/redelivered completion with a malformed or
// different Pending payload stays an exact zero-mutation
// Duplicate: duplicate detection runs BEFORE payload
// validation.
func TestPendingDeathDuplicateBeforeValidation(t *testing.T) {
	e := newPlayerEngine(t, nil)
	id, tok := pendingCompletionPlayer(t, e, testCharacterID())
	first := testDeathCompletion(tok, world.Vec3{X: 5, Y: 0, Z: 7}, testPostDeathVitals(t), testPostDeathDurableState())
	first.Pending = pendingFixture(37, 1234, int64ptr(99), false)
	if _, disp, err := e.PlayerAcceptImmediateDeathCompletion(first); err != nil || disp != DeathCompletionApplied {
		t.Fatalf("first = %d,%v", disp, err)
	}
	beforeSnap, _ := e.Entity(id)
	beforePending, _ := pendingOf(t, e, id)
	beforeDurable := durableOf(t, e, id)
	mutants := []*PendingDeathRuntime{
		pendingFixture(-1, 10, int64ptr(5), false),
		pendingFixture(1, 2, int64ptr(3), true),
		nil,
	}
	for i, p := range mutants {
		redeliver := testDeathCompletion(tok, world.Vec3{X: 90, Y: 0, Z: 90}, testPostDeathVitals(t), testFullDurableState())
		redeliver.Pending = p
		snap, disp, err := e.PlayerAcceptImmediateDeathCompletion(redeliver)
		if err != nil || disp != DeathCompletionDuplicate {
			t.Fatalf("mutant %d = %+v,%d,%v; want Duplicate,nil", i, snap, disp, err)
		}
		if snap != beforeSnap {
			t.Fatalf("mutant %d snapshot changed", i)
		}
		live, _ := pendingOf(t, e, id)
		requirePendingEqual(t, live, beforePending, "duplicate")
		requireDurableEqual(t, durableOf(t, e, id), beforeDurable, "duplicate")
	}
}

// TestPendingDeathCorpseIDAliasing proves caller mutation
// of the pointed CorpseID after completion submission or
// pending inspection cannot reach live sim state.
func TestPendingDeathCorpseIDAliasing(t *testing.T) {
	e := newPlayerEngine(t, nil)
	id, tok := pendingCompletionPlayer(t, e, testCharacterID())
	corpse := int64(99)
	completion := testDeathCompletion(tok, world.Vec3{X: 5, Y: 0, Z: 7}, testPostDeathVitals(t), testPostDeathDurableState())
	completion.Pending = pendingFixture(37, 1234, &corpse, false)
	if _, _, err := e.PlayerAcceptImmediateDeathCompletion(completion); err != nil {
		t.Fatal(err)
	}
	// Mutate the caller-owned pointed value after submission.
	corpse = 424242
	live, ok := pendingOf(t, e, id)
	if !ok || live.CorpseID == nil || *live.CorpseID != 99 {
		t.Fatalf("live pending = %+v,%v; want corpse 99", live, ok)
	}
	// Mutate the inspected copy's pointed value.
	*live.CorpseID = 777
	again, _ := pendingOf(t, e, id)
	if again.CorpseID == nil || *again.CorpseID != 99 {
		t.Fatalf("live pending after inspection poison = %+v", again)
	}
}

// TestEnqueueImmediateDeathCompletionPendingAliasing
// proves the typed ingress freezes the CorpseID pointer
// before owner execution: caller mutation after
// publication but before the owner applies installs the
// frozen original.
func TestEnqueueImmediateDeathCompletionPendingAliasing(t *testing.T) {
	col := newBlockingCollision()
	clk := newManualClock()
	e := completionRunEngine(t, clk, col, nil)
	tok, completion := prepareIngressDeath(t, e)
	corpse := int64(555)
	completion.Pending = pendingFixture(42, 500, &corpse, false)

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

	// Poison the caller-owned pointed value after
	// publication but before owner execution completes.
	corpse = 666666
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
	live, ok := pendingOf(t, e, tok.EntityID)
	if !ok {
		t.Fatalf("pending absent after ingress apply")
	}
	requirePendingEqual(t, live, PendingDeathRuntime{
		EffectiveCost: 42, DeathTimeSeconds: 500,
		CorpseID: int64ptr(555), PortalUsed: false,
	}, "ingress aliasing")
	stopOwner(t, cancel, done)
}

// TestPendingDeathHandoffRemoval proves pending state
// survives same-entity cell handoff, is discarded on
// removal, and restarts nil on fresh re-add.
func TestPendingDeathHandoffRemoval(t *testing.T) {
	e := newPlayerEngine(t, nil)
	v := testVitals()
	snap, err := e.AddPlayerEntityWithDurableState(testCharacterID(),
		world.Vec3{X: 1, Y: 0, Z: 1}, v, testRuntimeInputs(), testFullDurableState())
	if err != nil {
		t.Fatal(err)
	}
	id := snap.ID
	want := pendingFixture(37, 1234, int64ptr(99), false)
	if err := e.PlayerInstallRecoveredPendingDeath(id, want); err != nil {
		t.Fatal(err)
	}
	// Same-entity handoff preserves pending.
	ent, err := e.registry.lookup(id)
	if err != nil {
		t.Fatal(err)
	}
	destPos := world.Vec3{X: 33.5, Y: 0, Z: 1}
	dest, err := world.CellForPosition(destPos)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := e.registry.beginHandoff(id, OwnerRef{Cell: ent.cell, Generation: ent.generation}, dest, destPos)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.registry.commitHandoff(tok); err != nil {
		t.Fatal(err)
	}
	live, ok := pendingOf(t, e, id)
	if !ok {
		t.Fatalf("pending lost across handoff")
	}
	requirePendingEqual(t, live, PendingDeathRuntime{
		EffectiveCost: 37, DeathTimeSeconds: 1234,
		CorpseID: int64ptr(99), PortalUsed: false,
	}, "handoff")
	// Removal discards it.
	if err := e.RemoveEntity(id); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.PlayerPendingDeathOf(id); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("inspection after removal = %v; want ErrEntityNotFound", err)
	}
	// Fresh re-add starts nil unless hydrated.
	snap2, err := e.AddPlayerEntity(testCharacterID(), world.Vec3{X: 2, Y: 0, Z: 2}, v, testRuntimeInputs())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := pendingOf(t, e, snap2.ID); ok {
		t.Fatalf("fresh re-add carries pending")
	}
}

// TestPendingDeathHydration proves the authoritative
// recovery/hydration seam: active install on an Alive
// player, nil clearing, invalid zero-mutation, and life
// locking.
func TestPendingDeathHydration(t *testing.T) {
	e := newPlayerEngine(t, nil)
	v := testVitals()
	snap, err := e.AddPlayerEntityWithDurableState(testCharacterID(),
		world.Vec3{X: 1, Y: 0, Z: 1}, v, testRuntimeInputs(), testFullDurableState())
	if err != nil {
		t.Fatal(err)
	}
	id := snap.ID
	// Active install with exact values.
	corpse := int64(77)
	if err := e.PlayerInstallRecoveredPendingDeath(id, pendingFixture(55, 900, &corpse, true)); err != nil {
		t.Fatal(err)
	}
	corpse = 1
	live, ok := pendingOf(t, e, id)
	if !ok {
		t.Fatalf("hydrated pending absent")
	}
	requirePendingEqual(t, live, PendingDeathRuntime{
		EffectiveCost: 55, DeathTimeSeconds: 900,
		CorpseID: int64ptr(77), PortalUsed: true,
	}, "hydration")
	// Invalid hydration is zero mutation.
	for _, bad := range []*PendingDeathRuntime{
		pendingFixture(-1, 900, int64ptr(77), true),
		pendingFixture(101, 900, int64ptr(77), false),
		pendingFixture(55, -5, int64ptr(77), false),
		pendingFixture(55, 900, int64ptr(0), false),
	} {
		if err := e.PlayerInstallRecoveredPendingDeath(id, bad); err == nil {
			t.Fatalf("invalid hydration %+v accepted", bad)
		}
		still, ok := pendingOf(t, e, id)
		if !ok {
			t.Fatalf("invalid hydration cleared pending")
		}
		requirePendingEqual(t, still, PendingDeathRuntime{
			EffectiveCost: 55, DeathTimeSeconds: 900,
			CorpseID: int64ptr(77), PortalUsed: true,
		}, "invalid hydration")
	}
	// Nil clears to authoritative "no pending".
	if err := e.PlayerInstallRecoveredPendingDeath(id, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := pendingOf(t, e, id); ok {
		t.Fatalf("nil hydration left pending")
	}
	// Unknown / generic conventions.
	if err := e.PlayerInstallRecoveredPendingDeath(EntityID(9999), pendingFixture(1, 1, nil, false)); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("unknown hydration = %v", err)
	}
	gen, err := e.AddEntity(world.Vec3{X: 3, Y: 0, Z: 3})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.PlayerInstallRecoveredPendingDeath(gen.ID, pendingFixture(1, 1, nil, false)); !errors.Is(err, ErrEntityNotPlayer) {
		t.Fatalf("generic hydration = %v", err)
	}
	// Locked life rejected: DeathPersisting.
	zero := zeroHPVitals(t)
	dying, err := e.AddPlayerEntity(CharacterID(4242), world.Vec3{X: 4, Y: 0, Z: 4}, zero, testRuntimeInputs())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlayerBeginDeathPersistence(dying.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.PlayerInstallRecoveredPendingDeath(dying.ID, pendingFixture(1, 1, nil, false)); !errors.Is(err, ErrPlayerNotAlive) {
		t.Fatalf("DeathPersisting hydration = %v", err)
	}
}

// TestPendingDeathHydrationValidationOrder proves the frozen
// §9.5.1k hydration precedence: entity exists -> RESIDENT
// player -> Alive -> payload validates. A malformed payload
// never masks entity/life errors, and every failure is zero
// mutation.
func TestPendingDeathHydrationValidationOrder(t *testing.T) {
	malformed := &PendingDeathRuntime{
		EffectiveCost:    -1,
		DeathTimeSeconds: -1,
	}

	// Unknown EntityID + malformed payload -> ErrEntityNotFound.
	t.Run("unknown", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		if err := e.PlayerInstallRecoveredPendingDeath(EntityID(9999), malformed); !errors.Is(err, ErrEntityNotFound) {
			t.Fatalf("unknown+malformed = %v; want ErrEntityNotFound", err)
		}
	})

	// Generic entity + malformed payload -> ErrEntityNotPlayer.
	t.Run("generic", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		gen, err := e.AddEntity(world.Vec3{X: 3, Y: 0, Z: 3})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.PlayerInstallRecoveredPendingDeath(gen.ID, malformed); !errors.Is(err, ErrEntityNotPlayer) {
			t.Fatalf("generic+malformed = %v; want ErrEntityNotPlayer", err)
		}
	})

	// MIGRATING player + malformed payload ->
	// ErrCellHandoffRequired with pending unchanged.
	t.Run("migrating", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		snap, err := e.AddPlayerEntity(testCharacterID(),
			world.Vec3{X: 1, Y: 0, Z: 1}, testVitals(), testRuntimeInputs())
		if err != nil {
			t.Fatal(err)
		}
		id := snap.ID
		if err := e.PlayerInstallRecoveredPendingDeath(id, pendingFixture(55, 900, int64ptr(77), true)); err != nil {
			t.Fatal(err)
		}
		live, err := e.Entity(id)
		if err != nil {
			t.Fatal(err)
		}
		dest := world.CellCoord{X: live.Cell.X + 1, Z: live.Cell.Z}
		final := world.Vec3{X: float64(int32(dest.X) * 32), Y: 0, Z: 1}
		if _, err := e.registry.beginHandoff(id, OwnerRef{Cell: live.Cell, Generation: live.OwnershipGeneration}, dest, final); err != nil {
			t.Fatalf("beginHandoff: %v", err)
		}
		if err := e.PlayerInstallRecoveredPendingDeath(id, malformed); !errors.Is(err, ErrCellHandoffRequired) {
			t.Fatalf("migrating+malformed = %v; want ErrCellHandoffRequired", err)
		}
		still, ok := pendingOf(t, e, id)
		if !ok {
			t.Fatalf("migrating hydration cleared pending")
		}
		requirePendingEqual(t, still, PendingDeathRuntime{
			EffectiveCost: 55, DeathTimeSeconds: 900,
			CorpseID: int64ptr(77), PortalUsed: true,
		}, "migrating precedence")
		e.registry.abortHandoff(id)
	})

	// Locked DeathPersisting player + malformed payload ->
	// ErrPlayerNotAlive with pending unchanged.
	t.Run("locked", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		dying, err := e.AddPlayerEntity(CharacterID(4242),
			world.Vec3{X: 4, Y: 0, Z: 4}, zeroHPVitals(t), testRuntimeInputs())
		if err != nil {
			t.Fatal(err)
		}
		id := dying.ID
		if err := e.PlayerInstallRecoveredPendingDeath(id, pendingFixture(55, 900, int64ptr(77), true)); err != nil {
			t.Fatal(err)
		}
		if _, err := e.PlayerBeginDeathPersistence(id); err != nil {
			t.Fatal(err)
		}
		if err := e.PlayerInstallRecoveredPendingDeath(id, malformed); !errors.Is(err, ErrPlayerNotAlive) {
			t.Fatalf("locked+malformed = %v; want ErrPlayerNotAlive", err)
		}
		still, ok := pendingOf(t, e, id)
		if !ok {
			t.Fatalf("locked hydration cleared pending")
		}
		requirePendingEqual(t, still, PendingDeathRuntime{
			EffectiveCost: 55, DeathTimeSeconds: 900,
			CorpseID: int64ptr(77), PortalUsed: true,
		}, "locked precedence")
	})

	// Alive player + malformed payload -> validation error
	// with zero mutation (existing pending unchanged).
	t.Run("alive", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		snap, err := e.AddPlayerEntityWithDurableState(testCharacterID(),
			world.Vec3{X: 1, Y: 0, Z: 1}, testVitals(), testRuntimeInputs(), testFullDurableState())
		if err != nil {
			t.Fatal(err)
		}
		id := snap.ID
		if err := e.PlayerInstallRecoveredPendingDeath(id, pendingFixture(55, 900, int64ptr(77), true)); err != nil {
			t.Fatal(err)
		}
		if err := e.PlayerInstallRecoveredPendingDeath(id, malformed); err == nil {
			t.Fatalf("alive+malformed accepted")
		} else if errors.Is(err, ErrEntityNotFound) || errors.Is(err, ErrEntityNotPlayer) ||
			errors.Is(err, ErrCellHandoffRequired) || errors.Is(err, ErrPlayerNotAlive) {
			t.Fatalf("alive+malformed = %v; want validation error, not entity/life error", err)
		}
		still, ok := pendingOf(t, e, id)
		if !ok {
			t.Fatalf("alive invalid hydration cleared pending")
		}
		requirePendingEqual(t, still, PendingDeathRuntime{
			EffectiveCost: 55, DeathTimeSeconds: 900,
			CorpseID: int64ptr(77), PortalUsed: true,
		}, "alive precedence")
		if st, _ := lifeOf(t, e, id); st != PlayerLifeAlive {
			t.Fatalf("life = %d, want Alive", uint8(st))
		}
	})

	// Alive player + nil -> success, pending cleared.
	t.Run("nil", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		snap, err := e.AddPlayerEntityWithDurableState(testCharacterID(),
			world.Vec3{X: 1, Y: 0, Z: 1}, testVitals(), testRuntimeInputs(), testFullDurableState())
		if err != nil {
			t.Fatal(err)
		}
		id := snap.ID
		if err := e.PlayerInstallRecoveredPendingDeath(id, pendingFixture(55, 900, int64ptr(77), true)); err != nil {
			t.Fatal(err)
		}
		if err := e.PlayerInstallRecoveredPendingDeath(id, nil); err != nil {
			t.Fatalf("nil hydration = %v; want nil", err)
		}
		if _, ok := pendingOf(t, e, id); ok {
			t.Fatalf("nil hydration left pending")
		}
	})
}

// releaseUnderworldPlayer builds an AwaitingRespawn player
// with the given pending state and returns its token.
func releaseUnderworldPlayer(t *testing.T, e *Engine, charID CharacterID, pending *PendingDeathRuntime) (EntityID, DeathAttemptToken) {
	t.Helper()
	id, tok := pendingCompletionPlayer(t, e, charID)
	completion := testDeathCompletion(tok, world.Vec3{X: 5, Y: 0, Z: 7}, testPostDeathVitals(t), testPostDeathDurableState())
	completion.Pending = pending
	if _, disp, err := e.PlayerAcceptImmediateDeathCompletion(completion); err != nil || disp != DeathCompletionApplied {
		t.Fatalf("apply = %d,%v", disp, err)
	}
	return id, tok
}

// TestRespawnReleaseUnderworld proves the first exact-token
// release moves AwaitingRespawn -> Alive preserving
// EVERYTHING else, including bit-identical pending.
func TestRespawnReleaseUnderworld(t *testing.T) {
	e := newPlayerEngine(t, nil)
	id, tok := releaseUnderworldPlayer(t, e, testCharacterID(), pendingFixture(37, 1234, int64ptr(99), false))
	beforeSnap, _ := e.Entity(id)
	beforeVitals, _, _ := e.PlayerVitalsOf(id)
	beforeDurable := durableOf(t, e, id)
	beforePending, _ := pendingOf(t, e, id)
	beforeDeath, _, _ := e.PlayerLastDeathSecondsOf(id)

	snap, disp, err := e.PlayerReleaseRespawn(tok)
	if err != nil {
		t.Fatal(err)
	}
	if disp != RespawnReleaseApplied {
		t.Fatalf("disp = %d, want Applied", disp)
	}
	if st, _ := lifeOf(t, e, id); st != PlayerLifeAlive {
		t.Fatalf("life = %d, want Alive", uint8(st))
	}
	if snap.ID != id || snap.CharacterID != testCharacterID() || snap.Position != beforeSnap.Position {
		t.Fatalf("identity/position changed: %+v", snap)
	}
	if live, _, _ := e.PlayerVitalsOf(id); live != beforeVitals {
		t.Fatalf("vitals changed")
	}
	requireDurableEqual(t, durableOf(t, e, id), beforeDurable, "release")
	live, ok := pendingOf(t, e, id)
	if !ok {
		t.Fatalf("pending cleared by release")
	}
	requirePendingEqual(t, live, beforePending, "release")
	if after, _, _ := e.PlayerLastDeathSecondsOf(id); after != beforeDeath {
		t.Fatalf("lastDeath changed")
	}
}

// TestRespawnReleaseNewbieHome proves a nil-pending
// release lands Alive with nil pending.
func TestRespawnReleaseNewbieHome(t *testing.T) {
	e := newPlayerEngine(t, nil)
	id, tok := releaseUnderworldPlayer(t, e, testCharacterID(), nil)
	if _, disp, err := e.PlayerReleaseRespawn(tok); err != nil || disp != RespawnReleaseApplied {
		t.Fatalf("release = %d,%v", disp, err)
	}
	if st, _ := lifeOf(t, e, id); st != PlayerLifeAlive {
		t.Fatalf("life = %d, want Alive", uint8(st))
	}
	if _, ok := pendingOf(t, e, id); ok {
		t.Fatalf("pending appeared on newbie-home release")
	}
}

// TestRespawnReleaseDuplicate proves a second exact-token
// release is a zero-mutation Duplicate.
func TestRespawnReleaseDuplicate(t *testing.T) {
	e := newPlayerEngine(t, nil)
	id, tok := releaseUnderworldPlayer(t, e, testCharacterID(), pendingFixture(37, 1234, int64ptr(99), false))
	if _, _, err := e.PlayerReleaseRespawn(tok); err != nil {
		t.Fatal(err)
	}
	beforeSnap, _ := e.Entity(id)
	beforePending, _ := pendingOf(t, e, id)
	snap, disp, err := e.PlayerReleaseRespawn(tok)
	if err != nil || disp != RespawnReleaseDuplicate {
		t.Fatalf("second release = %+v,%d,%v; want Duplicate,nil", snap, disp, err)
	}
	if snap != beforeSnap {
		t.Fatalf("duplicate mutated snapshot")
	}
	live, _ := pendingOf(t, e, id)
	requirePendingEqual(t, live, beforePending, "duplicate release")
}

// TestRespawnReleaseMismatchABA proves wrong tokens fail
// closed and the removal/ABA rule holds with no fallback.
func TestRespawnReleaseMismatchABA(t *testing.T) {
	e := newPlayerEngine(t, nil)
	id, tok := releaseUnderworldPlayer(t, e, testCharacterID(), pendingFixture(37, 1234, int64ptr(99), false))
	badTokens := []DeathAttemptToken{
		{EntityID: EntityID(9999), CharacterID: tok.CharacterID, Epoch: tok.Epoch},
		{EntityID: tok.EntityID, CharacterID: CharacterID(31337), Epoch: tok.Epoch},
		{EntityID: tok.EntityID, CharacterID: tok.CharacterID, Epoch: tok.Epoch + 1},
		{EntityID: tok.EntityID, CharacterID: tok.CharacterID, Epoch: 0},
	}
	for i, bad := range badTokens {
		_, _, err := e.PlayerReleaseRespawn(bad)
		if i == 0 {
			if !errors.Is(err, ErrEntityNotFound) {
				t.Fatalf("unknown entity release = %v; want ErrEntityNotFound", err)
			}
			continue
		}
		if !errors.Is(err, ErrDeathAttemptMismatch) {
			t.Fatalf("bad token %d release = %v; want ErrDeathAttemptMismatch", i, err)
		}
	}
	if st, _ := lifeOf(t, e, id); st != PlayerLifeAwaitingRespawn {
		t.Fatalf("life moved by mismatches")
	}
	// DeathPersisting rejects with mismatch.
	e2 := newPlayerEngine(t, nil)
	locked, ltok := pendingCompletionPlayer(t, e2, testCharacterID())
	_ = locked
	if _, _, err := e2.PlayerReleaseRespawn(ltok); !errors.Is(err, ErrDeathAttemptMismatch) {
		t.Fatalf("DeathPersisting release = %v", err)
	}
	// ABA: remove A, add same CharacterID as B, release A
	// token -> ErrEntityNotFound with B unchanged.
	e3 := newPlayerEngine(t, nil)
	aID, aTok := releaseUnderworldPlayer(t, e3, testCharacterID(), pendingFixture(37, 1234, int64ptr(99), false))
	if err := e3.RemoveEntity(aID); err != nil {
		t.Fatal(err)
	}
	v := testVitals()
	bSnap, err := e3.AddPlayerEntity(testCharacterID(), world.Vec3{X: 9, Y: 0, Z: 9}, v, testRuntimeInputs())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e3.PlayerReleaseRespawn(aTok); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("ABA release = %v; want ErrEntityNotFound", err)
	}
	if st, _ := lifeOf(t, e3, bSnap.ID); st != PlayerLifeAlive {
		t.Fatalf("B life moved by ABA release")
	}
	if _, ok := pendingOf(t, e3, bSnap.ID); ok {
		t.Fatalf("B gained pending from ABA release")
	}
}

// TestRespawnReleaseGameplayGate proves release !=
// penalty consumption: before release ordinary
// mutation/input stays rejected; after an
// Underworld-bound release the same operation is
// accepted while pending remains active.
func TestRespawnReleaseGameplayGate(t *testing.T) {
	e := newPlayerEngine(t, nil)
	id, tok := releaseUnderworldPlayer(t, e, testCharacterID(), pendingFixture(37, 1234, int64ptr(99), false))
	move := MoveIntent{InputSeq: 50, HeldDirs: MoveDirForward, RunFlag: 0, Yaw: 100, SampleTick: e.CurrentTick()}
	if _, err := e.SubmitMove(id, move); !errors.Is(err, ErrPlayerNotAlive) {
		t.Fatalf("pre-release SubmitMove = %v; want ErrPlayerNotAlive", err)
	}
	if _, _, err := e.PlayerGainHealthNormal(id, 1); !errors.Is(err, ErrPlayerNotAlive) {
		t.Fatalf("pre-release mutation = %v; want ErrPlayerNotAlive", err)
	}
	if _, disp, err := e.PlayerReleaseRespawn(tok); err != nil || disp != RespawnReleaseApplied {
		t.Fatalf("release = %d,%v", disp, err)
	}
	if d, err := e.SubmitMove(id, move); err != nil || d != MoveAccepted {
		t.Fatalf("post-release SubmitMove = %d,%v; want Accepted,nil", d, err)
	}
	if _, _, err := e.PlayerGainHealthNormal(id, 1); err != nil {
		t.Fatalf("post-release mutation = %v; want nil", err)
	}
	live, ok := pendingOf(t, e, id)
	if !ok {
		t.Fatalf("pending consumed by release")
	}
	requirePendingEqual(t, live, PendingDeathRuntime{
		EffectiveCost: 37, DeathTimeSeconds: 1234,
		CorpseID: int64ptr(99), PortalUsed: false,
	}, "gameplay gate")
}
