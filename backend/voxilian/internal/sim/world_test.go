package sim

import (
	"errors"
	"math"
	"testing"

	"github.com/dlukt/voxilian/internal/world"
)

func TestAddEntityFirstID(t *testing.T) {
	r := newRegistry(40)
	snap, err := r.AddEntity(world.Vec3{X: 1, Y: 2, Z: 3})
	if err != nil {
		t.Fatalf("AddEntity error: %v", err)
	}
	if snap.ID != 1 {
		t.Fatalf("first ID = %d, want 1", snap.ID)
	}
	if snap.Cell != (world.CellCoord{X: 0, Z: 0}) {
		t.Fatalf("cell = %v, want {0 0}", snap.Cell)
	}
	if snap.Position != (world.Vec3{X: 1, Y: 2, Z: 3}) {
		t.Fatalf("position = %v", snap.Position)
	}
	if r.EntityCount() != 1 || r.CellCount() != 1 {
		t.Fatalf("counts = %d/%d, want 1/1", r.EntityCount(), r.CellCount())
	}
	got, err := r.Entity(snap.ID)
	if err != nil {
		t.Fatalf("Entity error: %v", err)
	}
	if got != snap {
		t.Fatalf("lookup = %+v, want %+v", got, snap)
	}
	h, err := r.History(snap.ID)
	if err != nil {
		t.Fatalf("History error: %v", err)
	}
	if len(h) != 0 {
		t.Fatalf("new entity history len = %d, want 0", len(h))
	}
	// Locator and cell membership agree.
	coords := r.CellCoords()
	if len(coords) != 1 || coords[0] != snap.Cell {
		t.Fatalf("CellCoords = %v, want [%v]", coords, snap.Cell)
	}
	inCell := r.EntitiesInCell(snap.Cell)
	if len(inCell) != 1 || inCell[0] != snap {
		t.Fatalf("EntitiesInCell = %+v, want [%+v]", inCell, snap)
	}
}

func TestAddEntityNegativeCells(t *testing.T) {
	r := newRegistry(40)
	a, err := r.AddEntity(world.Vec3{X: -0.001, Z: -32.001})
	if err != nil {
		t.Fatalf("AddEntity error: %v", err)
	}
	if a.Cell != (world.CellCoord{X: -1, Z: -2}) {
		t.Fatalf("cell = %v, want {-1 -2}", a.Cell)
	}
	b, err := r.AddEntity(world.Vec3{X: -32, Z: 0})
	if err != nil {
		t.Fatalf("AddEntity error: %v", err)
	}
	if b.Cell != (world.CellCoord{X: -1, Z: 0}) {
		t.Fatalf("cell = %v, want {-1 0}", b.Cell)
	}
	if a.ID != 1 || b.ID != 2 {
		t.Fatalf("IDs = %d,%d, want 1,2", a.ID, b.ID)
	}
}

func TestAddEntityInvalidAtomic(t *testing.T) {
	r := newRegistry(40)
	if _, err := r.AddEntity(world.Vec3{X: 5}); err != nil {
		t.Fatalf("setup add error: %v", err)
	}
	invalids := []world.Vec3{
		{X: math.NaN()},
		{Y: math.Inf(1)},
		{Z: math.Inf(-1)},
		{X: float64(int64(1) << 36)}, // cell 2^31: overflow
	}
	for i, p := range invalids {
		if _, err := r.AddEntity(p); !errors.Is(err, ErrInvalidPosition) {
			t.Fatalf("case %d: AddEntity = %v, want ErrInvalidPosition", i, err)
		}
	}
	if r.EntityCount() != 1 {
		t.Fatalf("entity count = %d, want 1 (no partial add)", r.EntityCount())
	}
	if r.CellCount() != 1 {
		t.Fatalf("cell count = %d, want 1 (no partial cell)", r.CellCount())
	}
	// The failed adds consumed NO IDs: the next valid add is 2.
	snap, err := r.AddEntity(world.Vec3{X: 6})
	if err != nil {
		t.Fatalf("AddEntity error: %v", err)
	}
	if snap.ID != 2 {
		t.Fatalf("next ID = %d, want 2", snap.ID)
	}
}

func TestRemoveEntity(t *testing.T) {
	r := newRegistry(40)
	a, _ := r.AddEntity(world.Vec3{X: 1})
	b, _ := r.AddEntity(world.Vec3{X: 2})
	if err := r.RemoveEntity(a.ID); err != nil {
		t.Fatalf("RemoveEntity error: %v", err)
	}
	if _, err := r.Entity(a.ID); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("Entity(removed) = %v, want ErrEntityNotFound", err)
	}
	if _, err := r.History(a.ID); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("History(removed) = %v, want ErrEntityNotFound", err)
	}
	if len(r.EntitiesInCell(a.Cell)) != 1 {
		t.Fatalf("cell still holds removed entity: %+v", r.EntitiesInCell(a.Cell))
	}
	// b shares cell {0 0}: cell survives with one member.
	if r.CellCount() != 1 {
		t.Fatalf("cell count = %d, want 1", r.CellCount())
	}
	// Remove b: cell becomes empty and is removed (T1 ownership cells).
	if err := r.RemoveEntity(b.ID); err != nil {
		t.Fatalf("RemoveEntity error: %v", err)
	}
	if r.CellCount() != 0 || r.EntityCount() != 0 {
		t.Fatalf("counts = %d/%d, want 0/0", r.EntityCount(), r.CellCount())
	}
	// Second remove is stable not-found.
	if err := r.RemoveEntity(b.ID); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("second Remove = %v, want ErrEntityNotFound", err)
	}
	// Unknown ID likewise.
	if err := r.RemoveEntity(999); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("Remove(unknown) = %v, want ErrEntityNotFound", err)
	}
	// IDs are never reused: next add is 3.
	c, err := r.AddEntity(world.Vec3{X: 3})
	if err != nil {
		t.Fatalf("AddEntity error: %v", err)
	}
	if c.ID != 3 {
		t.Fatalf("ID after removes = %d, want 3 (no reuse)", c.ID)
	}
	if h, _ := r.History(c.ID); len(h) != 0 {
		t.Fatalf("recreated history len = %d, want 0", len(h))
	}
}

func TestSetPositionSameCell(t *testing.T) {
	r := newRegistry(40)
	snap, _ := r.AddEntity(world.Vec3{X: 31})
	if err := r.SetPosition(snap.ID, world.Vec3{X: 31.5}); err != nil {
		t.Fatalf("SetPosition same cell error: %v", err)
	}
	got, _ := r.Entity(snap.ID)
	if got.Position.X != 31.5 || got.Cell != snap.Cell {
		t.Fatalf("after SetPosition = %+v, want X=31.5 same cell", got)
	}
}

func TestSetPositionCrossCellMatrix(t *testing.T) {
	cases := []struct {
		name    string
		start   world.Vec3
		dest    world.Vec3
		same    bool
		wantErr error
	}{
		{"31->31.5 same", world.Vec3{X: 31}, world.Vec3{X: 31.5}, true, nil},
		{"31->32 handoff", world.Vec3{X: 31}, world.Vec3{X: 32}, false, ErrCellHandoffRequired},
		{"0.1->-0.1 handoff", world.Vec3{X: 0.1}, world.Vec3{X: -0.1}, false, ErrCellHandoffRequired},
		{"-31->-32 same negative", world.Vec3{X: -31}, world.Vec3{X: -32}, true, nil},
		{"-32->-32.1 handoff", world.Vec3{X: -32}, world.Vec3{X: -32.1}, false, ErrCellHandoffRequired},
	}
	for _, tc := range cases {
		r := newRegistry(40)
		snap, err := r.AddEntity(tc.start)
		if err != nil {
			t.Fatalf("%s: setup add error: %v", tc.name, err)
		}
		err = r.SetPosition(snap.ID, tc.dest)
		if tc.same {
			if err != nil {
				t.Fatalf("%s: SetPosition error: %v", tc.name, err)
			}
			continue
		}
		if !errors.Is(err, tc.wantErr) {
			t.Fatalf("%s: SetPosition = %v, want %v", tc.name, err, tc.wantErr)
		}
		// Zero mutation: exact old position and membership intact.
		got, lerr := r.Entity(snap.ID)
		if lerr != nil {
			t.Fatalf("%s: lookup error: %v", tc.name, lerr)
		}
		if got.Position != tc.start || got.Cell != snap.Cell {
			t.Fatalf("%s: mutated to %+v, want unchanged %+v", tc.name, got, snap)
		}
		if len(r.EntitiesInCell(snap.Cell)) != 1 {
			t.Fatalf("%s: cell membership changed", tc.name)
		}
	}
}

func TestSetPositionInvalidAndUnknown(t *testing.T) {
	r := newRegistry(40)
	snap, _ := r.AddEntity(world.Vec3{X: 1})
	before, _ := r.Entity(snap.ID)
	if err := r.SetPosition(snap.ID, world.Vec3{X: math.NaN()}); !errors.Is(err, ErrInvalidPosition) {
		t.Fatalf("SetPosition(NaN) = %v, want ErrInvalidPosition", err)
	}
	after, _ := r.Entity(snap.ID)
	if after != before {
		t.Fatalf("invalid SetPosition mutated: %+v vs %+v", after, before)
	}
	if err := r.SetPosition(4242, world.Vec3{X: 1}); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("SetPosition(unknown) = %v, want ErrEntityNotFound", err)
	}
}

func TestDeterministicListings(t *testing.T) {
	r := newRegistry(40)
	// Insert in deliberately non-sorted order across cells.
	positions := []world.Vec3{
		{X: 100}, {X: -100}, {X: 0}, {X: 33}, {X: -33, Z: 40}, {X: 5, Z: -70},
	}
	var ids []EntityID
	for _, p := range positions {
		s, err := r.AddEntity(p)
		if err != nil {
			t.Fatalf("add error: %v", err)
		}
		ids = append(ids, s.ID)
	}
	coords := r.CellCoords()
	for i := 1; i < len(coords); i++ {
		if world.CompareCellCoords(coords[i-1], coords[i]) >= 0 {
			t.Fatalf("CellCoords not sorted: %v", coords)
		}
	}
	for _, c := range coords {
		list := r.EntitiesInCell(c)
		for i := 1; i < len(list); i++ {
			if list[i-1].ID >= list[i].ID {
				t.Fatalf("EntitiesInCell(%v) not ID-sorted: %v", c, list)
			}
		}
	}
	// Returned slices/copies cannot mutate the registry.
	coords[0] = world.CellCoord{X: 777, Z: 777}
	again := r.CellCoords()
	for _, c := range again {
		if c == (world.CellCoord{X: 777, Z: 777}) {
			t.Fatal("mutating CellCoords result altered registry")
		}
	}
	list := r.EntitiesInCell(again[0])
	if len(list) > 0 {
		list[0].Position.X = -123456
		fresh, _ := r.Entity(list[0].ID)
		if fresh.Position.X == -123456 {
			t.Fatal("mutating snapshot altered live state")
		}
	}
	if got := r.EntitiesInCell(world.CellCoord{X: 12345, Z: 12345}); len(got) != 0 {
		t.Fatalf("unknown cell listing = %v, want empty", got)
	}
	_ = ids
}
