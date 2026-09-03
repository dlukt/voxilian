-- M1-T6b: item/corpse/bank primitives (spec §8).
-- Reads, root inserts, corpse expiry scan/delete (corpses are not CAS
-- roots), and the low-level UpsertItemLocation building block.
--
-- UpsertItemLocation on an EXISTING item belongs ONLY inside the M1-T7b
-- root-CAS transaction (UPDATE item_instances ... revision = expected+1
-- WHERE revision = expected, then mutate location in the same txn).
-- Nothing in T6b may move an existing item outside that future path.

-- name: GetItemInstanceByID :one
SELECT *
FROM item_instances
WHERE id = $1;

-- name: GetItemLocationByItemID :one
SELECT *
FROM item_locations
WHERE item_id = $1;

-- name: InsertItemInstance :one
INSERT INTO item_instances (
    proto,
    qty,
    hits,
    enchants
)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: UpsertItemLocation :one
INSERT INTO item_locations (
    item_id,
    kind,
    character_id,
    corpse_id,
    container_item_id,
    vault_region,
    pos_x,
    pos_y,
    pos_z,
    slot
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (item_id) DO UPDATE SET
    kind = EXCLUDED.kind,
    character_id = EXCLUDED.character_id,
    corpse_id = EXCLUDED.corpse_id,
    container_item_id = EXCLUDED.container_item_id,
    vault_region = EXCLUDED.vault_region,
    pos_x = EXCLUDED.pos_x,
    pos_y = EXCLUDED.pos_y,
    pos_z = EXCLUDED.pos_z,
    slot = EXCLUDED.slot
RETURNING *;

-- name: InsertCorpse :one
INSERT INTO corpses (
    character_id,
    pos_x,
    pos_y,
    pos_z,
    expires_at
)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetCorpseByID :one
SELECT *
FROM corpses
WHERE id = $1;

-- name: ListExpiredCorpses :many
SELECT *
FROM corpses
WHERE expires_at <= $1
ORDER BY expires_at, id;

-- name: DeleteCorpse :exec
DELETE FROM corpses
WHERE id = $1;

-- name: GetBank :one
SELECT *
FROM banks
WHERE character_id = $1
  AND system = $2;

-- name: ListBanksByCharacter :many
SELECT *
FROM banks
WHERE character_id = $1
ORDER BY system;

-- name: InsertBank :one
INSERT INTO banks (
    character_id,
    system,
    balance
)
VALUES ($1, $2, $3)
RETURNING *;
