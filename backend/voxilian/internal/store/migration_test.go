package store_test

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dlukt/voxilian/internal/simtest"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

func migrationsDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(simtest.RepoRoot(t), "backend", "voxilian", "migrations")
}

func openMigrated(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("dialect: %v", err)
	}
	if err := goose.UpTo(db, migrationsDir(t), 1); err != nil {
		t.Fatalf("goose up to 1: %v", err)
	}
	return db
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var reg *string
	if err := db.QueryRow(`SELECT to_regclass($1)::text`, "public."+name).Scan(&reg); err != nil {
		t.Fatalf("to_regclass: %v", err)
	}
	return reg != nil
}

// TestMigration0001 proves the M1-T1 schema against real PostgreSQL 18.
// Fixture INSERTs are explicit; no production constructors or CRUD APIs.
func TestMigration0001(t *testing.T) {
	pg := simtest.StartPostgres18(t)
	db := openMigrated(t, pg.DSN)

	// 1-4: migration applied; extension and tables exist.
	var ver int64
	if err := db.QueryRow(`SELECT version_id FROM goose_db_version ORDER BY version_id DESC LIMIT 1`).Scan(&ver); err != nil {
		t.Fatalf("goose version: %v", err)
	}
	if ver != 1 {
		t.Fatalf("goose version = %d, want exactly migration 0001", ver)
	}
	var ext bool
	if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'citext')`).Scan(&ext); err != nil || !ext {
		t.Fatalf("citext extension missing: %v", err)
	}
	for _, tbl := range []string{"accounts", "characters"} {
		if !tableExists(t, db, tbl) {
			t.Fatalf("table %s missing", tbl)
		}
	}

	// 5: account creation works.
	var acct int64
	if err := db.QueryRow(
		`INSERT INTO accounts (keycloak_sub, email) VALUES ('sub-1', 'a@example.com') RETURNING id`,
	).Scan(&acct); err != nil {
		t.Fatalf("insert account: %v", err)
	}

	// 6: duplicate keycloak_sub rejected.
	if _, err := db.Exec(`INSERT INTO accounts (keycloak_sub) VALUES ('sub-1')`); err == nil ||
		!strings.Contains(err.Error(), "duplicate key value") {
		t.Fatalf("duplicate keycloak_sub err = %v, want unique violation", err)
	}

	charSQL := `INSERT INTO characters
		(account_id, slot, name, gender, face, might, intellect, stamina, agility, mysticism, aim,
		 karma, hometown, pos_x, pos_y, pos_z, vitals, advancement, flags, updated_at)
		VALUES ($1,$2,$3,0,'{}',10,10,10,10,10,10,0,'tos',0,0,0,'{}','{}',0,now()) RETURNING id`

	// 7: FK to nonexistent account rejected.
	if _, err := db.Exec(strings.Replace(charSQL, "($1,$2,$3,", "(999999,$2,$3,", 1), acct, 0, "Ghost"); err == nil {
		t.Fatal("character with bad account_id accepted")
	}

	// 8: slot outside 0/1 rejected.
	if _, err := db.Exec(charSQL, acct, 2, "OutOfSlot"); err == nil ||
		!strings.Contains(err.Error(), "characters_slot_check") {
		t.Fatalf("slot=2 err = %v, want CHECK violation", err)
	}

	// 9: second live character on same (account, slot) rejected.
	var c1 int64
	if err := db.QueryRow(charSQL, acct, 0, "Alice").Scan(&c1); err != nil {
		t.Fatalf("insert Alice: %v", err)
	}
	if _, err := db.Exec(charSQL, acct, 0, "Bob"); err == nil ||
		!strings.Contains(err.Error(), "chars_acct_slot_uidx") {
		t.Fatalf("duplicate live slot err = %v, want chars_acct_slot_uidx", err)
	}

	// 10: soft-delete frees the slot.
	mustExec(t, db, `UPDATE characters SET deleted_at = now() WHERE id = $1`, c1)
	var c2 int64
	if err := db.QueryRow(charSQL, acct, 0, "Bob").Scan(&c2); err != nil {
		t.Fatalf("slot reuse after soft-delete: %v", err)
	}

	// 11: live names case-insensitively unique (Alice is deleted, so use Bob).
	if _, err := db.Exec(charSQL, acct, 1, "bob"); err == nil ||
		!strings.Contains(err.Error(), "chars_name_uidx") {
		t.Fatalf("bob-vs-Bob err = %v, want chars_name_uidx", err)
	}

	// 12: name reusable after soft-delete.
	mustExec(t, db, `UPDATE characters SET deleted_at = now() WHERE id = $1`, c2)
	var c3 int64
	if err := db.QueryRow(charSQL, acct, 1, "BOB").Scan(&c3); err != nil {
		t.Fatalf("name reuse after soft-delete: %v", err)
	}

	// 13: revision defaults to 0 when omitted.
	var rev int64
	if err := db.QueryRow(`SELECT revision FROM characters WHERE id = $1`, c3).Scan(&rev); err != nil || rev != 0 {
		t.Fatalf("revision = %d, %v; want 0", rev, err)
	}

	// 14: positions round-trip as BIGINT (beyond int32 range).
	const big = 5_000_000_000
	mustExec(t, db, `UPDATE characters SET pos_x = $2, pos_y = $3, pos_z = $4 WHERE id = $1`, c3, big, -big, 0)
	var x, y, z int64
	if err := db.QueryRow(`SELECT pos_x, pos_y, pos_z FROM characters WHERE id = $1`, c3).Scan(&x, &y, &z); err != nil {
		t.Fatalf("select pos: %v", err)
	}
	if x != big || y != -big || z != 0 {
		t.Fatalf("pos = (%d,%d,%d), want (%d,%d,0)", x, y, z, big, -big)
	}

	// 15: down to zero removes M1-T1 tables.
	if err := goose.DownTo(db, migrationsDir(t), 0); err != nil {
		t.Fatalf("goose down: %v", err)
	}
	for _, tbl := range []string{"accounts", "characters"} {
		if tableExists(t, db, tbl) {
			t.Fatalf("table %s survives down-to-zero", tbl)
		}
	}
}
