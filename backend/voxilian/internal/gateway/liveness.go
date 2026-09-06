package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/dlukt/voxilian/internal/session"
)

// Frozen transport-liveness constants (spec §7.4.7). None is user
// config; all are internal policy.
const (
	// HeartbeatPingInterval is the preserved Ping/Pong cadence: 15 s.
	HeartbeatPingInterval = 15 * time.Second
	// HeartbeatPingTimeout bounds one Ping's wait for the matching
	// Pong: 15 s.
	HeartbeatPingTimeout = 15 * time.Second
	// StaleSweepInterval is the stale-presence sweep cadence: 30 s.
	StaleSweepInterval = 30 * time.Second
	// DisconnectCleanupTimeout bounds raw-disconnect/stale world
	// cleanup attempts on a fresh background context: 5 s.
	DisconnectCleanupTimeout = 5 * time.Second
)

// Pinger is the gateway-local WebSocket liveness seam (spec §7.4.7):
// one Ping sends a WebSocket control frame and waits for the matching
// Pong while the normal reader processes control frames. It is
// deliberately NOT part of session.Connection so transport-neutral
// session fakes never need WebSocket control-frame behavior.
type Pinger interface {
	Ping(ctx context.Context) error
}

// SessionReaper owns unexpected-transport-disconnect cleanup, stale
// heartbeat cleanup, and retry of detached world sessions (spec
// §7.4.7). It reuses the SAME WorldExit composition the
// CharacterHandler and EnterWorldHandler takeover use — there is no
// second cleanup implementation — and never handles opcode 126.
type SessionReaper struct {
	registry  *session.Registry
	presence  *PresenceRegistry
	worldExit WorldExit
}

// NewSessionReaper wires a SessionReaper. Every dependency is
// required: registry, presence, and the shared flush-first WorldExit
// composition.
func NewSessionReaper(registry *session.Registry, presence *PresenceRegistry, worldExit WorldExit) (*SessionReaper, error) {
	if registry == nil {
		return nil, errors.New("gateway: session registry is required")
	}
	if presence == nil {
		return nil, errors.New("gateway: presence registry is required")
	}
	if worldExit == nil {
		return nil, errors.New("gateway: world exit is required")
	}
	return &SessionReaper{registry: registry, presence: presence, worldExit: worldExit}, nil
}

// Reap performs idempotent disconnect/stale cleanup for sid (spec
// §7.4.7). Concurrent causes (read error, CloseNow, ACK lag, heartbeat
// timeout, hard auth expiry, kicked retirement, stale sweep) are safe:
// an authenticated session is guarded and re-read under its account
// lifecycle serialization, so no destructive WorldExit can race a
// leave/takeover/delete/enter and no double teardown follows a
// successful removal. The caller has already stopped the outbound
// queue, cancelled the authorization timer, and stopped the ping loop.
//
// No active world Presence (CONNECTED, AUTHENTICATED, or a
// CHARACTER_SELECTED session whose enter already rolled back) needs
// only Registry.Remove. A still-owned IN_WORLD + character + active
// Presence runs the flush-first WorldExit on a NEW bounded background
// context — never a dead request context — then CompleteLeaveWorld and
// Registry.Remove.
//
// A returned error means cleanup was intentionally NOT completed: the
// registry entry, Presence, sim entity, and fanout state are retained
// as world-cleanup-pending for the next sweep or a legitimate
// same-account takeover. Never Registry.Remove on world-cleanup
// failure.
func (re *SessionReaper) Reap(sid session.ID, cause string) error {
	snap, ok := re.registry.Get(sid)
	if !ok {
		return nil
	}
	if snap.AccountID > 0 {
		unlock := re.registry.LockAccount(snap.AccountID)
		defer unlock()
		// Authoritative re-read under the guard: never trust the
		// pre-lock snapshot.
		snap, ok = re.registry.Get(sid)
		if !ok {
			return nil
		}
	}
	if _, perr := re.presence.Snapshot(sid); perr != nil {
		// No active world Presence: plain registry removal suffices.
		re.registry.Remove(sid)
		return nil
	}
	if snap.State != session.StateInWorld || !snap.HasCharacter || snap.CharacterID == 0 {
		// Presence without a fully-owned IN_WORLD binding cannot exist
		// through the staged enter path (activation follows
		// CompleteEnterWorld): a loud internal invariant. Fail closed
		// by deactivating the orphaned Presence and removing the
		// session rather than wedging retry loops.
		slog.Error("gateway: reap invariant: presence without owned world binding",
			"session", uint64(sid), "state", snap.State.String(),
			"cause", cause)
		_, _ = re.presence.Deactivate(sid)
		re.registry.Remove(sid)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), DisconnectCleanupTimeout)
	defer cancel()
	if err := re.worldExit.ExitWorld(ctx, sid, snap.AccountID, snap.CharacterID); err != nil {
		// World state is NOT torn down: retain the registry entry,
		// Presence, entity, and fanout for the next sweep/takeover.
		return fmt.Errorf("gateway: reap world exit (%s): %w", cause, err)
	}
	if err := re.registry.CompleteLeaveWorld(sid, snap.CharacterID); err != nil {
		if !errors.Is(err, session.ErrNotFound) {
			// World state is already gone; a registry entry claiming
			// IN_WORLD would be the worse invariant (spec §7.4.7).
			slog.Error("gateway: reap complete-leave invariant",
				"session", uint64(sid), "cause", cause, "err", err)
		}
	}
	re.registry.Remove(sid)
	return nil
}

// Ticker is the deterministic timer seam (spec §7.4.7): production
// wraps time.Ticker; tests pulse manually with no sleeps.
type Ticker interface {
	// Fire yields each period elapsed.
	Fire() <-chan time.Time
	// Stop releases the timer.
	Stop()
}

// TickerFactory builds one Ticker for a period.
type TickerFactory func(period time.Duration) Ticker

// prodTicker adapts time.Ticker.
type prodTicker struct {
	t *time.Ticker
}

func (p *prodTicker) Fire() <-chan time.Time { return p.t.C }
func (p *prodTicker) Stop()                  { p.t.Stop() }

// ProdTickerFactory is the production TickerFactory.
func ProdTickerFactory(period time.Duration) Ticker {
	return &prodTicker{t: time.NewTicker(period)}
}

// TransportLiveness composes PresenceRegistry, SessionReaper, NowFunc,
// and a ticker factory into the server-level liveness runtime (spec
// §7.4.7): exactly one 30 s stale-sweep goroutine plus at most one
// 15 s heartbeat ping loop per accepted WebSocket. Construct with
// NewTransportLiveness; Start the sweep; Close is idempotent.
type TransportLiveness struct {
	presence  *PresenceRegistry
	registry  *session.Registry
	reaper    *SessionReaper
	now       NowFunc
	newTicker TickerFactory

	mu        sync.Mutex
	stopSweep chan struct{}
	closed    bool
	wg        sync.WaitGroup
}

// NewTransportLiveness wires a TransportLiveness. Presence, registry,
// reaper, clock, and ticker factory are all required. It starts
// nothing; call Start for the sweep loop.
func NewTransportLiveness(presence *PresenceRegistry, registry *session.Registry, reaper *SessionReaper, now NowFunc, newTicker TickerFactory) (*TransportLiveness, error) {
	if presence == nil {
		return nil, errors.New("gateway: presence registry is required")
	}
	if registry == nil {
		return nil, errors.New("gateway: session registry is required")
	}
	if reaper == nil {
		return nil, errors.New("gateway: session reaper is required")
	}
	if now == nil {
		return nil, errors.New("gateway: clock is required")
	}
	if newTicker == nil {
		return nil, errors.New("gateway: ticker factory is required")
	}
	return &TransportLiveness{
		presence:  presence,
		registry:  registry,
		reaper:    reaper,
		now:       now,
		newTicker: newTicker,
	}, nil
}

// Reaper exposes the composed SessionReaper (the Server teardown path
// delegates to it).
func (l *TransportLiveness) Reaper() *SessionReaper { return l.reaper }

// Start starts the single 30 s stale-sweep goroutine. Idempotent; a
// closed liveness never restarts.
func (l *TransportLiveness) Start() {
	l.mu.Lock()
	if l.closed || l.stopSweep != nil {
		l.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	l.stopSweep = stop
	l.mu.Unlock()
	l.wg.Add(1)
	go l.sweepLoop(stop)
}

// Close stops the sweep goroutine and waits for its exit. Idempotent.
// Per-connection ping loops are owned by their connections' teardown,
// not by Close.
func (l *TransportLiveness) Close() {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.closed = true
	stop := l.stopSweep
	l.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	l.wg.Wait()
}

func (l *TransportLiveness) sweepLoop(stop <-chan struct{}) {
	defer l.wg.Done()
	ticker := l.newTicker(StaleSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.Fire():
			l.Sweep()
		}
	}
}

// Sweep runs one stale sweep (spec §7.4.7) over the sorted
// StaleSessions(Now()) result: each stale session gets CloseNow on a
// live connection and reaper cleanup in sorted order. A WorldExit
// failure retains the stale world session for the NEXT sweep. It
// returns stale presences with no corresponding registry entry — an
// internal invariant that is already logged loudly, never silently
// accepted.
func (l *TransportLiveness) Sweep() []session.ID {
	var orphans []session.ID
	for _, sid := range l.presence.StaleSessions(l.now()) {
		snap, ok := l.registry.Get(sid)
		if !ok {
			orphans = append(orphans, sid)
			slog.Error("gateway: stale presence without session entry",
				"session", uint64(sid))
			continue
		}
		if snap.Conn != nil {
			_ = snap.Conn.CloseNow()
		}
		if err := l.reaper.Reap(sid, "stale_sweep"); err != nil {
			slog.Warn("gateway: stale world cleanup retained for retry",
				"session", uint64(sid), "err", err)
		}
	}
	return orphans
}

// StartPinger starts ONE heartbeat ping loop for an accepted
// connection (spec §7.4.7): each 15 s pulse pings only when the
// session has an active Presence (the pre-world baseline never pings).
// A successful Pong touches the heartbeat at the injected Now;
// application frames never count. A ping failure CloseNows the
// transport without touching the heartbeat — authoritative world
// cleanup belongs to the reaper. The returned stop ends the loop; it
// is idempotent and safe to call from teardown.
func (l *TransportLiveness) StartPinger(sid session.ID, p Pinger) (stop func()) {
	stopCh := make(chan struct{})
	done := make(chan struct{})
	ticker := l.newTicker(HeartbeatPingInterval)
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		defer close(done)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.Fire():
				l.pingOnce(sid, p)
			}
		}
	}()
	return sync.OnceFunc(func() {
		close(stopCh)
		<-done
	})
}

func (l *TransportLiveness) pingOnce(sid session.ID, p Pinger) {
	if _, err := l.presence.Snapshot(sid); err != nil {
		// No active Presence: skip the ping entirely.
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), HeartbeatPingTimeout)
	defer cancel()
	if err := p.Ping(ctx); err != nil {
		// No heartbeat touch; the reaper owns authoritative world
		// cleanup after the read loop unblocks.
		if snap, ok := l.registry.Get(sid); ok && snap.Conn != nil {
			_ = snap.Conn.CloseNow()
		}
		return
	}
	// TouchHeartbeat after a concurrent deactivation reports
	// ErrPresenceNotFound: normal lifecycle convergence, never a
	// recreate.
	_ = l.presence.TouchHeartbeat(sid, l.now())
}
