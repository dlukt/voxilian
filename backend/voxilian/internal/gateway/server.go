package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/dlukt/voxilian/internal/auth"
	"github.com/dlukt/voxilian/internal/proto"
	"github.com/dlukt/voxilian/internal/session"
)

// ReauthGrace is the canonical hard authorization grace period
// (spec §6.2.2): authorizationDeadline = TokenExp + ReauthGrace. At or
// after the deadline no new opcode is dispatched, a best-effort 202
// session_expired goes out, and the connection closes. Defined once,
// here, where the connection deadline is owned.
const ReauthGrace = 90 * time.Second

// errAuthDeadline ends a connection whose hard authorization deadline
// has passed. It is internal: the client already received (best-effort)
// a 202 session_expired.
var errAuthDeadline = errors.New("gateway: authorization deadline exceeded")

// AccountProvisioner maps a validated subject to its durable account.
// *store.PGStore satisfies this structurally; the gateway never imports
// the store package, pgx, or sqlc.
type AccountProvisioner interface {
	EnsureAccount(ctx context.Context, sub string, email *string) (int64, error)
}

// AccountProvisionerFunc adapts a plain function to an
// AccountProvisioner (nil-guard defaults, tests).
type AccountProvisionerFunc func(ctx context.Context, sub string, email *string) (int64, error)

// EnsureAccount implements AccountProvisioner.
func (f AccountProvisionerFunc) EnsureAccount(ctx context.Context, sub string, email *string) (int64, error) {
	return f(ctx, sub, email)
}

// NowFunc supplies wall-clock authorization time. Simulation tick and
// authorization time are different concepts: TickFunc never drives JWT
// expiry or deadline checks.
type NowFunc func() time.Time

// CancelFunc releases a scheduled callback.
type CancelFunc func()

// ScheduleFunc arranges fn to run at wall-clock time at and returns its
// cancellation. Production uses time.AfterFunc; tests use a manual
// deterministic scheduler.
type ScheduleFunc func(at time.Time, fn func()) CancelFunc

// TickFunc supplies the S→C frame tick. T1 owns no sim, so the tick
// source is injected; tests use a deterministic constant and M4 will
// supply the real sim tick.
type TickFunc func() uint32

// WelcomeFunc supplies the 200 welcome payload for a successful hello
// without importing world/sim/config into the gateway.
type WelcomeFunc func(ctx context.Context) proto.Welcome

// SendFunc is the narrow responder handed to MessageHandlers: it
// allocates the session's next S→C sequence number, frames the payload
// with proto.EncodeFrame, and writes one binary message. Handlers must
// not invent frame sequencing themselves.
type SendFunc func(opcode uint16, msgVersion uint16, encode func(*proto.Encoder) error) error

// MessageHandler owns the semantics of recognized C→S opcodes other
// than hello/reauth. T1 owns transport plus the lifecycle gate; later
// tasks (character CRUD, enter_world, gameplay) implement this seam.
type MessageHandler interface {
	Handle(
		ctx context.Context,
		sid session.ID,
		header proto.Header,
		payload *proto.Decoder,
		send SendFunc,
	) error
}

// ClientError asks the gateway to emit a 202 error reply with the given
// machine-readable code and keep the connection alive, without the
// handler importing WebSocket details.
type ClientError struct {
	Code    uint16
	Message string
}

// Error implements error.
func (e *ClientError) Error() string {
	return fmt.Sprintf("client error %d: %s", e.Code, e.Message)
}

// wsConnection adapts *websocket.Conn to the session-level connection
// handle the registry retains for later forced takeover/kick.
type wsConnection struct {
	conn *websocket.Conn
}

// Close implements session.Connection.
func (w *wsConnection) Close(reason string) error {
	return w.conn.Close(websocket.StatusNormalClosure, reason)
}

// ServerDeps wires a gateway. Registry is required; every nil seam gets
// a safe default (failing validator/provisioner, zero welcome/tick,
// wall clock, AfterFunc scheduling, no-op handler) so tests only set
// what they exercise. No DI framework, no config wiring (later tasks).
type ServerDeps struct {
	Registry  *session.Registry
	Validator auth.Validator
	Accounts  AccountProvisioner

	Welcome  WelcomeFunc
	Tick     TickFunc
	Now      NowFunc
	Schedule ScheduleFunc

	Handler MessageHandler
}

// Server is the WebSocket edge: it accepts connections, registers one
// session per connection, validates access JWTs, provisions accounts,
// enforces the lifecycle permission gate plus the hard authorization
// deadline, and routes allowed messages to the handler seam. Construct
// with NewServer; tests instantiate it directly over httptest (no
// cmd/serve wiring in T2).
type Server struct {
	Registry  *session.Registry
	validator auth.Validator
	accounts  AccountProvisioner
	welcome   WelcomeFunc
	tick      TickFunc
	now       NowFunc
	schedule  ScheduleFunc
	handler   MessageHandler
}

// NewServer builds a gateway from deps.
func NewServer(deps ServerDeps) *Server {
	s := &Server{
		Registry:  deps.Registry,
		validator: deps.Validator,
		accounts:  deps.Accounts,
		welcome:   deps.Welcome,
		tick:      deps.Tick,
		now:       deps.Now,
		schedule:  deps.Schedule,
		handler:   deps.Handler,
	}
	if s.validator == nil {
		s.validator = auth.ValidatorFunc(func(context.Context, string) (auth.Identity, error) {
			return auth.Identity{}, errors.New("gateway: validator not configured")
		})
	}
	if s.accounts == nil {
		s.accounts = AccountProvisionerFunc(func(context.Context, string, *string) (int64, error) {
			return 0, errors.New("gateway: account provisioner not configured")
		})
	}
	if s.welcome == nil {
		s.welcome = func(context.Context) proto.Welcome { return proto.Welcome{} }
	}
	if s.tick == nil {
		s.tick = func() uint32 { return 0 }
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.schedule == nil {
		s.schedule = prodSchedule
	}
	return s
}

// prodSchedule is the production ScheduleFunc.
func prodSchedule(at time.Time, fn func()) CancelFunc {
	d := time.Until(at)
	if d < 0 {
		d = 0
	}
	t := time.AfterFunc(d, fn)
	return func() { t.Stop() }
}

// connAuth is the per-connection authorization-deadline controller. The
// scheduled callback only ever terminates the connection whose
// generation and TokenExp it still matches: a successful reauth
// supersedes the old timer, and a late-firing stale callback can never
// disconnect the reauthenticated session.
type connAuth struct {
	mu       sync.Mutex
	wmu      sync.Mutex
	cancel   CancelFunc
	gen      uint64
	deadline time.Time
	done     bool
}

// ServeHTTP accepts one WebSocket connection and serves it until it
// terminates, then cancels its deadline timer and removes its session
// from the registry (idempotent cleanup of sub/character indexes and
// connection references).
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		slog.Warn("gateway: websocket accept failed", "err", err)
		return
	}
	defer func() {
		_ = conn.Close(websocket.StatusNormalClosure, "closing")
	}()
	// Bounded-memory transport rule (spec v0.3.8 §6): the application
	// frame ceiling is exactly 64 KiB. Oversized messages are terminated
	// by the transport with status 1009 and never parsed.
	conn.SetReadLimit(proto.MaxFrameSize)

	ctx := r.Context()
	sid := s.Registry.Create(&wsConnection{conn: conn})
	ca := &connAuth{}
	defer func() {
		s.cancelDeadline(ca)
		s.Registry.Remove(sid)
	}()

	for {
		msgType, data, err := conn.Read(ctx)
		if err != nil {
			// Oversized messages die here by transport rule (status
			// 1009, already applied by the library): never parsed,
			// never answered with a 202.
			if errors.Is(err, websocket.ErrMessageTooBig) {
				slog.Warn("gateway: message too big, closing",
					"session", uint64(sid),
					"status", int(websocket.CloseStatus(err)))
			}
			return
		}
		if msgType != websocket.MessageBinary {
			// Text (or any non-binary) application message: protocol
			// error on the first offense, session continues.
			if err := s.sendError(ctx, ca, conn, sid,
				proto.ErrorCodeProtocol, "text websocket messages are not supported"); err != nil {
				return
			}
			continue
		}
		if err := s.handleBinary(ctx, ca, conn, sid, data); err != nil {
			return
		}
	}
}

// handleBinary routes one binary message that passed the transport read
// limit. A non-nil return ends the connection; handled protocol
// violations reply 202 and return nil to keep the session alive.
func (s *Server) handleBinary(
	ctx context.Context,
	ca *connAuth,
	conn *websocket.Conn,
	sid session.ID,
	data []byte,
) error {
	header, payload, err := proto.DecodeFrame(data)
	if err != nil {
		msg := "malformed binary frame"
		if errors.Is(err, proto.ErrTruncated) {
			msg = "binary frame is shorter than protocol header"
		} else if errors.Is(err, proto.ErrFrameTooLarge) {
			msg = "binary frame exceeds the 64 KiB ceiling"
		}
		return s.replyError(ctx, ca, conn, sid, proto.ErrorCodeProtocol, msg)
	}
	// Unknown opcodes and client-sent S→C opcodes are protocol errors,
	// never bad_state, and never disconnect on the first offense.
	if !session.IsClientOpcode(header.Opcode) {
		return s.replyError(ctx, ca, conn, sid,
			proto.ErrorCodeProtocol,
			fmt.Sprintf("unsupported client opcode %d", header.Opcode))
	}
	snap, ok := s.Registry.Get(sid)
	if !ok {
		return errors.New("gateway: session vanished mid-connection")
	}
	// Incoming C→S seq/tick are opaque: no ordering, wrap, or
	// future-tick enforcement here (M3-T5/M4 own those).
	if !session.Allowed(snap.State, header.Opcode) {
		return s.replyError(ctx, ca, conn, sid,
			proto.ErrorCodeBadState,
			fmt.Sprintf("opcode %d is not allowed in %s", header.Opcode, snap.State))
	}
	// Hard authorization deadline, enforced synchronously before any
	// dispatch — handler, gameplay, or reauth. A CONNECTED session has
	// no established token and is exempt.
	if snap.State != session.StateConnected &&
		!s.now().Before(snap.TokenExp.Add(ReauthGrace)) {
		_ = s.sendError(ctx, ca, conn, sid,
			proto.ErrorCodeSessionExpired, "reauth deadline exceeded")
		return errAuthDeadline
	}
	switch header.Opcode {
	case proto.OpcodeHello:
		return s.finishStep(ctx, ca, conn, sid, s.handleHello(ctx, ca, conn, sid, payload))
	case proto.OpcodeReauth:
		return s.finishStep(ctx, ca, conn, sid, s.handleReauth(ctx, ca, conn, sid, snap, payload))
	default:
		return s.dispatch(ctx, ca, conn, sid, header, payload)
	}
}

// replyError sends a 202 and keeps the session alive; a failed write
// ends the connection.
func (s *Server) replyError(
	ctx context.Context,
	ca *connAuth,
	conn *websocket.Conn,
	sid session.ID,
	code uint16,
	msg string,
) error {
	if err := s.sendError(ctx, ca, conn, sid, code, msg); err != nil {
		return err
	}
	return nil
}

// finishStep maps a hello/reauth step result onto the wire: nil means
// the step already replied, a *ClientError becomes a 202, anything else
// is an internal failure that ends the connection.
func (s *Server) finishStep(
	ctx context.Context,
	ca *connAuth,
	conn *websocket.Conn,
	sid session.ID,
	err error,
) error {
	if err == nil {
		return nil
	}
	var cerr *ClientError
	if errors.As(err, &cerr) {
		return s.replyError(ctx, ca, conn, sid, cerr.Code, cerr.Message)
	}
	return err
}

// handleHello serves opcode 100: decode, validate the access JWT,
// auto-provision the account, atomically establish identity
// (CONNECTED → AUTHENTICATED), arm the authorization deadline, and
// reply 200. JWT failure yields session_expired; provisioning failure
// yields retry; both leave the session CONNECTED with no partial
// authentication. Every reply uses MessageVersion1 and the next S→C
// session sequence.
func (s *Server) handleHello(
	ctx context.Context,
	ca *connAuth,
	conn *websocket.Conn,
	sid session.ID,
	payload *proto.Decoder,
) error {
	hello, err := proto.DecodeHello(payload)
	if err != nil {
		return &ClientError{Code: proto.ErrorCodeProtocol, Message: "malformed hello payload"}
	}
	id, err := s.validator.Validate(ctx, hello.AccessToken)
	if err != nil {
		return &ClientError{Code: proto.ErrorCodeSessionExpired, Message: "hello authentication failed"}
	}
	var email *string
	if id.HasEmail {
		email = &id.Email
	}
	accountID, err := s.accounts.EnsureAccount(ctx, id.Sub, email)
	if err != nil {
		return &ClientError{Code: proto.ErrorCodeRetry, Message: "account provisioning unavailable"}
	}
	if err := s.Registry.Authenticate(sid, id.Sub, accountID, id.ExpiresAt); err != nil {
		if errors.Is(err, session.ErrBadState) || errors.Is(err, session.ErrNotFound) {
			return &ClientError{
				Code:    proto.ErrorCodeBadState,
				Message: "hello is not allowed in current session state",
			}
		}
		return err
	}
	s.armDeadline(ca, conn, sid, id.ExpiresAt)
	welcome := s.welcome(ctx)
	return s.send(ctx, ca, conn, sid, proto.OpcodeWelcome, proto.MessageVersion1, func(e *proto.Encoder) error {
		welcome.Encode(e)
		return nil
	})
}

// handleReauth serves opcode 101 over an established session: decode,
// validate the new JWT (it must be currently valid — grace never
// extends token validity), require identical identity against the
// existing session, update only TokenExp, replace the authorization
// deadline, and reply 201. The provisioner is never consulted: reauth
// keeps working during a PG outage. Any failure leaves identity,
// TokenExp, and remaining grace intact.
func (s *Server) handleReauth(
	ctx context.Context,
	ca *connAuth,
	conn *websocket.Conn,
	sid session.ID,
	snap session.Snapshot,
	payload *proto.Decoder,
) error {
	reauth, err := proto.DecodeReauth(payload)
	if err != nil {
		return &ClientError{Code: proto.ErrorCodeProtocol, Message: "malformed reauth payload"}
	}
	id, err := s.validator.Validate(ctx, reauth.AccessToken)
	if err != nil {
		return &ClientError{Code: proto.ErrorCodeSessionExpired, Message: "reauth authentication failed"}
	}
	if err := s.Registry.Reauthenticate(sid, id.Sub, snap.AccountID, id.ExpiresAt); err != nil {
		if errors.Is(err, session.ErrIdentityMismatch) ||
			errors.Is(err, session.ErrBadState) ||
			errors.Is(err, session.ErrNotFound) {
			return &ClientError{Code: proto.ErrorCodeSessionExpired, Message: "reauth identity mismatch"}
		}
		return err
	}
	s.armDeadline(ca, conn, sid, id.ExpiresAt)
	return s.send(ctx, ca, conn, sid, proto.OpcodeReauthOK, proto.MessageVersion1, func(e *proto.Encoder) error {
		proto.ReauthOK{}.Encode(e)
		return nil
	})
}

// dispatch passes an allowed non-connect opcode to the handler seam with
// a responder that always allocates framing. A *ClientError becomes a
// 202 with the connection alive; an unexpected internal error is logged
// and ends the connection.
func (s *Server) dispatch(
	ctx context.Context,
	ca *connAuth,
	conn *websocket.Conn,
	sid session.ID,
	header proto.Header,
	payload *proto.Decoder,
) error {
	if s.handler == nil {
		return nil
	}
	send := func(opcode uint16, msgVersion uint16, encode func(*proto.Encoder) error) error {
		return s.send(ctx, ca, conn, sid, opcode, msgVersion, encode)
	}
	if err := s.handler.Handle(ctx, sid, header, payload, send); err != nil {
		var cerr *ClientError
		if errors.As(err, &cerr) {
			return s.replyError(ctx, ca, conn, sid, cerr.Code, cerr.Message)
		}
		slog.Error("gateway: handler internal failure", "session", uint64(sid),
			"opcode", header.Opcode, "err", err)
		return err
	}
	return nil
}

// armDeadline schedules TokenExp + ReauthGrace for this connection,
// cancelling/superseding any previous timer. The callback only fires
// for its own generation and TokenExp, so a stale timer can never
// disconnect a reauthenticated session.
func (s *Server) armDeadline(ca *connAuth, conn *websocket.Conn, sid session.ID, tokenExp time.Time) {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	if ca.cancel != nil {
		ca.cancel()
		ca.cancel = nil
	}
	if ca.done {
		return
	}
	ca.gen++
	gen := ca.gen
	dl := tokenExp.Add(ReauthGrace)
	ca.deadline = dl
	ca.cancel = s.schedule(dl, func() { s.onDeadline(ca, conn, sid, gen, dl) })
}

// cancelDeadline releases the connection's timer at teardown so no
// timer retains the connection/session.
func (s *Server) cancelDeadline(ca *connAuth) {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	ca.done = true
	if ca.cancel != nil {
		ca.cancel()
		ca.cancel = nil
	}
}

// onDeadline terminates a connection whose hard deadline passed, even
// when idle: best-effort 202 session_expired on a bounded context, then
// close (which unblocks the read loop into cleanup). Stale callbacks —
// superseded generation, changed TokenExp, vanished session, or an
// early fire — return without effect.
func (s *Server) onDeadline(ca *connAuth, conn *websocket.Conn, sid session.ID, gen uint64, dl time.Time) {
	ca.mu.Lock()
	stale := ca.done || gen != ca.gen || !dl.Equal(ca.deadline)
	ca.mu.Unlock()
	if stale {
		return
	}
	snap, ok := s.Registry.Get(sid)
	if !ok {
		return
	}
	if !snap.TokenExp.Add(ReauthGrace).Equal(dl) {
		return
	}
	if s.now().Before(dl) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.sendError(ctx, ca, conn, sid, proto.ErrorCodeSessionExpired, "reauth deadline exceeded")
	// Close asynchronously: the close handshake waits for the peer's
	// response, which must never block this timer callback (an idle or
	// dead peer would stall it up to the library's handshake timeout).
	// The goroutine is bounded by that same timeout, not a leak, and a
	// concurrent deferred Close elsewhere is a harmless no-op.
	go func() {
		_ = conn.Close(websocket.StatusNormalClosure, "reauth deadline exceeded")
	}()
}

// sendError writes one 202 error frame: MessageVersion1, the next S→C
// session sequence, the injected tick, and the numeric code plus a
// diagnostic message clients must not parse for control flow.
func (s *Server) sendError(
	ctx context.Context,
	ca *connAuth,
	conn *websocket.Conn,
	sid session.ID,
	code uint16,
	msg string,
) error {
	return s.send(ctx, ca, conn, sid, proto.OpcodeError, proto.MessageVersion1, func(e *proto.Encoder) error {
		proto.ErrorMessage{Code: code, Message: msg}.Encode(e)
		return nil
	})
}

// send allocates the session S→C sequence, encodes one frame, and
// writes it as a single binary WebSocket message. Writes serialize on
// the connection mutex because the deadline callback may write while
// the read loop is dispatching.
func (s *Server) send(
	ctx context.Context,
	ca *connAuth,
	conn *websocket.Conn,
	sid session.ID,
	opcode uint16,
	msgVersion uint16,
	encode func(*proto.Encoder) error,
) error {
	seq, err := s.Registry.NextServerSeq(sid)
	if err != nil {
		return err
	}
	frame, err := proto.EncodeFrame(proto.Header{
		Opcode:     opcode,
		MsgVersion: msgVersion,
		Seq:        seq,
		Tick:       s.tick(),
	}, encode)
	if err != nil {
		return err
	}
	ca.wmu.Lock()
	defer ca.wmu.Unlock()
	return conn.Write(ctx, websocket.MessageBinary, frame)
}
