package sim

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dlukt/voxilian/internal/world"
)

// synthOwner is the test-only durable owner: gameplay value plus
// its reconciliation fence. No production entity carries this;
// tests associate owners with EntityIDs where routing matters.
type synthOwner struct {
	value       int64
	state       *ReconcileState
	noticeCalls int
	mutateCalls int
}

func mustOwner(t *testing.T, knownRevision, value int64) *synthOwner {
	t.Helper()
	st, err := NewReconcileState(knownRevision)
	if err != nil {
		t.Fatalf("NewReconcileState(%d): %v", knownRevision, err)
	}
	return &synthOwner{value: value, state: st}
}

// noticeTo builds an exact-next notice apply that installs value.
func (o *synthOwner) noticeTo(value int64) func() error {
	return func() error {
		o.noticeCalls++
		o.value = value
		return nil
	}
}

// mutate runs the mandatory gate: reconcile first, then callback.
func (o *synthOwner) mutate(ctx context.Context, reload ReloadFunc, fn func()) error {
	if err := o.state.EnsureReconciled(ctx, reload); err != nil {
		return err
	}
	o.mutateCalls++
	fn()
	return nil
}

func (o *synthOwner) snapshot() (ReconcileSnapshot, int64) {
	return o.state.Snapshot(), o.value
}

func TestReconcileStateInit(t *testing.T) {
	st, err := NewReconcileState(5)
	if err != nil {
		t.Fatalf("New(5): %v", err)
	}
	if got := st.Snapshot(); got != (ReconcileSnapshot{KnownRevision: 5, RequiredRevision: 5}) {
		t.Fatalf("init = %+v, want known5/req5/clear", got)
	}
	if _, err := NewReconcileState(0); err != nil {
		t.Fatalf("New(0): %v", err)
	}
	if _, err := NewReconcileState(-1); !errors.Is(err, ErrInvalidDurableRevision) {
		t.Fatalf("New(-1) = %v, want ErrInvalidDurableRevision", err)
	}
}

func TestReconcileMarkCommitted(t *testing.T) {
	st, err := NewReconcileState(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkCommitted(1); err != nil {
		t.Fatalf("mark1: %v", err)
	}
	if got := st.Snapshot(); got.Pending != true || got.RequiredRevision != 1 || got.KnownRevision != 0 {
		t.Fatalf("after mark1 = %+v", got)
	}
	before := st.Snapshot()
	if err := st.MarkCommitted(1); err != nil {
		t.Fatalf("re-mark1: %v", err)
	}
	if got := st.Snapshot(); got != before {
		t.Fatalf("re-mark changed state: %+v vs %+v", got, before)
	}
	if err := st.MarkCommitted(0); err != nil {
		t.Fatalf("mark0: %v", err)
	}
	if got := st.Snapshot(); got != before {
		t.Fatalf("stale mark changed state: %+v", got)
	}
	if err := st.MarkCommitted(2); err != nil {
		t.Fatalf("mark2: %v", err)
	}
	if got := st.Snapshot(); !got.Pending || got.RequiredRevision != 2 {
		t.Fatalf("after mark2 = %+v", got)
	}
	if err := st.MarkCommitted(-1); !errors.Is(err, ErrInvalidDurableRevision) {
		t.Fatalf("negative = %v, want ErrInvalidDurableRevision", err)
	}
	if got := st.Snapshot(); got.RequiredRevision != 2 || got.KnownRevision != 0 {
		t.Fatalf("negative mutated: %+v", got)
	}
}

func TestReconcileMultipleMarks(t *testing.T) {
	o := mustOwner(t, 5, 50)
	if err := o.state.MarkCommitted(6); err != nil {
		t.Fatal(err)
	}
	if err := o.state.MarkCommitted(7); err != nil {
		t.Fatal(err)
	}
	snap, value := o.snapshot()
	if !snap.Pending || snap.RequiredRevision != 7 || snap.KnownRevision != 5 || value != 50 {
		t.Fatalf("marks = %+v val %d, want pending/req7/known5/val50", snap, value)
	}
	// Never lower the requirement.
	if err := o.state.MarkCommitted(6); err != nil {
		t.Fatal(err)
	}
	if got := o.state.Snapshot(); got.RequiredRevision != 7 {
		t.Fatalf("requirement lowered: %+v", got)
	}
}

func TestReconcileNoticeInvalidOpID(t *testing.T) {
	o := mustOwner(t, 0, 100)
	before := o.state.Snapshot()
	d, err := o.state.ApplyCommitNotice(DurableCommitNotice{ID: OpID(0), Revision: 1}, o.noticeTo(110))
	if !errors.Is(err, ErrInvalidOpID) {
		t.Fatalf("op 0 = %v,%v, want ErrInvalidOpID", d, err)
	}
	if o.noticeCalls != 0 || o.value != 100 {
		t.Fatal("invalid op applied payload")
	}
	if got := o.state.Snapshot(); got != before {
		t.Fatalf("invalid op mutated fence: %+v", got)
	}
}

func TestReconcileNoticeSequential(t *testing.T) {
	o := mustOwner(t, 0, 100)
	if err := o.state.MarkCommitted(1); err != nil {
		t.Fatal(err)
	}
	if err := o.state.MarkCommitted(2); err != nil {
		t.Fatal(err)
	}
	d, err := o.state.ApplyCommitNotice(DurableCommitNotice{ID: OpID(11), Revision: 1}, o.noticeTo(110))
	if err != nil || d != CommitNoticeApplied {
		t.Fatalf("notice1 = %v,%v", d, err)
	}
	if snap, v := o.snapshot(); !snap.Pending || snap.KnownRevision != 1 || snap.RequiredRevision != 2 || v != 110 {
		t.Fatalf("after notice1 = %+v val %d", snap, v)
	}
	d, err = o.state.ApplyCommitNotice(DurableCommitNotice{ID: OpID(12), Revision: 2}, o.noticeTo(120))
	if err != nil || d != CommitNoticeApplied {
		t.Fatalf("notice2 = %v,%v", d, err)
	}
	if snap, v := o.snapshot(); snap.Pending || snap.KnownRevision != 2 || v != 120 {
		t.Fatalf("after notice2 = %+v val %d, want clear/known2/120", snap, v)
	}
	if o.noticeCalls != 2 {
		t.Fatalf("callbacks = %d, want 2 (no PG reload)", o.noticeCalls)
	}
}

func TestReconcileNoticeStale(t *testing.T) {
	o := mustOwner(t, 6, 160)
	for _, rev := range []int64{6, 5} {
		d, err := o.state.ApplyCommitNotice(DurableCommitNotice{ID: OpID(21), Revision: rev}, o.noticeTo(999))
		if err != nil || d != CommitNoticeStale {
			t.Fatalf("notice %d = %v,%v, want stale", rev, d, err)
		}
	}
	if o.noticeCalls != 0 || o.value != 160 {
		t.Fatal("stale notice mutated payload")
	}
	if got := o.state.Snapshot(); got.KnownRevision != 6 || got.Pending {
		t.Fatalf("stale moved fence: %+v", got)
	}
}

func TestReconcileNoticeGap(t *testing.T) {
	o := mustOwner(t, 5, 150)
	d, err := o.state.ApplyCommitNotice(DurableCommitNotice{ID: OpID(31), Revision: 7}, o.noticeTo(170))
	if !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("gap = %v,%v, want ErrReconcileRequired", d, err)
	}
	if o.noticeCalls != 0 || o.value != 150 {
		t.Fatal("gap applied payload")
	}
	if got := o.state.Snapshot(); !got.Pending || got.RequiredRevision < 7 || got.KnownRevision != 5 {
		t.Fatalf("gap fence = %+v, want pending/req>=7/known5", got)
	}
}

func TestReconcileNoticeApplyFailure(t *testing.T) {
	o := mustOwner(t, 5, 150)
	calls := 0
	fail := func() error {
		calls++
		return errSyntheticApply
	}
	_, err := o.state.ApplyCommitNotice(DurableCommitNotice{ID: OpID(41), Revision: 6}, fail)
	if !errors.Is(err, errSyntheticApply) {
		t.Fatalf("failure = %v, want sentinel", err)
	}
	if got := o.state.Snapshot(); got.KnownRevision != 5 || !got.Pending || got.RequiredRevision < 6 {
		t.Fatalf("after failure = %+v", got)
	}
	if o.value != 150 {
		t.Fatal("failed notice mutated value")
	}
	// Same notice may invoke apply again.
	d, err := o.state.ApplyCommitNotice(DurableCommitNotice{ID: OpID(41), Revision: 6}, o.noticeTo(160))
	if err != nil || d != CommitNoticeApplied {
		t.Fatalf("retry = %v,%v", d, err)
	}
	if calls != 1 || o.noticeCalls != 1 || o.value != 160 {
		t.Fatalf("retry state calls=%d notice=%d val=%d", calls, o.noticeCalls, o.value)
	}
}

func TestReconcileLateNoticeAfterReload(t *testing.T) {
	ctx := context.Background()
	o := mustOwner(t, 0, 100)
	if err := o.state.MarkCommitted(1); err != nil {
		t.Fatal(err)
	}
	// Notice dropped; reload brings memory to rev1.
	stagedVal, stagedRev := int64(110), int64(1)
	if err := o.state.EnsureReconciled(ctx, func(context.Context) (ReloadCandidate, error) {
		return ReloadCandidate{Revision: stagedRev, Apply: func() error {
			o.value = stagedVal
			return nil
		}}, nil
	}); err != nil {
		t.Fatalf("reload: %v", err)
	}
	// The late rev-1 notice is a stale no-op: no second mutation.
	d, err := o.state.ApplyCommitNotice(DurableCommitNotice{ID: OpID(51), Revision: 1}, o.noticeTo(999))
	if err != nil || d != CommitNoticeStale {
		t.Fatalf("late notice = %v,%v, want stale", d, err)
	}
	if o.noticeCalls != 0 || o.value != 110 {
		t.Fatalf("late notice mutated: calls=%d val=%d", o.noticeCalls, o.value)
	}
}

func TestReconcileNewerReloadLateNotice(t *testing.T) {
	ctx := context.Background()
	o := mustOwner(t, 5, 150)
	if err := o.state.MarkCommitted(6); err != nil {
		t.Fatal(err)
	}
	// PG is already newer: reload leaps to 8 and clears.
	if err := o.state.EnsureReconciled(ctx, func(context.Context) (ReloadCandidate, error) {
		return ReloadCandidate{Revision: 8, Apply: func() error {
			o.value = 180
			return nil
		}}, nil
	}); err != nil {
		t.Fatalf("reload: %v", err)
	}
	d, err := o.state.ApplyCommitNotice(DurableCommitNotice{ID: OpID(52), Revision: 6}, o.noticeTo(999))
	if err != nil || d != CommitNoticeStale {
		t.Fatalf("late notice after newer reload = %v,%v, want stale", d, err)
	}
	if snap, v := o.snapshot(); snap.KnownRevision != 8 || v != 180 {
		t.Fatalf("rolled back: %+v val %d", snap, v)
	}
}

func TestEnsureReconciledFastPath(t *testing.T) {
	o := mustOwner(t, 4, 140)
	calls := 0
	loader := func(context.Context) (ReloadCandidate, error) {
		calls++
		return ReloadCandidate{}, nil
	}
	if err := o.state.EnsureReconciled(context.Background(), loader); err != nil {
		t.Fatalf("fast path: %v", err)
	}
	if calls != 0 {
		t.Fatal("healthy hot path performed a PG read")
	}
	// A nil loader is equally fine while clear.
	if err := o.state.EnsureReconciled(context.Background(), nil); err != nil {
		t.Fatalf("nil-loader fast path: %v", err)
	}
}

func TestEnsureReconciledLoadError(t *testing.T) {
	o := mustOwner(t, 0, 100)
	if err := o.state.MarkCommitted(1); err != nil {
		t.Fatal(err)
	}
	before := o.state.Snapshot()
	err := o.state.EnsureReconciled(context.Background(), func(context.Context) (ReloadCandidate, error) {
		return ReloadCandidate{}, errSyntheticApply
	})
	if !errors.Is(err, errSyntheticApply) {
		t.Fatalf("load error = %v, want sentinel", err)
	}
	if got := o.state.Snapshot(); got != before {
		t.Fatalf("load error mutated fence: %+v", got)
	}
	// A healthy loader may succeed on the next attempt.
	if err := o.state.EnsureReconciled(context.Background(), func(context.Context) (ReloadCandidate, error) {
		return ReloadCandidate{Revision: 1, Apply: o.noticeTo(110)}, nil
	}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if snap, v := o.snapshot(); snap.KnownRevision != 1 || v != 110 || snap.Pending {
		t.Fatalf("after retry = %+v val %d", snap, v)
	}
}

func TestEnsureReconciledBehind(t *testing.T) {
	o := mustOwner(t, 1, 110)
	if err := o.state.MarkCommitted(3); err != nil {
		t.Fatal(err)
	}
	applyCalls := 0
	err := o.state.EnsureReconciled(context.Background(), func(context.Context) (ReloadCandidate, error) {
		return ReloadCandidate{Revision: 2, Apply: func() error {
			applyCalls++
			return nil
		}}, nil
	})
	if !errors.Is(err, ErrReconcileRevisionBehind) {
		t.Fatalf("behind = %v, want ErrReconcileRevisionBehind", err)
	}
	if applyCalls != 0 {
		t.Fatal("behind candidate applied")
	}
	if got := o.state.Snapshot(); !got.Pending || got.KnownRevision != 1 {
		t.Fatalf("behind moved fence: %+v", got)
	}
}

func TestEnsureReconciledRegression(t *testing.T) {
	o := mustOwner(t, 5, 150)
	if err := o.state.MarkCommitted(6); err != nil {
		t.Fatal(err)
	}
	applyCalls := 0
	err := o.state.EnsureReconciled(context.Background(), func(context.Context) (ReloadCandidate, error) {
		return ReloadCandidate{Revision: 4, Apply: func() error {
			applyCalls++
			return nil
		}}, nil
	})
	if !errors.Is(err, ErrReconcileRevisionRegression) {
		t.Fatalf("regression = %v, want ErrReconcileRevisionRegression", err)
	}
	if applyCalls != 0 {
		t.Fatal("regressing candidate applied")
	}
	if got := o.state.Snapshot(); got.KnownRevision != 5 {
		t.Fatalf("revision moved backward: %+v", got)
	}
}

func TestEnsureReconciledExact(t *testing.T) {
	o := mustOwner(t, 1, 110)
	if err := o.state.MarkCommitted(2); err != nil {
		t.Fatal(err)
	}
	applyCalls := 0
	err := o.state.EnsureReconciled(context.Background(), func(context.Context) (ReloadCandidate, error) {
		return ReloadCandidate{Revision: 2, Apply: func() error {
			applyCalls++
			o.value = 120
			return nil
		}}, nil
	})
	if err != nil {
		t.Fatalf("exact: %v", err)
	}
	if applyCalls != 1 {
		t.Fatalf("apply calls = %d, want 1", applyCalls)
	}
	if snap, v := o.snapshot(); snap.KnownRevision != 2 || snap.Pending || v != 120 {
		t.Fatalf("after exact = %+v val %d", snap, v)
	}
}

func TestEnsureReconciledLeapForward(t *testing.T) {
	o := mustOwner(t, 1, 110)
	if err := o.state.MarkCommitted(2); err != nil {
		t.Fatal(err)
	}
	applyCalls := 0
	err := o.state.EnsureReconciled(context.Background(), func(context.Context) (ReloadCandidate, error) {
		return ReloadCandidate{Revision: 5, Apply: func() error {
			applyCalls++
			o.value = 150
			return nil
		}}, nil
	})
	if err != nil {
		t.Fatalf("leap: %v", err)
	}
	if applyCalls != 1 {
		t.Fatalf("apply calls = %d, want 1", applyCalls)
	}
	if snap, v := o.snapshot(); snap.KnownRevision != 5 || snap.Pending || v != 150 {
		t.Fatalf("after leap = %+v val %d, want known5/clear/150", snap, v)
	}
}

func TestEnsureReconciledApplyFailure(t *testing.T) {
	o := mustOwner(t, 1, 110)
	if err := o.state.MarkCommitted(2); err != nil {
		t.Fatal(err)
	}
	err := o.state.EnsureReconciled(context.Background(), func(context.Context) (ReloadCandidate, error) {
		return ReloadCandidate{Revision: 2, Apply: func() error {
			return errSyntheticApply
		}}, nil
	})
	if !errors.Is(err, errSyntheticApply) {
		t.Fatalf("apply failure = %v, want sentinel", err)
	}
	if got := o.state.Snapshot(); got.KnownRevision != 1 || !got.Pending {
		t.Fatalf("apply failure moved fence: %+v", got)
	}
	if o.value != 110 {
		t.Fatal("failed candidate mutated value")
	}
}

func TestReconcileMutationGate(t *testing.T) {
	ctx := context.Background()
	o := mustOwner(t, 0, 200)
	if err := o.state.MarkCommitted(1); err != nil {
		t.Fatal(err)
	}
	// Blocked while the loader fails: mutation never runs.
	if err := o.mutate(ctx, func(context.Context) (ReloadCandidate, error) {
		return ReloadCandidate{}, errSyntheticApply
	}, func() {
		t.Fatal("mutation ran before reconciliation")
	}); !errors.Is(err, errSyntheticApply) {
		t.Fatalf("blocked mutate = %v", err)
	}
	if o.mutateCalls != 0 {
		t.Fatal("mutation callback ran while pending")
	}
	// After a healthy reload the callback observes RELOADED state.
	var sawValue, sawKnown int64
	err := o.mutate(ctx, func(context.Context) (ReloadCandidate, error) {
		return ReloadCandidate{Revision: 1, Apply: func() error {
			o.value = 180
			return nil
		}}, nil
	}, func() {
		sawValue = o.value
		sawKnown = o.state.Snapshot().KnownRevision
		o.value += 5
	})
	if err != nil {
		t.Fatalf("mutate: %v", err)
	}
	if sawValue != 180 || sawKnown != 1 {
		t.Fatalf("callback saw value=%d known=%d, want 180/1 (stale 200 mutated)", sawValue, sawKnown)
	}
	if o.value != 185 || o.mutateCalls != 1 {
		t.Fatalf("after mutate val=%d calls=%d, want 185/1", o.value, o.mutateCalls)
	}
}

// TestReconcileTwoOwnerProof models one durable operation over
// two synthetic owners: both fences install before any notice,
// A delivers, B drops, and B's next mutation reloads first.
func TestReconcileTwoOwnerProof(t *testing.T) {
	ctx := context.Background()
	a := mustOwner(t, 0, 100)
	b := mustOwner(t, 0, 200)
	// Fences for BOTH install before ANY notification.
	if err := a.state.MarkCommitted(1); err != nil {
		t.Fatal(err)
	}
	if err := b.state.MarkCommitted(1); err != nil {
		t.Fatal(err)
	}
	d, err := a.state.ApplyCommitNotice(DurableCommitNotice{ID: OpID(61), Revision: 1}, a.noticeTo(110))
	if err != nil || d != CommitNoticeApplied {
		t.Fatalf("A notice = %v,%v", d, err)
	}
	// B notice dropped: B stays fenced on stale memory.
	if sa, va := a.snapshot(); sa.Pending || sa.KnownRevision != 1 || va != 110 {
		t.Fatalf("A = %+v val %d, want current 110/k1", sa, va)
	}
	if sb, vb := b.snapshot(); !sb.Pending || sb.RequiredRevision != 1 || sb.KnownRevision != 0 || vb != 200 {
		t.Fatalf("B = %+v val %d, want pending/req1/known0/200", sb, vb)
	}
	// B's next mutation reloads the committed PG state first:
	// the callback must see 180/rev1, never stale 200/rev0.
	var saw int64
	if err := b.mutate(ctx, func(context.Context) (ReloadCandidate, error) {
		staged, rev := int64(180), int64(1)
		return ReloadCandidate{Revision: rev, Apply: func() error {
			b.value = staged
			return nil
		}}, nil
	}, func() {
		saw = b.value
		b.value += 5
	}); err != nil {
		t.Fatalf("B mutate: %v", err)
	}
	if saw != 180 {
		t.Fatalf("B callback saw %d, want committed 180 (stale 200 + 5 = 205 forbidden)", saw)
	}
	if sb, vb := b.snapshot(); sb.Pending || sb.KnownRevision != 1 || vb != 185 {
		t.Fatalf("B after = %+v val %d, want clear/k1/185", sb, vb)
	}
}

func TestReconcileMissingMiddle(t *testing.T) {
	o := mustOwner(t, 0, 100)
	if err := o.state.MarkCommitted(2); err != nil {
		t.Fatal(err)
	}
	_, err := o.state.ApplyCommitNotice(DurableCommitNotice{ID: OpID(62), Revision: 2}, o.noticeTo(120))
	if !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("missing middle = %v, want gap", err)
	}
	if o.noticeCalls != 0 || o.value != 100 {
		t.Fatal("gapped notice applied")
	}
	// A rev-2 reload resolves without replaying the notice.
	if err := o.state.EnsureReconciled(context.Background(), func(context.Context) (ReloadCandidate, error) {
		return ReloadCandidate{Revision: 2, Apply: o.noticeTo(120)}, nil
	}); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if snap, v := o.snapshot(); snap.KnownRevision != 2 || snap.Pending || v != 120 {
		t.Fatalf("after reload = %+v val %d", snap, v)
	}
}

// t3bNoticeEngine builds one engine with one entity per durable
// owner so commit notices can ride real CrossCellOps.
type t3bNoticeEngine struct {
	e   *Engine
	g   *OpIDGenerator
	ids map[*synthOwner]EntityID
}

func mustT3bNoticeEngine(t *testing.T, owners ...*synthOwner) *t3bNoticeEngine {
	t.Helper()
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	clk := &scriptClock{ms: OpIDEpochUnixMillis + 40000}
	w := &t3bNoticeEngine{e: e, g: mustOpIDGen(t, 1, clk), ids: make(map[*synthOwner]EntityID)}
	for i, o := range owners {
		snap, err := e.AddEntity(world.Vec3{X: 16 + float64(i*64), Z: 16})
		if err != nil {
			t.Fatalf("add: %v", err)
		}
		w.ids[o] = snap.ID
	}
	return w
}

// deliverNotice routes one DurableCommitNotice through T3b's
// deliverCrossCellOp under the SAME OpID, returning both
// dispositions.
func (w *t3bNoticeEngine) deliverNotice(t *testing.T, o *synthOwner, opID OpID, rev, to int64) (CrossCellDisposition, CommitNoticeDisposition, error) {
	t.Helper()
	id := w.ids[o]
	snap, err := w.e.Entity(id)
	if err != nil {
		t.Fatalf("owner snapshot: %v", err)
	}
	op := CrossCellOp{ID: opID, Target: id, TargetOwner: OwnerRef{Cell: snap.Cell, Generation: snap.OwnershipGeneration}}
	var nd CommitNoticeDisposition
	d, err := w.e.deliverCrossCellOp(op, func(*entity) error {
		var nerr error
		nd, nerr = o.state.ApplyCommitNotice(DurableCommitNotice{ID: opID, Revision: rev}, o.noticeTo(to))
		return nerr
	})
	return d, nd, err
}

func TestReconcileT3bLateNotice(t *testing.T) {
	a := mustOwner(t, 0, 100)
	w := mustT3bNoticeEngine(t, a)
	opID := mustNext(t, w.g)
	if err := a.state.MarkCommitted(1); err != nil {
		t.Fatal(err)
	}
	// Notice dropped; reload brings memory to rev1 (value 110).
	if err := a.state.EnsureReconciled(context.Background(), func(context.Context) (ReloadCandidate, error) {
		return ReloadCandidate{Revision: 1, Apply: a.noticeTo(110)}, nil
	}); err != nil {
		t.Fatalf("reload: %v", err)
	}
	// First late delivery of SAME OpID: T3c no-op, T3b Applied
	// and recorded.
	d, nd, err := w.deliverNotice(t, a, opID, 1, 999)
	if err != nil || d != CrossCellApplied || nd != CommitNoticeStale {
		t.Fatalf("late = t3b %v t3c %v err %v, want applied/stale", d, nd, err)
	}
	if a.value != 110 || a.noticeCalls != 1 {
		t.Fatalf("late notice mutated durable state: val=%d calls=%d", a.value, a.noticeCalls)
	}
	// Second redelivery is a T3b Duplicate; callback not entered.
	calls := a.noticeCalls
	d, _, err = w.deliverNotice(t, a, opID, 1, 999)
	if err != nil || d != CrossCellDuplicate {
		t.Fatalf("redelivery = %v,%v, want duplicate", d, err)
	}
	if a.noticeCalls != calls || a.value != 110 {
		t.Fatal("duplicate re-entered reconciliation")
	}
}

func TestReconcileT3bGap(t *testing.T) {
	b := mustOwner(t, 0, 200)
	w := mustT3bNoticeEngine(t, b)
	opID := mustNext(t, w.g)
	if err := b.state.MarkCommitted(2); err != nil {
		t.Fatal(err)
	}
	// Gap callback errors: T3b MUST NOT cache the OpID.
	_, _, err := w.deliverNotice(t, b, opID, 2, 220)
	if !errors.Is(err, ErrReconcileRequired) {
		t.Fatalf("gap delivery = %v, want ErrReconcileRequired via T3b", err)
	}
	if b.noticeCalls != 0 || b.value != 200 {
		t.Fatal("gapped notice applied")
	}
	// Reload rev2, then retry SAME OpID: stale no-op, T3b records.
	if err := b.state.EnsureReconciled(context.Background(), func(context.Context) (ReloadCandidate, error) {
		return ReloadCandidate{Revision: 2, Apply: b.noticeTo(220)}, nil
	}); err != nil {
		t.Fatalf("reload: %v", err)
	}
	d, nd, err := w.deliverNotice(t, b, opID, 2, 999)
	if err != nil || d != CrossCellApplied || nd != CommitNoticeStale {
		t.Fatalf("retry = t3b %v t3c %v err %v, want applied/stale", d, nd, err)
	}
	if b.value != 220 {
		t.Fatalf("value = %d, want reloaded 220 (not 999)", b.value)
	}
	d, _, err = w.deliverNotice(t, b, opID, 2, 999)
	if err != nil || d != CrossCellDuplicate {
		t.Fatalf("third = %v,%v, want duplicate", d, err)
	}
}

func TestReconcileT3bStaleRouteUnchanged(t *testing.T) {
	a := mustOwner(t, 0, 100)
	w := mustT3bNoticeEngine(t, a)
	if err := a.state.MarkCommitted(1); err != nil {
		t.Fatal(err)
	}
	id := w.ids[a]
	bad := CrossCellOp{ID: mustNext(t, w.g), Target: id,
		TargetOwner: OwnerRef{Cell: world.CellCoord{X: 9, Z: 9}, Generation: 1}}
	entered := false
	_, err := w.e.deliverCrossCellOp(bad, func(*entity) error {
		entered = true
		_, nerr := a.state.ApplyCommitNotice(DurableCommitNotice{ID: bad.ID, Revision: 1}, a.noticeTo(110))
		return nerr
	})
	if !errors.Is(err, ErrCrossCellStaleRoute) {
		t.Fatalf("wrong owner = %v, want ErrCrossCellStaleRoute", err)
	}
	if entered || a.noticeCalls != 0 {
		t.Fatal("stale route entered reconciliation")
	}
	// Refresh and retry the same OpID: T3b/T3c operate normally.
	snap, _ := w.e.Entity(id)
	bad.TargetOwner = OwnerRef{Cell: snap.Cell, Generation: snap.OwnershipGeneration}
	d, err := w.e.deliverCrossCellOp(bad, func(*entity) error {
		_, nerr := a.state.ApplyCommitNotice(DurableCommitNotice{ID: bad.ID, Revision: 1}, a.noticeTo(110))
		return nerr
	})
	if err != nil || d != CrossCellApplied {
		t.Fatalf("refreshed = %v,%v, want applied", d, err)
	}
	if a.value != 110 {
		t.Fatalf("value = %d, want 110", a.value)
	}
}

func TestReconcileT3bMigratingUnchanged(t *testing.T) {
	a := mustOwner(t, 0, 100)
	w := mustT3bNoticeEngine(t, a)
	if err := a.state.MarkCommitted(1); err != nil {
		t.Fatal(err)
	}
	id := w.ids[a]
	holdMigration(t, w.e.registry, id, world.CellCoord{X: 2, Z: 0}, world.Vec3{X: 80.075, Z: 16})
	snap, _ := w.e.Entity(id)
	op := CrossCellOp{ID: mustNext(t, w.g), Target: id,
		TargetOwner: OwnerRef{Cell: snap.Cell, Generation: snap.OwnershipGeneration}}
	entered := false
	_, err := w.e.deliverCrossCellOp(op, func(*entity) error {
		entered = true
		return nil
	})
	if !errors.Is(err, ErrCrossCellTargetMigrating) {
		t.Fatalf("migrating = %v, want ErrCrossCellTargetMigrating", err)
	}
	if entered || a.noticeCalls != 0 {
		t.Fatal("T3c bypassed the migrating gate into quiesced state")
	}
}

func TestReconcileRecentOpsIndependent(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	clk := &scriptClock{ms: OpIDEpochUnixMillis + 41000}
	g := mustOpIDGen(t, 1, clk)
	snap, err := e.AddEntity(world.Vec3{X: 16, Z: 16})
	if err != nil {
		t.Fatal(err)
	}
	owner := OwnerRef{Cell: snap.Cell, Generation: snap.OwnershipGeneration}
	op := CrossCellOp{ID: mustNext(t, g), Target: snap.ID, TargetOwner: owner}
	if _, err := e.deliverCrossCellOp(op, func(*entity) error { return nil }); err != nil {
		t.Fatalf("t3b apply: %v", err)
	}
	// Mark reconciliation pending and reload durable synthetic
	// state: the T3b cache must be unaffected.
	st, err := NewReconcileState(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkCommitted(1); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureReconciled(context.Background(), func(context.Context) (ReloadCandidate, error) {
		return ReloadCandidate{Revision: 1, Apply: func() error { return nil }}, nil
	}); err != nil {
		t.Fatalf("reload: %v", err)
	}
	ent, _ := e.registry.lookup(snap.ID)
	before := ent.recentOps.length()
	d, err := e.deliverCrossCellOp(op, func(*entity) error { return nil })
	if err != nil || d != CrossCellDuplicate {
		t.Fatalf("redelivery = %v,%v, want duplicate", d, err)
	}
	if ent.recentOps.length() != before || !ent.recentOps.contains(op.ID) {
		t.Fatal("reload disturbed recentOpIDs")
	}
}

func TestReconcileRevisionDomains(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	snap, err := e.AddEntity(world.Vec3{X: 16, Z: 16})
	if err != nil {
		t.Fatal(err)
	}
	// Ownership generation advances to 9 while persisted
	// revision logic lives entirely in its own domain.
	ent, _ := e.registry.lookup(snap.ID)
	ent.generation = 9
	o := mustOwner(t, 2, 120)
	clk := &scriptClock{ms: OpIDEpochUnixMillis + 42000}
	g := mustOpIDGen(t, 1, clk)
	op := CrossCellOp{ID: mustNext(t, g), Target: snap.ID,
		TargetOwner: OwnerRef{Cell: snap.Cell, Generation: 9}}
	ndCalls := 0
	d, err := e.deliverCrossCellOp(op, func(*entity) error {
		var nerr error
		var nd CommitNoticeDisposition
		nd, nerr = o.state.ApplyCommitNotice(DurableCommitNotice{ID: op.ID, Revision: 3}, func() error {
			ndCalls++
			o.value = 130
			return nil
		})
		_ = nd
		return nerr
	})
	if err != nil || d != CrossCellApplied {
		t.Fatalf("gen-9 delivery = %v,%v", d, err)
	}
	if o.value != 130 || o.state.Snapshot().KnownRevision != 3 {
		t.Fatal("revision-3 notice misgoverned beside generation 9")
	}
	// And a revision-9 fence is a revision fact, not a
	// generation echo: from known3 it fences req9.
	if err := o.state.MarkCommitted(9); err != nil {
		t.Fatal(err)
	}
	if got := o.state.Snapshot(); !got.Pending || got.RequiredRevision != 9 || got.KnownRevision != 3 {
		t.Fatalf("mark9 = %+v", got)
	}
}

func TestReconcileHandoffPendingSurvives(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	snap, err := e.AddEntity(world.Vec3{X: 16, Z: 16})
	if err != nil {
		t.Fatal(err)
	}
	// Synthetic association by EntityID: routing change must not
	// reconcile durable data.
	fences := map[EntityID]*ReconcileState{}
	st, err := NewReconcileState(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkCommitted(2); err != nil {
		t.Fatal(err)
	}
	fences[snap.ID] = st
	tok := holdMigration(t, e.registry, snap.ID, world.CellCoord{X: 1, Z: 0}, world.Vec3{X: 32.075, Z: 16})
	if _, err := e.registry.commitHandoff(tok); err != nil {
		t.Fatalf("commit: %v", err)
	}
	got, ok := fences[snap.ID]
	if !ok {
		t.Fatal("fence association lost across handoff")
	}
	if snapAfter, _ := e.Entity(snap.ID); snapAfter.OwnershipGeneration != 2 {
		t.Fatalf("generation = %d, want 2", snapAfter.OwnershipGeneration)
	}
	if s := got.Snapshot(); !s.Pending || s.KnownRevision != 0 || s.RequiredRevision != 2 {
		t.Fatalf("pending fence cleared by routing change: %+v", s)
	}
	requireRegistryInvariants(t, e.registry)
}

func TestReconcileDeterministicTrace(t *testing.T) {
	run := func() string {
		var sb strings.Builder
		ctx := context.Background()
		e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
		clk := &scriptClock{ms: OpIDEpochUnixMillis + 43000}
		g := mustOpIDGen(t, 1, clk)
		mk := func(x float64, known, val int64) (*synthOwner, EntityID) {
			o := mustOwner(t, known, val)
			snap, err := e.AddEntity(world.Vec3{X: x, Z: 16})
			if err != nil {
				t.Fatalf("add: %v", err)
			}
			return o, snap.ID
		}
		a, idA := mk(16, 0, 100)
		b, idB := mk(80, 0, 200)
		ownerOf := func(id EntityID) OwnerRef {
			snap, err := e.Entity(id)
			if err != nil {
				t.Fatalf("owner: %v", err)
			}
			return OwnerRef{Cell: snap.Cell, Generation: snap.OwnershipGeneration}
		}
		notice := func(o *synthOwner, id EntityID, opID OpID, rev, to int64) (CrossCellDisposition, CommitNoticeDisposition, error) {
			var nd CommitNoticeDisposition
			d, err := e.deliverCrossCellOp(CrossCellOp{ID: opID, Target: id, TargetOwner: ownerOf(id)},
				func(*entity) error {
					var nerr error
					nd, nerr = o.state.ApplyCommitNotice(DurableCommitNotice{ID: opID, Revision: rev}, o.noticeTo(to))
					return nerr
				})
			return d, nd, err
		}
		fence := func(o *synthOwner, rev int64) {
			if err := o.state.MarkCommitted(rev); err != nil {
				t.Fatalf("fence %d: %v", rev, err)
			}
		}
		trace := func(tag string, td CrossCellDisposition, terr error, nd CommitNoticeDisposition, nerr error) {
			sa, va := a.snapshot()
			sbb, vb := b.snapshot()
			fmt.Fprintf(&sb, "%s t3b:%s/%v t3c:%s/%v A:{k%d r%d p%v v%d} B:{k%d r%d p%v v%d} nc:%d/%d\n",
				tag, td, terr == nil, nd, nerr == nil,
				sa.KnownRevision, sa.RequiredRevision, sa.Pending, va,
				sbb.KnownRevision, sbb.RequiredRevision, sbb.Pending, vb,
				a.noticeCalls, b.noticeCalls)
		}
		// Commit 1, fence both before any notice.
		fence(a, 1)
		fence(b, 1)
		d, nd, err := notice(a, idA, mustNext(t, g), 1, 110) // deliver A
		trace("nA1", d, err, nd, nil)
		// Drop B notice. B's next mutation reloads staged PG rev1
		// (180), then mutates +5 from the committed value.
		if err := b.mutate(ctx, func(context.Context) (ReloadCandidate, error) {
			return ReloadCandidate{Revision: 1, Apply: b.noticeTo(180)}, nil
		}, func() { b.value += 5 }); err != nil {
			t.Fatalf("B mutate: %v", err)
		}
		sbb, vb := b.snapshot()
		fmt.Fprintf(&sb, "Bmut k%d r%d p%v v%d mc:%d\n", sbb.KnownRevision, sbb.RequiredRevision, sbb.Pending, vb, b.mutateCalls)
		d, nd, err = notice(b, idB, mustNext(t, g), 1, 999) // late B notice
		trace("lateB1", d, err, nd, nil)
		// Second commit, fence both, then a gap notice on A.
		fence(a, 2)
		fence(b, 2)
		d, nd, err = notice(a, idA, mustNext(t, g), 3, 130) // gap
		trace("gapA3", d, err, nd, err)
		if err := a.state.EnsureReconciled(ctx, func(context.Context) (ReloadCandidate, error) {
			return ReloadCandidate{Revision: 3, Apply: a.noticeTo(130)}, nil
		}); err != nil {
			t.Fatalf("A reload: %v", err)
		}
		sa, va := a.snapshot()
		fmt.Fprintf(&sb, "Areload k%d r%d p%v v%d\n", sa.KnownRevision, sa.RequiredRevision, sa.Pending, va)
		return sb.String()
	}
	a, b := run(), run()
	if a != b {
		t.Fatalf("same-script traces differ:\n%s\n---\n%s", a, b)
	}
	for _, want := range []string{
		"nA1 t3b:applied/true t3c:applied/true",
		"Bmut k1 r1 pfalse v185 mc:1",
		"lateB1 t3b:applied/true t3c:stale/true",
		"gapA3 t3b:applied/false",
		"Areload k3 r3 pfalse v130",
	} {
		if !strings.Contains(a, want) {
			t.Fatalf("trace missing %q:\n%s", want, a)
		}
	}
}

// ---- M4-T4b B1: RequireReload primitive (spec §5.6.9) ----

func TestRequireReloadBasics(t *testing.T) {
	r, err := NewReconcileState(5)
	if err != nil {
		t.Fatal(err)
	}
	r.RequireReload()
	snap := r.Snapshot()
	if snap.KnownRevision != 5 || !snap.Pending || snap.RequiredRevision != 5 {
		t.Fatalf("after RequireReload: %+v, want known5 pending req5", snap)
	}
	// Idempotent: again changes nothing.
	r.RequireReload()
	snap = r.Snapshot()
	if snap.KnownRevision != 5 || !snap.Pending || snap.RequiredRevision != 5 {
		t.Fatalf("after second RequireReload: %+v, want unchanged", snap)
	}
}

func TestRequireReloadPreservesHigherRequirement(t *testing.T) {
	r, err := NewReconcileState(5)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.MarkCommitted(7); err != nil {
		t.Fatal(err)
	}
	r.RequireReload()
	snap := r.Snapshot()
	if snap.KnownRevision != 5 || !snap.Pending || snap.RequiredRevision != 7 {
		t.Fatalf("snap = %+v, want known5 pending req7 (never lowered)", snap)
	}
}

func TestRequireReloadForcesLoaderAndAcceptsKnown(t *testing.T) {
	r, err := NewReconcileState(5)
	if err != nil {
		t.Fatal(err)
	}
	r.RequireReload()
	called := false
	applied := false
	err = r.EnsureReconciled(context.Background(), func(ctx context.Context) (ReloadCandidate, error) {
		called = true
		return ReloadCandidate{
			Revision: 5, // pure verify/reload at known: valid.
			Apply:    func() error { applied = true; return nil },
		}, nil
	})
	if err != nil {
		t.Fatalf("EnsureReconciled: %v", err)
	}
	if !called || !applied {
		t.Fatalf("called = %v applied = %v, want true/true", called, applied)
	}
	snap := r.Snapshot()
	if snap.Pending || snap.KnownRevision != 5 {
		t.Fatalf("snap = %+v, want clear known5", snap)
	}
	// Without RequireReload the loader is never invoked.
	r2, err := NewReconcileState(5)
	if err != nil {
		t.Fatal(err)
	}
	called = false
	if err := r2.EnsureReconciled(context.Background(), func(ctx context.Context) (ReloadCandidate, error) {
		called = true
		return ReloadCandidate{}, nil
	}); err != nil {
		t.Fatalf("EnsureReconciled clear: %v", err)
	}
	if called {
		t.Fatal("loader invoked while clear")
	}
}

func TestRequireReloadBehindAndRegression(t *testing.T) {
	r, err := NewReconcileState(5)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.MarkCommitted(7); err != nil {
		t.Fatal(err)
	}
	r.RequireReload()
	// Candidate 6: above known but below required 7 -> behind.
	if err := r.EnsureReconciled(context.Background(), func(ctx context.Context) (ReloadCandidate, error) {
		return ReloadCandidate{Revision: 6, Apply: func() error { return nil }}, nil
	}); !errors.Is(err, ErrReconcileRevisionBehind) {
		t.Fatalf("rev6 err = %v, want behind", err)
	}
	// Candidate 4: below known -> regression.
	if err := r.EnsureReconciled(context.Background(), func(ctx context.Context) (ReloadCandidate, error) {
		return ReloadCandidate{Revision: 4, Apply: func() error { return nil }}, nil
	}); !errors.Is(err, ErrReconcileRevisionRegression) {
		t.Fatalf("rev4 err = %v, want regression", err)
	}
	snap := r.Snapshot()
	if !snap.Pending || snap.RequiredRevision != 7 || snap.KnownRevision != 5 {
		t.Fatalf("snap = %+v, want still pending req7 known5", snap)
	}
	// Candidate 8 leaps forward and clears.
	if err := r.EnsureReconciled(context.Background(), func(ctx context.Context) (ReloadCandidate, error) {
		return ReloadCandidate{Revision: 8, Apply: func() error { return nil }}, nil
	}); err != nil {
		t.Fatalf("rev8: %v", err)
	}
	snap = r.Snapshot()
	if snap.Pending || snap.KnownRevision != 8 {
		t.Fatalf("snap = %+v, want clear known8", snap)
	}
}
