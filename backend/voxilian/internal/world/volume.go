package world

// VolumeFlags is the opaque volume-flag bitset sampled at an entity's
// authoritative position (spec §5.3.5). M4-T2 merely preserves/samples
// these bits as sim state; safe-zone, no-PK, sanctuary, interior, and
// other gameplay interpretations belong to later content/gameplay work,
// which assigns flag bit numbers then — not here.
//
// The richer future WorldSource MUST implement/embed/satisfy the sim's
// minimal collision/volume query seam rather than replacing movement
// with a second collision abstraction.
type VolumeFlags uint32

// VolumeNone is the zero volume-flag value: no volume annotations at
// the sampled position.
const VolumeNone VolumeFlags = 0
