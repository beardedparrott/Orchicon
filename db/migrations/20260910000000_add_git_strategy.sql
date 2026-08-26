-- Add git_strategy to projects and workflows.
-- Projects: default 'local' (push branch, no PR) preserves worktree isolation while not requiring GitHub.
-- Workflows: nullable override; NULL means "use project default".
ALTER TABLE "projects" ADD COLUMN "git_strategy" text NOT NULL DEFAULT 'local';
ALTER TABLE "workflows" ADD COLUMN "git_strategy" text NULL;

-- Backfill workflows: leave NULL (inherit project)
-- Add check constraints for valid values
ALTER TABLE "projects" ADD CONSTRAINT "projects_git_strategy_check" CHECK (git_strategy IN ('local','pr','none'));
ALTER TABLE "workflows" ADD CONSTRAINT "workflows_git_strategy_check" CHECK (git_strategy IS NULL OR git_strategy IN ('local','pr','none'));
