package sim

import (
	"errors"
	"fmt"
)

// Stable death-domain errors (spec §9.5). Matching MUST use errors.Is,
// never string parsing as control flow. Existing shared errors are
// reused: ErrNilRNG (nil RNG on a rolling planner), ErrInvalidVitals
// (corrupt PlayerVitals input), ErrInvalidCombatStat (effective
// attribute outside 1..70), ErrInvalidSpellPower (portal power outside
// 1..99), ErrInvalidHealthAmount (impossible HP adjustment).
var (
	// ErrInvalidDeathCost marks a death-cost input outside its frozen
	// domain: default cost 1..100, pending cost 0..100.
	ErrInvalidDeathCost = errors.New("sim: invalid death cost")
	// ErrInvalidDeathTime marks a negative death-time/age scalar.
	ErrInvalidDeathTime = errors.New("sim: invalid death time")
	// ErrInvalidDeathInput marks any other call-contract violation:
	// unknown disposition, contradictory resolved inputs, duplicate or
	// out-of-domain item/ability keys, advancement/vitals/drop planning
	// requested for an avoided death.
	ErrInvalidDeathInput = errors.New("sim: invalid death input")
)

// Frozen source-derived death constants (spec §9.5.3, §9.5.4, §9.5.5,
// §9.5.9, §9.5.10, §9.5.12–§9.5.14; player.kod, body.kod, portlife.kod,
// blakston.khd). These are immutable plan data; no timer, entity, or
// row is created here.
const (
	// PlayerCorpseDecomposeMs is the player-corpse lifetime
	// (body.kod CreateTimer 600000; mob corpses use 120000 elsewhere).
	PlayerCorpseDecomposeMs int64 = 600000
	// PlayerCorpseNoStealMs is the initial period during which only the
	// corpse's own player may take items (body.kod NoStealTimer).
	PlayerCorpseNoStealMs int64 = 25000
	// PKProtectionDurationMs is the dropped-loot PK pickup restriction
	// (player.kod PKPOINTER_TIME = 10*60*1000).
	PKProtectionDurationMs int64 = 600000
	// DoubleDeathGuardSeconds is the double-death rejection window
	// (player.kod: a death less than this many whole seconds after the
	// previous one is discarded; exactly +2 proceeds).
	DoubleDeathGuardSeconds int64 = 2

	minDefaultDeathCost = 1 // settings.kod documented domain
	maxDefaultDeathCost = 100
	minPendingDeathCost = 0 // zero = cheap death (no penalties)
	maxPendingDeathCost = 100

	portalCostFloor            = 5 // portlife.kod bound(newCost,5,80)
	portalCostCeil             = 80
	portalFreshBoundarySeconds = 60 // strict < boundary

	// deathAbilityEligibilityAbove: abilities strictly greater than this
	// are eligible for death loss (source `> 5`).
	deathAbilityEligibilityAbove = 5
	deathAbilityFloor            = 1 // ChangeSpellAbility/ChangeSkillAbility bound
	deathAbilityCeil             = 99
	ordinaryAbilityLoss          = -1
	murdererAbilityLoss          = -2

	// pkillEnableHP is the guild-quit base-HP threshold
	// (blakston.khd PKILL_ENABLE_HP).
	pkillEnableHP = 30

	// Post-death vitals (player.kod Killed).
	deathVigorDivisor = 4
	deathVigorCap     = 50 // bound(Vigor/4, 0, 50) before NewVigor
	angelManaBonus    = 2  // Mana = MaxMana/2 + 2 for eligible players

	// Karma booby prize (player.kod Killed; karma is stored x100).
	hammerKarmaThreshold = 5000
	karmaPrizeFloor      = 0
	maxKarmaHundredths   = 10000
	minKarmaHundredths   = -10000
)

// DeathDisposition classifies a death attempt (spec §9.5.2). The three
// values are semantically distinct: Avoided is NOT a death (no corpse,
// no drop, no Underworld, no kill record); Cheap is a REAL death with
// zero drop/penalty cost (corpse still created, pipeline still runs);
// Normal is a real death with the settings-sourced cost.
type DeathDisposition int8

const (
	// DeathAvoided: arena non-real death, prison room, or
	// safe-player-attack room. HP becomes 1; T5c composes the
	// NewHealth-equivalent runtime follow-up.
	DeathAvoided DeathDisposition = iota
	// DeathCheap: frenzy night, newbie-zone room, newbie-honor string,
	// or token death. Corpse yes, drops no, advancement untouched,
	// DeathCost 0.
	DeathCheap
	// DeathNormal: ordinary real death. Full pipeline, DeathCost = the
	// resolved settings default (1..100).
	DeathNormal
)

// String implements fmt.Stringer for readable test failures.
func (d DeathDisposition) String() string {
	switch d {
	case DeathAvoided:
		return "avoided"
	case DeathCheap:
		return "cheap"
	case DeathNormal:
		return "normal"
	default:
		return fmt.Sprintf("unknown(%d)", int8(d))
	}
}

// checkDisposition rejects unknown enum values (hostile casts).
func checkDisposition(d DeathDisposition) error {
	if d != DeathAvoided && d != DeathCheap && d != DeathNormal {
		return fmt.Errorf("sim: death disposition %d: %w", int8(d), ErrInvalidDeathInput)
	}
	return nil
}

// ValidateDefaultDeathCost rejects a resolved settings default outside
// 1..100 (spec §9.5.9). The default is server configuration, never a
// silent 100.
func ValidateDefaultDeathCost(cost int) error {
	if cost < minDefaultDeathCost || cost > maxDefaultDeathCost {
		return fmt.Errorf("sim: default death cost %d: %w", cost, ErrInvalidDeathCost)
	}
	return nil
}

// ValidatePendingDeathCost rejects a pending cost outside 0..100
// (0 = cheap; portal results are 5..80; spec §9.5.9).
func ValidatePendingDeathCost(cost int) error {
	if cost < minPendingDeathCost || cost > maxPendingDeathCost {
		return fmt.Errorf("sim: pending death cost %d: %w", cost, ErrInvalidDeathCost)
	}
	return nil
}

// DeathBlockedByDoubleDeath freezes the source double-death guard
// (spec §9.5.3): a death is discarded iff now is STRICTLY less than
// lastDeath + 2 seconds (whole seconds; exactly +2 proceeds). Pure
// decision over resolved scalars; T5c owns the runtime gate.
func DeathBlockedByDoubleDeath(lastDeathSeconds, nowSeconds int64) (bool, error) {
	if lastDeathSeconds < 0 || nowSeconds < 0 {
		return false, fmt.Errorf("sim: death time %d/%d: %w", lastDeathSeconds, nowSeconds, ErrInvalidDeathTime)
	}
	guarded, ok := checkedAdd(lastDeathSeconds, DoubleDeathGuardSeconds)
	if !ok {
		return false, fmt.Errorf("sim: death time %d: %w", lastDeathSeconds, ErrInvalidDeathTime)
	}
	return nowSeconds < guarded, nil
}

// DeathContext carries every RESOLVED world/game-mode fact the source
// Killed routing consumes (spec §9.5.2). The caller resolves room
// membership, arena state, honor strings, frenzy night, and carried
// tokens; NO volume flags, room IDs, or item classes live here. The
// three avoided conditions are only resolvable for a logged-on death;
// the runtime must not set them otherwise (source gates the whole
// branch on pbLogged_on).
type DeathContext struct {
	// FrenzyActive: global chaos/frenzy night at death time (cheap +
	// special post-death vitals branch).
	FrenzyActive bool
	// NewbieZoneDeath: the death room lies in the resolved newbie room
	// range (cheap + newbie-home respawn target).
	NewbieZoneDeath bool
	// NewbieHonor: the victim's honor string is the resolved newbie
	// honor (cheap).
	NewbieHonor bool
	// CarriesToken: the victim holds a resolved token item (cheap,
	// token becomes unused).
	CarriesToken bool
	// ArenaNonRealDeath: arena room AND in play AND arena-real-death
	// disabled (avoided).
	ArenaNonRealDeath bool
	// PrisonRoom: OutOfGrace prison room class (avoided).
	PrisonRoom bool
	// SafePlayerAttack: room safe-player-attack fact, source
	// ROOM_SAFE_DEATH (avoided).
	SafePlayerAttack bool
}

// DeathDispositionPlan is the immutable immediate-routing result
// (spec §9.5.2). It answers: is this a death, is it cheap, what is the
// starting cost, and which immediately-observable side conditions hold.
type DeathDispositionPlan struct {
	// Disposition of the death attempt.
	Disposition DeathDisposition
	// DeathCost is the starting pending cost: 0 for cheap deaths, the
	// validated settings default for normal deaths.
	DeathCost int
	// TokenDeath marks a token-triggered cheap death (the token becomes
	// unused; T5b1/T5c own that transition).
	TokenDeath bool
	// SpecialItemsKept: the source special-item KEEP-guard (sent as
	// ActivateCheapDeath in the avoided branch and after the
	// cheap-by-frenzy/newbie-zone/newbie-honor determination, BEFORE
	// the token check). Guard armed -> special artifacts SURVIVE the
	// death; guard not armed (normal deaths and pure TOKEN deaths — the
	// deliberate source ordering) -> the artifact is lost back into
	// circulation by its deferred OwnerKilled handling.
	SpecialItemsKept bool
	// KillerIsPlayer echoes the resolved killer-is-a-player fact; only
	// normal deaths actually apply PK protection to drops, so the drop
	// planner (not this plan) consumes it.
	KillerIsPlayer bool
	// NewbieHomeRespawn: a real death in the resolved newbie range
	// respawns at the newbie home instead of the Underworld (resolved
	// placement seam data for T5c; no coordinates here).
	NewbieHomeRespawn bool
}

// PlanDeathDisposition freezes the source Killed routing decision
// (spec §9.5.2) as a pure function over resolved facts:
//
//	avoided if arena-non-real OR prison OR safe-player-attack
//	else cheap  if frenzy OR newbie zone OR newbie honor
//	else cheap  if token death (special-item loss already armed)
//	else normal with the validated settings default cost
//
// defaultDeathCost is the resolved server setting (1..100). Validation
// happens before any decision output.
func PlanDeathDisposition(defaultDeathCost int, ctx DeathContext, killerIsPlayer bool) (DeathDispositionPlan, error) {
	if err := ValidateDefaultDeathCost(defaultDeathCost); err != nil {
		return DeathDispositionPlan{}, err
	}
	if ctx.ArenaNonRealDeath || ctx.PrisonRoom || ctx.SafePlayerAttack {
		return DeathDispositionPlan{
			Disposition:      DeathAvoided,
			SpecialItemsKept: true,
			KillerIsPlayer:   killerIsPlayer,
		}, nil
	}
	cheap := ctx.FrenzyActive || ctx.NewbieZoneDeath || ctx.NewbieHonor
	// The special-item keep-guard is armed by the frenzy/newbie
	// determination and NOT by the token check placed after it (source
	// ordering: token deaths still lose the artifact).
	kept := cheap
	token := false
	if ctx.CarriesToken {
		cheap = true
		token = true
	}
	if cheap {
		return DeathDispositionPlan{
			Disposition:       DeathCheap,
			DeathCost:         0,
			TokenDeath:        token,
			SpecialItemsKept:  kept,
			KillerIsPlayer:    killerIsPlayer,
			NewbieHomeRespawn: ctx.NewbieZoneDeath,
		}, nil
	}
	return DeathDispositionPlan{
		Disposition:    DeathNormal,
		DeathCost:      defaultDeathCost,
		KillerIsPlayer: killerIsPlayer,
	}, nil
}

// CorpsePolicy is the immutable player-corpse plan data (spec §9.5.4):
// lifetime, initial owner-only pickup window, and the whole-second
// death-time scalar Portal of Life consumes. Cheap real deaths create
// the SAME corpse. Constants only — no corpse entity, row, or timer is
// created by T5a.
type CorpsePolicy struct {
	LifetimeMs       int64
	NoStealMs        int64
	DeathTimeSeconds int64
}

// PlanCorpse freezes the source corpse policy for a real death at
// deathTimeSeconds (whole seconds, >= 0). The resurrected-once fact is
// a Portal input (PlanPortalOfLife), not corpse data.
func PlanCorpse(deathTimeSeconds int64) (CorpsePolicy, error) {
	if deathTimeSeconds < 0 {
		return CorpsePolicy{}, fmt.Errorf("sim: corpse death time %d: %w", deathTimeSeconds, ErrInvalidDeathTime)
	}
	return CorpsePolicy{
		LifetimeMs:       PlayerCorpseDecomposeMs,
		NoStealMs:        PlayerCorpseNoStealMs,
		DeathTimeSeconds: deathTimeSeconds,
	}, nil
}

// DeathItemInput is ONE already-resolved per-item death policy
// (spec §9.5.5). Key is an opaque deterministic identity for plan
// output only — mechanics never depend on its value and it carries NO
// durable identity semantics. DropOnDeath is the resolved item policy
// (source base true, with shield/ring/key/crystal and item-attribute
// vetoes). RoomAccepts is the resolved room hold/movement acceptance
// for this item at the death position (source ReqNewHold AND
// ReqSomethingMoved). SpecialItem marks artifacts whose keep/lose
// outcome rides the disposition's SpecialItemsKept guard (source
// ActivateCheapDeath/OwnerKilled), independent of dropping.
type DeathItemInput struct {
	Key         int
	DropOnDeath bool
	RoomAccepts bool
	SpecialItem bool
}

// DeathDropItem is one item's placement result in the ordered drop
// plan.
type DeathDropItem struct {
	Key int
	// Drop: the item relocates to the death position (source NewHold at
	// the death square, unmerged). False means the item stays with the
	// player (undroppable policy or a rejected room check).
	Drop bool
	// SpecialItem echoes the artifact mark; SpecialItemKept states the
	// keep-guard outcome for it (kept on avoided and non-token cheap
	// deaths; lost on normal and token deaths).
	SpecialItem     bool
	SpecialItemKept bool
	// NeedsPKProtection: the dropped item must carry the PK pickup
	// restriction (killer was a player; normal death only).
	NeedsPKProtection bool
	// PKProtectionDurationMs is PKProtectionDurationMs when protected,
	// 0 otherwise.
	PKProtectionDurationMs int64
}

// DeathDropPlan is the ordered immutable relocation plan (input order
// preserved over the two flat inventory families; source has no nested
// player containers, so there is no recursive descent).
type DeathDropPlan struct {
	Items []DeathDropItem
}

// PlanDeathDrops freezes the source drop loop (spec §9.5.5): for a
// NORMAL death, an item drops iff the room accepts it AND its resolved
// policy says drop-on-death; dropped items carry PK protection metadata
// when the killer was a player. CHEAP deaths drop nothing but still
// echo the special-item keep-guard outcome. AVOIDED deaths never reach
// drop planning. No catalog, PG, live inventory, or item classes are
// consulted.
func PlanDeathDrops(plan DeathDispositionPlan, items []DeathItemInput) (DeathDropPlan, error) {
	disposition := plan.Disposition
	if err := checkDisposition(disposition); err != nil {
		return DeathDropPlan{}, err
	}
	if disposition == DeathAvoided {
		return DeathDropPlan{}, fmt.Errorf("sim: drop plan for avoided death: %w", ErrInvalidDeathInput)
	}
	seen := make(map[int]struct{}, len(items))
	out := DeathDropPlan{Items: make([]DeathDropItem, 0, len(items))}
	for _, it := range items {
		if _, dup := seen[it.Key]; dup {
			return DeathDropPlan{}, fmt.Errorf("sim: drop item key %d duplicated: %w", it.Key, ErrInvalidDeathInput)
		}
		seen[it.Key] = struct{}{}
		drops := disposition == DeathNormal && it.DropOnDeath && it.RoomAccepts
		item := DeathDropItem{
			Key:             it.Key,
			Drop:            drops,
			SpecialItem:     it.SpecialItem,
			SpecialItemKept: it.SpecialItem && plan.SpecialItemsKept,
		}
		if drops && plan.KillerIsPlayer {
			item.NeedsPKProtection = true
			item.PKProtectionDurationMs = PKProtectionDurationMs
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

// DeathAdvancementPlan freezes the immediate advancement effects of a
// real death (spec §9.5.6). All fields are durable character-advancement
// state later written by T5b1; this value is only the plan.
type DeathAdvancementPlan struct {
	// PointsAfter: advancement points after the death (0 for normal;
	// unchanged for cheap).
	PointsAfter int
	// GainChanceAfter: gain chance after integer halving (truncation
	// toward zero, source KOD `/` = C division; usually negative).
	GainChanceAfter int
	// ResetGainFlags: clear did-damage/took-damage/dodged + kill target.
	ResetGainFlags bool
	// ResetAtrophyFlags: mark all spell entries unused (the atrophy
	// feature itself stays disabled).
	ResetAtrophyFlags bool
}

// PlanDeathAdvancement freezes the source immediate advancement block
// (spec §9.5.6): NORMAL death zeroes advancement points, halves the
// gain chance, and resets gain/atrophy flags; CHEAP death changes
// nothing (values echo through). AVOIDED deaths never reach this plan.
func PlanDeathAdvancement(disposition DeathDisposition, points, gainChance int) (DeathAdvancementPlan, error) {
	if err := checkDisposition(disposition); err != nil {
		return DeathAdvancementPlan{}, err
	}
	if disposition == DeathAvoided {
		return DeathAdvancementPlan{}, fmt.Errorf("sim: advancement plan for avoided death: %w", ErrInvalidDeathInput)
	}
	if disposition == DeathCheap {
		return DeathAdvancementPlan{
			PointsAfter:       points,
			GainChanceAfter:   gainChance,
			ResetGainFlags:    false,
			ResetAtrophyFlags: false,
		}, nil
	}
	return DeathAdvancementPlan{
		PointsAfter:       0,
		GainChanceAfter:   gainChance / 2, // Go int division truncates toward zero, matching KOD
		ResetGainFlags:    true,
		ResetAtrophyFlags: true,
	}, nil
}

// PostDeathVitalsInput is the resolved input for the immediate
// post-death vitals calculation (spec §9.5.7).
type PostDeathVitalsInput struct {
	// Vitals: the victim's current authoritative vitals (validated).
	Vitals PlayerVitals
	// Disposition: cheap or normal (avoided deaths never reach this
	// calculation; T5c composes their trivial HP=1).
	Disposition DeathDisposition
	// FrenzyActive: frenzy night at death time (cheap frenzy branch:
	// half maxima, vigor 100).
	FrenzyActive bool
	// AngelMailEligible: the resolved still-newbie AND not-murderer AND
	// cost>0 gate (source PFLAG_TUTORIAL false + PFLAG_MURDERER false).
	// Contradictory with FrenzyActive (frenzy forces cost 0).
	AngelMailEligible bool
}

// PlanPostDeathVitals freezes the source post-death vitals assignment
// (spec §9.5.7) as a pure calculation:
//
//	frenzy death: HP = MaxHP/2, Mana = MaxMana/2, Vigor = 100
//	ordinary death: HP = 1, Mana = 1,
//	                Vigor = bound(Vigor/4, 0, 50) then bound 1..200
//	angel override (eligible, non-frenzy): Mana = MaxMana/2 + 2
//
// Exertion, stomach, base/max HP and mana ceilings, and the rest
// threshold are untouched. The result always passes PlayerVitals.
// Validate(). No Engine, entity, or timer is touched (T5c recreates
// the regen timers per §9.4b).
func PlanPostDeathVitals(in PostDeathVitalsInput) (PlayerVitals, error) {
	if err := in.Vitals.Validate(); err != nil {
		return PlayerVitals{}, err
	}
	if err := checkDisposition(in.Disposition); err != nil {
		return PlayerVitals{}, err
	}
	if in.Disposition == DeathAvoided {
		return PlayerVitals{}, fmt.Errorf("sim: post-death vitals for avoided death: %w", ErrInvalidDeathInput)
	}
	if in.FrenzyActive && in.AngelMailEligible {
		return PlayerVitals{}, fmt.Errorf("sim: angel mail on frenzy death: %w", ErrInvalidDeathInput)
	}
	v := in.Vitals
	if in.FrenzyActive {
		v.HP = v.MaxHP / 2
		v.Mana = v.MaxMana / 2
		v.Vigor = 100
	} else {
		v.HP = 1
		v.Mana = 1
		vig := int64(v.Vigor) / deathVigorDivisor
		vig = boundInt64(vig, 0, deathVigorCap)
		vig = boundInt64(vig, minVigor, maxVigor) // source NewVigor bound
		out, ok := toInt(vig)
		if !ok {
			return PlayerVitals{}, fmt.Errorf("sim: post-death vigor: %w", ErrInvalidDeathInput)
		}
		v.Vigor = out
		if in.AngelMailEligible {
			half, ok := toInt(int64(v.MaxMana) / 2)
			if !ok {
				return PlayerVitals{}, fmt.Errorf("sim: angel mana: %w", ErrInvalidDeathInput)
			}
			mana, ok := checkedAdd(int64(half), angelManaBonus)
			if !ok {
				return PlayerVitals{}, fmt.Errorf("sim: angel mana: %w", ErrInvalidDeathInput)
			}
			outMana, ok := toInt(mana)
			if !ok {
				return PlayerVitals{}, fmt.Errorf("sim: angel mana: %w", ErrInvalidDeathInput)
			}
			v.Mana = outMana
		}
	}
	if err := v.Validate(); err != nil {
		return PlayerVitals{}, fmt.Errorf("sim: post-death vitals invalid: %w", err)
	}
	return v, nil
}

// DeathPhase is the persistence-agnostic lifecycle phase of the
// pending-death plan (spec §9.5.8).
type DeathPhase int8

const (
	// DeathPhaseNone: no death penalties are outstanding.
	DeathPhaseNone DeathPhase = iota
	// DeathPhasePending: the player died and Underworld-exit penalties
	// are outstanding (T5b1 persists this; T5b2 consumes it).
	DeathPhasePending
)

// PendingDeathPlan is the semantic state between immediate death and
// Underworld exit (spec §9.5.8), as an immutable value. It is
// deliberately persistence-agnostic: no revision, no nullable SQL
// shape, no JSON storage design. T5b1 owns the durable representation.
type PendingDeathPlan struct {
	Phase              DeathPhase
	EffectiveDeathCost int
	DeathTimeSeconds   int64
	Corpse             CorpsePolicy
}

// PlanPendingDeath composes the disposition plan and corpse policy
// into the pending-death value: avoided deaths have no pending phase;
// real deaths (cheap AND normal) are pending with their starting cost
// (cheap = 0) and death-time scalar.
func PlanPendingDeath(plan DeathDispositionPlan, corpse CorpsePolicy) (PendingDeathPlan, error) {
	if err := checkDisposition(plan.Disposition); err != nil {
		return PendingDeathPlan{}, err
	}
	if corpse.LifetimeMs != PlayerCorpseDecomposeMs || corpse.NoStealMs != PlayerCorpseNoStealMs {
		return PendingDeathPlan{}, fmt.Errorf("sim: pending-death corpse policy: %w", ErrInvalidDeathInput)
	}
	if corpse.DeathTimeSeconds < 0 {
		return PendingDeathPlan{}, fmt.Errorf("sim: death time %d: %w", corpse.DeathTimeSeconds, ErrInvalidDeathTime)
	}
	if plan.Disposition == DeathAvoided {
		return PendingDeathPlan{Phase: DeathPhaseNone}, nil
	}
	if err := ValidatePendingDeathCost(plan.DeathCost); err != nil {
		return PendingDeathPlan{}, err
	}
	return PendingDeathPlan{
		Phase:              DeathPhasePending,
		EffectiveDeathCost: plan.DeathCost,
		DeathTimeSeconds:   corpse.DeathTimeSeconds,
		Corpse:             corpse,
	}, nil
}

// PortalOfLifeInput is the resolved Portal-of-Life calculation input
// (spec §9.5.10): the victim's current pending cost, the corpse age in
// WHOLE seconds, and the caster's spell power (1..99).
type PortalOfLifeInput struct {
	PendingCost       int
	CorpseAgeSeconds  int64
	SpellPower        int
	CorpseAlreadyUsed bool
}

// PlanPortalOfLife freezes the source portlife.kod GetDeathCost
// formula (spec §9.5.10) as a pure calculation:
//
//	age < 60 : timeAdj = -(60 - age)        // age 0 -> -60 ... 59 -> -1
//	age >= 60: timeAdj = age/10 - 6         // age 60 -> 0 ... 600 -> +54
//	newCost  = pendingCost - (power - timeAdj)
//	newCost  = bound(newCost, 5, 80)
//
// Integer arithmetic with truncating division; the 60-second boundary
// is strict. CorpseAlreadyUsed reports the source resurrected flag: a
// corpse receives at most ONE portal (the caller resolves the flag;
// re-targeting an already-ported corpse is ErrInvalidDeathInput).
// Negative age is a domain error; power is validated 1..99 BEFORE any
// output. No spell/corpse lookup, gateway, timer, or DB.
func PlanPortalOfLife(in PortalOfLifeInput) (int, error) {
	if err := checkSpellPower(in.SpellPower); err != nil {
		return 0, err
	}
	if in.CorpseAgeSeconds < 0 {
		return 0, fmt.Errorf("sim: portal corpse age %d: %w", in.CorpseAgeSeconds, ErrInvalidDeathTime)
	}
	if err := ValidatePendingDeathCost(in.PendingCost); err != nil {
		return 0, err
	}
	if in.CorpseAlreadyUsed {
		return 0, fmt.Errorf("sim: portal on resurrected corpse: %w", ErrInvalidDeathInput)
	}
	var timeAdj int64
	if in.CorpseAgeSeconds < portalFreshBoundarySeconds {
		timeAdj = -(portalFreshBoundarySeconds - in.CorpseAgeSeconds)
	} else {
		timeAdj = in.CorpseAgeSeconds/10 - 6
	}
	// newCost = pendingCost - (power - timeAdj), overflow-safe in int64.
	delta, ok := checkedSub(int64(in.SpellPower), timeAdj)
	if !ok {
		return 0, fmt.Errorf("sim: portal adjustment: %w", ErrInvalidDeathInput)
	}
	newCost, ok := checkedSub(int64(in.PendingCost), delta)
	if !ok {
		return 0, fmt.Errorf("sim: portal adjustment: %w", ErrInvalidDeathInput)
	}
	newCost = boundInt64(newCost, portalCostFloor, portalCostCeil)
	out, ok := toInt(newCost)
	if !ok {
		return 0, fmt.Errorf("sim: portal adjustment: %w", ErrInvalidDeathInput)
	}
	return out, nil
}

// ReducePendingDeathCost freezes source SetDeathCost semantics
// (spec §9.5.10): the pending death cost only ever LOWERS — a proposed
// result at or above the current cost is ignored (admin override is out
// of scope). Both inputs must be valid pending costs.
func ReducePendingDeathCost(current, proposed int) (int, error) {
	if err := ValidatePendingDeathCost(current); err != nil {
		return 0, err
	}
	if err := ValidatePendingDeathCost(proposed); err != nil {
		return 0, err
	}
	if proposed < current {
		return proposed, nil
	}
	return current, nil
}

// DeathAbilityInput is one already-known ability of the victim
// (spec §9.5.13). Key is an opaque deterministic identity (stable
// spell/skill catalog IDs arrive later; mechanics never depend on the
// value). Ability is the current stored ability percentage 1..99.
type DeathAbilityInput struct {
	Key     int
	Ability int
}

// DeathAbilityLoss records one planned ability decrement, in roll and
// list order.
type DeathAbilityLoss struct {
	Key         int
	FromAbility int
	ToAbility   int
	Loss        int // FromAbility - ToAbility (> 0)
	StaminaRoll int // 1..100 (the failed save)
	CostRoll    int // 1..100 (< scaled cost)
}

// DeathPenaltyInput is the complete resolved input for the delayed
// Underworld-exit penalties (spec §9.5.11–§9.5.13).
type DeathPenaltyInput struct {
	// PendingCost: the effective pending cost 0..100 (possibly already
	// portal-reduced).
	PendingCost int
	// DefaultCost: the resolved server settings default 1..100 (the
	// full-cost comparison base).
	DefaultCost int
	// FrenzyActive at UNDERWORLD-EXIT time (not death time): the source
	// skips all penalties during frenzy.
	FrenzyActive bool
	// StillNewbie: source PFLAG_TUTORIAL is FALSE (the flag is TRUE
	// once a player is no longer a newbie). Resolved input; never
	// inferred from HP.
	StillNewbie bool
	// Murderer: resolved PFLAG_MURDERER.
	Murderer bool
	// Stamina: effective stamina 1..70 (resolved, no lookup).
	Stamina int
	// Vitals: current authoritative vitals (validated); consumed only
	// by the BaseMaxHP penalty composition.
	Vitals PlayerVitals
	// Spells and Skills: the victim's abilities in canonical
	// (caller-supplied deterministic) order.
	Spells []DeathAbilityInput
	Skills []DeathAbilityInput
}

// DeathPenaltyPlan is the immutable Underworld-exit penalty result
// (spec §9.5.11–§9.5.14). It states exactly what changes; T5b2 owns
// the durable transaction.
type DeathPenaltyPlan struct {
	// ClearOutlaw / ClearHaunted: full-cost (>= default) justice clears.
	// ClearHaunted is also set on a frenzy-exit (source returns early
	// after clearing the revenant flag; the un-consumed cost residue is
	// a source artifact — Voxilian always clears the pending phase).
	ClearOutlaw  bool
	ClearHaunted bool
	// ReevaluatePK: PK-status re-evaluation hook (full-cost branch;
	// phase-2 justice integration consumes it).
	ReevaluatePK bool
	// CostScaled: the still-newbie-and-not-murderer branch applied
	// cost/3 (no HP roll happened).
	CostScaled bool
	// ScaledCost: the cost actually used for the ability rolls (the /3
	// result, the unchanged cost, or 0).
	ScaledCost int
	// HPRolled/HPRoll: the HP-loss roll happened and its value (0 is
	// never a valid roll; rolls are 1..100). Only the
	// experienced-or-murderer branch rolls.
	HPRolled bool
	HPRoll   int
	// LostBaseMaxHP: the ACTUAL base-max delta applied (1, or 0 at the
	// floor 20). The same delta flowed into MaxHP (source
	// GainBaseMaxHealth -> GainMaxHealth); current HP is untouched.
	LostBaseMaxHP int
	// VitalsAfter: vitals after the composed T4a penalty helpers
	// (identical to Vitals when no loss).
	VitalsAfter PlayerVitals
	// QuitGuild: base max HP (after penalties) < PKILL_ENABLE_HP (30).
	// Phase-2 guild integration consumes it; no live guild state exists
	// on the player entity.
	QuitGuild bool
	// AbilityLosses: spells then skills, input order, losers only.
	AbilityLosses []DeathAbilityLoss
	// ClearPending: always true — the pending-death phase ends with
	// this plan (penalties exactly once; source zeroes piDeathCost).
	ClearPending bool
}

// checkAbilities validates one ability list: ability domain 1..99 and
// unique keys.
func checkAbilities(kind string, list []DeathAbilityInput) error {
	seen := make(map[int]struct{}, len(list))
	for _, a := range list {
		if a.Ability < deathAbilityFloor || a.Ability > deathAbilityCeil {
			return fmt.Errorf("sim: death %s ability %d: %w", kind, a.Ability, ErrInvalidDeathInput)
		}
		if _, dup := seen[a.Key]; dup {
			return fmt.Errorf("sim: death %s key %d duplicated: %w", kind, a.Key, ErrInvalidDeathInput)
		}
		seen[a.Key] = struct{}{}
	}
	return nil
}

// PlanDeathPenalties freezes source ApplyDeathPenalties (spec
// §9.5.11–§9.5.13) as a pure planner over resolved inputs with the
// exact frozen RNG consumption order:
//
//  0. ALL validation (including nil RNG) precedes the first roll.
//  1. frenzy at exit: clear haunted, nothing else (no rolls).
//  2. cost >= default: clear outlaw, re-evaluate PK, clear haunted.
//  3. cost > 0:
//     still-newbie AND not murderer -> cost = cost/3 (no roll)
//     ELSE -> HP roll: roll <= cost loses 1 BaseMaxHP (composed via
//     AdjustBaseMaxHP + AdjustMaxHP with the same delta;
//     current HP untouched; floor 20).
//  4. guild-quit check on the post-penalty base max HP (< 30) — every
//     non-frenzy exit, even cost 0.
//  5. loss amount: -2 if murderer else -1 (read here, after step 2's
//     clears — the async PK re-evaluation does not feed this check).
//  6. per spell then per skill, in input order: eligible iff ability
//     > 5; stamina save roll > Stamina fails (== saves); failed save
//     rolls the cost roll; cost roll < scaled cost loses; result
//     ability = bound(ability + loss, 1, 99).
//
// An ineligible ability consumes NO rolls; a passed stamina save
// consumes NO cost roll; a failed save consumes the cost roll even at
// cost 0 (the comparison still evaluates). Uses the injected RNG seam
// only; no global randomness.
func PlanDeathPenalties(rng RNG, in DeathPenaltyInput) (DeathPenaltyPlan, error) {
	var plan DeathPenaltyPlan
	if rng == nil {
		return plan, ErrNilRNG
	}
	if err := ValidatePendingDeathCost(in.PendingCost); err != nil {
		return plan, err
	}
	if err := ValidateDefaultDeathCost(in.DefaultCost); err != nil {
		return plan, err
	}
	if err := checkEffectiveAttr("stamina", in.Stamina); err != nil {
		return plan, err
	}
	if err := in.Vitals.Validate(); err != nil {
		return plan, err
	}
	if err := checkAbilities("spell", in.Spells); err != nil {
		return plan, err
	}
	if err := checkAbilities("skill", in.Skills); err != nil {
		return plan, err
	}
	plan.VitalsAfter = in.Vitals
	plan.ScaledCost = in.PendingCost
	plan.ClearPending = true

	// 1. Frenzy at exit: skip everything, keep the haunted clear.
	if in.FrenzyActive {
		plan.ClearHaunted = true
		return plan, nil
	}

	// 2. Full cost: justice clears.
	if in.PendingCost >= in.DefaultCost {
		plan.ClearOutlaw = true
		plan.ReevaluatePK = true
		plan.ClearHaunted = true
	}

	// 3. Cost scaling or the HP-loss roll.
	if in.PendingCost > 0 {
		if in.StillNewbie && !in.Murderer {
			plan.ScaledCost = in.PendingCost / 3
			plan.CostScaled = true
		} else {
			roll, err := RollD100(rng)
			if err != nil {
				return DeathPenaltyPlan{}, err
			}
			plan.HPRolled = true
			plan.HPRoll = roll
			if roll <= in.PendingCost { // roll == cost LOSES (strictness frozen)
				v2, delta, err := AdjustBaseMaxHP(in.Vitals, -1, in.Stamina)
				if err != nil {
					return DeathPenaltyPlan{}, err
				}
				v3, _, err := AdjustMaxHP(v2, delta)
				if err != nil {
					return DeathPenaltyPlan{}, err
				}
				plan.VitalsAfter = v3
				// delta is the SIGNED actual change from the T4a
				// helper (-1, or 0 at the floor); report magnitude.
				plan.LostBaseMaxHP = -delta
			}
		}
	}

	// 4. Guild-quit check on the post-penalty base max HP.
	plan.QuitGuild = plan.VitalsAfter.BaseMaxHP < pkillEnableHP

	// 5. Loss severity (murderer flag read after the step-2 clears).
	loss := ordinaryAbilityLoss
	if in.Murderer {
		loss = murdererAbilityLoss
	}

	// 6. Ability losses: spells then skills, input order.
	plan.AbilityLosses = make([]DeathAbilityLoss, 0, len(in.Spells)+len(in.Skills))
	for _, list := range [][]DeathAbilityInput{in.Spells, in.Skills} {
		for _, a := range list {
			if a.Ability <= deathAbilityEligibilityAbove {
				continue // ineligible: NO roll consumed
			}
			staminaRoll, err := RollD100(rng)
			if err != nil {
				return DeathPenaltyPlan{}, err
			}
			if staminaRoll <= in.Stamina { // roll == Stamina SAVES
				continue // saved: NO cost roll consumed
			}
			costRoll, err := RollD100(rng)
			if err != nil {
				return DeathPenaltyPlan{}, err
			}
			if costRoll < plan.ScaledCost { // roll == cost SAVES
				newAbility := boundInt64(int64(a.Ability)+int64(loss), deathAbilityFloor, deathAbilityCeil)
				plan.AbilityLosses = append(plan.AbilityLosses, DeathAbilityLoss{
					Key:         a.Key,
					FromAbility: a.Ability,
					ToAbility:   int(newAbility),
					Loss:        a.Ability - int(newAbility),
					StaminaRoll: staminaRoll,
					CostRoll:    costRoll,
				})
			}
		}
	}
	return plan, nil
}

// KarmaPrizeKind classifies the immediate karma booby prize of a
// normal death (spec §9.5.14; content itself is deferred to M9).
type KarmaPrizeKind int8

const (
	// KarmaPrizeNone: karma < 0.
	KarmaPrizeNone KarmaPrizeKind = iota
	// KarmaPrizeHammer: karma > 5000 (hundredths) and home is not the
	// newbie start.
	KarmaPrizeHammer
	// KarmaPrizeMace: karma >= 0 without the hammer condition.
	KarmaPrizeMace
)

// PlanKarmaBoobyPrize freezes the source booby-prize selection
// (spec §9.5.14): karma > 5000 AND home is not the resolved newbie
// start -> hammer; else karma >= 0 -> mace; else none. karma is the
// stored hundredths value (-10000..10000). homeIsNewbieStart is a
// resolved boolean, never a room ID. The prize items are M9 content;
// only the classification is frozen here.
func PlanKarmaBoobyPrize(karma int32, homeIsNewbieStart bool) (KarmaPrizeKind, error) {
	if karma < minKarmaHundredths || karma > maxKarmaHundredths {
		return KarmaPrizeNone, fmt.Errorf("sim: karma %d: %w", karma, ErrInvalidDeathInput)
	}
	if karma > hammerKarmaThreshold && !homeIsNewbieStart {
		return KarmaPrizeHammer, nil
	}
	if karma >= karmaPrizeFloor {
		return KarmaPrizeMace, nil
	}
	return KarmaPrizeNone, nil
}
