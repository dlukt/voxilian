package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"

	"github.com/dlukt/voxilian/migrations"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// MigrationStatus is one embedded migration's deployment state.
type MigrationStatus struct {
	Version int64
	Path    string
	Applied bool
}

// MigrationReport summarizes embedded-vs-applied migration state.
// Target comes from the embedded provider, never a hardcoded constant.
type MigrationReport struct {
	Current int64
	Target  int64
	Entries []MigrationStatus
}

// Pending counts unapplied embedded migrations.
func (r MigrationReport) Pending() int {
	n := 0
	for _, e := range r.Entries {
		if !e.Applied {
			n++
		}
	}
	return n
}

// Migrator runs embedded goose migrations over PostgreSQL. Construct via
// OpenMigrator; it owns its *sql.DB until Close.
type Migrator struct {
	db       *sql.DB
	provider *goose.Provider
}

// OpenMigrator connects (verifying with ctx) and builds a goose v3.28
// Provider over the embedded migration FS. No global goose state is
// touched. The DSN never appears in returned errors.
func OpenMigrator(ctx context.Context, dsn string) (*Migrator, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("migrate: open postgres: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: connect postgres: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: create provider: %w", err)
	}
	return &Migrator{db: db, provider: provider}, nil
}

// Close releases the database handle.
func (m *Migrator) Close() error {
	if err := m.db.Close(); err != nil {
		return fmt.Errorf("migrate: close: %w", err)
	}
	return nil
}

// Up applies all pending embedded migrations. Already-current is a
// strict no-op success (important for retried init jobs).
func (m *Migrator) Up(ctx context.Context) error {
	if _, err := m.provider.Up(ctx); err != nil {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

// Down rolls back exactly one currently applied migration.
func (m *Migrator) Down(ctx context.Context) error {
	if _, err := m.provider.Down(ctx); err != nil {
		return fmt.Errorf("migrate down: %w", err)
	}
	return nil
}

// Status reports current/target plus per-migration applied state.
func (m *Migrator) Status(ctx context.Context) (MigrationReport, error) {
	current, target, err := m.provider.GetVersions(ctx)
	if err != nil {
		return MigrationReport{}, fmt.Errorf("migrate status: versions: %w", err)
	}
	stats, err := m.provider.Status(ctx)
	if err != nil {
		return MigrationReport{}, fmt.Errorf("migrate status: %w", err)
	}
	rep := MigrationReport{Current: current, Target: target}
	for _, s := range stats {
		rep.Entries = append(rep.Entries, MigrationStatus{
			Version: s.Source.Version,
			Path:    filepath.Base(s.Source.Path),
			Applied: s.State == goose.StateApplied,
		})
	}
	return rep, nil
}
