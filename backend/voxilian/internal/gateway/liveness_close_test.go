package gateway

import (
	"context"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dlukt/voxilian/internal/session"
	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/world"
)

// ---------------------------------------------------------------------------
// TransportLiveness.Close pinger ownership (spec §7.4.7, v0.3.24).
//
// The pre-correction runtime shared one WaitGroup between the sweep
// and every ping loop but Close only stopped the sweep: a live
// connection's ping loop kept the WaitGroup positive forever. These
// tests prove Close owns and stops every ping loop.
// ---------------------------------------------------------------------------

// stubPinger is a no-op Pinger (no socket needed for lifecycle tests).
type stubPinger struct{}

func (stubPinger) Ping(context.Context) error { return nil }

// closedLiveness wires a TransportLiveness over manual tickers.
func closedLiveness(t *testing.T) (*TransportLiveness, *session.Registry, *PresenceRegistry, *manualTickerFactory) {
	t.Helper()
	reg := session.NewRegistry()
	presence, err := NewPresenceRegistry(testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	exit := WorldExitFunc(func(context.Context, session.ID, int64, int64) error { return nil })
	reaper, err := NewSessionReaper(reg, presence, exit)
	if err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{t: worldRuntimeBase}
	tf := newManualTickerFactory()
	lv, err := NewTransportLiveness(presence, reg, reaper, clk.Now, tf.newTicker)
	if err != nil {
		t.Fatal(err)
	}
	return lv, reg, presence, tf
}

// liveSessionWithPresence registers a session with active Presence so
// ping pulses actually exercise the Ping path.
func liveSessionWithPresence(t *testing.T, reg *session.Registry, presence *PresenceRegistry, charID int64, entityID uint64) session.ID {
	t.Helper()
	sid := reg.Create(nil)
	if _, err := presence.Activate(sid, charID, sim.EntityID(entityID), world.CellCoord{}, worldRuntimeBase); err != nil {
		t.Fatal(err)
	}
	return sid
}

// TestLivenessCloseWithActivePinger (B13): sweep running plus one
// active ping loop with manual tickers. Close (WITHOUT stopping the
// pinger first) must return, stop the sweep, stop the ping loop
// (ticker receiver gone), and leave the returned stop safe and
// idempotent. No socket close is required for Close to return.
func TestLivenessCloseWithActivePinger(t *testing.T) {
	lv, reg, presence, tf := closedLiveness(t)
	lv.Start()
	sid := liveSessionWithPresence(t, reg, presence, 500, 7)
	stop := lv.StartPinger(sid, stubPinger{})
	pingTick := tf.last(HeartbeatPingInterval)
	if pingTick == nil {
		t.Fatalf("no ping ticker registered")
	}
	// Prove the loop is alive: it consumes a pulse.
	pingTick.pulse()

	done := make(chan struct{})
	go func() {
		defer close(done)
		lv.Close()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("Close hung with an active ping loop (shared-WaitGroup bug)")
	}
	// Sweep loop exited.
	if tf.last(StaleSweepInterval).tryPulse() {
		t.Fatalf("sweep loop still running after Close")
	}
	// Ping loop exited: no receiver remains.
	if pingTick.tryPulse() {
		t.Fatalf("ping loop still running after Close")
	}
	// Returned per-connection stop remains safe/idempotent afterwards.
	stop()
	stop()
}

// TestLivenessCloseManyPingers (B14): 32 active ping loops all stop
// on Close — no leak, no double-close panic, no hanging WaitGroup.
func TestLivenessCloseManyPingers(t *testing.T) {
	lv, reg, presence, tf := closedLiveness(t)
	lv.Start()
	var stops []func()
	for i := 0; i < 32; i++ {
		sid := liveSessionWithPresence(t, reg, presence, int64(500+i), uint64(100+i))
		stops = append(stops, lv.StartPinger(sid, stubPinger{}))
	}
	if got := tf.count(HeartbeatPingInterval); got != 32 {
		t.Fatalf("ping tickers = %d, want 32", got)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		lv.Close()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("Close hung with 32 active ping loops")
	}
	tf.mu.Lock()
	tickers := append([]*manualTicker(nil), tf.tickers[HeartbeatPingInterval]...)
	tf.mu.Unlock()
	for i, tk := range tickers {
		if tk.tryPulse() {
			t.Fatalf("ping loop %d still running after Close", i)
		}
	}
	for _, stop := range stops {
		stop()
	}
}

// TestLivenessStartPingerAfterClose (B15): after Close, StartPinger
// starts no ticker, starts no goroutine, and returns a safe
// idempotent stop.
func TestLivenessStartPingerAfterClose(t *testing.T) {
	lv, reg, _, tf := closedLiveness(t)
	lv.Start()
	lv.Close()
	before := tf.count(HeartbeatPingInterval)
	sid := reg.Create(nil)
	stop := lv.StartPinger(sid, stubPinger{})
	if stop == nil {
		t.Fatalf("nil stop after Close")
	}
	stop()
	stop()
	if got := tf.count(HeartbeatPingInterval); got != before {
		t.Fatalf("ping tickers = %d after Close, want %d (no new loop)", got, before)
	}
}

// TestLivenessStopVsCloseRace (B16): the connection-local stop and
// the global Close raced over many iterations — no panic, no
// deadlock, no race (run with -race), exactly one loop lifetime.
func TestLivenessStopVsCloseRace(t *testing.T) {
	for i := 0; i < 50; i++ {
		lv, reg, presence, tf := closedLiveness(t)
		lv.Start()
		sid := liveSessionWithPresence(t, reg, presence, 600, 700)
		stop := lv.StartPinger(sid, stubPinger{})
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			stop()
		}()
		go func() {
			defer wg.Done()
			<-start
			lv.Close()
		}()
		close(start)
		waited := make(chan struct{})
		go func() {
			defer close(waited)
			wg.Wait()
		}()
		select {
		case <-waited:
		case <-time.After(10 * time.Second):
			t.Fatalf("iter %d: stop-vs-Close hung", i)
		}
		// Closed liveness never restarts; exactly one ping ticker
		// was ever created for this iteration.
		if got := tf.count(HeartbeatPingInterval); got != 1 {
			t.Fatalf("iter %d: ping tickers = %d, want exactly 1", i, got)
		}
		lv.Close()
	}
}

// TestServerCloseWithLiveConnection (B17): a real WebSocket through a
// Server with Liveness configured stays connected while
// server.Close() is called. The call MUST return without requiring
// the WebSocket to disconnect first; the connection is closed
// explicitly afterwards for cleanup. This catches the shared
// WaitGroup bug end to end.
func TestServerCloseWithLiveConnection(t *testing.T) {
	reg := session.NewRegistry()
	presence, err := NewPresenceRegistry(testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	exit := WorldExitFunc(func(context.Context, session.ID, int64, int64) error { return nil })
	reaper, err := NewSessionReaper(reg, presence, exit)
	if err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{t: worldRuntimeBase}
	tf := newManualTickerFactory()
	lv, err := NewTransportLiveness(presence, reg, reaper, clk.Now, tf.newTicker)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(ServerDeps{
		Registry: reg,
		Liveness: lv,
		Tick:     func() uint32 { return 1000 },
		Now:      clk.Now,
	})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = client.CloseNow() }()
	// The server side registered exactly one session with a ping loop.
	deadline := time.Now().Add(5 * time.Second)
	for tf.count(HeartbeatPingInterval) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("server never started the per-connection ping loop")
		}
		time.Sleep(time.Millisecond)
	}
	// The connection stays alive: Close must still return.
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		srv.Close()
	}()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatalf("Server.Close hung with a live WebSocket (ping loop not owned)")
	}
	srv.Close() // idempotent
}

// ---------------------------------------------------------------------------
// TransportLiveness.Start sweep lifecycle ordering (spec §7.4.7,
// v0.3.24 corrective closure).
//
// Start's open/closed decision, stopSweep publication, WaitGroup Add,
// and sweep launch must form one lifecycle transition under the
// mutex. Otherwise Close could snapshot stopSweep, close it, Wait on
// a zero count, and return — while a losing Start still runs wg.Add
// and launches a sweep on the closed channel afterwards.
// ---------------------------------------------------------------------------

// waitSweepTicker spins (no sleeps) until the sweep loop creates its
// ticker, proving the goroutine reached sweepLoop.
func waitSweepTicker(t *testing.T, tf *manualTickerFactory) *manualTicker {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if tk := tf.last(StaleSweepInterval); tk != nil {
			return tk
		}
		if time.Now().After(deadline) {
			t.Fatalf("sweep ticker never created")
		}
		runtime.Gosched()
	}
}

// waitSweepLive spins (no sleeps) until the sweep loop is parked in
// its select (a pulse is consumed), proving exactly one live loop.
func waitSweepLive(t *testing.T, tk *manualTicker) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if tk.tryPulse() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("sweep loop never became live")
		}
		runtime.Gosched()
	}
}

// assertNoSweepLive proves no sweep ticker has a live receiver. Valid
// only after Close has returned (Wait guarantees an accounted loop
// exited), so a single non-blocking pass is deterministic.
func assertNoSweepLive(t *testing.T, tf *manualTickerFactory) {
	t.Helper()
	tf.mu.Lock()
	tickers := append([]*manualTicker(nil), tf.tickers[StaleSweepInterval]...)
	tf.mu.Unlock()
	for i, tk := range tickers {
		if tk.tryPulse() {
			t.Fatalf("sweep ticker %d still has a live receiver after Close", i)
		}
	}
}

func livenessIsClosed(lv *TransportLiveness) bool {
	lv.mu.Lock()
	defer lv.mu.Unlock()
	return lv.closed
}

// TestLivenessStartCloseDeterministic (A): Start -> Close leaves
// exactly one sweep that Close waits out; no receiver survives.
func TestLivenessStartCloseDeterministic(t *testing.T) {
	lv, _, _, tf := closedLiveness(t)
	lv.Start()
	// The ticker is created inside the sweep goroutine: wait for it
	// to prove the single loop exists, then re-Start idempotently.
	waitSweepTicker(t, tf)
	lv.Start() // idempotent: still one sweep
	if got := tf.count(StaleSweepInterval); got != 1 {
		t.Fatalf("sweep tickers = %d after Start x2, want exactly 1", got)
	}
	waitSweepLive(t, tf.last(StaleSweepInterval))
	done := make(chan struct{})
	go func() {
		defer close(done)
		lv.Close()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("Close hung after Start")
	}
	assertNoSweepLive(t, tf)
	if got := tf.count(StaleSweepInterval); got != 1 {
		t.Fatalf("sweep tickers = %d after Close, want 1 (no new loop)", got)
	}
	lv.Close() // idempotent
}

// TestLivenessCloseStartNoRestart (B): Close -> Start creates no
// ticker and no goroutine.
func TestLivenessCloseStartNoRestart(t *testing.T) {
	lv, _, _, tf := closedLiveness(t)
	lv.Close()
	before := tf.count(StaleSweepInterval)
	lv.Start()
	lv.Start()
	if got := tf.count(StaleSweepInterval); got != before {
		t.Fatalf("sweep tickers = %d after Close->Start, want %d", got, before)
	}
	assertNoSweepLive(t, tf)
	if !livenessIsClosed(lv) {
		t.Fatalf("liveness not marked closed")
	}
	lv.Close() // idempotent
}

// TestLivenessConcurrentStartSingleSweep (C): concurrent Start x 32
// yields exactly one sweep loop.
func TestLivenessConcurrentStartSingleSweep(t *testing.T) {
	const n = 32
	lv, _, _, tf := closedLiveness(t)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			<-start
			lv.Start()
		}()
	}
	close(start)
	waited := make(chan struct{})
	go func() {
		defer close(waited)
		wg.Wait()
	}()
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatalf("concurrent Start hung")
	}
	// No Close ran here, so exactly one Start won and its goroutine
	// must materialize exactly one ticker; all Starts already
	// returned so the count cannot grow beyond that.
	waitSweepTicker(t, tf)
	if got := tf.count(StaleSweepInterval); got != 1 {
		t.Fatalf("sweep tickers = %d after concurrent Start x%d, want exactly 1", got, n)
	}
	waitSweepLive(t, tf.last(StaleSweepInterval))
	lv.Close()
	assertNoSweepLive(t, tf)
	lv.Close() // idempotent
}

// TestLivenessConcurrentStartCloseQuiesces (D): concurrent Start x 32
// + Close. After all operations return: zero live sweep loops, zero
// restart ability, no panic, no WaitGroup misuse.
func TestLivenessConcurrentStartCloseQuiesces(t *testing.T) {
	const n = 32
	lv, _, _, tf := closedLiveness(t)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(n + 1)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			<-start
			lv.Start()
		}()
	}
	go func() {
		defer wg.Done()
		<-start
		lv.Close()
	}()
	close(start)
	waited := make(chan struct{})
	go func() {
		defer close(waited)
		wg.Wait()
	}()
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatalf("concurrent Start+Close hung (WaitGroup misuse?)")
	}
	// At most one sweep was ever created, none is live, restart is
	// impossible, and Close stays idempotent.
	if got := tf.count(StaleSweepInterval); got > 1 {
		t.Fatalf("sweep tickers = %d, want at most 1", got)
	}
	assertNoSweepLive(t, tf)
	lv.Close()
	before := tf.count(StaleSweepInterval)
	lv.Start()
	if got := tf.count(StaleSweepInterval); got != before {
		t.Fatalf("sweep restarted after Close: %d -> %d", before, got)
	}
	if !livenessIsClosed(lv) {
		t.Fatalf("liveness not marked closed")
	}
}

// TestLivenessStartVsCloseRace races Start against Close from a
// common barrier over many iterations. After BOTH return: the
// liveness is closed, a later Start does nothing, no sweep ticker
// has a live receiver, no sweep goroutine survives (Close's Wait
// covered every accounted Add), and Close remains idempotent. No
// sleeps: barrier + Wait channels and the manual ticker seam only
// (10 s arms are hang guards, never synchronization).
func TestLivenessStartVsCloseRace(t *testing.T) {
	const iters = 1000
	for i := 0; i < iters; i++ {
		lv, _, _, tf := closedLiveness(t)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			lv.Start()
		}()
		go func() {
			defer wg.Done()
			<-start
			lv.Close()
		}()
		close(start)
		waited := make(chan struct{})
		go func() {
			defer close(waited)
			wg.Wait()
		}()
		select {
		case <-waited:
		case <-time.After(10 * time.Second):
			t.Fatalf("iter %d: Start-vs-Close hung (WaitGroup Add raced Wait?)", i)
		}
		if !livenessIsClosed(lv) {
			t.Fatalf("iter %d: liveness not closed after Start-vs-Close", i)
		}
		if got := tf.count(StaleSweepInterval); got > 1 {
			t.Fatalf("iter %d: sweep tickers = %d, want at most 1", i, got)
		}
		assertNoSweepLive(t, tf)
		before := tf.count(StaleSweepInterval)
		lv.Start()
		if got := tf.count(StaleSweepInterval); got != before {
			t.Fatalf("iter %d: Start after Close created a sweep", i)
		}
		lv.Close() // idempotent, must not hang or panic
		assertNoSweepLive(t, tf)
	}
}
