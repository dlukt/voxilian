package sim

import (
	"errors"
	"math"
)

// Stable M5-T3b special-spell errors (spec §9.3b.14). Matching MUST use
// errors.Is, never string parsing as control flow.
var (
	// ErrInvalidSpecialVictim marks an impossible special-spell
	// victim snapshot: unknown kind, MaxHP < 1, HP < 1, Intellect
	// outside the source-valid 1..70 effective domain, or
	// Difficulty < 1.
	ErrInvalidSpecialVictim = errors.New("sim: invalid special victim")
	// ErrInvalidWallKind marks an unknown WallDamageKind value.
	ErrInvalidWallKind = errors.New("sim: invalid wall kind")
	// ErrInvalidEarthquakeSeverity marks an earthquake severity or
	// percent outside its valid domain (severity < 1, percent
	// outside 0..100).
	ErrInvalidEarthquakeSeverity = errors.New("sim: invalid earthquake severity")
	// ErrInvalidWallLifetime marks an impossible wall lifetime
	// input (negative base seconds).
	ErrInvalidWallLifetime = errors.New("sim: invalid wall lifetime")
)

// TouchProficiency resolves the touch-attack proficiency component
// (spec §9.3b.1, source touchatk.kod GetProf):
//
//	proficiency = max(PunchAbility, (Mysticism*3)/2)
//
// Integer order is binding: Mysticism*3 first (64-bit), then /2 —
// never Mysticism*(3/2). Mysticism is the already-resolved
// effective attribute; punchAbility the resolved Punch skill (0 when
// unknown). Negative inputs are ErrInvalidCombatStat. The result
// feeds the REAL T1 PlayerOffense (no second offense formula); the
// dead viHit_Factor classvar is deliberately NOT consumed.
func TouchProficiency(punchAbility, mysticism int) (int, error) {
	if punchAbility < 0 || mysticism < 0 {
		return 0, ErrInvalidCombatStat
	}
	contrib := satMul(int64(mysticism), 3) / 2
	if int64(punchAbility) > contrib {
		return punchAbility, nil
	}
	return int(contrib), nil
}

// RollTouchDamage resolves one generic touch-attack damage event
// (spec §9.3b.2, source touchatk.kod FindDamage):
//
//	r = inclusive Random(minDamage, maxDamage)
//	half = r/2            (integer truncation BEFORE the power multiply)
//	damage = half + (half*spellPower)/99 + 1
//	damage = bound(damage, 1, $)
//
// Divisor is SPELLPOWER_MAXIMUM = 99. Generic DamageFactors is
// identity here (Holy Touch applies separately via
// ApplyHolyTouchModifier). No Might, no weapon quality, no
// proficiency flat bonus, no T1 RawWeaponDamage: touch owns its
// formula. Spell power is the T3a valid 1..99 domain.
func RollTouchDamage(rng RNG, minDamage, maxDamage, spellPower int) (int, error) {
	if minDamage < 0 || maxDamage < 0 {
		return 0, ErrInvalidDamageValue
	}
	if err := checkSpellPower(spellPower); err != nil {
		return 0, err
	}
	raw, err := rollBounded(rng, minDamage, maxDamage)
	if err != nil {
		return 0, err
	}
	half := int64(raw) / 2
	damage := satAdd(half, satMul(half, int64(spellPower))/maxSpellPower)
	// The source +1 guarantees at least 1; saturate instead of
	// overflowing on absurd inputs.
	if damage == math.MaxInt64 {
		return int(damage), nil
	}
	damage++
	if damage < 1 {
		damage = 1
	}
	return int(damage), nil
}

// ApplyHolyTouchModifier applies the source-audited Holy Touch
// damage-factor override (spec §9.3b.6, source holytch.kod
// DamageFactors) over already-resolved inputs — no victim object,
// no IsUndead/GetKarma callbacks, no catalog ID:
//
//	undead victim:  damage = damage * 2
//	non-undead:     damage = damage + ((-victimKarma)*damage)/200
//
// C truncation toward zero on /200 (64-bit SIGNED intermediate;
// negative karma is an ordinary resolved input, never an error):
// negative karma increases damage, zero leaves it unchanged,
// positive karma decreases it; undead exactly doubles with the karma
// path skipped. damage MUST be >= 0.
func ApplyHolyTouchModifier(damage, victimKarma int, undead bool) (int, error) {
	if damage < 0 {
		return 0, ErrInvalidDamageValue
	}
	if undead {
		return int(satMul(int64(damage), 2)), nil
	}
	k := int64(victimKarma)
	var neg int64
	if k == math.MinInt64 {
		neg = math.MaxInt64 // saturate the unrepresentable negation
	} else {
		neg = -k
	}
	term := satMulSigned(neg, int64(damage)) / 200
	return int(satAddSigned(int64(damage), term)), nil
}

// TouchDurationMs computes the touch-enchantment duration value
// (spec §9.3b.4, source touchatk.kod GetDuration):
//
//	secondsUnit = inclusive Random(spellPower/3, spellPower/2)
//	secondsUnit = bound(secondsUnit, 10, 75)
//	durationMs = secondsUnit * 6 * 1000
//
// Integer spellPower/3 and spellPower/2 evaluate BEFORE the draw.
// Value-only: no timers. Future runtime composes with T3a CastTicks;
// no duplicate tick conversion. Spell power is T3a 1..99.
func TouchDurationMs(rng RNG, spellPower int) (int, error) {
	if err := checkSpellPower(spellPower); err != nil {
		return 0, err
	}
	lo := spellPower / 3
	hi := spellPower / 2
	unit, err := rollBounded(rng, lo, hi)
	if err != nil {
		return 0, err
	}
	unit = int(boundInt64(int64(unit), 10, 75))
	return unit * 6 * 1000, nil
}

// IllusionaryVictimKind selects the IW formula branch (spec §9.3b.7).
type IllusionaryVictimKind int

const (
	// VictimPlayer uses the Intellect base over buffed MaxHP.
	VictimPlayer IllusionaryVictimKind = iota + 1
	// VictimMonster uses the Difficulty base over max hit points.
	VictimMonster
)

// IllusionaryVictim is the immutable victim snapshot for
// Illusionary Wounds (spec §9.3b.7). Only the selected kind's
// fields are consumed. No live entity pointer.
type IllusionaryVictim struct {
	Kind IllusionaryVictimKind
	// Intellect is the already-resolved effective Intellect
	// (source-valid domain 1..70); consumed for players only.
	Intellect int
	// Difficulty is the already-resolved monster difficulty (>= 1);
	// consumed for monsters only.
	Difficulty int
	// MaxHP is source GetMaxHealth for players (BUFFED max, not
	// BaseMaxHP) and ReturnMaxHitPoints for monsters. HP is current
	// health; living targets only (both >= 1).
	MaxHP int
	HP    int
}

// IllusionaryWoundsResult is the pre-application IW value (spec
// §9.3b.8–§9.3b.9, B9). Policy is always PolicyAbsolute. No damage
// is applied here; T4/T7 own HP application.
type IllusionaryWoundsResult struct {
	Loss       int
	DurationMs int
	Policy     DamagePolicy
}

// checkIllusionaryVictim validates the snapshot domain.
func checkIllusionaryVictim(v IllusionaryVictim) error {
	switch v.Kind {
	case VictimPlayer:
		if v.Intellect < 1 || v.Intellect > 70 {
			return ErrInvalidSpecialVictim
		}
	case VictimMonster:
		if v.Difficulty < 1 {
			return ErrInvalidSpecialVictim
		}
	default:
		return ErrInvalidSpecialVictim
	}
	if v.MaxHP < 1 || v.HP < 1 {
		return ErrInvalidSpecialVictim
	}
	return nil
}

// IllusionaryWoundsLoss computes the absolute HP-loss value (spec
// §9.3b.7–§9.3b.8, source illwound.kod GetHPLoss at factor 1):
//
//	Player:   base = 17 + (50-Intellect)/10
//	Monster:  base = 30 - bound(Difficulty*2, 1, 20)
//	loss = (base*spellPower)/100          (divisor 100, NOT 99)
//	loss = bound(loss, 0, MaxHP/3)        (floor)
//	loss = bound(loss, 0, HP-1)
//
// Binding cap order: MaxHP/3 first, then HP-1. Never lethal
// (HP=1 yields 0); may return 0. The source iFactor division is
// unreachable (no live caller passes iFactor > 1) and is NOT
// implemented. Do NOT route through ApplyPlayerDamageCaps,
// ApplyResistance, or ApplyDefenseModifiers: absolute path.
func IllusionaryWoundsLoss(victim IllusionaryVictim, spellPower int) (int, error) {
	if err := checkIllusionaryVictim(victim); err != nil {
		return 0, err
	}
	if err := checkSpellPower(spellPower); err != nil {
		return 0, err
	}
	var base int64
	if victim.Kind == VictimPlayer {
		base = 17 + (int64(50-victim.Intellect))/10
	} else {
		base = 30 - boundInt64(satMulSigned(int64(victim.Difficulty), 2), 1, 20)
	}
	loss := satMul(base, int64(spellPower)) / 100
	loss = boundInt64(loss, 0, int64(victim.MaxHP)/3)
	loss = boundInt64(loss, 0, int64(victim.HP)-1)
	return int(loss), nil
}

// IllusionaryWoundsDuration computes the IW enchantment duration
// value (spec §9.3b.8, source GetDuration):
//
//	durationMs = 20000 + spellPower*750, bound(..., 20000, 80000)
//
// 20 s..80 s. Value-only, no timer. Spell power is T3a 1..99.
func IllusionaryWoundsDuration(spellPower int) (int, error) {
	if err := checkSpellPower(spellPower); err != nil {
		return 0, err
	}
	d := satAdd(20000, satMul(int64(spellPower), 750))
	return int(boundInt64(d, 20000, 80000)), nil
}

// RollIllusionaryWounds composes the loss, duration, and absolute
// policy into one pre-application value (spec §9.3b.8). No RNG is
// involved (the IW formula is deterministic); no damage applied.
func RollIllusionaryWounds(victim IllusionaryVictim, spellPower int) (IllusionaryWoundsResult, error) {
	var res IllusionaryWoundsResult
	loss, err := IllusionaryWoundsLoss(victim, spellPower)
	if err != nil {
		return res, err
	}
	dur, err := IllusionaryWoundsDuration(spellPower)
	if err != nil {
		return res, err
	}
	return IllusionaryWoundsResult{Loss: loss, DurationMs: dur, Policy: PolicyAbsolute}, nil
}

// IllusionaryRefund resolves the IW expiration refund (spec §9.3b.8,
// source EndEnchantmentEffects): the INITIALLY APPLIED loss was
// stored as enchantment state; on expiration restore that stored
// amount iff the victim is alive, else 0. Never a re-roll, never a
// re-computation. No healing mutation here. appliedLoss MUST be >= 0.
func IllusionaryRefund(appliedLoss int, victimAlive bool) (int, error) {
	if appliedLoss < 0 {
		return 0, ErrInvalidDamageValue
	}
	if !victimAlive {
		return 0, nil
	}
	return appliedLoss, nil
}

// VampiricDrainHeal computes the heal amount from the
// POST-application damage result (spec §9.3b.9, source
// vampdrn.kod DoSideEffect with DAMAGE_FACTOR_TO_HEAL = 2).
// killed MUST be an explicit boolean (source nil-magic is not
// reproduced):
//
//	nonlethal:  heal = bound(appliedDamage/2, 1, $)
//	lethal:     heal = bound(resolvedBaseDamageMax/2, 1, $)
//
// The lethal path IGNORES the applied scalar and uses the
// caller-supplied resolved prototype max (source Vampiric Drain
// piDamageMax = 18 → 9; that number is a test vector, never
// production catalog content). appliedDamage < 0 is
// ErrInvalidDamageValue; resolvedBaseDamageMax < 1 is
// ErrInvalidDamageValue. No HP state, no GainHealth, no T3a raw
// damage duplication.
func VampiricDrainHeal(appliedDamage int, killed bool, resolvedBaseDamageMax int) (int, error) {
	if resolvedBaseDamageMax < 1 {
		return 0, ErrInvalidDamageValue
	}
	if killed {
		heal := resolvedBaseDamageMax / 2
		if heal < 1 {
			heal = 1
		}
		return heal, nil
	}
	if appliedDamage < 0 {
		return 0, ErrInvalidDamageValue
	}
	heal := appliedDamage / 2
	if heal < 1 {
		heal = 1
	}
	return heal, nil
}

// Earthquake mechanics constants (spec §9.3b.10–§9.3b.11, source
// earthqua.kod): 5..9 dice, item-self MaxDamage 9, squared-distance
// thresholds 64/400. Mechanics constants for this archetype, not a
// catalog spell ID.
const (
	quakeDamageMin   = 5
	quakeDamageMax   = 9
	quakeItemMax     = 9
	quakeFullSquared = 64                                  // 8*8
	quakeZeroSquared = 400                                 // 20*20
	quakeDenominator = quakeZeroSquared - quakeFullSquared // 336
)

// EarthquakeSeverity computes the normal-cast severity (spec
// §9.3b.10, source CastSpell):
//
//	severity = 1 + spellPower/25
//
// Integer division. Spell power T3a 1..99; no invented clamp
// (24→1, 25→2, 49→2, 50→3, 74→3, 75→4, 99→4).
func EarthquakeSeverity(spellPower int) (int, error) {
	if err := checkSpellPower(spellPower); err != nil {
		return 0, err
	}
	return 1 + spellPower/25, nil
}

// checkEarthquakeSeverity rejects severity < 1.
func checkEarthquakeSeverity(severity int) error {
	if severity < 1 {
		return ErrInvalidEarthquakeSeverity
	}
	return nil
}

// EarthquakeDamagePercent computes the squared-distance damage
// percent (spec §9.3b.10, source ComputeDamage):
//
//	squaredDistance <= 64:   100
//	squaredDistance > 400:   0
//	else: 100*(400-squaredDistance)/(400-64)
//
// Integer truncation AFTER the 100*(...) multiplication (64-bit).
// Exactly 400 yields 0 via interpolation; above 400 is also 0.
// Input is already-resolved SQUARED distance: no sqrt, no float;
// negative is ErrInvalidSquaredDistance.
func EarthquakeDamagePercent(squaredDistance int) (int, error) {
	if squaredDistance < 0 {
		return 0, ErrInvalidSquaredDistance
	}
	sq := int64(squaredDistance)
	switch {
	case sq <= quakeFullSquared:
		return 100, nil
	case sq > quakeZeroSquared:
		return 0, nil
	default:
		return int(satMul(100, quakeZeroSquared-sq) / quakeDenominator), nil
	}
}

// RollEarthquakeDamage resolves caster-mode earthquake damage
// (spec §9.3b.11, source ComputeDamage):
//
//	roll = inclusive Random(5, 9)
//	damage = (roll*severity)*percent/100
//
// Operation order binding: (roll*severity) first (64-bit), then
// *percent, ONE truncation at /100. Zero percent may yield zero —
// do NOT floor to 1 in T3b. severity MUST be >= 1; percent MUST be
// 0..100 (both ErrInvalidEarthquakeSeverity).
func RollEarthquakeDamage(rng RNG, severity, percent int) (int, error) {
	if err := checkEarthquakeSeverity(severity); err != nil {
		return 0, err
	}
	if percent < 0 || percent > 100 {
		return 0, ErrInvalidEarthquakeSeverity
	}
	roll, err := rollBounded(rng, quakeDamageMin, quakeDamageMax)
	if err != nil {
		return 0, err
	}
	return int(satMul(satMul(int64(roll), int64(severity)), int64(percent)) / 100), nil
}

// RollEnvironmentalEarthquakeDamage resolves environmental-mode
// damage (spec §9.3b.11): inclusive Random(5,9)*severity. No
// caster-position input. severity MUST be >= 1.
func RollEnvironmentalEarthquakeDamage(rng RNG, severity int) (int, error) {
	if err := checkEarthquakeSeverity(severity); err != nil {
		return 0, err
	}
	roll, err := rollBounded(rng, quakeDamageMin, quakeDamageMax)
	if err != nil {
		return 0, err
	}
	return int(satMul(int64(roll), int64(severity))), nil
}

// EarthquakeItemSelfDamage resolves the player item/scroll
// self-damage special case (spec §9.3b.11, source
// scroll-punishment rule): MaxDamage*severity with MaxDamage = 9.
// NO RNG for this self-hit. Normal non-item player self-damage uses
// RollEarthquakeDamage at squared distance zero instead. severity
// MUST be >= 1.
func EarthquakeItemSelfDamage(severity int) (int, error) {
	if err := checkEarthquakeSeverity(severity); err != nil {
		return 0, err
	}
	return int(satMul(quakeItemMax, int64(severity))), nil
}

// EarthquakeSignature is the ordinary quake attack signature (spec
// §9.3b.11, source viAttack_spell = SPELL_ALL + QUAKE, atype 0):
// pure spell, policy ordinary. T2 armor DamageReduce is bypassed as
// pure spell; T2 resistance still applies; no absolute behavior.
func EarthquakeSignature() DamageSignature {
	return DamageSignature{
		Weapon: 0,
		Spell:  uint32(ResistSpellAll) | uint32(ResistSpellQuake),
	}
}
