package gateway

import (
	"context"
	"errors"
	"fmt"

	"github.com/dlukt/voxilian/internal/character"
	"github.com/dlukt/voxilian/internal/proto"
	"github.com/dlukt/voxilian/internal/session"
)

// CharacterLookup is the narrow slot-lookup seam EnterWorldHandler
// needs. *character.Service (and the gateway CharacterService)
// satisfies it structurally; no Store dependency leaks here.
type CharacterLookup interface {
	FindBySlot(
		ctx context.Context,
		accountID int64,
		slot uint8,
	) (character.Descriptor, error)
}

// BaselineSink exposes ONLY the three baseline event kinds a provider
// may emit. There is deliberately no generic opcode callback: 217,
// 219, and 202 are lifecycle/control frames owned by the gateway, so
// a world provider can never send them.
type BaselineSink interface {
	CellSnapshot(proto.CellSnapshot) error
	ChunkFragment(proto.ChunkFragment) error
	ShopList(proto.ShopList) error
}

// BaselineProvider streams one character baseline in provider-chosen
// order through the sink. M3 uses fakes/test doubles; M10-T4 supplies
// real cells, classic-mode chunks, and nearby vendor listings.
type BaselineProvider interface {
	StreamBaseline(
		ctx context.Context,
		sid session.ID,
		accountID int64,
		characterID int64,
		sink BaselineSink,
	) error
}

// BaselineProviderFunc adapts a plain function to a BaselineProvider
// (tests, future wiring).
type BaselineProviderFunc func(
	ctx context.Context,
	sid session.ID,
	accountID int64,
	characterID int64,
	sink BaselineSink,
) error

// StreamBaseline implements BaselineProvider.
func (f BaselineProviderFunc) StreamBaseline(
	ctx context.Context,
	sid session.ID,
	accountID int64,
	characterID int64,
	sink BaselineSink,
) error {
	return f(ctx, sid, accountID, characterID, sink)
}

// baselineWriteError marks a BaselineProvider failure caused by a
// failed sink/send write, as opposed to an operational provider
// failure. The sink wraps send errors itself so the distinction
// survives provider propagation (even through further %w wrapping):
// write failures terminate the connection, operational failures roll
// back with a 202 retry.
type baselineWriteError struct {
	err error
}

func (e *baselineWriteError) Error() string {
	return fmt.Sprintf("gateway: baseline sink write: %v", e.err)
}

func (e *baselineWriteError) Unwrap() error {
	return e.err
}

// gatewayBaselineSink adapts SendFunc to BaselineSink: synchronous,
// unbuffered, provider order preserved exactly. Every frame uses
// MessageVersion1; sink send failures surface as *baselineWriteError.
type gatewayBaselineSink struct {
	send SendFunc
}

// CellSnapshot implements BaselineSink (opcode 203).
func (s gatewayBaselineSink) CellSnapshot(m proto.CellSnapshot) error {
	if err := s.send(proto.OpcodeCellSnapshot, proto.MessageVersion1,
		func(e *proto.Encoder) error {
			m.Encode(e)
			return nil
		}); err != nil {
		return &baselineWriteError{err: err}
	}
	return nil
}

// ChunkFragment implements BaselineSink (opcode 218).
func (s gatewayBaselineSink) ChunkFragment(m proto.ChunkFragment) error {
	if err := s.send(proto.OpcodeChunkFragment, proto.MessageVersion1,
		func(e *proto.Encoder) error {
			m.Encode(e)
			return nil
		}); err != nil {
		return &baselineWriteError{err: err}
	}
	return nil
}

// ShopList implements BaselineSink (opcode 220).
func (s gatewayBaselineSink) ShopList(m proto.ShopList) error {
	if err := s.send(proto.OpcodeShopList, proto.MessageVersion1,
		func(e *proto.Encoder) error {
			m.Encode(e)
			return nil
		}); err != nil {
		return &baselineWriteError{err: err}
	}
	return nil
}

// EnterWorldHandler owns exactly opcode 124 enter_world. All other
// allowed opcodes delegate to Next unchanged (121/122/123/126 belong
// to CharacterHandler upstream; gameplay/ack belong downstream). It
// holds no WebSocket connection: replies go through SendFunc.
type EnterWorldHandler struct {
	Characters CharacterLookup
	Registry   *session.Registry
	Baseline   BaselineProvider
	Next       MessageHandler
}

// NewEnterWorldHandler wires an EnterWorldHandler. Characters,
// Registry, and Baseline are required (no silent no-op provider);
// Next may be nil, in which case unowned allowed opcodes are consumed
// without a reply.
func NewEnterWorldHandler(
	characters CharacterLookup,
	registry *session.Registry,
	baseline BaselineProvider,
	next MessageHandler,
) (*EnterWorldHandler, error) {
	if characters == nil {
		return nil, errors.New("gateway: character lookup is required")
	}
	if registry == nil {
		return nil, errors.New("gateway: session registry is required")
	}
	if baseline == nil {
		return nil, errors.New("gateway: baseline provider is required")
	}
	return &EnterWorldHandler{
		Characters: characters,
		Registry:   registry,
		Baseline:   baseline,
		Next:       next,
	}, nil
}

// Handle implements MessageHandler.
func (h *EnterWorldHandler) Handle(
	ctx context.Context,
	sid session.ID,
	header proto.Header,
	payload *proto.Decoder,
	send SendFunc,
) error {
	if header.Opcode != proto.OpcodeEnterWorld {
		if h.Next == nil {
			return nil
		}
		return h.Next.Handle(ctx, sid, header, payload, send)
	}
	return h.enter(ctx, sid, payload, send)
}

// enter serves 124. The account lifecycle guard is released before any
// simple-failure response (invalid/empty slot, lookup retry, staged
// duplicate retry); once the handler commits to BeginEnterWorld the
// guard intentionally stays held through baseline emission and the
// world_ready barrier (spec §6.1.2) — the whole baseline is the
// lifecycle transaction being serialized, so this is the deliberate
// exception to the delete path's no-network-under-guard rule.
func (h *EnterWorldHandler) enter(
	ctx context.Context,
	sid session.ID,
	payload *proto.Decoder,
	send SendFunc,
) error {
	req, err := proto.DecodeEnterWorld(payload)
	if err != nil {
		return &ClientError{Code: proto.ErrorCodeProtocol, Message: "malformed enter_world"}
	}
	snap, ok := h.Registry.Get(sid)
	if !ok {
		return fmt.Errorf("gateway: enter for vanished session %d", uint64(sid))
	}
	unlock := h.Registry.LockAccount(snap.AccountID)
	// Re-read under guard: never trust the pre-lock snapshot for
	// state, binding, or account identity.
	cur, ok := h.Registry.Get(sid)
	if !ok || cur.State != session.StateAuthenticated ||
		cur.HasCharacter || cur.AccountID != snap.AccountID {
		unlock()
		return fmt.Errorf("gateway: enter re-read state %+v", cur)
	}
	desc, err := h.Characters.FindBySlot(ctx, cur.AccountID, req.Slot)
	if err != nil {
		switch {
		case errors.Is(err, character.ErrInvalidSlot),
			errors.Is(err, character.ErrNotFound):
			unlock()
			return sendCharacterOp(send, proto.CharacterOpEnterWorld, proto.CharacterOpRejected)
		case errors.Is(err, character.ErrPersistence):
			unlock()
			return &ClientError{Code: proto.ErrorCodeRetry, Message: "enter_world unavailable"}
		default:
			unlock()
			return fmt.Errorf("gateway: enter lookup: %w", err)
		}
	}
	if _, found, err := h.Registry.WorldSessionForAccount(cur.AccountID, sid); err != nil {
		unlock()
		return fmt.Errorf("gateway: enter arbitration: %w", err)
	} else if found {
		// Deliberate T4a staging (spec §6.1.2): a second same-account
		// world-active session means retry, not takeover. T4b replaces
		// this branch with quiesce/flush/kick semantics. Nothing
		// mutates here: no kick, no flush, no unbind, no baseline.
		unlock()
		return &ClientError{Code: proto.ErrorCodeRetry, Message: "account already in world"}
	}
	// Commit point: the guard now spans 217, baseline, 219, complete.
	if err := h.Registry.BeginEnterWorld(sid, desc.ID); err != nil {
		// Includes ErrCharacterInUse after arbitration: same-account
		// means the world query just missed it (staging race) and a
		// foreign owner means an ownership invariant failure. Either
		// way fail closed — never wire character_in_use here.
		unlock()
		return fmt.Errorf("gateway: begin enter: %w", err)
	}
	if err := sendCharacterOp(send, proto.CharacterOpEnterWorld, proto.CharacterOpOK); err != nil {
		_ = h.Registry.AbortEnterWorld(sid, desc.ID)
		unlock()
		return err
	}
	sink := gatewayBaselineSink{send: send}
	if err := h.Baseline.StreamBaseline(ctx, sid, cur.AccountID, desc.ID, sink); err != nil {
		_ = h.Registry.AbortEnterWorld(sid, desc.ID)
		unlock()
		var werr *baselineWriteError
		if errors.As(err, &werr) {
			return fmt.Errorf("gateway: enter baseline write: %w", err)
		}
		return &ClientError{Code: proto.ErrorCodeRetry, Message: "enter_world baseline unavailable"}
	}
	if err := send(proto.OpcodeWorldReady, proto.MessageVersion1,
		func(e *proto.Encoder) error {
			proto.WorldReady{}.Encode(e)
			return nil
		}); err != nil {
		_ = h.Registry.AbortEnterWorld(sid, desc.ID)
		unlock()
		return err
	}
	// Only after the 219 write succeeds does the registry complete, so
	// no inbound gameplay can run before the client holds the barrier
	// AND the registry is IN_WORLD. A failure here is an internal
	// invariant failure: terminate, never claim success.
	if err := h.Registry.CompleteEnterWorld(sid, desc.ID); err != nil {
		unlock()
		return fmt.Errorf("gateway: complete enter after world_ready: %w", err)
	}
	unlock()
	return nil
}
