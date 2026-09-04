package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/dlukt/voxilian/internal/store/gen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// Store is the persistence boundary for future sim/gateway consumers.
// Method signatures use Store-domain types only: no gen.*, pgx.*,
// pgtype.*, or pgxpool.* escapes the interface (the constructor alone
// takes a pool). Low-level primitives (DeleteCharacterSpells,
// UpsertItemLocation, CASUpdate*, Get*ForUpdate) stay inside the package.
type Store interface {
	SaveCharacterSnapshot(ctx context.Context, snap CharacterSnapshot) (int64, error)
	SoftDeleteCharacter(ctx context.Context, id, expectedRevision int64) (int64, error)
	SaveItemSnapshot(ctx context.Context, snap ItemSnapshot) (int64, error)
	SaveBankBalance(ctx context.Context, snap BankSnapshot) (int64, error)
	UpsertCatalogBatch(ctx context.Context, batch CatalogBatch, allowDowngrade bool) error
	LoadCatalogRegistry(ctx context.Context) (*CatalogRegistry, error)
	EnsureAccount(ctx context.Context, keycloakSub string, email *string) (int64, error)
	CreateCharacter(ctx context.Context, nc NewCharacter) (int64, error)
	ListLiveCharacters(ctx context.Context, accountID int64) ([]LiveCharacter, error)
}

// BankSnapshot is the complete bank write: composite identity plus the
// new balance. ExpectedRevision drives the CAS; no other fields exist
// on the aggregate.
type BankSnapshot struct {
	CharacterID      int64
	System           string
	ExpectedRevision int64
	Balance          int64
}

// NewCharacterAbility is one ability row of a creation aggregate:
// stable catalog ID plus server-resolved initial ability percentage.
// Atrophy always starts false (no field: the store hard-codes it).
type NewCharacterAbility struct {
	ID      int32
	Ability int16
}

// NewCharacterItem is one starter-inventory template of a creation
// aggregate. Empty Enchants persists as {}.
type NewCharacterItem struct {
	ProtoID  int32
	Qty      int32
	Hits     int32
	Enchants []byte
	Slot     string
}

// NewCharacter is the complete creation aggregate (spec §9): root
// fields plus abilities plus starter inventory. Face/Vitals/
// Advancement carry already-encoded JSON. No proto/gen/pgx types.
type NewCharacter struct {
	AccountID int64
	Slot      int16
	Name      string
	Gender    int16
	Face      []byte

	Might     int16
	Intellect int16
	Stamina   int16
	Agility   int16
	Mysticism int16
	Aim       int16

	Karma       int32
	Hometown    string
	PosX        int64
	PosY        int64
	PosZ        int64
	Vitals      []byte
	Advancement []byte
	Flags       int32

	Spells []NewCharacterAbility
	Skills []NewCharacterAbility
	Items  []NewCharacterItem
}

// LiveCharacter is the store-domain list row: identity plus the fields
// character-list summaries and slot lookup need.
type LiveCharacter struct {
	ID       int64
	Slot     int16
	Name     string
	Revision int64
	Vitals   []byte
}

// PGStore is the concrete PostgreSQL Store. Construct via New; methods
// satisfy Store.
type PGStore struct {
	pool  *pgxpool.Pool
	stale *prometheus.CounterVec
}

var _ Store = (*PGStore)(nil)

// New wires a Store over pool, registering the stale-revision counter on
// the supplied registry (e.g. observe.Server.Registry()). A nil registry
// gets a private throwaway one (unregistered operational mode for CLI
// and tests that do not assert metrics). Duplicate registration against
// a shared registry returns an error; use fresh registries per Store in
// tests.
func New(pool *pgxpool.Pool, registerer prometheus.Registerer) (*PGStore, error) {
	if registerer == nil {
		registerer = prometheus.NewRegistry()
	}
	stale := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "voxilian_store_stale_revision_total",
			Help: "Number of persisted aggregate CAS attempts rejected because the expected revision did not match.",
		},
		[]string{"aggregate"},
	)
	if err := registerer.Register(stale); err != nil {
		return nil, fmt.Errorf("store: register stale counter: %w", err)
	}
	return &PGStore{pool: pool, stale: stale}, nil
}

// recordStale counts one rejected CAS at the public operation boundary.
// Call exactly when returning ErrStaleRevision — never for cycle, CHECK,
// FK, version, commit, or connection failures.
func (s *PGStore) recordStale(aggregate string) {
	s.stale.WithLabelValues(aggregate).Inc()
}

// saveCharacterSnapshotTx is the private seam future critical operations
// reuse to compose character state + ledger in one transaction.
func saveCharacterSnapshotTx(ctx context.Context, tx pgx.Tx, snap CharacterSnapshot) (int64, error) {
	q := gen.New(tx)
	newRev, err := q.CASUpdateCharacterSnapshot(ctx, gen.CASUpdateCharacterSnapshotParams{
		Karma: snap.Karma, PosX: snap.PosX, PosY: snap.PosY, PosZ: snap.PosZ,
		Vitals: snap.Vitals, Advancement: snap.Advancement, Flags: snap.Flags,
		ExpectedRevision: snap.ExpectedRevision, ID: snap.ID,
	})
	if err != nil {
		return 0, err
	}
	if err := q.DeleteCharacterSpells(ctx, snap.ID); err != nil {
		return 0, err
	}
	for _, sp := range snap.Spells {
		if err := q.InsertCharacterSpell(ctx, gen.InsertCharacterSpellParams{
			CharacterID: snap.ID, SpellID: sp.SpellID, Ability: sp.Ability, AtrophyFlag: sp.AtrophyFlag,
		}); err != nil {
			return 0, err
		}
	}
	if err := q.DeleteCharacterSkills(ctx, snap.ID); err != nil {
		return 0, err
	}
	for _, sk := range snap.Skills {
		if err := q.InsertCharacterSkill(ctx, gen.InsertCharacterSkillParams{
			CharacterID: snap.ID, SkillID: sk.SkillID, Ability: sk.Ability, AtrophyFlag: sk.AtrophyFlag,
		}); err != nil {
			return 0, err
		}
	}
	return newRev, nil
}

// SaveCharacterSnapshot persists a complete character aggregate snapshot.
// See saveCharacterSnapshotTx and snapshot.go for ordering semantics.
func (s *PGStore) SaveCharacterSnapshot(ctx context.Context, snap CharacterSnapshot) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: save character snapshot: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	newRev, err := saveCharacterSnapshotTx(ctx, tx, snap)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.recordStale("character")
			return 0, staleRevision("save character snapshot", snap.ID, snap.ExpectedRevision)
		}
		return 0, fmt.Errorf("store: save character snapshot: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: save character snapshot: commit: %w", err)
	}
	return newRev, nil
}

// SoftDeleteCharacter CAS-deletes a live character (see snapshot.go).
func (s *PGStore) SoftDeleteCharacter(ctx context.Context, id, expectedRevision int64) (int64, error) {
	q := gen.New(s.pool)
	newRev, err := q.CASSoftDeleteCharacter(ctx, gen.CASSoftDeleteCharacterParams{
		ExpectedRevision: expectedRevision, ID: id,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.recordStale("character")
			return 0, staleRevision("soft delete character", id, expectedRevision)
		}
		return 0, fmt.Errorf("store: soft delete character: %w", err)
	}
	return newRev, nil
}

// SaveItemSnapshot persists a complete item aggregate snapshot (see
// item_snapshot.go for ordering, locking, and cycle semantics).
func (s *PGStore) SaveItemSnapshot(ctx context.Context, snap ItemSnapshot) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: save item snapshot: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	newRev, err := saveItemSnapshotTx(ctx, tx, snap)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.recordStale("item")
			return 0, staleRevision("save item snapshot", snap.ID, snap.ExpectedRevision)
		}
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: save item snapshot: commit: %w", err)
	}
	return newRev, nil
}

// saveBankBalance is the private seam for future trade/bank/ledger
// composition: same CAS against a caller-owned transaction.
func saveBankBalance(ctx context.Context, q *gen.Queries, snap BankSnapshot) (int64, error) {
	return q.CASUpdateBankBalance(ctx, gen.CASUpdateBankBalanceParams{
		Balance: snap.Balance, ExpectedRevision: snap.ExpectedRevision,
		CharacterID: snap.CharacterID, System: snap.System,
	})
}

// SaveBankBalance CAS-advances one bank root. Missing row, stale, or
// future revision all surface as ErrStaleRevision; nothing auto-creates.
func (s *PGStore) SaveBankBalance(ctx context.Context, snap BankSnapshot) (int64, error) {
	newRev, err := saveBankBalance(ctx, gen.New(s.pool), snap)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.recordStale("bank")
			return 0, staleRevision("save bank balance", snap.CharacterID, snap.ExpectedRevision)
		}
		return 0, fmt.Errorf("store: save bank balance: %w", err)
	}
	return newRev, nil
}

// UpsertCatalogBatch applies a version-ruled batch atomically (see
// catalog.go). Method form for Store consumers.
func (s *PGStore) UpsertCatalogBatch(ctx context.Context, batch CatalogBatch, allowDowngrade bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: catalog batch: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := upsertCatalogBatchTx(ctx, tx, batch, allowDowngrade); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: catalog batch: commit: %w", err)
	}
	return nil
}

// LoadCatalogRegistry reads all five catalogs under one repeatable-read
// snapshot (see catalog.go).
func (s *PGStore) LoadCatalogRegistry(ctx context.Context) (*CatalogRegistry, error) {
	return loadCatalogRegistry(ctx, s.pool)
}

// EnsureAccount maps a validated Keycloak subject to its durable
// account, auto-provisioning the row on first login (spec §6.2.1). An
// existing subject returns its ID without touching the stored email —
// login is not profile synchronization. A new row stores the validated
// email (CITEXT) or NULL when absent. Concurrent first logins for one
// subject converge: a UNIQUE-race loser re-reads by sub and returns the
// winner's ID instead of failing the login.
func (s *PGStore) EnsureAccount(ctx context.Context, keycloakSub string, email *string) (int64, error) {
	if keycloakSub == "" {
		return 0, errors.New("store: ensure account: empty sub")
	}
	q := gen.New(s.pool)
	if acct, err := q.GetAccountByKeycloakSub(ctx, keycloakSub); err == nil {
		return acct.ID, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("store: ensure account: lookup: %w", err)
	}
	var em pgtype.Text
	if email != nil && *email != "" {
		em = pgtype.Text{String: *email, Valid: true}
	}
	acct, err := q.CreateAccount(ctx, gen.CreateAccountParams{KeycloakSub: keycloakSub, Email: em})
	if err == nil {
		return acct.ID, nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		winner, rerr := q.GetAccountByKeycloakSub(ctx, keycloakSub)
		if rerr != nil {
			return 0, fmt.Errorf("store: ensure account: re-read after conflict: %w", rerr)
		}
		return winner.ID, nil
	}
	return 0, fmt.Errorf("store: ensure account: create: %w", err)
}
