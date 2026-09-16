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

// ImmediateDeathCompletion is the one complete authoritative
// immediate-death completion value (spec §9.5.1h, M5-T5c3c3a):
// ONLY already-authoritative post-death live state — the
// correlated DeathAttemptToken, the resolved post-death
// placement, the resolved post-death vitals, the already-resolved
// ephemeral runtime-input snapshot, and the resulting post-death
// durable shadow (as produced by T5c3c2 / future authoritative
// recovery). It carries NO Store result, corpse ID, revision,
// Saver metadata, pending-death row, PK-protection row, PG
// handle, or error. T5c3c3b later produces this value only after
// either a successful critical persistence result OR
// authoritative materialized-state reconciliation proving the
// state that must be installed; this layer does not know which
// route produced it. Treat values as immutable: owner-local
// application deep-freezes the durable shadow and typed ingress
// freezes the payload before publication, so caller mutation
// after submission cannot reach live state.
type ImmediateDeathCompletion struct {
	Token         DeathAttemptToken
	Placement     world.Vec3
	Vitals        PlayerVitals
	RuntimeInputs PlayerVitalsRuntimeInputs
	Durable       PlayerDurableState
}

// freezeImmediateDeathCompletion deep-copies a completion payload
// into independent ownership (spec §9.5.1h): Placement, Vitals,
// and RuntimeInputs are plain values, while the Durable shadow
// reuses the T5c3c2 deep-freeze (Advancement bytes,
// Spells/Skills/Items slices, every item Enchants slice). The
// deep-copy itself performs no entity mutation.
func freezeImmediateDeathCompletion(c ImmediateDeathCompletion) ImmediateDeathCompletion {
	c.Durable = freezePlayerDurableState(c.Durable)
	return c
}

// classifyDeathCompletion resolves the entity named by a death
// completion token and classifies the attempt lifecycle (spec
// §9.5.1f/§9.5.1h): the shared token/entity/lifecycle
// classification behind both PlayerAcceptPostDeathState and
// PlayerAcceptImmediateDeathCompletion. Binding, in this order:
// a MIGRATING entity keeps ErrCellHandoffRequired; an unknown
// EntityID reports the lookup error (ErrEntityNotFound,
// preserving the removal/ABA rule with no CharacterID-only
// fallback lookup); a generic entity, a wrong CharacterID, a
// wrong/zero epoch, or an incompatible life state yields
// ErrDeathAttemptMismatch. Every failure is zero mutation. On
// success it reports whether this is the first completion (life
// == DeathPersisting, duplicate false) or a retry/redelivery of
// the already-applied attempt (life == AwaitingRespawn,
// duplicate true).
func (e *Engine) classifyDeathCompletion(token DeathAttemptToken) (ent *entity, duplicate bool, err error) {
	if _, migrating := e.registry.migrations[token.EntityID]; migrating {
		return nil, false, fmt.Errorf("%w: id %d migrating", ErrCellHandoffRequired, uint64(token.EntityID))
	}
	ent, err = e.registry.lookup(token.EntityID)
	if err != nil {
		return nil, false, err
	}
	if !ent.isPlayer {
		return nil, false, fmt.Errorf("%w: id %d not a player", ErrDeathAttemptMismatch, uint64(token.EntityID))
	}
	if token.CharacterID != ent.characterID {
		return nil, false, fmt.Errorf("%w: id %d character %d vs live %d", ErrDeathAttemptMismatch, uint64(token.EntityID), int64(token.CharacterID), int64(ent.characterID))
	}
	if token.Epoch == 0 || token.Epoch != ent.deathEpoch {
		return nil, false, fmt.Errorf("%w: id %d epoch %d vs live %d", ErrDeathAttemptMismatch, uint64(token.EntityID), token.Epoch, ent.deathEpoch)
	}
	switch ent.lifeState {
	case PlayerLifeDeathPersisting:
		return ent, false, nil
	case PlayerLifeAwaitingRespawn:
		return ent, true, nil
	default:
		return nil, false, fmt.Errorf("%w: id %d life %d", ErrDeathAttemptMismatch, uint64(token.EntityID), uint8(ent.lifeState))
	}
}

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
	ent, duplicate, err := e.classifyDeathCompletion(token)
	if err != nil {
		return EntitySnapshot{}, DeathCompletionApplied, err
	}
	if duplicate {
		return ent.snapshot(), DeathCompletionDuplicate, nil
	}
	snap, err := e.PlayerInstallPostDeathState(token.EntityID, placement, vitals, runtimeInputs)
	if err != nil {
		return EntitySnapshot{}, DeathCompletionApplied, err
	}
	ent.lifeState = PlayerLifeAwaitingRespawn
	return snap, DeathCompletionApplied, nil
}

// PlayerAcceptImmediateDeathCompletion is the canonical
// authoritative immediate-death completion target (spec
// §9.5.1h, M5-T5c3c3a): the owner-local installation of an
// already-authoritative completion value carrying placement,
// post-death vitals, the already-resolved runtime-input
// snapshot, AND the post-death durable shadow. It MUST NOT
// perform persistence and does NOT recalculate any durable
// content: the supplied state became authoritative via
// successful critical persistence OR authoritative
// materialized-state reconciliation by caller contract.
//
// Token/entity/lifecycle validation reuses exactly the c3c1
// correlation domain via classifyDeathCompletion: unknown
// EntityID -> ErrEntityNotFound (no CharacterID-only fallback,
// preserving the removal/ABA rule); MIGRATING ->
// ErrCellHandoffRequired; generic entity, wrong CharacterID,
// wrong/zero epoch, or incompatible life state ->
// ErrDeathAttemptMismatch. Every failure is zero mutation.
//
// Duplicate completion (token exactly matching the current
// attempt while life == AwaitingRespawn) returns
// DeathCompletionDuplicate with the current EntitySnapshot and
// nil error with ABSOLUTELY ZERO mutation. Duplicate detection
// happens BEFORE validating or installing the completion
// payload: a duplicate/redelivered completion is already
// obsolete as a mutation and must not revalidate replacement
// durable content, relocate, reset history, replace the durable
// shadow, re-anchor the stomach, restart deadlines, or change
// generation.
//
// First valid completion (life == DeathPersisting) validates
// ALL replacement values before any live mutation — placement,
// vitals, runtime inputs (existing rules), and the durable
// shadow (existing T5c3c2 validation, deep-frozen before
// applying) — then invokes the EXISTING
// PlayerInstallPostDeathState semantics; ONLY after that
// succeeds does it replace the durable shadow with the frozen
// post-death shadow and move life state -> AwaitingRespawn,
// returning DeathCompletionApplied. On install failure life
// state REMAINS DeathPersisting, the token REMAINS
// current/valid, the entity remains in the T5c3b guaranteed
// rollback/quiesced state with its PRE-DEATH durable shadow,
// and the install error returns (never a silent transition
// back Alive). No partial durable install is permitted.
//
// The completion emits NO PlayerVitalsObserver event (the state
// was already accepted by the critical death
// persistence/recovery path; T5c4 owns wire presentation), and
// replacing the durable shadow adds no new observer and
// generates no second persistence dirty event.
//
// Owner-local: call only from the sim owner goroutine (Run/Step)
// or in Step-driven tests. Concurrent callers use
// EnqueueImmediateDeathCompletion.
func (e *Engine) PlayerAcceptImmediateDeathCompletion(completion ImmediateDeathCompletion) (EntitySnapshot, DeathCompletionDisposition, error) {
	ent, duplicate, err := e.classifyDeathCompletion(completion.Token)
	if err != nil {
		return EntitySnapshot{}, DeathCompletionApplied, err
	}
	if duplicate {
		return ent.snapshot(), DeathCompletionDuplicate, nil
	}
	if _, err := world.CellForPosition(completion.Placement); err != nil {
		return EntitySnapshot{}, DeathCompletionApplied, fmt.Errorf("%w: %w", ErrInvalidPosition, err)
	}
	if err := completion.Vitals.Validate(); err != nil {
		return EntitySnapshot{}, DeathCompletionApplied, err
	}
	if err := completion.RuntimeInputs.Validate(); err != nil {
		return EntitySnapshot{}, DeathCompletionApplied, err
	}
	if err := validatePlayerDurableState(completion.Durable); err != nil {
		return EntitySnapshot{}, DeathCompletionApplied, err
	}
	frozen := freezePlayerDurableState(completion.Durable)
	snap, err := e.PlayerInstallPostDeathState(ent.id, completion.Placement, completion.Vitals, completion.RuntimeInputs)
	if err != nil {
		return EntitySnapshot{}, DeathCompletionApplied, err
	}
	ent.durable = &frozen
	ent.lifeState = PlayerLifeAwaitingRespawn
	return snap, DeathCompletionApplied, nil
}
