package sim

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/dlukt/voxilian/internal/world"
)

// M5-T5c3c1 immediate-death lifecycle gate + attempt correlation
// tests (spec §9.5.1f): owner-local PlayerBeginDeathPersistence +
// PlayerAcceptPostDeathState with the PlayerLifeState gate.
// Deterministic, no sleeps, no wall clock, no Store/persist/
// gateway/proto involvement.

// zeroHPVitals is a Validate-valid canonical value with HP == 0.
func zeroHPVitals(t *testing.T) PlayerVitals {
	t.Helper()
	v := testVitals()
	v.HP = 0
	if err := v.Validate(); err != nil {
		t.Fatalf("zeroHPVitals invalid: %v", err)
	}
	return v
}

// prepareDeathGatePlayer builds a resident zero-HP player with rich
// live state: a processed movement anchor (seq 1), a newer
// accepted-but-unprocessed pending move (seq 2), nonzero speed,
// armed mana/rest deadlines (health can never arm at HP 0),
// actedSinceEntry true, nonempty position history, and a seeded
// recent-OpID cache.
func prepareDeathGatePlayer(t *testing.T, e *Engine, charID CharacterID) EntityID {
	t.Helper()
	v := zeroHPVitals(t)
	v.Mana = 5
	if err := v.Validate(); err != nil {
		t.Fatalf("death-gate vitals invalid: %v", err)
	}
	snap, err := e.AddPlayerEntity(charID, world.Vec3{X: 1, Y: 0, Z: 1}, v, testRuntimeInputs())
	if err != nil {
		t.Fatalf("AddPlayerEntity: %v", err)
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

// lifeOf is a fatal-on-error PlayerLifeStateOf wrapper.
func lifeOf(t *testing.T, e *Engine, id EntityID) (PlayerLifeState, bool) {
	t.Helper()
	st, ok, err := e.PlayerLifeStateOf(id)
	if err != nil {
		t.Fatalf("PlayerLifeStateOf(%d): %v", uint64(id), err)
	}
	return st, ok
}

// beginDeath is a fatal-on-error PlayerBeginDeathPersistence wrapper.
func beginDeath(t *testing.T, e *Engine, id EntityID) DeathAttemptToken {
	t.Helper()
	tok, err := e.PlayerBeginDeathPersistence(id)
	if err != nil {
		t.Fatalf("PlayerBeginDeathPersistence(%d): %v", uint64(id), err)
	}
	return tok
}

// acceptDeath is a fatal-on-error PlayerAcceptPostDeathState wrapper.
func acceptDeath(t *testing.T, e *Engine, tok DeathAttemptToken, pos world.Vec3, v PlayerVitals) (EntitySnapshot, DeathCompletionDisposition) {
	t.Helper()
	snap, disp, err := e.PlayerAcceptPostDeathState(tok, pos, v, testRuntimeInputs())
	if err != nil {
		t.Fatalf("PlayerAcceptPostDeathState(%+v): %v", tok, err)
	}
	return snap, disp
}

// TestPlayerLifeInitialState proves every created/attached player
// starts Alive, generic entities report no meaningful state, and
// handoff preserves the Alive state.
func TestPlayerLifeInitialState(t *testing.T) {
	e := newPlayerEngine(t, nil)

	snap, err := e.AddPlayerEntity(testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1}, testVitals(), testRuntimeInputs())
	if err != nil {
		t.Fatalf("AddPlayerEntity: %v", err)
	}
	if st, ok := lifeOf(t, e, snap.ID); !ok || st != PlayerLifeAlive {
		t.Fatalf("added player life = %d,%v; want Alive,true", uint8(st), ok)
	}

	gen, err := e.AddEntity(world.Vec3{X: 2, Y: 0, Z: 2})
	if err != nil {
		t.Fatalf("AddEntity: %v", err)
	}
	if st, ok := lifeOf(t, e, gen.ID); ok || st != PlayerLifeAlive {
		t.Fatalf("generic life = %d,%v; want zero,false", uint8(st), ok)
	}

	attach, err := e.AddEntity(world.Vec3{X: 3, Y: 0, Z: 3})
	if err != nil {
		t.Fatalf("AddEntity: %v", err)
	}
	if err := e.AttachPlayerVitals(attach.ID, CharacterID(21), testVitals(), testRuntimeInputs()); err != nil {
		t.Fatalf("AttachPlayerVitals: %v", err)
	}
	if st, ok := lifeOf(t, e, attach.ID); !ok || st != PlayerLifeAlive {
		t.Fatalf("attached player life = %d,%v; want Alive,true", uint8(st), ok)
	}

	// Handoff preserves the Alive state (same entity object moves).
	near, err := e.AddPlayerEntity(CharacterID(22), world.Vec3{X: 31.9, Y: 0, Z: 16}, testVitals(), testRuntimeInputs())
	if err != nil {
		t.Fatalf("AddPlayerEntity: %v", err)
	}
	submitMove(t, e, near.ID, 1, MoveDirForward, 0, 1024)
	e.Step()
	if got, err := e.Entity(near.ID); err != nil || got.Cell != (world.CellCoord{X: 1, Z: 0}) {
		t.Fatalf("handoff = %+v,%v; want cell {1 0}", got, err)
	}
	if st, ok := lifeOf(t, e, near.ID); !ok || st != PlayerLifeAlive {
		t.Fatalf("post-handoff life = %d,%v; want Alive,true", uint8(st), ok)
	}

	if _, _, err := e.PlayerLifeStateOf(EntityID(999)); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("unknown life err = %v, want ErrEntityNotFound", err)
	}
}

// TestPlayerBeginDeathSuccess proves the begin transition: exact
// token, DeathPersisting life, and every T5c3b quiesce invariant.
func TestPlayerBeginDeathSuccess(t *testing.T) {
	rec := &vitalsRecorder{}
	e := newPlayerEngine(t, rec)
	id := prepareDeathGatePlayer(t, e, testCharacterID())

	before := entOf(t, e, id)
	wantPos, wantCell, wantGen := before.position, before.cell, before.generation
	wantYaw := before.yaw
	wantVitals := before.vitals
	wantInputs := before.runtimeInputs
	wantAnchor := before.stomachAnchorTick
	histBefore, err := e.History(id)
	if err != nil || len(histBefore) == 0 {
		t.Fatalf("setup: history len=%d err=%v, want nonempty", len(histBefore), err)
	}
	if !before.hasPending || before.lastAcceptedSeq != 2 {
		t.Fatalf("setup: pending=%v accepted=%d, want true/2", before.hasPending, before.lastAcceptedSeq)
	}
	if !before.manaArmed || !before.restArmed {
		t.Fatalf("setup: mana=%v rest=%v, want armed", before.manaArmed, before.restArmed)
	}

	tok := beginDeath(t, e, id)
	if tok.EntityID != id || tok.CharacterID != testCharacterID() || tok.Epoch != 1 {
		t.Fatalf("token = %+v, want {%d %d 1}", tok, uint64(id), int64(testCharacterID()))
	}
	if st, ok := lifeOf(t, e, id); !ok || st != PlayerLifeDeathPersisting {
		t.Fatalf("life = %d,%v; want DeathPersisting,true", uint8(st), ok)
	}

	after := entOf(t, e, id)
	if after.activeHeldDirs != 0 || after.activeRun || after.speed != 0 {
		t.Fatalf("movement not stopped: dirs=%d run=%v speed=%d",
			after.activeHeldDirs, after.activeRun, after.speed)
	}
	if after.hasPending || after.pending != (MoveIntent{}) {
		t.Fatalf("pending not cleared: %+v", after.pending)
	}
	if after.healthArmed || after.manaArmed || after.restArmed {
		t.Fatalf("deadlines still armed: health=%v mana=%v rest=%v",
			after.healthArmed, after.manaArmed, after.restArmed)
	}
	if after.healthDue != 0 || after.manaDue != 0 || after.restDue != 0 {
		t.Fatalf("dues not canonical zero: %d/%d/%d", after.healthDue, after.manaDue, after.restDue)
	}
	if after.actedSinceEntry {
		t.Fatalf("actedSinceEntry still true")
	}
	if after.position != wantPos || after.cell != wantCell || after.generation != wantGen {
		t.Fatalf("ownership moved")
	}
	if after.id != id || after.characterID != testCharacterID() {
		t.Fatalf("identity changed")
	}
	if after.yaw != wantYaw {
		t.Fatalf("yaw = %d, want %d", after.yaw, wantYaw)
	}
	if !after.hasAccepted || after.lastAcceptedSeq != 2 {
		t.Fatalf("accepted frontier = %v/%d, want true/2", after.hasAccepted, after.lastAcceptedSeq)
	}
	if !after.hasProcessed || after.lastProcessedSeq != 1 {
		t.Fatalf("processed frontier = %v/%d, want true/1", after.hasProcessed, after.lastProcessedSeq)
	}
	if after.vitals != wantVitals {
		t.Fatalf("vitals changed: %+v, want %+v", after.vitals, wantVitals)
	}
	if after.runtimeInputs != wantInputs {
		t.Fatalf("runtime inputs changed")
	}
	if after.stomachAnchorTick != wantAnchor {
		t.Fatalf("stomach anchor = %d, want %d", after.stomachAnchorTick, wantAnchor)
	}
	if after.deathEpoch != 1 {
		t.Fatalf("epoch = %d, want 1", after.deathEpoch)
	}
	histAfter, err := e.History(id)
	if err != nil || len(histAfter) != len(histBefore) {
		t.Fatalf("history changed: len=%d err=%v, want %d", len(histAfter), err, len(histBefore))
	}
	for i := range histBefore {
		if histAfter[i] != histBefore[i] {
			t.Fatalf("history[%d] changed", i)
		}
	}
	if after.recentOps == nil || !after.recentOps.contains(OpID(7)) || !after.recentOps.contains(OpID(9)) {
		t.Fatalf("recent OpIDs not preserved")
	}
	if rec.count() != 0 {
		t.Fatalf("vitals observer fired %d events, want 0", rec.count())
	}
}

// lifeProbe captures the full observable lifecycle state for
// zero-mutation comparison.
type lifeProbe struct {
	snap    EntitySnapshot
	vitals  PlayerVitals
	runtime PlayerVitalsRuntimeSnapshot
	life    PlayerLifeState
	epoch   uint64
	hist    []PositionSample
}

func captureLifeProbe(t *testing.T, e *Engine, id EntityID) lifeProbe {
	t.Helper()
	snap, err := e.Entity(id)
	if err != nil {
		t.Fatalf("Entity: %v", err)
	}
	v, ok, err := e.PlayerVitalsOf(id)
	if err != nil || !ok {
		t.Fatalf("PlayerVitalsOf = %+v,%v,%v", v, ok, err)
	}
	rt, ok, err := e.PlayerVitalsRuntimeOf(id)
	if err != nil || !ok {
		t.Fatalf("PlayerVitalsRuntimeOf = %+v,%v,%v", rt, ok, err)
	}
	st, ok, err := e.PlayerLifeStateOf(id)
	if err != nil || !ok {
		t.Fatalf("PlayerLifeStateOf = %d,%v,%v", uint8(st), ok, err)
	}
	hist, err := e.History(id)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	return lifeProbe{
		snap: snap, vitals: v, runtime: rt,
		life: st, epoch: entOf(t, e, id).deathEpoch, hist: hist,
	}
}

func requireLifeProbeUnchanged(t *testing.T, e *Engine, id EntityID, want lifeProbe, what string) {
	t.Helper()
	got := captureLifeProbe(t, e, id)
	if got.snap != want.snap || got.vitals != want.vitals || got.runtime != want.runtime ||
		got.life != want.life || got.epoch != want.epoch || len(got.hist) != len(want.hist) {
		t.Fatalf("%s mutated state:\ngot  %+v\nwant %+v", what, got, want)
	}
	for i := range want.hist {
		if got.hist[i] != want.hist[i] {
			t.Fatalf("%s mutated history[%d]", what, i)
		}
	}
}

// TestPlayerBeginDeathFailures proves the begin failure matrix with
// zero mutation and the exact stable errors.
func TestPlayerBeginDeathFailures(t *testing.T) {
	e := newPlayerEngine(t, nil)

	// Unknown entity.
	if _, err := e.PlayerBeginDeathPersistence(EntityID(999)); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("unknown err = %v, want ErrEntityNotFound", err)
	}

	// Generic entity.
	gen, err := e.AddEntity(world.Vec3{X: 2, Y: 0, Z: 2})
	if err != nil {
		t.Fatalf("AddEntity: %v", err)
	}
	genSnap, err := e.Entity(gen.ID)
	if err != nil {
		t.Fatalf("Entity: %v", err)
	}
	if _, err := e.PlayerBeginDeathPersistence(gen.ID); !errors.Is(err, ErrEntityNotPlayer) {
		t.Fatalf("generic err = %v, want ErrEntityNotPlayer", err)
	}
	if after, err := e.Entity(gen.ID); err != nil || after != genSnap {
		t.Fatalf("generic mutated: %+v,%v", after, err)
	}

	// MIGRATING player.
	mig, err := e.AddPlayerEntity(CharacterID(11), world.Vec3{X: 3, Y: 0, Z: 3}, zeroHPVitals(t), testRuntimeInputs())
	if err != nil {
		t.Fatalf("AddPlayerEntity: %v", err)
	}
	migBefore, err := e.Entity(mig.ID)
	if err != nil {
		t.Fatalf("Entity: %v", err)
	}
	beginTestMigration(t, e, mig.ID, world.CellCoord{X: 1, Z: 0}, world.Vec3{X: 40, Y: 0, Z: 3})
	migBefore, err = e.Entity(mig.ID)
	if err != nil {
		t.Fatalf("Entity(migrating): %v", err)
	}
	if _, err := e.PlayerBeginDeathPersistence(mig.ID); !errors.Is(err, ErrCellHandoffRequired) {
		t.Fatalf("migrating err = %v, want ErrCellHandoffRequired", err)
		e.registry.abortHandoff(mig.ID)
	}
	if after, err := e.Entity(mig.ID); err != nil || after != migBefore {
		t.Fatalf("migrating mutated: %+v vs %+v", after, migBefore)
	}
	e.registry.abortHandoff(mig.ID)

	// Alive player with HP != 0.
	alive, err := e.AddPlayerEntity(CharacterID(12), world.Vec3{X: 4, Y: 0, Z: 4}, testVitals(), testRuntimeInputs())
	if err != nil {
		t.Fatalf("AddPlayerEntity: %v", err)
	}
	aliveProbe := captureLifeProbe(t, e, alive.ID)
	if _, err := e.PlayerBeginDeathPersistence(alive.ID); !errors.Is(err, ErrPlayerNotDead) {
		t.Fatalf("alive HP!=0 err = %v, want ErrPlayerNotDead", err)
	}
	requireLifeProbeUnchanged(t, e, alive.ID, aliveProbe, "HP!=0 begin")

	// Already DeathPersisting player.
	dying := prepareDeathGatePlayer(t, e, CharacterID(13))
	dyingTok := beginDeath(t, e, dying)
	dyingProbe := captureLifeProbe(t, e, dying)
	if _, err := e.PlayerBeginDeathPersistence(dying); !errors.Is(err, ErrPlayerNotAlive) {
		t.Fatalf("persisting err = %v, want ErrPlayerNotAlive", err)
	}
	requireLifeProbeUnchanged(t, e, dying, dyingProbe, "double begin")
	if entOf(t, e, dying).deathEpoch != dyingTok.Epoch {
		t.Fatalf("double begin advanced the epoch")
	}

	// AwaitingRespawn player.
	respawnPos := world.Vec3{X: 5, Y: 0, Z: 7}
	if _, _, err := e.PlayerAcceptPostDeathState(dyingTok, respawnPos, testVitals(), testRuntimeInputs()); err != nil {
		t.Fatalf("accept: %v", err)
	}
	respawnProbe := captureLifeProbe(t, e, dying)
	if _, err := e.PlayerBeginDeathPersistence(dying); !errors.Is(err, ErrPlayerNotAlive) {
		t.Fatalf("awaiting err = %v, want ErrPlayerNotAlive", err)
	}
	requireLifeProbeUnchanged(t, e, dying, respawnProbe, "awaiting begin")

	// Epoch exhaustion.
	exh, err := e.AddPlayerEntity(CharacterID(14), world.Vec3{X: 6, Y: 0, Z: 6}, zeroHPVitals(t), testRuntimeInputs())
	if err != nil {
		t.Fatalf("AddPlayerEntity: %v", err)
	}
	entOf(t, e, exh.ID).deathEpoch = math.MaxUint64
	exhProbe := captureLifeProbe(t, e, exh.ID)
	if _, err := e.PlayerBeginDeathPersistence(exh.ID); !errors.Is(err, ErrDeathAttemptExhausted) {
		t.Fatalf("exhausted err = %v, want ErrDeathAttemptExhausted", err)
	}
	requireLifeProbeUnchanged(t, e, exh.ID, exhProbe, "exhausted begin")
	if entOf(t, e, exh.ID).deathEpoch != math.MaxUint64 {
		t.Fatalf("exhausted begin wrapped the epoch")
	}
}

// setupLockedPlayer builds a player locked in the requested state
// with pre-lock sequence anchors (accepted/processed seq 1) and
// returns its pre-lock position.
func setupLockedPlayer(t *testing.T, e *Engine, charID CharacterID, want PlayerLifeState) (EntityID, world.Vec3) {
	t.Helper()
	snap, err := e.AddPlayerEntity(charID, world.Vec3{X: 1, Y: 0, Z: 1}, zeroHPVitals(t), testRuntimeInputs())
	if err != nil {
		t.Fatalf("AddPlayerEntity: %v", err)
	}
	id := snap.ID
	submitMove(t, e, id, 1, MoveDirForward, 0, 0)
	e.Step()
	tok := beginDeath(t, e, id)
	if want == PlayerLifeAwaitingRespawn {
		acceptDeath(t, e, tok, world.Vec3{X: 5, Y: 0, Z: 7}, testVitals())
	}
	if st, _ := lifeOf(t, e, id); st != want {
		t.Fatalf("setup life = %d, want %d", uint8(st), uint8(want))
	}
	locked, err := e.Entity(id)
	if err != nil {
		t.Fatalf("Entity: %v", err)
	}
	return id, locked.Position
}

// requireMoveAnchorsUnchanged proves a rejected input consumed no
// InputSeq and mutated no movement state.
func requireMoveAnchorsUnchanged(t *testing.T, e *Engine, id EntityID, wantAccepted uint32, wantPos world.Vec3, what string) {
	t.Helper()
	ent := entOf(t, e, id)
	if !ent.hasAccepted || ent.lastAcceptedSeq != wantAccepted {
		t.Fatalf("%s: accepted = %v/%d, want true/%d", what, ent.hasAccepted, ent.lastAcceptedSeq, wantAccepted)
	}
	if !ent.hasProcessed || ent.lastProcessedSeq != 1 {
		t.Fatalf("%s: processed = %v/%d, want true/1", what, ent.hasProcessed, ent.lastProcessedSeq)
	}
	if ent.hasPending {
		t.Fatalf("%s: pending control created", what)
	}
	if ent.position != wantPos {
		t.Fatalf("%s: position = %+v, want %+v", what, ent.position, wantPos)
	}
}

// TestLockedPlayerSubmitMoveRejects proves SubmitMove rejection in
// both locked states with untouched anchors.
func TestLockedPlayerSubmitMoveRejects(t *testing.T) {
	for _, want := range []PlayerLifeState{PlayerLifeDeathPersisting, PlayerLifeAwaitingRespawn} {
		e := newPlayerEngine(t, nil)
		id, prePos := setupLockedPlayer(t, e, testCharacterID(), want)
		what := map[PlayerLifeState]string{
			PlayerLifeDeathPersisting: "DeathPersisting",
			PlayerLifeAwaitingRespawn: "AwaitingRespawn",
		}[want]
		if d, err := e.SubmitMove(id, MoveIntent{InputSeq: 2, HeldDirs: MoveDirForward, Yaw: 0}); !errors.Is(err, ErrPlayerNotAlive) || d != MoveAccepted {
			t.Fatalf("%s SubmitMove = %v,%v; want accepted,ErrPlayerNotAlive", what, d, err)
		}
		requireMoveAnchorsUnchanged(t, e, id, 1, prePos, what)
		// A stale pre-lock sequence stays stale, never resurrected.
		if d, err := e.SubmitMove(id, MoveIntent{InputSeq: 1}); !errors.Is(err, ErrPlayerNotAlive) || d != MoveAccepted {
			t.Fatalf("%s SubmitMove(stale) = %v,%v; want accepted,ErrPlayerNotAlive", what, d, err)
		}
		requireMoveAnchorsUnchanged(t, e, id, 1, prePos, what+" stale")
	}
}

// TestLockedPlayerEnqueueMoveRejects proves the same rejection
// through the concurrent owner mailbox under Engine.Run.
func TestLockedPlayerEnqueueMoveRejects(t *testing.T) {
	for _, want := range []PlayerLifeState{PlayerLifeDeathPersisting, PlayerLifeAwaitingRespawn} {
		clk := newManualClock()
		e := ingressEngine(t, clk, openCollision{})
		id, prePos := setupLockedPlayer(t, e, testCharacterID(), want)
		cancel, done := runOwner(t, e, clk)
		if _, err := e.EnqueueMove(context.Background(), id, MoveIntent{InputSeq: 2, HeldDirs: MoveDirForward, Yaw: 0}); !errors.Is(err, ErrPlayerNotAlive) {
			t.Fatalf("life %d EnqueueMove err = %v, want ErrPlayerNotAlive", uint8(want), err)
		}
		stopOwner(t, cancel, done)
		requireMoveAnchorsUnchanged(t, e, id, 1, prePos, "enqueue")
	}
}

// TestLockedPlayerMutationFamilies proves every ordinary
// owner-local Player* mutation resolves through the shared
// active-player gate: representative calls fail with
// ErrPlayerNotAlive and bit-identical state in both locked states.
func TestLockedPlayerMutationFamilies(t *testing.T) {
	for _, want := range []PlayerLifeState{PlayerLifeDeathPersisting, PlayerLifeAwaitingRespawn} {
		e := newPlayerEngine(t, nil)
		id, _ := setupLockedPlayer(t, e, testCharacterID(), want)
		inputs := testRuntimeInputs()
		calls := []struct {
			name string
			call func() error
		}{
			{"PlayerLoseHealth", func() error { _, _, err := e.PlayerLoseHealth(id, 1, false); return err }},
			{"PlayerGainHealthNormal", func() error { _, _, err := e.PlayerGainHealthNormal(id, 1); return err }},
			{"PlayerGainHealthOvercap", func() error { _, _, err := e.PlayerGainHealthOvercap(id, 1); return err }},
			{"PlayerAdjustBaseMaxHP", func() error { _, _, err := e.PlayerAdjustBaseMaxHP(id, 1, 25); return err }},
			{"PlayerAdjustMaxHP", func() error { _, _, err := e.PlayerAdjustMaxHP(id, 1); return err }},
			{"PlayerLoseMana", func() error { _, _, err := e.PlayerLoseMana(id, 1); return err }},
			{"PlayerGainMana", func() error { _, _, err := e.PlayerGainMana(id, 1, true); return err }},
			{"PlayerAdjustMaxMana", func() error { _, _, err := e.PlayerAdjustMaxMana(id, 1); return err }},
			{"PlayerApplyExertion", func() error { _, err := e.PlayerApplyExertion(id, 1, false); return err }},
			{"PlayerApplyRestExertion", func() error { _, err := e.PlayerApplyRestExertion(id, 1, 1); return err }},
			{"PlayerSetRestThreshold", func() error { _, err := e.PlayerSetRestThreshold(id, 50); return err }},
			{"PlayerSetVitalsRuntimeInputs", func() error { return e.PlayerSetVitalsRuntimeInputs(id, inputs) }},
			{"PlayerStartResting", func() error { return e.PlayerStartResting(id) }},
			{"PlayerStopResting", func() error { return e.PlayerStopResting(id) }},
			{"PlayerMarkActedSinceEntry", func() error { return e.PlayerMarkActedSinceEntry(id) }},
			{"PlayerApplyEntryActedPolicy", func() error { return e.PlayerApplyEntryActedPolicy(id, true) }},
			{"PlayerUpdateStomach", func() error { _, err := e.PlayerUpdateStomach(id); return err }},
		}
		for _, tc := range calls {
			before := captureLifeProbe(t, e, id)
			if err := tc.call(); !errors.Is(err, ErrPlayerNotAlive) {
				t.Fatalf("life %d %s err = %v, want ErrPlayerNotAlive", uint8(want), tc.name, err)
			}
			requireLifeProbeUnchanged(t, e, id, before, tc.name)
		}
		// Inspection stays allowed while locked.
		if _, ok, err := e.PlayerVitalsOf(id); err != nil || !ok {
			t.Fatalf("life %d PlayerVitalsOf = %v,%v; want player,nil", uint8(want), ok, err)
		}
		if _, ok, err := e.PlayerVitalsRuntimeOf(id); err != nil || !ok {
			t.Fatalf("life %d PlayerVitalsRuntimeOf = %v,%v; want player,nil", uint8(want), ok, err)
		}
		if _, err := e.Entity(id); err != nil {
			t.Fatalf("life %d Entity: %v", uint8(want), err)
		}
		if _, err := e.History(id); err != nil {
			t.Fatalf("life %d History: %v", uint8(want), err)
		}
		if _, err := e.PlayerIsResting(id); err != nil {
			t.Fatalf("life %d PlayerIsResting: %v", uint8(want), err)
		}
		// Removal stays allowed while locked.
		locked, err := e.AddPlayerEntity(CharacterID(99), world.Vec3{X: 9, Y: 0, Z: 9}, zeroHPVitals(t), testRuntimeInputs())
		if err != nil {
			t.Fatalf("AddPlayerEntity: %v", err)
		}
		beginDeath(t, e, locked.ID)
		if err := e.RemoveEntity(locked.ID); err != nil {
			t.Fatalf("RemoveEntity(locked): %v", err)
		}
	}
}

// TestLockedPlayerSetPositionRejects proves the SetPosition gate:
// locked players reject with zero mutation, generic entities and
// Alive players keep the existing behavior.
func TestLockedPlayerSetPositionRejects(t *testing.T) {
	for _, want := range []PlayerLifeState{PlayerLifeDeathPersisting, PlayerLifeAwaitingRespawn} {
		e := newPlayerEngine(t, nil)
		id, _ := setupLockedPlayer(t, e, testCharacterID(), want)
		before := captureLifeProbe(t, e, id)
		// Same-cell destination: must still reject (gate first).
		same := before.snap.Position
		same.X += 1
		if err := e.SetPosition(id, same); !errors.Is(err, ErrPlayerNotAlive) {
			t.Fatalf("life %d SetPosition err = %v, want ErrPlayerNotAlive", uint8(want), err)
		}
		requireLifeProbeUnchanged(t, e, id, before, "SetPosition")
	}

	e := newPlayerEngine(t, nil)
	gen, err := e.AddEntity(world.Vec3{X: 2, Y: 0, Z: 2})
	if err != nil {
		t.Fatalf("AddEntity: %v", err)
	}
	if err := e.SetPosition(gen.ID, world.Vec3{X: 3, Y: 0, Z: 3}); err != nil {
		t.Fatalf("generic SetPosition: %v", err)
	}
	if got, err := e.Entity(gen.ID); err != nil || got.Position != (world.Vec3{X: 3, Y: 0, Z: 3}) {
		t.Fatalf("generic SetPosition = %+v,%v", got, err)
	}

	alive, err := e.AddPlayerEntity(CharacterID(31), world.Vec3{X: 4, Y: 0, Z: 4}, testVitals(), testRuntimeInputs())
	if err != nil {
		t.Fatalf("AddPlayerEntity: %v", err)
	}
	if err := e.SetPosition(alive.ID, world.Vec3{X: 5, Y: 0, Z: 5}); err != nil {
		t.Fatalf("alive SetPosition: %v", err)
	}
	if got, err := e.Entity(alive.ID); err != nil || got.Position != (world.Vec3{X: 5, Y: 0, Z: 5}) {
		t.Fatalf("alive SetPosition = %+v,%v", got, err)
	}
}

// TestDeathPersistingStepQuiescence proves a DeathPersisting player
// advances many Steps without moving, recreating deadlines, or
// changing vitals (history may append stationary samples).
func TestDeathPersistingStepQuiescence(t *testing.T) {
	e := newPlayerEngine(t, nil)
	id := prepareDeathGatePlayer(t, e, testCharacterID())
	beginDeath(t, e, id)
	wantPos := entOf(t, e, id).position
	wantVitals := entOf(t, e, id).vitals

	for i := 0; i < 200; i++ {
		e.Step()
	}
	after := entOf(t, e, id)
	if after.position != wantPos {
		t.Fatalf("position = %+v, want %+v", after.position, wantPos)
	}
	if after.vitals != wantVitals {
		t.Fatalf("vitals changed: %+v, want %+v", after.vitals, wantVitals)
	}
	if after.healthArmed || after.manaArmed || after.restArmed {
		t.Fatalf("deadlines recreated: health=%v mana=%v rest=%v",
			after.healthArmed, after.manaArmed, after.restArmed)
	}
	if after.healthDue != 0 || after.manaDue != 0 || after.restDue != 0 {
		t.Fatalf("dues recreated: %d/%d/%d", after.healthDue, after.manaDue, after.restDue)
	}
	if after.activeHeldDirs != 0 || after.activeRun || after.speed != 0 || after.hasPending {
		t.Fatalf("movement resumed")
	}
	if st, _ := lifeOf(t, e, id); st != PlayerLifeDeathPersisting {
		t.Fatalf("life = %d, want DeathPersisting", uint8(st))
	}
	hist, err := e.History(id)
	if err != nil || len(hist) == 0 {
		t.Fatalf("history len=%d err=%v, want stationary samples", len(hist), err)
	}
	for i, s := range hist {
		if s.Position != wantPos {
			t.Fatalf("history[%d] = %+v, want %+v", i, s.Position, wantPos)
		}
	}
}

// TestPlayerAcceptPostDeathStateFirstCompletion proves the first
// valid completion on both placement paths with exact T5c3b
// install semantics.
func TestPlayerAcceptPostDeathStateFirstCompletion(t *testing.T) {
	paths := []struct {
		name string
		dest world.Vec3
		gen  uint64 // expected generation delta
	}{
		{"same-cell", world.Vec3{X: 5, Y: 0, Z: 7}, 0},
		{"cross-cell", world.Vec3{X: 40, Y: 0, Z: 3}, 1},
	}
	for _, p := range paths {
		rec := &vitalsRecorder{}
		e := newPlayerEngine(t, rec)
		id := prepareDeathGatePlayer(t, e, testCharacterID())
		before, err := e.Entity(id)
		if err != nil {
			t.Fatalf("%s Entity: %v", p.name, err)
		}
		tok := beginDeath(t, e, id)
		tick := e.CurrentTick()
		post := testVitals()
		post.HP = 10
		post.Mana = 5
		inputs := testRuntimeInputs()

		got, disp := acceptDeath(t, e, tok, p.dest, post)
		if disp != DeathCompletionApplied {
			t.Fatalf("%s disp = %d, want Applied", p.name, disp)
		}
		if st, _ := lifeOf(t, e, id); st != PlayerLifeAwaitingRespawn {
			t.Fatalf("%s life = %d, want AwaitingRespawn", p.name, uint8(st))
		}
		if got.ID != id || got.CharacterID != testCharacterID() {
			t.Fatalf("%s identity = %+v", p.name, got)
		}
		if got.Position != p.dest {
			t.Fatalf("%s position = %+v, want %+v", p.name, got.Position, p.dest)
		}
		if got.OwnershipGeneration != before.OwnershipGeneration+p.gen {
			t.Fatalf("%s generation = %d, want +%d", p.name, got.OwnershipGeneration, p.gen)
		}
		if live, ok, err := e.PlayerVitalsOf(id); err != nil || !ok || live != post {
			t.Fatalf("%s vitals = %+v,%v,%v; want %+v", p.name, live, ok, err, post)
		}
		rt, ok, err := e.PlayerVitalsRuntimeOf(id)
		if err != nil || !ok || rt.Inputs != inputs {
			t.Fatalf("%s runtime = %+v,%v,%v", p.name, rt, ok, err)
		}
		if rt.ActedSinceEntry {
			t.Fatalf("%s actedSinceEntry true", p.name)
		}
		if rt.RestArmed || rt.RestDue != 0 {
			t.Fatalf("%s rest armed: %+v", p.name, rt)
		}
		if rt.StomachAnchorTick != tick {
			t.Fatalf("%s stomach anchor = %d, want %d", p.name, rt.StomachAnchorTick, tick)
		}
		if !rt.HealthArmed || rt.HealthDue != wantHealthDue(t, e, tick, post, inputs) {
			t.Fatalf("%s health = %v/%d, want fresh deadline", p.name, rt.HealthArmed, rt.HealthDue)
		}
		if !rt.ManaArmed || rt.ManaDue != wantManaDue(t, e, tick, post, inputs) {
			t.Fatalf("%s mana = %v/%d, want fresh deadline", p.name, rt.ManaArmed, rt.ManaDue)
		}
		ent := entOf(t, e, id)
		if ent.activeHeldDirs != 0 || ent.activeRun || ent.speed != 0 || ent.hasPending {
			t.Fatalf("%s movement not stopped", p.name)
		}
		if !ent.hasAccepted || ent.lastAcceptedSeq != 2 || !ent.hasProcessed || ent.lastProcessedSeq != 1 {
			t.Fatalf("%s sequence anchors changed", p.name)
		}
		if ent.recentOps == nil || !ent.recentOps.contains(OpID(7)) || !ent.recentOps.contains(OpID(9)) {
			t.Fatalf("%s recent OpIDs not preserved", p.name)
		}
		if hist, err := e.History(id); err != nil || len(hist) != 0 {
			t.Fatalf("%s history len=%d err=%v, want empty", p.name, len(hist), err)
		}
		if rec.count() != 0 {
			t.Fatalf("%s observer fired %d events, want 0", p.name, rec.count())
		}
		if ent.deathEpoch != tok.Epoch {
			t.Fatalf("%s epoch changed by completion", p.name)
		}
	}
}

// TestPlayerAcceptPostDeathStateDuplicate proves retry/redelivery
// idempotence: the same token + payload is an exact no-op.
func TestPlayerAcceptPostDeathStateDuplicate(t *testing.T) {
	rec := &vitalsRecorder{}
	e := newPlayerEngine(t, rec)
	id := prepareDeathGatePlayer(t, e, testCharacterID())
	tok := beginDeath(t, e, id)
	dest := world.Vec3{X: 5, Y: 0, Z: 7}
	post := testVitals()
	post.HP = 10
	post.Mana = 5
	first, disp := acceptDeath(t, e, tok, dest, post)
	if disp != DeathCompletionApplied {
		t.Fatalf("first disp = %d, want Applied", disp)
	}
	eventsAfterFirst := rec.count()

	ent := entOf(t, e, id)
	wantSnap := ent.snapshot()
	wantVitals := ent.vitals
	wantRuntime := ent.runtimeSnapshotValue()
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

	dup, ddisp, err := e.PlayerAcceptPostDeathState(tok, dest, post, testRuntimeInputs())
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

// TestPlayerAcceptPostDeathStateMismatch proves the mismatch matrix:
// wrong EntityID/CharacterID/epoch or incompatible state yields
// ErrDeathAttemptMismatch (unknown IDs keep ErrEntityNotFound)
// with zero mutation.
func TestPlayerAcceptPostDeathStateMismatch(t *testing.T) {
	e := newPlayerEngine(t, nil)
	id := prepareDeathGatePlayer(t, e, testCharacterID())
	tok := beginDeath(t, e, id)
	goodPos := world.Vec3{X: 5, Y: 0, Z: 7}
	goodVitals := testVitals()

	other, err := e.AddPlayerEntity(CharacterID(55), world.Vec3{X: 9, Y: 0, Z: 9}, testVitals(), testRuntimeInputs())
	if err != nil {
		t.Fatalf("AddPlayerEntity: %v", err)
	}
	gen, err := e.AddEntity(world.Vec3{X: 10, Y: 0, Z: 10})
	if err != nil {
		t.Fatalf("AddEntity: %v", err)
	}
	probe := captureLifeProbe(t, e, id)

	cases := []struct {
		name  string
		token DeathAttemptToken
	}{
		{"wrong entity generic", DeathAttemptToken{EntityID: gen.ID, CharacterID: testCharacterID(), Epoch: 1}},
		{"wrong entity other player", DeathAttemptToken{EntityID: other.ID, CharacterID: testCharacterID(), Epoch: 1}},
		{"wrong character", DeathAttemptToken{EntityID: id, CharacterID: CharacterID(999), Epoch: tok.Epoch}},
		{"zero character", DeathAttemptToken{EntityID: id, CharacterID: 0, Epoch: tok.Epoch}},
		{"future epoch", DeathAttemptToken{EntityID: id, CharacterID: testCharacterID(), Epoch: tok.Epoch + 1}},
		{"zero epoch", DeathAttemptToken{EntityID: id, CharacterID: testCharacterID(), Epoch: 0}},
	}
	for _, tc := range cases {
		if _, _, err := e.PlayerAcceptPostDeathState(tc.token, goodPos, goodVitals, testRuntimeInputs()); !errors.Is(err, ErrDeathAttemptMismatch) {
			t.Fatalf("%s err = %v, want ErrDeathAttemptMismatch", tc.name, err)
		}
		requireLifeProbeUnchanged(t, e, id, probe, tc.name)
	}

	// Unknown EntityID keeps ErrEntityNotFound.
	if _, _, err := e.PlayerAcceptPostDeathState(
		DeathAttemptToken{EntityID: EntityID(999), CharacterID: testCharacterID(), Epoch: 1},
		goodPos, goodVitals, testRuntimeInputs(),
	); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("unknown entity err = %v, want ErrEntityNotFound", err)
	}
	requireLifeProbeUnchanged(t, e, id, probe, "unknown entity")

	// A valid-shaped token on an Alive player (never began) is an
	// incompatible state: mismatch, not applied.
	if _, _, err := e.PlayerAcceptPostDeathState(
		DeathAttemptToken{EntityID: other.ID, CharacterID: CharacterID(55), Epoch: 0},
		goodPos, goodVitals, testRuntimeInputs(),
	); !errors.Is(err, ErrDeathAttemptMismatch) {
		t.Fatalf("alive forged-token err = %v, want ErrDeathAttemptMismatch", err)
	}

	// A duplicate-epoch token after the attempt completed is stale
	// once a NEW attempt exists — covered by the ABA test; here
	// prove a mismatched epoch on the awaiting entity mismatches.
	stale := tok
	stale.Epoch++
	if _, _, err := e.PlayerAcceptPostDeathState(stale, goodPos, goodVitals, testRuntimeInputs()); !errors.Is(err, ErrDeathAttemptMismatch) {
		t.Fatalf("stale epoch err = %v, want ErrDeathAttemptMismatch", err)
	}
	requireLifeProbeUnchanged(t, e, id, probe, "stale epoch")
}

// TestPlayerAcceptPostDeathStateFailedInstallKeepsAttemptLive
// forces a deterministic install failure (cross-cell generation
// exhaustion), proves the attempt stays live with T5c3b
// rollback guarantees, then retries the SAME token successfully
// with no new epoch.
func TestPlayerAcceptPostDeathStateFailedInstallKeepsAttemptLive(t *testing.T) {
	e := newPlayerEngine(t, nil)
	id := prepareDeathGatePlayer(t, e, testCharacterID())
	tok := beginDeath(t, e, id)
	probe := captureLifeProbe(t, e, id)
	entOf(t, e, id).generation = math.MaxUint64

	dest := world.Vec3{X: 40, Y: 0, Z: 3}
	if _, _, err := e.PlayerAcceptPostDeathState(tok, dest, testVitals(), testRuntimeInputs()); !errors.Is(err, ErrOwnershipGenerationExhausted) {
		t.Fatalf("install err = %v, want ErrOwnershipGenerationExhausted", err)
	}
	if st, _ := lifeOf(t, e, id); st != PlayerLifeDeathPersisting {
		t.Fatalf("life = %d, want DeathPersisting after failed install", uint8(st))
	}
	after := entOf(t, e, id)
	if after.generation != math.MaxUint64 {
		t.Fatalf("generation = %d, want forced MaxUint64", after.generation)
	}
	got := captureLifeProbe(t, e, id)
	if got.snap.Position != probe.snap.Position || got.snap.Cell != probe.snap.Cell ||
		got.vitals != probe.vitals || got.runtime != probe.runtime ||
		len(got.hist) != len(probe.hist) || got.epoch != probe.epoch || got.life != probe.life {
		t.Fatalf("failed install mutated state:\ngot  %+v\nwant %+v", got, probe)
	}
	if len(e.registry.migrations) != 0 {
		t.Fatalf("migration record left behind")
	}

	// Fix the test-only cause and retry the SAME token: success
	// with no new epoch.
	entOf(t, e, id).generation = probe.snap.OwnershipGeneration
	snap, disp, err := e.PlayerAcceptPostDeathState(tok, dest, testVitals(), testRuntimeInputs())
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if disp != DeathCompletionApplied {
		t.Fatalf("retry disp = %d, want Applied", disp)
	}
	if snap.Position != dest {
		t.Fatalf("retry position = %+v, want %+v", snap.Position, dest)
	}
	if st, _ := lifeOf(t, e, id); st != PlayerLifeAwaitingRespawn {
		t.Fatalf("retry life = %d, want AwaitingRespawn", uint8(st))
	}
	if entOf(t, e, id).deathEpoch != tok.Epoch {
		t.Fatalf("retry advanced the epoch")
	}
}

// TestDeathAttemptRemovalABA proves EntityIDs are never reused: a
// late completion for a removed entity returns ErrEntityNotFound
// and never touches the new entity for the same CharacterID.
func TestDeathAttemptRemovalABA(t *testing.T) {
	e := newPlayerEngine(t, nil)
	a, err := e.AddPlayerEntity(testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1}, zeroHPVitals(t), testRuntimeInputs())
	if err != nil {
		t.Fatalf("AddPlayerEntity: %v", err)
	}
	oldTok := beginDeath(t, e, a.ID)
	if err := e.RemoveEntity(a.ID); err != nil {
		t.Fatalf("RemoveEntity: %v", err)
	}
	b, err := e.AddPlayerEntity(testCharacterID(), world.Vec3{X: 2, Y: 0, Z: 2}, testVitals(), testRuntimeInputs())
	if err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if b.ID == a.ID {
		t.Fatalf("EntityID reused: %d", uint64(b.ID))
	}
	if st, ok := lifeOf(t, e, b.ID); !ok || st != PlayerLifeAlive {
		t.Fatalf("re-added life = %d,%v; want Alive,true", uint8(st), ok)
	}
	bProbe := captureLifeProbe(t, e, b.ID)
	if _, _, err := e.PlayerAcceptPostDeathState(oldTok, world.Vec3{X: 5, Y: 0, Z: 7}, testVitals(), testRuntimeInputs()); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("late completion err = %v, want ErrEntityNotFound", err)
	}
	requireLifeProbeUnchanged(t, e, b.ID, bProbe, "late completion")
}

// TestAwaitingRespawnInternalRegen proves the AwaitingRespawn
// runtime rule: external mutation/input stays rejected while the
// existing internal deterministic health/mana runtime still runs
// its normal source logic (idle HP re-arms without healing, mana
// gains without rest starting).
func TestAwaitingRespawnInternalRegen(t *testing.T) {
	e := newPlayerEngine(t, nil)
	snap, err := e.AddPlayerEntity(testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1}, zeroHPVitals(t), testRuntimeInputs())
	if err != nil {
		t.Fatalf("AddPlayerEntity: %v", err)
	}
	id := snap.ID
	tok := beginDeath(t, e, id)
	post := testVitals()
	post.HP = 10
	post.Mana = 5
	inputs := testRuntimeInputs()
	dest := world.Vec3{X: 5, Y: 0, Z: 7}
	tick := e.CurrentTick()
	if _, _, err := e.PlayerAcceptPostDeathState(tok, dest, post, inputs); err != nil {
		t.Fatalf("accept: %v", err)
	}
	rt, _, err := e.PlayerVitalsRuntimeOf(id)
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	if !rt.HealthArmed || !rt.ManaArmed || rt.RestArmed {
		t.Fatalf("setup runtime = %+v, want health+mana armed, rest absent", rt)
	}

	// External mutation/input stays rejected.
	if _, _, err := e.PlayerLoseHealth(id, 1, false); !errors.Is(err, ErrPlayerNotAlive) {
		t.Fatalf("LoseHealth err = %v, want ErrPlayerNotAlive", err)
	}
	if _, err := e.SubmitMove(id, MoveIntent{InputSeq: 1, HeldDirs: MoveDirForward}); !errors.Is(err, ErrPlayerNotAlive) {
		t.Fatalf("SubmitMove err = %v, want ErrPlayerNotAlive", err)
	}

	// Drive the deterministic timer exactly to the mana due tick:
	// internal mana regen fires per its normal rules while rest
	// never starts and idle HP never heals.
	steps := rt.ManaDue - tick // small u32 distance, always ordered
	for i := uint32(0); i < steps; i++ {
		e.Step()
	}
	live, ok, err := e.PlayerVitalsOf(id)
	if err != nil || !ok {
		t.Fatalf("PlayerVitalsOf = %+v,%v,%v", live, ok, err)
	}
	if live.Mana != post.Mana+1 {
		t.Fatalf("mana = %d, want %d (one internal uncapped gain)", live.Mana, post.Mana+1)
	}
	if live.HP != post.HP {
		t.Fatalf("hp = %d, want unchanged %d (idle, never acted)", live.HP, post.HP)
	}
	rtAfter, _, err := e.PlayerVitalsRuntimeOf(id)
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	if rtAfter.RestArmed || rtAfter.RestDue != 0 {
		t.Fatalf("rest started: %+v", rtAfter)
	}
	if rtAfter.ActedSinceEntry {
		t.Fatalf("actedSinceEntry set while locked")
	}
	if st, _ := lifeOf(t, e, id); st != PlayerLifeAwaitingRespawn {
		t.Fatalf("life = %d, want AwaitingRespawn", uint8(st))
	}
	if got, err := e.Entity(id); err != nil || got.Position != dest {
		t.Fatalf("position = %+v,%v; want %+v", got, err, dest)
	}
}
