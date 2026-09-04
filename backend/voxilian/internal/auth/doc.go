// Package auth validates Keycloak access JWTs against JWKS and maps
// subjects to accounts (spec D6/§11). No local credentials live here or
// anywhere in the backend.
//
// Staged delivery: M3 provides verified startup-JWKS access-token
// validation (one fetch at construction, immutable key set, real hello
// and reauth paths). M11 adds refresh/cache/rotation and deployment
// hardening (background cache, key rotation, TTL/backoff, pre-auth
// rate limits).
package auth
