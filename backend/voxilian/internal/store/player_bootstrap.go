package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dlukt/voxilian/internal/store/gen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Player reconnect bootstrap recovery read (spec §9.5.1m C3,
// M5-T5c4): the dedicated T5c4 Store-domain recovery value
// containing ONLY what a fresh reconnect bootstrap
// requires. It deliberately does NOT widen the frozen
// DeathCharacterRecoverySnapshot death-reconciliation
// shape: that snapshot carries no inventory aggregates and
// no base-stat fields, and growing it for reconnect would
// couple the death-reconciliation path to world-entry
// needs. No mechanics, no Portal/penalty math, no
// mutation, no auto-creation.

// PlayerBootstrapItemSnapshot is one directly
// character-owned carried inventory item (spec §9.5.1m
// C2): the complete item root content plus its
// character-location slot label. Only kind = 0 rows for
// the reconnect character are enumerated, in ascending
// item-id order; ground, corpse, vault, and
// container-contained rows are never carried inventory
// and are unrepresentable in the frozen sim-domain
// shadow shape.
type PlayerBootstrapItemSnapshot struct {
	ID       int64
	ProtoID  int32
	Qty      int32
	Hits     int32
	Enchants json.RawMessage
	Slot     string
}

// PlayerBootstrapRecoverySnapshot is the complete
// T5c4 reconnect recovery shape: the character root
// fields the bootstrap adapter maps (position, vitals,
// karma, advancement, flags, durable base stamina and
// mysticism for runtime-input resolution), spells,
// skills, the exact C2 inventory item set, and the
// optional pending-death child. Pending nil means no
// pending_deaths row. A nil Items slice means the
// character owns no carried inventory (never a partial
// enumeration: every error returns the zero value).
type PlayerBootstrapRecoverySnapshot struct {
	CharacterID      int64
	ExpectedRevision int64

	Stamina   int16
	Mysticism int16

	Karma       int32
	PosX        int64
	PosY        int64
	PosZ        int64
	Vitals      json.RawMessage
	Advancement json.RawMessage
	Flags       int32

	Spells []CharacterSpellSnapshot
	Skills []CharacterSkillSnapshot
	Items  []PlayerBootstrapItemSnapshot

	Pending *PendingDeathSnapshot
}

// loadPlayerBootstrapRecoveryTx composes the complete
// reconnect recovery shape inside an already-begun
// recovery transaction from the existing generated
// queries plus the one C4-authorized read-only
// inventory enumeration. The query order is fixed
// (character, spells, skills, inventory, pending) and
// every failure aborts with zero staged state.
func loadPlayerBootstrapRecoveryTx(ctx context.Context, tx pgx.Tx, characterID int64) (PlayerBootstrapRecoverySnapshot, error) {
	q := gen.New(tx)
	row, err := q.GetCharacterByID(ctx, characterID)
	if err != nil {
		return PlayerBootstrapRecoverySnapshot{}, fmt.Errorf(
			"store: load player bootstrap recovery id=%d: %w", characterID, err)
	}
	// Recovery is for a live gameplay character;
	// GetCharacterByID also sees soft-deleted rows, which
	// must never resurrect into recovery. Missing-row
	// convention, never a zero snapshot.
	if row.DeletedAt.Valid {
		return PlayerBootstrapRecoverySnapshot{}, fmt.Errorf(
			"store: load player bootstrap recovery id=%d: deleted: %w", characterID, pgx.ErrNoRows)
	}
	// ListCharacterSpells/Skills already order by
	// spell_id/skill_id; ListCharacterInventoryItems orders
	// by item id: deterministic, never Go map iteration
	// order.
	spells, err := q.ListCharacterSpells(ctx, characterID)
	if err != nil {
		return PlayerBootstrapRecoverySnapshot{}, fmt.Errorf(
			"store: load player bootstrap recovery id=%d: spells: %w", characterID, err)
	}
	skills, err := q.ListCharacterSkills(ctx, characterID)
	if err != nil {
		return PlayerBootstrapRecoverySnapshot{}, fmt.Errorf(
			"store: load player bootstrap recovery id=%d: skills: %w", characterID, err)
	}
	itemRows, err := q.ListCharacterInventoryItems(ctx, pgtype.Int8{Int64: characterID, Valid: true})
	if err != nil {
		return PlayerBootstrapRecoverySnapshot{}, fmt.Errorf(
			"store: load player bootstrap recovery id=%d: inventory: %w", characterID, err)
	}
	pendingRow, err := q.GetPendingDeathByCharacter(ctx, characterID)
	var pending *PendingDeathSnapshot
	switch {
	case err == nil:
		pending = &PendingDeathSnapshot{
			CharacterID:      pendingRow.CharacterID,
			EffectiveCost:    pendingRow.EffectiveCost,
			DeathTimeSeconds: pendingRow.DeathTimeSeconds,
			CorpseID:         int8Value(pendingRow.CorpseID),
			PortalUsed:       pendingRow.PortalUsed,
		}
	case errors.Is(err, pgx.ErrNoRows):
		pending = nil
	default:
		return PlayerBootstrapRecoverySnapshot{}, fmt.Errorf(
			"store: load player bootstrap recovery id=%d: pending death: %w", characterID, err)
	}
	charSpells := make([]CharacterSpellSnapshot, 0, len(spells))
	for _, sp := range spells {
		charSpells = append(charSpells, CharacterSpellSnapshot{
			SpellID: sp.SpellID, Ability: sp.Ability, AtrophyFlag: sp.AtrophyFlag,
		})
	}
	charSkills := make([]CharacterSkillSnapshot, 0, len(skills))
	for _, sk := range skills {
		charSkills = append(charSkills, CharacterSkillSnapshot{
			SkillID: sk.SkillID, Ability: sk.Ability, AtrophyFlag: sk.AtrophyFlag,
		})
	}
	items := make([]PlayerBootstrapItemSnapshot, 0, len(itemRows))
	for _, it := range itemRows {
		// kind = 0 rows carry a non-null slot by the
		// item_locations CHECK; a null here is a corrupt
		// aggregate, never an invented empty label.
		if !it.Slot.Valid {
			return PlayerBootstrapRecoverySnapshot{}, fmt.Errorf(
				"store: load player bootstrap recovery id=%d: item id=%d missing slot: %w",
				characterID, it.ID, pgx.ErrNoRows)
		}
		items = append(items, PlayerBootstrapItemSnapshot{
			ID:       it.ID,
			ProtoID:  it.Proto,
			Qty:      it.Qty,
			Hits:     it.Hits,
			Enchants: append([]byte(nil), it.Enchants...),
			Slot:     it.Slot.String,
		})
	}
	return PlayerBootstrapRecoverySnapshot{
		CharacterID:      row.ID,
		ExpectedRevision: row.Revision,
		Stamina:          row.Stamina,
		Mysticism:        row.Mysticism,
		Karma:            row.Karma,
		PosX:             row.PosX,
		PosY:             row.PosY,
		PosZ:             row.PosZ,
		Vitals:           append([]byte(nil), row.Vitals...),
		Advancement:      append([]byte(nil), row.Advancement...),
		Flags:            row.Flags,
		Spells:           charSpells,
		Skills:           charSkills,
		Items:            items,
		Pending:          pending,
	}, nil
}

// LoadPlayerBootstrapRecovery reads one live character's
// complete T5c4 reconnect recovery shape in ONE
// REPEATABLE READ / READ ONLY transaction (spec §9.5.1m
// C3): character root, base stats for runtime-input
// resolution, spells, skills, the exact carried
// inventory set, and the optional pending-death child.
// Read-only: never a stale-metric increment, never a
// write. A failed read-only commit returns zero result
// plus the error, never staged state.
func (s *PGStore) LoadPlayerBootstrapRecovery(ctx context.Context, characterID int64) (PlayerBootstrapRecoverySnapshot, error) {
	tx, err := beginDeathRecoveryTx(ctx, s.pool)
	if err != nil {
		return PlayerBootstrapRecoverySnapshot{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	res, err := loadPlayerBootstrapRecoveryTx(ctx, tx, characterID)
	if err != nil {
		return PlayerBootstrapRecoverySnapshot{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PlayerBootstrapRecoverySnapshot{}, fmt.Errorf(
			"store: load player bootstrap recovery id=%d: commit: %w", characterID, err)
	}
	return res, nil
}
