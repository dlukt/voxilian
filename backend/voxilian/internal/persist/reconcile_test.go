package persist

import (
	"context"
	"errors"
	"testing"

	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/store"
)

// fakeBankLoader stages LoadBankBalance results or errors.
type fakeBankLoader struct {
	snap  store.BankSnapshot
	err   error
	calls int
}

func (f *fakeBankLoader) LoadBankBalance(context.Context, int64, string) (store.BankSnapshot, error) {
	f.calls++
	if f.err != nil {
		return store.BankSnapshot{}, f.err
	}
	return f.snap, nil
}

var errFakeLoad = errors.New("persist test load failure")

func TestBankReloadStagedNoEarlyMutation(t *testing.T) {
	loader := &fakeBankLoader{snap: store.BankSnapshot{
		CharacterID: 4, System: "tos", ExpectedRevision: 6, Balance: 150,
	}}
	mutated := false
	var applied store.BankSnapshot
	reload := BankReload(loader, 4, "tos", func(s store.BankSnapshot) error {
		mutated = true
		applied = s
		return nil
	})
	cand, err := reload(context.Background())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if mutated {
		t.Fatal("live memory mutated during SELECT staging")
	}
	if cand.Revision != 6 {
		t.Fatalf("candidate revision = %d, want 6", cand.Revision)
	}
	if cand.Apply == nil {
		t.Fatal("nil apply")
	}
	if err := cand.Apply(); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !mutated || applied.Balance != 150 || applied.ExpectedRevision != 6 {
		t.Fatalf("applied = %+v mutated = %v", applied, mutated)
	}
}

func TestBankReloadLoaderError(t *testing.T) {
	loader := &fakeBankLoader{err: errFakeLoad}
	reload := BankReload(loader, 4, "tos", func(store.BankSnapshot) error {
		t.Error("apply must not run on loader error")
		return nil
	})
	if _, err := reload(context.Background()); !errors.Is(err, errFakeLoad) {
		t.Fatalf("err = %v, want loader cause", err)
	}
}

func mustReconcileState(t *testing.T, known int64) *sim.ReconcileState {
	t.Helper()
	r, err := sim.NewReconcileState(known)
	if err != nil {
		t.Fatalf("NewReconcileState: %v", err)
	}
	return r
}

func TestReconcileSaverSuccess(t *testing.T) {
	ctx := context.Background()
	fs := newFakeSnapshotStore()
	fs.bankRev[bankKey(4, "tos")] = 1
	loader := &fakeBankLoader{snap: store.BankSnapshot{
		CharacterID: 4, System: "tos", ExpectedRevision: 1, Balance: 150,
	}}
	state := mustReconcileState(t, 0)
	s := mustSaverForPersist(t)
	key := sim.AggregateKey{Kind: sim.AggregateBank, ID: 4, Scope: "tos"}
	if err := s.Track(key, 0); err != nil {
		t.Fatal(err)
	}
	// Stale the saver first: durable moved to 1, saver still knows 0.
	bjob, err := NewBankSnapshotJob(fs, store.BankSnapshot{CharacterID: 4, System: "tos", Balance: 120})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDirty(key, bjob.Write); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushDirty(ctx); !errors.Is(err, sim.ErrSnapshotStale) {
		t.Fatalf("flush err = %v, want stale", err)
	}
	var mem store.BankSnapshot
	haveMem := false
	if err := ReconcileSaver(ctx, state, s, key, BankReload(loader, 4, "tos",
		func(snap store.BankSnapshot) error {
			mem = snap
			haveMem = true
			return nil
		})); err != nil {
		t.Fatalf("ReconcileSaver: %v", err)
	}
	if !haveMem || mem.Balance != 150 {
		t.Fatalf("mem = %+v", mem)
	}
	rs := state.Snapshot()
	if rs.Pending || rs.KnownRevision != 1 {
		t.Fatalf("t3c = %+v, want clear known1", rs)
	}
	ss, _ := s.Inspect(key)
	if ss.Blocked || ss.Dirty || ss.KnownRevision != 1 {
		t.Fatalf("saver = %+v, want clear known1 clean", ss)
	}
}

func TestReconcileSaverLoaderFailure(t *testing.T) {
	ctx := context.Background()
	state := mustReconcileState(t, 0)
	s := mustSaverForPersist(t)
	key := sim.AggregateKey{Kind: sim.AggregateBank, ID: 4, Scope: "tos"}
	if err := s.Track(key, 0); err != nil {
		t.Fatal(err)
	}
	loader := &fakeBankLoader{err: errFakeLoad}
	applied := false
	err := ReconcileSaver(ctx, state, s, key, BankReload(loader, 4, "tos",
		func(store.BankSnapshot) error { applied = true; return nil }))
	if !errors.Is(err, errFakeLoad) {
		t.Fatalf("err = %v, want loader cause", err)
	}
	if applied {
		t.Fatal("apply ran despite loader failure")
	}
	if !state.Snapshot().Pending {
		t.Fatal("t3c not pending after loader failure")
	}
	ss, _ := s.Inspect(key)
	if ss.Blocked {
		t.Fatal("saver unexpectedly blocked (was never stale-blocked here)")
	}
}

// TestReconcileSaverResolveFailureRefences covers §8.3.13/A20: the
// T3c reload succeeds but ResolveReconciled cannot proceed (key
// untracked at the saver layer), so the bridge re-fences T3c before
// returning.
func TestReconcileSaverResolveFailureRefences(t *testing.T) {
	ctx := context.Background()
	loader := &fakeBankLoader{snap: store.BankSnapshot{
		CharacterID: 4, System: "tos", ExpectedRevision: 3, Balance: 200,
	}}
	state := mustReconcileState(t, 0)
	s := mustSaverForPersist(t)
	key := sim.AggregateKey{Kind: sim.AggregateBank, ID: 4, Scope: "tos"}
	// NOTE: key deliberately NOT tracked: ResolveReconciled fails
	// with not-tracked after a successful memory reload.
	applied := false
	err := ReconcileSaver(ctx, state, s, key, BankReload(loader, 4, "tos",
		func(store.BankSnapshot) error { applied = true; return nil }))
	if !errors.Is(err, sim.ErrAggregateNotTracked) {
		t.Fatalf("err = %v, want not-tracked resolve failure", err)
	}
	if !applied {
		t.Fatal("memory reload/apply did not run")
	}
	rs := state.Snapshot()
	if !rs.Pending {
		t.Fatalf("t3c = %+v: must be re-fenced pending after resolve failure", rs)
	}
}

// TestReconcileSaverPreservesHigherRequired proves a pre-existing
// required=7 fence survives a bridge reload that only reaches 5.
func TestReconcileSaverPreservesHigherRequired(t *testing.T) {
	ctx := context.Background()
	loader := &fakeBankLoader{snap: store.BankSnapshot{
		CharacterID: 4, System: "tos", ExpectedRevision: 5, Balance: 150,
	}}
	state := mustReconcileState(t, 5)
	if err := state.MarkCommitted(7); err != nil {
		t.Fatal(err)
	}
	s := mustSaverForPersist(t)
	key := sim.AggregateKey{Kind: sim.AggregateBank, ID: 4, Scope: "tos"}
	if err := s.Track(key, 5); err != nil {
		t.Fatal(err)
	}
	err := ReconcileSaver(ctx, state, s, key, BankReload(loader, 4, "tos",
		func(store.BankSnapshot) error { return nil }))
	// Candidate 5 < required 7: behind. Bridge fails, both pending.
	if !errors.Is(err, sim.ErrReconcileRevisionBehind) {
		t.Fatalf("err = %v, want behind", err)
	}
	rs := state.Snapshot()
	if !rs.Pending || rs.RequiredRevision != 7 {
		t.Fatalf("t3c = %+v, want pending req7", rs)
	}
}

// TestReconcileSaverCancelledContext proves cancellation aborts the
// bridge without clearing either layer.
func TestReconcileSaverCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	state := mustReconcileState(t, 0)
	s := mustSaverForPersist(t)
	key := sim.AggregateKey{Kind: sim.AggregateBank, ID: 4, Scope: "tos"}
	if err := s.Track(key, 0); err != nil {
		t.Fatal(err)
	}
	loader := &fakeBankLoader{snap: store.BankSnapshot{ExpectedRevision: 1}}
	err := ReconcileSaver(ctx, state, s, key, BankReload(loader, 4, "tos",
		func(store.BankSnapshot) error { return nil }))
	if err == nil {
		t.Fatal("cancelled bridge returned nil")
	}
	if !state.Snapshot().Pending {
		t.Fatal("t3c cleared under cancelled context")
	}
}
