package sim

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/dlukt/voxilian/internal/world"
)

func TestHandoffDiagonalCorner(t *testing.T) {
	sink := &recordSink{}
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1), Movement: sink})
	snap, _ := e.AddEntity(world.Vec3{X: 31.9, Z: 31.9})
	// yaw1024: forward=+X, right=+Z; F+R is the (+X,+Z) diagonal.
	submitMove(t, e, snap.ID, 1, MoveDirForward|MoveDirRight, 0, 1024)
	e.Step()
	got, _ := e.Entity(snap.ID)
	if got.Cell != (world.CellCoord{X: 1, Z: 1}) {
		t.Fatalf("cell = %v, want corner {1 1}", got.Cell)
	}
	if math.Abs(got.Position.X-32.0237) > 1e-3 || math.Abs(got.Position.Z-32.0237) > 1e-3 {
		t.Fatalf("pos = %v, want corner cross", got.Position)
	}
	dist := math.Hypot(got.Position.X-31.9, got.Position.Z-31.9)
	if math.Abs(dist-0.175) > 1e-9 {
		t.Fatalf("dist = %v, want exactly one normalized 0.175 step", dist)
	}
	if got.OwnershipGeneration != 2 {
		t.Fatalf("generation = %d, want 2", got.OwnershipGeneration)
	}
	u := sink.all()
	if len(u) != 1 || u[0].HandoffRequired || u[0].Blocked {
		t.Fatalf("updates = %+v, want clean transfer", sink.all())
	}
	requireRegistryInvariants(t, e.registry)
}

func TestHandoffContinuousNoPause(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	snap, _ := e.AddEntity(world.Vec3{X: 31.9, Z: 16})
	submitMove(t, e, snap.ID, 1, MoveDirForward, 0, 1024)
	e.Step() // crosses to 32.075
	submitMove(t, e, snap.ID, 2, MoveDirForward, 0, 1024)
	e.Step() // continues to 32.25 in the destination
	got, _ := e.Entity(snap.ID)
	if math.Abs(got.Position.X-32.25) > 1e-9 {
		t.Fatalf("pos = %v, want two full 0.175 steps (no pause/double)", got.Position)
	}
	if got.OwnershipGeneration != 2 {
		t.Fatalf("generation = %d, want 2 (one transfer)", got.OwnershipGeneration)
	}
	if got.LastProcessedInputSeq != 2 {
		t.Fatalf("anchor = %d, want 2", got.LastProcessedInputSeq)
	}
	h, _ := e.History(snap.ID)
	if len(h) != 2 || h[0].Tick != 1 || h[1].Tick != 2 {
		t.Fatalf("history = %+v, want one sample per tick", h)
	}
	requireRegistryInvariants(t, e.registry)
}

func TestHandoffDestinationLater(t *testing.T) {
	sink := &recordSink{}
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1), Movement: sink})
	// Destination sorts later and already hosts another entity.
	other, _ := e.AddEntity(world.Vec3{X: 40, Z: 16})   // cell {1,0}, id 1
	cross, _ := e.AddEntity(world.Vec3{X: 31.9, Z: 16}) // cell {0,0}, id 2
	submitMove(t, e, other.ID, 1, MoveDirForward, 0, 0)
	submitMove(t, e, cross.ID, 1, MoveDirForward, 0, 1024)
	e.Step()
	got, _ := e.Entity(cross.ID)
	if math.Abs(got.Position.X-32.075) > 1e-9 {
		t.Fatalf("crosser displaced %v, want exactly one 0.175 step", got.Position)
	}
	if got.OwnershipGeneration != 2 {
		t.Fatalf("generation = %d, want 2", got.OwnershipGeneration)
	}
	hCross, _ := e.History(cross.ID)
	hOther, _ := e.History(other.ID)
	if len(hCross) != 1 || len(hOther) != 1 {
		t.Fatalf("histories = %d/%d, want exactly one sample each", len(hCross), len(hOther))
	}
	// The pre-existing destination entity processed normally once.
	gOther, _ := e.Entity(other.ID)
	if math.Abs(gOther.Position.Z-(16-0.175)) > 1e-9 {
		t.Fatalf("dest entity = %v, want one normal step", gOther.Position)
	}
	// Sink order stays canonical even with the mid-tick install.
	u := sink.all()
	if len(u) != 2 {
		t.Fatalf("updates = %d, want 2", len(u))
	}
	requireRegistryInvariants(t, e.registry)
}

func TestHandoffDestinationEarlier(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	other, _ := e.AddEntity(world.Vec3{X: 8, Z: 8})    // cell {0,0}
	cross, _ := e.AddEntity(world.Vec3{X: 32.1, Z: 8}) // cell {1,0}
	submitMove(t, e, other.ID, 1, MoveDirForward, 0, 0)
	submitMove(t, e, cross.ID, 1, MoveDirForward, 0, 3072) // -X into {0,0}
	e.Step()
	got, _ := e.Entity(cross.ID)
	if math.Abs(got.Position.X-31.925) > 1e-9 || got.Cell != (world.CellCoord{X: 0, Z: 0}) {
		t.Fatalf("crosser = %+v, want one step into earlier cell", got)
	}
	if got.OwnershipGeneration != 2 {
		t.Fatalf("generation = %d, want 2", got.OwnershipGeneration)
	}
	h, _ := e.History(cross.ID)
	if len(h) != 1 || h[0].Tick != 1 {
		t.Fatalf("history = %+v, want exactly one sample (no double process)", h)
	}
	requireRegistryInvariants(t, e.registry)
}

func TestHandoffSimultaneous(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	mk := func(x, z float64) EntitySnapshot {
		s, err := e.AddEntity(world.Vec3{X: x, Z: z})
		if err != nil {
			t.Fatalf("add: %v", err)
		}
		return s
	}
	// Scrambled insertion across sources/destinations/directions.
	// (Z lanes sit comfortably inside cell 0/2 to avoid float-drift
	// edge effects at exact lane boundaries.)
	a := mk(31.9, 16)    // -> {1,0}
	b := mk(31.95, 16.5) // -> {1,0} (same destination as A)
	c := mk(32.1, 64)    // -> {0,2} (opposite direction)
	d := mk(-31.9, -32)  // -> {-2,-1} (negative lane)
	submitMove(t, e, a.ID, 1, MoveDirForward, 0, 1024)
	submitMove(t, e, b.ID, 1, MoveDirForward, 0, 1024)
	submitMove(t, e, c.ID, 1, MoveDirForward, 0, 3072)
	submitMove(t, e, d.ID, 1, MoveDirForward, 0, 3072)
	if n := e.EntityCount(); n != 4 {
		t.Fatalf("count = %d, want 4", n)
	}
	e.Step()
	wantCells := map[EntityID]world.CellCoord{
		a.ID: {X: 1, Z: 0},
		b.ID: {X: 1, Z: 0},
		c.ID: {X: 0, Z: 2},
		d.ID: {X: -2, Z: -1},
	}
	for id, want := range wantCells {
		got, _ := e.Entity(id)
		if got.Cell != want {
			t.Fatalf("id %d cell = %v, want %v", uint64(id), got.Cell, want)
		}
		if got.OwnershipGeneration != 2 {
			t.Fatalf("id %d generation = %d, want 2", uint64(id), got.OwnershipGeneration)
		}
		h, _ := e.History(id)
		if len(h) != 1 || h[0].Tick != 1 {
			t.Fatalf("id %d history = %+v, want one sample", uint64(id), h)
		}
	}
	if n := e.EntityCount(); n != 4 {
		t.Fatalf("count after = %d, want 4", n)
	}
	inDest := e.EntitiesInCell(world.CellCoord{X: 1, Z: 0})
	if len(inDest) != 2 {
		t.Fatalf("shared destination holds %d, want A+B once each", len(inDest))
	}
	requireRegistryInvariants(t, e.registry)
}

func TestHandoffSourceCleanup(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	a, _ := e.AddEntity(world.Vec3{X: 31.9, Z: 16})
	b, _ := e.AddEntity(world.Vec3{X: 30, Z: 16})
	submitMove(t, e, a.ID, 1, MoveDirForward, 0, 1024)
	e.Step()
	// One resident left: source cell survives.
	if n := e.CellCount(); n != 2 {
		t.Fatalf("cells = %d, want source + destination", n)
	}
	submitMove(t, e, b.ID, 1, MoveDirForward, 1, 1024) // run 0.35: 30 -> 30.35
	for i := 0; i < 5; i++ {
		e.Step() // 30.35 ... 31.75, still source
	}
	submitMove(t, e, b.ID, 7, MoveDirForward, 1, 1024)
	e.Step() // 31.75 -> 32.1 crosses; source empties
	got, _ := e.Entity(b.ID)
	if got.Cell != (world.CellCoord{X: 1, Z: 0}) {
		t.Fatalf("b = %+v, want destination", got)
	}
	if n := e.CellCount(); n != 1 {
		t.Fatalf("cells = %d, want only destination (source removed)", n)
	}
	if coords := e.CellCoords(); coords[0] != (world.CellCoord{X: 1, Z: 0}) {
		t.Fatalf("cells = %v", coords)
	}
	requireRegistryInvariants(t, e.registry)
}

func TestHandoffBackAndForth(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	snap, _ := e.AddEntity(world.Vec3{X: 31.9, Z: 16})
	moves := []struct {
		seq uint32
		yaw uint16
		pos float64
		gen uint64
	}{
		{1, 1024, 32.075, 2},
		{2, 3072, 31.9, 3},
		{3, 1024, 32.075, 4},
	}
	for i, m := range moves {
		submitMove(t, e, snap.ID, m.seq, MoveDirForward, 0, m.yaw)
		e.Step()
		got, _ := e.Entity(snap.ID)
		if math.Abs(got.Position.X-m.pos) > 1e-9 {
			t.Fatalf("crossing %d pos = %v, want %v", i, got.Position, m.pos)
		}
		if got.OwnershipGeneration != m.gen || got.ID != snap.ID {
			t.Fatalf("crossing %d = %+v", i, got)
		}
		if got.LastProcessedInputSeq != m.seq {
			t.Fatalf("crossing %d anchor = %d", i, got.LastProcessedInputSeq)
		}
	}
	h, _ := e.History(snap.ID)
	if len(h) != 3 {
		t.Fatalf("history len = %d, want 3 chronological samples", len(h))
	}
	for i, s := range h {
		if s.Tick != uint32(i+1) {
			t.Fatalf("history[%d] tick = %d", i, s.Tick)
		}
	}
	requireRegistryInvariants(t, e.registry)
}

func TestHandoffMovementStatePreservation(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	snap, _ := e.AddEntity(world.Vec3{X: 31.5, Z: 16})
	submitMove(t, e, snap.ID, 7, MoveDirForward, 0, 1024)
	e.Step() // 31.675, anchor 7
	submitMove(t, e, snap.ID, 8, MoveDirForward, 0, 1024)
	e.Step() // 31.85, anchor 8
	submitMove(t, e, snap.ID, 9, MoveDirForward, 0, 1024)
	e.Step() // 32.025 crosses, gen 2
	got, _ := e.Entity(snap.ID)
	ent, _ := e.registry.lookup(snap.ID)
	if got.Yaw != 1024 || got.Speed != 35 {
		t.Fatalf("yaw/speed = %d/%d", got.Yaw, got.Speed)
	}
	if ent.activeHeldDirs != MoveDirForward || ent.activeRun {
		t.Fatalf("active control = %02x run=%v", ent.activeHeldDirs, ent.activeRun)
	}
	if ent.lastAcceptedSeq != 9 || ent.lastProcessedSeq != 9 {
		t.Fatalf("seqs = accepted %d processed %d, want 9/9",
			ent.lastAcceptedSeq, ent.lastProcessedSeq)
	}
	requireRegistryInvariants(t, e.registry)
}

func TestHandoffHistoryPreservation(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	snap, _ := e.AddEntity(world.Vec3{X: 31.325, Z: 16})
	submitMove(t, e, snap.ID, 1, MoveDirForward, 0, 1024)
	e.Step() // 31.5
	e.Step() // 31.675
	e.Step() // 31.85
	e.Step() // 32.025 crosses
	h, _ := e.History(snap.ID)
	if len(h) != 4 {
		t.Fatalf("history len = %d, want 4 (3 pre-cross + crossing)", len(h))
	}
	for i, s := range h {
		if s.Tick != uint32(i+1) {
			t.Fatalf("history[%d] tick = %d, want chronological", i, s.Tick)
		}
	}
	if math.Abs(h[3].Position.X-32.025) > 1e-9 {
		t.Fatalf("crossing sample = %v, want destination", h[3].Position)
	}
	if math.Abs(h[2].Position.X-31.85) > 1e-9 {
		t.Fatalf("pre-crossing sample lost: %v", h[2].Position)
	}
	requireRegistryInvariants(t, e.registry)
}

func TestHandoffCollisionBefore(t *testing.T) {
	fc := &fakeCollision{solid: func(p world.Vec3) bool { return p.X >= 32.0 }}
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1), Collision: fc})
	snap, _ := e.AddEntity(world.Vec3{X: 31.9, Z: 16})
	submitMove(t, e, snap.ID, 1, MoveDirForward, 0, 1024)
	e.Step()
	got, _ := e.Entity(snap.ID)
	if got.Position.X != 31.9 || got.Cell != (world.CellCoord{X: 0, Z: 0}) {
		t.Fatalf("blocked-before-boundary moved: %+v", got)
	}
	if got.OwnershipGeneration != 1 {
		t.Fatalf("generation = %d, want unchanged 1", got.OwnershipGeneration)
	}
	if got.Speed != 0 {
		t.Fatalf("speed = %d, want 0", got.Speed)
	}
	if len(e.registry.migrations) != 0 {
		t.Fatalf("migration opened for blocked movement")
	}
	requireRegistryInvariants(t, e.registry)
}

func TestHandoffCollisionAfter(t *testing.T) {
	// 20Hz run = 0.35m in 2x0.175 substeps from 31.95: 32.125 free
	// (destination-side), 32.3 solid -> handoff with Blocked.
	fc := &fakeCollision{solid: func(p world.Vec3) bool { return p.X >= 32.2 }}
	sink := &recordSink{}
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1), Collision: fc, Movement: sink})
	snap, _ := e.AddEntity(world.Vec3{X: 31.95, Z: 16})
	submitMove(t, e, snap.ID, 1, MoveDirForward, 1, 1024)
	e.Step()
	got, _ := e.Entity(snap.ID)
	if math.Abs(got.Position.X-32.125) > 1e-9 {
		t.Fatalf("pos = %v, want last-free 32.125", got.Position)
	}
	if got.Cell != (world.CellCoord{X: 1, Z: 0}) {
		t.Fatalf("cell = %v, want destination (no rollback)", got.Cell)
	}
	if got.OwnershipGeneration != 2 {
		t.Fatalf("generation = %d, want 2", got.OwnershipGeneration)
	}
	if got.Speed != 0 {
		t.Fatalf("speed = %d, want 0 (blocked after crossing)", got.Speed)
	}
	u := sink.all()
	if len(u) != 1 || !u[0].Blocked || u[0].HandoffRequired {
		t.Fatalf("updates = %+v, want blocked clean transfer", sink.all())
	}
	if n := fc.queryCount(); n != 2 {
		t.Fatalf("SolidAt queries = %d, want 2 (until block)", n)
	}
	h, _ := e.History(snap.ID)
	if len(h) != 1 || math.Abs(h[0].Position.X-32.125) > 1e-9 {
		t.Fatalf("history = %+v, want last-free destination sample", h)
	}
	requireRegistryInvariants(t, e.registry)
}

func TestHandoffVolumeFlags(t *testing.T) {
	fc := &fakeCollision{flags: func(p world.Vec3) world.VolumeFlags {
		if p.X >= 32 {
			return world.VolumeFlags(9)
		}
		return world.VolumeFlags(7)
	}}
	sink := &recordSink{}
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1), Collision: fc, Movement: sink})
	snap, _ := e.AddEntity(world.Vec3{X: 31.9, Z: 16})
	if snap.VolumeFlags != world.VolumeFlags(7) {
		t.Fatalf("initial flags = %d, want 7", uint32(snap.VolumeFlags))
	}
	submitMove(t, e, snap.ID, 1, MoveDirForward, 0, 1024)
	e.Step()
	got, _ := e.Entity(snap.ID)
	if got.VolumeFlags != world.VolumeFlags(9) {
		t.Fatalf("snapshot flags = %d, want destination 9", uint32(got.VolumeFlags))
	}
	u := sink.all()
	if len(u) != 1 || u[0].VolumeFlags != world.VolumeFlags(9) {
		t.Fatalf("updates = %+v, want destination flags", sink.all())
	}
}

func TestHandoffTickAndSeqWrap(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	snap, _ := e.AddEntity(world.Vec3{X: 31.9, Z: 16})
	e.tick.Store(math.MaxUint32 - 1)
	moves := []struct {
		seq uint32
		yaw uint16
		pos float64
		gen uint64
	}{
		{math.MaxUint32 - 1, 1024, 32.075, 2},
		{math.MaxUint32, 3072, 31.9, 3},
		{0, 1024, 32.075, 4},
	}
	for i, m := range moves {
		if _, err := e.SubmitMove(snap.ID, MoveIntent{
			InputSeq: m.seq, HeldDirs: MoveDirForward, Yaw: m.yaw, SampleTick: e.CurrentTick(),
		}); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
		e.Step()
		got, _ := e.Entity(snap.ID)
		if math.Abs(got.Position.X-m.pos) > 1e-9 || got.OwnershipGeneration != m.gen {
			t.Fatalf("crossing %d = %+v", i, got)
		}
		if got.LastProcessedInputSeq != m.seq {
			t.Fatalf("crossing %d anchor = %d, want %d", i, got.LastProcessedInputSeq, m.seq)
		}
	}
	if e.CurrentTick() != 1 {
		t.Fatalf("tick = %d, want 1 after wrap", e.CurrentTick())
	}
	h, _ := e.History(snap.ID)
	if len(h) != 3 || h[0].Tick != math.MaxUint32 || h[1].Tick != 0 || h[2].Tick != 1 {
		t.Fatalf("history ticks = %+v, want MaxUint32,0,1 chronological", h)
	}
	requireRegistryInvariants(t, e.registry)
}

// traceHandoff renders tick, ordered ownership state (cell, generation,
// pos/yaw/speed/anchor/flags), history tails, and movement updates.
func traceHandoff(e *Engine, updates []MovementUpdate) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "tick:%d\n", e.CurrentTick())
	for _, c := range e.CellCoords() {
		fmt.Fprintf(&sb, "cell:%d,%d\n", c.X, c.Z)
		for _, s := range e.EntitiesInCell(c) {
			fmt.Fprintf(&sb, "ent:%d own:{%d,%d} pos:%.3f,%.3f yaw:%d spd:%d anc:%d flg:%d\n",
				uint64(s.ID), s.Cell.X, s.Cell.Z, s.Position.X, s.Position.Z,
				s.Yaw, s.Speed, s.LastProcessedInputSeq, uint32(s.VolumeFlags))
			h, _ := e.History(s.ID)
			tail := h
			if len(tail) > 2 {
				tail = tail[len(tail)-2:]
			}
			for _, smp := range tail {
				fmt.Fprintf(&sb, "hist:%d %.3f,%.3f\n", smp.Tick, smp.Position.X, smp.Position.Z)
			}
		}
	}
	for _, u := range updates {
		fmt.Fprintf(&sb, "upd:%d ent:%d %.3f,%.3f spd:%d anc:%d blk:%v ho:%v\n",
			u.Tick, uint64(u.EntityID), u.Position.X, u.Position.Z,
			u.Speed, u.LastProcessedInputSeq, u.Blocked, u.HandoffRequired)
	}
	return sb.String()
}

func handoffTestDeps(clk Clock, rng RNG, sink *recordSink) EngineDeps {
	return EngineDeps{
		Clock: clk,
		RNG:   rng,
		Collision: &fakeCollision{solid: func(p world.Vec3) bool {
			if p.Z >= 120 && p.Z < 136 {
				return p.X >= 32 // collision-before lane
			}
			if p.Z >= 152 && p.Z < 168 {
				return p.X >= 32.2 // collision-after lane
			}
			return false
		}},
		RunGate:  RunGateFunc(func(id EntityID) bool { return uint64(id)%4 != 0 }),
		Movement: sink,
	}
}

func runHandoffScript(e *Engine, sink *recordSink, ticks int) string {
	mk := func(x, z float64) EntityID {
		s, err := e.AddEntity(world.Vec3{X: x, Z: z})
		if err != nil {
			panic(fmt.Sprintf("handoff script add: %v", err))
		}
		return s.ID
	}
	p := mk(31.9, 16)    // positive crosser, walk +X, continues after
	n := mk(-31.9, -32)  // negative crosser, walk -X
	d := mk(31.9, 63.9)  // diagonal corner crosser, yaw1024 F+R
	cb := mk(31.9, 128)  // collision-before: blocked, never crosses
	ca := mk(31.95, 160) // collision-after: run crosses blocked
	w := mk(319.9, 192)  // inputSeq-wrap crosser
	o1 := mk(31.9, 256)  // opposite pair, +X
	o2 := mk(32.1, 288)  // opposite pair, -X
	s1 := mk(31.9, 320)  // same-destination pair
	s2 := mk(31.95, 320.5)
	q := mk(31.9, 352) // queue-holder: manual held migration mid-script
	_ = q
	ids := []EntityID{p, n, d, cb, ca, w, o1, o2, s1, s2}
	var sb strings.Builder
	for k := 1; k <= ticks; k++ {
		before := len(sink.all())
		for j, id := range ids {
			seq := uint32(k*16 + j)
			dirs, yaw, run := uint8(MoveDirForward), uint16(1024), uint8(0)
			switch id {
			case n:
				yaw = 3072
			case d:
				dirs = MoveDirForward | MoveDirRight
			case ca:
				run = 1
			case w:
				// InputSeq wrap domain: MaxUint32-1, MaxUint32, 0, 1...
				seq = uint32(math.MaxUint32-2) + uint32(k)
			case o2:
				yaw = 3072
			}
			if _, err := e.SubmitMove(id, MoveIntent{
				InputSeq: seq, HeldDirs: dirs, RunFlag: run, Yaw: yaw, SampleTick: e.CurrentTick(),
			}); err != nil {
				panic(fmt.Sprintf("handoff script tick %d: %v", k, err))
			}
		}
		// Queue-holder: same submits, plus a manual held migration
		// with queued intents between ticks 2 and 3 (identical on
		// both engines), proving queued-control determinism. On tick
		// 3 q submits nothing so the drained pending (seq 34) is the
		// control actually processed.
		qseq := uint32(k * 16)
		if k != 3 {
			if _, err := e.SubmitMove(q, MoveIntent{InputSeq: qseq, HeldDirs: MoveDirForward, Yaw: 1024, SampleTick: e.CurrentTick()}); err != nil {
				panic(fmt.Sprintf("handoff script q tick %d: %v", k, err))
			}
		}
		e.Step()
		if k == 2 {
			ent, err := e.registry.lookup(q)
			if err != nil {
				panic(fmt.Sprintf("handoff script q lookup: %v", err))
			}
			ent.position = world.Vec3{X: 64.075, Z: 352}
			tok, err := e.registry.beginHandoff(q,
				OwnerRef{Cell: ent.cell, Generation: ent.generation},
				world.CellCoord{X: 2, Z: 11})
			if err != nil {
				panic(fmt.Sprintf("handoff script q begin: %v", err))
			}
			for _, qs := range []uint32{qseq + 1, qseq + 2} {
				if _, err := e.SubmitMove(q, MoveIntent{InputSeq: qs, HeldDirs: MoveDirBackward, Yaw: 2048, SampleTick: e.CurrentTick()}); err != nil {
					panic(fmt.Sprintf("handoff script q queue: %v", err))
				}
			}
			if _, err := e.registry.commitHandoff(tok); err != nil {
				panic(fmt.Sprintf("handoff script q commit: %v", err))
			}
		}
		sb.WriteString(traceHandoff(e, sink.all()[before:]))
	}
	return sb.String()
}

func TestHandoffDeterministicTrace(t *testing.T) {
	mk := func() (*Engine, *recordSink) {
		sink := &recordSink{}
		e := mustEngine(t, 20, handoffTestDeps(newManualClock(), newTestRNG(4242), sink))
		return e, sink
	}
	eA, sinkA := mk()
	traceA := runHandoffScript(eA, sinkA, 6)
	eB, sinkB := mk()
	traceB := runHandoffScript(eB, sinkB, 6)
	if traceA != traceB {
		t.Fatal("same-script handoff traces differ")
	}
	for _, want := range []string{
		"tick:6\n", "own:{1,0}", "own:{-2,-1}", "own:{1,2}",
		"blk:true", "ho:false", "hist:",
	} {
		if !strings.Contains(traceA, want) {
			t.Fatalf("handoff trace missing %q", want)
		}
	}
	// Q's drained queue must have installed pending seq qseq+2 = 2*16+2.
	// (Proves queued migration controls appear in the trace.)
	if !strings.Contains(traceA, "anc:34") {
		t.Fatalf("handoff trace missing drained anchor 34")
	}
}
