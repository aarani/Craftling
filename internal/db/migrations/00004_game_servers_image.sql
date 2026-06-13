-- OCI image rootfs (P4): record the image a server boots from.
--
-- image is the OCI/docker reference and image_digest the pinned manifest
-- digest the control plane resolved at create time. Together they let the
-- agent build a reproducible squashfs rootfs (internal/image) instead of a
-- static per-version ext4 base. Both default to '' so existing rows — and
-- servers created without an image — keep the legacy ext4 path.

-- +goose Up
ALTER TABLE game_servers ADD COLUMN IF NOT EXISTS image TEXT NOT NULL DEFAULT '';
ALTER TABLE game_servers ADD COLUMN IF NOT EXISTS image_digest TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE game_servers DROP COLUMN IF EXISTS image_digest;
ALTER TABLE game_servers DROP COLUMN IF EXISTS image;
