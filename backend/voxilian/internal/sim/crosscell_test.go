package sim

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dlukt/voxilian/internal/world"
)

// syntheticAggregate is the TEST-ONLY mutable aggregate proving
// cross-cell delivery (spec §5.5.17): a deterministic
// integer/counter mutation standing in for future real gameplay
// operations. Production entities gain no such field and no
// production SyntheticDamage/Trade/Counter types exist.
type syntheticAggregate struct {
	value      int64
	applyCalls int
}

// errSyntheticApply is the sentinel apply failure for
// retry tests.
var errSyntheticApply = errors.New("synthetic apply failure")

// syntheticApply builds the receiver-side apply callback over the
// test harness aggregates. On failure it mutates nothing.
func syntheticApply(aggs map[EntityID]*syntheticAggregate, delta int64, fail bool) func(*entity) error {
	return func(ent *entity) error {
		if fail {
			return fmt.Errorf("synthetic op: %w", errSyntheticApply)
		}
		agg := aggs[ent.id]
		agg.value += delta
		agg.applyCalls++
		return nil
	}
}

// mustCrossCellWorld builds one engine, one worker-1 generator on
// a scripted clock, and an empty aggregate harness.
func mustCrossCellWorld(t *testing.T, ms int64) (*Engine, *OpIDGenerator, map[EntityID]*syntheticAggregate) {
	t.Helper()
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	clk := &scriptClock{ms: ms}
	return e, mustOpIDGen(t, 1, clk), make(map[EntityID]*syntheticAggregate)
}

// mustSyntheticTarget adds a resident entity and registers its
// test aggregate.
func mustSyntheticTarget(t *testing.T, e *Engine, aggs map[EntityID]*syntheticAggregate, pos world.Vec3) (EntityID, *syntheticAggregate) {
	t.Helper()
	snap, err := e.AddEntity(pos)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	agg := &syntheticAggregate{}
	aggs[snap.ID] = agg
	return snap.ID, agg
}

// sendSynthetic is the test-only coordinator: it knows ONLY the
// target EntityID, a committed (immutable snapshot) ownership
// epoch, a fresh OpID, and the test payload delta. It never
// obtains a mutable neighbor entity pointer; the receiver-owned
// deliverCrossCellOp performs the authoritative mutation,
// preserving "coordinator cell NEVER mutates neighbor directly".
func sendSynthetic(t *testing.T, e *Engine, g *OpIDGenerator, target EntityID, delta int64, aggs map[EntityID]*syntheticAggregate, fail bool) (CrossCellOp, CrossCellDisposition, error) {
	t.Helper()
	snap, err := e.Entity(target)
	if err != nil {
		t.Fatalf("sender snapshot: %v", err)
	}
	id, err := g.Next()
	if err != nil {
		t.Fatalf("sender op id: %v", err)
	}
	op := CrossCellOp{ID: id, Target: target, TargetOwner: OwnerRef{Cell: snap.Cell, Generation: snap.OwnershipGeneration}}
	d, err := e.deliverCrossCellOp(op, syntheticApply(aggs, delta, fail))
	return op, d, err
}

// resendSynthetic retries the SAME envelope/OpID through the
// receiver primitive.
func resendSynthetic(e *Engine, op CrossCellOp, aggs map[EntityID]*syntheticAggregate, delta int64, fail bool) (CrossCellDisposition, error) {
	return e.deliverCrossCellOp(op, syntheticApply(aggs, delta, fail))
}

// ownerOf reads the current authoritative ownership epoch.
func ownerOf(t *testing.T, e *Engine, id EntityID) OwnerRef {
	t.Helper()
	snap, err := e.Entity(id)
	if err != nil {
		t.Fatalf("owner snapshot: %v", err)
	}
	return OwnerRef{Cell: snap.Cell, Generation: snap.OwnershipGeneration}
}

func TestRecentOpIDsSemantics(t *testing.T) {
	c := newRecentOpIDs()
	if c.contains(OpID(7)) || c.length() != 0 {
		t.Fatal("empty cache reports membership")
	}
	c.insert(OpID(1))
	c.insert(OpID(2))
	c.insert(OpID(3))
	if !c.contains(OpID(1)) || !c.contains(OpID(2)) || !c.contains(OpID(3)) {
		t.Fatal("inserted IDs missing")
	}
	// Duplicate insert: no reorder, no second entry.
	c.insert(OpID(2))
	if c.length() != 3 {
		t.Fatalf("len = %d, want 3 after duplicate", c.length())
	}
	// Fill to exactly capacity with 253 more unique IDs.
	for i := uint64(4); i <= 256; i++ {
		c.insert(OpID(i))
	}
	if c.length() != RecentOpIDCapacity {
		t.Fatalf("len = %d, want %d", c.length(), RecentOpIDCapacity)
	}
	// Duplicate of the OLDEST must not refresh it: the next
	// unique insert still evicts ID 1 first.
	c.insert(OpID(1))
	c.insert(OpID(257))
	if c.length() != RecentOpIDCapacity {
		t.Fatalf("len = %d, want bounded %d", c.length(), RecentOpIDCapacity)
	}
	if c.contains(OpID(1)) {
		t.Fatal("duplicate insert reordered the oldest entry")
	}
	if !c.contains(OpID(2)) || !c.contains(OpID(257)) {
		t.Fatal("newest entries lost after eviction")
	}
	// Thousands of wraps stay bounded.
	for i := uint64(258); i < 5000; i++ {
		c.insert(OpID(100000 + i))
	}
	if c.length() != RecentOpIDCapacity {
		t.Fatalf("len after wraps = %d, want %d", c.length(), RecentOpIDCapacity)
	}
	if len(c.index) != RecentOpIDCapacity {
		t.Fatalf("index len = %d, want bounded %d (unbounded map leak)", len(c.index), RecentOpIDCapacity)
	}
	if !c.contains(OpID(100000+4999)) || c.contains(OpID(257)) {
		t.Fatal("eviction order broken after many wraps")
	}
}

func TestCrossCellSyntheticExactlyOnce(t *testing.T) {
	e, g, aggs := mustCrossCellWorld(t, OpIDEpochUnixMillis+20000)
	id, agg := mustSyntheticTarget(t, e, aggs, world.Vec3{X: 16, Z: 16})
	op, d, err := sendSynthetic(t, e, g, id, 7, aggs, false)
	if err != nil || d != CrossCellApplied {
		t.Fatalf("first delivery = %v,%v, want applied", d, err)
	}
	if agg.value != 7 || agg.applyCalls != 1 {
		t.Fatalf("aggregate = %+v, want value 7 calls 1", agg)
	}
	d, err = resendSynthetic(e, op, aggs, 7, false)
	if err != nil || d != CrossCellDuplicate {
		t.Fatalf("redelivery = %v,%v, want duplicate", d, err)
	}
	if agg.value != 7 || agg.applyCalls != 1 {
		t.Fatalf("duplicate mutated: %+v", agg)
	}
}

func TestCrossCellLostAck(t *testing.T) {
	e, g, aggs := mustCrossCellWorld(t, OpIDEpochUnixMillis+21000)
	id, agg := mustSyntheticTarget(t, e, aggs, world.Vec3{X: 16, Z: 16})
	// First delivery succeeds, but the sender discards the result
	// (lost acknowledgement) and retries the same envelope.
	op, _, _ := sendSynthetic(t, e, g, id, 7, aggs, false)
	d, err := resendSynthetic(e, op, aggs, 7, false)
	if err != nil || d != CrossCellDuplicate {
		t.Fatalf("lost-ack retry = %v,%v, want duplicate", d, err)
	}
	if agg.value != 7 || agg.applyCalls != 1 {
		t.Fatalf("exactly-once violated: %+v", agg)
	}
}

func TestCrossCellApplyFailureRetry(t *testing.T) {
	e, g, aggs := mustCrossCellWorld(t, OpIDEpochUnixMillis+22000)
	id, agg := mustSyntheticTarget(t, e, aggs, world.Vec3{X: 16, Z: 16})
	op, _, err := sendSynthetic(t, e, g, id, 7, aggs, true)
	if !errors.Is(err, errSyntheticApply) {
		t.Fatalf("failing apply = %v, want sentinel", err)
	}
	if agg.value != 0 || agg.applyCalls != 0 {
		t.Fatalf("failed apply mutated: %+v", agg)
	}
	// Failed applies never enter the cache: the entity stays
	// cache-free and the same OpID may invoke apply again.
	ent, _ := e.registry.lookup(id)
	if ent.recentOps != nil && ent.recentOps.length() != 0 {
		t.Fatal("failed apply recorded in cache")
	}
	d, err := resendSynthetic(e, op, aggs, 7, false)
	if err != nil || d != CrossCellApplied {
		t.Fatalf("retry = %v,%v, want applied", d, err)
	}
	if agg.value != 7 || agg.applyCalls != 1 {
		t.Fatalf("retry aggregate = %+v, want 7/1", agg)
	}
	if !ent.recentOps.contains(op.ID) || ent.recentOps.length() != 1 {
		t.Fatal("successful retry recorded not exactly once")
	}
	d, err = resendSynthetic(e, op, aggs, 7, false)
	if err != nil || d != CrossCellDuplicate {
		t.Fatalf("third delivery = %v,%v, want duplicate", d, err)
	}
	if agg.applyCalls != 1 {
		t.Fatalf("apply calls = %d, want 1", agg.applyCalls)
	}
}

func TestCrossCellInvalidAndUnknown(t *testing.T) {
	e, g, aggs := mustCrossCellWorld(t, OpIDEpochUnixMillis+23000)
	id, agg := mustSyntheticTarget(t, e, aggs, world.Vec3{X: 16, Z: 16})
	// Invalid OpID fails before apply, with no cache allocation.
	bad := CrossCellOp{ID: OpID(0), Target: id, TargetOwner: ownerOf(t, e, id)}
	if _, err := e.deliverCrossCellOp(bad, syntheticApply(aggs, 7, false)); !errors.Is(err, ErrInvalidOpID) {
		t.Fatalf("op 0 = %v, want ErrInvalidOpID", err)
	}
	if agg.applyCalls != 0 {
		t.Fatal("invalid op invoked apply")
	}
	ent, _ := e.registry.lookup(id)
	if ent.recentOps != nil {
		t.Fatal("invalid op allocated cache")
	}
	// Unknown target is not-found, never route-stale, no dedupe.
	ghost := CrossCellOp{ID: mustNext(t, g), Target: EntityID(9999), TargetOwner: OwnerRef{Cell: world.CellCoord{X: 9, Z: 9}, Generation: 1}}
	if _, err := e.deliverCrossCellOp(ghost, syntheticApply(aggs, 7, false)); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("unknown target = %v, want ErrEntityNotFound", err)
	}
	if errors.Is(errGhostRoute(t, e, g, aggs), ErrCrossCellStaleRoute) {
		t.Fatal("unknown target misclassified as stale route")
	}
}

// errGhostRoute delivers to an unknown ID and returns the raw error.
func errGhostRoute(t *testing.T, e *Engine, g *OpIDGenerator, aggs map[EntityID]*syntheticAggregate) error {
	t.Helper()
	ghost := CrossCellOp{ID: mustNext(t, g), Target: EntityID(8888), TargetOwner: OwnerRef{}}
	_, err := e.deliverCrossCellOp(ghost, syntheticApply(aggs, 7, false))
	return err
}

func TestCrossCellStaleRouteAndRefresh(t *testing.T) {
	e, g, aggs := mustCrossCellWorld(t, OpIDEpochUnixMillis+24000)
	id, agg := mustSyntheticTarget(t, e, aggs, world.Vec3{X: 16, Z: 16})
	// Envelope names an old/wrong owner: no mutation, no cache.
	stale := CrossCellOp{ID: mustNext(t, g), Target: id,
		TargetOwner: OwnerRef{Cell: world.CellCoord{X: 9, Z: 9}, Generation: 1}}
	if _, err := e.deliverCrossCellOp(stale, syntheticApply(aggs, 7, false)); !errors.Is(err, ErrCrossCellStaleRoute) {
		t.Fatalf("wrong owner = %v, want ErrCrossCellStaleRoute", err)
	}
	if agg.applyCalls != 0 {
		t.Fatal("stale route invoked apply")
	}
	ent, _ := e.registry.lookup(id)
	if ent.recentOps != nil {
		t.Fatal("stale route allocated cache")
	}
	// Route refresh retries the SAME OpID with the current owner.
	stale.TargetOwner = ownerOf(t, e, id)
	d, err := e.deliverCrossCellOp(stale, syntheticApply(aggs, 7, false))
	if err != nil || d != CrossCellApplied {
		t.Fatalf("refreshed retry = %v,%v, want applied (same OpID)", d, err)
	}
	if agg.value != 7 || agg.applyCalls != 1 {
		t.Fatalf("aggregate = %+v, want 7/1", agg)
	}
}

func TestCrossCellHandoffDedupe(t *testing.T) {
	e, g, aggs := mustCrossCellWorld(t, OpIDEpochUnixMillis+25000)
	id, agg := mustSyntheticTarget(t, e, aggs, world.Vec3{X: 16, Z: 16})
	op, d, err := sendSynthetic(t, e, g, id, 7, aggs, false)
	if err != nil || d != CrossCellApplied {
		t.Fatalf("source apply = %v,%v", d, err)
	}
	// Handoff A{0,0}/g1 -> B{1,0}/g2. The same entity object
	// (and its cache) transfers; nothing is reset or copied.
	tok := holdMigration(t, e.registry, id, world.CellCoord{X: 1, Z: 0}, world.Vec3{X: 32.075, Z: 16})
	if _, err := e.registry.commitHandoff(tok); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// Retry with the stale source owner: route-stale, no apply.
	if _, err := resendSynthetic(e, op, aggs, 7, false); !errors.Is(err, ErrCrossCellStaleRoute) {
		t.Fatalf("stale-owner retry = %v, want ErrCrossCellStaleRoute", err)
	}
	if agg.applyCalls != 1 {
		t.Fatal("stale retry mutated")
	}
	// Refresh to the destination owner, retry the SAME OpID:
	// duplicate, no second mutation.
	op.TargetOwner = ownerOf(t, e, id)
	if got := op.TargetOwner; got != (OwnerRef{Cell: world.CellCoord{X: 1, Z: 0}, Generation: 2}) {
		t.Fatalf("refreshed owner = %+v, want B/g2", got)
	}
	d, err = resendSynthetic(e, op, aggs, 7, false)
	if err != nil || d != CrossCellDuplicate {
		t.Fatalf("post-handoff retry = %v,%v, want duplicate", d, err)
	}
	if agg.value != 7 || agg.applyCalls != 1 {
		t.Fatalf("double-apply across handoff: %+v", agg)
	}
	// Dedupe state demonstrably survived the transfer.
	ent, _ := e.registry.lookup(id)
	if !ent.recentOps.contains(op.ID) {
		t.Fatal("recent-op cache lost across handoff")
	}
	requireRegistryInvariants(t, e.registry)
}

func TestCrossCellFirstDeliveryDuringMigration(t *testing.T) {
	e, g, aggs := mustCrossCellWorld(t, OpIDEpochUnixMillis+26000)
	id, agg := mustSyntheticTarget(t, e, aggs, world.Vec3{X: 16, Z: 16})
	tok := holdMigration(t, e.registry, id, world.CellCoord{X: 1, Z: 0}, world.Vec3{X: 32.075, Z: 16})
	op := CrossCellOp{ID: mustNext(t, g), Target: id, TargetOwner: ownerOf(t, e, id)}
	// Not yet applied and target migrating: retryable migrating
	// error, no apply, no cache insert — and the cross-cell op
	// never enters the movement-intent migration queue.
	if _, err := e.deliverCrossCellOp(op, syntheticApply(aggs, 7, false)); !errors.Is(err, ErrCrossCellTargetMigrating) {
		t.Fatalf("migrating delivery = %v, want ErrCrossCellTargetMigrating", err)
	}
	if agg.applyCalls != 0 {
		t.Fatal("migrating target applied")
	}
	rec := e.registry.migrations[id]
	if len(rec.queued) != 0 {
		t.Fatal("cross-cell op leaked into the MoveIntent queue")
	}
	if rec.entity.recentOps.contains(op.ID) {
		t.Fatal("migrating error inserted cache entry")
	}
	if _, err := e.registry.commitHandoff(tok); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// Refresh owner, retry SAME OpID: applied exactly once.
	op.TargetOwner = ownerOf(t, e, id)
	d, err := e.deliverCrossCellOp(op, syntheticApply(aggs, 7, false))
	if err != nil || d != CrossCellApplied {
		t.Fatalf("post-install retry = %v,%v, want applied", d, err)
	}
	if agg.value != 7 || agg.applyCalls != 1 {
		t.Fatalf("aggregate = %+v, want 7/1", agg)
	}
	requireRegistryInvariants(t, e.registry)
}

func TestCrossCellRetryAfterAbort(t *testing.T) {
	e, g, aggs := mustCrossCellWorld(t, OpIDEpochUnixMillis+27000)
	id, agg := mustSyntheticTarget(t, e, aggs, world.Vec3{X: 16, Z: 16})
	srcOwner := ownerOf(t, e, id)
	holdMigration(t, e.registry, id, world.CellCoord{X: 1, Z: 0}, world.Vec3{X: 32.075, Z: 16})
	op := CrossCellOp{ID: mustNext(t, g), Target: id, TargetOwner: srcOwner}
	if _, err := e.deliverCrossCellOp(op, syntheticApply(aggs, 7, false)); !errors.Is(err, ErrCrossCellTargetMigrating) {
		t.Fatalf("migrating delivery = %v, want ErrCrossCellTargetMigrating", err)
	}
	// Lossless abort restores the source owner; retrying the SAME
	// OpID under the refreshed source owner applies — no
	// operation is lost merely because the handoff aborted.
	if !e.registry.abortHandoff(id) {
		t.Fatal("abort returned false")
	}
	op.TargetOwner = ownerOf(t, e, id)
	if op.TargetOwner != srcOwner {
		t.Fatalf("post-abort owner = %+v, want restored %+v", op.TargetOwner, srcOwner)
	}
	d, err := e.deliverCrossCellOp(op, syntheticApply(aggs, 7, false))
	if err != nil || d != CrossCellApplied {
		t.Fatalf("post-abort retry = %v,%v, want applied", d, err)
	}
	if agg.value != 7 || agg.applyCalls != 1 {
		t.Fatalf("aggregate = %+v, want 7/1", agg)
	}
	requireRegistryInvariants(t, e.registry)
}

func TestCrossCellAbortPreservesCache(t *testing.T) {
	e, g, aggs := mustCrossCellWorld(t, OpIDEpochUnixMillis+28000)
	id, agg := mustSyntheticTarget(t, e, aggs, world.Vec3{X: 16, Z: 16})
	op, _, _ := sendSynthetic(t, e, g, id, 7, aggs, false)
	holdMigration(t, e.registry, id, world.CellCoord{X: 1, Z: 0}, world.Vec3{X: 32.075, Z: 16})
	if !e.registry.abortHandoff(id) {
		t.Fatal("abort returned false")
	}
	// Lossless abort preserved dedupe state too: the same op is
	// still a duplicate, with no second mutation.
	d, err := resendSynthetic(e, op, aggs, 7, false)
	if err != nil || d != CrossCellDuplicate {
		t.Fatalf("post-abort redelivery = %v,%v, want duplicate", d, err)
	}
	if agg.value != 7 || agg.applyCalls != 1 {
		t.Fatalf("aggregate = %+v, want 7/1", agg)
	}
	requireRegistryInvariants(t, e.registry)
}

func TestCrossCellHandoffDuplicateNoCacheReset(t *testing.T) {
	e, g, aggs := mustCrossCellWorld(t, OpIDEpochUnixMillis+29000)
	id, agg := mustSyntheticTarget(t, e, aggs, world.Vec3{X: 16, Z: 16})
	op, _, _ := sendSynthetic(t, e, g, id, 7, aggs, false)
	tok := holdMigration(t, e.registry, id, world.CellCoord{X: 1, Z: 0}, world.Vec3{X: 32.075, Z: 16})
	if _, err := e.registry.commitHandoff(tok); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// T3a duplicate handoff delivery must not clear, replay, or
	// otherwise modify recent cross-cell OpIDs.
	d, err := e.registry.commitHandoff(tok)
	if err != nil || d != HandoffDuplicate {
		t.Fatalf("redelivery = %v,%v, want duplicate", d, err)
	}
	op.TargetOwner = ownerOf(t, e, id)
	d2, err := resendSynthetic(e, op, aggs, 7, false)
	if err != nil || d2 != CrossCellDuplicate {
		t.Fatalf("post-handoff redelivery = %v,%v, want duplicate", d2, err)
	}
	if agg.value != 7 || agg.applyCalls != 1 {
		t.Fatalf("aggregate = %+v, want 7/1", agg)
	}
	requireRegistryInvariants(t, e.registry)
}

func TestCrossCellCacheCapacityHandoff(t *testing.T) {
	e, g, aggs := mustCrossCellWorld(t, OpIDEpochUnixMillis+30000)
	id, agg := mustSyntheticTarget(t, e, aggs, world.Vec3{X: 16, Z: 16})
	// Fill the cache to exactly capacity with distinct applied ops.
	ids := make([]OpID, 0, RecentOpIDCapacity)
	for i := 0; i < RecentOpIDCapacity; i++ {
		op, d, err := sendSynthetic(t, e, g, id, 1, aggs, false)
		if err != nil || d != CrossCellApplied {
			t.Fatalf("fill %d = %v,%v", i, d, err)
		}
		ids = append(ids, op.ID)
	}
	ent, _ := e.registry.lookup(id)
	if ent.recentOps.length() != RecentOpIDCapacity {
		t.Fatalf("len = %d, want %d", ent.recentOps.length(), RecentOpIDCapacity)
	}
	if agg.applyCalls != RecentOpIDCapacity {
		t.Fatalf("calls = %d, want %d", agg.applyCalls, RecentOpIDCapacity)
	}
	// Handoff preserves the full cache: same retained IDs, same
	// oldest/newest eviction semantics.
	tok := holdMigration(t, e.registry, id, world.CellCoord{X: 1, Z: 0}, world.Vec3{X: 32.075, Z: 16})
	if _, err := e.registry.commitHandoff(tok); err != nil {
		t.Fatalf("commit: %v", err)
	}
	ent, _ = e.registry.lookup(id)
	if ent.recentOps.length() != RecentOpIDCapacity {
		t.Fatalf("post-handoff len = %d, want %d", ent.recentOps.length(), RecentOpIDCapacity)
	}
	for _, want := range ids {
		if !ent.recentOps.contains(want) {
			t.Fatalf("op %d lost across handoff", uint64(want))
		}
	}
	// Normal eviction continues: the next op evicts the oldest.
	next, d, err := sendSynthetic(t, e, g, id, 1, aggs, false)
	if err != nil || d != CrossCellApplied {
		t.Fatalf("post-handoff op = %v,%v", d, err)
	}
	if ent.recentOps.contains(ids[0]) {
		t.Fatal("oldest entry survived past capacity")
	}
	if !ent.recentOps.contains(next.ID) || !ent.recentOps.contains(ids[len(ids)-1]) {
		t.Fatal("newest entries missing after eviction")
	}
	if ent.recentOps.length() != RecentOpIDCapacity {
		t.Fatalf("len = %d, want bounded %d", ent.recentOps.length(), RecentOpIDCapacity)
	}
	requireRegistryInvariants(t, e.registry)
}

func TestCrossCellRemovalResetsDedupe(t *testing.T) {
	e, g, aggs := mustCrossCellWorld(t, OpIDEpochUnixMillis+31000)
	id, _ := mustSyntheticTarget(t, e, aggs, world.Vec3{X: 16, Z: 16})
	if _, _, err := sendSynthetic(t, e, g, id, 7, aggs, false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := e.RemoveEntity(id); err != nil {
		t.Fatalf("remove: %v", err)
	}
	// Dedupe state disappears with the entity; a new EntityID
	// starts clean (EntityIDs are never reused: no ABA).
	id2, agg2 := mustSyntheticTarget(t, e, aggs, world.Vec3{X: 48, Z: 16})
	if id2 == id {
		t.Fatal("EntityID reused after removal")
	}
	if _, d, err := sendSynthetic(t, e, g, id2, 7, aggs, false); err != nil || d != CrossCellApplied {
		t.Fatalf("fresh op on new ID = %v,%v, want applied", d, err)
	}
	if agg2.applyCalls != 1 {
		t.Fatalf("calls = %d, want 1 (no global cache leak)", agg2.applyCalls)
	}
	requireRegistryInvariants(t, e.registry)
}

func TestCrossCellDeterministicTrace(t *testing.T) {
	run := func() string {
		var sb strings.Builder
		e, g, aggs := mustCrossCellWorld(t, OpIDEpochUnixMillis+32000)
		id, agg := mustSyntheticTarget(t, e, aggs, world.Vec3{X: 16, Z: 16})
		disps := func(d CrossCellDisposition, err error) string {
			if err != nil {
				return "err:" + errClass(err)
			}
			return d.String()
		}
		_, d, err := sendSynthetic(t, e, g, id, 7, aggs, false) // fresh
		fmt.Fprintf(&sb, "fresh:%s v:%d c:%d\n", disps(d, err), agg.value, agg.applyCalls)
		op2, _, _ := sendSynthetic(t, e, g, id, 3, aggs, false) // lost ACK
		d, err = resendSynthetic(e, op2, aggs, 3, false)
		fmt.Fprintf(&sb, "lostack:%s v:%d c:%d\n", disps(d, err), agg.value, agg.applyCalls)
		op3, _, err3 := sendSynthetic(t, e, g, id, 5, aggs, true) // apply error
		fmt.Fprintf(&sb, "fail:%s v:%d c:%d\n", disps(CrossCellApplied, err3), agg.value, agg.applyCalls)
		d, err = resendSynthetic(e, op3, aggs, 5, false) // retry ok
		fmt.Fprintf(&sb, "failretry:%s v:%d c:%d\n", disps(d, err), agg.value, agg.applyCalls)
		d, err = resendSynthetic(e, op3, aggs, 5, false) // dup
		fmt.Fprintf(&sb, "faildup:%s v:%d c:%d\n", disps(d, err), agg.value, agg.applyCalls)
		stale := CrossCellOp{ID: mustNext(t, g), Target: id,
			TargetOwner: OwnerRef{Cell: world.CellCoord{X: 9, Z: 9}, Generation: 1}}
		_, err = e.deliverCrossCellOp(stale, syntheticApply(aggs, 1, false))
		fmt.Fprintf(&sb, "stale:%s\n", disps(CrossCellApplied, err))
		stale.TargetOwner = ownerOf(t, e, id) // refresh, same ID
		d, err = e.deliverCrossCellOp(stale, syntheticApply(aggs, 1, false))
		fmt.Fprintf(&sb, "refresh:%s v:%d c:%d\n", disps(d, err), agg.value, agg.applyCalls)
		tok := holdMigration(t, e.registry, id, world.CellCoord{X: 1, Z: 0}, world.Vec3{X: 32.075, Z: 16})
		op4 := CrossCellOp{ID: mustNext(t, g), Target: id, TargetOwner: stale.TargetOwner}
		_, err = e.deliverCrossCellOp(op4, syntheticApply(aggs, 1, false)) // migrating
		fmt.Fprintf(&sb, "migrating:%s q:%d\n", disps(CrossCellApplied, err), len(e.registry.migrations[id].queued))
		if _, err := e.registry.commitHandoff(tok); err != nil {
			t.Fatalf("commit: %v", err)
		}
		op4.TargetOwner = ownerOf(t, e, id)
		d, err = e.deliverCrossCellOp(op4, syntheticApply(aggs, 1, false)) // install retry
		fmt.Fprintf(&sb, "installed:%s v:%d c:%d\n", disps(d, err), agg.value, agg.applyCalls)
		stale.TargetOwner = ownerOf(t, e, id) // pre-handoff op under post-handoff owner
		d, err = e.deliverCrossCellOp(stale, syntheticApply(aggs, 1, false))
		fmt.Fprintf(&sb, "handdup:%s v:%d c:%d\n", disps(d, err), agg.value, agg.applyCalls)
		op5 := CrossCellOp{ID: mustNext(t, g), Target: id, TargetOwner: ownerOf(t, e, id)}
		holdMigration(t, e.registry, id, world.CellCoord{X: 0, Z: 0}, world.Vec3{X: 16, Z: 16})
		_, err = e.deliverCrossCellOp(op5, syntheticApply(aggs, 1, false)) // migrating again
		fmt.Fprintf(&sb, "migrating2:%s\n", disps(CrossCellApplied, err))
		if !e.registry.abortHandoff(id) {
			t.Fatal("abort returned false")
		}
		op5.TargetOwner = ownerOf(t, e, id) // post-abort owner
		d, err = e.deliverCrossCellOp(op5, syntheticApply(aggs, 1, false))
		fmt.Fprintf(&sb, "abortretry:%s v:%d c:%d\n", disps(d, err), agg.value, agg.applyCalls)
		for i := 0; i < 300; i++ { // eviction pressure
			if _, _, err := sendSynthetic(t, e, g, id, 1, aggs, false); err != nil {
				t.Fatalf("evict %d: %v", i, err)
			}
		}
		snap, _ := e.Entity(id)
		ent, _ := e.registry.lookup(id)
		fmt.Fprintf(&sb, "final:v:%d c:%d own:{%d,%d,g%d} cache:%d\n",
			agg.value, agg.applyCalls, snap.Cell.X, snap.Cell.Z, snap.OwnershipGeneration, ent.recentOps.length())
		return sb.String()
	}
	a, b := run(), run()
	if a != b {
		t.Fatalf("same-script traces differ:\n%s\n---\n%s", a, b)
	}
	for _, want := range []string{
		"fresh:applied", "lostack:duplicate", "fail:err:synthetic",
		"failretry:applied", "faildup:duplicate", "stale:err:stale",
		"refresh:applied", "migrating:err:migrating", "installed:applied",
		"handdup:duplicate", "migrating2:err:migrating", "abortretry:applied", "cache:256",
	} {
		if !strings.Contains(a, want) {
			t.Fatalf("trace missing %q:\n%s", want, a)
		}
	}
}

// errClass names the delivery error domain for trace comparison.
func errClass(err error) string {
	switch {
	case errors.Is(err, errSyntheticApply):
		return "synthetic"
	case errors.Is(err, ErrCrossCellStaleRoute):
		return "stale"
	case errors.Is(err, ErrCrossCellTargetMigrating):
		return "migrating"
	case errors.Is(err, ErrInvalidOpID):
		return "invalid"
	case errors.Is(err, ErrEntityNotFound):
		return "notfound"
	default:
		return "other"
	}
}
