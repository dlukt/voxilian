-- M5-T5b1a: low-level death-persistence primitives (spec §9.5.8a).
-- Building blocks for the T5b1b/T5b2 transactions ONLY. No public Store
-- mutation may call these outside a character-root CAS transaction
-- (pending_deaths) or an item-root CAS transaction (item_pk_protections):
-- pending death without the character/corpse transaction, or PK
-- protection without item CAS, is forbidden. No pending-cost UPDATE
-- exists here — the lowers-only Portal mutation belongs to T5b2, which
-- freezes its own update semantics.

-- name: InsertPendingDeath :one
INSERT INTO pending_deaths (
    character_id,
    effective_cost,
    death_time_seconds,
    corpse_id,
    portal_used
)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetPendingDeathByCharacter :one
SELECT *
FROM pending_deaths
WHERE character_id = $1;

-- name: DeletePendingDeathByCharacter :exec
DELETE FROM pending_deaths
WHERE character_id = $1;

-- name: UpsertItemPKProtection :one
INSERT INTO item_pk_protections (
    item_id,
    victim_character_id,
    expires_at
)
VALUES ($1, $2, $3)
ON CONFLICT (item_id) DO UPDATE SET
    victim_character_id = EXCLUDED.victim_character_id,
    expires_at = EXCLUDED.expires_at
RETURNING *;

-- name: GetItemPKProtection :one
SELECT *
FROM item_pk_protections
WHERE item_id = $1;

-- name: DeleteItemPKProtection :exec
DELETE FROM item_pk_protections
WHERE item_id = $1;
