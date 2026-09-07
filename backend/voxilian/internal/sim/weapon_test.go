package sim

import (
	"errors"
	"math"
	"testing"
)

// scriptRNG is a deterministic stub RNG replaying fixed Uint64 values.
type scriptRNG struct {
	vals []uint64
	at   int
}

func (s *scriptRNG) Uint64() uint64 {
	if len(s.vals) == 0 {
		return 0
	}
	v := s.vals[s.at%len(s.vals)]
	s.at++
	return v
}

func TestWeaponFamilyTableGolden(t *testing.T) {
	// Frozen spec §9.1.7 rows (verified against weapon.kod constants).
	want := map[WeaponFamily]WeaponFamilyStats{
		WeaponBludgeon: {HitMod: 75, DamageMin: 4, DamageMax: 8, DisarmMod: -5, SpellMod: 0, Range: 2},
		WeaponThrust:   {HitMod: 125, DamageMin: 3, DamageMax: 8, DisarmMod: 10, SpellMod: -10, Range: 3},
		WeaponSlash:    {HitMod: 0, DamageMin: 5, DamageMax: 11, DisarmMod: 0, SpellMod: -15, Range: 2},
	}
	for fam, w := range want {
		got, err := WeaponFamilyStatsOf(fam)
		if err != nil {
			t.Fatalf("family %d: %v", int(fam), err)
		}
		if got != w {
			t.Fatalf("family %d = %+v, want %+v", int(fam), got, w)
		}
	}
	if _, err := WeaponFamilyStatsOf(WeaponFamily(99)); !errors.Is(err, ErrUnknownWeaponFamily) {
		t.Fatalf("unknown family err = %v, want ErrUnknownWeaponFamily", err)
	}
}

func TestWeaponQualityTableGolden(t *testing.T) {
	// Frozen spec §9.1.8 rows; Normal is explicit zeros, never
	// accidental enum-zero.
	want := map[WeaponQuality]WeaponQualityMods{
		WeaponQualityLow:      {HitMod: 0, DamageMod: -1, DisarmMod: -5, SpellMod: 5, RangeMod: 0},
		WeaponQualityNormal:   {HitMod: 0, DamageMod: 0, DisarmMod: 0, SpellMod: 0, RangeMod: 0},
		WeaponQualityHigh:     {HitMod: 50, DamageMod: 1, DisarmMod: 5, SpellMod: -5, RangeMod: 0},
		WeaponQualityNerudite: {HitMod: 25, DamageMod: 1, DisarmMod: 0, SpellMod: 5, RangeMod: 0},
	}
	for q, w := range want {
		got, err := WeaponQualityModsOf(q)
		if err != nil {
			t.Fatalf("quality %d: %v", int(q), err)
		}
		if got != w {
			t.Fatalf("quality %d = %+v, want %+v", int(q), got, w)
		}
	}
	if _, err := WeaponQualityModsOf(WeaponQuality(99)); !errors.Is(err, ErrUnknownWeaponQuality) {
		t.Fatalf("unknown quality err = %v, want ErrUnknownWeaponQuality", err)
	}
}

func TestWeaponHitModifierGolden(t *testing.T) {
	// ModifyHitRoll order: family + quality + resolved bonus.
	got, err := WeaponHitModifier(WeaponSlash, WeaponQualityLow, 0)
	if err != nil || got != 0 {
		t.Fatalf("slash/low = %d,%v, want 0,nil", got, err)
	}
	got, err = WeaponHitModifier(WeaponThrust, WeaponQualityHigh, 0)
	if err != nil || got != 175 {
		t.Fatalf("thrust/high = %d,%v, want 175,nil", got, err)
	}
	got, err = WeaponHitModifier(WeaponSlash, WeaponQualityNerudite, 3)
	if err != nil || got != 28 {
		t.Fatalf("slash/nerudite/+3 = %d,%v, want 28,nil", got, err)
	}
	got, err = WeaponHitModifier(WeaponBludgeon, WeaponQualityNormal, 0)
	if err != nil || got != 75 {
		t.Fatalf("bludgeon/normal = %d,%v, want 75,nil", got, err)
	}
	if _, err := WeaponHitModifier(WeaponFamily(7), WeaponQualityNormal, 0); !errors.Is(err, ErrUnknownWeaponFamily) {
		t.Fatalf("bad family err = %v", err)
	}
	if _, err := WeaponHitModifier(WeaponSlash, WeaponQuality(7), 0); !errors.Is(err, ErrUnknownWeaponQuality) {
		t.Fatalf("bad quality err = %v", err)
	}
}

func TestRollWeaponBaseEndpoints(t *testing.T) {
	// Inclusive-range proof (spec §9.1.9): scripted raw values force
	// both endpoints of the slash 5..11 range.
	span := uint64(11 - 5 + 1)
	lo := &scriptRNG{vals: []uint64{0}}        // 5 + 0
	hi := &scriptRNG{vals: []uint64{span - 1}} // 5 + 6 = 11
	mid := &scriptRNG{vals: []uint64{3}}       // 5 + 3 = 8
	got, err := RollWeaponBase(lo, WeaponSlash, WeaponQualityNormal)
	if err != nil || got != 5 {
		t.Fatalf("min roll = %d,%v, want 5,nil", got, err)
	}
	got, err = RollWeaponBase(hi, WeaponSlash, WeaponQualityNormal)
	if err != nil || got != 11 {
		t.Fatalf("max roll = %d,%v, want 11,nil", got, err)
	}
	got, err = RollWeaponBase(mid, WeaponSlash, WeaponQualityHigh)
	if err != nil || got != 9 { // 8 + quality +1
		t.Fatalf("mid/high roll = %d,%v, want 9,nil", got, err)
	}
	got, err = RollWeaponBase(lo, WeaponBludgeon, WeaponQualityLow)
	if err != nil || got != 3 { // 4 + quality -1
		t.Fatalf("bludgeon/low min = %d,%v, want 3,nil", got, err)
	}
	if _, err := RollWeaponBase(nil, WeaponSlash, WeaponQualityNormal); !errors.Is(err, ErrNilRNG) {
		t.Fatalf("nil rng err = %v, want ErrNilRNG", err)
	}
}

func TestRawWeaponDamageGolden(t *testing.T) {
	// Representative full calculation (spec §9.1.10), hand-computed.
	// WeaponBaseDamage 9 (= slash roll 8 + high quality +1, resolved
	// upstream by RollWeaponBase): s = 9*80/100 = 7;
	// profFlat = 51*5/100 = 2; attrBonus = 40-25 = 15;
	// m = 115*7/100 = 8; raw = 10.
	got, err := RawWeaponDamage(RawDamageInput{
		WeaponBaseDamage: 9, DamageBonus: 0,
		DamageFactor: DamageFactorSlash, Proficiency: 50,
		MaxProfDamage: DefaultMaxProfDamage, Attr: 40,
	})
	if err != nil || got != 10 {
		t.Fatalf("slash example = %d,%v, want 10,nil", got, err)
	}
	// Might <= 25 baseline: +0% (might 10 -> bonus 0).
	// w = 6; s = 6*100/100 = 6; prof 0 -> flat 5/100 = 0;
	// m = 100*6/100 = 6; raw = 6.
	got, err = RawWeaponDamage(RawDamageInput{
		WeaponBaseDamage: 6, DamageFactor: DamageFactorDefault,
		MaxProfDamage: DefaultMaxProfDamage, Attr: 10,
	})
	if err != nil || got != 6 {
		t.Fatalf("might-baseline = %d,%v, want 6,nil", got, err)
	}
	// Might >= 65 capped at +40%: might 70 and 100 agree.
	// w = 6; s = 6; flat 0; m = 140*6/100 = 8; raw = 8.
	for _, might := range []int{65, 70, 100} {
		got, err = RawWeaponDamage(RawDamageInput{
			WeaponBaseDamage: 6, DamageFactor: DamageFactorDefault,
			MaxProfDamage: DefaultMaxProfDamage, Attr: might,
		})
		if err != nil || got != 8 {
			t.Fatalf("might-cap(%d) = %d,%v, want 8,nil", might, got, err)
		}
	}
	// Intermediate might: 40 -> +15%: m = 115*6/100 = 6 (690/100 trunc).
	got, err = RawWeaponDamage(RawDamageInput{
		WeaponBaseDamage: 6, DamageFactor: DamageFactorDefault,
		MaxProfDamage: DefaultMaxProfDamage, Attr: 40,
	})
	if err != nil || got != 6 {
		t.Fatalf("might-mid = %d,%v, want 6,nil", got, err)
	}
	// Fire path: Aim substitution is caller-resolved through Attr with
	// the Fire factor 90. w = 6; s = 540/100 = 5; flat 0;
	// m = 115*5/100 = 5 (575/100); raw = 5.
	got, err = RawWeaponDamage(RawDamageInput{
		WeaponBaseDamage: 6, DamageFactor: DamageFactorFire,
		MaxProfDamage: DefaultMaxProfDamage, Attr: 40,
	})
	if err != nil || got != 5 {
		t.Fatalf("fire/aim = %d,%v, want 5,nil", got, err)
	}
	// Minimum-one floor: degenerate zero pipeline still yields 1.
	got, err = RawWeaponDamage(RawDamageInput{
		WeaponBaseDamage: -50, DamageFactor: DamageFactorDefault,
		MaxProfDamage: DefaultMaxProfDamage, Attr: 1,
	})
	if err != nil || got != 1 {
		t.Fatalf("degenerate = %d,%v, want 1,nil", got, err)
	}
	// No-double-count proof: with zero proficiency and baseline might,
	// raw equals the scaled base exactly (base counted once).
	// w = 7; s = 7; m = 7; raw = 7.
	got, err = RawWeaponDamage(RawDamageInput{
		WeaponBaseDamage: 7, DamageFactor: DamageFactorDefault,
		MaxProfDamage: DefaultMaxProfDamage, Attr: 25,
	})
	if err != nil || got != 7 {
		t.Fatalf("no-double = %d,%v, want 7,nil", got, err)
	}
	// Invalid inputs fail explicitly.
	for _, bad := range []RawDamageInput{
		{DamageFactor: -1, MaxProfDamage: 5},
		{DamageFactor: 100, MaxProfDamage: -1},
		{DamageFactor: 100, MaxProfDamage: 5, Proficiency: -1},
		{DamageFactor: 100, MaxProfDamage: 5, Attr: -1},
	} {
		if _, err := RawWeaponDamage(bad); !errors.Is(err, ErrInvalidCombatStat) {
			t.Fatalf("bad %+v err = %v, want ErrInvalidCombatStat", bad, err)
		}
	}
}

// TestWeaponDamageComposition is the mandatory real-composition golden
// test: the actual output of RollWeaponBase feeds RawWeaponDamage with
// no manual restatement of the roll + quality arithmetic.
func TestWeaponDamageComposition(t *testing.T) {
	// Slash, High quality, scripted raw family roll 8 (span 7, index 3).
	// RollWeaponBase -> 8 + 1 = 9; then WeaponBaseDamage 9, bonus 0,
	// factor 80, prof 50, maxProf 5, attr 40 -> 10.
	base, err := RollWeaponBase(&scriptRNG{vals: []uint64{3}}, WeaponSlash, WeaponQualityHigh)
	if err != nil || base != 9 {
		t.Fatalf("RollWeaponBase = %d,%v, want 9,nil", base, err)
	}
	got, err := RawWeaponDamage(RawDamageInput{
		WeaponBaseDamage: base, DamageBonus: 0,
		DamageFactor: DamageFactorSlash, Proficiency: 50,
		MaxProfDamage: DefaultMaxProfDamage, Attr: 40,
	})
	if err != nil || got != 10 {
		t.Fatalf("composition = %d,%v, want 10,nil", got, err)
	}
}

// TestWeaponQualityOnceMatrix proves each quality modifier enters
// exactly once through the real RollWeaponBase -> RawWeaponDamage
// composition: a re-applied (twice) or dropped (zero times) modifier
// fails the expected values. Fixed slash roll 8 (script index 3),
// DamageBonus 0, factor 100 (identity scaling), prof 0, maxProf 5
// (flat 0), attr 25 (baseline +0%): final equals the composed base.
func TestWeaponQualityOnceMatrix(t *testing.T) {
	cases := []struct {
		quality WeaponQuality
		// wantBase is 8 + the frozen quality DamageMod.
		wantBase int
	}{
		{WeaponQualityLow, 7},
		{WeaponQualityNormal, 8},
		{WeaponQualityHigh, 9},
		{WeaponQualityNerudite, 9},
	}
	for _, c := range cases {
		base, err := RollWeaponBase(&scriptRNG{vals: []uint64{3}}, WeaponSlash, c.quality)
		if err != nil || base != c.wantBase {
			t.Fatalf("quality %d base = %d,%v, want %d,nil", int(c.quality), base, err, c.wantBase)
		}
		got, err := RawWeaponDamage(RawDamageInput{
			WeaponBaseDamage: base,
			DamageFactor:     DamageFactorDefault,
			MaxProfDamage:    DefaultMaxProfDamage,
			Attr:             25,
		})
		if err != nil || got != c.wantBase {
			t.Fatalf("quality %d final = %d,%v, want %d,nil", int(c.quality), got, err, c.wantBase)
		}
	}
}

// TestWeaponDamageEndpointComposition forces the minimum and maximum
// family rolls through the full public composition, preserving the
// inclusive range behavior end to end (slash 5..11, span 7).
func TestWeaponDamageEndpointComposition(t *testing.T) {
	lo, err := RollWeaponBase(&scriptRNG{vals: []uint64{0}}, WeaponSlash, WeaponQualityNormal)
	if err != nil || lo != 5 {
		t.Fatalf("min composition base = %d,%v, want 5,nil", lo, err)
	}
	hi, err := RollWeaponBase(&scriptRNG{vals: []uint64{6}}, WeaponSlash, WeaponQualityNormal)
	if err != nil || hi != 11 {
		t.Fatalf("max composition base = %d,%v, want 11,nil", hi, err)
	}
	for _, base := range []int{lo, hi} {
		got, err := RawWeaponDamage(RawDamageInput{
			WeaponBaseDamage: base,
			DamageFactor:     DamageFactorDefault,
			MaxProfDamage:    DefaultMaxProfDamage,
			Attr:             25,
		})
		if err != nil || got != base {
			t.Fatalf("endpoint base %d final = %d,%v, want %d,nil", base, got, err, base)
		}
	}
}

// TestWeaponDamageBonusOnce proves DamageBonus enters exactly once
// before DamageFactor scaling: with composed base 8 (slash/normal,
// script index 3), factor 100, flat 0, baseline attr, bonus b yields
// exactly 8 + b.
func TestWeaponDamageBonusOnce(t *testing.T) {
	base, err := RollWeaponBase(&scriptRNG{vals: []uint64{3}}, WeaponSlash, WeaponQualityNormal)
	if err != nil || base != 8 {
		t.Fatalf("base = %d,%v, want 8,nil", base, err)
	}
	for _, bonus := range []int{0, 5, -3} {
		got, err := RawWeaponDamage(RawDamageInput{
			WeaponBaseDamage: base, DamageBonus: bonus,
			DamageFactor:  DamageFactorDefault,
			MaxProfDamage: DefaultMaxProfDamage,
			Attr:          25,
		})
		if err != nil || got != 8+bonus {
			t.Fatalf("bonus %d final = %d,%v, want %d,nil", bonus, got, err, 8+bonus)
		}
	}
	// Scaling proof: the same bonus enters before the factor.
	// base 8 + bonus 5 = 13; 13*80/100 = 10 (1040/100 trunc).
	got, err := RawWeaponDamage(RawDamageInput{
		WeaponBaseDamage: base, DamageBonus: 5,
		DamageFactor: DamageFactorSlash,
		Proficiency:  0, MaxProfDamage: DefaultMaxProfDamage, Attr: 25,
	})
	if err != nil || got != 10 {
		t.Fatalf("scaled bonus final = %d,%v, want 10,nil", got, err)
	}
}

func TestWeaponLookupValueStability(t *testing.T) {
	// Successful lookups are immutable/value-only: mutating a returned
	// copy never affects the next lookup (property, spec §9.1.17).
	for _, f := range []WeaponFamily{WeaponBludgeon, WeaponThrust, WeaponSlash} {
		a, err := WeaponFamilyStatsOf(f)
		if err != nil {
			t.Fatal(err)
		}
		a.HitMod = -9999
		b, err := WeaponFamilyStatsOf(f)
		if err != nil {
			t.Fatal(err)
		}
		if b.HitMod == -9999 {
			t.Fatalf("family %d lookup mutated by caller", int(f))
		}
	}
	for _, q := range []WeaponQuality{WeaponQualityLow, WeaponQualityNormal, WeaponQualityHigh, WeaponQualityNerudite} {
		a, err := WeaponQualityModsOf(q)
		if err != nil {
			t.Fatal(err)
		}
		a.DamageMod = -9999
		b, err := WeaponQualityModsOf(q)
		if err != nil {
			t.Fatal(err)
		}
		if b.DamageMod == -9999 {
			t.Fatalf("quality %d lookup mutated by caller", int(q))
		}
	}
}

func TestRawDamageMonotonic(t *testing.T) {
	// Monotonicity where guaranteed: raw damage is non-decreasing in
	// the base roll with all else fixed (property, independent oracle:
	// simple loop comparison, no production call for expectations).
	prev := 0
	for base := 0; base <= 12; base++ {
		got, err := RawWeaponDamage(RawDamageInput{
			WeaponBaseDamage: base, DamageFactor: DamageFactorSlash,
			Proficiency: 30, MaxProfDamage: DefaultMaxProfDamage, Attr: 40,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got < prev {
			t.Fatalf("non-monotonic at base %d: %d < %d", base, got, prev)
		}
		prev = got
	}
}

func TestRawDamageLargeInputs(t *testing.T) {
	// Hostile-scale inputs stay total: no panic, no sign flip, floor 1.
	larges := []int{0, 1, 1 << 30, math.MaxInt - 1, math.MaxInt}
	for _, v := range larges {
		for _, in := range []RawDamageInput{
			{WeaponBaseDamage: v, DamageFactor: 100, MaxProfDamage: 5, Attr: 40},
			{WeaponBaseDamage: 6, DamageBonus: v, DamageFactor: 100, MaxProfDamage: 5, Attr: 40},
			{WeaponBaseDamage: 6, DamageFactor: v, MaxProfDamage: 5, Attr: 40},
		} {
			got, err := RawWeaponDamage(in)
			if err != nil {
				t.Fatalf("input %+v: unexpected err %v", in, err)
			}
			if got < 1 {
				t.Fatalf("input %+v: got %d, want >= 1", in, got)
			}
		}
	}
	negBonus := RawDamageInput{WeaponBaseDamage: 1, DamageBonus: -100, DamageFactor: 100, MaxProfDamage: 5, Attr: 1}
	if got, err := RawWeaponDamage(negBonus); err != nil || got != 1 {
		t.Fatalf("negative bonus = %d,%v, want 1,nil", got, err)
	}
}

func FuzzRawWeaponDamage(f *testing.F) {
	f.Add(6, 0, 100, 50, 5, 40)
	f.Add(1, 0, 80, 0, 5, 25)
	f.Add(11, 10, 90, 99, 5, 70)
	f.Fuzz(func(t *testing.T, base, bonus, factor, prof, maxProf, attr int) {
		got, err := RawWeaponDamage(RawDamageInput{
			WeaponBaseDamage: base, DamageBonus: bonus,
			DamageFactor: factor, Proficiency: prof,
			MaxProfDamage: maxProf, Attr: attr,
		})
		if factor < 0 || maxProf < 0 || prof < 0 || attr < 0 {
			if !errors.Is(err, ErrInvalidCombatStat) {
				t.Fatalf("expected ErrInvalidCombatStat, got %d,%v", got, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got < 1 {
			t.Fatalf("damage %d < 1", got)
		}
	})
}
