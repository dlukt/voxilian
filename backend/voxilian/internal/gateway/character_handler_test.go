package gateway

import (
	"context"
	"errors"
	"fmt"
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

type fakeCharacters struct {
	mu sync.Mutex

	listEntries  []character.ListEntry
	listErr      error
	listAccounts []int64

	createID       int64
	createErr      error
	createReqs     []character.CreateRequest
	createAccounts []int64

	desc      character.Descriptor
	descErr   error
	findSlots []uint8

	deleteRev   int64
	deleteErr   error
	deleteCalls [][2]int64
}

func (f *fakeCharacters) List(_ context.Context, accountID int64) ([]character.ListEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listAccounts = append(f.listAccounts, accountID)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listEntries, nil
}

func (f *fakeCharacters) Create(_ context.Context, accountID int64, req character.CreateRequest) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createAccounts = append(f.createAccounts, accountID)
	f.createReqs = append(f.createReqs, req)
	if f.createErr != nil {
		return 0, f.createErr
	}
	return f.createID, nil
}

func (f *fakeCharacters) FindBySlot(_ context.Context, _ int64, slot uint8) (character.Descriptor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.findSlots = append(f.findSlots, slot)
	if f.descErr != nil {
		return character.Descriptor{}, f.descErr
	}
	return f.desc, nil
}

func (f *fakeCharacters) Delete(_ context.Context, id, rev int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls = append(f.deleteCalls, [2]int64{id, rev})
	if f.deleteErr != nil {
		return 0, f.deleteErr
	}
	return f.deleteRev, nil
}

func (f *fakeCharacters) counts() (finds, deletes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.findSlots), len(f.deleteCalls)
}

type sentFrame struct {
	opcode  uint16
	version uint16
	payload []byte
}

type sendRecorder struct {
	mu     sync.Mutex
	frames []sentFrame
}

func (s *sendRecorder) send(opcode uint16, version uint16, encode func(*proto.Encoder) error) error {
	e := proto.NewEncoder()
	if err := encode(e); err != nil {
		return err
	}
	b, err := e.Bytes()
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, sentFrame{opcode: opcode, version: version, payload: b})
	return nil
}

func (s *sendRecorder) all() []sentFrame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sentFrame(nil), s.frames...)
}

type recordingNext struct {
	mu     sync.Mutex
	calls  int
	sid    session.ID
	header proto.Header
	dec    *proto.Decoder
}

func (n *recordingNext) Handle(
	_ context.Context,
	sid session.ID,
	header proto.Header,
	payload *proto.Decoder,
	send SendFunc,
) error {
	n.mu.Lock()
	n.calls++
	n.sid = sid
	n.header = header
	n.dec = payload
	n.mu.Unlock()
	return send(proto.OpcodeWorldReady, proto.MessageVersion1, func(e *proto.Encoder) error {
		proto.WorldReady{}.Encode(e)
		return nil
	})
}

type recordingExit struct {
	mu       sync.Mutex
	calls    int
	block    chan struct{} // when non-nil, ExitWorld waits for close
	entered  chan struct{}
	err      error
	observed []session.Snapshot // registry state seen at entry
	registry *session.Registry
}

func (w *recordingExit) ExitWorld(
	_ context.Context,
	sid session.ID,
	accountID int64,
	characterID int64,
) error {
	w.mu.Lock()
	if w.entered != nil {
		close(w.entered)
		w.entered = nil
	}
	if w.registry != nil {
		if snap, ok := w.registry.GetByCharacter(characterID); ok {
			w.observed = append(w.observed, snap)
		}
	}
	_ = sid
	_ = accountID
	block := w.block
	err := w.err
	w.calls++
	w.mu.Unlock()
	if block != nil {
		<-block
	}
	return err
}

func (w *recordingExit) setErr(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.err = err
}

// setEntered installs the entry barrier channel (test side).
func (w *recordingExit) setEntered(ch chan struct{}) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.entered = ch
}

// enteredCh returns the current entry barrier for waiting.
func (w *recordingExit) enteredCh() chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.entered
}

// setBlock installs the flush gate channel (test side).
func (w *recordingExit) setBlock(ch chan struct{}) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.block = ch
}

// closeBlock releases a blocked flush exactly once.
func (w *recordingExit) closeBlock() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.block != nil {
		close(w.block)
		w.block = nil
	}
}

func (w *recordingExit) callCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

// ---------------------------------------------------------------------------
// setup
// ---------------------------------------------------------------------------

type handlerFixture struct {
	reg   *session.Registry
	chars *fakeCharacters
	sends *sendRecorder
	exit  *recordingExit
	next  *recordingNext
	h     *CharacterHandler
}

func newHandlerFixture(t *testing.T) *handlerFixture {
	t.Helper()
	reg := session.NewRegistry()
	chars := &fakeCharacters{}
	sends := &sendRecorder{}
	exit := &recordingExit{registry: reg}
	next := &recordingNext{}
	h, err := NewCharacterHandler(chars, reg, WorldExitFunc(exit.ExitWorld), next)
	if err != nil {
		t.Fatalf("NewCharacterHandler: %v", err)
	}
	return &handlerFixture{reg: reg, chars: chars, sends: sends, exit: exit, next: next, h: h}
}

// authSession creates an AUTHENTICATED session; when bind != 0 it also
// binds the character and advances to want (CHARACTER_SELECTED or
// IN_WORLD).
func (f *handlerFixture) authSession(t *testing.T, accountID, bind int64, want session.State) session.ID {
	t.Helper()
	id := f.reg.Create(nil)
	if err := f.reg.Authenticate(id, "sub", accountID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if bind != 0 {
		if err := f.reg.BindCharacter(id, bind); err != nil {
			t.Fatalf("bind: %v", err)
		}
		for _, next := range []session.State{session.StateCharacterSelected, session.StateInWorld} {
			cur, _ := f.reg.Get(id)
			if cur.State == want {
				break
			}
			if err := f.reg.CompareAndSetState(id, cur.State, next); err != nil {
				t.Fatalf("cas: %v", err)
			}
		}
	}
	snap, _ := f.reg.Get(id)
	if snap.State != want {
		t.Fatalf("state = %s, want %s", snap.State, want)
	}
	return id
}

func encodeCreate(t *testing.T, slot uint8, name string) []byte {
	t.Helper()
	frame, err := proto.EncodeFrame(
		proto.Header{Opcode: proto.OpcodeCharacterCreate, MsgVersion: 1},
		func(e *proto.Encoder) error {
			proto.CharacterCreate{Slot: slot, Name: name}.Encode(e)
			return nil
		})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return frame
}

func callHandle(
	t *testing.T,
	h *CharacterHandler,
	sid session.ID,
	opcode uint16,
	payload []byte,
	send SendFunc,
) (proto.Header, *proto.Decoder, error) {
	t.Helper()
	header, dec, err := proto.DecodeFrame(payload)
	if err != nil {
		t.Fatalf("decode test frame: %v", err)
	}
	if header.Opcode != opcode {
		t.Fatalf("test frame opcode = %d, want %d", header.Opcode, opcode)
	}
	err = h.Handle(context.Background(), sid, header, dec, send)
	return header, dec, err
}

func decodeOp(t *testing.T, f sentFrame) proto.CharacterOp {
	t.Helper()
	if f.opcode != proto.OpcodeCharacterOp {
		t.Fatalf("opcode = %d, want 217", f.opcode)
	}
	if f.version != proto.MessageVersion1 {
		t.Fatalf("msg_version = %d, want 1", f.version)
	}
	op, err := proto.DecodeCharacterOp(proto.NewDecoder(f.payload))
	if err != nil {
		t.Fatalf("decode 217: %v", err)
	}
	return op
}

// ---------------------------------------------------------------------------
// constructor
// ---------------------------------------------------------------------------

func TestNewCharacterHandlerRequiresDeps(t *testing.T) {
	reg := session.NewRegistry()
	chars := &fakeCharacters{}
	exit := WorldExitFunc(func(context.Context, session.ID, int64, int64) error { return nil })
	if _, err := NewCharacterHandler(nil, reg, exit, nil); err == nil {
		t.Error("nil characters accepted")
	}
	if _, err := NewCharacterHandler(chars, nil, exit, nil); err == nil {
		t.Error("nil registry accepted")
	}
	if _, err := NewCharacterHandler(chars, reg, nil, nil); err == nil {
		t.Error("nil WorldExit accepted (must never silently succeed)")
	}
	if _, err := NewCharacterHandler(chars, reg, exit, nil); err != nil {
		t.Errorf("nil Next rejected: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 121 list
// ---------------------------------------------------------------------------

func TestListSuccess(t *testing.T) {
	f := newHandlerFixture(t)
	sid := f.authSession(t, 42, 0, session.StateAuthenticated)
	f.chars.listEntries = []character.ListEntry{
		{Slot: 0, Name: "Aria", Level: 20},
		{Slot: 1, Name: "Bram", Level: 35},
	}
	empty, err := proto.EncodeFrame(proto.Header{Opcode: proto.OpcodeCharacterList, MsgVersion: 1},
		func(e *proto.Encoder) error {
			proto.CharacterListRequest{}.Encode(e)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := callHandle(t, f.h, sid, proto.OpcodeCharacterList, empty, f.sends.send); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	frames := f.sends.all()
	if len(frames) != 1 || frames[0].opcode != proto.OpcodeCharacterListResult {
		t.Fatalf("frames = %+v, want one 216", frames)
	}
	if frames[0].version != proto.MessageVersion1 {
		t.Errorf("msg_version = %d, want 1", frames[0].version)
	}
	list, err := proto.DecodeCharacterList(proto.NewDecoder(frames[0].payload))
	if err != nil {
		t.Fatalf("decode 216: %v", err)
	}
	if len(list.Characters) != 2 ||
		list.Characters[0].Slot != 0 || list.Characters[0].CharName != "Aria" || list.Characters[0].Level != 20 ||
		list.Characters[1].Slot != 1 || list.Characters[1].CharName != "Bram" || list.Characters[1].Level != 35 {
		t.Errorf("216 = %+v, want ordered service entries, no DB IDs", list.Characters)
	}
	f.chars.mu.Lock()
	defer f.chars.mu.Unlock()
	if len(f.chars.listAccounts) != 1 || f.chars.listAccounts[0] != 42 {
		t.Errorf("service accounts = %v, want [42] from snapshot", f.chars.listAccounts)
	}
}

func TestListFailureMapping(t *testing.T) {
	f := newHandlerFixture(t)
	sid := f.authSession(t, 42, 0, session.StateAuthenticated)
	empty, _ := proto.EncodeFrame(proto.Header{Opcode: proto.OpcodeCharacterList, MsgVersion: 1},
		func(e *proto.Encoder) error {
			proto.CharacterListRequest{}.Encode(e)
			return nil
		})

	f.chars.listErr = character.ErrPersistence
	_, _, err := callHandle(t, f.h, sid, proto.OpcodeCharacterList, empty, f.sends.send)
	var cerr *ClientError
	if !errors.As(err, &cerr) || cerr.Code != proto.ErrorCodeRetry {
		t.Errorf("err = %v, want 202 retry ClientError", err)
	}

	f.chars.listErr = character.ErrInvalidContent
	_, _, err = callHandle(t, f.h, sid, proto.OpcodeCharacterList, empty, f.sends.send)
	if errors.As(err, &cerr) {
		t.Errorf("corrupt state became %v; must stay internal", cerr)
	}
	if err == nil {
		t.Error("corrupt state must be an internal failure")
	}
}

// ---------------------------------------------------------------------------
// 122 create
// ---------------------------------------------------------------------------

func TestCreateMapping(t *testing.T) {
	cases := []struct {
		name    string
		svcErr  error
		wantOp  *proto.CharacterOp // nil → expect ClientError instead
		wantErr uint16             // expected 202 code when wantOp == nil
	}{
		{"invalid slot", character.ErrInvalidSlot, &proto.CharacterOp{Op: proto.CharacterOpCreate, OK: proto.CharacterOpRejected}, 0},
		{"invalid name", character.ErrInvalidName, &proto.CharacterOp{Op: proto.CharacterOpCreate, OK: proto.CharacterOpRejected}, 0},
		{"name taken", character.ErrNameUnavailable, nil, proto.ErrorCodeNameTaken},
		{"slot occupied", character.ErrSlotOccupied, nil, proto.ErrorCodeSlotOccupied},
		{"bad stats", character.ErrBadStats, nil, proto.ErrorCodeBadStats},
		{"bad budget", character.ErrBadBudget, nil, proto.ErrorCodeBadBudget},
		{"persistence", character.ErrPersistence, nil, proto.ErrorCodeRetry},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newHandlerFixture(t)
			sid := f.authSession(t, 42, 0, session.StateAuthenticated)
			f.chars.createID = 99
			f.chars.createErr = tc.svcErr
			_, _, err := callHandle(t, f.h, sid, proto.OpcodeCharacterCreate,
				encodeCreate(t, 0, "Aria"), f.sends.send)
			if tc.wantOp != nil {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				frames := f.sends.all()
				if len(frames) != 1 {
					t.Fatalf("frames = %d, want 1", len(frames))
				}
				if got := decodeOp(t, frames[0]); got != *tc.wantOp {
					t.Errorf("217 = %+v, want %+v", got, *tc.wantOp)
				}
				return
			}
			var cerr *ClientError
			if !errors.As(err, &cerr) || cerr.Code != tc.wantErr {
				t.Errorf("err = %v, want 202 code %d", err, tc.wantErr)
			}
		})
	}
}

func TestCreateSuccessAndInternal(t *testing.T) {
	f := newHandlerFixture(t)
	sid := f.authSession(t, 42, 0, session.StateAuthenticated)
	f.chars.createID = 99
	_, _, err := callHandle(t, f.h, sid, proto.OpcodeCharacterCreate,
		encodeCreate(t, 1, "Bram"), f.sends.send)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	frames := f.sends.all()
	if len(frames) != 1 {
		t.Fatalf("frames = %d", len(frames))
	}
	if got := decodeOp(t, frames[0]); got.Op != proto.CharacterOpCreate || got.OK != proto.CharacterOpOK {
		t.Errorf("217 = %+v, want create/ok", got)
	}
	f.chars.mu.Lock()
	gotReq := f.chars.createReqs[0]
	gotAcct := f.chars.createAccounts[0]
	f.chars.mu.Unlock()
	if gotAcct != 42 || gotReq.Slot != 1 || gotReq.Name != "Bram" {
		t.Errorf("domain request = acct %d %+v", gotAcct, gotReq)
	}
	// No state transition, no bind: still plain AUTHENTICATED.
	snap, _ := f.reg.Get(sid)
	if snap.State != session.StateAuthenticated || snap.HasCharacter {
		t.Errorf("session changed by create: %+v", snap)
	}

	// Corrupt trusted content stays internal (connection-ending, not 202).
	f.chars.createErr = character.ErrInvalidContent
	_, _, err = callHandle(t, f.h, sid, proto.OpcodeCharacterCreate,
		encodeCreate(t, 0, "Aria"), f.sends.send)
	var cerr *ClientError
	if errors.As(err, &cerr) {
		t.Errorf("internal became %v", cerr)
	}
	if err == nil {
		t.Error("expected internal failure")
	}
}

func TestCreateMalformed(t *testing.T) {
	f := newHandlerFixture(t)
	sid := f.authSession(t, 42, 0, session.StateAuthenticated)
	short, err := proto.EncodeFrame(proto.Header{Opcode: proto.OpcodeCharacterCreate, MsgVersion: 1},
		func(e *proto.Encoder) error {
			e.U8(0) // truncated create payload
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	_, _, herr := callHandle(t, f.h, sid, proto.OpcodeCharacterCreate, short, f.sends.send)
	var cerr *ClientError
	if !errors.As(herr, &cerr) || cerr.Code != proto.ErrorCodeProtocol {
		t.Errorf("err = %v, want protocol_error", herr)
	}
	finds, _ := f.chars.counts()
	_ = finds
	f.chars.mu.Lock()
	defer f.chars.mu.Unlock()
	if len(f.chars.createReqs) != 0 {
		t.Error("malformed create reached the service")
	}
}

// ---------------------------------------------------------------------------
// 123 delete
// ---------------------------------------------------------------------------

func encodeDelete(t *testing.T, slot uint8) []byte {
	t.Helper()
	frame, err := proto.EncodeFrame(proto.Header{Opcode: proto.OpcodeCharacterDelete, MsgVersion: 1},
		func(e *proto.Encoder) error {
			proto.CharacterDelete{Slot: slot}.Encode(e)
			return nil
		})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return frame
}

func TestDeleteInvalidOrEmptySlot(t *testing.T) {
	for _, tc := range []struct {
		name string
		slot uint8
		err  error
	}{
		{"invalid slot", 7, character.ErrInvalidSlot},
		{"empty slot", 0, character.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHandlerFixture(t)
			sid := f.authSession(t, 42, 0, session.StateAuthenticated)
			f.chars.descErr = tc.err
			_, _, err := callHandle(t, f.h, sid, proto.OpcodeCharacterDelete,
				encodeDelete(t, tc.slot), f.sends.send)
			if err != nil {
				t.Fatalf("err = %v, want 217 rejected reply", err)
			}
			frames := f.sends.all()
			if len(frames) != 1 {
				t.Fatalf("frames = %d", len(frames))
			}
			if got := decodeOp(t, frames[0]); got.Op != proto.CharacterOpDelete || got.OK != proto.CharacterOpRejected {
				t.Errorf("217 = %+v, want delete/rejected", got)
			}
		})
	}
}

func TestDeleteInUse(t *testing.T) {
	for _, state := range []session.State{session.StateCharacterSelected, session.StateInWorld} {
		t.Run(state.String(), func(t *testing.T) {
			f := newHandlerFixture(t)
			owner := f.authSession(t, 42, 900, state)
			_ = owner
			deleter := f.authSession(t, 42, 0, session.StateAuthenticated)
			f.chars.desc = character.Descriptor{ID: 900, Slot: 0, Name: "Aria", Revision: 3}
			_, _, err := callHandle(t, f.h, deleter, proto.OpcodeCharacterDelete,
				encodeDelete(t, 0), f.sends.send)
			var cerr *ClientError
			if !errors.As(err, &cerr) || cerr.Code != proto.ErrorCodeCharacterInUse {
				t.Fatalf("err = %v, want character_in_use", err)
			}
			if _, deletes := f.chars.counts(); deletes != 0 {
				t.Errorf("Delete called %d times while in use; want 0", deletes)
			}
		})
	}
}

func TestDeleteImpossibleBindingFailsClosed(t *testing.T) {
	f := newHandlerFixture(t)
	// Character bound while the owning session stays AUTHENTICATED
	// (authSession only advances past AUTHENTICATED when asked): an
	// impossible registry state that must fail closed, never delete.
	owner := f.authSession(t, 42, 901, session.StateAuthenticated)
	_ = owner
	deleter := f.authSession(t, 42, 0, session.StateAuthenticated)
	f.chars.desc = character.Descriptor{ID: 901, Slot: 0, Name: "Aria", Revision: 3}
	_, _, err := callHandle(t, f.h, deleter, proto.OpcodeCharacterDelete,
		encodeDelete(t, 0), f.sends.send)
	var cerr *ClientError
	if errors.As(err, &cerr) {
		t.Errorf("invariant failure became %v; must stay internal", cerr)
	}
	if err == nil {
		t.Error("expected internal failure")
	}
	if _, deletes := f.chars.counts(); deletes != 0 {
		t.Errorf("Delete called %d times on invariant failure", deletes)
	}
}

func TestDeleteSuccessAndRetry(t *testing.T) {
	f := newHandlerFixture(t)
	sid := f.authSession(t, 42, 0, session.StateAuthenticated)
	f.chars.desc = character.Descriptor{ID: 902, Slot: 1, Name: "Bram", Revision: 4}
	f.chars.deleteRev = 5
	_, _, err := callHandle(t, f.h, sid, proto.OpcodeCharacterDelete,
		encodeDelete(t, 1), f.sends.send)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	frames := f.sends.all()
	if len(frames) != 1 {
		t.Fatalf("frames = %d", len(frames))
	}
	if got := decodeOp(t, frames[0]); got.Op != proto.CharacterOpDelete || got.OK != proto.CharacterOpOK {
		t.Errorf("217 = %+v, want delete/ok", got)
	}
	f.chars.mu.Lock()
	calls := append([][2]int64(nil), f.chars.deleteCalls...)
	f.chars.mu.Unlock()
	if len(calls) != 1 || calls[0] != [2]int64{902, 4} {
		t.Errorf("delete calls = %v, want [{902 4}]", calls)
	}

	f.chars.deleteErr = character.ErrPersistence
	_, _, err = callHandle(t, f.h, sid, proto.OpcodeCharacterDelete,
		encodeDelete(t, 1), f.sends.send)
	var cerr *ClientError
	if !errors.As(err, &cerr) || cerr.Code != proto.ErrorCodeRetry {
		t.Errorf("err = %v, want retry", err)
	}
}

func TestDeleteMalformed(t *testing.T) {
	f := newHandlerFixture(t)
	sid := f.authSession(t, 42, 0, session.StateAuthenticated)
	full, err := proto.EncodeFrame(proto.Header{Opcode: proto.OpcodeCharacterDelete, MsgVersion: 1},
		func(e *proto.Encoder) error {
			proto.CharacterDelete{Slot: 0}.Encode(e)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	// CharacterDelete payload is a single slot byte; a truncated frame
	// body is malformed (outer framing stays valid).
	truncated := append([]byte{}, full[:len(full)-1]...)
	_, _, herr := callHandle(t, f.h, sid, proto.OpcodeCharacterDelete, truncated, f.sends.send)
	var cerr *ClientError
	if !errors.As(herr, &cerr) || cerr.Code != proto.ErrorCodeProtocol {
		t.Errorf("err = %v, want protocol_error", herr)
	}
}

func TestDeleteUsesAccountGuard(t *testing.T) {
	f := newHandlerFixture(t)
	sid := f.authSession(t, 42, 0, session.StateAuthenticated)
	f.chars.desc = character.Descriptor{ID: 903, Slot: 0, Name: "Aria", Revision: 1}

	// The test holds the guard first (conclusive hold). The handler can
	// only reach FindBySlot by acquiring it.
	unlock := f.reg.LockAccount(42)
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		close(started)
		_, _, _ = callHandle(t, f.h, sid, proto.OpcodeCharacterDelete,
			encodeDelete(t, 0), f.sends.send)
	}()
	<-started
	// Yield until the worker has run as far as it can: blocked on the
	// guard when correct, already at FindBySlot when the guard is
	// bypassed. No timing sleeps; Gosched only forces scheduling.
	for i := 0; i < 1000; i++ {
		runtime.Gosched()
	}
	if finds, _ := f.chars.counts(); finds != 0 {
		unlock()
		t.Fatalf("FindBySlot reached %d times while guard held", finds)
	}
	unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("delete did not complete after guard release")
	}
	if finds, _ := f.chars.counts(); finds != 1 {
		t.Errorf("FindBySlot reached %d times, want 1 after release", finds)
	}
	frames := f.sends.all()
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want the delete 217", len(frames))
	}
	if got := decodeOp(t, frames[0]); got.Op != proto.CharacterOpDelete || got.OK != proto.CharacterOpOK {
		t.Errorf("217 = %+v, want delete/ok", got)
	}
}

// ---------------------------------------------------------------------------
// 126 leave
// ---------------------------------------------------------------------------

func encodeLeave(t *testing.T) []byte {
	t.Helper()
	frame, err := proto.EncodeFrame(proto.Header{Opcode: proto.OpcodeLeaveWorld, MsgVersion: 1},
		func(e *proto.Encoder) error {
			proto.LeaveWorld{}.Encode(e)
			return nil
		})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return frame
}

func TestLeaveSuccess(t *testing.T) {
	f := newHandlerFixture(t)
	f.exit.setEntered(make(chan struct{}))
	sid := f.authSession(t, 42, 910, session.StateInWorld)
	_, _, err := callHandle(t, f.h, sid, proto.OpcodeLeaveWorld, encodeLeave(t), f.sends.send)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	// WorldExit observed the pre-completion registry state.
	f.exit.mu.Lock()
	observed := append([]session.Snapshot(nil), f.exit.observed...)
	f.exit.mu.Unlock()
	if len(observed) != 1 {
		t.Fatalf("exit observations = %d, want 1", len(observed))
	}
	ob := observed[0]
	if ob.State != session.StateInWorld || !ob.HasCharacter || ob.CharacterID != 910 {
		t.Errorf("exit observed %+v, want IN_WORLD bound to 910", ob)
	}
	// Silent success: no frame sent.
	if frames := f.sends.all(); len(frames) != 0 {
		t.Errorf("frames = %+v on leave success; want none", frames)
	}
	// Registry completed atomically.
	snap, _ := f.reg.Get(sid)
	if snap.State != session.StateAuthenticated || snap.HasCharacter || snap.CharacterID != 0 {
		t.Errorf("after leave: %+v", snap)
	}
	if _, ok := f.reg.SessionByCharacter(910); ok {
		t.Error("character index still present")
	}
	// Identity (incl. deadline input) untouched.
	if snap.Sub != "sub" || snap.AccountID != 42 || !snap.Authenticated || snap.TokenExp.IsZero() {
		t.Errorf("identity changed: %+v", snap)
	}
}

func TestLeaveFailureKeepsBinding(t *testing.T) {
	f := newHandlerFixture(t)
	sid := f.authSession(t, 42, 911, session.StateInWorld)
	f.exit.setErr(errors.New("flush failed"))
	_, _, err := callHandle(t, f.h, sid, proto.OpcodeLeaveWorld, encodeLeave(t), f.sends.send)
	var cerr *ClientError
	if !errors.As(err, &cerr) || cerr.Code != proto.ErrorCodeRetry {
		t.Fatalf("err = %v, want retry", err)
	}
	snap, _ := f.reg.Get(sid)
	if snap.State != session.StateInWorld || !snap.HasCharacter || snap.CharacterID != 911 {
		t.Errorf("partial leave: %+v", snap)
	}
	if owner, ok := f.reg.SessionByCharacter(911); !ok || owner != sid {
		t.Errorf("index = %d,%v, want owner", owner, ok)
	}
	// Retry with a healthy seam succeeds.
	f.exit.setErr(nil)
	_, _, err = callHandle(t, f.h, sid, proto.OpcodeLeaveWorld, encodeLeave(t), f.sends.send)
	if err != nil {
		t.Fatalf("retry err = %v", err)
	}
	snap, _ = f.reg.Get(sid)
	if snap.State != session.StateAuthenticated || snap.HasCharacter {
		t.Errorf("after retry: %+v", snap)
	}
}

func TestLeaveGuarded(t *testing.T) {
	f := newHandlerFixture(t)
	f.exit.setEntered(make(chan struct{}))
	f.exit.setBlock(make(chan struct{}))
	sid := f.authSession(t, 42, 912, session.StateInWorld)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = callHandle(t, f.h, sid, proto.OpcodeLeaveWorld, encodeLeave(t), f.sends.send)
	}()
	<-f.exit.enteredCh() // handler holds the account guard inside ExitWorld.

	started := make(chan struct{})
	acquired := make(chan struct{})
	go func() {
		close(started)
		unlock := f.reg.LockAccount(42)
		defer unlock()
		close(acquired)
	}()
	<-started
	// Force-schedule the contender: it must block on the guard the
	// leave holds. An unguarded ExitWorld would let it acquire.
	for i := 0; i < 1000; i++ {
		runtime.Gosched()
	}
	select {
	case <-acquired:
		t.Fatal("second lifecycle op entered while leave holds the guard")
	default:
	}
	f.exit.closeBlock() // release the flush; leave completes.
	<-done
	// Yield so the parked contender acquires the released guard.
	for i := 0; i < 1000; i++ {
		runtime.Gosched()
	}
	select {
	case <-acquired:
	default:
		t.Fatal("guard never released after leave")
	}
}

func TestDeleteVsLeaveSerialization(t *testing.T) {
	f := newHandlerFixture(t)
	f.exit.setEntered(make(chan struct{}))
	f.exit.setBlock(make(chan struct{}))
	inWorld := f.authSession(t, 7, 920, session.StateInWorld)
	authed := f.authSession(t, 7, 0, session.StateAuthenticated)
	f.chars.desc = character.Descriptor{ID: 920, Slot: 0, Name: "Aria", Revision: 2}

	leaveDone := make(chan struct{})
	go func() {
		defer close(leaveDone)
		_, _, _ = callHandle(t, f.h, inWorld, proto.OpcodeLeaveWorld, encodeLeave(t), f.sends.send)
	}()
	<-f.exit.enteredCh() // leave holds the guard mid-flush.

	deleteDone := make(chan error, 1)
	go func() {
		_, _, err := callHandle(t, f.h, authed, proto.OpcodeCharacterDelete,
			encodeDelete(t, 0), f.sends.send)
		deleteDone <- err
	}()
	// Force-schedule the deleter: a correct handler blocks it on the
	// guard, so FindBySlot cannot run yet. Without the guard it would
	// already have evaluated (and wrongly reported in-use).
	for i := 0; i < 1000; i++ {
		runtime.Gosched()
	}
	if finds, _ := f.chars.counts(); finds != 0 {
		t.Fatalf("FindBySlot reached while leave mid-flush")
	}
	// A parked contender proves the guard itself is held (not merely an
	// unscheduled deleter): it cannot acquire while leave is mid-flush.
	started := make(chan struct{})
	parked := make(chan struct{})
	go func() {
		close(started)
		unlock := f.reg.LockAccount(7)
		defer unlock()
		close(parked)
	}()
	<-started
	for i := 0; i < 1000; i++ {
		runtime.Gosched()
	}
	select {
	case <-parked:
		t.Fatal("lifecycle guard free while leave mid-flush")
	default:
	}
	f.exit.closeBlock()
	<-leaveDone
	<-parked
	if err := <-deleteDone; err != nil {
		t.Fatalf("delete err = %v", err)
	}
	frames := f.sends.all()
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want the delete 217", len(frames))
	}
	if got := decodeOp(t, frames[0]); got.Op != proto.CharacterOpDelete || got.OK != proto.CharacterOpOK {
		t.Errorf("217 = %+v, want delete/ok (character unbound by then)", got)
	}
	snap, _ := f.reg.Get(inWorld)
	if snap.State != session.StateAuthenticated || snap.HasCharacter {
		t.Errorf("leaver = %+v", snap)
	}
}

// ---------------------------------------------------------------------------
// delegation
// ---------------------------------------------------------------------------

func TestDelegatesUnownedOpcodes(t *testing.T) {
	for _, op := range []uint16{102, 124, 125} {
		op := op
		t.Run(fmt.Sprintf("opcode-%d", op), func(t *testing.T) {
			f := newHandlerFixture(t)
			sid := f.authSession(t, 42, 0, session.StateAuthenticated)
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
				t.Errorf("next = calls %d sid %d header %+v (same decoder %v)",
					f.next.calls, f.next.sid, f.next.header, f.next.dec == dec)
			}
			// Character service and WorldExit untouched.
			f.chars.mu.Lock()
			defer f.chars.mu.Unlock()
			if len(f.chars.createReqs) != 0 || len(f.chars.listAccounts) != 0 || len(f.chars.findSlots) != 0 {
				t.Error("character service invoked for unowned opcode")
			}
			f.exit.mu.Lock()
			exitCalls := f.exit.calls
			f.exit.mu.Unlock()
			if exitCalls != 0 {
				t.Error("WorldExit invoked for unowned opcode")
			}
			// SendFunc semantics passed through (marker 219 reply).
			if frames := f.sends.all(); len(frames) != 1 || frames[0].opcode != proto.OpcodeWorldReady {
				t.Errorf("frames = %+v, want delegated 219 marker", frames)
			}
		})
	}
}

func TestNilNextConsumesUnowned(t *testing.T) {
	reg := session.NewRegistry()
	chars := &fakeCharacters{}
	exit := &recordingExit{registry: reg}
	h, err := NewCharacterHandler(chars, reg, WorldExitFunc(exit.ExitWorld), nil)
	if err != nil {
		t.Fatal(err)
	}
	id := reg.Create(nil)
	if err := reg.Authenticate(id, "sub", 42, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	frame, err := proto.EncodeFrame(proto.Header{Opcode: 124, MsgVersion: 1},
		func(e *proto.Encoder) error {
			proto.EnterWorld{Slot: 0}.Encode(e)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	header, dec, _ := proto.DecodeFrame(frame)
	sends := &sendRecorder{}
	if err := h.Handle(context.Background(), id, header, dec, sends.send); err != nil {
		t.Errorf("err = %v, want nil consumption", err)
	}
	if frames := sends.all(); len(frames) != 0 {
		t.Errorf("frames = %+v, want none", frames)
	}
}
