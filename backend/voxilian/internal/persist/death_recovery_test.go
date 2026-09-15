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

// M5-T5c2a persist reload-adapter tests (spec §9.5.1b): staged
// DeathCharacterReload / DeathItemReload semantics, hostile-loader
// immutability, and integration with the UNCHANGED
// ReconcileSaver bridge. No PG, no Store writes.

type fakeDeathCharacterLoader struct {
	snap  store.DeathCharacterRecoverySnapshot
	err   error
	calls int
}

func (f *fakeDeathCharacterLoader) LoadDeathCharacterRecovery(context.Context, int64) (store.DeathCharacterRecoverySnapshot, error) {
	f.calls++
	if f.err != nil {
		return store.DeathCharacterRecoverySnapshot{}, f.err
	}
	return f.snap, nil
}

type fakeDeathItemLoader struct {
	snap  store.DeathItemRecoverySnapshot
	err   error
	calls int
}

func (f *fakeDeathItemLoader) LoadDeathItemRecovery(context.Context, int64) (store.DeathItemRecoverySnapshot, error) {
	f.calls++
	if f.err != nil {
		return store.DeathItemRecoverySnapshot{}, f.err
	}
	return f.snap, nil
}

func hostileDeathCharacter() store.DeathCharacterRecoverySnapshot {
	corpse := int64(77)
	return store.DeathCharacterRecoverySnapshot{
		Character: store.CharacterSnapshot{
			ID: 7, ExpectedRevision: 4, Karma: 11,
			PosX: 111, PosY: 222, PosZ: 333,
			Vitals:      json.RawMessage(`{"hp":1}`),
			Advancement: json.RawMessage(`{"pts":0}`),
			Flags:       3,
			Spells:      []store.CharacterSpellSnapshot{{SpellID: 1, Ability: 10}},
			Skills:      []store.CharacterSkillSnapshot{{SkillID: 2, Ability: 20}},
		},
		Pending: &store.PendingDeathSnapshot{
			CharacterID: 7, EffectiveCost: 40, DeathTimeSeconds: 1700000000,
			CorpseID: &corpse, PortalUsed: true,
		},
	}
}

func hostileDeathItem() store.DeathItemRecoverySnapshot {
	x, y, z := int64(7000), int64(8000), int64(9000)
	slot := "pack"
	return store.DeathItemRecoverySnapshot{
		Item: store.ItemSnapshot{
			ID: 9, ExpectedRevision: 5, Qty: 5, Hits: 77,
			Enchants: json.RawMessage(`{"e":1}`),
			Location: store.ItemLocationSnapshot{
				Kind: 1, PosX: &x, PosY: &y, PosZ: &z, Slot: &slot,
			},
		},
		PKProtection: &store.ItemPKProtectionSnapshot{
			ItemID: 9, VictimCharacterID: 7,
			ExpiresAt: hostileExpiry(),
		},
	}
}

func hostileExpiry() time.Time { return time.Unix(1700000600, 0).UTC() }

func TestDeathCharacterReloadStaged(t *testing.T) {
	loader := &fakeDeathCharacterLoader{snap: hostileDeathCharacter()}
	var applied store.DeathCharacterRecoverySnapshot
	mutated := false
	reload := DeathCharacterReload(loader, 7, func(s store.DeathCharacterRecoverySnapshot) error {
		mutated = true
		applied = s
		return nil
	})
	cand, err := reload(context.Background())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loader.calls != 1 {
		t.Fatalf("loader calls = %d, want 1", loader.calls)
	}
	if mutated {
		t.Fatal("live memory mutated during load staging")
	}
	if cand.Revision != 4 {
		t.Fatalf("candidate revision = %d, want loaded root 4", cand.Revision)
	}
	if cand.Apply == nil {
		t.Fatal("nil apply")
	}
	if err := cand.Apply(); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !mutated || applied.Character.ID != 7 || applied.Character.ExpectedRevision != 4 ||
		applied.Pending == nil || applied.Pending.EffectiveCost != 40 ||
		applied.Pending.CorpseID == nil || *applied.Pending.CorpseID != 77 {
		t.Fatalf("applied = %+v", applied)
	}
}

func TestDeathCharacterReloadImmutable(t *testing.T) {
	src := hostileDeathCharacter()
	loader := &fakeDeathCharacterLoader{snap: src}
	var applied store.DeathCharacterRecoverySnapshot
	reload := DeathCharacterReload(loader, 7, func(s store.DeathCharacterRecoverySnapshot) error {
		applied = s
		return nil
	})
	cand, err := reload(context.Background())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	// Hostile loader mutates/reuses every shared buffer AFTER the
	// load returns; staged Apply must still see frozen originals.
	loader.snap.Character.Vitals[1] = 'X'
	loader.snap.Character.Advancement[1] = 'X'
	loader.snap.Character.Spells[0].Ability = 99
	loader.snap.Character.Skills[0].Ability = 99
	loader.snap.Character.Spells = append(loader.snap.Character.Spells,
		store.CharacterSpellSnapshot{SpellID: 9, Ability: 9})
	*loader.snap.Pending.CorpseID = 12345
	loader.snap.Pending.EffectiveCost = 0
	loader.snap.Pending = nil
	if err := cand.Apply(); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if string(applied.Character.Vitals) != `{"hp":1}` ||
		string(applied.Character.Advancement) != `{"pts":0}` {
		t.Fatalf("vitals/advancement mutated: %s %s", applied.Character.Vitals, applied.Character.Advancement)
	}
	if len(applied.Character.Spells) != 1 || applied.Character.Spells[0].Ability != 10 ||
		len(applied.Character.Skills) != 1 || applied.Character.Skills[0].Ability != 20 {
		t.Fatalf("abilities mutated: %+v %+v", applied.Character.Spells, applied.Character.Skills)
	}
	if applied.Pending == nil || applied.Pending.EffectiveCost != 40 ||
		applied.Pending.CorpseID == nil || *applied.Pending.CorpseID != 77 {
		t.Fatalf("pending mutated: %+v", applied.Pending)
	}
}

func TestDeathCharacterReloadLoaderError(t *testing.T) {
	loader := &fakeDeathCharacterLoader{err: errFakeLoad}
	reload := DeathCharacterReload(loader, 7, func(store.DeathCharacterRecoverySnapshot) error {
		t.Error("apply must not run on loader error")
		return nil
	})
	if _, err := reload(context.Background()); !errors.Is(err, errFakeLoad) {
		t.Fatalf("err = %v, want loader cause", err)
	}
}

func TestDeathItemReloadStaged(t *testing.T) {
	loader := &fakeDeathItemLoader{snap: hostileDeathItem()}
	var applied store.DeathItemRecoverySnapshot
	mutated := false
	reload := DeathItemReload(loader, 9, func(s store.DeathItemRecoverySnapshot) error {
		mutated = true
		applied = s
		return nil
	})
	cand, err := reload(context.Background())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loader.calls != 1 {
		t.Fatalf("loader calls = %d, want 1", loader.calls)
	}
	if mutated {
		t.Fatal("live memory mutated during load staging")
	}
	if cand.Revision != 5 {
		t.Fatalf("candidate revision = %d, want loaded root 5", cand.Revision)
	}
	if cand.Apply == nil {
		t.Fatal("nil apply")
	}
	if err := cand.Apply(); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !mutated || applied.Item.ID != 9 || applied.Item.ExpectedRevision != 5 ||
		applied.PKProtection == nil || applied.PKProtection.VictimCharacterID != 7 {
		t.Fatalf("applied = %+v", applied)
	}
}

func TestDeathItemReloadImmutable(t *testing.T) {
	loader := &fakeDeathItemLoader{snap: hostileDeathItem()}
	var applied store.DeathItemRecoverySnapshot
	reload := DeathItemReload(loader, 9, func(s store.DeathItemRecoverySnapshot) error {
		applied = s
		return nil
	})
	cand, err := reload(context.Background())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	loader.snap.Item.Enchants[1] = 'X'
	*loader.snap.Item.Location.PosX = -1
	*loader.snap.Item.Location.Slot = "hand"
	loader.snap.Item.Location.PosY = nil
	loader.snap.PKProtection.VictimCharacterID = 12345
	loader.snap.PKProtection = nil
	if err := cand.Apply(); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if string(applied.Item.Enchants) != `{"e":1}` {
		t.Fatalf("enchants mutated: %s", applied.Item.Enchants)
	}
	loc := applied.Item.Location
	if loc.PosX == nil || *loc.PosX != 7000 || loc.PosY == nil || *loc.PosY != 8000 ||
		loc.PosZ == nil || *loc.PosZ != 9000 || loc.Slot == nil || *loc.Slot != "pack" {
		t.Fatalf("location mutated: %+v", loc)
	}
	if applied.PKProtection == nil || applied.PKProtection.VictimCharacterID != 7 ||
		!applied.PKProtection.ExpiresAt.Equal(hostileExpiry()) {
		t.Fatalf("protection mutated: %+v", applied.PKProtection)
	}
}

func TestDeathItemReloadLoaderError(t *testing.T) {
	loader := &fakeDeathItemLoader{err: errFakeLoad}
	reload := DeathItemReload(loader, 9, func(store.DeathItemRecoverySnapshot) error {
		t.Error("apply must not run on loader error")
		return nil
	})
	if _, err := reload(context.Background()); !errors.Is(err, errFakeLoad) {
		t.Fatalf("err = %v, want loader cause", err)
	}
}

// blockSaverForDeathRecovery drives one key into the
// reconcile-blocked state through a stale flush, mirroring the
// bank proof: durable moved ahead while the saver still knows the
// old revision.
func blockSaverForDeathRecovery(t *testing.T, fs *fakeSnapshotStore, s *sim.Saver, key sim.AggregateKey, job SnapshotJob) {
	t.Helper()
	if err := s.Track(key, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDirty(key, job.Write); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushDirty(context.Background()); !errors.Is(err, sim.ErrSnapshotStale) {
		t.Fatalf("flush err = %v, want stale", err)
	}
}

func TestDeathCharacterReconcileSaverSuccess(t *testing.T) {
	ctx := context.Background()
	fs := newFakeSnapshotStore()
	fs.charRev[7] = 1
	s := mustSaverForPersist(t)
	key := sim.AggregateKey{Kind: sim.AggregateCharacter, ID: 7}
	job, err := NewCharacterSnapshotJob(fs, store.CharacterSnapshot{ID: 7, Karma: 1})
	if err != nil {
		t.Fatal(err)
	}
	blockSaverForDeathRecovery(t, fs, s, key, job)
	loader := &fakeDeathCharacterLoader{snap: hostileDeathCharacter()}
	// Authoritative PG moved to 4; the staged candidate bears it.
	loader.snap.Character.ExpectedRevision = 4
	state := mustReconcileState(t, 0)
	var mem store.DeathCharacterRecoverySnapshot
	haveMem := false
	if err := ReconcileSaver(ctx, state, s, key, DeathCharacterReload(loader, 7,
		func(snap store.DeathCharacterRecoverySnapshot) error {
			mem = snap
			haveMem = true
			return nil
		})); err != nil {
		t.Fatalf("ReconcileSaver: %v", err)
	}
	if !haveMem || mem.Character.ExpectedRevision != 4 || mem.Pending == nil ||
		mem.Pending.EffectiveCost != 40 {
		t.Fatalf("mem = %+v", mem)
	}
	rs := state.Snapshot()
	if rs.Pending || rs.KnownRevision != 4 {
		t.Fatalf("t3c = %+v, want clear known4", rs)
	}
	ss, _ := s.Inspect(key)
	if ss.Blocked || ss.Dirty || ss.KnownRevision != 4 {
		t.Fatalf("saver = %+v, want clear known4 clean", ss)
	}
}

func TestDeathItemReconcileSaverSuccess(t *testing.T) {
	ctx := context.Background()
	fs := newFakeSnapshotStore()
	fs.itemRev[9] = 1
	s := mustSaverForPersist(t)
	key := sim.AggregateKey{Kind: sim.AggregateItem, ID: 9}
	job, err := NewItemSnapshotJob(fs, store.ItemSnapshot{ID: 9, Qty: 1})
	if err != nil {
		t.Fatal(err)
	}
	blockSaverForDeathRecovery(t, fs, s, key, job)
	loader := &fakeDeathItemLoader{snap: hostileDeathItem()}
	loader.snap.Item.ExpectedRevision = 5
	state := mustReconcileState(t, 0)
	var mem store.DeathItemRecoverySnapshot
	haveMem := false
	if err := ReconcileSaver(ctx, state, s, key, DeathItemReload(loader, 9,
		func(snap store.DeathItemRecoverySnapshot) error {
			mem = snap
			haveMem = true
			return nil
		})); err != nil {
		t.Fatalf("ReconcileSaver: %v", err)
	}
	if !haveMem || mem.Item.ExpectedRevision != 5 || mem.PKProtection == nil ||
		mem.PKProtection.VictimCharacterID != 7 {
		t.Fatalf("mem = %+v", mem)
	}
	rs := state.Snapshot()
	if rs.Pending || rs.KnownRevision != 5 {
		t.Fatalf("t3c = %+v, want clear known5", rs)
	}
	ss, _ := s.Inspect(key)
	if ss.Blocked || ss.Dirty || ss.KnownRevision != 5 {
		t.Fatalf("saver = %+v, want clear known5 clean", ss)
	}
}

func TestDeathRecoveryReconcileSaverLoaderFailure(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"character", "item"} {
		t.Run(name, func(t *testing.T) {
			fs := newFakeSnapshotStore()
			s := mustSaverForPersist(t)
			var reload sim.ReloadFunc
			var key sim.AggregateKey
			applied := false
			if name == "character" {
				fs.charRev[7] = 1
				job, err := NewCharacterSnapshotJob(fs, store.CharacterSnapshot{ID: 7})
				if err != nil {
					t.Fatal(err)
				}
				key = sim.AggregateKey{Kind: sim.AggregateCharacter, ID: 7}
				blockSaverForDeathRecovery(t, fs, s, key, job)
				loader := &fakeDeathCharacterLoader{err: errFakeLoad}
				reload = DeathCharacterReload(loader, 7,
					func(store.DeathCharacterRecoverySnapshot) error {
						applied = true
						return nil
					})
			} else {
				fs.itemRev[9] = 1
				job, err := NewItemSnapshotJob(fs, store.ItemSnapshot{ID: 9})
				if err != nil {
					t.Fatal(err)
				}
				key = sim.AggregateKey{Kind: sim.AggregateItem, ID: 9}
				blockSaverForDeathRecovery(t, fs, s, key, job)
				loader := &fakeDeathItemLoader{err: errFakeLoad}
				reload = DeathItemReload(loader, 9,
					func(store.DeathItemRecoverySnapshot) error {
						applied = true
						return nil
					})
			}
			state := mustReconcileState(t, 0)
			if err := ReconcileSaver(ctx, state, s, key, reload); !errors.Is(err, errFakeLoad) {
				t.Fatalf("err = %v, want loader cause", err)
			}
			if applied {
				t.Fatal("apply ran despite loader failure")
			}
			if rs := state.Snapshot(); !rs.Pending {
				t.Fatalf("t3c = %+v, want still pending", rs)
			}
			if ss, _ := s.Inspect(key); !ss.Blocked {
				t.Fatalf("saver = %+v, want still blocked", ss)
			}
		})
	}
}
