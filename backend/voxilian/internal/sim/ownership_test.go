package sim

import (
	"errors"
	"math"
	"testing"

	"github.com/dlukt/voxilian/internal/world"
)

// requireRegistryInvariants proves the binding route invariants: every
// resident locator resolves to one cell entity with matching Cell and
// nonzero generation; no ID lives in two cells; migrating IDs are in
// no cell map and no locator; generations chain From.G+1 == To.G;
// queues stay bounded; EntityCount covers resident + migrating.
func requireRegistryInvariants(t *testing.T, r *registry) {
	t.Helper()
	seen := make(map[EntityID]string)
	for coord, c := range r.cells {
		for id, ent := range c.entities {
			if ent.cell != coord {
				t.Fatalf("id %d cell %v vs map %v", uint64(id), ent.cell, coord)
			}
			if ent.generation == 0 {
				t.Fatalf("id %d has reserved zero generation", uint64(id))
			}
			if loc, ok := r.entityCell[id]; !ok || loc != coord {
				t.Fatalf("id %d locator = %v,%v, want %v", uint64(id), loc, ok, coord)
			}
			if prev, dup := seen[id]; dup {
				t.Fatalf("id %d in two states (%s + resident)", uint64(id), prev)
			}
			seen[id] = "resident"
		}
	}
	for id, coord := range r.entityCell {
		c, ok := r.cells[coord]
		if !ok {
			t.Fatalf("id %d locator without cell %v", uint64(id), coord)
		}
		if _, ok := c.entities[id]; !ok {
			t.Fatalf("id %d locator without entity", uint64(id))
		}
	}
	for id, rec := range r.migrations {
		if prev, dup := seen[id]; dup {
			t.Fatalf("id %d in two states (%s + migrating)", uint64(id), prev)
		}
		seen[id] = "migrating"
		if _, ok := r.entityCell[id]; ok {
			t.Fatalf("id %d both migrating and resident", uint64(id))
		}
		if rec.entity == nil {
			t.Fatalf("id %d migration without entity", uint64(id))
		}
		if rec.to.Generation != rec.from.Generation+1 || rec.to.Generation == 0 {
			t.Fatalf("id %d generation chain %+v -> %+v", uint64(id), rec.from, rec.to)
		}
		if len(rec.queued) > MigrationMoveQueueCapacity {
			t.Fatalf("id %d queue len %d over capacity", uint64(id), len(rec.queued))
		}
		for coord, c := range r.cells {
			if _, ok := c.entities[id]; ok {
				t.Fatalf("id %d migrating yet in cell %v", uint64(id), coord)
			}
		}
	}
	if r.EntityCount() != len(seen) {
		t.Fatalf("EntityCount = %d, want %d live routes", r.EntityCount(), len(seen))
	}
}

// holdMigration moves the entity's position dest-side and opens a
// held migration via package-private begin (B37 mechanism).
func holdMigration(t *testing.T, r *registry, id EntityID, dest world.CellCoord, pos world.Vec3) handoffToken {
	t.Helper()
	ent, err := r.lookup(id)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	from := OwnerRef{Cell: ent.cell, Generation: ent.generation}
	ent.position = pos
	tok, err := r.beginHandoff(id, from, dest)
	if err != nil {
		t.Fatalf("beginHandoff: %v", err)
	}
	return tok
}

func TestOwnershipInitial(t *testing.T) {
	r := newRegistry(40)
	snap, err := r.AddEntity(world.Vec3{X: 5})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if snap.OwnershipGeneration != 1 {
		t.Fatalf("initial generation = %d, want 1", snap.OwnershipGeneration)
	}
	got, _ := r.Entity(snap.ID)
	if got.OwnershipGeneration != 1 || got.Cell != snap.Cell {
		t.Fatalf("snapshot = %+v", got)
	}
	requireRegistryInvariants(t, r)
	// Invalid add creates no ownership state.
	if _, err := r.AddEntity(world.Vec3{X: math.NaN()}); !errors.Is(err, ErrInvalidPosition) {
		t.Fatalf("invalid add = %v", err)
	}
	requireRegistryInvariants(t, r)
	if r.EntityCount() != 1 {
		t.Fatalf("count = %d, want 1", r.EntityCount())
	}
}

func TestOwnershipBeginCommit(t *testing.T) {
	r := newRegistry(40)
	snap, _ := r.AddEntity(world.Vec3{X: 31.9})
	_, _ = r.AddEntity(world.Vec3{X: 0}) // same source cell, stays put
	src := OwnerRef{Cell: snap.Cell, Generation: 1}
	dest := world.CellCoord{X: 1, Z: 0}
	ent, _ := r.lookup(snap.ID)
	ent.position = world.Vec3{X: 32.075}
	ent.yaw = 1024
	tok, err := r.beginHandoff(snap.ID, src, dest)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if tok.to != (OwnerRef{Cell: dest, Generation: 2}) {
		t.Fatalf("token to = %+v, want {dest g2}", tok.to)
	}
	// Quiesced: not resident anywhere, still counted, still inspectable.
	if _, err := r.Entity(snap.ID); err != nil {
		t.Fatalf("migrating inspect: %v", err)
	}
	if r.EntityCount() != 2 {
		t.Fatalf("count = %d, want 2 (migrating retained)", r.EntityCount())
	}
	if len(r.EntitiesInCell(src.Cell)) != 1 {
		t.Fatalf("source still lists quiesced entity")
	}
	requireRegistryInvariants(t, r)
	d, err := r.commitHandoff(tok)
	if err != nil || d != HandoffInstalled {
		t.Fatalf("commit = %v,%v", d, err)
	}
	got, _ := r.Entity(snap.ID)
	if got.Cell != dest || got.OwnershipGeneration != 2 || got.Yaw != 1024 {
		t.Fatalf("installed = %+v", got)
	}
	if got.Position.X != 32.075 {
		t.Fatalf("pos = %v, want final", got.Position)
	}
	requireRegistryInvariants(t, r)
}

func TestOwnershipBeginValidation(t *testing.T) {
	r := newRegistry(40)
	snap, _ := r.AddEntity(world.Vec3{X: 31.9})
	src := OwnerRef{Cell: snap.Cell, Generation: 1}
	dest := world.CellCoord{X: 1, Z: 0}
	ent, _ := r.lookup(snap.ID)
	ent.position = world.Vec3{X: 32.075}
	bads := []struct {
		name string
		id   EntityID
		from OwnerRef
		dest world.CellCoord
	}{
		{"wrong cell", snap.ID, OwnerRef{Cell: dest, Generation: 1}, dest},
		{"wrong generation", snap.ID, OwnerRef{Cell: src.Cell, Generation: 2}, dest},
		{"zero generation", snap.ID, OwnerRef{Cell: src.Cell, Generation: 0}, dest},
		{"dest == source", snap.ID, src, src.Cell},
		{"unknown id", 9999, src, dest},
	}
	for _, tc := range bads {
		if _, err := r.beginHandoff(tc.id, tc.from, tc.dest); !errors.Is(err, ErrOwnershipMismatch) && !errors.Is(err, ErrEntityNotFound) {
			t.Fatalf("%s: begin = %v, want mismatch/not-found", tc.name, err)
		}
	}
	// Destination must match the entity's actual final position.
	if _, err := r.beginHandoff(snap.ID, src, world.CellCoord{X: 5, Z: 5}); !errors.Is(err, ErrOwnershipMismatch) {
		t.Fatalf("wrong dest = %v, want mismatch", err)
	}
	requireRegistryInvariants(t, r)
	if r.EntityCount() != 1 {
		t.Fatalf("count = %d, want 1 (zero mutation)", r.EntityCount())
	}
	// Already migrating: second begin is a mismatch, not a second record.
	tok := holdMigration(t, r, snap.ID, dest, world.Vec3{X: 32.075})
	if _, err := r.beginHandoff(snap.ID, src, dest); !errors.Is(err, ErrOwnershipMismatch) {
		t.Fatalf("double begin = %v, want mismatch", err)
	}
	if _, err := r.commitHandoff(tok); err != nil {
		t.Fatalf("commit: %v", err)
	}
	requireRegistryInvariants(t, r)
}

func TestOwnershipGenerationExhaustion(t *testing.T) {
	sink := &recordSink{}
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1), Movement: sink})
	snap, _ := e.AddEntity(world.Vec3{X: 31.9, Z: 16})
	ent, _ := e.registry.lookup(snap.ID)
	ent.generation = math.MaxUint64
	submitMove(t, e, snap.ID, 1, MoveDirForward, 0, 1024)
	e.Step()
	got, _ := e.Entity(snap.ID)
	if got.Position.X != 31.9 {
		t.Fatalf("exhausted step moved: %v", got.Position)
	}
	if got.Speed != 0 {
		t.Fatalf("speed = %d, want 0", got.Speed)
	}
	if got.OwnershipGeneration != math.MaxUint64 {
		t.Fatalf("generation wrapped: %d", got.OwnershipGeneration)
	}
	if got.Cell != (world.CellCoord{X: 0, Z: 0}) {
		t.Fatalf("cell changed: %v", got.Cell)
	}
	// Yaw/anchor still reflect the consumed movement input.
	if got.Yaw != 1024 || got.LastProcessedInputSeq != 1 {
		t.Fatalf("yaw/anchor = %d/%d, want 1024/1", got.Yaw, got.LastProcessedInputSeq)
	}
	u := sink.all()
	if len(u) != 1 || !u[0].HandoffRequired || u[0].Blocked {
		t.Fatalf("updates = %+v, want held handoff-required update", sink.all())
	}
	// No destination membership was created.
	if coords := e.CellCoords(); len(coords) != 1 {
		t.Fatalf("cells = %v, want source only", coords)
	}
	h, _ := e.History(snap.ID)
	if len(h) != 1 || h[0].Position.X != 31.9 {
		t.Fatalf("history = %+v, want held source sample", h)
	}
	requireRegistryInvariants(t, e.registry)
}

func TestMigrationQueueAccepted(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	snap, _ := e.AddEntity(world.Vec3{X: 16})
	submitMove(t, e, snap.ID, 10, MoveDirForward, 0, 0)
	tok := holdMigration(t, e.registry, snap.ID, world.CellCoord{X: 1, Z: 0}, world.Vec3{X: 32.075})
	_ = tok
	mk := func(seq uint32) MoveIntent { return MoveIntent{InputSeq: seq, HeldDirs: MoveDirForward, SampleTick: 0} }
	for _, seq := range []uint32{11, 12} {
		if d, err := e.SubmitMove(snap.ID, mk(seq)); err != nil || d != MoveAccepted {
			t.Fatalf("migrating submit %d = %v,%v", seq, d, err)
		}
	}
	rec := e.registry.migrations[snap.ID]
	if len(rec.queued) != 2 || rec.frontier != 12 {
		t.Fatalf("queue = %d frontier = %d, want 2/12", len(rec.queued), rec.frontier)
	}
	// Entity control state itself is untouched before install.
	entSnap, _ := e.Entity(snap.ID)
	if entSnap.LastProcessedInputSeq != 0 {
		t.Fatalf("anchor moved during migration: %d", entSnap.LastProcessedInputSeq)
	}
	got := rec.entity
	if got.lastAcceptedSeq != 10 || !got.hasPending {
		t.Fatalf("entity accepted=%d pending=%v, want 10/true (untouched by begin)",
			got.lastAcceptedSeq, got.hasPending)
	}
	requireRegistryInvariants(t, e.registry)
}

func TestMigrationQueueDuplicateStale(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	snap, _ := e.AddEntity(world.Vec3{X: 16})
	submitMove(t, e, snap.ID, 10, MoveDirForward, 0, 0)
	holdMigration(t, e.registry, snap.ID, world.CellCoord{X: 1, Z: 0}, world.Vec3{X: 32.075})
	mk := func(seq uint32, dirs uint8) MoveIntent {
		return MoveIntent{InputSeq: seq, HeldDirs: dirs, RunFlag: 1, Yaw: 3000, SampleTick: 0}
	}
	if d, _ := e.SubmitMove(snap.ID, mk(11, MoveDirForward)); d != MoveAccepted {
		t.Fatalf("queue 11 = %v", d)
	}
	if d, _ := e.SubmitMove(snap.ID, mk(12, MoveDirForward)); d != MoveAccepted {
		t.Fatalf("queue 12 = %v", d)
	}
	// Duplicate with hostile payload: no rewrite of queued state.
	if d, _ := e.SubmitMove(snap.ID, mk(12, MoveDirBackward)); d != MoveDuplicate {
		t.Fatalf("dup 12 = %v, want duplicate", d)
	}
	rec := e.registry.migrations[snap.ID]
	if len(rec.queued) != 2 || rec.queued[1].HeldDirs != MoveDirForward {
		t.Fatalf("queued state rewritten: %+v", rec.queued)
	}
	// Stale serially older input.
	if d, _ := e.SubmitMove(snap.ID, mk(9, MoveDirForward)); d != MoveStale {
		t.Fatalf("stale 9 = %v, want stale", d)
	}
	if len(rec.queued) != 2 {
		t.Fatalf("queue len = %d, want 2", len(rec.queued))
	}
}

func TestMigrationQueueWrapAndAmbiguity(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	snap, _ := e.AddEntity(world.Vec3{X: 16})
	submitMove(t, e, snap.ID, math.MaxUint32-1, MoveDirForward, 0, 0)
	holdMigration(t, e.registry, snap.ID, world.CellCoord{X: 1, Z: 0}, world.Vec3{X: 32.075})
	mk := func(seq uint32) MoveIntent { return MoveIntent{InputSeq: seq, SampleTick: 0} }
	for _, seq := range []uint32{math.MaxUint32, 0, 1} {
		if d, err := e.SubmitMove(snap.ID, mk(seq)); err != nil || d != MoveAccepted {
			t.Fatalf("wrap %d = %v,%v", seq, d, err)
		}
	}
	rec := e.registry.migrations[snap.ID]
	if rec.frontier != 1 || len(rec.queued) != 3 {
		t.Fatalf("frontier/queue = %d/%d, want 1/3", rec.frontier, len(rec.queued))
	}
	if _, err := e.SubmitMove(snap.ID, mk(1+(1<<31))); !errors.Is(err, ErrAmbiguousInputSeq) {
		t.Fatalf("half-range = %v, want ambiguity", err)
	}
	if rec.frontier != 1 || len(rec.queued) != 3 {
		t.Fatalf("ambiguity mutated queue: %d/%d", rec.frontier, len(rec.queued))
	}
}

func TestMigrationQueueValidation(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	snap, _ := e.AddEntity(world.Vec3{X: 16})
	submitMove(t, e, snap.ID, 10, MoveDirForward, 0, 0)
	holdMigration(t, e.registry, snap.ID, world.CellCoord{X: 1, Z: 0}, world.Vec3{X: 32.075})
	rec := e.registry.migrations[snap.ID]
	if _, err := e.SubmitMove(snap.ID, MoveIntent{InputSeq: 11, Yaw: 4096}); !errors.Is(err, ErrInvalidMoveYaw) {
		t.Fatalf("bad yaw while migrating accepted")
	}
	if _, err := e.SubmitMove(snap.ID, MoveIntent{InputSeq: 11, SampleTick: 500}); !errors.Is(err, ErrFutureInputTick) {
		t.Fatalf("future tick while migrating accepted")
	}
	if len(rec.queued) != 0 || rec.frontier != 10 {
		t.Fatalf("invalid intents consumed queue/frontier: %d/%d", len(rec.queued), rec.frontier)
	}
	// The same sequence stays eligible with valid fields.
	if d, err := e.SubmitMove(snap.ID, MoveIntent{InputSeq: 11, SampleTick: 0}); err != nil || d != MoveAccepted {
		t.Fatalf("retry 11 = %v,%v", d, err)
	}
}

func TestMigrationQueueCapacity(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	snap, _ := e.AddEntity(world.Vec3{X: 16})
	submitMove(t, e, snap.ID, 10, MoveDirForward, 0, 0)
	tok := holdMigration(t, e.registry, snap.ID, world.CellCoord{X: 1, Z: 0}, world.Vec3{X: 32.075})
	_ = tok
	for i := uint32(11); i < 11+64; i++ {
		if d, err := e.SubmitMove(snap.ID, MoveIntent{InputSeq: i, SampleTick: 0}); err != nil || d != MoveAccepted {
			t.Fatalf("queue %d = %v,%v", i, d, err)
		}
	}
	rec := e.registry.migrations[snap.ID]
	if len(rec.queued) != 64 || rec.frontier != 74 {
		t.Fatalf("queue/frontier = %d/%d, want 64/74", len(rec.queued), rec.frontier)
	}
	before := append([]MoveIntent(nil), rec.queued...)
	if _, err := e.SubmitMove(snap.ID, MoveIntent{InputSeq: 75, SampleTick: 0}); !errors.Is(err, ErrMigrationQueueFull) {
		t.Fatalf("65th = %v, want ErrMigrationQueueFull", err)
	}
	if len(rec.queued) != 64 || rec.frontier != 74 {
		t.Fatalf("saturation mutated queue: %d/%d", len(rec.queued), rec.frontier)
	}
	for i := range before {
		if rec.queued[i] != before[i] {
			t.Fatalf("queued content changed at %d", i)
		}
	}
	requireRegistryInvariants(t, e.registry)
}

func TestMigrationRetryAfterInstall(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	snap, _ := e.AddEntity(world.Vec3{X: 16})
	submitMove(t, e, snap.ID, 10, MoveDirForward, 0, 0)
	tok := holdMigration(t, e.registry, snap.ID, world.CellCoord{X: 1, Z: 0}, world.Vec3{X: 32.075})
	for i := uint32(11); i < 11+64; i++ {
		if _, err := e.SubmitMove(snap.ID, MoveIntent{InputSeq: i, SampleTick: 0}); err != nil {
			t.Fatalf("queue %d: %v", i, err)
		}
	}
	if _, err := e.SubmitMove(snap.ID, MoveIntent{InputSeq: 75, SampleTick: 0}); !errors.Is(err, ErrMigrationQueueFull) {
		t.Fatalf("saturation expected")
	}
	if _, err := e.registry.commitHandoff(tok); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// The rejected sequence is still eligible after install.
	if d, err := e.SubmitMove(snap.ID, MoveIntent{InputSeq: 75, SampleTick: e.CurrentTick()}); err != nil || d != MoveAccepted {
		t.Fatalf("retry 75 = %v,%v, want accepted", d, err)
	}
	requireRegistryInvariants(t, e.registry)
}

func TestMigrationDrainFIFO(t *testing.T) {
	sink := &recordSink{}
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1), Movement: sink})
	snap, _ := e.AddEntity(world.Vec3{X: 16, Z: 16})
	submitMove(t, e, snap.ID, 5, MoveDirForward, 0, 0)
	e.Step() // anchor 5, one step taken
	pre, _ := e.Entity(snap.ID)
	tok := holdMigration(t, e.registry, snap.ID, world.CellCoord{X: 1, Z: 0}, world.Vec3{X: 32.075, Z: 16})
	for _, seq := range []uint32{10, 11, 12} {
		if _, err := e.SubmitMove(snap.ID, MoveIntent{InputSeq: seq, HeldDirs: MoveDirBackward, Yaw: 2048, SampleTick: 0}); err != nil {
			t.Fatalf("queue %d: %v", seq, err)
		}
	}
	if _, err := e.registry.commitHandoff(tok); err != nil {
		t.Fatalf("commit: %v", err)
	}
	got, _ := e.Entity(snap.ID)
	ent, _ := e.registry.lookup(snap.ID)
	if ent.lastAcceptedSeq != 12 || !ent.hasPending {
		t.Fatalf("drain did not reconstruct frontier: accepted=%d pending=%v",
			ent.lastAcceptedSeq, ent.hasPending)
	}
	if got.LastProcessedInputSeq != pre.LastProcessedInputSeq {
		t.Fatalf("drain moved anchor: %d vs %d", got.LastProcessedInputSeq, pre.LastProcessedInputSeq)
	}
	if got.Position.X != 32.075 {
		t.Fatalf("install moved entity: %v", got.Position)
	}
	// No second movement in the handoff tick: only the manual hold
	// happened; the queued newest control runs NEXT tick.
	if len(sink.all()) != 1 {
		t.Fatalf("handoff-tick updates = %d, want only the pre-hold step", len(sink.all()))
	}
	e.Step()
	after, _ := e.Entity(snap.ID)
	if after.LastProcessedInputSeq != 12 {
		t.Fatalf("anchor = %d, want 12 after next tick", after.LastProcessedInputSeq)
	}
	if after.Position.Z == got.Position.Z || after.Yaw != 2048 {
		t.Fatalf("queued control did not drive next tick: %+v", after)
	}
	requireRegistryInvariants(t, e.registry)
}

func TestMigrationHistoryUntouched(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	snap, _ := e.AddEntity(world.Vec3{X: 16, Z: 16})
	submitMove(t, e, snap.ID, 5, MoveDirForward, 0, 0)
	e.Step()
	e.Step()
	before, _ := e.History(snap.ID)
	tok := holdMigration(t, e.registry, snap.ID, world.CellCoord{X: 1, Z: 0}, world.Vec3{X: 32.075, Z: 16})
	if _, err := e.SubmitMove(snap.ID, MoveIntent{InputSeq: 6, SampleTick: 0}); err != nil {
		t.Fatalf("queue: %v", err)
	}
	if _, err := e.registry.commitHandoff(tok); err != nil {
		t.Fatalf("commit: %v", err)
	}
	after, _ := e.History(snap.ID)
	if len(after) != len(before) {
		t.Fatalf("queue/drain touched history: %d vs %d", len(after), len(before))
	}
	for i := range before {
		if after[i] != before[i] {
			t.Fatalf("history changed at %d", i)
		}
	}
}

func TestHandoffDuplicateDelivery(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	snap, _ := e.AddEntity(world.Vec3{X: 16})
	submitMove(t, e, snap.ID, 10, MoveDirForward, 0, 0)
	tok := holdMigration(t, e.registry, snap.ID, world.CellCoord{X: 1, Z: 0}, world.Vec3{X: 32.075})
	if _, err := e.SubmitMove(snap.ID, MoveIntent{InputSeq: 11, SampleTick: 0}); err != nil {
		t.Fatalf("queue: %v", err)
	}
	if d, err := e.registry.commitHandoff(tok); err != nil || d != HandoffInstalled {
		t.Fatalf("commit = %v,%v", d, err)
	}
	mid, _ := e.Entity(snap.ID)
	hMid, _ := e.History(snap.ID)
	d, err := e.registry.commitHandoff(tok)
	if err != nil || d != HandoffDuplicate {
		t.Fatalf("redelivery = %v,%v, want duplicate", d, err)
	}
	after, _ := e.Entity(snap.ID)
	if after != mid {
		t.Fatalf("duplicate mutated state: %+v vs %+v", after, mid)
	}
	hAfter, _ := e.History(snap.ID)
	if len(hAfter) != len(hMid) {
		t.Fatalf("duplicate replayed history: %d vs %d", len(hAfter), len(hMid))
	}
	requireRegistryInvariants(t, e.registry)
}

func TestHandoffStaleDelivery(t *testing.T) {
	r := newRegistry(40)
	snap, _ := r.AddEntity(world.Vec3{X: 31.9})
	tok := holdMigration(t, r, snap.ID, world.CellCoord{X: 1, Z: 0}, world.Vec3{X: 32.075})
	if _, err := r.commitHandoff(tok); err != nil {
		t.Fatalf("commit: %v", err)
	}
	mid, _ := r.Entity(snap.ID)
	// Older envelope: generation below current ownership.
	stale := handoffToken{id: snap.ID, from: OwnerRef{Cell: snap.Cell, Generation: 1}, to: OwnerRef{Cell: snap.Cell, Generation: 1}}
	d, err := r.commitHandoff(stale)
	if err != nil || d != HandoffStale {
		t.Fatalf("stale = %v,%v, want stale", d, err)
	}
	after, _ := r.Entity(snap.ID)
	if after != mid {
		t.Fatalf("stale mutated state: %+v vs %+v", after, mid)
	}
	// No source resurrection: old cell holds nothing of this ID.
	if inCell := r.EntitiesInCell(snap.Cell); len(inCell) != 0 {
		t.Fatalf("stale resurrected source membership: %+v", inCell)
	}
	requireRegistryInvariants(t, r)
}

func TestHandoffGenerationGap(t *testing.T) {
	r := newRegistry(40)
	snap, _ := r.AddEntity(world.Vec3{X: 16})
	gap := handoffToken{
		id:   snap.ID,
		from: OwnerRef{Cell: snap.Cell, Generation: 1},
		to:   OwnerRef{Cell: world.CellCoord{X: 1, Z: 0}, Generation: 7},
	}
	if _, err := r.commitHandoff(gap); !errors.Is(err, ErrOwnershipMismatch) {
		t.Fatalf("gap = %v, want mismatch", err)
	}
	got, _ := r.Entity(snap.ID)
	if got.OwnershipGeneration != 1 || got.Cell != snap.Cell {
		t.Fatalf("gap mutated: %+v", got)
	}
	requireRegistryInvariants(t, r)
}

func TestHandoffWrongSource(t *testing.T) {
	r := newRegistry(40)
	snap, _ := r.AddEntity(world.Vec3{X: 16})
	ent, _ := r.lookup(snap.ID)
	ent.position = world.Vec3{X: 32.075}
	// Forged token with the wrong source cell while a real migration
	// with different endpoints is open.
	real := holdMigration(t, r, snap.ID, world.CellCoord{X: 1, Z: 0}, world.Vec3{X: 32.075})
	_ = real
	forged := handoffToken{
		id:   snap.ID,
		from: OwnerRef{Cell: world.CellCoord{X: 9, Z: 9}, Generation: 1},
		to:   OwnerRef{Cell: world.CellCoord{X: 1, Z: 0}, Generation: 2},
	}
	if _, err := r.commitHandoff(forged); !errors.Is(err, ErrOwnershipMismatch) {
		t.Fatalf("wrong source = %v, want mismatch", err)
	}
	// Wrong destination against installed state likewise mismatches.
	if _, err := r.commitHandoff(real); err != nil {
		t.Fatalf("real commit: %v", err)
	}
	wrongDest := handoffToken{
		id:   snap.ID,
		from: OwnerRef{Cell: snap.Cell, Generation: 1},
		to:   OwnerRef{Cell: world.CellCoord{X: 9, Z: 9}, Generation: 2},
	}
	if _, err := r.commitHandoff(wrongDest); !errors.Is(err, ErrOwnershipMismatch) {
		t.Fatalf("wrong dest = %v, want mismatch", err)
	}
	requireRegistryInvariants(t, r)
}

func TestRemoveMigratingEntity(t *testing.T) {
	r := newRegistry(40)
	snap, _ := r.AddEntity(world.Vec3{X: 16})
	holdMigration(t, r, snap.ID, world.CellCoord{X: 1, Z: 0}, world.Vec3{X: 32.075})
	if err := r.RemoveEntity(snap.ID); err != nil {
		t.Fatalf("remove migrating: %v", err)
	}
	if r.EntityCount() != 0 {
		t.Fatalf("count = %d, want 0", r.EntityCount())
	}
	if _, err := r.Entity(snap.ID); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("lookup after remove = %v", err)
	}
	if err := r.RemoveEntity(snap.ID); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("second remove = %v", err)
	}
	requireRegistryInvariants(t, r)
}

func TestSetPositionWhileMigrating(t *testing.T) {
	r := newRegistry(40)
	snap, _ := r.AddEntity(world.Vec3{X: 16})
	holdMigration(t, r, snap.ID, world.CellCoord{X: 1, Z: 0}, world.Vec3{X: 32.075})
	if err := r.SetPosition(snap.ID, world.Vec3{X: 17}); !errors.Is(err, ErrCellHandoffRequired) {
		t.Fatalf("setpos migrating = %v, want handoff-required", err)
	}
	requireRegistryInvariants(t, r)
}

func TestHandoffPendingPreservation(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	snap, _ := e.AddEntity(world.Vec3{X: 16, Z: 16})
	submitMove(t, e, snap.ID, 10, MoveDirRight, 0, 512)
	// Hold migration with the intent still pending (no Step yet).
	tok := holdMigration(t, e.registry, snap.ID, world.CellCoord{X: 1, Z: 0}, world.Vec3{X: 32.075, Z: 16})
	if _, err := e.registry.commitHandoff(tok); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if installed, _ := e.Entity(snap.ID); installed.Cell != (world.CellCoord{X: 1, Z: 0}) {
		t.Fatalf("not installed: %+v", installed)
	}
	ent, _ := e.registry.lookup(snap.ID)
	if !ent.hasPending || ent.lastAcceptedSeq != 10 {
		t.Fatalf("pending not preserved: accepted=%d pending=%v",
			ent.lastAcceptedSeq, ent.hasPending)
	}
	// Next Step processes the preserved control in the destination.
	e.Step()
	after, _ := e.Entity(snap.ID)
	if after.LastProcessedInputSeq != 10 || after.Yaw != 512 {
		t.Fatalf("preserved control not processed: %+v", after)
	}
	if after.Position.X <= 32.075 {
		t.Fatalf("preserved control did not move: %v", after.Position)
	}
	requireRegistryInvariants(t, e.registry)
}
