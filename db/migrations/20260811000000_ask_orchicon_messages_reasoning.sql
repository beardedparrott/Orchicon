-- Ask Orchicon: persist assistant reasoning chunks on messages.
--
-- Reasoning (thinking) parts unwrapped from the SSE bus are accumulated per
-- turn and stored as a JSON array of strings (one entry per reasoning part,
-- boundaries preserved). Additive and forward-only: existing rows default to
-- an empty array. RLS is untouched — the existing tenant_isolation policy
-- already scopes the row (no new table).
ALTER TABLE ask_orchicon_messages
  ADD COLUMN IF NOT EXISTS reasoning jsonb NOT NULL DEFAULT '[]';
