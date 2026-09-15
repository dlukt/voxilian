package sim

import (
	"errors"
	"math"
	"testing"

	"github.com/dlukt/voxilian/internal/world"
)

// M5-T5c3b death-safe runtime transition tests (spec §9.5.1e):
// owner-local PlayerQuiesceForDeath + PlayerInstallPostDeathState.
// Deterministic, no sleeps, no wall clock, no Store/persist/
// gateway/proto involvement.

// seedRecentOps attaches a distinctive dedupe cache to a live
// entity (surgery): death transitions MUST preserve it.
func seedRecentOps(t *testing.T, e *Engine, id EntityID, ops ...OpID) {
	t.Helper()
	ent := entOf(t, e, id)
	ent.recentOps = newRecentOpIDs()
	for _, op := range ops {
		ent.recentOps.insert(op)
	}
}

// prepareQuiescePlayer builds a player with rich live state: a
// processed movement anchor (seq 1, run-forward, yaw 100), a newer
// accepted-but-unprocessed pending move (seq 2), nonzero speed,
// armed health/mana/rest deadlines, actedSinceEntry true, and
// nonempty position history plus a seeded recent-OpID cache.
func prepareQuiescePlayer(t *testing.T, e *Engine) EntityID {
	t.Helper()
	v := testVitals()
	v.HP = 10
	v.Mana = 5
	snap, err := e.AddPlayerEntity(testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1}, v, testRuntimeInputs())
	if err != nil {
		t.Fatalf("AddPlayerEntity: %v", err)
	}
	id := snap.ID
	if err := e.PlayerStartResting(id); err != nil {
		t.Fatalf("PlayerStartResting: %v", err)
	}
	if _, err := e.SubmitMove(id, MoveIntent{InputSeq: 1, HeldDirs: MoveDirForward, RunFlag: 1, Yaw: 100}); err != nil {
		t.Fatalf("SubmitMove(1): %v", err)
	}
	e.Step()
	if _, err := e.SubmitMove(id, MoveIntent{InputSeq: 2, HeldDirs: MoveDirLeft, Yaw: 200}); err != nil {
		t.Fatalf("SubmitMove(2): %v", err)
	}
	seedRecentOps(t, e, id, OpID(7), OpID(9))
	return id
}

func TestPlayerQuiesceForDeathExactBehavior(t *testing.T) {
	rec := &vitalsRecorder{}
	e := newPlayerEngine(t, rec)
	id := prepareQuiescePlayer(t, e)

	before := entOf(t, e, id)
	if !before.hasPending || before.lastAcceptedSeq != 2 {
		t.Fatalf("setup: pending=%v accepted=%d, want true/2", before.hasPending, before.lastAcceptedSeq)
	}
	if !before.hasProcessed || before.lastProcessedSeq != 1 {
		t.Fatalf("setup: processed=%v seq=%d, want true/1", before.hasProcessed, before.lastProcessedSeq)
	}
	if before.speed == 0 || !before.activeRun || before.activeHeldDirs == 0 {
		t.Fatalf("setup: speed=%d run=%v dirs=%d, want moving", before.speed, before.activeRun, before.activeHeldDirs)
	}
	if !before.healthArmed || !before.manaArmed || !before.restArmed {
		t.Fatalf("setup: health=%v mana=%v rest=%v, want all armed",
			before.healthArmed, before.manaArmed, before.restArmed)
	}
	if !before.actedSinceEntry {
		t.Fatalf("setup: actedSinceEntry false, want true")
	}
	wantPos, wantCell, wantGen := before.position, before.cell, before.generation
	wantYaw := before.yaw
	wantVitals := before.vitals
	wantInputs := before.runtimeInputs
	wantAnchor := before.stomachAnchorTick
	histBefore, err := e.History(id)
	if err != nil || len(histBefore) == 0 {
		t.Fatalf("setup: history len=%d err=%v, want nonempty", len(histBefore), err)
	}

	if err := e.PlayerQuiesceForDeath(id); err != nil {
		t.Fatalf("PlayerQuiesceForDeath: %v", err)
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
	// Preserved anchors and identity.
	if after.position != wantPos || after.cell != wantCell || after.generation != wantGen {
		t.Fatalf("ownership moved: %+v/%v/g%d, want %+v/%v/g%d",
			after.position, after.cell, after.generation, wantPos, wantCell, wantGen)
	}
	if after.id != id || after.characterID != testCharacterID() {
		t.Fatalf("identity changed: id=%d char=%d", uint64(after.id), int64(after.characterID))
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
		t.Fatalf("runtime inputs changed: %+v, want %+v", after.runtimeInputs, wantInputs)
	}
	if after.stomachAnchorTick != wantAnchor {
		t.Fatalf("stomach anchor = %d, want %d", after.stomachAnchorTick, wantAnchor)
	}
	histAfter, err := e.History(id)
	if err != nil || len(histAfter) != len(histBefore) {
		t.Fatalf("history changed: len=%d err=%v, want %d", len(histAfter), err, len(histBefore))
	}
	for i := range histBefore {
		if histAfter[i] != histBefore[i] {
			t.Fatalf("history[%d] changed: %+v, want %+v", i, histAfter[i], histBefore[i])
		}
	}
	if after.recentOps == nil || !after.recentOps.contains(OpID(7)) || !after.recentOps.contains(OpID(9)) {
		t.Fatalf("recent OpIDs not preserved")
	}
	if rec.count() != 0 {
		t.Fatalf("vitals observer fired %d events, want 0", rec.count())
	}

	// Stale/duplicate pre-death sequences must NOT become fresh
	// merely because the pending move was discarded.
	if d, err := e.SubmitMove(id, MoveIntent{InputSeq: 1}); err != nil || d != MoveStale {
		t.Fatalf("SubmitMove(seq1) = %v,%v; want stale,nil", d, err)
	}
	if d, err := e.SubmitMove(id, MoveIntent{InputSeq: 2}); err != nil || d != MoveDuplicate {
		t.Fatalf("SubmitMove(seq2) = %v,%v; want duplicate,nil", d, err)
	}
	if d, err := e.SubmitMove(id, MoveIntent{InputSeq: 3}); err != nil || d != MoveAccepted {
		t.Fatalf("SubmitMove(seq3) = %v,%v; want accepted,nil", d, err)
	}
}

func TestPlayerQuiesceForDeathIdempotent(t *testing.T) {
	e := newPlayerEngine(t, nil)
	id := prepareQuiescePlayer(t, e)
	if err := e.PlayerQuiesceForDeath(id); err != nil {
		t.Fatalf("first quiesce: %v", err)
	}
	first := entOf(t, e, id).snapshot()
	rtFirst, _, _ := e.PlayerVitalsRuntimeOf(id)
	if err := e.PlayerQuiesceForDeath(id); err != nil {
		t.Fatalf("second quiesce: %v", err)
	}
	second := entOf(t, e, id).snapshot()
	rtSecond, _, _ := e.PlayerVitalsRuntimeOf(id)
	if first != second || rtFirst != rtSecond {
		t.Fatalf("second quiesce not a no-op:\n%+v\n%+v", rtFirst, rtSecond)
	}
}

// beginTestMigration quiesces a resident player into a migration
// record (surgery via the production beginHandoff); the caller
// must abortHandoff when done.
func beginTestMigration(t *testing.T, e *Engine, id EntityID, dest world.CellCoord, final world.Vec3) {
	t.Helper()
	ent := entOf(t, e, id)
	from := OwnerRef{Cell: ent.cell, Generation: ent.generation}
	if _, err := e.registry.beginHandoff(id, from, dest, final); err != nil {
		t.Fatalf("beginHandoff: %v", err)
	}
}

func TestPlayerQuiesceForDeathFailures(t *testing.T) {
	e := newPlayerEngine(t, nil)

	// Unknown entity.
	if err := e.PlayerQuiesceForDeath(EntityID(999)); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("unknown err = %v, want ErrEntityNotFound", err)
	}
	if e.EntityCount() != 0 {
		t.Fatalf("unknown quiesce mutated registry")
	}

	// Generic entity: zero mutation.
	gen, err := e.AddEntity(world.Vec3{X: 2, Y: 0, Z: 2})
	if err != nil {
		t.Fatalf("AddEntity: %v", err)
	}
	if err := e.PlayerQuiesceForDeath(gen.ID); !errors.Is(err, ErrEntityNotPlayer) {
		t.Fatalf("generic err = %v, want ErrEntityNotPlayer", err)
	}
	afterGen, err := e.Entity(gen.ID)
	if err != nil || afterGen != gen {
		t.Fatalf("generic mutated: %+v,%v; want %+v", afterGen, err, gen)
	}

	// MIGRATING player: zero mutation.
	v := testVitals()
	pSnap, err := e.AddPlayerEntity(CharacterID(11), world.Vec3{X: 3, Y: 0, Z: 3}, v, testRuntimeInputs())
	if err != nil {
		t.Fatalf("AddPlayerEntity: %v", err)
	}
	migBefore, err := e.Entity(pSnap.ID)
	if err != nil {
		t.Fatalf("Entity: %v", err)
	}
	beginTestMigration(t, e, pSnap.ID, world.CellCoord{X: 1, Z: 0}, world.Vec3{X: 40, Y: 0, Z: 3})
	// Baseline AFTER begin: beginHandoff itself quiesces the source
	// (entity takes its final dest-side position inside the record).
	// The quiesce failure must mutate nothing beyond that.
	migBefore, err = e.Entity(pSnap.ID)
	if err != nil {
		t.Fatalf("Entity(migrating): %v", err)
	}
	if err := e.PlayerQuiesceForDeath(pSnap.ID); !errors.Is(err, ErrCellHandoffRequired) {
		t.Fatalf("migrating err = %v, want ErrCellHandoffRequired", err)
		e.registry.abortHandoff(pSnap.ID)
	}
	migAfter, err := e.Entity(pSnap.ID)
	if err != nil || migAfter != migBefore {
		t.Fatalf("migrating mutated: %+v,%v; want %+v", migAfter, err, migBefore)
	}
	e.registry.abortHandoff(pSnap.ID)
}

// installState captures the full observable player state for
// rollback comparison.
type installState struct {
	snap    EntitySnapshot
	vitals  PlayerVitals
	runtime PlayerVitalsRuntimeSnapshot
	histLen int
	opLen   int
}

func captureInstallState(t *testing.T, e *Engine, id EntityID) installState {
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
	hist, err := e.History(id)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	return installState{
		snap:    snap,
		vitals:  v,
		runtime: rt,
		histLen: len(hist),
		opLen:   entOf(t, e, id).recentOps.length(),
	}
}

func requireInstallStateUnchanged(t *testing.T, e *Engine, id EntityID, want installState, what string) {
	t.Helper()
	got := captureInstallState(t, e, id)
	if got != want {
		t.Fatalf("%s mutated state:\ngot  %+v\nwant %+v", what, got, want)
	}
}

// wantDue computes the expected fresh deadline from the current
// tick using the production T4a interval helpers + the ONE
// canonical CastTicks conversion.
func wantHealthDue(t *testing.T, e *Engine, tick uint32, v PlayerVitals, in PlayerVitalsRuntimeInputs) uint32 {
	t.Helper()
	ms, err := HealthRegenIntervalMs(v.Vigor, in.EffectiveStamina, v.MaxHP, 0, in.RestoratePower)
	if err != nil {
		t.Fatalf("HealthRegenIntervalMs: %v", err)
	}
	delay, err := CastTicks(ms, e.TickHz())
	if err != nil {
		t.Fatalf("CastTicks: %v", err)
	}
	return tick + uint32(delay)
}

func wantManaDue(t *testing.T, e *Engine, tick uint32, v PlayerVitals, in PlayerVitalsRuntimeInputs) uint32 {
	t.Helper()
	ms, err := ManaRegenIntervalMs(v.Mana, v.MaxMana, v.Vigor, in.EffectiveMysticism, 0, in.RejuvenatePower, in.ManaFocusPower)
	if err != nil {
		t.Fatalf("ManaRegenIntervalMs: %v", err)
	}
	delay, err := CastTicks(ms, e.TickHz())
	if err != nil {
		t.Fatalf("CastTicks: %v", err)
	}
	return tick + uint32(delay)
}

func TestPlayerInstallPostDeathStateSameCell(t *testing.T) {
	const flags = world.VolumeFlags(0xA5)
	rec := &vitalsRecorder{}
	e := mustEngine(t, 20, EngineDeps{
		Clock:     newManualClock(),
		RNG:       newTestRNG(1),
		Collision: openCollision{flags: flags},
		Vitals:    rec,
	})
	id := prepareQuiescePlayer(t, e)
	if err := e.PlayerQuiesceForDeath(id); err != nil {
		t.Fatalf("quiesce: %v", err)
	}
	// Distinctive stale dues: a correct install MUST NOT retain them.
	stale := entOf(t, e, id)
	stale.healthDue = 111
	stale.manaDue = 222
	stale.restDue = 333

	before, err := e.Entity(id)
	if err != nil {
		t.Fatalf("Entity: %v", err)
	}
	postVitals := testVitals()
	postVitals.HP = 10
	postVitals.Mana = 5
	postInputs := testRuntimeInputs()
	dest := world.Vec3{X: 5, Y: 0, Z: 7}
	tick := e.CurrentTick()

	got, err := e.PlayerInstallPostDeathState(id, dest, postVitals, postInputs)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if got.ID != id || got.CharacterID != testCharacterID() {
		t.Fatalf("identity changed: %+v", got)
	}
	if got.Cell != before.Cell || got.OwnershipGeneration != before.OwnershipGeneration {
		t.Fatalf("same-cell changed cell/gen: %+v, want %v/g%d", got, before.Cell, before.OwnershipGeneration)
	}
	if got.Position != dest {
		t.Fatalf("position = %+v, want %+v", got.Position, dest)
	}
	if got.VolumeFlags != flags {
		t.Fatalf("volume flags = %d, want %d (resampled)", uint32(got.VolumeFlags), uint32(flags))
	}
	live, ok, err := e.PlayerVitalsOf(id)
	if err != nil || !ok || live != postVitals {
		t.Fatalf("vitals = %+v,%v,%v; want %+v", live, ok, err, postVitals)
	}
	rt, ok, err := e.PlayerVitalsRuntimeOf(id)
	if err != nil || !ok {
		t.Fatalf("runtime err: %v", err)
	}
	if rt.Inputs != postInputs {
		t.Fatalf("inputs = %+v, want %+v", rt.Inputs, postInputs)
	}
	if rt.ActedSinceEntry {
		t.Fatalf("actedSinceEntry true, want false")
	}
	if rt.RestArmed || rt.RestDue != 0 {
		t.Fatalf("rest armed after install: %+v", rt)
	}
	if rt.StomachAnchorTick != tick {
		t.Fatalf("stomach anchor = %d, want current tick %d", rt.StomachAnchorTick, tick)
	}
	if !rt.HealthArmed || rt.HealthDue != wantHealthDue(t, e, tick, postVitals, postInputs) {
		t.Fatalf("health slot = %v/%d, want fresh deadline", rt.HealthArmed, rt.HealthDue)
	}
	if !rt.ManaArmed || rt.ManaDue != wantManaDue(t, e, tick, postVitals, postInputs) {
		t.Fatalf("mana slot = %v/%d, want fresh deadline", rt.ManaArmed, rt.ManaDue)
	}
	ent := entOf(t, e, id)
	if ent.activeHeldDirs != 0 || ent.activeRun || ent.speed != 0 || ent.hasPending {
		t.Fatalf("movement not stopped after install")
	}
	if !ent.hasAccepted || ent.lastAcceptedSeq != 2 || !ent.hasProcessed || ent.lastProcessedSeq != 1 {
		t.Fatalf("sequence anchors changed: %v/%d %v/%d",
			ent.hasAccepted, ent.lastAcceptedSeq, ent.hasProcessed, ent.lastProcessedSeq)
	}
	if ent.recentOps == nil || !ent.recentOps.contains(OpID(7)) || !ent.recentOps.contains(OpID(9)) {
		t.Fatalf("recent OpIDs not preserved")
	}
	if rec.count() != 0 {
		t.Fatalf("vitals observer fired %d events, want 0", rec.count())
	}
	hist, err := e.History(id)
	if err != nil || len(hist) != 0 {
		t.Fatalf("history len=%d err=%v immediately after install, want empty", len(hist), err)
	}
	if cap := ent.history.Capacity(); cap != e.HistoryCapacity() {
		t.Fatalf("history capacity = %d, want preserved %d", cap, e.HistoryCapacity())
	}
	// The NEXT normal Step appends exactly one destination-side sample.
	e.Step()
	hist, err = e.History(id)
	if err != nil || len(hist) != 1 {
		t.Fatalf("history len=%d err=%v after one Step, want exactly 1", len(hist), err)
	}
	if hist[0].Position != dest || hist[0].Tick != e.CurrentTick() {
		t.Fatalf("sample = %+v, want dest %+v at tick %d", hist[0], dest, e.CurrentTick())
	}
}

func TestPlayerInstallPostDeathStateCrossCell(t *testing.T) {
	rec := &vitalsRecorder{}
	e := newPlayerEngine(t, rec)
	id := prepareQuiescePlayer(t, e)
	if err := e.PlayerQuiesceForDeath(id); err != nil {
		t.Fatalf("quiesce: %v", err)
	}
	before, err := e.Entity(id)
	if err != nil {
		t.Fatalf("Entity: %v", err)
	}
	if count := e.EntityCount(); count != 1 {
		t.Fatalf("setup EntityCount = %d, want 1", count)
	}
	postVitals := testVitals()
	postVitals.HP = 10
	postVitals.Mana = 5
	postInputs := testRuntimeInputs()
	dest := world.Vec3{X: 40, Y: 0, Z: 3} // cell {1,0}

	got, err := e.PlayerInstallPostDeathState(id, dest, postVitals, postInputs)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if got.ID != id || got.CharacterID != testCharacterID() {
		t.Fatalf("identity changed: %+v", got)
	}
	if got.Cell != (world.CellCoord{X: 1, Z: 0}) {
		t.Fatalf("cell = %v, want {1 0}", got.Cell)
	}
	if got.OwnershipGeneration != before.OwnershipGeneration+1 {
		t.Fatalf("generation = %d, want %d", got.OwnershipGeneration, before.OwnershipGeneration+1)
	}
	if got.Position != dest {
		t.Fatalf("position = %+v, want %+v", got.Position, dest)
	}
	if len(e.registry.migrations) != 0 {
		t.Fatalf("migration record remains: %+v", e.registry.migrations)
	}
	if live, ok := e.registry.liveEntityForCharacter(testCharacterID()); !ok || live != id {
		t.Fatalf("identity index = %d,%v; want %d,true", uint64(live), ok, uint64(id))
	}
	if e.EntityCount() != 1 {
		t.Fatalf("EntityCount = %d, want 1 (no remove/re-add)", e.EntityCount())
	}
	live, ok, err := e.PlayerVitalsOf(id)
	if err != nil || !ok || live != postVitals {
		t.Fatalf("vitals = %+v,%v,%v; want %+v", live, ok, err, postVitals)
	}
	rt, ok, err := e.PlayerVitalsRuntimeOf(id)
	if err != nil || !ok || rt.Inputs != postInputs {
		t.Fatalf("runtime = %+v,%v,%v", rt, ok, err)
	}
	if rt.RestArmed {
		t.Fatalf("rest armed after cross-cell install")
	}
	ent := entOf(t, e, id)
	if !ent.hasAccepted || ent.lastAcceptedSeq != 2 || !ent.hasProcessed || ent.lastProcessedSeq != 1 {
		t.Fatalf("sequence anchors changed")
	}
	if ent.recentOps == nil || !ent.recentOps.contains(OpID(7)) || !ent.recentOps.contains(OpID(9)) {
		t.Fatalf("recent OpIDs not preserved")
	}
	hist, err := e.History(id)
	if err != nil || len(hist) != 0 {
		t.Fatalf("history len=%d err=%v, want empty", len(hist), err)
	}
	if rec.count() != 0 {
		t.Fatalf("vitals observer fired %d events, want 0", rec.count())
	}
}

// solidCollision reports solid everywhere: ordinary movement
// could never walk there, but a resolved death placement MUST
// still install — death relocation is an explicit authoritative
// remap, not a walk.
type solidCollision struct {
	flags world.VolumeFlags
}

func (s solidCollision) SolidAt(world.Vec3) bool { return true }

func (s solidCollision) VolumeFlagsAt(p world.Vec3) world.VolumeFlags { return s.flags }

func TestPlayerInstallPostDeathStateTrustedRemapIgnoresSolid(t *testing.T) {
	const flags = world.VolumeFlags(0x5A)
	e := mustEngine(t, 20, EngineDeps{
		Clock:     newManualClock(),
		RNG:       newTestRNG(1),
		Collision: solidCollision{flags: flags},
	})
	v := testVitals()
	v.HP = 10
	v.Mana = 5
	snap, err := e.AddPlayerEntity(testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1}, v, testRuntimeInputs())
	if err != nil {
		t.Fatalf("AddPlayerEntity: %v", err)
	}
	dest := world.Vec3{X: 6, Y: 0, Z: 6}
	got, err := e.PlayerInstallPostDeathState(snap.ID, dest, v, testRuntimeInputs())
	if err != nil {
		t.Fatalf("install onto solid destination: %v", err)
	}
	if got.Position != dest {
		t.Fatalf("position = %+v, want %+v", got.Position, dest)
	}
	if got.VolumeFlags != flags {
		t.Fatalf("volume flags = %d, want sampled %d", uint32(got.VolumeFlags), uint32(flags))
	}
}

func TestPlayerInstallPostDeathStateValidationRollback(t *testing.T) {
	e := newPlayerEngine(t, nil)
	id := prepareQuiescePlayer(t, e)
	before := captureInstallState(t, e, id)

	goodPos := world.Vec3{X: 5, Y: 0, Z: 7}
	goodVitals := testVitals()
	goodInputs := testRuntimeInputs()
	badVitals := testVitals()
	badVitals.Vigor = 0 // outside 1..200
	badInputs := testRuntimeInputs()
	badInputs.EffectiveStamina = 0

	cases := []struct {
		name   string
		target EntityID
		pos    world.Vec3
		vitals PlayerVitals
		inputs PlayerVitalsRuntimeInputs
	}{
		{"nonfinite placement", id, world.Vec3{X: math.NaN(), Y: 0, Z: 7}, goodVitals, goodInputs},
		{"invalid vitals", id, goodPos, badVitals, goodInputs},
		{"invalid runtime inputs", id, goodPos, goodVitals, badInputs},
		{"unknown entity", EntityID(999), goodPos, goodVitals, goodInputs},
	}
	for _, tc := range cases {
		if _, err := e.PlayerInstallPostDeathState(tc.target, tc.pos, tc.vitals, tc.inputs); err == nil {
			t.Fatalf("%s: install succeeded, want error", tc.name)
		}
		requireInstallStateUnchanged(t, e, id, before, tc.name)
	}

	// Generic entity.
	gen, err := e.AddEntity(world.Vec3{X: 9, Y: 0, Z: 9})
	if err != nil {
		t.Fatalf("AddEntity: %v", err)
	}
	if _, err := e.PlayerInstallPostDeathState(gen.ID, goodPos, goodVitals, goodInputs); !errors.Is(err, ErrEntityNotPlayer) {
		t.Fatalf("generic err = %v, want ErrEntityNotPlayer", err)
	}
	requireInstallStateUnchanged(t, e, id, before, "generic entity")

	// MIGRATING player: zero mutation on the quiesced record.
	beginTestMigration(t, e, id, world.CellCoord{X: 1, Z: 0}, world.Vec3{X: 40, Y: 0, Z: 1})
	migBefore, err := e.Entity(id)
	if err != nil {
		t.Fatalf("Entity(migrating): %v", err)
	}
	if _, err := e.PlayerInstallPostDeathState(id, goodPos, goodVitals, goodInputs); !errors.Is(err, ErrCellHandoffRequired) {
		t.Fatalf("migrating err = %v, want ErrCellHandoffRequired", err)
		e.registry.abortHandoff(id)
	}
	migAfter, err := e.Entity(id)
	if err != nil || migAfter != migBefore {
		t.Fatalf("migrating mutated: %+v vs %+v", migAfter, migBefore)
	}
	e.registry.abortHandoff(id)
	requireInstallStateUnchanged(t, e, id, before, "migrating player")
}

func TestPlayerInstallPostDeathStateGenerationExhausted(t *testing.T) {
	e := newPlayerEngine(t, nil)
	id := prepareQuiescePlayer(t, e)
	before := captureInstallState(t, e, id)
	entOf(t, e, id).generation = math.MaxUint64

	_, err := e.PlayerInstallPostDeathState(id, world.Vec3{X: 40, Y: 0, Z: 1}, testVitals(), testRuntimeInputs())
	if !errors.Is(err, ErrOwnershipGenerationExhausted) {
		t.Fatalf("err = %v, want ErrOwnershipGenerationExhausted", err)
	}
	// The exhaustion surgery itself changed generation; everything
	// else must be bit-identical: position, cell, vitals, runtime,
	// history, and identity untouched, with generation staying at
	// the forced maximum and no migration record left behind.
	after := entOf(t, e, id)
	if after.generation != math.MaxUint64 {
		t.Fatalf("generation = %d, want forced MaxUint64", after.generation)
	}
	got := captureInstallState(t, e, id)
	if got.snap.Position != before.snap.Position || got.snap.Cell != before.snap.Cell ||
		got.snap.OwnershipGeneration != math.MaxUint64 || got.vitals != before.vitals ||
		got.runtime != before.runtime || got.histLen != before.histLen || got.opLen != before.opLen {
		t.Fatalf("exhaustion mutated state:\ngot  %+v\nwant %+v", got, before)
	}
	if got.snap.ID != id || got.snap.CharacterID != testCharacterID() {
		t.Fatalf("identity changed: %+v", got.snap)
	}
	if len(e.registry.migrations) != 0 {
		t.Fatalf("migration record left behind")
	}
}

func TestPostDeathRuntimeCreationMatrix(t *testing.T) {
	newEngine := func() *Engine { return newPlayerEngine(t, nil) }

	// Damaged post-death state: both deadlines armed from the install tick.
	e := newEngine()
	v := testVitals()
	v.HP = 10
	v.Mana = 5
	snap, err := e.AddPlayerEntity(testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1}, v, testRuntimeInputs())
	if err != nil {
		t.Fatalf("AddPlayerEntity: %v", err)
	}
	tick := e.CurrentTick()
	post := testVitals()
	post.HP = 10
	post.Mana = 5
	in := testRuntimeInputs()
	if _, err := e.PlayerInstallPostDeathState(snap.ID, world.Vec3{X: 2, Y: 0, Z: 2}, post, in); err != nil {
		t.Fatalf("install: %v", err)
	}
	rt, _, err := e.PlayerVitalsRuntimeOf(snap.ID)
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	if !rt.HealthArmed || rt.HealthDue != wantHealthDue(t, e, tick, post, in) {
		t.Fatalf("damaged health = %v/%d, want armed fresh", rt.HealthArmed, rt.HealthDue)
	}
	if !rt.ManaArmed || rt.ManaDue != wantManaDue(t, e, tick, post, in) {
		t.Fatalf("damaged mana = %v/%d, want armed fresh", rt.ManaArmed, rt.ManaDue)
	}
	if rt.RestArmed || rt.RestDue != 0 {
		t.Fatalf("damaged rest armed: %+v", rt)
	}

	// Full post-death state: neither deadline armed, rest absent.
	e2 := newEngine()
	full := testVitals() // HP == MaxHP, Mana == MaxMana
	snap2, err := e2.AddPlayerEntity(testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1}, v, testRuntimeInputs())
	if err != nil {
		t.Fatalf("AddPlayerEntity: %v", err)
	}
	if _, err := e2.PlayerInstallPostDeathState(snap2.ID, world.Vec3{X: 2, Y: 0, Z: 2}, full, testRuntimeInputs()); err != nil {
		t.Fatalf("install: %v", err)
	}
	rt2, _, err := e2.PlayerVitalsRuntimeOf(snap2.ID)
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	if rt2.HealthArmed || rt2.ManaArmed || rt2.RestArmed {
		t.Fatalf("full state armed slots: %+v", rt2)
	}
	if rt2.HealthDue != 0 || rt2.ManaDue != 0 || rt2.RestDue != 0 {
		t.Fatalf("full state nonzero dues: %+v", rt2)
	}
}

// TestPostDeathInstallNeverArmsRest is the source-correction
// regression (spec §9.5.1e): Meridian real death calls
// NewHealth/NewMana/NewVigor, but NewVigor's body only bounds
// piVigor to 1..viMax_vigor and redraws Vigor — it creates NO
// rest/vigor timer. Post-death install therefore NEVER auto-arms
// rest, even for genuine T5a post-death vitals. Do NOT call
// PlayerStartResting here.
func TestPostDeathInstallNeverArmsRest(t *testing.T) {
	e := newPlayerEngine(t, nil)
	v := testVitals()
	snap, err := e.AddPlayerEntity(testCharacterID(), world.Vec3{X: 1, Y: 0, Z: 1}, v, testRuntimeInputs())
	if err != nil {
		t.Fatalf("AddPlayerEntity: %v", err)
	}
	// Genuine T5a ordinary-death post-death vitals: HP=1, Mana=1,
	// Vigor=bound(Vigor/4,0,50) then 1..200.
	post, err := PlanPostDeathVitals(PostDeathVitalsInput{
		Vitals:      v,
		Disposition: DeathNormal,
	})
	if err != nil {
		t.Fatalf("PlanPostDeathVitals: %v", err)
	}
	if _, err := e.PlayerInstallPostDeathState(snap.ID, world.Vec3{X: 2, Y: 0, Z: 2}, post, testRuntimeInputs()); err != nil {
		t.Fatalf("install: %v", err)
	}
	if resting, err := e.PlayerIsResting(snap.ID); err != nil || resting {
		t.Fatalf("PlayerIsResting = %v,%v; want false,nil", resting, err)
	}
	rt, _, err := e.PlayerVitalsRuntimeOf(snap.ID)
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	if rt.RestArmed || rt.RestDue != 0 {
		t.Fatalf("rest auto-armed: %+v", rt)
	}
	live, ok, err := e.PlayerVitalsOf(snap.ID)
	if err != nil || !ok || live != post {
		t.Fatalf("vitals = %+v,%v,%v; want %+v", live, ok, err, post)
	}
}
