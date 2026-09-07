package sim

import (
	"errors"
	"testing"
)

// forceBounded returns a scriptRNG drawing want from an inclusive
// [min, max] rollBounded draw: want = min + v%(max-min+1).
func forceBounded(want, min, max int) *scriptRNG {
	span := max - min + 1
	return &scriptRNG{vals: []uint64{uint64(want-min+span*100) % uint64(span)}}
}

func TestTouchProficiencyGolden(t *testing.T) {
	// max(Punch, (Myst*3)/2) with *3 before /2. Source touchatk.kod GetProf.
	cases := []struct {
		punch, myst, want int
		name              string
	}{
		{60, 30, 60, "punch wins"},
		{30, 40, 60, "mysticism wins: (40*3)/2"},
		{45, 30, 45, "equality: (30*3)/2 = 45"},
		{0, 1, 1, "minimal mysticism: 3/2 = 1"},
		{31, 31, 46, "odd mysticism truncation: 93/2 = 46, not 31"},
		{0, 0, 0, "zero inputs"},
		{99, 70, 105, "high mysticism: (70*3)/2 = 105"},
	}
	for _, c := range cases {
		got, err := TouchProficiency(c.punch, c.myst)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Fatalf("%s: punch %d myst %d = %d, want %d",
				c.name, c.punch, c.myst, got, c.want)
		}
	}
	if _, err := TouchProficiency(-1, 30); !errors.Is(err, ErrInvalidCombatStat) {
		t.Fatalf("negative punch err = %v", err)
	}
	if _, err := TouchProficiency(30, -1); !errors.Is(err, ErrInvalidCombatStat) {
		t.Fatalf("negative myst err = %v", err)
	}
}

func TestTouchDamageGolden(t *testing.T) {
	// half = r/2 BEFORE power multiply; damage = half+(half*power)/99+1.
	// Source touchatk.kod FindDamage.
	cases := []struct {
		min, max, power, force, want int
		name                         string
	}{
		{3, 6, 1, 3, 2, "min roll low power: 1+0+1"},
		{3, 6, 50, 3, 2, "min roll mid power: 1+50/99=0+1"},
		{3, 6, 99, 3, 3, "min roll max power: 1+1+1"},
		{3, 6, 1, 6, 4, "max roll low power: 3+0+1"},
		{3, 6, 50, 6, 5, "max roll mid power: 3+150/99=1+1"},
		{3, 6, 99, 6, 7, "max roll max power: 3+297/99=3+1"},
		{4, 9, 1, 4, 3, "flame-like min: 2+0+1"},
		{4, 9, 99, 9, 9, "flame-like max: 4+396/99=4+1"},
		// Odd-roll proofs: half-before-multiply cannot be replaced by
		// raw-first arithmetic (5*99/99/2+1 would give 3, not 5).
		{3, 6, 99, 5, 5, "odd roll: half=2, 2+198/99=2+1"},
		{3, 6, 50, 5, 4, "odd roll mid: half=2, 2+100/99=1+1"},
	}
	for _, c := range cases {
		got, err := RollTouchDamage(forceBounded(c.force, c.min, c.max), c.min, c.max, c.power)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Fatalf("%s: roll %d power %d = %d, want %d",
				c.name, c.force, c.power, got, c.want)
		}
	}
	if _, err := RollTouchDamage(nil, 3, 6, 50); !errors.Is(err, ErrNilRNG) {
		t.Fatalf("nil rng err = %v", err)
	}
	if _, err := RollTouchDamage(&scriptRNG{}, 6, 3, 50); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("min>max err = %v", err)
	}
	if _, err := RollTouchDamage(&scriptRNG{}, -1, 6, 50); !errors.Is(err, ErrInvalidDamageValue) {
		t.Fatalf("negative min err = %v", err)
	}
	if _, err := RollTouchDamage(&scriptRNG{}, 3, 6, 0); !errors.Is(err, ErrInvalidSpellPower) {
		t.Fatalf("power 0 err = %v", err)
	}
}

func TestTouchRealT1Composition(t *testing.T) {
	// Mandatory B4 composition over REAL T1 functions, hand-computed.
	// Stroke(spell ability) 40, Punch 30, Mysticism 40, Aim 25, BaseMaxHP 40.
	prof, err := TouchProficiency(30, 40) // max(30, 60) = 60
	if err != nil || prof != 60 {
		t.Fatalf("proficiency = %d,%v, want 60,nil", prof, err)
	}
	off, err := PlayerOffense(PlayerOffenseInput{
		Stroke: 40, Proficiency: prof, Aim: 25, BaseMaxHP: 40,
	})
	// 40*3 + 60*2 + 25*4 + (40*3)/2 = 120+120+100+60 = 400.
	if err != nil || off != 400 {
		t.Fatalf("offense = %d,%v, want 400,nil", off, err)
	}
	def, err := PlayerDefense(PlayerDefenseInput{
		Parry: 30, Block: 0, Dodge: 20, Agility: 25, BaseMaxHP: 40,
	})
	// 30*2 + 0 + 20*3 + 25*4 + 60 = 60+0+60+100+60 = 280.
	if err != nil || def != 280 {
		t.Fatalf("defense = %d,%v, want 280,nil", def, err)
	}
	// chance = 400*55/280 = 22000/280 = 78 (truncated).
	hit, err := RollHit(forceRoll(78), off, def)
	if err != nil {
		t.Fatalf("hit: %v", err)
	}
	if hit.Chance != 78 || hit.Roll != 78 || !hit.Landed {
		t.Fatalf("hit = %+v, want chance/roll 78 landed", hit)
	}
	miss, err := RollHit(forceRoll(79), off, def)
	if err != nil || miss.Landed {
		t.Fatalf("roll 79 = %+v,%v, want miss", miss, err)
	}
	// A viHit_Factor-style +80 bonus would give offense 480 and chance
	// 480*55/280 = 94: the golden 78 fails if it is ever added.
	if hit.Chance == 94 {
		t.Fatal("hit-factor bonus leaked into offense")
	}
}

func TestTouchRealT2MixedComposition(t *testing.T) {
	// Mandatory B6: both weapon AND spell signature nonzero -> the
	// existing T2 mixed 2/3 armor rule applies. T3b duplicates nothing.
	sig := DamageSignature{Weapon: 0x2000 | 0x4000, Spell: 0x0001 | 0x0004} // unarmed+punch, ALL+SHOCK
	if ClassifyDamageClass(sig.Weapon, sig.Spell) != DamageClassWeaponSpell {
		t.Fatal("touch signature must classify mixed weapon+spell")
	}
	// Armor r=6 forced max roll 6: capped damage-1, then mixed (6*2)/3 = 4.
	mit, err := ApplyDefenseModifiers(&scriptRNG{vals: []uint64{4}}, 12, DamageClassWeaponSpell,
		[]DefenseModifier{{DamageReduce: 6}}, false)
	if err != nil {
		t.Fatalf("defense stage: %v", err)
	}
	if mit.FinalDamage != 8 { // 12-4
		t.Fatalf("mixed mitigation = %d, want 8 (2/3 of max roll 6)", mit.FinalDamage)
	}
	// Resistance still applies: SHOCK +25 -> 8*75/100 = 6.
	eff := ResolveResistance([]ResistanceEntry{{IsSpell: true, Tag: ResistSpellShock, Value: 25}}, sig)
	if eff != 25 {
		t.Fatalf("effective = %d, want 25", eff)
	}
	final, err := ApplyResistance(mit.FinalDamage, eff)
	if err != nil || final != 6 {
		t.Fatalf("resisted = %d,%v, want 6,nil", final, err)
	}
}

func TestTouchDurationGolden(t *testing.T) {
	// Random(power/3,power/2) bound(10,75), then *6*1000.
	ms, err := TouchDurationMs(&scriptRNG{}, 1) // [0,0] -> 0 -> bound 10
	if err != nil || ms != 60000 {
		t.Fatalf("power 1 = %d,%v, want 60000", ms, err)
	}
	ms, err = TouchDurationMs(forceBounded(49, 33, 49), 99) // max endpoint
	if err != nil || ms != 49*6000 {
		t.Fatalf("power 99 max = %d,%v, want %d", ms, err, 49*6000)
	}
	ms, err = TouchDurationMs(forceBounded(33, 33, 49), 99) // min endpoint
	if err != nil || ms != 33*6000 {
		t.Fatalf("power 99 min = %d,%v, want %d", ms, err, 33*6000)
	}
	ms, err = TouchDurationMs(forceBounded(30, 20, 30), 60)
	if err != nil || ms != 180000 {
		t.Fatalf("power 60 max = %d,%v, want 180000", ms, err)
	}
	if _, err := TouchDurationMs(nil, 50); !errors.Is(err, ErrNilRNG) {
		t.Fatalf("nil rng err = %v", err)
	}
	if _, err := TouchDurationMs(&scriptRNG{}, 0); !errors.Is(err, ErrInvalidSpellPower) {
		t.Fatalf("power 0 err = %v", err)
	}
}

func TestHolyTouchModifierGolden(t *testing.T) {
	// Source holytch.kod DamageFactors over resolved inputs.
	got, err := ApplyHolyTouchModifier(10, 0, true)
	if err != nil || got != 20 {
		t.Fatalf("undead = %d,%v, want 20,nil", got, err)
	}
	// Undead skips the karma path even for extreme karma.
	got, err = ApplyHolyTouchModifier(10, 100, true)
	if err != nil || got != 20 {
		t.Fatalf("undead extreme karma = %d,%v, want 20,nil", got, err)
	}
	cases := []struct {
		damage, karma, want int
		name                string
	}{
		{10, -100, 15, "negative karma increases: 10+1000/200"},
		{10, 0, 10, "zero karma unchanged"},
		{10, 100, 5, "positive karma decreases: 10-1000/200"},
		{201, -1, 202, "truncation: 201/200 = 1"},
		{199, -1, 199, "truncation: 199/200 = 0"},
		{10, 3, 10, "C truncation toward zero: -30/200 = 0, not -1"},
		{0, -100, 0, "zero damage stays zero"},
	}
	for _, c := range cases {
		got, err := ApplyHolyTouchModifier(c.damage, c.karma, false)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Fatalf("%s: damage %d karma %d = %d, want %d",
				c.name, c.damage, c.karma, got, c.want)
		}
	}
	if _, err := ApplyHolyTouchModifier(-1, 0, false); !errors.Is(err, ErrInvalidDamageValue) {
		t.Fatalf("negative damage err = %v", err)
	}
	// Overflow-safe signed arithmetic: extreme karma cannot panic,
	// flip sign, or wrap.
	got, err = ApplyHolyTouchModifier(10, -1<<62, false)
	if err != nil || got < 10 {
		t.Fatalf("extreme negative karma = %d,%v, want large positive", got, err)
	}
	got, err = ApplyHolyTouchModifier(10, 1<<62, false)
	if err != nil {
		t.Fatalf("extreme positive karma: %v", err)
	}
	_ = got
}

func TestIllusionaryWoundsPlayerGolden(t *testing.T) {
	// base = 17+(50-Int)/10; loss = base*power/100; caps MaxHP/3, HP-1.
	player := func(intellect, maxHP, hp int) IllusionaryVictim {
		return IllusionaryVictim{Kind: VictimPlayer, Intellect: intellect, MaxHP: maxHP, HP: hp}
	}
	cases := []struct {
		victim IllusionaryVictim
		power  int
		want   int
		name   string
	}{
		{player(50, 40, 40), 50, 8, "int 50 power 50: 17*50/100"},
		{player(10, 40, 40), 50, 10, "low int: 21*50/100 = 1050/100"},
		{player(50, 40, 40), 1, 0, "power 1: 17/100 = 0"},
		{player(10, 60, 60), 99, 20, "MaxHP/3 boundary: 21*99/100 = 20, cap 20"},
		{player(10, 90, 10), 99, 9, "HP-1 tighter than MaxHP/3"},
		{player(10, 90, 1), 99, 0, "HP=1 -> loss 0"},
		{player(1, 100, 100), 99, 20, "int 1: 21*99/100 = 20"},
		{player(70, 100, 100), 99, 14, "int 70: 15*99/100 = 1485/100"},
	}
	for _, c := range cases {
		got, err := IllusionaryWoundsLoss(c.victim, c.power)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Fatalf("%s = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestIllusionaryWoundsMonsterGolden(t *testing.T) {
	// base = 30-bound(diff*2,1,20); loss = base*power/100.
	monster := func(diff, maxHP, hp int) IllusionaryVictim {
		return IllusionaryVictim{Kind: VictimMonster, Difficulty: diff, MaxHP: maxHP, HP: hp}
	}
	cases := []struct {
		victim IllusionaryVictim
		power  int
		want   int
		name   string
	}{
		{monster(4, 60, 60), 50, 11, "diff 4: 22*50/100"},
		{monster(1, 100, 100), 99, 27, "low diff: 28*99/100 = 2772/100"},
		{monster(20, 100, 100), 50, 5, "high diff: 10*50/100"},
		{monster(100, 100, 100), 50, 5, "clipped diff: bound(200,1,20) = 20"},
		{monster(6, 30, 5), 99, 4, "HP-1 cap: base 18*99/100 = 17 -> 4"},
	}
	for _, c := range cases {
		got, err := IllusionaryWoundsLoss(c.victim, c.power)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Fatalf("%s = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestIllusionaryWoundsVictimErrors(t *testing.T) {
	okPlayer := IllusionaryVictim{Kind: VictimPlayer, Intellect: 25, MaxHP: 40, HP: 40}
	if _, err := IllusionaryWoundsLoss(okPlayer, 0); !errors.Is(err, ErrInvalidSpellPower) {
		t.Fatalf("power 0 err = %v", err)
	}
	bad := okPlayer
	bad.Kind = IllusionaryVictimKind(0)
	if _, err := IllusionaryWoundsLoss(bad, 50); !errors.Is(err, ErrInvalidSpecialVictim) {
		t.Fatalf("kind 0 err = %v", err)
	}
	for _, mutate := range []func(*IllusionaryVictim){
		func(v *IllusionaryVictim) { v.Intellect = 0 },
		func(v *IllusionaryVictim) { v.Intellect = 71 },
		func(v *IllusionaryVictim) { v.MaxHP = 0 },
		func(v *IllusionaryVictim) { v.HP = 0 },
	} {
		v := okPlayer
		mutate(&v)
		if _, err := IllusionaryWoundsLoss(v, 50); !errors.Is(err, ErrInvalidSpecialVictim) {
			t.Fatalf("bad player snapshot %+v err = %v, want ErrInvalidSpecialVictim", v, err)
		}
	}
	badMonster := IllusionaryVictim{Kind: VictimMonster, Difficulty: 0, MaxHP: 40, HP: 40}
	if _, err := IllusionaryWoundsLoss(badMonster, 50); !errors.Is(err, ErrInvalidSpecialVictim) {
		t.Fatalf("difficulty 0 err = %v", err)
	}
}

func TestIllusionaryWoundsDurationRefund(t *testing.T) {
	// durationMs = 20000+power*750 bound(20000,80000).
	cases := []struct {
		power, want int
	}{
		{1, 20750},
		{50, 57500},
		{80, 80000}, // exact cap edge: 20000+60000
		{81, 80000}, // 80750 -> capped
		{99, 80000}, // 94250 -> capped
	}
	for _, c := range cases {
		got, err := IllusionaryWoundsDuration(c.power)
		if err != nil || got != c.want {
			t.Fatalf("power %d = %d,%v, want %d,nil", c.power, got, err, c.want)
		}
	}
	if _, err := IllusionaryWoundsDuration(0); !errors.Is(err, ErrInvalidSpellPower) {
		t.Fatalf("power 0 err = %v", err)
	}
	// Refund: applied amount iff alive, else 0. Never recomputed.
	got, err := IllusionaryRefund(9, true)
	if err != nil || got != 9 {
		t.Fatalf("alive refund = %d,%v, want 9,nil", got, err)
	}
	got, err = IllusionaryRefund(9, false)
	if err != nil || got != 0 {
		t.Fatalf("dead refund = %d,%v, want 0,nil", got, err)
	}
	got, err = IllusionaryRefund(0, true)
	if err != nil || got != 0 {
		t.Fatalf("zero refund = %d,%v, want 0,nil", got, err)
	}
	if _, err := IllusionaryRefund(-1, true); !errors.Is(err, ErrInvalidDamageValue) {
		t.Fatalf("negative applied err = %v", err)
	}
	// Result composes loss + duration + absolute policy; no damage applied.
	res, err := RollIllusionaryWounds(
		IllusionaryVictim{Kind: VictimPlayer, Intellect: 50, MaxHP: 40, HP: 40}, 50)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if res.Loss != 8 || res.DurationMs != 57500 || res.Policy != PolicyAbsolute {
		t.Fatalf("result = %+v, want loss 8 duration 57500 absolute", res)
	}
}

func TestVampiricDrainHealGolden(t *testing.T) {
	// Nonlethal: bound(applied/2,1,$). Source vampdrn.kod DoSideEffect.
	cases := []struct {
		applied, want int
	}{
		{0, 1},
		{1, 1},
		{2, 1},
		{3, 1},
		{4, 2},
		{17, 8},
		{100, 50},
	}
	for _, c := range cases {
		got, err := VampiricDrainHeal(c.applied, false, 18)
		if err != nil || got != c.want {
			t.Fatalf("applied %d = %d,%v, want %d,nil", c.applied, got, err, c.want)
		}
	}
	// Lethal ignores the applied scalar: prototype max 18 -> 9.
	for _, applied := range []int{0, 1, 12, 999} {
		got, err := VampiricDrainHeal(applied, true, 18)
		if err != nil || got != 9 {
			t.Fatalf("lethal applied %d = %d,%v, want 9,nil", applied, got, err)
		}
	}
	if _, err := VampiricDrainHeal(-1, false, 18); !errors.Is(err, ErrInvalidDamageValue) {
		t.Fatalf("negative applied err = %v", err)
	}
	if _, err := VampiricDrainHeal(5, false, 0); !errors.Is(err, ErrInvalidDamageValue) {
		t.Fatalf("zero prototype max err = %v", err)
	}
	if _, err := VampiricDrainHeal(5, true, -3); !errors.Is(err, ErrInvalidDamageValue) {
		t.Fatalf("negative prototype max err = %v", err)
	}
}

func TestVampCrossStageOrdering(t *testing.T) {
	// B12: T3a raw -> T2 resistance -> test-only applied -> T3b heal.
	// The helper MUST receive the post-application value.
	raw, err := RollAttackSpellDamage(forceBounded(12, 8, 12), AttackSpellInput{
		Min: 8, Max: 12, Power: 99, Origin: OriginMonster,
		Policy: PolicyOrdinary, Signature: DamageSignature{Spell: 0x0001 | 0x0002},
	})
	if err != nil || raw.Damage != 12 {
		t.Fatalf("raw = %+v,%v, want 12", raw, err)
	}
	eff := ResolveResistance([]ResistanceEntry{{IsSpell: true, Tag: ResistSpellFire, Value: 50}}, raw.Signature)
	applied, err := ApplyResistance(raw.Damage, eff) // 12*50/100 = 6
	if err != nil || applied != 6 {
		t.Fatalf("applied = %d,%v, want 6", applied, err)
	}
	heal, err := VampiricDrainHeal(applied, false, 18)
	if err != nil || heal != 3 {
		t.Fatalf("heal = %d,%v, want 3", heal, err)
	}
	// Raw-direct control differs: feeding raw 12 would heal 6.
	wrong, _ := VampiricDrainHeal(raw.Damage, false, 18)
	if wrong == heal {
		t.Fatal("raw-direct control must differ from post-application heal")
	}
}

func TestEarthquakeSeverityGolden(t *testing.T) {
	// 1+power/25. Source earthqua.kod CastSpell.
	cases := []struct {
		power, want int
	}{
		{1, 1}, {24, 1}, {25, 2}, {49, 2}, {50, 3}, {74, 3}, {75, 4}, {99, 4},
	}
	for _, c := range cases {
		got, err := EarthquakeSeverity(c.power)
		if err != nil || got != c.want {
			t.Fatalf("power %d = %d,%v, want %d,nil", c.power, got, err, c.want)
		}
	}
	if _, err := EarthquakeSeverity(0); !errors.Is(err, ErrInvalidSpellPower) {
		t.Fatalf("power 0 err = %v", err)
	}
}

func TestEarthquakeDamagePercentGolden(t *testing.T) {
	// Squared-distance rule: <=64 -> 100; >400 -> 0;
	// else 100*(400-sq)/336 truncated. Source ComputeDamage.
	// Expected values computed by hand here (independent oracle for B14).
	cases := []struct {
		sq, want int
		name     string
	}{
		{0, 100, "caster self"},
		{64, 100, "full boundary"},
		{65, 99, "just past full: 100*335/336 = 33500/336"},
		{100, 89, "linear-distance killer: 100*300/336 = 30000/336 (linear-10 would give 83)"},
		{232, 50, "mid literal: 100*168/336 = 50"},
		{399, 0, "near zero: 100*1/336 = 0"},
		{400, 0, "zero boundary via interpolation"},
		{401, 0, "past zero"},
		{1000000, 0, "far past zero"},
	}
	for _, c := range cases {
		got, err := EarthquakeDamagePercent(c.sq)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Fatalf("%s: sq %d = %d, want %d", c.name, c.sq, got, c.want)
		}
	}
	if _, err := EarthquakeDamagePercent(-1); !errors.Is(err, ErrInvalidSquaredDistance) {
		t.Fatalf("negative sq err = %v", err)
	}
}

func TestEarthquakeDamageModesGolden(t *testing.T) {
	// Caster mode: (roll*severity)*percent/100. Source ComputeDamage.
	got, err := RollEarthquakeDamage(forceBounded(5, 5, 9), 2, 50) // (5*2)*50/100
	if err != nil || got != 5 {
		t.Fatalf("forced 5 sev 2 pct 50 = %d,%v, want 5", got, err)
	}
	got, err = RollEarthquakeDamage(forceBounded(9, 5, 9), 2, 50) // (9*2)*50/100
	if err != nil || got != 9 {
		t.Fatalf("forced 9 sev 2 pct 50 = %d,%v, want 9", got, err)
	}
	// Zero percent yields zero: no floor-1 in T3b.
	got, err = RollEarthquakeDamage(forceBounded(9, 5, 9), 3, 0)
	if err != nil || got != 0 {
		t.Fatalf("zero percent = %d,%v, want 0", got, err)
	}
	// Normal self at sq=0 (percent 100): (5*2)*100/100 = 10.
	pct, _ := EarthquakeDamagePercent(0)
	got, err = RollEarthquakeDamage(forceBounded(5, 5, 9), 2, pct)
	if err != nil || got != 10 {
		t.Fatalf("normal self = %d,%v, want 10", got, err)
	}
	// Environmental: roll*severity, no percent.
	got, err = RollEnvironmentalEarthquakeDamage(forceBounded(9, 5, 9), 3)
	if err != nil || got != 27 {
		t.Fatalf("environmental = %d,%v, want 27", got, err)
	}
	// Item self: 9*severity, no RNG.
	got, err = EarthquakeItemSelfDamage(2)
	if err != nil || got != 18 {
		t.Fatalf("item self sev 2 = %d,%v, want 18", got, err)
	}
	got, err = EarthquakeItemSelfDamage(4)
	if err != nil || got != 36 {
		t.Fatalf("item self sev 4 = %d,%v, want 36", got, err)
	}
	if _, err := RollEarthquakeDamage(&scriptRNG{}, 0, 50); !errors.Is(err, ErrInvalidEarthquakeSeverity) {
		t.Fatalf("severity 0 err = %v", err)
	}
	if _, err := RollEarthquakeDamage(&scriptRNG{}, 2, 101); !errors.Is(err, ErrInvalidEarthquakeSeverity) {
		t.Fatalf("percent 101 err = %v", err)
	}
	if _, err := RollEarthquakeDamage(nil, 2, 50); !errors.Is(err, ErrNilRNG) {
		t.Fatalf("nil rng err = %v", err)
	}
	if _, err := RollEnvironmentalEarthquakeDamage(&scriptRNG{}, 0); !errors.Is(err, ErrInvalidEarthquakeSeverity) {
		t.Fatalf("env severity 0 err = %v", err)
	}
	if _, err := EarthquakeItemSelfDamage(0); !errors.Is(err, ErrInvalidEarthquakeSeverity) {
		t.Fatalf("item severity 0 err = %v", err)
	}
}

func TestEarthquakeRealT2Composition(t *testing.T) {
	// B15: quake raw -> T2 pure-spell class -> quake resistance.
	// Armor bypassed (no weapon domain); resistance applies; no T3b cap.
	sig := EarthquakeSignature()
	if sig.Weapon != 0 {
		t.Fatalf("quake weapon domain = %x, want none", sig.Weapon)
	}
	if ClassifyDamageClass(sig.Weapon, sig.Spell) != DamageClassSpell {
		t.Fatal("quake must classify pure spell")
	}
	mit, err := ApplyDefenseModifiers(&scriptRNG{vals: []uint64{4}}, 10, DamageClassSpell,
		[]DefenseModifier{{DamageReduce: 6}}, false)
	if err != nil || mit.FinalDamage != 10 {
		t.Fatalf("quake through armor = %+v,%v, want 10 (bypass)", mit, err)
	}
	eff := ResolveResistance([]ResistanceEntry{{IsSpell: true, Tag: ResistSpellQuake, Value: 25}}, sig)
	if eff != 25 {
		t.Fatalf("quake resist = %d, want 25", eff)
	}
	final, err := ApplyResistance(mit.FinalDamage, eff) // 10*75/100 = 7
	if err != nil || final != 7 {
		t.Fatalf("resisted quake = %d,%v, want 7", final, err)
	}
}
