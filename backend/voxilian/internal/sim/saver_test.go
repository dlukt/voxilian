package sim

import (
	"context"
	"errors"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	randv2 "math/rand/v2"
)

// ---- helpers ----

func charSaverKey(id int64) AggregateKey { return AggregateKey{Kind: AggregateCharacter, ID: id} }

func itemSaverKey(id int64) AggregateKey { return AggregateKey{Kind: AggregateItem, ID: id} }

func bankSaverKey(charID int64, system string) AggregateKey {
	return AggregateKey{Kind: AggregateBank, ID: charID, Scope: system}
}

func mustSaver(t *testing.T, clk Clock) *Saver {
	t.Helper()
	s, err := NewSaver(SaverConfig{Interval: 60 * time.Second, Clock: clk})
	if err != nil {
		t.Fatalf("NewSaver: %v", err)
	}
	return s
}

func mustTrack(t *testing.T, s *Saver, key AggregateKey, rev int64) {
	t.Helper()
	if err := s.Track(key, rev); err != nil {
		t.Fatalf("Track(%v,%d): %v", key, rev, err)
	}
}

// casOK is the standard §8.1 writer: exactly one revision forward.
func casOK(ctx context.Context, exp int64) (int64, error) { return exp + 1, nil }

var errSaverTransient = errors.New("saver test transient")

// waitSaverCond spins without sleeping until cond holds or the
// watchdog fires. It matches the repo's Gosched polling style.
func waitSaverCond(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", desc)
		}
		// Yield, never sleep.
		runtime.Gosched()
	}
}

// ---- B4: key validation, metric names, canonical order ----

func TestSaverAggregateKeyValidation(t *testing.T) {
	valid := []AggregateKey{
		charSaverKey(1),
		itemSaverKey(7),
		bankSaverKey(3, "tos"),
	}
	for _, k := range valid {
		if err := k.validate(); err != nil {
			t.Errorf("validate(%v) = %v, want nil", k, err)
		}
	}
	invalid := []AggregateKey{
		{}, // unknown kind
		{Kind: AggregateKind(99), ID: 1},
		{Kind: AggregateCharacter, ID: 0},
		{Kind: AggregateCharacter, ID: -2},
		{Kind: AggregateItem, ID: 0},
		{Kind: AggregateCharacter, ID: 1, Scope: "x"},
		{Kind: AggregateItem, ID: 1, Scope: "x"},
		{Kind: AggregateBank, ID: 1, Scope: ""},
		{Kind: AggregateBank, ID: 0, Scope: "tos"},
	}
	for _, k := range invalid {
		if err := k.validate(); !errors.Is(err, ErrInvalidAggregateKey) {
			t.Errorf("validate(%v) = %v, want ErrInvalidAggregateKey", k, err)
		}
	}
	if got := AggregateCharacter.MetricName(); got != "character" {
		t.Errorf("character MetricName = %q", got)
	}
	if got := AggregateItem.MetricName(); got != "item" {
		t.Errorf("item MetricName = %q", got)
	}
	if got := AggregateBank.MetricName(); got != "bank" {
		t.Errorf("bank MetricName = %q", got)
	}

	// Canonical order: Kind -> ID -> Scope.
	hostile := []AggregateKey{
		bankSaverKey(2, "b"),
		charSaverKey(9),
		bankSaverKey(2, "a"),
		itemSaverKey(1),
		charSaverKey(1),
		bankSaverKey(1, "z"),
	}
	want := []AggregateKey{
		charSaverKey(1),
		charSaverKey(9),
		itemSaverKey(1),
		bankSaverKey(1, "z"),
		bankSaverKey(2, "a"),
		bankSaverKey(2, "b"),
	}
	got := append([]AggregateKey(nil), hostile...)
	for i := 0; i < len(got); i++ {
		for j := i + 1; j < len(got); j++ {
			if got[j].Less(got[i]) {
				got[i], got[j] = got[j], got[i]
			}
		}
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order[%d] = %v, want %v (full %v)", i, got[i], want[i], got)
		}
	}
}

func TestSaverConstructorValidation(t *testing.T) {
	if _, err := NewSaver(SaverConfig{Interval: 0, Clock: newManualClock()}); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("zero interval err = %v, want ErrInvalidConfig", err)
	}
	if _, err := NewSaver(SaverConfig{Interval: -time.Second, Clock: newManualClock()}); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("negative interval err = %v, want ErrInvalidConfig", err)
	}
	if _, err := NewSaver(SaverConfig{Interval: time.Second, Clock: nil}); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("nil clock err = %v, want ErrInvalidConfig", err)
	}
}

// ---- B5: tracking lifecycle ----

func TestSaverTracking(t *testing.T) {
	s := mustSaver(t, newManualClock())
	mustTrack(t, s, charSaverKey(1), 0)
	mustTrack(t, s, itemSaverKey(7), 7)
	if got := s.TrackedCount(); got != 2 {
		t.Fatalf("TrackedCount = %d, want 2", got)
	}
	if err := s.Track(charSaverKey(2), -1); !errors.Is(err, ErrInvalidDurableRevision) {
		t.Errorf("negative Track err = %v, want ErrInvalidDurableRevision", err)
	}
	if got := s.TrackedCount(); got != 2 {
		t.Errorf("TrackedCount after failed track = %d, want 2", got)
	}
	if err := s.Track(charSaverKey(1), 0); !errors.Is(err, ErrAggregateAlreadyTracked) {
		t.Errorf("duplicate Track err = %v, want ErrAggregateAlreadyTracked", err)
	}
	if err := s.Track(AggregateKey{}, 0); !errors.Is(err, ErrInvalidAggregateKey) {
		t.Errorf("invalid Track err = %v, want ErrInvalidAggregateKey", err)
	}
	if err := s.Untrack(charSaverKey(1)); err != nil {
		t.Errorf("clean Untrack err = %v", err)
	}
	if err := s.Untrack(charSaverKey(1)); !errors.Is(err, ErrAggregateNotTracked) {
		t.Errorf("unknown Untrack err = %v, want ErrAggregateNotTracked", err)
	}
	if err := s.Untrack(AggregateKey{}); !errors.Is(err, ErrInvalidAggregateKey) {
		t.Errorf("invalid Untrack err = %v, want ErrInvalidAggregateKey", err)
	}
	// Dirty untrack rejected.
	mustTrack(t, s, charSaverKey(9), 0)
	if err := s.MarkDirty(charSaverKey(9), casOK); err != nil {
		t.Fatalf("MarkDirty: %v", err)
	}
	if err := s.Untrack(charSaverKey(9)); !errors.Is(err, ErrSaverUntrackDirty) {
		t.Errorf("dirty Untrack err = %v, want ErrSaverUntrackDirty", err)
	}
	// Blocked untrack rejected.
	mustTrack(t, s, charSaverKey(10), 3)
	if err := s.MarkDirty(charSaverKey(10), func(ctx context.Context, exp int64) (int64, error) {
		return 0, ErrSnapshotStale
	}); err != nil {
		t.Fatalf("MarkDirty: %v", err)
	}
	if err := s.FlushDirty(context.Background()); !errors.Is(err, ErrSnapshotStale) {
		t.Fatalf("FlushDirty err = %v, want stale", err)
	}
	if err := s.Untrack(charSaverKey(10)); !errors.Is(err, ErrSaverUntrackDirty) {
		t.Errorf("blocked Untrack err = %v, want ErrSaverUntrackDirty", err)
	}
	// Unknown-key operations.
	if err := s.MarkDirty(charSaverKey(404), casOK); !errors.Is(err, ErrAggregateNotTracked) {
		t.Errorf("unknown MarkDirty err = %v, want ErrAggregateNotTracked", err)
	}
	if err := s.MarkDirty(charSaverKey(9), nil); !errors.Is(err, ErrInvalidSnapshot) {
		t.Errorf("nil MarkDirty err = %v, want ErrInvalidSnapshot", err)
	}
	if _, err := s.WriteThrough(context.Background(), charSaverKey(404), casOK); !errors.Is(err, ErrAggregateNotTracked) {
		t.Errorf("unknown WriteThrough err = %v, want ErrAggregateNotTracked", err)
	}
	if err := s.ResolveReconciled(context.Background(), charSaverKey(404), 1); !errors.Is(err, ErrAggregateNotTracked) {
		t.Errorf("unknown Resolve err = %v, want ErrAggregateNotTracked", err)
	}
	if err := s.ResolveReconciled(context.Background(), charSaverKey(9), -1); !errors.Is(err, ErrInvalidDurableRevision) {
		t.Errorf("negative Resolve err = %v, want ErrInvalidDurableRevision", err)
	}
	if _, err := s.Inspect(charSaverKey(404)); !errors.Is(err, ErrAggregateNotTracked) {
		t.Errorf("unknown Inspect err = %v, want ErrAggregateNotTracked", err)
	}
}

// ---- B6: latest-wins coalescing ----

func TestSaverMarkDirtyLatestWins(t *testing.T) {
	s := mustSaver(t, newManualClock())
	key := charSaverKey(1)
	mustTrack(t, s, key, 0)
	var ran []string
	mk := func(name string) SnapshotWrite {
		return func(ctx context.Context, exp int64) (int64, error) {
			ran = append(ran, name)
			return exp + 1, nil
		}
	}
	if err := s.MarkDirty(key, mk("A1")); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDirty(key, mk("A2")); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDirty(key, mk("A3")); err != nil {
		t.Fatal(err)
	}
	if got := s.DirtyCount(); got != 1 {
		t.Fatalf("DirtyCount = %d, want 1", got)
	}
	if err := s.FlushDirty(context.Background()); err != nil {
		t.Fatalf("FlushDirty: %v", err)
	}
	if len(ran) != 1 || ran[0] != "A3" {
		t.Fatalf("ran = %v, want [A3]", ran)
	}
	if got := s.DirtyCount(); got != 0 {
		t.Fatalf("DirtyCount after flush = %d, want 0", got)
	}
}

// ---- B7: immutable capture proof ----

func TestSaverImmutableCapture(t *testing.T) {
	s := mustSaver(t, newManualClock())
	key := itemSaverKey(5)
	mustTrack(t, s, key, 2)
	src := []int{1, 2, 3}
	// Producer copies before enqueueing (the required pattern).
	frozen := append([]int(nil), src...)
	var observed []int
	if err := s.MarkDirty(key, func(ctx context.Context, exp int64) (int64, error) {
		observed = append([]int(nil), frozen...)
		return exp + 1, nil
	}); err != nil {
		t.Fatal(err)
	}
	src[0] = 99 // live mutation after capture must not leak in.
	if err := s.FlushDirty(context.Background()); err != nil {
		t.Fatalf("FlushDirty: %v", err)
	}
	if len(observed) != 3 || observed[0] != 1 || observed[1] != 2 || observed[2] != 3 {
		t.Fatalf("observed = %v, want [1 2 3]", observed)
	}
}

// ---- B8: known revision passed into writer ----

func TestSaverKnownRevisionPassedToWriter(t *testing.T) {
	s := mustSaver(t, newManualClock())
	key := bankSaverKey(11, "tos")
	mustTrack(t, s, key, 7)
	var gotExp int64 = -1
	if err := s.MarkDirty(key, func(ctx context.Context, exp int64) (int64, error) {
		gotExp = exp
		return 8, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushDirty(context.Background()); err != nil {
		t.Fatalf("FlushDirty: %v", err)
	}
	if gotExp != 7 {
		t.Fatalf("expectedRevision = %d, want 7", gotExp)
	}
	snap, err := s.Inspect(key)
	if err != nil {
		t.Fatal(err)
	}
	if snap.KnownRevision != 8 || snap.Dirty || snap.Blocked {
		t.Fatalf("snap = %+v, want known8 clean unblocked", snap)
	}
}

// ---- B9: revision invariant ----

func TestSaverRevisionInvariant(t *testing.T) {
	for _, ret := range []int64{9, 7} { // +2 and unchanged from expected 7
		s := mustSaver(t, newManualClock())
		key := charSaverKey(1)
		mustTrack(t, s, key, 7)
		if err := s.MarkDirty(key, func(ctx context.Context, exp int64) (int64, error) {
			return ret, nil
		}); err != nil {
			t.Fatal(err)
		}
		err := s.FlushDirty(context.Background())
		if !errors.Is(err, ErrSaverRevisionInvariant) {
			t.Fatalf("ret %d: FlushDirty err = %v, want invariant", ret, err)
		}
		snap, _ := s.Inspect(key)
		if !snap.Blocked || snap.KnownRevision != 7 {
			t.Fatalf("ret %d: snap = %+v, want blocked known7", ret, snap)
		}
	}
}

// ---- B10: MaxInt64 fails before callback ----

func TestSaverMaxInt64FailsClosed(t *testing.T) {
	s := mustSaver(t, newManualClock())
	key := charSaverKey(1)
	mustTrack(t, s, key, math.MaxInt64)
	calls := 0
	if err := s.MarkDirty(key, func(ctx context.Context, exp int64) (int64, error) {
		calls++
		return exp + 1, nil
	}); err != nil {
		t.Fatal(err)
	}
	err := s.FlushDirty(context.Background())
	if !errors.Is(err, ErrSaverRevisionInvariant) {
		t.Fatalf("FlushDirty err = %v, want invariant", err)
	}
	if calls != 0 {
		t.Fatalf("writer calls = %d, want 0 (no overflow attempt)", calls)
	}
}

// ---- B11: dirty-while-in-flight barrier ----

func TestSaverDirtyWhileInFlight(t *testing.T) {
	s := mustSaver(t, newManualClock())
	key := charSaverKey(1)
	mustTrack(t, s, key, 0)
	entered := make(chan int64, 1)
	proceed := make(chan int64, 1)
	writerA := func(ctx context.Context, exp int64) (int64, error) {
		entered <- exp
		return <-proceed, nil
	}
	if err := s.MarkDirty(key, writerA); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	done := make(chan error, 1)
	go func() { done <- s.FlushDirty(ctx) }()
	if exp := <-entered; exp != 0 {
		t.Fatalf("A expected = %d, want 0", exp)
	}
	var expB int64 = -1
	if err := s.MarkDirty(key, func(ctx context.Context, exp int64) (int64, error) {
		expB = exp
		return exp + 1, nil
	}); err != nil {
		t.Fatal(err)
	}
	proceed <- 1 // A commits rev 1.
	if err := <-done; err != nil {
		t.Fatalf("first flush: %v", err)
	}
	snap, _ := s.Inspect(key)
	if snap.KnownRevision != 1 || !snap.Dirty {
		t.Fatalf("after A: snap = %+v, want known1 dirty", snap)
	}
	if err := s.FlushDirty(ctx); err != nil {
		t.Fatalf("second flush: %v", err)
	}
	if expB != 1 {
		t.Fatalf("B expected = %d, want 1", expB)
	}
	snap, _ = s.Inspect(key)
	if snap.KnownRevision != 2 || snap.Dirty {
		t.Fatalf("final snap = %+v, want known2 clean", snap)
	}
}

// ---- B12: transient retry ----

func TestSaverTransientRetry(t *testing.T) {
	s := mustSaver(t, newManualClock())
	key := charSaverKey(1)
	mustTrack(t, s, key, 4)
	var exps []int64
	attempt := 0
	if err := s.MarkDirty(key, func(ctx context.Context, exp int64) (int64, error) {
		attempt++
		exps = append(exps, exp)
		if attempt == 1 {
			return 0, errSaverTransient
		}
		return exp + 1, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushDirty(context.Background()); !errors.Is(err, errSaverTransient) {
		t.Fatalf("first flush err = %v, want transient", err)
	}
	snap, _ := s.Inspect(key)
	if snap.KnownRevision != 4 || !snap.Dirty || snap.Blocked {
		t.Fatalf("after transient: snap = %+v, want known4 dirty unblocked", snap)
	}
	if err := s.FlushDirty(context.Background()); err != nil {
		t.Fatalf("retry flush: %v", err)
	}
	if len(exps) != 2 || exps[0] != 4 || exps[1] != 4 {
		t.Fatalf("exps = %v, want [4 4]", exps)
	}
	snap, _ = s.Inspect(key)
	if snap.KnownRevision != 5 || snap.Dirty {
		t.Fatalf("final snap = %+v, want known5 clean", snap)
	}
}

// ---- B13/B14/B15: stale block + reconciliation ----

func TestSaverStaleBlockAndResolve(t *testing.T) {
	s := mustSaver(t, newManualClock())
	key := charSaverKey(1)
	mustTrack(t, s, key, 3)
	calls := 0
	stale := func(ctx context.Context, exp int64) (int64, error) {
		calls++
		return 0, ErrSnapshotStale
	}
	if err := s.MarkDirty(key, stale); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushDirty(context.Background()); !errors.Is(err, ErrSnapshotStale) {
		t.Fatalf("flush err = %v, want stale", err)
	}
	snap, _ := s.Inspect(key)
	if snap.KnownRevision != 3 || !snap.Blocked {
		t.Fatalf("after stale: snap = %+v, want known3 blocked", snap)
	}
	// Automatic retries stop: no writer call, reconcile error out.
	if err := s.FlushDirty(context.Background()); !errors.Is(err, ErrSaverReconcileRequired) {
		t.Fatalf("retry flush err = %v, want reconcile-required", err)
	}
	if calls != 1 {
		t.Fatalf("writer calls = %d, want 1 (no stale hammering)", calls)
	}
	if err := s.MarkDirty(key, casOK); !errors.Is(err, ErrSaverReconcileRequired) {
		t.Errorf("blocked MarkDirty err = %v, want reconcile-required", err)
	}
	if _, err := s.WriteThrough(context.Background(), key, casOK); !errors.Is(err, ErrSaverReconcileRequired) {
		t.Errorf("blocked WriteThrough err = %v, want reconcile-required", err)
	}
	// Regression guard: resolve below known fails, block stays.
	if err := s.ResolveReconciled(context.Background(), key, 2); !errors.Is(err, ErrSaverRevisionInvariant) {
		t.Errorf("regress Resolve err = %v, want invariant", err)
	}
	snap, _ = s.Inspect(key)
	if snap.KnownRevision != 3 || !snap.Blocked {
		t.Fatalf("after regress: snap = %+v, want known3 blocked", snap)
	}
	// Successful reconciliation: known jumps, clean, unblocked.
	if err := s.ResolveReconciled(context.Background(), key, 5); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	snap, _ = s.Inspect(key)
	if snap.KnownRevision != 5 || snap.Dirty || snap.Blocked {
		t.Fatalf("after resolve: snap = %+v, want known5 clean unblocked", snap)
	}
	// Fresh post-reconcile mutation saves against the new revision.
	var gotExp int64 = -1
	if err := s.MarkDirty(key, func(ctx context.Context, exp int64) (int64, error) {
		gotExp = exp
		return exp + 1, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushDirty(context.Background()); err != nil {
		t.Fatalf("post-reconcile flush: %v", err)
	}
	if gotExp != 5 {
		t.Fatalf("post-reconcile expected = %d, want 5", gotExp)
	}
}

// Resolve discards pre-reload pending jobs queued during the
// in-flight stale save.
func TestSaverResolveDiscardsPreReloadPending(t *testing.T) {
	s := mustSaver(t, newManualClock())
	key := itemSaverKey(2)
	mustTrack(t, s, key, 0)
	entered := make(chan struct{})
	proceed := make(chan error, 1)
	if err := s.MarkDirty(key, func(ctx context.Context, exp int64) (int64, error) {
		close(entered)
		return 0, <-proceed
	}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.FlushDirty(context.Background()) }()
	<-entered
	bRan := false
	if err := s.MarkDirty(key, func(ctx context.Context, exp int64) (int64, error) {
		bRan = true
		return exp + 1, nil
	}); err != nil {
		t.Fatal(err)
	}
	proceed <- ErrSnapshotStale
	if err := <-done; !errors.Is(err, ErrSnapshotStale) {
		t.Fatalf("flush err = %v, want stale", err)
	}
	snap, _ := s.Inspect(key)
	if !snap.Blocked || !snap.Dirty {
		t.Fatalf("snap = %+v, want blocked with pre-reload pending", snap)
	}
	if err := s.ResolveReconciled(context.Background(), key, 4); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := s.FlushDirty(context.Background()); err != nil {
		t.Fatalf("post-resolve flush: %v", err)
	}
	if bRan {
		t.Fatal("pre-reload pending snapshot ran after reconcile: must be discarded")
	}
	if got := s.DirtyCount(); got != 0 {
		t.Fatalf("DirtyCount = %d, want 0", got)
	}
}

// ---- B16-B20: critical write-through ----

func TestSaverWriteThroughBasic(t *testing.T) {
	s := mustSaver(t, newManualClock())
	key := charSaverKey(1)
	mustTrack(t, s, key, 0)
	var gotExp int64 = -1
	var calls int32
	rev, err := s.WriteThrough(context.Background(), key,
		func(ctx context.Context, exp int64) (int64, error) {
			atomic.AddInt32(&calls, 1)
			gotExp = exp
			return exp + 1, nil
		})
	if err != nil || rev != 1 {
		t.Fatalf("WriteThrough = (%d,%v), want (1,nil)", rev, err)
	}
	if gotExp != 0 || atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("exp = %d calls = %d, want 0/1", gotExp, calls)
	}
	snap, _ := s.Inspect(key)
	if snap.KnownRevision != 1 || snap.Dirty {
		t.Fatalf("snap = %+v, want known1 clean", snap)
	}
}

func TestSaverWriteThroughSupersedesOlderPending(t *testing.T) {
	s := mustSaver(t, newManualClock())
	key := charSaverKey(1)
	mustTrack(t, s, key, 0)
	aRan := false
	if err := s.MarkDirty(key, func(ctx context.Context, exp int64) (int64, error) {
		aRan = true
		return exp + 1, nil
	}); err != nil {
		t.Fatal(err)
	}
	rev, err := s.WriteThrough(context.Background(), key, casOK)
	if err != nil || rev != 1 {
		t.Fatalf("WriteThrough = (%d,%v)", rev, err)
	}
	if err := s.FlushDirty(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if aRan {
		t.Fatal("older pending A ran after critical B: must be superseded")
	}
	if got := s.DirtyCount(); got != 0 {
		t.Fatalf("DirtyCount = %d, want 0", got)
	}
}

func TestSaverWriteThroughNewerPendingSurvives(t *testing.T) {
	s := mustSaver(t, newManualClock())
	key := charSaverKey(1)
	mustTrack(t, s, key, 0)
	entered := make(chan int64, 1)
	proceed := make(chan int64, 1)
	writerB := func(ctx context.Context, exp int64) (int64, error) {
		entered <- exp
		return <-proceed, nil
	}
	done := make(chan struct {
		rev int64
		err error
	}, 1)
	go func() {
		rev, err := s.WriteThrough(context.Background(), key, writerB)
		done <- struct {
			rev int64
			err error
		}{rev, err}
	}()
	if exp := <-entered; exp != 0 {
		t.Fatalf("B expected = %d, want 0", exp)
	}
	var expC int64 = -1
	if err := s.MarkDirty(key, func(ctx context.Context, exp int64) (int64, error) {
		expC = exp
		return exp + 1, nil
	}); err != nil {
		t.Fatal(err)
	}
	proceed <- 1
	res := <-done
	if res.err != nil || res.rev != 1 {
		t.Fatalf("WriteThrough = (%d,%v)", res.rev, res.err)
	}
	snap, _ := s.Inspect(key)
	if snap.KnownRevision != 1 || !snap.Dirty {
		t.Fatalf("after B: snap = %+v, want known1 dirty (C retained)", snap)
	}
	if err := s.FlushDirty(context.Background()); err != nil {
		t.Fatalf("flush C: %v", err)
	}
	if expC != 1 {
		t.Fatalf("C expected = %d, want 1", expC)
	}
	snap, _ = s.Inspect(key)
	if snap.KnownRevision != 2 || snap.Dirty {
		t.Fatalf("final snap = %+v, want known2 clean", snap)
	}
}

func TestSaverWriteThroughFailedRetained(t *testing.T) {
	s := mustSaver(t, newManualClock())
	key := charSaverKey(1)
	mustTrack(t, s, key, 6)
	calls := 0
	if _, err := s.WriteThrough(context.Background(), key,
		func(ctx context.Context, exp int64) (int64, error) {
			calls++
			if calls == 1 {
				return 0, errSaverTransient
			}
			return exp + 1, nil
		}); !errors.Is(err, errSaverTransient) {
		t.Fatalf("WriteThrough err = %v, want transient", err)
	}
	snap, _ := s.Inspect(key)
	if !snap.Dirty || snap.KnownRevision != 6 {
		t.Fatalf("after failed critical: snap = %+v, want known6 dirty", snap)
	}
	if err := s.FlushDirty(context.Background()); err != nil {
		t.Fatalf("retry flush: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (periodic retry of retained critical)", calls)
	}
	snap, _ = s.Inspect(key)
	if snap.KnownRevision != 7 || snap.Dirty {
		t.Fatalf("final snap = %+v, want known7 clean", snap)
	}
}

func TestSaverWriteThroughFailedWithNewerSnapshot(t *testing.T) {
	s := mustSaver(t, newManualClock())
	key := charSaverKey(1)
	mustTrack(t, s, key, 0)
	entered := make(chan struct{})
	proceed := make(chan error, 1)
	cCalls := 0
	writerC := func(ctx context.Context, exp int64) (int64, error) {
		cCalls++
		close(entered)
		return 0, <-proceed
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.WriteThrough(context.Background(), key, writerC)
		done <- err
	}()
	<-entered
	dCalls := 0
	var expD int64 = -1
	if err := s.MarkDirty(key, func(ctx context.Context, exp int64) (int64, error) {
		dCalls++
		expD = exp
		return exp + 1, nil
	}); err != nil {
		t.Fatal(err)
	}
	proceed <- errSaverTransient
	if err := <-done; !errors.Is(err, errSaverTransient) {
		t.Fatalf("WriteThrough err = %v, want transient", err)
	}
	if err := s.FlushDirty(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if dCalls != 1 || expD != 0 {
		t.Fatalf("dCalls = %d expD = %d, want 1/0 (newer D wins)", dCalls, expD)
	}
	if cCalls != 1 {
		t.Fatalf("cCalls = %d, want 1 (failed C never re-ran over D)", cCalls)
	}
}

func TestSaverStaleWriteThroughBlocks(t *testing.T) {
	s := mustSaver(t, newManualClock())
	key := charSaverKey(1)
	mustTrack(t, s, key, 2)
	_, err := s.WriteThrough(context.Background(), key,
		func(ctx context.Context, exp int64) (int64, error) { return 0, ErrSnapshotStale })
	if !errors.Is(err, ErrSnapshotStale) {
		t.Fatalf("WriteThrough err = %v, want stale", err)
	}
	snap, _ := s.Inspect(key)
	if !snap.Blocked || snap.KnownRevision != 2 {
		t.Fatalf("snap = %+v, want blocked known2", snap)
	}
}

// ---- B21: same-key serialization, periodic vs critical ----

func TestSaverSameKeySerialization(t *testing.T) {
	s := mustSaver(t, newManualClock())
	key := charSaverKey(1)
	mustTrack(t, s, key, 0)
	enteredA := make(chan int64, 1)
	proceedA := make(chan int64, 1)
	if err := s.MarkDirty(key, func(ctx context.Context, exp int64) (int64, error) {
		enteredA <- exp
		return <-proceedA, nil
	}); err != nil {
		t.Fatal(err)
	}
	flushDone := make(chan error, 1)
	go func() { flushDone <- s.FlushDirty(context.Background()) }()
	if exp := <-enteredA; exp != 0 {
		t.Fatalf("A expected = %d, want 0", exp)
	}
	var cur, maxSame int32
	bStarted := make(chan int64, 1)
	var expB int64 = -1
	wtDone := make(chan error, 1)
	go func() {
		_, err := s.WriteThrough(context.Background(), key,
			func(ctx context.Context, exp int64) (int64, error) {
				bStarted <- exp
				n := atomic.AddInt32(&cur, 1)
				for {
					m := atomic.LoadInt32(&maxSame)
					if n <= m || atomic.CompareAndSwapInt32(&maxSame, m, n) {
						break
					}
				}
				for i := 0; i < 200; i++ {
				}
				atomic.AddInt32(&cur, -1)
				expB = exp
				return exp + 1, nil
			})
		wtDone <- err
	}()
	// B must not begin while A owns the slot: yield widely, assert silence.
	for i := 0; i < 2000; i++ {
		select {
		case exp := <-bStarted:
			t.Fatalf("critical B began with exp %d while periodic A in flight", exp)
		default:
		}
	}
	proceedA <- 1
	if err := <-flushDone; err != nil {
		t.Fatalf("flush A: %v", err)
	}
	select {
	case exp := <-bStarted:
		if exp != 1 {
			t.Fatalf("B expected = %d, want A's acknowledged 1", exp)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("critical B never began after A released")
	}
	if err := <-wtDone; err != nil {
		t.Fatalf("WriteThrough B: %v", err)
	}
	if expB != 1 {
		t.Fatalf("B expected = %d, want 1", expB)
	}
	if got := atomic.LoadInt32(&maxSame); got != 1 {
		t.Fatalf("max same-key concurrency = %d, want 1", got)
	}
	snap, _ := s.Inspect(key)
	if snap.KnownRevision != 2 || snap.Dirty {
		t.Fatalf("final snap = %+v, want known2 clean", snap)
	}
}

// ---- B22/B23: different-key independence ----

func TestSaverDifferentKeyIndependence(t *testing.T) {
	s := mustSaver(t, newManualClock())
	keyA := charSaverKey(1)
	keyB := itemSaverKey(2)
	mustTrack(t, s, keyA, 0)
	mustTrack(t, s, keyB, 10)
	enteredA := make(chan struct{})
	proceedA := make(chan int64, 1)
	if err := s.MarkDirty(keyA, func(ctx context.Context, exp int64) (int64, error) {
		close(enteredA)
		return <-proceedA, nil
	}); err != nil {
		t.Fatal(err)
	}
	flushDone := make(chan error, 1)
	go func() { flushDone <- s.FlushDirty(context.Background()) }()
	<-enteredA
	// MarkDirty for B returns promptly while A is blocked.
	if err := s.MarkDirty(keyB, casOK); err != nil {
		t.Fatalf("MarkDirty B while A blocked: %v", err)
	}
	// Critical write-through for B completes before A is released.
	rev, err := s.WriteThrough(context.Background(), keyB, casOK)
	if err != nil || rev != 11 {
		t.Fatalf("WriteThrough B = (%d,%v), want (11,nil)", rev, err)
	}
	proceedA <- 1
	if err := <-flushDone; err != nil {
		t.Fatalf("flush: %v", err)
	}
	snapA, _ := s.Inspect(keyA)
	snapB, _ := s.Inspect(keyB)
	if snapA.KnownRevision != 1 || snapB.KnownRevision != 11 {
		t.Fatalf("snaps = %+v %+v, want known1/known11", snapA, snapB)
	}
}

// ---- B24/B25: canonical order + continue after failure ----

func TestSaverFlushDirtyCanonicalOrder(t *testing.T) {
	s := mustSaver(t, newManualClock())
	hostile := []AggregateKey{
		bankSaverKey(2, "b"),
		charSaverKey(9),
		bankSaverKey(2, "a"),
		itemSaverKey(1),
		charSaverKey(1),
		bankSaverKey(1, "z"),
	}
	var mu sync.Mutex
	var trace []AggregateKey
	for i, k := range hostile {
		mustTrack(t, s, k, int64(i))
		k := k
		if err := s.MarkDirty(k, func(ctx context.Context, exp int64) (int64, error) {
			mu.Lock()
			trace = append(trace, k)
			mu.Unlock()
			return exp + 1, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.FlushDirty(context.Background()); err != nil {
		t.Fatalf("FlushDirty: %v", err)
	}
	want := []AggregateKey{
		charSaverKey(1),
		charSaverKey(9),
		itemSaverKey(1),
		bankSaverKey(1, "z"),
		bankSaverKey(2, "a"),
		bankSaverKey(2, "b"),
	}
	mu.Lock()
	defer mu.Unlock()
	if len(trace) != len(want) {
		t.Fatalf("trace = %v, want %v", trace, want)
	}
	for i := range want {
		if trace[i] != want[i] {
			t.Fatalf("trace = %v, want %v", trace, want)
		}
	}
}

func TestSaverFlushContinuesAfterIndependentFailure(t *testing.T) {
	s := mustSaver(t, newManualClock())
	keyA, keyB, keyC := charSaverKey(1), charSaverKey(2), charSaverKey(3)
	for _, k := range []AggregateKey{keyA, keyB, keyC} {
		mustTrack(t, s, k, 0)
	}
	errB := errors.New("saver test B blew up")
	bCalls := 0
	if err := s.MarkDirty(keyA, casOK); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDirty(keyB, func(ctx context.Context, exp int64) (int64, error) {
		bCalls++
		return 0, errB
	}); err != nil {
		t.Fatal(err)
	}
	cRan := false
	if err := s.MarkDirty(keyC, func(ctx context.Context, exp int64) (int64, error) {
		cRan = true
		return exp + 1, nil
	}); err != nil {
		t.Fatal(err)
	}
	err := s.FlushDirty(context.Background())
	if !errors.Is(err, errB) {
		t.Fatalf("FlushDirty err = %v, want wrapped B error", err)
	}
	if !cRan || bCalls != 1 {
		t.Fatalf("cRan = %v bCalls = %d, want true/1", cRan, bCalls)
	}
	snapA, _ := s.Inspect(keyA)
	snapB, _ := s.Inspect(keyB)
	snapC, _ := s.Inspect(keyC)
	if snapA.Dirty || snapC.Dirty {
		t.Fatalf("A/C still dirty: %+v %+v", snapA, snapC)
	}
	if !snapB.Dirty {
		t.Fatalf("B clean after failure: %+v", snapB)
	}
}

// ---- B26/B27: context behavior ----

func TestSaverContextCancelledBeforeOwnership(t *testing.T) {
	s := mustSaver(t, newManualClock())
	key := charSaverKey(1)
	mustTrack(t, s, key, 0)
	entered := make(chan struct{})
	proceed := make(chan int64, 1)
	if err := s.MarkDirty(key, func(ctx context.Context, exp int64) (int64, error) {
		close(entered)
		return <-proceed, nil
	}); err != nil {
		t.Fatal(err)
	}
	flushDone := make(chan error, 1)
	go func() { flushDone <- s.FlushDirty(context.Background()) }()
	<-entered
	// Same-key WriteThrough with a dead context: its writer must
	// not run while the gate is held.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ran := false
	spy := func(ctx context.Context, exp int64) (int64, error) {
		ran = true
		return exp + 1, nil
	}
	if _, err := s.WriteThrough(ctx, key, spy); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled WriteThrough err = %v, want context.Canceled", err)
	}
	// A pending key flushed under a dead context: no writer call,
	// context error out, dirty state retained.
	key2 := charSaverKey(2)
	mustTrack(t, s, key2, 0)
	spy2Ran := false
	if err := s.MarkDirty(key2, func(ctx context.Context, exp int64) (int64, error) {
		spy2Ran = true
		return exp + 1, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushDirty(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled FlushDirty err = %v, want context.Canceled", err)
	}
	if ran || spy2Ran {
		t.Fatal("writer ran under cancelled context")
	}
	snap2, _ := s.Inspect(key2)
	if !snap2.Dirty {
		t.Fatalf("key2 clean after cancelled flush: %+v", snap2)
	}
	proceed <- 1
	if err := <-flushDone; err != nil {
		t.Fatalf("release flush: %v", err)
	}
}

func TestSaverContextCancelDuringWriter(t *testing.T) {
	s := mustSaver(t, newManualClock())
	key := charSaverKey(1)
	mustTrack(t, s, key, 0)
	entered := make(chan struct{})
	var once sync.Once
	if err := s.MarkDirty(key, func(ctx context.Context, exp int64) (int64, error) {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return 0, ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.FlushDirty(ctx) }()
	<-entered
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("flush err = %v, want context.Canceled", err)
	}
	snap, _ := s.Inspect(key)
	if !snap.Dirty || snap.Blocked {
		t.Fatalf("after cancel: snap = %+v, want dirty unblocked", snap)
	}
}

// ---- B28-B32: Run semantics ----

func TestSaverRunIntervalAndSingleTicker(t *testing.T) {
	clk := newManualClock()
	s := mustSaver(t, clk)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- s.Run(ctx) }()
	waitSaverCond(t, "ticker creation", func() bool { return clk.tickerCount() == 1 })
	periods := clk.periods()
	if len(periods) != 1 || periods[0] != 60*time.Second {
		t.Fatalf("periods = %v, want exactly [60s]", periods)
	}
	cancel()
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err = %v, want context.Canceled", err)
	}
	if got := clk.firstTicker().stopCount(); got != 1 {
		t.Fatalf("ticker stops = %d, want exactly 1", got)
	}
}

func TestSaverRunOnePulseOneFlush(t *testing.T) {
	clk := newManualClock()
	s := mustSaver(t, clk)
	key := charSaverKey(1)
	mustTrack(t, s, key, 0)
	var calls int32
	if err := s.MarkDirty(key, func(ctx context.Context, exp int64) (int64, error) {
		atomic.AddInt32(&calls, 1)
		return exp + 1, nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- s.Run(ctx) }()
	waitSaverCond(t, "ticker creation", func() bool { return clk.firstTicker() != nil })
	tk := clk.firstTicker()
	tk.pulse(time.Now())
	waitSaverCond(t, "first save", func() bool { return atomic.LoadInt32(&calls) == 1 })
	// A second pulse with no new dirt causes zero writer calls. To
	// prove the pass was consumed, queue B behind it: if the empty
	// pass re-ran A, the count would exceed 1 before B runs.
	tk.pulse(time.Now())
	keyB := charSaverKey(2)
	mustTrack(t, s, keyB, 0)
	bDone := make(chan struct{})
	if err := s.MarkDirty(keyB, func(ctx context.Context, exp int64) (int64, error) {
		close(bDone)
		return exp + 1, nil
	}); err != nil {
		t.Fatal(err)
	}
	tk.pulse(time.Now())
	select {
	case <-bDone:
	case <-time.After(10 * time.Second):
		t.Fatal("B save never ran")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("A calls = %d, want exactly 1 (empty pulse saved nothing)", got)
	}
	cancel()
	<-runDone
}

func TestSaverRunFailureContinues(t *testing.T) {
	clk := newManualClock()
	var observed []error
	var omu sync.Mutex
	s, err := NewSaver(SaverConfig{
		Interval: 60 * time.Second,
		Clock:    clk,
		OnError: func(err error) {
			omu.Lock()
			observed = append(observed, err)
			omu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	key := charSaverKey(1)
	mustTrack(t, s, key, 0)
	attempt := 0
	if err := s.MarkDirty(key, func(ctx context.Context, exp int64) (int64, error) {
		attempt++
		if attempt == 1 {
			return 0, errSaverTransient
		}
		return exp + 1, nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- s.Run(ctx) }()
	waitSaverCond(t, "ticker creation", func() bool { return clk.firstTicker() != nil })
	tk := clk.firstTicker()
	tk.pulse(time.Now())
	waitSaverCond(t, "observed transient", func() bool {
		omu.Lock()
		defer omu.Unlock()
		return len(observed) == 1
	})
	omu.Lock()
	first := observed[0]
	omu.Unlock()
	if !errors.Is(first, errSaverTransient) {
		t.Fatalf("observed = %v, want transient", first)
	}
	tk.pulse(time.Now()) // Run survives: second pulse retries successfully.
	waitSaverCond(t, "recovered clean", func() bool {
		snap, err := s.Inspect(key)
		return err == nil && snap.KnownRevision == 1 && !snap.Dirty
	})
	cancel()
	<-runDone
}

func TestSaverRunCancellationNoExtraFlush(t *testing.T) {
	clk := newManualClock()
	s := mustSaver(t, clk)
	key := charSaverKey(1)
	mustTrack(t, s, key, 0)
	var calls int32
	if err := s.MarkDirty(key, func(ctx context.Context, exp int64) (int64, error) {
		atomic.AddInt32(&calls, 1)
		return exp + 1, nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- s.Run(ctx) }()
	waitSaverCond(t, "ticker creation", func() bool { return clk.firstTicker() != nil })
	clk.firstTicker().pulse(time.Now())
	waitSaverCond(t, "save", func() bool { return atomic.LoadInt32(&calls) == 1 })
	cancel()
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err = %v, want context.Canceled", err)
	}
	if got := clk.firstTicker().stopCount(); got != 1 {
		t.Fatalf("stops = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("calls after cancel = %d, want 1 (no extra final save)", got)
	}
}

// ---- B33-B36: FlushAll + untrack ----

func TestSaverFlushAllCleanShutdown(t *testing.T) {
	s := mustSaver(t, newManualClock())
	keys := []AggregateKey{charSaverKey(1), itemSaverKey(2), bankSaverKey(3, "tos")}
	for i, k := range keys {
		mustTrack(t, s, k, int64(i))
		if err := s.MarkDirty(k, casOK); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.FlushAll(context.Background()); err != nil {
		t.Fatalf("FlushAll: %v", err)
	}
	if got := s.DirtyCount(); got != 0 {
		t.Fatalf("DirtyCount = %d, want 0", got)
	}
	for _, k := range keys {
		if err := s.Untrack(k); err != nil {
			t.Fatalf("Untrack(%v): %v", k, err)
		}
	}
	if got := s.TrackedCount(); got != 0 {
		t.Fatalf("TrackedCount = %d, want 0", got)
	}
}

func TestSaverFlushAllDeadline(t *testing.T) {
	s := mustSaver(t, newManualClock())
	keyA, keyB := charSaverKey(1), charSaverKey(2)
	mustTrack(t, s, keyA, 0)
	mustTrack(t, s, keyB, 0)
	entered := make(chan struct{})
	var once sync.Once
	if err := s.MarkDirty(keyA, func(ctx context.Context, exp int64) (int64, error) {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return 0, ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	bRan := false
	if err := s.MarkDirty(keyB, func(ctx context.Context, exp int64) (int64, error) {
		bRan = true
		return exp + 1, nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.FlushAll(ctx) }()
	<-entered
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("FlushAll err = %v, want context.Canceled", err)
	}
	if bRan {
		t.Fatal("key B ran after the deadline fired")
	}
	snap, _ := s.Inspect(keyA)
	if !snap.Dirty {
		t.Fatalf("A clean after deadline: %+v (failures stay dirty)", snap)
	}
}

func TestSaverFlushAllNoRetrySpin(t *testing.T) {
	s := mustSaver(t, newManualClock())
	key := charSaverKey(1)
	mustTrack(t, s, key, 0)
	calls := 0
	if err := s.MarkDirty(key, func(ctx context.Context, exp int64) (int64, error) {
		calls++
		return 0, errSaverTransient
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushAll(context.Background()); !errors.Is(err, errSaverTransient) {
		t.Fatalf("FlushAll err = %v, want transient", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want exactly 1 (no spin)", calls)
	}
}

// ---- B37-B39: model/property + concurrency ----

type saverModel struct {
	known   int64
	pg      int64
	pending bool
	blocked bool
}

func TestSaverStressModel(t *testing.T) {
	const seeds = 128
	const actions = 128
	kinds := []AggregateKey{
		charSaverKey(1),
		charSaverKey(2),
		itemSaverKey(7),
		bankSaverKey(3, "tos"),
	}
	for seed := 0; seed < seeds; seed++ {
		rng := randv2.New(randv2.NewPCG(uint64(seed)+1, uint64(seed)*31+7))
		s := mustSaver(t, newManualClock())
		model := make(map[AggregateKey]*saverModel)
		// outcome: 0 success, 1 transient, 2 stale, 3 invariant.
		scripts := make(map[AggregateKey][]int)
		for _, k := range kinds {
			rev := int64(rng.IntN(4))
			mustTrack(t, s, k, rev)
			model[k] = &saverModel{known: rev, pg: rev}
			var script []int
			for i := 0; i < 2*actions; i++ {
				r := rng.IntN(100)
				switch {
				case r < 70:
					script = append(script, 0)
				case r < 85:
					script = append(script, 1)
				case r < 95:
					script = append(script, 2)
				default:
					script = append(script, 3)
				}
			}
			scripts[k] = script
		}
		var testGen uint64
		committedGen := make(map[AggregateKey]uint64)
		hasCommitted := make(map[AggregateKey]bool)
		type execRec struct {
			key     AggregateKey
			gen     uint64
			exp     int64
			outcome int
			newRev  int64
		}
		var execLog []execRec
		writerFor := func(k AggregateKey) SnapshotWrite {
			testGen++
			g := testGen
			return func(ctx context.Context, exp int64) (int64, error) {
				// B38 no-stale-overwrite: a generation older than an
				// already-persisted newer generation must never run.
				if hasCommitted[k] && g < committedGen[k] {
					t.Fatalf("seed %d: key %v gen %d ran after committed gen %d",
						seed, k, g, committedGen[k])
				}
				q := scripts[k]
				outcome := 0
				if len(q) > 0 {
					outcome, scripts[k] = q[0], q[1:]
				}
				rec := execRec{key: k, gen: g, exp: exp, outcome: outcome}
				switch outcome {
				case 0:
					committedGen[k] = g
					hasCommitted[k] = true
					rec.newRev = exp + 1
					execLog = append(execLog, rec)
					return exp + 1, nil
				case 1:
					execLog = append(execLog, rec)
					return 0, errSaverTransient
				case 2:
					execLog = append(execLog, rec)
					return 0, ErrSnapshotStale
				default:
					rec.newRev = exp + 2
					execLog = append(execLog, rec)
					return exp + 2, nil
				}
			}
		}
		// replay applies one logged execution to the model copy and
		// returns the predicted post-state. viaWriteThrough selects
		// the critical-path rules; otherwise the periodic rules.
		replay := func(m saverModel, r execRec, viaWriteThrough bool) saverModel {
			if r.exp != m.known {
				t.Fatalf("seed %d: key %v executed with exp %d, model known %d",
					seed, r.key, r.exp, m.known)
			}
			switch r.outcome {
			case 0:
				if r.newRev != r.exp+1 {
					t.Fatalf("seed %d: success rev %d != exp+1", seed, r.newRev)
				}
				m.known = r.newRev
				m.pg = r.newRev
				m.pending = false
			case 1:
				m.pending = true
			case 2:
				m.blocked = true
				if !viaWriteThrough {
					m.pending = false
				}
				if m.pg <= m.known {
					m.pg = m.known + 1 // another writer advanced PG.
				}
			default:
				m.blocked = true
				if !viaWriteThrough {
					m.pending = true
				}
			}
			return m
		}
		check := func(op string) {
			t.Helper()
			for _, k := range kinds {
				snap, err := s.Inspect(k)
				if err != nil {
					t.Fatalf("seed %d op %s: inspect %v: %v", seed, op, k, err)
				}
				m := model[k]
				if snap.KnownRevision != m.known || snap.Dirty != m.pending || snap.Blocked != m.blocked {
					t.Fatalf("seed %d op %s key %v: saver %+v, model %+v",
						seed, op, k, snap, m)
				}
			}
			dirty := 0
			for _, m := range model {
				if m.pending {
					dirty++
				}
			}
			if got := s.DirtyCount(); got != dirty {
				t.Fatalf("seed %d op %s: DirtyCount = %d, model %d", seed, op, got, dirty)
			}
		}
		ctx := context.Background()
		for step := 0; step < actions; step++ {
			k := kinds[rng.IntN(len(kinds))]
			m := model[k]
			switch rng.IntN(10) {
			case 0, 1, 2, 3, 4: // MarkDirty
				err := s.MarkDirty(k, writerFor(k))
				if m.blocked {
					if !errors.Is(err, ErrSaverReconcileRequired) {
						t.Fatalf("seed %d: blocked mark err = %v", seed, err)
					}
				} else {
					if err != nil {
						t.Fatalf("seed %d: mark err = %v", seed, err)
					}
					m.pending = true
				}
			case 5, 6: // FlushDirty over all keys.
				before := make(map[AggregateKey]saverModel, len(model))
				for kk, mm := range model {
					before[kk] = *mm
				}
				execLog = nil
				_ = s.FlushDirty(ctx)
				// Replay executions in canonical key order: the
				// saver attempts each dirty unblocked key at most
				// once, so the log holds at most one rec per key.
				seen := make(map[AggregateKey]int)
				for _, r := range execLog {
					seen[r.key]++
				}
				for kk, n := range seen {
					if n != 1 {
						t.Fatalf("seed %d: key %v executed %d times in one pass", seed, kk, n)
					}
				}
				for _, r := range execLog {
					bm := before[r.key]
					if !bm.pending || bm.blocked {
						t.Fatalf("seed %d: key %v executed while clean/blocked", seed, r.key)
					}
					nm := replay(bm, r, false)
					*model[r.key] = nm
					before[r.key] = nm
				}
			case 7: // WriteThrough
				w := writerFor(k)
				pre := *m
				execLog = nil
				rev, err := s.WriteThrough(ctx, k, w)
				if m.blocked {
					if !errors.Is(err, ErrSaverReconcileRequired) {
						t.Fatalf("seed %d: blocked writethrough err = %v", seed, err)
					}
					if len(execLog) != 0 {
						t.Fatalf("seed %d: blocked writethrough executed", seed)
					}
				} else {
					if len(execLog) != 1 {
						t.Fatalf("seed %d: writethrough executions = %d", seed, len(execLog))
					}
					nm := replay(pre, execLog[0], true)
					*m = nm
					if execLog[0].outcome == 0 && (err != nil || rev != nm.known) {
						t.Fatalf("seed %d: writethrough = (%d,%v), want (%d,nil)",
							seed, rev, err, nm.known)
					}
					if execLog[0].outcome != 0 && err == nil {
						t.Fatalf("seed %d: failed writethrough returned nil", seed)
					}
				}
			default: // ResolveReconciled
				auth := m.pg
				if auth < m.known {
					auth = m.known
				}
				if rng.IntN(2) == 0 {
					auth += int64(rng.IntN(3))
				}
				if err := s.ResolveReconciled(ctx, k, auth); err != nil {
					t.Fatalf("seed %d: resolve(%d) err = %v", seed, auth, err)
				}
				m.known = auth
				m.pg = auth
				m.pending = false
				m.blocked = false
			}
			check("step")
		}
	}
}

func TestSaverConcurrentSameKeyMaxOne(t *testing.T) {
	s := mustSaver(t, newManualClock())
	key := charSaverKey(1)
	mustTrack(t, s, key, 0)
	const n = 8
	var cur, maxSeen int32
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.WriteThrough(context.Background(), key,
				func(ctx context.Context, exp int64) (int64, error) {
					c := atomic.AddInt32(&cur, 1)
					for {
						m := atomic.LoadInt32(&maxSeen)
						if c <= m || atomic.CompareAndSwapInt32(&maxSeen, m, c) {
							break
						}
					}
					for j := 0; j < 500; j++ {
					}
					atomic.AddInt32(&cur, -1)
					return exp + 1, nil
				})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d err = %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&maxSeen); got != 1 {
		t.Fatalf("max same-key concurrency = %d, want 1", got)
	}
	snap, _ := s.Inspect(key)
	if snap.KnownRevision != n || snap.Dirty {
		t.Fatalf("snap = %+v, want known%d clean", snap, n)
	}
}
