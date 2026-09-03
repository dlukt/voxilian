package store

import (
	"context"
	"errors"
	"testing"

	"github.com/dlukt/voxilian/internal/store/gen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func int8(v int64) pgtype.Int8 {
	return pgtype.Int8{Int64: v, Valid: true}
}

func text(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: true}
}

func itemCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM item_instances`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCreateItemWithLocationSuccess(t *testing.T) {
	pool, q := openQueries(t)
	ctx := context.Background()
	acct, err := q.CreateAccount(ctx, gen.CreateAccountParams{KeycloakSub: "sub-item"})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := createCharacter(ctx, q, validCharParams(acct.ID, 0, "Holder"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO item_protos (id,kind,slot,base,version) VALUES (700,0,'mainhand','{}',1)`); err != nil {
		t.Fatal(err)
	}

	root, placed, err := createItemWithLocation(ctx, pool,
		gen.InsertItemInstanceParams{Proto: 700, Qty: 3, Hits: 100, Enchants: []byte("{}")},
		NewItemLocation{Kind: 0, CharacterID: int8(ch.ID), Slot: text("mainhand")},
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if root.ID <= 0 || root.Revision != 0 || root.Proto != 700 || root.Qty != 3 {
		t.Fatalf("root = %+v", root)
	}
	got, err := q.GetItemInstanceByID(ctx, root.ID)
	if err != nil || got.ID != root.ID {
		t.Fatalf("get root: %+v, %v", got, err)
	}
	loc, err := q.GetItemLocationByItemID(ctx, root.ID)
	if err != nil {
		t.Fatalf("get location: %v", err)
	}
	if loc.ItemID != root.ID || loc.Kind != 0 || !loc.CharacterID.Valid || loc.CharacterID.Int64 != ch.ID || !loc.Slot.Valid || loc.Slot.String != "mainhand" {
		t.Fatalf("location = %+v", loc)
	}
	if placed.ItemID != root.ID {
		t.Fatalf("placed = %+v", placed)
	}

	// Missing location row reads as NoRows (transaction/failure fixture shape).
	if _, err := q.GetItemLocationByItemID(ctx, root.ID+999999); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing location err = %v, want NoRows", err)
	}
}

func TestCreateItemWithLocationRollback(t *testing.T) {
	pool, _ := openQueries(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO item_protos (id,kind,slot,base,version) VALUES (701,0,NULL,'{}',1)`); err != nil {
		t.Fatal(err)
	}
	before := itemCount(t, pool)

	// Inventory kind with NULL character/slot violates the location CHECK.
	_, _, err := createItemWithLocation(ctx, pool,
		gen.InsertItemInstanceParams{Proto: 701, Qty: 1, Hits: 1, Enchants: []byte("{}")},
		NewItemLocation{Kind: 0},
	)
	if err == nil {
		t.Fatal("invalid location accepted")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != "item_locations_kind_state_check" {
		t.Fatalf("err = %v, want kind_state_check PgError", err)
	}
	if after := itemCount(t, pool); after != before {
		t.Fatalf("orphan root committed: count %d → %d", before, after)
	}
}

func TestGroundItemBigCoords(t *testing.T) {
	pool, _ := openQueries(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO item_protos (id,kind,slot,base,version) VALUES (702,0,NULL,'{}',1)`); err != nil {
		t.Fatal(err)
	}
	root, placed, err := createItemWithLocation(ctx, pool,
		gen.InsertItemInstanceParams{Proto: 702, Qty: 1, Hits: 1, Enchants: []byte("{}")},
		NewItemLocation{Kind: 1, PosX: int8(5_000_000_000), PosY: int8(-5_000_000_000), PosZ: int8(0)},
	)
	if err != nil {
		t.Fatalf("create ground: %v", err)
	}
	if !placed.PosX.Valid || placed.PosX.Int64 != 5_000_000_000 {
		t.Fatalf("coords = %+v", placed)
	}
	_ = root
}
