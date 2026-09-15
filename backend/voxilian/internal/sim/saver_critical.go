package sim

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
)

// Multi-root critical Saver coordination (spec §8.3.17, §9.5.1a,
// M5-T5c1): the generic Store-agnostic primitive for a critical PG
// transaction that atomically advances MULTIPLE already-tracked
// aggregate roots. The Saver owns the gates of all participating
// roots simultaneously while ONE caller-supplied callback executes
// the underlying critical persistence. This file performs NO PG
// I/O, imports NO store/persist/pgx/sqlc/Prometheus packages, adds
// NO metric, and contains NO death-specific, gateway, proto, or
// recovery-query logic.

// AggregateRevision pairs one tracked aggregate root with a
// persisted revision: an expected (currently known) revision on the
// way into a CriticalSetWrite, or a newly committed revision on
// the way out of WriteCriticalSet. Slices of this value type are
// always canonical by AggregateKey.Less, never Go map order.
type AggregateRevision struct {
	Key      AggregateKey
	Revision int64
}

// CriticalSetWrite is the multi-root persistence seam: it executes
// the ONE underlying critical transaction for the whole
// participant set. The saver supplies the current authoritative
// known persisted revision per participant (canonical order) only
// after all participant gates are owned; the callback MUST return
// exactly one result per participant with each revision advanced
// by exactly one. Slices are fresh per call: mutating them cannot
// alter Saver state.
type CriticalSetWrite func(ctx context.Context, expected []AggregateRevision) ([]AggregateRevision, error)

// ErrInvalidCriticalSet marks a malformed critical participant set
// (empty set, nil callback, duplicate key). Invalid AggregateKey
// values keep the existing ErrInvalidAggregateKey domain.
var ErrInvalidCriticalSet = errors.New("sim: invalid critical set")

// WriteCriticalSet atomically advances a set of already-tracked
// aggregate roots through ONE caller callback. Ordinary per-root
// WriteThrough calls would destroy the atomic Store transaction,
// so every participant's existing per-key saver gate (the SAME
// gate registry shared with WriteThrough, saveOne/FlushDirty, and
// ResolveReconciled — no second lock registry) is owned
// simultaneously while the callback runs.
//
// Binding semantics (spec §9.5.1a): canonical AggregateKey.Less
// gate order (input order never determines lock order; the
// caller's slice is never reordered in place); execution-time
// revision capture after gate ownership; one critical dirty
// generation per participant with pending.gen <= critical
// supersession and newer-pending retention; conservative
// post-invocation error rule (ANY callback error advances
// nothing, clears no pending state, reconcile-blocks ALL
// participants, and preserves both the callback cause and
// ErrSaverReconcileRequired); no blanket block for failures
// before invocation; MaxInt64 fail-closed; exact expected+1
// result validation with canonical result order.
func (s *Saver) WriteCriticalSet(ctx context.Context, keys []AggregateKey, write CriticalSetWrite) ([]AggregateRevision, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("%w: empty participant set", ErrInvalidCriticalSet)
	}
	if write == nil {
		return nil, fmt.Errorf("%w: nil write", ErrInvalidCriticalSet)
	}
	for _, k := range keys {
		if err := k.validate(); err != nil {
			return nil, err
		}
	}
	seen := make(map[AggregateKey]struct{}, len(keys))
	for _, k := range keys {
		if _, ok := seen[k]; ok {
			return nil, fmt.Errorf("%w: duplicate participant %v", ErrInvalidCriticalSet, k)
		}
		seen[k] = struct{}{}
	}

	// Canonical copy: input order never determines lock order and
	// the caller's slice is never reordered in place.
	ordered := append([]AggregateKey(nil), keys...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Less(ordered[j]) })

	// Resolve participant entries without holding gates yet.
	s.mu.Lock()
	entries := make([]*saverEntry, len(ordered))
	for i, k := range ordered {
		e, ok := s.entries[k]
		if !ok {
			s.mu.Unlock()
			return nil, fmt.Errorf("%w: %v", ErrAggregateNotTracked, k)
		}
		entries[i] = e
	}
	s.mu.Unlock()

	// Acquire every per-key gate in canonical order so overlapping
	// critical sets cannot deadlock. A cancellation while waiting
	// releases every already-acquired gate with zero Saver
	// mutation, zero callback invocation, and no new block.
	releases := make([]func(), 0, len(entries))
	releaseAll := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	for _, e := range entries {
		rel, err := e.acquire(ctx)
		if err != nil {
			releaseAll()
			return nil, err
		}
		releases = append(releases, rel)
	}
	// A cancellation that landed during (or before) the gate wait
	// must not run the callback: re-check deterministically after
	// ownership, mirroring WriteThrough.
	if err := ctx.Err(); err != nil {
		releaseAll()
		return nil, fmt.Errorf("sim: saver critical-set: %w", err)
	}

	// Participant preparation under ONE short metadata section,
	// before the callback. Every participant is validated FIRST;
	// generations are allocated only after the WHOLE set passes,
	// so no partial allocation occurs when a later participant
	// fails preflight. The callback runs with mu released.
	s.mu.Lock()
	for i, k := range ordered {
		cur, ok := s.entries[k]
		if !ok || cur != entries[i] {
			s.mu.Unlock()
			releaseAll()
			return nil, fmt.Errorf("%w: %v", ErrAggregateNotTracked, k)
		}
	}
	for _, k := range ordered {
		if s.entries[k].blocked {
			s.mu.Unlock()
			releaseAll()
			return nil, fmt.Errorf("%w: %v", ErrSaverReconcileRequired, k)
		}
	}
	for _, k := range ordered {
		if s.entries[k].seq == math.MaxUint64 {
			s.mu.Unlock()
			releaseAll()
			return nil, fmt.Errorf("%w: dirty generation exhausted for %v", ErrSaverRevisionInvariant, k)
		}
	}
	// WriteThrough fail-closed spirit: a participant whose known
	// revision cannot advance blocks (that participant only —
	// unrelated participants are never altered here) and no
	// transaction is attempted.
	var maxed []AggregateKey
	for _, k := range ordered {
		if s.entries[k].known == math.MaxInt64 {
			maxed = append(maxed, k)
		}
	}
	if len(maxed) > 0 {
		for _, k := range maxed {
			s.entries[k].blocked = true
		}
		s.mu.Unlock()
		releaseAll()
		return nil, fmt.Errorf("%w: known revision %d cannot advance for %v",
			ErrSaverRevisionInvariant, int64(math.MaxInt64), maxed[0])
	}
	critGen := make([]uint64, len(ordered))
	expected := make([]AggregateRevision, len(ordered))
	for i, k := range ordered {
		e := s.entries[k]
		e.seq++
		critGen[i] = e.seq
		e.inflight = true
		expected[i] = AggregateRevision{Key: k, Revision: e.known}
	}
	s.mu.Unlock()

	results, werr := write(ctx, append([]AggregateRevision(nil), expected...))
	if werr != nil {
		// Commit-ambiguous by construction: the callback already
		// ran, so its error cannot be classified as definitely
		// rolled back vs acknowledgement lost after commit. The
		// generic safe rule advances nothing, clears no pending
		// state, and reconcile-blocks ALL participants; a later
		// owner reconciles every participant via the existing
		// ResolveReconciled before mutation resumes. No retry.
		s.mu.Lock()
		for _, k := range ordered {
			if e, ok := s.entries[k]; ok {
				e.inflight = false
				e.blocked = true
			}
		}
		s.mu.Unlock()
		releaseAll()
		return nil, fmt.Errorf("sim: saver critical-set: %w", errors.Join(werr, ErrSaverReconcileRequired))
	}

	// Validate the callback result against the captured expected
	// set. Callback order is not trusted; exactly one
	// expected+1 result per participant is required.
	expFor := make(map[AggregateKey]int64, len(ordered))
	for _, er := range expected {
		expFor[er.Key] = er.Revision
	}
	newFor := make(map[AggregateKey]int64, len(ordered))
	var detail error
	if len(results) != len(ordered) {
		detail = fmt.Errorf("%w: critical-set result count %d want %d",
			ErrSaverRevisionInvariant, len(results), len(ordered))
	} else {
		for _, r := range results {
			exp, ok := expFor[r.Key]
			if !ok {
				detail = fmt.Errorf("%w: critical-set result extra key %v",
					ErrSaverRevisionInvariant, r.Key)
				break
			}
			if _, dup := newFor[r.Key]; dup {
				detail = fmt.Errorf("%w: critical-set result duplicate key %v",
					ErrSaverRevisionInvariant, r.Key)
				break
			}
			if r.Revision < 0 || r.Revision != exp+1 {
				detail = fmt.Errorf("%w: critical-set result %v expected %d got %d",
					ErrSaverRevisionInvariant, r.Key, exp+1, r.Revision)
				break
			}
			newFor[r.Key] = r.Revision
		}
		if detail == nil && len(newFor) != len(ordered) {
			detail = fmt.Errorf("%w: critical-set result missing key",
				ErrSaverRevisionInvariant)
		}
	}
	if detail != nil {
		// The callback already executed, so malformed success
		// results are commit-ambiguous from the Saver's
		// perspective: same conservative treatment as a
		// callback error, with zero/empty success output and
		// no partial acceptance.
		s.mu.Lock()
		for _, k := range ordered {
			if e, ok := s.entries[k]; ok {
				e.inflight = false
				e.blocked = true
			}
		}
		s.mu.Unlock()
		releaseAll()
		return nil, fmt.Errorf("sim: saver critical-set: %w", errors.Join(detail, ErrSaverReconcileRequired))
	}

	// Fully valid result: advance ALL participants under one
	// metadata section so no participant temporarily appears
	// advanced while another keeps its old revision. Pending
	// snapshots at or below the critical generation are
	// superseded; newer ones (a MarkDirty that arrived while the
	// callback ran) are retained for a later ordinary flush.
	s.mu.Lock()
	for i, k := range ordered {
		e := s.entries[k]
		e.known = newFor[k]
		e.inflight = false
		if e.pending != nil && e.pending.gen <= critGen[i] {
			e.pending = nil
		}
	}
	s.mu.Unlock()
	releaseAll()

	out := make([]AggregateRevision, len(ordered))
	for i, k := range ordered {
		out[i] = AggregateRevision{Key: k, Revision: newFor[k]}
	}
	return out, nil
}
