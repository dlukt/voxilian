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

// Bounded off-owner Underworld-exit penalty persistence executor
// (spec §9.5.1k, M5-T5c3d3b, frozen v0.3.57): the Store-domain
// penalty mapper, the bounded executor with queue-capacity plus
// Saver-critical-slot reservation, the concrete
// PenaltyExecutionReservation holding the d3b reserved Saver
// critical slot from Reserve, exactly-one reserved
// `CriticalSetReservation.Execute` with at-most-once Store, the
// in-critical-callback read-only lost-ack proof (E+1 + exact
// Character content + Pending == nil), normal / proven-lost-ack
// owner completion, definitive-pre-Store owner retryable
// notification, and bounded same-mailbox redelivery. No Portal
// work. A persistence worker NEVER mutates a live sim entity;
// live post-penalty installation happens ONLY through the typed
// d3a owner completion. Proven lost-ack NEVER calls
// ReconcileSaver / ResolveReconciled and NEVER replays Store.

// Stable persist-domain penalty errors. Matching MUST use
// errors.Is, never string parsing as control flow. Sim penalty
// gameplay errors are never reused for executor lifecycle
// failures.
var (
	// ErrPenaltyExecutorNotRunning reports a reservation against
	// an executor with no Run owning it.
	ErrPenaltyExecutorNotRunning = errors.New("persist: penalty executor not running")
	// ErrPenaltyExecutorQueueFull reports a non-blocking
	// reservation against saturated bounded queue capacity.
	// Nothing is reserved.
	ErrPenaltyExecutorQueueFull = errors.New("persist: penalty executor queue full")
	// ErrPenaltyExecutorShutdown reports work that can never
	// execute because the executor shut down before publication
	// or before execution began. Store was never called.
	ErrPenaltyExecutorShutdown = errors.New("persist: penalty executor shutdown")
	// ErrPenaltyExecutorAlreadyRunning reports a second
	// concurrent Run against an already-running executor.
	ErrPenaltyExecutorAlreadyRunning = errors.New("persist: penalty executor already running")
	// ErrPenaltyExecutorInvalid reports invalid executor
	// configuration or invalid submitted penalty work.
	ErrPenaltyExecutorInvalid = errors.New("persist: penalty executor invalid")
	// ErrPenaltyReservationCanceled is the stable sentinel
	// delivered to a successfully Prepared reservation's
	// buffered result when it is cancelled before activation.
	ErrPenaltyReservationCanceled = errors.New("persist: penalty reservation canceled")
	// ErrPenaltyReservationNotPrepared reports Result or
	// Activate access before successful Prepare: no result
	// channel exists yet.
	ErrPenaltyReservationNotPrepared = errors.New("persist: penalty reservation not prepared")
	// ErrDeathPenaltyCommitUnproven is the stable sentinel
	// returned when the in-callback materialized state does NOT
	// prove the attempted penalty transaction committed
	// (revision mismatch, content mismatch, pending row still
	// present). NO Store replay, NO owner completion, and —
	// once the reserved callback was invoked — NO owner
	// retryable notification: the live penalty attempt remains
	// in flight.
	ErrDeathPenaltyCommitUnproven = errors.New("persist: death penalty commit unproven")
)

// PenaltyExecutionStore is the narrow penalty Store seam the
// executor owns: the penalty write transaction plus the read-only
// T5c2a character recovery loader used ONLY inside the held-gate
// callback. Proven satisfied by *store.PGStore. No unrelated
// death-entry/item persistence dependency.
type PenaltyExecutionStore interface {
	CommitDeathPenalties(context.Context, store.DeathPenaltiesRequest) (store.DeathPenaltiesResult, error)
	LoadDeathCharacterRecovery(context.Context, int64) (store.DeathCharacterRecoverySnapshot, error)
}

// Compile-time proof that production PGStore satisfies the
// penalty executor seam (no wrapper, no replacement method).
var _ PenaltyExecutionStore = (*store.PGStore)(nil)

// PenaltyOwnerSink is the already-existing typed d3a owner
// ingress only: success completion plus definitive pre-Store
// retryable notification on the SAME sim mailbox. Satisfied by
// *sim.Engine. No gateway dependency, no CharacterID fallback.
type PenaltyOwnerSink interface {
	EnqueueDeathPenaltyCompletion(
		context.Context,
		sim.DeathPenaltyCompletion,
	) (sim.DeathPenaltyCompletionDisposition, error)
	EnqueueDeathPenaltyPersistenceRetryable(
		context.Context,
		sim.DeathPenaltyAttemptToken,
	) (sim.DeathPenaltyRetryDisposition, error)
}

// Compile-time proof that the sim Engine is the penalty owner
// sink (the owner remains the ONLY live-state mutator).
var _ PenaltyOwnerSink = (*sim.Engine)(nil)

// Compile-time proof that the concrete persist executor and
// reservation satisfy the narrow store-independent sim
// capabilities, so d3a owner-local orchestration can compose
// them without importing persist into sim (forbidden
// direction).
var _ sim.DeathPenaltyWorkProvider = (*PenaltyExecutor)(nil)
var _ sim.DeathPenaltyWorkReservation = (*PenaltyExecutionReservation)(nil)

// defaultPenaltyExecutorRetryDelay is the deterministic pause
// between ErrSimIngressFull redeliveries of the SAME frozen
// completion/retryable on the same fixed worker: bounded
// resources, no goroutine per retry, no busy-spin.
const defaultPenaltyExecutorRetryDelay = 5 * time.Millisecond

// defaultPenaltyRetryTimeout bounds one definitive pre-Store
// retryable delivery on a non-cancelled executor-owned context.
const defaultPenaltyRetryTimeout = 2 * time.Second

// PenaltyExecutorConfig validates worker count, queue capacity,
// and required dependencies. RetryDelay controls the
// ingress-full redelivery pause; RetryTimeout bounds definitive
// pre-Store retryable delivery; non-positive selects the
// defaults. Exact field names follow repository conventions.
type PenaltyExecutorConfig struct {
	Workers       int
	QueueCapacity int
	Store         PenaltyExecutionStore
	Saver         *sim.Saver
	Sink          PenaltyOwnerSink
	RetryDelay    time.Duration
	RetryTimeout  time.Duration
}

// PenaltyPersistenceResult is the one definitive bounded result
// per activated penalty job, observable without owner mutation.
// Recovered=false means normal CommitDeathPenalties
// acknowledgement; Recovered=true means completion followed
// proven in-callback materialized recovery. RetryNotified=true
// means a definitive pre-Store owner retryable notification was
// delivered (Store was never called; always paired with a
// terminal shutdown/execution Err). Err != nil means no
// successful owner completion delivery. No mutable Store
// snapshots are exposed through the result.
type PenaltyPersistenceResult struct {
	Recovered     bool
	Delivery      sim.DeathPenaltyCompletionDisposition
	RetryNotified bool
	RetryDelivery sim.DeathPenaltyRetryDisposition
	Err           error
}

// penaltyExecutionJob is the frozen per-job state: the Store
// request and the frozen penalty capture (token plus the
// intended post-penalty content for proof/completion), both
// fully owned by the executor (caller mutation after successful
// Prepare cannot reach them), plus the ALREADY-HELD d3b Saver
// critical reservation carrying the Character gate from Reserve
// time, plus the buffered result channel (cap 1 so workers never
// wait for the caller to receive).
type penaltyExecutionJob struct {
	req      store.DeathPenaltiesRequest
	capture  sim.DeathPenaltyCapture
	critical *sim.CriticalSetReservation
	res      chan PenaltyPersistenceResult
}

// PenaltyExecutor is the fixed bounded penalty-persistence
// executor: fixed worker count, bounded job queue, NO goroutine
// per penalty, NO unbounded queue, NO second completion queue,
// NO Store replay, explicit one-shot Run lifecycle.
// Construction itself leaks no goroutines.
//
// Queue-capacity permits: the executor owns exactly
// QueueCapacity permits. A live (reserved or prepared)
// PenaltyExecutionReservation owns one permit; a permit is
// released exactly once when its queued job is dequeued by a
// worker, when its queued job is drained during shutdown, when
// its reservation is cancelled, when its reservation
// preparation fails, or when its activation observes shutdown.
// Running jobs that a worker already dequeued hold no permit.
// The combined count of unactivated live reservations plus jobs
// currently occupying the bounded queue never exceeds
// QueueCapacity.
type PenaltyExecutor struct {
	store        PenaltyExecutionStore
	saver        *sim.Saver
	sink         PenaltyOwnerSink
	retryDelay   time.Duration
	retryTimeout time.Duration
	queue        chan penaltyExecutionJob
	permits      int

	mu         sync.Mutex
	running    bool
	started    bool
	numWorkers int
}

// NewPenaltyExecutor validates configuration and builds a
// non-running executor. Workers start only in Run.
func NewPenaltyExecutor(cfg PenaltyExecutorConfig) (*PenaltyExecutor, error) {
	if cfg.Workers <= 0 {
		return nil, fmt.Errorf("persist: penalty executor workers=%d: %w", cfg.Workers, ErrPenaltyExecutorInvalid)
	}
	if cfg.QueueCapacity <= 0 {
		return nil, fmt.Errorf("persist: penalty executor queue capacity=%d: %w", cfg.QueueCapacity, ErrPenaltyExecutorInvalid)
	}
	if cfg.Store == nil {
		return nil, fmt.Errorf("persist: penalty executor nil store: %w", ErrPenaltyExecutorInvalid)
	}
	if cfg.Saver == nil {
		return nil, fmt.Errorf("persist: penalty executor nil saver: %w", ErrPenaltyExecutorInvalid)
	}
	if cfg.Sink == nil {
		return nil, fmt.Errorf("persist: penalty executor nil sink: %w", ErrPenaltyExecutorInvalid)
	}
	delay := cfg.RetryDelay
	if delay <= 0 {
		delay = defaultPenaltyExecutorRetryDelay
	}
	retryTimeout := cfg.RetryTimeout
	if retryTimeout <= 0 {
		retryTimeout = defaultPenaltyRetryTimeout
	}
	return &PenaltyExecutor{
		store:        cfg.Store,
		saver:        cfg.Saver,
		sink:         cfg.Sink,
		retryDelay:   delay,
		retryTimeout: retryTimeout,
		queue:        make(chan penaltyExecutionJob, cfg.QueueCapacity),
		permits:      cfg.QueueCapacity,
		numWorkers:   cfg.Workers,
	}, nil
}

// Run owns the fixed workers until ctx is cancelled: it stops
// accepting new reservations, fails every queued-but-not-started
// job with a definitive shutdown terminal result (permit
// returned, held Saver critical reservation cancelled, Store
// call ZERO, definitive owner retryable notification
// attempted), lets running reserved callbacks observe the
// cancelled execution context, waits for workers, and returns.
// No waiter is stranded; no worker leaks. A second concurrent
// Run fails. The executor is one-shot: any Run after the first
// Run has terminated fails with ErrPenaltyExecutorShutdown
// without starting workers.
func (x *PenaltyExecutor) Run(ctx context.Context) error {
	x.mu.Lock()
	if x.running {
		x.mu.Unlock()
		return ErrPenaltyExecutorAlreadyRunning
	}
	if x.started {
		x.mu.Unlock()
		return ErrPenaltyExecutorShutdown
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
			// definitive pre-Store owner retryable
			// notification, and resolves terminally.
			x.releasePermit()
			job.critical.Cancel()
			disp, rerr := x.deliverRetryable(ctx, job.capture.Token)
			res := PenaltyPersistenceResult{RetryNotified: rerr == nil, RetryDelivery: disp, Err: ErrPenaltyExecutorShutdown}
			if rerr != nil {
				res.Err = errors.Join(ErrPenaltyExecutorShutdown, rerr)
			}
			job.res <- res
		default:
			wg.Wait()
			return ctx.Err()
		}
	}
}

// ReserveDeathPenaltyWork synchronously and non-blockingly owns
// BOTH scarce resources BEFORE the d3a owner may run RNG (spec
// §9.5.1k, frozen v0.3.57): one bounded executor queue permit
// PLUS one Saver ReserveCriticalSet slot for the Character root
// (exactly one participant). Acquisition order is queue permit
// first, Saver slot second. If the Saver reservation fails the
// queue permit is released, nothing is published, Store calls
// are ZERO, and the owner stays untouched (this failure occurs
// before d3a RNG). A failed reservation publishes nothing,
// allocates no penalty attempt, and mutates no sim state. This
// is NOT additional capacity. No Store work happens at Reserve
// time.
func (x *PenaltyExecutor) ReserveDeathPenaltyWork(characterID sim.CharacterID) (sim.DeathPenaltyWorkReservation, error) {
	charID := int64(characterID)
	if charID <= 0 {
		return nil, fmt.Errorf("persist: penalty reserve character id=%d: %w", charID, ErrPenaltyExecutorInvalid)
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if !x.running {
		return nil, ErrPenaltyExecutorNotRunning
	}
	if x.permits <= 0 {
		return nil, ErrPenaltyExecutorQueueFull
	}
	x.permits--
	critical, err := x.saver.ReserveCriticalSet([]sim.AggregateKey{
		{Kind: sim.AggregateCharacter, ID: charID},
	})
	if err != nil {
		x.permits++
		return nil, err
	}
	return &PenaltyExecutionReservation{ex: x, state: penaltyReservationReserved, charID: charID, critical: critical}, nil
}

// releasePermit returns exactly one queue permit. Callers must
// guarantee single ownership (reservation state machine, worker
// dequeue, shutdown drain).
func (x *PenaltyExecutor) releasePermit() {
	x.mu.Lock()
	x.permits++
	x.mu.Unlock()
}

// penaltyReservationState is the one-shot reservation lifecycle:
// no transition goes backwards.
type penaltyReservationState uint8

const (
	// penaltyReservationReserved owns one queue permit plus one
	// live Saver CriticalSetReservation for the Character root
	// and contains no frozen work yet.
	penaltyReservationReserved penaltyReservationState = iota
	// penaltyReservationPrepared owns one queue permit, the held
	// Saver slot, the fully validated/frozen Store request, the
	// frozen penalty capture, and the buffered result channel,
	// with NOTHING published to workers yet.
	penaltyReservationPrepared
	// penaltyReservationActivated has handed exactly one frozen
	// job to executor processing, or has definitively resolved
	// its result as executor shutdown when shutdown won the
	// lifecycle race.
	penaltyReservationActivated
	// penaltyReservationCancelled will execute nothing; owned
	// capacity and the held Saver slot were returned exactly
	// once.
	penaltyReservationCancelled
)

// PenaltyExecutionReservation is one one-shot bounded capacity
// reservation on a PenaltyExecutor. A live (reserved or
// prepared) reservation owns exactly one queue permit plus the
// held Character Saver critical slot acquired at Reserve time.
// A reservation contains no goroutine, no queued job, and no
// Store operation until activation.
//
// Thread safety: methods may be called from different goroutines
// during tests/future composition. One-shot state transitions
// are protected by the reservation mutex: no double permit
// release, no double Saver cancel, no double queue publication,
// no duplicate result send. Lock order is reservation mutex ->
// executor mutex; the executor mutex never acquires the
// reservation mutex, and neither mutex is held while Store
// executes, recovery executes, or owner completion executes.
type PenaltyExecutionReservation struct {
	ex *PenaltyExecutor

	mu       sync.Mutex
	state    penaltyReservationState
	charID   int64
	req      store.DeathPenaltiesRequest
	capture  sim.DeathPenaltyCapture
	critical *sim.CriticalSetReservation
	res      chan PenaltyPersistenceResult
}

// PrepareDeathPenaltyWork performs CPU/in-memory work ONLY after
// the owner already installed the private attempt (life is
// already PlayerLifeDeathPenaltyPersisting): clone the capture,
// map it through MapDeathPenaltyCapture, and freeze the Store
// request plus the capture. It performs NO Saver reservation
// (the Character slot was acquired at Reserve time, before RNG),
// NO queue capacity acquisition, NO Store call, NO PG, NO
// recovery, and NO owner mutation.
//
// On Prepare failure: Store calls are ZERO, nothing is
// published, the held Saver slot is cancelled and the queue
// permit released exactly once, and the error returns. The d3a
// caller retains its frozen owner attempt and may retry later
// with the exact plan. Repeated Cancel stays safe: no double
// permit return, no double Saver cancel.
func (r *PenaltyExecutionReservation) PrepareDeathPenaltyWork(capture sim.DeathPenaltyCapture) error {
	r.mu.Lock()
	if r.state != penaltyReservationReserved {
		state := r.state
		r.mu.Unlock()
		if state == penaltyReservationCancelled {
			return ErrPenaltyReservationCanceled
		}
		return fmt.Errorf("persist: penalty reservation prepare in state %d: %w",
			uint8(state), ErrPenaltyExecutorInvalid)
	}
	r.mu.Unlock()

	// Immutable private access: later caller mutation of the
	// submitted capture cannot reach mapping, freezing, proof,
	// or completion.
	frozen := sim.CloneDeathPenaltyCapture(capture)
	var req store.DeathPenaltiesRequest
	var err error
	if int64(frozen.Token.CharacterID) != r.charID {
		err = fmt.Errorf("persist: penalty prepare character %d vs reserved %d: %w",
			int64(frozen.Token.CharacterID), r.charID, ErrPenaltyExecutorInvalid)
	} else {
		req, err = MapDeathPenaltyCapture(frozen)
		if err == nil {
			req = freezePenaltyRequest(req)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != penaltyReservationReserved {
		// A concurrent Cancel won the race while the bounded
		// CPU work ran: the permit was already returned and
		// the Saver slot already cancelled by Cancel, so drop
		// the work without touching permits, Saver slots, or
		// publishing anything.
		if r.state == penaltyReservationCancelled {
			return ErrPenaltyReservationCanceled
		}
		return fmt.Errorf("persist: penalty reservation prepare in state %d: %w",
			uint8(r.state), ErrPenaltyExecutorInvalid)
	}
	if err != nil {
		critical := r.critical
		r.state = penaltyReservationCancelled
		r.req = store.DeathPenaltiesRequest{}
		r.capture = sim.DeathPenaltyCapture{}
		r.critical = nil
		// Lock order is reservation mutex -> executor mutex
		// (never the reverse), so releasing the permit here
		// cannot deadlock.
		if critical != nil {
			critical.Cancel()
		}
		r.ex.releasePermit()
		return err
	}
	r.state = penaltyReservationPrepared
	r.req = req
	r.capture = frozen
	r.res = make(chan PenaltyPersistenceResult, 1)
	return nil
}

// ActivateDeathPenaltyWork publishes the already-prepared work.
// A successfully prepared reservation already proves capacity,
// so Activate MUST NOT return queue-full. On a normal running
// executor exactly one frozen job is handed to the existing
// queue (no second queue, no goroutine, no second worker pool)
// and nil returns. Repeated successful Activate performs no
// second publication and returns nil.
//
// If executor shutdown won BEFORE publication: the held Saver
// critical reservation is cancelled, the queue permit is
// released, nothing is published, Store calls are ZERO, the
// buffered result resolves terminally with
// ErrPenaltyExecutorShutdown, and that same shutdown error
// returns. No typed retryable notification is sent for this
// synchronous Activate failure: d3a itself receives the error
// in the same owner turn and already keeps life locked with the
// exact frozen capture and persistenceActive == false.
//
// Activate before successful Prepare reports
// ErrPenaltyReservationNotPrepared; Activate after Cancel
// reports ErrPenaltyReservationCanceled.
func (r *PenaltyExecutionReservation) ActivateDeathPenaltyWork() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch r.state {
	case penaltyReservationActivated:
		return nil
	case penaltyReservationCancelled:
		return ErrPenaltyReservationCanceled
	case penaltyReservationReserved:
		return ErrPenaltyReservationNotPrepared
	}
	r.state = penaltyReservationActivated
	job := penaltyExecutionJob{req: r.req, capture: r.capture, critical: r.critical, res: r.res}
	res := r.res
	x := r.ex
	x.mu.Lock()
	if !x.running {
		x.permits++
		x.mu.Unlock()
		job.critical.Cancel()
		res <- PenaltyPersistenceResult{Err: ErrPenaltyExecutorShutdown}
		return ErrPenaltyExecutorShutdown
	}
	// The held reservation permit becomes the queued job's
	// permit: the free count is unchanged. Queue space is
	// guaranteed by permit accounting, so publication cannot
	// block; the default branch is unreachable defense that
	// still resolves terminally without stranding a waiter.
	select {
	case x.queue <- job:
		x.mu.Unlock()
		return nil
	default:
		x.permits++
		x.mu.Unlock()
		job.critical.Cancel()
		res <- PenaltyPersistenceResult{Err: ErrPenaltyExecutorShutdown}
		return ErrPenaltyExecutorShutdown
	}
}

// CancelDeathPenaltyWork abandons the reservation. It is
// idempotent: for reserved/prepared state there is no Store
// call, no sink call, and no queue publication; the held Saver
// critical reservation (if present) is cancelled and the queue
// permit is released exactly once. If a result channel already
// exists from successful Prepare, cancellation places one
// definitive terminal result carrying
// ErrPenaltyReservationCanceled into that buffered channel so no
// observer waits forever. Cancel after activation is a no-op: it
// never retracts an authoritative queued/running persistence
// job. Cancellation stays safe after executor shutdown.
func (r *PenaltyExecutionReservation) CancelDeathPenaltyWork() {
	r.mu.Lock()
	if r.state == penaltyReservationActivated || r.state == penaltyReservationCancelled {
		r.mu.Unlock()
		return
	}
	prepared := r.state == penaltyReservationPrepared
	critical := r.critical
	res := r.res
	r.state = penaltyReservationCancelled
	r.req = store.DeathPenaltiesRequest{}
	r.capture = sim.DeathPenaltyCapture{}
	r.critical = nil
	r.mu.Unlock()
	if critical != nil {
		critical.Cancel()
	}
	r.ex.releasePermit()
	if prepared {
		res <- PenaltyPersistenceResult{Err: ErrPenaltyReservationCanceled}
	}
}

// Result returns the reservation's definitive bounded result
// channel without exposing the persist result type through the
// sim interface. Before successful Prepare no result channel is
// available (ErrPenaltyReservationNotPrepared). After successful
// Prepare the SAME buffered channel (capacity 1, never
// requiring the caller to receive for worker progress) returns
// every time: after Activate, after prepared Cancel (receiving
// ErrPenaltyReservationCanceled), and after
// shutdown-before-activation (receiving
// ErrPenaltyExecutorShutdown). No new completion queue is
// created and no replacement channel is ever issued.
func (r *PenaltyExecutionReservation) Result() (<-chan PenaltyPersistenceResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.res == nil {
		return nil, ErrPenaltyReservationNotPrepared
	}
	return r.res, nil
}

func (x *PenaltyExecutor) worker(ctx context.Context) {
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
				// attempt the definitive typed owner
				// retryable notification.
				job.critical.Cancel()
				disp, rerr := x.deliverRetryable(ctx, job.capture.Token)
				res := PenaltyPersistenceResult{RetryNotified: rerr == nil, RetryDelivery: disp, Err: ErrPenaltyExecutorShutdown}
				if rerr != nil {
					res.Err = errors.Join(ErrPenaltyExecutorShutdown, rerr)
				}
				job.res <- res
				return
			}
			job.res <- x.execute(ctx, job)
		}
	}
}

// execute runs one frozen job to its definitive bounded result:
// normal Store acknowledgement, proven in-callback materialized
// recovery, or a fail-closed error. It NEVER replays Store and
// NEVER mutates live sim state. The worker executes exactly ONE
// reserved CriticalSetReservation.Execute (no ordinary
// Saver.WriteCriticalSet, no CommitDeathPenalties reacquiring
// the wrong coordination): the callback receives the exact
// execution-time expected revision E, sets ExpectedRevision =
// E, and calls Store AT MOST ONCE.
func (x *PenaltyExecutor) execute(
	ctx context.Context,
	job penaltyExecutionJob,
) PenaltyPersistenceResult {
	var (
		completion      sim.DeathPenaltyCompletion
		recovered       bool
		callbackInvoked bool
	)
	charKey := sim.AggregateKey{Kind: sim.AggregateCharacter, ID: job.req.Character.ID}
	_, err := job.critical.Execute(ctx, func(
		cbCtx context.Context, expected []sim.AggregateRevision,
	) ([]sim.AggregateRevision, error) {
		callbackInvoked = true
		charRev, ok := revisionByKey(expected, charKey)
		if !ok {
			return nil, fmt.Errorf("persist: penalty execute: missing character revision for %v", charKey)
		}
		call := job.req
		call.Character.ExpectedRevision = charRev
		res, serr := x.store.CommitDeathPenalties(cbCtx, call)
		if serr == nil {
			// Nominal Store success must still satisfy the
			// penalty result contract; an impossible result
			// takes the same in-callback proof path because
			// the transaction may nevertheless have
			// committed. No second Store call.
			if res.CharacterRevision != charRev+1 {
				rec, rerr := x.store.LoadDeathCharacterRecovery(cbCtx, job.req.Character.ID)
				if rerr != nil {
					return nil, fmt.Errorf("persist: penalty inconsistent success character=%d: %w",
						job.req.Character.ID, rerr)
				}
				if perr := provePenaltyCommit(job, charRev, rec, nil); perr != nil {
					return nil, perr
				}
				completion = sim.DeathPenaltyCompletion{Token: job.capture.Token}
				recovered = true
				return []sim.AggregateRevision{{Key: charKey, Revision: charRev + 1}}, nil
			}
			completion = sim.DeathPenaltyCompletion{Token: job.capture.Token}
			recovered = false
			return []sim.AggregateRevision{{Key: charKey, Revision: res.CharacterRevision}}, nil
		}
		// ANY Store error after invocation: DO NOT replay
		// Store. While STILL inside the held-gate callback,
		// load the read-only materialized recovery into
		// worker-local data.
		rec, rerr := x.store.LoadDeathCharacterRecovery(cbCtx, job.req.Character.ID)
		if rerr != nil {
			return nil, fmt.Errorf("persist: penalty commit character=%d: %w",
				job.req.Character.ID, errors.Join(rerr, serr))
		}
		if perr := provePenaltyCommit(job, charRev, rec, serr); perr != nil {
			return nil, perr
		}
		completion = sim.DeathPenaltyCompletion{Token: job.capture.Token}
		recovered = true
		return []sim.AggregateRevision{{Key: charKey, Revision: charRev + 1}}, nil
	})
	if err != nil {
		if !callbackInvoked {
			// The reserved Execute failed before callback
			// invocation (cancellation won first): Store was
			// NEVER invoked, so the definitive typed owner
			// retryable notification is permitted. The
			// retryable criterion is the RESERVED CALLBACK
			// never being invoked — never merely that
			// Store was not reached.
			disp, rerr := x.deliverRetryable(ctx, job.capture.Token)
			res := PenaltyPersistenceResult{RetryNotified: rerr == nil, RetryDelivery: disp, Err: err}
			if rerr != nil {
				res.Err = errors.Join(err, rerr)
			}
			return res
		}
		// The callback was invoked (Store may or may not have
		// been reached): fail closed. The callback error
		// conservatively blocks the Saver; NO owner success
		// completion, NO Store replay, and NEVER an owner
		// retryable notification — even if recovery appears
		// unchanged. The live penalty attempt remains in
		// flight; reconnect/authoritative recovery is the
		// escape hatch.
		return PenaltyPersistenceResult{Err: err}
	}
	return x.deliver(ctx, completion, recovered)
}

// provePenaltyCommit conservatively classifies whether the
// attempted penalty transaction is PROVEN committed, while
// still inside the held-gate callback:
//
//   - character root revision MUST equal exactly E+1 (not >=);
//   - recovered Character root MUST semantically equal the
//     frozen intended post-penalty Character snapshot,
//     excluding ExpectedRevision (reusing
//     equalDeathCharacterContent: never raw JSON bytes);
//   - recovered Pending MUST be nil: the exactly-once
//     consumption/deletion proof. Old CorpseID, PortalUsed,
//     EffectiveCost, and DeathTimeSeconds are NEVER inspected
//     after the deletion proof — they no longer exist because
//     the pending row must be absent.
//
// Any deviation returns ErrDeathPenaltyCommitUnproven
// (preserving the original Store error in the cause chain where
// applicable): the callback then returns error, the Saver stays
// conservatively blocked, and no Store replay ever runs on this
// path.
func provePenaltyCommit(
	job penaltyExecutionJob,
	charRev int64,
	rec store.DeathCharacterRecoverySnapshot,
	commitErr error,
) error {
	unproven := func(format string, args ...any) error {
		msg := fmt.Sprintf("persist: penalty commit unproven: "+format, args...)
		if commitErr != nil {
			return fmt.Errorf("%s: %w", msg, errors.Join(ErrDeathPenaltyCommitUnproven, commitErr))
		}
		return fmt.Errorf("%s: %w", msg, ErrDeathPenaltyCommitUnproven)
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
		return fmt.Errorf("persist: penalty proof character %d: %w", job.req.Character.ID, err)
	}
	if rec.Pending != nil {
		return unproven("character %d pending still present %+v, want nil (exactly-once deletion)",
			job.req.Character.ID, rec.Pending)
	}
	return nil
}

// deliver sends the SAME frozen token-only completion through
// typed owner ingress. A nil-error Applied is success; a
// nil-error Duplicate is ALSO success (the expected idempotent
// redelivery result — no second live-state apply occurs). If
// delivery returns ErrSimIngressFull the PG transaction is
// already authoritative, so the SAME frozen completion (with the
// SAME token) is retried/redelivered on the same fixed worker
// with context-aware bounded retry: no Store replay, no second
// PG recovery, no goroutine per retry, no busy-spin.
// Engine-stop, unknown entity, attempt-mismatch, handoff, and
// payload/install validation errors are terminal: the delivery
// error returns with no CharacterID fallback lookup and no
// live-state mutation from the worker. The committed PG state
// remains authoritative for reconnect/restart recovery.
func (x *PenaltyExecutor) deliver(
	ctx context.Context,
	completion sim.DeathPenaltyCompletion,
	recovered bool,
) PenaltyPersistenceResult {
	for {
		disp, err := x.sink.EnqueueDeathPenaltyCompletion(ctx, completion)
		if err == nil {
			return PenaltyPersistenceResult{Recovered: recovered, Delivery: disp}
		}
		if !errors.Is(err, sim.ErrSimIngressFull) {
			return PenaltyPersistenceResult{Err: err}
		}
		timer := time.NewTimer(x.retryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return PenaltyPersistenceResult{Err: ctx.Err()}
		case <-timer.C:
		}
	}
}

// deliverRetryable delivers one definitive pre-Store typed owner
// retryable notification with the exact frozen token. It runs
// ONLY when the executor knows the reserved callback was NEVER
// invoked (queued shutdown discard, pre-execution cancellation,
// pre-callback Execute failure) — never merely because Store
// returned an error, and NEVER after callback invocation.
// Successful owner results (Applied, Duplicate) both count as
// definitive success. Delivery uses a short bounded
// non-cancelled context owned by the executor (never an
// unbounded Background wait): only ErrSimIngressFull retries
// within the bounded window, with no goroutine per retryable
// and no background delivery after Run returns. Engine-stop,
// token-mismatch, and admission errors are terminal. If
// retryable delivery ultimately fails, the returned error
// reports it; Store still was never called.
func (x *PenaltyExecutor) deliverRetryable(
	parent context.Context,
	token sim.DeathPenaltyAttemptToken,
) (sim.DeathPenaltyRetryDisposition, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), x.retryTimeout)
	defer cancel()
	for {
		disp, err := x.sink.EnqueueDeathPenaltyPersistenceRetryable(ctx, token)
		if err == nil {
			switch disp {
			case sim.DeathPenaltyRetryApplied, sim.DeathPenaltyRetryDuplicate:
				return disp, nil
			default:
				return disp, fmt.Errorf(
					"persist: penalty retryable disposition %d: %w", uint8(disp), ErrPenaltyExecutorInvalid)
			}
		}
		if !errors.Is(err, sim.ErrSimIngressFull) {
			return sim.DeathPenaltyRetryApplied, err
		}
		timer := time.NewTimer(x.retryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return sim.DeathPenaltyRetryApplied, ctx.Err()
		case <-timer.C:
		}
	}
}

// freezePenaltyRequest deep-freezes a mapped penalty request
// into immutable staged ownership: the character snapshot via
// the exact freezeCharacterSnapshot rule. ExpectedRevision stays
// the placeholder until the reserved Saver callback overwrites
// it from the execution-time revision.
func freezePenaltyRequest(req store.DeathPenaltiesRequest) store.DeathPenaltiesRequest {
	frozen := req
	frozen.Character = freezeCharacterSnapshot(req.Character)
	return frozen
}

// MapDeathPenaltyCapture maps an already-resolved sim penalty
// capture to the existing store.DeathPenaltiesRequest. Character
// mapping: ID = token CharacterID, ExpectedRevision = 0
// placeholder (the reserved Saver callback remains the ONLY
// layer injecting the execution-time revision),
// Karma/Flags/Spells/Skills/Advancement = the complete
// POST-penalty captured durable state, position = CURRENT
// capture position in int64 millimeters (existing conversion
// semantics), Vitals = JSON encoding of CURRENT capture vitals.
// ExpectedPendingCost = the RAW durable pending cost
// int16(capture.PendingBefore.EffectiveCost) — NEVER
// capture.Plan.ScaledCost. Every mapper error returns the ZERO
// request. The mapper owns its output bytes/slices
// independently of the sim capture (the request freezer remains
// the final defensive freeze before queue publication) and never
// sorts the owner capture. No Store/PG call.
func MapDeathPenaltyCapture(capture sim.DeathPenaltyCapture) (store.DeathPenaltiesRequest, error) {
	var zero store.DeathPenaltiesRequest
	fail := func(format string, args ...any) (store.DeathPenaltiesRequest, error) {
		return zero, fmt.Errorf("persist: map penalty capture "+format+": %w",
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
		return fail("penalty epoch=0")
	}
	if err := capture.Vitals.Validate(); err != nil {
		return zero, fmt.Errorf("persist: map penalty capture vitals: %w", err)
	}
	posX, posY, posZ, err := storePosition(
		capture.Position.X, capture.Position.Y, capture.Position.Z)
	if err != nil {
		return zero, fmt.Errorf("persist: map penalty capture position: %w", err)
	}
	if len(capture.Durable.Advancement) == 0 || !json.Valid(capture.Durable.Advancement) {
		return fail("advancement is not valid JSON")
	}
	vitalsJSON, err := json.Marshal(capture.Vitals)
	if err != nil {
		return zero, fmt.Errorf("persist: map penalty capture vitals encode: %w", err)
	}
	spells, err := mapPenaltyAbilities("spell", capture.Durable.Spells)
	if err != nil {
		return zero, err
	}
	skills, err := mapPenaltyAbilities("skill", capture.Durable.Skills)
	if err != nil {
		return zero, err
	}
	// PendingBefore carries the pre-consumption pending value:
	// CorpseID (nil or not) and PortalUsed (either way) NEVER
	// gate Underworld-exit consumption. Only the value domain
	// validates.
	if err := sim.ValidatePendingDeathRuntime(&capture.PendingBefore); err != nil {
		return zero, fmt.Errorf("persist: map penalty capture pending: %w", err)
	}
	if capture.PendingBefore.EffectiveCost < 0 || capture.PendingBefore.EffectiveCost > 100 {
		return fail("pending cost=%d outside store domain", capture.PendingBefore.EffectiveCost)
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
	return store.DeathPenaltiesRequest{
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
		// The RAW durable pending cost: the T5b2b transaction
		// verifies this value against the pending row, never
		// the newbie-scaled plan cost.
		ExpectedPendingCost: int16(capture.PendingBefore.EffectiveCost),
	}, nil
}

// mapPenaltyAbilities validates one durable ability namespace
// for the penalty mapper (catalog IDs 1..65535, ability 1..99,
// no duplicates within the namespace; spell and skill
// namespaces are independent) and returns an independent copy.
// Every error returns nil plus the error for the caller to
// convert to the zero request.
func mapPenaltyAbilities(
	kind string,
	list []sim.PlayerAbilityState,
) ([]sim.PlayerAbilityState, error) {
	out := make([]sim.PlayerAbilityState, 0, len(list))
	seen := make(map[int32]struct{}, len(list))
	for _, a := range list {
		if a.ID < 1 || a.ID > 65535 {
			return nil, fmt.Errorf(
				"persist: map penalty capture %s id=%d outside catalog domain: %w",
				kind, a.ID, sim.ErrInvalidDeathInput)
		}
		if a.Ability < 1 || a.Ability > 99 {
			return nil, fmt.Errorf(
				"persist: map penalty capture %s id=%d ability=%d: %w",
				kind, a.ID, a.Ability, sim.ErrInvalidDeathInput)
		}
		if _, dup := seen[a.ID]; dup {
			return nil, fmt.Errorf(
				"persist: map penalty capture duplicate %s id=%d: %w",
				kind, a.ID, sim.ErrInvalidDeathInput)
		}
		seen[a.ID] = struct{}{}
		out = append(out, a)
	}
	return out, nil
}
