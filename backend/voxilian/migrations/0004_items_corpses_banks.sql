-- M1-T4: item aggregate root + locations, corpses, banks (spec §8).
-- item_instances.revision guards the root AND its item_locations row;
-- banks.revision guards each balance root; corpses are not CAS roots.
-- No CAS logic here (M1-T7b/T7c). No gameplay positivity/overdraft
-- rules, no cascades, no triggers.

-- +goose Up
CREATE TABLE item_instances (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    proto INTEGER NOT NULL REFERENCES item_protos (id),
    qty INTEGER NOT NULL,
    hits INTEGER NOT NULL,
    enchants JSONB NOT NULL,
    revision BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE corpses (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    character_id BIGINT NOT NULL REFERENCES characters (id),
    pos_x BIGINT NOT NULL,
    pos_y BIGINT NOT NULL,
    pos_z BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX corpses_expires_at_idx ON corpses (expires_at);

CREATE TABLE banks (
    character_id BIGINT NOT NULL REFERENCES characters (id),
    system TEXT NOT NULL,
    balance BIGINT NOT NULL,
    revision BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (character_id, system)
);

CREATE TABLE item_locations (
    item_id BIGINT PRIMARY KEY REFERENCES item_instances (id),
    kind SMALLINT NOT NULL,
    character_id BIGINT NULL REFERENCES characters (id),
    corpse_id BIGINT NULL REFERENCES corpses (id),
    container_item_id BIGINT NULL REFERENCES item_instances (id),
    vault_region TEXT NULL,
    pos_x BIGINT NULL,
    pos_y BIGINT NULL,
    pos_z BIGINT NULL,
    slot TEXT NULL,
    CONSTRAINT item_locations_kind_state_check CHECK (
        (kind = 0 AND character_id IS NOT NULL AND slot IS NOT NULL
            AND corpse_id IS NULL AND container_item_id IS NULL
            AND vault_region IS NULL
            AND pos_x IS NULL AND pos_y IS NULL AND pos_z IS NULL) OR
        (kind = 1 AND pos_x IS NOT NULL AND pos_y IS NOT NULL
            AND pos_z IS NOT NULL
            AND character_id IS NULL AND corpse_id IS NULL
            AND container_item_id IS NULL AND vault_region IS NULL
            AND slot IS NULL) OR
        (kind = 2 AND corpse_id IS NOT NULL AND character_id IS NULL
            AND container_item_id IS NULL AND vault_region IS NULL
            AND slot IS NULL
            AND pos_x IS NULL AND pos_y IS NULL AND pos_z IS NULL) OR
        (kind = 3 AND character_id IS NOT NULL AND vault_region IS NOT NULL
            AND slot IS NOT NULL AND corpse_id IS NULL
            AND container_item_id IS NULL
            AND pos_x IS NULL AND pos_y IS NULL AND pos_z IS NULL) OR
        (kind = 4 AND container_item_id IS NOT NULL AND slot IS NOT NULL
            AND character_id IS NULL AND corpse_id IS NULL
            AND vault_region IS NULL
            AND pos_x IS NULL AND pos_y IS NULL AND pos_z IS NULL)
    ),
    CONSTRAINT item_locations_no_self_container_check CHECK (
        container_item_id IS NULL OR container_item_id <> item_id
    )
);

CREATE INDEX item_locations_character_corpse_idx
    ON item_locations (character_id, corpse_id);

-- +goose Down
DROP TABLE IF EXISTS item_locations;
DROP TABLE IF EXISTS banks;
DROP TABLE IF EXISTS corpses;
DROP TABLE IF EXISTS item_instances;
