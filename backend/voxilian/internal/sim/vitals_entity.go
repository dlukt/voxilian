package sim

import (
	"errors"
	"fmt"

	"github.com/dlukt/voxilian/internal/world"
)

// Player-vitals entity integration (spec §9.4b, M5-T4b1): attaching the
// canonical §9.4 value to live sim entities, immutable inspection, the
// owner-local mutation surface composing the T4a production helpers, the
// narrow dirty/event seam, and the authoritative player run gate.
// T4b1 owns NO scheduling: no health/mana/rest deadlines, no
// acted-since-entry state, no stomach anchor (all T4b2), no death (T5),
// no gateway, no Store/persist imports.

// VigorRunThreshold freezes source VIGOR_RUN_THRESHOLD (spec §9.4b.7,
// user.kod): running is denied iff Vigor < 10, i.e. allowed at exactly
// 10. This is deliberately NOT the strict HasVigor mechanic (§9.4.18),
// which would wrongly deny Vigor exactly 10.
const VigorRunThreshold = 10

// Stable player-vitals entity errors (spec §9.4b.3/§9.4b.5). Matching
// MUST use errors.Is, never string parsing as control flow.
var (
	// ErrEntityNotPlayer marks a player-vitals mutation or attachment
	// against a generic (non-player) entity. Zero mutation.
	ErrEntityNotPlayer = errors.New("sim: entity is not a player")
	// ErrEntityAlreadyPlayer marks an attach attempt on an entity that
	// already carries player vitals. Zero mutation; re-load/re-attach is
	// not a T4b1 concept (respawn composes remove+add later).
	ErrEntityAlreadyPlayer = errors.New("sim: entity is already a player")
)

// PlayerVitalsEvent is the immutable dirty/event seam payload
// (spec §9.4b.6): the entity plus before/after value copies of one REAL
// vitals state change. It carries no CharacterID, revision, session, or
// persistence ownership; durable mapping belongs to later composition.
type PlayerVitalsEvent struct {
	EntityID EntityID
	Before   PlayerVitals
	After    PlayerVitals
}

// PlayerVitalsObserver receives real player-vitals state changes
// (spec §9.4b.6). Implementations MUST be non-blocking/bounded on the
// sim owner goroutine and own NO persistence: the observer may never
// perform blocking PG work. It fires iff a mutation changed the value
// (Before != After); T4a no-ops and failures never fire.
type PlayerVitalsObserver interface {
	OnPlayerVitalsChange(PlayerVitalsEvent)
}

// PlayerVitalsObserverFunc adapts a plain function to
// PlayerVitalsObserver.
type PlayerVitalsObserverFunc func(PlayerVitalsEvent)

// OnPlayerVitalsChange implements PlayerVitalsObserver.
func (f PlayerVitalsObserverFunc) OnPlayerVitalsChange(ev PlayerVitalsEvent) { f(ev) }

// AddPlayerEntity inserts a player entity carrying authoritative vitals
// (spec §9.4b.3 + §9.4b.3a): validates the vitals AND the T4b2 runtime
// inputs FIRST (an invalid value of either consumes no EntityID and
// mutates nothing), then performs the ordinary generic add (same
// all-or-nothing position/ID-exhaustion semantics) and installs a VALUE
// COPY of the vitals plus the player classification plus the ATOMIC
// §9.4b.9 initial runtime state (fresh acted flag, no rest deadline,
// stomach anchored at the current tick, health armed iff HP != MaxHP &&
// HP > 0, mana armed iff Mana != MaxMana — from the current tick using
// the supplied inputs), sampling VolumeFlagsAt at the initial position
// like AddEntity. The caller's vitals/inputs are never aliased; no
// default Stamina/Mysticism is invented.
//
// Owner-local: call only from the sim owner goroutine (Run/Step) or in
// Step-driven tests. Concurrent gateway callers keep using
// EnqueueAddEntity (generic); the typed concurrent player-add command is
// deferred to the gateway world-entry composition task (spec §9.4b.3).
func (e *Engine) AddPlayerEntity(pos world.Vec3, vitals PlayerVitals, runtimeInputs PlayerVitalsRuntimeInputs) (EntitySnapshot, error) {
	if err := vitals.Validate(); err != nil {
		return EntitySnapshot{}, err
	}
	if err := runtimeInputs.Validate(); err != nil {
		return EntitySnapshot{}, err
	}
	snap, err := e.registry.AddEntity(pos)
	if err != nil {
		return EntitySnapshot{}, err
	}
	ent, err := e.registry.lookup(snap.ID)
	if err != nil {
		return EntitySnapshot{}, fmt.Errorf("sim: entity %d vanished after add: %w", uint64(snap.ID), err)
	}
	ent.isPlayer = true
	ent.vitals = vitals
	ent.volumeFlags = e.collision.VolumeFlagsAt(pos)
	e.initPlayerRuntime(ent, runtimeInputs)
	return ent.snapshot(), nil
}

// AttachPlayerVitals converts an existing generic entity into a player
// entity carrying a validated VALUE COPY of vitals plus the atomic
// §9.4b.9/§9.4b.3a initial runtime state over the validated runtime
// inputs. Unknown IDs report ErrEntityNotFound, already-player entities
// ErrEntityAlreadyPlayer, and MIGRATING ownership fails with zero
// mutation (the quiesced entity accepts no source-side gameplay
// mutation, §5.4.2). No event fires: classification is not a vitals
// state change (§9.4b.6).
//
// Owner-local: call only from the sim owner goroutine (Run/Step) or in
// Step-driven tests.
func (e *Engine) AttachPlayerVitals(id EntityID, vitals PlayerVitals, runtimeInputs PlayerVitalsRuntimeInputs) error {
	if err := vitals.Validate(); err != nil {
		return err
	}
	if err := runtimeInputs.Validate(); err != nil {
		return err
	}
	if _, migrating := e.registry.migrations[id]; migrating {
		return fmt.Errorf("%w: id %d migrating", ErrCellHandoffRequired, uint64(id))
	}
	ent, err := e.registry.lookup(id)
	if err != nil {
		return err
	}
	if ent.isPlayer {
		return fmt.Errorf("%w: id %d", ErrEntityAlreadyPlayer, uint64(id))
	}
	ent.isPlayer = true
	ent.vitals = vitals
	e.initPlayerRuntime(ent, runtimeInputs)
	return nil
}

// PlayerVitalsOf inspects a live entity's authoritative vitals
// immutably (spec §9.4b.4): unknown ID -> ErrEntityNotFound; a known
// generic entity -> (zero value, false, nil); a player -> (value COPY,
// true, nil). The boolean distinguishes "not a player" from a valid
// player whose vitals legitimately hold zero-valued fields (creation
// Stomach 0, Exertion 0): zero fields are never "no vitals". A
// migrating entity still inspects read-only under its quiesced source
// ownership (same rule as registry.Entity). No mutable pointer into the
// registry escapes.
func (e *Engine) PlayerVitalsOf(id EntityID) (PlayerVitals, bool, error) {
	if rec, ok := e.registry.migrations[id]; ok {
		if !rec.entity.isPlayer {
			return PlayerVitals{}, false, nil
		}
		return rec.entity.vitals, true, nil
	}
	ent, err := e.registry.lookup(id)
	if err != nil {
		return PlayerVitals{}, false, err
	}
	if !ent.isPlayer {
		return PlayerVitals{}, false, nil
	}
	return ent.vitals, true, nil
}

// resolvePlayer resolves a live RESIDENT player entity for owner-local
// mutation (spec §9.4b.5): MIGRATING ownership fails with zero mutation
// (ErrCellHandoffRequired, mirroring SetPosition), unknown IDs report
// ErrEntityNotFound, and generic entities ErrEntityNotPlayer.
func (e *Engine) resolvePlayer(id EntityID) (*entity, error) {
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

// commitVitals stores a successful mutation's result and fires the
// dirty/event seam exactly once iff the value really changed
// (spec §9.4b.5–§9.4b.6, §9.4b.22 v0.3.31). It is the ONE narrow live
// post-commit validity guard: a value whose Validate() fails is NOT
// committed — live state stays bit-identical, no event fires, and the
// caller receives the validation error (hardening the source-
// unreachable hostile corner where a signed AdjustMaxMana would make
// MaxMana < 1; the T4a pures stay unbounded/source-faithful). Callers
// MUST have produced after through a T4a production helper.
func (e *Engine) commitVitals(ent *entity, after PlayerVitals) error {
	if err := after.Validate(); err != nil {
		return err
	}
	before := ent.vitals
	ent.vitals = after
	if after != before && e.vitalsObs != nil {
		e.vitalsObs.OnPlayerVitalsChange(PlayerVitalsEvent{
			EntityID: ent.id,
			Before:   before,
			After:    after,
		})
	}
	return nil
}

// PlayerLoseHealth applies the §9.4.6 loss primitive to a player
// entity's authoritative vitals (spec §9.4b.5). The result is the REAL
// T4a triple (before/after/applied plus ZeroHP and the Decay
// classification); no death transition happens here (T5 composes the
// ZeroHP handoff later). After a successful commit the health deadline
// reconciles per NewHealth from the current tick (spec §9.4b.11).
// Owner-local.
func (e *Engine) PlayerLoseHealth(id EntityID, amount int, decay bool) (PlayerVitals, HealthLossResult, error) {
	ent, err := e.resolvePlayer(id)
	if err != nil {
		return PlayerVitals{}, HealthLossResult{}, err
	}
	after, res, err := LoseHealth(ent.vitals, amount, decay)
	if err != nil {
		return ent.vitals, HealthLossResult{}, err
	}
	if err := e.commitVitals(ent, after); err != nil {
		return ent.vitals, HealthLossResult{}, err
	}
	e.reconcileHealth(ent, e.tick.Load())
	return after, res, nil
}

// PlayerGainHealthNormal applies the §9.4.7 capped heal. A no-op result
// (gain 0) leaves the stored value bit-identical and fires no event.
// After a successful commit the health deadline reconciles per
// NewHealth (spec §9.4b.11). Owner-local.
func (e *Engine) PlayerGainHealthNormal(id EntityID, amount int) (PlayerVitals, int, error) {
	ent, err := e.resolvePlayer(id)
	if err != nil {
		return PlayerVitals{}, 0, err
	}
	after, gained, err := GainHealthNormal(ent.vitals, amount)
	if err != nil {
		return ent.vitals, 0, err
	}
	if err := e.commitVitals(ent, after); err != nil {
		return ent.vitals, 0, err
	}
	e.reconcileHealth(ent, e.tick.Load())
	return after, gained, nil
}

// PlayerGainHealthOvercap applies the §9.4.8 over-max/vamp heal,
// including the source-faithful already-above-2*Max corner whose actual
// delta is negative. After a successful commit the health deadline
// reconciles per NewHealth (spec §9.4b.11). Owner-local.
func (e *Engine) PlayerGainHealthOvercap(id EntityID, amount int) (PlayerVitals, int, error) {
	ent, err := e.resolvePlayer(id)
	if err != nil {
		return PlayerVitals{}, 0, err
	}
	after, delta, err := GainHealthOvercap(ent.vitals, amount)
	if err != nil {
		return ent.vitals, 0, err
	}
	if err := e.commitVitals(ent, after); err != nil {
		return ent.vitals, 0, err
	}
	e.reconcileHealth(ent, e.tick.Load())
	return after, delta, nil
}

// PlayerAdjustBaseMaxHP applies the §9.4.4 two-step base-max primitive
// (the follow-on MaxHP adjustment is caller composition, exactly as in
// T4a; the follow-on PlayerAdjustMaxHP call performs its own health
// reconciliation — BaseMaxHP alone does not change HP or MaxHP, so it
// does not alter the NewHealth state relation). The guarded commit is
// checked explicitly like every other wrapper even though the bounded
// result is always Validate-valid. Owner-local.
func (e *Engine) PlayerAdjustBaseMaxHP(id EntityID, amount, effectiveStamina int) (PlayerVitals, int, error) {
	ent, err := e.resolvePlayer(id)
	if err != nil {
		return PlayerVitals{}, 0, err
	}
	after, delta, err := AdjustBaseMaxHP(ent.vitals, amount, effectiveStamina)
	if err != nil {
		return ent.vitals, 0, err
	}
	if err := e.commitVitals(ent, after); err != nil {
		return ent.vitals, 0, err
	}
	return after, delta, nil
}

// PlayerAdjustMaxHP applies the §9.4.5 non-clamping MaxHP modifier;
// current HP is untouched. After a successful commit the health
// deadline reconciles per NewHealth from the current tick (spec
// §9.4b.11): a MaxHP change can create the slot (equality broken),
// cancel it (equality reached), or keep the exact existing due.
// Owner-local.
func (e *Engine) PlayerAdjustMaxHP(id EntityID, amount int) (PlayerVitals, int, error) {
	ent, err := e.resolvePlayer(id)
	if err != nil {
		return PlayerVitals{}, 0, err
	}
	after, delta, err := AdjustMaxHP(ent.vitals, amount)
	if err != nil {
		return ent.vitals, 0, err
	}
	if err := e.commitVitals(ent, after); err != nil {
		return ent.vitals, 0, err
	}
	e.reconcileHealth(ent, e.tick.Load())
	return after, delta, nil
}

// PlayerLoseMana applies the §9.4.14 loss primitive; the returned value
// is the ACTUAL mana lost after the zero clamp. After a successful
// commit the mana deadline reconciles per NewMana (spec §9.4b.12).
// Owner-local.
func (e *Engine) PlayerLoseMana(id EntityID, amount int) (PlayerVitals, int, error) {
	ent, err := e.resolvePlayer(id)
	if err != nil {
		return PlayerVitals{}, 0, err
	}
	after, lost, err := LoseMana(ent.vitals, amount)
	if err != nil {
		return ent.vitals, 0, err
	}
	if err := e.commitVitals(ent, after); err != nil {
		return ent.vitals, 0, err
	}
	e.reconcileMana(ent, e.tick.Load())
	return after, lost, nil
}

// PlayerGainMana applies the §9.4.15 gain primitive in both modes; the
// capped mode preserves the source-faithful negative-delta corner
// (Mana above MaxMana). After a successful commit the mana deadline
// reconciles per NewMana (spec §9.4b.12). Owner-local.
func (e *Engine) PlayerGainMana(id EntityID, amount int, capped bool) (PlayerVitals, int, error) {
	ent, err := e.resolvePlayer(id)
	if err != nil {
		return PlayerVitals{}, 0, err
	}
	after, gained, err := GainMana(ent.vitals, amount, capped)
	if err != nil {
		return ent.vitals, 0, err
	}
	if err := e.commitVitals(ent, after); err != nil {
		return ent.vitals, 0, err
	}
	e.reconcileMana(ent, e.tick.Load())
	return after, gained, nil
}

// PlayerAdjustMaxMana applies the §9.4.13 unbounded MaxMana add path
// (the T4a pure stays source-faithful); the §9.4b.22 commit guard
// rejects a result whose Validate() fails (e.g. MaxMana below 1) with
// bit-identical live state and no event. After a successful commit the
// mana deadline reconciles per NewMana (spec §9.4b.12). Owner-local.
func (e *Engine) PlayerAdjustMaxMana(id EntityID, amount int) (PlayerVitals, int, error) {
	ent, err := e.resolvePlayer(id)
	if err != nil {
		return PlayerVitals{}, 0, err
	}
	after, delta, err := AdjustMaxMana(ent.vitals, amount)
	if err != nil {
		return ent.vitals, 0, err
	}
	if err := e.commitVitals(ent, after); err != nil {
		return ent.vitals, 0, err
	}
	e.reconcileMana(ent, e.tick.Load())
	return after, delta, nil
}

// PlayerApplyExertion applies the §9.4.19 general accumulator
// (including the strict >20000 conversion boundary). Owner-local.
func (e *Engine) PlayerApplyExertion(id EntityID, amount int64, setToThreshold bool) (PlayerVitals, error) {
	ent, err := e.resolvePlayer(id)
	if err != nil {
		return PlayerVitals{}, err
	}
	after, err := ApplyExertion(ent.vitals, amount, setToThreshold)
	if err != nil {
		return ent.vitals, err
	}
	if err := e.commitVitals(ent, after); err != nil {
		return ent.vitals, err
	}
	return after, nil
}

// PlayerApplyRestExertion applies the §9.4.21 rest-specific accumulator
// (resolved 1/2/3 room multiplier, residual clearing). It is the pure
// mutation surface only: the T4b2 rest scheduler — not this wrapper —
// decides WHEN recovery events fire; Second Wind blocking stays T6.
// Owner-local.
func (e *Engine) PlayerApplyRestExertion(id EntityID, amount int64, roomMultiplier int) (PlayerVitals, error) {
	ent, err := e.resolvePlayer(id)
	if err != nil {
		return PlayerVitals{}, err
	}
	after, err := ApplyRestExertion(ent.vitals, amount, roomMultiplier)
	if err != nil {
		return ent.vitals, err
	}
	if err := e.commitVitals(ent, after); err != nil {
		return ent.vitals, err
	}
	return after, nil
}

// PlayerSetRestThreshold applies the §9.4.20 explicit 10..100 threshold
// domain (no silent clamp). Owner-local.
func (e *Engine) PlayerSetRestThreshold(id EntityID, threshold int) (PlayerVitals, error) {
	ent, err := e.resolvePlayer(id)
	if err != nil {
		return PlayerVitals{}, err
	}
	after, err := SetRestThreshold(ent.vitals, threshold)
	if err != nil {
		return ent.vitals, err
	}
	if err := e.commitVitals(ent, after); err != nil {
		return ent.vitals, err
	}
	return after, nil
}

// entityCanRun resolves the per-entity run decision (spec §9.4b.7/
// §9.4b.18): a player uses its authoritative current Vigor via the
// frozen source rule Vigor >= VigorRunThreshold (NOT strict HasVigor,
// which would wrongly deny exactly 10); a generic entity delegates to
// the injected M4 RunGate UNCHANGED. Run denial still only falls back
// to walk; the gate mutates nothing.
func (e *Engine) entityCanRun(ent *entity) bool {
	if ent.isPlayer {
		return ent.vitals.Vigor >= VigorRunThreshold
	}
	return e.runGate.CanRun(ent.id)
}
