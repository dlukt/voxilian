package sim

import (
	"context"
	"errors"
	"sync"

	"github.com/dlukt/voxilian/internal/world"
)

// SimIngressCapacity is the exact bounded owner-mailbox capacity
// (spec §5.2.10): 256 commands per Engine. No per-entity or
// per-command-type capacity exists.
const SimIngressCapacity = 256

// Stable ingress/run-ownership errors (spec §5.2.10). Match with
// errors.Is, never string parsing.
var (
	// ErrSimIngressFull marks an Enqueue* admission against a full
	// owner mailbox: nothing published, nothing mutated, no inputSeq
	// consumed. Bounded gateway-to-owner overload.
	ErrSimIngressFull = errors.New("sim: ingress mailbox full")
	// ErrEngineNotRunning marks an Enqueue* admission while no Run
	// owns the engine (before the first Run or after it exited).
	ErrEngineNotRunning = errors.New("sim: engine not running")
	// ErrEngineAlreadyRunning marks a second concurrent Run against
	// an engine that already has an owner.
	ErrEngineAlreadyRunning = errors.New("sim: engine already running")
	// ErrEngineStopped marks a queued-but-not-executed command failed
	// by Run teardown. No command survives into a later Run
	// generation.
	ErrEngineStopped = errors.New("sim: engine stopped")
)

// ingressCommand is the private typed command union the owner
// mailbox carries (spec §5.2.10 + §9.5.1h + §9.5.1k + §9.5.1l):
// exactly generic add, player add, remove, move,
// immediate-death completion, the two Portal-of-Life commands,
// the two penalty commands, the T5c4 respawn release, and the
// T5c4 atomic player-recovery bootstrap. Gateway-facing code can
// never submit arbitrary closures: there is no func(*Engine)
// command.
type ingressCommand interface {
	// execute runs the command on the sim owner goroutine and
	// delivers its definitive result. It never blocks on the caller:
	// every result channel is buffered with capacity 1.
	execute(e *Engine)
	// fail reports teardown without execution (Run exit drain).
	// Exactly one of execute/fail runs per admitted command.
	fail(err error)
}

// ingressAddResult is the typed completion of one add command.
type ingressAddResult struct {
	snap EntitySnapshot
	err  error
}

type ingressAdd struct {
	pos world.Vec3
	res chan ingressAddResult
}

func (c ingressAdd) execute(e *Engine) {
	snap, err := e.AddEntity(c.pos)
	c.res <- ingressAddResult{snap: snap, err: err}
}

func (c ingressAdd) fail(err error) {
	c.res <- ingressAddResult{err: err}
}

// ingressAddPlayer is the typed player-add owner command
// (spec §9.5.1d): it carries already-resolved creation values only —
// no lookup, no recovery — and the owner executes the normal
// AddPlayerEntity path.
type ingressAddPlayer struct {
	characterID   CharacterID
	pos           world.Vec3
	vitals        PlayerVitals
	runtimeInputs PlayerVitalsRuntimeInputs
	res           chan ingressAddResult
}

func (c ingressAddPlayer) execute(e *Engine) {
	snap, err := e.AddPlayerEntity(c.characterID, c.pos, c.vitals, c.runtimeInputs)
	c.res <- ingressAddResult{snap: snap, err: err}
}

func (c ingressAddPlayer) fail(err error) {
	c.res <- ingressAddResult{err: err}
}

type ingressRemove struct {
	id  EntityID
	res chan error
}

func (c ingressRemove) execute(e *Engine) {
	c.res <- e.RemoveEntity(c.id)
}

func (c ingressRemove) fail(err error) {
	c.res <- err
}

// ingressMoveResult is the typed completion of one move command,
// preserving the exact owner-local SubmitMove semantics.
type ingressMoveResult struct {
	disp MoveDisposition
	err  error
}

type ingressMove struct {
	id     EntityID
	intent MoveIntent
	res    chan ingressMoveResult
}

func (c ingressMove) execute(e *Engine) {
	disp, err := e.SubmitMove(c.id, c.intent)
	c.res <- ingressMoveResult{disp: disp, err: err}
}

func (c ingressMove) fail(err error) {
	c.res <- ingressMoveResult{err: err}
}

// ingressDeathCompletionResult is the typed completion of one
// immediate-death completion command, preserving the exact
// owner-local PlayerAcceptImmediateDeathCompletion semantics
// (Applied vs Duplicate vs error).
type ingressDeathCompletionResult struct {
	snap EntitySnapshot
	disp DeathCompletionDisposition
	err  error
}

// ingressImmediateDeathCompletion is the typed authoritative
// death-completion owner command (spec §9.5.1h): it carries one
// already-frozen ImmediateDeathCompletion value only — no
// lookup, no recovery, no persistence — and the owner executes
// the normal PlayerAcceptImmediateDeathCompletion path. It uses
// the SAME bounded mailbox and admission rules as every other
// ingress command; the future c3c3b executor redelivers the SAME
// completion/token after ErrSimIngressFull, and a redelivery
// after the first apply resolves as the existing zero-mutation
// Duplicate result.
type ingressImmediateDeathCompletion struct {
	completion ImmediateDeathCompletion
	res        chan ingressDeathCompletionResult
}

func (c ingressImmediateDeathCompletion) execute(e *Engine) {
	snap, disp, err := e.PlayerAcceptImmediateDeathCompletion(c.completion)
	c.res <- ingressDeathCompletionResult{snap: snap, disp: disp, err: err}
}

func (c ingressImmediateDeathCompletion) fail(err error) {
	c.res <- ingressDeathCompletionResult{err: err}
}

// ingressPortalCompletionResult is the typed completion of one
// Portal-of-Life completion command, preserving the exact
// owner-local PlayerAcceptPortalOfLifeCompletion semantics
// (Applied vs Duplicate vs error).
type ingressPortalCompletionResult struct {
	disp PortalCompletionDisposition
	err  error
}

// ingressPortalCompletion is the typed authoritative
// Portal-completion owner command (spec §9.5.1k): it carries one
// already-frozen PortalOfLifeCompletion value only — no lookup,
// no recovery, no persistence — and the owner executes the normal
// PlayerAcceptPortalOfLifeCompletion path. It uses the SAME bounded
// mailbox and admission rules as every other ingress command; the
// future c3d2b executor redelivers the SAME completion/token after
// ErrSimIngressFull, and a redelivery after the first apply resolves
// as the existing zero-mutation Duplicate result.
type ingressPortalCompletion struct {
	completion PortalOfLifeCompletion
	res        chan ingressPortalCompletionResult
}

func (c ingressPortalCompletion) execute(e *Engine) {
	disp, err := e.PlayerAcceptPortalOfLifeCompletion(c.completion)
	c.res <- ingressPortalCompletionResult{disp: disp, err: err}
}

func (c ingressPortalCompletion) fail(err error) {
	c.res <- ingressPortalCompletionResult{err: err}
}

// ingressPortalAbortResult is the typed completion of one
// Portal-of-Life abort command, preserving the exact owner-local
// PlayerAbortPortalOfLifeAttempt semantics.
type ingressPortalAbortResult struct {
	disp PortalAbortDisposition
	err  error
}

// ingressPortalAbort is the typed definitive Portal-abort owner
// command (spec §9.5.1k): it carries one PortalAttemptToken only
// and the owner executes the normal
// PlayerAbortPortalOfLifeAttempt path on the SAME bounded mailbox.
type ingressPortalAbort struct {
	token PortalAttemptToken
	res   chan ingressPortalAbortResult
}

func (c ingressPortalAbort) execute(e *Engine) {
	disp, err := e.PlayerAbortPortalOfLifeAttempt(c.token)
	c.res <- ingressPortalAbortResult{disp: disp, err: err}
}

func (c ingressPortalAbort) fail(err error) {
	c.res <- ingressPortalAbortResult{err: err}
}

// ingressDeathPenaltyRetryableResult is the typed completion
// of one penalty retryable-notification command, preserving
// the exact owner-local
// PlayerMarkDeathPenaltyPersistenceRetryable semantics.
type ingressDeathPenaltyRetryableResult struct {
	disp DeathPenaltyRetryDisposition
	err  error
}

// ingressDeathPenaltyRetryable is the typed pre-Store
// penalty-retryable owner command (spec §9.5.1k): it
// carries one DeathPenaltyAttemptToken only and the owner
// executes the normal
// PlayerMarkDeathPenaltyPersistenceRetryable path on the
// SAME bounded mailbox.
type ingressDeathPenaltyRetryable struct {
	token DeathPenaltyAttemptToken
	res   chan ingressDeathPenaltyRetryableResult
}

func (c ingressDeathPenaltyRetryable) execute(e *Engine) {
	disp, err := e.PlayerMarkDeathPenaltyPersistenceRetryable(c.token)
	c.res <- ingressDeathPenaltyRetryableResult{disp: disp, err: err}
}

func (c ingressDeathPenaltyRetryable) fail(err error) {
	c.res <- ingressDeathPenaltyRetryableResult{err: err}
}

// ingressDeathPenaltyCompletionResult is the typed completion
// of one penalty success-completion command, preserving the
// exact owner-local PlayerAcceptDeathPenaltyCompletion
// semantics (Applied vs Duplicate vs error).
type ingressDeathPenaltyCompletionResult struct {
	disp DeathPenaltyCompletionDisposition
	err  error
}

// ingressDeathPenaltyCompletion is the typed authoritative
// penalty-completion owner command (spec §9.5.1k): it
// carries one DeathPenaltyCompletion value only (the
// attempt token; the post-penalty state is already frozen
// on the entity) and the owner executes the normal
// PlayerAcceptDeathPenaltyCompletion path. It uses the
// SAME bounded mailbox and admission rules as every other
// ingress command; the future d3b executor redelivers the
// SAME completion/token after ErrSimIngressFull, and a
// redelivery after the first apply resolves as the
// existing zero-mutation Duplicate result.
type ingressDeathPenaltyCompletion struct {
	completion DeathPenaltyCompletion
	res        chan ingressDeathPenaltyCompletionResult
}

func (c ingressDeathPenaltyCompletion) execute(e *Engine) {
	disp, err := e.PlayerAcceptDeathPenaltyCompletion(c.completion)
	c.res <- ingressDeathPenaltyCompletionResult{disp: disp, err: err}
}

func (c ingressDeathPenaltyCompletion) fail(err error) {
	c.res <- ingressDeathPenaltyCompletionResult{err: err}
}

// ingressState is the run-ownership coordination only: whether a Run
// currently owns the engine. It MUST NOT become a mutex protecting
// normal sim entity state — mutable sim stays single-owner, and the
// owner never takes this lock while executing commands or steps.
type ingressState struct {
	mu      sync.Mutex
	running bool
}

// claimRun makes the caller the Run owner, or reports
// ErrEngineAlreadyRunning when one is active.
func (s *ingressState) claimRun() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return ErrEngineAlreadyRunning
	}
	s.running = true
	return nil
}

// EnqueueAddEntity submits a generic entity add through the sim owner
// (spec §5.2.10). It is safe to call from non-sim goroutines while
// Run is active and returns the real owner-local EntitySnapshot.
// Generic entities only: players use EnqueueAddPlayerEntity.
func (e *Engine) EnqueueAddEntity(ctx context.Context, pos world.Vec3) (EntitySnapshot, error) {
	cmd := ingressAdd{pos: pos, res: make(chan ingressAddResult, 1)}
	if err := e.admit(ctx, cmd); err != nil {
		return EntitySnapshot{}, err
	}
	// Admitted commands are authoritative: later caller cancellation
	// does NOT retract them, so the caller waits for the exact
	// command's definitive result (never an ambiguous maybe).
	res := <-cmd.res
	return res.snap, res.err
}

// EnqueueAddPlayerEntity submits a typed player add through the sim
// owner (spec §9.5.1d). It uses the SAME bounded mailbox and admission
// rules as every other ingress command and returns the real
// owner-local EntitySnapshot: admitted commands are authoritative and
// the owner invokes the normal AddPlayerEntity path (same validation,
// duplicate-binding, and all-or-nothing semantics). Existing
// EnqueueAddEntity stays available for generic entities; no gateway
// wiring happens here.
func (e *Engine) EnqueueAddPlayerEntity(ctx context.Context, characterID CharacterID, pos world.Vec3, vitals PlayerVitals, runtimeInputs PlayerVitalsRuntimeInputs) (EntitySnapshot, error) {
	cmd := ingressAddPlayer{characterID: characterID, pos: pos, vitals: vitals, runtimeInputs: runtimeInputs, res: make(chan ingressAddResult, 1)}
	if err := e.admit(ctx, cmd); err != nil {
		return EntitySnapshot{}, err
	}
	// Admitted commands are authoritative: later caller cancellation
	// does NOT retract them, so the caller waits for the exact
	// command's definitive result (never an ambiguous maybe).
	res := <-cmd.res
	return res.snap, res.err
}

// EnqueueRemoveEntity submits an entity removal through the sim
// owner. Existing RemoveEntity (including ErrEntityNotFound)
// semantics are preserved exactly.
func (e *Engine) EnqueueRemoveEntity(ctx context.Context, id EntityID) error {
	cmd := ingressRemove{id: id, res: make(chan error, 1)}
	if err := e.admit(ctx, cmd); err != nil {
		return err
	}
	return <-cmd.res
}

// EnqueueMove submits movement control through the sim owner. It
// only updates pending control via the existing SubmitMove (exact
// MoveAccepted/MoveDuplicate/MoveStale/error semantics); positions
// still change only during Step.
func (e *Engine) EnqueueMove(ctx context.Context, id EntityID, intent MoveIntent) (MoveDisposition, error) {
	cmd := ingressMove{id: id, intent: intent, res: make(chan ingressMoveResult, 1)}
	if err := e.admit(ctx, cmd); err != nil {
		return MoveAccepted, err
	}
	res := <-cmd.res
	return res.disp, res.err
}

// EnqueueImmediateDeathCompletion submits one authoritative
// immediate-death completion through the sim owner (spec
// §9.5.1h). It uses the SAME bounded mailbox and admission rules
// as every other ingress command and returns the real
// owner-local result: admitted commands are authoritative and the
// owner invokes the normal PlayerAcceptImmediateDeathCompletion
// path (same token/lifecycle validation, Applied vs
// zero-mutation Duplicate semantics, atomic
// placement/vitals/runtime/durable installation). The completion
// payload is frozen before publication, so caller mutation after
// this call begins cannot change what the owner applies. On
// admission or execution error the disposition is meaningless —
// check err first.
func (e *Engine) EnqueueImmediateDeathCompletion(ctx context.Context, completion ImmediateDeathCompletion) (EntitySnapshot, DeathCompletionDisposition, error) {
	cmd := ingressImmediateDeathCompletion{completion: freezeImmediateDeathCompletion(completion), res: make(chan ingressDeathCompletionResult, 1)}
	if err := e.admit(ctx, cmd); err != nil {
		return EntitySnapshot{}, DeathCompletionApplied, err
	}
	// Admitted commands are authoritative: later caller cancellation
	// does NOT retract them, so the caller waits for the exact
	// command's definitive result (never an ambiguous maybe).
	res := <-cmd.res
	return res.snap, res.disp, res.err
}

// EnqueuePortalOfLifeCompletion submits one authoritative
// Portal-of-Life completion through the sim owner (spec
// §9.5.1k). It uses the SAME bounded mailbox and admission rules
// as every other ingress command and returns the real
// owner-local result: admitted commands are authoritative and the
// owner invokes the normal PlayerAcceptPortalOfLifeCompletion
// path (same token/lifecycle validation, Applied vs
// zero-mutation Duplicate semantics, pending-only
// installation). The completion payload is frozen before
// publication, so caller mutation after this call begins cannot
// change what the owner applies. On admission or execution error
// the disposition is meaningless — check err first.
func (e *Engine) EnqueuePortalOfLifeCompletion(ctx context.Context, completion PortalOfLifeCompletion) (PortalCompletionDisposition, error) {
	cmd := ingressPortalCompletion{completion: freezePortalOfLifeCompletion(completion), res: make(chan ingressPortalCompletionResult, 1)}
	if err := e.admit(ctx, cmd); err != nil {
		return PortalCompletionApplied, err
	}
	// Admitted commands are authoritative: later caller cancellation
	// does NOT retract them, so the caller waits for the exact
	// command's definitive result (never an ambiguous maybe).
	res := <-cmd.res
	return res.disp, res.err
}

// EnqueuePortalOfLifeAbort submits one definitive Portal-of-Life
// abort through the sim owner (spec §9.5.1k). It uses the SAME
// bounded mailbox and admission rules as every other ingress
// command and returns the real owner-local result (Aborted vs
// idempotent Duplicate vs terminal mismatch). On admission or
// execution error the disposition is meaningless — check err
// first.
func (e *Engine) EnqueuePortalOfLifeAbort(ctx context.Context, token PortalAttemptToken) (PortalAbortDisposition, error) {
	cmd := ingressPortalAbort{token: token, res: make(chan ingressPortalAbortResult, 1)}
	if err := e.admit(ctx, cmd); err != nil {
		return PortalAbortAborted, err
	}
	res := <-cmd.res
	return res.disp, res.err
}

// EnqueueDeathPenaltyPersistenceRetryable submits one
// pre-Store penalty retryable notification through the sim
// owner (spec §9.5.1k). It uses the SAME bounded mailbox
// and admission rules as every other ingress command and
// returns the real owner-local result (Applied vs
// idempotent Duplicate vs terminal mismatch). On admission
// or execution error the disposition is meaningless —
// check err first.
func (e *Engine) EnqueueDeathPenaltyPersistenceRetryable(ctx context.Context, token DeathPenaltyAttemptToken) (DeathPenaltyRetryDisposition, error) {
	cmd := ingressDeathPenaltyRetryable{token: token, res: make(chan ingressDeathPenaltyRetryableResult, 1)}
	if err := e.admit(ctx, cmd); err != nil {
		return DeathPenaltyRetryApplied, err
	}
	res := <-cmd.res
	return res.disp, res.err
}

// EnqueueDeathPenaltyCompletion submits one authoritative
// penalty success completion through the sim owner (spec
// §9.5.1k). It uses the SAME bounded mailbox and admission
// rules as every other ingress command and returns the real
// owner-local result: admitted commands are authoritative
// and the owner invokes the normal
// PlayerAcceptDeathPenaltyCompletion path (same
// token/lifecycle validation, Applied vs zero-mutation
// Duplicate semantics, exact frozen-capture install). On
// admission or execution error the disposition is
// meaningless — check err first.
func (e *Engine) EnqueueDeathPenaltyCompletion(ctx context.Context, completion DeathPenaltyCompletion) (DeathPenaltyCompletionDisposition, error) {
	cmd := ingressDeathPenaltyCompletion{completion: completion, res: make(chan ingressDeathPenaltyCompletionResult, 1)}
	if err := e.admit(ctx, cmd); err != nil {
		return DeathPenaltyCompletionApplied, err
	}
	res := <-cmd.res
	return res.disp, res.err
}

// admit performs the deterministic pre-publication checks and the
// immediate bounded publication as one atomic step under the
// run-state lock: a cancelled context returns its error without
// publishing; a non-running engine reports ErrEngineNotRunning; a
// full mailbox reports ErrSimIngressFull with zero mutation. The
// lock is held across the non-blocking channel send so a concurrent
// Run exit drain cannot interleave between the running check and
// publication (no admitted command is ever orphaned).
func (e *Engine) admit(ctx context.Context, cmd ingressCommand) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.runState.mu.Lock()
	defer e.runState.mu.Unlock()
	if !e.runState.running {
		return ErrEngineNotRunning
	}
	select {
	case e.ingress <- cmd:
		return nil
	default:
		return ErrSimIngressFull
	}
}

// releaseRun marks the engine non-running and fails every
// queued-but-not-executed command with ErrEngineStopped. No waiter
// stays parked and no command survives into a later Run generation.
func (e *Engine) releaseRun() {
	e.runState.mu.Lock()
	defer e.runState.mu.Unlock()
	e.runState.running = false
	for {
		select {
		case cmd := <-e.ingress:
			cmd.fail(ErrEngineStopped)
		default:
			return
		}
	}
}

// runIngressStep is one owner-loop scheduling pass (spec §5.2.10
// tick priority): a ready ticker pulse wins before another queued
// command, and a tick that became ready while blocked wins before
// the just-dequeued command executes. It reports whether the owner
// loop must exit (caller context done).
func (e *Engine) runIngressStep(ctx context.Context, ticker Ticker) bool {
	select {
	case <-ticker.C():
		e.Step()
		return false
	default:
	}
	if ctx.Err() != nil {
		return true
	}
	select {
	case <-ctx.Done():
		return true
	case <-ticker.C():
		e.Step()
		return false
	case cmd := <-e.ingress:
		select {
		case <-ticker.C():
			e.Step()
		default:
		}
		cmd.execute(e)
		return false
	}
}
