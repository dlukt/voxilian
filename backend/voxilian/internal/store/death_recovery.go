package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dlukt/voxilian/internal/store/gen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Materialized death recovery reads (spec §9.5.1b, M5-T5c2a):
// read-only composite loaders returning the COMPLETE
// death-relevant aggregate state for reconciliation/restart after
// stale, callback error / commit ambiguity, reconnect, or crash.
// pending_deaths is CHARACTER aggregate child state and
// item_pk_protections is ITEM aggregate child state, so recovery
// of those roots MUST include those children — a bare
// CharacterSnapshot/ItemSnapshot reload is forbidden for the death
// reconciliation path. No mechanics, no Portal/penalty math, no
// mutation, no auto-creation, no stale metric, no new metric.

// PendingDeathSnapshot is the exact currently materialized
// pending-death child state. pending_deaths.created_at is
// operational only and is deliberately absent. CorpseID nil means
// the pending death survives while its corpse expired/was deleted.
type PendingDeathSnapshot struct {
	CharacterID      int64
	EffectiveCost    int16
	DeathTimeSeconds int64
	CorpseID         *int64
	PortalUsed       bool
}

// DeathCharacterRecoverySnapshot is the complete character
// death-recovery shape: the full mutable character aggregate plus
// its optional pending-death child. Pending nil means no
// pending_deaths row.
type DeathCharacterRecoverySnapshot struct {
	Character CharacterSnapshot
	Pending   *PendingDeathSnapshot
}

// ItemPKProtectionSnapshot is the exact materialized PK-protection
// child state. No killer identity is invented; expiry is reported
// verbatim, never interpreted or enforced here.
type ItemPKProtectionSnapshot struct {
	ItemID            int64
	VictimCharacterID int64
	ExpiresAt         time.Time
}

// DeathItemRecoverySnapshot is the complete item death-recovery
// shape: the full item aggregate plus its optional protection
// child. PKProtection nil means no item_pk_protections row.
type DeathItemRecoverySnapshot struct {
	Item         ItemSnapshot
	PKProtection *ItemPKProtectionSnapshot
}

func int8Value(v pgtype.Int8) *int64 {
	if !v.Valid {
		return nil
	}
	out := v.Int64
	return &out
}

func textValue(v pgtype.Text) *string {
	if !v.Valid {
		return nil
	}
	out := v.String
	return &out
}

// beginDeathRecoveryTx opens the ONE repeatable-read, read-only
// transaction each recovery read executes inside (spec §9.5.1b):
// multi-statement aggregate loads must observe one internally
// coherent committed snapshot. pgx.Tx never escapes internal/store.
func beginDeathRecoveryTx(ctx context.Context, pool *pgxpool.Pool) (pgx.Tx, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("store: death recovery: begin: %w", err)
	}
	return tx, nil
}

// loadDeathCharacterRecoveryTx composes the complete character
// recovery shape inside an already-begun recovery transaction from
// the existing generated queries only.
func loadDeathCharacterRecoveryTx(ctx context.Context, tx pgx.Tx, characterID int64) (DeathCharacterRecoverySnapshot, error) {
	q := gen.New(tx)
	row, err := q.GetCharacterByID(ctx, characterID)
	if err != nil {
		return DeathCharacterRecoverySnapshot{}, fmt.Errorf(
			"store: load death character recovery id=%d: %w", characterID, err)
	}
	// Recovery is for a live gameplay character; GetCharacterByID
	// also sees soft-deleted rows, which must never resurrect into
	// recovery. Missing-row convention, never a zero snapshot.
	if row.DeletedAt.Valid {
		return DeathCharacterRecoverySnapshot{}, fmt.Errorf(
			"store: load death character recovery id=%d: deleted: %w", characterID, pgx.ErrNoRows)
	}
	// ListCharacterSpells/Skills already order by spell_id/skill_id:
	// deterministic, never Go map iteration order.
	spells, err := q.ListCharacterSpells(ctx, characterID)
	if err != nil {
		return DeathCharacterRecoverySnapshot{}, fmt.Errorf(
			"store: load death character recovery id=%d: spells: %w", characterID, err)
	}
	skills, err := q.ListCharacterSkills(ctx, characterID)
	if err != nil {
		return DeathCharacterRecoverySnapshot{}, fmt.Errorf(
			"store: load death character recovery id=%d: skills: %w", characterID, err)
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
		return DeathCharacterRecoverySnapshot{}, fmt.Errorf(
			"store: load death character recovery id=%d: pending death: %w", characterID, err)
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
	return DeathCharacterRecoverySnapshot{
		Character: CharacterSnapshot{
			ID:               row.ID,
			ExpectedRevision: row.Revision,
			Karma:            row.Karma,
			PosX:             row.PosX,
			PosY:             row.PosY,
			PosZ:             row.PosZ,
			Vitals:           append([]byte(nil), row.Vitals...),
			Advancement:      append([]byte(nil), row.Advancement...),
			Flags:            row.Flags,
			Spells:           charSpells,
			Skills:           charSkills,
		},
		Pending: pending,
	}, nil
}

// LoadDeathCharacterRecovery reads one live character's complete
// death-recovery shape: full aggregate snapshot (whose
// ExpectedRevision IS the persisted characters.revision) plus the
// optional pending-death child. Read-only: never a stale-metric
// increment, never a write. A failed read-only commit returns zero
// result plus the error, never staged state.
func (s *PGStore) LoadDeathCharacterRecovery(ctx context.Context, characterID int64) (DeathCharacterRecoverySnapshot, error) {
	tx, err := beginDeathRecoveryTx(ctx, s.pool)
	if err != nil {
		return DeathCharacterRecoverySnapshot{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	res, err := loadDeathCharacterRecoveryTx(ctx, tx, characterID)
	if err != nil {
		return DeathCharacterRecoverySnapshot{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return DeathCharacterRecoverySnapshot{}, fmt.Errorf(
			"store: load death character recovery id=%d: commit: %w", characterID, err)
	}
	return res, nil
}

// loadDeathItemRecoveryTx composes the complete item recovery shape
// inside an already-begun recovery transaction from the existing
// generated queries only. A missing location is an
// incomplete/corrupt materialized aggregate: the load fails, and
// no Kind=0 location is fabricated.
func loadDeathItemRecoveryTx(ctx context.Context, tx pgx.Tx, itemID int64) (DeathItemRecoverySnapshot, error) {
	q := gen.New(tx)
	inst, err := q.GetItemInstanceByID(ctx, itemID)
	if err != nil {
		return DeathItemRecoverySnapshot{}, fmt.Errorf(
			"store: load death item recovery id=%d: %w", itemID, err)
	}
	loc, err := q.GetItemLocationByItemID(ctx, itemID)
	if err != nil {
		return DeathItemRecoverySnapshot{}, fmt.Errorf(
			"store: load death item recovery id=%d: location: %w", itemID, err)
	}
	protRow, err := q.GetItemPKProtection(ctx, itemID)
	var prot *ItemPKProtectionSnapshot
	switch {
	case err == nil:
		prot = &ItemPKProtectionSnapshot{
			ItemID:            protRow.ItemID,
			VictimCharacterID: protRow.VictimCharacterID,
			ExpiresAt:         protRow.ExpiresAt.Time,
		}
	case errors.Is(err, pgx.ErrNoRows):
		prot = nil
	default:
		return DeathItemRecoverySnapshot{}, fmt.Errorf(
			"store: load death item recovery id=%d: protection: %w", itemID, err)
	}
	return DeathItemRecoverySnapshot{
		Item: ItemSnapshot{
			ID:               inst.ID,
			ExpectedRevision: inst.Revision,
			Qty:              inst.Qty,
			Hits:             inst.Hits,
			Enchants:         append([]byte(nil), inst.Enchants...),
			Location: ItemLocationSnapshot{
				Kind:            loc.Kind,
				CharacterID:     int8Value(loc.CharacterID),
				CorpseID:        int8Value(loc.CorpseID),
				ContainerItemID: int8Value(loc.ContainerItemID),
				VaultRegion:     textValue(loc.VaultRegion),
				PosX:            int8Value(loc.PosX),
				PosY:            int8Value(loc.PosY),
				PosZ:            int8Value(loc.PosZ),
				Slot:            textValue(loc.Slot),
			},
		},
		PKProtection: prot,
	}, nil
}

// LoadDeathItemRecovery reads one item's complete death-recovery
// shape: full aggregate snapshot (whose ExpectedRevision IS the
// persisted item_instances.revision, expiry reported verbatim)
// plus the optional PK-protection child. Read-only: never a
// stale-metric increment, never a write. A failed read-only commit
// returns zero result plus the error, never staged state.
func (s *PGStore) LoadDeathItemRecovery(ctx context.Context, itemID int64) (DeathItemRecoverySnapshot, error) {
	tx, err := beginDeathRecoveryTx(ctx, s.pool)
	if err != nil {
		return DeathItemRecoverySnapshot{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	res, err := loadDeathItemRecoveryTx(ctx, tx, itemID)
	if err != nil {
		return DeathItemRecoverySnapshot{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return DeathItemRecoverySnapshot{}, fmt.Errorf(
			"store: load death item recovery id=%d: commit: %w", itemID, err)
	}
	return res, nil
}
