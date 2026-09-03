-- Reverse of 20260917000000_canonicalize_model_refs: strip the default
-- adapter prefix where it was added by the forward migration. This is
-- best-effort (canonical refs that existed before the forward migration
-- are indistinguishable from rewritten ones and are left intact).
--
-- A reversed ref is "canonical minus the 'opencode/' prefix" where the
-- remainder does NOT itself start with a registered adapter kind.

UPDATE worker_versions
SET model_ref = substr(model_ref, length('opencode/') + 1)
WHERE model_ref LIKE 'opencode/%'
  AND split_part(substr(model_ref, length('opencode/') + 1), '/', 1)
      NOT IN ('opencode', 'claude', 'orchicon');

UPDATE tenant_settings
SET default_worker_model = substr(default_worker_model, length('opencode/') + 1)
WHERE default_worker_model LIKE 'opencode/%'
  AND split_part(substr(default_worker_model, length('opencode/') + 1), '/', 1)
      NOT IN ('opencode', 'claude', 'orchicon');

UPDATE tenant_settings
SET default_ask_orchicon_model = substr(default_ask_orchicon_model, length('opencode/') + 1)
WHERE default_ask_orchicon_model LIKE 'opencode/%'
  AND split_part(substr(default_ask_orchicon_model, length('opencode/') + 1), '/', 1)
      NOT IN ('opencode', 'claude', 'orchicon');