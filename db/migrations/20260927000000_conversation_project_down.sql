-- Reverses 20260927000000_conversation_project.sql: the conversation/project association and its index.
DROP INDEX IF EXISTS "ask_orchicon_conversations_project_idx";

ALTER TABLE "ask_orchicon_conversations"
  DROP COLUMN IF EXISTS "project_id";
