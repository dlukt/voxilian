package sim

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
)

// Snapshot saver core (spec §8.3, M4-T4a): generic revision-safe
// machinery for tracked durable aggregates. It owns NO gameplay
// state, imports NO store/pgx/Prometheus packages, and performs NO
// PG I/O itself: every persistence attempt runs through a caller
// supplied SnapshotWrite closure. Real Store composition, metrics,
// and crash proof belong to M4-T4b.

// AggregateKind names the durable aggregate family coordinated by
// the saver (spec §8.3.2). It is INTERNAL persistence coordination
// only — never an EntityID, NetEntityID, or wire value.
type AggregateKind uint8

const (
	// AggregateCharacter tracks a durable character root
	// (ID = character root ID, Scope empty).
	AggregateCharacter AggregateKind = iota + 1
	// AggregateItem tracks a durable item root
	// (ID = item root ID, Scope empty).
	AggregateItem
	// AggregateBank tracks a durable bank root
	// (ID = durable character ID, Scope = bank system).
	AggregateBank
)

// MetricName returns the trusted low-cardinality metric kind name
// frozen for future T4b observability (spec §8.3.2): exactly
// "character", "item", or "bank". No IDs or user data.
func (k AggregateKind) MetricName() string {
	switch k {
	case AggregateCharacter:
		return "character"
	case AggregateItem:
		return "item"
	case AggregateBank:
		return "bank"
	default:
		return "unknown"
	}
}

// AggregateKey identifies one tracked durable aggregate root
// (spec §8.3.2). Character/item carry the durable root ID with
// empty Scope; bank carries the durable character ID with the
// non-empty bank system as Scope.
type AggregateKey struct {
	Kind  AggregateKind
	ID    int64
	Scope string
}

// Less orders saver keys canonically: Kind ascending, then ID
// ascending, then Scope lexical ascending (spec §8.3.10). Flush
// passes use this order, never Go map iteration order.
func (k AggregateKey) Less(o AggregateKey) bool {
	if k.Kind != o.Kind {
		return k.Kind < o.Kind
	}
	if k.ID != o.ID {
		return k.ID < o.ID
	}
	return k.Scope < o.Scope
}

// validate rejects unknown kinds, non-positive IDs, and misplaced
// scopes without touching saver state.
func (k AggregateKey) validate() error {
	switch k.Kind {
	case AggregateCharacter, AggregateItem:
		if k.Scope != "" {
			return fmt.Errorf("%w: character/item key must have empty scope", ErrInvalidAggregateKey)
		}
	case AggregateBank:
		if k.Scope == "" {
			return fmt.Errorf("%w: bank key must have non-empty scope", ErrInvalidAggregateKey)
		}
	default:
		return fmt.Errorf("%w: unknown kind %d", ErrInvalidAggregateKey, uint8(k.Kind))
	}
	if k.ID <= 0 {
		return fmt.Errorf("%w: id %d must be > 0", ErrInvalidAggregateKey, k.ID)
	}
	return nil
}

// SnapshotWrite is the persistence seam (spec §8.3.3): a queued job
// capturing a COMPLETE immutable aggregate snapshot. The closure
// captures snapshot CONTENT but does NOT freeze ExpectedRevision —
// the saver supplies the current authoritative known persisted
// revision only when the write actually owns the aggregate's save
// slot. A nil error requires newRevision == expectedRevision + 1
// (every §8.1 CAS advances exactly one revision).
type SnapshotWrite func(ctx context.Context, expectedRevision int64) (newRevision int64, err error)

// SaverErrorObserver receives a periodic Run pass's joined flush
// error. It MUST be non-blocking; sim never blocks persistence on
// observation. Nil means discard.
type SaverErrorObserver func(err error)

// Stable saver-domain errors. Matching MUST use errors.Is, never
// string parsing as control flow.
var (
	// ErrInvalidAggregateKey marks a malformed AggregateKey
	// (unknown kind, non-positive ID, misplaced scope).
	ErrInvalidAggregateKey = errors.New("sim: invalid aggregate key")
	// ErrAggregateNotTracked marks operations on a key with no
	// live saver registration.
	ErrAggregateNotTracked = errors.New("sim: aggregate not tracked")
	// ErrAggregateAlreadyTracked marks Track of a live key.
	ErrAggregateAlreadyTracked = errors.New("sim: aggregate already tracked")
	// ErrSaverUntrackDirty marks Untrack of a key that still
	// holds unsaved, in-flight, or reconcile-blocked state.
	ErrSaverUntrackDirty = errors.New("sim: cannot untrack dirty aggregate")
	// ErrInvalidSnapshot marks a nil SnapshotWrite.
	ErrInvalidSnapshot = errors.New("sim: invalid snapshot write")
	// ErrSnapshotStale is the saver-domain stale-CAS sentinel
	// (spec §8.3.8). T4b maps store.ErrStaleRevision into it at
	// the composition boundary.
	ErrSnapshotStale = errors.New("sim: snapshot stale")
	// ErrSaverReconcileRequired marks MarkDirty/WriteThrough on
	// a reconcile-blocked key: stale-memory snapshots are never
	// silently accepted for later persistence.
	ErrSaverReconcileRequired = errors.New("sim: saver reconcile required")
	// ErrSaverRevisionInvariant marks a writer that returned nil
	// error with newRevision != expectedRevision + 1, or a save
	// that cannot advance past math.MaxInt64: durable state
	// cannot be inferred, so the key reconcile-blocks.
	ErrSaverRevisionInvariant = errors.New("sim: saver revision invariant")
)

// SaverConfig carries the saver's scheduling inputs (spec §8.3.6).
// Interval is the periodic flush period (production: the existing
// config.SnapshotIntervalSeconds as a duration); Clock is the
// existing sim Clock/Ticker seam. OnError is optional.
type SaverConfig struct {
	Interval time.Duration
	Clock    Clock
	OnError  SaverErrorObserver
}

// pendingJob is one queued immutable full snapshot plus the dirty
// generation that produced it.
type pendingJob struct {
	gen   uint64
	write SnapshotWrite
}

// saverEntry is one tracked aggregate's coordination state. The
// dirty generation (seq) is ephemeral: it only tells whether a
// newer dirty snapshot arrived while an older save was in flight.
// It is never persisted or sent anywhere.
type saverEntry struct {
	key      AggregateKey
	known    int64
	blocked  bool
	pending  *pendingJob
	inflight bool
	seq      uint64
	// gate serializes this key's persistence callbacks: exactly
	// ONE saver write owner at a time. Capacity 1, pre-filled;
	// acquire = receive, release = send. Never held with mu.
	gate chan struct{}
}

// Saver is the revision-safe snapshot coordinator (spec §8.3). A
// short mutex guards entry lookup, dirty replacement, and revision
// bookkeeping ONLY; it is never held while a SnapshotWrite runs.
// APIs are safe for concurrent use by Run, MarkDirty, WriteThrough,
// and manual/shutdown flush callers.
type Saver struct {
	mu       sync.Mutex
	entries  map[AggregateKey]*saverEntry
	interval time.Duration
	clock    Clock
	onError  SaverErrorObserver
}

// NewSaver validates its scheduling inputs (interval > 0,
// clock != nil) and returns a saver with no tracked aggregates.
func NewSaver(cfg SaverConfig) (*Saver, error) {
	if cfg.Interval <= 0 {
		return nil, fmt.Errorf("%w: saver interval %v must be > 0", ErrInvalidConfig, cfg.Interval)
	}
	if cfg.Clock == nil {
		return nil, fmt.Errorf("%w: saver clock must not be nil", ErrInvalidConfig)
	}
	return &Saver{
		entries:  make(map[AggregateKey]*saverEntry),
		interval: cfg.Interval,
		clock:    cfg.Clock,
		onError:  cfg.OnError,
	}, nil
}

// Track registers a durable aggregate from its known authoritative
// persisted revision (spec §8.3.1): the ACTUAL revision from PG or
// another already-authoritative operation, never a silent zero for
// unknown state. Negative revisions fail with zero registration.
// ErrInvalidDurableRevision (T3c) is reused: same int64 >= 0 domain.
func (s *Saver) Track(key AggregateKey, knownRevision int64) error {
	if err := key.validate(); err != nil {
		return err
	}
	if knownRevision < 0 {
		return fmt.Errorf("%w: track %v revision %d", ErrInvalidDurableRevision, key, knownRevision)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[key]; ok {
		return fmt.Errorf("%w: %v", ErrAggregateAlreadyTracked, key)
	}
	s.entries[key] = &saverEntry{
		key:   key,
		known: knownRevision,
		gate:  make(chan struct{}, 1),
	}
	s.entries[key].gate <- struct{}{}
	return nil
}

// Untrack removes a clean tracked aggregate. It rejects dirty,
// in-flight, or reconcile-blocked keys so unsaved state is never
// silently abandoned: flush or reconcile first.
func (s *Saver) Untrack(key AggregateKey) error {
	if err := key.validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok {
		return fmt.Errorf("%w: %v", ErrAggregateNotTracked, key)
	}
	if e.blocked || e.pending != nil || e.inflight {
		return fmt.Errorf("%w: %v", ErrSaverUntrackDirty, key)
	}
	delete(s.entries, key)
	return nil
}

// MarkDirty records a COMPLETE immutable snapshot as the latest
// pending state for key (spec §8.3.4): it increments the dirty
// generation, replaces any queued-but-not-in-flight older snapshot,
// and returns promptly without PG I/O. On a reconcile-blocked key
// it returns ErrSaverReconcileRequired and records nothing.
func (s *Saver) MarkDirty(key AggregateKey, snapshot SnapshotWrite) error {
	if snapshot == nil {
		return fmt.Errorf("%w: nil write for %v", ErrInvalidSnapshot, key)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok {
		return fmt.Errorf("%w: %v", ErrAggregateNotTracked, key)
	}
	if e.blocked {
		return fmt.Errorf("%w: %v", ErrSaverReconcileRequired, key)
	}
	if e.seq == math.MaxUint64 {
		return fmt.Errorf("%w: dirty generation exhausted for %v", ErrSaverRevisionInvariant, key)
	}
	e.seq++
	e.pending = &pendingJob{gen: e.seq, write: snapshot}
	return nil
}

// WriteThrough synchronously persists one full snapshot through the
// SAME per-key write gate as periodic saves (spec §8.3.9). It
// allocates a dirty generation for the critical snapshot up front
// so a newer MarkDirty arriving while it waits/runs survives: on
// success, pending snapshots with generation <= the critical
// generation are superseded, newer ones retained. On transient
// failure with no newer pending snapshot, the critical snapshot
// re-enters pending dirty state instead of being lost.
func (s *Saver) WriteThrough(ctx context.Context, key AggregateKey, snapshot SnapshotWrite) (int64, error) {
	if snapshot == nil {
		return 0, fmt.Errorf("%w: nil write for %v", ErrInvalidSnapshot, key)
	}
	s.mu.Lock()
	e, ok := s.entries[key]
	if !ok {
		s.mu.Unlock()
		return 0, fmt.Errorf("%w: %v", ErrAggregateNotTracked, key)
	}
	if e.blocked {
		s.mu.Unlock()
		return 0, fmt.Errorf("%w: %v", ErrSaverReconcileRequired, key)
	}
	if e.seq == math.MaxUint64 {
		s.mu.Unlock()
		return 0, fmt.Errorf("%w: dirty generation exhausted for %v", ErrSaverRevisionInvariant, key)
	}
	e.seq++
	critGen := e.seq
	job := pendingJob{gen: critGen, write: snapshot}
	s.mu.Unlock()

	release, err := e.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer release()

	s.mu.Lock()
	if cur, ok := s.entries[key]; !ok || cur != e {
		s.mu.Unlock()
		return 0, fmt.Errorf("%w: %v", ErrAggregateNotTracked, key)
	}
	if e.blocked {
		s.mu.Unlock()
		return 0, fmt.Errorf("%w: %v", ErrSaverReconcileRequired, key)
	}
	if e.known == math.MaxInt64 {
		e.blocked = true
		s.mu.Unlock()
		return 0, fmt.Errorf("%w: known revision %d cannot advance for %v",
			ErrSaverRevisionInvariant, e.known, key)
	}
	expected := e.known
	e.inflight = true
	s.mu.Unlock()

	newRev, werr := job.write(ctx, expected)

	s.mu.Lock()
	defer s.mu.Unlock()
	e.inflight = false
	if werr != nil {
		if errors.Is(werr, ErrSnapshotStale) {
			e.blocked = true
			return 0, fmt.Errorf("sim: saver write-through %v: %w", key, werr)
		}
		if e.pending == nil || e.pending.gen <= critGen {
			cp := job
			e.pending = &cp
		}
		return 0, fmt.Errorf("sim: saver write-through %v: %w", key, werr)
	}
	if newRev != expected+1 {
		e.blocked = true
		return 0, fmt.Errorf("%w: write-through %v expected %d got %d",
			ErrSaverRevisionInvariant, key, expected+1, newRev)
	}
	e.known = newRev
	if e.pending != nil && e.pending.gen <= critGen {
		e.pending = nil
	}
	return newRev, nil
}

// ResolveReconciled clears a reconcile block ONLY after the owning
// layer reloaded/replaced memory from authoritative PG under the
// T3c mutation fence (spec §8.3.8). It waits for this key's saver
// write ownership, requires authoritativeRevision >= known (no
// backward revision), sets known to it, discards ALL
// pre-reconciliation pending jobs (they describe stale pre-reload
// memory), and marks the aggregate clean.
func (s *Saver) ResolveReconciled(ctx context.Context, key AggregateKey, authoritativeRevision int64) error {
	if authoritativeRevision < 0 {
		return fmt.Errorf("%w: resolve %v revision %d", ErrInvalidDurableRevision, key, authoritativeRevision)
	}
	s.mu.Lock()
	e, ok := s.entries[key]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %v", ErrAggregateNotTracked, key)
	}
	release, err := e.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()

	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.entries[key]; !ok || cur != e {
		return fmt.Errorf("%w: %v", ErrAggregateNotTracked, key)
	}
	if authoritativeRevision < e.known {
		return fmt.Errorf("%w: resolve %v revision %d below known %d",
			ErrSaverRevisionInvariant, key, authoritativeRevision, e.known)
	}
	e.known = authoritativeRevision
	e.blocked = false
	e.pending = nil
	return nil
}

// acquire waits for this entry's write ownership, context-aware. A
// cancelled context runs no writer. The caller MUST call release
// exactly once after successful acquisition, without holding mu.
func (e *saverEntry) acquire(ctx context.Context) (release func(), err error) {
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("sim: saver gate for %v: %w", e.key, ctx.Err())
	case <-e.gate:
		return func() { e.gate <- struct{}{} }, nil
	}
}

// saveOne attempts key's pending snapshot at most once. It acquires
// the per-key gate WITHOUT holding the global mutex, revalidates
// registration/block/pending state under the mutex, releases the
// mutex over I/O, then applies the outcome rules (spec §8.3.7/8.3.8).
func (s *Saver) saveOne(ctx context.Context, key AggregateKey) error {
	s.mu.Lock()
	e, ok := s.entries[key]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %v", ErrAggregateNotTracked, key)
	}
	release, err := e.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()

	s.mu.Lock()
	if cur, ok := s.entries[key]; !ok || cur != e {
		s.mu.Unlock()
		return fmt.Errorf("%w: %v", ErrAggregateNotTracked, key)
	}
	if e.blocked {
		s.mu.Unlock()
		return fmt.Errorf("%w: %v", ErrSaverReconcileRequired, key)
	}
	if e.pending == nil {
		s.mu.Unlock()
		return nil
	}
	if e.known == math.MaxInt64 {
		e.blocked = true
		s.mu.Unlock()
		return fmt.Errorf("%w: known revision %d cannot advance for %v",
			ErrSaverRevisionInvariant, e.known, key)
	}
	job := *e.pending
	e.pending = nil
	e.inflight = true
	expected := e.known
	s.mu.Unlock()

	newRev, werr := job.write(ctx, expected)

	s.mu.Lock()
	defer s.mu.Unlock()
	e.inflight = false
	if werr != nil {
		if errors.Is(werr, ErrSnapshotStale) {
			e.blocked = true
			return fmt.Errorf("sim: saver save %v: %w", key, werr)
		}
		if e.pending == nil {
			cp := job
			e.pending = &cp
		}
		return fmt.Errorf("sim: saver save %v: %w", key, werr)
	}
	if newRev != expected+1 {
		e.blocked = true
		if e.pending == nil {
			cp := job
			e.pending = &cp
		}
		return fmt.Errorf("%w: save %v expected %d got %d",
			ErrSaverRevisionInvariant, key, expected+1, newRev)
	}
	e.known = newRev
	return nil
}

// flushPass captures the currently dirty unblocked key set, sorts it
// canonically, attempts each key at most once, continues to
// unrelated keys after one ordinary failure, and joins errors while
// preserving errors.Is. Context cancellation stops the pass without
// burning further PG calls.
func (s *Saver) flushPass(ctx context.Context) error {
	s.mu.Lock()
	var keys []AggregateKey
	for k, e := range s.entries {
		// Dirty unblocked keys are attempted; reconcile-blocked
		// keys are reported (never written: saveOne returns the
		// reconcile condition without invoking the writer).
		if e.pending != nil || e.blocked {
			keys = append(keys, k)
		}
	}
	s.mu.Unlock()
	sort.Slice(keys, func(i, j int) bool { return keys[i].Less(keys[j]) })
	var errs []error
	for _, k := range keys {
		if err := ctx.Err(); err != nil {
			errs = append(errs, fmt.Errorf("sim: saver flush %v: %w", k, err))
			break
		}
		if err := s.saveOne(ctx, k); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// FlushDirty is the deterministic manual flush (spec §8.3.10): one
// attempt per eligible dirty key, canonical order, errors joined.
func (s *Saver) FlushDirty(ctx context.Context) error {
	return s.flushPass(ctx)
}

// FlushAll is the context-bounded shutdown primitive (spec §8.3.10).
// Caller contract: producer/sim mutation is quiesced first, then
// FlushAll with a deadline context. One attempt per eligible dirty
// key, no infinite retry spin; failures remain dirty and returned.
func (s *Saver) FlushAll(ctx context.Context) error {
	return s.flushPass(ctx)
}

// Run blocks on the saver's ticker: one delivered pulse maps to one
// FlushDirty pass with no catch-up bursts (spec §8.3.6). Transient
// failures go to the optional observer without killing the loop.
// Cancellation performs no extra flush; the ticker stops exactly
// once on exit.
func (s *Saver) Run(ctx context.Context) error {
	t := s.clock.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case tm := <-t.C():
			_ = tm
			if err := s.FlushDirty(ctx); err != nil {
				if ctx.Err() == nil && s.onError != nil {
					s.onError(err)
				}
			}
		}
	}
}

// SaverEntrySnapshot is the immutable inspection result for
// tests/debugging: the known persisted revision, whether a pending
// snapshot waits, whether a save owns the slot, and whether the key
// is reconcile-blocked. No mutable internal pointer escapes.
type SaverEntrySnapshot struct {
	KnownRevision int64
	Dirty         bool
	InFlight      bool
	Blocked       bool
}

// Inspect copies one key's saver state.
func (s *Saver) Inspect(key AggregateKey) (SaverEntrySnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok {
		return SaverEntrySnapshot{}, fmt.Errorf("%w: %v", ErrAggregateNotTracked, key)
	}
	return SaverEntrySnapshot{
		KnownRevision: e.known,
		Dirty:         e.pending != nil,
		InFlight:      e.inflight,
		Blocked:       e.blocked,
	}, nil
}

// TrackedCount returns the number of live saver registrations.
func (s *Saver) TrackedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// DirtyCount returns the number of tracked aggregates holding a
// queued pending snapshot.
func (s *Saver) DirtyCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.entries {
		if e.pending != nil {
			n++
		}
	}
	return n
}
