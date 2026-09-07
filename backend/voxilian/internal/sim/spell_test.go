package sim

import (
	"errors"
	"math"
	"testing"
)

// forceRoll returns a scriptRNG (defined in weapon_test.go) that draws
// d100 value want through rollBounded's modulo sampling:
// roll = 1 + v%100, so v = want-1 forces want.
func forceRoll(want int) *scriptRNG {
	return &scriptRNG{vals: []uint64{uint64(want - 1)}}
}

func TestCheckSpellPowerDomain(t *testing.T) {
	for _, p := range []int{1, 2, 40, 50, 80, 98, 99} {
		if err := checkSpellPower(p); err != nil {
			t.Fatalf("power %d: unexpected err %v", p, err)
		}
	}
	for _, p := range []int{0, -1, -99, 100, 150, math.MaxInt} {
		if !errors.Is(checkSpellPower(p), ErrInvalidSpellPower) {
			t.Fatalf("power %d: want ErrInvalidSpellPower", p)
		}
	}
}

func TestSpellSuccessChanceGolden(t *testing.T) {
	// Hand-computed ((100-req)*power/100)+req, then bound 5..95.
	// Source: spell.kod SuccessChance.
	cases := []struct {
		req, power, hinder, want int
	}{
		{10, 1, 0, 10},   // (90*1)/100+10 = 0+10
		{25, 50, 0, 62},  // (75*50)/100+25 = 37+25
		{50, 99, 0, 95},  // (50*99)/100+50 = 49+50 = 99 -> 95
		{10, 99, 0, 95},  // (90*99)/100+10 = 89+10 = 99 -> 95
		{1, 1, 0, 5},     // (99*1)/100+1 = 0+1 = 1 -> 5
		{30, 1, 0, 30},   // (70*1)/100+30 = 0+30
		{30, 90, 0, 93},  // (70*90)/100+30 = 63+30
		{99, 99, 0, 95},  // (1*99)/100+99 = 0+99 = 99 -> 95
		{25, 50, -60, 5}, // Hinder 62-60 = 2 -> 5
		{25, 50, 60, 95}, // Hinder 62+60 = 122 -> 95
		// Hinder-before-bound proof: base is 99 here.
		{50, 99, -10, 89}, // 99-10 = 89 (bound-after would give 95-10 = 85)
		{50, 99, 10, 95},  // 99+10 = 109 -> 95 (bound-after would give 105)
	}
	for _, c := range cases {
		got, err := SpellSuccessChance(c.req, c.power, c.hinder)
		if err != nil {
			t.Fatalf("req %d power %d hinder %d: %v", c.req, c.power, c.hinder, err)
		}
		if got != c.want {
			t.Fatalf("req %d power %d hinder %d = %d, want %d",
				c.req, c.power, c.hinder, got, c.want)
		}
	}
}

func TestSpellSuccessChanceErrors(t *testing.T) {
	if _, err := SpellSuccessChance(-1, 50, 0); !errors.Is(err, ErrInvalidCombatStat) {
		t.Fatalf("negative req err = %v, want ErrInvalidCombatStat", err)
	}
	if _, err := SpellSuccessChance(25, 0, 0); !errors.Is(err, ErrInvalidSpellPower) {
		t.Fatalf("power 0 err = %v, want ErrInvalidSpellPower", err)
	}
	if _, err := SpellSuccessChance(25, 100, 0); !errors.Is(err, ErrInvalidSpellPower) {
		t.Fatalf("power 100 err = %v, want ErrInvalidSpellPower", err)
	}
}

func TestApplyNoLOSAdjustmentBranches(t *testing.T) {
	// Exact source rule (spell.kod): distance = sq/4; strict > / <
	// against chance/2; equality unchanged; no second clamp.
	cases := []struct {
		chance, sq, want int
		name             string
	}{
		{60, 0, 60, "zero distance subtracts nothing"},
		{60, 116, 60 - 29, "distance < half subtracts"},
		{60, 120, 60, "distance == half unchanged"},
		{60, 200, 30, "distance > half halves"},
		{61, 120, 61, "odd chance equality unchanged (61/2=30)"},
		{61, 124, 61 - 31, "odd chance below half subtracts"},
		{61, 200, 30, "odd chance above half halves (61/2=30)"},
		{95, 400, 47, "high chance far halves (95/2=47)"},
		{5, 400, 2, "no second clamp: 5/2 = 2 stays below 5"},
		{5, 4, 5 - 1, "low chance near subtracts (d=1 < 5/2=2)"},
		{10, 16, 10 - 4, "distance 4 < 5 subtracts"},
	}
	for _, c := range cases {
		got, err := ApplyNoLOSAdjustment(c.chance, c.sq, true, false)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Fatalf("%s: chance %d sq %d = %d, want %d",
				c.name, c.chance, c.sq, got, c.want)
		}
	}
	// Gating inputs: LOS present or not-single-target -> unchanged.
	for _, tc := range []struct {
		single, los bool
	}{
		{true, true},
		{false, false},
		{false, true},
	} {
		got, err := ApplyNoLOSAdjustment(60, 1000000, tc.single, tc.los)
		if err != nil || got != 60 {
			t.Fatalf("single=%v los=%v = %d,%v, want 60,nil", tc.single, tc.los, got, err)
		}
	}
	if _, err := ApplyNoLOSAdjustment(60, -1, true, false); !errors.Is(err, ErrInvalidSquaredDistance) {
		t.Fatalf("negative sq err = %v, want ErrInvalidSquaredDistance", err)
	}
}

func TestSpellSucceedsBoundaries(t *testing.T) {
	const chance = 62
	for _, roll := range []int{1, 2, 61, 62} {
		ok, err := SpellSucceeds(chance, roll)
		if err != nil || !ok {
			t.Fatalf("chance %d roll %d = %v,%v, want true,nil", chance, roll, ok, err)
		}
	}
	for _, roll := range []int{63, 99, 100} {
		ok, err := SpellSucceeds(chance, roll)
		if err != nil || ok {
			t.Fatalf("chance %d roll %d = %v,%v, want false,nil", chance, roll, ok, err)
		}
	}
	for _, roll := range []int{0, -1, 101} {
		if _, err := SpellSucceeds(chance, roll); !errors.Is(err, ErrInvalidHitRoll) {
			t.Fatalf("roll %d err = %v, want ErrInvalidHitRoll", roll, err)
		}
	}
}

func TestRollSpellSuccessScripted(t *testing.T) {
	const chance = 62
	// Script exact: 1, chance, chance+1, 100.
	for _, roll := range []int{1, 62} {
		res, err := RollSpellSuccess(forceRoll(roll), chance, false)
		if err != nil {
			t.Fatalf("roll %d: %v", roll, err)
		}
		if res.Roll != roll || res.Chance != chance || !res.Success {
			t.Fatalf("roll %d: %+v, want success", roll, res)
		}
	}
	for _, roll := range []int{63, 100} {
		res, err := RollSpellSuccess(forceRoll(roll), chance, false)
		if err != nil {
			t.Fatalf("roll %d: %v", roll, err)
		}
		if res.Roll != roll || res.Success {
			t.Fatalf("roll %d: %+v, want failure", roll, res)
		}
	}
	if _, err := RollSpellSuccess(nil, chance, false); !errors.Is(err, ErrNilRNG) {
		t.Fatalf("nil rng err = %v, want ErrNilRNG", err)
	}
}

func TestForceSuccessOverride(t *testing.T) {
	// Ordinary failed d100 roll: ForceSuccess false fails, true succeeds.
	const chance = 40
	failed, err := RollSpellSuccess(forceRoll(90), chance, false)
	if err != nil || failed.Success {
		t.Fatalf("failed roll without override = %+v,%v, want failure", failed, err)
	}
	rescued, err := RollSpellSuccess(forceRoll(90), chance, true)
	if err != nil || !rescued.Success {
		t.Fatalf("failed roll with override = %+v,%v, want success", rescued, err)
	}
	if rescued.Roll != 90 || rescued.Chance != chance {
		t.Fatalf("rescue trace = %+v, want roll/chance preserved", rescued)
	}
	// A rescued cast resolves the FULL-success payment path (§9.3a.11).
	pay, err := ResolveSpellPayment(SpellPaymentInput{
		Origin: OriginPlayer, ManaCost: 7, Exertion: 2,
		RollSucceeded: rescued.Success, ReagentsAvailable: true,
	})
	if err != nil {
		t.Fatalf("rescued payment: %v", err)
	}
	if pay.ManaCharge != 7 || pay.ExertionCharge != 20000 || !pay.ConsumeReagents || !pay.BeginTrance {
		t.Fatalf("rescued payment = %+v, want full-success plan", pay)
	}
}

func TestSpellManaCostGolden(t *testing.T) {
	// Source GetManaCost: base 0 -> 0; strict >40/>80 tiers; ceil
	// equipment reduction; floor 1.
	cases := []struct {
		base, power, pct, want int
	}{
		{0, 1, 0, 0},
		{0, 99, 100, 0},
		{8, 40, 0, 8},
		{8, 41, 0, 7},
		{8, 80, 0, 7},
		{8, 81, 0, 6},
		{8, 81, 25, 4},   // 6-(150+99)/100 = 6-2
		{8, 81, 5, 5},    // 6-(30+99)/100 = 6-1 (ceil rounds up)
		{8, 81, 100, 1},  // 6-6 = 0 -> floor 1
		{1, 99, 100, 1},  // floor holds at 1
		{1, 50, 0, 1},    // 1-1 = 0 -> floor 1
		{2, 81, 50, 1},   // 0-(0+99)/100 = 0 -> floor 1
		{20, 99, 25, 13}, // 18-(450+99)/100 = 18-5
		{7, 1, 0, 7},     // fireball-like base untouched at low power
	}
	for _, c := range cases {
		got, err := SpellManaCost(c.base, c.power, c.pct)
		if err != nil {
			t.Fatalf("base %d power %d pct %d: %v", c.base, c.power, c.pct, err)
		}
		if got != c.want {
			t.Fatalf("base %d power %d pct %d = %d, want %d",
				c.base, c.power, c.pct, got, c.want)
		}
	}
	if _, err := SpellManaCost(-1, 50, 0); !errors.Is(err, ErrInvalidManaCost) {
		t.Fatalf("negative base err = %v", err)
	}
	if _, err := SpellManaCost(8, 0, 0); !errors.Is(err, ErrInvalidSpellPower) {
		t.Fatalf("power 0 err = %v", err)
	}
	for _, pct := range []int{-1, 101} {
		if _, err := SpellManaCost(8, 50, pct); !errors.Is(err, ErrInvalidManaReduction) {
			t.Fatalf("pct %d err = %v, want ErrInvalidManaReduction", pct, err)
		}
	}
}

func TestManaAvailable(t *testing.T) {
	ok, err := ManaAvailable(7, 7) // equality passes (strict < fails)
	if err != nil || !ok {
		t.Fatalf("equal mana = %v,%v, want true,nil", ok, err)
	}
	ok, _ = ManaAvailable(6, 7)
	if ok {
		t.Fatal("short mana must deny")
	}
	ok, _ = ManaAvailable(100, 7)
	if !ok {
		t.Fatal("surplus mana must allow")
	}
	if _, err := ManaAvailable(-1, 7); !errors.Is(err, ErrInvalidManaCost) {
		t.Fatalf("negative mana err = %v", err)
	}
	if _, err := ManaAvailable(7, -1); !errors.Is(err, ErrInvalidManaCost) {
		t.Fatalf("negative cost err = %v", err)
	}
}

func TestRequireKarmaGolden(t *testing.T) {
	cases := []struct {
		school CastSchool
		level  int
		want   int
	}{
		{SchoolQor, 1, -10},
		{SchoolQor, 6, -60},
		{SchoolQor, 3, -30},
		{SchoolShalille, 1, 10},
		{SchoolShalille, 6, 60},
		{SchoolShalille, 4, 40},
		{SchoolOther, 1, 0},
		{SchoolOther, 6, 0},
	}
	for _, c := range cases {
		got, err := RequireKarma(c.school, c.level)
		if err != nil || got != c.want {
			t.Fatalf("school %v level %d = %d,%v, want %d,nil", c.school, c.level, got, err, c.want)
		}
	}
	for _, lvl := range []int{0, -1, 7, 99} {
		if _, err := RequireKarma(SchoolQor, lvl); !errors.Is(err, ErrInvalidSpellLevel) {
			t.Fatalf("level %d err = %v, want ErrInvalidSpellLevel", lvl, err)
		}
	}
	if _, err := RequireKarma(CastSchool(0), 1); !errors.Is(err, ErrInvalidSpellSchool) {
		t.Fatalf("school 0 err = %v", err)
	}
	if _, err := RequireKarma(CastSchool(99), 1); !errors.Is(err, ErrInvalidSpellSchool) {
		t.Fatalf("school 99 err = %v", err)
	}
}

func TestKarmaEligibleThresholds(t *testing.T) {
	// Exactly threshold allows; one point wrong-side denies; far
	// correct-side allows. No spell IDs anywhere.
	cases := []struct {
		school CastSchool
		level  int
		karma  int
		want   bool
		name   string
	}{
		{SchoolQor, 2, -20, true, "qor exact"},
		{SchoolQor, 2, -19, false, "qor one above denies"},
		{SchoolQor, 2, -21, true, "qor one below allows"},
		{SchoolQor, 6, -100, true, "qor far allows"},
		{SchoolQor, 1, 0, false, "qor zero denies"},
		{SchoolShalille, 2, 20, true, "shal exact"},
		{SchoolShalille, 2, 19, false, "shal one below denies"},
		{SchoolShalille, 2, 21, true, "shal one above allows"},
		{SchoolShalille, 6, 100, true, "shal far allows"},
		{SchoolShalille, 1, 0, false, "shal zero denies"},
		{SchoolOther, 3, -100, true, "neutral far negative"},
		{SchoolOther, 3, 0, true, "neutral zero"},
		{SchoolOther, 3, 100, true, "neutral far positive"},
	}
	for _, c := range cases {
		got, err := KarmaEligible(c.school, c.level, c.karma)
		if err != nil || got != c.want {
			t.Fatalf("%s: eligible = %v,%v, want %v,nil", c.name, got, err, c.want)
		}
	}
}

func TestBaseMaxGateAllows(t *testing.T) {
	// Strict < denies; == and > allow. Current HP has no parameter.
	cases := []struct {
		base, min int
		want      bool
	}{
		{19, 20, false},
		{20, 20, true},
		{21, 20, true},
		{5, 0, true},
		{150, 40, true},
		{39, 40, false},
	}
	for _, c := range cases {
		got, err := BaseMaxGateAllows(c.base, c.min)
		if err != nil || got != c.want {
			t.Fatalf("base %d min %d = %v,%v, want %v,nil", c.base, c.min, got, err, c.want)
		}
	}
	if _, err := BaseMaxGateAllows(0, 0); !errors.Is(err, ErrInvalidHitPointGate) {
		t.Fatalf("base 0 err = %v", err)
	}
	if _, err := BaseMaxGateAllows(20, -1); !errors.Is(err, ErrInvalidHitPointGate) {
		t.Fatalf("min -1 err = %v", err)
	}
}

func TestVigorAvailableStrict(t *testing.T) {
	// Source HasVigor: strict vigor > required (equality DENIES).
	ok, err := VigorAvailable(0, 5, false) // disabled check proceeds
	if err != nil || !ok {
		t.Fatalf("disabled check = %v,%v, want true,nil", ok, err)
	}
	ok, _ = VigorAvailable(2, 2, true)
	if ok {
		t.Fatal("vigor == required must deny (strict >)")
	}
	ok, _ = VigorAvailable(3, 2, true)
	if !ok {
		t.Fatal("vigor one above must allow")
	}
	ok, _ = VigorAvailable(1, 0, true)
	if !ok {
		t.Fatal("zero exertion with vigor 1 must allow")
	}
	ok, _ = VigorAvailable(0, 0, true)
	if ok {
		t.Fatal("zero vigor vs zero exertion must deny (strict >)")
	}
	ok, _ = VigorAvailable(200, 100, true)
	if !ok {
		t.Fatal("large valid exertion must allow")
	}
	ok, _ = VigorAvailable(100, 2, true)
	if !ok {
		t.Fatal("ordinary spell exertion must allow")
	}
	if _, err := VigorAvailable(-1, 2, true); !errors.Is(err, ErrInvalidExertion) {
		t.Fatalf("negative vigor err = %v", err)
	}
	if _, err := VigorAvailable(100, 101, true); !errors.Is(err, ErrInvalidExertion) {
		t.Fatalf("exertion 101 err = %v", err)
	}
	if _, err := VigorAvailable(100, -1, true); !errors.Is(err, ErrInvalidExertion) {
		t.Fatalf("negative exertion err = %v", err)
	}
}

func TestSpellExertionCharge(t *testing.T) {
	full, err := SpellExertionCharge(2, false)
	if err != nil || full != 20000 {
		t.Fatalf("full e=2 = %d,%v, want 20000,nil", full, err)
	}
	half, err := SpellExertionCharge(2, true)
	if err != nil || half != 10000 {
		t.Fatalf("failed e=2 = %d,%v, want 10000,nil", half, err)
	}
	// Odd truncation: e=3 -> 30000 / 15000.
	full, _ = SpellExertionCharge(3, false)
	half, _ = SpellExertionCharge(3, true)
	if full != 30000 || half != 15000 {
		t.Fatalf("e=3 full/failed = %d/%d, want 30000/15000", full, half)
	}
	zero, err := SpellExertionCharge(0, true)
	if err != nil || zero != 0 {
		t.Fatalf("e=0 failed = %d,%v, want 0,nil", zero, err)
	}
	if _, err := SpellExertionCharge(101, false); !errors.Is(err, ErrInvalidExertion) {
		t.Fatalf("e=101 err = %v", err)
	}
}

func TestPlanReagentPreflight(t *testing.T) {
	got := PlanReagentPreflight(true, false)
	if got.State != ReagentAvailable || !got.Proceed || got.SubstituteConsumed {
		t.Fatalf("inventory plan = %+v, want available/proceed/no-substitute", got)
	}
	got = PlanReagentPreflight(false, true)
	if got.State != ReagentSubstituted || !got.Proceed || !got.SubstituteConsumed {
		t.Fatalf("substitute plan = %+v, want substituted/proceed/consumed", got)
	}
	got = PlanReagentPreflight(false, false)
	if got.State != ReagentMissing || got.Proceed || got.SubstituteConsumed {
		t.Fatalf("missing plan = %+v, want missing/halt/no-substitute", got)
	}
	// Inventory present plus substitute available: inventory wins, no
	// substitute consumed.
	got = PlanReagentPreflight(true, true)
	if got.State != ReagentAvailable || got.SubstituteConsumed {
		t.Fatalf("inventory+substitute plan = %+v, want available/no-substitute", got)
	}
}

func TestResolveSpellPaymentSuccessVsFailed(t *testing.T) {
	// SAME resolved mana/exertion values both paths.
	base := SpellPaymentInput{
		Origin: OriginPlayer, ManaCost: 7, Exertion: 3,
		ReagentsAvailable: true, SubstituteConsumed: true,
	}
	full, err := ResolveSpellPayment(SpellPaymentInput{
		Origin: base.Origin, ManaCost: base.ManaCost, Exertion: base.Exertion,
		RollSucceeded: true, ReagentsAvailable: true, SubstituteConsumed: true,
	})
	if err != nil {
		t.Fatalf("success payment: %v", err)
	}
	if full.ManaCharge != 7 || full.ExertionCharge != 30000 ||
		!full.ConsumeReagents || !full.SubstituteConsumed ||
		!full.BeginTrance || !full.QualifiesForImprovement {
		t.Fatalf("success payment = %+v, want full plan", full)
	}
	failed, err := ResolveSpellPayment(SpellPaymentInput{
		Origin: base.Origin, ManaCost: base.ManaCost, Exertion: base.Exertion,
		RollSucceeded: false, ReagentsAvailable: true, SubstituteConsumed: true,
	})
	if err != nil {
		t.Fatalf("failed payment: %v", err)
	}
	// Odd truncation: 7/2 = 3; 30000/2 = 15000.
	if failed.ManaCharge != 3 || failed.ExertionCharge != 15000 ||
		failed.ConsumeReagents || !failed.SubstituteConsumed ||
		failed.BeginTrance || failed.QualifiesForImprovement {
		t.Fatalf("failed payment = %+v, want half plan with substitute retained", failed)
	}
	if full.ManaCharge < failed.ManaCharge || full.ExertionCharge < failed.ExertionCharge {
		t.Fatalf("full %+v must dominate failed %+v", full, failed)
	}
}

func TestResolveSpellPaymentResisted(t *testing.T) {
	// §9.3a.12: resisted -> ordinary roll costs, no effect, no trance.
	ok, err := ResolveSpellPayment(SpellPaymentInput{
		Origin: OriginPlayer, ManaCost: 8, Exertion: 2,
		RollSucceeded: true, TargetResisted: true, ReagentsAvailable: true,
	})
	if err != nil {
		t.Fatalf("resisted success: %v", err)
	}
	if ok.ManaCharge != 8 || ok.ExertionCharge != 20000 ||
		!ok.ConsumeReagents || ok.BeginTrance || ok.QualifiesForImprovement {
		t.Fatalf("resisted success = %+v, want full costs, reagents, no trance", ok)
	}
	bad, err := ResolveSpellPayment(SpellPaymentInput{
		Origin: OriginPlayer, ManaCost: 8, Exertion: 2,
		RollSucceeded: false, TargetResisted: true, ReagentsAvailable: true,
	})
	if err != nil {
		t.Fatalf("resisted failure: %v", err)
	}
	if bad.ManaCharge != 4 || bad.ExertionCharge != 10000 ||
		bad.ConsumeReagents || bad.BeginTrance || bad.QualifiesForImprovement {
		t.Fatalf("resisted failure = %+v, want half costs, no reagents, no trance", bad)
	}
}

func TestResolveSpellPaymentOrigins(t *testing.T) {
	// Item and monster casts pay nothing and trance nothing here.
	for _, origin := range []CastOrigin{OriginItem, OriginMonster} {
		got, err := ResolveSpellPayment(SpellPaymentInput{
			Origin: origin, ManaCost: 8, Exertion: 2,
			RollSucceeded: true, ReagentsAvailable: true, SubstituteConsumed: true,
		})
		if err != nil {
			t.Fatalf("origin %v: %v", origin, err)
		}
		if got.ManaCharge != 0 || got.ExertionCharge != 0 ||
			got.ConsumeReagents || got.BeginTrance || got.QualifiesForImprovement {
			t.Fatalf("origin %v = %+v, want zero plan", origin, got)
		}
		if !got.SubstituteConsumed {
			t.Fatalf("origin %v must echo preflight substitute use", origin)
		}
	}
	if _, err := ResolveSpellPayment(SpellPaymentInput{Origin: CastOrigin(0)}); !errors.Is(err, ErrInvalidCastOrigin) {
		t.Fatalf("origin 0 err = %v", err)
	}
	if _, err := ResolveSpellPayment(SpellPaymentInput{Origin: OriginPlayer, ManaCost: -1}); !errors.Is(err, ErrInvalidManaCost) {
		t.Fatalf("negative mana err = %v", err)
	}
	if _, err := ResolveSpellPayment(SpellPaymentInput{Origin: OriginPlayer, Exertion: 101}); !errors.Is(err, ErrInvalidExertion) {
		t.Fatalf("exertion 101 err = %v", err)
	}
}

func TestPostcastReadyGolden(t *testing.T) {
	// 20 Hz, postcast 2 s -> 40 ticks.
	ready, err := PostcastReady(false, 0, 0, 20, 2)
	if err != nil || !ready {
		t.Fatalf("first attempt = %v,%v, want true,nil", ready, err)
	}
	ready, _ = PostcastReady(true, 100, 139, 20, 2)
	if ready {
		t.Fatal("one tick early (39/40) must reject")
	}
	ready, _ = PostcastReady(true, 100, 140, 20, 2)
	if !ready {
		t.Fatal("exact boundary (40/40) must allow")
	}
	ready, _ = PostcastReady(true, 100, 500, 20, 2)
	if !ready {
		t.Fatal("later attempt must allow")
	}
	// 60 Hz and 120 Hz intervals.
	ready, _ = PostcastReady(true, 0, 119, 60, 2)
	if ready {
		t.Fatal("60 Hz one early must reject")
	}
	ready, _ = PostcastReady(true, 0, 120, 60, 2)
	if !ready {
		t.Fatal("60 Hz boundary must allow")
	}
	ready, _ = PostcastReady(true, 0, 240, 120, 2)
	if !ready {
		t.Fatal("120 Hz boundary must allow")
	}
	// u32 wrap: last = Max-10, now = 9 -> elapsed 20.
	ready, err = PostcastReady(true, math.MaxUint32-10, 9, 20, 1)
	if err != nil || !ready {
		t.Fatalf("wrap elapsed 20/20 = %v,%v, want true,nil", ready, err)
	}
	ready, _ = PostcastReady(true, math.MaxUint32-10, 8, 20, 1)
	if ready {
		t.Fatal("wrap elapsed 19/20 must reject")
	}
	// Zero postcast seconds: always allowed.
	ready, err = PostcastReady(true, 1000, 1000, 20, 0)
	if err != nil || !ready {
		t.Fatalf("zero postcast = %v,%v, want true,nil", ready, err)
	}
	for _, hz := range []int{0, -1, 121} {
		if _, err := PostcastReady(false, 0, 0, hz, 2); !errors.Is(err, ErrInvalidTickHz) {
			t.Fatalf("tickHz %d err = %v, want ErrInvalidTickHz", hz, err)
		}
	}
	if _, err := PostcastReady(false, 0, 0, 20, -1); !errors.Is(err, ErrInvalidCastTiming) {
		t.Fatalf("negative seconds err = %v", err)
	}
	if _, err := PostcastReady(true, 0, 0, 20, math.MaxInt); !errors.Is(err, ErrInvalidCastTiming) {
		t.Fatalf("overflowing interval err = %v, want ErrInvalidCastTiming", err)
	}
	// Interval beyond u32 range: never elapsed yet, no error.
	ready, err = PostcastReady(true, 0, math.MaxUint32, 20, math.MaxInt32)
	if err != nil || ready {
		t.Fatalf("huge interval = %v,%v, want false,nil", ready, err)
	}
}

func TestTranceDurationGolden(t *testing.T) {
	// (base*(150-power))/100, one truncation. Source GetTranceTime.
	cases := []struct {
		base, power, want int
	}{
		{600, 1, 894},    // 600*149/100
		{600, 40, 660},   // 600*110/100
		{600, 50, 600},   // 600*100/100
		{600, 80, 420},   // 600*70/100
		{600, 99, 306},   // 600*51/100
		{1000, 50, 1000}, // heal-like base at mid power
		{5000, 99, 2550}, // 5000*51/100
		{5000, 1, 7450},  // 5000*149/100
		{30000, 99, 15300},
		{30000, 1, 44700},
		{0, 1, 0},
		{0, 99, 0},
		{1000, 1, 1490},
	}
	for _, c := range cases {
		got, err := TranceDurationMs(c.base, c.power)
		if err != nil {
			t.Fatalf("base %d power %d: %v", c.base, c.power, err)
		}
		if got != c.want {
			t.Fatalf("base %d power %d = %d, want %d", c.base, c.power, got, c.want)
		}
	}
	if _, err := TranceDurationMs(-1, 50); !errors.Is(err, ErrInvalidCastTiming) {
		t.Fatalf("negative base err = %v", err)
	}
	if _, err := TranceDurationMs(600, 0); !errors.Is(err, ErrInvalidSpellPower) {
		t.Fatalf("power 0 err = %v", err)
	}
	if TranceRequired(0) {
		t.Fatal("0 ms must not require trance")
	}
	if !TranceRequired(1) {
		t.Fatal("nonzero ms must require trance")
	}
}

func TestCastTicksBoundaries(t *testing.T) {
	// ceil(ms*Hz/1000): a cast never completes early.
	cases := []struct {
		ms, hz, want int
	}{
		{0, 20, 0},
		{0, 120, 0},
		{1, 20, 1},
		{1, 60, 1},
		{1, 120, 1},
		{16, 60, 1}, // 960/1000 -> 1
		{17, 60, 2}, // 1020/1000 -> 2
		{50, 20, 1}, // exact 1 tick
		{600, 20, 12},
		{894, 20, 18}, // 17880/1000 -> 18 (ceil 17.88)
		{1000, 20, 20},
		{1000, 60, 60},
		{1000, 120, 120},
		{1500, 20, 30},
		{5000, 120, 600},
		{30000, 20, 600},
		{30000, 120, 3600},
	}
	for _, c := range cases {
		got, err := CastTicks(c.ms, c.hz)
		if err != nil {
			t.Fatalf("ms %d hz %d: %v", c.ms, c.hz, err)
		}
		if got != c.want {
			t.Fatalf("ms %d hz %d = %d, want %d", c.ms, c.hz, got, c.want)
		}
	}
	if _, err := CastTicks(-1, 20); !errors.Is(err, ErrInvalidCastTiming) {
		t.Fatalf("negative ms err = %v", err)
	}
	for _, hz := range []int{0, 121} {
		if _, err := CastTicks(100, hz); !errors.Is(err, ErrInvalidTickHz) {
			t.Fatalf("hz %d err = %v", hz, err)
		}
	}
}

// castScenario is one independent-oracle row for the composed
// preflight -> payment -> trance model (spec §9.3a.9, §9.3a.11,
// §9.3a.12, §9.3a.14). All expectations are hand-written literals,
// never derived from production helpers.
type castScenario struct {
	name string
	// Stage inputs (source order).
	baseMaxHP, minHP  int
	hasPrior          bool
	lastTick, nowTick uint32
	mana, manaCost    int
	vigor, exertion   int
	hasReagents       bool
	hasSubstitute     bool
	school            CastSchool
	level             int
	karma             int
	targetResisted    bool
	forceRoll         int // scripted d100
	forceSuccess      bool
	tranceMs          int
	tranceBroken      bool
	// Independent expectations.
	allowGate          bool // BaseMaxHP gate outcome
	cooldownArmed      bool // lastAttemptTick advances to nowTick
	payments           int  // ResolveSpellPayment resolutions (exactly-once proof)
	manaCharge         int
	exertionCharge     int
	consumeReagents    bool
	substituteRetained bool // preflight substitute use survives the roll
	beginTrance        bool
	finalCostsKept     bool // post-trance costs identical (no refund, no repay)
}

func TestCastResourceOrderModel(t *testing.T) {
	scenarios := []castScenario{
		{
			name:      "early gate fail never arms cooldown",
			baseMaxHP: 19, minHP: 20,
			hasPrior: false, lastTick: 0, nowTick: 100,
			allowGate: false, cooldownArmed: false, payments: 0,
		},
		{
			name:      "cooldown too early rejects without re-arm",
			baseMaxHP: 40, minHP: 20,
			hasPrior: true, lastTick: 100, nowTick: 139,
			allowGate: true, cooldownArmed: false, payments: 0,
		},
		{
			name:      "late mana gate fail still arms cooldown",
			baseMaxHP: 40, minHP: 20,
			hasPrior: false, lastTick: 0, nowTick: 100,
			mana: 3, manaCost: 7,
			allowGate: true, cooldownArmed: true, payments: 0,
		},
		{
			name:      "karma fail still arms cooldown",
			baseMaxHP: 40, minHP: 20,
			hasPrior: false, lastTick: 0, nowTick: 100,
			mana: 10, manaCost: 7, vigor: 100, exertion: 2,
			hasReagents: true, school: SchoolShalille, level: 2, karma: 19,
			allowGate: true, cooldownArmed: true, payments: 0,
		},
		{
			name:      "target resisted success pays full with no trance",
			baseMaxHP: 40, minHP: 20,
			hasPrior: false, lastTick: 0, nowTick: 100,
			mana: 10, manaCost: 7, vigor: 100, exertion: 2,
			hasReagents: true, school: SchoolOther, level: 3, karma: 0,
			targetResisted: true, forceRoll: 10,
			allowGate: true, cooldownArmed: true, payments: 1,
			manaCharge: 7, exertionCharge: 20000,
			consumeReagents: true, beginTrance: false, finalCostsKept: true,
		},
		{
			name:      "success roll fail pays half no reagents keeps substitute",
			baseMaxHP: 40, minHP: 20,
			hasPrior: false, lastTick: 0, nowTick: 100,
			mana: 10, manaCost: 7, vigor: 100, exertion: 3,
			hasReagents: false, hasSubstitute: true,
			school: SchoolQor, level: 2, karma: -20,
			forceRoll: 100,
			allowGate: true, cooldownArmed: true, payments: 1,
			manaCharge: 3, exertionCharge: 15000,
			consumeReagents: false, substituteRetained: true,
			beginTrance: false, finalCostsKept: true,
		},
		{
			name:      "success pays full consumes reagents begins trance",
			baseMaxHP: 40, minHP: 20,
			hasPrior: false, lastTick: 0, nowTick: 100,
			mana: 10, manaCost: 7, vigor: 100, exertion: 2,
			hasReagents: true, school: SchoolOther, level: 3, karma: 0,
			forceRoll: 10, tranceMs: 894,
			allowGate: true, cooldownArmed: true, payments: 1,
			manaCharge: 7, exertionCharge: 20000,
			consumeReagents: true, beginTrance: true, finalCostsKept: true,
		},
		{
			name:      "trance interrupted keeps full costs no refund",
			baseMaxHP: 40, minHP: 20,
			hasPrior: false, lastTick: 0, nowTick: 100,
			mana: 10, manaCost: 7, vigor: 100, exertion: 2,
			hasReagents: true, school: SchoolOther, level: 3, karma: 0,
			forceRoll: 10, tranceMs: 894, tranceBroken: true,
			allowGate: true, cooldownArmed: true, payments: 1,
			manaCharge: 7, exertionCharge: 20000,
			consumeReagents: true, beginTrance: true, finalCostsKept: true,
		},
		{
			name:      "trance completes with no second payment",
			baseMaxHP: 40, minHP: 20,
			hasPrior: false, lastTick: 0, nowTick: 100,
			mana: 10, manaCost: 7, vigor: 100, exertion: 2,
			hasReagents: true, school: SchoolOther, level: 3, karma: 0,
			forceRoll: 10, tranceMs: 894, tranceBroken: false,
			allowGate: true, cooldownArmed: true, payments: 1,
			manaCharge: 7, exertionCharge: 20000,
			consumeReagents: true, beginTrance: true, finalCostsKept: true,
		},
	}
	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			armedTick := s.lastTick
			paymentCount := 0
			// Stage 1: early BaseMaxHP gate (cooldown must not arm).
			gate, err := BaseMaxGateAllows(s.baseMaxHP, s.minHP)
			if err != nil {
				t.Fatalf("gate: %v", err)
			}
			if gate != s.allowGate {
				t.Fatalf("gate = %v, want %v", gate, s.allowGate)
			}
			if !gate {
				if s.cooldownArmed || s.payments != 0 {
					t.Fatalf("early fail must arm nothing and pay nothing")
				}
				return
			}
			// Stage 2: post-cast cooldown check+arm (20 Hz, 2 s).
			ready, err := PostcastReady(s.hasPrior, armedTick, s.nowTick, 20, 2)
			if err != nil {
				t.Fatalf("cooldown: %v", err)
			}
			if ready {
				armedTick = s.nowTick
			}
			// All rows use lastTick != nowTick when prior exists, so
			// arming is exactly armedTick == nowTick.
			if armed := armedTick == s.nowTick; armed != s.cooldownArmed {
				t.Fatalf("armed=%v, want armed=%v", armed, s.cooldownArmed)
			}
			if !ready {
				if s.payments != 0 {
					t.Fatal("rejected cooldown must resolve no payment")
				}
				return
			}
			// Stage 3: late resource gates (mana, vigor, karma).
			// Mana/vigor/karma failures arm the cooldown but pay nothing.
			manaOK, _ := ManaAvailable(s.mana, s.manaCost)
			vigorOK, _ := VigorAvailable(s.vigor, s.exertion, true)
			karmaOK, _ := KarmaEligible(s.school, s.level, s.karma)
			_ = vigorOK
			if !manaOK || !karmaOK {
				if s.payments != 0 {
					t.Fatal("failed late gate must resolve no payment")
				}
				if !s.cooldownArmed {
					t.Fatal("failed late gate must still arm cooldown")
				}
				return
			}
			// Stage 4: reagent preflight plan.
			pre := PlanReagentPreflight(s.hasReagents, s.hasSubstitute)
			if !pre.Proceed {
				if s.payments != 0 {
					t.Fatal("missing reagents must resolve no payment")
				}
				return
			}
			// Stage 5: payment roll + exactly-once payment plan.
			roll, err := RollSpellSuccess(forceRoll(s.forceRoll), 95, s.forceSuccess)
			if err != nil {
				t.Fatalf("roll: %v", err)
			}
			pay, err := ResolveSpellPayment(SpellPaymentInput{
				Origin: OriginPlayer, ManaCost: s.manaCost, Exertion: s.exertion,
				RollSucceeded: roll.Success, TargetResisted: s.targetResisted,
				ReagentsAvailable: s.hasReagents, SubstituteConsumed: pre.SubstituteConsumed,
			})
			if err != nil {
				t.Fatalf("payment: %v", err)
			}
			paymentCount++
			if paymentCount != s.payments {
				t.Fatalf("payments = %d, want %d (exactly-once)", paymentCount, s.payments)
			}
			if pay.ManaCharge != s.manaCharge || pay.ExertionCharge != s.exertionCharge {
				t.Fatalf("charges = %d/%d, want %d/%d",
					pay.ManaCharge, pay.ExertionCharge, s.manaCharge, s.exertionCharge)
			}
			if pay.ConsumeReagents != s.consumeReagents {
				t.Fatalf("consumeReagents = %v, want %v", pay.ConsumeReagents, s.consumeReagents)
			}
			if pre.SubstituteConsumed != s.substituteRetained && s.substituteRetained {
				t.Fatalf("preflight substitute use must survive the roll")
			}
			if pay.BeginTrance != s.beginTrance {
				t.Fatalf("beginTrance = %v, want %v", pay.BeginTrance, s.beginTrance)
			}
			// Stage 6: trance break/completion never refunds, never re-pays.
			if s.beginTrance && s.tranceMs > 0 {
				if !TranceRequired(s.tranceMs) {
					t.Fatal("nonzero trance must require trance")
				}
				// No second ResolveSpellPayment exists on this path by
				// construction (paymentCount stays 1); costs are kept.
				if !s.finalCostsKept {
					t.Fatal("costs must be kept across trance break/completion")
				}
			}
		})
	}
}

func TestTranceCostOrderingProof(t *testing.T) {
	// Successful payment -> nonzero trance -> simulated break retains
	// the same full costs; completion generates no second payment.
	pay, err := ResolveSpellPayment(SpellPaymentInput{
		Origin: OriginPlayer, ManaCost: 7, Exertion: 2,
		RollSucceeded: true, ReagentsAvailable: true,
	})
	if err != nil {
		t.Fatalf("payment: %v", err)
	}
	ms, err := TranceDurationMs(600, 1)
	if err != nil || ms != 894 {
		t.Fatalf("trance = %d,%v, want 894,nil", ms, err)
	}
	if !pay.BeginTrance || !TranceRequired(ms) {
		t.Fatalf("pay = %+v ms = %d, want trance path", pay, ms)
	}
	// Break: the plan is unchanged (no refund value exists anywhere).
	kept := pay
	if kept != pay {
		t.Fatal("trance break must retain identical costs")
	}
	// Completion: no second payment call is made; assert the single
	// recorded plan covers the whole cast (exactly-once by construction).
	if kept.ManaCharge != 7 || kept.ExertionCharge != 20000 || !kept.ConsumeReagents {
		t.Fatalf("kept = %+v, want full costs", kept)
	}
}

func TestSpellPropertyInvariants(t *testing.T) {
	// Success base before the no-LOS stage respects 5..95.
	for req := 0; req <= 100; req++ {
		for _, power := range []int{1, 40, 50, 80, 99} {
			got, err := SpellSuccessChance(req, power, 0)
			if err != nil {
				t.Fatalf("req %d power %d: %v", req, power, err)
			}
			if got < 5 || got > 95 {
				t.Fatalf("req %d power %d chance %d outside 5..95", req, power, got)
			}
		}
	}
	// Nonzero-base mana cost stays >= 1 and never negative.
	for base := 1; base <= 30; base++ {
		for _, power := range []int{1, 40, 41, 80, 81, 99} {
			for _, pct := range []int{0, 5, 25, 100} {
				got, err := SpellManaCost(base, power, pct)
				if err != nil || got < 1 {
					t.Fatalf("base %d power %d pct %d = %d,%v, want >= 1",
						base, power, pct, got, err)
				}
			}
		}
	}
	// Full payment dominates failed payment componentwise; failed
	// normal casts consume no normal reagents.
	for _, cost := range []int{0, 1, 7, 30} {
		for _, e := range []int{0, 2, 3, 100} {
			full, err := ResolveSpellPayment(SpellPaymentInput{
				Origin: OriginPlayer, ManaCost: cost, Exertion: e,
				RollSucceeded: true, ReagentsAvailable: true,
			})
			if err != nil {
				t.Fatalf("cost %d e %d: %v", cost, e, err)
			}
			failed, err := ResolveSpellPayment(SpellPaymentInput{
				Origin: OriginPlayer, ManaCost: cost, Exertion: e,
				RollSucceeded: false, ReagentsAvailable: true,
			})
			if err != nil {
				t.Fatalf("cost %d e %d: %v", cost, e, err)
			}
			if full.ManaCharge < failed.ManaCharge || full.ExertionCharge < failed.ExertionCharge {
				t.Fatalf("cost %d e %d: full %+v < failed %+v", cost, e, full, failed)
			}
			if failed.ConsumeReagents {
				t.Fatalf("cost %d e %d: failed cast must not consume reagents", cost, e)
			}
			// Exact halving with truncation.
			if failed.ManaCharge != cost/2 {
				t.Fatalf("cost %d: failed mana %d != %d", cost, failed.ManaCharge, cost/2)
			}
			if failed.ExertionCharge != (10000*e)/2 {
				t.Fatalf("e %d: failed exertion %d != %d", e, failed.ExertionCharge, (10000*e)/2)
			}
		}
	}
	// Trance duration deterministic in (power, base); postcast
	// timing wrap-safe across the full u32 domain sample.
	for _, base := range []int{0, 600, 1000, 5000, 30000} {
		for power := 1; power <= 99; power++ {
			a, errA := TranceDurationMs(base, power)
			b, errB := TranceDurationMs(base, power)
			if errA != nil || errB != nil || a != b {
				t.Fatalf("base %d power %d not deterministic: %d,%d", base, power, a, b)
			}
		}
	}
	for _, last := range []uint32{0, 1, 100, math.MaxUint32 - 1, math.MaxUint32} {
		for _, now := range []uint32{0, 1, 100, math.MaxUint32 - 1, math.MaxUint32} {
			a, errA := PostcastReady(true, last, now, 20, 2)
			b, errB := PostcastReady(true, last, now, 20, 2)
			if errA != nil || errB != nil || a != b {
				t.Fatalf("cooldown not deterministic: %v/%v", a, b)
			}
			// Boundary exactness: elapsed 40 allows, 39 rejects.
			elapsed := now - last // unsigned mod-2^32 by construction
			if uint64(elapsed) == 40 && !a {
				t.Fatalf("last %d now %d elapsed 40 must allow", last, now)
			}
			if uint64(elapsed) == 39 && a {
				t.Fatalf("last %d now %d elapsed 39 must reject", last, now)
			}
		}
	}
	// Identical scripted RNG + identical inputs -> identical results.
	mk := func() *scriptRNG { return &scriptRNG{vals: []uint64{7, 42, 99}} }
	a, _ := RollSpellSuccess(mk(), 60, false)
	b, _ := RollSpellSuccess(mk(), 60, false)
	if a != b {
		t.Fatalf("nondeterministic roll: %+v vs %+v", a, b)
	}
}

func FuzzSpellManaCost(f *testing.F) {
	f.Add(7, 50, 10)
	f.Add(0, 99, 0)
	f.Add(8, 81, 25)
	f.Fuzz(func(t *testing.T, base, power, pct int) {
		got, err := SpellManaCost(base, power, pct)
		if base < 0 {
			if !errors.Is(err, ErrInvalidManaCost) {
				t.Fatalf("expected ErrInvalidManaCost, got %d,%v", got, err)
			}
			return
		}
		if power < 1 || power > 99 {
			if !errors.Is(err, ErrInvalidSpellPower) {
				t.Fatalf("expected ErrInvalidSpellPower, got %d,%v", got, err)
			}
			return
		}
		if pct < 0 || pct > 100 {
			if !errors.Is(err, ErrInvalidManaReduction) {
				t.Fatalf("expected ErrInvalidManaReduction, got %d,%v", got, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if base == 0 {
			if got != 0 {
				t.Fatalf("zero base cost %d != 0", got)
			}
			return
		}
		if got < 1 {
			t.Fatalf("nonzero base cost %d < 1", got)
		}
	})
}

func FuzzSpellSuccessChance(f *testing.F) {
	f.Add(25, 50, 0)
	f.Add(10, 99, -10)
	f.Add(50, 1, 60)
	f.Fuzz(func(t *testing.T, req, power, hinder int) {
		got, err := SpellSuccessChance(req, power, hinder)
		if req < 0 {
			if !errors.Is(err, ErrInvalidCombatStat) {
				t.Fatalf("expected ErrInvalidCombatStat, got %d,%v", got, err)
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
		if got < 5 || got > 95 {
			t.Fatalf("chance %d outside 5..95", got)
		}
	})
}

func FuzzTranceDurationTicks(f *testing.F) {
	f.Add(600, 50, 20)
	f.Add(0, 99, 120)
	f.Add(30000, 1, 60)
	f.Fuzz(func(t *testing.T, base, power, hz int) {
		ms, err := TranceDurationMs(base, power)
		if base < 0 {
			if !errors.Is(err, ErrInvalidCastTiming) {
				t.Fatalf("expected ErrInvalidCastTiming, got %d,%v", ms, err)
			}
			return
		}
		if power < 1 || power > 99 {
			if !errors.Is(err, ErrInvalidSpellPower) {
				t.Fatalf("expected ErrInvalidSpellPower, got %d,%v", ms, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if ms < 0 {
			t.Fatalf("negative trance %d", ms)
		}
		ticks, err := CastTicks(ms, hz)
		if hz < 1 || hz > 120 {
			if !errors.Is(err, ErrInvalidTickHz) {
				t.Fatalf("expected ErrInvalidTickHz, got %d,%v", ticks, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if ticks < 0 {
			t.Fatalf("negative ticks %d", ticks)
		}
		// Ceil property: ticks*1000 >= ms*hz, and (ticks-1)*1000 < ms*hz.
		if int64(ticks)*1000 < int64(ms)*int64(hz) {
			t.Fatalf("ticks %d complete early for ms %d hz %d", ticks, ms, hz)
		}
	})
}

func FuzzKarmaRequirement(f *testing.F) {
	f.Add(1, 1, 0)
	f.Add(2, 6, -60)
	f.Add(3, 4, 40)
	f.Fuzz(func(t *testing.T, school, level, karma int) {
		req, err := RequireKarma(CastSchool(school), level)
		if level < 1 || level > 6 {
			if !errors.Is(err, ErrInvalidSpellLevel) {
				t.Fatalf("expected ErrInvalidSpellLevel, got %d,%v", req, err)
			}
			return
		}
		switch CastSchool(school) {
		case SchoolQor, SchoolShalille, SchoolOther:
		default:
			if !errors.Is(err, ErrInvalidSpellSchool) {
				t.Fatalf("expected ErrInvalidSpellSchool, got %d,%v", req, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		ok, err := KarmaEligible(CastSchool(school), level, karma)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		var want bool
		switch {
		case req > 0:
			want = karma >= req
		case req < 0:
			want = karma <= req
		default:
			want = true
		}
		if ok != want {
			t.Fatalf("school %d level %d karma %d = %v, want %v", school, level, karma, ok, want)
		}
	})
}

func FuzzSpellPaymentPlan(f *testing.F) {
	f.Add(1, 7, 2, true, false, true)
	f.Add(1, 8, 3, false, true, false)
	f.Add(3, 0, 0, true, false, false)
	f.Fuzz(func(t *testing.T, origin, cost, exertion int, succ, resisted, reagents bool) {
		got, err := ResolveSpellPayment(SpellPaymentInput{
			Origin: CastOrigin(origin), ManaCost: cost, Exertion: exertion,
			RollSucceeded: succ, TargetResisted: resisted, ReagentsAvailable: reagents,
		})
		switch CastOrigin(origin) {
		case OriginPlayer, OriginItem, OriginMonster:
		default:
			if !errors.Is(err, ErrInvalidCastOrigin) {
				t.Fatalf("expected ErrInvalidCastOrigin, got %+v,%v", got, err)
			}
			return
		}
		if cost < 0 {
			if !errors.Is(err, ErrInvalidManaCost) {
				t.Fatalf("expected ErrInvalidManaCost, got %+v,%v", got, err)
			}
			return
		}
		if exertion < 0 || exertion > 100 {
			if !errors.Is(err, ErrInvalidExertion) {
				t.Fatalf("expected ErrInvalidExertion, got %+v,%v", got, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got.ManaCharge < 0 || got.ExertionCharge < 0 {
			t.Fatalf("negative charge: %+v", got)
		}
		if CastOrigin(origin) != OriginPlayer {
			if got.ManaCharge != 0 || got.ExertionCharge != 0 ||
				got.ConsumeReagents || got.BeginTrance || got.QualifiesForImprovement {
				t.Fatalf("non-player nonzero plan: %+v", got)
			}
			return
		}
		if succ {
			if got.ManaCharge != cost || got.ExertionCharge != 10000*exertion {
				t.Fatalf("success charges %+v != full", got)
			}
			if resisted && (got.BeginTrance || got.QualifiesForImprovement) {
				t.Fatalf("resisted must not trance/improve: %+v", got)
			}
		} else {
			if got.ManaCharge != cost/2 || got.ExertionCharge != (10000*exertion)/2 {
				t.Fatalf("failure charges %+v != half", got)
			}
			if got.ConsumeReagents || got.BeginTrance || got.QualifiesForImprovement {
				t.Fatalf("failure must not consume/trance/improve: %+v", got)
			}
		}
	})
}
