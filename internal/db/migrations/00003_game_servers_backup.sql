-- P5b/P5c world persistence: on-demand and tracked world backups.
--
-- backup_requested is a user-set flag the reconciler honors (the reconciler is
-- the sole writer of compute side effects, so the API never snapshots directly):
-- it takes a live snapshot via the agent, then clears the flag and stamps
-- last_backup_at. Both default-safe so the column add is non-disruptive.

-- +goose Up
ALTER TABLE game_servers ADD COLUMN IF NOT EXISTS backup_requested BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE game_servers ADD COLUMN IF NOT EXISTS last_backup_at TIMESTAMPTZ;

-- +goose Down
ALTER TABLE game_servers DROP COLUMN IF EXISTS last_backup_at;
ALTER TABLE game_servers DROP COLUMN IF EXISTS backup_requested;
