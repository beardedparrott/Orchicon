-- Add git branch discipline to SSE Behavior so it treats it as identity, not just context.

UPDATE worker_versions
SET behavior = behavior || E'\n\nGit discipline:\n- Commit early and often with clear messages.\n- NEVER commit directly to `main` or `master`.\n- Always create a feature or bugfix branch.\n- Use the existing branch from the previous iteration if one exists — do not create a new branch on re-work.'
WHERE worker_id = 'w_se_senior_software_engineer';
