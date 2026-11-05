-- VERIFICATION: a merged ref is not a delivered tree.
--
-- REPORTED, after six runs that all integrated correctly: C:\projects\weatherapp contained .git, .orchicon and
-- .orchicon-worktrees and NOTHING ELSE. `git status` showed 55 staged deletions while `develop` held all 53
-- files, and only `git reset --hard develop` recovered it. The reflog proved the merges were real, the file
-- counts grew monotonically, the tests passed in a fresh checkout — and the operator, looking at an empty
-- directory, reasonably concluded the whole run had failed.
--
-- The record said `merged` and was right about the REF and silent about the TREE. That is the gap: an
-- integration has only succeeded when the result is USABLE, and "the branch moved" is not evidence of that.
--
-- So the row now carries the verification outcome:
--
--	pending -> integrating -> verified | failed
--
-- with `conflicted` (a merge conflict needing a person) and the terminal `skipped`/`adopted` alongside. A `failed`
-- record BLOCKS the project's queue, which was the explicit requirement: "a failing check must not be recorded as
-- success and must not let the next run start".
--
-- `integrating` is written BEFORE the ref moves, so a plane that dies mid-merge leaves a row saying an attempt
-- was interrupted rather than one that looks untouched — state that survives a restart instead of being inferred.
ALTER TABLE "local_integrations"
  ADD COLUMN IF NOT EXISTS "verification_detail" text NOT NULL DEFAULT '';
ALTER TABLE "local_integrations"
  ADD COLUMN IF NOT EXISTS "verified_at" timestamptz;

-- Existing rows that claimed `merged` landed but were never PROVEN. They are left in place rather than
-- rewritten, because the platform treats `merged` as "landed, unverified" and re-examines it — which is also how
-- a project that already has a stale checkout gets repaired instead of quietly staying broken. Nothing here can
-- decide in SQL whether a tree is materialised; only the verification pass can.

-- The queue read now includes the unproven states, so the index has to match what the gate asks for.
DROP INDEX IF EXISTS "local_integrations_queue_idx";
CREATE INDEX IF NOT EXISTS "local_integrations_queue_idx"
  ON "local_integrations" ("tenant_id", "project_id", "status", "created_at");
