package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dlukt/voxilian/internal/proto"
	"github.com/dlukt/voxilian/internal/session"
	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/world"
)

// M5-T5c4 end-to-end proof (spec §9.5.1l): one complete
// in-memory gateway/sim composition proving live IN_WORLD
// player -> authoritative death begin -> 214 with
// recipient-local victim handle -> persistence completion
// accepted -> post-death teleport / AOI reconcile -> 215
// authoritative position -> client 120 ->
// AwaitingRespawn -> Alive with pending preserved, then
// proving 120 is NOT Underworld LeaveHold, then proving
// fresh reconnect recovery without replaying wire or ack
// state.

// deathE2EEnv is the full T5c4 composition over a real
// engine: the engine names the DeathWireRuntime as its
// Death sink, and the 120 path runs through the real
// GameplayIngressHandler rate gate into DeathAckHandler.
type deathE2EEnv struct {
	t        *testing.T
	reg      *session.Registry
	presence *PresenceRegistry
	source   *fakePresentation
	fanout   *FanoutRuntime
	runtime  *DeathWireRuntime
	engine   *sim.Engine
	prods    map[session.ID]*methodRecordingProducer
}

func newDeathE2EEnv(t *testing.T) *deathE2EEnv {
	t.Helper()
	reg := session.NewRegistry()
	presence, err := NewPresenceRegistry(testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	source := newFakePresentation()
	fanout, err := NewFanoutRuntime(presence, reg, source, 20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fanout.Close)
	runtime, err := NewDeathWireRuntime(presence, reg, fanout)
	if err != nil {
		t.Fatal(err)
	}
	clk := new(gwTestClock)
	engine, err := sim.NewEngine(sim.EngineConfig{TickHz: 20}, sim.EngineDeps{
		Clock: clk, RNG: &gwTestRNG{}, Collision: gwOpenCollision{},
		RunGate: gwStaticGate{allow: true}, Death: runtime,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &deathE2EEnv{
		t: t, reg: reg, presence: presence, source: source,
		fanout: fanout, runtime: runtime, engine: engine,
		prods: make(map[session.ID]*methodRecordingProducer),
	}
}

func (e *deathE2EEnv) addSession() session.ID {
	e.t.Helper()
	tr := newFakeOutTransport()
	ob := &recordingObserver{}
	oc := newOutboundConn(OutboundDeps{
		Conn: tr, Registry: e.reg,
		Tick:     func() uint32 { return 1000 },
		Policy:   DefaultOutboundPolicy(),
		Observer: ob,
	})
	rec := &methodRecordingProducer{OutboundProducer: oc, t: e.t}
	sid := e.reg.Create(rec)
	e.prods[sid] = rec
	return sid
}

func (e *deathE2EEnv) activate(sid session.ID, charID int64, entity sim.EntityID, center world.CellCoord) {
	e.t.Helper()
	if _, err := e.presence.Activate(sid, charID, entity, center, worldRuntimeBase); err != nil {
		e.t.Fatalf("Activate: %v", err)
	}
	if err := e.fanout.BootstrapSession(context.Background(), sid); err != nil {
		e.t.Fatalf("BootstrapSession(%d): %v", uint64(sid), err)
	}
}

// putLivePresentation mirrors the world layer's hot
// presentation bookkeeping for a live sim entity: the
// production M10 source tracks authoritative positions;
// tests publish them explicitly from engine inspection.
func (e *deathE2EEnv) putLivePresentation(id sim.EntityID) {
	e.t.Helper()
	snap, err := e.engine.Entity(id)
	if err != nil {
		e.t.Fatalf("Entity(%d): %v", uint64(id), err)
	}
	e.source.put(EntityPresentation{EntityID: id, Position: snap.Position, Kind: 2, Proto: 7})
}

// e2eVitals returns zero-HP vitals (death begin
// precondition) and full post-death vitals.
func e2eVitals(t *testing.T) (zero, full sim.PlayerVitals) {
	t.Helper()
	full, err := sim.NewPlayerVitals(25)
	if err != nil {
		t.Fatal(err)
	}
	zero = full
	zero.HP = 0
	if err := zero.Validate(); err != nil {
		t.Fatal(err)
	}
	return zero, full
}

func e2eInputs() sim.PlayerVitalsRuntimeInputs {
	return sim.PlayerVitalsRuntimeInputs{
		EffectiveStamina: 25, EffectiveMysticism: 25, RestRecoveryMultiplier: 1,
	}
}

func e2eDurable() sim.PlayerDurableState {
	return sim.PlayerDurableState{Karma: 10, Advancement: []byte(`{}`), Flags: 2}
}

// e2eIngress wraps the engine's SimIngress seam so the
// hot presentation source mirrors player recovery staging
// exactly at the world-entry boundary (the production M10
// world layer's own bookkeeping).
type e2eIngress struct {
	SimIngress
	env *deathE2EEnv
}

func (w *e2eIngress) EnqueueAddPlayerEntityWithRecovery(ctx context.Context, boot sim.PlayerRecoveryBootstrap) (sim.EntitySnapshot, error) {
	snap, err := w.SimIngress.EnqueueAddPlayerEntityWithRecovery(ctx, boot)
	if err == nil {
		w.env.source.put(EntityPresentation{EntityID: snap.ID, Position: snap.Position, Kind: 2, Proto: 7})
	}
	return snap, err
}

func TestDeathWireE2E(t *testing.T) {
	const charID = int64(500)
	start := world.Vec3{X: 8, Y: 0, Z: 8}
	startCell, _ := world.CellForPosition(start)
	dest := world.Vec3{X: 400, Y: 0, Z: -300}

	env := newDeathE2EEnv(t)
	owner := env.addSession()
	viewer := env.addSession()

	zero, full := e2eVitals(t)
	// Step-driven setup while no Run owns the engine: a
	// live zero-HP player entity.
	added, err := env.engine.AddPlayerEntity(sim.CharacterID(charID), start, zero, e2eInputs())
	if err != nil {
		t.Fatalf("AddPlayerEntity: %v", err)
	}
	victim := added.ID
	env.source.put(EntityPresentation{EntityID: victim, Position: start, Kind: 2, Proto: 7})
	viewerEnt, err := env.engine.AddEntity(world.Vec3{X: 1300, Y: 0, Z: 1300})
	if err != nil {
		t.Fatal(err)
	}
	env.source.put(EntityPresentation{EntityID: viewerEnt.ID, Position: viewerEnt.Position, Kind: 2, Proto: 7})
	env.activate(owner, charID, victim, startCell)
	viewerCell, _ := world.CellForPosition(viewerEnt.Position)
	env.activate(viewer, 501, viewerEnt.ID, viewerCell)
	if _, _, err := env.presence.EnsureVisible(viewer, victim); err != nil {
		t.Fatal(err)
	}

	// Authoritative death begin: exactly one 214 per
	// session, each with its recipient-local handle.
	tok, err := env.engine.PlayerBeginDeathPersistence(victim)
	if err != nil {
		t.Fatalf("PlayerBeginDeathPersistence: %v", err)
	}
	own214 := callsFor(env.prods[owner], proto.OpcodeDeath)
	if len(own214) != 1 || decodeDeath(t, own214[0].payload).Victim != 1 {
		t.Fatalf("owner 214 = %d calls; want exactly one with victim handle 1", len(own214))
	}
	view214 := callsFor(env.prods[viewer], proto.OpcodeDeath)
	wh, _, _ := env.presence.VisibleHandle(viewer, victim)
	if len(view214) != 1 || decodeDeath(t, view214[0].payload).Victim != uint32(wh) {
		t.Fatalf("viewer 214 = %d calls; want exactly one with local handle %d", len(view214), uint32(wh))
	}

	// Persistence completion accepted by the owner
	// (Underworld-bound: pending survives): post-death
	// teleport reconciles AOI and exactly one 215
	// reaches only the controlling session.
	cancel, done := runSimOwner(t, env.engine)
	defer stopSimOwner(t, cancel, done)
	corpse := int64(77)
	comp := sim.ImmediateDeathCompletion{
		Token:         tok,
		Placement:     dest,
		Vitals:        full,
		RuntimeInputs: e2eInputs(),
		Durable:       e2eDurable(),
		Pending: &sim.PendingDeathRuntime{
			EffectiveCost: 40, DeathTimeSeconds: 1000, CorpseID: &corpse, PortalUsed: false,
		},
	}
	snap, disp, err := env.engine.EnqueueImmediateDeathCompletion(context.Background(), comp)
	if err != nil || disp != sim.DeathCompletionApplied {
		t.Fatalf("completion = %v,%v; want Applied,nil", disp, err)
	}
	if snap.Position != dest {
		t.Fatalf("accepted pos = %+v; want %+v", snap.Position, dest)
	}
	own215 := callsFor(env.prods[owner], proto.OpcodeRespawn)
	if len(own215) != 1 {
		t.Fatalf("owner 215 calls = %d; want 1", len(own215))
	}
	wantPos, _ := WirePosition(dest)
	if got := decodeRespawn(t, own215[0].payload); got.Pos != wantPos {
		t.Fatalf("215 pos = %+v; want %+v", got.Pos, wantPos)
	}
	if got := callsFor(env.prods[viewer], proto.OpcodeRespawn); len(got) != 0 {
		t.Fatalf("viewer 215 calls = %d; want 0", len(got))
	}
	// 214 precedes 215 on the victim session.
	var order []uint16
	for _, c := range env.prods[owner].snapshot() {
		if c.opcode == proto.OpcodeDeath || c.opcode == proto.OpcodeRespawn {
			order = append(order, c.opcode)
		}
	}
	if len(order) != 2 || order[0] != proto.OpcodeDeath || order[1] != proto.OpcodeRespawn {
		t.Fatalf("victim order = %v; want [214 215]", order)
	}
	// Presence center followed the teleport.
	if psnap, err := env.presence.Snapshot(owner); err != nil || psnap.CenterCell != snap.Cell {
		t.Fatalf("owner center = %+v,%v; want %v", psnap.CenterCell, err, snap.Cell)
	}

	// Client 120 through the real rate gate: silent
	// success, AwaitingRespawn -> Alive, pending
	// bit-identical (Underworld-bound death stays
	// pending in ordinary Underworld gameplay).
	acks, err := NewDeathAckHandler(env.runtime, env.engine, nil)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := NewGameplayIngressHandler(env.presence, env.engine, func() time.Time { return worldRuntimeBase }, acks)
	if err != nil {
		t.Fatal(err)
	}
	header, dec := ackFrame(t)
	if err := gate.Handle(context.Background(), owner, header, dec, nil); err != nil {
		t.Fatalf("120 = %v; want silent success", err)
	}
	if st, _, err := env.engine.PlayerLifeStateOf(victim); err != nil || st != sim.PlayerLifeAlive {
		t.Fatalf("life = %d,%v; want Alive,nil", uint8(st), err)
	}
	gotPending, ok, err := env.engine.PlayerPendingDeathOf(victim)
	if err != nil || !ok {
		t.Fatalf("pending = %+v,%v,%v; want present", gotPending, ok, err)
	}
	if gotPending.EffectiveCost != 40 || gotPending.DeathTimeSeconds != 1000 ||
		gotPending.CorpseID == nil || *gotPending.CorpseID != 77 || gotPending.PortalUsed {
		t.Fatalf("pending mutated by 120: %+v", gotPending)
	}
	// 120 is NOT Underworld LeaveHold: no penalty
	// quiesce happened and vitals are exactly the
	// installed post-death values (a penalty would
	// have consumed pending and altered MaxHP).
	gotVitals, isPlayer, err := env.engine.PlayerVitalsOf(victim)
	if err != nil || !isPlayer || gotVitals != full {
		t.Fatalf("vitals = %+v,%v,%v; want installed %+v", gotVitals, isPlayer, err, full)
	}
	// Exact duplicate 120 is silent idempotent success.
	header2, dec2 := ackFrame(t)
	if err := gate.Handle(context.Background(), owner, header2, dec2, nil); err != nil {
		t.Fatalf("dup 120 = %v; want silent success", err)
	}
	if st, _, _ := env.engine.PlayerLifeStateOf(victim); st != sim.PlayerLifeAlive {
		t.Fatalf("life after dup = %d; want Alive", uint8(st))
	}
}

func TestDeathReconnectE2E(t *testing.T) {
	const charID = int64(500)
	const acctID = int64(42)
	dest := world.Vec3{X: 400, Y: 0, Z: -300}

	env := newDeathE2EEnv(t)
	old := env.addSession()
	_, full := e2eVitals(t)
	added, err := env.engine.AddPlayerEntity(sim.CharacterID(charID), dest, full, e2eInputs())
	if err != nil {
		t.Fatal(err)
	}
	oldEntity := added.ID
	cell, _ := world.CellForPosition(dest)
	env.source.put(EntityPresentation{EntityID: oldEntity, Position: dest, Kind: 2, Proto: 7})
	env.activate(old, charID, oldEntity, cell)

	// Disconnect before any 120: the live entity is
	// removed, presence deactivated, correlation
	// forgotten. No durable ack debt may remain.
	cancel, done := runSimOwner(t, env.engine)
	defer stopSimOwner(t, cancel, done)
	if err := env.engine.EnqueueRemoveEntity(context.Background(), oldEntity); err != nil {
		t.Fatal(err)
	}
	if err := env.fanout.RemovePresence(context.Background(), old, oldEntity); err != nil {
		t.Fatal(err)
	}
	if _, err := env.presence.Deactivate(old); err != nil {
		t.Fatal(err)
	}
	env.runtime.ForgetSession(old)

	// Fresh session, fresh enter_world from authoritative
	// materialized state carrying a pending death.
	fresh := env.addSession()
	corpse := int64(78)
	boot := sim.PlayerRecoveryBootstrap{
		CharacterID:   sim.CharacterID(charID),
		Position:      dest,
		Vitals:        full,
		RuntimeInputs: e2eInputs(),
		Durable:       e2eDurable(),
		Pending: &sim.PendingDeathRuntime{
			EffectiveCost: 35, DeathTimeSeconds: 2000, CorpseID: &corpse, PortalUsed: true,
		},
	}
	loader := PlayerBootstrapLoaderFunc(func(_ context.Context, id int64) (sim.PlayerRecoveryBootstrap, error) {
		if id != charID {
			t.Errorf("loader character = %d; want %d", id, charID)
		}
		return boot, nil
	})
	spawn := SpawnResolverFunc(func(context.Context, int64, int64) (world.Vec3, error) {
		t.Error("spawn resolver consulted on bootstrap path")
		return world.Vec3{}, nil
	})
	rt, err := NewWorldSessionRuntime(&e2eIngress{SimIngress: env.engine, env: env}, env.presence, env.reg, spawn,
		func() time.Time { return worldRuntimeBase }, &recordingDownstream{}, env.fanout)
	if err != nil {
		t.Fatal(err)
	}
	rt.SetPlayerBootstrapLoader(loader)
	rt.SetRespawnForgetter(env.runtime.ForgetSession)
	if err := rt.PrepareEnter(context.Background(), fresh, acctID, charID); err != nil {
		t.Fatalf("PrepareEnter: %v", err)
	}
	if err := rt.CommitEnter(context.Background(), fresh); err != nil {
		t.Fatalf("CommitEnter: %v", err)
	}
	psnap, err := env.presence.Snapshot(fresh)
	if err != nil {
		t.Fatalf("fresh presence: %v", err)
	}
	newEntity := psnap.EntityID
	if newEntity == oldEntity {
		t.Fatalf("fresh entity = old entity %d; want new EntityID (no ABA)", uint64(oldEntity))
	}
	// Fresh player is ordinary Alive gameplay with the
	// exact pending hydrated — no 120 required to
	// activate the connection.
	if st, _, err := env.engine.PlayerLifeStateOf(newEntity); err != nil || st != sim.PlayerLifeAlive {
		t.Fatalf("fresh life = %d,%v; want Alive,nil", uint8(st), err)
	}
	gotPending, ok, err := env.engine.PlayerPendingDeathOf(newEntity)
	if err != nil || !ok {
		t.Fatalf("fresh pending = %+v,%v,%v; want present", gotPending, ok, err)
	}
	if gotPending.EffectiveCost != 35 || gotPending.DeathTimeSeconds != 2000 ||
		gotPending.CorpseID == nil || *gotPending.CorpseID != 78 || !gotPending.PortalUsed {
		t.Fatalf("fresh pending wrong: %+v", gotPending)
	}
	// No old wire replayed on the fresh session, and no
	// old ack state inherited.
	for _, opcode := range []uint16{proto.OpcodeDeath, proto.OpcodeRespawn} {
		if got := callsFor(env.prods[fresh], opcode); len(got) != 0 {
			t.Fatalf("fresh session opcode %d calls = %d; want 0 (no replay)", opcode, len(got))
		}
	}
	acks, err := NewDeathAckHandler(env.runtime, env.engine, nil)
	if err != nil {
		t.Fatal(err)
	}
	header, dec := ackFrame(t)
	if err := acks.Handle(context.Background(), fresh, header, dec, nil); clientErrorOf(t, err).Code != proto.ErrorCodeProtocol {
		t.Fatalf("fresh 120 = %v; want protocol_error (no inherited ack)", err)
	}
	// The fresh player is immediately usable gameplay
	// (movement control accepted on the new entity).
	if _, err := env.engine.EnqueueMove(context.Background(), newEntity, sim.MoveIntent{InputSeq: 1, Yaw: 100}); err != nil {
		t.Fatalf("fresh move = %v; want accepted", err)
	}
}

func TestDeathReconnectNoPendingE2E(t *testing.T) {
	// Reconnect variant with Pending == nil (direct
	// newbie-home death or post-penalty state): the
	// fresh player enters as ordinary Alive gameplay
	// with no pending hydrated and no invented row.
	const charID = int64(501)
	const acctID = int64(43)
	dest := world.Vec3{X: -200, Y: 0, Z: 150}

	env := newDeathE2EEnv(t)
	cancel, done := runSimOwner(t, env.engine)
	defer stopSimOwner(t, cancel, done)

	fresh := env.addSession()
	_, full := e2eVitals(t)
	boot := sim.PlayerRecoveryBootstrap{
		CharacterID:   sim.CharacterID(charID),
		Position:      dest,
		Vitals:        full,
		RuntimeInputs: e2eInputs(),
		Durable:       e2eDurable(),
		Pending:       nil,
	}
	rt, err := NewWorldSessionRuntime(&e2eIngress{SimIngress: env.engine, env: env}, env.presence, env.reg,
		SpawnResolverFunc(func(context.Context, int64, int64) (world.Vec3, error) { return world.Vec3{}, nil }),
		func() time.Time { return worldRuntimeBase }, &recordingDownstream{}, env.fanout)
	if err != nil {
		t.Fatal(err)
	}
	rt.SetPlayerBootstrapLoader(PlayerBootstrapLoaderFunc(
		func(context.Context, int64) (sim.PlayerRecoveryBootstrap, error) { return boot, nil }))
	if err := rt.PrepareEnter(context.Background(), fresh, acctID, charID); err != nil {
		t.Fatalf("PrepareEnter: %v", err)
	}
	if err := rt.CommitEnter(context.Background(), fresh); err != nil {
		t.Fatalf("CommitEnter: %v", err)
	}
	psnap, err := env.presence.Snapshot(fresh)
	if err != nil {
		t.Fatalf("fresh presence: %v", err)
	}
	if st, _, err := env.engine.PlayerLifeStateOf(psnap.EntityID); err != nil || st != sim.PlayerLifeAlive {
		t.Fatalf("fresh life = %d,%v; want Alive,nil", uint8(st), err)
	}
	if _, ok, err := env.engine.PlayerPendingDeathOf(psnap.EntityID); err != nil || ok {
		t.Fatalf("fresh pending present = %v,%v; want false,nil", ok, err)
	}
	for _, opcode := range []uint16{proto.OpcodeDeath, proto.OpcodeRespawn} {
		if got := callsFor(env.prods[fresh], opcode); len(got) != 0 {
			t.Fatalf("fresh session opcode %d calls = %d; want 0 (no replay)", opcode, len(got))
		}
	}
}

func testBootstrap(charID int64, pos world.Vec3, pending *sim.PendingDeathRuntime) sim.PlayerRecoveryBootstrap {
	full, err := sim.NewPlayerVitals(25)
	if err != nil {
		panic(err)
	}
	return sim.PlayerRecoveryBootstrap{
		CharacterID:   sim.CharacterID(charID),
		Position:      pos,
		Vitals:        full,
		RuntimeInputs: e2eInputs(),
		Durable:       e2eDurable(),
		Pending:       pending,
	}
}

func TestWorldEnterBootstrapPath(t *testing.T) {
	const charID = int64(500)
	pos := world.Vec3{X: 400, Y: 0, Z: -300}

	setup := func(t *testing.T) (*WorldSessionRuntime, *recordingSim, *PresenceRegistry) {
		t.Helper()
		fakeSim := &recordingSim{}
		rt, presence, _, _ := testRuntime(t, fakeSim, worldRuntimeBase)
		return rt, fakeSim, presence
	}

	t.Run("bootstrap-stages-player", func(t *testing.T) {
		rt, fakeSim, presence := setup(t)
		want := testBootstrap(charID, pos, nil)
		rt.SetPlayerBootstrapLoader(PlayerBootstrapLoaderFunc(
			func(_ context.Context, id int64) (sim.PlayerRecoveryBootstrap, error) {
				if id != charID {
					t.Errorf("loader character = %d", id)
				}
				return want, nil
			}))
		if err := rt.PrepareEnter(context.Background(), 1, 11, charID); err != nil {
			t.Fatalf("PrepareEnter: %v", err)
		}
		fakeSim.mu.Lock()
		defer fakeSim.mu.Unlock()
		if len(fakeSim.recoverBoots) != 1 {
			t.Fatalf("bootstrap calls = %d; want 1", len(fakeSim.recoverBoots))
		}
		if fakeSim.adds != 0 {
			t.Fatalf("generic adds = %d; want 0 (player path, no spawn)", fakeSim.adds)
		}
		if _, err := presence.Snapshot(1); !errors.Is(err, ErrPresenceNotFound) {
			t.Fatalf("presence exists before commit: %v", err)
		}
	})

	t.Run("loader-failure-retryable", func(t *testing.T) {
		rt, fakeSim, _ := setup(t)
		rt.SetPlayerBootstrapLoader(PlayerBootstrapLoaderFunc(
			func(context.Context, int64) (sim.PlayerRecoveryBootstrap, error) {
				return sim.PlayerRecoveryBootstrap{}, errors.New("pg down")
			}))
		if err := rt.PrepareEnter(context.Background(), 1, 11, charID); !errors.Is(err, ErrWorldEntryRetry) {
			t.Fatalf("PrepareEnter = %v; want ErrWorldEntryRetry", err)
		}
		fakeSim.mu.Lock()
		defer fakeSim.mu.Unlock()
		if len(fakeSim.recoverBoots) != 0 {
			t.Fatalf("bootstrap staged despite loader failure")
		}
		if err := rt.AbortEnter(context.Background(), 1); err != nil {
			t.Fatalf("AbortEnter after failed prepare = %v; want nil", err)
		}
	})

	t.Run("character-mismatch-invalid", func(t *testing.T) {
		rt, _, _ := setup(t)
		rt.SetPlayerBootstrapLoader(PlayerBootstrapLoaderFunc(
			func(context.Context, int64) (sim.PlayerRecoveryBootstrap, error) {
				return testBootstrap(charID+1, pos, nil), nil
			}))
		if err := rt.PrepareEnter(context.Background(), 1, 11, charID); !errors.Is(err, ErrWorldSpawnInvalid) {
			t.Fatalf("PrepareEnter = %v; want ErrWorldSpawnInvalid", err)
		}
	})

	t.Run("ingress-saturation-retryable", func(t *testing.T) {
		rt, _, _ := setup(t)
		rt.SetPlayerBootstrapLoader(PlayerBootstrapLoaderFunc(
			func(context.Context, int64) (sim.PlayerRecoveryBootstrap, error) {
				return testBootstrap(charID, pos, nil), nil
			}))
		// recordingSim surfaces recoverErr through the
		// recovery ingress; saturation must map to
		// retry with no staged entry left behind.
		rt2fake := &recordingSim{recoverErr: sim.ErrSimIngressFull}
		rt2, _, _, _ := testRuntime(t, rt2fake, worldRuntimeBase)
		rt2.SetPlayerBootstrapLoader(PlayerBootstrapLoaderFunc(
			func(context.Context, int64) (sim.PlayerRecoveryBootstrap, error) {
				return testBootstrap(charID, pos, nil), nil
			}))
		_ = rt
		if err := rt2.PrepareEnter(context.Background(), 1, 11, charID); !errors.Is(err, ErrWorldEntryRetry) {
			t.Fatalf("PrepareEnter = %v; want ErrWorldEntryRetry", err)
		}
	})

	t.Run("nil-loader-legacy-path", func(t *testing.T) {
		rt, fakeSim, _ := setup(t)
		if err := rt.PrepareEnter(context.Background(), 1, 11, charID); err != nil {
			t.Fatalf("PrepareEnter: %v", err)
		}
		fakeSim.mu.Lock()
		defer fakeSim.mu.Unlock()
		if fakeSim.adds != 1 || len(fakeSim.recoverBoots) != 0 {
			t.Fatalf("adds = %d bootstraps = %d; want 1/0 (legacy spawn path)",
				fakeSim.adds, len(fakeSim.recoverBoots))
		}
	})

	t.Run("exit-forgets-respawn", func(t *testing.T) {
		rt, _, presence := setup(t)
		var forgot []session.ID
		rt.SetRespawnForgetter(func(sid session.ID) { forgot = append(forgot, sid) })
		home, _ := world.CellForPosition(world.Vec3{})
		if _, err := presence.Activate(1, charID, sim.EntityID(7), home, worldRuntimeBase); err != nil {
			t.Fatal(err)
		}
		if err := rt.ExitWorld(context.Background(), 1, 11, charID); err != nil {
			t.Fatalf("ExitWorld: %v", err)
		}
		if len(forgot) != 1 || forgot[0] != session.ID(1) {
			t.Fatalf("forgotten = %v; want [1]", forgot)
		}
	})
}

func TestDeathCriticalSaturation(t *testing.T) {
	const victim = sim.EntityID(7)
	const char = sim.CharacterID(500)
	dest := world.Vec3{X: 400, Y: 0, Z: -300}

	h := newDeathHarness(t)
	// The owner session is created over a toggleable
	// producer (saturable mid-test) held by the
	// registry — which the runtime resolves producers
	// through. The viewer is an ordinary healthy
	// session.
	tr := newFakeOutTransport()
	oc := newOutboundConn(OutboundDeps{
		Conn: tr, Registry: h.fix.reg,
		Tick:     func() uint32 { return 1000 },
		Policy:   DefaultOutboundPolicy(),
		Observer: &recordingObserver{},
	})
	sat := &toggleFailProducer{OutboundProducer: oc}
	ownerRec := &methodRecordingProducer{OutboundProducer: sat, t: t}
	owner := h.fix.reg.Create(ownerRec)
	h.fix.prods[owner] = ownerRec
	// The witness owns an entity in the post-death cell,
	// so it stays subscribed across the teleport and
	// observes every death cycle.
	witness := h.addReadySession(DefaultOutboundPolicy())
	destCell, _ := world.CellForPosition(dest)
	home, _ := world.CellForPosition(world.Vec3{})
	h.fix.activate(owner, 500, victim, home)
	h.fix.activate(witness, 501, sim.EntityID(900), destCell)
	if _, _, err := h.fix.presence.EnsureVisible(witness, victim); err != nil {
		t.Fatal(err)
	}
	h.fix.source.put(EntityPresentation{EntityID: victim, Position: world.Vec3{}, Kind: 2, Proto: 7})
	h.fix.source.put(EntityPresentation{EntityID: sim.EntityID(900), Position: dest, Kind: 2, Proto: 7})
	h.fix.bootstrap(owner)
	h.fix.bootstrap(witness)

	// Saturate ONLY the victim session's critical lane
	// (armed after bootstrap so readiness held).
	sat.setFail(true)
	h.runtime.OnDeathBegin(beginEv(victim, char, 9))
	// The 214 admission was attempted and failed closed;
	// the healthy viewer still got exactly one 214:
	// saturation closes only the affected recipient.
	if got := callsFor(h.fix.prods[witness], proto.OpcodeDeath); len(got) != 1 {
		t.Fatalf("witness 214 calls = %d; want 1", len(got))
	}
	if n := sat.closeNowCount(); n != 1 {
		t.Fatalf("failed recipient close requests = %d; want 1", n)
	}
	// 215 to the saturated owner cannot admit either:
	// no ack correlation may exist for that session.
	h.runtime.OnDeathCompleted(completedEv(victim, char, 9, dest))
	header, dec := ackFrame(t)
	if err := h.acks.Handle(context.Background(), owner, header, dec, nil); clientErrorOf(t, err).Code != proto.ErrorCodeProtocol {
		t.Fatalf("saturated 120 = %v; want protocol_error (no correlation)", err)
	}
	// Fail-closed is sticky: a second death while still
	// saturated serves the healthy viewer again but
	// never retries the failed recipient (reconnect /
	// full baseline is its only recovery).
	h.runtime.OnDeathBegin(beginEv(victim, char, 10))
	if got := callsFor(h.fix.prods[witness], proto.OpcodeDeath); len(got) != 2 {
		t.Fatalf("witness 214 calls = %d; want 2 (runtime not wedged)", len(got))
	}
	if got := len(callsFor(h.fix.prods[owner], proto.OpcodeDeath)); got != 1 {
		t.Fatalf("failed owner 214 attempts = %d; want 1 (no retry after fail-closed)", got)
	}
	// Recovery is a fresh baseline: un-saturate the
	// transport, re-bootstrap the session, and the next
	// death cycle presents fully again. (The recorder
	// counts admission attempts, so the saturated 215
	// attempt plus the recovered one total two; success
	// is proven by the live ack correlation below.)
	sat.setFail(false)
	h.fix.bootstrap(owner)
	h.runtime.OnDeathBegin(beginEv(victim, char, 11))
	h.runtime.OnDeathCompleted(completedEv(victim, char, 11, dest))
	if got := callsFor(h.fix.prods[owner], proto.OpcodeRespawn); len(got) != 2 {
		t.Fatalf("owner 215 attempts = %d; want 2 (failed + recovered)", len(got))
	}
	header2, dec2 := ackFrame(t)
	if err := h.acks.Handle(context.Background(), owner, header2, dec2, nil); err != nil {
		t.Fatalf("recovered 120 = %v; want silent success (live correlation)", err)
	}
}
