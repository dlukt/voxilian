package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/dlukt/voxilian/internal/proto"
	"github.com/dlukt/voxilian/internal/session"
	"github.com/dlukt/voxilian/internal/sim"
)

// Gateway death wire/state integration (spec §9.5.1l,
// M5-T5c4): the narrow gateway-owned death presentation
// and respawn-correlation layer over the existing
// Presence/fanout/outbound machinery. It owns NO death
// gameplay mechanics, NO RNG, NO Store requests, NO
// pending-cost calculation, NO Portal/penalty state —
// only ephemeral transport state (the admitted respawn
// correlation per live session/presence epoch) plus the
// opcode-120 transport handler. All wire entity IDs are
// recipient-local NetEntityIDs resolved per recipient;
// sim.EntityID, CharacterID, database IDs, and array
// indexes never cross the wire.

// respawnCorrelation is the ephemeral gateway-owned
// correlation binding one admitted 215 to the exact live
// death attempt it presented (spec §9.5.1l L6). It exists
// ONLY between the 215 critical admission for a live
// attempt and the session/presence epoch teardown. It is
// never durable, never inherited across session epochs,
// and never keyed by CharacterID alone.
type respawnCorrelation struct {
	entity sim.EntityID
	charID sim.CharacterID
	token  sim.DeathAttemptToken
}

// DeathReleaseSim is the narrow sim seam the 120 handler
// needs: the typed respawn-release owner ingress.
// *sim.Engine satisfies it structurally.
type DeathReleaseSim interface {
	EnqueuePlayerReleaseRespawn(context.Context, sim.DeathAttemptToken) (sim.EntitySnapshot, sim.RespawnReleaseDisposition, error)
}

// DeathWireRuntime composes PresenceRegistry, the session
// Registry, and FanoutRuntime into the T5c4 death
// presentation layer (spec §9.5.1l). It implements
// sim.DeathPresentationSink (synchronous, non-blocking:
// Presence copies plus TryCritical only) and serves
// opcode 120 through DeathAckHandler. It is safe for
// concurrent use; the small correlation mutex is never
// held across Presence calls, sim ingress, fanout work,
// or socket writes.
type DeathWireRuntime struct {
	presence *PresenceRegistry
	sessions *session.Registry
	fanout   *FanoutRuntime

	mu   sync.Mutex
	corr map[session.ID]respawnCorrelation
}

var _ sim.DeathPresentationSink = (*DeathWireRuntime)(nil)

// NewDeathWireRuntime wires a DeathWireRuntime. Every
// dependency is required: presence, sessions, and fanout
// must never silently default. It takes no sim ingress:
// the release ingress is supplied per-call by
// DeathAckHandler, so engine construction (which names
// this runtime as its Death sink) never cycles.
func NewDeathWireRuntime(presence *PresenceRegistry, sessions *session.Registry, fanout *FanoutRuntime) (*DeathWireRuntime, error) {
	if presence == nil {
		return nil, errors.New("gateway: presence registry is required")
	}
	if sessions == nil {
		return nil, errors.New("gateway: session registry is required")
	}
	if fanout == nil {
		return nil, errors.New("gateway: fanout runtime is required")
	}
	return &DeathWireRuntime{
		presence: presence,
		sessions: sessions,
		fanout:   fanout,
		corr:     make(map[session.ID]respawnCorrelation),
	}, nil
}

// OnDeathBegin implements sim.DeathPresentationSink
// (spec §9.5.1l L1/L2): exactly one 214 per ready viewer
// of the dead entity for this live attempt. It runs on
// the sim owner goroutine and never blocks: one Presence
// copy plus non-blocking TryCritical frames only.
// Persistence redelivery never re-fires the begin, so no
// second 214 can arise here.
func (r *DeathWireRuntime) OnDeathBegin(ev sim.DeathBeginEvent) {
	r.fanout.EmitDeath(ev.Token.EntityID)
}

// OnDeathCompleted implements sim.DeathPresentationSink
// (spec §9.5.1l L3–L6): after the owner accepted the
// persistence completion, reconcile the post-death
// teleport through the existing fanout machinery, send
// the authoritative 215 to the currently controlling
// session, and — only after successful critical admission
// — establish that session's exact ephemeral respawn
// correlation. Duplicate completions never reach this
// sink (the owner emits only Applied). No gameplay state
// is applied here: the sim owner already did that.
func (r *DeathWireRuntime) OnDeathCompleted(ev sim.DeathCompletedEvent) {
	if ev.Snapshot.CharacterID != ev.Token.CharacterID ||
		ev.Snapshot.ID != ev.Token.EntityID {
		return
	}
	owner, controlled := r.fanout.RelocateEntity(ev.Snapshot, ev.Tick)
	if !controlled {
		// No controlling session (disconnect/takeover
		// race): the authoritative death stands without
		// presentation. Fresh reconnect owns recovery;
		// nothing is delivered to a replacement by
		// CharacterID, and no correlation is stored.
		return
	}
	if osnap, err := r.presence.Snapshot(owner); err != nil ||
		osnap.EntityID != ev.Token.EntityID ||
		sim.CharacterID(osnap.CharacterID) != ev.Token.CharacterID {
		return
	}
	wire, err := WirePosition(ev.Snapshot.Position)
	if err != nil {
		// An impossible authoritative position is an
		// internal fail-closed invariant, never a silent
		// clamp: close the session for reconnect/resync
		// without presentation or correlation.
		r.fanout.failRecipient(owner, "respawn_wire_position", err)
		return
	}
	if err := r.tryCritical(owner, proto.OpcodeRespawn,
		func(e *proto.Encoder) error {
			proto.Respawn{Pos: wire}.Encode(e)
			return nil
		}); err != nil {
		// Critical saturation closes only the victim
		// session; authoritative sim/death/persistence
		// state is NOT rolled back. Reconnect/full
		// baseline is recovery.
		r.fanout.failRecipient(owner, "respawn_critical", err)
		return
	}
	r.mu.Lock()
	r.corr[owner] = respawnCorrelation{
		entity: ev.Token.EntityID,
		charID: ev.Token.CharacterID,
		token:  ev.Token,
	}
	r.mu.Unlock()
}

// tryCritical admits one critical frame to sid without
// waiting, resolving the recipient's queue-capable
// outbound without holding any Presence lock.
func (r *DeathWireRuntime) tryCritical(sid session.ID, opcode uint16, encode func(*proto.Encoder) error) error {
	snap, ok := r.sessions.Get(sid)
	if !ok || snap.Conn == nil {
		return ErrFanoutNoProducer
	}
	p, ok := snap.Conn.(OutboundProducer)
	if !ok {
		return ErrFanoutNoProducer
	}
	return p.TryCritical(sid, opcode, proto.MessageVersion1, encode)
}

// ForgetSession drops sid's ephemeral respawn correlation
// (takeover/disconnect/exit teardown). It is idempotent
// and never touches Presence, sim, or sockets: even
// without it, correlation verification against the live
// Presence snapshot keeps a stale entry harmless.
func (r *DeathWireRuntime) ForgetSession(sid session.ID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.corr, sid)
}

// ReleaseRespawn serves one decoded C→S 120 respawn_ack
// for sid (spec §9.5.1l L6–L8): resolve the live
// Presence, resolve the exact Phase-A respawn
// correlation, verify it still names this session epoch's
// controlled EntityID and CharacterID, and submit the
// exact token through the typed owner ingress. Applied
// and Duplicate are both silent success with no invented
// packet; pending death, Portal, and penalty state are
// never touched (120 != LeaveHold !=
// ApplyDeathPenalties: the owner primitive preserves
// every other live state bit by construction).
func (r *DeathWireRuntime) ReleaseRespawn(ctx context.Context, sid session.ID, release DeathReleaseSim) error {
	snap, err := r.presence.Snapshot(sid)
	if err != nil {
		return fmt.Errorf("gateway: respawn ack for session without presence %d: %w", uint64(sid), err)
	}
	r.mu.Lock()
	corr, ok := r.corr[sid]
	r.mu.Unlock()
	if !ok {
		return &ClientError{Code: proto.ErrorCodeProtocol, Message: "no respawn pending"}
	}
	if corr.entity != snap.EntityID || corr.charID != sim.CharacterID(snap.CharacterID) {
		return &ClientError{Code: proto.ErrorCodeProtocol, Message: "stale respawn correlation"}
	}
	_, disp, err := release.EnqueuePlayerReleaseRespawn(ctx, corr.token)
	if err != nil {
		return mapRespawnReleaseError(err)
	}
	_ = disp
	return nil
}

// mapRespawnReleaseError maps sim-owner release outcomes
// to the frozen 202 registry (spec §9.5.1l L8). Success
// dispositions are silent upstream; only errors map here.
func mapRespawnReleaseError(err error) error {
	switch {
	case errors.Is(err, sim.ErrSimIngressFull),
		errors.Is(err, sim.ErrEngineNotRunning),
		errors.Is(err, sim.ErrEngineStopped):
		return &ClientError{Code: proto.ErrorCodeRetry, Message: "respawn unavailable, retry"}
	case errors.Is(err, sim.ErrDeathAttemptMismatch):
		return &ClientError{Code: proto.ErrorCodeProtocol, Message: "stale respawn correlation"}
	case errors.Is(err, sim.ErrEntityNotFound),
		errors.Is(err, sim.ErrCellHandoffRequired):
		// Gateway presence and sim ownership diverged:
		// internal fail-closed, never invalid_handle and
		// never silent success.
		return fmt.Errorf("gateway: respawn release for missing sim entity: %w", err)
	default:
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("gateway: respawn release context: %w", err)
		}
		return fmt.Errorf("gateway: respawn release: %w", err)
	}
}

// DeathAckHandler owns exactly C→S 120 respawn_ack
// transport (spec §9.5.1l L7): it runs AFTER the
// GameplayIngressHandler general intent rate gate (chain
// it as Next), so it adds no second intent token charge.
// Structurally valid 120 decodes here; malformed payloads
// fail with protocol_error before any correlation or sim
// work. All other opcodes delegate unchanged to Next.
type DeathAckHandler struct {
	Runtime *DeathWireRuntime
	Release DeathReleaseSim
	Next    MessageHandler
}

// NewDeathAckHandler wires a DeathAckHandler. Runtime and
// Release are required; Next may be nil, consuming
// delegated opcodes silently.
func NewDeathAckHandler(runtime *DeathWireRuntime, release DeathReleaseSim, next MessageHandler) (*DeathAckHandler, error) {
	if runtime == nil {
		return nil, errors.New("gateway: death wire runtime is required")
	}
	if release == nil {
		return nil, errors.New("gateway: death release ingress is required")
	}
	return &DeathAckHandler{Runtime: runtime, Release: release, Next: next}, nil
}

// Handle implements MessageHandler.
func (h *DeathAckHandler) Handle(
	ctx context.Context,
	sid session.ID,
	header proto.Header,
	payload *proto.Decoder,
	send SendFunc,
) error {
	if header.Opcode != proto.OpcodeRespawnAck {
		if h.Next == nil {
			return nil
		}
		return h.Next.Handle(ctx, sid, header, payload, send)
	}
	if _, err := proto.DecodeRespawnAck(payload); err != nil {
		return &ClientError{Code: proto.ErrorCodeProtocol, Message: "malformed respawn_ack"}
	}
	return h.Runtime.ReleaseRespawn(ctx, sid, h.Release)
}

// PlayerBootstrapLoader is the narrow injected
// recovery/bootstrap seam for T5c4 reconnect world entry
// (spec §9.5.1l L11): gateway -> abstract loader,
// concrete adapter outside gateway over the existing
// LoadDeathCharacterRecovery read. It returns an
// already-resolved sim-domain bootstrap value only — no
// Store-domain snapshot, pgx handle, or sqlc row may
// leak into gateway. Calls must not block the caller
// beyond one bounded PG read; no gameplay, no RNG, no
// Portal calculation, no penalty planning.
type PlayerBootstrapLoader interface {
	LoadPlayerBootstrap(ctx context.Context, characterID int64) (sim.PlayerRecoveryBootstrap, error)
}

// PlayerBootstrapLoaderFunc adapts a plain function to a
// PlayerBootstrapLoader.
type PlayerBootstrapLoaderFunc func(ctx context.Context, characterID int64) (sim.PlayerRecoveryBootstrap, error)

// LoadPlayerBootstrap implements PlayerBootstrapLoader.
func (f PlayerBootstrapLoaderFunc) LoadPlayerBootstrap(ctx context.Context, characterID int64) (sim.PlayerRecoveryBootstrap, error) {
	return f(ctx, characterID)
}
