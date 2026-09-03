// Package store is the ONLY package that may import pgx and sqlc
// generated code. It exposes the Store interface (CRUD plus
// aggregate-root compare-and-swap writes per spec §8.1/D7) against
// PostgreSQL 18. Migrations live in migrations/ (goose).
package store
