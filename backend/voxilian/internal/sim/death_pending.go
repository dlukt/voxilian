package sim

import (
	"fmt"
)

// Authoritative pending-death owner state + respawn-release
// primitive (spec §9.5.1k, M5-T5c3d1): the store-independent
// runtime value the sim owner holds once an immediate death
// becomes durable, plus the owner-local transitions around
// it. `internal/sim` only: no store, persist, pgx, sqlc
// output, gateway, session, or proto import; no blocking
// work, no goroutines, no persistence, no wire behavior, no
// Portal/penalty mechanics, no pending deletion.

// PendingDeathRuntime is the authoritative owner-local
// pending-death state (spec §9.5.1k): the exact pending
// cost, death time, optional corpse identity, and
// portal-used state that became durable together with the
// death entry. A nil *PendingDeathRuntime is the ONLY
// absence encoding (no Active boolean). CorpseID == nil is
// valid even while pending remains active: the schema uses
// `pending_deaths.corpse_id ON DELETE SET NULL`, so
// pending penalties may survive corpse expiry. It
// contains NO Store revision, Saver revision,
// CharacterSnapshot, PG handle, session ID, or
// NetEntityID. Treat values as immutable: installation
// deep-freezes (including the optional CorpseID pointer)
// and inspection returns independent copies.
type PendingDeathRuntime struct {
	EffectiveCost    int
	DeathTimeSeconds int64
	CorpseID         *int64
	PortalUsed       bool
}

// ValidatePendingDeathRuntime rejects a pending-death value
// outside its binding domain before it can become live
// (spec §9.5.1k): EffectiveCost 0..100, DeathTimeSeconds
// >= 0, and a non-nil CorpseID pointing at an ID > 0.
// PortalUsed carries no constraint. A nil value is valid
// (authoritative "no pending death") and validates
// trivially. Exported so the `internal/persist` recovery
// mapper shares the exact owner domain (sim MUST NOT be
// imported by store, but persist may import sim).
func ValidatePendingDeathRuntime(p *PendingDeathRuntime) error {
	if p == nil {
		return nil
	}
	if err := ValidatePendingDeathCost(p.EffectiveCost); err != nil {
		return err
	}
	if p.DeathTimeSeconds < 0 {
		return fmt.Errorf("sim: pending death time %d: %w", p.DeathTimeSeconds, ErrInvalidDeathTime)
	}
	if p.CorpseID != nil && *p.CorpseID <= 0 {
		return fmt.Errorf("sim: pending corpse id=%d: %w", *p.CorpseID, ErrInvalidDeathInput)
	}
	return nil
}

// freezePendingDeathRuntime deep-copies a validated pending
// value into immutable private ownership (spec §9.5.1k):
// the optional CorpseID pointer is copied so later caller
// mutation of the pointed value cannot reach live sim
// state. A nil value stays nil.
func freezePendingDeathRuntime(p *PendingDeathRuntime) *PendingDeathRuntime {
	if p == nil {
		return nil
	}
	out := *p
	if p.CorpseID != nil {
		id := *p.CorpseID
		out.CorpseID = &id
	}
	return &out
}

// PlayerPendingDeathOf inspects a live entity's
// authoritative pending-death state immutably (spec
// §9.5.1k): unknown ID -> ErrEntityNotFound; a known
// generic entity -> (zero value, false, nil); a player
// with no pending death -> (zero value, false, nil); a
// player with pending death -> (independent deep COPY,
// true, nil). A migrating entity still inspects
// read-only under its quiesced source ownership.
// Inspection never mutates and stays allowed while the
// player is locked (DeathPersisting, AwaitingRespawn,
// Alive, MIGRATING).
func (e *Engine) PlayerPendingDeathOf(id EntityID) (PendingDeathRuntime, bool, error) {
	var ent *entity
	if rec, ok := e.registry.migrations[id]; ok {
		ent = rec.entity
	} else {
		var err error
		ent, err = e.registry.lookup(id)
		if err != nil {
			return PendingDeathRuntime{}, false, err
		}
	}
	if !ent.isPlayer || ent.pendingDeath == nil {
		return PendingDeathRuntime{}, false, nil
	}
	return *freezePendingDeathRuntime(ent.pendingDeath), true, nil
}

// PlayerInstallRecoveredPendingDeath is the owner-local
// recovery/hydration primitive for authoritative
// materialized-state hydration only (spec §9.5.1k, future
// T5c4 reconnect integration): it installs the exact
// recovered pending state onto a resident Alive player.
// It is NOT a general gameplay mutation.
//
// Preconditions, in this order: entity exists
// (ErrEntityNotFound); entity is a RESIDENT player
// (generic -> ErrEntityNotPlayer, MIGRATING ->
// ErrCellHandoffRequired); life state == Alive
// (ErrPlayerNotAlive — a locked player cannot be
// hydrated); no live Portal attempt outstanding
// (ErrPortalAttemptInFlight — hydration must not
// replace the pending state underneath an attempt;
// checked after the entity/life gates but before
// payload validation/install, preserving the frozen
// d1 error precedence); the supplied value validates
// (nil is a valid authoritative "no pending death"
// value). Every failure is zero mutation: an invalid
// value installs nothing and leaves the existing
// pending state unchanged. On success the frozen
// value (or nil) replaces the live pending state
// atomically. No Store read occurs here; no
// Portal/penalty mechanics.
//
// Owner-local: call only from the sim owner goroutine
// (Run/Step) or in Step-driven tests.
func (e *Engine) PlayerInstallRecoveredPendingDeath(id EntityID, pending *PendingDeathRuntime) error {
	ent, err := e.resolvePlayerAnyLife(id)
	if err != nil {
		return err
	}
	if ent.lifeState != PlayerLifeAlive {
		return fmt.Errorf("%w: id %d life %d", ErrPlayerNotAlive, uint64(id), uint8(ent.lifeState))
	}
	if ent.portalInFlight {
		return fmt.Errorf("%w: id %d portal attempt in flight", ErrPortalAttemptInFlight, uint64(id))
	}
	if err := ValidatePendingDeathRuntime(pending); err != nil {
		return err
	}
	frozen := freezePendingDeathRuntime(pending)
	ent.pendingDeath = frozen
	return nil
}

// RespawnReleaseDisposition is the PlayerReleaseRespawn
// outcome. Applied vs Duplicate are ordinary results, not
// errors; on error the disposition is meaningless — check
// err first.
type RespawnReleaseDisposition uint8

const (
	// RespawnReleaseApplied means the exact current token
	// released the player from AwaitingRespawn to Alive,
	// preserving every other live state bit (including
	// active pending death).
	RespawnReleaseApplied RespawnReleaseDisposition = iota
	// RespawnReleaseDuplicate means the token exactly
	// matches the already-released attempt while Alive: a
	// retransmitted release answered with zero mutation.
	RespawnReleaseDuplicate
)

// PlayerReleaseRespawn is the owner-local gameplay-release
// primitive (spec §9.5.1k, future T5c4 composition): it
// moves a respawned player from AwaitingRespawn back to
// ordinary Alive gameplay. It is DISTINCT from Underworld
// LeaveHold / ApplyDeathPenalties / pending deletion /
// Portal: for an Underworld-bound death the pending state
// stays bit-identical across the release, and the player
// participates in ordinary Underworld gameplay while the
// delayed-death phase remains outstanding. T5c3d3 later
// consumes pending ONLY when the game reports the player
// actually leaving the Underworld.
//
// Token/entity/lifecycle validation: a MIGRATING entity
// keeps ErrCellHandoffRequired; an unknown EntityID
// reports ErrEntityNotFound (preserving the removal/ABA
// rule with no CharacterID-only fallback lookup); a
// generic entity, a wrong CharacterID, a wrong/zero
// epoch, or life == DeathPersisting yields
// ErrDeathAttemptMismatch. Every failure is zero
// mutation.
//
// First release (exact current token with life ==
// AwaitingRespawn) sets life -> Alive and NOTHING ELSE:
// position, vitals, runtime, durable, pendingDeath,
// deathEpoch, lastDeathSeconds, history, and identity
// are preserved. Duplicate release (token exactly
// matching the entity's current nonzero death attempt
// while life == Alive) returns RespawnReleaseDuplicate
// with nil error and zero mutation: retransmitted
// release is safe.
//
// Owner-local: call only from the sim owner goroutine
// (Run/Step) or in Step-driven tests.
func (e *Engine) PlayerReleaseRespawn(token DeathAttemptToken) (EntitySnapshot, RespawnReleaseDisposition, error) {
	if _, migrating := e.registry.migrations[token.EntityID]; migrating {
		return EntitySnapshot{}, RespawnReleaseApplied, fmt.Errorf("%w: id %d migrating", ErrCellHandoffRequired, uint64(token.EntityID))
	}
	ent, err := e.registry.lookup(token.EntityID)
	if err != nil {
		return EntitySnapshot{}, RespawnReleaseApplied, err
	}
	if !ent.isPlayer {
		return EntitySnapshot{}, RespawnReleaseApplied, fmt.Errorf("%w: id %d not a player", ErrDeathAttemptMismatch, uint64(token.EntityID))
	}
	if token.CharacterID != ent.characterID {
		return EntitySnapshot{}, RespawnReleaseApplied, fmt.Errorf("%w: id %d character %d vs live %d", ErrDeathAttemptMismatch, uint64(token.EntityID), int64(token.CharacterID), int64(ent.characterID))
	}
	if token.Epoch == 0 || token.Epoch != ent.deathEpoch {
		return EntitySnapshot{}, RespawnReleaseApplied, fmt.Errorf("%w: id %d epoch %d vs live %d", ErrDeathAttemptMismatch, uint64(token.EntityID), token.Epoch, ent.deathEpoch)
	}
	switch ent.lifeState {
	case PlayerLifeAwaitingRespawn:
		ent.lifeState = PlayerLifeAlive
		return ent.snapshot(), RespawnReleaseApplied, nil
	case PlayerLifeAlive:
		return ent.snapshot(), RespawnReleaseDuplicate, nil
	default:
		return EntitySnapshot{}, RespawnReleaseApplied, fmt.Errorf("%w: id %d life %d", ErrDeathAttemptMismatch, uint64(token.EntityID), uint8(ent.lifeState))
	}
}
