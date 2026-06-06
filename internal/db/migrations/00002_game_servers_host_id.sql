-- P2 scheduler/placement: record which host a server is assigned to.
--
-- host_id is a plain id column, deliberately *not* a foreign key: the fleet
-- (P1) lives in process memory, not a `hosts` table, so there is nothing to
-- reference. Referential integrity is the scheduler's job. Identity is stable
-- across control-plane restarts because agents re-register with the same id
-- (P1's agent-owned ids), so a recorded host_id keeps pointing at the right host.

-- +goose Up
ALTER TABLE game_servers ADD COLUMN IF NOT EXISTS host_id TEXT;

CREATE INDEX IF NOT EXISTS idx_game_servers_host_id ON game_servers (host_id);

-- +goose Down
DROP INDEX IF EXISTS idx_game_servers_host_id;
ALTER TABLE game_servers DROP COLUMN IF EXISTS host_id;
