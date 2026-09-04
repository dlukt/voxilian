package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

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
// holds no WebSocket connection: current-session replies go through
// SendFunc, and the cross-session kicked frame goes through the old
// session's own session.Connection.
type EnterWorldHandler struct {
	Characters CharacterLookup
	Registry   *session.Registry
	Baseline   BaselineProvider
	WorldExit  WorldExit
	Tick       TickFunc
	Next       MessageHandler
}

// EnterWorldHandlerDeps wires an EnterWorldHandler without a growing
// positional constructor. Characters, Registry, Baseline, WorldExit,
// and Tick are required (no silent no-op defaults); Next may be nil,
// in which case unowned allowed opcodes are consumed without a reply.
// WorldExit must be the SAME instance the CharacterHandler chain uses:
// normal leave_world and forced takeover share one world-side
// quiesce/flush contract.
type EnterWorldHandlerDeps struct {
	Characters CharacterLookup
	Registry   *session.Registry
	Baseline   BaselineProvider
	WorldExit  WorldExit
	Tick       TickFunc
	Next       MessageHandler
}

// NewEnterWorldHandler wires an EnterWorldHandler from deps.
func NewEnterWorldHandler(deps EnterWorldHandlerDeps) (*EnterWorldHandler, error) {
	if deps.Characters == nil {
		return nil, errors.New("gateway: character lookup is required")
	}
	if deps.Registry == nil {
		return nil, errors.New("gateway: session registry is required")
	}
	if deps.Baseline == nil {
		return nil, errors.New("gateway: baseline provider is required")
	}
	if deps.WorldExit == nil {
		return nil, errors.New("gateway: world exit seam is required")
	}
	if deps.Tick == nil {
		return nil, errors.New("gateway: tick source is required")
	}
	return &EnterWorldHandler{
		Characters: deps.Characters,
		Registry:   deps.Registry,
		Baseline:   deps.Baseline,
		WorldExit:  deps.WorldExit,
		Tick:       deps.Tick,
		Next:       deps.Next,
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

// enter serves 124. The account lifecycle guard spans the ENTIRE
// logical operation (spec §6.1.2): re-read, requested-character
// lookup, old-session discovery, old WorldExit flush, old unbind,
// old retirement/kick, new BeginEnterWorld, 217, baseline, 219, and
// new CompleteEnterWorld — so no deletion, leave, or other enter can
// interleave. The guard is released before any simple-failure
// response (invalid/empty slot, lookup retry, takeover flush retry);
// once the handler commits to BeginEnterWorld the guard intentionally
// stays held through baseline emission and the world_ready barrier —
// the whole baseline is the lifecycle transaction being serialized,
// so this is the deliberate exception to the delete path's
// no-network-under-guard rule.
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
	// Resolve the requested character BEFORE touching the old session:
	// an invalid/empty slot or an unavailable lookup must never kick
	// somebody merely because the new request itself was invalid.
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
	// Duplicate-login takeover (spec §6.1.2): a previous same-account
	// world session is flushed, unbound, kicked, and retired BEFORE
	// the new baseline can load stale PG state.
	if old, found, qerr := h.Registry.WorldSessionForAccount(cur.AccountID, sid); qerr != nil {
		unlock()
		return fmt.Errorf("gateway: enter arbitration: %w", qerr)
	} else if found {
		if terr := h.takeoverOld(ctx, cur.AccountID, old); terr != nil {
			unlock()
			return terr
		}
	}
	// Commit point: the guard now spans 217, baseline, 219, complete.
	if err := h.Registry.BeginEnterWorld(sid, desc.ID); err != nil {
		// Includes ErrCharacterInUse after takeover: a foreign owner
		// (never same-account, just flushed) is an ownership invariant
		// failure. Fail closed — never wire character_in_use here.
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

// kickWriteBudget bounds the best-effort kicked control write so a
// broken or slow old client can never hold the account lifecycle guard
// indefinitely. It is an M3 control-write budget, not a wire contract;
// T5 owns the real slow-client budgets and no config drives it.
var kickWriteBudget = 500 * time.Millisecond

// kickedMessage is the static kicked diagnostic. Client logic branches
// on ErrorCodeKicked only; the message must never carry account,
// character, or session identifiers.
const kickedMessage = "session replaced by another login"

// takeoverOld quiesces and retires a previous same-account world
// session (spec §6.1.2) while the caller holds the account lifecycle
// guard. The ordering is the stale-PG safety barrier: flush the OLD
// session's world state first, then atomically release its binding,
// then kick/retire it — only afterward may the replacement run its own
// baseline. A WorldExit failure maps to retry with the old session
// fully untouched (still IN_WORLD, bound, registered, un-kicked, no
// new baseline); every other failure is an internal invariant failure
// that must fail closed rather than proceed with uncertain state.
func (h *EnterWorldHandler) takeoverOld(
	ctx context.Context,
	accountID int64,
	old session.Snapshot,
) error {
	if old.AccountID != accountID {
		return fmt.Errorf("gateway: takeover account mismatch: %+v", old)
	}
	if old.State != session.StateInWorld {
		// A CHARACTER_SELECTED candidate cannot legitimately survive
		// the account-guard acquisition this handler already holds
		// (the enter path holds the same guard across its entire
		// baseline): fail closed — no flush, no kick, no new baseline.
		return fmt.Errorf("gateway: takeover of %s session %d: %w",
			old.State, uint64(old.ID), session.ErrInvariant)
	}
	if !old.HasCharacter || old.CharacterID == 0 || old.Conn == nil {
		return fmt.Errorf("gateway: takeover candidate invariant: %+v", old)
	}
	// Flush old world state FIRST: no kick, no unbind, and no new
	// BeginEnterWorld/baseline before the old session's dirty state is
	// durably quiesced — a new connection must never load stale PG
	// state while the old one still holds dirty in-memory state.
	if err := h.WorldExit.ExitWorld(ctx, old.ID, old.AccountID, old.CharacterID); err != nil {
		return &ClientError{Code: proto.ErrorCodeRetry, Message: "account world flush unavailable"}
	}
	// Atomically release the old binding through the same T3b
	// primitive a normal leave_world uses (never UnbindCharacter plus a
	// state CAS as two operations). ErrNotFound means the old
	// transport/session already vanished concurrently (Registry.Remove
	// does not take the account guard) AFTER the flush committed —
	// safe to continue the takeover. Any other failure leaves
	// uncertain state and must fail closed.
	if err := h.Registry.CompleteLeaveWorld(old.ID, old.CharacterID); err != nil {
		if !errors.Is(err, session.ErrNotFound) {
			return fmt.Errorf("gateway: takeover unbind: %w", err)
		}
	}
	h.retireKicked(old)
	return nil
}

// retireKicked performs the authoritative retirement of an
// already-flushed, already-unbound old session: a best-effort 202
// kicked frame written inside the old connection's writer slot — with
// the registry removal inside the SAME slot, so any later old-session
// writer that obtains the slot must fail its NextServerSeq rather than
// emit another application frame — followed by an immediate forced
// close. Retirement is NOT best effort: a lost kick frame (busy or
// broken old writer) or a failed CloseNow still removes the session,
// and the old ServeHTTP deferred cleanup may Remove again safely.
func (h *EnterWorldHandler) retireKicked(old session.Snapshot) {
	// Deliberately derived from Background, not the new client's
	// request context: once the old flush has committed, retirement
	// must complete even if the new client disconnects at this exact
	// moment.
	ctx, cancel := context.WithTimeout(context.Background(), kickWriteBudget)
	defer cancel()
	builder := buildSessionFrame(h.Registry, h.Tick, old.ID,
		proto.OpcodeError, proto.MessageVersion1,
		func(e *proto.Encoder) error {
			proto.ErrorMessage{
				Code:    proto.ErrorCodeKicked,
				Message: kickedMessage,
			}.Encode(e)
			return nil
		})
	err := old.Conn.WriteBinary(ctx, func() ([]byte, error) {
		frame, berr := builder()
		if berr != nil {
			return nil, berr
		}
		h.Registry.Remove(old.ID)
		return frame, nil
	})
	if err != nil {
		// The kick frame is best effort; retirement is not. This also
		// covers a writer that never obtained serialization before the
		// bounded context expired.
		h.Registry.Remove(old.ID)
	}
	if cerr := old.Conn.CloseNow(); cerr != nil {
		// A physical close failure can never resurrect a flushed,
		// unbound, retired old session; it is transport cleanup only.
		slog.Warn("gateway: kicked connection close failed",
			"session", uint64(old.ID), "err", cerr)
	}
}
