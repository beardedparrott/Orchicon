-- Fix worker agents_md: the previous migration (20260730000000) targeted
-- version-specific IDs that don't match the actual rows. Match by worker_id
-- instead to hit all versions of each worker.

UPDATE worker_versions SET agents_md = '' WHERE worker_id = 'w_se_senior_software_engineer';

UPDATE worker_versions SET agents_md = '' WHERE worker_id = 'w_se_pr_reviewer';

UPDATE worker_versions SET agents_md = '' WHERE worker_id = 'w_se_qa_engineer';

UPDATE worker_versions
SET agents_md = E'## Your role\nYou are the final say. Based on the context provided, determine if an approval is warranted.\n\nIf rejected, explain specifically what needs to be fixed before the next review cycle.'
WHERE worker_id = 'w_se_ai_approver';
