package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
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

func strptr(s string) *string { return &s }

// TestEnsureAccountKeepsEmail proves provisioning stores the first-seen
// email and never re-synchronizes it on later logins.
func TestEnsureAccountKeepsEmail(t *testing.T) {
	pool, q := openQueries(t)
	ctx := context.Background()
	st, err := New(pool, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	first, err := st.EnsureAccount(ctx, "sub-a", strptr("first@example.com"))
	if err != nil || first <= 0 {
		t.Fatalf("EnsureAccount new = %d, %v", first, err)
	}
	again, err := st.EnsureAccount(ctx, "sub-a", strptr("changed@example.com"))
	if err != nil || again != first {
		t.Fatalf("EnsureAccount existing = %d, %v; want %d", again, err, first)
	}
	row, err := q.GetAccountByKeycloakSub(ctx, "sub-a")
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if !row.Email.Valid || row.Email.String != "first@example.com" {
		t.Errorf("durable email = %+v, want first@example.com", row.Email)
	}
	// Absent email provisions NULL, not empty string.
	noid, err := st.EnsureAccount(ctx, "sub-b", nil)
	if err != nil || noid <= 0 || noid == first {
		t.Fatalf("EnsureAccount no-email = %d, %v", noid, err)
	}
	row, err = q.GetAccountByKeycloakSub(ctx, "sub-b")
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if row.Email.Valid {
		t.Errorf("durable email = %+v, want NULL", row.Email)
	}
	if _, err := st.EnsureAccount(ctx, "", strptr("x@y")); err == nil {
		t.Error("empty sub must fail")
	}
}

// TestEnsureAccountConcurrentFirstLogin proves simultaneous first logins
// converge on one durable row and one ID via the UNIQUE race path.
func TestEnsureAccountConcurrentFirstLogin(t *testing.T) {
	pool, _ := openQueries(t)
	ctx := context.Background()
	st, err := New(pool, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const n = 8
	ids := make([]int64, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			var email *string
			if i%2 == 0 {
				email = strptr("race@example.com")
			}
			ids[i], errs[i] = st.EnsureAccount(ctx, "race-sub", email)
		}(i)
	}
	close(start)
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if ids[i] != ids[0] || ids[0] <= 0 {
			t.Fatalf("ids = %v, want all equal positive", ids)
		}
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM accounts WHERE keycloak_sub = $1`, "race-sub").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("durable rows = %d, want exactly 1", count)
	}
}

func seedCreationProtos(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	mustUpsert(t, pool, CatalogBatch{
		Spells: []SpellProtoRecord{spellRec(7001, 1, 5, `{}`), spellRec(7002, 1, 5, `{}`)},
		Skills: []SkillProtoRecord{skillRec(7101, 1)},
		Items: []ItemProtoRecord{
			itemRec(7201, 1, strp("hand")),
			itemRec(7202, 1, strp("coins")),
		},
	}, false)
}

func validNewCharacter(accountID int64) NewCharacter {
	return NewCharacter{
		AccountID: accountID, Slot: 0, Name: "Aria", Gender: 1,
		Face:  []byte(`{"hair_style":3,"hair_color":4,"skin_tone":5,"parts":[6,7,8,9,10]}`),
		Might: 10, Intellect: 10, Stamina: 10, Agility: 10, Mysticism: 25, Aim: 10,
		Karma: 20, Hometown: "tos", PosX: 1, PosY: 2, PosZ: 3,
		Vitals:      []byte(`{"hp":20,"base_max":20,"max":20,"mana":20,"max_mana":20,"vigor":100,"threshold":80,"stomach":0}`),
		Advancement: []byte(`{}`), Flags: 0,
		Spells: []NewCharacterAbility{{ID: 7001, Ability: 50}, {ID: 7002, Ability: 30}},
		Skills: []NewCharacterAbility{{ID: 7101, Ability: 25}},
		Items: []NewCharacterItem{
			{ProtoID: 7201, Qty: 1, Hits: 100, Slot: "hand"},
			{ProtoID: 7202, Qty: 500, Hits: 0, Slot: "coins"},
		},
	}
}

func countFor(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// TestCreateCharacterRollback proves a mid-transaction failure (bad item
// proto FK after the root + abilities are already inserted) leaves no
// partial rows behind.
func TestCreateCharacterRollback(t *testing.T) {
	pool, _ := openQueries(t)
	seedCreationProtos(t, pool)
	ctx := context.Background()
	st, err := New(pool, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	acct, err := st.EnsureAccount(ctx, "rollback-sub", nil)
	if err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}
	nc := validNewCharacter(acct)
	nc.Items[0].ProtoID = 424242 // no such item proto: FK fails post-root.
	if _, err := st.CreateCharacter(ctx, nc); err == nil {
		t.Fatal("expected FK failure")
	}
	if c := countFor(t, pool, `SELECT COUNT(*) FROM characters WHERE account_id = $1`, acct); c != 0 {
		t.Errorf("character rows = %d, want 0", c)
	}
	if c := countFor(t, pool, `SELECT COUNT(*) FROM character_spells cs JOIN characters c ON c.id = cs.character_id WHERE c.account_id = $1`, acct); c != 0 {
		t.Errorf("spell rows = %d, want 0", c)
	}
	if c := countFor(t, pool, `SELECT COUNT(*) FROM character_skills cs JOIN characters c ON c.id = cs.character_id WHERE c.account_id = $1`, acct); c != 0 {
		t.Errorf("skill rows = %d, want 0", c)
	}
	if c := countFor(t, pool, `SELECT COUNT(*) FROM item_instances`); c != 0 {
		t.Errorf("item instances = %d, want 0", c)
	}
	if c := countFor(t, pool, `SELECT COUNT(*) FROM item_locations`); c != 0 {
		t.Errorf("item locations = %d, want 0", c)
	}
}

// TestCreateCharacterConflicts proves the root INSERT stays the race
// authority without SELECT preflight.
func TestCreateCharacterConflicts(t *testing.T) {
	pool, _ := openQueries(t)
	seedCreationProtos(t, pool)
	ctx := context.Background()
	st, err := New(pool, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	acct, err := st.EnsureAccount(ctx, "conflict-sub", nil)
	if err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}
	if _, err := st.CreateCharacter(ctx, validNewCharacter(acct)); err != nil {
		t.Fatalf("first create: %v", err)
	}
	dup := validNewCharacter(acct)
	dup.Name = "Bram"
	if _, err := st.CreateCharacter(ctx, dup); !errors.Is(err, ErrSlotOccupied) {
		t.Errorf("same-slot err = %v, want ErrSlotOccupied", err)
	}
	dup = validNewCharacter(acct)
	dup.Slot = 1
	if _, err := st.CreateCharacter(ctx, dup); !errors.Is(err, ErrNameTaken) {
		t.Errorf("same-name err = %v, want ErrNameTaken", err)
	}
}

// TestSoftDeleteLifecycle proves soft-delete keeps the row + children,
// advances revision, hides the row from live views, and frees slot/name.
func TestSoftDeleteLifecycle(t *testing.T) {
	pool, _ := openQueries(t)
	seedCreationProtos(t, pool)
	ctx := context.Background()
	st, err := New(pool, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	acct, err := st.EnsureAccount(ctx, "delete-sub", nil)
	if err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}
	id, err := st.CreateCharacter(ctx, validNewCharacter(acct))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	live, err := st.ListLiveCharacters(ctx, acct)
	if err != nil || len(live) != 1 || live[0].Revision != 0 {
		t.Fatalf("live = %+v, %v; want one revision-0 row", live, err)
	}
	newRev, err := st.SoftDeleteCharacter(ctx, id, 0)
	if err != nil || newRev != 1 {
		t.Fatalf("delete = %d, %v; want revision 1", newRev, err)
	}
	var deletedAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `SELECT deleted_at FROM characters WHERE id = $1`, id).Scan(&deletedAt); err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if !deletedAt.Valid {
		t.Error("deleted_at is NULL after soft-delete")
	}
	if live, err := st.ListLiveCharacters(ctx, acct); err != nil || len(live) != 0 {
		t.Errorf("live after delete = %+v, %v; want none", live, err)
	}
	if c := countFor(t, pool, `SELECT COUNT(*) FROM character_spells WHERE character_id = $1`, id); c != 2 {
		t.Errorf("spell children = %d, want preserved 2", c)
	}
	if c := countFor(t, pool, `SELECT COUNT(*) FROM character_skills WHERE character_id = $1`, id); c != 1 {
		t.Errorf("skill children = %d, want preserved 1", c)
	}
	// Slot and name are reusable now.
	if _, err := st.CreateCharacter(ctx, validNewCharacter(acct)); err != nil {
		t.Errorf("recreate after delete: %v", err)
	}
	// Deleting the same revision again is stale.
	if _, err := st.SoftDeleteCharacter(ctx, id, 0); !errors.Is(err, ErrStaleRevision) {
		t.Errorf("stale delete err = %v, want ErrStaleRevision", err)
	}
}
