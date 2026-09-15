package sim

import (
	"fmt"

	"github.com/dlukt/voxilian/internal/world"
)

// Death-safe runtime transition primitives (spec §9.5.1e, M5-T5c3b):
// owner-local-only building blocks that T5c3c/T5c3d later compose
// into the async death lifecycle. `internal/sim` only: no store,
// persist, pgx, sqlc output, gateway, session, or proto import; no
// blocking work, no goroutines, no persistence, no wire behavior.
//
// Two distinct concepts, never combined with persistence:
//
//  1. PlayerQuiesceForDeath stops current movement and cancels the
//     ephemeral health/mana/rest deadlines while the async death
//     operation is outstanding. It is a primitive, NOT the
//     long-lived lifecycle gate: no PlayerLifeState, no
//     dead/persisting enums, no permanent movement rejection, no
//     async-operation ownership (T5c3c owns that state machine).
//
//  2. PlayerInstallPostDeathState installs already-accepted
//     authoritative post-death state (trusted resolved explicit
//     remap, NOT ordinary movement) after it became authoritative
//     via successful critical persistence OR authoritative
//     materialized-state reconciliation. T5c3b itself cannot verify
//     PostgreSQL acceptance.
//
// Source fidelity (meridian59.md §9.5, player.kod): real death calls
// NewHealth/NewMana/NewVigor, but NewVigor only bounds piVigor to
// 1..viMax_vigor and redraws Vigor — it creates NO rest/vigor
// timer. Post-death runtime reinitialization therefore recreates
// the health deadline per NewHealth and the mana deadline per
// NewMana, while rest REMAINS ABSENT: PlayerStartResting is never
// called here.

// PlayerQuiesceForDeath stops a resident player's current movement
// and cancels its ephemeral runtime deadlines (spec §9.5.1e).
// Resolution: unknown EntityID -> ErrEntityNotFound; generic
// entity -> ErrEntityNotPlayer; MIGRATING entity -> the existing
// ErrCellHandoffRequired. All failures are zero mutation.
//
// Successful quiesce performs only ephemeral runtime mutation:
// activeHeldDirs = 0, activeRun = false, speed = 0, the pending
// move discarded, all three deadline slots disarmed with canonical
// zero dues, actedSinceEntry = false. It PRESERVES yaw, the
// accepted/processed sequence anchors (pre-death input sequences
// MUST NOT become valid again after respawn), EntityID,
// CharacterID, position, cell, ownership generation, current
// vitals, current runtime inputs, the stomach anchor, position
// history, and the recent OpID dedupe cache. It emits no
// PlayerVitalsObserver event. A second call on an otherwise
// unchanged resident player is an exact idempotent no-op.
//
// Owner-local: call only from the sim owner goroutine (Run/Step)
// or in Step-driven tests.
func (e *Engine) PlayerQuiesceForDeath(id EntityID) error {
	ent, err := e.resolvePlayer(id)
	if err != nil {
		return err
	}
	ent.activeHeldDirs = 0
	ent.activeRun = false
	ent.speed = 0
	ent.hasPending = false
	ent.pending = MoveIntent{}
	ent.healthArmed = false
	ent.healthDue = 0
	ent.manaArmed = false
	ent.manaDue = 0
	ent.restArmed = false
	ent.restDue = 0
	ent.actedSinceEntry = false
	return nil
}

// PlayerInstallPostDeathState installs already-resolved
// authoritative post-death state onto a resident player entity
// (spec §9.5.1e): a trusted resolved explicit remap to placement
// carrying the supplied validated vitals and runtime inputs.
//
// Binding caller contract: T5c3c/T5c3d may call this only after
// the supplied state has become authoritative via successful
// critical persistence OR authoritative materialized-state
// reconciliation. This function performs no world-content lookup,
// no newbie/Underworld decision, no T5a calculation, no
// persistence check, and no Store call.
//
// Before ANY mutation it validates: placement is valid
// world.CellForPosition input; vitals.Validate();
// runtimeInputs.Validate(); entity exists; entity is a player;
// entity is RESIDENT, not MIGRATING. Any validation/resolution
// error leaves bit-identical position/cell/generation, movement
// state, vitals, runtime metadata, history, CharacterID binding,
// and recent OpIDs: no partial install is allowed.
//
// Relocation is NOT ordinary movement: no walk/run integration,
// no path through intermediate positions, no SolidAt
// movement-collision validation, no coordinate clamp/snap — only
// normal world-position validity applies, and destination
// VolumeFlags are re-sampled via VolumeFlagsAt after relocation.
// Same-cell placement changes position directly with generation
// unchanged; cross-cell placement reuses the EXISTING ownership
// machinery (beginHandoff + commitHandoff): same entity object,
// same EntityID, same CharacterID, generation exactly +1,
// destination resident at completion, no remove/re-add. A
// begin failure (including ownership-generation exhaustion)
// mutates nothing; an unexpected commit failure aborts the
// handoff (exact source ownership/position restored) and
// installs nothing.
//
// A death remap is NOT entity recreation: EntityID,
// CharacterID, the recent OpID dedupe cache, yaw, and the
// accepted/processed sequence anchors are preserved; only
// current movement control is cleared. The retained
// position-history ring is cleared (capacity preserved) with no
// synthesized sample, so History is empty immediately after
// install; the NEXT normal Engine.Step appends the first
// destination-side sample.
//
// The supplied vitals install as the complete authoritative live
// value (no re-derivation, no T5a rerun, no deltas) with NO
// PlayerVitalsObserver event: the state was already accepted by
// the critical death persistence/recovery path, and re-emitting
// it could schedule a redundant second persistence write (T5c4
// owns wire presentation). The ephemeral runtime then rebuilds
// from a clean state at the current Engine tick — supplied
// runtime inputs, actedSinceEntry false, stomach anchored now,
// health/mana created from the current tick per the existing
// T4b2 semantics, rest ABSENT (NewVigor creates no rest timer).
// The single-owner model is the atomicity boundary: no
// intermediate public state exists.
//
// Owner-local: call only from the sim owner goroutine (Run/Step)
// or in Step-driven tests.
func (e *Engine) PlayerInstallPostDeathState(id EntityID, placement world.Vec3, vitals PlayerVitals, runtimeInputs PlayerVitalsRuntimeInputs) (EntitySnapshot, error) {
	dest, err := world.CellForPosition(placement)
	if err != nil {
		return EntitySnapshot{}, fmt.Errorf("%w: %w", ErrInvalidPosition, err)
	}
	if err := vitals.Validate(); err != nil {
		return EntitySnapshot{}, err
	}
	if err := runtimeInputs.Validate(); err != nil {
		return EntitySnapshot{}, err
	}
	ent, err := e.resolvePlayer(id)
	if err != nil {
		return EntitySnapshot{}, err
	}
	if dest == ent.cell {
		ent.position = placement
	} else {
		from := OwnerRef{Cell: ent.cell, Generation: ent.generation}
		tok, err := e.registry.beginHandoff(ent.id, from, dest, placement)
		if err != nil {
			return EntitySnapshot{}, err
		}
		if _, err := e.registry.commitHandoff(tok); err != nil {
			// Practically unreachable locally (the token was just
			// issued): roll back rather than strand the entity
			// mid-migration, then report with zero install
			// mutation. Abort itself is the complete rollback
			// primitive — source ownership, exact source
			// position, and queued controls are restored.
			e.registry.abortHandoff(ent.id)
			return EntitySnapshot{}, err
		}
	}
	ent.volumeFlags = e.collision.VolumeFlagsAt(placement)
	ent.vitals = vitals
	ent.activeHeldDirs = 0
	ent.activeRun = false
	ent.speed = 0
	ent.hasPending = false
	ent.pending = MoveIntent{}
	ent.history.Reset()
	e.resetPostDeathRuntime(ent, runtimeInputs)
	return ent.snapshot(), nil
}

// resetPostDeathRuntime rebuilds a player's ephemeral runtime from
// a clean state at the current Engine tick (spec §9.5.1e): old
// health/mana due ticks are discarded, rest is forced absent with
// a canonical zero due, and the shared T4b2 initialization helper
// then installs the supplied inputs, clears actedSinceEntry,
// anchors the stomach now, and creates health/mana deadlines from
// the current tick iff HP != MaxHP && HP > 0 / Mana != MaxMana.
// Rest is NEVER auto-armed: source NewVigor only bounds/draws
// Vigor and starts no rest timer.
func (e *Engine) resetPostDeathRuntime(ent *entity, inputs PlayerVitalsRuntimeInputs) {
	ent.healthArmed = false
	ent.healthDue = 0
	ent.manaArmed = false
	ent.manaDue = 0
	ent.restArmed = false
	ent.restDue = 0
	e.initPlayerRuntime(ent, inputs)
}
