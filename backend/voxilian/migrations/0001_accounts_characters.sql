-- M1-T1: accounts + characters + citext + live-character uniqueness.
-- Later milestones own: catalogs (M1-T2), spells/skills (M1-T3),
-- items/corpses/banks (M1-T4), ledger/kills/bans (M1-T5), and the six
-- 1..50 stat CHECKs (M1-T3). Nothing beyond M1-T1 scope lives here.

-- +goose Up
CREATE EXTENSION IF NOT EXISTS citext;

CREATE TABLE accounts (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    keycloak_sub TEXT NOT NULL UNIQUE,
    email CITEXT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE characters (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    account_id BIGINT NOT NULL REFERENCES accounts (id),
    slot SMALLINT NOT NULL CHECK (slot IN (0, 1)),
    name CITEXT NOT NULL,
    gender SMALLINT NOT NULL,
    face JSONB NOT NULL,
    might SMALLINT NOT NULL,
    intellect SMALLINT NOT NULL,
    stamina SMALLINT NOT NULL,
    agility SMALLINT NOT NULL,
    mysticism SMALLINT NOT NULL,
    aim SMALLINT NOT NULL,
    karma INT NOT NULL,
    hometown TEXT NOT NULL,
    pos_x BIGINT NOT NULL,
    pos_y BIGINT NOT NULL,
    pos_z BIGINT NOT NULL,
    vitals JSONB NOT NULL,
    advancement JSONB NOT NULL,
    flags INT NOT NULL,
    revision BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ NULL
);

CREATE INDEX characters_account_id_idx ON characters (account_id);

CREATE UNIQUE INDEX chars_acct_slot_uidx
    ON characters (account_id, slot)
    WHERE deleted_at IS NULL;

CREATE UNIQUE INDEX chars_name_uidx
    ON characters (name)
    WHERE deleted_at IS NULL;

-- +goose Down
DROP TABLE IF EXISTS characters;
DROP TABLE IF EXISTS accounts;
DROP EXTENSION IF EXISTS citext;
