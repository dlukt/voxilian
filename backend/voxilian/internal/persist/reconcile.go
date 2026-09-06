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
