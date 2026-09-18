package persist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/store"
)

// Bounded off-owner Portal-of-Life persistence executor (spec
// §9.5.1k, M5-T5c3d2b2, frozen v0.3.55): the Store-domain Portal
// mapper, the bounded executor with queue-capacity reservation,
// the concrete PortalOfLifeWorkReservation holding the d2b1
// reserved Saver critical slot from Prepare, exactly-one reserved
// `CriticalSetReservation.Execute` with at-most-once Store, the
// in-critical-callback read-only lost-ack proof, normal /
// proven-lost-ack owner completion, definite-pre-execution owner
// abort, and bounded same-mailbox redelivery. No Underworld-exit
// penalties. A persistence worker NEVER mutates a live sim entity;
// live pending replacement happens ONLY through the typed d2a
// owner completion. Proven lost-ack NEVER calls ReconcileSaver /
// ResolveReconciled and NEVER replays Store.
//
// NOTE (tripwire): the d2b2 production path below MUST NOT contain
// ReconcileSaver or ResolveReconciled. The in-callback recovery is
// read-only LoadDeathCharacterRecovery; the reserved Saver success
// path itself accepts E+1.

// Stable persist-domain Portal errors. Matching MUST use errors.Is,
// never string parsing as control flow. Sim Portal gameplay errors
// are never reused for executor lifecycle failures.
var (
	// ErrPortalExecutorNotRunning reports a reservation against an
	// executor with no Run owning it.
	ErrPortalExecutorNotRunning = errors.New("persist: portal executor not running")
	// ErrPortalExecutorQueueFull reports a non-blocking reservation
	// against saturated bounded queue capacity. Nothing is reserved.
	ErrPortalExecutorQueueFull = errors.New("persist: portal executor queue full")
	// ErrPortalExecutorShutdown reports work that can never execute
	// because the executor shut down before publication or before
	// execution began. Store was never called.
	ErrPortalExecutorShutdown = errors.New("persist: portal executor shutdown")
	// ErrPortalExecutorAlreadyRunning reports a second concurrent Run
	// against an already-running executor.
	ErrPortalExecutorAlreadyRunning = errors.New("persist: portal executor already running")
	// ErrPortalExecutorInvalid reports invalid executor configuration
	// or invalid submitted Portal work.
	ErrPortalExecutorInvalid = errors.New("persist: portal executor invalid")
	// ErrPortalReservationCanceled is the stable sentinel delivered to
	// a successfully Prepared reservation's buffered result when it is
	// cancelled before activation.
	ErrPortalReservationCanceled = errors.New("persist: portal reservation canceled")
	// ErrPortalReservationNotPrepared reports Result or Activate access
	// before successful Prepare: no result channel exists yet.
	ErrPortalReservationNotPrepared = errors.New("persist: portal reservation not prepared")
	// ErrPortalCommitUnproven is the stable sentinel returned when the
	// in-callback materialized state does NOT prove the attempted
	// Portal transaction committed (revision mismatch, content
	// mismatch, pending-row mismatch). NO Store replay, NO owner
	// completion, and — once Store was invoked — NO owner abort: the
	// live Portal attempt remains in flight.
	ErrPortalCommitUnproven = errors.New("persist: portal commit unproven")
)

// PortalExecutionStore is the narrow Portal Store seam the executor
// owns: the Portal write transaction plus the read-only T5c2a
// character recovery loader used ONLY inside the held-gate
// callback. Proven satisfied by *store.PGStore. No unrelated
// death-entry/item persistence dependency.
type PortalExecutionStore interface {
	CommitPortalOfLife(context.Context, store.PortalOfLifeRequest) (store.PortalOfLifeResult, error)
	LoadDeathCharacterRecovery(context.Context, int64) (store.DeathCharacterRecoverySnapshot, error)
}

// Compile-time proof that production PGStore satisfies the Portal
// executor seam (no wrapper, no replacement method).
var _ PortalExecutionStore = (*store.PGStore)(nil)

// PortalOwnerSink is the already-existing typed d2a owner ingress
// only: success completion plus definitive abort on the SAME sim
// mailbox. Satisfied by *sim.Engine. No gateway dependency, no
// CharacterID fallback.
type PortalOwnerSink interface {
	EnqueuePortalOfLifeCompletion(
		context.Context,
		sim.PortalOfLifeCompletion,
	) (sim.PortalCompletionDisposition, error)
	EnqueuePortalOfLifeAbort(
		context.Context,
		sim.PortalAttemptToken,
	) (sim.PortalAbortDisposition, error)
}

// Compile-time proof that the sim Engine is the Portal owner sink
// (the owner remains the ONLY live-state mutator).
var _ PortalOwnerSink = (*sim.Engine)(nil)

// Compile-time proof that the concrete persist reservation satisfies
// the narrow store-independent sim capability, so d2a owner-local
// orchestration can compose it without importing persist into sim
// (forbidden direction).
var _ sim.PortalOfLifeWorkReservation = (*PortalExecutionReservation)(nil)

// defaultPortalExecutorRetryDelay is the deterministic pause between
// ErrSimIngressFull redeliveries of the SAME frozen completion/abort
// on the same fixed worker: bounded resources, no goroutine per
// retry, no busy-spin.
const defaultPortalExecutorRetryDelay = 5 * time.Millisecond

// defaultPortalAbortTimeout bounds one definitive pre-Store abort
// delivery on a non-cancelled executor-owned context.
const defaultPortalAbortTimeout = 2 * time.Second

// PortalExecutorConfig validates worker count, queue capacity, and
// required dependencies. RetryDelay controls the ingress-full
// redelivery pause; AbortTimeout bounds definitive pre-Store abort
// delivery; non-positive selects the defaults. Exact field names
// follow repository conventions.
type PortalExecutorConfig struct {
	Workers       int
	QueueCapacity int
	Store         PortalExecutionStore
	Saver         *sim.Saver
	Sink          PortalOwnerSink
	RetryDelay    time.Duration
	AbortTimeout  time.Duration
}

// PortalPersistenceResult is the one definitive bounded result per
// activated Portal job, observable without owner mutation.
// Recovered=false means normal CommitPortalOfLife acknowledgement;
// Recovered=true means completion followed proven in-callback
// materialized recovery. Aborted=true means a definitive pre-Store
// owner abort was delivered (Store was never called; always paired
// with a terminal shutdown/execution Err). Err != nil means no
// successful owner completion delivery. No mutable Store snapshots
// are exposed through the result.
type PortalPersistenceResult struct {
	Recovered bool
	Delivery  sim.PortalCompletionDisposition
	Aborted   bool
	Err       error
}

// portalExecutionJob is the frozen per-job state: the Store request
// and the frozen Portal capture (token, expected cost, pending
// identity for proof/completion), both fully owned by the executor
// (caller mutation after successful Prepare cannot reach them),
// plus the ALREADY-HELD d2b1 Saver critical reservation carrying
// the Character gate from owner Prepare time, plus the buffered
// result channel (cap 1 so workers never wait for the caller to
// receive).
type portalExecutionJob struct {
	req      store.PortalOfLifeRequest
	capture  sim.PortalOfLifeCapture
	critical *sim.CriticalSetReservation
	res      chan PortalPersistenceResult
}

// PortalExecutor is the fixed bounded Portal-persistence executor:
// fixed worker count, bounded job queue, NO goroutine per Portal, NO
// unbounded queue, NO second completion queue, NO Store replay,
// explicit one-shot Run lifecycle. Construction itself leaks no
// goroutines.
//
// Queue-capacity permits: the executor owns exactly QueueCapacity
// permits. A live (reserved or prepared) PortalExecutionReservation
// owns one permit; a permit is released exactly once when its queued
// job is dequeued by a worker, when its queued job is drained during
// shutdown, when its reservation is cancelled, when its reservation
// preparation fails, or when its activation observes shutdown.
// Running jobs that a worker already dequeued hold no permit. The
// combined count of unactivated live reservations plus jobs currently
// occupying the bounded queue never exceeds QueueCapacity.
type PortalExecutor struct {
	store        PortalExecutionStore
	saver        *sim.Saver
	sink         PortalOwnerSink
	retryDelay   time.Duration
	abortTimeout time.Duration
	queue        chan portalExecutionJob
	permits      int

	mu         sync.Mutex
	running    bool
	started    bool
	numWorkers int
}

// NewPortalExecutor validates configuration and builds a non-running
// executor. Workers start only in Run.
func NewPortalExecutor(cfg PortalExecutorConfig) (*PortalExecutor, error) {
	if cfg.Workers <= 0 {
		return nil, fmt.Errorf("persist: portal executor workers=%d: %w", cfg.Workers, ErrPortalExecutorInvalid)
	}
	if cfg.QueueCapacity <= 0 {
		return nil, fmt.Errorf("persist: portal executor queue capacity=%d: %w", cfg.QueueCapacity, ErrPortalExecutorInvalid)
	}
	if cfg.Store == nil {
		return nil, fmt.Errorf("persist: portal executor nil store: %w", ErrPortalExecutorInvalid)
	}
	if cfg.Saver == nil {
		return nil, fmt.Errorf("persist: portal executor nil saver: %w", ErrPortalExecutorInvalid)
	}
	if cfg.Sink == nil {
		return nil, fmt.Errorf("persist: portal executor nil sink: %w", ErrPortalExecutorInvalid)
	}
	delay := cfg.RetryDelay
	if delay <= 0 {
		delay = defaultPortalExecutorRetryDelay
	}
	abortTimeout := cfg.AbortTimeout
	if abortTimeout <= 0 {
		abortTimeout = defaultPortalAbortTimeout
	}
	return &PortalExecutor{
		store:        cfg.Store,
		saver:        cfg.Saver,
		sink:         cfg.Sink,
		retryDelay:   delay,
		abortTimeout: abortTimeout,
		queue:        make(chan portalExecutionJob, cfg.QueueCapacity),
		permits:      cfg.QueueCapacity,
		numWorkers:   cfg.Workers,
	}, nil
}

// Run owns the fixed workers until ctx is cancelled: it stops
// accepting new reservations, fails every queued-but-not-started job
// with a definitive shutdown terminal result (permit returned, held
// Saver critical reservation cancelled, Store call ZERO, definitive
// owner abort attempted), lets running reserved callbacks observe the
// cancelled execution context, waits for workers, and returns. No
// waiter is stranded; no worker leaks. A second concurrent Run fails.
// The executor is one-shot: any Run after the first Run has
// terminated fails with ErrPortalExecutorShutdown without starting
// workers.
func (x *PortalExecutor) Run(ctx context.Context) error {
	x.mu.Lock()
	if x.running {
		x.mu.Unlock()
		return ErrPortalExecutorAlreadyRunning
	}
	if x.started {
		x.mu.Unlock()
		return ErrPortalExecutorShutdown
	}
	x.running = true
	x.started = true
	x.mu.Unlock()

	var wg sync.WaitGroup
	for range x.numWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			x.worker(ctx)
		}()
	}
	<-ctx.Done()
	x.mu.Lock()
	x.running = false
	x.mu.Unlock()
	for {
		select {
		case job := <-x.queue:
			// One drained queued job returns one queue
			// permit, cancels its held Saver critical
			// reservation (Store call ZERO), attempts the
			// definitive pre-Store owner abort, and
			// resolves terminally.
			x.releasePermit()
			job.critical.Cancel()
			aborted, aerr := x.deliverAbort(ctx, job.capture.Token)
			res := PortalPersistenceResult{Aborted: aborted, Err: ErrPortalExecutorShutdown}
			if !aborted {
				res.Err = errors.Join(ErrPortalExecutorShutdown, aerr)
			}
			job.res <- res
		default:
			wg.Wait()
			return ctx.Err()
		}
	}
}

// ReservePortalOfLife reserves ONE future queued Portal job worth of
// the executor's existing QueueCapacity. It is non-blocking with
// order "executor running check, then one queue permit attempt": a
// non-running executor reports ErrPortalExecutorNotRunning, a
// saturated executor reports ErrPortalExecutorQueueFull, otherwise
// one live reservation results. A failed reservation publishes
// nothing, allocates no Portal attempt, and mutates no sim state.
// This is NOT additional capacity. No Store/Saver work happens at
// Reserve time.
func (x *PortalExecutor) ReservePortalOfLife() (*PortalExecutionReservation, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if !x.running {
		return nil, ErrPortalExecutorNotRunning
	}
	if x.permits <= 0 {
		return nil, ErrPortalExecutorQueueFull
	}
	x.permits--
	return &PortalExecutionReservation{ex: x, state: portalReservationReserved}, nil
}

// releasePermit returns exactly one queue permit. Callers must
// guarantee single ownership (reservation state machine, worker
// dequeue, shutdown drain).
func (x *PortalExecutor) releasePermit() {
	x.mu.Lock()
	x.permits++
	x.mu.Unlock()
}

// portalReservationState is the one-shot reservation lifecycle: no
// transition goes backwards.
type portalReservationState uint8

const (
	// portalReservationReserved owns one queue permit and contains
	// no frozen work and no Saver reservation yet.
	portalReservationReserved portalReservationState = iota
	// portalReservationPrepared owns one queue permit plus the fully
	// validated/frozen Store request, the frozen Portal capture, one
	// live Saver CriticalSetReservation, and the buffered result
	// channel, with NOTHING published to workers yet.
	portalReservationPrepared
	// portalReservationActivated has handed exactly one frozen job to
	// executor processing, or has definitively resolved its result as
	// executor shutdown when shutdown won the lifecycle race.
	portalReservationActivated
	// portalReservationCancelled will execute nothing; owned capacity
	// and any held Saver slot were returned exactly once.
	portalReservationCancelled
)

// PortalExecutionReservation is one one-shot bounded capacity
// reservation on a PortalExecutor. A live (reserved or prepared)
// reservation owns exactly one queue permit. A reservation contains
// no goroutine, no queued job, and no Store operation until
// activation.
//
// Thread safety: methods may be called from different goroutines
// during tests/future composition. One-shot state transitions are
// protected by the reservation mutex: no double permit release, no
// double Saver cancel, no double queue publication, no duplicate
// result send. Lock order is reservation mutex -> executor mutex; the
// executor mutex never acquires the reservation mutex, and neither
// mutex is held while Store executes, recovery executes, or owner
// completion executes.
type PortalExecutionReservation struct {
	ex *PortalExecutor

	mu       sync.Mutex
	state    portalReservationState
	req      store.PortalOfLifeRequest
	capture  sim.PortalOfLifeCapture
	critical *sim.CriticalSetReservation
	res      chan PortalPersistenceResult
}

// PreparePortalOfLifeWork validates, maps, and deep-freezes the
// complete Portal work BEFORE the d2a owner performs portalEpoch++,
// portalInFlight installation, and activation (spec §9.5.1k): no
// alternate mapper, no Store call, no recovery, no owner mutation.
// It performs bounded CPU / in-memory work only, in required order:
//
//  1. validate/map/freeze the PortalOfLifeCapture into the Store
//     request (immutable private ownership);
//  2. acquire Saver.ReserveCriticalSet for the character root (the
//     queue permit was already acquired by ReservePortalOfLife).
//
// Only if BOTH succeed is the reservation prepared: it then owns the
// queue permit, the frozen work, and one live Saver critical slot
// holding the Character's EXISTING per-key gate plus the reserved
// critical generation, so no later WriteThrough, WriteCriticalSet,
// or ResolveReconciled can overtake the prepared Portal operation
// while MarkDirty stays non-blocking.
//
// On mapping failure: the queue permit is released, the reservation
// is terminal, no Saver reservation exists, and the wrapped
// validation error returns. On Saver reservation failure (including
// busy, reconcile-required, revision-invariant): the queue permit is
// released, the reservation is terminal, and the exact/wrapped cause
// returns. Store call count is ZERO on every failure path. Later
// caller mutation of the capture (Advancement, Spells, Skills, Items,
// Enchants, Pending CorpseID, Vitals-source data) cannot reach the
// frozen work.
func (r *PortalExecutionReservation) PreparePortalOfLifeWork(capture sim.PortalOfLifeCapture) error {
	r.mu.Lock()
	if r.state != portalReservationReserved {
		state := r.state
		r.mu.Unlock()
		if state == portalReservationCancelled {
			return ErrPortalReservationCanceled
		}
		return fmt.Errorf("persist: portal reservation prepare in state %d: %w",
			uint8(state), ErrPortalExecutorInvalid)
	}
	r.mu.Unlock()

	// Immutable private access: later caller mutation of the
	// submitted capture cannot reach mapping, freezing, proof, or
	// completion.
	frozen := sim.ClonePortalOfLifeCapture(capture)
	req, err := MapPortalOfLifeCapture(frozen)
	var critical *sim.CriticalSetReservation
	if err == nil {
		req = freezePortalOfLifeRequest(req)
		critical, err = r.ex.saver.ReserveCriticalSet([]sim.AggregateKey{
			{Kind: sim.AggregateCharacter, ID: int64(frozen.Token.CharacterID)},
		})
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != portalReservationReserved {
		// A concurrent Cancel won the race while the bounded CPU
		// work ran: the permit was already returned by Cancel, so
		// drop the work without touching permits or publishing
		// anything. A just-acquired Saver slot is cancelled.
		if critical != nil {
			critical.Cancel()
		}
		if r.state == portalReservationCancelled {
			return ErrPortalReservationCanceled
		}
		return fmt.Errorf("persist: portal reservation prepare in state %d: %w",
			uint8(r.state), ErrPortalExecutorInvalid)
	}
	if err != nil {
		r.state = portalReservationCancelled
		r.req = store.PortalOfLifeRequest{}
		r.capture = sim.PortalOfLifeCapture{}
		// Lock order is reservation mutex -> executor mutex (never
		// the reverse), so releasing the permit here cannot
		// deadlock.
		r.ex.releasePermit()
		return err
	}
	r.state = portalReservationPrepared
	r.req = req
	r.capture = frozen
	r.critical = critical
	r.res = make(chan PortalPersistenceResult, 1)
	return nil
}

// ActivatePortalOfLifeWork publishes the already-prepared work. On a
// normal running executor exactly one frozen job is handed to the
// existing queue (no second queue, no goroutine, no second worker
// pool) and nil returns. It MUST NOT return queue-full: the held
// queue permit proves queue capacity. Repeated successful Activate
// performs no second publication and returns nil.
//
// If executor shutdown won BEFORE publication: the held Saver
// critical reservation is cancelled, the queue permit is released,
// nothing is published, Store calls are ZERO, the buffered result
// resolves terminally with ErrPortalExecutorShutdown, and that same
// shutdown error returns. This is the definitive pre-publication
// error the d2a owner rolls back synchronously in the same owner
// turn (clearing portalInFlight with the epoch kept consumed); no
// typed abort ingress is used for this path.
//
// Activate before successful Prepare reports
// ErrPortalReservationNotPrepared; Activate after Cancel reports
// ErrPortalReservationCanceled.
func (r *PortalExecutionReservation) ActivatePortalOfLifeWork() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch r.state {
	case portalReservationActivated:
		return nil
	case portalReservationCancelled:
		return ErrPortalReservationCanceled
	case portalReservationReserved:
		return ErrPortalReservationNotPrepared
	}
	r.state = portalReservationActivated
	job := portalExecutionJob{req: r.req, capture: r.capture, critical: r.critical, res: r.res}
	res := r.res
	x := r.ex
	x.mu.Lock()
	if !x.running {
		x.permits++
		x.mu.Unlock()
		job.critical.Cancel()
		res <- PortalPersistenceResult{Err: ErrPortalExecutorShutdown}
		return ErrPortalExecutorShutdown
	}
	// The held reservation permit becomes the queued job's permit:
	// the free count is unchanged. Queue space is guaranteed by
	// permit accounting, so publication cannot block; the default
	// branch is unreachable defense that still resolves terminally
	// without stranding a waiter.
	select {
	case x.queue <- job:
		x.mu.Unlock()
		return nil
	default:
		x.permits++
		x.mu.Unlock()
		job.critical.Cancel()
		res <- PortalPersistenceResult{Err: ErrPortalExecutorShutdown}
		return ErrPortalExecutorShutdown
	}
}

// CancelPortalOfLifeWork abandons the reservation. It is idempotent:
// for reserved/prepared state there is no Store call, no sink call,
// and no queue publication; the held Saver critical reservation (if
// present) is cancelled and the queue permit is released exactly
// once. If a result channel already exists from successful Prepare,
// cancellation places one definitive terminal result carrying
// ErrPortalReservationCanceled into that buffered channel so no
// observer waits forever. Cancel after activation is a no-op: it
// never retracts an authoritative queued/running persistence job.
// Cancellation stays safe after executor shutdown.
func (r *PortalExecutionReservation) CancelPortalOfLifeWork() {
	r.mu.Lock()
	if r.state == portalReservationActivated || r.state == portalReservationCancelled {
		r.mu.Unlock()
		return
	}
	prepared := r.state == portalReservationPrepared
	critical := r.critical
	res := r.res
	r.state = portalReservationCancelled
	r.req = store.PortalOfLifeRequest{}
	r.capture = sim.PortalOfLifeCapture{}
	r.critical = nil
	r.mu.Unlock()
	if critical != nil {
		critical.Cancel()
	}
	r.ex.releasePermit()
	if prepared {
		res <- PortalPersistenceResult{Err: ErrPortalReservationCanceled}
	}
}

// Result returns the reservation's definitive bounded result channel
// without exposing the persist result type through the sim interface.
// Before successful Prepare no result channel is available
// (ErrPortalReservationNotPrepared). After successful Prepare the
// SAME buffered channel (capacity 1, never requiring the caller to
// receive for worker progress) returns every time: after Activate,
// after prepared Cancel (receiving ErrPortalReservationCanceled),
// and after shutdown-before-activation (receiving
// ErrPortalExecutorShutdown). No new completion queue is created and
// no replacement channel is ever issued.
func (r *PortalExecutionReservation) Result() (<-chan PortalPersistenceResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.res == nil {
		return nil, ErrPortalReservationNotPrepared
	}
	return r.res, nil
}

func (x *PortalExecutor) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-x.queue:
			// Queue capacity is free the moment a worker
			// dequeues: release exactly this queued job's
			// permit BEFORE Store/recovery execution, then
			// keep the accepted worker cancellation rule
			// below with no double-release. The job's Saver
			// critical reservation continues holding the
			// Character gate.
			x.releasePermit()
			if ctx.Err() != nil {
				// Shutdown won before execution began: NO
				// Store call, cancel the held Saver slot,
				// attempt the definitive typed owner abort.
				job.critical.Cancel()
				aborted, aerr := x.deliverAbort(ctx, job.capture.Token)
				res := PortalPersistenceResult{Aborted: aborted, Err: ErrPortalExecutorShutdown}
				if !aborted {
					res.Err = errors.Join(ErrPortalExecutorShutdown, aerr)
				}
				job.res <- res
				return
			}
			job.res <- x.execute(ctx, job)
		}
	}
}

// execute runs one frozen job to its definitive bounded result: normal
// Store acknowledgement, proven in-callback materialized recovery, or
// a fail-closed error. It NEVER replays Store and NEVER mutates live
// sim state. The worker executes exactly ONE reserved
// CriticalSetReservation.Execute (no ordinary Saver.WriteCriticalSet,
// no CommitPortalOfLife reacquiring the wrong coordination): the
// callback receives the exact execution-time expected revision E,
// sets ExpectedRevision = E, and calls Store AT MOST ONCE.
func (x *PortalExecutor) execute(
	ctx context.Context,
	job portalExecutionJob,
) PortalPersistenceResult {
	var (
		completion   sim.PortalOfLifeCompletion
		recovered    bool
		storeInvoked bool
	)
	charKey := sim.AggregateKey{Kind: sim.AggregateCharacter, ID: job.req.Character.ID}
	_, err := job.critical.Execute(ctx, func(
		cbCtx context.Context, expected []sim.AggregateRevision,
	) ([]sim.AggregateRevision, error) {
		charRev, ok := revisionByKey(expected, charKey)
		if !ok {
			return nil, fmt.Errorf("persist: portal execute: missing character revision for %v", charKey)
		}
		storeInvoked = true
		call := job.req
		call.Character.ExpectedRevision = charRev
		res, serr := x.store.CommitPortalOfLife(cbCtx, call)
		if serr == nil {
			// Nominal Store success must still satisfy the
			// Portal result contract; an impossible result
			// takes the same in-callback proof path because
			// the transaction may nevertheless have
			// committed.
			if res.CharacterRevision != charRev+1 ||
				res.EffectiveCost != int16(job.capture.ExpectedEffectiveCost) {
				rec, rerr := x.store.LoadDeathCharacterRecovery(cbCtx, job.req.Character.ID)
				if rerr != nil {
					return nil, fmt.Errorf("persist: portal inconsistent success character=%d: %w",
						job.req.Character.ID, rerr)
				}
				if perr := provePortalCommit(job, charRev, rec, nil); perr != nil {
					return nil, perr
				}
				c, cerr := recoveredPortalCompletion(job.capture, rec.Pending)
				if cerr != nil {
					return nil, cerr
				}
				completion = c
				recovered = true
				return []sim.AggregateRevision{{Key: charKey, Revision: charRev + 1}}, nil
			}
			c, cerr := normalPortalCompletion(job.capture, res)
			if cerr != nil {
				return nil, cerr
			}
			completion = c
			recovered = false
			return []sim.AggregateRevision{{Key: charKey, Revision: res.CharacterRevision}}, nil
		}
		// ANY Store error after invocation: DO NOT replay Store.
		// While STILL inside the held-gate callback, load the
		// read-only materialized recovery into worker-local data.
		rec, rerr := x.store.LoadDeathCharacterRecovery(cbCtx, job.req.Character.ID)
		if rerr != nil {
			return nil, fmt.Errorf("persist: portal commit character=%d: %w",
				job.req.Character.ID, errors.Join(rerr, serr))
		}
		if perr := provePortalCommit(job, charRev, rec, serr); perr != nil {
			return nil, perr
		}
		c, cerr := recoveredPortalCompletion(job.capture, rec.Pending)
		if cerr != nil {
			return nil, cerr
		}
		completion = c
		recovered = true
		return []sim.AggregateRevision{{Key: charKey, Revision: charRev + 1}}, nil
	})
	if err != nil {
		if !storeInvoked {
			// The reserved Execute failed before callback
			// invocation (cancellation won first): Store was
			// NEVER invoked, so the definitive typed owner
			// abort is permitted. Never abort merely because
			// Store returned an error.
			aborted, aerr := x.deliverAbort(ctx, job.capture.Token)
			res := PortalPersistenceResult{Aborted: aborted, Err: err}
			if !aborted {
				res.Err = errors.Join(err, aerr)
			}
			return res
		}
		// Store was invoked (or the callback ran far enough to
		// cross the Store boundary): fail closed. The callback
		// error conservatively blocks the Saver; NO owner success
		// completion, NO Store replay, and NEVER an owner abort —
		// even if recovery appears unchanged. The live Portal
		// attempt remains in flight; reconnect/authoritative
		// recovery is the escape hatch.
		return PortalPersistenceResult{Err: err}
	}
	return x.deliver(ctx, completion, recovered)
}

// normalPortalCompletion builds the owner completion for a valid
// Store success WITHOUT a PG reload: the frozen captured pending
// with the authoritative result cost, PortalUsed = true, and a copy
// of the target CorpseID. The recovered Character root is never
// applied over current live gameplay state — only this pending-only
// completion is later delivered. The completion token is the frozen
// capture token.
func normalPortalCompletion(
	capture sim.PortalOfLifeCapture,
	res store.PortalOfLifeResult,
) (sim.PortalOfLifeCompletion, error) {
	pending := capture.PendingBefore
	pending.EffectiveCost = int(res.EffectiveCost)
	pending.PortalUsed = true
	corpseID := capture.TargetCorpseID
	pending.CorpseID = &corpseID
	if err := sim.ValidatePendingDeathRuntime(&pending); err != nil {
		return sim.PortalOfLifeCompletion{}, fmt.Errorf("persist: portal normal completion: %w", err)
	}
	return sim.PortalOfLifeCompletion{Token: capture.Token, Pending: pending}, nil
}

// recoveredPortalCompletion builds the owner completion for a proven
// lost-ack from the RECOVERED pending row through the shared recovery
// mapper: authoritative EffectiveCost, DeathTimeSeconds, PortalUsed,
// and optional CorpseID. It never substitutes the original non-nil
// CorpseID when recovery says nil. The recovered Character root is
// proof only and is never applied over live gameplay state.
func recoveredPortalCompletion(
	capture sim.PortalOfLifeCapture,
	recovered *store.PendingDeathSnapshot,
) (sim.PortalOfLifeCompletion, error) {
	pending, err := MapPendingDeathRecovery(sim.CharacterID(capture.Token.CharacterID), recovered)
	if err != nil {
		return sim.PortalOfLifeCompletion{}, fmt.Errorf("persist: portal recovered completion: %w", err)
	}
	if pending == nil {
		return sim.PortalOfLifeCompletion{}, fmt.Errorf(
			"persist: portal recovered completion character=%d missing pending: %w",
			int64(capture.Token.CharacterID), ErrPortalCommitUnproven)
	}
	return sim.PortalOfLifeCompletion{Token: capture.Token, Pending: *pending}, nil
}

// provePortalCommit conservatively classifies whether the attempted
// Portal transaction is PROVEN committed, while still inside the
// held-gate callback:
//
//   - character root revision MUST equal exactly E+1 (not >=);
//   - recovered Character root MUST semantically equal the frozen
//     intended Portal Character snapshot, excluding ExpectedRevision
//     (reusing equalDeathCharacterContent: never raw JSON bytes);
//   - recovered Pending MUST be non-nil with the exact character,
//     death time, expected cost, and PortalUsed == true; CorpseID nil
//     is VALID (expiry may NULL it after commit), else it must equal
//     the capture target; no other corpse lookup.
//
// Any deviation returns ErrPortalCommitUnproven (preserving the
// original Store error in the cause chain where applicable): the
// callback then returns error, the Saver stays conservatively
// blocked, and no ReconcileSaver / ResolveReconciled / Store replay
// ever runs on this path.
func provePortalCommit(
	job portalExecutionJob,
	charRev int64,
	rec store.DeathCharacterRecoverySnapshot,
	commitErr error,
) error {
	unproven := func(format string, args ...any) error {
		msg := fmt.Sprintf("persist: portal commit unproven: "+format, args...)
		if commitErr != nil {
			return fmt.Errorf("%s: %w", msg, errors.Join(ErrPortalCommitUnproven, commitErr))
		}
		return fmt.Errorf("%s: %w", msg, ErrPortalCommitUnproven)
	}
	if rec.Character.ExpectedRevision != charRev+1 {
		return unproven("character %d revision %d, want expected+1 %d",
			job.req.Character.ID, rec.Character.ExpectedRevision, charRev+1)
	}
	if err := equalDeathCharacterContent(job.req.Character, rec.Character); err != nil {
		var dm *deathProofMismatch
		if errors.As(err, &dm) {
			return unproven("character %d content: %s", job.req.Character.ID, dm.msg)
		}
		return fmt.Errorf("persist: portal proof character %d: %w", job.req.Character.ID, err)
	}
	pending := rec.Pending
	if pending == nil {
		return unproven("character %d missing pending row", job.req.Character.ID)
	}
	if pending.CharacterID != job.req.Character.ID {
		return unproven("pending character %d, want %d", pending.CharacterID, job.req.Character.ID)
	}
	if pending.DeathTimeSeconds != job.capture.PendingBefore.DeathTimeSeconds {
		return unproven("pending death time %d, want %d",
			pending.DeathTimeSeconds, job.capture.PendingBefore.DeathTimeSeconds)
	}
	if pending.EffectiveCost != int16(job.capture.ExpectedEffectiveCost) {
		return unproven("pending cost %d, want %d",
			pending.EffectiveCost, job.capture.ExpectedEffectiveCost)
	}
	if !pending.PortalUsed {
		return unproven("pending portal unused, want used")
	}
	if pending.CorpseID != nil && *pending.CorpseID != job.capture.TargetCorpseID {
		return unproven("pending corpse %d, want %d", *pending.CorpseID, job.capture.TargetCorpseID)
	}
	return nil
}

// deliver sends the SAME frozen completion through typed owner
// ingress. A nil-error Applied is success; a nil-error Duplicate is
// ALSO success (the expected idempotent redelivery result — no second
// live-state apply occurs). If delivery returns ErrSimIngressFull the
// PG transaction is already authoritative, so the SAME frozen
// completion (with the SAME token) is retried/redelivered on the same
// fixed worker with context-aware bounded retry: no Store replay, no
// second PG recovery, no goroutine per retry, no busy-spin.
// Engine-stop, unknown entity, attempt-mismatch, handoff, and
// payload/install validation errors are terminal: the delivery error
// returns with no CharacterID fallback lookup and no live-state
// mutation from the worker. The committed PG state remains
// authoritative for reconnect/restart recovery.
func (x *PortalExecutor) deliver(
	ctx context.Context,
	completion sim.PortalOfLifeCompletion,
	recovered bool,
) PortalPersistenceResult {
	for {
		disp, err := x.sink.EnqueuePortalOfLifeCompletion(ctx, completion)
		if err == nil {
			return PortalPersistenceResult{Recovered: recovered, Delivery: disp}
		}
		if !errors.Is(err, sim.ErrSimIngressFull) {
			return PortalPersistenceResult{Err: err}
		}
		timer := time.NewTimer(x.retryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return PortalPersistenceResult{Err: ctx.Err()}
		case <-timer.C:
		}
	}
}

// deliverAbort delivers one definitive pre-Store typed owner abort
// with the exact frozen token. It runs ONLY when the executor knows
// Store.CommitPortalOfLife was NEVER invoked (queued shutdown
// discard, pre-execution cancellation, pre-callback Execute
// failure) — never merely because Store returned an error. Successful
// owner results (Aborted, Duplicate) both count as definitive
// success. Delivery uses a short bounded non-cancelled context owned
// by the executor (never an unbounded Background wait): only
// ErrSimIngressFull retries within the bounded window, with no
// goroutine per abort and no background delivery after Run returns.
// Engine-stop, token-mismatch, and admission errors are terminal. If
// abort delivery ultimately fails, the returned error reports it;
// Store still was never called.
func (x *PortalExecutor) deliverAbort(
	parent context.Context,
	token sim.PortalAttemptToken,
) (bool, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), x.abortTimeout)
	defer cancel()
	for {
		disp, err := x.sink.EnqueuePortalOfLifeAbort(ctx, token)
		if err == nil {
			switch disp {
			case sim.PortalAbortAborted, sim.PortalAbortDuplicate:
				return true, nil
			default:
				return false, fmt.Errorf(
					"persist: portal abort disposition %d: %w", uint8(disp), ErrPortalExecutorInvalid)
			}
		}
		if !errors.Is(err, sim.ErrSimIngressFull) {
			return false, err
		}
		timer := time.NewTimer(x.retryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, ctx.Err()
		case <-timer.C:
		}
	}
}

// freezePortalOfLifeRequest deep-freezes a mapped Portal request into
// immutable staged ownership: the character snapshot via the exact
// freezeCharacterSnapshot rule. Scalar corpse/cost fields copy
// normally. ExpectedRevision stays the placeholder until the reserved
// Saver callback overwrites it from the execution-time revision.
func freezePortalOfLifeRequest(req store.PortalOfLifeRequest) store.PortalOfLifeRequest {
	frozen := req
	frozen.Character = freezeCharacterSnapshot(req.Character)
	return frozen
}

// MapPortalOfLifeCapture maps an already-resolved sim Portal capture
// to the existing store.PortalOfLifeRequest. Character mapping: ID =
// token CharacterID, ExpectedRevision = 0 placeholder (the reserved
// Saver callback remains the ONLY layer injecting the execution-time
// revision), Karma/Flags/Spells/Skills/Advancement = the complete
// CURRENT captured durable state, position = CURRENT capture
// position in int64 millimeters (existing conversion semantics),
// Vitals = JSON encoding of CURRENT capture vitals. CorpseID = the
// target corpse, ProposedCost = the resolved proposal (never persist
// ExpectedEffectiveCost directly). Every mapper error returns the
// ZERO request. The mapper owns its output bytes/slices
// independently of the sim capture (the request freezer remains the
// final defensive freeze before queue publication). No Store/PG call.
func MapPortalOfLifeCapture(capture sim.PortalOfLifeCapture) (store.PortalOfLifeRequest, error) {
	var zero store.PortalOfLifeRequest
	fail := func(format string, args ...any) (store.PortalOfLifeRequest, error) {
		return zero, fmt.Errorf("persist: map portal capture "+format+": %w",
			append(args, sim.ErrInvalidDeathInput)...)
	}
	if capture.Token.EntityID == 0 {
		return fail("entity id=0")
	}
	charID := int64(capture.Token.CharacterID)
	if charID <= 0 {
		return fail("character id=%d", charID)
	}
	if capture.Token.Epoch == 0 {
		return fail("portal epoch=0")
	}
	if err := capture.Vitals.Validate(); err != nil {
		return zero, fmt.Errorf("persist: map portal capture vitals: %w", err)
	}
	posX, posY, posZ, err := storePosition(
		capture.Position.X, capture.Position.Y, capture.Position.Z)
	if err != nil {
		return zero, fmt.Errorf("persist: map portal capture position: %w", err)
	}
	if len(capture.Durable.Advancement) == 0 || !json.Valid(capture.Durable.Advancement) {
		return fail("advancement is not valid JSON")
	}
	vitalsJSON, err := json.Marshal(capture.Vitals)
	if err != nil {
		return zero, fmt.Errorf("persist: map portal capture vitals encode: %w", err)
	}
	spells, err := mapPortalAbilities("spell", capture.Durable.Spells)
	if err != nil {
		return zero, err
	}
	skills, err := mapPortalAbilities("skill", capture.Durable.Skills)
	if err != nil {
		return zero, err
	}
	if err := sim.ValidatePendingDeathRuntime(&capture.PendingBefore); err != nil {
		return zero, fmt.Errorf("persist: map portal capture pending: %w", err)
	}
	if capture.PendingBefore.PortalUsed {
		return fail("pending portal already used")
	}
	if capture.PendingBefore.CorpseID == nil {
		return fail("pending corpse is nil")
	}
	if capture.TargetCorpseID <= 0 {
		return fail("target corpse id=%d", capture.TargetCorpseID)
	}
	if *capture.PendingBefore.CorpseID != capture.TargetCorpseID {
		return fail("target corpse %d vs pending %d",
			capture.TargetCorpseID, *capture.PendingBefore.CorpseID)
	}
	if capture.ProposedCost < 5 || capture.ProposedCost > 80 {
		return fail("proposed cost=%d", capture.ProposedCost)
	}
	expected, err := sim.ReducePendingDeathCost(capture.PendingBefore.EffectiveCost, capture.ProposedCost)
	if err != nil {
		return zero, fmt.Errorf("persist: map portal capture expected cost: %w", err)
	}
	if capture.ExpectedEffectiveCost != expected {
		return fail("expected effective cost=%d, want lowers-only %d",
			capture.ExpectedEffectiveCost, expected)
	}

	storeSpells := make([]store.CharacterSpellSnapshot, 0, len(spells))
	for _, sp := range spells {
		storeSpells = append(storeSpells, store.CharacterSpellSnapshot{
			SpellID: sp.ID, Ability: sp.Ability, AtrophyFlag: sp.AtrophyFlag,
		})
	}
	storeSkills := make([]store.CharacterSkillSnapshot, 0, len(skills))
	for _, sk := range skills {
		storeSkills = append(storeSkills, store.CharacterSkillSnapshot{
			SkillID: sk.ID, Ability: sk.Ability, AtrophyFlag: sk.AtrophyFlag,
		})
	}
	return store.PortalOfLifeRequest{
		Character: store.CharacterSnapshot{
			ID:               charID,
			ExpectedRevision: 0,
			Karma:            capture.Durable.Karma,
			PosX:             posX,
			PosY:             posY,
			PosZ:             posZ,
			Vitals:           vitalsJSON,
			Advancement:      append([]byte(nil), capture.Durable.Advancement...),
			Flags:            capture.Durable.Flags,
			Spells:           storeSpells,
			Skills:           storeSkills,
		},
		CorpseID:     capture.TargetCorpseID,
		ProposedCost: int16(capture.ProposedCost),
	}, nil
}

// mapPortalAbilities validates one durable ability namespace for the
// Portal mapper (catalog IDs 1..65535, ability 1..99, no duplicates
// within the namespace; spell and skill namespaces are independent)
// and returns an independent copy. Every error returns nil plus the
// error for the caller to convert to the zero request.
func mapPortalAbilities(
	kind string,
	list []sim.PlayerAbilityState,
) ([]sim.PlayerAbilityState, error) {
	out := make([]sim.PlayerAbilityState, 0, len(list))
	seen := make(map[int32]struct{}, len(list))
	for _, a := range list {
		if a.ID < 1 || a.ID > 65535 {
			return nil, fmt.Errorf(
				"persist: map portal capture %s id=%d outside catalog domain: %w",
				kind, a.ID, sim.ErrInvalidDeathInput)
		}
		if a.Ability < 1 || a.Ability > 99 {
			return nil, fmt.Errorf(
				"persist: map portal capture %s id=%d ability=%d: %w",
				kind, a.ID, a.Ability, sim.ErrInvalidDeathInput)
		}
		if _, dup := seen[a.ID]; dup {
			return nil, fmt.Errorf(
				"persist: map portal capture duplicate %s id=%d: %w",
				kind, a.ID, sim.ErrInvalidDeathInput)
		}
		seen[a.ID] = struct{}{}
		out = append(out, a)
	}
	return out, nil
}
