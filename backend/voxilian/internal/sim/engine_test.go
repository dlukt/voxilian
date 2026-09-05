package sim

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dlukt/voxilian/internal/world"
)

// testRNG is a deterministic stdlib math/rand/v2 source behind the
// narrow RNG seam. Each test names its seed explicitly.
type testRNG struct {
	r *rand.Rand
}

func newTestRNG(seed uint64) *testRNG {
	return &testRNG{r: rand.New(rand.NewPCG(seed, seed^0x9E3779B97F4A7C15))}
}

func (t *testRNG) Uint64() uint64 { return t.r.Uint64() }

// manualTicker delivers test-controlled pulses without wall time.
type manualTicker struct {
	ch    chan time.Time
	stops int
	mu    sync.Mutex
}

func newManualTicker() *manualTicker {
	return &manualTicker{ch: make(chan time.Time, 1024)}
}

func (m *manualTicker) C() <-chan time.Time { return m.ch }

func (m *manualTicker) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stops++
}

func (m *manualTicker) stopCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stops
}

func (m *manualTicker) pulse(t time.Time) { m.ch <- t }

// manualClock records requested periods and hands out manual tickers.
type manualClock struct {
	mu      sync.Mutex
	tickers []*manualTicker
	durs    []time.Duration
}

func newManualClock() *manualClock { return &manualClock{} }

func (c *manualClock) NewTicker(d time.Duration) Ticker {
	c.mu.Lock()
	defer c.mu.Unlock()
	mt := newManualTicker()
	c.tickers = append(c.tickers, mt)
	c.durs = append(c.durs, d)
	return mt
}

func (c *manualClock) tickerCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.tickers)
}

func (c *manualClock) firstTicker() *manualTicker {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.tickers) == 0 {
		return nil
	}
	return c.tickers[0]
}

func (c *manualClock) periods() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.durs...)
}

func mustEngine(t *testing.T, hz int, deps EngineDeps) *Engine {
	t.Helper()
	if deps.Collision == nil {
		deps.Collision = openCollision{}
	}
	if deps.RunGate == nil {
		deps.RunGate = staticGate{allow: true}
	}
	e, err := NewEngine(EngineConfig{TickHz: hz}, deps)
	if err != nil {
		t.Fatalf("NewEngine(%d) error: %v", hz, err)
	}
	return e
}

// waitForTick spins without sleeping until the engine reaches want.
func waitForTick(t *testing.T, e *Engine, want uint32) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for e.CurrentTick() != want {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for tick %d (at %d)", want, e.CurrentTick())
		}
		runtime.Gosched()
	}
}

// waitTickerCount spins without sleeping until the clock owns want tickers.
func waitTickerCount(t *testing.T, c *manualClock, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for c.tickerCount() != want {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %d tickers (have %d)", want, c.tickerCount())
		}
		runtime.Gosched()
	}
}

func TestEngineConfigValidation(t *testing.T) {
	clk := newManualClock()
	rng := newTestRNG(1)
	for _, hz := range []int{0, -5, 121, 1000} {
		if _, err := NewEngine(EngineConfig{TickHz: hz}, EngineDeps{Clock: clk, RNG: rng}); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("NewEngine(%d) = %v, want ErrInvalidConfig", hz, err)
		}
	}
	if _, err := NewEngine(EngineConfig{TickHz: 20}, EngineDeps{Clock: nil, RNG: rng}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil clock = %v, want ErrInvalidConfig", err)
	}
	if _, err := NewEngine(EngineConfig{TickHz: 20}, EngineDeps{Clock: clk, RNG: nil}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil rng = %v, want ErrInvalidConfig", err)
	}
	if _, err := NewEngine(EngineConfig{TickHz: 20}, EngineDeps{Clock: clk, RNG: rng}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil collision = %v, want ErrInvalidConfig", err)
	}
	if _, err := NewEngine(EngineConfig{TickHz: 20}, EngineDeps{Clock: clk, RNG: rng, Collision: openCollision{}}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil run gate = %v, want ErrInvalidConfig", err)
	}
	for _, hz := range []int{1, 20, 120} {
		if _, err := NewEngine(EngineConfig{TickHz: hz}, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1), Collision: openCollision{}, RunGate: staticGate{allow: true}}); err != nil {
			t.Fatalf("NewEngine(%d) error: %v", hz, err)
		}
	}
}

func TestEngineTickDurationAndDT(t *testing.T) {
	e20 := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	if e20.TickDuration() != 50*time.Millisecond {
		t.Fatalf("20 Hz period = %v, want 50ms", e20.TickDuration())
	}
	if e20.DTSeconds() != 1.0/20.0 {
		t.Fatalf("20 Hz dt = %v, want %v", e20.DTSeconds(), 1.0/20.0)
	}
	e10 := mustEngine(t, 10, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	if e10.TickDuration() != 100*time.Millisecond {
		t.Fatalf("10 Hz period = %v, want 100ms", e10.TickDuration())
	}
	if e10.DTSeconds() != 1.0/10.0 {
		t.Fatalf("10 Hz dt = %v, want %v", e10.DTSeconds(), 1.0/10.0)
	}
}

func TestEngineFirstTick(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	if e.CurrentTick() != 0 {
		t.Fatalf("initial tick = %d, want 0", e.CurrentTick())
	}
	e.Step()
	if e.CurrentTick() != 1 {
		t.Fatalf("after 1 step tick = %d, want 1", e.CurrentTick())
	}
	e.Step()
	if e.CurrentTick() != 2 {
		t.Fatalf("after 2 steps tick = %d, want 2", e.CurrentTick())
	}
}

func TestEngineTickWrap(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(1)})
	snap, err := e.AddEntity(world.Vec3{X: 1})
	if err != nil {
		t.Fatalf("AddEntity error: %v", err)
	}
	e.tick.Store(math.MaxUint32 - 1)
	e.Step()
	if e.CurrentTick() != math.MaxUint32 {
		t.Fatalf("tick = %d, want MaxUint32", e.CurrentTick())
	}
	e.Step()
	if e.CurrentTick() != 0 {
		t.Fatalf("tick = %d, want 0 after wrap", e.CurrentTick())
	}
	e.Step()
	if e.CurrentTick() != 1 {
		t.Fatalf("tick = %d, want 1 after wrap", e.CurrentTick())
	}
	// History keeps wrapped values in chronological ring order.
	h, err := e.History(snap.ID)
	if err != nil {
		t.Fatalf("History error: %v", err)
	}
	if len(h) != 3 {
		t.Fatalf("history len = %d, want 3", len(h))
	}
	if h[0].Tick != math.MaxUint32 || h[1].Tick != 0 || h[2].Tick != 1 {
		t.Fatalf("history ticks = %d,%d,%d, want MaxUint32,0,1", h[0].Tick, h[1].Tick, h[2].Tick)
	}
}

func TestEngineRunManualTicker(t *testing.T) {
	clk := newManualClock()
	e := mustEngine(t, 20, EngineDeps{Clock: clk, RNG: newTestRNG(7)})
	if clk.tickerCount() != 0 {
		t.Fatal("NewEngine must not start simulation (no ticker before Run)")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	waitTickerCount(t, clk, 1)
	mt := clk.firstTicker()
	if mt == nil {
		t.Fatal("no ticker created by Run")
	}
	periods := clk.periods()
	if len(periods) != 1 || periods[0] != 50*time.Millisecond {
		t.Fatalf("ticker periods = %v, want [50ms]", periods)
	}
	if e.CurrentTick() != 0 {
		t.Fatalf("before pulse tick = %d, want 0", e.CurrentTick())
	}
	now := time.Now()
	mt.pulse(now)
	waitForTick(t, e, 1)
	mt.pulse(now)
	waitForTick(t, e, 2)
	mt.pulse(now)
	waitForTick(t, e, 3)
	// Cancellation executes no extra final tick and returns.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if e.CurrentTick() != 3 {
		t.Fatalf("after cancel tick = %d, want 3 (no extra final tick)", e.CurrentTick())
	}
	// An extra pulse after exit drives nothing.
	mt.pulse(now)
	if e.CurrentTick() != 3 {
		t.Fatalf("extra pulse after exit tick = %d, want 3", e.CurrentTick())
	}
	if got := mt.stopCount(); got != 1 {
		t.Fatalf("ticker Stop called %d times, want exactly 1", got)
	}
	if n := clk.tickerCount(); n != 1 {
		t.Fatalf("Run created %d tickers, want exactly 1", n)
	}
}

func TestEngineRunNoCatchUp(t *testing.T) {
	clk := newManualClock()
	e := mustEngine(t, 20, EngineDeps{Clock: clk, RNG: newTestRNG(9)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	waitTickerCount(t, clk, 1)
	mt := clk.firstTicker()
	// One pulse far in the future is still exactly one tick: the engine
	// never derives catch-up bursts or dt from wall-clock timestamps.
	mt.pulse(time.Now().Add(1000 * time.Hour))
	waitForTick(t, e, 1)
	if e.CurrentTick() != 1 {
		t.Fatalf("tick = %d, want exactly 1 after one far-future pulse", e.CurrentTick())
	}
	if e.DTSeconds() != 1.0/20.0 {
		t.Fatalf("dt changed by wall timestamp: %v", e.DTSeconds())
	}
}

func TestEngineHistorySampling20Hz(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(3)})
	snap, err := e.AddEntity(world.Vec3{X: 10, Y: 1, Z: -5})
	if err != nil {
		t.Fatalf("AddEntity error: %v", err)
	}
	for i := 0; i < 40; i++ {
		e.Step()
	}
	h, err := e.History(snap.ID)
	if err != nil {
		t.Fatalf("History error: %v", err)
	}
	if len(h) != 40 {
		t.Fatalf("history len = %d, want 40", len(h))
	}
	for i, s := range h {
		if s.Tick != uint32(i+1) {
			t.Fatalf("sample %d tick = %d, want %d", i, s.Tick, i+1)
		}
		if s.Position != snap.Position {
			t.Fatalf("sample %d pos = %v, want repeated %+v", i, s.Position, snap.Position)
		}
	}
	for i := 0; i < 5; i++ {
		e.Step()
	}
	h, _ = e.History(snap.ID)
	if len(h) != 40 {
		t.Fatalf("history len = %d, want 40 after 45 ticks", len(h))
	}
	for i, s := range h {
		if s.Tick != uint32(i+6) {
			t.Fatalf("sample %d tick = %d, want %d (ticks 6..45)", i, s.Tick, i+6)
		}
	}
}

func TestEngineHistoryRecordsSameCellMutations(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(4)})
	snap, _ := e.AddEntity(world.Vec3{X: 10})
	base := 10.0
	for i := 1; i <= 10; i++ {
		next := world.Vec3{X: base + float64(i)*0.1}
		if err := e.SetPosition(snap.ID, next); err != nil {
			t.Fatalf("tick %d SetPosition error: %v", i, err)
		}
		e.Step()
		h, _ := e.History(snap.ID)
		latest := h[len(h)-1]
		if latest.Tick != uint32(i) {
			t.Fatalf("tick %d: latest sample tick = %d", i, latest.Tick)
		}
		if latest.Position.X != next.X {
			t.Fatalf("tick %d: sample X = %v, want authoritative %v", i, latest.Position.X, next.X)
		}
	}
}

func TestEngineHistoryHorizonConfigurable(t *testing.T) {
	e10 := mustEngine(t, 10, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(5)})
	if e10.HistoryCapacity() != 20 {
		t.Fatalf("10 Hz history capacity = %d, want 20", e10.HistoryCapacity())
	}
	e30 := mustEngine(t, 30, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(5)})
	if e30.HistoryCapacity() != 60 {
		t.Fatalf("30 Hz history capacity = %d, want 60", e30.HistoryCapacity())
	}
	snap, _ := e10.AddEntity(world.Vec3{X: 1})
	for i := 0; i < 25; i++ {
		e10.Step()
	}
	h, _ := e10.History(snap.ID)
	if len(h) != 20 {
		t.Fatalf("10 Hz history len = %d, want 20", len(h))
	}
	if h[0].Tick != 6 || h[19].Tick != 25 {
		t.Fatalf("10 Hz ticks = %d..%d, want 6..25", h[0].Tick, h[19].Tick)
	}
}

func TestEngineMultipleEntitiesSampledOnce(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(6)})
	a, _ := e.AddEntity(world.Vec3{X: 5})          // positive cell
	b, _ := e.AddEntity(world.Vec3{X: -5})         // negative cell
	c, _ := e.AddEntity(world.Vec3{X: 5.5})        // same cell as a
	d, _ := e.AddEntity(world.Vec3{X: 100, Z: 96}) // distant cell
	for _, id := range []EntityID{a.ID, b.ID, c.ID, d.ID} {
		if h, _ := e.History(id); len(h) != 0 {
			t.Fatalf("entity %d history before step = %d, want 0", id, len(h))
		}
	}
	e.Step()
	for _, id := range []EntityID{a.ID, b.ID, c.ID, d.ID} {
		h, _ := e.History(id)
		if len(h) != 1 || h[0].Tick != 1 {
			t.Fatalf("entity %d history = %+v, want exactly one sample at tick 1", id, h)
		}
	}
	e.Step()
	for _, id := range []EntityID{a.ID, b.ID, c.ID, d.ID} {
		h, _ := e.History(id)
		if len(h) != 2 || h[1].Tick != 2 {
			t.Fatalf("entity %d history = %+v, want two samples ending at tick 2", id, h)
		}
	}
}

func TestEngineDeterministicOrder(t *testing.T) {
	e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(11)})
	// 5 cells x 4 entities in deliberately non-sorted insertion order.
	cellBases := []world.Vec3{
		{X: 100}, {X: -100}, {X: 0}, {X: 33}, {X: -33, Z: 40},
	}
	order := []int{3, 0, 4, 1, 2, 3, 1, 4, 0, 2, 4, 3, 2, 0, 1, 2, 4, 0, 1, 3}
	for i, ci := range order {
		base := cellBases[ci]
		if _, err := e.AddEntity(world.Vec3{X: base.X + float64(i%4), Y: float64(i), Z: base.Z}); err != nil {
			t.Fatalf("add %d error: %v", i, err)
		}
	}
	trace := traceEngine(e, nil)
	_ = trace
	for i := 0; i < 5; i++ {
		e.Step()
	}
	// Repeated traces of the same state are byte-identical.
	a := traceEngine(e, nil)
	b := traceEngine(e, nil)
	if a != b {
		t.Fatal("repeated traces differ: ordering unstable")
	}
	// Canonical ordering holds in the trace (numeric X then Z).
	lines := strings.Split(a, "\n")
	var cells []world.CellCoord
	for _, l := range lines {
		if strings.HasPrefix(l, "cell:") {
			var x, z int32
			if _, err := fmt.Sscanf(l, "cell:%d,%d", &x, &z); err != nil {
				t.Fatalf("unparsable cell line %q: %v", l, err)
			}
			cells = append(cells, world.CellCoord{X: x, Z: z})
		}
	}
	if len(cells) == 0 {
		t.Fatal("trace has no cells")
	}
	for i := 1; i < len(cells); i++ {
		if world.CompareCellCoords(cells[i-1], cells[i]) >= 0 {
			t.Fatalf("cells not in canonical order:\n%s", a)
		}
	}
}

// traceEngine renders the deterministic observable trace: tick, ordered
// cells, ordered entity snapshots, and ordered history tails.
func traceEngine(e *Engine, rngDraws []uint64) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "tick:%d\n", e.CurrentTick())
	for _, c := range e.CellCoords() {
		fmt.Fprintf(&sb, "cell:%d,%d\n", c.X, c.Z)
		for _, s := range e.EntitiesInCell(c) {
			fmt.Fprintf(&sb, "ent:%d pos:%.3f,%.3f,%.3f cell:%d,%d\n",
				uint64(s.ID), s.Position.X, s.Position.Y, s.Position.Z, s.Cell.X, s.Cell.Z)
			h, err := e.History(s.ID)
			if err != nil {
				fmt.Fprintf(&sb, "hist-err:%v\n", err)
				continue
			}
			tail := h
			if len(tail) > 3 {
				tail = tail[len(tail)-3:]
			}
			for _, smp := range tail {
				fmt.Fprintf(&sb, "hist:%d %.3f,%.3f,%.3f\n",
					smp.Tick, smp.Position.X, smp.Position.Y, smp.Position.Z)
			}
		}
	}
	for _, v := range rngDraws {
		fmt.Fprintf(&sb, "rng:%d\n", v)
	}
	return sb.String()
}

// runScriptedTrace builds n entities across several cells, runs ticks
// with deterministic same-cell mutations, and folds every tick's full
// trace plus controlled RNG draws into one comparable string.
func runScriptedTrace(e *Engine, seed uint64, ticks int) string {
	cellBases := []float64{16, -48, 80, -112, 144, -16}
	var ids []EntityID
	var bases []world.Vec3
	for i := 0; i < 24; i++ {
		base := world.Vec3{X: cellBases[i%len(cellBases)], Y: float64(i % 3), Z: cellBases[(i*5)%len(cellBases)]}
		snap, err := e.AddEntity(base)
		if err != nil {
			panic(fmt.Sprintf("script add: %v", err))
		}
		ids = append(ids, snap.ID)
		bases = append(bases, base)
	}
	_ = seed
	var sb strings.Builder
	for k := 1; k <= ticks; k++ {
		for j, id := range ids {
			// Deterministic offsets in [-2,+2] around the cell center:
			// cell centers have >= 14 m margin, so mutations stay
			// same-cell and never trigger handoff staging.
			ox := float64((k*7+int(id)*13)%9)*0.5 - 2.0
			oz := float64((k*11+int(id)*17)%9)*0.5 - 2.0
			next := world.Vec3{X: bases[j].X + ox, Y: bases[j].Y, Z: bases[j].Z + oz}
			if err := e.SetPosition(id, next); err != nil {
				panic(fmt.Sprintf("script setpos tick %d: %v", k, err))
			}
		}
		e.Step()
		var draws []uint64
		for d := 0; d < 4; d++ {
			draws = append(draws, e.RandUint64())
		}
		sb.WriteString(traceEngine(e, draws))
	}
	return sb.String()
}

func TestEngineDeterministicTrace(t *testing.T) {
	mk := func(seed uint64) (*Engine, string) {
		e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(seed)})
		return e, runScriptedTrace(e, seed, 60)
	}
	eA, traceA := mk(12345)
	_ = eA
	eB, traceB := mk(12345)
	_ = eB
	if traceA != traceB {
		t.Fatal("same-seed traces differ: determinism violated")
	}
	if len(traceA) == 0 {
		t.Fatal("empty trace proves nothing")
	}
	// The trace must meaningfully cover ticks, cells, entities, history.
	for _, want := range []string{"tick:60\n", "hist:", "rng:"} {
		if !strings.Contains(traceA, want) {
			t.Fatalf("trace missing %q", want)
		}
	}
	if n := strings.Count(traceA, "ent:"); n < 24*60 {
		t.Fatalf("trace covers %d entity lines, want >= %d", n, 24*60)
	}
}

func TestEngineRNGSeam(t *testing.T) {
	draw := func(seed uint64, n int) []uint64 {
		e := mustEngine(t, 20, EngineDeps{Clock: newManualClock(), RNG: newTestRNG(seed)})
		out := make([]uint64, n)
		for i := range out {
			out[i] = e.RandUint64()
		}
		return out
	}
	a1 := draw(99, 16)
	a2 := draw(99, 16)
	for i := range a1 {
		if a1[i] != a2[i] {
			t.Fatalf("same seed diverged at %d: %d vs %d", i, a1[i], a2[i])
		}
	}
	b := draw(100, 16)
	same := true
	for i := range a1 {
		if a1[i] != b[i] {
			same = false
		}
	}
	if same {
		t.Fatal("different seeds produced identical RNG traces: seam not wired")
	}
}
