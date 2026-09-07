package sim

import (
	"math"
	"testing"
)

func TestResistanceAggregationGolden(t *testing.T) {
	// Hand-computed per spec §9.2.16: best strictly-positive +
	// worst strictly-negative, per-side clip, sum.
	slash := DamageSignature{Weapon: uint32(ResistWeaponNonMagic) | uint32(ResistWeaponSlash)}
	cases := []struct {
		name    string
		entries []ResistanceEntry
		sig     DamageSignature
		want    int
	}{
		{"none", nil, slash, 0},
		{"unrelated ignored", []ResistanceEntry{{Tag: ResistWeaponBludgeon, Value: 40}}, slash, 0},
		{"single positive", []ResistanceEntry{{Tag: ResistWeaponSlash, Value: 20}}, slash, 20},
		{"best of positives", []ResistanceEntry{
			{Tag: ResistWeaponSlash, Value: 20},
			{Tag: ResistWeaponAll, Value: 50},
		}, slash, 50},
		{"worst of weaknesses", []ResistanceEntry{
			{Tag: ResistWeaponSlash, Value: -10},
			{Tag: ResistWeaponAll, Value: -30},
		}, slash, -30},
		{"positive plus weakness", []ResistanceEntry{
			{Tag: ResistWeaponSlash, Value: 50},
			{Tag: ResistWeaponAll, Value: -30},
		}, slash, 20},
		{"upper clip", []ResistanceEntry{{Tag: ResistWeaponSlash, Value: 150}}, slash, 100},
		{"lower clip", []ResistanceEntry{{Tag: ResistWeaponSlash, Value: -150}}, slash, -100},
		{"opposed clips cancel", []ResistanceEntry{
			{Tag: ResistWeaponSlash, Value: 150},
			{Tag: ResistWeaponAll, Value: -150},
		}, slash, 0},
		{"matching zero inert", []ResistanceEntry{{Tag: ResistWeaponSlash, Value: 0}}, slash, 0},
	}
	for _, c := range cases {
		if got := ResolveResistance(c.entries, c.sig); got != c.want {
			t.Fatalf("%s = %d; want %d", c.name, got, c.want)
		}
	}
}

func TestResistanceDuplicateMerge(t *testing.T) {
	// Same-tag duplicates sum before matching (mirrors source
	// AddResistance, which merges into one list element).
	slash := DamageSignature{Weapon: uint32(ResistWeaponSlash)}
	entries := []ResistanceEntry{
		{Tag: ResistWeaponSlash, Value: 30},
		{Tag: ResistWeaponSlash, Value: 30},
	}
	if got := ResolveResistance(entries, slash); got != 60 {
		t.Fatalf("merged duplicates = %d; want 60", got)
	}
	// Net-zero duplicates match but change nothing.
	entries = []ResistanceEntry{
		{Tag: ResistWeaponSlash, Value: 25},
		{Tag: ResistWeaponSlash, Value: -25},
	}
	if got := ResolveResistance(entries, slash); got != 0 {
		t.Fatalf("netted duplicates = %d; want 0", got)
	}
}

func TestResistanceWildcardSpecific(t *testing.T) {
	// Source content vectors (spec §9.2.22): each entry counted once,
	// unrelated types ignored, weapon/spell domains separated.
	weaponSig := DamageSignature{Weapon: uint32(ResistWeaponNonMagic) | uint32(ResistWeaponSlash)}
	fireSig := DamageSignature{Spell: uint32(ResistSpellAll) | uint32(ResistSpellFire)}
	holySig := DamageSignature{Spell: uint32(ResistSpellAll) | uint32(ResistSpellHoly)}
	pierceSig := DamageSignature{Weapon: uint32(ResistWeaponNonMagic) | uint32(ResistWeaponPierce)}

	// Leather WEAP_ALL +5 with Gold slash/bludgeon/thrust +10 each:
	// slash attack sees +10 (best), bludgeon entry ignored.
	leatherGold := []ResistanceEntry{
		{Tag: ResistWeaponAll, Value: 5},
		{Tag: ResistWeaponSlash, Value: 10},
		{Tag: ResistWeaponBludgeon, Value: 10},
	}
	if got := ResolveResistance(leatherGold, weaponSig); got != 10 {
		t.Fatalf("leather+gold vs slash = %d; want 10", got)
	}
	// Helm SPELL_ALL +15 with Nerudite fire/shock/cold/acid +20:
	// fire attack sees +20.
	helmNeru := []ResistanceEntry{
		{IsSpell: true, Tag: ResistSpellAll, Value: 15},
		{IsSpell: true, Tag: ResistSpellFire, Value: 20},
		{IsSpell: true, Tag: ResistSpellShock, Value: 20},
	}
	if got := ResolveResistance(helmNeru, fireSig); got != 20 {
		t.Fatalf("helm+nerudite vs fire = %d; want 20", got)
	}
	// Knight SPELL_ALL -20 with Plate fire -10 / shock -15: fire
	// attack sees only the worst weakness (-20).
	knightPlate := []ResistanceEntry{
		{IsSpell: true, Tag: ResistSpellAll, Value: -20},
		{IsSpell: true, Tag: ResistSpellFire, Value: -10},
	}
	if got := ResolveResistance(knightPlate, fireSig); got != -20 {
		t.Fatalf("knight+plate vs fire = %d; want -20", got)
	}
	// Knight pierce +10 with disciple WEAP_ALL -10 vs pierce attack:
	// +10 best, -10 worst -> 0.
	knightDisciple := []ResistanceEntry{
		{Tag: ResistWeaponPierce, Value: 10},
		{Tag: ResistWeaponAll, Value: -10},
	}
	if got := ResolveResistance(knightDisciple, pierceSig); got != 0 {
		t.Fatalf("knight+disciple vs pierce = %d; want 0", got)
	}
	// Same entries vs holy spell: weapon entries ignored, disciple
	// shock +15 ignored, holy -20 (orc-style) applies.
	holyEntries := []ResistanceEntry{
		{Tag: ResistWeaponPierce, Value: 10},
		{IsSpell: true, Tag: ResistSpellHoly, Value: -20},
		{IsSpell: true, Tag: ResistSpellShock, Value: 15},
	}
	if got := ResolveResistance(holyEntries, holySig); got != -20 {
		t.Fatalf("holy entries vs holy = %d; want -20", got)
	}
	// Zero-vector signature matches nothing (ALL special-case needs
	// a nonzero vector).
	zero := DamageSignature{}
	all := []ResistanceEntry{
		{Tag: ResistWeaponAll, Value: 50},
		{IsSpell: true, Tag: ResistSpellAll, Value: 50},
	}
	if got := ResolveResistance(all, zero); got != 0 {
		t.Fatalf("zero signature = %d; want 0", got)
	}
}

func TestResistanceTransformGolden(t *testing.T) {
	// Hand-computed unified form damage*(100-eff)/100 (spec §9.2.17).
	cases := []struct{ dmg, eff, want int }{
		{100, 0, 100},
		{100, 25, 75},
		{100, 50, 50},
		{100, 100, 0}, // exactly 0: no minimum inside T2
		{100, -25, 125},
		{100, -100, 200}, // exactly double
		{7, 50, 3},       // 7*50/100 = 350/100 truncation
		{13, -25, 16},    // 13*125/100 = 1625/100 truncation
		{29, 25, 21},     // 29*75/100 = 2175/100 truncation
		{0, 50, 0},
		{0, -100, 0},
	}
	for _, c := range cases {
		got, err := ApplyResistance(c.dmg, c.eff)
		if err != nil || got != c.want {
			t.Fatalf("resist(%d,%d) = %d, %v; want %d",
				c.dmg, c.eff, got, err, c.want)
		}
	}
	// Out-of-range effective values are defensively clipped (composed
	// paths never produce them).
	if got, _ := ApplyResistance(100, 250); got != 0 {
		t.Fatalf("over-clip = %d; want 0", got)
	}
	if got, _ := ApplyResistance(100, -250); got != 200 {
		t.Fatalf("under-clip = %d; want 200", got)
	}
	if _, err := ApplyResistance(-5, 0); err != ErrInvalidDamageValue {
		t.Fatalf("negative damage err = %v", err)
	}
}

func TestResistanceInputImmutability(t *testing.T) {
	entries := []ResistanceEntry{
		{Tag: ResistWeaponSlash, Value: 30},
		{Tag: ResistWeaponSlash, Value: 20},
		{IsSpell: true, Tag: ResistSpellFire, Value: -10},
	}
	snapshot := append([]ResistanceEntry(nil), entries...)
	sig := DamageSignature{
		Weapon: uint32(ResistWeaponSlash),
		Spell:  uint32(ResistSpellFire),
	}
	ResolveResistance(entries, sig)
	for i := range entries {
		if entries[i] != snapshot[i] {
			t.Fatalf("input mutated at %d: %+v vs %+v", i, entries[i], snapshot[i])
		}
	}
	mods := []DefenseModifier{{DamageReduce: 6}, {DamageReduce: 4, RequiresBlock: true}}
	modSnap := append([]DefenseModifier(nil), mods...)
	_, _ = ApplyDefenseModifiers(&scriptRNG{vals: []uint64{1}}, 20, DamageClassWeapon, mods, true)
	for i := range mods {
		if mods[i] != modSnap[i] {
			t.Fatalf("mods mutated at %d", i)
		}
	}
}

func TestResistanceDeterminismTrace(t *testing.T) {
	// Same scripted RNG + same immutable input -> same output,
	// across both stages composed.
	entries := []ResistanceEntry{
		{Tag: ResistWeaponAll, Value: 5},
		{Tag: ResistWeaponSlash, Value: 10},
		{IsSpell: true, Tag: ResistSpellFire, Value: -10},
	}
	mods := []DefenseModifier{{DamageReduce: 6}, {DamageReduce: 1, RequiresBlock: true}}
	sig := DamageSignature{Weapon: uint32(ResistWeaponNonMagic) | uint32(ResistWeaponSlash)}
	run := func() (int, int) {
		m, _ := ApplyDefenseModifiers(&scriptRNG{vals: []uint64{4, 1}}, 20, DamageClassWeapon, mods, true)
		eff := ResolveResistance(entries, sig)
		out, _ := ApplyResistance(m.FinalDamage, eff)
		return m.FinalDamage, out
	}
	m1, o1 := run()
	m2, o2 := run()
	if m1 != m2 || o1 != o2 {
		t.Fatalf("nondeterministic trace: (%d,%d) vs (%d,%d)", m1, o1, m2, o2)
	}
	// Hand oracle: armor 20-6-1 = 13; resistance best +10 -> 13*90/100 = 11.
	if m1 != 13 || o1 != 11 {
		t.Fatalf("trace = (%d,%d); want (13,11)", m1, o1)
	}
}

func TestResistanceOverflowRobustness(t *testing.T) {
	huge := DamageSignature{Weapon: math.MaxUint32, Spell: math.MaxUint32}
	entries := []ResistanceEntry{
		{Tag: ResistWeaponAll, Value: math.MaxInt},
		{Tag: ResistWeaponSlash, Value: math.MinInt},
		{IsSpell: true, Tag: ResistSpellAll, Value: math.MaxInt},
		{IsSpell: true, Tag: ResistSpellQuake, Value: math.MinInt},
	}
	if got := ResolveResistance(entries, huge); got < -100 || got > 100 {
		t.Fatalf("huge aggregation = %d; want -100..100", got)
	}
	if got, err := ApplyResistance(math.MaxInt32, -100); err != nil || got != 2*math.MaxInt32 {
		t.Fatalf("huge vuln = %d, %v", got, err)
	}
}

func FuzzResistanceAggregationBounds(f *testing.F) {
	f.Add(uint32(0x82), uint32(0), int32(10), int32(0))
	f.Add(uint32(0), uint32(0x03), int32(-15), int32(20))
	f.Fuzz(func(t *testing.T, weapon, spell uint32, v0, v1 int32) {
		entries := []ResistanceEntry{
			{Tag: ResistWeaponAll, Value: int(v0)},
			{Tag: ResistWeaponSlash, Value: int(v1)},
			{IsSpell: true, Tag: ResistSpellAll, Value: int(v0)},
			{IsSpell: true, Tag: ResistSpellFire, Value: int(v1)},
		}
		got := ResolveResistance(entries, DamageSignature{Weapon: weapon, Spell: spell})
		if got < -100 || got > 100 {
			t.Fatalf("effective %d out of range", got)
		}
	})
}

func FuzzResistanceTransformBounds(f *testing.F) {
	f.Add(100, 25)
	f.Add(7, -100)
	f.Add(0, 100)
	f.Fuzz(func(t *testing.T, dmg, eff int) {
		if dmg < 0 || dmg > 1000000 || eff < -1000000 || eff > 1000000 {
			t.Skip()
		}
		got, err := ApplyResistance(dmg, eff)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		// Monotonic source guarantees: positive resistance never
		// increases, negative never decreases, hard bounds hold.
		clipped := eff
		if clipped > 100 {
			clipped = 100
		}
		if clipped < -100 {
			clipped = -100
		}
		if clipped >= 0 && got > dmg {
			t.Fatalf("resist increased %d -> %d (eff %d)", dmg, got, eff)
		}
		if clipped <= 0 && got < dmg {
			t.Fatalf("weakness decreased %d -> %d (eff %d)", dmg, got, eff)
		}
		if got < 0 || got > 2*dmg {
			t.Fatalf("result %d out of [0,%d] (eff %d)", got, 2*dmg, eff)
		}
	})
}
