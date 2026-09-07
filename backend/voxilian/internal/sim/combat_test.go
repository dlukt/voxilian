package sim

import (
	"errors"
	"math"
	"testing"
)

func TestPlayerOffenseGolden(t *testing.T) {
	cases := []struct {
		name string
		in   PlayerOffenseInput
		want int
	}{
		// Mid player from the research notes: 50/50 stroke/prof,
		// Aim 40, BaseMaxHP 80 -> 150+100+160+120 = 530.
		{"mid-fighter", PlayerOffenseInput{Stroke: 50, Proficiency: 50, Aim: 40, BaseMaxHP: 80}, 530},
		// Odd BaseMaxHP truncation: (21*3)/2 = 63/2 = 31.
		{"odd-base-trunc", PlayerOffenseInput{BaseMaxHP: 21}, 31},
		// Even base: (20*3)/2 = 30.
		{"even-base", PlayerOffenseInput{BaseMaxHP: 20}, 30},
		// All zero still floors at 1 (bound, not raw 0).
		{"all-zero-floors", PlayerOffenseInput{}, 1},
		// Weapon + extra modifiers compose additively:
		// 0 + 125 (thrust) + 50 (high) + 10 = 185.
		{"mods-add", PlayerOffenseInput{WeaponHitMod: 175, ExtraMods: 10}, 185},
		// Upper clamp: 99*3+99*2+70*4+150*3/2 = 297+198+280+225 = 1000.
		{"upper-exact", PlayerOffenseInput{Stroke: 99, Proficiency: 99, Aim: 70, BaseMaxHP: 150}, 1000},
		// Over the cap clamps to 1000.
		{"upper-clamp", PlayerOffenseInput{Stroke: 99, Proficiency: 99, Aim: 70, BaseMaxHP: 150, ExtraMods: 500}, 1000},
		// Negative trusted mods can pull down but never below 1.
		{"negative-mods-floor", PlayerOffenseInput{BaseMaxHP: 20, ExtraMods: -1000}, 1},
	}
	for _, c := range cases {
		got, err := PlayerOffense(c.in)
		if err != nil || got != c.want {
			t.Fatalf("%s = %d,%v, want %d,nil", c.name, got, err, c.want)
		}
	}
	// Negative impossible stats fail explicitly.
	for _, bad := range []PlayerOffenseInput{
		{Stroke: -1}, {Proficiency: -1}, {Aim: -1}, {BaseMaxHP: -1},
	} {
		if _, err := PlayerOffense(bad); !errors.Is(err, ErrInvalidCombatStat) {
			t.Fatalf("bad %+v err = %v, want ErrInvalidCombatStat", bad, err)
		}
	}
}

func TestPlayerDefenseGolden(t *testing.T) {
	cases := []struct {
		name string
		in   PlayerDefenseInput
		want int
	}{
		// Parry 50/Block 30/Dodge 40/Agi 40/Base 80:
		// 100+30+120+160+120 = 530.
		{"mid-defender", PlayerDefenseInput{Parry: 50, Block: 30, Dodge: 40, Agility: 40, BaseMaxHP: 80}, 530},
		// Unarmed/unshielded zeroed inputs: 0+0+0+40*4+30 = 190.
		{"zeroed-components", PlayerDefenseInput{Agility: 40, BaseMaxHP: 20}, 190},
		// Odd base truncation: (21*3)/2 = 31.
		{"odd-base-trunc", PlayerDefenseInput{BaseMaxHP: 21}, 31},
		// All zero floors at 1.
		{"all-zero-floors", PlayerDefenseInput{}, 1},
		// Extra scalar terms compose (future T2 seam).
		{"extra-mods", PlayerDefenseInput{BaseMaxHP: 20, ExtraMods: 25}, 55},
		// Upper clamp.
		{"upper-clamp", PlayerDefenseInput{Parry: 99, Block: 99, Dodge: 99, Agility: 70, BaseMaxHP: 150, ExtraMods: 9999}, 1000},
	}
	for _, c := range cases {
		got, err := PlayerDefense(c.in)
		if err != nil || got != c.want {
			t.Fatalf("%s = %d,%v, want %d,nil", c.name, got, err, c.want)
		}
	}
	for _, bad := range []PlayerDefenseInput{
		{Parry: -1}, {Block: -1}, {Dodge: -1}, {Agility: -1}, {BaseMaxHP: -1},
	} {
		if _, err := PlayerDefense(bad); !errors.Is(err, ErrInvalidCombatStat) {
			t.Fatalf("bad %+v err = %v, want ErrInvalidCombatStat", bad, err)
		}
	}
}

func TestMonsterRatingGolden(t *testing.T) {
	// Frozen research vectors (spec §9.1.5).
	orc, err := MonsterRating(45, 6)
	if err != nil || orc != 495 {
		t.Fatalf("orc = %d,%v, want 495,nil", orc, err)
	}
	yeti, err := MonsterRating(170, 9)
	if err != nil || yeti != 1050 {
		t.Fatalf("yeti = %d,%v, want 1050,nil", yeti, err)
	}
	// Zero inputs floor at 1 via the bound.
	zero, err := MonsterRating(0, 0)
	if err != nil || zero != 1 {
		t.Fatalf("zero = %d,%v, want 1,nil", zero, err)
	}
	// Upper clamp.
	huge, err := MonsterRating(1000, 9)
	if err != nil || huge != 1500 {
		t.Fatalf("huge = %d,%v, want 1500,nil", huge, err)
	}
	// Negative impossible inputs fail.
	if _, err := MonsterRating(-1, 5); !errors.Is(err, ErrInvalidCombatRating) {
		t.Fatalf("neg level err = %v", err)
	}
	if _, err := MonsterRating(50, -2); !errors.Is(err, ErrInvalidCombatRating) {
		t.Fatalf("neg difficulty err = %v", err)
	}
}

func TestHitChanceGolden(t *testing.T) {
	cases := []struct {
		name     string
		off, def int
		want     int
	}{
		// Lower clamp: 1*55/1000 = 0 -> 10.
		{"lower-clamp", 1, 1000, 10},
		// Normal truncation: 430*55 = 23650; 23650/450 = 52 (52.57..).
		{"normal-trunc", 430, 450, 52},
		{"upper-clamp", 1000, 1, 95},
		// Exact: 20*55/22 = 1100/22 = 50.
		{"exact-div", 20, 22, 50},
		// Boundary: raw 9 -> 10; raw 96 -> 95.
		{"clamp-low-edge", 9, 55, 10},   // 495/55 = 9 -> 10
		{"clamp-high-edge", 96, 55, 95}, // 5280/55 = 96 -> 95
		// Equal ratings: 55 -> 55.
		{"equal", 300, 300, 55},
		// Monster-vs-player scale: 1050 vs 530 -> 57750/530 = 108 -> 95.
		{"yeti-vs-mid", 1050, 530, 95},
		// Orc vs mid: 495*55 = 27225; 27225/530 = 51 (51.36..).
		{"orc-vs-mid", 495, 530, 51},
	}
	for _, c := range cases {
		got, err := HitChance(c.off, c.def)
		if err != nil || got != c.want {
			t.Fatalf("%s = %d,%v, want %d,nil", c.name, got, err, c.want)
		}
	}
	// Invalid denominators/ratings: stable errors, never divide by zero.
	for _, bad := range [][2]int{{0, 100}, {100, 0}, {0, 0}, {-5, 100}, {100, -5}} {
		if _, err := HitChance(bad[0], bad[1]); !errors.Is(err, ErrInvalidCombatRating) {
			t.Fatalf("HitChance(%d,%d) err = %v, want ErrInvalidCombatRating", bad[0], bad[1], err)
		}
	}
}

func TestHitLandsBoundaries(t *testing.T) {
	// Frozen d100 threshold semantics (spec §9.1.6): hit iff chance >= roll.
	cases := []struct {
		chance, roll int
		want         bool
	}{
		{10, 1, true}, {10, 10, true}, {10, 11, false},
		{95, 95, true}, {95, 96, false}, {95, 100, false},
		{55, 55, true}, {55, 56, false}, {1, 1, true}, {100, 100, true},
	}
	for _, c := range cases {
		got, err := HitLands(c.chance, c.roll)
		if err != nil || got != c.want {
			t.Fatalf("HitLands(%d,%d) = %v,%v, want %v,nil", c.chance, c.roll, got, err, c.want)
		}
	}
	for _, roll := range []int{0, -1, 101, 1000} {
		if _, err := HitLands(50, roll); !errors.Is(err, ErrInvalidHitRoll) {
			t.Fatalf("roll %d err = %v, want ErrInvalidHitRoll", roll, err)
		}
	}
}

func TestRollD100Endpoints(t *testing.T) {
	// Scripted RNG forces the d100 endpoints and the exact hit
	// threshold through the real RollD100 + HitLands path.
	lo, err := RollD100(&scriptRNG{vals: []uint64{0}})
	if err != nil || lo != 1 {
		t.Fatalf("d100 lo = %d,%v, want 1,nil", lo, err)
	}
	hi, err := RollD100(&scriptRNG{vals: []uint64{99}})
	if err != nil || hi != 100 {
		t.Fatalf("d100 hi = %d,%v, want 100,nil", hi, err)
	}
	thr, err := RollD100(&scriptRNG{vals: []uint64{9}})
	if err != nil || thr != 10 {
		t.Fatalf("d100 threshold = %d,%v, want 10,nil", thr, err)
	}
	thrPlus, err := RollD100(&scriptRNG{vals: []uint64{10}})
	if err != nil || thrPlus != 11 {
		t.Fatalf("d100 threshold+1 = %d,%v, want 11,nil", thrPlus, err)
	}
	// Chance-10 scripted trace: roll 10 hits, roll 11 misses.
	r10, _ := RollHit(&scriptRNG{vals: []uint64{9}}, 10, 55) // chance = 550/55 = 10
	if !r10.Landed || r10.Chance != 10 || r10.Roll != 10 {
		t.Fatalf("trace hit = %+v, want {10 10 true}", r10)
	}
	r11, _ := RollHit(&scriptRNG{vals: []uint64{10}}, 10, 55)
	if r11.Landed || r11.Chance != 10 || r11.Roll != 11 {
		t.Fatalf("trace miss = %+v, want {10 11 false}", r11)
	}
	if _, err := RollD100(nil); !errors.Is(err, ErrNilRNG) {
		t.Fatalf("nil rng err = %v, want ErrNilRNG", err)
	}
	if _, err := RollHit(&scriptRNG{vals: []uint64{0}}, 0, 55); !errors.Is(err, ErrInvalidCombatRating) {
		t.Fatalf("bad offense err = %v", err)
	}
}

func TestDeterministicTrace(t *testing.T) {
	// Same scripted RNG + same inputs -> byte-identical combat traces.
	vals := []uint64{7, 42, 99, 3, 1000}
	run := func() []HitResolution {
		rng := &scriptRNG{vals: vals}
		var out []HitResolution
		for _, pair := range [][2]int{{430, 450}, {495, 530}, {1050, 530}, {1, 1000}} {
			res, err := RollHit(rng, pair[0], pair[1])
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, res)
		}
		return out
	}
	a, b := run(), run()
	if len(a) != len(b) {
		t.Fatal("trace length mismatch")
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("trace[%d] = %+v vs %+v", i, a[i], b[i])
		}
	}
}

func TestPlayerDamageCapsGolden(t *testing.T) {
	victim := func(hp, base int, outlaw, murderer bool) VictimSnapshot {
		return VictimSnapshot{HP: hp, BaseMaxHP: base, Outlaw: outlaw, Murderer: murderer}
	}
	cases := []struct {
		name string
		dmg  int
		vic  VictimSnapshot
		prot bool
		want int
	}{
		// One-third cap: HP 10 < 40, ceil(20/3) = 7; 30 -> 7.
		{"third-cap", 30, victim(10, 20, false, false), false, 7},
		// 30 cap alone: over-max HP (vampire rule) skips the third cap.
		{"overmax-skips-third", 100, victim(50, 20, false, false), false, 30},
		// Strict < boundary: HP == 2*Base is NOT below -> no third cap.
		{"strict-lt-boundary", 100, victim(40, 20, false, false), false, 30},
		// One below the boundary: HP 39 < 40 -> third cap binds.
		{"strict-lt-inside", 100, victim(39, 20, false, false), false, 7},
		// 30 cap: mid damage unaffected by third (limit 30, base 90).
		{"thirty-cap", 100, victim(10, 90, false, false), false, 30},
		// Both caps active, third binds below 30: base 60 -> limit 20.
		{"both-bind-third", 100, victim(10, 60, false, false), false, 20},
		// Outlaw exempt from third only: 100 -> 30 (never above 30).
		{"outlaw-exempt", 100, victim(10, 20, true, false), false, 30},
		// Murderer exempt from third only.
		{"murderer-exempt", 100, victim(10, 20, false, true), false, 30},
		// Protection setting restores the third cap for murderers.
		{"murderer-protected", 100, victim(10, 20, false, true), true, 7},
		// Small damage passes through: 5 -> 5.
		{"small-passthrough", 5, victim(10, 20, false, false), false, 5},
		// Minimum-one: 0 -> 1 (then third limit 7 keeps 1).
		{"minimum-one", 0, victim(10, 20, false, false), false, 1},
		// Ceil proof: base 22 -> (22+2)/3 = 8 (floor would be 7).
		{"ceil-proof", 100, victim(10, 22, false, false), false, 8},
		// Third limit above small damage never inflates: 3 stays 3.
		{"no-inflate", 3, victim(10, 90, false, false), false, 3},
	}
	for _, c := range cases {
		got, err := ApplyPlayerDamageCaps(c.dmg, c.vic, c.prot)
		if err != nil || got != c.want {
			t.Fatalf("%s = %d,%v, want %d,nil", c.name, got, err, c.want)
		}
	}
	// Invalid inputs fail explicitly.
	if _, err := ApplyPlayerDamageCaps(-1, victim(10, 20, false, false), false); !errors.Is(err, ErrInvalidDamageValue) {
		t.Fatalf("neg damage err = %v", err)
	}
	if _, err := ApplyPlayerDamageCaps(10, victim(10, 0, false, false), false); !errors.Is(err, ErrInvalidVictimSnapshot) {
		t.Fatalf("zero base err = %v", err)
	}
	if _, err := ApplyPlayerDamageCaps(10, victim(-1, 20, false, false), false); !errors.Is(err, ErrInvalidVictimSnapshot) {
		t.Fatalf("neg hp err = %v", err)
	}
}

func TestSeverityGolden(t *testing.T) {
	cases := []struct {
		name             string
		dmg              int
		killed, isPlayer bool
		maxHP            int
		want             Severity
	}{
		{"nick-1", 1, false, false, 0, SeverityNick},
		{"nick-5", 5, false, false, 0, SeverityNick},
		{"wound-6", 6, false, false, 0, SeverityWound},
		{"wound-15", 15, false, false, 0, SeverityWound},
		{"damage-16", 16, false, false, 0, SeverityDamage},
		{"damage-30", 30, false, false, 0, SeverityDamage},
		{"slay-lethal", 30, true, true, 90, SeveritySlay},
		{"slay-lethal-low", 1, true, false, 0, SeveritySlay},
		// Forced damage: 10 >= floor(30/3) = 10 (buffed Max, >=).
		{"forced-damage", 10, false, true, 30, SeverityDamage},
		// Just under the force line: 9 < 10 -> wound by thresholds.
		{"force-boundary-below", 9, false, true, 30, SeverityWound},
		// Force line uses floor division: maxHP 31 -> 31/3 = 10; 10 forces.
		{"force-floor-div", 10, false, true, 31, SeverityDamage},
		// Non-players never force: 10 stays wound.
		{"no-force-monster", 10, false, false, 0, SeverityWound},
		// Post-cap applied damage classifies: 7 -> wound.
		{"applied-7", 7, false, true, 90, SeverityWound},
	}
	for _, c := range cases {
		got, err := ClassifySeverity(c.dmg, c.killed, c.isPlayer, c.maxHP)
		if err != nil || got != c.want {
			t.Fatalf("%s = %v,%v, want %v,nil", c.name, got, err, c.want)
		}
	}
	// Invalid: non-lethal damage < 1; player victim with MaxHP < 1.
	if _, err := ClassifySeverity(0, false, false, 0); !errors.Is(err, ErrInvalidDamageValue) {
		t.Fatalf("zero dmg err = %v", err)
	}
	if _, err := ClassifySeverity(5, false, true, 0); !errors.Is(err, ErrInvalidDamageValue) {
		t.Fatalf("zero maxHP err = %v", err)
	}
}

func TestSwingCooldownGolden(t *testing.T) {
	// First swing always allowed (no history).
	ready, err := SwingReady(false, 0, 12345, 20)
	if err != nil || !ready {
		t.Fatalf("first = %v,%v, want true,nil", ready, err)
	}
	// One tick early at 20 Hz: elapsed 19 -> denied.
	ready, err = SwingReady(true, 100, 119, 20)
	if err != nil || ready {
		t.Fatalf("early = %v,%v, want false,nil", ready, err)
	}
	// Exact one-second boundary: elapsed 20 -> allowed.
	ready, err = SwingReady(true, 100, 120, 20)
	if err != nil || !ready {
		t.Fatalf("boundary = %v,%v, want true,nil", ready, err)
	}
	// Later: allowed.
	ready, err = SwingReady(true, 100, 500, 20)
	if err != nil || !ready {
		t.Fatalf("later = %v,%v, want true,nil", ready, err)
	}
	// Wrap: last = MaxUint32-9, now = 9 -> elapsed 19 -> denied at 20 Hz.
	ready, err = SwingReady(true, math.MaxUint32-9, 9, 20)
	if err != nil || ready {
		t.Fatalf("wrap-early = %v,%v, want false,nil", ready, err)
	}
	// Wrap: now = 10 -> elapsed 20 -> allowed.
	ready, err = SwingReady(true, math.MaxUint32-9, 10, 20)
	if err != nil || !ready {
		t.Fatalf("wrap-boundary = %v,%v, want true,nil", ready, err)
	}
	// 60 Hz / 120 Hz boundaries.
	ready, err = SwingReady(true, 0, 60, 60)
	if err != nil || !ready {
		t.Fatalf("hz60 = %v,%v, want true,nil", ready, err)
	}
	ready, err = SwingReady(true, 0, 119, 120)
	if err != nil || ready {
		t.Fatalf("hz120-early = %v,%v, want false,nil", ready, err)
	}
	// Invalid tickHz fails explicitly.
	for _, hz := range []int{0, -1, 121, 1000} {
		if _, err := SwingReady(true, 0, 100, hz); !errors.Is(err, ErrInvalidTickHz) {
			t.Fatalf("hz %d err = %v, want ErrInvalidTickHz", hz, err)
		}
	}
}

func TestSwingVigorGolden(t *testing.T) {
	if ExertionPerVigor != 10000 {
		t.Fatalf("ExertionPerVigor = %d, want 10000", ExertionPerVigor)
	}
	if StandardSwingExertionCost != 2000 {
		t.Fatalf("StandardSwingExertionCost = %d, want 2000", StandardSwingExertionCost)
	}
	// Sufficient: 100 > 2 -> proceed, full 2000 charge.
	ok, charge, err := ResolveSwingExertion(100, StandardSwingVigorRequired, StandardSwingExertionCost)
	if err != nil || !ok || charge != 2000 {
		t.Fatalf("sufficient = %v,%d,%v, want true,2000,nil", ok, charge, err)
	}
	// Exact threshold denies (strict >): 2 > 2 is false, charge 0.
	ok, charge, err = ResolveSwingExertion(2, StandardSwingVigorRequired, StandardSwingExertionCost)
	if err != nil || ok || charge != 0 {
		t.Fatalf("threshold = %v,%d,%v, want false,0,nil", ok, charge, err)
	}
	// Insufficient denies with zero charge.
	ok, charge, err = ResolveSwingExertion(0, StandardSwingVigorRequired, StandardSwingExertionCost)
	if err != nil || ok || charge != 0 {
		t.Fatalf("insufficient = %v,%d,%v, want false,0,nil", ok, charge, err)
	}
	// Negative impossible inputs fail.
	if _, _, err := ResolveSwingExertion(-1, 2, 2000); !errors.Is(err, ErrInvalidCombatStat) {
		t.Fatalf("neg vigor err = %v", err)
	}
}

// Property sweeps with independent oracles (the expected values below
// re-express the frozen formulas inline; they never call production).

func TestOffenseBoundsProperty(t *testing.T) {
	strokes := []int{0, 1, 25, 50, 99, 500, 1 << 30, math.MaxInt32}
	for _, s := range strokes {
		for _, p := range []int{0, 1, 50, 99, 1 << 30} {
			for _, a := range []int{0, 1, 40, 70} {
				for _, b := range []int{0, 1, 20, 80, 150, 1 << 30} {
					got, err := PlayerOffense(PlayerOffenseInput{Stroke: s, Proficiency: p, Aim: a, BaseMaxHP: b})
					if err != nil {
						t.Fatal(err)
					}
					// Independent oracle: saturating plain arithmetic.
					want := int64(s)*3 + int64(p)*2 + int64(a)*4 + (int64(b)*3)/2
					if want < 1 {
						want = 1
					}
					if want > 1000 {
						want = 1000
					}
					// Oracle inputs above are small enough that no
					// saturation applies except the documented bounds.
					if int64(got) != want {
						t.Fatalf("offense(%d,%d,%d,%d) = %d, oracle %d", s, p, a, b, got, want)
					}
				}
			}
		}
	}
}

func TestDefenseBoundsProperty(t *testing.T) {
	for _, parry := range []int{0, 1, 50, 99} {
		for _, block := range []int{0, 1, 30, 120} {
			for _, dodge := range []int{0, 1, 40, 99} {
				for _, agi := range []int{0, 1, 40, 70} {
					for _, b := range []int{0, 1, 20, 150} {
						got, err := PlayerDefense(PlayerDefenseInput{Parry: parry, Block: block, Dodge: dodge, Agility: agi, BaseMaxHP: b})
						if err != nil {
							t.Fatal(err)
						}
						want := int64(parry)*2 + int64(block) + int64(dodge)*3 + int64(agi)*4 + (int64(b)*3)/2
						if want < 1 {
							want = 1
						}
						if want > 1000 {
							want = 1000
						}
						if int64(got) != want {
							t.Fatalf("defense = %d, oracle %d", got, want)
						}
					}
				}
			}
		}
	}
}

func TestHitChanceBoundsProperty(t *testing.T) {
	offs := []int{1, 2, 10, 55, 300, 1000, 1500, 1 << 30, math.MaxInt}
	defs := []int{1, 2, 10, 55, 300, 1000, 1500, 1 << 30, math.MaxInt}
	for _, o := range offs {
		for _, d := range defs {
			got, err := HitChance(o, d)
			if err != nil {
				t.Fatal(err)
			}
			if got < 10 || got > 95 {
				t.Fatalf("HitChance(%d,%d) = %d, want 10..95", o, d, got)
			}
			// Independent oracle with saturating multiply.
			prod := int64(o) * 55
			if o > math.MaxInt/55 {
				prod = math.MaxInt64
			}
			want := prod / int64(d)
			if want < 10 {
				want = 10
			}
			if want > 95 {
				want = 95
			}
			if got != int(want) {
				t.Fatalf("HitChance(%d,%d) = %d, oracle %d", o, d, got, want)
			}
		}
	}
	// Monotonicity: chance non-decreasing in offense, non-increasing
	// in defense (guaranteed by the formula shape + clamp).
	prev := 0
	for o := 1; o <= 2000; o += 7 {
		got, _ := HitChance(o, 500)
		if got < prev {
			t.Fatalf("non-monotonic in offense at %d", o)
		}
		prev = got
	}
	prev = 100
	for d := 1; d <= 2000; d += 7 {
		got, _ := HitChance(500, d)
		if got > prev {
			t.Fatalf("non-monotonic in defense at %d", d)
		}
		prev = got
	}
}

func TestCapsBoundsProperty(t *testing.T) {
	for _, base := range []int{1, 2, 3, 20, 21, 22, 60, 90, 150, 1000, 1 << 40, math.MaxInt} {
		for _, hp := range []int{0, 1, base / 2, base, base * 2} {
			if hp < 0 {
				continue
			}
			for _, dmg := range []int{0, 1, 5, 15, 16, 30, 31, 100, 1 << 40} {
				got, err := ApplyPlayerDamageCaps(dmg, VictimSnapshot{HP: hp, BaseMaxHP: base}, false)
				if err != nil {
					t.Fatal(err)
				}
				if got < 1 || got > 30 {
					t.Fatalf("caps(%d,hp%d,base%d) = %d, want 1..30", dmg, hp, base, got)
				}
				// Independent oracle for the third-cap line (overflow-safe
				// re-expression, not a production call).
				var below bool
				if int64(base) > int64(math.MaxInt)/2 {
					below = true
				} else {
					below = int64(hp) < 2*int64(base)
				}
				if below {
					limit := int64(base) / 3
					if int64(base)%3 != 0 {
						limit++
					}
					if int64(got) > limit {
						t.Fatalf("caps(%d,hp%d,base%d) = %d exceeds ceil %d", dmg, hp, base, got, limit)
					}
				}
			}
		}
	}
}

func TestCooldownWrapProperty(t *testing.T) {
	// ready == (unsigned elapsed >= cooldown) for all wrap positions
	// (independent oracle, no production call for expectations).
	lasts := []uint32{0, 1, 100, math.MaxUint32 - 5, math.MaxUint32 - 1, math.MaxUint32}
	for _, hz := range []int{1, 20, 60, 120} {
		for _, last := range lasts {
			for delta := uint32(0); delta <= uint32(hz)+2; delta++ {
				now := last + delta // wraps mod 2^32
				got, err := SwingReady(true, last, now, hz)
				if err != nil {
					t.Fatal(err)
				}
				if want := delta >= uint32(hz); got != want {
					t.Fatalf("hz %d last %d now %d (delta %d): got %v want %v", hz, last, now, delta, got, want)
				}
			}
		}
	}
}

func FuzzHitChance(f *testing.F) {
	f.Add(430, 450)
	f.Add(1, 1000)
	f.Add(1000, 1)
	f.Fuzz(func(t *testing.T, off, def int) {
		got, err := HitChance(off, def)
		if off <= 0 || def <= 0 {
			if !errors.Is(err, ErrInvalidCombatRating) {
				t.Fatalf("expected ErrInvalidCombatRating, got %d,%v", got, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got < 10 || got > 95 {
			t.Fatalf("chance %d out of 10..95", got)
		}
	})
}

func FuzzMonsterRating(f *testing.F) {
	f.Add(45, 6)
	f.Add(170, 9)
	f.Add(0, 0)
	f.Fuzz(func(t *testing.T, level, diff int) {
		got, err := MonsterRating(level, diff)
		if level < 0 || diff < 0 {
			if !errors.Is(err, ErrInvalidCombatRating) {
				t.Fatalf("expected ErrInvalidCombatRating, got %d,%v", got, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got < 1 || got > 1500 {
			t.Fatalf("rating %d out of 1..1500", got)
		}
	})
}

func FuzzPlayerDamageCaps(f *testing.F) {
	f.Add(30, 10, 20, false, false, false)
	f.Add(100, 39, 20, true, false, true)
	f.Add(0, 0, 1, false, false, false)
	f.Fuzz(func(t *testing.T, dmg, hp, base int, outlaw, murderer, prot bool) {
		got, err := ApplyPlayerDamageCaps(dmg, VictimSnapshot{HP: hp, BaseMaxHP: base, Outlaw: outlaw, Murderer: murderer}, prot)
		if dmg < 0 || base < 1 || hp < 0 {
			if err == nil {
				t.Fatalf("expected error for dmg %d hp %d base %d", dmg, hp, base)
			}
			return
		}
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got < 1 || got > 30 {
			t.Fatalf("capped %d out of 1..30", got)
		}
	})
}

func FuzzClassifySeverity(f *testing.F) {
	f.Add(1, false, false, 0)
	f.Add(16, false, true, 30)
	f.Add(30, true, true, 90)
	f.Fuzz(func(t *testing.T, dmg int, killed, isPlayer bool, maxHP int) {
		got, err := ClassifySeverity(dmg, killed, isPlayer, maxHP)
		if killed {
			if err != nil || got != SeveritySlay {
				t.Fatalf("killed = %v,%v, want slay,nil", got, err)
			}
			return
		}
		if dmg < 1 || (isPlayer && maxHP < 1) {
			if !errors.Is(err, ErrInvalidDamageValue) {
				t.Fatalf("expected ErrInvalidDamageValue, got %v,%v", got, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		switch got {
		case SeverityNick, SeverityWound, SeverityDamage:
		default:
			t.Fatalf("non-lethal severity %v invalid", got)
		}
	})
}

func FuzzSwingReady(f *testing.F) {
	f.Add(true, uint32(100), uint32(119), 20)
	f.Add(false, uint32(0), uint32(0), 20)
	f.Add(true, uint32(math.MaxUint32), uint32(0), 1)
	f.Fuzz(func(t *testing.T, hasSwung bool, last, now uint32, hz int) {
		got, err := SwingReady(hasSwung, last, now, hz)
		if hz < 1 || hz > 120 {
			if !errors.Is(err, ErrInvalidTickHz) {
				t.Fatalf("expected ErrInvalidTickHz, got %v,%v", got, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		want := !hasSwung || now-last >= uint32(hz)
		if got != want {
			t.Fatalf("SwingReady(%v,%d,%d,%d) = %v, want %v", hasSwung, last, now, hz, got, want)
		}
	})
}
