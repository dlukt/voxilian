package persist

import (
	"fmt"

	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/store"
)

// Pending-death recovery/hydration mapping (spec §9.5.1k,
// M5-T5c3d1): the ONE narrow reusable translation of a
// materialized `store.PendingDeathSnapshot` into the
// store-independent `sim.PendingDeathRuntime` owner value.
// It does no PG I/O, no owner mutation, and no mechanics.
// The c3c3b lost-ack completion construction uses it;
// T5c4 reconnect/hydration may reuse it later.

// MapPendingDeathRecovery maps one materialized
// pending-death row onto the sim-domain owner value
// (spec §9.5.1k). A nil row maps to (nil, nil):
// authoritative "no pending death". A non-nil row
// requires CharacterID == expectedCharacterID and the d1
// value domain (cost 0..100, death time >= 0, non-nil
// CorpseID pointing at an ID > 0); the returned value
// owns an independent deep copy of the optional CorpseID
// pointer, so later caller mutation of the Store snapshot
// cannot reach sim state. Every error returns (nil,
// err).
func MapPendingDeathRecovery(
	expectedCharacterID sim.CharacterID,
	pending *store.PendingDeathSnapshot,
) (*sim.PendingDeathRuntime, error) {
	if pending == nil {
		return nil, nil
	}
	if pending.CharacterID != int64(expectedCharacterID) {
		return nil, fmt.Errorf("persist: pending death character %d, want %d: %w",
			pending.CharacterID, int64(expectedCharacterID), sim.ErrInvalidDeathInput)
	}
	out := &sim.PendingDeathRuntime{
		EffectiveCost:    int(pending.EffectiveCost),
		DeathTimeSeconds: pending.DeathTimeSeconds,
		PortalUsed:       pending.PortalUsed,
	}
	if pending.CorpseID != nil {
		id := *pending.CorpseID
		out.CorpseID = &id
	}
	if err := sim.ValidatePendingDeathRuntime(out); err != nil {
		return nil, fmt.Errorf("persist: map pending death recovery: %w", err)
	}
	return out, nil
}
