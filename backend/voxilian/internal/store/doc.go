// Package store is the ONLY package that may import pgx and sqlc
// generated code. It owns PostgreSQL/sqlc CRUD/query primitives
// internally, and exposes safe domain-level Store operations plus
// aggregate-root compare-and-swap writes (spec §8.1/D7) against
// PostgreSQL 18. Migrations live in migrations/ (goose).
package store
