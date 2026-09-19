package persist

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/store"
	"github.com/dlukt/voxilian/internal/world"
)

// Reconnect player-bootstrap mapping (spec §9.5.1l L11,
// M5-T5c4): the ONE narrow adapter translating a
// materialized death-recovery read into the
// already-resolved sim-domain bootstrap value the gateway
// world-entry path installs. It does no PG I/O itself
// beyond the injected loader, no owner mutation, no
// gameplay, no RNG, no Portal calculation, no penalty
// planning. This is the explicitly frozen narrow scope
// widening of T5c4: no migration, no query, no generated
// change, no character.Descriptor change, and gateway
// still never imports store/pgx/sqlc.

// PlayerBootstrapStore is the narrow loader interface the
// bootstrap adapter needs, structurally satisfied by
// *store.PGStore. It returns the complete frozen
// death-recovery shape (character aggregate plus optional
// pending-death child); a nil Pending means no
// pending_deaths row.
type PlayerBootstrapStore interface {
	LoadDeathCharacterRecovery(context.Context, int64) (store.DeathCharacterRecoverySnapshot, error)
}

// Reconnect runtime-input resolution (spec §9.5.1l L11):
// effective attributes are EPHEMERAL resolved inputs
// (spec §9.4b.14a), never durable, so every fresh world
// entry MUST re-resolve them. T5c4 owns no stats/content
// seam and adds no query, so the adapter resolves them
// neutrally: ordinary room multiplier, no spell powers
// active, mid-domain effective attributes. Real
// content-owned resolution (M9/M10) supersedes this
// neutral choice without changing the seam shape.
const (
	reconnectEffectiveStamina   = 10
	reconnectEffectiveMysticism = 10
)

// LoadPlayerBootstrap maps one materialized character
// recovery read onto the sim-domain reconnect bootstrap
// value (spec §9.5.1l L10/L11). Binding: characterID must
// be a valid durable identity and match the recovered
// root; Store millimeter positions convert to meters
// (mm/1000 exactly); Vitals JSON decodes to
// sim.PlayerVitals; Karma/Advancement/Flags/Spells/Skills
// map onto the durable shadow (item aggregates recover
// through their own item paths, so the character
// bootstrap carries no item shadow); pending maps through
// the shared recovery mapper (nil stays nil: no pending
// row is invented, a present row is never discarded).
// Every error returns the zero bootstrap: a fresh entity
// is never built from partial recovery.
func LoadPlayerBootstrap(
	ctx context.Context,
	st PlayerBootstrapStore,
	characterID int64,
) (sim.PlayerRecoveryBootstrap, error) {
	if characterID <= 0 {
		return sim.PlayerRecoveryBootstrap{}, fmt.Errorf(
			"persist: player bootstrap character %d: %w", characterID, sim.ErrInvalidCharacterID)
	}
	rec, err := st.LoadDeathCharacterRecovery(ctx, characterID)
	if err != nil {
		return sim.PlayerRecoveryBootstrap{}, fmt.Errorf(
			"persist: player bootstrap character %d load: %w", characterID, err)
	}
	if rec.Character.ID != characterID {
		return sim.PlayerRecoveryBootstrap{}, fmt.Errorf(
			"persist: player bootstrap character %d, recovered %d: %w",
			characterID, rec.Character.ID, sim.ErrInvalidDeathInput)
	}
	pos := world.Vec3{
		X: float64(rec.Character.PosX) / 1000,
		Y: float64(rec.Character.PosY) / 1000,
		Z: float64(rec.Character.PosZ) / 1000,
	}
	if _, err := world.CellForPosition(pos); err != nil {
		return sim.PlayerRecoveryBootstrap{}, fmt.Errorf(
			"persist: player bootstrap character %d position: %w", characterID, err)
	}
	var vitals sim.PlayerVitals
	if err := json.Unmarshal(rec.Character.Vitals, &vitals); err != nil {
		return sim.PlayerRecoveryBootstrap{}, fmt.Errorf(
			"persist: player bootstrap character %d vitals: %w", characterID, err)
	}
	if err := vitals.Validate(); err != nil {
		return sim.PlayerRecoveryBootstrap{}, fmt.Errorf(
			"persist: player bootstrap character %d vitals: %w", characterID, err)
	}
	durable := sim.PlayerDurableState{
		Karma:       rec.Character.Karma,
		Advancement: append([]byte(nil), rec.Character.Advancement...),
		Flags:       rec.Character.Flags,
	}
	for _, sp := range rec.Character.Spells {
		durable.Spells = append(durable.Spells, sim.PlayerAbilityState{
			ID:          sp.SpellID,
			Ability:     sp.Ability,
			AtrophyFlag: sp.AtrophyFlag,
		})
	}
	for _, sk := range rec.Character.Skills {
		durable.Skills = append(durable.Skills, sim.PlayerAbilityState{
			ID:          sk.SkillID,
			Ability:     sk.Ability,
			AtrophyFlag: sk.AtrophyFlag,
		})
	}
	inputs := sim.PlayerVitalsRuntimeInputs{
		EffectiveStamina:       reconnectEffectiveStamina,
		EffectiveMysticism:     reconnectEffectiveMysticism,
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
