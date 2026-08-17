-- local_credentials.force_password_change: boolean flag that marks a
-- credential as needing a forced password change before the operator can
-- proceed.
--
-- When the bootstrap seeds a fresh plane (or a reset re-arms it) with the
-- built-in default (username="admin", password="admin"), this flag is set
-- to true. The flag rides on the login and session responses; the SPA
-- renders a full-screen change-password gate in place of the app content
-- while it is true. Setting a new password via SetLocalCredential clears
-- the flag. ORCHICON_LOCAL_ADMIN_PASSWORD (a pinned seed or reset) skips
-- the flag entirely.
--
-- Forward-only (ADD COLUMN IF NOT EXISTS — mandatory per AGENTS.md).
-- RLS SQL is unchanged; run `make migrate-hash` after hand-editing.

ALTER TABLE "local_credentials"
  ADD COLUMN IF NOT EXISTS "force_password_change" boolean NOT NULL DEFAULT false;

