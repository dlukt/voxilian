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
// inspection; pending-control internals stay private. IsPlayer is the
// M5-T4b1 classification flag (spec §9.4b.2): false for every generic
// M4 entity. The vitals VALUE itself is inspected separately
// (PlayerVitalsOf) so a valid player with zero-valued vitals fields is
// never confused with a non-player.
type EntitySnapshot struct {
	ID                    EntityID
	Position              world.Vec3
	Cell                  world.CellCoord
	Yaw                   uint16
	Speed                 uint8
	VolumeFlags           world.VolumeFlags
	LastProcessedInputSeq uint32
	// OwnershipGeneration is the current {cell,generation} epoch's
	// generation (spec §5.4.1): 1 at creation, +1 per handoff.
	OwnershipGeneration uint64
	// IsPlayer reports the player-entity classification (spec §9.4b.2).
	IsPlayer bool
}

// entity is the M4-T1 base entity (identity, authoritative position,
// current cell, position history) extended with ONLY movement-owned
// state (spec §5.3.7) and player-owned vitals state (spec §9.4b.2).
// No combat, inventory, velocity, or other gameplay fields before their
// owning tasks. Only the single sim writer mutates it: no per-entity
// lock.
type entity struct {
	id       EntityID
	position world.Vec3
	cell     world.CellCoord
	history  *positionHistory

	// generation is the ownership epoch generation (spec §5.4.1):
	// reserved 0 is never stored on a live entity.
	generation uint64

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

	// isPlayer marks a player entity carrying authoritative vitals
	// (spec §9.4b.2); vitals is meaningful only when isPlayer holds.
	// The value copy rules of §9.4b.3 apply: attach stores a copy and
	// inspection returns copies, so no caller can alias live state.
	isPlayer bool
	vitals   PlayerVitals

	// Player-owned ephemeral vitals runtime metadata (spec §9.4b.10,
	// v0.3.31): installed atomically with the vitals at attach/add
	// (§9.4b.3a), never persisted (§9.4b.9), and riding this SAME
	// entity object through cell handoff (§9.4b.8). runtimeInputs is
	// the authoritative current resolved regen-input snapshot
	// (§9.4b.14a). Each deadline slot is an explicit armed bit plus a
	// u32 due tick: due 0 is a VALID wrapped deadline, so absence is
	// NEVER encoded as due == 0. restArmed IS the resting state — no
	// redundant independent resting boolean exists. No time.Time, no
	// timer handle, no goroutine.
	runtimeInputs     PlayerVitalsRuntimeInputs
	healthArmed       bool
	healthDue         uint32
	manaArmed         bool
	manaDue           uint32
	restArmed         bool
	restDue           uint32
	actedSinceEntry   bool
	stomachAnchorTick uint32

	// recentOps is the bounded cross-cell dedupe cache
	// (spec §5.5.15): the most recent RecentOpIDCapacity
	// SUCCESSFULLY APPLIED OpIDs for this entity. Nil until the
	// first successful cross-cell apply (lazy: entities that
	// never receive cross-cell operations allocate nothing).
	// The cache travels with this same entity object across
	// handoff — it is never reset or copied — and is discarded
	// with the entity on removal. No snapshot, wire, or DB
	// representation: ephemeral bounded dedupe only.
	recentOps *recentOpIDs
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
		OwnershipGeneration:   e.generation,
		IsPlayer:              e.isPlayer,
	}
}
