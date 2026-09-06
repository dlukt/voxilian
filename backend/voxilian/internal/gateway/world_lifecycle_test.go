package gateway

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dlukt/voxilian/internal/character"
	"github.com/dlukt/voxilian/internal/proto"
	"github.com/dlukt/voxilian/internal/session"
	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/world"
)

// emptyBaseline streams no entity state (spec §7.3.4): T5b1
// integration tests combine real sim entry with the M3 provider
// using an EMPTY entity baseline, never duplicated fake IDs.
type emptyBaseline struct {
	mu    sync.Mutex
	calls int
}

func (b *emptyBaseline) StreamBaseline(context.Context, session.ID, int64, int64, BaselineSink) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	return nil
}

func (b *emptyBaseline) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// countingSpawn resolves one fixed position and counts calls.
type countingSpawn struct {
	mu    sync.Mutex
	pos   world.Vec3
	calls int
	err   error
}

func (s *countingSpawn) ResolveSpawn(context.Context, int64, int64) (world.Vec3, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.pos, s.err
}

func (s *countingSpawn) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// loggingWorld decorates a WorldSessionRuntime with an ordered call
// log while remaining the SAME instance for WorldExit + WorldEnter.
type loggingWorld struct {
	inner *WorldSessionRuntime
	log   *eventLog
}

func (l *loggingWorld) PrepareEnter(ctx context.Context, sid session.ID, acct, char int64) error {
	l.log.add("prepare")
	return l.inner.PrepareEnter(ctx, sid, acct, char)
}

func (l *loggingWorld) CommitEnter(sid session.ID) error {
	l.log.add("commit")
	return l.inner.CommitEnter(sid)
}

func (l *loggingWorld) AbortEnter(ctx context.Context, sid session.ID) error {
	l.log.add("abort")
	return l.inner.AbortEnter(ctx, sid)
}

func (l *loggingWorld) ExitWorld(ctx context.Context, sid session.ID, acct, char int64) error {
	l.log.add("exit")
	return l.inner.ExitWorld(ctx, sid, acct, char)
}

// lifecycleChars is a minimal CharacterService: one fixed descriptor.
type lifecycleChars struct {
	desc character.Descriptor
}

func (c *lifecycleChars) List(context.Context, int64) ([]character.ListEntry, error) {
	return []character.ListEntry{{Slot: 0, Name: c.desc.Name, Level: 20}}, nil
}

func (c *lifecycleChars) Create(context.Context, int64, character.CreateRequest) (int64, error) {
	return 0, errors.New("not used")
}

func (c *lifecycleChars) FindBySlot(_ context.Context, _ int64, slot uint8) (character.Descriptor, error) {
	if slot != 0 {
		return character.Descriptor{}, character.ErrNotFound
	}
	return c.desc, nil
}

func (c *lifecycleChars) Delete(context.Context, int64, int64) (int64, error) {
	return 0, errors.New("not used")
}

type lifecycleFixture struct {
	reg        *session.Registry
	presence   *PresenceRegistry
	fakeSim    *recordingSim
	spawn      *countingSpawn
	downstream *recordingDownstream
	runtime    *WorldSessionRuntime
	world      *loggingWorld
	provider   *emptyBaseline
	now        time.Time
	h          *EnterWorldHandler
	chars      *CharacterHandler
	sends      *sendRecorder
}

func newLifecycleFixture(t *testing.T, spawnPos world.Vec3, now time.Time) *lifecycleFixture {
	t.Helper()
	reg := session.NewRegistry()
	presence, err := NewPresenceRegistry(testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	fakeSim := &recordingSim{}
	spawn := &countingSpawn{pos: spawnPos}
	downstream := &recordingDownstream{}
	runtime, err := NewWorldSessionRuntime(fakeSim, presence, spawn, func() time.Time { return now }, downstream)
	if err != nil {
		t.Fatal(err)
	}
	log := &eventLog{}
	lw := &loggingWorld{inner: runtime, log: log}
	provider := &emptyBaseline{}
	charsSvc := &lifecycleChars{desc: character.Descriptor{ID: 500, Slot: 0, Name: "Aria", Revision: 3}}
	enter, err := NewEnterWorldHandler(EnterWorldHandlerDeps{
		Characters: charsSvc,
		Registry:   reg,
		Baseline:   provider,
		WorldExit:  lw, // same composition instance (spec §7.3.4)
		WorldEnter: lw,
		Tick:       func() uint32 { return 777 },
	})
	if err != nil {
		t.Fatal(err)
	}
	chars, err := NewCharacterHandler(charsSvc, reg, lw, enter)
	if err != nil {
		t.Fatal(err)
	}
	return &lifecycleFixture{
		reg: reg, presence: presence, fakeSim: fakeSim, spawn: spawn,
		downstream: downstream, runtime: runtime, world: lw,
		provider: provider, now: now, h: enter, chars: chars,
		sends: &sendRecorder{},
	}
}

func (f *lifecycleFixture) authSession(t *testing.T, accountID int64) session.ID {
	t.Helper()
	id := f.reg.Create(newTakeoverConn())
	if err := f.reg.Authenticate(id, "sub", accountID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	return id
}

func (f *lifecycleFixture) enter(t *testing.T, sid session.ID) error {
	t.Helper()
	sends := &sendRecorder{}
	_, _, err := callHandle(t, f.h, sid, proto.OpcodeEnterWorld, encodeEnter(t, 0), sends.send)
	f.sends = sends
	return err
}

// sentOpcodes returns the recorded S→C opcode sequence.
func sentOpcodes(sends *sendRecorder) []uint16 {
	var ops []uint16
	for _, fr := range sends.all() {
		ops = append(ops, fr.opcode)
	}
	return ops
}

func TestLifecycleEnterSuccessOrder(t *testing.T) {
	f := newLifecycleFixture(t, world.Vec3{X: 4}, worldRuntimeBase)
	sid := f.authSession(t, 11)
	if err := f.enter(t, sid); err != nil {
		t.Fatalf("enter: %v", err)
	}
	// Exact observable order: prepare, 217 OK, baseline, 219, commit.
	// (Begin/Complete are registry-internal, implied by IN_WORLD.)
	if got := f.world.log.slice(); !equalStrings(got, []string{"prepare", "commit"}) {
		t.Fatalf("world order = %v, want [prepare commit]", got)
	}
	if got := sentOpcodes(f.sends); !equalU16(got, []uint16{proto.OpcodeCharacterOp, proto.OpcodeWorldReady}) {
		t.Fatalf("wire order = %v, want [217 219]", got)
	}
	if n := f.provider.count(); n != 1 {
		t.Fatalf("baseline calls = %d, want 1", n)
	}
	snap, err := f.presence.Snapshot(sid)
	if err != nil {
		t.Fatalf("presence after enter: %v", err)
	}
	if snap.EntityID != 1 || snap.OwnNetID != 1 || !snap.HeartbeatAt.Equal(worldRuntimeBase) {
		t.Fatalf("presence = %+v", snap)
	}
	if s, _ := f.reg.Get(sid); s.State != session.StateInWorld {
		t.Fatalf("session state = %s, want IN_WORLD", s.State)
	}
}

func TestLifecyclePrepareFailureNo217(t *testing.T) {
	f := newLifecycleFixture(t, world.Vec3{X: 4}, worldRuntimeBase)
	f.spawn.err = errors.New("world down")
	sid := f.authSession(t, 11)
	err := f.enter(t, sid)
	cerr, ok := err.(*ClientError)
	if !ok || cerr.Code != proto.ErrorCodeRetry {
		t.Fatalf("prepare failure err = %v, want 202 retry", err)
	}
	if len(f.sends.all()) != 0 {
		t.Fatalf("frames sent despite prepare failure: %v", sentOpcodes(f.sends))
	}
	if n := f.provider.count(); n != 0 {
		t.Fatalf("baseline ran without staged entity")
	}
	if s, _ := f.reg.Get(sid); s.State != session.StateAuthenticated || s.HasCharacter {
		t.Fatalf("binding survives prepare failure: %+v", s)
	}
	if _, err := f.presence.Snapshot(sid); !errors.Is(err, ErrPresenceNotFound) {
		t.Fatalf("presence after prepare failure: %v", err)
	}
}

func TestLifecycle217FailureRollback(t *testing.T) {
	f := newLifecycleFixture(t, world.Vec3{X: 4}, worldRuntimeBase)
	sid := f.authSession(t, 11)
	boom := errors.New("217 write down")
	send := SendFunc(func(opcode uint16, _ uint16, _ func(*proto.Encoder) error) error {
		if opcode == proto.OpcodeCharacterOp {
			return boom
		}
		return nil
	})
	_, _, err := callHandle(t, f.h, sid, proto.OpcodeEnterWorld, encodeEnter(t, 0), send)
	if err == nil {
		t.Fatalf("217 failure returned nil")
	}
	if got := f.world.log.slice(); !equalStrings(got, []string{"prepare", "abort"}) {
		t.Fatalf("world calls = %v, want [prepare abort]", got)
	}
	if f.fakeSim.adds != 1 || len(f.fakeSim.removes) != 1 {
		t.Fatalf("staged entity leaked: adds=%d removes=%v", f.fakeSim.adds, f.fakeSim.removes)
	}
	if s, _ := f.reg.Get(sid); s.State != session.StateAuthenticated || s.HasCharacter {
		t.Fatalf("binding survives 217 failure: %+v", s)
	}
	if _, err := f.presence.Snapshot(sid); !errors.Is(err, ErrPresenceNotFound) {
		t.Fatalf("presence after 217 failure: %v", err)
	}
}

func TestLifecycleBaselineFailureRollback(t *testing.T) {
	f := newLifecycleFixture(t, world.Vec3{X: 4}, worldRuntimeBase)
	sid := f.authSession(t, 11)
	// Operational baseline failure with an EMPTY staging baseline:
	// replace provider with a failing one for this enter.
	provider := BaselineProviderFunc(func(context.Context, session.ID, int64, int64, BaselineSink) error {
		return errors.New("cells unavailable")
	})
	old := f.h.Baseline
	f.h.Baseline = provider
	defer func() { f.h.Baseline = old }()
	err := f.enter(t, sid)
	cerr, ok := err.(*ClientError)
	if !ok || cerr.Code != proto.ErrorCodeRetry {
		t.Fatalf("baseline failure err = %v, want 202 retry", err)
	}
	if got := f.world.log.slice(); !equalStrings(got, []string{"prepare", "abort"}) {
		t.Fatalf("world calls = %v, want [prepare abort]", got)
	}
	if len(f.fakeSim.removes) != 1 {
		t.Fatalf("staged entity leaked: removes=%v", f.fakeSim.removes)
	}
	if _, err := f.presence.Snapshot(sid); !errors.Is(err, ErrPresenceNotFound) {
		t.Fatalf("presence after baseline failure: %v", err)
	}
}

func TestLifecycle219FailureRollback(t *testing.T) {
	f := newLifecycleFixture(t, world.Vec3{X: 4}, worldRuntimeBase)
	sid := f.authSession(t, 11)
	boom := errors.New("219 write down")
	send := SendFunc(func(opcode uint16, _ uint16, _ func(*proto.Encoder) error) error {
		if opcode == proto.OpcodeWorldReady {
			return boom
		}
		return nil
	})
	_, _, err := callHandle(t, f.h, sid, proto.OpcodeEnterWorld, encodeEnter(t, 0), send)
	if err == nil {
		t.Fatalf("219 failure returned nil")
	}
	if got := f.world.log.slice(); !equalStrings(got, []string{"prepare", "abort"}) {
		t.Fatalf("world calls = %v, want [prepare abort]", got)
	}
	if len(f.fakeSim.removes) != 1 {
		t.Fatalf("staged entity leaked: removes=%v", f.fakeSim.removes)
	}
	if _, err := f.presence.Snapshot(sid); !errors.Is(err, ErrPresenceNotFound) {
		t.Fatalf("presence after 219 failure: %v", err)
	}
}

func TestLifecycleCompleteFailureCleanup(t *testing.T) {
	f := newLifecycleFixture(t, world.Vec3{X: 4}, worldRuntimeBase)
	sid := f.authSession(t, 11)
	// Evil baseline: remove the session mid-stream so 219 still writes
	// but CompleteEnterWorld fails after the physical barrier.
	provider := BaselineProviderFunc(func(_ context.Context, id session.ID, _ int64, _ int64, _ BaselineSink) error {
		f.reg.Remove(id)
		return nil
	})
	old := f.h.Baseline
	f.h.Baseline = provider
	defer func() { f.h.Baseline = old }()
	err := f.enter(t, sid)
	var cerr *ClientError
	if errors.As(err, &cerr) {
		t.Fatalf("complete failure mapped to 202 %d: client may have seen 219", cerr.Code)
	}
	if err == nil {
		t.Fatalf("complete failure returned nil")
	}
	if got := f.world.log.slice(); !equalStrings(got, []string{"prepare", "abort"}) {
		t.Fatalf("world calls = %v, want [prepare abort]", got)
	}
	if _, err := f.presence.Snapshot(sid); !errors.Is(err, ErrPresenceNotFound) {
		t.Fatalf("presence after complete failure: %v", err)
	}
}

func TestLifecycleCommitFailureCleanup(t *testing.T) {
	f := newLifecycleFixture(t, world.Vec3{X: 4}, worldRuntimeBase)
	sid := f.authSession(t, 11)
	// Force a commit conflict: pre-activate this sid's presence.
	if _, err := f.presence.Activate(sid, 500, 999, world.CellCoord{}, worldRuntimeBase); err != nil {
		t.Fatal(err)
	}
	err := f.enter(t, sid)
	var cerr *ClientError
	if errors.As(err, &cerr) {
		t.Fatalf("commit failure mapped to 202 %d, want internal", cerr.Code)
	}
	if err == nil {
		t.Fatalf("commit failure returned nil")
	}
	if got := f.world.log.slice(); !equalStrings(got, []string{"prepare", "commit", "abort"}) {
		t.Fatalf("world calls = %v, want [prepare commit abort]", got)
	}
	// Staged entity removed; just-completed binding rolled back.
	if len(f.fakeSim.removes) != 1 {
		t.Fatalf("staged entity leaked: removes=%v", f.fakeSim.removes)
	}
	if s, ok := f.reg.Get(sid); !ok || s.State != session.StateAuthenticated || s.HasCharacter {
		t.Fatalf("binding survives commit failure: %+v", s)
	}
}

func TestLifecycleNormalLeave(t *testing.T) {
	f := newLifecycleFixture(t, world.Vec3{X: 4}, worldRuntimeBase)
	sid := f.authSession(t, 11)
	if err := f.enter(t, sid); err != nil {
		t.Fatalf("enter: %v", err)
	}
	leave, err := proto.EncodeFrame(proto.Header{Opcode: proto.OpcodeLeaveWorld, MsgVersion: 1},
		func(e *proto.Encoder) error { proto.LeaveWorld{}.Encode(e); return nil })
	if err != nil {
		t.Fatal(err)
	}
	sends := &sendRecorder{}
	_, _, herr := callHandle(t, f.chars, sid, proto.OpcodeLeaveWorld, leave, sends.send)
	if herr != nil {
		t.Fatalf("leave: %v", herr)
	}
	if len(sends.all()) != 0 {
		t.Fatalf("leave replied %d frames, want silent success", len(sends.all()))
	}
	// Order: downstream flush, sim remove, presence deactivate.
	if got := f.world.log.slice(); !equalStrings(got, []string{"prepare", "commit", "exit"}) {
		t.Fatalf("world calls = %v", got)
	}
	if f.downstream.calls != 1 {
		t.Fatalf("downstream flush calls = %d, want 1", f.downstream.calls)
	}
	if _, err := f.presence.Snapshot(sid); !errors.Is(err, ErrPresenceNotFound) {
		t.Fatalf("presence survives leave: %v", err)
	}
	if s, _ := f.reg.Get(sid); s.State != session.StateAuthenticated || s.HasCharacter {
		t.Fatalf("session after leave: %+v", s)
	}
}

func TestLifecycleLeaveRuntimeFailureRetry(t *testing.T) {
	f := newLifecycleFixture(t, world.Vec3{X: 4}, worldRuntimeBase)
	sid := f.authSession(t, 11)
	if err := f.enter(t, sid); err != nil {
		t.Fatalf("enter: %v", err)
	}
	f.downstream.err = errors.New("flush unavailable")
	leave, err := proto.EncodeFrame(proto.Header{Opcode: proto.OpcodeLeaveWorld, MsgVersion: 1},
		func(e *proto.Encoder) error { proto.LeaveWorld{}.Encode(e); return nil })
	if err != nil {
		t.Fatal(err)
	}
	_, _, herr := callHandle(t, f.chars, sid, proto.OpcodeLeaveWorld, leave, (&sendRecorder{}).send)
	cerr, ok := herr.(*ClientError)
	if !ok || cerr.Code != proto.ErrorCodeRetry {
		t.Fatalf("leave failure err = %v, want 202 retry", herr)
	}
	if s, _ := f.reg.Get(sid); s.State != session.StateInWorld || !s.HasCharacter {
		t.Fatalf("session disturbed by failed leave: %+v", s)
	}
	if _, err := f.presence.Snapshot(sid); err != nil {
		t.Fatalf("presence lost on failed leave: %v", err)
	}
	if len(f.fakeSim.removes) != 0 {
		t.Fatalf("entity removed despite flush failure")
	}
}

func TestLifecycleTakeoverRealRuntime(t *testing.T) {
	f := newLifecycleFixture(t, world.Vec3{X: 4}, worldRuntimeBase)
	old := f.authSession(t, 11)
	if err := f.enter(t, old); err != nil {
		t.Fatalf("old enter: %v", err)
	}
	oldSnap, _ := f.presence.Snapshot(old)
	fresh := f.authSession(t, 11)
	if err := f.enter(t, fresh); err != nil {
		t.Fatalf("takeover enter: %v", err)
	}
	// Old presence gone; old entity removed (first staged entity).
	if _, err := f.presence.Snapshot(old); !errors.Is(err, ErrPresenceNotFound) {
		t.Fatalf("old presence survives takeover: %v", err)
	}
	if len(f.fakeSim.removes) != 1 || f.fakeSim.removes[0] != oldSnap.EntityID {
		t.Fatalf("removes = %v, want [old entity %d]", f.fakeSim.removes, uint64(oldSnap.EntityID))
	}
	// New epoch uses a fresh sim EntityID and fresh presence.
	newSnap, err := f.presence.Snapshot(fresh)
	if err != nil {
		t.Fatalf("new presence: %v", err)
	}
	if newSnap.EntityID == oldSnap.EntityID {
		t.Fatalf("takeover reused entity %d", uint64(newSnap.EntityID))
	}
	if newSnap.OwnNetID != 1 {
		t.Fatalf("new OwnNetID = %d", newSnap.OwnNetID)
	}
	// Old session retired through the existing kick path.
	if s, ok := f.reg.Get(old); ok {
		t.Fatalf("old session survives: %+v", s)
	}
	// Stale-state barrier order: old exit before new prepare/commit.
	want := []string{"prepare", "commit", "exit", "prepare", "commit"}
	if got := f.world.log.slice(); !equalStrings(got, want) {
		t.Fatalf("world order = %v, want %v", got, want)
	}
}

func TestLifecycleTakeoverFlushFailure(t *testing.T) {
	f := newLifecycleFixture(t, world.Vec3{X: 4}, worldRuntimeBase)
	old := f.authSession(t, 11)
	if err := f.enter(t, old); err != nil {
		t.Fatalf("old enter: %v", err)
	}
	oldSnap, _ := f.presence.Snapshot(old)
	f.downstream.err = errors.New("flush unavailable")
	fresh := f.authSession(t, 11)
	err := f.enter(t, fresh)
	cerr, ok := err.(*ClientError)
	if !ok || cerr.Code != proto.ErrorCodeRetry {
		t.Fatalf("takeover flush failure err = %v, want 202 retry", err)
	}
	// Old world untouched and unkicked.
	if s, _ := f.reg.Get(old); s.State != session.StateInWorld || !s.HasCharacter {
		t.Fatalf("old session disturbed: %+v", s)
	}
	if _, err := f.presence.Snapshot(old); err != nil {
		t.Fatalf("old presence lost: %v", err)
	}
	if len(f.fakeSim.removes) != 0 {
		t.Fatalf("old entity removed despite flush failure")
	}
	// New staging never started: no spawn, no baseline, no presence.
	if n := f.spawn.count(); n != 1 {
		t.Fatalf("spawn calls = %d, want 1 (old enter only)", n)
	}
	if n := f.provider.count(); n != 1 {
		t.Fatalf("baseline calls = %d, want 1 (old enter only)", n)
	}
	if _, err := f.presence.Snapshot(fresh); !errors.Is(err, ErrPresenceNotFound) {
		t.Fatalf("new presence exists after failed takeover: %v", err)
	}
	_ = oldSnap
}

func TestLifecycleReenterFreshEpoch(t *testing.T) {
	now := worldRuntimeBase
	f := newLifecycleFixture(t, world.Vec3{X: 4}, now)
	sid := f.authSession(t, 11)
	if err := f.enter(t, sid); err != nil {
		t.Fatalf("enter 1: %v", err)
	}
	first, _ := f.presence.Snapshot(sid)
	leave, err := proto.EncodeFrame(proto.Header{Opcode: proto.OpcodeLeaveWorld, MsgVersion: 1},
		func(e *proto.Encoder) error { proto.LeaveWorld{}.Encode(e); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, _, herr := callHandle(t, f.chars, sid, proto.OpcodeLeaveWorld, leave, (&sendRecorder{}).send); herr != nil {
		t.Fatalf("leave: %v", herr)
	}
	if err := f.enter(t, sid); err != nil {
		t.Fatalf("enter 2: %v", err)
	}
	second, err := f.presence.Snapshot(sid)
	if err != nil {
		t.Fatalf("presence after re-enter: %v", err)
	}
	if second.EntityID == first.EntityID {
		t.Fatalf("re-enter reused entity %d", uint64(second.EntityID))
	}
	if second.OwnNetID != 1 {
		t.Fatalf("re-enter OwnNetID = %d", second.OwnNetID)
	}
	// Fresh full buckets: the whole burst is available again.
	for i := 0; i < 10; i++ {
		if ok, _ := f.presence.AllowMove(sid, now); !ok {
			t.Fatalf("re-enter move burst exhausted at %d", i)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalU16(a, b []uint16) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// waitSimTick polls CurrentTick (atomic, race-free) until want.
func waitSimTick(t *testing.T, e interface{ CurrentTick() uint32 }, want uint32) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for e.CurrentTick() != want {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for sim tick %d", want)
		}
	}
}

// TestGatewayRealEngineMovementProof is the B68 composition: real
// Engine.Run + owner mailbox + real Presence EntityID + real
// GameplayIngressHandler. A 102 submitted through the handler moves
// the entity under existing M4-T2 semantics after one manual tick,
// with the reconciliation anchor advanced. T5b1 emits no 205: silent
// handler success is the whole signal.
func TestGatewayRealEngineMovementProof(t *testing.T) {
	clk := new(gwTestClock)
	e := realSimEngine(t, clk)
	cancel, done := runSimOwner(t, e)
	ctx := context.Background()
	snap, err := e.EnqueueAddEntity(ctx, world.Vec3{})
	if err != nil {
		t.Fatalf("stage entity: %v", err)
	}
	presence, err := NewPresenceRegistry(testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	now := worldRuntimeBase
	if _, err := presence.Activate(7, 500, snap.ID, snap.Cell, now); err != nil {
		t.Fatalf("activate: %v", err)
	}
	h, err := NewGameplayIngressHandler(presence, e, func() time.Time { return now }, nil)
	if err != nil {
		t.Fatal(err)
	}
	frame := encodeMove(t, proto.Header{Opcode: proto.OpcodeMove, MsgVersion: 1, Seq: 9, Tick: 0},
		proto.Move{InputSeq: 1, HeldDirs: sim.MoveDirForward, Yaw: 0})
	header, dec, err := proto.DecodeFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Handle(ctx, 7, header, dec, nil); err != nil {
		t.Fatalf("102 through handler: %v", err)
	}
	clk.current().pulse(worldRuntimeBase)
	waitSimTick(t, e, 1)
	stopSimOwner(t, cancel, done)
	got, err := e.Entity(snap.ID)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if want := -3.5 / 20.0; got.Position.X != 0 || got.Position.Z < want-1e-9 || got.Position.Z > want+1e-9 {
		t.Fatalf("position = %v, want walk step -Z", got.Position)
	}
	if got.LastProcessedInputSeq != 1 {
		t.Fatalf("anchor = %d, want 1", got.LastProcessedInputSeq)
	}
}
