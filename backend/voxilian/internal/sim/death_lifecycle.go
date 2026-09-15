package sim

import (
	"fmt"
	"math"

	"github.com/dlukt/voxilian/internal/world"
)

// Immediate-death lifecycle gate + attempt correlation
// (spec §9.5.1f, M5-T5c3c1): the owner-local state machine that
// prevents new gameplay from resuming while async death
// persistence is outstanding. `internal/sim` only: no store,
// persist, pgx, sqlc output, gateway, session, or proto import; no
// blocking work, no goroutines, no persistence, no wire behavior,
// no T5a plan composition, no completion ingress, no zero-HP
// automatic dispatch. It composes the two T5c3b primitives
// (PlayerQuiesceForDeath + PlayerInstallPostDeathState) and adds
// ONLY the life-state gate and the attempt-correlation token. The
// complete immutable persistence-content capture (T5c3c2) and the
// bounded off-owner execution (T5c3c3) are explicitly out of scope.

// PlayerLifeState is the ephemeral owner-local player life state
// (spec §9.5.1f). The zero value is PlayerLifeAlive, so every
// successfully created/attached player starts Alive with no extra
// initialization; generic entities carry the zero value, which is
// meaningless for them. Never persisted, never sent on the wire.
// Character handoff preserves it (the SAME entity object moves);
// removal discards it with the entity.
type PlayerLifeState uint8

const (
	// PlayerLifeAlive is normal gameplay: mutations/input accepted.
	PlayerLifeAlive PlayerLifeState = iota
	// PlayerLifeDeathPersisting means a real death has been
	// owner-accepted for async persistence: the entity is
	// quiesced and critical persistence/recovery has not yet been
	// accepted back by the owner.
	PlayerLifeDeathPersisting
	// PlayerLifeAwaitingRespawn means authoritative post-death
	// placement/vitals have been installed but the
	// transport/gameplay respawn-release phase (T5c4/T5c3d) has not
	// yet completed. T5c3c1 provides NO transition out of it.
	PlayerLifeAwaitingRespawn
)

// DeathAttemptToken correlates one immediate-death persistence
// attempt with its completion (spec §9.5.1f): a plain immutable
// value naming the entity, its live CharacterID, and the
// per-entity ephemeral death epoch after increment. It is NOT an
// OpID, NetEntityID, Store revision, or session ID. It exists
// solely so a late/duplicate off-owner completion can never apply
// to the wrong death attempt.
type DeathAttemptToken struct {
	EntityID    EntityID
	CharacterID CharacterID
	Epoch       uint64
}

// DeathCompletionDisposition is the PlayerAcceptPostDeathState
// outcome. Applied vs Duplicate are ordinary results, not errors;
// on error the disposition is meaningless — check err first.
type DeathCompletionDisposition uint8

const (
	// DeathCompletionApplied means the first valid completion for
	// the current attempt installed post-death state and moved the
	// player to AwaitingRespawn.
	DeathCompletionApplied DeathCompletionDisposition = iota
	// DeathCompletionDuplicate means the token exactly matches the
	// already-applied attempt while AwaitingRespawn: a
	// retry/redelivery answered with zero mutation.
	DeathCompletionDuplicate
)

// resolvePlayerAnyLife resolves the live RESIDENT player entity
// regardless of PlayerLifeState (spec §9.5.1f): MIGRATING
// ownership fails with zero mutation (ErrCellHandoffRequired,
// mirroring SetPosition), unknown IDs report ErrEntityNotFound,
// and generic entities ErrEntityNotPlayer. T5c3b lifecycle
// primitives and the T5c3c1 begin/completion transitions use this
// path; ordinary gameplay mutation uses resolveActivePlayer.
func (e *Engine) resolvePlayerAnyLife(id EntityID) (*entity, error) {
	if _, migrating := e.registry.migrations[id]; migrating {
		return nil, fmt.Errorf("%w: id %d migrating", ErrCellHandoffRequired, uint64(id))
	}
	ent, err := e.registry.lookup(id)
	if err != nil {
		return nil, err
	}
	if !ent.isPlayer {
		return nil, fmt.Errorf("%w: id %d", ErrEntityNotPlayer, uint64(id))
	}
	return ent, nil
}

// resolveActivePlayer resolves the ordinary gameplay-mutable
// Alive player (spec §9.5.1f): the resident-player resolution plus
// the life-state gate. A DeathPersisting or AwaitingRespawn player
// reports ErrPlayerNotAlive with zero mutation. Every ordinary
// owner-local Player* gameplay mutation/input path MUST resolve
// through here so no family accidentally bypasses the gate.
func (e *Engine) resolveActivePlayer(id EntityID) (*entity, error) {
	ent, err := e.resolvePlayerAnyLife(id)
	if err != nil {
		return nil, err
	}
	if ent.lifeState != PlayerLifeAlive {
		return nil, fmt.Errorf("%w: id %d life %d", ErrPlayerNotAlive, uint64(id), uint8(ent.lifeState))
	}
	return ent, nil
}

// PlayerLifeStateOf inspects a live entity's player life state
// immutably (spec §9.5.1f): unknown ID -> ErrEntityNotFound; a
// known generic entity -> (zero value, false, nil); a player ->
// (current state, true, nil). A migrating entity still inspects
// read-only under its quiesced source ownership. Inspection never
// mutates and stays allowed while the player is locked.
func (e *Engine) PlayerLifeStateOf(id EntityID) (PlayerLifeState, bool, error) {
	if rec, ok := e.registry.migrations[id]; ok {
		if !rec.entity.isPlayer {
			return PlayerLifeAlive, false, nil
		}
		return rec.entity.lifeState, true, nil
	}
	ent, err := e.registry.lookup(id)
	if err != nil {
		return PlayerLifeAlive, false, err
	}
	if !ent.isPlayer {
		return PlayerLifeAlive, false, nil
	}
	return ent.lifeState, true, nil
}

// PlayerBeginDeathPersistence is the owner-local begin-death
// transition (spec §9.5.1f): it accepts a resident zero-HP Alive
// player for async persistence. This is NOT the T5a planner or a
// zero-HP automatic dispatch — the caller has already decided this
// is a real death.
//
// Preconditions, in this order: entity exists (ErrEntityNotFound);
// entity is a RESIDENT player (generic -> ErrEntityNotPlayer,
// MIGRATING -> ErrCellHandoffRequired); life state == Alive
// (ErrPlayerNotAlive); vitals.HP == 0 (ErrPlayerNotDead); death
// epoch can increment (ErrDeathAttemptExhausted at MaxUint64).
// Every failure is zero mutation: no quiesce, no epoch increment,
// no life change.
//
// On success, in the SAME owner turn: invoke the existing
// PlayerQuiesceForDeath semantics (preserving every T5c3b quiesce
// invariant), increment the death epoch exactly once, set life
// state = DeathPersisting, and return the exact token. No
// persistence call, no goroutine, no sink invocation, no T5a
// planner, no wall clock.
//
// Owner-local: call only from the sim owner goroutine (Run/Step)
// or in Step-driven tests.
func (e *Engine) PlayerBeginDeathPersistence(id EntityID) (DeathAttemptToken, error) {
	ent, err := e.resolvePlayerAnyLife(id)
	if err != nil {
		return DeathAttemptToken{}, err
	}
	if ent.lifeState != PlayerLifeAlive {
		return DeathAttemptToken{}, fmt.Errorf("%w: id %d life %d", ErrPlayerNotAlive, uint64(id), uint8(ent.lifeState))
	}
	if ent.vitals.HP != 0 {
		return DeathAttemptToken{}, fmt.Errorf("%w: id %d hp %d", ErrPlayerNotDead, uint64(id), ent.vitals.HP)
	}
	if ent.deathEpoch == math.MaxUint64 {
		return DeathAttemptToken{}, fmt.Errorf("%w: id %d", ErrDeathAttemptExhausted, uint64(id))
	}
	// Preconditions hold for a resident player, so the primitive
	// cannot fail here; propagate defensively with zero
	// lifecycle mutation on the unreachable path.
	if err := e.PlayerQuiesceForDeath(id); err != nil {
		return DeathAttemptToken{}, err
	}
	ent.deathEpoch++
	ent.lifeState = PlayerLifeDeathPersisting
	return DeathAttemptToken{
		EntityID:    ent.id,
		CharacterID: ent.characterID,
		Epoch:       ent.deathEpoch,
	}, nil
}

// PlayerAcceptPostDeathState is the correlated owner-local
// post-death completion/install wrapper (spec §9.5.1f) and the
// future T5c3c3 typed-completion target. It MUST NOT perform
// persistence: the supplied placement/vitals/runtime inputs are
// already authoritative (accepted by critical persistence or
// materialized-state reconciliation) by caller contract.
//
// Validation: the entity named by the token exists
// (ErrEntityNotFound, preserving the removal/ABA rule with no
// CharacterID-only fallback lookup); a MIGRATING entity keeps the
// existing ErrCellHandoffRequired; a generic entity, a wrong
// CharacterID, a wrong epoch, or an incompatible life state yields
// ErrDeathAttemptMismatch. Every failure is zero mutation.
//
// First valid completion (life == DeathPersisting) calls the
// EXISTING PlayerInstallPostDeathState: on success life state ->
// AwaitingRespawn with the installed EntitySnapshot and
// DeathCompletionApplied; on install failure life state REMAINS
// DeathPersisting, the token REMAINS current/valid, the entity
// remains in the T5c3b guaranteed rollback/quiesced state, and the
// install error returns (never a silent transition back Alive).
//
// Duplicate completion (same EntityID/CharacterID/epoch with life
// == AwaitingRespawn) is a retry/redelivery of the
// already-applied attempt: return DeathCompletionDuplicate with
// the current EntitySnapshot and nil error with ABSOLUTELY ZERO
// mutation.
//
// Owner-local: call only from the sim owner goroutine (Run/Step)
// or in Step-driven tests.
func (e *Engine) PlayerAcceptPostDeathState(token DeathAttemptToken, placement world.Vec3, vitals PlayerVitals, runtimeInputs PlayerVitalsRuntimeInputs) (EntitySnapshot, DeathCompletionDisposition, error) {
	if _, migrating := e.registry.migrations[token.EntityID]; migrating {
		return EntitySnapshot{}, DeathCompletionApplied, fmt.Errorf("%w: id %d migrating", ErrCellHandoffRequired, uint64(token.EntityID))
	}
	ent, err := e.registry.lookup(token.EntityID)
	if err != nil {
		return EntitySnapshot{}, DeathCompletionApplied, err
	}
	if !ent.isPlayer {
		return EntitySnapshot{}, DeathCompletionApplied, fmt.Errorf("%w: id %d not a player", ErrDeathAttemptMismatch, uint64(token.EntityID))
	}
	if token.CharacterID != ent.characterID {
		return EntitySnapshot{}, DeathCompletionApplied, fmt.Errorf("%w: id %d character %d vs live %d", ErrDeathAttemptMismatch, uint64(token.EntityID), int64(token.CharacterID), int64(ent.characterID))
	}
	if token.Epoch == 0 || token.Epoch != ent.deathEpoch {
		return EntitySnapshot{}, DeathCompletionApplied, fmt.Errorf("%w: id %d epoch %d vs live %d", ErrDeathAttemptMismatch, uint64(token.EntityID), token.Epoch, ent.deathEpoch)
	}
	switch ent.lifeState {
	case PlayerLifeDeathPersisting:
		snap, err := e.PlayerInstallPostDeathState(token.EntityID, placement, vitals, runtimeInputs)
		if err != nil {
			return EntitySnapshot{}, DeathCompletionApplied, err
		}
		ent.lifeState = PlayerLifeAwaitingRespawn
		return snap, DeathCompletionApplied, nil
	case PlayerLifeAwaitingRespawn:
		return ent.snapshot(), DeathCompletionDuplicate, nil
	default:
		return EntitySnapshot{}, DeathCompletionApplied, fmt.Errorf("%w: id %d life %d", ErrDeathAttemptMismatch, uint64(token.EntityID), uint8(ent.lifeState))
	}
}
