package sim

import (
	"errors"
	"math"
)

// Stable M5-T2 defense-domain errors (spec §9.2.20). Matching MUST use
// errors.Is, never string parsing as control flow.
var (
	// ErrInvalidDefenseModifier marks an impossible defense-modifier
	// value: negative DamageReduce, or a shield (RequiresBlock) entry
	// carrying nonzero DefensePower (the shield bonus belongs to the
	// Block rating/chance paths only, never to DefensePower).
	ErrInvalidDefenseModifier = errors.New("sim: invalid defense modifier")
	// ErrInvalidDefenseSkill marks a negative impossible defensive
	// skill/attribute input (ability, requisite stat). Zero is legal.
	ErrInvalidDefenseSkill = errors.New("sim: invalid defense skill")
	// ErrInvalidDamageClass marks an unknown DamageClass enum value.
	ErrInvalidDamageClass = errors.New("sim: invalid damage class")
)

// Defense rating clamp for the shield block value
// (spec §9.2.5, source shield.kod GetBlockAbility).
const (
	minBlockRating = 1
	maxBlockRating = 120
)

// DefenseCapability carries already-resolved eligibility booleans for
// the three defensive components (spec §9.2.4). T2 performs no live
// equipment lookup, no room/flag reads, and no vigor storage access:
// the future runtime resolves these before calling.
type DefenseCapability struct {
	HasWeapon      bool // weapon equipped (parry eligibility)
	ParryCostOK    bool // parry CanPayCosts resolved (NO_FIGHT + future gates)
	AttackCanParry bool // incoming stroke/monster CanParry
	HasShield      bool // effective shield equipped (block eligibility)
	BlockCostOK    bool // block CanPayCosts resolved (future gates; source has no flag gate)
	AttackCanBlock bool // incoming stroke/monster CanBlock
	DodgeCostOK    bool // dodge CanPayCosts resolved (NO_MOVE + future gates)
	AttackCanDodge bool // incoming stroke/monster CanDodge
}

// checkDefenseSkill rejects negative impossible skill inputs.
func checkDefenseSkill(v int) error {
	if v < 0 {
		return ErrInvalidDefenseSkill
	}
	return nil
}

// ResolveParryComponent returns the Parry value consumed by T1
// PlayerDefense (spec §9.2.5): 0 unless armed, cost-capable, and the
// incoming attack is parryable; otherwise the resolved ability.
func ResolveParryComponent(parryAbility int, cap DefenseCapability) (int, error) {
	if err := checkDefenseSkill(parryAbility); err != nil {
		return 0, err
	}
	if !cap.HasWeapon || !cap.ParryCostOK || !cap.AttackCanParry {
		return 0, nil
	}
	return parryAbility, nil
}

// ResolveDodgeComponent returns the Dodge value consumed by T1
// PlayerDefense (spec §9.2.5): 0 unless cost-capable and the incoming
// attack is dodgeable (no equipment requirement in source); otherwise
// the resolved ability.
func ResolveDodgeComponent(dodgeAbility int, cap DefenseCapability) (int, error) {
	if err := checkDefenseSkill(dodgeAbility); err != nil {
		return 0, err
	}
	if !cap.DodgeCostOK || !cap.AttackCanDodge {
		return 0, nil
	}
	return dodgeAbility, nil
}

// ResolveBlockComponent returns the Block value consumed by T1
// PlayerDefense (spec §9.2.5): 0 when disabled (no effective shield
// or a failed gate); otherwise bound(blockSkill + shieldBonus, 1,
// 120). The bonus is the shield piDefense_bonus (source never reads
// piBlockBonus). Negative blockSkill is a domain error; a negative
// shieldBonus is a trusted resolved numeric that flows through the
// clamp normally.
func ResolveBlockComponent(blockSkill, shieldBonus int, cap DefenseCapability) (int, error) {
	if err := checkDefenseSkill(blockSkill); err != nil {
		return 0, err
	}
	if !cap.HasShield || !cap.BlockCostOK || !cap.AttackCanBlock {
		return 0, nil
	}
	sum := satAddSigned(int64(blockSkill), int64(shieldBonus))
	return int(boundInt64(sum, minBlockRating, maxBlockRating)), nil
}

// DefenseSkillChance computes the shared generic skill success chance
// (spec §9.2.6, source skill.kod SuccessChance):
//
//	chance = ((100-requisiteStat)*ability)/100 + requisiteStat + modifier
//
// with ONE truncation (product first, 64-bit saturating intermediate).
// The result is intentionally UNCLAMPED (source has no clamp):
// chance > 100 always succeeds, chance < 1 always fails. requisiteStat
// is the effective Agility for all three defensive skills. modifier is
// a trusted resolved numeric (shield piDefense_bonus for Block, 0 for
// Parry/Dodge). Negative ability/requisiteStat are domain errors.
func DefenseSkillChance(ability, requisiteStat, modifier int) (int, error) {
	if err := checkDefenseSkill(ability); err != nil {
		return 0, err
	}
	if err := checkDefenseSkill(requisiteStat); err != nil {
		return 0, err
	}
	diff := int64(100) - int64(requisiteStat)
	prod := satMulSigned(diff, int64(ability))
	ch := satAddSigned(satAddSigned(prod/d100Sides, int64(requisiteStat)), int64(modifier))
	return int(ch), nil
}

// satMulSigned returns a*b saturating at the int64 extremes. Unlike
// satMul (non-negative T1 operands), the skill-chance difference term
// may be negative when requisiteStat > 100.
func satMulSigned(a, b int64) int64 {
	if a == 0 || b == 0 {
		return 0
	}
	if a > 0 && b > 0 {
		if a > math.MaxInt64/b {
			return math.MaxInt64
		}
		return a * b
	}
	if a < 0 && b < 0 {
		// |a|*|b| overflows iff a < MaxInt64/b (b<0 flips sense);
		// mind math.MinInt64 (its negation overflows).
		if a == math.MinInt64 || b == math.MinInt64 {
			return math.MaxInt64
		}
		if -a > math.MaxInt64/(-b) {
			return math.MaxInt64
		}
		return a * b
	}
	// Mixed signs: result negative, saturate at MinInt64.
	if a > 0 { // b < 0
		if b < math.MinInt64/a {
			return math.MinInt64
		}
		return a * b
	}
	// a < 0, b > 0
	if a < math.MinInt64/b {
		return math.MinInt64
	}
	return a * b
}

// DefenseSkillRoll is the deterministic trace of one defensive-skill
// success check (spec §9.2.6).
type DefenseSkillRoll struct {
	Chance  int
	Roll    int
	Success bool
}

// RollDefenseSkill resolves DefenseSkillChance then draws one d100:
// success iff roll <= chance (spec §9.2.6, source
// "if random(1,100) > num return FALSE"). All three defensive
// contexts (Parry, Dodge, Block) call this ONE function — frozen
// explicitly because source defines no per-skill override.
func RollDefenseSkill(rng RNG, ability, requisiteStat, modifier int) (DefenseSkillRoll, error) {
	var res DefenseSkillRoll
	ch, err := DefenseSkillChance(ability, requisiteStat, modifier)
	if err != nil {
		return res, err
	}
	roll, err := RollD100(rng)
	if err != nil {
		return res, err
	}
	return DefenseSkillRoll{Chance: ch, Roll: roll, Success: roll <= ch}, nil
}

// BlockOutcome is the deterministic trace of one shield block check
// (spec §9.2.7, source shield.kod ModifyDefenseDamage): the reduction
// applies only when the defender HAS block (Attempted) and wins the
// success roll with the shield bonus as modifier (Succeeded).
// Attempted/Succeeded are data for the future M6 advancement hook;
// T2 never calls ImproveAbility.
type BlockOutcome struct {
	Chance    int
	Roll      int
	Attempted bool
	Succeeded bool
}

// RollBlock resolves the Block success roll (spec §9.2.7). When the
// defender has no block ability, no RNG is consumed and the outcome
// reports Attempted=false. requisiteStat is the effective Agility;
// shieldBonus is the shield piDefense_bonus (same value as the rating
// path, §9.2.5 — separate placements, never double-counted).
func RollBlock(rng RNG, blockAbility, requisiteStat, shieldBonus int) (BlockOutcome, error) {
	var res BlockOutcome
	if err := checkDefenseSkill(blockAbility); err != nil {
		return res, err
	}
	if err := checkDefenseSkill(requisiteStat); err != nil {
		return res, err
	}
	if blockAbility == 0 {
		return res, nil
	}
	roll, err := RollDefenseSkill(rng, blockAbility, requisiteStat, shieldBonus)
	if err != nil {
		return res, err
	}
	return BlockOutcome{
		Chance:    roll.Chance,
		Roll:      roll.Roll,
		Attempted: true,
		Succeeded: roll.Success,
	}, nil
}

// DefenseModifier is the generic immutable worn-defense value object
// (spec §9.2.9). No named armor/shield classes exist in T2; M9-T13
// owns content prototypes and maps them onto these numbers.
type DefenseModifier struct {
	// DefensePower is the signed ModifyDefensePower bonus. Shield
	// entries MUST carry 0 (source shield.kod returns defense_power
	// unchanged); nonzero shield power is ErrInvalidDefenseModifier.
	DefensePower int
	// DamageReduce is piDamage_reduce r (>= 0). 0 skips the reduction
	// stage for that entry.
	DamageReduce int
	// RequiresBlock gates the entry's reduction on a successful Block
	// roll (standard shields). Armor carries false. The soldier-shield
	// exception (source soldshld.kod: reduction without the Block
	// check) is integration wiring modeled as RequiresBlock=false.
	RequiresBlock bool
}

// checkDefenseModifier validates one entry's impossible-domain edges.
func checkDefenseModifier(m DefenseModifier) error {
	if m.DamageReduce < 0 {
		return ErrInvalidDefenseModifier
	}
	if m.RequiresBlock && m.DefensePower != 0 {
		return ErrInvalidDefenseModifier
	}
	return nil
}

// ResolveDefensePowerModifier returns the plain signed sum of worn
// DefensePower bonuses (spec §9.2.8: source adds with no per-item
// clamp; T1 owns the final 1..1000 bound). Suitable as
// PlayerDefenseInput.ExtraMods. T2 never calls PlayerDefense here.
func ResolveDefensePowerModifier(mods []DefenseModifier) (int, error) {
	var total int64
	for _, m := range mods {
		if err := checkDefenseModifier(m); err != nil {
			return 0, err
		}
		total = satAddSigned(total, int64(m.DefensePower))
	}
	return int(total), nil
}

// DamageClass is the typed armor-reduction damage classification
// (spec §9.2.11). The zero value is DamageClassWeapon, matching the
// source branch (aspell == 0 takes the full-reduction path,
// including the degenerate zero-vector case).
type DamageClass uint8

const (
	// DamageClassWeapon is pure weapon damage (spellBits == 0):
	// full reduction.
	DamageClassWeapon DamageClass = iota
	// DamageClassSpell is pure spell damage (weaponBits == 0,
	// spellBits != 0): armor reduction is exactly 0.
	DamageClassSpell
	// DamageClassWeaponSpell is mixed damage (both nonzero): the
	// rolled reduction is scaled 2/3 after the draw and cap.
	DamageClassWeaponSpell
)

// ClassifyDamageClass derives the DamageClass from raw attack
// bitvectors exactly as the source branch does (spec §9.2.11):
// spellBits == 0 -> weapon; both nonzero -> mixed; otherwise spell.
func ClassifyDamageClass(weaponBits, spellBits uint32) DamageClass {
	if spellBits == 0 {
		return DamageClassWeapon
	}
	if weaponBits != 0 {
		return DamageClassWeaponSpell
	}
	return DamageClassSpell
}

// checkDamageClass rejects unknown enum values.
func checkDamageClass(c DamageClass) error {
	switch c {
	case DamageClassWeapon, DamageClassSpell, DamageClassWeaponSpell:
		return nil
	default:
		return ErrInvalidDamageClass
	}
}

// RollDamageReduction computes ONE modifier's reduction amount (spec
// §9.2.10, source defmod.kod ModifyDefenseDamage): randomInclusive(r/3,
// r), capped at damage-1, then scaled by damage class (pure spell ->
// 0, mixed -> (reduce*2)/3 AFTER cap). r/3 floors before the draw;
// the 2/3 scaling is a single truncation on 64-bit width.
//
//   - damage < 0 -> ErrInvalidDamageValue; damage == 0 returns 0 with
//     no RNG consumed (documented freeze: the literal source trace is
//     a degenerate bound artifact on unreachable input, and T2 owns
//     no minimum, §9.2.2).
//   - r < 0 -> ErrInvalidDefenseModifier; r == 0 returns 0 without
//     consuming RNG.
func RollDamageReduction(rng RNG, reduce, damage int, class DamageClass) (int, error) {
	if err := checkDamageClass(class); err != nil {
		return 0, err
	}
	if damage < 0 {
		return 0, ErrInvalidDamageValue
	}
	if damage == 0 {
		return 0, nil
	}
	if reduce < 0 {
		return 0, ErrInvalidDefenseModifier
	}
	if reduce == 0 {
		return 0, nil
	}
	roll, err := rollBounded(rng, reduce/3, reduce)
	if err != nil {
		return 0, err
	}
	if cap := damage - 1; roll > cap {
		roll = cap
	}
	switch class {
	case DamageClassWeapon:
		return roll, nil
	case DamageClassSpell:
		return 0, nil
	default: // DamageClassWeaponSpell (checked above)
		return int((int64(roll) * 2) / 3), nil
	}
}

// DefenseMitigationResult is the deterministic trace of sequential
// armor/shield application (spec §9.2.12). ModifiersApplied counts
// entries that produced nonzero reduction (durability hook for
// M7/M9); T2 mutates nothing.
type DefenseMitigationResult struct {
	FinalDamage      int
	TotalReduced     int
	ModifiersApplied int
}

// ApplyDefenseModifiers applies every worn modifier sequentially in
// slice order (spec §9.2.12: each sees already-reduced damage with
// its own RNG draw; each caps at its current damage-1). Shield
// entries (RequiresBlock) are skipped unless blockSucceeded. For
// fixed rolls the result equals max(damage - total, 1) for damage >=
// 1 regardless of order (frozen order-independence); RNG consumption
// still follows slice order deterministically. The result is never
// floored to 1 here (T1 caps own the minimum).
func ApplyDefenseModifiers(rng RNG, damage int, class DamageClass, mods []DefenseModifier, blockSucceeded bool) (DefenseMitigationResult, error) {
	var res DefenseMitigationResult
	if err := checkDamageClass(class); err != nil {
		return res, err
	}
	if damage < 0 {
		return res, ErrInvalidDamageValue
	}
	current := damage
	for _, m := range mods {
		if m.DamageReduce < 0 {
			return res, ErrInvalidDefenseModifier
		}
		if m.RequiresBlock && !blockSucceeded {
			continue
		}
		red, err := RollDamageReduction(rng, m.DamageReduce, current, class)
		if err != nil {
			return res, err
		}
		if red > 0 {
			res.ModifiersApplied++
			res.TotalReduced += red
			current -= red
		}
	}
	res.FinalDamage = current
	return res, nil
}
