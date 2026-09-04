package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dlukt/voxilian/internal/proto"
	"github.com/dlukt/voxilian/internal/session"
)

// noBuildConn accepts WriteBinary but never invokes the builder — it
// proves sequence allocation cannot happen before writer ownership.
type noBuildConn struct{}

func (c *noBuildConn) WriteBinary(context.Context, session.BinaryFrameBuilder) error { return nil }
func (c *noBuildConn) Close(string) error                                            { return nil }
func (c *noBuildConn) CloseNow() error                                               { return nil }

// gateWriterConn reproduces the production writer contract for the
// shared sender: one context-aware gate, build only inside it, writes
// recorded in physical order.
type gateWriterConn struct {
	gate   chan struct{}
	mu     sync.Mutex
	frames [][]byte
}

func newGateWriterConn() *gateWriterConn {
	return &gateWriterConn{gate: make(chan struct{}, 1)}
}

func (c *gateWriterConn) WriteBinary(ctx context.Context, build session.BinaryFrameBuilder) error {
	select {
	case c.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-c.gate }()
	frame, err := build()
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.frames = append(c.frames, frame)
	c.mu.Unlock()
	return nil
}

func (c *gateWriterConn) Close(string) error { return nil }

// CloseNow must not wait for the gate; it records nothing.
func (c *gateWriterConn) CloseNow() error { return nil }

func (c *gateWriterConn) recorded() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.frames...)
}

// TestWSConnectionWriteBinaryGateContextAware proves the production
// writer gate is context-aware: a second WriteBinary whose context is
// cancelled while another writer holds the gate returns
// context.Canceled without ever invoking its builder. Neither builder
// touches the underlying websocket, so no live socket is needed.
func TestWSConnectionWriteBinaryGateContextAware(t *testing.T) {
	w := newWSConnection(nil)
	sentinel := errors.New("builder sentinel")
	started := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		err := w.WriteBinary(context.Background(), func() ([]byte, error) {
			close(started)
			<-release
			return nil, sentinel
		})
		firstDone <- err
	}()
	<-started // the first writer owns the gate inside its builder

	secondInvoked := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	go func() {
		err := w.WriteBinary(ctx, func() ([]byte, error) {
			select {
			case secondInvoked <- struct{}{}:
			default:
			}
			return []byte{1}, nil
		})
		secondDone <- err
	}()
	cancel()
	if err := <-secondDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("second WriteBinary err = %v, want context.Canceled", err)
	}
	select {
	case <-secondInvoked:
		t.Fatal("second builder invoked without writer ownership")
	default:
	}
	close(release)
	if err := <-firstDone; !errors.Is(err, sentinel) {
		t.Fatalf("first WriteBinary err = %v, want the builder sentinel", err)
	}
}

// dialServerSideConn opens a real local WebSocket pair and returns the
// SERVER-side connection (what ServeHTTP would wrap).
func dialServerSideConn(t *testing.T) *websocket.Conn {
	t.Helper()
	ready := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ready <- c
		for {
			if _, _, err := c.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.CloseNow() })
	select {
	case c := <-ready:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("server side never accepted")
		return nil
	}
}

// TestWSConnectionCloseNowBypassesWriterGate proves forced close does
// NOT serialize behind the application writer: with a builder blocked
// inside the gate, CloseNow still completes immediately (it may
// interrupt a stuck write).
func TestWSConnectionCloseNowBypassesWriterGate(t *testing.T) {
	w := newWSConnection(dialServerSideConn(t))
	started := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = w.WriteBinary(context.Background(), func() ([]byte, error) {
			close(started)
			<-release
			return []byte{9, 9, 9}, nil
		})
	}()
	<-started // the writer gate is held by the blocked builder

	done := make(chan error, 1)
	go func() { done <- w.CloseNow() }()
	select {
	case <-done:
		// Completed while the gate was held: forced close is not
		// serialized behind application writes.
	case <-time.After(5 * time.Second):
		t.Fatal("CloseNow blocked behind the application writer gate")
	}
	close(release)
}

// TestSendSessionFrameAllocatesSeqInsideWriter is the deterministic
// regression for the seq-vs-write-order latent issue: a connection
// that never runs the builder must never consume a sequence number.
// If NextServerSeq moved back outside WriteBinary, the sequence would
// be allocated here and this test fails.
func TestSendSessionFrameAllocatesSeqInsideWriter(t *testing.T) {
	reg := session.NewRegistry()
	conn := &noBuildConn{}
	id := reg.Create(conn)
	if err := sendSessionFrame(context.Background(), reg, func() uint32 { return 1 },
		id, proto.OpcodeError, proto.MessageVersion1,
		func(e *proto.Encoder) error {
			proto.ErrorMessage{Code: 1, Message: "x"}.Encode(e)
			return nil
		}); err != nil {
		t.Fatalf("sendSessionFrame: %v", err)
	}
	seq, err := reg.NextServerSeq(id)
	if err != nil || seq != 1 {
		t.Fatalf("NextServerSeq = %d,%v want 1,nil — sequence allocation escaped the writer slot", seq, err)
	}
}

// TestSendSessionFrameRequiresLiveConnection covers the degenerate
// sender inputs: a vanished session and a session with no connection.
func TestSendSessionFrameRequiresLiveConnection(t *testing.T) {
	reg := session.NewRegistry()
	send := func(id session.ID) error {
		return sendSessionFrame(context.Background(), reg, func() uint32 { return 0 },
			id, proto.OpcodeError, proto.MessageVersion1,
			func(e *proto.Encoder) error { return nil })
	}
	if err := send(9999); err == nil {
		t.Error("vanished session accepted")
	}
	bare := reg.Create(nil)
	if err := send(bare); err == nil {
		t.Error("connection-less session accepted")
	}
}

// TestSendSessionFrameConcurrentWireOrderMonotonic runs concurrent
// sends through the shared sender over a production-shaped serialized
// connection and asserts the physical write order is exactly
// seq 1..N — never N+1 before N — with no lost or duplicated frames.
func TestSendSessionFrameConcurrentWireOrderMonotonic(t *testing.T) {
	reg := session.NewRegistry()
	conn := newGateWriterConn()
	id := reg.Create(conn)
	const workers, per = 25, 4
	tick := func() uint32 { return 31337 }
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				if err := sendSessionFrame(context.Background(), reg, tick, id,
					proto.OpcodeError, proto.MessageVersion1,
					func(e *proto.Encoder) error {
						proto.ErrorMessage{Code: 1, Message: "order"}.Encode(e)
						return nil
					}); err != nil {
					t.Errorf("send: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	frames := conn.recorded()
	if len(frames) != workers*per {
		t.Fatalf("frames = %d, want %d", len(frames), workers*per)
	}
	for i, frame := range frames {
		header, _, err := proto.DecodeFrame(frame)
		if err != nil {
			t.Fatalf("frame %d decode: %v", i, err)
		}
		if header.Seq != uint32(i+1) {
			t.Fatalf("physical write %d carries seq %d, want %d — seq allocation escaped writer serialization", i, header.Seq, i+1)
		}
		if header.Tick != 31337 {
			t.Fatalf("frame %d tick = %d, want injected tick", i, header.Tick)
		}
	}
}

// TestServerDeadlineAndNormalWritesShareWriter runs a normal read-loop
// send and the authorization-deadline callback write concurrently: both
// must land on the SAME connection writer as one ordered stream with
// unique ascending sequences and the injected tick. (Before T4b the
// deadline write used a separate connAuth mutex.)
func TestServerDeadlineAndNormalWritesShareWriter(t *testing.T) {
	reg := session.NewRegistry()
	conn := newGateWriterConn()
	id := reg.Create(conn)
	// Expired more than ReauthGrace ago so the fired deadline is past.
	exp := time.Now().Add(-2 * time.Minute)
	if err := reg.Authenticate(id, "sub", 1, exp); err != nil {
		t.Fatal(err)
	}
	var fire func()
	s := NewServer(ServerDeps{
		Registry: reg,
		Tick:     func() uint32 { return 77 },
		Now:      func() time.Time { return time.Now() },
		Schedule: func(at time.Time, fn func()) CancelFunc { fire = fn; return func() {} },
	})
	ca := &connAuth{}
	s.armDeadline(ca, id, exp)
	if fire == nil {
		t.Fatal("deadline never scheduled")
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := s.send(context.Background(), id, proto.OpcodeWelcome, proto.MessageVersion1,
			func(e *proto.Encoder) error { return nil }); err != nil {
			t.Errorf("normal send: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		fire() // the deadline callback writes its 202 session_expired
	}()
	wg.Wait()

	frames := conn.recorded()
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2 (deadline + normal on ONE writer)", len(frames))
	}
	seen := map[uint32]bool{}
	sawExpired := false
	var lastSeq uint32
	for i, frame := range frames {
		header, payload, err := proto.DecodeFrame(frame)
		if err != nil {
			t.Fatalf("frame %d decode: %v", i, err)
		}
		if seen[header.Seq] {
			t.Fatalf("duplicate seq %d — two independent writers allocated sequences", header.Seq)
		}
		seen[header.Seq] = true
		if i > 0 && header.Seq < lastSeq {
			t.Fatalf("seq order %d after %d on the shared writer", header.Seq, lastSeq)
		}
		lastSeq = header.Seq
		if header.Tick != 77 {
			t.Errorf("frame %d tick = %d, want injected 77", i, header.Tick)
		}
		if header.Opcode == proto.OpcodeError {
			msg, err := proto.DecodeErrorMessage(payload)
			if err != nil {
				t.Fatalf("decode deadline 202: %v", err)
			}
			if msg.Code != proto.ErrorCodeSessionExpired {
				t.Errorf("deadline code = %d, want session_expired", msg.Code)
			}
			sawExpired = true
		}
	}
	if !sawExpired {
		t.Error("deadline 202 session_expired never reached the shared writer")
	}
	if len(seen) != 2 {
		t.Errorf("distinct sequences = %d, want 2", len(seen))
	}
}
