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
	"github.com/dlukt/voxilian/internal/character"
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

func (f *fakeAccounts) setFail(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = v
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
	f.accounts.setFail(true)
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
	f.accounts.setFail(false)
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

// ---------------------------------------------------------------------------
// character WS + PG integration (M3-T3b)
// ---------------------------------------------------------------------------

// Test-only stable IDs for the integration content profile. Explicitly
// not canonical game IDs; production invents none.
const (
	intSpellFree = 100
	intSpellPaid = 101
	intSkillPaid = 201
	intItemMace  = 9001
	intItemCoins = 9002
)

type wsExitFake struct {
	mu       sync.Mutex
	fail     bool
	calls    int
	observed []session.Snapshot
	reg      *session.Registry
}

func (f *wsExitFake) ExitWorld(
	_ context.Context,
	_ session.ID,
	_ int64,
	characterID int64,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if snap, ok := f.reg.GetByCharacter(characterID); ok {
		f.observed = append(f.observed, snap)
	}
	if f.fail {
		return errors.New("flush failed")
	}
	return nil
}

func (f *wsExitFake) setFail(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = v
}

func (f *wsExitFake) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *wsExitFake) observedCopy() []session.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]session.Snapshot(nil), f.observed...)
}

type wsNextFake struct {
	mu      sync.Mutex
	calls   int
	headers []proto.Header
	sids    []session.ID
	marker  bool // when true, reply 219 to prove delegation + SendFunc
}

func (n *wsNextFake) callCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.calls
}

func (n *wsNextFake) Handle(
	_ context.Context,
	sid session.ID,
	header proto.Header,
	_ *proto.Decoder,
	send gateway.SendFunc,
) error {
	n.mu.Lock()
	n.calls++
	n.headers = append(n.headers, header)
	n.sids = append(n.sids, sid)
	marker := n.marker
	n.mu.Unlock()
	if !marker {
		return nil
	}
	return send(proto.OpcodeWorldReady, proto.MessageVersion1, func(e *proto.Encoder) error {
		proto.WorldReady{}.Encode(e)
		return nil
	})
}

type charFixture struct {
	reg   *session.Registry
	clock *simtest.Clock
	sched *manualScheduler
	ts    *httptest.Server
	st    *store.PGStore
	pool  *pgxpool.Pool
	svc   *character.Service
	exit  *wsExitFake
	next  *wsNextFake
	t0    time.Time
	exp1  time.Time
}

func charStrPtr(s string) *string { return &s }

func newCharFixture(t *testing.T, nextMarker bool, baseline gateway.BaselineProvider) *charFixture {
	t.Helper()
	t0 := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	f := &charFixture{
		reg:   session.NewRegistry(),
		clock: simtest.NewClock(t0),
		sched: &manualScheduler{},
		t0:    t0,
		exp1:  t0.Add(5 * time.Minute),
	}
	st, pool := openProvisionedStore(t)
	f.st = st
	f.pool = pool
	ctx := context.Background()
	if err := st.UpsertCatalogBatch(ctx, store.CatalogBatch{
		Spells: []store.SpellProtoRecord{
			{ID: intSpellFree, School: 1, Level: 1, Mana: 5, Exertion: 1, CastMs: 100, MinHP: 1,
				Reagents: json.RawMessage(`{}`), Params: json.RawMessage(`{}`), Version: 1},
			{ID: intSpellPaid, School: 1, Level: 1, Mana: 5, Exertion: 1, CastMs: 100, MinHP: 1,
				Reagents: json.RawMessage(`{}`), Params: json.RawMessage(`{}`), Version: 1},
		},
		Skills: []store.SkillProtoRecord{
			{ID: intSkillPaid, Division: 1, Level: 1, Exertion: 1,
				Params: json.RawMessage(`{}`), Version: 1},
		},
		Items: []store.ItemProtoRecord{
			{ID: intItemMace, Kind: 0, Slot: charStrPtr("hand"), Base: json.RawMessage(`{}`), Version: 1},
			{ID: intItemCoins, Kind: 0, Slot: charStrPtr("coins"), Base: json.RawMessage(`{}`), Version: 1},
		},
	}, false); err != nil {
		t.Fatalf("seed catalog: %v", err)
	}
	content := character.StaticContent{
		Spells: map[uint16]character.AbilitySpec{
			intSpellFree: {ID: intSpellFree, Level: 1, Offered: true, InitialAbility: 50, School: 1},
			intSpellPaid: {ID: intSpellPaid, Level: 1, Offered: true, InitialAbility: 30, School: 1},
		},
		Skills: map[uint16]character.AbilitySpec{
			intSkillPaid: {ID: intSkillPaid, Level: 1, Offered: true, InitialAbility: 25},
		},
		Profile: character.StarterProfile{
			Spells: []character.AbilitySpec{
				{ID: intSpellFree, Level: 1, Offered: true, InitialAbility: 50, School: 1},
			},
			Items: []character.StarterItem{
				{ProtoID: intItemMace, Qty: 1, Hits: 100, Slot: "hand"},
				{ProtoID: intItemCoins, Qty: 500, Hits: 0, Slot: "coins"},
			},
			Hometown: "tos", PosX: 1, PosY: 2, PosZ: 3,
		},
	}
	svc, err := character.NewService(content,
		character.NewNamePolicy([]string{"goblin"}, []string{"admin"}), st)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	f.svc = svc
	f.exit = &wsExitFake{reg: f.reg}
	f.next = &wsNextFake{marker: nextMarker}
	validator := &fakeValidator{
		ids: map[string]auth.Identity{
			"tok": {Sub: "test-sub", ExpiresAt: f.exp1},
		},
		now: f.clock.Now,
	}
	chars, err := gateway.NewCharacterHandler(svc, f.reg, gateway.WorldExitFunc(f.exit.ExitWorld), f.next)
	if err != nil {
		t.Fatalf("NewCharacterHandler: %v", err)
	}
	// T4 chain when a baseline provider is supplied: CharacterHandler
	// owns 121/122/123/126 and delegates 124 to EnterWorldHandler,
	// which delegates the rest to the recording Next. Both handlers
	// share ONE WorldExit instance: normal leave_world and forced
	// takeover are the same world-side quiesce/flush contract.
	var top gateway.MessageHandler = chars
	if baseline != nil {
		exit := gateway.WorldExitFunc(f.exit.ExitWorld)
		enter, err := gateway.NewEnterWorldHandler(gateway.EnterWorldHandlerDeps{
			Characters: svc,
			Registry:   f.reg,
			Baseline:   baseline,
			WorldExit:  exit,
			Tick:       func() uint32 { return testTick },
			Next:       f.next,
		})
		if err != nil {
			t.Fatalf("NewEnterWorldHandler: %v", err)
		}
		top, err = gateway.NewCharacterHandler(svc, f.reg, exit, enter)
		if err != nil {
			t.Fatalf("NewCharacterHandler: %v", err)
		}
	}
	srv := gateway.NewServer(gateway.ServerDeps{
		Registry:  f.reg,
		Validator: validator,
		Accounts:  st,
		Welcome:   func(context.Context) proto.Welcome { return fixedWelcome },
		Tick:      func() uint32 { return testTick },
		Now:       f.clock.Now,
		Schedule:  f.sched.schedule,
		Handler:   top,
	})
	f.ts = httptest.NewServer(srv)
	t.Cleanup(f.ts.Close)
	return f
}

func (f *charFixture) dial(t *testing.T) *websocket.Conn {
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

// helloChar performs hello and returns the provisioned account ID.
func (f *charFixture) helloChar(t *testing.T, c *websocket.Conn) int64 {
	t.Helper()
	sendHello(t, c, "tok", 1, 1)
	header, _ := readFrame(t, c)
	if header.Opcode != proto.OpcodeWelcome {
		t.Fatalf("opcode = %d, want 200", header.Opcode)
	}
	ids := f.reg.SessionsBySub("test-sub")
	if len(ids) == 0 {
		t.Fatal("no session indexed")
	}
	snap, _ := f.reg.Get(ids[len(ids)-1])
	return snap.AccountID
}

func validCharCreate(slot uint8, name string) proto.CharacterCreate {
	return proto.CharacterCreate{
		Slot:   slot,
		Name:   name,
		Gender: 1,
		Face:   proto.CharacterFace{HairStyle: 1, HairColor: 2, SkinTone: 3, Parts: [5]uint8{1, 2, 3, 4, 5}},
		Stats:  [6]uint8{10, 10, 10, 10, 10, 10},
		Spells: []uint16{intSpellPaid},
		Skills: []uint16{intSkillPaid},
	}
}

func sendCreate(t *testing.T, c *websocket.Conn, seq uint32, req proto.CharacterCreate) {
	t.Helper()
	sendFrame(t, c,
		proto.Header{Opcode: proto.OpcodeCharacterCreate, MsgVersion: 1, Seq: seq, Tick: 1},
		func(e *proto.Encoder) error {
			req.Encode(e)
			return nil
		})
}

func sendDelete(t *testing.T, c *websocket.Conn, seq uint32, slot uint8) {
	t.Helper()
	sendFrame(t, c,
		proto.Header{Opcode: proto.OpcodeCharacterDelete, MsgVersion: 1, Seq: seq, Tick: 1},
		func(e *proto.Encoder) error {
			proto.CharacterDelete{Slot: slot}.Encode(e)
			return nil
		})
}

func sendLeave(t *testing.T, c *websocket.Conn, seq uint32) {
	t.Helper()
	sendFrame(t, c,
		proto.Header{Opcode: proto.OpcodeLeaveWorld, MsgVersion: 1, Seq: seq, Tick: 1},
		func(e *proto.Encoder) error {
			proto.LeaveWorld{}.Encode(e)
			return nil
		})
}

func sendEnterWorld(t *testing.T, c *websocket.Conn, seq uint32, slot uint8) {
	t.Helper()
	sendFrame(t, c,
		proto.Header{Opcode: proto.OpcodeEnterWorld, MsgVersion: 1, Seq: seq, Tick: 1},
		func(e *proto.Encoder) error {
			proto.EnterWorld{Slot: slot}.Encode(e)
			return nil
		})
}

func readCharOp(t *testing.T, c *websocket.Conn) (proto.Header, proto.CharacterOp) {
	t.Helper()
	header, payload := readFrame(t, c)
	if header.Opcode != proto.OpcodeCharacterOp {
		t.Fatalf("opcode = %d, want 217", header.Opcode)
	}
	if header.MsgVersion != proto.MessageVersion1 {
		t.Fatalf("msg_version = %d, want 1", header.MsgVersion)
	}
	op, err := proto.DecodeCharacterOp(payload)
	if err != nil {
		t.Fatalf("decode 217: %v", err)
	}
	return header, op
}

func readCharList(t *testing.T, c *websocket.Conn) (proto.Header, proto.CharacterList) {
	t.Helper()
	header, payload := readFrame(t, c)
	if header.Opcode != proto.OpcodeCharacterListResult {
		t.Fatalf("opcode = %d, want 216", header.Opcode)
	}
	if header.MsgVersion != proto.MessageVersion1 {
		t.Fatalf("msg_version = %d, want 1", header.MsgVersion)
	}
	list, err := proto.DecodeCharacterList(payload)
	if err != nil {
		t.Fatalf("decode 216: %v", err)
	}
	return header, list
}

func (f *charFixture) liveCount(t *testing.T, accountID int64) int {
	t.Helper()
	rows, err := f.st.ListLiveCharacters(context.Background(), accountID)
	if err != nil {
		t.Fatalf("list live: %v", err)
	}
	return len(rows)
}

// TestCharCRUDLiveSequence is the end-to-end M3 proof: hello, empty
// list, create, listed level 20, delete, empty list — with S→C seq and
// msg_version asserted on every reply.
func TestCharCRUDLiveSequence(t *testing.T) {
	f := newCharFixture(t, false, nil)
	c := f.dial(t)
	accountID := f.helloChar(t, c)

	sendCharList(t, c, 2)
	h216a, list := readCharList(t, c)
	if len(list.Characters) != 0 {
		t.Fatalf("initial list = %+v, want empty", list.Characters)
	}
	if h216a.Seq != 2 {
		t.Errorf("216 seq = %d, want 2", h216a.Seq)
	}

	sendCreate(t, c, 3, validCharCreate(0, "Aria"))
	h217, op := readCharOp(t, c)
	if op.Op != proto.CharacterOpCreate || op.OK != proto.CharacterOpOK {
		t.Fatalf("217 = %+v, want create/ok", op)
	}
	if h217.Seq != 3 {
		t.Errorf("217 seq = %d, want 3", h217.Seq)
	}

	sendCharList(t, c, 4)
	h216b, list := readCharList(t, c)
	if h216b.Seq != 4 {
		t.Errorf("216 seq = %d, want 4", h216b.Seq)
	}
	if len(list.Characters) != 1 {
		t.Fatalf("list = %+v, want one row", list.Characters)
	}
	got := list.Characters[0]
	if got.Slot != 0 || got.CharName != "Aria" || got.Level != 20 {
		t.Errorf("row = %+v, want slot 0 Aria level 20", got)
	}

	sendDelete(t, c, 5, 0)
	h217d, op := readCharOp(t, c)
	if op.Op != proto.CharacterOpDelete || op.OK != proto.CharacterOpOK {
		t.Fatalf("217 = %+v, want delete/ok", op)
	}
	if h217d.Seq != 5 {
		t.Errorf("217 seq = %d, want 5", h217d.Seq)
	}

	sendCharList(t, c, 6)
	h216c, list := readCharList(t, c)
	if h216c.Seq != 6 {
		t.Errorf("216 seq = %d, want 6", h216c.Seq)
	}
	if len(list.Characters) != 0 {
		t.Errorf("final list = %+v, want empty", list.Characters)
	}
	// Durable soft-delete behind the wire.
	if n := f.liveCount(t, accountID); n != 0 {
		t.Errorf("live rows = %d, want 0", n)
	}
	var deleted bool
	if err := f.pool.QueryRow(context.Background(),
		`SELECT deleted_at IS NOT NULL FROM characters WHERE account_id = $1`, accountID).Scan(&deleted); err != nil {
		t.Fatalf("durable row: %v", err)
	}
	if !deleted {
		t.Error("durable row not soft-deleted")
	}
}

// TestCharCreateMappingWS proves every create mapping over a real
// socket; each rejection leaves the connection usable for the next.
func TestCharCreateMappingWS(t *testing.T) {
	f := newCharFixture(t, false, nil)
	c := f.dial(t)
	f.helloChar(t, c)

	badStats := validCharCreate(0, "Aria")
	badStats.Stats = [6]uint8{50, 50, 50, 50, 50, 50}
	badBudget := validCharCreate(0, "Aria")
	badBudget.Spells = []uint16{999}
	cases := []struct {
		name   string
		req    proto.CharacterCreate
		op     *proto.CharacterOp // nil → expect 202
		err204 uint16
	}{
		{"invalid slot", validCharCreate(7, "Aria"),
			&proto.CharacterOp{Op: proto.CharacterOpCreate, OK: proto.CharacterOpRejected}, 0},
		{"invalid name", validCharCreate(0, "ab"),
			&proto.CharacterOp{Op: proto.CharacterOpCreate, OK: proto.CharacterOpRejected}, 0},
		{"blocked name", validCharCreate(0, "goblin"), nil, proto.ErrorCodeNameTaken},
		{"bad stats", badStats, nil, proto.ErrorCodeBadStats},
		{"bad budget", badBudget, nil, proto.ErrorCodeBadBudget},
	}
	var seq uint32 = 2
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sendCreate(t, c, seq, tc.req)
			seq++
			if tc.op != nil {
				_, op := readCharOp(t, c)
				if op != *tc.op {
					t.Errorf("217 = %+v, want %+v", op, *tc.op)
				}
				return
			}
			_, msg := readError(t, c)
			if msg.Code != tc.err204 {
				t.Errorf("code = %d, want %d", msg.Code, tc.err204)
			}
		})
	}
	// Usable afterward: a valid create succeeds on the same socket.
	sendCreate(t, c, seq, validCharCreate(0, "Aria"))
	seq++
	if _, op := readCharOp(t, c); op.OK != proto.CharacterOpOK {
		t.Fatalf("217 = %+v after rejections", op)
	}
	// Same live name, free slot → name_taken.
	sendCreate(t, c, seq, validCharCreate(1, "Aria"))
	seq++
	if _, msg := readError(t, c); msg.Code != proto.ErrorCodeNameTaken {
		t.Errorf("duplicate name code = %d, want name_taken", msg.Code)
	}
	// Occupied slot, fresh name → slot_occupied.
	sendCreate(t, c, seq, validCharCreate(0, "Bram"))
	seq++
	if _, msg := readError(t, c); msg.Code != proto.ErrorCodeSlotOccupied {
		t.Errorf("occupied slot code = %d, want slot_occupied", msg.Code)
	}
}

// TestCharMalformedWS proves truncated 122/123 payloads (valid outer
// frames) yield protocol errors without disconnecting.
func TestCharMalformedWS(t *testing.T) {
	f := newCharFixture(t, false, nil)
	c := f.dial(t)
	f.helloChar(t, c)

	sendFrame(t, c,
		proto.Header{Opcode: proto.OpcodeCharacterCreate, MsgVersion: 1, Seq: 2, Tick: 1},
		func(e *proto.Encoder) error {
			e.U8(0)
			e.U8(1)
			e.U8(2)
			return nil
		})
	if _, msg := readError(t, c); msg.Code != proto.ErrorCodeProtocol {
		t.Errorf("122 code = %d, want protocol_error", msg.Code)
	}
	sendFrame(t, c,
		proto.Header{Opcode: proto.OpcodeCharacterDelete, MsgVersion: 1, Seq: 3, Tick: 1},
		func(e *proto.Encoder) error { return nil })
	if _, msg := readError(t, c); msg.Code != proto.ErrorCodeProtocol {
		t.Errorf("123 code = %d, want protocol_error", msg.Code)
	}
	sendCharList(t, c, 4)
	if _, list := readCharList(t, c); len(list.Characters) != 0 {
		t.Errorf("list after malformed = %+v", list.Characters)
	}
}

// TestCharDeleteInUseWS builds two sessions for one account, parks one
// in CHARACTER_SELECTED then IN_WORLD via the registry's real lifecycle
// primitives, and proves deletion from the other socket reports in-use
// both times.
func TestCharDeleteInUseWS(t *testing.T) {
	f := newCharFixture(t, false, nil)
	c1 := f.dial(t)
	accountID := f.helloChar(t, c1)
	sid1 := f.reg.SessionsBySub("test-sub")[0]
	sendCreate(t, c1, 2, validCharCreate(0, "Aria"))
	if _, op := readCharOp(t, c1); op.OK != proto.CharacterOpOK {
		t.Fatalf("create = %+v", op)
	}
	rows, err := f.st.ListLiveCharacters(context.Background(), accountID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("live = %+v, %v", rows, err)
	}
	charID := rows[0].ID

	c2 := f.dial(t)
	f.helloChar(t, c2)
	// The owner (bound session) must be c1's session, identified
	// deterministically: SessionsBySub order is random, and binding c2's
	// own session would make the delete correctly report bad_state
	// instead of character_in_use.
	if ids := f.reg.SessionsBySub("test-sub"); len(ids) != 2 {
		t.Fatalf("sessions = %d, want 2", len(ids))
	}
	owner := sid1
	if err := f.reg.BeginEnterWorld(owner, charID); err != nil {
		t.Fatalf("begin enter: %v", err)
	}
	sendDelete(t, c2, 3, 0)
	if _, msg := readError(t, c2); msg.Code != proto.ErrorCodeCharacterInUse {
		t.Fatalf("delete in CHARACTER_SELECTED: code = %d, want character_in_use", msg.Code)
	}
	if err := f.reg.CompleteEnterWorld(owner, charID); err != nil {
		t.Fatalf("complete enter: %v", err)
	}
	sendDelete(t, c2, 4, 0)
	if _, msg := readError(t, c2); msg.Code != proto.ErrorCodeCharacterInUse {
		t.Fatalf("delete in IN_WORLD: code = %d, want character_in_use", msg.Code)
	}
}

// TestCharLeaveWorldWS proves a silent successful leave: the next S→C
// frame after 126 is the 121-driven 216, and the registry is clean.
func TestCharLeaveWorldWS(t *testing.T) {
	f := newCharFixture(t, false, nil)
	c := f.dial(t)
	accountID := f.helloChar(t, c)
	sendCreate(t, c, 2, validCharCreate(0, "Aria"))
	if _, op := readCharOp(t, c); op.OK != proto.CharacterOpOK {
		t.Fatalf("create = %+v", op)
	}
	rows, err := f.st.ListLiveCharacters(context.Background(), accountID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("live = %+v, %v", rows, err)
	}
	charID := rows[0].ID
	ids := f.reg.SessionsBySub("test-sub")
	sid := ids[0]
	// Real lifecycle primitives (the only legal IN_WORLD path — they
	// also initialize the ACK-flow epoch atomically).
	if err := f.reg.BeginEnterWorld(sid, charID); err != nil {
		t.Fatalf("begin enter: %v", err)
	}
	if err := f.reg.CompleteEnterWorld(sid, charID); err != nil {
		t.Fatalf("complete enter: %v", err)
	}

	sendLeave(t, c, 3)
	sendCharList(t, c, 4)
	// No hidden leave frame: the very next reply is the 216.
	if _, list := readCharList(t, c); len(list.Characters) != 1 {
		t.Errorf("list after leave = %+v", list.Characters)
	}
	snap, _ := f.reg.Get(sid)
	if snap.State != session.StateAuthenticated || snap.HasCharacter || snap.CharacterID != 0 {
		t.Errorf("after leave: %+v", snap)
	}
	if _, ok := f.reg.SessionByCharacter(charID); ok {
		t.Error("character index still present")
	}
	if f.exit.callCount() != 1 {
		t.Fatalf("WorldExit calls = %d, want 1", f.exit.callCount())
	}
	ob := f.exit.observedCopy()[0]
	if ob.State != session.StateInWorld || !ob.HasCharacter || ob.CharacterID != charID {
		t.Errorf("exit observed %+v, want IN_WORLD bound", ob)
	}
}

// TestCharLeaveWorldFailureWS proves a failed flush keeps the session
// fully bound, and a later retry succeeds.
func TestCharLeaveWorldFailureWS(t *testing.T) {
	f := newCharFixture(t, false, nil)
	c := f.dial(t)
	accountID := f.helloChar(t, c)
	sendCreate(t, c, 2, validCharCreate(0, "Aria"))
	if _, op := readCharOp(t, c); op.OK != proto.CharacterOpOK {
		t.Fatalf("create = %+v", op)
	}
	rows, err := f.st.ListLiveCharacters(context.Background(), accountID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("live = %+v, %v", rows, err)
	}
	charID := rows[0].ID
	sid := f.reg.SessionsBySub("test-sub")[0]
	// Real lifecycle primitives (the only legal IN_WORLD path — they
	// also initialize the ACK-flow epoch atomically).
	if err := f.reg.BeginEnterWorld(sid, charID); err != nil {
		t.Fatalf("begin enter: %v", err)
	}
	if err := f.reg.CompleteEnterWorld(sid, charID); err != nil {
		t.Fatalf("complete enter: %v", err)
	}

	f.exit.setFail(true)
	sendLeave(t, c, 3)
	if _, msg := readError(t, c); msg.Code != proto.ErrorCodeRetry {
		t.Fatalf("code = %d, want retry", msg.Code)
	}
	snap, _ := f.reg.Get(sid)
	if snap.State != session.StateInWorld || !snap.HasCharacter || snap.CharacterID != charID {
		t.Errorf("partial leave: %+v", snap)
	}
	if owner, ok := f.reg.SessionByCharacter(charID); !ok || owner != sid {
		t.Errorf("index = %d,%v", owner, ok)
	}

	f.exit.setFail(false)
	sendLeave(t, c, 4)
	sendCharList(t, c, 5)
	if _, list := readCharList(t, c); len(list.Characters) != 1 {
		t.Errorf("list after retry = %+v", list.Characters)
	}
}

// TestEnterWorldDelegatedWS guards the T3/T4 boundary: 124 reaches Next
// exactly once with SendFunc intact, while T3b emits nothing itself.
func TestEnterWorldDelegatedWS(t *testing.T) {
	f := newCharFixture(t, true, nil)
	c := f.dial(t)
	f.helloChar(t, c)

	sendEnterWorld(t, c, 2, 0)
	header, payload := readFrame(t, c)
	if header.Opcode != proto.OpcodeWorldReady {
		t.Fatalf("opcode = %d, want Next marker 219 (no T3b 217)", header.Opcode)
	}
	if _, err := proto.DecodeWorldReady(payload); err != nil {
		t.Fatalf("decode marker: %v", err)
	}
	f.next.mu.Lock()
	defer f.next.mu.Unlock()
	if f.next.calls != 1 {
		t.Fatalf("Next calls = %d, want exactly 1", f.next.calls)
	}
	if f.next.headers[0].Opcode != proto.OpcodeEnterWorld {
		t.Errorf("delegated opcode = %d, want 124 unchanged", f.next.headers[0].Opcode)
	}
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM characters`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("characters = %d, want 0 (no T3b service operation)", n)
	}
	ids := f.reg.SessionsBySub("test-sub")
	snap, _ := f.reg.Get(ids[0])
	if snap.State != session.StateAuthenticated || snap.HasCharacter {
		t.Errorf("session changed by 124: %+v", snap)
	}
}

// ---------------------------------------------------------------------------
// enter_world WS + PG integration (M3-T4a)
// ---------------------------------------------------------------------------

type wsBaselineEvent struct {
	kind uint16 // 203, 218, or 220
	snap proto.CellSnapshot
	frag proto.ChunkFragment
	shop proto.ShopList
}

type wsBaselineFake struct {
	mu          sync.Mutex
	events      []wsBaselineEvent
	err         error
	calls       int
	entered     chan struct{} // closed once on first entry; never reassigned
	enteredOnce sync.Once
	block       chan struct{}
	blockAfter  int // emit first N events, then wait for block; -1 disables
	sawAcct     int64
	sawChar     int64
}

func newWsBaselineFake() *wsBaselineFake {
	return &wsBaselineFake{entered: make(chan struct{}), blockAfter: -1}
}

func (f *wsBaselineFake) StreamBaseline(
	ctx context.Context,
	_ session.ID,
	accountID int64,
	characterID int64,
	sink gateway.BaselineSink,
) error {
	f.mu.Lock()
	f.calls++
	f.sawAcct = accountID
	f.sawChar = characterID
	f.mu.Unlock()
	f.enteredOnce.Do(func() { close(f.entered) })
	f.mu.Lock()
	events := append([]wsBaselineEvent(nil), f.events...)
	err := f.err
	block := f.block
	n := f.blockAfter
	f.mu.Unlock()
	for i, ev := range events {
		if block != nil && i == n {
			select {
			case <-block:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		var serr error
		switch ev.kind {
		case proto.OpcodeCellSnapshot:
			serr = sink.CellSnapshot(ev.snap)
		case proto.OpcodeChunkFragment:
			serr = sink.ChunkFragment(ev.frag)
		case proto.OpcodeShopList:
			serr = sink.ShopList(ev.shop)
		default:
			return errors.New("ws baseline: unknown event kind")
		}
		if serr != nil {
			return serr
		}
	}
	return err
}

func (f *wsBaselineFake) setEvents(events []wsBaselineEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = events
}

func (f *wsBaselineFake) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *wsBaselineFake) setBlock(ch chan struct{}, after int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.block = ch
	f.blockAfter = after
}

func (f *wsBaselineFake) closeBlock() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.block != nil {
		close(f.block)
		f.block = nil
	}
}

func (f *wsBaselineFake) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func intSnap(cellX int32, entity uint32) proto.CellSnapshot {
	return proto.CellSnapshot{
		Cell: proto.Cell{X: cellX, Z: 0},
		Entities: []proto.EntityEntry{
			{Entity: entity, Kind: 1, Proto: 21, Pos: proto.Position{X: 1, Y: 2, Z: 3}, Angle: 100, Speed: 5},
		},
	}
}

func intFrag(idx uint16) proto.ChunkFragment {
	return proto.ChunkFragment{
		Cell: proto.Cell{X: 0, Z: 0}, ChunkIdx: 7,
		FragIdx: idx, FragCount: 2, Bytes: []byte{byte(idx + 1), 2, 3},
	}
}

func intShop() proto.ShopList {
	return proto.ShopList{
		Vendor: 77,
		Listings: []proto.ShopListingEntry{
			{Listing: 3, Price: 100, Qty: 5},
		},
	}
}

// enterChar creates slot-0 "Aria" and returns its durable character ID.
func (f *charFixture) enterChar(t *testing.T, c *websocket.Conn, accountID int64, seq uint32) int64 {
	t.Helper()
	sendCreate(t, c, seq, validCharCreate(0, "Aria"))
	if _, op := readCharOp(t, c); op.OK != proto.CharacterOpOK {
		t.Fatalf("create = %+v", op)
	}
	rows, err := f.st.ListLiveCharacters(context.Background(), accountID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("live = %+v, %v", rows, err)
	}
	return rows[0].ID
}

// TestEnterWorldBaselineWS is the end-to-end T4a proof: exact wire
// order 217/203/218/218/203/220/219 with decoded payloads, contiguous
// S→C seq, msg_version 1, and IN_WORLD bound afterward.
func TestEnterWorldBaselineWS(t *testing.T) {
	provider := newWsBaselineFake()
	provider.setEvents([]wsBaselineEvent{
		{kind: proto.OpcodeCellSnapshot, snap: intSnap(0, 11)},
		{kind: proto.OpcodeChunkFragment, frag: intFrag(0)},
		{kind: proto.OpcodeChunkFragment, frag: intFrag(1)},
		{kind: proto.OpcodeCellSnapshot, snap: intSnap(1, 12)},
		{kind: proto.OpcodeShopList, shop: intShop()},
	})
	f := newCharFixture(t, false, provider)
	c := f.dial(t)
	accountID := f.helloChar(t, c) // 200 seq 1
	charID := f.enterChar(t, c, accountID, 2)

	sendEnterWorld(t, c, 3, 0)
	type frame struct {
		header  proto.Header
		payload *proto.Decoder
	}
	read := func() frame {
		t.Helper()
		h, p := readFrame(t, c)
		if h.MsgVersion != proto.MessageVersion1 {
			t.Fatalf("msg_version = %d, want 1", h.MsgVersion)
		}
		return frame{header: h, payload: p}
	}
	// 200(1) hello, 217 create(2) already consumed; baseline must be
	// exactly 217(3), 203(4), 218(5), 218(6), 203(7), 220(8), 219(9).
	wantOps := []uint16{
		proto.OpcodeCharacterOp,
		proto.OpcodeCellSnapshot,
		proto.OpcodeChunkFragment,
		proto.OpcodeChunkFragment,
		proto.OpcodeCellSnapshot,
		proto.OpcodeShopList,
		proto.OpcodeWorldReady,
	}
	var frames []frame
	for range wantOps {
		frames = append(frames, read())
	}
	for i, want := range wantOps {
		if frames[i].header.Opcode != want {
			t.Fatalf("frame %d opcode = %d, want %d", i, frames[i].header.Opcode, want)
		}
		if frames[i].header.Seq != uint32(3+i) {
			t.Fatalf("frame %d seq = %d, want %d", i, frames[i].header.Seq, 3+i)
		}
	}
	if op, err := proto.DecodeCharacterOp(frames[0].payload); err != nil ||
		op.Op != proto.CharacterOpEnterWorld || op.OK != proto.CharacterOpOK {
		t.Fatalf("217 = %+v, %v", op, err)
	}
	if got, err := proto.DecodeCellSnapshot(frames[1].payload); err != nil || got.Cell.X != 0 {
		t.Fatalf("203a = %+v, %v", got, err)
	} else if len(got.Entities) != 1 || got.Entities[0].Entity != 11 {
		t.Fatalf("203a entities = %+v", got.Entities)
	}
	for i, wantIdx := range []uint16{0, 1} {
		got, err := proto.DecodeChunkFragment(frames[2+i].payload)
		if err != nil || got.FragIdx != wantIdx || got.FragCount != 2 {
			t.Fatalf("218[%d] = %+v, %v", i, got, err)
		}
		if len(got.Bytes) != 3 || got.Bytes[0] != byte(wantIdx+1) {
			t.Fatalf("218[%d] bytes = %v", i, got.Bytes)
		}
	}
	if got, err := proto.DecodeCellSnapshot(frames[4].payload); err != nil || got.Cell.X != 1 {
		t.Fatalf("203b = %+v, %v", got, err)
	}
	if got, err := proto.DecodeShopList(frames[5].payload); err != nil ||
		got.Vendor != 77 || len(got.Listings) != 1 || got.Listings[0].Listing != 3 {
		t.Fatalf("220 = %+v, %v", got, err)
	}
	if _, err := proto.DecodeWorldReady(frames[6].payload); err != nil {
		t.Fatalf("219 decode: %v", err)
	}
	// Registry is IN_WORLD and bound; provider got internal IDs only.
	ids := f.reg.SessionsBySub("test-sub")
	snap := waitForSessionState(t, f.reg, ids[0], session.StateInWorld)
	if !snap.HasCharacter || snap.CharacterID != charID {
		t.Errorf("after enter: %+v, want IN_WORLD bound to %d", snap, charID)
	}
	if owner, ok := f.reg.SessionByCharacter(charID); !ok || owner != ids[0] {
		t.Errorf("index = %d,%v", owner, ok)
	}
	// Nothing follows the barrier: the next reply is a live 121→216,
	// and a second 124 hits the existing bad_state gate.
	sendCharList(t, c, 10)
	if _, list := readCharList(t, c); len(list.Characters) != 1 {
		t.Errorf("list after enter = %+v", list.Characters)
	}
	sendEnterWorld(t, c, 11, 0)
	if _, msg := readError(t, c); msg.Code != proto.ErrorCodeBadState {
		t.Errorf("second 124 code = %d, want bad_state from the server gate", msg.Code)
	}
}

// TestEnterWorldBarrierWS blocks the provider mid-baseline and proves
// the session sits CHARACTER_SELECTED with no 219 until release.
func TestEnterWorldBarrierWS(t *testing.T) {
	provider := newWsBaselineFake()
	provider.setEvents([]wsBaselineEvent{
		{kind: proto.OpcodeCellSnapshot, snap: intSnap(0, 11)},
		{kind: proto.OpcodeShopList, shop: intShop()},
	})
	f := newCharFixture(t, false, provider)
	c := f.dial(t)
	accountID := f.helloChar(t, c)
	charID := f.enterChar(t, c, accountID, 2)
	ids := f.reg.SessionsBySub("test-sub")
	sid := ids[0]

	provider.setBlock(make(chan struct{}), 1) // emit 203, then wait
	sendEnterWorld(t, c, 3, 0)
	// 217 then 203 arrive; the 220/219 cannot.
	if _, op := readCharOp(t, c); op.OK != proto.CharacterOpOK {
		t.Fatalf("217 = %+v", op)
	}
	h, _ := readFrame(t, c)
	if h.Opcode != proto.OpcodeCellSnapshot {
		t.Fatalf("opcode = %d, want 203", h.Opcode)
	}
	<-provider.entered
	snap, _ := f.reg.Get(sid)
	if snap.State != session.StateCharacterSelected || !snap.HasCharacter || snap.CharacterID != charID {
		t.Fatalf("mid-baseline = %+v, want SELECTED bound", snap)
	}
	provider.closeBlock()
	h, _ = readFrame(t, c)
	if h.Opcode != proto.OpcodeShopList {
		t.Fatalf("opcode = %d, want 220 after release", h.Opcode)
	}
	h, p := readFrame(t, c)
	if h.Opcode != proto.OpcodeWorldReady {
		t.Fatalf("opcode = %d, want 219 barrier", h.Opcode)
	}
	if _, err := proto.DecodeWorldReady(p); err != nil {
		t.Fatalf("219 decode: %v", err)
	}
	waitForSessionState(t, f.reg, sid, session.StateInWorld)
}

func mustGetSession(t *testing.T, r *session.Registry, id session.ID) session.Snapshot {
	t.Helper()
	snap, ok := r.Get(id)
	if !ok {
		t.Fatalf("session %d vanished", uint64(id))
	}
	return snap
}

// waitForSessionState polls until the session reaches want. Reading the
// 219 world_ready frame from the client proves the PHYSICAL WRITE; the
// registry's CHARACTER_SELECTED→IN_WORLD completion runs on the handler
// goroutine immediately after its send returns, so the two observations
// race. The poll is bounded and fails loudly — completion that never
// happens still fails the test.
func waitForSessionState(t *testing.T, r *session.Registry, id session.ID, want session.State) session.Snapshot {
	t.Helper()
	var snap session.Snapshot
	waitFor(t, fmt.Sprintf("session %d to reach %s", uint64(id), want), func() bool {
		s, ok := r.Get(id)
		if ok {
			snap = s
		}
		return ok && s.State == want
	})
	return snap
}

// TestEnterWorldOperationalFailureWS proves the provisional-baseline
// contract: 217/203/218 then 202 retry, no 219, rollback, and a
// same-socket retry that completes.
func TestEnterWorldOperationalFailureWS(t *testing.T) {
	provider := newWsBaselineFake()
	provider.setEvents([]wsBaselineEvent{
		{kind: proto.OpcodeCellSnapshot, snap: intSnap(0, 11)},
		{kind: proto.OpcodeChunkFragment, frag: intFrag(0)},
	})
	provider.setErr(errors.New("cells unavailable"))
	f := newCharFixture(t, false, provider)
	c := f.dial(t)
	accountID := f.helloChar(t, c)
	charID := f.enterChar(t, c, accountID, 2)
	ids := f.reg.SessionsBySub("test-sub")
	sid := ids[0]

	sendEnterWorld(t, c, 3, 0)
	if _, op := readCharOp(t, c); op.OK != proto.CharacterOpOK {
		t.Fatalf("217 = %+v", op)
	}
	if h, _ := readFrame(t, c); h.Opcode != proto.OpcodeCellSnapshot {
		t.Fatalf("opcode = %d, want 203", h.Opcode)
	}
	if h, _ := readFrame(t, c); h.Opcode != proto.OpcodeChunkFragment {
		t.Fatalf("opcode = %d, want 218", h.Opcode)
	}
	if _, msg := readError(t, c); msg.Code != proto.ErrorCodeRetry {
		t.Fatalf("code = %d, want retry", msg.Code)
	}
	snap, _ := f.reg.Get(sid)
	if snap.State != session.StateAuthenticated || snap.HasCharacter {
		t.Fatalf("not rolled back: %+v", snap)
	}
	if _, ok := f.reg.SessionByCharacter(charID); ok {
		t.Fatal("index leaked")
	}
	// Same socket retries to a full success.
	provider.setErr(nil)
	sendEnterWorld(t, c, 4, 0)
	if _, op := readCharOp(t, c); op.OK != proto.CharacterOpOK {
		t.Fatalf("retry 217 = %+v", op)
	}
	if h, _ := readFrame(t, c); h.Opcode != proto.OpcodeCellSnapshot {
		t.Fatalf("opcode = %d", h.Opcode)
	}
	if h, _ := readFrame(t, c); h.Opcode != proto.OpcodeChunkFragment {
		t.Fatalf("opcode = %d", h.Opcode)
	}
	if h, _ := readFrame(t, c); h.Opcode != proto.OpcodeWorldReady {
		t.Fatalf("opcode = %d, want 219", h.Opcode)
	}
	waitForSessionState(t, f.reg, sid, session.StateInWorld)
}

// TestEnterWorldSimultaneousTakeoverWS: A enters and blocks
// mid-baseline; B's enter cannot overlap it; once A completes to
// IN_WORLD, B takes it over — A is kicked/retired, B becomes the one
// IN_WORLD session, and the two baselines never overlapped.
func TestEnterWorldSimultaneousTakeoverWS(t *testing.T) {
	provider := newWsBaselineFake()
	provider.setEvents([]wsBaselineEvent{
		{kind: proto.OpcodeCellSnapshot, snap: intSnap(0, 11)},
		{kind: proto.OpcodeCellSnapshot, snap: intSnap(1, 12)},
	})
	f := newCharFixture(t, false, provider)
	cA := f.dial(t)
	accountID := f.helloChar(t, cA)
	charID := f.enterChar(t, cA, accountID, 2)
	sidA := f.reg.SessionsBySub("test-sub")[0] // deterministic: A is the only session so far

	cB := f.dial(t)
	f.helloChar(t, cB)
	if ids := f.reg.SessionsBySub("test-sub"); len(ids) != 2 {
		t.Fatalf("sessions = %d, want 2", len(ids))
	}

	provider.setBlock(make(chan struct{}), 1) // emit 203, then wait
	sendEnterWorld(t, cA, 3, 0)
	if _, op := readCharOp(t, cA); op.OK != proto.CharacterOpOK {
		t.Fatalf("A 217 = %+v", op)
	}
	if h, _ := readFrame(t, cA); h.Opcode != proto.OpcodeCellSnapshot {
		t.Fatalf("A opcode = %d, want 203", h.Opcode)
	}
	<-provider.entered // A holds the account guard mid-baseline

	// B enters while A holds the guard; B's baseline cannot overlap A's.
	sendEnterWorld(t, cB, 3, 0)
	provider.closeBlock()
	// A completes: second 203, then 219 -> IN_WORLD.
	if h, _ := readFrame(t, cA); h.Opcode != proto.OpcodeCellSnapshot {
		t.Fatalf("A opcode = %d, want second 203", h.Opcode)
	}
	if h, _ := readFrame(t, cA); h.Opcode != proto.OpcodeWorldReady {
		t.Fatalf("A opcode = %d, want 219", h.Opcode)
	}
	// B's takeover kicks A, then runs B's own baseline.
	if _, msg := readError(t, cA); msg.Code != proto.ErrorCodeKicked {
		t.Fatalf("A code = %d, want kicked", msg.Code)
	}
	readClosed(t, cA)
	if _, op := readCharOp(t, cB); op.OK != proto.CharacterOpOK {
		t.Fatalf("B 217 = %+v", op)
	}
	for i := 0; i < 2; i++ {
		if h, _ := readFrame(t, cB); h.Opcode != proto.OpcodeCellSnapshot {
			t.Fatalf("B opcode = %d, want 203", h.Opcode)
		}
	}
	if h, _ := readFrame(t, cB); h.Opcode != proto.OpcodeWorldReady {
		t.Fatalf("B opcode = %d, want 219", h.Opcode)
	}

	waitFor(t, "old session cleanup", func() bool { return f.reg.Len() == 1 })
	if _, ok := f.reg.Get(sidA); ok {
		t.Error("winner A still registered after takeover")
	}
	inWorld := 0
	for _, id := range f.reg.SessionsBySub("test-sub") {
		s, _ := f.reg.Get(id)
		if s.State == session.StateInWorld {
			inWorld++
			if !s.HasCharacter || s.CharacterID != charID {
				t.Errorf("B = %+v, want IN_WORLD bound to %d", s, charID)
			}
		}
	}
	if inWorld != 1 {
		t.Errorf("IN_WORLD sessions = %d, want 1", inWorld)
	}
	if n := f.exit.callCount(); n != 1 {
		t.Errorf("WorldExit calls = %d, want 1 (A flushed once)", n)
	}
	if n := provider.callCount(); n != 2 {
		t.Errorf("provider calls = %d, want 2 (A then B, never overlapping)", n)
	}
}

// TestTakeoverKicksOldSameCharacterWS is the healthy duplicate-login
// proof over real sockets: c1 holds the character IN_WORLD, c2 enters
// the same character, c1 receives a decodable 202 kicked consuming its
// OWN sequence (never c2's) with the injected tick, then its
// connection becomes unusable — while c2 completes 217/baseline/219
// and becomes the only IN_WORLD session.
func TestTakeoverKicksOldSameCharacterWS(t *testing.T) {
	provider := newWsBaselineFake()
	provider.setEvents([]wsBaselineEvent{
		{kind: proto.OpcodeCellSnapshot, snap: intSnap(0, 11)},
	})
	f := newCharFixture(t, false, provider)
	c1 := f.dial(t)
	accountID := f.helloChar(t, c1)            // 200 seq 1
	charID := f.enterChar(t, c1, accountID, 2) // 217 create seq 2

	sendEnterWorld(t, c1, 3, 0) // 217(3), 203(4), 219(5)
	if _, op := readCharOp(t, c1); op.OK != proto.CharacterOpOK {
		t.Fatalf("c1 217 = %+v", op)
	}
	if h, _ := readFrame(t, c1); h.Opcode != proto.OpcodeCellSnapshot {
		t.Fatalf("c1 opcode = %d, want 203", h.Opcode)
	}
	if h, _ := readFrame(t, c1); h.Opcode != proto.OpcodeWorldReady {
		t.Fatalf("c1 opcode = %d, want 219", h.Opcode)
	}
	sid1 := f.reg.SessionsBySub("test-sub")[0] // deterministic: c1 is the only session
	waitForSessionState(t, f.reg, sid1, session.StateInWorld)

	// Diverge the two sessions' sequence counters.
	sendCharList(t, c1, 6) // 216 seq 6 on c1
	if h, _ := readCharList(t, c1); h.Seq != 6 {
		t.Fatalf("c1 216 seq = %d, want 6", h.Seq)
	}
	c2 := f.dial(t)
	f.helloChar(t, c2)     // 200 seq 1 on c2
	sendCharList(t, c2, 2) // 216 seq 2 on c2
	if h, _ := readCharList(t, c2); h.Seq != 2 {
		t.Fatalf("c2 216 seq = %d, want 2", h.Seq)
	}

	sendEnterWorld(t, c2, 3, 0)
	// c1: decodable kicked 202 with c1's OWN next seq (7) and the
	// injected tick — no shared account sequence, no reset for c2.
	header, msg := readError(t, c1)
	if msg.Code != proto.ErrorCodeKicked {
		t.Fatalf("c1 code = %d, want kicked", msg.Code)
	}
	if header.Seq != 7 {
		t.Errorf("kicked seq = %d, want 7 (old session's own counter)", header.Seq)
	}
	if header.MsgVersion != proto.MessageVersion1 {
		t.Errorf("kicked msg_version = %d, want 1", header.MsgVersion)
	}
	if header.Tick != testTick {
		t.Errorf("kicked tick = %d, want injected %d", header.Tick, testTick)
	}
	// The connection is force-closed; no particular close status is
	// promised (CloseNow performs no handshake).
	readClosed(t, c1)

	// c2: its baseline continues ITS counter — 217(3), 203(4), 219(5).
	h217, op := readCharOp(t, c2)
	if op.OK != proto.CharacterOpOK {
		t.Fatalf("c2 217 = %+v", op)
	}
	if h217.Seq != 3 {
		t.Errorf("c2 217 seq = %d, want 3", h217.Seq)
	}
	if h, _ := readFrame(t, c2); h.Opcode != proto.OpcodeCellSnapshot || h.Seq != 4 {
		t.Errorf("c2 203 = opcode %d seq %d, want 203/4", h.Opcode, h.Seq)
	}
	h219, p := readFrame(t, c2)
	if h219.Opcode != proto.OpcodeWorldReady || h219.Seq != 5 {
		t.Fatalf("c2 219 = opcode %d seq %d, want 219/5", h219.Opcode, h219.Seq)
	}
	if _, err := proto.DecodeWorldReady(p); err != nil {
		t.Fatalf("219 decode: %v", err)
	}

	// Registry: old fully retired, new indexed, exactly one IN_WORLD.
	waitFor(t, "old session cleanup", func() bool { return f.reg.Len() == 1 })
	if _, ok := f.reg.Get(sid1); ok {
		t.Error("old session still registered")
	}
	if got := len(f.reg.SessionsBySub("test-sub")); got != 1 {
		t.Fatalf("sessions by sub = %d, want 1", got)
	}
	sid2 := f.reg.SessionsBySub("test-sub")[0]
	s2 := waitForSessionState(t, f.reg, sid2, session.StateInWorld)
	if !s2.HasCharacter || s2.CharacterID != charID {
		t.Errorf("new session = %+v, want IN_WORLD bound to %d", s2, charID)
	}
	if owner, ok := f.reg.SessionByCharacter(charID); !ok || owner != sid2 {
		t.Errorf("character index = %d,%v, want the new session", owner, ok)
	}
	if n := f.exit.callCount(); n != 1 {
		t.Errorf("WorldExit calls = %d, want 1", n)
	}
	if n := provider.callCount(); n != 2 {
		t.Errorf("provider calls = %d, want 2", n)
	}
}

// TestTakeoverDifferentCharacterWS proves one-world-session-per-account
// (not merely one-per-character): c1 is IN_WORLD on X, c2 enters the
// same account's Y — X is flushed and released, c2 baselines Y.
func TestTakeoverDifferentCharacterWS(t *testing.T) {
	provider := newWsBaselineFake()
	provider.setEvents([]wsBaselineEvent{
		{kind: proto.OpcodeCellSnapshot, snap: intSnap(0, 11)},
	})
	f := newCharFixture(t, false, provider)
	c1 := f.dial(t)
	accountID := f.helloChar(t, c1)
	sendCreate(t, c1, 2, validCharCreate(0, "Aria"))
	if _, op := readCharOp(t, c1); op.OK != proto.CharacterOpOK {
		t.Fatalf("create Aria = %+v", op)
	}
	sendCreate(t, c1, 3, validCharCreate(1, "Bram"))
	if _, op := readCharOp(t, c1); op.OK != proto.CharacterOpOK {
		t.Fatalf("create Bram = %+v", op)
	}
	rows, err := f.st.ListLiveCharacters(context.Background(), accountID)
	if err != nil || len(rows) != 2 {
		t.Fatalf("live characters = %+v, %v", rows, err)
	}
	var charX, charY int64 // X = slot 0 (Aria, old), Y = slot 1 (Bram, new)
	for _, r := range rows {
		if r.Slot == 0 {
			charX = r.ID
		} else {
			charY = r.ID
		}
	}
	sendEnterWorld(t, c1, 4, 0) // 217(4), 203(5), 219(6): IN_WORLD on X
	if _, op := readCharOp(t, c1); op.OK != proto.CharacterOpOK {
		t.Fatalf("c1 217 = %+v", op)
	}
	if h, _ := readFrame(t, c1); h.Opcode != proto.OpcodeCellSnapshot {
		t.Fatalf("c1 opcode = %d", h.Opcode)
	}
	if h, _ := readFrame(t, c1); h.Opcode != proto.OpcodeWorldReady {
		t.Fatalf("c1 opcode = %d, want 219", h.Opcode)
	}
	sid1 := f.reg.SessionsBySub("test-sub")[0]

	c2 := f.dial(t)
	f.helloChar(t, c2)
	sendEnterWorld(t, c2, 2, 1) // slot 1 = Bram

	if _, msg := readError(t, c1); msg.Code != proto.ErrorCodeKicked {
		t.Fatalf("c1 code = %d, want kicked", msg.Code)
	}
	readClosed(t, c1)
	if _, op := readCharOp(t, c2); op.OK != proto.CharacterOpOK {
		t.Fatalf("c2 217 = %+v", op)
	}
	if h, _ := readFrame(t, c2); h.Opcode != proto.OpcodeCellSnapshot {
		t.Fatalf("c2 opcode = %d, want 203", h.Opcode)
	}
	if h, _ := readFrame(t, c2); h.Opcode != proto.OpcodeWorldReady {
		t.Fatalf("c2 opcode = %d, want 219", h.Opcode)
	}

	// WorldExit flushed the OLD character X; the baseline served Y.
	if n := f.exit.callCount(); n != 1 {
		t.Fatalf("WorldExit calls = %d, want 1", n)
	}
	if ob := f.exit.observedCopy(); len(ob) != 1 || ob[0].CharacterID != charX {
		t.Errorf("WorldExit observed = %+v, want the old character %d", ob, charX)
	}
	provider.mu.Lock()
	sawChar := provider.sawChar
	provider.mu.Unlock()
	if sawChar != charY {
		t.Errorf("baseline character = %d, want requested %d", sawChar, charY)
	}

	waitFor(t, "old session cleanup", func() bool { return f.reg.Len() == 1 })
	if _, ok := f.reg.Get(sid1); ok {
		t.Error("old session still registered")
	}
	if _, ok := f.reg.SessionByCharacter(charX); ok {
		t.Error("old character X still indexed")
	}
	sid2 := f.reg.SessionsBySub("test-sub")[0]
	s2 := waitForSessionState(t, f.reg, sid2, session.StateInWorld)
	if !s2.HasCharacter || s2.CharacterID != charY {
		t.Errorf("new session = %+v, want IN_WORLD bound to %d", s2, charY)
	}
}

// TestEnterWorldDisconnectCleanupWS: closing the socket mid-baseline
// must remove the session and its provisional character index.
func TestEnterWorldDisconnectCleanupWS(t *testing.T) {
	provider := newWsBaselineFake()
	provider.setEvents([]wsBaselineEvent{
		{kind: proto.OpcodeCellSnapshot, snap: intSnap(0, 11)},
	})
	f := newCharFixture(t, false, provider)
	c := f.dial(t)
	accountID := f.helloChar(t, c)
	charID := f.enterChar(t, c, accountID, 2)

	provider.setBlock(make(chan struct{}), 0) // block on entry
	sendEnterWorld(t, c, 3, 0)
	if _, op := readCharOp(t, c); op.OK != proto.CharacterOpOK {
		t.Fatalf("217 = %+v", op)
	}
	<-provider.entered
	ids := f.reg.SessionsBySub("test-sub")
	if s, _ := f.reg.Get(ids[0]); s.State != session.StateCharacterSelected {
		t.Fatalf("mid-baseline = %+v", s)
	}
	// Abrupt close (no handshake: the server is synchronously
	// mid-baseline and cannot answer one), then release the stranded
	// provider so its writes fail and cleanup runs.
	if err := c.CloseNow(); err != nil {
		t.Fatalf("close now: %v", err)
	}
	provider.closeBlock() // let the stranded baseline fail its writes
	waitFor(t, "session cleanup", func() bool { return f.reg.Len() == 0 })
	if _, ok := f.reg.SessionByCharacter(charID); ok {
		t.Error("provisional character index leaked after disconnect")
	}
}

// TestEnterWorldSlotMappingWS: malformed 124, invalid/empty slots, then
// a successful enter on the same socket.
func TestEnterWorldSlotMappingWS(t *testing.T) {
	provider := newWsBaselineFake()
	f := newCharFixture(t, false, provider)
	c := f.dial(t)
	accountID := f.helloChar(t, c)

	// Malformed payload inside a valid outer frame.
	sendFrame(t, c,
		proto.Header{Opcode: proto.OpcodeEnterWorld, MsgVersion: 1, Seq: 2, Tick: 1},
		func(e *proto.Encoder) error { return nil })
	if _, msg := readError(t, c); msg.Code != proto.ErrorCodeProtocol {
		t.Errorf("malformed code = %d, want protocol_error", msg.Code)
	}
	// Invalid slot and empty (but valid) slot.
	sendEnterWorld(t, c, 3, 7)
	if _, op := readCharOp(t, c); op.Op != proto.CharacterOpEnterWorld || op.OK != proto.CharacterOpRejected {
		t.Errorf("217 = %+v, want enter/rejected", op)
	}
	sendEnterWorld(t, c, 4, 1)
	if _, op := readCharOp(t, c); op.Op != proto.CharacterOpEnterWorld || op.OK != proto.CharacterOpRejected {
		t.Errorf("217 = %+v, want enter/rejected", op)
	}
	if n := provider.callCount(); n != 0 {
		t.Errorf("provider calls = %d, want 0 before valid enter", n)
	}
	// Same socket proceeds to a full success.
	charID := f.enterChar(t, c, accountID, 5)
	_ = charID
	sendEnterWorld(t, c, 6, 0)
	if _, op := readCharOp(t, c); op.OK != proto.CharacterOpOK {
		t.Fatalf("217 = %+v", op)
	}
	if h, _ := readFrame(t, c); h.Opcode != proto.OpcodeWorldReady {
		t.Fatalf("opcode = %d, want 219 (empty baseline)", h.Opcode)
	}
	ids := f.reg.SessionsBySub("test-sub")
	waitForSessionState(t, f.reg, ids[0], session.StateInWorld)
}
