// M3 exit audit: model-based lifecycle property + fuzz tests (spec
// §6.1/§6.1.2/§7). Random opcode sequences are driven through the REAL
// gateway — Server.handleBinary over complete proto.EncodeFrame frames,
// the real Registry, CharacterHandler → EnterWorldHandler chain — while
// an INDEPENDENT hard-coded model of §6.1 predicts every outcome. The
// oracle in this file never consults session.Allowed or any other
// implementation decision: divergence between model and implementation
// fails the test.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/dlukt/voxilian/internal/auth"
	"github.com/dlukt/voxilian/internal/character"
	"github.com/dlukt/voxilian/internal/proto"
	"github.com/dlukt/voxilian/internal/session"
	"github.com/dlukt/voxilian/internal/simtest"
)

// ---------------------------------------------------------------------------
// independent §6.1 oracle
// ---------------------------------------------------------------------------

// m3ModelAllows is the independent permission oracle, transcribed
// directly from spec §6.1: 100→CONNECTED; 101/121→AUTHENTICATED and
// later; 122/123/124→AUTHENTICATED only; 125→CHARACTER_SELECTED/
// IN_WORLD; 126 and gameplay 102–120→IN_WORLD only. It deliberately
// does NOT call session.Allowed — that function is part of the system
// under test. Only the wire opcode numbers are shared with the
// implementation.
func m3ModelAllows(st session.State, opcode uint16) bool {
	switch opcode {
	case proto.OpcodeHello:
		return st == session.StateConnected
	case proto.OpcodeReauth, proto.OpcodeCharacterList:
		return st == session.StateAuthenticated ||
			st == session.StateCharacterSelected ||
			st == session.StateInWorld
	case proto.OpcodeCharacterCreate, proto.OpcodeCharacterDelete, proto.OpcodeEnterWorld:
		return st == session.StateAuthenticated
	case proto.OpcodeAck:
		return st == session.StateCharacterSelected || st == session.StateInWorld
	case proto.OpcodeLeaveWorld:
		return st == session.StateInWorld
	}
	if opcode >= proto.OpcodeMove && opcode <= proto.OpcodeRespawnAck { // 102–120
		return st == session.StateInWorld
	}
	return false
}

// ---------------------------------------------------------------------------
// protocol payload sources (golden fixtures + explicit identity payloads)
// ---------------------------------------------------------------------------

// m3FixtureNames maps every C→S opcode to its committed M2 golden frame.
var m3FixtureNames = map[uint16]string{
	100: "100_hello.hex", 101: "101_reauth.hex",
	102: "102_move.hex", 103: "103_attack.hex",
	104: "104_cast.hex", 105: "105_use.hex",
	106: "106_get.hex", 107: "107_drop.hex",
	108: "108_put.hex", 109: "109_give.hex",
	110: "110_offer.hex", 111: "111_counter.hex",
	112: "112_accept.hex", 113: "113_cancel.hex",
	114: "114_buy.hex", 115: "115_rest.hex",
	116: "116_eat.hex", 117: "117_say.hex",
	118: "118_say_group.hex", 119: "119_safety_toggle.hex",
	120: "120_respawn_ack.hex", 121: "121_character_list_request.hex",
	122: "122_character_create.hex", 123: "123_character_delete.hex",
	124: "124_enter_world.hex", 125: "125_ack.hex",
	126: "126_leave_world.hex",
}

// m3FixturePayloads caches the payload bytes (everything after the
// 12-byte fixture header) of each golden frame once per process, so the
// fuzz inner loop never touches the filesystem. Fixtures are read-only:
// their payloads are re-framed with the runtime header and never
// mutated or regenerated.
var m3FixturePayloads struct {
	once    sync.Once
	payload map[uint16][]byte
}

// m3CanonicalPayload returns the golden payload for a non-identity
// opcode. Opcodes 100/101 are identity-bearing: callers must encode a
// test token explicitly (audit §20) instead of depending on whatever
// token text happens to sit in the fixture.
func m3CanonicalPayload(t *testing.T, opcode uint16) []byte {
	t.Helper()
	if opcode == proto.OpcodeHello || opcode == proto.OpcodeReauth {
		t.Fatalf("m3: opcode %d carries identity; encode a test token explicitly", opcode)
	}
	m3FixturePayloads.once.Do(func() {
		m3FixturePayloads.payload = make(map[uint16][]byte, len(m3FixtureNames))
		for op, name := range m3FixtureNames {
			frame := simtest.ProtocolGolden(t, name)
			m3FixturePayloads.payload[op] = append([]byte(nil), frame[proto.HeaderSize:]...)
		}
	})
	p, ok := m3FixturePayloads.payload[opcode]
	if !ok {
		t.Fatalf("m3: no golden payload for opcode %d", opcode)
	}
	return p
}

// m3EncodePayload encodes payload bytes through an Encoder func.
func m3EncodePayload(t *testing.T, encode func(*proto.Encoder) error) []byte {
	t.Helper()
	e := proto.NewEncoder()
	if err := encode(e); err != nil {
		t.Fatalf("m3: payload encode: %v", err)
	}
	b, err := e.Bytes()
	if err != nil {
		t.Fatalf("m3: payload bytes: %v", err)
	}
	return b
}

// m3RawPayload adapts raw payload bytes to an Encoder func.
func m3RawPayload(b []byte) func(*proto.Encoder) error {
	return func(e *proto.Encoder) error {
		e.WriteBytes(b)
		return nil
	}
}

// m3HelloPayload/m3ReauthPayload encode identity-bearing payloads with
// explicit test tokens rather than fixture token text (§20).
func m3HelloPayload(token string) func(*proto.Encoder) error {
	return func(enc *proto.Encoder) error {
		proto.Hello{ClientVersion: 1, ProtoVersion: 1, AccessToken: token}.Encode(enc)
		return nil
	}
}

func m3ReauthPayload(token string) func(*proto.Encoder) error {
	return func(enc *proto.Encoder) error {
		proto.Reauth{AccessToken: token}.Encode(enc)
		return nil
	}
}

// m3EdgeSeq values randomize the opaque incoming C→S header seq/tick
// (§22): the lifecycle oracle must ignore them entirely.
var m3EdgeSeq = []uint32{0, 1, 0x7fffffff, 0x80000000, 0xffffffff}

// m3Slots maps a 2-bit variant to a slot request; slot 7 is
// deliberately invalid (only 0/1 exist).
var m3Slots = []uint8{0, 1, 7, 0}

// ---------------------------------------------------------------------------
// test-only seams
// ---------------------------------------------------------------------------

// m3RecConn is the synchronous recording test connection (audit §7):
// WriteBinary executes the BinaryFrameBuilder inline and records the
// complete frame; Close/CloseNow record their kinds. TakeFrames
// atomically returns and clears the frames recorded since the previous
// call, keeping long random sequences bounded in memory. It implements
// no OutboundProducer method, so sendSessionFrame keeps its existing
// direct writer path — the T5a/T5b queue is deliberately out of scope.
type m3RecConn struct {
	mu        sync.Mutex
	frames    [][]byte
	closeNows int
}

func (c *m3RecConn) WriteBinary(_ context.Context, build session.BinaryFrameBuilder) error {
	frame, err := build()
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.frames = append(c.frames, frame)
	c.mu.Unlock()
	return nil
}

func (c *m3RecConn) Close(string) error { return nil }

func (c *m3RecConn) CloseNow() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeNows++
	return nil
}

// TakeFrames atomically returns and clears the recorded frames.
func (c *m3RecConn) TakeFrames() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.frames
	c.frames = nil
	return out
}

func (c *m3RecConn) CloseNowCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeNows
}

// m3Chars is the deterministic character fake (audit §10): two
// characters per account; List/Create/Delete/FindBySlot succeed
// deterministically; every call is counted. It performs no domain
// validation (T3a owns that) and characters are never really removed —
// the lifecycle model, not the store, is under audit. Test character
// IDs are model internals and never travel on the wire (§67).
type m3Chars struct {
	mu        sync.Mutex
	byAccount map[int64][2]int64

	listCalls   int
	createCalls int
	deleteCalls int
	deleteIDs   []int64
}

func newM3Chars() *m3Chars {
	return &m3Chars{byAccount: map[int64][2]int64{
		1: {101, 102},
		2: {201, 202},
	}}
}

func (c *m3Chars) charOf(accountID int64, slot uint8) int64 {
	if slot > 1 {
		return 0
	}
	return c.byAccount[accountID][slot]
}

func (c *m3Chars) List(ctx context.Context, accountID int64) ([]character.ListEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.listCalls++
	return []character.ListEntry{
		{Slot: 0, Name: fmt.Sprintf("m3-char-%d-0", accountID), Level: 20},
		{Slot: 1, Name: fmt.Sprintf("m3-char-%d-1", accountID), Level: 20},
	}, nil
}

func (c *m3Chars) Create(context.Context, int64, character.CreateRequest) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.createCalls++
	return 900, nil
}

func (c *m3Chars) FindBySlot(_ context.Context, accountID int64, slot uint8) (character.Descriptor, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if slot > 1 {
		return character.Descriptor{}, fmt.Errorf("m3: slot %d: %w", slot, character.ErrInvalidSlot)
	}
	pair, ok := c.byAccount[accountID]
	if !ok {
		return character.Descriptor{}, fmt.Errorf("m3: account %d: %w", accountID, character.ErrNotFound)
	}
	return character.Descriptor{ID: pair[slot], Slot: slot, Name: "m3", Revision: 1}, nil
}

func (c *m3Chars) Delete(_ context.Context, id, _ int64) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deleteCalls++
	c.deleteIDs = append(c.deleteIDs, id)
	return id, nil
}

// m3Baseline is the controllable BaselineProvider (audit §11): success
// mode emits exactly one minimal 203 cell_snapshot and returns nil;
// failNext mode fails BEFORE the first provider-emitted frame, so the
// expected transition is exactly the frozen §6.1.2 rollback.
type m3Baseline struct {
	mu       sync.Mutex
	calls    int
	failNext bool
}

func (b *m3Baseline) setFailNext(v bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failNext = v
}

func (b *m3Baseline) StreamBaseline(
	_ context.Context,
	_ session.ID,
	_ int64,
	_ int64,
	sink BaselineSink,
) error {
	b.mu.Lock()
	b.calls++
	fail := b.failNext
	b.failNext = false
	b.mu.Unlock()
	if fail {
		return errors.New("m3: baseline provider down")
	}
	return sink.CellSnapshot(proto.CellSnapshot{Cell: proto.Cell{X: 1, Z: 2}})
}

// m3ExitCall records one WorldExit invocation.
type m3ExitCall struct {
	sid  session.ID
	acct int64
	char int64
}

// m3Exit is the controllable WorldExit fake (audit §12): success or
// fail-next, with full call recording. It backs both normal leave_world
// and duplicate-login takeover flush.
type m3Exit struct {
	mu       sync.Mutex
	calls    []m3ExitCall
	failNext bool
}

func (x *m3Exit) setFailNext(v bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.failNext = v
}

func (x *m3Exit) ExitWorld(_ context.Context, sid session.ID, acct, char int64) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.calls = append(x.calls, m3ExitCall{sid: sid, acct: acct, char: char})
	if x.failNext {
		x.failNext = false
		return errors.New("m3: world exit down")
	}
	return nil
}

func (x *m3Exit) lastCall() m3ExitCall {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.calls[len(x.calls)-1]
}

// m3Auth is the deterministic auth fake (audit §13): fixed identities,
// full call recording (to prove the bad-state gate runs before any
// validator work).
type m3Auth struct {
	mu    sync.Mutex
	ids   map[string]auth.Identity
	calls []string
}

func (a *m3Auth) Validate(_ context.Context, tok string) (auth.Identity, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, tok)
	id, ok := a.ids[tok]
	if !ok {
		return auth.Identity{}, errors.New("m3: unknown token")
	}
	return id, nil
}

func (a *m3Auth) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

// m3Accounts is the deterministic provisioner: sub → accountID.
type m3Accounts struct {
	mu    sync.Mutex
	subs  map[string]int64
	calls int
}

func (p *m3Accounts) EnsureAccount(_ context.Context, sub string, _ *string) (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	id, ok := p.subs[sub]
	if !ok {
		return 0, errors.New("m3: unknown sub")
	}
	return id, nil
}

// m3Next is the test gameplay Next handler (audit §9): records every
// call, performs no lifecycle mutation, stays silent on the wire.
type m3Next struct {
	mu    sync.Mutex
	calls []m3GameplayCall
}

// m3GameplayCall records one downstream gameplay dispatch.
type m3GameplayCall struct {
	sid    session.ID
	opcode uint16
}

func (n *m3Next) Handle(
	_ context.Context,
	sid session.ID,
	header proto.Header,
	_ *proto.Decoder,
	_ SendFunc,
) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, m3GameplayCall{sid: sid, opcode: header.Opcode})
	return nil
}

func (n *m3Next) lastCall() m3GameplayCall {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.calls[len(n.calls)-1]
}

// ---------------------------------------------------------------------------
// engine: peers, model, execution
// ---------------------------------------------------------------------------

// m3Peer is one logical client (audit §15/§16): A1 and A2 share one
// account (duplicate-login coverage); B1 is an independent second
// account. Expected lifecycle state lives HERE and is evolved from the
// previous model state plus each action's spec semantics — never read
// back from the Registry.
type m3Peer struct {
	name       string
	account    int64
	sub        string
	token      string
	tokenFresh string
	otherToken string // a DIFFERENT account's valid token (mismatch reauth)

	live  bool
	sid   session.ID
	conn  *m3RecConn
	ca    *connAuth
	state session.State
	char  int64
	// lastSeq is the last S→C sequence recorded on this connection
	// (0 = none yet): every further frame must continue from it.
	lastSeq uint32
	// flowBase is the seq of the 219 that opened the current IN_WORLD
	// flow epoch (0 = no active epoch), observed on the wire.
	flowBase uint32
}

// m3Counters snapshots every fake's call count; step expectations are
// exact deltas, so "no handler/service/baseline/world-exit call" is a
// hard property, not an accident.
type m3Counters struct {
	validator int
	accounts  int
	list      int
	create    int
	delete    int
	baseline  int
	exit      int
	next      int
}

// m3WantFrame describes one expected S→C frame on one connection.
type m3WantFrame struct {
	opcode uint16
	code   uint16 // OpcodeError: expected 202 code
	charOp uint8  // OpcodeCharacterOp: expected op
	charOK uint8  // OpcodeCharacterOp: expected ok
}

// m3Engine drives the real gateway against the independent model.
type m3Engine struct {
	label string // e.g. "seed=7" — included in every failure (§72)
	step  int
	cur   string // human description of the current action

	reg      *session.Registry
	srv      *Server
	chars    *m3Chars
	baseline *m3Baseline
	exit     *m3Exit
	authFake *m3Auth
	accounts *m3Accounts
	next     *m3Next
	peers    []*m3Peer

	want      map[string][]m3WantFrame // per peer, this step
	wantKick  map[string]bool          // peers expected to be CloseNow'd
	wantDelta m3Counters               // expected counter delta this step
	// flowMayChange marks the two steps whose spec semantics legitimately
	// touch a flow epoch (successful enter initializes it at the 219;
	// successful leave clears it). Every other step must leave every
	// live peer's flow epoch bit-identical (§24).
	flowMayChange bool
	usedSIDs      map[session.ID]bool // every sid ever issued (never reused)
	seenClose     map[string]int      // last observed CloseNow count per peer
}

// m3Now/m3Exp are the fixed authorization clock (audit §13/§14): token
// expiries sit safely in the future of a frozen clock, so the hard
// deadline never interferes with the lifecycle model.
var (
	m3Now     = time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	m3Exp     = m3Now.Add(30 * time.Minute)
	m3ExpLate = m3Now.Add(60 * time.Minute)
)

// newM3Engine wires the REAL handler chain over the deterministic
// seams: CharacterHandler → EnterWorldHandler → gameplay recorder, one
// Registry, one Server with a no-op scheduler and the fixed clock.
func newM3Engine(t *testing.T, label string) *m3Engine {
	t.Helper()
	reg := session.NewRegistry()
	chars := newM3Chars()
	baseline := &m3Baseline{}
	exit := &m3Exit{}
	next := &m3Next{}
	enter, err := NewEnterWorldHandler(EnterWorldHandlerDeps{
		Characters: chars,
		Registry:   reg,
		Baseline:   baseline,
		WorldExit:  exit, // same instance the CharacterHandler uses
		Tick:       func() uint32 { return 777 },
		Next:       next,
	})
	if err != nil {
		t.Fatalf("m3: NewEnterWorldHandler: %v", err)
	}
	charHandler, err := NewCharacterHandler(chars, reg, exit, enter)
	if err != nil {
		t.Fatalf("m3: NewCharacterHandler: %v", err)
	}
	authFake := &m3Auth{ids: map[string]auth.Identity{
		"m3-tok-a":       {Sub: "m3-account-a", ExpiresAt: m3Exp},
		"m3-tok-a-fresh": {Sub: "m3-account-a", ExpiresAt: m3ExpLate},
		"m3-tok-b":       {Sub: "m3-account-b", ExpiresAt: m3Exp},
		"m3-tok-b-fresh": {Sub: "m3-account-b", ExpiresAt: m3ExpLate},
	}}
	accounts := &m3Accounts{subs: map[string]int64{
		"m3-account-a": 1,
		"m3-account-b": 2,
	}}
	srv := NewServer(ServerDeps{
		Registry:  reg,
		Validator: authFake,
		Accounts:  accounts,
		Welcome:   func(context.Context) proto.Welcome { return proto.Welcome{} },
		Tick:      func() uint32 { return 777 },
		Now:       func() time.Time { return m3Now },
		Schedule:  func(time.Time, func()) CancelFunc { return func() {} }, // no timers (§14)
		Handler:   charHandler,
	})
	e := &m3Engine{
		label:    label,
		reg:      reg,
		srv:      srv,
		chars:    chars,
		baseline: baseline,
		exit:     exit,
		authFake: authFake,
		accounts: accounts,
		next:     next,
		peers: []*m3Peer{
			{name: "A1", account: 1, sub: "m3-account-a",
				token: "m3-tok-a", tokenFresh: "m3-tok-a-fresh", otherToken: "m3-tok-b"},
			{name: "A2", account: 1, sub: "m3-account-a",
				token: "m3-tok-a", tokenFresh: "m3-tok-a-fresh", otherToken: "m3-tok-b"},
			{name: "B1", account: 2, sub: "m3-account-b",
				token: "m3-tok-b", tokenFresh: "m3-tok-b-fresh", otherToken: "m3-tok-a"},
		},
		want:      make(map[string][]m3WantFrame),
		wantKick:  make(map[string]bool),
		usedSIDs:  make(map[session.ID]bool),
		seenClose: make(map[string]int),
	}
	// Three logical peers start connected (fresh CONNECTED sessions).
	for _, p := range e.peers {
		e.connect(t, p)
	}
	e.assertInvariants(t)
	return e
}

// failf reports an oracle violation with full reproduction context
// (audit §72): label, step, action, model view, actual registry view.
func (e *m3Engine) failf(t *testing.T, format string, args ...any) {
	t.Helper()
	all := make([]any, 0, len(args)+5)
	all = append(all, e.label, e.step, e.cur)
	all = append(all, args...)
	all = append(all, e.modelView(), e.registryView())
	t.Fatalf("m3 lifecycle %s step %d (%s): "+format+"\nmodel:    %s\nregistry: %s", all...)
}

// modelView renders the expected state of every peer.
func (e *m3Engine) modelView() string {
	out := ""
	for _, p := range e.peers {
		out += fmt.Sprintf("[%s sid=%d %s live=%v char=%d flowBase=%d] ",
			p.name, uint64(p.sid), p.state, p.live, p.char, p.flowBase)
	}
	return out
}

// registryView renders the actual registry state of every peer sid.
func (e *m3Engine) registryView() string {
	out := ""
	for _, p := range e.peers {
		if snap, ok := e.reg.Get(p.sid); ok {
			out += fmt.Sprintf("[%s sid=%d %s auth=%v sub=%q acct=%d char=%d/%v] ",
				p.name, uint64(p.sid), snap.State, snap.Authenticated, snap.Sub,
				snap.AccountID, snap.CharacterID, snap.HasCharacter)
		} else {
			out += fmt.Sprintf("[%s sid=%d ABSENT] ", p.name, uint64(p.sid))
		}
	}
	return out
}

// counters snapshots every fake.
func (e *m3Engine) counters() m3Counters {
	e.chars.mu.Lock()
	list, create, del := e.chars.listCalls, e.chars.createCalls, e.chars.deleteCalls
	e.chars.mu.Unlock()
	e.baseline.mu.Lock()
	base := e.baseline.calls
	e.baseline.mu.Unlock()
	e.exit.mu.Lock()
	exits := len(e.exit.calls)
	e.exit.mu.Unlock()
	e.next.mu.Lock()
	next := len(e.next.calls)
	e.next.mu.Unlock()
	e.accounts.mu.Lock()
	acct := e.accounts.calls
	e.accounts.mu.Unlock()
	return m3Counters{
		validator: e.authFake.callCount(),
		accounts:  acct,
		list:      list,
		create:    create,
		delete:    del,
		baseline:  base,
		exit:      exits,
		next:      next,
	}
}

// flowSnapshot captures the flow epoch of every live peer, for the
// "no flow-epoch mutation" half of the wrong-state property (§24).
func (e *m3Engine) flowSnapshot(t *testing.T) map[session.ID]session.FlowSnapshot {
	t.Helper()
	out := make(map[session.ID]session.FlowSnapshot)
	for _, p := range e.peers {
		if !p.live {
			continue
		}
		fs, err := e.reg.FlowState(p.sid)
		if err != nil {
			e.failf(t, "FlowState(%s): %v", p.name, err)
		}
		out[p.sid] = fs
	}
	return out
}

// checkFlowsUnchanged proves no flow epoch moved during a step whose
// semantics may not touch one. Peers that died during the step (their
// epochs die with the session) are skipped; removal cleanup is covered
// by assertInvariants.
func (e *m3Engine) checkFlowsUnchanged(t *testing.T, before map[session.ID]session.FlowSnapshot) {
	t.Helper()
	for _, p := range e.peers {
		if !p.live {
			continue
		}
		want, wasLive := before[p.sid]
		if !wasLive {
			continue
		}
		fs, err := e.reg.FlowState(p.sid)
		if err != nil {
			e.failf(t, "FlowState(%s): %v", p.name, err)
		}
		if fs != want {
			e.failf(t, "%s flow epoch moved: %+v → %+v", p.name, want, fs)
		}
	}
}

// connect creates a brand-new session for a dead peer (audit §16/§51):
// fresh session.ID, CONNECTED, no identity, no binding, fresh S→C
// sequence epoch. Removed IDs are never resurrected.
func (e *m3Engine) connect(t *testing.T, p *m3Peer) {
	t.Helper()
	if p.live {
		return
	}
	p.conn = &m3RecConn{}
	p.ca = &connAuth{}
	sid := e.reg.Create(p.conn)
	if e.usedSIDs[sid] {
		e.failf(t, "Registry reused session ID %d", uint64(sid))
	}
	e.usedSIDs[sid] = true
	p.sid = sid
	p.live = true
	p.state = session.StateConnected
	p.char = 0
	p.lastSeq = 0
	p.flowBase = 0
	e.seenClose[p.name] = 0
}

// disconnect models a client going away (audit §50): release the
// deadline timer, remove the session (idempotent registry cleanup of
// sub/character indexes), mark the peer dead.
func (e *m3Engine) disconnect(p *m3Peer) {
	if !p.live {
		return
	}
	e.srv.cancelDeadline(p.ca)
	e.reg.Remove(p.sid)
	p.live = false
	p.char = 0
	p.flowBase = 0
}

// sendFrame feeds one complete encoded protocol frame into the REAL
// gateway for peer p (audit §6: Server.handleBinary directly, but the
// full binary protocol decoder stays in the path).
func (e *m3Engine) sendFrame(
	t *testing.T,
	p *m3Peer,
	opcode uint16,
	mv uint16,
	seq, tick uint32,
	encode func(*proto.Encoder) error,
) {
	t.Helper()
	frame, err := proto.EncodeFrame(proto.Header{
		Opcode: opcode, MsgVersion: mv, Seq: seq, Tick: tick,
	}, encode)
	if err != nil {
		e.failf(t, "EncodeFrame(%d): %v", opcode, err)
	}
	if err := e.srv.handleBinary(context.Background(), p.ca, p.sid, frame); err != nil {
		e.failf(t, "handleBinary(op %d) ended the connection: %v", opcode, err)
	}
}

// wantFrame appends one expected frame for a peer this step.
func (e *m3Engine) wantFrame(p *m3Peer, w m3WantFrame) {
	e.want[p.name] = append(e.want[p.name], w)
}

// want202 is the 202 expectation shorthand.
func (e *m3Engine) want202(p *m3Peer, code uint16) {
	e.wantFrame(p, m3WantFrame{opcode: proto.OpcodeError, code: code})
}

// wantCharOp is the 217 expectation shorthand.
func (e *m3Engine) wantCharOp(p *m3Peer, op, ok uint8) {
	e.wantFrame(p, m3WantFrame{opcode: proto.OpcodeCharacterOp, charOp: op, charOK: ok})
}

// gate implements the shared wrong-state branch (audit §24/§25): the
// independent oracle forbids the opcode in the model state, so the real
// gateway must answer exactly one 202 bad_state. The step driver then
// proves zero counter deltas and unchanged flow epochs, and
// assertInvariants proves no state/binding mutation. It reports whether
// it installed the expectation.
func (e *m3Engine) gate(p *m3Peer, opcode uint16) bool {
	if m3ModelAllows(p.state, opcode) {
		return false
	}
	e.want202(p, proto.ErrorCodeBadState)
	return true
}

// drainAndCheckFrames collects every connection's frames since the
// previous step and verifies them against the step's expectations:
// exact frame sequence per connection, MessageVersion1 on every frame
// (§66), per-connection S→C seq contiguity modulo 2^32 (§65 — never
// compared across sessions), expected 202 codes, expected 217 op/ok,
// and forced closes only where the model predicted a takeover
// retirement.
func (e *m3Engine) drainAndCheckFrames(t *testing.T) {
	t.Helper()
	for _, p := range e.peers {
		frames := p.conn.TakeFrames()
		want := e.want[p.name]
		if len(frames) != len(want) {
			e.failf(t, "%s recorded %d frames, want %d (%+v)", p.name, len(frames), len(want), want)
		}
		for i, raw := range frames {
			h, payload, err := proto.DecodeFrame(raw)
			if err != nil {
				e.failf(t, "%s frame %d undecodable: %v", p.name, i, err)
			}
			if h.MsgVersion != proto.MessageVersion1 {
				e.failf(t, "%s frame %d (op %d) msg_version = %d, want 1", p.name, i, h.Opcode, h.MsgVersion)
			}
			if h.Seq != p.lastSeq+1 {
				e.failf(t, "%s frame %d (op %d) seq = %d, want %d (contiguous per session)",
					p.name, i, h.Opcode, h.Seq, p.lastSeq+1)
			}
			p.lastSeq = h.Seq
			w := want[i]
			if h.Opcode != w.opcode {
				e.failf(t, "%s frame %d opcode = %d, want %d", p.name, i, h.Opcode, w.opcode)
			}
			switch h.Opcode {
			case proto.OpcodeError:
				msg, err := proto.DecodeErrorMessage(payload)
				if err != nil {
					e.failf(t, "%s frame %d: 202 decode: %v", p.name, i, err)
				}
				if msg.Code != w.code {
					e.failf(t, "%s frame %d: 202 code = %d, want %d", p.name, i, msg.Code, w.code)
				}
			case proto.OpcodeCharacterOp:
				op, err := proto.DecodeCharacterOp(payload)
				if err != nil {
					e.failf(t, "%s frame %d: 217 decode: %v", p.name, i, err)
				}
				if op.Op != w.charOp || op.OK != w.charOK {
					e.failf(t, "%s frame %d: 217 = %+v, want op=%d ok=%d", p.name, i, op, w.charOp, w.charOK)
				}
			case proto.OpcodeWorldReady:
				// The 219 that completes an enter opens the model's flow
				// epoch at exactly this sequence (observed on the wire,
				// never read from registry internals).
				p.flowBase = h.Seq
			}
		}
		// Forced closes: exactly the takeover retirements the model
		// predicted, on exactly the predicted peers.
		delta := p.conn.CloseNowCount() - e.seenClose[p.name]
		if e.wantKick[p.name] && delta != 1 {
			e.failf(t, "%s expected exactly one forced close, saw %d", p.name, delta)
		}
		if !e.wantKick[p.name] && delta != 0 {
			e.failf(t, "%s was force-closed %d times unexpectedly", p.name, delta)
		}
		e.seenClose[p.name] = p.conn.CloseNowCount()
	}
}

// checkCounters verifies the exact per-step call deltas (§24).
func (e *m3Engine) checkCounters(t *testing.T, before m3Counters) {
	t.Helper()
	after := e.counters()
	got := m3Counters{
		validator: after.validator - before.validator,
		accounts:  after.accounts - before.accounts,
		list:      after.list - before.list,
		create:    after.create - before.create,
		delete:    after.delete - before.delete,
		baseline:  after.baseline - before.baseline,
		exit:      after.exit - before.exit,
		next:      after.next - before.next,
	}
	if got != e.wantDelta {
		e.failf(t, "call deltas = %+v, want %+v", got, e.wantDelta)
	}
}

// oldWorldPeer finds the other live same-account session the model
// holds in CHARACTER_SELECTED/IN_WORLD (the takeover candidate).
func (e *m3Engine) oldWorldPeer(p *m3Peer) *m3Peer {
	for _, q := range e.peers {
		if q == p || !q.live || q.account != p.account {
			continue
		}
		if q.state == session.StateCharacterSelected || q.state == session.StateInWorld {
			return q
		}
	}
	return nil
}

// inUse reports whether any live model session holds charID in
// CHARACTER_SELECTED/IN_WORLD (the §6.1 delete arbitration view).
func (e *m3Engine) inUse(charID int64) bool {
	for _, q := range e.peers {
		if q.live && q.char == charID &&
			(q.state == session.StateCharacterSelected || q.state == session.StateInWorld) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// actions (each: predict from model → drive the real gateway → mutate model)
// ---------------------------------------------------------------------------

// doHello serves the hello property (audit §30/§31): valid token in
// CONNECTED → AUTHENTICATED + exactly one 200; invalid token → one 202
// session_expired with the session still CONNECTED and no identity; any
// hello in a wrong state → bad_state BEFORE validator work (the zero
// wantDelta proves the validator was never called).
func (e *m3Engine) doHello(t *testing.T, p *m3Peer, valid bool, mv uint16, seq, tick uint32) {
	t.Helper()
	token := p.token
	if !valid {
		token = "m3-invalid-token"
	}
	if e.gate(p, proto.OpcodeHello) {
		e.sendFrame(t, p, proto.OpcodeHello, mv, seq, tick, m3HelloPayload(token))
		return
	}
	e.wantDelta.validator = 1
	if valid {
		e.wantDelta.accounts = 1
		e.wantFrame(p, m3WantFrame{opcode: proto.OpcodeWelcome})
		p.state = session.StateAuthenticated
	} else {
		e.want202(p, proto.ErrorCodeSessionExpired)
	}
	e.sendFrame(t, p, proto.OpcodeHello, mv, seq, tick, m3HelloPayload(token))
}

// doReauth serves the reauth property (audit §32/§33): a same-identity
// refresh keeps the state and changes at most TokenExp; a valid token
// carrying a DIFFERENT identity, or an invalid token, yields one 202
// session_expired with identity, binding, and flow epoch untouched and
// no auto-provisioning; reauth in CONNECTED is bad_state without
// validator work.
func (e *m3Engine) doReauth(t *testing.T, p *m3Peer, mode int, mv uint16, seq, tick uint32) {
	t.Helper()
	var token string
	switch mode {
	case 0:
		token = p.tokenFresh // same identity, later expiry
	case 1:
		token = p.otherToken // valid JWT, different identity
	default:
		token = "m3-invalid-token"
	}
	if e.gate(p, proto.OpcodeReauth) {
		e.sendFrame(t, p, proto.OpcodeReauth, mv, seq, tick, m3ReauthPayload(token))
		return
	}
	beforeSnap, _ := e.reg.Get(p.sid)
	e.wantDelta.validator = 1
	if mode == 0 {
		e.wantFrame(p, m3WantFrame{opcode: proto.OpcodeReauthOK})
	} else {
		e.want202(p, proto.ErrorCodeSessionExpired)
	}
	e.sendFrame(t, p, proto.OpcodeReauth, mv, seq, tick, m3ReauthPayload(token))
	afterSnap, _ := e.reg.Get(p.sid)
	if mode == 0 {
		if !afterSnap.TokenExp.Equal(m3ExpLate) {
			e.failf(t, "reauth TokenExp = %v, want %v", afterSnap.TokenExp, m3ExpLate)
		}
	} else if !afterSnap.TokenExp.Equal(beforeSnap.TokenExp) {
		e.failf(t, "failed reauth mutated TokenExp: %v → %v", beforeSnap.TokenExp, afterSnap.TokenExp)
	}
}

// doCharList serves the 121 property (audit §34): allowed in
// AUTHENTICATED and later, replies exactly one 216, and the property
// fake never mutates lifecycle state.
func (e *m3Engine) doCharList(t *testing.T, p *m3Peer, mv uint16, seq, tick uint32) {
	t.Helper()
	if e.gate(p, proto.OpcodeCharacterList) {
		e.sendFrame(t, p, proto.OpcodeCharacterList, mv, seq, tick,
			m3RawPayload(m3CanonicalPayload(t, proto.OpcodeCharacterList)))
		return
	}
	e.wantDelta.list = 1
	e.wantFrame(p, m3WantFrame{opcode: proto.OpcodeCharacterListResult})
	e.sendFrame(t, p, proto.OpcodeCharacterList, mv, seq, tick,
		m3RawPayload(m3CanonicalPayload(t, proto.OpcodeCharacterList)))
}

// doCreate serves the 122 property (audit §35): AUTHENTICATED only; the
// deterministic fake succeeds → one 217 {create, ok=1}; lifecycle state
// unchanged. Domain validation belongs to T3a, not this audit.
func (e *m3Engine) doCreate(t *testing.T, p *m3Peer, mv uint16, seq, tick uint32) {
	t.Helper()
	if e.gate(p, proto.OpcodeCharacterCreate) {
		e.sendFrame(t, p, proto.OpcodeCharacterCreate, mv, seq, tick,
			m3RawPayload(m3CanonicalPayload(t, proto.OpcodeCharacterCreate)))
		return
	}
	e.wantDelta.create = 1
	e.wantCharOp(p, proto.CharacterOpCreate, proto.CharacterOpOK)
	e.sendFrame(t, p, proto.OpcodeCharacterCreate, mv, seq, tick,
		m3RawPayload(m3CanonicalPayload(t, proto.OpcodeCharacterCreate)))
}

// doDelete serves the 123 property (audit §36): AUTHENTICATED only. An
// invalid slot → one 217 {delete, rejected}; a character held by any
// live CHARACTER_SELECTED/IN_WORLD session → one 202 character_in_use
// with NO Delete call; otherwise → one 217 {delete, ok} and the Delete
// runs. The handler's Registry-based in-use check stays real.
func (e *m3Engine) doDelete(t *testing.T, p *m3Peer, slot uint8, mv uint16, seq, tick uint32) {
	t.Helper()
	payload := m3EncodePayload(t, func(enc *proto.Encoder) error {
		proto.CharacterDelete{Slot: slot}.Encode(enc)
		return nil
	})
	if e.gate(p, proto.OpcodeCharacterDelete) {
		e.sendFrame(t, p, proto.OpcodeCharacterDelete, mv, seq, tick, m3RawPayload(payload))
		return
	}
	switch {
	case slot > 1:
		e.wantCharOp(p, proto.CharacterOpDelete, proto.CharacterOpRejected)
	case e.inUse(e.chars.charOf(p.account, slot)):
		e.want202(p, proto.ErrorCodeCharacterInUse)
	default:
		e.wantDelta.delete = 1
		e.wantCharOp(p, proto.CharacterOpDelete, proto.CharacterOpOK)
	}
	e.sendFrame(t, p, proto.OpcodeCharacterDelete, mv, seq, tick, m3RawPayload(payload))
}

// doEnter serves the full §6.1.2 enter/takeover model (audit §37–§39,
// §46–§49): wrong state → bad_state; invalid slot → one 217 rejected
// with no takeover; a same-account old world session → WorldExit flush
// of the old character, unbind, kick, forced close, retirement (or one
// 202 retry with the old session fully untouched on flush failure);
// then 217 OK → baseline (failure → AbortEnterWorld + one 202 retry,
// old session stays retired) → 219 → IN_WORLD with a fresh flow epoch.
func (e *m3Engine) doEnter(
	t *testing.T,
	p *m3Peer,
	slot uint8,
	baselineFail, exitFail bool,
	mv uint16,
	seq, tick uint32,
) {
	t.Helper()
	payload := m3EncodePayload(t, func(enc *proto.Encoder) error {
		proto.EnterWorld{Slot: slot}.Encode(enc)
		return nil
	})
	if e.gate(p, proto.OpcodeEnterWorld) {
		e.sendFrame(t, p, proto.OpcodeEnterWorld, mv, seq, tick, m3RawPayload(payload))
		return
	}
	// Invalid/empty slot: one 217 rejected — never bad_state, retry, or a
	// takeover of some old session (audit §38).
	if slot > 1 {
		e.wantCharOp(p, proto.CharacterOpEnterWorld, proto.CharacterOpRejected)
		e.sendFrame(t, p, proto.OpcodeEnterWorld, mv, seq, tick, m3RawPayload(payload))
		return
	}
	char := e.chars.charOf(p.account, slot)
	old := e.oldWorldPeer(p)
	var oldSid session.ID
	var oldChar int64
	if old != nil {
		oldSid, oldChar = old.sid, old.char
	}
	checkOldFlush := func() {
		if old == nil {
			return
		}
		if got := e.exit.lastCall(); got.sid != oldSid || got.char != oldChar {
			e.failf(t, "takeover WorldExit = %+v, want old %s char %d", got, old.name, oldChar)
		}
	}
	if old != nil && exitFail {
		// Takeover flush failure (audit §48): old stays IN_WORLD, bound,
		// live, unkicked; the new session gets exactly one 202 retry; no
		// baseline begins.
		e.exit.setFailNext(true)
		e.wantDelta.exit = 1
		e.want202(p, proto.ErrorCodeRetry)
		e.sendFrame(t, p, proto.OpcodeEnterWorld, mv, seq, tick, m3RawPayload(payload))
		checkOldFlush()
		return
	}
	if old != nil {
		// Successful takeover (audit §46/§47): flush the old world
		// character exactly once BEFORE the replacement baseline, unbind,
		// kick, retire (one forced close). The old session is never
		// resurrected afterwards.
		e.wantDelta.exit = 1
		e.want202(old, proto.ErrorCodeKicked)
		e.wantKick[old.name] = true
		old.live = false
		old.char = 0
		old.flowBase = 0
	}
	if baselineFail {
		// Baseline failure (audit §39/§49): the 217 OK may already be
		// written, NO 219 ever, one 202 retry, back to
		// AUTHENTICATED/unbound — no partial binding survives.
		e.baseline.setFailNext(true)
		e.wantDelta.baseline = 1
		e.wantCharOp(p, proto.CharacterOpEnterWorld, proto.CharacterOpOK)
		e.want202(p, proto.ErrorCodeRetry)
		e.sendFrame(t, p, proto.OpcodeEnterWorld, mv, seq, tick, m3RawPayload(payload))
		checkOldFlush()
		return
	}
	// Success (audit §37): one 217 OK, one 203, one 219 → IN_WORLD
	// bound, fresh flow epoch at the 219 sequence.
	e.wantDelta.baseline = 1
	e.wantCharOp(p, proto.CharacterOpEnterWorld, proto.CharacterOpOK)
	e.wantFrame(p, m3WantFrame{opcode: proto.OpcodeCellSnapshot})
	e.wantFrame(p, m3WantFrame{opcode: proto.OpcodeWorldReady})
	e.flowMayChange = true
	e.sendFrame(t, p, proto.OpcodeEnterWorld, mv, seq, tick, m3RawPayload(payload))
	p.state = session.StateInWorld
	p.char = char
	checkOldFlush()
}

// doLeave serves the 126 property (audit §40/§41): IN_WORLD only; on
// flush failure exactly one 202 retry with the session still IN_WORLD,
// bound, and the flow epoch bit-identical; on success NO response frame
// at all — silent transition to AUTHENTICATED/unbound with the
// character index gone and the epoch cleared.
func (e *m3Engine) doLeave(t *testing.T, p *m3Peer, exitFail bool, mv uint16, seq, tick uint32) {
	t.Helper()
	payload := m3RawPayload(m3CanonicalPayload(t, proto.OpcodeLeaveWorld))
	if e.gate(p, proto.OpcodeLeaveWorld) {
		e.sendFrame(t, p, proto.OpcodeLeaveWorld, mv, seq, tick, payload)
		return
	}
	e.wantDelta.exit = 1
	beforeChar := p.char
	if exitFail {
		e.exit.setFailNext(true)
		e.want202(p, proto.ErrorCodeRetry)
	}
	e.sendFrame(t, p, proto.OpcodeLeaveWorld, mv, seq, tick, payload)
	if got := e.exit.lastCall(); got.sid != p.sid || got.char != beforeChar {
		e.failf(t, "leave WorldExit = %+v, want %s char %d", got, p.name, beforeChar)
	}
	if !exitFail {
		e.flowMayChange = true
		p.state = session.StateAuthenticated
		p.char = 0
		p.flowBase = 0
	}
}

// doAck serves the lifecycle-level ACK property (audit §43/§44):
// CONNECTED/AUTHENTICATED → bad_state; CHARACTER_SELECTED → a
// structurally valid ACK is a silent no-op; IN_WORLD → duplicate/stale
// ACKs are silent with no lifecycle change, future/malformed ACKs are
// exactly one 202 protocol_error. A valid, duplicate, or stale ACK
// never produces any success response. Exact ACK arithmetic is T5b's
// dedicated suite.
func (e *m3Engine) doAck(t *testing.T, p *m3Peer, variant int, mv uint16, seq, tick uint32) {
	t.Helper()
	ack := func(v uint32) func(*proto.Encoder) error {
		return func(enc *proto.Encoder) error {
			proto.Ack{AckSeq: v}.Encode(enc)
			return nil
		}
	}
	if e.gate(p, proto.OpcodeAck) {
		e.sendFrame(t, p, proto.OpcodeAck, mv, seq, tick, ack(p.flowBase))
		return
	}
	switch p.state {
	case session.StateCharacterSelected:
		// Structurally valid ACK before the flow epoch exists: no-op.
		e.sendFrame(t, p, proto.OpcodeAck, mv, seq, tick, ack(7))
	case session.StateInWorld:
		switch variant {
		case 0: // duplicate of lastAck: silent
			e.sendFrame(t, p, proto.OpcodeAck, mv, seq, tick, ack(p.flowBase))
		case 1: // stale (serially before lastAck): silent
			e.sendFrame(t, p, proto.OpcodeAck, mv, seq, tick, ack(p.flowBase-1))
		case 2: // future (ahead of sent flow): exactly one 202 protocol_error
			e.want202(p, proto.ErrorCodeProtocol)
			e.sendFrame(t, p, proto.OpcodeAck, mv, seq, tick, ack(p.flowBase+0x40000000))
		default: // malformed: truncated payload → exactly one 202 protocol_error
			e.want202(p, proto.ErrorCodeProtocol)
			e.sendFrame(t, p, proto.OpcodeAck, mv, seq, tick, func(enc *proto.Encoder) error {
				enc.WriteBytes([]byte{1, 2}) // 2 of 4 ackSeq bytes
				return nil
			})
		}
	default:
		e.failf(t, "ack dispatched in %s — oracle says allowed", p.state)
	}
}

// doGameplay serves the gameplay property (audit §42): 102–120 reach
// the downstream recorder ONLY in IN_WORLD, exactly once, silent on the
// wire, and never mutate lifecycle state.
func (e *m3Engine) doGameplay(t *testing.T, p *m3Peer, opcode uint16, mv uint16, seq, tick uint32) {
	t.Helper()
	if e.gate(p, opcode) {
		e.sendFrame(t, p, opcode, mv, seq, tick, m3RawPayload(m3CanonicalPayload(t, opcode)))
		return
	}
	e.wantDelta.next = 1
	e.sendFrame(t, p, opcode, mv, seq, tick, m3RawPayload(m3CanonicalPayload(t, opcode)))
	if got := e.next.lastCall(); got.sid != p.sid || got.opcode != opcode {
		e.failf(t, "gameplay dispatch = %+v, want %s op %d", got, p.name, opcode)
	}
}

// doMalformedKnown serves the malformed-payload property (audit
// §25/§26): in a wrong state bad_state wins BEFORE payload decoding; in
// an allowed state a truncated payload of a gateway-decoded opcode
// (100/101/122–125 with non-empty payload) is exactly one 202
// protocol_error with no service calls. Gameplay opcodes 102–120 are
// NOT decoded by the M3 chain (the spec freezes no gameplay payload
// validation before M4), so a truncated intent still reaches the
// recorder silently.
func (e *m3Engine) doMalformedKnown(t *testing.T, p *m3Peer, opcode uint16, mv uint16, seq, tick uint32) {
	t.Helper()
	var canonical []byte
	switch opcode {
	case proto.OpcodeHello:
		canonical = m3EncodePayload(t, m3HelloPayload(p.token))
	case proto.OpcodeReauth:
		canonical = m3EncodePayload(t, m3ReauthPayload(p.tokenFresh))
	default:
		canonical = m3CanonicalPayload(t, opcode)
	}
	if len(canonical) == 0 {
		// Empty-payload opcodes have nothing to truncate: exercise the
		// malformed whole-frame path instead.
		e.doShortFrame(t, p, 5)
		return
	}
	truncated := m3RawPayload(canonical[:len(canonical)-1])
	if e.gate(p, opcode) {
		// §25: the wrong-state gate must win before payload decoding —
		// never protocol_error merely because the payload was malformed.
		e.sendFrame(t, p, opcode, mv, seq, tick, truncated)
		return
	}
	if opcode >= proto.OpcodeMove && opcode <= proto.OpcodeRespawnAck {
		// Allowed-state gameplay with junk bytes: M3 passes intents
		// through untouched; the recorder sees the session exactly once.
		e.wantDelta.next = 1
		e.sendFrame(t, p, opcode, mv, seq, tick, truncated)
		if got := e.next.lastCall(); got.sid != p.sid || got.opcode != opcode {
			e.failf(t, "gameplay dispatch = %+v, want %s op %d", got, p.name, opcode)
		}
		return
	}
	e.want202(p, proto.ErrorCodeProtocol)
	e.sendFrame(t, p, opcode, mv, seq, tick, truncated)
}

// doShortFrame serves the malformed whole-frame property (audit §27):
// fewer bytes than proto.HeaderSize → exactly one 202 protocol_error,
// no handler call, state unchanged — regardless of what opcode bytes
// the prefix happens to contain.
func (e *m3Engine) doShortFrame(t *testing.T, p *m3Peer, n int) {
	t.Helper()
	if n >= proto.HeaderSize {
		n = proto.HeaderSize - 1
	}
	frame := make([]byte, n)
	if n > 0 {
		frame[0] = byte(proto.OpcodeHello) // a valid opcode inside an invalid frame
	}
	e.want202(p, proto.ErrorCodeProtocol)
	if err := e.srv.handleBinary(context.Background(), p.ca, p.sid, frame); err != nil {
		e.failf(t, "handleBinary(short frame) ended the connection: %v", err)
	}
}

var (
	m3UnknownOpcodes = []uint16{0, 99, 127, 199, 221, 65535}
	m3ServerOpcodes  = []uint16{200, 202, 219, 220}
)

// doUnknownOpcode serves audit §28: unknown opcodes get exactly one 202
// protocol_error in EVERY lifecycle state — never bad_state.
func (e *m3Engine) doUnknownOpcode(t *testing.T, p *m3Peer, opcode uint16, seq, tick uint32) {
	t.Helper()
	e.want202(p, proto.ErrorCodeProtocol)
	e.sendFrame(t, p, opcode, proto.MessageVersion1, seq, tick, nil)
}

// doServerOpcode serves audit §29: a client sending an S→C opcode
// (200/202/219/220) gets exactly one 202 protocol_error in every state
// — never bad_state.
func (e *m3Engine) doServerOpcode(t *testing.T, p *m3Peer, opcode uint16, seq, tick uint32) {
	t.Helper()
	e.want202(p, proto.ErrorCodeProtocol)
	e.sendFrame(t, p, opcode, proto.MessageVersion1, seq, tick, nil)
}

// ---------------------------------------------------------------------------
// central step driver
// ---------------------------------------------------------------------------

// m3StepBytes is the fuzz input consumed per action (audit §73): a
// control byte (top 2 bits = peer, low 6 bits = action class), a
// variant byte, and an edge byte (incoming seq/tick selection, slot
// variation, and future msg_version selection).
const m3StepBytes = 3

// m3Action classes for the control byte's low 6 bits. Class 0 and every
// unassigned class (24–63) map to the core §23 action — an arbitrary
// known opcode with a canonical valid payload — keeping the large
// majority of random mass on "random opcode sequences". Classes 12–23
// are progress operators (below).
const (
	m3ClassKnownOpcode = 0
	m3ClassMalformed   = 1
	m3ClassShortFrame  = 2
	m3ClassUnknown     = 3
	m3ClassServerOp    = 4
	m3ClassHello       = 5
	m3ClassReauth      = 6
	m3ClassEnter       = 7
	m3ClassLeave       = 8
	m3ClassAck         = 9
	m3ClassDisconnect  = 10
	m3ClassConnect     = 11
	m3ClassProgress    = 12 // .. through class 23
)

// m3RunLifecycleModel executes one bounded action sequence against a
// fresh engine and asserts the full invariant set after EVERY action
// (audit §53/§74/§77). The same runner backs the deterministic property
// test and the fuzz target, so the oracle is shared and never swapped
// for session.Allowed (§76).
func m3RunLifecycleModel(t *testing.T, label string, input []byte) {
	t.Helper()
	const (
		maxInputBytes = 512
		maxActions    = 256
	)
	if len(input) > maxInputBytes {
		input = input[:maxInputBytes]
	}
	e := newM3Engine(t, label)
	actions := 0
	for i := 0; i+m3StepBytes <= len(input) && actions < maxActions; i += m3StepBytes {
		e.runStep(t, input[i], input[i+1], input[i+2])
		actions++
	}
}

// runStep decodes one (control, variant, edge) triple into an action
// against one peer and runs the model→gateway→invariants cycle.
func (e *m3Engine) runStep(t *testing.T, control, variant, edge byte) {
	t.Helper()
	e.step++
	peerIdx := int(control >> 6)
	if peerIdx > 2 {
		peerIdx = 2
	}
	p := e.peers[peerIdx]
	seq := m3EdgeSeq[edge%uint8(len(m3EdgeSeq))]
	tick := m3EdgeSeq[(edge/5)%uint8(len(m3EdgeSeq))]
	mv := proto.MessageVersion1
	if edge%7 == 6 {
		mv = 65535 // future additive msg_version: routing must not change (§21)
	}
	class := control & 0x3f
	e.cur = fmt.Sprintf("peer=%s class=%d variant=%d edge=%d state=%s",
		p.name, class, variant, edge, p.state)
	e.want = make(map[string][]m3WantFrame)
	e.wantKick = make(map[string]bool)
	e.wantDelta = m3Counters{}
	e.flowMayChange = false

	// A dead peer's connection is gone: no frame can arrive on it. Frame
	// actions against a dead peer are no-ops (reconnect is the only way
	// back); only connect targets a dead peer legitimately. Invariants
	// still run after every step (§53).
	if !p.live && class != m3ClassConnect {
		e.cur += " dead-peer no-op"
		e.drainAndCheckFrames(t)
		e.assertInvariants(t)
		return
	}

	before := e.counters()
	beforeFlows := e.flowSnapshot(t)

	opcodeOf := func(v byte) uint16 { return 100 + uint16(v%27) }

	switch class {
	case m3ClassMalformed:
		e.cur += fmt.Sprintf(" malformed op=%d", opcodeOf(variant))
		e.doMalformedKnown(t, p, opcodeOf(variant), mv, seq, tick)
	case m3ClassShortFrame:
		e.cur += fmt.Sprintf(" shortFrame n=%d", variant%12)
		e.doShortFrame(t, p, int(variant%12))
	case m3ClassUnknown:
		op := m3UnknownOpcodes[variant%uint8(len(m3UnknownOpcodes))]
		e.cur += fmt.Sprintf(" unknown op=%d", op)
		e.doUnknownOpcode(t, p, op, seq, tick)
	case m3ClassServerOp:
		op := m3ServerOpcodes[variant%uint8(len(m3ServerOpcodes))]
		e.cur += fmt.Sprintf(" serverOp=%d", op)
		e.doServerOpcode(t, p, op, seq, tick)
	case m3ClassHello:
		e.cur += fmt.Sprintf(" hello valid=%v", variant%2 == 0)
		e.doHello(t, p, variant%2 == 0, mv, seq, tick)
	case m3ClassReauth:
		e.cur += fmt.Sprintf(" reauth mode=%d", variant%3)
		e.doReauth(t, p, int(variant%3), mv, seq, tick)
	case m3ClassEnter:
		slot := m3Slots[variant&3]
		e.cur += fmt.Sprintf(" enter slot=%d baselineFail=%v exitFail=%v",
			slot, variant&4 != 0, variant&8 != 0)
		e.doEnter(t, p, slot, variant&4 != 0, variant&8 != 0, mv, seq, tick)
	case m3ClassLeave:
		e.cur += fmt.Sprintf(" leave exitFail=%v", variant%2 == 1)
		e.doLeave(t, p, variant%2 == 1, mv, seq, tick)
	case m3ClassAck:
		e.cur += fmt.Sprintf(" ack variant=%d", variant%4)
		e.doAck(t, p, int(variant%4), mv, seq, tick)
	case m3ClassDisconnect:
		e.cur += " disconnect"
		e.disconnect(p)
	case m3ClassConnect:
		e.cur += " connect"
		e.connect(t, p)
	default:
		if class >= m3ClassProgress && class < m3ClassProgress+12 {
			// Progress operator: drive this peer's lifecycle FORWARD one
			// step (connect → hello → enter → leave), with the same random
			// seam variants as the dedicated actions (slot, baseline
			// failure, WorldExit failure). Purely random opcode draws
			// rarely line up A1 IN_WORLD with an A2 enter; this keeps the
			// duplicate-login takeover family (audit §46–§49) inside the
			// RANDOM stream, not just the curated corpus.
			switch {
			case !p.live:
				e.cur += " progress connect"
				e.connect(t, p)
			case p.state == session.StateConnected:
				e.cur += " progress hello"
				e.doHello(t, p, true, mv, seq, tick)
			case p.state == session.StateAuthenticated:
				slot := m3Slots[variant&3]
				e.cur += fmt.Sprintf(" progress enter slot=%d baselineFail=%v exitFail=%v",
					slot, variant&4 != 0, variant&8 != 0)
				e.doEnter(t, p, slot, variant&4 != 0, variant&8 != 0, mv, seq, tick)
			case p.state == session.StateInWorld:
				e.cur += fmt.Sprintf(" progress leave exitFail=%v", variant&2 != 0)
				e.doLeave(t, p, variant&2 != 0, mv, seq, tick)
			}
		} else {
			// The core §23 action: an arbitrary known opcode 100..126 with
			// a canonical valid payload; the model checks the hard-coded
			// permission table.
			opcode := opcodeOf(variant)
			e.cur += fmt.Sprintf(" op=%d", opcode)
			switch opcode {
			case proto.OpcodeHello:
				e.doHello(t, p, true, mv, seq, tick)
			case proto.OpcodeReauth:
				e.doReauth(t, p, 0, mv, seq, tick)
			case proto.OpcodeCharacterList:
				e.doCharList(t, p, mv, seq, tick)
			case proto.OpcodeCharacterCreate:
				e.doCreate(t, p, mv, seq, tick)
			case proto.OpcodeCharacterDelete:
				slot := m3Slots[edge&3]
				e.cur += fmt.Sprintf(" slot=%d", slot)
				e.doDelete(t, p, slot, mv, seq, tick)
			case proto.OpcodeEnterWorld:
				slot := m3Slots[edge&3]
				e.cur += fmt.Sprintf(" slot=%d", slot)
				e.doEnter(t, p, slot, false, false, mv, seq, tick)
			case proto.OpcodeAck:
				e.doAck(t, p, 0, mv, seq, tick)
			case proto.OpcodeLeaveWorld:
				e.doLeave(t, p, false, mv, seq, tick)
			default:
				e.doGameplay(t, p, opcode, mv, seq, tick)
			}
		}
	}

	e.drainAndCheckFrames(t)
	e.checkCounters(t, before)
	if !e.flowMayChange {
		e.checkFlowsUnchanged(t, beforeFlows)
	}
	e.assertInvariants(t)
}

// ---------------------------------------------------------------------------
// invariants (audit §53–§66), asserted after EVERY action
// ---------------------------------------------------------------------------

// m3AllChars is every fake character id, for index sweeps.
var m3AllChars = []int64{101, 102, 201, 202}

// assertInvariants compares the independent model against the real
// Registry: per-state structural invariants (§54–§57), flow/state
// equivalence (§58), character index (§59), sub index as sets (§60),
// one world-active session per account (§61), deterministic arbitration
// (§62), zero leaked guards (§63), and removed-session cleanup (§64).
func (e *m3Engine) assertInvariants(t *testing.T) {
	t.Helper()
	live := 0
	for _, p := range e.peers {
		if !p.live {
			// §64 removed-session invariant.
			if _, ok := e.reg.Get(p.sid); ok {
				e.failf(t, "%s dead but still registered as sid %d", p.name, uint64(p.sid))
			}
			if _, err := e.reg.FlowState(p.sid); !errors.Is(err, session.ErrNotFound) {
				e.failf(t, "%s dead but FlowState = %v, want ErrNotFound", p.name, err)
			}
			for _, c := range m3AllChars {
				if owner, ok := e.reg.SessionByCharacter(c); ok && owner == p.sid {
					e.failf(t, "character %d index still points at dead %s sid %d", c, p.name, uint64(p.sid))
				}
			}
			continue
		}
		live++
		snap, ok := e.reg.Get(p.sid)
		if !ok {
			e.failf(t, "%s live but missing from registry", p.name)
		}
		if snap.State != p.state {
			e.failf(t, "%s state = %s, model says %s — state table violation", p.name, snap.State, p.state)
		}
		switch p.state {
		case session.StateConnected: // §54
			if snap.Authenticated || snap.Sub != "" || snap.AccountID != 0 || snap.HasCharacter {
				e.failf(t, "%s CONNECTED invariant: %+v", p.name, snap)
			}
		case session.StateAuthenticated: // §55
			if !snap.Authenticated || snap.Sub != p.sub || snap.AccountID != p.account || snap.HasCharacter {
				e.failf(t, "%s AUTHENTICATED invariant: %+v (want sub=%s acct=%d)",
					p.name, snap, p.sub, p.account)
			}
		case session.StateCharacterSelected: // §56
			if !snap.Authenticated || snap.Sub != p.sub || snap.AccountID != p.account ||
				!snap.HasCharacter || snap.CharacterID != p.char || p.char == 0 {
				e.failf(t, "%s CHARACTER_SELECTED invariant: %+v model char=%d", p.name, snap, p.char)
			}
			if owner, ok := e.reg.SessionByCharacter(p.char); !ok || owner != p.sid {
				e.failf(t, "%s character index = %d,%v, want sid %d", p.name, owner, ok, uint64(p.sid))
			}
		case session.StateInWorld: // §57
			if !snap.Authenticated || snap.Sub != p.sub || snap.AccountID != p.account ||
				!snap.HasCharacter || snap.CharacterID != p.char || p.char == 0 {
				e.failf(t, "%s IN_WORLD invariant: %+v model char=%d", p.name, snap, p.char)
			}
			if owner, ok := e.reg.SessionByCharacter(p.char); !ok || owner != p.sid {
				e.failf(t, "%s character index = %d,%v, want sid %d", p.name, owner, ok, uint64(p.sid))
			}
		}
		// §58 flow/state equivalence at every stable point.
		fs, err := e.reg.FlowState(p.sid)
		if err != nil {
			e.failf(t, "%s FlowState: %v", p.name, err)
		}
		if fs.Active != (p.state == session.StateInWorld) {
			e.failf(t, "%s flowActive = %v in %s — must equal IN_WORLD", p.name, fs.Active, p.state)
		}
	}
	if e.reg.Len() != live {
		e.failf(t, "Registry.Len = %d, model live sessions = %d", e.reg.Len(), live)
	}

	// §59 character-index invariant: every fake character resolves to
	// exactly the model's live world-active owner (or nobody).
	for _, c := range m3AllChars {
		wantOwner := session.ID(0)
		found := false
		for _, p := range e.peers {
			if p.live && p.char == c &&
				(p.state == session.StateCharacterSelected || p.state == session.StateInWorld) {
				wantOwner, found = p.sid, true
			}
		}
		owner, ok := e.reg.SessionByCharacter(c)
		if found != ok || (ok && owner != wantOwner) {
			e.failf(t, "character %d index = %d,%v, want %d,%v", c, owner, ok, wantOwner, found)
		}
	}

	// §60 sub-index invariant, compared as SETS (map order is random).
	for _, sub := range []string{"m3-account-a", "m3-account-b"} {
		want := map[session.ID]bool{}
		for _, p := range e.peers {
			if p.live && p.sub == sub && p.state != session.StateConnected {
				want[p.sid] = true
			}
		}
		got := map[session.ID]bool{}
		for _, id := range e.reg.SessionsBySub(sub) {
			got[id] = true
		}
		if len(got) != len(want) {
			e.failf(t, "SessionsBySub(%s) set = %v, want %v", sub, got, want)
		}
		for id := range got {
			if !want[id] {
				e.failf(t, "SessionsBySub(%s) contains unexpected sid %d", sub, uint64(id))
			}
		}
	}

	// §61/§62 one world-active session per account + deterministic
	// arbitration (never resolved by picking one).
	for _, acct := range []int64{1, 2} {
		var world []*m3Peer
		for _, p := range e.peers {
			if p.live && p.account == acct &&
				(p.state == session.StateCharacterSelected || p.state == session.StateInWorld) {
				world = append(world, p)
			}
		}
		if len(world) > 1 {
			e.failf(t, "account %d has %d world-active model sessions", acct, len(world))
		}
		snap, found, err := e.reg.WorldSessionForAccount(acct, 0)
		if err != nil {
			e.failf(t, "WorldSessionForAccount(%d) = %v", acct, err)
		}
		if found != (len(world) == 1) {
			e.failf(t, "WorldSessionForAccount(%d) found=%v, model has %d", acct, found, len(world))
		}
		if found && snap.ID != world[0].sid {
			e.failf(t, "WorldSessionForAccount(%d) = sid %d, want %d", acct, uint64(snap.ID), uint64(world[0].sid))
		}
		if len(world) == 1 {
			if _, again, err := e.reg.WorldSessionForAccount(acct, world[0].sid); err != nil || again {
				e.failf(t, "WorldSessionForAccount(%d, exclude owner) = %v,%v", acct, again, err)
			}
		}
	}

	// §63 no leaked per-account lifecycle guards — including under the
	// failing baseline/WorldExit paths.
	if n := e.reg.GuardCount(); n != 0 {
		e.failf(t, "GuardCount = %d, want 0 (guard leaked)", n)
	}
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

// TestM3LifecycleRandomSequences is the M3 exit criterion (audit §70/
// §71): 128 deterministic seeds × 128 actions = 16,384 modeled
// lifecycle steps over the real gateway, with the independent §6.1
// oracle and the full invariant set checked after every step. No
// sleeps, no extra goroutines, no PG/Docker/network in the loop; the
// seed alone reproduces any failure.
func TestM3LifecycleRandomSequences(t *testing.T) {
	const seeds = 128
	const actions = 128
	for seed := uint64(1); seed <= seeds; seed++ {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			rng := simtest.NewRNG(seed)
			input := make([]byte, actions*m3StepBytes)
			for i := range input {
				input[i] = byte(rng.Uint64())
			}
			m3RunLifecycleModel(t, fmt.Sprintf("seed=%d", seed), input)
		})
	}
}

// TestM3LifecycleSelectedStateRandomProbes audits CHARACTER_SELECTED
// deliberately (audit §45/§80): the synchronous enter handler never
// exposes it as a stable network-visible state, so each probe
// authenticates, moves into CHARACTER_SELECTED through the REAL
// Registry primitive BeginEnterWorld, sends one opcode through the real
// Server.handleBinary, checks the independent selected-state oracle,
// and rolls back through the real AbortEnterWorld. All 27 C→S opcodes
// are covered across repeated rounds; no CompareAndSetState shortcut
// ever manufactures the half-state, and each probe holds at most one
// world-active session per account.
func TestM3LifecycleSelectedStateRandomProbes(t *testing.T) {
	const rounds = 4
	for r := 0; r < rounds; r++ {
		rng := simtest.NewRNG(uint64(1000 + r))
		for opcode := uint16(100); opcode <= 126; opcode++ {
			e := newM3Engine(t, fmt.Sprintf("probe r=%d op=%d", r, opcode))
			p := e.peers[rng.Intn(len(e.peers))]
			char := e.chars.charOf(p.account, 0)

			// Authenticate through the real gateway path.
			e.step++
			e.cur = fmt.Sprintf("probe-hello peer=%s", p.name)
			e.wantDelta = m3Counters{}
			e.doHello(t, p, true, proto.MessageVersion1, 1, 1)
			e.drainAndCheckFrames(t)
			e.assertInvariants(t)

			// Real registry primitive into the provisional state.
			if err := e.reg.BeginEnterWorld(p.sid, char); err != nil {
				t.Fatalf("probe: BeginEnterWorld: %v", err)
			}
			p.state = session.StateCharacterSelected
			p.char = char
			e.assertInvariants(t)

			// One opcode through the real server; the independent oracle
			// decides what CHARACTER_SELECTED must allow: 101/121/125 are
			// allowed (125 silently), everything else is bad_state.
			e.step++
			e.cur = fmt.Sprintf("probe peer=%s op=%d", p.name, opcode)
			e.want = make(map[string][]m3WantFrame)
			e.wantDelta = m3Counters{}
			before := e.counters()
			var encode func(*proto.Encoder) error
			switch opcode {
			case proto.OpcodeHello:
				encode = m3HelloPayload(p.token)
			case proto.OpcodeReauth:
				encode = m3ReauthPayload(p.tokenFresh)
			default:
				encode = m3RawPayload(m3CanonicalPayload(t, opcode))
			}
			if !m3ModelAllows(session.StateCharacterSelected, opcode) {
				e.want202(p, proto.ErrorCodeBadState)
			} else {
				switch opcode {
				case proto.OpcodeReauth:
					e.wantFrame(p, m3WantFrame{opcode: proto.OpcodeReauthOK})
					e.wantDelta.validator = 1
				case proto.OpcodeCharacterList:
					e.wantFrame(p, m3WantFrame{opcode: proto.OpcodeCharacterListResult})
					e.wantDelta.list = 1
				case proto.OpcodeAck:
					// silent no-op before the flow epoch exists
				}
			}
			e.sendFrame(t, p, opcode, proto.MessageVersion1,
				m3EdgeSeq[rng.Intn(len(m3EdgeSeq))], m3EdgeSeq[rng.Intn(len(m3EdgeSeq))], encode)
			e.drainAndCheckFrames(t)
			e.checkCounters(t, before)
			e.assertInvariants(t)

			// Real rollback primitive; the binding must fully restore.
			if err := e.reg.AbortEnterWorld(p.sid, char); err != nil {
				t.Fatalf("probe: AbortEnterWorld: %v", err)
			}
			p.state = session.StateAuthenticated
			p.char = 0
			e.assertInvariants(t)
		}
	}
}

// ---------------------------------------------------------------------------
// fuzz target (audit §73–§77)
// ---------------------------------------------------------------------------

// m3FuzzStep encodes one action triple.
func m3FuzzStep(peer, class int, variant, edge byte) []byte {
	return []byte{byte(peer<<6 | class), variant, edge}
}

// m3FuzzSeedCorpus is the curated corpus (audit §75): one entry per
// frozen M3 lifecycle scenario, executed as plain seed tests by
// `go test` even when fuzzing is not enabled.
func m3FuzzSeedCorpus() [][]byte {
	concat := func(steps ...[]byte) []byte {
		var out []byte
		for _, s := range steps {
			out = append(out, s...)
		}
		return out
	}
	return [][]byte{
		// hello → enter → gameplay → leave
		concat(m3FuzzStep(0, m3ClassHello, 0, 0), m3FuzzStep(0, m3ClassEnter, 0, 0), m3FuzzStep(0, m3ClassKnownOpcode, 2, 0), m3FuzzStep(0, m3ClassLeave, 0, 0)),
		// hello → create → delete
		concat(m3FuzzStep(0, m3ClassHello, 0, 0), m3FuzzStep(0, m3ClassKnownOpcode, 22, 0), m3FuzzStep(0, m3ClassKnownOpcode, 23, 1)),
		// bad-state opcodes before hello (121/124/125/126/101)
		concat(m3FuzzStep(0, m3ClassKnownOpcode, 21, 0), m3FuzzStep(0, m3ClassKnownOpcode, 24, 0), m3FuzzStep(0, m3ClassKnownOpcode, 25, 0), m3FuzzStep(0, m3ClassKnownOpcode, 26, 0), m3FuzzStep(0, m3ClassKnownOpcode, 1, 0)),
		// hello → enter → ACK → gameplay
		concat(m3FuzzStep(0, m3ClassHello, 0, 0), m3FuzzStep(0, m3ClassEnter, 0, 0), m3FuzzStep(0, m3ClassAck, 0, 0), m3FuzzStep(0, m3ClassAck, 2, 0), m3FuzzStep(0, m3ClassKnownOpcode, 2, 0)),
		// same-account A1 enter → A2 takeover (same character)
		concat(m3FuzzStep(0, m3ClassHello, 0, 0), m3FuzzStep(0, m3ClassEnter, 0, 0), m3FuzzStep(1, m3ClassHello, 0, 0), m3FuzzStep(1, m3ClassEnter, 0, 0)),
		// takeover WorldExit failure (variant bit 3)
		concat(m3FuzzStep(0, m3ClassHello, 0, 0), m3FuzzStep(0, m3ClassEnter, 0, 0), m3FuzzStep(1, m3ClassHello, 0, 0), m3FuzzStep(1, m3ClassEnter, 8, 0)),
		// takeover success → new baseline failure (variant bit 2)
		concat(m3FuzzStep(0, m3ClassHello, 0, 0), m3FuzzStep(0, m3ClassEnter, 0, 0), m3FuzzStep(1, m3ClassHello, 0, 0), m3FuzzStep(1, m3ClassEnter, 4, 0)),
		// different-character takeover (A1 slot 0, A2 slot 1)
		concat(m3FuzzStep(0, m3ClassHello, 0, 0), m3FuzzStep(0, m3ClassEnter, 0, 0), m3FuzzStep(1, m3ClassHello, 0, 0), m3FuzzStep(1, m3ClassEnter, 1, 0)),
		// leave failure → retry → leave success
		concat(m3FuzzStep(0, m3ClassHello, 0, 0), m3FuzzStep(0, m3ClassEnter, 0, 0), m3FuzzStep(0, m3ClassLeave, 1, 0), m3FuzzStep(0, m3ClassLeave, 0, 0)),
		// malformed known-opcode sequence (122, then 125 and 102 in world)
		concat(m3FuzzStep(0, m3ClassHello, 0, 0), m3FuzzStep(0, m3ClassMalformed, 22, 0), m3FuzzStep(0, m3ClassEnter, 0, 0), m3FuzzStep(0, m3ClassMalformed, 25, 0), m3FuzzStep(0, m3ClassMalformed, 2, 0)),
		// unknown / client-sent S→C opcode sequence
		concat(m3FuzzStep(0, m3ClassHello, 0, 0), m3FuzzStep(0, m3ClassUnknown, 0, 0), m3FuzzStep(0, m3ClassUnknown, 3, 0), m3FuzzStep(0, m3ClassServerOp, 0, 0), m3FuzzStep(0, m3ClassServerOp, 2, 0)),
		// disconnect → reconnect → hello → enter (fresh session/seq epoch)
		concat(m3FuzzStep(0, m3ClassHello, 0, 0), m3FuzzStep(0, m3ClassEnter, 0, 0), m3FuzzStep(0, m3ClassDisconnect, 0, 0), m3FuzzStep(0, m3ClassConnect, 0, 0), m3FuzzStep(0, m3ClassHello, 0, 0), m3FuzzStep(0, m3ClassEnter, 0, 0)),
		// reauth fresh → mismatch → invalid
		concat(m3FuzzStep(0, m3ClassHello, 0, 0), m3FuzzStep(0, m3ClassReauth, 0, 0), m3FuzzStep(0, m3ClassReauth, 1, 0), m3FuzzStep(0, m3ClassReauth, 2, 0)),
	}
}

// FuzzM3LifecycleModel turns arbitrary bytes into bounded, meaningful
// lifecycle action sequences (≤512 input bytes, ≤256 actions per
// execution) against the same independent oracle and invariant set as
// the deterministic property test (audit §73–§77).
func FuzzM3LifecycleModel(f *testing.F) {
	for _, seed := range m3FuzzSeedCorpus() {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		m3RunLifecycleModel(t, "fuzz", data)
	})
}
