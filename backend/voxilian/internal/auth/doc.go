// Package auth validates Keycloak access JWTs against cached JWKS and
// maps subjects to accounts (spec D6/§11). No local credentials live
// here or anywhere in the backend. Full implementation lands in M11.
package auth
