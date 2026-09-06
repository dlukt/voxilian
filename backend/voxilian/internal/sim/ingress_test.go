package sim

import (
	"context"
	"errors"
	"math/rand/v2"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/dlukt/voxilian/internal/world"
)

// ingressTestTick is the deterministic pulse payload for manual
// tickers (timestamps are ignored by the owner loop).
var ingressTestTick = time.Unix(1_700_000_000, 0).UTC()

// runOwner starts Engine.Run and waits until this Run owns the
// engine, returning the cancel func and the Run result channel.
func runOwner(t *testing.T, e *Engine) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		e.runState.mu.Lock()
		running := e.runState.running
		e.runState.mu.Unlock()
		if running {
			return cancel, done
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for Run ownership")
		}
		runtime.Gosched()
	}
}

// stopOwner cancels the run and waits for Run to return nil.
func stopOwner(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("timeout waiting for Run exit")
	}
}

// waitMailboxLen spins (bounded, no sleep) until len(e.ingress) == n.
func waitMailboxLen(t *testing.T, e *Engine, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for len(e.ingress) != n {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for mailbox len %d (have %d)", n, len(e.ingress))
		}
		runtime.Gosched()
	}
}

// blockingCollision stalls VolumeFlagsAt for registered positions
// (simulating slow owner work deterministically) and signals entry.
type blockingCollision struct {
	openCollision
	mu      sync.Mutex
	blocked map[world.Vec3]chan struct{}
	entered chan world.Vec3
}

func newBlockingCollision() *blockingCollision {
	return &blockingCollision{
		blocked: make(map[world.Vec3]chan struct{}),
		entered: make(chan world.Vec3, 1024),
	}
}

func (b *blockingCollision) block(pos world.Vec3) chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan struct{})
	b.blocked[pos] = ch
	return ch
}

func (b *blockingCollision) VolumeFlagsAt(p world.Vec3) world.VolumeFlags {
	b.mu.Lock()
	ch := b.blocked[p]
	b.mu.Unlock()
	if ch != nil {
		b.entered <- p
		<-ch
	}
	return b.openCollision.VolumeFlagsAt(p)
}

// waitEntered waits for the owner to enter VolumeFlagsAt for pos.
func waitEntered(t *testing.T, b *blockingCollision, pos world.Vec3) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case got := <-b.entered:
			if got == pos {
				return
			}
		case <-deadline:
			t.Fatalf("timeout waiting for owner at %v", pos)
		}
	}
}

func ingressEngine(t *testing.T, clk *manualClock, col CollisionWorld) *Engine {
	t.Helper()
	return mustEngine(t, 20, EngineDeps{
		Clock: clk, RNG: newTestRNG(7), Collision: col, RunGate: staticGate{allow: true},
	})
}

func TestRunOwnerConcurrentSecondFails(t *testing.T) {
	e := ingressEngine(t, newManualClock(), openCollision{})
	cancel, done := runOwner(t, e)
	defer stopOwner(t, cancel, done)
	if err := e.Run(context.Background()); !errors.Is(err, ErrEngineAlreadyRunning) {
		t.Fatalf("second concurrent Run = %v, want ErrEngineAlreadyRunning", err)
	}
	if tick := e.CurrentTick(); tick != 0 {
		t.Fatalf("tick advanced without pulses: %d", tick)
	}
}

func TestEnqueueBeforeRunNotRunning(t *testing.T) {
	e := ingressEngine(t, newManualClock(), openCollision{})
	ctx := context.Background()
	if _, err := e.EnqueueAddEntity(ctx, world.Vec3{}); !errors.Is(err, ErrEngineNotRunning) {
		t.Errorf("add before Run = %v", err)
	}
	if err := e.EnqueueRemoveEntity(ctx, EntityID(1)); !errors.Is(err, ErrEngineNotRunning) {
		t.Errorf("remove before Run = %v", err)
	}
	if _, err := e.EnqueueMove(ctx, EntityID(1), MoveIntent{InputSeq: 1}); !errors.Is(err, ErrEngineNotRunning) {
		t.Errorf("move before Run = %v", err)
	}
	if n := e.EntityCount(); n != 0 {
		t.Errorf("EntityCount = %d after rejected enqueues", n)
	}
}

func TestIngressCapacity256(t *testing.T) {
	col := newBlockingCollision()
	e := ingressEngine(t, newManualClock(), col)
	cancel, done := runOwner(t, e)

	blockPos := world.Vec3{X: 1}
	release := col.block(blockPos)
	first := make(chan ingressAddResult, 1)
	go func() {
		snap, err := e.EnqueueAddEntity(context.Background(), blockPos)
		first <- ingressAddResult{snap: snap, err: err}
	}()
	waitEntered(t, col, blockPos)

	// Owner holds #1; fill exactly the 256 mailbox slots from bounded
	// goroutines (no goroutine explosion: fixed set, all joined).
	type addOut struct {
		snap EntitySnapshot
		err  error
	}
	results := make(chan addOut, SimIngressCapacity)
	for i := 0; i < SimIngressCapacity; i++ {
		go func(i int) {
			snap, err := e.EnqueueAddEntity(context.Background(), world.Vec3{X: float64(100 + i)})
			results <- addOut{snap: snap, err: err}
		}(i)
	}
	waitMailboxLen(t, e, SimIngressCapacity)
	if _, err := e.EnqueueAddEntity(context.Background(), world.Vec3{X: 9999}); !errors.Is(err, ErrSimIngressFull) {
		t.Fatalf("257th command = %v, want ErrSimIngressFull", err)
	}
	if _, err := e.EnqueueMove(context.Background(), EntityID(1), MoveIntent{InputSeq: 1}); !errors.Is(err, ErrSimIngressFull) {
		t.Fatalf("move on full mailbox = %v, want ErrSimIngressFull", err)
	}
	close(release)
	if res := <-first; res.err != nil {
		t.Fatalf("first add: %v", res.err)
	}
	seen := make(map[EntityID]bool)
	for i := 0; i < SimIngressCapacity; i++ {
		select {
		case r := <-results:
			if r.err != nil {
				t.Fatalf("queued add #%d: %v", i, r.err)
			}
			if seen[r.snap.ID] {
				t.Fatalf("duplicate entity ID %d", uint64(r.snap.ID))
			}
			seen[r.snap.ID] = true
		case <-time.After(10 * time.Second):
			t.Fatalf("timeout waiting for queued add #%d", i)
		}
	}
	// Owner stopped before direct observation: the rejected 257th
	// command must have mutated nothing, so exactly 1 + 256
	// executed adds exist (a leaked rejection would read 258).
	stopOwner(t, cancel, done)
	if n := e.EntityCount(); n != SimIngressCapacity+1 {
		t.Fatalf("EntityCount = %d, want %d", n, SimIngressCapacity+1)
	}
}

func TestEnqueueCancelledBeforeAdmission(t *testing.T) {
	e := ingressEngine(t, newManualClock(), openCollision{})
	cancel, done := runOwner(t, e)
	defer stopOwner(t, cancel, done)
	ctx, stop := context.WithCancel(context.Background())
	stop()
	if _, err := e.EnqueueAddEntity(ctx, world.Vec3{X: 3}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled add = %v, want context.Canceled", err)
	}
	if err := e.EnqueueRemoveEntity(ctx, EntityID(1)); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled remove = %v, want context.Canceled", err)
	}
	if _, err := e.EnqueueMove(ctx, EntityID(1), MoveIntent{}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled move = %v, want context.Canceled", err)
	}
	if n := e.EntityCount(); n != 0 {
		t.Errorf("cancelled admission mutated: count %d", n)
	}
}

func TestEnqueueCancelledAfterAdmissionExecutes(t *testing.T) {
	col := newBlockingCollision()
	e := ingressEngine(t, newManualClock(), col)
	cancel, done := runOwner(t, e)
	defer stopOwner(t, cancel, done)

	release := col.block(world.Vec3{X: 1})
	firstDone := make(chan error, 1)
	go func() {
		_, err := e.EnqueueAddEntity(context.Background(), world.Vec3{X: 1})
		firstDone <- err
	}()
	waitEntered(t, col, world.Vec3{X: 1})

	// Admit a second command, then cancel its caller: the admitted
	// command is authoritative and still executes exactly once.
	callCtx, stopCall := context.WithCancel(context.Background())
	secondDone := make(chan ingressMoveResult, 1)
	go func() {
		disp, err := e.EnqueueMove(callCtx, EntityID(999), MoveIntent{InputSeq: 1})
		secondDone <- ingressMoveResult{disp: disp, err: err}
	}()
	waitMailboxLen(t, e, 1)
	stopCall()
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first add: %v", err)
	}
	select {
	case res := <-secondDone:
		// Unknown entity: definitive owner-local result, not a
		// cancellation ambiguity.
		if !errors.Is(res.err, ErrEntityNotFound) {
			t.Fatalf("post-cancel move = %v,%v; want ErrEntityNotFound", res.disp, res.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("admitted command never completed after caller cancel")
	}
}

func TestRunStopDrainsQueued(t *testing.T) {
	col := newBlockingCollision()
	clk := newManualClock()
	e := ingressEngine(t, clk, col)
	cancel, done := runOwner(t, e)

	release := col.block(world.Vec3{X: 1})
	firstDone := make(chan error, 1)
	go func() {
		_, err := e.EnqueueAddEntity(context.Background(), world.Vec3{X: 1})
		firstDone <- err
	}()
	waitEntered(t, col, world.Vec3{X: 1})

	queued := make([]chan error, 3)
	for i := range queued {
		queued[i] = make(chan error, 1)
		go func(i int, ch chan error) {
			_, err := e.EnqueueAddEntity(context.Background(), world.Vec3{X: float64(10 + i)})
			ch <- err
		}(i, queued[i])
	}
	waitMailboxLen(t, e, len(queued))
	cancel()
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("in-flight add: %v", err)
	}
	for i, ch := range queued {
		select {
		case err := <-ch:
			if !errors.Is(err, ErrEngineStopped) {
				t.Fatalf("queued add #%d = %v, want ErrEngineStopped", i, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("queued add #%d waiter hung", i)
		}
	}
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}

	// Sequential restart owns a fresh empty generation: no stopped
	// command reappears, and new work executes.
	cancel2, done2 := runOwner(t, e)
	snap, err := e.EnqueueAddEntity(context.Background(), world.Vec3{X: 42})
	if err != nil || snap.ID == InvalidEntityID {
		t.Fatalf("post-restart add = %+v,%v", snap, err)
	}
	stopOwner(t, cancel2, done2)
	if n := e.EntityCount(); n != 2 {
		t.Fatalf("EntityCount after restart = %d, want 2 (only executed adds)", n)
	}
}

func TestIngressTickPriorityOverBacklog(t *testing.T) {
	col := newBlockingCollision()
	clk := newManualClock()
	e := ingressEngine(t, clk, col)
	cancel, done := runOwner(t, e)
	defer stopOwner(t, cancel, done)

	p1 := world.Vec3{X: 1}
	r1 := col.block(p1)
	firstDone := make(chan error, 1)
	go func() {
		_, err := e.EnqueueAddEntity(context.Background(), p1)
		firstDone <- err
	}()
	waitEntered(t, col, p1)

	p2 := world.Vec3{X: 2}
	r2 := col.block(p2)
	secondDone := make(chan error, 1)
	go func() {
		_, err := e.EnqueueAddEntity(context.Background(), p2)
		secondDone <- err
	}()
	waitMailboxLen(t, e, 1)
	clk.firstTicker().pulse(ingressTestTick)

	// Release the in-flight command. The ready tick MUST execute
	// before the loop starts the queued #2: when #2 begins (blocks),
	// the tick is already done.
	close(r1)
	if err := <-firstDone; err != nil {
		t.Fatalf("first add: %v", err)
	}
	waitEntered(t, col, p2)
	if tick := e.CurrentTick(); tick != 1 {
		t.Fatalf("tick = %d when queued command began; want 1 (tick first)", tick)
	}
	close(r2)
	if err := <-secondDone; err != nil {
		t.Fatalf("second add: %v", err)
	}
}

func TestEnqueueAddRealEngine(t *testing.T) {
	e := ingressEngine(t, newManualClock(), openCollision{})
	cancel, done := runOwner(t, e)
	pos := world.Vec3{X: 4, Y: 0, Z: -8}
	snap, err := e.EnqueueAddEntity(context.Background(), pos)
	if err != nil {
		t.Fatalf("EnqueueAddEntity: %v", err)
	}
	if snap.ID == InvalidEntityID || snap.Position != pos {
		t.Fatalf("snapshot = %+v, want live ID at %v", snap, pos)
	}
	stopOwner(t, cancel, done)
	// Direct registry state after Run stopped: the one executed add.
	if n := e.EntityCount(); n != 1 {
		t.Fatalf("EntityCount after stop = %d", n)
	}
	got, err := e.Entity(snap.ID)
	if err != nil || got.Position != pos {
		t.Fatalf("registry state = %+v,%v", got, err)
	}
}

func TestEnqueueRemoveRealEngine(t *testing.T) {
	e := ingressEngine(t, newManualClock(), openCollision{})
	cancel, done := runOwner(t, e)
	ctx := context.Background()
	snap, err := e.EnqueueAddEntity(ctx, world.Vec3{X: 5})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := e.EnqueueRemoveEntity(ctx, snap.ID); err != nil {
		t.Fatalf("remove: %v", err)
	}
	stopOwner(t, cancel, done)
	if n := e.EntityCount(); n != 0 {
		t.Fatalf("EntityCount = %d after remove", n)
	}
	// Removal of the missing entity through a restarted owner keeps
	// the exact ErrEntityNotFound semantics.
	cancel2, done2 := runOwner(t, e)
	if err := e.EnqueueRemoveEntity(ctx, snap.ID); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("second remove = %v, want ErrEntityNotFound", err)
	}
	stopOwner(t, cancel2, done2)
}

func TestEnqueueMoveRealEngine(t *testing.T) {
	clk := newManualClock()
	e := ingressEngine(t, clk, openCollision{})
	cancel, done := runOwner(t, e)
	ctx := context.Background()
	snap, err := e.EnqueueAddEntity(ctx, world.Vec3{})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	intent := MoveIntent{InputSeq: 1, HeldDirs: MoveDirForward, Yaw: 0, SampleTick: 0}
	disp, err := e.EnqueueMove(ctx, snap.ID, intent)
	if err != nil || disp != MoveAccepted {
		t.Fatalf("move = %v,%v; want accepted,nil", disp, err)
	}
	// Control only: stop the owner and prove no movement happened
	// before any tick (direct reads need a stopped owner).
	stopOwner(t, cancel, done)
	if got, _ := e.Entity(snap.ID); got.Position != (world.Vec3{}) {
		t.Fatalf("position moved before tick: %v", got.Position)
	}
	// Restart, pulse exactly one manual tick, stop, then observe.
	cancel2, done2 := runOwner(t, e)
	currentTicker(clk).pulse(ingressTestTick)
	waitForTick(t, e, 1)
	stopOwner(t, cancel2, done2)
	got, err := e.Entity(snap.ID)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	// Walk 3.5 m/s forward (yaw 0 faces -Z) for one 20 Hz step
	// (float64 arithmetic: compare within 1e-9).
	if want := -3.5 / 20.0; got.Position.X != 0 || got.Position.Z < want-1e-9 || got.Position.Z > want+1e-9 {
		t.Fatalf("position = %v, want {0,0,%v}", got.Position, want)
	}
	if got.LastProcessedInputSeq != 1 {
		t.Fatalf("anchor = %d, want 1", got.LastProcessedInputSeq)
	}
}

func TestEnqueueMoveDuplicateStale(t *testing.T) {
	e := ingressEngine(t, newManualClock(), openCollision{})
	cancel, done := runOwner(t, e)
	defer stopOwner(t, cancel, done)
	ctx := context.Background()
	snap, err := e.EnqueueAddEntity(ctx, world.Vec3{})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	mk := func(seq uint32) MoveIntent {
		return MoveIntent{InputSeq: seq, HeldDirs: MoveDirForward, Yaw: 0, SampleTick: 0}
	}
	if disp, err := e.EnqueueMove(ctx, snap.ID, mk(5)); err != nil || disp != MoveAccepted {
		t.Fatalf("seq 5 = %v,%v", disp, err)
	}
	if disp, err := e.EnqueueMove(ctx, snap.ID, mk(5)); err != nil || disp != MoveDuplicate {
		t.Fatalf("seq 5 again = %v,%v; want duplicate", disp, err)
	}
	if disp, err := e.EnqueueMove(ctx, snap.ID, mk(4)); err != nil || disp != MoveStale {
		t.Fatalf("seq 4 = %v,%v; want stale", disp, err)
	}
	if disp, err := e.EnqueueMove(ctx, snap.ID, mk(6)); err != nil || disp != MoveAccepted {
		t.Fatalf("seq 6 = %v,%v; want accepted", disp, err)
	}
}

func TestEnqueueMoveMigrationQueue(t *testing.T) {
	clk := newManualClock()
	e := ingressEngine(t, clk, openCollision{})
	// Stage the migration owner-locally while no Run is active (the
	// test is the sim owner here): then Run owns execution while the
	// gateway-like caller submits through the mailbox.
	snap, err := e.AddEntity(world.Vec3{})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	ent, err := e.registry.lookup(snap.ID)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	_, err = e.registry.beginHandoff(snap.ID,
		OwnerRef{Cell: ent.cell, Generation: ent.generation},
		world.CellCoord{X: 1, Z: 0}, world.Vec3{X: 33})
	if err != nil {
		t.Fatalf("beginHandoff: %v", err)
	}
	cancel, done := runOwner(t, e)
	ctx := context.Background()
	mk := func(seq uint32) MoveIntent {
		return MoveIntent{InputSeq: seq, HeldDirs: MoveDirForward, Yaw: 0, SampleTick: 0}
	}
	for seq := uint32(1); seq <= MigrationMoveQueueCapacity; seq++ {
		if disp, err := e.EnqueueMove(ctx, snap.ID, mk(seq)); err != nil || disp != MoveAccepted {
			t.Fatalf("queued seq %d = %v,%v", seq, disp, err)
		}
	}
	if _, err := e.EnqueueMove(ctx, snap.ID, mk(MigrationMoveQueueCapacity+1)); !errors.Is(err, ErrMigrationQueueFull) {
		t.Fatalf("65th queued move = %v, want ErrMigrationQueueFull", err)
	}
	// The rejected sequence was NOT consumed: after aborting the
	// migration it submits cleanly as a resident intent.
	stopOwner(t, cancel, done)
	if !e.registry.abortHandoff(snap.ID) {
		t.Fatalf("abortHandoff failed")
	}
	if disp, err := e.SubmitMove(snap.ID, mk(MigrationMoveQueueCapacity+1)); err != nil || disp != MoveAccepted {
		t.Fatalf("retry seq 65 after abort = %v,%v; want accepted", disp, err)
	}
}

func TestIngressRaceRunPlusMoves(t *testing.T) {
	clk := newManualClock()
	e := ingressEngine(t, clk, openCollision{})
	cancel, done := runOwner(t, e)
	ctx := context.Background()
	snap, err := e.EnqueueAddEntity(ctx, world.Vec3{})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 1; i <= 25; i++ {
				_, _ = e.EnqueueMove(ctx, snap.ID, MoveIntent{
					InputSeq:   uint32(w*100 + i),
					HeldDirs:   MoveDirForward,
					Yaw:        0,
					SampleTick: e.CurrentTick(),
				})
			}
		}(w)
	}
	for i := 0; i < 10; i++ {
		currentTicker(clk).pulse(ingressTestTick)
	}
	wg.Wait()
	stopOwner(t, cancel, done)
	if _, err := e.Entity(snap.ID); err != nil {
		t.Fatalf("entity lost under race: %v", err)
	}
}

// currentTicker returns the newest manual ticker under the clock
// lock (restarts create a new ticker per Run).
func currentTicker(c *manualClock) *manualTicker {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tickers[len(c.tickers)-1]
}

func TestIngressPropertyModel(t *testing.T) {
	for seed := uint64(0); seed < 64; seed++ {
		rng := rand.New(rand.NewPCG(seed, seed^0x9E3779B97F4A7C15))
		clk := newManualClock()
		e := ingressEngine(t, clk, openCollision{})
		ctx := context.Background()
		var runCancel context.CancelFunc
		var runDone <-chan error
		running := false
		live := make(map[EntityID]bool)
		lastTick := uint32(0)
		start := func() {
			runCancel, runDone = runOwner(t, e)
			running = true
		}
		stop := func() {
			stopOwner(t, runCancel, runDone)
			running = false
		}
		start()
		for step := 0; step < 128; step++ {
			switch rng.IntN(7) {
			case 0: // synchronous add
				snap, err := e.EnqueueAddEntity(ctx, world.Vec3{X: float64(step) + 0.5})
				if err == nil {
					if live[snap.ID] {
						t.Fatalf("seed %d: duplicate add ID %d", seed, uint64(snap.ID))
					}
					live[snap.ID] = true
				} else if !errors.Is(err, ErrSimIngressFull) {
					t.Fatalf("seed %d: add = %v", seed, err)
				}
			case 1: // async add racing Run stop: success, Stopped,
				// or NotRunning are all model-consistent
				resCh := make(chan ingressAddResult, 1)
				go func(x float64) {
					snap, err := e.EnqueueAddEntity(ctx, world.Vec3{X: x})
					resCh <- ingressAddResult{snap: snap, err: err}
				}(float64(step) + 0.25)
				stop()
				res := <-resCh
				if res.err == nil {
					if live[res.snap.ID] {
						t.Fatalf("seed %d: duplicate add ID %d", seed, uint64(res.snap.ID))
					}
					live[res.snap.ID] = true
				} else if !errors.Is(res.err, ErrEngineStopped) && !errors.Is(res.err, ErrEngineNotRunning) {
					t.Fatalf("seed %d: async add = %v", seed, res.err)
				}
				start()
			case 2: // move a live entity
				for id := range live {
					disp, err := e.EnqueueMove(ctx, id, MoveIntent{
						InputSeq:   uint32(step + 1),
						HeldDirs:   MoveDirForward,
						Yaw:        0,
						SampleTick: e.CurrentTick(),
					})
					if err != nil && !errors.Is(err, ErrSimIngressFull) {
						t.Fatalf("seed %d: move = %v", seed, err)
					}
					_ = disp
					break
				}
			case 3: // tick
				currentTicker(clk).pulse(ingressTestTick)
			case 4: // remove a live entity
				for id := range live {
					if err := e.EnqueueRemoveEntity(ctx, id); err == nil {
						delete(live, id)
					} else if !errors.Is(err, ErrSimIngressFull) && !errors.Is(err, ErrEntityNotFound) {
						t.Fatalf("seed %d: remove = %v", seed, err)
					}
					break
				}
			case 5: // cancel run: model/runner agreement is
				// checked on the stopped owner before restart
				stop()
				if n := e.EntityCount(); n != len(live) {
					t.Fatalf("seed %d step %d: count %d, model %d", seed, step, n, len(live))
				}
			case 6: // observe tick monotonicity
				if tick := e.CurrentTick(); tick < lastTick {
					t.Fatalf("seed %d: tick regressed %d->%d", seed, lastTick, tick)
				} else {
					lastTick = tick
				}
			}
			if !running {
				start()
			}
		}
		stop()
		if n := e.EntityCount(); n != len(live) {
			t.Fatalf("seed %d: final count %d, model %d", seed, n, len(live))
		}
	}
}
