package sim

import (
	"errors"
	"math"
)

// Stable combat-domain errors (spec §9.1.16). Matching MUST use
// errors.Is, never string parsing as control flow.
var (
	// ErrInvalidCombatRating marks a non-positive offense/defense rating
	// input to HitChance, or a negative level/difficulty to
	// MonsterRating. Rating constructors normalize valid inputs; this
	// guards the residual divide-by-zero / impossible-domain edge.
	ErrInvalidCombatRating = errors.New("sim: invalid combat rating")
	// ErrInvalidCombatStat marks a negative impossible ability/attribute
	// input (stroke, proficiency, aim, agility, parry/block/dodge,
	// BaseMaxHP, proficiency, attribute). Zero is a legal arithmetic
	// input; negatives are not.
	ErrInvalidCombatStat = errors.New("sim: invalid combat stat")
	// ErrInvalidRange marks a bounded-roll range with min > max.
	ErrInvalidRange = errors.New("sim: invalid roll range")
	// ErrNilRNG marks a nil RNG passed to a roll helper.
	ErrNilRNG = errors.New("sim: nil RNG")
	// ErrInvalidHitRoll marks a d100 value outside 1..100.
	ErrInvalidHitRoll = errors.New("sim: invalid hit roll")
	// ErrInvalidVictimSnapshot marks an impossible victim snapshot
	// (BaseMaxHP < 1 or HP < 0) for the damage-cap primitive.
	ErrInvalidVictimSnapshot = errors.New("sim: invalid victim snapshot")
	// ErrInvalidTickHz marks a tickHz outside the configured 1..120
	// range for the swing-cooldown primitive.
	ErrInvalidTickHz = errors.New("sim: invalid tickHz")
	// ErrInvalidDamageValue marks an impossible damage input (negative
	// raw damage, or non-lethal applied damage < 1, or a player-victim
	// MaxHP < 1) for caps/severity.
	ErrInvalidDamageValue = errors.New("sim: invalid damage value")
)

// Combat rating bounds (spec §9.1.3–§9.1.5).
const (
	minCombatRating    = 1
	maxPlayerRating    = 1000
	maxMonsterRating   = 1500
	minHitChance       = 10
	maxHitChance       = 95
	equalChanceHit     = 55 // source EQUAL_CHANCE_HIT
	maxDamagePerHit    = 30 // source MAX_DAMAGE_PER_HIT
	healthDamageFract  = 3  // source MAX_HEALTH_DAMAGE_FRACTION
	d100Sides          = 100
	severityWoundAbove = 5  // source DAMAGE_THRESHOLD_WOUND
	severityDmgAbove   = 15 // source DAMAGE_THRESHOLD_DAMAGE
)

// satMul returns a*b saturating at math.MaxInt64. Operands MUST be
// non-negative (all T1 combat operands are); saturation feeds the final
// bound clamp, so no overflow can flip a sign or bypass a cap.
func satMul(a, b int64) int64 {
	if a == 0 || b == 0 {
		return 0
	}
	if a > math.MaxInt64/b {
		return math.MaxInt64
	}
	return a * b
}

// satAdd returns a+b saturating at math.MaxInt64. Operands MUST be
// non-negative.
func satAdd(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// boundInt64 clamps v into [lo, hi] (source C_Bound with integer args).
func boundInt64(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// checkStat rejects negative impossible stat inputs.
func checkStat(v int) error {
	if v < 0 {
		return ErrInvalidCombatStat
	}
	return nil
}

// PlayerOffenseInput carries already-resolved integer inputs for the
// player offense formula (spec §9.1.3). Stroke/Proficiency are the
// wielded-weapon abilities (unarmed: Punch/Brawling); Aim is the
// effective attribute; WeaponHitMod is the resolved family+quality hit
// modifier plus any resolved numeric enchant HitBonus; ExtraMods carries
// future already-resolved additive attack modifiers (0 in T1).
type PlayerOffenseInput struct {
	Stroke       int
	Proficiency  int
	Aim          int
	BaseMaxHP    int
	WeaponHitMod int
	ExtraMods    int
}

// PlayerOffense computes
// Stroke*3 + Proficiency*2 + Aim*4 + (BaseMaxHP*3)/2 + WeaponHitMod +
// ExtraMods, bound 1..1000. The BaseMaxHP term evaluates left-to-right
// with one truncation: ((BaseMaxHP*3)/2).
func PlayerOffense(in PlayerOffenseInput) (int, error) {
	for _, v := range []int{in.Stroke, in.Proficiency, in.Aim, in.BaseMaxHP} {
		if err := checkStat(v); err != nil {
			return 0, err
		}
	}
	// WeaponHitMod/ExtraMods are trusted resolved numerics (future
	// buff/enchant systems own their ranges); they compose additively
	// and the final bound still holds.
	base := int64(in.BaseMaxHP)
	off := satMul(int64(in.Stroke), 3)
	off = satAdd(off, satMul(int64(in.Proficiency), 2))
	off = satAdd(off, satMul(int64(in.Aim), 4))
	off = satAdd(off, satMul(base, 3)/2)
	off = satAddSigned(off, satAddSigned(int64(in.WeaponHitMod), int64(in.ExtraMods)))
	return int(boundInt64(off, minCombatRating, maxPlayerRating)), nil
}

// PlayerDefenseInput carries already-resolved integer inputs for the
// player defense arithmetic (spec §9.1.4). Unavailable components are
// already-zeroed numbers (no weapon -> Parry 0, no shield -> Block 0,
// failed cost check -> Dodge 0); T1 takes plain numbers, no capability
// flags and no armor/shield objects. ExtraMods carries future
// already-resolved scalar defense-power terms (0 in T1; M5-T2 owns the
// armor/shield sources).
type PlayerDefenseInput struct {
	Parry     int
	Block     int
	Dodge     int
	Agility   int
	BaseMaxHP int
	ExtraMods int
}

// PlayerDefense computes
// Parry*2 + Block + Dodge*3 + Agility*4 + (BaseMaxHP*3)/2 + ExtraMods,
// bound 1..1000.
func PlayerDefense(in PlayerDefenseInput) (int, error) {
	for _, v := range []int{in.Parry, in.Block, in.Dodge, in.Agility, in.BaseMaxHP} {
		if err := checkStat(v); err != nil {
			return 0, err
		}
	}
	base := int64(in.BaseMaxHP)
	def := satMul(int64(in.Parry), 2)
	def = satAdd(def, int64(in.Block))
	def = satAdd(def, satMul(int64(in.Dodge), 3))
	def = satAdd(def, satMul(int64(in.Agility), 4))
	def = satAdd(def, satMul(base, 3)/2)
	def = satAddSigned(def, int64(in.ExtraMods))
	return int(boundInt64(def, minCombatRating, maxPlayerRating)), nil
}

// MonsterRating computes the generic M59 monster combat rating
// (spec §9.1.5): 3*Level + 60*Difficulty, bound 1..1500. The same
// primitive serves monster Offense and Defense. Per-mob overrides and
// status effects (Palsy) are later systems, not T1.
func MonsterRating(level, difficulty int) (int, error) {
	if level < 0 || difficulty < 0 {
		return 0, ErrInvalidCombatRating
	}
	r := satAdd(satMul(int64(level), 3), satMul(int64(difficulty), 60))
	return int(boundInt64(r, minCombatRating, maxMonsterRating)), nil
}

// HitChance computes (Offense*55)/Defense with integer truncation,
// bound 10..95 (spec §9.1.6). Rating inputs are normalized by their
// constructors; non-positive inputs are still rejected here with a
// stable error rather than dividing by zero. No panic, no negative
// result.
func HitChance(offense, defense int) (int, error) {
	if offense <= 0 || defense <= 0 {
		return 0, ErrInvalidCombatRating
	}
	ch := satMul(int64(offense), equalChanceHit) / int64(defense)
	return int(boundInt64(ch, minHitChance, maxHitChance)), nil
}

// rollBounded draws an inclusive [min, max] integer from rng using
// modulo sampling. It is deterministic under an injected scripted RNG,
// never touches global math/rand, crypto/rand, or the wall clock, and
// never panics on a valid range. The modulo bias (at most
// (span-1)/2^64 per draw) is documented and negligible; the helper
// stays total (no rejection loop that a hostile scripted RNG could
// stall).
func rollBounded(rng RNG, min, max int) (int, error) {
	if rng == nil {
		return 0, ErrNilRNG
	}
	if min > max {
		return 0, ErrInvalidRange
	}
	// Exact span as uint64 (fits: min <= max implies max-min fits).
	span := uint64(max) - uint64(min) + 1
	if span == 0 {
		// Full int64 range (min == math.MinInt64, max == math.MaxInt64):
		// every uint64 maps bijectively onto the range.
		return int(int64(rng.Uint64())), nil
	}
	return min + int(rng.Uint64()%span), nil
}

// RollD100 draws a d100 in 1..100 inclusive from the injected RNG
// (spec §9.1.6).
func RollD100(rng RNG) (int, error) {
	return rollBounded(rng, 1, d100Sides)
}

// HitLands reports whether a swing hits: hit iff chance >= roll
// (spec §9.1.6). The roll MUST be a 1..100 d100 value (outside that is
// a domain error); chance needs no validation — the comparison is
// total, so boundary tests read exactly.
func HitLands(chance, roll int) (bool, error) {
	if roll < 1 || roll > d100Sides {
		return false, ErrInvalidHitRoll
	}
	return chance >= roll, nil
}

// HitResolution is the deterministic trace of one hit attempt.
type HitResolution struct {
	Chance int
	Roll   int
	Landed bool
}

// RollHit composes HitChance + RollD100 + HitLands into one
// deterministic trace: identical scripted RNG + identical ratings
// yield identical results.
func RollHit(rng RNG, offense, defense int) (HitResolution, error) {
	var res HitResolution
	ch, err := HitChance(offense, defense)
	if err != nil {
		return res, err
	}
	roll, err := RollD100(rng)
	if err != nil {
		return res, err
	}
	landed, err := HitLands(ch, roll)
	if err != nil {
		return res, err
	}
	return HitResolution{Chance: ch, Roll: roll, Landed: landed}, nil
}

// VictimSnapshot is the immutable victim input for the final player-hit
// cap primitive (spec §9.1.12). It is calculation data only — NOT live
// vitals storage (M5-T4 owns that). HP is current health, BaseMaxHP the
// unbuffed maximum; both sides of the one-third rule use BaseMaxHP.
type VictimSnapshot struct {
	HP        int
	BaseMaxHP int
	Outlaw    bool
	Murderer  bool
}

// hpBelowDoubleBase reports HP < 2*BaseMaxHP with overflow-safe order
// operations (no raw 2*BaseMaxHP product).
func hpBelowDoubleBase(hp, base int64) bool {
	if base > int64(math.MaxInt64)/2 {
		// 2*base exceeds any int64 hp: strictly below holds.
		return true
	}
	return hp < 2*base
}

// ceilDiv3 returns ceil(base/3) for base >= 1 without overflow.
func ceilDiv3(base int64) int64 {
	return base/3 + boolToInt64(base%3 != 0)
}

func boolToInt64(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// ApplyPlayerDamageCaps applies the final player-hit cap stage
// (spec §9.1.12) to post-mitigation damage: minimum 1, then the
// one-third cap, then the 30 cap. Order is binding. Monsters MUST NOT
// use this function (they floor at 1 only); the absolute
// (Illusionary-Wounds-like) path skips it entirely (M5-T3).
//
// murdererProtection is the already-resolved
// DamageCapProtectionMurderersEnabled setting (default FALSE): with
// FALSE, outlaws/murderers are exempt from the one-third cap only —
// never from the 30 cap.
func ApplyPlayerDamageCaps(damage int, victim VictimSnapshot, murdererProtection bool) (int, error) {
	if damage < 0 {
		return 0, ErrInvalidDamageValue
	}
	if victim.BaseMaxHP < 1 || victim.HP < 0 {
		return 0, ErrInvalidVictimSnapshot
	}
	if damage <= 0 {
		damage = 1
	}
	hp := int64(victim.HP)
	base := int64(victim.BaseMaxHP)
	if hpBelowDoubleBase(hp, base) &&
		((!victim.Outlaw && !victim.Murderer) || murdererProtection) {
		if limit := ceilDiv3(base); int64(damage) > limit {
			damage = int(limit)
		}
	}
	if damage > maxDamagePerHit {
		damage = maxDamagePerHit
	}
	return damage, nil
}

// Severity is the damage severity classification hook (spec §9.1.13):
// domain enum only, never authoritative English prose. Future
// presentation maps these to localized text/effects.
type Severity int

const (
	// SeverityNick is 1..5 damage.
	SeverityNick Severity = iota + 1
	// SeverityWound is 6..15 damage.
	SeverityWound
	// SeverityDamage is >15 damage, or a forced one-third-MaxHP hit.
	SeverityDamage
	// SeveritySlay is a lethal hit.
	SeveritySlay
)

// String returns the stable classification name (debug/test use; NOT
// authoritative combat prose).
func (s Severity) String() string {
	switch s {
	case SeverityNick:
		return "nick"
	case SeverityWound:
		return "wound"
	case SeverityDamage:
		return "damage"
	case SeveritySlay:
		return "slay"
	default:
		return "unknown"
	}
}

// ClassifySeverity classifies APPLIED post-cap damage (spec §9.1.13).
// killed selects Slay and overrides everything (source lethal $ path).
// Otherwise, for player victims only, appliedDamage >=
// floor(victimMaxHP/3) — using the BUFFED MaxHP with >= — forces Damage.
// Remaining non-lethal damage uses >15 Damage, >5 Wound, >0 Nick.
func ClassifySeverity(appliedDamage int, killed, victimIsPlayer bool, victimMaxHP int) (Severity, error) {
	if killed {
		return SeveritySlay, nil
	}
	if appliedDamage < 1 {
		return 0, ErrInvalidDamageValue
	}
	if victimIsPlayer {
		if victimMaxHP < 1 {
			return 0, ErrInvalidDamageValue
		}
		if int64(appliedDamage) >= int64(victimMaxHP)/3 {
			return SeverityDamage, nil
		}
	}
	switch {
	case appliedDamage > severityDmgAbove:
		return SeverityDamage, nil
	case appliedDamage > severityWoundAbove:
		return SeverityWound, nil
	default:
		return SeverityNick, nil
	}
}

// SwingReady is the one-swing-per-second timing primitive in simulation
// time (spec §9.1.14): cooldownTicks = tickHz. Pure function over the
// u32 tick serial domain; caller-owned storage holds (hasSwung,
// lastSwingTick) and records lastSwingTick = nowTick when this check
// passes (attempt-time arming, mirroring source IsOkayAttackTime —
// later failure stages do not disarm it). Rejected too-early attempts
// MUST NOT update stored state. First swing (hasSwung == false) is
// always allowed. Allowed iff unsigned mod-2^32 elapsed
// (nowTick-lastSwingTick) >= cooldownTicks, so behavior is correct
// across MaxUint32 -> 0 wrap with no half-range special case.
func SwingReady(hasSwung bool, lastSwingTick, nowTick uint32, tickHz int) (bool, error) {
	if tickHz < minTickHz || tickHz > maxTickHz {
		return false, ErrInvalidTickHz
	}
	if !hasSwung {
		return true, nil
	}
	return nowTick-lastSwingTick >= uint32(tickHz), nil
}

// Vigor/exertion cost contract (spec §9.1.15): integer exertion domain
// shared with the future M5-T4 mutable model. 10000 exertion = 1 vigor.
const (
	// ExertionPerVigor is the exertion-per-vigor unit.
	ExertionPerVigor = 10000
	// StandardSwingExertionCost is the frozen standard weapon-swing
	// charge: 2000 exertion = 0.2 vigor (source slash/fire
	// viSkillExertion = 2 charged as 1000*viSkillExertion).
	StandardSwingExertionCost = 2000
	// StandardSwingVigorRequired is the vigor-unit gate threshold for a
	// gated standard swing (source HasVigor amount = viSkillExertion).
	StandardSwingVigorRequired = 2
)

// ResolveSwingExertion resolves one weapon-swing vigor transaction
// without owning any mutable vigor state (M5-T4 owns that). The gate
// mirrors source HasVigor exactly: strict vigor > required (equality
// DENIES). Standard slash/fire strokes set vbCheck_exertion = FALSE and
// therefore skip the gate entirely — they always proceed and always pay
// the full StandardSwingExertionCost on execution; this helper exists for
// gated strokes (threshold in vigor units, cost in exertion). A denied
// gate charges 0 at this stage (the half-cost SkillFailed path belongs
// to non-stroke skills and is out of T1 scope); the denial still consumes
// the swing cooldown via §9.1.14 attempt-time arming. Negative inputs are
// domain errors.
func ResolveSwingExertion(vigor, vigorRequired, costExertion int) (proceed bool, charge int, err error) {
	if vigor < 0 || vigorRequired < 0 || costExertion < 0 {
		return false, 0, ErrInvalidCombatStat
	}
	if vigor > vigorRequired {
		return true, costExertion, nil
	}
	return false, 0, nil
}
