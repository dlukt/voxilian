package persist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/store"
)

// fakeSnapshotStore is a scriptable narrow Store: exact-revision CAS
// against in-memory revisions, with injectable failures.
type fakeSnapshotStore struct {
	mu       sync.Mutex
	charRev  map[int64]int64
	itemRev  map[int64]int64
	bankRev  map[string]int64
	gotChar  []store.CharacterSnapshot
	gotItem  []store.ItemSnapshot
	gotBank  []store.BankSnapshot
	failNext error
}

func newFakeSnapshotStore() *fakeSnapshotStore {
	return &fakeSnapshotStore{
		charRev: map[int64]int64{},
		itemRev: map[int64]int64{},
		bankRev: map[string]int64{},
	}
}

func bankKey(charID int64, system string) string {
	return fmt.Sprintf("%d/%s", charID, system)
}

func (f *fakeSnapshotStore) SaveCharacterSnapshot(_ context.Context, snap store.CharacterSnapshot) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotChar = append(f.gotChar, snap)
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return 0, err
	}
	if snap.ExpectedRevision != f.charRev[snap.ID] {
		return 0, fmt.Errorf("fake save character: %w", store.ErrStaleRevision)
	}
	f.charRev[snap.ID]++
	return f.charRev[snap.ID], nil
}

func (f *fakeSnapshotStore) SaveItemSnapshot(_ context.Context, snap store.ItemSnapshot) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotItem = append(f.gotItem, snap)
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return 0, err
	}
	if snap.ExpectedRevision != f.itemRev[snap.ID] {
		return 0, fmt.Errorf("fake save item: %w", store.ErrStaleRevision)
	}
	f.itemRev[snap.ID]++
	return f.itemRev[snap.ID], nil
}

func (f *fakeSnapshotStore) SaveBankBalance(_ context.Context, snap store.BankSnapshot) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotBank = append(f.gotBank, snap)
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return 0, err
	}
	k := bankKey(snap.CharacterID, snap.System)
	if snap.ExpectedRevision != f.bankRev[k] {
		return 0, fmt.Errorf("fake save bank: %w", store.ErrStaleRevision)
	}
	f.bankRev[k]++
	return f.bankRev[k], nil
}

var errFakeTransient = errors.New("persist test transient")

func mustSaverForPersist(t *testing.T) *sim.Saver {
	t.Helper()
	s, err := sim.NewSaver(sim.SaverConfig{Interval: 60_000_000_000, Clock: sim.NewSystemClock()})
	if err != nil {
		t.Fatalf("NewSaver: %v", err)
	}
	return s
}

func TestCharacterJobKeyAndRevision(t *testing.T) {
	fs := newFakeSnapshotStore()
	snap := store.CharacterSnapshot{
		ID: 42, ExpectedRevision: 999, // hostile: must be ignored.
		Karma: 5, Vitals: json.RawMessage(`{"hp":40}`),
		Spells: []store.CharacterSpellSnapshot{{SpellID: 1, Ability: 10}},
		Skills: []store.CharacterSkillSnapshot{{SkillID: 2, Ability: 20}},
	}
	job, err := NewCharacterSnapshotJob(fs, snap)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	if job.Key.Kind != sim.AggregateCharacter || job.Key.ID != 42 || job.Key.Scope != "" {
		t.Fatalf("key = %+v", job.Key)
	}
	s := mustSaverForPersist(t)
	if err := s.Track(job.Key, 4); err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	fs.charRev[42] = 4 // durable starts where the saver tracked it.
	fs.mu.Unlock()
	if err := s.MarkDirty(job.Key, job.Write); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushDirty(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if len(fs.gotChar) != 1 {
		t.Fatalf("store calls = %d", len(fs.gotChar))
	}
	got := fs.gotChar[0]
	if got.ExpectedRevision != 4 {
		t.Fatalf("store received expected %d, want saver-known 4 (input 999 ignored)", got.ExpectedRevision)
	}
	snapState, _ := s.Inspect(job.Key)
	if snapState.KnownRevision != 5 {
		t.Fatalf("saver known = %d, want 5", snapState.KnownRevision)
	}
}

func TestCharacterJobDeepCopy(t *testing.T) {
	fs := newFakeSnapshotStore()
	vitals := json.RawMessage(`{"hp":40}`)
	adv := json.RawMessage(`{"pts":1}`)
	snap := store.CharacterSnapshot{
		ID: 7, Vitals: vitals, Advancement: adv,
		Spells: []store.CharacterSpellSnapshot{{SpellID: 1, Ability: 10}},
		Skills: []store.CharacterSkillSnapshot{{SkillID: 1, Ability: 30}},
	}
	job, err := NewCharacterSnapshotJob(fs, snap)
	if err != nil {
		t.Fatal(err)
	}
	// Mutate every caller-owned input after construction.
	vitals[1] = 'X'
	adv[1] = 'X'
	snap.Spells[0].Ability = 99
	snap.Skills[0].Ability = 99
	rev, err := job.Write(context.Background(), 0)
	if err != nil || rev != 1 {
		t.Fatalf("write = (%d,%v)", rev, err)
	}
	got := fs.gotChar[0]
	if string(got.Vitals) != `{"hp":40}` || string(got.Advancement) != `{"pts":1}` {
		t.Fatalf("bytes leaked: %s %s", got.Vitals, got.Advancement)
	}
	if got.Spells[0].Ability != 10 || got.Skills[0].Ability != 30 {
		t.Fatalf("slices leaked: %+v %+v", got.Spells, got.Skills)
	}
}

func TestAdapterStaleDualSentinel(t *testing.T) {
	fs := newFakeSnapshotStore()
	ctx := context.Background()
	// Character.
	cjob, err := NewCharacterSnapshotJob(fs, store.CharacterSnapshot{ID: 1})
	if err != nil {
		t.Fatal(err)
	}
	fs.charRev[1] = 3 // durable ahead: expected 0 is stale.
	if _, err := cjob.Write(ctx, 0); !errors.Is(err, sim.ErrSnapshotStale) || !errors.Is(err, store.ErrStaleRevision) {
		t.Fatalf("char stale err = %v, want both sentinels", err)
	}
	// Item.
	ijob, err := NewItemSnapshotJob(fs, store.ItemSnapshot{ID: 2})
	if err != nil {
		t.Fatal(err)
	}
	fs.itemRev[2] = 1
	if _, err := ijob.Write(ctx, 0); !errors.Is(err, sim.ErrSnapshotStale) || !errors.Is(err, store.ErrStaleRevision) {
		t.Fatalf("item stale err = %v, want both sentinels", err)
	}
	// Bank.
	bjob, err := NewBankSnapshotJob(fs, store.BankSnapshot{CharacterID: 3, System: "tos"})
	if err != nil {
		t.Fatal(err)
	}
	fs.bankRev[bankKey(3, "tos")] = 2
	if _, err := bjob.Write(ctx, 1); !errors.Is(err, sim.ErrSnapshotStale) || !errors.Is(err, store.ErrStaleRevision) {
		t.Fatalf("bank stale err = %v, want both sentinels", err)
	}
	// Non-stale errors are preserved, never converted.
	fs.failNext = errFakeTransient
	if _, err := bjob.Write(ctx, 2); !errors.Is(err, errFakeTransient) {
		t.Fatalf("transient err = %v", err)
	} else if errors.Is(err, sim.ErrSnapshotStale) {
		t.Fatalf("transient converted to stale: %v", err)
	}
}

func TestItemJobDeepCopyPointers(t *testing.T) {
	fs := newFakeSnapshotStore()
	charID, corpseID, contID := int64(11), int64(22), int64(33)
	region, slot := "barloque", "back"
	px, py, pz := int64(1), int64(2), int64(3)
	snap := store.ItemSnapshot{
		ID: 5, Qty: 2, Hits: 100, Enchants: json.RawMessage(`{"e":1}`),
		Location: store.ItemLocationSnapshot{
			Kind: 0, CharacterID: &charID, CorpseID: &corpseID,
			ContainerItemID: &contID, VaultRegion: &region,
			PosX: &px, PosY: &py, PosZ: &pz, Slot: &slot,
		},
	}
	job, err := NewItemSnapshotJob(fs, snap)
	if err != nil {
		t.Fatal(err)
	}
	if job.Key.Kind != sim.AggregateItem || job.Key.ID != 5 {
		t.Fatalf("key = %+v", job.Key)
	}
	// Mutate every pointed-to value after construction.
	charID, corpseID, contID = 99, 99, 99
	region, slot = "ZZZ", "ZZZ"
	px, py, pz = 99, 99, 99
	snap.Enchants[1] = 'X'
	if _, err := job.Write(context.Background(), 0); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := fs.gotItem[0]
	loc := got.Location
	if *loc.CharacterID != 11 || *loc.CorpseID != 22 || *loc.ContainerItemID != 33 ||
		*loc.VaultRegion != "barloque" || *loc.PosX != 1 || *loc.PosY != 2 ||
		*loc.PosZ != 3 || *loc.Slot != "back" {
		t.Fatalf("location pointers leaked: %+v", loc)
	}
	if string(got.Enchants) != `{"e":1}` {
		t.Fatalf("enchants leaked: %s", got.Enchants)
	}
}

func TestBankJobKeyAndHostileRevision(t *testing.T) {
	fs := newFakeSnapshotStore()
	job, err := NewBankSnapshotJob(fs, store.BankSnapshot{
		CharacterID: 8, System: "koc", ExpectedRevision: 777, Balance: 150,
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.Key.Kind != sim.AggregateBank || job.Key.ID != 8 || job.Key.Scope != "koc" {
		t.Fatalf("key = %+v", job.Key)
	}
	rev, err := job.Write(context.Background(), 0)
	if err != nil || rev != 1 {
		t.Fatalf("write = (%d,%v)", rev, err)
	}
	if fs.gotBank[0].ExpectedRevision != 0 || fs.gotBank[0].Balance != 150 {
		t.Fatalf("store got %+v", fs.gotBank[0])
	}
}

func TestFactoryInvalidInputs(t *testing.T) {
	fs := newFakeSnapshotStore()
	if _, err := NewCharacterSnapshotJob(fs, store.CharacterSnapshot{ID: 0}); !errors.Is(err, sim.ErrInvalidAggregateKey) {
		t.Errorf("char 0: %v", err)
	}
	if _, err := NewCharacterSnapshotJob(fs, store.CharacterSnapshot{ID: -1}); !errors.Is(err, sim.ErrInvalidAggregateKey) {
		t.Errorf("char -1: %v", err)
	}
	if _, err := NewItemSnapshotJob(fs, store.ItemSnapshot{ID: 0}); !errors.Is(err, sim.ErrInvalidAggregateKey) {
		t.Errorf("item 0: %v", err)
	}
	if _, err := NewBankSnapshotJob(fs, store.BankSnapshot{CharacterID: 0, System: "tos"}); !errors.Is(err, sim.ErrInvalidAggregateKey) {
		t.Errorf("bank char 0: %v", err)
	}
	if _, err := NewBankSnapshotJob(fs, store.BankSnapshot{CharacterID: 1, System: ""}); !errors.Is(err, sim.ErrInvalidAggregateKey) {
		t.Errorf("bank empty system: %v", err)
	}
}
