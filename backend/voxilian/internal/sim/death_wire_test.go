package sim

import (
	"context"
	"errors"
	"testing"

	"github.com/dlukt/voxilian/internal/world"
)

// M5-T5c4 death wire/state integration owner tests (spec
// §9.5.1l): the typed respawn-release ingress, the atomic
// player-recovery bootstrap, and the death-presentation
// sink emission rules. Deterministic; owner-local
// lifecycle setup runs Step-driven BEFORE Run owns the
// engine, and mailbox admission runs under Run.

// wireAwaitingPlayer builds an AwaitingRespawn player via
// the real owner completion path and returns its id and
// exact token. Call before Run owns the engine.
func wireAwaitingPlayer(t *testing.T, e *Engine, charID CharacterID, pending *PendingDeathRuntime) (EntityID, DeathAttemptToken) {
	t.Helper()
	id, tok := pendingCompletionPlayer(t, e, charID)
	dest := world.Vec3{X: 400, Y: 0, Z: -300}
	comp := testDeathCompletion(tok, dest, testVitals(), testFullDurableState())
	comp.Pending = pending
	if _, disp, err := e.PlayerAcceptImmediateDeathCompletion(comp); err != nil || disp != DeathCompletionApplied {
		t.Fatalf("completion = %v,%v; want Applied,nil", disp, err)
	}
	if st, _ := lifeOf(t, e, id); st != PlayerLifeAwaitingRespawn {
		t.Fatalf("life = %d; want AwaitingRespawn", uint8(st))
	}
	return id, tok
}

func TestPlayerReleaseRespawnOwner(t *testing.T) {
	t.Run("applied-preserves-pending", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		char := testCharacterID()
		id, tok := wireAwaitingPlayer(t, e, char, pendingFixture(40, 1000, int64ptr(9), false))
		before, hasBefore := pendingOf(t, e, id)

		snap, disp, err := e.PlayerReleaseRespawn(tok)
		if err != nil || disp != RespawnReleaseApplied {
			t.Fatalf("release = %v,%v; want Applied,nil", disp, err)
		}
		if snap.ID != id {
			t.Fatalf("snap.ID = %d; want %d", uint64(snap.ID), uint64(id))
		}
		if st, _ := lifeOf(t, e, id); st != PlayerLifeAlive {
			t.Fatalf("life = %d; want Alive", uint8(st))
		}
		after, hasAfter := pendingOf(t, e, id)
		if !hasBefore || !hasAfter {
			t.Fatalf("pending presence = %v/%v; want true/true", hasBefore, hasAfter)
		}
		requirePendingEqual(t, after, before, "release-preserved")
	})

	t.Run("duplicate-idempotent", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		id, tok := wireAwaitingPlayer(t, e, testCharacterID(), nil)
		if _, disp, err := e.PlayerReleaseRespawn(tok); err != nil || disp != RespawnReleaseApplied {
			t.Fatalf("first = %v,%v; want Applied,nil", disp, err)
		}
		snap, disp, err := e.PlayerReleaseRespawn(tok)
		if err != nil || disp != RespawnReleaseDuplicate {
			t.Fatalf("second = %v,%v; want Duplicate,nil", disp, err)
		}
		if snap.ID != id {
			t.Fatalf("dup snap.ID = %d; want %d", uint64(snap.ID), uint64(id))
		}
	})

	t.Run("wrong-entity", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		_, tok := wireAwaitingPlayer(t, e, testCharacterID(), nil)
		tok.EntityID = EntityID(9999)
		if _, _, err := e.PlayerReleaseRespawn(tok); !errors.Is(err, ErrEntityNotFound) {
			t.Fatalf("wrong entity = %v; want ErrEntityNotFound", err)
		}
	})

	t.Run("wrong-character", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		_, tok := wireAwaitingPlayer(t, e, testCharacterID(), nil)
		tok.CharacterID = testCharacterID() + 1
		if _, _, err := e.PlayerReleaseRespawn(tok); !errors.Is(err, ErrDeathAttemptMismatch) {
			t.Fatalf("wrong char = %v; want ErrDeathAttemptMismatch", err)
		}
	})

	t.Run("wrong-epoch", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		_, tok := wireAwaitingPlayer(t, e, testCharacterID(), nil)
		tok.Epoch++
		if _, _, err := e.PlayerReleaseRespawn(tok); !errors.Is(err, ErrDeathAttemptMismatch) {
			t.Fatalf("wrong epoch = %v; want ErrDeathAttemptMismatch", err)
		}
	})

	t.Run("death-persisting-rejects", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		_, tok := pendingCompletionPlayer(t, e, testCharacterID())
		if _, _, err := e.PlayerReleaseRespawn(tok); !errors.Is(err, ErrDeathAttemptMismatch) {
			t.Fatalf("persisting release = %v; want ErrDeathAttemptMismatch", err)
		}
	})

	t.Run("penalty-persisting-rejects", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		id, tok := wireAwaitingPlayer(t, e, testCharacterID(), pendingFixture(30, 700, nil, false))
		ent, err := e.registry.lookup(id)
		if err != nil {
			t.Fatal(err)
		}
		ent.lifeState = PlayerLifeDeathPenaltyPersisting
		if _, _, err := e.PlayerReleaseRespawn(tok); !errors.Is(err, ErrDeathAttemptMismatch) {
			t.Fatalf("penalty-persisting release = %v; want ErrDeathAttemptMismatch", err)
		}
	})

	t.Run("migrating-rejects", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		id, tok := wireAwaitingPlayer(t, e, testCharacterID(), nil)
		live, err := e.Entity(id)
		if err != nil {
			t.Fatal(err)
		}
		dest := world.CellCoord{X: live.Cell.X + 1, Z: live.Cell.Z}
		final := world.Vec3{X: float64(int32(dest.X) * 32), Y: 0, Z: -300}
		if _, err := e.registry.beginHandoff(id, OwnerRef{Cell: live.Cell, Generation: live.OwnershipGeneration}, dest, final); err != nil {
			t.Fatalf("beginHandoff: %v", err)
		}
		if _, _, err := e.PlayerReleaseRespawn(tok); !errors.Is(err, ErrCellHandoffRequired) {
			t.Fatalf("migrating release = %v; want ErrCellHandoffRequired", err)
		}
	})
}

func TestEnqueuePlayerReleaseRespawn(t *testing.T) {
	t.Run("applied-then-duplicate", func(t *testing.T) {
		clk := newManualClock()
		e := ingressEngine(t, clk, openCollision{})
		id, tok := wireAwaitingPlayer(t, e, testCharacterID(), pendingFixture(40, 1000, int64ptr(9), false))
		cancel, done := runOwner(t, e, clk)
		defer stopOwner(t, cancel, done)

		ctx := context.Background()
		if _, disp, err := e.EnqueuePlayerReleaseRespawn(ctx, tok); err != nil || disp != RespawnReleaseApplied {
			t.Fatalf("first = %v,%v; want Applied,nil", disp, err)
		}
		snap, disp, err := e.EnqueuePlayerReleaseRespawn(ctx, tok)
		if err != nil || disp != RespawnReleaseDuplicate {
			t.Fatalf("second = %v,%v; want Duplicate,nil", disp, err)
		}
		if snap.ID != id {
			t.Fatalf("dup snap.ID = %d; want %d", uint64(snap.ID), uint64(id))
		}
		if st, _ := lifeOf(t, e, id); st != PlayerLifeAlive {
			t.Fatalf("life = %d; want Alive", uint8(st))
		}
	})

	t.Run("mailbox-full", func(t *testing.T) {
		col := newBlockingCollision()
		clk := newManualClock()
		e := ingressEngine(t, clk, col)
		cancel, done := runOwner(t, e, clk)
		defer stopOwner(t, cancel, done)

		blockPos := world.Vec3{X: 1}
		release := col.block(blockPos)
		defer close(release)
		first := make(chan ingressAddResult, 1)
		go func() {
			snap, err := e.EnqueueAddEntity(context.Background(), blockPos)
			first <- ingressAddResult{snap: snap, err: err}
		}()
		waitEntered(t, col, blockPos)
		results := make(chan error, SimIngressCapacity)
		for i := 0; i < SimIngressCapacity; i++ {
			go func(i int) {
				_, _, err := e.EnqueuePlayerReleaseRespawn(context.Background(), DeathAttemptToken{
					EntityID:    EntityID(1000 + i),
					CharacterID: testCharacterID(),
					Epoch:       1,
				})
				results <- err
			}(i)
		}
		waitMailboxLen(t, e, SimIngressCapacity)
		if _, _, err := e.EnqueuePlayerReleaseRespawn(context.Background(), DeathAttemptToken{
			EntityID: 1, CharacterID: testCharacterID(), Epoch: 1,
		}); !errors.Is(err, ErrSimIngressFull) {
			t.Fatalf("full mailbox release = %v; want ErrSimIngressFull", err)
		}
	})

	t.Run("not-running", func(t *testing.T) {
		e := ingressEngine(t, newManualClock(), openCollision{})
		tok := DeathAttemptToken{EntityID: 1, CharacterID: testCharacterID(), Epoch: 1}
		if _, _, err := e.EnqueuePlayerReleaseRespawn(context.Background(), tok); !errors.Is(err, ErrEngineNotRunning) {
			t.Fatalf("pre-run release = %v; want ErrEngineNotRunning", err)
		}
	})

	t.Run("stopped-rejects", func(t *testing.T) {
		clk := newManualClock()
		e := ingressEngine(t, clk, openCollision{})
		cancel, done := runOwner(t, e, clk)
		stopOwner(t, cancel, done)
		tok := DeathAttemptToken{EntityID: 1, CharacterID: testCharacterID(), Epoch: 1}
		if _, _, err := e.EnqueuePlayerReleaseRespawn(context.Background(), tok); !errors.Is(err, ErrEngineNotRunning) {
			t.Fatalf("post-stop release = %v; want ErrEngineNotRunning", err)
		}
	})

	t.Run("cancel-before-admission", func(t *testing.T) {
		clk := newManualClock()
		e := ingressEngine(t, clk, openCollision{})
		cancel, done := runOwner(t, e, clk)
		defer stopOwner(t, cancel, done)
		ctx, ccancel := context.WithCancel(context.Background())
		ccancel()
		tok := DeathAttemptToken{EntityID: 1, CharacterID: testCharacterID(), Epoch: 1}
		if _, _, err := e.EnqueuePlayerReleaseRespawn(ctx, tok); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled release = %v; want context.Canceled", err)
		}
		if n := e.EntityCount(); n != 0 {
			t.Fatalf("EntityCount = %d after cancelled admission", n)
		}
	})

	t.Run("cancel-after-admission-completes", func(t *testing.T) {
		col := newBlockingCollision()
		clk := newManualClock()
		e := ingressEngine(t, clk, col)
		_, tok := wireAwaitingPlayer(t, e, testCharacterID(), nil)
		cancel, done := runOwner(t, e, clk)
		defer stopOwner(t, cancel, done)

		// Park the owner on a blocked add so the release
		// below is deterministically ADMITTED but not yet
		// executed; cancelling afterwards must not retract
		// it.
		blockPos := world.Vec3{X: 77}
		unblock := col.block(blockPos)
		addRes := make(chan ingressAddResult, 1)
		go func() {
			snap, err := e.EnqueueAddEntity(context.Background(), blockPos)
			addRes <- ingressAddResult{snap: snap, err: err}
		}()
		waitEntered(t, col, blockPos)
		ctx, ccancel := context.WithCancel(context.Background())
		type out struct {
			disp RespawnReleaseDisposition
			err  error
		}
		res := make(chan out, 1)
		go func() {
			_, disp, err := e.EnqueuePlayerReleaseRespawn(ctx, tok)
			res <- out{disp: disp, err: err}
		}()
		waitMailboxLen(t, e, 1)
		ccancel()
		close(unblock)
		got := <-res
		if got.err != nil || got.disp != RespawnReleaseApplied {
			t.Fatalf("post-admission cancel = %v,%v; want Applied,nil", got.disp, got.err)
		}
	})
}

func TestDeathDisconnectDuringPersisting(t *testing.T) {
	// The old entity disappears (disconnect/takeover)
	// while DeathPersisting: a late persistence
	// completion for the old token must fail closed
	// without touching the replacement entity, and
	// durable state stays authoritative for the next
	// fresh reconnect.
	e := newPlayerEngine(t, nil)
	char := testCharacterID()
	oldID, oldTok := pendingCompletionPlayer(t, e, char)
	if err := e.RemoveEntity(oldID); err != nil {
		t.Fatalf("RemoveEntity: %v", err)
	}
	comp := testDeathCompletion(oldTok, world.Vec3{X: 400}, testVitals(), testFullDurableState())
	if _, _, err := e.PlayerAcceptImmediateDeathCompletion(comp); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("old completion after removal = %v; want ErrEntityNotFound", err)
	}
	// A replacement entity for the same character
	// starts clean: it carries a fresh EntityID (IDs
	// are never reused, so the old token cannot even
	// address it) and a zero-HP begin works with a
	// fresh epoch.
	v := zeroHPVitals(t)
	newSnap, err := e.AddPlayerEntity(char, world.Vec3{X: 9, Y: 0, Z: 9}, v, testRuntimeInputs())
	if err != nil {
		t.Fatalf("replacement add: %v", err)
	}
	if newSnap.ID == oldID {
		t.Fatalf("replacement reused EntityID %d", uint64(oldID))
	}
	if _, _, err := e.PlayerReleaseRespawn(oldTok); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("old token vs replacement = %v; want ErrEntityNotFound", err)
	}
	newTok, err := e.PlayerBeginDeathPersistence(newSnap.ID)
	if err != nil {
		t.Fatalf("replacement begin: %v", err)
	}
	if newTok.Epoch == oldTok.Epoch && newTok.EntityID == oldTok.EntityID {
		t.Fatalf("replacement token %+v aliases removed attempt %+v", newTok, oldTok)
	}
	if newTok.EntityID != newSnap.ID {
		t.Fatalf("replacement token entity = %d; want %d", uint64(newTok.EntityID), uint64(newSnap.ID))
	}
}

func TestAddPlayerEntityWithRecovery(t *testing.T) {
	bootFixture := func(charID CharacterID, pending *PendingDeathRuntime) PlayerRecoveryBootstrap {
		return PlayerRecoveryBootstrap{
			CharacterID:   charID,
			Position:      world.Vec3{X: 10, Y: 0, Z: -20},
			Vitals:        testVitals(),
			RuntimeInputs: testRuntimeInputs(),
			Durable:       testFullDurableState(),
			Pending:       pending,
		}
	}

	t.Run("full-with-pending-atomic", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		boot := bootFixture(testCharacterID(), pendingFixture(60, 1200, int64ptr(41), true))
		snap, err := e.AddPlayerEntityWithRecovery(boot)
		if err != nil {
			t.Fatalf("AddPlayerEntityWithRecovery: %v", err)
		}
		if !snap.IsPlayer || snap.CharacterID != testCharacterID() {
			t.Fatalf("snap = %+v; want player char %d", snap, int64(testCharacterID()))
		}
		// No separate hydrate step ran: pending is already
		// live immediately after the single add.
		got, ok, err := e.PlayerPendingDeathOf(snap.ID)
		if err != nil || !ok {
			t.Fatalf("pending = %+v,%v,%v; want present", got, ok, err)
		}
		requirePendingEqual(t, got, *boot.Pending, "bootstrap")
		if st, _ := lifeOf(t, e, snap.ID); st != PlayerLifeAlive {
			t.Fatalf("life = %d; want Alive", uint8(st))
		}
	})

	t.Run("nil-pending", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		snap, err := e.AddPlayerEntityWithRecovery(bootFixture(testCharacterID(), nil))
		if err != nil {
			t.Fatalf("AddPlayerEntityWithRecovery: %v", err)
		}
		if _, ok, err := e.PlayerPendingDeathOf(snap.ID); err != nil || ok {
			t.Fatalf("pending present = %v,%v; want false,nil", ok, err)
		}
	})

	t.Run("invalid-pending-consumes-no-entity", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		bad := bootFixture(testCharacterID(), &PendingDeathRuntime{EffectiveCost: 101})
		if _, err := e.AddPlayerEntityWithRecovery(bad); err == nil {
			t.Fatalf("invalid pending accepted")
		}
		if n := e.EntityCount(); n != 0 {
			t.Fatalf("EntityCount = %d after rejected bootstrap", n)
		}
		snap, err := e.AddPlayerEntityWithRecovery(bootFixture(testCharacterID(), nil))
		if err != nil {
			t.Fatalf("retry: %v", err)
		}
		if snap.ID != EntityID(1) {
			t.Fatalf("first ID = %d; want 1 (no consumption on failure)", uint64(snap.ID))
		}
	})

	t.Run("invalid-durable-consumes-no-entity", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		boot := bootFixture(testCharacterID(), nil)
		boot.Durable.Advancement = []byte(`[1,2`)
		if _, err := e.AddPlayerEntityWithRecovery(boot); err == nil {
			t.Fatalf("invalid durable accepted")
		}
		if n := e.EntityCount(); n != 0 {
			t.Fatalf("EntityCount = %d after rejected bootstrap", n)
		}
	})

	t.Run("invalid-vitals-consumes-no-entity", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		boot := bootFixture(testCharacterID(), nil)
		boot.Vitals.HP = -1
		if _, err := e.AddPlayerEntityWithRecovery(boot); err == nil {
			t.Fatalf("invalid vitals accepted")
		}
		if n := e.EntityCount(); n != 0 {
			t.Fatalf("EntityCount = %d after rejected bootstrap", n)
		}
	})

	t.Run("duplicate-character-keeps-first", func(t *testing.T) {
		e := newPlayerEngine(t, nil)
		first, err := e.AddPlayerEntityWithRecovery(bootFixture(testCharacterID(), pendingFixture(25, 500, nil, false)))
		if err != nil {
			t.Fatalf("first: %v", err)
		}
		second := bootFixture(testCharacterID(), nil)
		second.Position = world.Vec3{X: 90, Y: 0, Z: 90}
		if _, err := e.AddPlayerEntityWithRecovery(second); !errors.Is(err, ErrCharacterAlreadyActive) {
			t.Fatalf("duplicate char = %v; want ErrCharacterAlreadyActive", err)
		}
		got, ok, err := e.PlayerPendingDeathOf(first.ID)
		if err != nil || !ok || got.EffectiveCost != 25 {
			t.Fatalf("first pending disturbed: %+v,%v,%v", got, ok, err)
		}
	})

	t.Run("ingress/atomic-with-pending", func(t *testing.T) {
		clk := newManualClock()
		e := ingressEngine(t, clk, openCollision{})
		cancel, done := runOwner(t, e, clk)
		defer stopOwner(t, cancel, done)
		boot := bootFixture(testCharacterID(), pendingFixture(70, 1300, int64ptr(55), false))
		snap, err := e.EnqueueAddPlayerEntityWithRecovery(context.Background(), boot)
		if err != nil {
			t.Fatalf("ingress bootstrap: %v", err)
		}
		got, ok, err := e.PlayerPendingDeathOf(snap.ID)
		if err != nil || !ok {
			t.Fatalf("pending = %+v,%v,%v; want present", got, ok, err)
		}
		requirePendingEqual(t, got, *boot.Pending, "ingress bootstrap")
	})

	t.Run("ingress/not-running", func(t *testing.T) {
		e := ingressEngine(t, newManualClock(), openCollision{})
		boot := bootFixture(testCharacterID(), nil)
		if _, err := e.EnqueueAddPlayerEntityWithRecovery(context.Background(), boot); !errors.Is(err, ErrEngineNotRunning) {
			t.Fatalf("pre-run bootstrap = %v; want ErrEngineNotRunning", err)
		}
	})

	t.Run("ingress/freezes-payload", func(t *testing.T) {
		clk := newManualClock()
		e := ingressEngine(t, clk, openCollision{})
		cancel, done := runOwner(t, e, clk)
		defer stopOwner(t, cancel, done)
		corpse := int64(61)
		boot := bootFixture(testCharacterID(), &PendingDeathRuntime{EffectiveCost: 10, DeathTimeSeconds: 5, CorpseID: &corpse})
		snap, err := e.EnqueueAddPlayerEntityWithRecovery(context.Background(), boot)
		if err != nil {
			t.Fatalf("ingress bootstrap: %v", err)
		}
		// Hostile caller mutation after submission must not
		// reach live state.
		corpse = 9999
		boot.Durable.Spells = append(boot.Durable.Spells, PlayerAbilityState{ID: 999, Ability: 50})
		got, ok, err := e.PlayerPendingDeathOf(snap.ID)
		if err != nil || !ok || got.CorpseID == nil || *got.CorpseID != 61 {
			t.Fatalf("pending aliased caller memory: %+v,%v,%v", got, ok, err)
		}
	})
}

// recordingDeathSink is a test DeathPresentationSink.
type recordingDeathSink struct {
	begins     []DeathBeginEvent
	completeds []DeathCompletedEvent
}

func (s *recordingDeathSink) OnDeathBegin(ev DeathBeginEvent) { s.begins = append(s.begins, ev) }

func (s *recordingDeathSink) OnDeathCompleted(ev DeathCompletedEvent) {
	s.completeds = append(s.completeds, ev)
}

func TestDeathPresentationSink(t *testing.T) {
	sinkEngine := func(t *testing.T, sink DeathPresentationSink) *Engine {
		t.Helper()
		return mustEngine(t, 20, EngineDeps{
			Clock: newManualClock(), RNG: newTestRNG(7), Collision: openCollision{},
			RunGate: staticGate{allow: true}, Death: sink,
		})
	}

	t.Run("begin-emits-once-with-token", func(t *testing.T) {
		sink := &recordingDeathSink{}
		e := sinkEngine(t, sink)
		id, tok := pendingCompletionPlayer(t, e, testCharacterID())
		if len(sink.begins) != 1 {
			t.Fatalf("begins = %d; want 1", len(sink.begins))
		}
		if sink.begins[0].Token != tok {
			t.Fatalf("begin token = %+v; want %+v", sink.begins[0].Token, tok)
		}
		// A second begin while DeathPersisting fails and
		// emits nothing more.
		if _, err := e.PlayerBeginDeathPersistence(id); err == nil {
			t.Fatalf("second begin accepted")
		}
		if len(sink.begins) != 1 {
			t.Fatalf("begins = %d after failed begin; want 1", len(sink.begins))
		}
		if len(sink.completeds) != 0 {
			t.Fatalf("completeds = %d before completion; want 0", len(sink.completeds))
		}
	})

	t.Run("applied-completion-emits-snapshot", func(t *testing.T) {
		sink := &recordingDeathSink{}
		e := sinkEngine(t, sink)
		id, tok := pendingCompletionPlayer(t, e, testCharacterID())
		dest := world.Vec3{X: 400, Y: 0, Z: -300}
		comp := testDeathCompletion(tok, dest, testVitals(), testFullDurableState())
		snap, disp, err := e.PlayerAcceptImmediateDeathCompletion(comp)
		if err != nil || disp != DeathCompletionApplied {
			t.Fatalf("completion = %v,%v; want Applied,nil", disp, err)
		}
		if len(sink.completeds) != 1 {
			t.Fatalf("completeds = %d; want 1", len(sink.completeds))
		}
		got := sink.completeds[0]
		if got.Token != tok || got.Snapshot.ID != id || got.Snapshot.Position != dest {
			t.Fatalf("completed event = %+v; want token %+v id %d pos %v", got, tok, uint64(id), dest)
		}
		if snap.ID != id {
			t.Fatalf("snap.ID = %d; want %d", uint64(snap.ID), uint64(id))
		}
	})

	t.Run("duplicate-completion-emits-nothing", func(t *testing.T) {
		sink := &recordingDeathSink{}
		e := sinkEngine(t, sink)
		id, tok := pendingCompletionPlayer(t, e, testCharacterID())
		comp := testDeathCompletion(tok, world.Vec3{X: 400}, testVitals(), testFullDurableState())
		if _, _, err := e.PlayerAcceptImmediateDeathCompletion(comp); err != nil {
			t.Fatalf("first: %v", err)
		}
		if len(sink.completeds) != 1 {
			t.Fatalf("completeds = %d; want 1", len(sink.completeds))
		}
		if _, disp, err := e.PlayerAcceptImmediateDeathCompletion(comp); err != nil || disp != DeathCompletionDuplicate {
			t.Fatalf("second = %v,%v; want Duplicate,nil", disp, err)
		}
		if len(sink.completeds) != 1 {
			t.Fatalf("completeds = %d after duplicate; want 1", len(sink.completeds))
		}
		_ = id
	})

	t.Run("failed-completion-emits-nothing", func(t *testing.T) {
		sink := &recordingDeathSink{}
		e := sinkEngine(t, sink)
		id, tok := pendingCompletionPlayer(t, e, testCharacterID())
		comp := testDeathCompletion(tok, world.Vec3{X: 400}, testVitals(), testFullDurableState())
		comp.Vitals.HP = -5
		if _, _, err := e.PlayerAcceptImmediateDeathCompletion(comp); err == nil {
			t.Fatalf("bad completion accepted")
		}
		if len(sink.completeds) != 0 {
			t.Fatalf("completeds = %d after failure; want 0", len(sink.completeds))
		}
		_ = id
	})

	t.Run("nil-sink-safe", func(t *testing.T) {
		e := sinkEngine(t, nil)
		id, tok := pendingCompletionPlayer(t, e, testCharacterID())
		comp := testDeathCompletion(tok, world.Vec3{X: 400}, testVitals(), testFullDurableState())
		if _, _, err := e.PlayerAcceptImmediateDeathCompletion(comp); err != nil {
			t.Fatalf("completion without sink: %v", err)
		}
		_ = id
	})
}
