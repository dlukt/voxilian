package sim

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Reserved Saver critical slot tests (spec §9.5.1k, M5-T5c3d2b1,
// frozen v0.3.54): generic Store-independent Reserve/Execute/
// Cancel behavior only. No Store/persist/PG/gateway/proto
// involvement. Synchronization uses channels/barriers;
// time.After appears only as a failure watchdog, never as the
// correctness mechanism.

// ---- white-box helpers (same package) ----

func mustReserve(t *testing.T, s *Saver, keys []AggregateKey) *CriticalSetReservation {
	t.Helper()
	r, err := s.ReserveCriticalSet(keys)
	if err != nil {
		t.Fatalf("ReserveCriticalSet(%v): %v", keys, err)
	}
	if r == nil {
		t.Fatalf("ReserveCriticalSet(%v) returned nil reservation", keys)
	}
	return r
}

func saverSeqFor(t *testing.T, s *Saver, k AggregateKey) uint64 {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[k]
	if !ok {
		t.Fatalf("saverSeqFor: %v not tracked", k)
	}
	return e.seq
}

func saverReservedFor(t *testing.T, s *Saver, k AggregateKey) bool {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[k]
	if !ok {
		t.Fatalf("saverReservedFor: %v not tracked", k)
	}
	return e.reserved
}

func saverPendingGenFor(t *testing.T, s *Saver, k AggregateKey) (uint64, bool) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[k]
	if !ok {
		t.Fatalf("saverPendingGenFor: %v not tracked", k)
	}
	if e.pending == nil {
		return 0, false
	}
	return e.pending.gen, true
}

// ---- validation: zero mutation, zero reservation ----

func TestSaverCriticalReservationValidation(t *testing.T) {
	s := mustSaver(t, newManualClock())
	keyA := charSaverKey(1)
	mustTrack(t, s, keyA, 3)
	cases := map[string]struct {
		keys []AggregateKey
		want error
	}{
		"empty nil slice":     {nil, ErrInvalidCriticalSet},
		"empty non-nil slice": {[]AggregateKey{}, ErrInvalidCriticalSet},
		"duplicate key":       {[]AggregateKey{keyA, keyA}, ErrInvalidCriticalSet},
		"invalid key":         {[]AggregateKey{keyA, {Kind: AggregateCharacter, ID: 0}}, ErrInvalidAggregateKey},
		"unknown participant": {[]AggregateKey{itemSaverKey(99)}, ErrAggregateNotTracked},
	}
	for name, tc := range cases {
		r, err := s.ReserveCriticalSet(tc.keys)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
		if r != nil {
			t.Errorf("%s: reservation = %v, want nil", name, r)
			r.Cancel()
		}
	}
	snap, err := s.Inspect(keyA)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Blocked || snap.Dirty || snap.InFlight {
		t.Errorf("post-validation %v = %+v, want clean/unblocked/idle", keyA, snap)
	}
	if snap.KnownRevision != 3 {
		t.Errorf("known = %d, want 3", snap.KnownRevision)
	}
	if saverSeqFor(t, s, keyA) != 0 {
		t.Errorf("seq changed by failed validation")
	}
}

func TestSaverCriticalReservationExecuteNilWrite(t *testing.T) {
	s := mustSaver(t, newManualClock())
	key := charSaverKey(2)
	mustTrack(t, s, key, 0)
	r := mustReserve(t, s, []AggregateKey{key})
	var nilWrite CriticalSetWrite
	out, err := r.Execute(context.Background(), nilWrite)
	if !errors.Is(err, ErrInvalidCriticalSet) {
		t.Fatalf("Execute(nil) err = %v, want %v", err, ErrInvalidCriticalSet)
	}
	if out != nil {
		t.Fatalf("Execute(nil) out = %v, want nil", out)
	}
	// Zero mutation: the reservation is still Reserved and usable.
	if !saverReservedFor(t, s, key) {
		t.Fatalf("nil Execute consumed the reservation")
	}
	r.Cancel()
	if err := s.Untrack(key); err != nil {
		t.Fatalf("Untrack after Cancel: %v", err)
	}
}

// ---- 1. busy gate: non-blocking failure ----

func TestSaverCriticalReservationBusyGate(t *testing.T) {
	s := mustSaver(t, newManualClock())
	key := charSaverKey(11)
	mustTrack(t, s, key, 0)

	// Park a WriteThrough inside its callback so it owns the gate.
	entered := make(chan struct{})
	release := make(chan struct{})
	wtDone := make(chan error, 1)
	go func() {
		_, err := s.WriteThrough(context.Background(), key, func(ctx context.Context, exp int64) (int64, error) {
			close(entered)
			<-release
			return exp + 1, nil
		})
		wtDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("WriteThrough callback never entered")
	}

	seqBefore := saverSeqFor(t, s, key)
	type reserveResult struct {
		r   *CriticalSetReservation
		err error
	}
	resCh := make(chan reserveResult, 1)
	go func() {
		r, err := s.ReserveCriticalSet([]AggregateKey{key})
		resCh <- reserveResult{r: r, err: err}
	}()
	select {
	case res := <-resCh:
		if !errors.Is(res.err, ErrCriticalSetReservationBusy) {
			t.Fatalf("Reserve err = %v, want %v", res.err, ErrCriticalSetReservationBusy)
		}
		if res.r != nil {
			t.Fatalf("busy Reserve returned non-nil reservation")
			res.r.Cancel()
		}
	case <-time.After(10 * time.Second):
		t.Fatal("busy Reserve did not return immediately")
	}
	if got := saverSeqFor(t, s, key); got != seqBefore {
		t.Fatalf("busy Reserve allocated generation: seq %d -> %d", seqBefore, got)
	}
	if saverReservedFor(t, s, key) {
		t.Fatalf("busy Reserve left reserved metadata")
	}

	// The held writer is unaffected and completes normally.
	close(release)
	select {
	case err := <-wtDone:
		if err != nil {
			t.Fatalf("held WriteThrough: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("held WriteThrough never completed")
	}
	if snap, _ := s.Inspect(key); snap.KnownRevision != 1 {
		t.Fatalf("known = %d, want 1", snap.KnownRevision)
	}

	// After the writer completes a new reservation succeeds.
	mustReserve(t, s, []AggregateKey{key}).Cancel()
}

// ---- 2. reserve generation before newer MarkDirty (primary) ----

func TestSaverCriticalReservationNewerMarkDirtySurvives(t *testing.T) {
	s := mustSaver(t, newManualClock())
	key := charSaverKey(21)
	mustTrack(t, s, key, 0)

	r := mustReserve(t, s, []AggregateKey{key})
	reservedGen := r.critGen[0]

	var newerRan atomic.Bool
	var newerSawExpected atomic.Int64
	newerSawExpected.Store(-1)
	if err := s.MarkDirty(key, func(ctx context.Context, exp int64) (int64, error) {
		newerRan.Store(true)
		newerSawExpected.Store(exp)
		return exp + 1, nil
	}); err != nil {
		t.Fatalf("MarkDirty after Reserve: %v", err)
	}
	if gen, ok := saverPendingGenFor(t, s, key); !ok || gen <= reservedGen {
		t.Fatalf("newer pending gen = %d (ok=%v), want > reserved %d", gen, ok, reservedGen)
	}

	var gotExpected int64 = -1
	out, err := r.Execute(context.Background(), func(ctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
		gotExpected = exp[0].Revision
		return plusOneResults(exp), nil
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotExpected != 0 {
		t.Fatalf("callback expected = %d, want execution-time known 0", gotExpected)
	}
	if len(out) != 1 || out[0].Revision != 1 {
		t.Fatalf("out = %v, want [{key 1}]", out)
	}
	snap, err := s.Inspect(key)
	if err != nil {
		t.Fatal(err)
	}
	if snap.KnownRevision != 1 {
		t.Errorf("known = %d, want 1", snap.KnownRevision)
	}
	if !snap.Dirty {
		t.Errorf("Dirty = false after reserved success, want newer snapshot still queued")
	}
	if snap.Blocked || snap.InFlight {
		t.Errorf("snap = %+v, want unblocked/idle", snap)
	}
	if newerRan.Load() {
		t.Errorf("newer MarkDirty snapshot ran during reserved Execute")
	}

	// The surviving snapshot flushes with expected revision 1.
	if err := s.FlushDirty(context.Background()); err != nil {
		t.Fatalf("FlushDirty: %v", err)
	}
	if !newerRan.Load() {
		t.Fatalf("newer snapshot never flushed")
	}
	if got := newerSawExpected.Load(); got != 1 {
		t.Fatalf("newer snapshot expected = %d, want 1", got)
	}
	if snap, _ := s.Inspect(key); snap.KnownRevision != 2 {
		t.Fatalf("known = %d, want 2", snap.KnownRevision)
	}
}

// ---- 3. older pending snapshot is superseded ----

func TestSaverCriticalReservationOlderPendingSuperseded(t *testing.T) {
	s := mustSaver(t, newManualClock())
	key := charSaverKey(22)
	mustTrack(t, s, key, 4)
	var oldRan atomic.Bool
	if err := s.MarkDirty(key, func(ctx context.Context, exp int64) (int64, error) {
		oldRan.Store(true)
		return exp + 1, nil
	}); err != nil {
		t.Fatalf("MarkDirty: %v", err)
	}
	r := mustReserve(t, s, []AggregateKey{key})
	out, err := r.Execute(context.Background(), func(ctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
		return plusOneResults(exp), nil
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(out) != 1 || out[0].Revision != 5 {
		t.Fatalf("out = %v, want revision 5", out)
	}
	if oldRan.Load() {
		t.Fatalf("older SnapshotWrite ran; want superseded")
	}
	if snap, _ := s.Inspect(key); snap.Dirty {
		t.Fatalf("Dirty = true after reserved success, want older pending superseded")
	}
}

// ---- 4. WriteThrough cannot overtake reservation ----

func TestSaverCriticalReservationWriteThroughCannotOvertake(t *testing.T) {
	s := mustSaver(t, newManualClock())
	key := charSaverKey(31)
	mustTrack(t, s, key, 7)
	r := mustReserve(t, s, []AggregateKey{key})

	var wtRan atomic.Bool
	var wtSawExpected atomic.Int64
	wtSawExpected.Store(-1)
	wtDone := make(chan error, 1)
	go func() {
		_, err := s.WriteThrough(context.Background(), key, func(ctx context.Context, exp int64) (int64, error) {
			wtRan.Store(true)
			wtSawExpected.Store(exp)
			return exp + 1, nil
		})
		wtDone <- err
	}()
	// The WriteThrough callback cannot run while the reservation
	// holds the gate; the yield loop only lets the goroutine reach
	// the gate wait.
	for i := 0; i < 1000; i++ {
		if wtRan.Load() {
			t.Fatal("WriteThrough callback overtook the live reservation")
		}
		runtime.Gosched()
	}
	// The gate is still owned by the reservation, not the waiter.
	if _, err := s.ReserveCriticalSet([]AggregateKey{key}); !errors.Is(err, ErrCriticalSetReservationBusy) {
		t.Fatalf("re-Reserve err = %v, want %v (gate must be held)", err, ErrCriticalSetReservationBusy)
	}

	var resSawExpected int64 = -1
	out, err := r.Execute(context.Background(), func(ctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
		resSawExpected = exp[0].Revision
		return plusOneResults(exp), nil
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resSawExpected != 7 || len(out) != 1 || out[0].Revision != 8 {
		t.Fatalf("reserved callback expected = %d out = %v, want 7 / rev 8", resSawExpected, out)
	}

	select {
	case err := <-wtDone:
		if err != nil {
			t.Fatalf("WriteThrough: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("WriteThrough never proceeded after reservation release")
	}
	if got := wtSawExpected.Load(); got != 8 {
		t.Fatalf("WriteThrough expected = %d, want 8 (must see reserved E+1)", got)
	}
	if snap, _ := s.Inspect(key); snap.KnownRevision != 9 {
		t.Fatalf("known = %d, want 9", snap.KnownRevision)
	}
}

// ---- 5. ordinary WriteCriticalSet cannot overtake ----

func TestSaverCriticalReservationWriteCriticalSetCannotOvertake(t *testing.T) {
	s := mustSaver(t, newManualClock())
	key := charSaverKey(32)
	mustTrack(t, s, key, 7)
	r := mustReserve(t, s, []AggregateKey{key})

	var ordinaryRan atomic.Bool
	var ordinarySawExpected atomic.Int64
	ordinarySawExpected.Store(-1)
	ordDone := make(chan error, 1)
	go func() {
		_, err := s.WriteCriticalSet(context.Background(), []AggregateKey{key}, func(ctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
			ordinaryRan.Store(true)
			ordinarySawExpected.Store(exp[0].Revision)
			return plusOneResults(exp), nil
		})
		ordDone <- err
	}()
	for i := 0; i < 1000; i++ {
		if ordinaryRan.Load() {
			t.Fatal("ordinary WriteCriticalSet overtook the live reservation")
		}
		runtime.Gosched()
	}

	var resSawExpected int64 = -1
	if _, err := r.Execute(context.Background(), func(ctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
		resSawExpected = exp[0].Revision
		return plusOneResults(exp), nil
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resSawExpected != 7 {
		t.Fatalf("reserved callback expected = %d, want 7", resSawExpected)
	}
	select {
	case err := <-ordDone:
		if err != nil {
			t.Fatalf("ordinary WriteCriticalSet: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ordinary WriteCriticalSet never proceeded after release")
	}
	if got := ordinarySawExpected.Load(); got != 8 {
		t.Fatalf("ordinary callback expected = %d, want 8", got)
	}
	if snap, _ := s.Inspect(key); snap.KnownRevision != 9 {
		t.Fatalf("known = %d, want 9", snap.KnownRevision)
	}
}

// ---- 6. MarkDirty remains non-blocking ----

func TestSaverCriticalReservationMarkDirtyNonBlocking(t *testing.T) {
	s := mustSaver(t, newManualClock())
	key := charSaverKey(33)
	mustTrack(t, s, key, 0)
	r := mustReserve(t, s, []AggregateKey{key})

	var runCount atomic.Int32
	var lastIdx atomic.Int32
	lastIdx.Store(-1)
	for i := 0; i < 50; i++ {
		i := i
		if err := s.MarkDirty(key, func(ctx context.Context, exp int64) (int64, error) {
			runCount.Add(1)
			lastIdx.Store(int32(i))
			return exp + 1, nil
		}); err != nil {
			t.Fatalf("MarkDirty %d while reserved: %v", i, err)
		}
	}
	if snap, _ := s.Inspect(key); !snap.Dirty {
		t.Fatalf("Dirty = false with 50 queued snapshots")
	}
	if _, err := r.Execute(context.Background(), func(ctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
		return plusOneResults(exp), nil
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Only the latest snapshot may run on flush; no callback ran
	// until the later flush.
	if err := s.FlushDirty(context.Background()); err != nil {
		t.Fatalf("FlushDirty: %v", err)
	}
	if got := runCount.Load(); got != 1 {
		t.Fatalf("flushed run count = %d, want exactly the latest 1", got)
	}
	if got := lastIdx.Load(); got != 49 {
		t.Fatalf("flushed snapshot idx = %d, want latest 49", got)
	}
}

// ---- 7. Cancel ----

func TestSaverCriticalReservationCancel(t *testing.T) {
	s := mustSaver(t, newManualClock())
	key := charSaverKey(34)
	mustTrack(t, s, key, 0)
	r := mustReserve(t, s, []AggregateKey{key})
	firstGen := r.critGen[0]
	var newerRan atomic.Bool
	if err := s.MarkDirty(key, func(ctx context.Context, exp int64) (int64, error) {
		newerRan.Store(true)
		return exp + 1, nil
	}); err != nil {
		t.Fatalf("MarkDirty: %v", err)
	}
	seqBeforeCancel := saverSeqFor(t, s, key)

	r.Cancel()
	snap, err := s.Inspect(key)
	if err != nil {
		t.Fatal(err)
	}
	if snap.KnownRevision != 0 {
		t.Errorf("known = %d after Cancel, want unchanged 0", snap.KnownRevision)
	}
	if !snap.Dirty {
		t.Errorf("Dirty = false after Cancel, want pending preserved")
	}
	if snap.Blocked || snap.InFlight {
		t.Errorf("snap = %+v after Cancel, want unblocked/idle", snap)
	}
	if saverReservedFor(t, s, key) {
		t.Errorf("reserved metadata not cleared by Cancel")
	}
	if got := saverSeqFor(t, s, key); got != seqBeforeCancel {
		t.Errorf("seq %d -> %d across Cancel, want no rollback/reuse", seqBeforeCancel, got)
	}

	// The gate is released: a later WriteThrough proceeds.
	if _, err := s.WriteThrough(context.Background(), key, casOK); err != nil {
		t.Fatalf("WriteThrough after Cancel: %v", err)
	}
	if snap, _ := s.Inspect(key); snap.KnownRevision != 1 {
		t.Fatalf("known = %d, want 1", snap.KnownRevision)
	}

	// A later reservation allocates a strictly newer generation:
	// the cancelled generation gap is never reused.
	r2 := mustReserve(t, s, []AggregateKey{key})
	if r2.critGen[0] <= firstGen {
		t.Fatalf("second reserved gen %d <= cancelled gen %d (reused)", r2.critGen[0], firstGen)
	}
	// Repeated Cancel is a no-op and cannot double-release.
	r.Cancel()
	r2.Cancel()
	r2.Cancel()
	if _, err := r2.Execute(context.Background(), func(ctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
		return plusOneResults(exp), nil
	}); !errors.Is(err, ErrCriticalSetReservationConsumed) {
		t.Fatalf("Execute after Cancel err = %v, want %v", err, ErrCriticalSetReservationConsumed)
	}
	if newerRan.Load() {
		t.Fatalf("cancelled-then-superseded snapshot ran unexpectedly")
	}
}

// ---- 8. Untrack protection ----

func TestSaverCriticalReservationUntrackProtection(t *testing.T) {
	s := mustSaver(t, newManualClock())
	key := charSaverKey(35)
	mustTrack(t, s, key, 0)
	r := mustReserve(t, s, []AggregateKey{key})
	if err := s.Untrack(key); !errors.Is(err, ErrSaverUntrackDirty) {
		t.Fatalf("Untrack while reserved err = %v, want %v", err, ErrSaverUntrackDirty)
	}
	r.Cancel()
	if err := s.Untrack(key); err != nil {
		t.Fatalf("Untrack after Cancel when clean: %v", err)
	}
	if got := s.TrackedCount(); got != 0 {
		t.Fatalf("TrackedCount = %d, want 0", got)
	}
}

// ---- 9. context cancelled before Execute callback ----

func TestSaverCriticalReservationContextCancelled(t *testing.T) {
	s := mustSaver(t, newManualClock())
	key := charSaverKey(36)
	mustTrack(t, s, key, 2)
	r := mustReserve(t, s, []AggregateKey{key})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var invoked atomic.Bool
	out, err := r.Execute(ctx, func(ctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
		invoked.Store(true)
		return plusOneResults(exp), nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute err = %v, want %v", err, context.Canceled)
	}
	if out != nil {
		t.Fatalf("out = %v, want nil", out)
	}
	if invoked.Load() {
		t.Fatalf("callback invoked with cancelled context")
	}
	snap, err := s.Inspect(key)
	if err != nil {
		t.Fatal(err)
	}
	if snap.KnownRevision != 2 || snap.Dirty || snap.Blocked || snap.InFlight {
		t.Fatalf("snap = %+v after pre-callback abort, want known 2 clean/unblocked/idle", snap)
	}
	// The gate is released and the reservation is terminal.
	if _, err := s.WriteThrough(context.Background(), key, casOK); err != nil {
		t.Fatalf("WriteThrough after abort: %v", err)
	}
	if _, err := r.Execute(context.Background(), func(ctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
		return plusOneResults(exp), nil
	}); !errors.Is(err, ErrCriticalSetReservationConsumed) {
		t.Fatalf("second Execute err = %v, want %v", err, ErrCriticalSetReservationConsumed)
	}
	r.Cancel() // no-op, must not panic or double-release
}

// ---- 10. callback error ----

func TestSaverCriticalReservationCallbackError(t *testing.T) {
	s := mustSaver(t, newManualClock())
	key := charSaverKey(37)
	mustTrack(t, s, key, 3)
	r := mustReserve(t, s, []AggregateKey{key})
	var newerRan atomic.Bool
	if err := s.MarkDirty(key, func(ctx context.Context, exp int64) (int64, error) {
		newerRan.Store(true)
		return exp + 1, nil
	}); err != nil {
		t.Fatalf("MarkDirty: %v", err)
	}
	out, err := r.Execute(context.Background(), func(ctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
		return nil, errCriticalBoom
	})
	if !errors.Is(err, errCriticalBoom) {
		t.Fatalf("Execute err = %v, want cause %v", err, errCriticalBoom)
	}
	if !errors.Is(err, ErrSaverReconcileRequired) {
		t.Fatalf("Execute err = %v, want %v discoverable", err, ErrSaverReconcileRequired)
	}
	if out != nil {
		t.Fatalf("out = %v, want nil", out)
	}
	snap, err := s.Inspect(key)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Blocked {
		t.Errorf("Blocked = false after callback error, want reconcile-blocked")
	}
	if snap.KnownRevision != 3 {
		t.Errorf("known = %d, want unchanged 3", snap.KnownRevision)
	}
	if !snap.Dirty {
		t.Errorf("Dirty = false after callback error, want newer pending retained")
	}
	if newerRan.Load() {
		t.Errorf("newer snapshot ran on error path")
	}
	// The gate is released (a new Reserve reaches the blocked
	// prevalidation instead of Busy), and the reservation is
	// terminal. No ResolveReconciled is called in this test.
	if _, err := s.ReserveCriticalSet([]AggregateKey{key}); !errors.Is(err, ErrSaverReconcileRequired) {
		t.Fatalf("re-Reserve err = %v, want %v (gate released, key blocked)", err, ErrSaverReconcileRequired)
	}
	if _, err := r.Execute(context.Background(), func(ctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
		return plusOneResults(exp), nil
	}); !errors.Is(err, ErrCriticalSetReservationConsumed) {
		t.Fatalf("second Execute err = %v, want %v", err, ErrCriticalSetReservationConsumed)
	}
}

// ---- 11. success-result invariant matrix ----

func TestSaverCriticalReservationResultInvariants(t *testing.T) {
	key := charSaverKey(38)
	other := itemSaverKey(999)
	cases := map[string]func(exp []AggregateRevision) []AggregateRevision{
		"missing": func(exp []AggregateRevision) []AggregateRevision { return nil },
		"extra": func(exp []AggregateRevision) []AggregateRevision {
			return append(plusOneResults(exp), AggregateRevision{Key: other, Revision: 1})
		},
		"duplicate": func(exp []AggregateRevision) []AggregateRevision {
			out := plusOneResults(exp)
			return append(out, out[0])
		},
		"unchanged": func(exp []AggregateRevision) []AggregateRevision { return append([]AggregateRevision(nil), exp...) },
		"jump by 2": func(exp []AggregateRevision) []AggregateRevision {
			out := plusOneResults(exp)
			out[0].Revision++
			return out
		},
		"negative": func(exp []AggregateRevision) []AggregateRevision {
			out := plusOneResults(exp)
			out[0].Revision = -1
			return out
		},
	}
	for name, results := range cases {
		s := mustSaver(t, newManualClock())
		mustTrack(t, s, key, 7)
		var oldRan atomic.Bool
		if err := s.MarkDirty(key, func(ctx context.Context, exp int64) (int64, error) {
			oldRan.Store(true)
			return exp + 1, nil
		}); err != nil {
			t.Fatalf("%s: MarkDirty: %v", name, err)
		}
		r := mustReserve(t, s, []AggregateKey{key})
		out, err := r.Execute(context.Background(), func(ctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
			return results(exp), nil
		})
		if !errors.Is(err, ErrSaverRevisionInvariant) {
			t.Errorf("%s: err = %v, want %v", name, err, ErrSaverRevisionInvariant)
		}
		if !errors.Is(err, ErrSaverReconcileRequired) {
			t.Errorf("%s: err = %v, want %v discoverable", name, err, ErrSaverReconcileRequired)
		}
		if out != nil {
			t.Errorf("%s: out = %v, want nil", name, out)
		}
		snap, ierr := s.Inspect(key)
		if ierr != nil {
			t.Fatal(ierr)
		}
		if !snap.Blocked {
			t.Errorf("%s: Blocked = false, want reconcile-blocked", name)
		}
		if snap.KnownRevision != 7 {
			t.Errorf("%s: known = %d, want unchanged 7", name, snap.KnownRevision)
		}
		if !snap.Dirty {
			t.Errorf("%s: Dirty = false, want pending retained", name)
		}
		if oldRan.Load() {
			t.Errorf("%s: superseded snapshot ran on invariant failure", name)
		}
	}
}

// ---- 12. execution-time expected revision ----

func TestSaverCriticalReservationExecutionTimeExpected(t *testing.T) {
	s := mustSaver(t, newManualClock())
	key := charSaverKey(39)
	mustTrack(t, s, key, 5)

	// An older WriteThrough owns the gate: no reservation can
	// succeed yet, so no revision can be frozen early.
	entered := make(chan struct{})
	release := make(chan struct{})
	wtDone := make(chan error, 1)
	go func() {
		_, err := s.WriteThrough(context.Background(), key, func(ctx context.Context, exp int64) (int64, error) {
			close(entered)
			<-release
			return exp + 1, nil
		})
		wtDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("WriteThrough callback never entered")
	}
	if _, err := s.ReserveCriticalSet([]AggregateKey{key}); !errors.Is(err, ErrCriticalSetReservationBusy) {
		t.Fatalf("Reserve during older write err = %v, want %v", err, ErrCriticalSetReservationBusy)
	}
	close(release)
	select {
	case err := <-wtDone:
		if err != nil {
			t.Fatalf("older WriteThrough: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("older WriteThrough never completed")
	}

	// The reservation succeeds only after the older write; its
	// callback observes the CURRENT known revision at Execute.
	// Successful Reserve serializes all revision-changing
	// callbacks on this key (WriteThrough, ordinary
	// WriteCriticalSet, ResolveReconciled all wait on the SAME
	// held gates), so execution-time expected necessarily
	// equals known at acquisition and cannot change until
	// Execute/Cancel. This is a FEATURE of the gate
	// reservation, not permission to guess the revision at the
	// API boundary: the revision is still captured ONLY at
	// Execute time and never exposed by Reserve.
	r := mustReserve(t, s, []AggregateKey{key})
	var gotExpected int64 = -1
	if _, err := r.Execute(context.Background(), func(ctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
		gotExpected = exp[0].Revision
		return plusOneResults(exp), nil
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotExpected != 6 {
		t.Fatalf("callback expected = %d, want execution-time known 6", gotExpected)
	}
	if snap, _ := s.Inspect(key); snap.KnownRevision != 7 {
		t.Fatalf("known = %d, want 7", snap.KnownRevision)
	}
}

// ---- 13. multi-key canonical all-or-nothing ----

func TestSaverCriticalReservationMultiKeyAllOrNothing(t *testing.T) {
	s := mustSaver(t, newManualClock())
	keyA := charSaverKey(41)
	keyB := itemSaverKey(42)
	keyC := bankSaverKey(43, "sys")
	for _, k := range []AggregateKey{keyA, keyB, keyC} {
		mustTrack(t, s, k, 0)
	}

	// Hold the LAST canonical key's gate with a parked
	// WriteThrough while reserving in hostile (non-canonical)
	// order.
	entered := make(chan struct{})
	release := make(chan struct{})
	wtDone := make(chan error, 1)
	go func() {
		_, err := s.WriteThrough(context.Background(), keyC, func(ctx context.Context, exp int64) (int64, error) {
			close(entered)
			<-release
			return exp + 1, nil
		})
		wtDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("WriteThrough callback never entered")
	}

	hostile := []AggregateKey{keyC, keyA, keyB}
	before := append([]AggregateKey(nil), hostile...)
	// Note: the parked WriteThrough on C already allocated its
	// own critical generation up front, so record per-key
	// baselines: the failed Reserve must allocate nothing.
	seqBefore := map[AggregateKey]uint64{
		keyA: saverSeqFor(t, s, keyA),
		keyB: saverSeqFor(t, s, keyB),
		keyC: saverSeqFor(t, s, keyC),
	}
	if _, err := s.ReserveCriticalSet(hostile); !errors.Is(err, ErrCriticalSetReservationBusy) {
		t.Fatalf("hostile Reserve err = %v, want %v", err, ErrCriticalSetReservationBusy)
	}
	if !reflect.DeepEqual(hostile, before) {
		t.Fatalf("caller slice reordered: %v vs %v", hostile, before)
	}
	// Zero generation allocation on EVERY participant.
	for _, k := range []AggregateKey{keyA, keyB, keyC} {
		if got := saverSeqFor(t, s, k); got != seqBefore[k] {
			t.Fatalf("seq(%v) = %d after failed Reserve, want baseline %d", k, got, seqBefore[k])
		}
	}
	if saverReservedFor(t, s, keyA) || saverReservedFor(t, s, keyB) {
		t.Fatalf("partial reservation metadata left on A/B")
	}

	close(release)
	select {
	case err := <-wtDone:
		if err != nil {
			t.Fatalf("WriteThrough: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("WriteThrough never completed")
	}

	// Canonical acquisition now succeeds; the callback observes
	// canonical expected order regardless of input order.
	r := mustReserve(t, s, hostile)
	var gotOrder []AggregateKey
	out, err := r.Execute(context.Background(), func(ctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
		for _, e := range exp {
			gotOrder = append(gotOrder, e.Key)
		}
		return plusOneResults(exp), nil
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	wantOrder := []AggregateKey{keyA, keyB, keyC}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Fatalf("expected order = %v, want canonical %v", gotOrder, wantOrder)
	}
	if len(out) != 3 || out[0].Revision != 1 || out[1].Revision != 1 || out[2].Revision != 2 {
		t.Fatalf("out = %v, want [1 1 2] (C already at 1)", out)
	}
}

// ---- repeated Execute / Execute-after-Cancel ----

func TestSaverCriticalReservationExecuteConsumed(t *testing.T) {
	s := mustSaver(t, newManualClock())
	key := charSaverKey(44)
	mustTrack(t, s, key, 0)
	r := mustReserve(t, s, []AggregateKey{key})
	if _, err := r.Execute(context.Background(), func(ctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
		return plusOneResults(exp), nil
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := r.Execute(context.Background(), func(ctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
		return plusOneResults(exp), nil
	}); !errors.Is(err, ErrCriticalSetReservationConsumed) {
		t.Fatalf("second Execute err = %v, want %v", err, ErrCriticalSetReservationConsumed)
	}
	r.Cancel() // no-op after consumption
	if snap, _ := s.Inspect(key); snap.KnownRevision != 1 || snap.Dirty || snap.Blocked || snap.InFlight {
		t.Fatalf("snap = %+v, want known 1 clean/unblocked/idle", snap)
	}
}

// ---- 14. Cancel/Execute race (run with -race) ----

func TestSaverCriticalReservationCancelExecuteRace(t *testing.T) {
	for i := 0; i < 200; i++ {
		s := mustSaver(t, newManualClock())
		key := charSaverKey(int64(1000 + i))
		mustTrack(t, s, key, 0)
		r := mustReserve(t, s, []AggregateKey{key})

		var cbRan atomic.Int32
		var wg sync.WaitGroup
		execErrCh := make(chan error, 1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.Execute(context.Background(), func(ctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
				cbRan.Add(1)
				return plusOneResults(exp), nil
			})
			execErrCh <- err
		}()
		for j := 0; j < 3; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r.Cancel()
			}()
		}
		wg.Wait()

		switch got := cbRan.Load(); got {
		case 1:
			// Execute won: exactly one success, key advanced.
			select {
			case err := <-execErrCh:
				if err != nil {
					t.Fatalf("iter %d: Execute won but err = %v", i, err)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("iter %d: Execute result never arrived", i)
			}
			snap, ierr := s.Inspect(key)
			if ierr != nil {
				t.Fatal(ierr)
			}
			if snap.KnownRevision != 1 || snap.Dirty || snap.Blocked || snap.InFlight {
				t.Fatalf("iter %d: snap = %+v, want known 1 clean", i, snap)
			}
		case 0:
			// Cancel won: callback never ran, key untouched.
			select {
			case err := <-execErrCh:
				if !errors.Is(err, ErrCriticalSetReservationConsumed) {
					t.Fatalf("iter %d: Cancel won but Execute err = %v", i, err)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("iter %d: Execute result never arrived", i)
			}
			snap, ierr := s.Inspect(key)
			if ierr != nil {
				t.Fatal(ierr)
			}
			if snap.KnownRevision != 0 || snap.Dirty || snap.Blocked || snap.InFlight {
				t.Fatalf("iter %d: snap = %+v, want known 0 clean", i, snap)
			}
		default:
			t.Fatalf("iter %d: callback ran %d times, want at most once", i, got)
		}
		// Exactly one path owned the terminal release: the key is
		// idle and untrackable with no live reservation.
		if saverReservedFor(t, s, key) {
			t.Fatalf("iter %d: reserved metadata left after race", i)
		}
		if err := s.Untrack(key); err != nil {
			t.Fatalf("iter %d: Untrack after race: %v", i, err)
		}
	}
}
