package sim

import (
	"errors"
	"fmt"
	"math"

	"github.com/dlukt/voxilian/internal/world"
)

// Authoritative Underworld-exit penalty owner attempt
// lifecycle (spec §9.5.1k, M5-T5c3d3a): the
// store-independent sim-domain owner mechanics for the
// actual Underworld `LeaveHold` penalty event.
// `internal/sim` only: no store, persist, pgx, sqlc
// output, gateway, session, or proto import; no blocking
// work, no goroutines, no persistence, no wire behavior,
// no Saver implementation, no queue/worker, no
// `CommitDeathPenalties`, no recovery, no lost-ack proof,
// no justice/guild runtime.
//
// This API represents the authoritative gameplay event
// "the player actually leaves the Underworld". It is NOT
// C->S 120 `respawn_ack` (T5c4 owns opcode 120
// transport), NOT Portal use, and NOT ordinary movement
// input.
//
// The existing pure T5a `PlanDeathPenalties` is reused
// unchanged (exactly one call per live attempt); this
// layer owns ONLY attempt correlation, the
// gameplay-quiesced lifecycle, the frozen retryable
// capture, and the typed owner completion/retryable
// transition. The bounded off-owner penalty persistence
// executor belongs to future T5c3d3b, which implements
// `DeathPenaltyWorkProvider`.

// Stable penalty owner errors (spec §9.5.1k, M5-T5c3d3a).
// Match with errors.Is, never string parsing as control
// flow. Store errors are never reused in sim.
var (
	// ErrDeathPenaltyUnavailable marks an Underworld-exit
	// penalty attempt with no valid target: no pending
	// death outstanding. Zero mutation.
	ErrDeathPenaltyUnavailable = errors.New("sim: death penalty unavailable")
	// ErrDeathPenaltyAttemptMismatch marks a penalty
	// completion, retryable notification, or retry whose
	// token does not match the entity's current
	// character, penalty epoch, or attempt lifecycle
	// (including a completion while persistence is not
	// active, a retryable notification after successful
	// completion, or a late token for a removed/re-added
	// entity). Zero mutation.
	ErrDeathPenaltyAttemptMismatch = errors.New("sim: death penalty attempt mismatch")
	// ErrDeathPenaltyAttemptExhausted marks a penalty
	// begin whose per-entity penalty epoch cannot advance
	// without wrapping (already math.MaxUint64). Zero
	// mutation: the epoch does not advance, and no RNG
	// is consumed.
	ErrDeathPenaltyAttemptExhausted = errors.New("sim: death penalty attempt exhausted")
	// ErrDeathPenaltyPersistenceActive marks a penalty
	// retry requested while persistence is already
	// active, or a second penalty begin while one
	// attempt is already outstanding against the
	// entity. Zero mutation.
	ErrDeathPenaltyPersistenceActive = errors.New("sim: death penalty persistence active")
)

// Source player-flag bits for penalty interpretation
// (pinned source `blakston.khd` at upstream commit
// `095c07b`, independently verified; spec §9.5.1k,
// M5-T5c3d3a). The authoritative current values come
// from `PlayerDurableState.Flags`; the caller MUST NOT
// supply still-newbie/murderer independently.
const (
	deathPenaltyFlagMurderer int32 = 0x000002
	deathPenaltyFlagOutlaw   int32 = 0x000008
	deathPenaltyFlagHaunted  int32 = 0x000100
	deathPenaltyFlagTutorial int32 = 0x000800
)

// UnderworldExitResolvedInput is the narrow
// store-independent resolved Underworld-exit owner input
// (spec §9.5.1k, M5-T5c3d3a): the settings-sourced
// default death cost plus the Underworld-exit-time
// frenzy fact. Pending cost, still-newbie, murderer,
// stamina, vitals, and spell/skill inputs are derived
// from the live entity, never caller-supplied. Treat
// values as immutable.
type UnderworldExitResolvedInput struct {
	DefaultDeathCost int
	FrenzyActive     bool
}

// DeathPenaltyAttemptToken correlates one Underworld-exit
// penalty attempt with its completion/retry (spec
// §9.5.1k, M5-T5c3d3a): a plain immutable value naming
// the entity, its live CharacterID, and the per-entity
// ephemeral penalty epoch after increment. It is NOT the
// DeathAttemptToken epoch, NOT the PortalAttemptToken
// epoch, NOT a Store revision, NOT an OpID, NOT a
// NetEntityID, and NOT a session ID. All fields are
// nonzero for a valid attempt.
type DeathPenaltyAttemptToken struct {
	EntityID    EntityID
	CharacterID CharacterID
	Epoch       uint64
}

// penaltyAttemptState is the private owner-local
// ephemeral penalty attempt correlation (spec §9.5.1k,
// M5-T5c3d3a): the exact frozen capture plus whether its
// persistence is currently active (reserved+prepared+
// activated with no terminal result yet). Meaningful
// only while the owning entity's penaltyAttempt is
// non-nil; the entity life state is
// `PlayerLifeDeathPenaltyPersisting` for the whole
// attempt, active or retryable.
type penaltyAttemptState struct {
	capture           DeathPenaltyCapture
	persistenceActive bool
}

// DeathPenaltyCapture is the immutable frozen retryable
// penalty capture for one attempt (spec §9.5.1k,
// M5-T5c3d3a): the complete POST-penalty character state
// the future penalty Store persistence commits, plus the
// complete PRE-consumption pending value it consumes.
// Vitals and Durable are the exact post-penalty values;
// PendingBefore is the exact pre-consumption pending
// value. `Plan.ScaledCost` and
// `PendingBefore.EffectiveCost` are deliberately both
// present and NOT interchangeable: future Store mapping
// MUST use `PendingBefore.EffectiveCost` as
// `ExpectedPendingCost`, NEVER `Plan.ScaledCost`. Treat
// values as immutable: orchestration deep-freezes before
// Prepare, so no caller alias may reach Durable
// advancement/spells/skills/items/enchants,
// PendingBefore.CorpseID, or Plan.AbilityLosses.
type DeathPenaltyCapture struct {
	Token DeathPenaltyAttemptToken

	Position world.Vec3

	Vitals  PlayerVitals
	Durable PlayerDurableState

	PendingBefore PendingDeathRuntime

	Plan DeathPenaltyPlan
}

// copyDeathAbilityLosses copies a plan loss list preserving
// nil-vs-empty shape (the T5a planner returns a non-nil
// empty list when nothing is lost): a nil list stays nil,
// a non-nil list copies into independent ownership.
func copyDeathAbilityLosses(in []DeathAbilityLoss) []DeathAbilityLoss {
	if in == nil {
		return nil
	}
	out := make([]DeathAbilityLoss, len(in))
	copy(out, in)
	return out
}

// freezeDeathPenaltyCapture deep-copies a penalty capture
// into independent ownership: the Durable shadow reuses
// the T5c3c2 deep-freeze, PendingBefore reuses the d1
// deep-freeze (including the optional CorpseID pointer),
// and the plan ability losses are copied (preserving
// nil-vs-empty). Position, Vitals, Token, and the cost
// scalars are plain values. The deep-copy itself performs
// no entity mutation.
func freezeDeathPenaltyCapture(c DeathPenaltyCapture) DeathPenaltyCapture {
	c.Durable = freezePlayerDurableState(c.Durable)
	c.PendingBefore = *freezePendingDeathRuntime(&c.PendingBefore)
	c.Plan.AbilityLosses = copyDeathAbilityLosses(c.Plan.AbilityLosses)
	return c
}

// CloneDeathPenaltyCapture deep-copies a penalty capture
// into independent ownership for future off-owner
// persistence use (spec §9.5.1k, M5-T5c3d3b): the future
// executor freezes submitted work without access to the
// private freeze helper. Pure and additive: no entity
// mutation, no validation side effect, no persistence.
// It exposes no mutable entity pointers.
func CloneDeathPenaltyCapture(c DeathPenaltyCapture) DeathPenaltyCapture {
	return freezeDeathPenaltyCapture(c)
}

// DeathPenaltyWorkProvider is the ONE store-independent
// sim work-reservation provider for penalty persistence
// (spec §9.5.1k, M5-T5c3d3a; binding on future M5-T5c3d3b):
// the concrete d3b `PenaltyExecutor` implements it; d3a
// tests use an instrumented fake. It contains no
// Store/persist/PG type. It is invoked INSIDE the sim
// owner turn BEFORE any RNG is consumed and performs NO
// PG, NO network, NO disk, and NO RNG — only bounded
// in-memory admission / Saver-gate reservation. The
// future d3b Reserve MUST synchronously/non-blockingly
// own, before returning success, one bounded executor
// queue permit PLUS one Saver `ReserveCriticalSet` slot
// for the Character, so queue/gate saturation is known
// before RNG rolls and no infrastructure-capacity
// failure can consume penalty RNG.
type DeathPenaltyWorkProvider interface {
	// ReserveDeathPenaltyWork reserves bounded
	// persistence work for one penalty attempt. On
	// failure the caller consumes zero RNG and mutates
	// nothing.
	ReserveDeathPenaltyWork(characterID CharacterID) (DeathPenaltyWorkReservation, error)
}

// DeathPenaltyWorkReservation is the ONE store-independent
// sim work reservation for penalty persistence (spec
// §9.5.1k, M5-T5c3d3a, corrected v0.3.57). No
// Store/persist/revision/result-channel types. All methods
// are non-blocking with respect to PG/network/disk. Future
// d3b contract: Reserve already owns the queue permit +
// Saver critical slot; Prepare performs ONLY
// validation/mapping/freezing (never acquires Saver or
// queue capacity); Activate cannot fail queue-full after
// successful Reserve but may report a definitive
// pre-publication executor shutdown. Prepare runs AFTER
// the owner installed the private attempt and locked
// gameplay (life `PlayerLifeDeathPenaltyPersisting`),
// so a Prepare failure retains the lock for exact-plan
// retry instead of returning to `Alive`.
type DeathPenaltyWorkReservation interface {
	// PrepareDeathPenaltyWork validates and freezes the
	// complete penalty work for the already-installed
	// private attempt (life is already
	// `PlayerLifeDeathPenaltyPersisting`; the v0.3.56
	// "while still Alive" order is superseded by the
	// v0.3.57 anti-reroll correction). On success the
	// reservation owns the frozen work privately and the
	// caller proceeds to activation; on failure the
	// caller cancels AND retains the owner lock with
	// the exact frozen capture for exact-plan retry —
	// the attempt is never unwound to `Alive`.
	PrepareDeathPenaltyWork(DeathPenaltyCapture) error

	// ActivateDeathPenaltyWork publishes the
	// already-prepared work in the SAME owner turn as the
	// attempt installation. A non-nil error is definitive
	// pre-publication: the job was NOT published and
	// Store will NEVER be called by this reservation.
	// Unlike Portal, the owner KEEPS the gameplay lock
	// with the exact frozen capture on this path (no
	// unlock, no reroll) so infrastructure retry can
	// reuse the same plan.
	ActivateDeathPenaltyWork() error

	// CancelDeathPenaltyWork abandons the reservation
	// before activation with no Store call and no queue
	// publication. It is idempotent; cancel after
	// activation is a no-op that never retracts an
	// authoritative queued/running persistence job.
	CancelDeathPenaltyWork()
}

// DeathPenaltyOrchestrationResult is the canonical
// owner-local penalty begin result (spec §9.5.1k,
// M5-T5c3d3a): the attempt token plus the exact frozen
// penalty plan. It exposes no Store request/revision and
// no DB pending row type.
type DeathPenaltyOrchestrationResult struct {
	Token DeathPenaltyAttemptToken
	Plan  DeathPenaltyPlan
}

// deathPenaltyAbilityInputs derives the ordered planner
// ability inputs from the live durable shadow: spells in
// exact current order, then skills in exact current
// order, keyed by stable catalog ID with independent
// spell/skill namespaces.
func deathPenaltyAbilityInputs(durable PlayerDurableState) (spells, skills []DeathAbilityInput) {
	spells = make([]DeathAbilityInput, 0, len(durable.Spells))
	for _, s := range durable.Spells {
		spells = append(spells, DeathAbilityInput{Key: int(s.ID), Ability: int(s.Ability)})
	}
	skills = make([]DeathAbilityInput, 0, len(durable.Skills))
	for _, s := range durable.Skills {
		skills = append(skills, DeathAbilityInput{Key: int(s.ID), Ability: int(s.Ability)})
	}
	return spells, skills
}

// applyDeathPenaltyPlanToDurable maps a successful plan
// onto a post-penalty durable shadow: it clones the
// current shadow, applies the outlaw/haunted clears, and
// applies every ability loss by `Kind + ID` (requiring
// the live `Ability` to equal the loss `FromAbility`,
// preserving `ID` and `AtrophyFlag`, order, and
// membership). It adds/deletes no abilities. On an
// impossible plan/state inconsistency it mutates nothing
// and reports an error.
func applyDeathPenaltyPlanToDurable(current PlayerDurableState, plan DeathPenaltyPlan) (PlayerDurableState, error) {
	post := freezePlayerDurableState(current)
	if plan.ClearOutlaw {
		post.Flags &^= deathPenaltyFlagOutlaw
	}
	if plan.ClearHaunted {
		post.Flags &^= deathPenaltyFlagHaunted
	}
	for _, loss := range plan.AbilityLosses {
		var list []PlayerAbilityState
		switch loss.Kind {
		case DeathAbilitySpell:
			list = post.Spells
		case DeathAbilitySkill:
			list = post.Skills
		default:
			return PlayerDurableState{}, fmt.Errorf("sim: death penalty loss kind %d: %w", uint8(loss.Kind), ErrInvalidDeathInput)
		}
		applied := false
		for i := range list {
			if int(list[i].ID) != loss.Key {
				continue
			}
			if int(list[i].Ability) != loss.FromAbility {
				return PlayerDurableState{}, fmt.Errorf("sim: death penalty loss key %d ability %d vs live %d: %w", loss.Key, loss.FromAbility, int(list[i].Ability), ErrInvalidDeathInput)
			}
			list[i].Ability = int16(loss.ToAbility)
			applied = true
			break
		}
		if !applied {
			return PlayerDurableState{}, fmt.Errorf("sim: death penalty loss key %d absent: %w", loss.Key, ErrInvalidDeathInput)
		}
	}
	if err := validatePlayerDurableState(post); err != nil {
		return PlayerDurableState{}, err
	}
	return post, nil
}

// PlayerOrchestrateDeathPenalties is the canonical
// owner-local Underworld-exit penalty begin operation
// (spec §9.5.1k, M5-T5c3d3a): it validates, reserves
// bounded persistence work BEFORE consuming RNG,
// consumes RNG exactly once via the existing pure T5a
// planner, freezes the complete post-penalty capture,
// and installs exactly one gameplay-quiesced penalty
// attempt.
//
// Provider ownership transfers into this call: every
// post-Reserve pre-accept error Cancels exactly once; a
// successful attempt Activates exactly once in the SAME
// owner turn. A nil provider is a structural error with
// zero player mutation.
//
// Binding sequence: Phase 1 structural validation (zero
// mutation, zero RNG); Phase 2 provider Reserve in the
// SAME owner turn (failure: zero RNG, zero mutation,
// pending unchanged, life `Alive`); Phase 3 exactly one
// `PlanDeathPenalties` (failure: Cancel, zero owner
// mutation); post-penalty state construction (failure:
// Cancel, zero owner mutation); Phase 4 install BEFORE
// any remaining fallible operation: `penaltyEpoch++`,
// install the private frozen attempt, life =
// `PlayerLifeDeathPenaltyPersisting` (frozen v0.3.57;
// the v0.3.56 Prepare-while-Alive order is superseded
// because a post-RNG Prepare failure MUST NOT return to
// `Alive` for a reroll); Phase 5
// `PrepareDeathPenaltyWork` with the exact frozen
// capture (failure: Cancel, life STAYS locked with the
// exact private attempt/capture, the consumed epoch,
// pending unchanged, and `persistenceActive == false`:
// no unlock, no reroll); Phase 6
// `ActivateDeathPenaltyWork` in the SAME owner turn. A
// definitive pre-publication Activate error likewise
// KEEPS life locked with the exact private
// attempt/capture and the consumed epoch
// (`persistenceActive` stays false): no unlock, no
// reroll — deliberately different from Portal.
//
// A new attempt requires a resident player, life `Alive`,
// a non-nil pending death, a complete validating durable
// shadow, valid vitals/runtime inputs, NO Portal attempt
// in flight, and NO penalty attempt already active.
// `CorpseID` (nil or not) and `PortalUsed` (either way)
// never gate the exit.
//
// Owner-local: call only from the sim owner goroutine
// (Run/Step) or in Step-driven tests.
func (e *Engine) PlayerOrchestrateDeathPenalties(id EntityID, input UnderworldExitResolvedInput, rng RNG, provider DeathPenaltyWorkProvider) (DeathPenaltyOrchestrationResult, error) {
	if provider == nil {
		return DeathPenaltyOrchestrationResult{}, fmt.Errorf("sim: death penalty nil provider: %w", ErrInvalidDeathInput)
	}
	ent, err := e.resolvePlayerAnyLife(id)
	if err != nil {
		return DeathPenaltyOrchestrationResult{}, err
	}
	switch ent.lifeState {
	case PlayerLifeAlive:
	case PlayerLifeDeathPersisting, PlayerLifeAwaitingRespawn:
		return DeathPenaltyOrchestrationResult{}, fmt.Errorf("%w: id %d life %d", ErrPlayerNotAlive, uint64(id), uint8(ent.lifeState))
	default:
		return DeathPenaltyOrchestrationResult{}, fmt.Errorf("%w: id %d life %d", ErrDeathPenaltyPersistenceActive, uint64(id), uint8(ent.lifeState))
	}
	pending := ent.pendingDeath
	if pending == nil {
		return DeathPenaltyOrchestrationResult{}, fmt.Errorf("%w: id %d no pending death", ErrDeathPenaltyUnavailable, uint64(id))
	}
	if ent.portalInFlight {
		return DeathPenaltyOrchestrationResult{}, fmt.Errorf("%w: id %d", ErrPortalAttemptInFlight, uint64(id))
	}
	if ent.penaltyAttempt != nil {
		return DeathPenaltyOrchestrationResult{}, fmt.Errorf("%w: id %d", ErrDeathPenaltyPersistenceActive, uint64(id))
	}
	if ent.durable == nil {
		return DeathPenaltyOrchestrationResult{}, fmt.Errorf("%w: id %d", ErrPlayerDurableStateMissing, uint64(id))
	}
	if err := validatePlayerDurableState(*ent.durable); err != nil {
		return DeathPenaltyOrchestrationResult{}, err
	}
	if err := ent.vitals.Validate(); err != nil {
		return DeathPenaltyOrchestrationResult{}, err
	}
	if err := ent.runtimeInputs.Validate(); err != nil {
		return DeathPenaltyOrchestrationResult{}, err
	}
	if err := ValidateDefaultDeathCost(input.DefaultDeathCost); err != nil {
		return DeathPenaltyOrchestrationResult{}, err
	}
	if rng == nil {
		return DeathPenaltyOrchestrationResult{}, ErrNilRNG
	}
	if ent.penaltyEpoch == math.MaxUint64 {
		return DeathPenaltyOrchestrationResult{}, fmt.Errorf("%w: id %d", ErrDeathPenaltyAttemptExhausted, uint64(id))
	}
	// Source flag facts derive from the authoritative
	// durable shadow; the caller cannot override them.
	flags := ent.durable.Flags
	murderer := flags&deathPenaltyFlagMurderer != 0
	stillNewbie := flags&deathPenaltyFlagTutorial == 0
	spells, skills := deathPenaltyAbilityInputs(*ent.durable)
	plannerInput := DeathPenaltyInput{
		PendingCost:  pending.EffectiveCost,
		DefaultCost:  input.DefaultDeathCost,
		FrenzyActive: input.FrenzyActive,
		StillNewbie:  stillNewbie,
		Murderer:     murderer,
		Stamina:      ent.runtimeInputs.EffectiveStamina,
		Vitals:       ent.vitals,
		Spells:       spells,
		Skills:       skills,
	}
	// Reservation ownership transferred in: every
	// post-Reserve pre-accept error below cancels exactly
	// once.
	reservation, err := provider.ReserveDeathPenaltyWork(ent.characterID)
	if err != nil {
		return DeathPenaltyOrchestrationResult{}, err
	}
	if reservation == nil {
		return DeathPenaltyOrchestrationResult{}, fmt.Errorf("sim: death penalty nil reservation: %w", ErrInvalidDeathInput)
	}
	fail := func(err error) (DeathPenaltyOrchestrationResult, error) {
		reservation.CancelDeathPenaltyWork()
		return DeathPenaltyOrchestrationResult{}, err
	}
	plan, err := PlanDeathPenalties(rng, plannerInput)
	if err != nil {
		return fail(err)
	}
	postDurable, err := applyDeathPenaltyPlanToDurable(*ent.durable, plan)
	if err != nil {
		return fail(err)
	}
	if err := plan.VitalsAfter.Validate(); err != nil {
		return fail(err)
	}
	// The penalty epoch is predictable as penaltyEpoch+1
	// after all structural validation: construct the
	// complete capture with the predicted token while no
	// penalty attempt is live. The live
	// vitals/durable/pending stay PRE-penalty: the
	// planned post-state lives only in the private
	// capture until success completion.
	predicted := DeathPenaltyAttemptToken{
		EntityID:    ent.id,
		CharacterID: ent.characterID,
		Epoch:       ent.penaltyEpoch + 1,
	}
	frozenPlan := plan
	frozenPlan.AbilityLosses = copyDeathAbilityLosses(plan.AbilityLosses)
	capture := freezeDeathPenaltyCapture(DeathPenaltyCapture{
		Token:         predicted,
		Position:      ent.position,
		Vitals:        plan.VitalsAfter,
		Durable:       postDurable,
		PendingBefore: *pending,
		Plan:          frozenPlan,
	})
	// Install the attempt BEFORE any remaining fallible
	// Prepare/Activate operation (frozen v0.3.57): once
	// RNG was consumed, no failure below may unwind to
	// `Alive` for a reroll.
	ent.penaltyEpoch++
	actual := DeathPenaltyAttemptToken{
		EntityID:    ent.id,
		CharacterID: ent.characterID,
		Epoch:       ent.penaltyEpoch,
	}
	if actual != predicted {
		// Practically unreachable (single owner, no
		// interleaving mutation): fail closed without
		// stranding a live attempt.
		ent.penaltyEpoch--
		return fail(fmt.Errorf("sim: death penalty token prediction mismatch: %w", ErrDeathPenaltyAttemptMismatch))
	}
	ent.penaltyAttempt = &penaltyAttemptState{capture: capture, persistenceActive: false}
	ent.lifeState = PlayerLifeDeathPenaltyPersisting
	if err := reservation.PrepareDeathPenaltyWork(capture); err != nil {
		// Prepare failure after install: the reservation
		// never became prepared, so Cancel it — but KEEP
		// life locked with the exact private
		// attempt/capture and the consumed epoch
		// (`persistenceActive` stays false) so
		// infrastructure retry reuses the same frozen
		// plan: no unlock, no reroll, pending unchanged,
		// zero Activates.
		reservation.CancelDeathPenaltyWork()
		return DeathPenaltyOrchestrationResult{}, err
	}
	if err := reservation.ActivateDeathPenaltyWork(); err != nil {
		// Definitive pre-publication failure in the SAME
		// owner turn: the job was NOT published and Store
		// will NEVER be called by this reservation. KEEP
		// life locked with the exact private attempt and
		// the consumed epoch (`persistenceActive` stays
		// false) so infrastructure retry reuses the same
		// frozen plan: no unlock, no reroll.
		return DeathPenaltyOrchestrationResult{}, err
	}
	ent.penaltyAttempt.persistenceActive = true
	return DeathPenaltyOrchestrationResult{Token: actual, Plan: frozenPlan}, nil
}

// PlayerRetryDeathPenaltyPersistence retries persistence
// for a gameplay-quiesced penalty attempt whose
// persistence is NOT active (spec §9.5.1k, M5-T5c3d3a):
// the primary anti-reroll guarantee. It reuses the EXACT
// frozen stored capture with a fresh provider
// reservation and MUST NOT call `PlanDeathPenalties`,
// read RNG, increment the penalty epoch, or rebuild the
// plan from current state.
//
// Valid only when life ==
// `PlayerLifeDeathPenaltyPersisting` with a private
// attempt present and `persistenceActive == false`. On
// success `persistenceActive` becomes true with the same
// token/capture/epoch. On any Reserve/Prepare/Activate
// failure the player REMAINS locked with the same
// token/capture/epoch and `persistenceActive == false`.
// A nil provider is a structural error with zero
// mutation.
//
// Owner-local: call only from the sim owner goroutine
// (Run/Step) or in Step-driven tests.
func (e *Engine) PlayerRetryDeathPenaltyPersistence(id EntityID, provider DeathPenaltyWorkProvider) (DeathPenaltyAttemptToken, error) {
	if provider == nil {
		return DeathPenaltyAttemptToken{}, fmt.Errorf("sim: death penalty nil provider: %w", ErrInvalidDeathInput)
	}
	ent, err := e.resolvePlayerAnyLife(id)
	if err != nil {
		return DeathPenaltyAttemptToken{}, err
	}
	if ent.lifeState != PlayerLifeDeathPenaltyPersisting {
		return DeathPenaltyAttemptToken{}, fmt.Errorf("%w: id %d life %d", ErrDeathPenaltyAttemptMismatch, uint64(id), uint8(ent.lifeState))
	}
	attempt := ent.penaltyAttempt
	if attempt == nil {
		return DeathPenaltyAttemptToken{}, fmt.Errorf("%w: id %d no penalty attempt", ErrDeathPenaltyAttemptMismatch, uint64(id))
	}
	if attempt.persistenceActive {
		return DeathPenaltyAttemptToken{}, fmt.Errorf("%w: id %d", ErrDeathPenaltyPersistenceActive, uint64(id))
	}
	stored := attempt.capture
	reservation, err := provider.ReserveDeathPenaltyWork(ent.characterID)
	if err != nil {
		return DeathPenaltyAttemptToken{}, err
	}
	if reservation == nil {
		return DeathPenaltyAttemptToken{}, fmt.Errorf("sim: death penalty nil reservation: %w", ErrInvalidDeathInput)
	}
	if err := reservation.PrepareDeathPenaltyWork(stored); err != nil {
		reservation.CancelDeathPenaltyWork()
		return DeathPenaltyAttemptToken{}, err
	}
	if err := reservation.ActivateDeathPenaltyWork(); err != nil {
		return DeathPenaltyAttemptToken{}, err
	}
	attempt.persistenceActive = true
	return stored.Token, nil
}

// DeathPenaltyRetryDisposition is the
// PlayerMarkDeathPenaltyPersistenceRetryable outcome.
// Applied vs Duplicate are ordinary results, not errors;
// on error the disposition is meaningless — check err
// first.
type DeathPenaltyRetryDisposition uint8

const (
	// DeathPenaltyRetryApplied means the first exact
	// pre-Store retryable notification for the current
	// attempt flipped persistence to retryable with the
	// life lock, pending, frozen capture, epoch, and all
	// gameplay state preserved.
	DeathPenaltyRetryApplied DeathPenaltyRetryDisposition = iota
	// DeathPenaltyRetryDuplicate means the token exactly
	// matches the already-retryable attempt: a repeated
	// notification answered with zero mutation.
	DeathPenaltyRetryDuplicate
)

// PlayerMarkDeathPenaltyPersistenceRetryable is the typed
// owner-local pre-Store retryable transition (spec
// §9.5.1k, M5-T5c3d3a), for later d3b use ONLY when an
// already-activated persistence job definitively failed
// BEFORE its critical callback / Store execution (queued
// job drained during shutdown, worker cancellation before
// Execute, or Execute failing before callback
// invocation). A Store-crossed unproven path MUST NEVER
// call this notification.
//
// First exact notification (life ==
// `DeathPenaltyPersisting`, matching current token,
// private attempt present, `persistenceActive == true`)
// does ONLY `persistenceActive = false`, preserving the
// life lock, pending, frozen capture, penalty epoch,
// position, vitals, durable, runtime, and movement. A
// repeated exact notification while already retryable
// returns `DeathPenaltyRetryDuplicate` with zero
// mutation. A notification after successful penalty
// completion is a mismatch. Token/entity/lifecycle
// validation mirrors the completion path: MIGRATING
// keeps `ErrCellHandoffRequired`; unknown EntityID
// reports `ErrEntityNotFound` with no CharacterID-only
// fallback; generic entity, wrong CharacterID, or
// wrong/zero epoch yields
// `ErrDeathPenaltyAttemptMismatch`. Every failure is zero
// mutation.
//
// Owner-local: call only from the sim owner goroutine
// (Run/Step) or in Step-driven tests. Concurrent callers
// use EnqueueDeathPenaltyPersistenceRetryable.
func (e *Engine) PlayerMarkDeathPenaltyPersistenceRetryable(token DeathPenaltyAttemptToken) (DeathPenaltyRetryDisposition, error) {
	if _, migrating := e.registry.migrations[token.EntityID]; migrating {
		return DeathPenaltyRetryApplied, fmt.Errorf("%w: id %d migrating", ErrCellHandoffRequired, uint64(token.EntityID))
	}
	ent, err := e.registry.lookup(token.EntityID)
	if err != nil {
		return DeathPenaltyRetryApplied, err
	}
	if !ent.isPlayer {
		return DeathPenaltyRetryApplied, fmt.Errorf("%w: id %d not a player", ErrDeathPenaltyAttemptMismatch, uint64(token.EntityID))
	}
	if token.CharacterID != ent.characterID {
		return DeathPenaltyRetryApplied, fmt.Errorf("%w: id %d character %d vs live %d", ErrDeathPenaltyAttemptMismatch, uint64(token.EntityID), int64(token.CharacterID), int64(ent.characterID))
	}
	if token.Epoch == 0 || token.Epoch != ent.penaltyEpoch {
		return DeathPenaltyRetryApplied, fmt.Errorf("%w: id %d epoch %d vs live %d", ErrDeathPenaltyAttemptMismatch, uint64(token.EntityID), token.Epoch, ent.penaltyEpoch)
	}
	if ent.lifeState != PlayerLifeDeathPenaltyPersisting || ent.penaltyAttempt == nil {
		return DeathPenaltyRetryApplied, fmt.Errorf("%w: id %d life %d", ErrDeathPenaltyAttemptMismatch, uint64(token.EntityID), uint8(ent.lifeState))
	}
	if !ent.penaltyAttempt.persistenceActive {
		return DeathPenaltyRetryDuplicate, nil
	}
	ent.penaltyAttempt.persistenceActive = false
	return DeathPenaltyRetryApplied, nil
}

// DeathPenaltyCompletion is the typed penalty success
// completion (spec §9.5.1k, M5-T5c3d3a): the attempt
// token ONLY. The exact post-penalty state is already
// frozen privately on the entity, so no Store result,
// revision, or duplicate Character snapshot crosses this
// boundary. Treat values as immutable.
type DeathPenaltyCompletion struct {
	Token DeathPenaltyAttemptToken
}

// DeathPenaltyCompletionDisposition is the
// PlayerAcceptDeathPenaltyCompletion outcome. Applied vs
// Duplicate are ordinary results, not errors; on error
// the disposition is meaningless — check err first.
type DeathPenaltyCompletionDisposition uint8

const (
	// DeathPenaltyCompletionApplied means the first exact
	// completion for the current attempt installed the
	// stored post-penalty state, cleared pending, and
	// moved the player back to `Alive`.
	DeathPenaltyCompletionApplied DeathPenaltyCompletionDisposition = iota
	// DeathPenaltyCompletionDuplicate means the token
	// exactly matches the already-applied attempt while
	// `Alive` with no pending death and no private
	// attempt: a retry/redelivery answered with absolute
	// zero mutation.
	DeathPenaltyCompletionDuplicate
)

// PlayerAcceptDeathPenaltyCompletion is the correlated
// owner-local penalty success apply (spec §9.5.1k,
// M5-T5c3d3a). It MUST NOT perform persistence and does
// NOT rerun penalty mechanics or RNG: the supplied token
// only correlates the already-frozen private capture,
// which became authoritative via successful critical
// persistence by caller contract.
//
// First valid completion (exact EntityID/CharacterID/
// penalty epoch with life == `DeathPenaltyPersisting`,
// a private attempt present, and `persistenceActive ==
// true`) defensively validates the stored frozen capture
// before any live mutation, then applies it in frozen
// v0.3.57 order: install the exact stored post-penalty
// Vitals, install the exact deep-frozen Durable state,
// run the existing owner-local `reconcileHealth` at the
// current tick (so a MaxHP loss can arm/cancel/persist
// the health deadline per the frozen NewHealth rule
// while mana/rest slots stay bit-identical), then clear
// pending/the private attempt and move life to `Alive`
// — preserving position, CharacterID/EntityID,
// death/portal epochs, `lastDeathSeconds`, runtime
// inputs, mana/rest slots, stomach anchor, movement
// state, and history. It performs no RNG, no
// `commitVitals`, and emits NO `PlayerVitalsObserver`
// event (T5c4/future presentation owns client
// transport effects).
//
// Duplicate completion (same exact token with life ==
// `Alive`, no private attempt, no pending death, and the
// epoch still current) returns
// `DeathPenaltyCompletionDuplicate` with nil error and
// ABSOLUTELY ZERO mutation BEFORE any payload work. If a
// NEW pending death exists, the old token is NOT a
// Duplicate: it yields `ErrDeathPenaltyAttemptMismatch`.
//
// Token/entity/lifecycle validation: a MIGRATING entity
// keeps `ErrCellHandoffRequired`; an unknown EntityID
// reports `ErrEntityNotFound` (preserving the
// removal/ABA rule with no CharacterID-only fallback
// lookup); a generic entity, a wrong CharacterID, a
// wrong/zero epoch, or completion while
// `persistenceActive == false` yields
// `ErrDeathPenaltyAttemptMismatch`. Every failure is zero
// mutation.
//
// Owner-local: call only from the sim owner goroutine
// (Run/Step) or in Step-driven tests. Concurrent callers
// use EnqueueDeathPenaltyCompletion.
func (e *Engine) PlayerAcceptDeathPenaltyCompletion(completion DeathPenaltyCompletion) (DeathPenaltyCompletionDisposition, error) {
	token := completion.Token
	if _, migrating := e.registry.migrations[token.EntityID]; migrating {
		return DeathPenaltyCompletionApplied, fmt.Errorf("%w: id %d migrating", ErrCellHandoffRequired, uint64(token.EntityID))
	}
	ent, err := e.registry.lookup(token.EntityID)
	if err != nil {
		return DeathPenaltyCompletionApplied, err
	}
	if !ent.isPlayer {
		return DeathPenaltyCompletionApplied, fmt.Errorf("%w: id %d not a player", ErrDeathPenaltyAttemptMismatch, uint64(token.EntityID))
	}
	if token.CharacterID != ent.characterID {
		return DeathPenaltyCompletionApplied, fmt.Errorf("%w: id %d character %d vs live %d", ErrDeathPenaltyAttemptMismatch, uint64(token.EntityID), int64(token.CharacterID), int64(ent.characterID))
	}
	if token.Epoch == 0 || token.Epoch != ent.penaltyEpoch {
		return DeathPenaltyCompletionApplied, fmt.Errorf("%w: id %d epoch %d vs live %d", ErrDeathPenaltyAttemptMismatch, uint64(token.EntityID), token.Epoch, ent.penaltyEpoch)
	}
	switch ent.lifeState {
	case PlayerLifeDeathPenaltyPersisting:
		attempt := ent.penaltyAttempt
		if attempt == nil || !attempt.persistenceActive {
			return DeathPenaltyCompletionApplied, fmt.Errorf("%w: id %d epoch %d not active", ErrDeathPenaltyAttemptMismatch, uint64(token.EntityID), token.Epoch)
		}
		stored := attempt.capture
		if err := stored.Vitals.Validate(); err != nil {
			return DeathPenaltyCompletionApplied, err
		}
		if err := validatePlayerDurableState(stored.Durable); err != nil {
			return DeathPenaltyCompletionApplied, err
		}
		if err := ValidatePendingDeathRuntime(&stored.PendingBefore); err != nil {
			return DeathPenaltyCompletionApplied, err
		}
		ent.vitals = stored.Vitals
		ent.durable = func() *PlayerDurableState { frozen := freezePlayerDurableState(stored.Durable); return &frozen }()
		// Frozen v0.3.57 health semantics (source
		// `GainBaseMaxHealth -> GainMaxHealth -> NewHealth`,
		// Voxilian `PlayerAdjustMaxHP -> reconcileHealth`):
		// the stored post-penalty Vitals are installed
		// exactly, then the existing owner-local
		// `reconcileHealth` runs at the current tick so a
		// MaxHP change correctly arms/cancels/persists
		// the health deadline. Mana/rest slots stay
		// bit-identical: no mana/rest reconciliation, no
		// `commitVitals`, no observer event.
		e.reconcileHealth(ent, e.tick.Load())
		ent.pendingDeath = nil
		ent.penaltyAttempt = nil
		ent.lifeState = PlayerLifeAlive
		return DeathPenaltyCompletionApplied, nil
	case PlayerLifeAlive:
		if ent.penaltyAttempt == nil && ent.pendingDeath == nil {
			return DeathPenaltyCompletionDuplicate, nil
		}
		return DeathPenaltyCompletionApplied, fmt.Errorf("%w: id %d epoch %d already resolved or superseded", ErrDeathPenaltyAttemptMismatch, uint64(token.EntityID), token.Epoch)
	default:
		return DeathPenaltyCompletionApplied, fmt.Errorf("%w: id %d life %d", ErrDeathPenaltyAttemptMismatch, uint64(token.EntityID), uint8(ent.lifeState))
	}
}
