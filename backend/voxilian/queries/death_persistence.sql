-- M5-T5b1a: low-level death-persistence primitives (spec §9.5.8a).
-- Building blocks for the T5b1b/T5b2a/T5b2b transactions ONLY. No public Store
-- mutation may call these outside a character-root CAS transaction
-- (pending_deaths) or an item-root CAS transaction (item_pk_protections):
-- pending death without the character/corpse transaction, or PK
-- protection without item CAS, is forbidden. The ONE permitted
-- pending-cost UPDATE is the T5b2a lowers-only Portal mutation below
-- (spec §9.5.10a); no other UPDATE of either table exists.

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

-- name: ApplyPendingDeathPortal :one
UPDATE pending_deaths
SET effective_cost = LEAST(
        effective_cost,
        sqlc.arg(proposed_cost)::smallint
    ),
    portal_used = TRUE
WHERE character_id = sqlc.arg(character_id)
  AND corpse_id = sqlc.arg(corpse_id)
  AND portal_used = FALSE
RETURNING *;

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
