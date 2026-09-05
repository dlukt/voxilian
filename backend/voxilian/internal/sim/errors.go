package sim

import "errors"

// Stable registry/engine errors (spec §5.2). Exact naming is an
// implementation choice; matching MUST use errors.Is, never string
// parsing as control flow.
var (
	// ErrEntityNotFound marks lookup/remove/history of an unknown EntityID.
	ErrEntityNotFound = errors.New("sim: entity not found")
	// ErrInvalidPosition marks a rejected simulation position (NaN, Inf,
	// or an X/Z cell that cannot fit int32). It wraps the world-domain
	// validation error; errors.Is matches both layers.
	ErrInvalidPosition = errors.New("sim: invalid position")
	// ErrCellHandoffRequired marks a same-cell position mutation whose
	// destination belongs to another cell. M4-T3a owns real ownership
	// handoff; T1 fails explicitly and mutates nothing.
	ErrCellHandoffRequired = errors.New("sim: cell handoff required")
	// ErrInvalidConfig marks an invalid EngineConfig/EngineDeps value.
	ErrInvalidConfig = errors.New("sim: invalid config")
	// ErrEntityIDExhausted marks AddEntity when the monotonic EntityID
	// allocator is exhausted (math.MaxUint64 already issued). IDs are
	// never reused and the reserved zero ID is never issued; the failed
	// add mutates nothing. Match with errors.Is.
	ErrEntityIDExhausted = errors.New("sim: entity ID exhausted")
)
