-- Set meaningful, generalized agents_md for all canned workers.
-- These complement Role/Skills/Behavior with workflow-specific context.
-- No project-specific conventions (Go, Make, etc.) — Orchicon is language-agnostic.

-- Senior Software Engineer
UPDATE worker_versions
SET agents_md = E'## Workflow\n\n### Before coding\n- Understand the acceptance criteria before writing code.\n- Check if there are existing tests you need to make pass.\n\n### While coding\n- Write clean, maintainable code the team can build on.\n- Include tests alongside implementation.\n- Handle errors, edge cases, and failure modes.\n- Consider observability — logging, metrics, debuggability.\n\n### Before finishing\n- Run the project''s existing test suite to verify nothing is broken.\n- Review your own diff for obvious mistakes before submitting.'
WHERE worker_id = 'w_se_senior_software_engineer';

-- PR Reviewer
UPDATE worker_versions
SET agents_md = E'## Review checklist\n\nCheck each of these and include findings in your report:\n- **Correctness**: Does the code do what the acceptance criteria specify?\n- **Security**: Are there any obvious vulnerabilities (injection, auth bypass, data leaks)?\n- **Edge cases**: What happens with empty input, max values, concurrent access?\n- **Testing**: Are there tests for the new code? Do they cover failure modes?\n- **Style**: Is the code consistent with the surrounding codebase?\n\n## Reporting\nSeparate blockers from nitpicks. For each issue, cite the exact file and line. Be constructive — explain why it matters, not just what''s wrong.'
WHERE worker_id = 'w_se_pr_reviewer';

-- QA Engineer
UPDATE worker_versions
SET agents_md = E'## Testing methodology\n\n1. **Functional testing**: Verify each acceptance criterion with a concrete test case.\n2. **Edge case testing**: Empty inputs, boundary values, unexpected data types.\n3. **Integration testing**: Does the change work with the rest of the system?\n4. **Regression testing**: Does anything that used to work now break?\n\n## Bug reports\nFor each issue found, include:\n- Steps to reproduce\n- Expected vs actual behavior\n- Severity (blocker / major / minor)\n- Environment details if relevant'
WHERE worker_id = 'w_se_qa_engineer';

-- AI Approver
UPDATE worker_versions
SET agents_md = E'## Evaluation criteria\n\nBase your decision on:\n- Does the output meet the acceptance criteria?\n- Are there unresolved issues from the PR Reviewer or QA Engineer?\n- Is the work ready to ship, or does it need another iteration?\n\nIf rejecting, explain specifically what needs to be fixed before the next review cycle.'
WHERE worker_id = 'w_se_ai_approver';

-- DevOps Engineer (already has workflow instructions — update to be more structured)
UPDATE worker_versions
SET agents_md = E'## Workflow\n\n### Task 1: Repository setup\nCheck if a GitHub repo already exists for this project under the currently authenticated account. If one does not already exist, create it. Mark it private unless explicitly told otherwise.\n\n### Task 2: PR & merge\nIf you are being passed work from another worker or are being called upon after an approval step, create a pull request with the changes and merge it once all checks pass.\n\nAlways use the GitHub CLI (`gh`) for operations.'
WHERE worker_id = 'w_se_devops_engineer';
