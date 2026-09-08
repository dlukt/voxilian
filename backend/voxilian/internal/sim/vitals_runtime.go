package sim

import (
	"errors"
	"fmt"

	"github.com/dlukt/voxilian/internal/serial32"
)

// Deterministic vitals scheduling runtime (spec §9.4b, M5-T4b2):
// per-player deadline slots driven by Engine.Step inside the single
// sim-writer model, the plain-value resolved runtime-input snapshot
// (§9.4b.14a), the rest lifecycle hooks (§9.4b.13), actedSinceEntry
// (§9.4b.16), the stomach sim-time anchor (§9.4b.15), and immutable
// runtime inspection. No goroutine per player, no ticker/timer per
// player, no wall-clock gameplay deadline, no heap/global timer list,
// no second simulation path. Runtime metadata is ephemeral: never
// persisted, never fired through the vitals observer.

// ErrInvalidRuntimeElapsed marks an ambiguous runtime elapsed-tick
// derivation (>= 2^31 ticks between anchor and current): the modular
// u32 distance cannot be ordered, so the lazy update is rejected
// rather than guessed.
var ErrInvalidRuntimeElapsed = errors.New("sim: invalid runtime elapsed")

// PlayerVitalsRuntimeInputs is the authoritative CURRENT resolved
// regen-input snapshot consumed by the T4b2 scheduler (spec §9.4b.14a,
// v0.3.31). It is a plain value: no pointers, maps, or slices; no
// named-spell/room/world lookup state; no durable ID, revision, or
// session identity; never persisted. Later tasks update it when stat
// modifiers, Jala songs, or room policy change — always BEFORE the
// next create/re-arm consumes it.
type PlayerVitalsRuntimeInputs struct {
	// EffectiveStamina/EffectiveMysticism are the already-resolved
	// effective attributes (source bound(base+mod,1,70)): domain 1..70.
	EffectiveStamina   int
	EffectiveMysticism int

	// Power seams: 0 means absent, nonzero means the already-resolved
	// spell power 1..99.
	RestoratePower  int
	RejuvenatePower int
	ManaFocusPower  int
	InvigoratePower int

	// RestRecoveryMultiplier is the resolved room recovery multiplier:
	// exactly 1 (ordinary), 2 (sanctuary), or 3 (triple-heal; both
	// flags means 3, never 6). The caller resolves it; sim invents no
	// ROOM_* flag IDs or VolumeFlags bits.
	RestRecoveryMultiplier int
}

// Validate rejects corrupt runtime inputs (spec §9.4b.14a), reusing
// the existing stable error sentinels: ErrInvalidCombatStat for
// effective attributes outside 1..70, ErrInvalidSpellPower for nonzero
// powers outside 1..99, ErrInvalidVitals for a rest multiplier outside
// 1..3. Match with errors.Is, never string parsing.
func (in PlayerVitalsRuntimeInputs) Validate() error {
	if err := checkEffectiveAttr("stamina", in.EffectiveStamina); err != nil {
		return err
	}
	if err := checkEffectiveAttr("mysticism", in.EffectiveMysticism); err != nil {
		return err
	}
	for _, p := range []int{in.RestoratePower, in.RejuvenatePower, in.ManaFocusPower, in.InvigoratePower} {
		if p == 0 {
			continue
		}
		if err := checkSpellPower(p); err != nil {
			return err
		}
	}
	if in.RestRecoveryMultiplier < 1 || in.RestRecoveryMultiplier > 3 {
		return fmt.Errorf("sim: vitals runtime rest multiplier = %d: %w",
			in.RestRecoveryMultiplier, ErrInvalidVitals)
	}
	return nil
}

// PlayerVitalsRuntimeSnapshot is the immutable runtime inspection copy
// (spec §9.4b.14a/§9.4b.10): the current resolved inputs, the
// actedSinceEntry flag, the three deadline slots (armed + due), and
// the stomach anchor. Values only; no mutable pointer escapes.
type PlayerVitalsRuntimeSnapshot struct {
	Inputs            PlayerVitalsRuntimeInputs
	ActedSinceEntry   bool
	HealthArmed       bool
	HealthDue         uint32
	ManaArmed         bool
	ManaDue           uint32
	RestArmed         bool
	RestDue           uint32
	StomachAnchorTick uint32
}

// impossibleState panics on an error the validated invariant domains
// prove unreachable (spec §9.4b.1/§9.4b.22 totality): Validate-valid
// vitals + Validate-valid runtime inputs + TickHz 1..120 make every
// interval calculation, CastTicks conversion, timer-step mutation, and
// post-commit validation total. No public owner-local API can store
// invalid state (validated before storage, guarded at commit), so no
// client/input data can reach this panic; tests sweep the domains to
// prove it. It exists so silent divergence is impossible.
func impossibleState(context string, err error) {
	panic(fmt.Sprintf("sim: vitals runtime impossible state (%s): %v", context, err))
}

// initPlayerRuntime installs the §9.4b.9/§9.4b.3a initial runtime state
// atomically with a validated vitals value: fresh acted flag, no rest
// deadline, stomach anchored at the current simulation tick, and
// NewHealth/NewMana create semantics applied immediately from the
// current tick using the supplied (already validated) inputs.
func (e *Engine) initPlayerRuntime(ent *entity, inputs PlayerVitalsRuntimeInputs) {
	tick := e.tick.Load()
	ent.runtimeInputs = inputs
	ent.actedSinceEntry = false
	ent.restArmed = false
	ent.stomachAnchorTick = tick
	e.reconcileHealth(ent, tick)
	e.reconcileMana(ent, tick)
}

// armHealthDeadline creates/re-arms the health slot from the CURRENT
// tick using the CURRENT runtime inputs (spec §9.4b.10–§9.4b.11): the
// interval is the T4a HealthRegenIntervalMs over resolved values
// (faction phase 2 = 0), converted by the ONE canonical CastTicks
// ceil conversion; due = currentTick + delayTicks with normal u32
// wrap. Caller must have cleared/cancelled the slot first.
func (e *Engine) armHealthDeadline(ent *entity, tick uint32) {
	ms, err := HealthRegenIntervalMs(
		ent.vitals.Vigor, ent.runtimeInputs.EffectiveStamina,
		ent.vitals.MaxHP, 0, ent.runtimeInputs.RestoratePower)
	if err != nil {
		impossibleState("health interval", err)
	}
	delay, err := CastTicks(ms, e.tickHz)
	if err != nil {
		impossibleState("health cast ticks", err)
	}
	ent.healthDue = tick + uint32(delay)
	ent.healthArmed = true
}

// armManaDeadline creates/re-arms the mana slot (spec §9.4b.12): the
// T4a ManaRegenIntervalMs over resolved values (over-max naturally
// selects the 30000 ms BOOST_DECAY branch), ONE CastTicks conversion,
// due = currentTick + delayTicks.
func (e *Engine) armManaDeadline(ent *entity, tick uint32) {
	ms, err := ManaRegenIntervalMs(
		ent.vitals.Mana, ent.vitals.MaxMana, ent.vitals.Vigor,
		ent.runtimeInputs.EffectiveMysticism, 0,
		ent.runtimeInputs.RejuvenatePower, ent.runtimeInputs.ManaFocusPower)
	if err != nil {
		impossibleState("mana interval", err)
	}
	delay, err := CastTicks(ms, e.tickHz)
	if err != nil {
		impossibleState("mana cast ticks", err)
	}
	ent.manaDue = tick + uint32(delay)
	ent.manaArmed = true
}

// armRestDeadline creates/re-arms the rest slot (spec §9.4b.13): the
// T4a RestIntervalMs over resolved inputs, ONE CastTicks conversion,
// due = currentTick + delayTicks.
func (e *Engine) armRestDeadline(ent *entity, tick uint32) {
	ms, err := RestIntervalMs(
		ent.runtimeInputs.EffectiveStamina, ent.runtimeInputs.InvigoratePower)
	if err != nil {
		impossibleState("rest interval", err)
	}
	delay, err := CastTicks(ms, e.tickHz)
	if err != nil {
		impossibleState("rest cast ticks", err)
	}
	ent.restDue = tick + uint32(delay)
	ent.restArmed = true
}

// reconcileHealth applies NewHealth semantics from the CURRENT tick
// (spec §9.4b.11, frozen exactly from source NewHealth):
//
//	slot absent && HP != MaxHP && HP > 0 -> arm from current tick/inputs
//	slot present && HP == MaxHP          -> cancel
//	slot present && HP != MaxHP          -> KEEP the exact existing due
//
// Changed Vigor/stats/MaxHP never restart a running deadline: changed
// inputs affect only the NEXT create. HP == 0 never newly arms (the
// zero-HP/death boundary is M5-T5).
func (e *Engine) reconcileHealth(ent *entity, tick uint32) {
	if ent.healthArmed {
		if ent.vitals.HP == ent.vitals.MaxHP {
			ent.healthArmed = false
		}
		return
	}
	if ent.vitals.HP != ent.vitals.MaxHP && ent.vitals.HP > 0 {
		e.armHealthDeadline(ent, tick)
	}
}

// reconcileMana applies NewMana semantics from the CURRENT tick
// (spec §9.4b.12): absent && Mana != MaxMana -> arm; present &&
// Mana == MaxMana -> cancel; present && Mana != MaxMana -> KEEP the
// exact existing due. No acted gate (mana regen is not action-gated).
func (e *Engine) reconcileMana(ent *entity, tick uint32) {
	if ent.manaArmed {
		if ent.vitals.Mana == ent.vitals.MaxMana {
			ent.manaArmed = false
		}
		return
	}
	if ent.vitals.Mana != ent.vitals.MaxMana {
		e.armManaDeadline(ent, tick)
	}
}

// commitRuntimeVitals stores a timer-driven mutation through the ONE
// guarded dirty seam (spec §9.4b.6/§9.4b.22). The mutation inputs are
// total over validated state, so the post-commit guard cannot fail;
// impossibleState keeps that explicit.
func (e *Engine) commitRuntimeVitals(ent *entity, after PlayerVitals, context string) {
	if err := e.commitVitals(ent, after); err != nil {
		impossibleState(context, err)
	}
}

// stepPlayerVitalsRuntime is the per-player runtime Step phase
// (spec §9.4b.17, v0.3.31 phase order): called from Engine.Step for
// THIS SAME entity after movement output and before the history
// sample, processing the three slots in the FIXED order health, mana,
// rest. Each slot fires at most once per Step (clear FIRST, apply at
// most one event, then create/cancel/keep); no catch-up loop, no
// timer-map iteration, no second global pass.
func (e *Engine) stepPlayerVitalsRuntime(ent *entity, tick uint32) {
	e.fireHealthIfDue(ent, tick)
	e.fireManaIfDue(ent, tick)
	e.fireRestIfDue(ent, tick)
}

// slotDue reports whether an armed deadline is due at tick: a deadline
// is future iff serial32.After(due, tick); everything else (including
// equality) is due. CastTicks bounds every delay < 2^31, so the
// comparison is always unambiguous.
func slotDue(due, tick uint32) bool {
	return !serial32.After(due, tick)
}

// fireHealthIfDue processes one health deadline (spec §9.4b.11). The
// slot is cleared FIRST (source timer.c removes the node before
// dispatching); then, iff the player acted since entry: HP < MaxHP
// gains +1 through the normal heal path, HP > MaxHP decays -1 via
// LoseHealth(1, decay), equality mutates nothing. An idle player
// mutates nothing. NewHealth reconciliation then re-arms from the
// current tick with current inputs — an idle damaged player therefore
// keeps re-arming without healing.
func (e *Engine) fireHealthIfDue(ent *entity, tick uint32) {
	if !ent.healthArmed || !slotDue(ent.healthDue, tick) {
		return
	}
	ent.healthArmed = false
	if ent.actedSinceEntry {
		switch {
		case ent.vitals.HP < ent.vitals.MaxHP:
			after, _, err := GainHealthNormal(ent.vitals, 1)
			if err != nil {
				impossibleState("health gain step", err)
			}
			e.commitRuntimeVitals(ent, after, "health gain commit")
		case ent.vitals.HP > ent.vitals.MaxHP:
			after, _, err := LoseHealth(ent.vitals, 1, true)
			if err != nil {
				impossibleState("health decay step", err)
			}
			e.commitRuntimeVitals(ent, after, "health decay commit")
		}
	}
	e.reconcileHealth(ent, tick)
}

// fireManaIfDue processes one mana deadline (spec §9.4b.12): clear
// FIRST, then Mana < MaxMana -> +1 uncapped gain, Mana > MaxMana ->
// -1 decay, equality -> no mutation; then NewMana reconciliation (the
// over-max re-arm naturally selects the 30000 ms BOOST_DECAY branch).
func (e *Engine) fireManaIfDue(ent *entity, tick uint32) {
	if !ent.manaArmed || !slotDue(ent.manaDue, tick) {
		return
	}
	ent.manaArmed = false
	switch {
	case ent.vitals.Mana < ent.vitals.MaxMana:
		after, _, err := GainMana(ent.vitals, 1, false)
		if err != nil {
			impossibleState("mana gain step", err)
		}
		e.commitRuntimeVitals(ent, after, "mana gain commit")
	case ent.vitals.Mana > ent.vitals.MaxMana:
		after, _, err := LoseMana(ent.vitals, 1)
		if err != nil {
			impossibleState("mana decay step", err)
		}
		e.commitRuntimeVitals(ent, after, "mana decay commit")
	}
	e.reconcileMana(ent, tick)
}

// fireRestIfDue processes one rest deadline (spec §9.4b.13): clear
// FIRST; iff Vigor < RestThreshold apply ONE RestAddExertion recovery
// event (-10000 exertion scaled by the CURRENT fire-time
// RestRecoveryMultiplier — source checks then-current room state);
// ALWAYS re-arm the next deadline from the current tick using current
// inputs. Reaching the threshold does NOT stop resting.
func (e *Engine) fireRestIfDue(ent *entity, tick uint32) {
	if !ent.restArmed || !slotDue(ent.restDue, tick) {
		return
	}
	ent.restArmed = false
	if ent.vitals.Vigor < ent.vitals.RestThreshold {
		after, err := ApplyRestExertion(
			ent.vitals, -10000, ent.runtimeInputs.RestRecoveryMultiplier)
		if err != nil {
			impossibleState("rest recovery step", err)
		}
		e.commitRuntimeVitals(ent, after, "rest recovery commit")
	}
	e.armRestDeadline(ent, tick)
}

// PlayerSetVitalsRuntimeInputs replaces the authoritative resolved
// runtime-input snapshot (spec §9.4b.14a). The value validates BEFORE
// storage: hostile/invalid inputs are rejected with zero mutation.
// Source timing rule: an update MUST NOT reset/restart an already
// running health, mana, or rest deadline — the new values affect only
// the NEXT create/re-arm of each slot and the CURRENT rest recovery
// event's room multiplier (read at fire time). No vitals event fires:
// runtime inputs are not durable vitals state.
//
// Owner-local: call only from the sim owner goroutine (Run/Step) or in
// Step-driven tests.
func (e *Engine) PlayerSetVitalsRuntimeInputs(id EntityID, inputs PlayerVitalsRuntimeInputs) error {
	if err := inputs.Validate(); err != nil {
		return err
	}
	ent, err := e.resolvePlayer(id)
	if err != nil {
		return err
	}
	ent.runtimeInputs = inputs
	return nil
}

// PlayerVitalsRuntimeOf inspects a live player's runtime metadata
// immutably (spec §9.4b.14a): unknown ID -> ErrEntityNotFound; a
// known generic entity -> (zero value, false, nil); a player -> (value
// COPY, true, nil). A migrating entity still inspects read-only under
// its quiesced source ownership (same rule as PlayerVitalsOf). No
// mutable pointer into the registry escapes.
func (e *Engine) PlayerVitalsRuntimeOf(id EntityID) (PlayerVitalsRuntimeSnapshot, bool, error) {
	if rec, ok := e.registry.migrations[id]; ok {
		if !rec.entity.isPlayer {
			return PlayerVitalsRuntimeSnapshot{}, false, nil
		}
		return rec.entity.runtimeSnapshotValue(), true, nil
	}
	ent, err := e.registry.lookup(id)
	if err != nil {
		return PlayerVitalsRuntimeSnapshot{}, false, err
	}
	if !ent.isPlayer {
		return PlayerVitalsRuntimeSnapshot{}, false, nil
	}
	return ent.runtimeSnapshotValue(), true, nil
}

// runtimeSnapshotValue copies the entity's runtime metadata.
func (ent *entity) runtimeSnapshotValue() PlayerVitalsRuntimeSnapshot {
	return PlayerVitalsRuntimeSnapshot{
		Inputs:            ent.runtimeInputs,
		ActedSinceEntry:   ent.actedSinceEntry,
		HealthArmed:       ent.healthArmed,
		HealthDue:         ent.healthDue,
		ManaArmed:         ent.manaArmed,
		ManaDue:           ent.manaDue,
		RestArmed:         ent.restArmed,
		RestDue:           ent.restDue,
		StomachAnchorTick: ent.stomachAnchorTick,
	}
}

// PlayerStartResting arms the rest lifecycle (spec §9.4b.13): already
// resting -> exact no-op (same deadline); not resting -> arm from the
// current tick/current runtime inputs. No trance, message, or opcode
// logic (M5-T6). No vitals event fires (runtime metadata only).
//
// Owner-local: call only from the sim owner goroutine (Run/Step) or in
// Step-driven tests.
func (e *Engine) PlayerStartResting(id EntityID) error {
	ent, err := e.resolvePlayer(id)
	if err != nil {
		return err
	}
	if ent.restArmed {
		return nil
	}
	e.armRestDeadline(ent, e.tick.Load())
	return nil
}

// PlayerStopResting cancels the rest deadline if present (spec
// §9.4b.13); a second stop is a no-op. No vitals event fires.
//
// Owner-local: call only from the sim owner goroutine (Run/Step) or in
// Step-driven tests.
func (e *Engine) PlayerStopResting(id EntityID) error {
	ent, err := e.resolvePlayer(id)
	if err != nil {
		return err
	}
	ent.restArmed = false
	return nil
}

// PlayerIsResting reports the resting state, which IS the rest
// deadline's presence (spec §9.4b.13) — no independent boolean can
// disagree with the slot.
func (e *Engine) PlayerIsResting(id EntityID) (bool, error) {
	ent, err := e.resolvePlayer(id)
	if err != nil {
		return false, err
	}
	return ent.restArmed, nil
}

// PlayerMarkActedSinceEntry sets the first-action flag (spec
// §9.4b.16). Idempotent. Movement integration routes the same mark at
// input consumption; future T6/T7 actions call this hook. No vitals
// event fires.
//
// Owner-local: call only from the sim owner goroutine (Run/Step) or in
// Step-driven tests.
func (e *Engine) PlayerMarkActedSinceEntry(id EntityID) error {
	ent, err := e.resolvePlayer(id)
	if err != nil {
		return err
	}
	ent.actedSinceEntry = true
	return nil
}

// PlayerApplyEntryActedPolicy consumes an ALREADY-RESOLVED room-entry
// reset decision (spec §9.4b.16): shouldReset=true -> actedSinceEntry
// false; false -> preserve the current state. Voxilian assigns no
// ROOM_* flag bits here; the content-owning task resolves the policy.
// Technical 32 m cell handoff is NOT room entry and preserves the flag.
//
// Owner-local: call only from the sim owner goroutine (Run/Step) or in
// Step-driven tests.
func (e *Engine) PlayerApplyEntryActedPolicy(id EntityID, shouldReset bool) error {
	ent, err := e.resolvePlayer(id)
	if err != nil {
		return err
	}
	if shouldReset {
		ent.actedSinceEntry = false
	}
	return nil
}

// PlayerUpdateStomach lazily advances stomach decay off the sim-time
// anchor (spec §9.4b.15): elapsed whole seconds derive from the fixed
// tick domain (uint32 modular subtraction, guarded against an
// ambiguous >= 2^31 distance), DecayStomach is called EVEN WHEN
// wholeSeconds == 0 (so initial Stomach 0 source-faithfully becomes 1
// on the first explicit update), the result commits through the normal
// guarded dirty seam, and the anchor advances by EXACTLY the consumed
// whole-second ticks (preserving sub-second tick remainder; never
// anchor=current). No background timer, no offline digestion:
// remove/re-add/re-attach re-anchors at the current tick.
//
// Owner-local: call only from the sim owner goroutine (Run/Step) or in
// Step-driven tests.
func (e *Engine) PlayerUpdateStomach(id EntityID) (PlayerVitals, error) {
	ent, err := e.resolvePlayer(id)
	if err != nil {
		return PlayerVitals{}, err
	}
	current := e.tick.Load()
	elapsedTicks := current - ent.stomachAnchorTick // u32 modular
	if elapsedTicks >= 1<<31 {
		return ent.vitals, fmt.Errorf("sim: vitals stomach anchor %d vs current %d: %w",
			ent.stomachAnchorTick, current, ErrInvalidRuntimeElapsed)
	}
	wholeSeconds := elapsedTicks / uint32(e.tickHz)
	decayed, err := DecayStomach(ent.vitals.Stomach, int64(wholeSeconds))
	if err != nil {
		return ent.vitals, err
	}
	after := ent.vitals
	after.Stomach = decayed
	if err := e.commitVitals(ent, after); err != nil {
		return ent.vitals, err
	}
	// Advance by the consumed ticks only: wholeSeconds*tickHz <=
	// elapsedTicks < 2^31, so the u32 addition cannot overflow past
	// the already-wrapped modular domain.
	ent.stomachAnchorTick += wholeSeconds * uint32(e.tickHz)
	return after, nil
}
