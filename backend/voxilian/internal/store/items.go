package store

import (
	"context"
	"fmt"

	"github.com/dlukt/voxilian/internal/store/gen"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewItemLocation describes the initial location row for a brand-new
// item. Nullable columns use pgx nullable types; unset means SQL NULL.
type NewItemLocation struct {
	Kind            int16
	CharacterID     pgtype.Int8
	CorpseID        pgtype.Int8
	ContainerItemID pgtype.Int8
	VaultRegion     pgtype.Text
	PosX            pgtype.Int8
	PosY            pgtype.Int8
	PosZ            pgtype.Int8
	Slot            pgtype.Text
}

// createItemWithLocation atomically creates a NEW item aggregate root
// plus its initial location row. Safe without CAS because the root is
// brand-new at revision 0 with no previous concurrent owner.
//
// Moving an EXISTING item must go through the M1-T7b root-CAS
// transaction instead; no such helper exists here by design.
func createItemWithLocation(
	ctx context.Context,
	pool *pgxpool.Pool,
	item gen.InsertItemInstanceParams,
	loc NewItemLocation,
) (gen.ItemInstance, gen.ItemLocation, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return gen.ItemInstance{}, gen.ItemLocation{}, fmt.Errorf("store: create item: begin: %w", err)
	}
	// Rollback is a no-op after successful commit.
	defer func() { _ = tx.Rollback(ctx) }()

	q := gen.New(tx)
	root, err := q.InsertItemInstance(ctx, item)
	if err != nil {
		return gen.ItemInstance{}, gen.ItemLocation{}, fmt.Errorf("store: create item: root: %w", err)
	}
	placed, err := q.UpsertItemLocation(ctx, gen.UpsertItemLocationParams{
		ItemID:          root.ID,
		Kind:            loc.Kind,
		CharacterID:     loc.CharacterID,
		CorpseID:        loc.CorpseID,
		ContainerItemID: loc.ContainerItemID,
		VaultRegion:     loc.VaultRegion,
		PosX:            loc.PosX,
		PosY:            loc.PosY,
		PosZ:            loc.PosZ,
		Slot:            loc.Slot,
	})
	if err != nil {
		return gen.ItemInstance{}, gen.ItemLocation{}, fmt.Errorf("store: create item: location: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return gen.ItemInstance{}, gen.ItemLocation{}, fmt.Errorf("store: create item: commit: %w", err)
	}
	return root, placed, nil
}
