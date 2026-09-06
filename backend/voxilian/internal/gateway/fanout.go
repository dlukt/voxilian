package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dlukt/voxilian/internal/proto"
	"github.com/dlukt/voxilian/internal/session"
	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/world"
)

// Frozen fanout constants (spec §7.4).
const (
	// FanoutEventCapacity is the exact bounded movement/control event
	// queue depth: 1024. No unbounded slice, no per-event goroutine.
	FanoutEventCapacity = 1024
	// FanoutControlTimeout bounds control-event (bootstrap/remove)
	// admission: 1 s internal policy, not user config.
	FanoutControlTimeout = 1 * time.Second
	// MovementFanoutMaxHz caps 205 emission per recipient/entity
	// visibility epoch: 10 Hz via tick stride, never wall clock.
	MovementFanoutMaxHz = 10
)

// Stable fanout errors. Match with errors.Is.
var (
	// ErrFanoutClosed marks control admission or completion against a
	// shut-down fanout runtime.
	ErrFanoutClosed = errors.New("gateway: fanout closed")
	// ErrFanoutControlTimeout marks a control event that could not be
	// admitted within FanoutControlTimeout.
	ErrFanoutControlTimeout = errors.New("gateway: fanout control admission timeout")
	// ErrFanoutNoProducer marks a recipient with no queue-capable
	// outbound: missing session, nil conn, or non-OutboundProducer
	// transport. Callers fail that recipient closed.
	ErrFanoutNoProducer = errors.New("gateway: fanout recipient has no outbound producer")
	// ErrPresentationMissing marks a required entity presentation the
	// source cannot supply: an internal world/presentation invariant
	// (never a silent fallback kind/proto).
	ErrPresentationMissing = errors.New("gateway: entity presentation missing")
)

// EntityPresentation is the hot in-memory presentation of one live
// entity for 204 fanout (spec §7.4.2). Kind/Proto come from the real
// content/world layer (fakes in tests); only Position/Yaw/Speed may
// be overridden by the MovementUpdate that caused visibility.
type EntityPresentation struct {
	EntityID sim.EntityID
	Tick     uint32

	Kind  uint8
	Proto uint16

	Position world.Vec3
	Yaw      uint16
	Speed    uint8
}

// EntityPresentationSource is the narrow injected HOT IN-MEMORY seam
// for fanout (spec §7.4.2): race-safe, non-blocking, non-PG,
// non-network, authoritative/current. EntitiesInCell returns unique
// EntityIDs sorted ascending whose positions actually map to that
// cell, as copies. M9/M10 later supply the real layer.
type EntityPresentationSource interface {
	Entity(entity sim.EntityID) (EntityPresentation, bool)
	EntitiesInCell(cell world.CellCoord) []EntityPresentation
}

// FanoutLifecycle is the narrow lifecycle seam the world runtime
// composes (spec §7.4.6): post-219 bootstrap and post-sim-remove
// fanout removal. *FanoutRuntime satisfies it; tests may substitute
// a no-op/recording fake where fanout is not under test.
type FanoutLifecycle interface {
	BootstrapSession(ctx context.Context, sid session.ID) error
	RemovePresence(ctx context.Context, sid session.ID, entity sim.EntityID) error
}

type fanoutEventKind uint8

const (
	fanoutMovement fanoutEventKind = iota
	fanoutBootstrap
	fanoutRemove
)

// fanoutEvent is the private typed pump-union (spec §7.4.3):
// movement updates plus bootstrap/remove controls. No arbitrary
// callbacks cross this boundary.
type fanoutEvent struct {
	kind   fanoutEventKind
	update sim.MovementUpdate
	sid    session.ID
	entity sim.EntityID
	// res is the cap-1 control completion signal; nil for movement.
	res chan error
}

// fanoutThrottleKey scopes 205 throttle state to one
// recipient/entity visibility epoch (spec §7.4.5).
type fanoutThrottleKey struct {
	sid    session.ID
	entity sim.EntityID
}

// fanoutThrottle records whether a 205 has been emitted in the
// current visibility epoch and the last emitted MovementUpdate.Tick.
type fanoutThrottle struct {
	sent bool
	last uint32
}

// FanoutRuntime consumes authoritative sim.MovementUpdate events and
// composes recipient-local 204/205/206 transport (spec §7.4). It
// owns no gameplay or movement rules. Exactly one bounded queue and
// exactly one pump goroutine exist per runtime; the sim-owner
// MovementSink admission never blocks. Use NewFanoutRuntime to
// construct and Close to shut down (idempotent).
type FanoutRuntime struct {
	presence *PresenceRegistry
	sessions *session.Registry
	source   EntityPresentationSource
	stride   uint32

	events    chan fanoutEvent
	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup

	dropped atomic.Uint64

	// Pump-owned state below: touched only by the pump goroutine,
	// never shared with producers.
	ready    map[session.ID]bool
	throttle map[fanoutThrottleKey]fanoutThrottle
}

var (
	_ sim.MovementSink = (*FanoutRuntime)(nil)
	_ FanoutLifecycle  = (*FanoutRuntime)(nil)
)

// NewFanoutRuntime wires a FanoutRuntime and starts its pump.
// Presence, sessions, and source are required; tickHz (the
// configured sim rate driving MovementUpdate.Tick) must be 1..120.
// The stride is max(1, (tickHz+9)/10): 20 Hz -> 2 for the 10 Hz cap.
func NewFanoutRuntime(presence *PresenceRegistry, sessions *session.Registry, source EntityPresentationSource, tickHz int) (*FanoutRuntime, error) {
	if presence == nil {
		return nil, errors.New("gateway: presence registry is required")
	}
	if sessions == nil {
		return nil, errors.New("gateway: session registry is required")
	}
	if source == nil {
		return nil, errors.New("gateway: presentation source is required")
	}
	if tickHz < 1 || tickHz > 120 {
		return nil, fmt.Errorf("gateway: invalid fanout tickHz %d", tickHz)
	}
	r := &FanoutRuntime{
		presence: presence,
		sessions: sessions,
		source:   source,
		stride:   uint32(max(1, (tickHz+9)/10)),
		events:   make(chan fanoutEvent, FanoutEventCapacity),
		done:     make(chan struct{}),
		ready:    make(map[session.ID]bool),
		throttle: make(map[fanoutThrottleKey]fanoutThrottle),
	}
	r.wg.Add(1)
	go r.loop()
	return r, nil
}

// OnMovement implements sim.MovementSink (spec §7.4.3): immediate
// non-blocking admission only. A full queue drops the NEWEST update
// with a counted/sampled diagnostic; a closed runtime drops. It
// never waits, sleeps, writes sockets, or touches Presence locks or
// the presentation source from the sim owner goroutine.
func (r *FanoutRuntime) OnMovement(u sim.MovementUpdate) {
	select {
	case <-r.done:
		r.countDrop("closed")
		return
	default:
	}
	select {
	case r.events <- fanoutEvent{kind: fanoutMovement, update: u}:
	default:
		r.countDrop("queue_full")
	}
}

func (r *FanoutRuntime) countDrop(reason string) {
	n := r.dropped.Add(1)
	if n%1000 == 1 {
		slog.Warn("gateway: fanout movement dropped",
			"reason", reason, "total", n)
	}
}

// DroppedMovementUpdates returns the bounded atomic diagnostic count
// of dropped movement fanout events (spec §7.4.3).
func (r *FanoutRuntime) DroppedMovementUpdates() uint64 {
	return r.dropped.Load()
}

// BootstrapSession enqueues the reliable post-219 bootstrap control
// for sid and waits for its exact result (spec §7.4.3/§7.4.4).
func (r *FanoutRuntime) BootstrapSession(ctx context.Context, sid session.ID) error {
	return r.submitControl(ctx, fanoutEvent{kind: fanoutBootstrap, sid: sid})
}

// RemovePresence enqueues the reliable post-sim-remove cleanup
// control for sid's entity and waits for its exact result.
func (r *FanoutRuntime) RemovePresence(ctx context.Context, sid session.ID, entity sim.EntityID) error {
	return r.submitControl(ctx, fanoutEvent{kind: fanoutRemove, sid: sid, entity: entity})
}

func (r *FanoutRuntime) submitControl(ctx context.Context, ev fanoutEvent) error {
	ev.res = make(chan error, 1)
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(FanoutControlTimeout)
	defer timer.Stop()
	select {
	case <-r.done:
		return ErrFanoutClosed
	case r.events <- ev:
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ErrFanoutControlTimeout
	}
	select {
	case err := <-ev.res:
		return err
	case <-r.done:
		return ErrFanoutClosed
	}
}

// Close shuts the runtime down idempotently: queued movements are
// discarded, queued control waiters receive ErrFanoutClosed, the
// pump exits, and future movement drops / future control is
// rejected. No send-on-closed panic; no restart.
func (r *FanoutRuntime) Close() {
	r.closeOnce.Do(func() { close(r.done) })
	r.wg.Wait()
}

func (r *FanoutRuntime) loop() {
	defer r.wg.Done()
	for {
		select {
		case <-r.done:
			r.drain()
			return
		case ev := <-r.events:
			// Close wins over a taken-but-unprocessed event: once the
			// runtime is closed, controls complete with ErrFanoutClosed
			// instead of racing a dispatch (spec §7.4.3). An in-flight
			// dispatch (already past this check) completes normally —
			// its admitted event stays authoritative.
			select {
			case <-r.done:
				ev.complete(ErrFanoutClosed)
				r.drain()
				return
			default:
			}
			r.dispatch(ev)
		}
	}
}

// complete delivers the control result exactly once.
func (ev *fanoutEvent) complete(err error) {
	if ev.res != nil {
		ev.res <- err
	}
}

func (r *FanoutRuntime) drain() {
	for {
		select {
		case ev := <-r.events:
			ev.complete(ErrFanoutClosed)
		default:
			return
		}
	}
}

func (r *FanoutRuntime) dispatch(ev fanoutEvent) {
	var err error
	switch ev.kind {
	case fanoutMovement:
		r.handleMovement(ev.update)
	case fanoutBootstrap:
		err = r.handleBootstrap(ev.sid)
	case fanoutRemove:
		err = r.handleRemove(ev.sid, ev.entity)
	}
	ev.complete(err)
}

// producerFor resolves sid's queue-capable outbound without holding
// any Presence lock (spec §7.4.5: never hold it across outbound).
func (r *FanoutRuntime) producerFor(sid session.ID) (OutboundProducer, error) {
	snap, ok := r.sessions.Get(sid)
	if !ok || snap.Conn == nil {
		return nil, ErrFanoutNoProducer
	}
	p, ok := snap.Conn.(OutboundProducer)
	if !ok {
		return nil, ErrFanoutNoProducer
	}
	return p, nil
}

// failRecipient marks sid not-ready and best-effort closes its
// transport (spec §7.4.5/§7.4.6): reconnect/full-resync is recovery.
// Presence mappings stay for the reaper/takeover paths to tear down.
func (r *FanoutRuntime) failRecipient(sid session.ID, reason string, err error) {
	delete(r.ready, sid)
	if snap, ok := r.sessions.Get(sid); ok && snap.Conn != nil {
		_ = snap.Conn.CloseNow()
	}
	slog.Warn("gateway: fanout recipient failed closed",
		"session", uint64(sid), "reason", reason, "err", err)
}

// handleMovement fans one authoritative MovementUpdate out (spec
// §7.4.5). Best effort per recipient; no error propagates to sim.
func (r *FanoutRuntime) handleMovement(u sim.MovementUpdate) {
	cell, err := world.CellForPosition(u.Position)
	if err != nil {
		slog.Error("gateway: fanout invalid authoritative position",
			"entity", uint64(u.EntityID), "err", err)
		return
	}
	owner, controlled := r.presence.Controller(u.EntityID)
	if controlled {
		snap, err := r.presence.Snapshot(owner)
		if err != nil {
			controlled = false
		} else if snap.CenterCell != cell {
			if _, err := r.presence.UpdateCenter(owner, cell); err != nil {
				slog.Error("gateway: fanout owner center update",
					"session", uint64(owner), "err", err)
				return
			}
			// Same-cell movement causes no subscription churn:
			// the full visible-set reconciliation runs only on a
			// center-cell change (spec §7.4.5).
			if r.ready[owner] {
				r.reconcileVisibleSet(owner, nil)
			}
		}
	}
	desired := r.readyOnly(r.presence.Subscribers(cell))
	current := r.readyOnly(r.presence.Viewers(u.EntityID))
	desiredSet := toSessionSet(desired)
	currentSet := toSessionSet(current)
	for _, sid := range desired {
		if !currentSet[sid] {
			r.addViewerCreate(sid, u.EntityID, &u)
		}
	}
	for _, sid := range current {
		if !desiredSet[sid] {
			r.removeViewerDestroy(sid, u.EntityID)
		}
	}
	for _, sid := range current {
		if desiredSet[sid] {
			r.sendMove(sid, u, controlled && sid == owner)
		}
	}
}

// reconcileVisibleSet reconciles sid's entire visible set against
// its NEW 49-cell desired set (spec §7.4.5): desired-but-invisible
// creates, visible-but-undesired removes with old-handle ordering,
// intersection retained, own always retained. dyn carries an
// optional movement event overriding presentation dynamics.
func (r *FanoutRuntime) reconcileVisibleSet(sid session.ID, dyn *sim.MovementUpdate) {
	snap, err := r.presence.Snapshot(sid)
	if err != nil {
		return
	}
	desired := map[sim.EntityID]struct{}{snap.EntityID: {}}
	for _, c := range snap.Cells {
		for _, pres := range r.source.EntitiesInCell(c) {
			desired[pres.EntityID] = struct{}{}
		}
	}
	visible, err := r.presence.VisibleEntities(sid)
	if err != nil {
		return
	}
	visibleSet := make(map[sim.EntityID]struct{}, len(visible))
	for _, e := range visible {
		visibleSet[e] = struct{}{}
	}
	var creates, removes []sim.EntityID
	for e := range desired {
		if _, ok := visibleSet[e]; !ok {
			creates = append(creates, e)
		}
	}
	for e := range visibleSet {
		if _, ok := desired[e]; !ok && e != snap.EntityID {
			removes = append(removes, e)
		}
	}
	slices.Sort(creates)
	slices.Sort(removes)
	for _, e := range creates {
		if _, _, err := r.presence.EnsureVisible(sid, e); err != nil {
			r.failRecipient(sid, "reconcile_ensure", err)
			continue
		}
		var event *sim.MovementUpdate
		if dyn != nil && dyn.EntityID == e {
			event = dyn
		}
		r.emitCreate(sid, e, event)
	}
	for _, e := range removes {
		r.removeViewerDestroy(sid, e)
	}
}

// addViewerCreate EnsureVisibles entity for sid and emits its 204
// (spec §7.4.5). The update causing visibility supplies
// position/yaw/speed; kind/proto always come from the presentation
// source. Already-visible targets are left alone (no duplicate
// 204); failures fail the recipient closed.
func (r *FanoutRuntime) addViewerCreate(sid session.ID, entity sim.EntityID, u *sim.MovementUpdate) {
	_, created, err := r.presence.EnsureVisible(sid, entity)
	if err != nil {
		r.failRecipient(sid, "ensure_visible", err)
		return
	}
	if !created {
		return
	}
	r.emitCreate(sid, entity, u)
}

// emitCreate sends one 204 for an already-EnsureVisible mapping
// using the recipient's current handle (spec §7.4.5). New-handle
// admission failure retires the handle (never reused) and fails the
// recipient; own-handle failure closes the owner without hiding.
func (r *FanoutRuntime) emitCreate(sid session.ID, entity sim.EntityID, u *sim.MovementUpdate) {
	pres, ok := r.source.Entity(entity)
	if !ok {
		r.failRecipient(sid, "presentation_missing", ErrPresentationMissing)
		return
	}
	h, visible, err := r.presence.VisibleHandle(sid, entity)
	if err != nil || !visible {
		r.failRecipient(sid, "handle_missing", err)
		return
	}
	pos, yaw, speed := pres.Position, pres.Yaw, pres.Speed
	if u != nil {
		pos, yaw, speed = u.Position, u.Yaw, u.Speed
	}
	wire, err := WirePosition(pos)
	if err != nil {
		r.failRecipient(sid, "wire_position", err)
		return
	}
	entry := proto.EntityEntry{
		Entity: uint32(h),
		Kind:   pres.Kind,
		Proto:  pres.Proto,
		Pos:    wire,
		Angle:  yaw,
		Speed:  speed,
	}
	err = r.tryCritical(sid, proto.OpcodeEntityCreate,
		func(e *proto.Encoder) error {
			proto.EntityCreate{Entity: entry}.Encode(e)
			return nil
		})
	if err != nil {
		if snap, serr := r.presence.Snapshot(sid); serr != nil || snap.EntityID != entity {
			_, _, _ = r.presence.HideVisible(sid, entity)
		}
		r.failRecipient(sid, "create_critical", err)
		return
	}
	delete(r.throttle, fanoutThrottleKey{sid: sid, entity: entity})
}

// removeViewerDestroy emits 206 with the OLD handle then retires the
// mapping (spec §7.4.5). Critical failure still retires: the
// recipient is already failed closed and reconnects/resyncs.
func (r *FanoutRuntime) removeViewerDestroy(sid session.ID, entity sim.EntityID) {
	h, visible, err := r.presence.VisibleHandle(sid, entity)
	if err != nil || !visible {
		return
	}
	if cerr := r.tryCritical(sid, proto.OpcodeEntityRemove,
		func(e *proto.Encoder) error {
			proto.EntityRemove{Entity: uint32(h)}.Encode(e)
			return nil
		}); cerr != nil {
		slog.Warn("gateway: fanout 206 critical failed, retiring anyway",
			"session", uint64(sid), "entity", uint64(entity), "err", cerr)
		r.failRecipient(sid, "remove_critical", cerr)
	}
	_, _, _ = r.presence.HideVisible(sid, entity)
	delete(r.throttle, fanoutThrottleKey{sid: sid, entity: entity})
}

// sendMove emits one throttled 205 to an already-visible recipient
// (spec §7.4.5). Owners carry the processed-input anchor; observers
// always see 0. State drops change nothing; closed recipients stop
// being targeted.
func (r *FanoutRuntime) sendMove(sid session.ID, u sim.MovementUpdate, isOwner bool) {
	h, visible, err := r.presence.VisibleHandle(sid, u.EntityID)
	if err != nil || !visible {
		return
	}
	key := fanoutThrottleKey{sid: sid, entity: u.EntityID}
	th := r.throttle[key]
	if th.sent && u.Tick-th.last < r.stride {
		return
	}
	wire, err := WirePosition(u.Position)
	if err != nil {
		r.failRecipient(sid, "move_wire_position", err)
		return
	}
	var anchor uint32
	if isOwner {
		anchor = u.LastProcessedInputSeq
	}
	move := proto.EntityMove{
		Entity:                uint32(h),
		Pos:                   wire,
		Angle:                 u.Yaw,
		Speed:                 u.Speed,
		LastProcessedInputSeq: anchor,
	}
	p, err := r.producerFor(sid)
	if err != nil {
		r.failRecipient(sid, "move_no_producer", err)
		return
	}
	res, err := p.TryState(sid,
		StateKey{Kind: proto.OpcodeEntityMove, ID: uint64(h)},
		proto.OpcodeEntityMove, proto.MessageVersion1,
		func(e *proto.Encoder) error {
			move.Encode(e)
			return nil
		})
	if err != nil {
		r.failRecipient(sid, "move_state", err)
		return
	}
	switch res {
	case StateClosed:
		delete(r.ready, sid)
	case StateDropped:
		// Coalescing budget shed newest state only: mapping and
		// throttle epoch are untouched; later movement corrects.
	case StateQueued, StateCoalesced:
		r.throttle[key] = fanoutThrottle{sent: true, last: u.Tick}
	}
}

func (r *FanoutRuntime) tryCritical(sid session.ID, opcode uint16, encode func(*proto.Encoder) error) error {
	p, err := r.producerFor(sid)
	if err != nil {
		return err
	}
	return p.TryCritical(sid, opcode, proto.MessageVersion1, encode)
}

// handleBootstrap brings one committed session to fanout-ready
// (spec §7.4.4): deterministic EntityID-ascending 204s (own handle 1
// included), then ready=true, then existing-viewer notification for
// the new controlled entity. Any joining-session failure leaves
// ready=false for the runtime rollback path.
func (r *FanoutRuntime) handleBootstrap(sid session.ID) error {
	snap, err := r.presence.Snapshot(sid)
	if err != nil {
		return err
	}
	seen := map[sim.EntityID]struct{}{snap.EntityID: {}}
	for _, c := range snap.Cells {
		for _, pres := range r.source.EntitiesInCell(c) {
			seen[pres.EntityID] = struct{}{}
		}
	}
	ids := make([]sim.EntityID, 0, len(seen))
	for e := range seen {
		ids = append(ids, e)
	}
	slices.Sort(ids)
	if _, ok := r.source.Entity(snap.EntityID); !ok {
		return fmt.Errorf("%w: own entity %d", ErrPresentationMissing, uint64(snap.EntityID))
	}
	for _, e := range ids {
		if _, _, err := r.presence.EnsureVisible(sid, e); err != nil {
			return err
		}
		if berr := r.emitCreateBoot(sid, snap, e); berr != nil {
			return berr
		}
	}
	r.ready[sid] = true
	r.notifyExistingViewers(snap)
	return nil
}

// emitCreateBoot sends one bootstrap 204 from presentation state.
// New-handle failure retires the handle; own-handle failure closes
// the joining session without hiding.
func (r *FanoutRuntime) emitCreateBoot(sid session.ID, snap PresenceSnapshot, entity sim.EntityID) error {
	pres, ok := r.source.Entity(entity)
	if !ok {
		r.failRecipient(sid, "bootstrap_presentation_missing", ErrPresentationMissing)
		return fmt.Errorf("%w: entity %d", ErrPresentationMissing, uint64(entity))
	}
	h, visible, err := r.presence.VisibleHandle(sid, entity)
	if err != nil || !visible {
		r.failRecipient(sid, "bootstrap_handle_missing", err)
		if err == nil {
			err = ErrFanoutNoProducer
		}
		return err
	}
	wire, err := WirePosition(pres.Position)
	if err != nil {
		r.failRecipient(sid, "bootstrap_wire_position", err)
		return err
	}
	entry := proto.EntityEntry{
		Entity: uint32(h),
		Kind:   pres.Kind,
		Proto:  pres.Proto,
		Pos:    wire,
		Angle:  pres.Yaw,
		Speed:  pres.Speed,
	}
	if err := r.tryCritical(sid, proto.OpcodeEntityCreate,
		func(e *proto.Encoder) error {
			proto.EntityCreate{Entity: entry}.Encode(e)
			return nil
		}); err != nil {
		if entity != snap.EntityID {
			_, _, _ = r.presence.HideVisible(sid, entity)
		}
		r.failRecipient(sid, "bootstrap_create_critical", err)
		return err
	}
	delete(r.throttle, fanoutThrottleKey{sid: sid, entity: entity})
	return nil
}

// notifyExistingViewers exposes a freshly bootstrapped controlled
// entity to every OTHER ready session subscribed to its cell (spec
// §7.4.4). One viewer's failure closes only that viewer.
func (r *FanoutRuntime) notifyExistingViewers(snap PresenceSnapshot) {
	ownPres, ok := r.source.Entity(snap.EntityID)
	if !ok {
		return
	}
	cell, err := world.CellForPosition(ownPres.Position)
	if err != nil {
		return
	}
	for _, other := range r.readyOnly(r.presence.Subscribers(cell)) {
		if other == snap.SessionID {
			continue
		}
		r.addViewerCreate(other, snap.EntityID, nil)
	}
}

// handleRemove processes the post-sim-remove cleanup control (spec
// §7.4.6): source goes not-ready with throttle cleared, ready
// viewers get 206 with mapping retirement, the source gets no own
// 206. Non-ready viewers retire silently (no wire debt).
func (r *FanoutRuntime) handleRemove(sid session.ID, entity sim.EntityID) error {
	delete(r.ready, sid)
	for k := range r.throttle {
		if k.sid == sid || k.entity == entity {
			delete(r.throttle, k)
		}
	}
	viewers := r.presence.Viewers(entity)
	for _, v := range viewers {
		if v == sid {
			continue
		}
		if r.ready[v] {
			r.removeViewerDestroy(v, entity)
		} else if _, _, err := r.presence.HideVisible(v, entity); err != nil {
			slog.Warn("gateway: fanout silent retire failed",
				"session", uint64(v), "entity", uint64(entity), "err", err)
		}
	}
	return nil
}

// readyOnly filters session IDs through the pump ready set,
// returning a fresh slice.
func (r *FanoutRuntime) readyOnly(ids []session.ID) []session.ID {
	var out []session.ID
	for _, sid := range ids {
		if r.ready[sid] {
			out = append(out, sid)
		}
	}
	return out
}

func toSessionSet(ids []session.ID) map[session.ID]bool {
	set := make(map[session.ID]bool, len(ids))
	for _, sid := range ids {
		set[sid] = true
	}
	return set
}
