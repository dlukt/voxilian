-- M1-T6a: account + character root queries (spec §8).
-- No generic character UPDATE/DELETE here by design: mutable aggregate
-- writes belong to M1-T7a CAS. Reads + slot-claimed insert only.

-- name: CreateAccount :one
INSERT INTO accounts (keycloak_sub, email)
VALUES ($1, $2)
RETURNING *;

-- name: GetAccountByID :one
SELECT * FROM accounts WHERE id = $1;

-- name: GetAccountByKeycloakSub :one
SELECT * FROM accounts WHERE keycloak_sub = $1;

-- name: ListLiveCharactersByAccount :many
SELECT * FROM characters
WHERE account_id = $1 AND deleted_at IS NULL
ORDER BY slot, id;

-- name: GetCharacterByID :one
SELECT * FROM characters WHERE id = $1;

-- name: GetLiveCharacterForAccount :one
SELECT * FROM characters
WHERE id = $1 AND account_id = $2 AND deleted_at IS NULL;

-- name: InsertCharacter :one
INSERT INTO characters (
    account_id, slot, name, gender, face,
    might, intellect, stamina, agility, mysticism, aim,
    karma, hometown, pos_x, pos_y, pos_z, vitals, advancement, flags
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, $9, $10, $11,
    $12, $13, $14, $15, $16, $17, $18, $19
)
RETURNING *;
