-- Reverse of 20260917000000_canonicalize_model_refs (best-effort).
--
-- A forward-rewritten ref is "opencode/" + the original legacy ref. A
-- ref that was ALREADY canonical before the forward migration has the
-- same shape — the two are structurally indistinguishable in SQL, so the
-- reverse cannot be exact. This down migration therefore targets the
-- UNAMBIGUOUS legacy shapes only: rows whose segment after the
-- "opencode/" prefix is NOT one of the opencode adapter's built-in
-- provider ids (anthropic, openai, local, opencode, opencode-go) — the
-- poisoned legacy population's heads (commandcode, local-models,
-- local-models-fast, tenant customs) are all outside that set, while
-- originally-canonical refs keep their built-in provider heads and stay
-- intact. Collateral (documented, accepted): refs rewritten from a
-- legacy ref whose head coincides with a built-in provider id (e.g.
-- "opencode-go/deepseek-v4-flash" → "opencode/opencode-go/deepseek-
-- v4-flash") are not stripped — the down migration is a dev-rollback
-- convenience, not a data guarantee; migrations are forward-only
-- (developer.md invariant #9) and this file exists to satisfy the paired
-- _down.sql convention.

UPDATE worker_versions
SET model_ref = substr(model_ref, length('opencode/') + 1)
WHERE model_ref LIKE 'opencode/%'
  AND split_part(substr(model_ref, length('opencode/') + 1), '/', 1)
      NOT IN ('anthropic', 'openai', 'local', 'opencode', 'opencode-go');

UPDATE tenant_settings
SET default_worker_model = substr(default_worker_model, length('opencode/') + 1)
WHERE default_worker_model LIKE 'opencode/%'
  AND split_part(substr(default_worker_model, length('opencode/') + 1), '/', 1)
      NOT IN ('anthropic', 'openai', 'local', 'opencode', 'opencode-go');

UPDATE tenant_settings
SET default_ask_orchicon_model = substr(default_ask_orchicon_model, length('opencode/') + 1)
WHERE default_ask_orchicon_model LIKE 'opencode/%'
  AND split_part(substr(default_ask_orchicon_model, length('opencode/') + 1), '/', 1)
      NOT IN ('anthropic', 'openai', 'local', 'opencode', 'opencode-go');