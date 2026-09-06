package gateway

import (
	"errors"
	"time"
)

// ErrInvalidRateLimitPolicy reports a RateLimitPolicy with a non-positive
// movement or intent rate (spec §7.2.9). Match with errors.Is.
var ErrInvalidRateLimitPolicy = errors.New("gateway: invalid rate-limit policy")

// RateLimitPolicy is the Store/config-independent gateway inbound policy
// (spec §7.2.9). Production T5b derives it from
// config.RateLimits.MovePerSec/IntentPerSec; this core never imports
// config.
type RateLimitPolicy struct {
	MovePerSec   int
	IntentPerSec int
}

// Validate reports whether the policy is usable: both rates MUST be
// positive. The spec defines no upper bound, so none is enforced here.
func (p RateLimitPolicy) Validate() error {
	if p.MovePerSec <= 0 || p.IntentPerSec <= 0 {
		return ErrInvalidRateLimitPolicy
	}
	return nil
}

// rateEpsilon absorbs float64 refill rounding so an exactly-refilled
// token is spendable deterministically (e.g. 10/s observed after
// exactly 100 ms).
const rateEpsilon = 1e-9

// tokenBucket is a small lazy continuous-refill token bucket (spec
// §7.2.9): capacity R tokens, starting full; refill R tokens/second
// computed from an explicitly supplied timestamp (no goroutine, no
// ticker, no time.Now inside). Fractional refill accumulates
// internally. Only the owning PresenceRegistry mutates it, always
// under the registry lock.
type tokenBucket struct {
	capacity float64
	tokens   float64
	rate     float64
	last     time.Time
}

// newTokenBucket returns a full bucket for rate tokens/second,
// anchored at the supplied timestamp.
func newTokenBucket(rate int, now time.Time) tokenBucket {
	return tokenBucket{
		capacity: float64(rate),
		tokens:   float64(rate),
		rate:     float64(rate),
		last:     now,
	}
}

// allow refills lazily from now and consumes exactly one token if
// available. A timestamp before the previous one grants nothing and
// never moves the internal clock backwards (elapsed clamps to 0).
func (b *tokenBucket) allow(now time.Time) bool {
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens += elapsed.Seconds() * b.rate
		if b.tokens > b.capacity {
			b.tokens = b.capacity
		}
		b.last = now
	}
	if b.tokens+rateEpsilon >= 1 {
		b.tokens--
		if b.tokens < 0 {
			b.tokens = 0
		}
		return true
	}
	return false
}

// snapshot returns the observable bucket state for diagnostics/tests.
// It is a value copy; callers never observe the live bucket.
func (b *tokenBucket) snapshot() (tokens, capacity float64) {
	return b.tokens, b.capacity
}
