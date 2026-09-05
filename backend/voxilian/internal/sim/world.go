package sim

import (
	"fmt"
	"slices"

	"github.com/dlukt/voxilian/internal/world"
)

// cell owns its entity collection (spec §5.2.5). Only the single engine
// simulation loop mutates it: no mutex, no per-cell goroutine. The
// mutable map is never exported.
type cell struct {
	coord    world.CellCoord
	entities map[EntityID]*entity
}

// registry is the active-cell/entity container (spec §5.2.5/§5.4):
// nextEntityID allocates fresh opaque IDs, cells holds the active
// sim ownership set, entityCell locates every RESIDENT entity, and
// migrations holds every MIGRATING entity's record (which owns the
// quiesced entity object — never a second copy). Every live EntityID
// has exactly one route: resident XOR migrating. No duplicate state
// structures may diverge: the cell map owns resident entities, the
// locator only records which cell owns each ID.
type registry struct {
	nextEntityID    EntityID
	cells           map[world.CellCoord]*cell
	entityCell      map[EntityID]world.CellCoord
	migrations      map[EntityID]*migrationRecord
	historyCapacity int
}

// newRegistry builds an empty registry with the fixed per-entity
// history capacity (2 * tickHz). Capacity MUST be positive.
func newRegistry(historyCapacity int) *registry {
	if historyCapacity <= 0 {
		panic(fmt.Sprintf("sim: newRegistry with non-positive history capacity %d", historyCapacity))
	}
	return &registry{
		nextEntityID:    InvalidEntityID + 1,
		cells:           make(map[world.CellCoord]*cell),
		entityCell:      make(map[EntityID]world.CellCoord),
		migrations:      make(map[EntityID]*migrationRecord),
		historyCapacity: historyCapacity,
	}
}

// AddEntity validates the position, computes the cell, allocates a
// fresh EntityID, lazily creates the active cell, inserts the entity,
// and updates the global locator — all-or-nothing (spec §5.2.5). An
// invalid position allocates NO ID and mutates nothing. Position
// validation runs BEFORE ID inspection/allocation, so an invalid
// position never advances or observes allocator state. Once
// math.MaxUint64 has been issued, nextEntityID wraps to the reserved
// zero value, which is the private exhausted sentinel: further adds
// fail with ErrEntityIDExhausted (zero is never issued as an ID) and
// mutate nothing.
func (r *registry) AddEntity(pos world.Vec3) (EntitySnapshot, error) {
	coord, err := world.CellForPosition(pos)
	if err != nil {
		return EntitySnapshot{}, fmt.Errorf("%w: %w", ErrInvalidPosition, err)
	}
	if r.nextEntityID == InvalidEntityID {
		return EntitySnapshot{}, fmt.Errorf("%w: allocator exhausted", ErrEntityIDExhausted)
	}
	id := r.nextEntityID
	c, ok := r.cells[coord]
	if !ok {
		c = &cell{coord: coord, entities: make(map[EntityID]*entity)}
		r.cells[coord] = c
	}
	// Movement state starts zeroed: yaw 0, speed 0, no accepted or
	// processed input, zero held control (spec §5.3.7). Ownership
	// starts at Generation 1 (spec §5.4.1). Volume flags default to
	// VolumeNone here; Engine.AddEntity samples the real collision
	// world at the initial position afterwards.
	e := &entity{
		id:         id,
		position:   pos,
		cell:       coord,
		history:    newPositionHistory(r.historyCapacity),
		generation: 1,
	}
	c.entities[id] = e
	r.entityCell[id] = coord
	r.nextEntityID++
	return e.snapshot(), nil
}

// RemoveEntity deletes the entity from its owning cell, deletes the
// global locator entry, and discards its history with it (spec
// §5.2.5/§5.2.6). A migrating entity is removed from its migration
// record instead (queue and history dropped with it). Empty resident
// cells are removed: T1 cells represent active sim ownership, not
// terrain storage. Unknown IDs return ErrEntityNotFound. IDs are
// never reused.
func (r *registry) RemoveEntity(id EntityID) error {
	if rec, ok := r.migrations[id]; ok {
		_ = rec
		delete(r.migrations, id)
		return nil
	}
	coord, ok := r.entityCell[id]
	if !ok {
		return fmt.Errorf("%w: id %d", ErrEntityNotFound, uint64(id))
	}
	c, ok := r.cells[coord]
	if !ok {
		return fmt.Errorf("%w: id %d (locator without cell)", ErrEntityNotFound, uint64(id))
	}
	if _, ok := c.entities[id]; !ok {
		return fmt.Errorf("%w: id %d (cell without entity)", ErrEntityNotFound, uint64(id))
	}
	delete(c.entities, id)
	delete(r.entityCell, id)
	if len(c.entities) == 0 {
		delete(r.cells, coord)
	}
	return nil
}

// SetPosition updates an entity's authoritative position when the
// destination belongs to the SAME cell (spec §5.2.5). A destination in
// another cell returns ErrCellHandoffRequired with ZERO mutation —
// M4-T3a owns real handoff and T1 MUST NOT silently move entities
// between cell maps.
func (r *registry) SetPosition(id EntityID, pos world.Vec3) error {
	if _, ok := r.migrations[id]; ok {
		// Ownership is in flux; manual position mutation cannot apply.
		// Movement handoff uses its dedicated ownership path.
		return fmt.Errorf("%w: id %d migrating", ErrCellHandoffRequired, uint64(id))
	}
	coord, ok := r.entityCell[id]
	if !ok {
		return fmt.Errorf("%w: id %d", ErrEntityNotFound, uint64(id))
	}
	dest, err := world.CellForPosition(pos)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidPosition, err)
	}
	if dest != coord {
		return fmt.Errorf("%w: %v -> %v", ErrCellHandoffRequired, coord, dest)
	}
	c, ok := r.cells[coord]
	if !ok {
		return fmt.Errorf("%w: id %d (locator without cell)", ErrEntityNotFound, uint64(id))
	}
	e, ok := c.entities[id]
	if !ok {
		return fmt.Errorf("%w: id %d (cell without entity)", ErrEntityNotFound, uint64(id))
	}
	e.position = pos
	return nil
}

// Entity returns an immutable snapshot copy (spec §5.2.5). A
// migrating entity still inspects (read-only) under its quiesced
// source ownership; inspection never mutates route state.
func (r *registry) Entity(id EntityID) (EntitySnapshot, error) {
	if rec, ok := r.migrations[id]; ok {
		return rec.entity.snapshot(), nil
	}
	coord, ok := r.entityCell[id]
	if !ok {
		return EntitySnapshot{}, fmt.Errorf("%w: id %d", ErrEntityNotFound, uint64(id))
	}
	c, ok := r.cells[coord]
	if !ok {
		return EntitySnapshot{}, fmt.Errorf("%w: id %d (locator without cell)", ErrEntityNotFound, uint64(id))
	}
	e, ok := c.entities[id]
	if !ok {
		return EntitySnapshot{}, fmt.Errorf("%w: id %d (cell without entity)", ErrEntityNotFound, uint64(id))
	}
	return e.snapshot(), nil
}

// CellCoords lists active cells in canonical deterministic order
// (X ascending, then Z ascending). The result is a fresh slice.
func (r *registry) CellCoords() []world.CellCoord {
	out := make([]world.CellCoord, 0, len(r.cells))
	for coord := range r.cells {
		out = append(out, coord)
	}
	world.SortCellCoords(out)
	return out
}

// EntitiesInCell lists one cell's entities as snapshots in EntityID
// ascending order (spec §5.2.5). Unknown cells return an empty slice.
// The result is a fresh slice of copies.
func (r *registry) EntitiesInCell(coord world.CellCoord) []EntitySnapshot {
	c, ok := r.cells[coord]
	if !ok {
		return nil
	}
	ids := make([]EntityID, 0, len(c.entities))
	for id := range c.entities {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	out := make([]EntitySnapshot, 0, len(ids))
	for _, id := range ids {
		out = append(out, c.entities[id].snapshot())
	}
	return out
}

// History returns a copy of the entity's retained position samples,
// oldest -> newest. Removing the entity removes its history. The
// quiesced migration record preserves history across handoff.
func (r *registry) History(id EntityID) ([]PositionSample, error) {
	if rec, ok := r.migrations[id]; ok {
		return rec.entity.history.Samples(), nil
	}
	coord, ok := r.entityCell[id]
	if !ok {
		return nil, fmt.Errorf("%w: id %d", ErrEntityNotFound, uint64(id))
	}
	c, ok := r.cells[coord]
	if !ok {
		return nil, fmt.Errorf("%w: id %d (locator without cell)", ErrEntityNotFound, uint64(id))
	}
	e, ok := c.entities[id]
	if !ok {
		return nil, fmt.Errorf("%w: id %d (cell without entity)", ErrEntityNotFound, uint64(id))
	}
	return e.history.Samples(), nil
}

// EntityCount reports the number of live entities, resident and
// migrating alike (order-independent): a migrating entity is never
// transiently lost.
func (r *registry) EntityCount() int { return len(r.entityCell) + len(r.migrations) }

// CellCount reports the number of active cells (order-independent).
func (r *registry) CellCount() int { return len(r.cells) }

// lookup returns the live entity pointer for engine-internal mutation.
// Callers MUST be the single sim writer; the pointer MUST NOT escape to
// gateway callers.
func (r *registry) lookup(id EntityID) (*entity, error) {
	coord, ok := r.entityCell[id]
	if !ok {
		return nil, fmt.Errorf("%w: id %d", ErrEntityNotFound, uint64(id))
	}
	c, ok := r.cells[coord]
	if !ok {
		return nil, fmt.Errorf("%w: id %d (locator without cell)", ErrEntityNotFound, uint64(id))
	}
	e, ok := c.entities[id]
	if !ok {
		return nil, fmt.Errorf("%w: id %d (cell without entity)", ErrEntityNotFound, uint64(id))
	}
	return e, nil
}
