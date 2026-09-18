package sim

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
)

// Reserved Saver critical slot / generation seam (spec §9.5.1k,
// M5-T5c3d2b1, frozen v0.3.54): the generic Store-independent
// reservation primitive for active-player Portal persistence.
// It reserves the EXISTING per-key Saver gate(s) plus one
// already-allocated critical dirty generation per participant
// at Reserve time, executes later with execution-time
// expected revisions, and keeps normal CriticalSet
// success/error semantics with newer-pending retention.
//
// This file performs NO PG I/O, imports NO store/persist/pgx/
// sqlc/Prometheus packages, adds NO metric, and contains NO
// death-specific, gateway, proto, or recovery-query logic.
// It changes NO existing WriteCriticalSet behavior: the
// shared result-validation helper below produces
// byte-for-byte identical errors, and WriteCriticalSet keeps
// its late (post-gate) generation allocation.

// Stable reservation-domain errors. Matching MUST use
// errors.Is, never string parsing as control flow.
var (
	// ErrCriticalSetReservationBusy marks a non-blocking
	// ReserveCriticalSet whose participant gate is currently
	// owned (e.g. by an in-flight WriteThrough,
	// WriteCriticalSet, ResolveReconciled, or another live
	// reservation). Zero generation allocation, zero
	// reservation metadata. The caller may retry later.
	ErrCriticalSetReservationBusy = errors.New("sim: critical-set reservation busy")
	// ErrCriticalSetReservationConsumed marks Execute on a
	// reservation that is no longer Reserved (already
	// Executing/Consumed/Cancelled). The callback never runs.
	ErrCriticalSetReservationConsumed = errors.New("sim: critical-set reservation consumed")
)

// criticalReservationState is the one-shot reservation
// lifecycle: Reserved -> Executing -> Consumed, or Reserved ->
// Cancelled. Guarded by CriticalSetReservation.mu.
type criticalReservationState uint8

const (
	criticalReservationReserved criticalReservationState = iota
	criticalReservationExecuting
	criticalReservationConsumed
	criticalReservationCancelled
)

// CriticalSetReservation is a live reserved Saver critical
// slot (spec §9.5.1k, M5-T5c3d2b1). A successful reservation
// represents the exact immutable participant key set in
// canonical key order, exclusive ownership of each EXISTING
// Saver per-key gate, and one already-allocated critical
// dirty generation per participant. It carries NO Store
// value, Portal value, PG handle, pre-gate revision guess,
// SnapshotWrite, or gameplay payload.
//
// Thread-safe one-shot lifecycle: exactly one of Execute /
// Cancel wins the Reserved -> terminal transition; Cancel
// after Execute began is a no-op that never releases a gate
// from underneath the running callback; repeated
// Execute/Cancel after terminal state returns
// ErrCriticalSetReservationConsumed / no-op respectively.
type CriticalSetReservation struct {
	saver *Saver
	// Canonical participant keys (AggregateKey.Less order);
	// entries and critGen are parallel to keys.
	keys     []AggregateKey
	entries  []*saverEntry
	releases []func()
	critGen  []uint64
	mu       sync.Mutex
	state    criticalReservationState
}

// ReserveCriticalSet non-blockingly reserves a critical slot
// for an already-tracked participant set (spec §9.5.1k,
// M5-T5c3d2b1). It validates the full participant set,
// canonicalizes key order (the caller's slice is never
// reordered), try-acquires every EXISTING per-key Saver gate
// in canonical order while Saver metadata is still protected
// from Untrack races, prevalidates ALL participants, and ONLY
// after the WHOLE set passes increments each entry's dirty
// generation exactly once and marks one live critical
// reservation per key.
//
// Non-blocking contract: if ANY participant gate is
// unavailable, every already-acquired gate is released and
// ErrCriticalSetReservationBusy is returned with zero
// generation allocation and zero reservation metadata. No
// waiting, no goroutine, no timer.
//
// Existing WriteCriticalSet behavior is unchanged: ordinary
// WriteCriticalSet keeps its late generation timing.
func (s *Saver) ReserveCriticalSet(keys []AggregateKey) (*CriticalSetReservation, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("%w: empty participant set", ErrInvalidCriticalSet)
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

	// Canonical copy: input order never determines lock order
	// and the caller's slice is never reordered in place.
	ordered := append([]AggregateKey(nil), keys...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Less(ordered[j]) })

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
	// Non-blocking all-or-nothing acquisition of the SAME
	// existing per-key gates in canonical order, while s.mu
	// is still held so Untrack cannot interleave. The
	// try-acquire is in-memory and never waits.
	releases := make([]func(), 0, len(entries))
	for _, e := range entries {
		rel, ok := e.tryAcquire()
		if !ok {
			for i := len(releases) - 1; i >= 0; i-- {
				releases[i]()
			}
			s.mu.Unlock()
			return nil, fmt.Errorf("%w: %v", ErrCriticalSetReservationBusy, e.key)
		}
		releases = append(releases, rel)
	}
	releaseAll := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	// Whole-set prevalidation BEFORE any generation
	// allocation, so no partial allocation occurs when a
	// later participant fails preflight.
	for _, k := range ordered {
		if s.entries[k].blocked {
			releaseAll()
			s.mu.Unlock()
			return nil, fmt.Errorf("%w: %v", ErrSaverReconcileRequired, k)
		}
	}
	for _, k := range ordered {
		if s.entries[k].seq == math.MaxUint64 {
			releaseAll()
			s.mu.Unlock()
			return nil, fmt.Errorf("%w: dirty generation exhausted for %v", ErrSaverRevisionInvariant, k)
		}
	}
	// Fail-closed spirit shared with WriteCriticalSet: a
	// participant whose known revision cannot advance blocks
	// (that participant only) and no reservation is formed.
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
		releaseAll()
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: known revision %d cannot advance for %v",
			ErrSaverRevisionInvariant, int64(math.MaxInt64), maxed[0])
	}
	critGen := make([]uint64, len(ordered))
	for i, k := range ordered {
		e := s.entries[k]
		e.seq++
		critGen[i] = e.seq
		e.reserved = true
	}
	s.mu.Unlock()

	return &CriticalSetReservation{
		saver:    s,
		keys:     ordered,
		entries:  entries,
		releases: releases,
		critGen:  critGen,
		state:    criticalReservationReserved,
	}, nil
}

// Cancel abandons a Reserved reservation: reserved metadata is
// cleared, every held Saver gate is released exactly once, the
// known revision and pending snapshots are left unchanged, and
// the dirty sequence is NOT rolled backwards (a cancelled
// reservation leaves a valid generation gap). Terminal.
// Repeated Cancel is a no-op. Cancel after Execute has begun
// is a no-op: it never releases a gate from underneath the
// running callback.
func (r *CriticalSetReservation) Cancel() {
	r.mu.Lock()
	if r.state != criticalReservationReserved {
		r.mu.Unlock()
		return
	}
	r.state = criticalReservationCancelled
	r.mu.Unlock()

	s := r.saver
	s.mu.Lock()
	for i, k := range r.keys {
		if e, ok := s.entries[k]; ok && e == r.entries[i] {
			e.reserved = false
		}
	}
	s.mu.Unlock()
	r.releaseAll()
}

// Execute runs the reserved critical write through the
// already-held gates (spec §9.5.1k, M5-T5c3d2b1). It MUST NOT
// reacquire the gates. Before the callback it checks context
// cancellation, verifies every reservation entry still names
// the tracked entry, verifies no participant became
// reconcile-blocked, verifies known revisions can advance,
// captures the CURRENT known revision per participant as the
// execution-time expected revision set, and transitions each
// entry reserved -> inflight with NO second seq allocation.
//
// Because a successful Reserve serializes all
// revision-changing callbacks on these keys (WriteThrough,
// ordinary WriteCriticalSet, ResolveReconciled all wait on
// the SAME held gates), the execution-time expected revision
// necessarily equals the known revision at reservation
// acquisition and cannot change until Execute/Cancel. This is
// a FEATURE of the gate reservation, not permission to guess
// the revision at the API boundary: expected revisions are
// still captured ONLY here at Execute time.
//
// Pre-callback failure: the callback is not invoked, no new
// reconcile block is caused by this reservation (existing
// block state, if discovered, is left as-is), reserved
// metadata is cleared, gates are released, and the
// reservation is terminal.
//
// Post-invocation failure uses the exact existing
// WriteCriticalSet conservative rule: ANY callback error
// advances nothing, clears no pending state,
// reconcile-blocks ALL participants, and preserves both the
// callback cause and ErrSaverReconcileRequired. Malformed
// callback success is commit-ambiguous with the same
// treatment plus ErrSaverRevisionInvariant.
//
// On fully valid success every participant advances to its
// returned revision with inflight/reserved cleared; pending
// snapshots with gen <= the reserved generation are
// superseded while newer ones (a MarkDirty that arrived
// after Reserve) are retained.
func (r *CriticalSetReservation) Execute(ctx context.Context, write CriticalSetWrite) ([]AggregateRevision, error) {
	if write == nil {
		return nil, fmt.Errorf("%w: nil write", ErrInvalidCriticalSet)
	}
	r.mu.Lock()
	if r.state != criticalReservationReserved {
		r.mu.Unlock()
		return nil, fmt.Errorf("%w: reservation already consumed or cancelled", ErrCriticalSetReservationConsumed)
	}
	r.state = criticalReservationExecuting
	r.mu.Unlock()

	s := r.saver
	// abort tears down a pre-callback failure: no callback,
	// no new block, metadata cleared, gates released,
	// reservation terminal.
	abort := func(err error) ([]AggregateRevision, error) {
		s.mu.Lock()
		for i, k := range r.keys {
			if e, ok := s.entries[k]; ok && e == r.entries[i] {
				e.reserved = false
			}
		}
		s.mu.Unlock()
		r.releaseAll()
		r.setConsumed()
		return nil, err
	}
	finish := func() {
		r.releaseAll()
		r.setConsumed()
	}

	if err := ctx.Err(); err != nil {
		return abort(fmt.Errorf("sim: saver reserved critical-set: %w", err))
	}

	s.mu.Lock()
	for i, k := range r.keys {
		cur, ok := s.entries[k]
		if !ok || cur != r.entries[i] {
			s.mu.Unlock()
			return abort(fmt.Errorf("%w: %v", ErrAggregateNotTracked, k))
		}
	}
	for _, k := range r.keys {
		if s.entries[k].blocked {
			s.mu.Unlock()
			return abort(fmt.Errorf("%w: %v", ErrSaverReconcileRequired, k))
		}
	}
	for _, k := range r.keys {
		if s.entries[k].known == math.MaxInt64 {
			s.mu.Unlock()
			return abort(fmt.Errorf("%w: known revision %d cannot advance for %v",
				ErrSaverRevisionInvariant, int64(math.MaxInt64), k))
		}
	}
	expected := make([]AggregateRevision, len(r.keys))
	for i, k := range r.keys {
		e := s.entries[k]
		e.reserved = false
		e.inflight = true
		expected[i] = AggregateRevision{Key: k, Revision: e.known}
	}
	s.mu.Unlock()

	results, werr := write(ctx, append([]AggregateRevision(nil), expected...))
	if werr != nil {
		// Commit-ambiguous by construction: same conservative
		// treatment as WriteCriticalSet.
		s.mu.Lock()
		for _, k := range r.keys {
			if e, ok := s.entries[k]; ok {
				e.inflight = false
				e.blocked = true
			}
		}
		s.mu.Unlock()
		finish()
		return nil, fmt.Errorf("sim: saver reserved critical-set: %w", errors.Join(werr, ErrSaverReconcileRequired))
	}

	newFor, detail := validateCriticalSetResults(expected, results)
	if detail != nil {
		s.mu.Lock()
		for _, k := range r.keys {
			if e, ok := s.entries[k]; ok {
				e.inflight = false
				e.blocked = true
			}
		}
		s.mu.Unlock()
		finish()
		return nil, fmt.Errorf("sim: saver reserved critical-set: %w", errors.Join(detail, ErrSaverReconcileRequired))
	}

	s.mu.Lock()
	for i, k := range r.keys {
		e := s.entries[k]
		e.known = newFor[k]
		e.inflight = false
		e.reserved = false
		if e.pending != nil && e.pending.gen <= r.critGen[i] {
			e.pending = nil
		}
	}
	s.mu.Unlock()
	finish()

	out := make([]AggregateRevision, len(r.keys))
	for i, k := range r.keys {
		out[i] = AggregateRevision{Key: k, Revision: newFor[k]}
	}
	return out, nil
}

// releaseAll releases every held Saver gate in reverse
// canonical order. Called exactly once per reservation, on
// exactly one terminal path (Execute won or Cancel won).
func (r *CriticalSetReservation) releaseAll() {
	for i := len(r.releases) - 1; i >= 0; i-- {
		r.releases[i]()
	}
}

// setConsumed marks the reservation terminal after Execute
// (success, callback error, invariant failure, or
// pre-callback abort).
func (r *CriticalSetReservation) setConsumed() {
	r.mu.Lock()
	r.state = criticalReservationConsumed
	r.mu.Unlock()
}

// validateCriticalSetResults checks callback results against
// the captured expected revisions: exactly one expected+1
// result per participant (no missing/extra/duplicate key,
// revision >= 0 and == expected+1). Shared by
// WriteCriticalSet and CriticalSetReservation.Execute so the
// conservative commit-ambiguity rule stays identical in both
// paths; error values are byte-for-byte identical to the
// former inline WriteCriticalSet validation.
func validateCriticalSetResults(expected, results []AggregateRevision) (map[AggregateKey]int64, error) {
	expFor := make(map[AggregateKey]int64, len(expected))
	for _, er := range expected {
		expFor[er.Key] = er.Revision
	}
	newFor := make(map[AggregateKey]int64, len(expected))
	var detail error
	if len(results) != len(expected) {
		detail = fmt.Errorf("%w: critical-set result count %d want %d",
			ErrSaverRevisionInvariant, len(results), len(expected))
	} else {
		for _, res := range results {
			exp, ok := expFor[res.Key]
			if !ok {
				detail = fmt.Errorf("%w: critical-set result extra key %v",
					ErrSaverRevisionInvariant, res.Key)
				break
			}
			if _, dup := newFor[res.Key]; dup {
				detail = fmt.Errorf("%w: critical-set result duplicate key %v",
					ErrSaverRevisionInvariant, res.Key)
				break
			}
			if res.Revision < 0 || res.Revision != exp+1 {
				detail = fmt.Errorf("%w: critical-set result %v expected %d got %d",
					ErrSaverRevisionInvariant, res.Key, exp+1, res.Revision)
				break
			}
			newFor[res.Key] = res.Revision
		}
		if detail == nil && len(newFor) != len(expected) {
			detail = fmt.Errorf("%w: critical-set result missing key",
				ErrSaverRevisionInvariant)
		}
	}
	if detail != nil {
		return nil, detail
	}
	return newFor, nil
}

// tryAcquire takes this entry's write ownership WITHOUT
// waiting: it reports false immediately when the gate is
// owned elsewhere. The caller MUST call the release func
// exactly once after a successful acquisition, without
// holding the Saver mutex.
func (e *saverEntry) tryAcquire() (release func(), ok bool) {
	select {
	case <-e.gate:
		return func() { e.gate <- struct{}{} }, true
	default:
		return nil, false
	}
}
