-- SSE's agents_md was left with Project conventions text; clear it entirely.
UPDATE worker_versions SET agents_md = '' WHERE worker_id = 'w_se_senior_software_engineer';
