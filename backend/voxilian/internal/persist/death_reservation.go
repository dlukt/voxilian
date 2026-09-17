package persist

import (
	"errors"
	"fmt"
	"sync"

	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/store"
)

// Guaranteed bounded executor reservation + prepared
// activation seam (spec §9.5.1i, M5-T5c3c3c1): capacity is
// reserved BEFORE the sim owner can enter the irreversible
// DeathPersisting state, so queue saturation can never strand
// a live player in DeathPersisting with no persistence job.
// The existing DeathExecutor.TrySubmit stays
// behavior-compatible; this file adds the explicit
// capacity-reservation API on the SAME executor plus the one
// concrete reservation behind the narrow sim capability.

var (
	// ErrDeathReservationCanceled is the stable
	// executor-domain sentinel delivered to a successfully
	// Prepared reservation's buffered result when it is
	// cancelled before activation. Matched with errors.Is.
	ErrDeathReservationCanceled = errors.New("persist: death reservation canceled")
	// ErrDeathReservationNotPrepared reports Result access
	// before successful Prepare: no result channel exists
	// yet. Matched with errors.Is.
	ErrDeathReservationNotPrepared = errors.New("persist: death reservation not prepared")
)

// Compile-time proof that the concrete persist reservation
// satisfies the narrow store-independent sim capability, so
// future c3c3c2 owner-local orchestration can compose it
// without importing persist into sim (forbidden direction).
var _ sim.ImmediateDeathWorkReservation = (*DeathExecutionReservation)(nil)

// prepareDeathExecutorWork is the ONE private validation /
// mapping / freezing core shared by TrySubmit and
// reservation Prepare (spec §9.5.1i): validate
// PlayerVitalsRuntimeInputs, MapImmediateDeathCapture,
// freeze the Store request, and freeze the future
// ImmediateDeathCompletion payload. No Store call, no Saver
// call, no recovery, no owner mutation. Caller mutation of
// the work value after this returns cannot reach the frozen
// outputs. Every error wraps ErrDeathExecutorInvalid,
// preserving the existing TrySubmit validation ordering
// (invalid work against a non-running executor keeps
// reporting ErrDeathExecutorInvalid, never
// ErrDeathExecutorNotRunning).
func prepareDeathExecutorWork(
	work ImmediateDeathPersistenceWork,
) (store.DeathEntryRequest, sim.ImmediateDeathCompletion, error) {
	var zeroReq store.DeathEntryRequest
	var zeroCompletion sim.ImmediateDeathCompletion
	if err := work.RuntimeInputs.Validate(); err != nil {
		return zeroReq, zeroCompletion, fmt.Errorf("persist: death executor work runtime inputs: %w: %w",
			err, ErrDeathExecutorInvalid)
	}
	req, err := MapImmediateDeathCapture(work.Capture)
	if err != nil {
		return zeroReq, zeroCompletion, fmt.Errorf("persist: death executor work capture: %w: %w",
			err, ErrDeathExecutorInvalid)
	}
	// Defensive second freeze: the mapper already owns its
	// output, and the T5c2b adapter freezes again before
	// callback execution. Freeze here too so the staged work
	// can never alias caller memory even if the mapper drifts.
	req = freezeDeathEntryRequest(req)
	// The future completion template carries token,
	// placement, vitals, runtime inputs, and durable only:
	// the authoritative Pending state is attached at
	// execution time (spec §9.5.1k) — from the committed
	// DeathEntryResult.CorpseID on normal success, or from
	// the recovered PendingDeathSnapshot on proven
	// lost-ack — never from the pre-commit plan alone.
	completion := sim.CloneImmediateDeathCompletion(sim.ImmediateDeathCompletion{
		Token:         work.Capture.Token,
		Placement:     work.Capture.Placement,
		Vitals:        work.Capture.Vitals,
		RuntimeInputs: work.RuntimeInputs,
		Durable:       work.Capture.Durable,
	})
	return req, completion, nil
}

// reservationState is the one-shot reservation lifecycle
// (spec §9.5.1i): no transition goes backwards.
type reservationState uint8

const (
	// reservationReserved owns one queue permit and
	// contains no published executor job yet.
	reservationReserved reservationState = iota
	// reservationPrepared owns one queue permit plus one
	// fully validated/frozen Store request, future owner
	// completion, and buffered result channel, with NOTHING
	// published to workers yet.
	reservationPrepared
	// reservationActivated has handed exactly one frozen job
	// to executor processing, or has definitively resolved
	// its result as executor shutdown when shutdown won the
	// lifecycle race.
	reservationActivated
	// reservationCancelled will execute nothing; owned
	// capacity was returned exactly once.
	reservationCancelled
)

// DeathExecutionReservation is one one-shot bounded capacity
// reservation on a DeathExecutor (spec §9.5.1i). A live
// (reserved or prepared) reservation owns exactly one queue
// permit: for configured QueueCapacity N, the combined count
// of unactivated live reservations plus jobs currently
// occupying the bounded queue never exceeds N. A reservation
// contains no goroutine, no queued job, and no Store
// operation until activation.
//
// Thread safety: methods may be called from different
// goroutines during tests/future composition. One-shot state
// transitions are protected by the reservation mutex: no
// double permit release, no double queue publication, no
// double result send, no panic on repeated Cancel/Activate.
// Lock order is reservation mutex -> executor mutex; the
// executor mutex never acquires the reservation mutex, and
// neither mutex is held while Store executes, recovery
// executes, or owner completion executes.
type DeathExecutionReservation struct {
	ex *DeathExecutor

	mu         sync.Mutex
	state      reservationState
	req        store.DeathEntryRequest
	completion sim.ImmediateDeathCompletion
	res        chan ImmediateDeathPersistenceResult
}

// ReserveImmediateDeath reserves ONE future queued death job
// worth of the executor's existing QueueCapacity (spec
// §9.5.1i). It is non-blocking with order "executor running
// check, then one queue permit attempt": a non-running
// executor reports ErrDeathExecutorNotRunning, a saturated
// executor reports ErrDeathExecutorQueueFull, otherwise one
// live reservation results. A failed reservation publishes
// nothing, allocates no death attempt, and mutates no sim
// state. This is NOT additional capacity.
func (x *DeathExecutor) ReserveImmediateDeath() (*DeathExecutionReservation, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if !x.running {
		return nil, ErrDeathExecutorNotRunning
	}
	if x.permits <= 0 {
		return nil, ErrDeathExecutorQueueFull
	}
	x.permits--
	return &DeathExecutionReservation{ex: x, state: reservationReserved}, nil
}

// releasePermit returns exactly one queue permit. Callers
// must guarantee single ownership (reservation state
// machine, worker dequeue, shutdown drain).
func (x *DeathExecutor) releasePermit() {
	x.mu.Lock()
	x.permits++
	x.mu.Unlock()
}

// PrepareImmediateDeathWork validates, maps, and
// deep-freezes the complete death work through the SAME
// semantics as TrySubmit (spec §9.5.1i): no alternate
// mapper, no alternate request representation, no Store
// call, no Saver call, no recovery, no owner mutation. It is
// safe to run BEFORE PlayerBeginDeathPersistence: nothing is
// published to workers yet.
//
// On success the reservation becomes prepared and privately
// owns the frozen work: later caller mutation of the
// capture Durable Advancement, Spells, Skills, Items,
// Enchants, AffectedItems, Vitals, or runtime inputs cannot
// reach it. On preparation failure the existing wrapped
// validation/mapping error returns, the reservation becomes
// terminal/cancelled, the queue permit is returned exactly
// once, and no job is published — so future c3c3c2 can
// treat "Prepare error == no DeathPersisting transition
// occurred" as a hard invariant.
func (r *DeathExecutionReservation) PrepareImmediateDeathWork(
	capture sim.ImmediateDeathCapture,
	runtimeInputs sim.PlayerVitalsRuntimeInputs,
) error {
	r.mu.Lock()
	if r.state != reservationReserved {
		state := r.state
		r.mu.Unlock()
		if state == reservationCancelled {
			return ErrDeathReservationCanceled
		}
		return fmt.Errorf("persist: death reservation prepare in state %d: %w",
			uint8(state), ErrDeathExecutorInvalid)
	}
	r.mu.Unlock()
	req, completion, err := prepareDeathExecutorWork(ImmediateDeathPersistenceWork{
		Capture:       capture,
		RuntimeInputs: runtimeInputs,
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != reservationReserved {
		// A concurrent Cancel won the race while the bounded
		// CPU work ran: the permit was already returned by
		// Cancel, so drop the prepared work without
		// touching permits or publishing anything.
		if r.state == reservationCancelled {
			return ErrDeathReservationCanceled
		}
		return fmt.Errorf("persist: death reservation prepare in state %d: %w",
			uint8(r.state), ErrDeathExecutorInvalid)
	}
	if err != nil {
		r.state = reservationCancelled
		r.req = store.DeathEntryRequest{}
		r.completion = sim.ImmediateDeathCompletion{}
		// Lock order is reservation mutex -> executor
		// mutex (never the reverse), so releasing the
		// permit here cannot deadlock.
		r.ex.releasePermit()
		return err
	}
	r.state = reservationPrepared
	r.req = req
	r.completion = completion
	r.res = make(chan ImmediateDeathPersistenceResult, 1)
	return nil
}

// ActivateImmediateDeathWork publishes the already-frozen
// prepared work (spec §9.5.1i). It is intentionally a
// no-error capability for future same-owner-turn use: after
// successful preparation, capacity failure is no longer a
// possible caller-visible branch — the held reservation
// permit is the proof that queue capacity exists, so
// activation is incapable of ErrDeathExecutorQueueFull. On a
// normal running executor exactly one frozen job is handed
// to the existing queue (no second queue, no goroutine, no
// second worker pool); activation never performs Store work
// itself. If executor shutdown has already made normal
// execution impossible before activation, activation does
// NOT block and does NOT publish an orphaned job: it
// resolves the reservation's buffered result exactly once
// with ErrDeathExecutorShutdown and returns its permit. A
// repeated Activate after the first activation is a no-op
// and never publishes twice.
func (r *DeathExecutionReservation) ActivateImmediateDeathWork() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != reservationPrepared {
		return
	}
	r.state = reservationActivated
	job := deathExecutorJob{req: r.req, completion: r.completion, res: r.res}
	res := r.res
	x := r.ex
	x.mu.Lock()
	if !x.running {
		x.permits++
		x.mu.Unlock()
		res <- ImmediateDeathPersistenceResult{Err: ErrDeathExecutorShutdown}
		return
	}
	// The held reservation permit becomes the queued job's
	// permit: the free count is unchanged. Queue space is
	// guaranteed by permit accounting, so publication cannot
	// block; the default branch is unreachable defense that
	// still resolves terminally without stranding a waiter.
	select {
	case x.queue <- job:
		x.mu.Unlock()
	default:
		x.permits++
		x.mu.Unlock()
		res <- ImmediateDeathPersistenceResult{Err: ErrDeathExecutorShutdown}
	}
}

// CancelImmediateDeathWork abandons the reservation (spec
// §9.5.1i). It is idempotent: before activation there is no
// Store call, no sink call, and no queue publication, and
// the queue permit is released exactly once. If a result
// channel already exists from successful Prepare,
// cancellation places one definitive terminal result
// carrying ErrDeathReservationCanceled into that buffered
// channel so no observer waits forever. Cancel after
// activation is a no-op: it never retracts an authoritative
// queued/running persistence job. Cancellation stays safe
// after executor shutdown.
func (r *DeathExecutionReservation) CancelImmediateDeathWork() {
	r.mu.Lock()
	if r.state == reservationActivated || r.state == reservationCancelled {
		r.mu.Unlock()
		return
	}
	prepared := r.state == reservationPrepared
	res := r.res
	r.state = reservationCancelled
	r.req = store.DeathEntryRequest{}
	r.completion = sim.ImmediateDeathCompletion{}
	r.mu.Unlock()
	r.ex.releasePermit()
	if prepared {
		res <- ImmediateDeathPersistenceResult{Err: ErrDeathReservationCanceled}
	}
}

// Result returns the reservation's definitive bounded result
// channel without exposing the persist result type through
// the sim interface (spec §9.5.1i). Before successful
// Prepare no result channel is available
// (ErrDeathReservationNotPrepared). After successful Prepare
// the SAME buffered channel returns every time: after
// Activate, after prepared Cancel (receiving
// ErrDeathReservationCanceled), and after
// shutdown-before-activation (receiving
// ErrDeathExecutorShutdown). No new completion queue is
// created and no replacement channel is ever issued.
func (r *DeathExecutionReservation) Result() (<-chan ImmediateDeathPersistenceResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.res == nil {
		return nil, ErrDeathReservationNotPrepared
	}
	return r.res, nil
}
