-- Backfill the typed budget-ladder columns on tenant_settings from the
-- legacy default_budget_overrides jsonb blob.
--
-- Why: 20260829000000_typed_budget_ladder.sql moved the budget ladder into
-- first-class typed columns (budget_*_frac_*, budget_*_msg_*, budget_compact_*)
-- but only migrated the GATE ceilings (tokens/cost_usd/tool_call_count/
-- wall_clock_seconds/compact_max_turns) out of the jsonb; it seeded the ladder
-- thresholds/messages/compaction toggles via column DEFAULTs. For a tenant row
-- that already held a populated default_budget_overrides at upgrade time, that
-- leaves the typed ladder at whatever the DEFAULTs / a subsequent write set,
-- which can diverge from the operator's configured values that still live in
-- the jsonb. This migration closes that gap by re-deriving every typed budget
-- column from the jsonb for rows that carry one.
--
-- Semantics:
--   * Idempotent — a row where the jsonb already matches the typed columns is
--     rewritten to the same values (COALESCE keeps an existing typed value when
--     the jsonb lacks the corresponding key, so nothing is nulled).
--   * Forward-only — an UPDATE only; no DDL. Rows whose jsonb is empty ({}), or
--     that carry none of the budget keys, are untouched and keep their column
--     DEFAULTs (0.25/0.5/0.75, the named messages, and compact tiers
--     {warn:false, escalate:true, final:true}).
--   * The jsonb remains the wire/API transport; the typed columns are the
--     authoritative source. This migration makes the two agree for pre-existing
--     rows.

UPDATE tenant_settings SET
  budget_tokens              = COALESCE((default_budget_overrides->>'tokens')::double precision, budget_tokens),
  budget_cost_usd            = COALESCE((default_budget_overrides->>'cost_usd')::double precision, budget_cost_usd),
  budget_tool_call_count     = COALESCE((default_budget_overrides->>'tool_call_count')::double precision, budget_tool_call_count),
  budget_wall_clock_seconds  = COALESCE((default_budget_overrides->>'wall_clock_seconds')::double precision, budget_wall_clock_seconds),
  budget_compact_max_turns   = COALESCE((default_budget_overrides->>'compact_max_turns')::double precision, budget_compact_max_turns),

  budget_compact_warn_tier     = COALESCE((default_budget_overrides->'compact_tiers'->>0)::boolean, budget_compact_warn_tier),
  budget_compact_escalate_tier = COALESCE((default_budget_overrides->'compact_tiers'->>1)::boolean, budget_compact_escalate_tier),
  budget_compact_final_tier    = COALESCE((default_budget_overrides->'compact_tiers'->>2)::boolean, budget_compact_final_tier),

  budget_warn_frac_tokens     = COALESCE((default_budget_overrides->'warnings'->'fractions'->'tokens'->>0)::double precision, budget_warn_frac_tokens),
  budget_escalate_frac_tokens = COALESCE((default_budget_overrides->'warnings'->'fractions'->'tokens'->>1)::double precision, budget_escalate_frac_tokens),
  budget_final_frac_tokens    = COALESCE((default_budget_overrides->'warnings'->'fractions'->'tokens'->>2)::double precision, budget_final_frac_tokens),
  budget_warn_frac_cost_usd     = COALESCE((default_budget_overrides->'warnings'->'fractions'->'cost_usd'->>0)::double precision, budget_warn_frac_cost_usd),
  budget_escalate_frac_cost_usd = COALESCE((default_budget_overrides->'warnings'->'fractions'->'cost_usd'->>1)::double precision, budget_escalate_frac_cost_usd),
  budget_final_frac_cost_usd    = COALESCE((default_budget_overrides->'warnings'->'fractions'->'cost_usd'->>2)::double precision, budget_final_frac_cost_usd),
  budget_warn_frac_tool_call_count     = COALESCE((default_budget_overrides->'warnings'->'fractions'->'tool_call_count'->>0)::double precision, budget_warn_frac_tool_call_count),
  budget_escalate_frac_tool_call_count = COALESCE((default_budget_overrides->'warnings'->'fractions'->'tool_call_count'->>1)::double precision, budget_escalate_frac_tool_call_count),
  budget_final_frac_tool_call_count    = COALESCE((default_budget_overrides->'warnings'->'fractions'->'tool_call_count'->>2)::double precision, budget_final_frac_tool_call_count),
  budget_warn_frac_wall_clock_seconds     = COALESCE((default_budget_overrides->'warnings'->'fractions'->'wall_clock_seconds'->>0)::double precision, budget_warn_frac_wall_clock_seconds),
  budget_escalate_frac_wall_clock_seconds = COALESCE((default_budget_overrides->'warnings'->'fractions'->'wall_clock_seconds'->>1)::double precision, budget_escalate_frac_wall_clock_seconds),
  budget_final_frac_wall_clock_seconds    = COALESCE((default_budget_overrides->'warnings'->'fractions'->'wall_clock_seconds'->>2)::double precision, budget_final_frac_wall_clock_seconds),

  budget_warn_msg_tokens     = COALESCE((default_budget_overrides->'warnings'->'messages'->'tokens'->>0), budget_warn_msg_tokens),
  budget_escalate_msg_tokens = COALESCE((default_budget_overrides->'warnings'->'messages'->'tokens'->>1), budget_escalate_msg_tokens),
  budget_final_msg_tokens    = COALESCE((default_budget_overrides->'warnings'->'messages'->'tokens'->>2), budget_final_msg_tokens),
  budget_warn_msg_cost_usd     = COALESCE((default_budget_overrides->'warnings'->'messages'->'cost_usd'->>0), budget_warn_msg_cost_usd),
  budget_escalate_msg_cost_usd = COALESCE((default_budget_overrides->'warnings'->'messages'->'cost_usd'->>1), budget_escalate_msg_cost_usd),
  budget_final_msg_cost_usd    = COALESCE((default_budget_overrides->'warnings'->'messages'->'cost_usd'->>2), budget_final_msg_cost_usd),
  budget_warn_msg_tool_call_count     = COALESCE((default_budget_overrides->'warnings'->'messages'->'tool_call_count'->>0), budget_warn_msg_tool_call_count),
  budget_escalate_msg_tool_call_count = COALESCE((default_budget_overrides->'warnings'->'messages'->'tool_call_count'->>1), budget_escalate_msg_tool_call_count),
  budget_final_msg_tool_call_count    = COALESCE((default_budget_overrides->'warnings'->'messages'->'tool_call_count'->>2), budget_final_msg_tool_call_count),
  budget_warn_msg_wall_clock_seconds     = COALESCE((default_budget_overrides->'warnings'->'messages'->'wall_clock_seconds'->>0), budget_warn_msg_wall_clock_seconds),
  budget_escalate_msg_wall_clock_seconds = COALESCE((default_budget_overrides->'warnings'->'messages'->'wall_clock_seconds'->>1), budget_escalate_msg_wall_clock_seconds),
  budget_final_msg_wall_clock_seconds    = COALESCE((default_budget_overrides->'warnings'->'messages'->'wall_clock_seconds'->>2), budget_final_msg_wall_clock_seconds)
WHERE default_budget_overrides ?| ARRAY['tokens', 'cost_usd', 'tool_call_count', 'wall_clock_seconds', 'compact_max_turns', 'compact_tiers', 'warnings'];
