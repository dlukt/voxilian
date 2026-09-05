package sim

import (
	"context"
	"fmt"
	"math"
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
// §5.2.8/§5.3.7/§5.4.3):
//
//  1. allocate/increment tick (0 -> 1 for the first step; u32 wrap is normal)
//  2. capture the tick-start resident ownership worklist in canonical
//     CellCoord/EntityID order with expected generations
//  3. per work item (at most once per entity per tick): verify still
//     resident under the expected epoch, else skip
//  4. consume newest pending movement input into active yaw/control/anchor
//  5. integrate horizontal movement (walk/run via RunGate)
//  6. resolve collision substeps across the path, then hand off on a
//     cross-cell final position (or hold on generation exhaustion)
//  7. sample final volume flags
//  8. emit MovementUpdate through the sink when applicable
//  9. append each processed entity's post-step position-history sample
//
// A migrated-in entity is absent from the tick-start worklist, so it
// can never be processed twice in one tick; it becomes eligible on
// the NEXT tick. Because handoff (not removal) is the only
// membership change, iteration needs no copy-on-write. Future
// systems insert their phase BEFORE history sampling; gameplay MUST
// NOT run after the same tick's historical sample.
func (e *Engine) Step() {
	tick := e.tick.Add(1)
	for _, item := range e.registry.residentWorklist() {
		ent := e.registry.verifyResident(item)
		if ent == nil {
			continue
		}
		if update, emit := e.stepEntity(ent, tick); emit && e.movement != nil {
			e.movement.OnMovement(update)
		}
		ent.history.Append(PositionSample{Tick: tick, Position: ent.position})
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
		// An idle entity (nothing newly processed) emits nothing and
		// performs no world queries at all. A newly processed
		// zero-direction control finalizes normally so its update
		// carries freshly sampled volume flags.
		if !processedNew {
			ent.speed = 0
			return MovementUpdate{
				Tick:                  tick,
				EntityID:              ent.id,
				Position:              ent.position,
				Yaw:                   ent.yaw,
				Speed:                 0,
				LastProcessedInputSeq: ent.lastProcessedSeq,
				VolumeFlags:           ent.volumeFlags,
			}, false
		}
		return e.finalize(ent, tick, 0, false, false), true
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
		return e.finalize(ent, tick, 0, false, false), true
	}

	// Deterministic collision substeps across the full displacement.
	// Candidates may cross a cell boundary: collision data is world
	// query data, and routing happens on the ACTUAL final
	// authoritative position (spec §5.4.4). This supersedes the
	// temporary T2 handoff-before-collision staging.
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
	dest, err := world.CellForPosition(final)
	if err != nil {
		e.observe(MovementAnomaly{
			EntityID:            ent.id,
			Tick:                tick,
			Kind:                AnomalyInvalidCandidate,
			ExpectedMaxDistance: distance,
			ObservedDistance:    distance,
		})
		return e.finalize(ent, tick, 0, false, false), true
	}
	if dest == ent.cell {
		ent.position = final
		if blocked {
			// Partial progress is retained, but the entity ends the step
			// stopped against the obstruction.
			return e.finalize(ent, tick, 0, true, false), true
		}
		return e.finalize(ent, tick, wireSpeed, false, false), true
	}
	// Cross-cell final: real ownership handoff (spec §5.4.4/§5.4.5).
	// All source gameplay calculation is complete; begin quiesces the
	// source and commit installs the destination in the same Step.
	start := ent.position
	ent.position = final
	from := OwnerRef{Cell: ent.cell, Generation: ent.generation}
	tok, err := e.registry.beginHandoff(ent.id, from, dest)
	if err != nil {
		// Generation exhaustion (or unexpected mismatch): hold the
		// source position with zero transfer. Yaw/anchor were
		// already consumed above and still advance.
		ent.position = start
		return e.finalize(ent, tick, 0, false, true), true
	}
	speed := wireSpeed
	if blocked {
		speed = 0
	}
	if _, err := e.registry.commitHandoff(tok); err != nil {
		// Practically unreachable locally (the token was just
		// issued): roll back rather than strand the entity
		// mid-migration, then hold as above.
		e.registry.abortHandoff(ent.id)
		ent.position = start
		return e.finalize(ent, tick, 0, false, true), true
	}
	return e.finalize(ent, tick, speed, blocked, false), true
}

// finalize is the single movement finalization path (spec §5.3.6):
// every movement-emitting outcome passes through here. It sets the
// final speed, samples VolumeFlagsAt at the final authoritative
// position (even when translation was held, staged, or defensively
// corrected), updates entity volume flags, and builds the
// MovementUpdate. A never-controlled idle entity never reaches this
// path, so it performs no world queries merely for volume flags.
func (e *Engine) finalize(ent *entity, tick uint32, speed uint8, blocked, handoff bool) MovementUpdate {
	ent.speed = speed
	ent.volumeFlags = e.collision.VolumeFlagsAt(ent.position)
	return e.update(ent, tick, speed, blocked, handoff)
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
// an entity (spec §5.3.4/§5.4.7). When RESIDENT it does NOT
// immediately integrate position; positions change only on Step.
// While MIGRATING, valid newer intents route into the entity's
// bounded migration queue without touching active control, the
// processed anchor, or the quiesced source. Validation (entity
// existence, yaw validity, sample-tick sanity, serial InputSeq
// classification) is deterministic, and a rejected intent mutates
// nothing: no pending input, active control, accepted/processed
// sequence, position, yaw, queue content, or routing frontier change.
// In particular a rejected future-tick intent does NOT consume its
// InputSeq.
func (e *Engine) SubmitMove(id EntityID, intent MoveIntent) (MoveDisposition, error) {
	if rec, ok := e.registry.migrations[id]; ok {
		return e.submitMigrating(rec, intent)
	}
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
	d, err := classifyMoveIntent(ent.hasAccepted, ent.lastAcceptedSeq, intent.InputSeq)
	if err != nil || d != MoveAccepted {
		// Duplicate/stale are no-ops (a client cannot rewrite
		// control under a seen sequence); ambiguity errors out.
		return d, err
	}
	ent.hasAccepted = true
	ent.lastAcceptedSeq = intent.InputSeq
	ent.pending = intent
	ent.hasPending = true
	return MoveAccepted, nil
}

// submitMigrating routes one intent into a quiesced migration's
// bounded FIFO (spec §5.4.7). Stateless validation runs before queue
// capacity is touched; sequencing uses the route's private frontier
// (seeded from the entity's accepted state at begin); only newer
// accepted intents append, advancing the frontier — never entity
// control state. A full queue rejects with ErrMigrationQueueFull and
// zero mutation, leaving the InputSeq retryable.
func (e *Engine) submitMigrating(rec *migrationRecord, intent MoveIntent) (MoveDisposition, error) {
	if intent.Yaw > MaxYaw {
		return MoveAccepted, fmt.Errorf("%w: yaw %d", ErrInvalidMoveYaw, intent.Yaw)
	}
	if err := classifySampleTick(intent.SampleTick, e.tick.Load(), e.tickHz); err != nil {
		return MoveAccepted, err
	}
	d, err := classifyMoveIntent(rec.hasFrontier, rec.frontier, intent.InputSeq)
	if err != nil || d != MoveAccepted {
		return d, err
	}
	if len(rec.queued) >= MigrationMoveQueueCapacity {
		return MoveAccepted, fmt.Errorf("%w: id %d len %d",
			ErrMigrationQueueFull, uint64(rec.id), len(rec.queued))
	}
	rec.queued = append(rec.queued, intent)
	rec.hasFrontier = true
	rec.frontier = intent.InputSeq
	return MoveAccepted, nil
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
