-- Reverse of 20261105000000_local_integration_verification.sql.
--
-- Drops the verification columns. The git-level truth is untouched either way: branches, commits and the
-- integration branch are unaffected by this migration in either direction, so a downgrade cannot lose WORK — only
-- the record of whether the result was ever proven. `verified` and `failed` rows keep their status text; the
-- pre-verification code (which knows only pending/merged/conflicted/skipped) will treat the unknown values as
-- work still outstanding, which is the safe direction: it re-examines rather than assumes success.
ALTER TABLE "local_integrations" DROP COLUMN IF EXISTS "verified_at";
ALTER TABLE "local_integrations" DROP COLUMN IF EXISTS "verification_detail";
