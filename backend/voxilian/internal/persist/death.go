package persist

import (
	"context"
	"fmt"

	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/store"
)

// Critical death persistence adapters (spec §9.5.1c, M5-T5c2b):
// the persist composition layer between sim.Saver.WriteCriticalSet
// and the already-frozen Store transactions
// Store.CommitDeathEntry / Store.CommitPortalOfLife /
// Store.CommitDeathPenalties. Dependency direction is binding:
// sim imports neither store nor persist, store imports neither
// sim nor persist, this package imports sim + store with NO
// pgx/generated-sqlc import. Store transactions are unchanged.
// No new migration, query, generated code, Store method, or sim
// production change.

// DeathPersistenceStore is the narrow death Store seam T5c2b
// composes: the three existing critical death transactions,
// nothing more.
type DeathPersistenceStore interface {
	CommitDeathEntry(context.Context, store.DeathEntryRequest) (store.DeathEntryResult, error)
	CommitPortalOfLife(context.Context, store.PortalOfLifeRequest) (store.PortalOfLifeResult, error)
	CommitDeathPenalties(context.Context, store.DeathPenaltiesRequest) (store.DeathPenaltiesResult, error)
}

// Compile-time proof that the production PGStore satisfies the
// narrow death seam (no wrapper, no replacement method).
var _ DeathPersistenceStore = (*store.PGStore)(nil)

// freezeDeathEntryRequest deep-freezes a complete death-entry
// request into immutable staged ownership: the character
// snapshot via the exact freezeCharacterSnapshot rule, the
// Items slice plus each item snapshot via the exact
// freezeItemSnapshot rule, and the optional Killer value. Scalar
// durations/costs/positions copy normally. ExpectedRevision
// fields are copied but MUST be overwritten from the
// execution-time Saver revisions inside the critical callback.
func freezeDeathEntryRequest(req store.DeathEntryRequest) store.DeathEntryRequest {
	frozen := req
	frozen.Character = freezeCharacterSnapshot(req.Character)
	frozen.Items = append([]store.DeathEntryItem(nil), req.Items...)
	for i, it := range req.Items {
		frozen.Items[i] = store.DeathEntryItem{
			Snapshot:             freezeItemSnapshot(it.Snapshot),
			PKProtectionDuration: it.PKProtectionDuration,
		}
	}
	if req.Killer != nil {
		killer := *req.Killer
		frozen.Killer = &killer
	}
	return frozen
}

func deathCharacterKey(characterID int64) sim.AggregateKey {
	return sim.AggregateKey{Kind: sim.AggregateCharacter, ID: characterID}
}

func deathItemKey(itemID int64) sim.AggregateKey {
	return sim.AggregateKey{Kind: sim.AggregateItem, ID: itemID}
}

// revisionByKey resolves one participant's execution-time Saver
// revision by exact key identity (never callback slice
// position).
func revisionByKey(expected []sim.AggregateRevision, key sim.AggregateKey) (int64, bool) {
	for _, e := range expected {
		if e.Key == key {
			return e.Revision, true
		}
	}
	return 0, false
}

// CommitDeathEntry executes one atomic death entry through
// sim.Saver.WriteCriticalSet: the character root plus one item
// root per frozen request Items element (zero items is a valid
// character-only set; duplicate item IDs fail before Store via
// the existing critical-set validation). Execution-time Saver
// revisions overwrite the frozen ExpectedRevision fields inside
// the callback only. On ANY WriteCriticalSet error the ZERO
// store result is returned (the visibility fence: CorpseID
// never escapes before Saver acceptance). No retry, no
// reconciliation — the caller reconciles every affected root
// via T5c2a on ErrSaverReconcileRequired.
func CommitDeathEntry(
	ctx context.Context,
	saver *sim.Saver,
	st DeathPersistenceStore,
	req store.DeathEntryRequest,
) (store.DeathEntryResult, error) {
	if saver == nil {
		return store.DeathEntryResult{}, fmt.Errorf("persist: commit death entry: nil saver")
	}
	if st == nil {
		return store.DeathEntryResult{}, fmt.Errorf("persist: commit death entry: nil store")
	}
	frozen := freezeDeathEntryRequest(req)
	charKey := deathCharacterKey(frozen.Character.ID)
	keys := make([]sim.AggregateKey, 0, 1+len(frozen.Items))
	keys = append(keys, charKey)
	for _, it := range frozen.Items {
		keys = append(keys, deathItemKey(it.Snapshot.ID))
	}
	var stored store.DeathEntryResult
	_, err := saver.WriteCriticalSet(ctx, keys, func(
		ctx context.Context, expected []sim.AggregateRevision,
	) ([]sim.AggregateRevision, error) {
		charRev, ok := revisionByKey(expected, charKey)
		if !ok {
			return nil, fmt.Errorf("persist: commit death entry: missing character revision for %v", charKey)
		}
		call := frozen
		call.Character.ExpectedRevision = charRev
		call.Items = append([]store.DeathEntryItem(nil), frozen.Items...)
		for i, it := range frozen.Items {
			itemRev, ok := revisionByKey(expected, deathItemKey(it.Snapshot.ID))
			if !ok {
				return nil, fmt.Errorf("persist: commit death entry: missing item revision for id=%d", it.Snapshot.ID)
			}
			call.Items[i].Snapshot.ExpectedRevision = itemRev
		}
		res, err := st.CommitDeathEntry(ctx, call)
		if err != nil {
			return nil, mapStale("commit death entry", err)
		}
		stored = res
		out := make([]sim.AggregateRevision, 0, len(expected))
		out = append(out, sim.AggregateRevision{Key: charKey, Revision: res.CharacterRevision})
		for _, ir := range res.ItemRevisions {
			out = append(out, sim.AggregateRevision{Key: deathItemKey(ir.ItemID), Revision: ir.Revision})
		}
		return out, nil
	})
	if err != nil {
		return store.DeathEntryResult{}, err
	}
	return stored, nil
}

// CommitPortalOfLife executes one Portal-of-Life transition
// through sim.Saver.WriteCriticalSet as a one-key critical set
// (character only) so the conservative post-callback ambiguity
// rule applies uniformly. Execution-time Saver revision
// overwrites the frozen character ExpectedRevision inside the
// callback only. On ANY WriteCriticalSet error the ZERO store
// result is returned (EffectiveCost never escapes before Saver
// acceptance). No retry, no reconciliation.
func CommitPortalOfLife(
	ctx context.Context,
	saver *sim.Saver,
	st DeathPersistenceStore,
	req store.PortalOfLifeRequest,
) (store.PortalOfLifeResult, error) {
	if saver == nil {
		return store.PortalOfLifeResult{}, fmt.Errorf("persist: commit portal of life: nil saver")
	}
	if st == nil {
		return store.PortalOfLifeResult{}, fmt.Errorf("persist: commit portal of life: nil store")
	}
	frozen := req
	frozen.Character = freezeCharacterSnapshot(req.Character)
	charKey := deathCharacterKey(frozen.Character.ID)
	var stored store.PortalOfLifeResult
	_, err := saver.WriteCriticalSet(ctx, []sim.AggregateKey{charKey}, func(
		ctx context.Context, expected []sim.AggregateRevision,
	) ([]sim.AggregateRevision, error) {
		charRev, ok := revisionByKey(expected, charKey)
		if !ok {
			return nil, fmt.Errorf("persist: commit portal of life: missing character revision for %v", charKey)
		}
		call := frozen
		call.Character.ExpectedRevision = charRev
		res, err := st.CommitPortalOfLife(ctx, call)
		if err != nil {
			return nil, mapStale("commit portal of life", err)
		}
		stored = res
		return []sim.AggregateRevision{{Key: charKey, Revision: res.CharacterRevision}}, nil
	})
	if err != nil {
		return store.PortalOfLifeResult{}, err
	}
	return stored, nil
}

// CommitDeathPenalties executes one Underworld-exit penalty
// consumption through sim.Saver.WriteCriticalSet as a one-key
// critical set (character only). Execution-time Saver revision
// overwrites the frozen character ExpectedRevision inside the
// callback only; the raw ExpectedPendingCost passes through
// unchanged (never recalculated). On ANY WriteCriticalSet error
// the ZERO store result is returned. No retry, no
// reconciliation.
func CommitDeathPenalties(
	ctx context.Context,
	saver *sim.Saver,
	st DeathPersistenceStore,
	req store.DeathPenaltiesRequest,
) (store.DeathPenaltiesResult, error) {
	if saver == nil {
		return store.DeathPenaltiesResult{}, fmt.Errorf("persist: commit death penalties: nil saver")
	}
	if st == nil {
		return store.DeathPenaltiesResult{}, fmt.Errorf("persist: commit death penalties: nil store")
	}
	frozen := req
	frozen.Character = freezeCharacterSnapshot(req.Character)
	charKey := deathCharacterKey(frozen.Character.ID)
	var stored store.DeathPenaltiesResult
	_, err := saver.WriteCriticalSet(ctx, []sim.AggregateKey{charKey}, func(
		ctx context.Context, expected []sim.AggregateRevision,
	) ([]sim.AggregateRevision, error) {
		charRev, ok := revisionByKey(expected, charKey)
		if !ok {
			return nil, fmt.Errorf("persist: commit death penalties: missing character revision for %v", charKey)
		}
		call := frozen
		call.Character.ExpectedRevision = charRev
		res, err := st.CommitDeathPenalties(ctx, call)
		if err != nil {
			return nil, mapStale("commit death penalties", err)
		}
		stored = res
		return []sim.AggregateRevision{{Key: charKey, Revision: res.CharacterRevision}}, nil
	})
	if err != nil {
		return store.DeathPenaltiesResult{}, err
	}
	return stored, nil
}
