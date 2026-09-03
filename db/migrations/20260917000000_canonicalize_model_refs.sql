-- Canonicalize pre-namespace model refs (ADR-0003 adapter/provider/model).
--
-- The adapter namespace introduced a left-greedy grammar in which segment 1
-- of a 3+ segment ref is the ADAPTER KIND (opencode/claude/orchicon). Refs
-- written before the namespace was free-form provider/<model> — including
-- slashed model ids like "commandcode/deepseek/deepseek-v4-flash" — are
-- misread by that grammar: the head becomes a phantom "adapter kind",
-- which poisoned adapter-change validation (workers could not re-save any
-- model ref), dispatch row-kind resolution, and the serve pair.
--
-- This migration rewrites stored refs into canonical form, mirroring
-- adapter.NormalizeRef with the built-in adapter-kind cut exactly:
--
--   no slash          → opencode/<ref>            (bare model id)
--   one slash         → opencode/<ref>            (legacy provider/model)
--   2+ slashes, head NOT in (opencode, claude, orchicon)
--                     → opencode/<ref>            (legacy slashed provider)
--   head IS a kind    → unchanged (already canonical)
--   NULL/empty        → unchanged
--
-- Examples:
--   commandcode/deepseek/deepseek-v4-flash → opencode/commandcode/deepseek/deepseek-v4-flash
--   opencode-go/deepseek-v4-flash          → opencode/opencode-go/deepseek-v4-flash
--   anthropic/claude-4                     → opencode/anthropic/claude-4
--   opencode/anthropic/m                   → unchanged
--
-- The same rewrite applies to the tenant default model refs. Worker-facing
-- validation additionally treats phantom-head refs as legacy data (the
-- identical-ref no-op), so rows this migration cannot foresee (restored
-- backups) still re-save cleanly.

-- worker_versions.model_ref
UPDATE worker_versions
SET model_ref = 'opencode/' || model_ref
WHERE model_ref IS NOT NULL
  AND model_ref <> ''
  AND (
        position('/' in model_ref) = 0
     OR (array_length(string_to_array(model_ref, '/'), 1) = 2
         AND split_part(model_ref, '/', 1) NOT IN ('opencode', 'claude', 'orchicon'))
     OR (array_length(string_to_array(model_ref, '/'), 1) >= 3
         AND split_part(model_ref, '/', 1) NOT IN ('opencode', 'claude', 'orchicon'))
      );

-- tenant_settings.default_worker_model
UPDATE tenant_settings
SET default_worker_model = 'opencode/' || default_worker_model
WHERE default_worker_model IS NOT NULL
  AND default_worker_model <> ''
  AND (
        position('/' in default_worker_model) = 0
     OR (array_length(string_to_array(default_worker_model, '/'), 1) = 2
         AND split_part(default_worker_model, '/', 1) NOT IN ('opencode', 'claude', 'orchicon'))
     OR (array_length(string_to_array(default_worker_model, '/'), 1) >= 3
         AND split_part(default_worker_model, '/', 1) NOT IN ('opencode', 'claude', 'orchicon'))
      );

-- tenant_settings.default_ask_orchicon_model
UPDATE tenant_settings
SET default_ask_orchicon_model = 'opencode/' || default_ask_orchicon_model
WHERE default_ask_orchicon_model IS NOT NULL
  AND default_ask_orchicon_model <> ''
  AND (
        position('/' in default_ask_orchicon_model) = 0
     OR (array_length(string_to_array(default_ask_orchicon_model, '/'), 1) = 2
         AND split_part(default_ask_orchicon_model, '/', 1) NOT IN ('opencode', 'claude', 'orchicon'))
     OR (array_length(string_to_array(default_ask_orchicon_model, '/'), 1) >= 3
         AND split_part(default_ask_orchicon_model, '/', 1) NOT IN ('opencode', 'claude', 'orchicon'))
      );