package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dlukt/voxilian/internal/session"
	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/world"
)

// ---------------------------------------------------------------------------
// Post-sim-remove emergency fallback over the REAL FanoutRuntime
// (spec §7.4.6, v0.3.24).
//
// The pre-correction B39 test used a recording fake FanoutLifecycle
// and therefore could not prove cleanup of the real FanoutRuntime
// ready/throttle maps. These tests fail the reliable RemovePresence
// path BEFORE fanout cleanup completes and prove the
// WorldSessionRuntime fallback invalidates real local state.
// ---------------------------------------------------------------------------

// readyCopy snapshots the fanout ready set (white-box, test-only).
func readyCopy(r *FanoutRuntime) map[session.ID]bool {
	r.meta.Lock()
	defer r.meta.Unlock()
	out := make(map[session.ID]bool, len(r.ready))
	for k, v := range r.ready {
		out[k] = v
	}
	return out
}

// throttleOwners counts throttle entries per recipient sid.
func throttleOwners(r *FanoutRuntime) map[session.ID]int {
	r.meta.Lock()
	defer r.meta.Unlock()
	out := make(map[session.ID]int)
	for k := range r.throttle {
		out[k.sid]++
	}
	return out
}

type fallbackChain struct {
	reg        *session.Registry
	presence   *PresenceRegistry
	source     *fakePresentation
	fanout     *FanoutRuntime
	fakeSim    *recordingSim
	downstream *recordingDownstream
	runtime    *WorldSessionRuntime
}

func newFallbackChain(t *testing.T) *fallbackChain {
	t.Helper()
	reg := session.NewRegistry()
	presence, err := NewPresenceRegistry(testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	source := newFakePresentation()
	realFanout, err := NewFanoutRuntime(presence, reg, source, 20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(realFanout.Close)
	fakeSim := &recordingSim{}
	downstream := &recordingDownstream{}
	spawn := SpawnResolverFunc(func(context.Context, int64, int64) (world.Vec3, error) {
		return world.Vec3{X: 4}, nil
	})
	rt, err := NewWorldSessionRuntime(fakeSim, presence, reg, spawn,
		func() time.Time { return worldRuntimeBase }, downstream, realFanout)
	if err != nil {
		t.Fatal(err)
	}
	return &fallbackChain{
		reg: reg, presence: presence, source: source, fanout: realFanout,
		fakeSim: fakeSim, downstream: downstream, runtime: rt,
	}
}

// addRealSession attaches a live session backed by a REAL outbound
// queue over a fake transport (CloseNow-observable).
func (c *fallbackChain) addRealSession(t *testing.T) (session.ID, *methodRecordingProducer, *fakeOutTransport) {
	t.Helper()
	tr := newFakeOutTransport()
	oc := newOutboundConn(OutboundDeps{
		Conn: tr, Registry: c.reg,
		Tick: func() uint32 { return 1000 }, Policy: DefaultOutboundPolicy(),
		Observer: &recordingObserver{},
	})
	rec := &methodRecordingProducer{OutboundProducer: oc, t: t}
	sid := c.reg.Create(rec)
	t.Cleanup(func() { oc.StopOutbound("test complete") })
	return sid, rec, tr
}

// barrier orders after every earlier fanout event.
func (c *fallbackChain) barrier(t *testing.T) {
	t.Helper()
	if err := c.fanout.RemovePresence(context.Background(), session.ID(999999), sim.EntityID(888888)); err != nil {
		t.Fatalf("pump barrier: %v", err)
	}
}

// TestRuntimeFallbackClearsRealFanoutState (B10): with source ready,
// viewer ready, and source/viewer throttle entries present, the
// reliable RemovePresence path fails BEFORE fanout cleanup (pump
// stalled, admission context canceled before publication). After the
// WorldSessionRuntime fallback: source/viewer ready absent, all
// throttle entries for affected sessions absent, removed-entity
// mappings retired, source Presence deactivated, sim entity removed
// exactly once and never resurrected.
func TestRuntimeFallbackClearsRealFanoutState(t *testing.T) {
	c := newFallbackChain(t)
	rt := c.runtime

	srcSid, _, srcTr := c.addRealSession(t)
	viewSid, _, viewTr := c.addRealSession(t)

	ctx := context.Background()
	if err := rt.PrepareEnter(ctx, srcSid, 11, 500); err != nil {
		t.Fatal(err)
	}
	c.source.put(EntityPresentation{EntityID: 1, Position: world.Vec3{X: 4}, Kind: 2, Proto: 7})
	if err := rt.CommitEnter(ctx, srcSid); err != nil {
		t.Fatalf("source commit: %v", err)
	}
	if _, err := c.presence.Activate(viewSid, 501, 9, world.CellCoord{}, worldRuntimeBase); err != nil {
		t.Fatal(err)
	}
	c.source.put(EntityPresentation{EntityID: 9, Position: world.Vec3{X: 1000}, Kind: 2, Proto: 7})
	if err := c.fanout.BootstrapSession(ctx, viewSid); err != nil {
		t.Fatalf("viewer bootstrap: %v", err)
	}
	// Two eligible updates (stride 2): 205s to source (owner) and
	// viewer, establishing throttle epochs for both.
	c.fanout.OnMovement(sim.MovementUpdate{EntityID: 1, Position: world.Vec3{X: 5}, Tick: 10})
	c.fanout.OnMovement(sim.MovementUpdate{EntityID: 1, Position: world.Vec3{X: 5}, Tick: 12})
	c.barrier(t)
	if _, visible, _ := c.presence.VisibleHandle(viewSid, 1); !visible {
		t.Fatalf("viewer never saw entity 1")
	}
	ready := readyCopy(c.fanout)
	if !ready[srcSid] || !ready[viewSid] {
		t.Fatalf("pre-failure ready = %v, want source+viewer ready", ready)
	}
	owners := throttleOwners(c.fanout)
	if owners[srcSid] == 0 || owners[viewSid] == 0 {
		t.Fatalf("pre-failure throttle owners = %v, want source+viewer epochs", owners)
	}

	// Deterministic failure seam: stall the pump inside
	// PresentationSource, fill the 1024-event buffer, start ExitWorld,
	// and cancel its control-admission context before publication.
	c.source.block = make(chan struct{})
	c.source.entered = make(chan sim.EntityID, 2048)
	c.source.put(EntityPresentation{EntityID: 3001, Position: world.Vec3{X: 500}, Kind: 3, Proto: 8})
	c.fanout.OnMovement(sim.MovementUpdate{EntityID: 3001, Position: world.Vec3{X: 6}, Tick: 20})
	select {
	case <-c.source.entered:
	case <-time.After(10 * time.Second):
		t.Fatalf("pump never stalled")
	}
	for i := 0; i < 1100; i++ {
		c.fanout.OnMovement(sim.MovementUpdate{EntityID: 1, Position: world.Vec3{X: 5}, Tick: uint32(100 + i)})
	}
	waitUntil(t, "event buffer full", func() bool { return len(c.fanout.events) == FanoutEventCapacity })

	exitCtx, cancel := context.WithCancel(context.Background())
	exitDone := make(chan error, 1)
	go func() {
		exitDone <- rt.ExitWorld(exitCtx, srcSid, 11, 500)
	}()
	// Sim removal is upstream of fanout removal: once recorded, the
	// next step IS the doomed RemovePresence publication wait.
	waitUntil(t, "sim removal", func() bool { return len(c.fakeSim.removedIDs()) == 1 })
	cancel()
	select {
	case err := <-exitDone:
		if err != nil {
			t.Fatalf("ExitWorld after fanout-remove failure: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("ExitWorld hung in fallback")
	}
	close(c.source.block)

	// Source + viewer ready state absent.
	ready = readyCopy(c.fanout)
	if ready[srcSid] || ready[viewSid] {
		t.Fatalf("post-fallback ready = %v, want neither source nor viewer", ready)
	}
	// All throttle entries for affected sessions absent.
	owners = throttleOwners(c.fanout)
	if owners[srcSid] != 0 || owners[viewSid] != 0 {
		t.Fatalf("post-fallback throttle owners = %v, want none for affected sessions", owners)
	}
	// Removed-entity mappings retired; source Presence deactivated.
	if _, visible, _ := c.presence.VisibleHandle(viewSid, 1); visible {
		t.Fatalf("viewer mapping for removed entity survives fallback")
	}
	if _, err := c.presence.Snapshot(srcSid); !errors.Is(err, ErrPresenceNotFound) {
		t.Fatalf("source presence survives fallback: %v", err)
	}
	if _, err := c.presence.Snapshot(viewSid); err != nil {
		t.Fatalf("viewer presence disturbed by fallback: %v", err)
	}
	// Sim entity removed exactly once, never resurrected.
	if got := c.fakeSim.removedIDs(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("sim removes = %v, want exactly [1]", got)
	}
	// Affected transports failed closed.
	if n := srcTr.closeNowCount(); n == 0 {
		t.Fatalf("source transport never closed by fallback")
	}
	if n := viewTr.closeNowCount(); n == 0 {
		t.Fatalf("viewer transport never closed by fallback")
	}
}

// failRemoveFanout wraps the REAL FanoutRuntime with a deterministically
// failing reliable-removal path (test-only; production semantics
// unchanged). Bootstrap/ForgetSession promote from the real runtime.
type failRemoveFanout struct {
	*FanoutRuntime
	err error
}

func (f failRemoveFanout) RemovePresence(context.Context, session.ID, sim.EntityID) error {
	return f.err
}

// TestRuntimeFallbackNoLeakAcrossCycles (B11): 256 failure/recovery
// lifecycles with fresh monotonically increasing session IDs. After
// each fallback, fanout-local bookkeeping must not grow with retired
// sessions: final ready/throttle cardinality covers only live fanout
// sessions/epochs.
func TestRuntimeFallbackNoLeakAcrossCycles(t *testing.T) {
	c := newFallbackChain(t)
	wrapper := failRemoveFanout{FanoutRuntime: c.fanout, err: errors.New("boom")}
	rt, err := NewWorldSessionRuntime(c.fakeSim, c.presence, c.reg,
		SpawnResolverFunc(func(context.Context, int64, int64) (world.Vec3, error) {
			return world.Vec3{X: 4}, nil
		}),
		func() time.Time { return worldRuntimeBase }, c.downstream, wrapper)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// One permanent live session: the only state allowed to survive.
	// It lives in a far cell whose 49-cell subscription excludes the
	// transient cell, so no fallback ever (correctly) fails it as an
	// affected viewer.
	liveSid, _, _ := c.addRealSession(t)
	liveEntity := sim.EntityID(7000)
	liveCenter, err := world.CellForPosition(world.Vec3{X: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.presence.Activate(liveSid, 900, liveEntity, liveCenter, worldRuntimeBase); err != nil {
		t.Fatal(err)
	}
	c.source.put(EntityPresentation{EntityID: liveEntity, Position: world.Vec3{X: 1000}, Kind: 2, Proto: 7})
	if err := c.fanout.BootstrapSession(ctx, liveSid); err != nil {
		t.Fatal(err)
	}
	// Same-cell movement inside the far cell: establishes the live
	// throttle epoch WITHOUT re-centering the subscription onto the
	// transient cell.
	c.fanout.OnMovement(sim.MovementUpdate{EntityID: liveEntity, Position: world.Vec3{X: 1005}, Tick: 10})
	c.barrier(t)

	var prev session.ID
	for i := 1; i <= 256; i++ {
		sid, _, _ := c.addRealSession(t)
		if sid <= prev {
			t.Fatalf("session IDs not monotonically increasing: %d after %d", uint64(sid), uint64(prev))
		}
		prev = sid
		charID := int64(1000 + i)
		entity := sim.EntityID(5000 + i)
		if _, err := c.presence.Activate(sid, charID, entity, world.CellCoord{}, worldRuntimeBase); err != nil {
			t.Fatalf("iter %d activate: %v", i, err)
		}
		c.source.put(EntityPresentation{EntityID: entity, Position: world.Vec3{X: 4}, Kind: 2, Proto: 7})
		if err := c.fanout.BootstrapSession(ctx, sid); err != nil {
			t.Fatalf("iter %d bootstrap: %v", i, err)
		}
		c.fanout.OnMovement(sim.MovementUpdate{EntityID: entity, Position: world.Vec3{X: 5}, Tick: 10})
		c.barrier(t)
		if err := rt.ExitWorld(ctx, sid, 11, charID); err != nil {
			t.Fatalf("iter %d exit: %v", i, err)
		}
		if _, err := c.presence.Snapshot(sid); !errors.Is(err, ErrPresenceNotFound) {
			t.Fatalf("iter %d source presence survives", i)
		}
	}
	ready := readyCopy(c.fanout)
	if len(ready) != 1 || !ready[liveSid] {
		t.Fatalf("final ready = %v, want only the live session", ready)
	}
	owners := throttleOwners(c.fanout)
	if len(owners) != 1 || owners[liveSid] == 0 {
		t.Fatalf("final throttle owners = %v, want only the live session epoch", owners)
	}
}

// TestFanoutNormalRemoveClearsLocalState (B12 supplement): the healthy
// queued RemovePresence path owns normal cleanup — source ready
// cleared plus source/entity throttle entries cleared — without any
// emergency fallback.
func TestFanoutNormalRemoveClearsLocalState(t *testing.T) {
	f := newFanoutFixture(t, 20)
	a := f.addSession(DefaultOutboundPolicy())
	b := f.addSession(DefaultOutboundPolicy())
	f.activate(a, 101, 1001, world.CellCoord{})
	f.activate(b, 102, 2001, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 1001, Position: world.Vec3{}, Kind: 2, Proto: 7})
	f.source.put(EntityPresentation{EntityID: 2001, Position: world.Vec3{X: 6}, Kind: 2, Proto: 7})
	f.bootstrap(a)
	f.bootstrap(b)
	f.move(1001, world.Vec3{X: 1}, 10, 35, 10, 10)
	f.drainPump()
	owners := throttleOwners(f.fanout)
	if owners[a] == 0 || owners[b] == 0 {
		t.Fatalf("pre-remove throttle owners = %v, want source+observer epochs", owners)
	}
	if err := f.fanout.RemovePresence(context.Background(), a, 1001); err != nil {
		t.Fatalf("RemovePresence: %v", err)
	}
	if got := readyCopy(f.fanout); got[a] {
		t.Fatalf("source still ready after normal remove")
	}
	// Source/entity throttle cleared for every session.
	f.fanout.meta.Lock()
	for k := range f.fanout.throttle {
		if k.sid == a || k.entity == 1001 {
			f.fanout.meta.Unlock()
			t.Fatalf("stale throttle entry survives normal remove: %+v", k)
		}
	}
	f.fanout.meta.Unlock()
	// No fallback ran: no transport was force-closed.
	if n := f.trans[a].closeNowCount(); n != 0 {
		t.Fatalf("source transport closed on the healthy path (%d)", n)
	}
	if n := f.trans[b].closeNowCount(); n != 0 {
		t.Fatalf("observer transport closed on the healthy path (%d)", n)
	}
}
