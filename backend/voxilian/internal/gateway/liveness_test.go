package gateway

import (
	"context"
	"errors"
	"net/http"
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
// deterministic timer seam + clock + poll helper
// ---------------------------------------------------------------------------

// manualTicker is the manually-pulsable Ticker: tests fire the exact
// 15 s ping / 30 s sweep cadences with zero sleeps.
type manualTicker struct {
	ch chan time.Time
}

func (m *manualTicker) Fire() <-chan time.Time { return m.ch }
func (m *manualTicker) Stop()                  {}

// pulse delivers one tick; it returns once the loop consumed it.
func (m *manualTicker) pulse() { m.ch <- time.Now() }

// tryPulse reports whether a receiver is still listening (proving loop
// exit after Close without blocking).
func (m *manualTicker) tryPulse() bool {
	select {
	case m.ch <- time.Now():
		return true
	default:
		return false
	}
}

// manualTickerFactory records every ticker by period so tests can
// pulse the ping (15 s) and sweep (30 s) loops independently.
type manualTickerFactory struct {
	mu      sync.Mutex
	tickers map[time.Duration][]*manualTicker
}

func newManualTickerFactory() *manualTickerFactory {
	return &manualTickerFactory{tickers: make(map[time.Duration][]*manualTicker)}
}

func (f *manualTickerFactory) newTicker(period time.Duration) Ticker {
	m := &manualTicker{ch: make(chan time.Time)}
	f.mu.Lock()
	f.tickers[period] = append(f.tickers[period], m)
	f.mu.Unlock()
	return m
}

// last returns the most recent ticker created for period.
func (f *manualTickerFactory) last(period time.Duration) *manualTicker {
	f.mu.Lock()
	defer f.mu.Unlock()
	ts := f.tickers[period]
	if len(ts) == 0 {
		return nil
	}
	return ts[len(ts)-1]
}

func (f *manualTickerFactory) count(period time.Duration) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tickers[period])
}

// fakeClock is the injected wall clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) set(t time.Time) { c.mu.Lock(); defer c.mu.Unlock(); c.t = t }

// eventually polls cond (busy, matching waitSimTick) until true; the
// only permitted asynchronous-wait helper for real WS completion.
func eventually(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout: %s", desc)
		}
	}
}

// countingPinger wraps a Pinger, counting Ping attempts.
type countingPinger struct {
	p     Pinger
	count atomic.Int32
}

func (c *countingPinger) Ping(ctx context.Context) error {
	c.count.Add(1)
	return c.p.Ping(ctx)
}

// recordingWorldExit is a scriptable WorldExit observing call order.
type recordingWorldExit struct {
	mu    sync.Mutex
	calls []session.ID
	err   error
}

func (w *recordingWorldExit) ExitWorld(_ context.Context, sid session.ID, _, _ int64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, sid)
	return w.err
}

func (w *recordingWorldExit) called() []session.ID {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]session.ID(nil), w.calls...)
}

// ---------------------------------------------------------------------------
// constructor validation
// ---------------------------------------------------------------------------

func TestLivenessRequiresDeps(t *testing.T) {
	reg := session.NewRegistry()
	presence, _ := NewPresenceRegistry(testPolicy())
	exit := WorldExitFunc(func(context.Context, session.ID, int64, int64) error { return nil })
	reaper, err := NewSessionReaper(reg, presence, exit)
	if err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{t: worldRuntimeBase}
	tf := newManualTickerFactory()
	if _, err := NewSessionReaper(nil, presence, exit); err == nil {
		t.Error("nil registry accepted")
	}
	if _, err := NewSessionReaper(reg, nil, exit); err == nil {
		t.Error("nil presence accepted")
	}
	if _, err := NewSessionReaper(reg, presence, nil); err == nil {
		t.Error("nil world exit accepted")
	}
	if _, err := NewTransportLiveness(nil, reg, reaper, clk.Now, tf.newTicker); err == nil {
		t.Error("nil presence accepted")
	}
	if _, err := NewTransportLiveness(presence, nil, reaper, clk.Now, tf.newTicker); err == nil {
		t.Error("nil registry accepted")
	}
	if _, err := NewTransportLiveness(presence, reg, nil, clk.Now, tf.newTicker); err == nil {
		t.Error("nil reaper accepted")
	}
	if _, err := NewTransportLiveness(presence, reg, reaper, nil, tf.newTicker); err == nil {
		t.Error("nil clock accepted")
	}
	if _, err := NewTransportLiveness(presence, reg, reaper, clk.Now, nil); err == nil {
		t.Error("nil ticker factory accepted")
	}
}

func TestLivenessStartCloseIdempotent(t *testing.T) {
	reg := session.NewRegistry()
	presence, _ := NewPresenceRegistry(testPolicy())
	exit := WorldExitFunc(func(context.Context, session.ID, int64, int64) error { return nil })
	reaper, _ := NewSessionReaper(reg, presence, exit)
	clk := &fakeClock{t: worldRuntimeBase}
	tf := newManualTickerFactory()
	lv, err := NewTransportLiveness(presence, reg, reaper, clk.Now, tf.newTicker)
	if err != nil {
		t.Fatal(err)
	}
	// Start is idempotent: exactly one sweep ticker.
	lv.Start()
	lv.Start()
	eventually(t, "sweep ticker", func() bool { return tf.count(StaleSweepInterval) >= 1 })
	if got := tf.count(StaleSweepInterval); got != 1 {
		t.Fatalf("sweep tickers = %d, want 1", got)
	}
	lv.Close()
	lv.Close()
	// The sweep loop exited: no receiver remains.
	if tf.last(StaleSweepInterval).tryPulse() {
		t.Fatalf("sweep loop still running after Close")
	}
	// A closed liveness never restarts.
	lv.Start()
	if tf.last(StaleSweepInterval).tryPulse() {
		t.Fatalf("sweep loop restarted after Close")
	}
}

// ---------------------------------------------------------------------------
// ping loop over a real WebSocket pair
// ---------------------------------------------------------------------------

// pingPair is one accepted server-side WebSocket with the liveness
// ping loop attached, plus its dialed client.
type pingPair struct {
	ts       *httptest.Server
	client   *websocket.Conn
	readErr  chan error
	handler  chan struct{} // closes when the server read loop exits
	sid      session.ID
	pinger   *countingPinger
	registry *session.Registry
}

// newPingPair accepts one real WebSocket; the handler registers the
// session, optionally activates Presence, and runs the per-connection
// ping loop until the transport dies.
func newPingPair(t *testing.T, lv *TransportLiveness, reg *session.Registry, presence *PresenceRegistry, withPresence bool) *pingPair {
	t.Helper()
	sidCh := make(chan session.ID, 1)
	pingerCh := make(chan *countingPinger, 1)
	handlerDone := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		wsConn := newWSConnection(conn)
		conn.SetReadLimit(proto.MaxFrameSize)
		sid := reg.Create(wsConn)
		if withPresence {
			if _, aerr := presence.Activate(sid, 500, 7, world.CellCoord{}, worldRuntimeBase); aerr != nil {
				_ = conn.CloseNow()
				return
			}
		}
		cp := &countingPinger{p: wsConn}
		stop := lv.StartPinger(sid, cp)
		defer stop()
		sidCh <- sid
		pingerCh <- cp
		for {
			if _, _, rerr := conn.Read(r.Context()); rerr != nil {
				return
			}
		}
	}))
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.CloseNow() })
	readErr := make(chan error, 1)
	go func() {
		for {
			if _, _, rerr := client.Read(context.Background()); rerr != nil {
				readErr <- rerr
				return
			}
		}
	}()
	var sid session.ID
	select {
	case sid = <-sidCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("handler never registered the session")
	}
	select {
	case cp := <-pingerCh:
		return &pingPair{
			ts: ts, client: client, readErr: readErr, handler: handlerDone,
			sid: sid, pinger: cp, registry: reg,
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("handler never started the ping loop")
	}
	return nil
}

func newTestLiveness(t *testing.T, reg *session.Registry, presence *PresenceRegistry, worldExit WorldExit, clk *fakeClock, tf *manualTickerFactory) *TransportLiveness {
	t.Helper()
	reaper, err := NewSessionReaper(reg, presence, worldExit)
	if err != nil {
		t.Fatal(err)
	}
	lv, err := NewTransportLiveness(presence, reg, reaper, clk.Now, tf.newTicker)
	if err != nil {
		t.Fatal(err)
	}
	return lv
}

// B46: a manual 15 s pulse with active Presence runs a real server
// Ping; the client reader processes it (auto-Pong); only then does the
// heartbeat advance to the injected Now.
func TestHeartbeatPingRealWebSocket(t *testing.T) {
	reg := session.NewRegistry()
	presence, _ := NewPresenceRegistry(testPolicy())
	clk := &fakeClock{t: worldRuntimeBase}
	tf := newManualTickerFactory()
	lv := newTestLiveness(t, reg, presence,
		WorldExitFunc(func(context.Context, session.ID, int64, int64) error { return nil }), clk, tf)
	p := newPingPair(t, lv, reg, presence, true)
	ping := tf.last(HeartbeatPingInterval)
	if ping == nil {
		t.Fatalf("no ping ticker created")
	}
	clk.set(worldRuntimeBase.Add(10 * time.Second))
	ping.pulse()
	eventually(t, "heartbeat advanced to injected now", func() bool {
		snap, err := presence.Snapshot(p.sid)
		return err == nil && snap.HeartbeatAt.Equal(worldRuntimeBase.Add(10*time.Second))
	})
	if p.pinger.count.Load() == 0 {
		t.Fatalf("heartbeat advanced without any Ping")
	}
}

// B47: without active Presence the 15 s pulse pings nothing and the
// connection stays alive.
func TestHeartbeatNoPresenceSkipsPing(t *testing.T) {
	reg := session.NewRegistry()
	presence, _ := NewPresenceRegistry(testPolicy())
	clk := &fakeClock{t: worldRuntimeBase}
	tf := newManualTickerFactory()
	lv := newTestLiveness(t, reg, presence,
		WorldExitFunc(func(context.Context, session.ID, int64, int64) error { return nil }), clk, tf)
	p := newPingPair(t, lv, reg, presence, false)
	ping := tf.last(HeartbeatPingInterval)
	ping.pulse()
	eventually(t, "pulse consumed", func() bool { return p.pinger.count.Load() >= 0 && tf.count(HeartbeatPingInterval) > 0 })
	if got := p.pinger.count.Load(); got != 0 {
		t.Fatalf("pre-Presence session pinged %d times, want 0", got)
	}
	// The connection remains: a client-side Ping still completes
	// (the server reader is alive and answers Pong).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.client.Ping(ctx); err != nil {
		t.Fatalf("connection disturbed by skipped pulse: %v", err)
	}
}

// B48: a successful Pong changes nothing about authorization lifetime.
func TestHeartbeatPongDoesNotChangeAuth(t *testing.T) {
	reg := session.NewRegistry()
	presence, _ := NewPresenceRegistry(testPolicy())
	clk := &fakeClock{t: worldRuntimeBase}
	tf := newManualTickerFactory()
	lv := newTestLiveness(t, reg, presence,
		WorldExitFunc(func(context.Context, session.ID, int64, int64) error { return nil }), clk, tf)
	p := newPingPair(t, lv, reg, presence, true)
	tokenExp := worldRuntimeBase.Add(time.Hour)
	if err := reg.Authenticate(p.sid, "sub-a", 11, tokenExp); err != nil {
		t.Fatal(err)
	}
	clk.set(worldRuntimeBase.Add(15 * time.Second))
	tf.last(HeartbeatPingInterval).pulse()
	eventually(t, "heartbeat touched", func() bool {
		snap, err := presence.Snapshot(p.sid)
		return err == nil && snap.HeartbeatAt.Equal(worldRuntimeBase.Add(15*time.Second))
	})
	snap, ok := reg.Get(p.sid)
	if !ok || !snap.TokenExp.Equal(tokenExp) || snap.State != session.StateAuthenticated {
		t.Fatalf("auth lifetime disturbed by Pong: %+v", snap)
	}
}

// errPinger fails every Ping immediately (a dead peer whose Pong can
// never arrive, without the transport race of a real close).
type errPinger struct{ attempts atomic.Int32 }

func (p *errPinger) Ping(context.Context) error {
	p.attempts.Add(1)
	return errors.New("pong lost")
}

// B49: a failed Ping (dead peer) touches no heartbeat and CloseNows
// the transport so the read loop unblocks into the reaper path.
func TestHeartbeatPingTimeoutNoTouch(t *testing.T) {
	reg := session.NewRegistry()
	presence, _ := NewPresenceRegistry(testPolicy())
	clk := &fakeClock{t: worldRuntimeBase}
	tf := newManualTickerFactory()
	lv := newTestLiveness(t, reg, presence,
		WorldExitFunc(func(context.Context, session.ID, int64, int64) error { return nil }), clk, tf)
	conn := newTakeoverConn()
	sid := reg.Create(conn)
	if _, err := presence.Activate(sid, 500, 7, world.CellCoord{}, worldRuntimeBase); err != nil {
		t.Fatal(err)
	}
	ep := &errPinger{}
	stop := lv.StartPinger(sid, ep)
	defer stop()
	clk.set(worldRuntimeBase.Add(31 * time.Second))
	tf.last(HeartbeatPingInterval).pulse()
	eventually(t, "failed ping force-closes transport", func() bool {
		return conn.closeNowCount() > 0
	})
	if ep.attempts.Load() == 0 {
		t.Fatalf("close without a ping attempt")
	}
	snap, err := presence.Snapshot(sid)
	if err != nil {
		t.Fatalf("presence lost by ping failure: %v", err)
	}
	if !snap.HeartbeatAt.Equal(worldRuntimeBase) {
		t.Fatalf("failed ping touched heartbeat: %v", snap.HeartbeatAt)
	}
	// No 202 was required: nothing was written; recovery is
	// reconnect/full-resync via the reaper path.
}

// ---------------------------------------------------------------------------
// reaper over the real Server + WorldSessionRuntime composition
// ---------------------------------------------------------------------------

// reaperFixture wires a real Server (real outbound queues over real
// WebSockets) whose Liveness/reaper share the same WorldSessionRuntime
// composition used by leave/takeover.
type reaperFixture struct {
	reg        *session.Registry
	presence   *PresenceRegistry
	fakeSim    *recordingSim
	downstream *recordingDownstream
	fanout     *recordingFanout
	runtime    *WorldSessionRuntime
	liveness   *TransportLiveness
	server     *Server
	ts         *httptest.Server
	clk        *fakeClock
	tf         *manualTickerFactory
	nextSid    int32
}

func newReaperFixture(t *testing.T) *reaperFixture {
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
	server := NewServer(ServerDeps{
		Registry: reg,
		Tick:     func() uint32 { return 1000 },
		Now:      clk.Now,
		Liveness: liveness,
	})
	ts := httptest.NewServer(server)
	t.Cleanup(func() {
		ts.Close()
		server.Close()
	})
	return &reaperFixture{
		reg: reg, presence: presence, fakeSim: fakeSim, downstream: downstream,
		fanout: fanout, runtime: runtime, liveness: liveness, server: server,
		ts: ts, clk: clk, tf: tf,
	}
}

// dial opens one client WebSocket and waits for its session to exist
// (IDs allocate monotonically from 1 on this fresh registry).
func (f *reaperFixture) dial(t *testing.T) (session.ID, *websocket.Conn) {
	t.Helper()
	sid := session.ID(atomic.AddInt32(&f.nextSid, 1))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(f.ts.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.CloseNow() })
	eventually(t, "session registered", func() bool {
		_, ok := f.reg.Get(sid)
		return ok
	})
	return sid, client
}

// enterWorldDirect drives one dialed session straight to a fully owned
// IN_WORLD + character + active Presence state (the staged-enter result
// the reaper expects), staging nothing else.
func (f *reaperFixture) enterWorldDirect(t *testing.T, accountID, charID int64, entity sim.EntityID) (session.ID, *websocket.Conn) {
	t.Helper()
	sid, client := f.dial(t)
	if err := f.reg.Authenticate(sid, "sub-"+string(rune('a'+sid-1)), accountID, f.clk.Now().Add(time.Hour)); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if err := f.reg.BeginEnterWorld(sid, charID); err != nil {
		t.Fatalf("begin enter: %v", err)
	}
	if err := f.reg.CompleteEnterWorld(sid, charID); err != nil {
		t.Fatalf("complete enter: %v", err)
	}
	if _, err := f.presence.Activate(sid, charID, entity, world.CellCoord{}, f.clk.Now()); err != nil {
		t.Fatalf("activate presence: %v", err)
	}
	return sid, client
}

// B50: an abruptly disconnected IN_WORLD session gets the full
// flush-first cleanup before its registry removal.
func TestRawDisconnectCleanupSucceeds(t *testing.T) {
	f := newReaperFixture(t)
	sid, client := f.enterWorldDirect(t, 11, 500, 1)
	if err := client.CloseNow(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "registry entry removed after cleanup", func() bool { return f.reg.Len() == 0 })
	if f.downstream.callCount() != 1 {
		t.Fatalf("downstream flush calls = %d, want 1", f.downstream.callCount())
	}
	if len(f.fakeSim.removedIDs()) != 1 || f.fakeSim.removedIDs()[0] != 1 {
		t.Fatalf("sim removes = %v, want [1]", f.fakeSim.removedIDs())
	}
	rms := f.fanout.removeCalls()
	if len(rms) != 1 || rms[0] != (fanoutRemoveCall{sid: sid, entity: 1}) {
		t.Fatalf("fanout removes = %+v, want [{%d 1}]", rms, sid)
	}
	if f.presence.Len() != 0 {
		t.Fatalf("presence survives disconnect cleanup")
	}
	if _, ok := f.reg.Get(sid); ok {
		t.Fatalf("registry entry survives disconnect cleanup")
	}
}

// B51: a failing downstream flush retains the whole world session for
// retry instead of orphaning world state.
func TestRawDisconnectFlushFailureRetains(t *testing.T) {
	f := newReaperFixture(t)
	sid, client := f.enterWorldDirect(t, 11, 500, 1)
	f.downstream.err = errors.New("flush unavailable")
	if err := client.CloseNow(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "cleanup attempted", func() bool { return f.downstream.callCount() == 1 })
	if _, ok := f.reg.Get(sid); !ok {
		t.Fatalf("registry entry removed despite failed cleanup")
	}
	snap, _ := f.reg.Get(sid)
	if snap.State != session.StateInWorld || !snap.HasCharacter {
		t.Fatalf("IN_WORLD binding disturbed by failed cleanup: %+v", snap)
	}
	if _, err := f.presence.Snapshot(sid); err != nil {
		t.Fatalf("presence lost by failed cleanup: %v", err)
	}
	if len(f.fakeSim.removedIDs()) != 0 {
		t.Fatalf("sim entity removed despite failed flush: %v", f.fakeSim.removedIDs())
	}
	if len(f.fanout.removeCalls()) != 0 {
		t.Fatalf("fanout removed despite failed flush: %+v", f.fanout.removeCalls())
	}
}

// B52: after a retained disconnect, the next sweep (dependency
// recovered) completes the cleanup and drops the stale entry.
func TestRawDisconnectRetryRetainedSweep(t *testing.T) {
	f := newReaperFixture(t)
	_, client := f.enterWorldDirect(t, 11, 500, 1)
	f.downstream.err = errors.New("flush unavailable")
	if err := client.CloseNow(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "failed cleanup attempted", func() bool { return f.downstream.callCount() == 1 })
	f.downstream.err = nil
	f.clk.set(worldRuntimeBase.Add(31 * time.Second))
	f.liveness.Sweep()
	eventually(t, "stale session reaped on retry", func() bool { return f.reg.Len() == 0 })
	if f.downstream.callCount() != 2 {
		t.Fatalf("downstream calls = %d, want 2 (retry succeeded)", f.downstream.callCount())
	}
	if f.presence.Len() != 0 {
		t.Fatalf("presence survives retry sweep")
	}
}

// B53: the stale boundary is inclusive at exactly HeartbeatTimeout;
// one second earlier the sweep does nothing.
func TestStaleBoundarySweep(t *testing.T) {
	f := newReaperFixture(t)
	sid, _ := f.enterWorldDirect(t, 11, 500, 1)
	f.clk.set(worldRuntimeBase.Add(PresenceHeartbeatTimeout - time.Second))
	f.liveness.Sweep()
	if f.downstream.callCount() != 0 {
		t.Fatalf("pre-boundary sweep reaped: %d calls", f.downstream.callCount())
	}
	if _, ok := f.reg.Get(sid); !ok {
		t.Fatalf("pre-boundary sweep removed the session")
	}
	if f.presence.Len() != 1 {
		t.Fatalf("pre-boundary sweep disturbed presence")
	}
	// Exactly HeartbeatAt + 30s is stale (inclusive).
	f.clk.set(worldRuntimeBase.Add(PresenceHeartbeatTimeout))
	f.liveness.Sweep()
	eventually(t, "boundary-stale session reaped", func() bool { return f.reg.Len() == 0 })
	if f.downstream.callCount() != 1 {
		t.Fatalf("boundary sweep flush calls = %d, want 1", f.downstream.callCount())
	}
}

// B54: reaper invocations follow the sorted StaleSessions order.
func TestStaleSweepSortedProcessing(t *testing.T) {
	f := newReaperFixture(t)
	sid1, _ := f.enterWorldDirect(t, 11, 500, 1)
	sid2, _ := f.enterWorldDirect(t, 12, 502, 22)
	// Sid 2 carries the OLDER heartbeat (stales first); processing
	// order still follows the sorted ID list [1 2].
	if err := f.presence.TouchHeartbeat(sid1, worldRuntimeBase.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	f.clk.set(worldRuntimeBase.Add(PresenceHeartbeatTimeout + 20*time.Second))
	f.liveness.Sweep()
	eventually(t, "both stale sessions reaped", func() bool { return f.reg.Len() == 0 })
	var order []session.ID
	for _, rm := range f.fanout.removeCalls() {
		order = append(order, rm.sid)
	}
	if len(order) != 2 || order[0] != sid1 || order[1] != sid2 {
		t.Fatalf("reap order = %v, want sorted [%d %d]", order, sid1, sid2)
	}
}

// B55: a failed sweep retains for the next sweep.
func TestStaleCleanupRetry(t *testing.T) {
	f := newReaperFixture(t)
	sid, _ := f.enterWorldDirect(t, 11, 500, 1)
	f.downstream.err = errors.New("flush unavailable")
	f.clk.set(worldRuntimeBase.Add(PresenceHeartbeatTimeout + time.Second))
	f.liveness.Sweep()
	// The sweep's CloseNow also unblocks the connection teardown into
	// its own (equally failing) reap: >= 1 attempts, all retained.
	eventually(t, "first sweep attempted", func() bool { return f.downstream.callCount() >= 1 })
	if _, ok := f.reg.Get(sid); !ok {
		t.Fatalf("failed sweep removed the stale world session")
	}
	if f.presence.Len() != 1 {
		t.Fatalf("failed sweep disturbed presence")
	}
	f.downstream.err = nil
	f.liveness.Sweep()
	eventually(t, "second sweep cleaned", func() bool { return f.reg.Len() == 0 })
	if f.presence.Len() != 0 {
		t.Fatalf("presence survives recovered sweep")
	}
}

// B56: concurrent read-error reap and stale sweep for one sid produce
// exactly one destructive WorldExit and a clean final state.
func TestReapConcurrentReadErrorAndSweep(t *testing.T) {
	f := newReaperFixture(t)
	sid, _ := f.enterWorldDirect(t, 11, 500, 1)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = f.liveness.Reaper().Reap(sid, "read_error")
	}()
	go func() {
		defer wg.Done()
		f.clk.set(worldRuntimeBase.Add(PresenceHeartbeatTimeout + time.Second))
		f.liveness.Sweep()
	}()
	wg.Wait()
	eventually(t, "final state clean", func() bool { return f.reg.Len() == 0 })
	if f.downstream.callCount() != 1 {
		t.Fatalf("destructive WorldExit ran %d times, want exactly 1", f.downstream.callCount())
	}
	if len(f.fakeSim.removedIDs()) != 1 {
		t.Fatalf("sim removes = %v, want exactly one", f.fakeSim.removedIDs())
	}
	if f.presence.Len() != 0 {
		t.Fatalf("presence deactivated twice or leaked")
	}
}

// A106: a stale presence with no registry entry is a reported internal
// invariant, never silently accepted as normal cleanup.
func TestStalePresenceWithoutSessionInvariant(t *testing.T) {
	f := newReaperFixture(t)
	if _, err := f.presence.Activate(999, 500, 7, world.CellCoord{}, worldRuntimeBase); err != nil {
		t.Fatal(err)
	}
	f.clk.set(worldRuntimeBase.Add(PresenceHeartbeatTimeout + time.Second))
	orphans := f.liveness.Sweep()
	if len(orphans) != 1 || orphans[0] != 999 {
		t.Fatalf("orphan report = %v, want [999]", orphans)
	}
	if f.presence.Len() != 1 {
		t.Fatalf("orphan presence silently cleaned")
	}
}

// B57: old-transport death racing a same-account takeover serializes
// on the account guard into exactly one legal outcome.
func TestReapConcurrentTakeover(t *testing.T) {
	chars := &multiChars{byAccount: map[int64]int64{11: 500}}
	c := newRealFanoutChain(t, chars, 20)
	reaper, err := NewSessionReaper(c.reg, c.presence, c.runtime)
	if err != nil {
		t.Fatal(err)
	}
	policy := DefaultOutboundPolicy()
	oldSid, _, _, _ := addOutSession(t, c.reg, 11, "sub-a", policy)
	newSid, _, _, _ := addOutSession(t, c.reg, 11, "sub-a2", policy)
	c.source.put(EntityPresentation{EntityID: 1, Position: world.Vec3{X: 4}, Kind: 2, Proto: 7})
	c.source.put(EntityPresentation{EntityID: 2, Position: world.Vec3{X: 4}, Kind: 2, Proto: 7})
	if err := c.enterWorld(t, oldSid); err != nil {
		t.Fatalf("old enter: %v", err)
	}
	frame := encodeEnter(t, 0)
	header, dec, err := proto.DecodeFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = reaper.Reap(oldSid, "disconnect")
	}()
	go func() {
		defer wg.Done()
		sends := &sendRecorder{}
		_ = c.enter.Handle(context.Background(), newSid, header, dec, sends.send)
	}()
	wg.Wait()
	eventually(t, "takeover completes", func() bool {
		_, ok := c.reg.Get(newSid)
		return ok
	})
	// Exactly one active presence epoch for the account's character:
	// the new session's; the old session is fully retired either way.
	if _, err := c.presence.Snapshot(oldSid); !errors.Is(err, ErrPresenceNotFound) {
		t.Fatalf("two active presences after race: old = %v", err)
	}
	snap, err := c.presence.Snapshot(newSid)
	if err != nil {
		t.Fatalf("new session never entered: %v", err)
	}
	if snap.EntityID != 2 {
		t.Fatalf("new entity = %d, want fresh 2", snap.EntityID)
	}
	if s, ok := c.reg.Get(newSid); !ok || s.State != session.StateInWorld || s.CharacterID != 500 {
		t.Fatalf("new binding = %+v", s)
	}
	if _, ok := c.reg.Get(oldSid); ok {
		t.Fatalf("old registry entry survives the race")
	}
	ids := c.fakeSim.removedIDs()
	if len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("old entity removed %v times, want exactly [1]", ids)
	}
	if _, _, err := c.reg.WorldSessionForAccount(11, newSid); err != nil {
		t.Fatalf("world arbitration broken after race: %v", err)
	}
}

// gatedPinger parks inside Ping until released, reproducing the
// Ping-succeeds-concurrently-with-leave interleaving (spec §7.4.7).
type gatedPinger struct {
	entered chan struct{}
	release chan struct{}
}

func (g *gatedPinger) Ping(context.Context) error {
	g.entered <- struct{}{}
	<-g.release
	return nil
}

// A113: a Pong landing after Presence deactivation is normal lifecycle
// convergence — the not-found touch never recreates Presence.
func TestHeartbeatPongAfterDeactivationNoRecreate(t *testing.T) {
	reg := session.NewRegistry()
	presence, _ := NewPresenceRegistry(testPolicy())
	clk := &fakeClock{t: worldRuntimeBase}
	tf := newManualTickerFactory()
	lv := newTestLiveness(t, reg, presence,
		WorldExitFunc(func(context.Context, session.ID, int64, int64) error { return nil }), clk, tf)
	sid := reg.Create(newTakeoverConn())
	if _, err := presence.Activate(sid, 500, 7, world.CellCoord{}, worldRuntimeBase); err != nil {
		t.Fatal(err)
	}
	g := &gatedPinger{entered: make(chan struct{}), release: make(chan struct{})}
	stop := lv.StartPinger(sid, g)
	defer stop()
	tf.last(HeartbeatPingInterval).pulse()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("ping never started")
	}
	// The leave completes while the Pong is in flight.
	if _, err := presence.Deactivate(sid); err != nil {
		t.Fatal(err)
	}
	close(g.release)
	// The touch reports not-found and is ignored: no recreate, ever.
	eventually(t, "ping loop settled", func() bool { return presence.Len() == 0 })
	stop()
	if presence.Len() != 0 {
		t.Fatalf("deactivated presence recreated by late Pong")
	}
}
