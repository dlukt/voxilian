package sim

import (
	"errors"
	"testing"
)

// scriptClock is the deterministic OpIDNow seam: tests advance ms
// explicitly, never wall time, never sleep.
type scriptClock struct{ ms int64 }

func (s *scriptClock) now() int64 { return s.ms }

func mustOpIDGen(t *testing.T, worker uint16, clk *scriptClock) *OpIDGenerator {
	t.Helper()
	g, err := NewOpIDGenerator(worker, clk.now)
	if err != nil {
		t.Fatalf("NewOpIDGenerator(%d) error: %v", worker, err)
	}
	return g
}

func mustNext(t *testing.T, g *OpIDGenerator) OpID {
	t.Helper()
	id, err := g.Next()
	if err != nil {
		t.Fatalf("Next() error: %v", err)
	}
	if id == OpID(0) {
		t.Fatal("Next() emitted reserved OpID 0")
	}
	return id
}

func TestOpIDGeneratorConfig(t *testing.T) {
	clk := &scriptClock{ms: OpIDEpochUnixMillis}
	if _, err := NewOpIDGenerator(0, clk.now); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("worker 0 = %v, want ErrInvalidConfig", err)
	}
	if _, err := NewOpIDGenerator(OpIDMaxWorker+1, clk.now); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("worker 1024 = %v, want ErrInvalidConfig", err)
	}
	if _, err := NewOpIDGenerator(1, nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil clock = %v, want ErrInvalidConfig", err)
	}
	if _, err := NewOpIDGenerator(1, clk.now); err != nil {
		t.Fatalf("worker 1 = %v, want success", err)
	}
	if _, err := NewOpIDGenerator(OpIDMaxWorker, clk.now); err != nil {
		t.Fatalf("worker 1023 = %v, want success", err)
	}
}

func TestOpIDGeneratorSequence(t *testing.T) {
	clk := &scriptClock{ms: OpIDEpochUnixMillis + 1000}
	g := mustOpIDGen(t, 1, clk)
	first := mustNext(t, g)
	if _, _, seq := first.Split(); seq != 0 {
		t.Fatalf("first seq = %d, want 0", seq)
	}
	second := mustNext(t, g)
	if _, _, seq := second.Split(); seq != 1 {
		t.Fatalf("second seq = %d, want 1", seq)
	}
	if second <= first {
		t.Fatalf("ids not strictly increasing: %d then %d", uint64(first), uint64(second))
	}
	prev := second
	for i := 2; i < 64; i++ {
		id := mustNext(t, g)
		if id <= prev {
			t.Fatalf("id %d not increasing at %d", uint64(id), i)
		}
		if _, w, seq := id.Split(); w != 1 || seq != uint16(i) {
			t.Fatalf("id splits to worker=%d seq=%d, want 1/%d", w, seq, i)
		}
		prev = id
	}
}

func TestOpIDGeneratorPerMillisecondBudget(t *testing.T) {
	clk := &scriptClock{ms: OpIDEpochUnixMillis + 5000}
	g := mustOpIDGen(t, 1, clk)
	seen := make(map[OpID]struct{}, 4097)
	for i := 0; i < 4096; i++ {
		id := mustNext(t, g)
		if _, _, seq := id.Split(); seq != uint16(i) {
			t.Fatalf("id %d seq = %d, want %d", i, seq, i)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id %d at %d", uint64(id), i)
		}
		seen[id] = struct{}{}
	}
	// The 4097th ID in the same millisecond exhausts the sequence.
	if _, err := g.Next(); !errors.Is(err, ErrOpIDSequenceExhausted) {
		t.Fatalf("4097th = %v, want ErrOpIDSequenceExhausted", err)
	}
	if len(seen) != 4096 {
		t.Fatalf("unique ids = %d, want 4096", len(seen))
	}
}

func TestOpIDGeneratorNextMillisecondRecovery(t *testing.T) {
	clk := &scriptClock{ms: OpIDEpochUnixMillis + 7000}
	g := mustOpIDGen(t, 7, clk)
	for i := 0; i < 4096; i++ {
		mustNext(t, g)
	}
	if _, err := g.Next(); !errors.Is(err, ErrOpIDSequenceExhausted) {
		t.Fatalf("exhaustion = %v, want ErrOpIDSequenceExhausted", err)
	}
	clk.ms++
	id := mustNext(t, g)
	delta, w, seq := id.Split()
	if seq != 0 {
		t.Fatalf("post-exhaustion seq = %d, want reset 0", seq)
	}
	if delta != 7001 || w != 7 {
		t.Fatalf("split = delta %d worker %d, want 7001/7", delta, w)
	}
}

func TestOpIDGeneratorClockRegression(t *testing.T) {
	clk := &scriptClock{ms: OpIDEpochUnixMillis + 9000}
	g := mustOpIDGen(t, 3, clk)
	a := mustNext(t, g) // seq 0 at T
	b := mustNext(t, g) // seq 1 at T
	clk.ms--
	if _, err := g.Next(); !errors.Is(err, ErrOpIDClockRegression) {
		t.Fatalf("T-1 = %v, want ErrOpIDClockRegression", err)
	}
	// Regression corrupts nothing: back at T the same-timestamp
	// sequence continues safely with no duplicate.
	clk.ms++
	c := mustNext(t, g)
	if _, _, seq := c.Split(); seq != 2 {
		t.Fatalf("resumed seq = %d, want 2", seq)
	}
	for _, id := range []OpID{a, b, c} {
		if id == OpID(0) {
			t.Fatal("emitted reserved zero")
		}
	}
	if a == b || b == c || a == c {
		t.Fatal("duplicate IDs across regression boundary")
	}
	clk.ms++
	d := mustNext(t, g)
	if _, _, seq := d.Split(); seq != 0 {
		t.Fatalf("next-ms seq = %d, want reset 0", seq)
	}
}

func TestOpIDEpochBoundaries(t *testing.T) {
	clk := &scriptClock{ms: OpIDEpochUnixMillis - 1}
	g := mustOpIDGen(t, 1, clk)
	if _, err := g.Next(); !errors.Is(err, ErrOpIDTimeBeforeEpoch) {
		t.Fatalf("epoch-1ms = %v, want ErrOpIDTimeBeforeEpoch", err)
	}
	clk.ms = OpIDEpochUnixMillis
	at := mustNext(t, g)
	if delta, w, seq := at.Split(); delta != 0 || w != 1 || seq != 0 {
		t.Fatalf("epoch split = %d/%d/%d, want 0/1/0", delta, w, seq)
	}
	clk.ms = OpIDEpochUnixMillis + 1
	after := mustNext(t, g)
	if delta, _, _ := after.Split(); delta != 1 {
		t.Fatalf("epoch+1 delta = %d, want 1", delta)
	}
	if after == OpID(0) {
		t.Fatal("emitted reserved zero at epoch boundary")
	}
}

func TestOpIDTimestampOverflow(t *testing.T) {
	maxMS := OpIDEpochUnixMillis + (1<<OpIDTimestampBits - 1)
	clk := &scriptClock{ms: maxMS}
	g := mustOpIDGen(t, 1, clk)
	id := mustNext(t, g)
	if delta, _, _ := id.Split(); delta != 1<<OpIDTimestampBits-1 {
		t.Fatalf("max delta = %d, want 2^41-1", delta)
	}
	clk.ms++
	if _, err := g.Next(); !errors.Is(err, ErrOpIDTimestampExhausted) {
		t.Fatalf("beyond 41 bits = %v, want ErrOpIDTimestampExhausted", err)
	}
	// No wrap: the generator refuses rather than truncating.
	clk.ms = maxMS
	id2 := mustNext(t, g)
	if _, _, seq := id2.Split(); seq != 1 {
		t.Fatalf("same-ms seq after refusal = %d, want 1 (no wrap)", seq)
	}
}

func TestOpIDMultipleWorkers(t *testing.T) {
	clk := &scriptClock{ms: OpIDEpochUnixMillis + 11000}
	gens := map[uint16]*OpIDGenerator{
		1:    mustOpIDGen(t, 1, clk),
		2:    mustOpIDGen(t, 2, clk),
		1023: mustOpIDGen(t, 1023, clk),
	}
	seen := make(map[OpID]uint16)
	for w, g := range gens {
		for i := 0; i < 16; i++ {
			id := mustNext(t, g)
			if prev, dup := seen[id]; dup {
				t.Fatalf("collision: id %d from workers %d and %d", uint64(id), prev, w)
			}
			seen[id] = w
			if _, got, seq := id.Split(); got != w || seq != uint16(i) {
				t.Fatalf("worker %d id splits to %d/%d", w, got, seq)
			}
		}
	}
}

func TestOpIDPackUnpackKnownValues(t *testing.T) {
	clk := &scriptClock{ms: OpIDEpochUnixMillis + 424242}
	g := mustOpIDGen(t, 9, clk)
	for i := 0; i < 4; i++ {
		mustNext(t, g)
	}
	id := mustNext(t, g) // 5th ID: seq 4
	delta, w, seq := id.Split()
	if delta != 424242 || w != 9 || seq != 4 {
		t.Fatalf("split = %d/%d/%d, want 424242/9/4", delta, w, seq)
	}
	want := OpID(424242<<OpIDTimestampShift | 9<<OpIDWorkerShift | 4)
	if id != want {
		t.Fatalf("id = %d, want packed %d", uint64(id), uint64(want))
	}
	// Masks recover each field independently of the others.
	if uint64(id)>>OpIDTimestampShift != 424242 {
		t.Fatal("timestamp mask leak")
	}
	if uint16(uint64(id)>>OpIDWorkerShift&OpIDMaxWorker) != 9 {
		t.Fatal("worker mask leak")
	}
	if uint16(uint64(id)&OpIDMaxSequence) != 4 {
		t.Fatal("sequence mask leak")
	}
}

func TestOpIDDeterministicTrace(t *testing.T) {
	script := []int64{0, 0, 0, 1, 1, 5, 5, 5}
	run := func(worker uint16) []OpID {
		clk := &scriptClock{ms: OpIDEpochUnixMillis + 100}
		g := mustOpIDGen(t, worker, clk)
		out := make([]OpID, 0, len(script))
		for _, step := range script {
			clk.ms = OpIDEpochUnixMillis + 100 + step
			out = append(out, mustNext(t, g))
		}
		return out
	}
	a, b := run(5), run(5)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("same worker/script diverged at %d: %d vs %d", i, uint64(a[i]), uint64(b[i]))
		}
	}
	c := run(6)
	diff := false
	for i := range a {
		if a[i] != c[i] {
			diff = true
		}
	}
	if !diff {
		t.Fatal("different workers produced identical traces")
	}
}
