-- Strengthen DevOps Engineer Behavior with role-scoping so it never
-- writes application code (complements the workflow-aware prompt context).

UPDATE worker_versions
SET behavior = 'Create private repos by default unless told otherwise. PR and merge when work is passed to you after approval. Your job is repository management and deployment operations — never write application code yourself. Leave implementation to the engineer, reviewing to the reviewer, and testing to the QA engineer.'
WHERE worker_id = 'w_se_devops_engineer';
