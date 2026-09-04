package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/dlukt/voxilian/internal/store/gen"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// itemLocationInventory is the item_locations.kind for directly owned
// character inventory (migration 0004 per-kind CHECK).
const itemLocationInventory int16 = 0

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

// CreateCharacter persists the complete creation aggregate in ONE
// transaction (spec §8.1/§9): character root, every spell/skill row,
// every starter item instance plus its inventory location. Any failure
// rolls back the root too — no partial rows survive. The root INSERT
// stays the transactional race authority (ErrSlotOccupied /
// ErrNameTaken); no SELECT preflight happens here. Method form for
// Store consumers.
func (s *PGStore) CreateCharacter(ctx context.Context, nc NewCharacter) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: create character: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := gen.New(tx)
	ch, err := createCharacter(ctx, q, gen.InsertCharacterParams{
		AccountID: nc.AccountID, Slot: nc.Slot, Name: nc.Name, Gender: nc.Gender, Face: nc.Face,
		Might: nc.Might, Intellect: nc.Intellect, Stamina: nc.Stamina,
		Agility: nc.Agility, Mysticism: nc.Mysticism, Aim: nc.Aim,
		Karma: nc.Karma, Hometown: nc.Hometown,
		PosX: nc.PosX, PosY: nc.PosY, PosZ: nc.PosZ,
		Vitals: nc.Vitals, Advancement: nc.Advancement, Flags: nc.Flags,
	})
	if err != nil {
		return 0, err
	}
	for _, sp := range nc.Spells {
		if err := q.InsertCharacterSpell(ctx, gen.InsertCharacterSpellParams{
			CharacterID: ch.ID, SpellID: sp.ID, Ability: sp.Ability, AtrophyFlag: false,
		}); err != nil {
			return 0, fmt.Errorf("store: create character: spell %d: %w", sp.ID, err)
		}
	}
	for _, sk := range nc.Skills {
		if err := q.InsertCharacterSkill(ctx, gen.InsertCharacterSkillParams{
			CharacterID: ch.ID, SkillID: sk.ID, Ability: sk.Ability, AtrophyFlag: false,
		}); err != nil {
			return 0, fmt.Errorf("store: create character: skill %d: %w", sk.ID, err)
		}
	}
	for _, it := range nc.Items {
		ench := it.Enchants
		if len(ench) == 0 {
			ench = []byte("{}")
		}
		inst, err := q.InsertItemInstance(ctx, gen.InsertItemInstanceParams{
			Proto: it.ProtoID, Qty: it.Qty, Hits: it.Hits, Enchants: ench,
		})
		if err != nil {
			return 0, fmt.Errorf("store: create character: item proto %d: %w", it.ProtoID, err)
		}
		if _, err := q.UpsertItemLocation(ctx, gen.UpsertItemLocationParams{
			ItemID:      inst.ID,
			Kind:        itemLocationInventory,
			CharacterID: pgtype.Int8{Int64: ch.ID, Valid: true},
			Slot:        pgtype.Text{String: it.Slot, Valid: true},
		}); err != nil {
			return 0, fmt.Errorf("store: create character: item %d location: %w", inst.ID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: create character: commit: %w", err)
	}
	return ch.ID, nil
}

// ListLiveCharacters returns live characters for one account as
// store-domain rows (backing for opcode-216 summaries and slot
// lookup). Method form for Store consumers.
func (s *PGStore) ListLiveCharacters(ctx context.Context, accountID int64) ([]LiveCharacter, error) {
	rows, err := gen.New(s.pool).ListLiveCharactersByAccount(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: list live characters: %w", err)
	}
	out := make([]LiveCharacter, 0, len(rows))
	for _, r := range rows {
		out = append(out, LiveCharacter{
			ID: r.ID, Slot: r.Slot, Name: r.Name, Revision: r.Revision, Vitals: r.Vitals,
		})
	}
	return out, nil
}
