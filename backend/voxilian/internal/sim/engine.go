package sim

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/dlukt/voxilian/internal/world"
)

// HistoryHorizonSeconds is the position-history horizon in simulated
// seconds (spec §5.2.6). Per-entity ring capacity is therefore
// 2 * tickHz samples (40 at the default 20 Hz).
const HistoryHorizonSeconds = 2

// tickHz bounds mirror config validation (1..120); the sim receives
// TickHz as a plain value and MUST NOT import the config package.
const (
	minTickHz = 1
	maxTickHz = 120
)

// RNG is the injected randomness dependency (spec §5.2.7). The seam is
// deliberately narrow: M4-T1 needs no random gameplay behavior, and
// future tasks expand internal helpers only when bounded rolls are
// actually required. Production seeding/wiring belongs to
// executable/bootstrap work, not T1.
type RNG interface {
	Uint64() uint64
}

// EngineConfig carries the fixed-step rate. The engine uses it as a
// FIXED simulation step (dtSeconds = 1.0/tickHz); wall-clock scheduling
// decides WHEN a step runs, never how much simulated time it
// represents.
type EngineConfig struct {
	TickHz int
}

// EngineDeps carries the engine's external seams. Both MUST be non-nil.
type EngineDeps struct {
	Clock Clock
	RNG   RNG
}

// Engine is the M4-T1 deterministic sim skeleton (spec §5.2): one
// in-process simulation loop owns mutation of ALL cells, so each cell
// has exactly one writer. There are no per-cell goroutines, no
// cell/entity locks, and no worker pools.
//
// Ownership: Step and Run own mutable sim execution. Do NOT call Step
// concurrently with Run; later intent queues will submit work to the
// sim owner instead of adding synchronization here.
//
// Construction via NewEngine is pure: it starts no goroutine and no
// background ticker. The owner explicitly drives simulation with Run
// (production) or Step (tests); both execute the SAME step core.
type Engine struct {
	tickHz   int
	tickDur  time.Duration
	dt       float64
	clock    Clock
	rng      RNG
	tick     atomic.Uint32
	registry *registry
}

// NewEngine validates config/deps and builds a tick-0 engine with an
// empty registry. It starts nothing.
func NewEngine(cfg EngineConfig, deps EngineDeps) (*Engine, error) {
	if cfg.TickHz < minTickHz || cfg.TickHz > maxTickHz {
		return nil, fmt.Errorf("%w: tick_hz %d out of range %d..%d",
			ErrInvalidConfig, cfg.TickHz, minTickHz, maxTickHz)
	}
	if deps.Clock == nil {
		return nil, fmt.Errorf("%w: nil clock", ErrInvalidConfig)
	}
	if deps.RNG == nil {
		return nil, fmt.Errorf("%w: nil rng", ErrInvalidConfig)
	}
	return &Engine{
		tickHz:   cfg.TickHz,
		tickDur:  time.Second / time.Duration(cfg.TickHz),
		dt:       1.0 / float64(cfg.TickHz),
		clock:    deps.Clock,
		rng:      deps.RNG,
		registry: newRegistry(HistoryHorizonSeconds * cfg.TickHz),
	}, nil
}

// CurrentTick returns the last executed tick (0 before the first
// step). It is safe for concurrent observation, e.g. later gateway
// TickFunc wiring. There is no writable tick.
func (e *Engine) CurrentTick() uint32 { return e.tick.Load() }

// TickHz returns the configured fixed-step rate.
func (e *Engine) TickHz() int { return e.tickHz }

// TickDuration returns the wall-clock ticker period for the configured
// rate (50 ms at 20 Hz). It schedules WHEN steps run; it never scales
// simulation state.
func (e *Engine) TickDuration() time.Duration { return e.tickDur }

// DTSeconds returns the fixed simulated seconds per step
// (1.0/tickHz; 0.05 at 20 Hz). Future movement/combat rates scale by
// this dt; actual elapsed wall time is never used.
func (e *Engine) DTSeconds() float64 { return e.dt }

// HistoryCapacity returns the per-entity ring capacity
// (2 * tickHz samples).
func (e *Engine) HistoryCapacity() int { return e.registry.historyCapacity }

// RandUint64 draws one value through the engine's injected RNG. T1
// gameplay uses no randomness; this seam exists so tests prove the
// dependency is genuinely wired (same seed -> same trace).
func (e *Engine) RandUint64() uint64 { return e.rng.Uint64() }

// Step executes exactly one fixed simulation step (spec §5.2.8):
//
//  1. allocate/increment tick (0 -> 1 for the first step; u32 wrap is normal)
//  2. process active cells in canonical CellCoord order
//  3. within a cell inspect/process entities in EntityID ascending order
//  4. append each live entity's post-step position-history sample
//
// T1 has no gameplay systems, so steps 2/3 perform skeleton
// bookkeeping only. Future systems insert their phase BEFORE history
// sampling; gameplay MUST NOT run after the same tick's sample.
func (e *Engine) Step() {
	tick := e.tick.Add(1)
	for _, coord := range e.registry.CellCoords() {
		for _, snap := range e.registry.EntitiesInCell(coord) {
			ent, err := e.registry.lookup(snap.ID)
			if err != nil {
				continue
			}
			ent.history.Append(PositionSample{Tick: tick, Position: ent.position})
		}
	}
}

// Run drives the fixed-step loop off the injected clock: one delivered
// ticker pulse executes exactly one Step (spec §5.2.3/§5.2.1). Pulse
// timestamps are ignored and MUST NOT affect simulation dt; no burst
// catch-up ticks are ever executed from elapsed wall time, so an
// overloaded process falls behind wall time instead of spiraling.
//
// Run creates exactly one ticker, stops it on exit, and returns when
// ctx is cancelled (returning nil). Cancellation executes no extra
// final tick and performs no persistence/shutdown work.
func (e *Engine) Run(ctx context.Context) error {
	ticker := e.clock.NewTicker(e.tickDur)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C():
			e.Step()
		}
	}
}

// AddEntity inserts a skeleton entity (see registry.AddEntity).
func (e *Engine) AddEntity(pos world.Vec3) (EntitySnapshot, error) {
	return e.registry.AddEntity(pos)
}

// RemoveEntity deletes an entity and discards its history.
func (e *Engine) RemoveEntity(id EntityID) error {
	return e.registry.RemoveEntity(id)
}

// SetPosition updates an entity's position within its cell, or returns
// ErrCellHandoffRequired with zero mutation across cells.
func (e *Engine) SetPosition(id EntityID, pos world.Vec3) error {
	return e.registry.SetPosition(id, pos)
}

// Entity returns an immutable snapshot copy.
func (e *Engine) Entity(id EntityID) (EntitySnapshot, error) {
	return e.registry.Entity(id)
}

// EntitiesInCell lists one cell's entities in EntityID ascending order.
func (e *Engine) EntitiesInCell(coord world.CellCoord) []EntitySnapshot {
	return e.registry.EntitiesInCell(coord)
}

// CellCoords lists active cells in canonical X/Z order.
func (e *Engine) CellCoords() []world.CellCoord {
	return e.registry.CellCoords()
}

// History returns an entity's retained samples, oldest -> newest.
func (e *Engine) History(id EntityID) ([]PositionSample, error) {
	return e.registry.History(id)
}

// EntityCount reports the number of live entities.
func (e *Engine) EntityCount() int { return e.registry.EntityCount() }

// CellCount reports the number of active cells.
func (e *Engine) CellCount() int { return e.registry.CellCount() }
