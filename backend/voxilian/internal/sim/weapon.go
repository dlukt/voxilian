package sim

import (
	"errors"
	"math"
)

// Weapon-domain errors (spec §9.1.16).
var (
	// ErrUnknownWeaponFamily marks an unrecognized WeaponFamily.
	ErrUnknownWeaponFamily = errors.New("sim: unknown weapon family")
	// ErrUnknownWeaponQuality marks an unrecognized WeaponQuality.
	ErrUnknownWeaponQuality = errors.New("sim: unknown weapon quality")
)

// WeaponFamily is the generic M59 weapon family (spec §9.1.7).
// Numerics mirror the source WEAPON_TYPE_* discriminants for audit
// traceability; they are NOT stable content/catalog IDs (M9-T13 owns
// those) and MUST NOT be persisted as proto IDs.
type WeaponFamily int

const (
	// WeaponBludgeon is the blunt family (source WEAPON_TYPE_BLUDGEON).
	WeaponBludgeon WeaponFamily = 0
	// WeaponThrust is the sword family (source WEAPON_TYPE_THRUST).
	WeaponThrust WeaponFamily = 1
	// WeaponSlash is the heavy family (source WEAPON_TYPE_SLASH).
	WeaponSlash WeaponFamily = 2
)

// WeaponQuality is the generic M59 weapon quality (spec §9.1.8).
// Numerics mirror the source WEAPON_QUALITY_*/WEAPON_NERUDITE
// discriminants for audit traceability; NOT content IDs.
type WeaponQuality int

const (
	// WeaponQualityLow is the low quality tier.
	WeaponQualityLow WeaponQuality = 0
	// WeaponQualityNormal is the default tier: explicitly zero
	// modifiers (source WEAPON_QUALITY_NORMAL has no modifier branch).
	WeaponQualityNormal WeaponQuality = 1
	// WeaponQualityHigh is the high quality tier.
	WeaponQualityHigh WeaponQuality = 2
	// WeaponQualityNerudite is the nerudite tier.
	WeaponQualityNerudite WeaponQuality = 3
)

// WeaponFamilyStats is one frozen family row (spec §9.1.7): damage
// range endpoints are INCLUSIVE.
type WeaponFamilyStats struct {
	HitMod    int
	DamageMin int
	DamageMax int
	DisarmMod int
	SpellMod  int
	Range     int
}

// WeaponQualityMods is one frozen quality row (spec §9.1.8): values
// compose additively with the family row.
type WeaponQualityMods struct {
	HitMod    int
	DamageMod int
	DisarmMod int
	SpellMod  int
	RangeMod  int
}

// weaponFamilyTable is the frozen family table. Fixed switch over a
// validated enum: callers receive a value copy and can never mutate
// the definition.
func weaponFamilyTable(f WeaponFamily) (WeaponFamilyStats, error) {
	switch f {
	case WeaponBludgeon:
		return WeaponFamilyStats{HitMod: 75, DamageMin: 4, DamageMax: 8, DisarmMod: -5, SpellMod: 0, Range: 2}, nil
	case WeaponThrust:
		return WeaponFamilyStats{HitMod: 125, DamageMin: 3, DamageMax: 8, DisarmMod: 10, SpellMod: -10, Range: 3}, nil
	case WeaponSlash:
		return WeaponFamilyStats{HitMod: 0, DamageMin: 5, DamageMax: 11, DisarmMod: 0, SpellMod: -15, Range: 2}, nil
	default:
		return WeaponFamilyStats{}, ErrUnknownWeaponFamily
	}
}

// WeaponFamilyStatsOf returns the frozen stats row for a family.
func WeaponFamilyStatsOf(f WeaponFamily) (WeaponFamilyStats, error) {
	return weaponFamilyTable(f)
}

// weaponQualityTable is the frozen quality table (fixed switch,
// value-only results).
func weaponQualityTable(q WeaponQuality) (WeaponQualityMods, error) {
	switch q {
	case WeaponQualityLow:
		return WeaponQualityMods{HitMod: 0, DamageMod: -1, DisarmMod: -5, SpellMod: 5, RangeMod: 0}, nil
	case WeaponQualityNormal:
		return WeaponQualityMods{}, nil
	case WeaponQualityHigh:
		return WeaponQualityMods{HitMod: 50, DamageMod: 1, DisarmMod: 5, SpellMod: -5, RangeMod: 0}, nil
	case WeaponQualityNerudite:
		return WeaponQualityMods{HitMod: 25, DamageMod: 1, DisarmMod: 0, SpellMod: 5, RangeMod: 0}, nil
	default:
		return WeaponQualityMods{}, ErrUnknownWeaponQuality
	}
}

// WeaponQualityModsOf returns the frozen modifier row for a quality.
func WeaponQualityModsOf(q WeaponQuality) (WeaponQualityMods, error) {
	return weaponQualityTable(q)
}

// WeaponHitModifier composes the ModifyHitRoll order (spec §9.1.3,
// §9.1.7–§9.1.8): family hit + quality hit + already-resolved numeric
// hitBonus (enchant DamageBonus/HitBonus sources are future content
// systems; the API trusts the resolved number).
func WeaponHitModifier(family WeaponFamily, quality WeaponQuality, hitBonus int) (int, error) {
	fam, err := weaponFamilyTable(family)
	if err != nil {
		return 0, err
	}
	mod, err := weaponQualityTable(quality)
	if err != nil {
		return 0, err
	}
	return fam.HitMod + mod.HitMod + hitBonus, nil
}

// RollWeaponBase rolls the inclusive family damage range and adds the
// quality damage modifier (spec §9.1.8–§9.1.9): GetBaseDamage order.
// The generic enchant/item DamageBonus is NOT added here — it joins in
// RawWeaponDamage (§9.1.10 GetDamage order). Deterministic under the
// injected RNG.
func RollWeaponBase(rng RNG, family WeaponFamily, quality WeaponQuality) (int, error) {
	fam, err := weaponFamilyTable(family)
	if err != nil {
		return 0, err
	}
	mod, err := weaponQualityTable(quality)
	if err != nil {
		return 0, err
	}
	roll, err := rollBounded(rng, fam.DamageMin, fam.DamageMax)
	if err != nil {
		return 0, err
	}
	return roll + mod.DamageMod, nil
}

// Stroke damage-factor constants (spec §9.1.10): Slash 80, Fire/bow 90,
// default 100 (source viDamage_factor).
const (
	DamageFactorDefault = 100
	DamageFactorSlash   = 80
	DamageFactorFire    = 90
	// DefaultMaxProfDamage is source viMaxProficiencyDamage.
	DefaultMaxProfDamage = 5
)

// satAddSigned returns a+b saturating at the int64 extremes (operands
// may be negative — trusted resolved content numerics).
func satAddSigned(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	if b < 0 && a < math.MinInt64-b {
		return math.MinInt64
	}
	return a + b
}

// scaleDamage returns (w*factor)/100 with C-truncation toward zero,
// saturating instead of overflowing. factor MUST be non-negative; w may
// be negative (hostile/degenerate content inputs stay total and flow
// into the final max(.,1) floor).
func scaleDamage(w, factor int64) int64 {
	if w == 0 || factor == 0 {
		return 0
	}
	if w > 0 {
		if w > math.MaxInt64/factor {
			return math.MaxInt64 / 100
		}
		return (w * factor) / 100
	}
	if w < math.MinInt64/factor {
		return math.MinInt64 / 100
	}
	return (w * factor) / 100
}

// RawDamageInput carries already-resolved integer inputs for the
// pre-mitigation physical weapon damage formula (spec §9.1.10).
// BaseRoll is the rolled base (RollWeaponBase result WITHOUT the
// quality modifier double-counted — pass the roll and the quality mod
// separately); DamageBonus is the resolved numeric enchant/item bonus
// (trusted future-content input); DamageFactor selects the stroke
// (80/90/100); Proficiency is the resolved weapon proficiency ability;
// MaxProfDamage is viMaxProficiencyDamage (5 default); Attr is the
// resolved Might (melee) or Aim (Fire) effective attribute.
type RawDamageInput struct {
	BaseRoll      int
	QualityDmgMod int
	DamageBonus   int
	DamageFactor  int
	Proficiency   int
	MaxProfDamage int
	Attr          int
}

// RawWeaponDamage computes pre-mitigation physical weapon damage with
// the exact source-audited operation order (spec §9.1.10):
//
//	w = BaseRoll + QualityDmgMod + DamageBonus
//	s = (w * DamageFactor) / 100            (truncation)
//	profFlat = ((Proficiency+1) * MaxProfDamage) / 100   (truncation)
//	attrBonus = bound(Attr-25, 0, 40)
//	m = ((100+attrBonus) * s) / 100         (truncation)
//	preMit = max(profFlat + m, 1)
//
// The base is counted exactly once (inside the attribute term) plus the
// small flat proficiency bonus — never doubled. No armor, resistance,
// or post-mitigation bonuses are applied here (M5-T2 owns those;
// see §9.1.11).
func RawWeaponDamage(in RawDamageInput) (int, error) {
	if in.DamageFactor < 0 || in.MaxProfDamage < 0 {
		return 0, ErrInvalidCombatStat
	}
	if in.Proficiency < 0 || in.Attr < 0 {
		return 0, ErrInvalidCombatStat
	}
	// Trusted resolved numerics (content-owned ranges) compose
	// additively; saturation keeps the pipeline total for absurd
	// magnitudes (w may legitimately go negative from degenerate
	// content and then floors to 1 at the end).
	w := satAddSigned(satAddSigned(int64(in.BaseRoll), int64(in.QualityDmgMod)), int64(in.DamageBonus))
	s := scaleDamage(w, int64(in.DamageFactor))
	profFlat := satMul(int64(in.Proficiency)+1, int64(in.MaxProfDamage)) / 100
	attrBonus := boundInt64(int64(in.Attr)-25, 0, 40)
	m := scaleDamage(s, 100+attrBonus)
	raw := satAddSigned(profFlat, m)
	if raw < 1 {
		raw = 1
	}
	return int(raw), nil
}
