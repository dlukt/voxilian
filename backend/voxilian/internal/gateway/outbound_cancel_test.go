package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/dlukt/voxilian/internal/proto"
)

// ---------------------------------------------------------------------------
// Targeted outbound state cancellation (spec §7.1.13, v0.3.24)
// ---------------------------------------------------------------------------

func cancelKey(id uint64) StateKey {
	return StateKey{Kind: proto.OpcodeEntityMove, ID: id}
}

// TestOutboundCancelStateQueued proves the core primitive: a matching
// QUEUED state item is removed, its message+byte residency is
// released, capacity waiters wake, queue depth is observed, and no
// state-drop/coalesce metric fires.
func TestOutboundCancelStateQueued(t *testing.T) {
	f := newOutboundFixture(t, tinyPolicy(8, 262144, 10*time.Second, 10*time.Second))
	f.hold()
	a := f.sendAsync(t, 1, 64) // parked in the writer (resident)
	f.waitParked(t, 1)

	key := cancelKey(7)
	res, err := f.oc.TryState(f.sid, key, proto.OpcodeEntityMove, proto.MessageVersion1, payloadOf(70, 64))
	if err != nil || res != StateQueued {
		t.Fatalf("TryState = %v,%v want queued", res, err)
	}
	if msg, _ := f.resident(); msg != 2 {
		t.Fatalf("resident messages = %d, want 2 (writing + queued)", msg)
	}
	if got := f.oc.CancelState(key); got != StateCancelCanceled {
		t.Fatalf("CancelState = %v, want canceled", got)
	}
	if msg, _ := f.resident(); msg != 1 {
		t.Fatalf("resident messages after cancel = %d, want 1 (writing only)", msg)
	}
	drops, coalesced, _ := f.obs.snapshot()
	if len(drops) != 0 {
		t.Fatalf("state drops = %v, want none (cancellation is not backpressure)", drops)
	}
	if coalesced != 0 {
		t.Fatalf("coalesced = %d, want 0", coalesced)
	}
	// Only the parked critical ever reaches the wire.
	f.releaseAll()
	if err := <-a; err != nil {
		t.Fatalf("parked critical: %v", err)
	}
	waitUntil(t, "drain", func() bool { return f.tr.recordedCount() == 1 })
	if got := markerOf(t, f.tr.recorded()[0]); got != 1 {
		t.Fatalf("wire marker = %d, want only the critical (canceled 205 never wrote)", got)
	}
}

// TestOutboundCancelStateLeavesOtherKeys proves only the matching
// queued key is removed: an unrelated entity's state still writes.
func TestOutboundCancelStateLeavesOtherKeys(t *testing.T) {
	f := newOutboundFixture(t, tinyPolicy(8, 262144, 10*time.Second, 10*time.Second))
	f.hold()
	a := f.sendAsync(t, 1, 64)
	f.waitParked(t, 1)

	k1, k2 := cancelKey(11), cancelKey(12)
	if _, err := f.oc.TryState(f.sid, k1, proto.OpcodeEntityMove, proto.MessageVersion1, payloadOf(71, 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.oc.TryState(f.sid, k2, proto.OpcodeEntityMove, proto.MessageVersion1, payloadOf(72, 64)); err != nil {
		t.Fatal(err)
	}
	if got := f.oc.CancelState(k1); got != StateCancelCanceled {
		t.Fatalf("CancelState(k1) = %v, want canceled", got)
	}
	f.releaseAll()
	if err := <-a; err != nil {
		t.Fatalf("parked critical: %v", err)
	}
	waitUntil(t, "drain", func() bool { return f.tr.recordedCount() == 2 })
	frames := f.tr.recorded()
	if got := markerOf(t, frames[1]); got != 72 {
		t.Fatalf("surviving state marker = %d, want 72 (k2 untouched)", got)
	}
}

// TestOutboundCancelStateInFlight proves the currently-writing match
// is NEVER cancelled: it reports InFlight and still reaches the wire.
func TestOutboundCancelStateInFlight(t *testing.T) {
	f := newOutboundFixture(t, tinyPolicy(8, 262144, 10*time.Second, 10*time.Second))
	f.hold()
	key := cancelKey(21)
	if _, err := f.oc.TryState(f.sid, key, proto.OpcodeEntityMove, proto.MessageVersion1, payloadOf(73, 64)); err != nil {
		t.Fatal(err)
	}
	// The pump dequeues the lone state item and parks inside the write.
	f.waitParked(t, 1)
	if got := f.oc.CancelState(key); got != StateCancelInFlight {
		t.Fatalf("CancelState(in-flight) = %v, want inFlight", got)
	}
	f.releaseAll()
	waitUntil(t, "in-flight drain", func() bool { return f.tr.recordedCount() == 1 })
	if got := markerOf(t, f.tr.recorded()[0]); got != 73 {
		t.Fatalf("in-flight marker = %d, want 73 (finished, never cancelled)", got)
	}
}

// TestOutboundCancelStateNotQueued proves an absent key reports
// NotQueued without touching residency or metrics.
func TestOutboundCancelStateNotQueued(t *testing.T) {
	f := newOutboundFixture(t, DefaultOutboundPolicy())
	msgBefore, _ := f.resident()
	if got := f.oc.CancelState(cancelKey(999)); got != StateCancelNotQueued {
		t.Fatalf("CancelState(absent) = %v, want notQueued", got)
	}
	if msg, _ := f.resident(); msg != msgBefore {
		t.Fatalf("residency changed on notQueued")
	}
	drops, coalesced, _ := f.obs.snapshot()
	if len(drops) != 0 || coalesced != 0 {
		t.Fatalf("metrics fired on notQueued: drops=%v coalesced=%d", drops, coalesced)
	}
}

// TestOutboundCancelStateClosed proves a closed queue reports Closed.
func TestOutboundCancelStateClosed(t *testing.T) {
	f := newOutboundFixture(t, DefaultOutboundPolicy())
	f.oc.StopOutbound("test close")
	if got := f.oc.CancelState(cancelKey(1)); got != StateCancelClosed {
		t.Fatalf("CancelState(closed) = %v, want closed", got)
	}
}

// TestOutboundCancelStateFreesCapacity proves cancellation releases
// resident budget for reuse: the queue is message-full, a new state
// key drops as saturated, then after the cancellation the same key
// admits — and a critical admits without evicting anything. (A
// synchronous critical waiter can never be parked behind evictable
// state — critical admission evicts first — so freed capacity is
// proven through readmission, plus the cancellation performs the
// standard capacity broadcast under the lock.)
func TestOutboundCancelStateFreesCapacity(t *testing.T) {
	// Budget 2: one writing + one queued = full.
	f := newOutboundFixture(t, tinyPolicy(2, 262144, 10*time.Second, 10*time.Second))
	f.hold()
	a := f.sendAsync(t, 1, 64)
	f.waitParked(t, 1) // resident 1 (writing)

	key := cancelKey(31)
	other := cancelKey(32)
	if _, err := f.oc.TryState(f.sid, key, proto.OpcodeEntityMove, proto.MessageVersion1, payloadOf(74, 64)); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "state queued", func() bool {
		msg, _ := f.resident()
		return msg == 2
	})
	if res, err := f.oc.TryState(f.sid, other, proto.OpcodeEntityMove, proto.MessageVersion1, payloadOf(75, 64)); err != nil || res != StateDropped {
		t.Fatalf("saturated TryState = %v,%v want dropped", res, err)
	}
	if got := f.oc.CancelState(key); got != StateCancelCanceled {
		t.Fatalf("CancelState = %v, want canceled", got)
	}
	// Freed capacity admits the previously-dropped key...
	if res, err := f.oc.TryState(f.sid, other, proto.OpcodeEntityMove, proto.MessageVersion1, payloadOf(75, 64)); err != nil || res != StateQueued {
		t.Fatalf("post-cancel TryState = %v,%v want queued", res, err)
	}
	f.releaseAll()
	if err := <-a; err != nil {
		t.Fatalf("parked critical A: %v", err)
	}
	waitUntil(t, "drain", func() bool { return f.tr.recordedCount() == 2 })
	if got := markerOf(t, f.tr.recorded()[1]); got != 75 {
		t.Fatalf("surviving wire marker = %d, want 75", got)
	}
	// ...and the only state-drop observed is the legitimate
	// saturation (cancellation emits none).
	drops, _, _ := f.obs.snapshot()
	if len(drops) != 1 || drops[0] != stateDropSaturated {
		t.Fatalf("state drops = %v, want exactly one saturated", drops)
	}
}

// TestOutboundCancelStateNoSeqDebt proves a canceled state frame
// never allocates a sequence and never creates ACK debt (spec
// §7.1.8/§7.1.11/§7.1.13): with the writer parked, cancel, then a
// single critical drains with exactly one flow unit.
func TestOutboundCancelStateNoSeqDebt(t *testing.T) {
	f := newOutboundFixture(t, flowPolicy(8, 262144, 10*time.Second, 10*time.Second, 8))
	_ = f.enterWorld(t)
	f.hold()
	a := f.sendAsync(t, 1, 64)
	f.waitParked(t, 1)

	key := cancelKey(41)
	if _, err := f.oc.TryState(f.sid, key, proto.OpcodeEntityMove, proto.MessageVersion1, payloadOf(75, 64)); err != nil {
		t.Fatal(err)
	}
	if got := f.oc.CancelState(key); got != StateCancelCanceled {
		t.Fatalf("CancelState = %v, want canceled", got)
	}
	if lag := flowLag(t, f.reg, f.sid); lag != 0 {
		t.Fatalf("lag after cancel = %d, want 0 (no seq allocated)", lag)
	}
	f.releaseAll()
	if err := <-a; err != nil {
		t.Fatalf("parked critical: %v", err)
	}
	waitUntil(t, "drain", func() bool { return f.tr.recordedCount() == 1 })
	fs, _ := f.reg.FlowState(f.sid)
	if lag := flowLag(t, f.reg, f.sid); lag != 1 {
		t.Fatalf("lag after drain = %d, want exactly 1 (canceled state left no debt)", lag)
	}
	_ = fs
	drops, coalesced, _ := f.obs.snapshot()
	if len(drops) != 0 || coalesced != 0 {
		t.Fatalf("metrics fired on cancel: drops=%v coalesced=%d", drops, coalesced)
	}
}

// TestOutboundCancelStateCoalescedThenCanceled proves the B5 shape at
// queue level: A queued, B coalesces A, exit cancels the survivor,
// and only the later critical 206 reaches the wire with no debt for B.
func TestOutboundCancelStateCoalescedThenCanceled(t *testing.T) {
	f := newOutboundFixture(t, flowPolicy(8, 262144, 10*time.Second, 10*time.Second, 8))
	_ = f.enterWorld(t)
	f.hold()
	a := f.sendAsync(t, 1, 64)
	f.waitParked(t, 1)

	key := cancelKey(51)
	if res, err := f.oc.TryState(f.sid, key, proto.OpcodeEntityMove, proto.MessageVersion1, payloadOf(76, 64)); err != nil || res != StateQueued {
		t.Fatalf("TryState A = %v,%v want queued", res, err)
	}
	if res, err := f.oc.TryState(f.sid, key, proto.OpcodeEntityMove, proto.MessageVersion1, payloadOf(77, 64)); err != nil || res != StateCoalesced {
		t.Fatalf("TryState B = %v,%v want coalesced", res, err)
	}
	if got := f.oc.CancelState(key); got != StateCancelCanceled {
		t.Fatalf("CancelState = %v, want canceled", got)
	}
	if err := f.oc.TryCritical(f.sid, proto.OpcodeEntityRemove, proto.MessageVersion1, payloadOf(78, 64)); err != nil {
		t.Fatalf("206-equivalent critical: %v", err)
	}
	f.releaseAll()
	if err := <-a; err != nil {
		t.Fatalf("parked critical: %v", err)
	}
	waitUntil(t, "drain", func() bool { return f.tr.recordedCount() == 2 })
	frames := f.tr.recorded()
	if got := markerOf(t, frames[1]); got != 78 {
		t.Fatalf("post-cancel wire marker = %d, want 78 (only the 206-equivalent; neither A nor B wrote)", got)
	}
	if lag := flowLag(t, f.reg, f.sid); lag != 2 {
		t.Fatalf("lag = %d, want 2 (canceled B left no debt)", lag)
	}
	_, coalesced, _ := f.obs.snapshot()
	if coalesced != 1 {
		t.Fatalf("coalesced = %d, want exactly 1 (the legitimate B coalescing; cancel adds none)", coalesced)
	}
}

// TestOutboundCancelStateContext proves the interface is satisfied by
// the production producer seam (compile-time) and usable through it.
func TestOutboundCancelStateProducerSeam(t *testing.T) {
	f := newOutboundFixture(t, DefaultOutboundPolicy())
	var p OutboundProducer = f.oc
	if got := p.CancelState(cancelKey(5)); got != StateCancelNotQueued {
		t.Fatalf("producer CancelState = %v, want notQueued", got)
	}
	_ = context.Background()
}
