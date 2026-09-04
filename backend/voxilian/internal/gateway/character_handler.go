package gateway

import (
	"context"
	"errors"
	"fmt"

	"github.com/dlukt/voxilian/internal/character"
	"github.com/dlukt/voxilian/internal/proto"
	"github.com/dlukt/voxilian/internal/session"
)

// CharacterService is the narrow gateway-facing character domain seam.
// *character.Service satisfies it structurally; the store package never
// appears here, preserving the gateway dependency boundary.
type CharacterService interface {
	List(
		ctx context.Context,
		accountID int64,
	) ([]character.ListEntry, error)

	Create(
		ctx context.Context,
		accountID int64,
		req character.CreateRequest,
	) (int64, error)

	FindBySlot(
		ctx context.Context,
		accountID int64,
		slot uint8,
	) (character.Descriptor, error)

	Delete(
		ctx context.Context,
		id int64,
		expectedRevision int64,
	) (int64, error)
}

// WorldExit flushes world-side character state on leave_world: quiesce
// the character, clear AOI, drop presence, flush dirty durable state.
// T3b defines only this seam (plus test fakes); the real world
// implementation lands later. It must never import internal/world or
// internal/sim.
type WorldExit interface {
	ExitWorld(
		ctx context.Context,
		sid session.ID,
		accountID int64,
		characterID int64,
	) error
}

// WorldExitFunc adapts a plain function to a WorldExit (tests, future
// wiring).
type WorldExitFunc func(
	ctx context.Context,
	sid session.ID,
	accountID int64,
	characterID int64,
) error

// ExitWorld implements WorldExit.
func (f WorldExitFunc) ExitWorld(
	ctx context.Context,
	sid session.ID,
	accountID int64,
	characterID int64,
) error {
	return f(ctx, sid, accountID, characterID)
}

// CharacterHandler owns exactly opcodes 121, 122, 123, and 126. Every
// other allowed opcode delegates to Next (124 enter_world belongs
// wholly to M3-T4; gameplay/ack belong to later tasks). It holds no
// WebSocket connection: all replies go through SendFunc, and
// machine-readable failures use ClientError.
type CharacterHandler struct {
	Characters CharacterService
	Registry   *session.Registry
	WorldExit  WorldExit
	Next       MessageHandler
}

// NewCharacterHandler wires a CharacterHandler. Characters, Registry,
// and WorldExit are required (a missing WorldExit must never silently
// succeed); Next may be nil, in which case unowned allowed opcodes are
// consumed without a reply.
func NewCharacterHandler(
	characters CharacterService,
	registry *session.Registry,
	worldExit WorldExit,
	next MessageHandler,
) (*CharacterHandler, error) {
	if characters == nil {
		return nil, errors.New("gateway: character service is required")
	}
	if registry == nil {
		return nil, errors.New("gateway: session registry is required")
	}
	if worldExit == nil {
		return nil, errors.New("gateway: world exit seam is required")
	}
	return &CharacterHandler{
		Characters: characters,
		Registry:   registry,
		WorldExit:  worldExit,
		Next:       next,
	}, nil
}

// Handle implements MessageHandler.
func (h *CharacterHandler) Handle(
	ctx context.Context,
	sid session.ID,
	header proto.Header,
	payload *proto.Decoder,
	send SendFunc,
) error {
	switch header.Opcode {
	case proto.OpcodeCharacterList:
		return h.list(ctx, sid, payload, send)
	case proto.OpcodeCharacterCreate:
		return h.create(ctx, sid, payload, send)
	case proto.OpcodeCharacterDelete:
		return h.delete(ctx, sid, payload, send)
	case proto.OpcodeLeaveWorld:
		return h.leave(ctx, sid, payload, send)
	default:
		// 124 enter_world is owned wholly by M3-T4; 102–120 and 125
		// belong to later tasks. Delegate unchanged.
		if h.Next == nil {
			return nil
		}
		return h.Next.Handle(ctx, sid, header, payload, send)
	}
}

// snapshot loads the live session or fails internal (the server gate
// already established it; absence is an invariant failure).
func (h *CharacterHandler) snapshot(sid session.ID) (session.Snapshot, error) {
	snap, ok := h.Registry.Get(sid)
	if !ok {
		return session.Snapshot{}, fmt.Errorf("gateway: character op for vanished session %d", uint64(sid))
	}
	return snap, nil
}

// list serves 121: decode, list by the session's account, reply 216.
// The client never supplies an account identifier.
func (h *CharacterHandler) list(
	ctx context.Context,
	sid session.ID,
	payload *proto.Decoder,
	send SendFunc,
) error {
	if _, err := proto.DecodeCharacterListRequest(payload); err != nil {
		return &ClientError{Code: proto.ErrorCodeProtocol, Message: "malformed character_list"}
	}
	snap, err := h.snapshot(sid)
	if err != nil {
		return err
	}
	entries, err := h.Characters.List(ctx, snap.AccountID)
	if err != nil {
		if errors.Is(err, character.ErrPersistence) {
			return &ClientError{Code: proto.ErrorCodeRetry, Message: "character list unavailable"}
		}
		return fmt.Errorf("gateway: character list: %w", err)
	}
	summaries := make([]proto.CharacterSummary, 0, len(entries))
	for _, e := range entries {
		summaries = append(summaries, proto.CharacterSummary{
			Slot:     e.Slot,
			CharName: e.Name,
			Level:    e.Level,
		})
	}
	return send(proto.OpcodeCharacterListResult, proto.MessageVersion1, func(enc *proto.Encoder) error {
		proto.CharacterList{Characters: summaries}.Encode(enc)
		return nil
	})
}

// create serves 122: decode exactly once, convert to the domain
// request (no duplicate validation here), and map every domain error
// to its frozen wire form.
func (h *CharacterHandler) create(
	ctx context.Context,
	sid session.ID,
	payload *proto.Decoder,
	send SendFunc,
) error {
	req, err := proto.DecodeCharacterCreate(payload)
	if err != nil {
		return &ClientError{Code: proto.ErrorCodeProtocol, Message: "malformed character_create"}
	}
	snap, err := h.snapshot(sid)
	if err != nil {
		return err
	}
	_, err = h.Characters.Create(ctx, snap.AccountID, character.CreateRequest{
		Slot:   req.Slot,
		Name:   req.Name,
		Gender: req.Gender,
		Face: character.Face{
			HairStyle: req.Face.HairStyle,
			HairColor: req.Face.HairColor,
			SkinTone:  req.Face.SkinTone,
			Parts:     req.Face.Parts,
		},
		Stats:    req.Stats,
		SpellIDs: req.Spells,
		SkillIDs: req.Skills,
	})
	if err != nil {
		switch {
		case errors.Is(err, character.ErrInvalidSlot),
			errors.Is(err, character.ErrInvalidName):
			return sendCharacterOp(send, proto.CharacterOpCreate, proto.CharacterOpRejected)
		case errors.Is(err, character.ErrNameUnavailable):
			return &ClientError{Code: proto.ErrorCodeNameTaken, Message: "character name unavailable"}
		case errors.Is(err, character.ErrSlotOccupied):
			return &ClientError{Code: proto.ErrorCodeSlotOccupied, Message: "character slot occupied"}
		case errors.Is(err, character.ErrBadStats):
			return &ClientError{Code: proto.ErrorCodeBadStats, Message: "invalid character stats"}
		case errors.Is(err, character.ErrBadBudget):
			return &ClientError{Code: proto.ErrorCodeBadBudget, Message: "invalid ability budget"}
		case errors.Is(err, character.ErrPersistence):
			return &ClientError{Code: proto.ErrorCodeRetry, Message: "character creation unavailable"}
		default:
			return fmt.Errorf("gateway: character create: %w", err)
		}
	}
	// Success changes nothing else: no state transition, no bind, no
	// world entry, no DB ID on the wire. The session stays
	// AUTHENTICATED.
	return sendCharacterOp(send, proto.CharacterOpCreate, proto.CharacterOpOK)
}

// deleteResult is the outcome of the guard-held delete decision:
// exactly one of rejected/done/cerr is set; err is internal.
type deleteResult struct {
	rejected bool
	done     bool
	cerr     *ClientError
}

// delete serves 123. The account lifecycle guard covers the whole
// critical decision (lookup → in-use check → CAS); all socket writes
// happen after the guard is released.
func (h *CharacterHandler) delete(
	ctx context.Context,
	sid session.ID,
	payload *proto.Decoder,
	send SendFunc,
) error {
	req, err := proto.DecodeCharacterDelete(payload)
	if err != nil {
		return &ClientError{Code: proto.ErrorCodeProtocol, Message: "malformed character_delete"}
	}
	snap, err := h.snapshot(sid)
	if err != nil {
		return err
	}
	unlock := h.Registry.LockAccount(snap.AccountID)
	res, derr := h.planDelete(ctx, snap.AccountID, req.Slot)
	unlock()
	if derr != nil {
		return derr
	}
	if res.cerr != nil {
		return res.cerr
	}
	if res.rejected {
		return sendCharacterOp(send, proto.CharacterOpDelete, proto.CharacterOpRejected)
	}
	return sendCharacterOp(send, proto.CharacterOpDelete, proto.CharacterOpOK)
}

// planDelete runs under the account lifecycle guard and performs no
// network I/O: look up the slot, fail closed on any live binding in
// CHARACTER_SELECTED/IN_WORLD (without touching persistence), then
// CAS-delete.
func (h *CharacterHandler) planDelete(
	ctx context.Context,
	accountID int64,
	slot uint8,
) (deleteResult, error) {
	desc, err := h.Characters.FindBySlot(ctx, accountID, slot)
	if err != nil {
		switch {
		case errors.Is(err, character.ErrInvalidSlot),
			errors.Is(err, character.ErrNotFound):
			return deleteResult{rejected: true}, nil
		case errors.Is(err, character.ErrPersistence):
			return deleteResult{cerr: &ClientError{
				Code:    proto.ErrorCodeRetry,
				Message: "character delete unavailable",
			}}, nil
		default:
			return deleteResult{}, fmt.Errorf("gateway: character delete lookup: %w", err)
		}
	}
	if bound, ok := h.Registry.GetByCharacter(desc.ID); ok {
		switch bound.State {
		case session.StateCharacterSelected, session.StateInWorld:
			return deleteResult{cerr: &ClientError{
				Code:    proto.ErrorCodeCharacterInUse,
				Message: "character is in use",
			}}, nil
		default:
			return deleteResult{}, fmt.Errorf(
				"gateway: character %d bound to session %d in %s",
				desc.ID, uint64(bound.ID), bound.State)
		}
	}
	if _, err := h.Characters.Delete(ctx, desc.ID, desc.Revision); err != nil {
		if errors.Is(err, character.ErrPersistence) {
			return deleteResult{cerr: &ClientError{
				Code:    proto.ErrorCodeRetry,
				Message: "character delete unavailable",
			}}, nil
		}
		return deleteResult{}, fmt.Errorf("gateway: character delete: %w", err)
	}
	return deleteResult{done: true}, nil
}

// leave serves 126: re-read state under the account guard, flush
// through WorldExit, then atomically unbind + transition. Success is
// silent; failure leaves the session fully bound in IN_WORLD.
func (h *CharacterHandler) leave(
	ctx context.Context,
	sid session.ID,
	payload *proto.Decoder,
	_ SendFunc,
) error {
	if _, err := proto.DecodeLeaveWorld(payload); err != nil {
		return &ClientError{Code: proto.ErrorCodeProtocol, Message: "malformed leave_world"}
	}
	snap, err := h.snapshot(sid)
	if err != nil {
		return err
	}
	unlock := h.Registry.LockAccount(snap.AccountID)
	cerr, derr := h.leaveLocked(ctx, sid)
	unlock()
	if derr != nil {
		return derr
	}
	if cerr != nil {
		return cerr
	}
	return nil // success: silent, no reply frame.
}

// leaveLocked runs under the account lifecycle guard: re-read the
// session (never trust the pre-guard snapshot), require IN_WORLD with
// a binding, flush via WorldExit, then atomically complete. A
// WorldExit failure returns a retry ClientError with the registry
// untouched; a CompleteLeaveWorld failure is an internal invariant
// failure (connection terminates; leave is never claimed).
func (h *CharacterHandler) leaveLocked(
	ctx context.Context,
	sid session.ID,
) (*ClientError, error) {
	cur, ok := h.Registry.Get(sid)
	if !ok {
		return nil, fmt.Errorf("gateway: leave for vanished session %d", uint64(sid))
	}
	if cur.State != session.StateInWorld || !cur.HasCharacter {
		return nil, fmt.Errorf("gateway: leave in %s bound=%v",
			cur.State, cur.HasCharacter)
	}
	if err := h.WorldExit.ExitWorld(ctx, sid, cur.AccountID, cur.CharacterID); err != nil {
		return &ClientError{Code: proto.ErrorCodeRetry, Message: "leave world unavailable"}, nil
	}
	if err := h.Registry.CompleteLeaveWorld(sid, cur.CharacterID); err != nil {
		return nil, fmt.Errorf("gateway: complete leave after flush: %w", err)
	}
	return nil, nil
}

// sendCharacterOp emits one 217 response with frozen named constants.
func sendCharacterOp(send SendFunc, op, ok uint8) error {
	return send(proto.OpcodeCharacterOp, proto.MessageVersion1, func(enc *proto.Encoder) error {
		proto.CharacterOp{Op: op, OK: ok}.Encode(enc)
		return nil
	})
}
