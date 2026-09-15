package sim

import (
	"context"
	"errors"
	"math"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// Multi-root critical Saver coordination tests (spec §8.3.17,
// §9.5.1a, M5-T5c1): generic Store-agnostic WriteCriticalSet
// behavior only. No Store/persist/PG/gateway/proto involvement.
// Synchronization uses channels/barriers; time.After appears only
// as a failure watchdog, never as the correctness mechanism.

var errCriticalBoom = errors.New("critical test boom")

type criticalResult struct {
	out []AggregateRevision
	err error
}

func isCanonicalRevisions(revs []AggregateRevision) bool {
	for i := 1; i < len(revs); i++ {
		if !revs[i-1].Key.Less(revs[i].Key) {
			return false
		}
	}
	return true
}

func plusOneResults(exp []AggregateRevision) []AggregateRevision {
	out := make([]AggregateRevision, len(exp))
	for i, e := range exp {
		out[i] = AggregateRevision{Key: e.Key, Revision: e.Revision + 1}
	}
	return out
}

func reversedRevisions(in []AggregateRevision) []AggregateRevision {
	out := append([]AggregateRevision(nil), in...)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// ---- validation: zero mutation, zero callback ----

func TestSaverCriticalSetValidation(t *testing.T) {
	s := mustSaver(t, newManualClock())
	keyA := charSaverKey(1)
	keyB := itemSaverKey(2)
	mustTrack(t, s, keyA, 3)
	mustTrack(t, s, keyB, 4)
	var invoked atomic.Bool
	cb := func(ctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
		invoked.Store(true)
		return plusOneResults(exp), nil
	}
	var nilWrite CriticalSetWrite
	cases := map[string]struct {
		keys  []AggregateKey
		write CriticalSetWrite
		want  error
	}{
		"empty nil slice":      {nil, cb, ErrInvalidCriticalSet},
		"empty non-nil slice":  {[]AggregateKey{}, cb, ErrInvalidCriticalSet},
		"nil callback":         {[]AggregateKey{keyA}, nilWrite, ErrInvalidCriticalSet},
		"duplicate key":        {[]AggregateKey{keyA, keyB, keyA}, cb, ErrInvalidCriticalSet},
		"invalid key":          {[]AggregateKey{keyA, {Kind: AggregateCharacter, ID: 0}}, cb, ErrInvalidAggregateKey},
		"unknown participant":  {[]AggregateKey{keyA, itemSaverKey(99)}, cb, ErrAggregateNotTracked},
		"single unknown":       {[]AggregateKey{itemSaverKey(99)}, cb, ErrAggregateNotTracked},
		"duplicate unknown ok": {[]AggregateKey{keyB, keyB}, cb, ErrInvalidCriticalSet},
	}
	for name, tc := range cases {
		invoked.Store(false)
		out, err := s.WriteCriticalSet(context.Background(), tc.keys, tc.write)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
		if out != nil {
			t.Errorf("%s: out = %v, want nil", name, out)
		}
		if invoked.Load() {
			t.Errorf("%s: callback invoked on validation failure", name)
		}
	}
	// Zero mutation: both tracked keys keep revisions, stay clean
	// and unblocked.
	for _, k := range []AggregateKey{keyA, keyB} {
		snap, err := s.Inspect(k)
		if err != nil {
			t.Fatal(err)
		}
		if snap.Blocked || snap.Dirty || snap.InFlight {
			t.Errorf("post-validation %v = %+v, want clean/unblocked/idle", k, snap)
		}
	}
	if snap, _ := s.Inspect(keyA); snap.KnownRevision != 3 {
		t.Errorf("keyA known = %d, want 3", snap.KnownRevision)
	}
	if snap, _ := s.Inspect(keyB); snap.KnownRevision != 4 {
		t.Errorf("keyB known = %d, want 4", snap.KnownRevision)
	}
	if got := s.TrackedCount(); got != 2 {
		t.Errorf("TrackedCount = %d, want 2", got)
	}
}

// ---- basic success: canonical expected, scrambled result, canonical out ----

func TestSaverCriticalSetBasicSuccessCanonical(t *testing.T) {
	s := mustSaver(t, newManualClock())
	c7 := charSaverKey(7)
	i10 := itemSaverKey(10)
	i20 := itemSaverKey(20)
	mustTrack(t, s, c7, 5)
	mustTrack(t, s, i10, 6)
	mustTrack(t, s, i20, 0)
	var gotExp []AggregateRevision
	out, err := s.WriteCriticalSet(context.Background(),
		[]AggregateKey{i20, c7, i10},
		func(ctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
			gotExp = append([]AggregateRevision(nil), exp...)
			// Return in scrambled order; the saver must accept
			// any order and normalize its public result.
			return reversedRevisions(plusOneResults(exp)), nil
		})
	if err != nil {
		t.Fatalf("WriteCriticalSet: %v", err)
	}
	wantExp := []AggregateRevision{{c7, 5}, {i10, 6}, {i20, 0}}
	if len(gotExp) != len(wantExp) {
		t.Fatalf("callback expected len = %d, want %d", len(gotExp), len(wantExp))
	}
	for i := range wantExp {
		if gotExp[i] != wantExp[i] {
			t.Fatalf("callback expected[%d] = %v, want %v (full %v)", i, gotExp[i], wantExp[i], gotExp)
		}
	}
	if !isCanonicalRevisions(out) {
		t.Fatalf("result not canonical: %v", out)
	}
	for i := range wantExp {
		want := AggregateRevision{wantExp[i].Key, wantExp[i].Revision + 1}
		if out[i] != want {
			t.Fatalf("out[%d] = %v, want %v (full %v)", i, out[i], want, out)
		}
	}
	for k, want := range map[AggregateKey]int64{c7: 6, i10: 7, i20: 1} {
		snap, err := s.Inspect(k)
		if err != nil {
			t.Fatal(err)
		}
		if snap.KnownRevision != want || snap.Dirty || snap.Blocked || snap.InFlight {
			t.Errorf("%v = %+v, want known %d clean/unblocked/idle", k, snap, want)
		}
	}
}

// ---- execution-time revisions: a single-root write committing
// while the critical set waits is observed by the callback ----

func TestSaverCriticalSetExecutionTimeRevisions(t *testing.T) {
	s := mustSaver(t, newManualClock())
	keyA := charSaverKey(1)
	keyB := itemSaverKey(2)
	mustTrack(t, s, keyA, 0)
	mustTrack(t, s, keyB, 4)
	enteredB := make(chan int64, 1)
	proceedB := make(chan int64, 1)
	wtDone := make(chan error, 1)
	go func() {
		_, err := s.WriteThrough(context.Background(), keyB,
			func(ctx context.Context, exp int64) (int64, error) {
				enteredB <- exp
				return <-proceedB, nil
			})
		wtDone <- err
	}()
	if exp := <-enteredB; exp != 4 {
		t.Fatalf("B writer expected = %d, want 4", exp)
	}
	cbEntered := make(chan []AggregateRevision, 1)
	critDone := make(chan criticalResult, 1)
	go func() {
		out, err := s.WriteCriticalSet(context.Background(),
			[]AggregateKey{keyB, keyA},
			func(ctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
				cbEntered <- append([]AggregateRevision(nil), exp...)
				return plusOneResults(exp), nil
			})
		critDone <- criticalResult{out, err}
	}()
	// The critical callback must not run while the single-root
	// write owns B: yield widely, assert silence.
	for i := 0; i < 2000; i++ {
		select {
		case got := <-cbEntered:
			t.Fatalf("critical callback ran with %v while single-root write owned B", got)
		default:
			runtime.Gosched()
		}
	}
	proceedB <- 5
	if err := <-wtDone; err != nil {
		t.Fatalf("single-root WriteThrough: %v", err)
	}
	var gotExp []AggregateRevision
	select {
	case gotExp = <-cbEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("critical callback never ran after B released")
	}
	// The callback must see B at 5 (post-commit), never the stale 4.
	want := map[AggregateKey]int64{keyA: 0, keyB: 5}
	if len(gotExp) != 2 {
		t.Fatalf("callback expected len = %d, want 2", len(gotExp))
	}
	for _, e := range gotExp {
		if want[e.Key] != e.Revision {
			t.Errorf("callback expected %v = %d, want %d", e.Key, e.Revision, want[e.Key])
		}
	}
	res := <-critDone
	if res.err != nil {
		t.Fatalf("WriteCriticalSet: %v", res.err)
	}
	if snap, _ := s.Inspect(keyA); snap.KnownRevision != 1 {
		t.Errorf("A known = %d, want 1", snap.KnownRevision)
	}
	if snap, _ := s.Inspect(keyB); snap.KnownRevision != 6 {
		t.Errorf("B known = %d, want 6", snap.KnownRevision)
	}
}

// ---- older pre-critical pending is superseded on success ----

func TestSaverCriticalSetSupersedesOlderPending(t *testing.T) {
	s := mustSaver(t, newManualClock())
	keyA := charSaverKey(1)
	keyB := itemSaverKey(2)
	mustTrack(t, s, keyA, 0)
	mustTrack(t, s, keyB, 4)
	var oldRan atomic.Bool
	if err := s.MarkDirty(keyA, func(ctx context.Context, exp int64) (int64, error) {
		oldRan.Store(true)
		return exp + 1, nil
	}); err != nil {
		t.Fatal(err)
	}
	out, err := s.WriteCriticalSet(context.Background(), []AggregateKey{keyA, keyB},
		func(ctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
			return plusOneResults(exp), nil
		})
	if err != nil {
		t.Fatalf("WriteCriticalSet: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("out len = %d, want 2", len(out))
	}
	if oldRan.Load() {
		t.Error("pre-critical pending snapshot ran after critical success")
	}
	snapA, _ := s.Inspect(keyA)
	if snapA.KnownRevision != 1 || snapA.Dirty {
		t.Errorf("A = %+v, want known 1 clean (older pending superseded)", snapA)
	}
	snapB, _ := s.Inspect(keyB)
	if snapB.KnownRevision != 5 || snapB.Dirty {
		t.Errorf("B = %+v, want known 5 clean", snapB)
	}
}

// ---- newer MarkDirty during the callback survives + flushes at the new revision ----

func TestSaverCriticalSetNewerPendingRetained(t *testing.T) {
	s := mustSaver(t, newManualClock())
	keyA := charSaverKey(1)
	keyB := itemSaverKey(2)
	mustTrack(t, s, keyA, 0)
	mustTrack(t, s, keyB, 0)
	entered := make(chan struct{})
	proceed := make(chan struct{})
	var newerExp atomic.Int64
	newerExp.Store(-1)
	critDone := make(chan error, 1)
	go func() {
		_, err := s.WriteCriticalSet(context.Background(), []AggregateKey{keyA, keyB},
			func(ctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
				close(entered)
				<-proceed
				return plusOneResults(exp), nil
			})
		critDone <- err
	}()
	<-entered
	// A mutation arriving while the callback runs receives a later
	// generation and must survive the critical commit.
	if err := s.MarkDirty(keyA, func(ctx context.Context, exp int64) (int64, error) {
		newerExp.Store(exp)
		return exp + 1, nil
	}); err != nil {
		t.Fatalf("MarkDirty during critical callback: %v", err)
	}
	close(proceed)
	if err := <-critDone; err != nil {
		t.Fatalf("WriteCriticalSet: %v", err)
	}
	snapA, _ := s.Inspect(keyA)
	if snapA.KnownRevision != 1 {
		t.Fatalf("A known = %d, want critical 1", snapA.KnownRevision)
	}
	if !snapA.Dirty {
		t.Fatal("newer pending snapshot erased by critical success")
	}
	// A later ordinary flush must execute it with the NEW known revision.
	if err := s.FlushDirty(context.Background()); err != nil {
		t.Fatalf("FlushDirty: %v", err)
	}
	if got := newerExp.Load(); got != 1 {
		t.Errorf("newer snapshot expected = %d, want NEW known 1", got)
	}
	snapA, _ = s.Inspect(keyA)
	if snapA.KnownRevision != 2 || snapA.Dirty {
		t.Errorf("A = %+v, want known 2 clean after flush", snapA)
	}
}

// ---- overlapping critical sets: canonical order prevents deadlock ----

func TestSaverCriticalSetOverlappingSetsNoDeadlock(t *testing.T) {
	s := mustSaver(t, newManualClock())
	c7 := charSaverKey(7)
	i10 := itemSaverKey(10)
	i20 := itemSaverKey(20)
	mustTrack(t, s, c7, 5)
	mustTrack(t, s, i10, 6)
	mustTrack(t, s, i20, 7)
	var cur, maxSame atomic.Int32
	enterCritical := func() (release func()) {
		n := cur.Add(1)
		for {
			m := maxSame.Load()
			if n <= m || maxSame.CompareAndSwap(m, n) {
				break
			}
		}
		return func() { cur.Add(-1) }
	}
	entered1 := make(chan []AggregateRevision, 1)
	proceed1 := make(chan struct{})
	var exp1 []AggregateRevision
	done1 := make(chan criticalResult, 1)
	go func() {
		out, err := s.WriteCriticalSet(context.Background(),
			// Intentionally reverse input order: item 20,
			// character 7, item 10.
			[]AggregateKey{i20, c7, i10},
			func(ctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
				release := enterCritical()
				defer release()
				exp1 = append([]AggregateRevision(nil), exp...)
				entered1 <- exp1
				<-proceed1
				return plusOneResults(exp), nil
			})
		done1 <- criticalResult{out, err}
	}()
	select {
	case <-entered1:
	case <-time.After(10 * time.Second):
		t.Fatal("first critical callback never entered")
	}
	entered2 := make(chan []AggregateRevision, 1)
	var exp2 []AggregateRevision
	done2 := make(chan criticalResult, 1)
	go func() {
		out, err := s.WriteCriticalSet(context.Background(),
			// Reverse relative to set A: item 10, character 7.
			[]AggregateKey{i10, c7},
			func(ctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
				release := enterCritical()
				defer release()
				exp2 = append([]AggregateRevision(nil), exp...)
				entered2 <- exp2
				return plusOneResults(exp), nil
			})
		done2 <- criticalResult{out, err}
	}()
	// The second set must be genuinely contending (not silently
	// idle): while the first callback is blocked, the second
	// callback must not enter.
	for i := 0; i < 2000; i++ {
		select {
		case got := <-entered2:
			t.Fatalf("overlapping callback entered with %v while first owned participants", got)
		default:
			runtime.Gosched()
		}
	}
	close(proceed1)
	res1 := <-done1
	if res1.err != nil {
		t.Fatalf("first critical set: %v", res1.err)
	}
	select {
	case <-entered2:
	case <-time.After(10 * time.Second):
		t.Fatal("second critical callback never entered after first released")
	}
	res2 := <-done2
	if res2.err != nil {
		t.Fatalf("second critical set: %v", res2.err)
	}
	if got := maxSame.Load(); got != 1 {
		t.Fatalf("max overlapping-callback concurrency = %d, want 1", got)
	}
	// The second callback must observe the revisions committed by
	// the first for every shared participant.
	revOf := func(list []AggregateRevision, k AggregateKey) int64 {
		for _, e := range list {
			if e.Key == k {
				return e.Revision
			}
		}
		return -1
	}
	for _, k := range []AggregateKey{c7, i10} {
		if got, want := revOf(exp2, k), revOf(exp1, k)+1; got != want {
			t.Errorf("second expected %v = %d, want first+1 = %d", k, got, want)
		}
	}
	for k, want := range map[AggregateKey]int64{c7: 7, i10: 8, i20: 8} {
		snap, err := s.Inspect(k)
		if err != nil {
			t.Fatal(err)
		}
		if snap.KnownRevision != want || snap.Dirty || snap.Blocked {
			t.Errorf("%v = %+v, want known %d clean/unblocked", k, snap, want)
		}
	}
}

// ---- single-root interop: critical set shares WriteThrough's gates ----

func TestSaverCriticalSetSingleRootInterop(t *testing.T) {
	s := mustSaver(t, newManualClock())
	keyA := charSaverKey(1)
	mustTrack(t, s, keyA, 0)
	enteredW := make(chan int64, 1)
	proceedW := make(chan int64, 1)
	wtDone := make(chan error, 1)
	go func() {
		_, err := s.WriteThrough(context.Background(), keyA,
			func(ctx context.Context, exp int64) (int64, error) {
				enteredW <- exp
				return <-proceedW, nil
			})
		wtDone <- err
	}()
	if exp := <-enteredW; exp != 0 {
		t.Fatalf("WriteThrough expected = %d, want 0", exp)
	}
	cbRan := make(chan int64, 1)
	critDone := make(chan criticalResult, 1)
	go func() {
		out, err := s.WriteCriticalSet(context.Background(), []AggregateKey{keyA},
			func(ctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
				cbRan <- exp[0].Revision
				return plusOneResults(exp), nil
			})
		critDone <- criticalResult{out, err}
	}()
	for i := 0; i < 2000; i++ {
		select {
		case got := <-cbRan:
			t.Fatalf("critical callback ran with %d while WriteThrough owned the gate", got)
		default:
			runtime.Gosched()
		}
	}
	proceedW <- 1
	if err := <-wtDone; err != nil {
		t.Fatalf("WriteThrough: %v", err)
	}
	select {
	case got := <-cbRan:
		if got != 1 {
			t.Fatalf("critical expected = %d, want post-commit 1", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("critical callback never ran after WriteThrough released")
	}
	res := <-critDone
	if res.err != nil {
		t.Fatalf("WriteCriticalSet: %v", res.err)
	}
	if snap, _ := s.Inspect(keyA); snap.KnownRevision != 2 {
		t.Errorf("A known = %d, want 2", snap.KnownRevision)
	}
}

// ---- reconcile-blocked input: no callback, others untouched ----

func TestSaverCriticalSetBlockedInputNoCallback(t *testing.T) {
	s := mustSaver(t, newManualClock())
	keyA := charSaverKey(1)
	keyB := itemSaverKey(2)
	mustTrack(t, s, keyA, 3)
	mustTrack(t, s, keyB, 4)
	if _, err := s.WriteThrough(context.Background(), keyB,
		func(ctx context.Context, exp int64) (int64, error) {
			return 0, ErrSnapshotStale
		}); !errors.Is(err, ErrSnapshotStale) {
		t.Fatalf("stale setup err = %v, want ErrSnapshotStale", err)
	}
	var invoked atomic.Bool
	out, err := s.WriteCriticalSet(context.Background(), []AggregateKey{keyB, keyA},
		func(ctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
			invoked.Store(true)
			return plusOneResults(exp), nil
		})
	if !errors.Is(err, ErrSaverReconcileRequired) {
		t.Fatalf("err = %v, want ErrSaverReconcileRequired", err)
	}
	if out != nil {
		t.Fatalf("out = %v, want nil", out)
	}
	if invoked.Load() {
		t.Fatal("callback invoked despite blocked participant")
	}
	// No other participant metadata changed.
	snapA, _ := s.Inspect(keyA)
	if snapA.KnownRevision != 3 || snapA.Blocked || snapA.Dirty || snapA.InFlight {
		t.Errorf("A = %+v, want known 3 clean/unblocked/idle", snapA)
	}
	snapB, _ := s.Inspect(keyB)
	if !snapB.Blocked {
		t.Errorf("B = %+v, want still blocked", snapB)
	}
}

// ---- ANY callback error: blanket block, cause preserved, recovery via ResolveReconciled ----

func TestSaverCriticalSetCallbackErrorBlocksAll(t *testing.T) {
	s := mustSaver(t, newManualClock())
	keyA := charSaverKey(1)
	keyB := itemSaverKey(2)
	mustTrack(t, s, keyA, 3)
	mustTrack(t, s, keyB, 4)
	var oldRan atomic.Bool
	if err := s.MarkDirty(keyA, func(ctx context.Context, exp int64) (int64, error) {
		oldRan.Store(true)
		return exp + 1, nil
	}); err != nil {
		t.Fatal(err)
	}
	out, err := s.WriteCriticalSet(context.Background(), []AggregateKey{keyA, keyB},
		func(ctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
			return nil, errCriticalBoom
		})
	if !errors.Is(err, errCriticalBoom) {
		t.Errorf("err = %v, want callback cause preserved", err)
	}
	if !errors.Is(err, ErrSaverReconcileRequired) {
		t.Errorf("err = %v, want ErrSaverReconcileRequired", err)
	}
	if out != nil {
		t.Errorf("out = %v, want nil", out)
	}
	for k, want := range map[AggregateKey]int64{keyA: 3, keyB: 4} {
		snap, ierr := s.Inspect(k)
		if ierr != nil {
			t.Fatal(ierr)
		}
		if snap.KnownRevision != want {
			t.Errorf("%v known = %d, want unchanged %d", k, snap.KnownRevision, want)
		}
		if !snap.Blocked || snap.InFlight {
			t.Errorf("%v = %+v, want blocked/idle", k, snap)
		}
	}
	if oldRan.Load() {
		t.Error("pre-existing pending snapshot ran during failed critical set")
	}
	snapA, _ := s.Inspect(keyA)
	if !snapA.Dirty {
		t.Error("pre-existing pending snapshot cleared by callback error")
	}

	// Recovery phase: every writer rejects until reconciled, and a
	// retrying critical set never invokes its callback.
	if err := s.MarkDirty(keyA, casOK); !errors.Is(err, ErrSaverReconcileRequired) {
		t.Errorf("MarkDirty after error = %v, want ErrSaverReconcileRequired", err)
	}
	if _, err := s.WriteThrough(context.Background(), keyB, casOK); !errors.Is(err, ErrSaverReconcileRequired) {
		t.Errorf("WriteThrough after error = %v, want ErrSaverReconcileRequired", err)
	}
	var retryInvoked atomic.Bool
	if _, err := s.WriteCriticalSet(context.Background(), []AggregateKey{keyA, keyB},
		func(ctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
			retryInvoked.Store(true)
			return plusOneResults(exp), nil
		}); !errors.Is(err, ErrSaverReconcileRequired) {
		t.Errorf("retrying critical set = %v, want ErrSaverReconcileRequired", err)
	}
	if retryInvoked.Load() {
		t.Error("retrying critical callback invoked despite block")
	}
	// Resolve each participant with its authoritative revision;
	// all become clean/unblocked again through the ONE existing
	// reconciliation mechanism.
	ctx := context.Background()
	if err := s.ResolveReconciled(ctx, keyA, 3); err != nil {
		t.Fatalf("ResolveReconciled A: %v", err)
	}
	if err := s.ResolveReconciled(ctx, keyB, 4); err != nil {
		t.Fatalf("ResolveReconciled B: %v", err)
	}
	for _, k := range []AggregateKey{keyA, keyB} {
		snap, ierr := s.Inspect(k)
		if ierr != nil {
			t.Fatal(ierr)
		}
		if snap.Blocked || snap.Dirty || snap.InFlight {
			t.Errorf("post-resolve %v = %+v, want clean/unblocked/idle", k, snap)
		}
	}
	out, err = s.WriteCriticalSet(ctx, []AggregateKey{keyB, keyA},
		func(cctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
			return plusOneResults(exp), nil
		})
	if err != nil {
		t.Fatalf("critical set after resolve: %v", err)
	}
	if len(out) != 2 || out[0].Revision != 4 || out[1].Revision != 5 {
		t.Errorf("post-resolve out = %v, want [{A 4} {B 5}] canonical", out)
	}
}

// ---- stale cause stays discoverable through the blanket block ----

func TestSaverCriticalSetStaleCausePreserved(t *testing.T) {
	s := mustSaver(t, newManualClock())
	keyA := charSaverKey(1)
	keyB := itemSaverKey(2)
	mustTrack(t, s, keyA, 0)
	mustTrack(t, s, keyB, 0)
	_, err := s.WriteCriticalSet(context.Background(), []AggregateKey{keyA, keyB},
		func(ctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
			return nil, errors.Join(errSaverTransient, ErrSnapshotStale)
		})
	if !errors.Is(err, ErrSnapshotStale) {
		t.Errorf("err = %v, want ErrSnapshotStale discoverable", err)
	}
	if !errors.Is(err, errSaverTransient) {
		t.Errorf("err = %v, want transient cause discoverable", err)
	}
	if !errors.Is(err, ErrSaverReconcileRequired) {
		t.Errorf("err = %v, want ErrSaverReconcileRequired", err)
	}
	for _, k := range []AggregateKey{keyA, keyB} {
		snap, ierr := s.Inspect(k)
		if ierr != nil {
			t.Fatal(ierr)
		}
		if !snap.Blocked || snap.KnownRevision != 0 {
			t.Errorf("%v = %+v, want blocked at known 0", k, snap)
		}
	}
}

// ---- success-result invariant matrix ----

func TestSaverCriticalSetResultInvariantMatrix(t *testing.T) {
	keyA := charSaverKey(1)
	keyB := itemSaverKey(2)
	extra := charSaverKey(9)
	newSet := func(t *testing.T) *Saver {
		t.Helper()
		s := mustSaver(t, newManualClock())
		mustTrack(t, s, keyA, 2)
		mustTrack(t, s, keyB, 5)
		var oldRan atomic.Bool
		if err := s.MarkDirty(keyA, func(ctx context.Context, exp int64) (int64, error) {
			oldRan.Store(true)
			return exp + 1, nil
		}); err != nil {
			t.Fatal(err)
		}
		return s
	}
	cases := map[string][]AggregateRevision{
		"missing key":    {{keyA, 3}},
		"extra key":      {{keyA, 3}, {keyB, 6}, {extra, 1}},
		"duplicate key":  {{keyA, 3}, {keyA, 3}},
		"one unchanged":  {{keyA, 2}, {keyB, 6}},
		"jump by two":    {{keyA, 3}, {keyB, 7}},
		"negative":       {{keyA, 3}, {keyB, -1}},
		"swapped inputs": {{keyB, 3}, {keyA, 6}},
	}
	for name, bad := range cases {
		s := newSet(t)
		out, err := s.WriteCriticalSet(context.Background(), []AggregateKey{keyA, keyB},
			func(ctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
				return append([]AggregateRevision(nil), bad...), nil
			})
		if !errors.Is(err, ErrSaverRevisionInvariant) {
			t.Errorf("%s: err = %v, want ErrSaverRevisionInvariant", name, err)
		}
		if !errors.Is(err, ErrSaverReconcileRequired) {
			t.Errorf("%s: err = %v, want ErrSaverReconcileRequired", name, err)
		}
		if out != nil {
			t.Errorf("%s: out = %v, want nil (no partial acceptance)", name, out)
		}
		for k, want := range map[AggregateKey]int64{keyA: 2, keyB: 5} {
			snap, ierr := s.Inspect(k)
			if ierr != nil {
				t.Fatal(ierr)
			}
			if snap.KnownRevision != want {
				t.Errorf("%s: %v known = %d, want unchanged %d", name, k, snap.KnownRevision, want)
			}
			if !snap.Blocked || snap.InFlight {
				t.Errorf("%s: %v = %+v, want blocked/idle", name, k, snap)
			}
		}
		snapA, _ := s.Inspect(keyA)
		if !snapA.Dirty {
			t.Errorf("%s: pre-existing pending cleared by invariant failure", name)
		}
	}
}

// ---- cancellation while waiting on a held gate: no callback, no block ----

func TestSaverCriticalSetContextCancelWhileWaiting(t *testing.T) {
	s := mustSaver(t, newManualClock())
	keyA := charSaverKey(1)
	keyB := itemSaverKey(2)
	mustTrack(t, s, keyA, 0)
	mustTrack(t, s, keyB, 0)
	enteredB := make(chan int64, 1)
	proceedB := make(chan int64, 1)
	wtDone := make(chan error, 1)
	go func() {
		_, err := s.WriteThrough(context.Background(), keyB,
			func(ctx context.Context, exp int64) (int64, error) {
				enteredB <- exp
				return <-proceedB, nil
			})
		wtDone <- err
	}()
	if exp := <-enteredB; exp != 0 {
		t.Fatalf("B writer expected = %d, want 0", exp)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var invoked atomic.Bool
	critDone := make(chan error, 1)
	go func() {
		_, err := s.WriteCriticalSet(ctx, []AggregateKey{keyA, keyB},
			func(cctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
				invoked.Store(true)
				return plusOneResults(exp), nil
			})
		critDone <- err
	}()
	// Yield so the critical set can contend on the held B gate;
	// the assertions below hold whichever gate boundary the
	// cancellation lands on, as long as it precedes invocation.
	for i := 0; i < 500; i++ {
		runtime.Gosched()
	}
	cancel()
	proceedB <- 1
	if err := <-wtDone; err != nil {
		t.Fatalf("WriteThrough B: %v", err)
	}
	select {
	case err := <-critDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("critical err = %v, want context.Canceled", err)
		}
		if errors.Is(err, ErrSaverReconcileRequired) {
			t.Fatalf("critical err = %v, must NOT require reconciliation (no callback ran)", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled critical set never returned")
	}
	if invoked.Load() {
		t.Fatal("callback invoked after cancellation")
	}
	for _, k := range []AggregateKey{keyA, keyB} {
		snap, ierr := s.Inspect(k)
		if ierr != nil {
			t.Fatal(ierr)
		}
		if snap.Blocked || snap.InFlight {
			t.Errorf("%v = %+v, want unblocked/idle after pre-callback cancel", k, snap)
		}
	}
	// All gates released: ordinary writes proceed on both keys.
	if _, err := s.WriteThrough(context.Background(), keyA, casOK); err != nil {
		t.Errorf("WriteThrough A after cancel: %v", err)
	}
	if _, err := s.WriteThrough(context.Background(), keyB, casOK); err != nil {
		t.Errorf("WriteThrough B after cancel: %v", err)
	}
}

// ---- pre-cancelled context: no callback, no block ----

func TestSaverCriticalSetPreCancelledContext(t *testing.T) {
	s := mustSaver(t, newManualClock())
	keyA := charSaverKey(1)
	mustTrack(t, s, keyA, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var invoked atomic.Bool
	out, err := s.WriteCriticalSet(ctx, []AggregateKey{keyA},
		func(cctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
			invoked.Store(true)
			return plusOneResults(exp), nil
		})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if out != nil {
		t.Fatalf("out = %v, want nil", out)
	}
	if invoked.Load() {
		t.Fatal("callback invoked on cancelled context")
	}
	snap, _ := s.Inspect(keyA)
	if snap.Blocked || snap.Dirty || snap.InFlight || snap.KnownRevision != 0 {
		t.Errorf("A = %+v, want untouched known 0", snap)
	}
}

// ---- callback returning context cancellation: full blanket block ----

func TestSaverCriticalSetCallbackContextCanceled(t *testing.T) {
	s := mustSaver(t, newManualClock())
	keyA := charSaverKey(1)
	keyB := itemSaverKey(2)
	mustTrack(t, s, keyA, 7)
	mustTrack(t, s, keyB, 8)
	_, err := s.WriteCriticalSet(context.Background(), []AggregateKey{keyA, keyB},
		func(ctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
			return nil, context.Canceled
		})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled discoverable", err)
	}
	if !errors.Is(err, ErrSaverReconcileRequired) {
		t.Errorf("err = %v, want ErrSaverReconcileRequired", err)
	}
	for k, want := range map[AggregateKey]int64{keyA: 7, keyB: 8} {
		snap, ierr := s.Inspect(k)
		if ierr != nil {
			t.Fatal(ierr)
		}
		if snap.KnownRevision != want || !snap.Blocked || snap.InFlight {
			t.Errorf("%v = %+v, want blocked at known %d", k, snap, want)
		}
	}
}

// ---- MaxInt64 fail-closed: no callback, unrelated untouched ----

func TestSaverCriticalSetMaxInt64FailClosed(t *testing.T) {
	s := mustSaver(t, newManualClock())
	keyA := charSaverKey(1)
	keyB := itemSaverKey(2)
	mustTrack(t, s, keyA, 0)
	mustTrack(t, s, keyB, math.MaxInt64)
	var invoked atomic.Bool
	out, err := s.WriteCriticalSet(context.Background(), []AggregateKey{keyA, keyB},
		func(ctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
			invoked.Store(true)
			return plusOneResults(exp), nil
		})
	if !errors.Is(err, ErrSaverRevisionInvariant) {
		t.Fatalf("err = %v, want ErrSaverRevisionInvariant", err)
	}
	if out != nil {
		t.Fatalf("out = %v, want nil", out)
	}
	if invoked.Load() {
		t.Fatal("callback invoked at MaxInt64")
	}
	snapA, _ := s.Inspect(keyA)
	if snapA.KnownRevision != 0 || snapA.Blocked || snapA.Dirty || snapA.InFlight {
		t.Errorf("unrelated A = %+v, want untouched known 0", snapA)
	}
	snapB, _ := s.Inspect(keyB)
	if snapB.KnownRevision != math.MaxInt64 {
		t.Errorf("B known = %d, want unchanged MaxInt64", snapB.KnownRevision)
	}
}

// ---- dirty-generation exhaustion: no callback, no mutation ----

func TestSaverCriticalSetSeqExhaustion(t *testing.T) {
	s := mustSaver(t, newManualClock())
	keyA := charSaverKey(1)
	mustTrack(t, s, keyA, 0)
	s.mu.Lock()
	s.entries[keyA].seq = math.MaxUint64
	s.mu.Unlock()
	var invoked atomic.Bool
	out, err := s.WriteCriticalSet(context.Background(), []AggregateKey{keyA},
		func(ctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
			invoked.Store(true)
			return plusOneResults(exp), nil
		})
	if !errors.Is(err, ErrSaverRevisionInvariant) {
		t.Fatalf("err = %v, want ErrSaverRevisionInvariant", err)
	}
	if out != nil {
		t.Fatalf("out = %v, want nil", out)
	}
	if invoked.Load() {
		t.Fatal("callback invoked on generation exhaustion")
	}
	snap, _ := s.Inspect(keyA)
	if snap.KnownRevision != 0 || snap.Blocked || snap.Dirty || snap.InFlight {
		t.Errorf("A = %+v, want untouched known 0", snap)
	}
}

// ---- caller immutability / determinism ----

func TestSaverCriticalSetCallerSlicesImmutable(t *testing.T) {
	s := mustSaver(t, newManualClock())
	c7 := charSaverKey(7)
	i10 := itemSaverKey(10)
	i20 := itemSaverKey(20)
	mustTrack(t, s, c7, 1)
	mustTrack(t, s, i10, 2)
	mustTrack(t, s, i20, 3)
	keys := []AggregateKey{i20, c7, i10}
	orig := append([]AggregateKey(nil), keys...)
	var gotExp []AggregateRevision
	out, err := s.WriteCriticalSet(context.Background(), keys,
		func(ctx context.Context, exp []AggregateRevision) ([]AggregateRevision, error) {
			gotExp = append([]AggregateRevision(nil), exp...)
			// Hostile mutation of the supplied slice must not
			// alter Saver metadata or the committed result.
			exp[0].Revision = 9999
			exp[0].Key = bankSaverKey(1, "tos")
			exp[1].Revision = -5
			return plusOneResults(gotExp), nil
		})
	if err != nil {
		t.Fatalf("WriteCriticalSet: %v", err)
	}
	for i := range keys {
		if keys[i] != orig[i] {
			t.Fatalf("input keys reordered: %v, want %v", keys, orig)
		}
	}
	if !isCanonicalRevisions(gotExp) {
		t.Fatalf("callback expected not canonical: %v", gotExp)
	}
	if !isCanonicalRevisions(out) {
		t.Fatalf("result not canonical: %v", out)
	}
	want := []AggregateRevision{{c7, 2}, {i10, 3}, {i20, 4}}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("out[%d] = %v, want %v (full %v)", i, out[i], want[i], out)
		}
	}
	for k, rev := range map[AggregateKey]int64{c7: 2, i10: 3, i20: 4} {
		snap, ierr := s.Inspect(k)
		if ierr != nil {
			t.Fatal(ierr)
		}
		if snap.KnownRevision != rev {
			t.Errorf("%v known = %d, want %d", k, snap.KnownRevision, rev)
		}
	}
}
