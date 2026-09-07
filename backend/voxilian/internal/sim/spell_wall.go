package sim

// WallDamageKind is the small mechanics enum for the three source
// wall archetypes T3b freezes (spec §9.3b.12): ordinary fire,
// ordinary lightning, illusionary fire. It is NOT a spell/proto/
// item/database/protocol ID; no named catalog table exists in sim.
type WallDamageKind uint8

const (
	// WallFire is the ordinary fire wall archetype.
	WallFire WallDamageKind = iota + 1
	// WallLightning is the ordinary lightning wall archetype.
	WallLightning
	// WallIllusionaryFire is the illusionary fire wall archetype
	// (passes spell power directly; illusionary IW mechanics).
	WallIllusionaryFire
)

// String returns the stable kind name (debug/test use).
func (k WallDamageKind) String() string {
	switch k {
	case WallFire:
		return "fire"
	case WallLightning:
		return "lightning"
	case WallIllusionaryFire:
		return "illusionary-fire"
	default:
		return "unknown"
	}
}

// checkWallKind rejects unknown wall kinds.
func checkWallKind(kind WallDamageKind) error {
	switch kind {
	case WallFire, WallLightning, WallIllusionaryFire:
		return nil
	default:
		return ErrInvalidWallKind
	}
}

// Wall placement arithmetic bounds (spec §9.3b.12, source
// walspell/firewall.kod, ltngwall.kod).
const (
	fireWallDivisor    = 6
	fireWallMaxCap     = 16
	lightningDivisor   = 4
	lightningMaxCap    = 25
	wallMinDamage      = 1
	fireBaseExtra      = 30
	fireBaseCap        = 180
	lightningBaseExtra = 20
	lightningBaseCap   = 120
	lightningBaseFloor = 20
	wallBaseFloor      = 30
)

// Wall element timing constants (spec §9.3b.12, source
// wallelem.kod EFFECT_INTERVAL and wallfire/wallltng GetDuration).
const (
	wallEffectIntervalMs = 1500
	wallJitterMin        = 90
	wallJitterMax        = 110
	wallLifeJitterSecs   = 20
	wallLifeMinMs        = 30000
	wallLifeMaxMs        = 200000
	// IllusionaryWallPowerThreshold is the exact source element
	// gate (spec §9.3b.13, wallfire.kod): below 35, no
	// illusionary damage effect; eligible at exactly 35.
	IllusionaryWallPowerThreshold = 35
)

// WallMaxDamage computes the placement max-damage arithmetic (spec
// §9.3b.12): fire spellPower/6 bound(1,16); lightning
// spellPower/4 bound(1,25). Illusionary fire does NOT convert:
// it returns spellPower DIRECTLY (later consumed as
// Illusionary-Wounds spell power). Spell power is T3a 1..99.
func WallMaxDamage(kind WallDamageKind, spellPower int) (int, error) {
	if err := checkWallKind(kind); err != nil {
		return 0, err
	}
	if err := checkSpellPower(spellPower); err != nil {
		return 0, err
	}
	switch kind {
	case WallFire:
		return int(boundInt64(int64(spellPower)/fireWallDivisor, wallMinDamage, fireWallMaxCap)), nil
	case WallLightning:
		return int(boundInt64(int64(spellPower)/lightningDivisor, wallMinDamage, lightningMaxCap)), nil
	default:
		return spellPower, nil
	}
}

// WallBaseLifetimeSeconds computes the placement base lifetime
// (spec §9.3b.12; NOT yet the final element lifetime): fire and
// illusionary spellPower*2+30 bound(30,180); lightning
// spellPower*2+20 bound(20,120). Spell power is T3a 1..99.
func WallBaseLifetimeSeconds(kind WallDamageKind, spellPower int) (int, error) {
	if err := checkWallKind(kind); err != nil {
		return 0, err
	}
	if err := checkSpellPower(spellPower); err != nil {
		return 0, err
	}
	switch kind {
	case WallLightning:
		return int(boundInt64(satAdd(satMul(int64(spellPower), 2), lightningBaseExtra), lightningBaseFloor, lightningBaseCap)), nil
	default:
		return int(boundInt64(satAdd(satMul(int64(spellPower), 2), fireBaseExtra), wallBaseFloor, fireBaseCap)), nil
	}
}

// RollWallLifetimeMs applies the element-constructor jitter shared
// by active fire/lightning elements and both passive fillers (spec
// §9.3b.12):
//
//	seconds = inclusive Random(baseSeconds-20, baseSeconds+20)
//	durationMs = seconds*1000, bound(..., 30000, 200000)
//
// Pure function over injected RNG; no timer, no time.Now. The
// 30 s floor matters at the low end (spell-side durations reach
// 20 s). baseSeconds MUST be >= 0 (ErrInvalidWallLifetime); a
// negative jitter low end (reachable only for base < 20, outside
// placement output) is guarded at 0 without changing reachable
// behavior.
func RollWallLifetimeMs(rng RNG, baseSeconds int) (int, error) {
	if baseSeconds < 0 {
		return 0, ErrInvalidWallLifetime
	}
	lo := baseSeconds - wallLifeJitterSecs
	if lo < 0 {
		lo = 0
	}
	seconds, err := rollBounded(rng, lo, baseSeconds+wallLifeJitterSecs)
	if err != nil {
		return 0, err
	}
	return int(boundInt64(satMul(int64(seconds), 1000), wallLifeMinMs, wallLifeMaxMs)), nil
}

// RollWallPeriodMs draws the next active-element periodic delay
// (spec §9.3b.12, source GetPeriodicDuration): every period draws a
// fresh percent = inclusive Random(90,110) and returns
// delayMs = (1500*percent)/100 — exact range 1350..1650 ms with
// integer source arithmetic. No scheduler, no ticker, no
// goroutine: the next delay value only. Exactly one RNG draw.
func RollWallPeriodMs(rng RNG) (int, error) {
	percent, err := rollBounded(rng, wallJitterMin, wallJitterMax)
	if err != nil {
		return 0, err
	}
	return int(satMul(wallEffectIntervalMs, int64(percent)) / 100), nil
}

// WallDamageResult is the ordinary wall-damage value (spec §9.3b.12,
// B19): pre-application raw damage plus the resistance signature
// for the future runtime. T3b runs no T2 stage inside the wall
// primitive. Policy is always PolicyOrdinary.
type WallDamageResult struct {
	RawDamage int
	Signature DamageSignature
	Policy    DamagePolicy
}

// wallDamageSignature returns the ordinary resistance signature for
// a wall kind: fire → FIRE, lightning → SHOCK (both with SPELL_ALL,
// matching source AssessDamage aspell values).
func wallDamageSignature(kind WallDamageKind) DamageSignature {
	switch kind {
	case WallLightning:
		return DamageSignature{Spell: uint32(ResistSpellAll) | uint32(ResistSpellShock)}
	default:
		return DamageSignature{Spell: uint32(ResistSpellAll) | uint32(ResistSpellFire)}
	}
}

// RollOrdinaryWallDamage resolves one active-element tick for
// ordinary fire/lightning walls (spec §9.3b.12, source
// wallfire.kod / wallltng.kod effect):
//
//	rawDamage = inclusive Random(0, maxDamage)
//
// Zero is legitimate pre-application output — T3b MUST NOT floor
// it to 1. maxDamage MUST be >= 0. Illusionary kind is rejected
// here (ErrInvalidWallKind): it uses RollIllusionaryWall.
func RollOrdinaryWallDamage(rng RNG, kind WallDamageKind, maxDamage int) (WallDamageResult, error) {
	var res WallDamageResult
	if kind != WallFire && kind != WallLightning {
		return res, ErrInvalidWallKind
	}
	if maxDamage < 0 {
		return res, ErrInvalidDamageValue
	}
	raw, err := rollBounded(rng, 0, maxDamage)
	if err != nil {
		return res, err
	}
	return WallDamageResult{
		RawDamage: raw,
		Signature: wallDamageSignature(kind),
		Policy:    PolicyOrdinary,
	}, nil
}

// IllusionaryWallResult is the illusionary-wall damage value (spec
// §9.3b.13, B21). RefundState derives from the ACTUALLY rolled
// absolute loss, never from the maximum possible loss. No
// enchantment is started in T3b. Policy is always PolicyAbsolute.
type IllusionaryWallResult struct {
	// Eligible is false when power < 35 or the IW maximum is <= 0:
	// no damage/enchantment effect.
	Eligible bool
	// RawLoss is the rolled inclusive Random(0, maxLoss) absolute loss.
	RawLoss int
	// RefundState is the future runtime's enchantment-state amount:
	// identical to RawLoss.
	RefundState int
	// DurationMs is the regular IW duration for the spell power.
	DurationMs int
	Policy     DamagePolicy
}

// RollIllusionaryWall resolves one illusionary-wall damage event
// (spec §9.3b.13, source wallfire.kod illusion branch): threshold
// gate at power 35, then maxLoss via the PRODUCTION IW loss
// primitive (mandatory reuse — no formula copy), then
// rawLoss = inclusive Random(0, maxLoss) with absolute policy.
// Non-lethal by construction (maxLoss <= HP-1, rawLoss <= maxLoss).
// Spell power is T3a 1..99.
func RollIllusionaryWall(rng RNG, victim IllusionaryVictim, spellPower int) (IllusionaryWallResult, error) {
	var res IllusionaryWallResult
	if err := checkSpellPower(spellPower); err != nil {
		return res, err
	}
	if spellPower < IllusionaryWallPowerThreshold {
		return res, nil
	}
	maxLoss, err := IllusionaryWoundsLoss(victim, spellPower)
	if err != nil {
		return res, err
	}
	if maxLoss <= 0 {
		return res, nil
	}
	raw, err := rollBounded(rng, 0, maxLoss)
	if err != nil {
		return res, err
	}
	dur, err := IllusionaryWoundsDuration(spellPower)
	if err != nil {
		return res, err
	}
	return IllusionaryWallResult{
		Eligible:    true,
		RawLoss:     raw,
		RefundState: raw,
		DurationMs:  dur,
		Policy:      PolicyAbsolute,
	}, nil
}
