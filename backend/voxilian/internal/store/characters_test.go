package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dlukt/voxilian/internal/simtest"
	"github.com/dlukt/voxilian/internal/store/gen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

func pgText(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: true}
}

func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}

// openQueries migrates a fresh disposable database to the full current
// schema (version 5) and returns a pool plus generated queries.
func openQueries(t *testing.T) (*pgxpool.Pool, *gen.Queries) {
	t.Helper()
	pg := simtest.StartPostgres18(t)
	sqldb, err := sql.Open("pgx", pg.DSN)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sqldb.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(simtest.RepoRoot(t), "backend", "voxilian", "migrations")
	if err := goose.UpTo(sqldb, dir, 5); err != nil {
		t.Fatalf("migrate to 5: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), pg.DSN)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, gen.New(pool)
}

func validCharParams(acct int64, slot int16, name string) gen.InsertCharacterParams {
	return gen.InsertCharacterParams{
		AccountID: acct, Slot: slot, Name: name, Gender: 0,
		Face: []byte("{}"), Might: 10, Intellect: 10, Stamina: 10,
		Agility: 10, Mysticism: 10, Aim: 10, Karma: 0, Hometown: "tos",
		PosX: 5_000_000_000, PosY: -5_000_000_000, PosZ: 0,
		Vitals: []byte("{}"), Advancement: []byte("{}"), Flags: 0,
	}
}

func TestAccountQueries(t *testing.T) {
	_, q := openQueries(t)
	ctx := context.Background()

	// 1-2: creation succeeds with generated ID.
	acct, err := q.CreateAccount(ctx, gen.CreateAccountParams{
		KeycloakSub: "sub-t6a",
		Email:       pgText("a@example.com"),
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if acct.ID <= 0 {
		t.Fatalf("account id = %d", acct.ID)
	}

	// 3-4: both lookups return it.
	byID, err := q.GetAccountByID(ctx, acct.ID)
	if err != nil || byID.KeycloakSub != "sub-t6a" {
		t.Fatalf("GetAccountByID: %+v, %v", byID, err)
	}
	bySub, err := q.GetAccountByKeycloakSub(ctx, "sub-t6a")
	if err != nil || bySub.ID != acct.ID {
		t.Fatalf("GetAccountByKeycloakSub: %+v, %v", bySub, err)
	}

	// 5-6: nullable email (NULL) and non-null round-trip.
	nullAcct, err := q.CreateAccount(ctx, gen.CreateAccountParams{KeycloakSub: "sub-null"})
	if err != nil || nullAcct.Email.Valid {
		t.Fatalf("null email: %+v, %v", nullAcct, err)
	}
	if !acct.Email.Valid || acct.Email.String != "a@example.com" {
		t.Fatalf("email round-trip: %+v", acct.Email)
	}

	// 7: duplicate sub stays a plain unique violation, unmapped.
	if _, err := q.CreateAccount(ctx, gen.CreateAccountParams{KeycloakSub: "sub-t6a"}); err == nil {
		t.Fatal("duplicate sub accepted")
	} else {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			t.Fatalf("dup sub err = %v, want 23505", err)
		}
		if errors.Is(err, ErrSlotOccupied) || errors.Is(err, ErrNameTaken) {
			t.Fatalf("account conflict must not map to character errors: %v", err)
		}
	}
}

func TestCreateCharacterSuccess(t *testing.T) {
	_, q := openQueries(t)
	ctx := context.Background()
	acct, err := q.CreateAccount(ctx, gen.CreateAccountParams{KeycloakSub: "sub-char"})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := createCharacter(ctx, q, validCharParams(acct.ID, 0, "Hero"))
	if err != nil {
		t.Fatalf("createCharacter: %v", err)
	}
	if ch.ID <= 0 || ch.AccountID != acct.ID || ch.Slot != 0 || ch.Name != "Hero" {
		t.Fatalf("row mismatch: %+v", ch)
	}
	if ch.Revision != 0 || !ch.CreatedAt.Valid || !ch.UpdatedAt.Valid || ch.DeletedAt.Valid {
		t.Fatalf("defaults wrong: %+v", ch)
	}
	if ch.PosX != 5_000_000_000 || ch.PosY != -5_000_000_000 {
		t.Fatalf("pos = (%d,%d)", ch.PosX, ch.PosY)
	}
	if ch.Might != 10 || string(ch.Face) != "{}" {
		t.Fatalf("fields: %+v", ch)
	}
}

func TestCreateCharacterSlotConflict(t *testing.T) {
	_, q := openQueries(t)
	ctx := context.Background()
	acct, _ := q.CreateAccount(ctx, gen.CreateAccountParams{KeycloakSub: "sub-slot"})
	if _, err := createCharacter(ctx, q, validCharParams(acct.ID, 0, "Alpha")); err != nil {
		t.Fatal(err)
	}
	_, err := createCharacter(ctx, q, validCharParams(acct.ID, 0, "Bravo"))
	if !errors.Is(err, ErrSlotOccupied) {
		t.Fatalf("err = %v, want ErrSlotOccupied", err)
	}
	if errors.Is(err, ErrNameTaken) {
		t.Fatalf("slot conflict mislabeled name: %v", err)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != "chars_acct_slot_uidx" {
		t.Fatalf("cause = %v, want chars_acct_slot_uidx PgError", err)
	}
}

func TestCreateCharacterNameConflict(t *testing.T) {
	_, q := openQueries(t)
	ctx := context.Background()
	a1, _ := q.CreateAccount(ctx, gen.CreateAccountParams{KeycloakSub: "sub-n1"})
	a2, _ := q.CreateAccount(ctx, gen.CreateAccountParams{KeycloakSub: "sub-n2"})
	if _, err := createCharacter(ctx, q, validCharParams(a1.ID, 0, "Alice")); err != nil {
		t.Fatal(err)
	}
	_, err := createCharacter(ctx, q, validCharParams(a2.ID, 1, "alice"))
	if !errors.Is(err, ErrNameTaken) {
		t.Fatalf("err = %v, want ErrNameTaken (CITEXT)", err)
	}
	if errors.Is(err, ErrSlotOccupied) {
		t.Fatalf("name conflict mislabeled slot: %v", err)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != "chars_name_uidx" {
		t.Fatalf("cause = %v, want chars_name_uidx PgError", err)
	}
}

func TestCreateCharacterUnrelatedErrors(t *testing.T) {
	_, q := openQueries(t)
	ctx := context.Background()
	// Bad account → FK violation, unmapped.
	if _, err := createCharacter(ctx, q, validCharParams(999999, 0, "Nope")); err == nil {
		t.Fatal("bad account accepted")
	} else if errors.Is(err, ErrSlotOccupied) || errors.Is(err, ErrNameTaken) {
		t.Fatalf("FK error mapped: %v", err)
	}
	// Bad stat → CHECK violation, unmapped, original preserved.
	acct, _ := q.CreateAccount(ctx, gen.CreateAccountParams{KeycloakSub: "sub-chk"})
	p := validCharParams(acct.ID, 0, "Weak")
	p.Might = 0
	if _, err := createCharacter(ctx, q, p); err == nil {
		t.Fatal("might=0 accepted")
	} else if errors.Is(err, ErrSlotOccupied) || errors.Is(err, ErrNameTaken) {
		t.Fatalf("CHECK error mapped: %v", err)
	} else if !contains(err.Error(), "characters_might_check") {
		t.Fatalf("original error lost: %v", err)
	}
}

func TestCharacterReads(t *testing.T) {
	pool, q := openQueries(t)
	ctx := context.Background()
	a1, _ := q.CreateAccount(ctx, gen.CreateAccountParams{KeycloakSub: "sub-r1"})
	a2, _ := q.CreateAccount(ctx, gen.CreateAccountParams{KeycloakSub: "sub-r2"})
	c0, _ := createCharacter(ctx, q, validCharParams(a1.ID, 0, "Zero"))
	c1, _ := createCharacter(ctx, q, validCharParams(a1.ID, 1, "One"))
	other, _ := createCharacter(ctx, q, validCharParams(a2.ID, 0, "Other"))

	// ListLiveCharactersByAccount: own account only, slot order.
	list, err := q.ListLiveCharactersByAccount(ctx, a1.ID)
	if err != nil || len(list) != 2 || list[0].ID != c0.ID || list[1].ID != c1.ID {
		t.Fatalf("list = %+v, %v", list, err)
	}
	_ = other

	// Soft-delete via raw test SQL (no production delete path by design).
	if _, err := pool.Exec(ctx, `UPDATE characters SET deleted_at = now() WHERE id = $1`, c0.ID); err != nil {
		t.Fatal(err)
	}
	list, err = q.ListLiveCharactersByAccount(ctx, a1.ID)
	if err != nil || len(list) != 1 || list[0].ID != c1.ID {
		t.Fatalf("list after delete = %+v, %v", list, err)
	}

	// GetCharacterByID still returns the soft-deleted row.
	got, err := q.GetCharacterByID(ctx, c0.ID)
	if err != nil || got.ID != c0.ID {
		t.Fatalf("GetCharacterByID deleted: %+v, %v", got, err)
	}

	// GetLiveCharacterForAccount: owner OK; wrong account / deleted → NoRows.
	if _, err := q.GetLiveCharacterForAccount(ctx, gen.GetLiveCharacterForAccountParams{ID: c1.ID, AccountID: a1.ID}); err != nil {
		t.Fatalf("live own: %v", err)
	}
	if _, err := q.GetLiveCharacterForAccount(ctx, gen.GetLiveCharacterForAccountParams{ID: c1.ID, AccountID: a2.ID}); !isNoRows(err) {
		t.Fatalf("wrong account err = %v, want NoRows", err)
	}
	if _, err := q.GetLiveCharacterForAccount(ctx, gen.GetLiveCharacterForAccountParams{ID: c0.ID, AccountID: a1.ID}); !isNoRows(err) {
		t.Fatalf("deleted err = %v, want NoRows", err)
	}
}
