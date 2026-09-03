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
	return openMigratedTo(t, dsn, 1)
}

// openDB opens the disposable database without migrating; callers own
// version control explicitly.
func openDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// openMigratedTo migrates a fresh disposable database to exactly version.
// Per-migration tests pin their own version so later migrations never
// break earlier tests.
func openMigratedTo(t *testing.T, dsn string, version int64) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("dialect: %v", err)
	}
	if err := goose.UpTo(db, migrationsDir(t), version); err != nil {
		t.Fatalf("goose up to %d: %v", version, err)
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

// uitoa formats non-negative and negative ints without importing more.
func uitoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// TestMigration0002 proves the M1-T2 catalog schema against real
// PostgreSQL 18. Explicit SQL fixtures only; no seed/store APIs
// (those belong to M1-T6d/M9).
func TestMigration0002(t *testing.T) {
	pg := simtest.StartPostgres18(t)
	db := openMigratedTo(t, pg.DSN, 2)

	// 1: goose reaches exactly version 2.
	var ver int64
	if err := db.QueryRow(`SELECT version_id FROM goose_db_version ORDER BY version_id DESC LIMIT 1`).Scan(&ver); err != nil {
		t.Fatalf("goose version: %v", err)
	}
	if ver != 2 {
		t.Fatalf("goose version = %d, want 2", ver)
	}

	// 2: all five new tables exist.
	tables := []string{"spell_protos", "skill_protos", "item_protos", "mob_protos", "shop_listings"}
	for _, tbl := range tables {
		if !tableExists(t, db, tbl) {
			t.Fatalf("table %s missing", tbl)
		}
	}

	// 3: id columns are integer, not smallint (u16 wire range needs it).
	for _, tbl := range []string{"spell_protos", "skill_protos", "item_protos", "mob_protos"} {
		var typ string
		q := `SELECT data_type FROM information_schema.columns WHERE table_name = $1 AND column_name = 'id'`
		if err := db.QueryRow(q, tbl).Scan(&typ); err != nil || typ != "integer" {
			t.Fatalf("%s.id type = %q, %v; want integer", tbl, typ, err)
		}
	}
	var listTyp string
	if err := db.QueryRow(`SELECT data_type FROM information_schema.columns WHERE table_name = 'shop_listings' AND column_name = 'listing'`).Scan(&listTyp); err != nil || listTyp != "integer" {
		t.Fatalf("shop_listings.listing type = %q, %v; want integer", listTyp, err)
	}

	protos := []string{"spell_protos", "skill_protos", "item_protos", "mob_protos"}

	// 4-5: boundary IDs 1 and 65535 succeed in every proto table.
	for _, tbl := range protos {
		for _, id := range []int{1, 65535} {
			if _, err := db.Exec(withID(tbl, id)); err != nil {
				t.Fatalf("insert %s id=%d: %v", tbl, id, err)
			}
		}
	}

	// 6-8: 0 and 65536 rejected by each table's range CHECK.
	// (IDs 1/65535 above are taken, so negatives use fresh keys.)
	for _, tbl := range protos {
		for _, id := range []int{0, 65536} {
			_, err := db.Exec(withID(tbl, id))
			if err == nil || !strings.Contains(err.Error(), tbl+"_id_check") {
				t.Fatalf("%s id=%d err = %v, want %s_id_check", tbl, id, err, tbl)
			}
		}
	}

	// 9: duplicate mob key rejected.
	if _, err := db.Exec(`INSERT INTO mob_protos (id,key,level,difficulty,karma,atk,resists,spells,loot_tid,version) VALUES (100,'dup-key',1,1,0,'{}','{}','{}',NULL,1)`); err != nil {
		t.Fatalf("insert dup-key base: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO mob_protos (id,key,level,difficulty,karma,atk,resists,spells,loot_tid,version) VALUES (101,'dup-key',1,1,0,'{}','{}','{}',NULL,1)`); err == nil ||
		!strings.Contains(err.Error(), "mob_protos_key_key") {
		t.Fatalf("duplicate mob key err = %v, want mob_protos_key_key", err)
	}

	// 10: valid mob + item + shop listing insert.
	mustExec(t, db, `INSERT INTO mob_protos (id,key,level,difficulty,karma,atk,resists,spells,loot_tid,version) VALUES (200,'vendor-a',45,6,-40,'{}','{}','{}','TID_ORC',1)`)
	mustExec(t, db, `INSERT INTO item_protos (id,kind,slot,base,version) VALUES (300,2,'mainhand','{}',1)`)
	mustExec(t, db, `INSERT INTO shop_listings (vendor_id,listing,item_proto,price,qty) VALUES (200,1,300,150,10)`)

	// 11-12: unknown vendor/item FKs rejected.
	if _, err := db.Exec(`INSERT INTO shop_listings (vendor_id,listing,item_proto,price,qty) VALUES (9999,1,300,1,1)`); err == nil ||
		!strings.Contains(err.Error(), "violates foreign key constraint") {
		t.Fatalf("bad vendor err = %v, want FK violation", err)
	}
	if _, err := db.Exec(`INSERT INTO shop_listings (vendor_id,listing,item_proto,price,qty) VALUES (200,2,9999,1,1)`); err == nil ||
		!strings.Contains(err.Error(), "violates foreign key constraint") {
		t.Fatalf("bad item err = %v, want FK violation", err)
	}

	// 13: listing 0 / 65536 rejected.
	for _, l := range []int{0, 65536} {
		q := `INSERT INTO shop_listings (vendor_id,listing,item_proto,price,qty) VALUES (200,` + uitoa(l) + `,300,1,1)`
		if _, err := db.Exec(q); err == nil || !strings.Contains(err.Error(), "shop_listings_listing_check") {
			t.Fatalf("listing=%d err = %v, want shop_listings_listing_check", l, err)
		}
	}

	// 14: duplicate (vendor, listing) rejected.
	if _, err := db.Exec(`INSERT INTO shop_listings (vendor_id,listing,item_proto,price,qty) VALUES (200,1,300,1,1)`); err == nil ||
		!strings.Contains(err.Error(), "shop_listings_pkey") {
		t.Fatalf("duplicate listing err = %v, want shop_listings_pkey", err)
	}

	// 15: same listing number on a different vendor is fine.
	mustExec(t, db, `INSERT INTO mob_protos (id,key,level,difficulty,karma,atk,resists,spells,loot_tid,version) VALUES (201,'vendor-b',1,1,0,'{}','{}','{}',NULL,1)`)
	mustExec(t, db, `INSERT INTO shop_listings (vendor_id,listing,item_proto,price,qty) VALUES (201,1,300,99,5)`)

	// 16: down 2 → 1 removes all five M1-T2 tables.
	if err := goose.DownTo(db, migrationsDir(t), 1); err != nil {
		t.Fatalf("goose down to 1: %v", err)
	}
	for _, tbl := range tables {
		if tableExists(t, db, tbl) {
			t.Fatalf("table %s survives down-to-1", tbl)
		}
	}

	// 17: M1-T1 tables still exist; version is 1.
	for _, tbl := range []string{"accounts", "characters"} {
		if !tableExists(t, db, tbl) {
			t.Fatalf("table %s lost by down-to-1", tbl)
		}
	}
	if err := db.QueryRow(`SELECT version_id FROM goose_db_version ORDER BY version_id DESC LIMIT 1`).Scan(&ver); err != nil || ver != 1 {
		t.Fatalf("version after down = %d, %v; want 1", ver, err)
	}
}

// withID rebuilds a proto insert at an arbitrary id for negative tests.
func withID(table string, id int) string {
	switch table {
	case "mob_protos":
		return "INSERT INTO mob_protos (id,key,level,difficulty,karma,atk,resists,spells,loot_tid,version) VALUES (" + uitoa(id) + ",'neg-" + uitoa(id) + "',1,1,0,'{}','{}','{}',NULL,1)"
	case "spell_protos":
		return "INSERT INTO spell_protos (id,school,level,mana,exertion,cast_ms,min_hp,outlaw,harmful,reagents,params,version) VALUES (" + uitoa(id) + ",1,1,1,1,0,1,false,false,'{}','{}',1)"
	case "skill_protos":
		return "INSERT INTO skill_protos (id,division,level,exertion,params,version) VALUES (" + uitoa(id) + ",1,1,1,'{}',1)"
	default: // item_protos
		return "INSERT INTO item_protos (id,kind,slot,base,version) VALUES (" + uitoa(id) + ",0,NULL,'{}',1)"
	}
}

// insertAbilityCharacter creates an account + character with all stats at
// `stat`, returning both ids. Explicit fixture, no production API.
func insertAbilityCharacter(t *testing.T, db *sql.DB, sub, name string, slot, stat int) (int64, int64) {
	t.Helper()
	var acct int64
	err := db.QueryRow(`INSERT INTO accounts (keycloak_sub) VALUES ($1) RETURNING id`, sub).Scan(&acct)
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}
	var char int64
	err = db.QueryRow(`INSERT INTO characters
		(account_id, slot, name, gender, face, might, intellect, stamina, agility, mysticism, aim,
		 karma, hometown, pos_x, pos_y, pos_z, vitals, advancement, flags, updated_at)
		VALUES ($1,$2,$3,0,'{}',$4,$4,$4,$4,$4,$4,0,'tos',0,0,0,'{}','{}',0,now()) RETURNING id`,
		acct, slot, name, stat).Scan(&char)
	if err != nil {
		t.Fatalf("insert character: %v", err)
	}
	return acct, char
}

func constraintExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var found bool
	q := `SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = $1)`
	if err := db.QueryRow(q, name).Scan(&found); err != nil {
		t.Fatalf("pg_constraint: %v", err)
	}
	return found
}

// TestMigration0003 proves the M1-T3 ability tables and stat CHECKs
// against real PostgreSQL 18. Pinned to version 3.
func TestMigration0003(t *testing.T) {
	pg := simtest.StartPostgres18(t)
	db := openMigratedTo(t, pg.DSN, 3)

	// 1-3: version and tables.
	var ver int64
	if err := db.QueryRow(`SELECT version_id FROM goose_db_version ORDER BY version_id DESC LIMIT 1`).Scan(&ver); err != nil {
		t.Fatalf("goose version: %v", err)
	}
	if ver != 3 {
		t.Fatalf("goose version = %d, want 3", ver)
	}
	for _, tbl := range []string{"character_spells", "character_skills"} {
		if !tableExists(t, db, tbl) {
			t.Fatalf("table %s missing", tbl)
		}
	}

	_, char := insertAbilityCharacter(t, db, "sub-m3", "Mightyor", 0, 10)

	// Stat constraints: 1/50 accepted, 0/51 rejected with explicit names.
	stats := []struct{ col, check string }{
		{"might", "characters_might_check"},
		{"intellect", "characters_intellect_check"},
		{"stamina", "characters_stamina_check"},
		{"agility", "characters_agility_check"},
		{"mysticism", "characters_mysticism_check"},
		{"aim", "characters_aim_check"},
	}
	for _, s := range stats {
		for _, v := range []int{1, 50} {
			mustExec(t, db, `UPDATE characters SET `+s.col+` = $2::smallint WHERE id = $1`, char, v)
		}
		for _, v := range []int{0, 51} {
			_, err := db.Exec(`UPDATE characters SET `+s.col+` = $2::smallint WHERE id = $1`, char, v)
			if err == nil || !strings.Contains(err.Error(), s.check) {
				t.Fatalf("%s=%d err = %v, want %s", s.col, v, err, s.check)
			}
		}
		mustExec(t, db, `UPDATE characters SET `+s.col+` = 10 WHERE id = $1`, char)
	}

	// Catalog fixtures for ability FKs.
	mustExec(t, db, `INSERT INTO spell_protos (id,school,level,mana,exertion,cast_ms,min_hp,outlaw,harmful,reagents,params,version) VALUES (1,1,1,3,2,0,1,false,false,'{}','{}',1)`)
	mustExec(t, db, `INSERT INTO skill_protos (id,division,level,exertion,params,version) VALUES (1,1,1,2,'{}',1)`)

	// 4-5: valid ability rows.
	mustExec(t, db, `INSERT INTO character_spells (character_id,spell_id,ability,atrophy_flag) VALUES ($1,1,50,false)`, char)
	mustExec(t, db, `INSERT INTO character_skills (character_id,skill_id,ability,atrophy_flag) VALUES ($1,1,50,false)`, char)

	// 6-7: ability 1/99 accepted both tables.
	for _, tbl := range []struct {
		table, idcol string
	}{{"character_spells", "spell_id"}, {"character_skills", "skill_id"}} {
		for _, v := range []int{1, 99} {
			mustExec(t, db, `UPDATE `+tbl.table+` SET ability = $2::smallint WHERE character_id = $1 AND `+tbl.idcol+` = 1`, char, v)
		}
	}

	// 8-11: ability 0/100 rejected with explicit CHECK names.
	for _, tc := range []struct {
		table, idcol, check string
	}{
		{"character_spells", "spell_id", "character_spells_ability_check"},
		{"character_skills", "skill_id", "character_skills_ability_check"},
	} {
		for _, v := range []int{0, 100} {
			_, err := db.Exec(`UPDATE `+tc.table+` SET ability = $2::smallint WHERE character_id = $1 AND `+tc.idcol+` = 1`, char, v)
			if err == nil || !strings.Contains(err.Error(), tc.check) {
				t.Fatalf("%s ability=%d err = %v, want %s", tc.table, v, err, tc.check)
			}
		}
	}

	// 12-14: FKs rejected (bad character, bad spell, bad skill).
	if _, err := db.Exec(`INSERT INTO character_spells (character_id,spell_id,ability,atrophy_flag) VALUES (999999,1,10,false)`); err == nil {
		t.Fatal("bad character_id accepted (spells)")
	}
	if _, err := db.Exec(`INSERT INTO character_spells (character_id,spell_id,ability,atrophy_flag) VALUES ($1,9999,10,false)`, char); err == nil {
		t.Fatal("bad spell_id accepted")
	}
	if _, err := db.Exec(`INSERT INTO character_skills (character_id,skill_id,ability,atrophy_flag) VALUES (999999,1,10,false)`); err == nil {
		t.Fatal("bad character_id accepted (skills)")
	}
	if _, err := db.Exec(`INSERT INTO character_skills (character_id,skill_id,ability,atrophy_flag) VALUES ($1,9999,10,false)`, char); err == nil {
		t.Fatal("bad skill_id accepted")
	}

	// 15-16: composite PKs reject duplicates.
	if _, err := db.Exec(`INSERT INTO character_spells (character_id,spell_id,ability,atrophy_flag) VALUES ($1,1,10,false)`, char); err == nil ||
		!strings.Contains(err.Error(), "character_spells_pkey") {
		t.Fatalf("dup spell err = %v, want character_spells_pkey", err)
	}
	if _, err := db.Exec(`INSERT INTO character_skills (character_id,skill_id,ability,atrophy_flag) VALUES ($1,1,10,false)`, char); err == nil ||
		!strings.Contains(err.Error(), "character_skills_pkey") {
		t.Fatalf("dup skill err = %v, want character_skills_pkey", err)
	}

	// 17-18: same spell/skill on a second character is fine.
	_, char2 := insertAbilityCharacter(t, db, "sub-m3b", "Secondor", 0, 10)
	mustExec(t, db, `INSERT INTO character_spells (character_id,spell_id,ability,atrophy_flag) VALUES ($1,1,10,true)`, char2)
	mustExec(t, db, `INSERT INTO character_skills (character_id,skill_id,ability,atrophy_flag) VALUES ($1,1,10,true)`, char2)

	// atrophy_flag round-trips both values.
	var flag bool
	if err := db.QueryRow(`SELECT atrophy_flag FROM character_spells WHERE character_id = $1`, char2).Scan(&flag); err != nil || !flag {
		t.Fatalf("atrophy_flag = %v, %v; want true", flag, err)
	}
	if err := db.QueryRow(`SELECT atrophy_flag FROM character_skills WHERE character_id = $1`, char).Scan(&flag); err != nil || flag {
		t.Fatalf("atrophy_flag = %v, %v; want false", flag, err)
	}

	// Rollback 3 → 2: tables gone, version 2, catalogs and
	// M1-T1 tables intact, stat CHECKs gone (pg_constraint inspection).
	if err := goose.DownTo(db, migrationsDir(t), 2); err != nil {
		t.Fatalf("goose down to 2: %v", err)
	}
	for _, tbl := range []string{"character_spells", "character_skills"} {
		if tableExists(t, db, tbl) {
			t.Fatalf("table %s survives down-to-2", tbl)
		}
	}
	if err := db.QueryRow(`SELECT version_id FROM goose_db_version ORDER BY version_id DESC LIMIT 1`).Scan(&ver); err != nil || ver != 2 {
		t.Fatalf("version after down = %d, %v; want 2", ver, err)
	}
	for _, tbl := range []string{"spell_protos", "skill_protos", "item_protos", "mob_protos", "shop_listings", "accounts", "characters"} {
		if !tableExists(t, db, tbl) {
			t.Fatalf("table %s lost by down-to-2", tbl)
		}
	}
	for _, c := range []string{"characters_might_check", "characters_aim_check"} {
		if constraintExists(t, db, c) {
			t.Fatalf("constraint %s survives down-to-2", c)
		}
	}
}

// TestMigration0003RejectsInvalidExisting proves migration 2 → 3 fails
// loudly on pre-existing invalid stat data. Isolated container: a failed
// goose run must not contaminate TestMigration0003.
func TestMigration0003RejectsInvalidExisting(t *testing.T) {
	pg := simtest.StartPostgres18(t)
	db := openDB(t, pg.DSN)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(db, migrationsDir(t), 2); err != nil {
		t.Fatalf("up to 2: %v", err)
	}
	var acct int64
	if err := db.QueryRow(`INSERT INTO accounts (keycloak_sub) VALUES ('sub-bad') RETURNING id`).Scan(&acct); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO characters
		(account_id, slot, name, gender, face, might, intellect, stamina, agility, mysticism, aim,
		 karma, hometown, pos_x, pos_y, pos_z, vitals, advancement, flags, updated_at)
		VALUES ($1,0,'Badstat',0,'{}',0,10,10,10,10,10,0,'tos',0,0,0,'{}','{}',0,now())`, acct)
	if err := goose.Up(db, migrationsDir(t)); err == nil {
		t.Fatal("migration 2 → 3 succeeded with might=0; must fail loudly")
	} else if !strings.Contains(err.Error(), "characters_might_check") {
		t.Fatalf("migration error = %v, want characters_might_check", err)
	}
	var ver int64
	if err := db.QueryRow(`SELECT version_id FROM goose_db_version ORDER BY version_id DESC LIMIT 1`).Scan(&ver); err != nil || ver != 2 {
		t.Fatalf("version after failed migration = %d, %v; want 2", ver, err)
	}
}
