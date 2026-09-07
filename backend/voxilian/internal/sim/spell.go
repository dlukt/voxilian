package sim

import (
	"errors"
	"math"
)

// Stable M5-T3a spell-domain errors (spec §9.3a.19). Matching MUST use
// errors.Is, never string parsing as control flow.
var (
	// ErrInvalidSpellPower marks a spell-power input outside the
	// source domain 1..99 (SPELLPOWER_MINIMUM..MAXIMUM). Zero,
	// negatives, and >99 are never silent clamps.
	ErrInvalidSpellPower = errors.New("sim: invalid spell power")
	// ErrInvalidSpellSchool marks an unknown CastSchool value.
	ErrInvalidSpellSchool = errors.New("sim: invalid spell school")
	// ErrInvalidSpellLevel marks a spell level outside 1..6.
	ErrInvalidSpellLevel = errors.New("sim: invalid spell level")
	// ErrInvalidManaCost marks a negative base/resolved mana cost.
	ErrInvalidManaCost = errors.New("sim: invalid mana cost")
	// ErrInvalidManaReduction marks an equipment mana-reduction
	// percent outside 0..100.
	ErrInvalidManaReduction = errors.New("sim: invalid mana reduction")
	// ErrInvalidExertion marks a spell exertion requirement outside
	// 0..100, or a negative vigor input.
	ErrInvalidExertion = errors.New("sim: invalid exertion")
	// ErrInvalidHitPointGate marks an impossible BaseMaxHP minimum
	// gate (BaseMaxHP < 1, or minimum < 0).
	ErrInvalidHitPointGate = errors.New("sim: invalid hit-point gate")
	// ErrInvalidCastTiming marks an impossible cast timing input
	// (negative durations, or interval arithmetic overflow).
	ErrInvalidCastTiming = errors.New("sim: invalid cast timing")
	// ErrInvalidSquaredDistance marks a negative squared distance.
	ErrInvalidSquaredDistance = errors.New("sim: invalid squared distance")
	// ErrInvalidCastOrigin marks an unknown CastOrigin value.
	ErrInvalidCastOrigin = errors.New("sim: invalid cast origin")
	// ErrInvalidDamagePolicy marks an unknown DamagePolicy value.
	ErrInvalidDamagePolicy = errors.New("sim: invalid damage policy")
)

// Spell-power domain (spec §9.3a.1, source blakston.khd).
const (
	minSpellPower = 1
	maxSpellPower = 99 // source SPELLPOWER_MAXIMUM, the damage divisor
)

// Spell success bounds (spec §9.3a.2, source spell.kod bound(num,5,95)).
const (
	minSpellChance = 5
	maxSpellChance = 95
)

// Spell-level domain (spec §9.3a.5, M59 spell levels L1..L6).
const (
	minSpellLevel = 1
	maxSpellLevel = 6
)

// Spell exertion domain (spec §9.3a.7, source viSpellExertion 0..100).
const maxSpellExertion = 100

// checkSpellPower rejects spell-power inputs outside 1..99.
func checkSpellPower(power int) error {
	if power < minSpellPower || power > maxSpellPower {
		return ErrInvalidSpellPower
	}
	return nil
}

// CastSchool is the T3a karma domain (spec §9.3a.5). Only the
// Qor/Shalille distinction carries a requirement; callers map every
// other ordinary school (Kraanan/Faren/Riija/Jala/DM) to SchoolOther.
// This is mechanics, not a content catalog: no spell IDs live here.
type CastSchool int

const (
	// SchoolQor requires negative karma (-10 per spell level).
	SchoolQor CastSchool = iota + 1
	// SchoolShalille requires positive karma (+10 per spell level).
	SchoolShalille
	// SchoolOther carries no karma requirement.
	SchoolOther
)

// String returns the stable school name (debug/test use).
func (s CastSchool) String() string {
	switch s {
	case SchoolQor:
		return "qor"
	case SchoolShalille:
		return "shalille"
	case SchoolOther:
		return "other"
	default:
		return "unknown"
	}
}

// RequireKarma returns the signed caster-karma requirement for a spell
// of the given school and level (spec §9.3a.5): Qor -10*level,
// Shalille +10*level, other schools 0. Level MUST be 1..6.
func RequireKarma(school CastSchool, level int) (int, error) {
	switch school {
	case SchoolQor, SchoolShalille, SchoolOther:
	default:
		return 0, ErrInvalidSpellSchool
	}
	if level < minSpellLevel || level > maxSpellLevel {
		return 0, ErrInvalidSpellLevel
	}
	switch school {
	case SchoolQor:
		return -10 * level, nil
	case SchoolShalille:
		return 10 * level, nil
	default:
		return 0, nil
	}
}

// KarmaEligible reports whether casterKarma satisfies the requirement
// (spec §9.3a.5, source KarmaCheck): a positive requirement passes iff
// casterKarma >= required, a negative one iff casterKarma <= required
// (equality passes both ways), and a zero requirement always allows.
// Karma values are trusted resolved integers (source stores hundredths
// divided down); they are not range-validated here.
func KarmaEligible(school CastSchool, level, casterKarma int) (bool, error) {
	required, err := RequireKarma(school, level)
	if err != nil {
		return false, err
	}
	switch {
	case required > 0:
		return casterKarma >= required, nil
	case required < 0:
		return casterKarma <= required, nil
	default:
		return true, nil
	}
}

// SpellSuccessChance computes the generic pre-noLOS spell success
// chance (spec §9.3a.2, source spell.kod SuccessChance):
//
//	base = ((100-requisiteStat)*spellPower)/100 + requisiteStat
//	chance = bound(base + ResolvedHinderDelta, 5, 95)
//
// with ONE truncation (product first, 64-bit intermediate).
// requisiteStat is the already-resolved division requisite
// (Mysticism/Stamina/Intellect by school); ResolvedHinderDelta is the
// already-resolved signed Jala/Hinder alteration applied BEFORE the
// bound (0 in the MVP; the Hinder implementation itself is phase 2).
// T3a performs no world queries. The no-LOS adjustment
// (ApplyNoLOSAdjustment) and the d100 roll (RollSpellSuccess) are
// separate stages.
func SpellSuccessChance(requisiteStat, spellPower, hinderDelta int) (int, error) {
	if err := checkStat(requisiteStat); err != nil {
		return 0, err
	}
	if err := checkSpellPower(spellPower); err != nil {
		return 0, err
	}
	// hinderDelta is a trusted resolved signed numeric (phase-2 Hinder
	// owns its range); it composes additively before the bound.
	base := satMul(int64(100)-int64(requisiteStat), int64(spellPower)) / d100Sides
	ch := satAddSigned(satAddSigned(base, int64(requisiteStat)), int64(hinderDelta))
	return int(boundInt64(ch, minSpellChance, maxSpellChance)), nil
}

// ApplyNoLOSAdjustment applies the exact source no-LOS distance rule
// (spec §9.3a.3) AFTER the 5..95 bound. It applies only when the
// already-resolved inputs state singleBattlerTarget AND NOT
// hasLineOfSight (the future runtime folds GetNumSpellTargets = 1,
// Battler-class, and non-immortal-DM conditions into these two
// booleans); otherwise chance returns unchanged. squaredDistance is
// the already-resolved integer squared distance (>= 0). This is NOT
// conventional distance falloff:
//
//	distance = squaredDistance/4 (integer division)
//	distance > chance/2  -> chance/2
//	distance < chance/2  -> chance-distance
//	equal                -> unchanged
//
// Both comparisons are strict; equality leaves chance unchanged.
// There is NO second clamp: the result may fall below 5 (even to
// 0/negative), exactly as source.
func ApplyNoLOSAdjustment(chance, squaredDistance int, singleBattlerTarget, hasLineOfSight bool) (int, error) {
	if squaredDistance < 0 {
		return 0, ErrInvalidSquaredDistance
	}
	if !singleBattlerTarget || hasLineOfSight {
		return chance, nil
	}
	distance := int64(squaredDistance) / 4
	half := int64(chance) / 2
	switch {
	case distance > half:
		return int(half), nil
	case distance < half:
		return int(int64(chance) - distance), nil
	default:
		return chance, nil
	}
}

// SpellSuccess is the deterministic trace of one spell success roll.
type SpellSuccess struct {
	Chance  int
	Roll    int
	Success bool
}

// RollSpellSuccess resolves one d100 spell-success roll (spec
// §9.3a.2): success iff roll <= chance. forceSuccess is the
// already-resolved ReagentRing in-use override: an ordinary failed
// roll with forceSuccess flips to success (no ring lookup, no
// charges, no inventory in T3a). chance is intentionally unvalidated
// (the post-distance value may lie below 5 by source design); the
// comparison is total. The roll MUST be drawn from the injected RNG;
// no second RNG implementation exists.
func RollSpellSuccess(rng RNG, chance int, forceSuccess bool) (SpellSuccess, error) {
	var res SpellSuccess
	roll, err := RollD100(rng)
	if err != nil {
		return res, err
	}
	res = SpellSuccess{Chance: chance, Roll: roll}
	if res.Roll <= res.Chance {
		res.Success = true
		return res, nil
	}
	res.Success = forceSuccess
	return res, nil
}

// SpellSucceeds reports the pure comparison d100 <= chance used by
// RollSpellSuccess (spec §9.3a.2). The roll MUST be 1..100.
func SpellSucceeds(chance, roll int) (bool, error) {
	if roll < 1 || roll > d100Sides {
		return false, ErrInvalidHitRoll
	}
	return roll <= chance, nil
}

// SpellManaCost computes the generic resolved mana cost (spec
// §9.3a.4, source spell.kod GetManaCost). baseMana is the
// already-resolved viMana (>= 0); reductionPct is the already-resolved
// scalar equipment reduction 0..100 (source: Princess Shield
// faction-rank percent; T3a never hard-codes the item):
//
//	base 0 -> 0 (bypasses everything, e.g. DM-style zero-mana spells)
//	power > 40 -> -1; power > 80 -> -1 (both strict)
//	cost -= ceil(cost*reductionPct/100)  [(cost*pct+99)/100]
//	floor at 1
func SpellManaCost(baseMana, spellPower, reductionPct int) (int, error) {
	if baseMana < 0 {
		return 0, ErrInvalidManaCost
	}
	if err := checkSpellPower(spellPower); err != nil {
		return 0, err
	}
	if reductionPct < 0 || reductionPct > 100 {
		return 0, ErrInvalidManaReduction
	}
	if baseMana == 0 {
		return 0, nil
	}
	cost := int64(baseMana)
	if spellPower > 40 {
		cost--
	}
	if spellPower > 80 {
		cost--
	}
	if reductionPct > 0 {
		cost -= (satMul(cost, int64(reductionPct)) + 99) / 100
	}
	return int(boundInt64(cost, 1, math.MaxInt64)), nil
}

// ManaAvailable mirrors the source CanPayManaVigor mana leg (spec
// §9.3a.4): currentMana >= cost passes (equality passes; strict <
// fails). Pure helper over immutable inputs; cost MUST be >= 0.
func ManaAvailable(currentMana, cost int) (bool, error) {
	if currentMana < 0 {
		return false, ErrInvalidManaCost
	}
	if cost < 0 {
		return false, ErrInvalidManaCost
	}
	return currentMana >= cost, nil
}

// checkExertion rejects spell exertion requirements outside 0..100.
func checkExertion(exertion int) error {
	if exertion < 0 || exertion > maxSpellExertion {
		return ErrInvalidExertion
	}
	return nil
}

// VigorAvailable mirrors source HasVigor with the STRICT threshold
// (spec §9.3a.7): currentVigor > required passes (equality DENIES).
// required is the viSpellExertion vigor-unit requirement (0..100).
// When checkEnabled is false (source vbCheck_exertion = FALSE) the
// gate is skipped and availability reports true.
func VigorAvailable(currentVigor, required int, checkEnabled bool) (bool, error) {
	if currentVigor < 0 {
		return false, ErrInvalidExertion
	}
	if err := checkExertion(required); err != nil {
		return false, err
	}
	if !checkEnabled {
		return true, nil
	}
	return currentVigor > required, nil
}

// SpellExertionCharge returns the integer exertion charge for one cast
// (spec §9.3a.7, §9.3a.11): full 10000*exertion on success, half with
// integer truncation on failure (source SpellFailed). Exertion MUST be
// 0..100. No float vigor, no live mutation.
func SpellExertionCharge(exertion int, failed bool) (int, error) {
	if err := checkExertion(exertion); err != nil {
		return 0, err
	}
	full := satMul(int64(ExertionPerVigor), int64(exertion))
	if failed {
		return int(full / 2), nil
	}
	return int(full), nil
}

// BaseMaxGateAllows freezes the generic piMinHitPoints gate (spec
// §9.3a.6, CORRECTED from the research summary): the source gate is
// caster.GetLevel() < piMinHitPoints and Player GetLevel() returns
// BaseMaxHealth, so the gate is over authoritative UNBUFFED BaseMaxHP
// — never current HP, never buffed MaxHP. Allow iff
// baseMaxHP >= minimum (equality passes; strict < denies). minimum 0
// (source default) always allows. Current HP MUST NOT appear here.
func BaseMaxGateAllows(baseMaxHP, minimum int) (bool, error) {
	if baseMaxHP < 1 || minimum < 0 {
		return false, ErrInvalidHitPointGate
	}
	return baseMaxHP >= minimum, nil
}

// ReagentState is the already-resolved preflight outcome (spec
// §9.3a.8). T3a invents no reagent item IDs; values are constructed
// only by PlanReagentPreflight (output-only domain, never validated
// on input paths).
type ReagentState int

const (
	// ReagentAvailable means inventory holds the required reagents.
	ReagentAvailable ReagentState = iota + 1
	// ReagentSubstituted means a substitute/ReagentRing satisfies the
	// requirement (its charge is already consumed/reserved in
	// preflight, before the later success roll).
	ReagentSubstituted
	// ReagentMissing means neither inventory nor substitute satisfies
	// the requirement.
	ReagentMissing
)

// String returns the stable state name (debug/test use).
func (s ReagentState) String() string {
	switch s {
	case ReagentAvailable:
		return "available"
	case ReagentSubstituted:
		return "substituted"
	case ReagentMissing:
		return "missing"
	default:
		return "unknown"
	}
}

// ReagentPlan is the DATA/transaction-plan output of reagent
// preflight (spec §9.3a.8): no inventory mutation, no charge
// counters in T3a.
type ReagentPlan struct {
	// State is the resolved preflight outcome.
	State ReagentState
	// Proceed is true unless the requirement is unsatisfied.
	Proceed bool
	// SubstituteConsumed records that a substitute charge was already
	// consumed/reserved during preflight. A later spell-roll failure
	// does NOT retroactively restore it.
	SubstituteConsumed bool
}

// PlanReagentPreflight resolves the reagent preflight plan from
// already-resolved booleans (spec §9.3a.8): inventory available
// proceeds with no substitute use; inventory missing with a
// substitute available proceeds with the substitute charge consumed;
// inventory missing with no substitute does not proceed. Total over
// booleans (no validation error domain).
func PlanReagentPreflight(hasInventory, hasSubstitute bool) ReagentPlan {
	switch {
	case hasInventory:
		return ReagentPlan{State: ReagentAvailable, Proceed: true}
	case hasSubstitute:
		return ReagentPlan{State: ReagentSubstituted, Proceed: true, SubstituteConsumed: true}
	default:
		return ReagentPlan{State: ReagentMissing}
	}
}

// PostcastReady is the post-cast cooldown primitive in simulation
// time (spec §9.3a.10, source IsOkayAttackTime): invalid timing input
// is rejected regardless of attempt state; then the first attempt
// (hasPriorAttempt == false) is always allowed; otherwise allowed iff
// unsigned mod-2^32 (nowTick-lastAttemptTick) >=
// postCastSeconds*tickHz. On an allowed attempt the caller arms
// lastAttemptTick = nowTick immediately at the source-equivalent
// stage (§9.3a.9: armed even if a later gate fails); rejected
// too-early attempts MUST NOT re-arm. postCastSeconds 0 (valid) is
// always allowed. tickHz MUST be 1..120; negative seconds or
// interval overflow are ErrInvalidCastTiming. A valid int64 interval
// larger than MaxUint32 is NOT an error: the first attempt is still
// allowed, while a prior attempt simply never reaches it (u32 elapsed
// distance is always < 2^32). No goroutine/timer; wrap-safe by
// unsigned arithmetic.
func PostcastReady(hasPriorAttempt bool, lastAttemptTick, nowTick uint32, tickHz, postCastSeconds int) (bool, error) {
	if tickHz < minTickHz || tickHz > maxTickHz {
		return false, ErrInvalidTickHz
	}
	if postCastSeconds < 0 {
		return false, ErrInvalidCastTiming
	}
	if int64(postCastSeconds) > math.MaxInt64/int64(tickHz) {
		return false, ErrInvalidCastTiming
	}
	if !hasPriorAttempt {
		return true, nil
	}
	interval := int64(postCastSeconds) * int64(tickHz)
	if interval > math.MaxUint32 {
		// The unsigned elapsed distance (always < 2^32) can never
		// reach the interval: never elapsed yet, no error.
		return false, nil
	}
	return nowTick-lastAttemptTick >= uint32(interval), nil
}

// TranceDurationMs computes the source cast/trance duration (spec
// §9.3a.13, source spell.kod GetTranceTime): base 0 -> 0, else
// (baseCastMs*(150-spellPower))/100 with ONE truncation (* first,
// 64-bit intermediate). Normal power 1..99 yields 149%..51%.
// Negative base is ErrInvalidCastTiming. (Source immortal-DM
// zero-trance and the Elusion override are runtime/content, not T3a.)
// No Warp Time implementation.
func TranceDurationMs(baseCastMs, spellPower int) (int, error) {
	if baseCastMs < 0 {
		return 0, ErrInvalidCastTiming
	}
	if err := checkSpellPower(spellPower); err != nil {
		return 0, err
	}
	if baseCastMs == 0 {
		return 0, nil
	}
	prod := satMul(int64(baseCastMs), int64(150-spellPower))
	return int(prod / 100), nil
}

// CastTicks converts a cast/trance duration to sim ticks (spec
// §9.3a.13): ticks = ceil(durationMs*tickHz/1000) so a cast never
// completes earlier than the source duration. Voxilian sim creates no
// wall-clock timer goroutines. Negative ms, tickHz outside 1..120, or
// product overflow are domain errors. No hidden floating point.
func CastTicks(durationMs, tickHz int) (int, error) {
	if durationMs < 0 {
		return 0, ErrInvalidCastTiming
	}
	if tickHz < minTickHz || tickHz > maxTickHz {
		return 0, ErrInvalidTickHz
	}
	prod := satMul(int64(durationMs), int64(tickHz))
	// ceil(prod/1000) without overflow: prod/1000 + (prod%1000 != 0).
	ticks := prod/1000 + boolToInt64(prod%1000 != 0)
	if ticks > math.MaxInt32 {
		return 0, ErrInvalidCastTiming
	}
	return int(ticks), nil
}

// TranceRequired reports whether a cast enters trance (spec §9.3a.14:
// tranceMs > 0). T3a owns no enchantment/timer state.
func TranceRequired(tranceMs int) bool {
	return tranceMs > 0
}

// SpellPaymentInput carries the already-resolved inputs for one
// cast's resource-payment plan (spec §9.3a.11–§9.3a.12). T3a returns
// a value/result plan; it does NOT mutate resources.
type SpellPaymentInput struct {
	// Origin selects the payment regime (§9.3a.16): player pays full
	// costs; item and monster casts pay nothing here.
	Origin CastOrigin
	// ManaCost is the resolved §9.3a.4 cost (>= 0).
	ManaCost int
	// Exertion is the viSpellExertion requirement (0..100).
	Exertion int
	// RollSucceeded is the resolved spell-payment roll outcome
	// (SuccessChance incl. ForceSuccess rescue, or the resist-path
	// consolation roll in §9.3a.12).
	RollSucceeded bool
	// TargetResisted marks the §9.3a.12 enchantment-resist path:
	// still resolve the ordinary payment roll (full or half per the
	// roll), with no effect and no trance. NOT the T2 numeric
	// ResistanceCheck.
	TargetResisted bool
	// ReagentsAvailable marks normal inventory reagents present for
	// consumption on a successful player cast.
	ReagentsAvailable bool
	// SubstituteConsumed echoes the already-resolved preflight
	// substitute use (§9.3a.8): it REMAINS consumed even when the
	// later roll fails.
	SubstituteConsumed bool
}

// SpellPayment is the resource-payment result plan for one cast.
type SpellPayment struct {
	// ManaCharge is the mana to charge: full cost on success,
	// cost/2 (integer truncation) on failure, 0 for item/monster.
	ManaCharge int
	// ExertionCharge is the exertion to charge: 10000*exertion on
	// success, half on failure, 0 for item/monster.
	ExertionCharge int
	// ConsumeReagents marks normal inventory reagent consumption:
	// YES only on a successful non-resisted player cast with
	// reagents available; never on failure, never for item/monster.
	ConsumeReagents bool
	// SubstituteConsumed echoes the preflight substitute use: it
	// survives roll failure (no retroactive restore).
	SubstituteConsumed bool
	// BeginTrance marks that trance may begin: successful
	// non-resisted player casts only (items cast directly, monsters
	// cast without trance, failures and resists never trance).
	BeginTrance bool
	// QualifiesForImprovement is the pure successful-cast hook flag
	// for future M6 composition (source gates ImproveAbility on NOT
	// bItemCast; resisted casts never reach CastSpell): success AND
	// player origin AND NOT resisted. No advancement is implemented.
	QualifiesForImprovement bool
}

// ResolveSpellPayment computes the full-vs-failed resource-payment
// plan (spec §9.3a.11–§9.3a.12, §9.3a.16). Payment resolves BEFORE
// the cast/trance timer begins (binding source UserCast order); a
// broken trance never refunds and completion never re-pays (those
// contracts constrain the future runtime, which consumes this plan
// exactly once).
func ResolveSpellPayment(in SpellPaymentInput) (SpellPayment, error) {
	switch in.Origin {
	case OriginPlayer, OriginItem, OriginMonster:
	default:
		return SpellPayment{}, ErrInvalidCastOrigin
	}
	if in.ManaCost < 0 {
		return SpellPayment{}, ErrInvalidManaCost
	}
	if err := checkExertion(in.Exertion); err != nil {
		return SpellPayment{}, err
	}
	out := SpellPayment{SubstituteConsumed: in.SubstituteConsumed}
	if in.Origin != OriginPlayer {
		// Item casts skip mana/vigor/reagent payment and cast
		// directly (no trance); monster casts pay nothing and
		// trance nothing. Karma still gates item casts, but that
		// gate lives outside this payment plan.
		return out, nil
	}
	fullExertion := satMul(int64(ExertionPerVigor), int64(in.Exertion))
	if in.RollSucceeded {
		out.ManaCharge = in.ManaCost
		out.ExertionCharge = int(fullExertion)
		// The resisted path (§9.3a.12) runs the ordinary PayCosts,
		// which deletes inventory reagents on success even though
		// the cast has no effect and starts no trance.
		if in.ReagentsAvailable {
			out.ConsumeReagents = true
		}
		if !in.TargetResisted {
			out.BeginTrance = true
			out.QualifiesForImprovement = true
		}
		return out, nil
	}
	// Failed roll without override (source SpellFailed: half mana,
	// half exertion, no normal reagents, no trance). A substitute
	// charge already consumed in preflight REMAINS consumed
	// (echoed above).
	out.ManaCharge = in.ManaCost / 2
	out.ExertionCharge = int(fullExertion / 2)
	return out, nil
}
