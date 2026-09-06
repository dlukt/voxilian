package gateway

import (
	"errors"
	"testing"
	"time"

	"github.com/dlukt/voxilian/internal/session"
	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/world"
)

// rateTestBase is the deterministic epoch every rate test anchors to.
// No test uses time.Now or time.Sleep.
var rateTestBase = time.Unix(1_700_000_000, 0).UTC()

func mustPresenceRegistry(t *testing.T, policy RateLimitPolicy) *PresenceRegistry {
	t.Helper()
	r, err := NewPresenceRegistry(policy)
	if err != nil {
		t.Fatalf("NewPresenceRegistry(%+v): %v", policy, err)
	}
	return r
}

func mustActivate(
	t *testing.T, r *PresenceRegistry,
	sid session.ID, charID int64, entity sim.EntityID,
	center world.CellCoord, hb time.Time,
) PresenceSnapshot {
	t.Helper()
	snap, err := r.Activate(sid, charID, entity, center, hb)
	if err != nil {
		t.Fatalf("Activate(sid=%d char=%d): %v", sid, charID, err)
	}
	return snap
}

func drainMoves(t *testing.T, r *PresenceRegistry, sid session.ID, n int, now time.Time) {
	t.Helper()
	for i := 0; i < n; i++ {
		ok, err := r.AllowMove(sid, now)
		if err != nil {
			t.Fatalf("AllowMove #%d: %v", i, err)
		}
		if !ok {
			t.Fatalf("AllowMove #%d denied, want allowed (burst %d)", i, n)
		}
	}
}

func TestRatePolicyValidation(t *testing.T) {
	for _, bad := range []RateLimitPolicy{
		{MovePerSec: 0, IntentPerSec: 10},
		{MovePerSec: 10, IntentPerSec: 0},
		{MovePerSec: -1, IntentPerSec: 10},
		{MovePerSec: 10, IntentPerSec: -5},
		{MovePerSec: 0, IntentPerSec: 0},
	} {
		if err := bad.Validate(); !errors.Is(err, ErrInvalidRateLimitPolicy) {
			t.Errorf("Validate(%+v) = %v, want ErrInvalidRateLimitPolicy", bad, err)
		}
		if _, err := NewPresenceRegistry(bad); !errors.Is(err, ErrInvalidRateLimitPolicy) {
			t.Errorf("NewPresenceRegistry(%+v) = %v, want ErrInvalidRateLimitPolicy", bad, err)
		}
	}
	// No upper bound is invented: large rates validate.
	if err := (RateLimitPolicy{MovePerSec: 1 << 20, IntentPerSec: 1 << 20}).Validate(); err != nil {
		t.Errorf("large policy rejected: %v", err)
	}
}

func TestMoveBucketInitialBurst(t *testing.T) {
	r := mustPresenceRegistry(t, RateLimitPolicy{MovePerSec: 10, IntentPerSec: 10})
	mustActivate(t, r, 1, 101, 1001, world.CellCoord{}, rateTestBase)
	drainMoves(t, r, 1, 10, rateTestBase)
	if ok, _ := r.AllowMove(1, rateTestBase); ok {
		t.Errorf("11th immediate move allowed at 10/s")
	}
	// Intent bucket is independent and still full.
	for i := 0; i < 10; i++ {
		ok, err := r.AllowIntent(1, rateTestBase)
		if err != nil || !ok {
			t.Fatalf("AllowIntent #%d = %v,%v after move exhaustion", i, ok, err)
		}
	}
	if ok, _ := r.AllowIntent(1, rateTestBase); ok {
		t.Errorf("11th immediate intent allowed at 10/s")
	}
}

func TestMoveBucketExact100msRefill(t *testing.T) {
	r := mustPresenceRegistry(t, RateLimitPolicy{MovePerSec: 10, IntentPerSec: 10})
	mustActivate(t, r, 1, 101, 1001, world.CellCoord{}, rateTestBase)
	drainMoves(t, r, 1, 10, rateTestBase)
	refilled := rateTestBase.Add(100 * time.Millisecond)
	if ok, _ := r.AllowMove(1, refilled); !ok {
		t.Fatalf("move denied at T0+100ms after emptying a 10/s bucket")
	}
	if ok, _ := r.AllowMove(1, refilled); ok {
		t.Errorf("second immediate move allowed at T0+100ms, want exactly one token")
	}
}

func TestBucketFractionalAccumulation(t *testing.T) {
	r := mustPresenceRegistry(t, RateLimitPolicy{MovePerSec: 4, IntentPerSec: 4})
	t0 := rateTestBase
	mustActivate(t, r, 1, 101, 1001, world.CellCoord{}, t0)
	drainMoves(t, r, 1, 4, t0)
	// +125ms at 4/s banks exactly 0.5 token: still denied.
	if ok, _ := r.AllowMove(1, t0.Add(125*time.Millisecond)); ok {
		t.Fatalf("move allowed with 0.5 token banked")
	}
	// A further +125ms completes the token.
	if ok, _ := r.AllowMove(1, t0.Add(250*time.Millisecond)); !ok {
		t.Fatalf("move denied after 250ms refilled 1.0 token at 4/s")
	}
}

func TestBucketCapacityClamp(t *testing.T) {
	r := mustPresenceRegistry(t, RateLimitPolicy{MovePerSec: 10, IntentPerSec: 7})
	mustActivate(t, r, 1, 101, 1001, world.CellCoord{}, rateTestBase)
	idle := rateTestBase.Add(time.Hour)
	// An hour idle refills only to the one-second burst capacity.
	for i := 0; i < 10; i++ {
		if ok, _ := r.AllowMove(1, idle); !ok {
			t.Fatalf("move #%d denied after idle, want burst of 10", i)
		}
	}
	if ok, _ := r.AllowMove(1, idle); ok {
		t.Errorf("11th move allowed after idle: capacity exceeded burst")
	}
	for i := 0; i < 7; i++ {
		if ok, _ := r.AllowIntent(1, idle); !ok {
			t.Fatalf("intent #%d denied after idle, want burst of 7", i)
		}
	}
	if ok, _ := r.AllowIntent(1, idle); ok {
		t.Errorf("8th intent allowed after idle: capacity exceeded burst")
	}
}

func TestBucketClockRegression(t *testing.T) {
	r := mustPresenceRegistry(t, RateLimitPolicy{MovePerSec: 10, IntentPerSec: 10})
	mustActivate(t, r, 1, 101, 1001, world.CellCoord{}, rateTestBase)
	drainMoves(t, r, 1, 10, rateTestBase)
	// A regressed timestamp mints nothing and never moves the clock back.
	if ok, _ := r.AllowMove(1, rateTestBase.Add(-time.Second)); ok {
		t.Fatalf("regressed clock granted a token")
	}
	// +100ms from the prior VALID timestamp still refills exactly one.
	if ok, _ := r.AllowMove(1, rateTestBase.Add(100*time.Millisecond)); !ok {
		t.Fatalf("valid refill after regression denied")
	}
	if ok, _ := r.AllowMove(1, rateTestBase.Add(100*time.Millisecond)); ok {
		t.Errorf("second move right after refill allowed")
	}
}

func TestBucketsIndependentRefill(t *testing.T) {
	r := mustPresenceRegistry(t, RateLimitPolicy{MovePerSec: 10, IntentPerSec: 10})
	mustActivate(t, r, 1, 101, 1001, world.CellCoord{}, rateTestBase)
	drainMoves(t, r, 1, 10, rateTestBase)
	// Movement is empty; intent refills on its own schedule.
	later := rateTestBase.Add(500 * time.Millisecond)
	for i := 0; i < 5; i++ {
		if ok, _ := r.AllowIntent(1, later); !ok {
			t.Fatalf("intent #%d denied at +500ms", i)
		}
	}
	// Movement banked exactly 5 tokens over the same window.
	for i := 0; i < 5; i++ {
		if ok, _ := r.AllowMove(1, later); !ok {
			t.Fatalf("move #%d denied at +500ms", i)
		}
	}
	if ok, _ := r.AllowMove(1, later); ok {
		t.Errorf("6th move allowed at +500ms after 5-token refill")
	}
}

func TestRateUnknownPresence(t *testing.T) {
	r := mustPresenceRegistry(t, RateLimitPolicy{MovePerSec: 10, IntentPerSec: 10})
	if _, err := r.AllowMove(77, rateTestBase); !errors.Is(err, ErrPresenceNotFound) {
		t.Errorf("AllowMove unknown = %v, want ErrPresenceNotFound", err)
	}
	if _, err := r.AllowIntent(77, rateTestBase); !errors.Is(err, ErrPresenceNotFound) {
		t.Errorf("AllowIntent unknown = %v, want ErrPresenceNotFound", err)
	}
	if n := r.Len(); n != 0 {
		t.Errorf("rate check created state: Len = %d", n)
	}
}

func TestRateEpochLifecycleResets(t *testing.T) {
	r := mustPresenceRegistry(t, RateLimitPolicy{MovePerSec: 2, IntentPerSec: 2})
	mustActivate(t, r, 1, 101, 1001, world.CellCoord{}, rateTestBase)
	drainMoves(t, r, 1, 2, rateTestBase)
	if _, err := r.Deactivate(1); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	if _, err := r.AllowMove(1, rateTestBase); !errors.Is(err, ErrPresenceNotFound) {
		t.Fatalf("AllowMove after deactivate = %v, want not-found", err)
	}
	// A later epoch for the same character starts with fresh full buckets.
	mustActivate(t, r, 1, 101, 1001, world.CellCoord{}, rateTestBase.Add(time.Minute))
	drainMoves(t, r, 1, 2, rateTestBase.Add(time.Minute))
}
