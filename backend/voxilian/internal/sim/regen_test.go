package sim

import (
	"errors"
	"math"
	"math/rand"
	"testing"
)

func TestNodeManaGoldens(t *testing.T) {
	// Myst 25: standard +6, double +12.
	s, err := StandardNodeMana(25)
	if err != nil || s != 6 {
		t.Fatalf("StandardNodeMana(25) = (%d,%v), want (6,nil)", s, err)
	}
	std, err := NodeManaBonus(NodeStandard, 25)
	if err != nil || std != 6 {
		t.Fatalf("NodeManaBonus(standard,25) = (%d,%v), want (6,nil)", std, err)
	}
	dbl, err := NodeManaBonus(NodeDouble, 25)
	if err != nil || dbl != 12 {
		t.Fatalf("NodeManaBonus(double,25) = (%d,%v), want (12,nil)", dbl, err)
	}
	// Range edges: Myst 1 -> 3, Myst 50 -> 8 (standard).
	s, err = StandardNodeMana(1)
	if err != nil || s != 3 {
		t.Fatalf("StandardNodeMana(1) = (%d,%v), want (3,nil)", s, err)
	}
	s, err = StandardNodeMana(50)
	if err != nil || s != 8 {
		t.Fatalf("StandardNodeMana(50) = (%d,%v), want (8,nil)", s, err)
	}
	if _, err := StandardNodeMana(0); !errors.Is(err, ErrInvalidCombatStat) {
		t.Fatalf("myst 0 err = %v, want ErrInvalidCombatStat", err)
	}
	if _, err := NodeManaBonus(NodeKind(99), 25); !errors.Is(err, ErrInvalidNodeKind) {
		t.Fatalf("bad kind err = %v, want ErrInvalidNodeKind", err)
	}
	// ComputeMaxMana: initial + nodes + other, no bound.
	total, err := ComputeMaxMana(20, []int{6, 6, 12}, 2)
	if err != nil || total != 46 {
		t.Fatalf("ComputeMaxMana = (%d,%v), want (46,nil)", total, err)
	}
	total, err = ComputeMaxMana(15, nil, 0)
	if err != nil || total != 15 {
		t.Fatalf("ComputeMaxMana bare = (%d,%v), want (15,nil)", total, err)
	}
	// Negative (unmeld-style) contributions sum arithmetically.
	total, err = ComputeMaxMana(20, []int{6, -6}, 0)
	if err != nil || total != 20 {
		t.Fatalf("ComputeMaxMana net = (%d,%v), want (20,nil)", total, err)
	}
	if _, err := ComputeMaxMana(math.MaxInt, []int{math.MaxInt}, 0); !errors.Is(err, ErrInvalidManaAmount) {
		t.Fatalf("hostile sum err = %v, want ErrInvalidManaAmount", err)
	}
}

func TestTimerStepGoldens(t *testing.T) {
	for _, tc := range []struct {
		val, max int
		want     TimerStep
	}{
		{10, 20, TimerGainOne},
		{20, 20, TimerNoChange},
		{25, 20, TimerDecayOne},
	} {
		got, err := HealthTimerStep(tc.val, tc.max)
		if err != nil || got != tc.want {
			t.Fatalf("HealthTimerStep(%d,%d) = (%v,%v), want (%v,nil)",
				tc.val, tc.max, got, err, tc.want)
		}
		got, err = ManaTimerStep(tc.val, tc.max)
		if err != nil || got != tc.want {
			t.Fatalf("ManaTimerStep(%d,%d) = (%v,%v), want (%v,nil)",
				tc.val, tc.max, got, err, tc.want)
		}
	}
	if _, err := HealthTimerStep(-1, 20); !errors.Is(err, ErrInvalidVitals) {
		t.Fatalf("negative hp step err = %v, want ErrInvalidVitals", err)
	}
	if _, err := ManaTimerStep(5, 0); !errors.Is(err, ErrInvalidVitals) {
		t.Fatalf("zero maxmana step err = %v, want ErrInvalidVitals", err)
	}
	if TimerGainOne.String() != "gain" || TimerNoChange.String() != "none" ||
		TimerDecayOne.String() != "decay" {
		t.Fatalf("TimerStep String mismatch")
	}
}

func TestHealthRegenIntervalGoldens(t *testing.T) {
	for _, tc := range []struct {
		vigor, stam, max, faction, song int
		want                            int
	}{
		// Base vectors (faction 0, no song). Hand-computed:
		// t=((200-V)^2)/6+1000; t=((125-S)*t)/100; t=(t*100)/bound(Max,40,100).
		{100, 25, 40, 0, 0, 6665},   // 1666+1000=2666; 2666; 266600/40
		{1, 25, 40, 0, 0, 19000},    // 6600+1000=7600; 7600; 760000/40
		{200, 25, 40, 0, 0, 2500},   // 0+1000; 1000; 100000/40
		{100, 25, 20, 0, 0, 6665},   // Max<40 uses 40
		{100, 25, 150, 0, 0, 2666},  // Max>100 uses 100
		{200, 70, 100, 0, 0, 1000},  // 550 -> final min 1000
		{100, 25, 40, 0, 50, 5165},  // Restorate: (6665*310)/400
		{100, 25, 40, 0, 99, 4348},  // (6665*261)/400
		{100, 25, 40, 0, 1, 5981},   // (6665*359)/400
		{100, 25, 40, 665, 0, 6000}, // faction subtract: 6665-665
	} {
		got, err := HealthRegenIntervalMs(tc.vigor, tc.stam, tc.max, tc.faction, tc.song)
		if err != nil {
			t.Fatalf("HealthRegen(%d,%d,%d,%d,%d): unexpected err %v",
				tc.vigor, tc.stam, tc.max, tc.faction, tc.song, err)
		}
		if got != tc.want {
			t.Fatalf("HealthRegen(%d,%d,%d,%d,%d) = %d, want %d",
				tc.vigor, tc.stam, tc.max, tc.faction, tc.song, got, tc.want)
		}
	}
	// Over-max HP reuses the same interval: the function takes no HP.
	a, _ := HealthRegenIntervalMs(100, 25, 20, 0, 0)
	if a != 6665 {
		t.Fatalf("over-max interval = %d, want 6665 (no HP branch)", a)
	}
	// Restorate helper bounds: input clamped to 1000..60000 first.
	out, err := ApplyRestorateAdjust(100, 99)
	if err != nil {
		t.Fatal(err)
	}
	// bound(100,1000,60000)=1000; (1000*261)/400=652 -> bound 670.
	if out != 670 {
		t.Fatalf("ApplyRestorateAdjust(100,99) = %d, want 670", out)
	}
	if _, err := ApplyRestorateAdjust(5000, 0); !errors.Is(err, ErrInvalidSpellPower) {
		t.Fatalf("power 0 err = %v, want ErrInvalidSpellPower", err)
	}
	// Domain errors.
	if _, err := HealthRegenIntervalMs(0, 25, 40, 0, 0); !errors.Is(err, ErrInvalidVitals) {
		t.Fatalf("vigor 0 err = %v, want ErrInvalidVitals", err)
	}
	if _, err := HealthRegenIntervalMs(100, 0, 40, 0, 0); !errors.Is(err, ErrInvalidCombatStat) {
		t.Fatalf("stam 0 err = %v, want ErrInvalidCombatStat", err)
	}
	if _, err := HealthRegenIntervalMs(100, 25, 19, 0, 0); !errors.Is(err, ErrInvalidVitals) {
		t.Fatalf("max 19 err = %v, want ErrInvalidVitals", err)
	}
}

func TestManaRegenIntervalGoldens(t *testing.T) {
	for _, tc := range []struct {
		mana, max, vigor, myst, faction, rej, focus int
		want                                        int
	}{
		// t=150000+(25-M)*1000; t=t*200/V; t=t/MM; bound 1..60s.
		{20, 20, 100, 25, 0, 0, 0, 15000},
		{20, 20, 1, 25, 0, 0, 0, 60000},   // 1500000 clamped
		{20, 20, 200, 25, 0, 0, 0, 7500},  // 150000/20
		{1, 1, 100, 25, 0, 0, 0, 60000},   // divisor edge MM=1
		{20, 200, 100, 25, 0, 0, 0, 1500}, // divisor edge MM=200
		{20, 200, 200, 70, 0, 0, 0, 1000}, // 525 -> min 1000
		{20, 20, 100, 25, 0, 50, 0, 11250},
		{20, 20, 100, 25, 0, 0, 50, 11250},
		{20, 20, 100, 25, 5000, 0, 0, 10000}, // faction subtract
	} {
		got, err := ManaRegenIntervalMs(tc.mana, tc.max, tc.vigor, tc.myst,
			tc.faction, tc.rej, tc.focus)
		if err != nil {
			t.Fatalf("ManaRegen(%d,%d,%d,%d,%d,%d,%d): unexpected err %v",
				tc.mana, tc.max, tc.vigor, tc.myst, tc.faction, tc.rej, tc.focus, err)
		}
		if got != tc.want {
			t.Fatalf("ManaRegen(%d,%d,%d,%d,%d,%d,%d) = %d, want %d",
				tc.mana, tc.max, tc.vigor, tc.myst, tc.faction, tc.rej, tc.focus, got, tc.want)
		}
	}
	// Over-max returns BOOST_DECAY_TIME exactly: no modifiers, no bounds.
	got, err := ManaRegenIntervalMs(25, 20, 100, 25, 0, 0, 0)
	if err != nil || got != 30000 {
		t.Fatalf("over-max mana interval = (%d,%v), want (30000,nil)", got, err)
	}
	// Helpers.
	r, err := ApplyRejuvenateAdjust(15000, 50)
	if err != nil || r != 11250 {
		t.Fatalf("Rejuvenate(15000,50) = (%d,%v), want (11250,nil)", r, err)
	}
	f, err := ApplyManaFocusAdjust(15000, 50)
	if err != nil || f != 11250 {
		t.Fatalf("Focus(15000,50) = (%d,%v), want (11250,nil)", f, err)
	}
	// Focus clamps to 500..60000.
	f, err = ApplyManaFocusAdjust(100, 99)
	if err != nil || f != 500 {
		t.Fatalf("Focus(100,99) = (%d,%v), want (500,nil)", f, err)
	}
	if _, err := ApplyRejuvenateAdjust(5000, 100); !errors.Is(err, ErrInvalidSpellPower) {
		t.Fatalf("power 100 err = %v, want ErrInvalidSpellPower", err)
	}
	// Domain errors.
	if _, err := ManaRegenIntervalMs(-1, 20, 100, 25, 0, 0, 0); !errors.Is(err, ErrInvalidVitals) {
		t.Fatalf("negative mana err = %v, want ErrInvalidVitals", err)
	}
	if _, err := ManaRegenIntervalMs(5, 20, 100, 0, 0, 0, 0); !errors.Is(err, ErrInvalidCombatStat) {
		t.Fatalf("myst 0 err = %v, want ErrInvalidCombatStat", err)
	}
}

func TestRestIntervalGoldens(t *testing.T) {
	// timeMs = 1000 + 30*(51-Stamina); no final bound in source.
	for _, tc := range []struct {
		stam, song, want int
	}{
		{1, 0, 2500},
		{25, 0, 1780},
		{50, 0, 1030},
		{70, 0, 430},
		{25, 50, 1335}, // Invigorate: (1780*150)/200
	} {
		got, err := RestIntervalMs(tc.stam, tc.song)
		if err != nil || got != tc.want {
			t.Fatalf("RestInterval(%d,%d) = (%d,%v), want (%d,nil)",
				tc.stam, tc.song, got, err, tc.want)
		}
	}
	i, err := ApplyInvigorateAdjust(2500, 99)
	if err != nil || i != 1262 { // (2500*101)/200 = 252500/200
		t.Fatalf("Invigorate(2500,99) = (%d,%v), want (1262,nil)", i, err)
	}
	if _, err := RestIntervalMs(0, 0); !errors.Is(err, ErrInvalidCombatStat) {
		t.Fatalf("stam 0 err = %v, want ErrInvalidCombatStat", err)
	}
	if _, err := RestIntervalMs(25, 100); !errors.Is(err, ErrInvalidSpellPower) {
		t.Fatalf("power 100 err = %v, want ErrInvalidSpellPower", err)
	}
}

// TestRegenProperties: intervals are positive and bounded, and monotone
// in the expected directions over randomized valid inputs.
func TestRegenProperties(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 2000; i++ {
		vigor := 1 + r.Intn(200)
		stam := 1 + r.Intn(70)
		myst := 1 + r.Intn(70)
		maxHP := 20 + r.Intn(300)
		maxMana := 1 + r.Intn(300)
		h, err := HealthRegenIntervalMs(vigor, stam, maxHP, 0, 0)
		if err != nil || h < 1000 || h > 60000 {
			t.Fatalf("health interval bounds: (%d,%v)", h, err)
		}
		hs, err := HealthRegenIntervalMs(vigor, stam, maxHP, 0, 50)
		if err != nil || hs < 670 || hs > 60000 || hs > h {
			t.Fatalf("restorate must not slow: %d vs %d (%v)", hs, h, err)
		}
		m, err := ManaRegenIntervalMs(0, maxMana, vigor, myst, 0, 0, 0)
		if err != nil || m < 1000 || m > 60000 {
			t.Fatalf("mana interval bounds: (%d,%v)", m, err)
		}
		mv, err := ManaRegenIntervalMs(maxMana+1, maxMana, vigor, myst, 0, 0, 0)
		if err != nil || mv != 30000 {
			t.Fatalf("over-max mana must be 30000: (%d,%v)", mv, err)
		}
		rt, err := RestIntervalMs(stam, 0)
		if err != nil || rt <= 0 {
			t.Fatalf("rest interval positive: (%d,%v)", rt, err)
		}
		ri, err := RestIntervalMs(stam, 50)
		if err != nil || ri > rt {
			t.Fatalf("invigorate must not slow: %d vs %d (%v)", ri, rt, err)
		}
		// Determinism.
		h2, _ := HealthRegenIntervalMs(vigor, stam, maxHP, 0, 0)
		if h != h2 {
			t.Fatalf("nondeterministic health interval")
		}
	}
}

func FuzzHealthRegenInterval(f *testing.F) {
	for _, s := range []struct {
		vigor, stam, max, faction, song int
	}{{100, 25, 40, 0, 0}, {1, 1, 20, 0, 99}, {200, 70, 500, 1000000, 0}} {
		f.Add(s.vigor, s.stam, s.max, s.faction, s.song)
	}
	f.Fuzz(func(t *testing.T, vigor, stam, max, faction, song int) {
		got, err := HealthRegenIntervalMs(vigor, stam, max, faction, song)
		valid := vigor >= 1 && vigor <= 200 && stam >= 1 && stam <= 70 &&
			max >= 20 && (song == 0 || (song >= 1 && song <= 99))
		if !valid {
			if err == nil {
				t.Fatalf("want domain err for (%d,%d,%d,%d,%d)", vigor, stam, max, faction, song)
			}
			return
		}
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		lo := 1000
		if song != 0 {
			lo = 670
		}
		if got < lo || got > 60000 {
			t.Fatalf("bounds: (%d,%d,%d,%d,%d) -> %d", vigor, stam, max, faction, song, got)
		}
	})
}

func FuzzManaRegenInterval(f *testing.F) {
	for _, s := range []struct {
		mana, max, vigor, myst, faction, rej, focus int
	}{{20, 20, 100, 25, 0, 0, 0}, {25, 20, 1, 1, 0, 99, 99}, {0, 1, 200, 70, 0, 0, 0}} {
		f.Add(s.mana, s.max, s.vigor, s.myst, s.faction, s.rej, s.focus)
	}
	f.Fuzz(func(t *testing.T, mana, max, vigor, myst, faction, rej, focus int) {
		got, err := ManaRegenIntervalMs(mana, max, vigor, myst, faction, rej, focus)
		powerOK := func(p int) bool { return p == 0 || (p >= 1 && p <= 99) }
		valid := mana >= 0 && max >= 1 && vigor >= 1 && vigor <= 200 &&
			myst >= 1 && myst <= 70 && powerOK(rej) && powerOK(focus)
		if !valid {
			if err == nil {
				t.Fatalf("want domain err")
			}
			return
		}
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if mana > max {
			if got != 30000 {
				t.Fatalf("over-max must be 30000, got %d", got)
			}
			return
		}
		if got < 500 || got > 60000 {
			t.Fatalf("bounds: got %d", got)
		}
	})
}
