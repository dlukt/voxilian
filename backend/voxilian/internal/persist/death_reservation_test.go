package persist

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/store"
)

// M5-T5c3c3c1 reservation unit tests (spec §9.5.1i): guaranteed
// bounded executor reservation + prepared activation seam. No
// PG here; the Saver is always real, Store and the owner sink
// are deterministic fakes. The sim interface assertion lives
// in death_reservation.go; these tests pin the concrete
// capacity/state/result behavior the future c3c3c2 owner
// composition depends on.

// permitsFree reads the executor's free queue-permit count
// (same-package inspection of the §9.5.1i accounting).
func permitsFree(t *testing.T, ex *DeathExecutor) int {
	t.Helper()
	ex.mu.Lock()
	defer ex.mu.Unlock()
	return ex.permits
}

// mustReserve fails the test on reservation error.
func mustReserve(t *testing.T, ex *DeathExecutor) *DeathExecutionReservation {
	t.Helper()
	r, err := ex.ReserveImmediateDeath()
	if err != nil {
		t.Fatalf("ReserveImmediateDeath: %v", err)
	}
	return r
}

// mustPrepareValid prepares valid fixture work on a
// reservation, failing the test on error.
func mustPrepareValid(t *testing.T, r *DeathExecutionReservation, work ImmediateDeathPersistenceWork) {
	t.Helper()
	if err := r.PrepareImmediateDeathWork(work.Capture, work.RuntimeInputs); err != nil {
		t.Fatalf("PrepareImmediateDeathWork: %v", err)
	}
}

// mustResult fails the test when no result channel exists.
func mustResult(t *testing.T, r *DeathExecutionReservation) <-chan ImmediateDeathPersistenceResult {
	t.Helper()
	ch, err := r.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	return ch
}

// drainOne non-blockingly receives at most one result,
// reporting how many were immediately available.
func drainOne(ch <-chan ImmediateDeathPersistenceResult) (ImmediateDeathPersistenceResult, int) {
	select {
	case res := <-ch:
		n := 1
		select {
		case <-ch:
			n = 2
		default:
		}
		return res, n
	default:
		return ImmediateDeathPersistenceResult{}, 0
	}
}

// Reservation consumes queue capacity immediately: TWO
// unactivated reservations against QueueCapacity 2 exhaust
// admission even though the actual job channel is still
// empty. This proves reservations are real capacity
// ownership, not advisory flags.
func TestDeathExecutorReservationConsumesCapacity(t *testing.T) {
	s := mustSaverForPersist(t)
	fs := newFakeRecoveryStore()
	sink := &fakeCompletionSink{}
	ex, _ := startExecutor(t, DeathExecutorConfig{
		Workers: 1, QueueCapacity: 2, Store: fs, Saver: s, Sink: sink,
	})
	r1 := mustReserve(t, ex)
	if got := permitsFree(t, ex); got != 1 {
		t.Fatalf("permits = %d, want 1 after first reservation", got)
	}
	r2 := mustReserve(t, ex)
	if got := permitsFree(t, ex); got != 0 {
		t.Fatalf("permits = %d, want 0 after second reservation", got)
	}
	if n := len(ex.queue); n != 0 {
		t.Fatalf("queued jobs = %d, want 0 (reservations publish nothing)", n)
	}
	if _, err := ex.ReserveImmediateDeath(); !errors.Is(err, ErrDeathExecutorQueueFull) {
		t.Fatalf("third reserve err = %v, want ErrDeathExecutorQueueFull", err)
	}
	if fs.deathCalls != 0 || sink.numCalls() != 0 {
		t.Fatalf("store=%d sink=%d, want 0/0 (reservations execute nothing)", fs.deathCalls, sink.numCalls())
	}
	r1.CancelImmediateDeathWork()
	r2.CancelImmediateDeathWork()
	if got := permitsFree(t, ex); got != 2 {
		t.Fatalf("permits = %d, want 2 after both cancelled", got)
	}
}

// Cancel returns capacity: a replacement reservation
// succeeds immediately, repeated Cancel neither panics nor
// releases twice, and capacity never exceeds QueueCapacity.
func TestDeathExecutorReservationCancelReturnsCapacity(t *testing.T) {
	s := mustSaverForPersist(t)
	fs := newFakeRecoveryStore()
	sink := &fakeCompletionSink{}
	ex, _ := startExecutor(t, DeathExecutorConfig{
		Workers: 1, QueueCapacity: 1, Store: fs, Saver: s, Sink: sink,
	})
	r := mustReserve(t, ex)
	if _, err := ex.ReserveImmediateDeath(); !errors.Is(err, ErrDeathExecutorQueueFull) {
		t.Fatalf("reserve while held err = %v, want ErrDeathExecutorQueueFull", err)
	}
	r.CancelImmediateDeathWork()
	r.CancelImmediateDeathWork()
	r.CancelImmediateDeathWork()
	if got := permitsFree(t, ex); got != 1 {
		t.Fatalf("permits = %d, want exactly 1 (no double release)", got)
	}
	replacement := mustReserve(t, ex)
	if got := permitsFree(t, ex); got != 0 {
		t.Fatalf("permits = %d, want 0 while replacement held", got)
	}
	replacement.CancelImmediateDeathWork()
	if got := permitsFree(t, ex); got != 1 {
		t.Fatalf("permits = %d, want 1 after replacement cancelled", got)
	}
	if fs.deathCalls != 0 || sink.numCalls() != 0 {
		t.Fatalf("store=%d sink=%d, want 0/0", fs.deathCalls, sink.numCalls())
	}
}

// Prepare failure returns capacity: invalid runtime inputs
// (and invalid capture) fail with the existing wrapped
// validation error, publish no job, terminate the
// reservation, and return the permit so a new reservation
// succeeds. Zero Store/sink work occurs.
func TestDeathExecutorReservationPrepareFailureReturnsCapacity(t *testing.T) {
	s := mustSaverForPersist(t)
	fs := newFakeRecoveryStore()
	sink := &fakeCompletionSink{}
	ex, _ := startExecutor(t, DeathExecutorConfig{
		Workers: 1, QueueCapacity: 2, Store: fs, Saver: s, Sink: sink,
	})
	work := executorWork(t)
	badInputs := work
	badInputs.RuntimeInputs = sim.PlayerVitalsRuntimeInputs{}
	r1 := mustReserve(t, ex)
	if err := r1.PrepareImmediateDeathWork(badInputs.Capture, badInputs.RuntimeInputs); !errors.Is(err, ErrDeathExecutorInvalid) {
		t.Fatalf("bad-inputs prepare err = %v, want ErrDeathExecutorInvalid", err)
	}
	if got := permitsFree(t, ex); got != 2 {
		t.Fatalf("permits = %d, want 2 after failed prepare", got)
	}
	badCapture := work
	badCapture.Capture.Token.CharacterID = 0
	r2 := mustReserve(t, ex)
	if err := r2.PrepareImmediateDeathWork(badCapture.Capture, badCapture.RuntimeInputs); !errors.Is(err, ErrDeathExecutorInvalid) {
		t.Fatalf("bad-capture prepare err = %v, want ErrDeathExecutorInvalid", err)
	}
	if got := permitsFree(t, ex); got != 2 {
		t.Fatalf("permits = %d, want 2 after second failed prepare", got)
	}
	if n := len(ex.queue); n != 0 {
		t.Fatalf("queued jobs = %d, want 0 (failed prepare publishes nothing)", n)
	}
	if fs.deathCalls != 0 || len(fs.itemCalls) != 0 {
		t.Fatalf("store calls = %d items = %v, want 0 (no Store work)", fs.deathCalls, fs.itemCalls)
	}
	if fs.charCalls != 0 || sink.numCalls() != 0 {
		t.Fatalf("recovery=%d sink=%d, want 0/0", fs.charCalls, sink.numCalls())
	}
	// A failed reservation is terminal: a second Prepare
	// reports without touching permits again.
	if err := r1.PrepareImmediateDeathWork(work.Capture, work.RuntimeInputs); err == nil {
		t.Fatal("second prepare on terminal reservation succeeded, want error")
	}
	if got := permitsFree(t, ex); got != 2 {
		t.Fatalf("permits = %d, want 2 (terminal re-prepare releases nothing)", got)
	}
	// Fresh capacity is fully usable again.
	fresh := mustReserve(t, ex)
	mustPrepareValid(t, fresh, work)
	fresh.CancelImmediateDeathWork()
	if got := permitsFree(t, ex); got != 2 {
		t.Fatalf("permits = %d, want 2 at end", got)
	}
}

// Prepared Cancel gives a definitive result: the SAME
// buffered channel receives ErrDeathReservationCanceled
// exactly once, with zero Store/recovery/sink work. A
// second Cancel does nothing.
func TestDeathExecutorReservationPreparedCancelResult(t *testing.T) {
	s := mustSaverForPersist(t)
	fs := newFakeRecoveryStore()
	sink := &fakeCompletionSink{}
	ex, _ := startExecutor(t, DeathExecutorConfig{
		Workers: 1, QueueCapacity: 2, Store: fs, Saver: s, Sink: sink,
	})
	work := executorWork(t)
	r := mustReserve(t, ex)
	mustPrepareValid(t, r, work)
	ch := mustResult(t, r)
	r.CancelImmediateDeathWork()
	res := awaitResult(t, ch)
	if !errors.Is(res.Err, ErrDeathReservationCanceled) {
		t.Fatalf("result err = %v, want ErrDeathReservationCanceled", res.Err)
	}
	r.CancelImmediateDeathWork()
	if _, n := drainOne(ch); n != 0 {
		t.Fatalf("second cancel produced %d extra results, want 0", n)
	}
	if got := permitsFree(t, ex); got != 2 {
		t.Fatalf("permits = %d, want 2 (exactly one release)", got)
	}
	if fs.deathCalls != 0 || fs.charCalls != 0 || len(fs.itemCalls) != 0 {
		t.Fatalf("store=%d char=%d items=%v, want zero", fs.deathCalls, fs.charCalls, fs.itemCalls)
	}
	if sink.numCalls() != 0 {
		t.Fatalf("sink calls = %d, want 0", sink.numCalls())
	}
}

// Reserved capacity cannot be stolen: with capacity 1 held
// by a prepared-but-unactivated reservation, an ordinary
// TrySubmit of valid work fails QueueFull; activating the
// prepared reservation then succeeds without a QueueFull
// branch and its frozen job executes exactly once.
func TestDeathExecutorReservationCapacityCannotBeStolen(t *testing.T) {
	s := mustSaverForPersist(t)
	trackExecutorRoots(t, s, 0, 0)
	fs := newFakeRecoveryStore()
	sink := &fakeCompletionSink{}
	ex, _ := startExecutor(t, DeathExecutorConfig{
		Workers: 1, QueueCapacity: 1, Store: fs, Saver: s, Sink: sink,
	})
	work := executorWork(t)
	r := mustReserve(t, ex)
	mustPrepareValid(t, r, work)
	if _, err := ex.TrySubmit(executorWork(t)); !errors.Is(err, ErrDeathExecutorQueueFull) {
		t.Fatalf("TrySubmit against held reservation err = %v, want ErrDeathExecutorQueueFull", err)
	}
	ch := mustResult(t, r)
	r.ActivateImmediateDeathWork()
	res := awaitResult(t, ch)
	if res.Err != nil {
		t.Fatalf("activated result err = %v", res.Err)
	}
	if res.Recovered {
		t.Fatal("Recovered = true, want false for normal ack")
	}
	if res.Delivery != sim.DeathCompletionApplied {
		t.Fatalf("delivery = %v, want Applied", res.Delivery)
	}
	if fs.deathCalls != 1 {
		t.Fatalf("store calls = %d, want exactly 1", fs.deathCalls)
	}
	if sink.numCalls() != 1 {
		t.Fatalf("sink calls = %d, want exactly 1", sink.numCalls())
	}
	if got := permitsFree(t, ex); got != 1 {
		t.Fatalf("permits = %d, want 1 after dequeue release", got)
	}
}

// Activate exactly once: repeated and concurrent Activate
// calls publish a single frozen job — one Store
// CommitDeathEntry, one result, at most one owner
// completion, no panic, no duplicate queue entry.
func TestDeathExecutorReservationActivateExactlyOnce(t *testing.T) {
	s := mustSaverForPersist(t)
	trackExecutorRoots(t, s, 0, 0)
	fs := newFakeRecoveryStore()
	sink := &fakeCompletionSink{}
	ex, _ := startExecutor(t, DeathExecutorConfig{
		Workers: 2, QueueCapacity: 4, Store: fs, Saver: s, Sink: sink,
	})
	r := mustReserve(t, ex)
	mustPrepareValid(t, r, executorWork(t))
	ch := mustResult(t, r)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.ActivateImmediateDeathWork()
		}()
	}
	r.ActivateImmediateDeathWork()
	r.ActivateImmediateDeathWork()
	wg.Wait()
	res := awaitResult(t, ch)
	if res.Err != nil {
		t.Fatalf("result err = %v", res.Err)
	}
	if _, n := drainOne(ch); n != 0 {
		t.Fatalf("extra results = %d, want 0 (exactly one result)", n)
	}
	if fs.deathCalls != 1 {
		t.Fatalf("store calls = %d, want exactly 1", fs.deathCalls)
	}
	if sink.numCalls() != 1 {
		t.Fatalf("sink calls = %d, want at most/exactly 1", sink.numCalls())
	}
}

// Prepare deep-freeze pins the future owner-safe Prepare
// boundary: caller mutation of every caller-aliased surface
// after successful Prepare cannot reach the Store request
// or the owner completion.
func TestDeathExecutorReservationPrepareDeepFreeze(t *testing.T) {
	s := mustSaverForPersist(t)
	trackExecutorRoots(t, s, 0, 0)
	work := executorWork(t)
	intended := intendedReq(t, work)
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	fs := newFakeRecoveryStore()
	fs.onDeath = func(req store.DeathEntryRequest) (store.DeathEntryResult, error) {
		once.Do(func() { close(entered) })
		<-release
		return echoDeathResult(req), nil
	}
	sink := &fakeCompletionSink{}
	ex, _ := startExecutor(t, DeathExecutorConfig{
		Workers: 1, QueueCapacity: 4, Store: fs, Saver: s, Sink: sink,
	})
	r := mustReserve(t, ex)
	mustPrepareValid(t, r, work)
	ch := mustResult(t, r)
	// Hostile post-prepare mutation of every caller-aliased
	// surface (mirrors the TrySubmit freeze test).
	work.Capture.Durable.Advancement[0] ^= 0xFF
	work.Capture.Durable.Spells[0].Ability++
	work.Capture.Durable.Skills[0].Ability++
	work.Capture.Durable.Items[0].Enchants[0] ^= 0xFF
	work.Capture.AffectedItems[0].Qty++
	work.Capture.Vitals.HP++
	work.RuntimeInputs.EffectiveStamina = 70
	r.ActivateImmediateDeathWork()
	<-entered
	close(release)
	res := awaitResult(t, ch)
	if res.Err != nil {
		t.Fatalf("result err = %v", res.Err)
	}
	got := fs.gotDeath[0]
	if string(got.Character.Advancement) != string(intended.Character.Advancement) {
		t.Fatal("Store advancement mutated by caller post-prepare")
	}
	if got.Character.Spells[0] != intended.Character.Spells[0] {
		t.Fatal("Store spells mutated by caller post-prepare")
	}
	if got.Items[0].Snapshot.Qty != intended.Items[0].Snapshot.Qty {
		t.Fatal("Store item qty mutated by caller post-prepare")
	}
	delivered := sink.calls[0]
	if string(delivered.Durable.Advancement) != string(intended.Character.Advancement) {
		t.Fatal("completion advancement mutated by caller post-prepare")
	}
	if delivered.RuntimeInputs != testDeathRuntimeInputs(t) {
		t.Fatalf("completion runtime inputs = %+v, want frozen original", delivered.RuntimeInputs)
	}
	if delivered.Vitals.HP != work.Capture.Vitals.HP-1 {
		t.Fatal("completion vitals mutated by caller post-prepare")
	}
}

// Worker dequeue frees queue capacity: with capacity 1,
// once the worker has dequeued the activated reservation
// and is blocked inside Store, the job is RUNNING (no
// longer occupying queue capacity) so a new reservation
// succeeds. This preserves existing QueueCapacity semantics.
func TestDeathExecutorReservationDequeueFreesCapacity(t *testing.T) {
	s := mustSaverForPersist(t)
	trackExecutorRoots(t, s, 0, 0)
	bs := &blockingDeathStore{
		fakeRecoveryStore: newFakeRecoveryStore(),
		entered:           make(chan struct{}),
		release:           make(chan struct{}),
	}
	sink := &fakeCompletionSink{}
	ex, _ := startExecutor(t, DeathExecutorConfig{
		Workers: 1, QueueCapacity: 1, Store: bs, Saver: s, Sink: sink,
	})
	rA := mustReserve(t, ex)
	mustPrepareValid(t, rA, executorWork(t))
	chA := mustResult(t, rA)
	rA.ActivateImmediateDeathWork()
	<-bs.entered
	// A is dequeued and running: its permit is free, so a
	// new reservation must succeed despite capacity 1.
	rB := mustReserve(t, ex)
	if got := permitsFree(t, ex); got != 0 {
		t.Fatalf("permits = %d, want 0 while B held", got)
	}
	close(bs.release)
	if res := awaitResult(t, chA); res.Err != nil {
		t.Fatalf("A err = %v", res.Err)
	}
	if bs.calls != 1 {
		t.Fatalf("store calls = %d, want 1", bs.calls)
	}
	rB.CancelImmediateDeathWork()
	if got := permitsFree(t, ex); got != 1 {
		t.Fatalf("permits = %d, want 1 at end", got)
	}
}

// Shutdown drains the queued permit: A running (blocked in
// Store) plus B activated/queued, then executor cancel — A
// keeps the existing running cancellation behavior, B gets
// ErrDeathExecutorShutdown, and teardown releases every
// permit exactly once with no panic or leak.
func TestDeathExecutorReservationShutdownDrainsQueuedPermit(t *testing.T) {
	s := mustSaverForPersist(t)
	trackExecutorRoots(t, s, 0, 0)
	bs := &blockingDeathStore{
		fakeRecoveryStore: newFakeRecoveryStore(),
		entered:           make(chan struct{}),
		release:           make(chan struct{}),
	}
	sink := &fakeCompletionSink{}
	ctx, cancel := context.WithCancel(context.Background())
	ex, err := NewDeathExecutor(DeathExecutorConfig{
		Workers: 1, QueueCapacity: 2, Store: bs, Saver: s, Sink: sink,
	})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	runErr := make(chan error, 1)
	go func() { runErr <- ex.Run(ctx) }()
	<-ex.ready
	resA := mustSubmit(t, ex, executorWork(t))
	<-bs.entered
	rB := mustReserve(t, ex)
	mustPrepareValid(t, rB, executorWork(t))
	chB := mustResult(t, rB)
	rB.ActivateImmediateDeathWork()
	cancel()
	if res := awaitResult(t, resA); !errors.Is(res.Err, context.Canceled) {
		t.Fatalf("running job err = %v, want context.Canceled", res.Err)
	}
	if res := awaitResult(t, chB); !errors.Is(res.Err, ErrDeathExecutorShutdown) {
		t.Fatalf("queued job err = %v, want ErrDeathExecutorShutdown", res.Err)
	}
	if err := <-runErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("run err = %v, want context.Canceled", err)
	}
	if bs.calls != 1 {
		t.Fatalf("store calls = %d, want exactly 1 (only running A entered Store)", bs.calls)
	}
	if sink.numCalls() != 0 {
		t.Fatalf("sink calls = %d, want 0 (no completion after shutdown)", sink.numCalls())
	}
	if got := permitsFree(t, ex); got != 2 {
		t.Fatalf("permits = %d, want 2 (dequeue + drain, exactly once each)", got)
	}
	if _, err := ex.ReserveImmediateDeath(); !errors.Is(err, ErrDeathExecutorNotRunning) {
		t.Fatalf("post-shutdown reserve err = %v, want ErrDeathExecutorNotRunning", err)
	}
}

// Shutdown before activation: reserve + prepare while
// running, stop the executor completely BEFORE Activate,
// then Activate. It returns promptly with NO Store call,
// NO recovery, NO sink, and the Result receives
// ErrDeathExecutorShutdown exactly once. Repeated Activate
// stays a no-op; Cancel afterwards stays a no-op.
func TestDeathExecutorReservationShutdownBeforeActivate(t *testing.T) {
	s := mustSaverForPersist(t)
	trackExecutorRoots(t, s, 0, 0)
	fs := newFakeRecoveryStore()
	sink := &fakeCompletionSink{}
	ctx, cancel := context.WithCancel(context.Background())
	ex, err := NewDeathExecutor(DeathExecutorConfig{
		Workers: 1, QueueCapacity: 2, Store: fs, Saver: s, Sink: sink,
	})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	runErr := make(chan error, 1)
	go func() { runErr <- ex.Run(ctx) }()
	<-ex.ready
	r := mustReserve(t, ex)
	mustPrepareValid(t, r, executorWork(t))
	ch := mustResult(t, r)
	cancel()
	if err := <-runErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("run err = %v, want context.Canceled", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.ActivateImmediateDeathWork()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Activate blocked after shutdown, want prompt return")
	}
	r.ActivateImmediateDeathWork()
	res := awaitResult(t, ch)
	if !errors.Is(res.Err, ErrDeathExecutorShutdown) {
		t.Fatalf("result err = %v, want ErrDeathExecutorShutdown", res.Err)
	}
	if _, n := drainOne(ch); n != 0 {
		t.Fatalf("extra results = %d, want 0 (exactly once)", n)
	}
	r.CancelImmediateDeathWork()
	if _, n := drainOne(ch); n != 0 {
		t.Fatalf("post-activate cancel produced %d results, want 0", n)
	}
	if fs.deathCalls != 0 || fs.charCalls != 0 || len(fs.itemCalls) != 0 {
		t.Fatalf("store=%d char=%d items=%v, want zero", fs.deathCalls, fs.charCalls, fs.itemCalls)
	}
	if sink.numCalls() != 0 {
		t.Fatalf("sink calls = %d, want 0", sink.numCalls())
	}
	if got := permitsFree(t, ex); got != 2 {
		t.Fatalf("permits = %d, want 2 (shutdown-activation returns its permit once)", got)
	}
}

// Result accessor: reserved-but-not-prepared reports a
// stable state error; prepared/activated/cancelled return
// the exact same buffered channel every time with no
// replacement channels.
func TestDeathExecutorReservationResultAccessor(t *testing.T) {
	s := mustSaverForPersist(t)
	trackExecutorRoots(t, s, 0, 0)
	fs := newFakeRecoveryStore()
	sink := &fakeCompletionSink{}
	ex, _ := startExecutor(t, DeathExecutorConfig{
		Workers: 1, QueueCapacity: 4, Store: fs, Saver: s, Sink: sink,
	})
	work := executorWork(t)
	// Reserved but not prepared: no result available.
	r := mustReserve(t, ex)
	if _, err := r.Result(); !errors.Is(err, ErrDeathReservationNotPrepared) {
		t.Fatalf("reserved Result err = %v, want ErrDeathReservationNotPrepared", err)
	}
	// Reserved cancel (never prepared): still no result.
	r.CancelImmediateDeathWork()
	if _, err := r.Result(); !errors.Is(err, ErrDeathReservationNotPrepared) {
		t.Fatalf("cancelled-unprepared Result err = %v, want ErrDeathReservationNotPrepared", err)
	}
	// Prepared: buffered channel exists and is stable.
	p := mustReserve(t, ex)
	mustPrepareValid(t, p, work)
	ch1 := mustResult(t, p)
	ch2 := mustResult(t, p)
	if ch1 != ch2 {
		t.Fatal("repeated Result returned different channels")
	}
	// Activated: same channel, successful execution.
	p.ActivateImmediateDeathWork()
	if ch3 := mustResult(t, p); ch3 != ch1 {
		t.Fatal("post-activate Result returned a different channel")
	}
	if res := awaitResult(t, ch1); res.Err != nil {
		t.Fatalf("activated result err = %v", res.Err)
	}
	// Prepared + cancelled: same channel receives the
	// cancellation sentinel.
	c := mustReserve(t, ex)
	mustPrepareValid(t, c, work)
	chC := mustResult(t, c)
	c.CancelImmediateDeathWork()
	if chAfter := mustResult(t, c); chAfter != chC {
		t.Fatal("post-cancel Result returned a different channel")
	}
	if res := awaitResult(t, chC); !errors.Is(res.Err, ErrDeathReservationCanceled) {
		t.Fatalf("cancelled result err = %v, want ErrDeathReservationCanceled", res.Err)
	}
}

// TrySubmit regression pins: invalid work before Run stays
// ErrDeathExecutorInvalid (validation ordering preserved,
// never NotRunning); valid work before Run stays
// ErrDeathExecutorNotRunning; a reservation-held full queue
// reports QueueFull; successful work keeps normal behavior.
func TestDeathExecutorReservationTrySubmitRegression(t *testing.T) {
	s := mustSaverForPersist(t)
	fs := newFakeRecoveryStore()
	sink := &fakeCompletionSink{}
	ex, err := NewDeathExecutor(DeathExecutorConfig{
		Workers: 1, QueueCapacity: 1, Store: fs, Saver: s, Sink: sink,
	})
	if err != nil {
		t.Fatal(err)
	}
	badInputs := executorWork(t)
	badInputs.RuntimeInputs = sim.PlayerVitalsRuntimeInputs{}
	if _, err := ex.TrySubmit(badInputs); !errors.Is(err, ErrDeathExecutorInvalid) {
		t.Fatalf("invalid pre-Run err = %v, want ErrDeathExecutorInvalid", err)
	}
	if _, err := ex.TrySubmit(executorWork(t)); !errors.Is(err, ErrDeathExecutorNotRunning) {
		t.Fatalf("valid pre-Run err = %v, want ErrDeathExecutorNotRunning", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- ex.Run(ctx) }()
	<-ex.ready
	t.Cleanup(func() {
		cancel()
		<-runErr
	})
	trackExecutorRoots(t, s, 0, 0)
	held := mustReserve(t, ex)
	if _, err := ex.TrySubmit(executorWork(t)); !errors.Is(err, ErrDeathExecutorQueueFull) {
		t.Fatalf("full-queue err = %v, want ErrDeathExecutorQueueFull", err)
	}
	held.CancelImmediateDeathWork()
	res := awaitResult(t, mustSubmit(t, ex, executorWork(t)))
	if res.Err != nil {
		t.Fatalf("successful work err = %v", res.Err)
	}
	if res.Recovered || res.Delivery != sim.DeathCompletionApplied {
		t.Fatalf("successful work = %+v, want normal Applied ack", res)
	}
	if fs.deathCalls != 1 || sink.numCalls() != 1 {
		t.Fatalf("store=%d sink=%d, want 1/1", fs.deathCalls, sink.numCalls())
	}
}

// Race stress over Activate vs Activate, Cancel vs Cancel,
// Activate vs Cancel, and concurrent Result reads on
// prepared reservations: exactly one terminal path owns the
// permit/job/result — no double send, no double release, no
// panic. Run under -race.
func TestDeathExecutorReservationRaceStress(t *testing.T) {
	const rounds = 50
	s := mustSaverForPersist(t)
	trackExecutorRoots(t, s, 0, 0)
	fs := newFakeRecoveryStore()
	sink := &fakeCompletionSink{}
	ex, _ := startExecutor(t, DeathExecutorConfig{
		Workers: 4, QueueCapacity: 64, Store: fs, Saver: s, Sink: sink,
	})
	for i := 0; i < rounds; i++ {
		r := mustReserve(t, ex)
		mustPrepareValid(t, r, executorWork(t))
		ch := mustResult(t, r)
		var wg sync.WaitGroup
		for k := 0; k < 4; k++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r.ActivateImmediateDeathWork()
			}()
			wg.Add(1)
			go func() {
				defer wg.Done()
				r.CancelImmediateDeathWork()
			}()
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = r.Result()
			}()
		}
		wg.Wait()
		// Exactly one terminal path owns the result: a
		// Cancel win already buffered the sentinel, an
		// Activate win delivers through the worker, so
		// await the one definitive result, then require
		// no second message.
		res := awaitResult(t, ch)
		if res.Err != nil && !errors.Is(res.Err, ErrDeathReservationCanceled) {
			t.Fatalf("round %d: result err = %v, want nil or ErrDeathReservationCanceled", i, res.Err)
		}
		if _, n := drainOne(ch); n != 0 {
			t.Fatalf("round %d: extra results = %d, want 0", i, n)
		}
	}
	if got := permitsFree(t, ex); got != 64 {
		t.Fatalf("permits = %d, want 64 (every permit returned exactly once)", got)
	}
	if fs.deathCalls != sink.numCalls() {
		t.Fatalf("store=%d sink=%d, want equal (one completion per execution)", fs.deathCalls, sink.numCalls())
	}
}
