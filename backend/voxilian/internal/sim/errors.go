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
	// ErrInvalidCharacterID marks a player add/attach with a
	// non-durable identity (spec §9.5.1d): only CharacterID > 0 may
	// bind to a player entity. Zero mutation.
	ErrInvalidCharacterID = errors.New("sim: invalid character ID")
	// ErrCharacterAlreadyActive marks a player add/attach whose
	// CharacterID is already bound to another live entity
	// (spec §9.5.1d): one-live-entity-per-CharacterID, where live
	// includes RESIDENT and MIGRATING. Zero mutation, and no EntityID
	// is consumed by the rejected add.
	ErrCharacterAlreadyActive = errors.New("sim: character already active")
	// ErrPlayerNotAlive marks an ordinary gameplay mutation/input
	// targeting a player whose life state is DeathPersisting or
	// AwaitingRespawn (spec §9.5.1f, M5-T5c3c1). Zero mutation:
	// rejected before any gameplay state changes and before any
	// movement InputSeq is consumed.
	ErrPlayerNotAlive = errors.New("sim: player not alive")
	// ErrPlayerNotDead marks a begin-death request while the
	// authoritative vitals HP != 0 (spec §9.5.1f, M5-T5c3c1). Zero
	// mutation.
	ErrPlayerNotDead = errors.New("sim: player not dead")
	// ErrDeathAttemptMismatch marks a post-death completion whose
	// token does not match the entity's current character, epoch,
	// or life state (spec §9.5.1f, M5-T5c3c1). Zero mutation.
	ErrDeathAttemptMismatch = errors.New("sim: death attempt mismatch")
	// ErrDeathAttemptExhausted marks a begin-death request whose
	// per-entity death epoch cannot advance without wrapping
	// (already math.MaxUint64; spec §9.5.1f, M5-T5c3c1). Zero
	// mutation: the epoch does not advance.
	ErrDeathAttemptExhausted = errors.New("sim: death attempt exhausted")
)
