package store

import (
	"context"
	"testing"

	"github.com/dlukt/voxilian/internal/sim"
	"github.com/dlukt/voxilian/internal/store/gen"
)

// This file proves M4-T3c post-commit reconciliation against real
// PG18 (spec §5.6.8). The existing banks(character_id, system,
// balance, revision) root is used ONLY as a convenient
// already-existing revisioned aggregate fixture: there is no new
// bank gameplay, no trade semantics, and no money-transfer API.
// The two-bank single-transaction commit below is TEST
// scaffolding composing the existing private saveBankBalance
// seam; no production multi-bank method is exposed. No ledger
// row is inserted, read, or replayed anywhere here: recovery
// comes from the materialized banks row alone.

// TestLoadBankBalance proves the reconciliation loader: the
// persisted balance plus the persisted revision (immediately
// CAS-ready), and a clean missing-row error with no
// auto-creation.
func TestLoadBankBalance(t *testing.T) {
	pool, q := openQueries(t)
	ctx := context.Background()
	st := newTestStore(t, pool)
	acct, err := q.CreateAccount(ctx, gen.CreateAccountParams{KeycloakSub: "sub-reconload"})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := createCharacter(ctx, q, validCharParams(acct.ID, 0, "Loader"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.InsertBank(ctx, gen.InsertBankParams{CharacterID: ch.ID, System: "tos", Balance: 100}); err != nil {
		t.Fatal(err)
	}
	got, err := st.LoadBankBalance(ctx, ch.ID, "tos")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Balance != 100 || got.ExpectedRevision != 0 || got.CharacterID != ch.ID || got.System != "tos" {
		t.Fatalf("loaded = %+v, want 100/r0 with composite key", got)
	}
	rev, err := st.SaveBankBalance(ctx, BankSnapshot{CharacterID: ch.ID, System: "tos", ExpectedRevision: 0, Balance: 200})
	if err != nil || rev != 1 {
		t.Fatalf("save = %d,%v", rev, err)
	}
	got, err = st.LoadBankBalance(ctx, ch.ID, "tos")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Balance != 200 || got.ExpectedRevision != 1 {
		t.Fatalf("reloaded = %+v, want 200/r1", got)
	}
	// Missing composite key: error, no row creation, no fake
	// zero snapshot treated as success.
	if _, err := st.LoadBankBalance(ctx, ch.ID, "nope"); !isNoRows(err) {
		t.Fatalf("missing = %v, want pgx.ErrNoRows convention", err)
	}
	if _, err := q.GetBank(ctx, gen.GetBankParams{CharacterID: ch.ID, System: "nope"}); !isNoRows(err) {
		t.Fatalf("loader created a row: %v", err)
	}
}

// reconMemBank is the test-only in-memory durable participant:
// gameplay value plus its sim reconciliation fence.
type reconMemBank struct {
	value int64
	state *sim.ReconcileState
}

func mustReconMemBank(t *testing.T, knownRevision, value int64) *reconMemBank {
	t.Helper()
	st, err := sim.NewReconcileState(knownRevision)
	if err != nil {
		t.Fatalf("NewReconcileState(%d): %v", knownRevision, err)
	}
	return &reconMemBank{value: value, state: st}
}

// TestPostCommitReconciliationPG proves the full post-commit
// cycle against real PG18: a test-only one-transaction two-root
// CAS commit, fence-after-commit, one delivered and one dropped
// notification, actual PG/memory divergence while fenced, a
// staged reload before B's next mutation, and a CAS rev1→rev2
// persist-after-reload proof.
func TestPostCommitReconciliationPG(t *testing.T) {
	pool, q := openQueries(t)
	ctx := context.Background()
	st := newTestStore(t, pool)
	acct, err := q.CreateAccount(ctx, gen.CreateAccountParams{KeycloakSub: "sub-reconpg"})
	if err != nil {
		t.Fatal(err)
	}
	chA, err := createCharacter(ctx, q, validCharParams(acct.ID, 0, "ReconA"))
	if err != nil {
		t.Fatal(err)
	}
	chB, err := createCharacter(ctx, q, validCharParams(acct.ID, 1, "ReconB"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.InsertBank(ctx, gen.InsertBankParams{CharacterID: chA.ID, System: "tos", Balance: 100}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.InsertBank(ctx, gen.InsertBankParams{CharacterID: chB.ID, System: "tos", Balance: 200}); err != nil {
		t.Fatal(err)
	}
	memA := mustReconMemBank(t, 0, 100)
	memB := mustReconMemBank(t, 0, 200)

	// Failed two-bank transaction: the second CAS misses, the
	// transaction rolls back, both PG rows are unchanged, and NO
	// fences install because no commit occurred.
	badTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	badQ := gen.New(badTx)
	if _, err := saveBankBalance(ctx, badQ, BankSnapshot{CharacterID: chA.ID, System: "tos", ExpectedRevision: 0, Balance: 110}); err != nil {
		_ = badTx.Rollback(ctx)
		t.Fatalf("first CAS in rollback case: %v", err)
	}
	if _, err := saveBankBalance(ctx, badQ, BankSnapshot{CharacterID: chB.ID, System: "tos", ExpectedRevision: 99, Balance: 180}); err == nil {
		_ = badTx.Rollback(ctx)
		t.Fatal("stale second CAS succeeded, want failure")
	}
	if err := badTx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	for _, tc := range []struct {
		id   int64
		want int64
	}{
		{chA.ID, 100}, {chB.ID, 200},
	} {
		row, err := q.GetBank(ctx, gen.GetBankParams{CharacterID: tc.id, System: "tos"})
		if err != nil || row.Balance != tc.want || row.Revision != 0 {
			t.Fatalf("rolled-back bank %d = %+v,%v", tc.id, row, err)
		}
	}
	if memA.state.Snapshot().Pending || memB.state.Snapshot().Pending {
		t.Fatal("fence installed without a commit")
	}

	// Successful two-bank transaction: both CAS operations under
	// one test-owned PG transaction, committed once.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tq := gen.New(tx)
	revA, err := saveBankBalance(ctx, tq, BankSnapshot{CharacterID: chA.ID, System: "tos", ExpectedRevision: 0, Balance: 110})
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("CAS A: %v", err)
	}
	revB, err := saveBankBalance(ctx, tq, BankSnapshot{CharacterID: chB.ID, System: "tos", ExpectedRevision: 0, Balance: 180})
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("CAS B: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if revA != 1 || revB != 1 {
		t.Fatalf("revs = %d/%d, want 1/1", revA, revB)
	}
	// Fences install ONLY after tx.Commit returns, before any
	// notification.
	if err := memA.state.MarkCommitted(revA); err != nil {
		t.Fatal(err)
	}
	if err := memB.state.MarkCommitted(revB); err != nil {
		t.Fatal(err)
	}

	// Deliver A's commit notice; DROP B's.
	opA, opB := sim.OpID(5001), sim.OpID(5002)
	if d, err := memA.state.ApplyCommitNotice(sim.DurableCommitNotice{ID: opA, Revision: revA}, func() error {
		memA.value = 110
		return nil
	}); err != nil || d != sim.CommitNoticeApplied {
		t.Fatalf("A notice = %v,%v", d, err)
	}
	// Intentional divergence while B is fenced: memory B is
	// stale, PG B is committed.
	if s := memA.state.Snapshot(); s.Pending || s.KnownRevision != 1 || memA.value != 110 {
		t.Fatalf("A = %+v val %d, want current 110/k1", s, memA.value)
	}
	if s := memB.state.Snapshot(); !s.Pending || s.RequiredRevision != 1 || s.KnownRevision != 0 || memB.value != 200 {
		t.Fatalf("B = %+v val %d, want pending/req1/known0/200", s, memB.value)
	}
	pgB, err := q.GetBank(ctx, gen.GetBankParams{CharacterID: chB.ID, System: "tos"})
	if err != nil || pgB.Balance != 180 || pgB.Revision != 1 {
		t.Fatalf("PG B = %+v,%v, want committed 180/r1", pgB, err)
	}
	_ = opB // B's notification never arrives.

	// B's next mutation reloads first: the staged loader reads
	// PG into LOCAL temporaries, validation runs, and only then
	// does Apply replace memory. The mutation callback must
	// observe the committed 180/rev1 — never stale 200/rev0.
	var sawValue, sawKnown int64
	loader := func(ctx context.Context) (sim.ReloadCandidate, error) {
		loaded, err := st.LoadBankBalance(ctx, chB.ID, "tos")
		if err != nil {
			return sim.ReloadCandidate{}, err
		}
		return sim.ReloadCandidate{Revision: loaded.ExpectedRevision, Apply: func() error {
			memB.value = loaded.Balance
			return nil
		}}, nil
	}
	if err := memB.state.EnsureReconciled(ctx, loader); err != nil {
		t.Fatalf("B reload: %v", err)
	}
	sawValue, sawKnown = memB.value, memB.state.Snapshot().KnownRevision
	if sawValue != 180 || sawKnown != 1 {
		t.Fatalf("post-reload B = %d/k%d, want 180/k1", sawValue, sawKnown)
	}
	memB.value += 5
	if memB.value != 185 {
		t.Fatalf("B after mutate = %d, want 185 (205 proves stale mutation)", memB.value)
	}

	// Persist-after-reload: the reconciled revision is the
	// correct CAS base — a focused revision proof, not a saver.
	revB2, err := st.SaveBankBalance(ctx, BankSnapshot{CharacterID: chB.ID, System: "tos", ExpectedRevision: 1, Balance: 185})
	if err != nil || revB2 != 2 {
		t.Fatalf("persist = %d,%v, want rev2", revB2, err)
	}
	final, err := st.LoadBankBalance(ctx, chB.ID, "tos")
	if err != nil || final.Balance != 185 || final.ExpectedRevision != 2 {
		t.Fatalf("final = %+v,%v, want 185/r2", final, err)
	}
}
