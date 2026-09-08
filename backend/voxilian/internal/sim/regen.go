package sim

import (
	"fmt"
)

// Pure vitals interval/step/node math (spec §9.4.9–§9.4.13, §9.4.16–§9.4.17,
// §9.4.22). No timers are created here; T4b owns all scheduling.

// Timer source constants (player.kod).
const (
	baseRegenTimeMs  = 150000 // source BASE_REGEN_TIME
	boostDecayTimeMs = 30000  // source BOOST_DECAY_TIME (mana-only)
)

// NodeKind is the small mechanics-only mana-node variant enum
// (spec §9.4.12). No node IDs, rooms, karma, or meld state: M9/content
// owns catalog identity.
type NodeKind int

const (
	// NodeStandard is an ordinary mana node.
	NodeStandard NodeKind = iota + 1
	// NodeDouble is the source special double variant (Fey node).
	NodeDouble
)

// StandardNodeMana freezes mananode.kod GetManaAdjust (spec §9.4.12):
// ((5 + mysticism) / 10) + 3 over the already-resolved meld-time
// effective mysticism (1..70).
func StandardNodeMana(mysticism int) (int, error) {
	if err := checkEffectiveAttr("mysticism", mysticism); err != nil {
		return 0, err
	}
	return (5+mysticism)/10 + 3, nil
}

// NodeManaBonus freezes the node-mana arithmetic including the Fey
// double (spec §9.4.12): Double = 2 * Standard. Audit conclusion: no
// second independent multiplier exists (AvarNode inherits standard).
func NodeManaBonus(kind NodeKind, mysticism int) (int, error) {
	base, err := StandardNodeMana(mysticism)
	if err != nil {
		return 0, err
	}
	switch kind {
	case NodeStandard:
		return base, nil
	case NodeDouble:
		doubled, ok := checkedMul(int64(base), 2)
		if !ok {
			return 0, fmt.Errorf("sim: vitals node bonus: %w", ErrInvalidManaAmount)
		}
		out, ok := toInt(doubled)
		if !ok {
			return 0, fmt.Errorf("sim: vitals node bonus: %w", ErrInvalidManaAmount)
		}
		return out, nil
	default:
		return 0, fmt.Errorf("sim: vitals node kind %d: %w", int(kind), ErrInvalidNodeKind)
	}
}

// ComputeMaxMana freezes the source ComputeMaxMana value semantics
// (spec §9.4.13) over already-resolved inputs: initial mana plus melded
// node contributions plus other (item/enchantment scalar) bonus. Source
// states NO bound on the result, so T4a imposes none; the sum is
// overflow-safe and hostile overflow is a domain error. No
// inventory/enchantment/node-list lookups.
func ComputeMaxMana(initialMana int, nodeBonuses []int, otherBonus int) (int, error) {
	total := int64(initialMana)
	for _, b := range nodeBonuses {
		var ok bool
		total, ok = checkedAdd(total, int64(b))
		if !ok {
			return 0, fmt.Errorf("sim: vitals compute max mana: %w", ErrInvalidManaAmount)
		}
	}
	total, ok := checkedAdd(total, int64(otherBonus))
	if !ok {
		return 0, fmt.Errorf("sim: vitals compute max mana: %w", ErrInvalidManaAmount)
	}
	out, ok := toInt(total)
	if !ok {
		return 0, fmt.Errorf("sim: vitals compute max mana: %w", ErrInvalidManaAmount)
	}
	return out, nil
}

// TimerStep is the pure one-event timer effect value (spec §9.4.9,
// §9.4.16). Decay steps are NOT combat damage/resource spending.
type TimerStep int

const (
	// TimerNoChange means the value already equals its max: no event.
	TimerNoChange TimerStep = iota
	// TimerGainOne means below max: ordinary +1.
	TimerGainOne
	// TimerDecayOne means above max: decay -1 (decay, not damage).
	TimerDecayOne
)

// String keeps the enum debuggable (same convention as CastOrigin).
func (s TimerStep) String() string {
	switch s {
	case TimerNoChange:
		return "none"
	case TimerGainOne:
		return "gain"
	case TimerDecayOne:
		return "decay"
	default:
		return "unknown"
	}
}

// HealthTimerStep freezes the source HealthTimer one-event decision
// (spec §9.4.9): HP<Max -> +1, HP==Max -> none (source deletes the timer
// via NewHealth), HP>Max -> decay -1. Moved-since-entry gating is T4b.
func HealthTimerStep(hp, maxHP int) (TimerStep, error) {
	if hp < 0 || maxHP < minMaxHP {
		return TimerNoChange, fmt.Errorf("sim: vitals health step %d/%d: %w", hp, maxHP, ErrInvalidVitals)
	}
	switch {
	case hp < maxHP:
		return TimerGainOne, nil
	case hp == maxHP:
		return TimerNoChange, nil
	default:
		return TimerDecayOne, nil
	}
}

// ManaTimerStep freezes the source ManaTimer one-event behavior
// (spec §9.4.16): Mana<Max -> +1, == -> none (timer deleted via NewMana),
// > -> -1 (not spending). No scheduling.
func ManaTimerStep(mana, maxMana int) (TimerStep, error) {
	if mana < 0 || maxMana < minMaxMana {
		return TimerNoChange, fmt.Errorf("sim: vitals mana step %d/%d: %w", mana, maxMana, ErrInvalidVitals)
	}
	switch {
	case mana < maxMana:
		return TimerGainOne, nil
	case mana == maxMana:
		return TimerNoChange, nil
	default:
		return TimerDecayOne, nil
	}
}

// ApplyRestorateAdjust freezes restorate.kod AdjustHealthTime
// (spec §9.4.10): bound input to 1000..60000, scale by
// (400-(40+power))/400, bound output to 670..60000. spellPower is the
// already-resolved iSpellPower (T3a 1..99); no spell lookup.
func ApplyRestorateAdjust(timeMs, spellPower int) (int, error) {
	if err := checkSpellPower(spellPower); err != nil {
		return 0, err
	}
	t := boundInt64(int64(timeMs), 1000, 60000)
	out := satMul(t, int64(400-(40+spellPower))) / 400
	return int(boundInt64(out, 670, 60000)), nil
}

// HealthRegenIntervalMs freezes source CalculateHealthTime (spec
// §9.4.10) with exact operation order and truncation. factionBonus is
// the already-resolved phase-2 scalar (0 in MVP tests); restoratePower
// is 0 for no song, else the already-resolved Restorate power (1..99).
// Audit conclusion: NO over-max branch — over-max decay reuses this
// same interval (BOOST_DECAY_TIME is mana-only).
func HealthRegenIntervalMs(vigor, effectiveStamina, maxHP, factionBonus, restoratePower int) (int, error) {
	if vigor < minVigor || vigor > maxVigor {
		return 0, fmt.Errorf("sim: vitals health interval vigor = %d: %w", vigor, ErrInvalidVitals)
	}
	if err := checkEffectiveAttr("stamina", effectiveStamina); err != nil {
		return 0, err
	}
	if maxHP < minMaxHP {
		return 0, fmt.Errorf("sim: vitals health interval max = %d: %w", maxHP, ErrInvalidVitals)
	}
	if restoratePower != 0 {
		if err := checkSpellPower(restoratePower); err != nil {
			return 0, err
		}
	}
	d := int64(200 - vigor)
	t := d*d/6 + 1000
	stm, ok := checkedMul(int64(125-effectiveStamina), t)
	if !ok {
		return 0, fmt.Errorf("sim: vitals health interval: %w", ErrInvalidVitals)
	}
	t = stm / 100
	tm, ok := checkedMul(t, 100)
	if !ok {
		return 0, fmt.Errorf("sim: vitals health interval: %w", ErrInvalidVitals)
	}
	t = tm / boundInt64(int64(maxHP), 40, 100)
	t, ok = checkedSub(t, int64(factionBonus))
	if !ok {
		return 0, fmt.Errorf("sim: vitals health interval: %w", ErrInvalidVitals)
	}
	if restoratePower != 0 {
		// checkSpellPower-validated; time fits int (see below).
		tv, ok := toInt(t)
		if !ok {
			return 0, fmt.Errorf("sim: vitals health interval: %w", ErrInvalidVitals)
		}
		return ApplyRestorateAdjust(tv, restoratePower)
	}
	return int(boundInt64(t, 1000, 60000)), nil
}

// ApplyRejuvenateAdjust freezes rejuven.kod AdjustManaTime
// (spec §9.4.17): (time * (200-power)) / 200 with NO clamp in the
// helper itself (callers bound). spellPower is already resolved 1..99.
func ApplyRejuvenateAdjust(timeMs, spellPower int) (int, error) {
	if err := checkSpellPower(spellPower); err != nil {
		return 0, err
	}
	if timeMs < 0 {
		return 0, fmt.Errorf("sim: vitals rejuvenate time %d: %w", timeMs, ErrInvalidVitals)
	}
	out, ok := checkedMul(int64(timeMs), int64(200-spellPower))
	if !ok {
		return 0, fmt.Errorf("sim: vitals rejuvenate time %d: %w", timeMs, ErrInvalidVitals)
	}
	return int(out / 200), nil
}

// ApplyManaFocusAdjust freezes focus.kod AdjustManaTime (spec §9.4.17):
// (time * (200-power)) / 200, then bound 500..60000. spellPower is
// already resolved 1..99.
func ApplyManaFocusAdjust(timeMs, spellPower int) (int, error) {
	if err := checkSpellPower(spellPower); err != nil {
		return 0, err
	}
	if timeMs < 0 {
		return 0, fmt.Errorf("sim: vitals focus time %d: %w", timeMs, ErrInvalidVitals)
	}
	out, ok := checkedMul(int64(timeMs), int64(200-spellPower))
	if !ok {
		return 0, fmt.Errorf("sim: vitals focus time %d: %w", timeMs, ErrInvalidVitals)
	}
	return int(boundInt64(out/200, 500, 60000)), nil
}

// ManaRegenIntervalMs freezes source CalculateManaTime (spec §9.4.17)
// with exact order. Mana ABOVE MaxMana returns BOOST_DECAY_TIME
// (30000 ms) with no modifiers and no bounds. Otherwise:
//
//	time = 150000 + (25-mysticism)*1000
//	time = time * 200 / bound(Vigor,1,$)   (lower bound only)
//	time = time / bound(MaxMana,1,$)       (lower bound only)
//	time -= factionBonus                   (resolved phase-2 scalar)
//	time = bound(time,1000,60000)
//	iff Rejuvenate: time = (time*(200-power))/200
//	time = bound(time,500,60000)
//	iff ManaFocus:  time = bound((time*(200-power))/200,500,60000)
//
// rejuvenatePower/manaFocusPower are 0 for absent, else resolved 1..99.
func ManaRegenIntervalMs(mana, maxMana, vigor, effectiveMysticism, factionBonus, rejuvenatePower, manaFocusPower int) (int, error) {
	if mana < 0 {
		return 0, fmt.Errorf("sim: vitals mana interval mana = %d: %w", mana, ErrInvalidVitals)
	}
	if maxMana < minMaxMana {
		return 0, fmt.Errorf("sim: vitals mana interval max = %d: %w", maxMana, ErrInvalidVitals)
	}
	if vigor < minVigor || vigor > maxVigor {
		return 0, fmt.Errorf("sim: vitals mana interval vigor = %d: %w", vigor, ErrInvalidVitals)
	}
	if err := checkEffectiveAttr("mysticism", effectiveMysticism); err != nil {
		return 0, err
	}
	for _, p := range []int{rejuvenatePower, manaFocusPower} {
		if p != 0 {
			if err := checkSpellPower(p); err != nil {
				return 0, err
			}
		}
	}
	if mana > maxMana {
		return boostDecayTimeMs, nil
	}
	tm, ok := checkedAdd(int64(baseRegenTimeMs), int64(25-effectiveMysticism)*1000)
	if !ok {
		return 0, fmt.Errorf("sim: vitals mana interval: %w", ErrInvalidVitals)
	}
	t := tm
	// Validated vigor >= 1 and maxMana >= 1 reproduce the source lower
	// bounds exactly (source states no upper bound on either).
	tm, ok = checkedMul(t, 200)
	if !ok {
		return 0, fmt.Errorf("sim: vitals mana interval: %w", ErrInvalidVitals)
	}
	t = tm / int64(vigor)
	t = t / int64(maxMana)
	t, ok = checkedSub(t, int64(factionBonus))
	if !ok {
		return 0, fmt.Errorf("sim: vitals mana interval: %w", ErrInvalidVitals)
	}
	t = boundInt64(t, 1000, 60000)
	if rejuvenatePower != 0 {
		t = t * int64(200-rejuvenatePower) / 200
	}
	t = boundInt64(t, 500, 60000)
	if manaFocusPower != 0 {
		t = boundInt64(t*int64(200-manaFocusPower)/200, 500, 60000)
	}
	return int(t), nil
}

// ApplyInvigorateAdjust freezes invigor.kod AdjustVigorTime
// (spec §9.4.22): (time * (200-power)) / 200 with NO clamp in the
// helper and NO final bound on the rest interval in source.
// spellPower is already resolved 1..99.
func ApplyInvigorateAdjust(timeMs, spellPower int) (int, error) {
	if err := checkSpellPower(spellPower); err != nil {
		return 0, err
	}
	if timeMs < 0 {
		return 0, fmt.Errorf("sim: vitals invigorate time %d: %w", timeMs, ErrInvalidVitals)
	}
	out, ok := checkedMul(int64(timeMs), int64(200-spellPower))
	if !ok {
		return 0, fmt.Errorf("sim: vitals invigorate time %d: %w", timeMs, ErrInvalidVitals)
	}
	return int(out / 200), nil
}

// RestIntervalMs freezes source GetRestTime base (spec §9.4.22):
// 1000 + 30*(51-effectiveStamina), plus the resolved Invigorate seam
// (0 = absent, else 1..99). Source states NO final bound; T4a adds none.
// No timer, no resting boolean, no Start/Stop runtime (T4b/T6).
func RestIntervalMs(effectiveStamina, invigoratePower int) (int, error) {
	if err := checkEffectiveAttr("stamina", effectiveStamina); err != nil {
		return 0, err
	}
	if invigoratePower != 0 {
		if err := checkSpellPower(invigoratePower); err != nil {
			return 0, err
		}
	}
	t := 1000 + 30*(51-effectiveStamina)
	if invigoratePower != 0 {
		return ApplyInvigorateAdjust(t, invigoratePower)
	}
	return t, nil
}
