-- Remove explicit ORCHICON WORKER SUMMARY / _decision format instructions
-- from canned worker AGENTS.md fields. These are now injected automatically
-- by buildCompositePrompt for every worker dispatch.
--
-- We match by worker_id (not version-specific ID) to catch any version
-- that may have been created through the API or previous migrations.

-- Senior Software Engineer: strip the Output format section, keep project conventions.
UPDATE worker_versions
SET agents_md = E'## Project conventions\n- Run tests with `go test ./...` or `npm test`\n- Run lint with `go vet ./...` and `npm run lint`\n- Keep merge commits clean: squash before merging\n- Every new feature needs tests\n- Write small, focused commits\n\n## Build & verify\n```bash\nmake ci        # full gate (lint + codegen + vet + test)\nmake build     # compile binary\nmake migrate   # apply DB migrations\nmake up        # start dev stack\n```'
WHERE worker_id = 'w_se_senior_software_engineer';

-- PR Reviewer: strip the Output format section entirely.
UPDATE worker_versions SET agents_md = '' WHERE worker_id = 'w_se_pr_reviewer';

-- QA Engineer: strip the Output format section entirely.
UPDATE worker_versions SET agents_md = '' WHERE worker_id = 'w_se_qa_engineer';

-- AI Approver: replace the custom Decision format with standard behavior note.
UPDATE worker_versions
SET agents_md = E'## Your role\nYou are the final say. Based on the context provided, determine if an approval is warranted.\n\nIf rejected, explain specifically what needs to be fixed before the next review cycle.'
WHERE worker_id = 'w_se_ai_approver';
