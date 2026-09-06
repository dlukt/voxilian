package gateway

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/dlukt/voxilian/internal/session"
	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/world"
)

// Stable presence/AOI/handle errors (spec §7.2). All are matched with
// errors.Is; callers MUST NOT parse strings.
var (
	// ErrPresenceInvalidIdentity reports malformed activation or lookup
	// identity: zero session ID, non-positive character ID, zero sim
	// entity ID, or a zero NetEntityID where a handle is required.
	ErrPresenceInvalidIdentity = errors.New("gateway: invalid presence identity")
	// ErrPresenceNotFound reports an operation against a session with
	// no active presence epoch.
	ErrPresenceNotFound = errors.New("gateway: presence not found")
	// ErrSessionAlreadyPresent reports activation for a session ID that
	// already owns an active presence.
	ErrSessionAlreadyPresent = errors.New("gateway: session already present")
	// ErrCharacterAlreadyPresent reports activation for a character ID
	// that already owns an active presence.
	ErrCharacterAlreadyPresent = errors.New("gateway: character already present")
	// ErrControlledEntityAlreadyPresent reports activation for a sim
	// entity ID that is already a controlled entity of an active
	// presence.
	ErrControlledEntityAlreadyPresent = errors.New("gateway: controlled entity already present")
	// ErrInvalidNetEntityID reports a zero/reserved NetEntityID where a
	// live handle is required.
	ErrInvalidNetEntityID = errors.New("gateway: invalid NetEntityID")
	// ErrNetEntityIDExhausted reports that a presence epoch allocated
	// every handle up to MaxUint32; handles never wrap or reuse.
	ErrNetEntityIDExhausted = errors.New("gateway: NetEntityID exhausted")
	// ErrOwnEntityVisibility reports an attempt to hide the session's
	// own controlled entity while its presence stays active.
	ErrOwnEntityVisibility = errors.New("gateway: cannot hide own entity")
	// ErrAOICellRange reports a center cell whose 3-cell neighborhood
	// cannot be represented in int32 coordinates (no wrapping).
	ErrAOICellRange = errors.New("gateway: AOI cell range overflow")
)

// Frozen presence/AOI/heartbeat constants (spec §§4, 7.2).
const (
	// AOICellRadius is the Chebyshev cell radius of the base AOI.
	AOICellRadius = 3
	// AOICellCount is the exact base-AOI cell count: 7x7 = 49.
	AOICellCount = 49
	// PresenceHeartbeatTimeout is the dead-presence sweep threshold:
	// now >= HeartbeatAt + timeout is stale (boundary inclusive).
	PresenceHeartbeatTimeout = 30 * time.Second
)

// NetEntityID is the gateway-owned session-local wire-entity handle
// (spec §7.2.8). Zero is invalid/reserved. Handles are never
// persisted, never equal-by-definition to sim.EntityID, and never
// reused within one presence epoch.
type NetEntityID uint32

// InvalidNetEntityID is the reserved zero handle; it never names a
// visible entity.
const InvalidNetEntityID NetEntityID = 0

// SubscriptionDelta is the entered/exited cell diff of one center
// update. Both slices are canonical (X ascending, then Z ascending)
// copies; empty (non-nil-observable) when the center did not change.
type SubscriptionDelta struct {
	Entered []world.CellCoord
	Exited  []world.CellCoord
}

// PresenceSnapshot is the immutable inspection copy of one active
// presence epoch (spec §7.2.3). Cells is a copied canonical slice;
// mutating it cannot mutate registry state.
type PresenceSnapshot struct {
	SessionID   session.ID
	CharacterID int64
	EntityID    sim.EntityID
	CenterCell  world.CellCoord
	Cells       []world.CellCoord
	HeartbeatAt time.Time
	OwnNetID    NetEntityID
}

// BaseAOICells returns the exact 49-cell Chebyshev 3-cell neighborhood
// around center in canonical order (spec §7.2.6). Centers within 3 of
// the int32 limits report ErrAOICellRange instead of wrapping.
func BaseAOICells(center world.CellCoord) ([]world.CellCoord, error) {
	cells := make([]world.CellCoord, 0, AOICellCount)
	for dx := -int64(AOICellRadius); dx <= int64(AOICellRadius); dx++ {
		for dz := -int64(AOICellRadius); dz <= int64(AOICellRadius); dz++ {
			x := int64(center.X) + dx
			z := int64(center.Z) + dz
			if x < math.MinInt32 || x > math.MaxInt32 ||
				z < math.MinInt32 || z > math.MaxInt32 {
				return nil, ErrAOICellRange
			}
			cells = append(cells, world.CellCoord{X: int32(x), Z: int32(z)})
		}
	}
	world.SortCellCoords(cells)
	return cells, nil
}

// presence is the mutable record behind one active world-presence
// epoch. Only the owning PresenceRegistry mutates it, always under
// the registry lock.
type presence struct {
	sessionID   session.ID
	characterID int64
	entityID    sim.EntityID
	center      world.CellCoord
	cells       []world.CellCoord
	heartbeatAt time.Time

	// forward maps visible sim entities to their session-local handle;
	// reverse maps live handles back. Retired handles appear in
	// neither. The own controlled entity is pinned in both for the
	// whole epoch.
	forward map[sim.EntityID]NetEntityID
	reverse map[NetEntityID]sim.EntityID
	// next is the next handle to allocate; 0 is the exhaustion
	// sentinel (never allocated, never wrapped to).
	next NetEntityID

	move   tokenBucket
	intent tokenBucket
}

// snapshot copies the presence's observable state. Callers hold at
// least a read lock; the result shares nothing mutable.
func (p *presence) snapshot() PresenceSnapshot {
	cells := make([]world.CellCoord, len(p.cells))
	copy(cells, p.cells)
	return PresenceSnapshot{
		SessionID:   p.sessionID,
		CharacterID: p.characterID,
		EntityID:    p.entityID,
		CenterCell:  p.center,
		Cells:       cells,
		HeartbeatAt: p.heartbeatAt,
		OwnNetID:    p.forward[p.entityID],
	}
}

// PresenceRegistry is the gateway-owned ephemeral world-session core
// (spec §7.2): active presences with one-to-one session/character/
// controlled-entity indexes, canonical AOI subscriptions with a
// reverse cell->session index, session-local NetEntityID visibility
// tables, per-presence token buckets, and heartbeat state. It is safe
// for concurrent use; one short mutex guards metadata and is never
// held over socket writes, sim calls, PG calls, or sleeps (this core
// performs none of those anyway). Use NewPresenceRegistry to
// construct.
type PresenceRegistry struct {
	mu       sync.RWMutex
	policy   RateLimitPolicy
	bySess   map[session.ID]*presence
	byChar   map[int64]*presence
	byEntity map[sim.EntityID]*presence
	// subs maps each subscribed cell to its live subscriber sessions.
	subs map[world.CellCoord]map[session.ID]struct{}
	// viewers maps each currently-visible entity to its live viewer
	// sessions (spec §7.4.1): the reverse of every presence's
	// forward visibility table, updated atomically under this same
	// lock. Empty sets are deleted; Deactivate leaves no ghost.
	viewers map[sim.EntityID]map[session.ID]struct{}
}

// addViewer records sid as a viewer of entity, creating the set.
func (r *PresenceRegistry) addViewer(entity sim.EntityID, sid session.ID) {
	set := r.viewers[entity]
	if set == nil {
		set = make(map[session.ID]struct{})
		r.viewers[entity] = set
	}
	set[sid] = struct{}{}
}

// removeViewer drops sid from entity's viewer set, deleting empties.
func (r *PresenceRegistry) removeViewer(entity sim.EntityID, sid session.ID) {
	if set := r.viewers[entity]; set != nil {
		delete(set, sid)
		if len(set) == 0 {
			delete(r.viewers, entity)
		}
	}
}

// NewPresenceRegistry returns an empty presence registry enforcing
// the given inbound rate policy for every presence it activates. An
// invalid policy is rejected before anything is constructed.
func NewPresenceRegistry(policy RateLimitPolicy) (*PresenceRegistry, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	return &PresenceRegistry{
		policy:   policy,
		bySess:   make(map[session.ID]*presence),
		byChar:   make(map[int64]*presence),
		byEntity: make(map[sim.EntityID]*presence),
		subs:     make(map[world.CellCoord]map[session.ID]struct{}),
		viewers:  make(map[sim.EntityID]map[session.ID]struct{}),
	}, nil
}

// Len returns the number of active presence epochs.
func (r *PresenceRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.bySess)
}

// Activate starts one active world-presence epoch: a pure ephemeral
// registry primitive (spec §7.2.4). It changes no session.Registry
// state, adds no sim entity, and sends nothing. The controlled entity
// is immediately bound to NetEntityID(1); both token buckets start
// full anchored at heartbeatAt. Conflicts and malformed identity
// leave the registry completely unchanged.
func (r *PresenceRegistry) Activate(sid session.ID, characterID int64, entity sim.EntityID, center world.CellCoord, heartbeatAt time.Time) (PresenceSnapshot, error) {
	if sid == 0 || characterID <= 0 || entity == sim.InvalidEntityID {
		return PresenceSnapshot{}, ErrPresenceInvalidIdentity
	}
	cells, err := BaseAOICells(center)
	if err != nil {
		return PresenceSnapshot{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, taken := r.bySess[sid]; taken {
		return PresenceSnapshot{}, ErrSessionAlreadyPresent
	}
	if _, taken := r.byChar[characterID]; taken {
		return PresenceSnapshot{}, ErrCharacterAlreadyPresent
	}
	if _, taken := r.byEntity[entity]; taken {
		return PresenceSnapshot{}, fmt.Errorf("gateway: entity %d controlled: %w", uint64(entity), ErrControlledEntityAlreadyPresent)
	}
	p := &presence{
		sessionID:   sid,
		characterID: characterID,
		entityID:    entity,
		center:      center,
		cells:       cells,
		heartbeatAt: heartbeatAt,
		forward:     map[sim.EntityID]NetEntityID{entity: NetEntityID(1)},
		reverse:     map[NetEntityID]sim.EntityID{NetEntityID(1): entity},
		next:        NetEntityID(2),
		move:        newTokenBucket(r.policy.MovePerSec, heartbeatAt),
		intent:      newTokenBucket(r.policy.IntentPerSec, heartbeatAt),
	}
	r.bySess[sid] = p
	r.byChar[characterID] = p
	r.byEntity[entity] = p
	r.addViewer(entity, sid)
	for _, c := range cells {
		set := r.subs[c]
		if set == nil {
			set = make(map[session.ID]struct{})
			r.subs[c] = set
		}
		set[sid] = struct{}{}
	}
	return p.snapshot(), nil
}

// Deactivate ends the presence epoch for sid, atomically removing its
// session/character/entity indexes, all AOI subscriptions, all
// visibility mappings, heartbeat state, and rate-limit state. It
// returns the prior epoch snapshot. Unknown sessions report
// ErrPresenceNotFound.
func (r *PresenceRegistry) Deactivate(sid session.ID) (PresenceSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.bySess[sid]
	if !ok {
		return PresenceSnapshot{}, ErrPresenceNotFound
	}
	prior := p.snapshot()
	delete(r.bySess, sid)
	delete(r.byChar, p.characterID)
	delete(r.byEntity, p.entityID)
	for entity := range p.forward {
		r.removeViewer(entity, sid)
	}
	for _, c := range p.cells {
		if set := r.subs[c]; set != nil {
			delete(set, sid)
			if len(set) == 0 {
				delete(r.subs, c)
			}
		}
	}
	return prior, nil
}

// UpdateCenter moves sid's subscription to the 49-cell base AOI around
// center and returns the entered/exited diff (both canonical). A
// no-op center keeps the reverse index untouched. Unknown sessions
// report ErrPresenceNotFound; unrepresentable neighborhoods report
// ErrAOICellRange with zero mutation.
func (r *PresenceRegistry) UpdateCenter(sid session.ID, center world.CellCoord) (SubscriptionDelta, error) {
	cells, err := BaseAOICells(center)
	if err != nil {
		return SubscriptionDelta{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.bySess[sid]
	if !ok {
		return SubscriptionDelta{}, ErrPresenceNotFound
	}
	if center == p.center {
		return SubscriptionDelta{}, nil
	}
	keep := make(map[world.CellCoord]struct{}, len(cells))
	for _, c := range cells {
		keep[c] = struct{}{}
	}
	var delta SubscriptionDelta
	for _, c := range p.cells {
		if _, ok := keep[c]; !ok {
			delta.Exited = append(delta.Exited, c)
		}
	}
	old := make(map[world.CellCoord]struct{}, len(p.cells))
	for _, c := range p.cells {
		old[c] = struct{}{}
	}
	for _, c := range cells {
		if _, ok := old[c]; !ok {
			delta.Entered = append(delta.Entered, c)
		}
	}
	for _, c := range delta.Exited {
		if set := r.subs[c]; set != nil {
			delete(set, sid)
			if len(set) == 0 {
				delete(r.subs, c)
			}
		}
	}
	for _, c := range delta.Entered {
		set := r.subs[c]
		if set == nil {
			set = make(map[session.ID]struct{})
			r.subs[c] = set
		}
		set[sid] = struct{}{}
	}
	p.center = center
	p.cells = cells
	return delta, nil
}

// Subscribers returns the unique live presence session IDs subscribed
// to cell, sorted numerically ascending, as a copy.
func (r *PresenceRegistry) Subscribers(cell world.CellCoord) []session.ID {
	r.mu.RLock()
	defer r.mu.RUnlock()
	set := r.subs[cell]
	ids := make([]session.ID, 0, len(set))
	for sid := range set {
		ids = append(ids, sid)
	}
	slices.Sort(ids)
	return ids
}

// Snapshot returns the immutable copy of sid's active presence epoch.
// Unknown sessions report ErrPresenceNotFound.
func (r *PresenceRegistry) Snapshot(sid session.ID) (PresenceSnapshot, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.bySess[sid]
	if !ok {
		return PresenceSnapshot{}, ErrPresenceNotFound
	}
	return p.snapshot(), nil
}

// EnsureVisible binds entity into sid's visibility table, allocating a
// fresh monotonic handle for newly visible entities (spec §7.2.8). An
// already-visible entity returns its existing handle with
// created=false. The own controlled entity is always already mapped.
func (r *PresenceRegistry) EnsureVisible(sid session.ID, entity sim.EntityID) (NetEntityID, bool, error) {
	if entity == sim.InvalidEntityID {
		return InvalidNetEntityID, false, ErrPresenceInvalidIdentity
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.bySess[sid]
	if !ok {
		return InvalidNetEntityID, false, ErrPresenceNotFound
	}
	if h, ok := p.forward[entity]; ok {
		return h, false, nil
	}
	if p.next == InvalidNetEntityID {
		return InvalidNetEntityID, false, ErrNetEntityIDExhausted
	}
	h := p.next
	if h == NetEntityID(math.MaxUint32) {
		p.next = InvalidNetEntityID
	} else {
		p.next++
	}
	p.forward[entity] = h
	p.reverse[h] = entity
	r.addViewer(entity, sid)
	return h, true, nil
}

// HideVisible retires entity from sid's visibility table, returning
// the retired handle. Absent entities are a no-op (zero handle,
// visible=false, nil error). The own controlled entity cannot be
// hidden while its presence is active.
func (r *PresenceRegistry) HideVisible(sid session.ID, entity sim.EntityID) (NetEntityID, bool, error) {
	if entity == sim.InvalidEntityID {
		return InvalidNetEntityID, false, ErrPresenceInvalidIdentity
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.bySess[sid]
	if !ok {
		return InvalidNetEntityID, false, ErrPresenceNotFound
	}
	if entity == p.entityID {
		return InvalidNetEntityID, false, ErrOwnEntityVisibility
	}
	h, ok := p.forward[entity]
	if !ok {
		return InvalidNetEntityID, false, nil
	}
	delete(p.forward, entity)
	delete(p.reverse, h)
	r.removeViewer(entity, sid)
	return h, true, nil
}

// ResolveHandle maps a currently-visible session-local handle back to
// its sim entity (spec §7.2.8). Zero, retired, unknown-presence, and
// other-session handles all report visible=false: the future
// target-handle gate denies them without aliasing.
func (r *PresenceRegistry) ResolveHandle(sid session.ID, net NetEntityID) (sim.EntityID, bool) {
	if net == InvalidNetEntityID {
		return sim.InvalidEntityID, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.bySess[sid]
	if !ok {
		return sim.InvalidEntityID, false
	}
	entity, ok := p.reverse[net]
	if !ok {
		return sim.InvalidEntityID, false
	}
	return entity, true
}

// Viewers returns the live sessions currently seeing entity
// (spec §7.4.1), sorted numerically ascending, as a copy. Unknown
// entities yield an empty result.
func (r *PresenceRegistry) Viewers(entity sim.EntityID) []session.ID {
	r.mu.RLock()
	defer r.mu.RUnlock()
	set := r.viewers[entity]
	ids := make([]session.ID, 0, len(set))
	for sid := range set {
		ids = append(ids, sid)
	}
	slices.Sort(ids)
	return ids
}

// VisibleEntities returns every sim entity sid currently sees
// (including its own controlled entity), sorted numerically
// ascending, as a copy. Unknown sessions report
// ErrPresenceNotFound.
func (r *PresenceRegistry) VisibleEntities(sid session.ID) ([]sim.EntityID, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.bySess[sid]
	if !ok {
		return nil, ErrPresenceNotFound
	}
	ids := make([]sim.EntityID, 0, len(p.forward))
	for entity := range p.forward {
		ids = append(ids, entity)
	}
	slices.Sort(ids)
	return ids, nil
}

// VisibleHandle returns sid's current session-local handle for
// entity. Unknown sessions report ErrPresenceNotFound; entities
// outside sid's visibility report visible=false.
func (r *PresenceRegistry) VisibleHandle(sid session.ID, entity sim.EntityID) (NetEntityID, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.bySess[sid]
	if !ok {
		return InvalidNetEntityID, false, ErrPresenceNotFound
	}
	h, ok := p.forward[entity]
	if !ok {
		return InvalidNetEntityID, false, nil
	}
	return h, true, nil
}

// Controller returns the session currently controlling entity (the
// reverse controlled-entity index), or false for uncontrolled/unknown
// entities. No scan over sessions is performed.
func (r *PresenceRegistry) Controller(entity sim.EntityID) (session.ID, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.byEntity[entity]
	if !ok {
		return 0, false
	}
	return p.sessionID, true
}

// TouchHeartbeat advances sid's heartbeat to now when now is strictly
// newer; equal/older timestamps are no-ops so a regressed clock never
// moves liveness backwards. Unknown sessions report
// ErrPresenceNotFound.
func (r *PresenceRegistry) TouchHeartbeat(sid session.ID, now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.bySess[sid]
	if !ok {
		return ErrPresenceNotFound
	}
	if now.After(p.heartbeatAt) {
		p.heartbeatAt = now
	}
	return nil
}

// StaleSessions returns the session IDs whose heartbeat is at least
// PresenceHeartbeatTimeout old (now >= HeartbeatAt + timeout,
// boundary inclusive), sorted ascending, as a copy. A heartbeat in
// the future is never stale. The query deletes nothing.
func (r *PresenceRegistry) StaleSessions(now time.Time) []session.ID {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var ids []session.ID
	for sid, p := range r.bySess {
		if !now.Before(p.heartbeatAt.Add(PresenceHeartbeatTimeout)) {
			ids = append(ids, sid)
		}
	}
	slices.Sort(ids)
	return ids
}

// AllowMove consumes one movement-bucket token for sid's active
// presence epoch at now, reporting whether the request is admitted
// (spec §7.2.9). Unknown sessions report ErrPresenceNotFound and
// create nothing.
func (r *PresenceRegistry) AllowMove(sid session.ID, now time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.bySess[sid]
	if !ok {
		return false, ErrPresenceNotFound
	}
	return p.move.allow(now), nil
}

// AllowIntent consumes one general-intent-bucket token for sid's
// active presence epoch at now. It is independent of AllowMove.
func (r *PresenceRegistry) AllowIntent(sid session.ID, now time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.bySess[sid]
	if !ok {
		return false, ErrPresenceNotFound
	}
	return p.intent.allow(now), nil
}
