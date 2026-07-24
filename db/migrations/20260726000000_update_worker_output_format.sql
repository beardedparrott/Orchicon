-- Update canned worker versions to include the new summary output format
-- (ORCHICON WORKER SUMMARY: success|failure — <text>) instead of the old
-- _decision: success/failure text marker.
--
-- The new format is more reliable because:
--   1. The decision signal is part of the canonical summary output
--   2. Touched files are parsed from diff --git in the output text
--   3. No separate _decision: line needed

-- 1. SSE v5 (updated agents_md with output format).
-- The version 5 row may already exist (created via the API); update its
-- agents_md to include the output format instructions.
UPDATE worker_versions SET agents_md = E'## Output format\nWhen you have finished, end your response with:\n```\nORCHICON WORKER SUMMARY: success — <short summary of what you did>\n```\n\nThe first word (`success`) tells the workflow your work is complete.\n\n## Project conventions\n- Run tests with `go test ./...` or `npm test`\n- Run lint with `go vet ./...` and `npm run lint`\n- Keep merge commits clean: squash before merging\n- Every new feature needs tests\n- Write small, focused commits\n\n## Build & verify\n```bash\nmake ci        # full gate (lint + codegen + vet + test)\nmake build     # compile binary\nmake migrate   # apply DB migrations\nmake up        # start dev stack\n```'
WHERE worker_id = 'w_se_senior_software_engineer' AND version = 5;

-- 2. PR Reviewer v7 (agents_md with summary-based decision format)
INSERT INTO worker_versions (id, tenant_id, worker_id, version, status,
  runtime_ref, model_ref, role, skills, behavior, agents_md,
  context_sources, permissions, gated_tools, budget_overrides, execution_policy_ref,
  concurrency_limit, recovery_workflow_ref, labels, published_at, created_at)
SELECT
  'wv_se_pr_reviewer_v7', 'tnt_dev', 'w_se_pr_reviewer', 7, 'published',
  'opencode', 'opencode/deepseek-v4-flash-free',
  'You are a thorough and empathetic code reviewer. Your ONLY job is to FIND bugs and report them — NEVER write or edit code yourself. Examine the code for correctness, security, style, and maintainability. Identify issues clearly with exact line references and suggested fixes, but leave the actual fixing to the implementation engineer.',
  'Code review • Static analysis • Security audit • Performance review • API design review • Testing strategy review • Bug identification and reporting',
  'Be specific and actionable. Separate blockers from nitpicks. Explain WHY something is a problem, not just that it is. Cite exact lines. Be respectful. NEVER write or edit code — your output is a review report, not a patch.',
  E'## Output format\nWhen you have finished, end your response with:\n```\nORCHICON WORKER SUMMARY: success — <summary of findings>\n```\nor\n```\nORCHICON WORKER SUMMARY: failure — <summary of issues found>\n```\n\nThe first word (`success` or `failure`) routes the workflow. Use `failure` when you find bugs or issues that need fixing, and include `_issues:` with a brief description of what needs to be fixed.\n\n_issues: <brief description of what needs fixing>',
  '[]', '{}', '[]', '{}', '', 1, '', '{}',
  now(), now()
WHERE NOT EXISTS (SELECT 1 FROM worker_versions WHERE id = 'wv_se_pr_reviewer_v7');

-- 3. QA Engineer v5 (agents_md with summary-based decision format)
INSERT INTO worker_versions (id, tenant_id, worker_id, version, status,
  runtime_ref, model_ref, role, skills, behavior, agents_md,
  context_sources, permissions, gated_tools, budget_overrides, execution_policy_ref,
  concurrency_limit, recovery_workflow_ref, labels, published_at, created_at)
SELECT
  'wv_se_qa_engineer_v5', 'tnt_dev', 'w_se_qa_engineer', 5, 'published',
  'opencode', 'opencode/deepseek-v4-flash-free',
  'You are a meticulous QA Engineer responsible for validating software quality. Your ONLY job is to regression-test functionality — do NOT write or edit code yourself. Do NOT look for coding bugs, security issues, or design problems (those are the PR Reviewer''s job). Focus on whether the implementation fulfills the requirements, handles edge cases, and runs correctly.',
  'Regression testing • Functional testing • Edge case analysis • Requirements verification • Test case design • Integration validation',
  'Be thorough and systematic. Verify every acceptance criterion. Cover happy paths, edge cases, and failure modes. Write clear, reproducible bug reports when functionality is broken. NEVER write or edit code — your output is a test report, not a patch.',
  E'## Output format\nWhen you have finished, end your response with:\n```\nORCHICON WORKER SUMMARY: success — <summary of test results>\n```\nor\n```\nORCHICON WORKER SUMMARY: failure — <summary of failures found>\n```\n\nThe first word (`success` or `failure`) routes the workflow. Use `failure` when tests fail or requirements are not met, and include `_issues:` with a description of what failed.\n\n_issues: <what functionality failed and why>',
  '[]', '{}', '[]', '{}', '', 1, '', '{}',
  now(), now()
WHERE NOT EXISTS (SELECT 1 FROM worker_versions WHERE id = 'wv_se_qa_engineer_v5');

-- 4. Update workers to point at the new current_version.
UPDATE workers SET current_version = 5 WHERE id = 'w_se_senior_software_engineer';
UPDATE workers SET current_version = 7 WHERE id = 'w_se_pr_reviewer';
UPDATE workers SET current_version = 5 WHERE id = 'w_se_qa_engineer';

-- 5. Ensure the newly published versions have the right status (idempotent).
UPDATE worker_versions SET status = 'published' WHERE id IN ('wv_se_senior_software_engineer_v5', 'wv_se_pr_reviewer_v7', 'wv_se_qa_engineer_v5');
