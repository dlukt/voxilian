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
// mailbox carries (spec §5.2.10): exactly add, remove, and move.
// Gateway-facing code can never submit arbitrary closures: there is
// no func(*Engine) command.
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

// EnqueueAddEntity submits an entity add through the sim owner
// (spec §5.2.10). It is safe to call from non-sim goroutines while
// Run is active and returns the real owner-local EntitySnapshot.
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
