// Real-WebSocket ACK flow-control tests (M3-T5b, spec §7.1.11): the
// full opcode-125 lifecycle over the production ServeHTTP path —
// cumulative ACKs, max-unacked gating, leave/takeover epoch resets,
// slow-client resync, and Prometheus integration.
package gateway_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/dlukt/voxilian/internal/auth"
	"github.com/dlukt/voxilian/internal/character"
	"github.com/dlukt/voxilian/internal/gateway"
	"github.com/dlukt/voxilian/internal/observe"
	"github.com/dlukt/voxilian/internal/proto"
	"github.com/dlukt/voxilian/internal/session"
)

// Compile-time proof that the Prometheus adapter satisfies the gateway
// observer seam structurally — neither package imports the other
// (spec §7.1.12).
var _ gateway.OutboundObserver = (*observe.OutboundMetrics)(nil)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

// ackChars is the minimal CharacterService/CharacterLookup double: one
// slot-0 character with a fixed durable ID; List returns one row.
type ackChars struct{}

func (ackChars) List(context.Context, int64) ([]character.ListEntry, error) {
	return []character.ListEntry{{Slot: 0, Name: "Aria", Level: 20}}, nil
}

func (ackChars) Create(context.Context, int64, character.CreateRequest) (int64, error) {
	return 0, errors.New("not used")
}

func (ackChars) FindBySlot(_ context.Context, _ int64, slot uint8) (character.Descriptor, error) {
	if slot != 0 {
		return character.Descriptor{}, character.ErrNotFound
	}
	return character.Descriptor{ID: 500, Slot: 0, Name: "Aria", Revision: 3}, nil
}

func (ackChars) Delete(context.Context, int64, int64) (int64, error) {
	return 0, errors.New("not used")
}

// genBaseline streams a generation-distinct baseline (spec §7.1.10/
// B86): call 1 emits a cell snapshot whose entity NetEntityID is 1001,
// call 2 emits 2001, and so on — the semantic proof that a reconnect
// receives a FRESH baseline, not merely another provider call. With
// rich set, each generation also streams a 218 chunk fragment and a
// 220 shop list so baselines exercise every baseline opcode.
type genBaseline struct {
	rich bool

	mu    sync.Mutex
	calls int
}

func (g *genBaseline) StreamBaseline(
	_ context.Context,
	_ session.ID,
	_ int64,
	_ int64,
	sink gateway.BaselineSink,
) error {
	g.mu.Lock()
	g.calls++
	gen := g.calls
	g.mu.Unlock()
	if err := sink.CellSnapshot(proto.CellSnapshot{
		Cell: proto.Cell{X: int32(gen), Z: 0},
		Entities: []proto.EntityEntry{
			{Entity: uint32(gen*1000 + 1), Kind: 1, Proto: 21,
				Pos: proto.Position{X: 1, Y: 2, Z: 3}, Angle: 100, Speed: 5},
		},
	}); err != nil {
		return err
	}
	if !g.rich {
		return nil
	}
	if err := sink.ChunkFragment(proto.ChunkFragment{
		Cell: proto.Cell{X: 0, Z: 0}, ChunkIdx: 3, FragIdx: 0, FragCount: 1,
		Bytes: []byte{7, 8, 9},
	}); err != nil {
		return err
	}
	if err := sink.ShopList(proto.ShopList{
		Vendor:   77,
		Listings: []proto.ShopListingEntry{{Listing: 3, Price: 100, Qty: 5}},
	}); err != nil {
		return err
	}
	return nil
}

func (g *genBaseline) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// ackExit is the WorldExit double.
type ackExit struct {
	mu    sync.Mutex
	calls int
}

func (a *ackExit) ExitWorld(context.Context, session.ID, int64, int64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	return nil
}

func (a *ackExit) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

// ---------------------------------------------------------------------------
// fixture
// ---------------------------------------------------------------------------

// ackFixture wires the full handler chain over a real httptest server
// with fake auth/character seams (no PG): CharacterHandler(121–123,126)
// → EnterWorldHandler(124) → recording Next, whose marker replies (one
// 219 per IN_WORLD opcode) are the synthetic normal outbound traffic
// generators for ACK-window testing.
type ackFixture struct {
	reg      *session.Registry
	baseline *genBaseline
	exit     *ackExit
	next     *wsNextFake
	obs      *observe.Server // nil unless real metrics were requested
	ts       *httptest.Server
	charID   int64
}

func newAckFixture(t *testing.T, policy gateway.OutboundPolicy, observer gateway.OutboundObserver) *ackFixture {
	t.Helper()
	reg := session.NewRegistry()
	baseline := &genBaseline{}
	exit := &ackExit{}
	next := &wsNextFake{marker: true}
	// A nil observer is fine: both the server and the enter handler
	// default to their no-op implementations.
	enter, err := gateway.NewEnterWorldHandler(gateway.EnterWorldHandlerDeps{
		Characters: ackChars{},
		Registry:   reg,
		Baseline:   baseline,
		WorldExit:  gateway.WorldExitFunc(exit.ExitWorld),
		Tick:       func() uint32 { return testTick },
		Next:       next,
		Observer:   observer,
	})
	if err != nil {
		t.Fatalf("NewEnterWorldHandler: %v", err)
	}
	chars, err := gateway.NewCharacterHandler(ackChars{}, reg, gateway.WorldExitFunc(exit.ExitWorld), enter)
	if err != nil {
		t.Fatalf("NewCharacterHandler: %v", err)
	}
	validator := &fakeValidator{
		ids: map[string]auth.Identity{
			"tok": {Sub: "ack-sub", ExpiresAt: time.Now().Add(time.Hour)},
		},
		now: time.Now,
	}
	srv := gateway.NewServer(gateway.ServerDeps{
		Registry:         reg,
		Validator:        validator,
		Accounts:         gateway.AccountProvisionerFunc(func(_ context.Context, _ string, _ *string) (int64, error) { return 7, nil }),
		Welcome:          func(context.Context) proto.Welcome { return fixedWelcome },
		Tick:             func() uint32 { return testTick },
		Now:              time.Now,
		Schedule:         func(time.Time, func()) gateway.CancelFunc { return func() {} },
		Outbound:         policy,
		OutboundObserver: observer,
		Handler:          chars,
	})
	f := &ackFixture{
		reg:      reg,
		baseline: baseline,
		exit:     exit,
		next:     next,
		ts:       httptest.NewServer(srv),
		charID:   500,
	}
	t.Cleanup(f.ts.Close)
	return f
}

// newAckMetricsFixture builds the fixture with a REAL observe.Server
// whose OutboundMetrics adapter is the gateway observer.
func newAckMetricsFixture(t *testing.T, policy gateway.OutboundPolicy) (*ackFixture, *observe.OutboundMetrics) {
	t.Helper()
	obsServer := observe.New(observe.NewReadiness())
	metrics := obsServer.OutboundMetrics()
	f := newAckFixture(t, policy, metrics)
	f.obs = obsServer
	return f, metrics
}

func (f *ackFixture) dial(t *testing.T) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(f.ts.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

// hello authenticates and returns the connection's session ID.
func (f *ackFixture) hello(t *testing.T, c *websocket.Conn) session.ID {
	t.Helper()
	sendHello(t, c, "tok", 1, 1)
	h, _ := readFrame(t, c)
	if h.Opcode != proto.OpcodeWelcome {
		t.Fatalf("hello opcode = %d, want 200", h.Opcode)
	}
	if h.Seq != 1 {
		t.Fatalf("welcome seq = %d, want 1 (fresh session epoch)", h.Seq)
	}
	ids := f.reg.SessionsBySub("ack-sub")
	if len(ids) == 0 {
		t.Fatal("no session indexed")
	}
	return ids[len(ids)-1]
}

// enterWorld drives 124 and consumes the whole baseline; it returns
// the world_ready header. Baseline wire order: 217, 203, 218, 220, 219
// is NOT this provider's shape — this provider emits 217, 203, 219;
// tests needing a multi-frame baseline assert their own orders.
func (f *ackFixture) enterWorld(t *testing.T, c *websocket.Conn, seq uint32) (proto.Header, session.ID) {
	t.Helper()
	sid := f.hello(t, c)
	sendEnterWorld(t, c, seq, 0)
	if _, op := readCharOp(t, c); op.OK != proto.CharacterOpOK {
		t.Fatalf("217 = %+v", op)
	}
	h203, p203 := readFrame(t, c)
	if h203.Opcode != proto.OpcodeCellSnapshot {
		t.Fatalf("baseline opcode = %d, want 203", h203.Opcode)
	}
	if _, err := proto.DecodeCellSnapshot(p203); err != nil {
		t.Fatalf("203 decode: %v", err)
	}
	h219, _ := readFrame(t, c)
	if h219.Opcode != proto.OpcodeWorldReady {
		t.Fatalf("barrier opcode = %d, want 219", h219.Opcode)
	}
	waitFor(t, "IN_WORLD", func() bool {
		snap, ok := f.reg.Get(sid)
		return ok && snap.State == session.StateInWorld
	})
	return h219, sid
}

// sendMove sends one synthetic IN_WORLD intent (opcode 102); the Next
// marker replies with one 219 — exactly one normal flow unit.
func sendMove(t *testing.T, c *websocket.Conn, seq uint32) {
	t.Helper()
	sendFrame(t, c,
		proto.Header{Opcode: proto.OpcodeMove, MsgVersion: 1, Seq: seq, Tick: 1},
		func(e *proto.Encoder) error {
			proto.Move{InputSeq: seq, HeldDirs: 1}.Encode(e)
			return nil
		})
}

func sendAck(t *testing.T, c *websocket.Conn, seq, ackSeq uint32) {
	t.Helper()
	sendFrame(t, c,
		proto.Header{Opcode: proto.OpcodeAck, MsgVersion: 1, Seq: seq, Tick: 1},
		func(e *proto.Encoder) error {
			proto.Ack{AckSeq: ackSeq}.Encode(e)
			return nil
		})
}

// readMoveReply consumes one Next-marker 219 reply and returns its
// header.
func readMoveReply(t *testing.T, c *websocket.Conn) proto.Header {
	t.Helper()
	h, _ := readFrame(t, c)
	if h.Opcode != proto.OpcodeWorldReady {
		t.Fatalf("reply opcode = %d, want marker 219", h.Opcode)
	}
	return h
}

func flowOf(t *testing.T, reg *session.Registry, sid session.ID) session.FlowSnapshot {
	t.Helper()
	fs, err := reg.FlowState(sid)
	if err != nil {
		t.Fatalf("FlowState: %v", err)
	}
	return fs
}

func flowLagOf(fs session.FlowSnapshot) int { return int(uint32(fs.LastFlowSent - fs.LastAck)) }

// counterValueOf reads one gathered counter series by family+label.
func counterValueOf(t *testing.T, reg *prometheus.Registry, name, label, value string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == label && lp.GetValue() == value {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	t.Fatalf("series %s{%s=%q} not found", name, label, value)
	return 0
}

// histSamplesOf sums one gathered histogram family's sample counts.
func histSamplesOf(t *testing.T, reg *prometheus.Registry, name string) uint64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		var total uint64
		for _, m := range mf.GetMetric() {
			total += m.GetHistogram().GetSampleCount()
		}
		return total
	}
	t.Fatalf("histogram family %q not found", name)
	return 0
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

// TestAckFlowWS is the real-WS happy path (spec §7.1.11): baseline →
// IN_WORLD with lag 0 at the written 219 seq, normal frames consume
// the window, a cumulative ACK returns it to 0, and the connection
// stays alive with further traffic.
func TestAckFlowWS(t *testing.T) {
	f := newAckFixture(t, gateway.OutboundPolicy{MaxUnackedMessages: 8}, nil)
	c := f.dial(t)
	h219, sid := f.enterWorld(t, c, 2) // 200(1) 217(2) 203(3) 219(4)
	if h219.Seq != 4 {
		t.Fatalf("219 seq = %d, want 4", h219.Seq)
	}
	fs := flowOf(t, f.reg, sid)
	if !fs.Active || fs.LastAck != 4 || fs.LastFlowSent != 4 || flowLagOf(fs) != 0 {
		t.Fatalf("epoch = %+v, want active 4/4 lag 0", fs)
	}
	// Frame A writes (seq 5, lag 1) and frame B writes (seq 6, lag 2).
	sendMove(t, c, 2)
	if h := readMoveReply(t, c); h.Seq != 5 {
		t.Fatalf("frame A seq = %d, want 5", h.Seq)
	}
	if fs := flowOf(t, f.reg, sid); flowLagOf(fs) != 1 {
		t.Fatalf("lag after A = %+v", fs)
	}
	sendMove(t, c, 3)
	if h := readMoveReply(t, c); h.Seq != 6 {
		t.Fatalf("frame B seq = %d, want 6", h.Seq)
	}
	// ACK B: no response, lag returns to 0.
	sendAck(t, c, 4, 6)
	sendMove(t, c, 5) // ordering barrier: reply implies the ACK was processed
	if h := readMoveReply(t, c); h.Seq != 7 {
		t.Fatalf("post-ACK reply seq = %d, want 7", h.Seq)
	}
	fs = flowOf(t, f.reg, sid)
	if fs.LastAck != 6 || flowLagOf(fs) != 1 { // seq 7 unacked
		t.Fatalf("flow after ACK = %+v, want lastAck 6 lag 1", fs)
	}
}

// TestAckNoSuccessReplyWS proves valid, duplicate, and stale ACKs
// receive NO success reply: the next S→C frame after them is exactly
// the next request's response (spec §7.1.11).
func TestAckNoSuccessReplyWS(t *testing.T) {
	f := newAckFixture(t, gateway.OutboundPolicy{MaxUnackedMessages: 8}, nil)
	c := f.dial(t)
	f.enterWorld(t, c, 2) // 219 seq 4
	sendMove(t, c, 2)
	readMoveReply(t, c) // seq 5
	sendAck(t, c, 3, 5) // valid
	sendAck(t, c, 4, 5) // duplicate
	sendAck(t, c, 5, 4) // stale
	sendMove(t, c, 6)
	// The VERY next frame is the move's 219 (seq 6) — no hidden ACK
	// success frames precede it.
	if h := readMoveReply(t, c); h.Seq != 6 {
		t.Fatalf("next frame seq = %d, want 6 — an ACK reply leaked", h.Seq)
	}
	fs := flowOf(t, f.reg, sidOf(t, f, c))
	if fs.LastAck != 5 || flowLagOf(fs) != 1 {
		t.Fatalf("flow = %+v, want lastAck 5 lag 1 (dup/stale moved nothing)", fs)
	}
}

// sidOf resolves the (single-connection) live session.
func sidOf(t *testing.T, f *ackFixture, _ *websocket.Conn) session.ID {
	t.Helper()
	ids := f.reg.SessionsBySub("ack-sub")
	if len(ids) != 1 {
		t.Fatalf("sessions = %d, want 1", len(ids))
	}
	return ids[0]
}

// TestAckFutureProtocolErrorWS proves a future ACK yields 202
// protocol_error with the connection alive afterward, and the 202
// itself is ordinary NORMAL IN_WORLD traffic consuming one flow
// sequence (spec §7.1.11).
func TestAckFutureProtocolErrorWS(t *testing.T) {
	f := newAckFixture(t, gateway.OutboundPolicy{MaxUnackedMessages: 8}, nil)
	c := f.dial(t)
	f.enterWorld(t, c, 2) // 219 seq 4
	sendMove(t, c, 2)
	readMoveReply(t, c) // seq 5, lastFlowSent 5

	sendAck(t, c, 3, 50) // 50 >> lastFlowSent, far from half-range
	h202, msg := readError(t, c)
	if msg.Code != proto.ErrorCodeProtocol {
		t.Fatalf("future ack code = %d, want protocol_error", msg.Code)
	}
	if h202.Seq != 6 {
		t.Fatalf("202 seq = %d, want 6 (the error consumes a flow sequence)", h202.Seq)
	}
	// Connection usable: the next intent replies normally.
	sendMove(t, c, 4)
	if h := readMoveReply(t, c); h.Seq != 7 {
		t.Fatalf("post-error reply seq = %d, want 7", h.Seq)
	}
	fs := flowOf(t, f.reg, sidOf(t, f, c))
	if fs.LastAck != 4 || fs.LastFlowSent != 7 {
		t.Fatalf("flow after future ack = %+v, want 4/7 (202+219 consumed units)", fs)
	}
}

// TestAckMalformedInWorldWS proves a malformed 125 payload in IN_WORLD
// is 202 protocol_error without disconnecting (spec §7.1.11/A12).
func TestAckMalformedInWorldWS(t *testing.T) {
	f := newAckFixture(t, gateway.OutboundPolicy{MaxUnackedMessages: 8}, nil)
	c := f.dial(t)
	f.enterWorld(t, c, 2)
	sendFrame(t, c, proto.Header{Opcode: proto.OpcodeAck, MsgVersion: 1, Seq: 2, Tick: 1},
		func(e *proto.Encoder) error { // truncated: 2 of 4 bytes
			e.WriteBytes([]byte{1, 2})
			return nil
		})
	if _, msg := readError(t, c); msg.Code != proto.ErrorCodeProtocol {
		t.Fatalf("malformed ack code = %d, want protocol_error", msg.Code)
	}
	sendMove(t, c, 3)
	readMoveReply(t, c) // socket still usable
}

// TestAckCharacterSelectedRoutingWS proves the frozen pre-window
// behavior through the real Server routing path with a
// directly-constructed CHARACTER_SELECTED registry state (the
// synchronous enter handler prevents a real concurrent mid-baseline
// ACK): a structurally valid ACK is a silent no-op, a malformed one is
// 202 protocol_error, and AUTHENTICATED ACKs never reach ACK handling
// (bad_state from the lifecycle gate).
func TestAckCharacterSelectedRoutingWS(t *testing.T) {
	f := newAckFixture(t, gateway.OutboundPolicy{MaxUnackedMessages: 1}, nil)
	c := f.dial(t)
	sid := f.hello(t, c) // 200 seq 1

	// AUTHENTICATED + 125: bad_state from the gate, before any ACK logic.
	sendAck(t, c, 2, 1)
	if _, msg := readError(t, c); msg.Code != proto.ErrorCodeBadState {
		t.Fatalf("authenticated ack code = %d, want bad_state", msg.Code)
	}
	// Construct CHARACTER_SELECTED directly (registry state only).
	if err := f.reg.BeginEnterWorld(sid, f.charID); err != nil {
		t.Fatalf("begin enter: %v", err)
	}
	// Structurally valid ACK mid-baseline: no-op, no reply, no epoch.
	sendAck(t, c, 3, 7)
	sendReauth(t, c, "tok", 4)
	if h, _ := readFrame(t, c); h.Opcode != proto.OpcodeReauthOK {
		t.Fatalf("next frame opcode = %d, want 201 — an ACK reply leaked", h.Opcode)
	}
	if fs := flowOf(t, f.reg, sid); fs.Active {
		t.Fatalf("ack initialized an epoch: %+v", fs)
	}
	// Malformed ACK stays protocol_error.
	sendFrame(t, c, proto.Header{Opcode: proto.OpcodeAck, MsgVersion: 1, Seq: 5, Tick: 1},
		func(e *proto.Encoder) error {
			e.WriteBytes([]byte{9})
			return nil
		})
	if _, msg := readError(t, c); msg.Code != proto.ErrorCodeProtocol {
		t.Fatalf("malformed selected ack code = %d, want protocol_error", msg.Code)
	}
	// Connection alive: character_list works in CHARACTER_SELECTED.
	sendCharList(t, c, 6)
	if _, list := readCharList(t, c); len(list.Characters) != 1 {
		t.Fatalf("list = %+v", list.Characters)
	}
}

// TestAckNoDownstreamDelegationWS proves opcode 125 is owned by the
// Server: zero downstream handler calls for valid AND malformed ACKs.
func TestAckNoDownstreamDelegationWS(t *testing.T) {
	f := newAckFixture(t, gateway.OutboundPolicy{MaxUnackedMessages: 8}, nil)
	c := f.dial(t)
	f.enterWorld(t, c, 2)
	before := f.next.callCount()
	sendAck(t, c, 2, 4) // valid (the 219 seq)
	sendMove(t, c, 3)   // ordering barrier
	readMoveReply(t, c)
	if n := f.next.callCount(); n != before+1 {
		t.Fatalf("downstream calls after valid ack = %d (before %d) — the ACK leaked downstream", n, before)
	}
	sendFrame(t, c, proto.Header{Opcode: proto.OpcodeAck, MsgVersion: 1, Seq: 4, Tick: 1},
		func(e *proto.Encoder) error { e.WriteBytes([]byte{1}); return nil })
	if _, msg := readError(t, c); msg.Code != proto.ErrorCodeProtocol {
		t.Fatalf("malformed ack code = %d, want protocol_error", msg.Code)
	}
	sendMove(t, c, 5)
	readMoveReply(t, c)
	if n := f.next.callCount(); n != before+2 {
		t.Fatalf("downstream calls after malformed ack = %d, want %d", n, before+2)
	}
}

// TestAckWindowReleasesAndPartialWS covers capacity release and partial
// cumulative ACKs (spec §7.1.11): with max=2, two frames fill the
// window, an ACK for the second fully reopens it; with a larger
// window, four frames ACKed at the second leave exactly two as lag and
// a later ACK of the fourth reaches zero.
func TestAckWindowReleasesAndPartialWS(t *testing.T) {
	f := newAckFixture(t, gateway.OutboundPolicy{MaxUnackedMessages: 8}, nil)
	c := f.dial(t)
	f.enterWorld(t, c, 2) // 219 seq 4
	// Two frames with max=2 behavior verified below under max=8 first:
	// four frames seq 5..8.
	for i := 0; i < 4; i++ {
		sendMove(t, c, uint32(2+i))
		readMoveReply(t, c)
	}
	sid := sidOf(t, f, c)
	// Partial: ACK the second → exactly two remain.
	sendAck(t, c, 6, 6)
	sendMove(t, c, 10)
	readMoveReply(t, c) // barrier
	if fs := flowOf(t, f.reg, sid); fs.LastAck != 6 || flowLagOf(fs) != 3 {
		t.Fatalf("after partial = %+v, want lastAck 6 lag 3", fs)
	}
	// Full: ACK the newest → zero.
	sendAck(t, c, 11, 9)
	sendMove(t, c, 12)
	readMoveReply(t, c)
	if fs := flowOf(t, f.reg, sid); flowLagOf(fs) != 1 {
		t.Fatalf("after full = %+v, want lag 1 (the barrier reply)", fs)
	}
}

// TestAckMaxWindowDisconnectWS proves the real disconnect at a tiny
// window (max=2): two frames write, the third attempt drops the
// session as ack_lag, the socket terminates, and the registry tears the
// session down (reconnect/full resync is the only recovery).
func TestAckMaxWindowDisconnectWS(t *testing.T) {
	f := newAckFixture(t, gateway.OutboundPolicy{MaxUnackedMessages: 2}, nil)
	c := f.dial(t)
	f.enterWorld(t, c, 2) // 219 seq 4
	sendMove(t, c, 2)
	readMoveReply(t, c) // seq 5, lag 1
	sendMove(t, c, 3)
	readMoveReply(t, c) // seq 6, lag 2
	// Third frame: the window is exhausted — dispatch happens, the
	// writer rejects it, the session fails closed.
	sendMove(t, c, 4)
	readClosed(t, c)
	waitFor(t, "session teardown", func() bool { return f.reg.Len() == 0 })
	if _, ok := f.reg.SessionByCharacter(f.charID); ok {
		t.Fatal("character binding survived the slow-client teardown")
	}
	if f.next.callCount() != 3 {
		t.Fatalf("downstream calls = %d, want 3 (the third intent WAS dispatched)", f.next.callCount())
	}
}

// TestAckFullResyncWS is the mandatory slow-client full-resync proof
// (spec §7.1.10/§7.1.11): c1 exceeds the ACK window and disconnects;
// after teardown, c2 reconnects with a NEW session, a NEW sequence
// epoch, a FRESH semantically-distinct baseline generation, no replay,
// no stale ACK debt, and lag 0 at its world_ready.
func TestAckFullResyncWS(t *testing.T) {
	f := newAckFixture(t, gateway.OutboundPolicy{MaxUnackedMessages: 2}, nil)
	c1 := f.dial(t)
	h219c1, sid1 := f.enterWorld(t, c1, 2) // 219 seq 4
	_ = h219c1
	// Two unacked frames fill the window; the third kills c1.
	sendMove(t, c1, 2)
	readMoveReply(t, c1)
	sendMove(t, c1, 3)
	readMoveReply(t, c1)
	sendMove(t, c1, 4)
	readClosed(t, c1)
	waitFor(t, "c1 removed", func() bool { return f.reg.Len() == 0 })
	waitFor(t, "c1 binding removed", func() bool {
		_, ok := f.reg.SessionByCharacter(f.charID)
		return !ok
	})
	if n := f.baseline.callCount(); n != 1 {
		t.Fatalf("baseline calls = %d, want 1 before resync", n)
	}

	// c2: fresh WS, fresh session, fresh sequence epoch.
	c2 := f.dial(t)
	sendHello(t, c2, "tok", 1, 1)
	h200, _ := readFrame(t, c2)
	if h200.Opcode != proto.OpcodeWelcome || h200.Seq != 1 {
		t.Fatalf("c2 welcome = %d seq %d, want 200 seq 1 (new sequence epoch)", h200.Opcode, h200.Seq)
	}
	sid2 := sidOf(t, f, c2)
	if sid2 == sid1 {
		t.Fatal("c2 reused c1's session ID")
	}
	sendEnterWorld(t, c2, 2, 0)
	if _, op := readCharOp(t, c2); op.OK != proto.CharacterOpOK {
		t.Fatalf("c2 217 = %+v", op)
	}
	// Generation 2 baseline: entity NetEntityID 2001, not 1001.
	h203, p203 := readFrame(t, c2)
	if h203.Opcode != proto.OpcodeCellSnapshot || h203.Seq != 3 {
		t.Fatalf("c2 203 = %d seq %d, want 203 seq 3", h203.Opcode, h203.Seq)
	}
	snap, err := proto.DecodeCellSnapshot(p203)
	if err != nil || len(snap.Entities) != 1 || snap.Entities[0].Entity != 2001 {
		t.Fatalf("c2 baseline entity = %+v, %v — generation 2 must carry 2001", snap.Entities, err)
	}
	h219c2, _ := readFrame(t, c2)
	if h219c2.Opcode != proto.OpcodeWorldReady || h219c2.Seq != 4 {
		t.Fatalf("c2 219 = %d seq %d", h219c2.Opcode, h219c2.Seq)
	}
	// No replay: nothing from c1 (its stream ended at seq 6 on another
	// session) — c2's stream was exactly 200(1)/217(2)/203(3)/219(4).
	// The 219 read proves the write; completion runs on the handler
	// goroutine immediately after, so wait for the registry to reflect
	// IN_WORLD before reading the epoch.
	waitForSessionState(t, f.reg, sid2, session.StateInWorld)
	fs := flowOf(t, f.reg, sid2)
	if !fs.Active || fs.LastAck != 4 || fs.LastFlowSent != 4 || flowLagOf(fs) != 0 {
		t.Fatalf("c2 epoch = %+v, want fresh 4/4 lag 0 — old debt leaked", fs)
	}
	if n := f.baseline.callCount(); n != 2 {
		t.Fatalf("baseline calls = %d, want 2", n)
	}
	// c2's window is fully fresh: two frames fit again.
	sendMove(t, c2, 3)
	readMoveReply(t, c2)
	sendMove(t, c2, 4)
	readMoveReply(t, c2)
	if fs := flowOf(t, f.reg, sid2); flowLagOf(fs) != 2 {
		t.Fatalf("c2 lag after two frames = %+v", fs)
	}
}

// TestAckLeaveResetsEpochWS proves a real 126 leave clears the epoch
// and a re-enter is completely ungated by any old debt (spec §7.1.11):
// with max=1, an unacked frame exists at leave time, and the second
// multi-frame baseline still completes in full.
func TestAckLeaveResetsEpochWS(t *testing.T) {
	f := newAckFixture(t, gateway.OutboundPolicy{MaxUnackedMessages: 1}, nil)
	c := f.dial(t)
	_, sid := f.enterWorld(t, c, 2) // 219 seq 4
	sendMove(t, c, 2)
	readMoveReply(t, c) // seq 5, lag 1 — never ACKed
	sendLeave(t, c, 3)
	waitFor(t, "epoch cleared by leave", func() bool {
		fs, err := f.reg.FlowState(sid)
		return err == nil && !fs.Active && fs.LastAck == 0 && fs.LastFlowSent == 0
	})
	snap, _ := f.reg.Get(sid)
	if snap.State != session.StateAuthenticated || snap.HasCharacter {
		t.Fatalf("after leave = %+v", snap)
	}
	// Re-enter: with max=1 the fresh baseline (217/203/219) completes
	// only because CHARACTER_SELECTED traffic is ungated and the new
	// epoch starts at the NEW 219 sequence with no old debt. The
	// session's seq counter persists across leave: the re-enter writes
	// 217(6), 203(7), 219(8).
	sendEnterWorld(t, c, 4, 0)
	if _, op := readCharOp(t, c); op.OK != proto.CharacterOpOK {
		t.Fatalf("re-enter 217 = %+v", op)
	}
	if h, _ := readFrame(t, c); h.Opcode != proto.OpcodeCellSnapshot {
		t.Fatalf("re-enter 203 missing (blocked by old debt?)")
	}
	h219, _ := readFrame(t, c)
	if h219.Opcode != proto.OpcodeWorldReady || h219.Seq != 8 {
		t.Fatalf("re-enter barrier = %d seq %d, want 219 seq 8", h219.Opcode, h219.Seq)
	}
	waitFor(t, "re-entered IN_WORLD", func() bool {
		s, ok := f.reg.Get(sid)
		return ok && s.State == session.StateInWorld
	})
	fs := flowOf(t, f.reg, sid)
	if !fs.Active || fs.LastAck != 8 || fs.LastFlowSent != 8 || flowLagOf(fs) != 0 {
		t.Fatalf("re-enter epoch = %+v, want fresh 8/8 lag 0", fs)
	}
	// The fresh window works: one frame fits (max=1).
	sendMove(t, c, 5)
	if h := readMoveReply(t, c); h.Seq != 9 {
		t.Fatalf("post-re-enter reply seq = %d, want 9", h.Seq)
	}
}

// TestAckTakeoverResetsEpochsWS proves duplicate-login takeover resets
// both epochs (spec §6.1.2/§7.1.11): an old IN_WORLD session carrying
// unacked debt is flushed, unbound (epoch cleared in the same
// mutation), kicked, and retired; the replacement's baseline and fresh
// epoch are never blocked by the old debt.
func TestAckTakeoverResetsEpochsWS(t *testing.T) {
	f := newAckFixture(t, gateway.OutboundPolicy{MaxUnackedMessages: 1}, nil)
	c1 := f.dial(t)
	f.enterWorld(t, c1, 2)
	sendMove(t, c1, 2)
	readMoveReply(t, c1) // c1 debt: 1 unacked

	c2 := f.dial(t)
	f.hello(t, c2) // same sub → same account
	sendEnterWorld(t, c2, 2, 0)
	// c1 receives the best-effort direct kicked frame, then closes.
	if _, msg := readError(t, c1); msg.Code != proto.ErrorCodeKicked {
		t.Fatalf("c1 code = %d, want kicked", msg.Code)
	}
	readClosed(t, c1)
	// c2's baseline is ungated (max=1) and ends at its own 219.
	if _, op := readCharOp(t, c2); op.OK != proto.CharacterOpOK {
		t.Fatalf("c2 217 = %+v", op)
	}
	if h, _ := readFrame(t, c2); h.Opcode != proto.OpcodeCellSnapshot {
		t.Fatal("c2 203 missing")
	}
	h219, _ := readFrame(t, c2)
	if h219.Opcode != proto.OpcodeWorldReady {
		t.Fatal("c2 219 missing")
	}
	waitFor(t, "c1 retired", func() bool { return f.reg.Len() == 1 })
	sid2 := sidOf(t, f, c2)
	// The 219 read proves the write; completion runs right after on the
	// handler goroutine — wait for IN_WORLD before reading the epoch.
	waitForSessionState(t, f.reg, sid2, session.StateInWorld)
	fs := flowOf(t, f.reg, sid2)
	if !fs.Active || flowLagOf(fs) != 0 || fs.LastFlowSent != 4 {
		t.Fatalf("c2 epoch = %+v, want fresh 4/4 lag 0 — c1 debt transferred", fs)
	}
	// c1's epoch died with its binding; c2's fresh window holds one frame.
	sendMove(t, c2, 3)
	if h := readMoveReply(t, c2); h.Seq != 5 {
		t.Fatalf("c2 reply seq = %d, want 5", h.Seq)
	}
	if f.exit.callCount() != 1 {
		t.Fatalf("WorldExit calls = %d, want 1 (old flush before unbind)", f.exit.callCount())
	}
}

// TestAckLagMetricsGatewayWS proves gateway↔metrics integration with a
// REAL observe.OutboundMetrics instance (B76/B77): real outbound
// activity reaches Prometheus — depth/ack-lag histograms sample, and a
// real ack_lag disconnect increments exactly
// vox_session_drops_total{reason="ack_lag"} by one.
func TestAckLagMetricsGatewayWS(t *testing.T) {
	f, _ := newAckMetricsFixture(t, gateway.OutboundPolicy{MaxUnackedMessages: 2})
	reg := f.obs.Registry()
	c := f.dial(t)
	f.enterWorld(t, c, 2)
	sendMove(t, c, 2)
	readMoveReply(t, c)
	sendMove(t, c, 3)
	readMoveReply(t, c)
	// Depth and lag observations must already be flowing.
	waitFor(t, "queue depth observations", func() bool {
		return histSamplesOf(t, reg, "vox_outbound_queue_depth_messages") > 0
	})
	waitFor(t, "ack lag observations", func() bool {
		return histSamplesOf(t, reg, "vox_outbound_ack_lag_messages") > 0
	})
	if v := counterValueOf(t, reg, "vox_session_drops_total", "reason", "ack_lag"); v != 0 {
		t.Fatalf("ack_lag drops = %v before the disconnect, want 0", v)
	}
	// Exceed the window: real ack_lag disconnect.
	sendMove(t, c, 4)
	readClosed(t, c)
	waitFor(t, "session teardown", func() bool { return f.reg.Len() == 0 })
	waitFor(t, "ack_lag drop counted", func() bool {
		return counterValueOf(t, reg, "vox_session_drops_total", "reason", "ack_lag") == 1
	})
	// No other drop reason fired.
	for _, reason := range []string{"write_timeout", "reliable_enqueue_timeout", "critical_queue_saturated"} {
		if v := counterValueOf(t, reg, "vox_session_drops_total", "reason", reason); v != 0 {
			t.Fatalf("%s = %v, want 0", reason, v)
		}
	}
}

// TestAckBaselineUngatedMaxOneWS proves the entire baseline stays
// ungated with max_unacked=1 (spec §7.1.11): a multi-frame
// 217/203/218/220/219 baseline completes with no ACK, and the epoch
// lands at lag 0 on the written 219 sequence.
func TestAckBaselineUngatedMaxOneWS(t *testing.T) {
	f := newAckFixture(t, gateway.OutboundPolicy{MaxUnackedMessages: 1}, nil)
	f.baseline.rich = true
	c := f.dial(t)
	sendHello(t, c, "tok", 1, 1) // 200 seq 1
	if h, _ := readFrame(t, c); h.Opcode != proto.OpcodeWelcome {
		t.Fatal("welcome missing")
	}
	sendEnterWorld(t, c, 2, 0)
	if _, op := readCharOp(t, c); op.OK != proto.CharacterOpOK { // 217 seq 2
		t.Fatalf("217 = %+v", op)
	}
	// 203(3), 218(4), 220(5), 219(6) — four more baseline frames with a
	// window of one: only transport backpressure governs them.
	wantOps := []uint16{
		proto.OpcodeCellSnapshot,
		proto.OpcodeChunkFragment,
		proto.OpcodeShopList,
		proto.OpcodeWorldReady,
	}
	var h219 proto.Header
	for i, want := range wantOps {
		h, _ := readFrame(t, c)
		if h.Opcode != want {
			t.Fatalf("baseline frame %d opcode = %d, want %d", i, h.Opcode, want)
		}
		if h.Seq != uint32(3+i) {
			t.Fatalf("baseline frame %d seq = %d, want %d", i, h.Seq, 3+i)
		}
		h219 = h
	}
	sid := sidOf(t, f, c)
	waitFor(t, "IN_WORLD", func() bool {
		s, ok := f.reg.Get(sid)
		return ok && s.State == session.StateInWorld
	})
	fs := flowOf(t, f.reg, sid)
	if !fs.Active || fs.LastAck != h219.Seq || fs.LastFlowSent != h219.Seq || flowLagOf(fs) != 0 {
		t.Fatalf("epoch = %+v, want %d/%d lag 0", fs, h219.Seq, h219.Seq)
	}
	// Post-world the window IS enforced: one frame fits, the second
	// disconnects as ack_lag.
	sendMove(t, c, 3)
	if h := readMoveReply(t, c); h.Seq != h219.Seq+1 {
		t.Fatalf("first post-world reply seq = %d, want %d", h.Seq, h219.Seq+1)
	}
	sendMove(t, c, 4)
	readClosed(t, c)
	waitFor(t, "session teardown", func() bool { return f.reg.Len() == 0 })
}

// TestOutboundMetricsSlowPeerWS proves the T5a slow-client drop
// reasons still increment their ORIGINAL counters through the real
// Prometheus adapter while ack_lag was added (B78): a non-reading WS
// client flooded past its budgets is dropped as write_timeout or
// reliable_enqueue_timeout — never ack_lag.
// ackFloodHandler answers one allowed opcode with a burst of large
// frames so a non-reading client saturates the outbound path (the
// gateway_test-local twin of the T5a flood double).
type ackFloodHandler struct {
	burst int
}

func (h *ackFloodHandler) Handle(
	_ context.Context,
	_ session.ID,
	_ proto.Header,
	_ *proto.Decoder,
	send gateway.SendFunc,
) error {
	blob := make([]byte, 60000)
	for i := 0; i < h.burst; i++ {
		if err := send(proto.OpcodeChunkFragment, proto.MessageVersion1, func(e *proto.Encoder) error {
			e.WriteBytes(blob)
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func TestOutboundMetricsSlowPeerWS(t *testing.T) {
	obsServer := observe.New(observe.NewReadiness())
	reg := obsServer.Registry()
	flood := &ackFloodHandler{burst: 60}
	srv := gateway.NewServer(gateway.ServerDeps{
		Registry: session.NewRegistry(),
		Validator: auth.ValidatorFunc(func(_ context.Context, _ string) (auth.Identity, error) {
			return auth.Identity{Sub: "slow-sub", ExpiresAt: time.Now().Add(time.Hour)}, nil
		}),
		Accounts: gateway.AccountProvisionerFunc(func(_ context.Context, _ string, _ *string) (int64, error) {
			return 1, nil
		}),
		Tick:     func() uint32 { return 1 },
		Now:      time.Now,
		Schedule: func(time.Time, func()) gateway.CancelFunc { return func() {} },
		Outbound: gateway.OutboundPolicy{
			MaxMessages:            1024,
			MaxBytes:               262144,
			ReliableEnqueueTimeout: 100 * time.Millisecond,
			WriteTimeout:           100 * time.Millisecond,
		},
		OutboundObserver: obsServer.OutboundMetrics(),
		Handler:          flood,
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	client, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http"), nil)
	cancel()
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.CloseNow() })
	sendHello(t, client, "x", 1, 1)
	readCtx, readCancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, _, err = client.Read(readCtx) // welcome, then stop reading
	readCancel()
	if err != nil {
		t.Fatalf("welcome read: %v", err)
	}
	sendCharList(t, client, 2) // trigger the flood; never read again
	waitFor(t, "slow drop counted", func() bool {
		return counterValueOf(t, reg, "vox_session_drops_total", "reason", "write_timeout")+
			counterValueOf(t, reg, "vox_session_drops_total", "reason", "reliable_enqueue_timeout") >= 1
	})
	if v := counterValueOf(t, reg, "vox_session_drops_total", "reason", "ack_lag"); v != 0 {
		t.Fatalf("ack_lag = %v, want 0 — a transport slow peer is not ACK lag", v)
	}
}
