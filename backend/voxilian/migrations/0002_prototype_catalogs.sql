-- M1-T2: prototype catalog tables (spec §8.2).
-- Stable prototype IDs are SOURCE-DEFINED wire IDs: plain INTEGER with
-- an explicit 1..65535 range CHECK. Deliberately NOT SMALLINT (signed,
-- max 32767), NOT SERIAL, NOT identity columns — the database must never
-- generate or constrain the wire namespace beyond the u16 range.
-- Version-upgrade/downgrade rules live in M1-T6d, not in SQL.

-- +goose Up
CREATE TABLE spell_protos (
    id INTEGER NOT NULL PRIMARY KEY CHECK (id BETWEEN 1 AND 65535),
    school SMALLINT NOT NULL,
    level SMALLINT NOT NULL,
    mana INT NOT NULL,
    exertion INT NOT NULL,
    cast_ms INT NOT NULL,
    min_hp INT NOT NULL,
    outlaw BOOL NOT NULL,
    harmful BOOL NOT NULL,
    reagents JSONB NOT NULL,
    params JSONB NOT NULL,
    version INT NOT NULL
);

CREATE TABLE skill_protos (
    id INTEGER NOT NULL PRIMARY KEY CHECK (id BETWEEN 1 AND 65535),
    division SMALLINT NOT NULL,
    level SMALLINT NOT NULL,
    exertion INT NOT NULL,
    params JSONB NOT NULL,
    version INT NOT NULL
);

CREATE TABLE item_protos (
    id INTEGER NOT NULL PRIMARY KEY CHECK (id BETWEEN 1 AND 65535),
    kind SMALLINT NOT NULL,
    -- NULLABLE: reagents, ammo, food, potions and other non-equippable
    -- prototypes have no equipment slot. No empty-string convention.
    slot TEXT NULL,
    base JSONB NOT NULL,
    version INT NOT NULL
);

CREATE TABLE mob_protos (
    id INTEGER NOT NULL PRIMARY KEY CHECK (id BETWEEN 1 AND 65535),
    key TEXT NOT NULL UNIQUE,
    level SMALLINT NOT NULL,
    difficulty SMALLINT NOT NULL,
    karma INT NOT NULL,
    atk JSONB NOT NULL,
    resists JSONB NOT NULL,
    spells JSONB NOT NULL,
    -- NULLABLE: absence of a loot table is a defined state (no loot),
    -- not a magic empty-string value.
    loot_tid TEXT NULL,
    version INT NOT NULL
);

CREATE TABLE shop_listings (
    vendor_id INTEGER NOT NULL REFERENCES mob_protos (id),
    listing INTEGER NOT NULL CHECK (listing BETWEEN 1 AND 65535),
    item_proto INTEGER NOT NULL REFERENCES item_protos (id),
    price BIGINT NOT NULL,
    qty INTEGER NOT NULL,
    PRIMARY KEY (vendor_id, listing)
);

-- +goose Down
DROP TABLE IF EXISTS shop_listings;
DROP TABLE IF EXISTS spell_protos;
DROP TABLE IF EXISTS skill_protos;
DROP TABLE IF EXISTS item_protos;
DROP TABLE IF EXISTS mob_protos;
