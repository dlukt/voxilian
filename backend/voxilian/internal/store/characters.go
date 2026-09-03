package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/dlukt/voxilian/internal/store/gen"
	"github.com/jackc/pgx/v5/pgconn"
)

// Character-create conflict sentinels. Exact machine-readable strings;
// callers match with errors.Is and may still recover the underlying
// *pgconn.PgError with errors.As.
var (
	ErrSlotOccupied = errors.New("slot_occupied")
	ErrNameTaken    = errors.New("name_taken")
)

// createCharacter inserts a character root row via the generated
// InsertCharacter query. The INSERT plus the partial unique indexes are
// the authority for slot/name claiming: no SELECT-then-INSERT preflight
// (race-prone) happens here. Unique violations on the two live-character
// indexes map to sentinels; every other error passes through unchanged.
func createCharacter(ctx context.Context, q *gen.Queries, arg gen.InsertCharacterParams) (gen.Character, error) {
	ch, err := q.InsertCharacter(ctx, arg)
	if err == nil {
		return ch, nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		switch pgErr.ConstraintName {
		case "chars_acct_slot_uidx":
			return gen.Character{}, fmt.Errorf("store: create character: %w: %w", ErrSlotOccupied, pgErr)
		case "chars_name_uidx":
			return gen.Character{}, fmt.Errorf("store: create character: %w: %w", ErrNameTaken, pgErr)
		}
	}
	return gen.Character{}, err
}
