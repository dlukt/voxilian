package persist

import (
	"errors"
	"testing"

	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/store"
	"github.com/dlukt/voxilian/internal/world"
)

// M5-T5c3d1 pending-death executor + mapper unit tests
// (spec §9.5.1k): normal-path pending construction
// (Underworld active vs newbie-home nil), proven-lost-ack
// pending construction from the recovered snapshot
// (CorpseID set and nil), the MapPendingDeathRecovery
// domain, and the fail-closed guarantee that unproven
// recovery never guesses pending from the original plan.
// No PG here (real-PG proofs extend
// death_executor_pg_test.go); Saver is always real, Store
// and the owner sink are deterministic fakes.

// newbieHomeWork builds valid newbie-home executor work:
// a cheap NewbieZoneDeath capture (direct newbie-home
// route, zero cost, no affected items, no killer) with
// canonical runtime inputs.
func newbieHomeWork(t *testing.T) ImmediateDeathPersistenceWork {
	t.Helper()
	e := mustSimEngine(t)
	v := zeroHPDeathVitals(t)
	snap, err := e.AddPlayerEntityWithDurableState(7, world.Vec3{X: 5, Y: 0, Z: 6}, v, testDeathRuntimeInputs(t), fullDeathDurableState())
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	_, base, err := e.PlayerBeginImmediateDeathCapture(snap.ID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	plan, err := sim.PlanDeathDisposition(100, sim.DeathContext{NewbieZoneDeath: true}, false)
	if err != nil {
		t.Fatalf("disposition: %v", err)
	}
	drops, err := sim.PlanDeathDrops(plan, []sim.DeathItemInput{{Key: 0}, {Key: 1}, {Key: 2}})
	if err != nil {
		t.Fatalf("drops: %v", err)
	}
	points, gain, err := sim.DecodeDeathAdvancementInputs(base.Durable.Advancement)
	if err != nil {
		t.Fatalf("decode: %v", err)
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
	post, err := sim.PlanPostDeathVitals(sim.PostDeathVitalsInput{Vitals: testDeathVitals(t), Disposition: sim.DeathCheap})
	if err != nil {
		t.Fatalf("vitals: %v", err)
	}
	capture, err := sim.BuildImmediateDeathCapture(sim.ImmediateDeathBuildInput{
		Base: base, Disposition: plan, Corpse: corpse,
		Drops: drops, Advancement: adv, PostVitals: post,
		Pending: pending, Placement: world.Vec3{X: 1, Y: 0, Z: 1},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return ImmediateDeathPersistenceWork{Capture: capture, RuntimeInputs: testDeathRuntimeInputs(t)}
}

func requireSimPending(t *testing.T, got *sim.PendingDeathRuntime, cost int, tm, corpse int64, corpseSet, portal bool, what string) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s pending nil, want cost%d time%d", what, cost, tm)
	}
	if got.EffectiveCost != cost || got.DeathTimeSeconds != tm || got.PortalUsed != portal {
		t.Fatalf("%s pending = %+v, want cost%d time%d portal%v", what, got, cost, tm, portal)
	}
	if corpseSet && (got.CorpseID == nil || *got.CorpseID != corpse) {
		t.Fatalf("%s corpse = %+v, want %d", what, got.CorpseID, corpse)
	}
	if !corpseSet && got.CorpseID != nil {
		t.Fatalf("%s corpse = %d, want nil", what, *got.CorpseID)
	}
}

// TestCompletionWithNormalPendingUnderworld proves the
// frozen normal-path construction: exact cost/time from
// the committed request, the generated CorpseID, and
// PortalUsed == false on an independent pointer.
func TestCompletionWithNormalPendingUnderworld(t *testing.T) {
	work := executorWork(t)
	req := intendedReq(t, work)
	// Pin the task's frozen values at unit level.
	req.EffectiveDeathCost = 42
	req.DeathTimeSeconds = 500
	req.NewbieHomeRespawn = false
	template, err := prepareTemplate(work)
	if err != nil {
		t.Fatal(err)
	}
	got, err := completionWithNormalPending(req, template, store.DeathEntryResult{CorpseID: 777})
	if err != nil {
		t.Fatalf("completionWithNormalPending: %v", err)
	}
	requireSimPending(t, got.Pending, 42, 500, 777, true, false, "normal")
	if got.Token != work.Capture.Token || got.Placement != work.Capture.Placement {
		t.Fatalf("template fields lost: %+v", got)
	}
	// Pointer independence: mutating the delivered
	// pointed value cannot affect a second construction.
	*got.Pending.CorpseID = -5
	again, err := completionWithNormalPending(req, template, store.DeathEntryResult{CorpseID: 777})
	if err != nil {
		t.Fatal(err)
	}
	requireSimPending(t, again.Pending, 42, 500, 777, true, false, "normal-again")
	if template.Pending != nil {
		t.Fatalf("template gained pending")
	}
}

// prepareTemplate runs the shared prepare core to obtain
// the frozen completion template for unit-level pending
// tests.
func prepareTemplate(work ImmediateDeathPersistenceWork) (sim.ImmediateDeathCompletion, error) {
	_, completion, err := prepareDeathExecutorWork(work)
	if err != nil {
		return sim.ImmediateDeathCompletion{}, err
	}
	return completion, nil
}

// TestCompletionWithNormalPendingNewbieHome proves a
// direct newbie-home commit carries Pending = nil even
// though the transaction still generated a corpse.
func TestCompletionWithNormalPendingNewbieHome(t *testing.T) {
	work := newbieHomeWork(t)
	req := intendedReq(t, work)
	if !req.NewbieHomeRespawn {
		t.Fatalf("fixture respawn = false, want newbie-home")
	}
	template, err := prepareTemplate(work)
	if err != nil {
		t.Fatal(err)
	}
	got, err := completionWithNormalPending(req, template, store.DeathEntryResult{CorpseID: 999})
	if err != nil {
		t.Fatalf("completionWithNormalPending: %v", err)
	}
	if got.Pending != nil {
		t.Fatalf("newbie-home pending = %+v, want nil", got.Pending)
	}
}

// TestCompletionWithNormalPendingBadCorpse proves a
// non-positive generated CorpseID on an Underworld-bound
// commit fails closed with no completion.
func TestCompletionWithNormalPendingBadCorpse(t *testing.T) {
	work := executorWork(t)
	req := intendedReq(t, work)
	template, err := prepareTemplate(work)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{0, -3} {
		if _, err := completionWithNormalPending(req, template, store.DeathEntryResult{CorpseID: id}); err == nil {
			t.Fatalf("corpse %d accepted", id)
		}
	}
}

// TestDeathExecutorNormalPendingDelivered proves the
// end-to-end normal Underworld commit: the sink receives
// Pending with the frozen cost/time, the generated
// CorpseID, and PortalUsed == false, with no recovery
// load.
func TestDeathExecutorNormalPendingDelivered(t *testing.T) {
	s := mustSaverForPersist(t)
	trackExecutorRoots(t, s, 0, 0)
	work := executorWork(t)
	intended := intendedReq(t, work)
	fs := newFakeRecoveryStore()
	fs.onDeath = func(req store.DeathEntryRequest) (store.DeathEntryResult, error) {
		res := echoDeathResult(req)
		res.CorpseID = 777
		return res, nil
	}
	sink := &fakeCompletionSink{}
	ex, _ := startExecutor(t, DeathExecutorConfig{
		Workers: 1, QueueCapacity: 4, Store: fs, Saver: s, Sink: sink,
	})
	res := awaitResult(t, mustSubmit(t, ex, work))
	if res.Err != nil {
		t.Fatalf("result err = %v", res.Err)
	}
	if res.Recovered {
		t.Fatal("Recovered = true, want false")
	}
	if fs.charCalls != 0 || len(fs.itemCalls) != 0 {
		t.Fatalf("recovery loads char=%d items=%v, want zero (no normal-path reload)", fs.charCalls, fs.itemCalls)
	}
	if sink.numCalls() != 1 {
		t.Fatalf("sink calls = %d, want 1", sink.numCalls())
	}
	requireSimPending(t, sink.calls[0].Pending,
		int(intended.EffectiveDeathCost), intended.DeathTimeSeconds, 777, true, false, "sink")
}

// TestDeathExecutorNewbieHomePendingNil proves the
// end-to-end normal newbie-home commit: the Store may
// still return a CorpseID, but the sink receives
// Pending = nil.
func TestDeathExecutorNewbieHomePendingNil(t *testing.T) {
	s := mustSaverForPersist(t)
	if err := s.Track(sim.AggregateKey{Kind: sim.AggregateCharacter, ID: 7}, 0); err != nil {
		t.Fatal(err)
	}
	work := newbieHomeWork(t)
	fs := newFakeRecoveryStore()
	fs.onDeath = func(req store.DeathEntryRequest) (store.DeathEntryResult, error) {
		res := echoDeathResult(req)
		res.CorpseID = 555
		return res, nil
	}
	sink := &fakeCompletionSink{}
	ex, _ := startExecutor(t, DeathExecutorConfig{
		Workers: 1, QueueCapacity: 4, Store: fs, Saver: s, Sink: sink,
	})
	res := awaitResult(t, mustSubmit(t, ex, work))
	if res.Err != nil {
		t.Fatalf("result err = %v", res.Err)
	}
	if sink.numCalls() != 1 {
		t.Fatalf("sink calls = %d, want 1", sink.numCalls())
	}
	if sink.calls[0].Pending != nil {
		t.Fatalf("newbie-home sink pending = %+v, want nil", sink.calls[0].Pending)
	}
}

// TestDeathExecutorLostAckPendingDelivered proves proven
// lost-ack delivers the RECOVERED authoritative pending
// values — both with a live CorpseID and with a nil
// CorpseID (corpse expired via ON DELETE SET NULL). No
// Store replay in either case.
func TestDeathExecutorLostAckPendingDelivered(t *testing.T) {
	for _, tc := range []struct {
		name      string
		corpse    *int64
		corpseSet bool
	}{
		{"corpse live", func() *int64 { v := int64(777000); return &v }(), true},
		{"corpse expired", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := mustSaverForPersist(t)
			trackExecutorRoots(t, s, 0, 0)
			work := executorWork(t)
			intended := intendedReq(t, work)
			fs := newFakeRecoveryStore()
			fs.onDeath = func(store.DeathEntryRequest) (store.DeathEntryResult, error) {
				return store.DeathEntryResult{}, errExecutorSynth
			}
			fs.charSnap = provenCharRec(intended.Character, underworldPending(intended, tc.corpse))
			for _, it := range intended.Items {
				fs.itemSnaps[it.Snapshot.ID] = provenItemRec(intended.Character.ID, it)
			}
			sink := &fakeCompletionSink{}
			ex, _ := startExecutor(t, DeathExecutorConfig{
				Workers: 1, QueueCapacity: 4, Store: fs, Saver: s, Sink: sink,
			})
			res := awaitResult(t, mustSubmit(t, ex, work))
			if res.Err != nil {
				t.Fatalf("result err = %v", res.Err)
			}
			if !res.Recovered {
				t.Fatal("Recovered = false, want true")
			}
			if fs.deathCalls != 1 {
				t.Fatalf("store calls = %d, want 1 (no replay)", fs.deathCalls)
			}
			if sink.numCalls() != 1 {
				t.Fatalf("sink calls = %d, want 1", sink.numCalls())
			}
			var wantCorpse int64
			if tc.corpse != nil {
				wantCorpse = *tc.corpse
			}
			requireSimPending(t, sink.calls[0].Pending,
				int(intended.EffectiveDeathCost), intended.DeathTimeSeconds,
				wantCorpse, tc.corpseSet, false, "lost-ack sink")
		})
	}
}

// TestCompletionWithRecoveredPendingCharacterMismatch
// proves the recovered mapper rejects a pending row for
// the wrong character instead of installing it.
func TestCompletionWithRecoveredPendingCharacterMismatch(t *testing.T) {
	work := executorWork(t)
	req := intendedReq(t, work)
	template, err := prepareTemplate(work)
	if err != nil {
		t.Fatal(err)
	}
	corpse := int64(1)
	bad := underworldPending(req, &corpse)
	bad.CharacterID++
	if _, err := completionWithRecoveredPending(req, template, bad); err == nil {
		t.Fatalf("wrong-character pending accepted")
	}
}

// TestDeathExecutorUnprovenPendingNotGuessed proves the
// fail-closed rule: when recovery cannot prove the
// commit (here a pending-shape mismatch), the executor
// returns ErrDeathCommitUnproven with NO owner completion
// — no pending state is guessed from the original plan.
func TestDeathExecutorUnprovenPendingNotGuessed(t *testing.T) {
	s := mustSaverForPersist(t)
	trackExecutorRoots(t, s, 0, 0)
	work := executorWork(t)
	intended := intendedReq(t, work)
	fs := newFakeRecoveryStore()
	fs.onDeath = func(store.DeathEntryRequest) (store.DeathEntryResult, error) {
		return store.DeathEntryResult{}, errExecutorSynth
	}
	corpse := int64(777000)
	pending := underworldPending(intended, &corpse)
	pending.EffectiveCost++
	fs.charSnap = provenCharRec(intended.Character, pending)
	for _, it := range intended.Items {
		fs.itemSnaps[it.Snapshot.ID] = provenItemRec(intended.Character.ID, it)
	}
	sink := &fakeCompletionSink{}
	ex, _ := startExecutor(t, DeathExecutorConfig{
		Workers: 1, QueueCapacity: 4, Store: fs, Saver: s, Sink: sink,
	})
	res := awaitResult(t, mustSubmit(t, ex, work))
	if res.Err == nil || !errors.Is(res.Err, ErrDeathCommitUnproven) {
		t.Fatalf("err = %v, want ErrDeathCommitUnproven", res.Err)
	}
	if fs.deathCalls != 1 {
		t.Fatalf("store calls = %d, want 1 (no replay)", fs.deathCalls)
	}
	if sink.numCalls() != 0 {
		t.Fatalf("sink calls = %d, want 0 (no guessed completion)", sink.numCalls())
	}
}

// TestMapPendingDeathRecovery pins the narrow recovery
// mapper: nil -> nil, character mismatch, bad cost, bad
// time, bad CorpseID rejected, values copied with a
// deep-copied CorpseID pointer.
func TestMapPendingDeathRecovery(t *testing.T) {
	base := &store.PendingDeathSnapshot{
		CharacterID: 7, EffectiveCost: 42, DeathTimeSeconds: 500,
		CorpseID: func() *int64 { v := int64(777); return &v }(), PortalUsed: true,
	}
	got, err := MapPendingDeathRecovery(7, base)
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	requireSimPending(t, got, 42, 500, 777, true, true, "mapper")
	// Deep copy: mutating the source snapshot (value and
	// pointed CorpseID) cannot reach the mapped value.
	*base.CorpseID = -9
	base.EffectiveCost = 1
	requireSimPending(t, got, 42, 500, 777, true, true, "mapper-copy")
	if got.CorpseID == base.CorpseID {
		t.Fatalf("corpse pointer aliases source")
	}
	if out, err := MapPendingDeathRecovery(7, nil); err != nil || out != nil {
		t.Fatalf("nil = %+v,%v; want nil,nil", out, err)
	}
	mismatch := *base
	mismatch.CharacterID = 8
	if _, err := MapPendingDeathRecovery(7, &mismatch); err == nil {
		t.Fatalf("character mismatch accepted")
	}
	for _, tc := range []struct {
		name   string
		mutate func(*store.PendingDeathSnapshot)
	}{
		{"cost -1", func(p *store.PendingDeathSnapshot) { p.EffectiveCost = -1 }},
		{"cost 101", func(p *store.PendingDeathSnapshot) { p.EffectiveCost = 101 }},
		{"negative time", func(p *store.PendingDeathSnapshot) { p.DeathTimeSeconds = -1 }},
		{"corpse 0", func(p *store.PendingDeathSnapshot) {
			v := int64(0)
			p.CorpseID = &v
		}},
		{"corpse -1", func(p *store.PendingDeathSnapshot) {
			v := int64(-1)
			p.CorpseID = &v
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := &store.PendingDeathSnapshot{
				CharacterID: 7, EffectiveCost: 42, DeathTimeSeconds: 500,
				CorpseID: func() *int64 { v := int64(777); return &v }(),
			}
			tc.mutate(bad)
			if _, err := MapPendingDeathRecovery(7, bad); err == nil {
				t.Fatalf("invalid snapshot accepted")
			}
		})
	}
	// Nil CorpseID is valid and stays nil.
	nilCorpse := &store.PendingDeathSnapshot{CharacterID: 7, EffectiveCost: 0, DeathTimeSeconds: 0}
	out, err := MapPendingDeathRecovery(7, nilCorpse)
	if err != nil {
		t.Fatalf("nil-corpse map: %v", err)
	}
	requireSimPending(t, out, 0, 0, 0, false, false, "nil-corpse")
}
