package gateway

import (
	"context"
	"net/http/httptest"
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
