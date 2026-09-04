package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/dlukt/voxilian/internal/proto"
	"github.com/dlukt/voxilian/internal/session"
)

// AuthResult is the validated identity a hello/reauth token maps to.
// T1 carries no JWT/Keycloak types: the fakeable AuthFunc seam produces
// this, and M3-T2 replaces the fake with real validation plus account
// provisioning without changing the gateway.
type AuthResult struct {
	Sub       string
	AccountID int64
	TokenExp  time.Time
}

// AuthFunc validates an access token and returns its identity.
// Any returned error means authentication failed.
type AuthFunc func(ctx context.Context, accessToken string) (AuthResult, error)

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

// Server is the WebSocket edge: it accepts connections, registers one
// session per connection, enforces the lifecycle permission gate, and
// routes allowed messages to the handler seam. Construct with NewServer;
// tests instantiate it directly over httptest (no cmd/serve wiring in
// T1).
type Server struct {
	Registry *session.Registry
	auth     AuthFunc
	welcome  WelcomeFunc
	tick     TickFunc
	handler  MessageHandler
}

// NewServer builds a gateway over reg. Nil seams get test-safe
// defaults: nil auth always fails, nil welcome yields a zero Welcome,
// nil tick yields 0, nil handler consumes allowed messages without a
// reply (their owners land in later tasks).
func NewServer(
	reg *session.Registry,
	auth AuthFunc,
	welcome WelcomeFunc,
	tick TickFunc,
	handler MessageHandler,
) *Server {
	if auth == nil {
		auth = func(context.Context, string) (AuthResult, error) {
			return AuthResult{}, errors.New("gateway: auth not configured")
		}
	}
	if welcome == nil {
		welcome = func(context.Context) proto.Welcome { return proto.Welcome{} }
	}
	if tick == nil {
		tick = func() uint32 { return 0 }
	}
	return &Server{
		Registry: reg,
		auth:     auth,
		welcome:  welcome,
		tick:     tick,
		handler:  handler,
	}
}

// ServeHTTP accepts one WebSocket connection and serves it until it
// terminates, then removes its session from the registry (idempotent
// cleanup of sub/character indexes and connection references).
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
	defer s.Registry.Remove(sid)

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
			if err := s.sendError(ctx, conn, sid,
				proto.ErrorCodeProtocol, "text websocket messages are not supported"); err != nil {
				return
			}
			continue
		}
		if err := s.handleBinary(ctx, conn, sid, data); err != nil {
			return
		}
	}
}

// handleBinary routes one binary message that passed the transport read
// limit. A non-nil return ends the connection; handled protocol
// violations reply 202 and return nil to keep the session alive.
func (s *Server) handleBinary(
	ctx context.Context,
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
		return s.replyError(ctx, conn, sid, proto.ErrorCodeProtocol, msg)
	}
	// Unknown opcodes and client-sent S→C opcodes are protocol errors,
	// never bad_state, and never disconnect on the first offense.
	if !session.IsClientOpcode(header.Opcode) {
		return s.replyError(ctx, conn, sid,
			proto.ErrorCodeProtocol,
			fmt.Sprintf("unsupported client opcode %d", header.Opcode))
	}
	snap, ok := s.Registry.Get(sid)
	if !ok {
		return errors.New("gateway: session vanished mid-connection")
	}
	// Incoming C→S seq/tick are opaque in T1: no ordering, wrap, or
	// future-tick enforcement here (M3-T5/M4 own those).
	if !session.Allowed(snap.State, header.Opcode) {
		return s.replyError(ctx, conn, sid,
			proto.ErrorCodeBadState,
			fmt.Sprintf("opcode %d is not allowed in %s", header.Opcode, snap.State))
	}
	switch header.Opcode {
	case proto.OpcodeHello:
		return s.finishStep(ctx, conn, sid, s.handleHello(ctx, conn, sid, payload))
	case proto.OpcodeReauth:
		return s.finishStep(ctx, conn, sid, s.handleReauth(ctx, conn, sid, payload))
	default:
		return s.dispatch(ctx, conn, sid, header, payload)
	}
}

// replyError sends a 202 and keeps the session alive; a failed write
// ends the connection.
func (s *Server) replyError(
	ctx context.Context,
	conn *websocket.Conn,
	sid session.ID,
	code uint16,
	msg string,
) error {
	if err := s.sendError(ctx, conn, sid, code, msg); err != nil {
		return err
	}
	return nil
}

// finishStep maps a hello/reauth step result onto the wire: nil means
// the step already replied, a *ClientError becomes a 202, anything else
// is an internal failure that ends the connection.
func (s *Server) finishStep(
	ctx context.Context,
	conn *websocket.Conn,
	sid session.ID,
	err error,
) error {
	if err == nil {
		return nil
	}
	var cerr *ClientError
	if errors.As(err, &cerr) {
		return s.replyError(ctx, conn, sid, cerr.Code, cerr.Message)
	}
	return err
}

// handleHello serves opcode 100: decode, fakeable-auth, atomically
// establish identity (CONNECTED → AUTHENTICATED), and reply 200.
// Every reply uses MessageVersion1 and the next S→C session sequence.
func (s *Server) handleHello(
	ctx context.Context,
	conn *websocket.Conn,
	sid session.ID,
	payload *proto.Decoder,
) error {
	hello, err := proto.DecodeHello(payload)
	if err != nil {
		return &ClientError{Code: proto.ErrorCodeProtocol, Message: "malformed hello payload"}
	}
	res, err := s.auth(ctx, hello.AccessToken)
	if err != nil {
		return &ClientError{Code: proto.ErrorCodeSessionExpired, Message: "hello authentication failed"}
	}
	if err := s.Registry.Authenticate(sid, res.Sub, res.AccountID, res.TokenExp); err != nil {
		if errors.Is(err, session.ErrBadState) || errors.Is(err, session.ErrNotFound) {
			return &ClientError{
				Code:    proto.ErrorCodeBadState,
				Message: "hello is not allowed in current session state",
			}
		}
		return err
	}
	welcome := s.welcome(ctx)
	return s.send(ctx, conn, sid, proto.OpcodeWelcome, proto.MessageVersion1, func(e *proto.Encoder) error {
		welcome.Encode(e)
		return nil
	})
}

// handleReauth serves opcode 101 over an established session: decode,
// fakeable-auth, require identical identity, refresh TokenExp, and
// reply 201. Identity change or auth failure yields session_expired and
// leaves the stored identity untouched.
func (s *Server) handleReauth(
	ctx context.Context,
	conn *websocket.Conn,
	sid session.ID,
	payload *proto.Decoder,
) error {
	reauth, err := proto.DecodeReauth(payload)
	if err != nil {
		return &ClientError{Code: proto.ErrorCodeProtocol, Message: "malformed reauth payload"}
	}
	res, err := s.auth(ctx, reauth.AccessToken)
	if err != nil {
		return &ClientError{Code: proto.ErrorCodeSessionExpired, Message: "reauth authentication failed"}
	}
	if err := s.Registry.Reauthenticate(sid, res.Sub, res.AccountID, res.TokenExp); err != nil {
		if errors.Is(err, session.ErrIdentityMismatch) ||
			errors.Is(err, session.ErrBadState) ||
			errors.Is(err, session.ErrNotFound) {
			return &ClientError{Code: proto.ErrorCodeSessionExpired, Message: "reauth identity mismatch"}
		}
		return err
	}
	return s.send(ctx, conn, sid, proto.OpcodeReauthOK, proto.MessageVersion1, func(e *proto.Encoder) error {
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
	conn *websocket.Conn,
	sid session.ID,
	header proto.Header,
	payload *proto.Decoder,
) error {
	if s.handler == nil {
		return nil
	}
	send := func(opcode uint16, msgVersion uint16, encode func(*proto.Encoder) error) error {
		return s.send(ctx, conn, sid, opcode, msgVersion, encode)
	}
	if err := s.handler.Handle(ctx, sid, header, payload, send); err != nil {
		var cerr *ClientError
		if errors.As(err, &cerr) {
			return s.replyError(ctx, conn, sid, cerr.Code, cerr.Message)
		}
		slog.Error("gateway: handler internal failure", "session", uint64(sid),
			"opcode", header.Opcode, "err", err)
		return err
	}
	return nil
}

// sendError writes one 202 error frame: MessageVersion1, the next S→C
// session sequence, the injected tick, and the numeric code plus a
// diagnostic message clients must not parse for control flow.
func (s *Server) sendError(
	ctx context.Context,
	conn *websocket.Conn,
	sid session.ID,
	code uint16,
	msg string,
) error {
	return s.send(ctx, conn, sid, proto.OpcodeError, proto.MessageVersion1, func(e *proto.Encoder) error {
		proto.ErrorMessage{Code: code, Message: msg}.Encode(e)
		return nil
	})
}

// send allocates the session S→C sequence, encodes one frame, and
// writes it as a single binary WebSocket message.
func (s *Server) send(
	ctx context.Context,
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
	return conn.Write(ctx, websocket.MessageBinary, frame)
}
