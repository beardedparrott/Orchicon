-- Add git branch best-practice instructions to the Principal Software Architect.

UPDATE worker_versions
SET agents_md = agents_md || E'\n\n## Git workflow\n- **NEVER commit directly to `main` or `master`.**\n- Always create a feature or bugfix branch for your work.\n- Use ADRs to document architecture decisions alongside the code changes.'
WHERE worker_id = 'w_se_principal_architect';
