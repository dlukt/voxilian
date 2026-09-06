package gateway

import (
	"context"
	"errors"
	"fmt"

	"github.com/dlukt/voxilian/internal/proto"
	"github.com/dlukt/voxilian/internal/session"
	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/world"
)

// SimIngress is the narrow gateway-to-sim seam (spec §7.3.1):
// owner-mailbox submission plus concurrent tick observation.
// *sim.Engine satisfies it structurally. Gateway code MUST NOT
// expose sim registry internals.
type SimIngress interface {
	EnqueueAddEntity(context.Context, world.Vec3) (sim.EntitySnapshot, error)
	EnqueueRemoveEntity(context.Context, sim.EntityID) error
	EnqueueMove(context.Context, sim.EntityID, sim.MoveIntent) (sim.MoveDisposition, error)
	CurrentTick() uint32
}

// GameplayIngressHandler owns opcode 102 transport/routing plus the
// general inbound rate gate for 103..120 (spec §7.3.2). It implements
// NO gameplay semantics: 102 converts wire Move into sim.MoveIntent
// and submits it through the sim owner (M4-T2 keeps all movement
// rules); 103..120 are only rate-gated, then delegated to Next
// UNCHANGED for their future owning tasks. All other opcodes
// delegate unchanged. 100/101/121..126 are never charged, so rate
// limiting cannot block reauth, ACK, or leave_world.
type GameplayIngressHandler struct {
	Presence *PresenceRegistry
	Sim      SimIngress
	Now      NowFunc
	Next     MessageHandler
}

// NewGameplayIngressHandler wires a GameplayIngressHandler.
// Presence, Sim, and Now are required (no hidden time source);
// Next may be nil, consuming allowed delegated opcodes silently.
func NewGameplayIngressHandler(presence *PresenceRegistry, simIngress SimIngress, now NowFunc, next MessageHandler) (*GameplayIngressHandler, error) {
	if presence == nil {
		return nil, errors.New("gateway: presence registry is required")
	}
	if simIngress == nil {
		return nil, errors.New("gateway: sim ingress is required")
	}
	if now == nil {
		return nil, errors.New("gateway: clock is required")
	}
	return &GameplayIngressHandler{
		Presence: presence,
		Sim:      simIngress,
		Now:      now,
		Next:     next,
	}, nil
}

// Handle implements MessageHandler.
func (h *GameplayIngressHandler) Handle(
	ctx context.Context,
	sid session.ID,
	header proto.Header,
	payload *proto.Decoder,
	send SendFunc,
) error {
	switch {
	case header.Opcode == proto.OpcodeMove:
		return h.move(ctx, sid, header, payload)
	case header.Opcode >= proto.OpcodeAttack && header.Opcode <= proto.OpcodeRespawnAck:
		return h.gateIntent(ctx, sid, header, payload, send)
	default:
		if h.Next == nil {
			return nil
		}
		return h.Next.Handle(ctx, sid, header, payload, send)
	}
}

// move serves 102: DecodeMove, resolve the active Presence,
// charge the movement bucket, submit through the sim owner, and map
// the result. Malformed payloads fail before charging; every
// structurally valid request consumes one token (even duplicates,
// stale, invalid-tick, ambiguous, and saturated outcomes).
func (h *GameplayIngressHandler) move(
	ctx context.Context,
	sid session.ID,
	header proto.Header,
	payload *proto.Decoder,
) error {
	mv, err := proto.DecodeMove(payload)
	if err != nil {
		return &ClientError{Code: proto.ErrorCodeProtocol, Message: "malformed move"}
	}
	snap, err := h.Presence.Snapshot(sid)
	if err != nil {
		return fmt.Errorf("gateway: move for session without presence %d: %w", uint64(sid), err)
	}
	allowed, err := h.Presence.AllowMove(sid, h.Now())
	if err != nil {
		return fmt.Errorf("gateway: move rate check: %w", err)
	}
	if !allowed {
		return &ClientError{Code: proto.ErrorCodeRateLimited, Message: "movement rate limited"}
	}
	intent := sim.MoveIntent{
		InputSeq:   mv.InputSeq,
		HeldDirs:   mv.HeldDirs,
		RunFlag:    mv.RunFlag,
		Yaw:        mv.Yaw,
		SampleTick: header.Tick,
	}
	disp, err := h.Sim.EnqueueMove(ctx, snap.EntityID, intent)
	if err != nil {
		return mapMoveError(err)
	}
	_ = disp
	return nil
}

// gateIntent rate-gates one 103..120 gameplay opcode WITHOUT decoding
// it: denied requests never reach Next; allowed requests delegate the
// original decoder unchanged.
func (h *GameplayIngressHandler) gateIntent(
	ctx context.Context,
	sid session.ID,
	header proto.Header,
	payload *proto.Decoder,
	send SendFunc,
) error {
	if _, err := h.Presence.Snapshot(sid); err != nil {
		return fmt.Errorf("gateway: intent for session without presence %d: %w", uint64(sid), err)
	}
	allowed, err := h.Presence.AllowIntent(sid, h.Now())
	if err != nil {
		return fmt.Errorf("gateway: intent rate check: %w", err)
	}
	if !allowed {
		return &ClientError{Code: proto.ErrorCodeRateLimited, Message: "gameplay rate limited"}
	}
	if h.Next == nil {
		return nil
	}
	return h.Next.Handle(ctx, sid, header, payload, send)
}

// mapMoveError maps sim-owner movement outcomes to the frozen 202
// registry (spec §7.3.2). Success dispositions are silent upstream;
// only errors map here.
func mapMoveError(err error) error {
	switch {
	case errors.Is(err, sim.ErrMigrationQueueFull),
		errors.Is(err, sim.ErrSimIngressFull),
		errors.Is(err, sim.ErrEngineNotRunning),
		errors.Is(err, sim.ErrEngineStopped):
		return &ClientError{Code: proto.ErrorCodeRetry, Message: "movement unavailable, retry"}
	case errors.Is(err, sim.ErrInvalidMoveYaw),
		errors.Is(err, sim.ErrFutureInputTick),
		errors.Is(err, sim.ErrAmbiguousInputSeq),
		errors.Is(err, sim.ErrAmbiguousSampleTick):
		return &ClientError{Code: proto.ErrorCodeProtocol, Message: "invalid movement"}
	case errors.Is(err, sim.ErrEntityNotFound):
		// Gateway presence and sim ownership diverged: internal
		// fail-closed, never a client invalid_handle.
		return fmt.Errorf("gateway: move for missing sim entity: %w", err)
	default:
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("gateway: move ingress context: %w", err)
		}
		return fmt.Errorf("gateway: move ingress: %w", err)
	}
}
