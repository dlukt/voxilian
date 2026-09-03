package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dlukt/voxilian/internal/store/gen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestCorpseQueries(t *testing.T) {
	pool, q := openQueries(t)
	ctx := context.Background()
	acct, err := q.CreateAccount(ctx, gen.CreateAccountParams{KeycloakSub: "sub-corpse"})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := createCharacter(ctx, q, validCharParams(acct.ID, 0, "Dead"))
	if err != nil {
		t.Fatal(err)
	}

	exp := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	corpse, err := q.InsertCorpse(ctx, gen.InsertCorpseParams{
		CharacterID: ch.ID, PosX: 5_000_000_000, PosY: -5_000_000_000, PosZ: 0,
		ExpiresAt: pgtype.Timestamptz{Time: exp, Valid: true},
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if corpse.ID <= 0 || !corpse.CreatedAt.Valid {
		t.Fatalf("corpse = %+v", corpse)
	}
	got, err := q.GetCorpseByID(ctx, corpse.ID)
	if err != nil || got.PosX != 5_000_000_000 || !got.ExpiresAt.Time.Equal(exp) {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if _, err := q.GetCorpseByID(ctx, corpse.ID+999999); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing corpse err = %v, want NoRows", err)
	}

	// Expiry scan: earlier + same-expiry-tiebreak by id, future excluded.
	earlier := exp.Add(-time.Hour)
	c0, err := q.InsertCorpse(ctx, gen.InsertCorpseParams{CharacterID: ch.ID, ExpiresAt: pgtype.Timestamptz{Time: earlier, Valid: true}})
	if err != nil {
		t.Fatal(err)
	}
	future, err := q.InsertCorpse(ctx, gen.InsertCorpseParams{CharacterID: ch.ID, ExpiresAt: pgtype.Timestamptz{Time: exp.Add(time.Hour), Valid: true}})
	if err != nil {
		t.Fatal(err)
	}
	list, err := q.ListExpiredCorpses(ctx, pgtype.Timestamptz{Time: exp, Valid: true})
	if err != nil || len(list) != 2 || list[0].ID != c0.ID || list[1].ID != corpse.ID {
		t.Fatalf("expired = %+v, %v", list, err)
	}

	// Delete unreferenced works; referenced fails FK (no cascade).
	if err := q.DeleteCorpse(ctx, future.ID); err != nil {
		t.Fatalf("delete unreferenced: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO item_protos (id,kind,slot,base,version) VALUES (710,0,NULL,'{}',1)
		ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	root, _, err := createItemWithLocation(ctx, pool,
		gen.InsertItemInstanceParams{Proto: 710, Qty: 1, Hits: 1, Enchants: []byte("{}")},
		NewItemLocation{Kind: 2, CorpseID: int8(corpse.ID)},
	)
	if err != nil {
		t.Fatal(err)
	}
	_ = root
	if err := q.DeleteCorpse(ctx, corpse.ID); err == nil {
		t.Fatal("referenced corpse deleted (cascade?)")
	} else {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
			t.Fatalf("err = %v, want FK violation", err)
		}
	}
}

func TestBankQueries(t *testing.T) {
	_, q := openQueries(t)
	ctx := context.Background()
	a1, err := q.CreateAccount(ctx, gen.CreateAccountParams{KeycloakSub: "sub-bank1"})
	if err != nil {
		t.Fatal(err)
	}
	a2, err := q.CreateAccount(ctx, gen.CreateAccountParams{KeycloakSub: "sub-bank2"})
	if err != nil {
		t.Fatal(err)
	}
	c1, err := createCharacter(ctx, q, validCharParams(a1.ID, 0, "Rich"))
	if err != nil {
		t.Fatal(err)
	}
	c2, err := createCharacter(ctx, q, validCharParams(a2.ID, 0, "Richb"))
	if err != nil {
		t.Fatal(err)
	}

	bank, err := q.InsertBank(ctx, gen.InsertBankParams{CharacterID: c1.ID, System: "tos", Balance: 5_000_000_000})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if bank.Revision != 0 || bank.Balance != 5_000_000_000 {
		t.Fatalf("bank = %+v", bank)
	}
	got, err := q.GetBank(ctx, gen.GetBankParams{CharacterID: c1.ID, System: "tos"})
	if err != nil || got.Balance != bank.Balance {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if _, err := q.GetBank(ctx, gen.GetBankParams{CharacterID: c1.ID, System: "nope"}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing bank err = %v, want NoRows", err)
	}

	if _, err := q.InsertBank(ctx, gen.InsertBankParams{CharacterID: c1.ID, System: "koc", Balance: -250}); err != nil {
		t.Fatalf("second system / negative: %v", err)
	}
	if _, err := q.InsertBank(ctx, gen.InsertBankParams{CharacterID: c2.ID, System: "tos", Balance: 0}); err != nil {
		t.Fatalf("other char same system: %v", err)
	}
	list, err := q.ListBanksByCharacter(ctx, c1.ID)
	if err != nil || len(list) != 2 || list[0].System != "koc" || list[1].System != "tos" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if _, err := q.InsertBank(ctx, gen.InsertBankParams{CharacterID: c1.ID, System: "tos", Balance: 0}); err == nil {
		t.Fatal("duplicate bank accepted")
	} else {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			t.Fatalf("err = %v, want unique violation", err)
		}
	}
	if _, err := q.InsertBank(ctx, gen.InsertBankParams{CharacterID: 999999, System: "tos", Balance: 0}); err == nil {
		t.Fatal("bad character accepted")
	}
}
