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
// (spec §5.2.5/§5.3.7). Returning a snapshot MUST NOT allow mutation
// of live sim state: it carries values only, no pointers into the
// registry. Movement-owned authoritative state (yaw, speed, volume
// flags, reconciliation anchor) is exposed for later M4-T5/baseline
// inspection; pending-control internals stay private.
type EntitySnapshot struct {
	ID                    EntityID
	Position              world.Vec3
	Cell                  world.CellCoord
	Yaw                   uint16
	Speed                 uint8
	VolumeFlags           world.VolumeFlags
	LastProcessedInputSeq uint32
}

// entity is the M4-T1 base entity (identity, authoritative position,
// current cell, position history) extended with ONLY movement-owned
// state (spec §5.3.7). No HP, stats, combat, inventory, velocity, or
// other gameplay fields before their owning tasks. Only the single
// sim writer mutates it: no per-entity lock.
type entity struct {
	id       EntityID
	position world.Vec3
	cell     world.CellCoord
	history  *positionHistory

	yaw         uint16
	speed       uint8
	volumeFlags world.VolumeFlags

	activeHeldDirs uint8
	activeRun      bool

	hasAccepted     bool
	lastAcceptedSeq uint32
	pending         MoveIntent
	hasPending      bool

	hasProcessed     bool
	lastProcessedSeq uint32
}

// snapshot copies the entity's observable state.
func (e *entity) snapshot() EntitySnapshot {
	return EntitySnapshot{
		ID:                    e.id,
		Position:              e.position,
		Cell:                  e.cell,
		Yaw:                   e.yaw,
		Speed:                 e.speed,
		VolumeFlags:           e.volumeFlags,
		LastProcessedInputSeq: e.lastProcessedSeq,
	}
}
