package persist

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/dlukt/voxilian/internal/observe"
	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/simtest"
	"github.com/dlukt/voxilian/internal/store"
	"github.com/dlukt/voxilian/internal/store/gen"
)

// openPG migrates a fresh disposable PG18 database to the full
// current schema and returns a pool plus generated queries. Each
// test builds its own *store.PGStore on its own registry (Store
// rejects duplicate metric registration on a shared registry).
func openPG(t *testing.T) (*pgxpool.Pool, *gen.Queries) {
	t.Helper()
	pg := simtest.StartPostgres18(t)
	sqldb, err := sql.Open("pgx", pg.DSN)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sqldb.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(simtest.RepoRoot(t), "backend", "voxilian", "migrations")
	if err := goose.UpTo(sqldb, dir, 5); err != nil {
		t.Fatalf("migrate to 5: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), pg.DSN)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, gen.New(pool)
}

// ---- fixtures (test-only pgx/raw SQL; production persist never touches pgx) ----

func pgAccountChar(t *testing.T, q *gen.Queries, sub, name string) int64 {
	t.Helper()
	ctx := context.Background()
	acct, err := q.CreateAccount(ctx, gen.CreateAccountParams{KeycloakSub: sub})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	ch, err := q.InsertCharacter(ctx, gen.InsertCharacterParams{
		AccountID: acct.ID, Slot: 0, Name: name, Gender: 0,
		Face: []byte("{}"), Might: 10, Intellect: 10, Stamina: 10,
		Agility: 10, Mysticism: 10, Aim: 10, Karma: 0, Hometown: "tos",
		PosX: 0, PosY: 0, PosZ: 0,
		Vitals: []byte("{}"), Advancement: []byte("{}"), Flags: 0,
	})
	if err != nil {
		t.Fatalf("InsertCharacter: %v", err)
	}
	return ch.ID
}

func pgAbilityProtos(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO spell_protos (id,school,level,mana,exertion,cast_ms,min_hp,outlaw,harmful,reagents,params,version) VALUES (1,1,1,1,1,0,1,false,false,'{}','{}',1),(2,2,1,1,1,0,1,false,false,'{}','{}',1),(3,3,1,1,1,0,1,false,false,'{}','{}',1) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO skill_protos (id,division,level,exertion,params,version) VALUES (1,1,1,1,'{}',1),(2,2,1,1,'{}',1) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
}

func pgItemRoot(t *testing.T, pool *pgxpool.Pool, charID int64) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO item_protos (id,kind,slot,base,version) VALUES (900,0,NULL,'{}',1) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	var itemID int64
	if err := pool.QueryRow(ctx, `INSERT INTO item_instances (proto,qty,hits,enchants) VALUES (900,1,100,'{}') RETURNING id`).Scan(&itemID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO item_locations (item_id,kind,character_id,slot) VALUES ($1,0,$2,'back')`, itemID, charID); err != nil {
		t.Fatal(err)
	}
	return itemID
}

func pgBankRoot(t *testing.T, pool *pgxpool.Pool, charID int64, system string, balance int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO banks (character_id,system,balance) VALUES ($1,$2,$3)`, charID, system, balance); err != nil {
		t.Fatal(err)
	}
}

// staleCounter reads voxilian_store_stale_revision_total{aggregate}
// from reg via DTOs (no brittle full-text matching).
func staleCounter(t *testing.T, reg interface {
	Gather() ([]*dto.MetricFamily, error)
}, aggregate string,
) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "voxilian_store_stale_revision_total" {
			continue
		}
		for _, m := range f.Metric {
			for _, lp := range m.Label {
				if lp.GetName() == "aggregate" && lp.GetValue() == aggregate {
					return m.Counter.GetValue()
				}
			}
		}
	}
	return 0
}

// waitNoLockWait polls (Gosched spin, watchdog, no sleeps) until
// the given backend is no longer lock-waiting: the server has
// aborted or finished its query.
func waitNoLockWait(t *testing.T, pool *pgxpool.Pool, pid int32) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var waiting bool
		if err := pool.QueryRow(context.Background(),
			`SELECT COALESCE(wait_event_type = 'Lock', false) FROM pg_stat_activity WHERE pid = $1`,
			pid).Scan(&waiting); err != nil {
			t.Fatalf("poll backend %d: %v", pid, err)
		}
		if !waiting {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("backend %d still lock-waiting after cancel", pid)
		}
		runtime.Gosched()
	}
}

// waitBlockedBackend polls pg_stat_activity (Gosched spin, watchdog,
// no sleeps) for exactly one lock-waiting backend matching likeQuery
// on the test database, excluding our own inspection backend.
// It returns the blocked backend's PID.
func waitBlockedBackend(t *testing.T, pool *pgxpool.Pool, likeQuery string) int32 {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var pids []int32
		rows, err := pool.Query(ctx, `SELECT pid FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid() AND wait_event_type = 'Lock' AND query ILIKE $1`, likeQuery)
		if err != nil {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		for rows.Next() {
			var pid int32
			if err := rows.Scan(&pid); err != nil {
				rows.Close()
				t.Fatalf("scan pid: %v", err)
			}
			pids = append(pids, pid)
		}
		rows.Close()
		if len(pids) == 1 {
			return pids[0]
		}
		if len(pids) > 1 {
			t.Fatalf("ambiguous blocked backends for %q: %v", likeQuery, pids)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for blocked backend matching %q", likeQuery)
		}
		runtime.Gosched()
	}
}

// ---- real PG18 happy path: all three families through FlushAll ----

func TestPersistPGFlushAllThreeRoots(t *testing.T) {
	pool, q := openPG(t)
	ctx := context.Background()
	reg := newPGRegistry(t)
	st, err := store.New(pool, reg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	charID := pgAccountChar(t, q, "sub-persist-3", "Persisted")
	pgAbilityProtos(t, pool)
	itemID := pgItemRoot(t, pool, charID)
	pgBankRoot(t, pool, charID, "tos", 100)

	s := mustSaverForPersist(t)
	charKey := sim.AggregateKey{Kind: sim.AggregateCharacter, ID: charID}
	itemKey := sim.AggregateKey{Kind: sim.AggregateItem, ID: itemID}
	bankKey := sim.AggregateKey{Kind: sim.AggregateBank, ID: charID, Scope: "tos"}
	for _, tr := range []struct {
		k sim.AggregateKey
		r int64
	}{{charKey, 0}, {itemKey, 0}, {bankKey, 0}} {
		if err := s.Track(tr.k, tr.r); err != nil {
			t.Fatal(err)
		}
	}

	cjob, err := NewCharacterSnapshotJob(st, store.CharacterSnapshot{
		ID: charID, Karma: 9, PosX: 7, PosY: 8, PosZ: 9,
		Vitals: json.RawMessage(`{"hp":41}`), Advancement: json.RawMessage(`{"pts":3}`), Flags: 3,
		Spells: []store.CharacterSpellSnapshot{{SpellID: 2, Ability: 50}},
		Skills: []store.CharacterSkillSnapshot{{SkillID: 2, Ability: 44}},
	})
	if err != nil {
		t.Fatal(err)
	}
	slot := "mainhand"
	ijob, err := NewItemSnapshotJob(st, store.ItemSnapshot{
		ID: itemID, Qty: 3, Hits: 77, Enchants: json.RawMessage(`{"e":2}`),
		Location: store.ItemLocationSnapshot{Kind: 0, CharacterID: &charID, Slot: &slot},
	})
	if err != nil {
		t.Fatal(err)
	}
	bjob, err := NewBankSnapshotJob(st, store.BankSnapshot{CharacterID: charID, System: "tos", Balance: 140})
	if err != nil {
		t.Fatal(err)
	}
	for _, mj := range []SnapshotJob{cjob, ijob, bjob} {
		if err := s.MarkDirty(mj.Key, mj.Write); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.FlushAll(ctx); err != nil {
		t.Fatalf("FlushAll: %v", err)
	}

	// Actual materialized character state.
	got := mustGetChar(t, q, charID)
	if got.Revision != 1 || got.Karma != 9 || got.PosX != 7 || got.Flags != 3 {
		t.Fatalf("char root = %+v", got)
	}
	if string(got.Vitals) != `{"hp": 41}` && string(got.Vitals) != `{"hp":41}` {
		t.Fatalf("vitals = %s", got.Vitals)
	}
	if got.Name != "Persisted" || got.Slot != 0 || got.Might != 10 || got.Hometown != "tos" || got.DeletedAt.Valid {
		t.Fatalf("identity drifted: %+v", got)
	}
	spells, err := q.ListCharacterSpells(ctx, charID)
	if err != nil || len(spells) != 1 || spells[0].SpellID != 2 || spells[0].Ability != 50 {
		t.Fatalf("spells = %+v, %v", spells, err)
	}
	skills, err := q.ListCharacterSkills(ctx, charID)
	if err != nil || len(skills) != 1 || skills[0].SkillID != 2 || skills[0].Ability != 44 {
		t.Fatalf("skills = %+v, %v", skills, err)
	}

	// Actual materialized item state.
	inst, err := q.GetItemInstanceByID(ctx, itemID)
	if err != nil || inst.Revision != 1 || inst.Qty != 3 || inst.Hits != 77 {
		t.Fatalf("item = %+v, %v", inst, err)
	}
	if string(inst.Enchants) != `{"e": 2}` && string(inst.Enchants) != `{"e":2}` {
		t.Fatalf("enchants = %s", inst.Enchants)
	}
	loc, err := q.GetItemLocationByItemID(ctx, itemID)
	if err != nil || loc.Kind != 0 || !loc.CharacterID.Valid || loc.CharacterID.Int64 != charID || loc.Slot.String != "mainhand" {
		t.Fatalf("location = %+v, %v", loc, err)
	}

	// Actual materialized bank state.
	bank, err := st.LoadBankBalance(ctx, charID, "tos")
	if err != nil || bank.Balance != 140 || bank.ExpectedRevision != 1 {
		t.Fatalf("bank = %+v, %v", bank, err)
	}

	// Saver state agrees.
	for _, k := range []sim.AggregateKey{charKey, itemKey, bankKey} {
		snap, err := s.Inspect(k)
		if err != nil {
			t.Fatal(err)
		}
		if snap.KnownRevision != 1 || snap.Dirty || snap.Blocked {
			t.Fatalf("saver %v = %+v, want known1 clean", k, snap)
		}
	}
}

func mustGetChar(t *testing.T, q *gen.Queries, id int64) gen.Character {
	t.Helper()
	ch, err := q.GetCharacterByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetCharacterByID: %v", err)
	}
	return ch
}

func newPGRegistry(t *testing.T) *prometheus.Registry {
	t.Helper()
	return prometheus.NewRegistry()
}

// ---- real WriteThrough proof ----

func TestPersistPGWriteThrough(t *testing.T) {
	pool, q := openPG(t)
	ctx := context.Background()
	st, err := store.New(pool, newPGRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	charID := pgAccountChar(t, q, "sub-persist-wt", "Writethru")
	pgAbilityProtos(t, pool)
	pgBankRoot(t, pool, charID, "tos", 100)

	s := mustSaverForPersist(t)
	bankKey := sim.AggregateKey{Kind: sim.AggregateBank, ID: charID, Scope: "tos"}
	if err := s.Track(bankKey, 0); err != nil {
		t.Fatal(err)
	}
	bjob, err := NewBankSnapshotJob(st, store.BankSnapshot{CharacterID: charID, System: "tos", Balance: 175})
	if err != nil {
		t.Fatal(err)
	}
	rev, err := s.WriteThrough(ctx, bankKey, bjob.Write)
	if err != nil || rev != 1 {
		t.Fatalf("WriteThrough = (%d,%v)", rev, err)
	}
	bank, err := st.LoadBankBalance(ctx, charID, "tos")
	if err != nil || bank.Balance != 175 || bank.ExpectedRevision != 1 {
		t.Fatalf("bank = %+v, %v", bank, err)
	}
	snap, _ := s.Inspect(bankKey)
	if snap.KnownRevision != 1 || snap.Dirty {
		t.Fatalf("saver = %+v, want known1 clean synchronously", snap)
	}
}

// ---- real stale-CAS matrix: character/item/bank ----

type countingStore struct {
	SnapshotStore
	charCalls, itemCalls, bankCalls *int
}

func (c *countingStore) SaveCharacterSnapshot(ctx context.Context, snap store.CharacterSnapshot) (int64, error) {
	*c.charCalls++
	return c.SnapshotStore.SaveCharacterSnapshot(ctx, snap)
}

func (c *countingStore) SaveItemSnapshot(ctx context.Context, snap store.ItemSnapshot) (int64, error) {
	*c.itemCalls++
	return c.SnapshotStore.SaveItemSnapshot(ctx, snap)
}

func (c *countingStore) SaveBankBalance(ctx context.Context, snap store.BankSnapshot) (int64, error) {
	*c.bankCalls++
	return c.SnapshotStore.SaveBankBalance(ctx, snap)
}

func TestPersistPGStaleMatrix(t *testing.T) {
	pool, q := openPG(t)
	ctx := context.Background()
	reg := newPGRegistry(t)
	st, err := store.New(pool, reg)
	if err != nil {
		t.Fatal(err)
	}
	charID := pgAccountChar(t, q, "sub-persist-stale", "Staler")
	pgAbilityProtos(t, pool)
	itemID := pgItemRoot(t, pool, charID)
	pgBankRoot(t, pool, charID, "tos", 100)

	// Advance PG independently: durable is rev1 everywhere.
	if _, err := st.SaveCharacterSnapshot(ctx, store.CharacterSnapshot{ID: charID, Vitals: []byte(`{}`), Advancement: []byte(`{}`)}); err != nil {
		t.Fatalf("advance char: %v", err)
	}
	slot := "back"
	if _, err := st.SaveItemSnapshot(ctx, store.ItemSnapshot{ID: itemID, Enchants: []byte(`{}`),
		Location: store.ItemLocationSnapshot{Kind: 0, CharacterID: &charID, Slot: &slot}}); err != nil {
		t.Fatalf("advance item: %v", err)
	}
	if _, err := st.SaveBankBalance(ctx, store.BankSnapshot{CharacterID: charID, System: "tos", Balance: 100}); err != nil {
		t.Fatalf("advance bank: %v", err)
	}

	var cc, ic, bc int
	cst := &countingStore{SnapshotStore: st, charCalls: &cc, itemCalls: &ic, bankCalls: &bc}
	s := mustSaverForPersist(t)
	charKey := sim.AggregateKey{Kind: sim.AggregateCharacter, ID: charID}
	itemKey := sim.AggregateKey{Kind: sim.AggregateItem, ID: itemID}
	bankKey := sim.AggregateKey{Kind: sim.AggregateBank, ID: charID, Scope: "tos"}
	for _, k := range []sim.AggregateKey{charKey, itemKey, bankKey} {
		if err := s.Track(k, 0); err != nil {
			t.Fatal(err)
		}
	}
	cjob, _ := NewCharacterSnapshotJob(cst, store.CharacterSnapshot{ID: charID, Karma: 1})
	ijob, _ := NewItemSnapshotJob(cst, store.ItemSnapshot{ID: itemID, Qty: 9,
		Location: store.ItemLocationSnapshot{Kind: 0, CharacterID: &charID, Slot: &slot}})
	bjob, _ := NewBankSnapshotJob(cst, store.BankSnapshot{CharacterID: charID, System: "tos", Balance: 999})
	for _, mj := range []SnapshotJob{cjob, ijob, bjob} {
		if err := s.MarkDirty(mj.Key, mj.Write); err != nil {
			t.Fatal(err)
		}
	}
	flushErr := s.FlushDirty(ctx)
	if !errors.Is(flushErr, sim.ErrSnapshotStale) || !errors.Is(flushErr, store.ErrStaleRevision) {
		t.Fatalf("flush err = %v, want both stale sentinels", flushErr)
	}
	// Every key blocked at known 0.
	for _, k := range []sim.AggregateKey{charKey, itemKey, bankKey} {
		snap, _ := s.Inspect(k)
		if !snap.Blocked || snap.KnownRevision != 0 {
			t.Fatalf("saver %v = %+v, want blocked known0", k, snap)
		}
	}
	// No re-hammer: a second flush performs zero Store writes.
	cc, ic, bc = 0, 0, 0
	if err := s.FlushDirty(ctx); !errors.Is(err, sim.ErrSaverReconcileRequired) {
		t.Fatalf("reflush err = %v, want reconcile-required", err)
	}
	if cc+ic+bc != 0 {
		t.Fatalf("store calls on reflush = %d/%d/%d, want zero", cc, ic, bc)
	}
	// Exact stale-metric increments, one per family.
	for _, agg := range []string{"character", "item", "bank"} {
		if got := staleCounter(t, reg, agg); got != 1 {
			t.Fatalf("stale counter %q = %v, want 1", agg, got)
		}
	}
	// PG state untouched by the stale attempts.
	got := mustGetChar(t, q, charID)
	if got.Revision != 1 || got.Karma != 0 {
		t.Fatalf("char overwritten by stale: %+v", got)
	}
	bank, _ := st.LoadBankBalance(ctx, charID, "tos")
	if bank.Balance != 100 || bank.ExpectedRevision != 1 {
		t.Fatalf("bank overwritten by stale: %+v", bank)
	}
}

// ---- saver lag against real PG with deterministic Now ----

type stepNowPG struct {
	t time.Time
}

func (s *stepNowPG) Now() time.Time { return s.t }

type lagTap struct {
	agg string
	lag time.Duration
	n   int
}

func (l *lagTap) SaverLag(aggregate string, lag time.Duration) {
	l.agg, l.lag = aggregate, lag
	l.n++
}

func histCountSum(t *testing.T, reg *prometheus.Registry, metric, aggregate string) (float64, float64) {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != metric {
			continue
		}
		for _, m := range f.Metric {
			for _, lp := range m.Label {
				if lp.GetName() == "aggregate" && lp.GetValue() == aggregate {
					return float64(m.Histogram.GetSampleCount()), m.Histogram.GetSampleSum()
				}
			}
		}
	}
	t.Fatalf("series %s{%s} missing", metric, aggregate)
	return 0, 0
}

func TestPersistPGSaverLagHappy(t *testing.T) {
	pool, q := openPG(t)
	ctx := context.Background()
	st, err := store.New(pool, newPGRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	charID := pgAccountChar(t, q, "sub-persist-lag", "Lagger")
	pgBankRoot(t, pool, charID, "tos", 100)

	now := &stepNowPG{t: time.Unix(100, 0)}
	metricsReg := prometheus.NewRegistry()
	metrics := observe.NewSaverMetrics(metricsReg)
	s, err := sim.NewSaver(sim.SaverConfig{
		Interval: 60 * 1_000_000_000, Clock: sim.NewSystemClock(),
		Now: now.Now, Observer: metrics,
	})
	if err != nil {
		t.Fatal(err)
	}
	bankKey := sim.AggregateKey{Kind: sim.AggregateBank, ID: charID, Scope: "tos"}
	if err := s.Track(bankKey, 0); err != nil {
		t.Fatal(err)
	}
	bjob, err := NewBankSnapshotJob(st, store.BankSnapshot{CharacterID: charID, System: "tos", Balance: 110})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDirty(bankKey, bjob.Write); err != nil {
		t.Fatal(err)
	}
	now.t = time.Unix(103, 500_000_000) // acknowledgement 3.5 s after capture.
	if err := s.FlushDirty(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	count, sum := histCountSum(t, metricsReg, "vox_saver_lag_seconds", "bank")
	if count != 1 || sum != 3.5 {
		t.Fatalf("bank lag count/sum = %v/%v, want 1/3.5", count, sum)
	}
}

// ---- real bank stale -> bridge reconciliation -> fresh mutation ----

func TestPersistPGBankReconciliation(t *testing.T) {
	pool, q := openPG(t)
	ctx := context.Background()
	st, err := store.New(pool, newPGRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	charID := pgAccountChar(t, q, "sub-persist-rec", "Recon")
	pgBankRoot(t, pool, charID, "tos", 100)

	s := mustSaverForPersist(t)
	bankKey := sim.AggregateKey{Kind: sim.AggregateBank, ID: charID, Scope: "tos"}
	if err := s.Track(bankKey, 0); err != nil {
		t.Fatal(err)
	}
	state, err := sim.NewReconcileState(0)
	if err != nil {
		t.Fatal(err)
	}
	var memBalance int64 = 100
	var memRev int64

	// PG advances independently to 150/rev1; the saver's rev0 write goes stale.
	if _, err := st.SaveBankBalance(ctx, store.BankSnapshot{CharacterID: charID, System: "tos", Balance: 150}); err != nil {
		t.Fatal(err)
	}
	staleJob, err := NewBankSnapshotJob(st, store.BankSnapshot{CharacterID: charID, System: "tos", Balance: 100})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDirty(bankKey, staleJob.Write); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushDirty(ctx); !errors.Is(err, sim.ErrSnapshotStale) {
		t.Fatalf("flush err = %v, want stale", err)
	}
	ss, _ := s.Inspect(bankKey)
	if !ss.Blocked || ss.KnownRevision != 0 {
		t.Fatalf("saver = %+v, want blocked known0", ss)
	}

	// Production bridge: forced reload, staged load, memory replace, resolve.
	if err := ReconcileSaver(ctx, state, s, bankKey, BankReload(st, charID, "tos",
		func(snap store.BankSnapshot) error {
			memBalance, memRev = snap.Balance, snap.ExpectedRevision
			return nil
		})); err != nil {
		t.Fatalf("ReconcileSaver: %v", err)
	}
	rs := state.Snapshot()
	if rs.Pending || rs.KnownRevision != 1 {
		t.Fatalf("t3c = %+v, want clear known1", rs)
	}
	ss, _ = s.Inspect(bankKey)
	if ss.Blocked || ss.Dirty || ss.KnownRevision != 1 {
		t.Fatalf("saver = %+v, want clear known1 clean", ss)
	}
	if memBalance != 150 || memRev != 1 {
		t.Fatalf("memory = %d/rev%d, want 150/rev1", memBalance, memRev)
	}

	// Fresh post-reconcile mutation persists against expected1 -> rev2.
	memBalance += 5
	fresh, err := NewBankSnapshotJob(st, store.BankSnapshot{CharacterID: charID, System: "tos", Balance: memBalance})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDirty(bankKey, fresh.Write); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushDirty(ctx); err != nil {
		t.Fatalf("post-reconcile flush: %v", err)
	}
	bank, _ := st.LoadBankBalance(ctx, charID, "tos")
	if bank.Balance != 155 || bank.ExpectedRevision != 2 {
		t.Fatalf("bank = %+v, want 155/rev2", bank)
	}
	ss, _ = s.Inspect(bankKey)
	if ss.KnownRevision != 2 {
		t.Fatalf("saver = %+v, want known2", ss)
	}
}

func TestPersistPGReconcileReloadFailure(t *testing.T) {
	pool, q := openPG(t)
	ctx := context.Background()
	st, err := store.New(pool, newPGRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	charID := pgAccountChar(t, q, "sub-persist-recfail", "Recfail")
	pgBankRoot(t, pool, charID, "tos", 100)

	s := mustSaverForPersist(t)
	bankKey := sim.AggregateKey{Kind: sim.AggregateBank, ID: charID, Scope: "tos"}
	if err := s.Track(bankKey, 0); err != nil {
		t.Fatal(err)
	}
	state, err := sim.NewReconcileState(0)
	if err != nil {
		t.Fatal(err)
	}
	// Stale-block the saver first via an independent PG advance.
	if _, err := st.SaveBankBalance(ctx, store.BankSnapshot{CharacterID: charID, System: "tos", Balance: 150}); err != nil {
		t.Fatal(err)
	}
	staleJob, _ := NewBankSnapshotJob(st, store.BankSnapshot{CharacterID: charID, System: "tos", Balance: 100})
	if err := s.MarkDirty(bankKey, staleJob.Write); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushDirty(ctx); !errors.Is(err, sim.ErrSnapshotStale) {
		t.Fatalf("flush err = %v, want stale", err)
	}
	memBalance := int64(100)
	failLoader := &fakeBankLoader{err: errFakeLoad}
	err = ReconcileSaver(ctx, state, s, bankKey, BankReload(failLoader, charID, "tos",
		func(store.BankSnapshot) error { memBalance = -1; return nil }))
	if !errors.Is(err, errFakeLoad) {
		t.Fatalf("bridge err = %v, want loader cause", err)
	}
	if !state.Snapshot().Pending {
		t.Fatal("t3c cleared despite loader failure")
	}
	ss, _ := s.Inspect(bankKey)
	if !ss.Blocked || ss.KnownRevision != 0 {
		t.Fatalf("saver = %+v, want still blocked known0", ss)
	}
	if memBalance != 100 {
		t.Fatalf("memory mutated on failed reload: %d", memBalance)
	}
	// Retry with healthy PG converges.
	if err := ReconcileSaver(ctx, state, s, bankKey, BankReload(st, charID, "tos",
		func(snap store.BankSnapshot) error { memBalance = snap.Balance; return nil })); err != nil {
		t.Fatalf("healthy retry: %v", err)
	}
	if memBalance != 150 {
		t.Fatalf("memory = %d, want 150", memBalance)
	}
	ss, _ = s.Inspect(bankKey)
	if ss.Blocked || ss.KnownRevision != 1 {
		t.Fatalf("saver = %+v, want clear known1", ss)
	}
}

// ---- ambiguous commit: real PG CAS, lost acknowledgement, stale retry, reconcile ----

var errLostAck = errors.New("persist test lost success acknowledgement")

type lostAckStore struct {
	SnapshotStore
	armed *bool
}

func (s *lostAckStore) SaveBankBalance(ctx context.Context, snap store.BankSnapshot) (int64, error) {
	rev, err := s.SnapshotStore.SaveBankBalance(ctx, snap)
	if err != nil {
		return rev, err
	}
	if *s.armed {
		*s.armed = false
		return 0, errLostAck // PG committed; the caller lost the result.
	}
	return rev, nil
}

func TestPersistPGAmbiguousCommit(t *testing.T) {
	pool, q := openPG(t)
	ctx := context.Background()
	st, err := store.New(pool, newPGRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	charID := pgAccountChar(t, q, "sub-persist-amb", "Ambiguous")
	pgBankRoot(t, pool, charID, "tos", 100)

	s := mustSaverForPersist(t)
	bankKey := sim.AggregateKey{Kind: sim.AggregateBank, ID: charID, Scope: "tos"}
	if err := s.Track(bankKey, 0); err != nil {
		t.Fatal(err)
	}
	state, err := sim.NewReconcileState(0)
	if err != nil {
		t.Fatal(err)
	}
	armed := true
	wrap := &lostAckStore{SnapshotStore: st, armed: &armed}
	job, err := NewBankSnapshotJob(wrap, store.BankSnapshot{CharacterID: charID, System: "tos", Balance: 120})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDirty(bankKey, job.Write); err != nil {
		t.Fatal(err)
	}

	// First attempt: PG commits 120/rev1, acknowledgement lost.
	if err := s.FlushDirty(ctx); !errors.Is(err, errLostAck) {
		t.Fatalf("first flush err = %v, want lost-ack", err)
	} else if errors.Is(err, sim.ErrSnapshotStale) {
		t.Fatalf("lost ack misclassified as stale: %v", err)
	}
	bank, _ := st.LoadBankBalance(ctx, charID, "tos")
	if bank.Balance != 120 || bank.ExpectedRevision != 1 {
		t.Fatalf("PG = %+v, want committed 120/rev1", bank)
	}
	ss, _ := s.Inspect(bankKey)
	if ss.KnownRevision != 0 || !ss.Dirty || ss.Blocked {
		t.Fatalf("saver = %+v, want known0 dirty unblocked (no guessing)", ss)
	}

	// Retry of the retained snapshot: PG already rev1 -> stale -> block.
	if err := s.FlushDirty(ctx); !errors.Is(err, sim.ErrSnapshotStale) ||
		!errors.Is(err, store.ErrStaleRevision) {
		t.Fatalf("retry err = %v, want both stale sentinels", err)
	}
	ss, _ = s.Inspect(bankKey)
	if ss.KnownRevision != 0 || !ss.Blocked {
		t.Fatalf("saver = %+v, want known0 blocked", ss)
	}

	// Reconcile from PG, then the next mutation persists against rev1.
	var memBalance int64
	if err := ReconcileSaver(ctx, state, s, bankKey, BankReload(st, charID, "tos",
		func(snap store.BankSnapshot) error { memBalance = snap.Balance; return nil })); err != nil {
		t.Fatalf("ReconcileSaver: %v", err)
	}
	if memBalance != 120 {
		t.Fatalf("memory = %d, want reloaded 120", memBalance)
	}
	next, err := NewBankSnapshotJob(wrap, store.BankSnapshot{CharacterID: charID, System: "tos", Balance: 130})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDirty(bankKey, next.Write); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushDirty(ctx); err != nil {
		t.Fatalf("post-reconcile flush: %v", err)
	}
	bank, _ = st.LoadBankBalance(ctx, charID, "tos")
	if bank.Balance != 130 || bank.ExpectedRevision != 2 {
		t.Fatalf("PG = %+v, want 130/rev2 (no double-write, no overwrite)", bank)
	}
	ss, _ = s.Inspect(bankKey)
	if ss.KnownRevision != 2 || ss.Dirty || ss.Blocked {
		t.Fatalf("saver = %+v, want known2 clean", ss)
	}
}

// ---- crash injection: terminate the real SaveCharacterSnapshot backend mid-transaction ----

func TestPersistPGCrashMidCharacterSave(t *testing.T) {
	pool, q := openPG(t)
	ctx := context.Background()
	reg := newPGRegistry(t)
	st, err := store.New(pool, reg)
	if err != nil {
		t.Fatal(err)
	}
	charID := pgAccountChar(t, q, "sub-persist-crash", "Crasher")
	pgAbilityProtos(t, pool)
	// Seed initial child rows so rollback has something to preserve.
	if err := q.InsertCharacterSpell(ctx, gen.InsertCharacterSpellParams{CharacterID: charID, SpellID: 1, Ability: 10}); err != nil {
		t.Fatal(err)
	}
	if err := q.InsertCharacterSkill(ctx, gen.InsertCharacterSkillParams{CharacterID: charID, SkillID: 1, Ability: 30}); err != nil {
		t.Fatal(err)
	}
	var isSuper bool
	if err := pool.QueryRow(ctx, `SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&isSuper); err != nil || !isSuper {
		t.Fatalf("crash test needs a superuser backend (pg_terminate_backend): %v super=%v", err, isSuper)
	}

	s := mustSaverForPersist(t)
	charKey := sim.AggregateKey{Kind: sim.AggregateCharacter, ID: charID}
	if err := s.Track(charKey, 0); err != nil {
		t.Fatal(err)
	}
	cjob, err := NewCharacterSnapshotJob(st, store.CharacterSnapshot{
		ID: charID, Karma: 42,
		Vitals: json.RawMessage(`{"hp":99}`), Advancement: json.RawMessage(`{}`),
		Spells: []store.CharacterSpellSnapshot{{SpellID: 3, Ability: 77}},
		Skills: []store.CharacterSkillSnapshot{{SkillID: 2, Ability: 66}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDirty(charKey, cjob.Write); err != nil {
		t.Fatal(err)
	}

	// Control connection holds the child-table lock: the worker's
	// root CAS executes, then its spell replacement blocks.
	ctrl, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ctrl.Release()
	ctrlTx, err := ctrl.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ctrlTx.Exec(ctx, `LOCK TABLE character_spells IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	flushDone := make(chan error, 1)
	go func() { flushDone <- s.FlushDirty(ctx) }()
	// The blocked backend is the worker's spell DELETE: exactly one
	// lock-waiter, and it is not our control connection (idle) nor
	// this inspection backend (excluded by the query itself).
	blockedPID := waitBlockedBackend(t, pool, `%character_spells%`)
	var ctrlPID int32
	if err := ctrl.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&ctrlPID); err != nil {
		t.Fatal(err)
	}
	if blockedPID == ctrlPID {
		t.Fatalf("would terminate the control connection (pid %d)", blockedPID)
	}
	var terminated bool
	if err := pool.QueryRow(ctx, `SELECT pg_terminate_backend($1)`, blockedPID).Scan(&terminated); err != nil || !terminated {
		t.Fatalf("terminate pid %d: %v terminated=%v", blockedPID, err, terminated)
	}
	if err := ctrlTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	flushErr := <-flushDone
	if flushErr == nil {
		t.Fatal("terminated save returned nil")
	}
	if errors.Is(flushErr, sim.ErrSnapshotStale) || errors.Is(flushErr, store.ErrStaleRevision) {
		t.Fatalf("connection death misclassified as stale: %v", flushErr)
	}

	// Full rollback: root, spells, and skills all pre-save.
	got := mustGetChar(t, q, charID)
	if got.Revision != 0 || got.Karma != 0 {
		t.Fatalf("root not rolled back: %+v", got)
	}
	spells, err := q.ListCharacterSpells(ctx, charID)
	if err != nil || len(spells) != 1 || spells[0].SpellID != 1 || spells[0].Ability != 10 {
		t.Fatalf("spells not rolled back: %+v, %v", spells, err)
	}
	skills, err := q.ListCharacterSkills(ctx, charID)
	if err != nil || len(skills) != 1 || skills[0].SkillID != 1 {
		t.Fatalf("skills not rolled back: %+v, %v", skills, err)
	}
	// The uncommitted termination is not a stale CAS.
	if got := staleCounter(t, reg, "character"); got != 0 {
		t.Fatalf("stale counter = %v after crash, want 0", got)
	}
	// Saver retains the dirty snapshot unblocked at the old revision.
	ss, _ := s.Inspect(charKey)
	if ss.KnownRevision != 0 || !ss.Dirty || ss.Blocked {
		t.Fatalf("saver = %+v, want known0 dirty unblocked", ss)
	}
	// Retry with the same snapshot and expected revision commits.
	if err := s.FlushDirty(ctx); err != nil {
		t.Fatalf("retry flush: %v", err)
	}
	got = mustGetChar(t, q, charID)
	if got.Revision != 1 || got.Karma != 42 {
		t.Fatalf("retry root = %+v, want rev1 karma42", got)
	}
}

// ---- shutdown: PG row lock + FlushAll cancellation ----

func TestPersistPGShutdownFlushCancellation(t *testing.T) {
	pool, q := openPG(t)
	charID := pgAccountChar(t, q, "sub-persist-shut", "Shutdown")
	pgBankRoot(t, pool, charID, "tos", 100)
	st, err := store.New(pool, newPGRegistry(t))
	if err != nil {
		t.Fatal(err)
	}

	s := mustSaverForPersist(t)
	bankKey := sim.AggregateKey{Kind: sim.AggregateBank, ID: charID, Scope: "tos"}
	if err := s.Track(bankKey, 0); err != nil {
		t.Fatal(err)
	}
	bjob, err := NewBankSnapshotJob(st, store.BankSnapshot{CharacterID: charID, System: "tos", Balance: 160})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDirty(bankKey, bjob.Write); err != nil {
		t.Fatal(err)
	}

	// Control transaction holds the bank row lock.
	ctrl, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer ctrl.Release()
	ctrlTx, err := ctrl.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ctrlTx.Exec(context.Background(), `UPDATE banks SET balance = balance WHERE character_id = $1 AND system = 'tos'`, charID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	flushDone := make(chan error, 1)
	go func() { flushDone <- s.FlushAll(ctx) }()
	// The worker's bank UPDATE must be the one lock-waiter.
	blockedPID := waitBlockedBackend(t, pool, `%banks%`)
	var ctrlPID int32
	if err := ctrl.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&ctrlPID); err != nil {
		t.Fatal(err)
	}
	if blockedPID == ctrlPID {
		t.Fatalf("blocked pid is the control connection (%d)", ctrlPID)
	}
	cancel()
	// Wait for the SERVER to abort the blocked query while this
	// test still holds the row lock: commit is impossible for as
	// long as the lock is held, so whatever the server reaches,
	// PG cannot have advanced. This closes the cancel/commit
	// race where a client-side cancel returns while the server
	// would otherwise commit after a later lock release.
	waitNoLockWait(t, pool, blockedPID)
	flushErr := <-flushDone
	if !errors.Is(flushErr, context.Canceled) {
		t.Fatalf("FlushAll err = %v, want context.Canceled discoverable", flushErr)
	}
	var dbrev int64
	if err := pool.QueryRow(context.Background(), `SELECT revision FROM banks WHERE character_id=$1 AND system='tos'`, charID).Scan(&dbrev); err != nil || dbrev != 0 {
		t.Fatalf("PG advanced under held lock: rev=%d err=%v", dbrev, err)
	}
	if err := ctrlTx.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Dirty state retained at the old revision.
	ss, _ := s.Inspect(bankKey)
	if ss.KnownRevision != 0 || !ss.Dirty {
		t.Fatalf("saver = %+v, want known0 dirty after cancel", ss)
	}
	// A later healthy flush succeeds.
	if err := s.FlushAll(context.Background()); err != nil {
		t.Fatalf("healthy flush: %v", err)
	}
	bank, _ := st.LoadBankBalance(context.Background(), charID, "tos")
	if bank.Balance != 160 || bank.ExpectedRevision != 1 {
		t.Fatalf("bank = %+v, want 160/rev1", bank)
	}
}

// ---- no retry spin: one FlushAll attempts each key once ----

func TestPersistPGNoRetrySpin(t *testing.T) {
	pool, q := openPG(t)
	charID := pgAccountChar(t, q, "sub-persist-spin", "Spinner")
	pgBankRoot(t, pool, charID, "tos", 100)
	st, err := store.New(pool, newPGRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	fail := &failBankStore{SnapshotStore: st, calls: &calls}
	s := mustSaverForPersist(t)
	bankKey := sim.AggregateKey{Kind: sim.AggregateBank, ID: charID, Scope: "tos"}
	if err := s.Track(bankKey, 0); err != nil {
		t.Fatal(err)
	}
	bjob, err := NewBankSnapshotJob(fail, store.BankSnapshot{CharacterID: charID, System: "tos", Balance: 111})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDirty(bankKey, bjob.Write); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushAll(context.Background()); !errors.Is(err, errFakeTransient) {
		t.Fatalf("FlushAll err = %v, want transient", err)
	}
	if calls != 1 {
		t.Fatalf("store calls = %d, want exactly 1 (no spin)", calls)
	}
}

type failBankStore struct {
	SnapshotStore
	calls *int
}

func (f *failBankStore) SaveBankBalance(context.Context, store.BankSnapshot) (int64, error) {
	*f.calls++
	return 0, errFakeTransient
}

// ---- shared registry: Store stale metric + saver lag coexist ----

func TestPersistPGRegistryCoexistence(t *testing.T) {
	pool, q := openPG(t)
	ctx := context.Background()
	obs := observe.New(observe.NewReadiness())
	st, err := store.New(pool, obs.Registry())
	if err != nil {
		t.Fatalf("store.New on observe registry: %v", err)
	}
	charID := pgAccountChar(t, q, "sub-persist-coex", "Coexist")
	pgBankRoot(t, pool, charID, "tos", 100)
	if _, err := st.SaveBankBalance(ctx, store.BankSnapshot{CharacterID: charID, System: "tos", Balance: 100}); err != nil {
		t.Fatal(err)
	}
	// One stale attempt to materialize the stale series.
	if _, err := st.SaveBankBalance(ctx, store.BankSnapshot{CharacterID: charID, System: "tos", Balance: 100}); !errors.Is(err, store.ErrStaleRevision) {
		t.Fatalf("stale err = %v", err)
	}
	obs.SaverMetrics().SaverLag("bank", 250*time.Millisecond)
	families, err := obs.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, f := range families {
		seen[f.GetName()] = true
	}
	if !seen["vox_saver_lag_seconds"] || !seen["voxilian_store_stale_revision_total"] {
		t.Fatalf("registry families missing coexistence: %v", seen)
	}
}
