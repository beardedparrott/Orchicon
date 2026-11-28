-- Promote two context-hygiene knobs out of the default_budget_overrides JSON document and into typed
-- columns, matching every other setting on the page.
--
-- WHY. They were carried only inside the JSON document, which is DERIVED from the typed budget ladder by
-- an allowlist (db.BudgetJSON). A key with no column exists in the stored blob but has nothing to
-- rehydrate it on READ, so a saved value was written and then invisible on the next GetSettings — the
-- settings form came back blank after every save. context_compaction and memory never had this problem
-- because they are columns; this makes these two match.
--
-- NULL means UNSET and is not zero: unset falls back to the env var / built-in default, while a stored 0
-- is an explicit operator value.
ALTER TABLE tenant_settings ADD COLUMN IF NOT EXISTS max_tool_output_bytes bigint;
ALTER TABLE tenant_settings ADD COLUMN IF NOT EXISTS context_editing_keep_tool_calls integer;

-- Carry any value already sitting in the budget document into the new columns, so an operator who saved
-- one before this migration does not silently lose it.
UPDATE tenant_settings
   SET max_tool_output_bytes = (default_budget_overrides->>'max_tool_output_bytes')::bigint
 WHERE max_tool_output_bytes IS NULL
   AND default_budget_overrides->>'max_tool_output_bytes' ~ '^[0-9]+$';

UPDATE tenant_settings
   SET context_editing_keep_tool_calls = (default_budget_overrides->'context_editing'->>'keep_tool_calls')::integer
 WHERE context_editing_keep_tool_calls IS NULL
   AND default_budget_overrides->'context_editing'->>'keep_tool_calls' ~ '^[0-9]+$';
