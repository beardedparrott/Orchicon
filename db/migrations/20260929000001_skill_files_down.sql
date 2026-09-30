-- Reverses 20260929000001_skill_files.sql: the skill_files path arrays on
-- projects, ask_orchicon_conversations and worker_versions.
--
-- Drops only the columns this migration added. The pre-existing free-text
-- `skills` columns are NOT touched — they predate this migration and are a
-- different field (see the forward migration's header).
ALTER TABLE "projects"
  DROP COLUMN IF EXISTS "skill_files";

ALTER TABLE "ask_orchicon_conversations"
  DROP COLUMN IF EXISTS "skill_files";

ALTER TABLE "worker_versions"
  DROP COLUMN IF EXISTS "skill_files";
