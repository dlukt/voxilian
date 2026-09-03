package store

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dlukt/voxilian/internal/store/gen"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func itemSnapFixture(t *testing.T, pool *pgxpool.Pool, q *gen.Queries) (charID, corpseID int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO item_protos (id,kind,slot,base,version) VALUES (900,0,NULL,'{}',1) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	acct, err := q.CreateAccount(ctx, gen.CreateAccountParams{KeycloakSub: "sub-itemsnap"})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := createCharacter(ctx, q, validCharParams(acct.ID, 0, "Itemholder"))
	if err != nil {
		t.Fatal(err)
	}
	corpse, err := q.InsertCorpse(ctx, gen.InsertCorpseParams{
		CharacterID: ch.ID, ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ch.ID, corpse.ID
}

func groundSnap(id int64, rev int64) ItemSnapshot {
	x, y := int64(5_000_000_000), int64(-5_000_000_000)
	return ItemSnapshot{ID: id, ExpectedRevision: rev,
		Qty: 3, Hits: 100, Enchants: json.RawMessage(`{"g":1}`),
		Location: ItemLocationSnapshot{Kind: 1, PosX: &x, PosY: &y, PosZ: new(int64)}}
}

func readItem(t *testing.T, q *gen.Queries, id int64) gen.ItemInstance {
	t.Helper()
	it, err := q.GetItemInstanceByID(context.Background(), id)
	if err != nil {
		t.Fatalf("read item: %v", err)
	}
	return it
}

func readLoc(t *testing.T, q *gen.Queries, id int64) gen.ItemLocation {
	t.Helper()
	loc, err := q.GetItemLocationByItemID(context.Background(), id)
	if err != nil {
		t.Fatalf("read location: %v", err)
	}
	return loc
}

func TestSaveItemSnapshotSuccess(t *testing.T) {
	pool, q := openQueries(t)
	st := newTestStore(t, pool)
	ctx := context.Background()
	charID, _ := itemSnapFixture(t, pool, q)
	root, _, err := createItemWithLocation(ctx, pool,
		gen.InsertItemInstanceParams{Proto: 900, Qty: 1, Hits: 1, Enchants: []byte("{}")},
		NewItemLocation{Kind: 1, PosX: int8(0), PosY: int8(0), PosZ: int8(0)},
	)
	if err != nil {
		t.Fatal(err)
	}

	rev, err := st.SaveItemSnapshot(ctx, groundSnap(root.ID, 0))
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if rev != 1 {
		t.Fatalf("rev = %d, want 1", rev)
	}
	got := readItem(t, q, root.ID)
	if got.Revision != 1 || got.Qty != 3 || got.Hits != 100 || string(got.Enchants) == "" {
		t.Fatalf("root = %+v", got)
	}
	if got.Proto != 900 || got.ID != root.ID || !got.CreatedAt.Valid {
		t.Fatalf("identity drifted: %+v", got)
	}
	loc := readLoc(t, q, root.ID)
	if loc.Kind != 1 || !loc.PosX.Valid || loc.PosX.Int64 != 5_000_000_000 {
		t.Fatalf("location = %+v", loc)
	}
	_ = charID
}

func TestSaveItemLocationReplacement(t *testing.T) {
	pool, q := openQueries(t)
	st := newTestStore(t, pool)
	ctx := context.Background()
	charID, _ := itemSnapFixture(t, pool, q)
	root, _, err := createItemWithLocation(ctx, pool,
		gen.InsertItemInstanceParams{Proto: 900, Qty: 1, Hits: 1, Enchants: []byte("{}")},
		NewItemLocation{Kind: 1, PosX: int8(1), PosY: int8(2), PosZ: int8(3)},
	)
	if err != nil {
		t.Fatal(err)
	}

	// ground → inventory: old coords must NULL out.
	slot := "mainhand"
	snap := groundSnap(root.ID, 0)
	snap.Location = ItemLocationSnapshot{Kind: 0, CharacterID: &charID, Slot: &slot}
	if _, err := st.SaveItemSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	loc := readLoc(t, q, root.ID)
	if loc.Kind != 0 || !loc.CharacterID.Valid || loc.PosX.Valid || loc.PosY.Valid || loc.PosZ.Valid {
		t.Fatalf("replaced = %+v", loc)
	}

	// inventory → vault.
	region := "barloque"
	snap.ExpectedRevision = 1
	snap.Location = ItemLocationSnapshot{Kind: 3, CharacterID: &charID, VaultRegion: &region, Slot: &slot}
	if _, err := st.SaveItemSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	loc = readLoc(t, q, root.ID)
	if loc.Kind != 3 || !loc.VaultRegion.Valid || loc.VaultRegion.String != "barloque" {
		t.Fatalf("vault = %+v", loc)
	}

	// vault → container.
	bag, _, err := createItemWithLocation(ctx, pool,
		gen.InsertItemInstanceParams{Proto: 900, Qty: 1, Hits: 1, Enchants: []byte("{}")},
		NewItemLocation{Kind: 0, CharacterID: int8(charID), Slot: text("back")},
	)
	if err != nil {
		t.Fatal(err)
	}
	snap.ExpectedRevision = 2
	pocket := "pocket"
	snap.Location = ItemLocationSnapshot{Kind: 4, ContainerItemID: &bag.ID, Slot: &pocket}
	if _, err := st.SaveItemSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	loc = readLoc(t, q, root.ID)
	if loc.Kind != 4 || !loc.ContainerItemID.Valid || loc.ContainerItemID.Int64 != bag.ID {
		t.Fatalf("container = %+v", loc)
	}
}

func TestSaveItemInvalidLocationRollback(t *testing.T) {
	pool, q := openQueries(t)
	st := newTestStore(t, pool)
	ctx := context.Background()
	_, _ = itemSnapFixture(t, pool, q)
	root, _, err := createItemWithLocation(ctx, pool,
		gen.InsertItemInstanceParams{Proto: 900, Qty: 7, Hits: 77, Enchants: []byte(`{"k":1}`)},
		NewItemLocation{Kind: 1, PosX: int8(1), PosY: int8(2), PosZ: int8(3)},
	)
	if err != nil {
		t.Fatal(err)
	}
	bad := groundSnap(root.ID, 0)
	bad.Qty = 99
	bad.Location = ItemLocationSnapshot{Kind: 0} // missing character+slot
	_, err = st.SaveItemSnapshot(ctx, bad)
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) || pgErr.ConstraintName != "item_locations_kind_state_check" {
		t.Fatalf("err = %v, want kind_state_check", err)
	}
	got := readItem(t, q, root.ID)
	if got.Revision != 0 || got.Qty != 7 {
		t.Fatalf("root not rolled back: %+v", got)
	}
	loc := readLoc(t, q, root.ID)
	if loc.Kind != 1 || !loc.PosX.Valid || loc.PosX.Int64 != 1 {
		t.Fatalf("location not rolled back: %+v", loc)
	}
}

func TestSaveItemFKRollback(t *testing.T) {
	pool, q := openQueries(t)
	st := newTestStore(t, pool)
	ctx := context.Background()
	_, _ = itemSnapFixture(t, pool, q)
	root, _, err := createItemWithLocation(ctx, pool,
		gen.InsertItemInstanceParams{Proto: 900, Qty: 1, Hits: 1, Enchants: []byte("{}")},
		NewItemLocation{Kind: 1, PosX: int8(1), PosY: int8(2), PosZ: int8(3)},
	)
	if err != nil {
		t.Fatal(err)
	}
	ghost := int64(999999)
	bad := groundSnap(root.ID, 0)
	bad.Location = ItemLocationSnapshot{Kind: 2, CorpseID: &ghost}
	_, err = st.SaveItemSnapshot(ctx, bad)
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) {
		t.Fatalf("err = %v, want preserved PgError", err)
	}
	if errors.Is(err, ErrContainerCycle) {
		t.Fatalf("FK failure mislabeled cycle: %v", err)
	}
	if got := readItem(t, q, root.ID); got.Revision != 0 {
		t.Fatalf("root moved: %+v", got)
	}
}

func TestSaveItemStale(t *testing.T) {
	pool, q := openQueries(t)
	st := newTestStore(t, pool)
	ctx := context.Background()
	_, _ = itemSnapFixture(t, pool, q)
	root, _, err := createItemWithLocation(ctx, pool,
		gen.InsertItemInstanceParams{Proto: 900, Qty: 1, Hits: 1, Enchants: []byte("{}")},
		NewItemLocation{Kind: 1, PosX: int8(1), PosY: int8(2), PosZ: int8(3)},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveItemSnapshot(ctx, groundSnap(root.ID, 0)); err != nil {
		t.Fatal(err)
	}
	stale := groundSnap(root.ID, 0)
	stale.Qty = 99
	nine := int64(9)
	stale.Location = ItemLocationSnapshot{Kind: 1, PosX: &nine, PosY: &nine, PosZ: &nine}
	if _, err := st.SaveItemSnapshot(ctx, stale); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("err = %v, want stale", err)
	}
	if got := readItem(t, q, root.ID); got.Revision != 1 || got.Qty != 3 {
		t.Fatalf("root touched: %+v", got)
	}
	if loc := readLoc(t, q, root.ID); loc.PosX.Int64 != 5_000_000_000 {
		t.Fatalf("location touched: %+v", loc)
	}
	stale.ExpectedRevision = 99
	if _, err := st.SaveItemSnapshot(ctx, stale); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("future err = %v, want stale", err)
	}
}

func TestSaveItemRace(t *testing.T) {
	pool, q := openQueries(t)
	st := newTestStore(t, pool)
	ctx := context.Background()
	_, _ = itemSnapFixture(t, pool, q)
	root, _, err := createItemWithLocation(ctx, pool,
		gen.InsertItemInstanceParams{Proto: 900, Qty: 1, Hits: 1, Enchants: []byte("{}")},
		NewItemLocation{Kind: 1, PosX: int8(1), PosY: int8(2), PosZ: int8(3)},
	)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(qty int32, x int64) ItemSnapshot {
		s := groundSnap(root.ID, 0)
		s.Qty, s.Hits = qty, qty
		s.Location = ItemLocationSnapshot{Kind: 1, PosX: &x, PosY: &x, PosZ: &x}
		return s
	}
	type res struct {
		rev int64
		err error
	}
	ch := make(chan res, 2)
	var wg sync.WaitGroup
	for _, s := range []ItemSnapshot{mk(11, 11), mk(22, 22)} {
		wg.Add(1)
		go func(s ItemSnapshot) {
			defer wg.Done()
			rev, err := st.SaveItemSnapshot(ctx, s)
			ch <- res{rev, err}
		}(s)
	}
	wg.Wait()
	close(ch)
	var wins, stalem int
	for r := range ch {
		if r.err == nil {
			wins++
			if r.rev != 1 {
				t.Fatalf("winner rev = %d", r.rev)
			}
		} else if errors.Is(r.err, ErrStaleRevision) {
			stalem++
		} else {
			t.Fatalf("unexpected: %v", r.err)
		}
	}
	if wins != 1 || stalem != 1 {
		t.Fatalf("wins=%d stale=%d", wins, stalem)
	}
	got := readItem(t, q, root.ID)
	loc := readLoc(t, q, root.ID)
	if got.Revision != 1 || (got.Qty != 11 && got.Qty != 22) {
		t.Fatalf("root = %+v", got)
	}
	if (got.Qty == 11) != (loc.PosX.Int64 == 11) {
		t.Fatalf("mixed root/location: %+v %+v", got, loc)
	}
}

func TestItemSelfContainer(t *testing.T) {
	pool, q := openQueries(t)
	st := newTestStore(t, pool)
	ctx := context.Background()
	_, _ = itemSnapFixture(t, pool, q)
	root, _, err := createItemWithLocation(ctx, pool,
		gen.InsertItemInstanceParams{Proto: 900, Qty: 1, Hits: 1, Enchants: []byte("{}")},
		NewItemLocation{Kind: 1, PosX: int8(1), PosY: int8(2), PosZ: int8(3)},
	)
	if err != nil {
		t.Fatal(err)
	}
	slot := "pocket"
	self := groundSnap(root.ID, 0)
	self.Location = ItemLocationSnapshot{Kind: 4, ContainerItemID: &root.ID, Slot: &slot}
	if _, err := st.SaveItemSnapshot(ctx, self); !errors.Is(err, ErrContainerCycle) {
		t.Fatalf("err = %v, want cycle", err)
	}
	if got := readItem(t, q, root.ID); got.Revision != 0 {
		t.Fatalf("root moved: %+v", got)
	}
}

func TestItemDeepCycles(t *testing.T) {
	pool, q := openQueries(t)
	st := newTestStore(t, pool)
	ctx := context.Background()
	_, _ = itemSnapFixture(t, pool, q)
	mkground := func() int64 {
		r, _, err := createItemWithLocation(ctx, pool,
			gen.InsertItemInstanceParams{Proto: 900, Qty: 1, Hits: 1, Enchants: []byte("{}")},
			NewItemLocation{Kind: 1, PosX: int8(1), PosY: int8(2), PosZ: int8(3)})
		if err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	putIn := func(id, rev int64, cont int64) {
		t.Helper()
		slot := "pocket"
		s := groundSnap(id, rev)
		s.Location = ItemLocationSnapshot{Kind: 4, ContainerItemID: &cont, Slot: &slot}
		if _, err := st.SaveItemSnapshot(ctx, s); err != nil {
			t.Fatalf("setup place %d→%d: %v", id, cont, err)
		}
	}
	a, b, c := mkground(), mkground(), mkground()
	putIn(b, 0, a) // B → A

	// A → B must fail (two-node cycle).
	slot := "pocket"
	ab := groundSnap(a, 0)
	ab.Location = ItemLocationSnapshot{Kind: 4, ContainerItemID: &b, Slot: &slot}
	if _, err := st.SaveItemSnapshot(ctx, ab); !errors.Is(err, ErrContainerCycle) {
		t.Fatalf("A→B err = %v, want cycle", err)
	}
	if got := readItem(t, q, a); got.Revision != 0 {
		t.Fatalf("A moved: %+v", got)
	}

	// C → B → A chain; A → C must fail (three-node cycle).
	putIn(c, 0, b)
	ac := groundSnap(a, 0)
	ac.Location = ItemLocationSnapshot{Kind: 4, ContainerItemID: &c, Slot: &slot}
	if _, err := st.SaveItemSnapshot(ctx, ac); !errors.Is(err, ErrContainerCycle) {
		t.Fatalf("A→C err = %v, want cycle", err)
	}
}

func TestItemCorruptAncestryTerminates(t *testing.T) {
	pool, q := openQueries(t)
	st := newTestStore(t, pool)
	ctx := context.Background()
	_, _ = itemSnapFixture(t, pool, q)
	mkground := func() int64 {
		r, _, err := createItemWithLocation(ctx, pool,
			gen.InsertItemInstanceParams{Proto: 900, Qty: 1, Hits: 1, Enchants: []byte("{}")},
			NewItemLocation{Kind: 1, PosX: int8(1), PosY: int8(2), PosZ: int8(3)})
		if err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	x, y, z := mkground(), mkground(), mkground()
	// Raw-SQL corrupt cycle X ↔ Y (bypasses Store on purpose).
	if _, err := pool.Exec(ctx, `UPDATE item_locations SET kind=4, container_item_id=$2, slot='s', pos_x=NULL, pos_y=NULL, pos_z=NULL WHERE item_id=$1`, x, y); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE item_locations SET kind=4, container_item_id=$2, slot='s', pos_x=NULL, pos_y=NULL, pos_z=NULL WHERE item_id=$1`, y, x); err != nil {
		t.Fatal(err)
	}
	// Placing Z into the corrupt ancestry must terminate with cycle.
	slot := "pocket"
	zs := groundSnap(z, 0)
	zs.Location = ItemLocationSnapshot{Kind: 4, ContainerItemID: &y, Slot: &slot}
	done := make(chan error, 1)
	go func() {
		_, err := st.SaveItemSnapshot(ctx, zs)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrContainerCycle) {
			t.Fatalf("err = %v, want cycle", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("cycle query did not terminate")
	}
	if got := readItem(t, q, z); got.Revision != 0 {
		t.Fatalf("Z moved: %+v", got)
	}
}

func TestItemReverseCycleRace(t *testing.T) {
	pool, q := openQueries(t)
	st := newTestStore(t, pool)
	ctx := context.Background()
	_, _ = itemSnapFixture(t, pool, q)
	mkground := func() int64 {
		r, _, err := createItemWithLocation(ctx, pool,
			gen.InsertItemInstanceParams{Proto: 900, Qty: 1, Hits: 1, Enchants: []byte("{}")},
			NewItemLocation{Kind: 1, PosX: int8(1), PosY: int8(2), PosZ: int8(3)})
		if err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	a, b := mkground(), mkground()
	mkMove := func(id, cont int64) ItemSnapshot {
		slot := "pocket"
		s := groundSnap(id, 0)
		s.Location = ItemLocationSnapshot{Kind: 4, ContainerItemID: &cont, Slot: &slot}
		return s
	}
	type res struct {
		id  int64
		err error
	}
	ch := make(chan res, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := st.SaveItemSnapshot(ctx, mkMove(a, b))
		ch <- res{a, err}
	}()
	go func() {
		defer wg.Done()
		_, err := st.SaveItemSnapshot(ctx, mkMove(b, a))
		ch <- res{b, err}
	}()
	wg.Wait()
	close(ch)
	var wins, cycles int
	var winner int64
	for r := range ch {
		if r.err == nil {
			wins++
			winner = r.id
		} else if errors.Is(r.err, ErrContainerCycle) {
			cycles++
		} else {
			t.Fatalf("unexpected err for %d: %v", r.id, r.err)
		}
	}
	if wins != 1 || cycles != 1 {
		t.Fatalf("wins=%d cycles=%d, want 1/1 (roots distinct: no stale expected)", wins, cycles)
	}
	wa, wb := readItem(t, q, a), readItem(t, q, b)
	if winner == a && (wa.Revision != 1 || wb.Revision != 0) {
		t.Fatalf("revs A=%d B=%d", wa.Revision, wb.Revision)
	}
	if winner == b && (wb.Revision != 1 || wa.Revision != 0) {
		t.Fatalf("revs A=%d B=%d", wa.Revision, wb.Revision)
	}
	// Exactly one edge, acyclic ancestry.
	la, lb := readLoc(t, q, a), readLoc(t, q, b)
	edges := 0
	if la.Kind == 4 {
		edges++
	}
	if lb.Kind == 4 {
		edges++
	}
	if edges != 1 {
		t.Fatalf("edges=%d, want 1", edges)
	}
}
