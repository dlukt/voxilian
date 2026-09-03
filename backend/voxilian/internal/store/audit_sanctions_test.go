package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dlukt/voxilian/internal/store/gen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func timeDate(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func sanctionBackdateSQL(kind string) string {
	if kind == "ban" {
		return `UPDATE bans SET created_at = $1 WHERE account_id = $2`
	}
	return `UPDATE mutes SET created_at = $1 WHERE account_id = $2`
}

// auditFixtures builds two accounts, one character each, one item proto +
// instance, and two mob protos via raw SQL (catalog production access is
// M1-T6d; fixtures are test-only).
func auditFixtures(t *testing.T, pool *pgxpool.Pool, q *gen.Queries) (a1, a2, c1, c2, item int64, m1, m2 int32) {
	t.Helper()
	ctx := context.Background()
	mkacct := func(sub string) int64 {
		acct, err := q.CreateAccount(ctx, gen.CreateAccountParams{KeycloakSub: sub})
		if err != nil {
			t.Fatal(err)
		}
		return acct.ID
	}
	a1, a2 = mkacct("sub-au1"), mkacct("sub-au2")
	mkchar := func(acct int64, slot int16, name string) int64 {
		ch, err := createCharacter(ctx, q, validCharParams(acct, slot, name))
		if err != nil {
			t.Fatal(err)
		}
		return ch.ID
	}
	c1, c2 = mkchar(a1, 0, "Auditor"), mkchar(a2, 0, "Auditee")
	if _, err := pool.Exec(ctx, `INSERT INTO item_protos (id,kind,slot,base,version) VALUES (800,0,NULL,'{}',1)`); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO item_instances (proto,qty,hits,enchants) VALUES (800,1,1,'{}') RETURNING id`).Scan(&item); err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct {
		id  int32
		key string
	}{{7000, "au-mob-a"}, {7001, "au-mob-b"}} {
		if _, err := pool.Exec(ctx, `INSERT INTO mob_protos (id,key,level,difficulty,karma,atk,resists,spells,loot_tid,version) VALUES ($1,$2,45,6,-40,'{}','{}','{}',NULL,1)`, m.id, m.key); err != nil {
			t.Fatal(err)
		}
	}
	return a1, a2, c1, c2, item, 7000, 7001
}

func TestLedgerAppend(t *testing.T) {
	pool, q := openQueries(t)
	ctx := context.Background()
	a1, a2, c1, c2, item, _, _ := auditFixtures(t, pool, q)

	// 1-5: actor/counterparty shape matrix.
	row, err := q.InsertLedger(ctx, gen.InsertLedgerParams{Kind: 1, ActorAccountID: int8(a1)})
	if err != nil {
		t.Fatalf("account actor: %v", err)
	}
	if row.ID <= 0 || !row.CreatedAt.Valid {
		t.Fatalf("identity/timestamp: %+v", row)
	}
	if _, err := q.InsertLedger(ctx, gen.InsertLedgerParams{Kind: 1, ActorCharacterID: int8(c1)}); err != nil {
		t.Fatalf("character actor: %v", err)
	}
	if _, err := q.InsertLedger(ctx, gen.InsertLedgerParams{Kind: 2, ActorCharacterID: int8(c1)}); err != nil {
		t.Fatalf("no counterparty: %v", err)
	}
	if _, err := q.InsertLedger(ctx, gen.InsertLedgerParams{Kind: 2, ActorCharacterID: int8(c1), CptyAccountID: int8(a2)}); err != nil {
		t.Fatalf("account cpty: %v", err)
	}
	if _, err := q.InsertLedger(ctx, gen.InsertLedgerParams{Kind: 2, ActorCharacterID: int8(c1), CptyCharacterID: int8(c2)}); err != nil {
		t.Fatalf("character cpty: %v", err)
	}

	// Actor/counterparty identity violations stay PG errors, unmapped.
	for _, tc := range []struct {
		name string
		arg  gen.InsertLedgerParams
		want string
	}{
		{"no-actor", gen.InsertLedgerParams{Kind: 1}, "ledger_actor_identity_check"},
		{"both-actors", gen.InsertLedgerParams{Kind: 1, ActorAccountID: int8(a1), ActorCharacterID: int8(c1)}, "ledger_actor_identity_check"},
		{"both-cpty", gen.InsertLedgerParams{Kind: 1, ActorAccountID: int8(a1), CptyAccountID: int8(a2), CptyCharacterID: int8(c2)}, "ledger_counterparty_identity_check"},
	} {
		_, err := q.InsertLedger(ctx, tc.arg)
		var pgErr *pgconn.PgError
		if err == nil || !errors.As(err, &pgErr) || pgErr.ConstraintName != tc.want {
			t.Fatalf("%s err = %v, want %s", tc.name, err, tc.want)
		}
	}

	// FK violations surface normally.
	if _, err := q.InsertLedger(ctx, gen.InsertLedgerParams{Kind: 1, ActorCharacterID: int8(999999)}); err == nil {
		t.Fatal("bad actor accepted")
	}
	if _, err := q.InsertLedger(ctx, gen.InsertLedgerParams{Kind: 1, ActorCharacterID: int8(c1), ItemID: int8(999999)}); err == nil {
		t.Fatal("bad item accepted")
	}

	// 6-9: payload shapes — BIGINT/negative amount, NULL qty/item, item FK.
	got, err := q.InsertLedger(ctx, gen.InsertLedgerParams{
		Kind: 3, ActorAccountID: int8(a1), Amount: int8(5_000_000_000), ItemID: int8(item),
	})
	if err != nil || !got.Amount.Valid || got.Amount.Int64 != 5_000_000_000 || !got.ItemID.Valid {
		t.Fatalf("payload = %+v, %v", got, err)
	}
	if got.Qty.Valid {
		t.Fatalf("qty must stay NULL: %+v", got)
	}
	if _, err := q.InsertLedger(ctx, gen.InsertLedgerParams{Kind: 3, ActorAccountID: int8(a1), Amount: int8(-250)}); err != nil {
		t.Fatalf("negative amount: %v", err)
	}
}

func TestKillAppend(t *testing.T) {
	pool, q := openQueries(t)
	ctx := context.Background()
	_, _, c1, c2, _, m1, m2 := auditFixtures(t, pool, q)

	// All four pairings.
	pairs := []gen.InsertKillParams{
		{KillerKind: 0, KillerCharacterID: int8(c1), VictimKind: 0, VictimCharacterID: int8(c2)},
		{KillerKind: 0, KillerCharacterID: int8(c1), VictimKind: 1, VictimMobID: int32p(m2)},
		{KillerKind: 1, KillerMobID: int32p(m1), VictimKind: 0, VictimCharacterID: int8(c2)},
		{KillerKind: 1, KillerMobID: int32p(m1), VictimKind: 1, VictimMobID: int32p(m2),
			PosX: 5_000_000_000, PosY: -5_000_000_000, PosZ: 0},
	}
	for i, p := range pairs {
		row, err := q.InsertKill(ctx, p)
		if err != nil {
			t.Fatalf("pair %d: %v", i, err)
		}
		if row.ID <= 0 || !row.CreatedAt.Valid || row.KillerKind != p.KillerKind || row.VictimKind != p.VictimKind {
			t.Fatalf("pair %d row = %+v", i, row)
		}
	}
	row, err := q.InsertKill(ctx, pairs[3])
	if err != nil || row.PosX != 5_000_000_000 || row.PosY != -5_000_000_000 {
		t.Fatalf("coords = %+v, %v", row, err)
	}

	// Malformed identities hit named checks; bad FKs surface.
	bad := []struct {
		name string
		arg  gen.InsertKillParams
		want string
	}{
		{"killer-no-char", gen.InsertKillParams{KillerKind: 0, VictimKind: 0, VictimCharacterID: int8(c2)}, "kills_killer_identity_check"},
		{"killer-no-mob", gen.InsertKillParams{KillerKind: 1, VictimKind: 0, VictimCharacterID: int8(c2)}, "kills_killer_identity_check"},
		{"killer-kind", gen.InsertKillParams{KillerKind: 2, KillerCharacterID: int8(c1), VictimKind: 0, VictimCharacterID: int8(c2)}, "kills_killer_identity_check"},
		{"victim-no-char", gen.InsertKillParams{KillerKind: 0, KillerCharacterID: int8(c1), VictimKind: 0}, "kills_victim_identity_check"},
		{"victim-no-mob", gen.InsertKillParams{KillerKind: 0, KillerCharacterID: int8(c1), VictimKind: 1}, "kills_victim_identity_check"},
		{"victim-kind", gen.InsertKillParams{KillerKind: 0, KillerCharacterID: int8(c1), VictimKind: 2, VictimCharacterID: int8(c2)}, "kills_victim_identity_check"},
	}
	for _, tc := range bad {
		_, err := q.InsertKill(ctx, tc.arg)
		var pgErr *pgconn.PgError
		if err == nil || !errors.As(err, &pgErr) || pgErr.ConstraintName != tc.want {
			t.Fatalf("%s err = %v, want %s", tc.name, err, tc.want)
		}
	}
	if _, err := q.InsertKill(ctx, gen.InsertKillParams{KillerKind: 0, KillerCharacterID: int8(999999), VictimKind: 0, VictimCharacterID: int8(c2)}); err == nil {
		t.Fatal("bad killer char accepted")
	}
	if _, err := q.InsertKill(ctx, gen.InsertKillParams{KillerKind: 1, KillerMobID: int32p(9999), VictimKind: 0, VictimCharacterID: int8(c2)}); err == nil {
		t.Fatal("bad killer mob accepted")
	}
}

func int32p(v int32) pgtype.Int4 {
	return pgtype.Int4{Int32: v, Valid: true}
}

func TestSanctions(t *testing.T) {
	testSanction(t, "ban",
		func(ctx context.Context, q *gen.Queries, acct int64, reason string, exp pgtype.Timestamptz) (sanctionRow, error) {
			b, err := q.UpsertBan(ctx, gen.UpsertBanParams{AccountID: acct, Reason: reason, ExpiresAt: exp})
			return sanctionRow{acct: b.AccountID, reason: b.Reason, exp: b.ExpiresAt, created: b.CreatedAt}, err
		},
		func(ctx context.Context, q *gen.Queries, acct int64) (sanctionRow, error) {
			b, err := q.GetBan(ctx, acct)
			return sanctionRow{acct: b.AccountID, reason: b.Reason, exp: b.ExpiresAt, created: b.CreatedAt}, err
		},
		func(ctx context.Context, q *gen.Queries, acct int64) error {
			return q.DeleteBan(ctx, acct)
		},
	)
	testSanction(t, "mute",
		func(ctx context.Context, q *gen.Queries, acct int64, reason string, exp pgtype.Timestamptz) (sanctionRow, error) {
			b, err := q.UpsertMute(ctx, gen.UpsertMuteParams{AccountID: acct, Reason: reason, ExpiresAt: exp})
			return sanctionRow{acct: b.AccountID, reason: b.Reason, exp: b.ExpiresAt, created: b.CreatedAt}, err
		},
		func(ctx context.Context, q *gen.Queries, acct int64) (sanctionRow, error) {
			b, err := q.GetMute(ctx, acct)
			return sanctionRow{acct: b.AccountID, reason: b.Reason, exp: b.ExpiresAt, created: b.CreatedAt}, err
		},
		func(ctx context.Context, q *gen.Queries, acct int64) error {
			return q.DeleteMute(ctx, acct)
		},
	)
}

type sanctionRow struct {
	acct    int64
	reason  string
	exp     pgtype.Timestamptz
	created pgtype.Timestamptz
}

func testSanction(
	t *testing.T,
	kind string,
	upsert func(ctx context.Context, q *gen.Queries, acct int64, reason string, exp pgtype.Timestamptz) (sanctionRow, error),
	get func(ctx context.Context, q *gen.Queries, acct int64) (sanctionRow, error),
	revoke func(ctx context.Context, q *gen.Queries, acct int64) error,
) {
	pool, q := openQueries(t)
	ctx := context.Background()
	acctRow, err := q.CreateAccount(ctx, gen.CreateAccountParams{KeycloakSub: "sub-san-" + kind})
	if err != nil {
		t.Fatal(err)
	}
	acct := acctRow.ID

	// Permanent sanction.
	row, err := upsert(ctx, q, acct, "cheating", pgtype.Timestamptz{})
	if err != nil || row.reason != "cheating" || row.exp.Valid || !row.created.Valid {
		t.Fatalf("[%s] permanent = %+v, %v", kind, row, err)
	}
	got, err := get(ctx, q, acct)
	if err != nil || got.reason != "cheating" {
		t.Fatalf("[%s] get = %+v, %v", kind, got, err)
	}

	// Temporary sanction round-trips expiry exactly.
	exp := pgtype.Timestamptz{Time: timeDate(2027, 6, 1), Valid: true}
	if _, err := upsert(ctx, q, acct, "spam", exp); err != nil {
		t.Fatalf("[%s] temporary: %v", kind, err)
	}
	// Past expiry stays readable (enforcement decides activity, not SQL).
	past := pgtype.Timestamptz{Time: timeDate(2020, 1, 1), Valid: true}
	if _, err := upsert(ctx, q, acct, "old", past); err != nil {
		t.Fatalf("[%s] past: %v", kind, err)
	}
	got, err = get(ctx, q, acct)
	if err != nil || got.reason != "old" || !got.exp.Valid {
		t.Fatalf("[%s] past-expiry get = %+v, %v", kind, got, err)
	}

	// Replacement resets created_at: backdate via test-only SQL, then upsert.
	if _, err := pool.Exec(ctx, sanctionBackdateSQL(kind), timeDate(2020, 1, 2), acct); err != nil {
		t.Fatalf("[%s] backdate: %v", kind, err)
	}
	row, err = upsert(ctx, q, acct, "again", pgtype.Timestamptz{})
	if err != nil {
		t.Fatalf("[%s] replace: %v", kind, err)
	}
	if row.reason != "again" || row.exp.Valid || !row.created.Time.After(timeDate(2020, 1, 2)) {
		t.Fatalf("[%s] replacement = %+v", kind, row)
	}

	// Revoke → NoRows; second revoke harmless; bad account FK.
	if err := revoke(ctx, q, acct); err != nil {
		t.Fatalf("[%s] revoke: %v", kind, err)
	}
	if _, err := get(ctx, q, acct); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("[%s] post-revoke get = %v, want NoRows", kind, err)
	}
	if err := revoke(ctx, q, acct); err != nil {
		t.Fatalf("[%s] second revoke: %v", kind, err)
	}
	if _, err := upsert(ctx, q, 999999, "x", pgtype.Timestamptz{}); err == nil {
		t.Fatalf("[%s] bad account accepted", kind)
	}
}
