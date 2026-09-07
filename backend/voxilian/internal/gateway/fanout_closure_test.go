package gateway

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dlukt/voxilian/internal/proto"
	"github.com/dlukt/voxilian/internal/session"
	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/world"
)

// ---------------------------------------------------------------------------
// Physical 205/206 wire ordering with a stalled writer (v0.3.24).
//
// The pre-correction B60 test drained the writer between every event,
// so 205 and 206 never piled up and the queued-205-after-206 hazard
// was invisible. These tests hold the physical writer with an
// unrelated frame, pile a 205 behind it, and prove the terminal 206
// either cancels the queued 205 or orders after the in-flight one —
// while the session stays healthy (a temporarily blocked writer that
// recovers before WriteTimeout is NOT a slow client).
// ---------------------------------------------------------------------------

// closureFixture bootstraps one session with a visible remote entity
// and drains the wire, returning the session and the remote entity's
// recipient-local handle H.
func closureFixture(t *testing.T) (*fanoutFixture, session.ID, sim.EntityID, uint32) {
	t.Helper()
	f := newFanoutFixture(t, 20) // stride 2
	sid := f.addSession(DefaultOutboundPolicy())
	f.activate(sid, 500, 1001, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 1001, Position: world.Vec3{X: 1}, Kind: 2, Proto: 7})
	f.source.put(EntityPresentation{EntityID: 2001, Position: world.Vec3{X: 500}, Kind: 3, Proto: 8})
	f.bootstrap(sid)
	// 2001 enters the AOI: one 204.
	f.move(2001, world.Vec3{X: 6}, 10, 35, 10, 0)
	f.drainPump()
	waitUntil(t, "204 on wire", func() bool { return f.trans[sid].recordedCount() == 2 })
	h, visible, err := f.presence.VisibleHandle(sid, 2001)
	if err != nil || !visible {
		t.Fatalf("2001 handle: h=%d visible=%v err=%v", uint32(h), visible, err)
	}
	return f, sid, 2001, uint32(h)
}

// blockerPayload is an unrelated critical frame occupying the writer.
func blockerPayload(e *proto.Encoder) error {
	e.U32(0xB10C)
	return nil
}

type wireFrame struct {
	opcode uint16
	handle uint32 // 205/206 entity handle; 0 otherwise
}

// decodeWire decodes every recorded frame's opcode (+205/206 handle).
func decodeWire(t *testing.T, frames [][]byte) []wireFrame {
	t.Helper()
	var out []wireFrame
	for _, raw := range frames {
		hdr, dec, err := proto.DecodeFrame(raw)
		if err != nil {
			t.Fatal(err)
		}
		wf := wireFrame{opcode: hdr.Opcode}
		switch hdr.Opcode {
		case proto.OpcodeEntityMove:
			m, err := proto.DecodeEntityMove(dec)
			if err != nil {
				t.Fatal(err)
			}
			wf.handle = m.Entity
		case proto.OpcodeEntityRemove:
			m, err := proto.DecodeEntityRemove(dec)
			if err != nil {
				t.Fatal(err)
			}
			wf.handle = m.Entity
		}
		out = append(out, wf)
	}
	return out
}

// TestFanoutQueued205CanceledBefore206 is the key missing proof
// (B2): with the writer occupied by an unrelated frame, a 205(H) is
// admitted into the queued state lane; while the writer is still
// occupied, visibility exits; the pump cancels the queued 205 and
// admits the critical 206; the writer recovers before WriteTimeout.
// The wire MUST contain the blocker then the 206(H) and MUST NOT
// contain the canceled 205(H). The session stays healthy.
func TestFanoutQueued205CanceledBefore206(t *testing.T) {
	f, sid, entity, h := closureFixture(t)

	hold := make(chan struct{})
	f.trans[sid].setHold(hold)
	baseBuilds := f.trans[sid].buildCount()
	if err := f.prods[sid].TryCritical(sid, proto.OpcodeSaid, proto.MessageVersion1, blockerPayload); err != nil {
		t.Fatalf("blocker: %v", err)
	}
	waitUntil(t, "blocker parked in writer", func() bool {
		return f.trans[sid].buildCount() == baseBuilds+1
	})

	// 205(H) piles up in the queued state lane behind the blocker.
	f.move(entity, world.Vec3{X: 7}, 20, 35, 12, 0)
	f.drainPump()
	if n := f.trans[sid].recordedCount(); n != 2 {
		t.Fatalf("recorded = %d, want 2 (writer still parked; 205 must stay queued)", n)
	}
	var saw205Admission bool
	for _, c := range f.prods[sid].snapshot() {
		if c.state && c.opcode == proto.OpcodeEntityMove && c.key.ID == uint64(h) {
			saw205Admission = true
		}
	}
	if !saw205Admission {
		t.Fatalf("205(H=%d) was never admitted to the state lane", h)
	}

	// Visibility exits while the writer is STILL occupied.
	f.move(entity, world.Vec3{X: 500}, 30, 35, 14, 0)
	f.drainPump()

	cancels := f.prods[sid].cancelSnapshot()
	if len(cancels) == 0 {
		t.Fatalf("no CancelState call during 206 retirement")
	}
	last := cancels[len(cancels)-1]
	if last.key != (StateKey{Kind: proto.OpcodeEntityMove, ID: uint64(h)}) || last.result != StateCancelCanceled {
		t.Fatalf("cancel = %+v, want {key 205/%d canceled}", last, h)
	}

	// Recover BEFORE WriteTimeout (default 5 s; immediate here).
	close(hold)
	waitUntil(t, "206 on wire", func() bool { return f.trans[sid].recordedCount() == 4 })

	tail := decodeWire(t, f.trans[sid].recorded()[2:])
	if len(tail) != 2 {
		t.Fatalf("post-204 wire = %+v, want [blocker 206]", tail)
	}
	if tail[0].opcode != proto.OpcodeSaid {
		t.Fatalf("post-204 wire = %+v, want blocker first", tail)
	}
	if tail[1].opcode != proto.OpcodeEntityRemove || tail[1].handle != h {
		t.Fatalf("post-204 wire = %+v, want 206(H=%d) second", tail, h)
	}
	// No 205(H) anywhere on the wire after the bootstrap epoch.
	for _, wf := range decodeWire(t, f.trans[sid].recorded()) {
		if wf.opcode == proto.OpcodeEntityMove && wf.handle == h {
			t.Fatalf("canceled 205(H=%d) physically wrote", h)
		}
	}
	// Healthy solely from this test: no slow-client drop, no
	// transport close, no state-drop/coalesce metric.
	drops, coalesced, sessionDrops := f.obs[sid].snapshot()
	if len(drops) != 0 || coalesced != 0 || len(sessionDrops) != 0 {
		t.Fatalf("unhealthy session: drops=%v coalesced=%d sessionDrops=%v", drops, coalesced, sessionDrops)
	}
	if n := f.trans[sid].closeNowCount(); n != 0 {
		t.Fatalf("transport closed %d times, want 0", n)
	}
}

// TestFanoutInFlight205CompletesBefore206 (B3): a 205(H) already
// selected and physically in-flight may NOT be cancelled; the exit
// reports InFlight, the 206 queues behind it, and the wire order is
// exactly 205(H) then 206(H) — never 206 then 205.
func TestFanoutInFlight205CompletesBefore206(t *testing.T) {
	f, sid, entity, h := closureFixture(t)

	hold := make(chan struct{})
	f.trans[sid].setHold(hold)
	baseBuilds := f.trans[sid].buildCount()
	f.move(entity, world.Vec3{X: 7}, 20, 35, 12, 0)
	f.drainPump()
	// The lone 205 is dequeued and parked inside the physical write.
	waitUntil(t, "205 in-flight", func() bool {
		return f.trans[sid].buildCount() == baseBuilds+1
	})

	f.move(entity, world.Vec3{X: 500}, 30, 35, 14, 0)
	f.drainPump()

	cancels := f.prods[sid].cancelSnapshot()
	if len(cancels) == 0 {
		t.Fatalf("no CancelState call during 206 retirement")
	}
	last := cancels[len(cancels)-1]
	if last.key != (StateKey{Kind: proto.OpcodeEntityMove, ID: uint64(h)}) || last.result != StateCancelInFlight {
		t.Fatalf("cancel = %+v, want {key 205/%d inFlight}", last, h)
	}

	close(hold)
	waitUntil(t, "205+206 on wire", func() bool { return f.trans[sid].recordedCount() == 4 })

	tail := decodeWire(t, f.trans[sid].recorded()[2:])
	if len(tail) != 2 {
		t.Fatalf("post-204 wire = %+v, want [205 206]", tail)
	}
	if tail[0].opcode != proto.OpcodeEntityMove || tail[0].handle != h {
		t.Fatalf("post-204 wire = %+v, want 205(H=%d) first", tail, h)
	}
	if tail[1].opcode != proto.OpcodeEntityRemove || tail[1].handle != h {
		t.Fatalf("post-204 wire = %+v, want 206(H=%d) second", tail, h)
	}
	drops, _, sessionDrops := f.obs[sid].snapshot()
	if len(drops) != 0 || len(sessionDrops) != 0 {
		t.Fatalf("unhealthy session: drops=%v sessionDrops=%v", drops, sessionDrops)
	}
}

// TestFanoutCoalesced205CanceledBefore206 (B5): 205-A(H) queued,
// 205-B(H) coalesces A, visibility exits, the survivor is canceled,
// and only the 206 reaches the wire — with no sequence/ACK debt for
// the canceled B.
func TestFanoutCoalesced205CanceledBefore206(t *testing.T) {
	f, sid, entity, h := closureFixture(t)

	hold := make(chan struct{})
	f.trans[sid].setHold(hold)
	baseBuilds := f.trans[sid].buildCount()
	if err := f.prods[sid].TryCritical(sid, proto.OpcodeSaid, proto.MessageVersion1, blockerPayload); err != nil {
		t.Fatalf("blocker: %v", err)
	}
	waitUntil(t, "blocker parked", func() bool {
		return f.trans[sid].buildCount() == baseBuilds+1
	})

	// Two eligible updates for the same epoch: A queues, B coalesces.
	f.move(entity, world.Vec3{X: 7}, 20, 35, 12, 0)
	f.move(entity, world.Vec3{X: 8}, 21, 35, 14, 0)
	f.drainPump()
	if n := f.trans[sid].recordedCount(); n != 2 {
		t.Fatalf("recorded = %d, want 2 (both 205s stay queued behind the blocker)", n)
	}

	f.move(entity, world.Vec3{X: 500}, 30, 35, 16, 0)
	f.drainPump()

	cancels := f.prods[sid].cancelSnapshot()
	if len(cancels) == 0 {
		t.Fatalf("no CancelState call during 206 retirement")
	}
	last := cancels[len(cancels)-1]
	if last.result != StateCancelCanceled || last.key.ID != uint64(h) {
		t.Fatalf("cancel = %+v, want canceled 205/%d", last, h)
	}

	close(hold)
	waitUntil(t, "206 on wire", func() bool { return f.trans[sid].recordedCount() == 4 })
	tail := decodeWire(t, f.trans[sid].recorded()[2:])
	if len(tail) != 2 || tail[0].opcode != proto.OpcodeSaid ||
		tail[1].opcode != proto.OpcodeEntityRemove || tail[1].handle != h {
		t.Fatalf("post-204 wire = %+v, want [blocker 206(H)] (neither A nor B)", tail)
	}
}

// ---------------------------------------------------------------------------
// Fanout control admission vs Close (v0.3.24)
// ---------------------------------------------------------------------------

// TestFanoutAdmittedControlExactResultOnClose (B6): an IN-FLIGHT
// control dispatch (admitted, already past the pump's close check,
// deterministically blocked inside PresentationSource) MUST NOT
// return merely because Close began. After the release the caller
// receives the dispatch's exact result (nil), and then Close
// completes.
func TestFanoutAdmittedControlExactResultOnClose(t *testing.T) {
	f := newFanoutFixture(t, 20)
	sid := f.addSession(DefaultOutboundPolicy())
	f.activate(sid, 101, 1001, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 1001, Position: world.Vec3{X: 1}, Yaw: 10, Speed: 35, Kind: 2, Proto: 7})
	f.source.block = make(chan struct{})
	f.source.entered = make(chan sim.EntityID, 2048)

	waiter := make(chan error, 1)
	go func() {
		waiter <- f.fanout.BootstrapSession(context.Background(), sid)
	}()
	select {
	case <-f.source.entered:
	case <-time.After(10 * time.Second):
		t.Fatalf("dispatch never entered (admission did not happen)")
	}
	// Dispatch is in-flight; start Close concurrently.
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		f.fanout.Close()
	}()
	// The admitted caller must NOT observe ErrFanoutClosed merely
	// because done closed.
	select {
	case err := <-waiter:
		t.Fatalf("admitted control returned early during Close: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(f.source.block)
	select {
	case err := <-waiter:
		if err != nil {
			t.Fatalf("admitted bootstrap result = %v, want nil (exact dispatch result)", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("admitted control hung after dispatch release")
	}
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatalf("Close hung after dispatch release")
	}
	if err := f.fanout.BootstrapSession(context.Background(), sid); !errors.Is(err, ErrFanoutClosed) {
		t.Fatalf("post-close bootstrap = %v, want ErrFanoutClosed", err)
	}
}

// TestFanoutControlCloseRace (B8): many Bootstrap/RemovePresence
// calls raced against Close. Every call ends as exactly one of:
// rejected before publication (ErrFanoutClosed), or admitted and
// completed with its exact dispatch result. No hung waiter, no
// stranded event, no ambiguous early ErrFanoutClosed after a
// successful dispatch.
func TestFanoutControlCloseRace(t *testing.T) {
	f := newFanoutFixture(t, 20)
	const sessions = 8
	sids := make([]session.ID, 0, sessions)
	for i := 0; i < sessions; i++ {
		sid := f.addSession(DefaultOutboundPolicy())
		f.activate(sid, int64(101+i), sim.EntityID(1001+i), world.CellCoord{})
		f.source.put(EntityPresentation{
			EntityID: sim.EntityID(1001 + i),
			Position: world.Vec3{X: 1}, Kind: 2, Proto: 7,
		})
		sids = append(sids, sid)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	type outcome struct {
		err error
	}
	results := make(chan outcome, sessions*2*25)
	for _, sid := range sids {
		for g := 0; g < 2; g++ {
			wg.Add(1)
			go func(sid session.ID, bootstrap bool) {
				defer wg.Done()
				<-start
				for i := 0; i < 25; i++ {
					var err error
					if bootstrap {
						err = f.fanout.BootstrapSession(context.Background(), sid)
					} else {
						err = f.fanout.RemovePresence(context.Background(), sid, sim.EntityID(1001))
					}
					// Valid sessions: dispatch succeeds (nil) or the
					// call loses to Close/drain (ErrFanoutClosed).
					// Anything else — or a hang — fails the test.
					if err != nil && !errors.Is(err, ErrFanoutClosed) {
						results <- outcome{err: err}
						return
					}
					results <- outcome{}
				}
			}(sid, g == 0)
		}
	}
	close(start)
	done := make(chan struct{})
	go func() {
		defer close(done)
		wg.Wait()
	}()
	// Close while admissions are in flight.
	f.fanout.Close()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("control callers hung across Close")
	}
	close(results)
	for o := range results {
		if o.err != nil {
			t.Fatalf("control result = %v, want nil or ErrFanoutClosed", o.err)
		}
	}
	// After Close: no event remains processable in the channel.
	if n := len(f.fanout.events); n != 0 {
		t.Fatalf("events stranded after Close: %d", n)
	}
}

// TestFanoutMovementCloseRace (B9): many OnMovement calls raced with
// Close. OnMovement never blocks the sim producer, never panics,
// Close completes, nothing remains processable, and post-Close
// movements count as closed drops.
func TestFanoutMovementCloseRace(t *testing.T) {
	f := newFanoutFixture(t, 20)
	sid := f.addSession(DefaultOutboundPolicy())
	f.activate(sid, 101, 1001, world.CellCoord{})
	f.source.put(EntityPresentation{EntityID: 1001, Position: world.Vec3{X: 1}, Kind: 2, Proto: 7})
	f.bootstrap(sid)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			<-start
			for i := 0; i < 200; i++ {
				f.fanout.OnMovement(sim.MovementUpdate{
					EntityID: 1001, Position: world.Vec3{X: 1}, Tick: uint32(i),
				})
			}
		}(g)
	}
	close(start)
	f.fanout.Close()
	waitDone := make(chan struct{})
	go func() {
		defer close(waitDone)
		wg.Wait()
	}()
	select {
	case <-waitDone:
	case <-time.After(30 * time.Second):
		t.Fatalf("OnMovement producers blocked across Close")
	}
	if n := len(f.fanout.events); n != 0 {
		t.Fatalf("events stranded after Close: %d", n)
	}
	before := f.fanout.DroppedMovementUpdates()
	for i := 0; i < 10; i++ {
		f.fanout.OnMovement(sim.MovementUpdate{EntityID: 1001})
	}
	if got := f.fanout.DroppedMovementUpdates(); got != before+10 {
		t.Fatalf("post-close closed-drops = %d, want +10", got-before)
	}
}
