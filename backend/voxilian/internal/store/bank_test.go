package store

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/dlukt/voxilian/internal/store/gen"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestSaveBankBalance(t *testing.T) {
	pool, q := openQueries(t)
	ctx := context.Background()
	st := newTestStore(t, pool)
	acct, err := q.CreateAccount(ctx, gen.CreateAccountParams{KeycloakSub: "sub-bankcas"})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := createCharacter(ctx, q, validCharParams(acct.ID, 0, "Banker"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.InsertBank(ctx, gen.InsertBankParams{CharacterID: ch.ID, System: "tos", Balance: 100}); err != nil {
		t.Fatal(err)
	}

	rev, err := st.SaveBankBalance(ctx, BankSnapshot{CharacterID: ch.ID, System: "tos", ExpectedRevision: 0, Balance: 5_000_000_000})
	if err != nil || rev != 1 {
		t.Fatalf("save rev = %d, %v", rev, err)
	}
	got, err := q.GetBank(ctx, gen.GetBankParams{CharacterID: ch.ID, System: "tos"})
	if err != nil || got.Balance != 5_000_000_000 || got.Revision != 1 || got.CharacterID != ch.ID || got.System != "tos" {
		t.Fatalf("bank = %+v, %v", got, err)
	}

	// Negative gameplay-agnostic value round-trips.
	if _, err := st.SaveBankBalance(ctx, BankSnapshot{CharacterID: ch.ID, System: "tos", ExpectedRevision: 1, Balance: -250}); err != nil {
		t.Fatalf("negative: %v", err)
	}

	// Stale + future + missing all surface ErrStaleRevision, no mutation.
	for _, tc := range []struct {
		name string
		snap BankSnapshot
	}{
		{"stale", BankSnapshot{CharacterID: ch.ID, System: "tos", ExpectedRevision: 1, Balance: 1}},
		{"future", BankSnapshot{CharacterID: ch.ID, System: "tos", ExpectedRevision: 99, Balance: 1}},
		{"missing", BankSnapshot{CharacterID: ch.ID, System: "nope", ExpectedRevision: 0, Balance: 1}},
	} {
		if _, err := st.SaveBankBalance(ctx, tc.snap); !errors.Is(err, ErrStaleRevision) {
			t.Fatalf("%s err = %v, want stale", tc.name, err)
		}
	}
	got, _ = q.GetBank(ctx, gen.GetBankParams{CharacterID: ch.ID, System: "tos"})
	if got.Balance != -250 || got.Revision != 2 {
		t.Fatalf("mutated by stale: %+v", got)
	}
}

func TestBankCompositeIsolation(t *testing.T) {
	pool, q := openQueries(t)
	ctx := context.Background()
	st := newTestStore(t, pool)
	acct, _ := q.CreateAccount(ctx, gen.CreateAccountParams{KeycloakSub: "sub-bankiso"})
	c1, _ := createCharacter(ctx, q, validCharParams(acct.ID, 0, "Iso"))
	c2, _ := createCharacter(ctx, q, validCharParams(acct.ID, 1, "Iso2"))
	for _, b := range []gen.InsertBankParams{
		{CharacterID: c1.ID, System: "gold", Balance: 1},
		{CharacterID: c1.ID, System: "guild", Balance: 2},
		{CharacterID: c2.ID, System: "gold", Balance: 3},
	} {
		if _, err := q.InsertBank(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.SaveBankBalance(ctx, BankSnapshot{CharacterID: c1.ID, System: "gold", ExpectedRevision: 0, Balance: 100}); err != nil {
		t.Fatal(err)
	}
	gold, _ := q.GetBank(ctx, gen.GetBankParams{CharacterID: c1.ID, System: "gold"})
	guild, _ := q.GetBank(ctx, gen.GetBankParams{CharacterID: c1.ID, System: "guild"})
	other, _ := q.GetBank(ctx, gen.GetBankParams{CharacterID: c2.ID, System: "gold"})
	if gold.Balance != 100 || gold.Revision != 1 || guild.Balance != 2 || guild.Revision != 0 || other.Balance != 3 {
		t.Fatalf("isolation broken: %+v %+v %+v", gold, guild, other)
	}
}

func TestBankConcurrentRace(t *testing.T) {
	pool, q := openQueries(t)
	ctx := context.Background()
	st := newTestStore(t, pool)
	acct, _ := q.CreateAccount(ctx, gen.CreateAccountParams{KeycloakSub: "sub-bankrace"})
	ch, _ := createCharacter(ctx, q, validCharParams(acct.ID, 0, "Racer"))
	if _, err := q.InsertBank(ctx, gen.InsertBankParams{CharacterID: ch.ID, System: "tos", Balance: 0}); err != nil {
		t.Fatal(err)
	}
	type res struct {
		rev int64
		err error
	}
	ch2 := make(chan res, 2)
	var wg sync.WaitGroup
	for _, bal := range []int64{100, 200} {
		wg.Add(1)
		go func(bal int64) {
			defer wg.Done()
			rev, err := st.SaveBankBalance(ctx, BankSnapshot{CharacterID: ch.ID, System: "tos", ExpectedRevision: 0, Balance: bal})
			ch2 <- res{rev, err}
		}(bal)
	}
	wg.Wait()
	close(ch2)
	var wins, stalem int
	for r := range ch2 {
		if r.err == nil {
			wins++
			if r.rev != 1 {
				t.Fatalf("rev = %d", r.rev)
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
	got, _ := q.GetBank(ctx, gen.GetBankParams{CharacterID: ch.ID, System: "tos"})
	if got.Revision != 1 || (got.Balance != 100 && got.Balance != 200) {
		t.Fatalf("bank = %+v", got)
	}

	// Independent roots proceed concurrently without interference.
	if _, err := q.InsertBank(ctx, gen.InsertBankParams{CharacterID: ch.ID, System: "koc", Balance: 0}); err != nil {
		t.Fatal(err)
	}
	var wg2 sync.WaitGroup
	errs := make(chan error, 2)
	for _, s := range []struct {
		sys string
		bal int64
	}{{"tos", 1000}, {"koc", 2000}} {
		// tos is at rev 1 now; read it fresh for a valid expectation.
		wg2.Add(1)
		go func(sys string, bal int64) {
			defer wg2.Done()
			g, err := q.GetBank(ctx, gen.GetBankParams{CharacterID: ch.ID, System: sys})
			if err != nil {
				errs <- err
				return
			}
			_, err = st.SaveBankBalance(ctx, BankSnapshot{CharacterID: ch.ID, System: sys, ExpectedRevision: g.Revision, Balance: bal})
			errs <- err
		}(s.sys, s.bal)
	}
	wg2.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("independent bank failed: %v", err)
		}
	}
}

func TestStaleMetric(t *testing.T) {
	pool, q := openQueries(t)
	ctx := context.Background()
	reg := prometheus.NewRegistry()
	st, err := New(pool, reg)
	if err != nil {
		t.Fatal(err)
	}
	count := func(agg string) float64 {
		return testutil.ToFloat64(st.stale.WithLabelValues(agg))
	}

	// Fixtures: account+char, item, bank.
	acct, _ := q.CreateAccount(ctx, gen.CreateAccountParams{KeycloakSub: "sub-metric"})
	ch, _ := createCharacter(ctx, q, validCharParams(acct.ID, 0, "Meter"))
	if _, err := pool.Exec(ctx, `INSERT INTO item_protos (id,kind,slot,base,version) VALUES (950,0,NULL,'{}',1) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	item, _, err := createItemWithLocation(ctx, pool,
		gen.InsertItemInstanceParams{Proto: 950, Qty: 1, Hits: 1, Enchants: []byte("{}")},
		NewItemLocation{Kind: 1, PosX: int8(1), PosY: int8(2), PosZ: int8(3)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.InsertBank(ctx, gen.InsertBankParams{CharacterID: ch.ID, System: "tos", Balance: 0}); err != nil {
		t.Fatal(err)
	}

	// One stale per aggregate: character save, item save, bank save.
	chsnap := CharacterSnapshot{ID: ch.ID, ExpectedRevision: 99, Vitals: []byte("{}"), Advancement: []byte("{}")}
	if _, err := st.SaveCharacterSnapshot(ctx, chsnap); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("char stale: %v", err)
	}
	x := int64(1)
	isnap := ItemSnapshot{ID: item.ID, ExpectedRevision: 99, Qty: 1, Hits: 1,
		Enchants: []byte("{}"),
		Location: ItemLocationSnapshot{Kind: 1, PosX: &x, PosY: &x, PosZ: &x}}
	if _, err := st.SaveItemSnapshot(ctx, isnap); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("item stale: %v", err)
	}
	if _, err := st.SaveBankBalance(ctx, BankSnapshot{CharacterID: ch.ID, System: "tos", ExpectedRevision: 99}); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("bank stale: %v", err)
	}
	if got := count("character"); got != 1 {
		t.Fatalf("character = %v, want 1", got)
	}
	if got := count("item"); got != 1 {
		t.Fatalf("item = %v, want 1", got)
	}
	if got := count("bank"); got != 1 {
		t.Fatalf("bank = %v, want 1", got)
	}

	// Stale soft-delete counts as character too.
	if _, err := st.SoftDeleteCharacter(ctx, ch.ID, 99); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("delete stale: %v", err)
	}
	if got := count("character"); got != 2 {
		t.Fatalf("character = %v, want 2", got)
	}

	// Non-stale item failure (cycle) must NOT increment.
	self := isnap
	self.ExpectedRevision = 0
	slot := "pocket"
	self.Location = ItemLocationSnapshot{Kind: 4, ContainerItemID: &item.ID, Slot: &slot}
	if _, err := st.SaveItemSnapshot(ctx, self); !errors.Is(err, ErrContainerCycle) {
		t.Fatalf("cycle: %v", err)
	}
	if got := count("item"); got != 1 {
		t.Fatalf("item = %v, want still 1", got)
	}
}
