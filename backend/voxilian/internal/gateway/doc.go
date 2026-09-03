// Package gateway owns the WebSocket edge: authentication, session
// registry, rate limiting, and AOI-filtered fanout to clients.
// The gateway MUST NOT contain gameplay rules; it routes intents to
// the sim and streams authoritative deltas back (spec §§3, 6, 7).
package gateway
