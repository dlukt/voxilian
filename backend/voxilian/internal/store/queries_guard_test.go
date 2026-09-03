package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/dlukt/voxilian/internal/simtest"
)

// rootUpdateRe matches a statement-level UPDATE of a CAS aggregate root.
// It deliberately does NOT match the `DO UPDATE SET` clause inside the
// sanctioned UpsertItemLocation (M1-T7b building block).
var rootUpdateRe = regexp.MustCompile(`(?mi)^\s*UPDATE\s+(characters|item_instances|banks)\b`)

// auditMutateRe matches statement-level UPDATE/DELETE of append-only
// audit tables. Sanction revoke-by-delete (bans/mutes) and sanction
// ON CONFLICT replacement are normative and NOT matched here.
var auditMutateRe = regexp.MustCompile(`(?mi)^\s*(UPDATE\s+ledger|DELETE\s+FROM\s+ledger|UPDATE\s+kills|DELETE\s+FROM\s+kills)\b`)

// TestNoMutableRootQueries guards the D7 escape hatch: before M1-T7a/b/c,
// no production query source may contain a root UPDATE on characters,
// item_instances, or banks. Raw SQL inside *_test.go files is excluded.
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
		if m := auditMutateRe.FindString(string(raw)); m != "" {
			t.Errorf("%s contains forbidden audit mutation %q (ledger/kills are append-only)", name, m)
		}
	}
}
