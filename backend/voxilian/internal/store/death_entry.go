package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/dlukt/voxilian/internal/store/gen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// ErrDeathAlreadyPending reports a death-entry replay: the victim
// already has a live pending_deaths row, so the new entry is rejected
// and the whole transaction rolls back. It maps ONLY the
// pending_deaths_pkey unique violation from the death transaction —
// never stale revisions, corpse-index, FK, or CHECK failures — and it
// never increments the stale-revision metric.
var ErrDeathAlreadyPending = errors.New("death already pending")

// ErrInvalidDeathEntry marks a malformed CommitDeathEntry request
// rejected before meaningful PG mutation. Matching MUST use
// errors.Is, never string parsing.
var ErrInvalidDeathEntry = errors.New("invalid death entry")

// DeathEntryKillerKind is the auditable killer identity domain for a
// death entry: character or mob, matching the existing kills
// killer_kind values. Zero is invalid/unset.
type DeathEntryKillerKind uint8

const (
	// DeathEntryKillerCharacter audits a player killer (killer_kind=0).
	DeathEntryKillerCharacter DeathEntryKillerKind = iota + 1
	// DeathEntryKillerMob audits a mob-proto killer (killer_kind=1).
	DeathEntryKillerMob
)

// DeathEntryKiller is the optional auditable killer in Store-domain
// types only. Exactly one identity must be valid for the selected
// kind; nil means an environmental death with no kills row.
type DeathEntryKiller struct {
	Kind DeathEntryKillerKind

	CharacterID int64
	MobID       int32
}

// DeathEntryItem is one caller-resolved item aggregate mutation of a
// death entry: the COMPLETE resulting item snapshot plus this death's
// PK-protection duration. Zero duration means this entry creates or
// replaces no protection; it never deletes existing protection.
type DeathEntryItem struct {
	Snapshot ItemSnapshot

	// PKProtectionDuration is zero for unprotected relocations
	// (including the Token-death special relocation).
	PKProtectionDuration time.Duration
}

// DeathEntryRequest is the complete already-resolved post-death
// aggregate input for one atomic death entry. The character snapshot
// is the COMPLETE post-death character aggregate (vitals incl. any
// restored rest threshold, position, advancement, flags, complete
// spells/skills); Store recalculates no gameplay mechanics.
// DeathPosX/Y/Z is the death position shared by the corpse row, all
// ground item relocations, and the kills audit row — deliberately NOT
// forced equal to Character.PosX/Y/Z (the durable post-death player
// position, e.g. newbie-home vs Underworld placement resolved by T5c).
type DeathEntryRequest struct {
	Character CharacterSnapshot

	DeathPosX int64
	DeathPosY int64
	DeathPosZ int64

	EffectiveDeathCost int16
	DeathTimeSeconds   int64

	CorpseLifetime time.Duration

	Items []DeathEntryItem

	Killer *DeathEntryKiller
}

// DeathEntryItemRevision is one committed item root revision, in
// ascending ItemID order.
type DeathEntryItemRevision struct {
	ItemID   int64
	Revision int64
}

// DeathEntryResult carries only durable information known after a
// successful commit. On ANY error the result is zero/empty: a
// generated CorpseID is never exposed before commit succeeds, and a
// commit error is ambiguous — the caller must reconcile/retry per
// §8.3, never assume rollback or success.
type DeathEntryResult struct {
	CharacterRevision int64
	CorpseID          int64
	ItemRevisions     []DeathEntryItemRevision
}

// Unix-microsecond bounds for the persisted timestamptz path. The
// pgx v5.10.0 binary TimestamptzCodec encodes finite timestamps as a
// signed int64 microsecond scalar (Unix seconds * 1e6 plus fractional
// microseconds, relative to a fixed Unix->Y2K offset) using unchecked
// arithmetic — so the death transaction must prove representability
// itself before a time.Time is ever constructed or sent.
const (
	unixMicrosPerSecond = int64(1_000_000)
	unixNanosPerMicro   = int64(1_000)
)

// deathExpiryTime derives baseSec + d as an exact expiry time.Time,
// proving first with checked integer arithmetic that the finite
// Unix-microsecond scalar the PostgreSQL/pgx timestamptz path encodes
// fits in a signed int64. Inputs must already satisfy baseSec >= 0
// and d >= 0 (enforced by request validation); violations here are
// still reported as ErrInvalidDeathEntry, never a panic.
//
// The check accounts for every overflow contributor: the whole
// seconds carried by the duration, its sub-second remainder
// (truncated to whole microseconds exactly as the codec encodes it),
// the Unix seconds -> microseconds multiplication, and the addition
// of the fractional microseconds. Because the base is non-negative,
// once the resulting Unix-microsecond value fits int64, the codec's
// later subtraction of the fixed Unix->Y2K offset is also safe.
//
// time.Time.Add is deliberately NOT used to detect overflow: only
// after the scalar is proven safe is a time.Time constructed from
// its exact seconds + microseconds decomposition. No calendar upper
// bound (year 9999 or otherwise) is imposed — rejection is purely
// about representability of the actual encoded scalar.
func deathExpiryTime(baseSec int64, d time.Duration) (time.Time, error) {
	if baseSec < 0 || d < 0 {
		return time.Time{}, fmt.Errorf("store: commit death entry negative timestamp base=%d duration=%s: %w",
			baseSec, d, ErrInvalidDeathEntry)
	}
	durMicros := int64(d) / unixNanosPerMicro
	if baseSec > (math.MaxInt64-durMicros)/unixMicrosPerSecond {
		return time.Time{}, fmt.Errorf("store: commit death entry timestamp overflow base=%d duration=%s: %w",
			baseSec, d, ErrInvalidDeathEntry)
	}
	totalMicros := baseSec*unixMicrosPerSecond + durMicros
	return time.Unix(totalMicros/unixMicrosPerSecond, (totalMicros%unixMicrosPerSecond)*unixNanosPerMicro).UTC(), nil
}

// deathCASStale marks which aggregate root CAS missed inside the
// death transaction so the public boundary can record exactly one
// stale metric with the right aggregate label. It unwraps to
// pgx.ErrNoRows for errors.Is compatibility.
type deathCASStale struct {
	aggregate string
	id        int64
	expected  int64
}

func (e *deathCASStale) Error() string {
	return fmt.Sprintf("store: commit death entry %s id=%d expected revision=%d: %s",
		e.aggregate, e.id, e.expected, pgx.ErrNoRows)
}

func (e *deathCASStale) Unwrap() error { return pgx.ErrNoRows }

// validateDeathEntryRequest rejects malformed requests before any PG
// mutation where practical. It never touches the database.
func validateDeathEntryRequest(req DeathEntryRequest) error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("store: commit death entry "+format+": %w",
			append(args, ErrInvalidDeathEntry)...)
	}
	if req.Character.ID <= 0 {
		return invalid("character id=%d", req.Character.ID)
	}
	if req.Character.ExpectedRevision < 0 {
		return invalid("character expected revision=%d", req.Character.ExpectedRevision)
	}
	if req.EffectiveDeathCost < 0 || req.EffectiveDeathCost > 100 {
		return invalid("effective cost=%d", req.EffectiveDeathCost)
	}
	if req.DeathTimeSeconds < 0 {
		return invalid("death time=%d", req.DeathTimeSeconds)
	}
	if req.CorpseLifetime <= 0 {
		return invalid("corpse lifetime=%s", req.CorpseLifetime)
	}
	seen := make(map[int64]struct{}, len(req.Items))
	for _, it := range req.Items {
		id := it.Snapshot.ID
		if id <= 0 {
			return invalid("item id=%d", id)
		}
		if it.Snapshot.ExpectedRevision < 0 {
			return invalid("item id=%d expected revision=%d", id, it.Snapshot.ExpectedRevision)
		}
		if _, dup := seen[id]; dup {
			return invalid("duplicate item id=%d", id)
		}
		seen[id] = struct{}{}
		if it.PKProtectionDuration < 0 {
			return invalid("item id=%d negative PK protection=%s", id, it.PKProtectionDuration)
		}
		loc := it.Snapshot.Location
		if loc.Kind != 1 {
			return invalid("item id=%d location kind=%d, want ground", id, loc.Kind)
		}
		if loc.PosX == nil || loc.PosY == nil || loc.PosZ == nil ||
			*loc.PosX != req.DeathPosX || *loc.PosY != req.DeathPosY || *loc.PosZ != req.DeathPosZ {
			return invalid("item id=%d ground position != death position", id)
		}
		if loc.CharacterID != nil || loc.CorpseID != nil ||
			loc.ContainerItemID != nil || loc.VaultRegion != nil || loc.Slot != nil {
			return invalid("item id=%d ground location carries non-ground reference", id)
		}
	}
	if req.Killer != nil {
		switch req.Killer.Kind {
		case DeathEntryKillerCharacter:
			if req.Killer.CharacterID <= 0 || req.Killer.MobID != 0 {
				return invalid("character killer identity character=%d mob=%d",
					req.Killer.CharacterID, req.Killer.MobID)
			}
		case DeathEntryKillerMob:
			if req.Killer.MobID <= 0 || req.Killer.CharacterID != 0 {
				return invalid("mob killer identity character=%d mob=%d",
					req.Killer.CharacterID, req.Killer.MobID)
			}
		default:
			return invalid("unknown killer kind=%d", uint8(req.Killer.Kind))
		}
	}
	// Derived persisted timestamps must be representable as the finite
	// Unix-microsecond scalar the timestamptz path encodes; a hostile
	// base or duration is rejected here, before Begin, with zero PG
	// mutation and zero stale-metric increment.
	if _, err := deathExpiryTime(req.DeathTimeSeconds, req.CorpseLifetime); err != nil {
		return err
	}
	for _, it := range req.Items {
		if it.PKProtectionDuration > 0 {
			if _, err := deathExpiryTime(req.DeathTimeSeconds, it.PKProtectionDuration); err != nil {
				return err
			}
		}
	}
	return nil
}

// sortedDeathItems returns a COPY of the caller's items sorted by Item
// ID ascending for the deterministic root lock/CAS order. The
// caller's slice is never reordered or modified.
func sortedDeathItems(items []DeathEntryItem) []DeathEntryItem {
	out := make([]DeathEntryItem, len(items))
	copy(out, items)
	sort.Slice(out, func(i, j int) bool { return out[i].Snapshot.ID < out[j].Snapshot.ID })
	return out
}

// commitDeathEntryTx composes the full death entry inside an
// already-begun transaction: character root CAS first, item roots in
// ascending ItemID order (each with its PK-protection upsert inside
// the same transaction right after its successful item-root CAS),
// then corpse insert (generated ID), pending-death insert, and the
// optional kills row — committing exactly once via the caller.
// Player death items are GROUND locations, so they never need the
// generated corpse ID. Raw pgx.ErrNoRows from a root CAS surfaces as
// *deathCASStale for the public boundary to map + count exactly once;
// every other error passes through unmapped.
func commitDeathEntryTx(ctx context.Context, tx pgx.Tx, req DeathEntryRequest) (DeathEntryResult, error) {
	q := gen.New(tx)

	charRev, err := saveCharacterSnapshotTx(ctx, tx, req.Character)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DeathEntryResult{}, &deathCASStale{
				aggregate: "character", id: req.Character.ID,
				expected: req.Character.ExpectedRevision,
			}
		}
		return DeathEntryResult{}, fmt.Errorf("store: commit death entry character: %w", err)
	}

	items := sortedDeathItems(req.Items)
	itemRevs := make([]DeathEntryItemRevision, 0, len(items))
	for _, it := range items {
		rev, err := saveItemSnapshotTx(ctx, tx, it.Snapshot)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return DeathEntryResult{}, &deathCASStale{
					aggregate: "item", id: it.Snapshot.ID,
					expected: it.Snapshot.ExpectedRevision,
				}
			}
			return DeathEntryResult{}, fmt.Errorf("store: commit death entry item id=%d: %w", it.Snapshot.ID, err)
		}
		if it.PKProtectionDuration > 0 {
			// Already proven representable by request validation;
			// re-derived through the same checked helper so
			// validation and execution cannot drift.
			protExpires, err := deathExpiryTime(req.DeathTimeSeconds, it.PKProtectionDuration)
			if err != nil {
				return DeathEntryResult{}, err
			}
			if _, err := q.UpsertItemPKProtection(ctx, gen.UpsertItemPKProtectionParams{
				ItemID:            it.Snapshot.ID,
				VictimCharacterID: req.Character.ID,
				ExpiresAt:         pgtype.Timestamptz{Time: protExpires, Valid: true},
			}); err != nil {
				return DeathEntryResult{}, fmt.Errorf("store: commit death entry item id=%d PK protection: %w", it.Snapshot.ID, err)
			}
		}
		itemRevs = append(itemRevs, DeathEntryItemRevision{ItemID: it.Snapshot.ID, Revision: rev})
	}

	corpseExpires, err := deathExpiryTime(req.DeathTimeSeconds, req.CorpseLifetime)
	if err != nil {
		return DeathEntryResult{}, err
	}
	corpse, err := q.InsertCorpse(ctx, gen.InsertCorpseParams{
		CharacterID: req.Character.ID,
		PosX:        req.DeathPosX,
		PosY:        req.DeathPosY,
		PosZ:        req.DeathPosZ,
		ExpiresAt:   pgtype.Timestamptz{Time: corpseExpires, Valid: true},
	})
	if err != nil {
		return DeathEntryResult{}, fmt.Errorf("store: commit death entry corpse: %w", err)
	}

	if _, err := q.InsertPendingDeath(ctx, gen.InsertPendingDeathParams{
		CharacterID:      req.Character.ID,
		EffectiveCost:    req.EffectiveDeathCost,
		DeathTimeSeconds: req.DeathTimeSeconds,
		CorpseID:         pgtype.Int8{Int64: corpse.ID, Valid: true},
		PortalUsed:       false,
	}); err != nil {
		return DeathEntryResult{}, fmt.Errorf("store: commit death entry pending death: %w", err)
	}

	if req.Killer != nil {
		kill := gen.InsertKillParams{
			VictimKind:        0,
			VictimCharacterID: pgtype.Int8{Int64: req.Character.ID, Valid: true},
			PosX:              req.DeathPosX,
			PosY:              req.DeathPosY,
			PosZ:              req.DeathPosZ,
		}
		switch req.Killer.Kind {
		case DeathEntryKillerCharacter:
			kill.KillerKind = 0
			kill.KillerCharacterID = pgtype.Int8{Int64: req.Killer.CharacterID, Valid: true}
		case DeathEntryKillerMob:
			kill.KillerKind = 1
			kill.KillerMobID = pgtype.Int4{Int32: req.Killer.MobID, Valid: true}
		}
		if _, err := q.InsertKill(ctx, kill); err != nil {
			return DeathEntryResult{}, fmt.Errorf("store: commit death entry kill: %w", err)
		}
	}

	return DeathEntryResult{
		CharacterRevision: charRev,
		CorpseID:          corpse.ID,
		ItemRevisions:     itemRevs,
	}, nil
}

// CommitDeathEntry atomically persists one immediate death entry
// (spec §9.5.8a, §8.1/§8.3): the already-resolved post-death
// character snapshot, zero or more caller-resolved ground item
// relocations at the death position (normal drops and/or the
// Token-death special relocation, each with optional PK protection),
// exactly one generated corpse row, the pending-death recovery row,
// and the kills audit row when the killer is auditable. ZERO ledger
// rows are written. Lock/CAS order is character root first, then
// item roots ascending ItemID. Any stale root rolls back everything
// and maps to ErrStaleRevision (counted once); a pending-death PK
// replay maps to ErrDeathAlreadyPending (never counted as stale).
func (s *PGStore) CommitDeathEntry(ctx context.Context, req DeathEntryRequest) (DeathEntryResult, error) {
	if err := validateDeathEntryRequest(req); err != nil {
		return DeathEntryResult{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DeathEntryResult{}, fmt.Errorf("store: commit death entry: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	res, err := commitDeathEntryTx(ctx, tx, req)
	if err != nil {
		var stale *deathCASStale
		if errors.As(err, &stale) {
			s.recordStale(stale.aggregate)
			return DeathEntryResult{}, fmt.Errorf(
				"store: commit death entry %s id=%d expected revision=%d: %w",
				stale.aggregate, stale.id, stale.expected, ErrStaleRevision)
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
			pgErr.ConstraintName == "pending_deaths_pkey" {
			return DeathEntryResult{}, fmt.Errorf(
				"store: commit death entry character=%d: %w", req.Character.ID, ErrDeathAlreadyPending)
		}
		return DeathEntryResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return DeathEntryResult{}, fmt.Errorf("store: commit death entry: commit: %w", err)
	}
	return res, nil
}
