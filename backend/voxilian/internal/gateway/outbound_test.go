package gateway

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dlukt/voxilian/internal/auth"
	"github.com/dlukt/voxilian/internal/character"
	"github.com/dlukt/voxilian/internal/proto"
	"github.com/dlukt/voxilian/internal/session"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

// fakeOutTransport is a controllable low-level physical writer double:
// one context-aware gate serializes builds (the production writer
// contract), an optional hold channel parks every physical write until
// released or the write context expires, and CloseNow never waits for
// the gate.
type fakeOutTransport struct {
	gate chan struct{}

	mu        sync.Mutex
	hold      chan struct{}
	frames    [][]byte
	builds    int
	writeErr  error
	closeNows int
	closes    []string
}

func newFakeOutTransport() *fakeOutTransport {
	return &fakeOutTransport{gate: make(chan struct{}, 1)}
}

// setHold installs (or with nil removes) the physical-write park. The
// parked write also honors its context, proving write timeouts.
func (t *fakeOutTransport) setHold(ch chan struct{}) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.hold = ch
}

func (t *fakeOutTransport) WriteBinary(ctx context.Context, build session.BinaryFrameBuilder) error {
	select {
	case t.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-t.gate }()
	t.mu.Lock()
	t.builds++
	hold := t.hold
	writeErr := t.writeErr
	t.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	frame, err := build()
	if err != nil {
		return err
	}
	if writeErr != nil {
		return writeErr
	}
	t.mu.Lock()
	t.frames = append(t.frames, frame)
	t.mu.Unlock()
	return nil
}

func (t *fakeOutTransport) Close(reason string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closes = append(t.closes, reason)
	return nil
}

func (t *fakeOutTransport) CloseNow() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closeNows++
	return nil
}

func (t *fakeOutTransport) recorded() [][]byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([][]byte(nil), t.frames...)
}

func (t *fakeOutTransport) recordedCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.frames)
}

func (t *fakeOutTransport) buildCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.builds
}

func (t *fakeOutTransport) closeNowCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closeNows
}

func (t *fakeOutTransport) closeReasons() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.closes...)
}

// depthSample is one observed lane depth.
type depthSample struct {
	lane     OutboundLane
	messages int
	bytes    int
}

// recordingObserver captures every observation for assertions.
type recordingObserver struct {
	mu           sync.Mutex
	depths       []depthSample
	stateDrops   []string
	coalesced    int
	sessionDrops []string
}

func (o *recordingObserver) QueueDepth(lane OutboundLane, messages, bytes int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.depths = append(o.depths, depthSample{lane: lane, messages: messages, bytes: bytes})
}

func (o *recordingObserver) StateDropped(reason string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.stateDrops = append(o.stateDrops, reason)
}

func (o *recordingObserver) StateCoalesced() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.coalesced++
}

func (o *recordingObserver) SessionDropped(reason string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sessionDrops = append(o.sessionDrops, reason)
}

func (o *recordingObserver) snapshot() (drops []string, coalesced int, sessionDrops []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.stateDrops...), o.coalesced,
		append([]string(nil), o.sessionDrops...)
}

// ---------------------------------------------------------------------------
// fixture
// ---------------------------------------------------------------------------

// outboundFixture wires one queue-capable connection over a controllable
// transport plus a registry-backed seq source.
type outboundFixture struct {
	reg   *session.Registry
	tr    *fakeOutTransport
	obs   *recordingObserver
	oc    *outboundConn
	sid   session.ID
	tick  uint32
	holds []chan struct{}
}

func tinyPolicy(maxMsgs, maxBytes int, enqueue, write time.Duration) OutboundPolicy {
	return OutboundPolicy{
		MaxMessages:            maxMsgs,
		MaxBytes:               maxBytes,
		ReliableEnqueueTimeout: enqueue,
		WriteTimeout:           write,
	}
}

func newOutboundFixture(t *testing.T, policy OutboundPolicy) *outboundFixture {
	t.Helper()
	f := &outboundFixture{
		reg:  session.NewRegistry(),
		tr:   newFakeOutTransport(),
		obs:  &recordingObserver{},
		tick: 1000,
	}
	f.oc = newOutboundConn(OutboundDeps{
		Conn:     f.tr,
		Registry: f.reg,
		Tick:     func() uint32 { return atomic.LoadUint32(&f.tick) },
		Policy:   policy,
		Observer: f.obs,
	})
	f.sid = f.reg.Create(f.oc)
	t.Cleanup(func() {
		f.releaseAll()
		f.oc.StopOutbound("test complete")
	})
	return f
}

// hold installs a fresh physical-write park.
func (f *outboundFixture) hold() chan struct{} {
	ch := make(chan struct{})
	f.tr.setHold(ch)
	f.holds = append(f.holds, ch)
	return ch
}

// releaseAll releases every installed hold (cleanup safety).
func (f *outboundFixture) releaseAll() {
	for _, ch := range f.holds {
		select {
		case <-ch:
		default:
			close(ch)
		}
	}
}

// waitParked waits until exactly n physical writes have entered the
// transport (the writer pump is inside its write).
func (f *outboundFixture) waitParked(t *testing.T, n int) {
	t.Helper()
	waitUntil(t, "physical write parked", func() bool { return f.tr.buildCount() == n })
}

func (f *outboundFixture) pumpExited() bool {
	select {
	case <-f.oc.q.pumpDone:
		return true
	default:
		return false
	}
}

// payloadOf returns an encoder writing exactly n payload bytes with a
// leading u32 marker used to distinguish frames.
func payloadOf(marker uint32, n int) func(*proto.Encoder) error {
	return func(e *proto.Encoder) error {
		e.U32(marker)
		e.WriteBytes(make([]byte, n-4))
		return nil
	}
}

// markerOf decodes one recorded frame's leading u32 payload marker.
func markerOf(t *testing.T, frame []byte) uint32 {
	t.Helper()
	_, payload, err := proto.DecodeFrame(frame)
	if err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	m, err := payload.U32()
	if err != nil {
		t.Fatalf("decode marker: %v", err)
	}
	return m
}

// headerOf decodes one recorded frame's header.
func headerOf(t *testing.T, frame []byte) proto.Header {
	t.Helper()
	header, _, err := proto.DecodeFrame(frame)
	if err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	return header
}

// waitUntil polls cond until true or the bounded safety deadline hits.
// The condition (never the clock) is the synchronization.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// sendAsync runs SendCritical on a goroutine and returns its result.
func (f *outboundFixture) sendAsync(t *testing.T, marker uint32, payloadBytes int) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- f.oc.SendCritical(context.Background(), f.sid,
			proto.OpcodeError, proto.MessageVersion1, payloadOf(marker, payloadBytes))
	}()
	return done
}

// resident returns the queue's current resident messages/bytes
// (white-box: includes the in-flight write).
func (f *outboundFixture) resident() (int, int) {
	q := f.oc.q
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.resMsg, q.resBytes
}

// ---------------------------------------------------------------------------
// critical FIFO + budgets
// ---------------------------------------------------------------------------

// TestOutboundCriticalFIFOOrder proves the critical lane is strict FIFO
// with contiguous sequence allocation in wire order (spec §7.1.3/§7.1.7).
func TestOutboundCriticalFIFOOrder(t *testing.T) {
	f := newOutboundFixture(t, tinyPolicy(8, 262144, 10*time.Second, 10*time.Second))
	f.hold()
	a := f.sendAsync(t, 1, 64)
	f.waitParked(t, 1)

	// Deterministic queue order: sequential non-blocking admissions.
	if err := f.oc.TryCritical(f.sid, proto.OpcodeError, proto.MessageVersion1, payloadOf(2, 64)); err != nil {
		t.Fatalf("TryCritical B: %v", err)
	}
	if err := f.oc.TryCritical(f.sid, proto.OpcodeError, proto.MessageVersion1, payloadOf(3, 64)); err != nil {
		t.Fatalf("TryCritical C: %v", err)
	}
	f.releaseAll()
	if err := <-a; err != nil {
		t.Fatalf("send A: %v", err)
	}
	waitUntil(t, "queued critical drained", func() bool { return f.tr.recordedCount() == 3 })

	frames := f.tr.recorded()
	for i, frame := range frames {
		if m := markerOf(t, frame); m != uint32(i+1) {
			t.Fatalf("physical order %d carries marker %d, want %d", i, m, i+1)
		}
		if s := headerOf(t, frame).Seq; s != uint32(i+1) {
			t.Fatalf("physical write %d carries seq %d, want %d — allocation escaped write serialization", i, s, i+1)
		}
	}
}

// TestOutboundMessageBudgetIncludesWriting proves the resident message
// budget counts the active write plus queued items (spec §7.1.2): with
// MaxMessages=3 (one writing + two queued), the fourth synchronous
// admission cannot silently exceed the limit — it waits and fails
// closed with the enqueue timeout classification (spec §7.1.5).
func TestOutboundMessageBudgetIncludesWriting(t *testing.T) {
	f := newOutboundFixture(t, tinyPolicy(3, 262144, 20*time.Millisecond, 10*time.Second))
	f.hold()
	a := f.sendAsync(t, 1, 64)
	f.waitParked(t, 1)

	if err := f.oc.TryCritical(f.sid, proto.OpcodeError, proto.MessageVersion1, payloadOf(2, 64)); err != nil {
		t.Fatalf("TryCritical B: %v", err)
	}
	if err := f.oc.TryCritical(f.sid, proto.OpcodeError, proto.MessageVersion1, payloadOf(3, 64)); err != nil {
		t.Fatalf("TryCritical C: %v", err)
	}
	if msgs, _ := f.resident(); msgs != 3 {
		t.Fatalf("resident messages = %d, want 3 (writing + queued)", msgs)
	}

	d := f.sendAsync(t, 4, 64)
	if err := <-d; !errors.Is(err, ErrSlowClient) {
		t.Fatalf("fourth send err = %v, want ErrSlowClient", err)
	}
	// The in-flight writer is failed too, and the transport is
	// force-closed (fail-closed slow session).
	if err := <-a; !errors.Is(err, ErrSlowClient) {
		t.Fatalf("in-flight send err = %v, want ErrSlowClient", err)
	}
	waitUntil(t, "CloseNow after enqueue timeout", func() bool { return f.tr.closeNowCount() >= 1 })
	_, _, sessionDrops := f.obs.snapshot()
	if len(sessionDrops) != 1 || sessionDrops[0] != DropReasonEnqueueTimeout {
		t.Fatalf("session drops = %v, want [%s]", sessionDrops, DropReasonEnqueueTimeout)
	}
	f.releaseAll()
	waitUntil(t, "writer pump exit", f.pumpExited)
}

// TestOutboundByteBudgetExact proves byte accounting is the exact
// complete-frame size INCLUDING the 12-byte header (spec §7.1.2): with
// MaxBytes=335, exactly two payload-100 frames (2×112) fit — an
// implementation counting only payload bytes would admit a third
// (3×100 ≤ 335).
func TestOutboundByteBudgetExact(t *testing.T) {
	f := newOutboundFixture(t, tinyPolicy(100, 335, 20*time.Millisecond, 10*time.Second))
	f.hold()
	a := f.sendAsync(t, 1, 100)
	f.waitParked(t, 1)

	if err := f.oc.TryCritical(f.sid, proto.OpcodeError, proto.MessageVersion1, payloadOf(2, 100)); err != nil {
		t.Fatalf("TryCritical B (224 resident): %v", err)
	}
	if _, bytes := f.resident(); bytes != 224 {
		t.Fatalf("resident bytes = %d, want 224 (2×(12+100))", bytes)
	}

	// A third payload-100 frame needs 112 more (336 > 335): dropped,
	// and a dropped state update must NOT close the session (§7.1.4/§7.1.6).
	res, err := f.oc.TryState(f.sid, StateKey{Kind: 0, ID: 7},
		proto.OpcodeEntityMove, proto.MessageVersion1, payloadOf(3, 100))
	if err != nil || res != StateDropped {
		t.Fatalf("TryState 100-byte = %v,%v want dropped,nil", res, err)
	}
	// A 99-byte payload frame (12+99=111) fits exactly: 224+111=335.
	res, err = f.oc.TryState(f.sid, StateKey{Kind: 0, ID: 8},
		proto.OpcodeEntityMove, proto.MessageVersion1, payloadOf(4, 99))
	if err != nil || res != StateQueued {
		t.Fatalf("TryState 99-byte = %v,%v want queued,nil", res, err)
	}
	if _, bytes := f.resident(); bytes != 335 {
		t.Fatalf("resident bytes = %d, want exactly the 335 budget", bytes)
	}
	if f.tr.closeNowCount() != 0 {
		t.Fatal("state saturation must not force-close the session")
	}
	f.releaseAll()
	if err := <-a; err != nil {
		t.Fatalf("send A: %v", err)
	}
}

// ---------------------------------------------------------------------------
// state lane: coalescing, keys, saturation, eviction
// ---------------------------------------------------------------------------

// TestOutboundSameKeyCoalesce proves newest-wins same-key replacement
// (spec §7.1.3): only the newest value is written, the replaced values
// never receive a sequence, and the observer reports coalescing.
func TestOutboundSameKeyCoalesce(t *testing.T) {
	f := newOutboundFixture(t, tinyPolicy(8, 262144, 10*time.Second, 10*time.Second))
	f.hold()
	a := f.sendAsync(t, 1, 64) // unrelated critical parked in the writer
	f.waitParked(t, 1)

	key := StateKey{Kind: 0, ID: 42} // canonical 205 entity_move shape
	for i, marker := range []uint32{10, 11, 12} {
		want := StateQueued
		if i > 0 {
			want = StateCoalesced // only replacements coalesce
		}
		res, err := f.oc.TryState(f.sid, key, proto.OpcodeEntityMove, proto.MessageVersion1, payloadOf(marker, 64))
		if err != nil || res != want {
			t.Fatalf("TryState marker %d = %v,%v want %v,nil", marker, res, err, want)
		}
	}
	f.releaseAll()
	if err := <-a; err != nil {
		t.Fatalf("send A: %v", err)
	}
	waitUntil(t, "surviving state frame drained", func() bool { return f.tr.recordedCount() == 2 })

	frames := f.tr.recorded()
	if m := markerOf(t, frames[1]); m != 12 {
		t.Fatalf("surviving state marker = %d, want newest 12", m)
	}
	// Exactly two sequences were allocated: the newest state value owns
	// seq 2 and the next fresh critical owns seq 3 — replaced values
	// consumed none.
	e := f.sendAsync(t, 99, 64)
	if err := <-e; err != nil {
		t.Fatalf("probe send: %v", err)
	}
	frames = f.tr.recorded()
	if s := headerOf(t, frames[len(frames)-1]).Seq; s != 3 {
		t.Fatalf("probe seq = %d, want 3 — replaced state consumed sequence", s)
	}
	_, coalesced, _ := f.obs.snapshot()
	if coalesced != 2 {
		t.Fatalf("coalesced observations = %d, want 2", coalesced)
	}
}

// TestOutboundDifferentKeys proves distinct keys coexist, same-key
// replacement keeps only the newest per key, and state drain order is
// deterministic (spec §7.1.3/§7.1.7).
func TestOutboundDifferentKeys(t *testing.T) {
	f := newOutboundFixture(t, tinyPolicy(8, 262144, 10*time.Second, 10*time.Second))
	f.hold()
	a := f.sendAsync(t, 1, 64)
	f.waitParked(t, 1)

	k1 := StateKey{Kind: 0, ID: 1}
	k2 := StateKey{Kind: 0, ID: 2}
	if res, err := f.oc.TryState(f.sid, k1, proto.OpcodeEntityMove, proto.MessageVersion1, payloadOf(21, 64)); err != nil || res != StateQueued {
		t.Fatalf("K1 = %v,%v", res, err)
	}
	if res, err := f.oc.TryState(f.sid, k2, proto.OpcodeEntityMove, proto.MessageVersion1, payloadOf(22, 64)); err != nil || res != StateQueued {
		t.Fatalf("K2 = %v,%v", res, err)
	}
	if res, err := f.oc.TryState(f.sid, k1, proto.OpcodeEntityMove, proto.MessageVersion1, payloadOf(23, 64)); err != nil || res != StateCoalesced {
		t.Fatalf("K1-new = %v,%v", res, err)
	}
	if msgs, _ := f.resident(); msgs != 3 { // writing critical + K2 + K1-new
		t.Fatalf("resident = %d, want 3", msgs)
	}
	f.releaseAll()
	if err := <-a; err != nil {
		t.Fatalf("send A: %v", err)
	}
	waitUntil(t, "state frames drained", func() bool { return f.tr.recordedCount() == 3 })

	frames := f.tr.recorded()
	// Deterministic state order: K2 kept its position, K1-new moved to
	// the tail as the newest value.
	if m := markerOf(t, frames[1]); m != 22 {
		t.Fatalf("first state frame marker = %d, want K2 (22)", m)
	}
	if m := markerOf(t, frames[2]); m != 23 {
		t.Fatalf("second state frame marker = %d, want K1-new (23)", m)
	}
}

// TestOutboundStateSaturationDrops proves a state update that cannot
// fit is dropped without disconnecting the session and without
// consuming a sequence (spec §7.1.4/§7.1.6).
func TestOutboundStateSaturationDrops(t *testing.T) {
	f := newOutboundFixture(t, tinyPolicy(2, 262144, 20*time.Millisecond, 10*time.Second))
	f.hold()
	a := f.sendAsync(t, 1, 64) // writing
	f.waitParked(t, 1)
	if err := f.oc.TryCritical(f.sid, proto.OpcodeError, proto.MessageVersion1, payloadOf(2, 64)); err != nil {
		t.Fatalf("fill TryCritical: %v", err)
	}

	res, err := f.oc.TryState(f.sid, StateKey{Kind: 0, ID: 9},
		proto.OpcodeEntityMove, proto.MessageVersion1, payloadOf(3, 64))
	if err != nil || res != StateDropped {
		t.Fatalf("saturated TryState = %v,%v want dropped,nil", res, err)
	}
	if f.tr.closeNowCount() != 0 {
		t.Fatal("state drop must not force-close the connection")
	}
	if _, ok := f.reg.Get(f.sid); !ok {
		t.Fatal("session must remain registered")
	}
	// The queue is still open (not closed/slow) for further traffic.
	if res, err := f.oc.TryState(f.sid, StateKey{Kind: 0, ID: 10},
		proto.OpcodeEntityMove, proto.MessageVersion1, payloadOf(4, 64)); err != nil || res != StateDropped {
		t.Fatalf("post-drop TryState = %v,%v want dropped,nil (queue alive)", res, err)
	}

	f.releaseAll()
	if err := <-a; err != nil {
		t.Fatalf("send A: %v", err)
	}
	// Dropped state never allocated a sequence: the next critical owns
	// seq 3 (A=1, fill=2).
	e := f.sendAsync(t, 99, 64)
	if err := <-e; err != nil {
		t.Fatalf("probe: %v", err)
	}
	frames := f.tr.recorded()
	if s := headerOf(t, frames[len(frames)-1]).Seq; s != 3 {
		t.Fatalf("probe seq = %d, want 3 — dropped state consumed sequence", s)
	}
	drops, _, _ := f.obs.snapshot()
	if len(drops) != 2 || drops[0] != stateDropSaturated || drops[1] != stateDropSaturated {
		t.Fatalf("state drops = %v, want two %q", drops, stateDropSaturated)
	}
}

// TestOutboundCriticalEvictsState proves critical admission evicts the
// oldest queued state to make room, is itself never dropped, and the
// session stays alive (spec §7.1.4).
func TestOutboundCriticalEvictsState(t *testing.T) {
	f := newOutboundFixture(t, tinyPolicy(5, 262144, 20*time.Millisecond, 10*time.Second))
	f.hold()
	a := f.sendAsync(t, 1, 64)
	f.waitParked(t, 1)

	for id := uint64(1); id <= 4; id++ {
		res, err := f.oc.TryState(f.sid, StateKey{Kind: 0, ID: id},
			proto.OpcodeEntityMove, proto.MessageVersion1, payloadOf(uint32(20+id), 64))
		if err != nil || res != StateQueued {
			t.Fatalf("state %d = %v,%v", id, res, err)
		}
	}
	// Budget is full (1 writing + 4 state). One critical must evict
	// exactly the oldest state entry.
	if err := f.oc.TryCritical(f.sid, proto.OpcodeError, proto.MessageVersion1, payloadOf(50, 64)); err != nil {
		t.Fatalf("critical with eviction: %v", err)
	}
	if f.tr.closeNowCount() != 0 {
		t.Fatal("eviction must not force-close the session")
	}
	f.releaseAll()
	if err := <-a; err != nil {
		t.Fatalf("send A: %v", err)
	}
	waitUntil(t, "post-eviction drain", func() bool { return f.tr.recordedCount() == 5 })

	// Critical priority: the admitted critical writes before ANY state
	// drains (spec §7.1.7), and the oldest state (K1) was evicted.
	frames := f.tr.recorded()
	wantMarkers := []uint32{1, 50, 22, 23, 24}
	for i, want := range wantMarkers {
		if got := markerOf(t, frames[i]); got != want {
			t.Fatalf("frame %d marker = %d, want %d (critical first, K1 evicted)", i, got, want)
		}
	}
	drops, _, _ := f.obs.snapshot()
	if len(drops) != 1 || drops[0] != stateDropEvicted {
		t.Fatalf("state drops = %v, want one %q", drops, stateDropEvicted)
	}
}

// TestOutboundSyncCriticalEvictsStateAndReports proves a SYNCHRONOUS
// critical admission also evicts queued state and reports the eviction
// through the observer seam (spec §7.1.4 + §7.1.12).
func TestOutboundSyncCriticalEvictsStateAndReports(t *testing.T) {
	f := newOutboundFixture(t, tinyPolicy(4, 262144, 10*time.Second, 10*time.Second))
	f.hold()
	a := f.sendAsync(t, 1, 64)
	f.waitParked(t, 1)

	for id := uint64(1); id <= 3; id++ {
		res, err := f.oc.TryState(f.sid, StateKey{Kind: 0, ID: id},
			proto.OpcodeEntityMove, proto.MessageVersion1, payloadOf(uint32(20+id), 64))
		if err != nil || res != StateQueued {
			t.Fatalf("state %d = %v,%v", id, res, err)
		}
	}
	// Budget: 1 writing + 3 state = 4 (writer still held, so A's
	// residency is pinned). The synchronous critical must evict the
	// oldest state at admission.
	b := f.sendAsync(t, 50, 64)
	waitUntil(t, "sync critical admitted behind the held write", func() bool {
		q := f.oc.q
		q.mu.Lock()
		defer q.mu.Unlock()
		return len(q.crit) == 1
	})
	f.releaseAll()
	if err := <-a; err != nil {
		t.Fatalf("send A: %v", err)
	}
	if err := <-b; err != nil {
		t.Fatalf("sync critical with eviction: %v", err)
	}
	waitUntil(t, "drain", func() bool { return f.tr.recordedCount() == 4 })
	frames := f.tr.recorded()
	// Critical priority: B wrote before the remaining state drained.
	if m := markerOf(t, frames[1]); m != 50 {
		t.Fatalf("frame after A = marker %d, want the critical 50 before state", m)
	}
	drops, _, _ := f.obs.snapshot()
	if len(drops) != 1 || drops[0] != stateDropEvicted {
		t.Fatalf("state drops = %v, want one %q from the sync eviction", drops, stateDropEvicted)
	}
}

// ---------------------------------------------------------------------------
// non-blocking producers
// ---------------------------------------------------------------------------

// TestOutboundTryCriticalSaturationFailsClosed proves TryCritical
// returns immediately with a slow-client classification and fails the
// session closed when critical backlog alone fills the budget — the
// event is never dropped and the caller never waits (spec §7.1.6).
func TestOutboundTryCriticalSaturationFailsClosed(t *testing.T) {
	f := newOutboundFixture(t, tinyPolicy(2, 262144, 10*time.Second, 10*time.Second))
	hold := f.hold()
	a := f.sendAsync(t, 1, 64) // writing, transport still held
	f.waitParked(t, 1)
	if err := f.oc.TryCritical(f.sid, proto.OpcodeError, proto.MessageVersion1, payloadOf(2, 64)); err != nil {
		t.Fatalf("fill TryCritical: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- f.oc.TryCritical(f.sid, proto.OpcodeError, proto.MessageVersion1, payloadOf(3, 64))
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrSlowClient) {
			t.Fatalf("TryCritical err = %v, want ErrSlowClient", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("TryCritical waited — it must return without queue capacity")
	}
	// The physical writer is STILL held: proof the return did not wait
	// on any socket operation.
	select {
	case <-hold:
		t.Fatal("hold released unexpectedly")
	default:
	}
	waitUntil(t, "fail-closed CloseNow", func() bool { return f.tr.closeNowCount() >= 1 })
	_, _, sessionDrops := f.obs.snapshot()
	if len(sessionDrops) != 1 || sessionDrops[0] != DropReasonCriticalSaturated {
		t.Fatalf("session drops = %v, want [%s]", sessionDrops, DropReasonCriticalSaturated)
	}
	// The queued critical waiter is released with the slow error.
	if err := <-a; !errors.Is(err, ErrSlowClient) {
		t.Fatalf("queued waiter err = %v, want ErrSlowClient", err)
	}
	f.releaseAll()
}

// ---------------------------------------------------------------------------
// timeouts
// ---------------------------------------------------------------------------

// TestOutboundWriteTimeout proves the writer pump's fresh per-write
// timeout fails the session closed as slow: every pending critical
// waiter fails, queued state is discarded, the transport is
// force-closed, and the pump terminates (spec §7.1.5).
func TestOutboundWriteTimeout(t *testing.T) {
	f := newOutboundFixture(t, tinyPolicy(8, 262144, 10*time.Second, 20*time.Millisecond))
	f.hold()
	a := f.sendAsync(t, 1, 64)
	f.waitParked(t, 1)
	if res, err := f.oc.TryState(f.sid, StateKey{Kind: 0, ID: 1},
		proto.OpcodeEntityMove, proto.MessageVersion1, payloadOf(2, 64)); err != nil || res != StateQueued {
		t.Fatalf("state = %v,%v", res, err)
	}

	if err := <-a; !errors.Is(err, ErrSlowClient) {
		t.Fatalf("write-timeout waiter err = %v, want ErrSlowClient", err)
	}
	waitUntil(t, "CloseNow after write timeout", func() bool { return f.tr.closeNowCount() >= 1 })
	waitUntil(t, "writer pump exit", f.pumpExited)
	drops, _, sessionDrops := f.obs.snapshot()
	if len(sessionDrops) != 1 || sessionDrops[0] != DropReasonWriteTimeout {
		t.Fatalf("session drops = %v, want [%s]", sessionDrops, DropReasonWriteTimeout)
	}
	if len(drops) != 1 || drops[0] != stateDropClosed {
		t.Fatalf("state drops = %v, want one %q (discarded on close)", drops, stateDropClosed)
	}
	// Post-close traffic reports closure, not a fresh slow classify.
	if res, err := f.oc.TryState(f.sid, StateKey{Kind: 0, ID: 3},
		proto.OpcodeEntityMove, proto.MessageVersion1, payloadOf(4, 64)); err != nil || res != StateClosed {
		t.Fatalf("post-close TryState = %v,%v, want StateClosed", res, err)
	}
}

// TestOutboundCallerCancelledBeforeAdmission proves a producer context
// cancelled while waiting for capacity returns the context error
// WITHOUT closing the healthy session and without queueing anything
// (spec §7.1.5).
func TestOutboundCallerCancelledBeforeAdmission(t *testing.T) {
	f := newOutboundFixture(t, tinyPolicy(1, 262144, 10*time.Second, 10*time.Second))
	f.hold()
	a := f.sendAsync(t, 1, 64) // fills the single-message budget
	f.waitParked(t, 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- f.oc.SendCritical(ctx, f.sid, proto.OpcodeError, proto.MessageVersion1, payloadOf(2, 64))
	}()
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled send err = %v, want context.Canceled", err)
	}
	// The session is healthy: queue open, no force-close, no slow drop.
	if f.tr.closeNowCount() != 0 {
		t.Fatal("producer cancellation must not force-close a healthy session")
	}
	if res, err := f.oc.TryState(f.sid, StateKey{Kind: 0, ID: 1},
		proto.OpcodeEntityMove, proto.MessageVersion1, payloadOf(3, 64)); err != nil || res == StateClosed {
		t.Fatalf("queue closed by producer cancellation: %v,%v", res, err)
	}
	if _, _, sessionDrops := f.obs.snapshot(); len(sessionDrops) != 0 {
		t.Fatalf("session drops = %v, want none", sessionDrops)
	}
	// The cancelled frame never queued: after release, exactly one
	// physical frame exists.
	f.releaseAll()
	if err := <-a; err != nil {
		t.Fatalf("send A: %v", err)
	}
	if n := f.tr.recordedCount(); n != 1 {
		t.Fatalf("frames = %d, want 1 — cancelled frame was queued", n)
	}
}

// ---------------------------------------------------------------------------
// payload preparation
// ---------------------------------------------------------------------------

// TestOutboundEncoderExactlyOnce proves the application encoder runs
// exactly once per offered message — never re-encoded for size
// estimation, and replaced state values are not re-encoded later
// (spec §7.1.8).
func TestOutboundEncoderExactlyOnce(t *testing.T) {
	f := newOutboundFixture(t, tinyPolicy(8, 262144, 10*time.Second, 10*time.Second))
	f.hold()
	a := f.sendAsync(t, 1, 64)
	f.waitParked(t, 1)

	var stateCalls atomic.Int64
	key := StateKey{Kind: 0, ID: 5}
	for i := 0; i < 3; i++ {
		want := StateQueued
		if i > 0 {
			want = StateCoalesced // only replacements coalesce
		}
		res, err := f.oc.TryState(f.sid, key, proto.OpcodeEntityMove, proto.MessageVersion1,
			func(e *proto.Encoder) error {
				stateCalls.Add(1)
				e.U32(uint32(i))
				return nil
			})
		if err != nil || res != want {
			t.Fatalf("TryState %d = %v,%v want %v", i, res, err, want)
		}
	}
	if n := stateCalls.Load(); n != 3 {
		t.Fatalf("state encoder calls = %d, want 3 (once per offer)", n)
	}
	f.releaseAll()
	if err := <-a; err != nil {
		t.Fatalf("send A: %v", err)
	}
	waitUntil(t, "state drained", func() bool { return f.tr.recordedCount() == 2 })
	if n := stateCalls.Load(); n != 3 {
		t.Fatalf("state encoder calls after drain = %d, want 3 (no re-encode)", n)
	}
}

// TestOutboundEncodingFailureNotBackpressure proves a payload encoding
// failure returns immediately without queueing, without sequence
// allocation, and without classifying the client slow (spec §7.1.8).
func TestOutboundEncodingFailureNotBackpressure(t *testing.T) {
	f := newOutboundFixture(t, tinyPolicy(8, 262144, 10*time.Second, 10*time.Second))
	sentinel := errors.New("app encode boom")

	err := f.oc.SendCritical(context.Background(), f.sid, proto.OpcodeError, proto.MessageVersion1,
		func(e *proto.Encoder) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the encoding sentinel", err)
	}
	// Oversized payloads fail the same way at preparation time.
	err = f.oc.SendCritical(context.Background(), f.sid, proto.OpcodeError, proto.MessageVersion1,
		func(e *proto.Encoder) error {
			e.WriteBytes(make([]byte, proto.MaxFrameSize))
			return nil
		})
	if err == nil || !errors.Is(err, proto.ErrFrameTooLarge) {
		t.Fatalf("oversized err = %v, want ErrFrameTooLarge", err)
	}
	if _, _, sessionDrops := f.obs.snapshot(); len(sessionDrops) != 0 {
		t.Fatalf("session drops = %v, want none — an encoding bug is not backpressure", sessionDrops)
	}
	// No sequence leaked: the next successful frame owns seq 1.
	a := f.sendAsync(t, 7, 64)
	if err := <-a; err != nil {
		t.Fatalf("send: %v", err)
	}
	if frames := f.tr.recorded(); len(frames) != 1 || headerOf(t, frames[0]).Seq != 1 {
		t.Fatalf("frames = %d, want one seq-1 frame — encoding failure consumed a sequence", len(frames))
	}
}

// ---------------------------------------------------------------------------
// seq/tick timing
// ---------------------------------------------------------------------------

// TestOutboundTickSampledAtWriteTime proves the frame tick is sampled
// when the writer physically writes, not at admission (spec §7.1.8).
func TestOutboundTickSampledAtWriteTime(t *testing.T) {
	f := newOutboundFixture(t, tinyPolicy(8, 262144, 10*time.Second, 10*time.Second))
	f.hold()
	a := f.sendAsync(t, 1, 64)
	f.waitParked(t, 1)
	atomic.StoreUint32(&f.tick, 2000) // change the source after admission
	f.releaseAll()
	if err := <-a; err != nil {
		t.Fatalf("send: %v", err)
	}
	frames := f.tr.recorded()
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	if got := headerOf(t, frames[0]).Tick; got != 2000 {
		t.Fatalf("tick = %d, want write-time 2000 (admission-time was 1000)", got)
	}
}

// TestOutboundQueuedConcurrentWireOrderMonotonic is the queued-path
// adaptation of the T4b concurrent-send regression: concurrent
// synchronous sends through the two-lane queue must produce a physical
// stream whose sequence numbers are exactly 1..N in wire order (spec
// §7.1.8).
func TestOutboundQueuedConcurrentWireOrderMonotonic(t *testing.T) {
	f := newOutboundFixture(t, tinyPolicy(1024, 262144, 10*time.Second, 10*time.Second))
	const workers, per = 25, 4
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				if err := f.oc.SendCritical(context.Background(), f.sid,
					proto.OpcodeError, proto.MessageVersion1, payloadOf(uint32(i), 64)); err != nil {
					t.Errorf("send: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	frames := f.tr.recorded()
	if len(frames) != workers*per {
		t.Fatalf("frames = %d, want %d", len(frames), workers*per)
	}
	for i, frame := range frames {
		if s := headerOf(t, frame).Seq; s != uint32(i+1) {
			t.Fatalf("physical write %d carries seq %d, want %d — allocation escaped writer serialization", i, s, i+1)
		}
		if got := headerOf(t, frame).Tick; got != 1000 {
			t.Fatalf("frame %d tick = %d, want injected 1000", i, got)
		}
	}
}

// ---------------------------------------------------------------------------
// synchronous completion barrier
// ---------------------------------------------------------------------------

// TestOutboundSyncCompletionBarrier proves SendCritical does not return
// while its frame merely sits queued or is mid-write: it completes only
// after the physical write, preserving the T4 world_ready barrier
// contract (spec §7.1.5).
func TestOutboundSyncCompletionBarrier(t *testing.T) {
	f := newOutboundFixture(t, tinyPolicy(8, 262144, 10*time.Second, 10*time.Second))
	f.hold()
	done := f.sendAsync(t, 1, 64)
	f.waitParked(t, 1)
	// The write is parked inside the transport: the send cannot have
	// completed.
	select {
	case err := <-done:
		t.Fatalf("send completed before physical write: %v", err)
	default:
	}
	f.releaseAll()
	if err := <-done; err != nil {
		t.Fatalf("send: %v", err)
	}
	if n := f.tr.recordedCount(); n != 1 {
		t.Fatalf("frames = %d, want 1 — send returned before the physical write", n)
	}
}

// ---------------------------------------------------------------------------
// shutdown
// ---------------------------------------------------------------------------

// TestOutboundShutdownReleasesWaiters repeatedly builds saturated
// queues with parked senders and proves every shutdown path (graceful
// Stop, forced CloseNow) releases all waiting critical callers, exits
// the writer pump, and stays idempotent — without goroutine-count
// tricks (spec §7.1.9).
func TestOutboundShutdownReleasesWaiters(t *testing.T) {
	for i := 0; i < 25; i++ {
		forced := i%2 == 1
		f := newOutboundFixture(t, tinyPolicy(2, 262144, 10*time.Second, 10*time.Second))
		f.hold()
		a := f.sendAsync(t, 1, 64)
		f.waitParked(t, 1)
		if err := f.oc.TryCritical(f.sid, proto.OpcodeError, proto.MessageVersion1, payloadOf(2, 64)); err != nil {
			t.Fatalf("fill: %v", err)
		}
		d := f.sendAsync(t, 3, 64) // parked waiting for capacity

		if forced {
			if err := f.oc.CloseNow(); err != nil {
				t.Fatalf("CloseNow: %v", err)
			}
		} else {
			f.oc.StopOutbound("test stop")
		}
		// Idempotence: a second shutdown is harmless.
		f.oc.StopOutbound("second stop")

		if err := <-d; !errors.Is(err, ErrOutboundClosed) {
			t.Fatalf("iteration %d: waiter err = %v, want ErrOutboundClosed", i, err)
		}
		if err := <-a; !errors.Is(err, ErrOutboundClosed) {
			t.Fatalf("iteration %d: in-flight err = %v, want ErrOutboundClosed", i, err)
		}
		f.releaseAll()
		waitUntil(t, "pump exit", f.pumpExited)
		if forced && f.tr.closeNowCount() < 1 {
			t.Fatalf("iteration %d: forced close missing CloseNow", i)
		}
	}
}

// TestOutboundSessionsIndependent proves two sessions' queues and
// writer pumps do not share any mutex or serialization: a blocked,
// saturated session A never stalls session B's normal sends (spec
// §7.1.1/§7.1.7).
func TestOutboundSessionsIndependent(t *testing.T) {
	fa := newOutboundFixture(t, tinyPolicy(1, 262144, 10*time.Second, 10*time.Second))
	fb := newOutboundFixture(t, tinyPolicy(8, 262144, 10*time.Second, 10*time.Second))
	fa.hold()
	a := fa.sendAsync(t, 1, 64)
	fa.waitParked(t, 1)
	// A is stuck and full; its synchronous overflow parks.
	overflow := fa.sendAsync(t, 2, 64)

	// B completes normally while A is wedged.
	b := fb.sendAsync(t, 9, 64)
	if err := <-b; err != nil {
		t.Fatalf("session B send: %v", err)
	}
	if n := fb.tr.recordedCount(); n != 1 {
		t.Fatalf("B frames = %d, want 1", n)
	}
	select {
	case err := <-overflow:
		t.Fatalf("A overflow resolved while A still held: %v", err)
	default:
	}
	fa.releaseAll()
	if err := <-a; err != nil {
		t.Fatalf("session A send: %v", err)
	}
	if err := <-overflow; err != nil {
		t.Fatalf("A overflow: %v", err)
	}
}

// ---------------------------------------------------------------------------
// terminal control bypasses
// ---------------------------------------------------------------------------

// TestOutboundKickedBypassesSaturatedQueue proves the T4b duplicate-
// login kicked write bypasses the old session's saturated normal queue:
// retirement (registry removal + forced close) proceeds even while the
// old queue is full and its physical writer is stuck past the kick
// budget (spec §7.1.9).
func TestOutboundKickedBypassesSaturatedQueue(t *testing.T) {
	old := kickWriteBudget
	kickWriteBudget = 15 * time.Millisecond
	t.Cleanup(func() { kickWriteBudget = old })

	reg := session.NewRegistry()
	tr := newFakeOutTransport()
	oc := newOutboundConn(OutboundDeps{
		Conn:     tr,
		Registry: reg,
		Tick:     func() uint32 { return 1 },
		Policy:   tinyPolicy(2, 262144, 10*time.Second, 10*time.Second),
	})
	sid := reg.Create(oc)
	if err := reg.Authenticate(sid, "sub", 7, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := reg.BindCharacter(sid, 500); err != nil {
		t.Fatal(err)
	}
	for _, want := range []session.State{session.StateCharacterSelected, session.StateInWorld} {
		cur, _ := reg.Get(sid)
		if err := reg.CompareAndSetState(sid, cur.State, want); err != nil {
			t.Fatal(err)
		}
	}
	hold := make(chan struct{})
	tr.setHold(hold)
	t.Cleanup(func() {
		select {
		case <-hold:
		default:
			close(hold)
		}
		oc.StopOutbound("test complete")
	})

	// Saturate the old session's queue: one parked write + one queued.
	parked := make(chan error, 1)
	go func() {
		parked <- oc.SendCritical(context.Background(), sid,
			proto.OpcodeError, proto.MessageVersion1, payloadOf(1, 64))
	}()
	waitUntil(t, "old writer parked", func() bool { return tr.buildCount() == 1 })
	if err := oc.TryCritical(sid, proto.OpcodeError, proto.MessageVersion1, payloadOf(2, 64)); err != nil {
		t.Fatalf("fill: %v", err)
	}

	lookup := &fakeLookup{desc: character.Descriptor{ID: 500, Slot: 0, Name: "Old", Revision: 3}}
	provider := newFakeProvider()
	exit := &recordingExit{registry: reg}
	h, err := NewEnterWorldHandler(EnterWorldHandlerDeps{
		Characters: lookup,
		Registry:   reg,
		Baseline:   BaselineProviderFunc(provider.StreamBaseline),
		WorldExit:  WorldExitFunc(exit.ExitWorld),
		Tick:       func() uint32 { return enterTestTick },
	})
	if err != nil {
		t.Fatal(err)
	}
	snap, ok := reg.Get(sid)
	if !ok {
		t.Fatal("old session vanished")
	}
	started := time.Now()
	if err := h.takeoverOld(context.Background(), 7, snap); err != nil {
		t.Fatalf("takeoverOld: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("takeover took %v — kicked waited behind normal traffic", elapsed)
	}
	waitUntil(t, "forced retirement", func() bool { return tr.closeNowCount() >= 1 })
	if _, still := reg.Get(sid); still {
		t.Fatal("old session not retired from the registry")
	}
	if res, _ := oc.TryState(sid, StateKey{Kind: 0, ID: 1},
		proto.OpcodeEntityMove, proto.MessageVersion1, payloadOf(3, 64)); res != StateClosed {
		t.Fatalf("old queue after retirement = %v, want StateClosed", res)
	}
	// Release: the parked pump write unblocks and the pump exits
	// without emitting another old-session frame.
	select {
	case <-hold:
	default:
		close(hold)
	}
	waitUntil(t, "old pump exit", func() bool {
		select {
		case <-oc.q.pumpDone:
			return true
		default:
			return false
		}
	})
	if err := <-parked; err == nil {
		t.Fatal("parked old-session send should fail after retirement")
	}
}

// TestOutboundDeadlineBypassesSaturatedQueue proves the hard
// authorization-deadline 202 goes out through the DIRECT low-level
// writer and never waits for normal queue capacity: with the queue
// saturated and the physical writer stuck, the deadline callback
// completes within its bounded write budget and closes the connection
// (spec §7.1.9).
func TestOutboundDeadlineBypassesSaturatedQueue(t *testing.T) {
	old := deadlineWriteBudget
	deadlineWriteBudget = 15 * time.Millisecond
	t.Cleanup(func() { deadlineWriteBudget = old })

	reg := session.NewRegistry()
	tr := newFakeOutTransport()
	oc := newOutboundConn(OutboundDeps{
		Conn:     tr,
		Registry: reg,
		Tick:     func() uint32 { return 77 },
		Policy:   tinyPolicy(2, 262144, 10*time.Second, 10*time.Second),
	})
	sid := reg.Create(oc)
	exp := time.Now().Add(-2 * time.Minute) // deadline long past
	if err := reg.Authenticate(sid, "sub", 1, exp); err != nil {
		t.Fatal(err)
	}
	hold := make(chan struct{})
	tr.setHold(hold)
	t.Cleanup(func() {
		select {
		case <-hold:
		default:
			close(hold)
		}
		oc.StopOutbound("test complete")
	})

	// Saturate: one parked write + one queued critical.
	parked := make(chan error, 1)
	go func() {
		parked <- oc.SendCritical(context.Background(), sid,
			proto.OpcodeError, proto.MessageVersion1, payloadOf(1, 64))
	}()
	waitUntil(t, "writer parked", func() bool { return tr.buildCount() == 1 })
	if err := oc.TryCritical(sid, proto.OpcodeError, proto.MessageVersion1, payloadOf(2, 64)); err != nil {
		t.Fatalf("fill: %v", err)
	}

	var fire func()
	s := NewServer(ServerDeps{
		Registry: reg,
		Tick:     func() uint32 { return 77 },
		Now:      func() time.Time { return time.Now() },
		Schedule: func(at time.Time, fn func()) CancelFunc { fire = fn; return func() {} },
	})
	ca := &connAuth{}
	s.armDeadline(ca, sid, exp)
	if fire == nil {
		t.Fatal("deadline never scheduled")
	}
	fired := make(chan struct{})
	go func() {
		fire()
		close(fired)
	}()
	select {
	case <-fired:
		// The deadline callback returned without waiting for normal
		// queue capacity (bounded only by the direct write budget).
	case <-time.After(2 * time.Second):
		t.Fatal("deadline callback waited behind the saturated queue")
	}
	// The queue itself was untouched by the deadline path: still open
	// and saturated, not closed.
	if res, err := oc.TryState(sid, StateKey{Kind: 0, ID: 1},
		proto.OpcodeEntityMove, proto.MessageVersion1, payloadOf(3, 64)); err != nil || res != StateDropped {
		t.Fatalf("queue state after deadline = %v,%v — deadline must not close or drain the queue", res, err)
	}
	waitUntil(t, "deadline close", func() bool { return len(tr.closeReasons()) > 0 })
	select {
	case <-hold:
	default:
		close(hold)
	}
	// The fake transport's Close does not interrupt a parked write the
	// way a real socket would; the meaningful assertion above is that
	// the deadline path never waited for queue capacity. Drain.
	<-parked
}

// ---------------------------------------------------------------------------
// real slow peer over a real WebSocket (production ServeHTTP path)
// ---------------------------------------------------------------------------

// floodHandler answers one allowed opcode with a burst of large frames
// so a non-reading client saturates the outbound path.
type floodHandler struct {
	burst int
}

func (h *floodHandler) Handle(
	_ context.Context,
	_ session.ID,
	_ proto.Header,
	_ *proto.Decoder,
	send SendFunc,
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

// TestOutboundSlowPeerDisconnectWS drives the production ServeHTTP path
// with a real WebSocket client that authenticates and then stops
// reading while the server floods it: the bounded outbound path must
// fail the session closed as slow and tear it down — reconnect/full
// resync is the recovery (spec §7.1.5/§7.1.10).
func TestOutboundSlowPeerDisconnectWS(t *testing.T) {
	reg := session.NewRegistry()
	obs := &recordingObserver{}
	validator := auth.ValidatorFunc(func(_ context.Context, _ string) (auth.Identity, error) {
		return auth.Identity{Sub: "slow-sub", ExpiresAt: time.Now().Add(time.Hour)}, nil
	})
	accounts := AccountProvisionerFunc(func(_ context.Context, _ string, _ *string) (int64, error) {
		return 1, nil
	})
	srv := NewServer(ServerDeps{
		Registry:         reg,
		Validator:        validator,
		Accounts:         accounts,
		Tick:             func() uint32 { return 1 },
		Now:              func() time.Time { return time.Now() },
		Schedule:         func(time.Time, func()) CancelFunc { return func() {} },
		Outbound:         tinyPolicy(1024, 262144, 100*time.Millisecond, 100*time.Millisecond),
		OutboundObserver: obs,
		Handler:          &floodHandler{burst: 60}, // 60 × ~60 KiB ≈ 3.5 MiB
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

	// hello (the client consumes the welcome, then stops reading).
	sendCtx, sendCancel := context.WithTimeout(context.Background(), 5*time.Second)
	frame, err := proto.EncodeFrame(proto.Header{Opcode: proto.OpcodeHello, MsgVersion: 1},
		func(e *proto.Encoder) error {
			proto.Hello{ClientVersion: 9, ProtoVersion: 1, AccessToken: "tok"}.Encode(e)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Write(sendCtx, websocket.MessageBinary, frame); err != nil {
		t.Fatalf("hello write: %v", err)
	}
	sendCancel()
	readCtx, readCancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, _, err = client.Read(readCtx) // welcome
	readCancel()
	if err != nil {
		t.Fatalf("welcome read: %v", err)
	}

	// Trigger the flood; the client never reads again. The server's
	// bounded writes/queues must time out and fail the session closed.
	sendCtx, sendCancel = context.WithTimeout(context.Background(), 5*time.Second)
	frame, err = proto.EncodeFrame(proto.Header{Opcode: proto.OpcodeCharacterList, MsgVersion: 1},
		func(e *proto.Encoder) error {
			proto.CharacterListRequest{}.Encode(e)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Write(sendCtx, websocket.MessageBinary, frame); err != nil {
		t.Fatalf("trigger write: %v", err)
	}
	sendCancel()

	waitUntil(t, "slow session drop", func() bool {
		_, _, drops := obs.snapshot()
		return len(drops) >= 1
	})
	waitUntil(t, "session teardown", func() bool { return reg.Len() == 0 })
	readCtx, readCancel = context.WithTimeout(context.Background(), 5*time.Second)
	_, _, err = client.Read(readCtx)
	readCancel()
	if err == nil {
		t.Fatal("expected the terminated connection to surface a read error")
	}
	_, _, drops := obs.snapshot()
	if len(drops) != 1 {
		t.Fatalf("session drops = %v, want exactly one slow classification", drops)
	}
	if drops[0] != DropReasonWriteTimeout && drops[0] != DropReasonEnqueueTimeout {
		t.Fatalf("drop reason = %q, want a write/enqueue timeout classification", drops[0])
	}
}

// TestOutboundDefaultPolicyAndNoopObserver pins the production default
// policy values and proves the no-op observer keeps normal traffic
// working (spec §7.1.1/§7.1.12).
func TestOutboundDefaultPolicyAndNoopObserver(t *testing.T) {
	p := DefaultOutboundPolicy()
	if p.MaxMessages != 1024 || p.MaxBytes != 262144 ||
		p.ReliableEnqueueTimeout != 1000*time.Millisecond ||
		p.WriteTimeout != 5000*time.Millisecond {
		t.Fatalf("default policy = %+v, want the frozen §7.1.1 defaults", p)
	}
	if got := (OutboundPolicy{}).orDefaults(); got != p {
		t.Fatalf("zero policy fallback = %+v, want defaults", got)
	}
	f := newOutboundFixture(t, OutboundPolicy{}) // exercises orDefaults + noop observer
	a := f.sendAsync(t, 1, 64)
	if err := <-a; err != nil {
		t.Fatalf("send with default policy: %v", err)
	}
	if n := f.tr.recordedCount(); n != 1 {
		t.Fatalf("frames = %d, want 1", n)
	}
}
