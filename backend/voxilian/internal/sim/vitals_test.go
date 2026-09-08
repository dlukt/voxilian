package sim

import (
	"encoding/json"
	"errors"
	"math"
	"math/rand"
	"testing"
)

// mustVitals returns a valid canonical vitals value for tests
// (creation state at Mysticism 25: mana 20).
func mustVitals(t *testing.T) PlayerVitals {
	t.Helper()
	v, err := NewPlayerVitals(25)
	if err != nil {
		t.Fatalf("NewPlayerVitals(25): unexpected err %v", err)
	}
	return v
}

func TestVitalsCreationGoldens(t *testing.T) {
	for _, tc := range []struct {
		myst int
		mana int
	}{
		{1, 15}, {5, 16}, {25, 20}, {50, 25},
	} {
		m, err := InitialMaxMana(tc.myst)
		if err != nil {
			t.Fatalf("InitialMaxMana(%d): unexpected err %v", tc.myst, err)
		}
		if m != tc.mana {
			t.Fatalf("InitialMaxMana(%d) = %d, want %d", tc.myst, m, tc.mana)
		}
		v, err := NewPlayerVitals(tc.myst)
		if err != nil {
			t.Fatalf("NewPlayerVitals(%d): unexpected err %v", tc.myst, err)
		}
		want := PlayerVitals{
			HP: 20, BaseMaxHP: 20, MaxHP: 20,
			Mana: tc.mana, MaxMana: tc.mana,
			Vigor: 100, RestThreshold: 80, Exertion: 0, Stomach: 0,
		}
		if v != want {
			t.Fatalf("NewPlayerVitals(%d) = %+v, want %+v", tc.myst, v, want)
		}
		if err := v.Validate(); err != nil {
			t.Fatalf("NewPlayerVitals(%d) invalid: %v", tc.myst, err)
		}
	}
	for _, bad := range []int{0, -3, 71, 100} {
		if _, err := NewPlayerVitals(bad); !errors.Is(err, ErrInvalidCombatStat) {
			t.Fatalf("NewPlayerVitals(%d) err = %v, want ErrInvalidCombatStat", bad, err)
		}
		if _, err := InitialMaxMana(bad); !errors.Is(err, ErrInvalidCombatStat) {
			t.Fatalf("InitialMaxMana(%d) err = %v, want ErrInvalidCombatStat", bad, err)
		}
	}
}

// TestVitalsJSONCompatibility proves the durable contract (spec §9.4.2):
// the exact creation shape (no exertion field) decodes with Exertion 0
// and validates, and a new encode/decode round-trip preserves a non-zero
// residual. Literal fixture: no import of internal/character, no cycle.
func TestVitalsJSONCompatibility(t *testing.T) {
	old := `{"hp":20,"base_max":20,"max":20,"mana":20,"max_mana":20,` +
		`"vigor":100,"threshold":80,"stomach":0}`
	var v PlayerVitals
	if err := json.Unmarshal([]byte(old), &v); err != nil {
		t.Fatalf("unmarshal creation shape: %v", err)
	}
	if v.Exertion != 0 {
		t.Fatalf("missing exertion decoded as %d, want 0", v.Exertion)
	}
	if err := v.Validate(); err != nil {
		t.Fatalf("creation shape invalid: %v", err)
	}

	v.Exertion = -12345
	enc, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var rt PlayerVitals
	if err := json.Unmarshal(enc, &rt); err != nil {
		t.Fatalf("round-trip unmarshal: %v", err)
	}
	if rt != v {
		t.Fatalf("round-trip = %+v, want %+v", rt, v)
	}
}

func TestAdjustBaseMaxHPGoldens(t *testing.T) {
	v := mustVitals(t)
	// Ordinary +1 (Stamina 25).
	nv, d, err := AdjustBaseMaxHP(v, 1, 25)
	if err != nil || nv.BaseMaxHP != 21 || d != 1 {
		t.Fatalf("base +1 = (%+v,%d,%v), want (21,1,nil)", nv, d, err)
	}
	// Negative change clips at the 20 floor: 21-5 -> 20, delta -1.
	nv2, d, err := AdjustBaseMaxHP(nv, -5, 25)
	if err != nil || nv2.BaseMaxHP != 20 || d != -1 {
		t.Fatalf("base 21-5 = (%+v,%d,%v), want (20,-1,nil)", nv2, d, err)
	}
	// Floor delta-0: 20-5 -> 20, delta 0.
	_, d, err = AdjustBaseMaxHP(v, -5, 25)
	if err != nil || d != 0 {
		t.Fatalf("base floor delta = (%d,%v), want (0,nil)", d, err)
	}
	// 100+Stamina ceiling: Stamina 1 -> 101.
	v101 := v
	v101.BaseMaxHP = 101
	nv3, d, err := AdjustBaseMaxHP(v101, 5, 1)
	if err != nil || nv3.BaseMaxHP != 101 || d != 0 {
		t.Fatalf("base 101+5/S1 = (%+v,%d,%v), want (101,0,nil)", nv3, d, err)
	}
	// 150 hard ceiling with high Stamina: 140+5/S70 -> 145 (+5).
	v140 := v
	v140.BaseMaxHP = 140
	nv4, d, err := AdjustBaseMaxHP(v140, 5, 70)
	if err != nil || nv4.BaseMaxHP != 145 || d != 5 {
		t.Fatalf("base 140+5/S70 = (%+v,%d,%v), want (145,5,nil)", nv4, d, err)
	}
	// 150+5/S70 -> 150, delta 0.
	v150 := v
	v150.BaseMaxHP = 150
	_, d, err = AdjustBaseMaxHP(v150, 5, 70)
	if err != nil || d != 0 {
		t.Fatalf("base hard-ceiling delta = (%d,%v), want (0,nil)", d, err)
	}
	// Base change never touches MaxHP by itself (composition is explicit).
	if nv.MaxHP != 20 || nv.HP != 20 {
		t.Fatalf("base adjust touched HP/Max: %+v", nv)
	}
	// Invalid stamina rejected.
	if _, _, err := AdjustBaseMaxHP(v, 1, 0); !errors.Is(err, ErrInvalidCombatStat) {
		t.Fatalf("stamina 0 err = %v, want ErrInvalidCombatStat", err)
	}
	if _, _, err := AdjustBaseMaxHP(v, 1, 71); !errors.Is(err, ErrInvalidCombatStat) {
		t.Fatalf("stamina 71 err = %v, want ErrInvalidCombatStat", err)
	}
	// Base+delta composition with MaxHP (source GainMaxHealth follow-on).
	nv5, d, err := AdjustBaseMaxHP(v, 1, 25)
	if err != nil {
		t.Fatal(err)
	}
	nv6, d2, err := AdjustMaxHP(nv5, d)
	if err != nil || nv6.MaxHP != 21 || d2 != 1 {
		t.Fatalf("max follow-on = (%+v,%d,%v), want (21,1,nil)", nv6, d2, err)
	}
}

func TestAdjustMaxHPGoldens(t *testing.T) {
	v := mustVitals(t)
	// Increase.
	nv, d, err := AdjustMaxHP(v, 5)
	if err != nil || nv.MaxHP != 25 || d != 5 {
		t.Fatalf("max +5 = (%+v,%d,%v), want (25,5,nil)", nv, d, err)
	}
	// Decrease to the 20 floor: 30-15 -> 20, delta -10, HP untouched.
	v2 := v
	v2.HP, v2.MaxHP = 25, 30
	nv2, d, err := AdjustMaxHP(v2, -15)
	if err != nil || nv2.MaxHP != 20 || d != -10 {
		t.Fatalf("max 30-15 = (%+v,%d,%v), want (20,-10,nil)", nv2, d, err)
	}
	if nv2.HP != 25 {
		t.Fatalf("max decrease clamped HP: %+v (HP must stay 25 above new Max 20)", nv2)
	}
	if err := nv2.Validate(); err != nil {
		t.Fatalf("over-max-after-decrease state invalid: %v", err)
	}
	// Already at floor: delta 0.
	_, d, err = AdjustMaxHP(v, -99)
	if err != nil || d != 0 {
		t.Fatalf("max floor delta = (%d,%v), want (0,nil)", d, err)
	}
}

func TestLoseHealthGoldens(t *testing.T) {
	v := mustVitals(t)
	for _, tc := range []struct {
		hp, amount     int
		after, applied int
		zero           bool
	}{
		{20, 5, 15, 5, false},
		{20, 20, 0, 20, true},
		{20, 99, 0, 20, true},
		{0, 5, 0, 0, true},
	} {
		vv := v
		vv.HP = tc.hp
		nv, r, err := LoseHealth(vv, tc.amount, false)
		if err != nil {
			t.Fatalf("LoseHealth(%d,%d): unexpected err %v", tc.hp, tc.amount, err)
		}
		if nv.HP != tc.after || r.Before != tc.hp || r.After != tc.after ||
			r.Applied != tc.applied || r.ZeroHP != tc.zero || r.Decay {
			t.Fatalf("LoseHealth(%d,%d) = (%+v,%+v), want after=%d applied=%d zero=%v",
				tc.hp, tc.amount, nv, r, tc.after, tc.applied, tc.zero)
		}
	}
	// Decay classification is observable.
	_, r, err := LoseHealth(v, 3, true)
	if err != nil || !r.Decay || r.Applied != 3 {
		t.Fatalf("decay loss = (%+v,%v), want Decay=true Applied=3", r, err)
	}
	// Negative loss rejected; state untouched.
	if _, _, err := LoseHealth(v, -1, false); !errors.Is(err, ErrInvalidHealthAmount) {
		t.Fatalf("negative loss err = %v, want ErrInvalidHealthAmount", err)
	}
	// Hostile amount cannot wrap below zero.
	nv, r, err := LoseHealth(v, math.MaxInt, false)
	if err != nil || nv.HP != 0 || r.Applied != 20 || !r.ZeroHP {
		t.Fatalf("MaxInt loss = (%+v,%+v,%v)", nv, r, err)
	}
}

func TestGainHealthNormalGoldens(t *testing.T) {
	v := mustVitals(t)
	for _, tc := range []struct {
		hp, max, amount int
		after, gained   int
	}{
		{10, 20, 5, 15, 5},
		{18, 20, 5, 20, 2},
		{20, 20, 5, 20, 0},
		{25, 20, 5, 25, 0}, // over-max untouched
	} {
		vv := v
		vv.HP, vv.MaxHP = tc.hp, tc.max
		nv, g, err := GainHealthNormal(vv, tc.amount)
		if err != nil {
			t.Fatalf("GainHealthNormal(%d/%d,+%d): unexpected err %v",
				tc.hp, tc.max, tc.amount, err)
		}
		if nv.HP != tc.after || g != tc.gained {
			t.Fatalf("GainHealthNormal(%d/%d,+%d) = (%d,%d), want (%d,%d)",
				tc.hp, tc.max, tc.amount, nv.HP, g, tc.after, tc.gained)
		}
	}
	if _, _, err := GainHealthNormal(v, -1); !errors.Is(err, ErrInvalidHealthAmount) {
		t.Fatalf("negative heal err = %v, want ErrInvalidHealthAmount", err)
	}
	// Hostile amount is a domain error, not saturation.
	vv := v
	vv.HP, vv.MaxHP = 10, math.MaxInt-1
	if _, _, err := GainHealthNormal(vv, math.MaxInt); !errors.Is(err, ErrInvalidHealthAmount) {
		t.Fatalf("hostile heal err = %v, want ErrInvalidHealthAmount", err)
	}
}

func TestGainHealthOvercapGoldens(t *testing.T) {
	v := mustVitals(t)
	// 20/20 +10 -> 30.
	nv, d, err := GainHealthOvercap(v, 10)
	if err != nil || nv.HP != 30 || d != 10 {
		t.Fatalf("overcap 20/20+10 = (%+v,%d,%v), want (30,10,nil)", nv, d, err)
	}
	// 30/20 +20 -> cap 40, delta 10.
	vv := v
	vv.HP = 30
	nv, d, err = GainHealthOvercap(vv, 20)
	if err != nil || nv.HP != 40 || d != 10 {
		t.Fatalf("overcap 30/20+20 = (%+v,%d,%v), want (40,10,nil)", nv, d, err)
	}
	// Already-above-2xMax corner (after a MaxHP reduction): source sets
	// HP DOWN to 2*Max even though unintuitive; delta is negative.
	vv.HP = 45
	nv, d, err = GainHealthOvercap(vv, 10)
	if err != nil || nv.HP != 40 || d != -5 {
		t.Fatalf("overcap corner 45/20+10 = (%+v,%d,%v), want (40,-5,nil)", nv, d, err)
	}
	if _, _, err := GainHealthOvercap(v, -1); !errors.Is(err, ErrInvalidHealthAmount) {
		t.Fatalf("negative overcap err = %v, want ErrInvalidHealthAmount", err)
	}
}

func TestLoseGainManaGoldens(t *testing.T) {
	v := mustVitals(t) // mana 20/20
	// Loss.
	nv, lost, err := LoseMana(v, 3)
	if err != nil || nv.Mana != 17 || lost != 3 {
		t.Fatalf("lose 10-3 = (%+v,%d,%v), want (17,3,nil)", nv, lost, err)
	}
	vv := v
	vv.Mana = 2
	nv, lost, err = LoseMana(vv, 5)
	if err != nil || nv.Mana != 0 || lost != 2 {
		t.Fatalf("lose 2-5 = (%+v,%d,%v), want (0,2,nil)", nv, lost, err)
	}
	if _, _, err := LoseMana(v, -1); !errors.Is(err, ErrInvalidManaAmount) {
		t.Fatalf("negative mana loss err = %v, want ErrInvalidManaAmount", err)
	}
	// Capped gain near max: 18/20 +5 -> 20 (2).
	vv.Mana, vv.MaxMana = 18, 20
	nv, g, err := GainMana(vv, 5, true)
	if err != nil || nv.Mana != 20 || g != 2 {
		t.Fatalf("capped 18/20+5 = (%+v,%d,%v), want (20,2,nil)", nv, g, err)
	}
	// Uncapped gain above max: 18/20 +5 -> 23 (5).
	nv, g, err = GainMana(vv, 5, false)
	if err != nil || nv.Mana != 23 || g != 5 {
		t.Fatalf("uncapped 18/20+5 = (%+v,%d,%v), want (23,5,nil)", nv, g, err)
	}
	if err := nv.Validate(); err != nil {
		t.Fatalf("uncapped-above-max state invalid: %v (Mana<=MaxMana is NOT an invariant)", err)
	}
	// Capped gain while already above max: source-faithful corner —
	// temp 28, gain = 5-(28-20) = -3, Mana clamped to 20.
	vv.Mana, vv.MaxMana = 23, 20
	nv, g, err = GainMana(vv, 5, true)
	if err != nil || nv.Mana != 20 || g != -3 {
		t.Fatalf("capped 23/20+5 = (%+v,%d,%v), want (20,-3,nil)", nv, g, err)
	}
	if _, _, err := GainMana(v, -1, true); !errors.Is(err, ErrInvalidManaAmount) {
		t.Fatalf("negative mana gain err = %v, want ErrInvalidManaAmount", err)
	}
	// MaxMana add primitive: no bound.
	nv, d, err := AdjustMaxMana(v, 6)
	if err != nil || nv.MaxMana != 26 || d != 6 {
		t.Fatalf("maxmana +6 = (%+v,%d,%v), want (26,6,nil)", nv, d, err)
	}
}

// TestGainManaCappedCorners pins the exact source delta in capped mode
// across the cap boundary, including the already-above-max corner where
// the nominal gain reduces Mana and the returned delta is negative
// (source iManaGained = amount - (tempMana - MaxMana)).
func TestGainManaCappedCorners(t *testing.T) {
	v := mustVitals(t) // mana 20/20
	for _, tc := range []struct {
		mana, max, amount int
		capped            bool
		after, gained     int
	}{
		{10, 20, 5, true, 15, 5},   // below max, cap not reached
		{18, 20, 5, true, 20, 2},   // near max, cap reached
		{20, 20, 5, true, 20, 0},   // exactly at max
		{23, 20, 5, true, 20, -3},  // above max: temp 28, 5-(28-20)
		{23, 20, 2, true, 20, -3},  // amount smaller than excess (3)
		{23, 20, 3, true, 20, -3},  // amount equal to excess
		{23, 20, 10, true, 20, -3}, // amount larger than excess
		{21, 20, 0, true, 20, -1},  // zero amount still clamps: 0-(21-20)
		{23, 20, 5, false, 28, 5},  // uncapped over-max unchanged
		{20, 20, 5, false, 25, 5},  // uncapped from max
		{10, 20, 5, false, 15, 5},  // uncapped below max
	} {
		vv := v
		vv.Mana, vv.MaxMana = tc.mana, tc.max
		nv, g, err := GainMana(vv, tc.amount, tc.capped)
		if err != nil {
			t.Fatalf("GainMana(%d/%d,%d,%v): unexpected err %v",
				tc.mana, tc.max, tc.amount, tc.capped, err)
		}
		if nv.Mana != tc.after || g != tc.gained {
			t.Fatalf("GainMana(%d/%d,%d,%v) = (%d,%d), want (%d,%d)",
				tc.mana, tc.max, tc.amount, tc.capped, nv.Mana, g, tc.after, tc.gained)
		}
		if g != nv.Mana-tc.mana {
			t.Fatalf("delta identity: GainMana(%d/%d,%d,%v) gained %d != %d-%d",
				tc.mana, tc.max, tc.amount, tc.capped, g, nv.Mana, tc.mana)
		}
	}
	// Hostile magnitudes: no overflow, no sign flip.
	vv := v
	vv.Mana, vv.MaxMana = math.MaxInt, 20
	nv, g, err := GainMana(vv, 1, true)
	if err != nil || nv.Mana != 20 || g != 20-math.MaxInt {
		t.Fatalf("hostile capped = (%+v,%d,%v), want (20,%d,nil)", nv, g, err, 20-math.MaxInt)
	}
	if _, _, err := GainMana(vv, 1, false); !errors.Is(err, ErrInvalidManaAmount) {
		t.Fatalf("hostile uncapped err = %v, want ErrInvalidManaAmount", err)
	}
	vv.Mana = math.MaxInt - 5
	nv, g, err = GainMana(vv, 5, true)
	if err != nil || nv.Mana != 20 || g != 20-(math.MaxInt-5) {
		t.Fatalf("hostile capped edge = (%+v,%d,%v)", nv, g, err)
	}
}

func TestHasVigorGoldens(t *testing.T) {
	v := mustVitals(t)
	v.Vigor = 10
	ok, err := v.HasVigor(9)
	if err != nil || !ok {
		t.Fatalf("HasVigor(10,9) = (%v,%v), want (true,nil)", ok, err)
	}
	ok, err = v.HasVigor(10)
	if err != nil || ok {
		t.Fatalf("HasVigor(10,10) = (%v,%v), want (false,nil) — strict >", ok, err)
	}
	if _, err := v.HasVigor(-1); !errors.Is(err, ErrInvalidExertion) {
		t.Fatalf("negative required err = %v, want ErrInvalidExertion", err)
	}
}

func TestApplyExertionGoldens(t *testing.T) {
	mk := func() PlayerVitals {
		v := mustVitals(t)
		v.Vigor, v.RestThreshold, v.Exertion = 100, 80, 0
		return v
	}
	// Exact strict->20000 boundary: no conversion at == 20000.
	for _, tc := range []struct {
		amount   int64
		vigor    int
		exertion int64
	}{
		{19999, 100, 19999},
		{20000, 100, 20000},
		{20001, 98, 1},
		{-19999, 100, -19999},
		{-20000, 100, -20000},
		{-20001, 102, -1},
	} {
		nv, err := ApplyExertion(mk(), tc.amount, false)
		if err != nil {
			t.Fatalf("ApplyExertion(%d): unexpected err %v", tc.amount, err)
		}
		if nv.Vigor != tc.vigor || nv.Exertion != tc.exertion {
			t.Fatalf("ApplyExertion(%d) = (vigor %d, ex %d), want (%d,%d)",
				tc.amount, nv.Vigor, nv.Exertion, tc.vigor, tc.exertion)
		}
	}
	// Signed truncation toward zero preserves the residual sign path:
	// +55555 -> lost 5 -> vigor 95, residual 5555.
	nv, err := ApplyExertion(mk(), 55555, false)
	if err != nil || nv.Vigor != 95 || nv.Exertion != 5555 {
		t.Fatalf("ApplyExertion(55555) = (%+v,%v), want (95,5555,nil)", nv, err)
	}
	// Lower clamp 1: vigor 2, +199999 -> lost 19 -> -17 -> 1, residual 9999.
	v := mk()
	v.Vigor = 2
	nv, err = ApplyExertion(v, 199999, false)
	if err != nil || nv.Vigor != 1 || nv.Exertion != 9999 {
		t.Fatalf("clamp-low = (%+v,%v), want (1,9999,nil)", nv, err)
	}
	// Upper clamp 200: vigor 199, -50000 -> lost -5 -> 204 -> 200, residual 0.
	v = mk()
	v.Vigor = 199
	nv, err = ApplyExertion(v, -50000, false)
	if err != nil || nv.Vigor != 200 || nv.Exertion != 0 {
		t.Fatalf("clamp-high = (%+v,%v), want (200,0,nil)", nv, err)
	}
	// SetToThreshold below threshold snaps and clears.
	v = mk()
	v.Vigor, v.Exertion = 50, 5000
	nv, err = ApplyExertion(v, 0, true)
	if err != nil || nv.Vigor != 80 || nv.Exertion != 0 {
		t.Fatalf("set-to-threshold-below = (%+v,%v), want (80,0,nil)", nv, err)
	}
	// SetToThreshold already >= threshold takes the ordinary path
	// (5000/10000 = 0: unchanged).
	v = mk()
	v.Exertion = 5000
	nv, err = ApplyExertion(v, 0, true)
	if err != nil || nv.Vigor != 100 || nv.Exertion != 5000 {
		t.Fatalf("set-to-threshold-above = (%+v,%v), want (100,5000,nil)", nv, err)
	}
	// Accumulator overflow is a domain error; state untouched.
	v = mk()
	v.Exertion = 20000
	if _, err := ApplyExertion(v, math.MaxInt64, false); !errors.Is(err, ErrInvalidExertion) {
		t.Fatalf("overflow err = %v, want ErrInvalidExertion", err)
	}
	// A lone huge (but addable) amount converts without overflow or panic:
	// MaxInt64/10000 vigor lost clamps vigor to 1, residual 5807.
	nv, err = ApplyExertion(mk(), math.MaxInt64, false)
	if err != nil || nv.Vigor != 1 || nv.Exertion != 5807 {
		t.Fatalf("huge convert = (%+v,%v), want (1,5807,nil)", nv, err)
	}
}

func TestApplyRestExertionGoldens(t *testing.T) {
	mk := func(vigor int) PlayerVitals {
		v := mustVitals(t)
		v.Vigor, v.RestThreshold, v.Exertion = vigor, 80, 0
		return v
	}
	// Above threshold: total no-op (state untouched, even with residual).
	v := mk(100)
	v.Exertion = 15000
	nv, err := ApplyRestExertion(v, -10000, 3)
	if err != nil || nv != v {
		t.Fatalf("above-threshold = (%+v,%v), want unchanged %+v", nv, err, v)
	}
	// Ordinary 1x: three -10000 ticks; strict boundary converts only on
	// the third (residual -20000 does NOT convert).
	nv, err = ApplyRestExertion(mk(70), -10000, 1)
	if err != nil || nv.Vigor != 70 || nv.Exertion != -10000 {
		t.Fatalf("rest 1x t1 = (%+v,%v), want (70,-10000,nil)", nv, err)
	}
	nv, err = ApplyRestExertion(nv, -10000, 1)
	if err != nil || nv.Vigor != 70 || nv.Exertion != -20000 {
		t.Fatalf("rest 1x t2 = (%+v,%v), want (70,-20000,nil)", nv, err)
	}
	nv, err = ApplyRestExertion(nv, -10000, 1)
	if err != nil || nv.Vigor != 73 || nv.Exertion != 0 {
		t.Fatalf("rest 1x t3 = (%+v,%v), want (73,0,nil) — residual CLEARED", nv, err)
	}
	// Sanctuary 2x: -10000 -> -20000 (no conversion yet), then converts.
	nv, err = ApplyRestExertion(mk(70), -10000, 2)
	if err != nil || nv.Vigor != 70 || nv.Exertion != -20000 {
		t.Fatalf("rest 2x t1 = (%+v,%v), want (70,-20000,nil)", nv, err)
	}
	nv, err = ApplyRestExertion(nv, -10000, 2)
	if err != nil || nv.Vigor != 74 || nv.Exertion != 0 {
		t.Fatalf("rest 2x t2 = (%+v,%v), want (74,0,nil)", nv, err)
	}
	// Triple 3x: single tick converts +3.
	nv, err = ApplyRestExertion(mk(70), -10000, 3)
	if err != nil || nv.Vigor != 73 || nv.Exertion != 0 {
		t.Fatalf("rest 3x = (%+v,%v), want (73,0,nil)", nv, err)
	}
	// Overshoot clamps UP to the threshold: 79 +3 -> 82 -> 80, cleared.
	nv, err = ApplyRestExertion(mk(79), -10000, 3)
	if err != nil || nv.Vigor != 80 || nv.Exertion != 0 {
		t.Fatalf("rest overshoot = (%+v,%v), want (80,0,nil)", nv, err)
	}
	// Positive amounts pass through unmultiplied.
	nv, err = ApplyRestExertion(mk(70), 5000, 3)
	if err != nil || nv.Vigor != 70 || nv.Exertion != 5000 {
		t.Fatalf("rest positive = (%+v,%v), want (70,5000,nil)", nv, err)
	}
	// Invalid multipliers rejected.
	for _, m := range []int{0, 4, -2} {
		if _, err := ApplyRestExertion(mk(70), -10000, m); !errors.Is(err, ErrInvalidVitals) {
			t.Fatalf("multiplier %d err = %v, want ErrInvalidVitals", m, err)
		}
	}
}

func TestSetRestThresholdGoldens(t *testing.T) {
	v := mustVitals(t)
	for _, th := range []int{10, 80, 100} {
		nv, err := SetRestThreshold(v, th)
		if err != nil || nv.RestThreshold != th {
			t.Fatalf("SetRestThreshold(%d) = (%+v,%v)", th, nv, err)
		}
	}
	for _, th := range []int{9, 101, 0, -5, 200} {
		if _, err := SetRestThreshold(v, th); !errors.Is(err, ErrInvalidRestThreshold) {
			t.Fatalf("SetRestThreshold(%d) err = %v, want ErrInvalidRestThreshold", th, err)
		}
	}
}

func TestDecayStomachGoldens(t *testing.T) {
	for _, tc := range []struct {
		stomach int
		elapsed int64
		want    int
	}{
		{0, 0, 1}, // initial-zero distinction: first update yields 1
		{100, 0, 100},
		{100, 8, 100},  // 96/100 = 0 drop: multiply BEFORE divide
		{100, 9, 99},   // 108/100 = 1
		{100, 100, 88}, // 1200/100 = 12
		{100, 833, 1},  // 9996/100 = 99
		{1, 1 << 40, 1},
	} {
		got, err := DecayStomach(tc.stomach, tc.elapsed)
		if err != nil {
			t.Fatalf("DecayStomach(%d,%d): unexpected err %v", tc.stomach, tc.elapsed, err)
		}
		if got != tc.want {
			t.Fatalf("DecayStomach(%d,%d) = %d, want %d", tc.stomach, tc.elapsed, got, tc.want)
		}
	}
	if _, err := DecayStomach(50, -1); !errors.Is(err, ErrInvalidElapsedTime) {
		t.Fatalf("negative elapsed err = %v, want ErrInvalidElapsedTime", err)
	}
	if _, err := DecayStomach(50, math.MaxInt64); !errors.Is(err, ErrInvalidElapsedTime) {
		t.Fatalf("hostile elapsed err = %v, want ErrInvalidElapsedTime", err)
	}
	if _, err := DecayStomach(101, 0); !errors.Is(err, ErrInvalidVitals) {
		t.Fatalf("stomach 101 err = %v, want ErrInvalidVitals", err)
	}
}

func TestCanEatGoldens(t *testing.T) {
	for _, tc := range []struct {
		stomach, filling int
		allow            bool
	}{
		{80, 20, true},  // 100 passes
		{80, 21, false}, // 101 fails
		{100, 0, true},
		{100, 1, false},
	} {
		ok, err := CanEat(tc.stomach, tc.filling)
		if err != nil || ok != tc.allow {
			t.Fatalf("CanEat(%d,%d) = (%v,%v), want (%v,nil)",
				tc.stomach, tc.filling, ok, err, tc.allow)
		}
	}
	if _, err := CanEat(50, -1); !errors.Is(err, ErrInvalidVitals) {
		t.Fatalf("negative filling err = %v, want ErrInvalidVitals", err)
	}
}

// TestVitalsValidationDomains pins the frozen validation domain and the
// explicitly REJECTED false invariants: over-max HP, over-max mana, and
// MaxHP below BaseMaxHP are all LEGAL states.
func TestVitalsValidationDomains(t *testing.T) {
	legal := mustVitals(t)
	legal.HP, legal.MaxHP = 40, 20     // vamp over-max: legal
	legal.Mana, legal.MaxMana = 30, 20 // uncapped over-max: legal
	legal.BaseMaxHP = 30               // MaxHP < BaseMaxHP: legal
	if err := legal.Validate(); err != nil {
		t.Fatalf("asymmetric over-max state must validate: %v", err)
	}
	bad := []PlayerVitals{
		func() PlayerVitals { v := mustVitals(t); v.HP = -1; return v }(),
		func() PlayerVitals { v := mustVitals(t); v.BaseMaxHP = 19; return v }(),
		func() PlayerVitals { v := mustVitals(t); v.BaseMaxHP = 151; return v }(),
		func() PlayerVitals { v := mustVitals(t); v.MaxHP = 19; return v }(),
		func() PlayerVitals { v := mustVitals(t); v.Mana = -1; return v }(),
		func() PlayerVitals { v := mustVitals(t); v.MaxMana = 0; return v }(),
		func() PlayerVitals { v := mustVitals(t); v.Vigor = 0; return v }(),
		func() PlayerVitals { v := mustVitals(t); v.Vigor = 201; return v }(),
		func() PlayerVitals { v := mustVitals(t); v.RestThreshold = 9; return v }(),
		func() PlayerVitals { v := mustVitals(t); v.Exertion = 20001; return v }(),
		func() PlayerVitals { v := mustVitals(t); v.Exertion = -20001; return v }(),
		func() PlayerVitals { v := mustVitals(t); v.Stomach = 101; return v }(),
	}
	for i, v := range bad {
		if err := v.Validate(); !errors.Is(err, ErrInvalidVitals) {
			t.Fatalf("bad[%d] (%+v) err = %v, want ErrInvalidVitals", i, v, err)
		}
	}
}

// TestVitalsCrossStageComposition proves the T1/T2/T3a/T3b -> T4a
// application boundary with the REAL helpers (test composition only;
// no runtime implemented).
func TestVitalsCrossStageComposition(t *testing.T) {
	// Damage: T1 raw -> T2 resistance -> T4a LoseHealth (Applied proven).
	raw, err := RawWeaponDamage(RawDamageInput{
		WeaponBaseDamage: 10, DamageBonus: 2, DamageFactor: 100,
		Proficiency: 20, MaxProfDamage: 5, Attr: 30,
	})
	if err != nil || raw != 13 {
		t.Fatalf("T1 raw = (%d,%v), want (13,nil)", raw, err)
	}
	mit, err := ApplyResistance(raw, 20)
	if err != nil || mit != 10 {
		t.Fatalf("T2 resisted = (%d,%v), want (10,nil)", mit, err)
	}
	victim := mustVitals(t)
	victim, res, err := LoseHealth(victim, mit, false)
	if err != nil || victim.HP != 10 || res.Applied != 10 || res.ZeroHP {
		t.Fatalf("T4 loss = (%+v,%+v,%v), want HP10 Applied10", victim, res, err)
	}

	// Illusionary Wounds: T3b absolute non-lethal loss -> T4a applies it;
	// non-lethality is preserved (HP stays > 0).
	iw, err := IllusionaryWoundsLoss(
		IllusionaryVictim{Kind: VictimPlayer, Intellect: 25, MaxHP: 30, HP: 20}, 50)
	if err != nil || iw != 9 {
		t.Fatalf("T3b IW loss = (%d,%v), want (9,nil)", iw, err)
	}
	victim2 := mustVitals(t)
	victim2.HP, victim2.MaxHP = 20, 30
	victim2, res, err = LoseHealth(victim2, iw, false)
	if err != nil || victim2.HP != 11 || res.ZeroHP {
		t.Fatalf("T4 IW apply = (%+v,%+v,%v), want HP11 alive", victim2, res, err)
	}

	// Vampiric Drain: T4 target loss -> actual Applied -> T3b heal ->
	// T4 caster over-max heal (non-lethal path).
	target := mustVitals(t)
	target, tres, err := LoseHealth(target, 7, false)
	if err != nil || tres.Applied != 7 {
		t.Fatalf("T4 target loss = (%+v,%v)", tres, err)
	}
	heal, err := VampiricDrainHeal(tres.Applied, false, 18)
	if err != nil || heal != 3 {
		t.Fatalf("T3b vamp heal = (%d,%v), want (3,nil)", heal, err)
	}
	caster := mustVitals(t)
	caster, delta, err := GainHealthOvercap(caster, heal)
	if err != nil || caster.HP != 23 || delta != 3 {
		t.Fatalf("T4 caster heal = (%+v,%d,%v), want (23,3,nil)", caster, delta, err)
	}
	// Lethal path uses the resolved prototype max, not the applied scalar.
	target2 := mustVitals(t)
	target2.HP = 5
	target2, tres, err = LoseHealth(target2, 20, false)
	if err != nil || !tres.ZeroHP || tres.Applied != 5 {
		t.Fatalf("T4 lethal loss = (%+v,%v)", tres, err)
	}
	heal, err = VampiricDrainHeal(tres.Applied, true, 18)
	if err != nil || heal != 9 {
		t.Fatalf("T3b lethal vamp heal = (%d,%v), want (9,nil)", heal, err)
	}

	// Spell payment: T3a plan -> T4a LoseMana + ApplyExertion (success).
	pay, err := ResolveSpellPayment(SpellPaymentInput{
		Origin: OriginPlayer, ManaCost: 5, Exertion: 2, RollSucceeded: true,
	})
	if err != nil || pay.ManaCharge != 5 || pay.ExertionCharge != 20000 {
		t.Fatalf("T3a payment = (%+v,%v), want (5,20000,nil)", pay, err)
	}
	caster2 := mustVitals(t)
	caster2, lost, err := LoseMana(caster2, pay.ManaCharge)
	if err != nil || caster2.Mana != 15 || lost != 5 {
		t.Fatalf("T4 mana charge = (%+v,%d,%v), want (15,5,nil)", caster2, lost, err)
	}
	caster2, err = ApplyExertion(caster2, int64(pay.ExertionCharge), false)
	if err != nil || caster2.Exertion != 20000 || caster2.Vigor != 100 {
		t.Fatalf("T4 exertion charge = (%+v,%v), want (ex20000,v100,nil)", caster2, err)
	}
	// Failed roll pays half through the same primitives.
	pay, err = ResolveSpellPayment(SpellPaymentInput{
		Origin: OriginPlayer, ManaCost: 5, Exertion: 2, RollSucceeded: false,
	})
	if err != nil || pay.ManaCharge != 2 || pay.ExertionCharge != 10000 {
		t.Fatalf("T3a failed payment = (%+v,%v), want (2,10000,nil)", pay, err)
	}
}

// TestVitalsProperties checks cross-cutting invariants over randomized
// valid states/inputs (fixed seed: deterministic, no flakes).
func TestVitalsProperties(t *testing.T) {
	r := rand.New(rand.NewSource(42))
	mkValid := func() PlayerVitals {
		return PlayerVitals{
			HP: r.Intn(60), BaseMaxHP: 20 + r.Intn(131),
			MaxHP: 20 + r.Intn(200), Mana: r.Intn(60),
			MaxMana: 1 + r.Intn(200), Vigor: 1 + r.Intn(200),
			RestThreshold: 10 + r.Intn(91),
			Exertion:      int64(r.Intn(40001) - 20000),
			Stomach:       r.Intn(101),
		}
	}
	for i := 0; i < 2000; i++ {
		v := mkValid()
		if err := v.Validate(); err != nil {
			t.Fatalf("mkValid invalid: %+v: %v", v, err)
		}
		// Loss never yields HP < 0; applied is exact.
		amt := r.Intn(80)
		nv, res, err := LoseHealth(v, amt, false)
		if err != nil {
			t.Fatalf("loss err: %v", err)
		}
		if nv.HP < 0 || res.Applied != res.Before-res.After {
			t.Fatalf("loss invariant: %+v %+v", nv, res)
		}
		// Normal heal from <= Max never exceeds Max and never lowers.
		nv2, g, err := GainHealthNormal(v, amt)
		if err != nil {
			t.Fatalf("heal err: %v", err)
		}
		if v.HP <= v.MaxHP && (nv2.HP > v.MaxHP || nv2.HP < v.HP || g != nv2.HP-v.HP) {
			t.Fatalf("normal-heal invariant: %+v -> %+v (%d)", v, nv2, g)
		}
		if v.HP > v.MaxHP && (nv2.HP != v.HP || g != 0) {
			t.Fatalf("over-max heal invariant: %+v -> %+v (%d)", v, nv2, g)
		}
		// Mana never negative after either mutation.
		nm, _, err := LoseMana(v, amt)
		if err != nil || nm.Mana < 0 {
			t.Fatalf("mana loss invariant: %+v %+v", nm, err)
		}
		nm2, _, err := GainMana(v, amt, r.Intn(2) == 0)
		if err != nil || nm2.Mana < 0 {
			t.Fatalf("mana gain invariant: %+v %+v", nm2, err)
		}
		// Exertion mutations end with vigor 1..200 and residual in range.
		ne, err := ApplyExertion(v, int64(r.Intn(200001)-100000), r.Intn(2) == 0)
		if err != nil {
			t.Fatalf("exertion err: %v", err)
		}
		if ne.Vigor < 1 || ne.Vigor > 200 ||
			ne.Exertion < -20000 || ne.Exertion > 20000 {
			t.Fatalf("exertion invariant: %+v", ne)
		}
		nr, err := ApplyRestExertion(v, int64(r.Intn(60001)-30000), 1+r.Intn(3))
		if err != nil {
			t.Fatalf("rest exertion err: %v", err)
		}
		if nr.Vigor < 1 || nr.Vigor > 200 {
			t.Fatalf("rest vigor invariant: %+v", nr)
		}
		// Stomach decay: always 1..100, never increases, monotone in
		// elapsed for the same start.
		d1, err := DecayStomach(v.Stomach, int64(r.Intn(2000)))
		if err != nil || d1 < 1 || d1 > 100 || (v.Stomach > 0 && d1 > v.Stomach) {
			t.Fatalf("stomach invariant: %d -> %d (%v)", v.Stomach, d1, err)
		}
		if v.Stomach == 0 && d1 != 1 {
			t.Fatalf("stomach zero-start must yield 1 or stay monotone: %d", d1)
		}
		// Determinism: same input -> same result.
		a, _, _ := LoseHealth(v, amt, false)
		b, _, _ := LoseHealth(v, amt, false)
		if a != b {
			t.Fatalf("nondeterministic loss: %+v vs %+v", a, b)
		}
	}
}

// Fuzz seeds for the cheap primitives: no panic, no overflow/sign flip,
// stable domain errors, documented bounds. Short in-test corpus runs
// below; `go test -fuzz` campaigns are NOT required.
func FuzzAdjustBaseMaxHP(f *testing.F) {
	for _, s := range []struct {
		base, amt, stam int
	}{{20, 1, 25}, {20, -5, 25}, {150, 5, 70}, {101, 5, 1}, {20, math.MaxInt, 25}} {
		f.Add(s.base, s.amt, s.stam)
	}
	f.Fuzz(func(t *testing.T, base, amt, stam int) {
		v := mustVitals(t)
		v.BaseMaxHP = base
		nv, d, err := AdjustBaseMaxHP(v, amt, stam)
		if err != nil {
			if !errors.Is(err, ErrInvalidVitals) && !errors.Is(err, ErrInvalidCombatStat) &&
				!errors.Is(err, ErrInvalidHealthAmount) {
				t.Fatalf("unstable err: %v", err)
			}
			return
		}
		if nv.BaseMaxHP < 20 || nv.BaseMaxHP > 150 || d != nv.BaseMaxHP-base {
			t.Fatalf("bounds: %+v delta %d", nv, d)
		}
	})
}

func FuzzLoseHealth(f *testing.F) {
	for _, s := range []struct{ hp, amt int }{{20, 5}, {20, 20}, {0, 0}, {40, 99}} {
		f.Add(s.hp, s.amt)
	}
	f.Fuzz(func(t *testing.T, hp, amt int) {
		v := mustVitals(t)
		v.HP = hp
		nv, res, err := LoseHealth(v, amt, false)
		if hp < 0 || amt < 0 {
			if !errors.Is(err, ErrInvalidVitals) && !errors.Is(err, ErrInvalidHealthAmount) {
				t.Fatalf("want domain err, got (%+v,%v)", res, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if nv.HP < 0 || res.Applied != hp-nv.HP {
			t.Fatalf("invariant: %+v %+v", nv, res)
		}
	})
}

func FuzzGainHealthNormal(f *testing.F) {
	for _, s := range []struct{ hp, max, amt int }{{10, 20, 5}, {25, 20, 5}, {20, 20, 0}} {
		f.Add(s.hp, s.max, s.amt)
	}
	f.Fuzz(func(t *testing.T, hp, max, amt int) {
		v := mustVitals(t)
		v.HP, v.MaxHP = hp, max
		nv, g, err := GainHealthNormal(v, amt)
		if hp < 0 || max < 20 || amt < 0 {
			if !errors.Is(err, ErrInvalidVitals) && !errors.Is(err, ErrInvalidHealthAmount) {
				t.Fatalf("want domain err, got (%+v,%d,%v)", nv, g, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if hp <= max && (nv.HP > max || nv.HP < hp || g != nv.HP-hp) {
			t.Fatalf("invariant: (%d/%d+%d) -> (%+v,%d)", hp, max, amt, nv, g)
		}
		if hp > max && (nv.HP != hp || g != 0) {
			t.Fatalf("over-max invariant: (%d/%d+%d) -> (%+v,%d)", hp, max, amt, nv, g)
		}
	})
}

func FuzzGainHealthOvercap(f *testing.F) {
	for _, s := range []struct {
		hp, max, amt int
	}{{20, 20, 10}, {30, 20, 20}, {45, 20, 10}, {0, 20, 0}} {
		f.Add(s.hp, s.max, s.amt)
	}
	f.Fuzz(func(t *testing.T, hp, max, amt int) {
		v := mustVitals(t)
		v.HP, v.MaxHP = hp, max
		nv, d, err := GainHealthOvercap(v, amt)
		if hp < 0 || max < 20 || amt < 0 {
			if !errors.Is(err, ErrInvalidVitals) && !errors.Is(err, ErrInvalidHealthAmount) {
				t.Fatalf("want domain err, got (%+v,%d,%v)", nv, d, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if d != nv.HP-hp {
			t.Fatalf("delta: (%d/%d+%d) -> (%+v,%d)", hp, max, amt, nv, d)
		}
		// Cap proof: a non-corner result never exceeds 2*Max.
		if int64(hp)+int64(amt) <= int64(max)*2 && nv.HP != hp+amt {
			t.Fatalf("below-cap must add exactly: (%d/%d+%d) -> %+v", hp, max, amt, nv)
		}
	})
}

func FuzzLoseGainMana(f *testing.F) {
	for _, s := range []struct {
		mana, max, amt int
	}{{10, 20, 3}, {2, 20, 5}, {18, 20, 5}} {
		f.Add(s.mana, s.max, s.amt)
	}
	f.Fuzz(func(t *testing.T, mana, max, amt int) {
		v := mustVitals(t)
		v.Mana, v.MaxMana = mana, max
		if mana < 0 || max < 1 || amt < 0 {
			nl, _, errL := LoseMana(v, amt)
			ng, _, errG := GainMana(v, amt, true)
			_ = nl
			_ = ng
			if !errors.Is(errL, ErrInvalidVitals) && !errors.Is(errL, ErrInvalidManaAmount) {
				t.Fatalf("lose: want domain err, got %v", errL)
			}
			if !errors.Is(errG, ErrInvalidVitals) && !errors.Is(errG, ErrInvalidManaAmount) {
				t.Fatalf("gain: want domain err, got %v", errG)
			}
			return
		}
		nl, lost, err := LoseMana(v, amt)
		if err != nil || nl.Mana < 0 || lost != mana-nl.Mana {
			t.Fatalf("lose invariant: (%d/%d-%d) -> (%+v,%d,%v)", mana, max, amt, nl, lost, err)
		}
		ng, gained, err := GainMana(v, amt, true)
		if err != nil || ng.Mana < 0 || ng.Mana > max || gained != ng.Mana-mana {
			t.Fatalf("capped invariant: (%d/%d+%d) -> (%+v,%d,%v)", mana, max, amt, ng, gained, err)
		}
		nu, gainedU, err := GainMana(v, amt, false)
		if err != nil {
			// Only hostile sum overflow may error on valid domains.
			if !errors.Is(err, ErrInvalidManaAmount) {
				t.Fatalf("uncapped: want overflow domain err, got %v", err)
			}
		} else if nu.Mana < 0 || gainedU != amt {
			t.Fatalf("uncapped invariant: (%d/%d+%d) -> (%+v,%d,%v)", mana, max, amt, nu, gainedU, err)
		}
	})
}

func FuzzApplyExertion(f *testing.F) {
	for _, a := range []int64{19999, 20000, 20001, -20000, -20001, 0, math.MaxInt64, math.MinInt64} {
		f.Add(a)
	}
	f.Fuzz(func(t *testing.T, amount int64) {
		v := mustVitals(t)
		nv, err := ApplyExertion(v, amount, false)
		if err != nil {
			if !errors.Is(err, ErrInvalidExertion) {
				t.Fatalf("unstable err: %v", err)
			}
			return
		}
		if nv.Vigor < 1 || nv.Vigor > 200 ||
			nv.Exertion < -20000 || nv.Exertion > 20000 {
			t.Fatalf("bounds: amount %d -> %+v", amount, nv)
		}
	})
}

func FuzzApplyRestExertion(f *testing.F) {
	for _, s := range []struct {
		amount int64
		mult   int
	}{{0, 1}, {-10000, 1}, {-10000, 2}, {-10000, 3}, {-10000, 0}, {5000, 3}} {
		f.Add(s.amount, s.mult)
	}
	f.Fuzz(func(t *testing.T, amount int64, mult int) {
		v := mustVitals(t)
		v.Vigor = 70
		nv, err := ApplyRestExertion(v, amount, mult)
		if mult < 1 || mult > 3 {
			if !errors.Is(err, ErrInvalidVitals) {
				t.Fatalf("want ErrInvalidVitals, got %v", err)
			}
			return
		}
		if err != nil {
			if !errors.Is(err, ErrInvalidExertion) {
				t.Fatalf("unstable err: %v", err)
			}
			return
		}
		if nv.Vigor < 1 || nv.Vigor > 80 || nv.Exertion < -20000 || nv.Exertion > 20000 {
			t.Fatalf("bounds: (%d,%d) -> %+v", amount, mult, nv)
		}
	})
}

func FuzzDecayStomach(f *testing.F) {
	for _, s := range []struct {
		stomach int
		elapsed int64
	}{{0, 0}, {100, 9}, {100, 833}, {50, 1000000}} {
		f.Add(s.stomach, s.elapsed)
	}
	f.Fuzz(func(t *testing.T, stomach int, elapsed int64) {
		got, err := DecayStomach(stomach, elapsed)
		if stomach < 0 || stomach > 100 || elapsed < 0 {
			if !errors.Is(err, ErrInvalidVitals) && !errors.Is(err, ErrInvalidElapsedTime) {
				t.Fatalf("want domain err, got (%d,%v)", got, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got < 1 || got > 100 || (stomach > 0 && got > stomach) {
			t.Fatalf("bounds: (%d,%d) -> %d", stomach, elapsed, got)
		}
	})
}
