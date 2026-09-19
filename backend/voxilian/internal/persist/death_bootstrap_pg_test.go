package persist

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/store"
)

// M5-T5c4 real-PostgreSQL 18 reconnect bootstrap proof
// (spec §9.5.1m C1/C2/C5): LoadPlayerBootstrap over a
// real migrated database maps the COMPLETE materialized
// character state exactly — real base Stamina/Mysticism
// (deliberately NOT 10/10), the exact carried inventory
// item set (contained/vault rows excluded), spell +
// skill — and sees Pending == nil when no row exists
// (incl. after a penalties-commit-shaped delete) while
// hydrating the exact Portal-committed shape. The real
// store.PGStore satisfies PlayerBootstrapStore.

func pgBootstrapState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, charID int64) (bagID, swordID int64) {
	t.Helper()
	vitals := `{"hp":20,"base_max":20,"max":20,"mana":20,"max_mana":20,"vigor":100,"threshold":80,"exertion":0,"stomach":0}`
	if _, err := pool.Exec(ctx, `UPDATE characters SET pos_x=$1,pos_y=$2,pos_z=$3,vitals=$4::jsonb,advancement=$5::jsonb,flags=$6,karma=$7,stamina=$8,mysticism=$9 WHERE id=$10`,
		400500, -1000, -300250, vitals, `{"pts":3}`, 5, 11, 7, 42, charID); err != nil {
		t.Fatalf("update character: %v", err)
	}
	pgAbilityProtos(t, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO item_protos (id,kind,slot,base,version) VALUES (1001,0,NULL,'{}',1),(1002,0,NULL,'{}',1),(1003,0,NULL,'{}',1),(1004,0,NULL,'{}',1) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO character_spells (character_id,spell_id,ability,atrophy_flag) VALUES ($1,1,50,true)`, charID); err != nil {
		t.Fatalf("insert spell: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO character_skills (character_id,skill_id,ability,atrophy_flag) VALUES ($1,2,20,false)`, charID); err != nil {
		t.Fatalf("insert skill: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO item_instances (proto,qty,hits,enchants) VALUES (1001,3,250,'{"glow":1}') RETURNING id`).Scan(&bagID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO item_locations (item_id,kind,character_id,slot) VALUES ($1,0,$2,'hand')`, bagID, charID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO item_instances (proto,qty,hits,enchants) VALUES (1002,500,0,'{}') RETURNING id`).Scan(&swordID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO item_locations (item_id,kind,character_id,slot) VALUES ($1,0,$2,'pack')`, swordID, charID); err != nil {
		t.Fatal(err)
	}
	// Never carried inventory: a container-contained gem
	// and a vault holding. Neither may leak into
	// Durable.Items.
	var gemID int64
	if err := pool.QueryRow(ctx, `INSERT INTO item_instances (proto,qty,hits,enchants) VALUES (1003,1,100,'{"bane":true}') RETURNING id`).Scan(&gemID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO item_locations (item_id,kind,container_item_id,slot) VALUES ($1,4,$2,'pocket')`, gemID, bagID); err != nil {
		t.Fatal(err)
	}
	var vaultID int64
	if err := pool.QueryRow(ctx, `INSERT INTO item_instances (proto,qty,hits,enchants) VALUES (1004,7,7,'{}') RETURNING id`).Scan(&vaultID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO item_locations (item_id,kind,character_id,vault_region,slot) VALUES ($1,3,$2,'tos','vault')`, vaultID, charID); err != nil {
		t.Fatal(err)
	}
	return bagID, swordID
}

// checkBootstrapInventory asserts the exact complete
// carried inventory set in ascending item-id order.
func checkBootstrapInventory(t *testing.T, boot sim.PlayerRecoveryBootstrap, bagID, swordID int64) {
	t.Helper()
	if len(boot.Durable.Items) != 2 {
		t.Fatalf("items = %+v; want exactly the 2 carried items", boot.Durable.Items)
	}
	want := []sim.PlayerInventoryItemState{
		{ID: bagID, ProtoID: 1001, Qty: 3, Hits: 250, Enchants: []byte(`{"glow":1}`), Slot: "hand"},
		{ID: swordID, ProtoID: 1002, Qty: 500, Hits: 0, Enchants: []byte(`{}`), Slot: "pack"},
	}
	for i, w := range want {
		got := boot.Durable.Items[i]
		if got.ID != w.ID || got.ProtoID != w.ProtoID || got.Qty != w.Qty ||
			got.Hits != w.Hits || got.Slot != w.Slot {
			t.Fatalf("items[%d] = %+v; want %+v", i, got, w)
		}
		if string(got.Enchants) != string(w.Enchants) &&
			!(string(got.Enchants) == `{"glow": 1}` && string(w.Enchants) == `{"glow":1}`) {
			t.Fatalf("items[%d] enchants = %s; want %s", i, got.Enchants, w.Enchants)
		}
	}
	if boot.Durable.Items[0].ID > boot.Durable.Items[1].ID {
		t.Fatalf("items out of ascending id order: %+v", boot.Durable.Items)
	}
}

// checkBootstrapRuntimeInputs asserts the §9.5.1m C5
// resolution: real durable base stats, neutral powers,
// ordinary multiplier — never magic 10/10.
func checkBootstrapRuntimeInputs(t *testing.T, boot sim.PlayerRecoveryBootstrap) {
	t.Helper()
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
}

func TestLoadPlayerBootstrapPG(t *testing.T) {
	ctx := context.Background()
	pool, q := openPG(t)
	st, err := store.New(pool, newPGRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	charID := pgAccountChar(t, q, "t5c4-bootstrap", "T5c4Boot")
	bagID, swordID := pgBootstrapState(t, ctx, pool, charID)

	t.Run("no-pending-row", func(t *testing.T) {
		boot, err := LoadPlayerBootstrap(ctx, st, charID)
		if err != nil {
			t.Fatalf("LoadPlayerBootstrap: %v", err)
		}
		if int64(boot.CharacterID) != charID {
			t.Fatalf("char = %d; want %d", int64(boot.CharacterID), charID)
		}
		if boot.Position.X != 400.5 || boot.Position.Y != -1 || boot.Position.Z != -300.25 {
			t.Fatalf("pos = %+v; want {400.5 -1 -300.25}", boot.Position)
		}
		if boot.Vitals.HP != 20 || boot.Vitals.MaxMana != 20 || boot.Vitals.RestThreshold != 80 {
			t.Fatalf("vitals = %+v", boot.Vitals)
		}
		if boot.Durable.Karma != 11 || boot.Durable.Flags != 5 {
			t.Fatalf("durable = %+v", boot.Durable)
		}
		if len(boot.Durable.Spells) != 1 || boot.Durable.Spells[0].Ability != 50 {
			t.Fatalf("spells = %+v", boot.Durable.Spells)
		}
		checkBootstrapRuntimeInputs(t, boot)
		checkBootstrapInventory(t, boot, bagID, swordID)
		if boot.Pending != nil {
			t.Fatalf("pending = %+v; want nil (no row invented)", boot.Pending)
		}
	})

	t.Run("portal-committed-pending", func(t *testing.T) {
		// Portal-of-Life commit shape: cost lowered,
		// portal_used set, corpse association kept.
		// Pending recovery must not mask inventory loss:
		// the carried set is asserted here too.
		if _, err := pool.Exec(ctx, `INSERT INTO pending_deaths (character_id,effective_cost,death_time_seconds,corpse_id,portal_used) VALUES ($1,35,2000,NULL,true)`, charID); err != nil {
			t.Fatalf("insert pending: %v", err)
		}
		boot, err := LoadPlayerBootstrap(ctx, st, charID)
		if err != nil {
			t.Fatalf("LoadPlayerBootstrap: %v", err)
		}
		if boot.Pending == nil {
			t.Fatalf("pending = nil; want Portal-committed row")
		}
		if boot.Pending.EffectiveCost != 35 || boot.Pending.DeathTimeSeconds != 2000 ||
			boot.Pending.CorpseID != nil || !boot.Pending.PortalUsed {
			t.Fatalf("pending = %+v; want cost 35, no corpse, portal used", boot.Pending)
		}
		checkBootstrapRuntimeInputs(t, boot)
		checkBootstrapInventory(t, boot, bagID, swordID)
	})

	t.Run("penalty-committed-pending-absent", func(t *testing.T) {
		// CommitDeathPenalties deletes the pending row in
		// the same transaction: reconnect must see
		// Pending == nil, never a guessed row — with the
		// carried set still intact.
		if _, err := pool.Exec(ctx, `DELETE FROM pending_deaths WHERE character_id=$1`, charID); err != nil {
			t.Fatalf("delete pending: %v", err)
		}
		boot, err := LoadPlayerBootstrap(ctx, st, charID)
		if err != nil {
			t.Fatalf("LoadPlayerBootstrap: %v", err)
		}
		if boot.Pending != nil {
			t.Fatalf("pending = %+v; want nil after penalty-shaped delete", boot.Pending)
		}
		checkBootstrapRuntimeInputs(t, boot)
		checkBootstrapInventory(t, boot, bagID, swordID)
	})
}
