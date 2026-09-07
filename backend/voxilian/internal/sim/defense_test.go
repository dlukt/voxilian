package sim

import (
	"errors"
	"math"
	"testing"
)

// countingRNG wraps scriptRNG draws to observe RNG consumption.
type countingRNG struct {
	scriptRNG
}

func enabledCap() DefenseCapability {
	return DefenseCapability{
		HasWeapon:      true,
		ParryCostOK:    true,
		AttackCanParry: true,
		HasShield:      true,
		BlockCostOK:    true,
		AttackCanBlock: true,
		DodgeCostOK:    true,
		AttackCanDodge: true,
	}
}

func TestParryDefenseCapability(t *testing.T) {
	full := enabledCap()
	got, err := ResolveParryComponent(50, full)
	if err != nil || got != 50 {
		t.Fatalf("enabled parry = %d, %v; want 50", got, err)
	}
	// Each gate independently zeroes the component (spec §9.2.4).
	off := full
	off.HasWeapon = false
	if got, _ := ResolveParryComponent(50, off); got != 0 {
		t.Fatalf("no weapon parry = %d; want 0", got)
	}
	off = full
	off.ParryCostOK = false // NO_FIGHT folded here
	if got, _ := ResolveParryComponent(50, off); got != 0 {
		t.Fatalf("NO_FIGHT parry = %d; want 0", got)
	}
	off = full
	off.AttackCanParry = false
	if got, _ := ResolveParryComponent(50, off); got != 0 {
		t.Fatalf("unparryable parry = %d; want 0", got)
	}
	// Unknown skill (ability 0) contributes 0 when enabled.
	if got, _ := ResolveParryComponent(0, full); got != 0 {
		t.Fatalf("ability-0 parry = %d; want 0", got)
	}
	if _, err := ResolveParryComponent(-1, full); !errors.Is(err, ErrInvalidDefenseSkill) {
		t.Fatalf("negative parry err = %v; want ErrInvalidDefenseSkill", err)
	}
}

func TestDodgeDefenseCapability(t *testing.T) {
	full := enabledCap()
	got, err := ResolveDodgeComponent(30, full)
	if err != nil || got != 30 {
		t.Fatalf("enabled dodge = %d, %v; want 30", got, err)
	}
	off := full
	off.DodgeCostOK = false // NO_MOVE folded here
	if got, _ := ResolveDodgeComponent(30, off); got != 0 {
		t.Fatalf("NO_MOVE dodge = %d; want 0", got)
	}
	off = full
	off.AttackCanDodge = false
	if got, _ := ResolveDodgeComponent(30, off); got != 0 {
		t.Fatalf("undodgeable dodge = %d; want 0", got)
	}
	// Dodge has NO equipment requirement: unarmed/unshielded defender
	// with passing gates still contributes (spec §9.2.4).
	bare := DefenseCapability{DodgeCostOK: true, AttackCanDodge: true}
	if got, _ := ResolveDodgeComponent(30, bare); got != 30 {
		t.Fatalf("unequipped dodge = %d; want 30", got)
	}
	if _, err := ResolveDodgeComponent(-2, full); !errors.Is(err, ErrInvalidDefenseSkill) {
		t.Fatalf("negative dodge err = %v; want ErrInvalidDefenseSkill", err)
	}
}

func TestBlockDefenseCapability(t *testing.T) {
	full := enabledCap()
	// Metal shield: skill 40 + bonus 5 = 45 (spec §9.2.5).
	got, err := ResolveBlockComponent(40, 5, full)
	if err != nil || got != 45 {
		t.Fatalf("metal block = %d, %v; want 45", got, err)
	}
	// Each gate independently zeroes the component (no 1..120 clamp
	// on the disabled path).
	for _, off := range []DefenseCapability{
		func() DefenseCapability { c := full; c.HasShield = false; return c }(),
		func() DefenseCapability { c := full; c.BlockCostOK = false; return c }(),
		func() DefenseCapability { c := full; c.AttackCanBlock = false; return c }(),
	} {
		if got, _ := ResolveBlockComponent(100, 20, off); got != 0 {
			t.Fatalf("disabled block = %d; want 0", got)
		}
	}
	// Enabled edge: skill 0 + bonus 0 still yields the clamped floor 1
	// (source bound(0,1,120)); reduction separately requires ability > 0.
	if got, _ := ResolveBlockComponent(0, 0, full); got != 1 {
		t.Fatalf("zero block rating = %d; want 1", got)
	}
	// Orc shield numbers: 90 + 20 = 110; overflow bonus clamps at 120.
	if got, _ := ResolveBlockComponent(90, 20, full); got != 110 {
		t.Fatalf("orc block = %d; want 110", got)
	}
	if got, _ := ResolveBlockComponent(115, 20, full); got != 120 {
		t.Fatalf("clamped block = %d; want 120", got)
	}
	if _, err := ResolveBlockComponent(-1, 0, full); !errors.Is(err, ErrInvalidDefenseSkill) {
		t.Fatalf("negative block skill err = %v; want ErrInvalidDefenseSkill", err)
	}
}

func TestDefenseSkillChanceGolden(t *testing.T) {
	// Hand-computed: ((100-req)*abil)/100 + req + mod (spec §9.2.6).
	cases := []struct {
		abil, req, mod, want int
	}{
		{50, 25, 0, 62},   // (75*50)/100+25 = 37+25
		{20, 10, 0, 28},   // (90*20)/100+10 = 18+10
		{0, 30, 15, 45},   // 0+30+15
		{99, 40, 0, 99},   // (60*99)/100+40 = 59+40
		{99, 40, 10, 109}, // unclamped over-100: always succeeds
		{0, 0, -5, -5},    // unclamped negative: always fails
		{1, 70, 0, 70},    // (30*1)/100+70 = 0+70 (truncation probe)
	}
	for _, c := range cases {
		got, err := DefenseSkillChance(c.abil, c.req, c.mod)
		if err != nil || got != c.want {
			t.Fatalf("chance(%d,%d,%d) = %d, %v; want %d",
				c.abil, c.req, c.mod, got, err, c.want)
		}
	}
	if _, err := DefenseSkillChance(-1, 10, 0); !errors.Is(err, ErrInvalidDefenseSkill) {
		t.Fatalf("negative ability err = %v", err)
	}
	if _, err := DefenseSkillChance(10, -1, 0); !errors.Is(err, ErrInvalidDefenseSkill) {
		t.Fatalf("negative requisite err = %v", err)
	}
}

func TestDefenseSkillSharedFormula(t *testing.T) {
	// Parry, Dodge, and Block contexts share ONE formula (spec §9.2.6:
	// source defines no per-skill override). All three contexts with
	// identical inputs must produce identical chances.
	for _, in := range [][3]int{{50, 25, 0}, {90, 40, 20}, {0, 0, 0}} {
		parry, _ := DefenseSkillChance(in[0], in[1], in[2])
		dodge, _ := DefenseSkillChance(in[0], in[1], in[2])
		block, _ := DefenseSkillChance(in[0], in[1], in[2])
		if parry != dodge || dodge != block {
			t.Fatalf("split formula %+v: %d/%d/%d", in, parry, dodge, block)
		}
	}
}

func TestDefenseSkillRollBoundaries(t *testing.T) {
	// Chance 62 (ability 50, req 25, mod 0). rollBounded(1,100):
	// roll = 1 + v%100.
	roll := func(v uint64) DefenseSkillRoll {
		r, err := RollDefenseSkill(&scriptRNG{vals: []uint64{v}}, 50, 25, 0)
		if err != nil {
			t.Fatalf("roll: %v", err)
		}
		return r
	}
	if r := roll(0); r.Roll != 1 || !r.Success {
		t.Fatalf("d100=1: %+v; want success", r)
	}
	if r := roll(61); r.Roll != 62 || !r.Success || r.Chance != 62 {
		t.Fatalf("roll==chance: %+v; want success", r)
	}
	if r := roll(62); r.Roll != 63 || r.Success {
		t.Fatalf("roll==chance+1: %+v; want failure", r)
	}
	if r := roll(99); r.Roll != 100 || r.Success {
		t.Fatalf("d100=100: %+v; want failure", r)
	}
	// Over-100 chance always succeeds, even on 100.
	r, err := RollDefenseSkill(&scriptRNG{vals: []uint64{99}}, 99, 40, 10)
	if err != nil || !r.Success {
		t.Fatalf("chance 109 on 100: %+v, %v; want success", r, err)
	}
	// Negative chance always fails, even on 1.
	r, err = RollDefenseSkill(&scriptRNG{vals: []uint64{0}}, 0, 0, -5)
	if err != nil || r.Success {
		t.Fatalf("chance -5 on 1: %+v, %v; want failure", r, err)
	}
	if _, err := RollDefenseSkill(nil, 50, 25, 0); !errors.Is(err, ErrNilRNG) {
		t.Fatalf("nil RNG err = %v; want ErrNilRNG", err)
	}
}

func TestBlockRollOutcome(t *testing.T) {
	// No block ability: no RNG consumed, Attempted=false.
	rng := &countingRNG{scriptRNG{vals: []uint64{0}}}
	out, err := RollBlock(rng, 0, 25, 10)
	if err != nil || out.Attempted || out.Succeeded {
		t.Fatalf("ability-0 block = %+v, %v", out, err)
	}
	if rng.at != 0 {
		t.Fatalf("ability-0 consumed %d RNG draws; want 0", rng.at)
	}
	// Ability 50, req 25, Gold bonus 10: chance = 37+25+10 = 72.
	rng = &countingRNG{scriptRNG{vals: []uint64{71}}}
	out, err = RollBlock(rng, 50, 25, 10)
	if err != nil || !out.Attempted || !out.Succeeded || out.Chance != 72 || out.Roll != 72 {
		t.Fatalf("block threshold = %+v, %v; want success at 72", out, err)
	}
	rng = &countingRNG{scriptRNG{vals: []uint64{72}}}
	out, err = RollBlock(rng, 50, 25, 10)
	if err != nil || !out.Attempted || out.Succeeded || out.Roll != 73 {
		t.Fatalf("block threshold+1 = %+v, %v; want failure at 73", out, err)
	}
	if _, err := RollBlock(&scriptRNG{}, -1, 25, 0); !errors.Is(err, ErrInvalidDefenseSkill) {
		t.Fatalf("negative block ability err = %v", err)
	}
}

func TestParryDodgeAreNotSecondEvasion(t *testing.T) {
	// Source audit (spec §9.2.7): Parry/Dodge SuccessChance is never
	// rolled in combat resolution, so no production path may negate a
	// landed hit via a parry/dodge trace. A landed hit's damage passes
	// through the mitigation stage unchanged when no modifiers apply,
	// and armor still applies when the block gate fails (the gate only
	// affects shield entries).
	rng := &scriptRNG{vals: []uint64{0}}
	res, err := ApplyDefenseModifiers(rng, 17, DamageClassWeapon, nil, false)
	if err != nil || res.FinalDamage != 17 || res.TotalReduced != 0 {
		t.Fatalf("empty mitigation = %+v, %v; want identity", res, err)
	}
	armor := []DefenseModifier{{DefensePower: -50, DamageReduce: 2}}
	res, err = ApplyDefenseModifiers(&scriptRNG{vals: []uint64{2}}, 20, DamageClassWeapon, armor, false)
	if err != nil || res.FinalDamage != 18 {
		t.Fatalf("armor with failed block = %+v, %v; want 18", res, err)
	}
	// A successful generic defensive-skill trace is M6 advancement
	// data only: it carries no damage effect by construction (there is
	// no RollParry/RollDodge combat API to compose).
	tr, err := RollDefenseSkill(&scriptRNG{vals: []uint64{0}}, 99, 1, 0)
	if err != nil || !tr.Success {
		t.Fatalf("skill trace = %+v, %v", tr, err)
	}
}

func TestDefensePowerModifierGolden(t *testing.T) {
	// Plain signed sums (spec §9.2.8).
	cases := []struct {
		name string
		mods []DefenseModifier
		want int
	}{
		{"zero", nil, 0},
		{"leather", []DefenseModifier{{DefensePower: 50}}, 50},
		{"plate", []DefenseModifier{{DefensePower: -200}}, -200},
		{"leather+chain+helm", []DefenseModifier{{DefensePower: 50}, {DefensePower: -50}, {DefensePower: 25}}, 25},
		{"shield excluded", []DefenseModifier{{DefensePower: 0, DamageReduce: 1, RequiresBlock: true}}, 0},
	}
	for _, c := range cases {
		got, err := ResolveDefensePowerModifier(c.mods)
		if err != nil || got != c.want {
			t.Fatalf("%s = %d, %v; want %d", c.name, got, err, c.want)
		}
	}
	// Shield bonus must never leak into DefensePower (no-double-count
	// guard, spec §9.2.8).
	bad := []DefenseModifier{{DefensePower: 10, RequiresBlock: true}}
	if _, err := ResolveDefensePowerModifier(bad); !errors.Is(err, ErrInvalidDefenseModifier) {
		t.Fatalf("shield power leak err = %v; want ErrInvalidDefenseModifier", err)
	}
	if _, err := ResolveDefensePowerModifier([]DefenseModifier{{DamageReduce: -3}}); !errors.Is(err, ErrInvalidDefenseModifier) {
		t.Fatalf("negative reduce err = %v; want ErrInvalidDefenseModifier", err)
	}
}

func TestPlayerDefenseComposition(t *testing.T) {
	// Mandatory T1 composition proof (spec §9.2.3): T2-resolved
	// components feed the REAL T1 PlayerDefense, never a duplicate.
	cap := enabledCap()
	parry, err := ResolveParryComponent(50, cap)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ResolveBlockComponent(40, 5, cap) // Metal shield numbers
	if err != nil {
		t.Fatal(err)
	}
	dodge, err := ResolveDodgeComponent(30, cap)
	if err != nil {
		t.Fatal(err)
	}
	extra, err := ResolveDefensePowerModifier([]DefenseModifier{
		{DefensePower: 50},  // Leather
		{DefensePower: -50}, // Chain
		{DefensePower: 25},  // Helm
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := PlayerDefense(PlayerDefenseInput{
		Parry: parry, Block: block, Dodge: dodge,
		Agility: 25, BaseMaxHP: 40, ExtraMods: extra,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 50*2 + 45 + 30*3 + 25*4 + (40*3)/2 + 25 = 100+45+90+100+60+25.
	if want := 420; got != want {
		t.Fatalf("composed defense = %d; want %d", got, want)
	}
}

func TestDefenseDamageClass(t *testing.T) {
	if got := ClassifyDamageClass(0x82, 0); got != DamageClassWeapon {
		t.Fatalf("weapon class = %d", got)
	}
	if got := ClassifyDamageClass(0, 0x03); got != DamageClassSpell {
		t.Fatalf("spell class = %d", got)
	}
	if got := ClassifyDamageClass(0x82, 0x03); got != DamageClassWeaponSpell {
		t.Fatalf("mixed class = %d", got)
	}
	// Degenerate zero-vector follows the source aspell==0 branch.
	if got := ClassifyDamageClass(0, 0); got != DamageClassWeapon {
		t.Fatalf("zero class = %d", got)
	}
}

func TestDefenseDamageReductionGolden(t *testing.T) {
	// r=0: no reduction, no RNG consumed.
	rng := &countingRNG{scriptRNG{vals: []uint64{99}}}
	if got, err := RollDamageReduction(rng, 0, 20, DamageClassWeapon); err != nil || got != 0 {
		t.Fatalf("r=0 = %d, %v", got, err)
	}
	if rng.at != 0 {
		t.Fatalf("r=0 consumed RNG")
	}
	// r=6, damage 20: random(2,6). Forced min (v=0 -> 2) and max
	// (span 5, v=4 -> 6).
	if got, _ := RollDamageReduction(&scriptRNG{vals: []uint64{0}}, 6, 20, DamageClassWeapon); got != 2 {
		t.Fatalf("min roll = %d; want 2", got)
	}
	if got, _ := RollDamageReduction(&scriptRNG{vals: []uint64{4}}, 6, 20, DamageClassWeapon); got != 6 {
		t.Fatalf("max roll = %d; want 6", got)
	}
	// Cap at damage-1: r=30 max roll 30 (low 10, span 21, v=20),
	// damage 3 -> capped 2.
	if got, _ := RollDamageReduction(&scriptRNG{vals: []uint64{20}}, 30, 3, DamageClassWeapon); got != 2 {
		t.Fatalf("capped roll = %d; want 2", got)
	}
	// Tiny damage: 1 -> 0 reduction (returns 1 via caller
	// subtraction); 0 -> 0 with no roll; negative errors.
	if got, _ := RollDamageReduction(&scriptRNG{vals: []uint64{99}}, 6, 1, DamageClassWeapon); got != 0 {
		t.Fatalf("damage-1 roll = %d; want 0", got)
	}
	rng0 := &countingRNG{scriptRNG{vals: []uint64{99}}}
	if got, err := RollDamageReduction(rng0, 6, 0, DamageClassWeapon); err != nil || got != 0 || rng0.at != 0 {
		t.Fatalf("damage-0 = %d, %v, draws %d", got, err, rng0.at)
	}
	if _, err := RollDamageReduction(&scriptRNG{}, 6, -1, DamageClassWeapon); !errors.Is(err, ErrInvalidDamageValue) {
		t.Fatalf("negative damage err = %v", err)
	}
	if _, err := RollDamageReduction(&scriptRNG{}, -2, 20, DamageClassWeapon); !errors.Is(err, ErrInvalidDefenseModifier) {
		t.Fatalf("negative r err = %v", err)
	}
	if _, err := RollDamageReduction(&scriptRNG{}, 6, 20, DamageClass(7)); !errors.Is(err, ErrInvalidDamageClass) {
		t.Fatalf("bad class err = %v", err)
	}
	// Pure spell bypasses armor entirely (r=6 max roll -> 0).
	if got, _ := RollDamageReduction(&scriptRNG{vals: []uint64{4}}, 6, 20, DamageClassSpell); got != 0 {
		t.Fatalf("pure-spell reduction = %d; want 0", got)
	}
	// Mixed scales the ROLLED+CAPPED value AFTER the draw (spec
	// §9.2.10): r=5 max roll 5 (low 1, span 5, v=4) -> (5*2)/3 = 3;
	// r=4 max roll 4 (low 1, span 4, v=3) -> (4*2)/3 = 2. A
	// scale-before-random implementation would draw from random(1,3)
	// / random(1,2) with different maxima and fail these vectors.
	if got, _ := RollDamageReduction(&scriptRNG{vals: []uint64{4}}, 5, 20, DamageClassWeaponSpell); got != 3 {
		t.Fatalf("mixed r=5 = %d; want 3", got)
	}
	if got, _ := RollDamageReduction(&scriptRNG{vals: []uint64{3}}, 4, 20, DamageClassWeaponSpell); got != 2 {
		t.Fatalf("mixed r=4 = %d; want 2", got)
	}
}

func TestDefenseMitigationSequential(t *testing.T) {
	mods := []DefenseModifier{{DamageReduce: 6}, {DamageReduce: 4}}
	// Scripted max rolls: r=6 (span 5, v=4 -> 6), r=4 (span 4, v=3
	// -> 4). Canonical slice order consumes draws in order.
	res, err := ApplyDefenseModifiers(&scriptRNG{vals: []uint64{4, 3}}, 20, DamageClassWeapon, mods, false)
	if err != nil || res.FinalDamage != 10 || res.TotalReduced != 10 || res.ModifiersApplied != 2 {
		t.Fatalf("sequential = %+v, %v; want final 10", res, err)
	}
	// Pure spell: identical rolls, zero effect.
	res, err = ApplyDefenseModifiers(&scriptRNG{vals: []uint64{4, 3}}, 20, DamageClassSpell, mods, false)
	if err != nil || res.FinalDamage != 20 || res.ModifiersApplied != 0 {
		t.Fatalf("spell sequential = %+v, %v; want 20", res, err)
	}
	// Shield entry skipped without block success, applied with it.
	withShield := []DefenseModifier{{DamageReduce: 2}, {DamageReduce: 1, RequiresBlock: true}}
	res, err = ApplyDefenseModifiers(&scriptRNG{vals: []uint64{2, 1}}, 20, DamageClassWeapon, withShield, false)
	if err != nil || res.FinalDamage != 18 || res.ModifiersApplied != 1 {
		t.Fatalf("unblocked shield = %+v, %v; want 18", res, err)
	}
	res, err = ApplyDefenseModifiers(&scriptRNG{vals: []uint64{2, 1}}, 20, DamageClassWeapon, withShield, true)
	if err != nil || res.FinalDamage != 17 || res.ModifiersApplied != 2 {
		t.Fatalf("blocked shield = %+v, %v; want 17", res, err)
	}
	// Damage 0 passes through as 0 (no minimum inside T2).
	res, err = ApplyDefenseModifiers(&scriptRNG{vals: []uint64{9}}, 0, DamageClassWeapon, mods, true)
	if err != nil || res.FinalDamage != 0 {
		t.Fatalf("zero mitigation = %+v, %v", res, err)
	}
	if _, err := ApplyDefenseModifiers(&scriptRNG{}, -1, DamageClassWeapon, mods, true); !errors.Is(err, ErrInvalidDamageValue) {
		t.Fatalf("negative mitigation err = %v", err)
	}
}

func TestDefenseMitigationCanonicalOrder(t *testing.T) {
	// Same scripted draws, reversed slice order: draws attach to
	// different items, pinning that RNG consumption follows slice
	// order (spec §9.2.12). r=6: low 2 span 5; r=4: low 1 span 4.
	mods := []DefenseModifier{{DamageReduce: 6}, {DamageReduce: 4}}
	fwd, err := ApplyDefenseModifiers(&scriptRNG{vals: []uint64{4, 3}}, 20, DamageClassWeapon, mods, false)
	if err != nil {
		t.Fatal(err)
	}
	rev, err := ApplyDefenseModifiers(&scriptRNG{vals: []uint64{4, 3}}, 20, DamageClassWeapon,
		[]DefenseModifier{{DamageReduce: 4}, {DamageReduce: 6}}, false)
	if err != nil {
		t.Fatal(err)
	}
	// Forward: 2+(4%5)=6 then 1+(3%4)=4 -> 20-10 = 10.
	// Reversed: 1+(4%4)=1 then 2+(3%5)=5 -> 20-6 = 14.
	if fwd.FinalDamage != 10 || rev.FinalDamage != 14 {
		t.Fatalf("order trace fwd=%+v rev=%+v; want 10/14", fwd, rev)
	}
}

func TestDefenseMitigationOrderIndependent(t *testing.T) {
	// With a CONSTANT scripted RNG, each modifier's roll depends only
	// on its own span (roll = low + c%span), not its slice position —
	// so every permutation must yield the identical final damage
	// (spec §9.2.12 order-independence, hand oracle: rolls 4,4,3
	// total 11, 20-11 = 9; damage 5 binds the cap -> 1).
	mods := []DefenseModifier{{DamageReduce: 6}, {DamageReduce: 4}, {DamageReduce: 9}}
	perms := [][]DefenseModifier{
		{mods[0], mods[1], mods[2]},
		{mods[0], mods[2], mods[1]},
		{mods[1], mods[0], mods[2]},
		{mods[1], mods[2], mods[0]},
		{mods[2], mods[0], mods[1]},
		{mods[2], mods[1], mods[0]},
	}
	for i, p := range perms {
		res, err := ApplyDefenseModifiers(&scriptRNG{vals: []uint64{7}}, 20, DamageClassWeapon, p, false)
		if err != nil || res.FinalDamage != 9 {
			t.Fatalf("perm %d = %+v, %v; want 9", i, res, err)
		}
		res, err = ApplyDefenseModifiers(&scriptRNG{vals: []uint64{7}}, 5, DamageClassWeapon, p, false)
		if err != nil || res.FinalDamage != 1 {
			t.Fatalf("capped perm %d = %+v, %v; want 1", i, res, err)
		}
	}
}

func TestShieldBlockMitigationIntegration(t *testing.T) {
	// Compose the REAL rating + roll + reduction with Gold-shield
	// numbers (bonus 10, r 1): the bonus lands in rating and chance,
	// never in DefensePower (spec §9.2.13).
	cap := enabledCap()
	rating, err := ResolveBlockComponent(40, 10, cap)
	if err != nil || rating != 50 {
		t.Fatalf("gold rating = %d, %v; want 50", rating, err)
	}
	// Chance = ((100-25)*40)/100+25+10 = 30+35 = 65; force roll 65.
	out, err := RollBlock(&scriptRNG{vals: []uint64{64}}, 40, 25, 10)
	if err != nil || !out.Succeeded {
		t.Fatalf("gold block = %+v, %v; want success", out, err)
	}
	mods := []DefenseModifier{{DefensePower: 0, DamageReduce: 1, RequiresBlock: true}}
	res, err := ApplyDefenseModifiers(&scriptRNG{vals: []uint64{1}}, 10, DamageClassWeapon, mods, out.Succeeded)
	if err != nil || res.FinalDamage != 9 || res.ModifiersApplied != 1 {
		t.Fatalf("gold reduction = %+v, %v; want 9", res, err)
	}
	// Failed block: no reduction.
	res, err = ApplyDefenseModifiers(&scriptRNG{vals: []uint64{1}}, 10, DamageClassWeapon, mods, false)
	if err != nil || res.FinalDamage != 10 || res.ModifiersApplied != 0 {
		t.Fatalf("failed block = %+v, %v; want 10", res, err)
	}
	// Power path excludes the shield bonus by construction.
	if _, err := ResolveDefensePowerModifier(mods); err != nil {
		t.Fatalf("shield power path: %v", err)
	}
}

func TestDefenseZeroStageOwnership(t *testing.T) {
	// +100 resistance yields exactly 0 through T2 alone; only the
	// TEST composes into T1 caps to prove stage ownership (spec
	// §9.2.2, §9.2.18): T2 production never calls ApplyPlayerDamageCaps.
	zeroed, err := ApplyResistance(100, 100)
	if err != nil || zeroed != 0 {
		t.Fatalf("resisted = %d, %v; want 0", zeroed, err)
	}
	final, err := ApplyPlayerDamageCaps(zeroed,
		VictimSnapshot{HP: 20, BaseMaxHP: 20}, false)
	if err != nil || final != 1 {
		t.Fatalf("capped = %d, %v; want 1", final, err)
	}
}

func TestDefenseOverflowRobustness(t *testing.T) {
	// Large legal inputs: no panic, no sign flip, documented bounds hold.
	if _, err := DefenseSkillChance(math.MaxInt32, 0, math.MaxInt32); err != nil {
		t.Fatalf("huge chance: %v", err)
	}
	if got, err := ApplyResistance(math.MaxInt32, -100); err != nil || got != 2*math.MaxInt32 {
		t.Fatalf("huge vuln = %d, %v", got, err)
	}
	if got, err := ApplyResistance(math.MaxInt32, 100); err != nil || got != 0 {
		t.Fatalf("huge resist = %d, %v", got, err)
	}
	if _, err := RollDamageReduction(&scriptRNG{vals: []uint64{math.MaxUint64}}, math.MaxInt32, math.MaxInt32, DamageClassWeapon); err != nil {
		t.Fatalf("huge reduction: %v", err)
	}
	if _, err := ResolveDefensePowerModifier([]DefenseModifier{{DefensePower: math.MaxInt}, {DefensePower: math.MaxInt}}); err != nil {
		t.Fatalf("huge power: %v", err)
	}
	if got, err := ResolveBlockComponent(math.MaxInt32, math.MaxInt32, enabledCap()); err != nil || got != maxBlockRating {
		t.Fatalf("huge block = %d, %v", got, err)
	}
}

func FuzzDefenseReductionBounds(f *testing.F) {
	f.Add(6, 20, uint64(4), 0)
	f.Add(0, 1, uint64(0), 1)
	f.Add(30, 3, uint64(20), 2)
	f.Fuzz(func(t *testing.T, r, dmg int, draw uint64, class int) {
		if r < 0 || r > 1000000 || dmg < 0 || dmg > 1000000 {
			t.Skip()
		}
		c := DamageClass((class%3 + 3) % 3)
		got, err := RollDamageReduction(&scriptRNG{vals: []uint64{draw}}, r, dmg, c)
		if err != nil {
			t.Fatalf("panic-guard err: %v", err)
		}
		if got < 0 || (dmg >= 1 && got > dmg-1) || (dmg == 0 && got != 0) {
			t.Fatalf("reduction %d out of [0,%d] (r=%d dmg=%d class=%d)", got, dmg-1, r, dmg, c)
		}
		if c == DamageClassSpell && got != 0 {
			t.Fatalf("spell reduction %d; want 0", got)
		}
	})
}

func FuzzBlockRatingBounds(f *testing.F) {
	f.Add(40, 5, true)
	f.Add(0, 0, true)
	f.Add(200, 200, false)
	f.Fuzz(func(t *testing.T, skill, bonus int, shield bool) {
		if skill < 0 || skill > 1000000 || bonus < -1000000 || bonus > 1000000 {
			t.Skip()
		}
		cap := enabledCap()
		cap.HasShield = shield
		got, err := ResolveBlockComponent(skill, bonus, cap)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !shield && got != 0 {
			t.Fatalf("disabled block = %d; want 0", got)
		}
		if shield && (got < 1 || got > 120) {
			t.Fatalf("enabled block = %d; want 1..120", got)
		}
	})
}
