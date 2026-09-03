-- M1-T6b: character ability primitives (spec §8).
-- Reads plus low-level child-row statements for the later M1-T7a
-- snapshot CAS (root CAS → delete children → insert snapshot, all via
-- q.WithTx). NO handwritten ability setters here: writing existing
-- abilities outside characters.revision CAS is forbidden.

-- name: ListCharacterSpells :many
SELECT *
FROM character_spells
WHERE character_id = $1
ORDER BY spell_id;

-- name: ListCharacterSkills :many
SELECT *
FROM character_skills
WHERE character_id = $1
ORDER BY skill_id;

-- name: DeleteCharacterSpells :exec
DELETE FROM character_spells
WHERE character_id = $1;

-- name: InsertCharacterSpell :exec
INSERT INTO character_spells (
    character_id,
    spell_id,
    ability,
    atrophy_flag
)
VALUES ($1, $2, $3, $4);

-- name: DeleteCharacterSkills :exec
DELETE FROM character_skills
WHERE character_id = $1;

-- name: InsertCharacterSkill :exec
INSERT INTO character_skills (
    character_id,
    skill_id,
    ability,
    atrophy_flag
)
VALUES ($1, $2, $3, $4);
