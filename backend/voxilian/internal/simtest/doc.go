// Package simtest holds reusable test harness conventions for the
// Voxilian backend (plan M0-T4): deterministic fake clock, seeded RNG,
// PostgreSQL 18 testcontainers helper, and repo-root golden fixture
// loading. It exists solely to support tests; production code MUST NOT
// import it (enforced by code review, not tooling).
package simtest
