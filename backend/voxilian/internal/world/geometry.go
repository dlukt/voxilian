// Package world defines the server canonical geometry API: the
// authoritative float64-meter vector, the 2D XZ cell grid, and the
// shared constants (spec §§4, 5.2.4, frozen v0.3.14).
//
// M4-T1 establishes this geometry API only. It performs no world
// loading, no world.toml parsing, and no gameplay behavior.
package world

import (
	"errors"
	"math"
	"slices"
)

// Canonical server geometry constants (spec §5.2.4).
const (
	// ChunkEdgeVoxels is the voxel chunk edge length.
	ChunkEdgeVoxels = 16
	// CellSizeMeters is the uniform XZ grid cell edge in meters.
	CellSizeMeters = 32
	// DefaultAOIRadiusMeters is the default AOI radius in meters.
	DefaultAOIRadiusMeters = 96
)

// Vec3 is the server authoritative world position in float64 meters
// (spec §5.2.4). It MUST NOT be confused with proto.Position, which is
// fixed-point wire millimeters; protocol conversion belongs to later
// gateway/sim integration.
type Vec3 struct {
	X float64
	Y float64
	Z float64
}

// CellCoord is a 2D XZ cell coordinate (spec §5.2.4). Y never selects
// the cell; this matches the wire cell shape (i32 cx + i32 cz).
type CellCoord struct {
	X int32
	Z int32
}

// ErrInvalidPosition marks a rejected simulation position: any NaN/Inf
// component, or an X/Z whose floored cell cannot fit int32. Match with
// errors.Is; callers MUST NOT parse strings.
var ErrInvalidPosition = errors.New("world: invalid position")

// int32 bounds as float64 for cell-fit checks.
const (
	maxInt32Float = float64(math.MaxInt32)
	minInt32Float = float64(math.MinInt32)
)

// ValidatePosition reports whether p is a legal simulation position:
// every component finite, and the floored X/Z cells fit int32.
func ValidatePosition(p Vec3) error {
	if math.IsNaN(p.X) || math.IsNaN(p.Y) || math.IsNaN(p.Z) {
		return ErrInvalidPosition
	}
	if math.IsInf(p.X, 0) || math.IsInf(p.Y, 0) || math.IsInf(p.Z, 0) {
		return ErrInvalidPosition
	}
	fx := math.Floor(p.X / float64(CellSizeMeters))
	fz := math.Floor(p.Z / float64(CellSizeMeters))
	if fx < minInt32Float || fx > maxInt32Float {
		return ErrInvalidPosition
	}
	if fz < minInt32Float || fz > maxInt32Float {
		return ErrInvalidPosition
	}
	return nil
}

// CellForPosition maps a validated position to its XZ cell using
// mathematical floor (spec §5.2.4). Negative-space correctness is
// mandatory: -0.001 -> -1, -32.0 -> -1, -32.001 -> -2.
func CellForPosition(p Vec3) (CellCoord, error) {
	if err := ValidatePosition(p); err != nil {
		return CellCoord{}, err
	}
	fx := math.Floor(p.X / float64(CellSizeMeters))
	fz := math.Floor(p.Z / float64(CellSizeMeters))
	return CellCoord{X: int32(fx), Z: int32(fz)}, nil
}

// CompareCellCoords orders cells X ascending, then Z ascending. This is
// the canonical deterministic cell iteration order (spec §5.2.4): any
// engine operation that can affect simulation state MUST iterate cells
// in this order and MUST NEVER depend on Go map iteration order.
func CompareCellCoords(a, b CellCoord) int {
	if a.X != b.X {
		if a.X < b.X {
			return -1
		}
		return 1
	}
	if a.Z != b.Z {
		if a.Z < b.Z {
			return -1
		}
		return 1
	}
	return 0
}

// SortCellCoords sorts coords in canonical deterministic order
// (X ascending, then Z ascending) in place.
func SortCellCoords(coords []CellCoord) {
	slices.SortFunc(coords, CompareCellCoords)
}
