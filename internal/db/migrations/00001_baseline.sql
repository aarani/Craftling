-- Baseline schema. Written idempotently (IF NOT EXISTS) so it applies cleanly
-- both to fresh databases and to ones created before goose was adopted, where
-- these tables already exist from the old apply-on-startup schema.

-- +goose Up
CREATE TABLE IF NOT EXISTS users (
    id            TEXT PRIMARY KEY,
    email         TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL DEFAULT 'user',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Idempotent for databases created before the role column existed.
ALTER TABLE users ADD COLUMN IF NOT EXISTS role TEXT NOT NULL DEFAULT 'user';

CREATE TABLE IF NOT EXISTS refresh_tokens (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_refresh_tokens_user_id ON refresh_tokens (user_id);

CREATE TABLE IF NOT EXISTS game_servers (
    id             TEXT PRIMARY KEY,
    owner_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name           TEXT NOT NULL,
    game           TEXT NOT NULL DEFAULT 'minecraft',
    version        TEXT NOT NULL,
    cpus           INT NOT NULL DEFAULT 2,
    memory_mb      INT NOT NULL DEFAULT 2048,
    desired_state  TEXT NOT NULL DEFAULT 'running',
    status         TEXT NOT NULL DEFAULT 'pending',
    vm_id          TEXT,
    host           TEXT,
    port           INT,
    status_message TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at     TIMESTAMPTZ
);

-- Idempotent for databases created before soft deletes existed.
ALTER TABLE game_servers ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_game_servers_owner_id ON game_servers (owner_id);

-- +goose Down
DROP TABLE IF EXISTS game_servers;
DROP TABLE IF EXISTS refresh_tokens;
DROP TABLE IF EXISTS users;
