package sim

import (
	"errors"
	"fmt"
	"math"

	"github.com/dlukt/voxilian/internal/world"
)

// Portal-of-Life owner attempt lifecycle (spec §9.5.1k,
// M5-T5c3d2a): the store-independent sim-domain owner
// mechanics for attempting a Portal of Life against the
// victim's authoritative pending-death state.
// `internal/sim` only: no store, persist, pgx, sqlc
// output, gateway, session, or proto import; no blocking
// work, no goroutines, no persistence, no wire behavior,
// no Saver interaction, no world CorpsePortal object, no
// caster mana charging, no spell targeting, no
// Underworld LeaveHold, no PlanDeathPenalties /
// CommitDeathPenalties, no pending deletion.
//
// The caller invokes the owner operation only after
// upstream gameplay has resolved the valid target/cast
// facts: this layer owns ONLY the victim pending-death
// transition planning/correlation. Bounded off-owner
// Portal persistence (Store mapper, CommitPortalOfLife,
// lost-ack proof, redelivery) belongs to future T5c3d2b,
// which implements PortalOfLifeWorkReservation.

// Stable Portal owner errors (spec §9.5.1k, M5-T5c3d2a).
// Match with errors.Is, never string parsing as control
// flow. Store errors are never reused in sim.
var (
	// ErrPortalUnavailable marks a Portal attempt with no
	// valid target: no pending death, an already-used or
	// expired (nil) corpse association, or a target
	// corpse that does not match the pending death.
	// Zero mutation.
	ErrPortalUnavailable = errors.New("sim: portal unavailable")
	// ErrPortalAttemptInFlight marks a second Portal
	// begin while one attempt is already outstanding
	// against the entity, or a pending-death hydration
	// underneath a live Portal attempt. Zero mutation.
	ErrPortalAttemptInFlight = errors.New("sim: portal attempt in flight")
	// ErrPortalAttemptMismatch marks a Portal completion
	// or abort whose token does not match the entity's
	// current character, Portal epoch, or attempt
	// lifecycle (including a late completion for a
	// definitively aborted attempt, an abort for an
	// already-succeeded attempt, or a completion payload
	// inconsistent with the attempt it names). Zero
	// mutation.
	ErrPortalAttemptMismatch = errors.New("sim: portal attempt mismatch")
	// ErrPortalAttemptExhausted marks a Portal begin
	// whose per-entity Portal epoch cannot advance
	// without wrapping (already math.MaxUint64). Zero
	// mutation: the epoch does not advance.
	ErrPortalAttemptExhausted = errors.New("sim: portal attempt exhausted")
)

// PortalOfLifeResolvedInput is the store-independent
// resolved Portal-of-Life owner input (spec §9.5.1k,
// M5-T5c3d2a): all facts are already resolved. No Store
// ID lookup, no corpse query, no spell catalog query,
// no caster/session lookup, no PG lookup, no wall
// clock. NowSeconds is resolved whole-second time;
// TargetCorpseID is the actually resolved corpse
// target. Treat values as immutable.
type PortalOfLifeResolvedInput struct {
	NowSeconds     int64
	SpellPower     int
	TargetCorpseID int64
}

// PortalAttemptToken correlates one Portal-of-Life
// owner attempt with its completion/abort (spec
// §9.5.1k, M5-T5c3d2a): a plain immutable value naming
// the entity, its live CharacterID, and the per-entity
// ephemeral Portal-attempt epoch after increment. It is
// NOT the DeathAttemptToken epoch, NOT a Store
// revision, NOT an OpID, NOT a NetEntityID, and NOT a
// session ID. A reconnect/hydrated player may already
// carry pendingDeath while its fresh live entity has
// local deathEpoch == 0, so Portal correlation MUST
// survive that valid owner shape; EntityID still
// protects removal/re-add ABA.
type PortalAttemptToken struct {
	EntityID    EntityID
	CharacterID CharacterID
	Epoch       uint64
}

// portalAttemptState is the private owner-local
// ephemeral Portal attempt correlation (spec §9.5.1k,
// M5-T5c3d2a): enough immutable facts to validate the
// completion — the target CorpseID, the pending
// DeathTimeSeconds, the pending EffectiveCost before
// Portal, and the expected effective cost after Portal.
// Meaningful only while the owning entity's
// portalInFlight holds.
type portalAttemptState struct {
	targetCorpseID        int64
	deathTimeSeconds      int64
	effectiveCostBefore   int
	expectedEffectiveCost int
}

// PortalOfLifeCapture is the immutable complete current
// character capture for one Portal attempt (spec
// §9.5.1k, M5-T5c3d2a): the complete CURRENT in-memory
// character state the future Portal Store persistence
// performs its character-root CAS from. Position is the
// CURRENT authoritative position, Vitals the CURRENT
// authoritative vitals, Durable the CURRENT complete
// deep-frozen shadow, PendingBefore the deep-frozen
// current pending state, plus the resolved target
// corpse and the composed proposed/expected costs. No
// runtime-input snapshot is persisted by Portal. Treat
// values as immutable: orchestration deep-freezes
// before Prepare, so no caller alias may reach
// Advancement, Spells, Skills, Items, Enchants, or
// PendingBefore.CorpseID.
type PortalOfLifeCapture struct {
	Token PortalAttemptToken

	Position world.Vec3
	Vitals   PlayerVitals
	Durable  PlayerDurableState

	PendingBefore PendingDeathRuntime

	TargetCorpseID        int64
	ProposedCost          int
	ExpectedEffectiveCost int
}

// freezePortalOfLifeCapture deep-copies a Portal capture
// into independent ownership: the Durable shadow reuses
// the T5c3c2 deep-freeze and PendingBefore reuses the
// d1 deep-freeze (including the optional CorpseID
// pointer). Position, Vitals, and the cost scalars are
// plain values. The deep-copy itself performs no entity
// mutation.
func freezePortalOfLifeCapture(c PortalOfLifeCapture) PortalOfLifeCapture {
	c.Durable = freezePlayerDurableState(c.Durable)
	c.PendingBefore = *freezePendingDeathRuntime(&c.PendingBefore)
	return c
}

// ClonePortalOfLifeCapture deep-copies a Portal capture
// into independent ownership (spec §9.5.1k, M5-T5c3d2b2):
// the Durable shadow reuses the T5c3c2 deep-freeze and
// PendingBefore reuses the d1 deep-freeze (including the
// optional CorpseID pointer). Position, Vitals, Token,
// and the cost scalars are plain values. Pure and
// additive: no entity mutation, no validation side
// effect, no persistence. It exposes no mutable entity
// pointers.
func ClonePortalOfLifeCapture(c PortalOfLifeCapture) PortalOfLifeCapture {
	return freezePortalOfLifeCapture(c)
}

// PortalOfLifeWorkReservation is the ONE
// store-independent sim work-reservation interface for
// Portal persistence (spec §9.5.1k, M5-T5c3d2a; activation
// refined v0.3.55 for M5-T5c3d2b2): the concrete d2b2
// reservation implements it; d2a tests use an
// instrumented fake. No persist type, no Store type, no
// revision, no PG handle, no result channel, no func
// closure. All methods are non-blocking with respect to
// PG/network/disk.
type PortalOfLifeWorkReservation interface {
	// PreparePortalOfLifeWork validates and freezes the
	// complete Portal work while no Portal attempt is
	// live. On success the reservation owns the frozen
	// work privately and the caller may proceed to the
	// owner-local attempt installation; on failure the
	// caller cancels and no attempt goes live.
	PreparePortalOfLifeWork(PortalOfLifeCapture) error

	// ActivatePortalOfLifeWork publishes the
	// already-prepared work. It runs in the SAME owner
	// turn as the attempt installation. After successful
	// Prepare it cannot fail with queue-full (the held
	// queue permit proves queue capacity). A non-nil
	// error is definitive pre-publication: the job was
	// NOT published, Store has NOT been called and will
	// NEVER be called by this reservation, and the
	// concrete reservation has already released/cancelled
	// its queue permit + Saver critical reservation.
	ActivatePortalOfLifeWork() error

	// CancelPortalOfLifeWork abandons the reservation
	// before activation with no Store call and no queue
	// publication. It is idempotent; cancel after
	// activation is a no-op that never retracts an
	// authoritative queued/running persistence job.
	CancelPortalOfLifeWork()
}

// PortalOfLifeOrchestrationResult is the canonical
// owner-local Portal begin result (spec §9.5.1k,
// M5-T5c3d2a): the attempt token plus the composed
// costs. It exposes no Store request/revision.
type PortalOfLifeOrchestrationResult struct {
	Token                 PortalAttemptToken
	ProposedCost          int
	ExpectedEffectiveCost int
}

// PlayerOrchestratePortalOfLife is the canonical
// owner-local Portal begin operation (spec §9.5.1k,
// M5-T5c3d2a): it validates, composes the pure T5a
// Portal calculation, captures complete current
// character state, reserves bounded persistence work,
// and installs exactly one Portal attempt.
//
// Reservation ownership transfers into this call:
// every pre-accept error Cancels exactly once; a
// successful attempt Activates exactly once in the
// SAME owner turn. A nil reservation is a structural
// error with zero player mutation.
//
// Binding sequence: validate identity/life/pending/
// input/durable/epoch; PlanPortalOfLife;
// ReducePendingDeathCost; deep-freeze the complete
// capture with the predicted portalEpoch+1 token;
// reservation.PreparePortalOfLifeWork(capture); ONLY
// after Prepare succeeds: portalEpoch++, assert actual
// token == predicted token, install the private
// attempt with portalInFlight=true, then
// reservation.ActivatePortalOfLifeWork() in the
// SAME owner turn. A definitive pre-publication
// Activate error (spec §9.5.1k, frozen v0.3.55) rolls
// back synchronously: portalInFlight and the private
// attempt clear, pending and all gameplay state stay
// unchanged, the incremented portalEpoch stays consumed
// (never reused, so stale tokens can never become
// valid), and the activation error returns. No typed
// abort ingress is used for that same-owner-turn
// failure. There is NO other fallible Portal
// preparation after portalInFlight=true. On Prepare
// failure the reservation is Cancelled with portalEpoch
// unchanged, portalInFlight false, pending unchanged,
// and the player otherwise unchanged: no stranded
// attempt.
//
// Portal acts on the victim/pending-death character
// and allows begin while the resident player is
// PlayerLifeAwaitingRespawn OR PlayerLifeAlive
// (AwaitingRespawn is the transport/gameplay release
// barrier, not the Meridian pending-death phase).
// DeathPersisting is rejected (authoritative pending
// state has not yet arrived); MIGRATING keeps
// ErrCellHandoffRequired. Portal begin performs NO
// life-state transition: an Alive player stays Alive
// with ordinary gameplay accepted while Portal
// persistence is in flight.
//
// Owner-local: call only from the sim owner goroutine
// (Run/Step) or in Step-driven tests.
func (e *Engine) PlayerOrchestratePortalOfLife(id EntityID, input PortalOfLifeResolvedInput, reservation PortalOfLifeWorkReservation) (PortalOfLifeOrchestrationResult, error) {
	if reservation == nil {
		return PortalOfLifeOrchestrationResult{}, fmt.Errorf("sim: portal nil reservation: %w", ErrInvalidDeathInput)
	}
	// Reservation ownership transferred in: every
	// pre-accept error below cancels exactly once.
	fail := func(err error) (PortalOfLifeOrchestrationResult, error) {
		reservation.CancelPortalOfLifeWork()
		return PortalOfLifeOrchestrationResult{}, err
	}
	ent, err := e.resolvePlayerAnyLife(id)
	if err != nil {
		return fail(err)
	}
	switch ent.lifeState {
	case PlayerLifeAwaitingRespawn, PlayerLifeAlive:
	default:
		return fail(fmt.Errorf("%w: id %d life %d", ErrPlayerNotAlive, uint64(id), uint8(ent.lifeState)))
	}
	if ent.portalInFlight {
		return fail(fmt.Errorf("%w: id %d", ErrPortalAttemptInFlight, uint64(id)))
	}
	pending := ent.pendingDeath
	if pending == nil {
		return fail(fmt.Errorf("%w: id %d no pending death", ErrPortalUnavailable, uint64(id)))
	}
	if pending.PortalUsed {
		return fail(fmt.Errorf("%w: id %d portal already used", ErrPortalUnavailable, uint64(id)))
	}
	if pending.CorpseID == nil {
		return fail(fmt.Errorf("%w: id %d corpse expired", ErrPortalUnavailable, uint64(id)))
	}
	if *pending.CorpseID <= 0 {
		return fail(fmt.Errorf("%w: id %d corpse id=%d", ErrPortalUnavailable, uint64(id), *pending.CorpseID))
	}
	if input.TargetCorpseID <= 0 {
		return fail(fmt.Errorf("%w: id %d target corpse id=%d", ErrPortalUnavailable, uint64(id), input.TargetCorpseID))
	}
	if input.TargetCorpseID != *pending.CorpseID {
		return fail(fmt.Errorf("%w: id %d target %d vs pending %d", ErrPortalUnavailable, uint64(id), input.TargetCorpseID, *pending.CorpseID))
	}
	if input.NowSeconds < 0 {
		return fail(fmt.Errorf("sim: portal time %d: %w", input.NowSeconds, ErrInvalidDeathTime))
	}
	if input.NowSeconds < pending.DeathTimeSeconds {
		return fail(fmt.Errorf("sim: portal time %d before death %d: %w", input.NowSeconds, pending.DeathTimeSeconds, ErrInvalidDeathTime))
	}
	if ent.durable == nil {
		return fail(fmt.Errorf("%w: id %d", ErrPlayerDurableStateMissing, uint64(id)))
	}
	if err := validatePlayerDurableState(*ent.durable); err != nil {
		return fail(err)
	}
	if ent.portalEpoch == math.MaxUint64 {
		return fail(fmt.Errorf("%w: id %d", ErrPortalAttemptExhausted, uint64(id)))
	}
	// Pure T5a composition only: the proposal is the
	// Store proposal domain 5..80; the expected cost is
	// the lowers-only owner result. A pending cost of 0
	// with a 5+ proposal still starts the attempt (the
	// Portal is consumed even when the effective cost
	// does not decrease).
	proposed, err := PlanPortalOfLife(PortalOfLifeInput{
		PendingCost:       pending.EffectiveCost,
		CorpseAgeSeconds:  input.NowSeconds - pending.DeathTimeSeconds,
		SpellPower:        input.SpellPower,
		CorpseAlreadyUsed: pending.PortalUsed,
	})
	if err != nil {
		return fail(err)
	}
	effective, err := ReducePendingDeathCost(pending.EffectiveCost, proposed)
	if err != nil {
		return fail(err)
	}
	// The Portal epoch is predictable as portalEpoch+1
	// after all structural validation: construct the
	// complete capture with the predicted token while
	// no Portal attempt is live.
	predicted := PortalAttemptToken{
		EntityID:    ent.id,
		CharacterID: ent.characterID,
		Epoch:       ent.portalEpoch + 1,
	}
	capture := freezePortalOfLifeCapture(PortalOfLifeCapture{
		Token:                 predicted,
		Position:              ent.position,
		Vitals:                ent.vitals,
		Durable:               *ent.durable,
		PendingBefore:         *pending,
		TargetCorpseID:        input.TargetCorpseID,
		ProposedCost:          proposed,
		ExpectedEffectiveCost: effective,
	})
	if err := reservation.PreparePortalOfLifeWork(capture); err != nil {
		return fail(err)
	}
	// Prepare succeeded: the fallible phase is over.
	// Install the attempt, then activate in the SAME
	// owner turn.
	ent.portalEpoch++
	actual := PortalAttemptToken{
		EntityID:    ent.id,
		CharacterID: ent.characterID,
		Epoch:       ent.portalEpoch,
	}
	if actual != predicted {
		// Practically unreachable (single owner, no
		// interleaving mutation): fail closed without
		// stranding a live attempt.
		ent.portalEpoch--
		return fail(fmt.Errorf("sim: portal token prediction mismatch: %w", ErrPortalAttemptMismatch))
	}
	ent.portalAttempt = portalAttemptState{
		targetCorpseID:        input.TargetCorpseID,
		deathTimeSeconds:      pending.DeathTimeSeconds,
		effectiveCostBefore:   pending.EffectiveCost,
		expectedEffectiveCost: effective,
	}
	ent.portalInFlight = true
	if err := reservation.ActivatePortalOfLifeWork(); err != nil {
		// Definitive pre-publication failure in the SAME
		// owner turn (spec §9.5.1k, frozen v0.3.55): the
		// job was NOT published and Store will NEVER be
		// called by this reservation (it already released
		// its queue permit + Saver critical reservation),
		// so no typed abort ingress is used. Clear the
		// attempt, leave pending and all gameplay state
		// unchanged, KEEP the incremented portalEpoch
		// consumed, and return the activation error.
		ent.portalInFlight = false
		ent.portalAttempt = portalAttemptState{}
		return PortalOfLifeOrchestrationResult{}, err
	}
	return PortalOfLifeOrchestrationResult{
		Token:                 actual,
		ProposedCost:          proposed,
		ExpectedEffectiveCost: effective,
	}, nil
}

// PortalOfLifeCompletion is the typed Portal success
// completion (spec §9.5.1k, M5-T5c3d2a): the attempt
// token plus the authoritative complete pending state
// after Portal, with PortalUsed == true,
// DeathTimeSeconds equal to the targeted pending
// death, EffectiveCost equal to this attempt's
// expected effective cost, and CorpseID nil allowed
// (the corpse may have expired after commit before
// recovery) else equal to the attempt's target. It
// carries no Store revision/result. Treat values as
// immutable: owner-local application deep-freezes and
// typed ingress freezes the payload before
// publication.
type PortalOfLifeCompletion struct {
	Token   PortalAttemptToken
	Pending PendingDeathRuntime
}

// freezePortalOfLifeCompletion deep-copies a completion
// payload into independent ownership (the Pending
// value reuses the d1 deep-freeze including the
// optional CorpseID pointer). The deep-copy itself
// performs no entity mutation.
func freezePortalOfLifeCompletion(c PortalOfLifeCompletion) PortalOfLifeCompletion {
	c.Pending = *freezePendingDeathRuntime(&c.Pending)
	return c
}

// PortalCompletionDisposition is the
// PlayerAcceptPortalOfLifeCompletion outcome. Applied
// vs Duplicate are ordinary results, not errors; on
// error the disposition is meaningless — check err
// first.
type PortalCompletionDisposition uint8

const (
	// PortalCompletionApplied means the first exact
	// completion for the current attempt replaced the
	// pending death with the authoritative Portal
	// result.
	PortalCompletionApplied PortalCompletionDisposition = iota
	// PortalCompletionDuplicate means the token epoch
	// already resolved successfully with live pending
	// PortalUsed: a retry/redelivery answered with zero
	// mutation before validating the redelivered
	// payload.
	PortalCompletionDuplicate
)

// PlayerAcceptPortalOfLifeCompletion is the correlated
// owner-local Portal success apply (spec §9.5.1k,
// M5-T5c3d2a). It MUST NOT perform persistence: the
// supplied pending state became authoritative via
// successful critical persistence by caller contract
// (future d2b normal + proven-lost-ack routes).
//
// Token/entity/lifecycle validation: a MIGRATING
// entity keeps ErrCellHandoffRequired; an unknown
// EntityID reports the lookup error
// (ErrEntityNotFound, preserving the removal/ABA rule
// with no CharacterID-only fallback lookup); a
// generic entity, a wrong CharacterID, or a wrong/zero
// Portal epoch yields ErrPortalAttemptMismatch. Every
// failure is zero mutation.
//
// Duplicate (token epoch exactly matching the current
// Portal epoch while NO attempt is in flight and live
// pending has PortalUsed == true) returns
// PortalCompletionDuplicate with nil error and
// ABSOLUTELY ZERO mutation BEFORE validating the
// redelivered payload. An old completion for a
// definitively aborted attempt never becomes
// Duplicate: it yields ErrPortalAttemptMismatch.
//
// First apply (exact EntityID/CharacterID/Portal epoch
// with the attempt in flight) validates that the
// CURRENT live pending still identifies the same
// pre-attempt pending death, then validates/freezes
// the completion pending. On success it replaces
// pendingDeath ONLY, clears portalInFlight and the
// private attempt, leaves portalEpoch unchanged, and
// preserves EVERYTHING else (life state, position,
// vitals, runtime, durable, deathEpoch,
// lastDeathSeconds, history, movement): Portal
// persistence MUST NOT roll back current gameplay
// state.
//
// Owner-local: call only from the sim owner goroutine
// (Run/Step) or in Step-driven tests. Concurrent
// callers use EnqueuePortalOfLifeCompletion.
func (e *Engine) PlayerAcceptPortalOfLifeCompletion(completion PortalOfLifeCompletion) (PortalCompletionDisposition, error) {
	token := completion.Token
	if _, migrating := e.registry.migrations[token.EntityID]; migrating {
		return PortalCompletionApplied, fmt.Errorf("%w: id %d migrating", ErrCellHandoffRequired, uint64(token.EntityID))
	}
	ent, err := e.registry.lookup(token.EntityID)
	if err != nil {
		return PortalCompletionApplied, err
	}
	if !ent.isPlayer {
		return PortalCompletionApplied, fmt.Errorf("%w: id %d not a player", ErrPortalAttemptMismatch, uint64(token.EntityID))
	}
	if token.CharacterID != ent.characterID {
		return PortalCompletionApplied, fmt.Errorf("%w: id %d character %d vs live %d", ErrPortalAttemptMismatch, uint64(token.EntityID), int64(token.CharacterID), int64(ent.characterID))
	}
	if token.Epoch == 0 || token.Epoch != ent.portalEpoch {
		return PortalCompletionApplied, fmt.Errorf("%w: id %d epoch %d vs live %d", ErrPortalAttemptMismatch, uint64(token.EntityID), token.Epoch, ent.portalEpoch)
	}
	if !ent.portalInFlight {
		// No live attempt for this epoch: either the
		// already-resolved success (Duplicate, checked
		// BEFORE payload validation) or a definitively
		// aborted attempt (Mismatch, never Duplicate).
		if ent.pendingDeath != nil && ent.pendingDeath.PortalUsed {
			return PortalCompletionDuplicate, nil
		}
		return PortalCompletionApplied, fmt.Errorf("%w: id %d epoch %d not in flight", ErrPortalAttemptMismatch, uint64(token.EntityID), token.Epoch)
	}
	// The CURRENT live pending must still identify the
	// same pre-attempt pending death before applying:
	// no hydration or gameplay path may have replaced
	// it underneath the attempt.
	attempt := ent.portalAttempt
	live := ent.pendingDeath
	if live == nil || live.PortalUsed ||
		live.DeathTimeSeconds != attempt.deathTimeSeconds ||
		live.EffectiveCost != attempt.effectiveCostBefore ||
		live.CorpseID == nil || *live.CorpseID != attempt.targetCorpseID {
		return PortalCompletionApplied, fmt.Errorf("%w: id %d live pending no longer identifies the attempt", ErrPortalAttemptMismatch, uint64(token.EntityID))
	}
	authoritative := completion.Pending
	if err := ValidatePendingDeathRuntime(&authoritative); err != nil {
		return PortalCompletionApplied, err
	}
	if !authoritative.PortalUsed {
		return PortalCompletionApplied, fmt.Errorf("%w: id %d completion leaves portal unused", ErrPortalAttemptMismatch, uint64(token.EntityID))
	}
	if authoritative.DeathTimeSeconds != attempt.deathTimeSeconds {
		return PortalCompletionApplied, fmt.Errorf("%w: id %d completion death time %d vs attempt %d", ErrPortalAttemptMismatch, uint64(token.EntityID), authoritative.DeathTimeSeconds, attempt.deathTimeSeconds)
	}
	if authoritative.EffectiveCost != attempt.expectedEffectiveCost {
		return PortalCompletionApplied, fmt.Errorf("%w: id %d completion cost %d vs attempt %d", ErrPortalAttemptMismatch, uint64(token.EntityID), authoritative.EffectiveCost, attempt.expectedEffectiveCost)
	}
	if authoritative.CorpseID != nil && *authoritative.CorpseID != attempt.targetCorpseID {
		return PortalCompletionApplied, fmt.Errorf("%w: id %d completion corpse %d vs attempt %d", ErrPortalAttemptMismatch, uint64(token.EntityID), *authoritative.CorpseID, attempt.targetCorpseID)
	}
	ent.pendingDeath = freezePendingDeathRuntime(&authoritative)
	ent.portalInFlight = false
	ent.portalAttempt = portalAttemptState{}
	return PortalCompletionApplied, nil
}

// PortalAbortDisposition is the
// PlayerAbortPortalOfLifeAttempt outcome. Aborted vs
// Duplicate are ordinary results, not errors; on error
// the disposition is meaningless — check err first.
type PortalAbortDisposition uint8

const (
	// PortalAbortAborted means the first exact abort for
	// the current attempt cleared the in-flight state
	// with pending unchanged.
	PortalAbortAborted PortalAbortDisposition = iota
	// PortalAbortDuplicate means the token epoch
	// already aborted (or was never begun past the
	// current epoch) with no live attempt and no
	// successful Portal: a retry answered with zero
	// mutation.
	PortalAbortDuplicate
)

// PlayerAbortPortalOfLifeAttempt is the definitive
// owner-local Portal abort primitive (spec §9.5.1k,
// M5-T5c3d2a), for later d2b use ONLY when it knows no
// authoritative Store mutation was accepted/executed.
//
// Token/entity/lifecycle validation mirrors the
// completion path: MIGRATING keeps
// ErrCellHandoffRequired; unknown EntityID reports
// ErrEntityNotFound with no CharacterID-only fallback;
// generic entity, wrong CharacterID, or wrong/zero
// Portal epoch yields ErrPortalAttemptMismatch. Every
// failure is zero mutation.
//
// First exact abort (epoch matches the live Portal
// epoch with the attempt in flight) clears
// portalInFlight and the private attempt, leaves
// pendingDeath AND portalEpoch unchanged, and returns
// PortalAbortAborted. Repeated exact abort (epoch
// matches, no live attempt, no successful Portal)
// returns PortalAbortDuplicate with nil error and zero
// mutation.
//
// An abort MUST NEVER undo a successful Portal, clear
// PortalUsed, or raise/lower cost: the same token
// after its success (live pending PortalUsed)
// yields ErrPortalAttemptMismatch.
//
// Owner-local: call only from the sim owner goroutine
// (Run/Step) or in Step-driven tests. Concurrent
// callers use EnqueuePortalOfLifeAbort.
func (e *Engine) PlayerAbortPortalOfLifeAttempt(token PortalAttemptToken) (PortalAbortDisposition, error) {
	if _, migrating := e.registry.migrations[token.EntityID]; migrating {
		return PortalAbortAborted, fmt.Errorf("%w: id %d migrating", ErrCellHandoffRequired, uint64(token.EntityID))
	}
	ent, err := e.registry.lookup(token.EntityID)
	if err != nil {
		return PortalAbortAborted, err
	}
	if !ent.isPlayer {
		return PortalAbortAborted, fmt.Errorf("%w: id %d not a player", ErrPortalAttemptMismatch, uint64(token.EntityID))
	}
	if token.CharacterID != ent.characterID {
		return PortalAbortAborted, fmt.Errorf("%w: id %d character %d vs live %d", ErrPortalAttemptMismatch, uint64(token.EntityID), int64(token.CharacterID), int64(ent.characterID))
	}
	if token.Epoch == 0 || token.Epoch != ent.portalEpoch {
		return PortalAbortAborted, fmt.Errorf("%w: id %d epoch %d vs live %d", ErrPortalAttemptMismatch, uint64(token.EntityID), token.Epoch, ent.portalEpoch)
	}
	if ent.portalInFlight {
		ent.portalInFlight = false
		ent.portalAttempt = portalAttemptState{}
		return PortalAbortAborted, nil
	}
	if ent.pendingDeath != nil && ent.pendingDeath.PortalUsed {
		return PortalAbortAborted, fmt.Errorf("%w: id %d epoch %d already succeeded", ErrPortalAttemptMismatch, uint64(token.EntityID), token.Epoch)
	}
	return PortalAbortDuplicate, nil
}
