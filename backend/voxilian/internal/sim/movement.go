package sim

import (
	"errors"
	"fmt"
	"math"

	"github.com/dlukt/voxilian/internal/serial32"
	"github.com/dlukt/voxilian/internal/world"
)

// Movement constants (spec §5.3.2/§5.3.3/§5.3.6).
const (
	// WalkSpeedMetersPerSecond is the exact horizontal walk target.
	WalkSpeedMetersPerSecond = 3.5
	// RunSpeedMetersPerSecond is the exact horizontal run target.
	RunSpeedMetersPerSecond = 7.0
	// WalkSpeedWire is the 205-compatible u8 speed for walking
	// (decimeters per second).
	WalkSpeedWire = 35
	// RunSpeedWire is the 205-compatible u8 speed for running.
	RunSpeedWire = 70
	// MaxCollisionStepMeters caps one point-query collision substep so
	// fast ticks cannot tunnel through thin obstructions.
	MaxCollisionStepMeters = 0.25
	// MaxYaw is the largest valid 12-bit yaw value.
	MaxYaw = 4095

	// MoveDir* are the heldDirs bit assignments (spec §5.3.2).
	MoveDirForward  = 1 << 0
	MoveDirBackward = 1 << 1
	MoveDirLeft     = 1 << 2
	MoveDirRight    = 1 << 3
	// MoveDirKnownMask covers the four movement bits; reserved high
	// bits 4..7 are ignored for movement semantics, never rejected.
	MoveDirKnownMask = 0x0f

	// displacementEpsilonMeters is a tiny floating-arithmetic tolerance
	// for the anomaly tripwire — never gameplay slack.
	displacementEpsilonMeters = 1e-9
)

// Stable movement-domain errors. Matching MUST use errors.Is, never
// string parsing as control flow.
var (
	// ErrInvalidMoveYaw marks a sim-domain MoveIntent with Yaw > 4095.
	ErrInvalidMoveYaw = errors.New("sim: invalid movement yaw")
	// ErrFutureInputTick marks a MoveIntent whose sample tick is more
	// than 5 simulated seconds in the future of the current sim tick.
	ErrFutureInputTick = errors.New("sim: future input tick")
	// ErrAmbiguousInputSeq marks a MoveIntent exactly 2^31 away from
	// the newest accepted input sequence (serially neither before
	// nor after).
	ErrAmbiguousInputSeq = errors.New("sim: ambiguous input sequence")
	// ErrAmbiguousSampleTick marks a MoveIntent whose sample tick is
	// exactly 2^31 away from the current sim tick.
	ErrAmbiguousSampleTick = errors.New("sim: ambiguous sample tick")
)

// MoveIntent is the sim-domain movement control matching opcode 102
// (spec §5.3.4). SampleTick comes from the C→S protocol header tick;
// the protocol Header itself never enters sim. This is NOT proto.Move.
type MoveIntent struct {
	InputSeq   uint32
	HeldDirs   uint8
	RunFlag    uint8
	Yaw        uint16
	SampleTick uint32
}

// MoveDisposition is the SubmitMove outcome. Duplicate/stale are
// ordinary no-op results, not errors. On error the returned
// disposition is meaningless — check err first.
type MoveDisposition int

const (
	// MoveAccepted means the intent became the newest pending control.
	MoveAccepted MoveDisposition = iota
	// MoveDuplicate means the sequence was already accepted; nothing changed.
	MoveDuplicate
	// MoveStale means the sequence is serially older; nothing changed.
	MoveStale
)

// String names the disposition for logs/tests.
func (d MoveDisposition) String() string {
	switch d {
	case MoveAccepted:
		return "accepted"
	case MoveDuplicate:
		return "duplicate"
	case MoveStale:
		return "stale"
	default:
		return "unknown"
	}
}

// CollisionWorld is the minimal consumer-defined collision/volume
// query seam owned by M4-T2 (spec §5.3.5). Queries are hot-path
// in-memory simulation reads: no network/PG I/O and no transient
// persistence errors. The richer future WorldSource MUST
// implement/embed/satisfy this seam; M10 MUST NOT invent a second
// collision abstraction.
type CollisionWorld interface {
	// SolidAt reports whether a position is obstructed.
	SolidAt(world.Vec3) bool
	// VolumeFlagsAt samples the opaque volume-flag bitset at a position.
	VolumeFlagsAt(world.Vec3) world.VolumeFlags
}

// RunGate is the narrow vigor hook for running (spec §5.3.3). Run
// requests with CanRun == false fall back to WALK; the move is never
// rejected and no vigor is mutated here. A later vitals task supplies
// the real (vigor >= 10) decision behind this gate.
type RunGate interface {
	CanRun(EntityID) bool
}

// MovementUpdate is the internal 205-COMPATIBLE sim movement output
// (spec §5.3.7). It is NOT proto.EntityMove: it carries the internal
// EntityID (uint64, never truncated to a session NetEntityID) and the
// real processed anchor (M4-T5 later zeroes the anchor for non-owner
// viewers on the wire).
type MovementUpdate struct {
	Tick                  uint32
	EntityID              EntityID
	Position              world.Vec3
	Yaw                   uint16
	Speed                 uint8
	LastProcessedInputSeq uint32
	Blocked               bool
	HandoffRequired       bool
	VolumeFlags           world.VolumeFlags
}

// MovementSink receives already-authoritative movement results for
// future fanout (spec §5.3.8). It is output only: it cannot change
// position, approve run, resolve collision, or alter the anchor.
// Implementations MUST be non-blocking/bounded; M4-T5 satisfies this
// via the existing non-blocking outbound state path.
type MovementSink interface {
	OnMovement(MovementUpdate)
}

// MovementAnomalyKind names the defensive tripwire violation.
type MovementAnomalyKind string

const (
	// AnomalyNonFiniteCandidate marks a non-finite movement candidate.
	AnomalyNonFiniteCandidate MovementAnomalyKind = "non_finite_candidate"
	// AnomalyExcessDisplacement marks horizontal displacement beyond
	// selectedSpeed * fixedDT + epsilon.
	AnomalyExcessDisplacement MovementAnomalyKind = "excess_displacement"
	// AnomalyInvalidCandidate marks a candidate the world domain
	// rejects (e.g. an int32-unrepresentable destination cell).
	AnomalyInvalidCandidate MovementAnomalyKind = "invalid_candidate"
)

// MovementAnomaly is the low-cardinality domain record reported
// through MovementObserver. No wire/session objects.
type MovementAnomaly struct {
	EntityID            EntityID
	Tick                uint32
	Kind                MovementAnomalyKind
	ExpectedMaxDistance float64
	ObservedDistance    float64
}

// MovementObserver is the narrow anomaly hook (spec §5.3.8). No
// Prometheus, gateway, or session labels here; runtime wiring may add
// structured logging later.
type MovementObserver interface {
	MovementAnomaly(MovementAnomaly)
}

// RunGateFunc adapts a plain function to a RunGate.
type RunGateFunc func(EntityID) bool

// CanRun implements RunGate.
func (f RunGateFunc) CanRun(id EntityID) bool { return f(id) }

// MovementSinkFunc adapts a plain function to a MovementSink.
type MovementSinkFunc func(MovementUpdate)

// OnMovement implements MovementSink.
func (f MovementSinkFunc) OnMovement(u MovementUpdate) { f(u) }

// MovementObserverFunc adapts a plain function to a MovementObserver.
type MovementObserverFunc func(MovementAnomaly)

// MovementAnomaly implements MovementObserver.
func (f MovementObserverFunc) MovementAnomaly(a MovementAnomaly) { f(a) }

// heldDirectionVector resolves heldDirs + yaw to a horizontal unit XZ
// vector. Opposing directions cancel; a lone-axis result has length 1;
// a diagonal (one longitudinal + one lateral) is normalized to length
// 1 so diagonal speed never exceeds axial speed. Yaw follows the
// Godot-friendly convention: yaw 0 faces -Z, positive turns clockwise
// viewed from +Y. Reserved high held bits are ignored. It reports
// whether any known direction is held.
func heldDirectionVector(heldDirs uint8, yaw uint16) (dx, dz float64, active bool) {
	dirs := heldDirs & MoveDirKnownMask
	longitudinal := 0.0
	if dirs&MoveDirForward != 0 {
		longitudinal++
	}
	if dirs&MoveDirBackward != 0 {
		longitudinal--
	}
	lateral := 0.0
	if dirs&MoveDirRight != 0 {
		lateral++
	}
	if dirs&MoveDirLeft != 0 {
		lateral--
	}
	if longitudinal == 0 && lateral == 0 {
		return 0, 0, false
	}
	theta := float64(yaw) * 2 * math.Pi / 4096
	sinT, cosT := math.Sin(theta), math.Cos(theta)
	// forward = (sinT, -cosT); right = (cosT, sinT).
	dx = longitudinal*sinT + lateral*cosT
	dz = longitudinal*(-cosT) + lateral*sinT
	if longitudinal != 0 && lateral != 0 {
		inv := 1 / math.Hypot(dx, dz)
		dx *= inv
		dz *= inv
	}
	return dx, dz, true
}

// checkDisplacement is the defensive tripwire invariant (spec
// §5.3.8): the base→candidate horizontal displacement must be finite
// and within maxDist + epsilon. It returns whether the candidate is
// acceptable and the observed horizontal distance. Only XZ matters;
// movement is horizontal by construction.
func checkDisplacement(base, candidate world.Vec3, maxDist float64) (bool, float64) {
	if math.IsNaN(candidate.X) || math.IsNaN(candidate.Z) ||
		math.IsInf(candidate.X, 0) || math.IsInf(candidate.Z, 0) {
		return false, math.NaN()
	}
	observed := math.Hypot(candidate.X-base.X, candidate.Z-base.Z)
	if math.IsNaN(observed) || math.IsInf(observed, 0) {
		return false, observed
	}
	return observed <= maxDist+displacementEpsilonMeters, observed
}

// isAmbiguousSeq reports whether two unequal sequences are exactly
// 2^31 apart — serially neither before nor after.
func isAmbiguousSeq(a, b uint32) bool {
	return a != b && !serial32.After(a, b) && !serial32.Before(a, b)
}

// classifySampleTick validates a MoveIntent sample tick against the
// current sim tick (spec §5.3.4): past ticks stay acceptable, exactly
// current+5*H is accepted, anything serially further future is
// rejected, and the exact 2^31 ambiguity is invalid.
func classifySampleTick(sample, current uint32, tickHz int) error {
	if sample == current {
		return nil
	}
	if isAmbiguousSeq(sample, current) {
		return fmt.Errorf("%w: sample %d vs current %d", ErrAmbiguousSampleTick, sample, current)
	}
	if serial32.After(sample, current) {
		if sample-current > uint32(5*tickHz) {
			return fmt.Errorf("%w: sample %d vs current %d at %d Hz",
				ErrFutureInputTick, sample, current, tickHz)
		}
	}
	return nil
}
