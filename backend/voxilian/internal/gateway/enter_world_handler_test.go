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

type enterFixture struct {
	reg      *session.Registry
	lookup   *fakeLookup
	provider *fakeProvider
	sends    *sendRecorder
	next     *recordingNext
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
	h, err := NewEnterWorldHandler(lookup, reg, BaselineProviderFunc(provider.StreamBaseline), next)
	if err != nil {
		t.Fatalf("NewEnterWorldHandler: %v", err)
	}
	return &enterFixture{reg: reg, lookup: lookup, provider: provider, sends: sends, next: next, h: h}
}

func (f *enterFixture) authSession(t *testing.T, accountID int64) session.ID {
	t.Helper()
	id := f.reg.Create(nil)
	if err := f.reg.Authenticate(id, "sub", accountID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	return id
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
	reg := session.NewRegistry()
	lookup := &fakeLookup{}
	provider := BaselineProviderFunc(func(context.Context, session.ID, int64, int64, BaselineSink) error {
		return nil
	})
	if _, err := NewEnterWorldHandler(nil, reg, provider, nil); err == nil {
		t.Error("nil lookup accepted")
	}
	if _, err := NewEnterWorldHandler(lookup, nil, provider, nil); err == nil {
		t.Error("nil registry accepted")
	}
	if _, err := NewEnterWorldHandler(lookup, reg, nil, nil); err == nil {
		t.Error("nil provider accepted (no silent no-op default)")
	}
	if _, err := NewEnterWorldHandler(lookup, reg, provider, nil); err != nil {
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
// staged duplicate + arbitration
// ---------------------------------------------------------------------------

func TestEnterStagedDuplicateRetry(t *testing.T) {
	for _, state := range []session.State{session.StateCharacterSelected, session.StateInWorld} {
		t.Run(state.String(), func(t *testing.T) {
			f := newEnterFixture(t, nil)
			sid := f.authSession(t, 11)
			busy := f.authSession(t, 11)
			if err := f.reg.BindCharacter(busy, 600); err != nil {
				t.Fatal(err)
			}
			cur, _ := f.reg.Get(busy)
			if cur.State != state {
				if err := f.reg.CompareAndSetState(busy, cur.State, state); err != nil {
					t.Fatal(err)
				}
				if state == session.StateInWorld {
					cur, _ = f.reg.Get(busy)
					if err := f.reg.CompareAndSetState(busy, cur.State, state); err != nil {
						t.Fatal(err)
					}
				}
			}
			_, _, err := callHandle(t, f.h, sid, proto.OpcodeEnterWorld,
				encodeEnter(t, 0), f.sends.send)
			var cerr *ClientError
			if !errors.As(err, &cerr) || cerr.Code != proto.ErrorCodeRetry {
				t.Fatalf("err = %v, want staged retry", err)
			}
			// Zero mutation on both sessions; nothing emitted, no kick,
			// no flush, no baseline.
			if s := mustGet(t, f.reg, sid); s.State != session.StateAuthenticated || s.HasCharacter {
				t.Errorf("self mutated: %+v", s)
			}
			if b := mustGet(t, f.reg, busy); b.State != state || !b.HasCharacter || b.CharacterID != 600 {
				t.Errorf("other mutated: %+v", b)
			}
			if frames := f.sends.all(); len(frames) != 0 {
				t.Errorf("frames = %d, want none", len(frames))
			}
			if n := f.provider.callCount(); n != 0 {
				t.Errorf("provider calls = %d, want 0", n)
			}
		})
	}
}

func mustGet(t *testing.T, r *session.Registry, id session.ID) session.Snapshot {
	t.Helper()
	snap, ok := r.Get(id)
	if !ok {
		t.Fatalf("session %d vanished", uint64(id))
	}
	return snap
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

func TestEnterGuardSerialization(t *testing.T) {
	reg := session.NewRegistry()
	fa := newEnterFixture(t, reg)
	fb := newEnterFixture(t, reg)
	a := fa.authSession(t, 21)
	b := fb.authSession(t, 21)

	fa.provider.setBlock(make(chan struct{}))

	aDone := make(chan error, 1)
	go func() {
		_, _, err := callHandle(t, fa.h, a, proto.OpcodeEnterWorld, encodeEnter(t, 0), fa.sends.send)
		aDone <- err
	}()
	<-fa.provider.entered // A holds the account guard mid-baseline.

	// B must not reach lookup, arbitration, begin, or provider while A
	// holds the guard. Force-schedule B, then check.
	bStarted := make(chan struct{})
	bDone := make(chan error, 1)
	go func() {
		close(bStarted)
		_, _, err := callHandle(t, fb.h, b, proto.OpcodeEnterWorld, encodeEnter(t, 0), fb.sends.send)
		bDone <- err
	}()
	<-bStarted
	for i := 0; i < 1000; i++ {
		runtime.Gosched()
	}
	if n := fb.lookup.callCount(); n != 0 {
		t.Fatalf("B lookup calls = %d while A mid-baseline", n)
	}
	if n := fb.provider.callCount(); n != 0 {
		t.Fatalf("B provider calls = %d while A mid-baseline", n)
	}
	fa.provider.closeBlock()
	if err := <-aDone; err != nil {
		t.Fatalf("A err = %v", err)
	}
	if err := <-bDone; err == nil {
		t.Fatal("B must not silently succeed while A is IN_WORLD")
	} else {
		var cerr *ClientError
		if !errors.As(err, &cerr) || cerr.Code != proto.ErrorCodeRetry {
			t.Fatalf("B err = %v, want staged retry", err)
		}
	}
	// A untouched IN_WORLD/bound; B untouched AUTHENTICATED/unbound.
	if s := mustGet(t, reg, a); s.State != session.StateInWorld || !s.HasCharacter {
		t.Errorf("A = %+v", s)
	}
	if s := mustGet(t, reg, b); s.State != session.StateAuthenticated || s.HasCharacter {
		t.Errorf("B = %+v", s)
	}
	if frames := fb.sends.all(); len(frames) != 0 {
		t.Errorf("B frames = %d, want none (retry is a ClientError)", len(frames))
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
