package sim

import (
	"errors"
	"fmt"
)

// RecentOpIDCapacity bounds each entity's recent-op cache
// (spec §5.5.15): the most recent 256 SUCCESSFULLY APPLIED
// distinct OpIDs. This covers the internal retry, lost
// acknowledgement, handoff, and short transport redelivery
// window only — not permanent event sourcing.
const RecentOpIDCapacity = 256

// Stable cross-cell delivery errors. Matching MUST use
// errors.Is, never string parsing as control flow.
var (
	// ErrInvalidOpID marks OpID(0): invalid/reserved. No
	// mutation, no cache insertion.
	ErrInvalidOpID = errors.New("sim: invalid op ID")
	// ErrCrossCellTargetMigrating marks delivery to a
	// currently-MIGRATING target whose cache does not already
	// hold the op: retryable, no apply, no dedupe insertion.
	// The caller retries the SAME OpID after migration
	// completes. This never becomes wire 202.
	ErrCrossCellTargetMigrating = errors.New("sim: cross-cell target migrating")
	// ErrCrossCellStaleRoute marks a resident target whose
	// current OwnerRef differs from the envelope's TargetOwner:
	// no mutation. The caller refreshes the current OwnerRef
	// and retries the SAME OpID; it does NOT allocate a
	// replacement OpID.
	ErrCrossCellStaleRoute = errors.New("sim: cross-cell stale route")
)

// recentOpIDs is the bounded recent-op dedupe cache
// (spec §5.5.15): O(1) membership plus bounded FIFO eviction,
// reusable by future cell-scoped receivers. Entities that never
// receive cross-cell operations never allocate one (the entity
// holds a nil pointer until the first successful apply).
// Duplicate delivery neither reorders the cache nor inserts a
// second entry; failed applies never enter it. After
// initialization/fill, steady-state operation allocates nothing
// and stores at most RecentOpIDCapacity entries — never an
// unbounded map standing alone.
type recentOpIDs struct {
	index map[OpID]struct{}
	ring  [RecentOpIDCapacity]OpID
	start int
	count int
}

// newRecentOpIDs builds an empty cache.
func newRecentOpIDs() *recentOpIDs {
	return &recentOpIDs{index: make(map[OpID]struct{}, RecentOpIDCapacity)}
}

// contains reports membership. A nil cache holds nothing.
func (c *recentOpIDs) contains(id OpID) bool {
	if c == nil {
		return false
	}
	_, ok := c.index[id]
	return ok
}

// length reports the number of retained IDs (at most
// RecentOpIDCapacity).
func (c *recentOpIDs) length() int {
	if c == nil {
		return 0
	}
	return c.count
}

// insert records a successfully applied ID. Duplicate inserts
// are no-ops (no reorder, no second entry). At capacity the
// oldest entry is evicted first. Callers insert only after a
// successful apply.
func (c *recentOpIDs) insert(id OpID) {
	if _, ok := c.index[id]; ok {
		return
	}
	if c.count == RecentOpIDCapacity {
		oldest := c.ring[c.start]
		delete(c.index, oldest)
		c.ring[c.start] = id
		c.index[id] = struct{}{}
		c.start = (c.start + 1) % RecentOpIDCapacity
		return
	}
	c.ring[(c.start+c.count)%RecentOpIDCapacity] = id
	c.index[id] = struct{}{}
	c.count++
}

// CrossCellOp is the minimal cross-cell routing envelope
// (spec §5.5.11): the opaque operation ID plus the target's
// expected ownership epoch. No production payload or operation
// kind travels here yet; the synthetic test supplies its apply
// behavior separately.
type CrossCellOp struct {
	ID          OpID
	Target      EntityID
	TargetOwner OwnerRef
}

// CrossCellDisposition is the deliverCrossCellOp outcome.
// Duplicate is an ordinary no-op result, not an error; on error
// the disposition is meaningless — check err first.
type CrossCellDisposition int

const (
	// CrossCellApplied means the apply callback ran exactly once
	// and the OpID entered the recent-op cache.
	CrossCellApplied CrossCellDisposition = iota
	// CrossCellDuplicate means the OpID was already applied;
	// apply was not called again.
	CrossCellDuplicate
)

// String names the disposition for logs/tests.
func (d CrossCellDisposition) String() string {
	switch d {
	case CrossCellApplied:
		return "applied"
	case CrossCellDuplicate:
		return "duplicate"
	default:
		return "unknown"
	}
}

// deliverCrossCellOp is the single receiver-side apply-once
// primitive (spec §5.5.13), internal to sim. It never hands out
// a mutable neighbor pointer: the coordinator cell supplies only
// the envelope, and this owner-side path performs the
// authoritative mutation through apply.
//
// Binding order for an ownership-matching resident target:
// validate OpID, check the recent-op cache, answer duplicate
// without apply, otherwise invoke apply, and only after a
// successful apply record the OpID and return applied.
//
// Because T3b is in-memory infrastructure, apply must obey: on
// error, no partial authoritative mutation (validate first, then
// make an in-memory mutation that cannot subsequently fail).
// T3b cannot roll back an arbitrary partially-mutating
// callback; T3c separately handles durable PG
// commit/reconciliation.
//
// Retry contract (spec §5.5.14): retry means resubmitting the
// exact same CrossCellOp with the same OpID. There is no retry
// goroutine, timer wheel, backoff, bus, or outbox here.
func (e *Engine) deliverCrossCellOp(op CrossCellOp, apply func(*entity) error) (CrossCellDisposition, error) {
	// Invalid ID first: before any apply or cache mutation.
	if op.ID == OpID(0) {
		return CrossCellApplied, fmt.Errorf("%w: cross-cell op to %d",
			ErrInvalidOpID, uint64(op.Target))
	}
	// A migrating target owns its quiesced entity in the record.
	// An already-applied op still answers duplicate (no second
	// mutation); anything new is retryable later under the same
	// identity — never applied into the quiesced entity and never
	// inserted into the cache. Cross-cell retries are
	// sender-driven with the same OpID, never queued into the
	// movement-intent migration queue.
	if rec, ok := e.registry.migrations[op.Target]; ok {
		if rec.entity.recentOps.contains(op.ID) {
			return CrossCellDuplicate, nil
		}
		return CrossCellApplied, fmt.Errorf("%w: id %d",
			ErrCrossCellTargetMigrating, uint64(op.Target))
	}
	// Authoritative owner is the registry Cell+Generation, never a
	// position-derived cell.
	ent, err := e.registry.lookup(op.Target)
	if err != nil {
		return CrossCellApplied, err
	}
	if ent.cell != op.TargetOwner.Cell || ent.generation != op.TargetOwner.Generation {
		return CrossCellApplied, fmt.Errorf("%w: id %d resident {%v,g%d} vs route {%v,g%d}",
			ErrCrossCellStaleRoute, uint64(op.Target),
			ent.cell, ent.generation, op.TargetOwner.Cell, op.TargetOwner.Generation)
	}
	// Dedupe before apply: a seen ID never re-invokes apply.
	if ent.recentOps.contains(op.ID) {
		return CrossCellDuplicate, nil
	}
	// Failed apply stays retryable by identity: the OpID does NOT
	// enter the cache, so the same envelope may invoke apply
	// again. The error propagates with errors.Is preserved.
	if err := apply(ent); err != nil {
		return CrossCellApplied, fmt.Errorf("sim: cross-cell apply id %d op %d: %w",
			uint64(op.Target), uint64(op.ID), err)
	}
	// Record-after-apply: only success becomes seen. Lazy
	// allocation keeps cross-cell-free entities cache-free.
	if ent.recentOps == nil {
		ent.recentOps = newRecentOpIDs()
	}
	ent.recentOps.insert(op.ID)
	return CrossCellApplied, nil
}
