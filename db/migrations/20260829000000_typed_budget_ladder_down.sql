-- Reverse 20260829000000_typed_budget_ladder: drop the typed budget columns.
-- The legacy default_budget_overrides jsonb column is retained (it was never
-- dropped), so no data is lost for code that still reads the blob.

ALTER TABLE tenant_settings
  DROP COLUMN IF EXISTS budget_tokens,
  DROP COLUMN IF EXISTS budget_cost_usd,
  DROP COLUMN IF EXISTS budget_tool_call_count,
  DROP COLUMN IF EXISTS budget_wall_clock_seconds,
  DROP COLUMN IF EXISTS budget_compact_max_turns,
  DROP COLUMN IF EXISTS budget_warn_frac_tokens,
  DROP COLUMN IF EXISTS budget_escalate_frac_tokens,
  DROP COLUMN IF EXISTS budget_final_frac_tokens,
  DROP COLUMN IF EXISTS budget_warn_frac_cost_usd,
  DROP COLUMN IF EXISTS budget_escalate_frac_cost_usd,
  DROP COLUMN IF EXISTS budget_final_frac_cost_usd,
  DROP COLUMN IF EXISTS budget_warn_frac_tool_call_count,
  DROP COLUMN IF EXISTS budget_escalate_frac_tool_call_count,
  DROP COLUMN IF EXISTS budget_final_frac_tool_call_count,
  DROP COLUMN IF EXISTS budget_warn_frac_wall_clock_seconds,
  DROP COLUMN IF EXISTS budget_escalate_frac_wall_clock_seconds,
  DROP COLUMN IF EXISTS budget_final_frac_wall_clock_seconds,
  DROP COLUMN IF EXISTS budget_warn_msg_tokens,
  DROP COLUMN IF EXISTS budget_escalate_msg_tokens,
  DROP COLUMN IF EXISTS budget_final_msg_tokens,
  DROP COLUMN IF EXISTS budget_warn_msg_cost_usd,
  DROP COLUMN IF EXISTS budget_escalate_msg_cost_usd,
  DROP COLUMN IF EXISTS budget_final_msg_cost_usd,
  DROP COLUMN IF EXISTS budget_warn_msg_tool_call_count,
  DROP COLUMN IF EXISTS budget_escalate_msg_tool_call_count,
  DROP COLUMN IF EXISTS budget_final_msg_tool_call_count,
  DROP COLUMN IF EXISTS budget_warn_msg_wall_clock_seconds,
  DROP COLUMN IF EXISTS budget_escalate_msg_wall_clock_seconds,
  DROP COLUMN IF EXISTS budget_final_msg_wall_clock_seconds;
