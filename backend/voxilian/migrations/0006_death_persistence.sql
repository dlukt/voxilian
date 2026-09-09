-- M5-T5b1a: durable death persistence (spec §9.5.8a).
-- pending_deaths is CURRENT state keyed by character (child of the
-- character aggregate: no revision column; gameplay writes happen only
-- inside a character-root CAS transaction composed by T5b1b/T5b2).
-- item_pk_protections is CURRENT PK-pointer state keyed by item (child
-- of the item aggregate: writes only beside a successful item-root CAS).
-- The corpses table is deliberately UNCHANGED: corpses remain non-CAS
-- roots and carry no DeathCost/Portal/death-time mechanics.

-- +goose Up
CREATE TABLE pending_deaths (
    character_id BIGINT PRIMARY KEY REFERENCES characters (id),
    effective_cost SMALLINT NOT NULL CONSTRAINT pending_deaths_effective_cost_check CHECK (effective_cost BETWEEN 0 AND 100),
    death_time_seconds BIGINT NOT NULL CONSTRAINT pending_deaths_death_time_check CHECK (death_time_seconds >= 0),
    corpse_id BIGINT NULL REFERENCES corpses (id) ON DELETE SET NULL,
    portal_used BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX pending_deaths_corpse_uidx
    ON pending_deaths (corpse_id)
    WHERE corpse_id IS NOT NULL;

CREATE TABLE item_pk_protections (
    item_id BIGINT PRIMARY KEY REFERENCES item_instances (id),
    victim_character_id BIGINT NOT NULL REFERENCES characters (id),
    expires_at TIMESTAMPTZ NOT NULL
);

-- +goose Down
DROP TABLE IF EXISTS item_pk_protections;
DROP TABLE IF EXISTS pending_deaths;
