package sim

import (
	"context"
	"fmt"

	"github.com/dlukt/voxilian/internal/world"
)

// Death wire/state integration owner seams (spec §9.5.1l,
// M5-T5c4): the narrow presentation sink the gateway
// observes, the typed respawn-release owner ingress, and
// the atomic player-recovery bootstrap. `internal/sim`
// only: no store, persist, pgx, sqlc output, gateway,
// session, or proto import; no blocking work, no
// goroutines, no persistence, no Portal/penalty mechanics,
// no pending deletion. The gateway MUST NOT mutate live
// entities directly: every mutation below runs on the sim
// owner through the SAME bounded mailbox.

// DeathBeginEvent is the already-authoritative death-begin
// presentation fact (spec §9.5.1l L1): the owner has
// accepted a real death and installed the live attempt
// token. It carries identity only — never HP, position,
// vitals, or Store state.
type DeathBeginEvent struct {
	Token DeathAttemptToken
}

// DeathCompletedEvent is the already-authoritative
// death-completion presentation fact (spec §9.5.1l
// L3/L5): the owner has accepted the persistence
// completion and installed the post-death state. It
// carries the accepted snapshot plus the exact token —
// never a Store result, revision, or Saver metadata —
// plus the owner tick at acceptance for fanout staleness
// comparison. It fires ONLY for DeathCompletionApplied;
// Duplicate completions, failed installs, and mismatches
// emit nothing.
type DeathCompletedEvent struct {
	Snapshot EntitySnapshot
	Token    DeathAttemptToken
	Tick     uint32
}

// DeathPresentationSink observes already-authoritative
// death lifecycle transitions for wire presentation
// (spec §9.5.1l L1). Implementations run on the sim owner
// goroutine and MUST be non-blocking/bounded: no socket
// waits, no PG I/O, no mailbox admission waits, no second
// ordering system. The gateway adapter emits through the
// existing critical lane (TryCritical) and the existing
// Presence/fanout primitives only. A nil sink selects no
// observation. The sink owns no gameplay authority and
// can never change what the owner installed.
type DeathPresentationSink interface {
	OnDeathBegin(DeathBeginEvent)
	OnDeathCompleted(DeathCompletedEvent)
}

// emitDeathBegin reports a successful owner transition
// into DeathPersisting. Call only from the sim owner
// after the token is installed.
func (e *Engine) emitDeathBegin(token DeathBeginEvent) {
	if e.death != nil {
		e.death.OnDeathBegin(token)
	}
}

// emitDeathCompleted reports an accepted (Applied) owner
// completion. Call only from the sim owner after the
// post-death state is installed.
func (e *Engine) emitDeathCompleted(ev DeathCompletedEvent) {
	if e.death != nil {
		e.death.OnDeathCompleted(ev)
	}
}

// PlayerRecoveryBootstrap is the complete already-resolved
// reconnect/world-entry bootstrap value (spec §9.5.1l
// L10/L12): the materialized player state a fresh entity
// is built from. Every field is sim-domain and already
// resolved — no Store types, no PG handles, no lookup
// inside the owner, no network. Pending nil is the
// authoritative "no pending death" value. Fresh ephemeral
// state (life, epochs, attempt tokens, ack state) is NOT
// carried: a fresh entity always starts ordinary Alive
// gameplay. Treat values as immutable: typed ingress
// freezes the payload before publication.
type PlayerRecoveryBootstrap struct {
	CharacterID   CharacterID
	Position      world.Vec3
	Vitals        PlayerVitals
	RuntimeInputs PlayerVitalsRuntimeInputs
	Durable       PlayerDurableState
	Pending       *PendingDeathRuntime
}

// freezePlayerRecoveryBootstrap deep-copies a bootstrap
// payload into independent ownership: the durable shadow
// reuses the c3c2 deep-freeze and the pending value the
// d1 deep-freeze (including the optional CorpseID
// pointer). Plain values copy by assignment.
func freezePlayerRecoveryBootstrap(b PlayerRecoveryBootstrap) PlayerRecoveryBootstrap {
	b.Durable = freezePlayerDurableState(b.Durable)
	b.Pending = freezePendingDeathRuntime(b.Pending)
	return b
}

// AddPlayerEntityWithRecovery inserts a player entity from
// an already-resolved recovery bootstrap atomically in
// ONE owner turn (spec §9.5.1l L12): identity, vitals,
// runtime, durable shadow, AND pending hydration install
// together, so world entry can never briefly expose an
// Alive player with missing pending state when durable
// recovery says a pending death exists.
//
// Validation precedes EntityID consumption, in this
// order: pending value domain (nil valid); durable
// shadow domain; then the ordinary
// AddPlayerEntityWithDurableState rules (CharacterID,
// vitals, runtime inputs, duplicate live binding,
// all-or-nothing position). Every failure consumes no
// EntityID and mutates nothing: no half-created player,
// no lost pending state, no CharacterID binding leak.
// The fresh entity starts PlayerLifeAlive with fresh
// ephemeral runtime; no life state, epoch, token, or ack
// state is recovered.
//
// Owner-local: call only from the sim owner goroutine
// (Run/Step) or in Step-driven tests. Concurrent gateway
// callers use EnqueueAddPlayerEntityWithRecovery.
func (e *Engine) AddPlayerEntityWithRecovery(boot PlayerRecoveryBootstrap) (EntitySnapshot, error) {
	if err := ValidatePendingDeathRuntime(boot.Pending); err != nil {
		return EntitySnapshot{}, err
	}
	frozenPending := freezePendingDeathRuntime(boot.Pending)
	snap, err := e.AddPlayerEntityWithDurableState(
		boot.CharacterID, boot.Position, boot.Vitals, boot.RuntimeInputs, boot.Durable)
	if err != nil {
		return EntitySnapshot{}, err
	}
	// The player add above succeeded, so the entity is a
	// resident player by construction; the lookup below
	// cannot observe a different owner epoch. A defensive
	// failure removes nothing silently: report internal.
	ent, err := e.registry.lookup(snap.ID)
	if err != nil {
		return EntitySnapshot{}, fmt.Errorf("sim: entity %d vanished after recovery add: %w", uint64(snap.ID), err)
	}
	ent.pendingDeath = frozenPending
	return ent.snapshot(), nil
}

// ingressRespawnReleaseResult is the typed completion of
// one respawn-release command, preserving the exact
// owner-local PlayerReleaseRespawn semantics (Applied vs
// Duplicate vs error).
type ingressRespawnReleaseResult struct {
	snap EntitySnapshot
	disp RespawnReleaseDisposition
	err  error
}

// ingressRespawnRelease is the typed respawn-release owner
// command (spec §9.5.1l L7): it carries one immutable
// DeathAttemptToken value only — no lookup, no recovery,
// no persistence — and the owner executes the normal
// PlayerReleaseRespawn path. It uses the SAME bounded
// mailbox and admission rules as every other ingress
// command. The owner-local primitive remains the only
// mutator.
type ingressRespawnRelease struct {
	token DeathAttemptToken
	res   chan ingressRespawnReleaseResult
}

func (c ingressRespawnRelease) execute(e *Engine) {
	snap, disp, err := e.PlayerReleaseRespawn(c.token)
	c.res <- ingressRespawnReleaseResult{snap: snap, disp: disp, err: err}
}

func (c ingressRespawnRelease) fail(err error) {
	c.res <- ingressRespawnReleaseResult{err: err}
}

// EnqueuePlayerReleaseRespawn submits one exact
// respawn-release token through the sim owner (spec
// §9.5.1l L7): the T5c4 opcode-120 transport path. It uses
// the SAME bounded mailbox and admission rules as every
// other ingress command and returns the real owner-local
// result (Applied vs zero-mutation Duplicate vs error).
// The token is a plain immutable value: nothing freezes.
// On admission or execution error the disposition is
// meaningless — check err first. This releases the
// transport/gameplay respawn phase ONLY: pending death,
// Portal, and penalty state are untouched (120 !=
// LeaveHold != ApplyDeathPenalties).
func (e *Engine) EnqueuePlayerReleaseRespawn(ctx context.Context, token DeathAttemptToken) (EntitySnapshot, RespawnReleaseDisposition, error) {
	cmd := ingressRespawnRelease{token: token, res: make(chan ingressRespawnReleaseResult, 1)}
	if err := e.admit(ctx, cmd); err != nil {
		return EntitySnapshot{}, RespawnReleaseApplied, err
	}
	// Admitted commands are authoritative: later caller
	// cancellation does NOT retract them, so the caller
	// waits for the exact command's definitive result
	// (never an ambiguous maybe).
	res := <-cmd.res
	return res.snap, res.disp, res.err
}

// ingressAddPlayerRecoveryResult is the typed completion
// of one player-recovery bootstrap command.
type ingressAddPlayerRecoveryResult struct {
	snap EntitySnapshot
	err  error
}

// ingressAddPlayerRecovery is the typed atomic
// player-recovery bootstrap owner command (spec §9.5.1l
// L12): it carries one already-frozen
// PlayerRecoveryBootstrap value only — no lookup, no
// recovery, no persistence — and the owner executes the
// normal AddPlayerEntityWithRecovery path. It uses the
// SAME bounded mailbox and admission rules as every other
// ingress command.
type ingressAddPlayerRecovery struct {
	boot PlayerRecoveryBootstrap
	res  chan ingressAddPlayerRecoveryResult
}

func (c ingressAddPlayerRecovery) execute(e *Engine) {
	snap, err := e.AddPlayerEntityWithRecovery(c.boot)
	c.res <- ingressAddPlayerRecoveryResult{snap: snap, err: err}
}

func (c ingressAddPlayerRecovery) fail(err error) {
	c.res <- ingressAddPlayerRecoveryResult{err: err}
}

// EnqueueAddPlayerEntityWithRecovery submits one atomic
// player-recovery bootstrap through the sim owner (spec
// §9.5.1l L10/L12): the T5c4 reconnect/world-entry path.
// It uses the SAME bounded mailbox and admission rules as
// every other ingress command and returns the real
// owner-local result. The payload is frozen before
// publication, so caller mutation after this call begins
// cannot change what the owner installs. On admission or
// execution error the snapshot is meaningless — check err
// first.
func (e *Engine) EnqueueAddPlayerEntityWithRecovery(ctx context.Context, boot PlayerRecoveryBootstrap) (EntitySnapshot, error) {
	cmd := ingressAddPlayerRecovery{boot: freezePlayerRecoveryBootstrap(boot), res: make(chan ingressAddPlayerRecoveryResult, 1)}
	if err := e.admit(ctx, cmd); err != nil {
		return EntitySnapshot{}, err
	}
	// Admitted commands are authoritative: later caller
	// cancellation does NOT retract them, so the caller
	// waits for the exact command's definitive result
	// (never an ambiguous maybe).
	res := <-cmd.res
	return res.snap, res.err
}
