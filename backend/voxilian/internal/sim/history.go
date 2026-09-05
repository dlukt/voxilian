package sim

import (
	"github.com/dlukt/voxilian/internal/world"
)

// PositionSample is one simulation-time position record (spec §5.2.6).
// No wall-clock timestamp: history is simulation-time data. The sample
// represents the entity's final authoritative position of its tick.
type PositionSample struct {
	Tick     uint32
	Position world.Vec3
}

// positionHistory is a real fixed-capacity ring buffer: oldest entries
// are overwritten when full, steady state performs no allocations, and
// inspection returns oldest -> newest copies (spec §5.2.6). Wrapped
// tick numbers are NEVER sorted numerically; ring order itself is
// chronological.
type positionHistory struct {
	buf   []PositionSample
	start int // index of the oldest valid sample
	count int // number of valid samples (<= len(buf))
}

// newPositionHistory allocates the ring once at entity creation.
// Capacity MUST be positive (2 * tickHz); callers validate.
func newPositionHistory(capacity int) *positionHistory {
	return &positionHistory{buf: make([]PositionSample, capacity)}
}

// Append records one post-tick sample, overwriting the oldest when full.
// Steady state performs no allocations.
func (h *positionHistory) Append(s PositionSample) {
	if len(h.buf) == 0 {
		return
	}
	if h.count < len(h.buf) {
		h.buf[(h.start+h.count)%len(h.buf)] = s
		h.count++
		return
	}
	h.buf[h.start] = s
	h.start = (h.start + 1) % len(h.buf)
}

// Len returns the number of retained samples.
func (h *positionHistory) Len() int { return h.count }

// Capacity returns the fixed ring capacity.
func (h *positionHistory) Capacity() int { return len(h.buf) }

// Samples returns a fresh copy of retained samples, oldest -> newest.
// Mutating the result MUST NOT alter internal ring state.
func (h *positionHistory) Samples() []PositionSample {
	out := make([]PositionSample, h.count)
	for i := 0; i < h.count; i++ {
		out[i] = h.buf[(h.start+i)%len(h.buf)]
	}
	return out
}

// Latest returns the newest sample (false when empty).
func (h *positionHistory) Latest() (PositionSample, bool) {
	if h.count == 0 {
		return PositionSample{}, false
	}
	return h.buf[(h.start+h.count-1)%len(h.buf)], true
}
