-- Skill files: a SELECTABLE list of skill paths (the feature), distinct from the
-- free-text `skills` prompt section that already exists on worker_versions and on
-- ask_orchicon_agent_config.
--
-- WHY A NEW COLUMN AND NOT THE EXISTING `skills` TEXT. Two different things share
-- the word "skills" and conflating them is exactly the defect this closes:
--
--   - `skills` (existing, on worker_versions and ask_orchicon_agent_config) is a
--     FREE-TEXT PROMPT SECTION — bullet-style prose a human typed. It stays
--     exactly as it is and is NOT touched here.
--   - `skill_files` (NEW) is a JSONB ARRAY OF ABSOLUTE FILE OR DIRECTORY PATHS,
--     validated by internal/contextfiles exactly like projects.context_files and
--     work_items.context_files, and rendered into the composite prompt by the
--     SAME renderer (contextfiles.RenderManifest) so a skill is a real, on-disk
--     artifact a worker reads on demand rather than prose it is told about.
--
-- The naming is deliberately distinct (`skill_files` vs `skills`) so the two can
-- never be confused again in code, proto, or SQL.
--
-- STORED ON `worker_versions`, NEVER ON `workers`: published versions are edited
-- in place (republish), and the version is the unit every dispatch pins to, so
-- the version is the only correct home for dispatch-affecting content.
--
-- Additive-only (AGENTS.md invariant #9): one NOT NULL column with a default on
-- each table, paired with a _down.sql like every other migration here.
ALTER TABLE "projects"
  ADD COLUMN IF NOT EXISTS "skill_files" jsonb NOT NULL DEFAULT '[]'::jsonb;

ALTER TABLE "ask_orchicon_conversations"
  ADD COLUMN IF NOT EXISTS "skill_files" jsonb NOT NULL DEFAULT '[]'::jsonb;

ALTER TABLE "worker_versions"
  ADD COLUMN IF NOT EXISTS "skill_files" jsonb NOT NULL DEFAULT '[]'::jsonb;

COMMENT ON COLUMN "projects"."skill_files" IS 'Absolute skill file/directory paths selected for this project; rendered into the worker AND Ask prompts by contextfiles.RenderManifest. Distinct from the free-text `skills` prompt section — these are real paths, not prose.';
COMMENT ON COLUMN "ask_orchicon_conversations"."skill_files" IS 'Absolute skill file/directory paths selected for this conversation (union-ed with the project''s at render time); rendered into the Ask system prompt by contextfiles.RenderManifest. Distinct from the free-text `skills` on ask_orchicon_agent_config — these are real paths, not prose.';
COMMENT ON COLUMN "worker_versions"."skill_files" IS 'Absolute skill file/directory paths selected for this worker version (union-ed with the project''s at render time); rendered into the composite worker prompt by contextfiles.RenderManifest. Lives on the VERSION because published versions are immutable-by-version. Distinct from the free-text `skills` prompt section — these are real paths, not prose.';
