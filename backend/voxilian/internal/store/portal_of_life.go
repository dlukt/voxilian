package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/dlukt/voxilian/internal/store/gen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// M5-T5b2a Portal-of-Life durable state transition (spec §9.5.10a,
// §8.1/§8.3). Portal persists immediately as its own atomic critical
// transaction AFTER successful death entry and BEFORE Underworld
// exit: character-root revision CAS first, then the lowers-only
// pending-death cost update with the once-per-corpse flag. No corpse
// mutation, no item mutation, no kill, no ledger, no pending-row
// deletion, no Underworld-exit penalties.

// ErrInvalidPortalOfLife marks a malformed CommitPortalOfLife request
// rejected before Begin. Matching MUST use errors.Is, never string
// parsing.
var ErrInvalidPortalOfLife = errors.New("invalid portal of life")

// ErrNoPendingDeath reports a Portal attempt with no live
// pending_deaths row for the character. The tentative character CAS
// rolls back; no stale metric is incremented.
var ErrNoPendingDeath = errors.New("no pending death")

// ErrPortalCorpseMismatch reports a Portal attempt whose requested
// corpse ID does not match the pending row's current corpse_id —
// including a pending row whose corpse_id became NULL through corpse
// expiry (ON DELETE SET NULL) or a request naming a different corpse.
// The tentative character CAS rolls back; no stale metric is
// incremented.
var ErrPortalCorpseMismatch = errors.New("portal corpse mismatch")

// ErrPortalAlreadyUsed reports a Portal attempt against the same
// corpse after portal_used became TRUE. The tentative character CAS
// rolls back; no stale metric is incremented.
var ErrPortalAlreadyUsed = errors.New("portal already used")

// Portal cost domain: the frozen output domain of the pure T5a Portal
// calculation (spec §9.5.10). The current pending cost itself lives
// in 0..100; Store calculates no formula and accepts only the
// caller-resolved proposal in this domain.
const (
	minPortalProposedCost int16 = 5
	maxPortalProposedCost int16 = 80
)

// PortalOfLifeRequest is the complete already-resolved Portal input.
// Character is the COMPLETE authoritative character snapshot: Portal
// normally changes no root gameplay field itself, but the child
// pending-death mutation belongs to the character aggregate, so the
// full snapshot composes through the character-root CAS.
// CorpseID is the durable target corpse the caster actually
// targeted. ProposedCost is the caller-resolved pure T5a Portal
// result (5..80). Store calculates no corpse age, spell power, or
// Portal formula.
type PortalOfLifeRequest struct {
	Character CharacterSnapshot

	CorpseID     int64
	ProposedCost int16
}

// PortalOfLifeResult carries only durable information known after a
// successful commit. On ANY error the result is zero: a commit error
// is ambiguous — the caller must reconcile/retry per §8.3, never
// assume rollback or success, and never locally advance revision.
type PortalOfLifeResult struct {
	CharacterRevision int64
	EffectiveCost     int16
}

// validatePortalOfLifeRequest rejects malformed requests before any
// PG mutation. It never touches the database.
func validatePortalOfLifeRequest(req PortalOfLifeRequest) error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("store: commit portal of life "+format+": %w",
			append(args, ErrInvalidPortalOfLife)...)
	}
	if req.Character.ID <= 0 {
		return invalid("character id=%d", req.Character.ID)
	}
	if req.Character.ExpectedRevision < 0 {
		return invalid("character expected revision=%d", req.Character.ExpectedRevision)
	}
	if req.CorpseID <= 0 {
		return invalid("corpse id=%d", req.CorpseID)
	}
	if req.ProposedCost < minPortalProposedCost || req.ProposedCost > maxPortalProposedCost {
		return invalid("proposed cost=%d", req.ProposedCost)
	}
	return nil
}

// commitPortalOfLifeTx composes the Portal transition inside an
// already-begun transaction: character-root CAS first (no
// pending-death query or mutation occurs before its success), then
// the atomic lowers-only cost update with the once flag. Raw
// pgx.ErrNoRows from the character CAS surfaces as *deathCASStale
// for the public boundary to map + count exactly once; a portal
// UPDATE miss is classified inside the SAME transaction via
// GetPendingDeathByCharacter so the tentative character CAS always
// rolls back with the semantic error. Every other error passes
// through unmapped.
func commitPortalOfLifeTx(ctx context.Context, tx pgx.Tx, req PortalOfLifeRequest) (PortalOfLifeResult, error) {
	q := gen.New(tx)

	charRev, err := saveCharacterSnapshotTx(ctx, tx, req.Character)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PortalOfLifeResult{}, &deathCASStale{
				aggregate: "character", id: req.Character.ID,
				expected: req.Character.ExpectedRevision,
			}
		}
		return PortalOfLifeResult{}, fmt.Errorf("store: commit portal of life character: %w", err)
	}

	updated, err := q.ApplyPendingDeathPortal(ctx, gen.ApplyPendingDeathPortalParams{
		ProposedCost: req.ProposedCost,
		CharacterID:  req.Character.ID,
		CorpseID:     pgtype.Int8{Int64: req.CorpseID, Valid: true},
	})
	if err == nil {
		return PortalOfLifeResult{
			CharacterRevision: charRev,
			EffectiveCost:     updated.EffectiveCost,
		}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return PortalOfLifeResult{}, fmt.Errorf("store: commit portal of life portal update: %w", err)
	}

	pending, gerr := q.GetPendingDeathByCharacter(ctx, req.Character.ID)
	if gerr != nil {
		if errors.Is(gerr, pgx.ErrNoRows) {
			return PortalOfLifeResult{}, fmt.Errorf(
				"store: commit portal of life character=%d: %w", req.Character.ID, ErrNoPendingDeath)
		}
		return PortalOfLifeResult{}, fmt.Errorf("store: commit portal of life pending read: %w", gerr)
	}
	if !pending.CorpseID.Valid || pending.CorpseID.Int64 != req.CorpseID {
		return PortalOfLifeResult{}, fmt.Errorf(
			"store: commit portal of life character=%d corpse=%d: %w",
			req.Character.ID, req.CorpseID, ErrPortalCorpseMismatch)
	}
	if pending.PortalUsed {
		return PortalOfLifeResult{}, fmt.Errorf(
			"store: commit portal of life character=%d corpse=%d: %w",
			req.Character.ID, req.CorpseID, ErrPortalAlreadyUsed)
	}
	return PortalOfLifeResult{}, fmt.Errorf(
		"store: commit portal of life character=%d corpse=%d: portal update missed a matching unused pending row",
		req.Character.ID, req.CorpseID)
}

// CommitPortalOfLife atomically persists one Portal-of-Life state
// transition (spec §9.5.10a, §8.1/§8.3): the already-resolved
// complete character snapshot through the character-root CAS plus
// the pending-death lowers-only cost update
// (`effective_cost = min(current, proposed)`) with
// `portal_used = TRUE` against the durable target corpse ID. ZERO
// ledger rows and ZERO kills rows are written. A stale character
// root rolls back everything and maps to ErrStaleRevision (counted
// once under "character"); portal semantic rejections roll back the
// tentative character CAS and never touch the stale metric.
func (s *PGStore) CommitPortalOfLife(ctx context.Context, req PortalOfLifeRequest) (PortalOfLifeResult, error) {
	if err := validatePortalOfLifeRequest(req); err != nil {
		return PortalOfLifeResult{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PortalOfLifeResult{}, fmt.Errorf("store: commit portal of life: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	res, err := commitPortalOfLifeTx(ctx, tx, req)
	if err != nil {
		var stale *deathCASStale
		if errors.As(err, &stale) {
			s.recordStale(stale.aggregate)
			return PortalOfLifeResult{}, fmt.Errorf(
				"store: commit portal of life %s id=%d expected revision=%d: %w",
				stale.aggregate, stale.id, stale.expected, ErrStaleRevision)
		}
		return PortalOfLifeResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PortalOfLifeResult{}, fmt.Errorf("store: commit portal of life: commit: %w", err)
	}
	return res, nil
}
