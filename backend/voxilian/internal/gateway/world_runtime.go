package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/dlukt/voxilian/internal/session"
	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/world"
)

// Stable world-entry errors (spec §7.3.4). The EnterWorldHandler maps
// ErrWorldEntryRetry to 202 retry; every other error fails closed as
// internal. Match with errors.Is, never string parsing.
var (
	// ErrWorldEntryRetry marks an operational world-entry failure
	// (spawn resolution, engine unavailable, owner-mailbox
	// saturation): the client may retry enter_world.
	ErrWorldEntryRetry = errors.New("gateway: world entry unavailable")
	// ErrWorldEntryPending marks a second PrepareEnter for a session
	// that already stages an entry: an internal lifecycle invariant.
	ErrWorldEntryPending = errors.New("gateway: world entry already staged")
	// ErrWorldEntryNoPending marks CommitEnter with no staged entry
	// for the session: an internal lifecycle invariant.
	ErrWorldEntryNoPending = errors.New("gateway: no staged world entry")
	// ErrWorldSpawnInvalid marks a trusted spawn position the sim
	// rejects: an internal trusted-data/world invariant, never
	// silently rewritten.
	ErrWorldSpawnInvalid = errors.New("gateway: invalid trusted spawn")
)

// SpawnResolver decides ONLY the authoritative initial world.Vec3
// for a world-presence epoch (spec §7.3.3). It never adds the
// entity, binds presence, sends a baseline, or changes session
// state. Tests use deterministic fakes; future real world/durable
// composition supplies it.
type SpawnResolver interface {
	ResolveSpawn(ctx context.Context, accountID int64, characterID int64) (world.Vec3, error)
}

// SpawnResolverFunc adapts a plain function to a SpawnResolver.
type SpawnResolverFunc func(ctx context.Context, accountID int64, characterID int64) (world.Vec3, error)

// ResolveSpawn implements SpawnResolver.
func (f SpawnResolverFunc) ResolveSpawn(ctx context.Context, accountID int64, characterID int64) (world.Vec3, error) {
	return f(ctx, accountID, characterID)
}

// WorldEnter is the staged world-entry seam EnterWorldHandler needs
// (spec §7.3.4). PrepareEnter stages the sim entity without
// activating Presence; CommitEnter activates Presence after the
// physical 219 + CompleteEnterWorld barrier; AbortEnter removes a
// staged entity. The same concrete WorldSessionRuntime also
// implements WorldExit, so leave and takeover share one composition.
type WorldEnter interface {
	PrepareEnter(ctx context.Context, sid session.ID, accountID int64, characterID int64) error
	CommitEnter(sid session.ID) error
	AbortEnter(ctx context.Context, sid session.ID) error
}

// pendingEntry is the bounded ephemeral staged-entry metadata: at
// most one per session, never persisted, never a history.
type pendingEntry struct {
	accountID   int64
	characterID int64
	entity      sim.EntityID
	cell        world.CellCoord
	staged      bool
}

// WorldSessionRuntime composes SimIngress, PresenceRegistry,
// SpawnResolver, NowFunc, and the downstream WorldExit
// quiesce/flush seam into one staged world-presence lifecycle
// (spec §7.3.4–§7.3.6). It is safe for concurrent use; the small
// pending-entry mutex is never held across SpawnResolver calls,
// sim Enqueue*, downstream WorldExit calls, socket writes, or
// PresenceRegistry calls.
type WorldSessionRuntime struct {
	sim        SimIngress
	presence   *PresenceRegistry
	spawn      SpawnResolver
	now        NowFunc
	downstream WorldExit

	mu      sync.Mutex
	pending map[session.ID]*pendingEntry
}

// NewWorldSessionRuntime wires a WorldSessionRuntime. Every
// dependency is required: a missing sim, presence, spawn, clock, or
// downstream flush seam must never silently succeed.
func NewWorldSessionRuntime(simIngress SimIngress, presence *PresenceRegistry, spawn SpawnResolver, now NowFunc, downstream WorldExit) (*WorldSessionRuntime, error) {
	if simIngress == nil {
		return nil, errors.New("gateway: sim ingress is required")
	}
	if presence == nil {
		return nil, errors.New("gateway: presence registry is required")
	}
	if spawn == nil {
		return nil, errors.New("gateway: spawn resolver is required")
	}
	if now == nil {
		return nil, errors.New("gateway: clock is required")
	}
	if downstream == nil {
		return nil, errors.New("gateway: downstream world exit is required")
	}
	return &WorldSessionRuntime{
		sim:        simIngress,
		presence:   presence,
		spawn:      spawn,
		now:        now,
		downstream: downstream,
		pending:    make(map[session.ID]*pendingEntry),
	}, nil
}

// PrepareEnter reserves the pending slot, resolves the spawn, adds
// one sim entity through the owner, and retains the returned
// EntityID + authoritative cell. Presence does NOT exist yet and no
// wire message is sent. A second prepare while pending is a stable
// conflict with no second entity.
func (r *WorldSessionRuntime) PrepareEnter(ctx context.Context, sid session.ID, accountID int64, characterID int64) error {
	r.mu.Lock()
	if _, dup := r.pending[sid]; dup {
		r.mu.Unlock()
		return ErrWorldEntryPending
	}
	r.pending[sid] = &pendingEntry{accountID: accountID, characterID: characterID}
	r.mu.Unlock()

	pos, err := r.spawn.ResolveSpawn(ctx, accountID, characterID)
	if err != nil {
		r.dropPending(sid)
		return fmt.Errorf("%w: resolve spawn: %w", ErrWorldEntryRetry, err)
	}
	snap, err := r.sim.EnqueueAddEntity(ctx, pos)
	if err != nil {
		r.dropPending(sid)
		switch {
		case errors.Is(err, sim.ErrSimIngressFull),
			errors.Is(err, sim.ErrEngineNotRunning),
			errors.Is(err, sim.ErrEngineStopped):
			return fmt.Errorf("%w: stage entity: %w", ErrWorldEntryRetry, err)
		case errors.Is(err, sim.ErrInvalidPosition):
			return fmt.Errorf("%w: %v: %w", ErrWorldSpawnInvalid, pos, err)
		default:
			return fmt.Errorf("gateway: stage entity: %w", err)
		}
	}
	r.mu.Lock()
	p, ok := r.pending[sid]
	if !ok || p.staged {
		r.mu.Unlock()
		// Defensive: the reservation vanished under us (no path
		// under the account guard does this). Remove the orphan
		// rather than leak a sim entity.
		_ = r.sim.EnqueueRemoveEntity(ctx, snap.ID)
		return fmt.Errorf("gateway: staged entry vanished for session %d", uint64(sid))
	}
	p.entity = snap.ID
	p.cell = snap.Cell
	p.staged = true
	r.mu.Unlock()
	return nil
}

// AbortEnter removes a prepared staged entry: EnqueueRemoveEntity,
// then pending metadata. No Presence existed, so none is
// deactivated. With no prepared entry it returns nil (frozen
// idempotent rollback choice). A pre-mutation removal failure
// retains pending state for retry/cleanup.
func (r *WorldSessionRuntime) AbortEnter(ctx context.Context, sid session.ID) error {
	r.mu.Lock()
	p, ok := r.pending[sid]
	r.mu.Unlock()
	if !ok {
		return nil
	}
	if !p.staged {
		r.dropPending(sid)
		return nil
	}
	if err := r.sim.EnqueueRemoveEntity(ctx, p.entity); err != nil {
		return fmt.Errorf("gateway: abort staged entity: %w", err)
	}
	r.dropPending(sid)
	return nil
}

// CommitEnter activates Presence from the staged entry AFTER the
// physical 219 write and CompleteEnterWorld (spec §7.3.4). Buckets
// start at commit time via Now(). On activation failure the staged
// entry is retained so AbortEnter still removes the sim entity; on
// success pending metadata is dropped. No wire message is emitted.
func (r *WorldSessionRuntime) CommitEnter(sid session.ID) error {
	r.mu.Lock()
	p, ok := r.pending[sid]
	r.mu.Unlock()
	if !ok || !p.staged {
		return ErrWorldEntryNoPending
	}
	if _, err := r.presence.Activate(sid, p.characterID, p.entity, p.cell, r.now()); err != nil {
		return fmt.Errorf("gateway: commit presence: %w", err)
	}
	r.dropPending(sid)
	return nil
}

// ExitWorld implements WorldExit for a healthy active presence
// (spec §7.3.6): verify the sid/character match, run the existing
// downstream quiesce/flush seam FIRST, then remove the sim entity,
// then deactivate Presence. Any failure stops with prior state
// intact so the caller can retry safely.
func (r *WorldSessionRuntime) ExitWorld(ctx context.Context, sid session.ID, accountID int64, characterID int64) error {
	snap, err := r.presence.Snapshot(sid)
	if err != nil {
		return fmt.Errorf("gateway: exit without active presence %d: %w", uint64(sid), err)
	}
	if snap.CharacterID != characterID {
		return fmt.Errorf("gateway: exit character mismatch session %d", uint64(sid))
	}
	if err := r.downstream.ExitWorld(ctx, sid, accountID, characterID); err != nil {
		return err
	}
	if err := r.sim.EnqueueRemoveEntity(ctx, snap.EntityID); err != nil {
		return fmt.Errorf("gateway: exit remove entity: %w", err)
	}
	if _, err := r.presence.Deactivate(sid); err != nil {
		return fmt.Errorf("gateway: exit deactivate presence: %w", err)
	}
	return nil
}

// dropPending removes the pending reservation unconditionally.
func (r *WorldSessionRuntime) dropPending(sid session.ID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.pending, sid)
}
