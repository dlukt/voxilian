package gateway

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/dlukt/voxilian/internal/session"
	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/world"
)

var worldRuntimeBase = time.Unix(1_700_000_000, 0).UTC()

// ---------------------------------------------------------------------------
// shared fakes
// ---------------------------------------------------------------------------

// recordedMove is one EnqueueMove observation.
type recordedMove struct {
	id     sim.EntityID
	intent sim.MoveIntent
}

// recordingSim is a scriptable SimIngress fake.
type recordingSim struct {
	mu        sync.Mutex
	adds      int
	addErr    error
	nextID    sim.EntityID
	removes   []sim.EntityID
	removeErr error
	moves     []recordedMove
	moveDisp  sim.MoveDisposition
	moveErr   error
	tick      uint32
}

func (f *recordingSim) EnqueueAddEntity(_ context.Context, pos world.Vec3) (sim.EntitySnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.adds++
	if f.addErr != nil {
		return sim.EntitySnapshot{}, f.addErr
	}
	f.nextID++
	cell, err := world.CellForPosition(pos)
	if err != nil {
		return sim.EntitySnapshot{}, err
	}
	return sim.EntitySnapshot{ID: f.nextID, Position: pos, Cell: cell}, nil
}

func (f *recordingSim) EnqueueRemoveEntity(_ context.Context, id sim.EntityID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removes = append(f.removes, id)
	return f.removeErr
}

func (f *recordingSim) EnqueueMove(_ context.Context, id sim.EntityID, intent sim.MoveIntent) (sim.MoveDisposition, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.moves = append(f.moves, recordedMove{id: id, intent: intent})
	return f.moveDisp, f.moveErr
}

func (f *recordingSim) CurrentTick() uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tick
}

func (f *recordingSim) moveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.moves)
}

// recordingDownstream is a scriptable downstream WorldExit fake.
type recordingDownstream struct {
	mu    sync.Mutex
	calls int
	err   error
	log   *eventLog
}

func (f *recordingDownstream) ExitWorld(_ context.Context, _ session.ID, _, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.log != nil {
		f.log.add("downstream")
	}
	return f.err
}

// fakeWorldEnter is a scriptable WorldEnter for handler-chain tests.
type fakeWorldEnter struct {
	mu         sync.Mutex
	calls      []string
	log        *eventLog
	prepareErr error
	commitErr  error
	abortErr   error
}

func (f *fakeWorldEnter) PrepareEnter(_ context.Context, _ session.ID, _, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "prepare")
	if f.log != nil {
		f.log.add("prepare")
	}
	return f.prepareErr
}

func (f *fakeWorldEnter) CommitEnter(_ session.ID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "commit")
	if f.log != nil {
		f.log.add("commit")
	}
	return f.commitErr
}

func (f *fakeWorldEnter) AbortEnter(_ context.Context, _ session.ID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "abort")
	if f.log != nil {
		f.log.add("abort")
	}
	return f.abortErr
}

func (f *fakeWorldEnter) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// ---------------------------------------------------------------------------
// real-sim test clock/deps (gateway package cannot reuse sim's
// unexported manual fakes)
// ---------------------------------------------------------------------------

type gwTestTicker struct{ ch chan time.Time }

func (m *gwTestTicker) C() <-chan time.Time { return m.ch }
func (m *gwTestTicker) Stop()               {}

func (m *gwTestTicker) pulse(t time.Time) { m.ch <- t }

type gwTestClock struct {
	mu      sync.Mutex
	tickers []*gwTestTicker
}

func (c *gwTestClock) NewTicker(d time.Duration) sim.Ticker {
	c.mu.Lock()
	defer c.mu.Unlock()
	mt := &gwTestTicker{ch: make(chan time.Time, 64)}
	c.tickers = append(c.tickers, mt)
	return mt
}

func (c *gwTestClock) current() *gwTestTicker {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tickers[len(c.tickers)-1]
}

type gwTestRNG struct{ v uint64 }

func (r *gwTestRNG) Uint64() uint64 {
	r.v++
	return r.v
}

type gwOpenCollision struct{}

func (gwOpenCollision) SolidAt(world.Vec3) bool { return false }
func (gwOpenCollision) VolumeFlagsAt(world.Vec3) world.VolumeFlags {
	return world.VolumeFlags(0)
}

type gwStaticGate struct{ allow bool }

func (g gwStaticGate) CanRun(sim.EntityID) bool { return g.allow }

func realSimEngine(t *testing.T, clk *gwTestClock) *sim.Engine {
	t.Helper()
	e, err := sim.NewEngine(sim.EngineConfig{TickHz: 20}, sim.EngineDeps{
		Clock: clk, RNG: &gwTestRNG{}, Collision: gwOpenCollision{}, RunGate: gwStaticGate{allow: true},
	})
	if err != nil {
		t.Fatalf("sim.NewEngine: %v", err)
	}
	return e
}

// runSimOwner starts engine Run and waits until this Run owns the
// engine, returning the cancel func and the Run result channel. A
// probe entity admitted through the mailbox proves ownership and is
// removed again, leaving no trace.
func runSimOwner(t *testing.T, e *sim.Engine) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		snap, err := e.EnqueueAddEntity(ctx, world.Vec3{})
		if errors.Is(err, sim.ErrEngineNotRunning) {
			if time.Now().After(deadline) {
				t.Fatalf("timeout waiting for sim Run ownership")
			}
			continue
		}
		if err != nil {
			t.Fatalf("probe add: %v", err)
		}
		if err := e.EnqueueRemoveEntity(ctx, snap.ID); err != nil {
			t.Fatalf("probe remove: %v", err)
		}
		return cancel, done
	}
}

func stopSimOwner(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("timeout waiting for sim Run exit")
	}
}

// ---------------------------------------------------------------------------
// runtime unit tests (fake sim)
// ---------------------------------------------------------------------------

func testRuntime(t *testing.T, fakeSim *recordingSim, now time.Time) (*WorldSessionRuntime, *PresenceRegistry, *recordingDownstream) {
	t.Helper()
	presence, err := NewPresenceRegistry(testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	downstream := &recordingDownstream{}
	spawn := SpawnResolverFunc(func(context.Context, int64, int64) (world.Vec3, error) {
		return world.Vec3{X: 4}, nil
	})
	rt, err := NewWorldSessionRuntime(fakeSim, presence, spawn, func() time.Time { return now }, downstream)
	if err != nil {
		t.Fatal(err)
	}
	return rt, presence, downstream
}

func TestWorldSessionRuntimeRequiresDeps(t *testing.T) {
	presence, _ := NewPresenceRegistry(testPolicy())
	fakeSim := &recordingSim{}
	spawn := SpawnResolverFunc(func(context.Context, int64, int64) (world.Vec3, error) { return world.Vec3{}, nil })
	now := func() time.Time { return worldRuntimeBase }
	downstream := WorldExitFunc(func(context.Context, session.ID, int64, int64) error { return nil })
	if _, err := NewWorldSessionRuntime(nil, presence, spawn, now, downstream); err == nil {
		t.Error("nil sim accepted")
	}
	if _, err := NewWorldSessionRuntime(fakeSim, nil, spawn, now, downstream); err == nil {
		t.Error("nil presence accepted")
	}
	if _, err := NewWorldSessionRuntime(fakeSim, presence, nil, now, downstream); err == nil {
		t.Error("nil spawn accepted")
	}
	if _, err := NewWorldSessionRuntime(fakeSim, presence, spawn, nil, downstream); err == nil {
		t.Error("nil clock accepted")
	}
	if _, err := NewWorldSessionRuntime(fakeSim, presence, spawn, now, nil); err == nil {
		t.Error("nil downstream accepted")
	}
}

func TestRuntimePrepareStagesWithoutPresence(t *testing.T) {
	fakeSim := &recordingSim{}
	rt, presence, _ := testRuntime(t, fakeSim, worldRuntimeBase)
	ctx := context.Background()
	if err := rt.PrepareEnter(ctx, 1, 11, 101); err != nil {
		t.Fatalf("PrepareEnter: %v", err)
	}
	// Mandatory: Presence absent after Prepare, before Commit.
	if _, err := presence.Snapshot(1); !errors.Is(err, ErrPresenceNotFound) {
		t.Fatalf("presence exists before commit: %v", err)
	}
	if fakeSim.adds != 1 {
		t.Fatalf("sim adds = %d, want 1", fakeSim.adds)
	}
	// Second Prepare conflicts with no second entity.
	if err := rt.PrepareEnter(ctx, 1, 11, 101); !errors.Is(err, ErrWorldEntryPending) {
		t.Fatalf("second PrepareEnter = %v, want ErrWorldEntryPending", err)
	}
	if fakeSim.adds != 1 {
		t.Fatalf("conflicting prepare added entity: adds = %d", fakeSim.adds)
	}
}

func TestRuntimePrepareSpawnFailure(t *testing.T) {
	fakeSim := &recordingSim{}
	presence, _ := NewPresenceRegistry(testPolicy())
	downstream := &recordingDownstream{}
	boom := errors.New("world down")
	spawn := SpawnResolverFunc(func(context.Context, int64, int64) (world.Vec3, error) {
		return world.Vec3{}, boom
	})
	rt, err := NewWorldSessionRuntime(fakeSim, presence, spawn, func() time.Time { return worldRuntimeBase }, downstream)
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.PrepareEnter(context.Background(), 1, 11, 101); !errors.Is(err, ErrWorldEntryRetry) {
		t.Fatalf("spawn failure = %v, want ErrWorldEntryRetry", err)
	}
	if fakeSim.adds != 0 {
		t.Fatalf("adds = %d after spawn failure", fakeSim.adds)
	}
	if _, err := presence.Snapshot(1); !errors.Is(err, ErrPresenceNotFound) {
		t.Fatalf("presence after spawn failure: %v", err)
	}
}

func TestRuntimePrepareIngressFullRetry(t *testing.T) {
	fakeSim := &recordingSim{addErr: sim.ErrSimIngressFull}
	rt, presence, _ := testRuntime(t, fakeSim, worldRuntimeBase)
	if err := rt.PrepareEnter(context.Background(), 1, 11, 101); !errors.Is(err, ErrWorldEntryRetry) {
		t.Fatalf("ingress-full prepare = %v, want ErrWorldEntryRetry", err)
	}
	if _, err := presence.Snapshot(1); !errors.Is(err, ErrPresenceNotFound) {
		t.Fatalf("presence after ingress-full: %v", err)
	}
	// Reservation removed: a later healthy Prepare can succeed.
	fakeSim.addErr = nil
	if err := rt.PrepareEnter(context.Background(), 1, 11, 101); err != nil {
		t.Fatalf("healthy retry PrepareEnter: %v", err)
	}
}

func TestRuntimeAbortRemovesStaged(t *testing.T) {
	fakeSim := &recordingSim{}
	rt, presence, _ := testRuntime(t, fakeSim, worldRuntimeBase)
	ctx := context.Background()
	if err := rt.PrepareEnter(ctx, 1, 11, 101); err != nil {
		t.Fatal(err)
	}
	if err := rt.AbortEnter(ctx, 1); err != nil {
		t.Fatalf("AbortEnter: %v", err)
	}
	if len(fakeSim.removes) != 1 || fakeSim.removes[0] != 1 {
		t.Fatalf("removes = %v, want [1]", fakeSim.removes)
	}
	if _, err := presence.Snapshot(1); !errors.Is(err, ErrPresenceNotFound) {
		t.Fatalf("presence after abort: %v", err)
	}
	// Second Abort is the idempotent nil.
	if err := rt.AbortEnter(ctx, 1); err != nil {
		t.Fatalf("second AbortEnter = %v, want nil", err)
	}
	if len(fakeSim.removes) != 1 {
		t.Fatalf("second abort removed again: %v", fakeSim.removes)
	}
}

func TestRuntimeAbortRemovalFailureRetains(t *testing.T) {
	fakeSim := &recordingSim{}
	rt, _, _ := testRuntime(t, fakeSim, worldRuntimeBase)
	ctx := context.Background()
	if err := rt.PrepareEnter(ctx, 1, 11, 101); err != nil {
		t.Fatal(err)
	}
	fakeSim.removeErr = errors.New("sim unavailable")
	if err := rt.AbortEnter(ctx, 1); err == nil {
		t.Fatalf("failing AbortEnter returned nil")
	}
	// Pending retained: a later healthy Abort finishes cleanup.
	fakeSim.removeErr = nil
	if err := rt.AbortEnter(ctx, 1); err != nil {
		t.Fatalf("retry AbortEnter: %v", err)
	}
	if len(fakeSim.removes) != 2 {
		t.Fatalf("removes = %v, want two attempts", fakeSim.removes)
	}
}

func TestRuntimeCommitActivatesPresence(t *testing.T) {
	fakeSim := &recordingSim{}
	now := worldRuntimeBase.Add(time.Minute)
	rt, presence, _ := testRuntime(t, fakeSim, now)
	ctx := context.Background()
	if err := rt.PrepareEnter(ctx, 1, 11, 101); err != nil {
		t.Fatal(err)
	}
	if err := rt.CommitEnter(1); err != nil {
		t.Fatalf("CommitEnter: %v", err)
	}
	snap, err := presence.Snapshot(1)
	if err != nil {
		t.Fatalf("Snapshot after commit: %v", err)
	}
	if snap.SessionID != 1 || snap.CharacterID != 101 || snap.EntityID != 1 {
		t.Fatalf("snapshot identity = %+v", snap)
	}
	if snap.OwnNetID != 1 {
		t.Fatalf("OwnNetID = %d, want 1", snap.OwnNetID)
	}
	if !snap.HeartbeatAt.Equal(now) {
		t.Fatalf("HeartbeatAt = %v, want injected Now %v", snap.HeartbeatAt, now)
	}
	wantCell := world.CellCoord{X: 0, Z: 0} // spawn {4,0,0} -> cell {0,0}
	if snap.CenterCell != wantCell {
		t.Fatalf("CenterCell = %v, want %v", snap.CenterCell, wantCell)
	}
	// Pending gone: second Commit and Abort are terminal no-ops.
	if err := rt.CommitEnter(1); !errors.Is(err, ErrWorldEntryNoPending) {
		t.Fatalf("second CommitEnter = %v, want ErrWorldEntryNoPending", err)
	}
	if err := rt.AbortEnter(ctx, 1); err != nil {
		t.Fatalf("AbortEnter after commit = %v, want nil", err)
	}
	if len(fakeSim.removes) != 0 {
		t.Fatalf("committed entity removed: %v", fakeSim.removes)
	}
}

func TestRuntimeCommitConflictKeepsStaged(t *testing.T) {
	fakeSim := &recordingSim{}
	rt, presence, _ := testRuntime(t, fakeSim, worldRuntimeBase)
	ctx := context.Background()
	if err := rt.PrepareEnter(ctx, 1, 11, 101); err != nil {
		t.Fatal(err)
	}
	// Force an activation conflict behind the runtime's back.
	if _, err := presence.Activate(1, 101, 999, world.CellCoord{}, worldRuntimeBase); err != nil {
		t.Fatal(err)
	}
	err := rt.CommitEnter(1)
	if err == nil || errors.Is(err, ErrWorldEntryRetry) {
		t.Fatalf("conflicting CommitEnter = %v, want internal (not retry)", err)
	}
	// Staged entry retained: AbortEnter still removes the sim entity.
	if err := rt.AbortEnter(ctx, 1); err != nil {
		t.Fatalf("cleanup AbortEnter: %v", err)
	}
	if len(fakeSim.removes) != 1 || fakeSim.removes[0] != 1 {
		t.Fatalf("removes = %v, want staged entity 1", fakeSim.removes)
	}
}

func TestRuntimeExitDownstreamFirst(t *testing.T) {
	log := &eventLog{}
	fakeSim := &recordingSim{}
	presence, _ := NewPresenceRegistry(testPolicy())
	downstream := &recordingDownstream{log: log}
	spawn := SpawnResolverFunc(func(context.Context, int64, int64) (world.Vec3, error) {
		return world.Vec3{X: 4}, nil
	})
	rt, err := NewWorldSessionRuntime(fakeSim, presence, spawn, func() time.Time { return worldRuntimeBase }, downstream)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := rt.PrepareEnter(ctx, 1, 11, 101); err != nil {
		t.Fatal(err)
	}
	if err := rt.CommitEnter(1); err != nil {
		t.Fatal(err)
	}
	// recordingSim removal also logs so order is observable.
	origRemoves := len(fakeSim.removes)
	if err := rt.ExitWorld(ctx, 1, 11, 101); err != nil {
		t.Fatalf("ExitWorld: %v", err)
	}
	if downstream.calls != 1 {
		t.Fatalf("downstream calls = %d, want 1 (first)", downstream.calls)
	}
	if got := log.slice(); len(got) != 1 || got[0] != "downstream" {
		t.Fatalf("event log = %v", got)
	}
	if len(fakeSim.removes) != origRemoves+1 || fakeSim.removes[origRemoves] != 1 {
		t.Fatalf("removes = %v, want staged entity after flush", fakeSim.removes)
	}
	if _, err := presence.Snapshot(1); !errors.Is(err, ErrPresenceNotFound) {
		t.Fatalf("presence survives successful exit: %v", err)
	}
}

func TestRuntimeExitDownstreamFailureIntact(t *testing.T) {
	fakeSim := &recordingSim{}
	rt, presence, downstream := testRuntime(t, fakeSim, worldRuntimeBase)
	ctx := context.Background()
	if err := rt.PrepareEnter(ctx, 1, 11, 101); err != nil {
		t.Fatal(err)
	}
	if err := rt.CommitEnter(1); err != nil {
		t.Fatal(err)
	}
	downstream.err = errors.New("flush unavailable")
	if err := rt.ExitWorld(ctx, 1, 11, 101); err == nil {
		t.Fatalf("failing ExitWorld returned nil")
	}
	if len(fakeSim.removes) != 0 {
		t.Fatalf("sim remove ran despite flush failure: %v", fakeSim.removes)
	}
	if _, err := presence.Snapshot(1); err != nil {
		t.Fatalf("presence lost on failed exit: %v", err)
	}
}

func TestRuntimeExitSimRemoveFailureRetains(t *testing.T) {
	fakeSim := &recordingSim{}
	rt, presence, _ := testRuntime(t, fakeSim, worldRuntimeBase)
	ctx := context.Background()
	if err := rt.PrepareEnter(ctx, 1, 11, 101); err != nil {
		t.Fatal(err)
	}
	if err := rt.CommitEnter(1); err != nil {
		t.Fatal(err)
	}
	fakeSim.removeErr = errors.New("sim unavailable")
	if err := rt.ExitWorld(ctx, 1, 11, 101); err == nil {
		t.Fatalf("failing ExitWorld returned nil")
	}
	if _, err := presence.Snapshot(1); err != nil {
		t.Fatalf("presence lost on failed sim remove: %v", err)
	}
}

func TestRuntimeExitCharacterMismatch(t *testing.T) {
	fakeSim := &recordingSim{}
	rt, _, _ := testRuntime(t, fakeSim, worldRuntimeBase)
	ctx := context.Background()
	if err := rt.PrepareEnter(ctx, 1, 11, 101); err != nil {
		t.Fatal(err)
	}
	if err := rt.CommitEnter(1); err != nil {
		t.Fatal(err)
	}
	if err := rt.ExitWorld(ctx, 1, 11, 999); err == nil {
		t.Fatalf("mismatched exit returned nil")
	}
	if len(fakeSim.removes) != 0 {
		t.Fatalf("mismatched exit removed entity")
	}
}

func TestRuntimeInvalidTrustedSpawn(t *testing.T) {
	clk := new(gwTestClock)
	e := realSimEngine(t, clk)
	cancel, done := runSimOwner(t, e)
	presence, _ := NewPresenceRegistry(testPolicy())
	downstream := &recordingDownstream{}
	nanSpawn := SpawnResolverFunc(func(context.Context, int64, int64) (world.Vec3, error) {
		return world.Vec3{X: math.NaN(), Y: 0, Z: 0}, nil
	})
	rt, err := NewWorldSessionRuntime(e, presence, nanSpawn, func() time.Time { return worldRuntimeBase }, downstream)
	if err != nil {
		t.Fatal(err)
	}
	err = rt.PrepareEnter(context.Background(), 1, 11, 101)
	if !errors.Is(err, ErrWorldSpawnInvalid) {
		t.Fatalf("NaN spawn prepare = %v, want ErrWorldSpawnInvalid (internal, never rewritten)", err)
	}
	if errors.Is(err, ErrWorldEntryRetry) {
		t.Fatalf("invalid trusted spawn classified retry: %v", err)
	}
	if _, err := presence.Snapshot(1); !errors.Is(err, ErrPresenceNotFound) {
		t.Fatalf("presence after invalid spawn: %v", err)
	}
	stopSimOwner(t, cancel, done)
	if n := e.EntityCount(); n != 0 {
		t.Fatalf("EntityCount = %d after rejected spawn", n)
	}
}
