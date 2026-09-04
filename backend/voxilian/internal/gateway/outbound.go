package gateway

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dlukt/voxilian/internal/proto"
	"github.com/dlukt/voxilian/internal/session"
)

// OutboundPolicy carries the frozen per-session outbound budgets
// (spec §7.1.1, v0.3.12) as durations for the gateway layer. Production
// derives it from the validated config.OutboundConfig; tests inject
// tiny policies directly. A zero/negative field set falls back to
// DefaultOutboundPolicy so no construction path can disable a bound.
type OutboundPolicy struct {
	// MaxMessages bounds total resident outbound messages per session.
	MaxMessages int
	// MaxBytes bounds total resident complete-frame bytes per session.
	MaxBytes int
	// ReliableEnqueueTimeout bounds a synchronous reliable producer's
	// wait for queue capacity before the session fails closed as slow.
	ReliableEnqueueTimeout time.Duration
	// WriteTimeout bounds one normal queued physical WebSocket write.
	WriteTimeout time.Duration
}

// DefaultOutboundPolicy mirrors the spec §7.1.1 MVP defaults
// (1024 messages / 256 KiB / 1 s enqueue / 5 s write). Max-unacked is
// T5b concern and lives in config only.
func DefaultOutboundPolicy() OutboundPolicy {
	return OutboundPolicy{
		MaxMessages:            1024,
		MaxBytes:               262144,
		ReliableEnqueueTimeout: 1000 * time.Millisecond,
		WriteTimeout:           5000 * time.Millisecond,
	}
}

func (p OutboundPolicy) orDefaults() OutboundPolicy {
	if p.MaxMessages <= 0 || p.MaxBytes <= 0 ||
		p.ReliableEnqueueTimeout <= 0 || p.WriteTimeout <= 0 {
		return DefaultOutboundPolicy()
	}
	return p
}

// StateKey is the producer-supplied coalescing key (spec §7.1.3): a
// generic comparable {kind, id} pair. The queue never decodes payloads
// to discover keys; the canonical M3 state producer is 205 entity_move
// keyed by NetEntityID, and lane selection is always an explicit
// producer API choice — SendFunc traffic stays critical.
type StateKey struct {
	Kind uint16
	ID   uint64
}

// OutboundLane names one outbound lane for observation (spec §7.1.12:
// exactly "critical" and "state", no opcode/entity labels).
type OutboundLane string

const (
	LaneCritical OutboundLane = "critical"
	LaneState    OutboundLane = "state"
)

// Frozen slow-client session-drop reasons (spec §7.1.12). ack_lag is
// reserved for M3-T5b and intentionally absent here.
const (
	DropReasonCriticalSaturated = "critical_queue_saturated"
	DropReasonEnqueueTimeout    = "reliable_enqueue_timeout"
	DropReasonWriteTimeout      = "write_timeout"
)

// Internal state-discard reasons (state-lane observations, not
// session-drop reasons).
const (
	stateDropEvicted   = "evicted"
	stateDropSaturated = "saturated"
	stateDropClosed    = "closed"
)

// Stable outbound errors for errors.Is matching. Slow-client failures
// never introduce a new wire code (spec §7.1.6): the connection simply
// terminates and the client reconnects into a full resync.
var (
	// ErrSlowClient classifies every fail-closed backpressure outcome:
	// reliable enqueue timeout, physical write timeout, and non-blocking
	// critical saturation.
	ErrSlowClient = errors.New("slow_client")
	// ErrOutboundClosed reports a send on an outbound queue that already
	// shut down (client disconnect, forced kick, or a prior failure).
	ErrOutboundClosed = errors.New("outbound_closed")
)

// slowClientError distinguishes the internal sub-cause while staying
// one stable ErrSlowClient classification for callers.
type slowClientError struct{ reason string }

func (e *slowClientError) Error() string { return "gateway: slow client: " + e.reason }
func (e *slowClientError) Unwrap() error { return ErrSlowClient }

// StateResult reports a TryState outcome (spec §7.1.6: at least
// queued/coalesced/dropped/closed-slow).
type StateResult int

const (
	StateQueued StateResult = iota
	StateCoalesced
	StateDropped
	StateClosed
)

func (r StateResult) String() string {
	switch r {
	case StateQueued:
		return "queued"
	case StateCoalesced:
		return "coalesced"
	case StateDropped:
		return "dropped"
	case StateClosed:
		return "closed"
	default:
		return fmt.Sprintf("StateResult(%d)", int(r))
	}
}

// OutboundObserver is the narrow, no-op-by-default observation seam
// (spec §7.1.12): M3-T5b attaches the Prometheus adapter here without
// rewriting queue internals. The queue never calls an observer while
// holding its mutex, so a metric implementation can never become part
// of queue lock contention and observer bugs cannot corrupt queue
// invariants.
type OutboundObserver interface {
	// QueueDepth reports the queued (not in-flight) depth of one lane.
	QueueDepth(lane OutboundLane, messages, bytes int)
	// StateDropped reports one discarded coalescible state message.
	StateDropped(reason string)
	// StateCoalesced reports one same-key newest-wins replacement.
	StateCoalesced()
	// SessionDropped reports one fail-closed slow-client disconnect.
	SessionDropped(reason string)
}

// noopOutboundObserver is the default observer.
type noopOutboundObserver struct{}

func (noopOutboundObserver) QueueDepth(OutboundLane, int, int) {}
func (noopOutboundObserver) StateDropped(string)               {}
func (noopOutboundObserver) StateCoalesced()                   {}
func (noopOutboundObserver) SessionDropped(string)             {}

// outboundItem is one prepared queued frame (spec §7.1.8): the payload
// is encoded exactly once BEFORE admission and frozen; the S→C sequence
// and frame tick are deliberately absent — they are allocated at
// physical write time. size is the exact complete-frame byte count
// including the 12-byte header. Critical items carry a bounded
// completion channel; state items never do.
type outboundItem struct {
	sid        session.ID
	opcode     uint16
	msgVersion uint16
	size       int
	payload    []byte
	state      bool
	key        StateKey
	done       chan error
	resolved   bool
}

// prepareOutboundItem encodes the payload exactly once, validates the
// complete-frame size, and freezes the bytes into a new item. An
// encoding failure returns immediately: nothing is queued, no sequence
// is allocated, and the client is not classified slow — an internal
// encoding bug is not backpressure (spec §7.1.8).
func prepareOutboundItem(
	sid session.ID,
	opcode uint16,
	msgVersion uint16,
	encode func(*proto.Encoder) error,
	isState bool,
	key StateKey,
) (*outboundItem, error) {
	var payload []byte
	if encode != nil {
		e := proto.NewEncoder()
		if err := encode(e); err != nil {
			return nil, err
		}
		buf, err := e.Bytes()
		if err != nil {
			return nil, err
		}
		if proto.HeaderSize+len(buf) > proto.MaxFrameSize {
			return nil, fmt.Errorf(
				"proto: frame size=%d max=%d: %w",
				proto.HeaderSize+len(buf), proto.MaxFrameSize, proto.ErrFrameTooLarge)
		}
		payload = append([]byte(nil), buf...) // the item owns its bytes
	}
	it := &outboundItem{
		sid:        sid,
		opcode:     opcode,
		msgVersion: msgVersion,
		size:       proto.HeaderSize + len(payload),
		payload:    payload,
		state:      isState,
		key:        key,
	}
	if !isState {
		it.done = make(chan error, 1)
	}
	return it, nil
}

// outboundReport is an observer-notification batch captured while the
// queue mutex was held and emitted after it was released (spec §7.1.12
// observer seam: never under the lock).
type outboundReport struct {
	reportDepth     bool
	critMsgs        int
	critBytes       int
	stMsgs          int
	stBytes         int
	stateDropReason string
	stateDropCount  int
	coalescedCount  int
	sessionDrop     string
}

// outboundQueue is one session's bounded two-lane outbound queue
// (spec §7.1): a FIFO critical lane that is never silently dropped, and
// a keyed coalescible state lane where newest wins. Both lanes share
// one total resident message+byte budget that also counts the frame
// currently being physically written (spec §7.1.2). Exactly one writer
// pump goroutine drains both lanes through the low-level connection
// writer (spec §7.1.7); admission waiters are woken by a broadcast
// channel, never by a goroutine per waiter.
type outboundQueue struct {
	conn     session.Connection // low-level physical writer (ONE gate)
	registry *session.Registry  // S→C seq source at write time
	tick     TickFunc
	policy   OutboundPolicy
	observer OutboundObserver

	mu        sync.Mutex
	crit      []*outboundItem // FIFO; crit[0] is next
	critBytes int             // queued critical bytes (depth only)
	state     map[StateKey]*list.Element
	order     *list.List // *outboundItem; front = oldest
	stBytes   int        // queued state bytes (depth only)
	resMsg    int        // resident messages (queued + writing)
	resBytes  int        // resident bytes (queued + writing)
	writing   *outboundItem
	closed    bool
	closeErr  error

	capBroadcast chan struct{} // closed+replaced under mu; wakes admission waiters
	wake         chan struct{} // cap-1 pump signal
	stop         chan struct{} // closed exactly once on queue closure
	pumpDone     chan struct{}
}

// OutboundDeps wires one outbound queue over an existing low-level
// connection. Conn is the physical writer whose WriteBinary owns the
// single application-writer serialization (the T4b gate): the queue
// pump, terminal kicked writes, and deadline writes all call through
// it, so no second raw writer path exists (spec §7.1.7).
type OutboundDeps struct {
	Conn     session.Connection
	Registry *session.Registry
	Tick     TickFunc
	Policy   OutboundPolicy
	Observer OutboundObserver
}

func newOutboundQueue(deps OutboundDeps) *outboundQueue {
	observer := deps.Observer
	if observer == nil {
		observer = noopOutboundObserver{}
	}
	q := &outboundQueue{
		conn:         deps.Conn,
		registry:     deps.Registry,
		tick:         deps.Tick,
		policy:       deps.Policy.orDefaults(),
		observer:     observer,
		state:        make(map[StateKey]*list.Element),
		order:        list.New(),
		capBroadcast: make(chan struct{}),
		wake:         make(chan struct{}, 1),
		stop:         make(chan struct{}),
		pumpDone:     make(chan struct{}),
	}
	go q.run()
	return q
}

// ---------------------------------------------------------------------------
// producer APIs
// ---------------------------------------------------------------------------

// SendCritical is the synchronous reliable producer path (spec §7.1.5):
// encode → admit critical → wait until that exact frame is physically
// written or definitively failed → return. A nil return means the frame
// reached the wire, never merely "queued successfully". Admission may
// wait for resident capacity, bounded by the internal reliable enqueue
// timeout and the caller context — whichever fires first (cancellation
// before admission abandons nothing; the internal deadline classifies
// the session slow and fails it closed). Once admitted, the frame is
// authoritative: it is written or the session fails closed, never
// silently cancelled by producer context.
func (q *outboundQueue) SendCritical(
	ctx context.Context,
	sid session.ID,
	opcode uint16,
	msgVersion uint16,
	encode func(*proto.Encoder) error,
) error {
	item, err := prepareOutboundItem(sid, opcode, msgVersion, encode, false, StateKey{})
	if err != nil {
		return err
	}
	var timer *time.Timer
	var timerC <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	q.mu.Lock()
	rep := outboundReport{}
	for !q.closed {
		ok, evicted := q.admitCriticalReportLocked(item)
		rep.mergeDrops(evicted)
		if ok {
			break
		}
		broadcast := q.capBroadcast
		q.mu.Unlock()
		if timer == nil {
			timer = time.NewTimer(q.policy.ReliableEnqueueTimeout)
			timerC = timer.C
		}
		var timedOut bool
		select {
		case <-broadcast:
		case <-ctx.Done():
			// Cancelled before admission: the item was never published,
			// so returning abandons nothing and the healthy session is
			// not closed (spec §7.1.5).
			return fmt.Errorf("gateway: outbound send cancelled before admission: %w", ctx.Err())
		case <-timerC:
			timedOut = true
		}
		q.mu.Lock()
		if q.closed {
			break
		}
		if timedOut {
			admitted, evictedNow := q.admitCriticalReportLocked(item)
			rep.mergeDrops(evictedNow)
			if admitted {
				// Capacity arrived at the deadline instant: prefer
				// admission over failing a live session.
				break
			}
			slow := &slowClientError{reason: DropReasonEnqueueTimeout}
			rep.mergeDrops(q.closeLocked(slow, DropReasonEnqueueTimeout, stateDropClosed))
			q.mu.Unlock()
			q.emit(rep)
			q.forceClose()
			return slow
		}
	}
	if q.closed {
		err := q.closeErr
		q.mu.Unlock()
		return err
	}
	rep.mergeDepthLocked(q)
	q.mu.Unlock()
	q.wakePump()
	q.emit(rep)
	// Admitted: wait for THIS frame's physical write or the session's
	// definitive failure. The wait is bounded by the pump's per-write
	// timeout and queue closure, never left parked after socket close.
	return <-item.done
}

// TryCritical is the non-blocking critical producer (spec §7.1.6): it
// prepares the payload, attempts immediate admission (evicting
// coalescible state if useful), and returns without ever waiting on
// the socket or queue capacity. If critical backlog alone fills the
// budget the event is NOT dropped — the session is failed closed and a
// stable slow-client classification is returned, leaving the producing
// goroutine free to continue.
func (q *outboundQueue) TryCritical(
	sid session.ID,
	opcode uint16,
	msgVersion uint16,
	encode func(*proto.Encoder) error,
) error {
	item, err := prepareOutboundItem(sid, opcode, msgVersion, encode, false, StateKey{})
	if err != nil {
		return err
	}
	q.mu.Lock()
	if q.closed {
		err := q.closeErr
		q.mu.Unlock()
		return err
	}
	admitted, rep := q.admitCriticalReportLocked(item)
	if !admitted {
		slow := &slowClientError{reason: DropReasonCriticalSaturated}
		rep = q.closeLocked(slow, DropReasonCriticalSaturated, stateDropClosed)
		q.mu.Unlock()
		q.emit(rep)
		q.forceClose()
		return slow
	}
	rep.reportDepth = true
	rep.mergeDepthLocked(q)
	q.mu.Unlock()
	q.wakePump()
	q.emit(rep)
	return nil
}

// TryState is the non-blocking coalescible-state producer (spec
// §7.1.6): same-key replacement when the key is queued, otherwise
// immediate admission if the budget allows, otherwise the newest
// update is dropped. It never disconnects merely because one state
// update was dropped.
func (q *outboundQueue) TryState(
	sid session.ID,
	key StateKey,
	opcode uint16,
	msgVersion uint16,
	encode func(*proto.Encoder) error,
) (StateResult, error) {
	item, err := prepareOutboundItem(sid, opcode, msgVersion, encode, true, key)
	if err != nil {
		return StateDropped, err
	}
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return StateClosed, nil
	}
	rep := outboundReport{}
	var res StateResult
	if elem, ok := q.state[key]; ok {
		// Newest wins (spec §7.1.3/§7.1.4): remove the old queued value
		// first, then attempt the newest — never retain known-stale
		// state merely because it was smaller.
		q.removeStateElemLocked(elem)
		rep.coalescedCount = 1
		if q.admitStateLocked(item) {
			res = StateCoalesced
		} else {
			res = StateDropped
			rep.stateDropReason = stateDropSaturated
			rep.stateDropCount = 1
		}
	} else if q.admitStateLocked(item) {
		res = StateQueued
	} else {
		res = StateDropped
		rep.stateDropReason = stateDropSaturated
		rep.stateDropCount = 1
	}
	q.broadcastCapacityLocked() // replacement may have freed capacity
	rep.mergeDepthLocked(q)
	q.mu.Unlock()
	q.wakePump()
	q.emit(rep)
	return res, nil
}

// ---------------------------------------------------------------------------
// writer pump
// ---------------------------------------------------------------------------

// run is the session's ONE writer pump (spec §7.1.7): critical lane
// first, state lane only when no critical frame is ready. Each physical
// write uses a fresh internal write timeout — never one inherited from
// a producer or the HTTP request context (spec §7.1.5).
func (q *outboundQueue) run() {
	defer close(q.pumpDone)
	for {
		q.mu.Lock()
		if q.closed {
			q.mu.Unlock()
			return
		}
		item := q.dequeueLocked()
		if item == nil {
			q.mu.Unlock()
			select {
			case <-q.wake:
			case <-q.stop:
			}
			continue
		}
		q.mu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), q.policy.WriteTimeout)
		err := q.conn.WriteBinary(ctx, q.frameBuilder(item))
		cancel()

		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				// Physical write timeout: fail the session closed as a
				// slow client (spec §7.1.5).
				q.failSlow(DropReasonWriteTimeout)
			} else {
				q.closeWriteError(err)
			}
			return
		}
		q.mu.Lock()
		if q.closed {
			// Closed while writing (kick/teardown): the closer already
			// resolved this item; never double-complete.
			q.mu.Unlock()
			return
		}
		rep := q.completeLocked(item)
		q.mu.Unlock()
		q.emit(rep)
	}
}

// frameBuilder returns the physical-write-time builder (spec §7.1.8):
// it runs inside the connection's writer slot, allocates the next S→C
// sequence, samples the tick, and appends the already-frozen payload —
// so sequence allocation order is exactly physical wire order.
func (q *outboundQueue) frameBuilder(item *outboundItem) session.BinaryFrameBuilder {
	return func() ([]byte, error) {
		seq, err := q.registry.NextServerSeq(item.sid)
		if err != nil {
			return nil, err
		}
		return proto.EncodeFrame(proto.Header{
			Opcode:     item.opcode,
			MsgVersion: item.msgVersion,
			Seq:        seq,
			Tick:       q.tick(),
		}, func(e *proto.Encoder) error {
			e.WriteBytes(item.payload)
			return nil
		})
	}
}

// ---------------------------------------------------------------------------
// admission (mu held)
// ---------------------------------------------------------------------------

// admitCriticalReportLocked attempts immediate critical admission,
// evicting oldest queued state as necessary (spec §7.1.4). Critical
// frames and the currently-writing frame are never evicted.
func (q *outboundQueue) admitCriticalReportLocked(item *outboundItem) (bool, outboundReport) {
	rep := outboundReport{}
	for q.wouldOverflow(item) {
		elem := q.order.Front()
		if elem == nil {
			break
		}
		q.removeStateElemLocked(elem)
		rep.stateDropCount++
	}
	if rep.stateDropCount > 0 {
		rep.stateDropReason = stateDropEvicted
		q.broadcastCapacityLocked()
	}
	if q.wouldOverflow(item) {
		return false, rep
	}
	q.crit = append(q.crit, item)
	q.critBytes += item.size
	q.resMsg++
	q.resBytes += item.size
	return true, rep
}

// admitStateLocked attempts immediate state admission; it never evicts
// anything and never waits (spec §7.1.4).
func (q *outboundQueue) admitStateLocked(item *outboundItem) bool {
	if q.wouldOverflow(item) {
		return false
	}
	q.state[item.key] = q.order.PushBack(item)
	q.stBytes += item.size
	q.resMsg++
	q.resBytes += item.size
	return true
}

// wouldOverflow reports whether admitting item would exceed the shared
// total resident budget (spec §7.1.2).
func (q *outboundQueue) wouldOverflow(item *outboundItem) bool {
	return q.resMsg+1 > q.policy.MaxMessages ||
		q.resBytes+item.size > q.policy.MaxBytes
}

// removeStateElemLocked removes one queued state element and releases
// its residency.
func (q *outboundQueue) removeStateElemLocked(elem *list.Element) {
	st := elem.Value.(*outboundItem)
	q.order.Remove(elem)
	delete(q.state, st.key)
	q.stBytes -= st.size
	q.resMsg--
	q.resBytes -= st.size
}

// dequeueLocked selects the next item — critical lane first (spec
// §7.1.7) — removes it from its lane, and marks it as the
// currently-writing frame. Its message+byte residency is deliberately
// NOT released yet: a frame stays resident while a slow socket holds
// it (spec §7.1.2).
func (q *outboundQueue) dequeueLocked() *outboundItem {
	if len(q.crit) > 0 {
		it := q.crit[0]
		q.crit = q.crit[1:]
		if len(q.crit) == 0 {
			q.crit = nil
		}
		q.critBytes -= it.size
		q.writing = it
		return it
	}
	if elem := q.order.Front(); elem != nil {
		it := elem.Value.(*outboundItem)
		q.order.Remove(elem)
		delete(q.state, it.key)
		q.stBytes -= it.size
		q.writing = it
		return it
	}
	return nil
}

// completeLocked finishes one physically written item: its residency
// is released only now, its synchronous waiter (if any) completes, and
// admission waiters are woken.
func (q *outboundQueue) completeLocked(item *outboundItem) outboundReport {
	q.resMsg--
	q.resBytes -= item.size
	if q.writing == item {
		q.writing = nil
	}
	q.resolveLocked(item, nil)
	q.broadcastCapacityLocked()
	return q.depthReportLocked()
}

// resolveLocked completes an item's bounded signal exactly once.
func (q *outboundQueue) resolveLocked(item *outboundItem, err error) {
	if item.resolved {
		return
	}
	item.resolved = true
	if item.done != nil {
		item.done <- err
	}
}

// ---------------------------------------------------------------------------
// closure
// ---------------------------------------------------------------------------

// Stop shuts the queue down idempotently (spec §7.1.9): the first call
// fails every pending synchronous waiter with ErrOutboundClosed,
// discards queued state and remaining critical contents, and stops the
// writer pump; later calls are harmless. It does not force the
// transport closed — graceful teardown owns that.
func (q *outboundQueue) Stop(reason string) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	rep := q.closeLocked(
		fmt.Errorf("%w: %s", ErrOutboundClosed, reason), "", stateDropClosed)
	q.mu.Unlock()
	q.emit(rep)
}

// failSlow is the one fail-closed slow-session path (spec §7.1.5/§7.1.6):
// close the queue with a stable slow-client classification, notify the
// observer, and force-close the transport so reconnect/full resync can
// recover. Idempotent.
func (q *outboundQueue) failSlow(reason string) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	slow := &slowClientError{reason: reason}
	rep := q.closeLocked(slow, reason, stateDropClosed)
	q.mu.Unlock()
	q.emit(rep)
	q.forceClose()
}

// closeWriteError closes the queue after a non-timeout physical write
// failure (broken socket, vanished session seq): waiters receive the
// actual classified error (spec §7.1.8). Not a slow-client drop.
func (q *outboundQueue) closeWriteError(err error) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	rep := q.closeLocked(
		fmt.Errorf("gateway: outbound write failed: %w", err), "", stateDropClosed)
	q.mu.Unlock()
	q.emit(rep)
	q.forceClose()
}

// closeLocked performs the one-time internal closure: fail all queued
// critical items and the in-flight write, discard the state lane, wake
// every admission waiter, and stop the pump. Callers emit the returned
// report and force the transport closed (when appropriate) AFTER
// releasing the mutex.
func (q *outboundQueue) closeLocked(
	err error,
	sessionDropReason string,
	stateDropReason string,
) outboundReport {
	q.closed = true
	q.closeErr = err
	for _, it := range q.crit {
		q.resolveLocked(it, err)
	}
	q.crit = nil
	q.critBytes = 0
	if q.writing != nil {
		q.resolveLocked(q.writing, err)
		q.writing = nil
	}
	rep := outboundReport{
		sessionDrop:     sessionDropReason,
		stateDropReason: stateDropReason,
		stateDropCount:  q.order.Len(),
	}
	for elem := q.order.Front(); elem != nil; {
		next := elem.Next()
		q.order.Remove(elem)
		elem = next
	}
	q.state = make(map[StateKey]*list.Element)
	q.stBytes = 0
	q.resMsg = 0
	q.resBytes = 0
	q.broadcastCapacityLocked()
	close(q.stop)
	return rep
}

// forceClose closes the underlying transport without waiting for the
// writer gate (spec §7.1.5).
func (q *outboundQueue) forceClose() {
	_ = q.conn.CloseNow()
}

// ---------------------------------------------------------------------------
// observation + plumbing
// ---------------------------------------------------------------------------

// broadcastCapacityLocked wakes every admission waiter by closing the
// current broadcast channel and installing a fresh one. Callers must
// hold mu; each channel is closed exactly once under the lock.
func (q *outboundQueue) broadcastCapacityLocked() {
	close(q.capBroadcast)
	q.capBroadcast = make(chan struct{})
}

func (q *outboundQueue) wakePump() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// depthReportLocked captures both lane depths for post-unlock emission.
func (q *outboundQueue) depthReportLocked() outboundReport {
	rep := outboundReport{reportDepth: true}
	rep.mergeDepthLocked(q)
	return rep
}

func (r *outboundReport) mergeDepthLocked(q *outboundQueue) {
	r.reportDepth = true
	r.critMsgs = len(q.crit)
	r.critBytes = q.critBytes
	r.stMsgs = q.order.Len()
	r.stBytes = q.stBytes
}

// mergeDrops accumulates another report's drop/coalesce/close
// observations (used when one logical operation spans several locked
// steps, e.g. admission retries that each evicted state).
func (r *outboundReport) mergeDrops(o outboundReport) {
	if o.stateDropCount > 0 {
		r.stateDropReason = o.stateDropReason
		r.stateDropCount += o.stateDropCount
	}
	r.coalescedCount += o.coalescedCount
	if o.sessionDrop != "" {
		r.sessionDrop = o.sessionDrop
	}
}

// emit delivers a captured report to the observer outside the queue
// mutex (spec §7.1.12).
func (q *outboundQueue) emit(r outboundReport) {
	if r.sessionDrop != "" {
		q.observer.SessionDropped(r.sessionDrop)
	}
	for i := 0; i < r.stateDropCount; i++ {
		q.observer.StateDropped(r.stateDropReason)
	}
	for i := 0; i < r.coalescedCount; i++ {
		q.observer.StateCoalesced()
	}
	if r.reportDepth {
		q.observer.QueueDepth(LaneCritical, r.critMsgs, r.critBytes)
		q.observer.QueueDepth(LaneState, r.stMsgs, r.stBytes)
	}
}

// ---------------------------------------------------------------------------
// queue-capable connection
// ---------------------------------------------------------------------------

// outboundConn couples one low-level connection with its outbound
// queue. It implements session.Connection by delegation: WriteBinary
// remains the DIRECT low-level emergency serialization primitive that
// terminal control traffic (duplicate-login kicked, hard-deadline
// session_expired) uses to bypass a saturated queue (spec §7.1.9), and
// CloseNow stops the queue before force-closing the transport so a
// forced retirement can never leave the pump or waiters alive (spec
// §7.1.9). The queue pump itself writes through the same underlying
// gate, so queued, kicked, and deadline writes can never physically
// write one WebSocket concurrently (spec §7.1.7).
type outboundConn struct {
	session.Connection
	q *outboundQueue
}

// OutboundProducer is the queue-facing producer seam (spec §7.1.5–§7.1.6)
// — the API future M4 cell-owner goroutines will use. It exposes only
// session IDs, protocol encoder callbacks, and state keys; it never
// leaks a WebSocket or transport type.
type OutboundProducer interface {
	session.Connection
	// SendCritical is the synchronous reliable producer: it returns nil
	// only after the frame was physically written.
	SendCritical(ctx context.Context, sid session.ID, opcode uint16, msgVersion uint16, encode func(*proto.Encoder) error) error
	// TryCritical admits immediately or fails the session closed; it
	// never waits.
	TryCritical(sid session.ID, opcode uint16, msgVersion uint16, encode func(*proto.Encoder) error) error
	// TryState coalesces/admits/drops immediately; it never waits and
	// never disconnects on a state drop alone.
	TryState(sid session.ID, key StateKey, opcode uint16, msgVersion uint16, encode func(*proto.Encoder) error) (StateResult, error)
	// StopOutbound shuts the queue down idempotently (teardown).
	StopOutbound(reason string)
}

// newOutboundConn builds the queue-capable connection over an existing
// low-level writer and starts its writer pump. The caller owns the
// resulting connection's lifetime: every path that creates one must
// eventually Stop/CloseNow it (ServeHTTP defers Stop; forced kick
// CloseNow).
func newOutboundConn(deps OutboundDeps) *outboundConn {
	c := &outboundConn{Connection: deps.Conn}
	c.q = newOutboundQueue(OutboundDeps{
		Conn:     deps.Conn,
		Registry: deps.Registry,
		Tick:     deps.Tick,
		Policy:   deps.Policy,
		Observer: deps.Observer,
	})
	return c
}

// CloseNow implements session.Connection: stop the outbound queue (so
// pending waiters fail and the pump exits) and force-close the
// transport without waiting for the writer gate.
func (c *outboundConn) CloseNow() error {
	c.q.Stop("forced close")
	return c.Connection.CloseNow()
}

// SendCritical implements OutboundProducer.
func (c *outboundConn) SendCritical(
	ctx context.Context,
	sid session.ID,
	opcode uint16,
	msgVersion uint16,
	encode func(*proto.Encoder) error,
) error {
	return c.q.SendCritical(ctx, sid, opcode, msgVersion, encode)
}

// TryCritical implements OutboundProducer.
func (c *outboundConn) TryCritical(
	sid session.ID,
	opcode uint16,
	msgVersion uint16,
	encode func(*proto.Encoder) error,
) error {
	return c.q.TryCritical(sid, opcode, msgVersion, encode)
}

// TryState implements OutboundProducer.
func (c *outboundConn) TryState(
	sid session.ID,
	key StateKey,
	opcode uint16,
	msgVersion uint16,
	encode func(*proto.Encoder) error,
) (StateResult, error) {
	return c.q.TryState(sid, key, opcode, msgVersion, encode)
}

// StopOutbound implements OutboundProducer.
func (c *outboundConn) StopOutbound(reason string) {
	c.q.Stop(reason)
}
