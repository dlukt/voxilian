package persist

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dlukt/voxilian/internal/store"
)

// M5-T5c4 real-PostgreSQL 18 reconnect bootstrap proof
// (spec §9.5.1l L10/L11/L13): LoadPlayerBootstrap over a
// real migrated database maps materialized character +
// pending state exactly, sees Pending == nil when no row
// exists (incl. after a penalties-commit-shaped delete),
// and hydrates the exact Portal-committed shape. The
// real store.PGStore satisfies PlayerBootstrapStore.

func pgBootstrapState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, charID int64) {
	t.Helper()
	vitals := `{"hp":20,"base_max":20,"max":20,"mana":20,"max_mana":20,"vigor":100,"threshold":80,"exertion":0,"stomach":0}`
	if _, err := pool.Exec(ctx, `UPDATE characters SET pos_x=$1,pos_y=$2,pos_z=$3,vitals=$4::jsonb,advancement=$5::jsonb,flags=$6,karma=$7 WHERE id=$8`,
		400500, -1000, -300250, vitals, `{"pts":3}`, 5, 11, charID); err != nil {
		t.Fatalf("update character: %v", err)
	}
	pgAbilityProtos(t, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO character_spells (character_id,spell_id,ability,atrophy_flag) VALUES ($1,1,50,true)`, charID); err != nil {
		t.Fatalf("insert spell: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO character_skills (character_id,skill_id,ability,atrophy_flag) VALUES ($1,2,20,false)`, charID); err != nil {
		t.Fatalf("insert skill: %v", err)
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
	pgBootstrapState(t, ctx, pool, charID)

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
		if boot.Pending != nil {
			t.Fatalf("pending = %+v; want nil (no row invented)", boot.Pending)
		}
	})

	t.Run("portal-committed-pending", func(t *testing.T) {
		// Portal-of-Life commit shape: cost lowered,
		// portal_used set, corpse association kept.
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
	})

	t.Run("penalty-committed-pending-absent", func(t *testing.T) {
		// CommitDeathPenalties deletes the pending row in
		// the same transaction: reconnect must see
		// Pending == nil, never a guessed row.
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
	})
}
