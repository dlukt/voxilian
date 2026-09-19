package persist

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/store"
)

// M5-T5c4 reconnect bootstrap adapter tests (spec
// §9.5.1m C1/C2/C5): Store recovery -> sim-domain
// bootstrap mapping over a fake loader (real PG coverage
// lives in death_bootstrap_pg_test.go). No PG, no Store
// writes.

type fakeBootstrapStore struct {
	snap store.PlayerBootstrapRecoverySnapshot
	err  error
}

func (f *fakeBootstrapStore) LoadPlayerBootstrapRecovery(context.Context, int64) (store.PlayerBootstrapRecoverySnapshot, error) {
	if f.err != nil {
		return store.PlayerBootstrapRecoverySnapshot{}, f.err
	}
	return f.snap, nil
}

func bootstrapVitalsJSON(t *testing.T) json.RawMessage {
	t.Helper()
	v := sim.PlayerVitals{
		HP: 20, BaseMaxHP: 20, MaxHP: 20,
		Mana: 20, MaxMana: 20, Vigor: 100,
		RestThreshold: 80, Exertion: 0, Stomach: 0,
	}
	if err := v.Validate(); err != nil {
		t.Fatalf("fixture vitals invalid: %v", err)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func bootstrapRecovery() store.PlayerBootstrapRecoverySnapshot {
	corpse := int64(77)
	return store.PlayerBootstrapRecoverySnapshot{
		CharacterID: 7, ExpectedRevision: 4,
		// Deliberately NOT 10/10: the adapter must derive
		// runtime inputs from the real durable base stats.
		Stamina:   7,
		Mysticism: 42,
		Karma:     11,
		PosX:      400500, PosY: -1000, PosZ: -300250,
		Advancement: json.RawMessage(`{"pts":3}`),
		Flags:       5,
		Spells:      []store.CharacterSpellSnapshot{{SpellID: 1, Ability: 50, AtrophyFlag: true}},
		Skills:      []store.CharacterSkillSnapshot{{SkillID: 2, Ability: 20}},
		Items: []store.PlayerBootstrapItemSnapshot{
			{ID: 101, ProtoID: 1001, Qty: 3, Hits: 250, Enchants: json.RawMessage(`{"glow":1}`), Slot: "hand"},
			{ID: 202, ProtoID: 1002, Qty: 500, Hits: 0, Enchants: json.RawMessage(`{}`), Slot: "pack"},
		},
		Pending: &store.PendingDeathSnapshot{
			CharacterID: 7, EffectiveCost: 40, DeathTimeSeconds: 1700000000,
			CorpseID: &corpse, PortalUsed: true,
		},
	}
}

func TestLoadPlayerBootstrap(t *testing.T) {
	withVitals := func(snap store.PlayerBootstrapRecoverySnapshot, t *testing.T) store.PlayerBootstrapRecoverySnapshot {
		t.Helper()
		snap.Vitals = bootstrapVitalsJSON(t)
		return snap
	}

	t.Run("full-with-pending", func(t *testing.T) {
		st := &fakeBootstrapStore{snap: withVitals(bootstrapRecovery(), t)}
		boot, err := LoadPlayerBootstrap(context.Background(), st, 7)
		if err != nil {
			t.Fatalf("LoadPlayerBootstrap: %v", err)
		}
		if boot.CharacterID != sim.CharacterID(7) {
			t.Fatalf("char = %d; want 7", int64(boot.CharacterID))
		}
		if boot.Position.X != 400.5 || boot.Position.Y != -1 || boot.Position.Z != -300.25 {
			t.Fatalf("pos = %+v; want {400.5 -1 -300.25}", boot.Position)
		}
		if boot.Vitals.HP != 20 || boot.Vitals.Vigor != 100 {
			t.Fatalf("vitals = %+v", boot.Vitals)
		}
		// §9.5.1m C5: runtime inputs derive from the real
		// durable base stats (7/42), never magic 10/10;
		// powers absent, ordinary room multiplier.
		if boot.RuntimeInputs.EffectiveStamina != 7 {
			t.Fatalf("EffectiveStamina = %d; want 7 (durable base stamina)", boot.RuntimeInputs.EffectiveStamina)
		}
		if boot.RuntimeInputs.EffectiveMysticism != 42 {
			t.Fatalf("EffectiveMysticism = %d; want 42 (durable base mysticism)", boot.RuntimeInputs.EffectiveMysticism)
		}
		if boot.RuntimeInputs.RestoratePower != 0 || boot.RuntimeInputs.RejuvenatePower != 0 ||
			boot.RuntimeInputs.ManaFocusPower != 0 || boot.RuntimeInputs.InvigoratePower != 0 {
			t.Fatalf("powers = %+v; want all absent (0)", boot.RuntimeInputs)
		}
		if boot.RuntimeInputs.RestRecoveryMultiplier != 1 {
			t.Fatalf("multiplier = %d; want 1 (ordinary)", boot.RuntimeInputs.RestRecoveryMultiplier)
		}
		if err := boot.RuntimeInputs.Validate(); err != nil {
			t.Fatalf("inputs: %v", err)
		}
		if boot.Durable.Karma != 11 || boot.Durable.Flags != 5 || string(boot.Durable.Advancement) != `{"pts":3}` {
			t.Fatalf("durable = %+v", boot.Durable)
		}
		if len(boot.Durable.Spells) != 1 || boot.Durable.Spells[0].ID != 1 ||
			boot.Durable.Spells[0].Ability != 50 || !boot.Durable.Spells[0].AtrophyFlag {
			t.Fatalf("spells = %+v", boot.Durable.Spells)
		}
		if len(boot.Durable.Skills) != 1 || boot.Durable.Skills[0].ID != 2 {
			t.Fatalf("skills = %+v", boot.Durable.Skills)
		}
		// §9.5.1m C1/C2: the complete exact carried
		// inventory set installs in enumeration order.
		if len(boot.Durable.Items) != 2 {
			t.Fatalf("items = %+v; want exactly 2 carried items", boot.Durable.Items)
		}
		want := []sim.PlayerInventoryItemState{
			{ID: 101, ProtoID: 1001, Qty: 3, Hits: 250, Enchants: []byte(`{"glow":1}`), Slot: "hand"},
			{ID: 202, ProtoID: 1002, Qty: 500, Hits: 0, Enchants: []byte(`{}`), Slot: "pack"},
		}
		for i, w := range want {
			got := boot.Durable.Items[i]
			if got.ID != w.ID || got.ProtoID != w.ProtoID || got.Qty != w.Qty ||
				got.Hits != w.Hits || string(got.Enchants) != string(w.Enchants) || got.Slot != w.Slot {
				t.Fatalf("items[%d] = %+v; want %+v", i, got, w)
			}
		}
		if boot.Pending == nil || boot.Pending.EffectiveCost != 40 ||
			boot.Pending.DeathTimeSeconds != 1700000000 ||
			boot.Pending.CorpseID == nil || *boot.Pending.CorpseID != 77 ||
			!boot.Pending.PortalUsed {
			t.Fatalf("pending = %+v", boot.Pending)
		}
	})

	t.Run("empty-inventory-stays-empty", func(t *testing.T) {
		rec := withVitals(bootstrapRecovery(), t)
		rec.Items = nil
		boot, err := LoadPlayerBootstrap(context.Background(), &fakeBootstrapStore{snap: rec}, 7)
		if err != nil {
			t.Fatalf("LoadPlayerBootstrap: %v", err)
		}
		if boot.Durable.Items != nil {
			t.Fatalf("items = %+v; want nil (character owns nothing)", boot.Durable.Items)
		}
	})

	t.Run("nil-pending-stays-nil", func(t *testing.T) {
		rec := withVitals(bootstrapRecovery(), t)
		rec.Pending = nil
		boot, err := LoadPlayerBootstrap(context.Background(), &fakeBootstrapStore{snap: rec}, 7)
		if err != nil {
			t.Fatalf("LoadPlayerBootstrap: %v", err)
		}
		if boot.Pending != nil {
			t.Fatalf("pending = %+v; want nil (no row invented)", boot.Pending)
		}
	})

	t.Run("loader-error", func(t *testing.T) {
		_, err := LoadPlayerBootstrap(context.Background(),
			&fakeBootstrapStore{err: errors.New("pg down")}, 7)
		if err == nil {
			t.Fatalf("loader error swallowed")
		}
	})

	t.Run("invalid-character", func(t *testing.T) {
		st := &fakeBootstrapStore{snap: withVitals(bootstrapRecovery(), t)}
		if _, err := LoadPlayerBootstrap(context.Background(), st, 0); !errors.Is(err, sim.ErrInvalidCharacterID) {
			t.Fatalf("char 0 = %v; want ErrInvalidCharacterID", err)
		}
	})

	t.Run("id-mismatch", func(t *testing.T) {
		st := &fakeBootstrapStore{snap: withVitals(bootstrapRecovery(), t)}
		if _, err := LoadPlayerBootstrap(context.Background(), st, 8); err == nil {
			t.Fatalf("mismatched id accepted")
		}
	})

	t.Run("bad-vitals", func(t *testing.T) {
		rec := bootstrapRecovery()
		rec.Vitals = json.RawMessage(`{"hp":-3}`)
		if _, err := LoadPlayerBootstrap(context.Background(), &fakeBootstrapStore{snap: rec}, 7); err == nil {
			t.Fatalf("corrupt vitals accepted")
		}
	})

	t.Run("pending-char-mismatch", func(t *testing.T) {
		rec := withVitals(bootstrapRecovery(), t)
		rec.Pending.CharacterID = 8
		if _, err := LoadPlayerBootstrap(context.Background(), &fakeBootstrapStore{snap: rec}, 7); err == nil {
			t.Fatalf("foreign pending accepted")
		}
	})

	t.Run("hostile-snapshot-immutability", func(t *testing.T) {
		rec := withVitals(bootstrapRecovery(), t)
		st := &fakeBootstrapStore{snap: rec}
		boot, err := LoadPlayerBootstrap(context.Background(), st, 7)
		if err != nil {
			t.Fatal(err)
		}
		*rec.Pending.CorpseID = 9999
		rec.Advancement[2] = '9'
		rec.Items[0].Enchants[2] = '9'
		if boot.Pending.CorpseID == nil || *boot.Pending.CorpseID != 77 {
			t.Fatalf("pending aliased loader memory: %+v", boot.Pending)
		}
		if string(boot.Durable.Advancement) != `{"pts":3}` {
			t.Fatalf("durable aliased loader memory: %s", boot.Durable.Advancement)
		}
		if string(boot.Durable.Items[0].Enchants) != `{"glow":1}` {
			t.Fatalf("item enchants aliased loader memory: %s", boot.Durable.Items[0].Enchants)
		}
	})
}
