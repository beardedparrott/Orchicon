-- Remove explicit _decision format instructions from canned worker
-- AGENTS.md fields. The ORCHICON WORKER SUMMARY instruction is now
-- injected automatically by the reconciler for all workers.

-- PR Reviewer: strip the Output format section
UPDATE worker_versions
SET agents_md = ''
WHERE id = 'wv_se_pr_reviewer_v1';

-- QA Engineer: strip the Output format section
UPDATE worker_versions
SET agents_md = ''
WHERE id = 'wv_se_qa_engineer_v1';

-- AI Approver: replace the custom Decision format with standard behavior
UPDATE worker_versions
SET agents_md = E'## Your role\nYou are the final say. Based on the context provided, determine if an approval is warranted.\n\nIf rejected, explain specifically what needs to be fixed before the next review cycle.'
WHERE id = 'wv_se_ai_approver_v1';
