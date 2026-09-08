package sim

import (
	"errors"
	"fmt"
	"math"
)

// Stable vitals-domain errors (spec §9.4.26). Matching MUST use
// errors.Is, never string parsing as control flow. ErrInvalidExertion
// (spell.go) is reused for exertion-amount domain errors;
// ErrInvalidCombatStat is reused for already-resolved effective
// attribute inputs outside 1..70; ErrInvalidSpellPower is reused for
// Jala/Focus power seams outside 1..99.
var (
	// ErrInvalidVitals marks a corrupt canonical vitals value or
	// call-contract violation (out-of-domain state field, threshold,
	// multiplier, timer input, filling).
	ErrInvalidVitals = errors.New("sim: invalid vitals")
	// ErrInvalidHealthAmount marks a negative or overflowing requested
	// health loss/gain/base/max adjustment.
	ErrInvalidHealthAmount = errors.New("sim: invalid health amount")
	// ErrInvalidManaAmount marks a negative or overflowing requested
	// mana loss/gain/max adjustment or max-mana sum.
	ErrInvalidManaAmount = errors.New("sim: invalid mana amount")
	// ErrInvalidRestThreshold marks a raw rest-threshold input outside
	// the source 10..100 domain.
	ErrInvalidRestThreshold = errors.New("sim: invalid rest threshold")
	// ErrInvalidElapsedTime marks a negative (or overflowing) lazy
	// stomach-decay elapsed-seconds input.
	ErrInvalidElapsedTime = errors.New("sim: invalid elapsed time")
	// ErrInvalidNodeKind marks an unknown mana-node mechanics variant.
	ErrInvalidNodeKind = errors.New("sim: invalid node kind")
)

// Canonical vitals bounds (spec §9.4.1–§9.4.3, source player.kod).
const (
	minVigor          = 1
	maxVigor          = 200 // source viMax_vigor
	minRestThreshold  = 10
	maxRestThreshold  = 100
	defaultThreshold  = 80
	minBaseMaxHP      = 20
	maxBaseMaxHPHard  = 150 // source hard cap
	minMaxHP          = 20  // source GainMaxHealth lower bound
	minMaxMana        = 1   // T4a corrupt-reject (source states no bound)
	minVigorChange    = 20000
	maxExertionAbs    = 20000 // canonical post-mutation residual range
	foodUseRate       = 12    // source FOOD_USE_RATE
	minStomach        = 0     // pre-first-update only
	maxStomach        = 100
	postUpdateStomMin = 1 // UpdateStomach bounds the result to 1..100
)

// PlayerVitals is the canonical authoritative player-vitals value
// (spec §9.4.1, source piHealth/piBase_max_health/piMax_health/piMana/
// piMax_mana/piVigor/piVigor_rest_threshold/piExertion/piStomach).
//
// Vigor is visible whole points; Exertion is the signed 1/10000-vigor
// residual accumulator (10000 exertion = 1 vigor). The struct is a plain
// value: copying it produces an independent snapshot (no pointers, maps,
// or slices). All mutation/derivation functions are pure and take/return
// this value; there is deliberately NO CharacterID, EntityID, timer
// handle, or wall-clock timestamp here (T4b owns runtime attachment).
//
// JSON tags implement the durable compatibility contract (spec §9.4.2):
// the creation field names are preserved exactly, and the authoritative
// residual extends them with `exertion` (missing on decode = zero).
type PlayerVitals struct {
	HP            int   `json:"hp"`
	BaseMaxHP     int   `json:"base_max"`
	MaxHP         int   `json:"max"`
	Mana          int   `json:"mana"`
	MaxMana       int   `json:"max_mana"`
	Vigor         int   `json:"vigor"`
	RestThreshold int   `json:"threshold"`
	Exertion      int64 `json:"exertion"`
	Stomach       int   `json:"stomach"`
}

// Validate rejects impossible/corrupt values (spec §9.4.3). It enforces
// ONLY source-valid domains: HP/Mana have lower bounds but NO upper
// bounds (over-max states are legal), and MaxHP/MaxMana move
// independently of BaseMaxHP/HP. In particular Validate MUST NOT reject
// HP > MaxHP, HP > 2*MaxHP, MaxHP < BaseMaxHP, or Mana > MaxMana.
func (v PlayerVitals) Validate() error {
	if v.HP < 0 {
		return fmt.Errorf("sim: vitals hp = %d: %w", v.HP, ErrInvalidVitals)
	}
	if v.BaseMaxHP < minBaseMaxHP || v.BaseMaxHP > maxBaseMaxHPHard {
		return fmt.Errorf("sim: vitals base_max = %d: %w", v.BaseMaxHP, ErrInvalidVitals)
	}
	if v.MaxHP < minMaxHP {
		return fmt.Errorf("sim: vitals max = %d: %w", v.MaxHP, ErrInvalidVitals)
	}
	if v.Mana < 0 {
		return fmt.Errorf("sim: vitals mana = %d: %w", v.Mana, ErrInvalidVitals)
	}
	if v.MaxMana < minMaxMana {
		return fmt.Errorf("sim: vitals max_mana = %d: %w", v.MaxMana, ErrInvalidVitals)
	}
	if v.Vigor < minVigor || v.Vigor > maxVigor {
		return fmt.Errorf("sim: vitals vigor = %d: %w", v.Vigor, ErrInvalidVitals)
	}
	if v.RestThreshold < minRestThreshold || v.RestThreshold > maxRestThreshold {
		return fmt.Errorf("sim: vitals threshold = %d: %w", v.RestThreshold, ErrInvalidVitals)
	}
	if v.Exertion < -maxExertionAbs || v.Exertion > maxExertionAbs {
		return fmt.Errorf("sim: vitals exertion = %d: %w", v.Exertion, ErrInvalidVitals)
	}
	if v.Stomach < minStomach || v.Stomach > maxStomach {
		return fmt.Errorf("sim: vitals stomach = %d: %w", v.Stomach, ErrInvalidVitals)
	}
	return nil
}

// NewPlayerVitals builds the canonical creation state (spec §9.4.24):
// HP/BaseMax/Max 20, Mana/MaxMana 15+Myst/5, Vigor 100, Threshold 80,
// Exertion 0, Stomach 0. effectiveMysticism is the already-resolved value
// (creation passes the raw 1..50 stat, inside the 1..70 domain).
func NewPlayerVitals(effectiveMysticism int) (PlayerVitals, error) {
	mana, err := InitialMaxMana(effectiveMysticism)
	if err != nil {
		return PlayerVitals{}, err
	}
	return PlayerVitals{
		HP: 20, BaseMaxHP: 20, MaxHP: 20,
		Mana: mana, MaxMana: mana,
		Vigor: 100, RestThreshold: defaultThreshold,
		Exertion: 0, Stomach: 0,
	}, nil
}

// checkEffectiveAttr validates an already-resolved effective attribute
// (source bound(base+mod,1,70), MAXIMUM_STAT=70). T4a performs no lookup.
func checkEffectiveAttr(name string, v int) error {
	if v < 1 || v > 70 {
		return fmt.Errorf("sim: vitals %s = %d: %w", name, v, ErrInvalidCombatStat)
	}
	return nil
}

// checkedAdd/checkedSub/checkedMul report overflow instead of wrapping so
// hostile inputs can never flip a sign or bypass a bound (spec §B3).
func checkedAdd(a, b int64) (int64, bool) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, false
	}
	return a + b, true
}

func checkedSub(a, b int64) (int64, bool) {
	if (b > 0 && a < math.MinInt64+b) || (b < 0 && a > math.MaxInt64+b) {
		return 0, false
	}
	return a - b, true
}

func checkedMul(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	if (a == math.MinInt64 && b == -1) || (b == math.MinInt64 && a == -1) {
		return 0, false
	}
	r := a * b
	if r/b != a {
		return 0, false
	}
	return r, true
}

// toInt converts an int64 to int, reporting overflow (portability guard;
// int is 64-bit on all supported targets).
func toInt(v int64) (int, bool) {
	const maxInt = int64(^uint(0) >> 1)
	const minInt = -maxInt - 1
	if v > maxInt || v < minInt {
		return 0, false
	}
	return int(v), true
}

// InitialMaxMana freezes source GetInitialMaxMana (spec §9.4.11):
// 15 + effectiveMysticism/5, integer truncation, no lookup.
func InitialMaxMana(effectiveMysticism int) (int, error) {
	if err := checkEffectiveAttr("mysticism", effectiveMysticism); err != nil {
		return 0, err
	}
	return 15 + effectiveMysticism/5, nil
}

// AdjustBaseMaxHP freezes source GainBaseMaxHealth (spec §9.4.4):
//
//	newBase = bound(oldBase + amount, 20, 100 + effectiveStamina)
//	newBase = bound(newBase, no-lower-change, 150)
//
// effectiveStamina is already resolved (1..70). Returns the new state and
// the ACTUAL delta (0 at a floor/ceiling). The source follow-on that adds
// the same delta to MaxHP is runtime composition, not part of this
// primitive. No advancement decision lives here.
func AdjustBaseMaxHP(v PlayerVitals, amount, effectiveStamina int) (PlayerVitals, int, error) {
	if err := v.Validate(); err != nil {
		return v, 0, err
	}
	if err := checkEffectiveAttr("stamina", effectiveStamina); err != nil {
		return v, 0, err
	}
	sum, ok := checkedAdd(int64(v.BaseMaxHP), int64(amount))
	if !ok {
		return v, 0, fmt.Errorf("sim: vitals base_max adjust %d: %w", amount, ErrInvalidHealthAmount)
	}
	newBase := boundInt64(sum, minBaseMaxHP, int64(100+effectiveStamina))
	if newBase > maxBaseMaxHPHard {
		newBase = maxBaseMaxHPHard
	}
	out, ok := toInt(newBase)
	if !ok {
		return v, 0, fmt.Errorf("sim: vitals base_max adjust %d: %w", amount, ErrInvalidHealthAmount)
	}
	delta := out - v.BaseMaxHP
	v.BaseMaxHP = out
	return v, delta, nil
}

// AdjustMaxHP freezes source GainMaxHealth (spec §9.4.5):
// MaxHP = bound(MaxHP + amount, 20, $). Current HP is deliberately NOT
// touched, preserving existing over-max health. Returns the actual delta.
func AdjustMaxHP(v PlayerVitals, amount int) (PlayerVitals, int, error) {
	if err := v.Validate(); err != nil {
		return v, 0, err
	}
	sum, ok := checkedAdd(int64(v.MaxHP), int64(amount))
	if !ok {
		return v, 0, fmt.Errorf("sim: vitals max adjust %d: %w", amount, ErrInvalidHealthAmount)
	}
	out, ok := toInt(boundInt64(sum, minMaxHP, math.MaxInt64))
	if !ok {
		return v, 0, fmt.Errorf("sim: vitals max adjust %d: %w", amount, ErrInvalidHealthAmount)
	}
	delta := out - v.MaxHP
	v.MaxHP = out
	return v, delta, nil
}

// AdjustMaxMana covers the source NewMaxMana add path (spec §9.4.13):
// MaxMana += amount with NO bound (source adds blindly). The node-list
// bitmask is excluded from T4a. Returns the actual delta (= amount).
func AdjustMaxMana(v PlayerVitals, amount int) (PlayerVitals, int, error) {
	if err := v.Validate(); err != nil {
		return v, 0, err
	}
	sum, ok := checkedAdd(int64(v.MaxMana), int64(amount))
	if !ok {
		return v, 0, fmt.Errorf("sim: vitals max_mana adjust %d: %w", amount, ErrInvalidManaAmount)
	}
	out, ok := toInt(sum)
	if !ok {
		return v, 0, fmt.Errorf("sim: vitals max_mana adjust %d: %w", amount, ErrInvalidManaAmount)
	}
	v.MaxMana = out
	return v, amount, nil
}

// HealthLossResult exposes the before/after/applied triple plus the two
// booleans later composition needs (spec §9.4.6): ZeroHP for the T5
// death handoff, Decay to distinguish over-max decay from combat damage
// (decay must never break trance in T7).
type HealthLossResult struct {
	Before  int
	After   int
	Applied int
	ZeroHP  bool
	Decay   bool
}

// LoseHealth freezes source LoseHealth value semantics (spec §9.4.6),
// minus the trance break (T7 owns "damage breaks trance"; T4a owns no
// live trance state). amount MUST be non-negative:
//
//	after = max(before - amount, 0); applied = before - after
//
// No death transition, no corpse, no ledger (M5-T5).
func LoseHealth(v PlayerVitals, amount int, decay bool) (PlayerVitals, HealthLossResult, error) {
	if err := v.Validate(); err != nil {
		return v, HealthLossResult{}, err
	}
	if amount < 0 {
		return v, HealthLossResult{}, fmt.Errorf("sim: vitals lose health %d: %w", amount, ErrInvalidHealthAmount)
	}
	before := v.HP
	after := before - amount
	if after < 0 {
		after = 0
	}
	v.HP = after
	return v, HealthLossResult{
		Before: before, After: after,
		Applied: before - after,
		ZeroHP:  after == 0,
		Decay:   decay,
	}, nil
}

// GainHealthNormal freezes source GainHealthNormal (spec §9.4.7).
// Negative amount is a domain error. HP above MaxHP is left unchanged
// (gain 0); otherwise HP rises capped at MaxHP. Returns actual gained.
func GainHealthNormal(v PlayerVitals, amount int) (PlayerVitals, int, error) {
	if err := v.Validate(); err != nil {
		return v, 0, err
	}
	if amount < 0 {
		return v, 0, fmt.Errorf("sim: vitals normal heal %d: %w", amount, ErrInvalidHealthAmount)
	}
	if v.HP > v.MaxHP {
		return v, 0, nil
	}
	sum, ok := checkedAdd(int64(v.HP), int64(amount))
	if !ok {
		return v, 0, fmt.Errorf("sim: vitals normal heal %d: %w", amount, ErrInvalidHealthAmount)
	}
	after := sum
	if after > int64(v.MaxHP) {
		after = int64(v.MaxHP)
	}
	out, ok := toInt(after)
	if !ok {
		return v, 0, fmt.Errorf("sim: vitals normal heal %d: %w", amount, ErrInvalidHealthAmount)
	}
	gained := out - v.HP
	v.HP = out
	return v, gained, nil
}

// GainHealthOvercap freezes source GainHealth separately (spec §9.4.8):
// ceiling 2*MaxHP (overflow-safe doubling), NEVER merged with normal
// healing. The already-above-2*Max corner (reachable after a MaxHP
// reduction) resolves per source even though unintuitive: the condition
// holds, so HP is SET DOWN to 2*MaxHP and the reported delta
// (after-before) is negative. Returns the actual delta.
func GainHealthOvercap(v PlayerVitals, amount int) (PlayerVitals, int, error) {
	if err := v.Validate(); err != nil {
		return v, 0, err
	}
	if amount < 0 {
		return v, 0, fmt.Errorf("sim: vitals overcap heal %d: %w", amount, ErrInvalidHealthAmount)
	}
	cap2, ok := checkedMul(int64(v.MaxHP), 2)
	if !ok {
		return v, 0, fmt.Errorf("sim: vitals overcap heal %d: %w", amount, ErrInvalidHealthAmount)
	}
	before := v.HP
	sum, sumOK := checkedAdd(int64(before), int64(amount))
	after := sum
	if !sumOK || sum > cap2 {
		after = cap2
	}
	out, ok := toInt(after)
	if !ok {
		return v, 0, fmt.Errorf("sim: vitals overcap heal %d: %w", amount, ErrInvalidHealthAmount)
	}
	v.HP = out
	return v, out - before, nil
}

// LoseMana freezes source LoseMana value semantics (spec §9.4.14): for
// non-negative loss, Mana -= amount clamped at 0 (source clamps in
// NewMana; T4a clamps in the primitive). Returns the ACTUAL mana lost.
// (Source leaves its local uninitialized when nothing clamps; T4a always
// returns a number.) No spell gate here.
func LoseMana(v PlayerVitals, amount int) (PlayerVitals, int, error) {
	if err := v.Validate(); err != nil {
		return v, 0, err
	}
	if amount < 0 {
		return v, 0, fmt.Errorf("sim: vitals lose mana %d: %w", amount, ErrInvalidManaAmount)
	}
	after := v.Mana - amount
	if after < 0 {
		after = 0
	}
	lost := v.Mana - after
	v.Mana = after
	return v, lost, nil
}

// GainMana freezes source GainMana both modes (spec §9.4.15). Negative
// amount is a domain error. Uncapped gains may exceed MaxMana (audit
// finds no source upper bound) and return amount. Capped gains clamp to
// MaxMana and return the EXACT source delta amount - (tempMana - MaxMana),
// which is NEGATIVE when Mana already starts above MaxMana (source-faithful
// corner, analogous to GainHealthOvercap): 23/20 +5 capped -> Mana 20,
// gain -3. The equivalent MaxMana - Mana form below is overflow-safe.
func GainMana(v PlayerVitals, amount int, capped bool) (PlayerVitals, int, error) {
	if err := v.Validate(); err != nil {
		return v, 0, err
	}
	if amount < 0 {
		return v, 0, fmt.Errorf("sim: vitals gain mana %d: %w", amount, ErrInvalidManaAmount)
	}
	sum, ok := checkedAdd(int64(v.Mana), int64(amount))
	if !ok {
		// Non-negative operands overflowed: the temporary mana exceeds
		// any MaxMana. Uncapped cannot represent the result; capped
		// falls through to the clamp with the overflow-safe delta.
		if !capped {
			return v, 0, fmt.Errorf("sim: vitals gain mana %d: %w", amount, ErrInvalidManaAmount)
		}
		return applyManaCap(v)
	}
	if capped && sum > int64(v.MaxMana) {
		return applyManaCap(v)
	}
	out, ok := toInt(sum)
	if !ok {
		return v, 0, fmt.Errorf("sim: vitals gain mana %d: %w", amount, ErrInvalidManaAmount)
	}
	v.Mana = out
	return v, amount, nil
}

// applyManaCap resolves the capped clamp: Mana = MaxMana, returning the
// exact source delta amount - (tempMana - MaxMana) via the algebraically
// identical overflow-safe form MaxMana - Mana (tempMana - amount == Mana
// exactly). Negative when Mana starts above MaxMana; never overflows:
// Mana is in [0, MaxInt64] and MaxMana >= 1, so the difference fits int64.
func applyManaCap(v PlayerVitals) (PlayerVitals, int, error) {
	gained, ok := toInt(int64(v.MaxMana) - int64(v.Mana))
	if !ok {
		return v, 0, fmt.Errorf("sim: vitals gain mana cap: %w", ErrInvalidManaAmount)
	}
	v.Mana = v.MaxMana
	return v, gained, nil
}

// HasVigor freezes source HasVigor exactly (spec §9.4.18): STRICT `>`,
// never `>=`. The authoritative-state counterpart for later T7/T6; T1/T3a
// cost-plan arithmetic is not duplicated here.
func (v PlayerVitals) HasVigor(required int) (bool, error) {
	if err := v.Validate(); err != nil {
		return false, err
	}
	if required < 0 {
		return false, fmt.Errorf("sim: vitals has vigor %d: %w", required, ErrInvalidExertion)
	}
	return v.Vigor > required, nil
}

// ApplyExertion freezes the source AddExertion accumulator semantics
// (spec §9.4.19) AFTER already-resolved external reductions/blocks.
// Faction percentage reduction, Second Wind blocking/auto-invocation, and
// skill lookup are EXCLUDED (M5-T6 owns Second Wind; factions phase 2):
//
//	Exertion += amount
//	if abs(Exertion) > 20000 OR setToThreshold:
//	    if setToThreshold AND Vigor < RestThreshold:
//	        Vigor = RestThreshold; Exertion = 0
//	    else:
//	        vigorLost = Exertion / 10000   (signed, trunc toward zero)
//	        Vigor -= vigorLost
//	        Exertion -= vigorLost * 10000  (sub-10000 residual PRESERVED)
//	    Vigor = bound(Vigor, 1, 200)
//
// Critical: abs(exertion) == 20000 does NOT trigger conversion (strict
// >). Overflow of the accumulator is a domain error, never saturation.
// setToThreshold is the generic value-only policy flag.
func ApplyExertion(v PlayerVitals, amount int64, setToThreshold bool) (PlayerVitals, error) {
	if err := v.Validate(); err != nil {
		return v, err
	}
	e, ok := checkedAdd(v.Exertion, amount)
	if !ok {
		return v, fmt.Errorf("sim: vitals exertion %d: %w", amount, ErrInvalidExertion)
	}
	v.Exertion = e
	if e > maxExertionAbs || e < -maxExertionAbs || setToThreshold {
		if setToThreshold && v.Vigor < v.RestThreshold {
			v.Vigor = v.RestThreshold
			v.Exertion = 0
		} else {
			lost := e / ExertionPerVigor // signed, truncates toward zero
			v.Vigor = int(boundInt64(int64(v.Vigor)-lost, minVigor, maxVigor))
			v.Exertion = e - lost*ExertionPerVigor
		}
	}
	return v, nil
}

// ApplyRestExertion freezes source RestAddExertion as a SEPARATE path
// (spec §9.4.21) with three frozen distinctions from ApplyExertion:
// (a) no-op while Vigor > RestThreshold (state untouched); (b) an
// already-resolved room multiplier (1 ordinary, 2 sanctuary, 3
// triple-heal; source assignment order makes both-flags-set mean 3x —
// the caller resolves, T4a only validates 1..3; the multiplier applies
// to negative/recovery amounts per source, positive amounts pass
// through); (c) conversion CLEARS the residual (Exertion = 0) and
// overshoot clamps UP to the threshold:
//
//	if Vigor > RestThreshold: no-op
//	Exertion += amount [* multiplier iff amount < 0]
//	if abs(Exertion) > 20000:
//	    Vigor -= Exertion / 10000
//	    if Vigor > RestThreshold: Vigor = RestThreshold
//	    Exertion = 0
//	    Vigor = bound(Vigor, 1, 200)
func ApplyRestExertion(v PlayerVitals, amount int64, roomMultiplier int) (PlayerVitals, error) {
	if err := v.Validate(); err != nil {
		return v, err
	}
	if roomMultiplier < 1 || roomMultiplier > 3 {
		return v, fmt.Errorf("sim: vitals rest multiplier %d: %w", roomMultiplier, ErrInvalidVitals)
	}
	if v.Vigor > v.RestThreshold {
		return v, nil
	}
	scaled := amount
	if amount < 0 {
		m, ok := checkedMul(amount, int64(roomMultiplier))
		if !ok {
			return v, fmt.Errorf("sim: vitals rest exertion %d: %w", amount, ErrInvalidExertion)
		}
		scaled = m
	}
	e, ok := checkedAdd(v.Exertion, scaled)
	if !ok {
		return v, fmt.Errorf("sim: vitals rest exertion %d: %w", amount, ErrInvalidExertion)
	}
	v.Exertion = e
	if e > maxExertionAbs || e < -maxExertionAbs {
		v.Vigor = int(boundInt64(int64(v.Vigor)-e/ExertionPerVigor, minVigor, maxVigor))
		if v.Vigor > v.RestThreshold {
			v.Vigor = v.RestThreshold
		}
		v.Exertion = 0
	}
	return v, nil
}

// SetRestThreshold freezes the source 10..100 domain (spec §9.4.20).
// Source clamps at runtime (plus a Second Wind force-to-10 owned by T6);
// T4a validates explicitly: raw input outside 10..100 is
// ErrInvalidRestThreshold, never a silent clamp.
func SetRestThreshold(v PlayerVitals, threshold int) (PlayerVitals, error) {
	if err := v.Validate(); err != nil {
		return v, err
	}
	if threshold < minRestThreshold || threshold > maxRestThreshold {
		return v, fmt.Errorf("sim: vitals rest threshold %d: %w", threshold, ErrInvalidRestThreshold)
	}
	v.RestThreshold = threshold
	return v, nil
}

// DecayStomach freezes source UpdateStomach as a pure function over
// (current stomach, elapsed whole seconds) — NO clock, NO stored
// timestamp in T4a (spec §9.4.23). GetTime() is seconds-based server
// time, hence whole-second inputs; T4b/T6 supply elapsed time.
//
//	decayed = stomach - (elapsedSeconds * 12) / 100   (multiply FIRST)
//	result = bound(decayed, 1, 100)
//
// stomach=0 with elapsed=0 yields 1 (initial-zero vs post-update
// distinction — pinned in tests). Negative elapsed is a domain error;
// the multiply uses overflow-safe 64-bit arithmetic.
func DecayStomach(stomach int, elapsedSeconds int64) (int, error) {
	if stomach < minStomach || stomach > maxStomach {
		return 0, fmt.Errorf("sim: vitals stomach = %d: %w", stomach, ErrInvalidVitals)
	}
	if elapsedSeconds < 0 {
		return 0, fmt.Errorf("sim: vitals stomach elapsed %d: %w", elapsedSeconds, ErrInvalidElapsedTime)
	}
	prod, ok := checkedMul(elapsedSeconds, foodUseRate)
	if !ok {
		return 0, fmt.Errorf("sim: vitals stomach elapsed %d: %w", elapsedSeconds, ErrInvalidElapsedTime)
	}
	drop := prod / 100
	decayed, ok := checkedSub(int64(stomach), drop)
	if !ok {
		// Only reachable for absurd elapsed values; the bound below
		// maps any such underflow to the floor. Not gameplay
		// saturation: the mathematical result is far below 1.
		decayed = postUpdateStomMin
	}
	return int(boundInt64(decayed, postUpdateStomMin, maxStomach)), nil
}

// CanEat freezes the source ReqEatSomething value rule over the
// POST-update stomach (spec §9.4.23): allow iff stomach + filling <=
// 100 (equality passes, 101 fails). T6 owns eat intent, items, and
// nutrition→exertion composition; this is only the capacity seam.
func CanEat(updatedStomach, filling int) (bool, error) {
	if updatedStomach < minStomach || updatedStomach > maxStomach {
		return false, fmt.Errorf("sim: vitals stomach = %d: %w", updatedStomach, ErrInvalidVitals)
	}
	if filling < 0 {
		return false, fmt.Errorf("sim: vitals filling = %d: %w", filling, ErrInvalidVitals)
	}
	sum, ok := checkedAdd(int64(updatedStomach), int64(filling))
	if !ok {
		return false, fmt.Errorf("sim: vitals filling = %d: %w", filling, ErrInvalidVitals)
	}
	return sum <= maxStomach, nil
}
