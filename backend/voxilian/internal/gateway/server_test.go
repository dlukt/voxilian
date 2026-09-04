package gateway_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/lestrrat-go/jwx/v4/jwa"
	"github.com/lestrrat-go/jwx/v4/jwk"
	"github.com/lestrrat-go/jwx/v4/jws"
	"github.com/lestrrat-go/jwx/v4/jwt"
	"github.com/pressly/goose/v3"

	"github.com/dlukt/voxilian/internal/auth"
	"github.com/dlukt/voxilian/internal/gateway"
	"github.com/dlukt/voxilian/internal/proto"
	"github.com/dlukt/voxilian/internal/session"
	"github.com/dlukt/voxilian/internal/simtest"
	"github.com/dlukt/voxilian/internal/store"
)

const testTick = 555

var fixedWelcome = proto.Welcome{
	ServerTimeMs: 123456789,
	Chunk:        16,
	AOIRadius:    96,
	TickRates:    []uint16{20},
	World:        proto.WorldInfo{Mode: 1, Seed: 7, Version: 3},
}

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

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

// fakeValidator mimics real validator semantics on the test clock:
// unknown/forbidden tokens fail, and canned identities expire against
// now (an expired identity is invalid immediately, no grace opens it).
type fakeValidator struct {
	mu    sync.Mutex
	ids   map[string]auth.Identity
	bad   map[string]bool
	calls []string
	now   func() time.Time
}

func (f *fakeValidator) Validate(_ context.Context, tok string) (auth.Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, tok)
	if f.bad[tok] {
		return auth.Identity{}, errors.New("forged token")
	}
	id, ok := f.ids[tok]
	if !ok {
		return auth.Identity{}, errors.New("unknown token")
	}
	if !f.now().Before(id.ExpiresAt) {
		return auth.Identity{}, fmt.Errorf("expired: %w", auth.ErrInvalidToken)
	}
	return id, nil
}

func (f *fakeValidator) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type accountCall struct {
	sub   string
	email *string
}

type fakeAccounts struct {
	mu    sync.Mutex
	ids   map[string]int64
	calls []accountCall
	fail  bool
}

func (f *fakeAccounts) EnsureAccount(_ context.Context, sub string, email *string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, accountCall{sub: sub, email: email})
	if f.fail {
		return 0, errors.New("pg unavailable")
	}
	id, ok := f.ids[sub]
	if !ok {
		return 0, fmt.Errorf("unknown sub %q", sub)
	}
	return id, nil
}

func (f *fakeAccounts) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeAccounts) lastCall() accountCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

type manualTimer struct {
	at        time.Time
	fn        func()
	cancelled bool
}

type manualScheduler struct {
	mu     sync.Mutex
	timers []*manualTimer
}

func (m *manualScheduler) schedule(at time.Time, fn func()) gateway.CancelFunc {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := &manualTimer{at: at, fn: fn}
	m.timers = append(m.timers, t)
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		t.cancelled = true
	}
}

// pending returns the non-cancelled timers.
func (m *manualScheduler) pending() []*manualTimer {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*manualTimer
	for _, t := range m.timers {
		if !t.cancelled {
			out = append(out, t)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// fixture
// ---------------------------------------------------------------------------

type fixture struct {
	reg       *session.Registry
	handler   *recordHandler
	validator *fakeValidator
	accounts  *fakeAccounts
	clock     *simtest.Clock
	sched     *manualScheduler
	ts        *httptest.Server
	t0        time.Time
	exp1      time.Time
	exp2      time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	t0 := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	f := &fixture{
		reg:     session.NewRegistry(),
		handler: &recordHandler{autoReply: true},
		clock:   simtest.NewClock(t0),
		sched:   &manualScheduler{},
		t0:      t0,
		exp1:    t0.Add(5 * time.Minute),
		exp2:    t0.Add(10 * time.Minute),
	}
	f.validator = &fakeValidator{
		ids: map[string]auth.Identity{
			"tok":  {Sub: "test-sub", Email: "hero@example.com", HasEmail: true, ExpiresAt: f.exp1},
			"tok2": {Sub: "test-sub", ExpiresAt: f.exp2},
			"evil": {Sub: "other-sub", ExpiresAt: f.exp1},
		},
		bad: map[string]bool{"bad": true},
		now: f.clock.Now,
	}
	f.accounts = &fakeAccounts{ids: map[string]int64{"test-sub": 42, "other-sub": 99}}
	srv := gateway.NewServer(gateway.ServerDeps{
		Registry:  f.reg,
		Validator: f.validator,
		Accounts:  f.accounts,
		Welcome:   func(context.Context) proto.Welcome { return fixedWelcome },
		Tick:      func() uint32 { return testTick },
		Now:       f.clock.Now,
		Schedule:  f.sched.schedule,
		Handler:   f.handler,
	})
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

// readClosed asserts the server terminated the connection.
func readClosed(t *testing.T, c *websocket.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := c.Read(ctx); err == nil {
		t.Fatal("expected terminated connection")
	}
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

func sendReauth(t *testing.T, c *websocket.Conn, token string, seq uint32) {
	t.Helper()
	sendFrame(t, c,
		proto.Header{Opcode: proto.OpcodeReauth, MsgVersion: 1, Seq: seq, Tick: 1},
		func(e *proto.Encoder) error {
			proto.Reauth{AccessToken: token}.Encode(e)
			return nil
		})
}

func sendCharList(t *testing.T, c *websocket.Conn, seq uint32) {
	t.Helper()
	sendFrame(t, c,
		proto.Header{Opcode: proto.OpcodeCharacterList, MsgVersion: 1, Seq: seq, Tick: 1},
		func(e *proto.Encoder) error {
			proto.CharacterListRequest{}.Encode(e)
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

// ---------------------------------------------------------------------------
// connect + hello
// ---------------------------------------------------------------------------

func TestConnectCreatesAndRemovesSession(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t)
	waitFor(t, "session registration", func() bool { return f.reg.Len() == 1 })
	if err := c.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatalf("close: %v", err)
	}
	waitFor(t, "session cleanup", func() bool { return f.reg.Len() == 0 })
	if n := len(f.sched.pending()); n != 0 {
		t.Errorf("pending deadlines after disconnect = %d, want 0", n)
	}
}

func TestHelloSuccess(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t)
	sendHello(t, c, "tok", 0xFFFFFFFF, 0x80000000) // opaque C→S serials

	header, payload := readFrame(t, c)
	if header.Opcode != proto.OpcodeWelcome {
		t.Fatalf("opcode = %d, want 200", header.Opcode)
	}
	if header.MsgVersion != proto.MessageVersion1 {
		t.Errorf("msg_version = %d, want 1", header.MsgVersion)
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
	if got.ServerTimeMs != fixedWelcome.ServerTimeMs || got.AOIRadius != fixedWelcome.AOIRadius {
		t.Errorf("welcome = %+v, want %+v", got, fixedWelcome)
	}
	snap := sessionOf(t, f, "test-sub")
	if snap.State != session.StateAuthenticated {
		t.Errorf("state = %s, want AUTHENTICATED", snap.State)
	}
	if snap.AccountID != 42 {
		t.Errorf("accountID = %d, want provisioned 42", snap.AccountID)
	}
	if !snap.TokenExp.Equal(f.exp1) {
		t.Errorf("tokenExp = %v, want %v", snap.TokenExp, f.exp1)
	}
	// Provisioner saw the validated identity exactly once, with email.
	if n := f.accounts.callCount(); n != 1 {
		t.Fatalf("provisioner calls = %d, want 1", n)
	}
	call := f.accounts.lastCall()
	if call.sub != "test-sub" {
		t.Errorf("provisioned sub = %q", call.sub)
	}
	if call.email == nil || *call.email != "hero@example.com" {
		t.Errorf("provisioned email = %v, want hero@example.com", call.email)
	}
	// The hard deadline is armed at TokenExp + 90 s.
	pending := f.sched.pending()
	if len(pending) != 1 || !pending[0].at.Equal(f.exp1.Add(gateway.ReauthGrace)) {
		t.Errorf("armed deadline = %v, want %v", pending, f.exp1.Add(gateway.ReauthGrace))
	}
}

func TestHelloJWTInvalid(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t)
	sendHello(t, c, "bad", 1, 1)
	_, msg := readError(t, c)
	if msg.Code != proto.ErrorCodeSessionExpired {
		t.Errorf("code = %d, want session_expired", msg.Code)
	}
	// No provisioning attempt for an invalid JWT, no sub index entry,
	// and the session is still CONNECTED (a good hello works after).
	if n := f.accounts.callCount(); n != 0 {
		t.Errorf("provisioner calls = %d, want 0", n)
	}
	if got := f.reg.SessionsBySub("test-sub"); len(got) != 0 {
		t.Errorf("sub index = %v, want empty", got)
	}
	sendHello(t, c, "tok", 2, 2)
	header, _ := readFrame(t, c)
	if header.Opcode != proto.OpcodeWelcome {
		t.Fatalf("opcode after retry = %d, want 200", header.Opcode)
	}
}

func TestHelloProvisioningFailure(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t)
	f.accounts.fail = true
	sendHello(t, c, "tok", 1, 1)
	_, msg := readError(t, c)
	if msg.Code != proto.ErrorCodeRetry {
		t.Fatalf("code = %d, want retry(6), not session_expired", msg.Code)
	}
	// No partial authentication: no sub index entry, and the session
	// still accepts a hello (proves CONNECTED — an AUTHENTICATED
	// session would reject hello with bad_state).
	if got := f.reg.SessionsBySub("test-sub"); len(got) != 0 {
		t.Fatalf("sub index = %v after failed provision, want empty", got)
	}
	f.accounts.fail = false
	sendHello(t, c, "tok", 2, 2)
	header, _ := readFrame(t, c)
	if header.Opcode != proto.OpcodeWelcome {
		t.Fatalf("opcode after recovery = %d, want 200", header.Opcode)
	}
	if snap := sessionOf(t, f, "test-sub"); snap.AccountID != 42 {
		t.Errorf("accountID = %d, want 42", snap.AccountID)
	}
}

func TestHelloMalformedPayload(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t)
	sendFrame(t, c,
		proto.Header{Opcode: proto.OpcodeHello, MsgVersion: 1, Seq: 1, Tick: 1},
		func(e *proto.Encoder) error {
			e.U8(0xFF)
			return nil
		})
	header, msg := readError(t, c)
	if header.MsgVersion != proto.MessageVersion1 {
		t.Errorf("error msg_version = %d, want 1", header.MsgVersion)
	}
	if msg.Code != proto.ErrorCodeProtocol {
		t.Errorf("code = %d, want protocol_error", msg.Code)
	}
	sendHello(t, c, "tok", 2, 2)
	header, _ = readFrame(t, c)
	if header.Opcode != proto.OpcodeWelcome {
		t.Fatalf("opcode after malformed hello = %d, want 200", header.Opcode)
	}
}

// ---------------------------------------------------------------------------
// reauth
// ---------------------------------------------------------------------------

func TestReauthSuccess(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t)
	sendHello(t, c, "tok", 1, 1)
	_, _ = readFrame(t, c)

	sendReauth(t, c, "tok2", 2)
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
	if snap := sessionOf(t, f, "test-sub"); !snap.TokenExp.Equal(f.exp2) {
		t.Errorf("tokenExp = %v, want %v", snap.TokenExp, f.exp2)
	}
	// The deadline moved to the new TokenExp + 90 s.
	pending := f.sched.pending()
	if len(pending) != 1 || !pending[0].at.Equal(f.exp2.Add(gateway.ReauthGrace)) {
		t.Fatalf("armed deadline = %v, want %v", pending, f.exp2.Add(gateway.ReauthGrace))
	}
}

func TestReauthPerformsNoProvisioning(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t)
	sendHello(t, c, "tok", 1, 1)
	_, _ = readFrame(t, c)
	if n := f.accounts.callCount(); n != 1 {
		t.Fatalf("provisioner calls after hello = %d, want 1", n)
	}
	sendReauth(t, c, "tok2", 2)
	header, _ := readFrame(t, c)
	if header.Opcode != proto.OpcodeReauthOK {
		t.Fatalf("opcode = %d, want 201", header.Opcode)
	}
	if n := f.accounts.callCount(); n != 1 {
		t.Errorf("provisioner calls after reauth = %d, want still 1 (no PG on reauth)", n)
	}
}

func TestReauthIdentityMismatch(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t)
	sendHello(t, c, "tok", 1, 1)
	_, _ = readFrame(t, c)
	oldDeadline := f.sched.pending()[0].at

	sendReauth(t, c, "evil", 2)
	_, msg := readError(t, c)
	if msg.Code != proto.ErrorCodeSessionExpired {
		t.Errorf("code = %d, want session_expired", msg.Code)
	}
	snap := sessionOf(t, f, "test-sub")
	if snap.AccountID != 42 || !snap.TokenExp.Equal(f.exp1) {
		t.Errorf("identity mutated by rejected reauth: %+v", snap)
	}
	// The new subject was not provisioned and the old deadline stands.
	if n := f.accounts.callCount(); n != 1 {
		t.Errorf("provisioner calls = %d, want 1 (no auto-provision on mismatch)", n)
	}
	pending := f.sched.pending()
	if len(pending) != 1 || !pending[0].at.Equal(oldDeadline) {
		t.Errorf("deadline changed by rejected reauth: %v, want %v", pending, oldDeadline)
	}
}

func TestReauthValidationFailureKeepsGrace(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t)
	sendHello(t, c, "tok", 1, 1)
	_, _ = readFrame(t, c)

	sendReauth(t, c, "bad", 2)
	_, msg := readError(t, c)
	if msg.Code != proto.ErrorCodeSessionExpired {
		t.Errorf("code = %d, want session_expired", msg.Code)
	}
	if snap := sessionOf(t, f, "test-sub"); !snap.TokenExp.Equal(f.exp1) {
		t.Errorf("tokenExp changed by failed reauth: %v", snap.TokenExp)
	}
	// Remaining grace is intact: an allowed message still dispatches.
	f.clock.Advance(60 * time.Second)
	sendCharList(t, c, 3)
	header, _ := readFrame(t, c)
	if header.Opcode != proto.OpcodeCharacterListResult {
		t.Fatalf("opcode in grace = %d, want 216", header.Opcode)
	}
}

// ---------------------------------------------------------------------------
// lifecycle gate regression (T1 behavior preserved)
// ---------------------------------------------------------------------------

func TestWrongStateKnownOpcode(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t)
	sendFrame(t, c,
		proto.Header{Opcode: proto.OpcodeCharacterCreate, MsgVersion: 1, Seq: 1, Tick: 1},
		func(e *proto.Encoder) error {
			proto.CharacterCreate{Slot: 0, Name: "Bob"}.Encode(e)
			return nil
		})
	_, msg := readError(t, c)
	if msg.Code != proto.ErrorCodeBadState {
		t.Errorf("code = %d, want bad_state", msg.Code)
	}
	if n := f.handler.callCount(); n != 0 {
		t.Errorf("handler called %d times for wrong-state opcode", n)
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
	header, _ := readFrame(t, c)
	if header.Opcode != proto.OpcodeCharacterListResult {
		t.Fatalf("opcode = %d, want 216", header.Opcode)
	}
	if header.Seq != 2 {
		t.Errorf("S→C seq = %d, want 2 (never copies C→S seq)", header.Seq)
	}
	if n := f.handler.callCount(); n != 1 {
		t.Fatalf("handler calls = %d, want 1", n)
	}
	call := f.handler.lastCall()
	if call.header.Opcode != proto.OpcodeCharacterList {
		t.Errorf("handler opcode = %d, want 121", call.header.Opcode)
	}
	if call.header.Seq != 0xFFFFFFFF || call.header.Tick != 0x80000000 {
		t.Errorf("handler saw seq=%#x tick=%#x, want opaque passthrough", call.header.Seq, call.header.Tick)
	}
}

func TestProtocolErrorsKeepConnection(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t)
	// Unknown opcode, S→C-only opcode, text message, short frame: all
	// protocol_error, none fatal.
	sendFrame(t, c,
		proto.Header{Opcode: 65535, MsgVersion: 1, Seq: 1, Tick: 1},
		func(e *proto.Encoder) error { return nil })
	if _, msg := readError(t, c); msg.Code != proto.ErrorCodeProtocol {
		t.Errorf("unknown opcode code = %d, want protocol_error", msg.Code)
	}
	sendFrame(t, c,
		proto.Header{Opcode: proto.OpcodeShopList, MsgVersion: 1, Seq: 2, Tick: 1},
		func(e *proto.Encoder) error { return nil })
	if _, msg := readError(t, c); msg.Code != proto.ErrorCodeProtocol {
		t.Errorf("wrong-direction code = %d, want protocol_error", msg.Code)
	}
	writeMsg(t, c, websocket.MessageText, []byte(`{"no":"json"}`))
	if _, msg := readError(t, c); msg.Code != proto.ErrorCodeProtocol {
		t.Errorf("text code = %d, want protocol_error", msg.Code)
	}
	writeMsg(t, c, websocket.MessageBinary, []byte{1, 2, 3, 4, 5})
	if _, msg := readError(t, c); msg.Code != proto.ErrorCodeProtocol {
		t.Errorf("short frame code = %d, want protocol_error", msg.Code)
	}
	sendHello(t, c, "tok", 3, 3)
	header, _ := readFrame(t, c)
	if header.Opcode != proto.OpcodeWelcome {
		t.Fatalf("opcode after protocol errors = %d, want 200", header.Opcode)
	}
}

func TestHandlerClientErrorKeepsConnection(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t)
	sendHello(t, c, "tok", 1, 1)
	_, _ = readFrame(t, c)

	f.handler.setErr(&gateway.ClientError{Code: proto.ErrorCodeRateLimited, Message: "slow down"})
	sendCharList(t, c, 2)
	_, msg := readError(t, c)
	if msg.Code != proto.ErrorCodeRateLimited {
		t.Errorf("code = %d, want rate_limited", msg.Code)
	}
	f.handler.setErr(nil)
	sendCharList(t, c, 3)
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
	sendCharList(t, c, 2)
	readClosed(t, c)
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
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	_, _, err := c.Read(ctx2)
	if err == nil {
		t.Fatal("expected termination after oversize message")
	}
	if status := websocket.CloseStatus(err); status != websocket.StatusMessageTooBig {
		t.Errorf("close status = %v, want 1009", status)
	}
	waitFor(t, "session cleanup after oversize", func() bool { return f.reg.Len() == 0 })
}

// ---------------------------------------------------------------------------
// hard authorization deadline
// ---------------------------------------------------------------------------

func TestGraceBoundaries(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t)
	sendHello(t, c, "tok", 1, 1)
	_, _ = readFrame(t, c)

	// Just before expiry: normal operation.
	f.clock.Advance(5*time.Minute - time.Second)
	sendCharList(t, c, 2)
	if header, _ := readFrame(t, c); header.Opcode != proto.OpcodeCharacterListResult {
		t.Fatalf("opcode before expiry = %d, want 216", header.Opcode)
	}
	// During grace (T+89s): still dispatched.
	f.clock.Advance(90 * time.Second)
	sendCharList(t, c, 3)
	if header, _ := readFrame(t, c); header.Opcode != proto.OpcodeCharacterListResult {
		t.Fatalf("opcode in grace = %d, want 216", header.Opcode)
	}
	if n := f.handler.callCount(); n != 2 {
		t.Fatalf("handler calls = %d, want 2", n)
	}
	// Exact hard boundary (T+90s): no dispatch, 202, then disconnect.
	f.clock.Advance(time.Second)
	sendCharList(t, c, 4)
	_, msg := readError(t, c)
	if msg.Code != proto.ErrorCodeSessionExpired {
		t.Errorf("code at deadline = %d, want session_expired", msg.Code)
	}
	if n := f.handler.callCount(); n != 2 {
		t.Errorf("handler calls at deadline = %d, want still 2", n)
	}
	readClosed(t, c)
	waitFor(t, "session cleanup at deadline", func() bool { return f.reg.Len() == 0 })
}

func TestReauthReplacesDeadlineAndStaleTimerCannotKill(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t)
	sendHello(t, c, "tok", 1, 1)
	_, _ = readFrame(t, c)
	oldTimer := f.sched.pending()[0]

	// Reauth at T+60s with a fresh token.
	f.clock.Advance(60 * time.Second)
	sendReauth(t, c, "tok2", 2)
	if header, _ := readFrame(t, c); header.Opcode != proto.OpcodeReauthOK {
		t.Fatalf("reauth in grace failed")
	}
	if snap := sessionOf(t, f, "test-sub"); !snap.TokenExp.Equal(f.exp2) {
		t.Fatalf("tokenExp = %v, want %v", snap.TokenExp, f.exp2)
	}
	// Manually fire the stale superseded callback: the connection must
	// survive with its new deadline intact.
	oldTimer.fn()
	sendCharList(t, c, 3)
	if header, _ := readFrame(t, c); header.Opcode != proto.OpcodeCharacterListResult {
		t.Fatalf("opcode after stale timer = %d, want 216 (connection must survive)", header.Opcode)
	}
	pending := f.sched.pending()
	if len(pending) != 1 || !pending[0].at.Equal(f.exp2.Add(gateway.ReauthGrace)) {
		t.Errorf("new deadline = %v, want %v", pending, f.exp2.Add(gateway.ReauthGrace))
	}
}

func TestReauthAfterHardDeadlineIsTooLate(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t)
	sendHello(t, c, "tok", 1, 1)
	_, _ = readFrame(t, c)
	callsBefore := f.validator.callCount()

	f.clock.Advance(5*time.Minute + 90*time.Second)
	sendReauth(t, c, "tok2", 2)
	_, msg := readError(t, c)
	if msg.Code != proto.ErrorCodeSessionExpired {
		t.Errorf("code = %d, want session_expired", msg.Code)
	}
	// The validator was not consulted to rescue the session.
	if n := f.validator.callCount(); n != callsBefore {
		t.Errorf("validator calls = %d, want %d (no rescue after deadline)", n, callsBefore)
	}
	readClosed(t, c)
	waitFor(t, "session cleanup", func() bool { return f.reg.Len() == 0 })
}

func TestIdleConnectionTerminatedAtDeadline(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t)
	sendHello(t, c, "tok", 1, 1)
	_, _ = readFrame(t, c)

	// Time passes with no inbound traffic; the scheduled callback fires.
	f.clock.Advance(5*time.Minute + 91*time.Second)
	pending := f.sched.pending()
	if len(pending) != 1 {
		t.Fatalf("pending deadlines = %d, want 1", len(pending))
	}
	pending[0].fn()
	_, msg := readError(t, c)
	if msg.Code != proto.ErrorCodeSessionExpired {
		t.Errorf("idle deadline code = %d, want session_expired", msg.Code)
	}
	readClosed(t, c)
	waitFor(t, "session cleanup after idle deadline", func() bool { return f.reg.Len() == 0 })
}

func TestDisconnectCancelsDeadline(t *testing.T) {
	f := newFixture(t)
	c := f.dial(t)
	sendHello(t, c, "tok", 1, 1)
	_, _ = readFrame(t, c)
	if n := len(f.sched.pending()); n != 1 {
		t.Fatalf("pending deadlines = %d, want 1", n)
	}
	if err := c.Close(websocket.StatusNormalClosure, "bye"); err != nil {
		t.Fatalf("close: %v", err)
	}
	waitFor(t, "session cleanup", func() bool { return f.reg.Len() == 0 })
	if n := len(f.sched.pending()); n != 0 {
		t.Errorf("pending deadlines after disconnect = %d, want 0 (timer cancelled)", n)
	}
}

// ---------------------------------------------------------------------------
// real validator + real PG integration
// ---------------------------------------------------------------------------

type realWorld struct {
	priv *rsa.PrivateKey
	set  jwk.Set
	hits *atomic.Int32
	jwks *httptest.Server
	now  time.Time
}

func newRealWorld(t *testing.T) *realWorld {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pub, err := jwk.PublicKeyOf(priv)
	if err != nil {
		t.Fatalf("public JWK: %v", err)
	}
	if err := pub.Set(jwk.KeyIDKey, "real-kid"); err != nil {
		t.Fatalf("set kid: %v", err)
	}
	if err := pub.Set(jwk.AlgorithmKey, jwa.RS256()); err != nil {
		t.Fatalf("set alg: %v", err)
	}
	set := jwk.NewSet()
	if err := set.AddKey(pub); err != nil {
		t.Fatalf("add key: %v", err)
	}
	w := &realWorld{
		priv: priv,
		set:  set,
		hits: &atomic.Int32{},
		now:  time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC),
	}
	w.jwks = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		w.hits.Add(1)
		rw.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(rw).Encode(set); err != nil {
			t.Errorf("serve JWKS: %v", err)
		}
	}))
	t.Cleanup(w.jwks.Close)
	return w
}

func (w *realWorld) mint(t *testing.T, key *rsa.PrivateKey, kid, sub, email string, exp time.Time) string {
	t.Helper()
	tok := jwt.New()
	_ = tok.Set(jwt.IssuerKey, "https://keycloak.test/realms/vox")
	_ = tok.Set(jwt.AudienceKey, []string{"voxilian"})
	_ = tok.Set(jwt.SubjectKey, sub)
	_ = tok.Set(jwt.ExpirationKey, exp)
	_ = tok.Set(jwt.IssuedAtKey, w.now)
	if email != "" {
		_ = tok.Set("email", email)
	}
	hdrs := jws.NewHeaders()
	_ = hdrs.Set(jws.KeyIDKey, kid)
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256(), key, jws.WithProtectedHeaders(hdrs)))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return string(signed)
}

func openProvisionedStore(t *testing.T) (*store.PGStore, *pgxpool.Pool) {
	t.Helper()
	pg := simtest.StartPostgres18(t)
	sqldb, err := sql.Open("pgx", pg.DSN)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sqldb.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(simtest.RepoRoot(t), "backend", "voxilian", "migrations")
	if err := goose.UpTo(sqldb, dir, 5); err != nil {
		t.Fatalf("migrate to 5: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), pg.DSN)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	st, err := store.New(pool, nil)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return st, pool
}

type realFixture struct {
	reg   *session.Registry
	clock *simtest.Clock
	sched *manualScheduler
	ts    *httptest.Server
	st    *store.PGStore
	pool  *pgxpool.Pool
	world *realWorld
}

func newRealFixture(t *testing.T) *realFixture {
	t.Helper()
	w := newRealWorld(t)
	clock := simtest.NewClock(w.now)
	validator, err := auth.NewJWKSValidator(context.Background(), auth.ValidatorConfig{
		Issuer:   "https://keycloak.test/realms/vox",
		Audience: "voxilian",
		JWKSURL:  w.jwks.URL,
		Clock:    clock,
	})
	if err != nil {
		t.Fatalf("NewJWKSValidator: %v", err)
	}
	st, pool := openProvisionedStore(t)
	f := &realFixture{
		reg:   session.NewRegistry(),
		clock: clock,
		sched: &manualScheduler{},
		st:    st,
		pool:  pool,
		world: w,
	}
	srv := gateway.NewServer(gateway.ServerDeps{
		Registry:  f.reg,
		Validator: validator,
		Accounts:  st, // *store.PGStore satisfies AccountProvisioner structurally.
		Welcome:   func(context.Context) proto.Welcome { return fixedWelcome },
		Tick:      func() uint32 { return testTick },
		Now:       clock.Now,
		Schedule:  f.sched.schedule,
		Handler:   &recordHandler{autoReply: true},
	})
	f.ts = httptest.NewServer(srv)
	t.Cleanup(f.ts.Close)
	return f
}

func (f *realFixture) dial(t *testing.T) *websocket.Conn {
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

func (f *realFixture) accountCount(t *testing.T, sub string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM accounts WHERE keycloak_sub = $1`, sub).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestFullHelloChainJWKSAndPG(t *testing.T) {
	f := newRealFixture(t)
	c := f.dial(t)
	exp := f.world.now.Add(5 * time.Minute)
	token := f.world.mint(t, f.world.priv, "real-kid", "hero-sub", "hero@example.com", exp)
	sendHello(t, c, token, 1, 1)

	header, _ := readFrame(t, c)
	if header.Opcode != proto.OpcodeWelcome {
		t.Fatalf("opcode = %d, want 200", header.Opcode)
	}
	ids := f.reg.SessionsBySub("hero-sub")
	if len(ids) != 1 {
		t.Fatalf("sub index = %v, want one session", ids)
	}
	snap, _ := f.reg.Get(ids[0])
	if snap.State != session.StateAuthenticated {
		t.Errorf("state = %s, want AUTHENTICATED", snap.State)
	}
	if snap.AccountID <= 0 {
		t.Errorf("accountID = %d, want provisioned positive ID", snap.AccountID)
	}
	if !snap.TokenExp.Equal(exp) {
		t.Errorf("tokenExp = %v, want JWT exp %v", snap.TokenExp, exp)
	}
	var sub string
	var em sql.NullString
	if err := f.pool.QueryRow(context.Background(),
		`SELECT keycloak_sub, email FROM accounts WHERE id = $1`, snap.AccountID).Scan(&sub, &em); err != nil {
		t.Fatalf("durable account: %v", err)
	}
	if sub != "hero-sub" {
		t.Errorf("durable sub = %q", sub)
	}
	if !em.Valid || em.String != "hero@example.com" {
		t.Errorf("durable email = %+v, want hero@example.com", em)
	}
	if n := f.world.hits.Load(); n != 1 {
		t.Errorf("JWKS hits = %d, want exactly 1 (fetch at construction only)", n)
	}
	// Reauth over the same connection uses the same durable mapping.
	reauthExp := f.world.now.Add(10 * time.Minute)
	sendReauth(t, c, f.world.mint(t, f.world.priv, "real-kid", "hero-sub", "", reauthExp), 2)
	if header, _ := readFrame(t, c); header.Opcode != proto.OpcodeReauthOK {
		t.Fatalf("reauth opcode = %d, want 201", header.Opcode)
	}
	if snap, _ := f.reg.Get(ids[0]); !snap.TokenExp.Equal(reauthExp) {
		t.Errorf("tokenExp after reauth = %v, want %v", snap.TokenExp, reauthExp)
	}
}

func TestForgedTokenGateway(t *testing.T) {
	f := newRealFixture(t)
	c := f.dial(t)
	evil, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate evil key: %v", err)
	}
	sendHello(t, c, f.world.mint(t, evil, "real-kid", "hero-sub", "", f.world.now.Add(time.Hour)), 1, 1)
	_, msg := readError(t, c)
	if msg.Code != proto.ErrorCodeSessionExpired {
		t.Errorf("code = %d, want session_expired", msg.Code)
	}
	if n := f.accountCount(t, "hero-sub"); n != 0 {
		t.Errorf("account rows = %d, want 0 (forged token provisions nothing)", n)
	}
	// Session is still CONNECTED: a valid hello works on the same socket.
	sendHello(t, c, f.world.mint(t, f.world.priv, "real-kid", "hero-sub", "", f.world.now.Add(time.Hour)), 2, 2)
	if header, _ := readFrame(t, c); header.Opcode != proto.OpcodeWelcome {
		t.Fatalf("opcode after forged retry = %d, want 200", header.Opcode)
	}
}

func TestExpiredTokenGateway(t *testing.T) {
	f := newRealFixture(t)
	c := f.dial(t)
	sendHello(t, c, f.world.mint(t, f.world.priv, "real-kid", "hero-sub", "", f.world.now.Add(-time.Minute)), 1, 1)
	_, msg := readError(t, c)
	if msg.Code != proto.ErrorCodeSessionExpired {
		t.Errorf("code = %d, want session_expired", msg.Code)
	}
	if n := f.accountCount(t, "hero-sub"); n != 0 {
		t.Errorf("account rows = %d, want 0", n)
	}
	if got := f.reg.SessionsBySub("hero-sub"); len(got) != 0 {
		t.Errorf("sub index = %v, want empty", got)
	}
}
