package world

import (
	"errors"
	"math"
	"testing"
)

func TestConstants(t *testing.T) {
	if ChunkEdgeVoxels != 16 {
		t.Fatalf("ChunkEdgeVoxels = %d, want 16", ChunkEdgeVoxels)
	}
	if CellSizeMeters != 32 {
		t.Fatalf("CellSizeMeters = %d, want 32", CellSizeMeters)
	}
	if DefaultAOIRadiusMeters != 96 {
		t.Fatalf("DefaultAOIRadiusMeters = %d, want 96", DefaultAOIRadiusMeters)
	}
}

func TestCellForPositionBoundaries(t *testing.T) {
	cases := []struct {
		x    float64
		want int32
	}{
		{0.0, 0},
		{31.999, 0},
		{32.0, 1},
		{64.0, 2},
		{-0.001, -1},
		{-31.999, -1},
		{-32.0, -1},
		{-32.001, -2},
		{-64.0, -2},
	}
	for _, tc := range cases {
		for _, axis := range []string{"X", "Z"} {
			p := Vec3{}
			if axis == "X" {
				p = Vec3{X: tc.x}
			} else {
				p = Vec3{Z: tc.x}
			}
			got, err := CellForPosition(p)
			if err != nil {
				t.Fatalf("CellForPosition(%s=%v) error: %v", axis, tc.x, err)
			}
			gotAxis := got.X
			if axis == "Z" {
				gotAxis = got.Z
			}
			if gotAxis != tc.want {
				t.Fatalf("CellForPosition(%s=%v) = %v, want cell %d", axis, tc.x, got, tc.want)
			}
			// The other axis at 0 must map to cell 0.
			other := got.Z
			if axis == "Z" {
				other = got.X
			}
			if other != 0 {
				t.Fatalf("CellForPosition(%s=%v) other axis = %d, want 0", axis, tc.x, other)
			}
		}
	}
}

func TestCellForPositionExactPairs(t *testing.T) {
	p := Vec3{X: 32.0, Z: -32.0}
	got, err := CellForPosition(p)
	if err != nil {
		t.Fatalf("CellForPosition error: %v", err)
	}
	if got != (CellCoord{X: 1, Z: -1}) {
		t.Fatalf("CellForPosition = %v, want {1 -1}", got)
	}
}

func TestValidatePositionNonFinite(t *testing.T) {
	bads := []Vec3{
		{X: math.NaN()},
		{Y: math.NaN()},
		{Z: math.NaN()},
		{X: math.Inf(1)},
		{X: math.Inf(-1)},
		{Y: math.Inf(1)},
		{Y: math.Inf(-1)},
		{Z: math.Inf(1)},
		{Z: math.Inf(-1)},
	}
	for i, p := range bads {
		if err := ValidatePosition(p); !errors.Is(err, ErrInvalidPosition) {
			t.Fatalf("case %d (%v): ValidatePosition = %v, want ErrInvalidPosition", i, p, err)
		}
		if _, err := CellForPosition(p); !errors.Is(err, ErrInvalidPosition) {
			t.Fatalf("case %d (%v): CellForPosition = %v, want ErrInvalidPosition", i, p, err)
		}
	}
}

func TestValidatePositionCellOverflow(t *testing.T) {
	// 2^36 = 68719476736; /32 = 2^31, one past MaxInt32.
	const huge = float64(int64(1) << 36)
	// Exact multiples of 32 below 2^53 are exactly representable.
	validMax := Vec3{X: huge - 32} // cell MaxInt32
	validMin := Vec3{X: -huge}     // cell MinInt32
	overPos := Vec3{X: huge}       // cell 2^31: overflow
	overNeg := Vec3{X: -huge - 32} // cell MinInt32-1: overflow
	overPosZ := Vec3{Z: huge}
	overNegZ := Vec3{Z: -huge - 32}

	for _, p := range []Vec3{validMax, validMin} {
		if err := ValidatePosition(p); err != nil {
			t.Fatalf("ValidatePosition(%v) = %v, want nil", p, err)
		}
	}
	for _, p := range []Vec3{overPos, overNeg, overPosZ, overNegZ} {
		if err := ValidatePosition(p); !errors.Is(err, ErrInvalidPosition) {
			t.Fatalf("ValidatePosition(%v) = %v, want ErrInvalidPosition", p, err)
		}
		if _, err := CellForPosition(p); !errors.Is(err, ErrInvalidPosition) {
			t.Fatalf("CellForPosition(%v) = %v, want ErrInvalidPosition", p, err)
		}
	}

	got, err := CellForPosition(validMax)
	if err != nil {
		t.Fatalf("CellForPosition(validMax) error: %v", err)
	}
	if got.X != math.MaxInt32 {
		t.Fatalf("validMax cell = %d, want %d", got.X, int32(math.MaxInt32))
	}
	got, err = CellForPosition(validMin)
	if err != nil {
		t.Fatalf("CellForPosition(validMin) error: %v", err)
	}
	if got.X != math.MinInt32 {
		t.Fatalf("validMin cell = %d, want %d", got.X, int32(math.MinInt32))
	}
}

func TestValidatePositionExtremeFiniteY(t *testing.T) {
	p := Vec3{X: 1.5, Y: math.MaxFloat64, Z: -2.5}
	if err := ValidatePosition(p); err != nil {
		t.Fatalf("ValidatePosition(extreme finite Y) = %v, want nil", err)
	}
	got, err := CellForPosition(p)
	if err != nil {
		t.Fatalf("CellForPosition(extreme finite Y) error: %v", err)
	}
	if got != (CellCoord{X: 0, Z: -1}) {
		t.Fatalf("CellForPosition = %v, want {0 -1}", got)
	}
}

func TestCompareCellCoordsOrdering(t *testing.T) {
	in := []CellCoord{{1, 0}, {-1, 5}, {0, 0}, {0, -1}, {-1, -2}, {1, -7}}
	want := []CellCoord{{-1, -2}, {-1, 5}, {0, -1}, {0, 0}, {1, -7}, {1, 0}}
	SortCellCoords(in)
	for i := range want {
		if in[i] != want[i] {
			t.Fatalf("sorted = %v, want %v", in, want)
		}
	}
	if CompareCellCoords(CellCoord{0, 0}, CellCoord{0, 0}) != 0 {
		t.Fatal("equal coords must compare 0")
	}
	if CompareCellCoords(CellCoord{2, 9}, CellCoord{3, -9}) >= 0 {
		t.Fatal("X ordering violated")
	}
	if CompareCellCoords(CellCoord{2, 1}, CellCoord{2, 2}) >= 0 {
		t.Fatal("Z ordering violated")
	}
}
