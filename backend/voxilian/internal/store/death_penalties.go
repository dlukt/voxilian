package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/dlukt/voxilian/internal/store/gen"
	"github.com/jackc/pgx/v5"
)

// M5-T5b2b exactly-once Underworld-exit penalty consumption (spec
// §9.5.11a, §8.1/§8.3). The Store operation takes a COMPLETE
// already-resolved post-penalty CharacterSnapshot plus the raw durable
// pending cost (ExpectedPendingCost = DeathPenaltyInput.PendingCost,
// NOT DeathPenaltyPlan.ScaledCost) and executes ONE transaction:
// validate, Begin, saveCharacterSnapshotTx exactly ONCE (character
// root CAS FIRST internally), GetPendingDeathByCharacter, raw-cost
// verification, DeletePendingDeathByCharacter, commit once. Success
// ALWAYS deletes the pending row. ZERO ledger rows, ZERO kills rows.
// Store runs no T5a mechanics and imports no internal/sim.

// ErrInvalidDeathPenalties marks a malformed CommitDeathPenalties
// request rejected before Begin. Matching MUST use errors.Is, never
// string parsing.
var ErrInvalidDeathPenalties = errors.New("invalid death penalties")

// ErrPendingDeathCostMismatch reports a penalty commit whose pending
// row exists but whose durable effective cost differs from the raw
// expected pending cost used for T5a planning. The tentative
// character snapshot rolls back; no stale metric is incremented.
var ErrPendingDeathCostMismatch = errors.New("pending death cost mismatch")

// DeathPenaltiesRequest is the complete already-resolved
// Underworld-exit input. Character is the COMPLETE post-penalty
// character aggregate (post-penalty vitals, resulting flags, complete
// spells/skills, position/karma/advancement as authoritative after
// phase-2 planning); Store derives none of these. ExpectedPendingCost
// is the raw durable effective cost used as
// DeathPenaltyInput.PendingCost — NOT DeathPenaltyPlan.ScaledCost.
type DeathPenaltiesRequest struct {
	Character CharacterSnapshot

	// ExpectedPendingCost is the raw durable effective cost (0..100).
	ExpectedPendingCost int16
}

// DeathPenaltiesResult carries only durable information known after a
// successful commit. On ANY error the result is zero: a commit error
// is ambiguous — the caller must reconcile/retry per §8.3, never
// assume rollback or success, and never locally advance revision.
type DeathPenaltiesResult struct {
	CharacterRevision int64
}

// validateDeathPenaltiesRequest rejects malformed requests before any
// PG mutation. It never touches the database.
func validateDeathPenaltiesRequest(req DeathPenaltiesRequest) error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("store: commit death penalties "+format+": %w",
			append(args, ErrInvalidDeathPenalties)...)
	}
	if req.Character.ID <= 0 {
		return invalid("character id=%d", req.Character.ID)
	}
	if req.Character.ExpectedRevision < 0 {
		return invalid("character expected revision=%d", req.Character.ExpectedRevision)
	}
	if req.ExpectedPendingCost < 0 || req.ExpectedPendingCost > 100 {
		return invalid("expected pending cost=%d", req.ExpectedPendingCost)
	}
	return nil
}

// commitDeathPenaltiesTx composes the penalty consumption inside an
// already-begun transaction: the complete character snapshot through
// the single saveCharacterSnapshotTx seam FIRST (its internal root
// CAS is the first aggregate-root mutation), then the pending-death
// read + raw-cost verification, then the same-transaction pending
// deletion. Raw pgx.ErrNoRows from the character CAS surfaces as
// *deathCASStale for the public boundary to map + count exactly once;
// pending semantic rejections classify inside the SAME transaction so
// the tentative character snapshot always rolls back. Every other
// error passes through unmapped.
func commitDeathPenaltiesTx(ctx context.Context, tx pgx.Tx, req DeathPenaltiesRequest) (DeathPenaltiesResult, error) {
	q := gen.New(tx)

	charRev, err := saveCharacterSnapshotTx(ctx, tx, req.Character)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DeathPenaltiesResult{}, &deathCASStale{
				aggregate: "character", id: req.Character.ID,
				expected: req.Character.ExpectedRevision,
			}
		}
		return DeathPenaltiesResult{}, fmt.Errorf("store: commit death penalties character: %w", err)
	}

	pending, err := q.GetPendingDeathByCharacter(ctx, req.Character.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DeathPenaltiesResult{}, fmt.Errorf(
				"store: commit death penalties character=%d: %w", req.Character.ID, ErrNoPendingDeath)
		}
		return DeathPenaltiesResult{}, fmt.Errorf("store: commit death penalties pending read: %w", err)
	}
	if pending.EffectiveCost != req.ExpectedPendingCost {
		return DeathPenaltiesResult{}, fmt.Errorf(
			"store: commit death penalties character=%d pending cost=%d expected=%d: %w",
			req.Character.ID, pending.EffectiveCost, req.ExpectedPendingCost, ErrPendingDeathCostMismatch)
	}

	if err := q.DeletePendingDeathByCharacter(ctx, req.Character.ID); err != nil {
		return DeathPenaltiesResult{}, fmt.Errorf("store: commit death penalties pending delete: %w", err)
	}

	return DeathPenaltiesResult{CharacterRevision: charRev}, nil
}

// CommitDeathPenalties atomically persists one Underworld-exit penalty
// consumption (spec §9.5.11a, §8.1/§8.3): the already-resolved complete
// post-penalty character snapshot through the character-root CAS plus
// the same-transaction pending-row deletion after raw-cost
// verification. ZERO ledger rows and ZERO kills rows are written.
// Neither the pending corpse association (set or NULL) nor the portal
// flag (false or true) gates consumption. A stale character root
// rolls back everything and maps to ErrStaleRevision (counted once
// under "character"); pending semantic rejections roll back the
// tentative character snapshot and never touch the stale metric.
func (s *PGStore) CommitDeathPenalties(ctx context.Context, req DeathPenaltiesRequest) (DeathPenaltiesResult, error) {
	if err := validateDeathPenaltiesRequest(req); err != nil {
		return DeathPenaltiesResult{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DeathPenaltiesResult{}, fmt.Errorf("store: commit death penalties: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	res, err := commitDeathPenaltiesTx(ctx, tx, req)
	if err != nil {
		var stale *deathCASStale
		if errors.As(err, &stale) {
			s.recordStale(stale.aggregate)
			return DeathPenaltiesResult{}, fmt.Errorf(
				"store: commit death penalties %s id=%d expected revision=%d: %w",
				stale.aggregate, stale.id, stale.expected, ErrStaleRevision)
		}
		return DeathPenaltiesResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return DeathPenaltiesResult{}, fmt.Errorf("store: commit death penalties: commit: %w", err)
	}
	return res, nil
}
