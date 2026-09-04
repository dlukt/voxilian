package gateway_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dlukt/voxilian/internal/gateway"
	"github.com/dlukt/voxilian/internal/proto"
	"github.com/dlukt/voxilian/internal/session"
)

const testTick = 555

var (
	fixedExp     = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	renewedExp   = time.Date(2026, 9, 4, 13, 0, 0, 0, time.UTC)
	fixedWelcome = proto.Welcome{
		ServerTimeMs: 123456789,
		Chunk:        16,
		AOIRadius:    96,
		TickRates:    []uint16{20},
		World:        proto.WorldInfo{Mode: 1, Seed: 7, Version: 3},
	}
)

type recordedCall struct {
	sid    session.ID
	header proto.Header
}

type recordHandler struct {
	mu        sync.Mutex
	calls     []recordedCall
	autoReply bool
	retErr    error
}

func (h *recordHandler) Handle(
	_ context.Context,
	sid session.ID,
	header proto.Header,
	_ *proto.Decoder,
	send gateway.SendFunc,
) error {
	h.mu.Lock()
	h.calls = append(h.calls, recordedCall{sid: sid, header: header})
	retErr := h.retErr
	autoReply := h.autoReply
	h.mu.Unlock()
	if retErr != nil {
		return retErr
	}
	if autoReply {
		return send(proto.OpcodeCharacterListResult, proto.MessageVersion1, func(e *proto.Encoder) error {
			proto.CharacterList{}.Encode(e)
			return nil
		})
	}
	return nil
}

func (h *recordHandler) setErr(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.retErr = err
}

func (h *recordHandler) callCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.calls)
}

func (h *recordHandler) lastCall() recordedCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls[len(h.calls)-1]
}

type fixture struct {
	reg     *session.Registry
	handler *recordHandler
	ts      *httptest.Server
	tokens  map[string]gateway.AuthResult
	bad     map[string]bool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		reg:     session.NewRegistry(),
		handler: &recordHandler{autoReply: true},
		tokens: map[string]gateway.AuthResult{
			"tok":  {Sub: "test-sub", AccountID: 42, TokenExp: fixedExp},
			"tok2": {Sub: "test-sub", AccountID: 42, TokenExp: renewedExp},
			"evil": {Sub: "other-sub", AccountID: 99, TokenExp: renewedExp},
		},
		bad: map[string]bool{"bad": true},
	}
	auth := func(_ context.Context, token string) (gateway.AuthResult, error) {
		if f.bad[token] {
			return gateway.AuthResult{}, errors.New("forged token")
		}
		res, ok := f.tokens[token]
		if !ok {
			return gateway.AuthResult{}, errors.New("unknown token")
		}
		return res, nil
	}
	srv := gateway.NewServer(f.reg, auth,
		func(context.Context) proto.Welcome { return fixedWelcome },
		func() uint32 { return testTick },
		f.handler)
	f.ts = httptest.NewServer(srv)
	t.Cleanup(f.ts.Close)
	return f
}

func (f *fixture) dial(t *testing.T) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(f.ts.URL, "http")
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(websocket.StatusNormalClosure, "") })
	return c
}

func writeMsg(t *testing.T, c *websocket.Conn, typ websocket.MessageType, data []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, typ, data); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func sendFrame(t *testing.T, c *websocket.Conn, header proto.Header, encode func(*proto.Encoder) error) {
	t.Helper()
	frame, err := proto.EncodeFrame(header, encode)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	writeMsg(t, c, websocket.MessageBinary, frame)
}

func readFrame(t *testing.T, c *websocket.Conn) (proto.Header, *proto.Decoder) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	typ, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != websocket.MessageBinary {
		t.Fatalf("message type = %v, want binary", typ)
	}
	header, payload, err := proto.DecodeFrame(data)
	if err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	return header, payload
}

func readError(t *testing.T, c *websocket.Conn) (proto.Header, proto.ErrorMessage) {
	t.Helper()
	header, payload := readFrame(t, c)
	if header.Opcode != proto.OpcodeError {
		t.Fatalf("opcode = %d, want 202", header.Opcode)
	}
	msg, err := proto.DecodeErrorMessage(payload)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}
	return header, msg
}

func sendHello(t *testing.T, c *websocket.Conn, token string, seq, tick uint32) {
	t.Helper()
	sendFrame(t, c,
		proto.Header{Opcode: proto.OpcodeHello, MsgVersion: 1, Seq: seq, Tick: tick},
		func(e *proto.Encoder) error {
			proto.Hello{ClientVersion: 9, ProtoVersion: 1, AccessToken: token}.Encode(e)
			return nil
		})
}

// waitFor polls cond until true or the bounded safety deadline hits. The
// condition (not timing) is the synchronization; the deadline only fails
// a stuck test instead of hanging it.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func sessionOf(t *testing.T, f *fixture, sub string) session.Snapshot {
	t.Helper()
	ids := f.reg.SessionsBySub(sub)
	if len(ids) != 1 {
		t.Fatalf("SessionsBySub(%q) = %v, want exactly one", sub, ids)
	}
	snap, ok := f.reg.Get(ids[0])
	if !ok {
		t.Fatalf("session %d vanished", ids[0])
	}
	return snap
}

func TestConnectCreatesAndRemovesSession(t *testing.T) {
	f := newFixture(t)
	if n := f.reg.Len(); n != 0 {
		t.Fatalf("Len() = %d, want 0 before connect", n)
	}
	c := f.dial(t)
	waitFor(t, "session registration", func() bool { return f.reg.Len() == 1 })
	if got := f.reg.SessionsBySub("test-sub"); len(got) != 0 {
		t.Errorf("sub index non-empty before hello: %v", got)
	}
	if err := c.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatalf("close: %v", err)
	}
	waitFor(t, "session cleanup", func() bool { return f.reg.Len() == 0 })
}

func TestHelloSuccess(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t)
	// Deliberately unusual C→S serial fields: routing must ignore them.
	sendHello(t, c, "tok", 0xFFFFFFFF, 0x80000000)

	header, payload := readFrame(t, c)
	if header.Opcode != proto.OpcodeWelcome {
		t.Fatalf("opcode = %d, want 200", header.Opcode)
	}
	if header.MsgVersion != proto.MessageVersion1 {
		t.Errorf("msg_version = %d, want %d", header.MsgVersion, proto.MessageVersion1)
	}
	if header.Seq != 1 {
		t.Errorf("first S→C seq = %d, want 1", header.Seq)
	}
	if header.Tick != testTick {
		t.Errorf("tick = %d, want %d", header.Tick, testTick)
	}
	got, err := proto.DecodeWelcome(payload)
	if err != nil {
		t.Fatalf("decode welcome: %v", err)
	}
	if !reflect.DeepEqual(got, fixedWelcome) {
		t.Errorf("welcome = %+v, want %+v", got, fixedWelcome)
	}

	snap := sessionOf(t, f, "test-sub")
	if snap.State != session.StateAuthenticated {
		t.Errorf("state = %s, want AUTHENTICATED", snap.State)
	}
	if snap.AccountID != 42 {
		t.Errorf("accountID = %d, want 42", snap.AccountID)
	}
	if !snap.TokenExp.Equal(fixedExp) {
		t.Errorf("tokenExp = %v, want %v", snap.TokenExp, fixedExp)
	}
}

func TestServerSeqSequentialAcrossReplies(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t)
	sendHello(t, c, "tok", 1, 1)
	header, _ := readFrame(t, c)
	if header.Seq != 1 {
		t.Fatalf("hello reply seq = %d, want 1", header.Seq)
	}
	// An allowed opcode reaches the auto-reply handler; the reply must
	// carry the next server seq, never the incoming C→S seq.
	sendFrame(t, c,
		proto.Header{Opcode: proto.OpcodeCharacterList, MsgVersion: 1, Seq: 0xFFFFFFFE, Tick: 0x80000000},
		func(e *proto.Encoder) error {
			proto.CharacterListRequest{}.Encode(e)
			return nil
		})
	header, _ = readFrame(t, c)
	if header.Opcode != proto.OpcodeCharacterListResult {
		t.Fatalf("opcode = %d, want 216", header.Opcode)
	}
	if header.Seq != 2 {
		t.Errorf("second S→C seq = %d, want 2 (incoming C→S seq must not be copied)", header.Seq)
	}
}

func TestHelloAuthFailureKeepsConnectedSession(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t)
	sendHello(t, c, "bad", 1, 1)
	_, msg := readError(t, c)
	if msg.Code != proto.ErrorCodeSessionExpired {
		t.Errorf("code = %d, want %d (session_expired)", msg.Code, proto.ErrorCodeSessionExpired)
	}
	if got := f.reg.SessionsBySub("test-sub"); len(got) != 0 {
		t.Errorf("sub index = %v after failed hello, want empty", got)
	}
	// Same connection still serves a good hello: the session survived.
	sendHello(t, c, "tok", 2, 2)
	header, _ := readFrame(t, c)
	if header.Opcode != proto.OpcodeWelcome {
		t.Fatalf("opcode after retry = %d, want 200", header.Opcode)
	}
}

func TestHelloMalformedPayload(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t)
	sendFrame(t, c,
		proto.Header{Opcode: proto.OpcodeHello, MsgVersion: 1, Seq: 1, Tick: 1},
		func(e *proto.Encoder) error {
			e.U8(0xFF) // truncated hello
			return nil
		})
	header, msg := readError(t, c)
	if header.MsgVersion != proto.MessageVersion1 {
		t.Errorf("error msg_version = %d, want 1", header.MsgVersion)
	}
	if msg.Code != proto.ErrorCodeProtocol {
		t.Errorf("code = %d, want %d (protocol_error)", msg.Code, proto.ErrorCodeProtocol)
	}
	sendHello(t, c, "tok", 2, 2)
	header, _ = readFrame(t, c)
	if header.Opcode != proto.OpcodeWelcome {
		t.Fatalf("opcode after malformed hello = %d, want 200", header.Opcode)
	}
}

func TestReauthSuccess(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t)
	sendHello(t, c, "tok", 1, 1)
	_, _ = readFrame(t, c)

	sendFrame(t, c,
		proto.Header{Opcode: proto.OpcodeReauth, MsgVersion: 1, Seq: 2, Tick: 2},
		func(e *proto.Encoder) error {
			proto.Reauth{AccessToken: "tok2"}.Encode(e)
			return nil
		})
	header, payload := readFrame(t, c)
	if header.Opcode != proto.OpcodeReauthOK {
		t.Fatalf("opcode = %d, want 201", header.Opcode)
	}
	if header.MsgVersion != proto.MessageVersion1 {
		t.Errorf("msg_version = %d, want 1", header.MsgVersion)
	}
	if _, err := proto.DecodeReauthOK(payload); err != nil {
		t.Fatalf("decode reauth_ok: %v", err)
	}
	if snap := sessionOf(t, f, "test-sub"); !snap.TokenExp.Equal(renewedExp) {
		t.Errorf("tokenExp = %v, want %v", snap.TokenExp, renewedExp)
	}
}

func TestReauthIdentityChangeRejected(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t)
	sendHello(t, c, "tok", 1, 1)
	_, _ = readFrame(t, c)

	sendFrame(t, c,
		proto.Header{Opcode: proto.OpcodeReauth, MsgVersion: 1, Seq: 2, Tick: 2},
		func(e *proto.Encoder) error {
			proto.Reauth{AccessToken: "evil"}.Encode(e)
			return nil
		})
	_, msg := readError(t, c)
	if msg.Code != proto.ErrorCodeSessionExpired {
		t.Errorf("code = %d, want %d (session_expired)", msg.Code, proto.ErrorCodeSessionExpired)
	}
	snap := sessionOf(t, f, "test-sub")
	if snap.AccountID != 42 {
		t.Errorf("accountID = %d after rejected reauth, want 42", snap.AccountID)
	}
	if !snap.TokenExp.Equal(fixedExp) {
		t.Errorf("tokenExp = %v after rejected reauth, want %v", snap.TokenExp, fixedExp)
	}
}

func TestWrongStateKnownOpcode(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t)
	// 122 character_create is AUTHENTICATED-only; this session is CONNECTED.
	sendFrame(t, c,
		proto.Header{Opcode: proto.OpcodeCharacterCreate, MsgVersion: 1, Seq: 1, Tick: 1},
		func(e *proto.Encoder) error {
			proto.CharacterCreate{Slot: 0, Name: "Bob"}.Encode(e)
			return nil
		})
	_, msg := readError(t, c)
	if msg.Code != proto.ErrorCodeBadState {
		t.Errorf("code = %d, want %d (bad_state)", msg.Code, proto.ErrorCodeBadState)
	}
	if n := f.handler.callCount(); n != 0 {
		t.Errorf("handler called %d times for wrong-state opcode, want 0", n)
	}
}

func TestAllowedOpcodeReachesHandler(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t)
	sendHello(t, c, "tok", 1, 1)
	_, _ = readFrame(t, c)

	sendFrame(t, c,
		proto.Header{Opcode: proto.OpcodeCharacterList, MsgVersion: 1, Seq: 0xFFFFFFFF, Tick: 0x80000000},
		func(e *proto.Encoder) error {
			proto.CharacterListRequest{}.Encode(e)
			return nil
		})
	header, _ := readFrame(t, c) // handler auto-reply synchronizes the assertion below.
	if header.Opcode != proto.OpcodeCharacterListResult {
		t.Fatalf("opcode = %d, want 216", header.Opcode)
	}
	if n := f.handler.callCount(); n != 1 {
		t.Fatalf("handler called %d times, want 1", n)
	}
	call := f.handler.lastCall()
	if call.header.Opcode != proto.OpcodeCharacterList {
		t.Errorf("handler opcode = %d, want 121", call.header.Opcode)
	}
	if call.header.Seq != 0xFFFFFFFF || call.header.Tick != 0x80000000 {
		t.Errorf("handler saw seq=%#x tick=%#x, want opaque passthrough",
			call.header.Seq, call.header.Tick)
	}
}

func TestUnknownOpcode(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t)
	sendFrame(t, c,
		proto.Header{Opcode: 65535, MsgVersion: 1, Seq: 1, Tick: 1},
		func(e *proto.Encoder) error { return nil })
	_, msg := readError(t, c)
	if msg.Code != proto.ErrorCodeProtocol {
		t.Errorf("unknown opcode code = %d, want %d (protocol_error)", msg.Code, proto.ErrorCodeProtocol)
	}
	// Connection remains usable.
	sendHello(t, c, "tok", 2, 2)
	header, _ := readFrame(t, c)
	if header.Opcode != proto.OpcodeWelcome {
		t.Fatalf("opcode after unknown opcode = %d, want 200", header.Opcode)
	}
}

func TestWrongDirectionOpcode(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t)
	// 220 shop_list is S→C-only; a client sending it gets protocol_error.
	sendFrame(t, c,
		proto.Header{Opcode: proto.OpcodeShopList, MsgVersion: 1, Seq: 1, Tick: 1},
		func(e *proto.Encoder) error { return nil })
	_, msg := readError(t, c)
	if msg.Code != proto.ErrorCodeProtocol {
		t.Errorf("wrong-direction code = %d, want %d (protocol_error)", msg.Code, proto.ErrorCodeProtocol)
	}
	sendHello(t, c, "tok", 2, 2)
	header, _ := readFrame(t, c)
	if header.Opcode != proto.OpcodeWelcome {
		t.Fatalf("opcode after wrong-direction opcode = %d, want 200", header.Opcode)
	}
}

func TestTextMessage(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t)
	writeMsg(t, c, websocket.MessageText, []byte(`{"hello":"json is not the protocol"}`))
	_, msg := readError(t, c)
	if msg.Code != proto.ErrorCodeProtocol {
		t.Errorf("text message code = %d, want %d (protocol_error)", msg.Code, proto.ErrorCodeProtocol)
	}
	sendHello(t, c, "tok", 1, 1)
	header, _ := readFrame(t, c)
	if header.Opcode != proto.OpcodeWelcome {
		t.Fatalf("opcode after text message = %d, want 200", header.Opcode)
	}
}

func TestMalformedShortFrame(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t)
	writeMsg(t, c, websocket.MessageBinary, []byte{1, 2, 3, 4, 5})
	_, msg := readError(t, c)
	if msg.Code != proto.ErrorCodeProtocol {
		t.Errorf("short frame code = %d, want %d (protocol_error)", msg.Code, proto.ErrorCodeProtocol)
	}
	// First offense does not disconnect.
	sendHello(t, c, "tok", 1, 1)
	header, _ := readFrame(t, c)
	if header.Opcode != proto.OpcodeWelcome {
		t.Fatalf("opcode after short frame = %d, want 200", header.Opcode)
	}
}

func TestHandlerClientErrorKeepsConnection(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t)
	sendHello(t, c, "tok", 1, 1)
	_, _ = readFrame(t, c)

	f.handler.setErr(&gateway.ClientError{Code: proto.ErrorCodeRateLimited, Message: "slow down"})
	sendFrame(t, c,
		proto.Header{Opcode: proto.OpcodeCharacterList, MsgVersion: 1, Seq: 2, Tick: 2},
		func(e *proto.Encoder) error {
			proto.CharacterListRequest{}.Encode(e)
			return nil
		})
	_, msg := readError(t, c)
	if msg.Code != proto.ErrorCodeRateLimited {
		t.Errorf("code = %d, want %d", msg.Code, proto.ErrorCodeRateLimited)
	}
	// Connection alive: clear the error and get the auto-reply.
	f.handler.setErr(nil)
	sendFrame(t, c,
		proto.Header{Opcode: proto.OpcodeCharacterList, MsgVersion: 1, Seq: 3, Tick: 3},
		func(e *proto.Encoder) error {
			proto.CharacterListRequest{}.Encode(e)
			return nil
		})
	header, _ := readFrame(t, c)
	if header.Opcode != proto.OpcodeCharacterListResult {
		t.Fatalf("opcode after client error = %d, want 216", header.Opcode)
	}
}

func TestHandlerInternalErrorEndsConnection(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t)
	sendHello(t, c, "tok", 1, 1)
	_, _ = readFrame(t, c)

	f.handler.setErr(errors.New("boom"))
	sendFrame(t, c,
		proto.Header{Opcode: proto.OpcodeCharacterList, MsgVersion: 1, Seq: 2, Tick: 2},
		func(e *proto.Encoder) error {
			proto.CharacterListRequest{}.Encode(e)
			return nil
		})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := c.Read(ctx); err == nil {
		t.Fatal("expected connection termination after internal handler error")
	}
	waitFor(t, "session cleanup after internal error", func() bool { return f.reg.Len() == 0 })
}

func TestOversizeMessageClosesWithMessageTooBig(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t)
	waitFor(t, "session registration", func() bool { return f.reg.Len() == 1 })
	big := make([]byte, proto.MaxFrameSize+1024)
	for i := range big {
		big[i] = 0xAB
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageBinary, big); err != nil {
		t.Fatalf("write oversize: %v", err)
	}
	// The transport terminates with 1009; no 202 can follow because the
	// application never receives the frame.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	_, _, err := c.Read(ctx2)
	if err == nil {
		t.Fatal("expected termination after oversize message")
	}
	if status := websocket.CloseStatus(err); status != websocket.StatusMessageTooBig {
		t.Errorf("close status = %v, want 1009 MessageTooBig", status)
	}
	waitFor(t, "session cleanup after oversize", func() bool { return f.reg.Len() == 0 })
}
