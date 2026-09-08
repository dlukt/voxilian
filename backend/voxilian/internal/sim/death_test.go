package sim

import (
	"errors"
	"math"
	"testing"
)

// --- test helpers ---------------------------------------------------------

// d100Rolls builds a scriptRNG that yields exactly the given d100
// outcomes in order (RollD100 = 1 + Uint64()%100, so outcome r needs
// raw value r-1).
func d100Rolls(outcomes ...int) *scriptRNG {
	vals := make([]uint64, len(outcomes))
	for i, r := range outcomes {
		vals[i] = uint64(r - 1)
	}
	return &scriptRNG{vals: vals}
}

func mkVitals(hp, base, max, mana, maxMana, vigor int) PlayerVitals {
	return PlayerVitals{
		HP: hp, BaseMaxHP: base, MaxHP: max,
		Mana: mana, MaxMana: maxMana,
		Vigor: vigor, RestThreshold: 80,
	}
}

func mustPlan(t *testing.T, def int, ctx DeathContext, killer bool) DeathDispositionPlan {
	t.Helper()
	p, err := PlanDeathDisposition(def, ctx, killer)
	if err != nil {
		t.Fatalf("PlanDeathDisposition: %v", err)
	}
	return p
}

// --- disposition: avoided vs cheap vs normal ------------------------------

func TestDeathDispositionAvoidedConditionsIndividually(t *testing.T) {
	conds := map[string]DeathContext{
		"arena-non-real":   {ArenaNonRealDeath: true},
		"prison":           {PrisonRoom: true},
		"safe-attack":      {SafePlayerAttack: true},
		"arena-and-prison": {ArenaNonRealDeath: true, PrisonRoom: true},
	}
	for name, ctx := range conds {
		p := mustPlan(t, 100, ctx, false)
		if p.Disposition != DeathAvoided {
			t.Fatalf("%s: disposition = %v, want avoided", name, p.Disposition)
		}
		if p.DeathCost != 0 {
			t.Fatalf("%s: avoided cost = %d, want 0", name, p.DeathCost)
		}
		if !p.SpecialItemsKept {
			t.Fatalf("%s: avoided deaths arm the special-item keep-guard", name)
		}
		if p.TokenDeath || p.NewbieHomeRespawn {
			t.Fatalf("%s: avoided death must not set token/newbie-respawn flags", name)
		}
		// An avoided death has no pending phase and no drop plan.
		corpse, err := PlanCorpse(1000)
		if err != nil {
			t.Fatalf("PlanCorpse: %v", err)
		}
		pend, err := PlanPendingDeath(p, corpse)
		if err != nil {
			t.Fatalf("PlanPendingDeath: %v", err)
		}
		if pend.Phase != DeathPhaseNone {
			t.Fatalf("%s: pending phase = %v, want none", name, pend.Phase)
		}
		if _, err := PlanDeathDrops(p, []DeathItemInput{{Key: 1, DropOnDeath: true, RoomAccepts: true}}); !errors.Is(err, ErrInvalidDeathInput) {
			t.Fatalf("%s: drop plan on avoided death: err = %v, want ErrInvalidDeathInput", name, err)
		}
		if _, err := PlanDeathAdvancement(p.Disposition, 5, -10); !errors.Is(err, ErrInvalidDeathInput) {
			t.Fatalf("%s: advancement plan on avoided death: err = %v, want ErrInvalidDeathInput", name, err)
		}
	}
}

func TestDeathDispositionCheapConditionsIndividually(t *testing.T) {
	conds := map[string]DeathContext{
		"frenzy":       {FrenzyActive: true},
		"newbie-zone":  {NewbieZoneDeath: true},
		"newbie-honor": {NewbieHonor: true},
		"token":        {CarriesToken: true},
	}
	for name, ctx := range conds {
		p := mustPlan(t, 100, ctx, true)
		if p.Disposition != DeathCheap {
			t.Fatalf("%s: disposition = %v, want cheap", name, p.Disposition)
		}
		if p.DeathCost != 0 {
			t.Fatalf("%s: cheap cost = %d, want 0", name, p.DeathCost)
		}
		// The keep-guard is armed by frenzy/newbie-zone/newbie-honor —
		// NOT by the token check placed after it: a pure token death
		// keeps the flag clear (the artifact is lost, per source
		// ordering).
		if want := name != "token"; p.SpecialItemsKept != want {
			t.Fatalf("%s: SpecialItemsKept = %v, want %v", name, p.SpecialItemsKept, want)
		}
		if want := name == "token"; p.TokenDeath != want {
			t.Fatalf("%s: TokenDeath = %v, want %v", name, p.TokenDeath, want)
		}
		if want := name == "newbie-zone"; p.NewbieHomeRespawn != want {
			t.Fatalf("%s: NewbieHomeRespawn = %v, want %v", name, p.NewbieHomeRespawn, want)
		}
		// Cheap deaths ARE real deaths: pending phase with cost 0 and a
		// corpse plan.
		corpse, err := PlanCorpse(4242)
		if err != nil {
			t.Fatalf("PlanCorpse: %v", err)
		}
		pend, err := PlanPendingDeath(p, corpse)
		if err != nil {
			t.Fatalf("PlanPendingDeath: %v", err)
		}
		if pend.Phase != DeathPhasePending || pend.EffectiveDeathCost != 0 || pend.DeathTimeSeconds != 4242 {
			t.Fatalf("%s: pending plan = %+v", name, pend)
		}
	}
	// Token deaths still lose the artifact: kept stays false even with
	// the token flag set.
	if p := mustPlan(t, 100, DeathContext{CarriesToken: true}, false); p.SpecialItemsKept || !p.TokenDeath {
		t.Fatalf("token death must clear the keep-guard: %+v", p)
	}
}

func TestDeathDispositionNormal(t *testing.T) {
	for _, def := range []int{1, 60, 90, 100} {
		p := mustPlan(t, def, DeathContext{}, true)
		if p.Disposition != DeathNormal {
			t.Fatalf("default %d: disposition = %v, want normal", def, p.Disposition)
		}
		if p.DeathCost != def {
			t.Fatalf("default %d: cost = %d", def, p.DeathCost)
		}
		if p.SpecialItemsKept || p.TokenDeath || p.NewbieHomeRespawn {
			t.Fatalf("default %d: normal death flags = %+v", def, p)
		}
		if !p.KillerIsPlayer {
			t.Fatalf("killer-is-player echo lost")
		}
	}
}

func TestDeathDispositionDefaultCostDomain(t *testing.T) {
	for _, bad := range []int{0, -1, 101, math.MaxInt} {
		if _, err := PlanDeathDisposition(bad, DeathContext{}, false); !errors.Is(err, ErrInvalidDeathCost) {
			t.Fatalf("default %d: err = %v, want ErrInvalidDeathCost", bad, err)
		}
	}
}

// --- double-death guard ---------------------------------------------------

func TestDeathDoubleDeathGuardBoundaries(t *testing.T) {
	cases := []struct {
		last, now int64
		blocked   bool
	}{
		{100, 100, true},
		{100, 101, true},
		{100, 101, true},
		{100, 102, false}, // exactly +2 seconds PROCEEDS (strict <)
		{100, 103, false},
		{0, 1, true},
		{0, 2, false},
	}
	for _, c := range cases {
		got, err := DeathBlockedByDoubleDeath(c.last, c.now)
		if err != nil {
			t.Fatalf("%v/%v: %v", c.last, c.now, err)
		}
		if got != c.blocked {
			t.Fatalf("last=%d now=%d: blocked = %v, want %v", c.last, c.now, got, c.blocked)
		}
	}
	if _, err := DeathBlockedByDoubleDeath(-1, 0); !errors.Is(err, ErrInvalidDeathTime) {
		t.Fatalf("negative last: err = %v", err)
	}
	if _, err := DeathBlockedByDoubleDeath(0, -1); !errors.Is(err, ErrInvalidDeathTime) {
		t.Fatalf("negative now: err = %v", err)
	}
	if _, err := DeathBlockedByDoubleDeath(math.MaxInt64, math.MaxInt64); !errors.Is(err, ErrInvalidDeathTime) {
		t.Fatalf("overflowing last: err = %v, want ErrInvalidDeathTime", err)
	}
}

// --- corpse constants ------------------------------------------------------

func TestCorpsePolicyConstants(t *testing.T) {
	c, err := PlanCorpse(7777)
	if err != nil {
		t.Fatalf("PlanCorpse: %v", err)
	}
	if c.LifetimeMs != 600000 {
		t.Fatalf("player corpse lifetime = %d, want 600000", c.LifetimeMs)
	}
	if c.NoStealMs != 25000 {
		t.Fatalf("no-steal window = %d, want 25000", c.NoStealMs)
	}
	if c.DeathTimeSeconds != 7777 {
		t.Fatalf("death time echo = %d, want 7777", c.DeathTimeSeconds)
	}
	if PKProtectionDurationMs != 600000 {
		t.Fatalf("PK protection duration = %d, want 600000", PKProtectionDurationMs)
	}
	if _, err := PlanCorpse(-1); !errors.Is(err, ErrInvalidDeathTime) {
		t.Fatalf("negative death time: err = %v", err)
	}
}

// --- immediate post-death vitals -------------------------------------------

func TestDeathPostVitalsOrdinaryBoundaries(t *testing.T) {
	cases := []struct {
		vigorBefore int
		vigorAfter  int
		note        string
	}{
		{1, 1, "1/4=0 floors to 1 (NewVigor)"},
		{2, 1, "2/4=0 floors to 1"},
		{3, 1, "3/4=0 floors to 1"},
		{4, 1, "4/4=1"},
		{5, 1, "5/4=1"},
		{7, 1, "7/4=1"},
		{8, 2, "8/4=2"},
		{99, 24, "99/4=24 truncation"},
		{100, 25, "100/4=25"},
		{199, 49, "199/4=49"},
		{200, 50, "200/4=50"},
		{201, 50, "unreachable live; cap 50"}, // Validate allows 1..200 only; guarded below
	}
	for _, c := range cases {
		if c.vigorBefore > 200 {
			continue
		}
		in := PostDeathVitalsInput{
			Vitals:      mkVitals(0, 40, 45, 7, 25, c.vigorBefore),
			Disposition: DeathNormal,
		}
		got, err := PlanPostDeathVitals(in)
		if err != nil {
			t.Fatalf("vigor %d: %v", c.vigorBefore, err)
		}
		if got.Vigor != c.vigorAfter {
			t.Fatalf("vigor %d (%s): after = %d, want %d", c.vigorBefore, c.note, got.Vigor, c.vigorAfter)
		}
		if got.HP != 1 || got.Mana != 1 {
			t.Fatalf("vigor %d: HP/Mana = %d/%d, want 1/1", c.vigorBefore, got.HP, got.Mana)
		}
		if err := got.Validate(); err != nil {
			t.Fatalf("vigor %d: result invalid: %v", c.vigorBefore, err)
		}
		// Untouched fields stay untouched.
		if got.BaseMaxHP != 40 || got.MaxHP != 45 || got.MaxMana != 25 ||
			got.RestThreshold != 80 || got.Exertion != 0 || got.Stomach != 0 {
			t.Fatalf("vigor %d: unrelated fields changed: %+v", c.vigorBefore, got)
		}
	}
	// Upper cap boundary at exactly the quartered cap: vigor 200 -> 50.
	hi, err := PlanPostDeathVitals(PostDeathVitalsInput{Vitals: mkVitals(0, 40, 45, 0, 25, 200), Disposition: DeathCheap})
	if err != nil || hi.Vigor != 50 {
		t.Fatalf("cap vector: %+v err=%v", hi, err)
	}
}

func TestDeathPostVitalsFrenzyBranch(t *testing.T) {
	in := PostDeathVitalsInput{
		Vitals:       mkVitals(0, 40, 45, 7, 25, 33), // odd maxima: truncation
		Disposition:  DeathCheap,
		FrenzyActive: true,
	}
	got, err := PlanPostDeathVitals(in)
	if err != nil {
		t.Fatalf("frenzy: %v", err)
	}
	if got.HP != 22 { // 45/2
		t.Fatalf("frenzy HP = %d, want 22", got.HP)
	}
	if got.Mana != 12 { // 25/2
		t.Fatalf("frenzy Mana = %d, want 12", got.Mana)
	}
	if got.Vigor != 100 {
		t.Fatalf("frenzy Vigor = %d, want 100", got.Vigor)
	}
	// Cheap non-frenzy death uses the ORDINARY branch.
	cheap, err := PlanPostDeathVitals(PostDeathVitalsInput{Vitals: mkVitals(0, 40, 45, 7, 25, 100), Disposition: DeathCheap})
	if err != nil || cheap.HP != 1 || cheap.Mana != 1 || cheap.Vigor != 25 {
		t.Fatalf("cheap ordinary vitals = %+v err=%v", cheap, err)
	}
}

func TestDeathPostVitalsAngelManaOverride(t *testing.T) {
	got, err := PlanPostDeathVitals(PostDeathVitalsInput{
		Vitals: mkVitals(0, 40, 45, 7, 25, 100), Disposition: DeathNormal,
		AngelMailEligible: true,
	})
	if err != nil {
		t.Fatalf("angel: %v", err)
	}
	if got.Mana != 14 { // 25/2 + 2
		t.Fatalf("angel Mana = %d, want 14", got.Mana)
	}
	if got.HP != 1 || got.Vigor != 25 {
		t.Fatalf("angel HP/Vigor = %d/%d, want 1/25", got.HP, got.Vigor)
	}
	even, err := PlanPostDeathVitals(PostDeathVitalsInput{
		Vitals: mkVitals(0, 40, 40, 7, 20, 100), Disposition: DeathNormal,
		AngelMailEligible: true,
	})
	if err != nil || even.Mana != 12 { // 20/2 + 2
		t.Fatalf("even angel Mana = %d err=%v", even.Mana, err)
	}
}

func TestDeathPostVitalsInvalidInputs(t *testing.T) {
	if _, err := PlanPostDeathVitals(PostDeathVitalsInput{Vitals: mkVitals(0, 40, 45, 1, 25, 100), Disposition: DeathAvoided}); !errors.Is(err, ErrInvalidDeathInput) {
		t.Fatalf("avoided: err = %v", err)
	}
	if _, err := PlanPostDeathVitals(PostDeathVitalsInput{Vitals: mkVitals(0, 40, 45, 1, 25, 100), Disposition: DeathNormal, FrenzyActive: true, AngelMailEligible: true}); !errors.Is(err, ErrInvalidDeathInput) {
		t.Fatalf("frenzy+angel: err = %v", err)
	}
	if _, err := PlanPostDeathVitals(PostDeathVitalsInput{Vitals: PlayerVitals{HP: -1}, Disposition: DeathNormal}); !errors.Is(err, ErrInvalidVitals) {
		t.Fatalf("corrupt vitals: err = %v", err)
	}
	if _, err := PlanPostDeathVitals(PostDeathVitalsInput{Vitals: mkVitals(0, 40, 45, 1, 25, 100), Disposition: DeathDisposition(7)}); !errors.Is(err, ErrInvalidDeathInput) {
		t.Fatalf("hostile disposition: err = %v", err)
	}
}

// --- immediate advancement --------------------------------------------------

func TestDeathAdvancementNormalHalving(t *testing.T) {
	cases := []struct{ before, after int }{
		{0, 0},
		{1, 0},
		{25, 12}, // positive odd truncates down toward zero
		{24, 12},
		{-25, -12}, // negative odd truncates UP toward zero (KOD = C division)
		{-24, -12},
		{-1, 0},
	}
	for _, c := range cases {
		p, err := PlanDeathAdvancement(DeathNormal, 37, c.before)
		if err != nil {
			t.Fatalf("%d: %v", c.before, err)
		}
		if p.PointsAfter != 0 {
			t.Fatalf("%d: points = %d, want 0", c.before, p.PointsAfter)
		}
		if p.GainChanceAfter != c.after {
			t.Fatalf("gain %d: after = %d, want %d", c.before, p.GainChanceAfter, c.after)
		}
		if !p.ResetGainFlags || !p.ResetAtrophyFlags {
			t.Fatalf("%d: resets = %v/%v, want true/true", c.before, p.ResetGainFlags, p.ResetAtrophyFlags)
		}
	}
}

func TestDeathAdvancementCheapUntouched(t *testing.T) {
	p, err := PlanDeathAdvancement(DeathCheap, 37, -25)
	if err != nil {
		t.Fatalf("cheap: %v", err)
	}
	if p.PointsAfter != 37 || p.GainChanceAfter != -25 || p.ResetGainFlags || p.ResetAtrophyFlags {
		t.Fatalf("cheap advancement plan = %+v, want untouched echo", p)
	}
}

// --- ordered drop plan -------------------------------------------------------

func TestDropPlanMatrix(t *testing.T) {
	normal := mustPlan(t, 100, DeathContext{}, true)
	items := []DeathItemInput{
		{Key: 11, DropOnDeath: true, RoomAccepts: true},
		{Key: 22, DropOnDeath: false, RoomAccepts: true}, // undroppable policy
		{Key: 33, DropOnDeath: true, RoomAccepts: false}, // room rejects
		{Key: 44, DropOnDeath: true, RoomAccepts: true, SpecialItem: true},
	}
	plan, err := PlanDeathDrops(normal, items)
	if err != nil {
		t.Fatalf("normal drops: %v", err)
	}
	if len(plan.Items) != 4 {
		t.Fatalf("plan length = %d, want 4 (order preserved)", len(plan.Items))
	}
	wantDrop := []bool{true, false, false, true}
	wantPK := []bool{true, false, false, true}
	for i, it := range plan.Items {
		if it.Drop != wantDrop[i] {
			t.Fatalf("item %d: drop = %v, want %v", i, it.Drop, wantDrop[i])
		}
		if it.NeedsPKProtection != wantPK[i] {
			t.Fatalf("item %d: pk = %v, want %v", i, it.NeedsPKProtection, wantPK[i])
		}
		if it.Drop && it.PKProtectionDurationMs != 600000 {
			t.Fatalf("item %d: pk duration = %d", i, it.PKProtectionDurationMs)
		}
		if !it.Drop && it.PKProtectionDurationMs != 0 {
			t.Fatalf("item %d: undropped pk duration = %d", i, it.PKProtectionDurationMs)
		}
	}
	if plan.Items[0].Key != 11 || plan.Items[3].Key != 44 {
		t.Fatalf("order not preserved: %+v", plan.Items)
	}
	if !plan.Items[3].SpecialItem || plan.Items[3].SpecialItemKept {
		t.Fatalf("normal death must lose the special artifact: %+v", plan.Items[3])
	}

	// Non-PK killer: no protection metadata.
	npkPlan := mustPlan(t, 100, DeathContext{}, false)
	npk, err := PlanDeathDrops(npkPlan, items)
	if err != nil {
		t.Fatalf("non-pk: %v", err)
	}
	for _, it := range npk.Items {
		if it.NeedsPKProtection || it.PKProtectionDurationMs != 0 {
			t.Fatalf("non-pk protection leaked: %+v", it)
		}
	}

	// Cheap death: nothing drops, keep-guard outcome still echoes.
	cheapPlan := mustPlan(t, 100, DeathContext{FrenzyActive: true}, true)
	cheap, err := PlanDeathDrops(cheapPlan, items)
	if err != nil {
		t.Fatalf("cheap: %v", err)
	}
	for _, it := range cheap.Items {
		if it.Drop || it.NeedsPKProtection {
			t.Fatalf("cheap drop leaked: %+v", it)
		}
	}
	if !cheap.Items[3].SpecialItemKept {
		t.Fatalf("non-token cheap death must keep the special artifact")
	}

	// Pure token death: cheap, but the artifact is lost.
	tokenPlan := mustPlan(t, 100, DeathContext{CarriesToken: true}, true)
	token, err := PlanDeathDrops(tokenPlan, items)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if token.Items[3].SpecialItemKept {
		t.Fatalf("token death must lose the special artifact: %+v", token.Items[3])
	}
}

func TestDropPlanInvalidInputs(t *testing.T) {
	avoided := mustPlan(t, 100, DeathContext{PrisonRoom: true}, false)
	if _, err := PlanDeathDrops(avoided, nil); !errors.Is(err, ErrInvalidDeathInput) {
		t.Fatalf("avoided: err = %v", err)
	}
	hostile := DeathDispositionPlan{Disposition: DeathDisposition(9)}
	if _, err := PlanDeathDrops(hostile, nil); !errors.Is(err, ErrInvalidDeathInput) {
		t.Fatalf("hostile disposition: err = %v", err)
	}
	normal := mustPlan(t, 100, DeathContext{}, false)
	dup := []DeathItemInput{{Key: 5, DropOnDeath: true, RoomAccepts: true}, {Key: 5}}
	if _, err := PlanDeathDrops(normal, dup); !errors.Is(err, ErrInvalidDeathInput) {
		t.Fatalf("duplicate key: err = %v", err)
	}
	empty, err := PlanDeathDrops(normal, nil)
	if err != nil || len(empty.Items) != 0 {
		t.Fatalf("empty inventory: %+v err=%v", empty, err)
	}
}

// --- Portal of Life -----------------------------------------------------------

func TestPortalGoldenVectors(t *testing.T) {
	cases := []struct {
		name string
		in   PortalOfLifeInput
		want int
	}{
		{"age0 power50 cost100", PortalOfLifeInput{PendingCost: 100, CorpseAgeSeconds: 0, SpellPower: 50}, 5},
		{"age0 power1 cost100", PortalOfLifeInput{PendingCost: 100, CorpseAgeSeconds: 0, SpellPower: 1}, 39},
		{"age0 power99", PortalOfLifeInput{PendingCost: 100, CorpseAgeSeconds: 0, SpellPower: 99}, 5},
		{"age59 power1 cost100 (bound)", PortalOfLifeInput{PendingCost: 100, CorpseAgeSeconds: 59, SpellPower: 1}, 80},
		{"age59 power22 cost100 (under bound)", PortalOfLifeInput{PendingCost: 100, CorpseAgeSeconds: 59, SpellPower: 22}, 77},
		{"age59 power99 cost100", PortalOfLifeInput{PendingCost: 100, CorpseAgeSeconds: 59, SpellPower: 99}, 5},
		{"age60 power1 cost100 -> bound 80", PortalOfLifeInput{PendingCost: 100, CorpseAgeSeconds: 60, SpellPower: 1}, 80},
		{"age61 power50 cost100", PortalOfLifeInput{PendingCost: 100, CorpseAgeSeconds: 61, SpellPower: 50}, 50},
		{"age61 power1 cost6", PortalOfLifeInput{PendingCost: 6, CorpseAgeSeconds: 61, SpellPower: 1}, 5},
		{"age69 power1 cost100", PortalOfLifeInput{PendingCost: 100, CorpseAgeSeconds: 69, SpellPower: 1}, 80}, // 69/10-6=0 -> 99 -> 80
		{"age70 power50 cost100", PortalOfLifeInput{PendingCost: 100, CorpseAgeSeconds: 70, SpellPower: 50}, 51},
		{"age599 power1 cost100", PortalOfLifeInput{PendingCost: 100, CorpseAgeSeconds: 599, SpellPower: 1}, 80},
		{"age600 lifetime edge", PortalOfLifeInput{PendingCost: 100, CorpseAgeSeconds: 600, SpellPower: 1}, 80},
		{"age600 power70 cost50", PortalOfLifeInput{PendingCost: 50, CorpseAgeSeconds: 600, SpellPower: 70}, 34},
		{"reduced pending 80 age0 power99", PortalOfLifeInput{PendingCost: 80, CorpseAgeSeconds: 0, SpellPower: 99}, 5},
		{"reduced pending 5 age0 power1", PortalOfLifeInput{PendingCost: 5, CorpseAgeSeconds: 0, SpellPower: 1}, 5}, // 5-61<5
		{"lower bound exact", PortalOfLifeInput{PendingCost: 61, CorpseAgeSeconds: 0, SpellPower: 1}, 5},            // 61-61=0 -> 5
		{"upper near-bound", PortalOfLifeInput{PendingCost: 80, CorpseAgeSeconds: 60, SpellPower: 1}, 79},
	}
	for _, c := range cases {
		got, err := PlanPortalOfLife(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Fatalf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
	// The 80 ceiling is reached by clamping (exact 80 needs power 0,
	// which is invalid): pinned twice above via age59/age60/age599/
	// age600 vectors whose raw results exceed it.
	got, err := PlanPortalOfLife(PortalOfLifeInput{PendingCost: 100, CorpseAgeSeconds: 599, SpellPower: 1})
	if err != nil || got != 80 {
		t.Fatalf("clamp-80 vector: got %d err=%v, want 80", got, err)
	}
}

func TestPortalInvalidInputs(t *testing.T) {
	bad := map[string]PortalOfLifeInput{
		"power 0":      {PendingCost: 100, CorpseAgeSeconds: 10, SpellPower: 0},
		"power 100":    {PendingCost: 100, CorpseAgeSeconds: 10, SpellPower: 100},
		"negative age": {PendingCost: 100, CorpseAgeSeconds: -1, SpellPower: 50},
		"cost 101":     {PendingCost: 101, CorpseAgeSeconds: 10, SpellPower: 50},
		"cost -1":      {PendingCost: -1, CorpseAgeSeconds: 10, SpellPower: 50},
		"used corpse":  {PendingCost: 100, CorpseAgeSeconds: 10, SpellPower: 50, CorpseAlreadyUsed: true},
	}
	for name, in := range bad {
		if _, err := PlanPortalOfLife(in); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}

func TestPortalReducePendingLowersOnly(t *testing.T) {
	cases := []struct{ current, proposed, want int }{
		{100, 80, 80},
		{80, 90, 80},
		{80, 80, 80},
		{0, 0, 0},
		{41, 5, 5},
	}
	for _, c := range cases {
		got, err := ReducePendingDeathCost(c.current, c.proposed)
		if err != nil {
			t.Fatalf("%d/%d: %v", c.current, c.proposed, err)
		}
		if got != c.want {
			t.Fatalf("reduce %d->%d: got %d, want %d", c.current, c.proposed, got, c.want)
		}
	}
	if _, err := ReducePendingDeathCost(101, 5); !errors.Is(err, ErrInvalidDeathCost) {
		t.Fatalf("current 101: err = %v", err)
	}
	if _, err := ReducePendingDeathCost(100, -1); !errors.Is(err, ErrInvalidDeathCost) {
		t.Fatalf("proposed -1: err = %v", err)
	}
}

// --- delayed penalties: cost scaling -------------------------------------------

func TestDeathPenaltyCostScaling(t *testing.T) {
	base := mkVitals(10, 40, 45, 5, 20, 100)

	// Still-newbie non-murderer: cost/3, NO HP roll. The one eligible
	// ability still consumes its stamina roll (30 == 30 saves, so no
	// cost roll follows) — proving the first consumed roll is NOT the
	// HP roll.
	rng := d100Rolls(30)
	p, err := PlanDeathPenalties(rng, DeathPenaltyInput{
		PendingCost: 100, DefaultCost: 100, StillNewbie: true, Murderer: false,
		Stamina: 30, Vitals: base,
		Spells: []DeathAbilityInput{{Key: 1, Ability: 6}},
	})
	if err != nil {
		t.Fatalf("newbie: %v", err)
	}
	if !p.CostScaled || p.ScaledCost != 33 {
		t.Fatalf("newbie scaling: %+v", p)
	}
	if p.HPRolled {
		t.Fatalf("newbie path must not roll HP")
	}
	if rng.at != 1 || len(p.AbilityLosses) != 0 {
		t.Fatalf("newbie path consumed %d draws with %d losses, want 1/0", rng.at, len(p.AbilityLosses))
	}
	_ = p

	// Ordinary experienced non-murderer: no scaling, HP roll happens.
	p2, err := PlanDeathPenalties(d100Rolls(100, 1, 1), DeathPenaltyInput{
		PendingCost: 100, DefaultCost: 100, StillNewbie: false, Murderer: false,
		Stamina: 30, Vitals: base,
	})
	if err != nil {
		t.Fatalf("experienced: %v", err)
	}
	if p2.CostScaled || p2.ScaledCost != 100 || !p2.HPRolled || p2.HPRoll != 100 || p2.LostBaseMaxHP != 1 {
		t.Fatalf("experienced scaling: %+v", p2)
	}

	// Murderer (even still-newbie flagged true): else-branch rolls HP.
	p3, err := PlanDeathPenalties(d100Rolls(50, 1), DeathPenaltyInput{
		PendingCost: 90, DefaultCost: 100, StillNewbie: true, Murderer: true,
		Stamina: 30, Vitals: base,
	})
	if err != nil {
		t.Fatalf("murderer: %v", err)
	}
	if p3.CostScaled || !p3.HPRolled || p3.LostBaseMaxHP != 1 {
		t.Fatalf("murderer scaling: %+v", p3)
	}

	// Zero cost (cheap death exit): no scaling, no HP roll.
	p4, err := PlanDeathPenalties(d100Rolls(), DeathPenaltyInput{
		PendingCost: 0, DefaultCost: 100,
		Stamina: 30, Vitals: base,
	})
	if err != nil {
		t.Fatalf("zero: %v", err)
	}
	if p4.CostScaled || p4.HPRolled || p4.ScaledCost != 0 {
		t.Fatalf("zero scaling: %+v", p4)
	}

	// Small-cost newbie truncation: 2/3 = 0, 1/3 = 0.
	for _, cost := range []int{1, 2} {
		pn, err := PlanDeathPenalties(d100Rolls(), DeathPenaltyInput{
			PendingCost: cost, DefaultCost: 100, StillNewbie: true,
			Stamina: 30, Vitals: base,
		})
		if err != nil || !pn.CostScaled || pn.ScaledCost != 0 {
			t.Fatalf("cost %d newbie: %+v err=%v", cost, pn, err)
		}
	}
	// 90/3 = 30, 61/3 = 20.
	for _, tc := range []struct{ cost, want int }{{90, 30}, {61, 20}} {
		pn, err := PlanDeathPenalties(d100Rolls(), DeathPenaltyInput{
			PendingCost: tc.cost, DefaultCost: 100, StillNewbie: true,
			Stamina: 30, Vitals: base,
		})
		if err != nil || pn.ScaledCost != tc.want {
			t.Fatalf("cost %d newbie: scaled %d err=%v", tc.cost, pn.ScaledCost, err)
		}
	}
}

// --- delayed penalties: HP roll boundary ----------------------------------------

func TestDeathPenaltyHPRollBoundary(t *testing.T) {
	base := mkVitals(10, 40, 45, 5, 20, 100)
	mk := func(roll int) DeathPenaltyInput {
		return DeathPenaltyInput{
			PendingCost: 60, DefaultCost: 100, Stamina: 30, Vitals: base,
		}
	}
	// roll == cost LOSES (boundary `<=`).
	p, err := PlanDeathPenalties(d100Rolls(60), mk(60))
	if err != nil {
		t.Fatalf("roll==cost: %v", err)
	}
	if !p.HPRolled || p.HPRoll != 60 || p.LostBaseMaxHP != 1 {
		t.Fatalf("roll==cost must lose: %+v", p)
	}
	if p.VitalsAfter.BaseMaxHP != 39 || p.VitalsAfter.MaxHP != 44 {
		t.Fatalf("T4a composition wrong: %+v", p.VitalsAfter)
	}
	if p.VitalsAfter.HP != 10 {
		t.Fatalf("current HP must be untouched: %+v", p.VitalsAfter)
	}
	// roll == cost+1 SAVES.
	p, err = PlanDeathPenalties(d100Rolls(61), mk(61))
	if err != nil {
		t.Fatalf("roll==cost+1: %v", err)
	}
	if p.LostBaseMaxHP != 0 || p.VitalsAfter.BaseMaxHP != 40 {
		t.Fatalf("roll==cost+1 must save: %+v", p)
	}
	// Base floor 20: losing at base 20 yields delta 0 (minimum pinned).
	floorV := mkVitals(10, 20, 20, 5, 20, 100)
	pf, err := PlanDeathPenalties(d100Rolls(1), DeathPenaltyInput{
		PendingCost: 60, DefaultCost: 100, Stamina: 30, Vitals: floorV,
	})
	if err != nil {
		t.Fatalf("floor: %v", err)
	}
	if pf.LostBaseMaxHP != 0 || pf.VitalsAfter.BaseMaxHP != 20 || pf.VitalsAfter.MaxHP != 20 {
		t.Fatalf("floor behavior: %+v", pf.VitalsAfter)
	}
	// The HP roll happens BEFORE any ability roll (frozen order).
	seq, err := PlanDeathPenalties(d100Rolls(1, 100, 1), DeathPenaltyInput{
		PendingCost: 60, DefaultCost: 100, Stamina: 30, Vitals: base,
		Spells: []DeathAbilityInput{{Key: 7, Ability: 50}},
	})
	if err != nil {
		t.Fatalf("order: %v", err)
	}
	if !seq.HPRolled || seq.HPRoll != 1 {
		t.Fatalf("first roll must be the HP roll: %+v", seq)
	}
	if len(seq.AbilityLosses) != 1 || seq.AbilityLosses[0].StaminaRoll != 100 || seq.AbilityLosses[0].CostRoll != 1 {
		t.Fatalf("subsequent rolls must feed the ability: %+v", seq.AbilityLosses)
	}
}

// --- delayed penalties: ability eligibility, saves, clamps -----------------------

func TestDeathPenaltyAbilityEligibility(t *testing.T) {
	base := mkVitals(10, 40, 45, 5, 20, 100)
	// Ability 5 is NOT eligible: no roll consumed at all (newbie path
	// selected so no HP roll fires either).
	rng := d100Rolls(100)
	p, err := PlanDeathPenalties(rng, DeathPenaltyInput{
		PendingCost: 100, DefaultCost: 100, StillNewbie: true,
		Stamina: 30, Vitals: base,
		Spells: []DeathAbilityInput{{Key: 1, Ability: 5}},
	})
	if err != nil {
		t.Fatalf("ability 5: %v", err)
	}
	if len(p.AbilityLosses) != 0 || rng.at != 0 {
		t.Fatalf("ability 5 must consume nothing: losses=%d draws=%d", len(p.AbilityLosses), rng.at)
	}
	// Ability 6 and 99 ARE eligible (newbie path: no HP roll; both
	// stamina saves fail and both cost rolls lose).
	rng2 := d100Rolls(100, 1, 100, 1)
	p2, err := PlanDeathPenalties(rng2, DeathPenaltyInput{
		PendingCost: 100, DefaultCost: 100, StillNewbie: true,
		Stamina: 30, Vitals: base,
		Spells: []DeathAbilityInput{{Key: 1, Ability: 6}, {Key: 2, Ability: 99}},
	})
	if err != nil {
		t.Fatalf("eligible: %v", err)
	}
	if len(p2.AbilityLosses) != 2 {
		t.Fatalf("eligible abilities must lose here: %+v", p2.AbilityLosses)
	}
}

func TestDeathPenaltyStaminaSaveBoundary(t *testing.T) {
	base := mkVitals(10, 40, 45, 5, 20, 100)
	// roll == Stamina SAVES: no cost roll consumed (newbie path so the
	// HP roll never fires).
	rng := d100Rolls(30)
	p, err := PlanDeathPenalties(rng, DeathPenaltyInput{
		PendingCost: 100, DefaultCost: 100, StillNewbie: true,
		Stamina: 30, Vitals: base,
		Spells: []DeathAbilityInput{{Key: 1, Ability: 50}},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(p.AbilityLosses) != 0 || rng.at != 1 {
		t.Fatalf("roll==Stamina must save after exactly one draw: losses=%d draws=%d", len(p.AbilityLosses), rng.at)
	}
	// roll == Stamina+1 fails the save and consumes the cost roll
	// (scaled cost 33; a cost roll of 1 loses).
	rng2 := d100Rolls(31, 1)
	p2, err := PlanDeathPenalties(rng2, DeathPenaltyInput{
		PendingCost: 100, DefaultCost: 100, StillNewbie: true,
		Stamina: 30, Vitals: base,
		Spells: []DeathAbilityInput{{Key: 1, Ability: 50}},
	})
	if err != nil {
		t.Fatalf("fail: %v", err)
	}
	if len(p2.AbilityLosses) != 1 || rng2.at != 2 {
		t.Fatalf("roll==Stamina+1 must fail and roll cost: %+v draws=%d", p2.AbilityLosses, rng2.at)
	}
}

func TestDeathPenaltyCostRollBoundary(t *testing.T) {
	base := mkVitals(10, 40, 45, 5, 20, 100)
	// roll == cost SAVES; roll == cost-1 LOSES. Cost 33 exercises the
	// newbie-scaled value too.
	save, err := PlanDeathPenalties(d100Rolls(100, 33), DeathPenaltyInput{
		PendingCost: 100, DefaultCost: 100, StillNewbie: true,
		Stamina: 30, Vitals: base,
		Spells: []DeathAbilityInput{{Key: 1, Ability: 50}},
	})
	if err != nil {
		t.Fatalf("cost save: %v", err)
	}
	if len(save.AbilityLosses) != 0 {
		t.Fatalf("roll==cost must save: %+v", save.AbilityLosses)
	}
	lose, err := PlanDeathPenalties(d100Rolls(100, 32), DeathPenaltyInput{
		PendingCost: 100, DefaultCost: 100, StillNewbie: true,
		Stamina: 30, Vitals: base,
		Spells: []DeathAbilityInput{{Key: 1, Ability: 50}},
	})
	if err != nil {
		t.Fatalf("cost lose: %v", err)
	}
	if len(lose.AbilityLosses) != 1 {
		t.Fatalf("roll==cost-1 must lose: %+v", lose.AbilityLosses)
	}
}

func TestDeathPenaltyLossAmountsAndClamp(t *testing.T) {
	base := mkVitals(10, 40, 45, 5, 20, 100)
	// Ordinary loss = -1 (newbie path: no HP roll; scaled cost 33).
	p, err := PlanDeathPenalties(d100Rolls(100, 1), DeathPenaltyInput{
		PendingCost: 100, DefaultCost: 100, StillNewbie: true,
		Stamina: 30, Vitals: base,
		Spells: []DeathAbilityInput{{Key: 1, Ability: 50}},
	})
	if err != nil {
		t.Fatalf("ordinary: %v", err)
	}
	l := p.AbilityLosses[0]
	if l.FromAbility != 50 || l.ToAbility != 49 || l.Loss != 1 {
		t.Fatalf("ordinary loss: %+v", l)
	}
	// Murderer loss = -2 (murderer forces the else branch: HP roll
	// first, then stamina fail, then cost lose); eligible 6 ->
	// bound(6-2,1,99) = 4 (clamp is 1, NOT the eligibility floor 5).
	p2, err := PlanDeathPenalties(d100Rolls(100, 100, 1), DeathPenaltyInput{
		PendingCost: 100, DefaultCost: 100, StillNewbie: true, Murderer: true,
		Stamina: 30, Vitals: base,
		Spells: []DeathAbilityInput{{Key: 1, Ability: 6}},
	})
	if err != nil {
		t.Fatalf("murderer clamp: %v", err)
	}
	if !p2.HPRolled {
		t.Fatalf("murderer must route to the HP-roll branch")
	}
	l2 := p2.AbilityLosses[0]
	if l2.FromAbility != 6 || l2.ToAbility != 4 || l2.Loss != 2 {
		t.Fatalf("murderer 6-2 must clamp to 4: %+v", l2)
	}
	// Murderer at ability 1 is ineligible anyway (1 <= 5), so the
	// floor-1 clamp is unreachable live; the bound still holds by
	// construction (pinned via the property test below).
}

func TestDeathPenaltySpellsAndSkillsSeparate(t *testing.T) {
	base := mkVitals(10, 40, 45, 5, 20, 100)
	// Full sequence (newbie path, scaled cost 33, no HP roll): spell
	// loses, skill saves, skill loses.
	p, err := PlanDeathPenalties(d100Rolls(100, 1, 30, 100, 1), DeathPenaltyInput{
		PendingCost: 100, DefaultCost: 100, StillNewbie: true,
		Stamina: 30, Vitals: base,
		Spells: []DeathAbilityInput{{Key: 101, Ability: 50}},
		Skills: []DeathAbilityInput{{Key: 201, Ability: 60}, {Key: 202, Ability: 70}},
	})
	if err != nil {
		t.Fatalf("separate: %v", err)
	}
	if len(p.AbilityLosses) != 2 {
		t.Fatalf("expected spell loss + second skill loss: %+v", p.AbilityLosses)
	}
	if p.AbilityLosses[0].Key != 101 {
		t.Fatalf("spells roll before skills: %+v", p.AbilityLosses)
	}
	if p.AbilityLosses[1].Key != 202 {
		t.Fatalf("saved skill excluded, order kept: %+v", p.AbilityLosses)
	}
}

// --- delayed penalties: RNG consumption order -----------------------------------

func TestDeathPenaltyRNGConsumptionOrder(t *testing.T) {
	base := mkVitals(10, 40, 45, 5, 20, 100)
	in := DeathPenaltyInput{
		PendingCost: 100, DefaultCost: 100, Stamina: 50, Vitals: base,
		Spells: []DeathAbilityInput{
			{Key: 1, Ability: 4},  // ineligible: NO draw
			{Key: 2, Ability: 50}, // stamina 51 fails, cost 100 loses
			{Key: 3, Ability: 60}, // stamina 50 saves: NO cost draw
		},
		Skills: []DeathAbilityInput{
			{Key: 4, Ability: 5},  // ineligible: NO draw
			{Key: 5, Ability: 70}, // stamina 99 fails, cost 100 loses
		},
	}
	// Draws in exact order: HP(100 loses), spell2 stam(51), spell2
	// cost(1), spell3 stam(50 saves), skill5 stam(99), skill5 cost(1).
	rng := d100Rolls(100, 51, 1, 50, 99, 1, 77, 77)
	p, err := PlanDeathPenalties(rng, in)
	if err != nil {
		t.Fatalf("order: %v", err)
	}
	if rng.at != 6 {
		t.Fatalf("consumed %d draws, want 6 (trailing 77s untouched)", rng.at)
	}
	if len(p.AbilityLosses) != 2 || p.AbilityLosses[0].Key != 2 || p.AbilityLosses[1].Key != 5 {
		t.Fatalf("losses: %+v", p.AbilityLosses)
	}
	if !p.HPRolled || p.LostBaseMaxHP != 1 {
		t.Fatalf("hp: %+v", p)
	}

	// Zero-cost exit still consumes stamina draws (and cost draws on
	// failed saves) for eligible abilities: source evaluates the full
	// short-circuit chain.
	rng0 := d100Rolls(60, 70) // stamina 60 > 50 fails, cost 70 < 0? no: 70 < 0 false
	p0, err := PlanDeathPenalties(rng0, DeathPenaltyInput{
		PendingCost: 0, DefaultCost: 100, Stamina: 50, Vitals: base,
		Spells: []DeathAbilityInput{{Key: 1, Ability: 50}},
	})
	if err != nil {
		t.Fatalf("zero-cost: %v", err)
	}
	if rng0.at != 2 || len(p0.AbilityLosses) != 0 {
		t.Fatalf("zero-cost consumption: draws=%d losses=%d, want 2/0", rng0.at, len(p0.AbilityLosses))
	}
}

// --- delayed penalties: hook flags ------------------------------------------------

func TestDeathPenaltyHookFlags(t *testing.T) {
	base := mkVitals(10, 40, 45, 5, 20, 100)

	// Full cost: outlaw + haunted cleared, PK re-evaluated.
	full, err := PlanDeathPenalties(d100Rolls(), DeathPenaltyInput{
		PendingCost: 100, DefaultCost: 100, Stamina: 30, Vitals: base,
	})
	if err != nil {
		t.Fatalf("full: %v", err)
	}
	if !full.ClearOutlaw || !full.ClearHaunted || !full.ReevaluatePK {
		t.Fatalf("full-cost clears: %+v", full)
	}

	// Portal-reduced cost below default: no clears.
	reduced, err := PlanDeathPenalties(d100Rolls(100), DeathPenaltyInput{
		PendingCost: 80, DefaultCost: 100, Stamina: 30, Vitals: base,
	})
	if err != nil {
		t.Fatalf("reduced: %v", err)
	}
	if reduced.ClearOutlaw || reduced.ClearHaunted || reduced.ReevaluatePK {
		t.Fatalf("reduced cost must not clear: %+v", reduced)
	}

	// Frenzy at exit: ONLY the haunted clear, nothing else, no rolls.
	frenzy, err := PlanDeathPenalties(d100Rolls(), DeathPenaltyInput{
		PendingCost: 100, DefaultCost: 100, FrenzyActive: true,
		Stamina: 30, Vitals: base,
		Spells: []DeathAbilityInput{{Key: 1, Ability: 90}},
	})
	if err != nil {
		t.Fatalf("frenzy: %v", err)
	}
	if !frenzy.ClearHaunted || frenzy.ClearOutlaw || frenzy.ReevaluatePK ||
		frenzy.HPRolled || len(frenzy.AbilityLosses) != 0 || !frenzy.ClearPending {
		t.Fatalf("frenzy exit: %+v", frenzy)
	}

	// Guild quit: base 31 loses 1 -> 30 -> 30 < 30 is FALSE -> stay.
	stay, err := PlanDeathPenalties(d100Rolls(1), DeathPenaltyInput{
		PendingCost: 100, DefaultCost: 100, Stamina: 30,
		Vitals: mkVitals(10, 31, 31, 5, 20, 100),
	})
	if err != nil {
		t.Fatalf("guild stay: %v", err)
	}
	if stay.QuitGuild || stay.VitalsAfter.BaseMaxHP != 30 {
		t.Fatalf("base 31 -> 30 must not quit: %+v", stay)
	}
	// Base 30 loses 1 -> 29 -> quit.
	quit, err := PlanDeathPenalties(d100Rolls(1), DeathPenaltyInput{
		PendingCost: 100, DefaultCost: 100, Stamina: 30,
		Vitals: mkVitals(10, 30, 30, 5, 20, 100),
	})
	if err != nil {
		t.Fatalf("guild quit: %v", err)
	}
	if !quit.QuitGuild || quit.VitalsAfter.BaseMaxHP != 29 {
		t.Fatalf("base 30 -> 29 must quit: %+v", quit)
	}
	// Cheap (cost 0) exit: guild check STILL runs — base 20 stays 20
	// and 20 < 30 quits.
	cheap, err := PlanDeathPenalties(d100Rolls(), DeathPenaltyInput{
		PendingCost: 0, DefaultCost: 100, Stamina: 30,
		Vitals: mkVitals(1, 20, 20, 1, 20, 50),
	})
	if err != nil {
		t.Fatalf("cheap guild: %v", err)
	}
	if !cheap.QuitGuild {
		t.Fatalf("cost-0 exit below 30 must still flag guild quit: %+v", cheap)
	}
}

func TestDeathPenaltyInvalidInputs(t *testing.T) {
	base := mkVitals(10, 40, 45, 5, 20, 100)
	if _, err := PlanDeathPenalties(nil, DeathPenaltyInput{PendingCost: 0, DefaultCost: 100, Stamina: 30, Vitals: base}); !errors.Is(err, ErrNilRNG) {
		t.Fatalf("nil rng: err = %v", err)
	}
	bads := []DeathPenaltyInput{
		{PendingCost: 101, DefaultCost: 100, Stamina: 30, Vitals: base},
		{PendingCost: -1, DefaultCost: 100, Stamina: 30, Vitals: base},
		{PendingCost: 100, DefaultCost: 0, Stamina: 30, Vitals: base},
		{PendingCost: 100, DefaultCost: 101, Stamina: 30, Vitals: base},
		{PendingCost: 100, DefaultCost: 100, Stamina: 0, Vitals: base},
		{PendingCost: 100, DefaultCost: 100, Stamina: 71, Vitals: base},
		{PendingCost: 100, DefaultCost: 100, Stamina: 30, Vitals: PlayerVitals{HP: -1}},
		{PendingCost: 100, DefaultCost: 100, Stamina: 30, Vitals: base, Spells: []DeathAbilityInput{{Key: 1, Ability: 0}}},
		{PendingCost: 100, DefaultCost: 100, Stamina: 30, Vitals: base, Spells: []DeathAbilityInput{{Key: 1, Ability: 100}}},
		{PendingCost: 100, DefaultCost: 100, Stamina: 30, Vitals: base, Spells: []DeathAbilityInput{{Key: 1}, {Key: 1}}},
		{PendingCost: 100, DefaultCost: 100, Stamina: 30, Vitals: base, Skills: []DeathAbilityInput{{Key: 2}, {Key: 2}}},
	}
	for i, in := range bads {
		if _, err := PlanDeathPenalties(d100Rolls(), in); err == nil {
			t.Fatalf("case %d: expected validation error, got nil", i)
		}
	}
	// Validation precedes RNG: no draws on invalid input.
	rng := d100Rolls()
	_, _ = PlanDeathPenalties(rng, bads[0])
	if rng.at != 0 {
		t.Fatalf("invalid input consumed %d draws", rng.at)
	}
}

// --- pending-death plan ------------------------------------------------------------

func TestDeathPendingPlanMatrix(t *testing.T) {
	corpse, err := PlanCorpse(999)
	if err != nil {
		t.Fatalf("corpse: %v", err)
	}
	normal := mustPlan(t, 90, DeathContext{}, false)
	pend, err := PlanPendingDeath(normal, corpse)
	if err != nil {
		t.Fatalf("normal pending: %v", err)
	}
	if pend.Phase != DeathPhasePending || pend.EffectiveDeathCost != 90 ||
		pend.DeathTimeSeconds != 999 || pend.Corpse.DeathTimeSeconds != 999 {
		t.Fatalf("normal pending: %+v", pend)
	}
	cheap := mustPlan(t, 90, DeathContext{FrenzyActive: true}, false)
	pendCheap, err := PlanPendingDeath(cheap, corpse)
	if err != nil {
		t.Fatalf("cheap pending: %v", err)
	}
	if pendCheap.Phase != DeathPhasePending || pendCheap.EffectiveDeathCost != 0 {
		t.Fatalf("cheap pending: %+v", pendCheap)
	}
	avoided := mustPlan(t, 90, DeathContext{PrisonRoom: true}, false)
	pendNone, err := PlanPendingDeath(avoided, corpse)
	if err != nil {
		t.Fatalf("avoided pending: %v", err)
	}
	if pendNone.Phase != DeathPhaseNone {
		t.Fatalf("avoided pending: %+v", pendNone)
	}
	// Foreign corpse policy is rejected.
	if _, err := PlanPendingDeath(normal, CorpsePolicy{LifetimeMs: 1, NoStealMs: 1, DeathTimeSeconds: 1}); !errors.Is(err, ErrInvalidDeathInput) {
		t.Fatalf("foreign corpse: err = %v", err)
	}
}

// --- karma booby prize ---------------------------------------------------------------

func TestDeathKarmaBoobyPrize(t *testing.T) {
	cases := []struct {
		karma  int32
		newbie bool
		want   KarmaPrizeKind
	}{
		{5001, false, KarmaPrizeHammer},
		{5001, true, KarmaPrizeMace},  // newbie home substitutes the mace
		{5000, false, KarmaPrizeMace}, // strict > 5000
		{10000, false, KarmaPrizeHammer},
		{0, false, KarmaPrizeMace},
		{-1, false, KarmaPrizeNone},
		{-10000, false, KarmaPrizeNone},
	}
	for _, c := range cases {
		got, err := PlanKarmaBoobyPrize(c.karma, c.newbie)
		if err != nil {
			t.Fatalf("%d/%v: %v", c.karma, c.newbie, err)
		}
		if got != c.want {
			t.Fatalf("%d/%v: got %d, want %d", c.karma, c.newbie, got, c.want)
		}
	}
	for _, bad := range []int32{10001, -10001} {
		if _, err := PlanKarmaBoobyPrize(bad, false); !errors.Is(err, ErrInvalidDeathInput) {
			t.Fatalf("karma %d: err = %v", bad, err)
		}
	}
}

// --- property / totality ---------------------------------------------------------------

func TestDeathProperties(t *testing.T) {
	// Portal totality: every (cost, power, age) result is 5..80.
	for cost := 0; cost <= 100; cost++ {
		for power := 1; power <= 99; power += 7 {
			for age := int64(0); age <= 700; age += 13 {
				got, err := PlanPortalOfLife(PortalOfLifeInput{PendingCost: cost, CorpseAgeSeconds: age, SpellPower: power})
				if err != nil {
					t.Fatalf("portal %d/%d/%d: %v", cost, power, age, err)
				}
				if got < 5 || got > 80 {
					t.Fatalf("portal %d/%d/%d = %d outside 5..80", cost, power, age, got)
				}
			}
		}
	}
	for _, hostile := range []int64{math.MaxInt64, 1 << 40} {
		if got, err := PlanPortalOfLife(PortalOfLifeInput{PendingCost: 100, CorpseAgeSeconds: hostile, SpellPower: 99}); err != nil || got != 80 {
			t.Fatalf("hostile age %d: %d %v", hostile, got, err)
		}
	}

	// Post-death vitals totality over the vigor domain.
	for vigor := 1; vigor <= 200; vigor++ {
		v := mkVitals(0, 40, 45, 0, 25, vigor)
		for _, disp := range []DeathDisposition{DeathCheap, DeathNormal} {
			got, err := PlanPostDeathVitals(PostDeathVitalsInput{Vitals: v, Disposition: disp})
			if err != nil {
				t.Fatalf("vigor %d %v: %v", vigor, disp, err)
			}
			if err := got.Validate(); err != nil {
				t.Fatalf("vigor %d %v invalid: %v", vigor, disp, err)
			}
			if got.HP != 1 || got.Mana != 1 {
				t.Fatalf("vigor %d %v: hp/mana", vigor, disp)
			}
			want := vigor / 4
			if want > 50 {
				want = 50
			}
			if want < 1 {
				want = 1
			}
			if got.Vigor != want {
				t.Fatalf("vigor %d: got %d want %d", vigor, got.Vigor, want)
			}
		}
		// Frenzy branch stays Validate-clean too.
		f, err := PlanPostDeathVitals(PostDeathVitalsInput{Vitals: v, Disposition: DeathCheap, FrenzyActive: true})
		if err != nil {
			t.Fatalf("frenzy vigor %d: %v", vigor, err)
		}
		if err := f.Validate(); err != nil {
			t.Fatalf("frenzy vigor %d invalid: %v", vigor, err)
		}
	}

	// Penalty totality over stamina/ability/cost domains with a fixed
	// rolling RNG.
	fixed := &scriptRNG{vals: []uint64{41}} // always rolls 42
	for stamina := 1; stamina <= 70; stamina += 3 {
		for cost := 0; cost <= 100; cost += 10 {
			for ability := 1; ability <= 99; ability += 4 {
				in := DeathPenaltyInput{
					PendingCost: cost, DefaultCost: 100, Stamina: stamina,
					Vitals: mkVitals(20, 40, 40, 5, 20, 100),
					Spells: []DeathAbilityInput{{Key: 1, Ability: ability}},
					Skills: []DeathAbilityInput{{Key: 2, Ability: ability}},
				}
				p, err := PlanDeathPenalties(fixed, in)
				if err != nil {
					t.Fatalf("%d/%d/%d: %v", stamina, cost, ability, err)
				}
				if err := p.VitalsAfter.Validate(); err != nil {
					t.Fatalf("vitals after invalid: %v", err)
				}
				if p.LostBaseMaxHP != 0 && p.LostBaseMaxHP != 1 {
					t.Fatalf("lost base = %d", p.LostBaseMaxHP)
				}
				for _, l := range p.AbilityLosses {
					if l.ToAbility < 1 || l.ToAbility > 99 || l.Loss < 1 || l.Loss > 2 {
						t.Fatalf("loss out of domain: %+v", l)
					}
					if l.ToAbility >= l.FromAbility {
						t.Fatalf("loss not a loss: %+v", l)
					}
				}
				if p.ScaledCost < 0 || p.ScaledCost > 100 {
					t.Fatalf("scaled cost = %d", p.ScaledCost)
				}
			}
		}
	}

	// Death-cost guard totality on hostile-but-valid scalars.
	if ok, err := DeathBlockedByDoubleDeath(1<<62, 1<<62+2); err != nil || ok {
		t.Fatalf("large-time guard: %v %v", ok, err)
	}
}

// --- immediate hooks: guardian angel mail -------------------------------------

func TestImmediateDeathHooksGuardianAngelMail(t *testing.T) {
	// The hook decision must equal the frozen gate for every combination,
	// and must match the §9.5.7 mana override exactly.
	for _, cost := range []int{0, 1, 50, 100} {
		for _, newbie := range []bool{false, true} {
			for _, murderer := range []bool{false, true} {
				var plan DeathDispositionPlan
				var disp DeathDisposition
				if cost == 0 {
					plan = mustPlan(t, 100, DeathContext{FrenzyActive: true}, false)
					disp = DeathCheap
				} else {
					plan = DeathDispositionPlan{Disposition: DeathNormal, DeathCost: cost}
					disp = DeathNormal
				}
				hooks, err := PlanImmediateDeathHooks(plan, ImmediateDeathHooksInput{
					StillNewbie: newbie, Murderer: murderer,
				})
				if err != nil {
					t.Fatalf("cost %d newbie %v murderer %v: %v", cost, newbie, murderer, err)
				}
				want := cost > 0 && newbie && !murderer
				if hooks.GuardianAngelMail != want {
					t.Fatalf("cost %d newbie %v murderer %v: mail = %v, want %v",
						cost, newbie, murderer, hooks.GuardianAngelMail, want)
				}
				if GuardianAngelMailEligible(cost, newbie, murderer) != want {
					t.Fatalf("helper drifts: cost %d newbie %v murderer %v", cost, newbie, murderer)
				}
				// The mana override consumes the same eligibility: mail
				// true iff mana becomes MaxMana/2+2.
				v, err := PlanPostDeathVitals(PostDeathVitalsInput{
					Vitals:      mkVitals(0, 40, 45, 1, 25, 100),
					Disposition: disp, AngelMailEligible: want,
				})
				if err != nil {
					t.Fatalf("vitals cost %d newbie %v murderer %v: %v", cost, newbie, murderer, err)
				}
				manaOverridden := v.Mana == 25/2+2
				if manaOverridden != hooks.GuardianAngelMail {
					t.Fatalf("cost %d newbie %v murderer %v: mail=%v mana-override=%v (mana=%d)",
						cost, newbie, murderer, hooks.GuardianAngelMail, manaOverridden, v.Mana)
				}
			}
		}
	}

	// Frenzy with an eligible gate is contradictory (same rule as vitals).
	normal := DeathDispositionPlan{Disposition: DeathNormal, DeathCost: 100}
	if _, err := PlanImmediateDeathHooks(normal, ImmediateDeathHooksInput{
		FrenzyActive: true, StillNewbie: true,
	}); !errors.Is(err, ErrInvalidDeathInput) {
		t.Fatalf("frenzy+eligible: err = %v", err)
	}
	// Invalid disposition plan is rejected, with zero output.
	if hooks, err := PlanImmediateDeathHooks(
		DeathDispositionPlan{Disposition: DeathCheap, DeathCost: 100},
		ImmediateDeathHooksInput{},
	); !errors.Is(err, ErrInvalidDeathInput) || hooks != (ImmediateDeathHooks{}) {
		t.Fatalf("hostile plan: hooks=%+v err=%v", hooks, err)
	}
}

// --- immediate hooks: soldier shield ------------------------------------------

func TestImmediateDeathHooksSoldierShield(t *testing.T) {
	normal := mustPlan(t, 100, DeathContext{}, false)
	cheap := mustPlan(t, 100, DeathContext{FrenzyActive: true}, false)
	avoided := mustPlan(t, 100, DeathContext{PrisonRoom: true}, false)
	cases := []struct {
		name string
		plan DeathDispositionPlan
		in   ImmediateDeathHooksInput
		want SoldierShieldDeathEffect
	}{
		{"rank1-enemy-delete", normal,
			ImmediateDeathHooksInput{HasSoldierShield: true, SoldierShieldRank: 1, KilledByShieldEnemy: true},
			SoldierShieldDeathEffect{Triggered: true, Delete: true}},
		{"rank3-enemy-delete", normal,
			ImmediateDeathHooksInput{HasSoldierShield: true, SoldierShieldRank: 3, KilledByShieldEnemy: true},
			SoldierShieldDeathEffect{Triggered: true, Delete: true}},
		{"rank4-enemy-survive-1", normal,
			ImmediateDeathHooksInput{HasSoldierShield: true, SoldierShieldRank: 4, KilledByShieldEnemy: true},
			SoldierShieldDeathEffect{Triggered: true, RankAfter: 1}},
		{"rank5-enemy-survive-1", normal,
			ImmediateDeathHooksInput{HasSoldierShield: true, SoldierShieldRank: 5, KilledByShieldEnemy: true},
			SoldierShieldDeathEffect{Triggered: true, RankAfter: 1}},
		{"rank6-enemy-survive-2", normal,
			ImmediateDeathHooksInput{HasSoldierShield: true, SoldierShieldRank: 6, KilledByShieldEnemy: true},
			SoldierShieldDeathEffect{Triggered: true, RankAfter: 2}},
		{"rank7-enemy-survive-3", normal,
			ImmediateDeathHooksInput{HasSoldierShield: true, SoldierShieldRank: 7, KilledByShieldEnemy: true},
			SoldierShieldDeathEffect{Triggered: true, RankAfter: 3}},
		{"rank8-enemy-survive-4", normal,
			ImmediateDeathHooksInput{HasSoldierShield: true, SoldierShieldRank: 8, KilledByShieldEnemy: true},
			SoldierShieldDeathEffect{Triggered: true, RankAfter: 4}},
		{"rank9-enemy-survive-5", normal,
			ImmediateDeathHooksInput{HasSoldierShield: true, SoldierShieldRank: 9, KilledByShieldEnemy: true},
			SoldierShieldDeathEffect{Triggered: true, RankAfter: 5}},
		{"rank10-enemy-survive-6", normal,
			ImmediateDeathHooksInput{HasSoldierShield: true, SoldierShieldRank: 10, KilledByShieldEnemy: true},
			SoldierShieldDeathEffect{Triggered: true, RankAfter: 6}},
		{"rank1-non-enemy-no-effect", normal,
			ImmediateDeathHooksInput{HasSoldierShield: true, SoldierShieldRank: 1},
			SoldierShieldDeathEffect{}},
		{"rank10-non-enemy-no-effect", normal,
			ImmediateDeathHooksInput{HasSoldierShield: true, SoldierShieldRank: 10},
			SoldierShieldDeathEffect{}},
		{"no-shield", normal,
			ImmediateDeathHooksInput{KilledByShieldEnemy: true},
			SoldierShieldDeathEffect{}},
		{"cheap-enemy-rank1-no-effect", cheap,
			ImmediateDeathHooksInput{HasSoldierShield: true, SoldierShieldRank: 1, KilledByShieldEnemy: true},
			SoldierShieldDeathEffect{}},
		{"cheap-enemy-rank10-no-effect", cheap,
			ImmediateDeathHooksInput{HasSoldierShield: true, SoldierShieldRank: 10, KilledByShieldEnemy: true},
			SoldierShieldDeathEffect{}},
		{"avoided-enemy-no-effect", avoided,
			ImmediateDeathHooksInput{HasSoldierShield: true, SoldierShieldRank: 1, KilledByShieldEnemy: true},
			SoldierShieldDeathEffect{}},
	}
	for _, c := range cases {
		hooks, err := PlanImmediateDeathHooks(c.plan, c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if hooks.SoldierShield != c.want {
			t.Fatalf("%s: effect = %+v, want %+v", c.name, hooks.SoldierShield, c.want)
		}
		if hooks.SoldierShield.Died() != c.want.Delete {
			t.Fatalf("%s: Died() = %v, want %v", c.name, hooks.SoldierShield.Died(), c.want.Delete)
		}
		if hooks.GuardianAngelMail {
			t.Fatalf("%s: unexpected angel mail (no newbie inputs)", c.name)
		}
	}
	// Rank outside 1..10 with a present shield is a domain error with
	// zero output.
	for _, rank := range []int{0, -1, 11, 99} {
		hooks, err := PlanImmediateDeathHooks(normal, ImmediateDeathHooksInput{
			HasSoldierShield: true, SoldierShieldRank: rank, KilledByShieldEnemy: true,
		})
		if !errors.Is(err, ErrInvalidDeathInput) || hooks != (ImmediateDeathHooks{}) {
			t.Fatalf("rank %d: hooks=%+v err=%v", rank, hooks, err)
		}
	}
}

// --- immediate hooks: normal+frenzy contradiction --------------------------------

func TestImmediateDeathHooksNormalFrenzyRejected(t *testing.T) {
	normal := mustPlan(t, 100, DeathContext{}, false)
	// Source-impossible: frenzy routing is a cheap real death before
	// immediate hooks are planned. Rejected regardless of
	// newbie/murderer/angel eligibility, with zero hook output.
	bads := map[string]ImmediateDeathHooksInput{
		"newbie-eligible":   {FrenzyActive: true, StillNewbie: true},
		"non-newbie":        {FrenzyActive: true},
		"murderer":          {FrenzyActive: true, StillNewbie: true, Murderer: true},
		"with-shield-enemy": {FrenzyActive: true, HasSoldierShield: true, SoldierShieldRank: 1, KilledByShieldEnemy: true},
	}
	for name, in := range bads {
		hooks, err := PlanImmediateDeathHooks(normal, in)
		if !errors.Is(err, ErrInvalidDeathInput) {
			t.Fatalf("%s: err = %v, want ErrInvalidDeathInput", name, err)
		}
		if hooks != (ImmediateDeathHooks{}) {
			t.Fatalf("%s: hooks = %+v, want zero", name, hooks)
		}
	}
	// The legitimate corners stay accepted: cheap + frenzy (frenzy routing
	// IS a cheap death), and avoided coexisting with a globally-active
	// frenzy via the avoided early-return precedence.
	cheap := mustPlan(t, 100, DeathContext{FrenzyActive: true}, false)
	if _, err := PlanImmediateDeathHooks(cheap, ImmediateDeathHooksInput{FrenzyActive: true}); err != nil {
		t.Fatalf("cheap frenzy: %v", err)
	}
	avoided := mustPlan(t, 100, DeathContext{PrisonRoom: true}, false)
	if _, err := PlanImmediateDeathHooks(avoided, ImmediateDeathHooksInput{FrenzyActive: true}); err != nil {
		t.Fatalf("avoided frenzy: %v", err)
	}
}

// --- ability namespace: same numeric key in both lists -------------------------

func TestDeathPenaltySameKeySpellAndSkill(t *testing.T) {
	base := mkVitals(10, 40, 45, 5, 20, 100)
	// HP roll first (100 <= 100 loses), then spell Key 7 loses
	// (stamina 50 > 30 fails, cost 1 < 100 loses), then skill Key 7
	// loses (stamina 60 > 30 fails, cost 1 < 100 loses).
	p, err := PlanDeathPenalties(d100Rolls(100, 50, 1, 60, 1), DeathPenaltyInput{
		PendingCost: 100, DefaultCost: 100, Stamina: 30, Vitals: base,
		Spells: []DeathAbilityInput{{Key: 7, Ability: 50}},
		Skills: []DeathAbilityInput{{Key: 7, Ability: 60}},
	})
	if err != nil {
		t.Fatalf("same key: %v", err)
	}
	if len(p.AbilityLosses) != 2 {
		t.Fatalf("want both Key-7 losses: %+v", p.AbilityLosses)
	}
	spell, skill := p.AbilityLosses[0], p.AbilityLosses[1]
	if spell.Kind != DeathAbilitySpell || spell.Key != 7 || spell.FromAbility != 50 {
		t.Fatalf("spell loss: %+v", spell)
	}
	if skill.Kind != DeathAbilitySkill || skill.Key != 7 || skill.FromAbility != 60 {
		t.Fatalf("skill loss: %+v", skill)
	}
	if spell.Kind == skill.Kind {
		t.Fatalf("losses indistinguishable: %+v", p.AbilityLosses)
	}
	// RNG order preserved: HP, spells in order, skills in order.
	if !p.HPRolled || p.HPRoll != 100 || spell.StaminaRoll != 50 || skill.StaminaRoll != 60 {
		t.Fatalf("consumption order drifted: %+v", p)
	}
}

// --- post-death vitals: contradiction guards ------------------------------------

func TestDeathPostVitalsContradictions(t *testing.T) {
	v := mkVitals(0, 40, 45, 1, 25, 100)
	bads := map[string]PostDeathVitalsInput{
		"normal+frenzy": {Vitals: v, Disposition: DeathNormal, FrenzyActive: true},
		"cheap+angel":   {Vitals: v, Disposition: DeathCheap, AngelMailEligible: true},
		"frenzy+angel":  {Vitals: v, Disposition: DeathCheap, FrenzyActive: true, AngelMailEligible: true},
		"avoided":       {Vitals: v, Disposition: DeathAvoided},
	}
	for name, in := range bads {
		got, err := PlanPostDeathVitals(in)
		if !errors.Is(err, ErrInvalidDeathInput) {
			t.Fatalf("%s: err = %v, want ErrInvalidDeathInput", name, err)
		}
		if got != (PlayerVitals{}) {
			t.Fatalf("%s: output = %+v, want zero", name, got)
		}
	}
	// The legitimate corners stay accepted.
	if _, err := PlanPostDeathVitals(PostDeathVitalsInput{Vitals: v, Disposition: DeathCheap}); err != nil {
		t.Fatalf("cheap plain: %v", err)
	}
	if _, err := PlanPostDeathVitals(PostDeathVitalsInput{Vitals: v, Disposition: DeathNormal}); err != nil {
		t.Fatalf("normal plain: %v", err)
	}
	if _, err := PlanPostDeathVitals(PostDeathVitalsInput{Vitals: v, Disposition: DeathCheap, FrenzyActive: true}); err != nil {
		t.Fatalf("cheap frenzy: %v", err)
	}
	if _, err := PlanPostDeathVitals(PostDeathVitalsInput{Vitals: v, Disposition: DeathNormal, AngelMailEligible: true}); err != nil {
		t.Fatalf("normal angel: %v", err)
	}
}

// --- disposition-plan invariants --------------------------------------------------

func TestDispositionPlanInvariants(t *testing.T) {
	corpse, err := PlanCorpse(7)
	if err != nil {
		t.Fatalf("corpse: %v", err)
	}
	hostiles := map[string]DeathDispositionPlan{
		"cheap+cost":            {Disposition: DeathCheap, DeathCost: 100, SpecialItemsKept: true},
		"normal+zero":           {Disposition: DeathNormal},
		"normal+token":          {Disposition: DeathNormal, DeathCost: 100, TokenDeath: true},
		"normal+newbie-respawn": {Disposition: DeathNormal, DeathCost: 100, NewbieHomeRespawn: true},
		"normal+kept":           {Disposition: DeathNormal, DeathCost: 100, SpecialItemsKept: true},
		"avoided+cost":          {Disposition: DeathAvoided, SpecialItemsKept: true, DeathCost: 5},
		"avoided+token":         {Disposition: DeathAvoided, SpecialItemsKept: true, TokenDeath: true},
		"avoided+respawn":       {Disposition: DeathAvoided, SpecialItemsKept: true, NewbieHomeRespawn: true},
		"avoided+unkept":        {Disposition: DeathAvoided},
		"cheap+unkept":          {Disposition: DeathCheap},
		"token+normal":          {Disposition: DeathNormal, DeathCost: 100, TokenDeath: true},
		"hostile-enum":          {Disposition: DeathDisposition(9)},
	}
	for name, plan := range hostiles {
		if _, err := PlanDeathDrops(plan, nil); !errors.Is(err, ErrInvalidDeathInput) {
			t.Fatalf("drops %s: err = %v", name, err)
		}
		if _, err := PlanPendingDeath(plan, corpse); !errors.Is(err, ErrInvalidDeathInput) {
			t.Fatalf("pending %s: err = %v", name, err)
		}
		if _, err := PlanImmediateDeathHooks(plan, ImmediateDeathHooksInput{}); !errors.Is(err, ErrInvalidDeathInput) {
			t.Fatalf("hooks %s: err = %v", name, err)
		}
	}
	// Legitimate combinations remain accepted by every consumer.
	legits := map[string]DeathDispositionPlan{
		"pure-token":    {Disposition: DeathCheap, TokenDeath: true},
		"newbie+token":  {Disposition: DeathCheap, TokenDeath: true, SpecialItemsKept: true, NewbieHomeRespawn: true},
		"frenzy-cheap":  {Disposition: DeathCheap, SpecialItemsKept: true},
		"normal":        {Disposition: DeathNormal, DeathCost: 100, KillerIsPlayer: true},
		"avoided":       {Disposition: DeathAvoided, SpecialItemsKept: true, KillerIsPlayer: true},
		"cheap-pk-echo": {Disposition: DeathCheap, SpecialItemsKept: true, KillerIsPlayer: true},
	}
	for name, plan := range legits {
		// Avoided deaths never reach drop planning by design; the
		// validator itself must still accept the plan (proven via
		// pending + hooks below).
		if plan.Disposition != DeathAvoided {
			if _, err := PlanDeathDrops(plan, nil); err != nil {
				t.Fatalf("drops legit %s: %v", name, err)
			}
		}
		if _, err := PlanPendingDeath(plan, corpse); err != nil {
			t.Fatalf("pending legit %s: %v", name, err)
		}
		if _, err := PlanImmediateDeathHooks(plan, ImmediateDeathHooksInput{}); err != nil {
			t.Fatalf("hooks legit %s: %v", name, err)
		}
	}
	// Constructor outputs always validate, including the tricky
	// newbie-zone + token sequencing (keep-guard stays armed).
	for _, ctx := range []DeathContext{
		{},
		{FrenzyActive: true},
		{NewbieZoneDeath: true},
		{NewbieHonor: true},
		{CarriesToken: true},
		{NewbieZoneDeath: true, CarriesToken: true},
		{FrenzyActive: true, CarriesToken: true},
		{PrisonRoom: true},
	} {
		p := mustPlan(t, 100, ctx, true)
		if p.Disposition != DeathAvoided {
			if _, err := PlanDeathDrops(p, nil); err != nil {
				t.Fatalf("ctor %+v drops: %v", ctx, err)
			}
		}
		if _, err := PlanPendingDeath(p, corpse); err != nil {
			t.Fatalf("ctor %+v pending: %v", ctx, err)
		}
	}
	// Token + earlier newbie cheap keeps the guard armed end to end.
	seq := mustPlan(t, 100, DeathContext{NewbieZoneDeath: true, CarriesToken: true}, true)
	if !seq.TokenDeath || !seq.SpecialItemsKept || seq.DeathCost != 0 {
		t.Fatalf("newbie+token sequencing: %+v", seq)
	}
	drops, err := PlanDeathDrops(seq, []DeathItemInput{{Key: 1, SpecialItem: true}})
	if err != nil {
		t.Fatalf("newbie+token drops: %v", err)
	}
	if !drops.Items[0].SpecialItemKept || drops.Items[0].SpecialItemLost() {
		t.Fatalf("newbie+token must keep the artifact: %+v", drops.Items[0])
	}
}

// --- special-item loss: single direct test ------------------------------------------

func TestDropPlanSpecialItemLost(t *testing.T) {
	items := []DeathItemInput{{Key: 1, DropOnDeath: true, RoomAccepts: true, SpecialItem: true}}
	// Normal death loses the artifact.
	normal, err := PlanDeathDrops(mustPlan(t, 100, DeathContext{}, false), items)
	if err != nil {
		t.Fatalf("normal: %v", err)
	}
	if !normal.Items[0].SpecialItemLost() {
		t.Fatalf("normal must lose the artifact: %+v", normal.Items[0])
	}
	// Pure token cheap death loses the artifact.
	token, err := PlanDeathDrops(mustPlan(t, 100, DeathContext{CarriesToken: true}, false), items)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if !token.Items[0].SpecialItemLost() {
		t.Fatalf("token must lose the artifact: %+v", token.Items[0])
	}
	// Non-token cheap death keeps it.
	cheap, err := PlanDeathDrops(mustPlan(t, 100, DeathContext{FrenzyActive: true}, false), items)
	if err != nil {
		t.Fatalf("cheap: %v", err)
	}
	if cheap.Items[0].SpecialItemLost() {
		t.Fatalf("non-token cheap must keep the artifact: %+v", cheap.Items[0])
	}
	// The helper is exactly SpecialItem && !SpecialItemKept everywhere.
	for _, plan := range []DeathDropPlan{normal, token, cheap} {
		for _, it := range plan.Items {
			if it.SpecialItemLost() != (it.SpecialItem && !it.SpecialItemKept) {
				t.Fatalf("helper drifted: %+v", it)
			}
		}
	}
}
