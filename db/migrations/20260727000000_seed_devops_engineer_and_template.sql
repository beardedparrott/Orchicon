-- Seed DevOps Engineer worker and a template workflow that demonstrates
-- the human-in-the-loop approval gate with loop-back on rejection.
--
-- The workflow follows this DAG:
--   Project → Work Item → Senior Software Engineer → PR Reviewer
--   → Human Approval → Loop Decision → DevOps Engineer
--
-- If the loop decision finds _decision: "rejected" in the work item's
-- results, it loops back to the Senior Software Engineer step (up to 5
-- iterations). If "approved", it proceeds to the DevOps Engineer who
-- creates the repo and PRs/merges.

-- ═══════════════════════════════════════════════════════════════════
-- 1. DevOps Engineer worker
-- ═══════════════════════════════════════════════════════════════════

INSERT INTO workers (id, tenant_id, name, slug, description, purpose, status, current_version, created_by)
VALUES
  ('w_se_devops_engineer', 'tnt_dev', 'DevOps Engineer', 'devops-engineer',
   'A master of GitOps who manages GitHub repositories, creates pull requests, and merges code after approval.',
   '', 'draft', 0, 'orchicon');

INSERT INTO worker_versions (id, tenant_id, worker_id, version, version_note, status,
  runtime_ref, model_ref, role, skills, behavior, agents_md,
  context_sources, permissions, gated_tools, budget_overrides, execution_policy_ref,
  concurrency_limit, recovery_workflow_ref, labels)
VALUES
  ('wv_se_devops_engineer_v1', 'tnt_dev', 'w_se_devops_engineer', 1, 'Pre-canned worker', 'draft',
   'opencode', 'opencode/deepseek-v4-flash-free',
   'You are a DevOps Engineer and master of GitOps. You manage GitHub repositories, create pull requests, and merge code after human approval.',
   'Git • GitHub • GitOps • CI/CD • PR management • Repository management • GitHub CLI • GitHub Actions',
   'Create private repos by default unless told otherwise. PR and merge when work is passed to you after approval.',
   E'## Workflow\n\n### Task 1: Repository Setup\nCheck if a GitHub repo already exists for this project under the currently authenticated account. If one does not already exist, create it. Mark it private unless explicitly told otherwise.\n\n### Task 2: PR & Merge\nIf you are being passed work from another worker or are being called upon after a human approval step, create a pull request with the changes and merge it once all checks pass.\n\nAlways use the GitHub CLI (`gh`) for operations.',
   '[]', '{}', '[]', '{}', '', 1, '', '{}');

-- ═══════════════════════════════════════════════════════════════════
-- 2. Template workflow: "Feature Development w/ Approval Gate"
-- ═══════════════════════════════════════════════════════════════════

INSERT INTO workflows (id, tenant_id, project_id, name, current_version, status, type)
VALUES ('wf_feature_approval_demo', 'tnt_dev', '', 'Feature Development w/ Approval Gate', 1, 'published', 'template');

INSERT INTO workflow_versions (id, tenant_id, workflow_id, version, version_note, status, steps, inputs, outputs)
VALUES ('wfv_feature_approval_demo_v1', 'tnt_dev', 'wf_feature_approval_demo', 1,
  'Template workflow demonstrating human approval gate with loop-back on rejection and DevOps Engineer final step',
  'published',
  '[
    {"id":"step-project","name":"Project","kind":"project","ref":"","worker_version":0,"depends_on":[],"gate_policy_ref":"","config":"","position_x":50,"position_y":200},
    {"id":"step-work-item","name":"Work Item","kind":"work_item","ref":"","worker_version":0,"depends_on":[],"gate_policy_ref":"","config":"","position_x":250,"position_y":200},
    {"id":"step-sse","name":"Senior Software Engineer","kind":"task","ref":"w_se_senior_software_engineer","worker_version":1,"depends_on":["step-project","step-work-item"],"gate_policy_ref":"","config":"","position_x":450,"position_y":200},
    {"id":"step-pr-reviewer","name":"PR Reviewer","kind":"task","ref":"w_se_pr_reviewer","worker_version":1,"depends_on":["step-sse"],"gate_policy_ref":"","config":"","position_x":650,"position_y":200},
    {"id":"step-approval","name":"Human Approval","kind":"approval","ref":"","worker_version":0,"depends_on":["step-pr-reviewer"],"gate_policy_ref":"","config":"","position_x":850,"position_y":200},
    {"id":"step-loop-decision","name":"Approval Loop","kind":"loop_decision","ref":"","worker_version":0,"depends_on":["step-approval"],"gate_policy_ref":"","config":"{\"loop_branch\":\"step-sse\",\"max_iterations\":5,\"success_value\":\"approved\",\"failure_value\":\"rejected\"}","position_x":1050,"position_y":200},
    {"id":"step-devops","name":"DevOps Engineer","kind":"task","ref":"w_se_devops_engineer","worker_version":1,"depends_on":["step-loop-decision"],"gate_policy_ref":"","config":"","position_x":1250,"position_y":200}
  ]'::jsonb,
  '{}'::jsonb,
  '{}'::jsonb);
