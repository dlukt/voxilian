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
// §9.5.1l L11): Store recovery -> sim-domain bootstrap
// mapping over a fake loader (real PG coverage lives in
// death_bootstrap_pg_test.go). No PG, no Store writes.

type fakeBootstrapStore struct {
	snap store.DeathCharacterRecoverySnapshot
	err  error
}

func (f *fakeBootstrapStore) LoadDeathCharacterRecovery(context.Context, int64) (store.DeathCharacterRecoverySnapshot, error) {
	if f.err != nil {
		return store.DeathCharacterRecoverySnapshot{}, f.err
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

func bootstrapRecovery() store.DeathCharacterRecoverySnapshot {
	corpse := int64(77)
	return store.DeathCharacterRecoverySnapshot{
		Character: store.CharacterSnapshot{
			ID: 7, ExpectedRevision: 4, Karma: 11,
			PosX: 400500, PosY: -1000, PosZ: -300250,
			Advancement: json.RawMessage(`{"pts":3}`),
			Flags:       5,
			Spells:      []store.CharacterSpellSnapshot{{SpellID: 1, Ability: 50, AtrophyFlag: true}},
			Skills:      []store.CharacterSkillSnapshot{{SkillID: 2, Ability: 20}},
		},
		Pending: &store.PendingDeathSnapshot{
			CharacterID: 7, EffectiveCost: 40, DeathTimeSeconds: 1700000000,
			CorpseID: &corpse, PortalUsed: true,
		},
	}
}

func TestLoadPlayerBootstrap(t *testing.T) {
	withVitals := func(snap store.DeathCharacterRecoverySnapshot, t *testing.T) store.DeathCharacterRecoverySnapshot {
		t.Helper()
		snap.Character.Vitals = bootstrapVitalsJSON(t)
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
		if boot.Durable.Items != nil {
			t.Fatalf("items = %+v; want nil (item aggregates recover separately)", boot.Durable.Items)
		}
		if boot.Pending == nil || boot.Pending.EffectiveCost != 40 ||
			boot.Pending.DeathTimeSeconds != 1700000000 ||
			boot.Pending.CorpseID == nil || *boot.Pending.CorpseID != 77 ||
			!boot.Pending.PortalUsed {
			t.Fatalf("pending = %+v", boot.Pending)
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
		rec.Character.Vitals = json.RawMessage(`{"hp":-3}`)
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
		rec.Character.Advancement[2] = '9'
		if boot.Pending.CorpseID == nil || *boot.Pending.CorpseID != 77 {
			t.Fatalf("pending aliased loader memory: %+v", boot.Pending)
		}
		if string(boot.Durable.Advancement) != `{"pts":3}` {
			t.Fatalf("durable aliased loader memory: %s", boot.Durable.Advancement)
		}
	})
}
