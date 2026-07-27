-- Discourage nitpicking: focus on genuine bugs, not style preferences.

UPDATE worker_versions
SET agents_md = agents_md || E'\n\n## Finding real bugs\nFocus on genuine bugs and correctness issues — not style preferences or cosmetic nits. Ask yourself: "Does this prevent the code from working correctly?" If the answer is no, it is probably not worth reporting. Prefer reporting 2-3 real bugs over 10 nitpicks.'
WHERE worker_id = 'w_se_pr_reviewer';

UPDATE worker_versions
SET agents_md = agents_md || E'\n\n## Finding real bugs\nFocus on functional failures — crashes, incorrect output, missing features. Do not report cosmetic issues or style preferences. A good bug report catches something that would actually break in production, not something that just looks odd.'
WHERE worker_id = 'w_se_qa_engineer';

UPDATE worker_versions
SET behavior = 'Be thorough and systematic. Focus on finding real bugs that break functionality — not cosmetic nits or minor style differences. Verify every acceptance criterion with concrete test cases. Cover happy paths, edge cases, and failure modes. Write clear, reproducible bug reports when functionality is broken. NEVER write or edit code — your output is a test report, not a patch.'
WHERE worker_id = 'w_se_qa_engineer';
