package sim

import (
	"errors"
	"math"
	"testing"
)

func TestWallMaxDamageGolden(t *testing.T) {
	// Fire: power/6 bound(1,16). Lightning: power/4 bound(1,25).
	// Source walspell/firewall.kod, ltngwall.kod.
	fire := []struct {
		power, want int
	}{
		{1, 1},   // 0 -> floor 1
		{5, 1},   // 0 -> floor 1
		{6, 1},   // exact 1
		{12, 2},  // truncation
		{95, 15}, // 95/6 = 15
		{96, 16}, // 96/6 = 16 = cap
		{99, 16}, // 99/6 = 16, capped
	}
	for _, c := range fire {
		got, err := WallMaxDamage(WallFire, c.power)
		if err != nil || got != c.want {
			t.Fatalf("fire power %d = %d,%v, want %d,nil", c.power, got, err, c.want)
		}
	}
	lightning := []struct {
		power, want int
	}{
		{1, 1},   // 0 -> floor 1
		{4, 1},   // exact 1
		{7, 1},   // truncation: 7/4 = 1
		{40, 10}, // ordinary
		{99, 24}, // 99/4 = 24 (cap 25 unreachable in-domain)
	}
	for _, c := range lightning {
		got, err := WallMaxDamage(WallLightning, c.power)
		if err != nil || got != c.want {
			t.Fatalf("lightning power %d = %d,%v, want %d,nil", c.power, got, err, c.want)
		}
	}
	// Illusionary passes spell power DIRECTLY (never power/6).
	for _, power := range []int{1, 34, 35, 99} {
		got, err := WallMaxDamage(WallIllusionaryFire, power)
		if err != nil || got != power {
			t.Fatalf("illusionary power %d = %d,%v, want %d,nil", power, got, err, power)
		}
	}
	if _, err := WallMaxDamage(WallDamageKind(0), 50); !errors.Is(err, ErrInvalidWallKind) {
		t.Fatalf("kind 0 err = %v", err)
	}
	if _, err := WallMaxDamage(WallFire, 0); !errors.Is(err, ErrInvalidSpellPower) {
		t.Fatalf("power 0 err = %v", err)
	}
}

func TestWallBaseLifetimeGolden(t *testing.T) {
	// Fire/illusionary: power*2+30 bound(30,180).
	// Lightning: power*2+20 bound(20,120).
	fire := []struct {
		power, want int
	}{
		{1, 32},
		{40, 110},
		{75, 180}, // 180 exact
		{99, 180}, // 228 -> capped
	}
	for _, c := range fire {
		for _, kind := range []WallDamageKind{WallFire, WallIllusionaryFire} {
			got, err := WallBaseLifetimeSeconds(kind, c.power)
			if err != nil || got != c.want {
				t.Fatalf("%v power %d = %d,%v, want %d,nil", kind, c.power, got, err, c.want)
			}
		}
	}
	lightning := []struct {
		power, want int
	}{
		{1, 22},
		{50, 120}, // 120 exact
		{99, 120}, // 218 -> capped
	}
	for _, c := range lightning {
		got, err := WallBaseLifetimeSeconds(WallLightning, c.power)
		if err != nil || got != c.want {
			t.Fatalf("lightning power %d = %d,%v, want %d,nil", c.power, got, err, c.want)
		}
	}
	if _, err := WallBaseLifetimeSeconds(WallDamageKind(9), 50); !errors.Is(err, ErrInvalidWallKind) {
		t.Fatalf("kind 9 err = %v", err)
	}
	if _, err := WallBaseLifetimeSeconds(WallFire, 100); !errors.Is(err, ErrInvalidSpellPower) {
		t.Fatalf("power 100 err = %v", err)
	}
}

func TestRollWallLifetimeGolden(t *testing.T) {
	// Random(base-20,base+20)*1000 bound(30000,200000).
	// Source wallfire/wallltng GetDuration + passive constructors.
	ms, err := RollWallLifetimeMs(forceBounded(80, 80, 120), 100) // forced min
	if err != nil || ms != 80000 {
		t.Fatalf("base 100 min jitter = %d,%v, want 80000", ms, err)
	}
	ms, err = RollWallLifetimeMs(forceBounded(120, 80, 120), 100) // forced max
	if err != nil || ms != 120000 {
		t.Fatalf("base 100 max jitter = %d,%v, want 120000", ms, err)
	}
	// Lower 30 s clamp: base 20 forced min -> 0 s -> 0 ms -> 30000.
	ms, err = RollWallLifetimeMs(forceBounded(0, 0, 40), 20)
	if err != nil || ms != 30000 {
		t.Fatalf("base 20 min jitter = %d,%v, want 30000", ms, err)
	}
	// Upper behavior: base 190 forced max -> 210 s -> 210000 -> 200000.
	ms, err = RollWallLifetimeMs(forceBounded(210, 170, 210), 190)
	if err != nil || ms != 200000 {
		t.Fatalf("base 190 max jitter = %d,%v, want 200000", ms, err)
	}
	if _, err := RollWallLifetimeMs(nil, 100); !errors.Is(err, ErrNilRNG) {
		t.Fatalf("nil rng err = %v", err)
	}
	if _, err := RollWallLifetimeMs(&scriptRNG{}, -1); !errors.Is(err, ErrInvalidWallLifetime) {
		t.Fatalf("negative base err = %v", err)
	}
}

func TestRollWallPeriodGolden(t *testing.T) {
	// (1500*Random(90,110))/100. Source wallelem.kod GetPeriodicDuration.
	ms, err := RollWallPeriodMs(forceBounded(90, 90, 110))
	if err != nil || ms != 1350 {
		t.Fatalf("jitter 90 = %d,%v, want 1350", ms, err)
	}
	ms, err = RollWallPeriodMs(forceBounded(100, 90, 110))
	if err != nil || ms != 1500 {
		t.Fatalf("jitter 100 = %d,%v, want 1500", ms, err)
	}
	ms, err = RollWallPeriodMs(forceBounded(110, 90, 110))
	if err != nil || ms != 1650 {
		t.Fatalf("jitter 110 = %d,%v, want 1650", ms, err)
	}
	if _, err := RollWallPeriodMs(nil); !errors.Is(err, ErrNilRNG) {
		t.Fatalf("nil rng err = %v", err)
	}
}

func TestRollOrdinaryWallDamageGolden(t *testing.T) {
	// Random(0,maxDamage); zero legitimate, never floored.
	// Source wallfire.kod / wallltng.kod effect.
	res, err := RollOrdinaryWallDamage(forceBounded(0, 0, 16), WallFire, 16)
	if err != nil {
		t.Fatalf("forced 0: %v", err)
	}
	if res.RawDamage != 0 || res.Policy != PolicyOrdinary {
		t.Fatalf("forced 0 = %+v, want raw 0 ordinary", res)
	}
	if res.Signature.Spell != uint32(ResistSpellAll)|uint32(ResistSpellFire) {
		t.Fatalf("fire signature = %x", res.Signature.Spell)
	}
	res, err = RollOrdinaryWallDamage(forceBounded(16, 0, 16), WallFire, 16)
	if err != nil || res.RawDamage != 16 {
		t.Fatalf("forced max = %+v,%v, want raw 16", res, err)
	}
	res, err = RollOrdinaryWallDamage(forceBounded(7, 0, 24), WallLightning, 24)
	if err != nil {
		t.Fatalf("lightning: %v", err)
	}
	if res.RawDamage != 7 || res.Policy != PolicyOrdinary {
		t.Fatalf("lightning = %+v, want raw 7 ordinary", res)
	}
	if res.Signature.Spell != uint32(ResistSpellAll)|uint32(ResistSpellShock) {
		t.Fatalf("lightning signature = %x", res.Signature.Spell)
	}
	if _, err := RollOrdinaryWallDamage(&scriptRNG{}, WallIllusionaryFire, 35); !errors.Is(err, ErrInvalidWallKind) {
		t.Fatalf("illusionary kind err = %v", err)
	}
	if _, err := RollOrdinaryWallDamage(&scriptRNG{}, WallDamageKind(0), 5); !errors.Is(err, ErrInvalidWallKind) {
		t.Fatalf("kind 0 err = %v", err)
	}
	if _, err := RollOrdinaryWallDamage(&scriptRNG{}, WallFire, -1); !errors.Is(err, ErrInvalidDamageValue) {
		t.Fatalf("negative max err = %v", err)
	}
	if _, err := RollOrdinaryWallDamage(nil, WallFire, 5); !errors.Is(err, ErrNilRNG) {
		t.Fatalf("nil rng err = %v", err)
	}
}

func TestRollIllusionaryWallGolden(t *testing.T) {
	victim := IllusionaryVictim{Kind: VictimPlayer, Intellect: 50, MaxHP: 40, HP: 40}
	// Power 34: below threshold, no effect.
	res, err := RollIllusionaryWall(&scriptRNG{}, victim, 34)
	if err != nil {
		t.Fatalf("power 34: %v", err)
	}
	if res.Eligible || res != (IllusionaryWallResult{}) {
		t.Fatalf("power 34 = %+v, want ineligible zero value", res)
	}
	// Power 35: eligible. IW max = 17*35/100 = 595/100 = 5 (caps 13/39).
	maxLoss, err := IllusionaryWoundsLoss(victim, 35)
	if err != nil || maxLoss != 5 {
		t.Fatalf("IW max power 35 = %d,%v, want 5", maxLoss, err)
	}
	res, err = RollIllusionaryWall(forceBounded(0, 0, 5), victim, 35)
	if err != nil {
		t.Fatalf("power 35: %v", err)
	}
	if !res.Eligible || res.RawLoss != 0 || res.RefundState != 0 {
		t.Fatalf("power 35 forced 0 = %+v, want eligible raw/refund 0", res)
	}
	if res.Policy != PolicyAbsolute {
		t.Fatalf("policy = %v, want absolute", res.Policy)
	}
	if res.DurationMs != 20000+35*750 {
		t.Fatalf("duration = %d, want %d", res.DurationMs, 20000+35*750)
	}
	// Forced random max: raw == IW max (shared-primitive proof, B20).
	res, err = RollIllusionaryWall(forceBounded(5, 0, 5), victim, 35)
	if err != nil {
		t.Fatalf("power 35 max: %v", err)
	}
	if !res.Eligible || res.RawLoss != maxLoss || res.RefundState != maxLoss {
		t.Fatalf("power 35 max = %+v, want raw/refund %d", res, maxLoss)
	}
	// IW max 0 (HP=1 victim): no effect even at eligible power.
	oneHP := IllusionaryVictim{Kind: VictimPlayer, Intellect: 50, MaxHP: 40, HP: 1}
	res, err = RollIllusionaryWall(&scriptRNG{}, oneHP, 99)
	if err != nil {
		t.Fatalf("HP=1: %v", err)
	}
	if res.Eligible {
		t.Fatalf("HP=1 maxLoss 0 must be ineligible: %+v", res)
	}
	if _, err := RollIllusionaryWall(&scriptRNG{}, victim, 0); !errors.Is(err, ErrInvalidSpellPower) {
		t.Fatalf("power 0 err = %v", err)
	}
	badVictim := IllusionaryVictim{Kind: VictimMonster, Difficulty: 0, MaxHP: 40, HP: 40}
	if _, err := RollIllusionaryWall(&scriptRNG{}, badVictim, 99); !errors.Is(err, ErrInvalidSpecialVictim) {
		t.Fatalf("bad victim err = %v", err)
	}
}

func TestIllusionaryWallNonLethalProperty(t *testing.T) {
	// maxLoss <= HP-1 and rawLoss <= maxLoss: never lethal by construction.
	for _, power := range []int{35, 50, 80, 99} {
		for _, hp := range []int{1, 2, 10, 40, 150} {
			for _, intellect := range []int{1, 25, 50, 70} {
				v := IllusionaryVictim{Kind: VictimPlayer, Intellect: intellect, MaxHP: 150, HP: hp}
				maxLoss, err := IllusionaryWoundsLoss(v, power)
				if err != nil {
					t.Fatalf("p %d hp %d int %d: %v", power, hp, intellect, err)
				}
				if maxLoss > hp-1 {
					t.Fatalf("p %d hp %d int %d: maxLoss %d > HP-1", power, hp, intellect, maxLoss)
				}
				for _, force := range []int{0, maxLoss} {
					if maxLoss == 0 {
						continue
					}
					res, err := RollIllusionaryWall(forceBounded(force, 0, maxLoss), v, power)
					if err != nil {
						t.Fatalf("roll: %v", err)
					}
					if !res.Eligible || res.RawLoss > maxLoss || res.RawLoss > hp-1 {
						t.Fatalf("p %d hp %d: %+v exceeds non-lethal bound", power, hp, res)
					}
					if res.RefundState != res.RawLoss {
						t.Fatalf("refund must equal rolled raw loss: %+v", res)
					}
				}
			}
		}
	}
}

func TestSpecialPropertyInvariants(t *testing.T) {
	// TouchProficiency oracle: >= Punch and >= floor(3*Myst/2).
	for punch := 0; punch <= 120; punch += 7 {
		for myst := 0; myst <= 120; myst += 5 {
			got, err := TouchProficiency(punch, myst)
			if err != nil {
				t.Fatalf("punch %d myst %d: %v", punch, myst, err)
			}
			want := int64(punch)
			if c := int64(myst) * 3 / 2; c > want {
				want = c
			}
			if int64(got) != want {
				t.Fatalf("punch %d myst %d = %d, want %d", punch, myst, got, want)
			}
		}
	}
	// Generic touch damage >= 1 and bounded above by max+1.
	for _, bounds := range [][2]int{{3, 6}, {4, 9}, {0, 0}, {0, 30}} {
		for power := 1; power <= 99; power++ {
			got, err := RollTouchDamage(&scriptRNG{vals: []uint64{uint64(power * 7919)}}, bounds[0], bounds[1], power)
			if err != nil {
				t.Fatalf("bounds %v power %d: %v", bounds, power, err)
			}
			if got < 1 || got > bounds[1]+1 {
				t.Fatalf("bounds %v power %d damage %d outside [1, max+1]", bounds, power, got)
			}
		}
	}
	// IW loss within [0, min(MaxHP/3, HP-1)] across the domain.
	for _, maxHP := range []int{1, 2, 3, 40, 150} {
		for hp := 1; hp <= maxHP; hp++ {
			for _, power := range []int{1, 50, 99} {
				pv := IllusionaryVictim{Kind: VictimPlayer, Intellect: 25, MaxHP: maxHP, HP: hp}
				got, err := IllusionaryWoundsLoss(pv, power)
				if err != nil {
					t.Fatalf("maxHP %d hp %d: %v", maxHP, hp, err)
				}
				limit := maxHP / 3
				if hp-1 < limit {
					limit = hp - 1
				}
				if got < 0 || got > limit {
					t.Fatalf("maxHP %d hp %d power %d loss %d outside [0, %d]",
						maxHP, hp, power, got, limit)
				}
				mv := IllusionaryVictim{Kind: VictimMonster, Difficulty: 9, MaxHP: maxHP, HP: hp}
				got, err = IllusionaryWoundsLoss(mv, power)
				if err != nil {
					t.Fatalf("monster maxHP %d hp %d: %v", maxHP, hp, err)
				}
				if got < 0 || got > limit {
					t.Fatalf("monster maxHP %d hp %d power %d loss %d outside [0, %d]",
						maxHP, hp, power, got, limit)
				}
			}
		}
	}
	// IW duration always 20 s..80 s.
	for power := 1; power <= 99; power++ {
		got, err := IllusionaryWoundsDuration(power)
		if err != nil || got < 20000 || got > 80000 {
			t.Fatalf("power %d duration %d,%v outside 20s..80s", power, got, err)
		}
	}
	// Earthquake percent oracle: independent restatement + monotonicity.
	prev := 101
	for sq := 0; sq <= 500; sq++ {
		got, err := EarthquakeDamagePercent(sq)
		if err != nil {
			t.Fatalf("sq %d: %v", sq, err)
		}
		var want int
		switch {
		case sq <= 64:
			want = 100
		case sq > 400:
			want = 0
		default:
			want = 100 * (400 - sq) / 336
		}
		if got != want {
			t.Fatalf("sq %d = %d, want %d", sq, got, want)
		}
		if got > prev {
			t.Fatalf("sq %d percent %d increased over %d", sq, got, prev)
		}
		prev = got
	}
	// Wall period/lifetime bounds; ordinary damage bounds.
	for seed := uint64(0); seed < 128; seed++ {
		ms, err := RollWallPeriodMs(&scriptRNG{vals: []uint64{seed}})
		if err != nil || ms < 1350 || ms > 1650 {
			t.Fatalf("seed %d period %d,%v outside 1350..1650", seed, ms, err)
		}
		life, err := RollWallLifetimeMs(&scriptRNG{vals: []uint64{seed * 31}}, 100)
		if err != nil || life < 30000 || life > 200000 {
			t.Fatalf("seed %d lifetime %d,%v outside 30s..200s", seed, life, err)
		}
		wd, err := RollOrdinaryWallDamage(&scriptRNG{vals: []uint64{seed}}, WallFire, 16)
		if err != nil || wd.RawDamage < 0 || wd.RawDamage > 16 {
			t.Fatalf("seed %d wall damage %+v,%v outside 0..16", seed, wd, err)
		}
	}
	// Vamp heal formula sweep: nonlethal d/2 floored at 1.
	for d := 0; d <= 60; d++ {
		got, err := VampiricDrainHeal(d, false, 18)
		if err != nil {
			t.Fatalf("d %d: %v", d, err)
		}
		want := d / 2
		if want < 1 {
			want = 1
		}
		if got != want {
			t.Fatalf("d %d heal %d, want %d", d, got, want)
		}
	}
	// Determinism: identical scripted RNG + inputs -> identical outputs.
	mk := func() *scriptRNG { return &scriptRNG{vals: []uint64{5, 17, 99}} }
	a1, _ := RollTouchDamage(mk(), 3, 9, 50)
	a2, _ := RollTouchDamage(mk(), 3, 9, 50)
	if a1 != a2 {
		t.Fatal("touch damage nondeterministic")
	}
	b1, _ := RollEarthquakeDamage(mk(), 3, 50)
	b2, _ := RollEarthquakeDamage(mk(), 3, 50)
	if b1 != b2 {
		t.Fatal("quake damage nondeterministic")
	}
}

func FuzzTouchProficiency(f *testing.F) {
	f.Add(30, 40)
	f.Add(0, 0)
	f.Add(99, 70)
	f.Fuzz(func(t *testing.T, punch, myst int) {
		got, err := TouchProficiency(punch, myst)
		if punch < 0 || myst < 0 {
			if !errors.Is(err, ErrInvalidCombatStat) {
				t.Fatalf("expected ErrInvalidCombatStat, got %d,%v", got, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got < punch {
			t.Fatalf("proficiency %d < punch %d", got, punch)
		}
	})
}

func FuzzTouchDamage(f *testing.F) {
	f.Add(3, 6, 50, uint64(4))
	f.Add(4, 9, 99, uint64(0))
	f.Fuzz(func(t *testing.T, min, max, power int, draw uint64) {
		got, err := RollTouchDamage(&scriptRNG{vals: []uint64{draw}}, min, max, power)
		if min < 0 || max < 0 {
			if !errors.Is(err, ErrInvalidDamageValue) {
				t.Fatalf("expected ErrInvalidDamageValue, got %d,%v", got, err)
			}
			return
		}
		if min > max {
			if !errors.Is(err, ErrInvalidRange) {
				t.Fatalf("expected ErrInvalidRange, got %d,%v", got, err)
			}
			return
		}
		if power < 1 || power > 99 {
			if !errors.Is(err, ErrInvalidSpellPower) {
				t.Fatalf("expected ErrInvalidSpellPower, got %d,%v", got, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got < 1 {
			t.Fatalf("touch damage %d < 1", got)
		}
	})
}

func FuzzIllusionaryWoundsLoss(f *testing.F) {
	f.Add(1, 25, 40, 40, 50)
	f.Add(2, 9, 60, 10, 99)
	f.Fuzz(func(t *testing.T, kind, x, maxHP, hp, power int) {
		v := IllusionaryVictim{MaxHP: maxHP, HP: hp}
		switch kind % 3 {
		case 0:
			v.Kind = VictimPlayer
			v.Intellect = x
		case 1, -1:
			v.Kind = VictimMonster
			v.Difficulty = x
		default:
			// Arbitrary kind value (valid or not); populate both
			// stat fields so validity hinges on the kind alone.
			v.Kind = IllusionaryVictimKind(kind)
			v.Intellect = x
			v.Difficulty = x
		}
		got, err := IllusionaryWoundsLoss(v, power)
		validKind := v.Kind == VictimPlayer || v.Kind == VictimMonster
		validStats := maxHP >= 1 && hp >= 1 && power >= 1 && power <= 99
		if v.Kind == VictimPlayer {
			validStats = validStats && x >= 1 && x <= 70
		} else if v.Kind == VictimMonster {
			validStats = validStats && x >= 1
		}
		if !validKind || !validStats {
			if !errors.Is(err, ErrInvalidSpecialVictim) && !errors.Is(err, ErrInvalidSpellPower) {
				t.Fatalf("expected victim/power error, got %d,%v", got, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got < 0 || got > maxHP/3 || got > hp-1 {
			t.Fatalf("loss %d outside [0, min(MaxHP/3, HP-1)]", got)
		}
	})
}

func FuzzHolyTouchModifier(f *testing.F) {
	f.Add(10, 0, false)
	f.Add(10, -100, false)
	f.Add(10, 100, true)
	f.Fuzz(func(t *testing.T, damage, karma int, undead bool) {
		got, err := ApplyHolyTouchModifier(damage, karma, undead)
		if damage < 0 {
			if !errors.Is(err, ErrInvalidDamageValue) {
				t.Fatalf("expected ErrInvalidDamageValue, got %d,%v", got, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got < 0 {
			t.Fatalf("holy damage %d < 0 (sign flip)", got)
		}
		if undead && damage <= math.MaxInt/2 && got != damage*2 {
			t.Fatalf("undead %d from %d, want exactly double", got, damage)
		}
	})
}

func FuzzVampiricDrainHeal(f *testing.F) {
	f.Add(6, false, 18)
	f.Add(0, false, 18)
	f.Add(12, true, 18)
	f.Fuzz(func(t *testing.T, applied int, killed bool, protoMax int) {
		got, err := VampiricDrainHeal(applied, killed, protoMax)
		if protoMax < 1 {
			if !errors.Is(err, ErrInvalidDamageValue) {
				t.Fatalf("expected ErrInvalidDamageValue, got %d,%v", got, err)
			}
			return
		}
		if !killed && applied < 0 {
			if !errors.Is(err, ErrInvalidDamageValue) {
				t.Fatalf("expected ErrInvalidDamageValue, got %d,%v", got, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got < 1 {
			t.Fatalf("heal %d < 1", got)
		}
		if !killed && got != applied/2 && applied/2 >= 1 {
			t.Fatalf("heal %d != %d/2", got, applied)
		}
		if !killed && applied/2 < 1 && got != 1 {
			t.Fatalf("heal %d must floor at 1", got)
		}
		if killed && got != protoMax/2 && protoMax/2 >= 1 {
			t.Fatalf("lethal heal %d != max/2", got)
		}
	})
}

func FuzzEarthquakeDamagePercent(f *testing.F) {
	f.Add(0)
	f.Add(64)
	f.Add(232)
	f.Add(400)
	f.Add(401)
	f.Fuzz(func(t *testing.T, sq int) {
		got, err := EarthquakeDamagePercent(sq)
		if sq < 0 {
			if !errors.Is(err, ErrInvalidSquaredDistance) {
				t.Fatalf("expected ErrInvalidSquaredDistance, got %d,%v", got, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got < 0 || got > 100 {
			t.Fatalf("percent %d outside 0..100", got)
		}
		if sq <= 64 && got != 100 {
			t.Fatalf("sq %d percent %d, want 100", sq, got)
		}
		if sq > 400 && got != 0 {
			t.Fatalf("sq %d percent %d, want 0", sq, got)
		}
	})
}

func FuzzWallConfig(f *testing.F) {
	f.Add(1, 50, 100)
	f.Add(3, 99, 20)
	f.Fuzz(func(t *testing.T, kind, power, base int) {
		maxDmg, err := WallMaxDamage(WallDamageKind(kind), power)
		switch WallDamageKind(kind) {
		case WallFire, WallLightning, WallIllusionaryFire:
		default:
			if !errors.Is(err, ErrInvalidWallKind) {
				t.Fatalf("expected ErrInvalidWallKind, got %d,%v", maxDmg, err)
			}
			return
		}
		if power < 1 || power > 99 {
			if !errors.Is(err, ErrInvalidSpellPower) {
				t.Fatalf("expected ErrInvalidSpellPower, got %d,%v", maxDmg, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if maxDmg < 1 {
			t.Fatalf("maxDamage %d < 1", maxDmg)
		}
		life, err := RollWallLifetimeMs(&scriptRNG{vals: []uint64{uint64(base)}}, base)
		if base < 0 {
			if !errors.Is(err, ErrInvalidWallLifetime) {
				t.Fatalf("expected ErrInvalidWallLifetime, got %d,%v", life, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if life < 30000 || life > 200000 {
			t.Fatalf("lifetime %d outside 30s..200s", life)
		}
	})
}
