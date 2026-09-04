package gateway

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/dlukt/voxilian/internal/character"
	"github.com/dlukt/voxilian/internal/proto"
	"github.com/dlukt/voxilian/internal/session"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

type fakeLookup struct {
	mu    sync.Mutex
	desc  character.Descriptor
	err   error
	calls int
}

func (f *fakeLookup) FindBySlot(_ context.Context, _ int64, _ uint8) (character.Descriptor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return character.Descriptor{}, f.err
	}
	return f.desc, nil
}

func (f *fakeLookup) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type baselineEvent struct {
	kind uint16 // 203, 218, or 220
	snap proto.CellSnapshot
	frag proto.ChunkFragment
	shop proto.ShopList
}

type fakeProvider struct {
	mu          sync.Mutex
	events      []baselineEvent
	err         error
	calls       int
	entered     chan struct{} // closed once on first entry; never reassigned
	enteredOnce sync.Once
	block       chan struct{}
	gotAcct     int64
	gotChar     int64
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{entered: make(chan struct{})}
}

func (f *fakeProvider) StreamBaseline(
	_ context.Context,
	_ session.ID,
	accountID int64,
	characterID int64,
	sink BaselineSink,
) error {
	f.mu.Lock()
	f.calls++
	f.gotAcct = accountID
	f.gotChar = characterID
	f.mu.Unlock()
	f.enteredOnce.Do(func() { close(f.entered) })
	f.mu.Lock()
	block := f.block
	events := append([]baselineEvent(nil), f.events...)
	err := f.err
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	for _, ev := range events {
		var serr error
		switch ev.kind {
		case proto.OpcodeCellSnapshot:
			serr = sink.CellSnapshot(ev.snap)
		case proto.OpcodeChunkFragment:
			serr = sink.ChunkFragment(ev.frag)
		case proto.OpcodeShopList:
			serr = sink.ShopList(ev.shop)
		default:
			return errors.New("fake provider: unknown event kind")
		}
		if serr != nil {
			return serr
		}
	}
	return err
}

func (f *fakeProvider) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// setBlock installs the mid-baseline gate channel (test side).
func (f *fakeProvider) setBlock(ch chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.block = ch
}

// closeBlock releases a blocked baseline exactly once.
func (f *fakeProvider) closeBlock() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.block != nil {
		close(f.block)
		f.block = nil
	}
}

func (f *fakeProvider) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// ---------------------------------------------------------------------------
// fixture
// ---------------------------------------------------------------------------

// enterTestTick is the injected gateway tick for handler-level frames.
const enterTestTick = 4242

// takeoverConn is a session.Connection double for cross-session
// retirement tests. It mimics the production writer contract: ONE
// context-aware gate serializes builds, build runs only inside it, and
// CloseNow never waits for the gate. writeErr models a broken socket
// at the physical write (after the builder ran); buildErr models a
// builder failure; onEvent hooks a shared ordering log.
type takeoverConn struct {
	gate      chan struct{}
	mu        sync.Mutex
	builds    int
	frames    [][]byte
	buildErr  error
	writeErr  error
	closes    []string
	closeNows int
	onEvent   func(ev string)
}

func newTakeoverConn() *takeoverConn {
	return &takeoverConn{gate: make(chan struct{}, 1)}
}

func (c *takeoverConn) WriteBinary(ctx context.Context, build session.BinaryFrameBuilder) error {
	select {
	case c.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-c.gate }()
	c.mu.Lock()
	c.builds++
	buildErr := c.buildErr
	writeErr := c.writeErr
	onEvent := c.onEvent
	c.mu.Unlock()
	if buildErr != nil {
		return buildErr
	}
	frame, err := build()
	if err != nil {
		return err
	}
	if onEvent != nil {
		onEvent("kick")
	}
	if writeErr != nil {
		return writeErr
	}
	c.mu.Lock()
	c.frames = append(c.frames, frame)
	c.mu.Unlock()
	return nil
}

func (c *takeoverConn) Close(reason string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closes = append(c.closes, reason)
	return nil
}

func (c *takeoverConn) CloseNow() error {
	c.mu.Lock()
	c.closeNows++
	onEvent := c.onEvent
	c.mu.Unlock()
	if onEvent != nil {
		onEvent("closeNow")
	}
	return nil
}

func (c *takeoverConn) setWriteErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeErr = err
}

func (c *takeoverConn) setOnEvent(fn func(ev string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onEvent = fn
}

func (c *takeoverConn) recorded() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.frames...)
}

func (c *takeoverConn) buildCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.builds
}

func (c *takeoverConn) closeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.closes)
}

func (c *takeoverConn) closeNowCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeNows
}

// eventLog is a mutex-guarded ordered event recorder shared by fakes
// that run on handler goroutines.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(ev string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, ev)
}

func (l *eventLog) slice() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

type enterFixture struct {
	reg      *session.Registry
	lookup   *fakeLookup
	provider *fakeProvider
	sends    *sendRecorder
	next     *recordingNext
	exit     *recordingExit
	conns    map[session.ID]*takeoverConn
	h        *EnterWorldHandler
}

func newEnterFixture(t *testing.T, reg *session.Registry) *enterFixture {
	t.Helper()
	if reg == nil {
		reg = session.NewRegistry()
	}
	lookup := &fakeLookup{desc: character.Descriptor{ID: 500, Slot: 0, Name: "Aria", Revision: 3}}
	provider := newFakeProvider()
	sends := &sendRecorder{}
	next := &recordingNext{}
	exit := &recordingExit{registry: reg}
	h, err := NewEnterWorldHandler(EnterWorldHandlerDeps{
		Characters: lookup,
		Registry:   reg,
		Baseline:   BaselineProviderFunc(provider.StreamBaseline),
		WorldExit:  WorldExitFunc(exit.ExitWorld),
		Tick:       func() uint32 { return enterTestTick },
		Next:       next,
	})
	if err != nil {
		t.Fatalf("NewEnterWorldHandler: %v", err)
	}
	return &enterFixture{
		reg:      reg,
		lookup:   lookup,
		provider: provider,
		sends:    sends,
		next:     next,
		exit:     exit,
		conns:    map[session.ID]*takeoverConn{},
		h:        h,
	}
}

func (f *enterFixture) authSession(t *testing.T, accountID int64) session.ID {
	t.Helper()
	conn := newTakeoverConn()
	id := f.reg.Create(conn)
	f.conns[id] = conn
	if err := f.reg.Authenticate(id, "sub", accountID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	return id
}

// inWorldSession creates a same-account session parked in IN_WORLD
// bound to characterID through test-visible registry moves — the state
// a duplicate login must take over.
func (f *enterFixture) inWorldSession(t *testing.T, accountID, characterID int64) session.ID {
	t.Helper()
	id := f.authSession(t, accountID)
	if err := f.reg.BindCharacter(id, characterID); err != nil {
		t.Fatalf("bind: %v", err)
	}
	for _, want := range []session.State{session.StateCharacterSelected, session.StateInWorld} {
		cur, _ := f.reg.Get(id)
		if err := f.reg.CompareAndSetState(id, cur.State, want); err != nil {
			t.Fatalf("cas to %s: %v", want, err)
		}
	}
	return id
}

// enterAsync runs the 124 handler on a goroutine and returns its error
// channel.
func (f *enterFixture) enterAsync(t *testing.T, sid session.ID, send SendFunc) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, _, err := callHandle(t, f.h, sid, proto.OpcodeEnterWorld, encodeEnter(t, 0), send)
		done <- err
	}()
	return done
}

// decodeKickedFrame decodes one recorded old-socket frame and asserts
// the frozen kicked wire shape: 202, msg_version 1.
func decodeKickedFrame(t *testing.T, frame []byte) (proto.Header, proto.ErrorMessage) {
	t.Helper()
	header, payload, err := proto.DecodeFrame(frame)
	if err != nil {
		t.Fatalf("decode kicked frame: %v", err)
	}
	if header.Opcode != proto.OpcodeError {
		t.Fatalf("opcode = %d, want 202", header.Opcode)
	}
	if header.MsgVersion != proto.MessageVersion1 {
		t.Fatalf("msg_version = %d, want 1", header.MsgVersion)
	}
	msg, err := proto.DecodeErrorMessage(payload)
	if err != nil {
		t.Fatalf("decode 202 payload: %v", err)
	}
	return header, msg
}

func encodeEnter(t *testing.T, slot uint8) []byte {
	t.Helper()
	frame, err := proto.EncodeFrame(proto.Header{Opcode: proto.OpcodeEnterWorld, MsgVersion: 1},
		func(e *proto.Encoder) error {
			proto.EnterWorld{Slot: slot}.Encode(e)
			return nil
		})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return frame
}

func testCellSnapshot() proto.CellSnapshot {
	return proto.CellSnapshot{
		Cell: proto.Cell{X: 1, Z: -2},
		Entities: []proto.EntityEntry{
			{Entity: 7, Kind: 1, Proto: 9, Pos: proto.Position{X: 100, Y: 200, Z: 300}, Angle: 100, Speed: 5},
		},
	}
}

func testChunkFragment(fragIdx uint16) proto.ChunkFragment {
	return proto.ChunkFragment{
		Cell:      proto.Cell{X: 1, Z: -2},
		ChunkIdx:  3,
		FragIdx:   fragIdx,
		FragCount: 2,
		Bytes:     []byte{byte(fragIdx), 9, 8},
	}
}

func testShopList() proto.ShopList {
	return proto.ShopList{
		Vendor: 42,
		Listings: []proto.ShopListingEntry{
			{Listing: 5, Price: 150, Qty: 10},
			{Listing: 6, Price: 50, Qty: 3},
		},
	}
}

// ---------------------------------------------------------------------------
// constructor + malformed + lookup mapping
// ---------------------------------------------------------------------------

func TestNewEnterWorldHandlerRequiresDeps(t *testing.T) {
	provider := BaselineProviderFunc(func(context.Context, session.ID, int64, int64, BaselineSink) error {
		return nil
	})
	exit := WorldExitFunc(func(context.Context, session.ID, int64, int64) error { return nil })
	tick := func() uint32 { return 1 }
	deps := func(mut func(*EnterWorldHandlerDeps)) EnterWorldHandlerDeps {
		d := EnterWorldHandlerDeps{
			Characters: &fakeLookup{},
			Registry:   session.NewRegistry(),
			Baseline:   provider,
			WorldExit:  exit,
			Tick:       tick,
		}
		if mut != nil {
			mut(&d)
		}
		return d
	}
	if _, err := NewEnterWorldHandler(deps(func(d *EnterWorldHandlerDeps) { d.Characters = nil })); err == nil {
		t.Error("nil lookup accepted")
	}
	if _, err := NewEnterWorldHandler(deps(func(d *EnterWorldHandlerDeps) { d.Registry = nil })); err == nil {
		t.Error("nil registry accepted")
	}
	if _, err := NewEnterWorldHandler(deps(func(d *EnterWorldHandlerDeps) { d.Baseline = nil })); err == nil {
		t.Error("nil provider accepted (no silent no-op default)")
	}
	if _, err := NewEnterWorldHandler(deps(func(d *EnterWorldHandlerDeps) { d.WorldExit = nil })); err == nil {
		t.Error("nil world exit accepted")
	}
	if _, err := NewEnterWorldHandler(deps(func(d *EnterWorldHandlerDeps) { d.Tick = nil })); err == nil {
		t.Error("nil tick accepted")
	}
	if _, err := NewEnterWorldHandler(deps(nil)); err != nil {
		t.Errorf("nil Next rejected: %v", err)
	}
}

func TestEnterMalformed(t *testing.T) {
	f := newEnterFixture(t, nil)
	sid := f.authSession(t, 11)
	// A complete 12-byte frame with an empty body: the outer framing is
	// valid, but EnterWorld requires its 1-byte slot payload.
	empty, err := proto.EncodeFrame(proto.Header{Opcode: proto.OpcodeEnterWorld, MsgVersion: 1},
		func(e *proto.Encoder) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	_, _, herr := callHandle(t, f.h, sid, proto.OpcodeEnterWorld, empty, f.sends.send)
	var cerr *ClientError
	if !errors.As(herr, &cerr) || cerr.Code != proto.ErrorCodeProtocol {
		t.Errorf("err = %v, want protocol_error", herr)
	}
	if n := f.provider.callCount(); n != 0 {
		t.Errorf("provider calls = %d, want 0", n)
	}
}

func TestEnterLookupMapping(t *testing.T) {
	cases := []struct {
		name      string
		lookupErr error
		rejected  bool // true → 217 rejected; false → ClientError below
		code      uint16
	}{
		{"invalid slot", character.ErrInvalidSlot, true, 0},
		{"empty slot", character.ErrNotFound, true, 0},
		{"persistence", character.ErrPersistence, false, proto.ErrorCodeRetry},
		{"invalid content", character.ErrInvalidContent, false, 0}, // internal sentinel
		{"unexpected", errors.New("boom"), false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newEnterFixture(t, nil)
			sid := f.authSession(t, 11)
			f.lookup.err = tc.lookupErr
			_, _, err := callHandle(t, f.h, sid, proto.OpcodeEnterWorld,
				encodeEnter(t, 0), f.sends.send)
			switch {
			case tc.rejected:
				if err != nil {
					t.Fatalf("err = %v, want 217 rejected", err)
				}
				frames := f.sends.all()
				if len(frames) != 1 {
					t.Fatalf("frames = %d", len(frames))
				}
				if got := decodeOp(t, frames[0]); got.Op != proto.CharacterOpEnterWorld ||
					got.OK != proto.CharacterOpRejected {
					t.Errorf("217 = %+v, want enter/rejected", got)
				}
			case tc.name == "persistence":
				var cerr *ClientError
				if !errors.As(err, &cerr) || cerr.Code != tc.code {
					t.Errorf("err = %v, want retry", err)
				}
			default:
				var cerr *ClientError
				if errors.As(err, &cerr) {
					t.Errorf("internal became %v", cerr)
				}
				if err == nil {
					t.Error("expected internal failure")
				}
			}
			if n := f.provider.callCount(); n != 0 {
				t.Errorf("provider calls = %d, want 0 (lookup before begin)", n)
			}
			snap, _ := f.reg.Get(sid)
			if snap.State != session.StateAuthenticated || snap.HasCharacter {
				t.Errorf("registry mutated: %+v", snap)
			}
		})
	}
}

func TestEnterVanishedAndReRead(t *testing.T) {
	f := newEnterFixture(t, nil)
	_, _, err := callHandle(t, f.h, 9999, proto.OpcodeEnterWorld,
		encodeEnter(t, 0), f.sends.send)
	if err == nil {
		t.Error("vanished session must fail internal")
	}
	// Session that left AUTHENTICATED before the call: gate bypass is an
	// internal invariant failure, never a baseline.
	sid := f.authSession(t, 11)
	cur, _ := f.reg.Get(sid)
	if err := f.reg.CompareAndSetState(sid, cur.State, session.StateCharacterSelected); err != nil {
		t.Fatal(err)
	}
	_, _, err = callHandle(t, f.h, sid, proto.OpcodeEnterWorld, encodeEnter(t, 0), f.sends.send)
	var cerr *ClientError
	if errors.As(err, &cerr) {
		t.Errorf("re-read mismatch became %v; must stay internal", cerr)
	}
	if err == nil {
		t.Error("expected internal failure")
	}
	if n := f.provider.callCount(); n != 0 {
		t.Errorf("provider calls = %d, want 0", n)
	}
}

func TestEnterBeginCharacterInUse(t *testing.T) {
	f := newEnterFixture(t, nil)
	sid := f.authSession(t, 11)
	// Character 500 bound to a DIFFERENT account's session: arbitration
	// (same-account only) passes, Begin must fail closed as internal —
	// never wire character_in_use for this enter.
	other := f.authSession(t, 12)
	if err := f.reg.BindCharacter(other, 500); err != nil {
		t.Fatal(err)
	}
	_, _, err := callHandle(t, f.h, sid, proto.OpcodeEnterWorld, encodeEnter(t, 0), f.sends.send)
	var cerr *ClientError
	if errors.As(err, &cerr) {
		t.Errorf("ownership invariant became %v; must stay internal", cerr)
	}
	if err == nil {
		t.Error("expected internal failure")
	}
	snap, _ := f.reg.Get(sid)
	if snap.State != session.StateAuthenticated || snap.HasCharacter {
		t.Errorf("mutated: %+v", snap)
	}
	if frames := f.sends.all(); len(frames) != 0 {
		t.Errorf("frames = %d, want none before abort path", len(frames))
	}
}

// ---------------------------------------------------------------------------
// takeover (M3-T4b)
// ---------------------------------------------------------------------------

func mustGet(t *testing.T, r *session.Registry, id session.ID) session.Snapshot {
	t.Helper()
	snap, ok := r.Get(id)
	if !ok {
		t.Fatalf("session %d vanished", uint64(id))
	}
	return snap
}

// countWorldActive reports how many same-sub registry sessions are
// CHARACTER_SELECTED or IN_WORLD.
func countWorldActive(t *testing.T, r *session.Registry, sub string) int {
	t.Helper()
	n := 0
	for _, id := range r.SessionsBySub(sub) {
		if s, ok := r.Get(id); ok &&
			(s.State == session.StateCharacterSelected || s.State == session.StateInWorld) {
			n++
		}
	}
	return n
}

// TestTakeoverSameAndDifferentCharacter is the shared takeover matrix:
// the old same-account IN_WORLD session is flushed with its OWN
// character, retired, kicked, and force-closed, and the new session
// baselines the REQUESTED character to IN_WORLD as the only
// world-active session.
func TestTakeoverSameAndDifferentCharacter(t *testing.T) {
	cases := []struct {
		name    string
		oldChar int64
		reqChar int64
	}{
		{"same character", 500, 500},
		{"different character", 600, 500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newEnterFixture(t, nil)
			old := f.inWorldSession(t, 11, tc.oldChar)
			fresh := f.authSession(t, 11)

			_, _, err := callHandle(t, f.h, fresh, proto.OpcodeEnterWorld,
				encodeEnter(t, 0), f.sends.send)
			if err != nil {
				t.Fatalf("takeover err = %v", err)
			}

			// WorldExit flushed the OLD session exactly once with the
			// OLD character — never the requested new one.
			if n := f.exit.callCount(); n != 1 {
				t.Fatalf("WorldExit calls = %d, want 1", n)
			}
			args := f.exit.argCopy()
			if args[0].sid != old || args[0].acct != 11 || args[0].char != tc.oldChar {
				t.Fatalf("WorldExit args = %+v, want old sid %d account 11 char %d",
					args[0], old, tc.oldChar)
			}

			// Old is retired: gone from every index it owned. For the
			// same-character case the character is immediately re-bound
			// by the replacement; for the different-character case it
			// must be fully free.
			if _, ok := f.reg.Get(old); ok {
				t.Error("old session still registered")
			}
			if owner, ok := f.reg.SessionByCharacter(tc.oldChar); ok && owner != fresh {
				t.Errorf("old character %d still indexed by %d", tc.oldChar, owner)
			}
			// Old received exactly one 202 kicked frame with the
			// injected tick, then one forced close.
			oc := f.conns[old]
			frames := oc.recorded()
			if len(frames) != 1 {
				t.Fatalf("old frames = %d, want exactly the kicked 202", len(frames))
			}
			header, msg := decodeKickedFrame(t, frames[0])
			if msg.Code != proto.ErrorCodeKicked {
				t.Errorf("kicked code = %d, want %d", msg.Code, proto.ErrorCodeKicked)
			}
			if msg.Message != "session replaced by another login" {
				t.Errorf("kicked message = %q, want the static diagnostic", msg.Message)
			}
			if header.Tick != enterTestTick {
				t.Errorf("kicked tick = %d, want injected %d", header.Tick, enterTestTick)
			}
			if header.Seq != 1 {
				t.Errorf("kicked seq = %d, want old session's next seq 1", header.Seq)
			}
			if n := oc.closeNowCount(); n != 1 {
				t.Errorf("CloseNow calls = %d, want 1", n)
			}
			if n := oc.closeCount(); n != 0 {
				t.Errorf("graceful Close calls = %d, want 0 (forced close only)", n)
			}

			// New baseline ran exactly once for the REQUESTED character.
			if n := f.provider.callCount(); n != 1 {
				t.Fatalf("provider calls = %d, want 1", n)
			}
			assertEnterSuccess(t, f, fresh, []uint16{proto.OpcodeCharacterOp, proto.OpcodeWorldReady})
			f.provider.mu.Lock()
			gotChar := f.provider.gotChar
			gotAcct := f.provider.gotAcct
			f.provider.mu.Unlock()
			if gotChar != tc.reqChar || gotAcct != 11 {
				t.Errorf("provider args = %d/%d, want requested %d/11", gotAcct, gotChar, tc.reqChar)
			}

			// Exactly one world-active session remains, and it is new.
			if n := countWorldActive(t, f.reg, "sub"); n != 1 {
				t.Errorf("world-active sessions = %d, want 1", n)
			}
			if ids := f.reg.SessionsBySub("sub"); len(ids) != 1 || ids[0] != fresh {
				t.Errorf("live sessions = %v, want only the replacement", ids)
			}
		})
	}
}

// TestTakeoverKickedUsesOldSessionSeq proves the kicked frame consumes
// the OLD session's own sequence counter — completely unrelated to the
// new session's.
func TestTakeoverKickedUsesOldSessionSeq(t *testing.T) {
	f := newEnterFixture(t, nil)
	old := f.inWorldSession(t, 11, 500)
	fresh := f.authSession(t, 11)
	// Diverge the counters: 3 frames already sent on old, 2 on new.
	for i := 0; i < 3; i++ {
		if _, err := f.reg.NextServerSeq(old); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := f.reg.NextServerSeq(fresh); err != nil {
			t.Fatal(err)
		}
	}

	_, _, err := callHandle(t, f.h, fresh, proto.OpcodeEnterWorld, encodeEnter(t, 0), f.sends.send)
	if err != nil {
		t.Fatalf("takeover err = %v", err)
	}
	frames := f.conns[old].recorded()
	if len(frames) != 1 {
		t.Fatalf("old frames = %d, want 1", len(frames))
	}
	header, msg := decodeKickedFrame(t, frames[0])
	if msg.Code != proto.ErrorCodeKicked {
		t.Fatalf("code = %d, want kicked", msg.Code)
	}
	if header.Seq != 4 {
		t.Errorf("kicked seq = %d, want 4 (old session's 4th — never the new counter)", header.Seq)
	}
	// The old session's counter stays frozen at its kicked value; the
	// new session's counter is untouched by the kick.
	if seq, err := f.reg.NextServerSeq(fresh); err != nil || seq != 3 {
		t.Errorf("new session seq = %d,%v want 3 (kick never touched it)", seq, err)
	}
}

// TestTakeoverEventOrder proves the stale-PG safety barrier ordering:
// old flush → old unbind (implicitly between flush and kick) → kicked
// write → forced close → new baseline.
func TestTakeoverEventOrder(t *testing.T) {
	reg := session.NewRegistry()
	log := &eventLog{}
	exit := WorldExitFunc(func(context.Context, session.ID, int64, int64) error {
		log.add("flush-old")
		return nil
	})
	provider := BaselineProviderFunc(func(context.Context, session.ID, int64, int64, BaselineSink) error {
		log.add("baseline-new")
		return nil
	})
	h, err := NewEnterWorldHandler(EnterWorldHandlerDeps{
		Characters: &fakeLookup{desc: character.Descriptor{ID: 500, Slot: 0, Name: "Aria"}},
		Registry:   reg,
		Baseline:   provider,
		WorldExit:  exit,
		Tick:       func() uint32 { return enterTestTick },
	})
	if err != nil {
		t.Fatalf("NewEnterWorldHandler: %v", err)
	}
	oldConn := newTakeoverConn()
	oldConn.setOnEvent(func(ev string) { log.add(ev + "-old") })
	old := reg.Create(oldConn)
	if err := reg.Authenticate(old, "sub", 11, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := reg.BindCharacter(old, 500); err != nil {
		t.Fatal(err)
	}
	for _, want := range []session.State{session.StateCharacterSelected, session.StateInWorld} {
		cur, _ := reg.Get(old)
		if err := reg.CompareAndSetState(old, cur.State, want); err != nil {
			t.Fatal(err)
		}
	}
	fresh := reg.Create(newTakeoverConn())
	if err := reg.Authenticate(fresh, "sub", 11, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	sends := &sendRecorder{}
	_, _, herr := callHandle(t, h, fresh, proto.OpcodeEnterWorld, encodeEnter(t, 0), sends.send)
	if herr != nil {
		t.Fatalf("takeover err = %v", herr)
	}
	want := []string{"flush-old", "kick-old", "closeNow-old", "baseline-new"}
	if got := log.slice(); !reflect.DeepEqual(got, want) {
		t.Fatalf("event order = %v, want %v", got, want)
	}
}

// TestTakeoverFlushBlocksBaseline: while the old WorldExit is blocked,
// no new baseline, no new binding, no kick, no close — the flush is the
// barrier.
func TestTakeoverFlushBlocksBaseline(t *testing.T) {
	f := newEnterFixture(t, nil)
	old := f.inWorldSession(t, 11, 500)
	fresh := f.authSession(t, 11)

	f.exit.setEntered(make(chan struct{}))
	f.exit.setBlock(make(chan struct{}))
	done := f.enterAsync(t, fresh, f.sends.send)
	<-f.exit.enteredCh()

	// While the flush is blocked: nothing downstream ran.
	if n := f.provider.callCount(); n != 0 {
		t.Fatalf("provider calls = %d while flush blocked", n)
	}
	if s := mustGet(t, f.reg, fresh); s.State != session.StateAuthenticated || s.HasCharacter {
		t.Fatalf("new session mutated during flush: %+v", s)
	}
	if s := mustGet(t, f.reg, old); s.State != session.StateInWorld || !s.HasCharacter {
		t.Fatalf("old session disturbed during flush: %+v", s)
	}
	if owner, ok := f.reg.SessionByCharacter(500); !ok || owner != old {
		t.Fatalf("old binding disturbed: %d,%v", owner, ok)
	}
	oc := f.conns[old]
	if frames := oc.recorded(); len(frames) != 0 {
		t.Fatalf("old socket wrote %d frames during flush", len(frames))
	}
	if n := oc.closeNowCount(); n != 0 {
		t.Fatalf("old CloseNow calls = %d during flush", n)
	}

	f.exit.closeBlock()
	if err := <-done; err != nil {
		t.Fatalf("takeover err = %v", err)
	}
	if n := f.exit.callCount(); n != 1 {
		t.Errorf("WorldExit calls = %d, want 1", n)
	}
	if _, ok := f.reg.Get(old); ok {
		t.Error("old still registered after takeover")
	}
	if s := mustGet(t, f.reg, fresh); s.State != session.StateInWorld {
		t.Errorf("new = %+v, want IN_WORLD", s)
	}
}

// TestTakeoverFlushFailure proves the rollback contract: a failed old
// flush yields retry for the new session with the old session fully
// untouched, and a later successful retry on the same new session
// completes the takeover.
func TestTakeoverFlushFailure(t *testing.T) {
	f := newEnterFixture(t, nil)
	old := f.inWorldSession(t, 11, 500)
	fresh := f.authSession(t, 11)

	f.exit.setErr(errors.New("flush failed"))
	_, _, err := callHandle(t, f.h, fresh, proto.OpcodeEnterWorld, encodeEnter(t, 0), f.sends.send)
	var cerr *ClientError
	if !errors.As(err, &cerr) || cerr.Code != proto.ErrorCodeRetry {
		t.Fatalf("err = %v, want retry ClientError", err)
	}
	// New: untouched, no frames.
	if s := mustGet(t, f.reg, fresh); s.State != session.StateAuthenticated || s.HasCharacter {
		t.Errorf("new mutated: %+v", s)
	}
	if frames := f.sends.all(); len(frames) != 0 {
		t.Errorf("new frames = %d, want none (retry is emitted by Server)", len(frames))
	}
	// Old: still IN_WORLD, bound, registered, un-kicked, open.
	if s := mustGet(t, f.reg, old); s.State != session.StateInWorld || !s.HasCharacter || s.CharacterID != 500 {
		t.Errorf("old disturbed by failed flush: %+v", s)
	}
	if owner, ok := f.reg.SessionByCharacter(500); !ok || owner != old {
		t.Errorf("old index disturbed: %d,%v", owner, ok)
	}
	oc := f.conns[old]
	if frames := oc.recorded(); len(frames) != 0 {
		t.Errorf("old socket frames = %d, want no kicked error", len(frames))
	}
	if n := oc.closeNowCount(); n != 0 {
		t.Errorf("old CloseNow = %d, want 0", n)
	}
	if n := f.provider.callCount(); n != 0 {
		t.Errorf("provider calls = %d, want no baseline", n)
	}

	// Same new session retries once the flush is healthy.
	f.exit.setErr(nil)
	_, _, err = callHandle(t, f.h, fresh, proto.OpcodeEnterWorld, encodeEnter(t, 0), f.sends.send)
	if err != nil {
		t.Fatalf("retry err = %v", err)
	}
	assertEnterSuccess(t, f, fresh, []uint16{proto.OpcodeCharacterOp, proto.OpcodeWorldReady})
	if _, ok := f.reg.Get(old); ok {
		t.Error("old still registered after successful retry")
	}
}

// TestTakeoverBrokenKickedWriter: a broken old socket cannot prevent
// the takeover — retirement and forced close still happen, the new
// baseline still completes.
func TestTakeoverBrokenKickedWriter(t *testing.T) {
	f := newEnterFixture(t, nil)
	old := f.inWorldSession(t, 11, 500)
	fresh := f.authSession(t, 11)
	f.conns[old].setWriteErr(errors.New("socket dead"))

	_, _, err := callHandle(t, f.h, fresh, proto.OpcodeEnterWorld, encodeEnter(t, 0), f.sends.send)
	if err != nil {
		t.Fatalf("takeover err = %v (broken old writer must not produce retry)", err)
	}
	if _, ok := f.reg.Get(old); ok {
		t.Error("old still registered")
	}
	oc := f.conns[old]
	if n := oc.buildCount(); n != 1 {
		t.Errorf("kick builder runs = %d, want 1", n)
	}
	if frames := oc.recorded(); len(frames) != 0 {
		t.Errorf("frames recorded on broken writer = %d, want 0", len(frames))
	}
	if n := oc.closeNowCount(); n != 1 {
		t.Errorf("CloseNow = %d, want 1", n)
	}
	if s := mustGet(t, f.reg, fresh); s.State != session.StateInWorld {
		t.Errorf("new = %+v, want IN_WORLD", s)
	}
}

// TestTakeoverKickWriterBusyTimesOut: an occupied old writer that
// never grants serialization within the bounded kick budget loses the
// kick frame, but retirement and forced close are mandatory.
func TestTakeoverKickWriterBusyTimesOut(t *testing.T) {
	f := newEnterFixture(t, nil)
	old := f.inWorldSession(t, 11, 500)
	fresh := f.authSession(t, 11)
	oc := f.conns[old]
	// Occupy the writer slot from the test side; nothing releases it
	// until the takeover is over.
	oc.gate <- struct{}{}
	defer func() { <-oc.gate }()

	save := kickWriteBudget
	kickWriteBudget = 5 * time.Millisecond
	defer func() { kickWriteBudget = save }()

	done := f.enterAsync(t, fresh, f.sends.send)
	if err := <-done; err != nil {
		t.Fatalf("takeover err = %v (busy old writer must not block retirement)", err)
	}
	if _, ok := f.reg.Get(old); ok {
		t.Error("old still registered despite busy writer")
	}
	if n := oc.buildCount(); n != 0 {
		t.Errorf("kick builder runs = %d, want 0 (writer never obtained)", n)
	}
	if frames := oc.recorded(); len(frames) != 0 {
		t.Errorf("frames = %d, want 0 (kick frame lost)", len(frames))
	}
	if n := oc.closeNowCount(); n != 1 {
		t.Errorf("CloseNow = %d, want 1 (retirement is not best effort)", n)
	}
	if s := mustGet(t, f.reg, fresh); s.State != session.StateInWorld {
		t.Errorf("new = %+v, want IN_WORLD", s)
	}
}

// TestTakeoverOldVanishedDuringFlush: the old transport may disappear
// (registry removal races past the account guard) while the flush is
// in flight; once the flush SUCCEEDS, ErrNotFound from the atomic
// unbind means "already retired" and the takeover proceeds.
func TestTakeoverOldVanishedDuringFlush(t *testing.T) {
	f := newEnterFixture(t, nil)
	old := f.inWorldSession(t, 11, 500)
	fresh := f.authSession(t, 11)

	f.exit.setEntered(make(chan struct{}))
	f.exit.setBlock(make(chan struct{}))
	done := f.enterAsync(t, fresh, f.sends.send)
	<-f.exit.enteredCh()
	f.reg.Remove(old) // old socket dies mid-flush
	f.exit.closeBlock()
	if err := <-done; err != nil {
		t.Fatalf("takeover err = %v (vanished old must not fail the replacement)", err)
	}
	if n := f.exit.callCount(); n != 1 {
		t.Errorf("WorldExit calls = %d, want 1", n)
	}
	if _, ok := f.reg.Get(old); ok {
		t.Error("old still registered")
	}
	// No kick frame is possible or required: the session vanished.
	if frames := f.conns[old].recorded(); len(frames) != 0 {
		t.Errorf("old frames = %d, want 0", len(frames))
	}
	if s := mustGet(t, f.reg, fresh); s.State != session.StateInWorld || s.CharacterID != 500 {
		t.Errorf("new = %+v, want IN_WORLD bound to 500", s)
	}
}

// TestTakeoverOldVanishedDuringFlushFailure: a vanished old socket
// never makes a FAILED durable flush safe — the new session still gets
// retry and no baseline.
func TestTakeoverOldVanishedDuringFlushFailure(t *testing.T) {
	f := newEnterFixture(t, nil)
	old := f.inWorldSession(t, 11, 500)
	fresh := f.authSession(t, 11)

	f.exit.setErr(errors.New("flush failed"))
	f.exit.setEntered(make(chan struct{}))
	f.exit.setBlock(make(chan struct{}))
	done := f.enterAsync(t, fresh, f.sends.send)
	<-f.exit.enteredCh()
	f.reg.Remove(old) // old socket dies mid-flush, and the flush fails
	f.exit.closeBlock()
	err := <-done
	var cerr *ClientError
	if !errors.As(err, &cerr) || cerr.Code != proto.ErrorCodeRetry {
		t.Fatalf("err = %v, want retry", err)
	}
	if n := f.provider.callCount(); n != 0 {
		t.Errorf("provider calls = %d, want 0", n)
	}
	if s := mustGet(t, f.reg, fresh); s.State != session.StateAuthenticated || s.HasCharacter {
		t.Errorf("new mutated: %+v", s)
	}
}

// TestTakeoverCharacterSelectedInvariant: a CHARACTER_SELECTED
// same-account candidate cannot legitimately survive the account-guard
// acquisition — fail closed without flushing, kicking, or starting a
// baseline.
func TestTakeoverCharacterSelectedInvariant(t *testing.T) {
	f := newEnterFixture(t, nil)
	fresh := f.authSession(t, 11)
	selected := f.authSession(t, 11)
	if err := f.reg.BindCharacter(selected, 600); err != nil {
		t.Fatal(err)
	}
	cur, _ := f.reg.Get(selected)
	if err := f.reg.CompareAndSetState(selected, cur.State, session.StateCharacterSelected); err != nil {
		t.Fatal(err)
	}

	_, _, err := callHandle(t, f.h, fresh, proto.OpcodeEnterWorld, encodeEnter(t, 0), f.sends.send)
	var cerr *ClientError
	if errors.As(err, &cerr) {
		t.Fatalf("invariant became client error %v; must stay internal", cerr)
	}
	if err == nil || !errors.Is(err, session.ErrInvariant) {
		t.Fatalf("err = %v, want internal ErrInvariant", err)
	}
	if n := f.exit.callCount(); n != 0 {
		t.Errorf("WorldExit calls = %d, want 0", n)
	}
	if n := f.provider.callCount(); n != 0 {
		t.Errorf("provider calls = %d, want 0", n)
	}
	if s := mustGet(t, f.reg, selected); s.State != session.StateCharacterSelected || !s.HasCharacter {
		t.Errorf("selected candidate disturbed: %+v", s)
	}
	if s := mustGet(t, f.reg, fresh); s.State != session.StateAuthenticated || s.HasCharacter {
		t.Errorf("new mutated: %+v", s)
	}
}

// TestTakeoverCandidateWithoutConnection: an IN_WORLD candidate with
// no retained connection is an identity invariant failure — never a
// silent takeover of an arbitrary session.
func TestTakeoverCandidateWithoutConnection(t *testing.T) {
	f := newEnterFixture(t, nil)
	fresh := f.authSession(t, 11)
	// Same-account IN_WORLD session whose registry entry holds no
	// connection (impossible through ServeHTTP; constructed directly).
	bare := f.reg.Create(nil)
	if err := f.reg.Authenticate(bare, "sub", 11, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.BindCharacter(bare, 600); err != nil {
		t.Fatal(err)
	}
	for _, want := range []session.State{session.StateCharacterSelected, session.StateInWorld} {
		cur, _ := f.reg.Get(bare)
		if err := f.reg.CompareAndSetState(bare, cur.State, want); err != nil {
			t.Fatal(err)
		}
	}

	_, _, err := callHandle(t, f.h, fresh, proto.OpcodeEnterWorld, encodeEnter(t, 0), f.sends.send)
	var cerr *ClientError
	if errors.As(err, &cerr) {
		t.Fatalf("invariant became client error %v", cerr)
	}
	if err == nil {
		t.Fatal("expected internal invariant failure")
	}
	if n := f.exit.callCount(); n != 0 {
		t.Errorf("WorldExit calls = %d, want 0", n)
	}
	if s := mustGet(t, f.reg, bare); s.State != session.StateInWorld || !s.HasCharacter {
		t.Errorf("bare candidate disturbed: %+v", s)
	}
}

// TestTakeoverNewBaselineFailureRollsBackNewOnly: once the old session
// is flushed and retired, a failing NEW baseline applies the
// provisional-baseline rule to the new session alone — no old-session
// resurrection.
func TestTakeoverNewBaselineFailureRollsBackNewOnly(t *testing.T) {
	f := newEnterFixture(t, nil)
	old := f.inWorldSession(t, 11, 500)
	fresh := f.authSession(t, 11)
	f.provider.setErr(errors.New("cells unavailable"))

	_, _, err := callHandle(t, f.h, fresh, proto.OpcodeEnterWorld, encodeEnter(t, 0), f.sends.send)
	var cerr *ClientError
	if !errors.As(err, &cerr) || cerr.Code != proto.ErrorCodeRetry {
		t.Fatalf("err = %v, want retry", err)
	}
	// New rolled back; old stays retired (never resurrected).
	if s := mustGet(t, f.reg, fresh); s.State != session.StateAuthenticated || s.HasCharacter {
		t.Errorf("new not rolled back: %+v", s)
	}
	if _, ok := f.reg.Get(old); ok {
		t.Error("old resurrected after new baseline failure")
	}
	if _, ok := f.reg.SessionByCharacter(500); ok {
		t.Error("old character re-bound after new baseline failure")
	}
	if n := f.exit.callCount(); n != 1 {
		t.Errorf("WorldExit calls = %d, want 1 (no re-flush of the retired old)", n)
	}
	if n := f.conns[old].closeNowCount(); n != 1 {
		t.Errorf("old CloseNow = %d, want 1", n)
	}
}

// TestTakeoverNew217WriteFailureAfterOldRetired: after the old session
// is retired, a failed new 217 write aborts the NEW selection and
// terminates; the old stays retired.
func TestTakeoverNew217WriteFailureAfterOldRetired(t *testing.T) {
	f := newEnterFixture(t, nil)
	old := f.inWorldSession(t, 11, 500)
	fresh := f.authSession(t, 11)
	boom := errors.New("socket dead")
	fail := func(uint16, uint16, func(*proto.Encoder) error) error { return boom }

	_, _, err := callHandle(t, f.h, fresh, proto.OpcodeEnterWorld, encodeEnter(t, 0), fail)
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("err = %v, want raw write error", err)
	}
	if s := mustGet(t, f.reg, fresh); s.State != session.StateAuthenticated || s.HasCharacter {
		t.Errorf("new not rolled back: %+v", s)
	}
	if _, ok := f.reg.Get(old); ok {
		t.Error("old resurrected after new 217 failure")
	}
}

func TestEnterArbitrationInvariantError(t *testing.T) {
	f := newEnterFixture(t, nil)
	sid := f.authSession(t, 11)
	// Force two world-active same-account sessions via direct binds
	// (BeginEnterWorld would refuse the second): the query must fail
	// closed, never pick one by map order.
	for _, charID := range []int64{601, 602} {
		other := f.authSession(t, 11)
		if err := f.reg.BindCharacter(other, charID); err != nil {
			t.Fatal(err)
		}
		cur, _ := f.reg.Get(other)
		if err := f.reg.CompareAndSetState(other, cur.State, session.StateInWorld); err != nil {
			t.Fatal(err)
		}
	}
	_, _, err := callHandle(t, f.h, sid, proto.OpcodeEnterWorld, encodeEnter(t, 0), f.sends.send)
	var cerr *ClientError
	if errors.As(err, &cerr) {
		t.Errorf("invariant became %v; must stay internal", cerr)
	}
	if err == nil {
		t.Error("expected internal invariant failure")
	}
}

// ---------------------------------------------------------------------------
// baselines
// ---------------------------------------------------------------------------

func assertEnterSuccess(t *testing.T, f *enterFixture, sid session.ID, wantOpcodes []uint16) []sentFrame {
	t.Helper()
	frames := f.sends.all()
	if len(frames) != len(wantOpcodes) {
		t.Fatalf("frames = %d, want %d", len(frames), len(wantOpcodes))
	}
	for i, want := range wantOpcodes {
		if frames[i].opcode != want {
			t.Fatalf("frame %d opcode = %d, want %d", i, frames[i].opcode, want)
		}
		if frames[i].version != proto.MessageVersion1 {
			t.Fatalf("frame %d version = %d, want 1", i, frames[i].version)
		}
	}
	// First lifecycle reply 217 enter OK, last baseline reply 219.
	if got := decodeOp(t, frames[0]); got.Op != proto.CharacterOpEnterWorld || got.OK != proto.CharacterOpOK {
		t.Fatalf("first = %+v, want 217 enter OK", got)
	}
	if _, err := proto.DecodeWorldReady(proto.NewDecoder(frames[len(frames)-1].payload)); err != nil {
		t.Fatalf("last 219 decode: %v", err)
	}
	snap := mustGet(t, f.reg, sid)
	if snap.State != session.StateInWorld || !snap.HasCharacter || snap.CharacterID != 500 {
		t.Fatalf("registry = %+v, want IN_WORLD bound to 500", snap)
	}
	if owner, ok := f.reg.SessionByCharacter(500); !ok || owner != sid {
		t.Fatalf("index = %d,%v", owner, ok)
	}
	return frames
}

func TestEnterEmptyBaseline(t *testing.T) {
	f := newEnterFixture(t, nil)
	sid := f.authSession(t, 11)
	_, _, err := callHandle(t, f.h, sid, proto.OpcodeEnterWorld, encodeEnter(t, 0), f.sends.send)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	assertEnterSuccess(t, f, sid, []uint16{proto.OpcodeCharacterOp, proto.OpcodeWorldReady})
}

func TestEnterMixedOrderedBaseline(t *testing.T) {
	f := newEnterFixture(t, nil)
	sid := f.authSession(t, 11)
	snap0 := testCellSnapshot()
	frag0 := testChunkFragment(0)
	frag1 := testChunkFragment(1)
	shop := testShopList()
	f.provider.events = []baselineEvent{
		{kind: proto.OpcodeCellSnapshot, snap: snap0},
		{kind: proto.OpcodeChunkFragment, frag: frag0},
		{kind: proto.OpcodeChunkFragment, frag: frag1},
		{kind: proto.OpcodeCellSnapshot, snap: snap0},
		{kind: proto.OpcodeShopList, shop: shop},
	}
	_, _, err := callHandle(t, f.h, sid, proto.OpcodeEnterWorld, encodeEnter(t, 0), f.sends.send)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	frames := assertEnterSuccess(t, f, sid, []uint16{
		proto.OpcodeCharacterOp,
		proto.OpcodeCellSnapshot,
		proto.OpcodeChunkFragment,
		proto.OpcodeChunkFragment,
		proto.OpcodeCellSnapshot,
		proto.OpcodeShopList,
		proto.OpcodeWorldReady,
	})
	// Decode every payload and compare semantics.
	if got, err := proto.DecodeCellSnapshot(proto.NewDecoder(frames[1].payload)); err != nil ||
		!reflect.DeepEqual(got, snap0) {
		t.Errorf("203 = %+v, %v", got, err)
	}
	for i, want := range []proto.ChunkFragment{frag0, frag1} {
		if got, err := proto.DecodeChunkFragment(proto.NewDecoder(frames[2+i].payload)); err != nil ||
			!reflect.DeepEqual(got, want) {
			t.Errorf("218[%d] = %+v, %v", i, got, err)
		}
	}
	if got, err := proto.DecodeCellSnapshot(proto.NewDecoder(frames[4].payload)); err != nil ||
		!reflect.DeepEqual(got, snap0) {
		t.Errorf("203b = %+v, %v", got, err)
	}
	if got, err := proto.DecodeShopList(proto.NewDecoder(frames[5].payload)); err != nil ||
		!reflect.DeepEqual(got, shop) {
		t.Errorf("220 = %+v, %v", got, err)
	}
	// Provider received the descriptor identity, never wire IDs.
	f.provider.mu.Lock()
	defer f.provider.mu.Unlock()
	if f.provider.gotAcct != 11 || f.provider.gotChar != 500 {
		t.Errorf("provider args = %d/%d, want 11/500", f.provider.gotAcct, f.provider.gotChar)
	}
}

func TestEnter217WriteFailure(t *testing.T) {
	f := newEnterFixture(t, nil)
	sid := f.authSession(t, 11)
	boom := errors.New("socket dead")
	fail := func(uint16, uint16, func(*proto.Encoder) error) error { return boom }
	_, _, err := callHandle(t, f.h, sid, proto.OpcodeEnterWorld, encodeEnter(t, 0), fail)
	var cerr *ClientError
	if err == nil || errors.As(err, &cerr) {
		t.Fatalf("err = %v, want raw internal write error", err)
	}
	if n := f.provider.callCount(); n != 0 {
		t.Errorf("provider calls = %d, want 0 (no baseline after 217 failure)", n)
	}
	snap := mustGet(t, f.reg, sid)
	if snap.State != session.StateAuthenticated || snap.HasCharacter {
		t.Errorf("not rolled back: %+v", snap)
	}
	if _, ok := f.reg.SessionByCharacter(500); ok {
		t.Error("index leaked")
	}
}

func TestEnterProviderOperationalFailure(t *testing.T) {
	f := newEnterFixture(t, nil)
	sid := f.authSession(t, 11)
	f.provider.events = []baselineEvent{
		{kind: proto.OpcodeCellSnapshot, snap: testCellSnapshot()},
		{kind: proto.OpcodeChunkFragment, frag: testChunkFragment(0)},
	}
	f.provider.setErr(errors.New("cells unavailable"))
	_, _, err := callHandle(t, f.h, sid, proto.OpcodeEnterWorld, encodeEnter(t, 0), f.sends.send)
	var cerr *ClientError
	if !errors.As(err, &cerr) || cerr.Code != proto.ErrorCodeRetry {
		t.Fatalf("err = %v, want retry", err)
	}
	// Provisional wire: 217 OK, 203, 218 — and NO 219. The 202 retry
	// itself is emitted by Server from the returned ClientError, so the
	// handler-level frames end at the last baseline event.
	frames := f.sends.all()
	if len(frames) != 3 {
		t.Fatalf("frames = %d, want 3", len(frames))
	}
	if frames[0].opcode != proto.OpcodeCharacterOp ||
		frames[1].opcode != proto.OpcodeCellSnapshot ||
		frames[2].opcode != proto.OpcodeChunkFragment {
		t.Errorf("framing = %d,%d,%d, want 217,203,218",
			frames[0].opcode, frames[1].opcode, frames[2].opcode)
	}
	snap := mustGet(t, f.reg, sid)
	if snap.State != session.StateAuthenticated || snap.HasCharacter {
		t.Errorf("not rolled back: %+v", snap)
	}
	if _, ok := f.reg.SessionByCharacter(500); ok {
		t.Error("index leaked")
	}
	// Same socket retries successfully with a healthy provider.
	f.sends = &sendRecorder{}
	f.provider.setErr(nil)
	_, _, err = callHandle(t, f.h, sid, proto.OpcodeEnterWorld, encodeEnter(t, 0), f.sends.send)
	if err != nil {
		t.Fatalf("retry err = %v", err)
	}
	assertEnterSuccess(t, f, sid, []uint16{
		proto.OpcodeCharacterOp,
		proto.OpcodeCellSnapshot,
		proto.OpcodeChunkFragment,
		proto.OpcodeWorldReady,
	})
}

func TestEnterProviderWriteFailure(t *testing.T) {
	f := newEnterFixture(t, nil)
	sid := f.authSession(t, 11)
	// The provider propagates a sink write failure as-is: the handler
	// must abort and return internal (terminating), never a 202.
	f.provider.events = []baselineEvent{
		{kind: proto.OpcodeCellSnapshot, snap: testCellSnapshot()},
	}
	f.provider.setErr(&baselineWriteError{err: errors.New("socket dead")})
	// The fake returns f.err after events, so the 203 emits first, then
	// the wrapped write error surfaces through propagation.
	_, _, err := callHandle(t, f.h, sid, proto.OpcodeEnterWorld, encodeEnter(t, 0), f.sends.send)
	var cerr *ClientError
	if errors.As(err, &cerr) {
		t.Fatalf("write failure became %v; must stay internal", cerr)
	}
	if err == nil {
		t.Fatal("expected internal write failure")
	}
	snap := mustGet(t, f.reg, sid)
	if snap.State != session.StateAuthenticated || snap.HasCharacter {
		t.Errorf("not rolled back: %+v", snap)
	}
}

func TestEnter219WriteFailure(t *testing.T) {
	f := newEnterFixture(t, nil)
	sid := f.authSession(t, 11)
	f.provider.events = []baselineEvent{
		{kind: proto.OpcodeCellSnapshot, snap: testCellSnapshot()},
	}
	boom := errors.New("socket dead at barrier")
	send := func(op uint16, ver uint16, enc func(*proto.Encoder) error) error {
		if op == proto.OpcodeWorldReady {
			return boom
		}
		return f.sends.send(op, ver, enc)
	}
	_, _, err := callHandle(t, f.h, sid, proto.OpcodeEnterWorld, encodeEnter(t, 0), send)
	if err == nil {
		t.Fatal("expected write failure")
	}
	var cerr *ClientError
	if errors.As(err, &cerr) {
		t.Fatalf("barrier failure became %v", cerr)
	}
	frames := f.sends.all()
	if len(frames) != 2 { // 217 + 203, no 219, no 202
		t.Fatalf("frames = %d, want 2", len(frames))
	}
	snap := mustGet(t, f.reg, sid)
	if snap.State != session.StateAuthenticated || snap.HasCharacter {
		t.Errorf("not rolled back: %+v", snap)
	}
}

func TestSinkWrapsWriteErrors(t *testing.T) {
	boom := errors.New("dead")
	sink := gatewayBaselineSink{send: func(uint16, uint16, func(*proto.Encoder) error) error {
		return boom
	}}
	for name, call := range map[string]func() error{
		"203": func() error { return sink.CellSnapshot(testCellSnapshot()) },
		"218": func() error { return sink.ChunkFragment(testChunkFragment(0)) },
		"220": func() error { return sink.ShopList(testShopList()) },
	} {
		err := call()
		var werr *baselineWriteError
		if !errors.As(err, &werr) {
			t.Errorf("%s err = %v, want *baselineWriteError", name, err)
		}
	}
}

// ---------------------------------------------------------------------------
// blocked baseline, guard serialization, delegation
// ---------------------------------------------------------------------------

func TestEnterBlockedBaselineState(t *testing.T) {
	f := newEnterFixture(t, nil)
	sid := f.authSession(t, 11)

	f.provider.setBlock(make(chan struct{}))
	done := make(chan error, 1)
	go func() {
		_, _, err := callHandle(t, f.h, sid, proto.OpcodeEnterWorld, encodeEnter(t, 0), f.sends.send)
		done <- err
	}()
	<-f.provider.entered
	// While the provider is blocked mid-baseline: provisional state.
	snap := mustGet(t, f.reg, sid)
	if snap.State != session.StateCharacterSelected || !snap.HasCharacter || snap.CharacterID != 500 {
		t.Errorf("mid-baseline = %+v, want SELECTED bound to 500", snap)
	}
	if owner, ok := f.reg.SessionByCharacter(500); !ok || owner != sid {
		t.Errorf("index = %d,%v", owner, ok)
	}
	f.provider.closeBlock()
	if err := <-done; err != nil {
		t.Fatalf("err = %v", err)
	}
	if s := mustGet(t, f.reg, sid); s.State != session.StateInWorld {
		t.Errorf("final = %+v, want IN_WORLD", s)
	}
}

// TestEnterSimultaneousSameAccountTakeover: two same-account enters
// serialize on the guard — deterministically forced by a blocked
// baseline — and the loser takes over the completed winner instead of
// receiving a retry.
func TestEnterSimultaneousSameAccountTakeover(t *testing.T) {
	reg := session.NewRegistry()
	f := newEnterFixture(t, reg)
	a := f.authSession(t, 21)
	b := f.authSession(t, 21)

	// A acquires the guard first and blocks mid-baseline.
	f.provider.setBlock(make(chan struct{}))
	aDone := f.enterAsync(t, a, f.sends.send)
	<-f.provider.entered // A holds the account guard mid-baseline.

	// B starts its enter and parks on the account guard.
	bStarted := make(chan struct{})
	bDone := make(chan error, 1)
	go func() {
		close(bStarted)
		_, _, err := callHandle(t, f.h, b, proto.OpcodeEnterWorld, encodeEnter(t, 0), f.sends.send)
		bDone <- err
	}()
	<-bStarted
	for i := 0; i < 1000; i++ {
		runtime.Gosched()
	}
	// B has not reached lookup or provider while A holds the guard.
	if n := f.lookup.callCount(); n != 1 {
		t.Fatalf("B lookup calls = %d while A mid-baseline, want 1 (A only)", n)
	}
	if n := f.provider.callCount(); n != 1 {
		t.Fatalf("B provider calls = %d while A mid-baseline, want 1 (A only)", n)
	}

	// Release A: it completes to IN_WORLD; B acquires the guard, sees A
	// as the old world session, and takes over.
	f.provider.closeBlock()
	if err := <-aDone; err != nil {
		t.Fatalf("A err = %v", err)
	}
	if err := <-bDone; err != nil {
		t.Fatalf("B takeover err = %v", err)
	}

	// A is retired and kicked; B is the only world-active session.
	if _, ok := reg.Get(a); ok {
		t.Fatal("completed winner A still registered after takeover")
	}
	ac := f.conns[a]
	frames := ac.recorded()
	if len(frames) != 1 {
		t.Fatalf("A frames = %d, want exactly the kicked 202", len(frames))
	}
	if _, msg := decodeKickedFrame(t, frames[0]); msg.Code != proto.ErrorCodeKicked {
		t.Fatalf("A final frame is not kicked")
	}
	if n := ac.closeNowCount(); n != 1 {
		t.Errorf("A CloseNow = %d, want 1", n)
	}
	if s := mustGet(t, reg, b); s.State != session.StateInWorld || !s.HasCharacter || s.CharacterID != 500 {
		t.Errorf("B = %+v, want IN_WORLD bound to 500", s)
	}
	if owner, ok := reg.SessionByCharacter(500); !ok || owner != b {
		t.Errorf("character index = %d,%v, want B", owner, ok)
	}
	// No overlapping baselines: exactly two provider runs (A then B),
	// and the old world was flushed exactly once.
	if n := f.provider.callCount(); n != 2 {
		t.Errorf("provider calls = %d, want 2", n)
	}
	if n := f.exit.callCount(); n != 1 {
		t.Errorf("WorldExit calls = %d, want 1 (A flushed once)", n)
	}
	if n := countWorldActive(t, reg, "sub"); n != 1 {
		t.Errorf("world-active sessions = %d, want 1", n)
	}
	if n := reg.GuardCount(); n != 0 {
		t.Errorf("guards = %d, want 0 (both released)", n)
	}
}

func TestEnterDifferentAccountsConcurrent(t *testing.T) {
	reg := session.NewRegistry()
	fa := newEnterFixture(t, reg)
	fb := newEnterFixture(t, reg)
	// Distinct characters per account: the point is guard independence,
	// not character contention.
	fb.lookup.mu.Lock()
	fb.lookup.desc = character.Descriptor{ID: 501, Slot: 0, Name: "Bram", Revision: 1}
	fb.lookup.mu.Unlock()
	a := fa.authSession(t, 31)
	b := fb.authSession(t, 32)

	fa.provider.setBlock(make(chan struct{}))

	aDone := make(chan error, 1)
	go func() {
		_, _, err := callHandle(t, fa.h, a, proto.OpcodeEnterWorld, encodeEnter(t, 0), fa.sends.send)
		aDone <- err
	}()
	<-fa.provider.entered // A blocked; its guard must not stop B.
	_, _, err := callHandle(t, fb.h, b, proto.OpcodeEnterWorld, encodeEnter(t, 0), fb.sends.send)
	if err != nil {
		t.Fatalf("B err = %v while A blocked", err)
	}
	if s := mustGet(t, reg, b); s.State != session.StateInWorld || !s.HasCharacter {
		t.Errorf("B = %+v, want IN_WORLD bound", s)
	}
	fa.provider.closeBlock()
	if err := <-aDone; err != nil {
		t.Fatalf("A err = %v", err)
	}
	if s := mustGet(t, reg, a); s.State != session.StateInWorld {
		t.Errorf("A = %+v", s)
	}
}

func TestEnterDelegatesOthers(t *testing.T) {
	for _, op := range []uint16{121, 125, 102} {
		op := op
		t.Run(fmt.Sprintf("opcode-%d", op), func(t *testing.T) {
			f := newEnterFixture(t, nil)
			sid := f.authSession(t, 11)
			frame, err := proto.EncodeFrame(proto.Header{Opcode: op, MsgVersion: 1, Seq: 9, Tick: 10},
				func(e *proto.Encoder) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			header, dec, herr := callHandle(t, f.h, sid, op, frame, f.sends.send)
			if herr != nil {
				t.Fatalf("err = %v", herr)
			}
			f.next.mu.Lock()
			defer f.next.mu.Unlock()
			if f.next.calls != 1 || f.next.sid != sid || f.next.header != header || f.next.dec != dec {
				t.Errorf("next calls=%d (want passthrough)", f.next.calls)
			}
			if n := f.provider.callCount(); n != 0 {
				t.Errorf("provider calls = %d", n)
			}
			if n := f.lookup.callCount(); n != 0 {
				t.Errorf("lookup calls = %d", n)
			}
		})
	}
}
