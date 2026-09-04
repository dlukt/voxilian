package store

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/dlukt/voxilian/internal/simtest"
	"github.com/dlukt/voxilian/migrations"
)

// TestMigratorEmbeddedSources proves the binary embeds exactly migrations
// 1-5 with the expected filenames (no SQL contents asserted here).
func TestMigratorEmbeddedSources(t *testing.T) {
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatalf("embedded FS: %v", err)
	}
	want := map[string]bool{
		"0001_accounts_characters.sql":   false,
		"0002_prototype_catalogs.sql":    false,
		"0003_character_abilities.sql":   false,
		"0004_items_corpses_banks.sql":   false,
		"0005_audit_kills_sanctions.sql": false,
	}
	for _, e := range entries {
		if _, ok := want[e.Name()]; !ok {
			t.Fatalf("unexpected embedded file %s", e.Name())
		}
		want[e.Name()] = true
	}
	for name, seen := range want {
		if !seen {
			t.Fatalf("embedded migration %s missing", name)
		}
	}
}

func openTestMigrator(t *testing.T) *Migrator {
	t.Helper()
	pg := simtest.StartPostgres18(t)
	m, err := OpenMigrator(context.Background(), pg.DSN)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func tablePresent(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var reg *string
	if err := db.QueryRow(`SELECT to_regclass($1)::text`, "public."+name).Scan(&reg); err != nil {
		t.Fatalf("to_regclass: %v", err)
	}
	return reg != nil
}

func TestMigratorFreshStatus(t *testing.T) {
	m := openTestMigrator(t)
	rep, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if rep.Current != 0 || rep.Target != 5 || rep.Pending() != 5 || len(rep.Entries) != 5 {
		t.Fatalf("fresh report = %+v", rep)
	}
}

func TestMigratorUpDownUp(t *testing.T) {
	m := openTestMigrator(t)
	ctx := context.Background()

	if err := m.Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}
	rep, err := m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Current != 5 || rep.Target != 5 || rep.Pending() != 0 {
		t.Fatalf("after up = %+v", rep)
	}
	for _, tbl := range []string{"accounts", "characters", "spell_protos", "item_instances", "banks", "ledger", "kills", "bans", "mutes"} {
		if !tablePresent(t, m.db, tbl) {
			t.Fatalf("table %s missing after up", tbl)
		}
	}
	var ext bool
	if err := m.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname='citext')`).Scan(&ext); err != nil || !ext {
		t.Fatalf("citext: %v", err)
	}

	// Idempotent second up.
	if err := m.Up(ctx); err != nil {
		t.Fatalf("second up: %v", err)
	}
	rep, _ = m.Status(ctx)
	if rep.Current != 5 || rep.Pending() != 0 {
		t.Fatalf("after second up = %+v", rep)
	}

	// Down rolls back exactly one (5), then up restores it.
	if err := m.Down(ctx); err != nil {
		t.Fatalf("down: %v", err)
	}
	rep, _ = m.Status(ctx)
	if rep.Current != 4 || rep.Pending() != 1 {
		t.Fatalf("after down = %+v", rep)
	}
	if tablePresent(t, m.db, "ledger") {
		t.Fatal("ledger survives down")
	}
	if !tablePresent(t, m.db, "banks") {
		t.Fatal("banks lost by down")
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("re-up: %v", err)
	}
	rep, _ = m.Status(ctx)
	if rep.Current != 5 || rep.Pending() != 0 {
		t.Fatalf("after re-up = %+v", rep)
	}
}

// TestMigratorNoWorkingDirectory proves embedded execution outside any
// repo/module directory (the runtime image ships only the binary).
func TestMigratorNoWorkingDirectory(t *testing.T) {
	if testing.Short() {
		t.Skip("needs container")
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	empty := t.TempDir()
	if err := os.Chdir(empty); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(cwd) }()

	m := openTestMigrator(t)
	ctx := context.Background()
	if err := m.Up(ctx); err != nil {
		t.Fatalf("up outside repo: %v", err)
	}
	rep, err := m.Status(ctx)
	if err != nil || rep.Current != 5 || len(rep.Entries) != 5 {
		t.Fatalf("report = %+v, err = %v", rep, err)
	}
}
