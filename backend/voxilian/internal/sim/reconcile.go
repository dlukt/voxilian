package sim

import (
	"context"
	"errors"
	"fmt"
)

// Stable reconciliation-domain errors. Matching MUST use
// errors.Is, never string parsing as control flow.
var (
	// ErrInvalidDurableRevision marks a negative persisted
	// revision supplied to reconciliation metadata. Persisted
	// root revisions are int64 >= 0; there is no unsigned cast.
	ErrInvalidDurableRevision = errors.New("sim: invalid durable revision")
	// ErrReconcileRequired marks a revision-gap commit notice
	// (notice beyond known+1): the payload was NOT applied and a
	// complete materialized-PG reload is required before the
	// next mutation. Never replay missing notifications.
	ErrReconcileRequired = errors.New("sim: reconcile required")
	// ErrReconcileRevisionBehind marks a reload candidate below
	// the required revision: Apply is not invoked, pending
	// remains, memory is unchanged.
	ErrReconcileRevisionBehind = errors.New("sim: reconcile revision behind")
	// ErrReconcileRevisionRegression marks a reload candidate
	// below the known revision: Apply is not invoked, and
	// in-memory persisted revision never moves backward.
	ErrReconcileRevisionRegression = errors.New("sim: reconcile revision regression")
)

// ReconcileState is the small reusable post-commit
// reconciliation fence (spec §5.6.2): the persisted aggregate
// revision the current in-memory contents are known to
// represent, plus whether a newer committed revision is still
// unreflected in memory. Single-writer, in-memory, ephemeral:
// not persisted, not a ledger, not an OpID cache, no mutex, not
// arbitrarily concurrent-safe. It is deliberately generic and
// attaches to no entity: tests associate it with owners as
// needed, and future real aggregates own one each.
type ReconcileState struct {
	known    int64
	pending  bool
	required int64
}

// NewReconcileState starts tracking at a known persisted
// revision: the caller supplies the ACTUAL persisted revision
// from the initial PG load (never a silent zero default for
// unknown persisted state). Negative revisions are rejected.
func NewReconcileState(knownRevision int64) (*ReconcileState, error) {
	if knownRevision < 0 {
		return nil, fmt.Errorf("%w: initial %d", ErrInvalidDurableRevision, knownRevision)
	}
	return &ReconcileState{known: knownRevision, required: knownRevision}, nil
}

// ReconcileSnapshot is the lightweight immutable inspection
// result for tests/debugging. No mutable internal pointer.
type ReconcileSnapshot struct {
	KnownRevision    int64
	Pending          bool
	RequiredRevision int64
}

// Snapshot copies the current fence state.
func (r *ReconcileState) Snapshot() ReconcileSnapshot {
	return ReconcileSnapshot{KnownRevision: r.known, Pending: r.pending, RequiredRevision: r.required}
}

// MarkCommitted records that a PG transaction successfully
// committed a new persisted revision for this aggregate
// (spec §5.6.3). It changes reconciliation metadata ONLY — no
// gameplay fields, no in-memory persisted revision, no OpID
// dedupe, no delta, no history. The in-memory aggregate may
// still represent knownRevision.
//
//   - revision < 0: invalid-revision error, zero mutation.
//   - revision <= known: stale/already-observed mark, unchanged.
//   - revision > known: pending with
//     requiredRevision = max(requiredRevision, revision), so
//     several marks before reload never lower the requirement.
//
// Callers install marks for EVERY affected participant AFTER a
// successful tx.Commit and BEFORE any commit notification.
func (r *ReconcileState) MarkCommitted(revision int64) error {
	if revision < 0 {
		return fmt.Errorf("%w: mark %d", ErrInvalidDurableRevision, revision)
	}
	if revision <= r.known {
		return nil
	}
	r.pending = true
	if revision > r.required {
		r.required = revision
	}
	return nil
}

// DurableCommitNotice is the generic commit-notification
// revision metadata (spec §5.6.4). ID is exactly the T3b
// operation's OpID — no replacement on redelivery, route
// refresh, or reload. The real gameplay payload belongs to the
// future owning operation; T3c carries no production payload.
type DurableCommitNotice struct {
	ID       OpID
	Revision int64
}

// CommitNoticeDisposition is the ApplyCommitNotice outcome. A
// revision gap is an error requiring reload, not a
// disposition; on error the disposition is meaningless — check
// err first.
type CommitNoticeDisposition int

const (
	// CommitNoticeApplied means the notice was the exact next
	// revision and its apply callback ran.
	CommitNoticeApplied CommitNoticeDisposition = iota
	// CommitNoticeStale means the notice revision was already
	// known: no-op, apply not called, no rollback.
	CommitNoticeStale
)

// String names the disposition for logs/tests.
func (d CommitNoticeDisposition) String() string {
	switch d {
	case CommitNoticeApplied:
		return "applied"
	case CommitNoticeStale:
		return "stale"
	default:
		return "unknown"
	}
}

// ApplyCommitNotice applies one commit notification as an
// incremental in-memory update ONLY when it is exactly the
// next revision (spec §5.6.4). Like T3b's apply callback, the
// supplied apply must leave the in-memory aggregate unmodified
// on error; T3c rolls back no partially-mutating callback.
//
//   - ID == 0: the existing T3b ErrInvalidOpID (no parallel
//     commit-ID domain); apply not called, metadata unchanged.
//   - notice.Revision <= known: stale no-op (covers late
//     notices arriving after a reload already reached that
//     revision or beyond).
//   - notice.Revision > known+1: revision gap — apply NOT
//     invoked, requiredRevision raised to at least the notice,
//     ErrReconcileRequired. Recovery is a full materialized-PG
//     reload, never replaying missing notifications.
//   - notice.Revision == known+1: apply runs; on error known is
//     unchanged with pending/required retained (the same notice
//     may be retried); on success known advances and the fence
//     clears once known >= required.
func (r *ReconcileState) ApplyCommitNotice(notice DurableCommitNotice, apply func() error) (CommitNoticeDisposition, error) {
	if notice.ID == OpID(0) {
		return CommitNoticeApplied, fmt.Errorf("%w: commit notice rev %d",
			ErrInvalidOpID, notice.Revision)
	}
	if notice.Revision <= r.known {
		return CommitNoticeStale, nil
	}
	if notice.Revision > r.known+1 {
		r.pending = true
		if notice.Revision > r.required {
			r.required = notice.Revision
		}
		return CommitNoticeApplied, fmt.Errorf("%w: known %d notice %d",
			ErrReconcileRequired, r.known, notice.Revision)
	}
	if err := apply(); err != nil {
		r.pending = true
		if notice.Revision > r.required {
			r.required = notice.Revision
		}
		return CommitNoticeApplied, fmt.Errorf("sim: commit notice rev %d: %w", notice.Revision, err)
	}
	r.known = notice.Revision
	if r.known >= r.required {
		r.pending = false
		r.required = r.known
	}
	return CommitNoticeApplied, nil
}

// ReloadCandidate is the staged PG reload (spec §5.6.5): the
// loader reads PG into temporary values and returns the loaded
// revision plus an apply closure. Reconciliation validates the
// revision BEFORE invoking Apply, so a stale load can never
// overwrite newer memory.
type ReloadCandidate struct {
	Revision int64
	Apply    func() error
}

// ReloadFunc loads a staged candidate. It MUST NOT mutate the
// live aggregate: population happens only through the returned
// Apply closure after revision validation.
type ReloadFunc func(ctx context.Context) (ReloadCandidate, error)

// EnsureReconciled is the mutation gate (spec §5.6.5): future
// aggregate mutations call reconcile-first, then validate, then
// mutate. While not pending it performs ZERO PG reads (the
// loader is never invoked and may be nil). While pending it
// stages a candidate and enforces:
//
//   - loader error: wrapped, pending/known unchanged, no Apply.
//   - candidate below known: revision-regression error, no Apply,
//     persisted revision never moves backward.
//   - candidate below required: revision-behind error, no Apply,
//     pending remains, memory unchanged.
//   - missing Apply: error, no state change.
//   - valid candidate with failing Apply: known unchanged,
//     pending remains, mutation stays blocked.
//   - valid candidate with successful Apply: known becomes the
//     candidate revision (leaping forward past the fence when PG
//     is newer), pending clears.
//
// No hidden goroutine, no background polling.
func (r *ReconcileState) EnsureReconciled(ctx context.Context, reload ReloadFunc) error {
	if !r.pending {
		return nil
	}
	cand, err := reload(ctx)
	if err != nil {
		return fmt.Errorf("sim: reconcile reload: %w", err)
	}
	if cand.Revision < r.known {
		return fmt.Errorf("%w: known %d candidate %d",
			ErrReconcileRevisionRegression, r.known, cand.Revision)
	}
	if cand.Revision < r.required {
		return fmt.Errorf("%w: required %d candidate %d",
			ErrReconcileRevisionBehind, r.required, cand.Revision)
	}
	if cand.Apply == nil {
		return fmt.Errorf("sim: reload candidate rev %d missing apply", cand.Revision)
	}
	if err := cand.Apply(); err != nil {
		return fmt.Errorf("sim: reload candidate rev %d apply: %w", cand.Revision, err)
	}
	r.known = cand.Revision
	r.pending = false
	r.required = r.known
	return nil
}
