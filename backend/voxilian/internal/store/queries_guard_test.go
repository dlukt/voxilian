package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/dlukt/voxilian/internal/simtest"
)

// rootUpdateRe is now empty-by-construction: every aggregate-root UPDATE
// has an explicit owner file (characters→character_cas.sql T7a,
// item_instances→item_cas.sql T7b, banks→bank_cas.sql T7c). It remains as
// a tripwire matching nothing; per-file rules below do the real work.
// It deliberately does NOT match the `DO UPDATE SET` clause inside
// sanctioned upserts (UpsertItemLocation, UpsertBan/Mute).
var rootUpdateRe = regexp.MustCompile(`(?mi)^\s*UPDATE\s+(ZZZ_NO_SUCH_TABLE)\b`)

// bankCASFile owns the only production UPDATE banks (T7c).
var bankCASFile = "bank_cas.sql"

var bankRootUpdateRe = regexp.MustCompile(`(?mi)^\s*UPDATE\s+banks\b`)

// bankDeleteRe forbids bank deletion: deletion semantics unspecified.
var bankDeleteRe = regexp.MustCompile(`(?mi)^\s*DELETE\s+FROM\s+banks\b`)

// itemCASFile owns the only production UPDATE item_instances (T7b).
var itemCASFile = "item_cas.sql"

var itemRootUpdateRe = regexp.MustCompile(`(?mi)^\s*UPDATE\s+item_instances\b`)

// itemDeleteRe forbids item destruction everywhere: no DestroyItem API,
// no tombstones in M1.
var itemDeleteRe = regexp.MustCompile(`(?mi)^\s*DELETE\s+FROM\s+item_instances\b`)

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

// deathPersistenceFile owns the only production writes to the T5b1a
// death-persistence tables (spec §9.5.8a). pending_deaths and
// item_pk_protections are aggregate children composed transactionally
// by T5b1b/T5b2a/T5b2b beside character/item-root CAS — never standalone.
// T5b2a owns the ONE permitted production UPDATE of pending_deaths
// (ApplyPendingDeathPortal: lowers-only cost + once flag, spec §9.5.10a).
// No UPDATE of item_pk_protections exists anywhere, and no second
// pending-death UPDATE may be added (the sanctioned UPSERT's
// `DO UPDATE SET` line starts with ON CONFLICT and is not matched,
// same as UpsertItemLocation above).
var deathPersistenceFile = "death_persistence.sql"

var deathWriteRe = regexp.MustCompile(`(?mi)^\s*(INSERT\s+INTO\s+(pending_deaths|item_pk_protections)\b|DELETE\s+FROM\s+(pending_deaths|item_pk_protections)\b)`)

var deathUpdateRe = regexp.MustCompile(`(?mi)^\s*UPDATE\s+(pending_deaths|item_pk_protections)\b`)

// deathPendingUpdateRe matches the one T5b2a-sanctioned UPDATE shape:
// UPDATE pending_deaths (statement level). deathPKUpdateRe matches any
// statement-level UPDATE of item_pk_protections, which is forbidden
// everywhere with no exception.
var deathPendingUpdateRe = regexp.MustCompile(`(?mi)^\s*UPDATE\s+pending_deaths\b`)

var deathPKUpdateRe = regexp.MustCompile(`(?mi)^\s*UPDATE\s+item_pk_protections\b`)

// portalUpdateQueryRe names the single permitted pending-death UPDATE
// query. The guard requires exactly one UPDATE pending_deaths
// statement in death_persistence.sql and requires it to be this query.
var portalUpdateQueryRe = regexp.MustCompile(`-- name: ApplyPendingDeathPortal\b`)

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
		if m := itemRootUpdateRe.FindString(string(raw)); m != "" && name != itemCASFile {
			t.Errorf("%s contains UPDATE item_instances outside %s", name, itemCASFile)
		}
		if m := itemDeleteRe.FindString(string(raw)); m != "" {
			t.Errorf("%s contains forbidden %q (no item destruction in M1)", name, m)
		}
		if name == itemCASFile {
			for _, want := range []string{"CASUpdateItemSnapshot", "LockItemContainmentGraph", "WouldCreateItemContainmentCycle"} {
				if !strings.Contains(string(raw), "-- name: "+want) {
					t.Errorf("%s missing required query %s", name, want)
				}
			}
		}
		if name == characterCASFile {
			for _, want := range []string{"CASUpdateCharacterSnapshot", "CASSoftDeleteCharacter"} {
				if !strings.Contains(string(raw), "-- name: "+want) {
					t.Errorf("%s missing required query %s", name, want)
				}
			}
		}
		if m := bankRootUpdateRe.FindString(string(raw)); m != "" && name != bankCASFile {
			t.Errorf("%s contains UPDATE banks outside %s", name, bankCASFile)
		}
		if m := bankDeleteRe.FindString(string(raw)); m != "" {
			t.Errorf("%s contains forbidden %q (bank deletion unspecified)", name, m)
		}
		if name == bankCASFile {
			if !strings.Contains(string(raw), "-- name: CASUpdateBankBalance") {
				t.Errorf("%s missing required query CASUpdateBankBalance", name)
			}
		}
		if m := auditMutateRe.FindString(string(raw)); m != "" {
			t.Errorf("%s contains forbidden audit mutation %q (ledger/kills are append-only)", name, m)
		}
		if m := deathWriteRe.FindString(string(raw)); m != "" && name != deathPersistenceFile {
			t.Errorf("%s contains death-persistence write outside %s: %q", name, deathPersistenceFile, m)
		}
		if m := deathUpdateRe.FindString(string(raw)); m != "" && name != deathPersistenceFile {
			t.Errorf("%s contains death-persistence UPDATE outside %s: %q", name, deathPersistenceFile, m)
		}
		if name == deathPersistenceFile {
			for _, want := range []string{"InsertPendingDeath", "GetPendingDeathByCharacter", "DeletePendingDeathByCharacter", "ApplyPendingDeathPortal", "UpsertItemPKProtection", "GetItemPKProtection", "DeleteItemPKProtection"} {
				if !strings.Contains(string(raw), "-- name: "+want) {
					t.Errorf("%s missing required query %s", name, want)
				}
			}
			if !portalUpdateQueryRe.MatchString(string(raw)) {
				t.Errorf("%s missing required query ApplyPendingDeathPortal", name)
			}
			if n := len(deathPendingUpdateRe.FindAllString(string(raw), -1)); n != 1 {
				t.Errorf("%s contains %d UPDATE pending_deaths statements, want exactly 1 (ApplyPendingDeathPortal)", name, n)
			}
			if m := deathPKUpdateRe.FindString(string(raw)); m != "" {
				t.Errorf("%s contains forbidden %q (no UPDATE of item_pk_protections)", name, m)
			}
		}
		if name == "catalogs.sql" {
			continue
		}
		if m := catalogAccessRe.FindString(stripSQLComments(string(raw))); m != "" {
			t.Errorf("%s touches catalog tables %q (only catalogs.sql may; M1-T6d)", name, m)
		}
	}
}
