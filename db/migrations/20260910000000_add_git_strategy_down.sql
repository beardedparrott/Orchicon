ALTER TABLE "projects" DROP CONSTRAINT IF EXISTS "projects_git_strategy_check";
ALTER TABLE "workflows" DROP CONSTRAINT IF EXISTS "workflows_git_strategy_check";
ALTER TABLE "projects" DROP COLUMN IF EXISTS "git_strategy";
ALTER TABLE "workflows" DROP COLUMN IF EXISTS "git_strategy";
