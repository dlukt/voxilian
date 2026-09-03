-- M1-T7b: item aggregate CAS statements (spec §8.1).
-- ONLY this file may contain UPDATE item_instances. Root CAS first,
-- then (for container edges) advisory serialization + ancestry check,
-- then location upsert — all in one transaction. No generic item
-- updater, no item DELETE, no proto mutation.

-- name: CASUpdateItemSnapshot :one
UPDATE item_instances
SET
    qty = sqlc.arg(qty),
    hits = sqlc.arg(hits),
    enchants = sqlc.arg(enchants),
    revision = sqlc.arg(expected_revision)::bigint + 1
WHERE id = sqlc.arg(id)
  AND revision = sqlc.arg(expected_revision)::bigint
RETURNING revision;

-- name: LockItemContainmentGraph :exec
SELECT pg_advisory_xact_lock(sqlc.arg(lock_key));

-- WouldCreateItemContainmentCycle follows destination-container ancestry
-- through kind=4 location rows. True when the destination IS the moving
-- item, an ancestor equals it, or the ancestry is already cyclic.
-- The visited array guarantees termination even on corrupt cyclic data:
-- a cyclic branch emits exactly one cycle=true row and stops expanding.
-- name: WouldCreateItemContainmentCycle :one
WITH RECURSIVE chain(current_id, visited, cycle) AS (
    SELECT
        sqlc.arg(destination_container_id)::bigint,
        ARRAY[sqlc.arg(destination_container_id)::bigint],
        false
    UNION
    SELECT
        l.container_item_id,
        chain.visited || l.container_item_id,
        (l.container_item_id = ANY (chain.visited))
    FROM item_locations l
    JOIN chain ON l.item_id = chain.current_id
    WHERE l.kind = 4
      AND l.container_item_id IS NOT NULL
      AND NOT chain.cycle
)
SELECT EXISTS (
    SELECT 1 FROM chain
    WHERE cycle OR current_id = sqlc.arg(moving_item_id)::bigint
);
