-- Ask Orchicon: conversational agent tables.
--
-- Stores conversations, messages, and agent configuration per-tenant.
-- All tables carry tenant_id with RLS (invariant #5, AGENTS.md).

-- Conversations: top-level chat sessions.
CREATE TABLE IF NOT EXISTS ask_orchicon_conversations (
  id          text NOT NULL,
  tenant_id   text NOT NULL,
  title       text NOT NULL DEFAULT '',
  model_ref   text NOT NULL DEFAULT '',
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),

  PRIMARY KEY (tenant_id, id)
);

ALTER TABLE ask_orchicon_conversations ENABLE ROW LEVEL SECURITY;
ALTER TABLE ask_orchicon_conversations FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON ask_orchicon_conversations
  FOR ALL
  USING (tenant_id = current_setting('app.tenant_id', true));

-- Messages: individual chat messages within a conversation.
CREATE TABLE IF NOT EXISTS ask_orchicon_messages (
  id              text NOT NULL,
  tenant_id       text NOT NULL,
  conversation_id text NOT NULL,
  role            text NOT NULL CHECK (role IN ('user', 'assistant', 'tool', 'system')),
  content         text NOT NULL DEFAULT '',
  tool_calls      jsonb NOT NULL DEFAULT '[]',
  tool_results    jsonb NOT NULL DEFAULT '[]',
  attachments     jsonb NOT NULL DEFAULT '[]',
  metadata        jsonb NOT NULL DEFAULT '{}',
  created_at      timestamptz NOT NULL DEFAULT now(),

  PRIMARY KEY (tenant_id, id)
);

CREATE INDEX IF NOT EXISTS idx_ask_messages_conversation
  ON ask_orchicon_messages (tenant_id, conversation_id, created_at);

ALTER TABLE ask_orchicon_messages ENABLE ROW LEVEL SECURITY;
ALTER TABLE ask_orchicon_messages FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON ask_orchicon_messages
  FOR ALL
  USING (tenant_id = current_setting('app.tenant_id', true));

-- Agent configuration: Orchicon's system prompt, skills, tools, and
-- behavior definition. One row per tenant (id = 'default').
CREATE TABLE IF NOT EXISTS ask_orchicon_agent_config (
  id                text NOT NULL DEFAULT 'default',
  tenant_id         text NOT NULL,
  system_prompt     text NOT NULL DEFAULT '',
  role              text NOT NULL DEFAULT '',
  skills            text NOT NULL DEFAULT '',
  behavior          text NOT NULL DEFAULT '',
  agents_md         text NOT NULL DEFAULT '',
  tool_definitions  jsonb NOT NULL DEFAULT '[]',
  context_sources   jsonb NOT NULL DEFAULT '[]',
  permissions       jsonb NOT NULL DEFAULT '[]',
  budget_overrides  jsonb NOT NULL DEFAULT '{}',
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),

  PRIMARY KEY (tenant_id, id)
);

ALTER TABLE ask_orchicon_agent_config ENABLE ROW LEVEL SECURITY;
ALTER TABLE ask_orchicon_agent_config FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON ask_orchicon_agent_config
  FOR ALL
  USING (tenant_id = current_setting('app.tenant_id', true));

-- Extend tenant_settings with Ask Orchicon feature flags.
ALTER TABLE tenant_settings
  ADD COLUMN IF NOT EXISTS ask_orchicon_feature_flags jsonb NOT NULL DEFAULT '{}';
