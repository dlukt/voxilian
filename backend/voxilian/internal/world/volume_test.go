package world

import "testing"

func TestVolumeNoneIsZero(t *testing.T) {
	var zero VolumeFlags
	if VolumeNone != 0 || zero != VolumeNone {
		t.Fatalf("VolumeNone = %d, want opaque zero", uint32(VolumeNone))
	}
	// Opaque bitset: distinct values round-trip without interpretation.
	var f VolumeFlags = 0xA5
	if uint32(f) != 0xA5 {
		t.Fatalf("VolumeFlags = %d, want preserved bits", uint32(f))
	}
}
