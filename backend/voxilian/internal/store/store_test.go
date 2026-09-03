package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/dlukt/voxilian/internal/simtest"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// newTestStore builds a PGStore on a fresh registry per test, so metric
// assertions stay isolated and duplicate registration never bites.
func newTestStore(t *testing.T, pool *pgxpool.Pool) *PGStore {
	t.Helper()
	st, err := New(pool, prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return st
}

func TestStoreSatisfiesInterface(t *testing.T) {
	pool, _ := openQueries(t)
	var s Store = newTestStore(t, pool)
	if s == nil {
		t.Fatal("nil store")
	}
}

var forbiddenImportRe = regexp.MustCompile(`(?m)^\s*(?:[\w.]+ +)?"(github\.com/jackc/pgx/v5[^"]*|github\.com/dlukt/voxilian/internal/store/gen)"`)

// TestPGXGenBoundary proves only internal/store imports pgx/sqlc-generated
// production code. Test files and internal/store itself are excluded;
// importing internal/store (the interface) is always allowed.
func TestPGXGenBoundary(t *testing.T) {
	root := filepath.Join(simtest.RepoRoot(t), "backend", "voxilian", "internal")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "store" {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			name := f.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(root, e.Name(), name))
			if err != nil {
				t.Fatal(err)
			}
			if m := forbiddenImportRe.FindString(string(raw)); m != "" {
				t.Errorf("%s/%s imports forbidden %q", e.Name(), name, m)
			}
		}
	}
}
