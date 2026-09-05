package sim

import (
	"context"
	"fmt"
	"math"
	"sync/atomic"
	"time"

	"github.com/dlukt/voxilian/internal/serial32"
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

// EngineDeps carries the engine's external seams. Clock, RNG,
// Collision, and RunGate are REQUIRED and fail fast when missing;
// Movement and Anomaly MAY be nil, which selects an internal no-op
// (output observers must never stall the sim owner).
type EngineDeps struct {
	Clock     Clock
	RNG       RNG
	Collision CollisionWorld
	RunGate   RunGate
	Movement  MovementSink
	Anomaly   MovementObserver
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
	tickHz    int
	tickDur   time.Duration
	dt        float64
	clock     Clock
	rng       RNG
	collision CollisionWorld
	runGate   RunGate
	movement  MovementSink
	anomaly   MovementObserver
	tick      atomic.Uint32
	registry  *registry
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
	if deps.Collision == nil {
		return nil, fmt.Errorf("%w: nil collision world", ErrInvalidConfig)
	}
	if deps.RunGate == nil {
		return nil, fmt.Errorf("%w: nil run gate", ErrInvalidConfig)
	}
	return &Engine{
		tickHz:    cfg.TickHz,
		tickDur:   time.Second / time.Duration(cfg.TickHz),
		dt:        1.0 / float64(cfg.TickHz),
		clock:     deps.Clock,
		rng:       deps.RNG,
		collision: deps.Collision,
		runGate:   deps.RunGate,
		movement:  deps.Movement,
		anomaly:   deps.Anomaly,
		registry:  newRegistry(HistoryHorizonSeconds * cfg.TickHz),
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

// Step executes exactly one fixed simulation step (spec
// §5.2.8/§5.3.7):
//
//  1. allocate/increment tick (0 -> 1 for the first step; u32 wrap is normal)
//  2. process active cells in canonical CellCoord order
//  3. within a cell inspect/process entities in EntityID ascending order
//  4. consume newest pending movement input into active yaw/control/anchor
//  5. integrate horizontal movement (walk/run via RunGate)
//  6. resolve collision substeps / cross-cell handoff staging
//  7. sample final volume flags
//  8. emit MovementUpdate through the sink when applicable
//  9. append each live entity's post-step position-history sample
//
// T2 adds movement phases 4-8; future systems likewise insert BEFORE
// history sampling. Gameplay MUST NOT run after the same tick's
// historical sample. Because T2 forbids actual handoff, movement
// cannot mutate cell membership during iteration.
func (e *Engine) Step() {
	tick := e.tick.Add(1)
	for _, coord := range e.registry.CellCoords() {
		for _, snap := range e.registry.EntitiesInCell(coord) {
			ent, err := e.registry.lookup(snap.ID)
			if err != nil {
				continue
			}
			if update, emit := e.stepEntity(ent, tick); emit && e.movement != nil {
				e.movement.OnMovement(update)
			}
			ent.history.Append(PositionSample{Tick: tick, Position: ent.position})
		}
	}
}

// stepEntity runs one entity's movement phase and reports its
// MovementUpdate plus whether the sink should receive it. Position
// history is appended by the Step caller AFTER this returns, so the
// sample always sees the final authoritative position of the tick.
func (e *Engine) stepEntity(ent *entity, tick uint32) (MovementUpdate, bool) {
	processedNew := false
	if ent.hasPending {
		// Consume the newest pending control: yaw updates even for a
		// zero/stopped control, and the anchor advances to the
		// processed sequence.
		ent.activeHeldDirs = ent.pending.HeldDirs & MoveDirKnownMask
		ent.activeRun = ent.pending.RunFlag != 0
		ent.yaw = ent.pending.Yaw
		ent.lastProcessedSeq = ent.pending.InputSeq
		ent.hasProcessed = true
		ent.hasPending = false
		processedNew = true
	}

	dx, dz, active := heldDirectionVector(ent.activeHeldDirs, ent.yaw)
	if !active {
		// Stationary: no translation, no gate/collision queries.
		// Speed reports stopped.
		ent.speed = 0
		return MovementUpdate{
			Tick:                  tick,
			EntityID:              ent.id,
			Position:              ent.position,
			Yaw:                   ent.yaw,
			Speed:                 0,
			LastProcessedInputSeq: ent.lastProcessedSeq,
			VolumeFlags:           ent.volumeFlags,
		}, processedNew
	}

	// Select walk/run. The run gate is consulted only for genuinely
	// moving controls, never for stationary ones.
	speedMPS := WalkSpeedMetersPerSecond
	wireSpeed := uint8(WalkSpeedWire)
	if ent.activeRun && e.runGate.CanRun(ent.id) {
		speedMPS = RunSpeedMetersPerSecond
		wireSpeed = uint8(RunSpeedWire)
	}
	distance := speedMPS * e.dt
	intended := world.Vec3{
		X: ent.position.X + dx*distance,
		Y: ent.position.Y,
		Z: ent.position.Z + dz*distance,
	}

	// Defensive tripwire: the computed candidate must remain finite
	// and within selectedSpeed*dt + epsilon (spec §5.3.8). A client
	// cannot submit positions (102 is intent-only), so a violation
	// means an internal invariant broke, never normal play.
	if ok, observed := checkDisplacement(ent.position, intended, distance); !ok {
		kind := AnomalyExcessDisplacement
		if math.IsNaN(observed) || math.IsInf(observed, 0) ||
			math.IsNaN(intended.X) || math.IsNaN(intended.Z) ||
			math.IsInf(intended.X, 0) || math.IsInf(intended.Z, 0) {
			kind = AnomalyNonFiniteCandidate
		}
		e.observe(MovementAnomaly{
			EntityID:            ent.id,
			Tick:                tick,
			Kind:                kind,
			ExpectedMaxDistance: distance,
			ObservedDistance:    observed,
		})
		ent.speed = 0
		return e.update(ent, tick, 0, false, false), true
	}

	// Cross-cell staging BEFORE collision: M4-T3a owns handoff, so an
	// intended final position in another cell holds translation
	// exactly (no creep, no transfer), with yaw/anchor already
	// updated above.
	dest, err := world.CellForPosition(intended)
	if err != nil {
		e.observe(MovementAnomaly{
			EntityID:            ent.id,
			Tick:                tick,
			Kind:                AnomalyInvalidCandidate,
			ExpectedMaxDistance: distance,
			ObservedDistance:    distance,
		})
		ent.speed = 0
		return e.update(ent, tick, 0, false, false), true
	}
	if dest != ent.cell {
		ent.speed = 0
		return e.update(ent, tick, 0, false, true), true
	}

	// Deterministic collision substeps within the current cell.
	final := ent.position
	blocked := false
	substeps := int(math.Ceil(distance / MaxCollisionStepMeters))
	if substeps < 1 {
		substeps = 1
	}
	stepX := (intended.X - ent.position.X) / float64(substeps)
	stepZ := (intended.Z - ent.position.Z) / float64(substeps)
	for i := 0; i < substeps; i++ {
		candidate := world.Vec3{
			X: final.X + stepX,
			Y: ent.position.Y,
			Z: final.Z + stepZ,
		}
		if e.collision.SolidAt(candidate) {
			blocked = true
			break
		}
		final = candidate
	}
	ent.position = final
	ent.volumeFlags = e.collision.VolumeFlagsAt(final)
	if blocked {
		// Partial progress is retained, but the entity ends the step
		// stopped against the obstruction.
		ent.speed = 0
		return e.update(ent, tick, 0, true, false), true
	}
	ent.speed = wireSpeed
	return e.update(ent, tick, wireSpeed, false, false), true
}

// update builds the MovementUpdate for the entity's current
// authoritative state.
func (e *Engine) update(ent *entity, tick uint32, speed uint8, blocked, handoff bool) MovementUpdate {
	return MovementUpdate{
		Tick:                  tick,
		EntityID:              ent.id,
		Position:              ent.position,
		Yaw:                   ent.yaw,
		Speed:                 speed,
		LastProcessedInputSeq: ent.lastProcessedSeq,
		Blocked:               blocked,
		HandoffRequired:       handoff,
		VolumeFlags:           ent.volumeFlags,
	}
}

// observe reports an anomaly unless no observer is configured.
func (e *Engine) observe(a MovementAnomaly) {
	if e.anomaly != nil {
		e.anomaly.MovementAnomaly(a)
	}
}

// SubmitMove queues/replaces authoritative movement CONTROL state for
// an entity (spec §5.3.4). It does NOT immediately integrate
// position; positions change only on Step. Validation (entity
// existence, yaw validity, sample-tick sanity, serial InputSeq
// classification) is deterministic, and a rejected intent mutates
// nothing: no pending input, active control, accepted/processed
// sequence, position, or yaw change. In particular a rejected
// future-tick intent does NOT consume its InputSeq.
func (e *Engine) SubmitMove(id EntityID, intent MoveIntent) (MoveDisposition, error) {
	ent, err := e.registry.lookup(id)
	if err != nil {
		return MoveAccepted, err
	}
	if intent.Yaw > MaxYaw {
		return MoveAccepted, fmt.Errorf("%w: yaw %d", ErrInvalidMoveYaw, intent.Yaw)
	}
	if err := classifySampleTick(intent.SampleTick, e.tick.Load(), e.tickHz); err != nil {
		return MoveAccepted, err
	}
	if !ent.hasAccepted {
		// First input: ANY u32 sequence is accepted.
		ent.hasAccepted = true
		ent.lastAcceptedSeq = intent.InputSeq
		ent.pending = intent
		ent.hasPending = true
		return MoveAccepted, nil
	}
	if intent.InputSeq == ent.lastAcceptedSeq {
		// Duplicate (even with a different payload): no mutation, so
		// a client cannot rewrite control under a seen sequence.
		return MoveDuplicate, nil
	}
	if isAmbiguousSeq(intent.InputSeq, ent.lastAcceptedSeq) {
		return MoveAccepted, fmt.Errorf("%w: input %d vs accepted %d",
			ErrAmbiguousInputSeq, intent.InputSeq, ent.lastAcceptedSeq)
	}
	if serial32.After(intent.InputSeq, ent.lastAcceptedSeq) {
		ent.lastAcceptedSeq = intent.InputSeq
		ent.pending = intent
		ent.hasPending = true
		return MoveAccepted, nil
	}
	return MoveStale, nil
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

// AddEntity inserts a skeleton entity (see registry.AddEntity) and
// samples VolumeFlagsAt at the initial position (spec §5.3.6). History
// remains initially empty.
func (e *Engine) AddEntity(pos world.Vec3) (EntitySnapshot, error) {
	snap, err := e.registry.AddEntity(pos)
	if err != nil {
		return EntitySnapshot{}, err
	}
	ent, err := e.registry.lookup(snap.ID)
	if err != nil {
		return EntitySnapshot{}, fmt.Errorf("sim: entity %d vanished after add: %w", uint64(snap.ID), err)
	}
	ent.volumeFlags = e.collision.VolumeFlagsAt(pos)
	return ent.snapshot(), nil
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
