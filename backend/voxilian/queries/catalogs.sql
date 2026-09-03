-- M1-T6d: prototype catalog registry primitives (spec §8.2).
-- ALL production catalog-table access lives in this file (guarded).
-- Version rules (no-op / conflict / newer / forced downgrade) are
-- implemented by the Store batch upsert, NOT by these statements alone.
-- Gameplay consumers MUST use the in-memory registry, never these
-- queries on hot paths.

-- name: ListAllSpellProtos :many
SELECT * FROM spell_protos
ORDER BY id;

-- name: ListAllSkillProtos :many
SELECT * FROM skill_protos
ORDER BY id;

-- name: ListAllItemProtos :many
SELECT * FROM item_protos
ORDER BY id;

-- name: ListAllMobProtos :many
SELECT * FROM mob_protos
ORDER BY id;

-- name: ListAllShopListings :many
SELECT * FROM shop_listings
ORDER BY vendor_id, listing;

-- name: InsertSpellProtoIfAbsent :one
INSERT INTO spell_protos (
    id, school, level, mana, exertion, cast_ms, min_hp,
    outlaw, harmful, reagents, params, version
)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
ON CONFLICT (id) DO NOTHING
RETURNING *;

-- name: GetSpellProtoForUpdate :one
SELECT * FROM spell_protos
WHERE id = $1
FOR UPDATE;

-- Exact-match params for the ::jsonb casts surface as Column10/Column11
-- in generated Go (positional casts have no inferable name); stable.
-- name: SpellProtoExactMatch :one
SELECT
    school = $2
    AND level = $3
    AND mana = $4
    AND exertion = $5
    AND cast_ms = $6
    AND min_hp = $7
    AND outlaw = $8
    AND harmful = $9
    AND reagents = $10::jsonb
    AND params = $11::jsonb
    AND version = $12
FROM spell_protos
WHERE id = $1;

-- name: UpdateSpellProto :one
UPDATE spell_protos SET
    school = $2, level = $3, mana = $4, exertion = $5,
    cast_ms = $6, min_hp = $7, outlaw = $8, harmful = $9,
    reagents = $10, params = $11, version = $12
WHERE id = $1
RETURNING *;

-- name: InsertSkillProtoIfAbsent :one
INSERT INTO skill_protos (id, division, level, exertion, params, version)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (id) DO NOTHING
RETURNING *;

-- name: GetSkillProtoForUpdate :one
SELECT * FROM skill_protos
WHERE id = $1
FOR UPDATE;

-- name: SkillProtoExactMatch :one
SELECT
    division = $2
    AND level = $3
    AND exertion = $4
    AND params = $5::jsonb
    AND version = $6
FROM skill_protos
WHERE id = $1;

-- name: UpdateSkillProto :one
UPDATE skill_protos SET
    division = $2, level = $3, exertion = $4, params = $5, version = $6
WHERE id = $1
RETURNING *;

-- name: InsertItemProtoIfAbsent :one
INSERT INTO item_protos (id, kind, slot, base, version)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (id) DO NOTHING
RETURNING *;

-- name: GetItemProtoForUpdate :one
SELECT * FROM item_protos
WHERE id = $1
FOR UPDATE;

-- name: ItemProtoExactMatch :one
SELECT
    kind = $2
    AND slot IS NOT DISTINCT FROM $3
    AND base = $4::jsonb
    AND version = $5
FROM item_protos
WHERE id = $1;

-- name: UpdateItemProto :one
UPDATE item_protos SET
    kind = $2, slot = $3, base = $4, version = $5
WHERE id = $1
RETURNING *;

-- name: InsertMobProtoIfAbsent :one
INSERT INTO mob_protos (
    id, key, level, difficulty, karma, atk, resists, spells, loot_tid, version
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (id) DO NOTHING
RETURNING *;

-- name: GetMobProtoForUpdate :one
SELECT * FROM mob_protos
WHERE id = $1
FOR UPDATE;

-- name: MobProtoExactMatch :one
SELECT
    key = $2
    AND level = $3
    AND difficulty = $4
    AND karma = $5
    AND atk = $6::jsonb
    AND resists = $7::jsonb
    AND spells = $8::jsonb
    AND loot_tid IS NOT DISTINCT FROM $9
    AND version = $10
FROM mob_protos
WHERE id = $1;

-- name: UpdateMobProto :one
UPDATE mob_protos SET
    key = $2, level = $3, difficulty = $4, karma = $5,
    atk = $6, resists = $7, spells = $8, loot_tid = $9, version = $10
WHERE id = $1
RETURNING *;

-- name: ListShopListingsByVendor :many
SELECT *
FROM shop_listings
WHERE vendor_id = $1
ORDER BY listing;

-- name: DeleteShopListingsByVendor :exec
DELETE FROM shop_listings
WHERE vendor_id = $1;

-- name: InsertShopListing :exec
INSERT INTO shop_listings (
    vendor_id,
    listing,
    item_proto,
    price,
    qty
)
VALUES ($1, $2, $3, $4, $5);
