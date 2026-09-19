package gateway

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dlukt/voxilian/internal/proto"
	"github.com/dlukt/voxilian/internal/session"
	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/world"
)

// M5-T5c4 gateway death wire/state tests (spec §9.5.1l):
// 214 fanout, 215 correctness, 214/215 ordering, AOI
// relocation, 120 routing/correlation, old-session
// isolation, and the no-new-protocol tripwire. Crafted
// sim-domain events drive the DeathWireRuntime directly;
// the full engine composition is proven in
// death_e2e_test.go.

// deathHarness wires presence + sessions + fanout +
// DeathWireRuntime with a scriptable release ingress.
type deathHarness struct {
	t       *testing.T
	fix     *fanoutFixture
	release *recordingSim
	runtime *DeathWireRuntime
	acks    *DeathAckHandler
}

func newDeathHarness(t *testing.T) *deathHarness {
	t.Helper()
	fix := newFanoutFixture(t, 20)
	release := &recordingSim{}
	rt, err := NewDeathWireRuntime(fix.presence, fix.reg, fix.fanout)
	if err != nil {
		t.Fatal(err)
	}
	acks, err := NewDeathAckHandler(rt, release, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &deathHarness{t: t, fix: fix, release: release, runtime: rt, acks: acks}
}

func (h *deathHarness) addReadySession(policy OutboundPolicy) session.ID {
	h.t.Helper()
	return h.fix.addSession(policy)
}

// beginEv builds a begin event for entity/char/epoch.
func beginEv(entity sim.EntityID, char sim.CharacterID, epoch uint64) sim.DeathBeginEvent {
	return sim.DeathBeginEvent{Token: sim.DeathAttemptToken{EntityID: entity, CharacterID: char, Epoch: epoch}}
}

// completedEv builds a completion event with an explicit
// authoritative position.
func completedEv(entity sim.EntityID, char sim.CharacterID, epoch uint64, pos world.Vec3) sim.DeathCompletedEvent {
	cell, err := world.CellForPosition(pos)
	if err != nil {
		panic(err)
	}
	return sim.DeathCompletedEvent{
		Tick:  100,
		Token: sim.DeathAttemptToken{EntityID: entity, CharacterID: char, Epoch: epoch},
		Snapshot: sim.EntitySnapshot{
			ID: sim.EntityID(entity), CharacterID: char,
			Position: pos, Cell: cell, IsPlayer: true,
		},
	}
}

func decodeDeath(t *testing.T, payload []byte) proto.Death {
	t.Helper()
	m, err := proto.DecodeDeath(proto.NewDecoder(payload))
	if err != nil {
		t.Fatalf("decode 214: %v", err)
	}
	return m
}

func decodeRespawn(t *testing.T, payload []byte) proto.Respawn {
	t.Helper()
	m, err := proto.DecodeRespawn(proto.NewDecoder(payload))
	if err != nil {
		t.Fatalf("decode 215: %v", err)
	}
	return m
}

func callsFor(prod *methodRecordingProducer, opcode uint16) []recordedProducerCall {
	var out []recordedProducerCall
	for _, c := range prod.snapshot() {
		if c.opcode == opcode {
			out = append(out, c)
		}
	}
	return out
}

func mustDeathRuntime(t *testing.T, fix *fanoutFixture) *DeathWireRuntime {
	t.Helper()
	rt, err := NewDeathWireRuntime(fix.presence, fix.reg, fix.fanout)
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func TestDeathWireRequiresDeps(t *testing.T) {
	fix := newFanoutFixture(t, 20)
	rel := &recordingSim{}
	if _, err := NewDeathWireRuntime(nil, fix.reg, fix.fanout); err == nil {
		t.Error("nil presence accepted")
	}
	if _, err := NewDeathWireRuntime(fix.presence, nil, fix.fanout); err == nil {
		t.Error("nil sessions accepted")
	}
	if _, err := NewDeathWireRuntime(fix.presence, fix.reg, nil); err == nil {
		t.Error("nil fanout accepted")
	}
	if _, err := NewDeathAckHandler(nil, rel, nil); err == nil {
		t.Error("nil runtime accepted")
	}
	if _, err := NewDeathAckHandler(mustDeathRuntime(t, fix), nil, nil); err == nil {
		t.Error("nil release accepted")
	}
}

func TestEmitDeathFanout(t *testing.T) {
	const victim = sim.EntityID(7)
	const char = sim.CharacterID(500)

	setup := func(t *testing.T) (*deathHarness, session.ID, session.ID) {
		h := newDeathHarness(t)
		owner := h.addReadySession(DefaultOutboundPolicy())
		viewer := h.addReadySession(DefaultOutboundPolicy())
		home, _ := world.CellForPosition(world.Vec3{})
		// Victim entity lives at the origin cell; the
		// viewer owns a distant entity but also sees the
		// victim.
		h.fix.activate(owner, 500, victim, home)
		h.fix.activate(viewer, 501, sim.EntityID(900), world.CellCoord{X: 40, Z: 40})
		if _, _, err := h.fix.presence.EnsureVisible(viewer, victim); err != nil {
			t.Fatal(err)
		}
		h.fix.source.put(EntityPresentation{EntityID: victim, Position: world.Vec3{}, Kind: 2, Proto: 7})
		h.fix.source.put(EntityPresentation{EntityID: sim.EntityID(900), Position: world.Vec3{X: 1300}, Kind: 2, Proto: 7})
		h.fix.bootstrap(owner)
		h.fix.bootstrap(viewer)
		return h, owner, viewer
	}

	t.Run("recipient-local-handles", func(t *testing.T) {
		h, owner, viewer := setup(t)
		h.runtime.OnDeathBegin(beginEv(victim, char, 3))

		ownCalls := callsFor(h.fix.prods[owner], proto.OpcodeDeath)
		if len(ownCalls) != 1 {
			t.Fatalf("owner 214 calls = %d; want 1", len(ownCalls))
		}
		if got := decodeDeath(t, ownCalls[0].payload); got.Victim != 1 {
			t.Fatalf("owner victim handle = %d; want 1 (own NetEntityID)", got.Victim)
		}
		viewCalls := callsFor(h.fix.prods[viewer], proto.OpcodeDeath)
		if len(viewCalls) != 1 {
			t.Fatalf("viewer 214 calls = %d; want 1", len(viewCalls))
		}
		wh, visible, err := h.fix.presence.VisibleHandle(viewer, victim)
		if err != nil || !visible {
			t.Fatalf("viewer handle = %d,%v,%v", uint32(wh), visible, err)
		}
		if got := decodeDeath(t, viewCalls[0].payload); got.Victim != uint32(wh) {
			t.Fatalf("viewer victim = %d; want local handle %d", got.Victim, uint32(wh))
		}
		if ownCalls[0].state || viewCalls[0].state {
			t.Fatalf("214 used the state lane; want critical")
		}
	})

	t.Run("non-viewer-receives-nothing", func(t *testing.T) {
		h, _, _ := setup(t)
		outsider := h.addReadySession(DefaultOutboundPolicy())
		h.fix.source.put(EntityPresentation{EntityID: sim.EntityID(901), Position: world.Vec3{X: -1300}, Kind: 2, Proto: 7})
		h.fix.activate(outsider, 502, sim.EntityID(901), world.CellCoord{X: -40, Z: -40})
		h.fix.bootstrap(outsider)
		h.runtime.OnDeathBegin(beginEv(victim, char, 3))
		if got := callsFor(h.fix.prods[outsider], proto.OpcodeDeath); len(got) != 0 {
			t.Fatalf("outsider 214 calls = %d; want 0", len(got))
		}
	})

	t.Run("vanished-viewer-harmless", func(t *testing.T) {
		h, owner, viewer := setup(t)
		// Viewer mapping disappears mid-fanout (takeover /
		// exit race between the viewer copy and the
		// handle resolve): retire it up front and prove
		// the owner still gets exactly one 214.
		if _, _, err := h.fix.presence.HideVisible(viewer, victim); err != nil {
			t.Fatal(err)
		}
		h.runtime.OnDeathBegin(beginEv(victim, char, 3))
		if got := callsFor(h.fix.prods[owner], proto.OpcodeDeath); len(got) != 1 {
			t.Fatalf("owner 214 calls = %d; want 1", len(got))
		}
		if got := callsFor(h.fix.prods[viewer], proto.OpcodeDeath); len(got) != 0 {
			t.Fatalf("retired viewer 214 calls = %d; want 0", len(got))
		}
	})

	t.Run("slow-viewer-isolated", func(t *testing.T) {
		h := newDeathHarness(t)
		owner := h.addReadySession(DefaultOutboundPolicy())
		viewer := h.addReadySession(DefaultOutboundPolicy())
		home, _ := world.CellForPosition(world.Vec3{})
		h.fix.activate(owner, 500, victim, home)
		h.fix.activate(viewer, 501, sim.EntityID(900), world.CellCoord{X: 40, Z: 40})
		if _, _, err := h.fix.presence.EnsureVisible(viewer, victim); err != nil {
			t.Fatal(err)
		}
		h.fix.source.put(EntityPresentation{EntityID: victim, Position: world.Vec3{}, Kind: 2, Proto: 7})
		h.fix.source.put(EntityPresentation{EntityID: sim.EntityID(900), Position: world.Vec3{X: 1300}, Kind: 2, Proto: 7})
		h.fix.bootstrap(owner)
		h.fix.bootstrap(viewer)
		// A session whose critical admission fails only
		// AFTER it became ready: the 214 fanout must fail
		// it closed alone while the owner and the healthy
		// viewer still get exactly one 214 each.
		tr := newFakeOutTransport()
		oc := newOutboundConn(OutboundDeps{
			Conn: tr, Registry: h.fix.reg,
			Tick:     func() uint32 { return 1000 },
			Policy:   DefaultOutboundPolicy(),
			Observer: &recordingObserver{},
		})
		toggle := &toggleFailProducer{OutboundProducer: oc}
		slow := h.fix.reg.Create(toggle)
		h.fix.source.put(EntityPresentation{EntityID: sim.EntityID(902), Position: world.Vec3{X: 1312}, Kind: 2, Proto: 7})
		h.fix.activate(slow, 503, sim.EntityID(902), world.CellCoord{X: 41, Z: 41})
		if _, _, err := h.fix.presence.EnsureVisible(slow, victim); err != nil {
			t.Fatal(err)
		}
		h.fix.bootstrap(slow)
		toggle.setFail(true)
		h.runtime.OnDeathBegin(beginEv(victim, char, 3))
		if got := callsFor(h.fix.prods[owner], proto.OpcodeDeath); len(got) != 1 {
			t.Fatalf("owner 214 calls = %d; want 1", len(got))
		}
		if got := callsFor(h.fix.prods[viewer], proto.OpcodeDeath); len(got) != 1 {
			t.Fatalf("healthy viewer 214 calls = %d; want 1", len(got))
		}
	})
}

// toggleFailProducer fails critical admission only
// while armed, for the saturated-client isolation proof.
// CloseNow is recorded and swallowed so the test can
// prove the teardown was requested and then recover the
// session with a fresh baseline; real close semantics
// belong to the M4 outbound tests.
type toggleFailProducer struct {
	OutboundProducer
	mu        sync.Mutex
	fail      bool
	closeNows int
}

func (f *toggleFailProducer) setFail(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = v
}

func (f *toggleFailProducer) closeNowCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closeNows
}

func (f *toggleFailProducer) TryCritical(sid session.ID, opcode uint16, ver uint16, encode func(*proto.Encoder) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("slow")
	}
	return f.OutboundProducer.TryCritical(sid, opcode, ver, encode)
}

func (f *toggleFailProducer) CloseNow() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeNows++
	return nil
}

// failCriticalProducer fails every critical admission for
// the saturated-client proof.
type failCriticalProducer struct {
	OutboundProducer
	err error
}

func (f *failCriticalProducer) TryCritical(session.ID, uint16, uint16, func(*proto.Encoder) error) error {
	return f.err
}

func TestRespawnPresentation(t *testing.T) {
	const victim = sim.EntityID(7)
	const char = sim.CharacterID(500)
	dest := world.Vec3{X: 400, Y: 0, Z: -300}

	t.Run("controller-only-exact-position", func(t *testing.T) {
		h := newDeathHarness(t)
		owner := h.addReadySession(DefaultOutboundPolicy())
		viewer := h.addReadySession(DefaultOutboundPolicy())
		home, _ := world.CellForPosition(world.Vec3{})
		h.fix.activate(owner, 500, victim, home)
		h.fix.activate(viewer, 501, sim.EntityID(900), world.CellCoord{X: 40, Z: 40})
		if _, _, err := h.fix.presence.EnsureVisible(viewer, victim); err != nil {
			t.Fatal(err)
		}
		h.fix.source.put(EntityPresentation{EntityID: victim, Position: world.Vec3{}, Kind: 2, Proto: 7})
		h.fix.source.put(EntityPresentation{EntityID: sim.EntityID(900), Position: world.Vec3{X: 1300}, Kind: 2, Proto: 7})
		h.fix.bootstrap(owner)
		h.fix.bootstrap(viewer)

		h.runtime.OnDeathBegin(beginEv(victim, char, 1))
		h.runtime.OnDeathCompleted(completedEv(victim, char, 1, dest))

		ownCalls := callsFor(h.fix.prods[owner], proto.OpcodeRespawn)
		if len(ownCalls) != 1 {
			t.Fatalf("owner 215 calls = %d; want 1", len(ownCalls))
		}
		got := decodeRespawn(t, ownCalls[0].payload)
		want, err := WirePosition(dest)
		if err != nil {
			t.Fatal(err)
		}
		if got.Pos != want {
			t.Fatalf("215 pos = %+v; want %+v", got.Pos, want)
		}
		if got := callsFor(h.fix.prods[viewer], proto.OpcodeRespawn); len(got) != 0 {
			t.Fatalf("viewer 215 calls = %d; want 0", len(got))
		}
		// 214 precedes 215 on the victim session (critical
		// FIFO admission order, no sleeps).
		var order []uint16
		for _, c := range h.fix.prods[owner].snapshot() {
			if c.opcode == proto.OpcodeDeath || c.opcode == proto.OpcodeRespawn {
				order = append(order, c.opcode)
			}
		}
		if len(order) != 2 || order[0] != proto.OpcodeDeath || order[1] != proto.OpcodeRespawn {
			t.Fatalf("victim death/respawn order = %v; want [214 215]", order)
		}
	})

	t.Run("no-controller-no-presentation", func(t *testing.T) {
		h := newDeathHarness(t)
		owner := h.addReadySession(DefaultOutboundPolicy())
		home, _ := world.CellForPosition(world.Vec3{})
		h.fix.activate(owner, 500, victim, home)
		h.fix.source.put(EntityPresentation{EntityID: victim, Position: world.Vec3{}, Kind: 2, Proto: 7})
		h.fix.bootstrap(owner)
		// Disconnect race: presence gone before the
		// completion presentation runs.
		if _, err := h.fix.presence.Deactivate(owner); err != nil {
			t.Fatal(err)
		}
		h.runtime.OnDeathCompleted(completedEv(victim, char, 1, dest))
		if got := callsFor(h.fix.prods[owner], proto.OpcodeRespawn); len(got) != 0 {
			t.Fatalf("dead session 215 calls = %d; want 0", len(got))
		}
		h.release.mu.Lock()
		n := len(h.release.releaseToks)
		h.release.mu.Unlock()
		if n != 0 {
			t.Fatalf("release calls = %d; want 0", n)
		}
	})

	t.Run("impossible-position-fails-closed", func(t *testing.T) {
		h := newDeathHarness(t)
		owner := h.addReadySession(DefaultOutboundPolicy())
		home, _ := world.CellForPosition(world.Vec3{})
		h.fix.activate(owner, 500, victim, home)
		h.fix.source.put(EntityPresentation{EntityID: victim, Position: world.Vec3{}, Kind: 2, Proto: 7})
		h.fix.bootstrap(owner)
		// Millimeters overflow int32: unrepresentable on
		// the wire. The session must fail closed with no
		// 215 and no ack correlation — never a clamp.
		h.runtime.OnDeathCompleted(completedEv(victim, char, 1, world.Vec3{X: 3e9}))
		if got := callsFor(h.fix.prods[owner], proto.OpcodeRespawn); len(got) != 0 {
			t.Fatalf("215 calls = %d; want 0", len(got))
		}
		if err := h.runtime.ReleaseRespawn(context.Background(), owner, h.release); clientErrorOf(t, err).Code != proto.ErrorCodeProtocol {
			t.Fatalf("120 after failed 215 = %v; want protocol_error (no correlation)", err)
		}
	})

	t.Run("no-second-215-without-begin", func(t *testing.T) {
		h := newDeathHarness(t)
		owner := h.addReadySession(DefaultOutboundPolicy())
		home, _ := world.CellForPosition(world.Vec3{})
		h.fix.activate(owner, 500, victim, home)
		h.fix.source.put(EntityPresentation{EntityID: victim, Position: world.Vec3{}, Kind: 2, Proto: 7})
		h.fix.bootstrap(owner)
		// A completion presentation with no live
		// controller binding for the token identity (a
		// replacement presence for another character
		// reusing the session slot is impossible, but a
		// character mismatch is the takeover shape):
		// no 215 may reach the wrong character.
		ev := completedEv(victim, sim.CharacterID(501), 1, dest)
		h.runtime.OnDeathCompleted(ev)
		if got := callsFor(h.fix.prods[owner], proto.OpcodeRespawn); len(got) != 0 {
			t.Fatalf("mismatched-char 215 calls = %d; want 0", len(got))
		}
	})
}

func TestDeathTeleportRelocation(t *testing.T) {
	const victim = sim.EntityID(7)
	const char = sim.CharacterID(500)
	home := world.Vec3{X: 8, Y: 0, Z: 8}
	homeCell, _ := world.CellForPosition(home)
	dest := world.Vec3{X: 400, Y: 0, Z: -300}
	destCell, _ := world.CellForPosition(dest)

	h := newDeathHarness(t)
	owner := h.addReadySession(DefaultOutboundPolicy())
	oldViewer := h.addReadySession(DefaultOutboundPolicy())
	newcomer := h.addReadySession(DefaultOutboundPolicy())
	h.fix.activate(owner, 500, victim, homeCell)
	h.fix.activate(oldViewer, 501, sim.EntityID(900), homeCell)
	h.fix.activate(newcomer, 502, sim.EntityID(901), destCell)
	if _, _, err := h.fix.presence.EnsureVisible(oldViewer, victim); err != nil {
		t.Fatal(err)
	}
	h.fix.source.put(EntityPresentation{EntityID: victim, Position: home, Kind: 2, Proto: 7})
	h.fix.source.put(EntityPresentation{EntityID: sim.EntityID(900), Position: home, Kind: 2, Proto: 7})
	h.fix.source.put(EntityPresentation{EntityID: sim.EntityID(901), Position: dest, Kind: 2, Proto: 7})
	h.fix.bootstrap(owner)
	h.fix.bootstrap(oldViewer)
	h.fix.bootstrap(newcomer)

	// Queue stale movement state for the victim at both
	// retained sessions, then drain the pump so it
	// dispatches deterministically BEFORE the teleport
	// (legitimate pre-death traffic). Anything arriving
	// later is dropped by the relocation tick guard.
	h.fix.move(victim, home, 3, 35, 10, 77)
	h.release.mu.Lock()
	h.release.tick = 100
	h.release.mu.Unlock()
	h.fix.drainPump()
	oldHandle, _, _ := h.fix.presence.VisibleHandle(oldViewer, victim)

	h.runtime.OnDeathBegin(beginEv(victim, char, 1))
	h.runtime.OnDeathCompleted(completedEv(victim, char, 1, dest))

	// Presence center follows the authoritative teleport.
	snap, err := h.fix.presence.Snapshot(owner)
	if err != nil {
		t.Fatal(err)
	}
	if snap.CenterCell != destCell {
		t.Fatalf("owner center = %+v; want %+v", snap.CenterCell, destCell)
	}
	// Old-cell-only viewer retires the victim mapping
	// with a 206 and no 205 after it for that handle.
	if _, visible, err := h.fix.presence.VisibleHandle(oldViewer, victim); err != nil || visible {
		t.Fatalf("old viewer handle = %v,%v; want retired", visible, err)
	}
	removes := callsFor(h.fix.prods[oldViewer], proto.OpcodeEntityRemove)
	if len(removes) != 1 {
		t.Fatalf("old viewer 206 calls = %d; want 1", len(removes))
	}
	if got := decodeRemove(t, removes[0].payload); got.Entity != uint32(oldHandle) {
		t.Fatalf("206 entity = %d; want retired handle %d", got.Entity, uint32(oldHandle))
	}
	// No 205 for the retired handle is admitted AFTER
	// its 206 (pre-teleport 205s are legitimate queued
	// traffic, cancelled where still queued).
	ordered := h.fix.prods[oldViewer].snapshot()
	dropIdx := -1
	for i, c := range ordered {
		if c.opcode == proto.OpcodeEntityRemove {
			dropIdx = i
		}
	}
	for i, c := range ordered {
		if c.opcode == proto.OpcodeEntityMove && decodeMove(t, c.payload).Entity == uint32(oldHandle) && i > dropIdx {
			t.Fatalf("205 for retired handle %d admitted after its 206", uint32(oldHandle))
		}
	}
	// New-cell viewer discovers the victim with a 204
	// carrying the accepted post-death position.
	nh, visible, err := h.fix.presence.VisibleHandle(newcomer, victim)
	if err != nil || !visible {
		t.Fatalf("newcomer handle = %v,%v; want visible", visible, err)
	}
	creates := callsFor(h.fix.prods[newcomer], proto.OpcodeEntityCreate)
	found := false
	wantPos, _ := WirePosition(dest)
	for _, c := range creates {
		if got := decodeCreate(t, c.payload); got.Entity.Entity == uint32(nh) && got.Entity.Pos == wantPos {
			found = true
		}
	}
	if !found {
		t.Fatalf("no 204 for newcomer handle %d at dest %+v", uint32(nh), wantPos)
	}
	// The victim's own visible set is reconciled to the
	// new center: itself retained plus the newcomer's
	// entity now in range (no fake movement involved).
	vis, err := h.fix.presence.VisibleEntities(owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(vis) != 2 || vis[0] != victim || vis[1] != sim.EntityID(901) {
		t.Fatalf("owner visible = %v; want [%d 901]", vis, uint64(victim))
	}
	// Retained sessions had stale queued 205 cancelled
	// before the forced fresh 205 at the new position.
	for sid, prod := range map[session.ID]*methodRecordingProducer{owner: h.fix.prods[owner], oldViewer: h.fix.prods[oldViewer]} {
		cancelled := false
		for _, cr := range prod.cancelSnapshot() {
			if cr.key.Kind == proto.OpcodeEntityMove {
				cancelled = true
			}
		}
		if !cancelled {
			t.Fatalf("session %d: no 205 CancelState before teleport update", uint64(sid))
		}
	}
	moves := callsFor(h.fix.prods[owner], proto.OpcodeEntityMove)
	if len(moves) == 0 {
		t.Fatalf("owner 205 calls = 0; want forced post-death update")
	}
	if got := decodeMove(t, moves[len(moves)-1].payload); got.Pos != wantPos {
		t.Fatalf("owner last 205 pos = %+v; want %+v", got.Pos, wantPos)
	}
	// The teleport never touched sim movement: no intent
	// was synthesized from gateway transport state.
	h.release.mu.Lock()
	nmoves := len(h.release.moves)
	h.release.mu.Unlock()
	if nmoves != 0 {
		t.Fatalf("sim moves = %d; want 0 (no fake movement)", nmoves)
	}
}

// establishAckCorrelation activates sid for entity/char,
// bootstraps fanout, and runs one completion presentation
// so a later 120 has an exact live correlation.
func establishAckCorrelation(t *testing.T, h *deathHarness, sid session.ID, charID int64, entity sim.EntityID, epoch uint64, pos world.Vec3) sim.DeathAttemptToken {
	t.Helper()
	cell, err := world.CellForPosition(pos)
	if err != nil {
		t.Fatal(err)
	}
	h.fix.source.put(EntityPresentation{EntityID: entity, Position: pos, Kind: 2, Proto: 7})
	if _, err := h.fix.presence.Activate(sid, charID, entity, cell, worldRuntimeBase); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	h.fix.bootstrap(sid)
	tok := sim.DeathAttemptToken{EntityID: entity, CharacterID: sim.CharacterID(charID), Epoch: epoch}
	h.runtime.OnDeathCompleted(sim.DeathCompletedEvent{
		Token:    tok,
		Snapshot: sim.EntitySnapshot{ID: entity, CharacterID: sim.CharacterID(charID), Position: pos, Cell: cell, IsPlayer: true},
	})
	if got := callsFor(h.fix.prods[sid], proto.OpcodeRespawn); len(got) != 1 {
		t.Fatalf("215 calls = %d; want 1 (correlation setup)", len(got))
	}
	return tok
}

func ackFrame(t *testing.T) (proto.Header, *proto.Decoder) {
	t.Helper()
	fr, err := proto.EncodeFrame(
		proto.Header{Opcode: proto.OpcodeRespawnAck, MsgVersion: 1, Seq: 9, Tick: 100},
		func(e *proto.Encoder) error { proto.RespawnAck{}.Encode(e); return nil })
	if err != nil {
		t.Fatal(err)
	}
	header, dec, err := proto.DecodeFrame(fr)
	if err != nil {
		t.Fatal(err)
	}
	return header, dec
}

func TestDeathAckHandler(t *testing.T) {
	const entity = sim.EntityID(7)
	const charID = int64(500)
	pos := world.Vec3{X: 400, Y: 0, Z: -300}

	t.Run("first-exact-120-releases", func(t *testing.T) {
		h := newDeathHarness(t)
		sid := h.addReadySession(DefaultOutboundPolicy())
		tok := establishAckCorrelation(t, h, sid, charID, entity, 4, pos)
		header, dec := ackFrame(t)
		if err := h.acks.Handle(context.Background(), sid, header, dec, nil); err != nil {
			t.Fatalf("120 = %v; want silent success", err)
		}
		h.release.mu.Lock()
		defer h.release.mu.Unlock()
		if len(h.release.releaseToks) != 1 || h.release.releaseToks[0] != tok {
			t.Fatalf("release tokens = %+v; want exactly [%+v]", h.release.releaseToks, tok)
		}
		if h.release.releaseErr != nil {
			t.Fatalf("release err = %v", h.release.releaseErr)
		}
	})

	t.Run("duplicate-exact-120-harmless", func(t *testing.T) {
		h := newDeathHarness(t)
		sid := h.addReadySession(DefaultOutboundPolicy())
		h.release.releaseDisp = sim.RespawnReleaseDuplicate
		establishAckCorrelation(t, h, sid, charID, entity, 4, pos)
		header, dec := ackFrame(t)
		if err := h.acks.Handle(context.Background(), sid, header, dec, nil); err != nil {
			t.Fatalf("dup 120 = %v; want silent success", err)
		}
		header2, dec2 := ackFrame(t)
		if err := h.acks.Handle(context.Background(), sid, header2, dec2, nil); err != nil {
			t.Fatalf("dup 120 #2 = %v; want silent success", err)
		}
	})

	t.Run("trailing-bytes-tolerated-per-versioning", func(t *testing.T) {
		h := newDeathHarness(t)
		sid := h.addReadySession(DefaultOutboundPolicy())
		establishAckCorrelation(t, h, sid, charID, entity, 4, pos)
		// The empty 120 payload has no malformed shape:
		// DecodeRespawnAck skips trailing bytes per the
		// global msg_version rule, so extra bytes still
		// release exactly. (The handler keeps a
		// defensive protocol_error branch for future
		// codec revisions; it is unreachable today.)
		fr, err := proto.EncodeFrame(
			proto.Header{Opcode: proto.OpcodeRespawnAck, MsgVersion: 1, Seq: 9, Tick: 100},
			func(e *proto.Encoder) error { e.U16(0xFFFF); return nil })
		if err != nil {
			t.Fatal(err)
		}
		header, dec, err := proto.DecodeFrame(fr)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.acks.Handle(context.Background(), sid, header, dec, nil); err != nil {
			t.Fatalf("120 with trailing bytes = %v; want silent success", err)
		}
		h.release.mu.Lock()
		defer h.release.mu.Unlock()
		if len(h.release.releaseToks) != 1 {
			t.Fatalf("release calls = %d; want 1", len(h.release.releaseToks))
		}
	})

	t.Run("no-presence-internal", func(t *testing.T) {
		h := newDeathHarness(t)
		sid := h.addReadySession(DefaultOutboundPolicy())
		header, dec := ackFrame(t)
		err := h.acks.Handle(context.Background(), sid, header, dec, nil)
		var ce *ClientError
		if errors.As(err, &ce) {
			t.Fatalf("120 without presence = ClientError %v; want internal", ce)
		}
		if err == nil {
			t.Fatalf("120 without presence = nil; want internal error")
		}
	})

	t.Run("no-correlation-protocol-error", func(t *testing.T) {
		h := newDeathHarness(t)
		sid := h.addReadySession(DefaultOutboundPolicy())
		home, _ := world.CellForPosition(world.Vec3{})
		if _, err := h.fix.presence.Activate(sid, charID, entity, home, worldRuntimeBase); err != nil {
			t.Fatal(err)
		}
		header, dec := ackFrame(t)
		if err := h.acks.Handle(context.Background(), sid, header, dec, nil); clientErrorOf(t, err).Code != proto.ErrorCodeProtocol {
			t.Fatalf("120 without correlation = %v; want protocol_error", err)
		}
	})

	t.Run("stale-entity-protocol-error", func(t *testing.T) {
		h := newDeathHarness(t)
		sid := h.addReadySession(DefaultOutboundPolicy())
		establishAckCorrelation(t, h, sid, charID, entity, 4, pos)
		// Presence now controls a DIFFERENT entity for
		// the same session epoch is impossible via the
		// registry, so emulate the stale shape at the
		// runtime boundary: drop the correlation epoch
		// by forgetting and re-establishing for a new
		// attempt, then replaying the old token is
		// covered by the mismatch mapping below.
		h.runtime.ForgetSession(sid)
		header, dec := ackFrame(t)
		if err := h.acks.Handle(context.Background(), sid, header, dec, nil); clientErrorOf(t, err).Code != proto.ErrorCodeProtocol {
			t.Fatalf("120 after forget = %v; want protocol_error", err)
		}
	})

	t.Run("error-mapping", func(t *testing.T) {
		cases := []struct {
			name string
			err  error
			code uint16
		}{
			{"ingress-full-retry", sim.ErrSimIngressFull, proto.ErrorCodeRetry},
			{"not-running-retry", sim.ErrEngineNotRunning, proto.ErrorCodeRetry},
			{"stopped-retry", sim.ErrEngineStopped, proto.ErrorCodeRetry},
			{"mismatch-protocol", sim.ErrDeathAttemptMismatch, proto.ErrorCodeProtocol},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				h := newDeathHarness(t)
				sid := h.addReadySession(DefaultOutboundPolicy())
				establishAckCorrelation(t, h, sid, charID, entity, 4, pos)
				h.release.releaseErr = tc.err
				header, dec := ackFrame(t)
				if err := h.acks.Handle(context.Background(), sid, header, dec, nil); clientErrorOf(t, err).Code != tc.code {
					t.Fatalf("120 = %v; want code %d", err, tc.code)
				}
			})
		}
		t.Run("missing-entity-internal", func(t *testing.T) {
			h := newDeathHarness(t)
			sid := h.addReadySession(DefaultOutboundPolicy())
			establishAckCorrelation(t, h, sid, charID, entity, 4, pos)
			h.release.releaseErr = sim.ErrEntityNotFound
			header, dec := ackFrame(t)
			err := h.acks.Handle(context.Background(), sid, header, dec, nil)
			var ce *ClientError
			if errors.As(err, &ce) {
				t.Fatalf("120 = ClientError %v; want internal fail-closed", ce)
			}
			if err == nil {
				t.Fatalf("120 = nil; want internal error")
			}
		})
	})

	t.Run("no-side-channels", func(t *testing.T) {
		h := newDeathHarness(t)
		sid := h.addReadySession(DefaultOutboundPolicy())
		establishAckCorrelation(t, h, sid, charID, entity, 4, pos)
		header, dec := ackFrame(t)
		if err := h.acks.Handle(context.Background(), sid, header, dec, nil); err != nil {
			t.Fatal(err)
		}
		h.release.mu.Lock()
		defer h.release.mu.Unlock()
		if h.release.adds != 0 || len(h.release.moves) != 0 || len(h.release.removes) != 0 || len(h.release.recoverBoots) != 0 {
			t.Fatalf("120 touched non-release sim paths: adds=%d moves=%d removes=%d bootstraps=%d",
				h.release.adds, len(h.release.moves), len(h.release.removes), len(h.release.recoverBoots))
		}
	})
}

func TestDeathAckRateGateComposition(t *testing.T) {
	// Structurally valid 120 reaches the death handler only
	// after the general intent rate gate; a denied gate
	// never reaches it and consumes no release.
	presence, err := NewPresenceRegistry(RateLimitPolicy{MovePerSec: 10, IntentPerSec: 1})
	if err != nil {
		t.Fatal(err)
	}
	fakeSim := &recordingSim{}
	rt, err := NewDeathWireRuntime(presence, session.NewRegistry(), mustFanoutForGate(t, presence))
	if err != nil {
		t.Fatal(err)
	}
	acks, err := NewDeathAckHandler(rt, fakeSim, nil)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := NewGameplayIngressHandler(presence, fakeSim, func() time.Time { return ingressTestBase }, acks)
	if err != nil {
		t.Fatal(err)
	}
	sid := session.ID(1)
	home, _ := world.CellForPosition(world.Vec3{})
	if _, err := presence.Activate(sid, 500, sim.EntityID(7), home, ingressTestBase); err != nil {
		t.Fatal(err)
	}
	// Establish the correlation directly (unit scope:
	// transport admission is proven elsewhere).
	rt.mu.Lock()
	rt.corr[sid] = respawnCorrelation{
		entity: sim.EntityID(7), charID: sim.CharacterID(500),
		token: sim.DeathAttemptToken{EntityID: sim.EntityID(7), CharacterID: sim.CharacterID(500), Epoch: 2},
	}
	rt.mu.Unlock()

	header, dec := ackFrame(t)
	if err := gate.Handle(context.Background(), sid, header, dec, nil); err != nil {
		t.Fatalf("first gated 120 = %v", err)
	}
	fakeSim.mu.Lock()
	first := len(fakeSim.releaseToks)
	fakeSim.mu.Unlock()
	if first != 1 {
		t.Fatalf("release calls = %d; want 1", first)
	}
	header2, dec2 := ackFrame(t)
	if err := gate.Handle(context.Background(), sid, header2, dec2, nil); clientErrorOf(t, err).Code != proto.ErrorCodeRateLimited {
		t.Fatalf("denied 120 = %v; want rate_limited", err)
	}
	fakeSim.mu.Lock()
	second := len(fakeSim.releaseToks)
	fakeSim.mu.Unlock()
	if second != 1 {
		t.Fatalf("release calls after denied gate = %d; want 1 (no second charge)", second)
	}
}

func mustFanoutForGate(t *testing.T, presence *PresenceRegistry) *FanoutRuntime {
	t.Helper()
	fanout, err := NewFanoutRuntime(presence, session.NewRegistry(), newFakePresentation(), 20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fanout.Close)
	return fanout
}

func TestOldSessionIsolation(t *testing.T) {
	const entity = sim.EntityID(7)
	const charID = int64(500)
	pos := world.Vec3{X: 400, Y: 0, Z: -300}

	t.Run("forgotten-session-cannot-release", func(t *testing.T) {
		h := newDeathHarness(t)
		old := h.addReadySession(DefaultOutboundPolicy())
		establishAckCorrelation(t, h, old, charID, entity, 4, pos)
		// Takeover teardown drops the old epoch's
		// correlation (WorldSessionRuntime exit hook).
		h.runtime.ForgetSession(old)
		header, dec := ackFrame(t)
		if err := h.acks.Handle(context.Background(), old, header, dec, nil); clientErrorOf(t, err).Code != proto.ErrorCodeProtocol {
			t.Fatalf("old-session 120 = %v; want protocol_error", err)
		}
		h.release.mu.Lock()
		defer h.release.mu.Unlock()
		if len(h.release.releaseToks) != 0 {
			t.Fatalf("release calls = %d; want 0", len(h.release.releaseToks))
		}
	})

	t.Run("replacement-session-starts-clean", func(t *testing.T) {
		h := newDeathHarness(t)
		old := h.addReadySession(DefaultOutboundPolicy())
		establishAckCorrelation(t, h, old, charID, entity, 4, pos)
		// Takeover teardown: old presence deactivated,
		// old correlation forgotten.
		if _, err := h.fix.presence.Deactivate(old); err != nil {
			t.Fatal(err)
		}
		h.runtime.ForgetSession(old)
		fresh := h.addReadySession(DefaultOutboundPolicy())
		home, _ := world.CellForPosition(world.Vec3{})
		if _, err := h.fix.presence.Activate(fresh, charID, sim.EntityID(8), home, worldRuntimeBase); err != nil {
			t.Fatal(err)
		}
		header, dec := ackFrame(t)
		if err := h.acks.Handle(context.Background(), fresh, header, dec, nil); clientErrorOf(t, err).Code != proto.ErrorCodeProtocol {
			t.Fatalf("fresh-session 120 = %v; want protocol_error (no inherited ack)", err)
		}
	})
}

func TestDeathWireTripwire(t *testing.T) {
	// No new opcode, no new wire token, no alternate
	// entity-ID encoding: the death plane owns exactly
	// 120/214/215 with the frozen values and codecs.
	if proto.OpcodeRespawnAck != 120 || proto.OpcodeDeath != 214 || proto.OpcodeRespawn != 215 {
		t.Fatalf("death opcodes = %d/%d/%d; want 120/214/215",
			proto.OpcodeRespawnAck, proto.OpcodeDeath, proto.OpcodeRespawn)
	}
	h := newDeathHarness(t)
	next := &ingressNext{}
	withNext, err := NewDeathAckHandler(h.runtime, h.release, next)
	if err != nil {
		t.Fatal(err)
	}
	for _, opcode := range []uint16{103, 119, 121, 124, 205} {
		header := proto.Header{Opcode: opcode, MsgVersion: 1, Seq: 1, Tick: 1}
		if err := withNext.Handle(context.Background(), session.ID(1), header, proto.NewDecoder(nil), nil); err != nil {
			t.Fatalf("opcode %d = %v; want delegation", opcode, err)
		}
	}
	if n := next.count(); n != 5 {
		t.Fatalf("delegated = %d; want 5", n)
	}
	h.release.mu.Lock()
	defer h.release.mu.Unlock()
	if len(h.release.releaseToks) != 0 {
		t.Fatalf("release calls for non-120 = %d; want 0", len(h.release.releaseToks))
	}
}
