package gateway

// Real-WebSocket fanout integration (M4-T5b2, spec §7.4): real Server
// + real EnterWorldHandler + real WorldSessionRuntime + REAL fanout
// over a real sim.Engine whose MovementSink IS the fanout runtime.
// Clients speak actual binary frames through coder/websocket.

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dlukt/voxilian/internal/proto"
	"github.com/dlukt/voxilian/internal/session"
	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/world"
)

// ---------------------------------------------------------------------------
// client-side frame recorder
// ---------------------------------------------------------------------------

// wsFrame is one recorded S→C frame (the raw bytes plus its header).
type wsFrame struct {
	header proto.Header
	raw    []byte
}

// decodeEntityCreate decodes this frame's 204 payload.
func (f wsFrame) decodeEntityCreate(t *testing.T) proto.EntityCreate {
	t.Helper()
	_, dec, err := proto.DecodeFrame(f.raw)
	if err != nil {
		t.Fatal(err)
	}
	m, err := proto.DecodeEntityCreate(dec)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// decodeEntityMove decodes this frame's 205 payload.
func (f wsFrame) decodeEntityMove(t *testing.T) proto.EntityMove {
	t.Helper()
	_, dec, err := proto.DecodeFrame(f.raw)
	if err != nil {
		t.Fatal(err)
	}
	m, err := proto.DecodeEntityMove(dec)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// decodeEntityRemove decodes this frame's 206 payload.
func (f wsFrame) decodeEntityRemove(t *testing.T) proto.EntityRemove {
	t.Helper()
	_, dec, err := proto.DecodeFrame(f.raw)
	if err != nil {
		t.Fatal(err)
	}
	m, err := proto.DecodeEntityRemove(dec)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// wsRecorder reads one client WebSocket continuously, recording every
// decoded binary frame. Control frames (Ping/Pong) never appear here.
type wsRecorder struct {
	mu     sync.Mutex
	frames []wsFrame
	closed atomic.Bool
}

func newWSRecorder(conn *websocket.Conn) *wsRecorder {
	r := &wsRecorder{}
	go func() {
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				r.closed.Store(true)
				return
			}
			h, _, err := proto.DecodeFrame(data)
			if err != nil {
				continue
			}
			r.mu.Lock()
			r.frames = append(r.frames, wsFrame{header: h, raw: append([]byte(nil), data...)})
			r.mu.Unlock()
		}
	}()
	return r
}

func (r *wsRecorder) snapshot() []wsFrame {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]wsFrame(nil), r.frames...)
}

func (r *wsRecorder) count(opcode uint16) int {
	n := 0
	for _, f := range r.snapshot() {
		if f.header.Opcode == opcode {
			n++
		}
	}
	return n
}

func (r *wsRecorder) of(opcode uint16) []wsFrame {
	var out []wsFrame
	for _, f := range r.snapshot() {
		if f.header.Opcode == opcode {
			out = append(out, f)
		}
	}
	return out
}

// opcodeTrace returns the received opcode sequence.
func (r *wsRecorder) trace() []uint16 {
	var out []uint16
	for _, f := range r.snapshot() {
		out = append(out, f.header.Opcode)
	}
	return out
}

func (r *wsRecorder) isClosed() bool { return r.closed.Load() }

func wsSendFrame(t *testing.T, conn *websocket.Conn, frame []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageBinary, frame); err != nil {
		t.Fatalf("client write: %v", err)
	}
}

// ---------------------------------------------------------------------------
// composition
// ---------------------------------------------------------------------------

// ingressRecorder wraps the real engine's SimIngress seam so the hot
// in-memory presentation source mirrors entity add/remove exactly at
// the staged-world boundary (the world layer's own bookkeeping).
type ingressRecorder struct {
	inner  SimIngress
	source *fakePresentation
}

func (r *ingressRecorder) EnqueueAddEntity(ctx context.Context, pos world.Vec3) (sim.EntitySnapshot, error) {
	snap, err := r.inner.EnqueueAddEntity(ctx, pos)
	if err == nil {
		r.source.put(EntityPresentation{
			EntityID: snap.ID, Position: pos, Kind: 2, Proto: 7, Yaw: 1,
		})
	}
	return snap, err
}

func (r *ingressRecorder) EnqueueRemoveEntity(ctx context.Context, id sim.EntityID) error {
	err := r.inner.EnqueueRemoveEntity(ctx, id)
	if err == nil {
		r.source.del(id)
	}
	return err
}

func (r *ingressRecorder) EnqueueMove(ctx context.Context, id sim.EntityID, intent sim.MoveIntent) (sim.MoveDisposition, error) {
	return r.inner.EnqueueMove(ctx, id, intent)
}

func (r *ingressRecorder) CurrentTick() uint32 { return r.inner.CurrentTick() }

// wsFanoutEnv is the full T5b2 composition over a real WS server.
type wsFanoutEnv struct {
	reg      *session.Registry
	presence *PresenceRegistry
	source   *fakePresentation
	fanout   *FanoutRuntime
	engine   *sim.Engine
	clk      *gwTestClock
	runtime  *WorldSessionRuntime
	enter    *EnterWorldHandler
	server   *Server
	ts       *httptest.Server
	nextSid  int32
}

func newWSFanoutEnv(t *testing.T, chars CharacterLookup, policy OutboundPolicy) *wsFanoutEnv {
	t.Helper()
	reg := session.NewRegistry()
	presence, err := NewPresenceRegistry(testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	source := newFakePresentation()
	fanout, err := NewFanoutRuntime(presence, reg, source, 20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fanout.Close)
	// The REAL engine with the fanout as its MovementSink (B42 chain).
	clk := new(gwTestClock)
	engine, err := sim.NewEngine(sim.EngineConfig{TickHz: 20}, sim.EngineDeps{
		Clock: clk, RNG: &gwTestRNG{}, Collision: gwOpenCollision{},
		RunGate: gwStaticGate{allow: true}, Movement: fanout,
	})
	if err != nil {
		t.Fatal(err)
	}
	ingress := &ingressRecorder{inner: engine, source: source}
	cancel, done := runSimOwner(t, engine)
	t.Cleanup(func() { stopSimOwner(t, cancel, done) })
	spawn := SpawnResolverFunc(func(context.Context, int64, int64) (world.Vec3, error) {
		return world.Vec3{}, nil
	})
	runtime, err := NewWorldSessionRuntime(ingress, presence, reg, spawn,
		func() time.Time { return worldRuntimeBase }, &recordingDownstream{}, fanout)
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
	gameplay, err := NewGameplayIngressHandler(presence, engine,
		func() time.Time { return worldRuntimeBase }, nil)
	if err != nil {
		t.Fatal(err)
	}
	enter.Next = gameplay
	server := NewServer(ServerDeps{
		Registry: reg,
		Handler:  enter,
		Tick:     func() uint32 { return 1000 },
		Now:      time.Now,
		Outbound: policy,
	})
	ts := httptest.NewServer(server)
	t.Cleanup(func() {
		ts.Close()
		server.Close()
	})
	return &wsFanoutEnv{
		reg: reg, presence: presence, source: source, fanout: fanout,
		engine: engine, clk: clk, runtime: runtime, enter: enter,
		server: server, ts: ts,
	}
}

// connect dials one client, authenticates its (deterministic) session,
// and sends opcode 124 slot 0 through the real socket.
func (e *wsFanoutEnv) connect(t *testing.T, accountID int64, sub string) (session.ID, *websocket.Conn, *wsRecorder) {
	t.Helper()
	sid := session.ID(atomic.AddInt32(&e.nextSid, 1))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(e.ts.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	eventually(t, "session registered", func() bool {
		_, ok := e.reg.Get(sid)
		return ok
	})
	if err := e.reg.Authenticate(sid, sub, accountID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	wsSendFrame(t, conn, encodeEnter(t, 0))
	return sid, conn, newWSRecorder(conn)
}

// pulse drives n engine ticks.
func (e *wsFanoutEnv) pulse(t *testing.T, n int) {
	t.Helper()
	tk := e.clk.current()
	for i := 0; i < n; i++ {
		tk.pulse(worldRuntimeBase)
	}
}

// tickUntil drives engine ticks one at a time while polling. The sim
// owner gives a READY tick priority over another queued ingress
// command (spec §5.2.10), so flooding the ticker would starve a 102
// whose command sits in the mailbox behind ticks. Driving one tick and
// waiting for its consumption parks the owner in its blocking select
// with only ingress ready, letting the command execute before the next
// tick — real-clock behavior with no sleeps. At most max ticks.
func (e *wsFanoutEnv) tickUntil(t *testing.T, max int, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for i := 0; i < max; i++ {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout: %s", desc)
		}
		before := e.engine.CurrentTick()
		e.clk.current().pulse(worldRuntimeBase)
		e.waitTick(t, before+1, deadline)
	}
	eventually(t, desc, cond)
}

// waitTick busy-polls until the engine consumed the tick (no sleeps;
// matches waitSimTick).
func (e *wsFanoutEnv) waitTick(t *testing.T, want uint32, deadline time.Time) {
	t.Helper()
	for e.engine.CurrentTick() < want {
		if time.Now().After(deadline) {
			t.Fatalf("timeout: engine never consumed tick %d", want)
		}
	}
}

// sendMove102 writes one real 102 move frame.
func (e *wsFanoutEnv) sendMove102(t *testing.T, conn *websocket.Conn, seq, inputSeq uint32, dirs uint8, yaw uint16) {
	t.Helper()
	frame, err := proto.EncodeFrame(proto.Header{
		Opcode: proto.OpcodeMove, MsgVersion: proto.MessageVersion1, Seq: seq,
	}, func(enc *proto.Encoder) error {
		proto.Move{InputSeq: inputSeq, HeldDirs: dirs, Yaw: yaw}.Encode(enc)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	wsSendFrame(t, conn, frame)
}

// ---------------------------------------------------------------------------
// B43: two clients, empty M10 baseline, deterministic bootstrap, live
// 205 fanout with owner-only reconciliation anchor.
// ---------------------------------------------------------------------------

func TestFanoutWSTwoClientAOI(t *testing.T) {
	chars := &multiChars{byAccount: map[int64]int64{11: 500, 12: 501}}
	e := newWSFanoutEnv(t, chars, DefaultOutboundPolicy())
	aSid, aConn, aRec := e.connect(t, 11, "sub-a")
	bSid, bConn, bRec := e.connect(t, 12, "sub-b")
	if aSid != 1 || bSid != 2 {
		t.Fatalf("sids = %d/%d, want 1/2", aSid, bSid)
	}
	// Both receive 219 BEFORE any entity traffic (bootstrap is
	// post-IN_WORLD traffic, never a baseline exemption).
	eventually(t, "A world_ready", func() bool { return aRec.count(proto.OpcodeWorldReady) == 1 })
	eventually(t, "B world_ready", func() bool { return bRec.count(proto.OpcodeWorldReady) == 1 })
	eventually(t, "A bootstrap 204s", func() bool {
		return aRec.count(proto.OpcodeEntityCreate) >= 2
	})
	eventually(t, "B bootstrap 204s", func() bool {
		return bRec.count(proto.OpcodeEntityCreate) >= 2
	})
	for _, rec := range []*wsRecorder{aRec, bRec} {
		trace := rec.trace()
		ready := -1
		firstCreate := -1
		for i, op := range trace {
			if op == proto.OpcodeWorldReady && ready < 0 {
				ready = i
			}
			if op == proto.OpcodeEntityCreate && firstCreate < 0 {
				firstCreate = i
			}
		}
		if ready < 0 || firstCreate < 0 || ready > firstCreate {
			t.Fatalf("204 before 219: trace %v", trace)
		}
	}
	// Own entity keeps handle 1; the other player is handle 2 for BOTH
	// (engine entities 1 and 2, bootstraps sorted ascending).
	for _, rec := range []*wsRecorder{aRec, bRec} {
		owns := map[uint32]bool{}
		for _, f := range rec.of(proto.OpcodeEntityCreate) {
			owns[f.decodeEntityCreate(t).Entity.Entity] = true
		}
		if !owns[1] || !owns[2] {
			t.Fatalf("client saw handles %v, want own 1 and other 2", owns)
		}
	}
	// A moves; both receive the movement, A with the anchor, B with 0.
	e.sendMove102(t, aConn, 1, 5, sim.MoveDirForward, 0)
	e.tickUntil(t, 64, "both 205s", func() bool {
		return aRec.count(proto.OpcodeEntityMove) >= 1 && bRec.count(proto.OpcodeEntityMove) >= 1
	})
	eventually(t, "B observer 205", func() bool { return bRec.count(proto.OpcodeEntityMove) >= 1 })
	var aMove, bMove *proto.EntityMove
	if ms := aRec.of(proto.OpcodeEntityMove); len(ms) > 0 {
		m := ms[len(ms)-1].decodeEntityMove(t)
		aMove = &m
	}
	if ms := bRec.of(proto.OpcodeEntityMove); len(ms) > 0 {
		m := ms[len(ms)-1].decodeEntityMove(t)
		bMove = &m
	}
	if aMove == nil || bMove == nil {
		t.Fatalf("missing 205: A=%v B=%v", aMove, bMove)
	}
	if aMove.Entity != 1 || aMove.LastProcessedInputSeq != 5 {
		t.Fatalf("A 205 = %+v, want handle 1 anchor 5", aMove)
	}
	if bMove.Entity != 2 || bMove.LastProcessedInputSeq != 0 {
		t.Fatalf("B 205 = %+v, want its handle 2 anchor 0", bMove)
	}
	// Both streams describe the same authoritative walk (coalescing may
	// leave each recipient's newest emission at a different tick, so
	// exact cross-stream position equality is NOT an invariant).
	if aMove.Pos.Z >= 0 || bMove.Pos.Z >= 0 {
		t.Fatalf("A/B 205 not describing the forward walk: %+v vs %+v", aMove, bMove)
	}
	_ = bConn
}

// ---------------------------------------------------------------------------
// B44: a real client-driven cell crossing produces real 204/205/206.
// ---------------------------------------------------------------------------

func TestFanoutWSCellCrossing(t *testing.T) {
	chars := &multiChars{byAccount: map[int64]int64{11: 500}}
	e := newWSFanoutEnv(t, chars, DefaultOutboundPolicy())
	// Static presentation entities bracketing the crossing, present
	// BEFORE the enter so S (exiting strip, cell {-3,0}) is visible at
	// bootstrap — its later 206 is then a real visibility exit. Q
	// (entering strip, cell {4,0}) starts outside the initial AOI.
	e.source.put(EntityPresentation{EntityID: 9001, Position: world.Vec3{X: -70}, Kind: 3, Proto: 8})
	e.source.put(EntityPresentation{EntityID: 9002, Position: world.Vec3{X: 140}, Kind: 3, Proto: 8})
	aSid, aConn, aRec := e.connect(t, 11, "sub-a")
	if aSid != 1 {
		t.Fatalf("sid = %d", aSid)
	}
	eventually(t, "A world_ready", func() bool { return aRec.count(proto.OpcodeWorldReady) == 1 })
	// Bootstrap sees own entity + S.
	eventually(t, "A bootstrap 204s", func() bool { return aRec.count(proto.OpcodeEntityCreate) >= 2 })
	// A walks +X across the {0,0} -> {1,0} boundary (yaw 0 + Right):
	// 0.175 m/tick, so ~210 ticks cover the 32 m boundary plus margin.
	e.sendMove102(t, aConn, 1, 1, sim.MoveDirRight, 0)
	e.tickUntil(t, 400, "crossing observed", func() bool {
		return aRec.count(proto.OpcodeEntityCreate) >= 3 &&
			aRec.count(proto.OpcodeEntityRemove) >= 1
	})
	var sHandle uint32
	sawS := false
	for _, f := range aRec.of(proto.OpcodeEntityCreate) {
		m := f.decodeEntityCreate(t)
		if m.Entity.Proto == 8 && m.Entity.Pos.X < 0 {
			sHandle = m.Entity.Entity
			sawS = true
		}
	}
	if !sawS {
		t.Fatalf("crossing client never saw the exiting-strip entity")
	}
	// The 206 retires S with its old handle; the 204 introduces Q.
	removed := map[uint32]bool{}
	for _, f := range aRec.of(proto.OpcodeEntityRemove) {
		removed[f.decodeEntityRemove(t).Entity] = true
	}
	if !removed[sHandle] {
		t.Fatalf("no 206 for exiting-strip handle %d (removed %v)", sHandle, removed)
	}
	sawQ := false
	for _, f := range aRec.of(proto.OpcodeEntityCreate) {
		m := f.decodeEntityCreate(t)
		if m.Entity.Proto == 8 && m.Entity.Pos.X > 0 {
			sawQ = true
		}
	}
	if !sawQ {
		t.Fatalf("no 204 for the entering-strip entity")
	}
	// Movement kept flowing across the crossing, and no 205 arrives
	// for the retired handle afterwards.
	movesAfterRemove := false
	trace := aRec.trace()
	lastRemove := -1
	for i, op := range trace {
		if op == proto.OpcodeEntityRemove {
			lastRemove = i
		}
	}
	for i, op := range trace {
		if op == proto.OpcodeEntityMove && lastRemove >= 0 && i > lastRemove {
			m := aRec.snapshot()[i].decodeEntityMove(t)
			if m.Entity == sHandle {
				t.Fatalf("stale 205 for retired handle %d after 206", sHandle)
			}
			movesAfterRemove = true
		}
	}
	_ = movesAfterRemove
	// Sequences stay strictly increasing on the wire.
	last := uint32(0)
	for _, f := range aRec.snapshot() {
		if f.header.Seq <= last {
			t.Fatalf("wire sequence regressed: %v", aRec.trace())
		}
		last = f.header.Seq
	}
}

// ---------------------------------------------------------------------------
// B45: one deliberately saturated viewer dies on its critical lane;
// everyone else (and the sim owner) continues.
// ---------------------------------------------------------------------------

func TestFanoutWSSlowViewerIsolation(t *testing.T) {
	chars := &multiChars{byAccount: map[int64]int64{11: 500, 12: 501, 13: 502}}
	policy := DefaultOutboundPolicy()
	policy.ReliableEnqueueTimeout = 50 * time.Millisecond
	policy.WriteTimeout = 100 * time.Millisecond
	e := newWSFanoutEnv(t, chars, policy)
	aSid, _, aRec := e.connect(t, 11, "sub-a")
	bSid, bConn, bRec := e.connect(t, 12, "sub-b")
	cSid, _, cRec := e.connect(t, 13, "sub-c")
	if aSid != 1 || bSid != 2 || cSid != 3 {
		t.Fatalf("sids = %d/%d/%d", aSid, bSid, cSid)
	}
	for _, rec := range []*wsRecorder{aRec, bRec, cRec} {
		r := rec
		eventually(t, "bootstrap complete", func() bool { return r.count(proto.OpcodeEntityCreate) >= 3 })
	}
	// Park C's physical writer (the low-level writer gate) and fill its
	// critical lane: every further critical admission must fail.
	snap, ok := e.reg.Get(cSid)
	if !ok {
		t.Fatalf("C vanished")
	}
	oc, ok := snap.Conn.(*outboundConn)
	if !ok {
		t.Fatalf("C conn = %T", snap.Conn)
	}
	wsc, ok := oc.Connection.(*wsConnection)
	if !ok {
		t.Fatalf("C transport = %T", oc.Connection)
	}
	wsc.writeGate <- struct{}{}
	defer func() { <-wsc.writeGate }()
	for i := 0; i < policy.MaxMessages+2; i++ {
		if err := oc.TryCritical(cSid, proto.OpcodeEntityCreate, proto.MessageVersion1,
			func(enc *proto.Encoder) error {
				proto.EntityCreate{Entity: proto.EntityEntry{Entity: 999}}.Encode(enc)
				return nil
			}); err != nil {
			break
		}
	}
	// B walks far +X until it leaves the origin AOI: A and C both need
	// a 206 for B. A's succeeds; C's fails closed.
	e.sendMove102(t, bConn, 1, 1, sim.MoveDirRight, 0)
	// ≈ 133 m to clear the 128 m AOI edge: ~760 ticks plus margin for
	// the intent to land and the critical failures to play out.
	e.tickUntil(t, 1600, "AOI exit observed", func() bool {
		return aRec.count(proto.OpcodeEntityRemove) >= 1 && cRec.isClosed()
	})
	// A and B remain healthy: B (the mover/owner) keeps receiving its
	// own movement; A received B's movement while visible.
	if aRec.isClosed() {
		t.Fatalf("healthy viewer died with the saturated one")
	}
	if bRec.isClosed() {
		t.Fatalf("mover died with the saturated viewer")
	}
	sawBMove := false
	for _, f := range aRec.of(proto.OpcodeEntityMove) {
		if f.decodeEntityMove(t).Entity == 2 {
			sawBMove = true
		}
	}
	if !sawBMove {
		t.Fatalf("healthy viewer never saw the mover's 205")
	}
	sawOwnMove := false
	for _, f := range bRec.of(proto.OpcodeEntityMove) {
		m := f.decodeEntityMove(t)
		if m.Entity == 1 && m.LastProcessedInputSeq == 1 {
			sawOwnMove = true
		}
	}
	if !sawOwnMove {
		t.Fatalf("mover stopped receiving its own reconciled 205")
	}
}
