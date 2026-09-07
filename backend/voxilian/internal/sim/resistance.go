package sim

// M5-T2 resistance domain (spec §9.2.14–§9.2.17, source battler.kod
// ResistanceCheck/GetDamageFromResistance, blakston.khd ATCK_* bits).
//
// ResistanceTag carries a source ATCK_* bit value within its domain.
// Weapon and spell bits share numeric values (both ALL bits are
// 0x0001), so the domain selector lives on the entry (explicit
// boolean), never in sign tricks — source stores spell entries as
// negated types, T2 does not.
type ResistanceTag uint32

const (
	// Weapon-domain tags (source ATCK_WEAP_*).
	ResistWeaponAll      ResistanceTag = 0x00001
	ResistWeaponNonMagic ResistanceTag = 0x00002
	ResistWeaponMagic    ResistanceTag = 0x00004
	ResistWeaponHit      ResistanceTag = 0x00008
	ResistWeaponBludgeon ResistanceTag = 0x00010
	ResistWeaponPierce   ResistanceTag = 0x00020
	ResistWeaponThrust   ResistanceTag = 0x00040
	ResistWeaponSlash    ResistanceTag = 0x00080
	ResistWeaponWhip     ResistanceTag = 0x00100
	ResistWeaponClaw     ResistanceTag = 0x00200
	ResistWeaponBite     ResistanceTag = 0x00400
	ResistWeaponSting    ResistanceTag = 0x00800
	ResistWeaponAcid     ResistanceTag = 0x01000
	ResistWeaponUnarmed  ResistanceTag = 0x02000
	ResistWeaponPunch    ResistanceTag = 0x04000
	ResistWeaponKick     ResistanceTag = 0x08000
	ResistWeaponNerudite ResistanceTag = 0x10000
	ResistWeaponSilver   ResistanceTag = 0x20000

	// Spell-domain tags (source ATCK_SPELL_*).
	ResistSpellAll         ResistanceTag = 0x0001
	ResistSpellFire        ResistanceTag = 0x0002
	ResistSpellShock       ResistanceTag = 0x0004
	ResistSpellCold        ResistanceTag = 0x0008
	ResistSpellHoly        ResistanceTag = 0x0010
	ResistSpellUnholy      ResistanceTag = 0x0020
	ResistSpellAcid        ResistanceTag = 0x0040
	ResistSpellQuake       ResistanceTag = 0x0080
	ResistSpellHunterSword ResistanceTag = 0x0100
)

// Resistance bounds (spec §9.2.16, source NO/MAX/MIN_RESISTANCE).
const (
	noResistance  = 0
	maxResistance = 100
	minResistance = -100
)

// ResistanceEntry is one resistance source value (spec §9.2.14).
// Value is unbounded on input (source AddResistance never clips;
// stacking may exceed ±100) — aggregation clips. Positive values
// resist, negative values are weaknesses.
type ResistanceEntry struct {
	IsSpell bool
	Tag     ResistanceTag
	Value   int
}

// DamageSignature advertises one damage event's applicable resistance
// domains as raw bitvectors (spec §9.2.14), exactly as source
// AssessDamage receives atype/aspell. Callers pass them through; ALL
// matching happens inside ResolveResistance. No caller normalization
// is required (weapon signatures omit the ALL bit while spell
// signatures include it; both resolve identically).
type DamageSignature struct {
	Weapon uint32
	Spell  uint32
}

// resistanceKey identifies one merge bucket for duplicate summation.
type resistanceKey struct {
	isSpell bool
	tag     ResistanceTag
}

// ResolveResistance computes the effective resistance for one damage
// event (spec §9.2.16, source battler.kod ResistanceCheck):
//
//	best = largest matching value strictly > 0, clipped above at +100
//	worst = most-negative matching value strictly < 0, clipped below at -100
//	effective = best + worst   (structurally in -100..+100)
//
// A weapon entry with type T matches iff (sig.Weapon & T) != 0, or
// sig.Weapon != 0 when T is WEAP_ALL. A spell entry matches iff
// (sig.Spell & T) != 0, or sig.Spell != 0 when T is SPELL_ALL.
// Duplicate same-(domain,tag) entries are merged by summation first
// (mirrors source AddResistance, which sums into one list element).
// The input slice is never mutated. No resistance means 0.
func ResolveResistance(entries []ResistanceEntry, sig DamageSignature) int {
	merged := make(map[resistanceKey]int64, len(entries))
	order := make([]resistanceKey, 0, len(entries))
	for _, e := range entries {
		k := resistanceKey{isSpell: e.IsSpell, tag: e.Tag}
		if _, seen := merged[k]; !seen {
			order = append(order, k)
		}
		merged[k] = satAddSigned(merged[k], int64(e.Value))
	}
	var best, worst int64
	for _, k := range order {
		v := merged[k]
		matched := false
		if !k.isSpell {
			t := uint32(k.tag)
			if sig.Weapon&uint32(t) != 0 {
				matched = true
			} else if sig.Weapon != 0 && ResistanceTag(t) == ResistWeaponAll {
				matched = true
			}
		} else {
			t := uint32(k.tag)
			if sig.Spell&uint32(t) != 0 {
				matched = true
			} else if sig.Spell != 0 && ResistanceTag(t) == ResistSpellAll {
				matched = true
			}
		}
		if !matched {
			continue
		}
		if v > best {
			best = v
		}
		if v < worst {
			worst = v
		}
	}
	if best > maxResistance {
		best = maxResistance
	}
	if worst < minResistance {
		worst = minResistance
	}
	return int(best + worst)
}

// ApplyResistance transforms damage by the effective resistance
// (spec §9.2.17, source GetDamageFromResistance, unified form — the
// two source branches are bit-identical in integer arithmetic):
//
//	result = (damage * (100 - effective)) / 100
//
// single truncation on a 64-bit saturating intermediate. effective is
// defensively bounded to [-100,+100] (ResolveResistance output is
// already in range, making the bound a no-op on composed paths).
// +100 yields exactly 0 (no minimum-one inside T2); -100 exactly
// doubles; 0 is identity; damage 0 yields 0. Negative damage is
// ErrInvalidDamageValue.
func ApplyResistance(damage, effective int) (int, error) {
	if damage < 0 {
		return 0, ErrInvalidDamageValue
	}
	if damage == 0 {
		return 0, nil
	}
	eff := boundInt64(int64(effective), minResistance, maxResistance)
	return int(satMul(int64(damage), 100-eff) / 100), nil
}
