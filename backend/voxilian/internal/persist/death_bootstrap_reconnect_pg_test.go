package persist

import (
	"context"
	"testing"

	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/store"
)

// M5-T5c4 reconnect integration proof (spec §9.5.1m C1,
// M5-T5c4 B6): the lossless chain PG inventory ->
// reconnect -> live PlayerDurableState.Items ->
// subsequent death capture, using the REAL persist
// bootstrap mapping (not a hand-built fake). A second
// character with zero-HP vitals reconnects with Portal
// pending state, then a new immediate-death capture
// still carries the recovered inventory. Fresh
// reconnect life is Alive with no recovered token/ack
// state (the bootstrap value carries none).

func TestReconnectBootstrapDeathCaptureChainPG(t *testing.T) {
	ctx := context.Background()
	pool, q := openPG(t)
	st, err := store.New(pool, newPGRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	charID := pgAccountChar(t, q, "t5c4-chain", "T5c4Chain")
	vitals := `{"hp":0,"base_max":20,"max":20,"mana":20,"max_mana":20,"vigor":100,"threshold":80,"exertion":0,"stomach":0}`
	if _, err := pool.Exec(ctx, `UPDATE characters SET pos_x=$1,pos_y=$2,pos_z=$3,vitals=$4::jsonb,advancement=$5::jsonb,flags=$6,karma=$7,stamina=$8,mysticism=$9 WHERE id=$10`,
		400500, -1000, -300250, vitals, `{"pts":3}`, 5, 11, 33, 5, charID); err != nil {
		t.Fatalf("update character: %v", err)
	}
	pgAbilityProtos(t, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO item_protos (id,kind,slot,base,version) VALUES (1001,0,NULL,'{}',1),(1002,0,NULL,'{}',1) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO character_spells (character_id,spell_id,ability,atrophy_flag) VALUES ($1,1,50,true)`, charID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO character_skills (character_id,skill_id,ability,atrophy_flag) VALUES ($1,2,20,false)`, charID); err != nil {
		t.Fatal(err)
	}
	var bagID, swordID int64
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
	// Portal-committed pending shape survives reconnect.
	if _, err := pool.Exec(ctx, `INSERT INTO pending_deaths (character_id,effective_cost,death_time_seconds,corpse_id,portal_used) VALUES ($1,35,2000,NULL,true)`, charID); err != nil {
		t.Fatal(err)
	}

	// Real mapping: PG materialized state -> bootstrap.
	boot, err := LoadPlayerBootstrap(ctx, st, charID)
	if err != nil {
		t.Fatalf("LoadPlayerBootstrap: %v", err)
	}

	// Atomic sim ingress: vitals, runtime, complete
	// durable shadow, and pending install in one turn.
	e := mustSimEngine(t)
	snap, err := e.AddPlayerEntityWithRecovery(boot)
	if err != nil {
		t.Fatalf("AddPlayerEntityWithRecovery: %v", err)
	}

	// Fresh reconnect life is ordinary Alive gameplay.
	if life, ok, err := e.PlayerLifeStateOf(snap.ID); err != nil || !ok || life != sim.PlayerLifeAlive {
		t.Fatalf("life = %d,%v,%v; want Alive,true,nil", uint8(life), ok, err)
	}
	// Pending hydrates exactly (Portal-committed shape).
	pending, ok, err := e.PlayerPendingDeathOf(snap.ID)
	if err != nil || !ok {
		t.Fatalf("pending = %+v,%v,%v; want present", pending, ok, err)
	}
	if pending.EffectiveCost != 35 || pending.DeathTimeSeconds != 2000 ||
		pending.CorpseID != nil || !pending.PortalUsed {
		t.Fatalf("pending = %+v; want Portal-committed cost 35", pending)
	}
	// Live durable shadow carries the recovered inventory.
	live, ok, err := e.PlayerDurableStateOf(snap.ID)
	if err != nil || !ok {
		t.Fatalf("durable = %+v,%v,%v; want present", live, ok, err)
	}
	if len(live.Items) != 2 || live.Items[0].ID != bagID || live.Items[1].ID != swordID {
		t.Fatalf("live items = %+v; want [%d %d]", live.Items, bagID, swordID)
	}
	// Runtime inputs reflect the real character (33/5).
	rt, ok, err := e.PlayerVitalsRuntimeOf(snap.ID)
	if err != nil || !ok {
		t.Fatalf("runtime = %+v,%v,%v; want present", rt, ok, err)
	}
	if rt.Inputs.EffectiveStamina != 33 || rt.Inputs.EffectiveMysticism != 5 {
		t.Fatalf("runtime inputs = %+v; want 33/5", rt.Inputs)
	}

	// A subsequent immediate-death capture is lossless:
	// the recovered inventory is still present with 1:1
	// opaque keys in authoritative order.
	_, base, err := e.PlayerBeginImmediateDeathCapture(snap.ID)
	if err != nil {
		t.Fatalf("PlayerBeginImmediateDeathCapture: %v", err)
	}
	if len(base.Durable.Items) != 2 || base.Durable.Items[0].ID != bagID ||
		base.Durable.Items[1].ID != swordID {
		t.Fatalf("capture items = %+v; want [%d %d]", base.Durable.Items, bagID, swordID)
	}
	if len(base.ItemKeys) != 2 || base.ItemKeys[0] != 0 || base.ItemKeys[1] != 1 {
		t.Fatalf("capture keys = %v; want [0 1]", base.ItemKeys)
	}
	if base.Durable.Items[0].ProtoID != 1001 || base.Durable.Items[0].Qty != 3 ||
		base.Durable.Items[0].Hits != 250 || base.Durable.Items[0].Slot != "hand" {
		t.Fatalf("capture items[0] = %+v", base.Durable.Items[0])
	}
	if base.Durable.Items[1].ProtoID != 1002 || base.Durable.Items[1].Qty != 500 ||
		base.Durable.Items[1].Hits != 0 || base.Durable.Items[1].Slot != "pack" {
		t.Fatalf("capture items[1] = %+v", base.Durable.Items[1])
	}
}
