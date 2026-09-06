// Package persist composes the revision-safe sim Saver with the
// existing Store CAS operations (spec §8.3.11–8.3.13). Dependency
// direction is binding: sim and store never import each other or
// this package; this package imports sim + store but NO
// pgx/generated sqlc directly. Tests may use pgx/raw SQL for
// fixtures; production code here may not.
package persist

import (
	"context"
	"errors"
	"fmt"

	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/store"
)

// SnapshotStore is the narrow Store write seam T4b composes: the
// three existing §8.1 CAS operations, nothing more.
type SnapshotStore interface {
	SaveCharacterSnapshot(context.Context, store.CharacterSnapshot) (int64, error)
	SaveItemSnapshot(context.Context, store.ItemSnapshot) (int64, error)
	SaveBankBalance(context.Context, store.BankSnapshot) (int64, error)
}

// Compile-time proof that the production PGStore satisfies the
// narrow seam (no wrapper, no replacement persistence method).
var _ SnapshotStore = (*store.PGStore)(nil)

// BankLoader is the narrow materialized bank reload seam over the
// existing store.LoadBankBalance.
type BankLoader interface {
	LoadBankBalance(context.Context, int64, string) (store.BankSnapshot, error)
}

// Compile-time proof for the reload seam.
var _ BankLoader = (*store.PGStore)(nil)

// SnapshotJob pairs a saver key with its write closure so callers
// cannot mismatch a snapshot with the wrong AggregateKey.
type SnapshotJob struct {
	Key   sim.AggregateKey
	Write sim.SnapshotWrite
}

// mapStale translates the Store stale sentinel into the
// saver-domain stale sentinel while preserving the Store cause
// (spec §8.3.12). Non-stale errors pass through untouched.
func mapStale(op string, err error) error {
	if errors.Is(err, store.ErrStaleRevision) {
		return fmt.Errorf("persist: %s: %w: %w", op, sim.ErrSnapshotStale, err)
	}
	return err
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	return append([]byte(nil), b...)
}

// NewCharacterSnapshotJob deep-copies a complete character snapshot
// and binds it to its saver key. The input ExpectedRevision is
// ignored (the saver owns revisions): at execution the closure
// sets ExpectedRevision from the saver's authoritative value
// immediately before invoking Store.
func NewCharacterSnapshotJob(st SnapshotStore, snap store.CharacterSnapshot) (SnapshotJob, error) {
	if snap.ID <= 0 {
		return SnapshotJob{}, fmt.Errorf("%w: character snapshot id %d",
			sim.ErrInvalidAggregateKey, snap.ID)
	}
	frozen := snap
	frozen.Vitals = cloneBytes(snap.Vitals)
	frozen.Advancement = cloneBytes(snap.Advancement)
	frozen.Spells = append([]store.CharacterSpellSnapshot(nil), snap.Spells...)
	frozen.Skills = append([]store.CharacterSkillSnapshot(nil), snap.Skills...)
	// Preserve nil-vs-empty for child sets: nil and empty both
	// mean "resulting child set is empty" downstream, but the
	// captured shape must not alias the caller's slice header.
	if snap.Spells == nil {
		frozen.Spells = nil
	}
	if snap.Skills == nil {
		frozen.Skills = nil
	}
	return SnapshotJob{
		Key: sim.AggregateKey{Kind: sim.AggregateCharacter, ID: snap.ID},
		Write: func(ctx context.Context, expectedRevision int64) (int64, error) {
			req := frozen
			req.ExpectedRevision = expectedRevision
			rev, err := st.SaveCharacterSnapshot(ctx, req)
			if err != nil {
				return 0, mapStale("save character snapshot", err)
			}
			return rev, nil
		},
	}, nil
}

func cloneInt64Ptr(p *int64) *int64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func cloneStringPtr(p *string) *string {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// NewItemSnapshotJob deep-copies a complete item snapshot (enchants
// bytes plus every location pointer VALUE into fresh storage) and
// binds it to its saver key. Input ExpectedRevision is ignored.
func NewItemSnapshotJob(st SnapshotStore, snap store.ItemSnapshot) (SnapshotJob, error) {
	if snap.ID <= 0 {
		return SnapshotJob{}, fmt.Errorf("%w: item snapshot id %d",
			sim.ErrInvalidAggregateKey, snap.ID)
	}
	frozen := snap
	frozen.Enchants = cloneBytes(snap.Enchants)
	loc := snap.Location
	loc.CharacterID = cloneInt64Ptr(loc.CharacterID)
	loc.CorpseID = cloneInt64Ptr(loc.CorpseID)
	loc.ContainerItemID = cloneInt64Ptr(loc.ContainerItemID)
	loc.VaultRegion = cloneStringPtr(loc.VaultRegion)
	loc.PosX = cloneInt64Ptr(loc.PosX)
	loc.PosY = cloneInt64Ptr(loc.PosY)
	loc.PosZ = cloneInt64Ptr(loc.PosZ)
	loc.Slot = cloneStringPtr(loc.Slot)
	frozen.Location = loc
	return SnapshotJob{
		Key: sim.AggregateKey{Kind: sim.AggregateItem, ID: snap.ID},
		Write: func(ctx context.Context, expectedRevision int64) (int64, error) {
			req := frozen
			req.ExpectedRevision = expectedRevision
			rev, err := st.SaveItemSnapshot(ctx, req)
			if err != nil {
				return 0, mapStale("save item snapshot", err)
			}
			return rev, nil
		},
	}, nil
}

// NewBankSnapshotJob copies a bank snapshot by value and binds it
// to its {bank, characterID, system} saver key. Input
// ExpectedRevision is ignored.
func NewBankSnapshotJob(st SnapshotStore, snap store.BankSnapshot) (SnapshotJob, error) {
	if snap.CharacterID <= 0 {
		return SnapshotJob{}, fmt.Errorf("%w: bank snapshot character id %d",
			sim.ErrInvalidAggregateKey, snap.CharacterID)
	}
	if snap.System == "" {
		return SnapshotJob{}, fmt.Errorf("%w: bank snapshot empty system",
			sim.ErrInvalidAggregateKey)
	}
	frozen := snap
	return SnapshotJob{
		Key: sim.AggregateKey{Kind: sim.AggregateBank, ID: snap.CharacterID, Scope: snap.System},
		Write: func(ctx context.Context, expectedRevision int64) (int64, error) {
			req := frozen
			req.ExpectedRevision = expectedRevision
			rev, err := st.SaveBankBalance(ctx, req)
			if err != nil {
				return 0, mapStale("save bank balance", err)
			}
			return rev, nil
		},
	}, nil
}
