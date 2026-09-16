package persist

import (
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/store"
)

// Complete immediate-death Store-domain mapping (spec §9.5.1g,
// M5-T5c3c2): the mechanical translation of an already-resolved
// sim-domain ImmediateDeathCapture into the existing
// store.DeathEntryRequest. This layer does NOT call Store, does
// NOT call Saver, does NOT perform recovery, and recalculates NO
// gameplay mechanics: the sim builder owns all death resolution.
// Every mapper error returns the ZERO request. The mapper owns
// its output bytes/slices independently of the sim capture:
// mutating the capture after mapping cannot mutate the Store
// request (the existing T5c2b freezer remains the final defensive
// freeze before callback execution).

// MetersToStoreMillimeters freezes the ONE mechanical position
// persistence conversion (spec §9.5.1g): millimeters =
// math.Round(meters * 1000) per axis, result type int64. Analogous
// to the existing wire rounding rule but with an int64 Store
// domain. NaN, +Inf, -Inf, and rounded/scaled values outside
// signed int64 are rejected. No truncation, no clamp, no wrap.
// The SAME helper converts the captured death position, the
// post-death Character position, and every affected ground item
// position.
func MetersToStoreMillimeters(meters float64) (int64, error) {
	if math.IsNaN(meters) || math.IsInf(meters, 0) {
		return 0, fmt.Errorf("persist: death capture position %v: %w", meters, sim.ErrInvalidDeathInput)
	}
	scaled := meters * 1000
	rounded := math.Round(scaled)
	// Exact binary64 boundary: 2^63 is representable while
	// float64(math.MaxInt64) rounds up to 2^63, so the upper
	// bound must not use float64(math.MaxInt64). +2^63 is
	// outside int64 (reject); -2^63 is math.MinInt64 (accept).
	limit := math.Ldexp(1, 63)
	if math.IsNaN(rounded) || math.IsInf(rounded, 0) ||
		rounded >= limit || rounded < -limit {
		return 0, fmt.Errorf("persist: death capture position %v out of int64 millimeters: %w", meters, sim.ErrInvalidDeathInput)
	}
	return int64(rounded), nil
}

// storePosition converts one world position to its three Store
// millimeter coordinates through the single frozen helper.
func storePosition(x, y, z float64) (int64, int64, int64, error) {
	px, err := MetersToStoreMillimeters(x)
	if err != nil {
		return 0, 0, 0, err
	}
	py, err := MetersToStoreMillimeters(y)
	if err != nil {
		return 0, 0, 0, err
	}
	pz, err := MetersToStoreMillimeters(z)
	if err != nil {
		return 0, 0, 0, err
	}
	return px, py, pz, nil
}

// MapImmediateDeathCapture maps an already-resolved sim capture to
// the existing store.DeathEntryRequest (spec §9.5.1g). Character
// mapping: ID = token CharacterID, ExpectedRevision = 0
// placeholder (T5c2b remains the ONLY layer injecting the
// execution-time Saver revision), Karma/Flags = resulting durable
// state, position = resolved post-death placement in int64 mm,
// Vitals = JSON encoding of resolved post-death PlayerVitals,
// Advancement = deep copy of already-resolved advancement bytes,
// complete Spells/Skills. Items mapping: one Store DeathEntryItem
// per affected item with ExpectedRevision = 0 placeholder,
// ID/Qty/Hits/Enchants = complete affected root content,
// Location kind=1 ground at the captured death position in mm
// with every ownership/container/vault/slot reference nil, and
// PKProtectionDuration from the resolved item mutation (never a
// generated CorpseID location; ProtoID is never mapped into the
// mutable ItemSnapshot). Request metadata: DeathPosXYZ = captured
// death position in mm, EffectiveDeathCost = validated entry cost,
// DeathTimeSeconds = corpse/death plan time, CorpseLifetime =
// source corpse lifetime, NewbieHomeRespawn = the disposition's
// resolved newbie-home fact, Killer = mechanical sim -> Store
// translation. Every mapper error returns the zero request.
func MapImmediateDeathCapture(capture sim.ImmediateDeathCapture) (store.DeathEntryRequest, error) {
	var zero store.DeathEntryRequest
	fail := func(format string, args ...any) (store.DeathEntryRequest, error) {
		return zero, fmt.Errorf("persist: map death capture "+format+": %w",
			append(args, sim.ErrInvalidDeathInput)...)
	}
	charID := int64(capture.Token.CharacterID)
	if charID <= 0 {
		return fail("character id=%d", charID)
	}
	if err := capture.Vitals.Validate(); err != nil {
		return zero, fmt.Errorf("persist: map death capture vitals: %w", err)
	}
	deathX, deathY, deathZ, err := storePosition(
		capture.DeathPosition.X, capture.DeathPosition.Y, capture.DeathPosition.Z)
	if err != nil {
		return zero, fmt.Errorf("persist: map death capture death position: %w", err)
	}
	postX, postY, postZ, err := storePosition(
		capture.Placement.X, capture.Placement.Y, capture.Placement.Z)
	if err != nil {
		return zero, fmt.Errorf("persist: map death capture placement: %w", err)
	}
	if len(capture.Durable.Advancement) == 0 || !json.Valid(capture.Durable.Advancement) {
		return fail("advancement is not valid JSON")
	}
	cost := capture.Disposition.DeathCost
	if cost < 0 || cost > 100 {
		return fail("effective cost=%d", cost)
	}
	if capture.Disposition.NewbieHomeRespawn && cost != 0 {
		return fail("newbie-home respawn with effective cost=%d, want 0", cost)
	}
	if capture.Corpse.DeathTimeSeconds < 0 {
		return fail("death time=%d", capture.Corpse.DeathTimeSeconds)
	}
	if capture.Corpse.LifetimeMs != sim.PlayerCorpseDecomposeMs {
		return fail("corpse lifetime=%d", capture.Corpse.LifetimeMs)
	}

	vitalsJSON, err := json.Marshal(capture.Vitals)
	if err != nil {
		return zero, fmt.Errorf("persist: map death capture vitals encode: %w", err)
	}
	spells := make([]store.CharacterSpellSnapshot, 0, len(capture.Durable.Spells))
	for _, sp := range capture.Durable.Spells {
		spells = append(spells, store.CharacterSpellSnapshot{
			SpellID: sp.ID, Ability: sp.Ability, AtrophyFlag: sp.AtrophyFlag,
		})
	}
	skills := make([]store.CharacterSkillSnapshot, 0, len(capture.Durable.Skills))
	for _, sk := range capture.Durable.Skills {
		skills = append(skills, store.CharacterSkillSnapshot{
			SkillID: sk.ID, Ability: sk.Ability, AtrophyFlag: sk.AtrophyFlag,
		})
	}

	items := make([]store.DeathEntryItem, 0, len(capture.AffectedItems))
	for _, a := range capture.AffectedItems {
		if a.ID <= 0 {
			return fail("item id=%d", a.ID)
		}
		if len(a.Enchants) == 0 || !json.Valid(a.Enchants) {
			return fail("item id=%d enchants is not valid JSON", a.ID)
		}
		if a.PKProtectionDurationMs < 0 {
			return fail("item id=%d negative PK protection=%dms", a.ID, a.PKProtectionDurationMs)
		}
		const maxMillis = int64(math.MaxInt64) / int64(time.Millisecond)
		if a.PKProtectionDurationMs > maxMillis {
			return fail("item id=%d PK protection=%dms overflows", a.ID, a.PKProtectionDurationMs)
		}
		dx, dy, dz := deathX, deathY, deathZ
		items = append(items, store.DeathEntryItem{
			Snapshot: store.ItemSnapshot{
				ID:               a.ID,
				ExpectedRevision: 0,
				Qty:              a.Qty,
				Hits:             a.Hits,
				Enchants:         append([]byte(nil), a.Enchants...),
				Location: store.ItemLocationSnapshot{
					Kind: 1,
					PosX: &dx, PosY: &dy, PosZ: &dz,
				},
			},
			PKProtectionDuration: time.Duration(a.PKProtectionDurationMs) * time.Millisecond,
		})
	}

	var killer *store.DeathEntryKiller
	switch capture.Killer.Kind {
	case sim.DeathKillerNone:
		if capture.Killer.CharacterID != sim.InvalidCharacterID || capture.Killer.MobID != 0 {
			return fail("killer none with identity")
		}
		killer = nil
	case sim.DeathKillerCharacter:
		if capture.Killer.CharacterID <= sim.InvalidCharacterID || capture.Killer.MobID != 0 {
			return fail("character killer identity character=%d mob=%d",
				int64(capture.Killer.CharacterID), capture.Killer.MobID)
		}
		killer = &store.DeathEntryKiller{
			Kind:        store.DeathEntryKillerCharacter,
			CharacterID: int64(capture.Killer.CharacterID),
		}
	case sim.DeathKillerMob:
		if capture.Killer.MobID <= 0 || capture.Killer.CharacterID != sim.InvalidCharacterID {
			return fail("mob killer identity character=%d mob=%d",
				int64(capture.Killer.CharacterID), capture.Killer.MobID)
		}
		killer = &store.DeathEntryKiller{
			Kind:  store.DeathEntryKillerMob,
			MobID: capture.Killer.MobID,
		}
	default:
		return fail("unknown killer kind=%d", uint8(capture.Killer.Kind))
	}

	return store.DeathEntryRequest{
		Character: store.CharacterSnapshot{
			ID:               charID,
			ExpectedRevision: 0,
			Karma:            capture.Durable.Karma,
			PosX:             postX,
			PosY:             postY,
			PosZ:             postZ,
			Vitals:           vitalsJSON,
			Advancement:      append([]byte(nil), capture.Durable.Advancement...),
			Flags:            capture.Durable.Flags,
			Spells:           spells,
			Skills:           skills,
		},
		DeathPosX:          deathX,
		DeathPosY:          deathY,
		DeathPosZ:          deathZ,
		EffectiveDeathCost: int16(cost),
		DeathTimeSeconds:   capture.Corpse.DeathTimeSeconds,
		CorpseLifetime:     time.Duration(sim.PlayerCorpseDecomposeMs) * time.Millisecond,
		NewbieHomeRespawn:  capture.Disposition.NewbieHomeRespawn,
		Items:              items,
		Killer:             killer,
	}, nil
}
