package sim

import "math"

// CastOrigin freezes the source player/item/monster cast differences
// (spec §9.3a.16): only normal player casts run spell-power scaling
// and Mana Focus and pay full costs; item casts skip scaling/focus
// and resource payment (karma still gates unless ItemSkipsKarma);
// monster casts scale nothing and pay nothing.
type CastOrigin int

const (
	// OriginPlayer is a normal player cast (full scaling, focus, costs).
	OriginPlayer CastOrigin = iota + 1
	// OriginItem is a player item/scroll/wand cast (source
	// bItemCast=TRUE: raw roll, no focus, no mana/vigor/reagent
	// payment; karma per policy).
	OriginItem
	// OriginMonster is a non-player cast (raw roll, no costs at all;
	// source CanPayCosts/PayCosts no-op for non-players).
	OriginMonster
)

// String returns the stable origin name (debug/test use).
func (o CastOrigin) String() string {
	switch o {
	case OriginPlayer:
		return "player"
	case OriginItem:
		return "item"
	case OriginMonster:
		return "monster"
	default:
		return "unknown"
	}
}

// checkCastOrigin rejects unknown origins.
func checkCastOrigin(o CastOrigin) error {
	switch o {
	case OriginPlayer, OriginItem, OriginMonster:
		return nil
	default:
		return ErrInvalidCastOrigin
	}
}

// DamagePolicy is the narrow absolute/resistance policy value (spec
// §9.3a.17, source absolute flag): ordinary spells take the normal
// T2 resistance pipeline; absolute spells bypass numeric resistance
// and bypass ordinary player damage caps where source requires. T3a
// implements NO absolute damage formula itself (Illusionary Wounds
// is T3b); this representation exists so T3b/T7 compose without
// ad-hoc booleans.
type DamagePolicy int

const (
	// PolicyOrdinary is a normal resisted spell.
	PolicyOrdinary DamagePolicy = iota + 1
	// PolicyAbsolute bypasses numeric resistance and ordinary player
	// caps per the source absolute path.
	PolicyAbsolute
)

// String returns the stable policy name (debug/test use).
func (p DamagePolicy) String() string {
	switch p {
	case PolicyOrdinary:
		return "ordinary"
	case PolicyAbsolute:
		return "absolute"
	default:
		return "unknown"
	}
}

// checkDamagePolicy rejects unknown policies.
func checkDamagePolicy(p DamagePolicy) error {
	switch p {
	case PolicyOrdinary, PolicyAbsolute:
		return nil
	default:
		return ErrInvalidDamagePolicy
	}
}

// ScaleAttackSpellDamage applies the generic AttackSpell spell-power
// scaling (spec §9.3a.15, source atakspel.kod CastSpell):
//
//	damage = (rawRoll * (50 + spellPower/2)) / SPELLPOWER_MAXIMUM
//
// Truncation order is binding: spellPower/2 truncates FIRST, then
// multiply (64-bit intermediate), then /99 truncates. The divisor is
// SPELLPOWER_MAXIMUM = 99, never 100. Scaling applies ONLY to normal
// player casts (OriginPlayer); item and monster casts return the raw
// roll unchanged (their spellPower input is unused and unvalidated).
// rawRoll MUST be >= 0.
func ScaleAttackSpellDamage(rawRoll, spellPower int, origin CastOrigin) (int, error) {
	if err := checkCastOrigin(origin); err != nil {
		return 0, err
	}
	if rawRoll < 0 {
		return 0, ErrInvalidDamageValue
	}
	if origin != OriginPlayer {
		return rawRoll, nil
	}
	if err := checkSpellPower(spellPower); err != nil {
		return 0, err
	}
	factor := int64(50 + spellPower/2)
	return int(satMul(int64(rawRoll), factor) / maxSpellPower), nil
}

// ManaFocusInput carries the already-resolved Mana Focus scalars
// (spec §9.3a.15): no enchantment implementation lives in T3a.
type ManaFocusInput struct {
	// Active marks the PFLAG_MANA_FOCUS flag set on the caster.
	Active bool
	// Power is the resolved Mana Focus enchantment spellpower.
	Power int
	// Bonus is the resolved spell piManaFocusBonus scalar.
	Bonus int
}

// ApplyManaFocus applies the generic Mana Focus damage bonus (spec
// §9.3a.15, source atakspel.kod):
//
//	damage += ((focusPower * focusBonus) / SPELLPOWER_MAXIMUM) + 1
//
// Multiply-then-divide (single truncation, 64-bit intermediate),
// then the unconditional +1 (source applies it even when the bonus
// scalar is 0). The bonus applies ONLY when active AND the origin is
// a normal player cast; otherwise damage returns unchanged (focus
// inputs then go unvalidated: they are meaningless off-path
// resolved numerics). damage MUST be >= 0.
func ApplyManaFocus(damage int, focus ManaFocusInput, origin CastOrigin) (int, error) {
	if err := checkCastOrigin(origin); err != nil {
		return 0, err
	}
	if damage < 0 {
		return 0, ErrInvalidDamageValue
	}
	if !focus.Active || origin != OriginPlayer {
		return damage, nil
	}
	if focus.Power < 0 || focus.Bonus < 0 {
		return 0, ErrInvalidCombatStat
	}
	bonus := satMul(int64(focus.Power), int64(focus.Bonus)) / maxSpellPower
	sum := satAdd(bonus, int64(damage))
	if sum == math.MaxInt64 {
		// Saturated: the source +1 cannot push past the cap.
		return int(sum), nil
	}
	return int(sum + 1), nil
}

// AttackSpellInput carries the already-resolved inputs for one
// generic AttackSpell damage event (spec §9.3a.15–§9.3a.17).
type AttackSpellInput struct {
	// Min/Max is the inclusive generic damage range (both endpoints
	// reachable; no named spell catalog in T3a).
	Min int
	Max int
	// Power is the resolved spell power 1..99 (used for player
	// scaling only).
	Power int
	// Origin selects scaling/focus behavior (§9.3a.16).
	Origin CastOrigin
	// Focus carries the resolved Mana Focus scalars.
	Focus ManaFocusInput
	// Policy is the ordinary/absolute policy value (§9.3a.17).
	Policy DamagePolicy
	// Signature is the pass-through attack signature consumed by the
	// future T2 resistance stage (T3a never interprets its bits).
	Signature DamageSignature
}

// AttackSpellResult is the PRE-APPLICATION spell damage and metadata
// (spec §9.3a.17). It does NOT call ApplyDefenseModifiers,
// ApplyResistance, or ApplyPlayerDamageCaps, and never mutates HP:
// runtime composition is later (§9.3a.18, §9.3c).
type AttackSpellResult struct {
	// Damage is the final pre-application damage (rolled, scaled,
	// focus-adjusted).
	Damage int
	// RawRoll is the inclusive [Min, Max] roll before scaling.
	RawRoll int
	// Origin echoes the cast origin.
	Origin CastOrigin
	// Policy echoes the damage policy.
	Policy DamagePolicy
	// Signature echoes the attack signature for the T2 stage.
	Signature DamageSignature
}

// RollAttackSpellDamage resolves one generic AttackSpell damage event
// (spec §9.3a.15–§9.3a.17): inclusive roll [Min, Max] from the
// injected RNG, then origin-selected spell-power scaling, then
// origin-selected Mana Focus. No second RNG implementation; no HP
// mutation; no T1 caps (an absolute-policy result MUST NOT be
// routed through ApplyPlayerDamageCaps by the future runtime either).
func RollAttackSpellDamage(rng RNG, in AttackSpellInput) (AttackSpellResult, error) {
	var res AttackSpellResult
	if err := checkCastOrigin(in.Origin); err != nil {
		return res, err
	}
	if err := checkDamagePolicy(in.Policy); err != nil {
		return res, err
	}
	if in.Min < 0 || in.Max < 0 {
		return res, ErrInvalidDamageValue
	}
	raw, err := rollBounded(rng, in.Min, in.Max)
	if err != nil {
		return res, err
	}
	scaled, err := ScaleAttackSpellDamage(raw, in.Power, in.Origin)
	if err != nil {
		return res, err
	}
	final, err := ApplyManaFocus(scaled, in.Focus, in.Origin)
	if err != nil {
		return res, err
	}
	return AttackSpellResult{
		Damage:    final,
		RawRoll:   raw,
		Origin:    in.Origin,
		Policy:    in.Policy,
		Signature: in.Signature,
	}, nil
}
