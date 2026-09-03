-- M1-T5: audit ledger, kills, account sanctions (spec §8).
-- Ledger/kills are audit/event rows, NOT aggregate roots: no revisions.
-- Bans/mutes are account-scoped current state (no history table in MVP).
-- "Append-only" is an application/role contract, not SQL triggers.

-- +goose Up
CREATE TABLE ledger (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind SMALLINT NOT NULL,
    actor_account_id BIGINT NULL REFERENCES accounts (id),
    actor_character_id BIGINT NULL REFERENCES characters (id),
    cpty_account_id BIGINT NULL REFERENCES accounts (id),
    cpty_character_id BIGINT NULL REFERENCES characters (id),
    amount BIGINT NULL,
    qty INTEGER NULL,
    item_id BIGINT NULL REFERENCES item_instances (id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ledger_actor_identity_check CHECK (
        num_nonnulls(actor_account_id, actor_character_id) = 1
    ),
    CONSTRAINT ledger_counterparty_identity_check CHECK (
        num_nonnulls(cpty_account_id, cpty_character_id) <= 1
    )
);

CREATE INDEX ledger_actor_character_created_idx
    ON ledger (actor_character_id, created_at);

CREATE INDEX ledger_actor_account_created_idx
    ON ledger (actor_account_id, created_at);

CREATE TABLE kills (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    killer_kind SMALLINT NOT NULL,
    killer_character_id BIGINT NULL REFERENCES characters (id),
    killer_mob_id INTEGER NULL REFERENCES mob_protos (id),
    victim_kind SMALLINT NOT NULL,
    victim_character_id BIGINT NULL REFERENCES characters (id),
    victim_mob_id INTEGER NULL REFERENCES mob_protos (id),
    pos_x BIGINT NOT NULL,
    pos_y BIGINT NOT NULL,
    pos_z BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT kills_killer_identity_check CHECK (
        (killer_kind = 0 AND killer_character_id IS NOT NULL
            AND killer_mob_id IS NULL) OR
        (killer_kind = 1 AND killer_character_id IS NULL
            AND killer_mob_id IS NOT NULL)
    ),
    CONSTRAINT kills_victim_identity_check CHECK (
        (victim_kind = 0 AND victim_character_id IS NOT NULL
            AND victim_mob_id IS NULL) OR
        (victim_kind = 1 AND victim_character_id IS NULL
            AND victim_mob_id IS NOT NULL)
    )
);

CREATE INDEX kills_victim_character_created_idx
    ON kills (victim_character_id, created_at);

CREATE TABLE bans (
    account_id BIGINT PRIMARY KEY REFERENCES accounts (id),
    reason TEXT NOT NULL,
    expires_at TIMESTAMPTZ NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE mutes (
    account_id BIGINT PRIMARY KEY REFERENCES accounts (id),
    reason TEXT NOT NULL,
    expires_at TIMESTAMPTZ NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS mutes;
DROP TABLE IF EXISTS bans;
DROP TABLE IF EXISTS kills;
DROP TABLE IF EXISTS ledger;
