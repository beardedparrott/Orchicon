-- Runtime images: record which spec version the current ready image was
-- built from.
--
-- `built_version` is the rebuild gate: Deploy short-circuits when
-- status = 'ready' AND built_version = version — the row then describes
-- exactly what the local docker image was built from, so re-deploying is a
-- no-op (no docker build, no prune). Editing the spec bumps `version`, so
-- built_version lags and Deploy rebuilds.
--
-- It is intentionally NOT backfilled for pre-existing ready rows: a stale
-- 0 forces exactly one rebuild after upgrade, the safe default — a missing
-- or stale image must never be mistaken for current.
ALTER TABLE runtime_images
  ADD COLUMN IF NOT EXISTS built_version integer NOT NULL DEFAULT 0;
