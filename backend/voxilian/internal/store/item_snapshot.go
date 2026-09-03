package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dlukt/voxilian/internal/store/gen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrContainerCycle reports a rejected containment placement: destination
// is the moving item, destination ancestry reaches it, or the ancestry is
// already cyclic.
var ErrContainerCycle = errors.New("container_cycle")

// itemContainmentLockKey is the single stable transaction-scoped advisory
// lock serializing containment-edge writes across all item roots.
const itemContainmentLockKey int64 = 1448038401

// ItemLocationSnapshot is the complete location state for an item save.
// Pointers mean SQL NULL; no pgtype leaks into this sim-facing API.
type ItemLocationSnapshot struct {
	Kind int16

	CharacterID     *int64
	CorpseID        *int64
	ContainerItemID *int64

	VaultRegion *string

	PosX *int64
	PosY *int64
	PosZ *int64

	Slot *string
}

// ItemSnapshot is a COMPLETE item aggregate snapshot: root fields plus
// the full resulting location row. No patch semantics.
type ItemSnapshot struct {
	ID               int64
	ExpectedRevision int64

	Qty      int32
	Hits     int32
	Enchants json.RawMessage

	Location ItemLocationSnapshot
}

func int8Null(v *int64) pgtype.Int8 {
	if v == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *v, Valid: true}
}

func textNull(v *string) pgtype.Text {
	if v == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *v, Valid: true}
}

// SaveItemSnapshot persists a complete item aggregate snapshot: root CAS
// FIRST, then (for container destinations) advisory serialization plus
// ancestry validation, then the complete location upsert — one READ
// COMMITTED transaction. Any failure rolls the root advance back.
func SaveItemSnapshot(ctx context.Context, pool *pgxpool.Pool, snap ItemSnapshot) (int64, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: save item snapshot: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rev, err := saveItemSnapshotTx(ctx, tx, snap)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: save item snapshot: commit: %w", err)
	}
	return rev, nil
}

// saveItemSnapshotTx is the private seam later critical operations reuse
// to compose materialized state + ledger in one Store transaction.
// pgx.Tx never escapes internal/store.
func saveItemSnapshotTx(ctx context.Context, tx pgx.Tx, snap ItemSnapshot) (int64, error) {
	q := gen.New(tx)

	newRev, err := q.CASUpdateItemSnapshot(ctx, gen.CASUpdateItemSnapshotParams{
		Qty: snap.Qty, Hits: snap.Hits, Enchants: snap.Enchants,
		ExpectedRevision: snap.ExpectedRevision, ID: snap.ID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, staleRevision("save item snapshot", snap.ID, snap.ExpectedRevision)
		}
		return 0, fmt.Errorf("store: save item snapshot: root CAS: %w", err)
	}

	loc := snap.Location
	if loc.Kind == 4 && loc.ContainerItemID != nil {
		if *loc.ContainerItemID == snap.ID {
			return 0, fmt.Errorf("store: item id=%d into container=%d: %w",
				snap.ID, *loc.ContainerItemID, ErrContainerCycle)
		}
		if err := q.LockItemContainmentGraph(ctx, itemContainmentLockKey); err != nil {
			return 0, fmt.Errorf("store: save item snapshot: containment lock: %w", err)
		}
		cycle, err := q.WouldCreateItemContainmentCycle(ctx, gen.WouldCreateItemContainmentCycleParams{
			MovingItemID: snap.ID, DestinationContainerID: *loc.ContainerItemID,
		})
		if err != nil {
			return 0, fmt.Errorf("store: save item snapshot: ancestry check: %w", err)
		}
		if cycle {
			return 0, fmt.Errorf("store: item id=%d into container=%d: %w",
				snap.ID, *loc.ContainerItemID, ErrContainerCycle)
		}
	}

	if _, err := q.UpsertItemLocation(ctx, gen.UpsertItemLocationParams{
		ItemID: snap.ID, Kind: loc.Kind,
		CharacterID: int8Null(loc.CharacterID), CorpseID: int8Null(loc.CorpseID),
		ContainerItemID: int8Null(loc.ContainerItemID), VaultRegion: textNull(loc.VaultRegion),
		PosX: int8Null(loc.PosX), PosY: int8Null(loc.PosY), PosZ: int8Null(loc.PosZ),
		Slot: textNull(loc.Slot),
	}); err != nil {
		return 0, fmt.Errorf("store: save item snapshot: location: %w", err)
	}
	return newRev, nil
}
