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
	}
}
