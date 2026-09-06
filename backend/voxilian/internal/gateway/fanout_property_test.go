package gateway

// M4-T5b2 property tests (spec §7.4): an INDEPENDENT hard-coded model
// of the frozen AOI fanout semantics mirrors every randomized action
// and predicts the exact admission stream per recipient. Divergence in
// visibility sets, handle lifetime, wire order, anchors, or throttle
// spacing between model and implementation fails the test.

import (
	"context"
	"errors"
	"math/rand"
	"testing"
	"time"

	"github.com/dlukt/voxilian/internal/proto"
	"github.com/dlukt/voxilian/internal/session"
	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/world"
)

// ---------------------------------------------------------------------------
// model
// ---------------------------------------------------------------------------

// fpWire is one predicted/recorded admission.
type fpWire struct {
	op     uint16
	handle uint32
	anchor uint32
	angle  uint16 // tick marker for 205s
	entity sim.EntityID
}

type fpThrottle struct {
	sent bool
	last uint32
}

// fpSession is the model's view of one session.
type fpSession struct {
	sid      session.ID
	char     int64
	entity   sim.EntityID
	active   bool
	ready    bool
	center   world.CellCoord
	spawn    world.Vec3
	visible  map[sim.EntityID]NetEntityID
	next     NetEntityID
	throttle map[sim.EntityID]fpThrottle
	retired  map[NetEntityID]bool
	wire     []fpWire
	epochAt  int // wire index where the current presence epoch began
}

// fpEntity is the model's view of one presented entity.
type fpEntity struct {
	id      sim.EntityID
	pos     world.Vec3
	present bool
}

type fpModel struct {
	t        *testing.T
	f        *fanoutFixture
	seed     int64
	act      int
	lastOp   string
	stride   uint32
	sessions map[session.ID]*fpSession
	order    []session.ID
	entities map[sim.EntityID]*fpEntity
	nextEnt  sim.EntityID
	tick     uint32
}

func fpCell(p world.Vec3) world.CellCoord {
	c, err := world.CellForPosition(p)
	if err != nil {
		panic(err)
	}
	return c
}

// subscribed reports whether s's 49-cell base AOI covers cell.
func (s *fpSession) subscribed(cell world.CellCoord) bool {
	d := int64(cell.X) - int64(s.center.X)
	if d < 0 {
		d = -d
	}
	z := int64(cell.Z) - int64(s.center.Z)
	if z < 0 {
		z = -z
	}
	return d <= AOICellRadius && z <= AOICellRadius
}

func (m *fpModel) controller(e sim.EntityID) (session.ID, bool) {
	for _, sid := range m.order {
		s := m.sessions[sid]
		if s.active && s.entity == e {
			return sid, true
		}
	}
	return 0, false
}

func (m *fpModel) viewers(e sim.EntityID) []session.ID {
	var out []session.ID
	for _, sid := range m.order {
		if _, ok := m.sessions[sid].visible[e]; ok {
			out = append(out, sid)
		}
	}
	return out
}

// allocHandle allocates the recipient-local handle for a newly visible
// entity (own is pinned to 1 at activation).
func (s *fpSession) allocHandle(e sim.EntityID) NetEntityID {
	var h NetEntityID
	if e == s.entity {
		h = NetEntityID(1)
		if _, exists := s.visible[e]; !exists {
			s.visible[e] = h
		}
	} else {
		h = s.next
		s.next++
		s.visible[e] = h
	}
	delete(s.throttle, e)
	return h
}

func (s *fpSession) retire(e sim.EntityID) {
	if h, ok := s.visible[e]; ok {
		delete(s.visible, e)
		s.retired[h] = true
		delete(s.throttle, e)
	}
}

// emit records one predicted admission.
func (s *fpSession) emit(w fpWire) {
	s.wire = append(s.wire, w)
}

// ---------------------------------------------------------------------------
// actions (mirroring spec §7.4 semantics exactly)
// ---------------------------------------------------------------------------

// enter activates + bootstraps one session with a fresh entity.
func (m *fpModel) enter(s *fpSession, entity sim.EntityID, spawn world.Vec3) {
	m.t.Helper()
	s.entity = entity
	s.spawn = spawn
	s.center = fpCell(spawn)
	s.visible = map[sim.EntityID]NetEntityID{entity: 1}
	s.next = 2
	s.throttle = map[sim.EntityID]fpThrottle{}
	// A new presence epoch restarts handle numbering (spec §7.2.8:
	// no reuse WITHIN an epoch); retirement history resets with it,
	// and the wire-lifetime invariants restart at the epoch boundary.
	s.retired = map[NetEntityID]bool{}
	s.epochAt = len(s.wire)
	s.active = true
	m.entities[entity] = &fpEntity{id: entity, pos: spawn, present: true}
	m.f.activate(s.sid, s.char, entity, s.center)
	m.f.source.put(EntityPresentation{
		EntityID: entity, Position: spawn, Kind: 2, Proto: 7, Yaw: 1,
	})
	m.f.bootstrap(s.sid)
	// Deterministic bootstrap set: own + presented entities in the 49
	// cells, EntityID ascending; own pinned to 1.
	ids := []sim.EntityID{entity}
	for e, ent := range m.entities {
		if e != entity && ent.present && s.subscribed(fpCell(ent.pos)) {
			ids = append(ids, e)
		}
	}
	sortFPIDs(ids)
	for _, e := range ids {
		h := s.allocHandle(e)
		s.emit(fpWire{op: proto.OpcodeEntityCreate, handle: uint32(h), entity: e})
	}
	s.ready = true
	// Existing ready subscribers of the own entity's cell learn it.
	for _, sid := range m.order {
		o := m.sessions[sid]
		if o.active && o.ready && o.sid != s.sid && o.subscribed(fpCell(spawn)) {
			if _, ok := o.visible[entity]; !ok {
				h := o.allocHandle(entity)
				o.emit(fpWire{op: proto.OpcodeEntityCreate, handle: uint32(h)})
			}
		}
	}
}

// exit mirrors the runtime's successful leave order.
func (m *fpModel) exit(s *fpSession) {
	m.t.Helper()
	if err := m.f.fanout.RemovePresence(context.Background(), s.sid, s.entity); err != nil {
		m.t.Fatalf("model exit: %v", err)
	}
	for _, v := range m.viewers(s.entity) {
		if v == s.sid {
			continue
		}
		vs := m.sessions[v]
		h, ok := vs.visible[s.entity]
		if !ok {
			continue
		}
		if vs.ready {
			vs.emit(fpWire{op: proto.OpcodeEntityRemove, handle: uint32(h), entity: s.entity})
		}
		vs.retire(s.entity)
	}
	if _, err := m.f.presence.Deactivate(s.sid); err != nil {
		m.t.Fatalf("model deactivate: %v", err)
	}
	m.entities[s.entity].present = false
	m.f.source.del(s.entity)
	s.active = false
	s.ready = false
	s.visible = map[sim.EntityID]NetEntityID{}
	s.throttle = map[sim.EntityID]fpThrottle{}
}

// move drives one authoritative movement update.
func (m *fpModel) move(e sim.EntityID, pos world.Vec3, anchor uint32) {
	m.t.Helper()
	ent := m.entities[e]
	oldCell := fpCell(ent.pos)
	ent.pos = pos
	m.f.source.setPos(e, pos) // the world layer tracks authoritative movement
	cell := fpCell(pos)
	m.tick++
	tick := m.tick
	// Drive the real runtime: yaw carries the tick marker so 205
	// spacing is verifiable on the wire.
	m.f.fanout.OnMovement(sim.MovementUpdate{
		EntityID: e, Position: pos, Yaw: uint16(tick & 0xFFF), Speed: 35,
		Tick: tick, LastProcessedInputSeq: anchor,
	})
	_ = oldCell
	if owner, ok := m.controller(e); ok {
		s := m.sessions[owner]
		if cell != s.center {
			s.center = cell
			if s.ready {
				m.reconcileOwner(s)
			}
		}
	}
	// Viewer reconciliation for the moved entity.
	desired := map[session.ID]bool{}
	for _, sid := range m.order {
		s := m.sessions[sid]
		if s.active && s.ready && s.subscribed(cell) {
			desired[sid] = true
		}
	}
	current := map[session.ID]bool{}
	for _, sid := range m.order {
		s := m.sessions[sid]
		if s.active && s.ready {
			if _, ok := s.visible[e]; ok {
				current[sid] = true
			}
		}
	}
	var adds []session.ID
	for sid := range desired {
		if !current[sid] {
			adds = append(adds, sid)
		}
	}
	sortFPSIDs(adds)
	var removes []session.ID
	for sid := range current {
		if !desired[sid] {
			removes = append(removes, sid)
		}
	}
	sortFPSIDs(removes)
	owner, _ := m.controller(e)
	for _, sid := range adds {
		s := m.sessions[sid]
		h := s.allocHandle(e)
		s.emit(fpWire{op: proto.OpcodeEntityCreate, handle: uint32(h), entity: e})
	}
	for _, sid := range removes {
		s := m.sessions[sid]
		h := s.visible[e]
		s.emit(fpWire{op: proto.OpcodeEntityRemove, handle: uint32(h), entity: e})
		s.retire(e)
	}
	for _, sid := range m.order {
		s := m.sessions[sid]
		if !s.active || !s.ready {
			continue
		}
		if desired[s.sid] && current[s.sid] {
			th := s.throttle[e]
			if th.sent && tick-th.last < m.stride {
				continue
			}
			h := s.visible[e]
			anc := uint32(0)
			if sid == owner {
				anc = anchor
			}
			s.emit(fpWire{op: proto.OpcodeEntityMove, handle: uint32(h), anchor: anc, angle: uint16(tick), entity: e})
			s.throttle[e] = fpThrottle{sent: true, last: tick}
		}
	}
}

// reconcileOwner mirrors the full-set reconciliation after the owner's
// center-cell change.
func (m *fpModel) reconcileOwner(s *fpSession) {
	m.t.Helper()
	desired := map[sim.EntityID]bool{s.entity: true}
	for e, ent := range m.entities {
		if ent.present && s.subscribed(fpCell(ent.pos)) {
			desired[e] = true
		}
	}
	var creates, removes []sim.EntityID
	for e := range desired {
		if _, ok := s.visible[e]; !ok {
			creates = append(creates, e)
		}
	}
	for e := range s.visible {
		if !desired[e] && e != s.entity {
			removes = append(removes, e)
		}
	}
	sortFPIDs(creates)
	sortFPIDs(removes)
	for _, e := range creates {
		h := s.allocHandle(e)
		s.emit(fpWire{op: proto.OpcodeEntityCreate, handle: uint32(h), entity: e})
	}
	for _, e := range removes {
		h := s.visible[e]
		s.emit(fpWire{op: proto.OpcodeEntityRemove, handle: uint32(h), entity: e})
		s.retire(e)
	}
}

func sortFPIDs(ids []sim.EntityID) {
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && ids[j-1] > ids[j]; j-- {
			ids[j-1], ids[j] = ids[j], ids[j-1]
		}
	}
}

func sortFPSIDs(ids []session.ID) {
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && ids[j-1] > ids[j]; j-- {
			ids[j-1], ids[j] = ids[j], ids[j-1]
		}
	}
}

// ---------------------------------------------------------------------------
// invariant checks after every action
// ---------------------------------------------------------------------------

func (m *fpModel) assertInvariants() {
	m.t.Helper()
	m.f.drainPump()
	for _, sid := range m.order {
		s := m.sessions[sid]
		if !s.active {
			if _, err := m.f.presence.Snapshot(sid); !errors.Is(err, ErrPresenceNotFound) {
				m.t.Fatalf("inactive session %d keeps presence: %v", sid, err)
			}
			continue
		}
		snap, err := m.f.presence.Snapshot(sid)
		if err != nil {
			m.t.Fatalf("active session %d lost presence: %v", sid, err)
		}
		if snap.OwnNetID != 1 {
			m.t.Fatalf("session %d own handle = %d, want 1", sid, snap.OwnNetID)
		}
		vis, err := m.f.presence.VisibleEntities(sid)
		if err != nil {
			m.t.Fatal(err)
		}
		if len(vis) != len(s.visible) {
			m.t.Fatalf("seed %d act %d (%s): session %d visible = %v, model %v",
				m.seed, m.act, m.lastOp, sid, vis, s.visible)
		}
		for i, e := range vis {
			if _, ok := s.visible[e]; !ok {
				m.t.Fatalf("session %d sees %v, model sees keys of %v (mismatch at %d)", sid, vis, s.visible, i)
			}
			h, _, err := m.f.presence.VisibleHandle(sid, e)
			if err != nil || h != s.visible[e] {
				m.t.Fatalf("session %d handle for %d = %d, model %d (%v)", sid, e, h, s.visible[e], err)
			}
			got, ok := m.f.presence.ResolveHandle(sid, h)
			if !ok || got != e {
				m.t.Fatalf("session %d resolve %d = (%d,%v), want %d", sid, h, got, ok, e)
			}
		}
		for h := range s.retired {
			if _, ok := m.f.presence.ResolveHandle(sid, h); ok {
				m.t.Fatalf("session %d retired handle %d resolves", sid, h)
			}
		}
	}
	// Reverse viewer index exactness.
	for e, ent := range m.entities {
		if !ent.present {
			if v := m.f.presence.Viewers(e); len(v) != 0 {
				m.t.Fatalf("absent entity %d has viewers %v", e, v)
			}
			continue
		}
		got := m.f.presence.Viewers(e)
		want := m.viewers(e)
		if len(got) != len(want) {
			m.t.Fatalf("entity %d viewers = %v, model %v", e, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				m.t.Fatalf("entity %d viewers = %v, model %v", e, got, want)
			}
		}
	}
	m.assertWire()
}

// assertWire compares the recorded admission stream against the model
// and checks per-epoch lifetime invariants.
func (m *fpModel) assertWire() {
	m.t.Helper()
	for _, sid := range m.order {
		s := m.sessions[sid]
		calls := m.f.prods[sid].snapshot()
		if len(calls) < len(s.wire) {
			m.t.Fatalf("session %d admitted %d frames, model %d", sid, len(calls), len(s.wire))
		}
		introduced := map[uint32]bool{}
		removed := map[uint32]bool{}
		lastTick := map[uint32]uint32{}
		for i, w := range s.wire[s.epochAt:] {
			c := calls[i+s.epochAt]
			if c.opcode != w.op {
				m.t.Fatalf("seed %d act %d (%s): session %d admission[%d] = op %d, model op %d",
					m.seed, m.act, m.lastOp, sid, i, c.opcode, w.op)
			}
			var handle uint32
			var anchor uint32
			var angle uint16
			switch c.opcode {
			case proto.OpcodeEntityCreate:
				handle = decodeCreate(m.t, c.payload).Entity.Entity
			case proto.OpcodeEntityMove:
				mv := decodeMove(m.t, c.payload)
				handle, anchor, angle = mv.Entity, mv.LastProcessedInputSeq, mv.Angle
			case proto.OpcodeEntityRemove:
				handle = decodeRemove(m.t, c.payload).Entity
			}
			if handle != w.handle {
				m.t.Fatalf("seed %d act %d (%s): session %d admission[%d] handle = %d, model %d (entity %d)",
					m.seed, m.act, m.lastOp, sid, i, handle, w.handle, w.entity)
			}
			switch c.opcode {
			case proto.OpcodeEntityCreate:
				if introduced[handle] || removed[handle] {
					m.t.Fatalf("session %d re-introduced handle %d", sid, handle)
				}
				introduced[handle] = true
			case proto.OpcodeEntityRemove:
				if !introduced[handle] || removed[handle] {
					m.t.Fatalf("session %d removed unknown/live handle %d", sid, handle)
				}
				removed[handle] = true
			case proto.OpcodeEntityMove:
				if !introduced[handle] || removed[handle] {
					m.t.Fatalf("session %d 205 for non-live handle %d", sid, handle)
				}
				// Exact anchor equality against the model already proves
				// the owner-only rule (the model emits nonzero anchors
				// solely for the controlling session).
				if anchor != w.anchor {
					m.t.Fatalf("seed %d act %d (%s): session %d 205 anchor = %d, model %d",
						m.seed, m.act, m.lastOp, sid, anchor, w.anchor)
				}
				if last, ok := lastTick[handle]; ok && uint32(angle)-last < m.stride {
					m.t.Fatalf("session %d 205 spacing %d < stride %d for handle %d",
						sid, uint32(angle)-last, m.stride, handle)
				}
				lastTick[handle] = uint32(angle)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// B61: fanout model property test
// ---------------------------------------------------------------------------

func TestFanoutPropertyModel(t *testing.T) {
	const (
		seeds   = 64
		actions = 256
		sessN   = 4
		ambN    = 6
	)
	spawns := []world.Vec3{
		{X: 0, Z: 0}, {X: 40, Z: 0}, {X: 0, Z: 40}, {X: 40, Z: 40},
	}
	ambient := []world.Vec3{
		{X: 8, Z: 8}, {X: 70, Z: 0}, {X: 0, Z: 70}, {X: -70, Z: -70},
		{X: 200, Z: 0}, {X: 0, Z: -8},
	}
	for seed := int64(1); seed <= seeds; seed++ {
		f := newFanoutFixture(t, 20)
		m := &fpModel{
			t: t, f: f, seed: seed, stride: 2,
			sessions: make(map[session.ID]*fpSession),
			entities: make(map[sim.EntityID]*fpEntity),
			nextEnt:  5000,
		}
		// Fixed ambient world content.
		for i, pos := range ambient {
			e := sim.EntityID(1000 + i)
			m.entities[e] = &fpEntity{id: e, pos: pos, present: true}
			f.source.put(EntityPresentation{
				EntityID: e, Position: pos, Kind: 3, Proto: 8, Yaw: 1,
			})
		}
		var sids []session.ID
		for i := 0; i < sessN; i++ {
			sid := f.addSession(DefaultOutboundPolicy())
			m.sessions[sid] = &fpSession{
				sid: sid, char: int64(500 + i), spawn: spawns[i],
				retired: map[NetEntityID]bool{},
			}
			m.order = append(m.order, sid)
			sids = append(sids, sid)
		}
		r := rand.New(rand.NewSource(seed))
		for a := 0; a < actions; a++ {
			m.act = a
			switch r.Intn(10) {
			case 0, 1: // enter an inactive session (fresh entity)
				m.lastOp = "enter"
				cand := m.pickInactive(r)
				if cand == nil {
					continue
				}
				m.nextEnt++
				m.enter(cand, m.nextEnt, cand.spawn)
			case 2: // exit an active session
				m.lastOp = "exit"
				cand := m.pickActive(r)
				if cand == nil {
					continue
				}
				m.exit(cand)
			default: // move a random presented entity
				m.lastOp = "move"
				var pool []sim.EntityID
				for e, ent := range m.entities {
					if ent.present {
						pool = append(pool, e)
					}
				}
				if len(pool) == 0 {
					continue
				}
				e := pool[r.Intn(len(pool))]
				anchor := uint32(0)
				if _, controlled := m.controller(e); controlled && r.Intn(2) == 0 {
					anchor = uint32(100 + r.Intn(1000))
				}
				pos := world.Vec3{
					X: float64(r.Intn(41)-20) * 10,
					Z: float64(r.Intn(41)-20) * 10,
				}
				m.lastOp = "move " + itoa(int(e)) + " -> " + itoa(int(pos.X)) + "," + itoa(int(pos.Z))
				m.move(e, pos, anchor)
			}
			m.assertInvariants()
		}
		// Drain: exit everyone, then no ghosts remain.
		for _, sid := range m.order {
			if s := m.sessions[sid]; s.active {
				m.exit(s)
				m.assertInvariants()
			}
		}
	}
}

func (m *fpModel) pickInactive(r *rand.Rand) *fpSession {
	var pool []*fpSession
	for _, sid := range m.order {
		if !m.sessions[sid].active {
			pool = append(pool, m.sessions[sid])
		}
	}
	if len(pool) == 0 {
		return nil
	}
	return pool[r.Intn(len(pool))]
}

func (m *fpModel) pickActive(r *rand.Rand) *fpSession {
	var pool []*fpSession
	for _, sid := range m.order {
		if m.sessions[sid].active {
			pool = append(pool, m.sessions[sid])
		}
	}
	if len(pool) == 0 {
		return nil
	}
	return pool[r.Intn(len(pool))]
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [12]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// ---------------------------------------------------------------------------
// B62: lifecycle/reaper property model (spec §7.4.6–§7.4.7)
// ---------------------------------------------------------------------------

// lpWorld is the model's view of one world session.
type lpWorld struct {
	sid           session.ID
	accountID     int64
	charID        int64
	entity        sim.EntityID
	inWorld       bool // active presence + IN_WORLD binding
	retained      bool // cleanup attempted and FAILED (world state kept)
	gone          bool
	dead          bool // registry entry removed (reaped/kicked): no re-enter
	enteredFanout bool
}

type lpModel struct {
	t          *testing.T
	seed       int64
	act        int
	lastOp     string
	reg        *session.Registry
	presence   *PresenceRegistry
	fakeSim    *recordingSim
	downstream *recordingDownstream
	fanout     *recordingFanout
	runtime    *WorldSessionRuntime
	reaper     *SessionReaper
	liveness   *TransportLiveness
	clk        *fakeClock
	sessions   []*lpWorld
	bySid      map[session.ID]*lpWorld
	nextEntity sim.EntityID
	flushDown  bool
}

func newLPModel(t *testing.T) *lpModel {
	t.Helper()
	reg := session.NewRegistry()
	presence, err := NewPresenceRegistry(testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{t: worldRuntimeBase}
	fakeSim := &recordingSim{}
	downstream := &recordingDownstream{}
	fanout := &recordingFanout{}
	spawn := SpawnResolverFunc(func(context.Context, int64, int64) (world.Vec3, error) {
		return world.Vec3{X: 4}, nil
	})
	runtime, err := NewWorldSessionRuntime(fakeSim, presence, reg, spawn, clk.Now, downstream, fanout)
	if err != nil {
		t.Fatal(err)
	}
	reaper, err := NewSessionReaper(reg, presence, runtime)
	if err != nil {
		t.Fatal(err)
	}
	tf := newManualTickerFactory()
	liveness, err := NewTransportLiveness(presence, reg, reaper, clk.Now, tf.newTicker)
	if err != nil {
		t.Fatal(err)
	}
	return &lpModel{
		t: t, reg: reg, presence: presence, fakeSim: fakeSim,
		downstream: downstream, fanout: fanout, runtime: runtime,
		reaper: reaper, liveness: liveness, clk: clk,
		bySid: make(map[session.ID]*lpWorld), nextEntity: 100,
	}
}

// newSession creates one AUTHENTICATED session slot.
func (m *lpModel) newSession(accountID, charID int64) *lpWorld {
	m.t.Helper()
	conn := newTakeoverConn()
	sid := m.reg.Create(conn)
	if err := m.reg.Authenticate(sid, "sub-lp", accountID, worldRuntimeBase.Add(time.Hour)); err != nil {
		m.t.Fatal(err)
	}
	w := &lpWorld{sid: sid, accountID: accountID, charID: charID}
	m.sessions = append(m.sessions, w)
	m.bySid[sid] = w
	return w
}

// enter drives the full staged enter (registry + presence + bootstrap).
func (m *lpModel) enter(w *lpWorld) {
	m.t.Helper()
	if err := m.reg.BeginEnterWorld(w.sid, w.charID); err != nil {
		m.t.Fatalf("begin enter: %v", err)
	}
	if err := m.reg.CompleteEnterWorld(w.sid, w.charID); err != nil {
		m.t.Fatalf("complete enter: %v", err)
	}
	ctx := context.Background()
	if err := m.runtime.PrepareEnter(ctx, w.sid, w.accountID, w.charID); err != nil {
		m.t.Fatalf("prepare: %v", err)
	}
	// The staged entity ID is the recording sim's allocation.
	m.fakeSim.mu.Lock()
	w.entity = m.fakeSim.nextID
	m.fakeSim.mu.Unlock()

	if err := m.runtime.CommitEnter(ctx, w.sid); err != nil {
		m.t.Fatalf("commit: %v", err)
	}
	w.inWorld, w.retained, w.gone = true, false, false
}

// leave drives the normal flush-first exit.
func (m *lpModel) leave(w *lpWorld) {
	m.t.Helper()
	err := m.runtime.ExitWorld(context.Background(), w.sid, w.accountID, w.charID)
	if m.flushDown {
		// Downstream failed: everything retained, session retryable.
		if err == nil {
			m.t.Fatalf("leave succeeded with downstream down")
		}
		w.retained = true
		return
	}
	if err != nil {
		w.retained = true
		return
	}
	if err := m.reg.CompleteLeaveWorld(w.sid, w.charID); err != nil {
		m.t.Fatalf("complete leave: %v", err)
	}
	w.inWorld, w.retained, w.gone = false, false, true
}

// disconnect drives the reaper path (raw disconnect / stale sweep).
func (m *lpModel) disconnect(w *lpWorld) {
	m.t.Helper()
	err := m.reaper.Reap(w.sid, "property_disconnect")
	if err != nil {
		if !w.retained && !w.inWorld {
			m.t.Fatalf("reap error for non-world session: %v", err)
		}
		w.retained = true
		return
	}
	w.inWorld, w.retained, w.gone = false, false, true
	w.dead = true
}

// sweep advances the clock past the heartbeat timeout and runs one
// sweep. Staleness is decided BEFORE the reap (it deactivates the
// presence being inspected); a healthy flush fully cleans every stale
// world session, a failed flush retains them for retry.
func (m *lpModel) sweep() {
	m.t.Helper()
	m.clk.set(m.clk.Now().Add(PresenceHeartbeatTimeout + time.Second))
	stale := map[session.ID]bool{}
	for _, w := range m.sessions {
		if w.inWorld && w.staleBy(m) {
			stale[w.sid] = true
		}
	}
	m.liveness.Sweep()
	for _, w := range m.sessions {
		if !w.inWorld || !stale[w.sid] {
			continue
		}
		if m.flushDown {
			if _, perr := m.presence.Snapshot(w.sid); perr != nil {
				m.t.Fatalf("sweep lost presence under failed flush")
			}
			w.retained = true
			continue
		}
		if _, perr := m.presence.Snapshot(w.sid); !errors.Is(perr, ErrPresenceNotFound) {
			m.t.Fatalf("healthy sweep left presence for stale session %d: %v", w.sid, perr)
		}
		w.inWorld, w.retained, w.gone = false, false, true
		w.dead = true
	}
}

// staleBy reports whether the model session's heartbeat is stale.
func (w *lpWorld) staleBy(m *lpModel) bool {
	snap, err := m.presence.Snapshot(w.sid)
	if err != nil {
		return false
	}
	return !m.clk.Now().Before(snap.HeartbeatAt.Add(PresenceHeartbeatTimeout))
}

// takeover emulates the duplicate-login sequence for the same account:
// old flush + unbind + kick/remove, then the new session enters.
func (m *lpModel) takeover(old *lpWorld) {
	m.t.Helper()
	if !old.inWorld {
		return
	}
	m.leave(old)
	if !old.gone {
		// Flush failed: old stays, takeover never proceeds.
		return
	}
	m.reg.Remove(old.sid)
	old.dead = true
	fresh := m.newSession(old.accountID, old.charID)
	m.enter(fresh)
}

// assert verifies the lifecycle invariants against the real state.
func (m *lpModel) assert() {
	m.t.Helper()
	active := 0
	seenChar := map[int64]bool{}
	for _, w := range m.sessions {
		if w.gone {
			if _, err := m.presence.Snapshot(w.sid); !errors.Is(err, ErrPresenceNotFound) {
				m.t.Fatalf("session %d gone but presence remains", w.sid)
			}
			if s, ok := m.reg.Get(w.sid); ok && s.State == session.StateInWorld {
				m.t.Fatalf("session %d gone but registry claims IN_WORLD", w.sid)
			}
			continue
		}
		if w.inWorld || w.retained {
			active++
			// No orphan presence without a recoverable registry entry.
			s, ok := m.reg.Get(w.sid)
			if !ok {
				m.t.Fatalf("world session %d has no registry entry", w.sid)
			}
			if s.State != session.StateInWorld || !s.HasCharacter {
				m.t.Fatalf("world session %d registry state = %+v", w.sid, s)
			}
			if _, err := m.presence.Snapshot(w.sid); err != nil {
				m.t.Fatalf("world session %d lost presence: %v", w.sid, err)
			}
			if seenChar[w.charID] {
				m.t.Fatalf("two active presences for character %d", w.charID)
			}
			seenChar[w.charID] = true
		}
	}
	if m.presence.Len() != active {
		m.t.Fatalf("presence epochs = %d, model active = %d", m.presence.Len(), active)
	}
	// Retained sessions keep their sim entity; cleaned ones do not.
	removed := map[sim.EntityID]bool{}
	for _, id := range m.removedEntities() {
		removed[id] = true
	}
	for _, w := range m.sessions {
		if w.gone && !removed[w.entity] {
			m.t.Fatalf("seed %d act %d (%s): cleaned session %d entity %d never removed",
				m.seed, m.act, m.lastOp, w.sid, w.entity)
		}
		if (w.inWorld || w.retained) && removed[w.entity] {
			m.t.Fatalf("live session %d entity %d removed early", w.sid, w.entity)
		}
	}
	// Fanout removal happened exactly for cleaned world sessions.
	removedFanout := map[session.ID]bool{}
	for _, rm := range m.fanout.removeCalls() {
		removedFanout[rm.sid] = true
	}
	for _, w := range m.sessions {
		if w.gone && w.enteredFanout && !removedFanout[w.sid] {
			m.t.Fatalf("cleaned session %d never removed from fanout", w.sid)
		}
		if (w.inWorld || w.retained) && removedFanout[w.sid] && w.enteredFanout {
			// Allowed only after a retained->cleaned transition; the
			// gone check above covers the final state.
			_ = w
		}
	}
}

func (m *lpModel) removedEntities() []sim.EntityID {
	return m.fakeSim.removedIDs()
}

func TestLifecycleReaperPropertyModel(t *testing.T) {
	const (
		seeds   = 64
		actions = 128
	)
	for seed := int64(1); seed <= seeds; seed++ {
		m := newLPModel(t)
		m.seed = seed
		r := rand.New(rand.NewSource(seed * 7919))
		var slots [3]*lpWorld
		for i := range slots {
			slots[i] = m.newSession(int64(11+i), int64(500+i))
		}
		for a := 0; a < actions; a++ {
			m.act = a
			switch r.Intn(12) {
			case 0, 1:
				m.lastOp = "enter"
				for _, w := range slots {
					if !w.inWorld && !w.retained && !w.dead {
						m.enter(w)
						w.enteredFanout = true
						break
					}
				}
			case 2:
				m.lastOp = "leave"
				for _, w := range slots {
					if w.inWorld {
						m.leave(w)
						break
					}
				}
			case 3, 4:
				m.lastOp = "disconnect"
				for _, w := range slots {
					if w.inWorld {
						m.disconnect(w)
						break
					}
				}
			case 5:
				m.flushDown = true
				m.downstream.err = errors.New("flush down")
			case 6:
				m.flushDown = false
				m.downstream.setErr(nil)
			case 7:
				m.lastOp = "sweep"
				m.sweep()
			case 8:
				m.lastOp = "takeover"
				for _, w := range slots {
					if w.inWorld {
						m.takeover(w)
						break
					}
				}
			}
			m.assert()
		}
		// Final drain: recover the flush and reap everything.
		m.flushDown = false
		m.downstream.setErr(nil)
		for {
			stale := false
			for _, w := range slots {
				if w.inWorld {
					stale = true
				}
			}
			if !stale {
				break
			}
			m.clk.set(m.clk.Now().Add(PresenceHeartbeatTimeout + time.Second))
			m.liveness.Sweep()
			for _, w := range slots {
				if w.inWorld {
					if _, err := m.presence.Snapshot(w.sid); errors.Is(err, ErrPresenceNotFound) {
						w.inWorld, w.gone = false, true
					} else {
						w.retained = true
					}
				}
			}
			m.assert()
		}
	}
}
