-- M1-T3: character ability children + six 1..50 stat CHECKs (spec §8).
-- Ability rows are aggregate children of characters (spec §8.1): no
-- revision columns, no timestamps, no separate IDs. spell_id/skill_id
-- are plain INTEGER FKs; the 1..65535 namespace is guaranteed by the
-- referenced catalog PK rows, so no duplicate range CHECK and no domain.
-- Stat-sum <= 200 is gameplay logic, NOT a DB constraint.

-- +goose Up
CREATE TABLE character_spells (
    character_id BIGINT NOT NULL REFERENCES characters (id),
    spell_id INTEGER NOT NULL REFERENCES spell_protos (id),
    ability SMALLINT NOT NULL CHECK (ability BETWEEN 1 AND 99),
    atrophy_flag BOOLEAN NOT NULL,
    PRIMARY KEY (character_id, spell_id)
);

CREATE TABLE character_skills (
    character_id BIGINT NOT NULL REFERENCES characters (id),
    skill_id INTEGER NOT NULL REFERENCES skill_protos (id),
    ability SMALLINT NOT NULL CHECK (ability BETWEEN 1 AND 99),
    atrophy_flag BOOLEAN NOT NULL,
    PRIMARY KEY (character_id, skill_id)
);

ALTER TABLE characters ADD CONSTRAINT characters_might_check CHECK (might BETWEEN 1 AND 50);
ALTER TABLE characters ADD CONSTRAINT characters_intellect_check CHECK (intellect BETWEEN 1 AND 50);
ALTER TABLE characters ADD CONSTRAINT characters_stamina_check CHECK (stamina BETWEEN 1 AND 50);
ALTER TABLE characters ADD CONSTRAINT characters_agility_check CHECK (agility BETWEEN 1 AND 50);
ALTER TABLE characters ADD CONSTRAINT characters_mysticism_check CHECK (mysticism BETWEEN 1 AND 50);
ALTER TABLE characters ADD CONSTRAINT characters_aim_check CHECK (aim BETWEEN 1 AND 50);

-- +goose Down
DROP TABLE IF EXISTS character_spells;
DROP TABLE IF EXISTS character_skills;
ALTER TABLE characters DROP CONSTRAINT IF EXISTS characters_might_check;
ALTER TABLE characters DROP CONSTRAINT IF EXISTS characters_intellect_check;
ALTER TABLE characters DROP CONSTRAINT IF EXISTS characters_stamina_check;
ALTER TABLE characters DROP CONSTRAINT IF EXISTS characters_agility_check;
ALTER TABLE characters DROP CONSTRAINT IF EXISTS characters_mysticism_check;
ALTER TABLE characters DROP CONSTRAINT IF EXISTS characters_aim_check;
