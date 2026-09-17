package persist

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/store"
	"github.com/dlukt/voxilian/internal/store/gen"
	"github.com/dlukt/voxilian/internal/world"
)

// M5-T5c3c3b real-PostgreSQL 18 executor proofs (spec §9.5.1h,
// frozen v0.3.49): normal commit through the executor, REAL
// lost-ack proof (real transaction commits exactly once, the
// adapter sees only the synthetic error, materialized recovery
// proves it with NO Store replay), and real stale-CAS proof
// (rejected attempt creates nothing, recovery fails closed).
// The real store.PGStore satisfies DeathExecutionStore; the
// owner sink stays a deterministic fake (typed c3c3a ingress
// is Engine-owned; c3c3c owns live orchestration).

// pgExecutorDurable builds catalog-valid durable state over
// the given real PG item IDs: spell IDs 1-2 and skill ID 1
// exist via pgAbilityProtos; proto 900 exists via pgItemRoot.
func pgExecutorDurable(itemIDs []int64) sim.PlayerDurableState {
	items := make([]sim.PlayerInventoryItemState, 0, len(itemIDs))
	for _, id := range itemIDs {
		items = append(items, sim.PlayerInventoryItemState{
			ID: id, ProtoID: 900, Qty: 3, Hits: 250,
			Enchants: []byte(`{"glow":1}`), Slot: "hand",
		})
	}
	return sim.PlayerDurableState{
		Karma:       150,
		Advancement: []byte(`{"adv_points":7,"gain_chance":-40,"school_casts":{"1":3},"custom":"keep"}`),
		Flags:       0x1274,
		Spells: []sim.PlayerAbilityState{
			{ID: 1, Ability: 50, AtrophyFlag: false},
			{ID: 2, Ability: 60, AtrophyFlag: true},
		},
		Skills: []sim.PlayerAbilityState{
			{ID: 1, Ability: 40, AtrophyFlag: false},
		},
		Items: items,
	}
}

// pgExecutorSetup mirrors deathPGSetup but retains the pool
// and query handle: PG executor tests need test-only SQL
// (corpse/pending counts, stale bumps) plus extra fixture
// characters (a real killer: kills.killer_character_id is an
// FK into characters, so a fictional killer ID would fail the
// real transaction with an FK violation, not a death error).
func pgExecutorSetup(
	t *testing.T, sub, name string, nItems int,
) (context.Context, *pgxpool.Pool, *gen.Queries, *store.PGStore, *sim.Saver, int64, []int64) {
	t.Helper()
	pool, q := openPG(t)
	ctx := context.Background()
	st, err := store.New(pool, newPGRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	charID := pgAccountChar(t, q, sub, name)
	pgAbilityProtos(t, pool)
	items := make([]int64, 0, nItems)
	for range nItems {
		items = append(items, pgItemRoot(t, pool, charID))
	}
	s := mustSaverForPersist(t)
	if err := s.Track(sim.AggregateKey{Kind: sim.AggregateCharacter, ID: charID}, 0); err != nil {
		t.Fatal(err)
	}
	for _, id := range items {
		if err := s.Track(sim.AggregateKey{Kind: sim.AggregateItem, ID: id}, 0); err != nil {
			t.Fatal(err)
		}
	}
	return ctx, pool, q, st, s, charID, items
}

// pgExecutorCapture builds a complete Normal-death sim capture
// over real PG identities through the public owner-local sim
// API (mirrors captureFixture with PG-valid catalog content).
// killerCharID > 0 selects a player killer (which must be a
// REAL character: kills.killer_character_id is an FK); <= 0
// selects an environmental death with no kills row.
func pgExecutorCapture(
	t *testing.T, charID int64, itemIDs []int64, killerCharID int64,
) sim.ImmediateDeathCapture {
	t.Helper()
	e := mustSimEngine(t)
	v := zeroHPDeathVitals(t)
	snap, err := e.AddPlayerEntityWithDurableState(sim.CharacterID(charID),
		world.Vec3{X: 5, Y: 0, Z: 6}, v, testDeathRuntimeInputs(t), pgExecutorDurable(itemIDs))
	if err != nil {
		t.Fatalf("add player: %v", err)
	}
	_, base, err := e.PlayerBeginImmediateDeathCapture(snap.ID)
	if err != nil {
		t.Fatalf("begin capture: %v", err)
	}
	killerIsPlayer := killerCharID > 0
	killer := sim.DeathKillerIdentity{Kind: sim.DeathKillerNone}
	if killerIsPlayer {
		killer = sim.DeathKillerIdentity{Kind: sim.DeathKillerCharacter, CharacterID: sim.CharacterID(killerCharID)}
	}
	plan, err := sim.PlanDeathDisposition(40, sim.DeathContext{}, killerIsPlayer)
	if err != nil {
		t.Fatalf("disposition: %v", err)
	}
	inputs := make([]sim.DeathItemInput, 0, len(itemIDs))
	for i := range itemIDs {
		inputs = append(inputs, sim.DeathItemInput{Key: i, DropOnDeath: true, RoomAccepts: true})
	}
	drops, err := sim.PlanDeathDrops(plan, inputs)
	if err != nil {
		t.Fatalf("drops: %v", err)
	}
	points, gain, err := sim.DecodeDeathAdvancementInputs(base.Durable.Advancement)
	if err != nil {
		t.Fatalf("advancement inputs: %v", err)
	}
	adv, err := sim.PlanDeathAdvancement(sim.DeathNormal, points, gain)
	if err != nil {
		t.Fatalf("advancement: %v", err)
	}
	corpse, err := sim.PlanCorpse(777)
	if err != nil {
		t.Fatalf("corpse: %v", err)
	}
	pending, err := sim.PlanPendingDeath(plan, corpse)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	post, err := sim.PlanPostDeathVitals(sim.PostDeathVitalsInput{
		Vitals: testDeathVitals(t), Disposition: sim.DeathNormal,
	})
	if err != nil {
		t.Fatalf("post vitals: %v", err)
	}
	got, err := sim.BuildImmediateDeathCapture(sim.ImmediateDeathBuildInput{
		Base: base, Disposition: plan, Corpse: corpse,
		Drops: drops, Advancement: adv, PostVitals: post,
		Pending: pending, Placement: world.Vec3{X: 100.5, Y: -2.25, Z: 100},
		Killer: killer,
	})
	if err != nil {
		t.Fatalf("build capture: %v", err)
	}
	return got
}

// Normal real commit through the executor: real Saver roots,
// one death (character + one affected item, Underworld-bound
// pending), real CommitDeathEntry, real materialized state,
// owner completion Applied, no recovery path, exact revisions.
func TestPersistPGDeathExecutorNormal(t *testing.T) {
	ctx, pool, q, st, s, charID, items := pgExecutorSetup(t, "sub-exec-normal", "ExecNormal", 1)
	killerID := pgAccountChar(t, q, "sub-exec-normal-k", "ExecNormalKiller")
	capture := pgExecutorCapture(t, charID, items, killerID)
	sink := &fakeCompletionSink{}
	ex, _ := startExecutor(t, DeathExecutorConfig{
		Workers: 2, QueueCapacity: 8, Store: st, Saver: s, Sink: sink,
	})
	res := awaitResult(t, mustSubmit(t, ex, ImmediateDeathPersistenceWork{
		Capture: capture, RuntimeInputs: testDeathRuntimeInputs(t),
	}))
	if res.Err != nil {
		t.Fatalf("result err = %v", res.Err)
	}
	if res.Recovered {
		t.Fatal("Recovered = true, want false (normal ack, no recovery path)")
	}
	if res.Delivery != sim.DeathCompletionApplied {
		t.Fatalf("delivery = %v, want Applied", res.Delivery)
	}
	if sink.numCalls() != 1 {
		t.Fatalf("sink calls = %d, want 1", sink.numCalls())
	}
	got := sink.calls[0]
	if int64(got.Token.CharacterID) != charID {
		t.Fatalf("completion character = %d, want %d", int64(got.Token.CharacterID), charID)
	}
	if got.Placement != capture.Placement {
		t.Fatalf("completion placement = %+v, want %+v", got.Placement, capture.Placement)
	}
	// Exact revisions on every participant, clean Saver.
	if insp, _ := s.Inspect(sim.AggregateKey{Kind: sim.AggregateCharacter, ID: charID}); insp.KnownRevision != 1 || insp.Blocked {
		t.Fatalf("saver char = %+v, want known1 clean", insp)
	}
	if insp, _ := s.Inspect(sim.AggregateKey{Kind: sim.AggregateItem, ID: items[0]}); insp.KnownRevision != 1 || insp.Blocked {
		t.Fatalf("saver item = %+v, want known1 clean", insp)
	}
	// Real materialized state: rev 1 at post-death placement,
	// pending cost 40 with corpse, item on the ground with PK
	// protection for the player killer.
	rec, err := st.LoadDeathCharacterRecovery(ctx, charID)
	if err != nil {
		t.Fatalf("character recovery: %v", err)
	}
	if rec.Character.ExpectedRevision != 1 || rec.Character.PosX != 100500 ||
		rec.Character.PosY != -2250 || rec.Character.PosZ != 100000 {
		t.Fatalf("character = rev%d pos %d/%d/%d, want rev1 at placement mm",
			rec.Character.ExpectedRevision, rec.Character.PosX, rec.Character.PosY, rec.Character.PosZ)
	}
	if rec.Pending == nil || rec.Pending.EffectiveCost != 40 ||
		rec.Pending.DeathTimeSeconds != 777 || rec.Pending.CorpseID == nil || rec.Pending.PortalUsed {
		t.Fatalf("pending = %+v, want cost40 time777 with corpse unused", rec.Pending)
	}
	irec, err := st.LoadDeathItemRecovery(ctx, items[0])
	if err != nil {
		t.Fatalf("item recovery: %v", err)
	}
	if irec.Item.ExpectedRevision != 1 || irec.Item.Location.Kind != 1 ||
		irec.Item.Location.PosX == nil || *irec.Item.Location.PosX != 5000 {
		t.Fatalf("item = %+v, want rev1 ground at death pos", irec.Item)
	}
	if irec.PKProtection == nil || irec.PKProtection.VictimCharacterID != charID {
		t.Fatalf("protection = %+v, want victim %d", irec.PKProtection, charID)
	}
	// d1 owner state: the sink completion carries the exact
	// authoritative pending state — cost/time from the
	// committed request, CorpseID == the actual generated
	// corpse row ID, PortalUsed == false.
	var corpseID int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM corpses WHERE character_id = $1`, charID).Scan(&corpseID); err != nil {
		t.Fatal(err)
	}
	pending := got.Pending
	if pending == nil || pending.EffectiveCost != 40 ||
		pending.DeathTimeSeconds != 777 || pending.CorpseID == nil ||
		*pending.CorpseID != corpseID || pending.PortalUsed {
		t.Fatalf("sink pending = %+v, want cost40 time777 corpse%d unused", pending, corpseID)
	}
}

// pgExecutorNewbieCapture builds a complete cheap
// newbie-home sim capture over real PG identities: direct
// newbie-home route, zero cost, no affected items, no
// killer, corpse row still generated, no pending row.
func pgExecutorNewbieCapture(t *testing.T, charID int64) sim.ImmediateDeathCapture {
	t.Helper()
	e := mustSimEngine(t)
	v := zeroHPDeathVitals(t)
	snap, err := e.AddPlayerEntityWithDurableState(sim.CharacterID(charID),
		world.Vec3{X: 5, Y: 0, Z: 6}, v, testDeathRuntimeInputs(t), pgExecutorDurable(nil))
	if err != nil {
		t.Fatalf("add player: %v", err)
	}
	_, base, err := e.PlayerBeginImmediateDeathCapture(snap.ID)
	if err != nil {
		t.Fatalf("begin capture: %v", err)
	}
	plan, err := sim.PlanDeathDisposition(40, sim.DeathContext{NewbieZoneDeath: true}, false)
	if err != nil {
		t.Fatalf("disposition: %v", err)
	}
	drops, err := sim.PlanDeathDrops(plan, nil)
	if err != nil {
		t.Fatalf("drops: %v", err)
	}
	points, gain, err := sim.DecodeDeathAdvancementInputs(base.Durable.Advancement)
	if err != nil {
		t.Fatalf("advancement inputs: %v", err)
	}
	adv, err := sim.PlanDeathAdvancement(sim.DeathCheap, points, gain)
	if err != nil {
		t.Fatalf("advancement: %v", err)
	}
	corpse, err := sim.PlanCorpse(12)
	if err != nil {
		t.Fatalf("corpse: %v", err)
	}
	pending, err := sim.PlanPendingDeath(plan, corpse)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	post, err := sim.PlanPostDeathVitals(sim.PostDeathVitalsInput{
		Vitals: testDeathVitals(t), Disposition: sim.DeathCheap,
	})
	if err != nil {
		t.Fatalf("post vitals: %v", err)
	}
	got, err := sim.BuildImmediateDeathCapture(sim.ImmediateDeathBuildInput{
		Base: base, Disposition: plan, Corpse: corpse,
		Drops: drops, Advancement: adv, PostVitals: post,
		Pending: pending, Placement: world.Vec3{X: 1, Y: 0, Z: 1},
		Killer: sim.DeathKillerIdentity{Kind: sim.DeathKillerNone},
	})
	if err != nil {
		t.Fatalf("build capture: %v", err)
	}
	return got
}

// Real newbie-home normal success through the executor:
// the corpse row exists, the sink completion carries
// Pending == nil, and no pending_deaths row exists. No
// Store replay, no recovery path.
func TestPersistPGDeathExecutorNewbieHome(t *testing.T) {
	ctx, pool, _, st, s, charID, _ := pgExecutorSetup(t, "sub-exec-newbie", "ExecNewbie", 0)
	capture := pgExecutorNewbieCapture(t, charID)
	sink := &fakeCompletionSink{}
	ex, _ := startExecutor(t, DeathExecutorConfig{
		Workers: 1, QueueCapacity: 4, Store: st, Saver: s, Sink: sink,
	})
	res := awaitResult(t, mustSubmit(t, ex, ImmediateDeathPersistenceWork{
		Capture: capture, RuntimeInputs: testDeathRuntimeInputs(t),
	}))
	if res.Err != nil {
		t.Fatalf("result err = %v", res.Err)
	}
	if res.Recovered {
		t.Fatal("Recovered = true, want false (normal ack, no recovery path)")
	}
	if res.Delivery != sim.DeathCompletionApplied {
		t.Fatalf("delivery = %v, want Applied", res.Delivery)
	}
	if sink.numCalls() != 1 {
		t.Fatalf("sink calls = %d, want 1", sink.numCalls())
	}
	if sink.calls[0].Pending != nil {
		t.Fatalf("sink pending = %+v, want nil (newbie-home)", sink.calls[0].Pending)
	}
	var corpses, pendings int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM corpses WHERE character_id = $1`, charID).Scan(&corpses); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM pending_deaths WHERE character_id = $1`, charID).Scan(&pendings); err != nil {
		t.Fatal(err)
	}
	if corpses != 1 || pendings != 0 {
		t.Fatalf("corpses=%d pendings=%d, want 1/0 (corpse kept, no pending row)", corpses, pendings)
	}
}

// execLostAckStore calls the REAL CommitDeathEntry first
// (materializing the transaction exactly once), discards its
// success, and returns the synthetic lost-ack error — the REAL
// ambiguity shape from the T5c2b lost-ack fixtures.
type execLostAckStore struct {
	DeathExecutionStore
	armed *bool
	calls int
}

func (w *execLostAckStore) CommitDeathEntry(
	ctx context.Context, req store.DeathEntryRequest,
) (store.DeathEntryResult, error) {
	w.calls++
	res, err := w.DeathExecutionStore.CommitDeathEntry(ctx, req)
	if err != nil {
		return res, err
	}
	if *w.armed {
		*w.armed = false
		return store.DeathEntryResult{}, errLostAck
	}
	return res, nil
}

// REAL lost-ack proof (mandatory): the real DB transaction
// committed exactly once, the executor sees only
// reconcile-required ambiguity, real T5c2a character + item
// recovery runs, all roots prove exact expected+1 with exact
// content, NO Store replay, owner completion delivered — with
// exactly one corpse row and exactly one pending row.
func TestPersistPGDeathExecutorLostAck(t *testing.T) {
	ctx, pool, q, st, s, charID, items := pgExecutorSetup(t, "sub-exec-lostack", "ExecLostAck", 2)
	killerID := pgAccountChar(t, q, "sub-exec-lostack-k", "ExecLostAckKiller")
	capture := pgExecutorCapture(t, charID, items, killerID)
	armed := true
	wrap := &execLostAckStore{DeathExecutionStore: st, armed: &armed}
	sink := &fakeCompletionSink{}
	ex, _ := startExecutor(t, DeathExecutorConfig{
		Workers: 2, QueueCapacity: 8, Store: wrap, Saver: s, Sink: sink,
	})
	res := awaitResult(t, mustSubmit(t, ex, ImmediateDeathPersistenceWork{
		Capture: capture, RuntimeInputs: testDeathRuntimeInputs(t),
	}))
	if res.Err != nil {
		t.Fatalf("result err = %v, want proven lost-ack success", res.Err)
	}
	if !res.Recovered {
		t.Fatal("Recovered = false, want true (proven materialized recovery)")
	}
	if res.Delivery != sim.DeathCompletionApplied {
		t.Fatalf("delivery = %v, want Applied", res.Delivery)
	}
	if wrap.calls != 1 {
		t.Fatalf("real CommitDeathEntry calls = %d, want exactly 1 (no replay)", wrap.calls)
	}
	if sink.numCalls() != 1 {
		t.Fatalf("sink calls = %d, want 1", sink.numCalls())
	}
	for _, k := range []sim.AggregateKey{
		{Kind: sim.AggregateCharacter, ID: charID},
		{Kind: sim.AggregateItem, ID: items[0]},
		{Kind: sim.AggregateItem, ID: items[1]},
	} {
		if insp, _ := s.Inspect(k); insp.KnownRevision != 1 || insp.Blocked {
			t.Fatalf("saver %v = %+v, want reconciled known1", k, insp)
		}
	}
	// Test-only DB inspection: exactly one corpse and one
	// pending row for this death (fixtures create none).
	var corpses, pendings int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM corpses WHERE character_id = $1`, charID).Scan(&corpses); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM pending_deaths WHERE character_id = $1`, charID).Scan(&pendings); err != nil {
		t.Fatal(err)
	}
	if corpses != 1 || pendings != 1 {
		t.Fatalf("corpses=%d pendings=%d, want exactly 1/1", corpses, pendings)
	}
	rec, err := st.LoadDeathCharacterRecovery(ctx, charID)
	if err != nil {
		t.Fatalf("character recovery: %v", err)
	}
	if rec.Character.ExpectedRevision != 1 || rec.Pending == nil || rec.Pending.EffectiveCost != 40 {
		t.Fatalf("materialized = %+v, want committed rev1 cost40", rec)
	}
	// d1 owner state: the proven lost-ack completion
	// delivers the recovered pending state exactly — no
	// Store replay, no guessed values.
	delivered := sink.calls[0].Pending
	if delivered == nil || delivered.EffectiveCost != int(rec.Pending.EffectiveCost) ||
		delivered.DeathTimeSeconds != rec.Pending.DeathTimeSeconds ||
		delivered.PortalUsed != rec.Pending.PortalUsed {
		t.Fatalf("sink pending = %+v, want recovered %+v", delivered, rec.Pending)
	}
	if (delivered.CorpseID == nil) != (rec.Pending.CorpseID == nil) ||
		(delivered.CorpseID != nil && *delivered.CorpseID != *rec.Pending.CorpseID) {
		t.Fatalf("sink corpse = %+v, want recovered %+v", delivered.CorpseID, rec.Pending.CorpseID)
	}
}

// execCountStore counts real Store commits (stale proof must
// show exactly one attempt, never a replay).
type execCountStore struct {
	DeathExecutionStore
	calls *int
}

func (w *execCountStore) CommitDeathEntry(
	ctx context.Context, req store.DeathEntryRequest,
) (store.DeathEntryResult, error) {
	*w.calls++
	return w.DeathExecutionStore.CommitDeathEntry(ctx, req)
}

// Real stale CAS (mandatory): the character root is advanced
// durably behind the Saver's known revision before execution.
// The death transaction does NOT commit, recovery loads the
// authoritative PG state, the Saver reconciles, the result is
// ErrDeathCommitUnproven with no completion, no corpse, and no
// Store replay — proving lost-ack and stale do not collapse
// into one recovery result.
func TestPersistPGDeathExecutorStale(t *testing.T) {
	ctx, pool, _, st, s, charID, items := pgExecutorSetup(t, "sub-exec-stale", "ExecStale", 1)
	// Durably advance the character behind the Saver (Saver
	// still knows 0; PG is now 1 with PRE-death content).
	if _, err := pool.Exec(ctx,
		`UPDATE characters SET revision = revision + 1 WHERE id = $1`, charID); err != nil {
		t.Fatal(err)
	}
	capture := pgExecutorCapture(t, charID, items, 0)
	calls := 0
	wrap := &execCountStore{DeathExecutionStore: st, calls: &calls}
	sink := &fakeCompletionSink{}
	ex, _ := startExecutor(t, DeathExecutorConfig{
		Workers: 1, QueueCapacity: 4, Store: wrap, Saver: s, Sink: sink,
	})
	res := awaitResult(t, mustSubmit(t, ex, ImmediateDeathPersistenceWork{
		Capture: capture, RuntimeInputs: testDeathRuntimeInputs(t),
	}))
	if res.Err == nil || !errors.Is(res.Err, ErrDeathCommitUnproven) {
		t.Fatalf("err = %v, want ErrDeathCommitUnproven", res.Err)
	}
	if calls != 1 {
		t.Fatalf("real Store calls = %d, want 1 (no replay)", calls)
	}
	if sink.numCalls() != 0 {
		t.Fatalf("sink calls = %d, want 0 (no completion)", sink.numCalls())
	}
	var corpses, pendings int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM corpses WHERE character_id = $1`, charID).Scan(&corpses); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM pending_deaths WHERE character_id = $1`, charID).Scan(&pendings); err != nil {
		t.Fatal(err)
	}
	if corpses != 0 || pendings != 0 {
		t.Fatalf("corpses=%d pendings=%d, want 0/0 (rejected attempt created nothing)", corpses, pendings)
	}
	// Saver reconciled every participant to authoritative PG
	// (character rev 1, item rev 0), clean.
	if insp, _ := s.Inspect(sim.AggregateKey{Kind: sim.AggregateCharacter, ID: charID}); insp.KnownRevision != 1 || insp.Blocked {
		t.Fatalf("saver char = %+v, want reconciled known1", insp)
	}
	if insp, _ := s.Inspect(sim.AggregateKey{Kind: sim.AggregateItem, ID: items[0]}); insp.KnownRevision != 0 || insp.Blocked {
		t.Fatalf("saver item = %+v, want reconciled known0", insp)
	}
}
