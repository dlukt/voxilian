package simtest

import (
	"context"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// Postgres wraps a disposable PostgreSQL 18 testcontainer. Callers use
// DSN and register nothing; cleanup runs via t.Cleanup so containers
// cannot leak on test failure.
type Postgres struct {
	ctr *postgres.PostgresContainer
	DSN string
}

// StartPostgres18 launches postgres:18-alpine with an isolated database,
// waits for readiness via the module's waiter, and returns connection
// details. It knows nothing about Voxilian migrations or store code;
// M1 suites reuse it as-is.
func StartPostgres18(t *testing.T) *Postgres {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	ctr, err := postgres.Run(ctx,
		"postgres:18-alpine",
		postgres.WithDatabase("voxtest"),
		postgres.WithUsername("vox"),
		postgres.WithPassword("voxtest"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("simtest: start postgres:18-alpine: %v", err)
	}
	t.Cleanup(func() {
		if err := ctr.Terminate(context.Background()); err != nil {
			t.Errorf("simtest: terminate postgres container: %v", err)
		}
	})
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("simtest: postgres connection string: %v", err)
	}
	return &Postgres{ctr: ctr, DSN: dsn}
}
