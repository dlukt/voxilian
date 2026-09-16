package persist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/store"
)

// M5-T5c3c3b executor + recovery-proof unit tests (spec
// §9.5.1h, frozen v0.3.49): exact callback expected-revision
// observation, conservative materialized commit proof, and the
// bounded off-owner executor. No PG here (real-PG proofs live
// in death_executor_pg_test.go); the Saver is always real,
// Store and the owner sink are deterministic fakes.

// ---- fakes ---------------------------------------------------

// fakeRecoveryStore scripts CommitDeathEntry (via the embedded
// T5c2b fake) and serves deterministic T5c2a recovery
// snapshots. Call counts prove zero-replay and
// per-participant recovery discipline.
type fakeRecoveryStore struct {
	*fakeDeathStore
	charSnap  store.DeathCharacterRecoverySnapshot
	charErr   error
	charCalls int
	itemSnaps map[int64]store.DeathItemRecoverySnapshot
	itemErrs  map[int64]error
	itemCalls map[int64]int
}

// Compile-time proof the fake satisfies the executor seam.
var _ DeathExecutionStore = (*fakeRecoveryStore)(nil)

func newFakeRecoveryStore() *fakeRecoveryStore {
	return &fakeRecoveryStore{
		fakeDeathStore: &fakeDeathStore{},
		itemSnaps:      map[int64]store.DeathItemRecoverySnapshot{},
		itemErrs:       map[int64]error{},
		itemCalls:      map[int64]int{},
	}
}

func (f *fakeRecoveryStore) LoadDeathCharacterRecovery(
	_ context.Context, _ int64,
) (store.DeathCharacterRecoverySnapshot, error) {
	f.charCalls++
	if f.charErr != nil {
		return store.DeathCharacterRecoverySnapshot{}, f.charErr
	}
	return f.charSnap, nil
}

func (f *fakeRecoveryStore) LoadDeathItemRecovery(
	_ context.Context, itemID int64,
) (store.DeathItemRecoverySnapshot, error) {
	f.itemCalls[itemID]++
	if err, ok := f.itemErrs[itemID]; ok {
		return store.DeathItemRecoverySnapshot{}, err
	}
	snap, ok := f.itemSnaps[itemID]
	if !ok {
		return store.DeathItemRecoverySnapshot{}, fmt.Errorf("test: no item snapshot for %d", itemID)
	}
	return snap, nil
}

// fakeCompletionSink scripts EnqueueImmediateDeathCompletion
// and records every delivered completion for redelivery proof.
type fakeCompletionSink struct {
	mu     sync.Mutex
	calls  []sim.ImmediateDeathCompletion
	onCall func(n int, c sim.ImmediateDeathCompletion) (
		sim.EntitySnapshot, sim.DeathCompletionDisposition, error)
}

func (f *fakeCompletionSink) EnqueueImmediateDeathCompletion(
	_ context.Context, c sim.ImmediateDeathCompletion,
) (sim.EntitySnapshot, sim.DeathCompletionDisposition, error) {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	n := len(f.calls)
	on := f.onCall
	f.mu.Unlock()
	if on != nil {
		return on(n, c)
	}
	return sim.EntitySnapshot{}, sim.DeathCompletionApplied, nil
}

func (f *fakeCompletionSink) numCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// ---- shared fixtures -----------------------------------------

var errExecutorSynth = errors.New("test: synthetic post-commit error")

// executorWork builds valid executor work from the real sim
// capture pipeline: a Normal death with affected items 101 +
// 303 (PK-protected, character killer 99) and kept item 202.
func executorWork(t *testing.T) ImmediateDeathPersistenceWork {
	t.Helper()
	return ImmediateDeathPersistenceWork{
		Capture:       captureFixture(t, sim.DeathKillerIdentity{Kind: sim.DeathKillerCharacter, CharacterID: 99}),
		RuntimeInputs: testDeathRuntimeInputs(t),
	}
}

// trackExecutorRoots tracks the fixture participant roots
// (character 7 + affected items 101/303) at the given Saver
// revisions. Item 202 is kept inventory, never a participant.
func trackExecutorRoots(t *testing.T, s *sim.Saver, charRev, itemRev int64) {
	t.Helper()
	if err := s.Track(sim.AggregateKey{Kind: sim.AggregateCharacter, ID: 7}, charRev); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{101, 303} {
		if err := s.Track(sim.AggregateKey{Kind: sim.AggregateItem, ID: id}, itemRev); err != nil {
			t.Fatal(err)
		}
	}
}

// intendedReq maps the work capture to the frozen intended
// Store request (the proof reference).
func intendedReq(t *testing.T, work ImmediateDeathPersistenceWork) store.DeathEntryRequest {
	t.Helper()
	req, err := MapImmediateDeathCapture(work.Capture)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// expectedZero builds callback expected revisions E=0 for the
// fixture participant set.
func expectedZero() []sim.AggregateRevision {
	return []sim.AggregateRevision{
		{Key: sim.AggregateKey{Kind: sim.AggregateCharacter, ID: 7}, Revision: 0},
		{Key: sim.AggregateKey{Kind: sim.AggregateItem, ID: 101}, Revision: 0},
		{Key: sim.AggregateKey{Kind: sim.AggregateItem, ID: 303}, Revision: 0},
	}
}

// provenCharRec builds the recovered character snapshot for
// the intended request at Exactly expected+1 with the given
// pending shape. Deep-copied: later mutation of the recovered
// value (reordering, mutants) must never leak into intended.
func provenCharRec(req store.CharacterSnapshot, pending *store.PendingDeathSnapshot) store.DeathCharacterRecoverySnapshot {
	c := freezeCharacterSnapshot(req)
	c.ExpectedRevision = 1
	return store.DeathCharacterRecoverySnapshot{Character: c, Pending: pending}
}

// underworldPending builds the exact pending row for an
// Underworld-bound request (corpseID nil allowed: corpse may
// have expired via ON DELETE SET NULL).
func underworldPending(req store.DeathEntryRequest, corpseID *int64) *store.PendingDeathSnapshot {
	return &store.PendingDeathSnapshot{
		CharacterID:      req.Character.ID,
		EffectiveCost:    req.EffectiveDeathCost,
		DeathTimeSeconds: req.DeathTimeSeconds,
		CorpseID:         corpseID,
		PortalUsed:       false,
	}
}

// provenItemRec builds the recovered item snapshot for one
// intended item at exactly expected+1, with a PK row exactly
// when the intended duration is positive. Deep-copied like
// provenCharRec.
func provenItemRec(victimID int64, item store.DeathEntryItem) store.DeathItemRecoverySnapshot {
	s := freezeItemSnapshot(item.Snapshot)
	s.ExpectedRevision = 1
	var prot *store.ItemPKProtectionSnapshot
	if item.PKProtectionDuration > 0 {
		prot = &store.ItemPKProtectionSnapshot{
			ItemID:            s.ID,
			VictimCharacterID: victimID,
			ExpiresAt:         time.Now().Add(time.Hour).UTC(),
		}
	}
	return store.DeathItemRecoverySnapshot{Item: s, PKProtection: prot}
}

// respaceJSON re-encodes JSON with deliberately different
// whitespace and key order (map marshal sorts keys): proof
// must compare semantics, never raw bytes.
func respaceJSON(t *testing.T, data []byte) []byte {
	t.Helper()
	var u any
	if err := json.Unmarshal(data, &u); err != nil {
		t.Fatalf("respace unmarshal: %v", err)
	}
	out, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		t.Fatalf("respace marshal: %v", err)
	}
	return out
}

// startExecutor runs the executor and returns it with a cancel
// that also waits for Run to exit (no leak across -count=50).
func startExecutor(
	t *testing.T, cfg DeathExecutorConfig,
) (*DeathExecutor, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ex, err := NewDeathExecutor(cfg)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	runErr := make(chan error, 1)
	go func() { runErr <- ex.Run(ctx) }()
	<-ex.ready
	t.Cleanup(func() {
		cancel()
		<-runErr
	})
	return ex, cancel
}

// awaitResult waits for a result with a failure-mode timeout
// (never a pacing sleep: results are buffered, delivery is
// prompt; the timeout only fails loudly on bugs).
func awaitResult(t *testing.T, ch <-chan ImmediateDeathPersistenceResult) ImmediateDeathPersistenceResult {
	t.Helper()
	select {
	case res := <-ch:
		return res
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for executor result")
		return ImmediateDeathPersistenceResult{}
	}
}

// ---- observed commit core (required tests 1-3) ----------------

// An older queued Saver write advances a participant before
// the critical callback executes: observed Expected MUST carry
// the ACTUAL execution-time revisions, not pre-call values and
// not the hostile request placeholders.
func TestObservedDeathCallbackExpectedRevisions(t *testing.T) {
	s := mustSaverForPersist(t)
	trackDeathRoots(t, s, 5, map[int64]int64{20: 4, 10: 8})
	// Older queued write: character 5 -> 6 before the critical
	// operation runs.
	if _, err := s.WriteThrough(context.Background(),
		sim.AggregateKey{Kind: sim.AggregateCharacter, ID: 7},
		func(_ context.Context, exp int64) (int64, error) {
			if exp != 5 {
				return 0, fmt.Errorf("test writer saw %d, want pre-call 5", exp)
			}
			return exp + 1, nil
		}); err != nil {
		t.Fatalf("advance writer: %v", err)
	}
	fs := &fakeDeathStore{}
	req := deathTestReq() // hostile 999s, items [20, 10].
	exec, err := commitDeathEntryObserved(context.Background(), s, fs, req)
	if err != nil {
		t.Fatalf("observed commit: %v", err)
	}
	got := fs.gotDeath[0]
	if got.Character.ExpectedRevision != 6 {
		t.Fatalf("Store character revision = %d, want execution-time 6 (not pre-call 5, not hostile 999)",
			got.Character.ExpectedRevision)
	}
	want := map[sim.AggregateKey]int64{
		{Kind: sim.AggregateCharacter, ID: 7}: 6,
		{Kind: sim.AggregateItem, ID: 20}:     4,
		{Kind: sim.AggregateItem, ID: 10}:     8,
	}
	if len(exec.Expected) != len(want) {
		t.Fatalf("Expected = %+v, want exactly %d participants", exec.Expected, len(want))
	}
	for k, w := range want {
		found := false
		for _, e := range exec.Expected {
			if e.Key == k {
				found = true
				if e.Revision != w {
					t.Fatalf("Expected[%v] = %d, want %d", k, e.Revision, w)
				}
			}
		}
		if !found {
			t.Fatalf("Expected missing participant %v: %+v", k, exec.Expected)
		}
	}
	if exec.Result.CharacterRevision != 7 {
		t.Fatalf("result char rev = %d, want 7", exec.Result.CharacterRevision)
	}
}

// A semantic Store error from INSIDE the callback still
// retains the exact expected revisions, the public adapter
// still returns the ZERO Store result, and reconciliation
// behavior is unchanged (cause + ReconcileRequired, never
// misclassified as stale).
func TestObservedDeathCallbackErrorRetainsExpected(t *testing.T) {
	s := mustSaverForPersist(t)
	trackDeathRoots(t, s, 10, map[int64]int64{20: 4, 10: 8})
	semantic := fmt.Errorf("commit: %w", store.ErrDeathAlreadyPending)
	fs := &fakeDeathStore{onDeath: func(store.DeathEntryRequest) (store.DeathEntryResult, error) {
		return store.DeathEntryResult{}, semantic
	}}
	exec, err := commitDeathEntryObserved(context.Background(), s, fs, deathTestReq())
	if err == nil {
		t.Fatal("want semantic callback error")
	}
	if !errors.Is(err, store.ErrDeathAlreadyPending) {
		t.Fatalf("err = %v, want semantic cause", err)
	}
	if !errors.Is(err, sim.ErrSaverReconcileRequired) {
		t.Fatalf("err = %v, want ErrSaverReconcileRequired", err)
	}
	if errors.Is(err, sim.ErrSnapshotStale) {
		t.Fatalf("semantic error misclassified as stale: %v", err)
	}
	if !isZeroDeathEntryResult(exec.Result) {
		t.Fatalf("result = %+v, want zero (visibility fence)", exec.Result)
	}
	if len(exec.Expected) != 3 {
		t.Fatalf("Expected = %+v, want 3 retained revisions", exec.Expected)
	}
	// Public API compatibility: same zero-result behavior.
	res, err := CommitDeathEntry(context.Background(), s, fs, deathTestReq())
	if err == nil || !isZeroDeathEntryResult(res) {
		t.Fatalf("public res = %+v err = %v, want zero result + error", res, err)
	}
}

// Pre-callback rejection (duplicate aggregate keys) invokes NO
// Store call and invents NO revision metadata.
func TestObservedDeathPreCallbackNoExpected(t *testing.T) {
	s := mustSaverForPersist(t)
	trackDeathRoots(t, s, 10, map[int64]int64{20: 4})
	fs := &fakeDeathStore{}
	req := deathTestReq()
	req.Items = []store.DeathEntryItem{deathTestGroundItem(20, 999), deathTestGroundItem(20, 999)}
	exec, err := commitDeathEntryObserved(context.Background(), s, fs, req)
	if err == nil {
		t.Fatal("want duplicate-key rejection")
	}
	if fs.deathCalls != 0 {
		t.Fatalf("store calls = %d, want 0", fs.deathCalls)
	}
	if len(exec.Expected) != 0 {
		t.Fatalf("Expected = %+v, want empty (callback never invoked)", exec.Expected)
	}
	if errors.Is(err, sim.ErrSaverReconcileRequired) {
		t.Fatalf("pre-callback rejection must not add reconcile block: %v", err)
	}
	if got := inspectKnown(t, s, sim.AggregateCharacter, 7); got.Blocked || got.KnownRevision != 10 {
		t.Fatalf("saver char = %+v, want untouched known10", got)
	}
}

// ---- recovery proof (required tests 4-12) --------------------

// proofSetup maps the real fixture capture and builds the
// proven recovery state: all roots at exactly expected+1 (E=0
// -> rev 1), exact intended content, exact Underworld pending
// shape. JSON is deliberately respaced/reordered between
// intended and recovered to prove semantic comparison.
func proofSetup(t *testing.T) (store.DeathEntryRequest, deathRecovery) {
	t.Helper()
	work := executorWork(t)
	req := intendedReq(t, work)
	corpse := int64(4242)
	rec := deathRecovery{
		char: provenCharRec(req.Character, underworldPending(req, &corpse)),
	}
	for _, it := range req.Items {
		rec.items = append(rec.items, provenItemRec(req.Character.ID, it))
	}
	// Deliberate formatting divergence: semantics must still
	// compare equal.
	rec.char.Character.Vitals = respaceJSON(t, req.Character.Vitals)
	rec.char.Character.Advancement = respaceJSON(t, req.Character.Advancement)
	// Deliberate spell order divergence: comparison is by
	// durable catalog ID, never slice position.
	sp := rec.char.Character.Spells
	for i, j := 0, len(sp)-1; i < j; i, j = i+1, j-1 {
		sp[i], sp[j] = sp[j], sp[i]
	}
	for i := range rec.items {
		rec.items[i].Item.Enchants = respaceJSON(t, rec.items[i].Item.Enchants)
	}
	return req, rec
}

// Proven committed: every root exactly expected+1, exact
// content, exact pending shape -> proof succeeds, completion
// permitted.
func TestDeathRecoveryProofCommitted(t *testing.T) {
	req, rec := proofSetup(t)
	if len(req.Items) != 2 {
		t.Fatalf("fixture items = %d, want 2 affected (101, 303)", len(req.Items))
	}
	if err := proveDeathCommit(req, expectedZero(), rec, errExecutorSynth); err != nil {
		t.Fatalf("proven commit rejected: %v", err)
	}
}

// Character revision == expected: the transaction provably did
// NOT advance this root -> ErrDeathCommitUnproven, no
// completion.
func TestDeathRecoveryProofCharacterRevisionEqualsExpected(t *testing.T) {
	req, rec := proofSetup(t)
	rec.char.Character.ExpectedRevision = 0
	err := proveDeathCommit(req, expectedZero(), rec, errExecutorSynth)
	if !errors.Is(err, ErrDeathCommitUnproven) {
		t.Fatalf("err = %v, want ErrDeathCommitUnproven", err)
	}
	if !errors.Is(err, errExecutorSynth) {
		t.Fatalf("err = %v, want original commit cause preserved", err)
	}
}

// Character revision > expected+1: newer durable state exists;
// the original result cannot be authoritative -> fail closed.
func TestDeathRecoveryProofCharacterRevisionBeyondExpectedPlusOne(t *testing.T) {
	req, rec := proofSetup(t)
	rec.char.Character.ExpectedRevision = 2
	if err := proveDeathCommit(req, expectedZero(), rec, errExecutorSynth); !errors.Is(err, ErrDeathCommitUnproven) {
		t.Fatalf("err = %v, want ErrDeathCommitUnproven", err)
	}
}

// Item revision mismatch (one affected item stale) -> fail
// closed even though every other participant proves.
func TestDeathRecoveryProofItemRevisionMismatch(t *testing.T) {
	req, rec := proofSetup(t)
	for i := range rec.items {
		if rec.items[i].Item.ID == 303 {
			rec.items[i].Item.ExpectedRevision = 0
		}
	}
	if err := proveDeathCommit(req, expectedZero(), rec, errExecutorSynth); !errors.Is(err, ErrDeathCommitUnproven) {
		t.Fatalf("err = %v, want ErrDeathCommitUnproven", err)
	}
}

// Character semantic mismatch at exact expected+1: one altered
// field (karma, position, flags, ability, JSON value) ->
// unproven. Never merges the plan over differing state.
func TestDeathRecoveryProofCharacterSemanticMismatch(t *testing.T) {
	mutants := map[string]func(*store.DeathCharacterRecoverySnapshot){
		"karma": func(r *store.DeathCharacterRecoverySnapshot) { r.Character.Karma++ },
		"pos":   func(r *store.DeathCharacterRecoverySnapshot) { r.Character.PosX++ },
		"flags": func(r *store.DeathCharacterRecoverySnapshot) { r.Character.Flags ^= 0x10 },
		"spell": func(r *store.DeathCharacterRecoverySnapshot) { r.Character.Spells[0].Ability++ },
		"skill": func(r *store.DeathCharacterRecoverySnapshot) {
			r.Character.Skills[0].AtrophyFlag = !r.Character.Skills[0].AtrophyFlag
		},
		"vitals": func(r *store.DeathCharacterRecoverySnapshot) { r.Character.Vitals = json.RawMessage(`{"hp":9999}`) },
		"advancement": func(r *store.DeathCharacterRecoverySnapshot) {
			r.Character.Advancement = json.RawMessage(`{"adv_points":9999}`)
		},
	}
	for name, mutate := range mutants {
		t.Run(name, func(t *testing.T) {
			req, rec := proofSetup(t)
			mutate(&rec.char)
			if err := proveDeathCommit(req, expectedZero(), rec, errExecutorSynth); !errors.Is(err, ErrDeathCommitUnproven) {
				t.Fatalf("mutant %s: err = %v, want ErrDeathCommitUnproven", name, err)
			}
		})
	}
}

// Item semantic mismatch at exact expected+1 (qty, hits,
// location, enchants value) -> unproven.
func TestDeathRecoveryProofItemSemanticMismatch(t *testing.T) {
	mutants := map[string]func(*store.DeathItemRecoverySnapshot){
		"qty":      func(r *store.DeathItemRecoverySnapshot) { r.Item.Qty++ },
		"hits":     func(r *store.DeathItemRecoverySnapshot) { r.Item.Hits++ },
		"location": func(r *store.DeathItemRecoverySnapshot) { x := *r.Item.Location.PosX + 1; r.Item.Location.PosX = &x },
		"enchants": func(r *store.DeathItemRecoverySnapshot) { r.Item.Enchants = json.RawMessage(`{"glow":2}`) },
		"nilpos":   func(r *store.DeathItemRecoverySnapshot) { r.Item.Location.PosX = nil },
	}
	for name, mutate := range mutants {
		t.Run(name, func(t *testing.T) {
			req, rec := proofSetup(t)
			for i := range rec.items {
				if rec.items[i].Item.ID == 101 {
					mutate(&rec.items[i])
				}
			}
			if err := proveDeathCommit(req, expectedZero(), rec, errExecutorSynth); !errors.Is(err, ErrDeathCommitUnproven) {
				t.Fatalf("mutant %s: err = %v, want ErrDeathCommitUnproven", name, err)
			}
		})
	}
}

// Newbie-home pending rule: proven state requires Pending ==
// nil (positive and negative cases).
func TestDeathRecoveryProofNewbieHomePending(t *testing.T) {
	work := executorWork(t)
	work.Capture.Disposition.NewbieHomeRespawn = true
	work.Capture.Disposition.DeathCost = 0
	req := intendedReq(t, work)
	if !req.NewbieHomeRespawn {
		t.Fatal("want newbie-home request")
	}
	charRev := provenCharRec(req.Character, nil)
	items := []store.DeathItemRecoverySnapshot{}
	for _, it := range req.Items {
		items = append(items, provenItemRec(req.Character.ID, it))
	}
	if err := proveDeathCommit(req, expectedZero(),
		deathRecovery{char: charRev, items: items}, errExecutorSynth); err != nil {
		t.Fatalf("newbie-home nil pending rejected: %v", err)
	}
	bad := provenCharRec(req.Character, underworldPending(req, nil))
	if err := proveDeathCommit(req, expectedZero(),
		deathRecovery{char: bad, items: items}, errExecutorSynth); !errors.Is(err, ErrDeathCommitUnproven) {
		t.Fatalf("newbie-home with pending row: err = %v, want ErrDeathCommitUnproven", err)
	}
}

// Underworld pending rule: non-nil row with exact cost, exact
// death time, PortalUsed=false. CorpseID nil (expired corpse)
// is accepted — CorpseID is never a strict identity.
func TestDeathRecoveryProofUnderworldPending(t *testing.T) {
	req, rec := proofSetup(t)
	// Positive with nil CorpseID (corpse expired via ON DELETE
	// SET NULL while the pending row survives).
	rec.char.Pending = underworldPending(req, nil)
	if err := proveDeathCommit(req, expectedZero(), rec, errExecutorSynth); err != nil {
		t.Fatalf("underworld nil-corpse pending rejected: %v", err)
	}
	bad := map[string]*store.PendingDeathSnapshot{}
	pCost := *underworldPending(req, nil)
	pCost.EffectiveCost++
	bad["cost"] = &pCost
	pTime := *underworldPending(req, nil)
	pTime.DeathTimeSeconds++
	bad["deathtime"] = &pTime
	pPortal := *underworldPending(req, nil)
	pPortal.PortalUsed = true
	bad["portal"] = &pPortal
	pChar := *underworldPending(req, nil)
	pChar.CharacterID++
	bad["character"] = &pChar
	bad["nil"] = nil
	for name, p := range bad {
		r := rec
		r.char.Pending = p
		if err := proveDeathCommit(req, expectedZero(), r, errExecutorSynth); !errors.Is(err, ErrDeathCommitUnproven) {
			t.Fatalf("pending %s: err = %v, want ErrDeathCommitUnproven", name, err)
		}
	}
}

// Zero protection duration means "no new write", not "delete
// old protection": a recovered pre-existing protection row
// MUST NOT fail the proof on that ground alone.
func TestDeathRecoveryProofZeroProtectionPreservesOld(t *testing.T) {
	work := ImmediateDeathPersistenceWork{
		Capture:       captureFixture(t, sim.DeathKillerIdentity{Kind: sim.DeathKillerNone}),
		RuntimeInputs: testDeathRuntimeInputs(t),
	}
	req := intendedReq(t, work)
	for _, it := range req.Items {
		if it.PKProtectionDuration != 0 {
			t.Fatalf("item %d duration = %s, want 0 without player killer", it.Snapshot.ID, it.PKProtectionDuration)
		}
	}
	corpse := int64(99)
	rec := deathRecovery{char: provenCharRec(req.Character, underworldPending(req, &corpse))}
	for _, it := range req.Items {
		s := it.Snapshot
		s.ExpectedRevision = 1
		rec.items = append(rec.items, store.DeathItemRecoverySnapshot{
			Item: s,
			PKProtection: &store.ItemPKProtectionSnapshot{
				ItemID:            s.ID,
				VictimCharacterID: req.Character.ID,
				ExpiresAt:         time.Now().Add(-time.Hour).UTC(),
			},
		})
	}
	if err := proveDeathCommit(req, expectedZero(), rec, errExecutorSynth); err != nil {
		t.Fatalf("pre-existing protection with zero duration rejected: %v", err)
	}
}

// ---- executor (required tests 13-23) -------------------------

// Normal commit success: one Store CommitDeathEntry, zero
// recovery loads, one owner completion, Applied accepted,
// Recovered=false.
func TestDeathExecutorNormalCommit(t *testing.T) {
	s := mustSaverForPersist(t)
	trackExecutorRoots(t, s, 0, 0)
	fs := newFakeRecoveryStore()
	sink := &fakeCompletionSink{}
	work := executorWork(t)
	ex, _ := startExecutor(t, DeathExecutorConfig{
		Workers: 2, QueueCapacity: 8, Store: fs, Saver: s, Sink: sink,
	})
	resCh, err := ex.TrySubmit(work)
	if err != nil {
		t.Fatalf("TrySubmit: %v", err)
	}
	res := awaitResult(t, resCh)
	if res.Err != nil {
		t.Fatalf("result err = %v", res.Err)
	}
	if res.Recovered {
		t.Fatal("Recovered = true, want false for normal ack")
	}
	if res.Delivery != sim.DeathCompletionApplied {
		t.Fatalf("delivery = %v, want Applied", res.Delivery)
	}
	if fs.deathCalls != 1 {
		t.Fatalf("store calls = %d, want 1", fs.deathCalls)
	}
	if fs.charCalls != 0 || len(fs.itemCalls) != 0 {
		t.Fatalf("recovery loads char=%d items=%v, want zero", fs.charCalls, fs.itemCalls)
	}
	if sink.numCalls() != 1 {
		t.Fatalf("sink calls = %d, want 1", sink.numCalls())
	}
	if sink.calls[0].Token != work.Capture.Token {
		t.Fatalf("completion token = %+v, want %+v", sink.calls[0].Token, work.Capture.Token)
	}
	if got := inspectKnown(t, s, sim.AggregateCharacter, 7); got.KnownRevision != 1 || got.Blocked {
		t.Fatalf("saver char = %+v, want known1 clean", got)
	}
}

// Owner Duplicate is ALSO success: the expected idempotent
// redelivery result, no Store replay.
func TestDeathExecutorDuplicateAccepted(t *testing.T) {
	s := mustSaverForPersist(t)
	trackExecutorRoots(t, s, 0, 0)
	fs := newFakeRecoveryStore()
	sink := &fakeCompletionSink{onCall: func(int, sim.ImmediateDeathCompletion) (
		sim.EntitySnapshot, sim.DeathCompletionDisposition, error) {
		return sim.EntitySnapshot{}, sim.DeathCompletionDuplicate, nil
	}}
	ex, _ := startExecutor(t, DeathExecutorConfig{
		Workers: 1, QueueCapacity: 4, Store: fs, Saver: s, Sink: sink,
	})
	res := awaitResult(t, mustSubmit(t, ex, executorWork(t)))
	if res.Err != nil {
		t.Fatalf("result err = %v", res.Err)
	}
	if res.Delivery != sim.DeathCompletionDuplicate {
		t.Fatalf("delivery = %v, want Duplicate", res.Delivery)
	}
	if fs.deathCalls != 1 || sink.numCalls() != 1 {
		t.Fatalf("store=%d sink=%d, want 1/1 (no replay)", fs.deathCalls, sink.numCalls())
	}
}

func mustSubmit(
	t *testing.T, ex *DeathExecutor, work ImmediateDeathPersistenceWork,
) <-chan ImmediateDeathPersistenceResult {
	t.Helper()
	resCh, err := ex.TrySubmit(work)
	if err != nil {
		t.Fatalf("TrySubmit: %v", err)
	}
	return resCh
}

// Lost acknowledgement, proven committed: the REAL ambiguity
// shape — Store effects materialized, the adapter saw only a
// synthetic post-commit error with ReconcileRequired. Every
// affected participant recovers exactly once, every Saver
// participant reconciles, proof succeeds, ZERO replay, one
// owner completion, Recovered=true.
func TestDeathExecutorLostAckProven(t *testing.T) {
	s := mustSaverForPersist(t)
	trackExecutorRoots(t, s, 0, 0)
	work := executorWork(t)
	intended := intendedReq(t, work)
	fs := newFakeRecoveryStore()
	fs.onDeath = func(store.DeathEntryRequest) (store.DeathEntryResult, error) {
		return store.DeathEntryResult{}, errExecutorSynth
	}
	corpse := int64(777000)
	fs.charSnap = provenCharRec(intended.Character, underworldPending(intended, &corpse))
	for _, it := range intended.Items {
		fs.itemSnaps[it.Snapshot.ID] = provenItemRec(intended.Character.ID, it)
	}
	sink := &fakeCompletionSink{}
	ex, _ := startExecutor(t, DeathExecutorConfig{
		Workers: 2, QueueCapacity: 8, Store: fs, Saver: s, Sink: sink,
	})
	res := awaitResult(t, mustSubmit(t, ex, work))
	if res.Err != nil {
		t.Fatalf("result err = %v, want proven success", res.Err)
	}
	if !res.Recovered {
		t.Fatal("Recovered = false, want true for proven lost-ack")
	}
	if res.Delivery != sim.DeathCompletionApplied {
		t.Fatalf("delivery = %v, want Applied", res.Delivery)
	}
	if fs.deathCalls != 1 {
		t.Fatalf("store calls = %d, want exactly 1 (no replay)", fs.deathCalls)
	}
	if fs.charCalls != 1 {
		t.Fatalf("character recoveries = %d, want 1", fs.charCalls)
	}
	for _, id := range []int64{101, 303} {
		if fs.itemCalls[id] != 1 {
			t.Fatalf("item %d recoveries = %d, want 1", id, fs.itemCalls[id])
		}
		if got := inspectKnown(t, s, sim.AggregateItem, id); got.KnownRevision != 1 || got.Blocked {
			t.Fatalf("saver item %d = %+v, want known1 clean", id, got)
		}
	}
	if got := inspectKnown(t, s, sim.AggregateCharacter, 7); got.KnownRevision != 1 || got.Blocked {
		t.Fatalf("saver char = %+v, want known1 clean", got)
	}
	if sink.numCalls() != 1 {
		t.Fatalf("sink calls = %d, want 1", sink.numCalls())
	}
}

// Stale: recovered state proves the death transaction did NOT
// commit. Saver recovery still occurs, but the result is
// ErrDeathCommitUnproven with zero replay and zero completion.
func TestDeathExecutorStaleUnproven(t *testing.T) {
	s := mustSaverForPersist(t)
	trackExecutorRoots(t, s, 0, 0)
	work := executorWork(t)
	intended := intendedReq(t, work)
	fs := newFakeRecoveryStore()
	fs.onDeath = func(store.DeathEntryRequest) (store.DeathEntryResult, error) {
		return store.DeathEntryResult{}, fmt.Errorf("commit: %w", store.ErrStaleRevision)
	}
	// PG still holds the pre-death revision: recovery loads
	// rev == expected, so THIS transaction provably did not
	// commit.
	staleChar := intended.Character // rev stays 0
	fs.charSnap = store.DeathCharacterRecoverySnapshot{
		Character: staleChar, Pending: nil,
	}
	for _, it := range intended.Items {
		fs.itemSnaps[it.Snapshot.ID] = store.DeathItemRecoverySnapshot{Item: it.Snapshot}
	}
	sink := &fakeCompletionSink{}
	ex, _ := startExecutor(t, DeathExecutorConfig{
		Workers: 1, QueueCapacity: 4, Store: fs, Saver: s, Sink: sink,
	})
	res := awaitResult(t, mustSubmit(t, ex, work))
	if !errors.Is(res.Err, ErrDeathCommitUnproven) {
		t.Fatalf("err = %v, want ErrDeathCommitUnproven", res.Err)
	}
	if fs.deathCalls != 1 {
		t.Fatalf("store calls = %d, want 1 (no replay)", fs.deathCalls)
	}
	if sink.numCalls() != 0 {
		t.Fatalf("sink calls = %d, want 0", sink.numCalls())
	}
	// Saver recovery still occurred: clean at the authoritative
	// pre-death revision.
	if got := inspectKnown(t, s, sim.AggregateCharacter, 7); got.Blocked || got.KnownRevision != 0 {
		t.Fatalf("saver char = %+v, want reconciled known0", got)
	}
}

// Semantic Store error after callback invocation: recovery
// runs because the Saver requires it, but recovered state does
// not prove THIS transaction -> unproven with the original
// semantic error preserved in the cause chain.
func TestDeathExecutorSemanticErrorUnproven(t *testing.T) {
	s := mustSaverForPersist(t)
	trackExecutorRoots(t, s, 0, 0)
	work := executorWork(t)
	intended := intendedReq(t, work)
	fs := newFakeRecoveryStore()
	fs.onDeath = func(store.DeathEntryRequest) (store.DeathEntryResult, error) {
		return store.DeathEntryResult{}, fmt.Errorf("commit: %w", store.ErrDeathAlreadyPending)
	}
	// An older pending row (different cost) proves another
	// state is authoritative, not this attempt.
	pending := underworldPending(intended, nil)
	pending.EffectiveCost++
	fs.charSnap = provenCharRec(intended.Character, pending)
	for _, it := range intended.Items {
		r := provenItemRec(intended.Character.ID, it)
		fs.itemSnaps[it.Snapshot.ID] = r
	}
	sink := &fakeCompletionSink{}
	ex, _ := startExecutor(t, DeathExecutorConfig{
		Workers: 1, QueueCapacity: 4, Store: fs, Saver: s, Sink: sink,
	})
	res := awaitResult(t, mustSubmit(t, ex, work))
	if !errors.Is(res.Err, ErrDeathCommitUnproven) {
		t.Fatalf("err = %v, want ErrDeathCommitUnproven", res.Err)
	}
	if !errors.Is(res.Err, store.ErrDeathAlreadyPending) {
		t.Fatalf("err = %v, want original semantic cause preserved", res.Err)
	}
	if fs.deathCalls != 1 || sink.numCalls() != 0 {
		t.Fatalf("store=%d sink=%d, want 1/0 (no replay, no completion)", fs.deathCalls, sink.numCalls())
	}
}

// One recovery loader fails across multiple participants: no
// completion, zero replay, definitive error, no worker leak —
// while the deterministic pass still attempts every other
// participant.
func TestDeathExecutorRecoveryLoaderFailure(t *testing.T) {
	s := mustSaverForPersist(t)
	trackExecutorRoots(t, s, 0, 0)
	work := executorWork(t)
	intended := intendedReq(t, work)
	fs := newFakeRecoveryStore()
	fs.onDeath = func(store.DeathEntryRequest) (store.DeathEntryResult, error) {
		return store.DeathEntryResult{}, errExecutorSynth
	}
	loaderBoom := errors.New("test: item recovery IO failure")
	fs.charSnap = provenCharRec(intended.Character, underworldPending(intended, nil))
	for _, it := range intended.Items {
		fs.itemSnaps[it.Snapshot.ID] = provenItemRec(intended.Character.ID, it)
	}
	fs.itemErrs[303] = loaderBoom
	sink := &fakeCompletionSink{}
	ex, _ := startExecutor(t, DeathExecutorConfig{
		Workers: 1, QueueCapacity: 4, Store: fs, Saver: s, Sink: sink,
	})
	res := awaitResult(t, mustSubmit(t, ex, work))
	if res.Err == nil || errors.Is(res.Err, ErrDeathCommitUnproven) {
		t.Fatalf("err = %v, want loader failure (not unproven)", res.Err)
	}
	if !errors.Is(res.Err, loaderBoom) {
		t.Fatalf("err = %v, want loader cause", res.Err)
	}
	if fs.deathCalls != 1 || sink.numCalls() != 0 {
		t.Fatalf("store=%d sink=%d, want 1/0", fs.deathCalls, sink.numCalls())
	}
	if fs.charCalls != 1 || fs.itemCalls[101] != 1 || fs.itemCalls[303] != 1 {
		t.Fatalf("recovery attempts char=%d items=%v, want every participant attempted",
			fs.charCalls, fs.itemCalls)
	}
}

// Owner mailbox saturation then success: the transaction is
// already authoritative, so the SAME completion/token is
// redelivered with exactly one Store call and no new goroutine
// per retry.
func TestDeathExecutorIngressFullRetry(t *testing.T) {
	s := mustSaverForPersist(t)
	trackExecutorRoots(t, s, 0, 0)
	fs := newFakeRecoveryStore()
	sink := &fakeCompletionSink{onCall: func(n int, _ sim.ImmediateDeathCompletion) (
		sim.EntitySnapshot, sim.DeathCompletionDisposition, error) {
		if n < 3 {
			return sim.EntitySnapshot{}, sim.DeathCompletionDisposition(0), sim.ErrSimIngressFull
		}
		return sim.EntitySnapshot{}, sim.DeathCompletionApplied, nil
	}}
	work := executorWork(t)
	ex, _ := startExecutor(t, DeathExecutorConfig{
		Workers: 1, QueueCapacity: 4, Store: fs, Saver: s, Sink: sink, RetryDelay: time.Millisecond,
	})
	res := awaitResult(t, mustSubmit(t, ex, work))
	if res.Err != nil {
		t.Fatalf("result err = %v, want eventual success", res.Err)
	}
	if fs.deathCalls != 1 {
		t.Fatalf("store calls = %d, want exactly 1", fs.deathCalls)
	}
	if sink.numCalls() != 3 {
		t.Fatalf("sink calls = %d, want 3 (full, full, applied)", sink.numCalls())
	}
	for i := 1; i < 3; i++ {
		if sink.calls[i].Token != sink.calls[0].Token {
			t.Fatalf("redelivery %d token drifted: %+v vs %+v", i, sink.calls[i].Token, sink.calls[0].Token)
		}
	}
}

// Engine stopped / not running (and attempt mismatch): NO
// delivery retry loop, Store not replayed, definitive result
// error.
func TestDeathExecutorEngineStopTerminal(t *testing.T) {
	for _, termErr := range []error{
		sim.ErrEngineNotRunning, sim.ErrEngineStopped, sim.ErrDeathAttemptMismatch,
	} {
		t.Run(termErr.Error(), func(t *testing.T) {
			s := mustSaverForPersist(t)
			trackExecutorRoots(t, s, 0, 0)
			fs := newFakeRecoveryStore()
			sink := &fakeCompletionSink{onCall: func(int, sim.ImmediateDeathCompletion) (
				sim.EntitySnapshot, sim.DeathCompletionDisposition, error) {
				return sim.EntitySnapshot{}, sim.DeathCompletionDisposition(0), termErr
			}}
			ex, _ := startExecutor(t, DeathExecutorConfig{
				Workers: 1, QueueCapacity: 4, Store: fs, Saver: s, Sink: sink,
			})
			res := awaitResult(t, mustSubmit(t, ex, executorWork(t)))
			if !errors.Is(res.Err, termErr) {
				t.Fatalf("err = %v, want terminal %v", res.Err, termErr)
			}
			if fs.deathCalls != 1 || sink.numCalls() != 1 {
				t.Fatalf("store=%d sink=%d, want 1/1 (no replay, no retry)",
					fs.deathCalls, sink.numCalls())
			}
		})
	}
}

// Submission deep-freeze: caller mutation of capture Durable
// bytes/slices, affected items, vitals, and runtime inputs
// AFTER successful submission cannot reach the Store request
// or the owner completion.
func TestDeathExecutorSubmitFreeze(t *testing.T) {
	s := mustSaverForPersist(t)
	trackExecutorRoots(t, s, 0, 0)
	work := executorWork(t)
	intended := intendedReq(t, work)
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	fs := newFakeRecoveryStore()
	fs.onDeath = func(req store.DeathEntryRequest) (store.DeathEntryResult, error) {
		once.Do(func() { close(entered) })
		<-release
		return echoDeathResult(req), nil
	}
	sink := &fakeCompletionSink{}
	ex, _ := startExecutor(t, DeathExecutorConfig{
		Workers: 1, QueueCapacity: 4, Store: fs, Saver: s, Sink: sink,
	})
	resCh, err := ex.TrySubmit(work)
	if err != nil {
		t.Fatalf("TrySubmit: %v", err)
	}
	<-entered
	// Hostile post-submit mutation of every caller-aliased
	// surface.
	work.Capture.Durable.Advancement[0] ^= 0xFF
	work.Capture.Durable.Spells[0].Ability++
	work.Capture.Durable.Skills[0].Ability++
	work.Capture.Durable.Items[0].Enchants[0] ^= 0xFF
	work.Capture.AffectedItems[0].Qty++
	work.Capture.Vitals.HP++
	work.RuntimeInputs.EffectiveStamina = 70
	close(release)
	res := awaitResult(t, resCh)
	if res.Err != nil {
		t.Fatalf("result err = %v", res.Err)
	}
	got := fs.gotDeath[0]
	if string(got.Character.Advancement) != string(intended.Character.Advancement) {
		t.Fatal("Store advancement mutated by caller post-submit")
	}
	if got.Character.Spells[0] != intended.Character.Spells[0] {
		t.Fatal("Store spells mutated by caller post-submit")
	}
	if got.Items[0].Snapshot.Qty != intended.Items[0].Snapshot.Qty {
		t.Fatal("Store item qty mutated by caller post-submit")
	}
	delivered := sink.calls[0]
	if string(delivered.Durable.Advancement) != string(intended.Character.Advancement) {
		t.Fatal("completion advancement mutated by caller post-submit")
	}
	if delivered.RuntimeInputs != testDeathRuntimeInputs(t) {
		t.Fatalf("completion runtime inputs = %+v, want frozen original", delivered.RuntimeInputs)
	}
	if delivered.Vitals.HP != work.Capture.Vitals.HP-1 {
		t.Fatal("completion vitals mutated by caller post-submit")
	}
}

// blockingDeathStore gates Store entry on test-controlled
// channels while honoring context cancellation (running work
// receives the cancelled context on shutdown).
type blockingDeathStore struct {
	*fakeRecoveryStore
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	calls   int
}

var _ DeathExecutionStore = (*blockingDeathStore)(nil)

func (b *blockingDeathStore) CommitDeathEntry(
	ctx context.Context, req store.DeathEntryRequest,
) (store.DeathEntryResult, error) {
	b.calls++
	b.once.Do(func() { close(b.entered) })
	select {
	case <-ctx.Done():
		return store.DeathEntryResult{}, ctx.Err()
	case <-b.release:
		return echoDeathResult(req), nil
	}
}

// Queue bound: with the worker blocked and the queue full,
// the next submission fails immediately with the stable full
// error and executes no extra work.
func TestDeathExecutorQueueBound(t *testing.T) {
	s := mustSaverForPersist(t)
	trackExecutorRoots(t, s, 0, 0)
	bs := &blockingDeathStore{
		fakeRecoveryStore: newFakeRecoveryStore(),
		entered:           make(chan struct{}),
		release:           make(chan struct{}),
	}
	sink := &fakeCompletionSink{}
	ex, _ := startExecutor(t, DeathExecutorConfig{
		Workers: 1, QueueCapacity: 1, Store: bs, Saver: s, Sink: sink,
	})
	resA := mustSubmit(t, ex, executorWork(t))
	<-bs.entered
	resB := mustSubmit(t, ex, executorWork(t))
	if _, err := ex.TrySubmit(executorWork(t)); !errors.Is(err, ErrDeathExecutorQueueFull) {
		t.Fatalf("err = %v, want ErrDeathExecutorQueueFull", err)
	}
	close(bs.release)
	if res := awaitResult(t, resA); res.Err != nil {
		t.Fatalf("A err = %v", res.Err)
	}
	if res := awaitResult(t, resB); res.Err != nil {
		t.Fatalf("B err = %v", res.Err)
	}
	if bs.calls != 2 {
		t.Fatalf("store calls = %d, want 2 (rejected submit executed nothing)", bs.calls)
	}
}

// Shutdown: a running job plus queued jobs, then executor
// cancel — every result channel receives a definitive terminal
// result, no waiter strands, no worker leaks.
func TestDeathExecutorShutdown(t *testing.T) {
	s := mustSaverForPersist(t)
	trackExecutorRoots(t, s, 0, 0)
	bs := &blockingDeathStore{
		fakeRecoveryStore: newFakeRecoveryStore(),
		entered:           make(chan struct{}),
		release:           make(chan struct{}),
	}
	sink := &fakeCompletionSink{}
	ctx, cancel := context.WithCancel(context.Background())
	ex, err := NewDeathExecutor(DeathExecutorConfig{
		Workers: 1, QueueCapacity: 2, Store: bs, Saver: s, Sink: sink,
	})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	runErr := make(chan error, 1)
	go func() { runErr <- ex.Run(ctx) }()
	<-ex.ready
	resA := mustSubmit(t, ex, executorWork(t))
	<-bs.entered
	resB := mustSubmit(t, ex, executorWork(t))
	cancel()
	res := awaitResult(t, resA)
	if res.Err == nil {
		t.Fatal("running job err = nil, want definitive shutdown error")
	}
	if res := awaitResult(t, resB); res.Err == nil {
		t.Fatal("queued job err = nil, want definitive shutdown error")
	}
	if err := <-runErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("run err = %v, want context.Canceled", err)
	}
	if sink.numCalls() != 0 {
		t.Fatalf("sink calls = %d, want 0 (no completion after shutdown)", sink.numCalls())
	}
}

// Executor configuration validation: worker count, queue
// capacity, and required dependencies are rejected with the
// stable invalid error; submission before Run fails with the
// stable not-running error; invalid work fails closed.
func TestDeathExecutorConfigValidation(t *testing.T) {
	s := mustSaverForPersist(t)
	fs := newFakeRecoveryStore()
	sink := &fakeCompletionSink{}
	good := DeathExecutorConfig{Workers: 1, QueueCapacity: 1, Store: fs, Saver: s, Sink: sink}
	for name, mutate := range map[string]func(*DeathExecutorConfig){
		"workers":  func(c *DeathExecutorConfig) { c.Workers = 0 },
		"capacity": func(c *DeathExecutorConfig) { c.QueueCapacity = 0 },
		"store":    func(c *DeathExecutorConfig) { c.Store = nil },
		"saver":    func(c *DeathExecutorConfig) { c.Saver = nil },
		"sink":     func(c *DeathExecutorConfig) { c.Sink = nil },
	} {
		cfg := good
		mutate(&cfg)
		if _, err := NewDeathExecutor(cfg); !errors.Is(err, ErrDeathExecutorInvalid) {
			t.Fatalf("%s: err = %v, want ErrDeathExecutorInvalid", name, err)
		}
	}
	ex, err := NewDeathExecutor(good)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ex.TrySubmit(executorWork(t)); !errors.Is(err, ErrDeathExecutorNotRunning) {
		t.Fatalf("pre-Run submit err = %v, want ErrDeathExecutorNotRunning", err)
	}
	badInputs := executorWork(t)
	badInputs.RuntimeInputs = sim.PlayerVitalsRuntimeInputs{}
	if _, err := ex.TrySubmit(badInputs); !errors.Is(err, ErrDeathExecutorInvalid) {
		t.Fatalf("bad runtime inputs err = %v, want ErrDeathExecutorInvalid", err)
	}
	badCapture := executorWork(t)
	badCapture.Capture.Token.CharacterID = 0
	if _, err := ex.TrySubmit(badCapture); !errors.Is(err, ErrDeathExecutorInvalid) {
		t.Fatalf("bad capture err = %v, want ErrDeathExecutorInvalid", err)
	}
}
