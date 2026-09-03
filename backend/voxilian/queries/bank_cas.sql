-- M1-T7c: bank aggregate CAS statement (spec §8.1).
-- ONLY this file may contain UPDATE banks. Single-row CAS needs no
-- surrounding transaction; future trade composition uses the private
-- seam in store code. No generic bank updater, no DELETE (bank deletion
-- semantics are unspecified).

-- name: CASUpdateBankBalance :one
UPDATE banks
SET
    balance = sqlc.arg(balance),
    revision = sqlc.arg(expected_revision)::bigint + 1
WHERE character_id = sqlc.arg(character_id)
  AND system = sqlc.arg(system)
  AND revision = sqlc.arg(expected_revision)::bigint
RETURNING revision;
