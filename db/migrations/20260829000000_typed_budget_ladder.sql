-- Typed execution-budget ladder columns on tenant_settings.
--
-- Moves the budget ladder (the per-dimension warn/escalate/final thresholds
-- and the message text) from an opaque default_budget_overrides jsonb blob
-- into first-class, typed, per-tenant columns that the Settings UI can edit.
--
-- Semantics:
--   * Gates  (tokens / cost_usd / tool_call_count / wall_clock_seconds /
--             compact_max_turns) are NULLABLE; a stored value is the operator's
--             explicit ceiling, NULL means "use the built-in default". An
--             explicit 0 disables that gate (matching the old JSON contract).
--   * Ladder thresholds (fracs) and messages are NOT NULL DEFAULT so a fresh
--             row lands on the front-loaded 25/50/75 + context-first copy,
--             and existing rows are backfilled to the same values.
--
-- The legacy default_budget_overrides jsonb column is kept (it remains the
-- wire/API transport and a compatibility carrier); these typed columns are
-- the authoritative source for the ladder. The migration migrates the
-- existing gate values out of the jsonb so an operator's configured ceilings
-- are preserved.

ALTER TABLE tenant_settings
  -- Gates (nullable: stored value = operator ceiling, NULL = built-in default,
  -- 0 = disable). Same keys as the old jsonb gate contract.
  ADD COLUMN budget_tokens DOUBLE PRECISION,
  ADD COLUMN budget_cost_usd DOUBLE PRECISION,
  ADD COLUMN budget_tool_call_count DOUBLE PRECISION,
  ADD COLUMN budget_wall_clock_seconds DOUBLE PRECISION,
  ADD COLUMN budget_compact_max_turns DOUBLE PRECISION,

  -- Ladder thresholds (front-loaded 25 / 50 / 75).
  ADD COLUMN budget_warn_frac_tokens DOUBLE PRECISION NOT NULL DEFAULT 0.25,
  ADD COLUMN budget_escalate_frac_tokens DOUBLE PRECISION NOT NULL DEFAULT 0.5,
  ADD COLUMN budget_final_frac_tokens DOUBLE PRECISION NOT NULL DEFAULT 0.75,
  ADD COLUMN budget_warn_frac_cost_usd DOUBLE PRECISION NOT NULL DEFAULT 0.25,
  ADD COLUMN budget_escalate_frac_cost_usd DOUBLE PRECISION NOT NULL DEFAULT 0.5,
  ADD COLUMN budget_final_frac_cost_usd DOUBLE PRECISION NOT NULL DEFAULT 0.75,
  ADD COLUMN budget_warn_frac_tool_call_count DOUBLE PRECISION NOT NULL DEFAULT 0.25,
  ADD COLUMN budget_escalate_frac_tool_call_count DOUBLE PRECISION NOT NULL DEFAULT 0.5,
  ADD COLUMN budget_final_frac_tool_call_count DOUBLE PRECISION NOT NULL DEFAULT 0.75,
  ADD COLUMN budget_warn_frac_wall_clock_seconds DOUBLE PRECISION NOT NULL DEFAULT 0.25,
  ADD COLUMN budget_escalate_frac_wall_clock_seconds DOUBLE PRECISION NOT NULL DEFAULT 0.5,
  ADD COLUMN budget_final_frac_wall_clock_seconds DOUBLE PRECISION NOT NULL DEFAULT 0.75,

  -- Ladder messages (context-first copy injected at each stage).
  ADD COLUMN budget_warn_msg_tokens TEXT NOT NULL DEFAULT 'WARNING: You have used {pct}% of your token budget. STOP and reconsider how you are handling context — you are re-sending too much on every turn. Stick to your todo list and ensure you are moving forward efficiently. Empty your todo, consolidate EVERY remaining read/probe into ONE batched tool call, and deliver only the minimal delta. Reduce context churn immediately or your session will be KILLED.',
  ADD COLUMN budget_escalate_msg_tokens TEXT NOT NULL DEFAULT 'CRITICAL: You have used {pct}% of your token budget. Your session context is too large and every turn re-sends it. STOP re-reading and exploring. Consolidate EVERY remaining tool call into a single batch, stick to your todo list, and finish the deliverable NOW. Your session will be KILLED if you keep spending at this rate.',
  ADD COLUMN budget_final_msg_tokens TEXT NOT NULL DEFAULT 'FINAL WARNING: You have used {pct}% of your token budget. This is your last chance. Stop all exploration. Finish your work in the next minimal number of tool calls or your session will be KILLED. Stick to your todo list and move forward efficiently. Deliver now.',
  ADD COLUMN budget_warn_msg_cost_usd TEXT NOT NULL DEFAULT 'WARNING: You have used {pct}% of your cost budget. STOP and reconsider how you are handling context — you are re-sending too much on every turn. Stick to your todo list, consolidate your remaining tool calls into ONE batch, and deliver the minimum. Reduce your spend immediately or your session will be KILLED.',
  ADD COLUMN budget_escalate_msg_cost_usd TEXT NOT NULL DEFAULT 'CRITICAL: You have used {pct}% of your cost budget. You are on pace to blow past it. Use only the cheapest possible tool calls, do not re-derive anything, stick to your todo list, and finish NOW. Your session will be KILLED if you keep spending.',
  ADD COLUMN budget_final_msg_cost_usd TEXT NOT NULL DEFAULT 'FINAL WARNING: You have used {pct}% of your cost budget. This is your last warning. Complete your work in the next minimal tool calls or your session will be KILLED. Deliver now.',
  ADD COLUMN budget_warn_msg_tool_call_count TEXT NOT NULL DEFAULT 'WARNING: YOU ARE CALLING TOOLS TOO OFTEN. STOP and batch your tool calls together into a single round-trip, stick to your todo list, and move forward efficiently — or you will risk your session being KILLED.',
  ADD COLUMN budget_escalate_msg_tool_call_count TEXT NOT NULL DEFAULT 'CRITICAL: YOU ARE STILL CALLING TOOLS TOO OFTEN. STOP the micro tool calls. You MUST batch them together into a single round-trip and focus on completing the todo list. Your session will be KILLED if you keep splitting your calls.',
  ADD COLUMN budget_final_msg_tool_call_count TEXT NOT NULL DEFAULT 'FINAL WARNING: YOUR TOOL CALL LIMIT IS ALMOST REACHED. YOU HAVE ONLY A HANDFUL OF TOOL CALLS LEFT. You MUST finish your work in the next tool calls or your session WILL BE KILLED. Batch everything. Stick to the todo list. Finish now.',
  ADD COLUMN budget_warn_msg_wall_clock_seconds TEXT NOT NULL DEFAULT 'WARNING: IT HAS BEEN {pct}% OF YOUR TIME BUDGET. STOP and work efficiently: batch your remaining tool calls, stick to your todo list, and finish your work to avoid exceeding budget — YOUR SESSION WILL BE KILLED.',
  ADD COLUMN budget_escalate_msg_wall_clock_seconds TEXT NOT NULL DEFAULT 'CRITICAL: YOU ARE RUNNING OUT OF TIME ({pct}% ELAPSED). STOP the slow path: batch your remaining tool calls, stick to your todo list, and finish NOW. Your session will be KILLED if you do not finish quickly.',
  ADD COLUMN budget_final_msg_wall_clock_seconds TEXT NOT NULL DEFAULT 'FINAL WARNING: {pct}% OF YOUR TIME IS GONE. You have almost no time left. Complete your work in the next tool calls. Stick to your todo list. Your session will be KILLED at the time limit.';

-- Migrate the operator's configured gate ceilings out of the legacy jsonb
-- blob into the typed columns (preserve existing limits). Absent keys leave
-- the column NULL (= built-in default). The ladder thresholds/messages are
-- NOT migrated from the jsonb — they are seeded to the current front-loaded
-- defaults above (the jsonb `warnings` block was a stale source of truth).
UPDATE tenant_settings SET
  budget_tokens               = (default_budget_overrides ->> 'tokens')::double precision,
  budget_cost_usd             = (default_budget_overrides ->> 'cost_usd')::double precision,
  budget_tool_call_count      = (default_budget_overrides ->> 'tool_call_count')::double precision,
  budget_wall_clock_seconds   = (default_budget_overrides ->> 'wall_clock_seconds')::double precision,
  budget_compact_max_turns    = (default_budget_overrides ->> 'compact_max_turns')::double precision
WHERE default_budget_overrides ?| ARRAY['tokens', 'cost_usd', 'tool_call_count', 'wall_clock_seconds', 'compact_max_turns'];
