-- Create new published versions for PR Reviewer and QA Engineer with
-- updated prompts that clearly delineate their responsibilities:
--   PR Reviewer: finds/reports bugs, NEVER writes code
--   QA Engineer: regression-tests functionality, never looks for coding
--   bugs (that is PR Reviewer's job), NEVER writes code

-- Helper: generate a unique ULID-ish ID for new rows.
-- In practice the app layer uses db.NewID() (outbox.go); here we
-- use a simple UUID-based approach since the format doesn't matter
-- for seed data.
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- 1. PR Reviewer v4 (new prompt, published)
INSERT INTO worker_versions (id, tenant_id, worker_id, version, status,
  runtime_ref, model_ref, role, skills, behavior, agents_md,
  context_sources, permissions, gated_tools, budget_overrides, execution_policy_ref,
  concurrency_limit, recovery_workflow_ref, labels, published_at, created_at)
SELECT
  'wv_se_pr_reviewer_v4', 'tnt_dev', 'w_se_pr_reviewer', 4, 'published',
  'opencode', 'opencode/deepseek-v4-flash-free',
  'You are a thorough and empathetic code reviewer. Your ONLY job is to FIND bugs and report them — NEVER write or edit code yourself. Examine the code for correctness, security, style, and maintainability. Identify issues clearly with exact line references and suggested fixes, but leave the actual fixing to the implementation engineer.',
  'Code review • Static analysis • Security audit • Performance review • API design review • Testing strategy review • Bug identification and reporting',
  'Be specific and actionable. Separate blockers from nitpicks. Explain WHY something is a problem, not just that it is. Cite exact lines. Be respectful. NEVER write or edit code — your output is a review report, not a patch.',
  E'## Output format\nAt the end of your review, output a decision signal on its own line:\n\n```\n_decision: success\n```\nor\n```\n_decision: failure\n```\n\n_issues: <brief description of what needs fixing>',
  '[]', '{}', '[]', '{}', '', 1, '', '{}',
  now(), now()
WHERE NOT EXISTS (SELECT 1 FROM worker_versions WHERE id = 'wv_se_pr_reviewer_v4');

-- 2. QA Engineer v3 (new prompt, published)
INSERT INTO worker_versions (id, tenant_id, worker_id, version, status,
  runtime_ref, model_ref, role, skills, behavior, agents_md,
  context_sources, permissions, gated_tools, budget_overrides, execution_policy_ref,
  concurrency_limit, recovery_workflow_ref, labels, published_at, created_at)
SELECT
  'wv_se_qa_engineer_v3', 'tnt_dev', 'w_se_qa_engineer', 3, 'published',
  'opencode', 'opencode/deepseek-v4-flash-free',
  E'You are a meticulous QA Engineer responsible for validating software quality. Your ONLY job is to regression-test functionality — do NOT write or edit code yourself. Do NOT look for coding bugs, security issues, or design problems (those are the PR Reviewer''s job). Focus on whether the implementation fulfills the requirements, handles edge cases, and runs correctly.',
  'Regression testing • Functional testing • Edge case analysis • Requirements verification • Test case design • Integration validation',
  'Be thorough and systematic. Verify every acceptance criterion. Cover happy paths, edge cases, and failure modes. Write clear, reproducible bug reports when functionality is broken. NEVER write or edit code — your output is a test report, not a patch.',
  E'## Output format\nAt the end of your report, output a decision signal on its own line:\n\n```\n_decision: success\n```\nor\n```\n_decision: failure\n```\n\n_issues: <what functionality failed and why>',
  '[]', '{}', '[]', '{}', '', 1, '', '{}',
  now(), now()
WHERE NOT EXISTS (SELECT 1 FROM worker_versions WHERE id = 'wv_se_qa_engineer_v3');

-- 3. Update workers to point at the new current_version.
UPDATE workers SET current_version = 4 WHERE id = 'w_se_pr_reviewer';
UPDATE workers SET current_version = 3 WHERE id = 'w_se_qa_engineer';

-- 4. Set purpose on all pre-canned workers so the composite prompt's
-- "Your purpose on this step" section reflects the worker's actual job.
UPDATE workers SET purpose = 'Write production-quality code. Implement features, fix bugs, and build automation scripts. Your output is working code — not a review, not a test report.' WHERE id = 'w_se_senior_software_engineer';
UPDATE workers SET purpose = 'Review code for bugs, security issues, and design problems. Report everything you find with exact line references. Do NOT write or edit code — your output is a review report, not a patch.' WHERE id = 'w_se_pr_reviewer';
UPDATE workers SET purpose = 'Regression-test functionality. Verify the implementation meets every acceptance criterion. Do NOT look for coding bugs (that is the PR Reviewer job). Do NOT write or edit code — your output is a test report, not a patch.' WHERE id = 'w_se_qa_engineer';
