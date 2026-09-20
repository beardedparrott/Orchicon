-- A conversation belongs to a PROJECT: the second, higher level of organization.
--
-- The operator: "We need to make a second higher level in organization for conversations. It should be
-- another drop down where all of the conversations are associated with Projects in a parent category. For
-- every project that is created (active or otherwise), there should be a list that can be dragged to and also
-- created from. ... Also we should add context to all three modes to know which chat belongs to which project
-- folder."
--
-- CATEGORIES ALREADY EXIST AND ARE A DIFFERENT AXIS. `category_assignments` groups conversations by a
-- tenant-defined label ("Software Development"), and that grouping is orthogonal to the operator's ask: the
-- PROJECT is the workspace a chat's work happens in — its project_dir is the directory the Ask file/shell
-- suite is scoped to — so it is a fact about the conversation, not a label someone applied to it. That is why
-- this is a COLUMN and not a row in category_assignments: only one project can be in force at a time, and it
-- has to be readable on the conversation row itself by every path that renders one.
--
-- '' MEANS UNASSIGNED, and is the default, so every existing row and every ordinary create keeps its current
-- behaviour — nothing to backfill, nothing to re-render. The id is stored as PLAIN TEXT with NO FOREIGN KEY,
-- deliberately: a project can be archived (and, for the ephemeral tenant, hard-deleted) and a conversation
-- must survive that as an unassigned chat rather than becoming undeletable or cascading away. The API layer
-- validates that a project id EXISTS when one is set; a stale id renders as an unknown project and can be
-- cleared, which is a strictly better failure than a broken conversation.
--
-- Additive-only (AGENTS.md invariant #9): one NOT NULL column with a default, plus an index. Follows the
-- _down.sql pairing convention of every other migration here.
ALTER TABLE "ask_orchicon_conversations"
  ADD COLUMN IF NOT EXISTS "project_id" TEXT NOT NULL DEFAULT '';

-- The rail and the GUI sidebar both list conversations GROUPED BY project, so the grouping needs an index —
-- and so does the "which conversations are in this project" read that a project folder's count comes from.
CREATE INDEX IF NOT EXISTS "ask_orchicon_conversations_project_idx"
  ON "ask_orchicon_conversations" ("tenant_id", "project_id", "updated_at" DESC);
