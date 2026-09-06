package gateway

import (
	"errors"
	"math"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/dlukt/voxilian/internal/session"
	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/world"
)

var presenceTestBase = time.Unix(1_700_000_000, 0).UTC()

func testPolicy() RateLimitPolicy { return RateLimitPolicy{MovePerSec: 10, IntentPerSec: 10} }

func isSortedCells(cells []world.CellCoord) bool {
	for i := 1; i < len(cells); i++ {
		if world.CompareCellCoords(cells[i-1], cells[i]) >= 0 {
			return false
		}
	}
	return true
}

func cellSet(cells []world.CellCoord) map[world.CellCoord]struct{} {
	set := make(map[world.CellCoord]struct{}, len(cells))
	for _, c := range cells {
		set[c] = struct{}{}
	}
	return set
}

func TestBaseAOIExact49(t *testing.T) {
	for _, center := range []world.CellCoord{{X: 0, Z: 0}, {X: -10, Z: -20}, {X: 12345, Z: -6789}} {
		cells, err := BaseAOICells(center)
		if err != nil {
			t.Fatalf("BaseAOICells(%v): %v", center, err)
		}
		if len(cells) != AOICellCount {
			t.Fatalf("BaseAOICells(%v) len = %d, want 49", center, len(cells))
		}
		if len(cellSet(cells)) != AOICellCount {
			t.Fatalf("BaseAOICells(%v) has duplicates", center)
		}
		if !isSortedCells(cells) {
			t.Fatalf("BaseAOICells(%v) not canonical X/Z order", center)
		}
		want := map[world.CellCoord]bool{
			{X: center.X - 3, Z: center.Z - 3}: true,
			center:                             true,
			{X: center.X + 3, Z: center.Z + 3}: true,
		}
		for c := range want {
			if _, ok := cellSet(cells)[c]; !ok {
				t.Fatalf("BaseAOICells(%v) missing %v", center, c)
			}
		}
	}
	cells, _ := BaseAOICells(world.CellCoord{})
	if first, last := cells[0], cells[len(cells)-1]; first != (world.CellCoord{X: -3, Z: -3}) || last != (world.CellCoord{X: 3, Z: 3}) {
		t.Fatalf("center {0,0} first/last = %v/%v, want {-3,-3}/{3,3}", first, last)
	}
}

func TestBaseAOIOverflow(t *testing.T) {
	// A center 3 inside the limit still represents the full neighborhood.
	safe := world.CellCoord{X: math.MaxInt32 - 3, Z: math.MinInt32 + 3}
	if cells, err := BaseAOICells(safe); err != nil || len(cells) != AOICellCount {
		t.Fatalf("safe edge center: cells=%d err=%v", len(cells), err)
	}
	for _, center := range []world.CellCoord{
		{X: math.MaxInt32, Z: 0},
		{X: math.MaxInt32 - 2, Z: 0},
		{X: 0, Z: math.MinInt32},
		{X: math.MinInt32, Z: math.MaxInt32},
	} {
		if _, err := BaseAOICells(center); !errors.Is(err, ErrAOICellRange) {
			t.Errorf("BaseAOICells(%v) = %v, want ErrAOICellRange", center, err)
		}
	}
	// Overflow activation mutates nothing.
	r := mustPresenceRegistry(t, testPolicy())
	if _, err := r.Activate(1, 101, 1001, world.CellCoord{X: math.MaxInt32}, presenceTestBase); !errors.Is(err, ErrAOICellRange) {
		t.Fatalf("Activate overflow = %v, want ErrAOICellRange", err)
	}
	if n := r.Len(); n != 0 {
		t.Fatalf("Len after failed activate = %d", n)
	}
	if _, err := r.Activate(1, 101, 1001, world.CellCoord{}, presenceTestBase); err != nil {
		t.Fatalf("Activate after failed attempt: %v", err)
	}
}

func TestPresenceActivateValidation(t *testing.T) {
	r := mustPresenceRegistry(t, testPolicy())
	for _, tc := range []struct {
		name string
		sid  session.ID
		char int64
		ent  sim.EntityID
	}{
		{"zero session", 0, 101, 1001},
		{"zero character", 1, 0, 1001},
		{"negative character", 1, -5, 1001},
		{"zero entity", 1, 101, sim.InvalidEntityID},
	} {
		if _, err := r.Activate(tc.sid, tc.char, tc.ent, world.CellCoord{}, presenceTestBase); !errors.Is(err, ErrPresenceInvalidIdentity) {
			t.Errorf("%s: err = %v, want ErrPresenceInvalidIdentity", tc.name, err)
		}
	}
	if n := r.Len(); n != 0 {
		t.Errorf("Len after invalid activates = %d", n)
	}
}

func TestPresenceActivateOwnHandleAndSubscriptions(t *testing.T) {
	r := mustPresenceRegistry(t, testPolicy())
	snap := mustActivate(t, r, 7, 101, 1001, world.CellCoord{}, presenceTestBase)
	if snap.OwnNetID != NetEntityID(1) {
		t.Errorf("OwnNetID = %d, want 1", snap.OwnNetID)
	}
	if len(snap.Cells) != AOICellCount || !isSortedCells(snap.Cells) {
		t.Errorf("snapshot cells: len=%d sorted=%v", len(snap.Cells), isSortedCells(snap.Cells))
	}
	if !snap.HeartbeatAt.Equal(presenceTestBase) {
		t.Errorf("HeartbeatAt = %v, want exact supplied time", snap.HeartbeatAt)
	}
	for _, c := range snap.Cells {
		subs := r.Subscribers(c)
		if len(subs) != 1 || subs[0] != 7 {
			t.Fatalf("cell %v subscribers = %v, want [7]", c, subs)
		}
	}
	if h, created, err := r.EnsureVisible(7, 1001); err != nil || h != 1 || created {
		t.Errorf("EnsureVisible(own) = %d,%v,%v; want 1,false,nil", h, created, err)
	}
}

func TestPresenceActivateConflictsAtomic(t *testing.T) {
	r := mustPresenceRegistry(t, testPolicy())
	mustActivate(t, r, 1, 101, 1001, world.CellCoord{}, presenceTestBase)
	before := r.Subscribers(world.CellCoord{})
	for _, tc := range []struct {
		name string
		sid  session.ID
		char int64
		ent  sim.EntityID
		err  error
	}{
		{"same session", 1, 102, 1002, ErrSessionAlreadyPresent},
		{"same character", 2, 101, 1002, ErrCharacterAlreadyPresent},
		{"same entity", 2, 102, 1001, ErrControlledEntityAlreadyPresent},
	} {
		if _, err := r.Activate(tc.sid, tc.char, tc.ent, world.CellCoord{X: 50, Z: 50}, presenceTestBase); !errors.Is(err, tc.err) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.err)
		}
	}
	if n := r.Len(); n != 1 {
		t.Errorf("Len after conflicts = %d, want 1", n)
	}
	// Original indexes, subscriptions, and handles are untouched.
	snap, err := r.Snapshot(1)
	if err != nil || snap.CenterCell != (world.CellCoord{}) || len(snap.Cells) != AOICellCount {
		t.Fatalf("original presence disturbed: %+v %v", snap, err)
	}
	if after := r.Subscribers(world.CellCoord{}); len(after) != len(before) {
		t.Errorf("subscriber set changed by failed activates")
	}
	if len(r.Subscribers(world.CellCoord{X: 50, Z: 50})) != 0 {
		t.Errorf("failed activate leaked subscriptions at new center")
	}
	if _, ok := r.ResolveHandle(2, 1); ok {
		t.Errorf("failed activate leaked handle state")
	}
}

func updateAndCheck(
	t *testing.T, r *PresenceRegistry, sid session.ID,
	center world.CellCoord, wantEntered, wantExited int,
) SubscriptionDelta {
	t.Helper()
	delta, err := r.UpdateCenter(sid, center)
	if err != nil {
		t.Fatalf("UpdateCenter(%v): %v", center, err)
	}
	if len(delta.Entered) != wantEntered || len(delta.Exited) != wantExited {
		t.Fatalf("UpdateCenter(%v): entered=%d exited=%d, want %d/%d",
			center, len(delta.Entered), len(delta.Exited), wantEntered, wantExited)
	}
	if !isSortedCells(delta.Entered) || !isSortedCells(delta.Exited) {
		t.Fatalf("delta not canonical for %v", center)
	}
	snap, _ := r.Snapshot(sid)
	if len(snap.Cells) != AOICellCount || !isSortedCells(snap.Cells) {
		t.Fatalf("post-update cells invalid: len=%d", len(snap.Cells))
	}
	if snap.CenterCell != center {
		t.Fatalf("center = %v, want %v", snap.CenterCell, center)
	}
	// Reverse index exactly matches the forward set.
	for _, c := range snap.Cells {
		found := false
		for _, s := range r.Subscribers(c) {
			if s == sid {
				found = true
			}
		}
		if !found {
			t.Fatalf("cell %v missing subscriber %d", c, sid)
		}
	}
	for _, c := range delta.Exited {
		for _, s := range r.Subscribers(c) {
			if s == sid {
				t.Fatalf("exited cell %v still subscribes %d", c, sid)
			}
		}
	}
	return delta
}

func TestPresenceSameCenterNoChurn(t *testing.T) {
	r := mustPresenceRegistry(t, testPolicy())
	mustActivate(t, r, 1, 101, 1001, world.CellCoord{X: 5, Z: -3}, presenceTestBase)
	delta, err := r.UpdateCenter(1, world.CellCoord{X: 5, Z: -3})
	if err != nil || len(delta.Entered) != 0 || len(delta.Exited) != 0 {
		t.Fatalf("same-center delta = %+v,%v; want empty", delta, err)
	}
	if subs := r.Subscribers(world.CellCoord{X: 5, Z: -3}); len(subs) != 1 {
		t.Fatalf("subscribers after no-op = %v", subs)
	}
}

func TestPresenceAxialChurn7x7(t *testing.T) {
	r := mustPresenceRegistry(t, testPolicy())
	mustActivate(t, r, 1, 101, 1001, world.CellCoord{}, presenceTestBase)
	updateAndCheck(t, r, 1, world.CellCoord{X: 1}, 7, 7)
}

func TestPresenceDiagonalChurn13x13(t *testing.T) {
	r := mustPresenceRegistry(t, testPolicy())
	mustActivate(t, r, 1, 101, 1001, world.CellCoord{}, presenceTestBase)
	updateAndCheck(t, r, 1, world.CellCoord{X: 1, Z: 1}, 13, 13)
}

func TestPresenceDisjointJump49x49(t *testing.T) {
	r := mustPresenceRegistry(t, testPolicy())
	mustActivate(t, r, 1, 101, 1001, world.CellCoord{}, presenceTestBase)
	updateAndCheck(t, r, 1, world.CellCoord{X: 100, Z: -100}, 49, 49)
	if subs := r.Subscribers(world.CellCoord{}); len(subs) != 0 {
		t.Fatalf("old center still subscribed: %v", subs)
	}
}

func TestPresenceUpdateUnknownAndOverflow(t *testing.T) {
	r := mustPresenceRegistry(t, testPolicy())
	if _, err := r.UpdateCenter(9, world.CellCoord{}); !errors.Is(err, ErrPresenceNotFound) {
		t.Errorf("UpdateCenter unknown = %v", err)
	}
	mustActivate(t, r, 1, 101, 1001, world.CellCoord{}, presenceTestBase)
	if _, err := r.UpdateCenter(1, world.CellCoord{X: math.MaxInt32}); !errors.Is(err, ErrAOICellRange) {
		t.Errorf("UpdateCenter overflow = %v", err)
	}
	if snap, _ := r.Snapshot(1); snap.CenterCell != (world.CellCoord{}) {
		t.Errorf("overflow update mutated center to %v", snap.CenterCell)
	}
}

func TestPresenceSubscribersMultiple(t *testing.T) {
	r := mustPresenceRegistry(t, testPolicy())
	mustActivate(t, r, 3, 103, 1003, world.CellCoord{}, presenceTestBase)
	mustActivate(t, r, 1, 101, 1001, world.CellCoord{X: 1}, presenceTestBase)
	mustActivate(t, r, 2, 102, 1002, world.CellCoord{X: -1}, presenceTestBase)
	subs := r.Subscribers(world.CellCoord{})
	if len(subs) != 3 || subs[0] != 1 || subs[1] != 2 || subs[2] != 3 {
		t.Fatalf("subscribers = %v, want [1 2 3] sorted unique", subs)
	}
	if _, err := r.Deactivate(2); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	if subs := r.Subscribers(world.CellCoord{}); len(subs) != 2 || subs[0] != 1 || subs[1] != 3 {
		t.Fatalf("subscribers after deactivate = %v, want [1 3]", subs)
	}
	// Mutating the result cannot affect the registry.
	subs[0] = 99
	if again := r.Subscribers(world.CellCoord{}); again[0] != 1 {
		t.Fatalf("subscriber copy aliases registry: %v", again)
	}
}

func TestPresenceDeactivateCleanup(t *testing.T) {
	r := mustPresenceRegistry(t, testPolicy())
	mustActivate(t, r, 1, 101, 1001, world.CellCoord{}, presenceTestBase)
	if _, _, err := r.EnsureVisible(1, 2001); err != nil {
		t.Fatal(err)
	}
	updateAndCheck(t, r, 1, world.CellCoord{X: 2, Z: 2}, 24, 24)
	prior, err := r.Deactivate(1)
	if err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	if prior.OwnNetID != 1 || len(prior.Cells) != AOICellCount {
		t.Errorf("prior snapshot incomplete: %+v", prior)
	}
	if n := r.Len(); n != 0 {
		t.Errorf("Len after deactivate = %d", n)
	}
	for _, c := range prior.Cells {
		for _, s := range r.Subscribers(c) {
			if s == 1 {
				t.Fatalf("cell %v retains deactivated session", c)
			}
		}
	}
	for _, err := range []error{
		func() error { _, e := r.Snapshot(1); return e }(),
		func() error { _, _, e := r.EnsureVisible(1, 2002); return e }(),
		func() error { _, _, e := r.HideVisible(1, 2001); return e }(),
		func() error { return r.TouchHeartbeat(1, presenceTestBase) }(),
		func() error { _, e := r.AllowMove(1, presenceTestBase); return e }(),
	} {
		if !errors.Is(err, ErrPresenceNotFound) {
			t.Errorf("post-deactivate op = %v, want ErrPresenceNotFound", err)
		}
	}
	if _, ok := r.ResolveHandle(1, 1); ok {
		t.Errorf("deactivated handle still resolves")
	}
	if _, err := r.Deactivate(1); !errors.Is(err, ErrPresenceNotFound) {
		t.Errorf("second Deactivate = %v, want ErrPresenceNotFound", err)
	}
}

func TestPresenceSnapshotImmutability(t *testing.T) {
	r := mustPresenceRegistry(t, testPolicy())
	mustActivate(t, r, 1, 101, 1001, world.CellCoord{}, presenceTestBase)
	snap, _ := r.Snapshot(1)
	snap.Cells[0] = world.CellCoord{X: 9999, Z: 9999}
	snap.Cells = append(snap.Cells, world.CellCoord{})
	fresh, _ := r.Snapshot(1)
	if len(fresh.Cells) != AOICellCount || fresh.Cells[0] == (world.CellCoord{X: 9999, Z: 9999}) {
		t.Fatalf("snapshot mutation leaked into registry")
	}
}

func TestEnsureVisibleAllocation(t *testing.T) {
	r := mustPresenceRegistry(t, testPolicy())
	mustActivate(t, r, 1, 101, 1001, world.CellCoord{}, presenceTestBase)
	h, created, err := r.EnsureVisible(1, 2001)
	if err != nil || h != 2 || !created {
		t.Fatalf("first ensure = %d,%v,%v; want 2,true,nil", h, created, err)
	}
	h, created, err = r.EnsureVisible(1, 2002)
	if err != nil || h != 3 || !created {
		t.Fatalf("second ensure = %d,%v,%v; want 3,true,nil", h, created, err)
	}
	h, created, err = r.EnsureVisible(1, 2001)
	if err != nil || h != 2 || created {
		t.Fatalf("re-ensure = %d,%v,%v; want 2,false,nil", h, created, err)
	}
	if ent, ok := r.ResolveHandle(1, 2); !ok || ent != 2001 {
		t.Fatalf("ResolveHandle(2) = %d,%v; want 2001,true", ent, ok)
	}
}

func TestHideReentryGetsNewHandle(t *testing.T) {
	r := mustPresenceRegistry(t, testPolicy())
	mustActivate(t, r, 1, 101, 1001, world.CellCoord{}, presenceTestBase)
	if _, _, err := r.EnsureVisible(1, 2001); err != nil {
		t.Fatal(err)
	}
	retired, gone, err := r.HideVisible(1, 2001)
	if err != nil || !gone || retired != 2 {
		t.Fatalf("hide = %d,%v,%v; want 2,true,nil", retired, gone, err)
	}
	if _, ok := r.ResolveHandle(1, 2); ok {
		t.Fatalf("retired handle 2 still resolves")
	}
	h, created, err := r.EnsureVisible(1, 2001)
	if err != nil || !created || h != 3 {
		t.Fatalf("re-ensure after hide = %d,%v,%v; want 3,true,nil (never 2 again)", h, created, err)
	}
	// Hiding an absent entity is a no-op, not an error.
	if h, gone, err := r.HideVisible(1, 2999); err != nil || gone || h != 0 {
		t.Fatalf("hide absent = %d,%v,%v; want 0,false,nil", h, gone, err)
	}
}

func TestVisibilitySessionIndependence(t *testing.T) {
	r := mustPresenceRegistry(t, testPolicy())
	mustActivate(t, r, 1, 101, 1001, world.CellCoord{}, presenceTestBase)
	mustActivate(t, r, 2, 102, 1002, world.CellCoord{}, presenceTestBase)
	ha, _, _ := r.EnsureVisible(1, 2001)
	mustActivate(t, r, 3, 103, 1003, world.CellCoord{}, presenceTestBase)
	hb, _, _ := r.EnsureVisible(2, 2001)
	if ha != 2 || hb != 2 {
		// Both first-visible handles coincide here; the invariant is
		// resolution isolation, asserted below either way.
		t.Logf("handles: A=%d B=%d", ha, hb)
	}
	if _, _, err := r.EnsureVisible(2, 2002); err != nil {
		t.Fatal(err)
	}
	if ent, ok := r.ResolveHandle(1, 3); ok || ent != sim.InvalidEntityID {
		t.Fatalf("session A resolved session B's handle 3 -> %d", ent)
	}
	if _, _, err := r.HideVisible(2, 2001); err != nil {
		t.Fatal(err)
	}
	if ent, ok := r.ResolveHandle(1, ha); !ok || ent != 2001 {
		t.Fatalf("session B hide affected session A: %d,%v", ent, ok)
	}
}

func TestHandleZeroInvalid(t *testing.T) {
	r := mustPresenceRegistry(t, testPolicy())
	mustActivate(t, r, 1, 101, 1001, world.CellCoord{}, presenceTestBase)
	if _, _, err := r.EnsureVisible(1, sim.InvalidEntityID); !errors.Is(err, ErrPresenceInvalidIdentity) {
		t.Errorf("EnsureVisible(0 entity) = %v", err)
	}
	if _, _, err := r.HideVisible(1, sim.InvalidEntityID); !errors.Is(err, ErrPresenceInvalidIdentity) {
		t.Errorf("HideVisible(0 entity) = %v", err)
	}
	if _, ok := r.ResolveHandle(1, InvalidNetEntityID); ok {
		t.Errorf("ResolveHandle(0) resolved")
	}
	if _, ok := r.ResolveHandle(1, 9999); ok {
		t.Errorf("ResolveHandle(unknown) resolved")
	}
}

func TestHandleExhaustion(t *testing.T) {
	r := mustPresenceRegistry(t, testPolicy())
	mustActivate(t, r, 1, 101, 1001, world.CellCoord{}, presenceTestBase)
	r.mu.Lock()
	r.bySess[1].next = NetEntityID(math.MaxUint32)
	r.mu.Unlock()
	h, created, err := r.EnsureVisible(1, 2001)
	if err != nil || !created || h != NetEntityID(math.MaxUint32) {
		t.Fatalf("MaxUint32 ensure = %d,%v,%v", h, created, err)
	}
	if _, _, err := r.EnsureVisible(1, 2002); !errors.Is(err, ErrNetEntityIDExhausted) {
		t.Fatalf("post-max ensure = %v, want ErrNetEntityIDExhausted", err)
	}
	if _, _, err := r.EnsureVisible(1, 2003); !errors.Is(err, ErrNetEntityIDExhausted) {
		t.Fatalf("second post-max ensure = %v, want ErrNetEntityIDExhausted", err)
	}
	// No wrap to 0, existing mappings unchanged.
	if _, ok := r.ResolveHandle(1, InvalidNetEntityID); ok {
		t.Fatalf("exhaustion wrapped to handle 0")
	}
	if ent, ok := r.ResolveHandle(1, NetEntityID(math.MaxUint32)); !ok || ent != 2001 {
		t.Fatalf("MaxUint32 mapping disturbed: %d,%v", ent, ok)
	}
	if ent, ok := r.ResolveHandle(1, 1); !ok || ent != 1001 {
		t.Fatalf("own mapping disturbed by exhaustion: %d,%v", ent, ok)
	}
}

func TestOwnVisibilityPinned(t *testing.T) {
	r := mustPresenceRegistry(t, testPolicy())
	mustActivate(t, r, 1, 101, 1001, world.CellCoord{}, presenceTestBase)
	if _, _, err := r.HideVisible(1, 1001); !errors.Is(err, ErrOwnEntityVisibility) {
		t.Fatalf("HideVisible(own) = %v, want ErrOwnEntityVisibility", err)
	}
	if ent, ok := r.ResolveHandle(1, 1); !ok || ent != 1001 {
		t.Fatalf("own mapping disturbed by hide attempt: %d,%v", ent, ok)
	}
}

func TestHeartbeatTouchMonotonic(t *testing.T) {
	r := mustPresenceRegistry(t, testPolicy())
	mustActivate(t, r, 1, 101, 1001, world.CellCoord{}, presenceTestBase)
	if err := r.TouchHeartbeat(1, presenceTestBase.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := r.TouchHeartbeat(1, presenceTestBase.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := r.TouchHeartbeat(1, presenceTestBase.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	snap, _ := r.Snapshot(1)
	if want := presenceTestBase.Add(10 * time.Second); !snap.HeartbeatAt.Equal(want) {
		t.Fatalf("HeartbeatAt = %v, want %v (equal/older are no-ops)", snap.HeartbeatAt, want)
	}
	if err := r.TouchHeartbeat(9, presenceTestBase); !errors.Is(err, ErrPresenceNotFound) {
		t.Errorf("TouchHeartbeat unknown = %v", err)
	}
}

func TestStaleBoundaryInclusive(t *testing.T) {
	r := mustPresenceRegistry(t, testPolicy())
	mustActivate(t, r, 1, 101, 1001, world.CellCoord{}, presenceTestBase)
	for _, tc := range []struct {
		at    time.Time
		stale bool
	}{
		{presenceTestBase.Add(29999 * time.Millisecond), false},
		{presenceTestBase.Add(30 * time.Second), true},
		{presenceTestBase.Add(31 * time.Second), true},
	} {
		stale := r.StaleSessions(tc.at)
		if got := len(stale) == 1; got != tc.stale {
			t.Errorf("at %v: stale=%v, want %v", tc.at, got, tc.stale)
		}
	}
}

func TestStaleOrderingAndNoDelete(t *testing.T) {
	r := mustPresenceRegistry(t, testPolicy())
	mustActivate(t, r, 5, 105, 1005, world.CellCoord{}, presenceTestBase)
	mustActivate(t, r, 2, 102, 1002, world.CellCoord{}, presenceTestBase)
	mustActivate(t, r, 9, 109, 1009, world.CellCoord{}, presenceTestBase)
	now := presenceTestBase.Add(time.Minute)
	first, second := r.StaleSessions(now), r.StaleSessions(now)
	for _, got := range [][]session.ID{first, second} {
		if len(got) != 3 || got[0] != 2 || got[1] != 5 || got[2] != 9 {
			t.Fatalf("stale sessions = %v, want [2 5 9] sorted", got)
		}
	}
	if n := r.Len(); n != 3 {
		t.Fatalf("stale query deleted records: Len = %d", n)
	}
}

func TestFutureHeartbeatNeverStale(t *testing.T) {
	r := mustPresenceRegistry(t, testPolicy())
	mustActivate(t, r, 1, 101, 1001, world.CellCoord{}, presenceTestBase.Add(time.Hour))
	if stale := r.StaleSessions(presenceTestBase); len(stale) != 0 {
		t.Fatalf("future heartbeat reported stale: %v", stale)
	}
}

// checkInvariants asserts every B42 model invariant under a read lock.
func checkInvariants(t *testing.T, r *PresenceRegistry, lastNext map[session.ID]NetEntityID, lastHB map[session.ID]time.Time, retired map[session.ID]map[NetEntityID]bool) {
	t.Helper()
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.bySess) != len(r.byChar) || len(r.bySess) != len(r.byEntity) {
		t.Fatalf("index counts disagree: sess=%d char=%d entity=%d",
			len(r.bySess), len(r.byChar), len(r.byEntity))
	}
	wantSubs := make(map[world.CellCoord]map[session.ID]struct{})
	for sid, p := range r.bySess {
		if len(p.cells) != AOICellCount || !isSortedCells(p.cells) {
			t.Fatalf("sid %d: cells len=%d sorted=%v", sid, len(p.cells), isSortedCells(p.cells))
		}
		if p.forward[p.entityID] != 1 || p.reverse[1] != p.entityID {
			t.Fatalf("sid %d: own handle not pinned at 1", sid)
		}
		if len(p.forward) != len(p.reverse) {
			t.Fatalf("sid %d: handle maps not bijective", sid)
		}
		for ent, h := range p.forward {
			if h == InvalidNetEntityID || ent == sim.InvalidEntityID {
				t.Fatalf("sid %d: zero handle/entity in table", sid)
			}
			if p.reverse[h] != ent {
				t.Fatalf("sid %d: forward/reverse disagree on %d", sid, h)
			}
		}
		for h := range p.reverse {
			if h == InvalidNetEntityID {
				t.Fatalf("sid %d: zero reverse handle", sid)
			}
		}
		if prev, ok := lastNext[sid]; ok && p.next != InvalidNetEntityID && p.next < prev {
			t.Fatalf("sid %d: next handle decreased %d -> %d", sid, prev, p.next)
		}
		for h := range retired[sid] {
			if _, ok := p.reverse[h]; ok {
				t.Fatalf("sid %d: retired handle %d resolves", sid, h)
			}
		}
		if mt, mc := p.move.snapshot(); mt < 0 || mt > mc {
			t.Fatalf("sid %d: move tokens %v/%v out of bounds", sid, mt, mc)
		}
		if it, ic := p.intent.snapshot(); it < 0 || it > ic {
			t.Fatalf("sid %d: intent tokens %v/%v out of bounds", sid, it, ic)
		}
		if prev, ok := lastHB[sid]; ok && p.heartbeatAt.Before(prev) {
			t.Fatalf("sid %d: heartbeat moved backwards", sid)
		}
		for _, c := range p.cells {
			set := wantSubs[c]
			if set == nil {
				set = make(map[session.ID]struct{})
				wantSubs[c] = set
			}
			set[sid] = struct{}{}
		}
	}
	if len(wantSubs) != len(r.subs) {
		t.Fatalf("reverse index has %d cells, forward implies %d", len(r.subs), len(wantSubs))
	}
	for c, want := range wantSubs {
		got := r.subs[c]
		if len(got) != len(want) {
			t.Fatalf("cell %v: %d subscribers, want %d", c, len(got), len(want))
		}
		for sid := range want {
			if _, ok := got[sid]; !ok {
				t.Fatalf("cell %v missing subscriber %d", c, sid)
			}
		}
	}
}

func TestPresencePropertyModel(t *testing.T) {
	centers := []world.CellCoord{
		{}, {X: 1}, {Z: 1}, {X: 1, Z: 1}, {X: -2, Z: 3},
		{X: 100, Z: -100}, {X: -500, Z: 500},
	}
	for seed := 0; seed < 128; seed++ {
		rng := rand.New(rand.NewSource(int64(seed)))
		r := mustPresenceRegistry(t, testPolicy())
		lastNext := make(map[session.ID]NetEntityID)
		lastHB := make(map[session.ID]time.Time)
		retired := make(map[session.ID]map[NetEntityID]bool)
		now := presenceTestBase
		recordHB := func(sid session.ID) {
			if snap, err := r.Snapshot(sid); err == nil {
				lastHB[sid] = snap.HeartbeatAt
			}
		}
		for step := 0; step < 128; step++ {
			now = now.Add(time.Duration(rng.Intn(5000)) * time.Millisecond)
			sid := session.ID(rng.Intn(4) + 1)
			char := int64(sid) + 100
			ctrl := sim.EntityID(1000 + uint64(sid))
			vis := sim.EntityID(2000 + uint64(rng.Intn(8)))
			switch rng.Intn(10) {
			case 0:
				snap, err := r.Activate(sid, char, ctrl, centers[rng.Intn(len(centers))], now)
				if err == nil {
					lastNext[sid] = 2
					lastHB[sid] = snap.HeartbeatAt
					delete(retired, sid)
				}
			case 1:
				if prior, err := r.Deactivate(sid); err == nil {
					_ = prior
					delete(lastNext, sid)
					delete(lastHB, sid)
					delete(retired, sid)
				}
			case 2:
				if _, err := r.UpdateCenter(sid, centers[rng.Intn(len(centers))]); err == nil {
					recordHB(sid)
				}
			case 3:
				if _, _, err := r.EnsureVisible(sid, vis); err == nil {
					recordHB(sid)
				}
			case 4:
				if h, gone, err := r.HideVisible(sid, vis); err == nil && gone {
					set := retired[sid]
					if set == nil {
						set = make(map[NetEntityID]bool)
						retired[sid] = set
					}
					set[h] = true
				}
			case 5:
				ts := now
				if rng.Intn(4) == 0 {
					ts = now.Add(-time.Duration(rng.Intn(5000)) * time.Millisecond)
				}
				if err := r.TouchHeartbeat(sid, ts); err == nil {
					recordHB(sid)
				}
			case 6:
				_, _ = r.AllowMove(sid, now)
			case 7:
				_, _ = r.AllowIntent(sid, now)
			case 8:
				_ = r.StaleSessions(now)
				for _, c := range centers[:3] {
					_ = r.Subscribers(c)
				}
			case 9:
				_, _ = r.ResolveHandle(sid, NetEntityID(rng.Intn(6)))
			}
			r.mu.RLock()
			for s, p := range r.bySess {
				lastNext[s] = p.next
			}
			r.mu.RUnlock()
			checkInvariants(t, r, lastNext, lastHB, retired)
		}
	}
}

func TestPresenceConcurrencyDifferentSessions(t *testing.T) {
	r := mustPresenceRegistry(t, testPolicy())
	const n = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 1; i <= n; i++ {
		wg.Add(1)
		go func(sid session.ID) {
			defer wg.Done()
			<-start
			char := int64(sid) + 100
			now := presenceTestBase
			if _, err := r.Activate(sid, char, sim.EntityID(1000+uint64(sid)), world.CellCoord{X: int32(sid)}, now); err != nil {
				t.Errorf("activate %d: %v", sid, err)
				return
			}
			for step := 0; step < 25; step++ {
				now = now.Add(time.Millisecond)
				ctr := world.CellCoord{X: int32(sid) + int32(step%3)}
				if _, err := r.UpdateCenter(sid, ctr); err != nil {
					t.Errorf("update %d: %v", sid, err)
				}
				if _, _, err := r.EnsureVisible(sid, sim.EntityID(2000+uint64(step%5))); err != nil {
					t.Errorf("ensure %d: %v", sid, err)
				}
				_ = r.TouchHeartbeat(sid, now)
				if _, err := r.Snapshot(sid); err != nil {
					t.Errorf("snapshot %d: %v", sid, err)
				}
				if _, err := r.AllowMove(sid, now); err != nil {
					t.Errorf("allow %d: %v", sid, err)
				}
			}
			if _, err := r.Deactivate(sid); err != nil {
				t.Errorf("deactivate %d: %v", sid, err)
			}
		}(session.ID(i))
	}
	// Overlapping read-only subscriber/stale queries.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 200; i++ {
			_ = r.Subscribers(world.CellCoord{X: int32(i % 10)})
			_ = r.StaleSessions(presenceTestBase.Add(time.Duration(i) * time.Millisecond))
		}
	}()
	close(start)
	wg.Wait()
	if n := r.Len(); n != 0 {
		t.Fatalf("Len after concurrent run = %d, want 0", n)
	}
}

func TestPresenceConcurrencySameSession(t *testing.T) {
	r := mustPresenceRegistry(t, testPolicy())
	mustActivate(t, r, 1, 101, 1001, world.CellCoord{}, presenceTestBase)
	start := make(chan struct{})
	var wg sync.WaitGroup
	worker := func(w int) {
		defer wg.Done()
		<-start
		base := presenceTestBase.Add(time.Duration(w) * time.Second)
		for step := 0; step < 50; step++ {
			now := base.Add(time.Duration(step) * time.Millisecond)
			ctr := world.CellCoord{X: int32(step % 2)}
			delta, err := r.UpdateCenter(1, ctr)
			if err != nil {
				t.Errorf("update: %v", err)
			}
			_ = delta
			_ = r.Subscribers(world.CellCoord{X: int32(step % 2)})
			snap, err := r.Snapshot(1)
			if err != nil {
				t.Errorf("snapshot: %v", err)
				continue
			}
			if len(snap.Cells) != AOICellCount || !isSortedCells(snap.Cells) {
				t.Errorf("torn 49-cell view: len=%d", len(snap.Cells))
			}
			_ = r.TouchHeartbeat(1, now)
			// Private visibility entities per worker: no defined-outcome race.
			ent := sim.EntityID(3000 + uint64(w*100+step%25))
			if _, _, err := r.EnsureVisible(1, ent); err != nil {
				t.Errorf("ensure: %v", err)
			}
		}
	}
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go worker(w)
	}
	close(start)
	wg.Wait()
	snap, err := r.Snapshot(1)
	if err != nil || len(snap.Cells) != AOICellCount {
		t.Fatalf("final presence invalid: %+v %v", snap, err)
	}
	checkInvariants(t, r, map[session.ID]NetEntityID{1: snap.OwnNetID},
		map[session.ID]time.Time{1: presenceTestBase}, map[session.ID]map[NetEntityID]bool{})
}
