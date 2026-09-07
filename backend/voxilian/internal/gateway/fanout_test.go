package gateway

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/dlukt/voxilian/internal/character"
	"github.com/dlukt/voxilian/internal/proto"
	"github.com/dlukt/voxilian/internal/session"
	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/world"
)

// ---------------------------------------------------------------------------
// fakes + fixture
// ---------------------------------------------------------------------------

// fakePresentation is a scriptable EntityPresentationSource.
type fakePresentation struct {
	mu      sync.Mutex
	ents    map[sim.EntityID]EntityPresentation
	block   chan struct{}
	entered chan sim.EntityID
}

func newFakePresentation() *fakePresentation {
	return &fakePresentation{ents: make(map[sim.EntityID]EntityPresentation)}
}

func (f *fakePresentation) put(p EntityPresentation) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ents[p.EntityID] = p
}

// del drops one entity's presentation (the world layer removing it).
func (f *fakePresentation) del(e sim.EntityID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.ents, e)
}

// setPos updates one entity's authoritative presentation position (the
// world layer tracking authoritative movement; spec §7.4.2).
func (f *fakePresentation) setPos(e sim.EntityID, pos world.Vec3) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.ents[e]; ok {
		p.Position = pos
		f.ents[e] = p
	}
}

func (f *fakePresentation) Entity(e sim.EntityID) (EntityPresentation, bool) {
	if f.block != nil {
		f.entered <- e
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.ents[e]
	return p, ok
}

func (f *fakePresentation) EntitiesInCell(c world.CellCoord) []EntityPresentation {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []EntityPresentation
	for _, p := range f.ents {
		cell, err := world.CellForPosition(p.Position)
		if err != nil || cell != c {
			continue
		}
		out = append(out, p)
	}
	sortEntities(out)
	return out
}

func sortEntities(ps []EntityPresentation) {
	for i := 1; i < len(ps); i++ {
		for j := i; j > 0 && ps[j-1].EntityID > ps[j].EntityID; j-- {
			ps[j-1], ps[j] = ps[j], ps[j-1]
		}
	}
}

// recordedProducerCall captures one TryCritical/TryState admission
// with its synchronously encoded payload.
type recordedProducerCall struct {
	state   bool
	opcode  uint16
	key     StateKey
	payload []byte
}

func encodeCall(t *testing.T, opcode uint16, encode func(*proto.Encoder) error) []byte {
	t.Helper()
	// NOTE: runs on the fanout pump goroutine — never t.Fatalf here
	// (FailNow would Goexit the pump and mask the failure as a hang).
	// Errorf records the failure; the nil payload fails the test-side
	// decode assertions deterministically.
	e := proto.NewEncoder()
	if err := encode(e); err != nil {
		t.Errorf("encode opcode %d: %v", opcode, err)
		return nil
	}
	b, err := e.Bytes()
	if err != nil {
		t.Errorf("payload opcode %d: %v", opcode, err)
		return nil
	}
	return b
}

// methodRecordingProducer wraps a real *outboundConn, recording the
// admission path (critical vs state + key) with encoded payloads.
type methodRecordingProducer struct {
	OutboundProducer
	t     *testing.T
	mu    sync.Mutex
	calls []recordedProducerCall
	// cancels records every CancelState call with its result (v0.3.24
	// 206-ordering proof); empty for tests that never retire.
	cancels []cancelRecord
}

// cancelRecord is one observed CancelState call.
type cancelRecord struct {
	key    StateKey
	result StateCancelResult
}

func (m *methodRecordingProducer) TryCritical(sid session.ID, opcode uint16, ver uint16, encode func(*proto.Encoder) error) error {
	m.mu.Lock()
	m.calls = append(m.calls, recordedProducerCall{opcode: opcode, payload: encodeCall(m.t, opcode, encode)})
	m.mu.Unlock()
	return m.OutboundProducer.TryCritical(sid, opcode, ver, encode)
}

func (m *methodRecordingProducer) TryState(sid session.ID, key StateKey, opcode uint16, ver uint16, encode func(*proto.Encoder) error) (StateResult, error) {
	m.mu.Lock()
	m.calls = append(m.calls, recordedProducerCall{state: true, opcode: opcode, key: key, payload: encodeCall(m.t, opcode, encode)})
	m.mu.Unlock()
	return m.OutboundProducer.TryState(sid, key, opcode, ver, encode)
}

func (m *methodRecordingProducer) snapshot() []recordedProducerCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]recordedProducerCall(nil), m.calls...)
}

// CancelState records the call and delegates to the real queue.
func (m *methodRecordingProducer) CancelState(key StateKey) StateCancelResult {
	res := m.OutboundProducer.CancelState(key)
	m.mu.Lock()
	m.cancels = append(m.cancels, cancelRecord{key: key, result: res})
	m.mu.Unlock()
	return res
}

func (m *methodRecordingProducer) cancelSnapshot() []cancelRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]cancelRecord(nil), m.cancels...)
}

func (m *methodRecordingProducer) opcodes() []uint16 {
	var out []uint16
	for _, c := range m.snapshot() {
		out = append(out, c.opcode)
	}
	return out
}

type fanoutFixture struct {
	t        *testing.T
	reg      *session.Registry
	presence *PresenceRegistry
	source   *fakePresentation
	fanout   *FanoutRuntime
	prods    map[session.ID]*methodRecordingProducer
	trans    map[session.ID]*fakeOutTransport
	obs      map[session.ID]*recordingObserver
	tickHz   int
}

func newFanoutFixture(t *testing.T, tickHz int) *fanoutFixture {
	t.Helper()
	reg := session.NewRegistry()
	presence, err := NewPresenceRegistry(testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	source := newFakePresentation()
	fanout, err := NewFanoutRuntime(presence, reg, source, tickHz)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fanout.Close)
	return &fanoutFixture{
		t: t, reg: reg, presence: presence, source: source,
		fanout: fanout, prods: make(map[session.ID]*methodRecordingProducer),
		trans: make(map[session.ID]*fakeOutTransport),
		obs:   make(map[session.ID]*recordingObserver), tickHz: tickHz,
	}
}

// addSession creates a live session backed by a REAL outbound queue
// over a fake transport, wrapped in the admission recorder.
func (f *fanoutFixture) addSession(policy OutboundPolicy) session.ID {
	f.t.Helper()
	tr := newFakeOutTransport()
	ob := &recordingObserver{}
	oc := newOutboundConn(OutboundDeps{
		Conn: tr, Registry: f.reg,
		Tick:     func() uint32 { return 1000 },
		Policy:   policy,
		Observer: ob,
	})
	rec := &methodRecordingProducer{OutboundProducer: oc, t: f.t}
	sid := f.reg.Create(rec)
	f.prods[sid] = rec
	f.trans[sid] = tr
	f.obs[sid] = ob
	return sid
}

func (f *fanoutFixture) activate(sid session.ID, char int64, entity sim.EntityID, center world.CellCoord) {
	f.t.Helper()
	if _, err := f.presence.Activate(sid, char, entity, center, worldRuntimeBase); err != nil {
		f.t.Fatalf("Activate: %v", err)
	}
}

func (f *fanoutFixture) bootstrap(sid session.ID) {
	f.t.Helper()
	if err := f.fanout.BootstrapSession(context.Background(), sid); err != nil {
		f.t.Fatalf("BootstrapSession(%d): %v", sid, err)
	}
}

// drainPump is the deterministic test barrier: a no-effect
// RemovePresence control ordered after every earlier event.
func (f *fanoutFixture) drainPump() {
	f.t.Helper()
	_ = f.fanout.RemovePresence(context.Background(), session.ID(999999), sim.EntityID(888888))
}

func (f *fanoutFixture) move(entity sim.EntityID, pos world.Vec3, yaw uint16, speed uint8, tick uint32, anchor uint32) {
	f.t.Helper()
	f.fanout.OnMovement(sim.MovementUpdate{
		EntityID: entity, Position: pos, Yaw: yaw, Speed: speed,
		Tick: tick, LastProcessedInputSeq: anchor,
	})
}

func decodeCreate(t *testing.T, payload []byte) proto.EntityCreate {
	t.Helper()
	m, err := proto.DecodeEntityCreate(proto.NewDecoder(payload))
	if err != nil {
		t.Fatalf("decode 204: %v", err)
	}
	return m
}

func decodeMove(t *testing.T, payload []byte) proto.EntityMove {
	t.Helper()
	m, err := proto.DecodeEntityMove(proto.NewDecoder(payload))
	if err != nil {
		t.Fatalf("decode 205: %v", err)
	}
	return m
}

func decodeRemove(t *testing.T, payload []byte) proto.EntityRemove {
	t.Helper()
	m, err := proto.DecodeEntityRemove(proto.NewDecoder(payload))
	if err != nil {
		t.Fatalf("decode 206: %v", err)
	}
	return m
}

func TestFanoutRequiresDeps(t *testing.T) {
	reg := session.NewRegistry()
	presence, _ := NewPresenceRegistry(testPolicy())
	source := newFakePresentation()
	if _, err := NewFanoutRuntime(nil, reg, source, 20); err == nil {
		t.Error("nil presence accepted")
	}
	if _, err := NewFanoutRuntime(presence, nil, source, 20); err == nil {
		t.Error("nil sessions accepted")
	}
	if _, err := NewFanoutRuntime(presence, reg, nil, 20); err == nil {
		t.Error("nil source accepted")
	}
	for _, hz := range []int{0, -1, 121, 1000} {
		if _, err := NewFanoutRuntime(presence, reg, source, hz); err == nil {
			t.Errorf("tickHz %d accepted", hz)
		}
	}
	r, err := NewFanoutRuntime(presence, reg, source, 20)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	r.Close() // idempotent
}

func TestFanoutMovementSaturation1024(t *testing.T) {
	f := newFanoutFixture(t, 20)
	sid := f.addSession(DefaultOutboundPolicy())
	f.activate(sid, 101, 1001, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 1001, Position: world.Vec3{X: 1}, Yaw: 10, Speed: 35, Kind: 2, Proto: 7})
	// Uncontrolled entity staged OUTSIDE the session's AOI; its
	// movement into a subscribed cell creates visibility.
	f.source.put(EntityPresentation{EntityID: 2001, Position: world.Vec3{X: 500}, Yaw: 20, Speed: 35, Kind: 3, Proto: 8})
	f.bootstrap(sid)
	// Stall the pump inside the presentation source: the first
	// visibility-making movement occupies the pump, the next 1024
	// fill the queue.
	f.source.block = make(chan struct{})
	f.source.entered = make(chan sim.EntityID, 2048)
	f.move(2001, world.Vec3{X: 5}, 20, 35, 10, 0)
	select {
	case <-f.source.entered:
	case <-time.After(10 * time.Second):
		t.Fatalf("pump never reached source")
	}
	for i := 0; i < FanoutEventCapacity; i++ {
		f.move(2001, world.Vec3{X: 5}, 20, 35, uint32(i+11), 0)
	}
	before := f.fanout.DroppedMovementUpdates()
	f.move(2001, world.Vec3{X: 5}, 20, 35, 9999, 0) // 1025th: dropped
	if got := f.fanout.DroppedMovementUpdates(); got != before+1 {
		t.Fatalf("dropped = %d, want %d", got, before+1)
	}
	// The sim caller never blocks: admission already returned above.
	close(f.source.block)
	f.drainPump()
}

func TestFanoutControlCompletionAndClose(t *testing.T) {
	f := newFanoutFixture(t, 20)
	sid := f.addSession(DefaultOutboundPolicy())
	f.activate(sid, 101, 1001, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 1001, Position: world.Vec3{X: 1}, Yaw: 10, Speed: 35, Kind: 2, Proto: 7})
	// Bootstrap completes exactly once through the real pump.
	if err := f.fanout.BootstrapSession(context.Background(), sid); err != nil {
		t.Fatalf("BootstrapSession: %v", err)
	}
	// Queued control waiter + close: waiter receives ErrFanoutClosed.
	// Stall on an uncontrolled visibility-making movement (the pump
	// only calls the source for new 204s): stage it outside the AOI
	// so bootstrap leaves it invisible.
	f.source.put(EntityPresentation{EntityID: 2001, Position: world.Vec3{X: 500}, Yaw: 20, Speed: 35, Kind: 3, Proto: 8})
	f.source.block = make(chan struct{})
	f.source.entered = make(chan sim.EntityID, 2048)
	f.move(2001, world.Vec3{X: 5}, 20, 35, 50, 0)
	select {
	case <-f.source.entered:
	case <-time.After(10 * time.Second):
		t.Fatalf("pump never stalled")
	}
	waiter := make(chan error, 1)
	go func() {
		waiter <- f.fanout.RemovePresence(context.Background(), sid, 1001)
	}()
	// The pump holds the stalled movement; let the control enqueue
	// behind it.
	deadline := time.Now().Add(10 * time.Second)
	for len(f.fanout.events) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("control never queued (len=%d)", len(f.fanout.events))
		}
	}
	// Close runs in the background: it can only finish draining once
	// the test releases the stalled pump below.
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		f.fanout.Close()
	}()
	// Gate the release on Close having actually closed the runtime.
	// The barrier observes close-state WITHOUT publishing another
	// control: under exact-result semantics (v0.3.24) a second
	// synchronously-waited control admitted before Close marks closed
	// would legitimately wait for the drain — which needs this very
	// release — so gating the release on such a waiter deadlocks.
	// The queued waiter below still deterministically completes
	// through the drain path with ErrFanoutClosed.
	admissionClosed := func() bool {
		f.fanout.adm.RLock()
		defer f.fanout.adm.RUnlock()
		return f.fanout.admClosed
	}
	deadline = time.Now().Add(10 * time.Second)
	for !admissionClosed() {
		if time.Now().After(deadline) {
			t.Fatalf("Close never closed admission")
		}
	}
	close(f.source.block)
	select {
	case err := <-waiter:
		if !errors.Is(err, ErrFanoutClosed) {
			t.Fatalf("queued waiter = %v, want ErrFanoutClosed", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("queued waiter hung")
	}
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatalf("Close hung")
	}
	// Future control is rejected; future movement drops.
	if err := f.fanout.BootstrapSession(context.Background(), sid); !errors.Is(err, ErrFanoutClosed) {
		t.Fatalf("post-close bootstrap = %v", err)
	}
	before := f.fanout.DroppedMovementUpdates()
	f.move(1001, world.Vec3{X: 1}, 0, 35, 51, 1)
	if got := f.fanout.DroppedMovementUpdates(); got != before+1 {
		t.Fatalf("post-close drop uncounted")
	}
}

func TestFanoutMovementOrdering(t *testing.T) {
	f := newFanoutFixture(t, 20)
	x := f.addSession(DefaultOutboundPolicy())
	f.activate(x, 101, 1001, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 1001, Position: world.Vec3{X: 1}, Yaw: 10, Speed: 35, Kind: 2, Proto: 7})
	// Uncontrolled entity staged OUTSIDE every AOI; movement B drives
	// it into X's subscribed cell.
	f.source.put(EntityPresentation{EntityID: 2001, Position: world.Vec3{X: 500}, Yaw: 20, Speed: 35, Kind: 3, Proto: 8})
	f.bootstrap(x)
	// Second session with a far-away own entity (outside X's AOI so
	// its reveal notifies nobody).
	y := f.addSession(DefaultOutboundPolicy())
	f.activate(y, 102, 1003, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 1003, Position: world.Vec3{X: 1000}, Yaw: 30, Speed: 0, Kind: 5, Proto: 13})
	// A, B admitted; C bootstrap is the synchronous ordering barrier
	// (its return proves A/B processed); D admitted after.
	f.move(1001, world.Vec3{X: 1}, 100, 35, 1, 11) // A: 205
	f.move(2001, world.Vec3{X: 5}, 200, 35, 3, 0)  // B: 204, no same-event 205
	f.bootstrap(y)                                 // C: Y's 204s
	f.move(1001, world.Vec3{X: 1}, 300, 35, 5, 15) // D: 205
	f.drainPump()
	var got []uint16
	var moves []proto.EntityMove
	for _, c := range f.prods[x].snapshot() {
		got = append(got, c.opcode)
		if c.opcode == proto.OpcodeEntityMove {
			moves = append(moves, decodeMove(t, c.payload))
		}
	}
	// Bootstrap's own 204 precedes; then A-205, B-204, D-205 proves
	// pump order A < B < D with the C barrier between B and D.
	want := []uint16{
		proto.OpcodeEntityCreate,
		proto.OpcodeEntityMove, proto.OpcodeEntityCreate, proto.OpcodeEntityMove,
	}
	if len(got) != len(want) {
		t.Fatalf("X opcodes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("X opcodes = %v, want %v", got, want)
		}
	}
	if len(moves) != 2 || moves[0].Angle != 100 || moves[1].Angle != 300 {
		t.Fatalf("205 order/payload = %+v", moves)
	}
	// Y bootstrapped exactly its visible set in EntityID ascending
	// order (2001 still stages outside every AOI): 1001 first with a
	// fresh handle 2, then own 1003 pinned to handle 1.
	var yCreates []proto.EntityCreate
	for _, c := range f.prods[y].snapshot() {
		if c.opcode == proto.OpcodeEntityCreate {
			yCreates = append(yCreates, decodeCreate(t, c.payload))
		}
	}
	if len(yCreates) != 2 {
		t.Fatalf("Y creates = %+v, want 2", yCreates)
	}
	if yCreates[0].Entity != (proto.EntityEntry{Entity: 2, Kind: 2, Proto: 7, Pos: proto.Position{X: 1000}, Angle: 10, Speed: 35}) {
		t.Fatalf("Y create[0] = %+v", yCreates[0].Entity)
	}
	if yCreates[1].Entity.Entity != 1 || yCreates[1].Entity.Kind != 5 || yCreates[1].Entity.Proto != 13 {
		t.Fatalf("Y create[1] = %+v, want own handle 1", yCreates[1].Entity)
	}
}

func TestFanoutNonReadyGetsNothing(t *testing.T) {
	f := newFanoutFixture(t, 20)
	sid := f.addSession(DefaultOutboundPolicy())
	f.activate(sid, 101, 1001, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 1001, Position: world.Vec3{X: 1}, Yaw: 10, Speed: 35, Kind: 2, Proto: 7})
	// Presence active but bootstrap not run: movement ignored.
	f.move(1001, world.Vec3{X: 2}, 50, 35, 1, 5)
	f.drainPump()
	if n := len(f.prods[sid].snapshot()); n != 0 {
		t.Fatalf("non-ready session received %d frames", n)
	}
	f.bootstrap(sid)
	f.move(1001, world.Vec3{X: 2}, 60, 35, 3, 6)
	f.drainPump()
	var moves int
	for _, c := range f.prods[sid].snapshot() {
		if c.opcode == proto.OpcodeEntityMove {
			moves++
		}
	}
	if moves != 1 {
		t.Fatalf("ready session 205s = %d, want 1", moves)
	}
}

func TestFanoutOwnBootstrapHandle1(t *testing.T) {
	f := newFanoutFixture(t, 20)
	sid := f.addSession(DefaultOutboundPolicy())
	f.activate(sid, 101, 1001, world.CellCoord{})
	// Empty-M10-baseline world: only the own entity exists.
	f.source.put(EntityPresentation{EntityID: 1001, Position: world.Vec3{X: 1, Y: 2, Z: 3}, Yaw: 512, Speed: 70, Kind: 2, Proto: 9})
	f.bootstrap(sid)
	var creates []proto.EntityCreate
	for _, c := range f.prods[sid].snapshot() {
		if c.opcode == proto.OpcodeEntityCreate {
			creates = append(creates, decodeCreate(t, c.payload))
		}
	}
	if len(creates) != 1 {
		t.Fatalf("bootstraps 204s = %d, want exactly own", len(creates))
	}
	got := creates[0].Entity
	if got.Entity != 1 || got.Kind != 2 || got.Proto != 9 {
		t.Fatalf("own 204 = %+v, want entity=1 kind=2 proto=9", got)
	}
	if got.Pos != (proto.Position{X: 1000, Y: 2000, Z: 3000}) {
		t.Fatalf("own 204 pos = %+v", got.Pos)
	}
	if got.Angle != 512 || got.Speed != 70 {
		t.Fatalf("own 204 yaw/speed = %d/%d", got.Angle, got.Speed)
	}
	// 204 used the critical lane, never state.
	for _, c := range f.prods[sid].snapshot() {
		if c.state {
			t.Fatalf("bootstrap used state lane for opcode %d", c.opcode)
		}
	}
}

func TestFanoutBootstrapDeterministicSet(t *testing.T) {
	f := newFanoutFixture(t, 20)
	sid := f.addSession(DefaultOutboundPolicy())
	f.activate(sid, 101, 1001, world.CellCoord{})
	// Entities spread across the AOI: own 1001 at center, 1002/1004
	// inside subscribed cells, 1999 outside (-100 -> cell {-4,0}).
	f.source.put(EntityPresentation{EntityID: 1001, Position: world.Vec3{X: 0}, Yaw: 1, Speed: 0, Kind: 2, Proto: 7})
	f.source.put(EntityPresentation{EntityID: 1004, Position: world.Vec3{X: 90}, Yaw: 2, Speed: 0, Kind: 3, Proto: 8})
	f.source.put(EntityPresentation{EntityID: 1002, Position: world.Vec3{X: -90}, Yaw: 3, Speed: 0, Kind: 3, Proto: 9})
	f.source.put(EntityPresentation{EntityID: 1999, Position: world.Vec3{X: -100}, Yaw: 4, Speed: 0, Kind: 3, Proto: 10})
	f.bootstrap(sid)
	var got []proto.EntityEntry
	for _, c := range f.prods[sid].snapshot() {
		if c.opcode == proto.OpcodeEntityCreate {
			got = append(got, decodeCreate(t, c.payload).Entity)
		}
	}
	// Only subscribed-cell entities, EntityID ascending, deterministic
	// recipient handles: own 1 pinned, then 2, 3 in entity order.
	if len(got) != 3 {
		t.Fatalf("bootstraps = %+v, want 3 (1999 excluded)", got)
	}
	wantHandles := []uint32{1, 2, 3}
	wantProtos := []uint16{7, 9, 8}
	for i := range got {
		if got[i].Entity != wantHandles[i] || got[i].Proto != wantProtos[i] {
			t.Fatalf("bootstraps = %+v", got)
		}
	}
}

func TestFanoutCrossSessionHandles(t *testing.T) {
	f := newFanoutFixture(t, 20)
	a := f.addSession(DefaultOutboundPolicy())
	b := f.addSession(DefaultOutboundPolicy())
	f.activate(a, 101, 1001, world.CellCoord{})
	f.activate(b, 102, 1002, world.CellCoord{})
	// Shared entity 2001 plus decoys so handles diverge: A sees an
	// extra entity first, B sees the shared one first.
	f.source.put(EntityPresentation{EntityID: 1001, Position: world.Vec3{}, Yaw: 1, Speed: 0, Kind: 2, Proto: 7})
	f.source.put(EntityPresentation{EntityID: 1002, Position: world.Vec3{X: 32}, Yaw: 1, Speed: 0, Kind: 2, Proto: 7})
	f.source.put(EntityPresentation{EntityID: 2001, Position: world.Vec3{X: 2}, Yaw: 1, Speed: 0, Kind: 3, Proto: 8})
	f.source.put(EntityPresentation{EntityID: 2002, Position: world.Vec3{X: 3}, Yaw: 1, Speed: 0, Kind: 3, Proto: 8})
	f.bootstrap(a)
	// B bootstraps after hiding 2002 from its AOI is impossible via
	// cells; instead verify per-recipient encoding on movement below.
	f.bootstrap(b)
	ha, _, err := f.presence.VisibleHandle(a, 2001)
	if err != nil {
		t.Fatal(err)
	}
	hb, _, err := f.presence.VisibleHandle(b, 2001)
	if err != nil {
		t.Fatal(err)
	}
	// Move the shared entity: each 205 uses the recipient-local handle.
	f.move(2001, world.Vec3{X: 2}, 77, 35, 10, 0)
	f.drainPump()
	var ma, mb *proto.EntityMove
	for _, c := range f.prods[a].snapshot() {
		if c.opcode == proto.OpcodeEntityMove {
			m := decodeMove(t, c.payload)
			ma = &m
		}
	}
	for _, c := range f.prods[b].snapshot() {
		if c.opcode == proto.OpcodeEntityMove {
			m := decodeMove(t, c.payload)
			mb = &m
		}
	}
	if ma == nil || mb == nil {
		t.Fatalf("missing 205s: a=%v b=%v", ma, mb)
	}
	if ma.Entity != uint32(ha) || mb.Entity != uint32(hb) {
		t.Fatalf("handles a=%d b=%d, want %d/%d", ma.Entity, mb.Entity, ha, hb)
	}
	if ma.LastProcessedInputSeq != 0 || mb.LastProcessedInputSeq != 0 {
		t.Fatalf("observer anchors = %d/%d, want 0/0", ma.LastProcessedInputSeq, mb.LastProcessedInputSeq)
	}
}

func TestFanoutExistingViewerNotification(t *testing.T) {
	f := newFanoutFixture(t, 20)
	a := f.addSession(DefaultOutboundPolicy())
	f.activate(a, 101, 1001, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 1001, Position: world.Vec3{}, Yaw: 1, Speed: 0, Kind: 2, Proto: 7})
	f.bootstrap(a)
	before := len(f.prods[a].snapshot())
	// B enters inside A's AOI with a far-away-unlisted own entity:
	// B's own bootstrap succeeds first, then A learns B.
	b := f.addSession(DefaultOutboundPolicy())
	f.activate(b, 102, 1002, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 1002, Position: world.Vec3{X: 8}, Yaw: 9, Speed: 0, Kind: 2, Proto: 7})
	f.bootstrap(b)
	f.drainPump()
	var aCreates []proto.EntityCreate
	for _, c := range f.prods[a].snapshot()[before:] {
		if c.opcode == proto.OpcodeEntityCreate {
			aCreates = append(aCreates, decodeCreate(t, c.payload))
		}
	}
	if len(aCreates) != 1 {
		t.Fatalf("A post-join creates = %+v, want exactly B", aCreates)
	}
	if aCreates[0].Entity.Proto != 7 || aCreates[0].Entity.Angle != 9 {
		t.Fatalf("A learned wrong entity: %+v", aCreates[0])
	}
	hb, _, _ := f.presence.VisibleHandle(a, 1002)
	if aCreates[0].Entity.Entity != uint32(hb) {
		t.Fatalf("A 204 handle %d != visible %d", aCreates[0].Entity.Entity, hb)
	}
}

func TestFanoutExistingViewerFailureIsolated(t *testing.T) {
	f := newFanoutFixture(t, 20)
	a := f.addSession(DefaultOutboundPolicy())
	f.activate(a, 101, 1001, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 1001, Position: world.Vec3{}, Yaw: 1, Speed: 0, Kind: 2, Proto: 7})
	f.bootstrap(a)
	// Saturate A's critical outbound so its next 204 fails.
	f.trans[a].setWriteErr(errors.New("socket dead"))
	b := f.addSession(DefaultOutboundPolicy())
	f.activate(b, 102, 1002, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 1002, Position: world.Vec3{X: 8}, Yaw: 9, Speed: 0, Kind: 2, Proto: 7})
	if err := f.fanout.BootstrapSession(context.Background(), b); err != nil {
		t.Fatalf("B bootstrap failed with broken A: %v", err)
	}
	// B remains successfully bootstrapped (own 204 present).
	var bCreates int
	for _, c := range f.prods[b].snapshot() {
		if c.opcode == proto.OpcodeEntityCreate {
			bCreates++
		}
	}
	if bCreates != 2 { // own 1002 + nearby 1001
		t.Fatalf("B creates = %d, want 2", bCreates)
	}
	f.trans[a].setWriteErr(nil)
}

func TestFanoutSameCellMovementOnly205(t *testing.T) {
	f := newFanoutFixture(t, 20)
	sid := f.addSession(DefaultOutboundPolicy())
	f.activate(sid, 101, 1001, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 1001, Position: world.Vec3{}, Yaw: 1, Speed: 0, Kind: 2, Proto: 7})
	f.bootstrap(sid)
	base := len(f.prods[sid].snapshot())
	// Same-cell movements: no churn, no 204/206, only throttled 205.
	f.move(1001, world.Vec3{X: 2}, 100, 35, 10, 21)
	f.move(1001, world.Vec3{X: 4}, 200, 35, 12, 22)
	f.drainPump()
	for _, c := range f.prods[sid].snapshot()[base:] {
		if c.opcode != proto.OpcodeEntityMove {
			t.Fatalf("same-cell opcode = %d, want only 205", c.opcode)
		}
	}
	m := decodeMove(t, f.prods[sid].snapshot()[base+1].payload)
	if m.LastProcessedInputSeq != 22 {
		t.Fatalf("owner anchor = %d, want 22", m.LastProcessedInputSeq)
	}
}

func TestFanoutAxialCrossingReconciles(t *testing.T) {
	f := newFanoutFixture(t, 20)
	sid := f.addSession(DefaultOutboundPolicy())
	f.activate(sid, 101, 1001, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 1001, Position: world.Vec3{}, Yaw: 1, Speed: 0, Kind: 2, Proto: 7})
	// Entity living in the exiting strip (cell {-3,0} side): visible
	// before the crossing via bootstrap, removed after.
	f.source.put(EntityPresentation{EntityID: 2001, Position: world.Vec3{X: -70}, Yaw: 1, Speed: 0, Kind: 3, Proto: 8})
	// Entity living in the entering strip (cell {4,0} side).
	f.source.put(EntityPresentation{EntityID: 2002, Position: world.Vec3{X: 140}, Yaw: 1, Speed: 0, Kind: 3, Proto: 8})
	f.bootstrap(sid)
	hOld, _, err := f.presence.VisibleHandle(sid, 2001)
	if err != nil {
		t.Fatal(err)
	}
	base := len(f.prods[sid].snapshot())
	// Owner crosses {0,0} -> {1,0}: 7 entered / 7 exited cells.
	f.move(1001, world.Vec3{X: 33}, 500, 35, 10, 31)
	f.drainPump()
	var ops []uint16
	var removes []proto.EntityRemove
	var creates []proto.EntityCreate
	for _, c := range f.prods[sid].snapshot()[base:] {
		ops = append(ops, c.opcode)
		switch c.opcode {
		case proto.OpcodeEntityRemove:
			removes = append(removes, decodeRemove(t, c.payload))
		case proto.OpcodeEntityCreate:
			creates = append(creates, decodeCreate(t, c.payload))
		}
	}
	if len(removes) != 1 || removes[0].Entity != uint32(hOld) {
		t.Fatalf("removes = %+v, want single 206 with old handle %d", removes, hOld)
	}
	if _, ok := f.presence.ResolveHandle(sid, hOld); ok {
		t.Fatalf("old handle %d still resolves after 206", hOld)
	}
	if len(creates) != 1 || creates[0].Entity.Proto != 8 {
		t.Fatalf("creates = %+v, want entering-strip 2002", creates)
	}
}

func TestFanoutDiagonalCrossingReconciles(t *testing.T) {
	f := newFanoutFixture(t, 20)
	sid := f.addSession(DefaultOutboundPolicy())
	f.activate(sid, 101, 1001, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 1001, Position: world.Vec3{}, Yaw: 1, Speed: 0, Kind: 2, Proto: 7})
	f.source.put(EntityPresentation{EntityID: 2001, Position: world.Vec3{X: -70, Z: -70}, Yaw: 1, Speed: 0, Kind: 3, Proto: 8})
	f.source.put(EntityPresentation{EntityID: 2002, Position: world.Vec3{X: 140, Z: 140}, Yaw: 1, Speed: 0, Kind: 3, Proto: 8})
	f.bootstrap(sid)
	base := len(f.prods[sid].snapshot())
	// {0,0} -> {1,1}: 13 entered / 13 exited cells.
	f.move(1001, world.Vec3{X: 33, Z: 33}, 500, 35, 10, 31)
	f.drainPump()
	var nCreate, nRemove, nMove int
	for _, c := range f.prods[sid].snapshot()[base:] {
		switch c.opcode {
		case proto.OpcodeEntityCreate:
			nCreate++
		case proto.OpcodeEntityRemove:
			nRemove++
		case proto.OpcodeEntityMove:
			nMove++
		}
	}
	if nCreate != 1 || nRemove != 1 {
		t.Fatalf("diagonal churn: creates=%d removes=%d, want 1/1", nCreate, nRemove)
	}
	_ = nMove
}

func TestFanoutMovementIntoAOI(t *testing.T) {
	f := newFanoutFixture(t, 20)
	sid := f.addSession(DefaultOutboundPolicy())
	f.activate(sid, 101, 1001, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 1001, Position: world.Vec3{}, Yaw: 1, Speed: 0, Kind: 2, Proto: 7})
	f.source.put(EntityPresentation{EntityID: 2001, Position: world.Vec3{X: 500}, Yaw: 44, Speed: 35, Kind: 3, Proto: 8})
	f.bootstrap(sid)
	base := len(f.prods[sid].snapshot())
	// 2001 walks from far away into a subscribed cell: 204 with the
	// movement's dynamics, and NO same-event 205.
	f.move(2001, world.Vec3{X: 6}, 44, 35, 10, 0)
	f.drainPump()
	after := f.prods[sid].snapshot()[base:]
	if len(after) != 1 || after[0].opcode != proto.OpcodeEntityCreate {
		t.Fatalf("into-AOI frames = %v, want single 204", opcodesOf(after))
	}
	got := decodeCreate(t, after[0].payload)
	if got.Entity.Angle != 44 || got.Entity.Speed != 35 || got.Entity.Pos.X != 6000 {
		t.Fatalf("204 dynamics = %+v, want movement's yaw/speed/pos", got.Entity)
	}
	if got.Entity.Kind != 3 || got.Entity.Proto != 8 {
		t.Fatalf("204 presentation = %+v", got.Entity)
	}
	// A later eligible update produces the 205.
	f.move(2001, world.Vec3{X: 7}, 45, 35, 12, 0)
	f.drainPump()
	after2 := f.prods[sid].snapshot()[base+1:]
	if len(after2) != 1 || after2[0].opcode != proto.OpcodeEntityMove {
		t.Fatalf("follow-up frames = %v, want single 205", opcodesOf(after2))
	}
}

func TestFanoutVisibilityExitAndReentry(t *testing.T) {
	f := newFanoutFixture(t, 20)
	sid := f.addSession(DefaultOutboundPolicy())
	f.activate(sid, 101, 1001, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 1001, Position: world.Vec3{}, Yaw: 1, Speed: 0, Kind: 2, Proto: 7})
	f.source.put(EntityPresentation{EntityID: 2001, Position: world.Vec3{X: 6}, Yaw: 1, Speed: 0, Kind: 3, Proto: 8})
	f.bootstrap(sid)
	h1, _, err := f.presence.VisibleHandle(sid, 2001)
	if err != nil {
		t.Fatal(err)
	}
	// 2001 leaves every subscribed cell: 206 with the OLD handle,
	// then the mapping retires.
	f.move(2001, world.Vec3{X: 500}, 10, 35, 10, 0)
	f.drainPump()
	var saw206 bool
	for _, c := range f.prods[sid].snapshot() {
		if c.opcode == proto.OpcodeEntityRemove {
			if got := decodeRemove(t, c.payload); got.Entity != uint32(h1) {
				t.Fatalf("206 handle = %d, want old %d", got.Entity, h1)
			}
			saw206 = true
		}
	}
	if !saw206 {
		t.Fatalf("no 206 on visibility exit")
	}
	if _, ok := f.presence.ResolveHandle(sid, h1); ok {
		t.Fatalf("retired handle %d resolves", h1)
	}
	// Re-entry allocates a fresh larger handle with a new 204.
	base := len(f.prods[sid].snapshot())
	f.move(2001, world.Vec3{X: 6}, 11, 35, 12, 0)
	f.drainPump()
	after := f.prods[sid].snapshot()[base:]
	if len(after) != 1 || after[0].opcode != proto.OpcodeEntityCreate {
		t.Fatalf("re-entry frames = %v, want single 204", opcodesOf(after))
	}
	h2 := decodeCreate(t, after[0].payload).Entity.Entity
	if h2 <= uint32(h1) {
		t.Fatalf("re-entry handle %d, want fresh larger than %d", h2, h1)
	}
}

func opcodesOf(calls []recordedProducerCall) []uint16 {
	var out []uint16
	for _, c := range calls {
		out = append(out, c.opcode)
	}
	return out
}

func TestFanoutOwn205Payload(t *testing.T) {
	f := newFanoutFixture(t, 20)
	sid := f.addSession(DefaultOutboundPolicy())
	f.activate(sid, 101, 1001, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 1001, Position: world.Vec3{}, Yaw: 1, Speed: 0, Kind: 2, Proto: 7})
	f.bootstrap(sid)
	f.move(1001, world.Vec3{X: 1.234, Y: 0.5, Z: -7.891}, 4095, 70, 9, 424242)
	f.drainPump()
	var moves []proto.EntityMove
	for _, c := range f.prods[sid].snapshot() {
		if c.opcode == proto.OpcodeEntityMove {
			moves = append(moves, decodeMove(t, c.payload))
		}
	}
	if len(moves) != 1 {
		t.Fatalf("owner 205s = %d", len(moves))
	}
	got := moves[0]
	if got.Entity != 1 {
		t.Fatalf("owner handle = %d, want 1", got.Entity)
	}
	if got.Pos != (proto.Position{X: 1234, Y: 500, Z: -7891}) {
		t.Fatalf("owner pos = %+v", got.Pos)
	}
	if got.Angle != 4095 || got.Speed != 70 {
		t.Fatalf("owner yaw/speed = %d/%d", got.Angle, got.Speed)
	}
	if got.LastProcessedInputSeq != 424242 {
		t.Fatalf("owner anchor = %d, want 424242", got.LastProcessedInputSeq)
	}
}

func TestFanout205StateKeyExact(t *testing.T) {
	f := newFanoutFixture(t, 20)
	sid := f.addSession(DefaultOutboundPolicy())
	f.activate(sid, 101, 1001, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 1001, Position: world.Vec3{}, Yaw: 1, Speed: 0, Kind: 2, Proto: 7})
	// 2001 starts INSIDE the AOI so both sessions establish visibility
	// for it at bootstrap; a later movement then produces per-recipient
	// 205 state keys rather than a create.
	f.source.put(EntityPresentation{EntityID: 2001, Position: world.Vec3{X: 6}, Yaw: 1, Speed: 0, Kind: 3, Proto: 8})
	f.bootstrap(sid)
	f.move(1001, world.Vec3{X: 1}, 10, 35, 5, 5)
	f.drainPump()
	for _, c := range f.prods[sid].snapshot() {
		if c.opcode != proto.OpcodeEntityMove {
			continue
		}
		if !c.state {
			t.Fatalf("205 not on state lane")
		}
		if c.key != (StateKey{Kind: proto.OpcodeEntityMove, ID: 1}) {
			t.Fatalf("205 key = %+v, want {205 1} recipient NetEntityID", c.key)
		}
	}
	// A second session sees the same entity under its own handle and
	// therefore its own coalescing key: A maps 2001 -> 2 (own 1001 is
	// pinned 1), B maps 2001 -> 3 (own 1002 pinned 1, then 1001 -> 2).
	b := f.addSession(DefaultOutboundPolicy())
	f.activate(b, 102, 1002, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 1002, Position: world.Vec3{X: 32}, Yaw: 1, Speed: 0, Kind: 2, Proto: 7})
	f.bootstrap(b)
	ha, aVisible, err := f.presence.VisibleHandle(sid, 2001)
	if err != nil || !aVisible || ha != 2 {
		t.Fatalf("A handle for 2001 = (%d, %v, %v), want 2", ha, aVisible, err)
	}
	hb, bVisible, err := f.presence.VisibleHandle(b, 2001)
	if err != nil || !bVisible || hb != 3 {
		t.Fatalf("B handle for 2001 = (%d, %v, %v), want 3", hb, bVisible, err)
	}
	f.move(2001, world.Vec3{X: 6}, 11, 35, 7, 0)
	f.drainPump()
	seen := map[uint64]bool{}
	for _, c := range f.prods[b].snapshot() {
		if c.opcode == proto.OpcodeEntityMove && c.state {
			seen[c.key.ID] = true
		}
	}
	if !seen[uint64(hb)] || len(seen) != 1 {
		t.Fatalf("B 205 keys = %v, want exactly recipient-local key %d", seen, hb)
	}
}

func TestFanout205CoalescingRealQueue(t *testing.T) {
	f := newFanoutFixture(t, 20)
	sid := f.addSession(DefaultOutboundPolicy())
	f.activate(sid, 101, 1001, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 1001, Position: world.Vec3{}, Yaw: 1, Speed: 0, Kind: 2, Proto: 7})
	f.bootstrap(sid)
	// Park the physical writer: admitted states queue without delivery.
	hold := make(chan struct{})
	f.trans[sid].setHold(hold)
	// Eligible ticks spaced beyond stride so each is a fresh 205.
	f.move(1001, world.Vec3{X: 1}, 100, 35, 10, 1)
	f.move(1001, world.Vec3{X: 2}, 200, 35, 12, 2)
	f.move(1001, world.Vec3{X: 3}, 300, 35, 14, 3)
	f.drainPump()
	if got := len(framesOf(t, f.trans[sid], proto.OpcodeEntityMove)); got != 0 {
		t.Fatalf("205s written while writer parked: %d", got)
	}
	close(hold)
	// Newest-wins: the writer eventually delivers the latest state
	// (which frame went in-flight first is scheduling-dependent; only
	// the FINAL delivered state is invariant).
	deadline := time.Now().Add(10 * time.Second)
	for {
		var last *proto.EntityMove
		for _, raw := range f.trans[sid].recorded() {
			h, dec, err := proto.DecodeFrame(raw)
			if err != nil || h.Opcode != proto.OpcodeEntityMove {
				continue
			}
			m, err := proto.DecodeEntityMove(dec)
			if err != nil {
				t.Fatal(err)
			}
			mm := m
			last = &mm
		}
		if last != nil && last.Angle == 300 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("writer never delivered newest 205 (last=%+v)", last)
		}
	}
}

func TestFanoutStateDropKeepsSession(t *testing.T) {
	policy := DefaultOutboundPolicy()
	policy.MaxMessages = 4
	policy.MaxBytes = 4096
	f := newFanoutFixture(t, 20)
	sid := f.addSession(policy)
	f.activate(sid, 101, 1001, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 1001, Position: world.Vec3{}, Yaw: 1, Speed: 0, Kind: 2, Proto: 7})
	f.bootstrap(sid)
	// White-box barrier over the REAL outbound queue: BootstrapSession
	// proves admission (TryCritical) only, not physical-write
	// completion, so the bootstrap 204 may already have drained by the
	// time bootstrap returns. Never assume its residency.
	oc, ok := f.prods[sid].OutboundProducer.(*outboundConn)
	if !ok {
		t.Fatalf("producer is %T, want *outboundConn", f.prods[sid].OutboundProducer)
	}
	q := oc.q
	waitQueue := func(desc string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timeout waiting for %s", desc)
			}
			time.Sleep(time.Millisecond)
		}
	}
	// Prove bootstrap is fully gone from queue residency before
	// filling the 4-slot budget deterministically.
	waitQueue("bootstrap outbound drained", func() bool {
		q.mu.Lock()
		defer q.mu.Unlock()
		return q.resMsg == 0 && len(q.crit) == 0 && q.order.Len() == 0 && q.writing == nil
	})
	// Park the physical writer, then admit one explicit unrelated
	// critical blocker and wait until it is physically in-flight
	// (parked inside the transport write).
	hold := make(chan struct{})
	f.trans[sid].setHold(hold)
	putCritical := func(entity uint32) {
		t.Helper()
		if err := f.prods[sid].TryCritical(sid, proto.OpcodeEntityCreate, proto.MessageVersion1,
			func(e *proto.Encoder) error {
				proto.EntityCreate{Entity: proto.EntityEntry{Entity: entity}}.Encode(e)
				return nil
			}); err != nil {
			t.Fatalf("fill critical: %v", err)
		}
	}
	putCritical(77)
	waitQueue("blocker in-flight", func() bool {
		q.mu.Lock()
		defer q.mu.Unlock()
		return q.resMsg == 1 && q.writing != nil
	})
	// Exactly three more critical frames: blocker writing (1) +
	// queued criticals (3) = 4/4 resident.
	for i := 0; i < 3; i++ {
		putCritical(78)
	}
	waitQueue("budget 4/4 resident", func() bool {
		q.mu.Lock()
		defer q.mu.Unlock()
		return q.resMsg == 4
	})
	// The next 205 (new state key) cannot fit: StateDropped, and the
	// observer records the shed.
	f.move(1001, world.Vec3{X: 1}, 10, 35, 10, 10)
	f.drainPump()
	drops, _, _ := f.obs[sid].snapshot()
	if len(drops) != 1 {
		t.Fatalf("state drops = %v, want exactly one shed", drops)
	}
	if drops[0] != "saturated" {
		t.Fatalf("state drop reason = %q, want saturated", drops[0])
	}
	// Session alive solely despite the drop: presence intact, handle
	// resolves, mapping retained.
	if _, err := f.presence.Snapshot(sid); err != nil {
		t.Fatalf("presence lost to state drop: %v", err)
	}
	if _, ok := f.presence.ResolveHandle(sid, 1); !ok {
		t.Fatalf("own handle lost to state drop")
	}
	close(hold)
	// Wait for the parked backlog to drain before proving later
	// traffic flows again; otherwise the follow-up 205 races the
	// outbound pump's drain of the 4 held criticals.
	waitQueue("held backlog drained", func() bool {
		q.mu.Lock()
		defer q.mu.Unlock()
		return q.resMsg == 0 && len(q.crit) == 0 && q.order.Len() == 0 && q.writing == nil
	})
	// A later movement may still be delivered.
	f.move(1001, world.Vec3{X: 2}, 11, 35, 12, 12)
	f.drainPump()
	found := false
	for _, c := range f.prods[sid].snapshot() {
		if c.opcode == proto.OpcodeEntityMove && c.state {
			found = true
		}
	}
	if !found {
		t.Fatalf("no later 205 admitted after drop")
	}
}

func TestFanout206CriticalSaturation(t *testing.T) {
	policy := DefaultOutboundPolicy()
	policy.MaxMessages = 3
	policy.MaxBytes = 256
	f := newFanoutFixture(t, 20)
	a := f.addSession(policy)
	b := f.addSession(DefaultOutboundPolicy())
	f.activate(a, 101, 1001, world.CellCoord{})
	f.activate(b, 102, 1002, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 1001, Position: world.Vec3{}, Yaw: 1, Speed: 0, Kind: 2, Proto: 7})
	f.source.put(EntityPresentation{EntityID: 1002, Position: world.Vec3{X: 32}, Yaw: 1, Speed: 0, Kind: 2, Proto: 7})
	f.bootstrap(a)
	f.bootstrap(b)
	h, _, err := f.presence.VisibleHandle(a, 1002)
	if err != nil {
		t.Fatal(err)
	}
	// Saturate A's critical backlog with a held writer, then force a
	// 206 by moving B's entity out of A's AOI.
	hold := make(chan struct{})
	f.trans[a].setHold(hold)
	for i := 0; i < 10; i++ {
		_ = f.prods[a].TryCritical(a, proto.OpcodeEntityCreate, proto.MessageVersion1,
			func(e *proto.Encoder) error {
				proto.EntityCreate{Entity: proto.EntityEntry{Entity: 99}}.Encode(e)
				return nil
			})
	}
	f.move(1002, world.Vec3{X: 5000}, 10, 35, 20, 0)
	f.drainPump()
	close(hold)
	// A failed closed but its stale handle retired anyway.
	if _, ok := f.presence.ResolveHandle(a, h); ok {
		t.Fatalf("A stale handle %d survives failed 206", h)
	}
	// B unaffected: presence intact with its own handle live.
	if _, err := f.presence.Snapshot(b); err != nil {
		t.Fatalf("B presence disturbed: %v", err)
	}
	if _, ok := f.presence.ResolveHandle(b, 1); !ok {
		t.Fatalf("B own handle lost")
	}
}

// ---------------------------------------------------------------------------
// 205 tick throttle (spec §7.4.5)
// ---------------------------------------------------------------------------

// stateMoves decodes every admitted 205 state call in order. Tests use
// Angle as a tick marker (yaw = tick).
func stateMoves(t *testing.T, calls []recordedProducerCall) []proto.EntityMove {
	t.Helper()
	var out []proto.EntityMove
	for _, c := range calls {
		if c.opcode == proto.OpcodeEntityMove && c.state {
			out = append(out, decodeMove(t, c.payload))
		}
	}
	return out
}

// feedTicks drives `ticks` authoritative same-cell movements of sid's
// own entity, one per tick starting at 1.
func feedOwnTicks(f *fanoutFixture, sid session.ID, entity sim.EntityID, ticks uint32) {
	for tick := uint32(1); tick <= ticks; tick++ {
		f.move(entity, world.Vec3{X: float64(tick)}, uint16(tick), 35, tick, tick)
	}
}

func TestFanoutThrottle20Hz(t *testing.T) {
	f := newFanoutFixture(t, 20) // stride 2 -> max 10 Hz
	sid := f.addSession(DefaultOutboundPolicy())
	f.activate(sid, 101, 1001, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 1001, Position: world.Vec3{}, Kind: 2, Proto: 7})
	f.bootstrap(sid)
	base := len(f.prods[sid].snapshot())
	feedOwnTicks(f, sid, 1001, 20)
	f.drainPump()
	moves := stateMoves(t, f.prods[sid].snapshot()[base:])
	if len(moves) != 10 {
		t.Fatalf("20 Hz: %d 205s for 20 ticks, want 10 (stride 2)", len(moves))
	}
	for i, m := range moves {
		if want := uint32(2*i + 1); uint32(m.Angle) != want {
			t.Fatalf("20 Hz 205[%d] at tick %d, want %d (spacing >= 2)", i, m.Angle, want)
		}
	}
}

func TestFanoutThrottle60Hz(t *testing.T) {
	f := newFanoutFixture(t, 60) // stride 6 -> max 10 Hz
	sid := f.addSession(DefaultOutboundPolicy())
	f.activate(sid, 101, 1001, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 1001, Position: world.Vec3{}, Kind: 2, Proto: 7})
	f.bootstrap(sid)
	base := len(f.prods[sid].snapshot())
	feedOwnTicks(f, sid, 1001, 60)
	f.drainPump()
	moves := stateMoves(t, f.prods[sid].snapshot()[base:])
	if len(moves) != 10 {
		t.Fatalf("60 Hz: %d 205s for 60 ticks, want 10 (stride 6)", len(moves))
	}
	for i, m := range moves {
		if want := uint32(6*i + 1); uint32(m.Angle) != want {
			t.Fatalf("60 Hz 205[%d] at tick %d, want %d (spacing >= 6)", i, m.Angle, want)
		}
	}
}

func TestFanoutThrottle7HzSource(t *testing.T) {
	f := newFanoutFixture(t, 7) // stride 1: every update representable
	sid := f.addSession(DefaultOutboundPolicy())
	f.activate(sid, 101, 1001, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 1001, Position: world.Vec3{}, Kind: 2, Proto: 7})
	f.bootstrap(sid)
	base := len(f.prods[sid].snapshot())
	feedOwnTicks(f, sid, 1001, 7)
	f.drainPump()
	moves := stateMoves(t, f.prods[sid].snapshot()[base:])
	if len(moves) != 7 {
		t.Fatalf("7 Hz: %d 205s for 7 ticks, want 7 (stride 1, <= 10 Hz)", len(moves))
	}
	for i, m := range moves {
		if uint32(m.Angle) != uint32(i+1) {
			t.Fatalf("7 Hz 205[%d] at tick %d", i, m.Angle)
		}
	}
}

func TestFanoutThrottleTickWrap(t *testing.T) {
	f := newFanoutFixture(t, 20) // stride 2
	sid := f.addSession(DefaultOutboundPolicy())
	f.activate(sid, 101, 1001, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 1001, Position: world.Vec3{}, Kind: 2, Proto: 7})
	f.bootstrap(sid)
	base := len(f.prods[sid].snapshot())
	// Position.X (exact small meters) is the emission marker; Angle
	// cannot carry these tick values (uint12).
	ticks := []uint32{math.MaxUint32 - 2, math.MaxUint32 - 1, math.MaxUint32, 0, 1, 2}
	for i, tick := range ticks {
		f.move(1001, world.Vec3{X: float64(i + 1)}, 7, 35, tick, tick)
	}
	f.drainPump()
	moves := stateMoves(t, f.prods[sid].snapshot()[base:])
	// Modulo-u32 distance: first, then +2 (MaxUint32), then +2 again
	// (tick 1). Neither a freeze (0 sends) nor a wrap burst (6 sends).
	wantX := []int32{1000, 3000, 5000}
	if len(moves) != len(wantX) {
		t.Fatalf("wrap: %d 205s, want %d: %v", len(moves), len(wantX), moves)
	}
	for i, m := range moves {
		if m.Pos.X != wantX[i] {
			t.Fatalf("wrap 205[%d] marker X = %d, want %d", i, m.Pos.X, wantX[i])
		}
	}
}

func TestFanoutPerViewerThrottleIndependence(t *testing.T) {
	f := newFanoutFixture(t, 20) // stride 2
	a := f.addSession(DefaultOutboundPolicy())
	b := f.addSession(DefaultOutboundPolicy())
	f.activate(a, 101, 1001, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 1001, Position: world.Vec3{}, Kind: 2, Proto: 7})
	f.source.put(EntityPresentation{EntityID: 2001, Position: world.Vec3{X: 6}, Kind: 3, Proto: 8})
	f.bootstrap(a)
	// A's epoch for 2001 emits at tick 1.
	f.move(2001, world.Vec3{X: 6}, 1, 35, 1, 0)
	f.drainPump()
	if got := len(stateMoves(t, f.prods[a].snapshot())); got != 1 {
		t.Fatalf("A first 205 count = %d, want 1", got)
	}
	// B bootstraps later: its own fresh epoch for 2001.
	f.activate(b, 102, 3001, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 3001, Position: world.Vec3{X: -6}, Kind: 2, Proto: 7})
	f.bootstrap(b)
	baseA := len(f.prods[a].snapshot())
	baseB := len(f.prods[b].snapshot())
	// Tick 2 is inside A's stride (suppressed for A) but B's epoch has
	// never emitted: only B gets this 205.
	f.move(2001, world.Vec3{X: 7}, 2, 35, 2, 0)
	f.drainPump()
	if got := len(stateMoves(t, f.prods[a].snapshot()[baseA:])); got != 0 {
		t.Fatalf("A 205 at tick 2 leaked inside stride: %v", stateMoves(t, f.prods[a].snapshot()[baseA:]))
	}
	mb := stateMoves(t, f.prods[b].snapshot()[baseB:])
	if len(mb) != 1 || uint32(mb[0].Angle) != 2 {
		t.Fatalf("B 205s = %+v, want single tick-2 emission (fresh epoch)", mb)
	}
}

// ---------------------------------------------------------------------------
// observer payload / anchor (spec §7.4.5)
// ---------------------------------------------------------------------------

func TestFanoutObserver205PayloadZeroAnchor(t *testing.T) {
	f := newFanoutFixture(t, 20)
	a := f.addSession(DefaultOutboundPolicy())
	b := f.addSession(DefaultOutboundPolicy())
	f.activate(a, 101, 1001, world.CellCoord{})
	f.activate(b, 102, 2001, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 1001, Position: world.Vec3{}, Kind: 2, Proto: 7})
	f.source.put(EntityPresentation{EntityID: 2001, Position: world.Vec3{X: 32}, Kind: 2, Proto: 7})
	f.bootstrap(a)
	f.bootstrap(b)
	hb, _, err := f.presence.VisibleHandle(b, 1001)
	if err != nil {
		t.Fatal(err)
	}
	baseA := len(f.prods[a].snapshot())
	baseB := len(f.prods[b].snapshot())
	f.move(1001, world.Vec3{X: 1.5, Y: -2.5, Z: 3.5}, 777, 60, 10, 99123)
	f.drainPump()
	ma := stateMoves(t, f.prods[a].snapshot()[baseA:])
	if len(ma) != 1 || ma[0].Entity != 1 || ma[0].LastProcessedInputSeq != 99123 {
		t.Fatalf("owner 205 = %+v, want handle 1 anchor 99123", ma)
	}
	mb := stateMoves(t, f.prods[b].snapshot()[baseB:])
	if len(mb) != 1 {
		t.Fatalf("observer 205 count = %d, want 1", len(mb))
	}
	got := mb[0]
	if got.Entity != uint32(hb) {
		t.Fatalf("observer handle = %d, want its own %d", got.Entity, hb)
	}
	if got.Pos != (proto.Position{X: 1500, Y: -2500, Z: 3500}) || got.Angle != 777 || got.Speed != 60 {
		t.Fatalf("observer dynamics = %+v, want same pos/yaw/speed as owner frame", got)
	}
	if got.LastProcessedInputSeq != 0 {
		t.Fatalf("observer anchor = %d, want 0 (never another session's input seq)", got.LastProcessedInputSeq)
	}
}

// ---------------------------------------------------------------------------
// RemovePresence / re-enter epochs (spec §7.4.6)
// ---------------------------------------------------------------------------

func TestFanoutRemovePresenceNormalExit(t *testing.T) {
	f := newFanoutFixture(t, 20)
	a := f.addSession(DefaultOutboundPolicy())
	b := f.addSession(DefaultOutboundPolicy())
	f.activate(a, 101, 1001, world.CellCoord{})
	f.activate(b, 102, 2001, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 1001, Position: world.Vec3{}, Kind: 2, Proto: 7})
	f.source.put(EntityPresentation{EntityID: 2001, Position: world.Vec3{X: 6}, Kind: 2, Proto: 7})
	f.bootstrap(a)
	f.bootstrap(b)
	hb, _, err := f.presence.VisibleHandle(b, 1001)
	if err != nil {
		t.Fatal(err)
	}
	baseA := len(f.prods[a].snapshot())
	baseB := len(f.prods[b].snapshot())
	// A leaves: sim removal already completed upstream; the runtime
	// then runs the reliable fanout removal.
	if err := f.fanout.RemovePresence(context.Background(), a, 1001); err != nil {
		t.Fatalf("RemovePresence: %v", err)
	}
	removes := []uint32{}
	for _, c := range f.prods[b].snapshot()[baseB:] {
		if c.opcode == proto.OpcodeEntityRemove {
			removes = append(removes, decodeRemove(t, c.payload).Entity)
		}
	}
	if len(removes) != 1 || removes[0] != uint32(hb) {
		t.Fatalf("B removes = %v, want single 206 with old handle %d", removes, hb)
	}
	if got := opcodesOf(f.prods[a].snapshot()[baseA:]); len(got) != 0 {
		t.Fatalf("leaving source received %v, want no own 206", got)
	}
	if _, ok := f.presence.ResolveHandle(b, hb); ok {
		t.Fatalf("B old handle for A survives retirement")
	}
	// A is fanout-not-ready: later movement reaches only B.
	baseA2 := len(f.prods[a].snapshot())
	baseB2 := len(f.prods[b].snapshot())
	f.move(2001, world.Vec3{X: 7}, 10, 35, 10, 10)
	f.drainPump()
	if got := opcodesOf(f.prods[a].snapshot()[baseA2:]); len(got) != 0 {
		t.Fatalf("not-ready source still targeted: %v", got)
	}
	if len(stateMoves(t, f.prods[b].snapshot()[baseB2:])) != 1 {
		t.Fatalf("B lost movement after A's exit")
	}
	// Local teardown then deactivates the source presence.
	if _, err := f.presence.Deactivate(a); err != nil {
		t.Fatal(err)
	}
	if viewers := f.presence.Viewers(1001); len(viewers) != 0 {
		t.Fatalf("ghost viewers survive deactivation: %v", viewers)
	}
}

func TestFanoutReenterFreshEpoch(t *testing.T) {
	f := newFanoutFixture(t, 20) // stride 2
	a := f.addSession(DefaultOutboundPolicy())
	b := f.addSession(DefaultOutboundPolicy())
	f.activate(a, 101, 1001, world.CellCoord{})
	f.activate(b, 102, 2001, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 1001, Position: world.Vec3{}, Kind: 2, Proto: 7})
	f.source.put(EntityPresentation{EntityID: 2001, Position: world.Vec3{X: 6}, Kind: 2, Proto: 7})
	f.bootstrap(a)
	f.bootstrap(b)
	hb, _, err := f.presence.VisibleHandle(b, 1001)
	if err != nil {
		t.Fatal(err)
	}
	// Epoch 1 consumes tick 10; tick 11 falls inside the stride.
	f.move(1001, world.Vec3{X: 1}, 10, 35, 10, 10)
	f.move(1001, world.Vec3{X: 1}, 11, 35, 11, 11)
	f.drainPump()
	// Leave + teardown, then the same session re-enters with a FRESH
	// sim EntityID (the sim never reuses IDs).
	if err := f.fanout.RemovePresence(context.Background(), a, 1001); err != nil {
		t.Fatal(err)
	}
	if _, err := f.presence.Deactivate(a); err != nil {
		t.Fatal(err)
	}
	f.source.put(EntityPresentation{EntityID: 1002, Position: world.Vec3{}, Kind: 2, Proto: 7})
	f.activate(a, 101, 1002, world.CellCoord{})
	// Capture BEFORE the re-bootstrap: the observer's fresh 204 comes
	// from the bootstrap's existing-viewer notification itself.
	baseB := len(f.prods[b].snapshot())
	f.bootstrap(a)
	// The fresh epoch emits at tick 11 even though the OLD epoch had
	// tick 11 suppressed by stride against tick 10: no throttle state
	// survived the retirement.
	f.move(1002, world.Vec3{X: 2}, 11, 35, 11, 11)
	f.drainPump()
	after := f.prods[b].snapshot()[baseB:]
	var fresh204 *proto.EntityCreate
	moves := 0
	for _, c := range after {
		switch c.opcode {
		case proto.OpcodeEntityCreate:
			got := decodeCreate(t, c.payload)
			if fresh204 != nil || got.Entity.Entity <= uint32(hb) {
				t.Fatalf("fresh 204 handle = %d, want single new handle > %d", got.Entity.Entity, hb)
			}
			cp := got
			fresh204 = &cp
		case proto.OpcodeEntityMove:
			moves++
		}
	}
	if fresh204 == nil {
		t.Fatalf("observer never learned the re-entered entity")
	}
	if moves != 1 {
		t.Fatalf("fresh-epoch 205 count = %d, want 1 (no stale throttle state)", moves)
	}
	if snap, err := f.presence.Snapshot(a); err != nil || snap.EntityID != 1002 || snap.OwnNetID != 1 {
		t.Fatalf("re-entered presence = (%+v, %v), want fresh entity with pinned handle 1", snap, err)
	}
}

// ---------------------------------------------------------------------------
// full-chain bootstrap failure and takeover with the REAL fanout
// ---------------------------------------------------------------------------

// multiChars hands each account its own single character so several
// sessions can hold concurrent presences through the real handler.
type multiChars struct{ byAccount map[int64]int64 }

func (m *multiChars) List(context.Context, int64) ([]character.ListEntry, error) {
	return nil, errors.New("not used")
}

func (m *multiChars) Create(context.Context, int64, character.CreateRequest) (int64, error) {
	return 0, errors.New("not used")
}

func (m *multiChars) FindBySlot(_ context.Context, accountID int64, slot uint8) (character.Descriptor, error) {
	if slot != 0 {
		return character.Descriptor{}, character.ErrNotFound
	}
	charID, ok := m.byAccount[accountID]
	if !ok {
		return character.Descriptor{}, character.ErrNotFound
	}
	return character.Descriptor{ID: charID, Slot: 0, Name: "Aria", Revision: 3}, nil
}

func (m *multiChars) Delete(context.Context, int64, int64) (int64, error) {
	return 0, errors.New("not used")
}

// addOutSession creates an AUTHENTICATED session on reg backed by a
// REAL outbound queue over a fake transport (admission recorded).
func addOutSession(t *testing.T, reg *session.Registry, accountID int64, sub string, policy OutboundPolicy) (session.ID, *methodRecordingProducer, *fakeOutTransport, *recordingObserver) {
	t.Helper()
	tr := newFakeOutTransport()
	ob := &recordingObserver{}
	oc := newOutboundConn(OutboundDeps{
		Conn: tr, Registry: reg,
		Tick: func() uint32 { return 1000 }, Policy: policy, Observer: ob,
	})
	rec := &methodRecordingProducer{OutboundProducer: oc, t: t}
	sid := reg.Create(rec)
	if err := reg.Authenticate(sid, sub, accountID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	return sid, rec, tr, ob
}

// realFanoutChain wires the real fanout + runtime + enter handler over
// shared registries, mirroring production composition.
type realFanoutChain struct {
	reg        *session.Registry
	presence   *PresenceRegistry
	source     *fakePresentation
	fanout     *FanoutRuntime
	fakeSim    *recordingSim
	downstream *recordingDownstream
	runtime    *WorldSessionRuntime
	enter      *EnterWorldHandler
}

func newRealFanoutChain(t *testing.T, chars CharacterLookup, tickHz int) *realFanoutChain {
	t.Helper()
	reg := session.NewRegistry()
	presence, err := NewPresenceRegistry(testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	source := newFakePresentation()
	fanout, err := NewFanoutRuntime(presence, reg, source, tickHz)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fanout.Close)
	fakeSim := &recordingSim{}
	downstream := &recordingDownstream{}
	spawn := SpawnResolverFunc(func(context.Context, int64, int64) (world.Vec3, error) {
		return world.Vec3{X: 4}, nil
	})
	runtime, err := NewWorldSessionRuntime(fakeSim, presence, reg, spawn,
		func() time.Time { return worldRuntimeBase }, downstream, fanout)
	if err != nil {
		t.Fatal(err)
	}
	enter, err := NewEnterWorldHandler(EnterWorldHandlerDeps{
		Characters: chars,
		Registry:   reg,
		Baseline:   &emptyBaseline{},
		WorldExit:  runtime, // same composition instance (spec §7.3.4)
		WorldEnter: runtime,
		Tick:       func() uint32 { return 1000 },
	})
	if err != nil {
		t.Fatal(err)
	}
	return &realFanoutChain{
		reg: reg, presence: presence, source: source, fanout: fanout,
		fakeSim: fakeSim, downstream: downstream, runtime: runtime, enter: enter,
	}
}

func (c *realFanoutChain) enterWorld(t *testing.T, sid session.ID) error {
	t.Helper()
	sends := &sendRecorder{}
	_, _, err := callHandle(t, c.enter, sid, proto.OpcodeEnterWorld, encodeEnter(t, 0), sends.send)
	return err
}

// B15: a joining session whose critical lane cannot absorb its own
// bootstrap fails closed through the WHOLE enter chain.
func TestFanoutBootstrapSlowRecipientFullRollback(t *testing.T) {
	chars := &multiChars{byAccount: map[int64]int64{11: 500}}
	c := newRealFanoutChain(t, chars, 20)
	policy := DefaultOutboundPolicy()
	policy.MaxMessages = 2
	policy.MaxBytes = 4096
	policy.ReliableEnqueueTimeout = 30 * time.Millisecond
	sid, rec, tr, obs := addOutSession(t, c.reg, 11, "sub-a", policy)
	// Park the physical writer: bootstrap 204s stay resident until the
	// tiny budget is exhausted.
	hold := make(chan struct{})
	tr.setHold(hold)
	// Own entity (recordingSim allocates 1 at the spawn point) plus two
	// neighbors: the third 204 cannot fit.
	c.source.put(EntityPresentation{EntityID: 1, Position: world.Vec3{X: 4}, Kind: 2, Proto: 7})
	c.source.put(EntityPresentation{EntityID: 2001, Position: world.Vec3{X: 6}, Kind: 3, Proto: 8})
	c.source.put(EntityPresentation{EntityID: 2002, Position: world.Vec3{X: 6}, Kind: 3, Proto: 8})
	err := c.enterWorld(t, sid)
	if err == nil {
		t.Fatalf("slow-recipient bootstrap succeeded")
	}
	if cerr, ok := err.(*ClientError); ok {
		t.Fatalf("bootstrap failure mapped to 202 %d, want internal", cerr.Code)
	}
	// Presence rolled back; staged entity removed by the enter
	// rollback; binding rolled back.
	if _, perr := c.presence.Snapshot(sid); !errors.Is(perr, ErrPresenceNotFound) {
		t.Fatalf("presence survives bootstrap failure: %v", perr)
	}
	if len(c.fakeSim.removes) != 1 {
		t.Fatalf("staged entity leaked: removes = %v", c.fakeSim.removes)
	}
	if s, ok := c.reg.Get(sid); !ok || s.State != session.StateAuthenticated || s.HasCharacter {
		t.Fatalf("binding survives bootstrap failure: %+v", s)
	}
	// The dying socket got no compensating 206s.
	for _, call := range rec.snapshot() {
		if call.opcode != proto.OpcodeEntityCreate {
			t.Fatalf("dying socket received opcode %d, want only 204s", call.opcode)
		}
	}
	if tr.closeNowCount() == 0 {
		t.Fatalf("slow source connection never failed closed")
	}
	_, _, drops := obs.snapshot()
	if len(drops) == 0 {
		t.Fatalf("slow-client drop unobserved")
	}
	// Fanout not ready: later movement in its (former) AOI targets it
	// with nothing.
	before := len(rec.snapshot())
	c.fanout.OnMovement(sim.MovementUpdate{
		EntityID: 3001, Position: world.Vec3{X: 6}, Yaw: 1, Speed: 0, Tick: 9,
	})
	if rerr := c.fanout.RemovePresence(context.Background(), 999999, 888888); rerr != nil {
		t.Fatalf("pump barrier: %v", rerr)
	}
	if got := len(rec.snapshot()); got != before {
		t.Fatalf("non-ready session received %d frames after failed bootstrap", got-before)
	}
	close(hold)
}

// B40: duplicate-login takeover through the real fanout — the observer
// sees the old entity leave and the fresh entity arrive.
func TestFanoutTakeoverWithRealFanout(t *testing.T) {
	chars := &multiChars{byAccount: map[int64]int64{11: 500, 12: 501}}
	c := newRealFanoutChain(t, chars, 20)
	policy := DefaultOutboundPolicy()
	oldSid, oldRec, oldTr, _ := addOutSession(t, c.reg, 11, "sub-a", policy)
	obsSid, obsRec, _, _ := addOutSession(t, c.reg, 12, "sub-b", policy)
	// recordingSim allocates 1 (old) and 2 (observer).
	c.source.put(EntityPresentation{EntityID: 1, Position: world.Vec3{X: 4}, Kind: 2, Proto: 7})
	c.source.put(EntityPresentation{EntityID: 2, Position: world.Vec3{X: 6}, Kind: 2, Proto: 7})
	if err := c.enterWorld(t, oldSid); err != nil {
		t.Fatalf("old enter: %v", err)
	}
	if err := c.enterWorld(t, obsSid); err != nil {
		t.Fatalf("observer enter: %v", err)
	}
	hOld, _, err := c.presence.VisibleHandle(obsSid, 1)
	if err != nil {
		t.Fatal(err)
	}
	baseObs := len(obsRec.snapshot())
	frozenOld := len(oldRec.snapshot())
	// The world layer drops the leaving entity's presentation exactly
	// when its sim entity is gone (takeover removes it).
	c.source.del(1)
	// A third same-account session takes over character 500.
	newSid, _, _, _ := addOutSession(t, c.reg, 11, "sub-a2", policy)
	c.source.put(EntityPresentation{EntityID: 3, Position: world.Vec3{X: 4}, Kind: 2, Proto: 7})
	if err := c.enterWorld(t, newSid); err != nil {
		t.Fatalf("takeover enter: %v", err)
	}
	after := obsRec.snapshot()[baseObs:]
	var removes []uint32
	var creates []uint32
	for _, call := range after {
		switch call.opcode {
		case proto.OpcodeEntityRemove:
			removes = append(removes, decodeRemove(t, call.payload).Entity)
		case proto.OpcodeEntityCreate:
			creates = append(creates, decodeCreate(t, call.payload).Entity.Entity)
		}
	}
	if len(removes) != 1 || removes[0] != uint32(hOld) {
		t.Fatalf("observer removes = %v, want single 206 with old handle %d", removes, hOld)
	}
	if len(creates) != 1 || creates[0] <= uint32(hOld) {
		t.Fatalf("observer creates = %v, want single fresh handle > %d", creates, hOld)
	}
	if _, ok := c.presence.ResolveHandle(obsSid, hOld); ok {
		t.Fatalf("old handle survives takeover")
	}
	if len(after) != 2 || after[0].opcode != proto.OpcodeEntityRemove || after[1].opcode != proto.OpcodeEntityCreate {
		t.Fatalf("observer order = %v, want 206 then fresh 204", opcodesOf(after))
	}
	// Old session fully retired: presence gone, registry gone,
	// transport closed, never targeted again.
	if _, perr := c.presence.Snapshot(oldSid); !errors.Is(perr, ErrPresenceNotFound) {
		t.Fatalf("old presence survives takeover: %v", perr)
	}
	if _, ok := c.reg.Get(oldSid); ok {
		t.Fatalf("old registry entry survives takeover")
	}
	if oldTr.closeNowCount() == 0 {
		t.Fatalf("old connection never force-closed")
	}
	// Fresh epoch: new entity 3 with its own presence and the observer
	// mutually visible; the old session receives nothing further.
	snap, err := c.presence.Snapshot(newSid)
	if err != nil || snap.EntityID != 3 || snap.OwnNetID != 1 {
		t.Fatalf("new presence = (%+v, %v), want fresh entity 3 handle 1", snap, err)
	}
	c.fanout.OnMovement(sim.MovementUpdate{
		EntityID: 3, Position: world.Vec3{X: 4.5}, Yaw: 2, Speed: 35, Tick: 10,
	})
	if rerr := c.fanout.RemovePresence(context.Background(), 999999, 888888); rerr != nil {
		t.Fatalf("pump barrier: %v", rerr)
	}
	if got := len(oldRec.snapshot()); got != frozenOld {
		t.Fatalf("retired old session received %d frames after takeover", got-frozenOld)
	}
	foundMove := false
	for _, call := range obsRec.snapshot()[baseObs:] {
		if call.opcode == proto.OpcodeEntityMove {
			foundMove = true
		}
	}
	if !foundMove {
		t.Fatalf("observer never saw the fresh entity move")
	}
}

// ---------------------------------------------------------------------------
// ACK-flow integration through fanout frames (spec §7.1.11)
// ---------------------------------------------------------------------------

// enterWorldRegistry drives a fanout-fixture session to a fully owned
// IN_WORLD binding so its frames consume ACK-flow sequence debt.
func enterWorldRegistry(t *testing.T, f *fanoutFixture, sid session.ID, charID int64) {
	t.Helper()
	if err := f.reg.Authenticate(sid, "sub-"+string(rune('a'+sid-1)), 11, worldRuntimeBase.Add(time.Hour)); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if err := f.reg.BeginEnterWorld(sid, charID); err != nil {
		t.Fatalf("begin enter: %v", err)
	}
	if err := f.reg.CompleteEnterWorld(sid, charID); err != nil {
		t.Fatalf("complete enter: %v", err)
	}
}

// framesOf decodes the physically recorded frames of an opcode, in
// wire order.
func framesOf(t *testing.T, tr *fakeOutTransport, opcode uint16) []proto.Header {
	t.Helper()
	var out []proto.Header
	for _, raw := range tr.recorded() {
		h, _, err := proto.DecodeFrame(raw)
		if err != nil {
			t.Fatalf("decode recorded frame: %v", err)
		}
		if h.Opcode == opcode {
			out = append(out, h)
		}
	}
	return out
}

// B58: 204/205/206 are normal IN_WORLD traffic — they fill the
// max-unacked window and the next write hits the existing ack_lag
// slow-client behavior with no fanout special-casing.
func TestFanoutAckFlowLagThroughFanout(t *testing.T) {
	policy := DefaultOutboundPolicy()
	policy.MaxUnackedMessages = 2
	f := newFanoutFixture(t, 20)
	sid := f.addSession(policy)
	enterWorldRegistry(t, f, sid, 500)
	f.activate(sid, 500, 1001, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 1001, Position: world.Vec3{}, Kind: 2, Proto: 7})
	f.source.put(EntityPresentation{EntityID: 2001, Position: world.Vec3{X: 6}, Kind: 3, Proto: 8})
	f.source.put(EntityPresentation{EntityID: 2002, Position: world.Vec3{X: 6}, Kind: 3, Proto: 8})
	// Bootstrap ADMISSION succeeds (TryCritical resolves at admission);
	// the unacked window violation surfaces at physical write time: the
	// third flow-controlled write fails closed as ack_lag.
	if err := f.fanout.BootstrapSession(context.Background(), sid); err != nil {
		t.Fatalf("bootstrap admission failed early: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, _, drops := f.obs[sid].snapshot()
		found := false
		for _, d := range drops {
			if d == DropReasonAckLag {
				found = true
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			_, _, drops := f.obs[sid].snapshot()
			t.Fatalf("session drop reasons = %v, want ack_lag", drops)
		}
	}
	// Exactly MaxUnacked flow-tracked frames were physically written;
	// the failed write allocated no sequence.
	if got := len(framesOf(t, f.trans[sid], proto.OpcodeEntityCreate)); got != 2 {
		t.Fatalf("written 204s = %d, want exactly the 2-frame window", got)
	}
	// The ack_lag drop stopped the queue: the fanout's next 205 hits a
	// closed state lane, stops targeting the recipient (ready cleared),
	// and nothing further is written.
	callsBefore := len(f.prods[sid].snapshot())
	framesBefore := len(f.trans[sid].recorded())
	f.move(1001, world.Vec3{X: 1}, 1, 35, 10, 10)
	f.move(1001, world.Vec3{X: 2}, 2, 35, 12, 12)
	f.drainPump()
	if got := len(f.prods[sid].snapshot()) - callsBefore; got > 1 {
		t.Fatalf("closed session kept being targeted: %d extra calls", got)
	}
	if got := len(f.trans[sid].recorded()) - framesBefore; got != 0 {
		t.Fatalf("frames written after ack_lag close: %d", got)
	}
}

// B59: coalesced 205s allocate exactly ONE S→C sequence / ACK debt —
// only the physically selected newest state reaches the writer.
func TestFanoutCoalesced205SingleSeqDebt(t *testing.T) {
	f := newFanoutFixture(t, 20)
	sid := f.addSession(DefaultOutboundPolicy())
	enterWorldRegistry(t, f, sid, 500)
	f.activate(sid, 500, 1001, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 1001, Position: world.Vec3{}, Kind: 2, Proto: 7})
	f.bootstrap(sid)
	// The bootstrap 204 resolves at admission; wait for its physical
	// write so the flow baseline is stable.
	writeDeadline := time.Now().Add(10 * time.Second)
	for len(framesOf(t, f.trans[sid], proto.OpcodeEntityCreate)) < 1 {
		if time.Now().After(writeDeadline) {
			t.Fatalf("bootstrap 204 never written")
		}
	}
	before, err := f.reg.FlowState(sid)
	if err != nil {
		t.Fatal(err)
	}
	// Park the writer; three stride-eligible updates coalesce to the
	// newest pending state.
	hold := make(chan struct{})
	f.trans[sid].setHold(hold)
	f.move(1001, world.Vec3{X: 1}, 100, 35, 10, 1)
	f.move(1001, world.Vec3{X: 2}, 200, 35, 12, 2)
	f.move(1001, world.Vec3{X: 3}, 300, 35, 14, 3)
	f.drainPump()
	if got := len(framesOf(t, f.trans[sid], proto.OpcodeEntityMove)); got != 0 {
		t.Fatalf("205s written while writer parked: %d", got)
	}
	close(hold)
	// The FINAL delivered state is the newest (which intermediate went
	// in-flight first is scheduling-dependent); ACK debt always equals
	// the number of physically written frames — never per-admission.
	deadline := time.Now().Add(10 * time.Second)
	for {
		moves := framesOf(t, f.trans[sid], proto.OpcodeEntityMove)
		if len(moves) > 0 {
			raw := f.trans[sid].recorded()
			var last *proto.EntityMove
			for _, r := range raw {
				h, dec, err := proto.DecodeFrame(r)
				if err != nil || h.Opcode != proto.OpcodeEntityMove {
					continue
				}
				m, err := proto.DecodeEntityMove(dec)
				if err != nil {
					t.Fatal(err)
				}
				mm := m
				last = &mm
			}
			if last != nil && last.Angle == 300 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("newest coalesced 205 never written")
		}
	}
	after, err := f.reg.FlowState(sid)
	if err != nil {
		t.Fatal(err)
	}
	physical := len(framesOf(t, f.trans[sid], proto.OpcodeEntityMove))
	if after.LastFlowSent != before.LastFlowSent+uint32(physical) {
		t.Fatalf("flow seq advanced %d -> %d across %d physical frames: debt must equal writes",
			before.LastFlowSent, after.LastFlowSent, physical)
	}
}

// B60: one visibility epoch's wire order is 204 -> 205 -> 206 with
// ascending sequences, no 205 before its 204, and no 205 after the
// 206; a later re-entry starts a fresh epoch with a fresh handle.
func TestFanoutEpochWireOrder204205206(t *testing.T) {
	f := newFanoutFixture(t, 20)
	sid := f.addSession(DefaultOutboundPolicy())
	enterWorldRegistry(t, f, sid, 500)
	f.activate(sid, 500, 1001, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 1001, Position: world.Vec3{}, Kind: 2, Proto: 7})
	f.source.put(EntityPresentation{EntityID: 2001, Position: world.Vec3{X: 500}, Kind: 3, Proto: 8})
	f.bootstrap(sid)
	bootDeadline := time.Now().Add(10 * time.Second)
	for len(f.trans[sid].recorded()) < 1 {
		if time.Now().After(bootDeadline) {
			t.Fatalf("bootstrap frames never written")
		}
	}
	base := len(f.trans[sid].recorded())
	// Epoch: 2001 enters the AOI, moves, then leaves. Each event's
	// frame is waited onto the wire before the next one, so this
	// drained epoch proves the basic 204 -> 205 -> 206 sequence and
	// handle-retirement order. It does NOT prove backlog ordering:
	// frames that pile up behind a stalled writer are reordered by
	// critical-first scheduling, and the queued-205-before-206 case
	// is proven separately by TestFanoutQueued205CanceledBefore206
	// (cancellation) and TestFanoutInFlight205CompletesBefore206
	// (in-flight completion). A temporarily stalled writer that
	// recovers before WriteTimeout is healthy, not slow.
	wantFrame := func(n int, why string) {
		t.Helper()
		for len(f.trans[sid].recorded()) < base+n {
			if time.Now().After(bootDeadline) {
				t.Fatalf("%s never written: %d frames", why, len(f.trans[sid].recorded())-base)
			}
		}
	}
	f.move(2001, world.Vec3{X: 6}, 10, 35, 10, 0) // -> 204 (create)
	f.drainPump()
	wantFrame(1, "204")
	f.move(2001, world.Vec3{X: 7}, 20, 35, 12, 0) // -> 205 (eligible)
	f.drainPump()
	wantFrame(2, "205")
	f.move(2001, world.Vec3{X: 500}, 30, 35, 14, 0) // -> 206 (exit)
	f.drainPump()
	wantFrame(3, "206")
	var ops []uint16
	var seqs []uint32
	var epochHandles []uint32
	for _, raw := range f.trans[sid].recorded()[base:] {
		h, dec, err := proto.DecodeFrame(raw)
		if err != nil {
			t.Fatal(err)
		}
		ops = append(ops, h.Opcode)
		seqs = append(seqs, h.Seq)
		switch h.Opcode {
		case proto.OpcodeEntityCreate:
			m, err := proto.DecodeEntityCreate(dec)
			if err != nil {
				t.Fatal(err)
			}
			epochHandles = append(epochHandles, m.Entity.Entity)
		case proto.OpcodeEntityRemove:
			m, err := proto.DecodeEntityRemove(dec)
			if err != nil {
				t.Fatal(err)
			}
			epochHandles = append(epochHandles, m.Entity)
		}
	}
	if len(ops) != 3 || ops[0] != proto.OpcodeEntityCreate ||
		ops[1] != proto.OpcodeEntityMove || ops[2] != proto.OpcodeEntityRemove {
		t.Fatalf("epoch wire order = %v, want [204 205 206]", ops)
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("wire sequences regressed: %v", seqs)
		}
	}
	// Only the 204/206 frames carry handles (the 205 reuses the 204's).
	h204, h206 := epochHandles[0], epochHandles[len(epochHandles)-1]
	if h204 == 0 || h206 != h204 {
		t.Fatalf("epoch handles = %v, want 206 retiring the 204 handle", epochHandles)
	}
	// No stale 205 after the retirement; re-entry starts a fresh epoch.
	f.move(2001, world.Vec3{X: 500}, 40, 35, 20, 0)
	f.drainPump()
	f.move(2001, world.Vec3{X: 6}, 50, 35, 22, 0)
	f.drainPump()
	wantFrame(4, "re-entry 204")
	tail := f.trans[sid].recorded()[base+3:]
	var tailOps []uint16
	var reentryHandle uint32
	for _, raw := range tail {
		h, dec, err := proto.DecodeFrame(raw)
		if err != nil {
			t.Fatal(err)
		}
		tailOps = append(tailOps, h.Opcode)
		if h.Opcode == proto.OpcodeEntityCreate {
			m, err := proto.DecodeEntityCreate(dec)
			if err != nil {
				t.Fatal(err)
			}
			reentryHandle = m.Entity.Entity
		}
	}
	if len(tailOps) != 1 || tailOps[0] != proto.OpcodeEntityCreate {
		t.Fatalf("post-retirement tail = %v, want a single fresh 204 (no stale 205)", tailOps)
	}
	if reentryHandle <= h204 {
		t.Fatalf("re-entry handle %d, want fresh larger than %d", reentryHandle, h204)
	}
}
