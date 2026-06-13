-- Per-server environment (P4): record the env vars a server boots with.
--
-- env holds the resolved "KEY=VALUE" entries the control plane derived from a
-- marketplace template's answers (EULA acceptance, game mode, difficulty, …).
-- The agent merges them over the image's own OCI env and publishes the result
-- to the guest via MMDS, so the same immutable squashfs rootfs can boot with
-- per-server configuration without baking it into the image. Defaults to the
-- empty array so existing rows — and servers created without a template — keep
-- the image's stock environment.

-- +goose Up
ALTER TABLE game_servers ADD COLUMN IF NOT EXISTS env TEXT[] NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE game_servers DROP COLUMN IF EXISTS env;
