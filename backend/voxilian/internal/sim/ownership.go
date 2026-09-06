package sim

import (
	"errors"
	"fmt"
	"slices"

	"github.com/dlukt/voxilian/internal/world"
)

// MigrationMoveQueueCapacity bounds each migrating entity's FIFO
// movement-intent queue (spec §5.4.7). Generous against the ≤10 Hz
// input rate and the normally sub-tick local transfer window; no
// unbounded growth, no external broker, no goroutine.
const MigrationMoveQueueCapacity = 64

// Stable ownership/migration errors. Matching MUST use errors.Is,
// never string parsing as control flow.
var (
	// ErrMigrationQueueFull marks a valid/newer migration intent that
	// arrived at a full 64-entry queue. Zero mutation: no append, no
	// frontier advance, no InputSeq consumption. M4-T5 later maps
	// exactly this condition to 202 error{retry}; nothing else does.
	ErrMigrationQueueFull = errors.New("sim: migration queue full")
	// ErrOwnershipGenerationExhausted marks a handoff that would wrap
	// past math.MaxUint64. The entity holds source position with
	// speed 0; this internal exhaustion is NOT queue saturation and
	// MUST NOT become retry.
	ErrOwnershipGenerationExhausted = errors.New("sim: ownership generation exhausted")
	// ErrOwnershipMismatch marks a handoff token that names the wrong
	// source/destination/EntityID or a generation gap. Zero mutation;
	// the engine never silently repairs it.
	ErrOwnershipMismatch = errors.New("sim: ownership mismatch")
)

// OwnerRef is the ownership epoch: the resident cell plus its
// generation (spec §5.4.1). Generation 0 is reserved/invalid; new
// entities start at 1; each successful handoff adds exactly one.
type OwnerRef struct {
	Cell       world.CellCoord
	Generation uint64
}

// HandoffDisposition is the commitHandoff outcome. Duplicate/stale
// are no-op results, not errors; on error the disposition is
// meaningless — check err first.
type HandoffDisposition int

const (
	// HandoffInstalled means the transfer committed to the destination.
	HandoffInstalled HandoffDisposition = iota
	// HandoffDuplicate means the exact transfer was already installed.
	HandoffDuplicate
	// HandoffStale means the envelope predates current ownership.
	HandoffStale
)

// String names the disposition for logs/tests.
func (d HandoffDisposition) String() string {
	switch d {
	case HandoffInstalled:
		return "installed"
	case HandoffDuplicate:
		return "duplicate"
	case HandoffStale:
		return "stale"
	default:
		return "unknown"
	}
}

// handoffToken names one ownership transfer for install/delivery.
// Tests may forge tokens to prove validation; production builds them
// only in beginHandoff.
type handoffToken struct {
	id   EntityID
	from OwnerRef
	to   OwnerRef
}

// migrationRecord is the explicit MIGRATING route state (spec
// §5.4.2): the quiesced entity plus its From/To refs, the exact
// pre-transfer source position for lossless abort, its bounded FIFO
// movement queue, and private sequence frontier. There is exactly
// one mutable entity object — the record OWNS it while migrating,
// never a second copy.
type migrationRecord struct {
	id     EntityID
	from   OwnerRef
	to     OwnerRef
	entity *entity
	// sourcePosition is the exact authoritative pre-transfer
	// position, captured at begin before the entity takes its
	// final dest-side position. Abort restores it verbatim —
	// never inferred from the source cell, never snapped to a
	// center/boundary.
	sourcePosition world.Vec3
	queued         []MoveIntent
	hasFrontier    bool
	frontier       uint32
}

// beginHandoff validates preconditions and quiesces the source
// (spec §5.4.4): the caller computes the final authoritative
// position and passes it explicitly. On success the record captures
// the exact pre-transfer source position (for lossless abort), the
// entity takes the final dest-side position, and resident
// membership moves into the migration record, which owns the
// entity until commit or abort. Failure mutates nothing: no
// membership change, no position change, no record. The entity's
// final gameplay state (yaw, speed, anchor, controls) must already
// be computed by the caller — after success the source performs no
// more gameplay mutation.
func (r *registry) beginHandoff(id EntityID, from OwnerRef, dest world.CellCoord, final world.Vec3) (handoffToken, error) {
	var zero handoffToken
	if from.Generation == 0 {
		return zero, fmt.Errorf("%w: reserved zero source generation", ErrOwnershipMismatch)
	}
	coord, ok := r.entityCell[id]
	if !ok {
		if _, mig := r.migrations[id]; mig {
			return zero, fmt.Errorf("%w: id %d already migrating", ErrOwnershipMismatch, uint64(id))
		}
		return zero, fmt.Errorf("%w: id %d", ErrEntityNotFound, uint64(id))
	}
	if coord != from.Cell {
		return zero, fmt.Errorf("%w: id %d resident %v vs source %v",
			ErrOwnershipMismatch, uint64(id), coord, from.Cell)
	}
	c, ok := r.cells[coord]
	if !ok {
		return zero, fmt.Errorf("%w: id %d locator without cell", ErrOwnershipMismatch, uint64(id))
	}
	ent, ok := c.entities[id]
	if !ok {
		return zero, fmt.Errorf("%w: id %d cell without entity", ErrOwnershipMismatch, uint64(id))
	}
	if ent.generation != from.Generation {
		return zero, fmt.Errorf("%w: id %d generation %d vs source %d",
			ErrOwnershipMismatch, uint64(id), ent.generation, from.Generation)
	}
	if dest == coord {
		return zero, fmt.Errorf("%w: destination == source %v", ErrOwnershipMismatch, coord)
	}
	next := from.Generation + 1
	if next == 0 {
		return zero, fmt.Errorf("%w: id %d at MaxUint64", ErrOwnershipGenerationExhausted, uint64(id))
	}
	if got, err := world.CellForPosition(final); err != nil || got != dest {
		return zero, fmt.Errorf("%w: id %d final %v vs destination %v",
			ErrOwnershipMismatch, uint64(id), final, dest)
	}
	tok := handoffToken{id: id, from: from, to: OwnerRef{Cell: dest, Generation: next}}
	r.migrations[id] = &migrationRecord{
		id:             id,
		from:           from,
		to:             tok.to,
		entity:         ent,
		sourcePosition: ent.position,
		queued:         make([]MoveIntent, 0, MigrationMoveQueueCapacity),
		hasFrontier:    ent.hasAccepted,
		frontier:       ent.lastAcceptedSeq,
	}
	ent.position = final
	delete(c.entities, id)
	delete(r.entityCell, id)
	return tok, nil
}

// commitHandoff installs a quiesced migration into its destination
// (spec §5.4.5): same EntityID exactly once, destination Cell,
// Generation G+1, all gameplay/history/control state preserved,
// FIFO queue drained through resident sequencing (establishing the
// next pending control WITHOUT same-tick movement), emptied source
// cell removed. EntityCount is unchanged. The entity's recent-op
// dedupe cache travels with the same object automatically —
// nothing is reset, copied, or replayed here.
func (r *registry) commitHandoff(tok handoffToken) (HandoffDisposition, error) {
	rec, ok := r.migrations[tok.id]
	if !ok {
		return r.resolvePostHandoff(tok)
	}
	if rec.id != tok.id || rec.from != tok.from || rec.to != tok.to {
		return HandoffInstalled, fmt.Errorf("%w: token %+v vs migration %+v",
			ErrOwnershipMismatch, tok, handoffRef{from: rec.from, to: rec.to})
	}
	ent := rec.entity
	destCell, ok := r.cells[rec.to.Cell]
	if !ok {
		destCell = &cell{coord: rec.to.Cell, entities: make(map[EntityID]*entity)}
		r.cells[rec.to.Cell] = destCell
	}
	if _, dup := destCell.entities[tok.id]; dup {
		r.abortHandoff(tok.id)
		return HandoffInstalled, fmt.Errorf("%w: id %d already destination-resident",
			ErrOwnershipMismatch, uint64(tok.id))
	}
	ent.cell = rec.to.Cell
	ent.generation = rec.to.Generation
	destCell.entities[tok.id] = ent
	r.entityCell[tok.id] = rec.to.Cell
	delete(r.migrations, tok.id)
	replayMigrationMoves(ent, rec.queued)
	if src, ok := r.cells[rec.from.Cell]; ok && len(src.entities) == 0 {
		delete(r.cells, rec.from.Cell)
	}
	return HandoffInstalled, nil
}

// replayMigrationMoves drains a migration's queued intents through
// the SAME resident sequencing/coalescing semantics as SubmitMove
// (spec §5.4.7): each queued intent classifies against the live
// accepted frontier, and only newer accepted intents advance it and
// replace pending control. It establishes pending control ONLY —
// no position integration, no anchor advance, no history touch.
// Commit and abort share this one helper; no second replay
// implementation may diverge from it. Only actually queued
// (accepted) intents replay: duplicates, stale inputs, ambiguous
// gaps, and queue-full rejections were never queued and can never
// be resurrected here.
func replayMigrationMoves(ent *entity, queued []MoveIntent) {
	for _, qi := range queued {
		d, err := classifyMoveIntent(ent.hasAccepted, ent.lastAcceptedSeq, qi.InputSeq)
		if err != nil || d != MoveAccepted {
			continue
		}
		ent.hasAccepted = true
		ent.lastAcceptedSeq = qi.InputSeq
		ent.pending = qi
		ent.hasPending = true
	}
}

// handoffRef renders migration endpoints for mismatch diagnostics.
type handoffRef struct {
	from OwnerRef
	to   OwnerRef
}

// resolvePostHandoff classifies delivery when no migration is open:
// an already-installed transfer is a duplicate, an older envelope is
// stale, anything else is a mismatch (or not-found for unknown IDs).
func (r *registry) resolvePostHandoff(tok handoffToken) (HandoffDisposition, error) {
	coord, ok := r.entityCell[tok.id]
	if !ok {
		return HandoffInstalled, fmt.Errorf("%w: id %d", ErrEntityNotFound, uint64(tok.id))
	}
	c, ok := r.cells[coord]
	if !ok {
		return HandoffInstalled, fmt.Errorf("%w: id %d locator without cell", ErrOwnershipMismatch, uint64(tok.id))
	}
	ent, ok := c.entities[tok.id]
	if !ok {
		return HandoffInstalled, fmt.Errorf("%w: id %d cell without entity", ErrOwnershipMismatch, uint64(tok.id))
	}
	if ent.cell == tok.to.Cell && ent.generation == tok.to.Generation {
		return HandoffDuplicate, nil
	}
	if ent.generation > tok.to.Generation {
		return HandoffStale, nil
	}
	return HandoffInstalled, fmt.Errorf("%w: id %d resident {%v,g%d} vs token to {%v,g%d}",
		ErrOwnershipMismatch, uint64(tok.id), ent.cell, ent.generation, tok.to.Cell, tok.to.Generation)
}

// abortHandoff is the complete rollback primitive for a quiesced
// migration: the same single entity object returns to RESIDENT
// source ownership with its source Cell, source Generation, exact
// pre-transfer source position, and source cell membership
// restored; accepted queued intents replay through the shared
// sequencing helper (becoming normal pending source-owner work for
// the next tick, never same-tick movement); EntityID, history,
// active controls, and the processed anchor are preserved; the
// migration record is removed. No caller-side follow-up repair is
// required for registry consistency. Normal flows never strand
// entities mid-migration; this runs only if a local commit fails
// unexpectedly so no tick boundary ever observes a lost entity.
func (r *registry) abortHandoff(id EntityID) bool {
	rec, ok := r.migrations[id]
	if !ok {
		return false
	}
	ent := rec.entity
	ent.cell = rec.from.Cell
	ent.generation = rec.from.Generation
	ent.position = rec.sourcePosition
	replayMigrationMoves(ent, rec.queued)
	srcCell, ok := r.cells[rec.from.Cell]
	if !ok {
		srcCell = &cell{coord: rec.from.Cell, entities: make(map[EntityID]*entity)}
		r.cells[rec.from.Cell] = srcCell
	}
	srcCell.entities[id] = ent
	r.entityCell[id] = rec.from.Cell
	delete(r.migrations, id)
	return true
}

// workItem is one tick-start ownership work entry: identity plus the
// expected resident epoch (spec §5.4.3). Order data only — never
// copied entity state.
type workItem struct {
	id         EntityID
	cell       world.CellCoord
	generation uint64
}

// residentWorklist captures the resident ownership worklist in
// canonical order (CellCoord X/Z, then EntityID) with each item's
// expected generation. Step processes this fixed snapshot so a
// migrated-in entity can never be seen twice in one tick.
func (r *registry) residentWorklist() []workItem {
	items := make([]workItem, 0, len(r.entityCell))
	for id, coord := range r.entityCell {
		c, ok := r.cells[coord]
		if !ok {
			continue
		}
		ent, ok := c.entities[id]
		if !ok {
			continue
		}
		items = append(items, workItem{id: id, cell: coord, generation: ent.generation})
	}
	sortWorkItems(items)
	return items
}

// sortWorkItems orders the tick-start worklist canonically: source
// CellCoord X ascending, then Z ascending, then EntityID ascending.
func sortWorkItems(items []workItem) {
	slices.SortFunc(items, func(a, b workItem) int {
		if c := world.CompareCellCoords(a.cell, b.cell); c != 0 {
			return c
		}
		switch {
		case a.id < b.id:
			return -1
		case a.id > b.id:
			return 1
		default:
			return 0
		}
	})
}

// verifyResident resolves a work item to its live entity only while
// it is still resident under the expected cell and generation;
// legitimately removed/migrated items resolve to nil and are
// skipped, never resurrected.
func (r *registry) verifyResident(item workItem) *entity {
	coord, ok := r.entityCell[item.id]
	if !ok || coord != item.cell {
		return nil
	}
	c, ok := r.cells[coord]
	if !ok {
		return nil
	}
	ent, ok := c.entities[item.id]
	if !ok || ent.generation != item.generation {
		return nil
	}
	return ent
}
