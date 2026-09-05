package sim

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/dlukt/voxilian/internal/proto"
	"github.com/dlukt/voxilian/internal/world"
)

// openCollision is the default test world: free space, no flags.
type openCollision struct {
	flags world.VolumeFlags
}

func (o openCollision) SolidAt(world.Vec3) bool { return false }

func (o openCollision) VolumeFlagsAt(world.Vec3) world.VolumeFlags { return o.flags }

// staticGate is a fixed run decision.
type staticGate struct{ allow bool }

func (g staticGate) CanRun(EntityID) bool { return g.allow }

// countGate records RunGate invocations.
type countGate struct {
	allow bool
	calls int
}

func (g *countGate) CanRun(EntityID) bool {
	g.calls++
	return g.allow
}

// fakeCollision is a scripted test world recording every query.
type fakeCollision struct {
	mu      sync.Mutex
	solid   func(world.Vec3) bool
	flags   func(world.Vec3) world.VolumeFlags
	queries []world.Vec3
}

func (f *fakeCollision) SolidAt(p world.Vec3) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, p)
	if f.solid == nil {
		return false
	}
	return f.solid(p)
}

func (f *fakeCollision) VolumeFlagsAt(p world.Vec3) world.VolumeFlags {
	if f.flags == nil {
		return world.VolumeNone
	}
	return f.flags(p)
}

func (f *fakeCollision) queryCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queries)
}

// recordSink is the synchronous bounded test MovementSink.
type recordSink struct {
	mu      sync.Mutex
	updates []MovementUpdate
}

func (r *recordSink) OnMovement(u MovementUpdate) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updates = append(r.updates, u)
}

func (r *recordSink) all() []MovementUpdate {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]MovementUpdate(nil), r.updates...)
}

func (r *recordSink) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updates = nil
}

// recordObserver captures anomaly reports.
type recordObserver struct {
	mu       sync.Mutex
	anomalys []MovementAnomaly
}

func (r *recordObserver) MovementAnomaly(a MovementAnomaly) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.anomalys = append(r.anomalys, a)
}

func (r *recordObserver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.anomalys)
}

// submitMove submits with the engine's current tick as the sample tick.
func submitMove(t *testing.T, e *Engine, id EntityID, seq uint32, dirs, run uint8, yaw uint16) MoveDisposition {
	t.Helper()
	d, err := e.SubmitMove(id, MoveIntent{
		InputSeq:   seq,
		HeldDirs:   dirs,
		RunFlag:    run,
		Yaw:        yaw,
		SampleTick: e.CurrentTick(),
	})
	if err != nil {
		t.Fatalf("SubmitMove(%d) error: %v", seq, err)
	}
	return d
}

func mustSubmit(t *testing.T, e *Engine, id EntityID, in MoveIntent) (MoveDisposition, error) {
	t.Helper()
	return e.SubmitMove(id, in)
}

func TestEngineDepsValidation(t *testing.T) {
	clk := newManualClock()
	rng := newTestRNG(1)
	full := EngineDeps{Clock: clk, RNG: rng, Collision: openCollision{}, RunGate: staticGate{true}}
	if _, err := NewEngine(EngineConfig{TickHz: 20}, full); err != nil {
		t.Fatalf("full deps error: %v", err)
	}
	noCollision := full
	noCollision.Collision = nil
	if _, err := NewEngine(EngineConfig{TickHz: 20}, noCollision); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil Collision = %v, want ErrInvalidConfig", err)
	}
	noGate := full
	noGate.RunGate = nil
	if _, err := NewEngine(EngineConfig{TickHz: 20}, noGate); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil RunGate = %v, want ErrInvalidConfig", err)
	}
	// Movement/Anomaly observers are optional no-ops.
	if _, err := NewEngine(EngineConfig{TickHz: 20}, full); err != nil {
		t.Fatalf("nil observers error: %v", err)
	}
}

func TestAddEntityMovementInit(t *testing.T) {
	fc := &fakeCollision{flags: func(world.Vec3) world.VolumeFlags { return world.VolumeFlags(7) }}
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1), Collision: fc, RunGate: staticGate{true}})
	snap, err := e.AddEntity(world.Vec3{X: 16, Y: 2, Z: 16})
	if err != nil {
		t.Fatalf("AddEntity error: %v", err)
	}
	if snap.Yaw != 0 || snap.Speed != 0 || snap.LastProcessedInputSeq != 0 {
		t.Fatalf("initial movement state = yaw %d speed %d anchor %d, want zeros",
			snap.Yaw, snap.Speed, snap.LastProcessedInputSeq)
	}
	if snap.VolumeFlags != world.VolumeFlags(7) {
		t.Fatalf("initial flags = %d, want sampled 7", uint32(snap.VolumeFlags))
	}
	if h, _ := e.History(snap.ID); len(h) != 0 {
		t.Fatalf("initial history len = %d, want 0", len(h))
	}
}

func TestSubmitMoveUnknownEntity(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	if _, err := e.SubmitMove(4242, MoveIntent{InputSeq: 1}); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("SubmitMove(unknown) = %v, want ErrEntityNotFound", err)
	}
}

func TestSubmitMoveValidation(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	snap, _ := e.AddEntity(world.Vec3{X: 16})
	// Yaw above the 12-bit domain is rejected with zero mutation.
	if _, err := mustSubmit(t, e, snap.ID, MoveIntent{InputSeq: 1, Yaw: 4096, SampleTick: 0}); !errors.Is(err, ErrInvalidMoveYaw) {
		t.Fatalf("yaw 4096 accepted")
	}
	if _, err := mustSubmit(t, e, snap.ID, MoveIntent{InputSeq: 1, Yaw: math.MaxUint16, SampleTick: 0}); !errors.Is(err, ErrInvalidMoveYaw) {
		t.Fatalf("yaw MaxUint16 accepted")
	}
	// Yaw 4095 is the valid ceiling.
	if d, err := mustSubmit(t, e, snap.ID, MoveIntent{InputSeq: 1, Yaw: 4095, SampleTick: 0}); err != nil || d != MoveAccepted {
		t.Fatalf("yaw 4095 = %v,%v, want accepted,nil", d, err)
	}
}

func TestSubmitMoveInputSeq(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	snap, _ := e.AddEntity(world.Vec3{X: 16})
	mk := func(seq uint32) MoveIntent {
		return MoveIntent{InputSeq: seq, SampleTick: e.CurrentTick()}
	}
	// First input: ANY sequence accepted, including 0.
	if d, _ := mustSubmit(t, e, snap.ID, mk(0)); d != MoveAccepted {
		t.Fatalf("first 0 = %v, want accepted", d)
	}
	if d, _ := mustSubmit(t, e, snap.ID, mk(0)); d != MoveDuplicate {
		t.Fatalf("duplicate 0 = %v, want duplicate", d)
	}
	if d, _ := mustSubmit(t, e, snap.ID, mk(1)); d != MoveAccepted {
		t.Fatalf("1 after 0 = %v, want accepted", d)
	}
	if d, _ := mustSubmit(t, e, snap.ID, mk(0)); d != MoveStale {
		t.Fatalf("0 after 1 = %v, want stale", d)
	}
	// Wraparound chain on a fresh entity.
	e2 := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	s2, _ := e2.AddEntity(world.Vec3{X: 16})
	mk2 := func(seq uint32) MoveIntent { return MoveIntent{InputSeq: seq} }
	for _, seq := range []uint32{math.MaxUint32 - 1, math.MaxUint32, 0, 1} {
		if d, err := mustSubmit(t, e2, s2.ID, mk2(seq)); err != nil || d != MoveAccepted {
			t.Fatalf("wrap seq %d = %v,%v, want accepted", seq, d, err)
		}
	}
	// Stale across the wrap: MaxUint32 is behind current 0.
	if d, _ := mustSubmit(t, e2, s2.ID, mk2(math.MaxUint32)); d != MoveStale {
		t.Fatalf("MaxUint32 after 0 = %v, want stale", d)
	}
	// Exact half-range is ambiguous, never silently stale.
	if _, err := mustSubmit(t, e2, s2.ID, mk2(1+(1<<31))); !errors.Is(err, ErrAmbiguousInputSeq) {
		t.Fatalf("half-range seq accepted as %v", err)
	}
}

func TestSubmitMoveSampleTick(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	snap, _ := e.AddEntity(world.Vec3{X: 16})
	// current 0: sample 100 (= 5*20) accepted, 101 rejected.
	if _, err := mustSubmit(t, e, snap.ID, MoveIntent{InputSeq: 1, SampleTick: 100}); err != nil {
		t.Fatalf("sample +100 error: %v", err)
	}
	if _, err := mustSubmit(t, e, snap.ID, MoveIntent{InputSeq: 2, SampleTick: 101}); !errors.Is(err, ErrFutureInputTick) {
		t.Fatalf("sample +101 = %v, want ErrFutureInputTick", err)
	}
	// Past sample ticks stay acceptable.
	if _, err := mustSubmit(t, e, snap.ID, MoveIntent{InputSeq: 2, SampleTick: 0}); err != nil {
		t.Fatalf("past sample error: %v", err)
	}
	// Wrap boundary: drive current near MaxUint32 and prove +100 works mod 2^32.
	cur := uint32(math.MaxUint32 - 50)
	e.tick.Store(cur)
	if _, err := mustSubmit(t, e, snap.ID, MoveIntent{InputSeq: 3, SampleTick: cur + 100}); err != nil {
		t.Fatalf("wrapped +100 error: %v", err)
	}
	if _, err := mustSubmit(t, e, snap.ID, MoveIntent{InputSeq: 4, SampleTick: cur + 101}); !errors.Is(err, ErrFutureInputTick) {
		t.Fatalf("wrapped +101 = %v, want ErrFutureInputTick", err)
	}
	// Exact half-range sample is ambiguous, not an old tick.
	if _, err := mustSubmit(t, e, snap.ID, MoveIntent{InputSeq: 5, SampleTick: cur + (1 << 31)}); !errors.Is(err, ErrAmbiguousSampleTick) {
		t.Fatalf("half-range sample = %v, want ErrAmbiguousSampleTick", err)
	}
}

func TestFutureTickDoesNotConsumeSeq(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	snap, _ := e.AddEntity(world.Vec3{X: 16})
	// Rejected future tick with a NEWER sequence must not consume it.
	if _, err := mustSubmit(t, e, snap.ID, MoveIntent{InputSeq: 5, SampleTick: 500}); !errors.Is(err, ErrFutureInputTick) {
		t.Fatalf("future = %v", err)
	}
	// The same sequence remains acceptable with a valid sample tick.
	if d, err := mustSubmit(t, e, snap.ID, MoveIntent{InputSeq: 5, SampleTick: 0}); err != nil || d != MoveAccepted {
		t.Fatalf("retry seq 5 = %v,%v, want accepted", d, err)
	}
}

func TestSubmitMoveCoalescing(t *testing.T) {
	sink := &recordSink{}
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1), Movement: sink})
	snap, _ := e.AddEntity(world.Vec3{X: 16, Z: 16})
	base := snap.Position
	submitMove(t, e, snap.ID, 10, MoveDirForward, 0, 0)
	submitMove(t, e, snap.ID, 11, MoveDirRight, 0, 0)
	submitMove(t, e, snap.ID, 12, MoveDirBackward, 0, 0)
	e.Step()
	got, _ := e.Entity(snap.ID)
	// Only seq 12 integrated: backward at yaw 0 is +Z.
	if got.Position.X != base.X || got.Position.Z != base.Z+0.175 {
		t.Fatalf("pos = %v, want backward-only step from %v", got.Position, base)
	}
	if got.LastProcessedInputSeq != 12 {
		t.Fatalf("anchor = %d, want 12", got.LastProcessedInputSeq)
	}
	if len(sink.all()) != 1 || sink.all()[0].LastProcessedInputSeq != 12 {
		t.Fatalf("updates = %+v, want one anchor-12 update", sink.all())
	}
}

func TestAcceptedVsProcessed(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	snap, _ := e.AddEntity(world.Vec3{X: 16, Z: 16})
	base := snap.Position
	submitMove(t, e, snap.ID, 10, MoveDirForward, 0, 0)
	submitMove(t, e, snap.ID, 11, MoveDirForward, 0, 0)
	// Before Step: newest accepted, nothing processed, position unchanged.
	got, _ := e.Entity(snap.ID)
	if got.Position != base {
		t.Fatalf("position moved before Step: %v", got.Position)
	}
	if got.LastProcessedInputSeq != 0 {
		t.Fatalf("anchor before Step = %d, want 0", got.LastProcessedInputSeq)
	}
	e.Step()
	got, _ = e.Entity(snap.ID)
	if got.LastProcessedInputSeq != 11 {
		t.Fatalf("anchor after Step = %d, want 11", got.LastProcessedInputSeq)
	}
	if got.Position.Z != base.Z-0.175 {
		t.Fatalf("pos = %v, want one forward step", got.Position)
	}
}

func TestMovementPersistentHeld(t *testing.T) {
	sink := &recordSink{}
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1), Movement: sink})
	snap, _ := e.AddEntity(world.Vec3{X: 16, Z: 16})
	base := snap.Position
	submitMove(t, e, snap.ID, 7, MoveDirForward, 0, 0)
	for i := 0; i < 4; i++ {
		e.Step()
	}
	got, _ := e.Entity(snap.ID)
	if math.Abs(got.Position.Z-(base.Z-0.7)) > 1e-9 {
		t.Fatalf("pos = %v, want 4x0.175 = 0.7m forward", got.Position)
	}
	if got.LastProcessedInputSeq != 7 {
		t.Fatalf("anchor = %d, want 7 (unchanged while holding)", got.LastProcessedInputSeq)
	}
	if len(sink.all()) != 4 {
		t.Fatalf("updates = %d, want 4 (one per tick while moving)", len(sink.all()))
	}
	for _, u := range sink.all() {
		if u.LastProcessedInputSeq != 7 {
			t.Fatalf("update anchor = %d, want 7", u.LastProcessedInputSeq)
		}
	}
}

func TestMovementStop(t *testing.T) {
	sink := &recordSink{}
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1), Movement: sink})
	snap, _ := e.AddEntity(world.Vec3{X: 16, Z: 16})
	submitMove(t, e, snap.ID, 7, MoveDirForward, 0, 0)
	e.Step()
	mid, _ := e.Entity(snap.ID)
	// Newer zero-dir control with a yaw change.
	submitMove(t, e, snap.ID, 8, 0, 0, 1024)
	sink.reset()
	e.Step()
	got, _ := e.Entity(snap.ID)
	if got.Position != mid.Position {
		t.Fatalf("stop step moved: %v -> %v", mid.Position, got.Position)
	}
	if got.Speed != 0 {
		t.Fatalf("speed = %d, want 0", got.Speed)
	}
	if got.Yaw != 1024 {
		t.Fatalf("yaw = %d, want 1024", got.Yaw)
	}
	if got.LastProcessedInputSeq != 8 {
		t.Fatalf("anchor = %d, want 8", got.LastProcessedInputSeq)
	}
	if len(sink.all()) != 1 {
		t.Fatalf("stop step must still emit one update, got %d", len(sink.all()))
	}
	// Following ticks stay stationary with no further updates.
	sink.reset()
	e.Step()
	e.Step()
	again, _ := e.Entity(snap.ID)
	if again.Position != mid.Position || len(sink.all()) != 0 {
		t.Fatalf("drifted or emitted while idle: %v updates %d", again.Position, len(sink.all()))
	}
}

func TestHeldDirectionCardinals(t *testing.T) {
	cases := []struct {
		yaw    uint16
		dirs   uint8
		dx, dz float64
		name   string
	}{
		{0, MoveDirForward, 0, -0.175, "yaw0 forward -Z"},
		{0, MoveDirRight, 0.175, 0, "yaw0 right +X"},
		{0, MoveDirBackward, 0, 0.175, "yaw0 backward +Z"},
		{0, MoveDirLeft, -0.175, 0, "yaw0 left -X"},
		{1024, MoveDirForward, 0.175, 0, "yaw1024 forward +X"},
		{1024, MoveDirRight, 0, 0.175, "yaw1024 right +Z"},
		{2048, MoveDirForward, 0, 0.175, "yaw2048 forward +Z"},
		{3072, MoveDirForward, -0.175, 0, "yaw3072 forward -X"},
	}
	for _, tc := range cases {
		e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
		snap, _ := e.AddEntity(world.Vec3{X: 16, Z: 16})
		submitMove(t, e, snap.ID, 1, tc.dirs, 0, tc.yaw)
		e.Step()
		got, _ := e.Entity(snap.ID)
		if math.Abs(got.Position.X-(16+tc.dx)) > 1e-9 || math.Abs(got.Position.Z-(16+tc.dz)) > 1e-9 {
			t.Fatalf("%s: pos = %v, want d=(%.3f,%.3f)", tc.name, got.Position, tc.dx, tc.dz)
		}
		if got.Yaw != tc.yaw {
			t.Fatalf("%s: yaw = %d", tc.name, got.Yaw)
		}
	}
}

func TestHeldDirectionOpposites(t *testing.T) {
	for _, dirs := range []uint8{
		MoveDirForward | MoveDirBackward,
		MoveDirLeft | MoveDirRight,
		MoveDirForward | MoveDirBackward | MoveDirLeft | MoveDirRight,
	} {
		e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
		snap, _ := e.AddEntity(world.Vec3{X: 16, Z: 16})
		submitMove(t, e, snap.ID, 1, dirs, 0, 2048)
		e.Step()
		got, _ := e.Entity(snap.ID)
		if got.Position.X != 16 || got.Position.Z != 16 {
			t.Fatalf("dirs %02x moved: %v", dirs, got.Position)
		}
		if got.Speed != 0 {
			t.Fatalf("dirs %02x speed = %d, want 0", dirs, got.Speed)
		}
		// Yaw still updates and the anchor still advances.
		if got.Yaw != 2048 || got.LastProcessedInputSeq != 1 {
			t.Fatalf("dirs %02x yaw/anchor = %d/%d", dirs, got.Yaw, got.LastProcessedInputSeq)
		}
	}
}

func TestDiagonalNormalization(t *testing.T) {
	for _, run := range []uint8{0, 1} {
		e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
		axial, _ := e.AddEntity(world.Vec3{X: 16, Z: 16})
		diag, _ := e.AddEntity(world.Vec3{X: -16, Z: 16})
		submitMove(t, e, axial.ID, 1, MoveDirForward, run, 0)
		submitMove(t, e, diag.ID, 1, MoveDirForward|MoveDirRight, run, 0)
		e.Step()
		a, _ := e.Entity(axial.ID)
		d, _ := e.Entity(diag.ID)
		want := 0.175
		if run != 0 {
			want = 0.35
		}
		axialDist := math.Hypot(a.Position.X-16, a.Position.Z-16)
		diagDist := math.Hypot(d.Position.X+16, d.Position.Z-16)
		if math.Abs(axialDist-want) > 1e-9 {
			t.Fatalf("run=%d axial dist = %v, want %v", run, axialDist, want)
		}
		if math.Abs(diagDist-want) > 1e-9 {
			t.Fatalf("run=%d diagonal dist = %v, want %v (normalized, not sqrt2)", run, diagDist, want)
		}
	}
}

func TestReservedDirBits(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	a, _ := e.AddEntity(world.Vec3{X: 16, Z: 16})
	b, _ := e.AddEntity(world.Vec3{X: -16, Z: 16})
	submitMove(t, e, a.ID, 1, MoveDirForward, 0, 0)
	submitMove(t, e, b.ID, 1, MoveDirForward|0x80, 0, 0)
	e.Step()
	ga, _ := e.Entity(a.ID)
	gb, _ := e.Entity(b.ID)
	// Both step identically from their own bases: -0.175 Z each.
	if math.Abs(ga.Position.Z-(16-0.175)) > 1e-9 || math.Abs(gb.Position.Z-(16-0.175)) > 1e-9 {
		t.Fatalf("reserved bit changed movement: %v vs %v", ga.Position, gb.Position)
	}
}

func TestRunFlagAndGate(t *testing.T) {
	// runFlag 0/1/7 with an allowing gate.
	for _, flag := range []uint8{0, 1, 7} {
		e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
		snap, _ := e.AddEntity(world.Vec3{X: 16, Z: 16})
		submitMove(t, e, snap.ID, 1, MoveDirForward, flag, 0)
		e.Step()
		got, _ := e.Entity(snap.ID)
		wantDist, wantSpeed := 0.175, uint8(35)
		if flag != 0 {
			wantDist, wantSpeed = 0.35, 70
		}
		if math.Abs((16-got.Position.Z)-wantDist) > 1e-9 || got.Speed != wantSpeed {
			t.Fatalf("flag %d: dist/speed = %v/%d, want %v/%d",
				flag, 16-got.Position.Z, got.Speed, wantDist, wantSpeed)
		}
	}
	// Run denied falls back to WALK: still moves at 3.5 m/s, no error.
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1), RunGate: staticGate{allow: false}})
	snap, _ := e.AddEntity(world.Vec3{X: 16, Z: 16})
	if d, err := mustSubmit(t, e, snap.ID, MoveIntent{InputSeq: 1, HeldDirs: MoveDirForward, RunFlag: 1, SampleTick: 0}); err != nil || d != MoveAccepted {
		t.Fatalf("denied run submit = %v,%v", d, err)
	}
	e.Step()
	got, _ := e.Entity(snap.ID)
	if math.Abs((16-got.Position.Z)-0.175) > 1e-9 || got.Speed != 35 {
		t.Fatalf("denied run: dist/speed = %v/%d, want 0.175/35", 16-got.Position.Z, got.Speed)
	}
}

func TestRunGateNotCalledWhenStationary(t *testing.T) {
	gate := &countGate{allow: true}
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1), RunGate: gate})
	snap, _ := e.AddEntity(world.Vec3{X: 16, Z: 16})
	// Stationary zero-dir control: gate must not be consulted.
	submitMove(t, e, snap.ID, 1, 0, 1, 512)
	e.Step()
	if gate.calls != 0 {
		t.Fatalf("gate called %d times for stationary control, want 0", gate.calls)
	}
	// Moving control consults the gate.
	submitMove(t, e, snap.ID, 2, MoveDirForward, 1, 0)
	e.Step()
	if gate.calls != 1 {
		t.Fatalf("gate called %d times for moving control, want 1", gate.calls)
	}
}

func TestMovementFixedDT(t *testing.T) {
	cases := []struct {
		hz                int
		walkStep, runStep float64
	}{
		{20, 0.175, 0.35},
		{10, 0.35, 0.7},
		{40, 0.0875, 0.175},
	}
	for _, tc := range cases {
		for _, run := range []uint8{0, 1} {
			e := mustEngine(t, tc.hz, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
			snap, _ := e.AddEntity(world.Vec3{X: 16, Z: 16})
			submitMove(t, e, snap.ID, 1, MoveDirForward, run, 0)
			e.Step()
			got, _ := e.Entity(snap.ID)
			want := tc.walkStep
			if run != 0 {
				want = tc.runStep
			}
			if math.Abs((16-got.Position.Z)-want) > 1e-9 {
				t.Fatalf("%dHz run=%d: step = %v, want %v", tc.hz, run, 16-got.Position.Z, want)
			}
		}
	}
}

func TestMovementPreservesY(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	snap, _ := e.AddEntity(world.Vec3{X: 16, Y: 123.456, Z: 16})
	submitMove(t, e, snap.ID, 1, MoveDirForward|MoveDirRight, 1, 777)
	for i := 0; i < 30; i++ {
		e.Step()
	}
	got, _ := e.Entity(snap.ID)
	if got.Position.Y != 123.456 {
		t.Fatalf("Y = %v, want exactly 123.456", got.Position.Y)
	}
}

func TestCollisionFreePath(t *testing.T) {
	fc := &fakeCollision{}
	sink := &recordSink{}
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1), Collision: fc, Movement: sink})
	snap, _ := e.AddEntity(world.Vec3{X: 16, Z: 16})
	submitMove(t, e, snap.ID, 1, MoveDirForward, 0, 0)
	e.Step()
	got, _ := e.Entity(snap.ID)
	if math.Abs(got.Position.Z-(16-0.175)) > 1e-9 || got.Position.X != 16 {
		t.Fatalf("pos = %v, want exact intended (16,15.825)", got.Position)
	}
	if got.Speed != 35 {
		t.Fatalf("speed = %d, want 35", got.Speed)
	}
	u := sink.all()
	if len(u) != 1 || u[0].Blocked || u[0].HandoffRequired || u[0].Speed != 35 || u[0].Tick != 1 {
		t.Fatalf("update = %+v, want clean moving update", u)
	}
	if u[0].EntityID != snap.ID || u[0].Position != got.Position || u[0].Yaw != 0 || u[0].LastProcessedInputSeq != 1 {
		t.Fatalf("update fields = %+v", u[0])
	}
}

func TestCollisionFirstSubstep(t *testing.T) {
	fc := &fakeCollision{solid: func(world.Vec3) bool { return true }}
	obs := &recordObserver{}
	sink := &recordSink{}
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1), Collision: fc, Movement: sink, Anomaly: obs})
	snap, _ := e.AddEntity(world.Vec3{X: 16, Z: 16})
	submitMove(t, e, snap.ID, 1, MoveDirForward, 0, 512)
	e.Step()
	got, _ := e.Entity(snap.ID)
	if got.Position.X != 16 || got.Position.Z != 16 {
		t.Fatalf("blocked step moved: %v", got.Position)
	}
	if got.Speed != 0 {
		t.Fatalf("speed = %d, want 0", got.Speed)
	}
	if got.Yaw != 512 || got.LastProcessedInputSeq != 1 {
		t.Fatalf("yaw/anchor = %d/%d, want 512/1", got.Yaw, got.LastProcessedInputSeq)
	}
	u := sink.all()
	if len(u) != 1 || !u[0].Blocked || u[0].Speed != 0 || u[0].HandoffRequired {
		t.Fatalf("update = %+v, want blocked speed-0 update", u)
	}
	// Ordinary collision is an authoritative correction, not an anomaly.
	if obs.count() != 0 {
		t.Fatalf("anomalies = %d, want 0 for normal collision", obs.count())
	}
	// Blocked history stores the corrected position, not the candidate.
	h, _ := e.History(snap.ID)
	if len(h) != 1 || h[0].Position.X != 16 || h[0].Position.Z != 16 {
		t.Fatalf("history = %+v, want corrected (16,16)", h)
	}
}

func TestCollisionPartial(t *testing.T) {
	// 10Hz walk = 0.35m in ceil(0.35/0.25)=2 substeps of 0.175:
	// Z 15.825 free, Z 15.65 solid -> last free candidate retained.
	fc := &fakeCollision{solid: func(p world.Vec3) bool { return p.Z <= 15.7 }}
	e := mustEngine(t, 10, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1), Collision: fc})
	snap, _ := e.AddEntity(world.Vec3{X: 16, Z: 16})
	submitMove(t, e, snap.ID, 1, MoveDirForward, 0, 0)
	e.Step()
	got, _ := e.Entity(snap.ID)
	if math.Abs(got.Position.Z-15.825) > 1e-9 {
		t.Fatalf("partial pos Z = %v, want 15.825 (last free)", got.Position.Z)
	}
	if got.Position.Z <= 15.7 {
		t.Fatalf("ended inside solid: %v", got.Position)
	}
	if got.Speed != 0 {
		t.Fatalf("speed = %d, want 0 (stopped against wall)", got.Speed)
	}
	h, _ := e.History(snap.ID)
	if len(h) != 1 || math.Abs(h[0].Position.Z-15.825) > 1e-9 {
		t.Fatalf("history = %+v, want partial final", h)
	}
}

func TestCollisionSubstepCount(t *testing.T) {
	fc := &fakeCollision{}
	e := mustEngine(t, 10, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1), Collision: fc})
	snap, _ := e.AddEntity(world.Vec3{X: 16, Z: 16})
	submitMove(t, e, snap.ID, 1, MoveDirForward, 1, 0) // 0.7m run at 10Hz
	e.Step()
	// ceil(0.7/0.25) = 3 substeps.
	if n := fc.queryCount(); n != 3 {
		t.Fatalf("SolidAt queries = %d, want ceil(0.7/0.25)=3", n)
	}
}

func TestCollisionNoTunneling(t *testing.T) {
	// 5Hz run: 1.4m per tick in ceil(1.4/0.25)=6 substeps. Queries land
	// near X 16.23/16.47/16.70/...: a thin wall covering the third
	// query must stop movement there instead of tunneling through.
	fc := &fakeCollision{solid: func(p world.Vec3) bool { return p.X >= 16.69 && p.X <= 16.71 }}
	e := mustEngine(t, 5, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1), Collision: fc})
	snap, _ := e.AddEntity(world.Vec3{X: 16, Z: 16})
	submitMove(t, e, snap.ID, 1, MoveDirRight, 1, 0) // yaw 0 right = +X
	e.Step()
	got, _ := e.Entity(snap.ID)
	if got.Position.X >= 16.69 {
		t.Fatalf("tunneled through wall: X = %v", got.Position.X)
	}
	if got.Position.X <= 16 {
		t.Fatalf("no progress: X = %v", got.Position.X)
	}
	if got.Speed != 0 {
		t.Fatalf("speed = %d, want 0", got.Speed)
	}
}

func TestMovementHandoffPositiveBoundary(t *testing.T) {
	obs := &recordObserver{}
	sink := &recordSink{}
	fc := &fakeCollision{}
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1), Collision: fc, Movement: sink, Anomaly: obs})
	snap, _ := e.AddEntity(world.Vec3{X: 31.9, Z: 16})
	submitMove(t, e, snap.ID, 1, MoveDirForward, 0, 1024) // +X toward 32
	e.Step()
	got, _ := e.Entity(snap.ID)
	if math.Abs(got.Position.X-32.075) > 1e-9 || got.Position.Z != 16 {
		t.Fatalf("handoff pos = %v, want (32.075,16)", got.Position)
	}
	if got.Cell != (world.CellCoord{X: 1, Z: 0}) {
		t.Fatalf("cell = %v, want {1 0}", got.Cell)
	}
	if got.ID != snap.ID {
		t.Fatalf("ID changed: %d vs %d", got.ID, snap.ID)
	}
	if got.OwnershipGeneration != 2 {
		t.Fatalf("generation = %d, want 2", got.OwnershipGeneration)
	}
	if got.Speed != 35 {
		t.Fatalf("speed = %d, want 35", got.Speed)
	}
	if got.Yaw != 1024 || got.LastProcessedInputSeq != 1 {
		t.Fatalf("yaw/anchor = %d/%d, want 1024/1", got.Yaw, got.LastProcessedInputSeq)
	}
	u := sink.all()
	if len(u) != 1 || u[0].HandoffRequired || u[0].Blocked || u[0].Speed != 35 {
		t.Fatalf("update = %+v, want clean transfer update", u)
	}
	if math.Abs(u[0].Position.X-32.075) > 1e-9 {
		t.Fatalf("update pos = %v, want destination", u[0].Position)
	}
	if obs.count() != 0 {
		t.Fatalf("anomalies = %d, want 0 for healthy handoff", obs.count())
	}
	// Exactly one history sample at the destination position.
	h, _ := e.History(snap.ID)
	if len(h) != 1 || math.Abs(h[0].Position.X-32.075) > 1e-9 {
		t.Fatalf("history = %+v, want one destination sample", h)
	}
	// Source cell removed, destination holds the same ID once.
	if coords := e.CellCoords(); len(coords) != 1 || coords[0] != (world.CellCoord{X: 1, Z: 0}) {
		t.Fatalf("cells = %v, want only destination", coords)
	}
	if inCell := e.EntitiesInCell(world.CellCoord{X: 1, Z: 0}); len(inCell) != 1 || inCell[0].ID != snap.ID {
		t.Fatalf("destination membership = %+v", inCell)
	}
}

func TestMovementHandoffNegativeBoundary(t *testing.T) {
	sink := &recordSink{}
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1), Movement: sink})
	// Cell -1 spans [-32,0): X=-31.9 moving -X reaches cell -2.
	snap, _ := e.AddEntity(world.Vec3{X: -31.9, Z: -16})
	submitMove(t, e, snap.ID, 1, MoveDirForward, 0, 3072) // yaw3072 forward = -X
	e.Step()
	got, _ := e.Entity(snap.ID)
	if math.Abs(got.Position.X-(-32.075)) > 1e-9 {
		t.Fatalf("negative handoff pos = %v, want -32.075", got.Position)
	}
	if got.Cell != (world.CellCoord{X: -2, Z: -1}) {
		t.Fatalf("cell = %v, want {-2 -1}", got.Cell)
	}
	if got.OwnershipGeneration != 2 {
		t.Fatalf("generation = %d, want 2", got.OwnershipGeneration)
	}
	u := sink.all()
	if len(u) != 1 || u[0].HandoffRequired || u[0].Blocked {
		t.Fatalf("update = %+v, want clean transfer", u)
	}
	// Crossing toward the zero cell likewise transfers: X=-0.05 moving
	// +X reaches 0.125 in cell 0.
	e2 := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	s2, _ := e2.AddEntity(world.Vec3{X: -0.05, Z: 0})
	submitMove(t, e2, s2.ID, 1, MoveDirForward, 0, 1024) // +X to 0.125 (cell 0)
	e2.Step()
	g2, _ := e2.Entity(s2.ID)
	// Float yaw math leaves Z a hair below zero (cos(π/2) ≈ 6e-17),
	// so the destination is {0,-1} by exact floor semantics.
	if math.Abs(g2.Position.X-0.125) > 1e-9 || g2.Cell != (world.CellCoord{X: 0, Z: -1}) {
		t.Fatalf("zero-crossing = %+v, want transfer to {0 -1}", g2)
	}
	if g2.OwnershipGeneration != 2 {
		t.Fatalf("generation = %d, want 2", g2.OwnershipGeneration)
	}
}

func TestVolumeFlags(t *testing.T) {
	fc := &fakeCollision{flags: func(p world.Vec3) world.VolumeFlags {
		if p.X >= 20 {
			return world.VolumeFlags(9)
		}
		return world.VolumeFlags(7)
	}}
	sink := &recordSink{}
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1), Collision: fc, Movement: sink})
	snap, _ := e.AddEntity(world.Vec3{X: 16, Z: 16})
	if snap.VolumeFlags != world.VolumeFlags(7) {
		t.Fatalf("initial flags = %d, want 7", uint32(snap.VolumeFlags))
	}
	// Run +X (0.35/tick) to X>=20, staying in cell 0.
	submitMove(t, e, snap.ID, 1, MoveDirForward, 1, 1024)
	for i := 0; i < 12; i++ {
		e.Step()
	}
	got, _ := e.Entity(snap.ID)
	if got.Position.X < 20 {
		t.Fatalf("X = %v, want >= 20", got.Position.X)
	}
	if got.Cell != (world.CellCoord{X: 0, Z: 0}) {
		t.Fatalf("cell = %v, want still source cell", got.Cell)
	}
	if got.VolumeFlags != world.VolumeFlags(9) {
		t.Fatalf("flags = %d, want 9 after moving", uint32(got.VolumeFlags))
	}
	u := sink.all()[len(sink.all())-1]
	if u.VolumeFlags != world.VolumeFlags(9) {
		t.Fatalf("update flags = %d, want 9", uint32(u.VolumeFlags))
	}
}

func TestVolumeResampleStationaryProcessed(t *testing.T) {
	current := world.VolumeFlags(7)
	fc := &fakeCollision{flags: func(world.Vec3) world.VolumeFlags { return current }}
	sink := &recordSink{}
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1), Collision: fc, Movement: sink})
	pos := world.Vec3{X: 16, Z: 16}
	snap, _ := e.AddEntity(pos)
	if snap.VolumeFlags != world.VolumeFlags(7) {
		t.Fatalf("initial flags = %d, want 7", uint32(snap.VolumeFlags))
	}
	// The world changes under the unchanged position.
	current = world.VolumeFlags(9)
	submitMove(t, e, snap.ID, 1, 0, 0, 1500)
	e.Step()
	got, _ := e.Entity(snap.ID)
	if got.Position != pos {
		t.Fatalf("stationary step moved: %v", got.Position)
	}
	if got.Speed != 0 {
		t.Fatalf("speed = %d, want 0", got.Speed)
	}
	if got.Yaw != 1500 || got.LastProcessedInputSeq != 1 {
		t.Fatalf("yaw/anchor = %d/%d, want 1500/1", got.Yaw, got.LastProcessedInputSeq)
	}
	if got.VolumeFlags != world.VolumeFlags(9) {
		t.Fatalf("snapshot flags = %d, want resampled 9", uint32(got.VolumeFlags))
	}
	u := sink.all()
	if len(u) != 1 {
		t.Fatalf("updates = %d, want 1", len(u))
	}
	if u[0].VolumeFlags != world.VolumeFlags(9) {
		t.Fatalf("update flags = %d, want 9", uint32(u[0].VolumeFlags))
	}
}

func TestVolumeHandoffTransfer(t *testing.T) {
	current := world.VolumeFlags(7)
	fc := &fakeCollision{flags: func(world.Vec3) world.VolumeFlags { return current }}
	sink := &recordSink{}
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1), Collision: fc, Movement: sink})
	snap, _ := e.AddEntity(world.Vec3{X: 31.9, Z: 16})
	if snap.VolumeFlags != world.VolumeFlags(7) {
		t.Fatalf("initial flags = %d, want 7", uint32(snap.VolumeFlags))
	}
	// Flags change before the crossing step.
	current = world.VolumeFlags(9)
	submitMove(t, e, snap.ID, 1, MoveDirForward, 0, 1024) // +X toward 32
	e.Step()
	got, _ := e.Entity(snap.ID)
	if math.Abs(got.Position.X-32.075) > 1e-9 || got.Cell != (world.CellCoord{X: 1, Z: 0}) {
		t.Fatalf("transfer = %+v, want destination {1 0}", got)
	}
	if got.VolumeFlags != world.VolumeFlags(9) {
		t.Fatalf("snapshot flags = %d, want resampled 9", uint32(got.VolumeFlags))
	}
	u := sink.all()
	if len(u) != 1 || u[0].HandoffRequired {
		t.Fatalf("updates = %+v, want one clean transfer update", sink.all())
	}
	if u[0].VolumeFlags != world.VolumeFlags(9) {
		t.Fatalf("update flags = %d, want 9", uint32(u[0].VolumeFlags))
	}
}

func TestStationaryProcessedUpdate(t *testing.T) {
	sink := &recordSink{}
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1), Movement: sink})
	snap, _ := e.AddEntity(world.Vec3{X: 16, Z: 16})
	submitMove(t, e, snap.ID, 1, 0, 0, 2000)
	e.Step()
	u := sink.all()
	if len(u) != 1 {
		t.Fatalf("stationary processed step emitted %d updates, want 1", len(u))
	}
	if u[0].Yaw != 2000 || u[0].Speed != 0 || u[0].LastProcessedInputSeq != 1 || u[0].Position != snap.Position {
		t.Fatalf("update = %+v", u[0])
	}
}

func TestNeverControlledNoUpdate(t *testing.T) {
	sink := &recordSink{}
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1), Movement: sink})
	snap, _ := e.AddEntity(world.Vec3{X: 16, Z: 16})
	e.Step()
	e.Step()
	if len(sink.all()) != 0 {
		t.Fatalf("never-controlled entity emitted %d updates", len(sink.all()))
	}
	// T1 history behavior is unchanged: one sample per tick.
	h, _ := e.History(snap.ID)
	if len(h) != 2 || h[0].Tick != 1 || h[1].Tick != 2 {
		t.Fatalf("history = %+v, want ticks 1,2", h)
	}
}

func TestMovementSinkOrder(t *testing.T) {
	sink := &recordSink{}
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1), Movement: sink})
	// Scrambled insertion across cells: IDs will interleave cell order.
	mk := func(x float64) EntityID {
		s, err := e.AddEntity(world.Vec3{X: x, Z: 0})
		if err != nil {
			t.Fatalf("add: %v", err)
		}
		return s.ID
	}
	idB := mk(100)  // cell 3
	idA := mk(-100) // cell -4
	idC := mk(0)    // cell 0
	idD := mk(33)   // cell 1
	idE := mk(-33)  // cell -2
	ids := []EntityID{idB, idA, idC, idD, idE}
	for i, id := range ids {
		submitMove(t, e, id, uint32(i+1), MoveDirForward, 0, 0)
	}
	e.Step()
	u := sink.all()
	if len(u) != len(ids) {
		t.Fatalf("updates = %d, want %d", len(u), len(ids))
	}
	// Sink updates use canonical order: CellCoord X/Z, then EntityID.
	for i := 1; i < len(u); i++ {
		ca, _ := world.CellForPosition(u[i-1].Position)
		cb, _ := world.CellForPosition(u[i].Position)
		if world.CompareCellCoords(ca, cb) > 0 ||
			(ca == cb && u[i-1].EntityID >= u[i].EntityID) {
			t.Fatalf("sink out of order at %d: %+v then %+v", i, u[i-1], u[i])
		}
	}
	// Updates are values: mutating one cannot alter sim state.
	u[0].Position.X = -999999
	got, _ := e.Entity(u[0].EntityID)
	if got.Position.X == -999999 {
		t.Fatal("mutating MovementUpdate altered live state")
	}
}

func TestHistoryAfterMovement(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	snap, _ := e.AddEntity(world.Vec3{X: 16, Z: 16})
	submitMove(t, e, snap.ID, 3, MoveDirRight, 0, 0) // +X 0.175
	e.Step()
	got, _ := e.Entity(snap.ID)
	h, _ := e.History(snap.ID)
	if len(h) != 1 || h[0].Tick != 3-2 {
		t.Fatalf("history = %+v, want one sample at tick 1", h)
	}
	if h[0].Position != got.Position {
		t.Fatalf("history pos = %v, entity pos = %v (one-tick lag)", h[0].Position, got.Position)
	}
}

func TestAnchorMonotonicity(t *testing.T) {
	sink := &recordSink{}
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1), Movement: sink})
	snap, _ := e.AddEntity(world.Vec3{X: 16, Z: 16})
	// Stay near cell center: yaw 0 forward (-Z) from Z=16 stays cell 0.
	for _, seq := range []uint32{math.MaxUint32 - 1, math.MaxUint32, 0, 1} {
		submitMove(t, e, snap.ID, seq, MoveDirForward, 0, 0)
		e.Step()
	}
	got, _ := e.Entity(snap.ID)
	if got.LastProcessedInputSeq != 1 {
		t.Fatalf("anchor = %d, want 1 after wrap chain", got.LastProcessedInputSeq)
	}
	// Duplicate/stale never move the anchor backwards.
	submitMove(t, e, snap.ID, 1, MoveDirBackward, 0, 0) // duplicate payload differs
	e.Step()
	submitMove(t, e, snap.ID, 0, MoveDirBackward, 0, 0) // stale
	e.Step()
	again, _ := e.Entity(snap.ID)
	if again.LastProcessedInputSeq != 1 {
		t.Fatalf("anchor regressed to %d", again.LastProcessedInputSeq)
	}
	// 6 forward steps total (4 + 1 after duplicate + 1 after stale):
	// dup/stale inputs never rewrote the control.
	if math.Abs(again.Position.Z-(16-6*0.175)) > 1e-9 {
		t.Fatalf("stale/duplicate altered control: %v", again.Position)
	}
}

func TestStaleCannotAlter(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	snap, _ := e.AddEntity(world.Vec3{X: 16, Z: 16})
	submitMove(t, e, snap.ID, 10, MoveDirForward, 0, 0)
	if d, _ := mustSubmit(t, e, snap.ID, MoveIntent{InputSeq: 9, HeldDirs: MoveDirBackward, SampleTick: 0}); d != MoveStale {
		t.Fatalf("seq 9 = %v, want stale", d)
	}
	e.Step()
	got, _ := e.Entity(snap.ID)
	if got.Position.Z != 16-0.175 || got.LastProcessedInputSeq != 10 {
		t.Fatalf("stale altered control: %+v", got)
	}
}

func TestDuplicateCannotAlter(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	snap, _ := e.AddEntity(world.Vec3{X: 16, Z: 16})
	submitMove(t, e, snap.ID, 10, MoveDirForward, 0, 100)
	// Same sequence, hostile different payload: still duplicate, no mutation.
	if d, _ := mustSubmit(t, e, snap.ID, MoveIntent{InputSeq: 10, HeldDirs: MoveDirBackward, RunFlag: 1, Yaw: 3000, SampleTick: 0}); d != MoveDuplicate {
		t.Fatalf("same seq new payload = %v, want duplicate", d)
	}
	e.Step()
	got, _ := e.Entity(snap.ID)
	// One forward step at the ORIGINAL yaw 100 survived: the duplicate
	// neither rewrote yaw (3000 would move -X) nor dirs nor anchor.
	if got.Yaw != 100 || got.LastProcessedInputSeq != 10 {
		t.Fatalf("duplicate rewrote control: %+v", got)
	}
	if math.Abs(math.Hypot(got.Position.X-16, got.Position.Z-16)-0.175) > 1e-9 {
		t.Fatalf("duplicate altered displacement: %+v", got)
	}
	if got.Position.X <= 16 {
		t.Fatalf("duplicate changed facing: %+v", got)
	}
}

func TestCheckDisplacement(t *testing.T) {
	base := world.Vec3{X: 16, Z: 16}
	// Exact max is acceptable.
	if ok, obs := checkDisplacement(base, world.Vec3{X: 16.175, Z: 16}, 0.175); !ok {
		t.Fatalf("exact max rejected (obs %v)", obs)
	}
	// Epsilon tolerance absorbs float noise strictly inside the
	// boundary, not gameplay slack.
	if ok, _ := checkDisplacement(base, world.Vec3{X: 16.175 + 5e-10, Z: 16}, 0.175); !ok {
		t.Fatal("just-inside-epsilon rejected")
	}
	if ok, obs := checkDisplacement(base, world.Vec3{X: 16.175 + 1e-6, Z: 16}, 0.175); ok {
		t.Fatalf("clear excess accepted (obs %v)", obs)
	}
	// Non-finite candidates fail with NaN distance.
	for _, c := range []world.Vec3{
		{X: math.NaN(), Z: 16}, {X: 16, Z: math.Inf(1)},
		{X: math.Inf(-1), Z: math.Inf(1)},
	} {
		if ok, obs := checkDisplacement(base, c, 0.175); ok || !math.IsNaN(obs) {
			t.Fatalf("non-finite %v accepted (ok=%v obs=%v)", c, ok, obs)
		}
	}
}

// traceMovement renders tick, ordered entity state (pos/yaw/speed/
// anchor/flags), history tails, and per-tick movement updates.
func traceMovement(e *Engine, updates []MovementUpdate) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "tick:%d\n", e.CurrentTick())
	for _, c := range e.CellCoords() {
		fmt.Fprintf(&sb, "cell:%d,%d\n", c.X, c.Z)
		for _, s := range e.EntitiesInCell(c) {
			fmt.Fprintf(&sb, "ent:%d pos:%.3f,%.3f,%.3f yaw:%d spd:%d anc:%d flg:%d cell:%d,%d\n",
				uint64(s.ID), s.Position.X, s.Position.Y, s.Position.Z,
				s.Yaw, s.Speed, s.LastProcessedInputSeq, uint32(s.VolumeFlags),
				s.Cell.X, s.Cell.Z)
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
		fmt.Fprintf(&sb, "upd:%d ent:%d %.3f,%.3f yaw:%d spd:%d anc:%d blk:%v ho:%v flg:%d\n",
			u.Tick, uint64(u.EntityID), u.Position.X, u.Position.Z,
			u.Yaw, u.Speed, u.LastProcessedInputSeq, u.Blocked, u.HandoffRequired,
			uint32(u.VolumeFlags))
	}
	return sb.String()
}

func runMovementScript(e *Engine, sink *recordSink, ticks int) string {
	// Deterministic layout: cell centers with wide margins so no
	// scripted step ever stages a handoff (T3a owns transfers).
	centers := []world.Vec3{
		{X: 16, Z: 16}, {X: -48, Z: 16}, {X: 80, Z: -16},
		{X: -112, Z: -48}, {X: 144, Z: 48}, {X: -16, Z: 112},
	}
	var ids []EntityID
	for _, c := range centers {
		s, err := e.AddEntity(c)
		if err != nil {
			panic(fmt.Sprintf("movement script add: %v", err))
		}
		ids = append(ids, s.ID)
	}
	var sb strings.Builder
	for k := 1; k <= ticks; k++ {
		before := len(sink.all())
		// Walk forward; run on even ticks (denied for id%4==0 by gate).
		for j, id := range ids {
			run := uint8(0)
			if k%2 == 0 {
				run = 1
			}
			dirs := uint8(MoveDirForward)
			yaw := uint16((k * 256) % 4096)
			switch {
			case k%7 == 0:
				dirs = 0 // stop input
			case k%5 == 0:
				dirs = MoveDirForward | MoveDirRight // diagonal
			case k%11 == 0:
				// Coalesced burst: three accepts, only the last runs.
				_, _ = e.SubmitMove(id, MoveIntent{InputSeq: uint32(k*10 + j), HeldDirs: MoveDirLeft, SampleTick: e.CurrentTick()})
				_, _ = e.SubmitMove(id, MoveIntent{InputSeq: uint32(k*10 + j + 1), HeldDirs: MoveDirRight, SampleTick: e.CurrentTick()})
				dirs = MoveDirBackward
			}
			// One entity exercises the input-sequence wrap domain.
			seq := uint32(k*3 + j)
			if j == 0 {
				seq = uint32(math.MaxUint32 - 40 + k)
			}
			if k%13 == 0 && j == 1 {
				// Stale resubmit of an old sequence: must not disturb.
				_, _ = e.SubmitMove(id, MoveIntent{InputSeq: 1, HeldDirs: MoveDirBackward, SampleTick: e.CurrentTick()})
			}
			if _, err := e.SubmitMove(id, MoveIntent{
				InputSeq: seq, HeldDirs: dirs, RunFlag: run, Yaw: yaw, SampleTick: e.CurrentTick(),
			}); err != nil {
				panic(fmt.Sprintf("movement script tick %d: %v", k, err))
			}
		}
		e.Step()
		sb.WriteString(traceMovement(e, sink.all()[before:]))
	}
	return sb.String()
}

func movementTestDeps(clk Clock, rng RNG, sink *recordSink) EngineDeps {
	// Wall at X>=1000 (never reached): proves collision plumbed without
	// interfering; run denied for every fourth entity.
	return EngineDeps{
		Clock:     clk,
		RNG:       rng,
		Collision: &fakeCollision{solid: func(p world.Vec3) bool { return p.X >= 1000 }},
		RunGate:   RunGateFunc(func(id EntityID) bool { return uint64(id)%4 != 0 }),
		Movement:  sink,
	}
}

func TestEngineMovementDeterministicTrace(t *testing.T) {
	mk := func() (*Engine, *recordSink) {
		sink := &recordSink{}
		e := mustEngine(t, 20, movementTestDeps(newManualClock(), newTestRNG(777), sink))
		return e, sink
	}
	eA, sinkA := mk()
	traceA := runMovementScript(eA, sinkA, 40)
	eB, sinkB := mk()
	traceB := runMovementScript(eB, sinkB, 40)
	if traceA != traceB {
		t.Fatal("same-script movement traces differ")
	}
	for _, want := range []string{"tick:40\n", "upd:", "hist:", "anc:", "spd:70", "spd:35", "spd:0"} {
		if !strings.Contains(traceA, want) {
			t.Fatalf("movement trace missing %q", want)
		}
	}
}

func TestMovementUpdateProtoCompatible(t *testing.T) {
	// TEST-ONLY meters→millimeters conversion proving MovementUpdate
	// carries every semantic field 205 needs. Production conversion
	// waits for M4-T5; M4-T5 also owns the real NetEntityID mapping
	// (the constant below stands in for it).
	sink := &recordSink{}
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1), Movement: sink})
	snap, _ := e.AddEntity(world.Vec3{X: 16.5, Y: 1.25, Z: -3.75})
	submitMove(t, e, snap.ID, 9, MoveDirForward, 1, 3000)
	e.Step()
	u := sink.all()
	if len(u) != 1 {
		t.Fatalf("updates = %d, want 1", len(u))
	}
	mm := func(m float64) int32 { return int32(math.Round(m * 1000)) }
	wire := proto.EntityMove{
		Entity:                42, // stand-in: M4-T5 owns NetEntityID mapping
		Pos:                   proto.Position{X: mm(u[0].Position.X), Y: mm(u[0].Position.Y), Z: mm(u[0].Position.Z)},
		Angle:                 u[0].Yaw,
		Speed:                 u[0].Speed,
		LastProcessedInputSeq: u[0].LastProcessedInputSeq,
	}
	enc := proto.NewEncoder()
	wire.Encode(enc)
	raw, err := enc.Bytes()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	dec, err := proto.DecodeEntityMove(proto.NewDecoder(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dec != wire {
		t.Fatalf("round trip = %+v, want %+v", dec, wire)
	}
	if dec.Angle != 3000 || dec.Speed != 70 || dec.LastProcessedInputSeq != 9 {
		t.Fatalf("fields = %+v", dec)
	}
	// Meters round-trip through millimeter fixed point off the update's
	// own authoritative position (which movement advanced one run step).
	wantPos := proto.Position{X: mm(u[0].Position.X), Y: mm(u[0].Position.Y), Z: mm(u[0].Position.Z)}
	if dec.Pos != wantPos {
		t.Fatalf("pos = %+v, want %v", dec.Pos, wantPos)
	}
}
