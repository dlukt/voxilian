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

// M5-T5c2b real PostgreSQL 18 composition proofs (spec
// §9.5.1c): full death lifecycle through Saver (entry ->
// portal -> penalties), real stale + T5c2a recovery, and the
// DeathEntry lost-acknowledgement composition proof. No direct
// Store call for the operation under test; the real
// store.PGStore satisfies DeathPersistenceStore.

const (
	deathPGX = int64(700)
	deathPGY = int64(800)
	deathPGZ = int64(900)
)

func deathPGSetup(t *testing.T, sub, name string, nItems int) (context.Context, *store.PGStore, *sim.Saver, int64, []int64) {
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
	return ctx, st, s, charID, items
}

func deathPGGroundItem(itemID int64, protect time.Duration) store.DeathEntryItem {
	x, y, z := deathPGX, deathPGY, deathPGZ
	return store.DeathEntryItem{
		Snapshot: store.ItemSnapshot{
			ID: itemID, ExpectedRevision: 999, // hostile: the adapter overwrites it.
			Qty: 5, Hits: 77, Enchants: json.RawMessage(`{"e":1}`),
			Location: store.ItemLocationSnapshot{Kind: 1, PosX: &x, PosY: &y, PosZ: &z},
		},
		PKProtectionDuration: protect,
	}
}

func deathPGCharSnap(charID int64) store.CharacterSnapshot {
	return store.CharacterSnapshot{
		ID: charID, ExpectedRevision: 999, // hostile: the adapter overwrites it.
		Karma: 3, PosX: 1, PosY: 2, PosZ: 3,
		Vitals:      json.RawMessage(`{"hp":1}`),
		Advancement: json.RawMessage(`{"pts":0}`),
		Flags:       3,
		Spells:      []store.CharacterSpellSnapshot{{SpellID: 1, Ability: 10}},
		Skills:      []store.CharacterSkillSnapshot{{SkillID: 1, Ability: 30}},
	}
}

func deathPGEntryReq(charID int64, items []int64) store.DeathEntryRequest {
	entry := []store.DeathEntryItem{deathPGGroundItem(items[0], time.Hour)}
	if len(items) > 1 {
		entry = append(entry, deathPGGroundItem(items[1], 0))
	}
	return store.DeathEntryRequest{
		Character:          deathPGCharSnap(charID),
		DeathPosX:          deathPGX,
		DeathPosY:          deathPGY,
		DeathPosZ:          deathPGZ,
		EffectiveDeathCost: 40,
		DeathTimeSeconds:   1700000000,
		CorpseLifetime:     10 * time.Minute,
		Items:              entry,
	}
}

func checkGround(t *testing.T, st *store.PGStore, ctx context.Context, itemID int64, wantRev int64, wantProt bool, charID int64) {
	t.Helper()
	rec, err := st.LoadDeathItemRecovery(ctx, itemID)
	if err != nil {
		t.Fatalf("item %d recovery: %v", itemID, err)
	}
	if rec.Item.ExpectedRevision != wantRev {
		t.Fatalf("item %d rev = %d, want %d", itemID, rec.Item.ExpectedRevision, wantRev)
	}
	loc := rec.Item.Location
	if loc.Kind != 1 || loc.PosX == nil || *loc.PosX != deathPGX ||
		loc.PosY == nil || *loc.PosY != deathPGY || loc.PosZ == nil || *loc.PosZ != deathPGZ {
		t.Fatalf("item %d location = %+v, want ground death pos", itemID, loc)
	}
	if wantProt && (rec.PKProtection == nil || rec.PKProtection.VictimCharacterID != charID) {
		t.Fatalf("item %d protection = %+v, want victim %d", itemID, rec.PKProtection, charID)
	}
	if !wantProt && rec.PKProtection != nil {
		t.Fatalf("item %d unexpected protection = %+v", itemID, rec.PKProtection)
	}
}

// Full lifecycle through Saver: death entry (2 items, one
// PK-protected) -> Portal -> penalties, all via the persist
// adapters with deliberately hostile embedded revisions.
func TestPersistPGDeathLifecycleThroughSaver(t *testing.T) {
	ctx, st, s, charID, items := deathPGSetup(t, "sub-death-life", "Deather", 2)

	res, err := CommitDeathEntry(ctx, s, st, deathPGEntryReq(charID, items))
	if err != nil {
		t.Fatalf("CommitDeathEntry: %v", err)
	}
	if res.CharacterRevision != 1 || res.CorpseID <= 0 {
		t.Fatalf("entry result = %+v, want rev1 corpse>0", res)
	}
	if len(res.ItemRevisions) != 2 {
		t.Fatalf("entry item revisions = %+v", res.ItemRevisions)
	}
	for _, ir := range res.ItemRevisions {
		if ir.Revision != 1 {
			t.Fatalf("entry item revisions = %+v, want all rev1", res.ItemRevisions)
		}
	}
	if got, _ := s.Inspect(sim.AggregateKey{Kind: sim.AggregateCharacter, ID: charID}); got.KnownRevision != 1 || got.Blocked {
		t.Fatalf("saver char = %+v, want known1 clean", got)
	}
	for _, id := range items {
		if got, _ := s.Inspect(sim.AggregateKey{Kind: sim.AggregateItem, ID: id}); got.KnownRevision != 1 || got.Blocked {
			t.Fatalf("saver item %d = %+v, want known1 clean", id, got)
		}
	}
	rec, err := st.LoadDeathCharacterRecovery(ctx, charID)
	if err != nil {
		t.Fatalf("character recovery: %v", err)
	}
	if rec.Character.ExpectedRevision != 1 || rec.Pending == nil ||
		rec.Pending.EffectiveCost != 40 || rec.Pending.CorpseID == nil ||
		*rec.Pending.CorpseID != res.CorpseID || rec.Pending.PortalUsed {
		t.Fatalf("pending = %+v, want cost40 corpse%d unused", rec.Pending, res.CorpseID)
	}
	checkGround(t, st, ctx, items[0], 1, true, charID)
	checkGround(t, st, ctx, items[1], 1, false, charID)

	// Portal through the one-key adapter.
	portalSnap := rec.Character
	portalSnap.ExpectedRevision = 999
	pres, err := CommitPortalOfLife(ctx, s, st, store.PortalOfLifeRequest{
		Character: portalSnap, CorpseID: res.CorpseID, ProposedCost: 20,
	})
	if err != nil {
		t.Fatalf("CommitPortalOfLife: %v", err)
	}
	if pres.CharacterRevision != 2 || pres.EffectiveCost != 20 {
		t.Fatalf("portal result = %+v, want rev2 cost20", pres)
	}
	if got, _ := s.Inspect(sim.AggregateKey{Kind: sim.AggregateCharacter, ID: charID}); got.KnownRevision != 2 || got.Blocked {
		t.Fatalf("saver char = %+v, want known2 clean", got)
	}
	rec2, err := st.LoadDeathCharacterRecovery(ctx, charID)
	if err != nil {
		t.Fatalf("character recovery: %v", err)
	}
	if rec2.Character.ExpectedRevision != 2 || rec2.Pending == nil ||
		rec2.Pending.EffectiveCost != 20 || !rec2.Pending.PortalUsed ||
		rec2.Pending.CorpseID == nil || *rec2.Pending.CorpseID != res.CorpseID {
		t.Fatalf("pending = %+v, want cost20 used same corpse", rec2.Pending)
	}
	checkGround(t, st, ctx, items[0], 1, true, charID)
	checkGround(t, st, ctx, items[1], 1, false, charID)

	// Penalties through the one-key adapter.
	penSnap := rec2.Character
	penSnap.ExpectedRevision = 999
	penSnap.Flags++
	penres, err := CommitDeathPenalties(ctx, s, st, store.DeathPenaltiesRequest{
		Character: penSnap, ExpectedPendingCost: 20,
	})
	if err != nil {
		t.Fatalf("CommitDeathPenalties: %v", err)
	}
	if penres.CharacterRevision != 3 {
		t.Fatalf("penalties result = %+v, want rev3", penres)
	}
	if got, _ := s.Inspect(sim.AggregateKey{Kind: sim.AggregateCharacter, ID: charID}); got.KnownRevision != 3 || got.Blocked {
		t.Fatalf("saver char = %+v, want known3 clean", got)
	}
	rec3, err := st.LoadDeathCharacterRecovery(ctx, charID)
	if err != nil {
		t.Fatalf("character recovery: %v", err)
	}
	if rec3.Character.ExpectedRevision != 3 || rec3.Pending != nil {
		t.Fatalf("post-penalty = rev%d pending%+v, want rev3 nil pending",
			rec3.Character.ExpectedRevision, rec3.Pending)
	}
	if rec3.Character.Flags != penSnap.Flags {
		t.Fatalf("post-penalty flags = %d, want %d", rec3.Character.Flags, penSnap.Flags)
	}
	checkGround(t, st, ctx, items[0], 1, true, charID)
	checkGround(t, st, ctx, items[1], 1, false, charID)
}

// Real stale composition: PG advances independently, the
// adapter reports all three sentinels and blocks every
// participant, then T5c2a reloads reconcile each root.
func TestPersistPGDeathStaleThenT5c2aRecovery(t *testing.T) {
	ctx, st, s, charID, items := deathPGSetup(t, "sub-death-stale", "Staler", 2)

	if _, err := st.SaveCharacterSnapshot(ctx, store.CharacterSnapshot{
		ID: charID, Vitals: []byte(`{}`), Advancement: []byte(`{}`),
	}); err != nil {
		t.Fatalf("external advance: %v", err)
	}
	res, err := CommitDeathEntry(ctx, s, st, deathPGEntryReq(charID, items))
	if !errors.Is(err, store.ErrStaleRevision) || !errors.Is(err, sim.ErrSnapshotStale) ||
		!errors.Is(err, sim.ErrSaverReconcileRequired) {
		t.Fatalf("err = %v, want all three sentinels", err)
	}
	if !isZeroDeathEntryResult(res) {
		t.Fatalf("result = %+v, want zero", res)
	}
	charKey := sim.AggregateKey{Kind: sim.AggregateCharacter, ID: charID}
	keys := []sim.AggregateKey{charKey}
	for _, id := range items {
		keys = append(keys, sim.AggregateKey{Kind: sim.AggregateItem, ID: id})
	}
	for _, k := range keys {
		if got, _ := s.Inspect(k); !got.Blocked || got.KnownRevision != 0 {
			t.Fatalf("participant %v = %+v, want blocked known0", k, got)
		}
	}

	// Explicit caller-owned recovery via T5c2a: no retry, no
	// guessing — reload authoritative state per root.
	noopChar := func(store.DeathCharacterRecoverySnapshot) error { return nil }
	noopItem := func(store.DeathItemRecoverySnapshot) error { return nil }
	charState, err := sim.NewReconcileState(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := ReconcileSaver(ctx, charState, s, charKey, DeathCharacterReload(st, charID, noopChar)); err != nil {
		t.Fatalf("ReconcileSaver char: %v", err)
	}
	for _, id := range items {
		itemKey := sim.AggregateKey{Kind: sim.AggregateItem, ID: id}
		itemState, err := sim.NewReconcileState(0)
		if err != nil {
			t.Fatal(err)
		}
		if err := ReconcileSaver(ctx, itemState, s, itemKey, DeathItemReload(st, id, noopItem)); err != nil {
			t.Fatalf("ReconcileSaver item %d: %v", id, err)
		}
	}
	if got, _ := s.Inspect(charKey); got.Blocked || got.KnownRevision != 1 {
		t.Fatalf("saver char = %+v, want clear known1", got)
	}
	for _, id := range items {
		k := sim.AggregateKey{Kind: sim.AggregateItem, ID: id}
		if got, _ := s.Inspect(k); got.Blocked || got.KnownRevision != 0 {
			t.Fatalf("saver item %d = %+v, want clear known0", id, got)
		}
	}
}

// Test-only seam: the REAL PGStore commit succeeds, but the
// success acknowledgement is discarded and replaced with a
// synthetic lost-ack error.
type deathLostAckStore struct {
	DeathPersistenceStore
	armed *bool
}

func (w *deathLostAckStore) CommitDeathEntry(ctx context.Context, req store.DeathEntryRequest) (store.DeathEntryResult, error) {
	res, err := w.DeathPersistenceStore.CommitDeathEntry(ctx, req)
	if err != nil {
		return res, err
	}
	if *w.armed {
		*w.armed = false
		return store.DeathEntryResult{}, errLostAck
	}
	return res, nil
}

// Commit-ambiguity composition proof: the real transaction
// COMMITTED, the adapter saw only the lost ack (zero result,
// cause + ReconcileRequired, old revisions, all blocked), and
// T5c2a recovery proves the committed state without replay.
func TestPersistPGDeathLostAckRecovery(t *testing.T) {
	ctx, st, s, charID, items := deathPGSetup(t, "sub-death-amb", "Ambiguous", 2)

	armed := true
	wrap := &deathLostAckStore{DeathPersistenceStore: st, armed: &armed}
	res, err := CommitDeathEntry(ctx, s, wrap, deathPGEntryReq(charID, items))
	if !errors.Is(err, errLostAck) {
		t.Fatalf("err = %v, want lost-ack cause", err)
	}
	if !errors.Is(err, sim.ErrSaverReconcileRequired) {
		t.Fatalf("err = %v, want ErrSaverReconcileRequired", err)
	}
	if errors.Is(err, sim.ErrSnapshotStale) {
		t.Fatalf("lost ack misclassified as stale: %v", err)
	}
	if !isZeroDeathEntryResult(res) {
		t.Fatalf("result = %+v, want zero", res)
	}
	charKey := sim.AggregateKey{Kind: sim.AggregateCharacter, ID: charID}
	keys := []sim.AggregateKey{charKey}
	for _, id := range items {
		keys = append(keys, sim.AggregateKey{Kind: sim.AggregateItem, ID: id})
	}
	for _, k := range keys {
		if got, _ := s.Inspect(k); !got.Blocked || got.KnownRevision != 0 {
			t.Fatalf("participant %v = %+v, want blocked known0", k, got)
		}
	}

	// PG proves the first commit happened (no replay used to
	// discover this).
	rec, err := st.LoadDeathCharacterRecovery(ctx, charID)
	if err != nil {
		t.Fatalf("character recovery: %v", err)
	}
	if rec.Character.ExpectedRevision != 1 || rec.Pending == nil || rec.Pending.CorpseID == nil {
		t.Fatalf("PG state = rev%d pending%+v, want committed rev1 with corpse", rec.Character.ExpectedRevision, rec.Pending)
	}
	checkGround(t, st, ctx, items[0], 1, true, charID)
	checkGround(t, st, ctx, items[1], 1, false, charID)

	// Reconcile every blocked participant to the recovered
	// authoritative revisions.
	noopChar := func(store.DeathCharacterRecoverySnapshot) error { return nil }
	noopItem := func(store.DeathItemRecoverySnapshot) error { return nil }
	charState, err := sim.NewReconcileState(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := ReconcileSaver(ctx, charState, s, charKey, DeathCharacterReload(st, charID, noopChar)); err != nil {
		t.Fatalf("ReconcileSaver char: %v", err)
	}
	for _, id := range items {
		itemKey := sim.AggregateKey{Kind: sim.AggregateItem, ID: id}
		itemState, err := sim.NewReconcileState(0)
		if err != nil {
			t.Fatal(err)
		}
		if err := ReconcileSaver(ctx, itemState, s, itemKey, DeathItemReload(st, id, noopItem)); err != nil {
			t.Fatalf("ReconcileSaver item %d: %v", id, err)
		}
	}
	for _, k := range keys {
		if got, _ := s.Inspect(k); got.Blocked || got.KnownRevision != 1 {
			t.Fatalf("participant %v = %+v, want clear known1", k, got)
		}
	}
}
