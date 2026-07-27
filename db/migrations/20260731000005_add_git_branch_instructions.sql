-- Add git branch best-practice instructions to workers.

-- Senior Software Engineer
UPDATE worker_versions
SET agents_md = agents_md || E'\n\n## Git workflow\n- Commit early and often with clear, descriptive messages.\n- **NEVER commit directly to `main` or `master`.**\n- Always create a feature or bugfix branch for your work.\n- Keep commits focused — one logical change per commit.'
WHERE worker_id = 'w_se_senior_software_engineer';

-- DevOps Engineer
UPDATE worker_versions
SET agents_md = agents_md || E'\n\n## Git workflow\n- **NEVER commit directly to `main` or `master`.**\n- Always work off a feature or bugfix branch.\n- PR and merge into `main` only after all checks pass and approvals are granted.'
WHERE worker_id = 'w_se_devops_engineer';
