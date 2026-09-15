package persist

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/store"
)

// M5-T5c2b adapter unit tests (spec §9.5.1c): execution-time
// revision injection, immutable request capture, pre-callback
// rejection, stale translation, semantic-error preservation,
// and the Saver-result visibility fence. No PG; a scriptable
// fake DeathPersistenceStore inspects exact Store requests.

type fakeDeathStore struct {
	deathCalls, portalCalls, penaltyCalls int
	gotDeath                              []store.DeathEntryRequest
	gotPortal                             []store.PortalOfLifeRequest
	gotPenalty                            []store.DeathPenaltiesRequest
	onDeath                               func(store.DeathEntryRequest) (store.DeathEntryResult, error)
	onPortal                              func(store.PortalOfLifeRequest) (store.PortalOfLifeResult, error)
	onPenalty                             func(store.DeathPenaltiesRequest) (store.DeathPenaltiesResult, error)
}

func echoDeathResult(req store.DeathEntryRequest) store.DeathEntryResult {
	revs := make([]store.DeathEntryItemRevision, 0, len(req.Items))
	for _, it := range req.Items {
		revs = append(revs, store.DeathEntryItemRevision{
			ItemID: it.Snapshot.ID, Revision: it.Snapshot.ExpectedRevision + 1,
		})
	}
	return store.DeathEntryResult{
		CharacterRevision: req.Character.ExpectedRevision + 1,
		CorpseID:          123,
		ItemRevisions:     revs,
	}
}

func (f *fakeDeathStore) CommitDeathEntry(_ context.Context, req store.DeathEntryRequest) (store.DeathEntryResult, error) {
	f.deathCalls++
	f.gotDeath = append(f.gotDeath, req)
	if f.onDeath != nil {
		return f.onDeath(req)
	}
	return echoDeathResult(req), nil
}

func (f *fakeDeathStore) CommitPortalOfLife(_ context.Context, req store.PortalOfLifeRequest) (store.PortalOfLifeResult, error) {
	f.portalCalls++
	f.gotPortal = append(f.gotPortal, req)
	if f.onPortal != nil {
		return f.onPortal(req)
	}
	return store.PortalOfLifeResult{
		CharacterRevision: req.Character.ExpectedRevision + 1,
		EffectiveCost:     40,
	}, nil
}

func (f *fakeDeathStore) CommitDeathPenalties(_ context.Context, req store.DeathPenaltiesRequest) (store.DeathPenaltiesResult, error) {
	f.penaltyCalls++
	f.gotPenalty = append(f.gotPenalty, req)
	if f.onPenalty != nil {
		return f.onPenalty(req)
	}
	return store.DeathPenaltiesResult{CharacterRevision: req.Character.ExpectedRevision + 1}, nil
}

const (
	deathTestX = int64(700)
	deathTestY = int64(800)
	deathTestZ = int64(900)
)

func deathTestChar(id, hostileRev int64) store.CharacterSnapshot {
	return store.CharacterSnapshot{
		ID: id, ExpectedRevision: hostileRev,
		Karma: 3, PosX: 1, PosY: 2, PosZ: 3,
		Vitals:      json.RawMessage(`{"hp":1}`),
		Advancement: json.RawMessage(`{"pts":0}`),
		Flags:       3,
		Spells:      []store.CharacterSpellSnapshot{{SpellID: 1, Ability: 10}},
		Skills:      []store.CharacterSkillSnapshot{{SkillID: 2, Ability: 20}},
	}
}

func deathTestGroundItem(id, hostileRev int64) store.DeathEntryItem {
	x, y, z := deathTestX, deathTestY, deathTestZ
	return store.DeathEntryItem{
		Snapshot: store.ItemSnapshot{
			ID: id, ExpectedRevision: hostileRev,
			Qty: 5, Hits: 77, Enchants: json.RawMessage(`{"e":1}`),
			Location: store.ItemLocationSnapshot{Kind: 1, PosX: &x, PosY: &y, PosZ: &z},
		},
	}
}

func deathTestReq() store.DeathEntryRequest {
	return store.DeathEntryRequest{
		Character:          deathTestChar(7, 999),
		DeathPosX:          deathTestX,
		DeathPosY:          deathTestY,
		DeathPosZ:          deathTestZ,
		EffectiveDeathCost: 40,
		DeathTimeSeconds:   1700000000,
		CorpseLifetime:     time.Minute,
		Items:              []store.DeathEntryItem{deathTestGroundItem(20, 999), deathTestGroundItem(10, 999)},
	}
}

func trackDeathRoots(t *testing.T, s *sim.Saver, charRev int64, itemRevs map[int64]int64) {
	t.Helper()
	if err := s.Track(sim.AggregateKey{Kind: sim.AggregateCharacter, ID: 7}, charRev); err != nil {
		t.Fatal(err)
	}
	for id, rev := range itemRevs {
		if err := s.Track(sim.AggregateKey{Kind: sim.AggregateItem, ID: id}, rev); err != nil {
			t.Fatal(err)
		}
	}
}

func isZeroDeathEntryResult(res store.DeathEntryResult) bool {
	return res.CharacterRevision == 0 && res.CorpseID == 0 && len(res.ItemRevisions) == 0
}

func inspectKnown(t *testing.T, s *sim.Saver, kind sim.AggregateKind, id int64) sim.SaverEntrySnapshot {
	t.Helper()
	snap, err := s.Inspect(sim.AggregateKey{Kind: kind, ID: id})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// Execution-time revision injection: hostile input revisions
// are ignored; the Store receives Saver-known revisions even
// with reverse/noncanonical request item order.
func TestCommitDeathEntryExecutionTimeRevisions(t *testing.T) {
	s := mustSaverForPersist(t)
	trackDeathRoots(t, s, 10, map[int64]int64{20: 4, 10: 8})
	fs := &fakeDeathStore{}
	req := deathTestReq() // items [20, 10]: reverse of canonical [10, 20].
	res, err := CommitDeathEntry(context.Background(), s, fs, req)
	if err != nil {
		t.Fatalf("CommitDeathEntry: %v", err)
	}
	if fs.deathCalls != 1 {
		t.Fatalf("store calls = %d, want 1", fs.deathCalls)
	}
	got := fs.gotDeath[0]
	if got.Character.ExpectedRevision != 10 {
		t.Fatalf("character ExpectedRevision = %d, want Saver-known 10", got.Character.ExpectedRevision)
	}
	for _, it := range got.Items {
		want := map[int64]int64{20: 4, 10: 8}[it.Snapshot.ID]
		if it.Snapshot.ExpectedRevision != want {
			t.Fatalf("item %d ExpectedRevision = %d, want %d", it.Snapshot.ID, it.Snapshot.ExpectedRevision, want)
		}
	}
	if res.CharacterRevision != 11 || res.CorpseID != 123 {
		t.Fatalf("result = %+v, want char rev 11 corpse 123", res)
	}
	revByID := map[int64]int64{}
	for _, ir := range res.ItemRevisions {
		revByID[ir.ItemID] = ir.Revision
	}
	if revByID[20] != 5 || revByID[10] != 9 || len(res.ItemRevisions) != 2 {
		t.Fatalf("item revisions = %+v, want 20->5 10->9", res.ItemRevisions)
	}
	if got := inspectKnown(t, s, sim.AggregateCharacter, 7); got.KnownRevision != 11 || got.Blocked {
		t.Fatalf("saver char = %+v, want known11 clean", got)
	}
	if got := inspectKnown(t, s, sim.AggregateItem, 20); got.KnownRevision != 5 || got.Blocked {
		t.Fatalf("saver item20 = %+v, want known5 clean", got)
	}
	if got := inspectKnown(t, s, sim.AggregateItem, 10); got.KnownRevision != 9 || got.Blocked {
		t.Fatalf("saver item10 = %+v, want known9 clean", got)
	}
	// Caller request order/content unchanged.
	if len(req.Items) != 2 || req.Items[0].Snapshot.ID != 20 || req.Items[1].Snapshot.ID != 10 {
		t.Fatalf("caller item order mutated: %+v", req.Items)
	}
	if req.Character.ExpectedRevision != 999 || req.Items[0].Snapshot.ExpectedRevision != 999 {
		t.Fatal("caller ExpectedRevision fields mutated")
	}
}

func TestCommitPortalOfLifeExecutionTimeRevision(t *testing.T) {
	s := mustSaverForPersist(t)
	if err := s.Track(sim.AggregateKey{Kind: sim.AggregateCharacter, ID: 7}, 6); err != nil {
		t.Fatal(err)
	}
	fs := &fakeDeathStore{}
	req := store.PortalOfLifeRequest{Character: deathTestChar(7, 0), CorpseID: 123, ProposedCost: 20}
	res, err := CommitPortalOfLife(context.Background(), s, fs, req)
	if err != nil {
		t.Fatalf("CommitPortalOfLife: %v", err)
	}
	if fs.portalCalls != 1 || fs.gotPortal[0].Character.ExpectedRevision != 6 {
		t.Fatalf("store got %+v, want ExpectedRevision 6", fs.gotPortal)
	}
	if fs.gotPortal[0].CorpseID != 123 || fs.gotPortal[0].ProposedCost != 20 {
		t.Fatalf("portal scalars altered: %+v", fs.gotPortal[0])
	}
	if res.CharacterRevision != 7 || res.EffectiveCost != 40 {
		t.Fatalf("result = %+v, want rev7 cost40", res)
	}
	if got := inspectKnown(t, s, sim.AggregateCharacter, 7); got.KnownRevision != 7 || got.Blocked {
		t.Fatalf("saver = %+v, want known7 clean", got)
	}
	if req.Character.ExpectedRevision != 0 {
		t.Fatal("caller ExpectedRevision mutated")
	}
}

func TestCommitDeathPenaltiesExecutionTimeRevision(t *testing.T) {
	s := mustSaverForPersist(t)
	if err := s.Track(sim.AggregateKey{Kind: sim.AggregateCharacter, ID: 7}, 2); err != nil {
		t.Fatal(err)
	}
	fs := &fakeDeathStore{}
	req := store.DeathPenaltiesRequest{Character: deathTestChar(7, 0), ExpectedPendingCost: 40}
	res, err := CommitDeathPenalties(context.Background(), s, fs, req)
	if err != nil {
		t.Fatalf("CommitDeathPenalties: %v", err)
	}
	if fs.penaltyCalls != 1 || fs.gotPenalty[0].Character.ExpectedRevision != 2 {
		t.Fatalf("store got %+v, want ExpectedRevision 2", fs.gotPenalty)
	}
	if fs.gotPenalty[0].ExpectedPendingCost != 40 {
		t.Fatalf("raw ExpectedPendingCost altered: %+v", fs.gotPenalty[0])
	}
	if res.CharacterRevision != 3 {
		t.Fatalf("result = %+v, want rev3", res)
	}
	if got := inspectKnown(t, s, sim.AggregateCharacter, 7); got.KnownRevision != 3 || got.Blocked {
		t.Fatalf("saver = %+v, want known3 clean", got)
	}
}

// Deep-copy in both directions: a hostile Store mutating the
// received request cannot alter caller memory, and post-call
// caller mutation cannot alter the executed Store request.
func TestCommitDeathEntryDeepCopy(t *testing.T) {
	s := mustSaverForPersist(t)
	trackDeathRoots(t, s, 10, map[int64]int64{20: 4})
	killer := &store.DeathEntryKiller{Kind: store.DeathEntryKillerCharacter, CharacterID: 9}
	req := deathTestReq()
	req.Items = req.Items[:1]
	req.Character.Spells = append(req.Character.Spells, store.CharacterSpellSnapshot{SpellID: 3, Ability: 30})
	req.Killer = killer
	fs := &fakeDeathStore{
		onDeath: func(got store.DeathEntryRequest) (store.DeathEntryResult, error) {
			got.Character.Vitals[0] = 'X'
			got.Character.Skills[0].Ability = -1
			got.Items[0].Snapshot.Enchants[0] = 'X'
			*got.Items[0].Snapshot.Location.PosX = -1
			return echoDeathResult(got), nil
		},
	}
	if _, err := CommitDeathEntry(context.Background(), s, fs, req); err != nil {
		t.Fatalf("CommitDeathEntry: %v", err)
	}
	if string(req.Character.Vitals) != `{"hp":1}` {
		t.Fatalf("caller vitals mutated: %s", req.Character.Vitals)
	}
	if req.Character.Spells[0].Ability != 10 || req.Character.Skills[0].Ability != 20 {
		t.Fatalf("caller abilities mutated: %+v %+v", req.Character.Spells, req.Character.Skills)
	}
	if string(req.Items[0].Snapshot.Enchants) != `{"e":1}` {
		t.Fatalf("caller enchants mutated: %s", req.Items[0].Snapshot.Enchants)
	}
	if *req.Items[0].Snapshot.Location.PosX != deathTestX {
		t.Fatalf("caller location mutated: %d", *req.Items[0].Snapshot.Location.PosX)
	}
	if killer.CharacterID != 9 || req.Killer.CharacterID != 9 {
		t.Fatalf("caller killer mutated: %+v", req.Killer)
	}
	// Post-call caller mutation must not have altered the
	// already-executed Store request (fields the fake left
	// alone: spells, PosY, Killer).
	req.Character.Vitals[0] = 'Z'
	req.Character.Spells[0].Ability = -5
	*req.Items[0].Snapshot.Location.PosY = -2
	req.Killer.CharacterID = -3
	captured := fs.gotDeath[0]
	if captured.Character.Spells[0].Ability != 10 {
		t.Fatalf("executed request spells altered: %+v", captured.Character.Spells)
	}
	if *captured.Items[0].Snapshot.Location.PosY != deathTestY {
		t.Fatalf("executed request location altered: %d", *captured.Items[0].Snapshot.Location.PosY)
	}
	if captured.Killer.CharacterID != 9 {
		t.Fatalf("executed request killer altered: %+v", captured.Killer)
	}
}

func TestCommitDeathEntryZeroItems(t *testing.T) {
	s := mustSaverForPersist(t)
	if err := s.Track(sim.AggregateKey{Kind: sim.AggregateCharacter, ID: 7}, 0); err != nil {
		t.Fatal(err)
	}
	fs := &fakeDeathStore{}
	req := deathTestReq()
	req.Items = nil
	res, err := CommitDeathEntry(context.Background(), s, fs, req)
	if err != nil {
		t.Fatalf("CommitDeathEntry: %v", err)
	}
	if fs.deathCalls != 1 || len(fs.gotDeath[0].Items) != 0 {
		t.Fatalf("store got %+v", fs.gotDeath)
	}
	if res.CharacterRevision != 1 || res.CorpseID != 123 || len(res.ItemRevisions) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if got := inspectKnown(t, s, sim.AggregateCharacter, 7); got.KnownRevision != 1 || got.Blocked {
		t.Fatalf("saver = %+v, want known1 clean", got)
	}
}

// NewbieHomeRespawn is scalar-copied through the immutable
// capture: the fake Store receives it unchanged while
// execution-time Saver revisions still overwrite hostile request
// revisions, and caller immutability holds.
func TestCommitDeathEntryNewbieHomeRespawnPassthrough(t *testing.T) {
	s := mustSaverForPersist(t)
	trackDeathRoots(t, s, 10, map[int64]int64{10: 8})
	fs := &fakeDeathStore{}
	req := deathTestReq()
	req.Items = req.Items[1:]
	req.EffectiveDeathCost = 0
	req.NewbieHomeRespawn = true
	res, err := CommitDeathEntry(context.Background(), s, fs, req)
	if err != nil {
		t.Fatalf("CommitDeathEntry: %v", err)
	}
	if fs.deathCalls != 1 {
		t.Fatalf("store calls = %d, want 1", fs.deathCalls)
	}
	got := fs.gotDeath[0]
	if !got.NewbieHomeRespawn {
		t.Fatalf("NewbieHomeRespawn lost in adapter: %+v", got)
	}
	if got.EffectiveDeathCost != 0 {
		t.Fatalf("EffectiveDeathCost = %d, want 0", got.EffectiveDeathCost)
	}
	if got.Character.ExpectedRevision != 10 {
		t.Fatalf("character ExpectedRevision = %d, want Saver-known 10", got.Character.ExpectedRevision)
	}
	if len(got.Items) != 1 || got.Items[0].Snapshot.ExpectedRevision != 8 {
		t.Fatalf("item revisions = %+v, want item10 rev 8", got.Items)
	}
	if res.CharacterRevision != 11 || res.CorpseID != 123 {
		t.Fatalf("result = %+v, want char rev 11 corpse 123", res)
	}
	if req.NewbieHomeRespawn != true || req.Character.ExpectedRevision != 999 {
		t.Fatal("caller request mutated")
	}
	if got := inspectKnown(t, s, sim.AggregateCharacter, 7); got.KnownRevision != 11 || got.Blocked {
		t.Fatalf("saver char = %+v, want known11 clean", got)
	}
}

// Pre-callback Saver rejection: Store MUST NOT be called, the
// result is zero, and tracked participants stay unblocked.
func TestCommitDeathAdaptersPreCallbackRejection(t *testing.T) {
	t.Run("death untracked item", func(t *testing.T) {
		s := mustSaverForPersist(t)
		if err := s.Track(sim.AggregateKey{Kind: sim.AggregateCharacter, ID: 7}, 3); err != nil {
			t.Fatal(err)
		}
		fs := &fakeDeathStore{}
		res, err := CommitDeathEntry(context.Background(), s, fs, deathTestReq())
		if !errors.Is(err, sim.ErrAggregateNotTracked) {
			t.Fatalf("err = %v, want ErrAggregateNotTracked", err)
		}
		if fs.deathCalls != 0 {
			t.Fatalf("store calls = %d, want 0", fs.deathCalls)
		}
		if !isZeroDeathEntryResult(res) {
			t.Fatalf("result = %+v, want zero", res)
		}
		if got := inspectKnown(t, s, sim.AggregateCharacter, 7); got.Blocked || got.KnownRevision != 3 {
			t.Fatalf("saver = %+v, want unblocked known3", got)
		}
	})
	t.Run("death duplicate item", func(t *testing.T) {
		s := mustSaverForPersist(t)
		trackDeathRoots(t, s, 10, map[int64]int64{20: 4})
		fs := &fakeDeathStore{}
		req := deathTestReq()
		req.Items = []store.DeathEntryItem{deathTestGroundItem(20, 999), deathTestGroundItem(20, 999)}
		_, err := CommitDeathEntry(context.Background(), s, fs, req)
		if !errors.Is(err, sim.ErrInvalidCriticalSet) {
			t.Fatalf("err = %v, want ErrInvalidCriticalSet", err)
		}
		if fs.deathCalls != 0 {
			t.Fatalf("store calls = %d, want 0", fs.deathCalls)
		}
		if got := inspectKnown(t, s, sim.AggregateCharacter, 7); got.Blocked {
			t.Fatalf("char blocked: %+v", got)
		}
		if got := inspectKnown(t, s, sim.AggregateItem, 20); got.Blocked {
			t.Fatalf("item blocked: %+v", got)
		}
	})
	t.Run("death invalid key", func(t *testing.T) {
		s := mustSaverForPersist(t)
		fs := &fakeDeathStore{}
		req := deathTestReq()
		req.Character.ID = 0
		_, err := CommitDeathEntry(context.Background(), s, fs, req)
		if !errors.Is(err, sim.ErrInvalidAggregateKey) {
			t.Fatalf("err = %v, want ErrInvalidAggregateKey", err)
		}
		if fs.deathCalls != 0 {
			t.Fatalf("store calls = %d, want 0", fs.deathCalls)
		}
	})
	t.Run("death cancelled context", func(t *testing.T) {
		s := mustSaverForPersist(t)
		trackDeathRoots(t, s, 10, map[int64]int64{20: 4, 10: 8})
		fs := &fakeDeathStore{}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := CommitDeathEntry(ctx, s, fs, deathTestReq())
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if errors.Is(err, sim.ErrSaverReconcileRequired) {
			t.Fatalf("pre-callback cancel must not reconcile-block: %v", err)
		}
		if fs.deathCalls != 0 {
			t.Fatalf("store calls = %d, want 0", fs.deathCalls)
		}
		if got := inspectKnown(t, s, sim.AggregateCharacter, 7); got.Blocked {
			t.Fatalf("char blocked: %+v", got)
		}
	})
	t.Run("portal untracked", func(t *testing.T) {
		s := mustSaverForPersist(t)
		fs := &fakeDeathStore{}
		_, err := CommitPortalOfLife(context.Background(), s, fs,
			store.PortalOfLifeRequest{Character: deathTestChar(7, 0), CorpseID: 1, ProposedCost: 20})
		if !errors.Is(err, sim.ErrAggregateNotTracked) {
			t.Fatalf("err = %v, want ErrAggregateNotTracked", err)
		}
		if fs.portalCalls != 0 {
			t.Fatalf("store calls = %d, want 0", fs.portalCalls)
		}
	})
	t.Run("penalties blocked stays", func(t *testing.T) {
		s := mustSaverForPersist(t)
		if err := s.Track(sim.AggregateKey{Kind: sim.AggregateCharacter, ID: 7}, 2); err != nil {
			t.Fatal(err)
		}
		fs := &fakeDeathStore{onPenalty: func(store.DeathPenaltiesRequest) (store.DeathPenaltiesResult, error) {
			return store.DeathPenaltiesResult{}, errors.New("boom")
		}}
		if _, err := CommitDeathPenalties(context.Background(), s, fs,
			store.DeathPenaltiesRequest{Character: deathTestChar(7, 0), ExpectedPendingCost: 1}); err == nil {
			t.Fatal("want error")
		}
		// A second attempt is rejected pre-callback with no new Store call.
		fs.onPenalty = nil
		_, err := CommitDeathPenalties(context.Background(), s, fs,
			store.DeathPenaltiesRequest{Character: deathTestChar(7, 0), ExpectedPendingCost: 1})
		if !errors.Is(err, sim.ErrSaverReconcileRequired) {
			t.Fatalf("err = %v, want ErrSaverReconcileRequired", err)
		}
		if fs.penaltyCalls != 1 {
			t.Fatalf("store calls = %d, want exactly 1", fs.penaltyCalls)
		}
	})
}

// Store stale translation for ALL THREE adapters: all three
// sentinels discoverable, zero result, revisions unchanged,
// every participant blocked (and left blocked: no automatic
// reconciliation).
func TestCommitDeathAdaptersStoreStale(t *testing.T) {
	mkStale := func(msg string) error {
		return errors.Join(errors.New(msg), store.ErrStaleRevision)
	}
	t.Run("death", func(t *testing.T) {
		s := mustSaverForPersist(t)
		trackDeathRoots(t, s, 10, map[int64]int64{20: 4, 10: 8})
		fs := &fakeDeathStore{onDeath: func(store.DeathEntryRequest) (store.DeathEntryResult, error) {
			return store.DeathEntryResult{}, mkStale("pg stale")
		}}
		res, err := CommitDeathEntry(context.Background(), s, fs, deathTestReq())
		if !errors.Is(err, store.ErrStaleRevision) || !errors.Is(err, sim.ErrSnapshotStale) ||
			!errors.Is(err, sim.ErrSaverReconcileRequired) {
			t.Fatalf("err = %v, want stale+snapshot-stale+reconcile-required", err)
		}
		if !isZeroDeathEntryResult(res) {
			t.Fatalf("result = %+v, want zero", res)
		}
		for _, k := range []sim.AggregateKey{
			{Kind: sim.AggregateCharacter, ID: 7},
			{Kind: sim.AggregateItem, ID: 20},
			{Kind: sim.AggregateItem, ID: 10},
		} {
			got, ierr := s.Inspect(k)
			if ierr != nil {
				t.Fatal(ierr)
			}
			if !got.Blocked {
				t.Fatalf("participant %v not blocked: %+v", k, got)
			}
		}
		if got := inspectKnown(t, s, sim.AggregateCharacter, 7); got.KnownRevision != 10 {
			t.Fatalf("char revision moved: %+v", got)
		}
		if got := inspectKnown(t, s, sim.AggregateItem, 20); got.KnownRevision != 4 {
			t.Fatalf("item20 revision moved: %+v", got)
		}
	})
	t.Run("portal", func(t *testing.T) {
		s := mustSaverForPersist(t)
		if err := s.Track(sim.AggregateKey{Kind: sim.AggregateCharacter, ID: 7}, 6); err != nil {
			t.Fatal(err)
		}
		fs := &fakeDeathStore{onPortal: func(store.PortalOfLifeRequest) (store.PortalOfLifeResult, error) {
			return store.PortalOfLifeResult{}, mkStale("pg stale")
		}}
		res, err := CommitPortalOfLife(context.Background(), s, fs,
			store.PortalOfLifeRequest{Character: deathTestChar(7, 0), CorpseID: 1, ProposedCost: 20})
		if !errors.Is(err, store.ErrStaleRevision) || !errors.Is(err, sim.ErrSnapshotStale) ||
			!errors.Is(err, sim.ErrSaverReconcileRequired) {
			t.Fatalf("err = %v, want all three sentinels", err)
		}
		if res != (store.PortalOfLifeResult{}) {
			t.Fatalf("result = %+v, want zero", res)
		}
		if got := inspectKnown(t, s, sim.AggregateCharacter, 7); !got.Blocked || got.KnownRevision != 6 {
			t.Fatalf("saver = %+v, want blocked known6", got)
		}
	})
	t.Run("penalties", func(t *testing.T) {
		s := mustSaverForPersist(t)
		if err := s.Track(sim.AggregateKey{Kind: sim.AggregateCharacter, ID: 7}, 2); err != nil {
			t.Fatal(err)
		}
		fs := &fakeDeathStore{onPenalty: func(store.DeathPenaltiesRequest) (store.DeathPenaltiesResult, error) {
			return store.DeathPenaltiesResult{}, mkStale("pg stale")
		}}
		res, err := CommitDeathPenalties(context.Background(), s, fs,
			store.DeathPenaltiesRequest{Character: deathTestChar(7, 0), ExpectedPendingCost: 40})
		if !errors.Is(err, store.ErrStaleRevision) || !errors.Is(err, sim.ErrSnapshotStale) ||
			!errors.Is(err, sim.ErrSaverReconcileRequired) {
			t.Fatalf("err = %v, want all three sentinels", err)
		}
		if res != (store.DeathPenaltiesResult{}) {
			t.Fatalf("result = %+v, want zero", res)
		}
		if got := inspectKnown(t, s, sim.AggregateCharacter, 7); !got.Blocked || got.KnownRevision != 2 {
			t.Fatalf("saver = %+v, want blocked known2", got)
		}
	})
}

// Semantic Store errors: original cause + ReconcileRequired,
// NEVER SnapshotStale, zero result, participants blocked.
func TestCommitDeathAdaptersSemanticErrors(t *testing.T) {
	t.Run("death already pending", func(t *testing.T) {
		s := mustSaverForPersist(t)
		trackDeathRoots(t, s, 10, map[int64]int64{20: 4, 10: 8})
		fs := &fakeDeathStore{onDeath: func(store.DeathEntryRequest) (store.DeathEntryResult, error) {
			return store.DeathEntryResult{}, store.ErrDeathAlreadyPending
		}}
		res, err := CommitDeathEntry(context.Background(), s, fs, deathTestReq())
		if !errors.Is(err, store.ErrDeathAlreadyPending) {
			t.Fatalf("err = %v, want ErrDeathAlreadyPending", err)
		}
		if !errors.Is(err, sim.ErrSaverReconcileRequired) {
			t.Fatalf("err = %v, want ErrSaverReconcileRequired", err)
		}
		if errors.Is(err, sim.ErrSnapshotStale) {
			t.Fatalf("semantic error misclassified as stale: %v", err)
		}
		if !isZeroDeathEntryResult(res) {
			t.Fatalf("result = %+v, want zero", res)
		}
		for _, id := range []int64{20, 10} {
			if got := inspectKnown(t, s, sim.AggregateItem, id); !got.Blocked {
				t.Fatalf("item %d not blocked: %+v", id, got)
			}
		}
	})
	t.Run("portal already used", func(t *testing.T) {
		s := mustSaverForPersist(t)
		if err := s.Track(sim.AggregateKey{Kind: sim.AggregateCharacter, ID: 7}, 6); err != nil {
			t.Fatal(err)
		}
		fs := &fakeDeathStore{onPortal: func(store.PortalOfLifeRequest) (store.PortalOfLifeResult, error) {
			return store.PortalOfLifeResult{}, store.ErrPortalAlreadyUsed
		}}
		res, err := CommitPortalOfLife(context.Background(), s, fs,
			store.PortalOfLifeRequest{Character: deathTestChar(7, 0), CorpseID: 1, ProposedCost: 20})
		if !errors.Is(err, store.ErrPortalAlreadyUsed) || !errors.Is(err, sim.ErrSaverReconcileRequired) {
			t.Fatalf("err = %v, want cause+reconcile-required", err)
		}
		if errors.Is(err, sim.ErrSnapshotStale) {
			t.Fatalf("semantic error misclassified as stale: %v", err)
		}
		if res != (store.PortalOfLifeResult{}) {
			t.Fatalf("result = %+v, want zero", res)
		}
	})
	t.Run("penalties cost mismatch", func(t *testing.T) {
		s := mustSaverForPersist(t)
		if err := s.Track(sim.AggregateKey{Kind: sim.AggregateCharacter, ID: 7}, 2); err != nil {
			t.Fatal(err)
		}
		fs := &fakeDeathStore{onPenalty: func(store.DeathPenaltiesRequest) (store.DeathPenaltiesResult, error) {
			return store.DeathPenaltiesResult{}, store.ErrPendingDeathCostMismatch
		}}
		res, err := CommitDeathPenalties(context.Background(), s, fs,
			store.DeathPenaltiesRequest{Character: deathTestChar(7, 0), ExpectedPendingCost: 40})
		if !errors.Is(err, store.ErrPendingDeathCostMismatch) || !errors.Is(err, sim.ErrSaverReconcileRequired) {
			t.Fatalf("err = %v, want cause+reconcile-required", err)
		}
		if errors.Is(err, sim.ErrSnapshotStale) {
			t.Fatalf("semantic error misclassified as stale: %v", err)
		}
		if res != (store.DeathPenaltiesResult{}) {
			t.Fatalf("result = %+v, want zero", res)
		}
	})
}

// Saver-result invariant / visibility fence: a valid-looking
// Store success with malformed root revisions MUST NOT leak
// CorpseID/EffectiveCost; the public result stays zero.
func TestCommitDeathAdaptersResultVisibilityFence(t *testing.T) {
	t.Run("death wrong item revision", func(t *testing.T) {
		s := mustSaverForPersist(t)
		trackDeathRoots(t, s, 10, map[int64]int64{20: 4, 10: 8})
		fs := &fakeDeathStore{onDeath: func(req store.DeathEntryRequest) (store.DeathEntryResult, error) {
			res := echoDeathResult(req)
			res.CorpseID = 999
			res.ItemRevisions[0].Revision += 5 // not expected+1.
			return res, nil
		}}
		res, err := CommitDeathEntry(context.Background(), s, fs, deathTestReq())
		if !errors.Is(err, sim.ErrSaverRevisionInvariant) || !errors.Is(err, sim.ErrSaverReconcileRequired) {
			t.Fatalf("err = %v, want invariant+reconcile-required", err)
		}
		if !isZeroDeathEntryResult(res) {
			t.Fatalf("CorpseID escaped: %+v", res)
		}
		if got := inspectKnown(t, s, sim.AggregateCharacter, 7); !got.Blocked || got.KnownRevision != 10 {
			t.Fatalf("saver char = %+v, want blocked known10", got)
		}
	})
	t.Run("death missing item revision", func(t *testing.T) {
		s := mustSaverForPersist(t)
		trackDeathRoots(t, s, 10, map[int64]int64{20: 4, 10: 8})
		fs := &fakeDeathStore{onDeath: func(req store.DeathEntryRequest) (store.DeathEntryResult, error) {
			res := echoDeathResult(req)
			res.CorpseID = 999
			res.ItemRevisions = res.ItemRevisions[:1]
			return res, nil
		}}
		res, err := CommitDeathEntry(context.Background(), s, fs, deathTestReq())
		if !errors.Is(err, sim.ErrSaverRevisionInvariant) || !errors.Is(err, sim.ErrSaverReconcileRequired) {
			t.Fatalf("err = %v, want invariant+reconcile-required", err)
		}
		if !isZeroDeathEntryResult(res) {
			t.Fatalf("CorpseID escaped: %+v", res)
		}
	})
	t.Run("death duplicate item revision", func(t *testing.T) {
		s := mustSaverForPersist(t)
		trackDeathRoots(t, s, 10, map[int64]int64{20: 4})
		req := deathTestReq()
		req.Items = req.Items[:1]
		fs := &fakeDeathStore{onDeath: func(req store.DeathEntryRequest) (store.DeathEntryResult, error) {
			res := echoDeathResult(req)
			res.ItemRevisions = append(res.ItemRevisions, res.ItemRevisions[0])
			return res, nil
		}}
		res, err := CommitDeathEntry(context.Background(), s, fs, req)
		if !errors.Is(err, sim.ErrSaverRevisionInvariant) || !errors.Is(err, sim.ErrSaverReconcileRequired) {
			t.Fatalf("err = %v, want invariant+reconcile-required", err)
		}
		if !isZeroDeathEntryResult(res) {
			t.Fatalf("CorpseID escaped: %+v", res)
		}
	})
	t.Run("portal wrong revision", func(t *testing.T) {
		s := mustSaverForPersist(t)
		if err := s.Track(sim.AggregateKey{Kind: sim.AggregateCharacter, ID: 7}, 6); err != nil {
			t.Fatal(err)
		}
		fs := &fakeDeathStore{onPortal: func(req store.PortalOfLifeRequest) (store.PortalOfLifeResult, error) {
			return store.PortalOfLifeResult{CharacterRevision: req.Character.ExpectedRevision + 9, EffectiveCost: 40}, nil
		}}
		res, err := CommitPortalOfLife(context.Background(), s, fs,
			store.PortalOfLifeRequest{Character: deathTestChar(7, 0), CorpseID: 1, ProposedCost: 20})
		if !errors.Is(err, sim.ErrSaverRevisionInvariant) || !errors.Is(err, sim.ErrSaverReconcileRequired) {
			t.Fatalf("err = %v, want invariant+reconcile-required", err)
		}
		if res != (store.PortalOfLifeResult{}) {
			t.Fatalf("EffectiveCost escaped: %+v", res)
		}
		if got := inspectKnown(t, s, sim.AggregateCharacter, 7); !got.Blocked || got.KnownRevision != 6 {
			t.Fatalf("saver = %+v, want blocked known6", got)
		}
	})
}
