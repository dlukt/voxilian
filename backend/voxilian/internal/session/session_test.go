package session

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stubConn is the minimal test Connection: it mimics the production
// writer contract (build runs only while the slot is held) and records
// frames plus Close/CloseNow.
type stubConn struct {
	mu        sync.Mutex
	frames    [][]byte
	buildErr  error
	closed    []string
	closeNows int
}

func (c *stubConn) WriteBinary(_ context.Context, build BinaryFrameBuilder) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.buildErr != nil {
		return c.buildErr
	}
	frame, err := build()
	if err != nil {
		return err
	}
	c.frames = append(c.frames, frame)
	return nil
}

func (c *stubConn) Close(reason string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = append(c.closed, reason)
	return nil
}

func (c *stubConn) CloseNow() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeNows++
	return nil
}

func TestStateString(t *testing.T) {
	cases := map[State]string{
		StateConnected:         "CONNECTED",
		StateAuthenticated:     "AUTHENTICATED",
		StateCharacterSelected: "CHARACTER_SELECTED",
		StateInWorld:           "IN_WORLD",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("State(%d).String() = %q, want %q", uint8(s), got, want)
		}
	}
}

// specAllowed transcribes the exact §6.1 permission table: the set of
// states each recognized C→S opcode is permitted in.
func specAllowed() map[uint16]map[State]bool {
	allow := func(states ...State) map[State]bool {
		m := make(map[State]bool)
		for _, s := range states {
			m[s] = true
		}
		return m
	}
	authAndLater := allow(StateAuthenticated, StateCharacterSelected, StateInWorld)
	inWorld := allow(StateInWorld)
	m := map[uint16]map[State]bool{
		100: allow(StateConnected), // hello
		101: authAndLater,          // reauth
		121: authAndLater,          // character_list
		122: allow(StateAuthenticated),
		123: allow(StateAuthenticated),
		124: allow(StateAuthenticated),
		125: allow(StateCharacterSelected, StateInWorld), // ack
		126: inWorld,                                     // leave_world
	}
	for op := uint16(102); op <= 120; op++ {
		m[op] = inWorld // gameplay
	}
	return m
}

func TestPermissionMatrixExhaustive(t *testing.T) {
	want := specAllowed()
	if len(want) != 27 {
		t.Fatalf("spec table has %d opcodes, want 27 (100–126)", len(want))
	}
	states := []State{StateConnected, StateAuthenticated, StateCharacterSelected, StateInWorld}
	checked := 0
	for op := uint16(100); op <= 126; op++ {
		allowedStates, ok := want[op]
		if !ok {
			t.Fatalf("opcode %d missing from spec table", op)
		}
		if !IsClientOpcode(op) {
			t.Errorf("IsClientOpcode(%d) = false, want true", op)
		}
		for _, s := range states {
			checked++
			if got := Allowed(s, op); got != allowedStates[s] {
				t.Errorf("Allowed(%s, %d) = %v, want %v", s, op, got, allowedStates[s])
			}
		}
	}
	if checked != 108 {
		t.Errorf("checked %d combinations, want 108 (27 opcodes × 4 states)", checked)
	}
}

func TestNotClientOpcodes(t *testing.T) {
	nonClient := []uint16{0, 99, 127, 199, 200, 202, 220, 221, 65535}
	states := []State{StateConnected, StateAuthenticated, StateCharacterSelected, StateInWorld}
	for _, op := range nonClient {
		if IsClientOpcode(op) {
			t.Errorf("IsClientOpcode(%d) = true, want false", op)
		}
		for _, s := range states {
			if Allowed(s, op) {
				t.Errorf("Allowed(%s, %d) = true, want false", s, op)
			}
		}
	}
}

func TestCreateGetRemove(t *testing.T) {
	r := NewRegistry()
	conn := &stubConn{}
	id := r.Create(conn)
	if id == 0 {
		t.Fatal("first session ID must not be 0")
	}
	snap, ok := r.Get(id)
	if !ok {
		t.Fatal("Get(create) = false")
	}
	if snap.State != StateConnected {
		t.Errorf("initial state = %s, want CONNECTED", snap.State)
	}
	if snap.Sub != "" || snap.AccountID != 0 || snap.Authenticated || snap.HasCharacter {
		t.Errorf("initial identity not empty: %+v", snap)
	}
	if !snap.TokenExp.IsZero() {
		t.Errorf("initial TokenExp not zero: %v", snap.TokenExp)
	}
	if r.Len() != 1 {
		t.Errorf("Len() = %d, want 1", r.Len())
	}
	// IDs are never reused: a second session gets a distinct ID.
	id2 := r.Create(nil)
	if id2 == id {
		t.Errorf("duplicate session ID %d", id)
	}
	r.Remove(id)
	if _, ok := r.Get(id); ok {
		t.Error("Get after Remove = true, want false")
	}
	if r.Len() != 1 {
		t.Errorf("Len() after Remove = %d, want 1", r.Len())
	}
	// Remove is idempotent.
	r.Remove(id)
	r.Remove(99999)
	if r.Len() != 1 {
		t.Errorf("Len() after idempotent Removes = %d, want 1", r.Len())
	}
}

func TestAuthenticate(t *testing.T) {
	r := NewRegistry()
	id := r.Create(nil)
	exp := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := r.Authenticate(id, "test-sub", 42, exp); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	snap, _ := r.Get(id)
	if snap.State != StateAuthenticated {
		t.Errorf("state = %s, want AUTHENTICATED", snap.State)
	}
	if snap.Sub != "test-sub" || snap.AccountID != 42 || !snap.Authenticated {
		t.Errorf("identity not established: %+v", snap)
	}
	if !snap.TokenExp.Equal(exp) {
		t.Errorf("TokenExp = %v, want %v", snap.TokenExp, exp)
	}
	if got := r.SessionsBySub("test-sub"); len(got) != 1 || got[0] != id {
		t.Errorf("SessionsBySub = %v, want [%d]", got, id)
	}
	// Second authenticate on the same session is rejected.
	if err := r.Authenticate(id, "test-sub", 42, exp); !errors.Is(err, ErrBadState) {
		t.Errorf("second Authenticate err = %v, want ErrBadState", err)
	}
	// Empty sub is rejected and leaves the session untouched.
	id2 := r.Create(nil)
	if err := r.Authenticate(id2, "", 7, exp); !errors.Is(err, ErrBadState) {
		t.Errorf("empty-sub Authenticate err = %v, want ErrBadState", err)
	}
	if snap, _ := r.Get(id2); snap.State != StateConnected {
		t.Errorf("state after failed auth = %s, want CONNECTED", snap.State)
	}
	if err := r.Authenticate(99999, "x", 1, exp); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown-ID Authenticate err = %v, want ErrNotFound", err)
	}
}

func TestMultipleSessionsOneSub(t *testing.T) {
	r := NewRegistry()
	exp := time.Now()
	a := r.Create(nil)
	b := r.Create(nil)
	if err := r.Authenticate(a, "shared-sub", 1, exp); err != nil {
		t.Fatal(err)
	}
	if err := r.Authenticate(b, "shared-sub", 1, exp); err != nil {
		t.Fatal(err)
	}
	got := r.SessionsBySub("shared-sub")
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	if len(got) != 2 || got[0] != a || got[1] != b {
		t.Errorf("SessionsBySub = %v, want [%d %d]", got, a, b)
	}
	r.Remove(a)
	if got := r.SessionsBySub("shared-sub"); len(got) != 1 || got[0] != b {
		t.Errorf("SessionsBySub after one Remove = %v, want [%d]", got, b)
	}
	r.Remove(b)
	if got := r.SessionsBySub("shared-sub"); len(got) != 0 {
		t.Errorf("SessionsBySub after both Removed = %v, want []", got)
	}
}

func TestReauthenticate(t *testing.T) {
	r := NewRegistry()
	id := r.Create(nil)
	exp1 := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	exp2 := time.Date(2026, 9, 4, 13, 0, 0, 0, time.UTC)
	if err := r.Authenticate(id, "sub-1", 42, exp1); err != nil {
		t.Fatal(err)
	}
	if err := r.Reauthenticate(id, "sub-1", 42, exp2); err != nil {
		t.Fatalf("Reauthenticate same identity: %v", err)
	}
	if snap, _ := r.Get(id); !snap.TokenExp.Equal(exp2) {
		t.Errorf("TokenExp = %v, want %v", snap.TokenExp, exp2)
	}
	// Identity change via different sub is rejected; expiry untouched.
	if err := r.Reauthenticate(id, "other-sub", 42, exp1); !errors.Is(err, ErrIdentityMismatch) {
		t.Errorf("changed-sub Reauthenticate err = %v, want ErrIdentityMismatch", err)
	}
	// Identity change via different accountID is rejected too.
	if err := r.Reauthenticate(id, "sub-1", 43, exp1); !errors.Is(err, ErrIdentityMismatch) {
		t.Errorf("changed-account Reauthenticate err = %v, want ErrIdentityMismatch", err)
	}
	if snap, _ := r.Get(id); !snap.TokenExp.Equal(exp2) {
		t.Errorf("TokenExp after rejected reauth = %v, want %v", snap.TokenExp, exp2)
	}
	if snap, _ := r.Get(id); snap.Sub != "sub-1" || snap.AccountID != 42 {
		t.Errorf("identity changed by rejected reauth: %+v", snap)
	}
	// Reauthenticating a CONNECTED session is a state error.
	connected := r.Create(nil)
	if err := r.Reauthenticate(connected, "sub-1", 42, exp2); !errors.Is(err, ErrBadState) {
		t.Errorf("CONNECTED Reauthenticate err = %v, want ErrBadState", err)
	}
	if err := r.Reauthenticate(99999, "sub-1", 42, exp2); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown-ID Reauthenticate err = %v, want ErrNotFound", err)
	}
}

func TestCharacterBinding(t *testing.T) {
	r := NewRegistry()
	a := r.Create(nil)
	b := r.Create(nil)
	if err := r.BindCharacter(a, 7); err != nil {
		t.Fatalf("BindCharacter: %v", err)
	}
	if owner, ok := r.SessionByCharacter(7); !ok || owner != a {
		t.Errorf("SessionByCharacter(7) = %d,%v want %d,true", owner, ok, a)
	}
	if snap, _ := r.Get(a); !snap.HasCharacter || snap.CharacterID != 7 {
		t.Errorf("snapshot after bind: %+v", snap)
	}
	// Same character on a second session is rejected without overwrite.
	if err := r.BindCharacter(b, 7); !errors.Is(err, ErrCharacterInUse) {
		t.Errorf("duplicate bind err = %v, want ErrCharacterInUse", err)
	}
	if owner, _ := r.SessionByCharacter(7); owner != a {
		t.Errorf("index overwritten by rejected bind: owner = %d", owner)
	}
	// Unbind releases the index for another session.
	if err := r.UnbindCharacter(a); err != nil {
		t.Fatalf("UnbindCharacter: %v", err)
	}
	if _, ok := r.SessionByCharacter(7); ok {
		t.Error("character still indexed after unbind")
	}
	if err := r.BindCharacter(b, 7); err != nil {
		t.Fatalf("bind after unbind: %v", err)
	}
	// Unbind is idempotent.
	if err := r.UnbindCharacter(a); err != nil {
		t.Errorf("idempotent UnbindCharacter: %v", err)
	}
	if err := r.BindCharacter(99999, 9); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown-ID bind err = %v, want ErrNotFound", err)
	}
	if err := r.UnbindCharacter(99999); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown-ID unbind err = %v, want ErrNotFound", err)
	}
}

func TestRemoveCleansAllIndexes(t *testing.T) {
	r := NewRegistry()
	exp := time.Now()
	id := r.Create(nil)
	if err := r.Authenticate(id, "gone-sub", 11, exp); err != nil {
		t.Fatal(err)
	}
	if err := r.BindCharacter(id, 99); err != nil {
		t.Fatal(err)
	}
	r.Remove(id)
	if got := r.SessionsBySub("gone-sub"); len(got) != 0 {
		t.Errorf("sub index not cleaned: %v", got)
	}
	if _, ok := r.SessionByCharacter(99); ok {
		t.Error("character index not cleaned")
	}
	// A stale session's cleanup must not clobber a newer binding of the
	// same character taken over by another session.
	s1 := r.Create(nil)
	s2 := r.Create(nil)
	if err := r.BindCharacter(s1, 5); err != nil {
		t.Fatal(err)
	}
	if err := r.UnbindCharacter(s1); err != nil {
		t.Fatal(err)
	}
	if err := r.BindCharacter(s2, 5); err != nil {
		t.Fatal(err)
	}
	r.Remove(s1)
	if owner, ok := r.SessionByCharacter(5); !ok || owner != s2 {
		t.Errorf("SessionByCharacter(5) = %d,%v after stale Remove, want %d,true", owner, ok, s2)
	}
}

func TestCompareAndSetState(t *testing.T) {
	r := NewRegistry()
	id := r.Create(nil)
	if err := r.CompareAndSetState(id, StateConnected, StateAuthenticated); err != nil {
		t.Fatalf("CAS CONNECTED→AUTHENTICATED: %v", err)
	}
	if snap, _ := r.Get(id); snap.State != StateAuthenticated {
		t.Errorf("state = %s, want AUTHENTICATED", snap.State)
	}
	// Mismatch leaves state untouched.
	if err := r.CompareAndSetState(id, StateConnected, StateInWorld); !errors.Is(err, ErrBadState) {
		t.Errorf("mismatched CAS err = %v, want ErrBadState", err)
	}
	if snap, _ := r.Get(id); snap.State != StateAuthenticated {
		t.Errorf("state after failed CAS = %s, want AUTHENTICATED", snap.State)
	}
	if err := r.CompareAndSetState(99999, StateConnected, StateAuthenticated); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown-ID CAS err = %v, want ErrNotFound", err)
	}
}

func TestNextServerSeqDeterministic(t *testing.T) {
	r := NewRegistry()
	id := r.Create(nil)
	for want := uint32(1); want <= 3; want++ {
		got, err := r.NextServerSeq(id)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("NextServerSeq() = %d, want %d", got, want)
		}
	}
	if _, err := r.NextServerSeq(99999); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown-ID seq err = %v, want ErrNotFound", err)
	}
}

func TestNextServerSeqWrap(t *testing.T) {
	r := NewRegistry()
	id := r.Create(nil)
	r.mu.Lock()
	r.byID[id].serverSeq.Store(math.MaxUint32 - 1)
	r.mu.Unlock()
	got, err := r.NextServerSeq(id)
	if err != nil || got != math.MaxUint32 {
		t.Fatalf("NextServerSeq() = %d,%v want %d,nil", got, err, uint32(math.MaxUint32))
	}
	got, err = r.NextServerSeq(id)
	if err != nil || got != 0 {
		t.Fatalf("wrap NextServerSeq() = %d,%v want 0,nil", got, err)
	}
	got, err = r.NextServerSeq(id)
	if err != nil || got != 1 {
		t.Fatalf("post-wrap NextServerSeq() = %d,%v want 1,nil", got, err)
	}
}

func TestNextServerSeqConcurrent(t *testing.T) {
	r := NewRegistry()
	id := r.Create(nil)
	const workers = 8
	const perWorker = 250
	var wg sync.WaitGroup
	results := make(chan uint32, workers*perWorker)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				seq, err := r.NextServerSeq(id)
				if err != nil {
					t.Errorf("NextServerSeq: %v", err)
					return
				}
				results <- seq
			}
		}()
	}
	wg.Wait()
	close(results)
	seen := make(map[uint32]bool, workers*perWorker)
	for seq := range results {
		if seen[seq] {
			t.Fatalf("duplicate server seq %d", seq)
		}
		seen[seq] = true
	}
	if len(seen) != workers*perWorker {
		t.Errorf("allocated %d seqs, want %d", len(seen), workers*perWorker)
	}
}

func TestLockAccountSerializesSameAccount(t *testing.T) {
	r := NewRegistry()
	acquiredA := make(chan struct{})
	startedB := make(chan struct{})
	acquiredB := make(chan struct{})
	doneB := make(chan struct{})

	unlockA := r.LockAccount(42)
	close(acquiredA)
	go func() {
		defer close(doneB)
		close(startedB)
		unlockB := r.LockAccount(42)
		defer unlockB()
		close(acquiredB)
	}()

	<-acquiredA
	<-startedB
	// While A holds the guard, B must not have acquired it. This holds
	// regardless of how far B has been scheduled: acquired is only
	// closed after LockAccount returns.
	select {
	case <-acquiredB:
		t.Fatal("same-account guard acquired concurrently")
	default:
	}
	unlockA()
	<-doneB
	select {
	case <-acquiredB:
	default:
		t.Fatal("same-account waiter never acquired the guard")
	}
	if n := r.GuardCount(); n != 0 {
		t.Errorf("GuardCount() = %d, want 0 (guard released)", n)
	}
}

func TestLockAccountDifferentAccountsConcurrent(t *testing.T) {
	r := NewRegistry()
	releaseA := make(chan struct{})
	acquiredA := make(chan struct{})
	acquiredB := make(chan struct{})
	releaseB := make(chan struct{})
	doneA := make(chan struct{})
	doneB := make(chan struct{})

	go func() {
		defer close(doneA)
		unlock := r.LockAccount(1)
		defer unlock()
		close(acquiredA)
		<-releaseA
	}()
	go func() {
		defer close(doneB)
		unlock := r.LockAccount(2)
		defer unlock()
		close(acquiredB)
		<-releaseB
	}()

	<-acquiredA
	<-acquiredB // B proceeded while A still holds its guard.
	close(releaseA)
	close(releaseB)
	<-doneA
	<-doneB
	if n := r.GuardCount(); n != 0 {
		t.Errorf("GuardCount() = %d, want 0", n)
	}
}

func TestRegistryConcurrentSmoke(t *testing.T) {
	r := NewRegistry()
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				id := r.Create(nil)
				sub := fmt.Sprintf("sub-%d", w)
				_ = r.Authenticate(id, sub, int64(w), time.Now())
				_ = r.BindCharacter(id, int64(w*1000+i))
				_, _ = r.NextServerSeq(id)
				_ = r.CompareAndSetState(id, StateAuthenticated, StateCharacterSelected)
				_ = r.UnbindCharacter(id)
				r.Remove(id)
			}
		}(w)
	}
	wg.Wait()
	if n := r.Len(); n != 0 {
		t.Errorf("Len() = %d, want 0 after concurrent churn", n)
	}
}

func setupInWorld(t *testing.T, r *Registry, sub string, accountID, characterID int64) ID {
	t.Helper()
	id := r.Create(nil)
	exp := time.Now().Add(time.Hour)
	if err := r.Authenticate(id, sub, accountID, exp); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if err := r.BindCharacter(id, characterID); err != nil {
		t.Fatalf("bind: %v", err)
	}
	for _, s := range []State{StateCharacterSelected, StateInWorld} {
		cur, _ := r.Get(id)
		if err := r.CompareAndSetState(id, cur.State, s); err != nil {
			t.Fatalf("cas to %s: %v", s, err)
		}
	}
	return id
}

func TestGetByCharacter(t *testing.T) {
	r := NewRegistry()
	if _, ok := r.GetByCharacter(99); ok {
		t.Fatal("unbound character resolved")
	}
	id := setupInWorld(t, r, "sub", 7, 99)
	snap, ok := r.GetByCharacter(99)
	if !ok {
		t.Fatal("bound character not resolved")
	}
	if snap.ID != id || snap.Sub != "sub" || snap.AccountID != 7 ||
		!snap.HasCharacter || snap.CharacterID != 99 || snap.State != StateInWorld {
		t.Errorf("snapshot = %+v", snap)
	}
	// Returned snapshots are copies: mutating the caller copy cannot
	// affect the registry.
	snap.State = StateConnected
	if cur, _ := r.Get(id); cur.State != StateInWorld {
		t.Errorf("registry mutated through snapshot: %+v", cur)
	}
	r.Remove(id)
	if _, ok := r.GetByCharacter(99); ok {
		t.Error("removed session still resolved")
	}
}

func TestCompleteLeaveWorld(t *testing.T) {
	r := NewRegistry()
	expBefore := time.Now().Add(time.Hour)
	id := r.Create(nil)
	if err := r.Authenticate(id, "leaver", 11, expBefore); err != nil {
		t.Fatal(err)
	}
	if err := r.BindCharacter(id, 55); err != nil {
		t.Fatal(err)
	}
	snap0, _ := r.Get(id)
	if err := r.CompareAndSetState(id, snap0.State, StateCharacterSelected); err != nil {
		t.Fatal(err)
	}
	if err := r.CompareAndSetState(id, StateCharacterSelected, StateInWorld); err != nil {
		t.Fatal(err)
	}
	if _, err := r.NextServerSeq(id); err != nil {
		t.Fatal(err)
	}

	if err := r.CompleteLeaveWorld(id, 55); err != nil {
		t.Fatalf("leave: %v", err)
	}
	snap, ok := r.Get(id)
	if !ok {
		t.Fatal("session vanished")
	}
	if snap.State != StateAuthenticated {
		t.Errorf("state = %s, want AUTHENTICATED", snap.State)
	}
	if snap.HasCharacter || snap.CharacterID != 0 {
		t.Errorf("binding not cleared: %+v", snap)
	}
	if _, taken := r.SessionByCharacter(55); taken {
		t.Error("character index not removed")
	}
	// Identity preserved: sub, account, expiry, auth flag, seq counter.
	if snap.Sub != "leaver" || snap.AccountID != 11 || !snap.Authenticated ||
		!snap.TokenExp.Equal(expBefore) {
		t.Errorf("identity changed: %+v", snap)
	}
	if seq, err := r.NextServerSeq(id); err != nil || seq != 2 {
		t.Errorf("server seq = %d, %v; want 2 (counter preserved)", seq, err)
	}
	// T5b: the same registry mutation cleared the flow epoch — no ACK
	// debt survives leave (spec §7.1.11).
	if fs := flowMust(t, r, id); fs.Active || fs.LastAck != 0 || fs.LastFlowSent != 0 {
		t.Errorf("flow after leave = %+v, want inactive and cleared", fs)
	}
}

func TestCompleteLeaveWorldNoMutationOnFailure(t *testing.T) {
	r := NewRegistry()
	id := setupInWorld(t, r, "sub", 7, 99)

	cases := map[string]func() error{
		"unknown session": func() error { return r.CompleteLeaveWorld(9999, 99) },
		"wrong character": func() error { return r.CompleteLeaveWorld(id, 100) },
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			err := call()
			if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrBadState) {
				t.Errorf("err = %v, want ErrNotFound/ErrBadState", err)
			}
		})
	}
	// Wrong state: move to AUTHENTICATED first via a second leave, then
	// retry the completed leave.
	if err := r.CompleteLeaveWorld(id, 99); err != nil {
		t.Fatalf("first leave: %v", err)
	}
	if err := r.CompleteLeaveWorld(id, 99); !errors.Is(err, ErrBadState) {
		t.Errorf("second leave err = %v, want ErrBadState", err)
	}
	// Missing binding: fresh authenticated session, never bound.
	plain := r.Create(nil)
	if err := r.Authenticate(plain, "plain", 8, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := r.CompleteLeaveWorld(plain, 1); !errors.Is(err, ErrBadState) {
		t.Errorf("unbound leave err = %v, want ErrBadState", err)
	}
	// The original session is untouched apart from its own completed leave.
	after, _ := r.Get(id)
	if after.State != StateAuthenticated || after.HasCharacter {
		t.Errorf("unexpected mutation: %+v", after)
	}
}

func TestCompleteLeaveWorldGuardsStaleIndex(t *testing.T) {
	r := NewRegistry()
	a := setupInWorld(t, r, "a", 1, 77)
	b := r.Create(nil)
	if err := r.Authenticate(b, "b", 2, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Force the index to point at b while a still believes it is bound:
	// CompleteLeaveWorld on a must fail rather than clear b's index.
	// (Reached by rebinding bookkeeping, not by public API drift.)
	r.mu.Lock()
	r.byChar[77] = b
	r.mu.Unlock()
	if err := r.CompleteLeaveWorld(a, 77); !errors.Is(err, ErrBadState) {
		t.Errorf("err = %v, want ErrBadState on index mismatch", err)
	}
	if owner, ok := r.SessionByCharacter(77); !ok || owner != b {
		t.Errorf("index disturbed: %d, %v", owner, ok)
	}
}

func setupAuthenticated(t *testing.T, r *Registry, sub string, accountID int64) ID {
	t.Helper()
	id := r.Create(nil)
	if err := r.Authenticate(id, sub, accountID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	return id
}

func snapshotMust(t *testing.T, r *Registry, id ID) Snapshot {
	t.Helper()
	snap, ok := r.Get(id)
	if !ok {
		t.Fatalf("session %d vanished", uint64(id))
	}
	return snap
}

func TestBeginEnterWorld(t *testing.T) {
	r := NewRegistry()
	id := setupAuthenticated(t, r, "sub", 7)
	exp := snapshotMust(t, r, id).TokenExp
	if _, err := r.NextServerSeq(id); err != nil {
		t.Fatal(err)
	}

	if err := r.BeginEnterWorld(id, 55); err != nil {
		t.Fatalf("begin: %v", err)
	}
	snap := snapshotMust(t, r, id)
	if snap.State != StateCharacterSelected || !snap.HasCharacter || snap.CharacterID != 55 {
		t.Errorf("after begin: %+v", snap)
	}
	if owner, ok := r.SessionByCharacter(55); !ok || owner != id {
		t.Errorf("byChar = %d,%v", owner, ok)
	}
	// Identity and server sequence preserved.
	if snap.Sub != "sub" || snap.AccountID != 7 || !snap.Authenticated || !snap.TokenExp.Equal(exp) {
		t.Errorf("identity changed: %+v", snap)
	}
	if seq, err := r.NextServerSeq(id); err != nil || seq != 2 {
		t.Errorf("seq = %d,%v want 2", seq, err)
	}

	// Wrong state (already selected): zero mutation.
	if err := r.BeginEnterWorld(id, 56); !errors.Is(err, ErrBadState) {
		t.Errorf("second begin err = %v, want ErrBadState", err)
	}
	// Already bound current session via another path: zero mutation.
	other := setupAuthenticated(t, r, "o", 7)
	if err := r.BindCharacter(other, 77); err != nil {
		t.Fatal(err)
	}
	if err := r.BeginEnterWorld(other, 78); !errors.Is(err, ErrBadState) {
		t.Errorf("bound begin err = %v, want ErrBadState", err)
	}
	// Character occupied elsewhere: ErrCharacterInUse, zero mutation.
	third := setupAuthenticated(t, r, "t", 8)
	if err := r.BeginEnterWorld(third, 55); !errors.Is(err, ErrCharacterInUse) {
		t.Errorf("occupied begin err = %v, want ErrCharacterInUse", err)
	}
	// Unknown session.
	if err := r.BeginEnterWorld(9999, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown err = %v, want ErrNotFound", err)
	}
	// Nothing moved: third still AUTHENTICATED/unbound, 55 still on id.
	if s := snapshotMust(t, r, third); s.State != StateAuthenticated || s.HasCharacter {
		t.Errorf("third mutated: %+v", s)
	}
	if owner, _ := r.SessionByCharacter(55); owner != id {
		t.Errorf("55 owner moved")
	}
	// CONNECTED session cannot begin.
	plain := r.Create(nil)
	if err := r.BeginEnterWorld(plain, 60); !errors.Is(err, ErrBadState) {
		t.Errorf("connected begin err = %v, want ErrBadState", err)
	}
	// T5b: a begun (baseline-streaming) session has no flow epoch.
	if fs := flowMust(t, r, id); fs.Active {
		t.Errorf("flow after begin = %+v, want inactive", fs)
	}
}

func TestAbortEnterWorld(t *testing.T) {
	r := NewRegistry()
	id := setupAuthenticated(t, r, "sub", 7)
	exp := snapshotMust(t, r, id).TokenExp
	if err := r.BeginEnterWorld(id, 55); err != nil {
		t.Fatal(err)
	}
	if _, err := r.NextServerSeq(id); err != nil {
		t.Fatal(err)
	}

	if err := r.AbortEnterWorld(id, 55); err != nil {
		t.Fatalf("abort: %v", err)
	}
	snap := snapshotMust(t, r, id)
	if snap.State != StateAuthenticated || snap.HasCharacter || snap.CharacterID != 0 {
		t.Errorf("after abort: %+v", snap)
	}
	if _, ok := r.SessionByCharacter(55); ok {
		t.Error("index not removed")
	}
	if snap.Sub != "sub" || snap.AccountID != 7 || !snap.TokenExp.Equal(exp) {
		t.Errorf("identity changed: %+v", snap)
	}
	if seq, err := r.NextServerSeq(id); err != nil || seq != 2 {
		t.Errorf("seq = %d,%v want 2", seq, err)
	}
	// T5b: a failed baseline leaves no partial epoch (spec §7.1.11).
	if fs := flowMust(t, r, id); fs.Active || fs.LastAck != 0 || fs.LastFlowSent != 0 {
		t.Errorf("flow after abort = %+v, want inactive and cleared", fs)
	}

	// Wrong state (already AUTHENTICATED): no mutation.
	if err := r.AbortEnterWorld(id, 55); !errors.Is(err, ErrBadState) {
		t.Errorf("second abort err = %v, want ErrBadState", err)
	}
	// Wrong character.
	if err := r.BeginEnterWorld(id, 55); err != nil {
		t.Fatal(err)
	}
	if err := r.AbortEnterWorld(id, 56); !errors.Is(err, ErrBadState) {
		t.Errorf("wrong char err = %v, want ErrBadState", err)
	}
	if s := snapshotMust(t, r, id); s.State != StateCharacterSelected || !s.HasCharacter {
		t.Errorf("mutated by failed abort: %+v", s)
	}
	// Missing binding (IN_WORLD session, different primitive path).
	inWorld := setupInWorld(t, r, "w", 7, 66)
	if err := r.AbortEnterWorld(inWorld, 66); !errors.Is(err, ErrBadState) {
		t.Errorf("in-world abort err = %v, want ErrBadState", err)
	}
	// Stale/mismatched index.
	r.mu.Lock()
	r.byChar[55] = inWorld
	r.mu.Unlock()
	if err := r.AbortEnterWorld(id, 55); !errors.Is(err, ErrBadState) {
		t.Errorf("stale index err = %v, want ErrBadState", err)
	}
	if owner, _ := r.SessionByCharacter(55); owner != inWorld {
		t.Error("stale index disturbed")
	}
	// Unknown session.
	if err := r.AbortEnterWorld(9999, 55); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown err = %v, want ErrNotFound", err)
	}
}

func TestCompleteEnterWorld(t *testing.T) {
	r := NewRegistry()
	id := setupAuthenticated(t, r, "sub", 7)
	exp := snapshotMust(t, r, id).TokenExp
	if err := r.BeginEnterWorld(id, 55); err != nil {
		t.Fatal(err)
	}
	if _, err := r.NextServerSeq(id); err != nil {
		t.Fatal(err)
	}

	if err := r.CompleteEnterWorld(id, 55); err != nil {
		t.Fatalf("complete: %v", err)
	}
	snap := snapshotMust(t, r, id)
	if snap.State != StateInWorld {
		t.Errorf("state = %s, want IN_WORLD", snap.State)
	}
	if !snap.HasCharacter || snap.CharacterID != 55 {
		t.Errorf("binding lost: %+v", snap)
	}
	if owner, ok := r.SessionByCharacter(55); !ok || owner != id {
		t.Errorf("index = %d,%v", owner, ok)
	}
	if snap.Sub != "sub" || snap.AccountID != 7 || !snap.TokenExp.Equal(exp) {
		t.Errorf("identity changed: %+v", snap)
	}
	if seq, err := r.NextServerSeq(id); err != nil || seq != 2 {
		t.Errorf("seq = %d,%v want 2", seq, err)
	}
	// T5b: the completion initialized the flow epoch atomically at the
	// current (already-written) server seq — one seq was allocated
	// before completing, so the baseline is exactly 1, never 2, and no
	// extra sequence was consumed here.
	if fs := flowMust(t, r, id); !fs.Active || fs.LastAck != 1 || fs.LastFlowSent != 1 {
		t.Errorf("flow after complete = %+v, want active at seq 1/1", fs)
	}

	// Wrong state (already IN_WORLD): no mutation.
	if err := r.CompleteEnterWorld(id, 55); !errors.Is(err, ErrBadState) {
		t.Errorf("second complete err = %v, want ErrBadState", err)
	}
	// Wrong character.
	auth2 := setupAuthenticated(t, r, "a2", 7)
	if err := r.BeginEnterWorld(auth2, 70); err != nil {
		t.Fatal(err)
	}
	if err := r.CompleteEnterWorld(auth2, 71); !errors.Is(err, ErrBadState) {
		t.Errorf("wrong char err = %v, want ErrBadState", err)
	}
	if s := snapshotMust(t, r, auth2); s.State != StateCharacterSelected {
		t.Errorf("mutated: %+v", s)
	}
	// Missing index.
	r.mu.Lock()
	delete(r.byChar, 70)
	r.mu.Unlock()
	if err := r.CompleteEnterWorld(auth2, 70); !errors.Is(err, ErrBadState) {
		t.Errorf("missing index err = %v, want ErrBadState", err)
	}
	// Unknown session.
	if err := r.CompleteEnterWorld(9999, 70); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown err = %v, want ErrNotFound", err)
	}
}

func TestWorldSessionForAccount(t *testing.T) {
	r := NewRegistry()
	me := setupAuthenticated(t, r, "me", 7)

	// None: only self (excluded) plus an AUTHENTICATED peer.
	peer := setupAuthenticated(t, r, "peer", 7)
	if _, ok, err := r.WorldSessionForAccount(7, me); err != nil || ok {
		t.Errorf("empty = %v,%v want false,nil", ok, err)
	}

	// One CHARACTER_SELECTED other session.
	sel := setupAuthenticated(t, r, "sel", 7)
	if err := r.BeginEnterWorld(sel, 80); err != nil {
		t.Fatal(err)
	}
	got, ok, err := r.WorldSessionForAccount(7, me)
	if err != nil || !ok {
		t.Fatalf("selected = %v,%v", ok, err)
	}
	if got.ID != sel || got.State != StateCharacterSelected || got.CharacterID != 80 {
		t.Errorf("got = %+v", got)
	}
	// Excluding the candidate itself yields none.
	if _, ok, err := r.WorldSessionForAccount(7, sel); err != nil || ok {
		t.Errorf("exclude-candidate = %v,%v", ok, err)
	}

	// One IN_WORLD other session instead.
	if err := r.CompleteEnterWorld(sel, 80); err != nil {
		t.Fatal(err)
	}
	got, ok, err = r.WorldSessionForAccount(7, me)
	if err != nil || !ok || got.ID != sel || got.State != StateInWorld {
		t.Errorf("in-world = %+v,%v,%v", got, ok, err)
	}

	// Other accounts and CONNECTED sessions never count.
	_ = peer
	otherAcct := setupInWorld(t, r, "other", 8, 81)
	_ = otherAcct
	if got, ok, err := r.WorldSessionForAccount(8, me); err != nil || !ok || got.AccountID != 8 {
		t.Errorf("other account = %+v,%v,%v", got, ok, err)
	}

	// Two world-active same-account sessions: invariant error, and the
	// outcome cannot depend on map order (run the query repeatedly).
	second := setupAuthenticated(t, r, "second", 7)
	if err := r.BeginEnterWorld(second, 82); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 25; i++ {
		if _, _, err := r.WorldSessionForAccount(7, me); !errors.Is(err, ErrInvariant) {
			t.Fatalf("iter %d err = %v, want ErrInvariant", i, err)
		}
	}
}

// ---------------------------------------------------------------------------
// T5b ACK-flow epoch (spec §7.1.11)
// ---------------------------------------------------------------------------

// flowMust reads the flow snapshot or fails the test.
func flowMust(t *testing.T, r *Registry, id ID) FlowSnapshot {
	t.Helper()
	fs, err := r.FlowState(id)
	if err != nil {
		t.Fatalf("FlowState(%d): %v", uint64(id), err)
	}
	return fs
}

// setupFlowSession drives one session through the REAL lifecycle
// primitives into IN_WORLD with an initialized flow epoch (the only
// legal production path).
func setupFlowSession(t *testing.T, r *Registry) ID {
	t.Helper()
	id := setupAuthenticated(t, r, "flow", 7)
	if err := r.BeginEnterWorld(id, 55); err != nil {
		t.Fatalf("begin enter: %v", err)
	}
	if err := r.CompleteEnterWorld(id, 55); err != nil {
		t.Fatalf("complete enter: %v", err)
	}
	return id
}

// forceFlow installs an exact flow epoch and server sequence value for
// wrap/edge tests. In-package reach into registry internals mirrors the
// existing byChar manipulation precedent.
func forceFlow(r *Registry, id ID, serverSeq, lastAck, lastFlowSent uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.byID[id]
	e.serverSeq.Store(serverSeq)
	e.lastAck = lastAck
	e.lastFlowSent = lastFlowSent
	e.flowActive = true
}

func flowLag(fs FlowSnapshot) int { return int(uint32(fs.LastFlowSent - fs.LastAck)) }

// TestFlowEpochLifecycle proves the epoch's full lifecycle coupling
// (spec §7.1.11): inactive through authenticate/begin, atomically
// initialized at the written 219 sequence on complete, atomically
// cleared on leave, and a fresh baseline with no old debt on re-enter.
func TestFlowEpochLifecycle(t *testing.T) {
	r := NewRegistry()
	id := r.Create(nil)

	// CONNECTED/AUTHENTICATED: no epoch.
	if fs := flowMust(t, r, id); fs.Active {
		t.Fatalf("flow after create = %+v, want inactive", fs)
	}
	if err := r.Authenticate(id, "sub", 7, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if fs := flowMust(t, r, id); fs.Active {
		t.Fatalf("flow after authenticate = %+v, want inactive", fs)
	}
	if err := r.BeginEnterWorld(id, 55); err != nil {
		t.Fatal(err)
	}
	if fs := flowMust(t, r, id); fs.Active {
		t.Fatalf("flow after begin = %+v, want inactive (baseline ungated)", fs)
	}
	// Baseline traffic: three untracked allocations while selected.
	for i := 0; i < 3; i++ {
		seq, tracked, lag, err := r.NextFlowControlledServerSeq(id, 4)
		if err != nil || tracked || lag != 0 || seq != uint32(i+1) {
			t.Fatalf("baseline alloc %d = %d,%v,%d,%v", i, seq, tracked, lag, err)
		}
	}
	if fs := flowMust(t, r, id); fs.Active || fs.LastFlowSent != 0 {
		t.Fatalf("baseline consumed flow: %+v", fs)
	}
	if err := r.CompleteEnterWorld(id, 55); err != nil {
		t.Fatal(err)
	}
	// Epoch initialized at the CURRENT server seq (3), never +1.
	if fs := flowMust(t, r, id); !fs.Active || fs.LastAck != 3 || fs.LastFlowSent != 3 || flowLag(fs) != 0 {
		t.Fatalf("flow after complete = %+v, want active 3/3 lag 0", fs)
	}
	// Two unacked IN_WORLD frames leave debt 2.
	for i, wantLag := range []int{1, 2} {
		seq, tracked, lag, err := r.NextFlowControlledServerSeq(id, 8)
		if err != nil || !tracked || lag != wantLag || seq != uint32(4+i) {
			t.Fatalf("world alloc %d = %d,%v,%d,%v", i, seq, tracked, lag, err)
		}
	}
	// Leave clears everything atomically.
	if err := r.CompleteLeaveWorld(id, 55); err != nil {
		t.Fatal(err)
	}
	if fs := flowMust(t, r, id); fs.Active || fs.LastAck != 0 || fs.LastFlowSent != 0 {
		t.Fatalf("flow after leave = %+v, want inactive and cleared", fs)
	}
	// Re-enter starts a completely new epoch at its new baseline with no
	// old debt.
	if _, err := r.NextServerSeq(id); err != nil { // seq 6
		t.Fatal(err)
	}
	if err := r.BeginEnterWorld(id, 55); err != nil {
		t.Fatal(err)
	}
	if err := r.CompleteEnterWorld(id, 55); err != nil {
		t.Fatal(err)
	}
	if fs := flowMust(t, r, id); !fs.Active || fs.LastAck != 6 || fs.LastFlowSent != 6 || flowLag(fs) != 0 {
		t.Fatalf("flow after re-enter = %+v, want fresh epoch 6/6 lag 0", fs)
	}
}

// TestCompleteEnterWorldCapturesWrittenSeq pins the exact captured
// world_ready sequence: the epoch baseline is the serverSeq AT complete
// time (the already-written 219), never worldReadySeq+1 and never a
// fresh allocation.
func TestCompleteEnterWorldCapturesWrittenSeq(t *testing.T) {
	r := NewRegistry()
	id := setupAuthenticated(t, r, "sub", 7)
	for i := 0; i < 5; i++ {
		if _, err := r.NextServerSeq(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.BeginEnterWorld(id, 55); err != nil {
		t.Fatal(err)
	}
	if err := r.CompleteEnterWorld(id, 55); err != nil {
		t.Fatal(err)
	}
	fs := flowMust(t, r, id)
	if fs.LastAck != 5 || fs.LastFlowSent != 5 {
		t.Fatalf("epoch baseline = %d/%d, want 5/5 (current seq, not +1)", fs.LastAck, fs.LastFlowSent)
	}
	// No sequence was consumed by the completion itself.
	if seq, err := r.NextServerSeq(id); err != nil || seq != 6 {
		t.Fatalf("next seq = %d,%v want 6 (complete allocated nothing)", seq, err)
	}
}

// TestBeginEnterWorldStaleFlowInvariant proves a stale active epoch at
// begin time is an internal invariant failure with zero mutation
// (spec §7.1.11): old ACK debt never rides into a new baseline.
func TestBeginEnterWorldStaleFlowInvariant(t *testing.T) {
	r := NewRegistry()
	id := setupAuthenticated(t, r, "sub", 7)
	// Manufacture the impossible residue directly: AUTHENTICATED,
	// unbound, but a still-active epoch.
	r.mu.Lock()
	e := r.byID[id]
	e.flowActive = true
	e.lastAck = 9
	e.lastFlowSent = 40
	r.mu.Unlock()
	err := r.BeginEnterWorld(id, 55)
	if !errors.Is(err, ErrInvariant) {
		t.Fatalf("begin with stale epoch err = %v, want ErrInvariant", err)
	}
	// Zero lifecycle mutation.
	snap := snapshotMust(t, r, id)
	if snap.State != StateAuthenticated || snap.HasCharacter {
		t.Fatalf("mutated by failed begin: %+v", snap)
	}
	if _, ok := r.SessionByCharacter(55); ok {
		t.Fatal("index created by failed begin")
	}
}

// TestFlowWindowThresholdAndNoSeqOnReject proves the frozen max-unacked
// semantics (spec §7.1.11, max=2 example): A→lag1, B→lag2, C rejected
// with ErrAckLag BEFORE any sequence exists, an ACK for B reopens
// capacity, and the next allocation succeeds.
func TestFlowWindowThresholdAndNoSeqOnReject(t *testing.T) {
	r := NewRegistry()
	id := setupFlowSession(t, r)
	base := flowMust(t, r, id).LastFlowSent // 0
	if lag := flowLag(flowMust(t, r, id)); lag != 0 {
		t.Fatalf("baseline lag = %d, want 0", lag)
	}
	// A: lag 1.
	seqA, tracked, lag, err := r.NextFlowControlledServerSeq(id, 2)
	if err != nil || !tracked || lag != 1 || seqA != base+1 {
		t.Fatalf("A = %d,%v,%d,%v", seqA, tracked, lag, err)
	}
	// B: lag 2 (still legal — lag <= max).
	seqB, _, lag, err := r.NextFlowControlledServerSeq(id, 2)
	if err != nil || lag != 2 || seqB != base+2 {
		t.Fatalf("B = %d,%d,%v", seqB, lag, err)
	}
	_ = seqB
	// C: rejected, no seq allocated.
	_, _, _, err = r.NextFlowControlledServerSeq(id, 2)
	if !errors.Is(err, ErrAckLag) {
		t.Fatalf("C err = %v, want ErrAckLag", err)
	}
	// Mandatory no-seq proof: the very next sequence allocation is B+1,
	// not B+2 — C consumed nothing.
	next, err := r.NextServerSeq(id)
	if err != nil || next != base+3 {
		t.Fatalf("next seq after rejection = %d,%v want %d (C consumed a sequence)", next, err, base+3)
	}
	// ACK for B reopens the window to exactly one unit.
	disp, lag, err := r.ApplyAck(id, seqB)
	if disp != AckAdvanced || lag != 0 || err != nil {
		t.Fatalf("ack B = %v,%d,%v", disp, lag, err)
	}
	// The probe above consumed base+3, so D owns base+4 with lag 1.
	seqD, tracked, lag, err := r.NextFlowControlledServerSeq(id, 2)
	if err != nil || !tracked || lag != 1 || seqD != base+4 {
		t.Fatalf("D = %d,%v,%d,%v", seqD, tracked, lag, err)
	}
	_ = seqA
}

// TestFlowAllocationUntrackedOutsideInWorld proves CONNECTED,
// AUTHENTICATED, and CHARACTER_SELECTED sessions allocate normal
// sequences without ACK accounting (spec §7.1.11): the baseline is
// never ACK-gated.
func TestFlowAllocationUntrackedOutsideInWorld(t *testing.T) {
	r := NewRegistry()
	id := r.Create(nil)
	// CONNECTED.
	seq, tracked, lag, err := r.NextFlowControlledServerSeq(id, 1)
	if err != nil || tracked || lag != 0 || seq != 1 {
		t.Fatalf("connected = %d,%v,%d,%v", seq, tracked, lag, err)
	}
	if err := r.Authenticate(id, "sub", 7, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// AUTHENTICATED.
	seq, tracked, lag, err = r.NextFlowControlledServerSeq(id, 1)
	if err != nil || tracked || lag != 0 || seq != 2 {
		t.Fatalf("authenticated = %d,%v,%d,%v", seq, tracked, lag, err)
	}
	// CHARACTER_SELECTED (mid-baseline).
	if err := r.BeginEnterWorld(id, 55); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		seq, tracked, lag, err = r.NextFlowControlledServerSeq(id, 1)
		if err != nil || tracked || lag != 0 || seq != uint32(3+i) {
			t.Fatalf("selected alloc %d = %d,%v,%d,%v", i, seq, tracked, lag, err)
		}
	}
	if fs := flowMust(t, r, id); fs.Active || fs.LastFlowSent != 0 {
		t.Fatalf("baseline moved flow: %+v", fs)
	}
	// Unknown session.
	if _, _, _, err := r.NextFlowControlledServerSeq(9999, 8); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown err = %v, want ErrNotFound", err)
	}
}

// TestFlowInvariantInWorldWithoutEpoch proves an IN_WORLD session with
// no initialized epoch (only constructible outside the real lifecycle)
// is an internal invariant failure at allocation time — never a silent
// baseline synthesized from the first post-world frame (spec §7.1.11).
func TestFlowInvariantInWorldWithoutEpoch(t *testing.T) {
	r := NewRegistry()
	id := setupInWorld(t, r, "cas", 7, 55) // CAS path: no epoch
	if fs := flowMust(t, r, id); fs.Active {
		t.Fatalf("cas in-world flow = %+v, want inactive", fs)
	}
	if _, _, _, err := r.NextFlowControlledServerSeq(id, 8); !errors.Is(err, ErrInvariant) {
		t.Fatalf("alloc err = %v, want ErrInvariant", err)
	}
	// ApplyAck hits the same invariant.
	if _, _, err := r.ApplyAck(id, 1); !errors.Is(err, ErrInvariant) {
		t.Fatalf("ack err = %v, want ErrInvariant", err)
	}
}

// TestNextFlowControlledMaxValidation proves an out-of-range window is
// an internal configuration error rather than a silently disabled
// window (spec §7.1.1/§7.1.11).
func TestNextFlowControlledMaxValidation(t *testing.T) {
	r := NewRegistry()
	id := setupAuthenticated(t, r, "sub", 7)
	for _, bad := range []int{0, -1, MaxUnackedWindow + 1} {
		if _, _, _, err := r.NextFlowControlledServerSeq(id, bad); !errors.Is(err, ErrInvariant) {
			t.Fatalf("max %d err = %v, want ErrInvariant", bad, err)
		}
	}
	for _, ok := range []int{MinUnackedWindow, 64, MaxUnackedWindow} {
		if _, _, _, err := r.NextFlowControlledServerSeq(id, ok); err != nil {
			t.Fatalf("max %d err = %v, want nil", ok, err)
		}
	}
}

// TestApplyAckClassifications covers the exact frozen classifications
// (spec §7.1.11): duplicate, stale, valid cumulative advance, and
// future/invalid — with lag values and no mutation on failure.
func TestApplyAckClassifications(t *testing.T) {
	r := NewRegistry()
	id := setupFlowSession(t, r)
	// Four unacked frames: seqs 1..4.
	for i := 0; i < 4; i++ {
		seq, tracked, lag, err := r.NextFlowControlledServerSeq(id, 64)
		if err != nil || !tracked || lag != i+1 {
			t.Fatalf("alloc %d = %d,%v,%d,%v", i, seq, tracked, lag, err)
		}
	}
	// PARTIAL cumulative advance: ACK the second frame — exactly frames
	// 3 and 4 remain as lag 2.
	disp, lag, err := r.ApplyAck(id, 2)
	if disp != AckAdvanced || lag != 2 || err != nil {
		t.Fatalf("partial advance = %v,%d,%v", disp, lag, err)
	}
	if fs := flowMust(t, r, id); fs.LastAck != 2 || fs.LastFlowSent != 4 {
		t.Fatalf("after partial = %+v, want ack 2 sent 4", fs)
	}
	// Duplicate of the current lastAck: no-op.
	if disp, lag, err = r.ApplyAck(id, 2); disp != AckDuplicate || lag != 2 || err != nil {
		t.Fatalf("duplicate = %v,%d,%v", disp, lag, err)
	}
	// Stale: serially before lastAck.
	if disp, lag, err = r.ApplyAck(id, 1); disp != AckStale || lag != 2 || err != nil {
		t.Fatalf("stale = %v,%d,%v", disp, lag, err)
	}
	// Neither stale nor duplicate moved the window.
	if fs := flowMust(t, r, id); fs.LastAck != 2 || flowLag(fs) != 2 {
		t.Fatalf("no-op moved flow: %+v", fs)
	}
	// Future: ahead of lastFlowSent; zero mutation.
	before := flowMust(t, r, id)
	if _, _, err = r.ApplyAck(id, 5); !errors.Is(err, ErrFutureAck) {
		t.Fatalf("future err = %v, want ErrFutureAck", err)
	}
	if after := flowMust(t, r, id); after != before {
		t.Fatalf("future mutated flow: %+v -> %+v", before, after)
	}
	// Full advance to the exact newest is legal (<= lastFlowSent).
	if disp, lag, err = r.ApplyAck(id, 4); disp != AckAdvanced || lag != 0 || err != nil {
		t.Fatalf("advance to newest = %v,%d,%v", disp, lag, err)
	}
	// Later ACK for the same point stays duplicate.
	if disp, lag, err = r.ApplyAck(id, 4); disp != AckDuplicate || lag != 0 || err != nil {
		t.Fatalf("late duplicate = %v,%d,%v", disp, lag, err)
	}
}

// TestApplyAckHalfRange proves an ACK at exactly the 2^31 distance is
// future/invalid in both directions — never accepted as stale
// (spec §7.1.11): both serial helpers are false there.
func TestApplyAckHalfRange(t *testing.T) {
	r := NewRegistry()
	id := setupFlowSession(t, r)
	for _, tc := range []struct {
		lastAck, lastFlowSent, ack uint32
	}{
		{0, 0, 0x80000000},
		{5, 5, 5 + 0x80000000},
		{0x80000000, 0x80000000, 0},
	} {
		forceFlow(r, id, tc.lastFlowSent, tc.lastAck, tc.lastFlowSent)
		before := flowMust(t, r, id)
		if _, _, err := r.ApplyAck(id, tc.ack); !errors.Is(err, ErrFutureAck) {
			t.Fatalf("ack %d at half-range from %d: err = %v, want ErrFutureAck",
				tc.ack, tc.lastAck, err)
		}
		if after := flowMust(t, r, id); after != before {
			t.Fatalf("half-range ack mutated flow: %+v -> %+v", before, after)
		}
	}
}

// TestFlowWraparound covers MaxUint32-1, MaxUint32, 0, and 1 (spec
// §6/§7.1.11): allocation wraps naturally, 0 is legitimately after
// MaxUint32, cumulative ACKs advance across the wrap, stale/future
// classifications hold, and lag stays a pure modulo distance.
func TestFlowWraparound(t *testing.T) {
	r := NewRegistry()
	id := setupFlowSession(t, r)
	// Epoch as if the 219 was written at MaxUint32-1.
	forceFlow(r, id, math.MaxUint32-1, math.MaxUint32-1, math.MaxUint32-1)
	if lag := flowLag(flowMust(t, r, id)); lag != 0 {
		t.Fatalf("wrap baseline lag = %d, want 0", lag)
	}
	// A: MaxUint32 (lag 1); B: 0 after the wrap (lag 2).
	seqA, _, lag, err := r.NextFlowControlledServerSeq(id, 4)
	if err != nil || seqA != math.MaxUint32 || lag != 1 {
		t.Fatalf("A = %d,%d,%v", seqA, lag, err)
	}
	seqB, _, lag, err := r.NextFlowControlledServerSeq(id, 4)
	if err != nil || seqB != 0 || lag != 2 {
		t.Fatalf("B = %d,%d,%v (wrap failed)", seqB, lag, err)
	}
	// Lag is the modulo distance across the wrap.
	if lag := flowLag(flowMust(t, r, id)); lag != 2 {
		t.Fatalf("wrap lag = %d, want 2", lag)
	}
	// Max-unacked still enforced across the wrap: lag 2 with max 2.
	if _, _, _, err := r.NextFlowControlledServerSeq(id, 2); !errors.Is(err, ErrAckLag) {
		t.Fatalf("wrap rejection err = %v, want ErrAckLag", err)
	}
	// Valid cumulative ACK across the wrap: 0 is after MaxUint32-1.
	if disp, lag, err := r.ApplyAck(id, 0); disp != AckAdvanced || lag != 0 || err != nil {
		t.Fatalf("wrap advance = %v,%d,%v", disp, lag, err)
	}
	// Stale MaxUint32 after lastAck=0.
	if disp, lag, err := r.ApplyAck(id, math.MaxUint32); disp != AckStale || lag != 0 || err != nil {
		t.Fatalf("stale MaxUint32 = %v,%d,%v", disp, lag, err)
	}
	// One more frame (seq 1), then future ACK across the wrap.
	if _, _, _, err := r.NextFlowControlledServerSeq(id, 4); err != nil {
		t.Fatal(err)
	}
	if fs := flowMust(t, r, id); fs.LastFlowSent != 1 || flowLag(fs) != 1 {
		t.Fatalf("post-wrap flow = %+v, want sent 1 lag 1", fs)
	}
	if _, _, err := r.ApplyAck(id, 2); !errors.Is(err, ErrFutureAck) {
		t.Fatalf("future across wrap err = %v, want ErrFutureAck", err)
	}
	// Duplicate at the wrapped point.
	if disp, _, err := r.ApplyAck(id, 0); disp != AckDuplicate || err != nil {
		t.Fatalf("wrap duplicate = %v,%v", disp, err)
	}
	// Advance to the wrapped newest.
	if disp, lag, err := r.ApplyAck(id, 1); disp != AckAdvanced || lag != 0 || err != nil {
		t.Fatalf("wrap advance to 1 = %v,%d,%v", disp, lag, err)
	}
}

// TestApplyAckStateErrors covers the state preconditions: unknown
// session, wrong state, and missing epoch.
func TestApplyAckStateErrors(t *testing.T) {
	r := NewRegistry()
	if _, _, err := r.ApplyAck(9999, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown err = %v, want ErrNotFound", err)
	}
	authed := setupAuthenticated(t, r, "authed", 7)
	if _, _, err := r.ApplyAck(authed, 1); !errors.Is(err, ErrBadState) {
		t.Fatalf("authenticated err = %v, want ErrBadState", err)
	}
	if err := r.BeginEnterWorld(authed, 55); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.ApplyAck(authed, 1); !errors.Is(err, ErrBadState) {
		t.Fatalf("selected err = %v, want ErrBadState", err)
	}
	if _, err := r.FlowState(9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("flow state unknown err = %v, want ErrNotFound", err)
	}
}

// TestFlowConcurrentAllocAck races sequence allocation against ACK
// application and inspection (the -race proof that flow state is
// guarded by the registry lock): the window never goes inconsistent
// and a final cumulative ACK always reaches lag 0.
func TestFlowConcurrentAllocAck(t *testing.T) {
	r := NewRegistry()
	id := setupFlowSession(t, r)
	const workers, per = 8, 50
	var maxSeq atomic.Uint32
	var wg sync.WaitGroup
	errs := make(chan error, workers+1)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				seq, tracked, lag, err := r.NextFlowControlledServerSeq(id, workers*per)
				if err != nil {
					errs <- err
					return
				}
				if !tracked || lag < 1 || lag > workers*per {
					errs <- fmt.Errorf("alloc = %d,%v,%d", seq, tracked, lag)
					return
				}
				for {
					old := maxSeq.Load()
					if seq <= old || maxSeq.CompareAndSwap(old, seq) {
						break
					}
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < workers*per/10; i++ {
			disp, _, err := r.ApplyAck(id, maxSeq.Load())
			if err != nil && !errors.Is(err, ErrFutureAck) {
				errs <- err
				return
			}
			_ = disp
			if _, err := r.FlowState(id); err != nil {
				errs <- err
				return
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	disp, lag, err := r.ApplyAck(id, maxSeq.Load())
	if disp != AckAdvanced && disp != AckDuplicate {
		t.Fatalf("final disp = %v", disp)
	}
	if err != nil || lag != 0 {
		t.Fatalf("final = %v,%d,%v want lag 0", disp, lag, err)
	}
}
