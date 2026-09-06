package sim

import (
	"errors"
	"fmt"
	"time"
)

// OpID is the opaque cross-cell operation identity (spec §5.1,
// §5.5.1). 0 is invalid/reserved and never issued; one generator
// never reuses an ID. An OpID is NOT a PostgreSQL ID, NOT an
// EntityID, NOT a session-local NetEntityID, and NOT a protocol
// sequence — there is no numeric relationship between those
// domains. Receivers treat it as opaque equality identity for
// idempotence, never as causal order.
type OpID uint64

// OpID bit-layout constants (spec §5.5.2): the 64-bit ID packs a
// 41-bit timestamp-millisecond delta, a 10-bit worker, and a 12-bit
// per-millisecond sequence. The high bit remains zero:
//
//	id = (timestampDelta << 22) | (workerID << 12) | sequence
const (
	// OpIDTimestampBits is the timestamp-delta width (41 bits,
	// roughly a 69-year window past the custom epoch).
	OpIDTimestampBits = 41
	// OpIDWorkerBits is the worker-ID width (10 bits, 1..1023).
	OpIDWorkerBits = 10
	// OpIDSequenceBits is the per-millisecond sequence width
	// (12 bits, 0..4095, i.e. up to 4096 IDs per worker per ms).
	OpIDSequenceBits = 12

	// OpIDWorkerShift positions the worker field above sequence.
	OpIDWorkerShift = OpIDSequenceBits
	// OpIDTimestampShift positions the timestamp field above
	// worker+sequence.
	OpIDTimestampShift = OpIDWorkerBits + OpIDSequenceBits

	// OpIDMaxWorker is the largest valid worker ID (1023).
	OpIDMaxWorker = (1 << OpIDWorkerBits) - 1
	// OpIDMaxSequence is the largest per-millisecond sequence (4095).
	OpIDMaxSequence = (1 << OpIDSequenceBits) - 1

	// opIDMaxTimestampDelta is the largest representable
	// epoch-relative millisecond delta (2^41 - 1).
	opIDMaxTimestampDelta = (1 << OpIDTimestampBits) - 1
)

// OpIDEpochUnixMillis is the frozen custom epoch
// (spec §5.5.3): 2026-01-01T00:00:00.000Z as Unix milliseconds.
// The timestamp portion of an ID is unixMillis(now) minus this.
const OpIDEpochUnixMillis int64 = 1767225600000

// Stable OpID-domain errors. Matching MUST use errors.Is, never
// string parsing as control flow.
var (
	// ErrOpIDSequenceExhausted marks the 4097th requested ID in one
	// millisecond (4096 IDs per worker per ms). Zero
	// generator-state corruption; the caller may retry once time
	// advances. The generator never sleeps, spins, wraps, or
	// borrows a future timestamp.
	ErrOpIDSequenceExhausted = errors.New("sim: op ID sequence exhausted")
	// ErrOpIDClockRegression marks an observed millisecond below
	// the last emitted timestamp. No ID is emitted and no state
	// rolls back; generation may resume once the clock catches up.
	ErrOpIDClockRegression = errors.New("sim: op ID clock regression")
	// ErrOpIDTimeBeforeEpoch marks a clock reading before the
	// custom epoch.
	ErrOpIDTimeBeforeEpoch = errors.New("sim: op ID time before epoch")
	// ErrOpIDTimestampExhausted marks an epoch-relative delta
	// beyond the 41-bit representable window. No truncation/wrap.
	ErrOpIDTimestampExhausted = errors.New("sim: op ID timestamp exhausted")
)

// OpIDNow is the injected millisecond-clock seam (spec §5.5.5),
// returning Unix milliseconds. Generator tests script this clock;
// production may pass ProductionOpIDNow. No hidden time.Now in
// generator tests.
type OpIDNow func() int64

// ProductionOpIDNow wraps time.Now().UnixMilli for future
// executable wiring (no cmd/serve wiring yet).
func ProductionOpIDNow() int64 { return time.Now().UnixMilli() }

// OpIDGenerator issues Snowflake-style OpIDs for one worker
// (spec §5.5). It keeps the sim single-writer model: one owning
// writer per generator, no mutex, NOT safe for arbitrary
// concurrent callers. Future workers each use their own worker
// ID/generator. Generation uses time plus the explicit worker
// plus a counter only: no RNG (never the engine RNG), no sleeps,
// no busy waits, no goroutines.
type OpIDGenerator struct {
	worker     uint64
	now        OpIDNow
	hasLast    bool
	lastMillis int64
	sequence   uint64
}

// NewOpIDGenerator validates the worker and clock seam. Worker 0
// is reserved/invalid (guaranteeing generated OpID 0 is
// impossible); valid workers are 1..1023; a nil clock is
// rejected. No random worker ID, no hostname hashing: the
// deployment configuration chooses the worker explicitly.
func NewOpIDGenerator(workerID uint16, now OpIDNow) (*OpIDGenerator, error) {
	if workerID == 0 || workerID > OpIDMaxWorker {
		return nil, fmt.Errorf("%w: op worker %d out of range 1..%d",
			ErrInvalidConfig, workerID, OpIDMaxWorker)
	}
	if now == nil {
		return nil, fmt.Errorf("%w: nil op ID clock", ErrInvalidConfig)
	}
	return &OpIDGenerator{worker: uint64(workerID), now: now}, nil
}

// Next issues the next ID (spec §5.5.6–§5.5.8): a new
// millisecond resets the sequence to 0; the same millisecond
// increments it. Sequence exhaustion, clock regression,
// pre-epoch time, and timestamp overflow are explicit errors
// that corrupt no state and never cause duplicate IDs — after an
// error and valid forward time, generation resumes safely.
func (g *OpIDGenerator) Next() (OpID, error) {
	now := g.now()
	delta := now - OpIDEpochUnixMillis
	if delta < 0 {
		return 0, fmt.Errorf("%w: clock %d before epoch %d",
			ErrOpIDTimeBeforeEpoch, now, OpIDEpochUnixMillis)
	}
	if delta > opIDMaxTimestampDelta {
		return 0, fmt.Errorf("%w: delta %d beyond 41 bits",
			ErrOpIDTimestampExhausted, delta)
	}
	if g.hasLast && now < g.lastMillis {
		return 0, fmt.Errorf("%w: clock %d below last %d",
			ErrOpIDClockRegression, now, g.lastMillis)
	}
	var seq uint64
	if g.hasLast && now == g.lastMillis {
		if g.sequence >= OpIDMaxSequence {
			return 0, fmt.Errorf("%w: worker %d at ms %d",
				ErrOpIDSequenceExhausted, g.worker, now)
		}
		seq = g.sequence + 1
	}
	g.hasLast = true
	g.lastMillis = now
	g.sequence = seq
	return OpID(uint64(delta)<<OpIDTimestampShift | g.worker<<OpIDWorkerShift | seq), nil
}

// Split unpacks an ID's fields for inspection/tests (spec
// §5.5.19). Gameplay MUST NOT depend on unpacked
// timestamp/worker/sequence; receiver dedupe treats OpID as
// opaque equality identity.
func (id OpID) Split() (timestampDelta uint64, workerID uint16, sequence uint16) {
	timestampDelta = uint64(id) >> OpIDTimestampShift
	workerID = uint16(uint64(id) >> OpIDWorkerShift & OpIDMaxWorker)
	sequence = uint16(uint64(id) & OpIDMaxSequence)
	return timestampDelta, workerID, sequence
}
