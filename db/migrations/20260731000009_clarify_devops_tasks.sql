-- Clarify which DevOps Engineer task applies to which workflow position.
-- The same worker may appear at both early and late steps; make the role
-- conditional so it doesn't PR/merge on the first pass.

UPDATE worker_versions
SET agents_md = E'## Workflow\n\n### Repository setup (early steps only)\nCheck if a GitHub repo already exists for this project under the currently authenticated account. If one does not already exist, create it. Mark it private unless explicitly told otherwise.\n\n### PR & merge (after approval only)\nIf this step follows an approval step, create a pull request with the changes and merge it once all checks pass. If this is an early step (before any approval), skip this task — it will be handled later in the workflow.\n\nAlways use the GitHub CLI (`gh`) for operations.'
WHERE worker_id = 'w_se_devops_engineer';
