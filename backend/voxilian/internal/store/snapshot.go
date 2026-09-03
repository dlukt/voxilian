package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dlukt/voxilian/internal/store/gen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrStaleRevision reports a lost CAS race: the aggregate moved since the
// caller read it. Shared by character/item/bank CAS (single sentinel).
var ErrStaleRevision = errors.New("stale_revision")

func staleRevision(op string, id, expected int64) error {
	return fmt.Errorf("store: %s id=%d expected revision=%d: %w", op, id, expected, ErrStaleRevision)
}

// CharacterSpellSnapshot is one complete spell row of a snapshot.
type CharacterSpellSnapshot struct {
	SpellID     int32
	Ability     int16
	AtrophyFlag bool
}

// CharacterSkillSnapshot is one complete skill row of a snapshot.
type CharacterSkillSnapshot struct {
	SkillID     int32
	Ability     int16
	AtrophyFlag bool
}

// CharacterSnapshot is a COMPLETE gameplay snapshot: nil and empty child
// slices both mean "resulting child set is empty". No patch semantics.
type CharacterSnapshot struct {
	ID               int64
	ExpectedRevision int64

	Karma       int32
	PosX        int64
	PosY        int64
	PosZ        int64
	Vitals      json.RawMessage
	Advancement json.RawMessage
	Flags       int32

	Spells []CharacterSpellSnapshot
	Skills []CharacterSkillSnapshot
}

// SoftDeleteCharacter CAS-deletes a live character: single atomic UPDATE,
// no child touch, revision advance. A second delete (or any racing
// save/delete on the same expected revision) is stale, not idempotent.
func SoftDeleteCharacter(ctx context.Context, pool *pgxpool.Pool, id, expectedRevision int64) (int64, error) {
	q := gen.New(pool)
	newRev, err := q.CASSoftDeleteCharacter(ctx, gen.CASSoftDeleteCharacterParams{
		ExpectedRevision: expectedRevision, ID: id,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, staleRevision("soft delete character", id, expectedRevision)
		}
		return 0, fmt.Errorf("store: soft delete character: %w", err)
	}
	return newRev, nil
}
