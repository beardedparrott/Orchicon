-- Ask Orchicon conversation sessions (Task 1): persist the opencode serve
-- session id on the conversation so follow-up turns reuse the same session
-- instead of spawning a per-message `opencode run` subprocess.
--
-- session_id is server-managed: never accepted from the client, opaque to
-- the API surface, and NOT a tenant-scope column (tenant scope stays on
-- tenant_id, so RLS is untouched). Empty default -> no backfill; a
-- conversation that never chatted (or predates this field) has '' and
-- creates a session lazily on its first message.

ALTER TABLE ask_orchicon_conversations
  ADD COLUMN IF NOT EXISTS session_id text NOT NULL DEFAULT '';
