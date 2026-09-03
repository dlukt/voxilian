// Package sim owns the authoritative world simulation: tick loop,
// cells, movement integration, combat, AI, regen, and advancement.
// All randomness and damage rolls happen here, never on the client
// (spec §§4, 5, 9). The sim depends on persistent state only through
// the store package's interfaces, never on pgx/sqlc directly.
package sim
