package persist

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/store"
	"github.com/dlukt/voxilian/internal/world"
)

// Reconnect player-bootstrap mapping (spec §9.5.1l L11 as
// corrected by §9.5.1m, M5-T5c4): the ONE narrow adapter
// translating a materialized bootstrap-recovery read into
// the already-resolved sim-domain bootstrap value the
// gateway world-entry path installs. It does no PG I/O
// itself beyond the injected loader, no owner mutation,
// no gameplay, no RNG, no Portal calculation, no penalty
// planning. The frozen scope is the dedicated C3 recovery
// read plus the one C4 read-only inventory enumeration:
// no migration, no write query, no character.Descriptor
// change, and gateway still never imports store/pgx/sqlc.

// PlayerBootstrapStore is the narrow loader interface the
// bootstrap adapter needs, structurally satisfied by
// *store.PGStore. It returns the complete frozen
// T5c4 reconnect recovery shape (character root, base
// stats, spells, skills, the exact carried inventory
// set, plus the optional pending-death child); a nil
// Pending means no pending_deaths row, and a nil/empty
// Items means the character owns no carried inventory.
type PlayerBootstrapStore interface {
	LoadPlayerBootstrapRecovery(context.Context, int64) (store.PlayerBootstrapRecoverySnapshot, error)
}

// boundReconnectEffectiveAttr applies the frozen §9.4b.14a
// composition effective = bound(base + mod, 1, 70) with
// the currently implemented neutral modifier (mod = 0:
// no stat-modifier, song, or content resolution system
// feeds this seam yet, so absent modifiers are
// explicitly neutral). The bound is always applied to
// the character's ACTUAL durable base stat; no fixed
// value is ever substituted for it.
func boundReconnectEffectiveAttr(base int16) int {
	v := int(base)
	if v < 1 {
		return 1
	}
	if v > 70 {
		return 70
	}
	return v
}

// LoadPlayerBootstrap maps one materialized player
// bootstrap recovery read onto the sim-domain reconnect
// bootstrap value (spec §9.5.1m C1/C2/C5). Binding:
// characterID must be a valid durable identity and match
// the recovered root; Store millimeter positions convert
// to meters (mm/1000 exactly); Vitals JSON decodes to
// sim.PlayerVitals; Karma/Advancement/Flags/Spells/Skills
// map onto the durable shadow together with the COMPLETE
// exact carried inventory item set (ID, immutable
// ProtoID, Qty, Hits, Enchants, Slot, each with
// independent copies, in the authoritative ascending
// item-id enumeration order); runtime inputs resolve
// from the real durable base stats with neutral powers
// (absent, 0) and the ordinary room multiplier (1);
// pending maps through the shared recovery mapper (nil
// stays nil: no pending row is invented, a present row
// is never discarded). Every error returns the zero
// bootstrap: a fresh entity is never built from partial
// recovery.
func LoadPlayerBootstrap(
	ctx context.Context,
	st PlayerBootstrapStore,
	characterID int64,
) (sim.PlayerRecoveryBootstrap, error) {
	if characterID <= 0 {
		return sim.PlayerRecoveryBootstrap{}, fmt.Errorf(
			"persist: player bootstrap character %d: %w", characterID, sim.ErrInvalidCharacterID)
	}
	rec, err := st.LoadPlayerBootstrapRecovery(ctx, characterID)
	if err != nil {
		return sim.PlayerRecoveryBootstrap{}, fmt.Errorf(
			"persist: player bootstrap character %d load: %w", characterID, err)
	}
	if rec.CharacterID != characterID {
		return sim.PlayerRecoveryBootstrap{}, fmt.Errorf(
			"persist: player bootstrap character %d, recovered %d: %w",
			characterID, rec.CharacterID, sim.ErrInvalidDeathInput)
	}
	pos := world.Vec3{
		X: float64(rec.PosX) / 1000,
		Y: float64(rec.PosY) / 1000,
		Z: float64(rec.PosZ) / 1000,
	}
	if _, err := world.CellForPosition(pos); err != nil {
		return sim.PlayerRecoveryBootstrap{}, fmt.Errorf(
			"persist: player bootstrap character %d position: %w", characterID, err)
	}
	var vitals sim.PlayerVitals
	if err := json.Unmarshal(rec.Vitals, &vitals); err != nil {
		return sim.PlayerRecoveryBootstrap{}, fmt.Errorf(
			"persist: player bootstrap character %d vitals: %w", characterID, err)
	}
	if err := vitals.Validate(); err != nil {
		return sim.PlayerRecoveryBootstrap{}, fmt.Errorf(
			"persist: player bootstrap character %d vitals: %w", characterID, err)
	}
	durable := sim.PlayerDurableState{
		Karma:       rec.Karma,
		Advancement: append([]byte(nil), rec.Advancement...),
		Flags:       rec.Flags,
	}
	for _, sp := range rec.Spells {
		durable.Spells = append(durable.Spells, sim.PlayerAbilityState{
			ID:          sp.SpellID,
			Ability:     sp.Ability,
			AtrophyFlag: sp.AtrophyFlag,
		})
	}
	for _, sk := range rec.Skills {
		durable.Skills = append(durable.Skills, sim.PlayerAbilityState{
			ID:          sk.SkillID,
			Ability:     sk.Ability,
			AtrophyFlag: sk.AtrophyFlag,
		})
	}
	for _, it := range rec.Items {
		durable.Items = append(durable.Items, sim.PlayerInventoryItemState{
			ID:       it.ID,
			ProtoID:  it.ProtoID,
			Qty:      it.Qty,
			Hits:     it.Hits,
			Enchants: append([]byte(nil), it.Enchants...),
			Slot:     it.Slot,
		})
	}
	inputs := sim.PlayerVitalsRuntimeInputs{
		EffectiveStamina:       boundReconnectEffectiveAttr(rec.Stamina),
		EffectiveMysticism:     boundReconnectEffectiveAttr(rec.Mysticism),
		RestRecoveryMultiplier: 1,
	}
	if err := inputs.Validate(); err != nil {
		return sim.PlayerRecoveryBootstrap{}, fmt.Errorf(
			"persist: player bootstrap reconnect inputs: %w", err)
	}
	pending, err := MapPendingDeathRecovery(sim.CharacterID(characterID), rec.Pending)
	if err != nil {
		return sim.PlayerRecoveryBootstrap{}, fmt.Errorf(
			"persist: player bootstrap character %d pending: %w", characterID, err)
	}
	return sim.PlayerRecoveryBootstrap{
		CharacterID:   sim.CharacterID(characterID),
		Position:      pos,
		Vitals:        vitals,
		RuntimeInputs: inputs,
		Durable:       durable,
		Pending:       pending,
	}, nil
}
