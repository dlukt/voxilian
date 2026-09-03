-- M1-T7a: character aggregate CAS statements (spec §8.1).
-- ONLY this file may contain UPDATE/DELETE on characters. The live
-- snapshot owns gameplay state + children; soft-delete is its own root
-- CAS. No blind setters, no physical DELETE, no undelete.

-- name: CASUpdateCharacterSnapshot :one
UPDATE characters
SET
    karma = sqlc.arg(karma),
    pos_x = sqlc.arg(pos_x),
    pos_y = sqlc.arg(pos_y),
    pos_z = sqlc.arg(pos_z),
    vitals = sqlc.arg(vitals),
    advancement = sqlc.arg(advancement),
    flags = sqlc.arg(flags),
    updated_at = now(),
    revision = sqlc.arg(expected_revision)::bigint + 1
WHERE id = sqlc.arg(id)
  AND revision = sqlc.arg(expected_revision)::bigint
  AND deleted_at IS NULL
RETURNING revision;

-- name: CASSoftDeleteCharacter :one
UPDATE characters
SET
    deleted_at = now(),
    updated_at = now(),
    revision = sqlc.arg(expected_revision)::bigint + 1
WHERE id = sqlc.arg(id)
  AND revision = sqlc.arg(expected_revision)::bigint
  AND deleted_at IS NULL
RETURNING revision;
