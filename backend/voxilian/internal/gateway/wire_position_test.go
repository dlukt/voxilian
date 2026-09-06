package gateway

import (
	"errors"
	"math"
	"testing"

	"github.com/dlukt/voxilian/internal/proto"
	"github.com/dlukt/voxilian/internal/world"
)

func TestWirePositionBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		meters  float64
		wantMM  int32
		wantErr bool
	}{
		{"zero", 0, 0, false},
		{"round down positive", 0.0004, 0, false},
		{"round half up positive", 0.0005, 1, false},
		{"round down negative", -0.0004, 0, false},
		{"round half away negative", -0.0005, -1, false},
		{"ordinary positive", 4.567, 4567, false},
		{"ordinary negative", -12.345, -12345, false},
		{"exact max boundary", 2147483.647, math.MaxInt32, false},
		{"just outside max", 2147483.648, 0, true},
		{"exact min boundary", -2147483.648, math.MinInt32, false},
		{"just outside min", -2147483.649, 0, true},
		{"huge", 1e12, 0, true},
		{"NaN", math.NaN(), 0, true},
		{"+Inf", math.Inf(1), 0, true},
		{"-Inf", math.Inf(-1), 0, true},
	} {
		got, err := WirePosition(world.Vec3{X: tc.meters, Y: tc.meters, Z: tc.meters})
		if tc.wantErr {
			if !errors.Is(err, ErrWirePositionRange) {
				t.Errorf("%s: err = %v, want ErrWirePositionRange", tc.name, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got != (proto.Position{X: tc.wantMM, Y: tc.wantMM, Z: tc.wantMM}) {
			t.Errorf("%s: got %v, want %d", tc.name, got, tc.wantMM)
		}
	}
}

func TestWirePositionAxesIndependent(t *testing.T) {
	got, err := WirePosition(world.Vec3{X: 1.5, Y: -2.25, Z: 0.0005})
	if err != nil {
		t.Fatal(err)
	}
	if got != (proto.Position{X: 1500, Y: -2250, Z: 1}) {
		t.Fatalf("got %v", got)
	}
	// One bad axis fails the whole conversion without clamping others.
	if _, err := WirePosition(world.Vec3{X: 1, Y: math.NaN(), Z: 3}); !errors.Is(err, ErrWirePositionRange) {
		t.Fatalf("partial NaN err = %v", err)
	}
}
