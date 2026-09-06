package gateway

import (
	"errors"
	"math"

	"github.com/dlukt/voxilian/internal/proto"
	"github.com/dlukt/voxilian/internal/world"
)

// ErrWirePositionRange marks an authoritative world position that
// cannot be represented on the wire: any NaN/±Inf component, or a
// rounded millimeter coordinate outside int32 (spec §7.4.2). The
// affected session must fail closed/resync; authoritative state is
// never clamped to fit. Match with errors.Is.
var ErrWirePositionRange = errors.New("gateway: wire position out of range")

// WirePosition converts authoritative float64 meters to wire int32
// millimeters (spec §7.4.2): wireMM = math.Round(meters * 1000) per
// axis, independently. This is the single frozen conversion; no
// second helper exists elsewhere.
func WirePosition(p world.Vec3) (proto.Position, error) {
	x, err := wireMillimeters(p.X)
	if err != nil {
		return proto.Position{}, err
	}
	y, err := wireMillimeters(p.Y)
	if err != nil {
		return proto.Position{}, err
	}
	z, err := wireMillimeters(p.Z)
	if err != nil {
		return proto.Position{}, err
	}
	return proto.Position{X: x, Y: y, Z: z}, nil
}

func wireMillimeters(meters float64) (int32, error) {
	if math.IsNaN(meters) || math.IsInf(meters, 0) {
		return 0, ErrWirePositionRange
	}
	mm := math.Round(meters * 1000)
	if mm < math.MinInt32 || mm > math.MaxInt32 {
		return 0, ErrWirePositionRange
	}
	return int32(mm), nil
}
