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
	// Representative full calculation (spec §9.1.10), hand-computed:
	// w = 8 + 1 + 0 = 9; s = 9*80/100 = 7; profFlat = 51*5/100 = 2;
	// attrBonus = 40-25 = 15; m = 115*7/100 = 8; raw = 10.
	got, err := RawWeaponDamage(RawDamageInput{
		BaseRoll: 8, QualityDmgMod: 1, DamageBonus: 0,
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
		BaseRoll: 6, DamageFactor: DamageFactorDefault,
		MaxProfDamage: DefaultMaxProfDamage, Attr: 10,
	})
	if err != nil || got != 6 {
		t.Fatalf("might-baseline = %d,%v, want 6,nil", got, err)
	}
	// Might >= 65 capped at +40%: might 70 and 100 agree.
	// w = 6; s = 6; flat 0; m = 140*6/100 = 8; raw = 8.
	for _, might := range []int{65, 70, 100} {
		got, err = RawWeaponDamage(RawDamageInput{
			BaseRoll: 6, DamageFactor: DamageFactorDefault,
			MaxProfDamage: DefaultMaxProfDamage, Attr: might,
		})
		if err != nil || got != 8 {
			t.Fatalf("might-cap(%d) = %d,%v, want 8,nil", might, got, err)
		}
	}
	// Intermediate might: 40 -> +15%: m = 115*6/100 = 6 (690/100 trunc).
	got, err = RawWeaponDamage(RawDamageInput{
		BaseRoll: 6, DamageFactor: DamageFactorDefault,
		MaxProfDamage: DefaultMaxProfDamage, Attr: 40,
	})
	if err != nil || got != 6 {
		t.Fatalf("might-mid = %d,%v, want 6,nil", got, err)
	}
	// Fire path: Aim substitution is caller-resolved through Attr with
	// the Fire factor 90. w = 6; s = 540/100 = 5; flat 0;
	// m = 115*5/100 = 5 (575/100); raw = 5.
	got, err = RawWeaponDamage(RawDamageInput{
		BaseRoll: 6, DamageFactor: DamageFactorFire,
		MaxProfDamage: DefaultMaxProfDamage, Attr: 40,
	})
	if err != nil || got != 5 {
		t.Fatalf("fire/aim = %d,%v, want 5,nil", got, err)
	}
	// Minimum-one floor: degenerate zero pipeline still yields 1.
	got, err = RawWeaponDamage(RawDamageInput{
		BaseRoll: 0, QualityDmgMod: -50, DamageFactor: DamageFactorDefault,
		MaxProfDamage: DefaultMaxProfDamage, Attr: 1,
	})
	if err != nil || got != 1 {
		t.Fatalf("degenerate = %d,%v, want 1,nil", got, err)
	}
	// No-double-count proof: with zero proficiency and baseline might,
	// raw equals the scaled base exactly (base counted once).
	// w = 7; s = 7; m = 7; raw = 7.
	got, err = RawWeaponDamage(RawDamageInput{
		BaseRoll: 7, DamageFactor: DamageFactorDefault,
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
	for roll := 0; roll <= 12; roll++ {
		got, err := RawWeaponDamage(RawDamageInput{
			BaseRoll: roll, DamageFactor: DamageFactorSlash,
			Proficiency: 30, MaxProfDamage: DefaultMaxProfDamage, Attr: 40,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got < prev {
			t.Fatalf("non-monotonic at roll %d: %d < %d", roll, got, prev)
		}
		prev = got
	}
}

func TestRawDamageLargeInputs(t *testing.T) {
	// Hostile-scale inputs stay total: no panic, no sign flip, floor 1.
	larges := []int{0, 1, 1 << 30, math.MaxInt - 1, math.MaxInt}
	for _, v := range larges {
		for _, in := range []RawDamageInput{
			{BaseRoll: v, DamageFactor: 100, MaxProfDamage: 5, Attr: 40},
			{BaseRoll: 6, QualityDmgMod: v, DamageFactor: 100, MaxProfDamage: 5, Attr: 40},
			{BaseRoll: 6, DamageBonus: v, DamageFactor: 100, MaxProfDamage: 5, Attr: 40},
			{BaseRoll: 6, DamageFactor: v, MaxProfDamage: 5, Attr: 40},
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
	negBonus := RawDamageInput{BaseRoll: 1, DamageBonus: -100, DamageFactor: 100, MaxProfDamage: 5, Attr: 1}
	if got, err := RawWeaponDamage(negBonus); err != nil || got != 1 {
		t.Fatalf("negative bonus = %d,%v, want 1,nil", got, err)
	}
}

func FuzzRawWeaponDamage(f *testing.F) {
	f.Add(6, 0, 0, 100, 50, 5, 40)
	f.Add(1, -1, 0, 80, 0, 5, 25)
	f.Add(11, 1, 10, 90, 99, 5, 70)
	f.Fuzz(func(t *testing.T, base, qmod, bonus, factor, prof, maxProf, attr int) {
		got, err := RawWeaponDamage(RawDamageInput{
			BaseRoll: base, QualityDmgMod: qmod, DamageBonus: bonus,
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
