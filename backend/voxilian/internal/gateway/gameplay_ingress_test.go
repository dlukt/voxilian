package gateway

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/dlukt/voxilian/internal/proto"
	"github.com/dlukt/voxilian/internal/session"
	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/world"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

var ingressTestBase = time.Unix(1_700_000_000, 0).UTC()

type ingressFixture struct {
	presence *PresenceRegistry
	fakeSim  *recordingSim
	now      time.Time
	next     *ingressNext
	h        *GameplayIngressHandler
}

func newIngressFixture(t *testing.T, policy RateLimitPolicy) *ingressFixture {
	t.Helper()
	presence, err := NewPresenceRegistry(policy)
	if err != nil {
		t.Fatal(err)
	}
	fakeSim := &recordingSim{}
	next := &ingressNext{}
	now := ingressTestBase
	h, err := NewGameplayIngressHandler(presence, fakeSim, func() time.Time { return now }, next)
	if err != nil {
		t.Fatal(err)
	}
	return &ingressFixture{presence: presence, fakeSim: fakeSim, now: now, next: next, h: h}
}

// activatePresence binds character 500 to a controlled sim entity for
// sid in the IN_WORLD-capable presence registry. Character, entity,
// and NetEntityID values are deliberately distinct.
func (f *ingressFixture) activatePresence(t *testing.T, sid session.ID, entity sim.EntityID) {
	t.Helper()
	if _, err := f.presence.Activate(sid, 500, entity, world.CellCoord{}, f.now); err != nil {
		t.Fatalf("Activate: %v", err)
	}
}

func encodeMove(t *testing.T, header proto.Header, mv proto.Move) []byte {
	t.Helper()
	frame, err := proto.EncodeFrame(header, func(e *proto.Encoder) error {
		mv.Encode(e)
		return nil
	})
	if err != nil {
		t.Fatalf("encode move: %v", err)
	}
	return frame
}

func callIngress(t *testing.T, h *GameplayIngressHandler, sid session.ID, frame []byte, send SendFunc) error {
	t.Helper()
	header, dec, err := proto.DecodeFrame(frame)
	if err != nil {
		t.Fatalf("decode test frame: %v", err)
	}
	return h.Handle(context.Background(), sid, header, dec, send)
}

func clientErrorOf(t *testing.T, err error) *ClientError {
	t.Helper()
	var cerr *ClientError
	if !errors.As(err, &cerr) {
		t.Fatalf("err = %v, want *ClientError", err)
	}
	return cerr
}

// ingressNext records delegated calls with their exact arguments and
// performs no send (unlike the marker-emitting recordingNext).
type ingressNext struct {
	mu    sync.Mutex
	calls []ingressCall
	err   error
}

type ingressCall struct {
	sid     session.ID
	header  proto.Header
	payload *proto.Decoder
	hasSend bool
}

func (n *ingressNext) Handle(_ context.Context, sid session.ID, header proto.Header, payload *proto.Decoder, send SendFunc) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, ingressCall{sid: sid, header: header, payload: payload, hasSend: send != nil})
	return n.err
}

func (n *ingressNext) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.calls)
}

func (n *ingressNext) last() ingressCall {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.calls[len(n.calls)-1]
}

// ---------------------------------------------------------------------------
// constructor + 102 routing
// ---------------------------------------------------------------------------

func TestGameplayIngressRequiresDeps(t *testing.T) {
	presence, _ := NewPresenceRegistry(testPolicy())
	fakeSim := &recordingSim{}
	now := func() time.Time { return ingressTestBase }
	if _, err := NewGameplayIngressHandler(nil, fakeSim, now, nil); err == nil {
		t.Error("nil presence accepted")
	}
	if _, err := NewGameplayIngressHandler(presence, nil, now, nil); err == nil {
		t.Error("nil sim accepted")
	}
	if _, err := NewGameplayIngressHandler(presence, fakeSim, nil, nil); err == nil {
		t.Error("nil clock accepted")
	}
	if _, err := NewGameplayIngressHandler(presence, fakeSim, now, nil); err != nil {
		t.Errorf("nil Next rejected: %v", err)
	}
}

func TestMoveMalformedNoChargeNoSim(t *testing.T) {
	f := newIngressFixture(t, testPolicy())
	f.activatePresence(t, 1, 7)
	truncated, err := proto.EncodeFrame(
		proto.Header{Opcode: proto.OpcodeMove, MsgVersion: 1},
		func(e *proto.Encoder) error {
			e.U32(9) // inputSeq only: heldDirs/runFlag/yaw missing
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if err := callIngress(t, f.h, 1, truncated, nil); clientErrorOf(t, err).Code != proto.ErrorCodeProtocol {
		t.Fatalf("malformed move err = %v, want protocol_error", err)
	}
	if n := f.fakeSim.moveCount(); n != 0 {
		t.Fatalf("malformed move submitted %d sim commands", n)
	}
	// The full burst is still available: malformed consumed nothing.
	for i := 0; i < 10; i++ {
		frame := encodeMove(t, proto.Header{Opcode: proto.OpcodeMove, MsgVersion: 1, Tick: 50},
			proto.Move{InputSeq: uint32(i + 1), HeldDirs: 1, Yaw: 100})
		if err := callIngress(t, f.h, 1, frame, nil); err != nil {
			t.Fatalf("valid move #%d after malformed: %v", i, err)
		}
	}
}

func TestMoveExactConversion(t *testing.T) {
	f := newIngressFixture(t, testPolicy())
	f.activatePresence(t, 1, 7)
	header := proto.Header{Opcode: proto.OpcodeMove, MsgVersion: 1, Seq: 999, Tick: 77}
	frame := encodeMove(t, header, proto.Move{InputSeq: 5, HeldDirs: 3, RunFlag: 1, Yaw: 2048})
	if err := callIngress(t, f.h, 1, frame, nil); err != nil {
		t.Fatalf("move: %v", err)
	}
	if n := f.fakeSim.moveCount(); n != 1 {
		t.Fatalf("sim moves = %d, want 1", n)
	}
	got := f.fakeSim.moves[0]
	// Presence EntityID routing: character 500, NetEntityID 1, and
	// header seq 999 must never leak into sim identity/sequencing.
	if got.id != 7 {
		t.Fatalf("sim entity = %d, want presence-controlled 7", uint64(got.id))
	}
	want := sim.MoveIntent{InputSeq: 5, HeldDirs: 3, RunFlag: 1, Yaw: 2048, SampleTick: 77}
	if got.intent != want {
		t.Fatalf("sim intent = %+v, want %+v", got.intent, want)
	}
}

func TestMoveMissingPresenceInvariant(t *testing.T) {
	f := newIngressFixture(t, testPolicy())
	frame := encodeMove(t, proto.Header{Opcode: proto.OpcodeMove, MsgVersion: 1, Tick: 1},
		proto.Move{InputSeq: 1, HeldDirs: 1, Yaw: 0})
	err := callIngress(t, f.h, 9, frame, nil)
	var cerr *ClientError
	if errors.As(err, &cerr) {
		t.Fatalf("missing presence mapped to 202 %d, want internal", cerr.Code)
	}
	if err == nil {
		t.Fatalf("missing presence returned nil")
	}
	if f.next.count() != 0 || f.fakeSim.moveCount() != 0 {
		t.Fatalf("missing presence reached Next/sim")
	}
}

func TestMoveRateDeniedNoSim(t *testing.T) {
	f := newIngressFixture(t, RateLimitPolicy{MovePerSec: 2, IntentPerSec: 2})
	f.activatePresence(t, 1, 7)
	mk := func(seq uint32) []byte {
		return encodeMove(t, proto.Header{Opcode: proto.OpcodeMove, MsgVersion: 1, Tick: 1},
			proto.Move{InputSeq: seq, HeldDirs: 1, Yaw: 0})
	}
	if err := callIngress(t, f.h, 1, mk(1), nil); err != nil {
		t.Fatal(err)
	}
	if err := callIngress(t, f.h, 1, mk(2), nil); err != nil {
		t.Fatal(err)
	}
	if err := callIngress(t, f.h, 1, mk(3), nil); clientErrorOf(t, err).Code != proto.ErrorCodeRateLimited {
		t.Fatalf("exhausted move err = %v, want rate_limited", err)
	}
	if n := f.fakeSim.moveCount(); n != 2 {
		t.Fatalf("denied move submitted: moves = %d", n)
	}
	// Connection remains usable: an intent on the other bucket passes.
	if err := callIngress(t, f.h, 1, encodeIntentFrame(t, proto.OpcodeAttack), nil); err != nil {
		t.Fatalf("intent after move denial: %v", err)
	}
}

func TestMoveDuplicateStaleConsumeRateSilent(t *testing.T) {
	for _, disp := range []sim.MoveDisposition{sim.MoveDuplicate, sim.MoveStale} {
		f := newIngressFixture(t, RateLimitPolicy{MovePerSec: 2, IntentPerSec: 2})
		f.activatePresence(t, 1, 7)
		f.fakeSim.moveDisp = disp
		mk := func(seq uint32) []byte {
			return encodeMove(t, proto.Header{Opcode: proto.OpcodeMove, MsgVersion: 1, Tick: 1},
				proto.Move{InputSeq: seq, HeldDirs: 1, Yaw: 0})
		}
		// Duplicate/stale are silent successes that still cost tokens.
		if err := callIngress(t, f.h, 1, mk(1), nil); err != nil {
			t.Fatalf("disp %v move 1: %v", disp, err)
		}
		if err := callIngress(t, f.h, 1, mk(1), nil); err != nil {
			t.Fatalf("disp %v move 2: %v", disp, err)
		}
		if err := callIngress(t, f.h, 1, mk(1), nil); clientErrorOf(t, err).Code != proto.ErrorCodeRateLimited {
			t.Fatalf("disp %v move 3 err = %v, want rate_limited", disp, err)
		}
	}
}

func TestMoveErrorMappings(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code uint16
	}{
		{"migration full", sim.ErrMigrationQueueFull, proto.ErrorCodeRetry},
		{"ingress full", sim.ErrSimIngressFull, proto.ErrorCodeRetry},
		{"engine not running", sim.ErrEngineNotRunning, proto.ErrorCodeRetry},
		{"engine stopped", sim.ErrEngineStopped, proto.ErrorCodeRetry},
		{"bad yaw", sim.ErrInvalidMoveYaw, proto.ErrorCodeProtocol},
		{"future tick", sim.ErrFutureInputTick, proto.ErrorCodeProtocol},
		{"ambiguous seq", sim.ErrAmbiguousInputSeq, proto.ErrorCodeProtocol},
		{"ambiguous sample", sim.ErrAmbiguousSampleTick, proto.ErrorCodeProtocol},
	} {
		f := newIngressFixture(t, testPolicy())
		f.activatePresence(t, 1, 7)
		f.fakeSim.moveErr = tc.err
		frame := encodeMove(t, proto.Header{Opcode: proto.OpcodeMove, MsgVersion: 1, Tick: 1},
			proto.Move{InputSeq: 1, HeldDirs: 1, Yaw: 0})
		if err := callIngress(t, f.h, 1, frame, nil); clientErrorOf(t, err).Code != tc.code {
			t.Errorf("%s: err = %v, want code %d", tc.name, err, tc.code)
		}
	}
}

func TestMoveEntityNotFoundInternal(t *testing.T) {
	f := newIngressFixture(t, testPolicy())
	f.activatePresence(t, 1, 7)
	f.fakeSim.moveErr = sim.ErrEntityNotFound
	frame := encodeMove(t, proto.Header{Opcode: proto.OpcodeMove, MsgVersion: 1, Tick: 1},
		proto.Move{InputSeq: 1, HeldDirs: 1, Yaw: 0})
	err := callIngress(t, f.h, 1, frame, nil)
	var cerr *ClientError
	if errors.As(err, &cerr) {
		t.Fatalf("diverged entity mapped to 202 %d, want internal", cerr.Code)
	}
	if err == nil {
		t.Fatalf("diverged entity returned nil")
	}
}

// ---------------------------------------------------------------------------
// 103..120 rate gate
// ---------------------------------------------------------------------------

func encodeIntentFrame(t *testing.T, opcode uint16) []byte {
	t.Helper()
	frame, err := proto.EncodeFrame(
		proto.Header{Opcode: opcode, MsgVersion: 1, Seq: 44, Tick: 55},
		func(e *proto.Encoder) error {
			e.U32(12345) // opaque gameplay payload: never decoded here
			return nil
		})
	if err != nil {
		t.Fatalf("encode intent: %v", err)
	}
	return frame
}

func TestIntentGateTableAllowed(t *testing.T) {
	for opcode := uint16(103); opcode <= 120; opcode++ {
		f := newIngressFixture(t, testPolicy())
		f.activatePresence(t, 1, 7)
		frame := encodeIntentFrame(t, opcode)
		header, dec, err := proto.DecodeFrame(frame)
		if err != nil {
			t.Fatal(err)
		}
		send := SendFunc(func(uint16, uint16, func(*proto.Encoder) error) error { return nil })
		if err := f.h.Handle(context.Background(), 1, header, dec, send); err != nil {
			t.Fatalf("opcode %d allowed: %v", opcode, err)
		}
		if n := f.next.count(); n != 1 {
			t.Fatalf("opcode %d: Next calls = %d, want 1", opcode, n)
		}
		got := f.next.last()
		if got.header != header || got.payload != dec || got.sid != 1 || !got.hasSend {
			t.Fatalf("opcode %d: Next args mutated: %+v", opcode, got)
		}
		// Each allowed call consumed exactly one intent token: 9 more
		// pass (burst 10), the 11th is denied with zero new Next.
		for i := 0; i < 9; i++ {
			fr := encodeIntentFrame(t, opcode)
			hd, dc, _ := proto.DecodeFrame(fr)
			if err := f.h.Handle(context.Background(), 1, hd, dc, send); err != nil {
				t.Fatalf("opcode %d call %d: %v", opcode, i, err)
			}
		}
		fr := encodeIntentFrame(t, opcode)
		hd, dc, _ := proto.DecodeFrame(fr)
		if err := f.h.Handle(context.Background(), 1, hd, dc, send); clientErrorOf(t, err).Code != proto.ErrorCodeRateLimited {
			t.Fatalf("opcode %d 11th err = %v, want rate_limited", opcode, err)
		}
		if n := f.next.count(); n != 10 {
			t.Fatalf("opcode %d: Next calls = %d, want 10", opcode, n)
		}
	}
}

func TestIntentGateTableDenied(t *testing.T) {
	for opcode := uint16(103); opcode <= 120; opcode++ {
		f := newIngressFixture(t, RateLimitPolicy{MovePerSec: 10, IntentPerSec: 1})
		f.activatePresence(t, 1, 7)
		frame := encodeIntentFrame(t, opcode)
		header, dec, _ := proto.DecodeFrame(frame)
		if err := f.h.Handle(context.Background(), 1, header, dec, nil); err != nil {
			t.Fatalf("opcode %d first: %v", opcode, err)
		}
		fr := encodeIntentFrame(t, opcode)
		hd, dc, _ := proto.DecodeFrame(fr)
		if err := f.h.Handle(context.Background(), 1, hd, dc, nil); clientErrorOf(t, err).Code != proto.ErrorCodeRateLimited {
			t.Fatalf("opcode %d denied err = %v, want rate_limited", opcode, err)
		}
		if n := f.next.count(); n != 1 {
			t.Fatalf("opcode %d: denied call reached Next (%d calls)", opcode, n)
		}
	}
}

func TestLifecycleOpcodesUncharged(t *testing.T) {
	f := newIngressFixture(t, RateLimitPolicy{MovePerSec: 1, IntentPerSec: 1})
	f.activatePresence(t, 1, 7)
	// Exhaust both buckets through the handler itself.
	drainMove := encodeMove(t, proto.Header{Opcode: proto.OpcodeMove, MsgVersion: 1, Tick: 1},
		proto.Move{InputSeq: 1, HeldDirs: 1, Yaw: 0})
	if err := callIngress(t, f.h, 1, drainMove, nil); err != nil {
		t.Fatal(err)
	}
	if err := callIngress(t, f.h, 1, encodeIntentFrame(t, proto.OpcodeAttack), nil); err != nil {
		t.Fatal(err)
	}
	// 121..126 delegate with zero calls to either bucket: Next runs,
	// and both buckets stay exactly as exhausted.
	for _, opcode := range []uint16{121, 122, 123, 124, 125, 126} {
		frame, err := proto.EncodeFrame(proto.Header{Opcode: opcode, MsgVersion: 1}, func(e *proto.Encoder) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		header, dec, _ := proto.DecodeFrame(frame)
		if err := f.h.Handle(context.Background(), 1, header, dec, nil); err != nil {
			t.Fatalf("opcode %d: %v", opcode, err)
		}
	}
	if n := f.next.count(); n != 7 {
		t.Fatalf("Next calls = %d, want 7 (1 intent + 6 lifecycle)", n)
	}
	if ok, _ := f.presence.AllowMove(1, f.now); ok {
		t.Fatalf("lifecycle opcodes recharged the move bucket")
	}
	if ok, _ := f.presence.AllowIntent(1, f.now); ok {
		t.Fatalf("lifecycle opcodes recharged the intent bucket")
	}
}

func TestBucketsIndependentAtHandler(t *testing.T) {
	f := newIngressFixture(t, RateLimitPolicy{MovePerSec: 1, IntentPerSec: 1})
	f.activatePresence(t, 1, 7)
	drainMove := encodeMove(t, proto.Header{Opcode: proto.OpcodeMove, MsgVersion: 1, Tick: 1},
		proto.Move{InputSeq: 1, HeldDirs: 1, Yaw: 0})
	if err := callIngress(t, f.h, 1, drainMove, nil); err != nil {
		t.Fatal(err)
	}
	// Move bucket empty, yet a 103 intent is allowed.
	if err := callIngress(t, f.h, 1, encodeIntentFrame(t, proto.OpcodeAttack), nil); err != nil {
		t.Fatalf("intent with empty move bucket: %v", err)
	}
	// Intent bucket now empty, yet time-advanced 102 is governed
	// independently (refill is per-bucket).
	f2 := newIngressFixture(t, RateLimitPolicy{MovePerSec: 1, IntentPerSec: 100})
	f2.activatePresence(t, 1, 7)
	if err := callIngress(t, f2.h, 1, encodeIntentFrame(t, proto.OpcodeAttack), nil); err != nil {
		t.Fatal(err)
	}
	if err := callIngress(t, f2.h, 1, encodeMove(t,
		proto.Header{Opcode: proto.OpcodeMove, MsgVersion: 1, Tick: 1},
		proto.Move{InputSeq: 1, HeldDirs: 1, Yaw: 0}), nil); err != nil {
		t.Fatalf("move with used intent bucket: %v", err)
	}
}

// ---------------------------------------------------------------------------
// handler rate/routing model (B72)
// ---------------------------------------------------------------------------

func TestGameplayIngressRateModel(t *testing.T) {
	const burst = 8
	policy := RateLimitPolicy{MovePerSec: burst, IntentPerSec: burst}
	for seed := uint64(0); seed < 64; seed++ {
		rng := rand.New(rand.NewPCG(seed, seed^0x9E3779B97F4A7C15))
		f := newIngressFixture(t, policy)
		f.activatePresence(t, 3, 71)
		moveLeft, intentLeft := burst, burst
		nextWant := 0
		seq := uint32(0)
		for step := 0; step < 64; step++ {
			switch rng.IntN(5) {
			case 0: // valid 102
				seq++
				frame := encodeMove(t, proto.Header{Opcode: proto.OpcodeMove, MsgVersion: 1, Tick: 9},
					proto.Move{InputSeq: seq, HeldDirs: 1, Yaw: 0})
				err := callIngress(t, f.h, 3, frame, nil)
				if moveLeft > 0 {
					moveLeft--
					if err != nil {
						t.Fatalf("seed %d step %d: allowed move: %v", seed, step, err)
					}
					if n := f.fakeSim.moveCount(); n != burst-moveLeft {
						t.Fatalf("seed %d step %d: sim moves = %d", seed, step, n)
					}
				} else if clientErrorOf(t, err).Code != proto.ErrorCodeRateLimited {
					t.Fatalf("seed %d step %d: denied move = %v", seed, step, err)
				}
			case 1: // malformed 102: never consumes
				before := burst - moveLeft
				frame, _ := proto.EncodeFrame(
					proto.Header{Opcode: proto.OpcodeMove, MsgVersion: 1},
					func(e *proto.Encoder) error { return nil })
				if err := callIngress(t, f.h, 3, frame, nil); clientErrorOf(t, err).Code != proto.ErrorCodeProtocol {
					t.Fatalf("seed %d step %d: malformed = %v", seed, step, err)
				}
				if n := f.fakeSim.moveCount(); n != before {
					t.Fatalf("seed %d step %d: malformed submitted", seed, step)
				}
			case 2: // intent opcode: Next gated on intent tokens
				op := uint16(103 + rng.IntN(18))
				frame := encodeIntentFrame(t, op)
				header, dec, _ := proto.DecodeFrame(frame)
				err := f.h.Handle(context.Background(), 3, header, dec, nil)
				if intentLeft > 0 {
					intentLeft--
					nextWant++
					if err != nil {
						t.Fatalf("seed %d step %d: allowed intent: %v", seed, step, err)
					}
					if n := f.next.count(); n != nextWant {
						t.Fatalf("seed %d step %d: Next calls = %d", seed, step, n)
					}
				} else if clientErrorOf(t, err).Code != proto.ErrorCodeRateLimited {
					t.Fatalf("seed %d step %d: denied intent = %v", seed, step, err)
				}
			case 3: // uncharged lifecycle opcode passthrough
				frame, _ := proto.EncodeFrame(
					proto.Header{Opcode: proto.OpcodeCharacterList, MsgVersion: 1},
					func(e *proto.Encoder) error { return nil })
				header, dec, _ := proto.DecodeFrame(frame)
				if err := f.h.Handle(context.Background(), 3, header, dec, nil); err != nil {
					t.Fatalf("seed %d step %d: lifecycle: %v", seed, step, err)
				}
				nextWant++
				if n := f.next.count(); n != nextWant {
					t.Fatalf("seed %d step %d: lifecycle not delegated", seed, step)
				}
			case 4: // unknown opcode passthrough
				frame, _ := proto.EncodeFrame(
					proto.Header{Opcode: 250, MsgVersion: 1},
					func(e *proto.Encoder) error { return nil })
				header, dec, _ := proto.DecodeFrame(frame)
				if err := f.h.Handle(context.Background(), 3, header, dec, nil); err != nil {
					t.Fatalf("seed %d step %d: unknown: %v", seed, step, err)
				}
				nextWant++
				if n := f.next.count(); n != nextWant {
					t.Fatalf("seed %d step %d: unknown not delegated", seed, step)
				}
			}
		}
	}
}
