-- M1-T6c: audit appends + sanction current-state primitives (spec §8).
-- Ledger/kills are append-only: INSERT (+ SELECT by ID only where the
-- tests need it — no list/history APIs until a consumer requires them).
-- Bans/mutes are single-row current state: get/upsert/revoke. Expiry
-- interpretation belongs to later enforcement code, never to SQL here.

-- name: InsertLedger :one
INSERT INTO ledger (
    kind,
    actor_account_id,
    actor_character_id,
    cpty_account_id,
    cpty_character_id,
    amount,
    qty,
    item_id
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: InsertKill :one
INSERT INTO kills (
    killer_kind,
    killer_character_id,
    killer_mob_id,
    victim_kind,
    victim_character_id,
    victim_mob_id,
    pos_x,
    pos_y,
    pos_z
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetBan :one
SELECT *
FROM bans
WHERE account_id = $1;

-- name: UpsertBan :one
INSERT INTO bans (
    account_id,
    reason,
    expires_at
)
VALUES ($1, $2, $3)
ON CONFLICT (account_id) DO UPDATE SET
    reason = EXCLUDED.reason,
    expires_at = EXCLUDED.expires_at,
    created_at = EXCLUDED.created_at
RETURNING *;

-- name: DeleteBan :exec
DELETE FROM bans
WHERE account_id = $1;

-- name: GetMute :one
SELECT *
FROM mutes
WHERE account_id = $1;

-- name: UpsertMute :one
INSERT INTO mutes (
    account_id,
    reason,
    expires_at
)
VALUES ($1, $2, $3)
ON CONFLICT (account_id) DO UPDATE SET
    reason = EXCLUDED.reason,
    expires_at = EXCLUDED.expires_at,
    created_at = EXCLUDED.created_at
RETURNING *;

-- name: DeleteMute :exec
DELETE FROM mutes
WHERE account_id = $1;
