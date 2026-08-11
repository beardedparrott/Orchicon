-- Ask Orchicon conversation mode (Task 4): the per-conversation persona.
--
-- mode selects the per-message system prompt applied to opencode turns:
-- 'brainstorm' (default, open systems-thinking partner) or 'orchicon'
-- (strictly-governed platform expert). The mode is read at turn-dispatch time
-- and applied per message via the opencode per-turn `system` field, so a mode
-- switch needs no session change or serve restart — the same session carries
-- the new persona on the next message.
--
-- Additive + forward-only: existing (and absent) rows default to 'brainstorm'
-- — no backfill needed. RLS is untouched: the existing tenant_isolation
-- policy already scopes the row (additive column on a covered table).

ALTER TABLE ask_orchicon_conversations
  ADD COLUMN IF NOT EXISTS mode text NOT NULL DEFAULT 'brainstorm';
