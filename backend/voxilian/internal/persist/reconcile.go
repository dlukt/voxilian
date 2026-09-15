package persist

import (
	"context"
	"fmt"

	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/store"
)

// BankReload stages a complete materialized bank snapshot for T3c
// reconciliation (spec §8.3.13): the loader reads PG into a
// temporary store-domain snapshot and the returned candidate
// carries its persisted revision; live memory replacement happens
// ONLY inside ReloadCandidate.Apply, after reconciliation revision
// validation. No live mutation occurs during the SELECT.
func BankReload(
	loader BankLoader,
	characterID int64,
	system string,
	apply func(store.BankSnapshot) error,
) sim.ReloadFunc {
	return func(ctx context.Context) (sim.ReloadCandidate, error) {
		snap, err := loader.LoadBankBalance(ctx, characterID, system)
		if err != nil {
			return sim.ReloadCandidate{}, fmt.Errorf("persist: bank reload: %w", err)
		}
		staged := snap
		return sim.ReloadCandidate{
			Revision: staged.ExpectedRevision,
			Apply: func() error {
				return apply(staged)
			},
		}, nil
	}
}

// DeathCharacterRecoveryLoader is the narrow materialized
// character-recovery seam over the existing
// store.LoadDeathCharacterRecovery.
type DeathCharacterRecoveryLoader interface {
	LoadDeathCharacterRecovery(context.Context, int64) (store.DeathCharacterRecoverySnapshot, error)
}

// DeathItemRecoveryLoader is the narrow materialized item-recovery
// seam over the existing store.LoadDeathItemRecovery.
type DeathItemRecoveryLoader interface {
	LoadDeathItemRecovery(context.Context, int64) (store.DeathItemRecoverySnapshot, error)
}

// Compile-time proof that the production PGStore satisfies both
// death-recovery reload seams.
var (
	_ DeathCharacterRecoveryLoader = (*store.PGStore)(nil)
	_ DeathItemRecoveryLoader      = (*store.PGStore)(nil)
)

func clonePendingDeathSnapshot(p *store.PendingDeathSnapshot) *store.PendingDeathSnapshot {
	if p == nil {
		return nil
	}
	out := *p
	out.CorpseID = cloneInt64Ptr(p.CorpseID)
	return &out
}

func cloneDeathCharacterRecovery(snap store.DeathCharacterRecoverySnapshot) store.DeathCharacterRecoverySnapshot {
	return store.DeathCharacterRecoverySnapshot{
		Character: freezeCharacterSnapshot(snap.Character),
		Pending:   clonePendingDeathSnapshot(snap.Pending),
	}
}

func clonePKProtectionSnapshot(p *store.ItemPKProtectionSnapshot) *store.ItemPKProtectionSnapshot {
	if p == nil {
		return nil
	}
	out := *p
	return &out
}

func cloneDeathItemRecovery(snap store.DeathItemRecoverySnapshot) store.DeathItemRecoverySnapshot {
	return store.DeathItemRecoverySnapshot{
		Item:         freezeItemSnapshot(snap.Item),
		PKProtection: clonePKProtectionSnapshot(snap.PKProtection),
	}
}

// DeathCharacterReload stages a complete materialized character
// death-recovery snapshot for reconciliation (spec §9.5.1b): the
// loader reads PG into temporary Store-domain values and the
// returned candidate carries the persisted character revision;
// live memory replacement happens ONLY inside
// ReloadCandidate.Apply, after reconciliation revision validation.
// No live mutation occurs during the load. The staged candidate
// owns immutable copies, so a loader mutating its buffers after
// return cannot alter what Apply receives.
func DeathCharacterReload(
	loader DeathCharacterRecoveryLoader,
	characterID int64,
	apply func(store.DeathCharacterRecoverySnapshot) error,
) sim.ReloadFunc {
	return func(ctx context.Context) (sim.ReloadCandidate, error) {
		snap, err := loader.LoadDeathCharacterRecovery(ctx, characterID)
		if err != nil {
			return sim.ReloadCandidate{}, fmt.Errorf("persist: death character reload: %w", err)
		}
		staged := cloneDeathCharacterRecovery(snap)
		return sim.ReloadCandidate{
			Revision: staged.Character.ExpectedRevision,
			Apply: func() error {
				return apply(staged)
			},
		}, nil
	}
}

// DeathItemReload stages a complete materialized item
// death-recovery snapshot for reconciliation (spec §9.5.1b) with
// the same staging contract as DeathCharacterReload: temporary
// load, persisted-revision candidate, deferred complete in-memory
// replacement, immutable staged copies.
func DeathItemReload(
	loader DeathItemRecoveryLoader,
	itemID int64,
	apply func(store.DeathItemRecoverySnapshot) error,
) sim.ReloadFunc {
	return func(ctx context.Context) (sim.ReloadCandidate, error) {
		snap, err := loader.LoadDeathItemRecovery(ctx, itemID)
		if err != nil {
			return sim.ReloadCandidate{}, fmt.Errorf("persist: death item reload: %w", err)
		}
		staged := cloneDeathItemRecovery(snap)
		return sim.ReloadCandidate{
			Revision: staged.Item.ExpectedRevision,
			Apply: func() error {
				return apply(staged)
			},
		}, nil
	}
}

// ReconcileSaver is the production saver-stale recovery bridge
// (spec §8.3.13): force a T3c reload without claiming a revision,
// stage and validate a full materialized PG replacement, then
// resolve the saver's reconcile block at the loaded revision.
// Owning gameplay continues only after both layers are clear. It
// runs while the owning sim aggregate is otherwise serialized.
//
// Fail-safe ordering: a loader failure leaves the saver blocked
// and T3c pending without calling ResolveReconciled. If the T3c
// reload succeeds but ResolveReconciled fails, RequireReload is
// invoked AGAIN before returning, so the layers can never look
// jointly ready when only one of them cleared.
func ReconcileSaver(
	ctx context.Context,
	state *sim.ReconcileState,
	saver *sim.Saver,
	key sim.AggregateKey,
	reload sim.ReloadFunc,
) error {
	state.RequireReload()
	if err := state.EnsureReconciled(ctx, reload); err != nil {
		return err
	}
	known := state.Snapshot().KnownRevision
	if err := saver.ResolveReconciled(ctx, key, known); err != nil {
		state.RequireReload()
		return fmt.Errorf("persist: saver resolve after reload: %w", err)
	}
	return nil
}
