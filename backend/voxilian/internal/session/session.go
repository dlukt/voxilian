package session

import (
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

// Connection is the narrow session-level handle the registry retains so
// later tasks can force-takeover/kick a session. The gateway adapts its
// WebSocket connection to this interface; this package never imports a
// WebSocket implementation.
type Connection interface {
	Close(reason string) error
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
)

// entry is the mutable registry record behind one Snapshot.
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
// removes the index, clears the binding, and moves the session to
// AUTHENTICATED. Identity (sub, accountID, tokenExp, connection,
// server seq) is unchanged. No public snapshot can show an
// intermediate AUTHENTICATED-yet-bound or IN_WORLD-yet-unbound state.
// Any precondition failure returns ErrBadState/ErrNotFound and leaves
// the registry untouched.
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
	return nil
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
func (r *Registry) NextServerSeq(id ID) (uint32, error) {
	r.mu.RLock()
	e, ok := r.byID[id]
	r.mu.RUnlock()
	if !ok {
		return 0, fmt.Errorf("session: seq for unknown session: %w", ErrNotFound)
	}
	return e.serverSeq.Add(1), nil
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
