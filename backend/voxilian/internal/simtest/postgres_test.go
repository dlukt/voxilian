package simtest

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestPostgres18Reachable starts a real postgres:18-alpine container,
// connects, and asserts major version 18. No schema or migrations.
func TestPostgres18Reachable(t *testing.T) {
	pg := StartPostgres18(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, pg.DSN)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	var version string
	if err := conn.QueryRow(ctx, "show server_version").Scan(&version); err != nil {
		t.Fatalf("server_version: %v", err)
	}
	t.Logf("postgres server_version = %s", version)
	if !strings.HasPrefix(version, "18") {
		t.Fatalf("major version = %q, want 18.x", version)
	}
	var one int
	if err := conn.QueryRow(ctx, "select 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("select 1 = %d, %v", one, err)
	}
}
