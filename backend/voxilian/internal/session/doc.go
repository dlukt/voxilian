// Package session owns the in-memory session and presence registries:
// sessionID → account/character/connection/state, indexed by Keycloak
// sub and by character, plus the per-account lifecycle guard (spec
// §§6.1, 7). No Redis, no external bus: single process (spec D3).
package session
