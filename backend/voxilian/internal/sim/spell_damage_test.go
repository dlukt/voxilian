package sim

import (
	"errors"
	"testing"
)

func TestScaleAttackSpellDamageGolden(t *testing.T) {
	// Source: (raw*(50+power/2))/99 with power/2 truncating FIRST.
	// Fireball-like 8..12 vectors from spec §9.3a.20.
	cases := []struct {
		raw, power, want int
		name             string
	}{
		{8, 1, 4, "min roll low power: 8*50/99 = 400/99"},
		{12, 1, 6, "max roll low power: 12*50/99 = 600/99"},
		{8, 50, 6, "min roll mid power: 8*75/99 = 600/99"},
		{12, 50, 9, "max roll mid power: 12*75/99 = 900/99"},
		{8, 99, 8, "min roll max power: 8*99/99"},
		{12, 99, 12, "max roll max power: 12*99/99"},
		// /99-vs-/100 killers: a /100 implementation fails these.
		{66, 50, 50, "mid-range /99 proof: 66*75/99 = 50 (/100 gives 49)"},
		{12, 99, 12, "max-power identity (/100 gives 11)"},
		// power/2-first truncation proof: float-style (50+power/2)
		// without integer halving would scale 50*51.5/99 -> 26.
		{50, 3, 25, "odd-power truncation: 50*51/99 = 2550/99"},
		{0, 99, 0, "zero roll stays zero"},
	}
	for _, c := range cases {
		got, err := ScaleAttackSpellDamage(c.raw, c.power, OriginPlayer)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Fatalf("%s: raw %d power %d = %d, want %d",
				c.name, c.raw, c.power, got, c.want)
		}
	}
	// Non-player origins return the raw roll with power unused.
	for _, origin := range []CastOrigin{OriginItem, OriginMonster} {
		got, err := ScaleAttackSpellDamage(12, 50, origin)
		if err != nil || got != 12 {
			t.Fatalf("origin %v = %d,%v, want 12,nil", origin, got, err)
		}
	}
	if _, err := ScaleAttackSpellDamage(8, 50, CastOrigin(0)); !errors.Is(err, ErrInvalidCastOrigin) {
		t.Fatalf("origin 0 err = %v", err)
	}
	if _, err := ScaleAttackSpellDamage(-1, 50, OriginPlayer); !errors.Is(err, ErrInvalidDamageValue) {
		t.Fatalf("negative raw err = %v", err)
	}
	if _, err := ScaleAttackSpellDamage(8, 0, OriginPlayer); !errors.Is(err, ErrInvalidSpellPower) {
		t.Fatalf("power 0 err = %v", err)
	}
	if _, err := ScaleAttackSpellDamage(8, 100, OriginPlayer); !errors.Is(err, ErrInvalidSpellPower) {
		t.Fatalf("power 100 err = %v", err)
	}
}

func TestManaFocusGolden(t *testing.T) {
	// Source: damage += ((focusPower*bonus)/99)+1, +1 unconditional.
	// Bonus scalar 5 (generic AttackSpell default piManaFocusBonus).
	cases := []struct {
		damage, power, bonus int
		want                 int
		name                 string
	}{
		{9, 1, 5, 10, "active low focus: (1*5)/99+1 = +1"},
		{9, 99, 5, 15, "active high focus: (99*5)/99+1 = +6"},
		{9, 50, 5, 12, "active mid focus: (50*5)/99+1 = 2+1"},
		{9, 99, 0, 10, "bonus 0 still +1"},
		{9, 1, 0, 10, "bonus 0 low power still +1"},
	}
	for _, c := range cases {
		got, err := ApplyManaFocus(c.damage, ManaFocusInput{Active: true, Power: c.power, Bonus: c.bonus}, OriginPlayer)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Fatalf("%s = %d, want %d", c.name, got, c.want)
		}
	}
	// Inactive focus leaves damage unchanged.
	got, err := ApplyManaFocus(9, ManaFocusInput{Active: false, Power: 99, Bonus: 5}, OriginPlayer)
	if err != nil || got != 9 {
		t.Fatalf("inactive = %d,%v, want 9,nil", got, err)
	}
	// Monster/item origins ignore focus even when active.
	for _, origin := range []CastOrigin{OriginItem, OriginMonster} {
		got, err := ApplyManaFocus(9, ManaFocusInput{Active: true, Power: 99, Bonus: 5}, origin)
		if err != nil || got != 9 {
			t.Fatalf("origin %v = %d,%v, want 9,nil", origin, got, err)
		}
	}
	if _, err := ApplyManaFocus(9, ManaFocusInput{Active: true, Power: -1}, OriginPlayer); !errors.Is(err, ErrInvalidCombatStat) {
		t.Fatalf("negative focus power err = %v", err)
	}
	if _, err := ApplyManaFocus(9, ManaFocusInput{Active: true, Bonus: -1}, OriginPlayer); !errors.Is(err, ErrInvalidCombatStat) {
		t.Fatalf("negative focus bonus err = %v", err)
	}
	if _, err := ApplyManaFocus(-1, ManaFocusInput{}, OriginPlayer); !errors.Is(err, ErrInvalidDamageValue) {
		t.Fatalf("negative damage err = %v", err)
	}
	if _, err := ApplyManaFocus(9, ManaFocusInput{}, CastOrigin(7)); !errors.Is(err, ErrInvalidCastOrigin) {
		t.Fatalf("bad origin err = %v", err)
	}
}

func TestAttackSpellOriginMatrix(t *testing.T) {
	// Identical raw roll + power MUST diverge by origin (composition
	// test over production paths, not implementation-derived outputs).
	// Scripted RNG: force the max roll 12 of Fireball-like 8..12.
	mkInput := func(o CastOrigin) AttackSpellInput {
		return AttackSpellInput{
			Min: 8, Max: 12, Power: 50, Origin: o,
			Focus:     ManaFocusInput{Active: true, Power: 99, Bonus: 5},
			Policy:    PolicyOrdinary,
			Signature: DamageSignature{Spell: 0x0001 | 0x0002},
		}
	}
	player, err := RollAttackSpellDamage(&scriptRNG{vals: []uint64{4}}, mkInput(OriginPlayer))
	if err != nil {
		t.Fatalf("player: %v", err)
	}
	// Roll: 8 + 4%5 = 12 (max). Scaled: 12*75/99 = 9. Focus: +6.
	if player.RawRoll != 12 || player.Damage != 15 {
		t.Fatalf("player = %+v, want raw 12 damage 15", player)
	}
	item, err := RollAttackSpellDamage(&scriptRNG{vals: []uint64{4}}, mkInput(OriginItem))
	if err != nil {
		t.Fatalf("item: %v", err)
	}
	if item.RawRoll != 12 || item.Damage != 12 {
		t.Fatalf("item = %+v, want raw 12 damage 12 (no scale/focus)", item)
	}
	monster, err := RollAttackSpellDamage(&scriptRNG{vals: []uint64{4}}, mkInput(OriginMonster))
	if err != nil {
		t.Fatalf("monster: %v", err)
	}
	if monster.RawRoll != 12 || monster.Damage != 12 {
		t.Fatalf("monster = %+v, want raw 12 damage 12 (no scale/focus)", monster)
	}
	// Forced min roll 8: player 8*75/99 = 600/99 = 6 (+6 focus = 12).
	playerMin, err := RollAttackSpellDamage(&scriptRNG{vals: []uint64{0}}, mkInput(OriginPlayer))
	if err != nil {
		t.Fatalf("player min: %v", err)
	}
	if playerMin.RawRoll != 8 || playerMin.Damage != 12 {
		t.Fatalf("player min = %+v, want raw 8 damage 12", playerMin)
	}
}

func TestRollAttackSpellDamageEndpoints(t *testing.T) {
	// Inclusive [min, max]: both endpoints reachable, no catalog entry.
	in := AttackSpellInput{Min: 8, Max: 12, Power: 99, Origin: OriginMonster, Policy: PolicyOrdinary}
	lo, err := RollAttackSpellDamage(&scriptRNG{vals: []uint64{0}}, in)
	if err != nil || lo.RawRoll != 8 || lo.Damage != 8 {
		t.Fatalf("min endpoint = %+v,%v, want raw/damage 8", lo, err)
	}
	hi, err := RollAttackSpellDamage(&scriptRNG{vals: []uint64{4}}, in)
	if err != nil || hi.RawRoll != 12 || hi.Damage != 12 {
		t.Fatalf("max endpoint = %+v,%v, want raw/damage 12", hi, err)
	}
	// Policy + signature + origin echo for the future runtime.
	if hi.Policy != PolicyOrdinary || hi.Origin != OriginMonster {
		t.Fatalf("metadata echo = %+v", hi)
	}
	abs, err := RollAttackSpellDamage(&scriptRNG{vals: []uint64{4}}, AttackSpellInput{
		Min: 8, Max: 12, Power: 99, Origin: OriginMonster, Policy: PolicyAbsolute,
	})
	if err != nil || abs.Policy != PolicyAbsolute || abs.Damage != 12 {
		t.Fatalf("absolute = %+v,%v", abs, err)
	}
	if _, err := RollAttackSpellDamage(nil, in); !errors.Is(err, ErrNilRNG) {
		t.Fatalf("nil rng err = %v", err)
	}
	badRange := in
	badRange.Min, badRange.Max = 12, 8
	if _, err := RollAttackSpellDamage(&scriptRNG{}, badRange); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("min>max err = %v", err)
	}
	negBounds := in
	negBounds.Min = -1
	if _, err := RollAttackSpellDamage(&scriptRNG{}, negBounds); !errors.Is(err, ErrInvalidDamageValue) {
		t.Fatalf("negative min err = %v", err)
	}
	badOrigin := in
	badOrigin.Origin = CastOrigin(9)
	if _, err := RollAttackSpellDamage(&scriptRNG{}, badOrigin); !errors.Is(err, ErrInvalidCastOrigin) {
		t.Fatalf("bad origin err = %v", err)
	}
	badPolicy := in
	badPolicy.Policy = DamagePolicy(9)
	if _, err := RollAttackSpellDamage(&scriptRNG{}, badPolicy); !errors.Is(err, ErrInvalidDamagePolicy) {
		t.Fatalf("bad policy err = %v", err)
	}
}

func TestAttackSpellT2Composition(t *testing.T) {
	// Real cross-stage proof (TEST ONLY composition; T3a production
	// never calls T1 caps): generic AttackSpell raw damage takes the
	// T2 pure-spell defense stage (NO armor DamageReduce) and DOES
	// take T2 resistance.
	raw, err := RollAttackSpellDamage(&scriptRNG{vals: []uint64{4}}, AttackSpellInput{
		Min: 8, Max: 12, Power: 99, Origin: OriginMonster, Policy: PolicyOrdinary,
		Signature: DamageSignature{Spell: 0x0001 | 0x0002}, // ALL+FIRE, fireball-style
	})
	if err != nil || raw.Damage != 12 {
		t.Fatalf("raw = %+v,%v, want 12", raw, err)
	}
	class := ClassifyDamageClass(0, raw.Signature.Spell)
	if class != DamageClassSpell {
		t.Fatalf("class = %v, want pure spell", class)
	}
	// Plate-like armor (DamageReduce 6) with max roll must NOT reduce
	// pure-spell damage.
	mit, err := ApplyDefenseModifiers(&scriptRNG{vals: []uint64{0xFFFFFFFF}}, raw.Damage, class,
		[]DefenseModifier{{DamageReduce: 6}}, false)
	if err != nil {
		t.Fatalf("defense stage: %v", err)
	}
	if mit.FinalDamage != 12 {
		t.Fatalf("pure spell through armor = %d, want 12 (bypass)", mit.FinalDamage)
	}
	// Fire resistance +25 DOES apply: 12*75/100 = 9.
	eff := ResolveResistance([]ResistanceEntry{{IsSpell: true, Tag: ResistSpellFire, Value: 25}}, raw.Signature)
	if eff != 25 {
		t.Fatalf("effective resist = %d, want 25", eff)
	}
	resisted, err := ApplyResistance(mit.FinalDamage, eff)
	if err != nil || resisted != 9 {
		t.Fatalf("resisted = %d,%v, want 9,nil", resisted, err)
	}
}

func TestDamagePolicyAbsoluteBypass(t *testing.T) {
	// Policy value distinguishes ordinary (resisted) from absolute
	// (bypass) WITHOUT mutating HP. The bypass itself is a TEST ONLY
	// composition: absolute skips ApplyResistance; T3b/T7 own the
	// actual absolute formulas (no Illusionary Wounds here).
	sig := DamageSignature{Spell: 0x0001 | 0x0002}
	entries := []ResistanceEntry{{IsSpell: true, Tag: ResistSpellFire, Value: 50}}
	ordinary, err := RollAttackSpellDamage(&scriptRNG{vals: []uint64{4}}, AttackSpellInput{
		Min: 8, Max: 12, Power: 99, Origin: OriginMonster,
		Policy: PolicyOrdinary, Signature: sig,
	})
	if err != nil {
		t.Fatalf("ordinary: %v", err)
	}
	eff := ResolveResistance(entries, ordinary.Signature)
	afterResist, err := ApplyResistance(ordinary.Damage, eff)
	if err != nil {
		t.Fatalf("resist: %v", err)
	}
	if afterResist != 6 { // 12*50/100
		t.Fatalf("ordinary after +50 resist = %d, want 6", afterResist)
	}
	absolute, err := RollAttackSpellDamage(&scriptRNG{vals: []uint64{4}}, AttackSpellInput{
		Min: 8, Max: 12, Power: 99, Origin: OriginMonster,
		Policy: PolicyAbsolute, Signature: sig,
	})
	if err != nil {
		t.Fatalf("absolute: %v", err)
	}
	// Absolute bypasses the numeric resistance stage entirely.
	if absolute.Policy != PolicyAbsolute || absolute.Damage != 12 {
		t.Fatalf("absolute = %+v, want policy + damage 12", absolute)
	}
	if afterResist == absolute.Damage {
		t.Fatal("policy must distinguish ordinary from absolute")
	}
}

func TestAttackSpellDeterminism(t *testing.T) {
	in := AttackSpellInput{
		Min: 8, Max: 12, Power: 50, Origin: OriginPlayer,
		Focus:  ManaFocusInput{Active: true, Power: 60, Bonus: 5},
		Policy: PolicyOrdinary, Signature: DamageSignature{Spell: 0x0002},
	}
	mk := func() *scriptRNG { return &scriptRNG{vals: []uint64{3}} }
	a, errA := RollAttackSpellDamage(mk(), in)
	b, errB := RollAttackSpellDamage(mk(), in)
	if errA != nil || errB != nil || a != b {
		t.Fatalf("nondeterministic: %+v vs %+v", a, b)
	}
	// 8+3%5 = 11 raw; 11*75/99 = 825/99 = 8; focus (60*5)/99+1 = 3+1.
	if a.RawRoll != 11 || a.Damage != 12 {
		t.Fatalf("deterministic = %+v, want raw 11 damage 12", a)
	}
}

func TestAttackSpellPropertyInvariants(t *testing.T) {
	// Player scaling never amplifies (factor <= 1 over 1..99);
	// results stay in [Min, Max+focus]; identical inputs deterministic.
	for _, power := range []int{1, 2, 50, 98, 99} {
		for raw := 0; raw <= 30; raw++ {
			scaled, err := ScaleAttackSpellDamage(raw, power, OriginPlayer)
			if err != nil {
				t.Fatalf("raw %d power %d: %v", raw, power, err)
			}
			if scaled < 0 || scaled > raw {
				t.Fatalf("raw %d power %d scaled %d outside [0, raw]", raw, power, scaled)
			}
		}
	}
}

func FuzzAttackSpellDamage(f *testing.F) {
	f.Add(8, 12, 50, 1, 0, 0)
	f.Add(1, 30, 99, 1, 99, 5)
	f.Add(0, 0, 1, 2, 0, 0)
	f.Fuzz(func(t *testing.T, min, max, power, origin, focusPower, focusBonus int) {
		in := AttackSpellInput{
			Min: min, Max: max, Power: power, Origin: CastOrigin(origin),
			Focus:  ManaFocusInput{Active: true, Power: focusPower, Bonus: focusBonus},
			Policy: PolicyOrdinary,
		}
		got, err := RollAttackSpellDamage(&scriptRNG{vals: []uint64{7}}, in)
		switch CastOrigin(origin) {
		case OriginPlayer, OriginItem, OriginMonster:
		default:
			if !errors.Is(err, ErrInvalidCastOrigin) {
				t.Fatalf("expected ErrInvalidCastOrigin, got %+v,%v", got, err)
			}
			return
		}
		if min < 0 || max < 0 {
			if !errors.Is(err, ErrInvalidDamageValue) {
				t.Fatalf("expected ErrInvalidDamageValue, got %+v,%v", got, err)
			}
			return
		}
		if min > max {
			if !errors.Is(err, ErrInvalidRange) {
				t.Fatalf("expected ErrInvalidRange, got %+v,%v", got, err)
			}
			return
		}
		if CastOrigin(origin) == OriginPlayer {
			if power < 1 || power > 99 {
				if !errors.Is(err, ErrInvalidSpellPower) {
					t.Fatalf("expected ErrInvalidSpellPower, got %+v,%v", got, err)
				}
				return
			}
			if focusPower < 0 || focusBonus < 0 {
				if !errors.Is(err, ErrInvalidCombatStat) {
					t.Fatalf("expected ErrInvalidCombatStat, got %+v,%v", got, err)
				}
				return
			}
		}
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got.RawRoll < min || got.RawRoll > max {
			t.Fatalf("raw %d outside [%d, %d]", got.RawRoll, min, max)
		}
		if got.Damage < 0 {
			t.Fatalf("negative damage %d", got.Damage)
		}
	})
}
