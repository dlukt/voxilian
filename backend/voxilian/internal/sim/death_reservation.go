package sim

// Immediate-death work reservation capability (spec §9.5.1i,
// M5-T5c3c3c1): the ONE narrow store-independent interface
// future c3c3c2 owner-local orchestration uses to guarantee
// bounded persistence capacity BEFORE the irreversible
// owner-local transition to DeathPersisting. `internal/sim`
// only: no Store type, no persist type, no revision, no PG
// handle, no result-channel type from persist, no arbitrary
// func/closure. All three methods are non-blocking with
// respect to PG/network/disk: Prepare may perform bounded CPU
// validation/mapping/freezing and short mutex/channel
// bookkeeping only; Activate and Cancel never do Store/PG
// work. The concrete *persist.DeathExecutionReservation
// satisfies this interface (compile-time proof lives in
// internal/persist, which may import sim; sim MUST NOT import
// persist).
//
// Intended owner-turn composition (owned by future c3c3c2,
// NOT implemented here):
//
// ```text
//  1. obtain/reserve capacity before lifecycle mutation
//  2. construct the complete capture
//  3. call Prepare while still Alive
//  4. only if Prepare succeeds:
//     enter DeathPersisting
//  5. call Activate in the SAME owner turn
//
// ```
type ImmediateDeathWorkReservation interface {
	// PrepareImmediateDeathWork validates, maps, and
	// deep-freezes the complete death work while the player
	// is still Alive. On success the reservation owns the
	// frozen work privately and the caller may proceed to
	// the irreversible lifecycle transition; on failure the
	// reservation is terminal with capacity returned and no
	// DeathPersisting transition may occur.
	PrepareImmediateDeathWork(
		capture ImmediateDeathCapture,
		runtimeInputs PlayerVitalsRuntimeInputs,
	) error

	// ActivateImmediateDeathWork publishes the already-frozen
	// prepared work to executor processing. It is a no-error
	// capability: after successful preparation, capacity
	// failure is no longer a possible caller-visible branch.
	// A repeated Activate after the first activation is a
	// no-op and never publishes twice.
	ActivateImmediateDeathWork()

	// CancelImmediateDeathWork abandons the reservation
	// before activation with no Store call, no sink call,
	// and no queue publication, returning owned capacity
	// exactly once. It is idempotent; cancel after
	// activation is a no-op that never retracts an
	// authoritative queued/running persistence job.
	CancelImmediateDeathWork()
}
