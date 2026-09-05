package sim

import (
	"github.com/dlukt/voxilian/internal/world"
)

// EntityID is the opaque internal sim entity identity (spec §5.2.5).
//
//   - 0 is invalid/reserved and never issued.
//   - The first allocated ID is 1; allocation is monotonic and never
//     reused during one engine lifetime (natural uint64 space).
//
// This ID is NOT a PostgreSQL durable ID, NOT a session-local
// NetEntityID, and NOT sent on the wire. Later systems may associate
// durable character/item identity and per-session NetEntityIDs with it
// without overloading those domains.
type EntityID uint64

// InvalidEntityID is the reserved zero value; it never names an entity.
const InvalidEntityID EntityID = 0

// EntitySnapshot is the immutable inspection copy of a live entity
// (spec §5.2.5). Returning a snapshot MUST NOT allow mutation of live
// sim state: it carries values only, no pointers into the registry.
type EntitySnapshot struct {
	ID       EntityID
	Position world.Vec3
	Cell     world.CellCoord
}

// entity is the minimal M4-T1 base entity: identity, authoritative
// position, current cell, and position history (spec §5.2.5). No HP,
// stats, combat, inventory, velocity, or movement input before their
// owning tasks.
type entity struct {
	id       EntityID
	position world.Vec3
	cell     world.CellCoord
	history  *positionHistory
}

// snapshot copies the entity's observable state.
func (e *entity) snapshot() EntitySnapshot {
	return EntitySnapshot{
		ID:       e.id,
		Position: e.position,
		Cell:     e.cell,
	}
}
