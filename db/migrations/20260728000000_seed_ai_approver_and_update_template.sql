-- Seed AI Approver worker and update the template workflow to use the
-- new approval features: no separate loop decision step, approval step
-- handles loop-back natively, project→work-item dependency order fixed.

-- ═══════════════════════════════════════════════════════════════════
-- 1. AI Approver worker
-- ═══════════════════════════════════════════════════════════════════

INSERT INTO workers (id, tenant_id, name, slug, description, purpose, status, current_version, created_by)
VALUES
  ('w_se_ai_approver', 'tnt_dev', 'AI Approver', 'ai-approver',
   'An AI-based approval authority that reviews upstream context and decides whether work meets the bar for acceptance.',
   '', 'draft', 0, 'orchicon');

INSERT INTO worker_versions (id, tenant_id, worker_id, version, version_note, status,
  runtime_ref, model_ref, role, skills, behavior, agents_md,
  context_sources, permissions, gated_tools, budget_overrides, execution_policy_ref,
  concurrency_limit, recovery_workflow_ref, labels)
VALUES
  ('wv_se_ai_approver_v1', 'tnt_dev', 'w_se_ai_approver', 1, 'Pre-canned worker', 'draft',
   'opencode', 'opencode/deepseek-v4-flash-free',
   'You are the final approval authority. Review the upstream context, diff, and acceptance criteria. Your job is to decide whether the work is ready to ship or needs to go back for rework.',
   'Code review • Quality assessment • Acceptance criteria verification • Risk evaluation • Final sign-off',
   'Be thorough and objective. Consider the acceptance criteria, code quality, test coverage, and any edge cases. Explain your reasoning clearly before giving your decision.',
   E'## Your role\nYou are the final say. Based on the context provided, determine if an approval is warranted.\n\n## Decision format\nAt the end of your review, output a decision signal on its own line:\n\n```\n_decision: approved\n```\nor\n```\n_decision: rejected\n```\n\nIf rejected, explain specifically what needs to be fixed before the next review cycle.',
   '[]', '{}', '[]', '{}', '', 1, '', '{}');

-- ═══════════════════════════════════════════════════════════════════
-- 2. Update the template workflow to use approval native loop-back
--    and correct project→work-item dependency ordering.
-- ═══════════════════════════════════════════════════════════════════

-- Remove the old template workflow version and header so we can
-- re-seed with the corrected steps.
DELETE FROM workflow_step_runs
WHERE workflow_run_id IN (SELECT id FROM workflow_runs WHERE workflow_id = 'wf_feature_approval_demo');
DELETE FROM workflow_runs WHERE workflow_id = 'wf_feature_approval_demo';
DELETE FROM workflow_versions WHERE workflow_id = 'wf_feature_approval_demo';
DELETE FROM workflows WHERE id = 'wf_feature_approval_demo';

-- Re-create with corrected DAG:
--   Project → Work Item → SSE → PR Reviewer → Approval (loop-back to SSE on rejection)
--   → DevOps Engineer
--
-- The approval step config carries loop_branch and max_iterations so
-- the reconciler loops back on rejection without a separate loop_decision step.
INSERT INTO workflows (id, tenant_id, project_id, name, current_version, status, type)
VALUES ('wf_feature_approval_demo', 'tnt_dev', '', 'Feature Development w/ Approval Gate', 1, 'published', 'template');

INSERT INTO workflow_versions (id, tenant_id, workflow_id, version, version_note, status, steps, inputs, outputs)
VALUES ('wfv_feature_approval_demo_v1', 'tnt_dev', 'wf_feature_approval_demo', 1,
  'Template workflow demonstrating approval gate with native loop-back and optional AI or human reviewer',
  'published',
  '[
    {"id":"step-project","name":"Project","kind":"project","ref":"","worker_version":0,"depends_on":[],"gate_policy_ref":"","config":"","position_x":50,"position_y":200},
    {"id":"step-work-item","name":"Work Item","kind":"work_item","ref":"","worker_version":0,"depends_on":["step-project"],"gate_policy_ref":"","config":"","position_x":250,"position_y":200},
    {"id":"step-sse","name":"Senior Software Engineer","kind":"task","ref":"w_se_senior_software_engineer","worker_version":1,"depends_on":["step-work-item"],"gate_policy_ref":"","config":"","position_x":450,"position_y":200},
    {"id":"step-pr-reviewer","name":"PR Reviewer","kind":"task","ref":"w_se_pr_reviewer","worker_version":1,"depends_on":["step-sse"],"gate_policy_ref":"","config":"","position_x":650,"position_y":200},
    {"id":"step-approval","name":"Approval Gate","kind":"approval","ref":"","worker_version":0,"depends_on":["step-pr-reviewer"],"gate_policy_ref":"","config":"{\"reviewer\":\"human\",\"loop_branch\":\"step-sse\",\"max_iterations\":5}","position_x":850,"position_y":200},
    {"id":"step-devops","name":"DevOps Engineer","kind":"task","ref":"w_se_devops_engineer","worker_version":1,"depends_on":["step-approval"],"gate_policy_ref":"","config":"","position_x":1050,"position_y":200}
  ]'::jsonb,
  '{}'::jsonb,
  '{}'::jsonb);
