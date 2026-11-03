-- First-class failure reporting on a workflow run.
--
-- WHY A COLUMN AND NOT run_context. A failure reason was being written into run_context, which is (a) opaque in
-- the API — it reaches the client base64-encoded — and (b) semantically wrong: run_context holds the run's
-- INPUTS (pr_url, provenance, integration stamps). A failure is an OUTCOME. Filing it as input is why an
-- operator could look at a failed run and see nothing: the run page rendered no error, and diagnose_failure
-- returned an empty analysis because there was no execution to analyse.
--
-- Reported: a run failed in 3.8s with "could not create the run worktree: ... fatal: invalid reference: HEAD"
-- visible only after decoding RunContext by hand, while all three operator surfaces said nothing — the work
-- item's acceptance review read "no summary recorded", and diagnose_failure returned
-- {"failure_analysis": [], "workflow_executions": null}.
--
-- error_kind is a small vocabulary the UI can group and phrase on ("worktree_preflight", "dispatch",
-- "runtime"), so the page can say what CLASS of thing went wrong without parsing prose. It is NOT an enum
-- constraint: a new kind must be addable without a migration, and an empty kind simply means "unclassified".
ALTER TABLE workflow_runs ADD COLUMN IF NOT EXISTS error_message TEXT NOT NULL DEFAULT '';
ALTER TABLE workflow_runs ADD COLUMN IF NOT EXISTS error_kind TEXT NOT NULL DEFAULT '';
