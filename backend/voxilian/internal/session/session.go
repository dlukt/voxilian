package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dlukt/voxilian/internal/proto"
)

// State is the four-state session lifecycle (spec §6.1):
// CONNECTED → AUTHENTICATED → CHARACTER_SELECTED → IN_WORLD.
// Only the CONNECTED → AUTHENTICATED transition is implemented in this
// package (via Authenticate); later tasks own the remaining transitions
// and perform them through CompareAndSetState while holding the
// per-account lifecycle guard.
type State uint8

const (
	StateConnected State = iota
	StateAuthenticated
	StateCharacterSelected
	StateInWorld
)

// String returns the stable log/test representation of a State.
func (s State) String() string {
	switch s {
	case StateConnected:
		return "CONNECTED"
	case StateAuthenticated:
		return "AUTHENTICATED"
	case StateCharacterSelected:
		return "CHARACTER_SELECTED"
	case StateInWorld:
		return "IN_WORLD"
	default:
		return fmt.Sprintf("State(%d)", uint8(s))
	}
}

// IsClientOpcode reports whether opcode is a recognized client-to-server
// opcode: exactly 100–126. Server-to-client opcodes (200–220) and every
// other value are not client opcodes.
func IsClientOpcode(opcode uint16) bool {
	return opcode >= proto.OpcodeHello && opcode <= proto.OpcodeLeaveWorld
}

// Allowed reports whether a structurally valid, recognized C→S opcode
// may be processed in the given lifecycle state (spec §6.1). It is a
// pure explicit table; it never relies on the numeric order of states.
// Unknown opcodes always return false: callers map those (and S→C
// opcodes sent by a client) to protocol_error, while a recognized
// client opcode in the wrong state maps to bad_state.
func Allowed(s State, opcode uint16) bool {
	if !IsClientOpcode(opcode) {
		return false
	}
	switch opcode {
	case proto.OpcodeHello: // 100
		return s == StateConnected
	case proto.OpcodeReauth: // 101
		return s == StateAuthenticated ||
			s == StateCharacterSelected ||
			s == StateInWorld
	case proto.OpcodeCharacterList: // 121
		return s == StateAuthenticated ||
			s == StateCharacterSelected ||
			s == StateInWorld
	case proto.OpcodeCharacterCreate, // 122
		proto.OpcodeCharacterDelete, // 123
		proto.OpcodeEnterWorld:      // 124
		return s == StateAuthenticated
	case proto.OpcodeAck: // 125
		return s == StateCharacterSelected || s == StateInWorld
	case proto.OpcodeLeaveWorld: // 126
		return s == StateInWorld
	default: // 102–120 gameplay intents
		return s == StateInWorld
	}
}

// BinaryFrameBuilder constructs exactly one complete binary frame. It
// runs only while its caller owns the connection's writer slot, so it
// is where per-connection S→C sequence allocation and frame encoding
// belong: the order builders execute is exactly the order their frames
// are physically written. It returns raw bytes only — never proto
// types — so this package stays transport- and protocol-agnostic.
type BinaryFrameBuilder func() ([]byte, error)

// Connection is the transport-neutral session-level handle the registry
// retains so the gateway can deliver frames to — and forcibly retire —
// a session from any goroutine, including another session's handler
// (duplicate-login takeover). Implementations own ONE
// application-writer serialization per connection:
//
// WriteBinary acquires that serialization, invokes build exactly once
// (only after ownership is obtained), writes the returned bytes as one
// binary message, then releases the serialization. If ctx is cancelled
// before serialization is obtained, build is never invoked.
//
// CloseNow closes the transport immediately WITHOUT waiting for the
// writer serialization, so a stuck or blocked application write can
// never prevent a forced takeover; the graceful Close keeps its close
// handshake. This package never imports a WebSocket implementation.
type Connection interface {
	WriteBinary(ctx context.Context, build BinaryFrameBuilder) error
	Close(reason string) error
	CloseNow() error
}

// ID is a process-local monotonic session identifier. IDs are
// internal/ephemeral: they never appear on the wire, are never reused
// during the lifetime of one Registry, and are not persisted.
type ID uint64

// Snapshot is an immutable copy of one session's registry state.
// Callers never observe mutable map entries.
type Snapshot struct {
	ID            ID
	Sub           string
	AccountID     int64
	Authenticated bool
	CharacterID   int64
	HasCharacter  bool
	Conn          Connection
	State         State
	TokenExp      time.Time
}

// Stable registry errors for errors.Is/errors.As matching.
var (
	// ErrNotFound reports an unknown session ID.
	ErrNotFound = errors.New("session_not_found")
	// ErrBadState reports a state-precondition failure (authenticate a
	// non-CONNECTED session, reauthenticate a CONNECTED session, or a
	// CompareAndSetState expected-state mismatch).
	ErrBadState = errors.New("bad_state")
	// ErrIdentityMismatch reports a reauthentication result carrying a
	// different sub or accountID than the established session identity.
	ErrIdentityMismatch = errors.New("identity_mismatch")
	// ErrCharacterInUse reports binding a character that is already
	// bound to a different live session.
	ErrCharacterInUse = errors.New("character_in_use")
	// ErrInvariant reports a registry invariant violation that must
	// fail closed (e.g. more than one world-active session for one
	// account). It is never a client error mapping.
	ErrInvariant = errors.New("invariant_violation")
	// ErrAckLag reports the max-unacked flow window is exhausted: the
	// session must fail closed as a slow client (spec §7.1.11). It is an
	// INTERNAL classification consumed by the gateway outbound writer,
	// never a wire error code.
	ErrAckLag = errors.New("ack_lag")
	// ErrFutureAck reports an ACK ahead of the sent-flow sequence (or at
	// an exact half-range distance): invalid, no mutation. The gateway
	// maps it to 202 protocol_error (spec §7.1.11).
	ErrFutureAck = errors.New("future_ack")
)

// Ack flow window bounds (spec §7.1.1): max_unacked_messages is
// validated to 1..1000000 by config; a direct allocator call outside
// that range is an internal configuration failure, never a silent
// disabled window.
const (
	MinUnackedWindow = 1
	MaxUnackedWindow = 1000000
)

// AckDisposition classifies one applied opcode-125 ACK (spec §7.1.11).
// A future ACK is an error (ErrFutureAck), not a disposition.
type AckDisposition uint8

const (
	// AckAdvanced is a valid cumulative advance: lastAck moved forward.
	AckAdvanced AckDisposition = iota
	// AckDuplicate repeated the current lastAck: no-op.
	AckDuplicate
	// AckStale is serially before lastAck: no-op.
	AckStale
)

// String returns the stable log/test representation.
func (d AckDisposition) String() string {
	switch d {
	case AckAdvanced:
		return "advanced"
	case AckDuplicate:
		return "duplicate"
	case AckStale:
		return "stale"
	default:
		return fmt.Sprintf("AckDisposition(%d)", uint8(d))
	}
}

// FlowSnapshot is an immutable copy of one session's ephemeral ACK-flow
// state (spec §7.1.11), for tests and diagnostics. Callers never
// observe mutable entry fields.
type FlowSnapshot struct {
	Active       bool
	LastAck      uint32
	LastFlowSent uint32
}

// entry is the mutable registry record behind one Snapshot. The
// flowActive/lastAck/lastFlowSent triple is the session-scoped ephemeral
// ACK-flow epoch (spec §7.1.11): guarded by the registry lock, never
// persisted, discarded with the session on Remove.
type entry struct {
	id            ID
	sub           string
	accountID     int64
	authenticated bool
	characterID   int64
	hasCharacter  bool
	conn          Connection
	state         State
	tokenExp      time.Time
	serverSeq     atomic.Uint32
	flowActive    bool
	lastAck       uint32
	lastFlowSent  uint32
}

// accountGuard is one ref-counted per-account lifecycle mutex.
type accountGuard struct {
	mu   sync.Mutex
	refs int
}

// Registry is the in-memory session registry (spec §7): sessionID →
// session entry, indexed by sub and by character, plus the per-account
// lifecycle guard keyed by accountID. All index mutations that logically
// belong together happen under the same lock; snapshots returned to
// callers are immutable copies. Use NewRegistry to construct.
type Registry struct {
	mu     sync.RWMutex
	next   ID
	byID   map[ID]*entry
	bySub  map[string]map[ID]struct{}
	byChar map[int64]ID

	guardMu sync.Mutex
	guards  map[int64]*accountGuard
}

// NewRegistry returns an empty session registry.
func NewRegistry() *Registry {
	return &Registry{
		byID:   make(map[ID]*entry),
		bySub:  make(map[string]map[ID]struct{}),
		byChar: make(map[int64]ID),
		guards: make(map[int64]*accountGuard),
	}
}

// Create adds a new CONNECTED session with empty identity (Sub empty,
// AccountID zero, no character, zero TokenExp) and returns its fresh
// session ID. IDs allocate monotonically from 1 and are never reused.
func (r *Registry) Create(conn Connection) ID {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	id := r.next
	r.byID[id] = &entry{id: id, conn: conn, state: StateConnected}
	return id
}

// Get returns an immutable snapshot of the session, or false when the
// ID is unknown (e.g. already removed at disconnect).
func (r *Registry) Get(id ID) (Snapshot, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.byID[id]
	if !ok {
		return Snapshot{}, false
	}
	return snapshotOf(e), true
}

// Len returns the number of live sessions.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byID)
}

// Authenticate performs the one lifecycle transition owned here:
// CONNECTED → AUTHENTICATED. It atomically sets sub, accountID,
// tokenExp, and the sub index. The sub must be non-empty. Later tasks
// (real JWT validation, account provisioning) only change how the
// arguments are produced, not this invariant.
func (r *Registry) Authenticate(id ID, sub string, accountID int64, tokenExp time.Time) error {
	if sub == "" {
		return fmt.Errorf("session: authenticate with empty sub: %w", ErrBadState)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byID[id]
	if !ok {
		return fmt.Errorf("session: authenticate unknown session: %w", ErrNotFound)
	}
	if e.state != StateConnected {
		return fmt.Errorf("session: authenticate in %s: %w", e.state, ErrBadState)
	}
	e.sub = sub
	e.accountID = accountID
	e.authenticated = true
	e.tokenExp = tokenExp
	e.state = StateAuthenticated
	set, ok := r.bySub[sub]
	if !ok {
		set = make(map[ID]struct{})
		r.bySub[sub] = set
	}
	set[id] = struct{}{}
	return nil
}

// Reauthenticate refreshes the token expiry of an established session.
// It MUST NOT change session identity: a result carrying a different
// sub or accountID is rejected with ErrIdentityMismatch and the stored
// identity and expiry are left untouched. Reauthenticating a CONNECTED
// (never-authenticated) session is ErrBadState.
func (r *Registry) Reauthenticate(id ID, sub string, accountID int64, tokenExp time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byID[id]
	if !ok {
		return fmt.Errorf("session: reauthenticate unknown session: %w", ErrNotFound)
	}
	if !e.authenticated {
		return fmt.Errorf("session: reauthenticate in %s: %w", e.state, ErrBadState)
	}
	if e.sub != sub || e.accountID != accountID {
		return fmt.Errorf("session: reauthenticate identity change: %w", ErrIdentityMismatch)
	}
	e.tokenExp = tokenExp
	return nil
}

// SessionsBySub returns the live session IDs currently indexed under
// sub. Several sessions may share one sub (only IN_WORLD play is
// exclusive, enforced by later tasks).
func (r *Registry) SessionsBySub(sub string) []ID {
	r.mu.RLock()
	defer r.mu.RUnlock()
	set := r.bySub[sub]
	out := make([]ID, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	return out
}

// SessionByCharacter returns the session ID the character is bound to,
// or false when the character is not bound to any live session.
func (r *Registry) SessionByCharacter(characterID int64) (ID, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.byChar[characterID]
	return id, ok
}

// GetByCharacter atomically resolves the character index to one
// immutable snapshot of the bound live session: one read lock covers
// the index lookup and the entry read, so callers never observe a torn
// index/entry pair. It returns false when the character is unbound or
// its indexed session is gone (stale index, treated as unbound).
func (r *Registry) GetByCharacter(characterID int64) (Snapshot, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.byChar[characterID]
	if !ok {
		return Snapshot{}, false
	}
	e, ok := r.byID[id]
	if !ok {
		return Snapshot{}, false
	}
	return snapshotOf(e), true
}

// CompleteLeaveWorld atomically completes a leave_world transition
// (spec §6.1): under ONE write lock it verifies the session exists,
// is IN_WORLD, holds a character binding for exactly
// expectedCharacterID, and still owns the character index — then
// removes the index, clears the binding, moves the session to
// AUTHENTICATED, and in the SAME mutation clears the ACK-flow epoch
// (flowActive=false, lastAck=0, lastFlowSent=0, spec §7.1.11) so no
// old ACK debt survives leave/re-enter or full resync. Identity (sub,
// accountID, tokenExp, connection, server seq) is unchanged. No public
// snapshot can show an intermediate AUTHENTICATED-yet-bound or
// IN_WORLD-yet-unbound state. Any precondition failure returns
// ErrBadState/ErrNotFound and leaves the registry untouched.
func (r *Registry) CompleteLeaveWorld(id ID, expectedCharacterID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byID[id]
	if !ok {
		return fmt.Errorf("session: leave unknown session: %w", ErrNotFound)
	}
	if e.state != StateInWorld {
		return fmt.Errorf("session: leave in %s: %w", e.state, ErrBadState)
	}
	if !e.hasCharacter || e.characterID != expectedCharacterID {
		return fmt.Errorf("session: leave character mismatch: %w", ErrBadState)
	}
	if owner, taken := r.byChar[e.characterID]; !taken || owner != id {
		return fmt.Errorf("session: leave index mismatch: %w", ErrBadState)
	}
	delete(r.byChar, e.characterID)
	e.characterID = 0
	e.hasCharacter = false
	e.state = StateAuthenticated
	e.flowActive = false
	e.lastAck = 0
	e.lastFlowSent = 0
	return nil
}

// BeginEnterWorld atomically begins an enter_world operation
// (spec §6.1.2): under ONE write lock it requires an existing
// AUTHENTICATED unbound session and a character free of other bindings,
// then moves to CHARACTER_SELECTED + bound + indexed. Identity, token,
// connection, and server sequence are unchanged. There is never a
// public CHARACTER_SELECTED-yet-unbound or AUTHENTICATED-yet-bound
// state, and any failure mutates nothing. A character bound elsewhere
// reports ErrCharacterInUse; callers must resolve ownership themselves
// and never surface a current-account enter as wire character_in_use.
func (r *Registry) BeginEnterWorld(id ID, characterID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byID[id]
	if !ok {
		return fmt.Errorf("session: begin enter unknown session: %w", ErrNotFound)
	}
	if e.state != StateAuthenticated {
		return fmt.Errorf("session: begin enter in %s: %w", e.state, ErrBadState)
	}
	if e.hasCharacter {
		return fmt.Errorf("session: begin enter already bound: %w", ErrBadState)
	}
	if owner, taken := r.byChar[characterID]; taken {
		if owner == id {
			return fmt.Errorf("session: begin enter inconsistent index: %w", ErrBadState)
		}
		return fmt.Errorf("session: character %d in use: %w", characterID, ErrCharacterInUse)
	}
	// A stale active flow epoch at begin time is an internal invariant
	// failure (spec §7.1.11): the previous epoch must have cleared in
	// CompleteLeaveWorld/Remove. Zero mutation — never carry old ACK
	// debt into a new baseline.
	if e.flowActive {
		return fmt.Errorf("session: begin enter with active flow epoch: %w", ErrInvariant)
	}
	e.state = StateCharacterSelected
	e.characterID = characterID
	e.hasCharacter = true
	r.byChar[characterID] = id
	return nil
}

// AbortEnterWorld atomically rolls a CHARACTER_SELECTED session back
// to the exact pre-enter state (spec §6.1.2): under ONE write lock it
// requires the selected state with a matching binding and matching
// index owner, then deletes the index, clears the binding, and returns
// to AUTHENTICATED. Identity, token, connection, and server sequence
// are unchanged. Any failure mutates nothing.
func (r *Registry) AbortEnterWorld(id ID, expectedCharacterID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byID[id]
	if !ok {
		return fmt.Errorf("session: abort enter unknown session: %w", ErrNotFound)
	}
	if e.state != StateCharacterSelected {
		return fmt.Errorf("session: abort enter in %s: %w", e.state, ErrBadState)
	}
	if !e.hasCharacter || e.characterID != expectedCharacterID {
		return fmt.Errorf("session: abort enter character mismatch: %w", ErrBadState)
	}
	if owner, taken := r.byChar[e.characterID]; !taken || owner != id {
		return fmt.Errorf("session: abort enter index mismatch: %w", ErrBadState)
	}
	delete(r.byChar, e.characterID)
	e.characterID = 0
	e.hasCharacter = false
	e.state = StateAuthenticated
	// No partial epoch survives a failed baseline (spec §7.1.11): the
	// flow state is inactive and cleared.
	e.flowActive = false
	e.lastAck = 0
	e.lastFlowSent = 0
	return nil
}

// CompleteEnterWorld atomically completes an enter_world operation
// after a successfully written 219 world_ready (spec §6.1.2): under
// ONE write lock it requires CHARACTER_SELECTED with a matching
// binding and matching index owner, then moves to IN_WORLD keeping the
// same binding and index — and in the SAME mutation initializes the
// ACK-flow epoch (spec §7.1.11) from the current server S→C sequence,
// which is the already physically-written 219 sequence because the
// caller's synchronous 219 send returned before this call. The
// baseline is captured as-is (never worldReadySeq+1; no sequence is
// allocated here). There is never an observable IN_WORLD interval with
// an uninitialized epoch. Any failure mutates nothing.
func (r *Registry) CompleteEnterWorld(id ID, expectedCharacterID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byID[id]
	if !ok {
		return fmt.Errorf("session: complete enter unknown session: %w", ErrNotFound)
	}
	if e.state != StateCharacterSelected {
		return fmt.Errorf("session: complete enter in %s: %w", e.state, ErrBadState)
	}
	if !e.hasCharacter || e.characterID != expectedCharacterID {
		return fmt.Errorf("session: complete enter character mismatch: %w", ErrBadState)
	}
	if owner, taken := r.byChar[e.characterID]; !taken || owner != id {
		return fmt.Errorf("session: complete enter index mismatch: %w", ErrBadState)
	}
	worldReadySeq := e.serverSeq.Load()
	e.state = StateInWorld
	e.flowActive = true
	e.lastAck = worldReadySeq
	e.lastFlowSent = worldReadySeq
	return nil
}

// WorldSessionForAccount deterministically finds another live session
// of the same account that is currently world-active
// (CHARACTER_SELECTED or IN_WORLD), excluding one session ID
// (normally the caller). The scan runs under ONE read lock and never
// depends on map iteration order: zero candidates return false, one
// returns its immutable snapshot, and more than one is an internal
// invariant failure (ErrInvariant) rather than an arbitrary pick.
// CONNECTED/AUTHENTICATED sessions never count.
func (r *Registry) WorldSessionForAccount(accountID int64, exclude ID) (Snapshot, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var found Snapshot
	count := 0
	for id, e := range r.byID {
		if id == exclude || e.accountID != accountID {
			continue
		}
		if e.state != StateCharacterSelected && e.state != StateInWorld {
			continue
		}
		count++
		if count == 1 {
			found = snapshotOf(e)
		}
	}
	if count > 1 {
		return Snapshot{}, false, fmt.Errorf(
			"session: %d world-active sessions for account %d: %w", count, accountID, ErrInvariant)
	}
	if count == 0 {
		return Snapshot{}, false, nil
	}
	return found, true, nil
}

// BindCharacter binds characterID to the session and records the
// character → session index. Binding a character that is already bound
// to a different live session fails with ErrCharacterInUse and changes
// nothing. Re-binding the session's own character is idempotent.
// Binding does not itself change lifecycle state; gameplay/world
// transitions belong to later tasks.
func (r *Registry) BindCharacter(id ID, characterID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byID[id]
	if !ok {
		return fmt.Errorf("session: bind unknown session: %w", ErrNotFound)
	}
	if owner, taken := r.byChar[characterID]; taken && owner != id {
		return fmt.Errorf("session: character %d in use: %w", characterID, ErrCharacterInUse)
	}
	if e.hasCharacter && e.characterID != characterID {
		delete(r.byChar, e.characterID)
	}
	e.characterID = characterID
	e.hasCharacter = true
	r.byChar[characterID] = id
	return nil
}

// UnbindCharacter removes the session's character binding and its
// character index entry. It is idempotent: a session with no binding
// succeeds without effect. An unknown session is ErrNotFound.
func (r *Registry) UnbindCharacter(id ID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byID[id]
	if !ok {
		return fmt.Errorf("session: unbind unknown session: %w", ErrNotFound)
	}
	if e.hasCharacter {
		delete(r.byChar, e.characterID)
		e.characterID = 0
		e.hasCharacter = false
	}
	return nil
}

// Remove deletes the session and cleans every index (sub, character)
// plus its connection reference. It is idempotent: removing an unknown
// ID succeeds without effect so disconnect cleanup can run exactly once
// per termination path without coordination.
func (r *Registry) Remove(id ID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byID[id]
	if !ok {
		return
	}
	delete(r.byID, id)
	if e.authenticated {
		if set, ok := r.bySub[e.sub]; ok {
			delete(set, id)
			if len(set) == 0 {
				delete(r.bySub, e.sub)
			}
		}
	}
	if e.hasCharacter {
		// Only clear the character index when it still points at this
		// session; a newer binding must never be clobbered by a stale
		// session's cleanup.
		if owner, taken := r.byChar[e.characterID]; taken && owner == id {
			delete(r.byChar, e.characterID)
		}
	}
	e.conn = nil
}

// CompareAndSetState moves the session from expected to next and
// reports whether the swap happened. It never silently overwrites a
// concurrent state: a mismatch leaves the session untouched and
// returns ErrBadState. Unknown IDs return ErrNotFound. This package
// freezes no universal transition graph; future tasks own the actual
// transitions and call this while holding the account lifecycle guard.
func (r *Registry) CompareAndSetState(id ID, expected, next State) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byID[id]
	if !ok {
		return fmt.Errorf("session: cas unknown session: %w", ErrNotFound)
	}
	if e.state != expected {
		return fmt.Errorf("session: cas expected %s have %s: %w", expected, e.state, ErrBadState)
	}
	e.state = next
	return nil
}

// NextServerSeq allocates the session's next independent S→C sequence
// number. Allocation is race-safe, starts deterministically at 1, and
// wraps naturally: after math.MaxUint32 the next value is 0, which is
// not an error. No ACK window, replay, or ordering logic lives here.
// Direct terminal frames (kicked, hard-deadline session_expired) and
// non-queue test connections keep using this path; the NORMAL queued
// writer uses NextFlowControlledServerSeq instead (spec §7.1.11).
func (r *Registry) NextServerSeq(id ID) (uint32, error) {
	r.mu.RLock()
	e, ok := r.byID[id]
	r.mu.RUnlock()
	if !ok {
		return 0, fmt.Errorf("session: seq for unknown session: %w", ErrNotFound)
	}
	return e.serverSeq.Add(1), nil
}

// NextFlowControlledServerSeq is the NORMAL queued writer's S→C
// sequence allocator (spec §7.1.11). It runs inside the connection's
// physical writer slot and is one atomic registry operation against ACK
// application and lifecycle transitions:
//
//   - Not IN_WORLD (CONNECTED/AUTHENTICATED replies, the
//     CHARACTER_SELECTED baseline, and the 219 itself): allocate the
//     next seq normally, tracked=false, lag=0 — no ACK accounting.
//   - IN_WORLD: require an initialized flow epoch (else internal
//     invariant failure). If the current unacked lag already equals or
//     exceeds maxUnacked, return ErrAckLag BEFORE any sequence is
//     allocated — the rejected frame consumes no seq, is never
//     written, and the session fails closed as a slow client.
//     Otherwise allocate the next seq, advance lastFlowSent, and
//     return tracked=true with the new lag (prior+1).
//
// maxUnacked must lie in MinUnackedWindow..MaxUnackedWindow; an
// out-of-range direct call is an internal configuration error
// (ErrInvariant), never a silently disabled window.
func (r *Registry) NextFlowControlledServerSeq(id ID, maxUnacked int) (seq uint32, tracked bool, lag int, err error) {
	if maxUnacked < MinUnackedWindow || maxUnacked > MaxUnackedWindow {
		return 0, false, 0, fmt.Errorf(
			"session: max_unacked %d out of %d..%d: %w",
			maxUnacked, MinUnackedWindow, MaxUnackedWindow, ErrInvariant)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byID[id]
	if !ok {
		return 0, false, 0, fmt.Errorf("session: flow seq for unknown session: %w", ErrNotFound)
	}
	if e.state != StateInWorld {
		return e.serverSeq.Add(1), false, 0, nil
	}
	if !e.flowActive {
		return 0, false, 0, fmt.Errorf(
			"session: flow seq in IN_WORLD without flow epoch: %w", ErrInvariant)
	}
	current := int(uint32(e.lastFlowSent - e.lastAck)) // pure modulo distance
	if current >= maxUnacked {
		return 0, false, 0, fmt.Errorf(
			"session: ack lag %d >= max %d: %w", current, maxUnacked, ErrAckLag)
	}
	seq = e.serverSeq.Add(1)
	e.lastFlowSent = seq
	return seq, true, current + 1, nil
}

// ApplyAck applies one cumulative opcode-125 ACK to the session's flow
// epoch (spec §7.1.11). It requires the session to exist, be IN_WORLD,
// and hold an active epoch; ordering uses ONLY the M2 serial helpers.
// Classifications: ack == lastAck → AckDuplicate (no-op); ack serially
// before lastAck → AckStale (no-op); lastAck < ack <= lastFlowSent →
// AckAdvanced (lastAck = ack); ack serially after lastFlowSent, or at
// an exact 2^31 (half-range) distance in either direction →
// ErrFutureAck with zero mutation. The returned lag is the current
// unacked window AFTER the classification. No payload is retained; no
// replay exists.
func (r *Registry) ApplyAck(id ID, ackSeq uint32) (AckDisposition, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byID[id]
	if !ok {
		return 0, 0, fmt.Errorf("session: ack for unknown session: %w", ErrNotFound)
	}
	if e.state != StateInWorld {
		return 0, 0, fmt.Errorf("session: ack in %s: %w", e.state, ErrBadState)
	}
	if !e.flowActive {
		return 0, 0, fmt.Errorf("session: ack without flow epoch: %w", ErrInvariant)
	}
	current := int(uint32(e.lastFlowSent - e.lastAck))
	switch {
	case ackSeq == e.lastAck:
		return AckDuplicate, current, nil
	case proto.Serial32Before(ackSeq, e.lastAck):
		return AckStale, current, nil
	case proto.Serial32After(ackSeq, e.lastFlowSent):
		return 0, 0, fmt.Errorf(
			"session: ack %d ahead of sent flow %d: %w", ackSeq, e.lastFlowSent, ErrFutureAck)
	case proto.Serial32After(ackSeq, e.lastAck):
		e.lastAck = ackSeq
		return AckAdvanced, int(uint32(e.lastFlowSent - e.lastAck)), nil
	default:
		// Exact half-range distance: both serial directions are false —
		// impossible during valid operation (the window is far below
		// 2^31); treat as future/invalid, never as stale.
		return 0, 0, fmt.Errorf(
			"session: ack %d at half-range (lastAck %d, sent %d): %w",
			ackSeq, e.lastAck, e.lastFlowSent, ErrFutureAck)
	}
}

// FlowState returns an immutable copy of the session's ACK-flow epoch
// (spec §7.1.11). It exists for tests and diagnostics; no mutable
// entry state escapes.
func (r *Registry) FlowState(id ID) (FlowSnapshot, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.byID[id]
	if !ok {
		return FlowSnapshot{}, fmt.Errorf("session: flow state for unknown session: %w", ErrNotFound)
	}
	return FlowSnapshot{Active: e.flowActive, LastAck: e.lastAck, LastFlowSent: e.lastFlowSent}, nil
}

// LockAccount returns the unlock function for the per-account lifecycle
// guard keyed by accountID (never by connection or character). Guards
// for the same account serialize; different accounts proceed
// concurrently. The keyed mutex is ref-counted and deleted when its
// last holder/waiter releases it, so the map does not grow without
// bound. T1 performs no takeover behavior; later tasks serialize
// enter_world, leave_world, forced takeover, and character deletion
// through this guard.
func (r *Registry) LockAccount(accountID int64) (unlock func()) {
	r.guardMu.Lock()
	g, ok := r.guards[accountID]
	if !ok {
		g = &accountGuard{}
		r.guards[accountID] = g
	}
	g.refs++
	r.guardMu.Unlock()

	g.mu.Lock()
	return func() {
		g.mu.Unlock()
		r.guardMu.Lock()
		g.refs--
		if g.refs == 0 {
			delete(r.guards, accountID)
		}
		r.guardMu.Unlock()
	}
}

// GuardCount reports the number of live keyed guards. It exists for
// tests to prove guards are released, not for production use.
func (r *Registry) GuardCount() int {
	r.guardMu.Lock()
	defer r.guardMu.Unlock()
	return len(r.guards)
}

func snapshotOf(e *entry) Snapshot {
	return Snapshot{
		ID:            e.id,
		Sub:           e.sub,
		AccountID:     e.accountID,
		Authenticated: e.authenticated,
		CharacterID:   e.characterID,
		HasCharacter:  e.hasCharacter,
		Conn:          e.conn,
		State:         e.state,
		TokenExp:      e.tokenExp,
	}
}
