package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/dlukt/voxilian/internal/simtest"
)

// rootUpdateRe matches a statement-level UPDATE of a CAS aggregate root
// that T7 does not own yet: item_instances (T7b) and banks (T7c).
// characters updates moved to the file-ownership rule below (T7a).
// It deliberately does NOT match the `DO UPDATE SET` clause inside the
// sanctioned UpsertItemLocation (M1-T7b building block).
var rootUpdateRe = regexp.MustCompile(`(?mi)^\s*UPDATE\s+(item_instances|banks)\b`)

// characterCASOwner restricts UPDATE characters to queries/character_cas.sql.
var characterCASFile = "character_cas.sql"

var characterRootUpdateRe = regexp.MustCompile(`(?mi)^\s*UPDATE\s+characters\b`)

// characterCASQueryRe allows only the two explicit T7a CAS statements.
var characterCASQueryRe = regexp.MustCompile(`-- name: (CASUpdateCharacterSnapshot|CASSoftDeleteCharacter)\b`)

// characterDeleteRe forbids physical character deletion everywhere:
// soft-delete only, always.
var characterDeleteRe = regexp.MustCompile(`(?mi)^\s*DELETE\s+FROM\s+characters\b`)

// auditMutateRe matches statement-level UPDATE/DELETE of append-only
// audit tables. Sanction revoke-by-delete (bans/mutes) and sanction
// ON CONFLICT replacement are normative and NOT matched here.
var auditMutateRe = regexp.MustCompile(`(?mi)^\s*(UPDATE\s+ledger|DELETE\s+FROM\s+ledger|UPDATE\s+kills|DELETE\s+FROM\s+kills)\b`)

// catalogAccessRe matches production SQL touching catalog tables. Only
// queries/catalogs.sql may do so (M1-T6d owns all catalog access).
// Comments are stripped first so prose like "never UPDATE ledger" or
// "catalog tables: ..." cannot trip the guard.
var catalogTables = []string{"spell_protos", "skill_protos", "item_protos", "mob_protos", "shop_listings"}

var catalogAccessRe = regexp.MustCompile(`(?is)\b(SELECT\b[^;]*?\bFROM\s+(?:spell_protos|skill_protos|item_protos|mob_protos|shop_listings)\b|INSERT\s+INTO\s+(?:spell_protos|skill_protos|item_protos|mob_protos|shop_listings)\b|UPDATE\s+(?:spell_protos|skill_protos|item_protos|mob_protos|shop_listings)\b|DELETE\s+FROM\s+(?:spell_protos|skill_protos|item_protos|mob_protos|shop_listings)\b)`)

var sqlLineCommentRe = regexp.MustCompile(`(?m)--[^\n]*$`)

func stripSQLComments(s string) string {
	return sqlLineCommentRe.ReplaceAllString(s, "")
}

// TestNoMutableRootQueries guards the D7 escape hatch: before M1-T7a/b/c,
// no production query source may contain a root UPDATE on characters,
// item_instances, or banks. It also guards ledger/kills append-only and
// catalog-table isolation (only queries/catalogs.sql may touch catalogs).
// Raw SQL inside *_test.go files is excluded.
func TestNoMutableRootQueries(t *testing.T) {
	queries := filepath.Join(simtest.RepoRoot(t), "backend", "voxilian", "queries")
	entries, err := os.ReadDir(queries)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(queries, name))
		if err != nil {
			t.Fatal(err)
		}
		if m := rootUpdateRe.FindString(string(raw)); m != "" {
			t.Errorf("%s contains forbidden root update %q (CAS owns it in M1-T7)", name, m)
		}
		if m := characterRootUpdateRe.FindString(string(raw)); m != "" && name != characterCASFile {
			t.Errorf("%s contains UPDATE characters outside %s", name, characterCASFile)
		}
		if m := characterDeleteRe.FindString(string(raw)); m != "" {
			t.Errorf("%s contains forbidden %q (soft-delete only)", name, m)
		}
		if name == characterCASFile {
			for _, want := range []string{"CASUpdateCharacterSnapshot", "CASSoftDeleteCharacter"} {
				if !strings.Contains(string(raw), "-- name: "+want) {
					t.Errorf("%s missing required query %s", name, want)
				}
			}
		}
		if m := auditMutateRe.FindString(string(raw)); m != "" {
			t.Errorf("%s contains forbidden audit mutation %q (ledger/kills are append-only)", name, m)
		}
		if name == "catalogs.sql" {
			continue
		}
		if m := catalogAccessRe.FindString(stripSQLComments(string(raw))); m != "" {
			t.Errorf("%s touches catalog tables %q (only catalogs.sql may; M1-T6d)", name, m)
		}
	}
}
